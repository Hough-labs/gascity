package main

import (
	"bufio"
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

const processArgsPSTimeout = 10 * time.Second

// lsofCommandTimeout bounds one logical question rather than one exec: helpers
// that try a formatted lsof form and then fall back to a plain one share a
// single deadline, so the retry cannot double the budget. It matches
// processArgsPSTimeout because lsof walks the host-wide open-file table, so its
// latency scales with the machine's total open files rather than with anything
// gc controls. It is a var only so tests can shorten it.
var lsofCommandTimeout = 10 * time.Second

// probeResult is the answer from a system probe that can fail to answer at all:
// the probe either established the fact (probeYes), established its absence
// (probeNo), or could not run to completion (probeUnknown).
//
// The third state is load-bearing. On Linux these facts come from /proc, which
// cannot time out; on Darwin they come from lsof under a deadline. Collapsing a
// failed probe into probeNo is what turns a loaded machine into a confident
// wrong answer -- "no process holds this port" while one does -- and callers
// then allocate a fresh port, adopt stale runtime state, or report drift on the
// strength of it.
//
// The contract for callers: probeUnknown may never stand in for probeNo in a
// decision that mutates state (allocating a port, repairing or adopting runtime
// state, advising that a file is safe to delete), and may never be reported as
// a confirmed negative. Where the only alternative to acting on probeNo is a
// non-destructive retry or leaving working state alone, treating probeUnknown
// as "keep going and re-probe" is correct -- but it is still recorded, never
// silently folded into the negative.
type probeResult int

const (
	probeUnknown probeResult = iota
	probeNo
	probeYes
)

// probeAnswer converts a definite result from a probe that ran to completion.
func probeAnswer(found bool) probeResult {
	if found {
		return probeYes
	}
	return probeNo
}

// String renders the tri-state for the tab-separated inspection reports.
func (r probeResult) String() string {
	switch r {
	case probeYes:
		return "true"
	case probeNo:
		return "false"
	default:
		return "unknown"
	}
}

type managedDoltProcessInspection struct {
	ManagedPID              int
	ManagedSource           string
	ManagedOwned            bool
	ManagedDeletedInodes    probeResult
	PortHolderPID           int
	PortHolderProbed        bool
	PortHolderOwned         bool
	PortHolderDeletedInodes probeResult
}

func inspectManagedDoltProcess(cityPath, port string) (managedDoltProcessInspection, error) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return managedDoltProcessInspection{}, err
	}
	info := managedDoltProcessInspection{}
	info.ManagedPID, info.ManagedSource = findManagedDoltPID(layout, port)
	if info.ManagedPID > 0 {
		info.ManagedOwned, info.ManagedDeletedInodes = inspectManagedDoltOwnership(info.ManagedPID, layout)
	}
	info.PortHolderPID, info.PortHolderProbed = findPortHolderPID(port)
	if info.PortHolderPID > 0 {
		info.PortHolderOwned, info.PortHolderDeletedInodes = inspectManagedDoltOwnership(info.PortHolderPID, layout)
	}
	return info, nil
}

func findManagedDoltPID(layout managedDoltRuntimeLayout, port string) (int, string) {
	if pid := managedPIDFromPIDFile(layout.PIDFile); pid > 0 {
		return pid, "pid-file"
	}
	// The probe flag is deliberately dropped here: this is a chain of discovery
	// strategies, so an unanswered port lookup simply falls through to the next
	// one rather than standing in for "no managed dolt".
	if pid, _ := findPortHolderPID(port); pid > 0 {
		return pid, "port-holder"
	}
	if pid := managedPIDFromPSByConfig(layout.ConfigFile); pid > 0 {
		return pid, "config"
	}
	if pid := managedPIDFromPSByDataDir(layout.DataDir); pid > 0 {
		return pid, "data-dir"
	}
	return 0, ""
}

func managedPIDFromPIDFile(pidFile string) int {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || !pidAlive(pid) {
		_ = os.Remove(pidFile)
		return 0
	}
	return pid
}

