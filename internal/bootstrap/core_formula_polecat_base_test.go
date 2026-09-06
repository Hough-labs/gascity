package bootstrap

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

// compileCoreFormula compiles a bundled core-pack formula from disk so tests
// assert on the exact step text shipped agents receive.
func compileCoreFormula(t *testing.T, name string) *formula.Recipe {
	t.Helper()
	prev := formula.IsFormulaV2Enabled()
	formula.SetFormulaV2Enabled(true)
	t.Cleanup(func() { formula.SetFormulaV2Enabled(prev) })

	recipe, err := formula.Compile(context.Background(), name, coreFormulaSearchPaths(t), map[string]string{
		"convoy_id":   "gc-convoy",
		"base_branch": "main",
	})
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return recipe
}

// workBeadRereadStepIDs are the mol-polecat-base steps that act on the work
// bead's brief rather than merely on the branch. Every one of them must read
// the bead itself: see assertStepRereadsWorkBead.
var workBeadRereadStepIDs = []string{"implement", "self-review"}

// assertStepRereadsWorkBead requires a step to derive the work bead from the
// input convoy AND show it. Deriving the ID alone is not enough — `implement`
// did exactly that for its commit message while the brief stayed unread.
func assertStepRereadsWorkBead(t *testing.T, recipe *formula.Recipe, stepID string) {
	t.Helper()
	step := recipe.StepByID(stepID)
	if step == nil {
		t.Fatalf("recipe missing step %s", stepID)
	}
	for _, want := range []string{
		"gc convoy status {{convoy_id}}",
		"WORK_BEAD_ID",
		`gc bd show "$WORK_BEAD_ID"`,
	} {
		if !strings.Contains(step.Description, want) {
			t.Fatalf("step %s description missing %q; a session that never ran load-context has no brief:\n%s",
				stepID, want, step.Description)
		}
	}
}

// TestCoreMolPolecatBaseStepsRereadWorkBead is the regression guard for
// gascity-w2uk. mol-polecat-base reads the work bead once, in `load-context`,
// and that read lives only as long as the session does. The same formula tells
// the agent to `gc runtime request-restart` under context pressure, and the
// controller respawns polecats routinely, so a fresh session can pour straight
// into `implement` with the operator's brief nowhere in its context. The
// instruction to read the bead has to live in the step that executes.
func TestCoreMolPolecatBaseStepsRereadWorkBead(t *testing.T) {
	recipe := compileCoreFormula(t, "mol-polecat-base")
	for _, id := range workBeadRereadStepIDs {
		t.Run(id, func(t *testing.T) {
			assertStepRereadsWorkBead(t, recipe, "mol-polecat-base."+id)
		})
	}
}

// TestCoreMolPolecatCommitInheritsWorkBeadReread pins the property through
// `extends`. Variant formulas override workspace-setup and the terminal step
// but inherit `implement` and `self-review`, so the fix must reach them
// without being forked into each child.
func TestCoreMolPolecatCommitInheritsWorkBeadReread(t *testing.T) {
	recipe := compileCoreFormula(t, "mol-polecat-commit")
	for _, id := range workBeadRereadStepIDs {
		t.Run(id, func(t *testing.T) {
			assertStepRereadsWorkBead(t, recipe, "mol-polecat-commit."+id)
		})
	}
}
