package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// paneObserveExecutor answers tmux calls by subcommand rather than by call
// order, so a test does not have to model how many round-trips ObserveInput
// makes on the way to the capture.
type paneObserveExecutor struct {
	calls [][]string
	// panes is the raw list-panes payload: "<pane_id>\t<command>\t<pid>"
	// rows, newline separated. A single row means a single-pane session, which
	// FindAgentPane deliberately leaves unresolved.
	panes       string
	env         map[string]string
	capture     string
	captureErr  error
	panesErr    error
	captureArgs []string
}

func (f *paneObserveExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	f.calls = append(f.calls, cp)
	switch tmuxSubcommand(cp) {
	case "list-panes":
		if f.panesErr != nil {
			return "", f.panesErr
		}
		return f.panes, nil
	case "show-environment":
		key := cp[len(cp)-1]
		val, ok := f.env[key]
		if !ok {
			return "", errors.New("unknown variable: " + key)
		}
		return key + "=" + val, nil
	case "capture-pane":
		f.captureArgs = cp
		if f.captureErr != nil {
			return "", f.captureErr
		}
		return f.capture, nil
	}
	return "", nil
}

func (f *paneObserveExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

// tmuxSubcommand returns the command verb of a recorded tmux argv, skipping
// the global flags run() injects ahead of it (-u, and -L <socket> when the
// city uses an isolated server).
func tmuxSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-u", "-N":
			continue
		case "-L", "-S", "-f":
			i++
			continue
		}
		return args[i]
	}
	return ""
}

// livePaneCapture is a witness pane as captured on 2026-09-14 with
// `tmux -L gc capture-pane -p -e`: a transcript, the box rule, the faint queue
// affordance on the input line, and the status footer.
const livePaneCapture = "\x1b[39m⏺ Successor gascity-wisp-3vo poured and assigned.\x1b[0m\n" +
	"  \x1b[38;5;239m\x1b[48;5;237m❯ \x1b[38;5;231mRun 'gc prime' to check worker status and begin patrol cycle.\x1b[39m    \x1b[49m\n" +
	"\x1b[38;5;244m────────\x1b[39m\n" +
	"\x1b[38;5;246m❯ \x1b[2m\x1b[39mPress up to edit queued messages\x1b[0m\n" +
	"\x1b[38;5;244m────────\x1b[39m\n" +
	"  \x1b[38;5;211m⏵⏵ bypass permissions on\x1b[38;5;246m (shift+tab to cycle) · ← for agents\x1b[39m"

// parkedPaneCapture is the same pane with the delivered prompt left sitting on
// the input line at normal intensity — a submit that never landed.
const parkedPaneCapture = "\x1b[39m⏺ Successor gascity-wisp-3vo poured and assigned.\x1b[0m\n" +
	"\x1b[38;5;244m────────\x1b[39m\n" +
	"\x1b[38;5;246m❯ \x1b[38;5;231mRun 'gc prime' to check worker status and begin patrol cycle.\x1b[0m\n" +
	"\x1b[38;5;244m────────\x1b[39m\n" +
	"  \x1b[38;5;211m⏵⏵ bypass permissions on\x1b[38;5;246m (shift+tab to cycle) · ← for agents\x1b[39m"

func newObservingTmux(fe *paneObserveExecutor) *Tmux {
	tm := NewTmux()
	tm.exec = fe
	return tm
}

// singlePaneExecutor is a one-pane session (the ordinary agent session shape)
// rendering the given capture at Claude's prompt glyph.
func singlePaneExecutor(capture string) *paneObserveExecutor {
	return &paneObserveExecutor{
		panes:   "%1\tclaude\t4242",
		env:     map[string]string{sessionReadyPromptEnvKey: DefaultReadyPromptPrefix},
		capture: capture,
	}
}

// TestObserveInputCapturesWithEscapes pins the flag the whole classification
// rests on: without `-e` tmux strips the styling, and a faint placeholder
// arrives at the reader as bare prompt text — the confusion that produced
// gascity-jw44.
func TestObserveInputCapturesWithEscapes(t *testing.T) {
	fe := singlePaneExecutor(livePaneCapture)

	if _, err := newObservingTmux(fe).ObserveInput("witness"); err != nil {
		t.Fatalf("ObserveInput: %v", err)
	}

	if fe.captureArgs == nil {
		t.Fatal("no capture-pane call recorded")
	}
	joined := strings.Join(fe.captureArgs, " ")
	for _, want := range []string{"-p", "-e"} {
		found := false
		for _, a := range fe.captureArgs {
			if a == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("capture-pane call %q is missing %q", joined, want)
		}
	}
	if !strings.Contains(joined, "witness") {
		t.Errorf("capture-pane call %q did not target the session", joined)
	}
}

