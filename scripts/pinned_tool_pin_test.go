package scripts_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pinnedToolSpec describes one version-pinned Go tool installed through
// scripts/install-pinned-go-tool.
//
// The install contract is identical for every such tool, so these tests are
// driven from a spec table rather than duplicated per tool: each tool growing
// its own guard is precisely how the pins drifted in the first place
// (gascity-430j for golangci-lint, gascity-1ra for gofumpt).
type pinnedToolSpec struct {
	// tool is the binary name written into BIN_DIR.
	tool string
	// target is the phony make target that installs it. It replaces the former
	// file-existence targets, whose recipes make ran only when the binary was
	// ABSENT -- so a wrong-version binary already sitting in the shared GOPATH
	// bin shadowed the pin forever and `make install-tools` could not correct
	// it.
	target string
	// makefileVar is the Makefile variable holding the pinned version.
	makefileVar string
	// versionArgs is the argument list the binary reports its version to.
	versionArgs []string
	// versionFormat renders a version string the way the real binary prints
	// it, with a single %s for the version. It shapes both the stub seeded
	// into BIN_DIR and the stub `go install` output, so the parsing the
	// installer does is exercised against realistic text rather than a
	// convenient one.
	versionFormat string
}

// pinnedTools is the set of tools wired to the shared installer. Adding a tool
// to the Makefile without adding it here leaves its pin unguarded.
func pinnedTools() []pinnedToolSpec {
	return []pinnedToolSpec{
		{
			tool:          "golangci-lint",
			target:        "install-golangci-lint",
			makefileVar:   "GOLANGCI_LINT_VERSION",
			versionArgs:   []string{"version"},
			versionFormat: `golangci-lint has version %s built with go1.26.5 from (unknown, modified: ?, mod sum: "") on (unknown)`,
		},
		{
			// gofumpt reports "v0.9.2 (go1.26.5)" -- a leading v the pin does
			// not carry, which the installer strips.
			tool:          "gofumpt",
			target:        "install-gofumpt",
			makefileVar:   "GOFUMPT_VERSION",
			versionArgs:   []string{"--version"},
			versionFormat: "v%s (go1.26.5)",
		},
	}
}

// pinnedVersion reads a pin from the Makefile rather than hardcoding it, so
// promoting a version does not require editing this test.
func pinnedVersion(t *testing.T, makefileVar string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(makefileVar) + ` := (\S+)$`).FindStringSubmatch(readMakefile(t))
	if m == nil {
		t.Fatalf("Makefile no longer defines %s := <version>", makefileVar)
	}
	return m[1]
}

// pinnedToolFixture runs the real Makefile target against a throwaway BIN_DIR
// with a stub `go` on PATH, so the install path is exercised end to end without
// a network round-trip and without touching the developer's GOPATH bin.
type pinnedToolFixture struct {
	t          *testing.T
	spec       pinnedToolSpec
	binDir     string
	shimDir    string
	stateDir   string
	installLog string
	pin        string
}

