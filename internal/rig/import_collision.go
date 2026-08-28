package rig

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// agentIdentity is the (dir, binding, name) triple config.ValidateAgents keys
// agent uniqueness on. Two rigs that publish the same triple make the city's
// config gc-fatal on a fresh supervisor init.
type agentIdentity struct{ dir, binding, name string }

// importCollision is one agent identity that the rig being added would publish
// and that another rig already publishes. At most one is reported per
// (binding, owning rig) pair: the operator's fix is per import, not per agent,
// so a four-agent pack yields one finding with one exemplar agent.
type importCollision struct {
	// binding is the added rig's import binding that publishes the identity,
	// or "" when the identity came from a source the rig does not import under
	// a binding (its own path pack, or a legacy include).
	binding string
	// source is the import source behind binding, when there is one.
	source string
	// agent is the exemplar agent's qualified name, as ValidateAgents renders
	// it in the duplicate-name error the operator will otherwise meet at
	// restart.
	agent string
	// ownerRig is the rig that already publishes the identity.
	ownerRig string
}

func (c importCollision) String() string {
	if c.binding == "" {
		return fmt.Sprintf("agent %q is already published by rig %q", c.agent, c.ownerRig)
	}
	return fmt.Sprintf("import %q (source %q) publishes agent %q, which rig %q already publishes",
		c.binding, c.source, c.agent, c.ownerRig)
}

// collisionKey collapses findings to one per import per owning rig.
type collisionKey struct{ binding, ownerRig string }

// collisionDetail is the shared explanation of why a duplicate identity is
// fatal, appended to both the refusal and the drop warning so the operator
// meets the mechanism at the moment of the edit rather than at a restart hours
// later.
const collisionDetail = "a pack source resolves to one registration, so when its agents pin `dir` to a rig name every rig that imports it publishes the same agent; a fresh supervisor init rejects that as a duplicate name and never starts"

// rigPublishedAgentIdentities expands r's packs in isolation and returns every
// agent identity the rig publishes, keyed by the triple ValidateAgents uses and
// mapped to the qualified name that error would print.
//
// Rigs are expanded one at a time on purpose: a probe holding every rig at once
// would trip city-wide merges (pack runtime registration errors on a collision)
// before the agent sets could be compared, and the per-rig split is what lets a
// collision name the rig that already owns the identity.
//
// City-scoped agents are excluded, and that exclusion is what keeps the guard
// from firing on every healthy city. A pack's city-scoped agent is hoisted out
// of rig scope and merged with dedupe (config.mergeHoistedCityAgents drops a
// hoisted agent whose identity is already present), so N rigs importing one
// shared pack yield exactly ONE such agent no matter how many import it. A
// per-rig probe sees each rig's pre-dedupe copy, so counting them would report
// a collision for every rig pair sharing any pack with a city-scoped agent —
// on the city this was written against, that meant "gastown.boot" colliding
// across all six rigs of a city that starts fine. Only rig-scoped and unscoped
// agents survive per-rig expansion into cfg.Agents, so only they can really
// duplicate.
//
// Modeling boundary: this reads the rig-expansion layer, so a rig's own
// [[rigs.patches]] overrides are applied (config.ExpandPacks applies them) but
// a city-level [[patches.agent]] that re-points an agent's `dir` is not. Such a
// patch could make a reported collision spurious; the guard's messages name
// exactly what it saw so an operator can act on or override the finding.
func rigPublishedAgentIdentities(fs fsys.FS, cityPath string, r config.Rig) (map[agentIdentity]string, error) {
	probe := &config.City{Rigs: []config.Rig{r}}
	if err := config.ExpandPacks(probe, fs, cityPath, nil); err != nil {
		return nil, err
	}
	out := make(map[agentIdentity]string, len(probe.Agents))
	for i := range probe.Agents {
		a := &probe.Agents[i]
		if a.Scope == "city" {
			continue
		}
		out[agentIdentity{dir: a.Dir, binding: a.BindingName, name: a.Name}] = a.QualifiedName()
	}
	return out, nil
}

// findRigImportCollisions reports the agent identities the rig named name would
// publish that another rig in cfg already publishes.
//
// Rigs whose packs do not expand — an uncached remote source, a pack deleted
// upstream — are returned in unchecked instead of failing: this is a guard on
// top of provisioning, and a guard that cannot read a pack must not block an
// add that would otherwise succeed. The caller surfaces unchecked as a warning
// so a skipped check is never silent.
func findRigImportCollisions(fs fsys.FS, cityPath string, cfg *config.City, name string) (collisions []importCollision, unchecked []string) {
	target := rigByName(cfg, name)
	if target == nil {
		return nil, nil
	}
	targetIDs, err := rigPublishedAgentIdentities(fs, cityPath, *target)
	if err != nil {
		return nil, []string{name}
	}
	if len(targetIDs) == 0 {
		return nil, nil
	}
	ids := sortedAgentIdentities(targetIDs)

	reported := make(map[collisionKey]bool, len(ids))
	for i := range cfg.Rigs {
		other := cfg.Rigs[i]
		if other.Name == name {
			continue
		}
		otherIDs, oErr := rigPublishedAgentIdentities(fs, cityPath, other)
		if oErr != nil {
			unchecked = append(unchecked, other.Name)
			continue
		}
		for _, id := range ids {
			if _, dup := otherIDs[id]; !dup {
				continue
			}
			key := collisionKey{binding: id.binding, ownerRig: other.Name}
			if reported[key] {
				continue
			}
			reported[key] = true
			collisions = append(collisions, importCollision{
				binding:  id.binding,
				source:   strings.TrimSpace(target.Imports[id.binding].Source),
				agent:    targetIDs[id],
				ownerRig: other.Name,
			})
		}
	}
	slices.SortFunc(collisions, func(a, b importCollision) int {
		if a.binding != b.binding {
			return strings.Compare(a.binding, b.binding)
		}
		return strings.Compare(a.agent, b.agent)
	})
	return collisions, unchecked
}

