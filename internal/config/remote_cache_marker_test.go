package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testMarkerCommit      = "abcdef1234567890abcdef1234567890abcdef12"
	testMarkerOtherCommit = "1234567890abcdef1234567890abcdef12345678"
)

// newValidatedRemoteCache fabricates a git-backed remote pack cache checkout
// for testMarkerCommit (a .git directory with an index and a detached HEAD,
// plus tracked content at two depths) and returns its root plus a counter of
// stubbed git executions.
func newValidatedRemoteCache(t *testing.T) (cacheRoot, cacheDir string, gitCalls *int) {
	t.Helper()
	const commit = testMarkerCommit
	ResetRemoteCacheValidationCache()
	t.Cleanup(ResetRemoteCacheValidationCache)

	cacheRoot = t.TempDir()
	cacheDir = filepath.Join(cacheRoot, "repo")
	mustMkdirAll(t, filepath.Join(cacheDir, ".git"), 0o755)
	mustMkdirAll(t, filepath.Join(cacheDir, "agents"), 0o755)
	writeTestFile(t, cacheDir, filepath.Join(".git", "index"), "idx")
	writeTestFile(t, cacheDir, filepath.Join(".git", "HEAD"), commit+"\n")
	writeTestFile(t, cacheDir, "pack.toml", "[pack]\nname = \"remote\"\nschema = 1\n")
	writeTestFile(t, cacheDir, filepath.Join("agents", "builder.md"), "builder\n")

	calls := 0
	gitCalls = &calls
	orig := runRepoCacheGit
	runRepoCacheGit = func(_ string, args ...string) (string, error) {
		calls++
		if len(args) > 0 && args[0] == "rev-parse" {
			return commit + "\n", nil
		}
		return "", nil // status --porcelain: clean worktree
	}
	t.Cleanup(func() { runRepoCacheGit = orig })
	return cacheRoot, cacheDir, gitCalls
}

const testMarkerSource = "git@github.com:example/pack"

// TestRemoteCacheMarker_SkipsGitInALaterProcess is the point of the marker: a
// one-shot gc invocation gets no value from the in-process memo, so it paid two
// git execs against the cached checkout on every single config load
// (gascity-7qmu). ResetRemoteCacheValidationCache stands in for the new
// process.
func TestRemoteCacheMarker_SkipsGitInALaterProcess(t *testing.T) {
	cacheRoot, cacheDir, gitCalls := newValidatedRemoteCache(t)

	if err := validateInstalledRemoteCacheLocked(testMarkerSource, cacheRoot, cacheDir, testMarkerCommit); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	first := *gitCalls
	if first == 0 {
		t.Fatal("first validation should run git (rev-parse + status)")
	}

	ResetRemoteCacheValidationCache()
	if err := validateInstalledRemoteCacheLocked(testMarkerSource, cacheRoot, cacheDir, testMarkerCommit); err != nil {
		t.Fatalf("validate in a fresh process: %v", err)
	}
	if *gitCalls != first {
		t.Fatalf("fresh process re-ran git (%d→%d); want the recorded validation reused", first, *gitCalls)
	}
}

// TestRemoteCacheMarker_RejectsWorktreeEdit is the correctness half: the marker
// replaces `git status --porcelain`, so an edit anywhere in the checkout must
// still reach git. The edit is deliberately nested, which the root+index
// fingerprint alone cannot see.
func TestRemoteCacheMarker_RejectsWorktreeEdit(t *testing.T) {
	cacheRoot, cacheDir, gitCalls := newValidatedRemoteCache(t)

	if err := validateInstalledRemoteCacheLocked(testMarkerSource, cacheRoot, cacheDir, testMarkerCommit); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	first := *gitCalls

	writeTestFile(t, cacheDir, filepath.Join("agents", "builder.md"), "builder edited in place\n")
	ResetRemoteCacheValidationCache()
	if err := validateInstalledRemoteCacheLocked(testMarkerSource, cacheRoot, cacheDir, testMarkerCommit); err != nil {
		t.Fatalf("validate after edit: %v", err)
	}
	if *gitCalls == first {
		t.Fatal("a nested worktree edit did not re-run git; the marker is trusting a changed tree")
	}
}

// TestRemoteCacheMarker_RejectsNewUntrackedFile covers the other half of what
// `git status --porcelain` reports: a file that appears in the checkout.
func TestRemoteCacheMarker_RejectsNewUntrackedFile(t *testing.T) {
	cacheRoot, cacheDir, gitCalls := newValidatedRemoteCache(t)

	if err := validateInstalledRemoteCacheLocked(testMarkerSource, cacheRoot, cacheDir, testMarkerCommit); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	first := *gitCalls

	writeTestFile(t, cacheDir, filepath.Join("agents", "stowaway.md"), "not from the pack\n")
	ResetRemoteCacheValidationCache()
	if err := validateInstalledRemoteCacheLocked(testMarkerSource, cacheRoot, cacheDir, testMarkerCommit); err != nil {
		t.Fatalf("validate after new file: %v", err)
	}
	if *gitCalls == first {
		t.Fatal("a new file in the checkout did not re-run git")
	}
}

// TestRemoteCacheMarker_RejectsMovedHead covers the case the tree fingerprint
// cannot: HEAD moved to another commit while index and worktree stayed put.
func TestRemoteCacheMarker_RejectsMovedHead(t *testing.T) {
	cacheRoot, cacheDir, gitCalls := newValidatedRemoteCache(t)

	if err := validateInstalledRemoteCacheLocked(testMarkerSource, cacheRoot, cacheDir, testMarkerCommit); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	first := *gitCalls

	writeTestFile(t, cacheDir, filepath.Join(".git", "HEAD"), testMarkerOtherCommit+"\n")
	ResetRemoteCacheValidationCache()
	if err := validateInstalledRemoteCacheLocked(testMarkerSource, cacheRoot, cacheDir, testMarkerCommit); err != nil {
		t.Fatalf("validate after HEAD move: %v", err)
	}
	if *gitCalls == first {
		t.Fatal("a moved HEAD did not re-run git")
	}
}

