package doctor

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// The stock gastown refinery sets both idle_timeout and sleep_after_idle, and
// strict mode already classifies the resulting warning as non-fatal. Doctor
// reported it at the default blocking severity, so every patrol that acts on
// blocking findings tripped on a supported configuration.
func TestConfigSemanticsCheck_Severity(t *testing.T) {
	idleSleepAgent := config.Agent{Name: "refinery", IdleTimeout: "2h", SleepAfterIdle: "300s"}
	tests := []struct {
		name         string
		agents       []config.Agent
		wantSeverity CheckSeverity
		wantDetails  int
	}{
		{
			name:         "only non-fatal warnings are advisory",
			agents:       []config.Agent{idleSleepAgent},
			wantSeverity: SeverityAdvisory,
			wantDetails:  1,
		},
		{
			name:         "one fatal-class warning keeps the result blocking",
			agents:       []config.Agent{idleSleepAgent, {Name: "worker", Session: "bogus"}},
			wantSeverity: SeverityBlocking,
			wantDetails:  2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test"},
				Agents:    tt.agents,
			}
			r := NewConfigSemanticsCheck(cfg, "city.toml").Run(&CheckContext{})
			if r.Status != StatusWarning {
				t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
			}
			if r.Severity != tt.wantSeverity {
				t.Fatalf("severity = %d, want %d; details = %v", r.Severity, tt.wantSeverity, r.Details)
			}
			if len(r.Details) != tt.wantDetails {
				t.Fatalf("details = %v, want %d warning(s) listed", r.Details, tt.wantDetails)
			}
		})
	}
}
