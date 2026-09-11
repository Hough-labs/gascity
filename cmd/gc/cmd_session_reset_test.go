package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCmdSessionReset_ClearsCircuitBreaker verifies that running
// `gc session reset <identity>` clears a tripped session circuit breaker
// for the matching named session, so the supervisor will respawn the
// session on the next tick. This is the operator-facing remediation path
// the breaker's ERROR log message points at.
func TestCmdSessionReset_ClearsCircuitBreaker(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-reset-cb-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	const identity = "session-a"
	bead, err := store.Create(beads.Bead{
		Title:  "named session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                        identity,
			"template":                     "worker",
			"session_name":                 "s-gc-reset-cb-test",
			"state":                        "awake",
			namedSessionMetadataKey:        "true",
			namedSessionIdentityMetadata:   identity,
			sessionCircuitStateMetadata:    circuitOpen.String(),
			sessionCircuitRestartsMetadata: `["2026-04-10T12:00:00Z"]`,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	// Trip the breaker by recording enough restarts inside
	// the rolling window with no progress events.
	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{
		Window:      30 * time.Minute,
		MaxRestarts: 3,
	})
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()
	now := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		cb.RecordRestart(identity, now.Add(time.Duration(i)*time.Second))
	}
	if !cb.IsOpen(identity, now.Add(time.Minute)) {
		t.Fatalf("precondition: expected breaker OPEN for %q after 4 restarts", identity)
	}

	lis, err := startControllerSocket(
		cityDir,
		func() {},
		nil,
		nil,
		make(chan reloadRequest),
		make(chan convergenceRequest, 1),
		make(chan struct{}, 1),
		make(chan struct{}, 1),
	)
	if err != nil {
		t.Fatalf("startControllerSocket: %v", err)
	}
	defer lis.Close()                              //nolint:errcheck
	defer os.Remove(controllerSocketPath(cityDir)) //nolint:errcheck

	// This test predates the confirmation wait (gascity-ksa part 3) and its
	// subject is the request path: the circuit breaker must be cleared before
	// the restart is queued. wait=0 selects the fire-and-forget mode so no
	// controller has to be simulated and every assertion below is unchanged.
	var stdout, stderr bytes.Buffer
	if code := cmdSessionResetWithOptions([]string{identity}, &stdout, &stderr, sessionResetOptions{}); code != 0 {
		t.Fatalf("cmdSessionReset = %d, want 0; stderr=%s", code, stderr.String())
	}

	if cb.IsOpen(identity, now.Add(time.Minute)) {
		t.Fatalf("breaker still OPEN for %q after `gc session reset %s`", identity, identity)
	}
	updated, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(session bead): %v", err)
	}
	if got := updated.Metadata[sessionCircuitStateMetadata]; got != "" {
		t.Fatalf("persisted circuit state = %q, want cleared", got)
	}
	if got := updated.Metadata[sessionCircuitRestartsMetadata]; got != "" {
		t.Fatalf("persisted restart history = %q, want cleared", got)
	}
	if got := updated.Metadata[sessionCircuitResetGenerationMetadata]; got == "" {
		t.Fatal("persisted reset generation is empty, want explicit reset generation")
	}
}