// findPortHolderPID reports the PID listening on port. The bool means "the
// probe ran to completion", not "a holder was found": (0, true) says the port is
// genuinely unheld, while (0, false) says we could not tell. No caller may read
// the second form as a free port.
func findPortHolderPID(port string) (int, bool) {
	port = strings.TrimSpace(port)
	if port == "" {
		return 0, false
	}
	if pid, checked := findPortHolderPIDFromProc(port); checked {
		return pid, true
	}
	return findPortHolderPIDFromLsof(port)
}

// findPortHolderPIDFromLsof answers the same question from lsof on hosts without
// /proc. Both attempts share one deadline so the fallback cannot double the
// budget, and a probe that never completed is reported as such rather than as an
// unheld port.
func findPortHolderPIDFromLsof(port string) (int, bool) {
	if _, err := exec.LookPath("lsof"); err != nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), lsofCommandTimeout)
	defer cancel()

	out, err := lsofOutputContext(ctx, "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-t")
	if lsofProbeFailed(err) {
		return 0, false
	}
	if err == nil {
		if pid := pidFromLsofPIDList(string(out)); pid > 0 {
			return pid, true
		}
	}

	out, err = lsofOutputContext(ctx, "-nP", "-iTCP:"+port, "-sTCP:LISTEN")
	if lsofProbeFailed(err) {
		return 0, false
	}
	if err != nil {
		// lsof ran and exited non-zero: nothing matched the query.
		return 0, true
	}
	return pidFromPlainPortLsofOutput(string(out), port), true
}

func pidFromLsofPIDList(output string) int {
	for _, field := range strings.Fields(output) {
		pid, err := strconv.Atoi(field)
		if err == nil && pidAlive(pid) {
			return pid
		}
	}
	return 0
}

func pidFromPlainPortLsofOutput(output, port string) int {
	portSuffix := ":" + strings.TrimSpace(port)
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, portSuffix) || !strings.Contains(line, "(LISTEN)") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err == nil && pidAlive(pid) {
			return pid
		}
	}
	return 0
}

func cwdFromFormattedLsofOutput(output string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "n") {
			path := normalizeLsofReportedPath(strings.TrimPrefix(line, "n"))
			if path != "" {
				return path, true
			}
		}
	}
	return "", false
}

func cwdFromPlainLsofOutput(output string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 9 || fields[3] != "cwd" {
			continue
		}
		path := plainLsofPath(fields)
		if path != "" {
			return path, true
		}
	}
	return "", false
}

func deletedDataInodeTargetsFromFormattedLsofOutput(output string) []string {
	var targets []string
	var currentName string
	currentDeleted := false
	flush := func() {
		if currentName != "" && currentDeleted {
			targets = append(targets, currentName)
		}
		currentName = ""
		currentDeleted = false
	}

	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'f':
			flush()
		case 'k':
			links := strings.TrimSpace(strings.TrimPrefix(line, "k"))
			if links == "0" {
				currentDeleted = true
			}
		case 'n':
			if currentName != "" {
				flush()
			}
			target := strings.TrimSpace(strings.TrimPrefix(line, "n"))
			if strings.Contains(target, " (deleted)") {
				currentDeleted = true
				target = strings.TrimSuffix(target, " (deleted)")
			}
			currentName = normalizeLsofReportedPath(target)
		}
	}
	flush()
	return targets
}

func deletedDataInodeTargetsFromPlainLsofOutput(output string) []string {
	var targets []string
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, " (deleted)") {
			continue
		}
		fields := strings.Fields(strings.TrimSuffix(line, " (deleted)"))
		target := plainLsofPath(fields)
		if target != "" {
			targets = append(targets, target)
		}
	}
	return targets
}

func plainLsofPath(fields []string) string {
	if len(fields) < 9 {
		return ""
	}
	return normalizeLsofReportedPath(strings.Join(fields[8:], " "))
}

