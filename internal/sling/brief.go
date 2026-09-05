package sling

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphv2"
)

// Sling-time carry of a target bead's brief into a formula's rendered context.
//
// A formula wisp root's own description is the FORMULA's boilerplate
// (internal/formula/compile.go rootDesc), never the target bead's text, so a
// formula that plans purely from its rendered context never sees the bead's
// instructions. Sling used to only WARN about that, and the warning fired
// regardless of whether the formula could act on it — inert on the polecat
// family, which reads the bead directly in its first step. Carrying the brief
// removes the gap for the formulas that have a var for it, which in turn makes
// the residual warning narrow enough to be actionable.
const (
	// briefContextPathVar is the formula var holding a read-only context bundle
	// ("Optional source context bundle path"). It is the only var the auto-carry
	// binds.
	briefContextPathVar = "context_path"

	// briefRequirementsPathVar is the formula var naming the requirements
	// artifact a planning formula WRITES ("Requirements artifact path to create
	// or reuse" — planning-base's requirements step writes there, and an
	// artifact-schema check gates the result). The auto-carry never binds it: a
	// brief written to that path is either overwritten by the formula's own
	// artifact or fails the schema gate first. It participates only in the
	// caller-supplied check and in the residual hint.
	briefRequirementsPathVar = "requirements_path"

	// briefDirName is the city-runtime directory holding materialized briefs,
	// a sibling of the molecule artifact tree ResolveSlingEnv projects.
	briefDirName = "briefs"

	// briefFileName is the brief document inside a bead's brief bundle.
	briefFileName = "brief.md"
)

// beadBriefCarry is the outcome of resolving one --on/default-formula attach
// against the target bead's brief: the vars to bind, and the operator
// diagnostic (if any) that survives the carry. The zero value means "carry
// nothing, say nothing".
type beadBriefCarry struct {
	// Vars holds "key=value" entries to append to SlingOpts.Vars.
	Vars []string
	// Hint is an operator diagnostic surfaced via SlingResult.BeadWarnings.
	Hint string
}

// resolveBeadBriefCarry decides whether a formula attach carries the target
// bead's brief into the formula's rendered context, and whether the operator
// still needs a diagnostic. It changes neither routing nor the materialized
// wisp beyond binding one variable.
//
// The rule, in order:
//
//  1. The caller already supplied context_path or requirements_path — they own
//     the context. Carry nothing, say nothing.
//  2. The bead has no description. There is no brief to carry, and nothing to
//     warn about.
//  3. The resolved recipe declares context_path. Materialize the brief, bind it,
//     and stay silent: the formula now has the brief by construction.
//  4. The recipe declares requirements_path but not context_path. Nothing is
//     safe to bind (see briefRequirementsPathVar), but naming that var is still
//     actionable for the operator, so hint.
//  5. The recipe declares neither var. The old note's advice was INERT here —
//     passing the flag binds a variable no step reads — so stay silent. This is
//     the mol-polecat-work / mol-scoped-work case whose false alarms cost
//     operators re-slings across several rigs (gascity-zmli).
func resolveBeadBriefCarry(opts SlingOpts, deps SlingDeps, querier BeadQuerier, beadID, formulaName string) beadBriefCarry {
	if querier == nil || beadID == "" || formulaName == "" {
		return beadBriefCarry{}
	}
	if callerSuppliedBriefVar(opts, deps) {
		return beadBriefCarry{}
	}
	bead, err := querier.Get(beadID)
	if err != nil {
		return beadBriefCarry{}
	}
	brief := formatBeadBrief(bead)
	if brief == "" {
		return beadBriefCarry{}
	}
	declared := declaredBriefVars(formulaName, SlingFormulaSearchPaths(deps, opts.Target))
	if !declared[briefContextPathVar] {
		if declared[briefRequirementsPathVar] {
			return beadBriefCarry{Hint: briefRequirementsOnlyHint(beadID, formulaName)}
		}
		return beadBriefCarry{}
	}
	dir, err := materializeBeadBrief(deps.CityPath, beadID, brief)
	if err != nil {
		return beadBriefCarry{Hint: briefCarryFailedHint(beadID, err)}
	}
	return beadBriefCarry{Vars: []string{briefContextPathVar + "=" + dir}}
}

