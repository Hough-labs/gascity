package tmux

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

// TestObserveNamedSocketAbsentPathConsultsHolder pins the fix for
// gascity-3z7d. This test previously asserted the opposite — that an absent
// socket file "cannot be held", so the holder observation was skipped as an
// optimisation. That premise is false and was measured false on 2026-09-08:
// `rm` a live tmux server's socket and the server keeps running with every
// session still bound to it; only the path to it is gone.
//
// That made the absent-path branch the one route by which the preflight
// authorized a cold start over a live server, which binds a second server and
// orphans the fleet. It is also self-perpetuating: an unlinked-but-live socket
// is exactly the residue a clobber leaves, so the branch re-armed the very
// failure the rest of this file prevents.
//
// Cost of consulting the holder here is one process-table read on the genuine
// cold-start path (city start). Cost of skipping it was the fleet.
func TestObserveNamedSocketAbsentPathConsultsHolder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		holder     socketHolderState
		wantSafe   bool
		wantReason string
	}{
		{
			name:     "nothing holds the name: genuine cold start still works",
			holder:   socketHolderAbsent,
			wantSafe: true,
		},
		{
			name:       "live server on an unlinked socket: never safe to rebind",
			holder:     socketHolderPresent,
			wantReason: "reason=unlinked-socket-live-holder",
		},
		{
			name:       "holder unknown: fail closed",
			holder:     socketHolderUnknown,
			wantReason: "reason=socket-holder-unknown-on-absent-path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holderCalls := 0
			err := observeNamedSocketUsing(context.Background(), "missing-socket",
				func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
				func(context.Context, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED },
				func(context.Context, string) socketHolderState {
					holderCalls++
					return tc.holder
				})
			if holderCalls != 1 {
				t.Fatalf("holder calls = %d, want 1 (absence of the file is not absence of the server)", holderCalls)
			}
			if tc.wantSafe {
				if err != nil {
					t.Fatalf("observe = %v, want nil (nothing holds the name, cold start must still work)", err)
				}
				return
			}
			if err == nil {
				t.Fatal("observe = nil, want a refusal: a live server still holds this socket")
			}
			// Must NOT be reported as saturation: retrying cannot re-link a
			// socket, so advertising "transient" would spin instead of
			// surfacing a state that needs an operator.
			if errors.Is(err, errSocketHolderLive) {
				t.Fatalf("observe = %v, want a degraded (not saturated/retryable) classification", err)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("observe = %q, want %q", err, tc.wantReason)
			}
		})
	}
}