// TestRemoteCacheMarker_RejectsSymbolicHead keeps the fast path narrow: a
// checkout whose HEAD is a symbolic ref cannot be resolved by reading one file,
// so it must fall through to git rather than be trusted.
func TestRemoteCacheMarker_RejectsSymbolicHead(t *testing.T) {
	cacheRoot, cacheDir, gitCalls := newValidatedRemoteCache(t)

	if err := validateInstalledRemoteCacheLocked(testMarkerSource, cacheRoot, cacheDir, testMarkerCommit); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	first := *gitCalls

	writeTestFile(t, cacheDir, filepath.Join(".git", "HEAD"), "ref: refs/heads/main\n")
	ResetRemoteCacheValidationCache()
	if err := validateInstalledRemoteCacheLocked(testMarkerSource, cacheRoot, cacheDir, testMarkerCommit); err != nil {
		t.Fatalf("validate with symbolic HEAD: %v", err)
	}
	if *gitCalls == first {
		t.Fatal("a symbolic HEAD did not re-run git")
	}
}

// TestRemoteCacheMarker_RejectsOtherCommit stops a marker recorded for one
// pinned commit from answering for another.
func TestRemoteCacheMarker_RejectsOtherCommit(t *testing.T) {
	cacheRoot, cacheDir, gitCalls := newValidatedRemoteCache(t)

	if err := validateInstalledRemoteCacheLocked(testMarkerSource, cacheRoot, cacheDir, testMarkerCommit); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	first := *gitCalls

	ResetRemoteCacheValidationCache()
	// HEAD still holds testMarkerCommit, so validating the OTHER commit must
	// reach git — which the stub reports as a mismatch.
	if err := validateInstalledRemoteCacheLocked(testMarkerSource, cacheRoot, cacheDir, testMarkerOtherCommit); err == nil {
		t.Fatal("validating a different commit against this checkout should fail")
	}
	if *gitCalls == first {
		t.Fatal("a different commit did not re-run git")
	}
}

// TestRemoteCacheMarker_SurvivesGitDirChurn pins the interaction that the
// in-process memo's fingerprint was already shaped around: `git status
// --porcelain` creates and removes a lock file under .git on every run, so
// neither fingerprint may look at the .git directory's own mtime.
func TestRemoteCacheMarker_SurvivesGitDirChurn(t *testing.T) {
	cacheRoot, cacheDir, gitCalls := newValidatedRemoteCache(t)

	if err := validateInstalledRemoteCacheLocked(testMarkerSource, cacheRoot, cacheDir, testMarkerCommit); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	first := *gitCalls

	writeTestFile(t, cacheDir, filepath.Join(".git", "index.lock"), "x")
	if err := os.Remove(filepath.Join(cacheDir, ".git", "index.lock")); err != nil {
		t.Fatal(err)
	}
	ResetRemoteCacheValidationCache()
	if err := validateInstalledRemoteCacheLocked(testMarkerSource, cacheRoot, cacheDir, testMarkerCommit); err != nil {
		t.Fatalf("validate after .git churn: %v", err)
	}
	if *gitCalls != first {
		t.Fatalf(".git churn busted the marker (%d→%d); want the recorded validation reused", first, *gitCalls)
	}
}

// TestRemoteCacheMarker_NotWrittenForBundledSources keeps this mechanism off
// the synthetic bundled caches. Those already have their own fast path, and
// their validator compares the materialized tree byte-for-byte against the
// binary's embedded content — a stray marker file inside one reads as drift.
func TestRemoteCacheMarker_NotWrittenForBundledSources(t *testing.T) {
	const source = "https://github.com/gastownhall/gascity.git//internal/bootstrap/packs/core"
	commit := strings.TrimPrefix(BundledPackImportVersion, "sha:")
	if !IsBundledSourceAtCanonicalPin(source, commit) {
		t.Fatalf("%s@%s is no longer a bundled source at its canonical pin; pick another fixture", source, commit)
	}

	cacheRoot := t.TempDir()
	cacheDir := filepath.Join(cacheRoot, "bundled")
	mustMkdirAll(t, filepath.Join(cacheDir, ".git"), 0o755)
	writeTestFile(t, cacheDir, filepath.Join(".git", "index"), "idx")
	writeTestFile(t, cacheDir, filepath.Join(".git", "HEAD"), commit+"\n")
	writeTestFile(t, cacheDir, "pack.toml", "[pack]\nname = \"core\"\nschema = 1\n")

	ResetRemoteCacheValidationCache()
	t.Cleanup(ResetRemoteCacheValidationCache)
	orig := runRepoCacheGit
	runRepoCacheGit = func(_ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			return commit + "\n", nil
		}
		return "", nil
	}
	t.Cleanup(func() { runRepoCacheGit = orig })

	// The synthetic marker is absent, so the bundled branch falls through to
	// the ordinary git contract; what matters is that neither outcome leaves a
	// validation marker inside a synthetic cache directory.
	_ = validateInstalledRemoteCacheLocked(source, cacheRoot, cacheDir, commit)
	if _, err := os.Stat(remoteCacheValidationMarkerPath(cacheDir)); !os.IsNotExist(err) {
		t.Fatalf("bundled source wrote a validation marker (stat err = %v); want none", err)
	}
}
