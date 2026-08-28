package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// writeDirPinnedSpecialistsPack lays down a local pack whose single rig-scoped
// agent pins `dir` to ownerRig. A local pack source resolves to one
// registration, so that pin is what turns a second importing rig into a
// duplicate agent identity.
func writeDirPinnedSpecialistsPack(t *testing.T, cityPath, ownerRig string) {
	t.Helper()
	packDir := filepath.Join(cityPath, "packs", "specialists")
	if err := os.MkdirAll(filepath.Join(packDir, "agents", "iris"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"),
		[]byte("[pack]\nname = \"specialists\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentToml := fmt.Sprintf("name = \"iris\"\nscope = \"rig\"\ndir = %q\n", ownerRig)
	if err := os.WriteFile(filepath.Join(packDir, "agents", "iris", "agent.toml"),
		[]byte(agentToml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// collisionCity writes a city that already has rig "alpha" importing the
// dir-pinned specialists pack, and offers that same pack as an unprompted
// [defaults.rig.imports] entry — the exact configuration `gc rig add` turned
// into a time bomb in gascity-wjq7.
func collisionCity(t *testing.T) (cityPath string) {
	t.Helper()
	cityPath = t.TempDir()
	alphaPath := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(alphaPath, 0o755); err != nil {
		t.Fatal(err)
	}
	// A rig's machine-local path lives in .gc/site.toml, not in city.toml —
	// city.toml rig.path is a rejected pre-1.0 surface.
	cityToml := `[workspace]

[defaults.rig.imports.specialists]
source = "packs/specialists"

[[rigs]]
name = "alpha"
prefix = "al"

[rigs.imports.specialists]
source = "packs/specialists"
`
	siteToml := fmt.Sprintf("workspace_name = \"test-city\"\n\n[[rig]]\nname = \"alpha\"\npath = %q\n", alphaPath)
	writeSchema2RigCity(t, cityPath, "test-city", cityToml, siteToml)
	writeDirPinnedSpecialistsPack(t, cityPath, "alpha")
	return cityPath
}

// assertFreshInitWouldStart runs the validation the supervisor's fresh-init
// path runs. Before gascity-wjq7 this is where the city died — hours after the
// edit, on the next restart, naming an agent in a rig the operator had not
// touched.
func assertFreshInitWouldStart(t *testing.T, cityPath string) {
	t.Helper()
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("loading city.toml after rig add: %v", err)
	}
	if err := config.ValidateAgents(cfg.Agents); err != nil {
		data, _ := os.ReadFile(filepath.Join(cityPath, "city.toml"))
		t.Fatalf("rig add wrote a config that is gc-fatal to a fresh supervisor init: %v\ncity.toml:\n%s", err, data)
	}
}

// TestRigAddDropsCollidingDefaultImport is the end-to-end regression for
// gascity-wjq7: adding a rig must not silently inherit a default import that
// republishes an agent identity another rig already owns.
func TestRigAddDropsCollidingDefaultImport(t *testing.T) {
	cityPath := collisionCity(t)
	betaPath := filepath.Join(t.TempDir(), "beta")
	if err := os.MkdirAll(betaPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, betaPath, nil, "beta", "be", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	assertFreshInitWouldStart(t, cityPath)

	// The operator must be able to connect the dropped import to the command
	// they just ran, which is the whole point of catching this at write time.
	warning := stderr.String()
	for _, want := range []string{"specialists", "alpha"} {
		if !strings.Contains(warning, want) {
			t.Errorf("stderr does not name %q; operator cannot connect the drop to the collision:\n%s", want, warning)
		}
	}
}

// TestRigAddRefusesCollidingExplicitInclude locks the other half of the
// asymmetry: an import the operator asked for by name is refused, not dropped,
// and the refusal leaves city.toml untouched.
func TestRigAddRefusesCollidingExplicitInclude(t *testing.T) {
	cityPath := collisionCity(t)
	before, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	betaPath := filepath.Join(t.TempDir(), "beta")
	if err := os.MkdirAll(betaPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, betaPath, []string{"packs/specialists"}, "beta", "be", "", false, false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doRigAdd accepted an --include that bricks a fresh init; stdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "alpha") {
		t.Errorf("refusal does not name the rig that already publishes the agent:\n%s", stderr.String())
	}

	after, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("a refused rig add rewrote city.toml:\n%s", after)
	}
}

// TestReloadSaysDuplicateAgentsAreFatalToFreshInit locks the asymmetry the
// gascity-wjq7 incident turned on. `gc reload` cannot be fatal — there is a
// running city to keep serving — but a warning that reads like any other
// warning is how a bad edit sat for hours and then killed the city on an
// unrelated restart. The reload error must say the config will not survive a
// fresh init.
func TestReloadSaysDuplicateAgentsAreFatalToFreshInit(t *testing.T) {
	configureTestDoltIdentityEnv(t)

	dir := shortSocketTempDir(t, "gc-reload-dupagent-")
	alphaPath := filepath.Join(dir, "alpha")
	betaPath := filepath.Join(dir, "beta")
	for _, p := range []string{alphaPath, betaPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cityToml := `[workspace]
name = "test"

[[rigs]]
name = "alpha"
prefix = "al"

[rigs.imports.specialists]
source = "packs/specialists"

[[rigs]]
name = "beta"
prefix = "be"

[rigs.imports.specialists]
source = "packs/specialists"
`
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"),
		[]byte("[pack]\nname = \"test\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(config.SiteBindingPath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	siteToml := fmt.Sprintf("workspace_name = \"test\"\n\n[[rig]]\nname = \"alpha\"\npath = %q\n\n[[rig]]\nname = \"beta\"\npath = %q\n", alphaPath, betaPath)
	if err := os.WriteFile(config.SiteBindingPath(dir), []byte(siteToml), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDirPinnedSpecialistsPack(t, dir, "alpha")

	_, err := tryReloadConfig(tomlPath, "test", dir)
	if err == nil {
		t.Fatal("expected agent validation to reject two rigs importing a dir-pinned pack")
	}
	if !strings.Contains(err.Error(), "validating agents") {
		t.Errorf("reload error lost its %q prefix: %v", "validating agents", err)
	}
	if !strings.Contains(err.Error(), "FATAL to a fresh supervisor init") {
		t.Errorf("reload error does not warn that the config bricks the next restart: %v", err)
	}
}
