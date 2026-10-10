//go:build darwin

package pidutil

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// platformStartTime returns pid's start time from the kern.proc.pid sysctl as
// "<sec>.<usec>". The kernel stamps p_starttime once at exec and never
// rewrites it, so the pair (pid, start time) is unique for the lifetime of a
// boot — the same identity guarantee /proc/<pid>/stat's starttime field gives
// on linux. Microsecond resolution makes a collision between an original
// process and a later one that recycled its PID effectively impossible, where
// `ps -o lstart=` resolves only to the second; and it costs no subprocess,
// which matters to callers that poll it every few milliseconds across a kill
// grace window.
//
// The sysctl reports only processes visible to the caller; for a PID that no
// longer exists it short-reads and x/sys reports EIO, which surfaces here as
// an error rather than a zero-valued identity.
func platformStartTime(pid int) (string, error) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", fmt.Errorf("pidutil: reading kern.proc.pid for PID %d: %w", pid, err)
	}
	started := proc.Proc.P_starttime
	if started.Sec == 0 && started.Usec == 0 {
		return "", fmt.Errorf("pidutil: kern.proc.pid reported no start time for PID %d", pid)
	}
	return fmt.Sprintf("%d.%06d", started.Sec, started.Usec), nil
}
