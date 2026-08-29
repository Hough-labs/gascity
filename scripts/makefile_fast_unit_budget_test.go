package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	if !strings.Contains(sweep, "$(UNIT_PKGS_SWEEP)") {
		t.Errorf("test sweep does not use $(UNIT_PKGS_SWEEP):\n%s", sweep)
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

// TestSweepPackageListIsSharedAndExcludesEveryShardedPackage keeps the single
// definition of "every unit package that still runs inside the sweep" shared
// between the default sweep and the Mac sweep, so the two cannot silently
// drift apart, and pins that the list is derived by subtracting the sharded
// packages from go list ./....
func TestSweepPackageListIsSharedAndExcludesEveryShardedPackage(t *testing.T) {
	makefile := readMakefile(t)

	def := sweepDefinition(t, makefile)
	if !strings.Contains(def, "go list ./...") {
		t.Errorf("UNIT_PKGS_SWEEP must enumerate packages from go list ./...: %s", def)
	}

	excluded := sweepExclusions(t, makefile)
	if !excluded["cmd/gc"] {
		t.Errorf("UNIT_PKGS_SWEEP must exclude cmd/gc: %s", def)
	}

	macRecipe := strings.Join(makeTargetRecipe(t, makefile, "test-mac"), "\n")
	if !strings.Contains(macRecipe, "$(UNIT_PKGS_SWEEP)") {
		t.Errorf("test-mac must reuse $(UNIT_PKGS_SWEEP):\n%s", macRecipe)
	}
}

// TestSweepExclusionMatchesWholeImportPathsOnly proves the exclusion filter is
// anchored on a full path segment rather than a prefix. Every sharded package
// has a sibling that must keep running inside the sweep — cmd/gc-write-mint
// beside cmd/gc, and scripts/cipolicy beneath scripts (gascity-5y4h) — and an
// unanchored alternation would silently stop testing them while every other
// guard here still passed: the sharded set and the exclusion set would still
// agree, and no shard loop would ever name the swallowed package.
//
// The pattern is read out of the Makefile and evaluated, not restated, so
// editing the alternation re-runs these cases against the new filter.
// Deliberately no subprocess: the property is string matching, and
// test/test-resources.toml ratchets subprocess call sites down.
func TestSweepExclusionMatchesWholeImportPathsOnly(t *testing.T) {
	pattern := regexp.MustCompile(sweepExclusionPattern(t, readMakefile(t)))

	const module = "github.com/gastownhall/gascity"
	for _, tc := range []struct {
		pkg  string
		kept bool
	}{
		{module + "/cmd/gc", false},
		{module + "/examples/gastown", false},
		{module + "/examples/bd/dolt", false},
		{module + "/scripts", false},
		{module + "/cmd/gc-write-mint", true},
		{module + "/scripts/cipolicy", true},
		{module + "/examples/bd", true},
		{module + "/internal/beads", true},
	} {
		if kept := !pattern.MatchString(tc.pkg); kept != tc.kept {
			t.Errorf("UNIT_PKGS_SWEEP keeps %s = %v, want %v (pattern %s)",
				tc.pkg, kept, tc.kept, pattern)
		}
	}
}

// TestOversizedPackagesLeaveTheSweepAndRunAsShards pins the fix for
// gascity-vdhw and gascity-5y4h. examples/gastown and examples/bd/dolt are
// large, effectively sequential packages that spawn a subprocess per test, so
// their cost is tests x subprocess spawn against one fixed per-binary deadline;
// measured on the Darwin gate they consumed 554-932s and 634-1136s of a 900s
// budget. scripts reaches the same cliff from a much lower baseline: a handful
// of its tests drive `make` and the real `go` tool against fixture repos, so
// its wall time tracks host contention rather than the code under test — it
// passed six times between 132s and 636s and hit the 900s wall on four
// separate occurrences.
//
// In every case that makes whether the gate passes a function of host load
// rather than of correctness, and it misattributes: the deadline panic names
// whichever test held the binary when the budget expired, so each occurrence
// accused a different innocent test. They must therefore leave the sweep and
// run through the same per-package shard runner cmd/gc uses (gascity-cgh), and
// a raised -timeout is explicitly not the remedy.
func TestOversizedPackagesLeaveTheSweepAndRunAsShards(t *testing.T) {
	makefile := readMakefile(t)

	sharded := shardedSweepPackages(t, makefile)
	for _, want := range []string{"./examples/gastown", "./examples/bd/dolt", "./scripts"} {
		if !slices.Contains(sharded, want) {
			t.Errorf("SHARDED_SWEEP_PKGS must contain %s: %v", want, sharded)
		}
	}

	if !regexp.MustCompile(`(?m)^SHARDED_UNIT_TOTAL \?= [0-9]+$`).MatchString(makefile) {
		t.Error("Makefile has no SHARDED_UNIT_TOTAL default")
	}

	for _, target := range []string{"test", "test-mac"} {
		recipe := strings.Join(makeTargetRecipe(t, makefile, target), "\n")
		for _, marker := range []string{
			"for p in $(SHARDED_SWEEP_PKGS)",
			"seq 1 $(SHARDED_UNIT_TOTAL)",
			`./scripts/test-go-test-shard "$$p" "$$s" $(SHARDED_UNIT_TOTAL)`,
			"GC_FAST_UNIT=1",
			"GO_TEST_COUNT=1",
			"GO_TEST_TIMEOUT=15m",
			"|| exit 1",
		} {
			if !strings.Contains(recipe, marker) {
				t.Errorf("%s target recipe is missing %q:\n%s", target, marker, recipe)
			}
		}
	}
}

// TestShardedPackagesAreExcludedFromTheSweep is the drift guard between
// the two places the sharded set is written: the SHARDED_SWEEP_PKGS list the
// shard loops iterate, and the go list filter that keeps those packages out of
// the sweep. A package named in one but not the other either runs twice under
// two deadlines or silently stops being tested at all.
func TestShardedPackagesAreExcludedFromTheSweep(t *testing.T) {
	makefile := readMakefile(t)

	excluded := sweepExclusions(t, makefile)
	// cmd/gc is sharded by its own loop (gascity-cgh), not by
	// SHARDED_SWEEP_PKGS, so account for it separately.
	want := map[string]bool{"cmd/gc": true}
	for _, pkg := range shardedSweepPackages(t, makefile) {
		want[strings.TrimPrefix(pkg, "./")] = true
	}

	for pkg := range want {
		if !excluded[pkg] {
			t.Errorf("%s is sharded but still swept by UNIT_PKGS_SWEEP; it would run twice, under two deadlines", pkg)
		}
	}
	for pkg := range excluded {
		if !want[pkg] {
			t.Errorf("UNIT_PKGS_SWEEP excludes %s but no shard loop covers it; it would stop being tested", pkg)
		}
	}
}

// TestMacGateTakesExactlyOneSlotForSweepAndShards pins the property the Mac
// lane was built around: test-mac is what agents run as their configured
// test_command, and scripts/gate-slot-run acquires non-blockingly, so every
// extra acquire is another chance to abort a gate that is already running
// after work has been done. Sweep and shards therefore share one acquire.
func TestMacGateTakesExactlyOneSlotForSweepAndShards(t *testing.T) {
	makefile := readMakefile(t)
	recipe := strings.Join(makeTargetRecipe(t, makefile, "test-mac"), "\n")

	if got := strings.Count(recipe, "scripts/gate-slot-run"); got != 1 {
		t.Errorf("test-mac must take exactly one gate slot, found %d acquisitions:\n%s", got, recipe)
	}
	if !strings.Contains(recipe, "./scripts/test-go-test-shard") {
		t.Errorf("test-mac must cover the sharded packages itself; nothing else runs them on Darwin:\n%s", recipe)
	}
}

// TestShardedPackagesMatchTheLocalParallelLane is the drift guard between the
// Makefile's sharded set and scripts/test-local-parallel's own copy of it.
// make cannot source a shell array and the script cannot expand a make
// variable, so the list is genuinely written twice; the failure it prevents is
// silent in both directions. A package sharded only in the Makefile still runs
// inside test-fast-parallel's single unit-core binary — the exact per-binary
// deadline this bead is about, against go's 10m default rather than 15m. A
// package excluded only in the script stops being tested by that lane at all.
func TestShardedPackagesMatchTheLocalParallelLane(t *testing.T) {
	makefile := readMakefile(t)

	fromMakefile := map[string]bool{"cmd/gc": true}
	for _, pkg := range shardedSweepPackages(t, makefile) {
		fromMakefile[strings.TrimPrefix(pkg, "./")] = true
	}

	script := readRepoFile(t, filepath.Join("scripts", "test-local-parallel"))
	def := regexp.MustCompile(`(?m)^SHARDED_PKGS=\(([^)]*)\)$`).FindStringSubmatch(script)
	if len(def) != 2 {
		t.Fatal("scripts/test-local-parallel has no SHARDED_PKGS definition")
	}
	fromScript := map[string]bool{}
	for _, pkg := range strings.Fields(def[1]) {
		fromScript[strings.TrimPrefix(pkg, "./")] = true
	}

	for pkg := range fromMakefile {
		if !fromScript[pkg] {
			t.Errorf("%s is sharded in the Makefile but scripts/test-local-parallel still sweeps it into unit-core", pkg)
		}
	}
	for pkg := range fromScript {
		if !fromMakefile[pkg] {
			t.Errorf("scripts/test-local-parallel excludes %s from unit-core but the Makefile does not shard it", pkg)
		}
	}
}

// TestLocalParallelFansOutEveryShardedPackage pins that the lanes which run the
// unit sweep also run the shard jobs that replace the packages they filtered
// out. unit-core going green while two packages quietly stopped running is the
// failure this catches.
func TestLocalParallelFansOutEveryShardedPackage(t *testing.T) {
	script := readRepoFile(t, filepath.Join("scripts", "test-local-parallel"))

	if !strings.Contains(script, "add_sweep_pkg_shards() {") {
		t.Fatal("scripts/test-local-parallel defines no add_sweep_pkg_shards")
	}
	if !strings.Contains(script, "./scripts/test-go-test-shard ${pkg} ${i} ${SHARD_TOTAL}") {
		t.Error("add_sweep_pkg_shards must drive scripts/test-go-test-shard, the same runner cmd/gc uses")
	}

	for _, mode := range []string{"fast", "full"} {
		body := shellCaseBody(t, script, mode)
		if !strings.Contains(body, "add_unit_core_job") {
			continue // this mode does not run the sweep, so it owes no shards
		}
		if !strings.Contains(body, "add_sweep_pkg_shards") {
			t.Errorf("%s mode runs the unit-core sweep but never fans out the sharded example packages:\n%s", mode, body)
		}
		if !strings.Contains(body, "add_cmd_gc_shards") {
			t.Errorf("%s mode runs the unit-core sweep but never fans out cmd/gc:\n%s", mode, body)
		}
	}
}

// shellCaseBody returns the body of one `case` arm in a shell script, from the
// `<mode>)` label to its `;;` terminator.
func shellCaseBody(t *testing.T, script, mode string) string {
	t.Helper()
	start := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(mode) + `\)\s*$`).FindStringIndex(script)
	if start == nil {
		t.Fatalf("scripts/test-local-parallel has no %q case arm", mode)
	}
	rest := script[start[1]:]
	end := strings.Index(rest, ";;")
	if end < 0 {
		t.Fatalf("%q case arm is not terminated by ;;", mode)
	}
	return rest[:end]
}

