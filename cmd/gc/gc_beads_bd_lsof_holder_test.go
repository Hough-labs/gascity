package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests live in the shell-harness lane established by
// gc_beads_bd_port_probe_test.go: shell functions are lifted out of the
// shipped script with extractShellFunction and driven through the single
// runBdScriptHarness exec seam, with stub binaries on PATH standing in for
// the host tools. No new subprocess or listener call site is introduced —
// internal/testpolicy/resourcecensus ratchets those and its baseline already
// names runBdScriptHarness and listeningPort as the seams for this file's
// package.

// lsofStubMode selects which of the four run_lsof outcomes a stub reproduces.
// The four are exhaustive: run_lsof either finds matching open files, runs and
// finds none, is killed by run_with_timeout, or is not installed at all.
type lsofStubMode int

const (
	// lsofStubFound exits 0 and prints a PID — a holder was identified.
	lsofStubFound lsofStubMode = iota
	// lsofStubEmpty exits 1 with no output — lsof ran and the port is
	// genuinely unheld.
	lsofStubEmpty
	// lsofStubTimeout outlives GC_LSOF_TIMEOUT_SECONDS so run_with_timeout
	// SIGTERMs it and run_lsof reports 143 — the probe learned nothing.
	lsofStubTimeout
	// lsofStubAbsent leaves no lsof on PATH, so run_lsof short-circuits to
	// 127 — also "the probe learned nothing", by a different route.
	lsofStubAbsent
)

// writeStubLsof puts an `lsof` stub reproducing one run_lsof outcome into dir.
func writeStubLsof(t *testing.T, dir string, mode lsofStubMode, pid string) {
	t.Helper()
	var body string
	switch mode {
	case lsofStubFound:
		body = "#!/bin/sh\nprintf '%s\\n' " + shellSingleQuote(pid) + "\nexit 0\n"
	case lsofStubEmpty:
		body = "#!/bin/sh\nexit 1\n"
	case lsofStubTimeout:
		// exec, so the SIGTERM run_with_timeout sends lands on the sleeping
		// process itself. A wrapping shell would die and leave an orphaned
		// `sleep` holding the command-substitution pipe open, stalling the
		// harness for the whole sleep instead of the timeout.
		body = "#!/bin/sh\nexec sleep 30\n"
	case lsofStubAbsent:
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "lsof"), []byte(body), 0o755); err != nil {
		t.Fatalf("write stub lsof: %v", err)
	}
}

// lsofStubTimeoutEnv returns the GC_LSOF_TIMEOUT_SECONDS a stub mode needs.
//
// The budget must never be the thing under test. A stub that exits immediately
// gets one it cannot plausibly overrun even on a heavily loaded host, so a slow
// fork cannot turn "the port is confirmed unheld" into a spurious timeout and
// silently move a case onto the branch its sibling is supposed to own. Only
// lsofStubTimeout, which sleeps far past its budget, runs against one it is
// guaranteed to blow.
func lsofStubTimeoutEnv(mode lsofStubMode) string {
	if mode == lsofStubTimeout {
		return "GC_LSOF_TIMEOUT_SECONDS=1"
	}
	return "GC_LSOF_TIMEOUT_SECONDS=30"
}

// writeStubBin writes a no-op executable named name into dir, so a
// `command -v name` guard in the script resolves without running the real
// tool.
func writeStubBin(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
}

// shadowPATHDir returns a directory holding symlinks to the only external
// programs the extracted shell functions need — `head` for find_port_holder
// and `sleep` for run_with_timeout's watchdog. A test that points PATH at this
// directory alone can reproduce "lsof is not installed" on a host that has
// lsof, which is the lsofStubAbsent case.
//
// The tool paths are probed with os.Stat rather than exec.LookPath because
// LookPath is one of the census-tracked subprocess call sites.
func shadowPATHDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"head", "sleep"} {
		var src string
		for _, cand := range []string{"/usr/bin/" + tool, "/bin/" + tool} {
			if _, err := os.Stat(cand); err == nil {
				src = cand
				break
			}
		}
		if src == "" {
			t.Skipf("%s not found in /usr/bin or /bin; cannot build a shadow PATH", tool)
		}
		if err := os.Symlink(src, filepath.Join(dir, tool)); err != nil {
			t.Fatalf("link %s: %v", tool, err)
		}
	}
	return dir
}

