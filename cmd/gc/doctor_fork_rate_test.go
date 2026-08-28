package main

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/doctor"
)

// forkRateCheckWith builds a forkRateCheck that returns the given /proc/stat
// snapshots in order (one per read) with an instant sleep, for deterministic
// testing without touching the real host.
func forkRateCheckWith(stats []string, warnPerSec float64) *forkRateCheck {
	i := 0
	return &forkRateCheck{
		sampleInterval: time.Second,
		warnPerSec:     warnPerSec,
		readProcStat: func() (string, error) {
			if i >= len(stats) {
				return "", errors.New("no more snapshots")
			}
			s := stats[i]
			i++
			return s, nil
		},
		sleep: func(time.Duration) {},
	}
}

func TestForkRateCheck_HighRateWarns(t *testing.T) {
	// 1000 -> 1600 over a 1s window = 600 forks/s, well above the 100/s warn.
	c := forkRateCheckWith([]string{"cpu 1 2 3\nprocesses 1000\n", "cpu 1 2 3\nprocesses 1600\n"}, 100)
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning (600/s >= 100)", r.Status)
	}
	if r.Severity != doctor.SeverityAdvisory {
		t.Fatalf("severity = %v, want SeverityAdvisory (observability, never gates)", r.Severity)
	}
	if !strings.Contains(r.Message, "600") {
		t.Fatalf("message should report the rate, got %q", r.Message)
	}
	if len(r.Details) == 0 {
		t.Fatalf("a warning should carry remediation Details (bpftrace + DoltLite/in-process)")
	}
}

func TestForkRateCheck_LowRateOK(t *testing.T) {
	// 1000 -> 1050 over 1s = 50 forks/s, below the warn threshold.
	c := forkRateCheckWith([]string{"processes 1000\n", "processes 1050\n"}, 100)
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK (50/s < 100)", r.Status)
	}
	if !strings.Contains(r.Message, "50") {
		t.Fatalf("message should report the rate, got %q", r.Message)
	}
}

func TestForkRateCheck_NonLinuxSkips(t *testing.T) {
	// /proc/stat without a "processes" line (non-Linux host): skip, never warn.
	c := forkRateCheckWith([]string{"cpu  1 2 3 4\n", "cpu  1 2 3 4\n"}, 100)
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK (skipped, not a false warning)", r.Status)
	}
	if !strings.Contains(strings.ToLower(r.Message), "skip") {
		t.Fatalf("message should indicate skipped, got %q", r.Message)
	}
}

func TestForkRateCheck_ReadErrorSkips(t *testing.T) {
	c := &forkRateCheck{
		sampleInterval: time.Second,
		warnPerSec:     100,
		readProcStat:   func() (string, error) { return "", errors.New("no /proc") },
		sleep:          func(time.Duration) {},
	}
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK (read error -> skip)", r.Status)
	}
}

func TestForkRateCheck_FastUnitSleepIsNoop(t *testing.T) {
	t.Setenv("GC_FAST_UNIT", "1")
	c := newForkRateCheck()
	c.readProcStat = func() (string, error) { return "processes 1000\n", nil }
	start := time.Now()
	c.Run(nil)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Run took %v with GC_FAST_UNIT=1, want <100ms (sleep must be no-op)", elapsed)
	}
}

func TestForkRateCheck_DefaultSleepRetained(t *testing.T) {
	t.Setenv("GC_FAST_UNIT", "")
	c := newForkRateCheck()
	var slept time.Duration
	c.sleep = func(d time.Duration) { slept = d }
	c.readProcStat = func() (string, error) { return "processes 1000\n", nil }
	c.Run(nil)
	if slept != defaultForkRateSampleInterval {
		t.Fatalf("sleep called with %v, want %v (default 1s must be retained without GC_FAST_UNIT)", slept, defaultForkRateSampleInterval)
	}
}

func TestParseProcessesCounter(t *testing.T) {
	if n, ok := parseProcessesCounter("cpu 1 2\nprocesses 4242\nctxt 99\n"); !ok || n != 4242 {
		t.Fatalf("parseProcessesCounter = (%d,%v), want (4242,true)", n, ok)
	}
	if _, ok := parseProcessesCounter("cpu 1 2\nctxt 99\n"); ok {
		t.Fatalf("parseProcessesCounter should return ok=false when 'processes' is absent")
	}
}

