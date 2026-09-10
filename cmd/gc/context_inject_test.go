package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return p
}

func usageLine(model string, input, cacheRead, cacheCreate int) string {
	return fmt.Sprintf(
		`{"type":"assistant","message":{"model":%q,"usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`,
		model, input, cacheRead, cacheCreate)
}

func hookInputFor(path string) []byte {
	return []byte(fmt.Sprintf(`{"transcript_path":%q,"hook_event_name":"UserPromptSubmit"}`, path))
}

func TestContextInjectSilentBelowAdvisory(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// 100k of 1M = 10% — well below the 60% advisory threshold.
	p := writeTranscript(t, usageLine("claude-fable-5", 1_000, 98_000, 1_000))
	if got := contextInjectLine(hookInputFor(p)); got != "" {
		t.Errorf("below advisory should be silent, got %q", got)
	}
}

func TestContextInjectAdvisoryBand(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// 700k of 1M = 70% — advisory band.
	p := writeTranscript(t, usageLine("claude-fable-5", 10_000, 680_000, 10_000))
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "700k/1000k") || !strings.Contains(got, "~70%") {
		t.Errorf("advisory line wrong: %q", got)
	}
	if !strings.Contains(got, "clean seam") || !strings.Contains(got, "reset") {
		t.Errorf("advisory must point toward a clean seam + planned reset, got %q", got)
	}
	if strings.Contains(got, "HIGH") {
		t.Errorf("advisory band must not be marked HIGH: %q", got)
	}
}

func TestContextInjectUrgentBand(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// 900k of 1M = 90% — urgent band.
	p := writeTranscript(t, usageLine("claude-opus-4-8[1m]", 50_000, 800_000, 50_000))
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "HIGH") || !strings.Contains(got, "gc handoff") {
		t.Errorf("urgent line must direct to handoff + self `gc handoff`: %q", got)
	}
	if !strings.Contains(got, "operator") {
		t.Errorf("urgent line must preserve the operator-stay-up override: %q", got)
	}
}

func TestContextInjectLastUsageEntryWins(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// Older 90% entry followed by a newer 10% one (post-compaction shape):
	// the LAST entry is the live context size, so this must be silent.
	p := writeTranscript(t,
		usageLine("claude-fable-5", 50_000, 800_000, 50_000),
		usageLine("claude-fable-5", 5_000, 90_000, 5_000),
	)
	if got := contextInjectLine(hookInputFor(p)); got != "" {
		t.Errorf("last entry (10%%) should win and be silent, got %q", got)
	}
}

func TestContextInjectDefaultWindow200k(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// 150k on an unrecognized model = 75% of the conservative 200k default.
	p := writeTranscript(t, usageLine("some-other-model", 10_000, 130_000, 10_000))
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "150k/200k") || !strings.Contains(got, "~75%") {
		t.Errorf("200k default window not applied: %q", got)
	}
}

func TestContextInjectWindowOverride(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_WINDOW_TOKENS", "500000")
	p := writeTranscript(t, usageLine("some-other-model", 10_000, 380_000, 10_000))
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "400k/500k") {
		t.Errorf("window override not applied: %q", got)
	}
}

func TestContextInjectThresholdOverrides(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_CONTEXT_ADVISORY_PCT", "30")
	t.Setenv("GC_CONTEXT_URGENT_PCT", "40")
	// 50% of 1M: above the overridden urgent threshold.
	p := writeTranscript(t, usageLine("claude-fable-5", 10_000, 480_000, 10_000))
	if got := contextInjectLine(hookInputFor(p)); !strings.Contains(got, "HIGH") {
		t.Errorf("threshold overrides not applied: %q", got)
	}
}

func TestContextInjectDisabled(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "0")
	p := writeTranscript(t, usageLine("claude-fable-5", 50_000, 800_000, 50_000))
	if got := contextInjectLine(hookInputFor(p)); got != "" {
		t.Errorf("disabled should be silent, got %q", got)
	}
}