// stubPATHDir returns a directory prepended to the real PATH, so stubs shadow
// the host's tools while the rest (mkdir, nc, ps) stay reachable. Used by the
// op-level tests, which need a working shell environment around the stub.
func stubPATHDir(t *testing.T) (dir, pathEnv string) {
	t.Helper()
	dir = t.TempDir()
	return dir, "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// lsofHarnessPreamble is the prelude every test script here shares: errexit on
// (the shipped script runs under `set -e`, and an unguarded call site that
// newly returns non-zero would abort — that must be caught here, not in
// production), plus the LSOF_TIMEOUT_SECONDS assignment lifted verbatim from
// the script's configuration block so the GC_LSOF_TIMEOUT_SECONDS contract is
// exercised rather than bypassed.
const lsofHarnessPreamble = "set -e\n" +
	"LSOF_TIMEOUT_SECONDS=\"${GC_LSOF_TIMEOUT_SECONDS:-2}\"\n" +
	"connect_host() { printf '127.0.0.1'; }\n"

// TestFindPortHolderDistinguishesLsofOutcomes is the core of gascity-jn1e.
//
// find_port_holder used to end in `run_lsof ... | head -1`, whose exit status
// is head's — always 0. run_lsof's status was the only thing separating "the
// port is genuinely free" from "the probe never completed", and the pipeline
// threw it away, so all four outcomes below collapsed into "empty output,
// success" for every caller.
//
// The load-bearing assertion is that lsofStubEmpty and lsofStubTimeout do NOT
// land on the same result: both print nothing, and only the status tells them
// apart.
func TestFindPortHolderDistinguishesLsofOutcomes(t *testing.T) {
	t.Parallel()

	src := gcBeadsBdScriptSource(t)
	fns := strings.Join([]string{
		extractShellFunction(t, src, "run_with_timeout"),
		extractShellFunction(t, src, "run_lsof"),
		extractShellFunction(t, src, "lsof_status_class"),
		extractShellFunction(t, src, "find_port_holder"),
	}, "\n")

	cases := []struct {
		name       string
		mode       lsofStubMode
		stubPID    string
		wantHolder string
		wantStatus string
	}{
		{"holder found", lsofStubFound, "4242", "4242", "0"},
		{"port confirmed unheld", lsofStubEmpty, "", "", "1"},
		{"probe killed at timeout", lsofStubTimeout, "", "", "2"},
		{"lsof not installed", lsofStubAbsent, "", "", "2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir := shadowPATHDir(t)
			writeStubLsof(t, binDir, tc.mode, tc.stubPID)

			script := lsofHarnessPreamble + fns + "\n" +
				"status=0\n" +
				"holder=$(find_port_holder) || status=$?\n" +
				"printf 'holder=[%s] status=%s\\n' \"$holder\" \"$status\"\n"

			out, exit := runBdScriptHarness(
				t, script,
				"PATH="+binDir,
				lsofStubTimeoutEnv(tc.mode),
				"DOLT_PORT=42188",
			)
			if exit != 0 {
				t.Fatalf("harness exit %d (an unguarded non-zero return under set -e?):\n%s", exit, out)
			}
			got := strings.TrimSpace(out)
			want := "holder=[" + tc.wantHolder + "] status=" + tc.wantStatus
			if got != want {
				t.Fatalf("find_port_holder produced %q, want %q", got, want)
			}
		})
	}
}

