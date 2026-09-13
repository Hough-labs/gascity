package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// RED tests for scripts/install-git-hooks (gascity-jiao). git runs exactly one
// hooks directory, so a repo whose core.hooksPath another tool owns — beads
// installs its dolt-sync hooks into .beads/hooks and points core.hooksPath
// there — never runs the hooks it tracks under .githooks/. The installer wires
// them up without evicting the owning tool, and the doctor check that reports
// the drift must agree the result is wired.

const beadsBlockFixture = `# --- BEGIN BEADS INTEGRATION v1.2.2 ---
# This section is managed by beads. Do not remove these markers.
echo "owner-block $*" >> "$HOOK_LOG"
# --- END BEADS INTEGRATION v1.2.2 ---`

func TestInstallGitHooksPointsUnownedHooksPathAtTrackedDir(t *testing.T) {
	repo := newHooksFixtureRepo(t)

	runInstallGitHooks(t, repo)

	if got := gitConfigValue(t, repo, "core.hooksPath"); got != ".githooks" {
		t.Fatalf("core.hooksPath = %q, want %q when no other tool owns it", got, ".githooks")
	}
	assertDoctorReportsHooksWired(t, repo)
}

func TestInstallGitHooksForwardsWhenAnotherToolOwnsHooksPath(t *testing.T) {
	repo := newHooksFixtureRepo(t)
	owned := ownHooksPath(t, repo)
	stale := "#!/usr/bin/env bash\nset -euo pipefail\necho stale >> \"$HOOK_LOG\"\n\n" + beadsBlockFixture + "\n"
	writeHookFixture(t, filepath.Join(owned, "pre-push"), stale)

	runInstallGitHooks(t, repo)

	if got := gitConfigValue(t, repo, "core.hooksPath"); got != owned {
		t.Fatalf("core.hooksPath = %q, want it left at the owning tool's %q", got, owned)
	}
	installed := readHookFile(t, filepath.Join(owned, "pre-push"))
	if !strings.Contains(installed, "gascity-hook-forwarder:") {
		t.Errorf("installed pre-push is not a forwarder:\n%s", installed)
	}
	if !strings.Contains(installed, beadsBlockFixture) {
		t.Errorf("installed pre-push dropped the owning tool's managed block:\n%s", installed)
	}
	if strings.Contains(installed, "echo stale") {
		t.Errorf("installed pre-push kept the stale hook body:\n%s", installed)
	}
	backups := backupFiles(t, owned, "pre-push")
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one copy of the displaced hook", backups)
	}
	if body := readHookFile(t, backups[0]); body != stale {
		t.Errorf("backup does not preserve the displaced hook verbatim:\n%s", body)
	}
	// .githooks/pre-commit has no counterpart in the owned dir: the installer
	// must wire every tracked hook, not only the ones already present.
	if body := readHookFile(t, filepath.Join(owned, "pre-commit")); !strings.Contains(body, "gascity-hook-forwarder:") {
		t.Errorf("pre-commit was not forwarded:\n%s", body)
	}
	assertDoctorReportsHooksWired(t, repo)
}

