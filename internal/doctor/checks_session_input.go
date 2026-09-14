package doctor

import (
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/runtime"
)

// parkedInputTextLimit bounds how much of a held prompt a finding quotes, so a
// pasted multi-kilobyte brief does not flood the report.
const parkedInputTextLimit = 120

// SessionInputCheck finds live sessions that are idle while still holding text
// nobody submitted — a prompt delivered to the pane and waiting on a keypress
// that will never come.
//
// It is the surface every other health signal lacked. A session in this state
// reports `active` in `gc session list`, carries a healthy session bead, emits
// no error, and advances its last-activity stamp when the text is written, so
// staleness heuristics keyed on activity never fire. The only way it was ever
// caught was an operator eyeballing a pane — and reading a pane by eye is what
// produced a P1 filed against healthy panes, because a plain capture renders
// the provider's own faint suggestion line identically to a parked prompt
// (gascity-jw44). The provider classifies the styling instead, so this check
// reports the distinction rather than the resemblance.
//
// Text held while the agent is mid-turn is queued behind that turn, not
// stalled, and is never a finding.
type SessionInputCheck struct {
	sp runtime.Provider
}

// NewSessionInputCheck creates a parked-input check over the given runtime
// provider. Providers that cannot classify rendered input make the check a
// no-op rather than a failure.
func NewSessionInputCheck(sp runtime.Provider) *SessionInputCheck {
	return &SessionInputCheck{sp: sp}
}

// Name returns the check identifier.
func (c *SessionInputCheck) Name() string { return "session-input-parked" }

// WarmupEligible returns false: the state this check hunts accrues while
// agents run, so it belongs on demand and on patrol, not in `gc start`.
func (c *SessionInputCheck) WarmupEligible() bool { return false }

// Run classifies the input area of every running session.
func (c *SessionInputCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	observer, ok := c.sp.(runtime.InputObserver)
	if !ok {
		r.Status = StatusOK
		r.Message = "runtime provider does not report pane input"
		return r
	}

	running, err := c.sp.ListRunning("")
	partial := runtime.IsPartialListError(err)
	if err != nil && !partial {
		r.Status = StatusError
		r.Message = fmt.Sprintf("listing sessions: %v", err)
		return r
	}

	parked, unreadable := classifySessions(observer, running)
	sort.Strings(parked)
	sort.Strings(unreadable)

	if len(parked) == 0 {
		r.Status = StatusOK
		switch {
		case partial:
			r.Status = StatusWarning
			r.Severity = SeverityAdvisory
			r.Message = fmt.Sprintf("listing sessions partially failed: %v", err)
		case len(unreadable) > 0:
			r.Message = fmt.Sprintf("no sessions holding unsubmitted text (%d unreadable)", len(unreadable))
			r.Details = unreadable
		default:
			r.Message = "no sessions holding unsubmitted text"
		}
		return r
	}

	r.Status = StatusWarning
	// Advisory: a parked pane stalls that one agent, and the remedy is a
	// keystroke. It must not gate dispatch to every other agent in the city.
	r.Severity = SeverityAdvisory
	if partial {
		r.Message = fmt.Sprintf("listing sessions partially failed: %v (%d visible session(s) idle holding unsubmitted text)", err, len(parked))
	} else {
		r.Message = fmt.Sprintf("%d session(s) idle holding unsubmitted text", len(parked))
	}
	details := make([]string, 0, len(parked)+len(unreadable))
	details = append(details, parked...)
	details = append(details, unreadable...)
	r.Details = details
	return r
}

// classifySessions splits running sessions into those idle holding
// unsubmitted text and those whose pane could not be read. A session the
// provider cannot classify is reported as unreadable, never as parked: this
// check exists because a confident wrong answer about a pane is worse than no
// answer.
func classifySessions(observer runtime.InputObserver, running []string) (parked, unreadable []string) {
	for _, name := range running {
		obs, err := observer.ObserveInput(name)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s: input not readable: %v", name, err))
			continue
		}
		if obs.State != runtime.InputStateHoldingText {
			continue
		}
		parked = append(parked, fmt.Sprintf("%s: holding %q", name, truncateHeldText(obs.HeldText)))
	}
	return parked, unreadable
}

// truncateHeldText bounds the quoted text to parkedInputTextLimit runes so a
// pasted multi-kilobyte brief does not flood the report.
func truncateHeldText(s string) string {
	if s == "" {
		return "unsubmitted text"
	}
	r := []rune(s)
	if len(r) <= parkedInputTextLimit {
		return s
	}
	return string(r[:parkedInputTextLimit]) + "…"
}

// CanFix returns true. The remedy is the keystroke the delivery dropped: one
// Enter on a pane that is idle and holding text submits what is already there.
// It adds no text, and an idle pane has no running turn to interrupt.
func (c *SessionInputCheck) CanFix() bool { return true }

// Fix submits the parked text on every session still holding it. The state is
// re-observed first so a pane that started working in the meantime is left
// alone.
func (c *SessionInputCheck) Fix(_ *CheckContext) error {
	observer, ok := c.sp.(runtime.InputObserver)
	if !ok {
		return nil
	}
	running, err := c.sp.ListRunning("")
	if runtime.IsPartialListError(err) {
		return fmt.Errorf("listing sessions partially failed: %w", err)
	}
	if err != nil {
		return err
	}
	for _, name := range running {
		obs, obsErr := observer.ObserveInput(name)
		if obsErr != nil || obs.State != runtime.InputStateHoldingText {
			continue
		}
		if err := c.sp.SendKeys(name, "Enter"); err != nil {
			return fmt.Errorf("submitting parked input for session %q: %w", name, err)
		}
	}
	return nil
}