func TestCmdSessionReset_ProviderConstructionFailureReturnsError(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "broken")

	cityDir := shortSocketTempDir(t, "gc-session-reset-provider-error-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	writeBuiltinImportsFixture(t, cityDir, "core")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "manual session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:session-a"},
		Metadata: map[string]string{
			"alias":        "sky",
			"template":     "session-a",
			"session_name": "s-gc-reset-provider-error",
			"state":        "awake",
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	lis, err := startControllerSocket(
		cityDir,
		func() {},
		nil,
		nil,
		make(chan reloadRequest),
		make(chan convergenceRequest, 1),
		make(chan struct{}, 1),
		make(chan struct{}, 1),
	)
	if err != nil {
		t.Fatalf("startControllerSocket: %v", err)
	}
	defer lis.Close()                              //nolint:errcheck
	defer os.Remove(controllerSocketPath(cityDir)) //nolint:errcheck

	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return nil, errors.New("injected provider failure")
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	var stdout, stderr bytes.Buffer
	if code := cmdSessionReset([]string{"sky"}, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdSessionReset = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
	if got, want := stderr.String(), "gc session reset: constructing session provider: injected provider failure\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestCmdSessionKill_ClearsCircuitBreaker(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-kill-cb-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	fakeProvider := runtime.NewFake()
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fakeProvider, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	const identity = "session-a"
	const sessionName = "s-gc-kill-cb-test"
	bead, err := store.Create(beads.Bead{
		Title:  "named session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                        identity,
			"template":                     "worker",
			"session_name":                 sessionName,
			"state":                        "awake",
			namedSessionMetadataKey:        "true",
			namedSessionIdentityMetadata:   identity,
			sessionCircuitStateMetadata:    circuitOpen.String(),
			sessionCircuitRestartsMetadata: `["2026-04-10T12:00:00Z"]`,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}
	if err := fakeProvider.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("fakeProvider.Start: %v", err)
	}
	if err := fakeProvider.SetMeta(sessionName, "GC_SESSION_ID", bead.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{
		Window:      30 * time.Minute,
		MaxRestarts: 3,
	})
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()
	now := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		cb.RecordRestart(identity, now.Add(time.Duration(i)*time.Second))
	}
	if !cb.IsOpen(identity, now.Add(time.Minute)) {
		t.Fatalf("precondition: expected breaker OPEN for %q after 4 restarts", identity)
	}

	lis, err := startControllerSocket(
		cityDir,
		func() {},
		nil,
		nil,
		make(chan reloadRequest),
		make(chan convergenceRequest, 1),
		make(chan struct{}, 1),
		make(chan struct{}, 1),
	)
	if err != nil {
		t.Fatalf("startControllerSocket: %v", err)
	}
	defer lis.Close()                              //nolint:errcheck
	defer os.Remove(controllerSocketPath(cityDir)) //nolint:errcheck

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
	}

	if fakeProvider.IsRunning(sessionName) {
		t.Fatalf("session %q still running after kill", sessionName)
	}
	if cb.IsOpen(identity, now.Add(time.Minute)) {
		t.Fatalf("breaker still OPEN for %q after `gc session kill %s`", identity, identity)
	}
	updated, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(session bead): %v", err)
	}
	if got := updated.Metadata[sessionCircuitStateMetadata]; got != "" {
		t.Fatalf("persisted circuit state = %q, want cleared", got)
	}
	if got := updated.Metadata[sessionCircuitRestartsMetadata]; got != "" {
		t.Fatalf("persisted restart history = %q, want cleared", got)
	}
	if got := updated.Metadata[sessionCircuitResetGenerationMetadata]; got == "" {
		t.Fatal("persisted reset generation is empty, want explicit reset generation")
	}
}

// TestCmdSessionKill_SyncsBeadToAsleep is the regression guard for #3629:
// `gc session kill` must sync the bead to asleep + refresh synced_at at the
// CLI layer (cmdSessionKill), otherwise the bead retains its prior live state
// ("awake") and a later `gc session wake` short-circuits on the stale
// metadata and never starts a fresh runtime. The write lives in cmdSessionKill
// (not Manager.Kill) so the drain-ack async-stop path is unaffected.
func TestCmdSessionKill_SyncsBeadToAsleep(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-kill-asleep-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	fakeProvider := runtime.NewFake()
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fakeProvider, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	const identity = "session-a"
	const sessionName = "s-gc-kill-asleep-test"
	bead, err := store.Create(beads.Bead{
		Title:  "named session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                      identity,
			"template":                   "worker",
			"session_name":               sessionName,
			"state":                      "awake",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}
	if err := fakeProvider.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("fakeProvider.Start: %v", err)
	}
	if err := fakeProvider.SetMeta(sessionName, "GC_SESSION_ID", bead.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	lis, err := startControllerSocket(
		cityDir,
		func() {},
		nil,
		nil,
		make(chan reloadRequest),
		make(chan convergenceRequest, 1),
		make(chan struct{}, 1),
		make(chan struct{}, 1),
	)
	if err != nil {
		t.Fatalf("startControllerSocket: %v", err)
	}
	defer lis.Close()                              //nolint:errcheck
	defer os.Remove(controllerSocketPath(cityDir)) //nolint:errcheck

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
	}

	updated, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(session bead): %v", err)
	}
	if got := updated.Metadata["state"]; got != string(session.StateAsleep) {
		t.Errorf("post-kill state = %q, want %q", got, session.StateAsleep)
	}
	if got := updated.Metadata["synced_at"]; got == "" {
		t.Error("post-kill synced_at is empty, want a refreshed timestamp")
	}
}

