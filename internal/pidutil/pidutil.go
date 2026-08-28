// Package pidutil contains small process helpers shared across GC packages.
package pidutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	psZombieTimeout  = 100 * time.Millisecond
	childEnumTimeout = 1 * time.Second
)

// Alive reports whether a PID exists and is not a zombie.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	statPath := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return !psReportsZombie(pid)
	}
	fields := strings.Fields(string(data))
	if len(fields) >= 3 && fields[2] == "Z" {
		return false
	}
	return true
}

// StartTime returns an opaque token identifying a PID's start time, used to
// disambiguate a recycled PID from the original target. The kernel never
// reuses a (pid, start time) pair for the lifetime of a boot, so a changed
// start time on the same PID proves the original process is gone and an
// unrelated one now holds the number.
//
// The source is platform-specific — /proc/<pid>/stat on linux, the
// kern.proc.pid sysctl on darwin — and the token is only ever compared for
// equality against another token from the same host, never parsed. It returns
// an error when the process record is unreadable or the host has no
// start-time source at all; callers treat that as "no identity signal
// available".
func StartTime(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("pidutil: invalid PID %d", pid)
	}
	return startTime(pid)
}

// AliveWithStartTime reports whether pid is alive AND still the same process
// identified by startTime. It closes the PID-reuse hole in Alive: during a
// post-SIGKILL reap wait the target's PID can be reaped and recycled to an
// unrelated new process inside the window, at which point plain Alive would
// wrongly report the (dead) target as still alive.
//
// An empty startTime disables the identity check and falls back to Alive — used
// when the original start time could not be captured before the wait, or on a
// host with no start-time source. A non-empty startTime that no longer matches
// means the PID was recycled: the original target is dead, so this returns
// false. When the current start time cannot be read despite Alive reporting
// true — a transient race, or a process that became unreadable — it keeps the
// conservative Alive answer rather than inventing a death.
func AliveWithStartTime(pid int, startTime string) bool {
	if !Alive(pid) {
		return false
	}
	if startTime == "" {
		return true
	}
	current, err := StartTime(pid)
	if err != nil {
		return true
	}
	return current == startTime
}

// AliveWithCmdline reports whether a PID exists, is not a zombie, and its
// command line satisfies match.
//
// It fails closed in every direction: a missing matcher, an unreadable command
// line, or a host with no argv source at all all report false. That bias is
// load-bearing rather than incidental. Callers are singleton guards asking
// "is my process already running under this PID?", so a wrong yes means a
// recycled PID suppresses the process that should have been started — silent,
// and indistinguishable from an idle system — while a wrong no starts a
// second one, which is observable and recoverable.
func AliveWithCmdline(pid int, match func([]string) bool) bool {
	if !Alive(pid) {
		return false
	}
	if match == nil {
		return false
	}
	argv, err := Cmdline(pid)
	if err != nil {
		return false
	}
	return match(argv)
}

// ArgvContainsSequence reports whether argv contains seq contiguously.
func ArgvContainsSequence(argv []string, seq ...string) bool {
	if len(seq) == 0 {
		return true
	}
	if len(argv) < len(seq) {
		return false
	}
	for i := 0; i <= len(argv)-len(seq); i++ {
		ok := true
		for j := range seq {
			if argv[i+j] != seq[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// ArgvHasFlagValue reports whether argv contains flag with value, either as
// "--flag value" or "--flag=value".
func ArgvHasFlagValue(argv []string, flag, value string) bool {
	if flag == "" || value == "" {
		return false
	}
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) && argv[i+1] == value {
			return true
		}
		if strings.HasPrefix(arg, flag+"=") && strings.TrimPrefix(arg, flag+"=") == value {
			return true
		}
	}
	return false
}

// Cmdline returns a PID's command line as the argument vector the process was
// executed with, normalized through NormalizeArgv. The source is
// platform-specific — /proc/<pid>/cmdline on linux, the kern.procargs2 sysctl
// on darwin — and both preserve argument boundaries, so an argument containing
// whitespace survives as one element. It returns an error when the process
// record is unreadable or the host has no argv source.
func Cmdline(pid int) ([]string, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("pidutil: invalid PID %d", pid)
	}
	return cmdline(pid)
}

// NormalizeArgv returns argv with empty and whitespace-only arguments
// dropped — the rule Cmdline applies to every platform's command line. Callers
// comparing a configured argv against Cmdline output must pass the
// configured side through this helper first so both sides share the same
// argument shape.
func NormalizeArgv(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		if strings.TrimSpace(arg) == "" {
			continue
		}
		out = append(out, arg)
	}
	return out
}

// ChildPIDs returns the pids of all live direct child processes of parent,
// enumerated portably via `ps -axo pid=,ppid=` rather than a /proc walk, so
// it works on darwin as well as linux. It returns an error when the ps
// invocation itself fails or times out, so callers can tell "enumeration
// ran and found nothing" apart from "enumeration did not run" — collapsing
// the two into an empty slice would let an unavailable check masquerade as
// a clean result.
//
// ps is itself alive, and a child of the caller, at the instant it captures
// the process table — so a caller checking its own children (parent ==
// os.Getpid(), the pattern this package's callers use for self leak checks)
// always sees ps's own transient pid/ppid row alongside any real children.
// The enumeration helper's own pid is excluded below so it can never
// masquerade as a leaked child.
func ChildPIDs(parent int) ([]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), childEnumTimeout)
	defer cancel()
	return childPIDs(ctx, parent)
}

// childPIDs is ChildPIDs with the enumeration deadline supplied by the caller
// instead of taken from childEnumTimeout.
//
// ChildPIDs binds it to childEnumTimeout, which is a budget sized for the real
// ps. Tests that assert on the parsing and self-exclusion logic shadow ps on
// PATH with a fake, and a fake is a brand-new executable that pays costs the
// real ps does not — a PATH walk, a fork+exec of a shell, and on darwin a
// first-execution syspolicy check. Under process-creation pressure those costs
// exceed the production budget and the deadline SIGKILLs the fake, so a test
// about which rows get filtered fails as "ps enumeration failed: signal:
// killed" instead (gascity-gs8). Injecting the deadline lets such a test buy
// enough headroom for its own fixture without touching the production
// constant, which stays covered by TestChildPIDsReturnsErrorWhenPSHangs.
func childPIDs(ctx context.Context, parent int) ([]int, error) {
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("pidutil: ps enumeration failed: %w", err)
	}
	selfPID := -1
	if cmd.Process != nil {
		selfPID = cmd.Process.Pid
	}

	var children []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, errPID := strconv.Atoi(fields[0])
		ppid, errPPID := strconv.Atoi(fields[1])
		if errPID != nil || errPPID != nil {
			continue
		}
		if pid == selfPID {
			continue
		}
		if ppid == parent {
			children = append(children, pid)
		}
	}
	return children, nil
}

func psReportsZombie(pid int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), psZombieTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	state := strings.TrimSpace(string(out))
	return strings.HasPrefix(state, "Z")
}
