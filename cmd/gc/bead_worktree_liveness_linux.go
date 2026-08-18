//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// collectLiveWorktreeState walks /proc/<pid>/cwd for every process on the host
// and records their canonical working directories. When the top-level /proc
// walk fails outright it returns scanned=false so the caller fails closed and
// reaps nothing. See bead_worktree_liveness.go for the contract every platform
// implementation shares.
func collectLiveWorktreeState() liveWorktreeState {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return liveWorktreeState{scanned: false}
	}
	seen := make(map[string]struct{})
	var cwds []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue // not a PID directory
		}
		link, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if err != nil || link == "" {
			continue
		}
		// A cwd whose inode has been unlinked carries a trailing " (deleted)"
		// marker. The directory is gone, so it can never match a live worktree
		// path on disk — drop it rather than canonicalize a bogus path. (The
		// rare live directory literally named "... (deleted)" would be dropped
		// too; that only ever loses protection for a pathological path the
		// fleet never creates, and the git-clean gate still applies.)
		if strings.HasSuffix(link, " (deleted)") {
			continue
		}
		canon := pathutil.NormalizePathForCompare(link)
		if canon == "" {
			continue
		}
		if _, ok := seen[canon]; ok {
			continue
		}
		seen[canon] = struct{}{}
		cwds = append(cwds, canon)
	}
	return liveWorktreeState{cwds: cwds, scanned: true}
}
