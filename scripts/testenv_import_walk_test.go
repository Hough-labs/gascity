package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestAddTestenvImportSkipsNestedWorktrees pins the fix for gascity-ru8b.
//
// Gastown checks every per-bead polecat worktree out INSIDE the polecat's home
// worktree, so `go run scripts/add-testenv-import.go` from a home used to walk
// the sibling worktrees as ordinary subdirectories. In each one it wrote a
// testenv_import_test.go that made internal/testenv import itself and scrubbed
// the legitimate testenv import out of that package's own tests — two separate
// ways of not compiling, in trees the caller does not own. Six worktrees were
// contaminated in gascity on 2026-08-26 before anyone noticed, and because the
// droppings never self-clean they parked the witness reaper's dirty-worktree
// gate on five closed beads permanently.
//
// The generator must treat a nested repository or module as a hard boundary and
// prune the walk there, while still doing its real job in the tree it owns.
func TestAddTestenvImportSkipsNestedWorktrees(t *testing.T) {
	home := newTestenvGeneratorHome(t)
	nested := filepath.Join(home, "worktrees", "gascity-nested")

	before := hashTree(t, nested)
	out := runTestenvGenerator(t, home)
	after := hashTree(t, nested)

	if diff := treeDiff(before, after); diff != "" {
		t.Errorf("generator rewrote the nested worktree it does not own:\n%s\ngenerator output:\n%s", diff, out)
	}
}

// TestAddTestenvImportStillGeneratesInItsOwnTree guards the other half of the
// nested-worktree fix: pruning must not cost the generator its real job in the
// module it was run for.
func TestAddTestenvImportStillGeneratesInItsOwnTree(t *testing.T) {
	home := newTestenvGeneratorHome(t)
	out := runTestenvGenerator(t, home)

	generated := filepath.Join(home, "internal", "foo", "testenv_import_test.go")
	body, err := os.ReadFile(generated)
	if err != nil {
		t.Fatalf("generator did not write %s: %v\ngenerator output:\n%s", generated, err, out)
	}
	if !strings.Contains(string(body), `_ "github.com/gastownhall/gascity/internal/testenv"`) {
		t.Errorf("generated file does not blank-import testenv:\n%s", body)
	}
}

// TestAddTestenvImportSelfSkipIsPackageIdentity pins the self-skip to the
// identity of the package that would end up importing itself, not to a path
// fragment. A guard that matches the suffix "internal/testenv" would wrongly
// skip an unrelated package that merely lives at a path ending that way, and a
// guard that matches the root-relative path is the bug this bead fixes. Only
// resolving the import path to a directory gets both cases right.
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

// hashTree fingerprints every tracked-looking file under root, ignoring git's
// own bookkeeping, so a caller can prove a tree is byte-identical.
func hashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == ".git" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		tree[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("hash tree %s: %v", root, err)
	}
	return tree
}

// treeDiff renders the added, removed, and modified paths between two
// fingerprints, or "" when they match.
func treeDiff(before, after map[string]string) string {
	paths := map[string]bool{}
	for path := range before {
		paths[path] = true
	}
	for path := range after {
		paths[path] = true
	}
	names := make([]string, 0, len(paths))
	for path := range paths {
		names = append(names, path)
	}
	sort.Strings(names)

	var diff []string
	for _, path := range names {
		was, hadWas := before[path]
		is, hadIs := after[path]
		switch {
		case !hadWas:
			diff = append(diff, "  added:    "+path)
		case !hadIs:
			diff = append(diff, "  removed:  "+path)
		case was != is:
			diff = append(diff, "  modified: "+path)
		}
	}
	return strings.Join(diff, "\n")
}
