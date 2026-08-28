//go:build !windows

package tmux

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLockSocketCreateFileIsMutuallyExclusive covers the cross-process half of
// the creation lock. The in-process semaphore cannot serialize what matters
// most here: gc runs as many short-lived processes, and two of them both
// deciding "the socket is absent, I may create the server" is the race the
// flock exists to lose deliberately.
//
// flock is held per open file description, so two acquisitions contend even
// from the same process — which is what makes this testable without spawning
// one.
func TestLockSocketCreateFileIsMutuallyExclusive(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", shortSocketDir(t))
	const socket = "gclockexclusive"

	release, err := lockSocketCreateFile(socket)
	if err != nil {
		t.Fatalf("first acquisition: %v", err)
	}

	// Bounded so a broken lock fails the test instead of hanging it.
	withCreateLockTimeout(t, 200*time.Millisecond)
	start := time.Now()
	if contended, err := lockSocketCreateFile(socket); err == nil {
		contended()
		release()
		t.Fatal("second acquisition succeeded while the lock was held")
	}
	if waited := time.Since(start); waited < 200*time.Millisecond {
		t.Fatalf("contended acquisition gave up after %s, want it to wait out the full budget", waited)
	}

	release()

	withCreateLockTimeout(t, 5*time.Second)
	reacquired, err := lockSocketCreateFile(socket)
	if err != nil {
		t.Fatalf("re-acquisition after release: %v", err)
	}
	reacquired()
}

// TestLockSocketCreateFileCreatesSocketDir covers the genuinely cold host,
// where nothing has created <tmpdir>/tmux-<uid> yet. 0o700 is not incidental:
// tmux refuses a socket directory that is group- or world-accessible.
func TestLockSocketCreateFileCreatesSocketDir(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", shortSocketDir(t))
	const socket = "gclockcolddir"

	release, err := lockSocketCreateFile(socket)
	if err != nil {
		t.Fatalf("acquiring the lock on a cold host: %v", err)
	}
	defer release()

	if _, err := os.Stat(socketCreateLockPath(socket)); err != nil {
		t.Fatalf("lock file was not created: %v", err)
	}
	dir, err := os.Stat(filepath.Dir(namedSocketPath(socket)))
	if err != nil {
		t.Fatalf("socket dir was not created: %v", err)
	}
	if perm := dir.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("socket dir mode = %o, want no group or other access (tmux refuses those)", perm)
	}
}

func withCreateLockTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := socketCreateLockTimeout
	socketCreateLockTimeout = d
	t.Cleanup(func() { socketCreateLockTimeout = prev })
}
