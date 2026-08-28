package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/doctor"
)

// forkRateCheck reports the host's process-creation (fork) rate — the dominant,
// and routinely misdiagnosed, driver of "high load" on Gas City hosts.
//
// Gas City's CLI-agent + per-command data plane means most gc/bd operations
// fork: `gc` forks `bd.real` per command, which in turn talks to a per-city
// `dolt` sql-server. A busy city therefore spends its load on *process churn*,
// not CPU work. Operators repeatedly misread the resulting high load average as
// CPU saturation — it is not: the load average counts runnable + uninterruptible
// tasks, which a fork storm inflates while CPU may be far from saturated
// (measured in the field at load ~25 with CPU ~66% busy and ~96% of forks coming
// from bd.real + dolt + gc, vs ~0.4% from the agents themselves). This check
// surfaces the actual fork rate so the misdiagnosis is caught early and the
// operator can reach for the right remedy.
//
// Pure observability (SeverityAdvisory): it never mutates anything and never
// gates. It reads the kernel's own cumulative fork counter from /proc/stat
// where that exists, and falls back to the platform counter described by
// forkCounterKind (on Darwin, a sequential-PID proxy) where it does not. A host
// with neither reports OK and skips. The durable remedies it points at are the
// embedded DoltLite backend (no per-city dolt sql-server) and the in-process
// bead store (no gc->bd.real fork per command); this check is the watch that
// tells an operator whether those are worth adopting.
type forkRateCheck struct {
	// sampleInterval is the window over which the fork delta is measured.
	sampleInterval time.Duration
	// warnPerSec is the forks/sec at or above which the check warns.
	warnPerSec float64
	// readProcStat returns the contents of /proc/stat. Injectable for tests.
	readProcStat func() (string, error)
	// sampleFallbackCounter returns the platform's fork counter for hosts with
	// no readable /proc/stat, per samplePlatformForkCounter. Injectable for
	// tests so the fallback arm is exercised on every GOOS, not only Darwin.
	sampleFallbackCounter func() (int64, error)
	// sleep waits the sample interval. Injectable for tests.
	sleep func(time.Duration)
}

const (
	defaultForkRateSampleInterval = time.Second
	// defaultForkRateWarnPerSec is a heuristic starting threshold. Sustained
	// process creation above this on a steady-state city is almost always the
	// per-command fork cascade (gc -> bd.real -> dolt), not real work. It is a
	// field-tuned default, not a hard limit; expose it via config if a city's
	// healthy baseline legitimately runs hotter.
	defaultForkRateWarnPerSec = 100.0
)

// forkCounterKind identifies which monotone process-creation counter a pair of
// samples came from. Every kind is read the same way — delta over a window —
// but they differ in exactness, in what a backwards delta means, and in how
// many process creations the sampling itself contributes. forkCounterTraits
// carries those differences so the reported number is never presented as more
// than the counter behind it can support.
type forkCounterKind int

const (
	// forkCounterNone means no counter could be read at all.
	forkCounterNone forkCounterKind = iota
	// forkCounterProcStat is Linux /proc/stat's cumulative "processes" field:
	// the kernel's own fork count, incremented on every fork/clone.
	forkCounterProcStat
	// forkCounterPIDDelta is the sequential-PID proxy used where no cumulative
	// counter exists (Darwin). See doctor_fork_rate_darwin.go.
	forkCounterPIDDelta
)

// forkCounterTraits describes how one counter kind must be corrected and
// described in operator-facing output.
type forkCounterTraits struct {
	// source names the counter in the reported message.
	source string
	// approximate is true when the counter only bounds the true rate from
	// below, so the reported figure must be qualified rather than stated.
	approximate bool
	// selfForks is the number of process creations the sampling itself adds
	// inside the measured window, subtracted before the rate is reported.
	selfForks int64
	// backwards explains a negative delta for this counter.
	backwards string
}

// traits returns the reporting and correction rules for this counter kind.
func (k forkCounterKind) traits() forkCounterTraits {
	switch k {
	case forkCounterProcStat:
		return forkCounterTraits{
			source:      "/proc/stat",
			approximate: false,
			// Reading a file forks nothing.
			selfForks: 0,
			backwards: "counter went backwards (reboot mid-sample?)",
		}
	case forkCounterPIDDelta:
		return forkCounterTraits{
			source:      "PID-delta proxy",
			approximate: true,
			// Each sample spawns one probe process. The opening sample is the
			// window's own start, so exactly one probe — the closing one —
			// falls inside the measured window.
			selfForks: 1,
			backwards: "PID counter wrapped mid-sample",
		}
	default:
		return forkCounterTraits{source: "unknown", approximate: true, backwards: "counter went backwards"}
	}
}

func newForkRateCheck() *forkRateCheck {
	fr := &forkRateCheck{
		sampleInterval: defaultForkRateSampleInterval,
		warnPerSec:     defaultForkRateWarnPerSec,
		readProcStat: func() (string, error) {
			b, err := os.ReadFile("/proc/stat")
			return string(b), err
		},
		sampleFallbackCounter: samplePlatformForkCounter,
		sleep:                 time.Sleep,
	}
	// Unit-cover lane (GC_FAST_UNIT=1): skip the 1s sample wait. The check is
	// still registered and still emits its result; the rate it reports is
	// irrelevant in CI. ~27 doDoctor() call sites × 1s = ~27s of otherwise-
	// wasted budget in the 8-minute cmd/gc cover run.
	if strings.TrimSpace(os.Getenv("GC_FAST_UNIT")) == "1" {
		fr.sleep = func(time.Duration) {}
	}
	return fr
}

