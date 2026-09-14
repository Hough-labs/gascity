package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestRuntimeObservationHoldOpensEpisodeOnlyWhenSessionsAreBelievedRunning pins
// the first-boot exemption. A failed observation can only be hiding a fleet
// that the controller already believes exists; on a genuinely empty city (or a
// first boot, where tmux has no server yet and ErrNoServer is therefore the
// NORMAL reading) there is nothing to protect and the city must still start.
func TestRuntimeObservationHoldOpensEpisodeOnlyWhenSessionsAreBelievedRunning(t *testing.T) {
	h := &runtimeObservationHold{}
	now := time.Date(2026, 9, 14, 13, 57, 44, 0, time.UTC)

	got := h.observe(true, 0, now)
	if got.Hold {
		t.Fatalf("observe(partial, believedRunning=0).Hold = true, want false (a first boot must still start)")
	}
	if got.Escalated {
		t.Errorf("observe(partial, believedRunning=0).Escalated = true, want false")
	}

	// The same provider outage one tick later, once sessions ARE believed
	// running, must open the episode and hold.
	got = h.observe(true, 13, now.Add(30*time.Second))
	if !got.Hold {
		t.Fatalf("observe(partial, believedRunning=13).Hold = false, want true")
	}
	if got.BelievedRunning != 13 {
		t.Errorf("BelievedRunning = %d, want 13", got.BelievedRunning)
	}
}

// TestRuntimeObservationHoldHoldsThenEscalatesOnSustainedOutage pins the
// escalation ladder: hold while the outage is young, then escalate exactly once
// and RELEASE so a genuinely dead runtime cannot deadlock the city forever.
func TestRuntimeObservationHoldHoldsThenEscalatesOnSustainedOutage(t *testing.T) {
	h := &runtimeObservationHold{}
	start := time.Date(2026, 9, 14, 13, 57, 44, 0, time.UTC)
	window := partialRuntimeObservationHoldWindow

	first := h.observe(true, 13, start)
	if !first.Hold {
		t.Fatalf("first partial tick Hold = false, want true")
	}
	if first.HeldTicks != 1 {
		t.Errorf("first.HeldTicks = %d, want 1", first.HeldTicks)
	}
	if first.Escalated {
		t.Errorf("first.Escalated = true, want false")
	}

	mid := h.observe(true, 13, start.Add(window/2))
	if !mid.Hold {
		t.Fatalf("mid-window Hold = false, want true")
	}
	if mid.HeldTicks != 2 {
		t.Errorf("mid.HeldTicks = %d, want 2", mid.HeldTicks)
	}
	if mid.Elapsed != window/2 {
		t.Errorf("mid.Elapsed = %v, want %v", mid.Elapsed, window/2)
	}

	past := h.observe(true, 13, start.Add(window+time.Second))
	if past.Hold {
		t.Fatalf("past-window Hold = true, want false (a sustained outage must not deadlock the city)")
	}
	if !past.Escalated {
		t.Fatalf("past-window Escalated = false, want true")
	}

	// Escalation fires exactly once per episode; later ticks stay released and
	// silent so the log does not spam a permanently dead runtime.
	again := h.observe(true, 13, start.Add(2*window))
	if again.Hold {
		t.Errorf("post-escalation Hold = true, want false")
	}
	if again.Escalated {
		t.Errorf("post-escalation Escalated = true, want false (escalate once per episode)")
	}
}

// TestRuntimeObservationHoldResetsOnSuccessfulListing pins that a recovered
// runtime closes the episode, so the NEXT outage gets a fresh hold window
// rather than inheriting a burnt-through one.
func TestRuntimeObservationHoldResetsOnSuccessfulListing(t *testing.T) {
	h := &runtimeObservationHold{}
	start := time.Date(2026, 9, 14, 13, 57, 44, 0, time.UTC)
	window := partialRuntimeObservationHoldWindow

	if got := h.observe(true, 13, start); !got.Hold {
		t.Fatalf("first partial tick Hold = false, want true")
	}
	if got := h.observe(true, 13, start.Add(window+time.Second)); !got.Escalated {
		t.Fatalf("expected escalation after the window elapsed")
	}
	if got := h.observe(false, 13, start.Add(3*time.Minute)); got.Hold || got.HeldTicks != 0 {
		t.Fatalf("clean listing decision = %+v, want no hold and a closed episode", got)
	}
	// A fresh outage re-opens the episode and holds again.
	got := h.observe(true, 13, start.Add(4*time.Minute))
	if !got.Hold {
		t.Fatalf("Hold after a recovered runtime = false, want true (a new episode gets a fresh window)")
	}
	if got.HeldTicks != 1 {
		t.Errorf("HeldTicks = %d, want 1 (a new episode restarts the count)", got.HeldTicks)
	}
}

