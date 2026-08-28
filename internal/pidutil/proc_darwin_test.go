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
// pidutil_test.go, which carries no tag and therefore runs on darwin. This file
// only unit-tests the KERN_PROCARGS2 decoder, which exists on no other
// platform. Do not move behavior assertions in here — hiding them behind a
// platform tag is what let gascity-ggq regress unnoticed.

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

func TestParseProcArgs2DecodesArgvAndStopsAtArgc(t *testing.T) {
	args := []string{"/usr/local/bin/gc", "nudge", "poll", "--city", "/Users/a b/city", "--session", "s-worker"}
	buf := procArgs2Buffer(uint32(len(args)), "/usr/local/bin/gc", 7, args, []string{"PATH=/usr/bin", "HOME=/Users/a b"})

	got, err := parseProcArgs2(buf)
	if err != nil {
		t.Fatalf("parseProcArgs2: %v", err)
	}
	if len(got) != len(args) {
		t.Fatalf("parseProcArgs2 = %q (%d elements), want %d — the environment must not be read as arguments", got, len(got), len(args))
	}
	for i := range args {
		if got[i] != args[i] {
			t.Fatalf("parseProcArgs2[%d] = %q, want %q (full argv %q)", i, got[i], args[i], got)
		}
	}
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

// TestStartTimeIsPerProcessOnDarwin pins the property the identity guard rests
// on: the token distinguishes two different processes, so a PID recycled to an
// unrelated process cannot pass a start-time comparison against the original.
func TestStartTimeIsPerProcessOnDarwin(t *testing.T) {
	self, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatalf("StartTime(%d): %v", os.Getpid(), err)
	}
	parent, err := StartTime(os.Getppid())
	if err != nil {
		t.Fatalf("StartTime(%d): %v", os.Getppid(), err)
	}
	if self == parent {
		t.Fatalf("StartTime is identical for PIDs %d and %d (%q) — the token cannot distinguish processes", os.Getpid(), os.Getppid(), self)
	}
}
