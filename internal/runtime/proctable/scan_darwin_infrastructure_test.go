//go:build darwin

package proctable

import "testing"

// realWorldTable models the live process table measured on hammer during the
// gc-eazs investigation (2026-09-15). The tmux server's argv is the
// `new-session -e KEY=VALUE ...` command that bootstrapped it, and `ps eww`
// concatenates argv with the environment block, so parseInlineEnv reads those
// argv tokens as if they were the server's own environment.
func realWorldTable() map[int]psRecord {
	mayorEnv := map[string]string{
		"GC_SESSION_ID":    "gc-50q12",
		"GC_RUNTIME_EPOCH": "43",
		"GC_CITY_PATH":     "/Users/hunter/gc",
	}
	deaconEnv := map[string]string{
		"GC_SESSION_ID":    "gc-jlx8",
		"GC_RUNTIME_EPOCH": "77",
		"GC_CITY_PATH":     "/Users/hunter/gc",
	}
	return map[int]psRecord{
		// The tmux server. ppid 1, and its argv carries the mayor's -e flags.
		32317: {pid: 32317, ppid: 1, command: "tmux", env: mayorEnv},
		// The mayor's pane leader — a genuine agent root.
		32318: {pid: 32318, ppid: 32317, command: "/opt/homebrew/bin/fish", env: mayorEnv},
		// The mayor's agent process — not a root; its parent is the pane leader.
		32373: {pid: 32373, ppid: 32318, command: "claude", env: mayorEnv},
		// The deacon's pane leader — a genuine agent root whose parent IS the
		// tmux server, which is why parent-is-infrastructure must keep it.
		33390: {pid: 33390, ppid: 32317, command: "/opt/homebrew/bin/fish", env: deaconEnv},
	}
}

// TestScanNeverReturnsTmuxServerAsAgentRoot is the regression test for gc-eazs:
// a `gc handoff` in the mayor's session took down the WHOLE city. The pre-start
// orphan sweep (session.killExistingOrphans) calls ScanBySessionID for the
// session it is about to start and group-SIGTERMs every untracked root it gets
// back. Because the tmux server reads as carrying the mayor's GC_SESSION_ID, it
// was returned as a kill target — and killing it destroys every session on the
// socket, including agents nobody targeted.
//
// Linux cannot hit this: its scanner reads /proc/<pid>/environ, the true
// environment, which has no GC_SESSION_ID for the server process.
func TestScanNeverReturnsTmuxServerAsAgentRoot(t *testing.T) {
	roots := rootsFromRecords(realWorldTable(), "gc-50q12")
	for _, r := range roots {
		if r.PID == 32317 {
			t.Fatalf("tmux server pid 32317 returned as an agent root for %s: "+
				"the orphan sweep would group-SIGTERM the tmux server and take the whole city down (roots=%+v)",
				r.SessionID, roots)
		}
	}
	if len(roots) != 1 || roots[0].PID != 32318 {
		t.Fatalf("roots = %+v, want exactly the mayor's pane leader (pid 32318)", roots)
	}
}

// TestScanStillReturnsPaneLeaderParentedToTmuxServer guards the fix against
// over-correction: excluding infrastructure processes must exclude the SERVER
// itself, never a real agent root that merely hangs off the server. The deacon's
// pane leader is parented directly to the tmux server and must stay a root.
func TestScanStillReturnsPaneLeaderParentedToTmuxServer(t *testing.T) {
	roots := rootsFromRecords(realWorldTable(), "gc-jlx8")
	if len(roots) != 1 || roots[0].PID != 33390 {
		t.Fatalf("roots = %+v, want the deacon's pane leader (pid 33390) even though its parent is the tmux server", roots)
	}
}

// TestIsScanRootRejectsTmuxServer covers the sibling predicate, which had the
// same parent-only infrastructure test.
func TestIsScanRootRejectsTmuxServer(t *testing.T) {
	if isScanRootFromRecords(realWorldTable(), 32317) {
		t.Fatal("tmux server classified as an agent scan root")
	}
	if !isScanRootFromRecords(realWorldTable(), 32318) {
		t.Fatal("mayor pane leader should be an agent scan root")
	}
	if !isScanRootFromRecords(realWorldTable(), 33390) {
		t.Fatal("deacon pane leader parented to the tmux server should be an agent scan root")
	}
}
