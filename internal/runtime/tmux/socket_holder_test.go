package tmux

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestObserveNamedSocketRefusedConsultsHolder pins the observation at the
// heart of gascity-n17v: ECONNREFUSED on a socket whose file is still present
// is ambiguous. It is a stale socket only if nothing holds it, and a live
// server with a full accept backlog otherwise. Before this, the observer read
// every refusal as "stale" and cleared tmux to unlink the path.
func TestObserveNamedSocketRefusedConsultsHolder(t *testing.T) {
	// A real socket file, not a synthetic FileInfo: the branch under test is
	// only reached when os.SameFile says the socket's identity is unchanged,
	// and SameFile only recognizes FileInfos the os package produced itself.
	path := shortPathSocketFixture(t)
	lstat := func(string) (os.FileInfo, error) { return os.Lstat(path) }
	refuse := func(context.Context, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED }

	for _, tc := range []struct {
		name       string
		holder     socketHolderState
		wantSafe   bool
		wantLive   bool
		wantReason string
	}{
		{
			name:     "nothing holds the socket: stale file, safe to rebind",
			holder:   socketHolderAbsent,
			wantSafe: true,
		},
		{
			name:       "live holder: saturated backlog, never safe",
			holder:     socketHolderPresent,
			wantLive:   true,
			wantReason: "reason=refused-by-live-holder",
		},
		{
			name:       "holder unknown: fail closed",
			holder:     socketHolderUnknown,
			wantReason: "reason=socket-holder-unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holderCalls := 0
			err := observeNamedSocketUsing(context.Background(), path, lstat, refuse,
				func(context.Context, string) socketHolderState {
					holderCalls++
					return tc.holder
				})
			if holderCalls != 1 {
				t.Fatalf("holder calls = %d, want 1", holderCalls)
			}
			if tc.wantSafe {
				if err != nil {
					t.Fatalf("observe = %v, want nil (stale socket is safe to rebind)", err)
				}
				return
			}
			if err == nil {
				t.Fatal("observe = nil, want a refusal")
			}
			if got := errors.Is(err, errSocketHolderLive); got != tc.wantLive {
				t.Fatalf("errors.Is(%v, errSocketHolderLive) = %v, want %v", err, got, tc.wantLive)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("observe = %q, want %q", err, tc.wantReason)
			}
		})
	}
}

// TestObserveNamedSocketAbsentPathSkipsHolder keeps the common cold start
// cheap: a socket that is not on disk cannot be held, so the process-table
// observation must not run.
func TestObserveNamedSocketAbsentPathSkipsHolder(t *testing.T) {
	err := observeNamedSocketUsing(context.Background(), "missing-socket",
		func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		func(context.Context, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED },
		func(context.Context, string) socketHolderState {
			t.Fatal("holder observation ran for an absent socket")
			return socketHolderUnknown
		})
	if err != nil {
		t.Fatalf("observe absent socket = %v, want nil", err)
	}
}

func TestProcNetUnixHolder(t *testing.T) {
	const listing = `Num       RefCount Protocol Flags    Type St Inode Path
ffff9c0a: 00000002 00000000 00010000 0001 01 26315 /tmp/tmux-1000/gc-city
ffff9c0b: 00000003 00000000 00000000 0001 03 26316 /run/dbus/system_bus_socket
ffff9c0c: 00000002 00000000 00010000 0001 01 26317
`
	dir := t.TempDir()
	path := filepath.Join(dir, "unix")
	if err := os.WriteFile(path, []byte(listing), 0o600); err != nil {
		t.Fatalf("write listing: %v", err)
	}

	t.Run("bound socket is held", func(t *testing.T) {
		state, decided := procNetUnixHolder(path, "/tmp/tmux-1000/gc-city")
		if !decided || state != socketHolderPresent {
			t.Fatalf("holder = (%v, %v), want (present, true)", state, decided)
		}
	})
	t.Run("unlisted socket is stale", func(t *testing.T) {
		state, decided := procNetUnixHolder(path, "/tmp/tmux-1000/gone")
		if !decided || state != socketHolderAbsent {
			t.Fatalf("holder = (%v, %v), want (absent, true)", state, decided)
		}
	})
	t.Run("missing listing defers to the process table", func(t *testing.T) {
		state, decided := procNetUnixHolder(filepath.Join(dir, "no-such-file"), "/tmp/tmux-1000/gc-city")
		if decided {
			t.Fatalf("holder = (%v, %v), want undecided so the fallback runs", state, decided)
		}
	})
}

func TestFieldsReferenceTmuxSocket(t *testing.T) {
	const socketPath = "/tmp/tmux-501/gc-city"
	aliases := map[string]struct{}{socketPath: {}, "/private" + socketPath: {}}

	for _, tc := range []struct {
		name string
		row  string
		want bool
	}{
		{
			name: "macOS server keeps the creating client's argv",
			row:  "4242 tmux -L gc-city new-session -d -s mayor",
			want: true,
		},
		{
			name: "linux server rewrites its process title",
			row:  "4242 tmux: server (/tmp/tmux-501/gc-city)",
			want: true,
		},
		{
			name: "explicit socket path",
			row:  "4242 tmux -S /private/tmp/tmux-501/gc-city has-session",
			want: true,
		},
		{
			name: "blocked client counts: over-reporting a holder only costs a retry",
			row:  "4242 tmux -L gc-city has-session -t nope",
			want: true,
		},
		{
			name: "another city's socket",
			row:  "4242 tmux -L other-city new-session -d -s mayor",
			want: false,
		},
		{
			name: "non-tmux process naming the socket",
			row:  "4242 grep -L gc-city",
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fieldsReferenceTmuxSocket(strings.Fields(tc.row), "gc-city", aliases); got != tc.want {
				t.Fatalf("fieldsReferenceTmuxSocket(%q) = %v, want %v", tc.row, got, tc.want)
			}
		})
	}
}

// TestNamedSocketHolderReportsLiveListener exercises the production probe end
// to end against a socket this test binds itself, which is the same shape the
// probe must recognize on a saturated tmux server: a bound socket that is not
// accepting.
func TestNamedSocketHolderReportsLiveListener(t *testing.T) {
	// /proc/net/unix answers exactly on Linux. Everywhere else the fallback
	// looks for a tmux process, and this listener is not one — so the honest
	// expectation there is "absent", not "present".
	if _, decided := procNetUnixHolder(procNetUnixPath, "/tmp/probe"); !decided {
		t.Skip("no /proc/net/unix on this host; the process-table fallback is covered by TestFieldsReferenceTmuxSocket")
	}
	path := filepath.Join(shortSocketDir(t), "held.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	if state := namedSocketHolder(context.Background(), path); state != socketHolderPresent {
		t.Fatalf("holder = %v, want present for a socket this test is listening on", state)
	}
}

// shortSocketDir returns a directory whose paths fit in sockaddr_un's 104-byte
// limit. t.TempDir() does not on macOS, where TMPDIR is already ~50 bytes deep
// before the test name is appended.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gcsk")
	if err != nil {
		t.Fatalf("creating short socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// shortPathSocketFixture leaves a real, unbound unix socket file on disk — the
// exact artifact a tmux server killed with SIGKILL leaves behind.
func shortPathSocketFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(shortSocketDir(t), "fixture.sock")
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	// Keep the file after the listener goes away, which is what makes it a
	// stale socket rather than a live one.
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return path
}
