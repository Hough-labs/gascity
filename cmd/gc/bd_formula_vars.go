package main

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

// Rig-scoped `[rigs.formula_vars]` reach formulas instantiated by `gc sling`
// through sling.BuildSlingFormulaVars. Nothing in the molecule/wisp path read
// them, so a formula poured by `gc bd mol wisp` — the shape every patrol loop
// uses — resolved unscoped against formula defaults and the rig's configured
// lint/test commands and merge gates never took effect.
//
// This file closes both halves of that gap for the `gc bd` passthrough, which
// already resolves rig scope for store routing and so can supply the vars
// without any pack change:
//
//   - Pour time: `mol wisp` / `mol pour` invocations gain a `--var` for each
//     rig formula var the caller did not set explicitly, so the poured root
//     (and any materialized step) renders with real values.
//   - Read time: `formula show` — the command a root-only wisp's agent uses to
//     read its steps, since root-only skips step materialization — has its
//     output rendered with the same vars instead of emitting raw
//     `{{placeholder}}` text.
//
// Precedence matches sling.BuildSlingFormulaVars: explicit --var beats rig
// formula_vars, which beat formula-level defaults.
//
// The read path substitutes rig formula_vars and NOTHING ELSE. Filling the
// remaining placeholders from the formula's own `[vars.*].default` was tried
// and is unsafe: mol-refinery-patrol declares `target_branch = "main"`, while
// the rig that pours it merges to `edge-integration` and supplies the real
// value as a pour-time `--var`. Rendering the default would hand the agent a
// plausible, wrong merge target where it previously saw an obviously
// unresolved `{{target_branch}}`. A visible placeholder is recoverable; a
// confidently wrong branch name is not. Pour-time routing vars stay the pour's
// business — see sling.SlingFormulaTargetBranch, which owns that resolution.

// molPourSubcommands are the `bd mol <sub>` subcommands that instantiate a
// formula and therefore accept formula vars. `mol burn` / `mol current` and
// friends operate on already-poured molecules and take no vars.
var molPourSubcommands = map[string]bool{"wisp": true, "pour": true}

// molWispManagementArgs are the positional arguments that select a `bd mol
// wisp` management subcommand rather than name a formula to pour.
var molWispManagementArgs = map[string]bool{"list": true, "gc": true}

// bdMolPourFormulaIndex returns the index in args of the positional naming the
// formula a `bd mol wisp` / `bd mol pour` invocation instantiates.
//
// It reports false when args is not such an invocation, when the positional is
// a `mol wisp` management subcommand (`list`, `gc`), when no formula name is
// present, or when an unrecognized flag makes the scan ambiguous. Ambiguity
// fails closed: an unknown flag may consume the next argument, so treating the
// following token as a formula name could inject vars into the wrong command.
func bdMolPourFormulaIndex(args []string) (int, bool) {
	if len(args) < 3 || args[0] != "mol" || !molPourSubcommands[args[1]] {
		return 0, false
	}
	sub := "mol " + args[1]
	valueFlags := bdflags.ValueFlags(sub)
	boolFlags := bdflags.BoolFlags(sub)
	if valueFlags == nil || boolFlags == nil {
		return 0, false
	}

	for i := 2; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			// Everything after the terminator is positional.
			if i+1 < len(args) {
				return i + 1, true
			}
			return 0, false
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			name, _, inline := strings.Cut(arg, "=")
			switch {
			case inline:
				// --flag=value carries its own value.
			case valueFlags[name]:
				i++
			case boolFlags[name]:
			default:
				return 0, false
			}
			continue
		}
		if args[1] == "wisp" && molWispManagementArgs[arg] {
			return 0, false
		}
		return i, true
	}
	return 0, false
}

// explicitFormulaVarKeys returns the formula var names the caller supplied
// explicitly via `--var name=value` or `--var=name=value`. Those always win
// over rig defaults, mirroring sling.BuildSlingFormulaVars.
func explicitFormulaVarKeys(args []string) map[string]bool {
	keys := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		var pair string
		switch {
		case args[i] == "--var" && i+1 < len(args):
			pair = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--var="):
			pair = strings.TrimPrefix(args[i], "--var=")
		default:
			continue
		}
		if key, _, ok := strings.Cut(pair, "="); ok && key != "" {
			keys[key] = true
		}
	}
	return keys
}

