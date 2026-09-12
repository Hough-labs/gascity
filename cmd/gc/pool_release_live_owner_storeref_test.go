package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// Reproduction for gascity-jrlv: a live, open session that genuinely holds an
// in-progress work bead must never be judged NOT to own it. That verdict is what
// authorizes the pool reconciler to release the bead out from under a running
// worker, which is what happened to winnow-iaroy on 2026-09-07 — twice, to an
// owner that was awake throughout.
//
// The reachable shape, established by enumerating the resolution space:
//
//	agent Dir            resolved  store-ref        owns rig-store work
//	rig name             true      "winnow"         yes
//	rig path             true      "winnow"         yes
//	maps to NO rig       true      ""               NO   <-- this test
//	maps to NO rig, city true      "\x00crossstore" yes
//	unresolvable agent   false     "\x00unresolved" yes
//
// The hinge is that "" is NOT a wildcard. openSessionReachableStoreRefInfo has
// two wildcard escapes ("\x00unresolved" for an unresolvable agent,
// "\x00crossstore" for a city-scoped one), and both fail OPEN — they keep the
// work. A non-city-scoped agent whose rig cannot be resolved falls through to
// workdir.ConfiguredRigName, which returns "" for a Dir matching no rig NAME and
// no rig PATH. That "" is then compared literally against the work-side ref,
// which coordClassStoreCandidates stamps as rig.Name for a rig store. "" never
// equals "winnow", so the live holder is judged to own only city-store work and
// its rig-routed work is released.
//
// This asserts the INVARIANT, not a particular remedy: whether the fix is a
// wildcard fallback for an unresolvable rig (fail open, consistent with the two
// existing escapes) or a liveness assertion at the release site, a live holder
// must come out owning its work.
func TestLiveRigScopedSessionOwnsItsRigStoreWork(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "winnow")

	cfg := &config.City{
		// Resolvable (bare dir/name identity matches), rig-scoped, and Dir maps to
		// neither the rig's name nor its path.
		Agents: []config.Agent{{Name: "polecat", Scope: "rig", Dir: "notarig"}},
		Rigs:   []config.Rig{{Name: "winnow", Path: rigPath}},
	}

	const assignee = "gastown__polecat-gc-8a4d"
	info := sessiontest.SeedBead(t, beads.Bead{
		ID:       "ga-live-owner",
		Type:     session.BeadType,
		Status:   "open", // alive and awake, exactly as in the incident
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"template": "notarig/polecat", "session_name": assignee},
	})

	// Guard the premise: if the agent stops resolving, this test silently becomes
	// the wildcard case and would pass for the wrong reason.
	if sessionAgentConfigInfo(cfg, info) == nil {
		t.Fatal("fixture no longer resolves its agent; the test would pass via the unresolved wildcard, not the path under test")
	}

	index := makeOpenSessionStoreRefIndex(cityPath, cfg, []session.Info{info}, true)

	// Work lives in the winnow rig store, so its ref is the rig name.
	const workStoreRef = "winnow"

	if !openSessionOwnsWork(nil, index, assignee, workStoreRef, true) {
		t.Fatalf("live session %q was judged NOT to own its in-progress work in store %q; "+
			"session indexed under store-ref %q (wildcards are %q / %q). "+
			"This verdict is what authorizes releasing a live worker's bead (gascity-jrlv).",
			assignee, workStoreRef,
			openSessionReachableStoreRefInfo(cityPath, cfg, info),
			unresolvedOpenSessionStoreRef, crossStoreOpenSessionStoreRef)
	}
}

// Control: the same session whose Dir IS the rig name resolves to "winnow" and
// owns its work. This pins the failure above to the unresolvable-Dir shape
// specifically, rather than the harness, the session metadata, or the assignee
// identity extraction.
func TestLiveRigScopedSessionOwnsWorkWhenDirResolvesToRig(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "winnow")

	cfg := &config.City{
		Agents: []config.Agent{{Name: "polecat", Scope: "rig", Dir: "winnow"}},
		Rigs:   []config.Rig{{Name: "winnow", Path: rigPath}},
	}

	const assignee = "gastown__polecat-gc-8a4d"
	info := sessiontest.SeedBead(t, beads.Bead{
		ID:       "ga-live-owner-ok",
		Type:     session.BeadType,
		Status:   "open",
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"template": "winnow/polecat", "session_name": assignee},
	})

	if sessionAgentConfigInfo(cfg, info) == nil {
		t.Fatal("control fixture no longer resolves its agent")
	}
	index := makeOpenSessionStoreRefIndex(cityPath, cfg, []session.Info{info}, true)
	if !openSessionOwnsWork(nil, index, assignee, "winnow", true) {
		t.Fatalf("control failed: session whose Dir resolves to the rig should own winnow-store work; "+
			"store-ref resolved to %q", openSessionReachableStoreRefInfo(cityPath, cfg, info))
	}
}

// The W3 split oracle's corpus contains no Dir-bearing agent, so the
// unresolvable-rig arm added for gascity-jrlv would otherwise be pinned by
// neither form. This equivalence check covers that arm directly, keeping the
// raw oracle honest about the branch it mirrors.
func TestOpenSessionReachableStoreRefMatchesRawForUnresolvableRig(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "polecat", Scope: "rig", Dir: "notarig"}},
		Rigs:   []config.Rig{{Name: "winnow", Path: filepath.Join(cityPath, "rigs", "winnow")}},
	}
	sb := beads.Bead{
		ID:       "ga-unresolvable-rig",
		Type:     session.BeadType,
		Status:   "open",
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"template": "notarig/polecat", "session_name": "gastown__polecat-gc-8a4d"},
	}
	info := sessiontest.SeedBead(t, sb)

	got := openSessionReachableStoreRefInfo(cityPath, cfg, info)
	want := rawOpenSessionReachableStoreRefRef(cityPath, cfg, sb)
	if got != want {
		t.Fatalf("info=%q raw=%q: the split forms disagree on the unresolvable-rig arm", got, want)
	}
	if got != unresolvedOpenSessionStoreRef {
		t.Fatalf("store-ref = %q, want the keep-on-match wildcard %q", got, unresolvedOpenSessionStoreRef)
	}
}