// TestRuntimeListingObservationFailed pins the distinction the bug turned on:
// an observation that FAILED (any listing error, partial or total) is not the
// same fact as an observed-empty fleet.
func TestRuntimeListingObservationFailed(t *testing.T) {
	tests := []struct {
		name      string
		listNames []string
		listErr   error
		want      bool
	}{
		{
			name:      "unreachable tmux server is a failed observation",
			listNames: nil,
			listErr:   &runtime.PartialListError{Err: errors.New("tmux server unreachable: no tmux server running")},
			want:      true,
		},
		{
			name:      "a non-partial listing error is also a failed observation",
			listNames: nil,
			listErr:   errors.New("provider exploded"),
			want:      true,
		},
		{
			name:      "an observed-empty fleet is a real observation",
			listNames: []string{},
			listErr:   nil,
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := &partialListPoolProvider{Fake: runtime.NewFake(), listNames: tt.listNames, listErr: tt.listErr}
			got, err := runtimeListingObservationFailed(sp)
			if got != tt.want {
				t.Fatalf("runtimeListingObservationFailed = %v (err %v), want %v", got, err, tt.want)
			}
			if tt.want && err == nil {
				t.Error("a failed observation must return the listing error for the operator log")
			}
		})
	}
}

// TestSessionsBelievedRunningCountsLiveAdvisoryStates pins the discriminator
// the episode opens on: only sessions whose last persisted advisory state
// claims a live runtime count, so a fleet that is merely DESIRED cannot keep
// the constructive path held.
func TestSessionsBelievedRunningCountsLiveAdvisoryStates(t *testing.T) {
	infos := []sessionpkg.Info{
		{ID: "a", MetadataState: string(sessionpkg.StateActive)},
		{ID: "b", MetadataState: string(sessionpkg.StateAwake)},
		{ID: "c", MetadataState: "  active  "},
		{ID: "d", MetadataState: string(sessionpkg.StateAsleep)},
		{ID: "e", MetadataState: string(sessionpkg.StateStartPending)},
		{ID: "f", MetadataState: string(sessionpkg.StateCreating)},
		{ID: "g", MetadataState: ""},
		{ID: "h", MetadataState: string(sessionpkg.StateActive), Closed: true},
	}
	if got, want := sessionsBelievedRunning(infos), 3; got != want {
		t.Fatalf("sessionsBelievedRunning = %d, want %d", got, want)
	}
	if got := sessionsBelievedRunning(nil); got != 0 {
		t.Errorf("sessionsBelievedRunning(nil) = %d, want 0", got)
	}
}

