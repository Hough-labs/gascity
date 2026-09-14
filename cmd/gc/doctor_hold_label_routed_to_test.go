package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// TestHoldLabelRoutedToCheck covers ga-fm2vgd.1: a bead carrying a
// hold:<value> label but whose gc.routed_to metadata is missing or does not
// match <value> has silently drifted from its intended route. The check must
// flag such beads across both city and rig stores, --fix must backfill
// gc.routed_to from the label value, hold:external must never be flagged,
// and beads with no hold:* label must be left untouched.
func TestHoldLabelRoutedToCheck(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}

	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		// Mismatch: hold:mayor label, no gc.routed_to at all.
		{ID: "H-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"}},
		// Mismatch: hold:mayor label, gc.routed_to set to something else.
		{
			ID: "H-2", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"},
			Metadata: map[string]string{"gc.routed_to": "reviewer"},
		},
		// Healthy: hold:mayor label, gc.routed_to already matches — must be left alone.
		{
			ID: "H-3", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"},
			Metadata: map[string]string{"gc.routed_to": "mayor"},
		},
		// Excluded by definition: hold:external names a human/out-of-system
		// dependency, never a routing gap, regardless of gc.routed_to state.
		{ID: "H-4", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:external"}},
		// No hold:* label at all — must be ignored even with empty gc.routed_to.
		{ID: "T-1", Title: "work", Type: "task", Status: "open"},
		// Generic over label value — must not require any special-casing of
		// "mayor" specifically; an arbitrary hold value must be caught too.
		{ID: "H-5", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:qa-lead"}},
		// Mismatch on a non-open bead: the scan must not filter to
		// Status=="open" only, or in_progress/blocked/deferred hold-labeled
		// beads silently escape it (ga-fm2vgd.2).
		{ID: "H-6", Title: "held", Type: "task", Status: "in_progress", Labels: []string{"hold:mayor"}},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RH-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"}},
	}, nil)
	stores := map[string]beads.Store{cityDir: cityStore, rigDir: rigStore}
	factory := func(path string) (beads.Store, error) {
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}

	check := newHoldLabelRoutedToCheck(cfg, cityDir, factory)

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
	details := strings.Join(res.Details, "\n")
	for _, want := range []string{"H-1", "H-2", "H-5", "H-6", "RH-1"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	for _, notWant := range []string{"H-3", "H-4", "T-1"} {
		if strings.Contains(details, notWant) {
			t.Fatalf("details should not mention %q:\n%s", notWant, details)
		}
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	// Idempotency: a second Run after Fix must report clean.
	if res2 := check.Run(&doctor.CheckContext{}); res2.Status != doctor.StatusOK {
		t.Fatalf("post-fix Run status = %v, want OK: %#v", res2.Status, res2)
	}

	h1, err := cityStore.Get("H-1")
	if err != nil {
		t.Fatalf("get H-1: %v", err)
	}
	if got := h1.Metadata["gc.routed_to"]; got != "mayor" {
		t.Errorf("H-1 gc.routed_to = %q, want mayor (backfilled from hold:mayor label)", got)
	}
	h2, err := cityStore.Get("H-2")
	if err != nil {
		t.Fatalf("get H-2: %v", err)
	}
	if got := h2.Metadata["gc.routed_to"]; got != "mayor" {
		t.Errorf("H-2 gc.routed_to = %q, want mayor (corrected from stale reviewer)", got)
	}
	h5, err := cityStore.Get("H-5")
	if err != nil {
		t.Fatalf("get H-5: %v", err)
	}
	if got := h5.Metadata["gc.routed_to"]; got != "qa-lead" {
		t.Errorf("H-5 gc.routed_to = %q, want qa-lead (arbitrary hold value, not special-cased)", got)
	}
	h6, err := cityStore.Get("H-6")
	if err != nil {
		t.Fatalf("get H-6: %v", err)
	}
	if got := h6.Metadata["gc.routed_to"]; got != "mayor" {
		t.Errorf("H-6 gc.routed_to = %q, want mayor (backfilled despite in_progress status)", got)
	}
	h4, err := cityStore.Get("H-4")
	if err != nil {
		t.Fatalf("get H-4: %v", err)
	}
	if got := h4.Metadata["gc.routed_to"]; got != "" {
		t.Errorf("H-4 (hold:external) gc.routed_to = %q, want untouched empty", got)
	}
	t1, err := cityStore.Get("T-1")
	if err != nil {
		t.Fatalf("get T-1: %v", err)
	}
	if got := t1.Metadata["gc.routed_to"]; got != "" {
		t.Errorf("T-1 (no hold label) gc.routed_to = %q, want untouched empty", got)
	}
	rh1, err := rigStore.Get("RH-1")
	if err != nil {
		t.Fatalf("get RH-1: %v", err)
	}
	if got := rh1.Metadata["gc.routed_to"]; got != "mayor" {
		t.Errorf("RH-1 gc.routed_to = %q, want mayor", got)
	}
}

// TestHoldLabelRoutedToCheckCleanStore confirms a store with no mismatches
// reports OK and CanFix advertises remediation.
func TestHoldLabelRoutedToCheckCleanStore(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{
			ID: "H-9", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"},
			Metadata: map[string]string{"gc.routed_to": "mayor"},
		},
		{ID: "H-10", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:external"}},
	}, nil)
	check := newHoldLabelRoutedToCheck(nil, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})
	if !check.CanFix() {
		t.Fatal("CanFix() = false, want true")
	}
	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Fatalf("Run status = %v, want OK: %#v", res.Status, res)
	}
}

