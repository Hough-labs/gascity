// Package main test: open-formula-step liveness (gascity-t2c).
//
// Ready() deliberately hides non-root formula step beads from generic work
// queues — readyExcludeTypes carries "step" because the parent molecule is the
// actionable unit for anyone reading `bd ready` (#1039). That exclusion is
// right for DISCOVERY and wrong for LIVENESS: an open step assigned to a live
// session is in-flight work.
//
// Both liveness engines read Ready(), so both inherit the exclusion:
//
//   - sessionHasAwakeAssignedWorkInStoreByIdentifiers feeds
//     DecideIdleTimeout's AssignedWork rung, so idle_timeout sees "no assigned
//     work" and stops the session.
//   - collectAssignedWorkBeadsWithStores' readyAssigned verdict feeds
//     AwakeWorkBead.Ready, so ComputeAwakeSet raises no "assigned-work" demand
//     and never re-wakes it.
//
// They must move together: ga-3ox7rk is the standing proof that fixing one
// engine and not the other produces a kill/wake treadmill instead of a fix.
//
// The bead this strands is the polecat's submit step — the step that pushes the
// branch to origin and reassigns the work to the refinery. Sleeping through it
// leaves finished, reviewed, committed work on an unpublished local branch that
// no discovery probe in the city matches.
package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const testStepSessionName = "worker-gc-7zy5"

// newSubmitStepStore builds the observed molecule shape: implement and
// self-review closed, submit still open and assigned to the session. Only the
// submit step carries the session's assignee, so the probes under test have
// exactly one candidate and a false verdict can only mean the submit step was
// missed. blocked leaves self-review open — and assigned to nobody — so submit
// is the session's only assigned bead but is not yet its executable turn.
func newSubmitStepStore(t *testing.T, stepType string, blocked bool) (beads.Store, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()

	root, err := store.Create(beads.Bead{
		Title:  "mol-polecat-work",
		Type:   "molecule",
		Status: "open",
	})
	if err != nil {
		t.Fatalf("create molecule root: %v", err)
	}

	mkStep := func(title, assignee string) beads.Bead {
		b, err := store.Create(beads.Bead{
			Title:    title,
			Type:     stepType,
			Status:   "open",
			Assignee: assignee,
			Metadata: map[string]string{"gc.routed_to": "worker"},
		})
		if err != nil {
			t.Fatalf("create step %q: %v", title, err)
		}
		if err := store.DepAdd(b.ID, root.ID, "tracks"); err != nil {
			t.Fatalf("dep tracks: %v", err)
		}
		return b
	}

	implement := mkStep("Implement the solution", "")
	review := mkStep("Self-review and run tests", "")
	submit := mkStep("Submit work to refinery and exit", testStepSessionName)
	if err := store.DepAdd(review.ID, implement.ID, "blocks"); err != nil {
		t.Fatalf("dep blocks: %v", err)
	}
	if err := store.DepAdd(submit.ID, review.ID, "blocks"); err != nil {
		t.Fatalf("dep blocks: %v", err)
	}

	if err := store.Close(implement.ID); err != nil {
		t.Fatalf("close implement: %v", err)
	}
	if !blocked {
		if err := store.Close(review.ID); err != nil {
			t.Fatalf("close self-review: %v", err)
		}
	}

	submit, err = store.Get(submit.ID)
	if err != nil {
		t.Fatalf("reload submit step: %v", err)
	}
	return store, submit
}

// TestSessionAwakeAssignedWork_CountsOpenAssignedFormulaStep is the RED proof
// for the idle-timeout half. DecideIdleTimeout defers its stop on
// AssignedWorkHas, but the fact is gathered through Ready(), which drops the
// "step" type — so a polecat holding nothing but an unblocked open submit step
// reads as AssignedWorkNone and is stopped.
func TestSessionAwakeAssignedWork_CountsOpenAssignedFormulaStep(t *testing.T) {
	store, submit := newSubmitStepStore(t, "step", false)

	has, err := sessionHasAwakeAssignedWorkInStoreByIdentifiers(store, []string{testStepSessionName})
	if err != nil {
		t.Fatalf("sessionHasAwakeAssignedWorkInStoreByIdentifiers: %v", err)
	}
	if !has {
		t.Fatalf("open assigned submit step %s does not count as awake assigned work: "+
			"idle_timeout will stop the session mid-molecule and strand the unpublished branch", submit.ID)
	}
}

// TestSessionAwakeAssignedWork_IgnoresBlockedOpenAssignedFormulaStep pins the
// other side of the contract. A step whose predecessor is still open is not the
// session's executable turn; counting it would hold a session awake on work it
// cannot run, which is the ga-3ox7rk treadmill in a new costume.
func TestSessionAwakeAssignedWork_IgnoresBlockedOpenAssignedFormulaStep(t *testing.T) {
	store, submit := newSubmitStepStore(t, "step", true)

	has, err := sessionHasAwakeAssignedWorkInStoreByIdentifiers(store, []string{testStepSessionName})
	if err != nil {
		t.Fatalf("sessionHasAwakeAssignedWorkInStoreByIdentifiers: %v", err)
	}
	if has {
		t.Fatalf("blocked submit step %s counted as awake assigned work: a session held awake on a "+
			"step it cannot run is the ga-3ox7rk kill/wake treadmill in a new costume", submit.ID)
	}
}

// TestCollectAssignedWorkBeads_MarksOpenAssignedFormulaStepReady is the RED
// proof for the awake-engine half. Without a readiness verdict the step lands
// in AwakeWorkBead with Ready=false, workBeadHasAwakeDemand returns false, and
// ComputeAwakeSet raises no "assigned-work" demand — so even a session that
// survives the idle ladder is never re-woken to finish the molecule.
func TestCollectAssignedWorkBeads_MarksOpenAssignedFormulaStepReady(t *testing.T) {
	store, submit := newSubmitStepStore(t, "step", false)

	sess, err := store.Create(beads.Bead{
		Title:  "polecat session",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name": testStepSessionName,
			"template":     "worker",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	snapshot := newSessionBeadSnapshot([]beads.Bead{sess})
	got, _, storeRefs, readyAssigned, partial := collectAssignedWorkBeadsWithStores(cfg, store, nil, nil, snapshot)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}

	found := false
	for i, b := range got {
		if b.ID != submit.ID {
			continue
		}
		ref := ""
		if i < len(storeRefs) {
			ref = storeRefs[i]
		}
		if readyAssigned[storeScopedBeadKey{StoreRef: ref, ID: b.ID}] {
			found = true
		}
	}
	if !found {
		t.Fatalf("submit step %s carries no wake-demand readiness verdict: "+
			"ComputeAwakeSet raises no assigned-work demand and the session is never re-woken", submit.ID)
	}
}