func normalizeLsofReportedPath(path string) string {
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

// processCWDFromLsof reads a process working directory on hosts without /proc.
// The probeResult separates "lsof reported no cwd" (probeNo) from "the probe
// never completed" (probeUnknown); the returned path is meaningful only for
// probeYes.
func processCWDFromLsof(pid int) (string, probeResult) {
	if _, err := exec.LookPath("lsof"); err != nil {
		return "", probeUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), lsofCommandTimeout)
	defer cancel()

	out, err := lsofOutputContext(ctx, "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn")
	if lsofProbeFailed(err) {
		return "", probeUnknown
	}
	if err == nil {
		if cwd, ok := cwdFromFormattedLsofOutput(string(out)); ok {
			return cwd, probeYes
		}
	}
	out, err = lsofOutputContext(ctx, "-a", "-p", strconv.Itoa(pid), "-d", "cwd")
	if lsofProbeFailed(err) {
		return "", probeUnknown
	}
	if err != nil {
		return "", probeNo
	}
	cwd, ok := cwdFromPlainLsofOutput(string(out))
	return cwd, probeAnswer(ok)
}

func benignManagedDeletedInodeTarget(target string) bool {
	clean := filepath.Clean(strings.TrimSpace(target))
	return strings.HasSuffix(clean, string(filepath.Separator)+".dolt"+string(filepath.Separator)+"noms"+string(filepath.Separator)+"LOCK")
}

// processHasDeletedDataInodes reports whether pid still holds inodes that have
// been unlinked from dataDir -- the signal that a dolt server is writing into a
// data directory that has since been replaced. probeUnknown means the question
// could not be answered, which callers must not read as a clean process.
func processHasDeletedDataInodes(pid int, dataDir string) probeResult {
	if pid <= 0 {
		return probeNo
	}
	if cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd")); err == nil && strings.HasSuffix(cwd, " (deleted)") {
		return probeYes
	}
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err == nil {
		for _, entry := range entries {
			target, readErr := os.Readlink(filepath.Join(fdDir, entry.Name()))
			if readErr != nil || !strings.Contains(target, " (deleted)") {
				continue
			}
			cleanTarget := strings.TrimSuffix(target, " (deleted)")
			if pathWithinOrSame(cleanTarget, dataDir) {
				if benignManagedDeletedInodeTarget(cleanTarget) {
					continue
				}
				return probeYes
			}
		}
		return probeNo
	}
	targets, probed := deletedDataInodeTargetsFromLsof(pid)
	if !probed {
		return probeUnknown
	}
	for _, target := range targets {
		if pathWithinOrSame(target, dataDir) {
			if benignManagedDeletedInodeTarget(target) {
				continue
			}
			return probeYes
		}
	}
	return probeNo
}

func pathWithinOrSame(path, root string) bool {
	path = normalizePathForCompare(strings.TrimSpace(strings.TrimSuffix(path, " (deleted)")))
	root = normalizePathForCompare(strings.TrimSpace(root))
	if path == "" || root == "" {
		return false
	}
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// deletedDataInodeTargetsFromLsof lists a process's unlinked open files on hosts
// without /proc. The bool means the probe ran to completion: a nil slice with
// false says we could not look, not that the process holds nothing.
func deletedDataInodeTargetsFromLsof(pid int) ([]string, bool) {
	if _, err := exec.LookPath("lsof"); err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), lsofCommandTimeout)
	defer cancel()

	out, err := lsofOutputContext(ctx, "-a", "-p", strconv.Itoa(pid), "+L1", "-Fnk")
	if lsofProbeFailed(err) {
		return nil, false
	}
	if err == nil {
		if targets := deletedDataInodeTargetsFromFormattedLsofOutput(string(out)); len(targets) > 0 {
			return targets, true
		}
	}
	out, err = lsofOutputContext(ctx, "-p", strconv.Itoa(pid))
	if lsofProbeFailed(err) {
		return nil, false
	}
	if err != nil {
		return nil, true
	}
	return deletedDataInodeTargetsFromPlainLsofOutput(string(out)), true
}

func lsofOutput(args ...string) ([]byte, error) {
	return lsofOutputWithTimeout(lsofCommandTimeout, args...)
}

// lsofOutputWithTimeout runs a single lsof under its own deadline. Prefer
// lsofOutputContext when several execs answer one logical question and must
// share a budget.
func lsofOutputWithTimeout(timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return lsofOutputContext(ctx, args...)
}