func TestCmdSessionKill_ClearsCircuitBreakerForAsleepNamedSession(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-kill-cb-asleep-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	const identity = "session-a"
	bead, err := store.Create(beads.Bead{
		Title:  "named session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                        identity,
			"template":                     "worker",
			"session_name":                 "s-gc-kill-cb-asleep-test",
			"state":                        string(session.StateAsleep),
			namedSessionMetadataKey:        "true",
			namedSessionIdentityMetadata:   identity,
			sessionCircuitStateMetadata:    circuitOpen.String(),
			sessionCircuitRestartsMetadata: `["2026-04-10T12:00:00Z"]`,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{
		Window:      30 * time.Minute,
		MaxRestarts: 3,
	})
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()
	now := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		cb.RecordRestart(identity, now.Add(time.Duration(i)*time.Second))
	}
	if !cb.IsOpen(identity, now.Add(time.Minute)) {
		t.Fatalf("precondition: expected breaker OPEN for %q after 4 restarts", identity)
	}

	lis, err := startControllerSocket(
		cityDir,
		func() {},
		nil,
		nil,
		make(chan reloadRequest),
		make(chan convergenceRequest, 1),
		make(chan struct{}, 1),
		make(chan struct{}, 1),
	)
	if err != nil {
		t.Fatalf("startControllerSocket: %v", err)
	}
	defer lis.Close()                              //nolint:errcheck
	defer os.Remove(controllerSocketPath(cityDir)) //nolint:errcheck

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	if cb.IsOpen(identity, now.Add(time.Minute)) {
		t.Fatalf("breaker still OPEN for %q after `gc session kill %s`", identity, identity)
	}
	updated, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(session bead): %v", err)
	}
	if got := updated.Metadata[sessionCircuitStateMetadata]; got != "" {
		t.Fatalf("persisted circuit state = %q, want cleared", got)
	}
	if got := updated.Metadata[sessionCircuitRestartsMetadata]; got != "" {
		t.Fatalf("persisted restart history = %q, want cleared", got)
	}
	if got := updated.Metadata[sessionCircuitResetGenerationMetadata]; got == "" {
		t.Fatal("persisted reset generation is empty, want explicit reset generation")
	}
}

func TestCmdSessionKill_RecordsStoppedWhenCircuitBreakerResetFails(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-kill-cb-reset-fails-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	fakeProvider := runtime.NewFake()
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fakeProvider, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	const identity = "session-a"
	const sessionName = "s-gc-kill-cb-reset-fails-test"
	bead, err := store.Create(beads.Bead{
		Title:  "named session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                        identity,
			"template":                     "worker",
			"session_name":                 sessionName,
			"state":                        "awake",
			namedSessionMetadataKey:        "true",
			namedSessionIdentityMetadata:   identity,
			sessionCircuitStateMetadata:    circuitOpen.String(),
			sessionCircuitRestartsMetadata: `["2026-04-10T12:00:00Z"]`,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}
	if err := fakeProvider.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("fakeProvider.Start: %v", err)
	}
	if err := fakeProvider.SetMeta(sessionName, "GC_SESSION_ID", bead.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	lis := startFailingCircuitResetController(t, cityDir)
	defer lis.Close()                              //nolint:errcheck
	defer os.Remove(controllerSocketPath(cityDir)) //nolint:errcheck

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
	}

	if fakeProvider.IsRunning(sessionName) {
		t.Fatalf("session %q still running after kill", sessionName)
	}
	if got := stderr.String(); !strings.Contains(got, "warning: clearing session circuit breaker") {
		t.Fatalf("stderr = %q, want circuit-breaker warning", got)
	}
	if got := stdout.String(); !strings.Contains(got, "Session "+bead.ID+" killed.") {
		t.Fatalf("stdout = %q, want killed message", got)
	}

	updated, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(session bead): %v", err)
	}
	if got := updated.Metadata[sessionCircuitStateMetadata]; got != circuitOpen.String() {
		t.Fatalf("persisted circuit state = %q, want unchanged after clear failure", got)
	}

	rec, err := events.NewFileRecorder(filepath.Join(cityDir, ".gc", "events.jsonl"), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewFileRecorder(events): %v", err)
	}
	defer rec.Close() //nolint:errcheck
	recorded, err := rec.List(events.Filter{Type: events.SessionStopped, Subject: bead.ID})
	if err != nil {
		t.Fatalf("List(SessionStopped): %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("SessionStopped events for %s = %d, want 1", bead.ID, len(recorded))
	}
}

