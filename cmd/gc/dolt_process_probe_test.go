package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeFakeLsof installs a stub lsof at the front of PATH for the test's life.
func writeFakeLsof(t *testing.T, script string) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "lsof"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(lsof): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
}

func shortenLsofTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	previous := lsofCommandTimeout
	lsofCommandTimeout = d
	t.Cleanup(func() { lsofCommandTimeout = previous })
}

// A probe that times out must not be reported as "this port has no holder".
// That conflation is the defect this contract exists to prevent: callers act on
// a false negative by allocating a new port or declaring drift.
func TestFindPortHolderPIDFromLsofSeparatesProbeFailureFromNoHolder(t *testing.T) {
	t.Run("timeout is unknown, not a negative", func(t *testing.T) {
		shortenLsofTimeout(t, 150*time.Millisecond)
		writeFakeLsof(t, "#!/bin/sh\nexec sleep 30\n")

		pid, probed := findPortHolderPIDFromLsof("3306")
		if probed {
			t.Fatal("findPortHolderPIDFromLsof reported a completed probe after a timeout")
		}
		if pid != 0 {
			t.Fatalf("findPortHolderPIDFromLsof pid = %d, want 0 on probe failure", pid)
		}
	})

	t.Run("no match is a genuine negative", func(t *testing.T) {
		shortenLsofTimeout(t, 5*time.Second)
		// Real lsof exits non-zero with empty output when nothing matches.
		writeFakeLsof(t, "#!/bin/sh\nexit 1\n")

		pid, probed := findPortHolderPIDFromLsof("3306")
		if !probed {
			t.Fatal("findPortHolderPIDFromLsof reported probe failure for an lsof that ran and matched nothing")
		}
		if pid != 0 {
			t.Fatalf("findPortHolderPIDFromLsof pid = %d, want 0 when nothing matched", pid)
		}
	})

	t.Run("missing lsof is unknown", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())

		pid, probed := findPortHolderPIDFromLsof("3306")
		if probed {
			t.Fatal("findPortHolderPIDFromLsof reported a completed probe with no lsof on PATH")
		}
		if pid != 0 {
			t.Fatalf("findPortHolderPIDFromLsof pid = %d, want 0 when lsof is absent", pid)
		}
	})

	t.Run("a live holder is found", func(t *testing.T) {
		shortenLsofTimeout(t, 5*time.Second)
		writeFakeLsof(t, "#!/bin/sh\necho "+strconv.Itoa(os.Getpid())+"\n")

		pid, probed := findPortHolderPIDFromLsof("3306")
		if !probed {
			t.Fatal("findPortHolderPIDFromLsof reported probe failure for a successful lsof")
		}
		if pid != os.Getpid() {
			t.Fatalf("findPortHolderPIDFromLsof pid = %d, want %d", pid, os.Getpid())
		}
	})
}

// Each helper tries a formatted lsof form and then a plain one. Both attempts
// must share one deadline, or the budget silently doubles.
func TestFindPortHolderPIDFromLsofSharesOneDeadlineAcrossAttempts(t *testing.T) {
	shortenLsofTimeout(t, 300*time.Millisecond)
	writeFakeLsof(t, "#!/bin/sh\nexec sleep 30\n")

	start := time.Now()
	if _, probed := findPortHolderPIDFromLsof("3306"); probed {
		t.Fatal("findPortHolderPIDFromLsof reported a completed probe after a timeout")
	}
	elapsed := time.Since(start)
	if elapsed > 2*lsofCommandTimeout {
		t.Fatalf("findPortHolderPIDFromLsof took %s, want one shared %s deadline, not one per exec", elapsed, lsofCommandTimeout)
	}
}

func TestLsofOutputWithTimeoutReportsDeadlineExceeded(t *testing.T) {
	writeFakeLsof(t, "#!/bin/sh\nexec sleep 30\n")

	_, err := lsofOutputWithTimeout(150*time.Millisecond, "-nP")
	if err == nil {
		t.Fatal("lsofOutputWithTimeout succeeded with a hanging lsof")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lsofOutputWithTimeout err = %v, want one wrapping context.DeadlineExceeded", err)
	}
	if !lsofProbeFailed(err) {
		t.Fatalf("lsofProbeFailed(%v) = false, want true for a deadline", err)
	}
}

// lsof exits non-zero when a query matches nothing. That is an answer, not a
// failure, and must not be classified as one.
func TestLsofProbeFailedTreatsExitStatusAsAnAnswer(t *testing.T) {
	writeFakeLsof(t, "#!/bin/sh\nexit 1\n")

	_, err := lsofOutputWithTimeout(5*time.Second, "-nP")
	if err == nil {
		t.Fatal("expected a non-zero exit from the stub lsof")
	}
	if lsofProbeFailed(err) {
		t.Fatalf("lsofProbeFailed(%v) = true, want false for a plain non-zero exit", err)
	}
}

