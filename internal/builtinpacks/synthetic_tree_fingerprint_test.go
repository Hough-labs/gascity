package builtinpacks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// corePackTomlRel is a materialized file every synthetic repo contains; the
// fingerprint tests tamper with it because it is small and always present.
const corePackTomlRel = "internal/bootstrap/packs/core/pack.toml"

// TestValidateSyntheticRepoSkipsContentReadWhenTreeFingerprintMatches pins the
// change-detection gate that keeps the per-invocation readiness pass off the
// full content comparison (gascity-i7v).
//
// MaterializeSyntheticRepo records a stat-only fingerprint of the tree it just
// wrote byte-for-byte. When a later validation recomputes that fingerprint and
// it still matches, the tree has not been touched since it was verified, so
// ValidateSyntheticRepo accepts it without re-reading every cached file.
//
// This test proves the content read is actually skipped, and therefore also
// documents the gate's blind spot: a tamper that preserves size, mode AND
// modification time is not detected here. Only `gc import check` (and the
// write-locked repair path) re-read content unconditionally.
func TestValidateSyntheticRepoSkipsContentReadWhenTreeFingerprintMatches(t *testing.T) {
	dst := materializeTestRepo(t)
	target := filepath.Join(dst, corePackTomlRel)

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(%q): %v", target, err)
	}
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", target, err)
	}
	if len(original) == 0 {
		t.Fatalf("materialized %s is empty; the test needs content to alter", corePackTomlRel)
	}

	// Same length, same mode, same mtime — only the bytes differ.
	tampered := append([]byte(nil), original...)
	tampered[0] ^= 0xff
	if err := os.WriteFile(target, tampered, info.Mode().Perm()); err != nil {
		t.Fatalf("WriteFile(%q): %v", target, err)
	}
	if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("Chtimes(%q): %v", target, err)
	}

	if err := ValidateSyntheticRepo(dst, testCommit); err != nil {
		t.Fatalf("ValidateSyntheticRepo re-read file content for an unchanged tree fingerprint: %v", err)
	}
}

// A tamper that keeps the file's size but not its modification time must still
// be caught: the fingerprint covers mtime precisely so an ordinary in-place
// rewrite — the shape TestEnsureBuiltinRuntimeAssetsRehydratesCorruptedCache
// relies on — always falls through to the full content comparison.
func TestValidateSyntheticRepoRejectsSameSizeTamperThatChangesModTime(t *testing.T) {
	dst := materializeTestRepo(t)
	target := filepath.Join(dst, corePackTomlRel)

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(%q): %v", target, err)
	}
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", target, err)
	}
	tampered := append([]byte(nil), original...)
	tampered[0] ^= 0xff
	if err := os.WriteFile(target, tampered, info.Mode().Perm()); err != nil {
		t.Fatalf("WriteFile(%q): %v", target, err)
	}
	if err := os.Chtimes(target, info.ModTime(), info.ModTime().Add(time.Second)); err != nil {
		t.Fatalf("Chtimes(%q): %v", target, err)
	}

	err = ValidateSyntheticRepo(dst, testCommit)
	if err == nil {
		t.Fatal("ValidateSyntheticRepo accepted a same-size tamper with a changed mtime")
	}
	if !strings.Contains(err.Error(), "content differs") {
		t.Fatalf("error = %v, want content differs", err)
	}
}

// A marker written before the fingerprint field existed carries no fingerprint.
// Validation must fall back to the full comparison rather than accepting the
// tree unverified.
func TestValidateSyntheticRepoFallsBackWhenMarkerHasNoFingerprint(t *testing.T) {
	dst := materializeTestRepo(t)
	stripSyntheticTreeFingerprint(t, dst)

	if err := ValidateSyntheticRepo(dst, testCommit); err != nil {
		t.Fatalf("ValidateSyntheticRepo on an intact legacy-marker cache: %v", err)
	}

	writeFile(t, filepath.Join(dst, corePackTomlRel), "[pack]\nname = \"tampered\"\nschema = 1\n")
	stripSyntheticTreeFingerprint(t, dst)
	err := ValidateSyntheticRepo(dst, testCommit)
	if err == nil {
		t.Fatal("ValidateSyntheticRepo accepted tampered content under a legacy marker")
	}
	if !strings.Contains(err.Error(), "content differs") {
		t.Fatalf("error = %v, want content differs", err)
	}
}

