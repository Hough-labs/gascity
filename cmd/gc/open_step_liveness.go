package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// formulaStepBeadType is the type molecule instantiation stamps on a non-root
// formula step (internal/molecule/molecule.go nonRootStepBeadType). It is in
// beads.readyExcludeTypes, so Ready() — and therefore every liveness probe
// built on Ready() — never returns one.
const formulaStepBeadType = "step"

// An open formula step bead is the session's executable turn when it is a
// non-root step, not deferred, assigned, and every blocking dependency has
// closed. That is Ready()'s own gate with exactly one difference: it does not
// apply readyExcludeTypes.
//
// The exclusion exists so `bd ready` shows the parent molecule rather than its
// scaffolding (#1039) — right for DISCOVERY, wrong for LIVENESS. An open step
// assigned to a live session is in-flight work, and a session that owns one is
// not idle. Reading liveness off Ready() let idle_timeout stop a polecat
// holding an open submit step — the step that pushes the branch to origin and
// hands the bead to the refinery — leaving finished, reviewed, committed work
// on an unpublished local branch that no discovery probe in the city matches
// (gascity-t2c).
//
// The blocking-dependency half is not optional. Counting a step the session
// cannot yet run would hold it awake on work it cannot make progress on, which
// is the ga-3ox7rk kill/wake treadmill in a new costume.
//
// The precedent for admitting what Ready() hides is
// appendOpenAssignedMoleculeWorkUnique, which already admits assigned
// molecule/wisp roots on the same reasoning: an assigned root is the executable
// turn even though Ready() hides molecule roots from generic work queues.
//
// The predicate is split in two so callers can batch. openAssignedStepCandidate
// is the store-free half — the cheap field checks that decide whether a bead is
// worth a dependency round-trip at all. Every caller runs it first over its
// whole candidate set, then resolves dependencies for the survivors in one
// stepDependencies call, so a store holding no open assigned steps costs no
// extra queries.
func openAssignedStepCandidate(b beads.Bead, now time.Time) bool {
	return b.Status == "open" &&
		b.Type == formulaStepBeadType &&
		strings.TrimSpace(b.Assignee) != "" &&
		!beads.IsDeferred(b, now)
}

// stepDependencies resolves the "down" dependencies of every id, preferring the
// store's DepListBatch fast-path so a reconcile tick spends one round-trip on
// the whole candidate set rather than one per step. DepListBatch is an optional
// capability (it is not on beads.Store), so the per-id DepList fallback is the
// contract, not a degraded mode.
func stepDependencies(store beads.Store, ids []string) (map[string][]beads.Dep, error) {
	if store == nil || len(ids) == 0 {
		return nil, nil
	}
	if batcher, ok := store.(interface {
		DepListBatch([]string) (map[string][]beads.Dep, error)
	}); ok {
		return batcher.DepListBatch(ids)
	}
	out := make(map[string][]beads.Dep, len(ids))
	for _, id := range ids {
		deps, err := store.DepList(id, "down")
		if err != nil {
			return nil, err
		}
		out[id] = deps
	}
	return out, nil
}

// stepBlockingDepsClosed reports whether every blocking dependency in deps has
// closed, memoizing target lookups in closedByID across a candidate set that
// commonly shares predecessors.
//
// A dependency the store cannot resolve is treated as still blocking (fail
// closed): admitting a step on an unreadable dependency would hold a session
// awake on work whose readiness is unknown, while the cost of a false negative
// is only that the ordinary idle path applies.
func stepBlockingDepsClosed(store beads.Store, deps []beads.Dep, closedByID map[string]bool) bool {
	if store == nil {
		return false
	}
	for _, dep := range deps {
		if !beads.IsReadyBlockingDependencyType(dep.Type) {
			continue
		}
		closed, seen := closedByID[dep.DependsOnID]
		if !seen {
			target, err := store.Get(dep.DependsOnID)
			closed = err == nil && target.Status == "closed"
			closedByID[dep.DependsOnID] = closed
		}
		if !closed {
			return false
		}
	}
	return true
}

// sessionHasOpenAssignedStepWorkForTier reports whether the identity owns an
// open, executable formula step in this store's tier. It is the liveness probe
// sessionHasAwakeAssignedWorkInStoreByIdentifiers adds alongside the
// in-progress and Ready() probes, covering the one shape those two structurally
// cannot see.
func sessionHasOpenAssignedStepWorkForTier(store beads.Store, assignee string, tierMode beads.TierMode) (bool, error) {
	if store == nil || strings.TrimSpace(assignee) == "" {
		return false, nil
	}
	wa := workAssignmentForStore(beads.WorkStore{Store: store})
	items, err := wa.OpenAssignedTo(assignee, "open", tierMode, true)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	candidates := make([]string, 0, len(items))
	byID := make(map[string]beads.Bead, len(items))
	for _, item := range items {
		if !openAssignedStepCandidate(item, now) {
			continue
		}
		candidates = append(candidates, item.ID)
		byID[item.ID] = item
	}
	if len(candidates) == 0 {
		return false, nil
	}
	deps, err := stepDependencies(store, candidates)
	if err != nil {
		return false, err
	}
	closedByID := make(map[string]bool)
	for _, id := range candidates {
		if stepBlockingDepsClosed(store, deps[id], closedByID) {
			return true, nil
		}
	}
	return false, nil
}
