//go:build darwin

package pidutil

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

// procArgcSize is the width of the leading argc word in a KERN_PROCARGS2
// buffer: the kernel writes it as a C int, which is 32 bits on every darwin
// architecture including arm64.
const procArgcSize = 4

// startTime returns pid's start time from the kern.proc.pid sysctl as
// "<sec>.<usec>". The kernel stamps p_starttime once at exec and never
// rewrites it, so the pair (pid, start time) is unique for the lifetime of a
// boot — the same identity guarantee /proc/<pid>/stat's starttime field gives
// on linux. Microsecond resolution makes a collision between an original
// process and a later one that recycled its PID effectively impossible.
//
// The sysctl reports only processes visible to the caller; for a PID that no
// longer exists it short-reads and x/sys reports EIO, which surfaces here as
// an error rather than a zero-valued identity.
func startTime(pid int) (string, error) {
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

// cmdline returns pid's argv from the kern.procargs2 sysctl, which hands back
// the real NUL-separated argument vector the process was executed with.
//
// This deliberately does not shell out to `ps -o args=`: ps renders argv as a
// single space-joined string, so an argument containing whitespace — a city
// path or session name with a space in it — is indistinguishable from two
// arguments once split. Callers here are identity guards that compare whole
// argv tokens, so a lossy split would silently stop matching. KERN_PROCARGS2
// preserves the boundaries and costs no subprocess.
//
// The sysctl is readable only for processes owned by the calling uid, which
// doubles as an ownership check: an unreadable process yields an error, and
// every caller treats that as "identity not established".
func cmdline(pid int) ([]string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("pidutil: reading kern.procargs2 for PID %d: %w", pid, err)
	}
	argv, err := parseProcArgs2(buf)
	if err != nil {
		return nil, fmt.Errorf("pidutil: parsing kern.procargs2 for PID %d: %w", pid, err)
	}
	return argv, nil
}

// parseProcArgs2 decodes a KERN_PROCARGS2 buffer. Its layout is an argc word,
// the NUL-terminated executable path, zero or more NUL bytes of alignment
// padding, then argc NUL-terminated argument strings, then the environment —
// which is why argc is read rather than splitting the whole buffer on NUL.
//
// Every malformed shape returns an error instead of a partial argv: a short
// read that silently produced fewer arguments would make an identity matcher
// answer on evidence it does not have.
func parseProcArgs2(buf []byte) ([]string, error) {
	if len(buf) < procArgcSize {
		return nil, fmt.Errorf("buffer is %d bytes, want at least %d for the argc word", len(buf), procArgcSize)
	}
	argc := int(binary.NativeEndian.Uint32(buf[:procArgcSize]))
	rest := buf[procArgcSize:]
	// Each argument occupies at least its NUL terminator, so an argc larger
	// than the bytes remaining cannot be real and is rejected rather than
	// driving the loop below off the end of a truncated buffer.
	if argc < 0 || argc > len(rest) {
		return nil, fmt.Errorf("argc %d exceeds the %d bytes of argument data", argc, len(rest))
	}
	if argc == 0 {
		return nil, nil
	}

	execPathEnd := bytes.IndexByte(rest, 0)
	if execPathEnd < 0 {
		return nil, fmt.Errorf("executable path is not NUL-terminated")
	}
	rest = rest[execPathEnd:]
	// The kernel pads between the executable path and argv[0] to an alignment
	// boundary; the padding is NUL bytes, and so is the path's own terminator.
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}

	argv := make([]string, 0, argc)
	for len(argv) < argc {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return nil, fmt.Errorf("argument %d of %d is not NUL-terminated", len(argv)+1, argc)
		}
		argv = append(argv, string(rest[:end]))
		rest = rest[end+1:]
	}
	return NormalizeArgv(argv), nil
}
