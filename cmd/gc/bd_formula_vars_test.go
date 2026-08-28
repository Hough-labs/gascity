package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestBdMolPourFormulaIndex(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantIndex int
		wantOK    bool
	}{
		{
			name:      "wisp with formula name",
			args:      []string{"mol", "wisp", "mol-refinery-patrol"},
			wantIndex: 2,
			wantOK:    true,
		},
		{
			name:      "wisp with flags before the formula",
			args:      []string{"mol", "wisp", "--root-only", "mol-refinery-patrol"},
			wantIndex: 3,
			wantOK:    true,
		},
		{
			name:      "wisp with a value flag before the formula",
			args:      []string{"mol", "wisp", "--var", "rig_name=gascity", "mol-refinery-patrol"},
			wantIndex: 4,
			wantOK:    true,
		},
		{
			name:      "wisp with an inline value flag before the formula",
			args:      []string{"mol", "wisp", "--var=rig_name=gascity", "mol-refinery-patrol"},
			wantIndex: 3,
			wantOK:    true,
		},
		{
			name:      "wisp with a global value flag before the formula",
			args:      []string{"mol", "wisp", "--actor", "refinery", "mol-refinery-patrol"},
			wantIndex: 4,
			wantOK:    true,
		},
		{
			name:      "pour with formula name",
			args:      []string{"mol", "pour", "mol-polecat-work", "--assignee", "gastown.furiosa"},
			wantIndex: 2,
			wantOK:    true,
		},
		{name: "wisp list is a management subcommand", args: []string{"mol", "wisp", "list"}, wantOK: false},
		{name: "wisp gc is a management subcommand", args: []string{"mol", "wisp", "gc"}, wantOK: false},
		{name: "wisp with no formula", args: []string{"mol", "wisp", "--root-only"}, wantOK: false},
		{name: "unknown flag is ambiguous", args: []string{"mol", "wisp", "--mystery", "x", "mol-a"}, wantOK: false},
		{name: "not a mol command", args: []string{"list", "--json"}, wantOK: false},
		{name: "mol burn is not a pour", args: []string{"mol", "burn", "gascity-wisp-a"}, wantOK: false},
		{name: "empty args", args: nil, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotIndex, gotOK := bdMolPourFormulaIndex(tt.args)
			if gotOK != tt.wantOK {
				t.Fatalf("bdMolPourFormulaIndex(%v) ok = %v, want %v", tt.args, gotOK, tt.wantOK)
			}
			if gotOK && gotIndex != tt.wantIndex {
				t.Errorf("bdMolPourFormulaIndex(%v) index = %d, want %d", tt.args, gotIndex, tt.wantIndex)
			}
		})
	}
}

func TestExplicitFormulaVarKeys(t *testing.T) {
	args := []string{"mol", "wisp", "mol-x", "--var", "rig_name=gascity", "--var=lint_command=make lint"}
	got := explicitFormulaVarKeys(args)
	want := map[string]bool{"rig_name": true, "lint_command": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("explicitFormulaVarKeys = %v, want %v", got, want)
	}
}

func TestInjectRigFormulaVars(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		rigVars map[string]string
		want    []string
	}{
		{
			name:    "no rig vars leaves args untouched",
			args:    []string{"mol", "wisp", "mol-a"},
			rigVars: nil,
			want:    []string{"mol", "wisp", "mol-a"},
		},
		{
			name:    "rig vars are appended in sorted order",
			args:    []string{"mol", "wisp", "mol-a", "--root-only"},
			rigVars: map[string]string{"test_command": "make test", "lint_command": "make lint"},
			want: []string{
				"mol", "wisp", "mol-a", "--root-only",
				"--var", "lint_command=make lint",
				"--var", "test_command=make test",
			},
		},
		{
			name:    "explicit --var wins over the rig default",
			args:    []string{"mol", "wisp", "mol-a", "--var", "lint_command=custom"},
			rigVars: map[string]string{"lint_command": "make lint", "test_command": "make test"},
			want: []string{
				"mol", "wisp", "mol-a", "--var", "lint_command=custom",
				"--var", "test_command=make test",
			},
		},
		{
			name:    "non-pour args are untouched",
			args:    []string{"list", "--json"},
			rigVars: map[string]string{"lint_command": "make lint"},
			want:    []string{"list", "--json"},
		},
		{
			name:    "wisp list is untouched",
			args:    []string{"mol", "wisp", "list"},
			rigVars: map[string]string{"lint_command": "make lint"},
			want:    []string{"mol", "wisp", "list"},
		},
		{
			name:    "injected flags land before a bare terminator",
			args:    []string{"mol", "wisp", "mol-a", "--", "trailing"},
			rigVars: map[string]string{"lint_command": "make lint"},
			want:    []string{"mol", "wisp", "mol-a", "--var", "lint_command=make lint", "--", "trailing"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := injectRigFormulaVars(tt.args, tt.rigVars)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("injectRigFormulaVars(%v, %v)\n got = %v\nwant = %v", tt.args, tt.rigVars, got, tt.want)
			}
		})
	}
}

