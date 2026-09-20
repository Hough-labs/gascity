package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readPrePushHook returns the tracked .githooks/pre-push source.
func readPrePushHook(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".githooks", "pre-push"))
	if err != nil {
		t.Fatalf("read .githooks/pre-push: %v", err)
	}
	return string(data)
}

// TestRaceLaneIsTheOnlyLaneCarryingTheDetector pins gascity-tvwi. Before it,
// the Makefile contained no -race anywhere, so `make test` / `make test-mac`
// and every sharded target ran without the detector. That is why the
// internal/api test-double races in gascity-lzzj survived indefinitely: they
// were not flaky in the gate, they were invisible to it.
//
// The guard is deliberately about the FLAG, not about which packages carry it.
// Scope is a judgement call that belongs in $(RACE_PKGS); "the gate runs the
// detector at all" is the invariant that must not silently revert.
func TestRaceLaneIsTheOnlyLaneCarryingTheDetector(t *testing.T) {
	makefile := readMakefile(t)

	recipe := strings.Join(makeTargetRecipe(t, makefile, "test-race"), " ")
	if !strings.Contains(recipe, " -race ") {
		t.Fatalf("test-race recipe does not pass -race to go test:\n%s", recipe)
	}
	if !strings.Contains(recipe, "$(RACE_PKGS)") {
		t.Fatalf("test-race recipe must run $(RACE_PKGS), not a hardcoded list:\n%s", recipe)
	}
	if !strings.Contains(recipe, "scripts/gate-slot-run") {
		t.Fatalf("test-race must take a push-gate slot like the other top-level lanes:\n%s", recipe)
	}
}

// TestRacePkgsCoversThePackageThatHadRaces pins the lane's floor. internal/api
// is where all four gascity-lzzj races were, so dropping it would leave the
// lane wired up and green while covering nothing that ever broke. The list may
// grow freely; it may not shrink past this.
//
// Since gascity-ujru the lane defaults to $(UNIT_PKGS_SWEEP) rather than an
// explicit list, so the floor can no longer be checked by looking for a literal
// "./internal/api". Naming the sweep satisfies the floor only because the sweep
// is a superset — which is a property of the sweep's own exclusion pattern, not
// of its name. So this asserts that property directly: whatever `go list ./...`
// yields for internal/api must survive the filter.
func TestRacePkgsCoversThePackageThatHadRaces(t *testing.T) {
	makefile := readMakefile(t)

	const marker = "RACE_PKGS ?= "
	idx := strings.Index(makefile, marker)
	if idx < 0 {
		t.Fatal("Makefile has no RACE_PKGS definition")
	}
	line := makefile[idx+len(marker):]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}

	pkgs := strings.Fields(line)
	if containsString(pkgs, "./internal/api") {
		return // an explicit list that still names it
	}
	if !containsString(pkgs, "$(UNIT_PKGS_SWEEP)") {
		t.Fatalf("RACE_PKGS = %q, want it to include ./internal/api (gascity-lzzj) "+
			"either explicitly or via $(UNIT_PKGS_SWEEP)", line)
	}

	// $(UNIT_PKGS_SWEEP) is `go list ./...` minus a grep -v -E pattern. Pull the
	// pattern out and prove it does not filter internal/api away.
	sweepIdx := strings.Index(makefile, "UNIT_PKGS_SWEEP = ")
	if sweepIdx < 0 {
		t.Fatal("Makefile has no UNIT_PKGS_SWEEP definition")
	}
	sweep := makefile[sweepIdx:]
	if nl := strings.IndexByte(sweep, '\n'); nl >= 0 {
		sweep = sweep[:nl]
	}
	m := regexp.MustCompile(`grep -v -E '([^']*)'`).FindStringSubmatch(sweep)
	if m == nil {
		t.Fatalf("UNIT_PKGS_SWEEP = %q, want a grep -v -E '<pattern>' this test can check", sweep)
	}
	// The Makefile doubles $ for make; undo that to get the shell-level pattern.
	pattern := strings.ReplaceAll(m[1], "$$", "$")
	excluded, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("UNIT_PKGS_SWEEP exclusion pattern %q does not compile: %v", pattern, err)
	}
	const apiPkg = "github.com/gastownhall/gascity/internal/api"
	if excluded.MatchString(apiPkg) {
		t.Fatalf("RACE_PKGS is $(UNIT_PKGS_SWEEP) but its exclusion pattern %q drops %s "+
			"(gascity-lzzj); the lane would be green while covering nothing that ever broke",
			pattern, apiPkg)
	}
}

// TestPrePushRunsTheRaceLaneOnEveryPlatform pins the wiring half. A lane no
// hook invokes is documentation, not a gate. The hook must run it OUTSIDE the
// uname switch: a data race is a property of the code, not of Darwin, and the
// two platform lanes are exec'd, so anything placed after the switch would
// never run at all.
func TestPrePushRunsTheRaceLaneOnEveryPlatform(t *testing.T) {
	hook := readPrePushHook(t)

	raceIdx := strings.Index(hook, "\nmake test-race\n")
	if raceIdx < 0 {
		t.Fatal(".githooks/pre-push does not invoke `make test-race`")
	}
	switchIdx := strings.Index(hook, `case "$(uname -s)" in`)
	if switchIdx < 0 {
		t.Fatal(".githooks/pre-push has no platform-lane switch")
	}
	if raceIdx > switchIdx {
		t.Fatal("`make test-race` must run BEFORE the platform switch; the switch exec's its lane, so nothing after it runs")
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
