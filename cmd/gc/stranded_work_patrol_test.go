package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/strandedwork"
)

// strandedRepo builds a repository whose default branch is "edge-integration"
// and returns its path. It mirrors the real shape: work branches are created
// off origin/<default>, and a published branch also has a remote-tracking ref.
func strandedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, dir, "init", "-b", "edge-integration")
	mustGit(t, dir, "config", "user.email", "test@test.invalid")
	mustGit(t, dir, "config", "user.name", "Test")
	mustGit(t, dir, "commit", "--allow-empty", "-m", "base")
	mustGit(t, dir, "update-ref", "refs/remotes/origin/edge-integration", "HEAD")
	mustGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/edge-integration")
	return dir
}

// patrolBranch creates branch at n commits ahead of origin/edge-integration,
// leaving the repository back on its default branch.
func patrolBranch(t *testing.T, dir, branch string, commits int) {
	t.Helper()
	mustGit(t, dir, "checkout", "-b", branch, "edge-integration")
	for i := 0; i < commits; i++ {
		mustGit(t, dir, "commit", "--allow-empty", "-m", branch+" work")
	}
	mustGit(t, dir, "checkout", "edge-integration")
}

func TestGitBranchProberReportsUnpublishedWorkAheadOfTarget(t *testing.T) {
	dir := strandedRepo(t)
	patrolBranch(t, dir, "polecat/gascity-cgh", 3)
	prober := newGitBranchProber(git.New(dir))

	got, err := prober.ProbeBranch("polecat/gascity-cgh", "edge-integration")
	if err != nil {
		t.Fatalf("ProbeBranch: %v", err)
	}
	want := strandedwork.BranchState{LocalExists: true, Target: "edge-integration", CommitsAhead: 3}
	if got != want {
		t.Errorf("ProbeBranch = %+v, want %+v", got, want)
	}
}

func TestGitBranchProberSeesAPublishedBranch(t *testing.T) {
	dir := strandedRepo(t)
	patrolBranch(t, dir, "polecat/gascity-3vr", 1)
	mustGit(t, dir, "update-ref", "refs/remotes/origin/polecat/gascity-3vr", "polecat/gascity-3vr")
	prober := newGitBranchProber(git.New(dir))

	got, err := prober.ProbeBranch("polecat/gascity-3vr", "edge-integration")
	if err != nil {
		t.Fatalf("ProbeBranch: %v", err)
	}
	if !got.OnOrigin {
		t.Errorf("ProbeBranch = %+v, want OnOrigin=true for a published branch", got)
	}
}

func TestGitBranchProberResolvesTheRepositoryDefaultForAnEmptyTarget(t *testing.T) {
	// Work stranded before its submit step records no target, because submit is
	// what writes one.
	dir := strandedRepo(t)
	patrolBranch(t, dir, "polecat/gascity-cgh", 2)
	prober := newGitBranchProber(git.New(dir))

	got, err := prober.ProbeBranch("polecat/gascity-cgh", "")
	if err != nil {
		t.Fatalf("ProbeBranch: %v", err)
	}
	if got.Target != "edge-integration" || got.CommitsAhead != 2 {
		t.Errorf("ProbeBranch = %+v, want the repository default resolved to edge-integration with 2 commits", got)
	}
}

func TestGitBranchProberReportsAnAbsentBranchWithoutProbingFurther(t *testing.T) {
	dir := strandedRepo(t)
	prober := newGitBranchProber(git.New(dir))

	got, err := prober.ProbeBranch("polecat/never-existed", "edge-integration")
	if err != nil {
		t.Fatalf("ProbeBranch: %v", err)
	}
	if got.LocalExists {
		t.Errorf("ProbeBranch = %+v, want LocalExists=false", got)
	}
}

func TestGitBranchProberFailsOnAnUnresolvableTarget(t *testing.T) {
	// Counting against a target the repository does not have would silently
	// answer the wrong question; it must surface instead.
	dir := strandedRepo(t)
	patrolBranch(t, dir, "polecat/gascity-cgh", 1)
	prober := newGitBranchProber(git.New(dir))

	if _, err := prober.ProbeBranch("polecat/gascity-cgh", "no-such-target"); err == nil {
		t.Error("ProbeBranch returned no error for an unresolvable target")
	}
}

