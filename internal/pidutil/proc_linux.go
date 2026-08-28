//go:build linux

package pidutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// startTime reads field 22 (starttime, in clock ticks since boot) of
// /proc/<pid>/stat.
//
// The comm field (field 2) is wrapped in parens and may itself contain spaces
// and parens, so parsing anchors on the final ')' and counts fields from
// there: field 3 (state) is the first token after "') '", making field 22
// (starttime) the token at index 19 of that suffix.
func startTime(pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	stat := string(data)
	rparen := strings.LastIndexByte(stat, ')')
	if rparen < 0 || rparen+2 >= len(stat) {
		return "", fmt.Errorf("pidutil: malformed stat for PID %d", pid)
	}
	fields := strings.Fields(stat[rparen+2:])
	const starttimeIndexAfterComm = 19 // field 22 minus fields 1-3 offset
	if len(fields) <= starttimeIndexAfterComm {
		return "", fmt.Errorf("pidutil: stat for PID %d has %d post-comm fields, want > %d", pid, len(fields), starttimeIndexAfterComm)
	}
	return fields[starttimeIndexAfterComm], nil
}

// cmdline reads the NUL-separated argv the kernel exposes at
// /proc/<pid>/cmdline.
func cmdline(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(string(data), "\x00")
	if trimmed == "" {
		return nil, nil
	}
	return NormalizeArgv(strings.Split(trimmed, "\x00")), nil
}
