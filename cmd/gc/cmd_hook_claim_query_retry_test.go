package main

import (
	"context"
	"fmt"
	"testing"
)

// transientWorkQueryErr reproduces the exact error shellWorkQueryWithEnv returns
// when the work query exceeds hookWorkQueryTimeout: the human-facing "timed out
// after" text wrapping the context.DeadlineExceeded sentinel.
func transientWorkQueryErr(command string) error {
	return fmt.Errorf("running work query %q: timed out after %s: %w", command, hookWorkQueryTimeout, context.DeadlineExceeded)
}

// TestClaimRevalidationTransientFailureIsRetried pins the one read
// selectStoreWithWorkRetrying does not cover. On a federated city the claim
// re-reads the selected primary store after discovery (claimStoreWithFallback),
// and a transient fault on that second read — Dolt briefly holding its
// schema-migration lock, gascity-3dz7 — must not fail a claim discovery just
// proved has work. The re-read gets the same bounded retry discovery does.
func TestClaimRevalidationTransientFailureIsRetried(t *testing.T) {
	withFastClaimRetries(t)
	h := newFailureHookHarness()
	h.script("/rig",
		hookRunnerAnswer{out: routedRowJSON("wb-1")},               // discovery
		hookRunnerAnswer{err: transientWorkQueryErr("work-query")}, // re-validation blip
		hookRunnerAnswer{out: routedRowJSON("wb-1")},               // re-validation retried
	)
	h.script("/city", hookRunnerAnswer{out: "[]"})
	stores := []hookStore{
		{dir: "/rig", env: []string{"BEADS_DIR=/rig"}},
		{dir: "/city", env: []string{"BEADS_DIR=/city"}, command: "bd ready --json"},
	}

	result, code, stderr := runFailureClaim(t, h, stores)

	if code != 0 {
		t.Fatalf("exit = %d, want 0: a transient re-validation fault must be retried; stderr=%s", code, stderr)
	}
	if result.Action != "work" || result.BeadID != "wb-1" {
		t.Fatalf("result = %+v, want wb-1 claimed after the retried re-validation", result)
	}
	if h.drained {
		t.Fatal("drain acknowledged for a seat that claimed work")
	}
}