func TestStrandedWorkThrottleEmitsOnceUntilTheFindingChanges(t *testing.T) {
	throttle := newStrandedWorkThrottle()
	finding := strandedwork.Finding{
		BeadID: "gascity-cgh", StoreRef: "rig:gascity",
		Branch: "polecat/gascity-cgh", Target: "edge-integration", CommitsAhead: 3,
	}

	if got := throttle.admit([]strandedwork.Finding{finding}); len(got) != 1 {
		t.Fatalf("first pass admitted %d findings, want 1", len(got))
	}
	// The controller ticks every 30s; an unchanged strand must not restate
	// itself thousands of times a day.
	if got := throttle.admit([]strandedwork.Finding{finding}); len(got) != 0 {
		t.Fatalf("repeat pass admitted %+v, want nothing new", got)
	}

	// More commits piled onto the same branch is new information.
	grown := finding
	grown.CommitsAhead = 5
	if got := throttle.admit([]strandedwork.Finding{grown}); len(got) != 1 {
		t.Fatalf("grown finding admitted %d, want 1", len(got))
	}
	// So is the branch reaching origin, which changes how urgent it is.
	published := grown
	published.OnOrigin = true
	if got := throttle.admit([]strandedwork.Finding{published}); len(got) != 1 {
		t.Fatalf("published finding admitted %d, want 1", len(got))
	}
}

func TestStrandedWorkThrottleReEmitsAfterABeadIsRepairedAndStrandsAgain(t *testing.T) {
	// Without forgetting repaired beads, a second strand with the identical
	// shape would be suppressed forever — the exact silence this patrol exists
	// to end.
	throttle := newStrandedWorkThrottle()
	finding := strandedwork.Finding{
		BeadID: "gascity-rwu", StoreRef: "rig:gascity",
		Branch: "polecat/gascity-rwu", Target: "edge-integration", CommitsAhead: 3,
	}

	throttle.admit([]strandedwork.Finding{finding})
	throttle.admit(nil) // repaired: the bead is no longer stranded
	if got := throttle.admit([]strandedwork.Finding{finding}); len(got) != 1 {
		t.Fatalf("re-strand admitted %d findings, want 1", len(got))
	}
}

func TestEmitStrandedWorkEventsCarriesTheRecoveryDecisionFields(t *testing.T) {
	rec := events.NewFake()
	finding := strandedwork.Finding{
		BeadID: "gascity-cgh", StoreRef: "rig:gascity",
		Branch: "polecat/gascity-cgh", Target: "edge-integration",
		CommitsAhead: 3, OnOrigin: false,
	}
	var stderr bytes.Buffer

	emitStrandedWorkEvents(rec, []strandedwork.Finding{finding}, "gc supervisor", &stderr)

	if len(rec.Events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(rec.Events))
	}
	ev := rec.Events[0]
	if ev.Type != events.BeadStranded {
		t.Errorf("event type = %q, want %q", ev.Type, events.BeadStranded)
	}
	if ev.Subject != "gascity-cgh" {
		t.Errorf("event subject = %q, want the stranded bead id", ev.Subject)
	}
	var payload api.BeadStrandedPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	want := api.BeadStrandedPayload{
		BeadID: "gascity-cgh", StoreRef: "rig:gascity",
		Branch: "polecat/gascity-cgh", Target: "edge-integration",
		CommitsAhead: 3, OnOrigin: false,
	}
	if payload != want {
		t.Errorf("payload = %+v, want %+v", payload, want)
	}
	// An operator reading stderr must see the same facts without the event log.
	for _, want := range []string{"gascity-cgh", "polecat/gascity-cgh", "edge-integration", "3", "not on origin"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to mention %q", stderr.String(), want)
		}
	}
}

