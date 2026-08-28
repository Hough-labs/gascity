package tmux

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// socketHolderState answers "is a live process bound to this named tmux
// socket?" — the question connect(2) cannot answer on its own.
//
// A unix socket whose accept backlog is full refuses connections with
// ECONNREFUSED, byte-for-byte the same observation as a socket file left
// behind by a server that died without unlinking it. Treating the first as
// the second is the whole bug: tmux then unlinks the path, binds a second
// server on it, and orphans every session on the original (gascity-n17v).
// Distinguishing them requires an out-of-band observation of who holds the
// socket, which is what this type carries.
type socketHolderState int

const (
	// socketHolderUnknown means the holder could not be determined. Callers
	// MUST treat it as "possibly held" and fail closed: refusing to create a
	// session costs a retry, while creating one over a live server costs the
	// fleet.
	socketHolderUnknown socketHolderState = iota
	// socketHolderPresent means a live process is bound to the socket. A
	// refused connect() against it is saturation, not absence.
	socketHolderPresent
	// socketHolderAbsent means nothing holds the socket: the file is a stale
	// artifact and letting tmux unlink and rebind it cannot orphan anything.
	socketHolderAbsent
)

// procNetUnixPath is Linux's authoritative listing of bound unix sockets. It
// is absent on every other platform, which selects the process-table
// observation below. procNetUnixHolder takes the path as a parameter so tests
// can point it at a fixture.
const procNetUnixPath = "/proc/net/unix"

// socketHolderPSTimeout bounds the process-table snapshot. The probe only runs
// on a host that is already refusing connections, so it must not become a new
// way to hang; a snapshot that overruns reports "unknown" and fails closed.
const socketHolderPSTimeout = 5 * time.Second

// namedSocketHolder reports whether a live process holds the socket at path.
//
// It never reports socketHolderAbsent on a guess: absence is claimed only from
// a listing that was read successfully and did not contain the socket.
func namedSocketHolder(ctx context.Context, path string) socketHolderState {
	if state, decided := procNetUnixHolder(procNetUnixPath, path); decided {
		return state
	}
	// The snapshot inspects local process state, not the tmux server that is
	// refusing connections, so it gets its own bounded budget rather than
	// whatever is left of the caller's deliberately short server-probe window.
	// Spending that window here would report "unknown" — and fail closed on a
	// socket that is merely stale — every time the host is loaded.
	return processTableHolder(context.WithoutCancel(ctx), path)
}

// procNetUnixHolder reads Linux's bound-socket table. The decided return is
// false when the table does not exist (any non-Linux host), which selects the
// process-table fallback; it is true — with socketHolderUnknown — when the
// table exists but could not be read, because a half-read table is not
// evidence of absence.
func procNetUnixHolder(listingPath, socketPath string) (state socketHolderState, decided bool) {
	f, err := os.Open(listingPath)
	if err != nil {
		return socketHolderUnknown, false
	}
	defer func() { _ = f.Close() }()

	wanted := socketPathAliases(socketPath)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// Columns are whitespace-separated and the bound path, when present,
		// is last. tmux socket paths never contain whitespace: they are
		// <tmpdir>/tmux-<uid>/<socket-name> and gc validates the name.
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if _, ok := wanted[fields[len(fields)-1]]; ok {
			return socketHolderPresent, true
		}
	}
	if err := scanner.Err(); err != nil {
		return socketHolderUnknown, true
	}
	return socketHolderAbsent, true
}

// processTableHolder looks for a tmux process bound to the socket. It is the
// observation of last resort, used where the kernel exposes no bound-socket
// table (macOS): lsof answers this exactly but takes seconds even on an idle
// host, which is far too slow for a path that runs while the fleet is already
// degraded.
//
// The match is deliberately generous — a blocked client counts as a holder —
// because over-reporting a holder only costs a retry, while under-reporting
// one authorizes the clobber.
func processTableHolder(ctx context.Context, socketPath string) socketHolderState {
	ctx, cancel := context.WithTimeout(ctx, socketHolderPSTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-Awwo", "pid=,args=").Output()
	if err != nil {
		return socketHolderUnknown
	}

	socketName := filepath.Base(socketPath)
	aliases := socketPathAliases(socketPath)
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if !fieldsReferenceTmuxSocket(fields, socketName, aliases) {
			continue
		}
		return socketHolderPresent
	}
	if err := scanner.Err(); err != nil {
		return socketHolderUnknown
	}
	return socketHolderAbsent
}

// fieldsReferenceTmuxSocket reports whether one `ps` row is a tmux process
// bound to the socket. Both spellings tmux can show are accepted: the client
// argv a macOS server keeps ("tmux -L <name> ..." / "-S <path>"), and the
// process title a Linux server rewrites itself to ("tmux: server (<path>)").
func fieldsReferenceTmuxSocket(fields []string, socketName string, aliases map[string]struct{}) bool {
	if len(fields) < 2 {
		return false
	}
	command := fields[1:]
	if !strings.Contains(filepath.Base(strings.TrimSuffix(command[0], ":")), "tmux") {
		return false
	}
	for i, field := range command {
		switch field {
		case "-L":
			if i+1 < len(command) && command[i+1] == socketName {
				return true
			}
		case "-S":
			if i+1 < len(command) {
				if _, ok := aliases[command[i+1]]; ok {
					return true
				}
			}
		}
		// Linux server title: tmux: server (/tmp/tmux-1000/gc-city)
		if _, ok := aliases[strings.Trim(field, "()")]; ok {
			return true
		}
	}
	return false
}

// socketPathAliases returns every spelling of the socket path a listing might
// use. macOS resolves /tmp through a symlink to /private/tmp, so a literal
// comparison against the configured path alone would miss the holder.
func socketPathAliases(socketPath string) map[string]struct{} {
	aliases := map[string]struct{}{socketPath: {}}
	if resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(socketPath)); err == nil {
		aliases[filepath.Join(resolvedDir, filepath.Base(socketPath))] = struct{}{}
	}
	return aliases
}