func TestContextInjectFailSafeSilent(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	for name, input := range map[string][]byte{
		"nil stdin":          nil,
		"garbage stdin":      []byte("not json"),
		"no transcript path": []byte(`{"hook_event_name":"UserPromptSubmit"}`),
		"missing file":       hookInputFor("/nonexistent/transcript.jsonl"),
	} {
		if got := contextInjectLine(input); got != "" {
			t.Errorf("%s: want silent, got %q", name, got)
		}
	}
	// Transcript with no usage entries.
	p := writeTranscript(t, `{"type":"user","message":{"content":"hi"}}`)
	if got := contextInjectLine(hookInputFor(p)); got != "" {
		t.Errorf("no-usage transcript: want silent, got %q", got)
	}
}

// Regression: the newest usage entry lacking a model string must not flip a
// 1M session to the 200k default (would fire the urgent tier far too early).
func TestContextInjectLastNonEmptyModelWins(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// First entry names the 1M model; the newest usage entry omits model.
	// 700k must read as 70% of 1M (advisory), not 350% of 200k.
	p := writeTranscript(t,
		usageLine("claude-fable-5", 10_000, 680_000, 10_000),
		`{"type":"assistant","message":{"usage":{"input_tokens":10000,"cache_read_input_tokens":680000,"cache_creation_input_tokens":10000}}}`,
	)
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "700k/1000k") {
		t.Errorf("empty-model newest entry must retain the 1M window: %q", got)
	}
	if strings.Contains(got, "HIGH") {
		t.Errorf("70%% of 1M is advisory, not urgent: %q", got)
	}
}

// Bare claude-opus-4-8 is a 1M-context model (no [1m] suffix in the transcript).
func TestContextInjectBareOpus48Is1M(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	p := writeTranscript(t, usageLine("claude-opus-4-8", 10_000, 680_000, 10_000))
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "700k/1000k") {
		t.Errorf("bare opus-4-8 must resolve to the 1M window: %q", got)
	}
}

// Sidecar/compaction call on a smaller-window model must not shrink the
// main-loop session's window: max-over-models wins. (The observed 782k/200k
// bug: a Fable session with bare-opus sidecar entries, newest entry opus.)
func TestContextInjectSidecarDoesNotShrinkWindow(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// Newest entry classifies 200k but carries the live (high) token count; an
	// earlier entry is the 1M main-loop model. Window must be 1M (max), so 700k
	// reads as ~70% (advisory), not ~350% of 200k.
	p := writeTranscript(t,
		usageLine("claude-fable-5", 10_000, 680_000, 10_000),   // main loop, 1M
		usageLine("claude-haiku-4-5", 10_000, 680_000, 10_000), // 200k-classified, newest, high tokens
	)
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "700k/1000k") {
		t.Errorf("a 200k-classified newest entry must not shrink the 1M session window: %q", got)
	}
}

// The urgent tier is the only tier that names a recycle verb, it fires
// automatically on a threshold, and nobody is in the loop when it does. So the
// verb it names has to survive: `gc session reset` strands the seat it fires on
// (five session.reset_stalled events on record) — the session record drops, the
// seat sleeps with flags degraded to `config`, and only an operator running
// `gc session wake` brings it back. `gc handoff` is the measured-working
// alternative. Driven through contextInjectLine so the real tier selector picks
// the band rather than the test asserting against a copied literal.
func TestContextInjectUrgentTierNamesWorkingRecycleVerb(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// 900k of 1M = 90% — above the 80% urgent threshold.
	p := writeTranscript(t, usageLine("claude-fable-5", 50_000, 800_000, 50_000))
	got := contextInjectLine(hookInputFor(p))
	if !strings.Contains(got, "gc handoff") {
		t.Errorf("urgent tier must name `gc handoff` as the recycle verb: %q", got)
	}
	if strings.Contains(got, "gc session reset") {
		t.Errorf("urgent tier must not name the stranding verb `gc session reset`: %q", got)
	}
}

