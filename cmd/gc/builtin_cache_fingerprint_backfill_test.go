package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/packman"
)

// The readiness pass runs on every gc invocation and used to re-read every
// file of every cached bundled pack to decide the cache was fine
// (gascity-i7v). builtinpacks now records a stat-only fingerprint so that
// check is cheap, but a cache materialized by an earlier gc build carries no
// fingerprint and would pay the full comparison forever. The ensure pass
// backfills it on the first invocation after the upgrade.
func TestEnsureRequiredBuiltinSourcesCachedBackfillsTreeFingerprint(t *testing.T) {
	clearGCEnv(t) // isolated GC_HOME so the backfill never touches the shared test cache
	cityPath := t.TempDir()
	materializeBuiltinPacksForTest(t, cityPath)

	cachePaths := requiredBuiltinSourceCachePathsForTest(t, cityPath)
	if len(cachePaths) == 0 {
		t.Fatal("no required builtin source caches resolved")
	}
	for _, cachePath := range cachePaths {
		stripSyntheticTreeFingerprintFile(t, cachePath)
		if builtinpacks.SyntheticTreeFingerprintCurrent(cachePath) {
			t.Fatalf("%s still reports a current fingerprint after stripping", cachePath)
		}
	}

	if err := ensureRequiredBuiltinSourcesCached(cityPath); err != nil {
		t.Fatalf("ensureRequiredBuiltinSourcesCached: %v", err)
	}

	for _, cachePath := range cachePaths {
		if !builtinpacks.SyntheticTreeFingerprintCurrent(cachePath) {
			t.Fatalf("%s was not backfilled with a tree fingerprint; every later gc "+
				"invocation keeps paying the full content comparison", cachePath)
		}
	}
}

// A cache whose content no longer matches the binary must not be blessed with
// a fingerprint — it has to be re-hydrated instead, exactly as before.
func TestEnsureRequiredBuiltinSourcesCachedRehydratesInsteadOfStampingCorruption(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	materializeBuiltinPacksForTest(t, cityPath)

	target := bundledGcBeadsBdScriptForTest(t)
	want, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", target, err)
	}
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho corrupted\n"), 0o755); err != nil {
		t.Fatalf("corrupting cached script: %v", err)
	}

	if err := ensureRequiredBuiltinSourcesCached(cityPath); err != nil {
		t.Fatalf("ensureRequiredBuiltinSourcesCached after corruption: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(rehydrated script): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("corrupted cached script was not rehydrated; got:\n%s", got)
	}
}

// requiredBuiltinSourceCachePathsForTest resolves the cache directory of every
// required bundled source for cityPath.
func requiredBuiltinSourceCachePathsForTest(t *testing.T, cityPath string) []string {
	t.Helper()
	commit := bundledPackImportCommit()
	var paths []string
	for name, source := range requiredBuiltinSources(cityPath) {
		cachePath, err := packman.RepoCachePath(source, commit)
		if err != nil {
			t.Fatalf("RepoCachePath(%s): %v", name, err)
		}
		paths = append(paths, cachePath)
	}
	return paths
}

// stripSyntheticTreeFingerprintFile rewrites a cache marker without its
// fingerprint line, standing in for a cache left by an earlier gc build.
func stripSyntheticTreeFingerprintFile(t *testing.T, cachePath string) {
	t.Helper()
	path := filepath.Join(cachePath, ".gc-bundled-pack-cache.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "tree_fingerprint ") {
			continue
		}
		kept = append(kept, line)
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

// The warm readiness pass must stay lock-free once the fingerprint is current:
// the backfill is a one-time upgrade cost, not a per-invocation write.
func TestEnsureRequiredBuiltinSourcesCachedTakesNoWriteLockWhenFingerprintCurrent(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	materializeBuiltinPacksForTest(t, cityPath)

	for _, cachePath := range requiredBuiltinSourceCachePathsForTest(t, cityPath) {
		if !builtinpacks.SyntheticTreeFingerprintCurrent(cachePath) {
			t.Fatalf("%s has no current fingerprint after materialization", cachePath)
		}
	}

	root, err := packman.RepoCacheRoot()
	if err != nil {
		t.Fatalf("RepoCacheRoot: %v", err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		_, err := config.WithRepoCacheWriteLock(root, func() (string, error) {
			close(locked)
			<-release
			return "", nil
		})
		lockDone <- err
	}()
	<-locked
	defer func() {
		close(release)
		if err := <-lockDone; err != nil {
			t.Errorf("releasing repo cache write lock: %v", err)
		}
	}()

	done := make(chan error, 1)
	go func() { done <- ensureRequiredBuiltinSourcesCached(cityPath) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("warm ensureRequiredBuiltinSourcesCached: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("warm readiness pass blocked on the repo-cache write lock; " +
			"want a lock-free pass when every fingerprint is already current")
	}
}