func TestHoldLabelRoutedToFixReportsOpenFailures(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "H-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"}},
	}, nil)
	check := newHoldLabelRoutedToCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path == rigDir {
			return nil, errors.New("permission denied")
		}
		return cityStore, nil
	})

	err := check.Fix(&doctor.CheckContext{})
	if err == nil {
		t.Fatal("Fix error = nil, want skipped scope error")
	}
	if got := err.Error(); !strings.Contains(got, "rig repo skipped") || !strings.Contains(got, "permission denied") {
		t.Fatalf("Fix error = %q, want rig open failure detail", got)
	}
	h1, getErr := cityStore.Get("H-1")
	if getErr != nil {
		t.Fatalf("get H-1: %v", getErr)
	}
	if got := h1.Metadata["gc.routed_to"]; got != "mayor" {
		t.Fatalf("H-1 gc.routed_to = %q, want available repair applied despite skipped rig", got)
	}
}

func TestHoldLabelRoutedToFixReportsListFailures(t *testing.T) {
	cityDir := t.TempDir()
	check := newHoldLabelRoutedToCheck(nil, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return holdLabelListErrorStore{Store: beads.NewMemStore()}, nil
	})

	err := check.Fix(&doctor.CheckContext{})
	if err == nil {
		t.Fatal("Fix error = nil, want skipped scope error")
	}
	if got := err.Error(); !strings.Contains(got, "city skipped") || !strings.Contains(got, "listing failed") {
		t.Fatalf("Fix error = %q, want list failure detail", got)
	}
}

type holdLabelListErrorStore struct {
	beads.Store
}

func (s holdLabelListErrorStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("listing failed")
}

// TestHoldLabelRoutedToCheckDefersToBoundQualifiedRoute covers gascity-enqx:
// when a hold:<value> label's value is the binding-unqualified short form of a
// bound agent, v2-routed-to-namespace owns the canonical qualified form and
// this check defers to it — accepting either form on an already-routed bead,
// and backfilling the QUALIFIED form so it never writes a route that check
// would immediately rewrite.
func TestHoldLabelRoutedToCheckDefersToBoundQualifiedRoute(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Agents: []config.Agent{{Name: "mayor", BindingName: "gastown"}}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		// Short form: the value v2-routed-to-namespace owns rewriting. This
		// check must accept it rather than compete for the field.
		{
			ID: "H-SHORT", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"},
			Metadata: map[string]string{"gc.routed_to": "mayor"},
		},
		// Qualified form: the route that actually resolves. Also accepted.
		{
			ID: "H-QUALIFIED", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"},
			Metadata: map[string]string{"gc.routed_to": "gastown.mayor"},
		},
		// Genuinely unrouted — the drift this check exists to catch.
		{ID: "H-MISSING", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"}},
		// Routed somewhere unrelated to the hold label.
		{
			ID: "H-STALE", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"},
			Metadata: map[string]string{"gc.routed_to": "reviewer"},
		},
	}, nil)
	check := newHoldLabelRoutedToCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
	details := strings.Join(res.Details, "\n")
	for _, want := range []string{
		`city bead H-MISSING has hold:mayor but gc.routed_to=""; use "gastown.mayor"`,
		`city bead H-STALE has hold:mayor but gc.routed_to="reviewer"; use "gastown.mayor"`,
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	for _, notWant := range []string{"H-SHORT", "H-QUALIFIED"} {
		if strings.Contains(details, notWant) {
			t.Fatalf("details should not mention %q (a form v2-routed-to-namespace owns):\n%s", notWant, details)
		}
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res2 := check.Run(&doctor.CheckContext{}); res2.Status != doctor.StatusOK {
		t.Fatalf("post-fix Run status = %v, want OK: %#v", res2.Status, res2)
	}

	for _, tc := range []struct{ id, want string }{
		{id: "H-MISSING", want: "gastown.mayor"},
		{id: "H-STALE", want: "gastown.mayor"},
		{id: "H-QUALIFIED", want: "gastown.mayor"},
		// Left exactly as found: rewriting the short form is
		// v2-routed-to-namespace's job, not this check's.
		{id: "H-SHORT", want: "mayor"},
	} {
		b, err := store.Get(tc.id)
		if err != nil {
			t.Fatalf("get %s: %v", tc.id, err)
		}
		if got := b.Metadata["gc.routed_to"]; got != tc.want {
			t.Errorf("%s gc.routed_to = %q, want %q", tc.id, got, tc.want)
		}
	}
}

// TestHoldLabelRoutedToCheckLeavesAmbiguousHoldRouteUnresolved covers a hold
// value that names a bound agent in more than one binding: there is no single
// qualified route to backfill, so Fix must leave the bead alone and Run must
// keep reporting it with the candidates — the same manual-resolution handling
// v2-routed-to-namespace gives an ambiguous short form. Guessing one binding
// here would reintroduce a write the other check cannot agree with.
func TestHoldLabelRoutedToCheckLeavesAmbiguousHoldRouteUnresolved(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Agents: []config.Agent{
		{Name: "mayor", BindingName: "gastown"},
		{Name: "mayor", BindingName: "otherpack"},
	}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "H-AMBIG", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"}},
		{
			ID: "H-PICKED", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"},
			Metadata: map[string]string{"gc.routed_to": "otherpack.mayor"},
		},
	}, nil)
	check := newHoldLabelRoutedToCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("status after fix = %v, want warning (ambiguous hold route must stay unresolved): %#v", res.Status, res)
	}
	details := strings.Join(res.Details, "\n")
	want := `city bead H-AMBIG has hold:mayor but gc.routed_to=""; use one of gastown.mayor, otherpack.mayor`
	if !strings.Contains(details, want) {
		t.Fatalf("details missing %q:\n%s", want, details)
	}
	if strings.Contains(details, "H-PICKED") {
		t.Fatalf("details should not mention H-PICKED (already on one of the candidates):\n%s", details)
	}

	ambig, err := store.Get("H-AMBIG")
	if err != nil {
		t.Fatalf("get H-AMBIG: %v", err)
	}
	if got := ambig.Metadata["gc.routed_to"]; got != "" {
		t.Errorf("H-AMBIG gc.routed_to = %q, want untouched empty (no unambiguous target)", got)
	}
}