func TestScanStrandedWorkScopeFindsTheObservedStrandedShape(t *testing.T) {
	// End-to-end over the real git boundary: the metadata signature plus a
	// branch that genuinely holds unmerged, unpublished commits.
	dir := strandedRepo(t)
	patrolBranch(t, dir, "polecat/gascity-cgh", 3)
	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{
		Title:    "the cmd/gc test-budget fix",
		Metadata: beads.StringMap{beadmeta.BranchMetadataKey: "polecat/gascity-cgh"},
	})
	if err != nil {
		t.Fatalf("seeding bead: %v", err)
	}

	scope := strandedWorkScope{storeRef: "rig:gascity", repoPath: dir, store: store}
	got, err := scanStrandedWorkScope(scope)
	if err != nil {
		t.Fatalf("scanStrandedWorkScope: %v", err)
	}
	want := strandedwork.Finding{
		BeadID: created.ID, StoreRef: "rig:gascity",
		Branch: "polecat/gascity-cgh", Target: "edge-integration",
		CommitsAhead: 3, OnOrigin: false,
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("scanStrandedWorkScope = %+v, want [%+v]", got, want)
	}
}

func TestScanStrandedWorkScopeSkipsAScopeWithoutARepository(t *testing.T) {
	// A store whose scope has no git repository (a city that is not a checkout)
	// holds no branches to strand; that is normal, not a failure.
	scope := strandedWorkScope{storeRef: "city:gc", repoPath: t.TempDir(), store: beads.NewMemStore()}

	got, err := scanStrandedWorkScope(scope)
	if err != nil {
		t.Errorf("scanStrandedWorkScope error = %v, want none", err)
	}
	if got != nil {
		t.Errorf("scanStrandedWorkScope = %+v, want no findings", got)
	}
}

func TestStrandedWorkScopesCoverTheCityAndEveryUnsuspendedRig(t *testing.T) {
	cityStore := beads.NewMemStore()
	liveStore := beads.NewMemStore()
	suspendedStore := beads.NewMemStore()
	cr := &CityRuntime{
		cityPath: "/city",
		cityName: "gc",
		cfg: &config.City{
			Rigs: []config.Rig{
				{Name: "gascity", Path: "/repos/gascity"},
				{Name: "winnow", Path: "/repos/winnow", SuspendedOnStart: true},
			},
		},
		standaloneCityStore: cityStore,
		standaloneRigStores: map[string]beads.Store{
			"gascity": liveStore,
			"winnow":  suspendedStore,
		},
	}

	got := cr.strandedWorkScopes()

	byRef := map[string]strandedWorkScope{}
	for _, s := range got {
		byRef[s.storeRef] = s
	}
	if _, ok := byRef["rig:winnow"]; ok {
		t.Error("scopes include the suspended rig winnow; a suspended rig is not being worked")
	}
	city, ok := byRef["city:gc"]
	if !ok {
		t.Fatalf("scopes = %+v, want the city store included", got)
	}
	if city.repoPath != "/city" || city.store != beads.Store(cityStore) {
		t.Errorf("city scope = %+v, want the city path and store", city)
	}
	rig, ok := byRef["rig:gascity"]
	if !ok {
		t.Fatalf("scopes = %+v, want the gascity rig included", got)
	}
	if rig.repoPath != "/repos/gascity" || rig.store != beads.Store(liveStore) {
		t.Errorf("gascity scope = %+v, want the rig path and store", rig)
	}
}

func TestPatrolStrandedWorkEmitsOncePerUnchangedStrand(t *testing.T) {
	dir := strandedRepo(t)
	patrolBranch(t, dir, "polecat/gascity-cgh", 3)
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:    "the cmd/gc test-budget fix",
		Metadata: beads.StringMap{beadmeta.BranchMetadataKey: "polecat/gascity-cgh"},
	}); err != nil {
		t.Fatalf("seeding bead: %v", err)
	}
	rec := events.NewFake()
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "gc",
		cfg:                 &config.City{Rigs: []config.Rig{{Name: "gascity", Path: dir}}},
		standaloneRigStores: map[string]beads.Store{"gascity": store},
		strandedWork:        newStrandedWorkThrottle(),
		rec:                 rec,
		stderr:              &stderr,
		logPrefix:           "gc supervisor",
	}

	// The second pass is placed past the scan interval deliberately: otherwise
	// the cadence gate would suppress it and this test would prove nothing
	// about emission throttling.
	now := time.Now()
	cr.patrolStrandedWork(now)
	if len(rec.Events) != 1 {
		t.Fatalf("first patrol recorded %d events, want 1: %s", len(rec.Events), stderr.String())
	}
	cr.patrolStrandedWork(now.Add(strandedWorkScanInterval))
	if len(rec.Events) != 1 {
		t.Fatalf("second patrol recorded %d events, want the unchanged strand throttled to 1", len(rec.Events))
	}
}