// StampSyntheticTreeFingerprint backfills the fingerprint onto a cache that a
// previous gc build materialized, so existing on-disk caches get the cheap gate
// without waiting to be re-materialized.
func TestStampSyntheticTreeFingerprintBackfillsLegacyMarker(t *testing.T) {
	dst := materializeTestRepo(t)
	stripSyntheticTreeFingerprint(t, dst)

	if SyntheticTreeFingerprintCurrent(dst) {
		t.Fatal("a stripped marker reports a current fingerprint")
	}
	if err := StampSyntheticTreeFingerprint(dst, testCommit); err != nil {
		t.Fatalf("StampSyntheticTreeFingerprint: %v", err)
	}
	if !SyntheticTreeFingerprintCurrent(dst) {
		t.Fatal("fingerprint is not current after stamping")
	}
	if err := ValidateSyntheticRepo(dst, testCommit); err != nil {
		t.Fatalf("ValidateSyntheticRepo after stamping: %v", err)
	}

	// The stamp must not have disturbed the marker's other fields.
	if err := ValidateSyntheticRepoFast(dst, testCommit); err != nil {
		t.Fatalf("ValidateSyntheticRepoFast after stamping: %v", err)
	}
}

// Stamping refuses a tree that does not currently validate, so a corrupted
// cache can never be blessed into the cheap path.
func TestStampSyntheticTreeFingerprintRefusesInvalidTree(t *testing.T) {
	dst := materializeTestRepo(t)
	writeFile(t, filepath.Join(dst, corePackTomlRel), "[pack]\nname = \"tampered\"\nschema = 1\n")
	stripSyntheticTreeFingerprint(t, dst)

	if err := StampSyntheticTreeFingerprint(dst, testCommit); err == nil {
		t.Fatal("StampSyntheticTreeFingerprint blessed a tampered tree")
	}
}

// The fingerprint must not be satisfied by a file that was deleted outright.
func TestValidateSyntheticRepoRejectsDeletedFileWithFingerprintMarker(t *testing.T) {
	dst := materializeTestRepo(t)
	target := filepath.Join(dst, corePackTomlRel)
	if err := os.Remove(target); err != nil {
		t.Fatalf("Remove(%q): %v", target, err)
	}

	if err := ValidateSyntheticRepo(dst, testCommit); err == nil {
		t.Fatal("ValidateSyntheticRepo accepted a cache with a deleted pack file")
	}
}

// stripSyntheticTreeFingerprint rewrites dir's marker without the fingerprint
// field, standing in for a cache materialized by a gc build that predates it.
func stripSyntheticTreeFingerprint(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, syntheticMarkerFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), syntheticTreeFingerprintTOMLKey+" ") {
			continue
		}
		kept = append(kept, line)
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

// BenchmarkValidateSyntheticRepoWarm measures the per-invocation cost the
// readiness pass pays for an untouched cache — the exact call every gc
// invocation makes before it reaches the beads store (gascity-i7v).
func BenchmarkValidateSyntheticRepoWarm(b *testing.B) {
	dst := filepath.Join(b.TempDir(), "cache")
	if err := MaterializeSyntheticRepo(dst, testCommit); err != nil {
		b.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ValidateSyntheticRepo(dst, testCommit); err != nil {
			b.Fatalf("ValidateSyntheticRepo: %v", err)
		}
	}
}

// BenchmarkValidateSyntheticRepoFullComparison measures the same call with the
// fingerprint gate defeated, i.e. the cost this change removes from the hot
// path. Read the two together.
func BenchmarkValidateSyntheticRepoFullComparison(b *testing.B) {
	dst := filepath.Join(b.TempDir(), "cache")
	if err := MaterializeSyntheticRepo(dst, testCommit); err != nil {
		b.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := validateSyntheticRepoContents(dst); err != nil {
			b.Fatalf("validateSyntheticRepoContents: %v", err)
		}
	}
}
