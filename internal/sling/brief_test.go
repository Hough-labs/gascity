package sling

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/runtime"
)

// briefFormulaFixtures are the four var shapes the carry has to tell apart. The
// names mirror the real formulas measured on the bead (gascity-zmli): the
// planning family declares context_path, the polecat family declares neither.
const (
	briefFixtureBothVars = `formula = "plan-both"
version = 1

[vars.context_path]
description = "Optional source context bundle path."
default = ""

[vars.requirements_path]
description = "Requirements artifact path to create or reuse."
default = ""

[[steps]]
id = "plan"
title = "Plan"
description = "Read {{context_path}} then write {{requirements_path}}."
`

	briefFixtureContextOnly = `formula = "plan-context"
version = 1

[vars.context_path]
description = "Optional source context bundle path."
default = ""

[[steps]]
id = "plan"
title = "Plan"
description = "Read {{context_path}}."
`

	briefFixtureRequirementsOnly = `formula = "plan-requirements"
version = 1

[vars.requirements_path]
description = "Requirements artifact path to create or reuse."
default = ""

[[steps]]
id = "plan"
title = "Plan"
description = "Write {{requirements_path}}."
`

	briefFixtureNeither = `formula = "polecat-like"
version = 1

[[steps]]
id = "work"
title = "Work"
description = "Run gc bd show on the work bead."
`
)

// briefTestEnv wires a city whose formula layer holds the named fixtures and
// whose store holds one bead, so a carry decision can be resolved end to end.
type briefTestEnv struct {
	deps  SlingDeps
	agent config.Agent
	store beads.Store
}

func newBriefTestEnv(t *testing.T, fixtures map[string]string) briefTestEnv {
	t.Helper()
	formulaDir := t.TempDir()
	for name, content := range fixtures {
		if err := os.WriteFile(filepath.Join(formulaDir, name+".toml"), []byte(content), 0o644); err != nil {
			t.Fatalf("writing fixture %s: %v", name, err)
		}
	}
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		FormulaLayers: config.FormulaLayers{City: []string{formulaDir}},
	}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.CityPath = t.TempDir()
	return briefTestEnv{deps: deps, agent: config.Agent{Name: "agent-a", MaxActiveSessions: intPtr(3)}, store: deps.Store}
}

// briefTestBeadTitle labels every bead these tests create. The title is
// incidental to the carry — only the description decides it — so it is fixed
// here rather than threaded through each call.
const briefTestBeadTitle = "Widget"

// createBead stores the bead and returns the ID the store assigned. Stores
// mint their own IDs, so the caller must route every later lookup through the
// returned value rather than the requested one.
func (e briefTestEnv) createBead(t *testing.T, description string) string {
	t.Helper()
	created, err := e.store.Create(beads.Bead{Title: briefTestBeadTitle, Description: description, Type: "task"})
	if err != nil {
		t.Fatalf("creating bead: %v", err)
	}
	return created.ID
}

func (e briefTestEnv) resolve(t *testing.T, beadID, formulaName string, userVars []string) beadBriefCarry {
	t.Helper()
	opts := SlingOpts{Target: e.agent, BeadOrFormula: beadID, Vars: userVars}
	return resolveBeadBriefCarry(opts, e.deps, e.deps.Store, beadID, formulaName)
}

func carriedContextPath(t *testing.T, carry beadBriefCarry) string {
	t.Helper()
	for _, v := range carry.Vars {
		if key, value, ok := strings.Cut(v, "="); ok && key == briefContextPathVar {
			return value
		}
	}
	return ""
}

