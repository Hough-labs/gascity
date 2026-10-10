//go:build darwin

package pidutil

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

// The build tag here is narrower than it looks: the portable behavior contract
// (matcher consulted, boundaries preserved, fail-closed) is asserted in
// untagged test files, which therefore run on darwin. This file only
// unit-tests the KERN_PROCARGS2 decoder against buffers rendered the way the
// darwin kernel writes them, and what the darwin start-time source adds over
// ps. Do not move behavior assertions in here — hiding them behind a platform
// tag is what let gascity-ggq regress unnoticed.

// procArgs2Buffer renders a KERN_PROCARGS2 buffer the way the kernel does:
// the argc word, the NUL-terminated executable path, alignment padding, then
// the NUL-terminated argument strings, then the environment.
func procArgs2Buffer(argc uint32, execPath string, padding int, args, env []string) []byte {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.NativeEndian, argc)
	buf.WriteString(execPath)
	buf.WriteByte(0)
	buf.Write(make([]byte, padding))
	for _, arg := range args {
		buf.WriteString(arg)
		buf.WriteByte(0)
	}
	for _, kv := range env {
		buf.WriteString(kv)
		buf.WriteByte(0)
	}
	return buf.Bytes()
}

func TestParseProcArgs2RejectsMalformedBuffers(t *testing.T) {
	args := []string{"/bin/sleep", "5"}
	truncated := procArgs2Buffer(uint32(len(args)), "/bin/sleep", 0, args, nil)
	truncated = truncated[:len(truncated)-1] // drop the final NUL terminator

	cases := []struct {
		name string
		buf  []byte
	}{
		{name: "shorter than the argc word", buf: []byte{0x01, 0x00}},
		{name: "argc larger than the argument data", buf: procArgs2Buffer(4096, "/bin/sleep", 0, args, nil)},
		{name: "executable path not terminated", buf: append(binary.NativeEndian.AppendUint32(nil, 1), []byte("/bin/sleep")...)},
		{name: "final argument not terminated", buf: truncated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A partial argv is worse than an error: an identity matcher would
			// answer on evidence the decode never actually established.
			if argv, err := parseProcArgs2(tc.buf); err == nil {
				t.Fatalf("parseProcArgs2 = %q, nil error; want an error", argv)
			}
		})
	}
}

func TestParseProcArgs2TreatsZeroArgcAsNoArgv(t *testing.T) {
	argv, err := parseProcArgs2(procArgs2Buffer(0, "/bin/sleep", 0, nil, nil))
	if err != nil {
		t.Fatalf("parseProcArgs2: %v", err)
	}
	if len(argv) != 0 {
		t.Fatalf("parseProcArgs2 = %q, want empty argv for argc 0", argv)
	}
}

// TestStartTimeResolvesBelowOneSecondOnDarwin pins what the kern.proc.pid
// source adds over `ps -o lstart=`: two processes started back to back, well
// inside one second, still get different tokens. lstart resolves only to the
// second, so a PID recycled within the second its predecessor started would
// compare equal and pass as the original process.
func TestStartTimeResolvesBelowOneSecondOnDarwin(t *testing.T) {
	first := startProcess(t, "sleep", "5").Process.Pid
	second := startProcess(t, "sleep", "5").Process.Pid
	a, err := StartTime(first)
	if err != nil {
		t.Fatalf("StartTime(%d): %v", first, err)
	}
	b, err := StartTime(second)
	if err != nil {
		t.Fatalf("StartTime(%d): %v", second, err)
	}
	if a == b {
		t.Fatalf("StartTime is %q for both PIDs %d and %d started back to back; the token cannot tell them apart", a, first, second)
	}
}

// TestStartTimeNeedsNoSubprocessOnDarwin pins the other half: the darwin
// source is a sysctl, not a ps fork. proctable polls StartTime on every tick of
// a kill's SIGTERM grace, so a ps-backed identity would fork once per poll and
// fail outright wherever ps cannot run.
func TestStartTimeNeedsNoSubprocessOnDarwin(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := StartTime(os.Getpid()); err != nil {
		t.Fatalf("StartTime(self) with no ps on PATH: %v", err)
	}
}