// callerSuppliedBriefVar reports whether either context var already has a value
// from the caller. It mirrors BuildSlingFormulaVars' precedence for exactly the
// two keys it cares about — explicit --var first, then rig formula_vars — so a
// rig that configures context_path counts as having supplied it, just as a --var
// does. Presence is the test, not emptiness: `--var context_path=` is a
// deliberate opt-out and has always suppressed the note.
func callerSuppliedBriefVar(opts SlingOpts, deps SlingDeps) bool {
	vars := make(map[string]string, len(opts.Vars))
	for _, v := range opts.Vars {
		if key, value, ok := strings.Cut(v, "="); ok && key != "" {
			vars[key] = value
		}
	}
	mergeRigFormulaVars(vars, deps.Cfg, opts.Target)
	if _, ok := vars[briefContextPathVar]; ok {
		return true
	}
	_, ok := vars[briefRequirementsPathVar]
	return ok
}

// declaredBriefVars reports which of the two context vars the named formula
// declares, after `extends` resolution. graphv2.LoadFormula is the plain
// load-and-resolve pass — it implies no graph.v2 semantics — so this costs a
// TOML parse and no recipe compile.
//
// A formula that cannot be loaded reports neither var. The attach that follows
// surfaces the real load error, and guessing here would resurrect the false
// alarm this path exists to remove.
func declaredBriefVars(formulaName string, searchPaths []string) map[string]bool {
	declared := make(map[string]bool, 2)
	resolved, err := graphv2.LoadFormula(formulaName, searchPaths)
	if err != nil || resolved == nil {
		return declared
	}
	for _, name := range []string{briefContextPathVar, briefRequirementsPathVar} {
		if _, ok := resolved.Vars[name]; ok {
			declared[name] = true
		}
	}
	return declared
}

// formatBeadBrief renders the bead's brief as the Markdown document the carried
// bundle holds, or "" when there is nothing to carry.
//
// Only Description is carried. beads.Bead models neither bd's `design` nor its
// `acceptance_criteria` column — no Gas City store reads either — so carrying
// those first needs the bead type and every store extended, which is tracked
// separately (gascity-zmli's discovered follow-up).
func formatBeadBrief(bead beads.Bead) string {
	body := strings.TrimSpace(bead.Description)
	if body == "" {
		return ""
	}
	label := strings.TrimSpace(bead.Title)
	if label == "" {
		label = bead.ID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", label)
	fmt.Fprintf(&b, "Bead: %s\n\n", bead.ID)
	b.WriteString(body)
	b.WriteString("\n")
	return b.String()
}

// materializeBeadBrief writes the brief into a per-bead bundle directory under
// the city runtime root and returns that directory. context_path names a bundle
// DIRECTORY — the shape sling's own note has always told operators to pass
// (`--var context_path=<dir>`).
//
// The path is stable per bead and the document is rewritten on every sling, so
// a re-sling after the bead's description changed carries the current text. The
// write is atomic (temp file + rename) per the project's file-write convention.
func materializeBeadBrief(cityPath, beadID, brief string) (string, error) {
	if strings.TrimSpace(cityPath) == "" {
		return "", errors.New("city path is not configured")
	}
	dir := filepath.Join(cityPath, ".gc", briefDirName, beadID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating brief bundle %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, briefFileName+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("creating brief for %s: %w", beadID, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	// Close unconditionally so the descriptor is released on the write-error
	// path too, then report whichever failure came first.
	_, writeErr := tmp.WriteString(brief)
	closeErr := tmp.Close()
	if writeErr != nil {
		return "", fmt.Errorf("writing brief for %s: %w", beadID, writeErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("writing brief for %s: %w", beadID, closeErr)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return "", fmt.Errorf("writing brief for %s: %w", beadID, err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, briefFileName)); err != nil {
		return "", fmt.Errorf("publishing brief for %s: %w", beadID, err)
	}
	return dir, nil
}

// briefRequirementsOnlyHint is the residual note for a recipe that declares
// requirements_path but not context_path: naming that var is actionable, but
// sling will not bind the brief there itself.
func briefRequirementsOnlyHint(beadID, formulaName string) string {
	return fmt.Sprintf("note: bead %s's description is not carried into %s's rendered context — it declares no %s to carry it into. Pass --var %s=<doc> to supply the instructions yourself; sling will not write there, because the formula treats that path as an artifact it produces.",
		beadID, formulaName, briefContextPathVar, briefRequirementsPathVar)
}

// briefCarryFailedHint reports a carry that could not be materialized. The
// attach still proceeds — a brief the formula cannot read is a degraded pour,
// not a failed one — but the operator is told, so a formula planning against a
// title is never silent.
func briefCarryFailedHint(beadID string, err error) string {
	return fmt.Sprintf("note: could not carry bead %s's description into the formula's rendered context: %v. Pass --var %s=<dir> to supply it yourself.",
		beadID, err, briefContextPathVar)
}
