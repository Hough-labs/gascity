package tmux

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// namedSocketPath resolves the exact path tmux uses for a named -L socket.
// tmux honors TMUX_TMPDIR here; TMPDIR is deliberately not a fallback.
func namedSocketPath(socketName string) string {
	tmpDir := os.Getenv("TMUX_TMPDIR")
	if tmpDir == "" {
		tmpDir = "/tmp"
	}
	return filepath.Join(tmpDir, fmt.Sprintf("tmux-%d", os.Getuid()), socketName)
}

// authorizeColdStart records WHICH observation concluded the socket is safe to
// rebind, and returns the nil that says so.
//
// Every safe verdict in this file is a bare `return nil`, and the only line the
// operator ever sees is tmux.go's "COLD START authorized" — which names the
// action but never the evidence. When a cold start took a live tmux server out
// from under an agent nobody had touched (gc-eazs), the four paths that can
// return nil were indistinguishable after the fact, and localizing it took a
// session of process forensics against `ps` samples. The reason costs one log
// line on a path that runs about once per city start.
func authorizeColdStart(path string, reason string) error {
	log.Printf("tmux socket observation: cold start authorized for %s (reason=%s)", path, reason)
	return nil
}

// errSocketHolderLive marks the one observation that means "a live process is
// bound to this socket and is refusing connections" — a saturated accept
// backlog rather than a dead server. Callers map it to ErrServerSaturated so
// the condition is reported as transient rather than as absence.
var errSocketHolderLive = errors.New("live process holds the socket")

// observeNamedSocket distinguishes a safely absent or stale named socket from
// a socket that might still belong to a live server. It fails closed whenever
// its filesystem, dial and holder observations cannot prove it is safe to
// create.
func observeNamedSocket(ctx context.Context, path string) error {
	return observeNamedSocketUsing(ctx, path, os.Lstat, func(ctx context.Context, path string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}, namedSocketHolder)
}

// observeNamedSocketWith keeps the socket policy testable without opening a
// listener, using the production holder observation.
func observeNamedSocketWith(
	ctx context.Context,
	path string,
	lstat func(string) (os.FileInfo, error),
	dial func(context.Context, string) (net.Conn, error),
) error {
	return observeNamedSocketUsing(ctx, path, lstat, dial, namedSocketHolder)
}

