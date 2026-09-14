package tmux

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Pane-input classification.
//
// A prompt line carrying text is the signature of BOTH a healthy idle agent
// and a stalled one, and an unstyled capture cannot tell them apart. Claude
// Code draws its own suggestion and affordance text on the input line faint
// (SGR 2) — "Press up to edit queued messages", or a suggested next prompt —
// while text a sender actually delivered is drawn at normal intensity. Strip
// the styling and the two are the same bytes, which is how a fleet-wide P1 came
// to be filed against healthy panes and how genuinely parked ones went
// unattributed for hours (gascity-jw44).
//
// Three properties of the capture bound what can honestly be claimed:
//
//   - Only the LIVE INPUT LINE is classified. A user turn that was submitted
//     long ago is echoed in the transcript on its own prompt row, and its
//     styling is byte-identical to a message still sitting in the provider's
//     queued box (both `ESC[38;5;239m ESC[48;5;237m❯ ESC[38;5;231m<text>`,
//     measured on live panes). Scanning the scrollback for held text would
//     therefore report every past turn of every healthy agent. The input line
//     is the last prompt row in the capture, and it is the only row whose
//     content gc did not already watch the agent consume.
//   - An unstyled row is INDETERMINATE, never a finding. Without the intensity
//     there is nothing to separate chrome from input, and a confident wrong
//     answer about a pane is what this classification exists to end.
//   - The prompt glyph is also the SELECTION CURSOR of an interactive option
//     list, where it marks the highlighted choice and the highlight is drawn
//     bright. A pane showing such a list is waiting on a human answer, not
//     holding a dropped prompt, so it is indeterminate too. Measured live: a
//     mayor pane at an option list rendered `ESC[38;5;153m❯ … Gate cold start
//     on prior state (Recommended)`, which is a menu cursor and would
//     otherwise have been reported as unsubmitted text.
//
// So classification keys on the rendered intensity of the input line, never on
// the strings themselves: role names and suggestion wording are not ours to
// enumerate, and the emphasis is.

// sgrDimParam is the SGR parameter for faint/dim rendering, which provider
// TUIs use for placeholder and affordance text.
const sgrDimParam = "2"

// sgrNormalIntensityParam is the SGR parameter that cancels faint/bold.
const sgrNormalIntensityParam = "22"

// paneInputLine is one classified prompt row of a pane capture.
type paneInputLine struct {
	// text is the row's content after the prompt glyph, trimmed.
	text string
	// dim reports whether the text was drawn faint (provider chrome).
	dim bool
	// styled reports whether the row carried any SGR sequence at all. An
	// unstyled row with text is indeterminate, never a finding.
	styled bool
}

// classifyPaneInput derives an input observation from an escape-preserving
// pane capture (`capture-pane -p -e`), newest line last.
//
// A busy indicator anywhere in the capture wins: text on the input line of a
// working agent is being typed or queued behind the running turn, not stalled.
// Otherwise the last prompt row — the live input line — decides, on the
// rendered intensity of its text. See the file comment for why earlier prompt
// rows are not consulted.
func classifyPaneInput(styledLines []string, promptPrefix string) runtime.InputObservation {
	plain := make([]string, 0, len(styledLines))
	inputLine := paneInputLine{}
	promptRow := -1
	for i, line := range styledLines {
		text, dim, styled := decodeStyledLine(line)
		plain = append(plain, text)
		if row, ok := promptRowText(text, promptPrefix, dim); ok {
			inputLine = paneInputLine{text: row.text, dim: row.dim, styled: styled}
			promptRow = i
		}
	}

	if paneContainsBusyIndicator(plain) {
		return runtime.InputObservation{State: runtime.InputStateWorking}
	}
	if promptRow < 0 {
		return runtime.InputObservation{State: runtime.InputStateUnknown}
	}
	if paneShowsSelectionMenu(plain[promptRow+1:]) {
		// The glyph is a selection cursor, not an input prompt.
		return runtime.InputObservation{State: runtime.InputStateUnknown}
	}
	if inputLine.text == "" {
		return runtime.InputObservation{State: runtime.InputStateIdle}
	}
	if !inputLine.styled {
		return runtime.InputObservation{State: runtime.InputStateUnknown}
	}
	if inputLine.dim {
		// The provider's own placeholder or affordance text. Not input.
		return runtime.InputObservation{State: runtime.InputStateIdle}
	}
	return runtime.InputObservation{
		State:    runtime.InputStateHoldingText,
		HeldText: inputLine.text,
	}
}