func TestInjectRigFormulaVarsDoesNotAliasInput(t *testing.T) {
	args := []string{"mol", "wisp", "mol-a"}
	got := injectRigFormulaVars(args, map[string]string{"lint_command": "make lint"})
	if len(args) != 3 {
		t.Fatalf("input args mutated: %v", args)
	}
	if len(got) != 5 {
		t.Fatalf("got = %v, want 5 elements", got)
	}
}

func TestBdFormulaShowName(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		want   string
		wantOK bool
	}{
		{name: "plain", args: []string{"formula", "show", "mol-refinery-patrol"}, want: "mol-refinery-patrol", wantOK: true},
		{name: "with json flag first", args: []string{"formula", "show", "--json", "mol-a"}, want: "mol-a", wantOK: true},
		{name: "with json flag last", args: []string{"formula", "show", "mol-a", "--json"}, want: "mol-a", wantOK: true},
		{name: "with a global value flag", args: []string{"formula", "show", "-C", "/tmp", "mol-a"}, want: "mol-a", wantOK: true},
		{name: "formula list is not show", args: []string{"formula", "list"}, wantOK: false},
		{name: "no name", args: []string{"formula", "show", "--json"}, wantOK: false},
		{name: "unknown flag is ambiguous", args: []string{"formula", "show", "--mystery", "v", "mol-a"}, wantOK: false},
		{name: "empty", args: nil, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotOK := bdFormulaShowName(tt.args)
			if gotOK != tt.wantOK {
				t.Fatalf("bdFormulaShowName(%v) ok = %v, want %v", tt.args, gotOK, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("bdFormulaShowName(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestSubstituteFormulaShowOutputText(t *testing.T) {
	out := []byte("Lint: {{lint_command}}\nTest: {{test_command}}\nUnknown: {{nope}}\n")
	vars := map[string]string{"lint_command": "make lint", "test_command": ""}
	got := string(substituteFormulaShowOutput(out, vars, false))
	want := "Lint: make lint\nTest: \nUnknown: {{nope}}\n"
	if got != want {
		t.Errorf("substituteFormulaShowOutput text\n got = %q\nwant = %q", got, want)
	}
}

func TestSubstituteFormulaShowOutputJSONStaysValid(t *testing.T) {
	src := map[string]any{
		"description": "run {{lint_command}} then {{test_command}}",
		"unresolved":  "{{nope}}",
	}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	vars := map[string]string{
		// A value carrying JSON metacharacters must not corrupt the document.
		"lint_command": `sh -c "echo \ hi"`,
		"test_command": "go test ./...",
	}
	got := substituteFormulaShowOutput(raw, vars, true)

	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("substituted JSON is invalid: %v\noutput: %s", err, got)
	}
	wantDesc := `run sh -c "echo \ hi" then go test ./...`
	if decoded["description"] != wantDesc {
		t.Errorf("description = %q, want %q", decoded["description"], wantDesc)
	}
	if decoded["unresolved"] != "{{nope}}" {
		t.Errorf("unresolved = %q, want the placeholder left intact", decoded["unresolved"])
	}
}

func TestSubstituteFormulaShowOutputNoVarsIsIdentity(t *testing.T) {
	out := []byte("Lint: {{lint_command}}\n")
	if got := substituteFormulaShowOutput(out, nil, false); string(got) != string(out) {
		t.Errorf("substituteFormulaShowOutput with no vars = %q, want %q", got, out)
	}
}

func TestBdArgsRequestJSON(t *testing.T) {
	if !bdArgsRequestJSON([]string{"formula", "show", "mol-a", "--json"}) {
		t.Error("bdArgsRequestJSON(--json) = false, want true")
	}
	if bdArgsRequestJSON([]string{"formula", "show", "mol-a"}) {
		t.Error("bdArgsRequestJSON(no flag) = true, want false")
	}
	if bdArgsRequestJSON([]string{"formula", "show", "--", "--json"}) {
		t.Error("bdArgsRequestJSON(after terminator) = true, want false")
	}
}

func TestSubstituteFormulaShowOutputRefineryPlaceholders(t *testing.T) {
	// Regression guard for the reported defect: the rig's configured lint and
	// test commands must reach the text a root-only wisp's agent reads.
	out := []byte("Run `{{lint_command}}`; skip when empty.\nApproval gate: {{require_merge_approval}}\n")
	vars := map[string]string{
		"lint_command":           "golangci-lint run ./...",
		"require_merge_approval": "true",
	}
	got := string(substituteFormulaShowOutput(out, vars, false))
	if strings.Contains(got, "{{") {
		t.Errorf("placeholders survived substitution: %q", got)
	}
	if !strings.Contains(got, "golangci-lint run ./...") || !strings.Contains(got, "Approval gate: true") {
		t.Errorf("substitution did not land: %q", got)
	}
}

// A formula's own [vars.*].default must never be rendered into `formula show`
// output. mol-refinery-patrol declares target_branch = "main" while the rig
// that pours it merges to edge-integration and passes the real value as a
// pour-time --var, so substituting the default would hand the agent a
// plausible but wrong merge target in place of a visibly unresolved token.
func TestSubstituteFormulaShowOutputLeavesUnconfiguredPlaceholders(t *testing.T) {
	out := []byte("Merge into {{target_branch}} after {{lint_command}}.\n")
	rigVars := map[string]string{"lint_command": "make lint"}
	got := string(substituteFormulaShowOutput(out, rigVars, false))
	want := "Merge into {{target_branch}} after make lint.\n"
	if got != want {
		t.Errorf("substituteFormulaShowOutput\n got = %q\nwant = %q", got, want)
	}
	if strings.Contains(got, "main") {
		t.Errorf("a formula default leaked into the rendered output: %q", got)
	}
}

func TestSubstituteFormulaShowOutputPreservesVariableListing(t *testing.T) {
	// bd's text output labels each Variables entry with the variable's own
	// placeholder. Rewriting that label would destroy the inventory an
	// operator reads the section for.
	out := []byte("📝 Variables:\n" +
		`   {{lint_command}}: Lint command (e.g., eslint .). Empty = skip. [default=""]` + "\n" +
		"\n🌲 Steps (1):\n   run {{lint_command}} before merging\n")
	vars := map[string]string{"lint_command": "make lint"}
	got := string(substituteFormulaShowOutput(out, vars, false))

	if !strings.Contains(got, `   {{lint_command}}: Lint command`) {
		t.Errorf("variable-listing label was rewritten:\n%s", got)
	}
	if !strings.Contains(got, "run make lint before merging") {
		t.Errorf("step text was not substituted:\n%s", got)
	}
}

func TestSubstituteFormulaShowOutputSubstitutesAfterAListingLabel(t *testing.T) {
	// Only the label itself is protected; the description that follows on the
	// same line still renders.
	out := []byte("   {{setup_command}}: defaults to {{lint_command}}\n")
	vars := map[string]string{"setup_command": "go mod download", "lint_command": "make lint"}
	got := string(substituteFormulaShowOutput(out, vars, false))
	want := "   {{setup_command}}: defaults to make lint\n"
	if got != want {
		t.Errorf("got = %q, want %q", got, want)
	}
}

func TestSubstituteFormulaShowOutputPreservesTrailingNewlines(t *testing.T) {
	out := []byte("a {{v}}\n\nb\n")
	got := string(substituteFormulaShowOutput(out, map[string]string{"v": "1"}, false))
	if got != "a 1\n\nb\n" {
		t.Errorf("line structure not preserved: %q", got)
	}
}

func TestRigFormulaVarsForBdTarget(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "gascity", FormulaVars: map[string]string{"lint_command": "make lint"}},
		{Name: "winnow"},
	}}

	tests := []struct {
		name   string
		cfg    *config.City
		target execStoreTarget
		want   map[string]string
	}{
		{
			name:   "rig with formula vars",
			cfg:    cfg,
			target: execStoreTarget{RigName: "gascity"},
			want:   map[string]string{"lint_command": "make lint"},
		},
		{name: "rig without formula vars", cfg: cfg, target: execStoreTarget{RigName: "winnow"}},
		{name: "unknown rig", cfg: cfg, target: execStoreTarget{RigName: "nope"}},
		{name: "city scope has no rig vars", cfg: cfg, target: execStoreTarget{}},
		{name: "blank rig name", cfg: cfg, target: execStoreTarget{RigName: "   "}},
		{name: "nil config", target: execStoreTarget{RigName: "gascity"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rigFormulaVarsForBdTarget(tt.cfg, tt.target)
			if len(got) != len(tt.want) {
				t.Fatalf("rigFormulaVarsForBdTarget = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("rigFormulaVarsForBdTarget[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestRigFormulaVarsForBdTargetDoesNotAliasConfig(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "gascity", FormulaVars: map[string]string{"lint_command": "make lint"}},
	}}
	got := rigFormulaVarsForBdTarget(cfg, execStoreTarget{RigName: "gascity"})
	got["lint_command"] = "mutated"
	if cfg.Rigs[0].FormulaVars["lint_command"] != "make lint" {
		t.Errorf("mutation leaked into config: %v", cfg.Rigs[0].FormulaVars)
	}
}