// TestReconcileSessionBeadsHoldsStartsOnPartialRuntimeObservation is the
// regression test for gascity-bjg2: a reconcile tick whose runtime listing was
// a failed observation must make ZERO start calls for a desired session the
// controller still believes is running, even though the per-session liveness
// probe reads it as dead. Without the hold, this is the 13-session fleet
// rebuild measured on 2026-09-14.
func TestReconcileSessionBeadsHoldsStartsOnPartialRuntimeObservation(t *testing.T) {
	// build produces the incident's shape: a session the controller believes is
	// active, carrying live assigned work, whose runtime the provider reports
	// as gone. That is what an unreachable tmux server looks like to the
	// per-session liveness probe — indistinguishable, without the listing
	// verdict, from a session that genuinely died.
	build := func() (*reconcilerTestEnv, beads.Bead, []beads.Bead) {
		env := newReconcilerTestEnv()
		env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
		env.addDesired("worker", "worker", false)
		session := env.createSessionBead("worker", "worker")
		env.markSessionActive(&session)
		env.setSessionMetadata(&session, map[string]string{
			// Woke well before this tick, so the session is past the
			// rapid-crash and churn-productivity windows and reaches the wake
			// loop as a plain dead-but-desired session.
			"last_woke_at": env.clk.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
		})

		task, err := env.store.Create(beads.Bead{Title: "assigned task", Type: "task"})
		if err != nil {
			t.Fatalf("Create(task): %v", err)
		}
		status := "in_progress"
		assignee := session.ID
		if err := env.store.Update(task.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
			t.Fatalf("Update(task): %v", err)
		}
		task, err = env.store.Get(task.ID)
		if err != nil {
			t.Fatalf("Get(task): %v", err)
		}
		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", session.ID, err)
		}
		return env, got, []beads.Bead{task}
	}
	runTick := func(env *reconcilerTestEnv, session beads.Bead, work []beads.Bead) int {
		return reconcileSessionBeads(
			context.Background(), []beads.Bead{session}, env.desiredState,
			configuredSessionNames(env.cfg, "", env.store), env.cfg, env.sp, env.store,
			nil, work, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
			nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
		)
	}

	// Control arm. Without it the hold arm below would pass for the wrong
	// reason — a setup that never respawns in the first place proves nothing.
	t.Run("observed-dead session is respawned", func(t *testing.T) {
		env, session, work := build()
		if woken := runTick(env, session, work); woken != 1 {
			t.Fatalf("woken = %d, want 1 (a genuinely observed-dead desired session must respawn); stderr=%s", woken, env.stderr.String())
		}
		if !env.sp.IsRunning("worker") {
			t.Fatal("observed-dead desired worker must be respawned")
		}
	})

	t.Run("failed observation holds the respawn", func(t *testing.T) {
		env, session, work := build()
		env.startOptions = append(env.startOptions, withPartialRuntimeObservationStartHold())
		if woken := runTick(env, session, work); woken != 0 {
			t.Fatalf("woken = %d, want 0 (an unobservable fleet is not an absent fleet); stderr=%s", woken, env.stderr.String())
		}
		if env.sp.IsRunning("worker") {
			t.Fatal("reconciler started a session from a runtime listing it could not make")
		}
		for _, call := range env.sp.SnapshotCalls() {
			if call.Method == "Start" {
				t.Fatalf("provider Start(%q) called under a failed runtime observation", call.Name)
			}
		}
		if !strings.Contains(env.stderr.String(), "partial runtime observation") {
			t.Errorf("stderr = %q, want the hold to name itself for the operator", env.stderr.String())
		}
	})
}

