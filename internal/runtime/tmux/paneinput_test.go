package tmux

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Fixtures captured from live panes with `tmux -L gc capture-pane -p -e`
// (gascity rig, 2026-09-14). They are the exact bytes the misdiagnosis in
// gascity-jw44 turned on: strip the styling and the placeholder row and the
// queued-message row are the same text.
const (
	// A faint suggestion the provider drew on the input line. Nobody sent it.
	fixturePlaceholderSuggestion = "\x1b[39m❯ \x1b[2mprune those stale branches\x1b[0m"
	// The faint affordance Claude Code draws while messages sit queued.
	fixturePlaceholderQueueHint = "\x1b[38;5;246m❯ \x1b[2m\x1b[39mPress up to edit queued messages\x1b[0m"
	// A user turn the agent already consumed, echoed in the transcript on its
	// own prompt row. Byte-identical styling to a message still sitting in the
	// provider's queued box, which is why only the input line is classified.
	fixtureTranscriptEcho = "  \x1b[38;5;239m\x1b[48;5;237m❯ \x1b[38;5;231mRun 'gc prime' to check worker status and begin patrol cycle.\x1b[39m             \x1b[49m"
	// A menu cursor: the prompt glyph marking the highlighted option of an
	// interactive list, highlight drawn bright. Captured from a live mayor pane.
	fixtureMenuCursor = "\x1b[38;5;153m❯\x1b[39m \x1b[38;5;246m1.\x1b[39m \x1b[38;5;153mGate cold start on prior state (Recommended)\x1b[39m"
	// The footer that identifies that list.
	fixtureMenuFooter = "\x1b[38;5;246mEnter\x1b[39m \x1b[38;5;246mto\x1b[39m \x1b[38;5;246mselect\x1b[39m \x1b[38;5;246m·\x1b[39m \x1b[38;5;246m↑/↓\x1b[39m \x1b[38;5;246mto\x1b[39m \x1b[38;5;246mnavigate\x1b[39m"
	// The footer of an ordinary input box, which must NOT read as a menu.
	fixtureInputFooter = "  \x1b[38;5;211m⏵⏵ bypass permissions on\x1b[38;5;246m (shift+tab to cycle) · ← for agents\x1b[39m"
	// Claude Code's live spinner footer.
	fixtureBusySpinner = "✳ Kerfuffling… (9m 3s · ↓ 30.8k tokens · thought for 1s)"
	// The box border row the input area is drawn between.
	fixtureBoxRule = "\x1b[38;5;244m────────\x1b[39m"
)

