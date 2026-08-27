package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The install target under test. It replaces the former file-existence target
// `$(BIN_DIR)/golangci-lint:`, whose recipe make ran only when the binary was
// ABSENT -- so a wrong-version binary already sitting in the shared GOPATH bin
// shadowed GOLANGCI_LINT_VERSION forever and `make install-tools` could not
// correct it (gascity-430j: a dirty 2.0.0 build shadowed the 2.12.0 pin on this
// host for ~3 months, silently invalidating every lint gate run against it).
const golangciLintInstallTarget = "install-golangci-lint"

// golangciLintPinnedVersion reads the pin from the Makefile rather than
// hardcoding it, so promoting GOLANGCI_LINT_VERSION does not require editing
// this test. CI keys its lint cache off the same line.
func golangciLintPinnedVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	m := regexp.MustCompile(`(?m)^GOLANGCI_LINT_VERSION := (\S+)$`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal("Makefile no longer defines GOLANGCI_LINT_VERSION := <version>")
	}
	return m[1]
}

// pinnedToolFixture runs the real Makefile target against a throwaway BIN_DIR
// with a stub `go` on PATH, so the install path is exercised end to end without
// a network round-trip and without touching the developer's GOPATH bin.
type pinnedToolFixture struct {
	t          *testing.T
	binDir     string
	shimDir    string
	stateDir   string
	installLog string
	pin        string
}

