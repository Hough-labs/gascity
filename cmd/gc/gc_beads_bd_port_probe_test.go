package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gcBeadsBdScriptSource reads the shipped bd lifecycle script.
func gcBeadsBdScriptSource(t *testing.T) string {
	t.Helper()
	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	b, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	return string(b)
}

// runBdScriptHarness runs a bash harness assembled from shell functions
// extracted out of the lifecycle script, returning its combined output and
// exit code.
//
// Every subprocess in this file routes through this one call site on purpose.
// internal/testpolicy/resourcecensus ratchets untagged subprocess call sites
// and its baseline cannot grow, so the four tests below share a single exec
// seam rather than each constructing their own — the same shape as
// runPackScript in examples/bd/dolt/health_test.go.
func runBdScriptHarness(t *testing.T, script string, env ...string) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run harness: %v\n%s", err, out)
		}
		return string(out), exitErr.ExitCode()
	}
	return string(out), 0
}

// writeFakeGCPortProbeHelper writes a stub `gc` helper that answers the two
// dolt-state subcommands the lifecycle script parses. Each field is a
// tab-separated key/value line, exactly as the real helper emits.
func writeFakeGCPortProbeHelper(t *testing.T, dir string, probeFields, inspectFields []string) string {
	t.Helper()
	emit := func(fields []string) string {
		var b strings.Builder
		for _, f := range fields {
			b.WriteString("  printf '%s\\n' " + shellSingleQuote(f) + "\n")
		}
		return b.String()
	}
	body := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"  'dolt-state probe-managed')\n" + emit(probeFields) + "  exit 0 ;;\n" +
		"  'dolt-state inspect-managed')\n" + emit(inspectFields) + "  exit 0 ;;\n" +
		"esac\n" +
		"exit 1\n"
	p := filepath.Join(dir, "gc")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake gc: %v", err)
	}
	return p
}

// listeningPort opens a real loopback listener and returns its port plus a
// stop func, so a TCP probe in the script sees a genuinely-held port.
func listeningPort(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		_ = ln.Close() //nolint:errcheck // test cleanup
		t.Fatalf("split host port: %v", err)
	}
	return port, func() { _ = ln.Close() }
}

// closedPort returns a port number with nothing listening on it.
func closedPort(t *testing.T) string {
	t.Helper()
	port, stop := listeningPort(t)
	stop()
	return port
}

// TestLoadProbeManagedCapturesPortHolderProbed pins the parse half of
// gascity-bh0: the lifecycle script must read port_holder_probed, which the
// Go layer has emitted since gascity-4k6, instead of letting it fall through
// the case statement unread.
//
// The default when the field is absent is "true" — an older helper that never
// emits it behaved as if every probe completed, and defaulting to "false"
// would make every such call look indeterminate and refuse to start.
func TestLoadProbeManagedCapturesPortHolderProbed(t *testing.T) {
	t.Parallel()

	src := gcBeadsBdScriptSource(t)
	fns := strings.Join([]string{
		extractShellFunction(t, src, "resolve_gc_helper_bin"),
		extractShellFunction(t, src, "load_probe_managed_from_gc"),
	}, "\n")

	cases := []struct {
		name        string
		probeFields []string
		wantProbed  string
	}{
		{
			name: "unanswered port holder probe",
			probeFields: []string{
				"running\tfalse",
				"port_holder_pid\t0",
				"port_holder_probed\tfalse",
				"port_holder_owned\tfalse",
				"port_holder_deleted_inodes\tfalse",
				"tcp_reachable\tfalse",
			},
			wantProbed: "false",
		},
		{
			name: "completed port holder probe",
			probeFields: []string{
				"running\tfalse",
				"port_holder_pid\t0",
				"port_holder_probed\ttrue",
				"port_holder_owned\tfalse",
				"port_holder_deleted_inodes\tfalse",
				"tcp_reachable\tfalse",
			},
			wantProbed: "true",
		},
		{
			name: "field absent defaults to probed",
			probeFields: []string{
				"running\tfalse",
				"port_holder_pid\t0",
				"port_holder_owned\tfalse",
				"tcp_reachable\tfalse",
			},
			wantProbed: "true",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			gcBin := writeFakeGCPortProbeHelper(t, binDir, tc.probeFields, nil)

			script := "connect_host() { printf '127.0.0.1'; }\n" +
				fns + "\n" +
				"load_probe_managed_from_gc\n" +
				"printf 'probed=%s\\n' \"$GC_PROBE_PORT_HOLDER_PROBED\"\n"

			out, exit := runBdScriptHarness(
				t, script,
				"GC_BIN="+gcBin,
				"GC_CITY_PATH="+t.TempDir(),
				"DOLT_PORT=42188",
			)
			if exit != 0 {
				t.Fatalf("harness exit %d:\n%s", exit, out)
			}
			got := strings.TrimSpace(out)
			want := "probed=" + tc.wantProbed
			if got != want {
				t.Fatalf("load_probe_managed_from_gc set %q, want %q", got, want)
			}
		})
	}
}

