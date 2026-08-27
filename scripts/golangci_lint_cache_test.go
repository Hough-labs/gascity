package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wantGolangciLintCacheRelDir is the repo-relative golangci-lint cache
// directory every lint gate must use. It matches the path CI already pins
// via GOLANGCI_LINT_CACHE in .github/workflows/{ci,mac-regression}.yml, and
// it is covered by the ".cache/" entry in .gitignore.
const wantGolangciLintCacheRelDir = ".cache/golangci-lint"

// TestMakefileGolangciLintCacheIsWorktreeScoped guards gascity-p1l: the lint
// gate must not fall back to golangci-lint's per-user default cache
// (~/Library/Caches/golangci-lint on darwin), which every git worktree on the
// box shares.
//
// golangci-lint deliberately keys cache entries on a *worktree-independent*
// identity -- internal/cache.computePkgHash rewrites each file's absolute
// name to "<module path><path relative to the module dir>" before hashing it
// with the file content -- so two worktrees of this repo holding identical
// content for a package resolve to the SAME cache entry. What is stored under
// that key is the analyzers' issues including result.Issue.Pos, whose
// Filename is the *absolute* path in whichever worktree populated the entry
// (pkg/goanalysis.saveIssuesToCache). //nolint suppression is applied after
// the cache load, by reading the file named in Pos -- so once the populating
// worktree drifts, the replayed issues are attributed to a foreign path whose
// nolint directives no longer line up, and the gate reports findings that do
// not exist in the tree being linted.
//
// The fix has to hold for every worktree with no action from the caller, so
// this asserts on the value a recipe actually sees in its environment (i.e.
// that the Makefile both defines AND exports it), not merely that the
// Makefile mentions the variable.
func TestMakefileGolangciLintCacheIsWorktreeScoped(t *testing.T) {
	repo := repoRoot(t)
	got := runMakefileGolangciLintCachePrintTarget(t, nil)

	if got == "" {
		t.Fatal("GOLANGCI_LINT_CACHE is unset in the recipe environment; lint gates fall back to the per-user cache shared by every worktree on this machine")
	}
	want := filepath.Join(repo, wantGolangciLintCacheRelDir)
	if got != want {
		t.Fatalf("GOLANGCI_LINT_CACHE = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, repo+string(filepath.Separator)) {
		t.Fatalf("GOLANGCI_LINT_CACHE = %q is outside this worktree (%q); it would still be shared with sibling worktrees", got, repo)
	}
}

// TestMakefileGolangciLintCacheRespectsCallerSuppliedValue guards the other
// half of the default: CI pins GOLANGCI_LINT_CACHE explicitly so it can
// restore/save the directory with actions/cache, and a developer or gate
// script may point it somewhere else deliberately. The Makefile default must
// fill in only when the caller supplied nothing.
func TestMakefileGolangciLintCacheRespectsCallerSuppliedValue(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "caller-lint-cache")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatalf("mkdir custom GOLANGCI_LINT_CACHE: %v", err)
	}
	got := runMakefileGolangciLintCachePrintTarget(t, []string{"GOLANGCI_LINT_CACHE=" + custom})
	if got != custom {
		t.Fatalf("GOLANGCI_LINT_CACHE = %q, want caller-supplied %q", got, custom)
	}
}

func runMakefileGolangciLintCachePrintTarget(t *testing.T, extraEnv []string) string {
	t.Helper()
	repo := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(repo, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	tmp := t.TempDir()
	testMakefile := filepath.Join(tmp, "Makefile")
	content := string(makefile) + `
.PHONY: print-golangci-lint-cache
print-golangci-lint-cache:
	@sh -c 'echo GOLANGCI_LINT_CACHE=$$GOLANGCI_LINT_CACHE'
`
	if err := os.WriteFile(testMakefile, []byte(content), 0o644); err != nil {
		t.Fatalf("write test Makefile: %v", err)
	}

	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"USER=" + os.Getenv("USER"),
		"SHELL=/bin/sh",
	}
	env = append(env, extraEnv...)

	cmd := makeCommand("--no-print-directory", "-f", testMakefile, "print-golangci-lint-cache")
	cmd.Dir = repo
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make print-golangci-lint-cache failed: %v\n%s", err, out)
	}
	line := strings.TrimSpace(string(out))
	const prefix = "GOLANGCI_LINT_CACHE="
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("unexpected output from print-golangci-lint-cache: %q", line)
	}
	return strings.TrimPrefix(line, prefix)
}

// TestGolangciLintCacheDirIsGitignored guards the other half of the
// gascity-p1l fix. Scoping the cache to $(CURDIR) puts it *inside* the
// worktree, so it only stays invisible while the path is ignored: a cold
// `make lint` on this repo writes ~50MB there, and if it were tracked every
// lint run would leave the worktree dirty and trip the clean-tree checks the
// commit and handoff gates run. Assert it rather than relying on a one-time
// manual check -- this rig has a live history of runtime artifacts landing in
// un-ignored paths.
func TestGolangciLintCacheDirIsGitignored(t *testing.T) {
	repo := repoRoot(t)
	cmd := testCommand("git", "check-ignore", "-q", filepath.Join(repo, wantGolangciLintCacheRelDir))
	cmd.Dir = repo
	// git check-ignore exits 0 when the path is ignored, 1 when it is not.
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s is not gitignored (git check-ignore: %v); every `make lint` would leave the worktree dirty", wantGolangciLintCacheRelDir, err)
	}
}
