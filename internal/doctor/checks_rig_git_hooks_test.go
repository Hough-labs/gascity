package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// RED tests for RigGitHooksCheck (gascity-jiao). The check reports when the
// hooks git will actually run are not the repo's tracked .githooks — the state
// that left this fork's pre-push ownership guard and pre-commit OpenAPI guards
// silently inert behind stale copies in a bd-owned hooks directory.

func TestRigGitHooksCheck_Metadata(t *testing.T) {
	c := NewRigGitHooksCheck(config.Rig{Name: "testrig", Path: t.TempDir()})

	if got, want := c.Name(), "rig:testrig:git-hooks"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if c.CanFix() {
		t.Error("CanFix() = true, want false — repointing core.hooksPath is an operator decision")
	}
	if c.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Errorf("Fix() = %v, want nil no-op", err)
	}
}

func TestRigGitHooksCheck_NoTrackedHooksDir_OK(t *testing.T) {
	repo := initGitHooksTestRepo(t)

	r := NewRigGitHooksCheck(config.Rig{Name: "testrig", Path: repo}).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "nothing to enforce") {
		t.Errorf("message = %q, want it to say there is nothing to enforce", r.Message)
	}
}

func TestRigGitHooksCheck_HooksPathIsTrackedDir_OK(t *testing.T) {
	repo := initGitHooksTestRepo(t)
	writeTrackedHook(t, repo, "pre-push", "#!/bin/sh\nexit 0\n")
	runGitForGitHooksTest(t, repo, "config", "core.hooksPath", ".githooks")

	r := NewRigGitHooksCheck(config.Rig{Name: "testrig", Path: repo}).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "pre-push") {
		t.Errorf("message = %q, want the enforced hook names", r.Message)
	}
	if r.FixHint != "" {
		t.Errorf("FixHint = %q, want empty for OK result", r.FixHint)
	}
}

func TestRigGitHooksCheck_IdenticalCopyInOwnedDir_OK(t *testing.T) {
	repo := initGitHooksTestRepo(t)
	body := "#!/bin/sh\nexit 0\n"
	writeTrackedHook(t, repo, "pre-push", body)
	owned := pointHooksPathAtOwnedDir(t, repo)
	writeExecutableForGitHooksTest(t, filepath.Join(owned, "pre-push"), body)

	r := NewRigGitHooksCheck(config.Rig{Name: "testrig", Path: repo}).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK for a byte-identical copy", r.Status, r.Message)
	}
}

func TestRigGitHooksCheck_ForwarderInOwnedDir_OK(t *testing.T) {
	repo := initGitHooksTestRepo(t)
	writeTrackedHook(t, repo, "pre-push", "#!/bin/sh\nexit 0\n")
	owned := pointHooksPathAtOwnedDir(t, repo)
	writeExecutableForGitHooksTest(t, filepath.Join(owned, "pre-push"), "#!/bin/sh\n"+
		"# gascity-hook-forwarder: .githooks/pre-push\n"+
		"exec \"$(git rev-parse --show-toplevel)/.githooks/pre-push\" \"$@\"\n")

	r := NewRigGitHooksCheck(config.Rig{Name: "testrig", Path: repo}).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK for a forwarder", r.Status, r.Message)
	}
}

func TestRigGitHooksCheck_StaleCopy_WarnsAdvisory(t *testing.T) {
	repo := initGitHooksTestRepo(t)
	writeTrackedHook(t, repo, "pre-commit", "#!/bin/sh\nexit 0\n")
	writeTrackedHook(t, repo, "pre-push", "#!/bin/sh\nrun_the_guard\nexit 0\n")
	owned := pointHooksPathAtOwnedDir(t, repo)
	writeExecutableForGitHooksTest(t, filepath.Join(owned, "pre-commit"), "#!/bin/sh\nexit 0\n")
	writeExecutableForGitHooksTest(t, filepath.Join(owned, "pre-push"), "#!/bin/sh\nexit 0\n")

	r := NewRigGitHooksCheck(config.Rig{Name: "testrig", Path: repo}).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning for a drifted copy", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("severity = %d, want SeverityAdvisory", r.Severity)
	}
	if !strings.Contains(r.Message, "1 of 2") {
		t.Errorf("message = %q, want the shadowed/tracked hook counts", r.Message)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "pre-push") {
		t.Errorf("Details = %v, want one line naming pre-push", r.Details)
	}
	if !strings.Contains(r.FixHint, "core.hooksPath") {
		t.Errorf("FixHint = %q, want a core.hooksPath remedy", r.FixHint)
	}
}

