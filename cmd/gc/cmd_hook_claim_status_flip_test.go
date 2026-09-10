package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The bd argument vectors hookClaimWithBdStore issues: the claim mutation and
// the canonical readback that verifies it.
var (
	hookClaimFlipClaimArgs = []string{"update", "work-1", "--claim", "--json"}
	hookClaimFlipShowArgs  = []string{"show", "--json", "work-1"}
)

// hookClaimFlipBead renders a bd JSON payload for work-1 assigned to worker-1 in
// the given status. status "open" is the half-claimed shape this suite exists
// for: our assignee landed but the open -> in_progress flip did not.
func hookClaimFlipBead(status string) string {
	return `[{"id":"work-1","status":"` + status + `","assignee":"worker-1","metadata":{"gc.routed_to":"rig/worker"}}]`
}

// hookClaimFlipResponse is one scripted bd reply: either output or an error.
type hookClaimFlipResponse struct {
	out string
	err error
}

// stubHookClaimBd installs a bd command runner that answers successive claim and
// show calls from their own scripts and records every argument vector it saw, so
// a test can assert both the outcome and the exact bd traffic that produced it.
func stubHookClaimBd(t *testing.T, claims, shows []hookClaimFlipResponse) *[][]string {
	t.Helper()
	original := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = original })

	calls := &[][]string{}
	claimN, showN := 0, 0
	next := func(script []hookClaimFlipResponse, i *int, label string) ([]byte, error) {
		if *i >= len(script) {
			t.Fatalf("unexpected extra bd %s call (#%d); script has %d", label, *i+1, len(script))
		}
		resp := script[*i]
		*i++
		return []byte(resp.out), resp.err
	}
	hookClaimCommandRunnerWithEnvContext = func(_ context.Context, _ map[string]string) beads.CommandRunner {
		return func(_ string, name string, args ...string) ([]byte, error) {
			if name != "bd" {
				t.Fatalf("command name = %q, want bd", name)
			}
			*calls = append(*calls, append([]string(nil), args...))
			switch {
			case reflect.DeepEqual(args, hookClaimFlipClaimArgs):
				return next(claims, &claimN, "claim")
			case reflect.DeepEqual(args, hookClaimFlipShowArgs):
				return next(shows, &showN, "show")
			default:
				t.Fatalf("unexpected bd args: %#v", args)
				return nil, nil
			}
		}
	}
	return calls
}

func okResponses(outs ...string) []hookClaimFlipResponse {
	responses := make([]hookClaimFlipResponse, 0, len(outs))
	for _, out := range outs {
		responses = append(responses, hookClaimFlipResponse{out: out})
	}
	return responses
}

// A claim whose status flip was lost must be re-applied, not published. This is
// the half-claimed state from gascity-ib53: bd reports a successful mutation and
// the canonical readback carries our assignee, but status is still open. Publishing
// it hands the worker a bead its mandated post-claim ownership gate refuses
// (assignee matches, status does not), which drain-acks and burns the session.
func TestHookClaimWithBdStoreReappliesLostStatusFlip(t *testing.T) {
	calls := stubHookClaimBd(t,
		okResponses(hookClaimFlipBead("open"), hookClaimFlipBead("in_progress")),
		okResponses(hookClaimFlipBead("open"), hookClaimFlipBead("in_progress")),
	)

	claimed, ok, err := hookClaimWithBdStore(context.Background(), "/rig", nil, "work-1", "worker-1")
	if err != nil {
		t.Fatalf("hookClaimWithBdStore: %v", err)
	}
	if !ok {
		t.Fatal("hookClaimWithBdStore ok = false, want true once the re-applied flip persisted")
	}
	if !strings.EqualFold(strings.TrimSpace(claimed.Status), "in_progress") {
		t.Fatalf("claimed status = %q, want the re-read in_progress bead", claimed.Status)
	}
	if len(*calls) != 4 {
		t.Fatalf("bd calls = %#v, want claim, show, re-applied claim, show", *calls)
	}
}

// A flip that will not persist must surface as an operational failure, never as a
// successful claim. ok=false with an error routes it to the caller's skip path,
// which reports drain/claims_errored instead of laundering a lost write into idle
// no_work or publishing an unworkable bead.
func TestHookClaimWithBdStoreRefusesPersistentHalfClaim(t *testing.T) {
	calls := stubHookClaimBd(t,
		okResponses(hookClaimFlipBead("open"), hookClaimFlipBead("open")),
		okResponses(hookClaimFlipBead("open"), hookClaimFlipBead("open")),
	)

	_, ok, err := hookClaimWithBdStore(context.Background(), "/rig", nil, "work-1", "worker-1")
	if ok {
		t.Fatal("hookClaimWithBdStore ok = true for a bead whose status flip never persisted")
	}
	if err == nil {
		t.Fatal("hookClaimWithBdStore err = nil, want the unpersisted flip surfaced as an operational failure")
	}
	if !strings.Contains(err.Error(), "in_progress") {
		t.Fatalf("err = %v, want the missing in_progress flip named", err)
	}
	if len(*calls) != 4 {
		t.Fatalf("bd calls = %#v, want exactly one bounded re-apply attempt", *calls)
	}
}

// The verification must cost nothing on the healthy path. hookClaimWithBdStore runs
// again on every hook tick through the existing_assignment / ready_assignment
// adoption paths, so an unconditional re-apply would emit a redundant write per tick
// per in-progress bead — the flood class stampHookClaimIdentity already guards against.
func TestHookClaimWithBdStoreDoesNotReapplyWhenFlipPersisted(t *testing.T) {
	calls := stubHookClaimBd(t,
		okResponses(hookClaimFlipBead("in_progress")),
		okResponses(hookClaimFlipBead("in_progress")),
	)

	_, ok, err := hookClaimWithBdStore(context.Background(), "/rig", nil, "work-1", "worker-1")
	if err != nil {
		t.Fatalf("hookClaimWithBdStore: %v", err)
	}
	if !ok {
		t.Fatal("hookClaimWithBdStore ok = false, want true")
	}
	if len(*calls) != 2 {
		t.Fatalf("bd calls = %#v, want only claim and canonical show on the healthy path", *calls)
	}
}

// Losing the re-apply to a different live claimant is a race, not a write failure:
// it must return the winner with ok=false and NO error, so the caller emits
// bead.claim_rejected (ADR-0009) and moves to the next candidate.
func TestHookClaimWithBdStoreReportsWinnerWhenReapplyLosesRace(t *testing.T) {
	winner := `[{"id":"work-1","status":"in_progress","assignee":"worker-2","metadata":{"gc.routed_to":"rig/worker"}}]`
	calls := stubHookClaimBd(t,
		[]hookClaimFlipResponse{
			{out: hookClaimFlipBead("open")},
			{out: "already assigned to worker-2", err: errors.New("exit status 1")},
		},
		okResponses(hookClaimFlipBead("open"), winner),
	)

	claimed, ok, err := hookClaimWithBdStore(context.Background(), "/rig", nil, "work-1", "worker-1")
	if err != nil {
		t.Fatalf("hookClaimWithBdStore: %v, want a lost race reported as ok=false without error", err)
	}
	if ok {
		t.Fatal("hookClaimWithBdStore ok = true after losing the re-apply to another claimant")
	}
	if claimed.Assignee != "worker-2" {
		t.Fatalf("claimed assignee = %q, want the winning claimant so the rejection event can name it", claimed.Assignee)
	}
	if len(*calls) != 4 {
		t.Fatalf("bd calls = %#v, want claim, show, re-applied claim, winner show", *calls)
	}
}
