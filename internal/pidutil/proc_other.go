//go:build !linux && !darwin

package pidutil

import "fmt"

// startTime reports that no PID-identity source is available. Hosts other than
// linux and darwin have neither /proc nor the darwin sysctls, so callers get an
// explicit "no identity signal" rather than a fabricated token.
func startTime(pid int) (string, error) {
	return "", fmt.Errorf("pidutil: no start-time source on this platform for PID %d", pid)
}

// cmdline reports that no argv source is available, which makes every
// cmdline-based identity guard on such a host answer no rather than yes.
func cmdline(pid int) ([]string, error) {
	return nil, fmt.Errorf("pidutil: no command-line source on this platform for PID %d", pid)
}
