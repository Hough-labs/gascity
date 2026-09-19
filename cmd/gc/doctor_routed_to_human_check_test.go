package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// humanHaltStoreFactory serves the given stores by scope path and fails the
// check for any scope the test did not set up, so an unexpected scope surfaces
// as a skipped-scope finding rather than passing silently.
func humanHaltStoreFactory(stores map[string]beads.Store) func(string) (beads.Store, error) {
	return func(path string) (beads.Store, error) {
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}
}

// humanHaltBead builds a bead halted to a human: the shape a gate leaves behind
// when it clears the assignee and routes the bead to the human fallback.
func humanHaltBead(id, status string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Title:    "halted work",
		Type:     "bug",
		Status:   status,
		Metadata: map[string]string{"gc.routed_to": "human"},
	}
}

func TestRoutedToHumanCheckNamesHaltedBeadInMessage(t *testing.T) {
	cityDir := t.TempDir()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		humanHaltBead("CITY-1", "open"),
		{ID: "CITY-2", Title: "ordinary work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "gastown.polecat"}},
	}, nil)

	result := newRoutedToHumanCheck(&config.City{}, cityDir, humanHaltStoreFactory(map[string]beads.Store{
		cityDir: cityStore,
	})).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	// The defect being fixed is that a halted bead emits no signal, so the ID
	// must ride in Message. Details are shown only in verbose mode
	// (doctor.CheckResult.Details), so a check that named the bead only there
	// would still be silent on a default `gc doctor` run.
	if !strings.Contains(result.Message, "CITY-1") {
		t.Fatalf("message %q does not name the halted bead", result.Message)
	}
	if strings.Contains(result.Message, "CITY-2") {
		t.Fatalf("message %q names a bead that is routed to an agent", result.Message)
	}
	details := strings.Join(result.Details, "\n")
	want := `city bead CITY-1 is halted to a human (gc.routed_to="human", status=open)`
	if !strings.Contains(details, want) {
		t.Fatalf("details missing %q:\n%s", want, details)
	}
	if result.FixHint == "" {
		t.Error("a check that cannot auto-fix must tell the reader what to do")
	}
}

// TestRoutedToHumanCheckWarnsOnBlockedHaltedBead pins the status the worked
// example actually carried: gascity-hpqe was halted with status=blocked and
// assignee=null, not status=open. A query filtered to Status:"open" would miss
// exactly the bead this check exists to surface, so the scan must match every
// non-closed status (the same reasoning hold-label-routed-to records for its
// own scan, ga-fm2vgd.2).
func TestRoutedToHumanCheckWarnsOnBlockedHaltedBead(t *testing.T) {
	cityDir := t.TempDir()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{humanHaltBead("CITY-1", "blocked")}, nil)

	result := newRoutedToHumanCheck(&config.City{}, cityDir, humanHaltStoreFactory(map[string]beads.Store{
		cityDir: cityStore,
	})).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning for a blocked halt: %#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "CITY-1") {
		t.Fatalf("message %q does not name the blocked halted bead", result.Message)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "status=blocked") {
		t.Fatalf("details do not report the halt status:\n%s", details)
	}
}

func TestRoutedToHumanCheckOKWhenNothingIsHalted(t *testing.T) {
	cityDir := t.TempDir()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "ordinary work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "gastown.polecat"}},
	}, nil)

	result := newRoutedToHumanCheck(&config.City{}, cityDir, humanHaltStoreFactory(map[string]beads.Store{
		cityDir: cityStore,
	})).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok: %#v", result.Status, result)
	}
}

// TestRoutedToHumanCheckIgnoresClosedHaltedBead is the regression guard that
// keeps this check a signal rather than a nag. A closed halt is a resolved
// halt; warning about it forever would make every historical halt permanent
// noise, readers would learn to ignore the finding, and gc.routed_to=human
// would be write-only again in practice — just louder.
func TestRoutedToHumanCheckIgnoresClosedHaltedBead(t *testing.T) {
	cityDir := t.TempDir()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{humanHaltBead("CITY-1", "closed")}, nil)

	result := newRoutedToHumanCheck(&config.City{}, cityDir, humanHaltStoreFactory(map[string]beads.Store{
		cityDir: cityStore,
	})).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok for a closed (resolved) halt: %#v", result.Status, result)
	}
	if strings.Contains(result.Message, "CITY-1") {
		t.Fatalf("message %q names a closed halt", result.Message)
	}
}

func TestRoutedToHumanCheckScansRigScopes(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{humanHaltBead("CITY-1", "open")}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{humanHaltBead("RIG-1", "blocked")}, nil)

	result := newRoutedToHumanCheck(cfg, cityDir, humanHaltStoreFactory(map[string]beads.Store{
		cityDir: cityStore,
		rigDir:  rigStore,
	})).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	for _, want := range []string{"CITY-1", "RIG-1"} {
		if !strings.Contains(result.Message, want) {
			t.Fatalf("message %q does not name %q", result.Message, want)
		}
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		`city bead CITY-1 is halted to a human (gc.routed_to="human", status=open)`,
		`rig repo bead RIG-1 is halted to a human (gc.routed_to="human", status=blocked)`,
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
}