func TestClassifyPaneInput(t *testing.T) {
	tests := []struct {
		name      string
		lines     []string
		wantState runtime.InputState
		wantHeld  string
	}{
		{
			name:      "faint suggestion on the input line is chrome, not input",
			lines:     []string{fixtureBoxRule, fixturePlaceholderSuggestion, fixtureBoxRule},
			wantState: runtime.InputStateIdle,
		},
		{
			name:      "faint queue affordance is chrome, not input",
			lines:     []string{fixtureBoxRule, fixturePlaceholderQueueHint, fixtureBoxRule},
			wantState: runtime.InputStateIdle,
		},
		{
			name:      "empty prompt is idle",
			lines:     []string{"\x1b[39m❯ \x1b[0m"},
			wantState: runtime.InputStateIdle,
		},
		{
			name:      "bright text on the input line is held",
			lines:     []string{fixtureBoxRule, "\x1b[39m❯ \x1b[39mwait for work and check the queue again\x1b[0m", fixtureBoxRule},
			wantState: runtime.InputStateHoldingText,
			wantHeld:  "wait for work and check the queue again",
		},
		{
			name:      "held text on a working pane is not a stall",
			lines:     []string{fixtureBusySpinner, fixtureBoxRule, "\x1b[39m❯ \x1b[39mhalf-typed line\x1b[0m", fixtureBoxRule},
			wantState: runtime.InputStateWorking,
		},
		{
			name:      "truecolor text is not mistaken for faint",
			lines:     []string{"\x1b[38;2;255;0;0m❯ \x1b[38;2;200;200;200mtruecolor draft\x1b[0m"},
			wantState: runtime.InputStateHoldingText,
			wantHeld:  "truecolor draft",
		},
		{
			name:      "256-color text is not mistaken for faint",
			lines:     []string{"\x1b[38;5;231m❯ \x1b[38;5;231m256 color draft\x1b[0m"},
			wantState: runtime.InputStateHoldingText,
			wantHeld:  "256 color draft",
		},
		{
			name:      "normal-intensity reset clears faint",
			lines:     []string{"\x1b[2m❯ \x1b[22mtyped after a faint span\x1b[0m"},
			wantState: runtime.InputStateHoldingText,
			wantHeld:  "typed after a faint span",
		},
		{
			name:      "unstyled prompt text is indeterminate, never a finding",
			lines:     []string{"❯ could be a draft or a suggestion"},
			wantState: runtime.InputStateUnknown,
		},
		{
			name:      "no prompt row at all is unknown",
			lines:     []string{"\x1b[39msome scrollback\x1b[0m", fixtureBoxRule},
			wantState: runtime.InputStateUnknown,
		},
		{
			name:      "prompt glyph inside a message body is not a prompt row",
			lines:     []string{"\x1b[39m⏺ I typed ❯ into the shell\x1b[0m", fixturePlaceholderQueueHint},
			wantState: runtime.InputStateIdle,
		},
		{
			name:      "box-bordered prompt row is classified",
			lines:     []string{"│ \x1b[39m❯ \x1b[39mgrok style draft\x1b[0m"},
			wantState: runtime.InputStateHoldingText,
			wantHeld:  "grok style draft",
		},
		{
			name:      "no-break space between glyph and text",
			lines:     []string{"\x1b[39m❯\u00a0\x1b[39mnbsp draft\x1b[0m"},
			wantState: runtime.InputStateHoldingText,
			wantHeld:  "nbsp draft",
		},
		{
			name:      "trailing padding on the input line is trimmed",
			lines:     []string{"\x1b[39m❯ \x1b[39mpadded draft         \x1b[0m"},
			wantState: runtime.InputStateHoldingText,
			wantHeld:  "padded draft",
		},
		{
			name:      "a menu cursor is not held text",
			lines:     []string{fixtureMenuCursor, "  2. Isolate the test load", fixtureMenuFooter},
			wantState: runtime.InputStateUnknown,
		},
		{
			name:      "the ordinary input-box footer does not read as a menu",
			lines:     []string{fixtureBoxRule, "\x1b[39m❯ \x1b[39mreal draft\x1b[0m", fixtureBoxRule, fixtureInputFooter},
			wantState: runtime.InputStateHoldingText,
			wantHeld:  "real draft",
		},
		{
			name:      "menu text above the input line does not suppress a finding",
			lines:     []string{"\x1b[39m⏺ use grep to select the lines and ↑/↓ to navigate them\x1b[0m", fixtureBoxRule, "\x1b[39m❯ \x1b[39mreal draft\x1b[0m", fixtureBoxRule},
			wantState: runtime.InputStateHoldingText,
			wantHeld:  "real draft",
		},
		{
			name:      "empty capture is unknown",
			lines:     nil,
			wantState: runtime.InputStateUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPaneInput(tc.lines, DefaultReadyPromptPrefix)
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q (held=%q)", got.State, tc.wantState, got.HeldText)
			}
			if got.HeldText != tc.wantHeld {
				t.Errorf("held text = %q, want %q", got.HeldText, tc.wantHeld)
			}
		})
	}
}

// TestClassifyPaneInputIgnoresTranscriptPromptRows is the false positive that
// would have made this check useless: every user turn an agent has ALREADY
// consumed is echoed in the transcript on its own prompt row, at normal
// intensity. Classifying those would report a finding against every healthy
// agent on every rig. Only the live input line — the last prompt row — counts.
func TestClassifyPaneInputIgnoresTranscriptPromptRows(t *testing.T) {
	lines := []string{
		fixtureTranscriptEcho,
		"\x1b[39m⏺ ran the patrol cycle\x1b[0m",
		fixtureTranscriptEcho,
		fixtureBoxRule,
		fixturePlaceholderQueueHint,
		fixtureBoxRule,
	}

	got := classifyPaneInput(lines, DefaultReadyPromptPrefix)

	if got.State != runtime.InputStateIdle {
		t.Fatalf("state = %q, want %q: consumed turns in the transcript are not held text", got.State, runtime.InputStateIdle)
	}
	if got.HeldText != "" {
		t.Errorf("held text = %q, want empty", got.HeldText)
	}
}

