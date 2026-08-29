package packman

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
)

// corePackTomlRel is a file every synthetic bundled-pack cache materializes.
const corePackTomlRel = "internal/bootstrap/packs/core/pack.toml"

// The bundled-pack cache validation used on the per-invocation readiness path
// short-circuits on a stat fingerprint (gascity-i7v), which is deliberately
// blind to a tamper that preserves size, mode and modification time. The
// integrity command must not inherit that blind spot: `gc import check` exists
// precisely to tell an operator whether the cache on disk is trustworthy.
func TestCheckInstalledRejectsStatPreservingTamperInBundledSyntheticCache(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	source := builtinpacks.MustSource("gastown")
	commit := canonicalBundledCommit(source)
	writeTestLockfile(t, city, map[string]LockedPack{
		source: {Version: "sha:" + commit, Commit: commit},
	})
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := builtinpacks.MaterializeSyntheticRepo(cachePath, commit); err != nil {
		t.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	tamperCachedFilePreservingStat(t, filepath.Join(cachePath, filepath.FromSlash(corePackTomlRel)))

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:gastown": {Source: source, Version: "sha:" + commit},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "invalid-synthetic-cache")
}

// The write-locked repair path re-reads content for the same reason: it is the
// path that heals a corrupt cache, so trusting the stat gate there would let a
// stat-preserving tamper survive every repair.
func TestEnsureRepoInCacheRehydratesStatPreservingTamper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	source := builtinpacks.MustSource("gastown")
	commit := canonicalBundledCommit(source)
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := builtinpacks.MaterializeSyntheticRepo(cachePath, commit); err != nil {
		t.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	target := filepath.Join(cachePath, filepath.FromSlash(corePackTomlRel))
	want, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", target, err)
	}
	tamperCachedFilePreservingStat(t, target)

	if _, err := EnsureRepoInCache("", source, commit); err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(rehydrated %q): %v", target, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the repair path left a stat-preserving tamper in the bundled pack cache")
	}
}

// tamperCachedFilePreservingStat rewrites one byte of path while restoring its
// size, mode and modification time, so only a content read can tell it changed.
func tamperCachedFilePreservingStat(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if len(original) == 0 {
		t.Fatalf("%q is empty; the test needs content to alter", path)
	}
	tampered := append([]byte(nil), original...)
	tampered[0] ^= 0xff
	if err := os.WriteFile(path, tampered, info.Mode().Perm()); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("Chtimes(%q): %v", path, err)
	}
}
