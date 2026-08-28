//go:build !darwin

package main

import "errors"

// errNoPlatformForkCounter reports that this platform has no fallback
// process-creation counter behind /proc/stat.
var errNoPlatformForkCounter = errors.New("no platform fork counter on this GOOS")

// samplePlatformForkCounter has no fallback implementation on this platform, so
// the fork-rate check keeps its honest skip when /proc/stat is unreadable.
//
// This build tag exists so an unported platform is inert and says so, rather
// than being handed a counter that does not mean what the check assumes. The
// Darwin arm (doctor_fork_rate_darwin.go) works only because macOS allocates
// PIDs *sequentially*; porting another platform means confirming that property
// there first, not reusing the PID probe on faith. A counter that is not
// monotone in process creations would report a confident wrong fork rate, which
// is strictly worse than the skip.
//
// Linux never reaches this: it reads the kernel's own cumulative counter from
// /proc/stat. A Linux host with no /proc mounted lands here and skips, which is
// the correct answer for a host whose process-creation counter is unreadable.
func samplePlatformForkCounter() (int64, error) {
	return 0, errNoPlatformForkCounter
}
