package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// golangciRunConfig is the subset of .golangci.yml these tests assert on.
// The field tag mirrors golangci-lint's own mapstructure key
// (pkg/config/run.go:29 in v2.12.0, the version Makefile:1 pins) and the
// "run" object of jsonschema/golangci.v2.5.jsonschema.json, whose
// additionalProperties is false -- so a misspelled key is a hard config error
// rather than a silently ignored one.
type golangciRunConfig struct {
	Run struct {
		AllowParallelRunners bool `yaml:"allow-parallel-runners"`
	} `yaml:"run"`
}

// TestGolangciLintAllowsParallelRunners guards gascity-ipk5: the lint gates
// must not serialize every worktree on this machine behind one lock file.
//
// golangci-lint's runCommand.preRunE calls acquireFileLock
// (pkg/commands/run.go:216), which locks
// filepath.Join(os.TempDir(), "golangci-lint.lock") -- a path derived from
// $TMPDIR alone. It is independent of GOLANGCI_LINT_CACHE, of the module, and
// of the working directory, so every worktree and every agent on the box
// contends on the same file. Worse, the wait is bounded at 5s and a run that
// cannot take the lock in time returns "parallel golangci-lint is running"
// (exit 3). On a machine running several agents that makes `make lint`
// frequently *unrunnable* rather than merely slow, which is a gate failure
// indistinguishable from a real lint failure to every caller of it.
//
// Setting run.allow-parallel-runners short-circuits acquireFileLock before it
// ever touches the lock file, so no gate entry point serializes on it. The
// config file is the right home for the fix rather than a flag on one recipe:
// the lock is taken by every `golangci-lint run` invocation, which means
// lint-full, lint-new, lint-changed, lint-affected and the
// .githooks/pre-commit hook all inherit it from here.
//
// The alternative, run.allow-serial-runners, is deliberately NOT the fix. It
// keeps the lock and only removes the 5s timeout, converting "gate refused"
// into "gate waits indefinitely" -- trading an unrunnable gate for an
// unbounded one on an already-saturated box.
//
// There is deliberately no behavioral test that proves the refusal is gone by
// holding the real lock: the lock is machine-global, so a test that took it
// would block every other agent's lint run on this box for its duration --
// causing the very outage this bead fixes.
func TestGolangciLintAllowsParallelRunners(t *testing.T) {
	cfg := readGolangciRunConfig(t)

	if !cfg.Run.AllowParallelRunners {
		t.Fatal("run.allow-parallel-runners is not enabled in .golangci.yml; every lint gate on this machine serializes on $TMPDIR/golangci-lint.lock and is refused outright after 5s (gascity-ipk5)")
	}
}

// TestGolangciLintParallelRunnersRequireWorktreeScopedCache guards the
// precondition that makes the test above safe.
//
// The single-instance lock exists to stop concurrent runs from corrupting a
// *shared* cache. Allowing parallel runners is only correct because
// gascity-p1l scoped GOLANGCI_LINT_CACHE to $(CURDIR), so concurrent runs no
// longer share one. Enabling parallel runners while the cache is
// machine-global would be a regression, not a fix: it would make p1l's
// phantom cross-worktree findings MORE frequent by adding concurrent writers
// to the single cache they are replayed from.
//
// That coupling is invisible in either file on its own, so assert it here.
// The two settings must move together or not at all.
func TestGolangciLintParallelRunnersRequireWorktreeScopedCache(t *testing.T) {
	cfg := readGolangciRunConfig(t)
	if !cfg.Run.AllowParallelRunners {
		t.Skip("parallel runners disabled; the shared-cache hazard this guards does not apply")
	}

	repo := repoRoot(t)
	cache := runMakefileGolangciLintCachePrintTarget(t, nil)
	if cache == "" {
		t.Fatal("run.allow-parallel-runners is enabled but GOLANGCI_LINT_CACHE is unset in the recipe environment; concurrent runs would share the per-user cache every worktree on this machine falls back to")
	}
	if !strings.HasPrefix(cache, repo+string(filepath.Separator)) {
		t.Fatalf("run.allow-parallel-runners is enabled but GOLANGCI_LINT_CACHE = %q is outside this worktree (%q); concurrent runs would write one shared cache and replay each other's findings", cache, repo)
	}
}

func readGolangciRunConfig(t *testing.T) golangciRunConfig {
	t.Helper()
	path := filepath.Join(repoRoot(t), ".golangci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg golangciRunConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return cfg
}
