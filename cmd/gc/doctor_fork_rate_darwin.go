//go:build darwin

package main

import (
	"fmt"
	"os/exec"
)

// forkRatePIDProbeBinary is the trivial program the PID probe spawns. It is in
// the macOS base system, does nothing, and exits immediately, so the probe
// costs exactly one process creation and no measurable CPU.
const forkRatePIDProbeBinary = "/usr/bin/true"

// samplePlatformForkCounter implements the Darwin arm of the fork counter
// documented in doctor_fork_rate.go: it returns a freshly allocated PID.
//
// macOS allocates PIDs sequentially, so a freshly allocated PID is a monotone
// counter of process creations and the delta between two of them counts the
// creations in between. Measured on Darwin 25.6.0/arm64 over 1s windows, this
// tracks the host's real churn (129-333 creations/s over eight consecutive 1s
// windows at load ~119), which is the signal /proc/stat's cumulative
// "processes" counter carries on Linux.
//
// It is a LOWER BOUND, not the kernel's own count — PID allocation may skip
// values — and the PID space wraps, so the caller must treat a negative delta
// as unknown and must name the number a proxy. forkCounterPIDDelta's traits
// carry both obligations.
//
// There is no non-forking source for this on macOS, which is why the probe
// spends a fork to read it:
//
//   - `kern.lastpid`, the BSD sysctl that would report the last allocated PID
//     directly, does not exist on Darwin (`sysctl kern.lastpid` -> unknown oid).
//   - The highest *live* PID is not a substitute. Once the PID space has
//     wrapped, a long-lived process pins that maximum and its delta reads a
//     constant zero: measured on this host at a fixed 99440 while the allocator
//     itself was down at ~44000 and moving.
func samplePlatformForkCounter() (int64, error) {
	cmd := exec.Command(forkRatePIDProbeBinary)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawning %s to sample a PID: %w", forkRatePIDProbeBinary, err)
	}
	pid := cmd.Process.Pid
	// Reap the child. The probe runs on every doctor invocation, so leaking a
	// zombie per sample would itself consume the PID space it measures.
	_ = cmd.Wait()
	if pid <= 0 {
		return 0, fmt.Errorf("spawned %s but got no PID", forkRatePIDProbeBinary)
	}
	return int64(pid), nil
}