// TestCityRuntimeBeadReconcileTick_PartialRuntimeListingHoldsFleetRebuild is
// the end-to-end guard for gascity-bjg2, at the seam where the incident
// happened: one controller reconcile tick, a runtime whose whole-fleet listing
// fails, and a session the controller still believes is running. The tick must
// make ZERO start calls. The control arm — the identical tick whose listing
// SUCCEEDS and honestly reports an empty fleet — must still respawn, because
// the whole fix rests on telling "observation failed" apart from "observed
// zero".
func TestCityRuntimeBeadReconcileTick_PartialRuntimeListingHoldsFleetRebuild(t *testing.T) {
	const sessionName = "worker-bd-123"

	build := func(listErr error) (*CityRuntime, *runtime.Fake, *bytes.Buffer, DesiredStateResult, *sessionBeadSnapshot) {
		store := beads.NewMemStore()
		session, err := store.Create(beads.Bead{
			Title:  "worker",
			Type:   sessionBeadType,
			Status: "open",
			Labels: []string{sessionBeadLabel, "agent:worker"},
			Metadata: map[string]string{
				"session_name":         sessionName,
				"template":             "worker",
				"agent_name":           "worker",
				"pool_slot":            "1",
				poolManagedMetadataKey: boolMetadata(true),
				// The controller believed this session was up when the runtime
				// went dark. That belief is what the hold protects.
				"state":              "awake",
				"continuation_epoch": "1",
				"generation":         "1",
				"last_woke_at":       time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
			},
		})
		if err != nil {
			t.Fatalf("Create session bead: %v", err)
		}

		// The runtime reports nothing running — which is exactly what an
		// unreachable tmux server looks like through the per-session probe.
		fake := runtime.NewFake()
		var sp runtime.Provider = fake
		if listErr != nil {
			sp = &partialListPoolProvider{Fake: fake, listErr: listErr}
		}

		stderr := &bytes.Buffer{}
		cr := &CityRuntime{
			cityPath:            t.TempDir(),
			cityName:            "maintainer-city",
			logPrefix:           "gc supervisor",
			cfg:                 &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}}},
			sp:                  sp,
			standaloneCityStore: store,
			sessionDrains:       newDrainTracker(),
			rec:                 events.Discard,
			stdout:              io.Discard,
			stderr:              stderr,
		}
		result := DesiredStateResult{
			State: map[string]TemplateParams{
				sessionName: {Command: "test-cmd", SessionName: sessionName, TemplateName: "worker"},
			},
			ScaleCheckCounts: map[string]int{"worker": 1},
			AssignedWorkBeads: []beads.Bead{
				workBead("ga-live", "worker", sessionName, "in_progress", 5),
			},
		}
		return cr, fake, stderr, result, newSessionBeadSnapshot([]beads.Bead{session})
	}

	// beadReconcileTick executes starts asynchronously, so join the wave before
	// reading the provider — and read it through SnapshotCalls, which copies
	// under the Fake's own lock.
	startCalls := func(cr *CityRuntime, fake *runtime.Fake) []string {
		cr.waitForAsyncStarts()
		var names []string
		for _, call := range fake.SnapshotCalls() {
			if call.Method == "Start" {
				names = append(names, call.Name)
			}
		}
		return names
	}

	t.Run("successful listing rebuilds the dead session", func(t *testing.T) {
		cr, fake, stderr, result, snapshot := build(nil)
		cr.beadReconcileTick(context.Background(), result, snapshot, nil, false)
		if got := startCalls(cr, fake); len(got) == 0 {
			t.Fatalf("Start calls = %v, want the observed-dead session rebuilt; stderr=%s", got, stderr.String())
		}
	})

	t.Run("failed listing holds the rebuild", func(t *testing.T) {
		cr, fake, stderr, result, snapshot := build(&runtime.PartialListError{
			Err: errors.New("tmux server unreachable: no tmux server running"),
		})
		cr.beadReconcileTick(context.Background(), result, snapshot, nil, false)
		if got := startCalls(cr, fake); len(got) != 0 {
			t.Fatalf("Start calls = %v, want none: the reconciler rebuilt a fleet it could not observe; stderr=%s", got, stderr.String())
		}
		if !strings.Contains(stderr.String(), "holding start decisions this tick") {
			t.Errorf("stderr = %q, want the controller to log the hold", stderr.String())
		}
		if !strings.Contains(stderr.String(), "1 session(s) still believed running") {
			t.Errorf("stderr = %q, want the hold to report what it is protecting", stderr.String())
		}
	})
}

