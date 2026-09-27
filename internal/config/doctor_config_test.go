package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestParseDoctorSection(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[doctor]
worktree_volume_warn_free = "80GB"
worktree_volume_error_free = "30GB"
nested_worktree_prune = true

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Doctor.WorktreeVolumeWarnFree != "80GB" {
		t.Errorf("WorktreeVolumeWarnFree = %q, want %q", cfg.Doctor.WorktreeVolumeWarnFree, "80GB")
	}
	if cfg.Doctor.WorktreeVolumeErrorFree != "30GB" {
		t.Errorf("WorktreeVolumeErrorFree = %q, want %q", cfg.Doctor.WorktreeVolumeErrorFree, "30GB")
	}
	if !cfg.Doctor.NestedWorktreePrune {
		t.Error("NestedWorktreePrune = false, want true")
	}
}

func TestParseDoctorLocalChecks(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[doctor]
worktree_volume_warn_free = "80GB"

[[doctor.check]]
name = "gopath-symlink"
script = "./scripts/check-gopath.sh"
description = "Verify GOPATH symlink"

[[doctor.check]]
name = "custom-env"
script = "./scripts/check-env.sh"
fix = "./scripts/fix-env.sh"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := len(cfg.Doctor.Checks); got != 2 {
		t.Fatalf("len(Doctor.Checks) = %d, want 2", got)
	}

	first := cfg.Doctor.Checks[0]
	if first.Name != "gopath-symlink" {
		t.Errorf("Checks[0].Name = %q, want %q", first.Name, "gopath-symlink")
	}
	if first.Script != "./scripts/check-gopath.sh" {
		t.Errorf("Checks[0].Script = %q, want %q", first.Script, "./scripts/check-gopath.sh")
	}
	if first.Description != "Verify GOPATH symlink" {
		t.Errorf("Checks[0].Description = %q, want %q", first.Description, "Verify GOPATH symlink")
	}
	if first.Fix != "" {
		t.Errorf("Checks[0].Fix = %q, want empty", first.Fix)
	}

	second := cfg.Doctor.Checks[1]
	if second.Name != "custom-env" {
		t.Errorf("Checks[1].Name = %q, want %q", second.Name, "custom-env")
	}
	if second.Script != "./scripts/check-env.sh" {
		t.Errorf("Checks[1].Script = %q, want %q", second.Script, "./scripts/check-env.sh")
	}
	if second.Description != "" {
		t.Errorf("Checks[1].Description = %q, want empty", second.Description)
	}
	if second.Fix != "./scripts/fix-env.sh" {
		t.Errorf("Checks[1].Fix = %q, want %q", second.Fix, "./scripts/fix-env.sh")
	}
}

func TestParseNoDoctorSection(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[[agent]]
name = "mayor"
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Doctor.WorktreeVolumeWarnFree != "" || cfg.Doctor.WorktreeVolumeErrorFree != "" {
		t.Errorf("Doctor section should be zero-valued; got %+v", cfg.Doctor)
	}
	if cfg.Doctor.NestedWorktreePrune {
		t.Error("NestedWorktreePrune defaults to true; want false")
	}

	// Unset Doctor must still return real defaults via accessor methods.
	if got := cfg.Doctor.WorktreeVolumeWarnFreeBytes(); got != defaultWorktreeVolumeWarnFreeBytes {
		t.Errorf("WorktreeVolumeWarnFreeBytes() = %d, want %d", got, defaultWorktreeVolumeWarnFreeBytes)
	}
	if got := cfg.Doctor.WorktreeVolumeErrorFreeBytes(); got != defaultWorktreeVolumeErrorFreeBytes {
		t.Errorf("WorktreeVolumeErrorFreeBytes() = %d, want %d", got, defaultWorktreeVolumeErrorFreeBytes)
	}
}

func TestMarshalOmitsEmptyDoctorSection(t *testing.T) {
	c := DefaultCity("test")
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "[doctor]") {
		t.Errorf("Marshal output should not contain '[doctor]' when empty:\n%s", data)
	}
}