// lsofOutputContext runs lsof with the hardening every caller needs: a WaitDelay
// so a child holding the pipes open cannot outlive the deadline, and a cancel
// that kills the whole process group rather than the direct child alone.
//
// A deadline hit is reported as an error wrapping context.DeadlineExceeded so
// callers can tell a truncated listing from a complete one; whatever lsof
// buffered before the kill is still returned alongside it.
func lsofOutputContext(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "lsof", args...)
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
	if ctxErr := ctx.Err(); ctxErr != nil {
		return out, fmt.Errorf("lsof: %w", ctxErr)
	}
	return out, err
}

// lsofProbeFailed reports whether err means the probe could not answer, as
// opposed to lsof answering with "nothing matched". lsof exits non-zero with
// empty output when a query has no matches, so a plain exit status is a genuine
// negative; a deadline, or a failure to launch lsof at all, is not.
func lsofProbeFailed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var exitErr *exec.ExitError
	return !errors.As(err, &exitErr)
}

func processHasDeletedDataInodesWithin(pid int, dataDir string, timeout time.Duration) probeResult {
	result := processHasDeletedDataInodes(pid, dataDir)
	if result == probeYes || timeout <= 0 {
		return result
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		result = processHasDeletedDataInodes(pid, dataDir)
		if result == probeYes {
			return result
		}
	}
	return result
}

func findPortHolderPIDFromProc(port string) (int, bool) {
	portNum, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0, true
	}
	inodes, checked := listeningSocketInodesFromProc(uint16(portNum))
	if !checked {
		return 0, false
	}
	if len(inodes) == 0 {
		return 0, true
	}
	return processWithSocketInodes(inodes), true
}

func listeningSocketInodesFromProc(port uint16) (map[string]struct{}, bool) {
	inodes := map[string]struct{}{}
	checked := false
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		checked = true
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			_, portHex, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			gotPort, err := strconv.ParseUint(portHex, 16, 16)
			if err != nil || uint16(gotPort) != port {
				continue
			}
			inodes[fields[9]] = struct{}{}
		}
	}
	return inodes, checked
}

func processWithSocketInodes(inodes map[string]struct{}) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !pidAlive(pid) {
			continue
		}
		fdDir := filepath.Join("/proc", entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
			if _, ok := inodes[inode]; ok {
				return pid
			}
		}
	}
	return 0
}

func managedPIDFromPSByConfig(configFile string) int {
	for _, line := range doltPSLines() {
		if !strings.Contains(line, "dolt sql-server") {
			continue
		}
		if !strings.Contains(line, "--config") || !strings.Contains(line, configFile) {
			continue
		}
		if pid := psLinePID(line); pid > 0 {
			return pid
		}
	}
	return 0
}

func managedPIDFromPSByDataDir(dataDir string) int {
	base := filepath.Base(dataDir)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return 0
	}
	for _, line := range doltPSLines() {
		if !strings.Contains(line, "dolt sql-server") {
			continue
		}
		if !strings.Contains(line, "--data-dir") || !strings.Contains(line, base) {
			continue
		}
		if pid := psLinePID(line); pid > 0 {
			return pid
		}
	}
	return 0
}