func TestPatrolStrandedWorkScansOnItsOwnCadenceRatherThanEveryTick(t *testing.T) {
	// A strand is a condition measured in hours and days — gascity-cgh sat
	// unreachable for six — so rescanning every 30s controller tick buys no
	// detection speed an operator can use, and charges every store a list and
	// every repository a probe to learn nothing changed.
	dir := strandedRepo(t)
	patrolBranch(t, dir, "polecat/gascity-cgh", 3)
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:    "the cmd/gc test-budget fix",
		Metadata: beads.StringMap{beadmeta.BranchMetadataKey: "polecat/gascity-cgh"},
	}); err != nil {
		t.Fatalf("seeding bead: %v", err)
	}
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "gc",
		cfg:                 &config.City{Rigs: []config.Rig{{Name: "gascity", Path: dir}}},
		standaloneRigStores: map[string]beads.Store{"gascity": store},
		strandedWork:        newStrandedWorkThrottle(),
		rec:                 events.NewFake(),
		stderr:              &stderr,
		logPrefix:           "gc supervisor",
	}
	counter := withCountingStrandedWorkProbe(t)

	now := time.Now()
	cr.patrolStrandedWork(now)
	afterFirst := counter.calls
	if afterFirst == 0 {
		t.Fatalf("first patrol made no git calls; it did not scan: %s", stderr.String())
	}

	// A tick later, well inside the interval: no scan, so no git at all.
	cr.patrolStrandedWork(now.Add(30 * time.Second))
	if counter.calls != afterFirst {
		t.Errorf("patrol one tick later made %d extra git call(s), want 0", counter.calls-afterFirst)
	}

	// Once the interval has elapsed it scans again — the patrol is throttled,
	// not disabled.
	cr.patrolStrandedWork(now.Add(strandedWorkScanInterval))
	if counter.calls == afterFirst {
		t.Error("patrol did not scan again after its interval elapsed")
	}
}

func TestStrandedWorkThrottleScansOnTheFirstPass(t *testing.T) {
	// A controller that has just started has never scanned; the gate must not
	// make it wait out an interval before the first look.
	if !newStrandedWorkThrottle().beginScan(time.Now()) {
		t.Error("beginScan refused the first pass")
	}
}

// countingStrandedWorkProbe wraps a real probe and counts every git invocation
// it forwards, so the patrol's cost on the healthy path can be asserted rather
// than assumed.
type countingStrandedWorkProbe struct {
	inner strandedWorkGitProbe
	calls int
}

func (p *countingStrandedWorkProbe) IsRepo() bool {
	p.calls++
	return p.inner.IsRepo()
}

func (p *countingStrandedWorkProbe) RefExists(fullRef string) (bool, error) {
	p.calls++
	return p.inner.RefExists(fullRef)
}

func (p *countingStrandedWorkProbe) CountCommitsAhead(base, tip string) (int, error) {
	p.calls++
	return p.inner.CountCommitsAhead(base, tip)
}

func (p *countingStrandedWorkProbe) DefaultBranch() (string, error) {
	p.calls++
	return p.inner.DefaultBranch()
}

// withCountingStrandedWorkProbe swaps the patrol's git factory for one that
// counts invocations, returning the counter.
func withCountingStrandedWorkProbe(t *testing.T) *countingStrandedWorkProbe {
	t.Helper()
	counter := &countingStrandedWorkProbe{}
	original := newStrandedWorkGitProbe
	newStrandedWorkGitProbe = func(workDir string) strandedWorkGitProbe {
		counter.inner = git.New(workDir)
		return counter
	}
	t.Cleanup(func() { newStrandedWorkGitProbe = original })
	return counter
}