func TestInstallGitHooksForwarderRunsTrackedHookWithStdinThenOwnerBlock(t *testing.T) {
	repo := newHooksFixtureRepo(t)
	owned := ownHooksPath(t, repo)
	writeHookFixture(t, filepath.Join(owned, "pre-push"), "#!/usr/bin/env bash\nset -euo pipefail\n\n"+beadsBlockFixture+"\n")

	runInstallGitHooks(t, repo)

	log := filepath.Join(t.TempDir(), "hook.log")
	out, err := runHook(t, repo, filepath.Join(owned, "pre-push"), log,
		"refs/heads/x 1111111111111111111111111111111111111111 refs/heads/x 0000000000000000000000000000000000000000\n",
		"origin", "git@example.invalid:x.git")
	if err != nil {
		t.Fatalf("forwarder failed: %v\n%s", err, out)
	}

	lines := readHookLines(t, log)
	want := []string{
		"tracked pre-push args=origin git@example.invalid:x.git",
		"tracked pre-push stdin=refs/heads/x 1111111111111111111111111111111111111111 refs/heads/x 0000000000000000000000000000000000000000",
		"owner-block origin git@example.invalid:x.git",
	}
	if len(lines) != len(want) {
		t.Fatalf("hook log = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("hook log line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestInstallGitHooksForwarderPropagatesTrackedHookFailure(t *testing.T) {
	repo := newHooksFixtureRepo(t)
	owned := ownHooksPath(t, repo)
	writeHookFixture(t, filepath.Join(repo, ".githooks", "pre-push"),
		"#!/usr/bin/env bash\necho \"tracked pre-push rejected\" >> \"$HOOK_LOG\"\nexit 7\n")
	writeHookFixture(t, filepath.Join(owned, "pre-push"), "#!/usr/bin/env bash\nset -euo pipefail\n\n"+beadsBlockFixture+"\n")

	runInstallGitHooks(t, repo)

	log := filepath.Join(t.TempDir(), "hook.log")
	out, err := runHook(t, repo, filepath.Join(owned, "pre-push"), log, "", "origin", "git@example.invalid:x.git")
	if err == nil {
		t.Fatalf("forwarder exited 0 despite a rejecting tracked hook:\n%s", out)
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("forwarder exit = %v, want the tracked hook's exit code 7\n%s", err, out)
	}
	for _, line := range readHookLines(t, log) {
		if strings.HasPrefix(line, "owner-block") {
			t.Error("owning tool's block ran after the tracked hook rejected the push")
		}
	}
}

// TestInstallGitHooksForwarderRunsUnderRealGitCommit exercises the forwarder
// through git itself rather than a direct call: git runs hooks with GIT_DIR
// exported, which is what a forwarder resolving the repo root has to survive.
func TestInstallGitHooksForwarderRunsUnderRealGitCommit(t *testing.T) {
	repo := newHooksFixtureRepo(t)
	ownHooksPath(t, repo)

	runInstallGitHooks(t, repo)

	log := filepath.Join(t.TempDir(), "hook.log")
	writeHookFixture(t, filepath.Join(repo, "file.txt"), "contents\n")
	runGitInHooksFixture(t, repo, log, "add", "file.txt")
	runGitInHooksFixture(t, repo, log, "commit", "-m", "trigger the pre-commit forwarder")

	if lines := readHookLines(t, log); len(lines) == 0 || !strings.HasPrefix(lines[0], "tracked pre-commit args=") {
		t.Fatalf("hook log = %v, want git to have run the tracked pre-commit hook through the forwarder", lines)
	}
}

// TestInstallGitHooksForwarderResolvesPerWorktree covers the shape this fleet
// actually pushes from: core.hooksPath is repo-wide, so one forwarder serves
// every linked worktree and must run each worktree's own .githooks — not the
// main checkout's.
func TestInstallGitHooksForwarderResolvesPerWorktree(t *testing.T) {
	repo := newHooksFixtureRepo(t)
	owned := ownHooksPath(t, repo)
	runGitInHooksFixture(t, repo, "", "add", ".")
	runGitInHooksFixture(t, repo, "", "commit", "-m", "initial")

	runInstallGitHooks(t, repo)

	linked, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	linked = filepath.Join(linked, "linked")
	runGitInHooksFixture(t, repo, "", "worktree", "add", "--detach", linked)
	writeHookFixture(t, filepath.Join(linked, ".githooks", "pre-commit"),
		"#!/usr/bin/env bash\nset -euo pipefail\necho \"linked worktree pre-commit\" >> \"$HOOK_LOG\"\n")

	log := filepath.Join(t.TempDir(), "hook.log")
	writeHookFixture(t, filepath.Join(linked, "file.txt"), "contents\n")
	runGitInHooksFixture(t, linked, log, "add", "file.txt")
	runGitInHooksFixture(t, linked, log, "commit", "-m", "trigger the forwarder from a linked worktree")

	if lines := readHookLines(t, log); len(lines) != 1 || lines[0] != "linked worktree pre-commit" {
		t.Fatalf("hook log = %v, want the linked worktree's own .githooks/pre-commit to have run", lines)
	}
	if _, err := os.Stat(filepath.Join(owned, "pre-commit")); err != nil {
		t.Fatalf("forwarder missing from the shared hooks dir: %v", err)
	}
}

func TestInstallGitHooksIsIdempotent(t *testing.T) {
	repo := newHooksFixtureRepo(t)
	owned := ownHooksPath(t, repo)
	writeHookFixture(t, filepath.Join(owned, "pre-push"), "#!/usr/bin/env bash\nset -euo pipefail\necho stale\n\n"+beadsBlockFixture+"\n")

	runInstallGitHooks(t, repo)
	first := readHookFile(t, filepath.Join(owned, "pre-push"))
	runInstallGitHooks(t, repo)
	second := readHookFile(t, filepath.Join(owned, "pre-push"))

	if first != second {
		t.Errorf("second install rewrote the forwarder:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if got := strings.Count(second, "BEGIN BEADS INTEGRATION"); got != 1 {
		t.Errorf("managed block count = %d after two installs, want 1", got)
	}
	if backups := backupFiles(t, owned, "pre-push"); len(backups) != 1 {
		t.Errorf("backups = %v after two installs, want exactly one", backups)
	}
}

// TestMakeSetupWiresHooksThroughTheInstaller keeps `make setup` on the one
// path that knows how to share core.hooksPath. Setting it directly evicts
// whichever tool already owns that directory — here beads, whose dolt-sync
// post-checkout/post-merge/prepare-commit-msg hooks live there.
func TestMakeSetupWiresHooksThroughTheInstaller(t *testing.T) {
	makefile := readHookFile(t, filepath.Join(repoRoot(t), "Makefile"))

	setupIdx := strings.Index(makefile, "\nsetup:")
	if setupIdx < 0 {
		t.Fatal("Makefile must define a setup target")
	}
	recipe := makefile[setupIdx+1:]
	if end := strings.Index(recipe, "\n\n"); end >= 0 {
		recipe = recipe[:end]
	}

	if !strings.Contains(recipe, "scripts/install-git-hooks") {
		t.Errorf("make setup must install hooks through scripts/install-git-hooks:\n%s", recipe)
	}
	if strings.Contains(recipe, "git config core.hooksPath") {
		t.Errorf("make setup must not set core.hooksPath directly — that evicts the tool that owns it:\n%s", recipe)
	}
}

// newHooksFixtureRepo builds a hermetic git repo whose tracked .githooks
// record what they saw, so a forwarder's behavior is observable.
func newHooksFixtureRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	runGitForHooksFixture(t, dir, "init")
	runGitForHooksFixture(t, dir, "config", "user.name", "Install Git Hooks Test")
	runGitForHooksFixture(t, dir, "config", "user.email", "install-git-hooks@example.invalid")
	for _, name := range []string{"pre-commit", "pre-push"} {
		writeHookFixture(t, filepath.Join(dir, ".githooks", name), "#!/usr/bin/env bash\nset -euo pipefail\n"+
			"echo \"tracked "+name+" args=$*\" >> \"$HOOK_LOG\"\n"+
			"while read -r line; do echo \"tracked "+name+" stdin=$line\" >> \"$HOOK_LOG\"; done\n")
	}
	return dir
}

// ownHooksPath simulates another tool claiming core.hooksPath.
func ownHooksPath(t *testing.T, repo string) string {
	t.Helper()
	owned := filepath.Join(repo, ".beads", "hooks")
	if err := os.MkdirAll(owned, 0o755); err != nil {
		t.Fatalf("create owned hooks dir: %v", err)
	}
	runGitForHooksFixture(t, repo, "config", "core.hooksPath", owned)
	return owned
}

func runInstallGitHooks(t *testing.T, repo string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(repoRoot(t), "scripts", "install-git-hooks"))
	cmd.Dir = repo
	cmd.Env = hooksFixtureEnv(t, "")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install-git-hooks failed: %v\n%s", err, out)
	}
}

// runHook invokes an installed hook the way git would: from the repo root,
// with the hook's arguments and its stdin.
func runHook(t *testing.T, repo, hook, log, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(hook, args...)
	cmd.Dir = repo
	cmd.Env = hooksFixtureEnv(t, log)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func hooksFixtureEnv(t *testing.T, log string) []string {
	t.Helper()
	if log == "" {
		log = filepath.Join(t.TempDir(), "unused-hook.log")
	}
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		"HOOK_LOG=" + log,
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
	}
}

// assertDoctorReportsHooksWired ties the installer to the check that reports
// this drift: after installing, gc doctor must see the tracked hooks as live.
func assertDoctorReportsHooksWired(t *testing.T, repo string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	r := doctor.NewRigGitHooksCheck(config.Rig{Name: "fixture", Path: repo}).Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusOK {
		t.Errorf("doctor rig:fixture:git-hooks = %d (%s)\n%s", r.Status, r.Message, strings.Join(r.Details, "\n"))
	}
}

func backupFiles(t *testing.T, dir, name string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var found []string
	for _, entry := range entries {
		if entry.Name() != name && strings.HasPrefix(entry.Name(), name+".") {
			found = append(found, filepath.Join(dir, entry.Name()))
		}
	}
	return found
}

func writeHookFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // hooks must be executable
		t.Fatalf("write %s: %v", path, err)
	}
}

func readHookFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func readHookLines(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", path, err)
	}
	trimmed := strings.TrimRight(string(body), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func gitConfigValue(t *testing.T, repo, key string) string {
	t.Helper()
	cmd := exec.Command("git", "config", "--get", key)
	cmd.Dir = repo
	cmd.Env = hooksFixtureEnv(t, "")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runGitInHooksFixture runs git so that hooks it triggers write to log.
func runGitInHooksFixture(t *testing.T, dir, log string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = hooksFixtureEnv(t, log)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func runGitForHooksFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = hooksFixtureEnv(t, "")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError) //nolint:errorlint // direct type is what exec returns here
	if ok {
		*target = exitErr
	}
	return ok
}
