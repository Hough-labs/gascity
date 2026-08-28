package pidutil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startProcess spawns name with args, registers cleanup that kills and reaps
// the child, and returns the started command.
//
// It is deliberately the only exec site in this file. The repository's
// resource census ratchets on how many subprocess call sites untagged tests
// contain, so every spawn here routes through this one helper instead of
// constructing its own exec.Cmd; test/test-resources.toml records that
// convention and the precedent it follows. New tests in this file should reuse
// it rather than add a call site.
//
// A test that needs an already-exited process waits for the child explicitly;
// the cleanup's redundant kill and wait then no-op.
//
// The child leads its own process group and cleanup signals the group, so a
// fixture that itself spawns something — a shell running a script — leaves no
// grandchild running after the test that started it.
func startProcess(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s %q: %v", name, args, err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}

func TestAliveTreatsZombieAsDead(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("zombie detection uses /proc on linux")
	}

	cmd := startProcess(t, "sh", "-c", "exit 0")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !Alive(cmd.Process.Pid) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("Alive(%d) stayed true for exited child", cmd.Process.Pid)
}

func TestPSReportsZombieReturnsWhenPSHangs(t *testing.T) {
	binDir := t.TempDir()
	psPath := filepath.Join(binDir, "ps")
	if err := os.WriteFile(psPath, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	start := time.Now()
	if got := psReportsZombie(os.Getpid()); got {
		t.Fatalf("psReportsZombie() = true, want false when ps hangs")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("psReportsZombie took %s, want bounded timeout", elapsed)
	}
}

func TestStartTimeStableForLivePID(t *testing.T) {
	first, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatalf("StartTime(%d): %v", os.Getpid(), err)
	}
	if first == "" {
		t.Fatalf("StartTime(%d) = empty, want a starttime token", os.Getpid())
	}
	second, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatalf("StartTime(%d) second call: %v", os.Getpid(), err)
	}
	if first != second {
		t.Fatalf("StartTime not stable across calls: %q vs %q", first, second)
	}
}

func TestStartTimeRejectsInvalidPID(t *testing.T) {
	if _, err := StartTime(0); err == nil {
		t.Fatal("StartTime(0) = nil error, want error")
	}
}

// TestAliveWithStartTimeDisambiguatesRecycledPID checks the three branches that
// close the PID-reuse hole: a matching start time reports alive, a mismatched
// one (the recycled-PID case) reports dead even though the PID is live, and an
// empty start time falls back to plain liveness.
func TestAliveWithStartTimeDisambiguatesRecycledPID(t *testing.T) {
	self := os.Getpid()
	st, err := StartTime(self)
	if err != nil {
		t.Fatalf("StartTime(%d): %v", self, err)
	}

	if !AliveWithStartTime(self, st) {
		t.Fatalf("AliveWithStartTime(%d, matching) = false, want alive", self)
	}
	// A different start-time token models the PID having been reaped and reused
	// by an unrelated process: the original target must read as dead.
	if AliveWithStartTime(self, st+"0") {
		t.Fatalf("AliveWithStartTime(%d, mismatched) = true, want dead (recycled)", self)
	}
	// Empty start time disables the identity check (start time uncaptured).
	if !AliveWithStartTime(self, "") {
		t.Fatalf("AliveWithStartTime(%d, empty) = false, want fallback to Alive", self)
	}
}

func TestAliveWithStartTimeDeadPID(t *testing.T) {
	cmd := startProcess(t, "true")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("waiting for test process: %v", err)
	}
	pid := cmd.Process.Pid
	if AliveWithStartTime(pid, "12345") {
		t.Fatalf("AliveWithStartTime(%d, ...) = true for exited process", pid)
	}
}

func TestAliveWithCmdlineRejectsUnrelatedLivePID(t *testing.T) {
	if AliveWithCmdline(os.Getpid(), func(_ []string) bool {
		return false
	}) {
		t.Fatalf("AliveWithCmdline(%d) = true for non-matching cmdline", os.Getpid())
	}
}

func TestAliveWithCmdlineAcceptsMatchingLivePID(t *testing.T) {
	if !AliveWithCmdline(os.Getpid(), func(argv []string) bool {
		return len(argv) > 0 && strings.Contains(filepath.Base(argv[0]), "pidutil")
	}) {
		t.Fatalf("AliveWithCmdline(%d) = false for matching cmdline", os.Getpid())
	}
}

func TestCmdlineReturnsOwnArgv(t *testing.T) {
	argv, err := Cmdline(os.Getpid())
	if err != nil {
		t.Fatalf("Cmdline(%d): %v", os.Getpid(), err)
	}
	if len(argv) == 0 || !strings.Contains(filepath.Base(argv[0]), "pidutil") {
		t.Fatalf("Cmdline(%d) = %v, want test binary argv", os.Getpid(), argv)
	}
}