// TestPortHolderProbeUnresolvedGuard pins the shell-side twin of bh0's
// port_probe_unanswered_blocks_start. An lsof probe that did not complete is
// an unknown, not a free port; the guard resolves it with an independent
// mechanism — a TCP connect — and reports "treat the port as held" only when
// something demonstrably answers.
//
// A probe that DID complete is trusted either way: status 1 means lsof looked
// and found nothing, and the guard must not second-guess that with a TCP
// tiebreak.
func TestPortHolderProbeUnresolvedGuard(t *testing.T) {
	t.Parallel()

	src := gcBeadsBdScriptSource(t)
	fns := strings.Join([]string{
		extractShellFunction(t, src, "tcp_check_port"),
		extractShellFunction(t, src, "port_holder_probe_unresolved"),
	}, "\n")

	held, stop := listeningPort(t)
	defer stop()
	free := closedPort(t)

	cases := []struct {
		name        string
		probeStatus string
		port        string
		wantBlock   bool
	}{
		{"holder named", "0", held, false},
		{"confirmed unheld, port free", "1", free, false},
		{"confirmed unheld, port answering anyway", "1", held, false},
		{"probe incomplete, port free", "2", free, false},
		{"probe incomplete, port held", "2", held, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := lsofHarnessPreamble + fns + "\n" +
				"rc=0\n" +
				"port_holder_probe_unresolved \"$PROBE_STATUS\" || rc=$?\n" +
				"printf 'rc=%s\\n' \"$rc\"\n"

			out, exit := runBdScriptHarness(
				t, script,
				"PROBE_STATUS="+tc.probeStatus,
				"DOLT_PORT="+tc.port,
			)
			if exit != 0 {
				t.Fatalf("harness exit %d:\n%s", exit, out)
			}
			gotBlock := strings.TrimSpace(out) == "rc=0"
			if gotBlock != tc.wantBlock {
				t.Fatalf("port_holder_probe_unresolved blocked=%v, want %v (status=%s port=%s)\n%s",
					gotBlock, tc.wantBlock, tc.probeStatus, tc.port, out)
			}
		})
	}
}

// TestOpProbeDistinguishesUnansweredLsofProbe covers op_probe on the no-helper
// path — the branch taken whenever resolve_gc_helper_bin comes back empty, and
// the one bh0's indeterminate flag never reached because it was only ever set
// from the helper's *_probed fields.
//
// An lsof probe killed at the timeout must exit 3 (indeterminate) when the
// port is answering, never 2 ("confirmed not running").
func TestOpProbeDistinguishesUnansweredLsofProbe(t *testing.T) {
	t.Parallel()

	src := gcBeadsBdScriptSource(t)
	fns := strings.Join([]string{
		extractShellFunction(t, src, "tcp_check_port"),
		extractShellFunction(t, src, "tcp_check"),
		extractShellFunction(t, src, "resolve_gc_helper_bin"),
		extractShellFunction(t, src, "load_probe_managed_from_gc"),
		extractShellFunction(t, src, "load_managed_process_inspection_from_gc"),
		extractShellFunction(t, src, "run_with_timeout"),
		extractShellFunction(t, src, "run_lsof"),
		extractShellFunction(t, src, "lsof_status_class"),
		extractShellFunction(t, src, "find_port_holder"),
		extractShellFunction(t, src, "op_probe"),
	}, "\n")

	held, stop := listeningPort(t)
	defer stop()
	free := closedPort(t)

	cases := []struct {
		name     string
		mode     lsofStubMode
		verifyOK bool
		port     string
		wantExit int
	}{
		{"holder found and verified", lsofStubFound, true, held, 0},
		{"confirmed not running", lsofStubEmpty, false, free, 2},
		{"probe incomplete, port answering", lsofStubTimeout, false, held, 3},
		{"probe incomplete, port free", lsofStubTimeout, false, free, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir, pathEnv := stubPATHDir(t)
			writeStubLsof(t, binDir, tc.mode, "4242")

			verify := "verify_our_server() { return 1; }\n"
			if tc.verifyOK {
				verify = "verify_our_server() { return 0; }\n"
			}
			script := lsofHarnessPreamble +
				"is_remote() { return 1; }\n" +
				verify +
				fns + "\n" +
				"op_probe\n"

			out, gotExit := runBdScriptHarness(
				t, script,
				pathEnv,
				"GC_BIN=",
				lsofStubTimeoutEnv(tc.mode),
				"GC_CITY_PATH="+t.TempDir(),
				"DOLT_PORT="+tc.port,
			)
			if gotExit != tc.wantExit {
				t.Fatalf("op_probe exit=%d, want %d (%s)\n%s", gotExit, tc.wantExit, tc.name, out)
			}
			if tc.wantExit == 3 && !strings.Contains(out, "port-holder probe") {
				t.Fatalf("op_probe exit 3 must explain the indeterminate result, got:\n%s", out)
			}
		})
	}
}