// TestLoadManagedProcessInspectionCapturesProbedFields pins the second parse
// loop. inspect-managed reports three probe-completion flags; none were read.
func TestLoadManagedProcessInspectionCapturesProbedFields(t *testing.T) {
	t.Parallel()

	src := gcBeadsBdScriptSource(t)
	fns := strings.Join([]string{
		extractShellFunction(t, src, "resolve_gc_helper_bin"),
		extractShellFunction(t, src, "load_managed_process_inspection_from_gc"),
	}, "\n")

	binDir := t.TempDir()
	gcBin := writeFakeGCPortProbeHelper(t, binDir, nil, []string{
		"managed_pid\t0",
		"managed_source\tstate",
		"managed_owned\tfalse",
		"managed_deleted_inodes\tfalse",
		"managed_deleted_inodes_probed\tfalse",
		"port_holder_pid\t0",
		"port_holder_probed\tfalse",
		"port_holder_owned\tfalse",
		"port_holder_deleted_inodes\tfalse",
		"port_holder_deleted_inodes_probed\tfalse",
	})

	script := fns + "\n" +
		"load_managed_process_inspection_from_gc\n" +
		"printf 'holder=%s managed_deleted=%s holder_deleted=%s\\n' " +
		"\"$GC_PORT_HOLDER_PROBED\" \"$GC_MANAGED_DELETED_PROBED\" \"$GC_PORT_HOLDER_DELETED_PROBED\"\n"

	out, exit := runBdScriptHarness(
		t, script,
		"GC_BIN="+gcBin,
		"GC_CITY_PATH="+t.TempDir(),
		"DOLT_PORT=42188",
	)
	if exit != 0 {
		t.Fatalf("harness exit %d:\n%s", exit, out)
	}
	got := strings.TrimSpace(out)
	want := "holder=false managed_deleted=false holder_deleted=false"
	if got != want {
		t.Fatalf("load_managed_process_inspection_from_gc set %q, want %q", got, want)
	}
}

// TestPortProbeUnansweredBlocksStart pins the start-path guard. An unanswered
// port-holder probe leaves port_holder_pid 0, which op_start previously read
// as "the port is free" and fell through to launching a second dolt. The guard
// resolves that unknown with an independent mechanism — a TCP connect — and
// blocks the start only when something is demonstrably listening.
func TestPortProbeUnansweredBlocksStart(t *testing.T) {
	t.Parallel()

	src := gcBeadsBdScriptSource(t)
	fns := strings.Join([]string{
		extractShellFunction(t, src, "tcp_check_port"),
		extractShellFunction(t, src, "port_probe_unanswered_blocks_start"),
	}, "\n")

	held, stop := listeningPort(t)
	defer stop()
	free := closedPort(t)

	cases := []struct {
		name      string
		probed    string
		port      string
		wantBlock bool
	}{
		{"probe completed, port free", "true", free, false},
		{"probe completed, port held", "true", held, false},
		{"probe unanswered, port held", "false", held, true},
		{"probe unanswered, port free", "false", free, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := "connect_host() { printf '127.0.0.1'; }\n" +
				fns + "\n" +
				"port_probe_unanswered_blocks_start\n"

			_, exit := runBdScriptHarness(
				t, script,
				"GC_PROBE_PORT_HOLDER_PROBED="+tc.probed,
				"DOLT_PORT="+tc.port,
			)
			gotBlock := exit == 0
			if gotBlock != tc.wantBlock {
				t.Fatalf("port_probe_unanswered_blocks_start blocked=%v, want %v (probed=%s port=%s)",
					gotBlock, tc.wantBlock, tc.probed, tc.port)
			}
		})
	}
}

