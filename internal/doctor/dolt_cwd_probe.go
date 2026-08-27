package doctor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// doltDoctorProcRoot is the /proc mount the cwd probe consults. It is a var so
// tests can point it at a directory holding no process entries and exercise the
// lsof fallback deterministically on Linux as well as Darwin.
var doltDoctorProcRoot = "/proc"

// doltCWDLsofTimeout bounds the cwd probe's lsof fallback. lsof walks the
// host-wide open-file table, so its latency tracks the machine's total open
// files rather than anything gc controls; the budget matches the sibling
// command probes for that reason.
var doltCWDLsofTimeout = 10 * time.Second

// managedDoltDoctorProcessCWD reports the working directory of pid. The bool
// means the probe ran to completion, not that a directory was found:
// ("", true) is a process whose cwd is genuinely unreadable, ("", false) is an
// unanswered question. Callers must not read the second form as evidence that
// the process is rooted somewhere else.
//
// /proc answers this on Linux; hosts without /proc fall back to lsof, matching
// the /proc-then-fallback shape of managedDoltDoctorProcCmdline and
// managedDoltDoctorPortHolderPID.
func managedDoltDoctorProcessCWD(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	if cwd, err := os.Readlink(filepath.Join(doltDoctorProcRoot, strconv.Itoa(pid), "cwd")); err == nil {
		return cwd, true
	}
	return managedDoltDoctorProcessCWDFromLsof(pid)
}

// managedDoltDoctorProcessCWDFromLsof answers the same question from lsof on
// hosts without /proc, and reports a deadline or a missing lsof as an
// unanswered probe rather than as an unreadable cwd.
func managedDoltDoctorProcessCWDFromLsof(pid int) (string, bool) {
	if _, err := exec.LookPath("lsof"); err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), doltCWDLsofTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn")
	// Without these a wedged lsof outlives its deadline: WaitDelay stops a child
	// holding the pipes open from blocking Output, and the cancel kills the whole
	// process group rather than the direct child alone.
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", false
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			// lsof never ran (absent, or not executable): we cannot tell.
			return "", false
		}
		// lsof ran and exited non-zero: it has no cwd entry for that pid.
		return "", true
	}
	cwd, _ := doltDoctorCWDFromLsofOutput(string(out))
	return cwd, true
}

// doltDoctorCWDFromLsofOutput reads the path out of `lsof -Fn` field output,
// whose cwd record is a single "n"-prefixed line.
func doltDoctorCWDFromLsofOutput(output string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "n") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "n"))
		if raw == "" {
			continue
		}
		return normalizeDoltDoctorLsofPath(raw), true
	}
	return "", false
}

// normalizeDoltDoctorLsofPath undoes the Darwin /private aliasing that lsof
// reports, so a cwd can be compared against a configured path. sameDoctorScope
// resolves symlinks, but only when both sides exist; a dolt process rooted in
// /var/folders is reported by lsof as /private/var/folders and would otherwise
// mismatch. Mirrors normalizeLsofReportedPath in cmd/gc, which internal/ cannot
// import.
func normalizeDoltDoctorLsofPath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	switch {
	case path == "/private/tmp":
		return "/tmp"
	case strings.HasPrefix(path, "/private/tmp/"):
		return "/tmp/" + strings.TrimPrefix(path, "/private/tmp/")
	case path == "/private/var":
		return "/var"
	case strings.HasPrefix(path, "/private/var/"):
		return "/var/" + strings.TrimPrefix(path, "/private/var/")
	default:
		return path
	}
}
