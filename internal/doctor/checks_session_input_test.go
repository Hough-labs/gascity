package doctor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// inputObservingProvider is a Fake that also classifies pane input, standing
// in for a runtime provider that implements runtime.InputObserver.
type inputObservingProvider struct {
	*runtime.Fake
	observations map[string]runtime.InputObservation
	errs         map[string]error
}

func newInputObservingProvider(t *testing.T, sessions ...string) *inputObservingProvider {
	t.Helper()
	f := runtime.NewFake()
	for _, s := range sessions {
		if err := f.Start(context.Background(), s, runtime.Config{}); err != nil {
			t.Fatalf("starting fake session %q: %v", s, err)
		}
	}
	return &inputObservingProvider{
		Fake:         f,
		observations: map[string]runtime.InputObservation{},
		errs:         map[string]error{},
	}
}

func (p *inputObservingProvider) ObserveInput(name string) (runtime.InputObservation, error) {
	if err := p.errs[name]; err != nil {
		return runtime.InputObservation{State: runtime.InputStateUnknown}, err
	}
	obs, ok := p.observations[name]
	if !ok {
		return runtime.InputObservation{State: runtime.InputStateIdle}, nil
	}
	return obs, nil
}

func (p *inputObservingProvider) sendKeyCalls() []runtime.Call {
	var out []runtime.Call
	for _, c := range p.Calls {
		if c.Method == "SendKeys" {
			out = append(out, c)
		}
	}
	return out
}

func TestSessionInputCheckReportsParkedSession(t *testing.T) {
	sp := newInputObservingProvider(t, "witness", "refinery")
	sp.observations["witness"] = runtime.InputObservation{
		State:    runtime.InputStateHoldingText,
		HeldText: "Run 'gc prime' to check worker status and begin patrol cycle.",
	}
	sp.observations["refinery"] = runtime.InputObservation{State: runtime.InputStateIdle}

	r := NewSessionInputCheck(sp).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning (message %q)", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %v, want SeverityAdvisory", r.Severity)
	}
	if !strings.Contains(r.Message, "1 session(s)") {
		t.Errorf("message = %q, want it to count one session", r.Message)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "witness") {
		t.Fatalf("details = %q, want one witness finding", r.Details)
	}
	if !strings.Contains(r.Details[0], "gc prime") {
		t.Errorf("details = %q, want the held text quoted", r.Details[0])
	}
}

func TestSessionInputCheckIgnoresWorkingAndIdleSessions(t *testing.T) {
	sp := newInputObservingProvider(t, "busy-with-queue", "idle-with-placeholder")
	// A queued message behind a running turn is pending, not stalled.
	sp.observations["busy-with-queue"] = runtime.InputObservation{State: runtime.InputStateWorking}
	sp.observations["idle-with-placeholder"] = runtime.InputObservation{State: runtime.InputStateIdle}

	r := NewSessionInputCheck(sp).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK (message %q, details %q)", r.Status, r.Message, r.Details)
	}
}

func TestSessionInputCheckReportsUnreadablePanesWithoutFlaggingThem(t *testing.T) {
	sp := newInputObservingProvider(t, "unreadable")
	sp.errs["unreadable"] = fmt.Errorf("capture-pane: no such pane")

	r := NewSessionInputCheck(sp).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK: an unreadable pane is not a finding", r.Status)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "not readable") {
		t.Errorf("details = %q, want the unreadable pane noted", r.Details)
	}
}

func TestSessionInputCheckUnknownStateIsNotAFinding(t *testing.T) {
	// An unstyled capture cannot tell placeholder chrome from a parked
	// prompt. Reporting it would recreate the misdiagnosis this check exists
	// to prevent.
	sp := newInputObservingProvider(t, "unstyled")
	sp.observations["unstyled"] = runtime.InputObservation{State: runtime.InputStateUnknown}

	r := NewSessionInputCheck(sp).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK for an unknown classification", r.Status)
	}
}

