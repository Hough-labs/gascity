package scripts_test

import (
	"os"
	"path/filepath"
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
	if !containsString(pkgs, "./internal/api") {
		t.Fatalf("RACE_PKGS = %q, want it to still include ./internal/api (gascity-lzzj)", line)
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