// TestObserveInputOnAHealthyPane runs the live witness capture end to end.
func TestObserveInputOnAHealthyPane(t *testing.T) {
	fe := singlePaneExecutor(livePaneCapture)

	obs, err := newObservingTmux(fe).ObserveInput("witness")
	if err != nil {
		t.Fatalf("ObserveInput: %v", err)
	}
	if obs.State != runtime.InputStateIdle {
		t.Fatalf("state = %q, want %q (held %q)", obs.State, runtime.InputStateIdle, obs.HeldText)
	}
}

// TestObserveInputOnAParkedPane runs the failure this bead is about end to end.
func TestObserveInputOnAParkedPane(t *testing.T) {
	fe := singlePaneExecutor(parkedPaneCapture)

	obs, err := newObservingTmux(fe).ObserveInput("witness")
	if err != nil {
		t.Fatalf("ObserveInput: %v", err)
	}
	if obs.State != runtime.InputStateHoldingText {
		t.Fatalf("state = %q, want %q", obs.State, runtime.InputStateHoldingText)
	}
	if obs.HeldText != "Run 'gc prime' to check worker status and begin patrol cycle." {
		t.Errorf("held text = %q, want the delivered prompt", obs.HeldText)
	}
}

// TestObserveInputHonorsTheConfiguredPromptPrefix keeps the observation working
// for a provider whose input glyph is not Claude's.
func TestObserveInputHonorsTheConfiguredPromptPrefix(t *testing.T) {
	fe := &paneObserveExecutor{
		panes:   "%1\tcodex\t4242",
		env:     map[string]string{sessionReadyPromptEnvKey: "> "},
		capture: "\x1b[39m> \x1b[38;5;231mdrafted but never sent\x1b[0m",
	}

	obs, err := newObservingTmux(fe).ObserveInput("other-provider")
	if err != nil {
		t.Fatalf("ObserveInput: %v", err)
	}
	if obs.State != runtime.InputStateHoldingText {
		t.Fatalf("state = %q, want %q", obs.State, runtime.InputStateHoldingText)
	}
	if obs.HeldText != "drafted but never sent" {
		t.Errorf("held text = %q, want the drafted line", obs.HeldText)
	}
}

// TestObserveInputCaptureFailureIsUnknown keeps a capture failure from reading
// as a healthy pane.
func TestObserveInputCaptureFailureIsUnknown(t *testing.T) {
	sentinel := errors.New("no such pane")
	fe := singlePaneExecutor("")
	fe.captureErr = sentinel

	obs, err := newObservingTmux(fe).ObserveInput("witness")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the capture error", err)
	}
	if obs.State != runtime.InputStateUnknown {
		t.Errorf("state = %q, want %q", obs.State, runtime.InputStateUnknown)
	}
}

// TestObserveInputTargetsTheResolvedAgentPane keeps a multi-pane session from
// being classified on whichever pane happens to be focused: the observation has
// to read the pane the agent actually runs in, the same one delivery types into.
func TestObserveInputTargetsTheResolvedAgentPane(t *testing.T) {
	fe := &paneObserveExecutor{
		panes:   "%9\tclaude\t4242\n%1\tbash\t4243",
		env:     map[string]string{sessionReadyPromptEnvKey: DefaultReadyPromptPrefix},
		capture: parkedPaneCapture,
	}

	obs, err := newObservingTmux(fe).ObserveInput("witness")
	if err != nil {
		t.Fatalf("ObserveInput: %v", err)
	}
	if obs.State != runtime.InputStateHoldingText {
		t.Fatalf("state = %q, want %q", obs.State, runtime.InputStateHoldingText)
	}
	if !strings.Contains(strings.Join(fe.captureArgs, " "), "%9") {
		t.Errorf("capture-pane call %q did not target the resolved agent pane", fe.captureArgs)
	}
}