// injectRigFormulaVars returns args with a `--var name=value` pair added for
// every rig formula var the caller did not set explicitly. Args that do not
// pour a formula are returned unchanged. The input slice is never mutated, and
// injected pairs are ordered by name so the forwarded command is deterministic.
//
// Injection lands before a bare `--` terminator when one is present, so the
// added flags stay flags rather than becoming positional arguments.
func injectRigFormulaVars(args []string, rigVars map[string]string) []string {
	if len(rigVars) == 0 {
		return args
	}
	if _, ok := bdMolPourFormulaIndex(args); !ok {
		return args
	}

	explicit := explicitFormulaVarKeys(args)
	names := make([]string, 0, len(rigVars))
	for name := range rigVars {
		if !explicit[name] {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return args
	}
	sort.Strings(names)

	injected := make([]string, 0, 2*len(names))
	for _, name := range names {
		injected = append(injected, "--var", name+"="+rigVars[name])
	}

	insertAt := len(args)
	for i, arg := range args {
		if arg == "--" {
			insertAt = i
			break
		}
	}

	out := make([]string, 0, len(args)+len(injected))
	out = append(out, args[:insertAt]...)
	out = append(out, injected...)
	out = append(out, args[insertAt:]...)
	return out
}

// bdFormulaShowName returns the formula name a `bd formula show` invocation
// targets. "formula show" is deliberately outside the per-subcommand bdflags
// manifest (it takes no flags of its own), so the scan uses the global flag
// sets. It reports false for any other command, for a missing name, and — fail
// closed, as in bdMolPourFormulaIndex — for an unrecognized flag.
func bdFormulaShowName(args []string) (string, bool) {
	if len(args) < 3 || args[0] != "formula" || args[1] != "show" {
		return "", false
	}
	valueFlags := bdflags.GlobalValueFlags()
	boolFlags := bdflags.GlobalBoolFlags()

	for i := 2; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			name, _, inline := strings.Cut(arg, "=")
			switch {
			case inline:
			case valueFlags[name]:
				i++
			case boolFlags[name]:
			default:
				return "", false
			}
			continue
		}
		return arg, true
	}
	return "", false
}

// bdArgsRequestJSON reports whether the bd invocation asks for JSON output.
// Tokens after a bare `--` terminator are positional and do not count.
func bdArgsRequestJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--json" {
			return true
		}
	}
	return false
}

// rigFormulaVarsForBdTarget returns the `[rigs.formula_vars]` defaults for the
// rig a bd command resolved to. City-scoped commands have no rig formula vars,
// so the result is empty rather than nil-checked by callers.
func rigFormulaVarsForBdTarget(cfg *config.City, target execStoreTarget) map[string]string {
	if cfg == nil || strings.TrimSpace(target.RigName) == "" {
		return nil
	}
	rig, ok := rigByName(cfg, target.RigName)
	if !ok {
		return nil
	}
	return cloneStringMap(rig.FormulaVars)
}

// varListingLabel matches the label bd's text `formula show` gives each entry
// in its "Variables:" section: the variable's own placeholder opening the line,
// immediately followed by a colon.
var varListingLabel = regexp.MustCompile(`^\s*\{\{[a-zA-Z_][a-zA-Z0-9_]*\}\}:`)

// substituteFormulaShowOutput renders {{placeholder}} tokens in bd's `formula
// show` output using vars, leaving tokens with no value intact exactly as
// formula.Substitute does elsewhere.
//
// In JSON mode the substitution happens on the raw document with each value
// pre-escaped as JSON string content, so bd's own formatting and key order
// survive untouched and a value carrying quotes or backslashes cannot corrupt
// the document. The JSON form names its variables as object keys, so no label
// needs protecting there.
//
// The text form does need it: bd labels each entry of its "Variables:" section
// with that variable's own placeholder, so a blind rewrite turns the inventory
// line "{{lint_command}}: Lint command ..." into "make lint: Lint command ...",
// destroying the list of variable names an operator reads it for. A placeholder
// that opens its own line as a listing label describes the formula rather than
// standing in for a value, so it is left alone; the rest of that line, and
// every other line, still renders.
func substituteFormulaShowOutput(out []byte, vars map[string]string, jsonMode bool) []byte {
	if len(vars) == 0 || len(out) == 0 {
		return out
	}
	if jsonMode {
		encodedVars := make(map[string]string, len(vars))
		for name, value := range vars {
			encoded, err := json.Marshal(value)
			if err != nil || len(encoded) < 2 {
				continue
			}
			// Strip the surrounding quotes: the placeholder already sits
			// inside a JSON string literal.
			encodedVars[name] = string(encoded[1 : len(encoded)-1])
		}
		return []byte(formula.Substitute(string(out), encodedVars))
	}

	lines := strings.Split(string(out), "\n")
	for i, line := range lines {
		if loc := varListingLabel.FindStringIndex(line); loc != nil {
			lines[i] = line[:loc[1]] + formula.Substitute(line[loc[1]:], vars)
			continue
		}
		lines[i] = formula.Substitute(line, vars)
	}
	return []byte(strings.Join(lines, "\n"))
}