func startFailingCircuitResetController(t *testing.T, cityDir string) net.Listener {
	t.Helper()
	sockPath := controllerSocketPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(controller socket dir): %v", err)
	}
	os.Remove(sockPath) //nolint:errcheck
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(%q): %v", sockPath, err)
	}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close() //nolint:errcheck
				scanner := bufio.NewScanner(conn)
				if !scanner.Scan() {
					return
				}
				line := scanner.Text()
				switch {
				case line == "ping":
					fmt.Fprintf(conn, "%d\n", os.Getpid()) //nolint:errcheck
				case strings.HasPrefix(line, sessionCircuitResetCommandPrefix):
					conn.Write([]byte(`{"outcome":"failed","error":"forced reset failure"}` + "\n")) //nolint:errcheck
				default:
					conn.Write([]byte("ok\n")) //nolint:errcheck
				}
			}(conn)
		}
	}()
	return lis
}

func TestCmdSessionReset_RequestsFreshRestartWithController(t *testing.T) {
	// Shares newSessionResetControllerEnv's single listener site rather than
	// opening another: test/test-resources.toml ratchets untagged net_listen
	// call/file totals and this file's quota is one, so new tests here reuse the
	// helper instead of adding a site.
	env := newSessionResetControllerEnv(t)
	cityDir, beadID := env.cityDir, env.beadID

	// This test predates the confirmation wait (gascity-ksa part 3) and its
	// subject is the request path: the ping/poke/poke sequence, and the
	// reconcile-owned fields left untouched until the controller commits.
	// wait=0 selects the fire-and-forget mode so that queued state is exactly
	// what the assertions below still observe.
	var stdout, stderr bytes.Buffer
	if code := cmdSessionResetWithOptions([]string{"sky"}, &stdout, &stderr, sessionResetOptions{}); code != 0 {
		t.Fatalf("cmdSessionReset(controller) = %d, want 0; stderr=%s", code, stderr.String())
	}

	gotCommands := make([]string, 0, 3)
	deadline := time.After(2 * time.Second)
	for len(gotCommands) < 3 {
		select {
		case cmd := <-env.commands:
			gotCommands = append(gotCommands, cmd)
		case <-deadline:
			t.Fatalf("timed out waiting for controller pokes, got %v", gotCommands)
		}
	}
	wantExact := []string{"ping\n", "poke\n", "poke\n"}
	for i, want := range wantExact {
		if gotCommands[i] != want {
			t.Fatalf("controller command %d = %q, want %q", i, gotCommands[i], want)
		}
	}

	reloaded, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(reload): %v", err)
	}
	got, err := reloaded.Get(beadID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", beadID, err)
	}
	if got.Metadata["restart_requested"] != "true" {
		t.Fatalf("restart_requested = %q, want true", got.Metadata["restart_requested"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["session_key"] != "original-key" {
		t.Fatalf("session_key = %q, want original key preserved until reconcile", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "hash-before-reset" {
		t.Fatalf("started_config_hash = %q, want original hash preserved until reconcile", got.Metadata["started_config_hash"])
	}
}

func TestCmdSessionReset_ControllerClearFailureDoesNotQueueRestart(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-reset-clear-fail-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "generic named session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                      "session-a",
			"template":                   "worker",
			"session_name":               "s-gc-reset-clear-fail",
			"state":                      "awake",
			"session_key":                "original-key",
			"started_config_hash":        "hash-before-reset",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "session-a",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	sockPath := controllerSocketPath(cityDir)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(%q): %v", sockPath, err)
	}
	defer lis.Close()         //nolint:errcheck
	defer os.Remove(sockPath) //nolint:errcheck

	commands := make(chan string, 3)
	errCh := make(chan error, 1)
	go func() {
		defer close(commands)
		for i := 0; i < 3; i++ {
			conn, err := lis.Accept()
			if err != nil {
				errCh <- err
				return
			}
			buf := make([]byte, 256)
			n, err := conn.Read(buf)
			if err != nil {
				conn.Close() //nolint:errcheck
				errCh <- err
				return
			}
			cmd := string(buf[:n])
			commands <- cmd
			reply := "ok\n"
			if cmd == "ping\n" {
				reply = "123\n"
			} else if strings.HasPrefix(cmd, "session-circuit-reset:") {
				reply = `{"outcome":"failed","error":"clear failed"}` + "\n"
			}
			if _, err := conn.Write([]byte(reply)); err != nil {
				conn.Close() //nolint:errcheck
				errCh <- err
				return
			}
			conn.Close() //nolint:errcheck
		}
	}()

	var stdout, stderr bytes.Buffer
	if code := cmdSessionReset([]string{"session-a"}, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdSessionReset = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `clearing session circuit breaker for "session-a": clear failed`) {
		t.Fatalf("stderr = %q, want controller clear failure", stderr.String())
	}

	gotCommands := make([]string, 0, 3)
	deadline := time.After(2 * time.Second)
	for len(gotCommands) < 3 {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("controller socket: %v", err)
			}
		case cmd, ok := <-commands:
			if !ok {
				t.Fatalf("controller commands = %v, want ping, poke, reset", gotCommands)
			}
			gotCommands = append(gotCommands, cmd)
		case <-deadline:
			t.Fatalf("timed out waiting for controller commands, got %v", gotCommands)
		}
	}
	if gotCommands[0] != "ping\n" || gotCommands[1] != "poke\n" || !strings.HasPrefix(gotCommands[2], "session-circuit-reset:") {
		t.Fatalf("controller commands = %v, want ping, poke, reset", gotCommands)
	}

	reloaded, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(reload): %v", err)
	}
	got, err := reloaded.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", bead.ID, err)
	}
	if got.Metadata["restart_requested"] == "true" {
		t.Fatalf("restart_requested = true, want no queued reset after controller clear failure")
	}
	if got.Metadata["continuation_reset_pending"] == "true" {
		t.Fatalf("continuation_reset_pending = true, want no queued reset after controller clear failure")
	}
}