func (c *forkRateCheck) Name() string                     { return "fork-rate" }
func (c *forkRateCheck) CanFix() bool                     { return false }
func (c *forkRateCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (c *forkRateCheck) WarmupEligible() bool             { return false }

// parseProcessesCounter extracts the cumulative "processes" (fork) counter from
// /proc/stat contents. The kernel increments it on every fork/clone of a new
// task. Returns ok=false when the field is absent (e.g. a non-Linux host).
func parseProcessesCounter(stat string) (int64, bool) {
	for _, line := range strings.Split(stat, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "processes" {
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, false
			}
			return n, true
		}
	}
	return 0, false
}

func (c *forkRateCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	res := &doctor.CheckResult{Name: c.Name(), Severity: doctor.SeverityAdvisory}

	n1, kind1, ok := c.sampleProcessesCounter()
	if !ok {
		res.Status = doctor.StatusOK
		res.Message = "fork-rate: no process-creation counter on this host (no /proc/stat, no platform fallback) — skipped"
		return res
	}
	c.sleep(c.sampleInterval)
	n2, kind2, ok := c.sampleProcessesCounter()
	if !ok {
		res.Status = doctor.StatusOK
		res.Message = "fork-rate: second counter read failed — skipped"
		return res
	}
	// A window whose ends come from different counters measures nothing: the
	// two are unrelated number lines, so their difference is meaningless.
	if kind1 != kind2 {
		res.Status = doctor.StatusOK
		res.Message = "fork-rate: counter source changed mid-sample — skipped"
		return res
	}
	traits := kind1.traits()

	secs := c.sampleInterval.Seconds()
	if secs <= 0 {
		secs = 1
	}
	// Discount the sampling's own process creations before reporting, so the
	// probe cannot show up as the churn it is measuring.
	perSec := float64(n2-n1-traits.selfForks) / secs
	if perSec < 0 {
		// The counter is not monotone across this window, so the delta counts
		// nothing. Treat as unknown rather than reporting a negative rate.
		res.Status = doctor.StatusOK
		res.Message = fmt.Sprintf("fork-rate: %s — skipped", traits.backwards)
		return res
	}

	// A proxy bounds the rate from below, so its figure is reported as an
	// approximation and named as a proxy rather than as the kernel's count.
	rate := fmt.Sprintf("%.0f forks/s", perSec)
	if traits.approximate {
		rate = fmt.Sprintf("at least %.0f forks/s", perSec)
	}

	if perSec >= c.warnPerSec {
		res.Status = doctor.StatusWarning
		res.Message = fmt.Sprintf("high process fork rate: %s (warn >= %.0f/s, via %s) — likely the per-command data-plane fork storm, not CPU load", rate, c.warnPerSec, traits.source)
		res.Details = []string{
			"A high fork rate — not CPU work — is what inflates the load average on Gas City hosts:",
			"the load average counts runnable + uninterruptible tasks, so a fork storm reads as 'high",
			"load' while CPU may be far from saturated. Don't infer CPU saturation from load alone.",
			"Usual driver: the per-command data plane — gc forks bd.real per command, which talks to a",
			"per-city dolt sql-server (gc + bd.real + dolt typically dominate; the agents are a rounding error).",
			"Confirm the sources (needs root): bpftrace -e 'tracepoint:sched:sched_process_fork { @[comm] = count(); }'",
			"Durable remedies: the embedded DoltLite backend (no per-city dolt sql-server) and the",
			"in-process bead store (no gc->bd.real fork per command).",
		}
		if traits.approximate {
			res.Details = append(res.Details,
				fmt.Sprintf("Measured via the %s, which counts process creations as a LOWER BOUND — the real rate is at or above this.", traits.source))
		}
		return res
	}

	res.Status = doctor.StatusOK
	res.Message = fmt.Sprintf("process fork rate: %s (sampled over %s via %s)", rate, c.sampleInterval, traits.source)
	return res
}

// sampleProcessesCounter reads the host's monotone process-creation counter
// once and reports which kind it came from.
//
// /proc/stat is preferred wherever it is readable: it is the kernel's own
// cumulative fork count, so it is exact and costs no process creation. Only
// when it is absent or unparsable does this fall through to the platform
// counter, which on Darwin is the sequential-PID proxy
// (doctor_fork_rate_darwin.go) and elsewhere does not exist. Returns ok=false
// when neither is available, which is what keeps the check's honest skip.
func (c *forkRateCheck) sampleProcessesCounter() (int64, forkCounterKind, bool) {
	if c.readProcStat != nil {
		if stat, err := c.readProcStat(); err == nil {
			if n, ok := parseProcessesCounter(stat); ok {
				return n, forkCounterProcStat, true
			}
		}
	}
	if c.sampleFallbackCounter == nil {
		return 0, forkCounterNone, false
	}
	n, err := c.sampleFallbackCounter()
	if err != nil {
		return 0, forkCounterNone, false
	}
	return n, forkCounterPIDDelta, true
}