// TestOpProbeDistinguishesUnansweredPortHolderProbe is the coverage the bead
// names: a helper reporting port_holder_pid 0 with port_holder_probed false
// must not reach the same branch as one reporting it with probed true. The
// completed probe is a confirmed negative (exit 2); the unanswered one with a
// live listener is indeterminate (exit 3), never "not running".
func TestOpProbeDistinguishesUnansweredPortHolderProbe(t *testing.T) {
	t.Parallel()

	src := gcBeadsBdScriptSource(t)
	fns := strings.Join([]string{
		extractShellFunction(t, src, "tcp_check_port"),
		extractShellFunction(t, src, "tcp_check"),
		extractShellFunction(t, src, "resolve_gc_helper_bin"),
		extractShellFunction(t, src, "load_probe_managed_from_gc"),
		extractShellFunction(t, src, "load_managed_process_inspection_from_gc"),
		extractShellFunction(t, src, "op_probe"),
	}, "\n")

	held, stop := listeningPort(t)
	defer stop()
	free := closedPort(t)

	probeFields := func(running, probed string) []string {
		return []string{
			"running\t" + running,
			"port_holder_pid\t0",
			"port_holder_probed\t" + probed,
			"port_holder_owned\tfalse",
			"port_holder_deleted_inodes\tfalse",
			"tcp_reachable\tfalse",
		}
	}
	inspectFields := func(probed string) []string {
		return []string{
			"managed_pid\t0",
			"managed_source\tstate",
			"managed_owned\tfalse",
			"managed_deleted_inodes\tfalse",
			"managed_deleted_inodes_probed\t" + probed,
			"port_holder_pid\t0",
			"port_holder_probed\t" + probed,
			"port_holder_owned\tfalse",
			"port_holder_deleted_inodes\tfalse",
			"port_holder_deleted_inodes_probed\t" + probed,
		}
	}

	cases := []struct {
		name     string
		running  string
		probed   string
		port     string
		wantExit int
	}{
		{"running server", "true", "true", held, 0},
		{"confirmed not running", "false", "true", free, 2},
		{"unanswered probe with live listener", "false", "false", held, 3},
		{"unanswered probe with free port", "false", "false", free, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			gcBin := writeFakeGCPortProbeHelper(t, binDir,
				probeFields(tc.running, tc.probed), inspectFields(tc.probed))

			script := "connect_host() { printf '127.0.0.1'; }\n" +
				"is_remote() { return 1; }\n" +
				"find_port_holder() { printf ''; }\n" +
				"verify_our_server() { return 1; }\n" +
				fns + "\n" +
				"op_probe\n"

			out, gotExit := runBdScriptHarness(
				t, script,
				"GC_BIN="+gcBin,
				"GC_CITY_PATH="+t.TempDir(),
				"DOLT_PORT="+tc.port,
			)
			if gotExit != tc.wantExit {
				t.Fatalf("op_probe exit=%d, want %d (running=%s probed=%s)\n%s",
					gotExit, tc.wantExit, tc.running, tc.probed, out)
			}
			if tc.wantExit == 3 && !strings.Contains(out, "port-holder probe") {
				t.Fatalf("op_probe exit 3 must explain the indeterminate result, got:\n%s", out)
			}
		})
	}
}
