package sling

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphv2"
)

// Sling-time carry of a target bead's brief into a formula's rendered context.
//
// A formula wisp root's own description is the FORMULA's boilerplate
// (internal/formula/compile.go rootDesc), never the target bead's text, so a
// formula that plans purely from its rendered context never sees the bead's
// instructions. attachedBeadInstructionsDroppedHint only WARNS about that, and
// only when no route carries the bead. For a formula that declares context_path
// there is a route sling can take itself: carry the brief into it, which also
// satisfies that hint's caller-supplied check.
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
	// caller-supplied check.
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

// carryBeadBrief applies resolveBeadBriefCarry to an --on/default-formula
// attach: it returns opts with any carried vars appended, and the carry's
// operator hint. The carry binds a formula var, so it has to run before
// attachFormulaToBead builds the var map; appending to opts.Vars covers the
// legacy and graph.v2 branches at once, since both derive their vars from that
// slice (BuildSlingFormulaVars and prepareGraphV2FormulaInvocation).
//
// opts is a value copy but opts.Vars shares the caller's backing array, so the
// carry appends to a clone rather than risking a stomp.
func carryBeadBrief(opts SlingOpts, deps SlingDeps, querier BeadQuerier, beadID, formulaName string) (SlingOpts, string) {
	carry := resolveBeadBriefCarry(opts, deps, querier, beadID, formulaName)
	if len(carry.Vars) > 0 {
		opts.Vars = append(slices.Clone(opts.Vars), carry.Vars...)
	}
	return opts, carry.Hint
}

// resolveBeadBriefCarry decides whether a formula attach carries the target
// bead's brief into the formula's rendered context, and whether the operator
// needs a diagnostic about a carry that could not be made. It changes neither
// routing nor the materialized wisp beyond binding one variable. Whether the
// operator should be told the description is not reachable at all stays with
// attachedBeadInstructionsDroppedHint.
//
// The rule, in order:
//
//  1. The caller already supplied context_path or requirements_path — they own
//     the context. Carry nothing, say nothing.
//  2. The resolved recipe does not declare context_path. There is nothing to
//     carry into: requirements_path names an artifact the formula WRITES (see
//     briefRequirementsPathVar), and a formula that declares neither var reads
//     its bead through gc.var.issue (gascity-zmli). Say nothing here.
//  3. The bead cannot be read. Hint: the carry was due, so the formula may plan
//     against a bare title.
//  4. The bead has no description. There is no brief to carry.
//  5. Materialize the brief and bind it. The formula now has the brief by
//     construction; a materialize failure is reported as a hint.
func resolveBeadBriefCarry(opts SlingOpts, deps SlingDeps, querier BeadQuerier, beadID, formulaName string) beadBriefCarry {
	if querier == nil || beadID == "" || formulaName == "" {
		return beadBriefCarry{}
	}
	if callerSuppliedBriefVar(opts, deps) {
		return beadBriefCarry{}
	}
	if !declaredBriefVars(formulaName, SlingFormulaSearchPaths(deps, opts.Target))[briefContextPathVar] {
		return beadBriefCarry{}
	}
	bead, err := querier.Get(beadID)
	if err != nil {
		// Do NOT fall silent here. This path DELIVERS the brief, so an
		// unreadable bead means the formula may plan against a bare title. The
		// hint is only appended when the attach itself succeeded, so a genuinely
		// missing bead still fails loudly on the attach instead of producing a
		// spurious note here.
		return beadBriefCarry{Hint: briefCarryFailedHint(beadID, err)}
	}
	brief := formatBeadBrief(bead)
	if brief == "" {
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
// two keys it cares about — explicit --var, then agent formula_vars, then rig
// formula_vars — so an agent or rig that configures context_path counts as
// having supplied it, just as a --var does. Presence is the test, not
// emptiness: `--var context_path=` is a deliberate opt-out.
func callerSuppliedBriefVar(opts SlingOpts, deps SlingDeps) bool {
	vars := make(map[string]string, len(opts.Vars))
	for _, v := range opts.Vars {
		if key, value, ok := strings.Cut(v, "="); ok && key != "" {
			vars[key] = value
		}
	}
	mergeAgentFormulaVars(vars, opts.Target)
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

// briefCarryFailedHint reports a carry that could not be materialized. The
// attach still proceeds — a brief the formula cannot read is a degraded pour,
// not a failed one — but the operator is told, so a formula planning against a
// title is never silent.
func briefCarryFailedHint(beadID string, err error) string {
	return fmt.Sprintf("note: could not carry bead %s's description into the formula's rendered context: %v. Pass --var %s=<dir> to supply it yourself.",
		beadID, err, briefContextPathVar)
}