func TestNormalizeArgv(t *testing.T) {
	got := NormalizeArgv([]string{"cut", "", "-d", " ", "\t ", "-f", "1"})
	want := []string{"cut", "-d", "-f", "1"}
	if len(got) != len(want) {
		t.Fatalf("NormalizeArgv = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NormalizeArgv = %q, want %q", got, want)
		}
	}
	if out := NormalizeArgv(nil); len(out) != 0 {
		t.Fatalf("NormalizeArgv(nil) = %q, want empty", out)
	}
}

func TestArgvContainsSequence(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--city", "/tmp/city"}
	cases := []struct {
		name string
		seq  []string
		want bool
	}{
		{name: "empty sequence", seq: nil, want: true},
		{name: "contiguous sequence", seq: []string{"nudge", "poll"}, want: true},
		{name: "non-contiguous sequence", seq: []string{"gc", "poll"}, want: false},
		{name: "argv shorter than sequence", seq: []string{"gc", "nudge", "poll", "--city", "/tmp/city", "extra"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ArgvContainsSequence(argv, tc.seq...); got != tc.want {
				t.Fatalf("ArgvContainsSequence(%v, %v) = %v, want %v", argv, tc.seq, got, tc.want)
			}
		})
	}
}

// TestChildPIDsFindsLiveChild is a RED test for ga-gxmz9n: ChildPIDs must
// enumerate a real live direct child portably (no /proc dependency), on
// linux and darwin alike.
func TestChildPIDsFindsLiveChild(t *testing.T) {
	cmd := startProcess(t, "sleep", "5")

	deadline := time.Now().Add(2 * time.Second)
	var pids []int
	for time.Now().Before(deadline) {
		var err error
		pids, err = ChildPIDs(os.Getpid())
		if err != nil {
			t.Fatalf("ChildPIDs(%d): %v", os.Getpid(), err)
		}
		if slices.Contains(pids, cmd.Process.Pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ChildPIDs(%d) = %v, want to contain live child pid %d", os.Getpid(), pids, cmd.Process.Pid)
}

// TestChildPIDsReturnsErrorWhenPSHangs is a RED test for ga-gxmz9n's binding
// constraint: when enumeration cannot complete, ChildPIDs must report an
// error rather than silently returning an empty (falsely "no children")
// result — otherwise a leak-detection caller cannot tell "checked, found
// none" apart from "never actually checked". Mirrors
// TestPSReportsZombieReturnsWhenPSHangs's PATH-shadowing technique.
func TestChildPIDsReturnsErrorWhenPSHangs(t *testing.T) {
	binDir := t.TempDir()
	psPath := filepath.Join(binDir, "ps")
	if err := os.WriteFile(psPath, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	start := time.Now()
	pids, err := ChildPIDs(os.Getpid())
	if err == nil {
		t.Fatalf("ChildPIDs with a hanging ps: got pids=%v err=nil, want a non-nil error", pids)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("ChildPIDs took %s, want bounded timeout", elapsed)
	}
}

// fakePSEnumTimeout is the enumeration deadline tests give childPIDs when they
// shadow ps on PATH with a fake. It is deliberately far larger than the
// production childEnumTimeout: a fake ps is a brand-new executable in a
// t.TempDir(), so calling it costs a PATH walk, a fork+exec of a shell and, on
// darwin, a first-execution syspolicy check — costs the real ps never pays.
// Under the process-creation pressure of the -p=4 test-mac sweep those costs
// alone exceed a second, and a test asserting on which rows ChildPIDs filters
// would fail as "ps enumeration failed: signal: killed" (gascity-gs8). The
// bound stays finite so a genuinely wedged fixture still fails rather than
// hanging until the go test timeout.
const fakePSEnumTimeout = 30 * time.Second

// TestChildPIDsExcludesItsOwnEnumerationHelper is a regression test: ps is
// itself alive, and a child of the caller, at the instant it captures the
// process table, so an unfiltered ChildPIDs(os.Getpid()) always reports at
// least one phantom "child" — the transient ps invocation itself — even
// when no real child exists. This is exactly the self-monitoring pattern
// this package's callers use for leak checks (ChildPIDs(os.Getpid())), and
// it produced a false-positive "leaked child" on every run of
// internal/workspacesvc's TestMain regardless of any real leak (ga-gxmz9n).
//
// The fake ps here reports a single row for itself ($$, the real parent),
// mirroring the one spurious row a genuine ps produces in the self-check
// case; ChildPIDs must recognize that row as its own helper and exclude it.
func TestChildPIDsExcludesItsOwnEnumerationHelper(t *testing.T) {
	binDir := t.TempDir()
	psPath := filepath.Join(binDir, "ps")
	script := fmt.Sprintf("#!/bin/sh\necho \"$$ %d\"\n", os.Getpid())
	if err := os.WriteFile(psPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	// childPIDs, not ChildPIDs: this asserts on the self-exclusion filter, not
	// on the production enumeration budget, so it must not be gated on whether
	// this fake fits inside childEnumTimeout. ChildPIDs' own binding of that
	// constant stays covered by TestChildPIDsReturnsErrorWhenPSHangs.
	ctx, cancel := context.WithTimeout(context.Background(), fakePSEnumTimeout)
	defer cancel()

	pids, err := childPIDs(ctx, os.Getpid())
	if err != nil {
		t.Fatalf("childPIDs(%d): %v", os.Getpid(), err)
	}
	if len(pids) != 0 {
		t.Fatalf("childPIDs(%d) = %v, want empty — the only ps row was the enumeration helper's own (self, parent) pair and must be excluded, not reported as a leaked child", os.Getpid(), pids)
	}
}

func TestArgvHasFlagValue(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--city", "/tmp/city-a", "--session=s-worker"}
	cases := []struct {
		name  string
		flag  string
		value string
		want  bool
	}{
		{name: "space form", flag: "--city", value: "/tmp/city-a", want: true},
		{name: "equals form", flag: "--session", value: "s-worker", want: true},
		{name: "wrong value", flag: "--city", value: "/tmp/city-b", want: false},
		{name: "empty flag", flag: "", value: "/tmp/city-a", want: false},
		{name: "empty value", flag: "--city", value: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ArgvHasFlagValue(argv, tc.flag, tc.value); got != tc.want {
				t.Fatalf("ArgvHasFlagValue(%v, %q, %q) = %v, want %v", argv, tc.flag, tc.value, got, tc.want)
			}
		})
	}
}

// TestAliveWithCmdlineConsultsMatcherForLiveProcess is the regression test for
// gascity-ggq. AliveWithCmdline used to short-circuit to true on every
// non-linux host without ever calling its matcher, so on darwin any live
// process holding a recycled PID answered "yes, that's mine" to the nudge
// poller's singleton check — gc then skipped spawning a poller and nudges to
// that session stopped being delivered with no error surfaced. The matcher
// must be consulted, and its answer must be the answer, on every supported
// platform.
func TestAliveWithCmdlineConsultsMatcherForLiveProcess(t *testing.T) {
	pid := startProcess(t, "sleep", "5").Process.Pid

	var seen []string
	if !AliveWithCmdline(pid, func(argv []string) bool {
		seen = argv
		return ArgvContainsSequence(argv, "sleep", "5")
	}) {
		t.Fatalf("AliveWithCmdline(%d, matching) = false, want true; matcher saw argv %q", pid, seen)
	}
	if !ArgvContainsSequence(seen, "sleep", "5") {
		t.Fatalf("matcher saw argv %q, want the spawned process's own argv", seen)
	}

	// The rejecting direction on the same live PID: a live process alone must
	// never satisfy the guard. This is the assertion that failed on darwin.
	if AliveWithCmdline(pid, func([]string) bool { return false }) {
		t.Fatalf("AliveWithCmdline(%d, rejecting) = true for a live PID, want false — the matcher's answer must decide", pid)
	}
}

// TestCmdlinePreservesArgumentsContainingWhitespace pins the argument-boundary
// contract the identity guard depends on: argv must come from a source that
// preserves the real vector, not from splitting a rendered command string on
// whitespace. A city path or session name containing a space is what separates
// the two — a whitespace split renders it as two arguments, and the nudge
// poller's matcher compares --city against a whole token, so the guard would
// stop recognizing its own poller.
func TestCmdlinePreservesArgumentsContainingWhitespace(t *testing.T) {
	// Two commands, not one: a shell handed a single simple command
	// exec-replaces itself with it, which would swap out the argv under test.
	const script = "sleep 5; :"
	cmd := startProcess(t, "sh", "-c", script)

	argv, err := Cmdline(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Cmdline(%d): %v", cmd.Process.Pid, err)
	}
	if len(argv) != 3 || argv[1] != "-c" || argv[2] != script {
		t.Fatalf("Cmdline(%d) = %q, want a 3-element argv with %q intact as a single argument", cmd.Process.Pid, argv, script)
	}
}

// TestCmdlineRejectsInvalidPID covers the fail-closed direction: a PID that
// cannot name a process yields an error, never an empty argv that a permissive
// matcher could accept.
func TestCmdlineRejectsInvalidPID(t *testing.T) {
	if argv, err := Cmdline(0); err == nil {
		t.Fatalf("Cmdline(0) = %q, nil error; want an error", argv)
	}
}

// TestAliveWithCmdlineRejectsNilMatcher documents that a caller with no
// identity predicate gets the conservative answer rather than plain liveness.
func TestAliveWithCmdlineRejectsNilMatcher(t *testing.T) {
	if AliveWithCmdline(os.Getpid(), nil) {
		t.Fatalf("AliveWithCmdline(%d, nil) = true, want false", os.Getpid())
	}
}
