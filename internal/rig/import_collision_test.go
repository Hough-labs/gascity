package rig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// writeLocalPack lays down a local pack under <city>/packs/<packName> with one
// rig-scoped agent. When ownerRig is non-empty the agent pins `dir` to it —
// the shape that makes a local pack source single-registration, so a second
// importing rig republishes the same agent identity (gascity-wjq7, twin of
// gc-jfa4). With ownerRig empty the agent takes each importing rig's own name,
// which is the ordinary shareable shape.
func writeLocalPack(t *testing.T, cityPath, packName, agentName, ownerRig string) {
	t.Helper()
	packDir := filepath.Join(cityPath, "packs", packName)
	if err := os.MkdirAll(filepath.Join(packDir, "agents", agentName), 0o755); err != nil {
		t.Fatal(err)
	}
	packToml := fmt.Sprintf("[pack]\nname = %q\nschema = 2\n", packName)
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatal(err)
	}
	agentToml := fmt.Sprintf("name = %q\nscope = \"rig\"\n", agentName)
	if ownerRig != "" {
		agentToml += fmt.Sprintf("dir = %q\n", ownerRig)
	}
	if err := os.WriteFile(filepath.Join(packDir, "agents", agentName, "agent.toml"), []byte(agentToml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// twoRigCity returns a city whose two rigs both import packs/<packName> under
// the same binding — "alpha" standing for the rig that already had the import
// and "beta" for the one `gc rig add` is registering.
func twoRigCity(cityPath, packName string) *config.City {
	imports := func() map[string]config.Import {
		return map[string]config.Import{packName: {Source: "packs/" + packName}}
	}
	return &config.City{Rigs: []config.Rig{
		{Name: "alpha", Path: filepath.Join(cityPath, "alpha"), Prefix: "al", Imports: imports()},
		{Name: "beta", Path: filepath.Join(cityPath, "beta"), Prefix: "be", Imports: imports()},
	}}
}

func expandedAgents(t *testing.T, cityPath string, cfg *config.City) []config.Agent {
	t.Helper()
	probe := &config.City{Rigs: cfg.Rigs}
	if err := config.ExpandPacks(probe, fsys.OSFS{}, cityPath, nil); err != nil {
		t.Fatalf("expanding packs: %v", err)
	}
	return probe.Agents
}

// TestGuardRigImportCollisions_DropsUnpromptedDefaultImport is the core
// regression for gascity-wjq7: the second rig's import came from
// [defaults.rig.imports], not from anything the operator typed, so the guard
// drops it and warns instead of refusing the add — and the config that
// survives passes the agent validation a fresh supervisor init runs.
func TestGuardRigImportCollisions_DropsUnpromptedDefaultImport(t *testing.T) {
	cityPath := t.TempDir()
	writeLocalPack(t, cityPath, "specialists", "iris", "alpha")
	cfg := twoRigCity(cityPath, "specialists")
	defaults := []config.BoundImport{{Binding: "specialists", Import: config.Import{Source: "packs/specialists"}}}

	res, err := guardRigImportCollisions(fsys.OSFS{}, cityPath, cfg, "beta", false, defaults)
	if err != nil {
		t.Fatalf("guardRigImportCollisions: %v", err)
	}
	if _, still := cfg.Rigs[1].Imports["specialists"]; still {
		t.Error("colliding default import survived on the rig being added")
	}
	if len(res.keptDefaults) != 0 {
		t.Errorf("keptDefaults = %v, want none (the only default collided)", res.keptDefaults)
	}
	if len(res.warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one naming the collision", res.warnings)
	}
	for _, want := range []string{"specialists", "alpha", "iris"} {
		if !strings.Contains(res.warnings[0], want) {
			t.Errorf("warning %q does not name %q", res.warnings[0], want)
		}
	}
	if err := config.ValidateAgents(expandedAgents(t, cityPath, cfg)); err != nil {
		t.Fatalf("config that survived the guard still fails a fresh supervisor init: %v", err)
	}
}

// TestGuardRigImportCollisions_RefusesExplicitInclude locks the asymmetry: an
// import the operator asked for by --include is refused rather than silently
// dropped, so `gc rig add` never reports success for a rig that does not have
// the pack the flag requested.
func TestGuardRigImportCollisions_RefusesExplicitInclude(t *testing.T) {
	cityPath := t.TempDir()
	writeLocalPack(t, cityPath, "specialists", "iris", "alpha")
	cfg := twoRigCity(cityPath, "specialists")

	_, err := guardRigImportCollisions(fsys.OSFS{}, cityPath, cfg, "beta", true, nil)
	if err == nil {
		t.Fatal("expected an error for an explicit --include that collides with another rig")
	}
	for _, want := range []string{"beta", "alpha", "specialists"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if _, dropped := cfg.Rigs[1].Imports["specialists"]; !dropped {
		t.Error("a refused add must leave the config untouched, not half-edited")
	}
}

// TestGuardRigImportCollisions_SharedPackWithoutPinnedDirIsFine is the
// false-positive guard. Every rig in a real city imports the same gastown pack;
// that is healthy precisely because those agents do not pin `dir`, so each rig
// publishes its own identity. The guard must key on the published identity, not
// on "two rigs name the same source".
func TestGuardRigImportCollisions_SharedPackWithoutPinnedDirIsFine(t *testing.T) {
	cityPath := t.TempDir()
	writeLocalPack(t, cityPath, "shared", "polecat", "")
	cfg := twoRigCity(cityPath, "shared")
	defaults := []config.BoundImport{{Binding: "shared", Import: config.Import{Source: "packs/shared"}}}

	res, err := guardRigImportCollisions(fsys.OSFS{}, cityPath, cfg, "beta", false, defaults)
	if err != nil {
		t.Fatalf("guardRigImportCollisions refused a legitimately shared pack: %v", err)
	}
	if len(res.warnings) != 0 {
		t.Errorf("warnings = %v, want none", res.warnings)
	}
	if _, ok := cfg.Rigs[1].Imports["shared"]; !ok {
		t.Error("guard dropped an import that does not collide")
	}
	if len(res.keptDefaults) != 1 {
		t.Errorf("keptDefaults = %v, want the single non-colliding default", res.keptDefaults)
	}
	if err := config.ValidateAgents(expandedAgents(t, cityPath, cfg)); err != nil {
		t.Fatalf("shared pack without a pinned dir should validate: %v", err)
	}
}

// writeCityScopedPack lays down a local pack whose single agent is city-scoped.
// Such an agent is hoisted out of rig scope and deduped city-wide, so every rig
// may import the pack without duplicating it.
func writeCityScopedPack(t *testing.T, cityPath, packName, agentName string) {
	t.Helper()
	packDir := filepath.Join(cityPath, "packs", packName)
	if err := os.MkdirAll(filepath.Join(packDir, "agents", agentName), 0o755); err != nil {
		t.Fatal(err)
	}
	packToml := fmt.Sprintf("[pack]\nname = %q\nschema = 2\n", packName)
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatal(err)
	}
	agentToml := fmt.Sprintf("name = %q\nscope = \"city\"\n", agentName)
	if err := os.WriteFile(filepath.Join(packDir, "agents", agentName, "agent.toml"), []byte(agentToml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGuardRigImportCollisions_CityScopedAgentIsNotACollision is the guard's
// most important false-positive test, because the shape it rules out is the
// common one: every rig in a real city imports the same base pack, and that
// pack carries city-scoped agents. Those are hoisted and merged with dedupe
// (config.mergeHoistedCityAgents), so N importers still yield one agent — but a
// per-rig probe sees N pre-dedupe copies. Counting them made the guard report a
// collision for every rig pair in a six-rig city that starts fine, which would
// have broken `gc rig add` outright rather than fixing it.
func TestGuardRigImportCollisions_CityScopedAgentIsNotACollision(t *testing.T) {
	cityPath := t.TempDir()
	writeCityScopedPack(t, cityPath, "base", "boot")
	cfg := twoRigCity(cityPath, "base")
	defaults := []config.BoundImport{{Binding: "base", Import: config.Import{Source: "packs/base"}}}

	res, err := guardRigImportCollisions(fsys.OSFS{}, cityPath, cfg, "beta", false, defaults)
	if err != nil {
		t.Fatalf("guard refused a shared pack whose agent is city-scoped: %v", err)
	}
	if len(res.warnings) != 0 {
		t.Errorf("warnings = %v, want none", res.warnings)
	}
	if _, ok := cfg.Rigs[1].Imports["base"]; !ok {
		t.Error("guard dropped a shared city-scoped pack import that does not collide")
	}
	if err := config.ValidateAgents(expandedAgents(t, cityPath, cfg)); err != nil {
		t.Fatalf("two rigs importing one city-scoped pack should validate: %v", err)
	}
}

// TestFindRigImportCollisions_ReportsUncheckedRig covers the fail-open path: a
// rig whose packs cannot be expanded is reported as unchecked rather than
// blocking the add or being dropped on the floor.
func TestFindRigImportCollisions_ReportsUncheckedRig(t *testing.T) {
	cityPath := t.TempDir()
	writeLocalPack(t, cityPath, "specialists", "iris", "alpha")
	cfg := twoRigCity(cityPath, "specialists")
	cfg.Rigs[0].Imports = map[string]config.Import{"missing": {Source: "packs/does-not-exist"}}

	collisions, unchecked := findRigImportCollisions(fsys.OSFS{}, cityPath, cfg, "beta")
	if len(collisions) != 0 {
		t.Errorf("collisions = %v, want none (the other rig could not be read)", collisions)
	}
	if len(unchecked) != 1 || unchecked[0] != "alpha" {
		t.Errorf("unchecked = %v, want [alpha]", unchecked)
	}
}
