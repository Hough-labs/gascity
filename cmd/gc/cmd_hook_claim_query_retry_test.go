package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// transientWorkQueryErr reproduces the exact error shellWorkQueryWithEnv returns
// when the work query exceeds hookWorkQueryTimeout: the human-facing "timed out
// after" text wrapping the context.DeadlineExceeded sentinel. Both halves matter
// — dispatch.IsTransientControllerError classifies on the sentinel AND on the
// "running work query ... timed out after" message shape.
func transientWorkQueryErr(command string) error {
	return fmt.Errorf("running work query %q: timed out after %s: %w", command, hookWorkQueryTimeout, context.DeadlineExceeded)
}

// fakeRetryClock replaces the retry backoff sleep for the duration of a test and
// records what the retry policy asked to wait, so the budget can be asserted
// without paying real wall time.
func fakeRetryClock(t *testing.T) *[]time.Duration {
	t.Helper()
	slept := []time.Duration{}
	prev := hookWorkQuerySleep
	hookWorkQuerySleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { hookWorkQuerySleep = prev })
	return &slept
}

func retryTestStores() []hookStore {
	return []hookStore{{dir: "city", env: []string{"GC_AGENT=worker"}}}
}

func retryTestOpts() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}
}

func retryTestOps(claimed *string) hookClaimOps {
	return hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			if claimed != nil {
				*claimed = beadID
			}
			return beads.Bead{
				ID:       beadID,
				Status:   "in_progress",
				Assignee: assignee,
				Metadata: map[string]string{"gc.routed_to": "worker"},
			}, true, nil
		},
		EmitClaimRejected: func(string, string, string) {},
		ResolveWorkBranch: func(string) string { return "" },
	}
}

const retryTestReadyWork = `[{"id":"hw-city","status":"open","metadata":{"gc.routed_to":"worker"}}]`

// A work query that times out once and succeeds immediately afterwards must not
// burn the session: the field report (gascity-3dz7) measured the very next
// attempt succeeding once Dolt released the schema-migration lock, so a
// sub-minute lock must not convert into a total claim failure.
func TestHookClaimRetriesTransientWorkQueryFailure(t *testing.T) {
	fakeRetryClock(t)
	calls := 0
	run := func(command, _ string, _ []string) (string, error) {
		calls++
		if calls == 1 {
			return "", transientWorkQueryErr(command)
		}
		return retryTestReadyWork, nil
	}

	var claimed string
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("bd ready --json", "city", nil, retryTestStores(),
		retryTestOpts(), retryTestOps(&claimed), run, func(string, error) {}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("claimHookWorkWithRunner = %d, want 0 (transient timeout must be retried); stderr=%s", code, stderr.String())
	}
	if calls < 2 {
		t.Fatalf("runner called %d times, want at least 2 (the retry)", calls)
	}
	if claimed != "hw-city" {
		t.Fatalf("claimed = %q, want %q", claimed, "hw-city")
	}
}

