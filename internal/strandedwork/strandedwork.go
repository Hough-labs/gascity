// Package strandedwork detects work beads that hold committed work no
// discovery probe in the city can reach.
//
// A work bead is stranded when all of the following hold at once:
//
//   - status is open;
//   - it has no assignee;
//   - it carries neither gc.routed_to nor gc.run_target (no route);
//   - it records a work branch (metadata "branch");
//   - that branch exists in the repository and holds commits its merge target
//     does not.
//
// That combination is matched by no discovery probe in the city: the
// assigned-work lookup keys on session identity, the pool demand probe requires
// a route, and the merge queue's find-work query requires an assignee. The bead
// reads exactly like "never started" while holding finished, committed work.
// Observed twice — gascity-3vr, and gascity-cgh, which sat unreachable for six
// days holding the fix for its rig's top bottleneck. Both were found only
// because a human went looking.
//
// This package detects and reports; it never repairs. A stranded bead may hold
// an unpublished branch, and whether to publish it, hand it to the merge queue,
// or discard it is a judgment call that belongs to an operator — not to Go.
// Scan is pure: the repository half of the signature arrives through an
// injected [BranchProber], so the decision logic is exercised without git.
package strandedwork

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Finding describes one stranded work bead. Every field is load-bearing for the
// operator deciding what to do with the branch: CommitsAhead says how much work
// is at stake, and OnOrigin separates a branch recoverable from the remote from
// one that is a single disk failure from total loss.
type Finding struct {
	// BeadID is the stranded work bead.
	BeadID string
	// StoreRef is the canonical reference of the store holding it
	// ("city:<name>" or "rig:<name>").
	StoreRef string
	// Branch is the work branch recorded on the bead.
	Branch string
	// Target is the branch CommitsAhead was measured against — the bead's own
	// merge target, or the repository default when the bead records none.
	Target string
	// CommitsAhead is the number of commits on Branch that Target does not have.
	// Always positive: a fully merged branch strands nothing and is not reported.
	CommitsAhead int
	// OnOrigin reports whether the branch has been published to the remote.
	OnOrigin bool
}

// BranchState is one repository's report about a work branch.
type BranchState struct {
	// LocalExists reports whether the repository has the branch at all.
	LocalExists bool
	// Target is the ref CommitsAhead was measured against, resolved by the
	// prober when the caller asked for the repository default.
	Target string
	// CommitsAhead is the number of commits on the branch the target lacks.
	CommitsAhead int
	// OnOrigin reports whether the branch has been published to the remote.
	OnOrigin bool
}

// BranchProber reports a repository's view of one work branch.
//
// An empty target means "measure against the repository's default branch"; the
// prober resolves it and returns the branch it used in [BranchState.Target].
// Resolving the default lazily keeps the cost off the healthy path, where a
// scan finds no candidate to probe at all.
type BranchProber interface {
	ProbeBranch(branch, target string) (BranchState, error)
}

// Candidate reports whether b matches the metadata half of the stranded
// signature — open, unassigned, unrouted, and recording a work branch —
// returning the branch and the merge target the bead records. An empty target
// means the bead records none, which is the normal shape for work stranded
// before its submit step (submit is what writes the target); the prober falls
// back to the repository default.
//
// It never reads a repository, so the cheap half of the signature rules out
// almost every bead before any subprocess runs.
func Candidate(b beads.Bead) (branch, target string, ok bool) {
	// Belt-and-braces with Scan's open-bead query: the guarantee belongs to the
	// signature, not to a particular store's filtering semantics.
	if b.Status != "open" || strings.TrimSpace(b.Assignee) != "" {
		return "", "", false
	}
	// Either route makes the bead discoverable: gc.routed_to is what the pool
	// demand probe keys on, and a carried gc.run_target is what route recovery
	// promotes back into gc.routed_to on the next tick.
	if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
		return "", "", false
	}
	if strings.TrimSpace(b.Metadata[beadmeta.RunTargetMetadataKey]) != "" {
		return "", "", false
	}
	branch = strings.TrimSpace(b.Metadata[beadmeta.BranchMetadataKey])
	if branch == "" {
		return "", "", false
	}
	return branch, strings.TrimSpace(b.Metadata[beadmeta.TargetMetadataKey]), true
}

// Scan returns every stranded work bead in store, ordered by bead ID so the
// findings (and the events emitted from them) are stable across passes.
// storeRef labels the store on each finding.
//
// Errors are per-bead and joined: a repository that cannot answer for one
// branch is reported rather than swallowed, and never yields a finding with
// zeroed CommitsAhead/OnOrigin fields — an operator reads those to decide how
// urgent the branch is, so a fabricated zero is worse than a surfaced error.
// The remaining beads are still scanned.
//
// A nil store or prober scans nothing, so a scope whose store is unavailable or
// whose repository cannot be resolved degrades to "no findings" instead of a
// failure.
func Scan(storeRef string, store beads.Store, prober BranchProber) ([]Finding, error) {
	if store == nil || prober == nil {
		return nil, nil
	}
	// Open work is the only place a strand can hide — a closed bead is done and
	// an in-progress one is held by a live claim — so the scan stays off the
	// whole store. AllowScan acknowledges the intentional population read, as in
	// the sibling open-bead sweeps.
	items, err := store.List(beads.ListQuery{Status: "open", AllowScan: true})
	if err != nil {
		return nil, fmt.Errorf("listing open work in %s: %w", storeRef, err)
	}
	var (
		findings []Finding
		errs     []error
	)
	for _, b := range items {
		branch, target, ok := Candidate(b)
		if !ok {
			continue
		}
		state, err := prober.ProbeBranch(branch, target)
		if err != nil {
			errs = append(errs, fmt.Errorf("bead %s: probing branch %q in %s: %w", b.ID, branch, storeRef, err))
			continue
		}
		// A branch the repository does not have, and a branch fully merged into
		// its target, both strand nothing.
		if !state.LocalExists || state.CommitsAhead <= 0 {
			continue
		}
		findings = append(findings, Finding{
			BeadID:       b.ID,
			StoreRef:     storeRef,
			Branch:       branch,
			Target:       state.Target,
			CommitsAhead: state.CommitsAhead,
			OnOrigin:     state.OnOrigin,
		})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].BeadID < findings[j].BeadID })
	return findings, errors.Join(errs...)
}