// readRepoFile reads a repo-relative file from the module root.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(data)
}

// sweepDefinition returns the right-hand side of the UNIT_PKGS_SWEEP
// assignment.
func sweepDefinition(t *testing.T, makefile string) string {
	t.Helper()
	def := regexp.MustCompile(`(?m)^UNIT_PKGS_SWEEP = ([^\n]+)$`).FindStringSubmatch(makefile)
	if len(def) != 2 {
		t.Fatal("Makefile has no UNIT_PKGS_SWEEP definition")
	}
	return def[1]
}

// sweepExclusionPattern returns the extended regexp UNIT_PKGS_SWEEP excludes
// on, as Go regexp source. It fails closed: this models exactly
// `grep -v -E '<pattern>'`, so any other pipeline shape fails loudly here
// rather than being silently mismodelled by callers.
func sweepExclusionPattern(t *testing.T, makefile string) string {
	t.Helper()
	def := strings.TrimSpace(sweepDefinition(t, makefile))
	def = strings.TrimSuffix(strings.TrimPrefix(def, "$(shell "), ")")

	_, pipeline, found := strings.Cut(def, "go list ./...")
	if !found {
		t.Fatalf("UNIT_PKGS_SWEEP has no `go list ./...` to filter: %s", def)
	}
	pipeline = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(pipeline), "|"))

	match := regexp.MustCompile(`^grep -v -E '([^']+)'$`).FindStringSubmatch(pipeline)
	if match == nil {
		t.Fatalf("UNIT_PKGS_SWEEP filter is no longer a single `grep -v -E '<pattern>'`, "+
			"which is all this guard knows how to evaluate; teach it the new shape "+
			"rather than dropping the coverage. Got: %q", pipeline)
	}
	// Make doubles `$` for the shell; undo that so the recipe's own anchor works.
	return strings.ReplaceAll(match[1], "$$", "$")
}

