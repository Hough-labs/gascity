package strandedwork

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

const (
	testStoreRef      = "rig:gascity"
	testDefaultTarget = "edge-integration"
)

// fakeProber scripts one BranchState per branch name. A branch with no scripted
// entry reports LocalExists=false, standing in for a branch absent from the
// repository's local refs. It records the (branch, target) pairs it was asked
// about so tests can assert the target fallback reached the repository.
type fakeProber struct {
	states map[string]BranchState
	err    error
	asked  []string
}

func (f *fakeProber) ProbeBranch(branch, target string) (BranchState, error) {
	f.asked = append(f.asked, branch+"->"+target)
	if f.err != nil {
		return BranchState{}, f.err
	}
	resolved := target
	if resolved == "" {
		resolved = testDefaultTarget
	}
	st := f.states[branch]
	st.Target = resolved
	return st, nil
}

// strandedBead builds a work bead carrying the full metadata half of the
// stranded signature: open, unassigned, unrouted, with a work branch recorded.
// Each mutator negates or varies one part of it.
func strandedBead(mutators ...func(*beads.Bead)) beads.Bead {
	b := beads.Bead{
		ID:     "gascity-cgh",
		Title:  "the cmd/gc test-budget fix",
		Status: "open",
		Metadata: beads.StringMap{
			beadmeta.BranchMetadataKey: "polecat/gascity-cgh",
			beadmeta.TargetMetadataKey: testDefaultTarget,
		},
	}
	for _, m := range mutators {
		m(&b)
	}
	return b
}

// listStore is a beads.Store that answers List from a fixed slice, honoring
// the status filter the way a real store does. Scan reads nothing else, so the
// embedded nil Store never has to be satisfied. Unlike beads.MemStore it keeps
// the IDs and statuses the test supplies, which is what the signature's
// status/identity assertions are about.
type listStore struct {
	beads.Store
	items []beads.Bead
	err   error
}

func (l listStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if l.err != nil {
		return nil, l.err
	}
	var out []beads.Bead
	for _, b := range l.items {
		if query.Status != "" && b.Status != query.Status {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// storeWith returns a store holding the given beads verbatim.
func storeWith(t *testing.T, items ...beads.Bead) listStore {
	t.Helper()
	return listStore{items: items}
}

// aheadProber scripts the observed shape of gascity-cgh: a local branch three
// commits ahead of its target that was never published.
func aheadProber() *fakeProber {
	return &fakeProber{states: map[string]BranchState{
		"polecat/gascity-cgh": {LocalExists: true, CommitsAhead: 3},
	}}
}

func TestScanReportsFullStrandedSignature(t *testing.T) {
	store := storeWith(t, strandedBead())

	got, err := Scan(testStoreRef, store, aheadProber())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []Finding{{
		BeadID:       "gascity-cgh",
		StoreRef:     testStoreRef,
		Branch:       "polecat/gascity-cgh",
		Target:       testDefaultTarget,
		CommitsAhead: 3,
		OnOrigin:     false,
	}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("Scan findings = %+v, want %+v", got, want)
	}
}

func TestScanReportsBranchPublishedToOrigin(t *testing.T) {
	// gascity-3vr's branch WAS on origin and gascity-cgh's was not. The origin
	// flag separates "recoverable from the remote" from "one disk failure from
	// total loss", so it must survive the scan rather than be assumed.
	store := storeWith(t, strandedBead())
	prober := &fakeProber{states: map[string]BranchState{
		"polecat/gascity-cgh": {LocalExists: true, CommitsAhead: 1, OnOrigin: true},
	}}

	got, err := Scan(testStoreRef, store, prober)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 || !got[0].OnOrigin {
		t.Fatalf("Scan findings = %+v, want a single finding with OnOrigin=true", got)
	}
}

func TestScanSkipsEachNegatedSignaturePart(t *testing.T) {
	// Every part of the metadata half of the signature, negated one at a time.
	// Each negation makes the bead discoverable by some probe (or not work-bearing
	// at all), so none may be reported.
	cases := []struct {
		name    string
		mutate  func(*beads.Bead)
		because string
	}{
		{
			name:    "claimed",
			mutate:  func(b *beads.Bead) { b.Status = "in_progress" },
			because: "an in-progress bead is held by a live claim",
		},
		{
			name:    "assigned",
			mutate:  func(b *beads.Bead) { b.Assignee = "gascity/gastown.refinery" },
			because: "the refinery find-work query discovers it by assignee",
		},
		{
			name: "routed to a pool",
			mutate: func(b *beads.Bead) {
				b.Metadata[beadmeta.RoutedToMetadataKey] = "gascity/gastown.polecat"
			},
			because: "the pool demand probe discovers it by route",
		},
		{
			name: "carrying a legacy run target",
			mutate: func(b *beads.Bead) {
				b.Metadata[beadmeta.RunTargetMetadataKey] = "gascity/gastown.polecat"
			},
			because: "route recovery restores gc.routed_to from the carried route",
		},
		{
			name:    "no work branch recorded",
			mutate:  func(b *beads.Bead) { delete(b.Metadata, beadmeta.BranchMetadataKey) },
			because: "a bead with no branch holds no committed work to strand",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := storeWith(t, strandedBead(tc.mutate))

			got, err := Scan(testStoreRef, store, aheadProber())
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("Scan findings = %+v, want none: %s", got, tc.because)
			}
		})
	}
}

func TestScanSkipsBranchesHoldingNoUnmergedWork(t *testing.T) {
	// The fifth part of the signature is about the branch itself, so it is
	// negated through the repository rather than the bead's metadata.
	cases := []struct {
		name    string
		state   BranchState
		because string
	}{
		{
			name:    "fully merged into its target",
			state:   BranchState{LocalExists: true, CommitsAhead: 0},
			because: "merged work is already in the target; nothing is stranded",
		},
		{
			name:    "absent from local refs",
			state:   BranchState{LocalExists: false, CommitsAhead: 3},
			because: "a branch the repository does not have holds no local commits",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := storeWith(t, strandedBead())
			prober := &fakeProber{states: map[string]BranchState{"polecat/gascity-cgh": tc.state}}

			got, err := Scan(testStoreRef, store, prober)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("Scan findings = %+v, want none: %s", got, tc.because)
			}
		})
	}
}

