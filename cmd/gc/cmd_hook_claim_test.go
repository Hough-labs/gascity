package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestHookClaimWithBdStoreReloadsCanonicalBeadAfterPartialMutation(t *testing.T) {
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })

	var calls [][]string
	hookClaimCommandRunnerWithEnvContext = func(_ context.Context, _ map[string]string) beads.CommandRunner {
		return func(_ string, name string, args ...string) ([]byte, error) {
			if name != "bd" {
				t.Fatalf("command name = %q, want bd", name)
			}
			calls = append(calls, append([]string(nil), args...))
			switch {
			case reflect.DeepEqual(args, []string{"update", "work-1", "--claim", "--json"}):
				return []byte(`[{"id":"work-1","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"rig/worker"}}]`), nil
			case reflect.DeepEqual(args, []string{"show", "--json", "work-1"}):
				return []byte(`[{"id":"work-1","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"rig/worker","gc.root_bead_id":"root-1","gc.continuation_group":"review"}}]`), nil
			default:
				t.Fatalf("unexpected bd args: %#v", args)
				return nil, nil
			}
		}
	}

	claimed, ok, err := hookClaimWithBdStore(context.Background(), "/rig", nil, "work-1", "worker-1")
	if err != nil {
		t.Fatalf("hookClaimWithBdStore: %v", err)
	}
	if !ok {
		t.Fatal("hookClaimWithBdStore ok = false, want true")
	}
	if claimed.Metadata["gc.root_bead_id"] != "root-1" || claimed.Metadata["gc.continuation_group"] != "review" {
		t.Fatalf("claimed metadata = %#v, want canonical root and continuation group", claimed.Metadata)
	}
	if len(calls) != 2 {
		t.Fatalf("bd calls = %#v, want claim update followed by canonical show", calls)
	}
}

