package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

func TestRigPackCoverageCheck_NoPacks(t *testing.T) {
	cfg := &config.City{}
	c := NewRigPackCoverageCheck(cfg, t.TempDir())
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestRigPackCoverageCheck_NoRigs(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "patrol"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, packDir, "patrol")

	cfg := &config.City{
		PackDirs: []string{packDir},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if len(r.Details) == 0 {
		t.Error("expected details about orphaned rig-scoped named_sessions")
	}
}

func TestRigPackCoverageCheck_RigIncludesPack(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "patrol"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, packDir, "patrol")

	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs: []config.Rig{
			{Name: "myproject"},
		},
		RigPackDirs: map[string][]string{
			"myproject": {packDir},
		},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
}

func TestRigPackCoverageCheck_SuspendedRigIgnored(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "patrol"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, packDir, "patrol")

	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs: []config.Rig{
			{Name: "myproject", Suspended: true},
		},
		RigPackDirs: map[string][]string{
			"myproject": {packDir},
		},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning (suspended rig should not count); msg = %s", r.Status, r.Message)
	}
}

func TestRigPackCoverageCheck_OnDemandNotWarned(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "helper"
scope = "rig"
mode = "on_demand"
`)
	writeTestAgent(t, packDir, "helper")

	cfg := &config.City{
		PackDirs: []string{packDir},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (on_demand should not warn); msg = %s", r.Status, r.Message)
	}
}

func TestRigPackCoverageCheck_CityScopedIgnored(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "coordinator"
scope = "city"
mode = "always"
`)
	writeTestAgent(t, packDir, "coordinator")

	cfg := &config.City{
		PackDirs: []string{packDir},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (city-scoped should not warn); msg = %s", r.Status, r.Message)
	}
}

func TestRigPackCoverageCheck_MultipleOrphanedSessions(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "patrol"
scope = "rig"
mode = "always"

[[named_session]]
template = "merger"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, packDir, "patrol")
	writeTestAgent(t, packDir, "merger")

	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs: []config.Rig{
			{Name: "myproject"},
		},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	found := 0
	for _, d := range r.Details {
		if strings.Contains(d, "patrol") || strings.Contains(d, "merger") {
			found++
		}
	}
	if found < 2 {
		t.Errorf("expected details for both patrol and merger, got %v", r.Details)
	}
}

func TestRigPackCoverageCheck_PartialCoverage(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "patrol"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, packDir, "patrol")

	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs: []config.Rig{
			{Name: "covered"},
			{Name: "uncovered"},
		},
		RigPackDirs: map[string][]string{
			"covered": {packDir},
		},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning (uncovered rig exists); msg = %s", r.Status, r.Message)
	}
	foundUncovered := false
	for _, d := range r.Details {
		if strings.Contains(d, "uncovered") {
			foundUncovered = true
		}
	}
	if !foundUncovered {
		t.Errorf("expected detail about uncovered rig, got %v", r.Details)
	}
}

// rigCoverageSuspensionCity builds a city where rig "covered" imports a pack
// declaring a rig-scoped always session and rig "parked" does not, so the
// check's verdict turns entirely on whether "parked" counts as active.
func rigCoverageSuspensionCity(t *testing.T) (string, *config.City) {
	t.Helper()
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "patrol"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, packDir, "patrol")
	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs: []config.Rig{
			{Name: "covered"},
			{Name: "parked"},
		},
		RigPackDirs: map[string][]string{
			"covered": {packDir},
		},
	}
	return dir, cfg
}

func setRigCoverageRuntimeSuspension(t *testing.T, cityPath, rig string, suspended bool) {
	t.Helper()
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, rig, &suspended); err != nil {
		t.Fatalf("writing runtime suspension for rig %q: %v", rig, err)
	}
}