func TestScanStrandedWorkScopeRunsNoGitWhenNoBeadIsACandidate(t *testing.T) {
	// The healthy path is the one that runs forever: the controller ticks every
	// 30s over every scope, and almost always nothing is stranded. A patrol that
	// shells out before it has a candidate pays a git subprocess per scope per
	// tick in perpetuity for an answer it never needed.
	dir := strandedRepo(t)
	store := beads.NewMemStore()
	// Open beads that fail the cheap metadata half: one routed, one with no
	// branch recorded at all.
	for _, meta := range []beads.StringMap{
		{beadmeta.BranchMetadataKey: "polecat/gascity-cgh", beadmeta.RoutedToMetadataKey: "gascity/gastown.polecat"},
		{},
	} {
		if _, err := store.Create(beads.Bead{Title: "not stranded", Metadata: meta}); err != nil {
			t.Fatalf("seeding bead: %v", err)
		}
	}
	counter := withCountingStrandedWorkProbe(t)

	got, err := scanStrandedWorkScope(strandedWorkScope{storeRef: "rig:gascity", repoPath: dir, store: store})
	if err != nil {
		t.Fatalf("scanStrandedWorkScope: %v", err)
	}
	if got != nil {
		t.Errorf("scanStrandedWorkScope = %+v, want no findings", got)
	}
	if counter.calls != 0 {
		t.Errorf("healthy pass made %d git call(s), want 0", counter.calls)
	}
}

func TestScanStrandedWorkScopeChecksTheRepositoryOnceAcrossManyCandidates(t *testing.T) {
	// The repository check is per-scope information, not per-bead: paying it
	// again for every candidate would scale the patrol's subprocess count with
	// the size of the strand it is reporting.
	dir := strandedRepo(t)
	for _, branch := range []string{"polecat/gascity-cgh", "polecat/gascity-3vr"} {
		patrolBranch(t, dir, branch, 1)
	}
	store := beads.NewMemStore()
	for _, branch := range []string{"polecat/gascity-cgh", "polecat/gascity-3vr"} {
		if _, err := store.Create(beads.Bead{
			Title:    "stranded",
			Metadata: beads.StringMap{beadmeta.BranchMetadataKey: branch, beadmeta.TargetMetadataKey: "edge-integration"},
		}); err != nil {
			t.Fatalf("seeding bead: %v", err)
		}
	}
	counter := withCountingStrandedWorkProbe(t)

	got, err := scanStrandedWorkScope(strandedWorkScope{storeRef: "rig:gascity", repoPath: dir, store: store})
	if err != nil {
		t.Fatalf("scanStrandedWorkScope: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("scanStrandedWorkScope = %+v, want both strands reported", got)
	}
	// One IsRepo, then per candidate: branch ref, target ref, count, origin ref.
	if want := 1 + 2*4; counter.calls != want {
		t.Errorf("git calls = %d, want %d (one repository check plus four probes per candidate)", counter.calls, want)
	}
}

func TestScanStrandedWorkScopeReportsNothingWhenTheScopeIsNotARepository(t *testing.T) {
	// A candidate bead in a directory that is not a checkout (a city that holds
	// beads but no source) strands nothing: there are no refs to hold work.
	// It must degrade to "no findings", not to an error an operator must triage.
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:    "candidate in a non-repository scope",
		Metadata: beads.StringMap{beadmeta.BranchMetadataKey: "polecat/gascity-cgh"},
	}); err != nil {
		t.Fatalf("seeding bead: %v", err)
	}

	got, err := scanStrandedWorkScope(strandedWorkScope{storeRef: "city:gc", repoPath: t.TempDir(), store: store})
	if err != nil {
		t.Errorf("scanStrandedWorkScope error = %v, want none", err)
	}
	if got != nil {
		t.Errorf("scanStrandedWorkScope = %+v, want no findings", got)
	}
}
