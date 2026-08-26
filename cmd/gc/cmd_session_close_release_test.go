package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCmdSessionCloseReleasesAssignedWorkBeads is a regression for
// gastownhall/gascity#2625. After `gc session close`, any work bead still
// assigned to the closed session must be released (Assignee cleared, Status
// reset to open) so the pool scale-check picks up the freed demand on the
// next reconcile tick. Without it, Source-1 CachedReady stays stale, the
// pool scale-check sees scaleCount=0, and no fresh worker spawns even
// though the demand is admittable.
func TestCmdSessionCloseReleasesAssignedWorkBeads(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	sessionBead, err := store.Create(beads.Bead{
		Title:  "stranded worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-stranded",
			"template":     "worker",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:    "admittable demand",
		Type:     "task",
		Assignee: sessionBead.ID,
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create(work bead): %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{sessionBead.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionClose = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}

	gotSession, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session bead): %v", err)
	}
	if gotSession.Status != "closed" {
		t.Errorf("session bead status = %q, want closed", gotSession.Status)
	}

	gotWork, err := reopened.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(work bead): %v", err)
	}
	if gotWork.Assignee != "" {
		t.Errorf("work bead Assignee = %q, want empty (released after session close)", gotWork.Assignee)
	}
	if gotWork.Status != "open" {
		t.Errorf("work bead Status = %q, want open (reset so the routed queue can re-pick it)", gotWork.Status)
	}
	// A bead that still carries its own gc.routed_to must keep it and must NOT
	// pick up a second, competing route from the retired session's template.
	// ReleaseWorkBead stamps the fallback only on an otherwise-unrouted bead;
	// this pins that half of the contract (gascity-t2c).
	if got := gotWork.Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
		t.Errorf("work bead gc.routed_to = %q, want it preserved as %q", got, "worker")
	}
	if got := gotWork.Metadata[beadmeta.RunTargetMetadataKey]; got != "" {
		t.Errorf("work bead gc.run_target = %q, want empty: an already-routed bead must not be re-stamped", got)
	}
}

// TestCmdSessionCloseRestoresPoolRouteOnClaimConsumedWork is a regression for
// gascity-t2c. Releasing a work bead off a retired session clears its assignee,
// but a bead whose gc.routed_to was consumed by the claim then carries no route
// at all — status=open, assignee="", gc.routed_to="" — which is invisible to
// every discovery probe in the city: the assigned-work lookup keys on session
// identity, the pool demand probe requires a route, and the refinery find-work
// query requires an assignee. The bead reads exactly like "never started" while
// holding finished, committed work on a branch nobody will ever look at again.
//
// unclaimWorkAssignedToRetiredSessionBead already takes a fallbackRoute for
// precisely this case, and every other release path (retry, reopen, orphan-pool,
// stranded-repair) supplies one. `gc session close` passed "" — so this one path
// released work into the void. Observed twice: gascity-3vr (2026-08-19) and
// gascity-cgh, which sat unreachable for six days holding the fix for the rig's
// top bottleneck.
func TestCmdSessionCloseRestoresPoolRouteOnClaimConsumedWork(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	sessionBead, err := store.Create(beads.Bead{
		Title:  "polecat that slept holding an open submit step",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-gc-7zy5",
			"template":     "worker",
			"state":        "asleep",
			"sleep_reason": "idle",
		},
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}

	// The claim consumed gc.routed_to: the bead is assigned to the session and
	// carries no route of its own. This is the shape of every claimed pool bead.
	work, err := store.Create(beads.Bead{
		Title:    "green work committed on a branch",
		Type:     "task",
		Assignee: "worker-gc-7zy5",
		Metadata: map[string]string{"gc.routed_to": ""},
	})
	if err != nil {
		t.Fatalf("Create(work bead): %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{sessionBead.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionClose = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	gotWork, err := reopened.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(work bead): %v", err)
	}
	if gotWork.Assignee != "" {
		t.Fatalf("work bead Assignee = %q, want empty (released after session close)", gotWork.Assignee)
	}
	if gotWork.Status != "open" {
		t.Fatalf("work bead Status = %q, want open", gotWork.Status)
	}
	route := gotWork.Metadata[beadmeta.RunTargetMetadataKey]
	if route == "" {
		t.Fatalf("work bead carries no route after release (gc.run_target=%q gc.routed_to=%q): "+
			"nothing in the city can discover it and the committed branch is stranded",
			gotWork.Metadata[beadmeta.RunTargetMetadataKey], gotWork.Metadata[beadmeta.RoutedToMetadataKey])
	}
	if route != "worker" {
		t.Fatalf("work bead gc.run_target = %q, want the retired session's template %q", route, "worker")
	}
}