// TestOpStopImplFailsClosedOnUnansweredPortHolderProbe covers the stop
// contract: success means the data dir is released. An lsof probe that never
// completed leaves pid empty, which used to take the "no process found" path
// and report a clean no-op stop while a live server still held the port.
func TestOpStopImplFailsClosedOnUnansweredPortHolderProbe(t *testing.T) {
	t.Parallel()

	src := gcBeadsBdScriptSource(t)
	fns := strings.Join([]string{
		extractShellFunction(t, src, "tcp_check_port"),
		extractShellFunction(t, src, "tcp_check"),
		extractShellFunction(t, src, "run_with_timeout"),
		extractShellFunction(t, src, "run_lsof"),
		extractShellFunction(t, src, "lsof_status_class"),
		extractShellFunction(t, src, "find_port_holder"),
		extractShellFunction(t, src, "port_holder_probe_unresolved"),
		extractShellFunction(t, src, "op_stop_impl"),
	}, "\n")

	held, stop := listeningPort(t)
	defer stop()
	free := closedPort(t)

	cases := []struct {
		name   string
		mode   lsofStubMode
		port   string
		wantRC string
	}{
		{"port confirmed free", lsofStubEmpty, free, "0"},
		{"probe incomplete, port free", lsofStubTimeout, free, "0"},
		{"probe incomplete, port answering", lsofStubTimeout, held, "1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir, pathEnv := stubPATHDir(t)
			writeStubLsof(t, binDir, tc.mode, "4242")

			script := lsofHarnessPreamble +
				"is_remote() { return 1; }\n" +
				"load_stop_managed_from_gc() { GC_STOP_MANAGED_USED=false; return 1; }\n" +
				"load_managed_process_inspection_from_gc() { return 1; }\n" +
				"find_dolt_pid() { printf ''; }\n" +
				"verify_our_server() { return 1; }\n" +
				"wait_dolt_data_lock_free() { return 0; }\n" +
				"save_state() { :; }\n" +
				fns + "\n" +
				"rc=0\n" +
				"op_stop_impl || rc=$?\n" +
				"printf 'rc=%s had_pid=%s\\n' \"$rc\" \"$GC_STOP_HAD_PID\"\n"

			out, exit := runBdScriptHarness(
				t, script,
				pathEnv,
				lsofStubTimeoutEnv(tc.mode),
				"DOLT_PORT="+tc.port,
				"PID_FILE="+filepath.Join(t.TempDir(), "dolt.pid"),
			)
			if exit != 0 {
				t.Fatalf("harness exit %d:\n%s", exit, out)
			}
			if !strings.Contains(out, "rc="+tc.wantRC+" ") {
				t.Fatalf("op_stop_impl returned unexpectedly (want rc=%s):\n%s", tc.wantRC, out)
			}
		})
	}
}