func newPinnedToolFixture(t *testing.T) *pinnedToolFixture {
	t.Helper()
	tmp := t.TempDir()
	f := &pinnedToolFixture{
		t:          t,
		binDir:     filepath.Join(tmp, "bin"),
		shimDir:    filepath.Join(tmp, "shim"),
		stateDir:   filepath.Join(tmp, "state"),
		installLog: filepath.Join(tmp, "state", "install.log"),
		pin:        golangciLintPinnedVersion(t),
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
cat > "$dest" <<STUB
#!/usr/bin/env sh
# stub-go-object
echo "$name has version $STUB_INSTALL_VERSION built with go1.26.5 from (unknown)"
STUB
chmod 0755 "$dest"
`)
	return f
}

// seedBinary writes a stub golangci-lint into BIN_DIR reporting the given
// version, in the exact shape the real binary prints it.
func (f *pinnedToolFixture) seedBinary(version string) {
	f.t.Helper()
	writeExecutable(f.t, f.toolPath(), `#!/usr/bin/env sh
echo "golangci-lint has version `+version+` built with go1.26.5 from (unknown, modified: ?, mod sum: \"\") on (unknown)"
`)
}

func (f *pinnedToolFixture) toolPath() string {
	return filepath.Join(f.binDir, "golangci-lint")
}

// run invokes the real Makefile target with BIN_DIR redirected at the fixture.
func (f *pinnedToolFixture) run(extraEnv ...string) (string, error) {
	f.t.Helper()
	cmd := makeCommand("--no-print-directory", golangciLintInstallTarget, "BIN_DIR="+f.binDir)
	cmd.Dir = repoRoot(f.t)
	env := append(
		os.Environ(),
		"PATH="+f.shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_STATE_DIR="+f.stateDir,
		"STUB_INSTALL_LOG="+f.installLog,
		"STUB_INSTALL_VERSION="+f.pin,
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
	out, err := testCommand(f.toolPath(), "version").CombinedOutput()
	if err != nil {
		f.t.Fatalf("probe installed golangci-lint: %v\n%s", err, out)
	}
	m := regexp.MustCompile(`\bv?(\d+\.\d+\.\d+\S*)`).FindStringSubmatch(string(out))
	if m == nil {
		f.t.Fatalf("no version token in %q", out)
	}
	return m[1]
}

// TestInstallGolangciLintReplacesWrongVersionBinary is the regression test for
// gascity-430j. The load-bearing assertion is that a binary reporting a version
// OTHER than the pin is REPLACED -- precisely what the old file-existence
// target failed to do.
func TestInstallGolangciLintReplacesWrongVersionBinary(t *testing.T) {
	f := newPinnedToolFixture(t)
	f.seedBinary("2.0.0-20260506110125-c0d3ddc9cf3f+dirty")

	out, err := f.run()
	if err != nil {
		t.Fatalf("make %s failed: %v\n%s", golangciLintInstallTarget, err, out)
	}

	installs := f.installs()
	if len(installs) != 1 {
		t.Fatalf("go install invocations = %d, want 1 (the stale binary must be reinstalled)\nlog: %v\nmake output:\n%s", len(installs), installs, out)
	}
	if !strings.Contains(installs[0], "@v"+f.pin) {
		t.Fatalf("go install %q does not pin @v%s", installs[0], f.pin)
	}
	if got := f.installedVersion(); got != f.pin {
		t.Fatalf("golangci-lint in BIN_DIR reports %q, want the pinned %q\nmake output:\n%s", got, f.pin, out)
	}
	f.assertNoStagingLeftovers()
}

// TestInstallGolangciLintSkipsWhenAlreadyPinned guards the other half of the
// contract: lint, lint-full, fmt-check and check all take this target as a
// prerequisite, so a binary already at the pin must NOT be reinstalled -- the
// gate cannot start paying a network round-trip per run.
func TestInstallGolangciLintSkipsWhenAlreadyPinned(t *testing.T) {
	f := newPinnedToolFixture(t)
	f.seedBinary(f.pin)
	before, err := os.ReadFile(f.toolPath())
	if err != nil {
		t.Fatalf("read seeded binary: %v", err)
	}

	out, err := f.run()
	if err != nil {
		t.Fatalf("make %s failed: %v\n%s", golangciLintInstallTarget, err, out)
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
}

// TestInstallGolangciLintInstallsWhenAbsentOrUnusable covers the cases that
// must be treated as a mismatch rather than silently accepted.
func TestInstallGolangciLintInstallsWhenAbsentOrUnusable(t *testing.T) {
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
				writeExecutable(f.t, f.toolPath(), "#!/usr/bin/env sh\necho 'golangci-lint has version (devel)'\n")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newPinnedToolFixture(t)
			tc.seed(f)

			out, err := f.run()
			if err != nil {
				t.Fatalf("make %s failed: %v\n%s", golangciLintInstallTarget, err, out)
			}
			if installs := f.installs(); len(installs) != 1 {
				t.Fatalf("go install invocations = %d, want 1\nlog: %v\nmake output:\n%s", len(installs), installs, out)
			}
			if got := f.installedVersion(); got != f.pin {
				t.Fatalf("golangci-lint in BIN_DIR reports %q, want the pinned %q\nmake output:\n%s", got, f.pin, out)
			}
		})
	}
}

// TestInstallGolangciLintRetriesTransientInstallFailures keeps the retry loop
// the file-existence target already had: a flaky proxy fetch must not fail the
// gate on the first attempt.
func TestInstallGolangciLintRetriesTransientInstallFailures(t *testing.T) {
	f := newPinnedToolFixture(t)

	out, err := f.run("STUB_INSTALL_FAILURES=2", "PINNED_TOOL_INSTALL_DELAY=0")
	if err != nil {
		t.Fatalf("make %s failed despite retries: %v\n%s", golangciLintInstallTarget, err, out)
	}
	if installs := f.installs(); len(installs) != 3 {
		t.Fatalf("go install invocations = %d, want 3 (2 failures then success)\nlog: %v\nmake output:\n%s", len(installs), installs, out)
	}
	if got := f.installedVersion(); got != f.pin {
		t.Fatalf("golangci-lint in BIN_DIR reports %q, want the pinned %q", got, f.pin)
	}
}

// TestInstallGolangciLintFailsAfterExhaustingRetries: when every attempt fails
// the gate must fail loudly rather than proceed with whatever is on disk.
func TestInstallGolangciLintFailsAfterExhaustingRetries(t *testing.T) {
	f := newPinnedToolFixture(t)
	f.seedBinary("1.2.3")

	out, err := f.run("STUB_INSTALL_FAILURES=99", "PINNED_TOOL_INSTALL_DELAY=0")
	if err == nil {
		t.Fatalf("make %s succeeded with every install attempt failing:\n%s", golangciLintInstallTarget, out)
	}
	if installs := f.installs(); len(installs) != 5 {
		t.Fatalf("go install invocations = %d, want the documented 5 attempts\nlog: %v\nmake output:\n%s", len(installs), installs, out)
	}
	// A failed install must not leave the developer with no tool at all: the
	// staged binary is moved into place only once it verifies.
	if got := f.installedVersion(); got != "1.2.3" {
		t.Fatalf("binary in BIN_DIR reports %q after a failed install, want the untouched 1.2.3", got)
	}
	f.assertNoStagingLeftovers()
}

// TestInstallGolangciLintFailsWhenInstalledVersionStillWrong: a successful
// install that yields a binary reporting something other than the pin must be
// reported, not accepted. Accepting it would mean either a silently unpinned
// linter (the bug) or a reinstall on every single lint run.
func TestInstallGolangciLintFailsWhenInstalledVersionStillWrong(t *testing.T) {
	f := newPinnedToolFixture(t)

	out, err := f.run("STUB_INSTALL_VERSION=9.9.9")
	if err == nil {
		t.Fatalf("make %s accepted a binary reporting 9.9.9 instead of %s:\n%s", golangciLintInstallTarget, f.pin, out)
	}
	if !strings.Contains(out, f.pin) {
		t.Fatalf("failure message does not name the pinned version %q:\n%s", f.pin, out)
	}
}

// TestLintGatesDependOnVersionCheckedInstall pins the wiring: every gate that
// runs golangci-lint must take the version-checked target as a prerequisite,
// and the file-existence target that caused gascity-430j must not come back.
func TestLintGatesDependOnVersionCheckedInstall(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	makefile := string(data)

	for _, target := range []string{
		"lint-full", "lint-new", "lint-changed", "lint-affected",
		"fmt", "fmt-check", "fmt-check-changed", "install-tools",
	} {
		re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `:([^\n]*)$`)
		m := re.FindStringSubmatch(makefile)
		if m == nil {
			t.Fatalf("Makefile no longer defines target %q", target)
		}
		if !strings.Contains(m[1], golangciLintInstallTarget) {
			t.Fatalf("target %q prerequisites are %q, want the version-checked %q", target, strings.TrimSpace(m[1]), golangciLintInstallTarget)
		}
	}

	if regexp.MustCompile(`(?m)^\$\(GOLANGCI_LINT\):`).MatchString(makefile) {
		t.Fatal("`$(GOLANGCI_LINT):` is back as a file-existence target; make would run its recipe only when the binary is absent, so a wrong-version binary shadows the pin again (gascity-430j)")
	}
}