// TestResolveBeadBriefCarry is the acceptance matrix from gascity-zmli: a
// recipe that declares context_path gets the brief carried and no note; a
// recipe that declares neither var gets neither (the old note's advice was
// inert there); a caller-supplied var always wins; an empty description
// produces nothing at all.
func TestResolveBeadBriefCarry(t *testing.T) {
	tests := []struct {
		name        string
		formula     string
		description string
		userVars    []string
		wantCarry   bool
		wantHint    bool
	}{
		{
			name:        "declares context_path and no caller var carries the brief",
			formula:     "plan-context",
			description: "Implement the widget.",
			wantCarry:   true,
		},
		{
			name:        "declares both vars carries into context_path only",
			formula:     "plan-both",
			description: "Implement the widget.",
			wantCarry:   true,
		},
		{
			name:        "declares neither var is silent",
			formula:     "polecat-like",
			description: "Implement the widget.",
		},
		{
			name:        "declares requirements_path only hints instead of binding",
			formula:     "plan-requirements",
			description: "Implement the widget.",
			wantHint:    true,
		},
		{
			name:        "caller context_path wins over the carry",
			formula:     "plan-context",
			description: "Implement the widget.",
			userVars:    []string{"context_path=/caller/bundle"},
		},
		{
			name:        "caller requirements_path suppresses the carry",
			formula:     "plan-context",
			description: "Implement the widget.",
			userVars:    []string{"requirements_path=/caller/reqs.md"},
		},
		{
			name:        "empty description carries nothing and says nothing",
			formula:     "plan-context",
			description: "",
		},
		{
			name:        "whitespace-only description carries nothing and says nothing",
			formula:     "plan-context",
			description: "   \n\t  ",
		},
		{
			name:        "empty description on a declares-neither formula is silent",
			formula:     "polecat-like",
			description: "",
		},
	}
	fixtures := map[string]string{
		"plan-both":         briefFixtureBothVars,
		"plan-context":      briefFixtureContextOnly,
		"plan-requirements": briefFixtureRequirementsOnly,
		"polecat-like":      briefFixtureNeither,
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newBriefTestEnv(t, fixtures)
			beadID := env.createBead(t, tc.description)

			carry := env.resolve(t, beadID, tc.formula, tc.userVars)

			got := carriedContextPath(t, carry)
			if tc.wantCarry {
				if got == "" {
					t.Fatalf("want a carried context_path, got vars %v", carry.Vars)
				}
				brief, err := os.ReadFile(filepath.Join(got, briefFileName))
				if err != nil {
					t.Fatalf("reading materialized brief: %v", err)
				}
				if !strings.Contains(string(brief), tc.description) {
					t.Errorf("brief missing the bead description: %q", brief)
				}
			} else if got != "" {
				t.Errorf("want no carry, got context_path=%q", got)
			}

			if tc.wantHint && carry.Hint == "" {
				t.Errorf("want a hint, got none")
			}
			if !tc.wantHint && carry.Hint != "" {
				t.Errorf("want no hint, got %q", carry.Hint)
			}
		})
	}
}

// TestResolveBeadBriefCarryNeverBindsRequirementsPath pins the reason the carry
// is context_path-only: requirements_path names an artifact the planning family
// WRITES, so binding the brief there would have the formula overwrite it.
func TestResolveBeadBriefCarryNeverBindsRequirementsPath(t *testing.T) {
	env := newBriefTestEnv(t, map[string]string{
		"plan-both":         briefFixtureBothVars,
		"plan-requirements": briefFixtureRequirementsOnly,
	})
	beadID := env.createBead(t, "Implement the widget.")

	for _, name := range []string{"plan-both", "plan-requirements"} {
		carry := env.resolve(t, beadID, name, nil)
		for _, v := range carry.Vars {
			if key, _, _ := strings.Cut(v, "="); key == briefRequirementsPathVar {
				t.Errorf("%s: carry bound requirements_path (%q); it is a write target", name, v)
			}
		}
	}
}

// TestResolveBeadBriefCarryRigFormulaVarsWin locks that a rig-configured
// context_path is treated as caller-supplied — it reaches the formula through
// the same precedence chain a --var does, so auto-carry must not override it.
func TestResolveBeadBriefCarryRigFormulaVarsWin(t *testing.T) {
	env := newBriefTestEnv(t, map[string]string{"plan-context": briefFixtureContextOnly})
	beadID := env.createBead(t, "Implement the widget.")
	env.deps.Cfg.Rigs = []config.Rig{{Name: "rig-a", FormulaVars: map[string]string{"context_path": "/rig/bundle"}}}
	env.agent.Dir = "rig-a"

	carry := env.resolve(t, beadID, "plan-context", nil)
	if got := carriedContextPath(t, carry); got != "" {
		t.Errorf("rig-configured context_path was overridden by the carry: %q", got)
	}
	if carry.Hint != "" {
		t.Errorf("rig-configured context_path should also silence the note, got %q", carry.Hint)
	}
}