// TestOpStartRefusesToLaunchOnUnansweredPortHolderProbe covers the worst of
// the four call sites. On the no-helper path an empty holder skipped the whole
// holder-handling block and fell straight through to the launch loop, so an
// lsof timeout started a SECOND dolt against a port a live server was already
// holding — exactly the hazard bh0 closed on the helper path.
//
// load_start_managed_from_gc is stubbed to mark the moment op_start reaches
// the launch section, so "refused" and "launched" are distinguishable exits.
func TestOpStartRefusesToLaunchOnUnansweredPortHolderProbe(t *testing.T) {
	t.Parallel()

	src := gcBeadsBdScriptSource(t)
	fns := strings.Join([]string{
		extractShellFunction(t, src, "die"),
		extractShellFunction(t, src, "tcp_check_port"),
		extractShellFunction(t, src, "tcp_check"),
		extractShellFunction(t, src, "run_with_timeout"),
		extractShellFunction(t, src, "run_lsof"),
		extractShellFunction(t, src, "lsof_status_class"),
		extractShellFunction(t, src, "find_port_holder"),
		extractShellFunction(t, src, "port_holder_probe_unresolved"),
		extractShellFunction(t, src, "op_start"),
	}, "\n")

	held, stop := listeningPort(t)
	defer stop()
	free := closedPort(t)

	const reachedLaunchExit = 90

	cases := []struct {
		name     string
		mode     lsofStubMode
		port     string
		wantExit int
	}{
		{"holder found, imposter cleared, start proceeds", lsofStubFound, held, reachedLaunchExit},
		{"port confirmed free, start proceeds", lsofStubEmpty, free, reachedLaunchExit},
		{"probe incomplete, port free, start proceeds", lsofStubTimeout, free, reachedLaunchExit},
		{"probe incomplete, port answering, start refused", lsofStubTimeout, held, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir, pathEnv := stubPATHDir(t)
			writeStubLsof(t, binDir, tc.mode, "4242")
			// op_start hard-requires both before it does anything else.
			writeStubBin(t, binDir, "flock")
			writeStubBin(t, binDir, "dolt")

			runDir := t.TempDir()
			script := lsofHarnessPreamble +
				"is_remote() { return 1; }\n" +
				"resolve_gc_helper_bin() { printf ''; }\n" +
				"ensure_dolt_identity() { :; }\n" +
				"load_existing_managed_from_gc() { return 1; }\n" +
				"load_managed_process_inspection_from_gc() { return 1; }\n" +
				"load_probe_managed_from_gc() { return 1; }\n" +
				"find_dolt_pid() { printf ''; }\n" +
				"verify_our_server() { return 1; }\n" +
				"wait_deleted_data_inodes() { return 1; }\n" +
				"kill_imposter() { :; }\n" +
				"graceful_stop_owned_pid() { return 0; }\n" +
				"load_start_managed_from_gc() { echo REACHED_LAUNCH; exit 90; }\n" +
				fns + "\n" +
				"op_start\n"

			out, gotExit := runBdScriptHarness(
				t, script,
				pathEnv,
				lsofStubTimeoutEnv(tc.mode),
				"DOLT_PORT="+tc.port,
				"DATA_DIR="+filepath.Join(runDir, "data"),
				"LOCK_FILE="+filepath.Join(runDir, "start.lock"),
				"LOG_FILE="+filepath.Join(runDir, "dolt.log"),
				"PID_FILE="+filepath.Join(runDir, "dolt.pid"),
			)
			if gotExit != tc.wantExit {
				t.Fatalf("op_start exit=%d, want %d (%s)\n%s", gotExit, tc.wantExit, tc.name, out)
			}
			switch tc.wantExit {
			case reachedLaunchExit:
				if !strings.Contains(out, "REACHED_LAUNCH") {
					t.Fatalf("op_start should have reached the launch loop:\n%s", out)
				}
			case 1:
				if strings.Contains(out, "REACHED_LAUNCH") {
					t.Fatalf("op_start reached the launch loop despite an unresolved port-holder probe:\n%s", out)
				}
				if !strings.Contains(out, "refusing to start dolt") {
					t.Fatalf("op_start must explain why it refused, got:\n%s", out)
				}
			}
		})
	}
}