func TestRoutedToHumanCheckSkipsEffectivelySuspendedRigs(t *testing.T) {
	cityDir := t.TempDir()
	startSuspendedDir := t.TempDir()
	runtimeSuspendedDir := t.TempDir()
	activeDir := t.TempDir()
	suspend := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityDir, "runtime-suspended", &suspend); err != nil {
		t.Fatalf("SetRigSuspended: %v", err)
	}
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "start-suspended", Path: startSuspendedDir, SuspendedOnStart: true},
		{Name: "runtime-suspended", Path: runtimeSuspendedDir},
		{Name: "active", Path: activeDir},
	}}

	var opened []string
	result := newRoutedToHumanCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		opened = append(opened, path)
		return beads.NewMemStoreFrom(0, nil, nil), nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok: %#v", result.Status, result)
	}
	for _, notWant := range []string{startSuspendedDir, runtimeSuspendedDir} {
		if containsString(opened, notWant) {
			t.Fatalf("opened suspended rig store %q; opened=%v", notWant, opened)
		}
	}
	for _, want := range []string{cityDir, activeDir} {
		if !containsString(opened, want) {
			t.Fatalf("did not open active scope %q; opened=%v", want, opened)
		}
	}
}

// TestRoutedToHumanCheckUsesTargetedRouteQuery pins the query shape: the route
// value is a fixed string, so this check must look it up by metadata rather
// than scanning every bead in every store. It must also leave closed beads to
// the store's default exclusion instead of asking for them.
func TestRoutedToHumanCheckUsesTargetedRouteQuery(t *testing.T) {
	cityDir := t.TempDir()
	store := &routeQuerySpyStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		humanHaltBead("CITY-1", "open"),
	}, nil)}

	result := newRoutedToHumanCheck(&config.City{}, cityDir, humanHaltStoreFactory(map[string]beads.Store{
		cityDir: store,
	})).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	if len(store.queries) == 0 {
		t.Fatal("expected at least one route query")
	}
	for _, query := range store.queries {
		if query.AllowScan {
			t.Fatalf("query %+v used AllowScan; the human route is a fixed value and must be a targeted lookup", query)
		}
		if got := query.Metadata["gc.routed_to"]; got != "human" {
			t.Fatalf("query %+v does not filter gc.routed_to to the human route", query)
		}
		if query.IncludeClosed {
			t.Fatalf("query %+v asked for closed beads; a closed halt is resolved", query)
		}
	}
}

func TestRoutedToHumanCheckWarnsOnSkippedStoreScopes(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}

	result := newRoutedToHumanCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		switch path {
		case cityDir:
			return nil, errors.New("city offline")
		case rigDir:
			return routeListErrorStore{err: errors.New("rig offline")}, nil
		default:
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"city skipped: opening bead store: city offline",
		"rig repo skipped: listing beads: rig offline",
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
}

// TestRoutedToHumanCheckSurfacingIsTheFix pins the design ruling: this check
// reports halted beads and never remediates them. Re-routing halted work needs
// a decision about who owns it, which is not a decision a doctor fix can make.
func TestRoutedToHumanCheckSurfacingIsTheFix(t *testing.T) {
	check := newRoutedToHumanCheck(&config.City{}, t.TempDir(), nil)
	if check.CanFix() {
		t.Error("CanFix() = true, want false: there is no safe automatic remedy for a halted bead")
	}
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Errorf("Fix() = %v, want nil", err)
	}
	if check.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
}

func TestBuildDoctorChecksRegistersRoutedToHumanHalts(t *testing.T) {
	checks := buildDoctorChecks(t.TempDir(), &config.City{}, nil, buildDoctorChecksOpts{
		Stderr:               io.Discard,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})
	for _, check := range checks {
		if check.Name() == "routed-to-human-halts" {
			return
		}
	}
	t.Fatalf("buildDoctorChecks did not register routed-to-human-halts; registered=%v", doctorCheckNames(checks))
}

// humanHaltSupersetStore answers every List with its whole corpus, ignoring the
// query's filters. A backend is allowed to satisfy a filtered query with a
// superset (internal/beads/query.go), so the check must do its own final
// filtering rather than trusting the query it asked for. Without that, a
// superset backend would resurrect every closed halt as permanent noise.
type humanHaltSupersetStore struct {
	beads.Store
	all []beads.Bead
}

func (s humanHaltSupersetStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return s.all, nil
}

func TestRoutedToHumanCheckFiltersSupersetStoreResults(t *testing.T) {
	cityDir := t.TempDir()
	store := humanHaltSupersetStore{all: []beads.Bead{
		humanHaltBead("CITY-OPEN", "open"),
		humanHaltBead("CITY-CLOSED", "closed"),
		{ID: "CITY-AGENT", Title: "ordinary work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "gastown.polecat"}},
	}}

	result := newRoutedToHumanCheck(&config.City{}, cityDir, humanHaltStoreFactory(map[string]beads.Store{
		cityDir: store,
	})).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "CITY-OPEN") {
		t.Fatalf("message %q does not name the unresolved halt", result.Message)
	}
	reported := strings.Join(append([]string{result.Message}, result.Details...), "\n")
	for _, notWant := range []string{"CITY-CLOSED", "CITY-AGENT"} {
		if strings.Contains(reported, notWant) {
			t.Fatalf("result reports %q, which the check must filter out itself:\n%s", notWant, reported)
		}
	}
}