// TestResolveBeadBriefCarryRewritesStaleBrief locks that a re-sling after the
// bead's description changed carries the CURRENT text, not the first pour's.
func TestResolveBeadBriefCarryRewritesStaleBrief(t *testing.T) {
	env := newBriefTestEnv(t, map[string]string{"plan-context": briefFixtureContextOnly})
	beadID := env.createBead(t, "First draft.")

	first := carriedContextPath(t, env.resolve(t, beadID, "plan-context", nil))
	if first == "" {
		t.Fatal("first sling did not carry a brief")
	}
	if err := env.store.Update(beadID, beads.UpdateOpts{Description: stringPtr("Second draft.")}); err != nil {
		t.Fatalf("updating bead: %v", err)
	}

	second := carriedContextPath(t, env.resolve(t, beadID, "plan-context", nil))
	if second != first {
		t.Errorf("brief path is not stable across slings: %q then %q", first, second)
	}
	brief, err := os.ReadFile(filepath.Join(second, briefFileName))
	if err != nil {
		t.Fatalf("reading re-materialized brief: %v", err)
	}
	if strings.Contains(string(brief), "First draft.") || !strings.Contains(string(brief), "Second draft.") {
		t.Errorf("re-sling carried the stale brief: %q", brief)
	}
}

// TestFormatBeadBrief pins the rendered document: it names the bead so an agent
// reading the bundle knows what it is looking at, and carries the description
// verbatim.
func TestFormatBeadBrief(t *testing.T) {
	got := formatBeadBrief(beads.Bead{ID: "tb-5", Title: "Widget", Description: "  Implement the widget.  "})
	if !strings.Contains(got, "Widget") || !strings.Contains(got, "tb-5") {
		t.Errorf("brief does not identify the bead: %q", got)
	}
	if !strings.Contains(got, "Implement the widget.") {
		t.Errorf("brief lost the description: %q", got)
	}
	if got := formatBeadBrief(beads.Bead{ID: "tb-5", Title: "Widget"}); got != "" {
		t.Errorf("empty description: want empty brief, got %q", got)
	}
	// A bead with no title still has to render — the ID is the fallback label.
	if got := formatBeadBrief(beads.Bead{ID: "tb-6", Description: "Body."}); !strings.Contains(got, "tb-6") {
		t.Errorf("untitled bead: want the ID as the label, got %q", got)
	}
}