// TestCityRuntimeBeadReconcileTick_PartialRuntimeListingHoldsAcrossTicks is the
// test that makes the hold more than one tick of theater.
//
// The reconciler's own state heal treats every tick's liveness reading as
// authoritative, so the FIRST held tick walks the session bead the hold is
// protecting from "awake" to "asleep". A hold that re-derived its justification
// from bead state each tick would therefore evaporate on tick 2 and rebuild the
// fleet one tick later than before the fix — which is why the episode captures
// what the controller believed when the outage began and holds on THAT for the
// whole window.
func TestCityRuntimeBeadReconcileTick_PartialRuntimeListingHoldsAcrossTicks(t *testing.T) {
	const sessionName = "worker-bd-123"

	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         sessionName,
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "awake",
			"continuation_epoch":   "1",
			"generation":           "1",
			"last_woke_at":         time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}

	fake := runtime.NewFake()
	sp := &partialListPoolProvider{
		Fake:    fake,
		listErr: &runtime.PartialListError{Err: errors.New("tmux server unreachable: no tmux server running")},
	}
	stderr := &bytes.Buffer{}
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "maintainer-city",
		logPrefix:           "gc supervisor",
		cfg:                 &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}}},
		sp:                  sp,
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              stderr,
	}
	result := func() DesiredStateResult {
		return DesiredStateResult{
			State: map[string]TemplateParams{
				sessionName: {Command: "test-cmd", SessionName: sessionName, TemplateName: "worker"},
			},
			ScaleCheckCounts: map[string]int{"worker": 1},
			AssignedWorkBeads: []beads.Bead{
				workBead("ga-live", "worker", sessionName, "in_progress", 5),
			},
		}
	}
	assertNoStarts := func(tick string) {
		t.Helper()
		cr.waitForAsyncStarts()
		for _, call := range fake.SnapshotCalls() {
			if call.Method == "Start" {
				t.Fatalf("%s: provider Start(%q) called while the runtime was unobservable; stderr=%s", tick, call.Name, stderr.String())
			}
		}
	}

	cr.beadReconcileTick(context.Background(), result(), newSessionBeadSnapshot([]beads.Bead{session}), nil, false)
	assertNoStarts("tick 1")

	// The heal has now walked the protected session out of its "awake" state on
	// the strength of the same unreadable runtime. Pin that, so this test fails
	// loudly if the heal ever stops doing it and the second tick below starts
	// passing for a reason it was not written to prove.
	healed, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after tick 1: %v", err)
	}
	if state := healed.Metadata["state"]; state == "awake" || state == "active" {
		t.Skipf("heal no longer demotes an unobserved session (state=%q); the cross-tick trap this test guards is gone", state)
	}

	cr.beadReconcileTick(context.Background(), result(), cr.loadSessionBeadSnapshot(), nil, false)
	assertNoStarts("tick 2")
	if strings.Count(stderr.String(), "holding start decisions this tick") != 2 {
		t.Errorf("stderr = %q, want the hold logged on both ticks", stderr.String())
	}
}

// TestInstallRuntimeObservationHoldEscalatesAndReleases pins the operator-facing
// half of the escalation ladder: the controller must say, once and loudly, that
// it is giving up and letting the fleet be rebuilt — never release in silence.
func TestInstallRuntimeObservationHoldEscalatesAndReleases(t *testing.T) {
	stderr := &bytes.Buffer{}
	cr := &CityRuntime{
		logPrefix: "gc supervisor",
		sp: &partialListPoolProvider{
			Fake:    runtime.NewFake(),
			listErr: &runtime.PartialListError{Err: errors.New("tmux server unreachable: no tmux server running")},
		},
		stderr: stderr,
	}
	openInfos := []sessionpkg.Info{{ID: "s1", MetadataState: string(sessionpkg.StateAwake)}}
	start := time.Date(2026, 9, 14, 13, 57, 44, 0, time.UTC)

	opts, fields := cr.installRuntimeObservationHold(nil, openInfos, start)
	if len(opts) != 1 {
		t.Fatalf("held tick installed %d start option(s), want 1", len(opts))
	}
	if fields["hold"] != true {
		t.Errorf("fields[hold] = %v, want true", fields["hold"])
	}

	// Past the window the hold must come off, loudly and exactly once.
	opts, fields = cr.installRuntimeObservationHold(nil, openInfos, start.Add(partialRuntimeObservationHoldWindow+time.Second))
	if len(opts) != 0 {
		t.Fatalf("escalated tick installed %d start option(s), want 0 (the hold must release)", len(opts))
	}
	if fields["escalated"] != true {
		t.Errorf("fields[escalated] = %v, want true", fields["escalated"])
	}
	if !strings.Contains(stderr.String(), "ESCALATION") {
		t.Errorf("stderr = %q, want a loud escalation rather than a silent release", stderr.String())
	}

	before := stderr.String()
	if opts, _ = cr.installRuntimeObservationHold(nil, openInfos, start.Add(2*partialRuntimeObservationHoldWindow)); len(opts) != 0 {
		t.Errorf("post-escalation tick installed %d start option(s), want 0", len(opts))
	}
	if stderr.String() != before {
		t.Errorf("escalation logged twice; second line = %q", strings.TrimPrefix(stderr.String(), before))
	}
}