func TestDoctorConfigWorktreeVolumeFreeAccessors(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)
	tests := []struct {
		name      string
		cfg       DoctorConfig
		wantWarn  int64
		wantError int64
	}{
		{
			name:      "empty falls back to defaults",
			cfg:       DoctorConfig{},
			wantWarn:  50 * gb,
			wantError: 20 * gb,
		},
		{
			name:      "explicit GB values",
			cfg:       DoctorConfig{WorktreeVolumeWarnFree: "80GB", WorktreeVolumeErrorFree: "30GB"},
			wantWarn:  80 * gb,
			wantError: 30 * gb,
		},
		{
			name:      "MB units",
			cfg:       DoctorConfig{WorktreeVolumeWarnFree: "2048MB", WorktreeVolumeErrorFree: "512MB"},
			wantWarn:  2048 * 1024 * 1024,
			wantError: 512 * 1024 * 1024,
		},
		{
			name:      "unparseable warn falls back to its default; error still parses",
			cfg:       DoctorConfig{WorktreeVolumeWarnFree: "junk", WorktreeVolumeErrorFree: "10GB"},
			wantWarn:  50 * gb,
			wantError: 10 * gb,
		},
		{
			name:      "zero and negative are treated as unset",
			cfg:       DoctorConfig{WorktreeVolumeWarnFree: "0GB", WorktreeVolumeErrorFree: "-5GB"},
			wantWarn:  50 * gb,
			wantError: 20 * gb,
		},
		{
			name:      "error above warn falls back to both defaults",
			cfg:       DoctorConfig{WorktreeVolumeWarnFree: "10GB", WorktreeVolumeErrorFree: "40GB"},
			wantWarn:  50 * gb,
			wantError: 20 * gb,
		},
		{
			name:      "error equal to warn falls back to both defaults",
			cfg:       DoctorConfig{WorktreeVolumeWarnFree: "30GB", WorktreeVolumeErrorFree: "30GB"},
			wantWarn:  50 * gb,
			wantError: 20 * gb,
		},
		{
			name:      "warn alone below the default error falls back to both defaults",
			cfg:       DoctorConfig{WorktreeVolumeWarnFree: "15GB"},
			wantWarn:  50 * gb,
			wantError: 20 * gb,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.WorktreeVolumeWarnFreeBytes(); got != tt.wantWarn {
				t.Errorf("WorktreeVolumeWarnFreeBytes() = %d, want %d", got, tt.wantWarn)
			}
			if got := tt.cfg.WorktreeVolumeErrorFreeBytes(); got != tt.wantError {
				t.Errorf("WorktreeVolumeErrorFreeBytes() = %d, want %d", got, tt.wantError)
			}
		})
	}
}

// TestRetiredWorktreeRigSizeKeysStillLoad guards the compatibility promise
// on worktree_rig_warn_size / worktree_rig_error_size: no check reads them,
// but unknown keys are fatal, so a city.toml that still sets them must load
// cleanly. Deleting the struct fields turns both assertions red.
func TestRetiredWorktreeRigSizeKeysStillLoad(t *testing.T) {
	data := `
[workspace]
name = "test-city"

[doctor]
worktree_rig_warn_size = "5GB"
worktree_rig_error_size = "30GB"
`
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(data)
	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	for _, w := range prov.Warnings {
		if strings.Contains(w, "worktree_rig_") {
			t.Errorf("retired worktree_rig_* key produced a load warning: %q", w)
		}
	}

	var cfg City
	md, err := toml.Decode(data, &cfg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if fatal := fatalUndecodedWarnings(md, "city.toml"); len(fatal) != 0 {
		t.Errorf("retired worktree_rig_* keys must not be fatal unknown fields: %v", fatal)
	}
}

func TestParseHumanSize(t *testing.T) {
	tests := []struct {
		input  string
		want   int64
		wantOK bool
	}{
		{"", 0, false},
		{"   ", 0, false},
		{"junk", 0, false},
		{"10", 10, true},      // bytes implied
		{"1024B", 1024, true}, // explicit B suffix
		{"1KB", 1024, true},
		{"5 mb", 5 * 1024 * 1024, true}, // case-insensitive, whitespace tolerant
		{"  10gb ", 10 * 1024 * 1024 * 1024, true},
		{"-5GB", -5 * 1024 * 1024 * 1024, true}, // accessor treats negative as unset; parser is permissive
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := parseHumanSize(tt.input)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v (input %q)", ok, tt.wantOK, tt.input)
			}
			if got != tt.want {
				t.Errorf("value = %d, want %d (input %q)", got, tt.want, tt.input)
			}
		})
	}
}