func doltPSLines() []string {
	out, err := exec.Command("ps", "ax", "-o", "pid,args").Output()
	if err != nil {
		return nil
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	lines := make([]string, 0, 16)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

func psLinePID(line string) int {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return 0
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || !pidAlive(pid) {
		return 0
	}
	return pid
}

func inspectManagedDoltOwnership(pid int, layout managedDoltRuntimeLayout) (bool, probeResult) {
	if pid <= 0 {
		return false, probeNo
	}

	stateDir := strings.TrimSpace(loadDoltRuntimeStateDataDir(layout.StateFile))
	if stateDir != "" && !samePath(stateDir, layout.DataDir) {
		return false, processHasDeletedDataInodes(pid, layout.DataDir)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		owned := managedDoltProcessOwnedWithStateDir(pid, layout, stateDir)
		deleted := processHasDeletedDataInodes(pid, layout.DataDir)
		if owned || deleted == probeYes || !pidAlive(pid) || time.Now().After(deadline) {
			return owned, deleted
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func managedDoltProcessOwnedWithStateDir(pid int, layout managedDoltRuntimeLayout, stateDir string) bool {
	if pid <= 0 {
		return false
	}
	if stateDir != "" && !samePath(stateDir, layout.DataDir) {
		return false
	}

	procArgs, _ := processArgs(pid)
	switch {
	case containsProcessConfig(procArgs, layout.ConfigFile):
		return true
	case hasOtherProcessConfig(procArgs):
		return false
	case processDataDirMatches(procArgs, layout.DataDir):
		return true
	case processCWDMatches(pid, layout.DataDir) == probeYes:
		// Ownership is a positive claim: an unproven cwd (probeUnknown) leaves
		// the process unadopted rather than asserting it is someone else's.
		return true
	default:
		return false
	}
}

func loadDoltRuntimeStateDataDir(path string) string {
	state, err := readDoltRuntimeStateFile(path)
	if err != nil {
		return ""
	}
	return state.DataDir
}

func processArgs(pid int) (string, error) {
	if args, err := processArgsFromProc(pid); err == nil && args != "" {
		return args, nil
	}
	return processArgsFromPS(pid, processArgsPSTimeout)
}

func processArgsFromProc(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return "", err
	}
	args := strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
	if args == "" {
		return "", fmt.Errorf("empty cmdline for pid %d", pid)
	}
	return args, nil
}

func processArgsFromPS(pid int, timeout time.Duration) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	if timeout <= 0 {
		timeout = processArgsPSTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "args=")
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
		return "", fmt.Errorf("ps args for pid %d: %w", pid, ctx.Err())
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func containsProcessConfig(args, configFile string) bool {
	return strings.Contains(args, "--config "+configFile) || strings.Contains(args, "--config="+configFile)
}

func hasOtherProcessConfig(args string) bool {
	return strings.Contains(args, "--config")
}

func processDataDirMatches(args, dataDir string) bool {
	index := strings.Index(args, "--data-dir")
	if index < 0 {
		return false
	}
	value := extractFlagValue(args[index:], "--data-dir")
	if value == "" {
		return false
	}
	return samePath(value, dataDir)
}

func extractFlagValue(args, flag string) string {
	fields := strings.Fields(args)
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if field == flag {
			if i+1 < len(fields) {
				return strings.TrimSpace(fields[i+1])
			}
			return ""
		}
		prefix := flag + "="
		if strings.HasPrefix(field, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(field, prefix))
		}
	}
	return ""
}

func processCWDMatches(pid int, dataDir string) probeResult {
	cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err == nil {
		return probeAnswer(samePath(cwd, dataDir))
	}
	cwd, result := processCWDFromLsof(pid)
	if result != probeYes {
		return result
	}
	return probeAnswer(samePath(cwd, dataDir))
}

func doltProcessInspectionFields(info managedDoltProcessInspection) []string {
	return []string{
		fmt.Sprintf("managed_pid\t%d", info.ManagedPID),
		"managed_source\t" + info.ManagedSource,
		fmt.Sprintf("managed_owned\t%t", info.ManagedOwned),
		fmt.Sprintf("managed_deleted_inodes\t%t", info.ManagedDeletedInodes == probeYes),
		fmt.Sprintf("managed_deleted_inodes_probed\t%t", info.ManagedDeletedInodes != probeUnknown),
		fmt.Sprintf("port_holder_pid\t%d", info.PortHolderPID),
		fmt.Sprintf("port_holder_probed\t%t", info.PortHolderProbed),
		fmt.Sprintf("port_holder_owned\t%t", info.PortHolderOwned),
		fmt.Sprintf("port_holder_deleted_inodes\t%t", info.PortHolderDeletedInodes == probeYes),
		fmt.Sprintf("port_holder_deleted_inodes_probed\t%t", info.PortHolderDeletedInodes != probeUnknown),
	}
}
