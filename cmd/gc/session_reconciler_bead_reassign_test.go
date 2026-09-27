package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestReconcileSessionBeads_AliveFreshModeReassignCyclesConversation verifies
// the fix for gastownhall/gascity#1893: an alive on_demand named session
// running wake_mode=fresh must cycle its conversation when bd update points
// the assignee at a new bead. The session keeps the same bead identifier in
// the store (it's a named session) but its conversation lineage is reset so
// the next wake starts fresh on the newly assigned bead.
func TestReconcileSessionBeads_AliveFreshModeReassignCyclesConversation(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "witness", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "witness", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "witness")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "witness",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "witness",
		namedSessionModeMetadata:     "on_demand",
		"template":                   "witness",
		"state":                      "active",
		"wake_mode":                  "fresh",
		"session_key":                "conversation-A",
		sessionpkg.CurrentBeadIDKey:  "wb-A",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	// Patrol formula poured wb-B and pointed the witness's assignee at it;
	// wb-A is gone (closed/burned), so the reconciler only sees wb-B.
	workBead := beads.Bead{ID: "wb-B", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

	if env.sp.IsRunning(sessionName) {
		t.Fatal("session should have been killed by fresh-cycle")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata[sessionpkg.CurrentBeadIDKey] != "wb-B" {
		t.Fatalf("%s = %q, want wb-B", sessionpkg.CurrentBeadIDKey, got.Metadata[sessionpkg.CurrentBeadIDKey])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want empty so the next wake takes the first-start path", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "conversation-A" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
}

// TestReconcileSessionBeads_AliveResumeModeReassignKeepsConversation verifies
// that wake_mode=resume sessions DO NOT cycle on bead reassign — the
// existing conversation is preserved and the agent picks up the new bead
// from its work query at its next prompt boundary.
func TestReconcileSessionBeads_AliveResumeModeReassignKeepsConversation(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "witness", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "witness", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "witness")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "witness",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "witness",
		namedSessionModeMetadata:     "on_demand",
		"template":                   "witness",
		"state":                      "active",
		// wake_mode unset (default = resume)
		"session_key":               "conversation-A",
		sessionpkg.CurrentBeadIDKey: "wb-A",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	workBead := beads.Bead{ID: "wb-B", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

	if !env.sp.IsRunning(sessionName) {
		t.Fatal("resume-mode session should still be running — divergence must not cycle non-fresh sessions")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata["session_key"] != "conversation-A" {
		t.Fatalf("session_key = %q, want conversation-A preserved", got.Metadata["session_key"])
	}
	if got.Metadata["continuation_reset_pending"] == "true" {
		t.Fatalf("continuation_reset_pending = true, want unset for resume mode (no cycle should have run)")
	}
}

// TestReconcileSessionBeads_AliveFreshPoolSameRootAdvanceKeepsConversation
// pins gascity-gwl0: mol-polecat-work assigns every step of a workflow to the
// claiming pool session up front, so closing one step moves the anchor to the
// next step of the SAME workflow root. That is not new work, and a
// wake_mode=fresh pool session must keep its process and conversation across
// it while the recorded bead follows the anchor.
func TestReconcileSessionBeads_AliveFreshPoolSameRootAdvanceKeepsConversation(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "polecat", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(4)}},
	}
	sessionName := "polecat-gc-1"
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "polecat",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		poolManagedMetadataKey:      boolMetadata(true),
		"template":                  "polecat",
		"state":                     "active",
		"wake_mode":                 "fresh",
		"session_key":               "conversation-A",
		sessionpkg.CurrentBeadIDKey: "mol-1.2",
		sessionpkg.CurrentRootIDKey: "mol-1",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	// Step mol-1.2 closed; the next step of the same workflow is the only
	// work still assigned to the session.
	nextStep := beads.Bead{
		ID: "mol-1.3", Title: "preflight-tests", Type: "task", Status: "in_progress", Assignee: sessionName,
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "mol-1"},
	}

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{nextStep})

	if !env.sp.IsRunning(sessionName) {
		t.Fatal("pool session was killed advancing between steps of one workflow root — want it kept alive")
	}
	if out := env.stdout.String(); strings.Contains(out, "Cycled fresh-mode") {
		t.Fatalf("stdout reports a fresh-mode cycle for a same-root step advance:\n%s", out)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata[sessionpkg.CurrentBeadIDKey] != "mol-1.3" {
		t.Fatalf("%s = %q, want mol-1.3 re-stamped to the new anchor", sessionpkg.CurrentBeadIDKey, got.Metadata[sessionpkg.CurrentBeadIDKey])
	}
	if got.Metadata[sessionpkg.CurrentRootIDKey] != "mol-1" {
		t.Fatalf("%s = %q, want mol-1 kept", sessionpkg.CurrentRootIDKey, got.Metadata[sessionpkg.CurrentRootIDKey])
	}
	if got.Metadata["session_key"] != "conversation-A" {
		t.Fatalf("session_key = %q, want conversation-A preserved", got.Metadata["session_key"])
	}
}

