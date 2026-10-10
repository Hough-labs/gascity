package tmux

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestErrServerSaturatedIsNotAbsence pins the sentinel wiring the whole fix
// rests on. gc absorbs ErrNoServer as "nothing is running here" at more than a
// dozen call sites; a saturated server that reports absence therefore makes gc
// rebuild a fleet whose sessions are all still alive.
func TestErrServerSaturatedIsNotAbsence(t *testing.T) {
	if errors.Is(ErrServerSaturated, ErrNoServer) {
		t.Fatal("ErrServerSaturated must not satisfy errors.Is(err, ErrNoServer)")
	}
	if !errors.Is(ErrServerSaturated, ErrServerDegraded) {
		t.Fatal("ErrServerSaturated must satisfy errors.Is(err, ErrServerDegraded) so existing degraded handling applies")
	}
}

// TestGuardedCreateReportsSaturationNotAbsence covers the trap the -N flag
// sets: -N never starts a server, so it answers a refused connect with
// "no server running" — the exact string that maps to ErrNoServer. Passing
// that through unclassified would be strictly worse than the clobber it
// replaced.
func TestGuardedCreateReportsSaturationNotAbsence(t *testing.T) {
	fe := probeAssertSet(
		[]string{"", ""},
		// Alive at preflight, saturated by the time the create connects.
		[]error{ErrSessionNotFound, ErrNoServer},
	)
	tm := &Tmux{cfg: Config{SocketName: "gc-test"}, exec: fe}

	err := tm.NewSession("gc-saturated", "")
	if err == nil {
		t.Fatal("NewSession = nil, want a saturation refusal")
	}
	if errors.Is(err, ErrNoServer) {
		t.Fatalf("err = %v, must not report a refused -N create as absence", err)
	}
	if !errors.Is(err, ErrServerSaturated) {
		t.Fatalf("err = %v, want ErrServerSaturated", err)
	}
	if !strings.Contains(err.Error(), namedSocketPath("gc-test")) {
		t.Errorf("err = %q, want the socket path for the operator", err)
	}
	assertNewSessionCall(t, fe.calls[1], true)
}

// TestGuardedCreatePreservesSessionExists guards EnsureSessionFresh, which
// treats ErrSessionExists as "already there, check for a zombie". Classifying
// every guarded failure as saturation would break that path.
func TestGuardedCreatePreservesSessionExists(t *testing.T) {
	fe := probeAssertSet([]string{"", ""}, []error{ErrSessionNotFound, ErrSessionExists})
	tm := &Tmux{cfg: Config{SocketName: "gc-test"}, exec: fe}

	err := tm.NewSession("gc-dup", "")
	if !errors.Is(err, ErrSessionExists) {
		t.Fatalf("err = %v, want ErrSessionExists to survive the guarded create", err)
	}
}