func TestScanMeasuresAgainstRepositoryDefaultWhenTargetUnset(t *testing.T) {
	// The submit step is what writes `target`, so a bead stranded before submit
	// has none — gascity-cgh's did not. The repository's default branch stands in,
	// and the finding reports the target the count was actually measured against.
	store := storeWith(t, strandedBead(func(b *beads.Bead) {
		delete(b.Metadata, beadmeta.TargetMetadataKey)
	}))
	prober := aheadProber()

	got, err := Scan(testStoreRef, store, prober)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Scan findings = %+v, want exactly one", got)
	}
	if got[0].Target != testDefaultTarget {
		t.Errorf("finding target = %q, want the repository default %q", got[0].Target, testDefaultTarget)
	}
	if len(prober.asked) != 1 || prober.asked[0] != "polecat/gascity-cgh->" {
		t.Errorf("prober asked %v, want an empty target so the repository resolves its own default", prober.asked)
	}
}

func TestScanReportsProbeFailureWithoutFabricatingAFinding(t *testing.T) {
	// A finding's commit count and origin flag are load-bearing — the mayor
	// decides what to do with the branch from them. An unreadable repository
	// must surface as an error, never as a finding with zeroed fields.
	store := storeWith(t, strandedBead())
	prober := &fakeProber{err: errors.New("not a git repository")}

	got, err := Scan(testStoreRef, store, prober)
	if len(got) != 0 {
		t.Fatalf("Scan findings = %+v, want none when the repository cannot be probed", got)
	}
	if err == nil {
		t.Fatal("Scan error = nil, want the probe failure surfaced")
	}
	if !strings.Contains(err.Error(), "gascity-cgh") {
		t.Errorf("Scan error = %v, want the failing bead named", err)
	}
}

func TestScanOrdersFindingsByBeadID(t *testing.T) {
	// Findings drive one event each; a stable order keeps the emitted stream
	// (and any diff of it) from churning on map iteration order.
	store := storeWith(
		t,
		strandedBead(func(b *beads.Bead) {
			b.ID = "gascity-rwu"
			b.Metadata[beadmeta.BranchMetadataKey] = "polecat/gascity-rwu"
		}),
		strandedBead(),
		strandedBead(func(b *beads.Bead) {
			b.ID = "gascity-3vr"
			b.Metadata[beadmeta.BranchMetadataKey] = "polecat/gascity-3vr"
		}),
	)
	prober := &fakeProber{states: map[string]BranchState{
		"polecat/gascity-rwu": {LocalExists: true, CommitsAhead: 3},
		"polecat/gascity-cgh": {LocalExists: true, CommitsAhead: 1},
		"polecat/gascity-3vr": {LocalExists: true, CommitsAhead: 2},
	}}

	got, err := Scan(testStoreRef, store, prober)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var ids []string
	for _, f := range got {
		ids = append(ids, f.BeadID)
	}
	want := []string{"gascity-3vr", "gascity-cgh", "gascity-rwu"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("finding order = %v, want %v", ids, want)
	}
}

func TestScanWithoutStoreOrProberIsANoOp(t *testing.T) {
	got, err := Scan(testStoreRef, nil, aheadProber())
	if got != nil || err != nil {
		t.Errorf("Scan(nil store) = (%+v, %v), want (nil, nil)", got, err)
	}
	got, err = Scan(testStoreRef, storeWith(t, strandedBead()), nil)
	if got != nil || err != nil {
		t.Errorf("Scan(nil prober) = (%+v, %v), want (nil, nil)", got, err)
	}
}

func TestCandidateRejectsNonOpenStatusIndependentOfStoreFiltering(t *testing.T) {
	// Scan asks the store for open beads, but the guarantee is the signature's,
	// not the query's: a store that filters loosely must not turn a claimed bead
	// into a finding.
	for _, status := range []string{"in_progress", "closed", ""} {
		b := strandedBead(func(b *beads.Bead) { b.Status = status })
		if _, _, ok := Candidate(b); ok {
			t.Errorf("Candidate(status=%q) reported a candidate, want none", status)
		}
	}
}

func TestScanSurfacesAnUnreadableStore(t *testing.T) {
	// A store that cannot be listed proves nothing about whether work is
	// stranded in it; reporting "no findings" would read as an all-clear.
	store := listStore{err: errors.New("dolt: connection refused")}

	got, err := Scan(testStoreRef, store, aheadProber())
	if got != nil {
		t.Errorf("Scan findings = %+v, want none", got)
	}
	if err == nil || !strings.Contains(err.Error(), testStoreRef) {
		t.Errorf("Scan error = %v, want the unreadable store named", err)
	}
}
