package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// makeTargetRecipe returns the recipe lines of a Makefile target with the
// leading tab stripped. Continuation lines are returned verbatim, so callers
// that only need to match markers can join them.
func makeTargetRecipe(t *testing.T, makefile, target string) []string {
	t.Helper()
	pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `:[^\n]*\n((?:\t[^\n]*\n?)+)`)
	match := pattern.FindStringSubmatch(makefile)
	if len(match) != 2 {
		t.Fatalf("Makefile has no %s target with a recipe", target)
	}
	lines := strings.Split(strings.TrimSuffix(match[1], "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimPrefix(lines[i], "\t")
	}
	return lines
}

// TestFastUnitTargetKeepsCmdGCOutOfThePackageSweep pins the fix for
// gascity-cgh. The `cmd/gc` fast-unit suite alone consumed ~14.4 minutes of the
// sweep's 15m per-binary budget, so ordinary parallel load pushed it past the
// deadline and `make test` failed on a clean tree. `make test` must therefore
// sweep every package EXCEPT `cmd/gc`, and cover `cmd/gc` through the existing
// per-package shard runner so no single test binary carries the whole package's
// runtime against one deadline.
func TestFastUnitTargetKeepsCmdGCOutOfThePackageSweep(t *testing.T) {
	makefile := readMakefile(t)
	lines := makeTargetRecipe(t, makefile, "test")

	var sweep string
	for _, line := range lines {
		if strings.Contains(line, "scripts/go-test-observable test --") {
			sweep = line
			break
		}
	}
	if sweep == "" {
		t.Fatalf("test target has no go-test-observable sweep command:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(sweep, "./...") {
		t.Errorf("test sweep still passes ./..., which pulls cmd/gc back under the sweep's single deadline:\n%s", sweep)
	}
	if !strings.Contains(sweep, "$(UNIT_PKGS_NONCMDGC)") {
		t.Errorf("test sweep does not use $(UNIT_PKGS_NONCMDGC):\n%s", sweep)
	}
	if !strings.Contains(sweep, "GC_FAST_UNIT=1") {
		t.Errorf("test sweep must stay on the fast-unit boundary:\n%s", sweep)
	}
}

// TestFastUnitTargetRunsCmdGCAsSequentialShards proves `make test` still covers
// cmd/gc after it leaves the sweep, and that it covers it one shard at a time.
// Sequencing is load-bearing: gascity-4h5 and gascity-4nv are cmd/gc failures
// that reproduce only when several cmd/gc shards run concurrently against
// shared host state, so the pre-commit and refinery gate must never fan them out.
func TestFastUnitTargetRunsCmdGCAsSequentialShards(t *testing.T) {
	makefile := readMakefile(t)
	recipe := strings.Join(makeTargetRecipe(t, makefile, "test"), "\n")

	for _, marker := range []string{
		"seq 1 $(CMD_GC_UNIT_TOTAL)",
		"./scripts/test-go-test-shard ./cmd/gc",
		"$(CMD_GC_UNIT_TOTAL)",
		"GC_FAST_UNIT=1",
		"GO_TEST_COUNT=1",
		"GO_TEST_TIMEOUT=15m",
		"|| exit 1",
	} {
		if !strings.Contains(recipe, marker) {
			t.Errorf("test target recipe is missing %q:\n%s", marker, recipe)
		}
	}
	if strings.Contains(recipe, "&") && !strings.Contains(recipe, "&&") {
		t.Errorf("test target recipe backgrounds a command; cmd/gc shards must stay sequential:\n%s", recipe)
	}
	if !regexp.MustCompile(`(?m)^CMD_GC_UNIT_TOTAL \?= [0-9]+$`).MatchString(makefile) {
		t.Error("Makefile has no CMD_GC_UNIT_TOTAL default")
	}
}

// TestNonCmdGCUnitPackageListIsSharedAndExcludesCmdGC keeps the single
// definition of "every unit package except cmd/gc" shared between the default
// sweep and the Mac sweep, so the two cannot silently drift apart.
func TestNonCmdGCUnitPackageListIsSharedAndExcludesCmdGC(t *testing.T) {
	makefile := readMakefile(t)

	def := regexp.MustCompile(`(?m)^UNIT_PKGS_NONCMDGC = ([^\n]+)$`).FindStringSubmatch(makefile)
	if len(def) != 2 {
		t.Fatal("Makefile has no UNIT_PKGS_NONCMDGC definition")
	}
	if !strings.Contains(def[1], "go list ./...") {
		t.Errorf("UNIT_PKGS_NONCMDGC must enumerate packages from go list ./...: %s", def[1])
	}
	if !strings.Contains(def[1], `grep -v '/cmd/gc$$'`) {
		t.Errorf("UNIT_PKGS_NONCMDGC must exclude cmd/gc: %s", def[1])
	}

	macRecipe := strings.Join(makeTargetRecipe(t, makefile, "test-mac"), "\n")
	if !strings.Contains(macRecipe, "$(UNIT_PKGS_NONCMDGC)") {
		t.Errorf("test-mac must reuse $(UNIT_PKGS_NONCMDGC):\n%s", macRecipe)
	}
}

func readMakefile(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	return string(data)
}
