package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// releasableWorkBead returns a MemStore holding one in_progress work bead
// assigned to assignee, plus the bead snapshot a release caller would hold.
func releasableWorkBead(t *testing.T, assignee string) (*beads.MemStore, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{Title: "work", Type: "task", Assignee: assignee})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set work in_progress: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work bead: %v", err)
	}
	return store, work
}

// failingReleaser is a store whose conditional release always errors. It
// embeds beads.Store so only ReleaseIfCurrent is exercised; any other method
// call is a test bug and panics on the nil interface.
type failingReleaser struct {
	beads.Store
	err error
}

func (f failingReleaser) ReleaseIfCurrent(string, string) (bool, error) { return false, f.err }

// soleBeadReleasedPayload asserts rec captured exactly one bead.released event
// for beadID and returns its decoded payload.
func soleBeadReleasedPayload(t *testing.T, rec *capturingRecorder, beadID string) events.BeadReleasedPayload {
	t.Helper()
	if len(rec.events) != 1 {
		t.Fatalf("event count = %d, want exactly 1; events=%+v", len(rec.events), rec.events)
	}
	e := rec.events[0]
	if e.Type != events.BeadReleased {
		t.Fatalf("type = %q, want %q", e.Type, events.BeadReleased)
	}
	if e.Subject != beadID {
		t.Fatalf("subject = %q, want %q", e.Subject, beadID)
	}
	var p events.BeadReleasedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return p
}

// A reconciler release that actually reverses the assignment emits exactly one
// bead.released event carrying the bead, the assignee it was taken from, and a
// caller identity naming the reconciler. Before this event the reversal was
// invisible to every audit surface (gascity-nk10).
func TestReleasePoolAssignmentIfCurrent_EmitsBeadReleasedNamingTheReconciler(t *testing.T) {
	store, work := releasableWorkBead(t, "worker-mc-dead")
	rec := &capturingRecorder{}

	released, handled := releasePoolAssignmentIfCurrent(store, work, rec)

	if !handled || !released {
		t.Fatalf("releasePoolAssignmentIfCurrent = (released=%v, handled=%v), want (true, true)", released, handled)
	}
	p := soleBeadReleasedPayload(t, rec, work.ID)
	if p.BeadID != work.ID || p.PriorAssignee != "worker-mc-dead" || p.ReleasedBy != beadReleaseReconcilerInitiator {
		t.Fatalf("payload = %+v, want bead_id=%s prior_assignee=worker-mc-dead released_by=%s",
			p, work.ID, beadReleaseReconcilerInitiator)
	}
}

// The CLI verb emits the same event type with a DIFFERENT caller identity: the
// incident requirement is that the two origins are distinguishable from the
// event alone, with no archeology.
func TestReleaseIfCurrentOnStore_EmitsBeadReleasedNamingTheCLIVerb(t *testing.T) {
	store, work := releasableWorkBead(t, "worker-mc-dead")
	rec := &capturingRecorder{}
	var stdout, stderr bytes.Buffer

	if code := releaseIfCurrentOnStore(store, rec, work.ID, "worker-mc-dead", &stdout, &stderr); code != 0 {
		t.Fatalf("releaseIfCurrentOnStore = %d, want 0; stderr=%q", code, stderr.String())
	}
	p := soleBeadReleasedPayload(t, rec, work.ID)
	if p.BeadID != work.ID || p.PriorAssignee != "worker-mc-dead" || p.ReleasedBy != beadReleaseCLIInitiator {
		t.Fatalf("payload = %+v, want bead_id=%s prior_assignee=worker-mc-dead released_by=%s",
			p, work.ID, beadReleaseCLIInitiator)
	}
	if beadReleaseCLIInitiator == beadReleaseReconcilerInitiator {
		t.Fatalf("CLI and reconciler identities are identical (%q); the two origins must be distinguishable",
			beadReleaseCLIInitiator)
	}
}

// A no-op release — the live row's assignee no longer matches the caller's
// expectation, so ReleaseIfCurrent returns released=false — emits ZERO events.
// The reconciler polls constantly; emitting here would bury the signal.
func TestReleasePoolAssignmentIfCurrent_NoOpReleaseEmitsNoEvent(t *testing.T) {
	store, work := releasableWorkBead(t, "worker-live")
	stale := work
	stale.Assignee = "worker-stale"
	rec := &capturingRecorder{}

	released, handled := releasePoolAssignmentIfCurrent(store, stale, rec)

	if !handled || released {
		t.Fatalf("releasePoolAssignmentIfCurrent = (released=%v, handled=%v), want (false, true)", released, handled)
	}
	if len(rec.events) != 0 {
		t.Fatalf("event count = %d, want 0 on a no-op release; events=%+v", len(rec.events), rec.events)
	}
}

// Same no-op contract on the CLI verb: "skipped" is not a release.
func TestReleaseIfCurrentOnStore_NoOpReleaseEmitsNoEvent(t *testing.T) {
	store, work := releasableWorkBead(t, "worker-live")
	rec := &capturingRecorder{}
	var stdout, stderr bytes.Buffer

	if code := releaseIfCurrentOnStore(store, rec, work.ID, "worker-stale", &stdout, &stderr); code != 0 {
		t.Fatalf("releaseIfCurrentOnStore = %d, want 0; stderr=%q", code, stderr.String())
	}
	if len(rec.events) != 0 {
		t.Fatalf("event count = %d, want 0 on a no-op release; events=%+v", len(rec.events), rec.events)
	}
}

// A release that fails emits ZERO events: nothing was reversed, so there is
// nothing to audit.
func TestReleasePoolAssignmentIfCurrent_ReleaseErrorEmitsNoEvent(t *testing.T) {
	_, work := releasableWorkBead(t, "worker-mc-dead")
	rec := &capturingRecorder{}

	released, handled := releasePoolAssignmentIfCurrent(
		failingReleaser{err: errors.New("backend down")}, work, rec)

	if !handled || released {
		t.Fatalf("releasePoolAssignmentIfCurrent = (released=%v, handled=%v), want (false, true)", released, handled)
	}
	if len(rec.events) != 0 {
		t.Fatalf("event count = %d, want 0 on a failed release; events=%+v", len(rec.events), rec.events)
	}
}

// Same on the CLI verb: a failed release exits non-zero and audits nothing.
func TestReleaseIfCurrentOnStore_ReleaseErrorEmitsNoEvent(t *testing.T) {
	rec := &capturingRecorder{}
	var stdout, stderr bytes.Buffer

	code := releaseIfCurrentOnStore(
		failingReleaser{err: errors.New("backend down")}, rec, "gc-abc", "worker-mc-dead", &stdout, &stderr)

	if code == 0 {
		t.Fatalf("releaseIfCurrentOnStore = 0 on a failed release, want non-zero; stdout=%q", stdout.String())
	}
	if len(rec.events) != 0 {
		t.Fatalf("event count = %d, want 0 on a failed release; events=%+v", len(rec.events), rec.events)
	}
}
