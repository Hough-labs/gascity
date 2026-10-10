package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAddTestenvImportSelfSkipIsPackageIdentity pins the generator's self-skip
// (gascity-ru8b): internal/testenv must never be given a testenv_import_test.go
// that makes it import itself, nor lose the import its own external test needs,
// while a decoy package whose path merely ends in internal/testenv is still
// wired up. The fixture is a polecat-home layout with a real per-bead worktree
// checked out beneath it; pruning that nested worktree is covered by
// TestAddTestenvImportSkipsNestedGitWorktrees.
func TestAddTestenvImportSelfSkipIsPackageIdentity(t *testing.T) {
	home := newTestenvGeneratorHome(t)
	out := runTestenvGenerator(t, home)

	realTestenv := filepath.Join(home, "internal", "testenv")
	if _, err := os.Stat(filepath.Join(realTestenv, "testenv_import_test.go")); !os.IsNotExist(err) {
		t.Errorf("generator made internal/testenv import itself (stat err = %v)\ngenerator output:\n%s", err, out)
	}
	ownTest, err := os.ReadFile(filepath.Join(realTestenv, "testenv_test.go"))
	if err != nil {
		t.Fatalf("read internal/testenv/testenv_test.go: %v", err)
	}
	if !strings.Contains(string(ownTest), `"github.com/gastownhall/gascity/internal/testenv"`) {
		t.Errorf("generator scrubbed the import internal/testenv's own external test needs:\n%s", ownTest)
	}

	// A decoy package whose path merely ends in internal/testenv is a different
	// package and must still be wired up.
	decoy := filepath.Join(home, "test", "fixtures", "internal", "testenv", "testenv_import_test.go")
	if _, err := os.Stat(decoy); err != nil {
		t.Errorf("generator skipped %s, so the self-skip matches a path fragment rather than package identity: %v\ngenerator output:\n%s", decoy, err, out)
	}
}

// newTestenvGeneratorHome builds a module that mirrors the gastown polecat-home
// layout: a git worktree with a real per-bead worktree checked out beneath it at
// worktrees/gascity-nested. It returns the home directory.
func newTestenvGeneratorHome(t *testing.T) string {
	t.Helper()

	home := filepath.Join(t.TempDir(), "home")
	writeTestFile(t, filepath.Join(home, "go.mod"), "module github.com/gastownhall/gascity\n\ngo 1.23\n")

	script, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "add-testenv-import.go"))
	if err != nil {
		t.Fatalf("read production generator: %v", err)
	}
	writeTestFile(t, filepath.Join(home, "scripts", "add-testenv-import.go"), string(script))

	writeTestFile(t, filepath.Join(home, "internal", "testenv", "testenv.go"),
		"package testenv\n\n// Scrub stands in for the real env scrub.\nfunc Scrub() {}\n")
	writeTestFile(t, filepath.Join(home, "internal", "testenv", "testenv_test.go"),
		"package testenv_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/gastownhall/gascity/internal/testenv\"\n)\n\nfunc TestScrub(t *testing.T) { testenv.Scrub() }\n")
	writeTestFile(t, filepath.Join(home, "internal", "foo", "foo_test.go"),
		"package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {}\n")
	writeTestFile(t, filepath.Join(home, "test", "fixtures", "internal", "testenv", "decoy_test.go"),
		"package testenv\n\nimport \"testing\"\n\nfunc TestDecoy(t *testing.T) {}\n")

	runGit(t, home, "init", "-b", "main")
	runGit(t, home, "add", ".")
	runGit(t, home, "commit", "-q", "-m", "fixture")
	runGit(t, home, "worktree", "add", "-q", "-b", "polecat/gascity-nested",
		filepath.Join("worktrees", "gascity-nested"), "main")

	return home
}

// runGit runs git in dir with user and repository configuration isolated from
// the developer's own, so the fixture is identical everywhere.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := testCommand("git", args...)
	cmd.Dir = dir
	cmd.Env = append(
		os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=gascity", "GIT_AUTHOR_EMAIL=gascity@example.com",
		"GIT_COMMITTER_NAME=gascity", "GIT_COMMITTER_EMAIL=gascity@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// runTestenvGenerator runs the generator exactly as its usage line documents,
// from the given module root, and returns its combined output.
func runTestenvGenerator(t *testing.T, dir string) string {
	t.Helper()
	cmd := testCommand("go", "run", filepath.Join("scripts", "add-testenv-import.go"))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run scripts/add-testenv-import.go in %s: %v\n%s", dir, err, out)
	}
	return string(out)
}