// The criterion that prevents the silent false-idle: once retries are exhausted
// the emitted JSON must not read as an idle pool. Asserted on the JSON body, not
// on the exit code, because a drain-acking wrapper only ever sees the body.
func TestHookClaimExhaustedTimeoutIsNotReportedAsNoWork(t *testing.T) {
	fakeRetryClock(t)
	run := func(command, _ string, _ []string) (string, error) {
		return "", transientWorkQueryErr(command)
	}

	var stdout, stderr bytes.Buffer
	claimHookWorkWithRunner("bd ready --json", "city", nil, retryTestStores(),
		retryTestOpts(), retryTestOps(nil), run, func(string, error) {}, &stdout, &stderr)

	if stdout.Len() == 0 {
		t.Fatalf("no JSON result emitted after exhausted retries; stderr=%s", stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Reason == hookClaimReasonNoWork {
		t.Fatalf("reason = %q, want a reason distinguishable from an idle pool", result.Reason)
	}
	if result.Action != "drain" {
		t.Fatalf("action = %q, want %q", result.Action, "drain")
	}
	if result.Reason == "" {
		t.Fatal("reason is empty, want an explicit query-failure reason")
	}
}

// A work query that is genuinely broken (not transient) must stay a hard
// failure and must NOT be retried, so the retry cannot mask real breakage.
func TestHookClaimDoesNotRetryNonTransientWorkQueryFailure(t *testing.T) {
	calls := 0
	run := func(command, _ string, _ []string) (string, error) {
		calls++
		return "", fmt.Errorf("running work query %q: %w", command, fmt.Errorf("exit status 127: bd: command not found"))
	}

	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("bd ready --json", "city", nil, retryTestStores(),
		retryTestOpts(), retryTestOps(nil), run, func(string, error) {}, &stdout, &stderr)

	if calls != 1 {
		t.Fatalf("runner called %d times, want exactly 1 (a hard failure must not be retried)", calls)
	}
	if code == 0 {
		t.Fatal("claimHookWorkWithRunner = 0, want non-zero for a non-transient work-query failure")
	}
}

// An empty pool must still drain as an ordinary no_work: the fix must not
// relabel a healthy idle store.
func TestHookClaimEmptyPoolStillDrainsNoWork(t *testing.T) {
	run := func(string, string, []string) (string, error) { return `[]`, nil }

	var stdout, stderr bytes.Buffer
	claimHookWorkWithRunner("bd ready --json", "city", nil, retryTestStores(),
		retryTestOpts(), retryTestOps(nil), run, func(string, error) {}, &stdout, &stderr)

	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("action/reason = %q/%q, want drain/%s", result.Action, result.Reason, hookClaimReasonNoWork)
	}
}

// The retry budget must terminate deterministically rather than hanging, and it
// must not sleep for real: a fully-timing-out runner returns after a bounded
// number of attempts.
func TestHookClaimRetryBudgetIsBounded(t *testing.T) {
	fakeRetryClock(t)
	calls := 0
	run := func(command, _ string, _ []string) (string, error) {
		calls++
		if calls > 20 {
			t.Fatal("runner called >20 times: retry budget is unbounded")
		}
		return "", transientWorkQueryErr(command)
	}

	done := make(chan int, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		done <- claimHookWorkWithRunner("bd ready --json", "city", nil, retryTestStores(),
			retryTestOpts(), retryTestOps(nil), run, func(string, error) {}, &stdout, &stderr)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("claimHookWorkWithRunner did not return: retry budget is not bounded")
	}
	if calls < 2 {
		t.Fatalf("runner called %d times, want at least 2 (a retry must be attempted)", calls)
	}
}

// The decorator is the single seam both query sites share, so its call-count and
// budget contract is asserted directly rather than inferred from the claim path
// (which runs the query more than once per claim: discovery, then claim-time
// re-validation).
func TestRetryTransientWorkQueryRetriesExactlyOnceThenSucceeds(t *testing.T) {
	slept := fakeRetryClock(t)
	calls := 0
	run := retryTransientWorkQuery(func(command, _ string, _ []string) (string, error) {
		calls++
		if calls == 1 {
			return "", transientWorkQueryErr(command)
		}
		return "ok", nil
	})

	out, err := run("bd ready --json", "city", nil)
	if err != nil {
		t.Fatalf("err = %v, want nil (the retry must succeed)", err)
	}
	if out != "ok" {
		t.Fatalf("out = %q, want %q", out, "ok")
	}
	if calls != 2 {
		t.Fatalf("runner called %d times, want exactly 2", calls)
	}
	if len(*slept) != 1 || (*slept)[0] != hookWorkQueryRetryBackoff {
		t.Fatalf("backoff sleeps = %v, want exactly one %s pause", *slept, hookWorkQueryRetryBackoff)
	}
}

func TestRetryTransientWorkQueryStopsAtAttemptBudget(t *testing.T) {
	slept := fakeRetryClock(t)
	calls := 0
	run := retryTransientWorkQuery(func(command, _ string, _ []string) (string, error) {
		calls++
		return "", transientWorkQueryErr(command)
	})

	if _, err := run("bd ready --json", "city", nil); err == nil {
		t.Fatal("err = nil, want the exhausted transient failure to surface")
	}
	if calls != hookWorkQueryRetryAttempts {
		t.Fatalf("runner called %d times, want exactly hookWorkQueryRetryAttempts (%d)", calls, hookWorkQueryRetryAttempts)
	}
	// One fewer pause than attempts: the budget must not sleep after the last try.
	if len(*slept) != hookWorkQueryRetryAttempts-1 {
		t.Fatalf("backoff sleeps = %d, want %d (no sleep after the final attempt)", len(*slept), hookWorkQueryRetryAttempts-1)
	}
}

func TestRetryTransientWorkQueryDoesNotRetryHardFailures(t *testing.T) {
	fakeRetryClock(t)
	calls := 0
	run := retryTransientWorkQuery(func(string, string, []string) (string, error) {
		calls++
		return "", fmt.Errorf("exit status 127: bd: command not found")
	})

	if _, err := run("bd ready --json", "city", nil); err == nil {
		t.Fatal("err = nil, want the hard failure to surface")
	}
	if calls != 1 {
		t.Fatalf("runner called %d times, want exactly 1", calls)
	}
}

func TestRetryTransientWorkQueryPassesThroughSuccess(t *testing.T) {
	fakeRetryClock(t)
	calls := 0
	run := retryTransientWorkQuery(func(string, string, []string) (string, error) {
		calls++
		return "rows", nil
	})

	out, err := run("bd ready --json", "city", nil)
	if err != nil || out != "rows" {
		t.Fatalf("run() = %q, %v; want %q, nil", out, err, "rows")
	}
	if calls != 1 {
		t.Fatalf("runner called %d times, want exactly 1 on the happy path", calls)
	}
}

// The exit contract for an exhausted timeout, pinned deliberately: it follows
// the drain contract every other operational-failure reason follows
// (claims_errored), so a --drain-ack wrapper acknowledges and exits 0 while a
// plain caller still gets a non-zero exit. The signal that the data plane failed
// rides on the reason field and the event bus, not on a bespoke exit code.
func TestHookClaimExhaustedTimeoutExitContract(t *testing.T) {
	for _, tc := range []struct {
		name     string
		drainAck bool
		want     int
	}{
		{name: "without drain-ack", drainAck: false, want: 1},
		{name: "with drain-ack", drainAck: true, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeRetryClock(t)
			run := func(command, _ string, _ []string) (string, error) {
				return "", transientWorkQueryErr(command)
			}
			opts := retryTestOpts()
			opts.DrainAck = tc.drainAck
			ops := retryTestOps(nil)
			ops.DrainAck = func(io.Writer) error { return nil }

			var emitted error
			var stdout, stderr bytes.Buffer
			code := claimHookWorkWithRunner("bd ready --json", "city", nil, retryTestStores(),
				opts, ops, run, func(_ string, err error) { emitted = err }, &stdout, &stderr)

			if code != tc.want {
				t.Fatalf("claimHookWorkWithRunner = %d, want %d", code, tc.want)
			}
			// The reconciler must still see the timeout regardless of exit code.
			if emitted == nil {
				t.Fatal("work-query failure was not emitted to the event bus")
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
			}
			if result.Reason != hookClaimReasonQueryTimeout {
				t.Fatalf("reason = %q, want %q", result.Reason, hookClaimReasonQueryTimeout)
			}
			if result.DrainAcknowledged != tc.drainAck {
				t.Fatalf("drain_acknowledged = %v, want %v", result.DrainAcknowledged, tc.drainAck)
			}
		})
	}
}