// TestClassifyPaneInputIgnoresAMenuCursor is the false positive the first live
// run of this classifier produced: a mayor pane sitting on an interactive
// option list was reported as holding unsubmitted text, because the prompt
// glyph there is the selection cursor and the highlighted option is drawn
// bright. A pane at an option list is waiting on a human answer by design.
func TestClassifyPaneInputIgnoresAMenuCursor(t *testing.T) {
	lines := []string{
		"\x1b[39m│ How do you want the tmux fleet-respawn bug fixed?\x1b[0m",
		fixtureMenuCursor,
		"  \x1b[38;5;246m2. Isolate the test load\x1b[39m",
		"  \x1b[38;5;246m5. Type something.\x1b[39m",
		fixtureMenuFooter,
	}

	got := classifyPaneInput(lines, DefaultReadyPromptPrefix)

	if got.State != runtime.InputStateUnknown {
		t.Fatalf("state = %q, want %q: a menu cursor is not an input prompt", got.State, runtime.InputStateUnknown)
	}
	if got.HeldText != "" {
		t.Errorf("held text = %q, want empty", got.HeldText)
	}
}

// TestClassifyPaneInputDistinguishesTheIdenticalPlainText is the regression
// this classifier exists for: the placeholder line and a parked prompt carry
// the SAME text once styling is stripped, so any classifier keyed on the text
// reports them identically — which is how healthy panes got a P1 filed against
// them and parked ones went unattributed (gascity-jw44).
func TestClassifyPaneInputDistinguishesTheIdenticalPlainText(t *testing.T) {
	const text = "Run 'gc prime' to check worker status and begin patrol cycle."

	placeholder := "\x1b[38;5;246m❯ \x1b[2m" + text + "\x1b[0m"
	parked := "\x1b[38;5;246m❯ \x1b[38;5;231m" + text + "\x1b[0m"

	if plain, _, _ := decodeStyledLine(placeholder); !strings.Contains(plain, text) {
		t.Fatalf("placeholder plain text = %q, want it to contain the shared text", plain)
	}
	if plain, _, _ := decodeStyledLine(parked); !strings.Contains(plain, text) {
		t.Fatalf("parked plain text = %q, want it to contain the shared text", plain)
	}

	if got := classifyPaneInput([]string{placeholder}, DefaultReadyPromptPrefix); got.State != runtime.InputStateIdle {
		t.Errorf("placeholder classified %q, want %q", got.State, runtime.InputStateIdle)
	}
	if got := classifyPaneInput([]string{parked}, DefaultReadyPromptPrefix); got.State != runtime.InputStateHoldingText {
		t.Errorf("parked classified %q, want %q", got.State, runtime.InputStateHoldingText)
	}
}