// Naming the right command is only half the fix: an agent runs the injected
// invocation verbatim, and `gc handoff` requires a subject (RangeArgs(1, 2) in
// newHandoffCmd), so the bare form exits non-zero and strands the seat just as
// surely as the old verb did. Parse the invocation back out of the message and
// hand it to the real command's own argument validator, so this tracks
// cmd_handoff.go instead of restating its rules.
func TestContextInjectUrgentTierNamesRunnableInvocation(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	p := writeTranscript(t, usageLine("claude-fable-5", 50_000, 800_000, 50_000))
	got := contextInjectLine(hookInputFor(p))

	m := regexp.MustCompile("`(gc handoff[^`]*)`").FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("urgent tier names no backticked `gc handoff ...` invocation: %q", got)
	}
	fields := shellFields(m[1])
	if len(fields) < 2 {
		t.Fatalf("unparseable invocation %q in urgent tier", m[1])
	}
	handoff := newHandoffCmd(io.Discard, io.Discard)
	if err := handoff.Args(handoff, fields[2:]); err != nil {
		t.Errorf("urgent tier recommends %q, which gc handoff rejects: %v", m[1], err)
	}
}

// shellFields splits a command line into the arguments a shell would hand the
// program: whitespace separates, double quotes group.
func shellFields(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote, started := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			started = true
		case !inQuote && (r == ' ' || r == '\t'):
			if started {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if started {
		out = append(out, cur.String())
	}
	return out
}

// The verb must not reappear in ANOTHER tier later. Sweep every band — silent,
// advisory, urgent — including both threshold edges, and assert none of them
// recommends `gc session reset`.
func TestContextInjectNoTierNamesSessionReset(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// claude-fable-5 is a 1M-window model, so tokens = pct * 10_000.
	for _, pct := range []int{0, 30, 59, 60, 61, 70, 79, 80, 81, 90, 99, 100} {
		p := writeTranscript(t, usageLine("claude-fable-5", pct*10_000, 0, 0))
		if got := contextInjectLine(hookInputFor(p)); strings.Contains(got, "gc session reset") {
			t.Errorf("%d%% tier names the stranding verb `gc session reset`: %q", pct, got)
		}
	}
}

// Regression: the fix belongs to the urgent tier alone. Assert the advisory
// string byte-for-byte so an edit aimed at the wrong tier fails here.
func TestContextInjectAdvisoryStringUnchanged(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	// 700k of 1M = 70% — between the 60% advisory and 80% urgent thresholds.
	p := writeTranscript(t, usageLine("claude-fable-5", 10_000, 680_000, 10_000))
	const want = "Context usage: 700k/1000k (~70%). Approaching the recycle zone. " +
		"Steer toward a clean seam: finish in-flight work, don't open new " +
		"long-horizon tasks, and keep durable notes/work-items current so a " +
		"handoff is cheap. Plan to hand off and reset before this climbs into " +
		"the urgent band — a fresh session from durable notes outperforms " +
		"riding lossy compaction.\n"
	if got := contextInjectLine(hookInputFor(p)); got != want {
		t.Errorf("advisory tier changed\n got: %q\nwant: %q", got, want)
	}
}

// Regression: swapping the verb must not cost the urgent tier its other
// load-bearing clauses. Each of these is still correct guidance and each has
// its own reason to exist — the clean seam, the mid-step warning, and the
// operator override that keeps a supervised seat from recycling itself.
func TestContextInjectUrgentTierKeepsLoadBearingClauses(t *testing.T) {
	t.Setenv("GC_INJECT_CONTEXT", "")
	p := writeTranscript(t, usageLine("claude-fable-5", 50_000, 800_000, 50_000))
	got := contextInjectLine(hookInputFor(p))
	for _, want := range []string{
		"reach a clean seam",
		"durable notes + work-item updates + memory",
		"do NOT abandon work mid-step",
		"(If an operator has told you to stay up, honor that and just hold at a clean seam instead of resetting.)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("urgent tier lost the clause %q: %q", want, got)
		}
	}
}