// paneShowsSelectionMenu reports whether the lines BELOW the last prompt row
// carry the footer of an interactive option list ("Enter to select · ↑/↓ to
// navigate · Esc to cancel"). Only those lines are scanned: the provider draws
// this footer beneath the list it belongs to, so ordinary transcript prose
// higher up cannot suppress a real finding, and the footer of a normal input
// box ("bypass permissions on (shift+tab to cycle)") does not match.
//
// The matcher is deliberately generous. A missed menu turns a healthy pane
// into a reported stall; a menu matched too eagerly only withholds one
// advisory finding. Those costs are not symmetric.
func paneShowsSelectionMenu(linesBelowPrompt []string) bool {
	for _, line := range linesBelowPrompt {
		if strings.Contains(line, "to select") ||
			strings.Contains(line, "to navigate") ||
			strings.Contains(line, "↑/↓") {
			return true
		}
	}
	return false
}

// promptRowHeld is the text a prompt row holds and how it was rendered.
type promptRowHeld struct {
	text string
	dim  bool
}

// promptRowText reports the text a pane row holds after its prompt glyph, and
// whether that text was drawn faint. ok is false when the row is not a prompt
// row. dim is the per-rune intensity of plain, as returned by
// decodeStyledLine.
//
// Only the leading run of padding and box border is skipped before the glyph,
// so a prompt character inside a message body is never mistaken for a prompt.
func promptRowText(plain string, promptPrefix string, dim []bool) (promptRowHeld, bool) {
	glyph := strings.TrimSpace(strings.ReplaceAll(promptPrefix, nbsp, " "))
	if glyph == "" {
		return promptRowHeld{}, false
	}
	runes := []rune(plain)
	i := 0
	for i < len(runes) && isPromptRowPadding(runes[i]) {
		i++
	}
	if !strings.HasPrefix(string(runes[i:]), glyph) {
		return promptRowHeld{}, false
	}
	i += len([]rune(glyph))
	for i < len(runes) && isPromptRowPadding(runes[i]) {
		i++
	}
	text := strings.TrimSpace(string(runes[i:]))
	if text == "" {
		return promptRowHeld{text: ""}, true
	}
	isDim := false
	if i < len(dim) {
		isDim = dim[i]
	}
	return promptRowHeld{text: text, dim: isDim}, true
}

// isPromptRowPadding reports whether r is layout, not content: the spaces and
// box border a TUI draws around its input line. Mirrors the tolerance
// matchesPromptPrefix applies (NBSP normalization happens before this).
func isPromptRowPadding(r rune) bool {
	switch r {
	case ' ', '\t', nbspRune, '│', '┃', '|':
		return true
	}
	return false
}

// decodeStyledLine splits one escape-preserving capture line into its plain
// text and, for each rune of that text, whether faint rendering was active
// when the terminal drew it. styled reports whether the line carried any SGR
// sequence, which is what separates "drawn bright" from "no styling
// information available".
//
// NBSP is normalized to a space so prompt matching behaves the same way
// matchesPromptPrefix does.
func decodeStyledLine(line string) (plain string, dim []bool, styled bool) {
	var b strings.Builder
	faint := false
	for i := 0; i < len(line); {
		if line[i] == escByte {
			seq, consumed, ok := scanEscapeSequence(line[i:])
			if !ok {
				// An unterminated sequence runs to the end of the line by
				// definition, so there is no content left behind it. Drop the
				// remainder rather than rendering control bytes as text.
				break
			}
			i += consumed
			if params, isSGR := sgrParams(seq); isSGR {
				styled = true
				faint = applySGRIntensity(faint, params)
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		if r == nbspRune {
			r = ' '
		}
		b.WriteRune(r)
		dim = append(dim, faint)
		i += size
	}
	return b.String(), dim, styled
}

// escByte is the ASCII escape that opens every terminal control sequence.
const escByte = 0x1b

// nbsp and nbspRune are the no-break space some agent TUIs render between the
// prompt glyph and the input text. Normalized to a plain space so prompt
// matching behaves the way matchesPromptPrefix does.
const (
	nbsp     = "\u00a0"
	nbspRune = '\u00a0'
)

// scanEscapeSequence returns the complete terminal escape sequence at the
// start of s, the bytes it occupies, and whether one was found. It recognizes
// CSI (ESC [ … final byte), OSC (ESC ] … BEL or ST) and the two-byte escapes,
// so a non-SGR sequence is consumed whole instead of leaking into the text.
func scanEscapeSequence(s string) (seq string, consumed int, ok bool) {
	if len(s) < 2 || s[0] != escByte {
		return "", 0, false
	}
	switch s[1] {
	case '[':
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return s[:i+1], i + 1, true
			}
		}
		return "", 0, false
	case ']':
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return s[:i+1], i + 1, true
			}
			if s[i] == escByte && i+1 < len(s) && s[i+1] == '\\' {
				return s[:i+2], i + 2, true
			}
		}
		return "", 0, false
	default:
		return s[:2], 2, true
	}
}

