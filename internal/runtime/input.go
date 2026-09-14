package runtime

// InputState classifies what a session's input area is holding right now. It
// separates the two things a stalled agent and a working agent look identical
// on every other health surface: a pane that is processing a turn, and a pane
// sitting idle with text nobody submitted.
type InputState string

const (
	// InputStateUnknown means the input area could not be classified — no
	// prompt line was found, or the capture carried no styling so the
	// provider's own placeholder chrome cannot be told apart from text a
	// sender put there. Never treat it as either healthy or parked.
	InputStateUnknown InputState = "unknown"
	// InputStateIdle means the agent is at an empty prompt: nothing is
	// pending. A provider-drawn placeholder or suggestion still counts as
	// idle — it is chrome, not input.
	InputStateIdle InputState = "idle"
	// InputStateWorking means the pane shows an active processing indicator.
	// Text held alongside it is queued behind the running turn and will be
	// consumed when that turn ends, so working is not a finding.
	InputStateWorking InputState = "working"
	// InputStateHoldingText means the pane is idle AND still holding text
	// that was never submitted — a delivered prompt waiting on a keypress
	// that will never come. The agent reports healthy everywhere else while
	// doing no work at all.
	InputStateHoldingText InputState = "holding_text"
)

// InputObservation is a point-in-time classification of a session's input
// area, derived from live pane content rather than from recorded state.
type InputObservation struct {
	// State is the classification.
	State InputState
	// HeldText is the unsubmitted text sitting on the input line. Empty
	// unless State is InputStateHoldingText.
	HeldText string
}

// InputObserver is an optional extension for providers that can classify the
// named session's input area from live pane content. Providers that cannot
// observe rendered input simply do not implement it, and callers type-assert.
type InputObserver interface {
	ObserveInput(name string) (InputObservation, error)
}
