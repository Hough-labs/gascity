package tmux

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// observeQueue returns an observe stub that yields the given observations in
// order, repeating the last one once exhausted, and a pointer to the call
// count.
func observeQueue(states ...runtime.InputState) (func() (runtime.InputObservation, error), *int) {
	calls := 0
	return func() (runtime.InputObservation, error) {
		i := calls
		calls++
		if i >= len(states) {
			i = len(states) - 1
		}
		obs := runtime.InputObservation{State: states[i]}
		if states[i] == runtime.InputStateHoldingText {
			obs.HeldText = "wait for work and check the queue again"
		}
		return obs, nil
	}, &calls
}

// TestResubmitParkedMessageSubmitsAParkedDraft is the case the discarded
// confirmation bool used to hide: busy was never observed AND the pane is still
// holding the text, so the submit really was lost. One Enter clears it.
func TestResubmitParkedMessageSubmitsAParkedDraft(t *testing.T) {
	observe, observeCalls := observeQueue(runtime.InputStateHoldingText, runtime.InputStateWorking)
	enters := 0
	wakes := 0

	submitted, err := resubmitParkedMessage(
		observe,
		func() error { enters++; return nil },
		func() { wakes++ },
		noSleep,
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !submitted {
		t.Fatal("submitted = false, want true (the remedial Enter cleared the pane)")
	}
	if enters != 1 {
		t.Errorf("enters = %d, want exactly 1 remedial Enter", enters)
	}
	if wakes != 1 {
		t.Errorf("wakes = %d, want 1", wakes)
	}
	if *observeCalls < 2 {
		t.Errorf("observe calls = %d, want the pane re-read after the Enter", *observeCalls)
	}
}

// TestResubmitParkedMessageLeavesAFastTurnAlone proves the other half of the
// ambiguity: an unconfirmed submit whose pane is no longer holding the text did
// land (a turn that finished inside the confirm budget). Sending Enter there
// would be input the agent never asked for.
func TestResubmitParkedMessageLeavesAFastTurnAlone(t *testing.T) {
	for _, state := range []runtime.InputState{
		runtime.InputStateIdle,
		runtime.InputStateWorking,
		runtime.InputStateUnknown,
	} {
		t.Run(string(state), func(t *testing.T) {
			observe, _ := observeQueue(state)
			enters := 0

			submitted, err := resubmitParkedMessage(observe, func() error { enters++; return nil }, func() {}, noSleep)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if !submitted {
				t.Errorf("submitted = false, want true for state %q", state)
			}
			if enters != 0 {
				t.Errorf("enters = %d, want 0: only a held message may be re-submitted", enters)
			}
		})
	}
}

// TestResubmitParkedMessageUnreadablePaneIsNotAFinding keeps a capture blip
// from being reported as a stalled agent.
func TestResubmitParkedMessageUnreadablePaneIsNotAFinding(t *testing.T) {
	enters := 0
	observe := func() (runtime.InputObservation, error) {
		return runtime.InputObservation{State: runtime.InputStateUnknown}, errors.New("capture-pane: no such pane")
	}

	submitted, err := resubmitParkedMessage(observe, func() error { enters++; return nil }, func() {}, noSleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !submitted {
		t.Error("submitted = false, want true: an unreadable pane is not evidence of a parked message")
	}
	if enters != 0 {
		t.Errorf("enters = %d, want 0", enters)
	}
}

// TestResubmitParkedMessageReportsAStillParkedMessage is the state that used to
// be invisible everywhere: after the remedial Enter the text is STILL sitting in
// the pane. The caller must learn that.
func TestResubmitParkedMessageReportsAStillParkedMessage(t *testing.T) {
	observe, _ := observeQueue(runtime.InputStateHoldingText)
	enters := 0

	submitted, err := resubmitParkedMessage(observe, func() error { enters++; return nil }, func() {}, noSleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if submitted {
		t.Error("submitted = true, want false: the pane never let go of the text")
	}
	if enters != 1 {
		t.Errorf("enters = %d, want exactly 1: retrying Enter forever is not a remedy", enters)
	}
}

// TestResubmitParkedMessageSurfacesASendFailure keeps a tmux-level failure from
// being read as a successful re-submit.
func TestResubmitParkedMessageSurfacesASendFailure(t *testing.T) {
	observe, _ := observeQueue(runtime.InputStateHoldingText)
	sendErr := errors.New("no server running")

	submitted, err := resubmitParkedMessage(observe, func() error { return sendErr }, func() {}, noSleep)

	if !errors.Is(err, sendErr) {
		t.Fatalf("err = %v, want the send error", err)
	}
	if submitted {
		t.Error("submitted = true, want false when Enter could not be delivered")
	}
}