// sgrParams returns the parameter list of an SGR sequence and whether seq is
// one. `ESC[m` yields an empty list, which SGR defines as a full reset.
func sgrParams(seq string) ([]string, bool) {
	if len(seq) < 3 || seq[0] != escByte || seq[1] != '[' || seq[len(seq)-1] != 'm' {
		return nil, false
	}
	body := seq[2 : len(seq)-1]
	if body == "" {
		return nil, true
	}
	return strings.Split(body, ";"), true
}

// applySGRIntensity folds one SGR parameter list into the faint-rendering
// state.
//
// Extended-color parameters are consumed with their own sub-parameters:
// `38;2;<r>;<g>;<b>` and `48;5;<n>` carry literal "2" and "5" selectors that
// would otherwise be read as attributes, which would make every truecolor span
// look faint.
func applySGRIntensity(faint bool, params []string) bool {
	if len(params) == 0 {
		return false
	}
	for i := 0; i < len(params); i++ {
		switch params[i] {
		case "", "0":
			faint = false
		case sgrDimParam:
			faint = true
		case sgrNormalIntensityParam:
			faint = false
		case "38", "48", "58":
			if i+1 >= len(params) {
				continue
			}
			switch params[i+1] {
			case "2":
				i += 4 // 38;2;r;g;b
			case "5":
				i += 2 // 38;5;n
			default:
				i++
			}
		}
	}
	return faint
}

// CapturePaneStyledLines captures the last N lines of a pane with SGR
// sequences preserved (`capture-pane -e`). Unlike CapturePaneLines, the
// styling survives, which is what makes provider placeholder chrome
// distinguishable from text a sender delivered.
func (t *Tmux) CapturePaneStyledLines(target string, lines int) ([]string, error) {
	out, err := t.run("capture-pane", "-p", "-e", "-t", target, "-S", fmt.Sprintf("-%d", lines))
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// ObserveInput classifies what the named session's input area is holding by
// reading the live pane, so a caller in any process gets the same answer
// without consulting recorded state. It resolves the agent pane the way
// delivery does, so a multi-pane session is classified on the pane the agent
// actually runs in.
func (t *Tmux) ObserveInput(session string) (runtime.InputObservation, error) {
	target := session
	if agentPane, err := t.FindAgentPane(session); err == nil && agentPane != "" {
		target = agentPane
	}
	return t.observeInputOn(session, target)
}

// observeInputOn classifies an already-resolved pane target. Delivery resolves
// the agent pane before it types, so it classifies the pane it typed into
// rather than resolving it a second time.
func (t *Tmux) observeInputOn(session, target string) (runtime.InputObservation, error) {
	lines, err := t.CapturePaneStyledLines(target, promptObservationLines)
	if err != nil {
		return runtime.InputObservation{State: runtime.InputStateUnknown}, err
	}
	return classifyPaneInput(lines, t.readyPromptPrefix(session)), nil
}

// readyPromptPrefix resolves the prompt glyph for a session, honoring the
// per-session override the runtime config exports.
func (t *Tmux) readyPromptPrefix(session string) string {
	if configured, err := t.GetEnvironment(session, sessionReadyPromptEnvKey); err == nil {
		return idlePromptPrefix(configured)
	}
	return DefaultReadyPromptPrefix
}
