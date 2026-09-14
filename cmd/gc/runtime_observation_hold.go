// runtime_observation_hold.go — the CONSTRUCTIVE half of the reconciler's
// partial-observation defense.
//
// Every DESTRUCTIVE reconciler path already refuses to act on a runtime listing
// it could not make: pool on_death (reconcilePoolDeaths), provider swap,
// shutdown listing (cmd_stop.go), orphan cleanup (session_beads.go) and
// controller stop targets (controller.go) all consult
// runtime.IsPartialListError and defer. The CONSTRUCTIVE path had no
// equivalent, and that asymmetry is what rebuilds a town. Demand is computed
// from the bead store, which stays readable; "running" is observed from the
// runtime, which does not. A totally unreachable tmux server surfaces as a
// runtime.PartialListError carrying a nil names slice, every session then
// probes as not-alive, and the reconciler starts one replacement per desired
// session — the fleet is rebuilt precisely BECAUSE it could not be seen.
// Measured 2026-09-14: 13 sessions destroyed and respawned inside 72 seconds,
// with the pool-death hold ("pool death check skipped due to partial session
// listing") and the cold-start rebuild seconds apart in the same supervisor
// log (gascity-bjg2).
//
// This gate is the squatter guard's reasoning with the sign flipped: an
// unobservable fleet is not an absent fleet.
//
// Two facts keep the hold from becoming an outage of its own:
//
//   - It opens only when the controller already believes sessions are running.
//     A failed observation can hide only a fleet that is there. On a first boot
//     tmux has no server yet, so ErrNoServer is the NORMAL reading, and holding
//     on it would stop the city from ever starting.
//   - It expires. A runtime that is genuinely gone for good must be rebuilt, so
//     one outage episode holds for a bounded window and then escalates loudly
//     and releases rather than deadlocking the city forever.
//
// Both of those rest on a third: the hold protects what THIS controller saw
// running, never what a previous one left behind. You cannot lose sight of
// something you never saw.
package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// partialRuntimeObservationHoldWindow bounds how long one failed-observation
// episode may suppress the reconciler's start decisions. It spans several
// patrol ticks at the 30s default (DaemonConfig.PatrolIntervalDuration), so an
// ordinary tmux blip is absorbed whole, while a runtime that is genuinely dead
// is rebuilt a couple of minutes late instead of never. It is a duration rather
// than a tick count because ticks are debounced and poke-driven: a burst of
// pokes would burn a tick budget in seconds and reopen the very window this
// closes.
const partialRuntimeObservationHoldWindow = 2 * time.Minute

// runtimeObservationHold tracks one partial-runtime-observation episode across
// reconciler ticks. It lives on CityRuntime because the escalation ladder is
// inherently cross-tick: a single tick cannot tell a blip from a death. The
// zero value is a closed episode and is ready to use; observe, its only entry
// point, is nil-safe and safe for concurrent use.
type runtimeObservationHold struct {
	mu sync.Mutex
	// open reports whether an outage episode is in progress. An episode opens
	// on the first failed observation that could be hiding a running fleet and
	// closes on the first successful listing.
	open bool
	// startedAt is when the current episode's first failed observation landed.
	startedAt time.Time
	// believedRunning is captured once, when the episode opens, and is NOT
	// re-read per tick: the reconciler's own state heal walks live sessions to
	// asleep while the runtime is unreadable, so a per-tick re-read would
	// erase the episode's justification one tick after it opened.
	believedRunning int
	// heldTicks counts the ticks this episode has actually held.
	heldTicks int
	// escalated records that this episode has already given up and released,
	// so the loud line fires exactly once per outage.
	escalated bool
	// sawCleanListing records that this controller has read the runtime
	// successfully at least once. It is a LIFETIME fact, not an episode one,
	// and closeEpisode deliberately leaves it set.
	//
	// It is what keeps the hold off a city that is merely starting. A boot
	// after a machine reboot finds session beads still marked "awake" from the
	// previous boot while the tmux server is gone, so believedRunning alone
	// would open an episode and stall the whole fleet for the hold window on
	// the strength of a belief this process never verified. A controller that
	// has never successfully read the runtime has no observation of its own to
	// be contradicted, so it starts — and the start itself cold-starts the
	// server, after which the next listing succeeds and the fleet is protected
	// from then on.
	sawCleanListing bool
}

// runtimeObservationDecision is one tick's verdict from a runtimeObservationHold.
type runtimeObservationDecision struct {
	// Hold reports that the reconciler must not turn this tick's liveness
	// readings into start decisions.
	Hold bool
	// BelievedRunning is how many sessions the controller believed were running
	// when the episode opened.
	BelievedRunning int
	// Elapsed is how long the current episode has run.
	Elapsed time.Duration
	// HeldTicks counts the ticks this episode has held, including this one.
	HeldTicks int
	// Escalated is true on the single tick an episode outlives its window and
	// releases the hold, so the caller logs the escalation exactly once.
	Escalated bool
}