func TestRigPackCoverageCheck_SuspendedOnStartRigIgnored(t *testing.T) {
	// `gc rig add --start-suspended` writes suspended_on_start, not the
	// deprecated `suspended` alias. Reading only the alias reported a parked
	// rig as a permanent coverage gap.
	dir, cfg := rigCoverageSuspensionCity(t)
	cfg.Rigs[1].SuspendedOnStart = true

	r := NewRigPackCoverageCheck(cfg, dir).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK (suspended_on_start rig should not count); msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
}

func TestRigPackCoverageCheck_RuntimeSuspendedRigIgnored(t *testing.T) {
	// `gc rig suspend` records only a runtime override; city.toml is untouched.
	dir, cfg := rigCoverageSuspensionCity(t)
	setRigCoverageRuntimeSuspension(t, dir, "parked", true)

	r := NewRigPackCoverageCheck(cfg, dir).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK (runtime-suspended rig should not count); msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
}

func TestRigPackCoverageCheck_ResumedRigStillChecked(t *testing.T) {
	// A rig authored suspended_on_start but resumed at runtime is live, so a
	// coverage gap on it is real and must still be reported.
	dir, cfg := rigCoverageSuspensionCity(t)
	cfg.Rigs[1].SuspendedOnStart = true
	setRigCoverageRuntimeSuspension(t, dir, "parked", false)

	r := NewRigPackCoverageCheck(cfg, dir).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning (resumed rig is active and uncovered); msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "missing from rig(s): parked") {
		t.Fatalf("details = %v, want the resumed rig named as uncovered", r.Details)
	}
}

func TestRigPackCoverageCheck_LegacySuspendedRigStillIgnored(t *testing.T) {
	dir, cfg := rigCoverageSuspensionCity(t)
	cfg.Rigs[1].Suspended = true

	r := NewRigPackCoverageCheck(cfg, dir).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK (config-level suspended rig should not count); msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
}

func TestRigPackCoverageCheck_UnreadableSuspensionStateFallsBackToSuspendedField(t *testing.T) {
	// An unreadable state file must not read as "every rig suspended", which
	// would hide every gap: only the config-level `suspended` field excludes a
	// rig, exactly as before effective suspension was consulted.
	dir, cfg := rigCoverageSuspensionCity(t)
	cfg.Rigs[1].SuspendedOnStart = true
	cfg.Rigs = append(cfg.Rigs, config.Rig{Name: "legacy", Suspended: true})
	statePath := citylayout.SuspensionStateFile(dir)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRigPackCoverageCheck(cfg, dir).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning (parked counts as active without readable state); msg = %s", r.Status, r.Message)
	}
	details := strings.Join(r.Details, "\n")
	if !strings.Contains(details, "missing from rig(s): parked") {
		t.Fatalf("details = %v, want parked reported as uncovered", r.Details)
	}
	if strings.Contains(details, "legacy") {
		t.Fatalf("details = %v, config-level suspended rig must stay excluded", r.Details)
	}
}

func TestRigPackCoverageCheck_FixHint(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "patrol"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, packDir, "patrol")

	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs: []config.Rig{
			{Name: "myproject"},
		},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.FixHint == "" {
		t.Error("expected a fix hint")
	}
	if !strings.Contains(r.FixHint, "defaults.rig.imports") {
		t.Errorf("FixHint = %q, want it to mention [defaults.rig.imports] to guide the operator to the actual config knob", r.FixHint)
	}
	if strings.Contains(r.FixHint, "pack.toml") {
		t.Errorf("FixHint = %q, want default rig imports to point at city.toml", r.FixHint)
	}
}

// TestRigPackCoverageCheck_PackWithoutRigSessions asserts that a pack
// which declares [pack] metadata but no rig-scoped always-mode sessions
// does NOT trigger a warning. The check is about orphaned rig-scoped
// always-mode sessions; a pack that doesn't declare any has nothing to
// orphan. Regression coverage for the "no-sessions" case the PR body
// claims is covered but TestRigPackCoverageCheck_NoPacks does not
// exercise (that one uses a zero-pack city).
func TestRigPackCoverageCheck_PackWithoutRigSessions(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "city-only")
	writeTestPack(t, packDir, `
[pack]
name = "city-only"
schema = 2

[[named_session]]
template = "coordinator"
scope = "city"
mode = "always"
`)
	writeTestAgent(t, packDir, "coordinator")

	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs:     []config.Rig{{Name: "myproject"}},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (no rig-scoped always sessions to orphan); details = %v", r.Status, r.Details)
	}
}