func TestDecodeStyledLine(t *testing.T) {
	tests := []struct {
		name       string
		line       string
		wantPlain  string
		wantStyled bool
		dimAt      map[int]bool
	}{
		{
			name:       "plain text carries no styling",
			line:       "hello",
			wantPlain:  "hello",
			wantStyled: false,
			dimAt:      map[int]bool{0: false, 4: false},
		},
		{
			name:       "faint span is marked per rune",
			line:       "a\x1b[2mb\x1b[0mc",
			wantPlain:  "abc",
			wantStyled: true,
			dimAt:      map[int]bool{0: false, 1: true, 2: false},
		},
		{
			name:       "bare ESC[m resets faint",
			line:       "\x1b[2ma\x1b[mb",
			wantPlain:  "ab",
			wantStyled: true,
			dimAt:      map[int]bool{0: true, 1: false},
		},
		{
			name:       "osc sequence is consumed whole",
			line:       "\x1b]0;title\x07after",
			wantPlain:  "after",
			wantStyled: false,
		},
		{
			name:       "osc terminated by string terminator is consumed whole",
			line:       "\x1b]0;title\x1b\\after",
			wantPlain:  "after",
			wantStyled: false,
		},
		{
			name:       "non-sgr csi does not set styled",
			line:       "\x1b[2Kcleared",
			wantPlain:  "cleared",
			wantStyled: false,
		},
		{
			name:       "truncated escape is dropped",
			line:       "text\x1b[",
			wantPlain:  "text",
			wantStyled: false,
		},
		{
			name:       "no-break space folds to a plain space",
			line:       "a\u00a0b",
			wantPlain:  "a b",
			wantStyled: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plain, dim, styled := decodeStyledLine(tc.line)
			if plain != tc.wantPlain {
				t.Errorf("plain = %q, want %q", plain, tc.wantPlain)
			}
			if styled != tc.wantStyled {
				t.Errorf("styled = %v, want %v", styled, tc.wantStyled)
			}
			if len(dim) != len([]rune(plain)) {
				t.Fatalf("dim has %d entries for %d runes", len(dim), len([]rune(plain)))
			}
			for idx, want := range tc.dimAt {
				if dim[idx] != want {
					t.Errorf("dim[%d] = %v, want %v", idx, dim[idx], want)
				}
			}
		})
	}
}

func TestApplySGRIntensity(t *testing.T) {
	tests := []struct {
		name   string
		faint  bool
		params []string
		want   bool
	}{
		{name: "empty params reset", faint: true, params: nil, want: false},
		{name: "explicit reset", faint: true, params: []string{"0"}, want: false},
		{name: "dim sets", faint: false, params: []string{"2"}, want: true},
		{name: "normal intensity clears", faint: true, params: []string{"22"}, want: false},
		{name: "bold does not clear dim", faint: true, params: []string{"1"}, want: true},
		{
			name:   "truecolor selector is not an attribute",
			faint:  false,
			params: []string{"38", "2", "255", "0", "0"},
			want:   false,
		},
		{
			name:   "truecolor after dim keeps dim",
			faint:  true,
			params: []string{"38", "2", "255", "0", "0"},
			want:   true,
		},
		{
			name:   "256-color selector is not an attribute",
			faint:  false,
			params: []string{"48", "5", "2"},
			want:   false,
		},
		{
			name:   "dim after a color span still sets",
			faint:  false,
			params: []string{"38", "5", "246", "2"},
			want:   true,
		},
		{
			name:   "truncated extended color does not panic",
			faint:  false,
			params: []string{"38"},
			want:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := applySGRIntensity(tc.faint, tc.params); got != tc.want {
				t.Errorf("applySGRIntensity(%v, %q) = %v, want %v", tc.faint, tc.params, got, tc.want)
			}
		})
	}
}

func TestPromptRowText(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantOK   bool
		wantText string
		wantDim  bool
	}{
		{name: "bare glyph", line: "❯ ", wantOK: true, wantText: ""},
		{name: "glyph with text", line: "❯ hello", wantOK: true, wantText: "hello"},
		{name: "indented glyph", line: "   ❯ hello", wantOK: true, wantText: "hello"},
		{name: "box border before glyph", line: "│ ❯ hello", wantOK: true, wantText: "hello"},
		{name: "glyph mid-line is not a prompt", line: "ran ❯ hello", wantOK: false},
		{name: "no glyph", line: "hello", wantOK: false},
		{name: "trailing padding trimmed", line: "❯ hello       ", wantOK: true, wantText: "hello"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plain, dim, _ := decodeStyledLine(tc.line)
			got, ok := promptRowText(plain, DefaultReadyPromptPrefix, dim)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.text != tc.wantText {
				t.Errorf("text = %q, want %q", got.text, tc.wantText)
			}
			if got.dim != tc.wantDim {
				t.Errorf("dim = %v, want %v", got.dim, tc.wantDim)
			}
		})
	}
}

func TestPromptRowTextEmptyPrefixIsNotAPrompt(t *testing.T) {
	plain, dim, _ := decodeStyledLine("❯ hello")
	if _, ok := promptRowText(plain, "", dim); ok {
		t.Error("an empty prompt prefix must not match any row")
	}
}
