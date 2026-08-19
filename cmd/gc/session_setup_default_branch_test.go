package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// The gastown pack's worktree-setup.sh creates each agent's persistent session
// branch and needs a start point. Until DefaultBranch was exposed here, the
// only "mainline" signal a pre_start script could reach was
// `git symbolic-ref refs/remotes/origin/HEAD`, which is a per-clone local
// pointer that drifts independently of the rig's configured default_branch.
// A rig whose default_branch is "edge-integration" but whose origin/HEAD still
// points at "main" therefore got session branches tracking refs/heads/main, and
// one `git pull --rebase` replayed the agent's work onto a stale base
// (gascity-rwu). These tests pin the rig-config value as the reachable source
// of truth.

func TestSessionSetupContextForAgent_DefaultBranchFromRig(t *testing.T) {
	rigs := []config.Rig{
		{Name: "gascity", Path: "/repos/gascity", DefaultBranch: "edge-integration"},
		{Name: "other", Path: "/repos/other", DefaultBranch: "trunk"},
	}
	agent := &config.Agent{Name: "polecat", Scope: "rig"}

	ctx := sessionSetupContextForAgent("/city", "bright-lights", "gascity/polecat", agent, rigs)

	if ctx.Rig != "gascity" {
		t.Fatalf("Rig = %q, want %q", ctx.Rig, "gascity")
	}
	if ctx.DefaultBranch != "edge-integration" {
		t.Errorf("DefaultBranch = %q, want %q", ctx.DefaultBranch, "edge-integration")
	}
}

func TestSessionSetupContextForAgent_DefaultBranchEmptyWhenRigUnset(t *testing.T) {
	// A rig with no recorded default_branch must yield an empty string rather
	// than a guessed "main": the pack script has to be able to tell "gc does
	// not know" from "gc says main", and only the former justifies its
	// origin/HEAD fallback.
	rigs := []config.Rig{{Name: "gascity", Path: "/repos/gascity"}}
	agent := &config.Agent{Name: "polecat", Scope: "rig"}

	ctx := sessionSetupContextForAgent("/city", "bright-lights", "gascity/polecat", agent, rigs)

	if ctx.DefaultBranch != "" {
		t.Errorf("DefaultBranch = %q, want empty", ctx.DefaultBranch)
	}
}

func TestSessionSetupContextForAgent_DefaultBranchEmptyForCityScopedAgent(t *testing.T) {
	// City-scoped agents belong to no rig, so there is no default branch to
	// report — and reporting another rig's would be worse than reporting none.
	rigs := []config.Rig{{Name: "gascity", Path: "/repos/gascity", DefaultBranch: "edge-integration"}}
	agent := &config.Agent{Name: "mayor"}

	ctx := sessionSetupContextForAgent("/city", "bright-lights", "mayor", agent, rigs)

	if ctx.Rig != "" {
		t.Fatalf("Rig = %q, want empty (city-scoped)", ctx.Rig)
	}
	if ctx.DefaultBranch != "" {
		t.Errorf("DefaultBranch = %q, want empty", ctx.DefaultBranch)
	}
}

func TestExpandSessionSetup_DefaultBranch(t *testing.T) {
	ctx := SessionSetupContext{
		Session:       "gascity--polecat",
		Agent:         "gascity/polecat",
		AgentBase:     "polecat",
		Rig:           "gascity",
		RigRoot:       "/repos/gascity",
		CityRoot:      "/city",
		CityName:      "bl",
		WorkDir:       "/city/.gc/worktrees/gascity/polecat",
		ConfigDir:     "/city/packs/gastown",
		DefaultBranch: "edge-integration",
	}
	cmds := []string{
		"{{.ConfigDir}}/assets/scripts/worktree-setup.sh {{.RigRoot}} {{.WorkDir}} {{.AgentBase}} --sync --default-branch {{.DefaultBranch}}",
	}

	got := expandSessionSetup(cmds, ctx)

	want := "/city/packs/gastown/assets/scripts/worktree-setup.sh /repos/gascity /city/.gc/worktrees/gascity/polecat polecat --sync --default-branch edge-integration"
	if got[0] != want {
		t.Errorf("got %q, want %q", got[0], want)
	}
}

// TestResolveTemplatePreStartExpandsDefaultBranch covers the path that actually
// runs at session start: resolveTemplate expands pre_start against its own
// SessionSetupContext literal, not the one sessionSetupContextForAgent builds.
// Wiring only the helper would leave {{.DefaultBranch}} rendering empty in the
// one place the worktree provisioner reads it.
func TestResolveTemplatePreStartExpandsDefaultBranch(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	rigRoot := filepath.Join(cityPath, "rigs", "gascity")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		rigs:       []config.Rig{{Name: "gascity", Path: rigRoot, DefaultBranch: "edge-integration"}},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{
		Name:     "polecat",
		Scope:    "rig",
		WorkDir:  ".gc/worktrees/polecat",
		PreStart: []string{"worktree-setup.sh {{.RigRoot}} --default-branch {{.DefaultBranch}}"},
	}
	tp, err := resolveTemplate(params, agent, "gascity/polecat", nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	if len(tp.Hints.PreStart) != 1 {
		t.Fatalf("PreStart = %v, want one expanded command", tp.Hints.PreStart)
	}
	want := "worktree-setup.sh " + rigRoot + " --default-branch edge-integration"
	if tp.Hints.PreStart[0] != want {
		t.Fatalf("PreStart[0] = %q, want %q", tp.Hints.PreStart[0], want)
	}
}

// TestResolveTemplatePreStartDefaultBranchEmptyWithoutRigConfig pins the
// config-only contract on the live path. cityPath is not a git repository and
// has no origin/HEAD, so an empty expansion here also proves the value is read
// from city.toml rather than probed — the probe is the source of truth this
// variable exists to replace.
func TestResolveTemplatePreStartDefaultBranchEmptyWithoutRigConfig(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	rigRoot := filepath.Join(cityPath, "rigs", "gascity")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		rigs:       []config.Rig{{Name: "gascity", Path: rigRoot}},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{
		Name:     "polecat",
		Scope:    "rig",
		WorkDir:  ".gc/worktrees/polecat",
		PreStart: []string{"worktree-setup.sh --default-branch [{{.DefaultBranch}}]"},
	}
	tp, err := resolveTemplate(params, agent, "gascity/polecat", nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	if len(tp.Hints.PreStart) != 1 {
		t.Fatalf("PreStart = %v, want one expanded command", tp.Hints.PreStart)
	}
	want := "worktree-setup.sh --default-branch []"
	if tp.Hints.PreStart[0] != want {
		t.Fatalf("PreStart[0] = %q, want %q", tp.Hints.PreStart[0], want)
	}
}
