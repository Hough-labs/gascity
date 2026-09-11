// TestBundledPackAgentsDeclareWakeMode guards gascity-xd9a: a bundled pack
// agent that omits wake_mode inherits Agent.EffectiveWakeMode's "resume"
// default (internal/config/config.go), so every wake reattaches the previous
// provider conversation and the transcript only ever grows. bd's dolt dog
// shipped that way purely by omission while every sibling dog declared
// "fresh", and the omission was mistaken for a deliberate choice for long
// enough to get a sleep policy removed from a live city.
//
// The invariant: a provider-backed agent in a repo-owned bundled pack must
// state its wake mode rather than inherit one. Agents that set start_command
// are exempt — that is ResolveProvider's escape hatch (internal/config/
// resolve.go), which returns a ResolvedProvider with no ResumeCommand and
// never consults the provider catalog, so such a session has no provider
// conversation to resume and wake_mode cannot affect it.

package packlint

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

// bundledPackRoots are the pack trees this repository owns and embeds into the
// gc binary, mirroring the repo-owned entries of builtinpacks.All(). The
// gastown and gascity packs come from the gascity-packs module and are linted
// in that repository.
var bundledPackRoots = []string{
	"internal/bootstrap/packs/core",
	"examples/bd",
}

func TestBundledPackAgentsDeclareWakeMode(t *testing.T) {
	root := repoRoot()

	type agentFile struct {
		StartCommand string `toml:"start_command"`
		WakeMode     string `toml:"wake_mode"`
	}

	var violations []string
	checked := 0
	for _, packRoot := range bundledPackRoots {
		dir := filepath.Join(root, packRoot)
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || d.Name() != "agent.toml" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			var agent agentFile
			if _, err := toml.Decode(string(data), &agent); err != nil {
				return fmt.Errorf("decoding %s: %w", path, err)
			}
			checked++
			if agent.StartCommand != "" {
				// Escape hatch: no provider conversation exists to resume.
				return nil
			}
			if agent.WakeMode == "" {
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					rel = path
				}
				violations = append(violations, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", packRoot, err)
		}
	}

	if checked == 0 {
		t.Fatalf("found no agent.toml under %v; the lint would pass vacuously", bundledPackRoots)
	}
	for _, v := range violations {
		t.Errorf("%s declares no wake_mode and no start_command, so it silently inherits the "+
			"\"resume\" default and grows one provider conversation across every wake; set "+
			"wake_mode explicitly (\"fresh\" for an agent that re-derives its state from beads "+
			"on each start, \"resume\" only when it genuinely needs the prior conversation)", v)
	}
}