func TestDoHookClaimStopsAfterCommittedClaimReadbackFailure(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[
			{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker"}},
			{"id":"work-2","status":"open","metadata":{"gc.routed_to":"worker"}}
		]`, nil
	}
	var attempts []string
	drained := false
	ops := hookClaimOps{
		Runner: runner,
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			attempts = append(attempts, beadID)
			return beads.Bead{ID: beadID, Assignee: assignee}, true, errors.New("canonical read failed")
		},
		DrainAck: func(io.Writer) error {
			drained = true
			return nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		DrainAck:     true,
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("doHookClaim = %d, want 1", code)
	}
	if got := strings.Join(attempts, ","); got != "work-1" {
		t.Fatalf("claim attempts = %q, want only committed work-1", got)
	}
	if drained {
		t.Fatal("drain acknowledged after committed claim readback failure")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "claimed work-1 but loading canonical bead failed") {
		t.Fatalf("stderr = %q, want committed-claim diagnostic", stderr.String())
	}
}

func TestDoHookClaimUsesSelectedStoreContextForMutationAndContinuation(t *testing.T) {
	var claimedDir string
	var claimedEnv []string
	var listedDir string
	var listedEnv []string
	var assignedDir string
	var assignedEnv []string
	var assignedBead string

	storeDir := "rig-store"
	storeEnv := []string{"BEADS_DIR=rig-store", "GC_RIG_ROOT=rig-root"}
	candidates := []beads.Bead{{
		ID:       "bead-1",
		Status:   "open",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.run_target": "route-1", "gc.root_bead_id": "root-1", "gc.continuation_group": "group-a"},
	}}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimedDir = dir
			claimedEnv = append([]string(nil), env...)
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress", Metadata: candidates[0].Metadata}, true, nil
		},
		ListContinuation: func(_ context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
			listedDir = dir
			listedEnv = append([]string(nil), env...)
			if rootID != "root-1" || group != "group-a" {
				t.Fatalf("continuation lookup = (%q, %q), want (root-1, group-a)", rootID, group)
			}
			return []beads.Bead{{ID: "sib-1", Status: "open", Metadata: candidates[0].Metadata}}, nil
		},
		AssignContinuation: func(_ context.Context, dir string, env []string, beadID, assignee string) error {
			assignedDir = dir
			assignedEnv = append([]string(nil), env...)
			assignedBead = beadID
			if assignee != "worker-1" {
				t.Fatalf("assignee = %q, want worker-1", assignee)
			}
			return nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", storeDir, hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		Env:                storeEnv,
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if claimedDir != storeDir {
		t.Fatalf("claimedDir = %q, want %q", claimedDir, storeDir)
	}
	if listedDir != storeDir {
		t.Fatalf("listedDir = %q, want %q", listedDir, storeDir)
	}
	if assignedDir != storeDir {
		t.Fatalf("assignedDir = %q, want %q", assignedDir, storeDir)
	}
	if !reflect.DeepEqual(claimedEnv, storeEnv) {
		t.Fatalf("claimedEnv = %#v, want %#v", claimedEnv, storeEnv)
	}
	if !reflect.DeepEqual(listedEnv, storeEnv) {
		t.Fatalf("listedEnv = %#v, want %#v", listedEnv, storeEnv)
	}
	if !reflect.DeepEqual(assignedEnv, storeEnv) {
		t.Fatalf("assignedEnv = %#v, want %#v", assignedEnv, storeEnv)
	}
	if assignedBead != "sib-1" {
		t.Fatalf("assignedBead = %q, want sib-1", assignedBead)
	}
}

// TestDoHookClaimSkipsBlockedRoutedHeadAndClaimsReadyBehindIt guards the
// widened-routed-tier fix: a routed tier's oldest candidate can be
// is_blocked (e.g. gated on a PR), and the hook must fall through to a
// Ready routed bead behind it rather than idle-exiting on the blocked head.
func TestDoHookClaimSkipsBlockedRoutedHeadAndClaimsReadyBehindIt(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "blocked-head", Status: "open", IsBlocked: boolPtr(true), Metadata: map[string]string{"gc.routed_to": "route-1"}},
		{ID: "ready-behind", Status: "open", Metadata: map[string]string{"gc.routed_to": "route-1"}},
	}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	var claimedBead string
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimedBead = beadID
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if claimedBead != "ready-behind" {
		t.Fatalf("claimedBead = %q, want ready-behind (blocked-head must be skipped)", claimedBead)
	}
}

// A bead that reaches the ready_assignment adoption path is open and already
// assigned to this session — the shape preassignHookContinuationGroup leaves on
// every continuation sibling, since it writes the assignee alone. Handing it back
// as work without flipping it to in_progress publishes a bead that a worker's
// post-claim ownership gate is right to refuse (assignee matches, status does
// not), which drain-acks the session into a respawn loop against a bead nothing
// will ever move. The adoption must perform the same atomic claim a fresh
// candidate gets. See gascity-hyl.
func TestDoHookClaimFlipsReadyAssignmentToInProgress(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[{"id":"step-2","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.continuation_group":"pool-workflow"}}]`, nil
	}
	var claimed []string
	ops := hookClaimOps{
		Runner: runner,
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimed = append(claimed, beadID)
			return beads.Bead{
				ID:       beadID,
				Status:   "in_progress",
				Assignee: assignee,
				Metadata: map[string]string{"gc.routed_to": "worker", "gc.root_bead_id": "root-1", "gc.continuation_group": "pool-workflow"},
			}, true, nil
		},
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return nil, nil
		},
		DrainAck: func(io.Writer) error {
			t.Fatal("drain acknowledged for adoptable ready work")
			return nil
		},
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim(ready assignment) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.Join(claimed, ","); got != "step-2" {
		t.Fatalf("claim attempts = %q, want the adopted bead step-2 to be claimed", got)
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "work" || result.Reason != "ready_assignment" || result.BeadID != "step-2" {
		t.Fatalf("unexpected claim result: %+v", result)
	}
	if result.Assignee != "worker-1" {
		t.Fatalf("result assignee = %q, want worker-1", result.Assignee)
	}
}

// Adoption is a claim, so it can lose. When the flip reports another claimant won,
// the bead must not be published as this session's work — publishing it would hand
// out a bead this session does not own.
func TestDoHookClaimDoesNotPublishReadyAssignmentLostToAnotherClaimant(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[{"id":"step-2","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`, nil
	}
	drained := false
	ops := hookClaimOps{
		Runner: runner,
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: "worker-2"}, false, nil
		},
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return nil, nil
		},
		DrainAck: func(io.Writer) error {
			drained = true
			return nil
		},
		EmitClaimRejected: func(string, string, string) {},
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim(lost adoption) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !drained {
		t.Fatal("lost adoption did not drain")
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "drain" {
		t.Fatalf("action = %q, want drain; result=%+v", result.Action, result)
	}
}

// A failed adoption write is an operational fault, not an idle store. It must
// drain with claims_errored so the write failure stays visible rather than being
// laundered into no_work.
func TestDoHookClaimReportsClaimsErroredWhenReadyAssignmentFlipFails(t *testing.T) {
	runner := func(string, string) (string, error) {
		return `[{"id":"step-2","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`, nil
	}
	ops := hookClaimOps{
		Runner: runner,
		Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			return beads.Bead{}, false, errors.New("store write failed")
		},
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return nil, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim(failed adoption) = %d, want 0; stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonClaimsErrored {
		t.Fatalf("result = %+v, want drain/claims_errored", result)
	}
}
