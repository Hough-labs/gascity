package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// humanRouteValue is the gc.routed_to value a gate writes when it halts a bead
// to a human instead of to an agent. It is a bare fallback identity, not a
// configured agent: no [[agent]] or [[named_session]] declares it, so nothing
// spawns for it and nothing polls it.
const humanRouteValue = "human"

// routedToHumanHaltHint tells the reader what to do with a halted bead, since
// this check deliberately cannot do it for them.
const routedToHumanHaltHint = "resolve each bead and close it, or give it an owner with " +
	"gc bd update <id> --assignee=<agent> --set-metadata gc.routed_to=<agent>"

// routedToHumanCheck reports unresolved beads whose gc.routed_to names the
// human fallback identity, so a bead halted to a human re-announces itself on
// every gc doctor run instead of going quiet.
//
// It exists because that value had no consumer at all. Nothing reads
// gc.routed_to=human: no agent polls for it, the witness's orphan scan keys on
// assignee — which a halt clears to null — and the reaper keys on a worktree.
// A halted bead therefore stopped emitting any signal, so it did not read as
// halted, it read as nothing, and the loss was unbounded in time. gascity-hpqe
// was recovered only because a witness noticed it by hand (gascity-r7s3).
//
// Surfacing is the whole remedy. This check never re-routes or reassigns a
// halted bead: who owns halted work is a human decision, so CanFix is false and
// being seen IS the fix. The write side is untouched — gates keep writing the
// value exactly as before (examples/gastown's tests pin that write).
type routedToHumanCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

// newRoutedToHumanCheck builds the halted-bead surfacing check for the city at
// cityPath and every active rig in cfg, reading each scope's bead store through
// newStore.
func newRoutedToHumanCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *routedToHumanCheck {
	return &routedToHumanCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

// Name returns the check's stable identifier.
func (c *routedToHumanCheck) Name() string { return "routed-to-human-halts" }

// CanFix returns false: a bead halted to a human has no safe automatic remedy.
// Re-routing it to an agent would require deciding who owns halted work.
func (c *routedToHumanCheck) CanFix() bool { return false }

// Fix is a no-op. This check surfaces halted beads and never remediates them;
// see CanFix.
func (c *routedToHumanCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *routedToHumanCheck) WarmupEligible() bool { return false }

// routedToHumanFinding is one unresolved bead halted to a human. status is
// carried because a halt lands on whatever lifecycle state the gate left behind
// — gascity-hpqe was "blocked", not "open" — and naming it tells the reader
// what they are resolving.
type routedToHumanFinding struct {
	label  string
	beadID string
	status string
}

func (f routedToHumanFinding) describe() string {
	return fmt.Sprintf("%s bead %s is halted to a human (%s=%q, status=%s)",
		f.label, f.beadID, beadmeta.RoutedToMetadataKey, humanRouteValue, f.status)
}

// Run reports every unresolved bead halted to a human. It warns rather than
// fails: a halted bead is a real state a gate chose, not a broken city, and
// reddening gc doctor over one would train readers to ignore the result.
func (c *routedToHumanCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	findings, skipped := c.collect()
	if len(findings) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), fmt.Sprintf("no unresolved beads are halted to a human (%s=%q)", beadmeta.RoutedToMetadataKey, humanRouteValue))
	}
	details := make([]string, 0, len(findings)+len(skipped))
	ids := make([]string, 0, len(findings))
	for _, f := range findings {
		details = append(details, f.describe())
		ids = append(ids, f.beadID)
	}
	details = append(details, skipped...)
	sort.Strings(details)
	sort.Strings(ids)
	if len(findings) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("%s check skipped %d scope(s)", c.Name(), len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	// The bead IDs belong in the summary, not only in Details: Details is shown
	// only in verbose mode, and a finding that names the halted bead only there
	// would leave it as silent as it was before this check existed.
	summary := fmt.Sprintf("%d unresolved bead(s) halted to a human: %s", len(findings), strings.Join(ids, ", "))
	if len(skipped) > 0 {
		summary = fmt.Sprintf("%s; %d scope(s) skipped", summary, len(skipped))
	}
	return warnCheck(c.Name(), summary, routedToHumanHaltHint, details)
}

// collect scans the city and every active rig for unresolved beads routed to
// the human fallback identity. A scope whose store cannot be opened or listed
// is reported as skipped rather than dropped, so an unreadable store never
// reads as "nothing is halted".
func (c *routedToHumanCheck) collect() (findings []routedToHumanFinding, skipped []string) {
	scopes := []struct{ label, path string }{{"city", c.cityPath}}
	if c.cfg != nil {
		suspState, _ := loadSuspensionState(fsys.OSFS{}, c.cityPath)
		for _, rig := range c.cfg.Rigs {
			if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			scopes = append(scopes, struct{ label, path string }{"rig " + rig.Name, rig.Path})
		}
	}
	for _, sc := range scopes {
		if c.newStore == nil || strings.TrimSpace(sc.path) == "" {
			continue
		}
		store, err := c.newStore(sc.path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", sc.label, err))
			continue
		}
		// A targeted metadata lookup, not a full-store scan: the human route is
		// a fixed value, so it needs no AllowScan. Status is left unset on
		// purpose — that matches every non-closed bead (open, in_progress,
		// blocked, deferred, ...), and a halt lands on blocked, so an exact
		// Status:"open" filter would hide the very shape this check exists to
		// find. Closed beads stay excluded because a closed halt is resolved.
		items, err := store.List(beads.ListQuery{
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: humanRouteValue},
		})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing beads: %v", sc.label, err))
			continue
		}
		for _, bead := range items {
			// Re-check both conditions the query asked for. A backend is
			// allowed to answer a filtered query with a superset
			// (internal/beads/query.go), and a closed or differently-routed
			// bead slipping through would turn this check into the permanent
			// noise its closed-halt exclusion exists to prevent.
			if strings.TrimSpace(bead.Metadata[beadmeta.RoutedToMetadataKey]) != humanRouteValue {
				continue
			}
			if bead.Status == "closed" {
				continue
			}
			findings = append(findings, routedToHumanFinding{
				label:  sc.label,
				beadID: bead.ID,
				status: bead.Status,
			})
		}
	}
	return findings, skipped
}
