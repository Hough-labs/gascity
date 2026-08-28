package nudgepoller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/pidutil"
)

func TestCommandArgsMatchCmdlineMatcher(t *testing.T) {
	cityPath := "/tmp/gc-city"
	sessionName := "sess-worker"
	agentName := "agent"

	argv := append([]string{"gc"}, CommandArgs(cityPath, sessionName, agentName)...)
	if !CmdlineMatcher(cityPath, sessionName, agentName)(argv) {
		t.Fatalf("CmdlineMatcher did not match CommandArgs argv: %v", argv)
	}
}

func TestCmdlineMatcherRejectsWrongOwnership(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--city", "/tmp/gc-city", "--session", "sess-worker", "agent"}
	cases := []struct {
		name        string
		cityPath    string
		sessionName string
		agentName   string
	}{
		{name: "empty city", cityPath: "", sessionName: "sess-worker", agentName: "agent"},
		{name: "empty session", cityPath: "/tmp/gc-city", sessionName: "", agentName: "agent"},
		{name: "empty target", cityPath: "/tmp/gc-city", sessionName: "sess-worker", agentName: ""},
		{name: "wrong city", cityPath: "/tmp/other-city", sessionName: "sess-worker", agentName: "agent"},
		{name: "wrong session", cityPath: "/tmp/gc-city", sessionName: "other-session", agentName: "agent"},
		{name: "wrong target", cityPath: "/tmp/gc-city", sessionName: "sess-worker", agentName: "session-id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if CmdlineMatcher(tc.cityPath, tc.sessionName, tc.agentName)(argv) {
				t.Fatalf("CmdlineMatcher(%q, %q, %q) matched %v", tc.cityPath, tc.sessionName, tc.agentName, argv)
			}
		})
	}
}

func TestCmdlineMatcherAcceptsFlagEqualsForm(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--session=sess-worker", "--city=/tmp/gc-city", "agent"}
	if !CmdlineMatcher("/tmp/gc-city", "sess-worker", "agent")(argv) {
		t.Fatalf("CmdlineMatcher did not match equals-form flags: %v", argv)
	}
}

func TestArgvHasPollTargetEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{
			name: "generated target after flags",
			argv: []string{"gc", "nudge", "poll", "--city", "/tmp/gc-city", "--session", "sess-worker", "agent"},
			want: true,
		},
		{
			name: "target before flags",
			argv: []string{"gc", "nudge", "poll", "agent", "--city", "/tmp/gc-city", "--session", "sess-worker"},
			want: true,
		},
		{
			name: "known space form flags before target",
			argv: []string{"gc", "nudge", "poll", "--interval", "1s", "--quiescence", "2s", "agent"},
			want: true,
		},
		{
			name: "known equals form flags before target",
			argv: []string{"gc", "nudge", "poll", "--interval=1s", "--quiescence=2s", "agent"},
			want: true,
		},
		{
			name: "unknown space form flag skips value",
			argv: []string{"gc", "nudge", "poll", "--future", "value", "agent"},
			want: true,
		},
		{
			name: "unknown equals form flag before target",
			argv: []string{"gc", "nudge", "poll", "--future=value", "agent"},
			want: true,
		},
		{
			name: "first positional must be target",
			argv: []string{"gc", "nudge", "poll", "other", "agent"},
			want: false,
		},
		{
			name: "no target",
			argv: []string{"gc", "nudge", "poll", "--city", "/tmp/gc-city"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := argvHasPollTarget(tc.argv, "agent"); got != tc.want {
				t.Fatalf("argvHasPollTarget(%v, agent) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

func TestCmdlineMatcherNormalizesCityPath(t *testing.T) {
	cityPath := t.TempDir()
	argv := []string{"gc", "nudge", "poll", "--city", cityPath, "--session", "sess-worker", "agent"}
	if !CmdlineMatcher(filepath.Join(cityPath, "."), "sess-worker", "agent")(argv) {
		t.Fatalf("CmdlineMatcher did not match equivalent city path spelling: %v", argv)
	}
}

func TestCmdlineMatcherAcceptsAnyMatchingCityFlag(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--city", "/tmp/other-city", "--city=/tmp/gc-city", "--session", "sess-worker", "agent"}
	if !CmdlineMatcher("/tmp/gc-city", "sess-worker", "agent")(argv) {
		t.Fatalf("CmdlineMatcher did not match later city flag: %v", argv)
	}
}

func TestPollerFileStemSanitizesSessionPrefix(t *testing.T) {
	stem := PollerFileStem(" ../sess worker/one ", "target")
	if !strings.HasPrefix(stem, "sess-worker-one-") {
		t.Fatalf("PollerFileStem prefix = %q, want sanitized session prefix", stem)
	}
	if strings.ContainsAny(stem, `/\ `) {
		t.Fatalf("PollerFileStem = %q, want filesystem-safe stem", stem)
	}
}

func TestPollerFileStemUsesFallbackPrefixForEmptySession(t *testing.T) {
	stem := PollerFileStem("   ", "target")
	if !strings.HasPrefix(stem, "session-") {
		t.Fatalf("PollerFileStem empty session prefix = %q, want session-*", stem)
	}
}

func TestPollerFileStemTruncatesLongSessionPrefix(t *testing.T) {
	stem := PollerFileStem(strings.Repeat("a", 60), "target")
	prefix, _, ok := strings.Cut(stem, "-")
	if !ok {
		t.Fatalf("PollerFileStem = %q, want prefix and digest", stem)
	}
	if len(prefix) != 48 {
		t.Fatalf("PollerFileStem prefix length = %d, want 48", len(prefix))
	}
}

func TestPollerFileStemDistinguishesSessionTargetTuples(t *testing.T) {
	if PollerFileStem("ab", "c") == PollerFileStem("a", "bc") {
		t.Fatal("PollerFileStem returned the same stem for distinct session/target tuples")
	}
}

func TestSafeFileStemPartTrimsUnsafeEdges(t *testing.T) {
	if got := safeFileStemPart(" ../worker session/. "); got != "worker-session" {
		t.Fatalf("safeFileStemPart() = %q, want worker-session", got)
	}
}

func TestCmdlineMatcherRequiresNudgePollCommand(t *testing.T) {
	argv := []string{"gc", "nudge", "--city", "/tmp/gc-city", "poll", "--session", "sess-worker", "agent"}
	if CmdlineMatcher("/tmp/gc-city", "sess-worker", "agent")(argv) {
		t.Fatalf("CmdlineMatcher matched non-contiguous nudge poll argv: %v", argv)
	}
}

// TestAliveWithCmdlineRecognizesRealPollerProcess exercises the singleton
// check the way production does — the real matcher against a real process
// table entry — rather than against a synthetic argv slice.
//
// That gap is why gascity-ggq stayed invisible: the matcher was always
// correct, and the tests above prove it, but off linux the guard returned true
// without ever calling it. A live process holding a recycled PID therefore
// answered "the poller is already running", gc skipped spawning one, and
// nudges to that session stopped being delivered with nothing logged.
//
// The city path here deliberately contains a space. Argv must reach the
// matcher with its argument boundaries intact; a source that renders argv as
// one space-joined string cannot represent this case, and the guard would stop
// recognizing its own poller.
func TestAliveWithCmdlineRecognizesRealPollerProcess(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "gc city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", cityPath, err)
	}
	const sessionName = "sess-worker"
	const agentName = "agent"

	// Two commands, not one: a shell handed a single simple command
	// exec-replaces itself with it, which would swap out the argv under test.
	// The poller argv tail rides along as the shell's own arguments.
	args := append([]string{"-c", "sleep 5; :"}, CommandArgs(cityPath, sessionName, agentName)...)

	// This is the package's only exec site, and the repository's resource
	// census records it as such (test/test-resources.toml): tests added here
	// should reuse it rather than spawn a second process. The child leads its
	// own process group so cleanup reaps the shell and the sleep it forks.
	cmd := exec.Command("sh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sh: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid

	if !pidutil.AliveWithCmdline(pid, CmdlineMatcher(cityPath, sessionName, agentName)) {
		argv, err := pidutil.Cmdline(pid)
		t.Fatalf("AliveWithCmdline(%d, matcher for %q) = false, want true; process argv = %q (err %v)", pid, cityPath, argv, err)
	}

	// The recycled-PID case: the same live PID, checked by a poller that does
	// not own it, must not be mistaken for that poller's own process.
	otherCity := filepath.Join(t.TempDir(), "other city")
	if err := os.MkdirAll(otherCity, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", otherCity, err)
	}
	if pidutil.AliveWithCmdline(pid, CmdlineMatcher(otherCity, sessionName, agentName)) {
		t.Fatalf("AliveWithCmdline(%d, matcher for %q) = true, want false — an unrelated live PID must not satisfy the singleton check", pid, otherCity)
	}
}
