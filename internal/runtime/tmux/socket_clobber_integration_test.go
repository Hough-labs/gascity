package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestNewSessionDoesNotClobberSaturatedSocket is the gascity-n17v regression
// test, and it reproduces the failure deterministically rather than
// approximating it.
//
// The mechanism: a tmux server that is alive but not accepting lets its 256-
// deep accept backlog fill, after which connect(2) is refused. tmux reads that
// refusal as "no server here", unlinks the socket, and binds a second server
// on the same path. Every session on the original server is orphaned, gc
// observes zero sessions, and rebuilds the whole fleet.
//
// The socket's inode is the assertion that matters: an unlink+bind replaces
// it, so an unchanged inode proves no second server was bound. tmux's exit
// code cannot stand in for it — the clobbering invocation exits 0.
func TestNewSessionDoesNotClobberSaturatedSocket(t *testing.T) {
	if os.Getenv("GC_TMUX_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_INTEGRATION=1 to run this real-tmux regression (freezes a throwaway tmux server)")
	}
	requireTmuxBinary(t)

	socketName := fmt.Sprintf("gcclobber%d", time.Now().UnixNano())
	tm := NewTmuxWithConfig(Config{SocketName: socketName})
	if err := tm.NewSession("victimzero", ""); err != nil {
		t.Fatalf("seeding the socket with a live server: %v", err)
	}
	t.Cleanup(func() { _, _ = tm.run("kill-server") })

	serverPID := tmuxServerPID(t, tm)
	socketPath := namedSocketPath(socketName)
	before := socketIdentity(t, socketPath)

	// Freeze the server: alive, holding the socket, never calling accept().
	if err := syscall.Kill(serverPID, syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP tmux server %d: %v", serverPID, err)
	}
	resumed := false
	resume := func() {
		if resumed {
			return
		}
		resumed = true
		_ = syscall.Kill(serverPID, syscall.SIGCONT)
	}
	t.Cleanup(resume)

	saturateAcceptBacklog(t, socketPath)

	err := tm.NewSession("victimclobber", "")

	if after := socketIdentity(t, socketPath); after != before {
		t.Fatalf("socket inode changed %s -> %s: tmux unlink+bound a second server on %s", before, after, socketPath)
	}
	if err == nil {
		t.Fatal("NewSession succeeded against a saturated server; it must refuse rather than clobber the socket")
	}
	if errors.Is(err, ErrNoServer) {
		t.Fatalf("err = %v, must not report a saturated server as ErrNoServer: absence is what makes gc rebuild the fleet", err)
	}
	if !errors.Is(err, ErrServerDegraded) {
		t.Fatalf("err = %v, want a degraded/saturated refusal", err)
	}

	// The server is still the one we started, still holding its session.
	resume()
	waitForSession(t, tm, "victimzero")
}

// TestColdStartCreatesServerWhenSocketAbsent is the other half of the
// contract: guarding the clobber must not cost gc the ability to start a
// server at all.
func TestColdStartCreatesServerWhenSocketAbsent(t *testing.T) {
	if os.Getenv("GC_TMUX_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_INTEGRATION=1 to run this real-tmux regression (spins a throwaway tmux server)")
	}
	requireTmuxBinary(t)

	socketName := fmt.Sprintf("gccoldstart%d", time.Now().UnixNano())
	socketPath := namedSocketPath(socketName)
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lstat %s = %v, want the socket to be absent before a cold start", socketPath, err)
	}

	tm := NewTmuxWithConfig(Config{SocketName: socketName})
	t.Cleanup(func() { _, _ = tm.run("kill-server") })
	if err := tm.NewSession("coldstart", ""); err != nil {
		t.Fatalf("cold start with no socket present: %v", err)
	}
	waitForSession(t, tm, "coldstart")
}

