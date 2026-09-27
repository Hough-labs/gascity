package config

import (
	"strings"
	"testing"
)

func TestValidateSemanticsNoWarnings(t *testing.T) {
	cfg := &City{
		Workspace: Workspace{Provider: "claude"},
		Providers: explicitBuiltins("claude", "codex"),
		Agents: []Agent{
			{Name: "mayor", Provider: "claude"},
			{Name: "worker", Provider: "codex"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got: %v", warnings)
	}
}

func TestValidateSemanticsUnknownAgentProvider(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{Name: "mayor", Provider: "cloude"}, // typo
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "cloude") {
		t.Errorf("warning should mention bad provider: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "mayor") {
		t.Errorf("warning should mention agent: %s", warnings[0])
	}
}

func TestValidateSemanticsCustomProviderOK(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"my-agent": {Command: "my-agent-cli"},
		},
		Agents: []Agent{
			{Name: "worker", Provider: "my-agent"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for custom provider, got: %v", warnings)
	}
}

func TestValidateSemanticsUnknownWorkspaceProvider(t *testing.T) {
	cfg := &City{
		Workspace: Workspace{Provider: "bogus"},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "[workspace]") {
		t.Errorf("warning should mention workspace: %s", warnings[0])
	}
}

func TestValidateSemanticsUnknownAgentDefaultsProvider(t *testing.T) {
	cfg := &City{
		AgentDefaults: AgentDefaults{Provider: "cdoex"},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "[agent_defaults]") {
		t.Errorf("warning should mention agent_defaults: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "cdoex") {
		t.Errorf("warning should mention bad provider: %s", warnings[0])
	}
}

func TestValidateSemanticsAgentDefaultsCustomProviderOK(t *testing.T) {
	cfg := &City{
		AgentDefaults: AgentDefaults{Provider: "local-llm"},
		Providers: map[string]ProviderSpec{
			"local-llm": {Command: "local-llm"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for custom agent default provider, got: %v", warnings)
	}
}

func TestValidateSemanticsStartCommandSkipsProviderCheck(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{Name: "custom", Provider: "nonexistent", StartCommand: "my-binary"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("start_command should skip provider check, got: %v", warnings)
	}
}

func TestValidateSemanticsAgentSessionTransportAllowsTmux(t *testing.T) {
	cfg := &City{
		Providers: explicitBuiltins("claude"),
		Agents: []Agent{
			{Name: "worker", Provider: "claude", Session: "tmux"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings for tmux session transport, got: %v", warnings)
	}
}

func TestValidateSemanticsAgentSessionTransportRejectsUnknown(t *testing.T) {
	cfg := &City{
		Providers: explicitBuiltins("claude"),
		Agents: []Agent{
			{Name: "worker", Provider: "claude", Session: "stdio"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "stdio") || !strings.Contains(warnings[0], "tmux") {
		t.Fatalf("warning should mention bad value and allowed transports: %s", warnings[0])
	}
}

func TestValidateSemanticsProviderPromptModeBad(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"bad": {PromptMode: "pipe"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "pipe") {
		t.Errorf("warning should mention bad value: %s", warnings[0])
	}
}

func TestValidateSemanticsProviderPromptFlagRequired(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"needsflag": {PromptMode: "flag"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "prompt_flag") {
		t.Errorf("warning should mention prompt_flag: %s", warnings[0])
	}
}

func TestValidateSemanticsProviderPromptFlagOK(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"ok": {PromptMode: "flag", PromptFlag: "--prompt"},
		},
	}
	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got: %v", warnings)
	}
}

func TestValidateSemanticsMultipleIssues(t *testing.T) {
	cfg := &City{
		Workspace: Workspace{Provider: "nope"},
		Providers: map[string]ProviderSpec{
			"bad": {PromptMode: "pipe"},
		},
		Agents: []Agent{
			{Name: "a1", Provider: "missing1"},
			{Name: "a2", Provider: "missing2"},
		},
	}
	warnings := ValidateSemantics(cfg, "test.toml")
	// 1 workspace + 2 agents + 1 provider = 4
	if len(warnings) != 4 {
		t.Fatalf("expected 4 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestValidateSemanticsIncludesSource(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{Name: "bad", Provider: "missing"},
		},
	}
	warnings := ValidateSemantics(cfg, "/path/to/city.toml")
	if len(warnings) == 0 {
		t.Fatal("expected warning")
	}
	if !strings.Contains(warnings[0], "/path/to/city.toml") {
		t.Errorf("warning should include source path: %s", warnings[0])
	}
}

func TestValidateAgentsScopeBadEnum(t *testing.T) {
	agents := []Agent{
		{Name: "bad", Scope: "global"},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for bad scope")
	}
	if !strings.Contains(err.Error(), "global") {
		t.Errorf("error should mention bad value: %v", err)
	}
}

func TestValidateAgentsScopeValidValues(t *testing.T) {
	for _, scope := range []string{"", "city", "rig"} {
		agents := []Agent{
			{Name: "ok", Scope: scope},
		}
		if err := ValidateAgents(agents); err != nil {
			t.Errorf("scope %q should be valid, got: %v", scope, err)
		}
	}
}

func TestValidateAgentsPromptModeBadEnum(t *testing.T) {
	agents := []Agent{
		{Name: "bad", PromptMode: "pipe"},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for bad prompt_mode")
	}
	if !strings.Contains(err.Error(), "pipe") {
		t.Errorf("error should mention bad value: %v", err)
	}
}

func TestValidateAgentsPromptModeValidValues(t *testing.T) {
	for _, mode := range []string{"", "arg", "flag", "none"} {
		agents := []Agent{
			{Name: "ok", PromptMode: mode, PromptFlag: "--p"},
		}
		if err := ValidateAgents(agents); err != nil {
			t.Errorf("prompt_mode %q should be valid, got: %v", mode, err)
		}
	}
}

func TestValidateAgentsLifecycleValues(t *testing.T) {
	for _, lifecycle := range []string{"", AgentLifecycleOneShot} {
		if err := ValidateAgents([]Agent{{Name: "ok", Lifecycle: lifecycle}}); err != nil {
			t.Errorf("lifecycle %q should be valid, got: %v", lifecycle, err)
		}
	}
	err := ValidateAgents([]Agent{{Name: "bad", Lifecycle: "short_lived"}})
	if err == nil {
		t.Fatal("expected error for bad lifecycle")
	}
	if !strings.Contains(err.Error(), "short_lived") {
		t.Errorf("error should mention bad value: %v", err)
	}
}

func TestValidateAgentsPromptFlagRequiredForFlagMode(t *testing.T) {
	agents := []Agent{
		{Name: "bad", PromptMode: "flag"},
	}
	err := ValidateAgents(agents)
	if err == nil {
		t.Fatal("expected error for missing prompt_flag")
	}
	if !strings.Contains(err.Error(), "prompt_flag") {
		t.Errorf("error should mention prompt_flag: %v", err)
	}
}

func TestValidateAgentsPromptFlagWithFlagModeOK(t *testing.T) {
	agents := []Agent{
		{Name: "ok", PromptMode: "flag", PromptFlag: "--prompt"},
	}
	if err := ValidateAgents(agents); err != nil {
		t.Errorf("should be valid: %v", err)
	}
}

// TestIsNonFatalConfigWarning pins the one classification strict mode and
// gc doctor share: one warning from each non-fatal class passes, and warnings
// that signal a real misconfiguration do not. Samples come from the producers
// wherever a producer is reachable, so a wording change cannot leave the
// classifier matching a string config no longer emits.
func TestIsNonFatalConfigWarning(t *testing.T) {
	semantic := func(t *testing.T, a Agent) string {
		t.Helper()
		warnings := ValidateSemantics(&City{Agents: []Agent{a}}, "city.toml")
		if len(warnings) != 1 {
			t.Fatalf("ValidateSemantics(%+v) = %v, want exactly one warning", a, warnings)
		}
		return warnings[0]
	}
	alwaysFresh := func(t *testing.T) string {
		t.Helper()
		warnings, err := ValidateNamedSessions(&City{
			Workspace:     Workspace{Name: "test-city"},
			Agents:        []Agent{{Name: "watchdog", WakeMode: "fresh"}},
			NamedSessions: []NamedSession{{Template: "watchdog", Mode: "always"}},
		})
		if err != nil || len(warnings) != 1 {
			t.Fatalf("ValidateNamedSessions = %v, %v; want exactly the always+fresh advisory", warnings, err)
		}
		return warnings[0]
	}

	tests := []struct {
		name    string
		warning func(t *testing.T) string
		want    bool
	}{
		{"site binding", func(*testing.T) string { return legacyRigPathSiteBindingWarning("repo") }, true},
		{"legacy v1 surface", func(*testing.T) string {
			return "city.toml: [packs] is deprecated in v2; use [imports] + packs.lock."
		}, true},
		{"legacy workspace field", func(*testing.T) string {
			return "city.toml: " + legacyWorkspaceFieldMarker("start_command") + ": Use per-agent `start_command` in `agent.toml` instead."
		}, true},
		{"idle sleep masked by idle timeout", func(t *testing.T) string {
			return semantic(t, Agent{Name: "refinery", IdleTimeout: "2h", SleepAfterIdle: "300s"})
		}, true},
		{"always-mode named session on fresh wake", alwaysFresh, true},
		{"retired key", func(*testing.T) string {
			return retiredKeyWarning("city.toml", "daemon.graph_workflows", retiredKey{RemovedIn: "v1.2.0"})
		}, true},
		{"unknown field", func(*testing.T) string { return unknownFieldWarning("city.toml", "bogus_key", nil) }, false},
		{"invalid session transport", func(t *testing.T) string {
			return semantic(t, Agent{Name: "worker", Session: "bogus"})
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := tt.warning(t)
			if got := IsNonFatalConfigWarning(w); got != tt.want {
				t.Fatalf("IsNonFatalConfigWarning(%q) = %v, want %v", w, got, tt.want)
			}
		})
	}
}
