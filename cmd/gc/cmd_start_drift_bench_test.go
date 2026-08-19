package main

// Benchmarks and NFR-budget tests for the supervisor drift-detection
// hot path (ga-a3ry.1 phase 3). Companion file to cmd_start_drift_test.go;
// kept separate so the unit-test file's flag-matrix and wording pins stay
// readable without a 200-line benchmark appendix.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// fakeHealthServer returns an httptest server that responds to /health
// with the supplied build_id. It approximates the supervisor's hot-path
// response so drift-detection benchmarks can measure the round-trip
// without spinning up a real supervisor.
func fakeHealthServer(buildID string) *httptest.Server {
	body := fmt.Sprintf(`{"status":"ok","version":"v0","build_id":%q,"uptime_sec":1,"cities_total":0,"cities_running":0}`, buildID)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// BenchmarkDriftDetect_NoDrift measures the cost of the no-drift path:
// supervisorAlive probe + /health round-trip + DetectBinaryDrift compare.
// NFR-4 from the architect's brief: <10ms per detection on the hot path.
//
// The probe and HTTP fetch are the only non-CPU costs in the no-drift
// path; this benchmark intentionally skips the city.toml load (which is
// well under a millisecond per the existing config tests).
func BenchmarkDriftDetect_NoDrift(b *testing.B) {
	const buildID = "abc12345"
	srv := fakeHealthServer(buildID)
	b.Cleanup(srv.Close)

	oldHook := supervisorAliveHook
	b.Cleanup(func() { supervisorAliveHook = oldHook })
	supervisorAliveHook = func() int { return 4242 }

	client := newHTTPSupervisorClient(srv.URL)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if pid := supervisorAliveHook(); pid == 0 {
			b.Fatalf("supervisorAlive returned 0")
		}
		status, err := client.Status(ctx)
		if err != nil {
			b.Fatalf("Status: %v", err)
		}
		if DetectBinaryDrift(buildID, status) {
			b.Fatalf("expected no drift; got drift")
		}
	}
}

// BenchmarkDriftDetect_WithRealisticPacks measures DetectPackDrift
// against a 5-pack tree with ~hundreds of files. NFR-1 budget: <100ms
// p95. The pack tree mirrors the gastown / consumer pack scale operators
// see in the field.
func BenchmarkDriftDetect_WithRealisticPacks(b *testing.B) {
	roots := buildRealisticPackTree(b, 5, 120)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drifted, err := DetectPackDrift(roots)
		if err != nil {
			b.Fatalf("DetectPackDrift: %v", err)
		}
		if len(drifted) != 0 {
			b.Fatalf("expected no drift on warmly-parsed roots; got %v", drifted)
		}
	}
}

// buildRealisticPackTree constructs n pack roots, each with filesPerPack
// regular files. ParsedAt is set to one hour in the future so DetectPackDrift
// reports no drift; the benchmark measures the walk cost, which dominates.
func buildRealisticPackTree(tb testing.TB, n, filesPerPack int) []PackRootStatus {
	tb.Helper()
	root := tb.TempDir()
	parsedAt := time.Now().Add(time.Hour)
	roots := make([]PackRootStatus, 0, n)
	for i := 0; i < n; i++ {
		dir := filepath.Join(root, fmt.Sprintf("pack-%d", i))
		// Spread files across two subdirectories so the walk visits
		// nested entries, matching real packs.
		sub1 := filepath.Join(dir, "agents")
		sub2 := filepath.Join(dir, "formulas")
		for _, d := range []string{sub1, sub2} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				tb.Fatalf("mkdir: %v", err)
			}
		}
		for j := 0; j < filesPerPack; j++ {
			target := sub1
			if j%2 == 0 {
				target = sub2
			}
			path := filepath.Join(target, fmt.Sprintf("file-%03d.toml", j))
			if err := os.WriteFile(path, []byte("name = \"x\"\n"), 0o644); err != nil {
				tb.Fatalf("write: %v", err)
			}
		}
		roots = append(roots, PackRootStatus{Dir: dir, ParsedAt: parsedAt})
	}
	return roots
}