func TestResetSessionCircuitBreakerOnControllerMalformedReply(t *testing.T) {
	cityDir := shortSocketTempDir(t, "gc-session-reset-malformed-")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	sockPath := controllerSocketPath(cityDir)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(%q): %v", sockPath, err)
	}
	defer lis.Close()         //nolint:errcheck
	defer os.Remove(sockPath) //nolint:errcheck

	errCh := make(chan error, 1)
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close() //nolint:errcheck
		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			errCh <- scanner.Err()
			return
		}
		if _, err := conn.Write([]byte("not-json\n")); err != nil {
			errCh <- err
		}
	}()

	err = resetSessionCircuitBreakerOnController(cityDir, "session-id", "rig-a/session-a")
	if err == nil {
		t.Fatal("resetSessionCircuitBreakerOnController = nil, want decode error")
	}
	if !strings.Contains(err.Error(), "decoding session circuit reset reply") {
		t.Fatalf("error = %v, want decode context", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("controller socket: %v", err)
		}
	default:
	}
}

func writeGenericNamedSessionCityTOML(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	data := []byte(`[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "session-a"
provider = "codex"
start_command = "echo"

[[named_session]]
template = "session-a"
`)
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}

// TestCmdSessionReset_UnconfirmedRestartDoesNotReportOK is the acceptance test
// for part 3 of gascity-ksa.
//
// The command used to be fire-and-forget by construction: a two-key metadata
// write, a poke, and exit 0. Nothing observed whether the restart happened, so
// the recorded incident — "returned ok (action=reset, session_id=gc-7rd0) but
// the session never restarted" — was not a malfunction, it was the contract.
// That contract is what this bead rejects: an operator or an agent resetting
// itself cannot tell a completed restart from a stranded seat.
func TestCmdSessionReset_UnconfirmedRestartDoesNotReportOK(t *testing.T) {
	env := newSessionResetControllerEnv(t)

	// No controller consumes the marker: this is the stall.
	var stdout, stderr bytes.Buffer
	code := cmdSessionResetWithOptions([]string{"sky"}, &stdout, &stderr, sessionResetOptions{
		json: true,
		wait: 150 * time.Millisecond,
	})
	if code == 0 {
		t.Fatalf("cmdSessionReset = 0 for an unconfirmed restart, want non-zero; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	body := strings.TrimSpace(stdout.String())
	if body == "" {
		t.Fatalf("no JSON body emitted for an unconfirmed reset; stderr=%q", stderr.String())
	}
	var got struct {
		OK        bool   `json:"ok"`
		Action    string `json:"action"`
		SessionID string `json:"session_id"`
		Confirmed *bool  `json:"confirmed"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	if got.OK {
		t.Fatalf("ok = true for an unconfirmed restart, want false; body=%s", body)
	}
	if got.Confirmed == nil || *got.Confirmed {
		t.Fatalf("confirmed = %v, want an explicit false; body=%s", got.Confirmed, body)
	}
	if got.SessionID != env.beadID {
		t.Fatalf("session_id = %q, want %q", got.SessionID, env.beadID)
	}
}

// TestCmdSessionReset_ConfirmedRestartReportsOK pins the other half of part 3:
// the ordinary success path is unchanged — exit 0, action=reset, session id.
//
// Driven with wait=0 so the command reports the request without a confirmation
// round trip. The confirmation predicate itself is covered directly by
// TestWaitForResetCommitted, which keeps this test free of a background
// controller simulator (and of the polling sleep one would need — test/
// test-resources.toml ratchets fixed_sleep call/file totals).
func TestCmdSessionReset_ConfirmedRestartReportsOK(t *testing.T) {
	env := newSessionResetControllerEnv(t)

	var stdout, stderr bytes.Buffer
	code := cmdSessionResetWithOptions([]string{"sky"}, &stdout, &stderr, sessionResetOptions{
		json: true,
	})
	if code != 0 {
		t.Fatalf("cmdSessionReset = %d for a confirmed restart, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	body := strings.TrimSpace(stdout.String())
	var got struct {
		OK        bool   `json:"ok"`
		Action    string `json:"action"`
		SessionID string `json:"session_id"`
		Confirmed *bool  `json:"confirmed"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	if !got.OK {
		t.Fatalf("ok = false for a confirmed restart; body=%s", body)
	}
	if got.Action != "reset" {
		t.Fatalf("action = %q, want %q; body=%s", got.Action, "reset", body)
	}
	if got.SessionID != env.beadID {
		t.Fatalf("session_id = %q, want %q; body=%s", got.SessionID, env.beadID, body)
	}
	if got.Confirmed == nil || !*got.Confirmed {
		t.Fatalf("confirmed = %v, want an explicit true; body=%s", got.Confirmed, body)
	}
}

type sessionResetControllerEnv struct {
	cityDir string
	store   beads.Store
	beadID  string
	// commands records each controller command the stub socket served, so a
	// test can assert the ping/poke sequence. Buffered and written
	// non-blockingly: tests that do not read it must not wedge the server.
	commands chan string
}

// newSessionResetControllerEnv builds a city with one manual session bead and a
// controller socket that answers ping/poke for the life of the test. It is the
// shared fixture for the gascity-ksa exit-contract tests: what varies between
// them is only whether anything ever consumes the reset marker.
func newSessionResetControllerEnv(t *testing.T) sessionResetControllerEnv {
	t.Helper()
	const alias = "sky"
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-reset-confirm-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "manual session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                      alias,
			"template":                   "worker",
			"session_name":               "s-gc-reset-confirm",
			"state":                      "awake",
			"session_key":                "original-key",
			"started_config_hash":        "hash-before-reset",
			"continuation_reset_pending": "",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	sockPath := controllerSocketPath(cityDir)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(%q): %v", sockPath, err)
	}
	t.Cleanup(func() {
		lis.Close()         //nolint:errcheck
		os.Remove(sockPath) //nolint:errcheck
	})
	commands := make(chan string, 16)
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 64)
			n, err := conn.Read(buf)
			if err != nil {
				conn.Close() //nolint:errcheck
				continue
			}
			cmd := string(buf[:n])
			select {
			case commands <- cmd:
			default: // nobody is reading; serving the command still matters
			}
			reply := "ok\n"
			if cmd == "ping\n" {
				reply = "123\n"
			}
			_, _ = conn.Write([]byte(reply)) //nolint:errcheck
			conn.Close()                     //nolint:errcheck
		}
	}()

	return sessionResetControllerEnv{cityDir: cityDir, store: store, beadID: bead.ID, commands: commands}
}