// sortedAgentIdentities orders the identity set so the exemplar agent a
// collision names is the same on every run — map iteration order would
// otherwise make the warning text nondeterministic.
func sortedAgentIdentities(ids map[agentIdentity]string) []agentIdentity {
	out := make([]agentIdentity, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	slices.SortFunc(out, func(a, b agentIdentity) int {
		if a.dir != b.dir {
			return strings.Compare(a.dir, b.dir)
		}
		if a.binding != b.binding {
			return strings.Compare(a.binding, b.binding)
		}
		return strings.Compare(a.name, b.name)
	})
	return out
}

// importGuardResult carries what guardRigImportCollisions changed: the default
// rig imports that survived the guard (the banner announces these) and the
// warning lines the caller emits.
type importGuardResult struct {
	keptDefaults []config.BoundImport
	warnings     []string
}

// guardRigImportCollisions keeps `gc rig add` from writing a city.toml that a
// fresh supervisor init would reject (gascity-wjq7, twin of gc-jfa4).
//
// The two cases are deliberately asymmetric, because what the operator asked
// for differs:
//
//   - explicit is true (--include named the source): refuse the add. The
//     operator asked for exactly this import; dropping it silently would report
//     success for a rig that does not have what the flag requested.
//   - explicit is false (the import came unprompted from
//     [defaults.rig.imports]): drop the colliding bindings and warn, naming the
//     collision. Nobody typed that import, and refusing the whole add over a
//     default is the wrong trade.
//
// A collision the guard cannot resolve by dropping a binding — the rig's own
// path pack, or a legacy include — is fatal either way: there is nothing to
// drop, and writing it would brick the next restart.
//
// On success it mutates the added rig's Imports in nextCfg in place, before any
// filesystem mutation, so a dropped binding never reaches city.toml. Every
// refusal returns before the first mutation, so a refused add leaves the config
// exactly as it found it.
func guardRigImportCollisions(fs fsys.FS, cityPath string, nextCfg *config.City, name string, explicit bool, defaultImports []config.BoundImport) (importGuardResult, error) {
	res := importGuardResult{keptDefaults: defaultImports}
	collisions, unchecked := findRigImportCollisions(fs, cityPath, nextCfg, name)
	if len(unchecked) > 0 {
		res.warnings = append(res.warnings, fmt.Sprintf(
			"warning: could not check rig(s) %s for import collisions (their packs did not expand); a shared pack source may still collide on the next restart",
			strings.Join(unchecked, ", ")))
	}
	if len(collisions) == 0 {
		return res, nil
	}
	if explicit {
		return importGuardResult{}, fmt.Errorf("rig %q: %s — %s. Drop the colliding --include, re-point the pack's `dir` at this rig, or add the rig without --include and edit city.toml directly",
			name, collisionSummary(collisions), collisionDetail)
	}

	added := rigByName(nextCfg, name)
	var droppable, undroppable []importCollision
	for _, c := range collisions {
		if c.binding == "" || added == nil {
			undroppable = append(undroppable, c)
			continue
		}
		if _, ok := added.Imports[c.binding]; !ok {
			undroppable = append(undroppable, c)
			continue
		}
		droppable = append(droppable, c)
	}
	if len(undroppable) > 0 {
		return importGuardResult{}, fmt.Errorf("rig %q: %s — %s", name, collisionSummary(undroppable), collisionDetail)
	}

	dropped := make(map[string]bool, len(droppable))
	for _, c := range droppable {
		if dropped[c.binding] {
			continue
		}
		delete(added.Imports, c.binding)
		dropped[c.binding] = true
		res.warnings = append(res.warnings, fmt.Sprintf(
			"warning: dropped default rig import %q (source %q): %s — %s",
			c.binding, c.source, c.String(), collisionDetail))
	}
	if len(added.Imports) == 0 {
		added.Imports = nil
	}
	res.keptDefaults = filterBoundImports(defaultImports, dropped)
	return res, nil
}

// rigByName returns a pointer to the named rig in cfg, or nil when absent.
func rigByName(cfg *config.City, name string) *config.Rig {
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == name {
			return &cfg.Rigs[i]
		}
	}
	return nil
}

// filterBoundImports drops the bindings marked in dropped.
func filterBoundImports(imports []config.BoundImport, dropped map[string]bool) []config.BoundImport {
	if len(dropped) == 0 {
		return imports
	}
	kept := make([]config.BoundImport, 0, len(imports))
	for _, bound := range imports {
		if dropped[bound.Binding] {
			continue
		}
		kept = append(kept, bound)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// collisionSummary renders collisions as one semicolon-joined clause for an
// error message.
func collisionSummary(collisions []importCollision) string {
	parts := make([]string, 0, len(collisions))
	for _, c := range collisions {
		parts = append(parts, c.String())
	}
	return strings.Join(parts, "; ")
}