// TestFindDoltPidSurfacesIncompleteProbe covers the fourth call site. An empty
// find_dolt_pid result used to be indistinguishable from a confirmed absence:
// the function ended in a `ps | ... | head -1` pipeline whose status is always
// 0, so nothing recorded whether the lsof step had actually run.
//
// The ps fallback still gets its turn either way — that is what makes this the
// least severe of the four — but an incomplete lsof probe now returns 2, so a
// miss by ps alone is not reported as proof the server is gone.
//
// The harness runs under `set -e` with the call-site guard the shipped script
// uses, so a missing `|| true` at any real call site would surface here as an
// aborted shell rather than in production.
func TestFindDoltPidSurfacesIncompleteProbe(t *testing.T) {
	t.Parallel()

	src := gcBeadsBdScriptSource(t)
	fns := strings.Join([]string{
		extractShellFunction(t, src, "run_with_timeout"),
		extractShellFunction(t, src, "run_lsof"),
		extractShellFunction(t, src, "lsof_status_class"),
		extractShellFunction(t, src, "find_port_holder"),
		extractShellFunction(t, src, "find_dolt_pid"),
	}, "\n")

	cases := []struct {
		name       string
		mode       lsofStubMode
		wantPID    string
		wantStatus string
	}{
		{"port holder names the server", lsofStubFound, "4242", "0"},
		{"every mechanism looked and found nothing", lsofStubEmpty, "", "1"},
		{"lsof probe never completed", lsofStubTimeout, "", "2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir, pathEnv := stubPATHDir(t)
			writeStubLsof(t, binDir, tc.mode, "4242")

			runDir := t.TempDir()
			script := lsofHarnessPreamble + fns + "\n" +
				"status=0\n" +
				"pid=$(find_dolt_pid) || status=$?\n" +
				"printf 'pid=[%s] status=%s\\n' \"$pid\" \"$status\"\n"

			out, exit := runBdScriptHarness(
				t, script,
				pathEnv,
				lsofStubTimeoutEnv(tc.mode),
				"DOLT_PORT=42188",
				// A basename no live dolt sql-server can be running under, so
				// the ps fallback is a genuine miss rather than a flake.
				"DATA_DIR="+filepath.Join(runDir, "doltdata-jn1e-probe"),
				"CONFIG_FILE="+filepath.Join(runDir, "config.yaml"),
				"PID_FILE="+filepath.Join(runDir, "dolt.pid"),
			)
			if exit != 0 {
				t.Fatalf("harness exit %d (a call site missing its `|| true` guard under set -e?):\n%s", exit, out)
			}
			got := strings.TrimSpace(out)
			want := "pid=[" + tc.wantPID + "] status=" + tc.wantStatus
			if got != want {
				t.Fatalf("find_dolt_pid produced %q, want %q", got, want)
			}
		})
	}
}

// TestProbeHelperCallSitesGuardErrexit is a source-level assertion, not a
// behavioral one. Both probe helpers now return non-zero on a genuine miss and
// the shipped script runs with `set -e`, so every `x=$(helper)` assignment must
// sit in a context that suppresses errexit. An unguarded one aborts the whole
// operation the first time no server is running — on a host with no lsof, that
// is every invocation.
//
// The behavioral tests above cover op_start, op_probe and op_stop_impl, but
// two call sites they do not reach — op_ensure_ready and the poll loop in
// wait_for_concurrent_start_ready — have no other guard, and a new one added
// later would have none at all. This check is what makes the invariant hold
// for call sites nobody wrote a harness for.
func TestProbeHelperCallSitesGuardErrexit(t *testing.T) {
	t.Parallel()

	src := gcBeadsBdScriptSource(t)
	lines := strings.Split(src, "\n")
	for _, helper := range []string{"find_dolt_pid", "find_port_holder"} {
		t.Run(helper, func(t *testing.T) {
			substitution := "=$(" + helper + ")"
			var unguarded []string
			for i, line := range lines {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "#") {
					// The helpers' own contract comments quote the guarded form.
					continue
				}
				assign := strings.Index(trimmed, substitution)
				if assign < 0 {
					continue
				}
				// Any `||` list suppresses errexit for its left operand. The
				// shipped call sites use `|| true` and `|| status=$?`, but the
				// invariant is the suppression, not the right-hand side.
				if strings.Contains(trimmed[assign:], "||") {
					continue
				}
				unguarded = append(unguarded, fmt.Sprintf("line %d: %s", i+1, trimmed))
			}
			if len(unguarded) > 0 {
				t.Fatalf("%s call sites must suppress errexit with a `||` list:\n%s",
					helper, strings.Join(unguarded, "\n"))
			}
		})
	}
}