// TestHoldLabelAndV2RoutedToChecksConvergeOnOneRoute is the regression for
// gascity-enqx. hold-label-routed-to and v2-routed-to-namespace both write
// gc.routed_to, and they used to demand opposite values for a hold:<short>
// bead: each check's fix re-broke the other, so the field flipped between
// "mayor" and "gastown.mayor" indefinitely, doctor reported the finding
// unresolved on every patrol, and the agents acting on those fix hints
// repaired it against each other for ~15h. Neither check's own idempotency
// test could see it — only running both can. Both orders and repeated passes
// must land on one stable route with both checks reporting OK.
func TestHoldLabelAndV2RoutedToChecksConvergeOnOneRoute(t *testing.T) {
	const qualified = "gastown.mayor"
	for _, tc := range []struct {
		name  string
		order []string
	}{
		{name: "v2 first (registration order)", order: []string{"v2", "hold"}},
		{name: "hold first", order: []string{"hold", "v2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			cfg := &config.City{Agents: []config.Agent{{Name: "mayor", BindingName: "gastown"}}}
			store := beads.NewMemStoreFrom(0, []beads.Bead{
				// The shape gascity-a2j3 was stuck in: a bare hold label and
				// the short-form route the hold check used to demand.
				{
					ID: "HELD", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"},
					Metadata: map[string]string{"gc.routed_to": "mayor"},
				},
			}, nil)
			factory := func(path string) (beads.Store, error) {
				if path != cityDir {
					return nil, fmt.Errorf("unexpected store path %q", path)
				}
				return store, nil
			}
			checks := map[string]doctor.Check{
				"v2":   newV2RoutedToNamespaceCheck(cfg, cityDir, factory),
				"hold": newHoldLabelRoutedToCheck(cfg, cityDir, factory),
			}

			// Two full passes: one pass cannot distinguish convergence from a
			// loop whose second half has not run yet.
			for pass := 1; pass <= 2; pass++ {
				for _, name := range tc.order {
					if err := checks[name].Fix(&doctor.CheckContext{}); err != nil {
						t.Fatalf("pass %d: %s Fix: %v", pass, name, err)
					}
					held, err := store.Get("HELD")
					if err != nil {
						t.Fatalf("pass %d: get HELD after %s: %v", pass, name, err)
					}
					if got := held.Metadata["gc.routed_to"]; got != "mayor" && got != qualified {
						t.Fatalf("pass %d: %s wrote gc.routed_to=%q, want one of the two routes under contention", pass, name, got)
					}
				}
			}

			held, err := store.Get("HELD")
			if err != nil {
				t.Fatalf("get HELD: %v", err)
			}
			if got := held.Metadata["gc.routed_to"]; got != qualified {
				t.Fatalf("gc.routed_to = %q after both fixes, want %q (the form that resolves)", got, qualified)
			}
			for _, name := range []string{"v2", "hold"} {
				if res := checks[name].Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
					t.Fatalf("%s still flags the bead after both checks ran: %#v", name, res)
				}
			}
		})
	}
}
