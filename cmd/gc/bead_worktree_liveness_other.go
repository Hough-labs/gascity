//go:build !linux && !darwin

package main

// collectLiveWorktreeState has no process-table implementation on this
// platform, so it reports the enumeration as failed and the reaper protects
// every candidate worktree.
//
// This build tag exists so an unported platform fails CLOSED and loudly-inert
// rather than failing to compile — and, more importantly, so nobody is tempted
// to satisfy the compiler with a stub that returns scanned=true. An empty live
// set with scanned=true reads as "no process is working anywhere" and would
// authorize the reaper to delete every closed-bead worktree on the host.
//
// Porting a new platform means implementing collectLiveWorktreeState against
// its real process table, under the contract documented in
// bead_worktree_liveness.go.
func collectLiveWorktreeState() liveWorktreeState {
	return liveWorktreeState{scanned: false}
}