// sweepExclusions returns the package suffixes UNIT_PKGS_SWEEP filters out of
// go list ./..., parsed from the grep alternation so the guard reads the real
// filter rather than a restatement of it.
func sweepExclusions(t *testing.T, makefile string) map[string]bool {
	t.Helper()
	def := sweepDefinition(t, makefile)
	alt := regexp.MustCompile(`grep -v -E '/\(([^)]*)\)\$\$'`).FindStringSubmatch(def)
	if len(alt) != 2 {
		t.Fatalf("UNIT_PKGS_SWEEP has no parseable grep -v -E '/(...)$$' exclusion: %s", def)
	}
	excluded := map[string]bool{}
	for _, pkg := range strings.Split(alt[1], "|") {
		if pkg != "" {
			excluded[pkg] = true
		}
	}
	return excluded
}

// shardedSweepPackages returns the packages listed in SHARDED_SWEEP_PKGS: the
// non-cmd/gc packages the sweep hands to the per-package shard runner.
func shardedSweepPackages(t *testing.T, makefile string) []string {
	t.Helper()
	def := regexp.MustCompile(`(?m)^SHARDED_SWEEP_PKGS = ([^\n]+)$`).FindStringSubmatch(makefile)
	if len(def) != 2 {
		t.Fatal("Makefile has no SHARDED_SWEEP_PKGS definition")
	}
	return strings.Fields(def[1])
}

func readMakefile(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	return string(data)
}