func newPinnedToolFixture(t *testing.T, spec pinnedToolSpec) *pinnedToolFixture {
	t.Helper()
	tmp := t.TempDir()
	f := &pinnedToolFixture{
		t:          t,
		spec:       spec,
		binDir:     filepath.Join(tmp, "bin"),
		shimDir:    filepath.Join(tmp, "shim"),
		stateDir:   filepath.Join(tmp, "state"),
		installLog: filepath.Join(tmp, "state", "install.log"),
		pin:        pinnedVersion(t, spec.makefileVar),
	}
	for _, dir := range []string{f.binDir, f.shimDir, f.stateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("locate real go: %v", err)
	}
	writeExecutable(t, filepath.Join(f.shimDir, "go"), `#!/usr/bin/env sh
# Stub go: intercepts `+"`go install`"+` and delegates everything else (env, list)
# to the real toolchain, so the Makefile still parses normally.
if [ "$1" != "install" ]; then
	exec `+realGo+` "$@"
fi
shift
printf '%s\n' "$*" >> "$STUB_INSTALL_LOG"
attempts=0
if [ -f "$STUB_STATE_DIR/attempts" ]; then
	attempts=$(cat "$STUB_STATE_DIR/attempts")
fi
attempts=$((attempts + 1))
printf '%s\n' "$attempts" > "$STUB_STATE_DIR/attempts"
if [ "$attempts" -le "${STUB_INSTALL_FAILURES:-0}" ]; then
	echo "stub go install: simulated failure $attempts" >&2
	exit 1
fi
if [ -z "$GOBIN" ]; then
	echo "stub go install: GOBIN unset; the install target must pin the destination" >&2
	exit 1
fi
spec="$1"
module="${spec%@*}"
name="${module##*/}"
mkdir -p "$GOBIN"
dest="$GOBIN/$name"
# Faithful to the real toolchain: go install refuses to write over a
# destination it did not produce ("build output ... already exists and is not
# an object file"), and it is exactly the shadowing binary this install path
# exists to replace that is not one. A stub carries a marker so the shim can
# tell its own previous output (overwritable) from a foreign file (refused).
if [ -e "$dest" ] && ! grep -q "stub-go-object" "$dest" 2>/dev/null; then
	echo "go install $spec: build output \"$dest\" already exists and is not an object file" >&2
	exit 1
fi
version_line=$(printf "$STUB_VERSION_FORMAT" "$STUB_INSTALL_VERSION")
cat > "$dest" <<STUB
#!/usr/bin/env sh
# stub-go-object
echo '$version_line'
STUB
chmod 0755 "$dest"
`)
	return f
}

// seedBinary writes a stub tool into BIN_DIR reporting the given version, in
// the shape the real binary prints it.
func (f *pinnedToolFixture) seedBinary(version string) {
	f.t.Helper()
	writeExecutable(f.t, f.toolPath(), "#!/usr/bin/env sh\necho '"+fmt.Sprintf(f.spec.versionFormat, version)+"'\n")
}

func (f *pinnedToolFixture) toolPath() string {
	return filepath.Join(f.binDir, f.spec.tool)
}