// TestGuardedCreateJoinsLiveServer covers the flag ordering the unit tests
// structurally cannot: they drive a fake executor, so nothing there proves
// real tmux accepts "-u -N -L <socket> new-session". Every session gc creates
// after the first takes this path, so a rejected flag order would break
// session creation everywhere while every unit test stayed green.
func TestGuardedCreateJoinsLiveServer(t *testing.T) {
	if os.Getenv("GC_TMUX_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_INTEGRATION=1 to run this real-tmux regression (spins a throwaway tmux server)")
	}
	requireTmuxBinary(t)

	socketName := fmt.Sprintf("gcguarded%d", time.Now().UnixNano())
	tm := NewTmuxWithConfig(Config{SocketName: socketName})
	t.Cleanup(func() { _, _ = tm.run("kill-server") })

	// First create is the cold start; it binds the socket.
	if err := tm.NewSession("guardedfirst", ""); err != nil {
		t.Fatalf("cold start: %v", err)
	}
	before := socketIdentity(t, namedSocketPath(socketName))

	// Second create runs against a live server, so it is guarded with -N.
	if err := tm.NewSession("guardedsecond", ""); err != nil {
		t.Fatalf("guarded create against a live server: %v", err)
	}
	if after := socketIdentity(t, namedSocketPath(socketName)); after != before {
		t.Fatalf("socket inode changed %s -> %s: the guarded create rebound the socket", before, after)
	}
	waitForSession(t, tm, "guardedfirst")
	waitForSession(t, tm, "guardedsecond")
}

func requireTmuxBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not installed: %v", err)
	}
}

func tmuxServerPID(t *testing.T, tm *Tmux) int {
	t.Helper()
	out, err := tm.run("display-message", "-p", "#{pid}")
	if err != nil {
		t.Fatalf("reading tmux server pid: %v", err)
	}
	pid, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("parsing tmux server pid %q: %v", out, err)
	}
	return pid
}

// socketIdentity returns the socket's inode. It is the only observation that
// distinguishes "tmux joined the existing server" from "tmux replaced it":
// the path is identical either way.
func socketIdentity(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return socketInode(info)
}

// saturateAcceptBacklog fills the frozen server's listen backlog so the kernel
// starts refusing connections. Slowness alone does not reproduce the bug: with
// room in the backlog, connect() still succeeds and tmux simply blocks. The
// clobber needs connect() to fail, which on a socket whose file still exists
// means ECONNREFUSED, which means the backlog is full.
func saturateAcceptBacklog(t *testing.T, socketPath string) {
	t.Helper()
	const attempts = 400
	held := make([]*os.File, 0, attempts)
	t.Cleanup(func() {
		for _, f := range held {
			_ = f.Close()
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; i < attempts; i++ {
		fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err != nil {
			t.Fatalf("socket(): %v", err)
		}
		// Connections that land in the backlog are kept open: closing them
		// would drain the very queue we are filling.
		if err := syscall.Connect(fd, unixSockaddr(t, socketPath)); err != nil {
			_ = syscall.Close(fd)
			if errors.Is(err, syscall.ECONNREFUSED) {
				return
			}
			t.Fatalf("connect(%s): %v", socketPath, err)
		}
		held = append(held, os.NewFile(uintptr(fd), socketPath))
		if time.Now().After(deadline) {
			break
		}
	}
	t.Fatalf("backlog never saturated after %d connections to %s", attempts, socketPath)
}

func unixSockaddr(t *testing.T, path string) syscall.Sockaddr {
	t.Helper()
	if len(path) >= 104 {
		t.Fatalf("socket path %q is too long for sockaddr_un", path)
	}
	return &syscall.SockaddrUnix{Name: path}
}

func waitForSession(t *testing.T, tm *Tmux, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if out, err := tm.run("list-sessions", "-F", "#{session_name}"); err == nil {
			for _, line := range strings.Split(out, "\n") {
				if strings.TrimSpace(line) == name {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("session %q never appeared on socket %s", name, tm.cfg.SocketName)
}