// forkRateCheckWithPIDs builds a forkRateCheck whose /proc/stat is unreadable
// and whose platform fallback returns the given PIDs in order, exercising the
// proxy arm on every GOOS rather than only on Darwin.
func forkRateCheckWithPIDs(pids []int64, warnPerSec float64) *forkRateCheck {
	i := 0
	return &forkRateCheck{
		sampleInterval: time.Second,
		warnPerSec:     warnPerSec,
		readProcStat:   func() (string, error) { return "", errors.New("no /proc") },
		sampleFallbackCounter: func() (int64, error) {
			if i >= len(pids) {
				return 0, errors.New("no more PIDs")
			}
			p := pids[i]
			i++
			return p, nil
		},
		sleep: func(time.Duration) {},
	}
}

func TestForkRateCheck_PIDProxyUsedWhenProcStatAbsent(t *testing.T) {
	// 1000 -> 1201 over 1s. One of those allocations is the closing probe
	// itself, so the reported rate must be 200/s, not 201/s.
	c := forkRateCheckWithPIDs([]int64{1000, 1201}, 1000)
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK (200/s < 1000)", r.Status)
	}
	if !strings.Contains(r.Message, "200") {
		t.Fatalf("message should report the self-fork-corrected rate 200, got %q", r.Message)
	}
	if strings.Contains(r.Message, "201") {
		t.Fatalf("message must discount the probe's own fork, got %q", r.Message)
	}
	if !strings.Contains(r.Message, "PID-delta proxy") {
		t.Fatalf("message must name the proxy rather than present it as the kernel counter, got %q", r.Message)
	}
	if !strings.Contains(r.Message, "at least") {
		t.Fatalf("a lower-bound proxy must be reported as a lower bound, got %q", r.Message)
	}
}

func TestForkRateCheck_PIDProxyWarnsAndNamesLowerBound(t *testing.T) {
	// 5000 -> 5301 = 300/s after discounting the probe, above the 100/s warn.
	c := forkRateCheckWithPIDs([]int64{5000, 5301}, 100)
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning (300/s >= 100)", r.Status)
	}
	if r.Severity != doctor.SeverityAdvisory {
		t.Fatalf("severity = %v, want SeverityAdvisory (observability, never gates)", r.Severity)
	}
	if !strings.Contains(r.Message, "300") {
		t.Fatalf("message should report the corrected rate, got %q", r.Message)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "LOWER BOUND") {
		t.Fatalf("a proxy warning must say the real rate is at or above the figure, got details %q", joined)
	}
}

func TestForkRateCheck_PIDWraparoundSkips(t *testing.T) {
	// macOS wraps the PID space at PID_MAX; the closing sample then sorts
	// below the opening one and the delta counts nothing.
	c := forkRateCheckWithPIDs([]int64{99900, 210}, 100)
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK (wraparound -> skip, never a negative rate)", r.Status)
	}
	if !strings.Contains(r.Message, "wrapped") {
		t.Fatalf("message should name PID wraparound, got %q", r.Message)
	}
	if !strings.Contains(strings.ToLower(r.Message), "skip") {
		t.Fatalf("message should indicate skipped, got %q", r.Message)
	}
}

func TestForkRateCheck_IdleHostReportsZeroNotNegative(t *testing.T) {
	// A window in which nothing but the probe itself forked: delta 1, self 1.
	// The corrected rate is exactly 0 and must report as a rate, not a skip.
	c := forkRateCheckWithPIDs([]int64{4000, 4001}, 100)
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK", r.Status)
	}
	if strings.Contains(strings.ToLower(r.Message), "skip") {
		t.Fatalf("a zero-fork window is a measurement, not a skip, got %q", r.Message)
	}
	if !strings.Contains(r.Message, "0 forks/s") {
		t.Fatalf("message should report 0 forks/s, got %q", r.Message)
	}
}