// TestRigPackCoverageCheck_MalformedPackToml asserts the [M1] behavior
// — a pack whose pack.toml exists but cannot be parsed surfaces a
// warning identifying the pack and the parse error, rather than
// silently skipping (which would defeat the diagnostic's purpose).
func TestRigPackCoverageCheck_MalformedPackToml(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "broken")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte("not valid = toml = at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs:     []config.Rig{{Name: "myproject"}},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning (malformed pack.toml); details = %v", r.Status, r.Details)
	}
	var found bool
	for _, d := range r.Details {
		if strings.Contains(d, "broken") && strings.Contains(d, "pack.toml") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a detail identifying packs/broken/pack.toml, got %v", r.Details)
	}
}

// TestRigPackCoverageCheck_SameNameLocalReplacementCovers is the
// regression for #3907: a rig-local pack replacement that shares the
// city pack's [pack].name and declares the identical rig-scoped always
// named_session is a deliberate override (a different polecat namepool,
// local prompt/formula changes, etc.), not a coverage gap. rigHasPackDir
// used to compare only exact absolute pack directory paths, so a rig
// importing packs/gastown-dh instead of the city's remote gastown pack
// always read as uncovered even though it declares the same required
// session under the same pack name.
func TestRigPackCoverageCheck_SameNameLocalReplacementCovers(t *testing.T) {
	dir := t.TempDir()
	cityPackDir := filepath.Join(dir, "packs", "gastown")
	writeTestPack(t, cityPackDir, `
[pack]
name = "gastown"
schema = 2

[[named_session]]
template = "witness"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, cityPackDir, "witness")

	localReplacementDir := filepath.Join(dir, "packs", "gastown-dh")
	writeTestPack(t, localReplacementDir, `
[pack]
name = "gastown"
schema = 2

[[named_session]]
template = "witness"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, localReplacementDir, "witness")

	cfg := &config.City{
		PackDirs: []string{cityPackDir},
		Rigs:     []config.Rig{{Name: "superlzy-dash"}},
		RigPackDirs: map[string][]string{
			"superlzy-dash": {localReplacementDir},
		},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (same-name local replacement declares the required session); msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
}

// TestRigPackCoverageCheck_DifferentNameLocalPackStillUncovered guards
// the #3907 fix's scope: a rig importing an unrelated local pack (a
// different [pack].name) must still be reported as a genuine coverage
// gap — the fix only recognizes a same-named replacement, not any local
// pack at all.
func TestRigPackCoverageCheck_DifferentNameLocalPackStillUncovered(t *testing.T) {
	dir := t.TempDir()
	cityPackDir := filepath.Join(dir, "packs", "gastown")
	writeTestPack(t, cityPackDir, `
[pack]
name = "gastown"
schema = 2

[[named_session]]
template = "witness"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, cityPackDir, "witness")

	unrelatedDir := filepath.Join(dir, "packs", "other")
	writeTestPack(t, unrelatedDir, `
[pack]
name = "other"
schema = 2
`)

	cfg := &config.City{
		PackDirs: []string{cityPackDir},
		Rigs:     []config.Rig{{Name: "myproject"}},
		RigPackDirs: map[string][]string{
			"myproject": {unrelatedDir},
		},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning (unrelated local pack does not cover gastown's witness session); msg = %s", r.Status, r.Message)
	}
}

func writeTestPack(t *testing.T, packDir, content string) {
	t.Helper()
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestAgent(t *testing.T, packDir, name string) {
	t.Helper()
	agentDir := filepath.Join(packDir, "agents", name)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte(`scope = "rig"`), 0o644); err != nil {
		t.Fatal(err)
	}
}
