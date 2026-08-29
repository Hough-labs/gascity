package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gateSweepRecipe returns the go-test-observable sweep line of a gate target.
func gateSweepRecipe(t *testing.T, makefile, target string) string {
	t.Helper()
	for _, line := range makeTargetRecipe(t, makefile, target) {
		if strings.Contains(line, "scripts/go-test-observable") {
			return line
		}
	}
	t.Fatalf("%s target has no go-test-observable sweep command", target)
	return ""
}

// runGateParallelism runs scripts/test-gate-parallelism with a pinned CPU
// budget and an optional GC_TEST_GATE_PARALLEL override, returning its trimmed
// combined output. An empty outerP omits the argument entirely so the usage
// path can be exercised.
//
// This is deliberately the only exec site in this file: both the arithmetic
// table and the fail-closed table route through it, so the repository resource
// census counts one subprocess call site for the whole file rather than one
// per table (test/test-resources.toml).
func runGateParallelism(t *testing.T, cpus, outerP, override string) (string, error) {
	t.Helper()

	root := repoRoot(t)
	var args []string
	if outerP != "" {
		args = append(args, outerP)
	}
	cmd := exec.Command(filepath.Join(root, "scripts", "test-gate-parallelism"), args...)
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GC_TEST_LOCAL_CPUS=" + cpus,
	}
	if override != "" {
		cmd.Env = append(cmd.Env, "GC_TEST_GATE_PARALLEL="+override)
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestGateLanesCapTestBinaryParallelism pins the fix for gascity-ngab. A
// `go test` binary defaults -parallel to GOMAXPROCS, so each of the -p
// concurrent binaries independently fans its t.Parallel() tests out across
// every core: the sweep's peak concurrency is -p x GOMAXPROCS, four times the
// core count on a 16-core host. That oversubscription is what inverts the
// wall-clock assertions the gate then reports as test failures. Both gate
// lanes must therefore size -parallel against the host rather than leaving it
// at its GOMAXPROCS default.
func TestGateLanesCapTestBinaryParallelism(t *testing.T) {
	makefile := readMakefile(t)

	if !strings.Contains(makefile, "GATE_TEST_P ?=") {
		t.Error("Makefile does not define GATE_TEST_P; the gate's -p value must be a single named knob")
	}
	if !strings.Contains(makefile, "GATE_TEST_PARALLEL ?=") {
		t.Error("Makefile does not define GATE_TEST_PARALLEL")
	}
	if !strings.Contains(makefile, "scripts/test-gate-parallelism") {
		t.Error("GATE_TEST_PARALLEL must be computed by scripts/test-gate-parallelism, not hardcoded")
	}

	for _, target := range []string{"test", "test-mac"} {
		sweep := gateSweepRecipe(t, makefile, target)
		if !strings.Contains(sweep, "-p=$(GATE_TEST_P)") {
			t.Errorf("%s sweep does not take -p from $(GATE_TEST_P):\n%s", target, sweep)
		}
		if !strings.Contains(sweep, "-parallel=$(GATE_TEST_PARALLEL)") {
			t.Errorf("%s sweep leaves -parallel at its GOMAXPROCS default:\n%s", target, sweep)
		}
	}
}

// shardLoopRegion returns the part of a gate target's recipe from its first
// shard loop onward. Both lanes put their sweep ahead of their loops, so this
// is exactly the text that must not acquire a -parallel bound.
func shardLoopRegion(t *testing.T, makefile, target string) string {
	t.Helper()
	recipe := strings.Join(makeTargetRecipe(t, makefile, target), "\n")
	start := -1
	for _, opener := range []string{
		"for s in $$(seq 1 $(CMD_GC_UNIT_TOTAL))",
		"for p in $(SHARDED_SWEEP_PKGS)",
	} {
		if i := strings.Index(recipe, opener); i >= 0 && (start < 0 || i < start) {
			start = i
		}
	}
	if start < 0 {
		t.Fatalf("%s target has no shard loop; this test no longer covers anything:\n%s", target, recipe)
	}
	if sweep := strings.Index(recipe, "scripts/go-test-observable"); sweep > start {
		t.Fatalf("%s target runs its sweep after its shard loops; the -parallel split has to be re-read:\n%s", target, recipe)
	}
	return recipe[start:]
}

// TestShardLegsKeepTheGOMAXPROCSDefaultParallelism records the other half of
// the gascity-ngab decision: $(GATE_TEST_PARALLEL) bounds each lane's SWEEP and
// deliberately does not reach the shard loops beside it (cmd/gc from
// gascity-cgh, $(SHARDED_SWEEP_PKGS) from gascity-vdhw and gascity-5y4h).
//
// That is the same rule, not an exemption from it. The bound enforces
// `-p x -parallel ~= cores`; a shard leg hands exactly one package to one
// `go test` and the loops are sequential, so at most one test binary is ever
// live — effectively -p=1, whose matching -parallel is the full core count,
// which is the GOMAXPROCS default those legs already get. Handing them
// $(GATE_TEST_PARALLEL) would not tighten a loose bound, it would apply the
// sweep's four-way divisor to a single-binary lane and under-subscribe it 4x.
//
// This is pinned rather than left to a comment because the asymmetry reads as
// an oversight: whoever next sees -parallel on the sweep and not on the shards
// will otherwise "finish the job". The sequencing the argument rests on is
// pinned by TestFastUnitTargetRunsCmdGCAsSequentialShards and
// TestOversizedExamplePackagesLeaveTheSweepAndRunAsShards; if a shard loop is
// ever fanned out, this decision has to be revisited with it.
func TestShardLegsKeepTheGOMAXPROCSDefaultParallelism(t *testing.T) {
	makefile := readMakefile(t)

	for _, target := range []string{"test", "test-mac"} {
		region := shardLoopRegion(t, makefile, target)
		if !strings.Contains(region, "scripts/test-go-test-shard") {
			t.Errorf("%s shard loop does not invoke the shard runner:\n%s", target, region)
		}
		for _, marker := range []string{"GATE_TEST_PARALLEL", "-parallel"} {
			if strings.Contains(region, marker) {
				t.Errorf("%s shard loop carries %q; a sequential single-package leg is already effectively -p=1, "+
					"so its correct -parallel is the full core count that GOMAXPROCS already supplies:\n%s", target, marker, region)
			}
		}
	}
}

// TestGateParallelismDividesTheCPUBudget covers scripts/test-gate-parallelism.
// The value is the per-binary share of the host's cores once -p binaries run
// at once, floored so that a small CI runner keeps its current behavior
// instead of being serialized down to one test at a time. The floor never
// exceeds the CPU budget, so the value never rises above the GOMAXPROCS
// default it replaces.
func TestGateParallelismDividesTheCPUBudget(t *testing.T) {
	tests := []struct {
		name     string
		cpus     string
		outerP   string
		override string
		want     string
	}{
		{name: "16 cores across 4 binaries", cpus: "16", outerP: "4", want: "4"},
		{name: "64 cores across 4 binaries", cpus: "64", outerP: "4", want: "16"},
		{name: "8 cores floors at 4", cpus: "8", outerP: "4", want: "4"},
		{name: "4 core runner is a no-op", cpus: "4", outerP: "4", want: "4"},
		// Below the floor the floor itself clamps to the budget, so the
		// result is the core count -- exactly the GOMAXPROCS default these
		// runners already get. A flat floor of 4 would raise it instead.
		{name: "2 core runner keeps its GOMAXPROCS default", cpus: "2", outerP: "4", want: "2"},
		{name: "1 core runner is a no-op", cpus: "1", outerP: "4", want: "1"},
		{name: "single binary keeps the whole budget", cpus: "16", outerP: "1", want: "16"},
		{name: "explicit override wins", cpus: "16", outerP: "4", override: "2", want: "2"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runGateParallelism(t, tc.cpus, tc.outerP, tc.override)
			if err != nil {
				t.Fatalf("test-gate-parallelism %s: %v\n%s", tc.outerP, err, got)
			}
			if got != tc.want {
				t.Errorf("test-gate-parallelism %s with %s cpus = %q, want %q", tc.outerP, tc.cpus, got, tc.want)
			}
		})
	}
}

// TestGateParallelismRejectsBadInput keeps the script fail-closed: a garbled
// value must not silently become an unbounded -parallel.
func TestGateParallelismRejectsBadInput(t *testing.T) {
	tests := []struct {
		name     string
		outerP   string
		override string
	}{
		{name: "non-numeric outer p", outerP: "four"},
		{name: "zero outer p", outerP: "0"},
		{name: "missing outer p", outerP: ""},
		{name: "non-numeric override", outerP: "4", override: "all"},
		{name: "zero override", outerP: "4", override: "0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if out, err := runGateParallelism(t, "16", tc.outerP, tc.override); err == nil {
				t.Errorf("test-gate-parallelism accepted bad input, printed %q", out)
			}
		})
	}
}