// run invokes the real Makefile target with BIN_DIR redirected at the fixture.
func (f *pinnedToolFixture) run(extraEnv ...string) (string, error) {
	f.t.Helper()
	cmd := makeCommand("--no-print-directory", f.spec.target, "BIN_DIR="+f.binDir)
	cmd.Dir = repoRoot(f.t)
	env := append(
		os.Environ(),
		"PATH="+f.shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_STATE_DIR="+f.stateDir,
		"STUB_INSTALL_LOG="+f.installLog,
		"STUB_INSTALL_VERSION="+f.pin,
		"STUB_VERSION_FORMAT="+f.spec.versionFormat,
	)
	env = append(env, extraEnv...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// installs returns the `go install` invocations the shim recorded.
func (f *pinnedToolFixture) installs() []string {
	f.t.Helper()
	data, err := os.ReadFile(f.installLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatalf("read install log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// assertNoStagingLeftovers checks the scratch GOBIN the installer stages into
// is cleaned up; BIN_DIR is the machine-wide GOPATH bin in real use.
func (f *pinnedToolFixture) assertNoStagingLeftovers() {
	f.t.Helper()
	entries, err := os.ReadDir(f.binDir)
	if err != nil {
		f.t.Fatalf("read BIN_DIR: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".install-pinned-go-tool.") {
			f.t.Fatalf("staging directory left behind in BIN_DIR: %s", entry.Name())
		}
	}
}

// installedVersion probes the binary in BIN_DIR the way the install target
// itself does.
func (f *pinnedToolFixture) installedVersion() string {
	f.t.Helper()
	out, err := testCommand(f.toolPath(), f.spec.versionArgs...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("probe installed %s: %v\n%s", f.spec.tool, err, out)
	}
	m := regexp.MustCompile(`\bv?(\d+\.\d+\.\d+\S*)`).FindStringSubmatch(string(out))
	if m == nil {
		f.t.Fatalf("no version token in %q", out)
	}
	return m[1]
}

// TestInstallPinnedToolReplacesWrongVersionBinary is the regression test for
// gascity-430j and gascity-1ra. The load-bearing assertion is that a binary
// reporting a version OTHER than the pin is REPLACED -- precisely what a
// file-existence target fails to do.
func TestInstallPinnedToolReplacesWrongVersionBinary(t *testing.T) {
	for _, spec := range pinnedTools() {
		t.Run(spec.tool, func(t *testing.T) {
			f := newPinnedToolFixture(t, spec)
			f.seedBinary("1.2.3")

			out, err := f.run()
			if err != nil {
				t.Fatalf("make %s failed: %v\n%s", spec.target, err, out)
			}

			installs := f.installs()
			if len(installs) != 1 {
				t.Fatalf("go install invocations = %d, want 1 (the stale binary must be reinstalled)\nlog: %v\nmake output:\n%s", len(installs), installs, out)
			}
			if !strings.Contains(installs[0], "@v"+f.pin) {
				t.Fatalf("go install %q does not pin @v%s", installs[0], f.pin)
			}
			if got := f.installedVersion(); got != f.pin {
				t.Fatalf("%s in BIN_DIR reports %q, want the pinned %q\nmake output:\n%s", spec.tool, got, f.pin, out)
			}
			f.assertNoStagingLeftovers()
		})
	}
}

// TestInstallPinnedToolSkipsWhenAlreadyPinned guards the other half of the
// contract: the lint and fmt gates take install-golangci-lint as a
// prerequisite, so a binary already at the pin must NOT be reinstalled -- the
// gate cannot start paying a network round-trip per run.
func TestInstallPinnedToolSkipsWhenAlreadyPinned(t *testing.T) {
	for _, spec := range pinnedTools() {
		t.Run(spec.tool, func(t *testing.T) {
			f := newPinnedToolFixture(t, spec)
			f.seedBinary(f.pin)
			before, err := os.ReadFile(f.toolPath())
			if err != nil {
				t.Fatalf("read seeded binary: %v", err)
			}

			out, err := f.run()
			if err != nil {
				t.Fatalf("make %s failed: %v\n%s", spec.target, err, out)
			}

			if installs := f.installs(); len(installs) != 0 {
				t.Fatalf("go install ran %d time(s) for a binary already at the pin: %v\nmake output:\n%s", len(installs), installs, out)
			}
			after, err := os.ReadFile(f.toolPath())
			if err != nil {
				t.Fatalf("read binary after install target: %v", err)
			}
			if string(after) != string(before) {
				t.Fatalf("binary at the pin was rewritten\nmake output:\n%s", out)
			}
		})
	}
}

// TestInstallPinnedToolInstallsWhenAbsentOrUnusable covers the cases that must
// be treated as a mismatch rather than silently accepted.
func TestInstallPinnedToolInstallsWhenAbsentOrUnusable(t *testing.T) {
	tests := []struct {
		name string
		seed func(f *pinnedToolFixture)
	}{
		{
			name: "absent",
			seed: func(*pinnedToolFixture) {},
		},
		{
			name: "not executable",
			seed: func(f *pinnedToolFixture) {
				if err := os.WriteFile(f.toolPath(), []byte("not a binary\n"), 0o644); err != nil {
					f.t.Fatalf("seed non-executable binary: %v", err)
				}
			},
		},
		{
			name: "version probe fails",
			seed: func(f *pinnedToolFixture) {
				writeExecutable(f.t, f.toolPath(), "#!/usr/bin/env sh\nexit 3\n")
			},
		},
		{
			name: "version output unparseable",
			seed: func(f *pinnedToolFixture) {
				writeExecutable(f.t, f.toolPath(), "#!/usr/bin/env sh\necho 'version (devel)'\n")
			},
		},
	}
	for _, spec := range pinnedTools() {
		t.Run(spec.tool, func(t *testing.T) {
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					f := newPinnedToolFixture(t, spec)
					tc.seed(f)

					out, err := f.run()
					if err != nil {
						t.Fatalf("make %s failed: %v\n%s", spec.target, err, out)
					}
					if installs := f.installs(); len(installs) != 1 {
						t.Fatalf("go install invocations = %d, want 1\nlog: %v\nmake output:\n%s", len(installs), installs, out)
					}
					if got := f.installedVersion(); got != f.pin {
						t.Fatalf("%s in BIN_DIR reports %q, want the pinned %q\nmake output:\n%s", spec.tool, got, f.pin, out)
					}
				})
			}
		})
	}
}

// TestInstallPinnedToolRetriesTransientInstallFailures keeps the retry loop the
// original file-existence target already had: a flaky proxy fetch must not fail
// the gate on the first attempt.
func TestInstallPinnedToolRetriesTransientInstallFailures(t *testing.T) {
	for _, spec := range pinnedTools() {
		t.Run(spec.tool, func(t *testing.T) {
			f := newPinnedToolFixture(t, spec)

			out, err := f.run("STUB_INSTALL_FAILURES=2", "PINNED_TOOL_INSTALL_DELAY=0")
			if err != nil {
				t.Fatalf("make %s failed despite retries: %v\n%s", spec.target, err, out)
			}
			if installs := f.installs(); len(installs) != 3 {
				t.Fatalf("go install invocations = %d, want 3 (2 failures then success)\nlog: %v\nmake output:\n%s", len(installs), installs, out)
			}
			if got := f.installedVersion(); got != f.pin {
				t.Fatalf("%s in BIN_DIR reports %q, want the pinned %q", spec.tool, got, f.pin)
			}
		})
	}
}

// TestInstallPinnedToolFailsAfterExhaustingRetries: when every attempt fails
// the gate must fail loudly rather than proceed with whatever is on disk.
func TestInstallPinnedToolFailsAfterExhaustingRetries(t *testing.T) {
	for _, spec := range pinnedTools() {
		t.Run(spec.tool, func(t *testing.T) {
			f := newPinnedToolFixture(t, spec)
			f.seedBinary("1.2.3")

			out, err := f.run("STUB_INSTALL_FAILURES=99", "PINNED_TOOL_INSTALL_DELAY=0")
			if err == nil {
				t.Fatalf("make %s succeeded with every install attempt failing:\n%s", spec.target, out)
			}
			if installs := f.installs(); len(installs) != 5 {
				t.Fatalf("go install invocations = %d, want the documented 5 attempts\nlog: %v\nmake output:\n%s", len(installs), installs, out)
			}
			// A failed install must not leave the developer with no tool at
			// all: the staged binary is moved into place only once it verifies.
			if got := f.installedVersion(); got != "1.2.3" {
				t.Fatalf("binary in BIN_DIR reports %q after a failed install, want the untouched 1.2.3", got)
			}
			f.assertNoStagingLeftovers()
		})
	}
}

// TestInstallPinnedToolFailsWhenInstalledVersionStillWrong: a successful
// install that yields a binary reporting something other than the pin must be
// reported, not accepted. Accepting it would mean either a silently unpinned
// tool (the bug) or a reinstall on every single gate run.
func TestInstallPinnedToolFailsWhenInstalledVersionStillWrong(t *testing.T) {
	for _, spec := range pinnedTools() {
		t.Run(spec.tool, func(t *testing.T) {
			f := newPinnedToolFixture(t, spec)

			out, err := f.run("STUB_INSTALL_VERSION=9.9.9")
			if err == nil {
				t.Fatalf("make %s accepted a binary reporting 9.9.9 instead of %s:\n%s", spec.target, f.pin, out)
			}
			if !strings.Contains(out, f.pin) {
				t.Fatalf("failure message does not name the pinned version %q:\n%s", f.pin, out)
			}
		})
	}
}

// TestInstallToolsInstallsEveryPinnedTool keeps `make install-tools` -- the
// documented bootstrap, and setup's only prerequisite -- covering every pinned
// tool. A tool pinned but left out of it is a pin nobody applies.
func TestInstallToolsInstallsEveryPinnedTool(t *testing.T) {
	makefile := readMakefile(t)
	m := regexp.MustCompile(`(?m)^install-tools:([^\n]*)$`).FindStringSubmatch(makefile)
	if m == nil {
		t.Fatal("Makefile no longer defines target \"install-tools\"")
	}
	for _, spec := range pinnedTools() {
		if !strings.Contains(m[1], spec.target) {
			t.Errorf("install-tools prerequisites are %q, want %q among them", strings.TrimSpace(m[1]), spec.target)
		}
	}
}

// TestLintGatesDependOnVersionCheckedInstall pins the wiring: every gate that
// runs golangci-lint must take the version-checked target as a prerequisite,
// and the file-existence target that caused gascity-430j must not come back.
func TestLintGatesDependOnVersionCheckedInstall(t *testing.T) {
	const target = "install-golangci-lint"
	makefile := readMakefile(t)

	for _, gate := range []string{
		"lint-full", "lint-new", "lint-changed", "lint-affected",
		"fmt", "fmt-check", "fmt-check-changed", "install-tools",
	} {
		re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(gate) + `:([^\n]*)$`)
		m := re.FindStringSubmatch(makefile)
		if m == nil {
			t.Fatalf("Makefile no longer defines target %q", gate)
		}
		if !strings.Contains(m[1], target) {
			t.Fatalf("target %q prerequisites are %q, want the version-checked %q", gate, strings.TrimSpace(m[1]), target)
		}
	}

	if regexp.MustCompile(`(?m)^\$\(GOLANGCI_LINT\):`).MatchString(makefile) {
		t.Fatal("`$(GOLANGCI_LINT):` is back as a file-existence target; make would run its recipe only when the binary is absent, so a wrong-version binary shadows the pin again (gascity-430j)")
	}
}

// TestMakefileInvokesPinnedToolsByAbsolutePath is the guard for the trap that
// makes this whole pin inert: installing into BIN_DIR does NOT change what a
// bare tool name resolves to. On a developer machine /opt/homebrew/bin
// precedes $(go env GOPATH)/bin, so a recipe running a bare `gofumpt` gets
// Homebrew's copy -- 0.10.0 here, a full minor ahead of the v0.9.2 that
// golangci-lint vendors -- and the reformat the pin exists to prevent happens
// anyway, invisibly to `make fmt-check` (gascity-1ra).
//
// The repo cannot manage machine PATH order, so the rule is that recipes name
// the pinned binary by its absolute path, exactly as $(GOLANGCI_LINT) already
// does.
func TestMakefileInvokesPinnedToolsByAbsolutePath(t *testing.T) {
	makefile := readMakefile(t)

	for _, spec := range pinnedTools() {
		pathVar := strings.ToUpper(strings.ReplaceAll(spec.tool, "-", "_"))
		re := regexp.MustCompile(`(?m)^` + pathVar + ` := \$\(BIN_DIR\)/` + regexp.QuoteMeta(spec.tool) + `$`)
		if !re.MatchString(makefile) {
			t.Errorf("Makefile does not define %s := $(BIN_DIR)/%s; recipes have no absolute-path handle for the pinned binary", pathVar, spec.tool)
		}
	}

	// Recipe lines (tab-indented) may name a pinned tool only through its
	// $(VAR), never bare. The install recipes are the one exception: they pass
	// the tool name and module path to the installer as arguments rather than
	// executing them.
	installRecipes := map[string]bool{}
	for _, spec := range pinnedTools() {
		installRecipes[spec.target] = true
	}
	currentTarget := ""
	for i, line := range strings.Split(makefile, "\n") {
		if m := regexp.MustCompile(`^([A-Za-z0-9_.-]+):`).FindStringSubmatch(line); m != nil {
			currentTarget = m[1]
			continue
		}
		if !strings.HasPrefix(line, "\t") || installRecipes[currentTarget] {
			continue
		}
		for _, spec := range pinnedTools() {
			if strings.Contains(line, spec.tool) {
				t.Errorf("Makefile:%d (target %q) names %q bare; PATH decides which binary that is, so use the $(BIN_DIR)-anchored variable instead:\n\t%s",
					i+1, currentTarget, spec.tool, strings.TrimSpace(line))
			}
		}
	}
}
