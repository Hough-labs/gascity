//go:build integration

package acp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

type acpConformanceFixture struct {
	once    sync.Once
	dir     string
	command string
	err     error
}

func TestACPConformance(t *testing.T) {
	var fixture acpConformanceFixture
	var counter int64

	runtimetest.RunProviderTests(t, func(caseT *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBackedWithDir(acpConformanceDir(caseT, t, &fixture), Config{}), runtime.Config{
			Command: acpConformanceCommand(caseT, t, &fixture),
			WorkDir: caseT.TempDir(),
		}, fmt.Sprintf("gc-acp-conform-%d", atomic.AddInt64(&counter, 1))
	})
}

// TestACPDefaultDirConformance runs the same full Provider conformance suite
// against the constructor cmd/gc's "acp" registration calls when no city path
// is present: NewSeamBacked, which keeps its control sockets and sidecar meta
// files in the shared os.TempDir()/gc-acp directory rather than an injected
// one. TestACPConformance proves only the WithDir sibling, so nothing exercised
// that composition.
//
// That directory is process-shared by design, so session names carry the PID —
// the suite only asserts membership of its own names, and PID-scoped names keep
// concurrent runs on one machine from colliding there. Isolating the directory
// is deliberately NOT done here: that is the WithDir proof that already exists.
//
// Only the fakeacp binary comes from the shared fixture; its short-path root
// (see prepareACPConformanceFixture) is irrelevant to this test, because the
// hashed sockKey keeps os.TempDir()/gc-acp/s{8 hex}.sock at 70 bytes on Darwin
// — well inside that platform's 104-byte sun_path cap, and independent of how
// long the session name is.
func TestACPDefaultDirConformance(t *testing.T) {
	var fixture acpConformanceFixture
	var counter int64

	runtimetest.RunProviderTests(t, func(caseT *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBacked(Config{}), runtime.Config{
			Command: acpConformanceCommand(caseT, t, &fixture),
			WorkDir: caseT.TempDir(),
		}, fmt.Sprintf("gc-acp-default-%d-%d", os.Getpid(), atomic.AddInt64(&counter, 1))
	})
}

func acpConformanceDir(caseT, ownerT *testing.T, fixture *acpConformanceFixture) string {
	caseT.Helper()
	if err := prepareACPConformanceFixture(ownerT, fixture); err != nil {
		caseT.Fatal(err)
	}
	return fixture.dir
}

func acpConformanceCommand(caseT, ownerT *testing.T, fixture *acpConformanceFixture) string {
	caseT.Helper()
	if err := prepareACPConformanceFixture(ownerT, fixture); err != nil {
		caseT.Fatal(err)
	}
	return fixture.command
}

func prepareACPConformanceFixture(ownerT *testing.T, fixture *acpConformanceFixture) error {
	fixture.once.Do(func() {
		// Unix socket paths are capped at 104 bytes on macOS (vs 108 on
		// Linux), so root the fixture directly under /tmp on Darwin.
		root := os.TempDir()
		if goruntime.GOOS == "darwin" {
			root = "/tmp"
		}
		fixtureRoot, err := os.MkdirTemp(root, "acp-conform")
		if err != nil {
			fixture.err = fmt.Errorf("create ACP conformance fixture: %w", err)
			return
		}
		ownerT.Cleanup(func() { _ = os.RemoveAll(fixtureRoot) })

		fixture.dir = filepath.Join(fixtureRoot, "acp")
		if err := os.MkdirAll(fixture.dir, 0o755); err != nil {
			fixture.err = fmt.Errorf("mkdir %q: %w", fixture.dir, err)
			return
		}

		modRoot, err := moduleRoot()
		if err != nil {
			fixture.err = err
			return
		}
		fixture.command = filepath.Join(fixtureRoot, "fakeacp")
		cmd := exec.Command("go", "build", "-o", fixture.command, "./testdata/fakeacp")
		cmd.Dir = filepath.Join(modRoot, "internal", "runtime", "acp")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fixture.err = fmt.Errorf("building fakeacp: %w", err)
		}
	})
	return fixture.err
}

func moduleRoot() (string, error) {
	cmd := exec.Command("go", "env", "GOMOD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == "/dev/null" {
		return "", fmt.Errorf("not in a Go module")
	}
	return filepath.Dir(filepath.Clean(mod)), nil
}