// TestDriftDetect_NoDrift_NFR4 is the unit-test counterpart of
// BenchmarkDriftDetect_NoDrift: it runs the same no-drift round-trip
// repeatedly and asserts the average cost is comfortably under NFR-4's
// 10ms budget. Failing here surfaces in `go test` (no -bench flag
// required), which is what CI runs.
func TestDriftDetect_NoDrift_NFR4(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NFR budget test in short mode")
	}
	const buildID = "abc12345"
	srv := fakeHealthServer(buildID)
	t.Cleanup(srv.Close)

	oldHook := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = oldHook })
	supervisorAliveHook = func() int { return 4242 }

	client := newHTTPSupervisorClient(srv.URL)
	ctx := context.Background()

	const iterations = 100
	const budget = 10 * time.Millisecond

	// Warm the loopback connection and the GC, then measure.
	for i := 0; i < 5; i++ {
		_, _ = client.Status(ctx)
	}

	start := time.Now()
	for i := 0; i < iterations; i++ {
		if pid := supervisorAliveHook(); pid == 0 {
			t.Fatalf("supervisorAlive returned 0")
		}
		status, err := client.Status(ctx)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if DetectBinaryDrift(buildID, status) {
			t.Fatalf("expected no drift")
		}
	}
	elapsed := time.Since(start)
	avg := elapsed / iterations
	if avg > budget {
		t.Fatalf("NFR-4 violated: avg detect cost = %s (>%s) over %d iterations", avg, budget, iterations)
	}
	t.Logf("NFR-4 OK: avg detect cost = %s over %d iterations (budget %s)", avg, iterations, budget)
}

// TestDriftDetect_WithRealisticPacks_NFR1 asserts that DetectPackDrift is
// CORRECT over a 5-pack / 120-file city, and reports the NFR-1 cost
// distribution as diagnostics. It deliberately does NOT fail on elapsed time.
//
// It used to assert p95 < 100ms. That assertion measured the host, not the
// code: p95 over 30 in-process samples is dominated by whatever else the
// machine is doing, and the budget sat inside the noise band rather than
// outside it. Measured on one Darwin host at one commit, ten back-to-back
// runs, no code change between them:
//
//	p95 26.1 27.2 28.0 30.8 34.1 35.5 39.0 48.9 102.0 103.2 ms  -> 2/10 FAIL
//	max observed 98.7ms loaded, and 106.2ms on a quiet machine
//
// So the budget was already below the observed maximum with the machine
// idle: it could only ever be met by luck, and every unlucky run charged a
// ~25 minute suite re-run to tell a false positive from a real regression.
// TESTING.md's "Flakes are defects" is explicit that timing artifacts are
// shard-balancing evidence rather than enforcement, and that an authoritative
// p95 needs twenty comparable samples out of trusted history -- not thirty
// samples from one process on a shared laptop.
//
// The timing measurement is not lost: BenchmarkDriftDetect_WithRealisticPacks
// above covers the same tree and is the right home for it, because `go test
// -bench` is run deliberately rather than on every push. What stays here is
// the part that is genuinely load-independent -- that a warmly-parsed tree
// reports no drift and no error, every iteration.
//
// gascity-l5w.
func TestDriftDetect_WithRealisticPacks_NFR1(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NFR diagnostic test in short mode")
	}
	roots := buildRealisticPackTree(t, 5, 120)

	const iterations = 30
	// nfr1Budget is the NFR-1 design target, retained so the log line below
	// stays comparable with the benchmark. It is a reporting reference, NOT
	// an assertion -- see the doc comment.
	const nfr1Budget = 100 * time.Millisecond

	samples := make([]time.Duration, 0, iterations)
	for i := 0; i < iterations; i++ {
		start := time.Now()
		drifted, err := DetectPackDrift(roots)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("DetectPackDrift: %v", err)
		}
		if len(drifted) != 0 {
			t.Fatalf("expected no drift; got %v", drifted)
		}
		samples = append(samples, elapsed)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	// p95 index for n=30: ceil(0.95 * 30) - 1 = 28.
	p95 := samples[len(samples)*95/100]
	t.Logf("NFR-1 diagnostic (not asserted): p95 detect cost = %s, max = %s, min = %s (design target %s, %d samples)",
		p95, samples[len(samples)-1], samples[0], nfr1Budget, iterations)
}
