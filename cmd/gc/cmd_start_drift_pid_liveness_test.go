package main

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// startLivenessSubject starts name with args and returns the running command.
// This is deliberately the only exec.Command site in the file: the resource
// census (internal/testpolicy/resourcecensus) ratchets test subprocess
// construction per call site, and every process these tests need is the same
// shape — a real child whose PID the liveness probes can be pointed at. Three
// inline constructions would have banked three calls against that ratchet to
// buy nothing; routing them through one helper banks one. New tests in this
// file should reuse it rather than add a call site.
func startLivenessSubject(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	return cmd
}

// TestPidGoneReportsReapedProcessAsGone pins the gone half of the contract: once
// the child has exited and been waited on, the PID is genuinely gone.
func TestPidGoneReportsReapedProcessAsGone(t *testing.T) {
	cmd := startLivenessSubject(t, "true")
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait true: %v", err)
	}
	if !pidGone(pid) {
		t.Fatalf("pidGone(%d) = false after the child exited and was reaped; want true", pid)
	}
}

// TestPidGoneReportsZombieAsGone covers the case the /proc/<pid>/status read
// existed for: a child that has exited but not yet been reaped still answers
// signal-zero, yet holds no ports or files, so the restart path must treat it
// as gone.
func TestPidGoneReportsZombieAsGone(t *testing.T) {
	cmd := startLivenessSubject(t, "true")
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if pidGone(pid) {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("pidGone(%d) = false for an un-reaped exited child; want true", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWaitForPIDExitActuallyKillsSurvivingProcess is the consequence test: the
// drift path spawns a replacement supervisor as soon as waitForPIDExit reports
// success, so a nil return has to mean the old process really stopped running.
// With the procfs-only pidGone this returned nil on the first poll while the
// supervisor kept running, and gc start then raced a second supervisor onto the
// same control socket.
//
// Liveness is judged by wait(2) rather than by pidGone so the assertion is
// independent of the function under test: a process that waitForPIDExit only
// believed it had killed would still be sleeping, and the wait would not
// return.
func TestWaitForPIDExitActuallyKillsSurvivingProcess(t *testing.T) {
	cmd := startLivenessSubject(t, "sleep", "30")
	pid := cmd.Process.Pid
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	if err := waitForPIDExit(pid, 100*time.Millisecond, 2*time.Second); err != nil {
		_ = cmd.Process.Kill()
		<-waited
		t.Fatalf("waitForPIDExit: %v", err)
	}

	select {
	case err := <-waited:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("sleep exited with %v; want a signal-terminated exit", err)
		}
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() {
			t.Fatalf("sleep exit status %v; want signaled", exitErr.Sys())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-waited
		t.Fatalf("waitForPIDExit reported pid %d exited, but the process was still running", pid)
	}
}