// TestDoSlingCarriesBriefIntoRootVarMetadata is the wiring + root-only-pour
// test. The carry is recorded as gc.var.context_path on the workflow root,
// which both the legacy molecule path and the graph.v2 path stamp before any
// step is materialized — so the brief reaches a root-only pour, where step
// variable substitution never runs.
func TestDoSlingCarriesBriefIntoRootVarMetadata(t *testing.T) {
	tests := []struct {
		name         string
		formula      string
		content      string
		wantRootOnly bool
	}{
		{
			name:    "poured formula",
			formula: "plan-context",
			content: briefFixtureContextOnly,
		},
		{
			name:         "root-only vapor pour",
			formula:      "plan-vapor",
			content:      strings.Replace(briefFixtureContextOnly, "version = 1", "version = 1\nphase = \"vapor\"", 1),
			wantRootOnly: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newBriefTestEnv(t, map[string]string{tc.formula: strings.Replace(tc.content, "plan-context", tc.formula, 1)})
			beadID := env.createBead(t, "Implement the widget.")

			// Pin that the vapor fixture really compiles root-only. Without
			// this the case silently degrades into a second poured run and
			// stops covering the trap it exists for.
			recipe, err := formula.Compile(context.Background(), tc.formula, SlingFormulaSearchPaths(env.deps, env.agent), nil)
			if err != nil {
				t.Fatalf("Compile(%s): %v", tc.formula, err)
			}
			if recipe.RootOnly != tc.wantRootOnly {
				t.Fatalf("fixture %s: RootOnly=%v, want %v", tc.formula, recipe.RootOnly, tc.wantRootOnly)
			}

			result, err := DoSling(SlingOpts{
				Target:        env.agent,
				BeadOrFormula: beadID,
				OnFormula:     tc.formula,
			}, env.deps, env.deps.Store)
			if err != nil {
				t.Fatalf("DoSling: %v", err)
			}
			if result.WispRootID == "" {
				t.Fatalf("no wisp root created: %+v", result)
			}
			root, err := env.store.Get(result.WispRootID)
			if err != nil {
				t.Fatalf("reading wisp root: %v", err)
			}
			carried := root.Metadata[beadmeta.FormulaVarPrefix+briefContextPathVar]
			if carried == "" {
				t.Fatalf("root bead has no %s%s metadata: %v", beadmeta.FormulaVarPrefix, briefContextPathVar, root.Metadata)
			}
			brief, err := os.ReadFile(filepath.Join(carried, briefFileName))
			if err != nil {
				t.Fatalf("reading brief at the carried path: %v", err)
			}
			if !strings.Contains(string(brief), "Implement the widget.") {
				t.Errorf("carried brief lost the description: %q", brief)
			}
			for _, w := range result.BeadWarnings {
				if strings.Contains(w, briefContextPathVar) {
					t.Errorf("carried formula should not also warn: %q", w)
				}
			}

			// Read the RENDERED step text, not just the bound var. On a poured
			// formula {{context_path}} must substitute to the carried path, so
			// the agent reading its step finds the brief. A root-only pour
			// materializes no step at all — there the root's gc.var metadata
			// asserted above is the whole delivery mechanism, which is exactly
			// why the carry does not depend on substitution.
			steps := poredStepDescriptions(t, env.store, result.WispRootID)
			if tc.wantRootOnly {
				if len(steps) != 0 {
					t.Errorf("root-only pour materialized %d step beads: %v", len(steps), steps)
				}
				return
			}
			if len(steps) == 0 {
				t.Fatal("poured formula materialized no step beads")
			}
			var rendered bool
			for _, d := range steps {
				if strings.Contains(d, carried) {
					rendered = true
				}
				if strings.Contains(d, "{{"+briefContextPathVar+"}}") {
					t.Errorf("step text left %s unsubstituted: %q", briefContextPathVar, d)
				}
			}
			if !rendered {
				t.Errorf("no step rendered the carried brief path %q: %v", carried, steps)
			}
		})
	}
}

// poredStepDescriptions returns the descriptions of the step beads materialized
// under a wisp root, so a test can assert what the agent will actually read.
func poredStepDescriptions(t *testing.T, store beads.Store, rootID string) []string {
	t.Helper()
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("listing beads: %v", err)
	}
	var out []string
	for _, b := range all {
		if b.ParentID == rootID {
			out = append(out, b.Description)
		}
	}
	return out
}

// TestDoSlingDeclaresNeitherIsSilent is the operator-facing regression this bead
// exists for: a polecat-shaped formula declaring neither var must produce no
// context_path/requirements_path note on a described bead.
func TestDoSlingDeclaresNeitherIsSilent(t *testing.T) {
	env := newBriefTestEnv(t, map[string]string{"polecat-like": briefFixtureNeither})
	beadID := env.createBead(t, "Implement the widget.")

	result, err := DoSling(SlingOpts{
		Target:        env.agent,
		BeadOrFormula: beadID,
		OnFormula:     "polecat-like",
	}, env.deps, env.deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	for _, w := range result.BeadWarnings {
		if strings.Contains(w, briefContextPathVar) || strings.Contains(w, briefRequirementsPathVar) {
			t.Errorf("declares-neither formula still warns: %q", w)
		}
	}
}

// shippedCoreFormulaDir returns the core formula directory that ships inside
// this binary, derived from this file's location so the test runs from any
// working directory.
func shippedCoreFormulaDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	return filepath.Join(root, "internal", "bootstrap", "packs", "core", "formulas")
}

// TestShippedCoreFormulasDeclareNoContextVar guards the operator-facing half of
// gascity-zmli against the formulas that actually ship in this binary. The note
// that cost operators re-slings fired on the polecat family, which reads its
// bead directly in step 1 and declares no context var to carry anything into —
// so every core formula must resolve to "declares neither", i.e. silent.
//
// If a core formula ever gains a context_path, this test fails and the author
// has to decide deliberately whether the brief should now be carried into it.
func TestShippedCoreFormulasDeclareNoContextVar(t *testing.T) {
	dir := shippedCoreFormulaDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var checked int
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".toml")
		if e.IsDir() || name == e.Name() {
			continue
		}
		checked++
		declared := declaredBriefVars(name, []string{dir})
		if declared[briefContextPathVar] || declared[briefRequirementsPathVar] {
			t.Errorf("core formula %s now declares a context var (%v); decide whether the brief should be carried into it", name, declared)
		}
	}
	if checked == 0 {
		t.Fatalf("no core formulas found under %s; the path is wrong and this test is vacuous", dir)
	}
	t.Logf("checked %d shipped core formulas", checked)
}