func TestProcessCWDFromLsofSeparatesProbeFailureFromNotFound(t *testing.T) {
	t.Run("timeout is unknown", func(t *testing.T) {
		shortenLsofTimeout(t, 150*time.Millisecond)
		writeFakeLsof(t, "#!/bin/sh\nexec sleep 30\n")

		if _, result := processCWDFromLsof(123); result != probeUnknown {
			t.Fatalf("processCWDFromLsof result = %v, want probeUnknown", result)
		}
	})

	t.Run("no match is a genuine negative", func(t *testing.T) {
		shortenLsofTimeout(t, 5*time.Second)
		writeFakeLsof(t, "#!/bin/sh\nexit 1\n")

		if _, result := processCWDFromLsof(123); result != probeNo {
			t.Fatalf("processCWDFromLsof result = %v, want probeNo", result)
		}
	})

	t.Run("a cwd record is found", func(t *testing.T) {
		shortenLsofTimeout(t, 5*time.Second)
		writeFakeLsof(t, "#!/bin/sh\nprintf 'p123\\nfcwd\\nn/private/tmp/gc-city/.beads/dolt\\n'\n")

		cwd, result := processCWDFromLsof(123)
		if result != probeYes {
			t.Fatalf("processCWDFromLsof result = %v, want probeYes", result)
		}
		if !samePath(cwd, "/tmp/gc-city/.beads/dolt") {
			t.Fatalf("processCWDFromLsof cwd = %q, want /tmp/gc-city/.beads/dolt", cwd)
		}
	})
}

func TestProcessHasDeletedDataInodesReportsUnknownWhenLsofFails(t *testing.T) {
	if _, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(os.Getpid()), "fd")); err == nil {
		t.Skip("host has /proc; the lsof fallback is unreachable here")
	}
	shortenLsofTimeout(t, 150*time.Millisecond)
	writeFakeLsof(t, "#!/bin/sh\nexec sleep 30\n")

	if result := processHasDeletedDataInodes(os.Getpid(), t.TempDir()); result != probeUnknown {
		t.Fatalf("processHasDeletedDataInodes = %v, want probeUnknown when the lsof probe fails", result)
	}
}

// The reported inspection must say the port holder is unknown rather than
// printing pid 0, which reads as "nothing is listening".
func TestDoltProcessInspectionFieldsSurfaceAnUnknownPortHolder(t *testing.T) {
	fields := doltProcessInspectionFields(managedDoltProcessInspection{PortHolderProbed: false})
	if !containsField(fields, "port_holder_probed\tfalse") {
		t.Fatalf("doltProcessInspectionFields = %v, want a port_holder_probed field", fields)
	}

	fields = doltProcessInspectionFields(managedDoltProcessInspection{PortHolderPID: 42, PortHolderProbed: true})
	if !containsField(fields, "port_holder_probed\ttrue") {
		t.Fatalf("doltProcessInspectionFields = %v, want port_holder_probed true", fields)
	}
}

func containsField(fields []string, want string) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	return false
}

// A port probe that could not run must never authorize reusing the port as if
// it were free: repair declines rather than adopting unverified state.
func TestRepairedManagedDoltRuntimeStateDeclinesWhenPortProbeFails(t *testing.T) {
	if _, err := os.Stat("/proc/net/tcp"); err == nil {
		t.Skip("host has /proc TCP tables; the lsof fallback is unreachable here")
	}
	shortenLsofTimeout(t, 150*time.Millisecond)
	writeFakeLsof(t, "#!/bin/sh\nexec sleep 30\n")

	dataDir := t.TempDir()
	layout := managedDoltRuntimeLayout{DataDir: dataDir}
	state := doltRuntimeState{Port: 3306, PID: os.Getpid(), DataDir: dataDir}

	if _, ok := repairedManagedDoltRuntimeState("", layout, state); ok {
		t.Fatal("repairedManagedDoltRuntimeState repaired state from a port probe that never completed")
	}
}

// A rig-local sql-server.info must not be called stale on the strength of a
// port probe that never ran: the advice attached to that verdict is that the
// file is safe to delete, and the recorded server may well be live.
func TestRigLocalDoltPIDFromSQLServerInfoReportsUnknownWhenPortProbeFails(t *testing.T) {
	if _, err := os.Stat("/proc/net/tcp"); err == nil {
		t.Skip("host has /proc TCP tables; the lsof fallback is unreachable here")
	}
	shortenLsofTimeout(t, 150*time.Millisecond)
	writeFakeLsof(t, "#!/bin/sh\nexec sleep 30\n")

	dir := t.TempDir()
	infoDir := filepath.Join(dir, ".dolt")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := strconv.Itoa(os.Getpid()) + ":28232:dead-beef-cafe-feed\n"
	if err := os.WriteFile(filepath.Join(infoDir, "sql-server.info"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(sql-server.info): %v", err)
	}

	pid, port, exists, live := rigLocalDoltPIDFromSQLServerInfo(dir)
	if !exists {
		t.Fatal("infoExists = false, want true")
	}
	if pid != os.Getpid() || port != 28232 {
		t.Fatalf("pid, port = %d, %d, want %d, 28232", pid, port, os.Getpid())
	}
	if live != probeUnknown {
		t.Fatalf("rigLocalLive = %v, want probeUnknown when the port probe never completed", live)
	}
}