func TestRigGitHooksCheck_HookNotInstalledInOwnedDir_Warns(t *testing.T) {
	repo := initGitHooksTestRepo(t)
	writeTrackedHook(t, repo, "pre-push", "#!/bin/sh\nexit 0\n")
	pointHooksPathAtOwnedDir(t, repo)

	r := NewRigGitHooksCheck(config.Rig{Name: "testrig", Path: repo}).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning when the hook is absent", r.Status, r.Message)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "not installed") {
		t.Errorf("Details = %v, want one line reporting the hook is not installed", r.Details)
	}
}

func TestRigGitHooksCheck_NonExecutableHookInOwnedDir_Warns(t *testing.T) {
	repo := initGitHooksTestRepo(t)
	body := "#!/bin/sh\nexit 0\n"
	writeTrackedHook(t, repo, "pre-push", body)
	owned := pointHooksPathAtOwnedDir(t, repo)
	if err := os.WriteFile(filepath.Join(owned, "pre-push"), []byte(body), 0o644); err != nil {
		t.Fatalf("write non-executable hook: %v", err)
	}

	r := NewRigGitHooksCheck(config.Rig{Name: "testrig", Path: repo}).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning for a non-executable hook", r.Status, r.Message)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "not executable") {
		t.Errorf("Details = %v, want one line reporting the hook is not executable", r.Details)
	}
}

func TestRigGitHooksCheck_DefaultHooksDirWithoutInstall_Warns(t *testing.T) {
	repo := initGitHooksTestRepo(t)
	writeTrackedHook(t, repo, "pre-push", "#!/bin/sh\nexit 0\n")

	r := NewRigGitHooksCheck(config.Rig{Name: "testrig", Path: repo}).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning when core.hooksPath was never installed", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, filepath.Join(".git", "hooks")) {
		t.Errorf("message = %q, want the effective hooks directory git resolved", r.Message)
	}
}

func TestRigGitHooksCheck_NotAGitRepo_Warns(t *testing.T) {
	dir := t.TempDir()
	writeTrackedHook(t, dir, "pre-push", "#!/bin/sh\nexit 0\n")

	r := NewRigGitHooksCheck(config.Rig{Name: "testrig", Path: dir}).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning outside a git repo", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "git") {
		t.Errorf("message = %q, want it to name the git resolution failure", r.Message)
	}
}

// initGitHooksTestRepo creates a hermetic git repo with one commit. Global and
// system git config are pinned away so an ambient core.hooksPath on the
// developer's machine cannot decide the outcome of these tests.
func initGitHooksTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	dir := t.TempDir()
	runGitForGitHooksTest(t, dir, "init")
	runGitForGitHooksTest(t, dir, "config", "user.name", "Rig Git Hooks Test")
	runGitForGitHooksTest(t, dir, "config", "user.email", "rig-git-hooks@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("initial\n"), 0o600); err != nil {
		t.Fatalf("write initial file: %v", err)
	}
	runGitForGitHooksTest(t, dir, "add", "README.md")
	runGitForGitHooksTest(t, dir, "commit", "-m", "initial")
	return dir
}

// pointHooksPathAtOwnedDir simulates another tool (beads installs its dolt-sync
// hooks this way) owning core.hooksPath, and returns that directory.
func pointHooksPathAtOwnedDir(t *testing.T, repo string) string {
	t.Helper()
	owned := filepath.Join(repo, ".beads", "hooks")
	if err := os.MkdirAll(owned, 0o755); err != nil {
		t.Fatalf("create owned hooks dir: %v", err)
	}
	runGitForGitHooksTest(t, repo, "config", "core.hooksPath", owned)
	return owned
}

func writeTrackedHook(t *testing.T, repo, name, body string) {
	t.Helper()
	dir := filepath.Join(repo, ".githooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create .githooks: %v", err)
	}
	writeExecutableForGitHooksTest(t, filepath.Join(dir, name), body)
}

func writeExecutableForGitHooksTest(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // hook scripts must be executable
		t.Fatalf("write %s: %v", path, err)
	}
}

func runGitForGitHooksTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