func TestForkRateCheck_ProcStatPreferredOverProxy(t *testing.T) {
	// When both are available the kernel's own counter wins, and the proxy is
	// never spawned — the check must not fork on a host that can just read.
	proxyCalls := 0
	c := &forkRateCheck{
		sampleInterval:        time.Second,
		warnPerSec:            100,
		readProcStat:          func() (string, error) { return "processes 1000\n", nil },
		sampleFallbackCounter: func() (int64, error) { proxyCalls++; return 7, nil },
		sleep:                 func(time.Duration) {},
	}
	r := c.Run(nil)
	if proxyCalls != 0 {
		t.Fatalf("proxy called %d times; /proc/stat must be preferred and cost no fork", proxyCalls)
	}
	if !strings.Contains(r.Message, "/proc/stat") {
		t.Fatalf("message should name /proc/stat as the source, got %q", r.Message)
	}
	if strings.Contains(r.Message, "at least") {
		t.Fatalf("the kernel counter is exact and must not be hedged, got %q", r.Message)
	}
}

func TestForkRateCheck_CounterSourceChangeMidSampleSkips(t *testing.T) {
	// /proc/stat readable on the first sample and gone on the second: the two
	// ends are unrelated number lines, so their difference measures nothing.
	reads := 0
	c := &forkRateCheck{
		sampleInterval: time.Second,
		warnPerSec:     100,
		readProcStat: func() (string, error) {
			reads++
			if reads == 1 {
				return "processes 1000\n", nil
			}
			return "", errors.New("no /proc")
		},
		sampleFallbackCounter: func() (int64, error) { return 60000, nil },
		sleep:                 func(time.Duration) {},
	}
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK", r.Status)
	}
	if !strings.Contains(r.Message, "source changed") {
		t.Fatalf("mixing two counters must skip and say so, got %q", r.Message)
	}
}

func TestForkRateCheck_NoCounterAtAllSkips(t *testing.T) {
	c := &forkRateCheck{
		sampleInterval:        time.Second,
		warnPerSec:            100,
		readProcStat:          func() (string, error) { return "", errors.New("no /proc") },
		sampleFallbackCounter: func() (int64, error) { return 0, errors.New("no platform counter") },
		sleep:                 func(time.Duration) {},
	}
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK (skipped, not a false warning)", r.Status)
	}
	if !strings.Contains(strings.ToLower(r.Message), "skip") {
		t.Fatalf("message should indicate skipped, got %q", r.Message)
	}
}

func TestForkCounterTraits(t *testing.T) {
	// The kernel counter is exact and costs no fork; the proxy is a lower
	// bound and spends exactly one process creation inside the window.
	if tr := forkCounterProcStat.traits(); tr.approximate || tr.selfForks != 0 {
		t.Fatalf("procstat traits = %+v, want exact with 0 self-forks", tr)
	}
	if tr := forkCounterPIDDelta.traits(); !tr.approximate || tr.selfForks != 1 {
		t.Fatalf("pid-delta traits = %+v, want approximate with 1 self-fork", tr)
	}
}

func TestSamplePlatformForkCounter(t *testing.T) {
	n1, err := samplePlatformForkCounter()
	if runtime.GOOS != "darwin" {
		if err == nil {
			t.Fatalf("samplePlatformForkCounter should report no fallback on %s, got %d", runtime.GOOS, n1)
		}
		return
	}
	if err != nil {
		t.Fatalf("samplePlatformForkCounter on darwin: %v", err)
	}
	n2, err := samplePlatformForkCounter()
	if err != nil {
		t.Fatalf("second sample on darwin: %v", err)
	}
	if n1 <= 0 || n2 <= 0 {
		t.Fatalf("samples = (%d, %d), want positive PIDs", n1, n2)
	}
	// Darwin allocates PIDs sequentially, so a later sample sorts above an
	// earlier one — the property the whole proxy rests on. Only a wraparound
	// between the two calls breaks it, which cannot happen back-to-back here
	// without ~99k intervening allocations.
	if n2 <= n1 {
		t.Fatalf("samples = (%d, %d), want strictly increasing (sequential PID allocation)", n1, n2)
	}
}