// TestProbeReportsSaturationWhenSocketHasLiveHolder is the preflight half of
// the same distinction: tmux says "no server" because the connect was refused,
// but a live process still holds the socket.
func TestProbeReportsSaturationWhenSocketHasLiveHolder(t *testing.T) {
	fe := &fakeExecutor{err: ErrNoServer}
	tm := &Tmux{
		cfg:  Config{SocketName: "gc-test"},
		exec: fe,
		serverSocketObserver: func(context.Context, string) error {
			return fmt.Errorf("%w: path=/tmp/gc-test inode=97 reason=refused-by-live-holder", errSocketHolderLive)
		},
	}

	err := tm.NewSession("gc-held", "")
	if !errors.Is(err, ErrServerSaturated) {
		t.Fatalf("err = %v, want ErrServerSaturated", err)
	}
	if errors.Is(err, ErrNoServer) {
		t.Fatalf("err = %v, must not wrap ErrNoServer", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("calls = %#v, want only the preflight probe: nothing may reach new-session", fe.calls)
	}
}

// TestColdStartFallsThroughWhenServerAppearsUnderLock covers the cross-process
// race the creation lock exists for: another gc process starts the server
// between our preflight and our lock. Creating unguarded at that point is the
// clobber, so the re-check must demote the call to a guarded one.
func TestColdStartFallsThroughWhenServerAppearsUnderLock(t *testing.T) {
	fe := probeAssertSet(
		[]string{"", "", "", ""},
		// preflight: no server -> cold start; re-check under the lock: a
		// server answered; guarded create; ConfigureServer.
		[]error{ErrNoServer, ErrSessionNotFound, nil, nil},
	)
	observerCalls := 0
	tm := &Tmux{
		cfg:  Config{SocketName: "gc-test"},
		exec: fe,
		serverSocketObserver: func(context.Context, string) error {
			observerCalls++
			return nil
		},
	}

	if err := tm.NewSession("gc-raced", ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if observerCalls != 1 {
		t.Fatalf("observer calls = %d, want 1: the re-check found a live server and never observed the socket", observerCalls)
	}
	if len(fe.calls) < 3 {
		t.Fatalf("calls = %#v, want two probes followed by a create", fe.calls)
	}
	assertNewSessionCall(t, fe.calls[2], true)
}

// serializingExecutor records whether two creates were ever in flight at once
// against the same socket.
type serializingExecutor struct {
	mu       sync.Mutex
	inFlight int
	overlaps int
	creates  int
}

func (s *serializingExecutor) execute(args []string) (string, error) {
	return s.executeCtx(context.Background(), args)
}

func (s *serializingExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	isCreate := false
	for _, a := range args {
		if a == "new-session" {
			isCreate = true
			break
		}
	}
	if !isCreate {
		// Answer the preflight as a healthy server holding no probe session.
		return "", ErrSessionNotFound
	}
	s.mu.Lock()
	s.creates++
	s.inFlight++
	if s.inFlight > 1 {
		s.overlaps++
	}
	s.mu.Unlock()

	time.Sleep(5 * time.Millisecond)

	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	return "", nil
}

// TestNewSessionSerializesCreationPerSocket covers the contributing defect:
// nothing serialized session creation, so a reconciler rebuilding a dozen
// seats issued a dozen simultaneous connect() calls on one socket — gc's own
// fan-out feeding the saturation that lets tmux clobber it.
func TestNewSessionSerializesCreationPerSocket(t *testing.T) {
	se := &serializingExecutor{}
	tm := &Tmux{cfg: Config{SocketName: "gc-serialized"}, exec: se}

	const seats = 12
	var wg sync.WaitGroup
	for i := range seats {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tm.NewSession(fmt.Sprintf("gcseat%d", i), ""); err != nil {
				t.Errorf("NewSession: %v", err)
			}
		}()
	}
	wg.Wait()

	se.mu.Lock()
	defer se.mu.Unlock()
	if se.creates != seats {
		t.Fatalf("creates = %d, want %d", se.creates, seats)
	}
	if se.overlaps != 0 {
		t.Fatalf("%d concurrent new-session invocations against one socket; creation must be single-file", se.overlaps)
	}
}

// TestGuardedCreateStagesSecretEnvUnderNoStartServer covers the composition of
// the two new-session defenses. A secret-bearing environment moves the create
// into a private command file, and runNewSession leads that file with
// start-server — a command allowed to create a server, so on a refused connect
// it takes the unlink+bind path -N exists to close. Against a server the
// preflight found alive, the staged create must therefore carry -N and must
// not lead with start-server, while the secret still stays off argv.
func TestGuardedCreateStagesSecretEnvUnderNoStartServer(t *testing.T) {
	const secret = "sk-test-not-a-real-credential"
	fe := probeAssertSet([]string{"", ""}, []error{ErrSessionNotFound, nil})
	tm := &Tmux{cfg: Config{SocketName: "gc-test"}, exec: fe}

	env := map[string]string{"ANTHROPIC_AUTH_TOKEN": secret}
	if err := tm.NewSessionWithCommandAndEnv("gc-staged", "", "claude", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if len(fe.calls) < 2 || !firstArgsContainHasSession(fe.calls[0]) {
		t.Fatalf("calls = %#v, want the preflight probe followed by the create", fe.calls)
	}
	create := fe.calls[1]
	verb := slices.Index(create, "source-file")
	if verb < 0 {
		t.Fatalf("create = %v, want a staged source-file invocation", create)
	}
	if !slices.Contains(create[:verb], "-N") {
		t.Fatalf("create = %v, want -N ahead of source-file so the staged create cannot start a server", create)
	}
	if slices.Contains(create, "start-server") {
		t.Fatalf("create = %v, must not lead with start-server against a live socket", create)
	}
	for _, call := range fe.calls {
		for _, arg := range call {
			if strings.Contains(arg, secret) {
				t.Fatalf("secret value reached tmux argv: %v", call)
			}
		}
	}
}

// TestColdStartStagesSecretEnvWithStartServer is the other mode: a cold start
// is the one create allowed to found the server, so its staged form keeps
// runNewSession's start-server lead and carries no -N.
func TestColdStartStagesSecretEnvWithStartServer(t *testing.T) {
	fe := probeAssertSet([]string{"", "", "", ""}, []error{ErrNoServer, ErrNoServer, nil, nil})
	tm := &Tmux{
		cfg:                  Config{SocketName: "gc-test"},
		exec:                 fe,
		serverSocketObserver: func(context.Context, string) error { return nil },
	}

	env := map[string]string{"ANTHROPIC_AUTH_TOKEN": "sk-test-not-a-real-credential"}
	if err := tm.NewSessionWithCommandAndEnv("gc-cold-staged", "", "claude", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if len(fe.calls) < 3 {
		t.Fatalf("calls = %#v, want two probes followed by the create", fe.calls)
	}
	create := fe.calls[2]
	if !slices.Contains(create, "start-server") || !slices.Contains(create, "source-file") {
		t.Fatalf("create = %v, want start-server ; source-file", create)
	}
	if slices.Contains(create, "-N") {
		t.Fatalf("create = %v, a cold start must be free to create the server", create)
	}
}

// TestGuardedStagedCreateReportsSaturationNotAbsence keeps the -N
// classification on the staged path: a refused source-file under -N reads as
// "no server running", which must surface as saturation, never absence.
func TestGuardedStagedCreateReportsSaturationNotAbsence(t *testing.T) {
	fe := probeAssertSet([]string{"", ""}, []error{ErrSessionNotFound, ErrNoServer})
	tm := &Tmux{cfg: Config{SocketName: "gc-test"}, exec: fe}

	err := tm.NewSessionWithCommandAndEnv("gc-staged-saturated", "", "claude",
		map[string]string{"ANTHROPIC_AUTH_TOKEN": "sk-test-not-a-real-credential"})
	if errors.Is(err, ErrNoServer) {
		t.Fatalf("err = %v, must not report a refused -N create as absence", err)
	}
	if !errors.Is(err, ErrServerSaturated) {
		t.Fatalf("err = %v, want ErrServerSaturated", err)
	}
}
