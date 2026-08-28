//go:build !windows

package tmux

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// socketCreateLockPath is the gc-owned lock file guarding the single tmux
// invocation allowed to create a server on a named socket.
//
// It is deliberately NOT tmux's own "<socket>.lock": tmux's client takes that
// lock itself while starting a server, so holding it here would deadlock the
// very command this lock exists to protect.
func socketCreateLockPath(socketName string) string {
	return namedSocketPath(socketName) + ".gc-create.lock"
}

// lockSocketCreateFile takes an exclusive, cross-process lock on the socket's
// creation lock file and returns the release function.
//
// The wait is bounded and jittered: a gc process wedged inside its own create
// must not lock every other process out of the fleet, and an unjittered retry
// wave is itself a source of the connect() storm that saturates the server.
func lockSocketCreateFile(socketName string) (func(), error) {
	path := socketCreateLockPath(socketName)
	// 0o700 matches what tmux itself requires of the socket directory: it
	// refuses to use one that is group- or world-accessible.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating tmux socket dir for %q: %w", socketName, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening session-creation lock %q: %w", path, err)
	}

	deadline := time.Now().Add(socketCreateLockTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("locking %q: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("timed out after %s waiting for session-creation lock %q", socketCreateLockTimeout, path)
		}
		time.Sleep(socketCreateLockRetryBase + time.Duration(rand.Int63n(int64(socketCreateLockRetryBase))))
	}
}
