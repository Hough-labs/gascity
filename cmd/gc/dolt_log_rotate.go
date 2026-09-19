package main

// Size-bounded rotation for the managed dolt sql-server log (gascity-zk97).
//
// The managed server's stdout and stderr are one file — <pack state
// dir>/dolt.log — opened O_APPEND and then held for the whole life of the
// server by both the supervising watchdog and, as an inherited descriptor,
// the dolt child. Nothing ever bounded it: one measured city carried a single
// 66.5 MB dolt.log spanning 58 days, ~331k of whose lines were the by-design
// read-timeout reaps that dolt-config.yaml documents. The volume is not
// itself a fault; the unbounded single file is, because it makes every
// diagnosis of this subsystem slow.
//
// # Why rotation runs at server start rather than on a timer
//
// The dominant writer is the dolt child, and it holds an inherited
// descriptor, not a path. Renaming dolt.log out from under a live child does
// not redirect it: the child keeps appending to the renamed generation while
// the watchdog's own lines go to the fresh file, which splits one server's
// output across two files and leaves the rotated generation growing
// unbounded. Copy-truncate trades that for a worse failure — it races the
// child's appends and can lose a partial line.
//
// So rotation runs at the one point in a server's life where no live writer
// holds the log: the managed start path, after
// waitForManagedDoltDataDirLockFree has proven the previous server released
// its store lock (so that process is gone) and before anything opens a
// handle. Every later holder — the parent's own *os.File, the watchdog's
// re-opened handle, the child's inherited descriptor — is opened after the
// rename and so lands on the fresh file.
//
// The limit of that choice is stated rather than hidden: the bound is
// enforced per server start, not continuously, so a server that never
// restarts never rotates. Giving the log a hard live bound means moving the
// child's output off the shared file onto a pipe the watchdog pumps, which
// changes when the server dies — a Go child whose stdout pipe loses its read
// end is killed by SIGPIPE — and that lifetime change does not belong in a
// logging change.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// rotateManagedDoltLog renames path aside once it has reached maxBytes,
// keeping at most keep numbered generations beside it — path.1 is the most
// recent rotation and path.<keep> the oldest — and pruning everything older,
// including generations stranded by a previously larger keep. It is a no-op
// when rotation is disabled (maxBytes <= 0), when path is blank or absent, or
// when path is still under the bound. keep below 1 is normalized to 1, so a
// rotation always retains the file it just rotated.
//
// Rotation is rename-only. It never truncates and never rewrites in place, so
// a descriptor another process still holds stays valid and keeps every byte
// already written through it: the renamed generation is the same inode.
// Callers are nevertheless expected to rotate only when no live writer holds
// the log, because a surviving writer would go on appending to the rotated
// generation instead of the fresh path (see the file header).
func rotateManagedDoltLog(path string, maxBytes int64, keep int) error {
	if maxBytes <= 0 || strings.TrimSpace(path) == "" {
		return nil
	}
	size, err := managedDoltLogSize(path)
	if err != nil {
		return fmt.Errorf("stat dolt log %s: %w", path, err)
	}
	if size < maxBytes {
		return nil
	}
	if keep < 1 {
		keep = 1
	}
	existing, err := managedDoltLogGenerations(path)
	if err != nil {
		return err
	}
	// Walking the generations that exist — rather than counting down from
	// keep — keeps the work proportional to the directory instead of to a
	// configured number, so an implausible keep cannot turn a server start
	// into millions of renames of files that were never there.
	for _, gen := range existing {
		if gen < keep {
			continue
		}
		// Pushed out of the retention window by the rotation below, or
		// stranded by a keep that has since shrunk.
		if err := os.Remove(managedDoltLogGeneration(path, gen)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("prune dolt log generation %s: %w", managedDoltLogGeneration(path, gen), err)
		}
	}
	// Shift the survivors down one slot, newest-numbered first, so no rename
	// lands on a generation that has not moved yet.
	for i := len(existing) - 1; i >= 0; i-- {
		gen := existing[i]
		if gen >= keep {
			continue
		}
		from := managedDoltLogGeneration(path, gen)
		if err := os.Rename(from, managedDoltLogGeneration(path, gen+1)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rotate dolt log generation %s: %w", from, err)
		}
	}
	// A log that vanished between the size check and here (another start
	// racing this one, an operator deleting it) needs no rotation, so a
	// missing source is success rather than an error.
	if err := os.Rename(path, managedDoltLogGeneration(path, 1)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rotate dolt log %s: %w", path, err)
	}
	return nil
}

// managedDoltLogGenerations returns the generation numbers of path's existing
// rotated logs, ascending. A missing directory yields none rather than an
// error, so a log whose scope was torn down mid-start is not a failure.
func managedDoltLogGenerations(path string) ([]int, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read dolt log dir %s: %w", dir, err)
	}
	var generations []int
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if gen, ok := managedDoltLogGenerationNumber(base, entry.Name()); ok {
			generations = append(generations, gen)
		}
	}
	slices.Sort(generations)
	return generations, nil
}

// managedDoltLogGeneration names the nth rotated generation of path.
func managedDoltLogGeneration(path string, gen int) string {
	return path + "." + strconv.Itoa(gen)
}

// managedDoltLogGenerationNumber reports the generation number encoded in
// name when it is a rotated generation of base, and whether it is one at all.
// Only a positive decimal suffix counts, so neither the live log nor an
// unrelated neighbor (dolt.log.gz, dolt-config.yaml) is ever mistaken for a
// generation and deleted.
func managedDoltLogGenerationNumber(base, name string) (int, bool) {
	suffix, ok := strings.CutPrefix(name, base+".")
	if !ok {
		return 0, false
	}
	gen, err := strconv.Atoi(suffix)
	if err != nil || gen < 1 || suffix != strconv.Itoa(gen) {
		return 0, false
	}
	return gen, true
}