// observeNamedSocketUsing additionally injects the holder observation. The
// lstat calls are context-bounded from the caller's perspective: an OS syscall
// already in progress cannot be canceled, but its buffered result cannot hold
// the caller after the context ends.
func observeNamedSocketUsing(
	ctx context.Context,
	path string,
	lstat func(string) (os.FileInfo, error),
	dial func(context.Context, string) (net.Conn, error),
	holder func(context.Context, string) socketHolderState,
) error {
	before, err := lstatWithContext(ctx, lstat, path)
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("path=%s inode=unknown peer_pid=unknown lstat=%w", path, contextErr)
	}
	if errors.Is(err, os.ErrNotExist) {
		// The socket FILE is absent, which is NOT proof the server is gone: a
		// tmux server survives having its socket unlinked and keeps every
		// session bound to it. Cold-starting here binds a second server on the
		// path and orphans the whole fleet — the exact outcome this file
		// exists to prevent, reached through the one branch that never asked
		// who holds the socket. An unlinked-but-live socket is precisely the
		// residue a partial clobber leaves behind, so this branch is what
		// turns a single clobber into a repeating one (gascity-3z7d).
		//
		// The holder observation keys on the socket NAME in the process table,
		// so it still answers with the file gone. Absence is claimed only from
		// a listing that was read successfully and did not contain it.
		switch holder(ctx, path) {
		case socketHolderAbsent:
			return authorizeColdStart(path, "absent-path-unheld")
		case socketHolderPresent:
			// Deliberately NOT errSocketHolderLive: that maps to
			// ErrServerSaturated, which advertises "transient, retry" — but no
			// amount of retrying re-links a socket. This is a stuck state an
			// operator must see, so it becomes ErrServerDegraded and is
			// refused loudly instead of quietly costing the fleet.
			return fmt.Errorf("path=%s inode=absent peer_pid=unknown reason=unlinked-socket-live-holder", path)
		default:
			return fmt.Errorf("path=%s inode=absent peer_pid=unknown reason=socket-holder-unknown-on-absent-path", path)
		}
	}
	if err != nil {
		return fmt.Errorf("path=%s inode=unknown peer_pid=unknown lstat=%w", path, err)
	}
	inode := socketInode(before)
	if before.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("path=%s inode=%s peer_pid=unknown reason=not-unix-socket", path, inode)
	}

	conn, err := dial(ctx, path)
	if err == nil {
		defer func() { _ = conn.Close() }()
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			return fmt.Errorf("path=%s inode=%s peer_pid=unknown reason=unexpected-connection-type-%T", path, inode, conn)
		}
		peerPID, peerErr := socketPeerPID(unixConn)
		if peerErr != nil {
			return fmt.Errorf("path=%s inode=%s peer_pid=unknown peer_pid_reason=%w", path, inode, peerErr)
		}
		return fmt.Errorf("path=%s inode=%s peer_pid=%d reason=live-unix-socket", path, inode, peerPID)
	}

	after, afterErr := lstatWithContext(ctx, lstat, path)
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("path=%s inode=%s peer_pid=unknown post_lstat=%w", path, inode, contextErr)
	}
	pathAbsent := errors.Is(afterErr, os.ErrNotExist)
	stable := afterErr == nil && os.SameFile(before, after)
	if errors.Is(err, syscall.ECONNREFUSED) && pathAbsent {
		// The connect was refused AND the socket file vanished between the two
		// lstats. That combination is not absence — it is the
		// unlinked-socket-live-holder state observed mid-probe. Both halves are
		// signals this file already treats as ambiguous on their own: a live
		// server with a full accept backlog refuses identically (gascity-n17v),
		// and a server survives having its socket unlinked with every session
		// still bound to it (gascity-3z7d). Arriving with BOTH is the strongest
		// available evidence of a live holder, yet this was the one branch that
		// never asked. Returning nil here authorizes tmux to bind a second
		// server on the path and orphan every session on the first (gc-eazs).
		switch holder(ctx, path) {
		case socketHolderAbsent:
			return authorizeColdStart(path, "refused-path-vanished-unheld")
		case socketHolderPresent:
			// Deliberately NOT errSocketHolderLive, for the same reason as the
			// absent-path branch: no amount of retrying re-links a socket, so
			// advertising "transient, retry" would spin instead of surfacing a
			// state an operator must resolve. The inode is the one observed
			// before the path vanished, which keeps this route distinguishable
			// in logs from the already-absent one (inode=absent) while both
			// report the same reason and demand the same action.
			return fmt.Errorf("path=%s inode=%s peer_pid=unknown reason=unlinked-socket-live-holder", path, inode)
		default:
			return fmt.Errorf("path=%s inode=%s peer_pid=unknown reason=socket-holder-unknown-on-absent-path", path, inode)
		}
	}
	if errors.Is(err, syscall.ECONNREFUSED) && stable {
		// The socket file is still here and refused the connection. That is
		// NOT proof the server is gone: a live server whose accept backlog is
		// full refuses connections identically (gascity-n17v). Only a
		// positive "nothing holds this socket" observation makes it safe to
		// let tmux unlink and rebind the path.
		switch holder(ctx, path) {
		case socketHolderAbsent:
			return authorizeColdStart(path, "refused-stale-unheld")
		case socketHolderPresent:
			return fmt.Errorf("%w: path=%s inode=%s peer_pid=unknown reason=refused-by-live-holder", errSocketHolderLive, path, inode)
		default:
			return fmt.Errorf("path=%s inode=%s peer_pid=unknown reason=socket-holder-unknown", path, inode)
		}
	}
	if errors.Is(err, os.ErrNotExist) && pathAbsent {
		return authorizeColdStart(path, "dial-and-path-absent")
	}
	if afterErr != nil {
		return fmt.Errorf("path=%s inode=%s peer_pid=unknown dial=%w post_lstat=%w", path, inode, err, afterErr)
	}
	return fmt.Errorf("path=%s inode=%s peer_pid=unknown dial=%w post_inode=%s reason=socket-identity-changed-or-dial-failed", path, inode, err, socketInode(after))
}

type lstatResult struct {
	info os.FileInfo
	err  error
}

func lstatWithContext(ctx context.Context, lstat func(string) (os.FileInfo, error), path string) (os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make(chan lstatResult, 1)
	go func() {
		info, err := lstat(path)
		result <- lstatResult{info: info, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-result:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return result.info, result.err
	}
}