func TestSessionInputCheckWithoutObserverSupportIsANoOp(t *testing.T) {
	r := NewSessionInputCheck(runtime.NewFake()).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK", r.Status)
	}
	if !strings.Contains(r.Message, "does not report pane input") {
		t.Errorf("message = %q, want it to name the missing capability", r.Message)
	}
}

// listFailingInputProvider fails ListRunning outright, standing in for a
// runtime whose session listing is unavailable.
type listFailingInputProvider struct {
	*inputObservingProvider
	listErr error
}

func (p *listFailingInputProvider) ListRunning(string) ([]string, error) {
	return nil, p.listErr
}

func TestSessionInputCheckListErrorIsAnError(t *testing.T) {
	sp := &listFailingInputProvider{
		inputObservingProvider: newInputObservingProvider(t, "witness"),
		listErr:                fmt.Errorf("no server running"),
	}

	r := NewSessionInputCheck(sp).Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %v, want StatusError when sessions cannot be listed", r.Status)
	}
	if !strings.Contains(r.Message, "no server running") {
		t.Errorf("message = %q, want it to carry the listing error", r.Message)
	}
}

// partialListInputProvider reports the sessions it can see alongside a
// PartialListError, the way a degraded runtime does.
type partialListInputProvider struct {
	*inputObservingProvider
	listErr error
}

func (p *partialListInputProvider) ListRunning(prefix string) ([]string, error) {
	names, _ := p.inputObservingProvider.ListRunning(prefix)
	return names, p.listErr
}

func TestSessionInputCheckPartialListStillReportsWhatItSaw(t *testing.T) {
	inner := newInputObservingProvider(t, "witness")
	inner.observations["witness"] = runtime.InputObservation{
		State:    runtime.InputStateHoldingText,
		HeldText: "close the warrant and fix the dog routing",
	}
	sp := &partialListInputProvider{
		inputObservingProvider: inner,
		listErr:                &runtime.PartialListError{Err: fmt.Errorf("one backend unreachable")},
	}

	r := NewSessionInputCheck(sp).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning", r.Status)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "witness") {
		t.Errorf("details = %q, want the visible parked session still reported", r.Details)
	}
}

func TestSessionInputCheckFixSubmitsOnlyParkedPanes(t *testing.T) {
	sp := newInputObservingProvider(t, "parked", "working", "idle")
	sp.observations["parked"] = runtime.InputObservation{
		State:    runtime.InputStateHoldingText,
		HeldText: "wait for work and check the queue again",
	}
	sp.observations["working"] = runtime.InputObservation{State: runtime.InputStateWorking}
	sp.observations["idle"] = runtime.InputObservation{State: runtime.InputStateIdle}

	check := NewSessionInputCheck(sp)
	if !check.CanFix() {
		t.Fatal("CanFix = false, want true")
	}
	if err := check.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	calls := sp.sendKeyCalls()
	if len(calls) != 1 {
		t.Fatalf("SendKeys calls = %+v, want exactly one", calls)
	}
	if calls[0].Name != "parked" {
		t.Errorf("SendKeys targeted %q, want the parked session", calls[0].Name)
	}
	if calls[0].Message != "Enter" {
		t.Errorf("SendKeys sent %q, want a bare Enter (Fix must add no text)", calls[0].Message)
	}
}

func TestSessionInputCheckFixReObservesBeforeSubmitting(t *testing.T) {
	// A pane that started working between Run and Fix must be left alone:
	// Enter into a running turn is input the agent never asked for.
	sp := newInputObservingProvider(t, "recovered")
	sp.observations["recovered"] = runtime.InputObservation{State: runtime.InputStateWorking}

	if err := NewSessionInputCheck(sp).Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if calls := sp.sendKeyCalls(); len(calls) != 0 {
		t.Errorf("SendKeys calls = %+v, want none", calls)
	}
}

func TestSessionInputCheckWarmupNotEligible(t *testing.T) {
	if NewSessionInputCheck(runtime.NewFake()).WarmupEligible() {
		t.Error("WarmupEligible = true, want false")
	}
}