// TestDeclaredBriefVarsSeesInheritedVars covers the real build-basic shape:
// build-basic's own file declares no [vars.context_path] — it inherits the var
// through `extends = ["build-base"]`, and `gc formula show build-basic` lists
// it. declaredBriefVars therefore has to read the RESOLVED formula, not the
// raw one; reading the raw file would silently fall through to "declares
// neither" and warn on a formula that can in fact be carried into.
func TestDeclaredBriefVarsSeesInheritedVars(t *testing.T) {
	dir := t.TempDir()
	fixtures := map[string]string{
		"ctx-base": `formula = "ctx-base"
version = 1

[vars.context_path]
description = "Optional source context bundle path."
default = ""

[[steps]]
id = "plan"
title = "Plan"
description = "Read {{context_path}}."
`,
		"ctx-child": `formula = "ctx-child"
version = 1
extends = ["ctx-base"]
`,
		"plain-base": `formula = "plain-base"
version = 1

[[steps]]
id = "work"
title = "Work"
description = "Work."
`,
		"plain-child": `formula = "plain-child"
version = 1
extends = ["plain-base"]
`,
	}
	for name, content := range fixtures {
		if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(content), 0o644); err != nil {
			t.Fatalf("writing fixture %s: %v", name, err)
		}
	}

	if got := declaredBriefVars("ctx-child", []string{dir}); !got[briefContextPathVar] {
		t.Errorf("inherited context_path not detected: %v", got)
	}
	if got := declaredBriefVars("plain-child", []string{dir}); got[briefContextPathVar] || got[briefRequirementsPathVar] {
		t.Errorf("plain-child inherits no context var, got %v", got)
	}
	// An unloadable formula reports neither rather than guessing — the attach
	// that follows surfaces the real error.
	if got := declaredBriefVars("no-such-formula", []string{dir}); len(got) != 0 {
		t.Errorf("unloadable formula: want no declared vars, got %v", got)
	}
}

// failingQuerier fails every Get, standing in for a transient store fault on
// the read the carry depends on.
type failingQuerier struct{ err error }

func (f failingQuerier) Get(string) (beads.Bead, error) { return beads.Bead{}, f.err }

// TestResolveBeadBriefCarryHintsWhenBeadUnreadable pins that an unreadable bead
// is reported rather than silently skipped. This path DELIVERS the brief now,
// so falling silent here would let a formula plan against a bare title with no
// signal — the exact failure the original note existed to surface.
func TestResolveBeadBriefCarryHintsWhenBeadUnreadable(t *testing.T) {
	env := newBriefTestEnv(t, map[string]string{"plan-context": briefFixtureContextOnly})
	opts := SlingOpts{Target: env.agent, BeadOrFormula: "tb-unreadable"}

	carry := resolveBeadBriefCarry(opts, env.deps, failingQuerier{err: errors.New("dolt: connection refused")}, "tb-unreadable", "plan-context")

	if carry.Hint == "" {
		t.Fatal("unreadable bead: want a hint, got silence")
	}
	if !strings.Contains(carry.Hint, "tb-unreadable") || !strings.Contains(carry.Hint, "connection refused") {
		t.Errorf("hint should name the bead and the cause: %q", carry.Hint)
	}
	if len(carry.Vars) != 0 {
		t.Errorf("unreadable bead must carry nothing, got %v", carry.Vars)
	}
	// A caller who supplied the var owns the context, so an unreadable bead is
	// not their problem and must stay silent.
	optsWithVar := SlingOpts{Target: env.agent, BeadOrFormula: "tb-unreadable", Vars: []string{"context_path=/caller/bundle"}}
	if c := resolveBeadBriefCarry(optsWithVar, env.deps, failingQuerier{err: errors.New("boom")}, "tb-unreadable", "plan-context"); c.Hint != "" {
		t.Errorf("caller-supplied context_path should stay silent, got %q", c.Hint)
	}
}