// observe records this tick's runtime-listing outcome and reports whether the
// constructive path must hold. observationFailed is the ListRunning verdict
// from runtimeListingObservationFailed; believedRunning is the count from
// sessionsBelievedRunning, consulted only when an episode opens. An episode
// that outlives partialRuntimeObservationHoldWindow releases rather than
// holding forever.
func (h *runtimeObservationHold) observe(observationFailed bool, believedRunning int, now time.Time) runtimeObservationDecision {
	if h == nil {
		return runtimeObservationDecision{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if !observationFailed {
		h.sawCleanListing = true
		h.closeEpisode()
		return runtimeObservationDecision{}
	}
	if !h.open {
		// Nothing this failed observation could be hiding. Either this
		// controller has never successfully read the runtime — so the beads'
		// belief is inherited from a previous process, not an observation of
		// its own, and holding would stall a boot — or it does not believe any
		// session is running, and a fleet nobody believes in cannot be torn
		// down by rebuilding it. Either way, leave the episode closed and let
		// the city start.
		if !h.sawCleanListing || believedRunning <= 0 {
			return runtimeObservationDecision{}
		}
		h.open = true
		h.startedAt = now
		h.believedRunning = believedRunning
		h.heldTicks = 0
		h.escalated = false
	}
	elapsed := now.Sub(h.startedAt)
	if elapsed > partialRuntimeObservationHoldWindow {
		// The outage outlived the window. A runtime that never comes back must
		// not keep the city from rebuilding, so release — loudly, once.
		decision := runtimeObservationDecision{
			BelievedRunning: h.believedRunning,
			Elapsed:         elapsed,
			HeldTicks:       h.heldTicks,
		}
		if !h.escalated {
			h.escalated = true
			decision.Escalated = true
		}
		return decision
	}
	h.heldTicks++
	return runtimeObservationDecision{
		Hold:            true,
		BelievedRunning: h.believedRunning,
		Elapsed:         elapsed,
		HeldTicks:       h.heldTicks,
	}
}

// closeEpisode ends any episode in progress so the next outage gets a fresh
// window. It deliberately leaves sawCleanListing alone: that this controller
// has seen the runtime is a fact about the controller, not about the episode.
// Callers hold h.mu.
func (h *runtimeObservationHold) closeEpisode() {
	h.open = false
	h.startedAt = time.Time{}
	h.believedRunning = 0
	h.heldTicks = 0
	h.escalated = false
}

// runtimeListingObservationFailed reports whether sp's whole-fleet listing was
// a failed OBSERVATION rather than a fleet observed to be absent, and returns
// the listing error so the caller can name it in the operator log.
//
// Any listing error counts. runtime.PartialListError is the signal a degraded
// or unreachable backend raises — the tmux adapter maps ErrNoServer onto it
// precisely so the reconciler-facing guards fire — but a provider that fails
// outright has observed no more than one that fails partially, and
// construction fails CLOSED here. Only (names, nil), an empty names slice
// included, is a real observation.
func runtimeListingObservationFailed(sp runtime.Provider) (bool, error) {
	if sp == nil {
		return false, nil
	}
	if _, err := sp.ListRunning(""); err != nil {
		return true, err
	}
	return false, nil
}

// sessionsBelievedRunning counts the open session beads whose last persisted
// advisory state claims a live runtime.
//
// It reads Info.MetadataState — the RAW persisted state — rather than the
// liveness-shaped Info.State, because the question is what the controller
// believed BEFORE this tick's runtime probe, which is exactly what the previous
// tick persisted. "awake" is the reconciler's own alias for "active" (see
// session.normalizeInfoState), so both count.
func sessionsBelievedRunning(infos []sessionpkg.Info) int {
	believed := 0
	for i := range infos {
		if infos[i].Closed {
			continue
		}
		switch sessionpkg.State(strings.TrimSpace(infos[i].MetadataState)) {
		case sessionpkg.StateActive, sessionpkg.StateAwake:
			believed++
		}
	}
	return believed
}

// installRuntimeObservationHold consults this tick's whole-fleet runtime
// listing and, when it was an observation that FAILED rather than a fleet
// observed to be absent, installs the constructive start hold on startOptions.
// It returns the possibly-extended options plus the trace fields describing the
// decision, and logs the hold — and, once per episode, its escalation — on the
// controller's stderr, which is the supervisor log the incident was diagnosed
// from.
//
// The probe takes its OWN listing rather than reusing the one reconcilePoolDeaths
// makes at the top of the tick: the hold needs the runtime's reachability as of
// the reconcile, and that earlier call returns before listing at all when no
// pool declares an on_death hook.
func (cr *CityRuntime) installRuntimeObservationHold(startOptions []startExecutionOption, openInfos []sessionpkg.Info, now time.Time) ([]startExecutionOption, map[string]any) {
	listingFailed, listErr := runtimeListingObservationFailed(cr.sp)
	decision := cr.runtimeObsHold.observe(listingFailed, sessionsBelievedRunning(openInfos), now)
	switch {
	case decision.Hold:
		fmt.Fprintf(cr.stderr, "%s: runtime session listing failed (%v); holding start decisions this tick — %d session(s) still believed running, outage %s of %s, held tick %d. An unobservable fleet is not an absent fleet (gascity-bjg2).\n", //nolint:errcheck // best-effort stderr
			cr.logPrefix, listErr, decision.BelievedRunning, decision.Elapsed.Round(time.Second), partialRuntimeObservationHoldWindow, decision.HeldTicks)
		startOptions = append(startOptions, withPartialRuntimeObservationStartHold())
	case decision.Escalated:
		fmt.Fprintf(cr.stderr, "%s: ESCALATION: the runtime session listing has failed continuously for %s (%v), past the %s hold window; releasing the start hold, so the %d session(s) the controller believed running are about to be rebuilt. The runtime is not recovering on its own — check it (gascity-bjg2).\n", //nolint:errcheck // best-effort stderr
			cr.logPrefix, decision.Elapsed.Round(time.Second), listErr, partialRuntimeObservationHoldWindow, decision.BelievedRunning)
	}
	return startOptions, map[string]any{
		"listing_failed":   listingFailed,
		"hold":             decision.Hold,
		"believed_running": decision.BelievedRunning,
		"held_ticks":       decision.HeldTicks,
		"escalated":        decision.Escalated,
	}
}
