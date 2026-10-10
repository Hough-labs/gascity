//go:build !darwin

package pidutil

// platformStartTime reads pid's start time with ps where /proc could not
// answer. Darwin reads the kern.proc.pid sysctl instead.
func platformStartTime(pid int) (string, error) {
	return psStartTime(pid)
}
