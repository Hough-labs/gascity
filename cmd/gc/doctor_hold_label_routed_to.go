package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// holdLabelExternalValue is the one hold:<value> value that never implies a
// routing gap: it names a human/out-of-system dependency, not an agent.
const holdLabelExternalValue = "external"

// holdLabelRoutedToCheck detects beads carrying a hold:<value> label whose
// gc.routed_to metadata is missing or names neither <value> nor a route that
// <value> canonically resolves to. gc.routed_to is the sole persisted routing
// key (ga-eld2x); a hold:<value> label with no matching gc.routed_to has
// silently drifted from its intended route. --fix backfills gc.routed_to from
// the label value, binding-qualified where that value names a bound agent.
//
// This check writes the same field as v2-routed-to-namespace
// (doctor_routed_to_checks.go), which owns one question: the binding-qualified
// canonical form of a bound agent's route. Deferring to it on that question is
// what keeps the two satisfiable together — see holdRouteExpectation.
type holdLabelRoutedToCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newHoldLabelRoutedToCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *holdLabelRoutedToCheck {
	return &holdLabelRoutedToCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *holdLabelRoutedToCheck) Name() string { return "hold-label-routed-to" }

func (c *holdLabelRoutedToCheck) CanFix() bool { return true }

func (c *holdLabelRoutedToCheck) WarmupEligible() bool { return false }

// holdLabelValue returns the hold value carried by labels, if any
// hold:<value> label is present and <value> is not "external".
func holdLabelValue(labels []string) (string, bool) {
	for _, l := range labels {
		val, ok := strings.CutPrefix(l, "hold:")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		if val == "" || val == holdLabelExternalValue {
			continue
		}
		return val, true
	}
	return "", false
}

// holdRouteExpectation resolves what gc.routed_to may hold for a bead labeled
// hold:<value>, given the short-form route aliases v2-routed-to-namespace
// derives from the city's bindings.
//
// The hold label value alone cannot be the expectation. A hold label is
// canonically bare ("hold:mayor" — engdocs/contributors/hold-label-conventions.md)
// while a route must be binding-qualified to resolve to anything, so demanding
// the raw suffix here while v2-routed-to-namespace demands the qualified form
// made the two checks mutually unsatisfiable: each check's fix re-broke the
// other, and the agents acting on those fix hints repaired the same field
// against each other indefinitely while every patrol reported the finding
// unresolved (gascity-enqx).
//
// accepted lists every value that satisfies the hold label: the label's own
// value, plus every qualified route that value canonically means. want is the
// single value Fix may write — the canonical form when the value names exactly
// one bound agent, so this check never writes a route v2-routed-to-namespace
// would immediately rewrite. want is empty when the value is ambiguous across
// bindings and there is no single rewrite target; those are left for manual
// resolution, exactly as v2-routed-to-namespace leaves them.
func holdRouteExpectation(value string, aliases map[string][]string) (want string, accepted []string) {
	canonicals := aliases[value]
	accepted = make([]string, 0, len(canonicals)+1)
	accepted = append(accepted, value)
	accepted = append(accepted, canonicals...)
	switch len(canonicals) {
	case 0:
		return value, accepted
	case 1:
		return canonicals[0], accepted
	default:
		return "", accepted
	}
}

// holdRouteTarget is a single bead whose hold:<value> label and gc.routed_to
// metadata have drifted apart. canonicals holds every qualified route the hold
// value can mean; when want is empty there is more than one and no unambiguous
// backfill target exists.
type holdRouteTarget struct {
	label      string
	store      beads.Store
	beadID     string
	hold       string
	want       string
	canonicals []string
	got        string
}

func (t holdRouteTarget) describe() string {
	if t.want != "" {
		return fmt.Sprintf("%s bead %s has hold:%s but gc.routed_to=%q; use %q", t.label, t.beadID, t.hold, t.got, t.want)
	}
	return fmt.Sprintf("%s bead %s has hold:%s but gc.routed_to=%q; use one of %s", t.label, t.beadID, t.hold, t.got, strings.Join(t.canonicals, ", "))
}

func (c *holdLabelRoutedToCheck) collect() (targets []holdRouteTarget, skipped []string) {
	aliases := boundRoutedToAliases(c.cfg)
	scopes := []struct{ label, path string }{{"city", c.cityPath}}
	if c.cfg != nil {
		for _, rig := range c.cfg.Rigs {
			if rig.Suspended || strings.TrimSpace(rig.Path) == "" {
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
		// hold:<value> carries a dynamic value suffix, so no targeted
		// label/metadata query is possible; AllowScan is required for a
		// broad filter (internal/beads/query.go). Status is left unset so the
		// scan matches every non-closed bead (open, in_progress, blocked,
		// deferred, ...), not just "open" — an exact Status match would
		// silently hide hold:<value> drift on any other status (ga-fm2vgd.2).
		items, err := store.List(beads.ListQuery{AllowScan: true})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing beads: %v", sc.label, err))
			continue
		}
		for _, b := range items {
			hold, ok := holdLabelValue(b.Labels)
			if !ok {
				continue
			}
			want, accepted := holdRouteExpectation(hold, aliases)
			got := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey])
			if slices.Contains(accepted, got) {
				continue
			}
			targets = append(targets, holdRouteTarget{
				label:      sc.label,
				store:      store,
				beadID:     b.ID,
				hold:       hold,
				want:       want,
				canonicals: accepted[1:],
				got:        got,
			})
		}
	}
	return targets, skipped
}

func (c *holdLabelRoutedToCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	targets, skipped := c.collect()
	if len(targets) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no hold:<value> labels are missing a matching gc.routed_to")
	}
	details := make([]string, 0, len(targets)+len(skipped))
	for _, tgt := range targets {
		details = append(details, tgt.describe())
	}
	details = append(details, skipped...)
	sort.Strings(details)
	if len(targets) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("hold-label-routed-to check skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d bead(s) carry a hold:<value> label without matching gc.routed_to", len(targets)),
		"run gc doctor --fix to backfill gc.routed_to from the hold:<value> label, binding-qualified where that value names a bound agent",
		details)
}

func (c *holdLabelRoutedToCheck) Fix(_ *doctor.CheckContext) error {
	targets, skipped := c.collect()
	for _, tgt := range targets {
		if tgt.want == "" {
			// The hold value names more than one bound agent, so there is no
			// single route to backfill. Run keeps reporting it with the
			// candidates; picking one here would be a guess.
			continue
		}
		if err := tgt.store.SetMetadata(tgt.beadID, beadmeta.RoutedToMetadataKey, tgt.want); err != nil {
			return fmt.Errorf("%s bead %s: backfill gc.routed_to: %w", tgt.label, tgt.beadID, err)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("hold-label-routed-to skipped %d scope(s): %s", len(skipped), strings.Join(skipped, "; "))
	}
	return nil
}
