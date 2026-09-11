package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

// sessionResetOptions carries the tunable parts of a reset request.
type sessionResetOptions struct {
	json bool
	// wait bounds how long to watch for the controller to commit the restart.
	// Zero restores the historical fire-and-forget behavior: request, then
	// report the request without confirming it.
	wait time.Duration
}

// newSessionResetCmd creates the "gc session reset <id-or-alias>" command.
func newSessionResetCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "reset <session-id-or-alias>",
		Short: "Restart a session fresh while preserving the bead",
		Long: `Request a fresh restart for an existing session without closing its bead.

The controller stops the current runtime and starts the same session again with
fresh provider conversation state. Session identity, alias, mail, and queued
work remain attached to the existing session bead. For named sessions, reset
also clears any tripped named-session respawn circuit breaker before requesting
the fresh restart.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := sessionResetOptions{json: jsonOutput, wait: wait}
			if !cmd.Flags().Changed("wait") {
				opts.wait = -1 // resolve from the city's startup timeout
			}
			if cmdSessionResetWithOptions(args, stdout, stderr, opts) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSONL")
	cmd.Flags().DurationVar(&wait, "wait", 0,
		"how long to wait for the controller to commit the restart before reporting it unconfirmed (0 disables the check; default: the city's session startup timeout)")
	return cmd
}

// cmdSessionReset is the CLI entry point for "gc session reset".
//
// This command intentionally requires a managed controller. The controller owns
// the fresh restart lifecycle, including key rotation and immediate restart of
// already-desired sessions.
func cmdSessionReset(args []string, stdout, stderr io.Writer, jsonOutput ...bool) int {
	return cmdSessionResetWithOptions(args, stdout, stderr, sessionResetOptions{
		json: sessionJSONRequested(jsonOutput),
		wait: -1,
	})
}

// cmdSessionResetWithOptions implements gc session reset.
//
// Requesting the restart is only half the job. The request itself is a metadata
// write the controller picks up asynchronously, so a command that returned as
// soon as the write landed reported success for a restart it had never
// observed — and a reset that stalled was indistinguishable, to the operator or
// to an agent resetting itself, from one that worked. This waits for the
// controller to commit the restart and reports honestly when it does not.
func cmdSessionResetWithOptions(args []string, stdout, stderr io.Writer, opts sessionResetOptions) int {
	asJSON := opts.json
	store, code := openCityStore(stderr, "gc session reset")
	if store == nil {
		return code
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session reset: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !cityUsesManagedReconciler(cityPath) {
		fmt.Fprintln(stderr, "gc session reset: a managed controller must be running") //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := pokeController(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc session reset: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cfg, _ := loadCityConfig(cityPath, stderr)

	// Every store consumer here is session-class (ID resolution, worker handle,
	// session-bead load), so route the whole flow through the session
	// coordination-class store for relocation-safety.
	sessStore := cliSessionStore(store, cfg, cityPath)
	sessionID, err := resolveSessionIDWithConfig(cityPath, cfg, sessStore, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc session reset: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp, err := newSessionProvider()
	if err != nil {
		fmt.Fprintf(stderr, "gc session reset: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	handle, err := workerHandleForSessionWithConfig(cityPath, sessStore, sp, cfg, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session reset: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	bead, err := sessStore.Get(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session reset: loading session %s: %v\n", sessionID, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	identity := namedSessionIdentity(bead)
	if identity != "" {
		if err := resetSessionCircuitBreakerOnController(cityPath, sessionID, identity); err != nil {
			fmt.Fprintf(stderr, "gc session reset: clearing session circuit breaker for %q: %v\n", identity, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	// Capture the durable restart marker before requesting, so the wait below
	// keys on THIS reset committing rather than on a value an earlier one left
	// behind.
	committedBefore := strings.TrimSpace(bead.Metadata[sessionpkg.ResetCommittedAtKey])

	if err := handle.Reset(context.Background()); err != nil {
		fmt.Fprintf(stderr, "gc session reset: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	_ = pokeController(cityPath)

	wait := opts.wait
	if wait < 0 {
		wait = cfg.Session.StartupTimeoutDuration()
	}
	confirmed := waitForResetCommitted(sessStore, sessionID, committedBefore, wait)

	if asJSON {
		if err := writeSessionActionJSONWithOK(stdout, sessionActionResult{
			Action:    "reset",
			SessionID: sessionID,
			Identity:  identity,
			Confirmed: &confirmed,
		}, confirmed); err != nil {
			fmt.Fprintf(stderr, "gc session reset: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if !confirmed {
			return 1
		}
		return 0
	}
	if !confirmed {
		msg := fmt.Sprintf(
			"gc session reset: restart requested for %s but the controller did not commit it within %s.\n"+
				"The runtime may already be stopped. Check `gc session status %s`; `gc session wake %s` forces it awake.\n",
			sessionID, wait, sessionID, sessionID)
		fmt.Fprint(stderr, msg) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "Session %s reset. Controller restarted it fresh.\n", sessionID) //nolint:errcheck // best-effort stdout
	return 0
}

// waitForResetCommitted reports whether the controller committed the requested
// restart within the budget.
//
// "Committed" is the controller consuming the request marker and stamping a new
// reset_committed_at — the same handoff that clears this session's wake
// blockers, so a commit now genuinely leads to a start rather than to a seat
// parked behind a stale quarantine timer. A zero or negative budget skips the
// check and reports the request as confirmed, preserving the old
// fire-and-forget behavior for callers that ask for it.
func waitForResetCommitted(store beads.Store, sessionID, committedBefore string, budget time.Duration) bool {
	if budget <= 0 {
		return true
	}
	deadline := time.Now().Add(budget)
	for {
		bead, err := store.Get(sessionID)
		if err == nil {
			restartPending := strings.TrimSpace(bead.Metadata["restart_requested"]) == "true"
			committedNow := strings.TrimSpace(bead.Metadata[sessionpkg.ResetCommittedAtKey])
			if !restartPending && committedNow != "" && committedNow != committedBefore {
				return true
			}
		}
		if !time.Now().Before(deadline) {
			return false
		}
		remaining := time.Until(deadline)
		if remaining > resetCommitPollInterval {
			remaining = resetCommitPollInterval
		}
		time.Sleep(remaining)
	}
}

// resetCommitPollInterval is how often the reset confirmation re-reads the
// session bead while waiting for the controller to commit.
const resetCommitPollInterval = 50 * time.Millisecond

func resetSessionCircuitBreakerAfterExplicitKill(cityPath string, store beads.Store, sessionID, identity string) error {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return nil
	}
	if strings.TrimSpace(cityPath) != "" && cityUsesManagedReconciler(cityPath) {
		if err := resetSessionCircuitBreakerOnController(cityPath, sessionID, identity); err != nil {
			return err
		}
		_ = pokeController(cityPath)
		return nil
	}
	return resetSessionCircuitBreakerState(store, sessionID, identity, defaultSessionCircuitBreaker())
}