// TestRecordCurrentBeadIDOnWake_StickyRoot pins the writer half of the sticky
// workflow root: a non-empty root is stamped beside the bead, and a rootless
// bead re-stamps the bead alone without clearing the recorded root.
func TestRecordCurrentBeadIDOnWake_StickyRoot(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		recordedBead, recorded string
		beadID, rootID         string
		wantFold               sessionpkg.MetadataPatch
		wantBead, wantRoot     string
	}{
		{
			name: "same-root step advance", recordedBead: "mol-1.2", recorded: "mol-1",
			beadID: "mol-1.3", rootID: "mol-1",
			wantFold: sessionpkg.MetadataPatch{sessionpkg.CurrentBeadIDKey: "mol-1.3"},
			wantBead: "mol-1.3", wantRoot: "mol-1",
		},
		{
			name: "rootless source bead keeps the root", recordedBead: "mol-1.2", recorded: "mol-1",
			beadID: "src-1", rootID: "",
			wantFold: sessionpkg.MetadataPatch{sessionpkg.CurrentBeadIDKey: "src-1"},
			wantBead: "src-1", wantRoot: "mol-1",
		},
		{
			name: "new root replaces the old one", recordedBead: "mol-1.9", recorded: "mol-1",
			beadID: "mol-2.1", rootID: "mol-2",
			wantFold: sessionpkg.MetadataPatch{sessionpkg.CurrentBeadIDKey: "mol-2.1", sessionpkg.CurrentRootIDKey: "mol-2"},
			wantBead: "mol-2.1", wantRoot: "mol-2",
		},
		{
			name: "root backfilled on an unchanged bead", recordedBead: "mol-1.2", recorded: "",
			beadID: "mol-1.2", rootID: "mol-1",
			wantFold: sessionpkg.MetadataPatch{sessionpkg.CurrentBeadIDKey: "mol-1.2", sessionpkg.CurrentRootIDKey: "mol-1"},
			wantBead: "mol-1.2", wantRoot: "mol-1",
		},
		{
			name: "nothing changed", recordedBead: "mol-1.2", recorded: "mol-1",
			beadID: "mol-1.2", rootID: "mol-1",
			wantFold: nil,
			wantBead: "mol-1.2", wantRoot: "mol-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			meta := map[string]string{sessionpkg.CurrentBeadIDKey: tc.recordedBead}
			if tc.recorded != "" {
				meta[sessionpkg.CurrentRootIDKey] = tc.recorded
			}
			b, err := store.Create(beads.Bead{Title: "polecat-gc-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			info := sessionpkg.Info{ID: b.ID, CurrentlyProcessingBeadID: tc.recordedBead, CurrentlyProcessingRootID: tc.recorded}

			fold := recordCurrentBeadIDOnWake(info, sessionFrontDoor(store), tc.beadID, tc.rootID, nil)

			if !reflect.DeepEqual(fold, tc.wantFold) {
				t.Fatalf("fold = %#v, want %#v", fold, tc.wantFold)
			}
			got, err := store.Get(b.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Metadata[sessionpkg.CurrentBeadIDKey] != tc.wantBead || got.Metadata[sessionpkg.CurrentRootIDKey] != tc.wantRoot {
				t.Fatalf("stored (bead, root) = (%q, %q), want (%q, %q)",
					got.Metadata[sessionpkg.CurrentBeadIDKey], got.Metadata[sessionpkg.CurrentRootIDKey], tc.wantBead, tc.wantRoot)
			}
		})
	}
}

// TestReconcileSessionBeads_AsleepWakeRecordsCurrentBead pins the recording
// half of the contract: when an asleep session is woken because of an
// assigned bead, the reconciler must stamp currently_processing_bead_id
// onto the session bead. Without this, the next reassign cycle would have
// no recorded current bead to compare against.
func TestReconcileSessionBeads_AsleepWakeRecordsCurrentBead(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "witness", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "witness", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "witness")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "witness",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "witness",
		namedSessionModeMetadata:     "on_demand",
		"template":                   "witness",
		"state":                      "asleep",
		"wake_mode":                  "fresh",
	})

	workBead := beads.Bead{ID: "wb-77", Title: "witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

	got, _ := env.store.Get(session.ID)
	if got.Metadata[sessionpkg.CurrentBeadIDKey] != "wb-77" {
		t.Fatalf("%s = %q, want wb-77 recorded at wake", sessionpkg.CurrentBeadIDKey, got.Metadata[sessionpkg.CurrentBeadIDKey])
	}
}

// TestReconcileSessionBeads_RecoveryPrefersRecordedBead pins crash-recovery
// behavior: when a session is asleep with a recorded current bead AND
// multiple beads are assigned, the reconciler must anchor on the recorded
// bead so the agent resumes the work it was last actively processing.
func TestReconcileSessionBeads_RecoveryPrefersRecordedBead(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "witness", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "witness", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "witness")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "witness",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "witness",
		namedSessionModeMetadata:     "on_demand",
		"template":                   "witness",
		"state":                      "asleep",
		"wake_mode":                  "fresh",
		sessionpkg.CurrentBeadIDKey:  "wb-current",
	})

	other := beads.Bead{ID: "wb-other", Title: "other wisp", Type: "task", Status: "open", Assignee: "witness"}
	current := beads.Bead{ID: "wb-current", Title: "current wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{other, current})

	got, _ := env.store.Get(session.ID)
	if got.Metadata[sessionpkg.CurrentBeadIDKey] != "wb-current" {
		t.Fatalf("%s = %q, want wb-current preserved across restart", sessionpkg.CurrentBeadIDKey, got.Metadata[sessionpkg.CurrentBeadIDKey])
	}
}

// reconcileSessionBeadsWithAssignedWork is a test-only wrapper that mirrors
// restartRequestTestEnv.reconcile but threads assignedWorkBeads through so
// ComputeAwakeSet sees the work demand. Tests for assigned-work-driven
// behavior need this hook; the existing helper in
// session_reconciler_restart_request_test.go intentionally passes nil.
func reconcileSessionBeadsWithAssignedWork(env *restartRequestTestEnv, sessions []beads.Bead, assignedWork []beads.Bead) {
	poolDesired := make(map[string]int)
	for _, tp := range env.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	_ = reconcileSessionBeads(
		context.Background(),
		sessions,
		env.desiredState,
		cfgNames,
		env.cfg,
		env.sp,
		env.store,
		nil,
		assignedWork,
		nil,
		env.dt,
		poolDesired,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
}