// TestObserveNamedSocketRefusedThenUnlinkedConsultsHolder pins the branch the
// other two tests in this file leave open.
//
// TestObserveNamedSocketRefusedConsultsHolder scopes itself to "a socket whose
// file is STILL PRESENT", and TestObserveNamedSocketAbsentPathConsultsHolder
// enters with the file already gone. Neither covers the race BETWEEN them: the
// file is present at the first lstat, the dial is refused, and the file is
// unlinked before the post-lstat reads it. That lands on
//
//	if errors.Is(err, syscall.ECONNREFUSED) && pathAbsent {
//	    return nil
//	}
//
// which returns "safe" without ever asking who holds the socket — the one
// remaining route to the outcome this file exists to prevent, and reached with
// BOTH of the signals that are supposed to trigger a holder check.
//
// The state it authorizes a cold start over is precisely
// unlinked-socket-live-holder: a saturated live server refuses the connect,
// and its socket has been unlinked by a partial clobber. Rebinding there binds
// a second server and orphans every session on the first — measured
// 2026-09-15, when a cold start took the tmux server out from under an agent
// that nothing had targeted (gc-eazs).
func TestObserveNamedSocketRefusedThenUnlinkedConsultsHolder(t *testing.T) {
	// A real socket file for the FIRST lstat: the branch is only reached past
	// the os.ModeSocket check, which a synthetic FileInfo cannot satisfy.
	path := shortPathSocketFixture(t)
	refuse := func(context.Context, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED }

	for _, tc := range []struct {
		name       string
		holder     socketHolderState
		wantSafe   bool
		wantReason string
	}{
		{
			name:     "nothing holds the name: genuine stale socket, cold start still works",
			holder:   socketHolderAbsent,
			wantSafe: true,
		},
		{
			name:       "live server whose socket was unlinked mid-probe: never safe to rebind",
			holder:     socketHolderPresent,
			wantReason: "reason=unlinked-socket-live-holder",
		},
		{
			name:       "holder unknown: fail closed",
			holder:     socketHolderUnknown,
			wantReason: "reason=socket-holder-unknown-on-absent-path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Present, then unlinked: the race this branch does not survive.
			lstatCalls := 0
			lstat := func(string) (os.FileInfo, error) {
				lstatCalls++
				if lstatCalls == 1 {
					return os.Lstat(path)
				}
				return nil, os.ErrNotExist
			}
			holderCalls := 0
			err := observeNamedSocketUsing(context.Background(), path, lstat, refuse,
				func(context.Context, string) socketHolderState {
					holderCalls++
					return tc.holder
				})
			if holderCalls != 1 {
				t.Fatalf("holder calls = %d, want 1 (a refusal plus a vanished path is the live-holder signature, not proof of absence)", holderCalls)
			}
			if tc.wantSafe {
				if err != nil {
					t.Fatalf("observe = %v, want nil (nothing holds the name, cold start must still work)", err)
				}
				return
			}
			if err == nil {
				t.Fatal("observe = nil, want a refusal: a live server still holds this socket")
			}
			// Same classification rule as the absent-path branch: retrying
			// cannot re-link a socket, so this must not advertise itself as
			// transient saturation.
			if errors.Is(err, errSocketHolderLive) {
				t.Fatalf("observe = %v, want a degraded (not saturated/retryable) classification", err)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("observe = %q, want %q", err, tc.wantReason)
			}
		})
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

// TestProcessTableHolderResamplesBeforeClaimingAbsence pins the defect that
// defeated the guard in production on 2026-09-15 (gc-eazs).
//
// processTableHolder claimed socketHolderAbsent from ONE `ps` listing, on the
// stated premise that "absence is claimed only from a listing that was read
// successfully and did not contain the socket". That premise is false on macOS:
// a successful listing can simply omit a live process.
//
// Measured, not theorized. A 200ms sampler watching the live tmux server logged
// twelve intervals where the server vanished from `ps` and the SAME PID returned
// 0.38-1.26s later, against one real server replacement in the same window. A
// process cannot die and come back with its own pid, so those are ps omitting a
// live process. Two of them landed ~2 minutes before the clobber.
//
// The cost is asymmetric and the file already says so: over-reporting a holder
// costs a retry, under-reporting it costs the fleet. So a single sighting in ANY
// sample means present, and absence requires every sample to agree.
func TestProcessTableHolderResamplesBeforeClaimingAbsence(t *testing.T) {
	const socketPath = "/tmp/tmux-501/gc"
	holderRow := "88986 tmux -u -L gc new-session -d -s gastown__mayor -c /Users/hunter/gc\n"
	otherRows := "412 /usr/sbin/cfprefsd daemon\n9931 /opt/homebrew/bin/fish\n"

	for _, tc := range []struct {
		name      string
		listings  []string
		want      socketHolderState
		wantReads int
	}{
		{
			// The regression: ps drops the live server from one sample.
			name:      "holder missing from the first listing but present in a later one",
			listings:  []string{otherRows, otherRows + holderRow, otherRows + holderRow},
			want:      socketHolderPresent,
			wantReads: 2,
		},
		{
			name:      "holder present immediately: no extra listings are read",
			listings:  []string{otherRows + holderRow},
			want:      socketHolderPresent,
			wantReads: 1,
		},
		{
			// A genuinely stale socket must still be rebindable, or every real
			// cold start stalls.
			name:      "every listing agrees the socket is unheld",
			listings:  []string{otherRows, otherRows, otherRows, otherRows, otherRows, otherRows},
			want:      socketHolderAbsent,
			wantReads: 0, // asserted as "all of them" below
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socketHolderResampleGap = 0
			t.Cleanup(func() { socketHolderResampleGap = 700 * time.Millisecond })

			reads := 0
			listings := tc.listings
			processTableListing = func(context.Context) ([]byte, error) {
				out := listings[min(reads, len(listings)-1)]
				reads++
				return []byte(out), nil
			}
			t.Cleanup(func() {
				processTableListing = func(ctx context.Context) ([]byte, error) {
					return exec.CommandContext(ctx, "ps", "-Awwo", "pid=,args=").Output()
				}
			})

			got := processTableHolder(context.Background(), socketPath)
			if got != tc.want {
				t.Fatalf("processTableHolder = %v, want %v (reads=%d)", got, tc.want, reads)
			}
			if tc.wantReads > 0 && reads != tc.wantReads {
				t.Fatalf("listings read = %d, want %d", reads, tc.wantReads)
			}
			if tc.want == socketHolderAbsent && reads < 2 {
				t.Fatalf("listings read = %d, want more than one before claiming absence", reads)
			}
		})
	}
}

// TestProcessTableHolderFailsClosedOnReadError keeps a listing that cannot be
// read from being mistaken for a listing that does not contain the socket.
func TestProcessTableHolderFailsClosedOnReadError(t *testing.T) {
	socketHolderResampleGap = 0
	t.Cleanup(func() { socketHolderResampleGap = 700 * time.Millisecond })
	processTableListing = func(context.Context) ([]byte, error) {
		return nil, errors.New("ps unavailable")
	}
	t.Cleanup(func() {
		processTableListing = func(ctx context.Context) ([]byte, error) {
			return exec.CommandContext(ctx, "ps", "-Awwo", "pid=,args=").Output()
		}
	})
	if got := processTableHolder(context.Background(), "/tmp/tmux-501/gc"); got != socketHolderUnknown {
		t.Fatalf("processTableHolder = %v, want unknown (fail closed)", got)
	}
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
