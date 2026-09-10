package main

import (
	"encoding/json"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// Caller identities stamped into bead.released events. The point of the event
// is that the next assignment-reversal incident can tell the reconciler from
// the CLI verb without archeology, so these are stable and greppable (same
// convention as orderTrackingWatchdogMetadataInitiator).
const (
	beadReleaseReconcilerInitiator = "pool-reconciler"
	beadReleaseCLIInitiator        = "gc-bd-release-if-current"
)

// emitBeadReleased records one bead.released event for an assignment reversal
// that actually happened: ReleaseIfCurrent moved the bead from in_progress to
// open and cleared priorAssignee. The store performs that reversal inside a
// transaction that writes no audit row, so this call site is the only place
// the reversal becomes observable, and releasedBy is the fact the store itself
// could never supply.
//
// Callers MUST invoke this only when ReleaseIfCurrent returned released ==
// true. The conditional release is a no-op whenever the live row no longer
// matches the expected assignee, and the reconciler polls constantly; emitting
// on those no-ops would bury the signal this event exists to create.
//
// A nil recorder is a no-op, so a call site with no event sink degrades to the
// silence that preceded this event rather than panicking.
func emitBeadReleased(rec events.Recorder, beadID, priorAssignee, releasedBy string, now time.Time) {
	if rec == nil || beadID == "" {
		return
	}
	payload, err := json.Marshal(events.BeadReleasedPayload{
		BeadID:        beadID,
		PriorAssignee: priorAssignee,
		ReleasedBy:    releasedBy,
	})
	if err != nil {
		return
	}
	rec.Record(events.Event{
		Type:    events.BeadReleased,
		Ts:      now.UTC(),
		Actor:   releasedBy,
		Subject: beadID,
		Message: formatBeadReleasedMessage(beadID, priorAssignee, releasedBy),
		Payload: payload,
	})
}

// formatBeadReleasedMessage renders the operator-facing text for a
// bead.released event.
func formatBeadReleasedMessage(beadID, priorAssignee, releasedBy string) string {
	assignee := priorAssignee
	if assignee == "" {
		assignee = "<unassigned>"
	}
	return "released " + beadID + " from " + assignee + " (by " + releasedBy +
		"); status in_progress->open, assignee cleared"
}
