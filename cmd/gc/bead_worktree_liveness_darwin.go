//go:build darwin

package main

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// lsofCWDScanTimeout bounds the single lsof invocation below. lsof can block
// indefinitely on an unresponsive mount, and this scan runs on the witness's
// pre_start path where a hang costs the agent its startup. A timeout is
// therefore treated as "could not enumerate" (scanned=false), which protects
// every worktree rather than reaping on a partial picture.
const lsofCWDScanTimeout = 10 * time.Second

// collectLiveWorktreeState enumerates live process working directories on
// Darwin, which has no /proc. It shells out to lsof — the same signal the dolt
// reaper already uses on this platform (dolt_process_inspection.go), rather
// than adding a cgo/sysctl dependency for one scanner.
//
// This is the Darwin half that gastownhall/gascity#4851 ("Make workspace leak
// detection portable and fail closed") never delivered: the fail-closed posture
// landed, the portable enumeration did not, so on macOS every reap candidate
// hit "liveness scan unavailable (failing closed, protecting all)" forever and
// the reaper could never remove anything.
//
// See bead_worktree_liveness.go for the contract shared with the Linux
// implementation.
func collectLiveWorktreeState() liveWorktreeState {
	// No lsof means no process table on this host. Fail closed rather than
	// reporting an empty live set, which would read as "nothing is running"
	// and authorize deleting every candidate.
	if _, err := exec.LookPath("lsof"); err != nil {
		return liveWorktreeState{scanned: false}
	}

	ctx, cancel := context.WithTimeout(context.Background(), lsofCWDScanTimeout)
	defer cancel()

	//	-n  skip DNS resolution      -w  suppress per-process warnings
	//	-P  skip port-name lookup    -a  AND the selectors together
	//	-d cwd  only the cwd file descriptor, not every open file
	//	-F n    machine-readable field output; each record is a "p<pid>" line,
	//	        an "f<fd>" line, then the "n<path>" line we want.
	cmd := exec.CommandContext(ctx, "lsof", "-n", "-P", "-w", "-a", "-d", "cwd", "-F", "n")
	out, err := cmd.Output()

	// A timeout is indeterminate, not empty: whatever lsof had printed so far
	// is an arbitrary prefix of the process table, and treating that prefix as
	// the complete live set would unprotect every process it had not reached.
	if ctx.Err() != nil {
		return liveWorktreeState{scanned: false}
	}

	// A non-zero exit is NOT fatal on its own. lsof exits 1 when it could not
	// stat some processes (other users', or ones that exited mid-scan) while
	// still printing every record it did read — the direct analog of the
	// Linux implementation skipping an unreadable /proc/<pid>/cwd. Parse what
	// we got; the emptiness check below is what distinguishes partial success
	// from total failure.
	_ = err

	seen := make(map[string]struct{})
	var cwds []string
	for _, line := range strings.Split(string(out), "\n") {
		// Field output prefixes every value with its field letter. Only "n"
		// (name) lines carry the cwd path.
		if len(line) < 2 || line[0] != 'n' {
			continue
		}
		path := strings.TrimRight(line[1:], "\r")
		if path == "" {
			continue
		}
		// Mirror the Linux implementation's guard: a cwd whose inode has been
		// unlinked can never match a live worktree path on disk.
		if strings.HasSuffix(path, " (deleted)") {
			continue
		}
		canon := pathutil.NormalizePathForCompare(path)
		if canon == "" {
			continue
		}
		if _, ok := seen[canon]; ok {
			continue
		}
		seen[canon] = struct{}{}
		cwds = append(cwds, canon)
	}

	// Zero usable cwds means the enumeration failed, not that the host is
	// idle: this process is itself running and has a cwd, so a working lsof
	// always returns at least one record. Reporting scanned=true here would
	// hand the reaper an empty live set and unprotect every candidate.
	if len(cwds) == 0 {
		return liveWorktreeState{scanned: false}
	}
	return liveWorktreeState{cwds: cwds, scanned: true}
}