// TestWaitForResetCommitted covers the predicate that decides whether
// gc session reset may report ok (gascity-ksa part 3).
//
// "Committed" means the controller consumed the request marker AND stamped a
// reset_committed_at different from the one this reset started with. Each half
// matters: a still-pending restart_requested means the controller has not
// picked the request up yet, and an unchanged reset_committed_at is residue
// from some EARLIER reset, which is exactly the stale value that would let a
// stalled reset report success.
func TestWaitForResetCommitted(t *testing.T) {
	env := newSessionResetControllerEnv(t)

	setMeta := func(t *testing.T, kv map[string]string) {
		t.Helper()
		if err := env.store.Update(env.beadID, beads.UpdateOpts{Metadata: kv}); err != nil {
			t.Fatalf("store.Update: %v", err)
		}
	}

	t.Run("committed", func(t *testing.T) {
		setMeta(t, map[string]string{
			"restart_requested":         "",
			session.ResetCommittedAtKey: "2026-09-11T10:00:00Z",
		})
		if !waitForResetCommitted(env.store, env.beadID, "2026-09-11T09:00:00Z", time.Second) {
			t.Fatalf("waitForResetCommitted = false, want true for a newly stamped reset_committed_at")
		}
	})

	t.Run("unchanged reset_committed_at is not this reset", func(t *testing.T) {
		setMeta(t, map[string]string{
			"restart_requested":         "",
			session.ResetCommittedAtKey: "2026-09-11T10:00:00Z",
		})
		if waitForResetCommitted(env.store, env.beadID, "2026-09-11T10:00:00Z", 150*time.Millisecond) {
			t.Fatalf("waitForResetCommitted = true for a reset_committed_at left by an earlier reset")
		}
	})

	t.Run("restart still pending", func(t *testing.T) {
		setMeta(t, map[string]string{
			"restart_requested":         "true",
			session.ResetCommittedAtKey: "2026-09-11T11:00:00Z",
		})
		if waitForResetCommitted(env.store, env.beadID, "2026-09-11T09:00:00Z", 150*time.Millisecond) {
			t.Fatalf("waitForResetCommitted = true while restart_requested is still pending")
		}
	})

	t.Run("zero budget skips the check", func(t *testing.T) {
		setMeta(t, map[string]string{
			"restart_requested":         "true",
			session.ResetCommittedAtKey: "",
		})
		if !waitForResetCommitted(env.store, env.beadID, "", 0) {
			t.Fatalf("waitForResetCommitted = false for a zero budget, want the fire-and-forget opt-out")
		}
	})
}
