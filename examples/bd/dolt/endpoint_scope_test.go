package dolt_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The four commands in this file — status, health, sql, logs — all resolve
// their endpoint from the city's MANAGED Dolt runtime state and historically
// printed results that read as city-wide. On a city where any rig pins its own
// external endpoint (gc.endpoint_origin: explicit) that is a confidently wrong
// answer, not a near miss: the managed server can hold a same-named but EMPTY
// database for such a rig, so the pre-escalation bundle reports "0 beads" for a
// ledger holding hundreds. These tests pin the provenance contract: every one
// of the four names the endpoint it actually queried, and status/health make
// the rigs they did NOT check visible rather than silent (gascity-0zw).

// The fixture rigs mirror the topology the defect was measured against: one
// rig pinned to its own endpoint on a different port from the managed server,
// and one inheriting the city endpoint.
const (
	pinnedRigName    = "winnow"
	pinnedRigHost    = "127.0.0.1"
	pinnedRigPort    = "3307"
	inheritedRigName = "feryn"
)

// writePinnedRig creates a rig directory whose .beads/config.yaml pins its own
// external Dolt endpoint, and returns the rig's absolute path. The rig
// directory is named for the rig so the name is recoverable from the path
// alone — the roster falls back to basename when the name column is empty.
func writePinnedRig(t *testing.T, parent string) string {
	t.Helper()
	rigPath := filepath.Join(parent, pinnedRigName)
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir rig beads dir: %v", err)
	}
	cfg := fmt.Sprintf("issue_prefix: %s\ndolt.mode: \"server\"\ngc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.host: %s\ndolt.port: %s\n",
		pinnedRigName, pinnedRigHost, pinnedRigPort)
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write rig config.yaml: %v", err)
	}
	return rigPath
}

// writeInheritedRig creates a rig directory that inherits the city endpoint.
// Such a rig IS covered by the managed-server commands and must therefore not
// appear in the not-checked list.
func writeInheritedRig(t *testing.T, parent string) string {
	t.Helper()
	rigPath := filepath.Join(parent, inheritedRigName)
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir rig beads dir: %v", err)
	}
	cfg := fmt.Sprintf("issue_prefix: %s\ngc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\n", inheritedRigName)
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write rig config.yaml: %v", err)
	}
	return rigPath
}

// writeFakeGCRigList installs a stub `gc` in dir whose `rig list --json`
// reports the supplied name -> path pairs. Every other gc subcommand exits 1,
// so a script that reaches for an unstubbed gc call degrades exactly as it
// would when gc itself is the broken component.
func writeFakeGCRigList(t *testing.T, dir string, rigs map[string]string) {
	t.Helper()
	entries := make([]string, 0, len(rigs))
	for name, path := range rigs {
		entries = append(entries, fmt.Sprintf(`{"name":%q,"path":%q,"prefix":%q}`, name, path, name))
	}
	payload := `{"schema_version":"1","rigs":[` + strings.Join(entries, ",") + `]}`
	writeExecutable(t, filepath.Join(dir, "gc"), "#!/bin/sh\nif [ \"$1\" = \"rig\" ] && [ \"$2\" = \"list\" ]; then\n  cat <<'JSON'\n"+payload+"\nJSON\n  exit 0\nfi\nexit 1\n")
}

// writeFakeGCBroken installs a stub `gc` that fails for every invocation —
// the state in which rig enumeration cannot be performed at all.
func writeFakeGCBroken(t *testing.T, dir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(dir, "gc"), "#!/bin/sh\nexit 1\n")
}

// writeFailingDolt installs a stub `dolt` that always fails, so health's SQL
// probe resolves deterministically without needing a real server.
func writeFailingDolt(t *testing.T, dir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(dir, "dolt"), "#!/bin/sh\nexit 1\n")
}

func runStatusWithPath(t *testing.T, cityPath, host, port, pathPrefix string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, statusScript))
	cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST="+host,
		"GC_DOLT_PORT="+port,
		"PATH="+pathPrefix+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestStatusNamesEndpointAndListsPinnedRigs is the core gascity-0zw guard for
// `gc dolt status`: the output must name the endpoint it probed and must list
// the rigs pinned to their own endpoint as NOT checked. Silence about those
// rigs is what let two seats diagnose an outage against the wrong server.
func TestStatusNamesEndpointAndListsPinnedRigs(t *testing.T) {
	cityPath := t.TempDir()
	writeFakeBeadsBD(t, cityPath, 0)

	rigParent := t.TempDir()
	pinned := writePinnedRig(t, rigParent)
	inherited := writeInheritedRig(t, rigParent)

	binDir := t.TempDir()
	writeFakeGCRigList(t, binDir, map[string]string{pinnedRigName: pinned, inheritedRigName: inherited})

	out, err := runStatusWithPath(t, cityPath, "127.0.0.1", "3311", binDir)
	if err != nil {
		t.Fatalf("status exited nonzero for reachable managed server: %v\n%s", err, out)
	}
	for _, want := range []string{"127.0.0.1:3311", "winnow", "127.0.0.1:3307"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q; got:\n%s", want, out)
		}
	}
	if !strings.Contains(strings.ToLower(out), "not checked") {
		t.Fatalf("status output never says the pinned rig was NOT checked; got:\n%s", out)
	}
	if strings.Contains(out, "feryn") {
		t.Fatalf("status listed an inherited rig as unchecked; it IS covered by this endpoint:\n%s", out)
	}
}

// TestStatusReportsUnknownWhenRigScanUnavailable pins the fail-loud half of the
// contract. When rig enumeration cannot run, status must say so — an omission
// that reads as "no pinned rigs" is the same wrong answer in a quieter form.
func TestStatusReportsUnknownWhenRigScanUnavailable(t *testing.T) {
	cityPath := t.TempDir()
	writeFakeBeadsBD(t, cityPath, 0)

	binDir := t.TempDir()
	writeFakeGCBroken(t, binDir)

	out, err := runStatusWithPath(t, cityPath, "127.0.0.1", "3311", binDir)
	if err != nil {
		t.Fatalf("status exited nonzero: %v\n%s", err, out)
	}
	if !strings.Contains(out, "UNKNOWN") {
		t.Fatalf("status did not report an unavailable rig scan as UNKNOWN; got:\n%s", out)
	}
}

// TestStatusExternalEndpointNamesItself keeps the external-endpoint path
// carrying its own provenance too.
func TestStatusExternalEndpointNamesItself(t *testing.T) {
	cityPath := t.TempDir()
	writeFakeBeadsBD(t, cityPath, 0)

	binDir := t.TempDir()
	writeFakeGCRigList(t, binDir, map[string]string{})

	out, err := runStatusWithPath(t, cityPath, "superlzy-dolt", "3306", binDir)
	if err != nil {
		t.Fatalf("status exited nonzero for reachable external endpoint: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Endpoint:") || !strings.Contains(out, "superlzy-dolt:3306") {
		t.Fatalf("status output missing external endpoint provenance; got:\n%s", out)
	}
}

func runHealthForScope(t *testing.T, cityPath, host, port, pathPrefix string, args ...string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("sh", append([]string{filepath.Join(root, healthScript)}, args...)...)
	cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT",
		"GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_HEALTH_SKIP_ZOMBIE_SCAN", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST="+host,
		"GC_DOLT_PORT="+port,
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_HEALTH_SKIP_ZOMBIE_SCAN=1",
		"PATH="+pathPrefix+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestHealthHumanNamesEndpointAndListsPinnedRigs is the gascity-0zw guard for
// the human-readable `gc dolt health`: it must name the endpoint it reported
// on and surface the rigs it did not check.
func TestHealthHumanNamesEndpointAndListsPinnedRigs(t *testing.T) {
	cityPath := t.TempDir()

	rigParent := t.TempDir()
	pinned := writePinnedRig(t, rigParent)

	binDir := t.TempDir()
	writeFakeGCRigList(t, binDir, map[string]string{pinnedRigName: pinned})
	writeFailingDolt(t, binDir)

	port, stop := startAcceptingListener(t)
	t.Cleanup(stop)

	// Server unreachable at the SQL layer (stub dolt fails), so health exits
	// non-zero here — the provenance block must print regardless.
	out, _ := runHealthForScope(t, cityPath, "127.0.0.1", strconv.Itoa(port), binDir)
	for _, want := range []string{"Endpoint:", "127.0.0.1:" + strconv.Itoa(port), "winnow", "127.0.0.1:3307"} {
		if !strings.Contains(out, want) {
			t.Fatalf("health human output missing %q; got:\n%s", want, out)
		}
	}
	if !strings.Contains(strings.ToLower(out), "not checked") {
		t.Fatalf("health human output never says the pinned rig was NOT checked; got:\n%s", out)
	}
}

// TestHealthJSONCarriesEndpointProvenance pins the machine-readable half:
// server.host and server.data_dir name the endpoint, and unchecked_endpoints
// enumerates the pinned rigs this report does not cover.
func TestHealthJSONCarriesEndpointProvenance(t *testing.T) {
	cityPath := t.TempDir()

	rigParent := t.TempDir()
	pinned := writePinnedRig(t, rigParent)

	binDir := t.TempDir()
	writeFakeGCRigList(t, binDir, map[string]string{pinnedRigName: pinned})
	writeFailingDolt(t, binDir)

	port, stop := startAcceptingListener(t)
	t.Cleanup(stop)

	out, err := runHealthForScope(t, cityPath, "127.0.0.1", strconv.Itoa(port), binDir, "--json")
	if err != nil {
		t.Fatalf("health --json exited nonzero: %v\n%s", err, out)
	}

	var report struct {
		Server struct {
			Host    string `json:"host"`
			Port    int    `json:"port"`
			DataDir string `json:"data_dir"`
		} `json:"server"`
		UncheckedEndpoints struct {
			Scanned bool `json:"scanned"`
			Rigs    []struct {
				Rig  string `json:"rig"`
				Host string `json:"host"`
				Port string `json:"port"`
			} `json:"rigs"`
		} `json:"unchecked_endpoints"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("health --json emitted invalid JSON: %v\n%s", err, out)
	}
	if report.Server.Host != "127.0.0.1" {
		t.Fatalf("server.host = %q; want 127.0.0.1\n%s", report.Server.Host, out)
	}
	if report.Server.DataDir == "" {
		t.Fatalf("server.data_dir empty; the report does not name the store it describes\n%s", out)
	}
	if !report.UncheckedEndpoints.Scanned {
		t.Fatalf("unchecked_endpoints.scanned = false with a working rig list\n%s", out)
	}
	if len(report.UncheckedEndpoints.Rigs) != 1 ||
		report.UncheckedEndpoints.Rigs[0].Rig != "winnow" ||
		report.UncheckedEndpoints.Rigs[0].Port != "3307" {
		t.Fatalf("unchecked_endpoints.rigs = %+v; want winnow at 127.0.0.1:3307\n%s", report.UncheckedEndpoints.Rigs, out)
	}
}

// TestHealthJSONMarksUnscannedWhenRigListUnavailable keeps the unknown case
// distinguishable from "no pinned rigs" for programmatic consumers.
func TestHealthJSONMarksUnscannedWhenRigListUnavailable(t *testing.T) {
	cityPath := t.TempDir()

	binDir := t.TempDir()
	writeFakeGCBroken(t, binDir)
	writeFailingDolt(t, binDir)

	port, stop := startAcceptingListener(t)
	t.Cleanup(stop)

	out, err := runHealthForScope(t, cityPath, "127.0.0.1", strconv.Itoa(port), binDir, "--json")
	if err != nil {
		t.Fatalf("health --json exited nonzero: %v\n%s", err, out)
	}
	var report struct {
		UncheckedEndpoints struct {
			Scanned bool `json:"scanned"`
		} `json:"unchecked_endpoints"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("health --json emitted invalid JSON: %v\n%s", err, out)
	}
	// Assert the key is present, not merely absent-and-zero: a missing
	// unchecked_endpoints block would satisfy `scanned == false` vacuously
	// while telling a consumer nothing at all.
	if !strings.Contains(out, `"unchecked_endpoints"`) {
		t.Fatalf("health --json omitted unchecked_endpoints entirely\n%s", out)
	}
	if report.UncheckedEndpoints.Scanned {
		t.Fatalf("unchecked_endpoints.scanned = true with a broken rig list\n%s", out)
	}
}

// TestSQLAnnouncesEndpointOnStderr pins scope item 3 for `gc dolt sql`: output
// pasted into an escalation must carry the endpoint it came from. The line goes
// to stderr so query results on stdout stay machine-parseable.
func TestSQLAnnouncesEndpointOnStderr(t *testing.T) {
	root := repoRoot(t)
	binDir := t.TempDir()
	writeFakeDolt(t, binDir)

	port, stop := startAcceptingListener(t)
	t.Cleanup(stop)

	cityPath := t.TempDir()
	cmd := exec.Command("sh", filepath.Join(root, sqlScript), "-q", "SELECT 1")
	cmd.Env = append(filteredEnv("PATH",
		"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR",
		"GC_CITY_PATH", "GC_PACK_DIR",
	),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT="+strconv.Itoa(port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sql.sh exited non-zero: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	want := "127.0.0.1:" + strconv.Itoa(port)
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("sql did not announce the endpoint %q on stderr; stderr:\n%s", want, stderr.String())
	}
	if strings.Contains(stdout.String(), want) {
		t.Fatalf("sql wrote endpoint provenance to stdout; it must not pollute query results:\n%s", stdout.String())
	}
}

// TestLogsAnnouncesEndpointAndLogFile pins scope item 3 for `gc dolt logs`:
// tailed output pasted into an escalation must name the server whose log it is.
func TestLogsAnnouncesEndpointAndLogFile(t *testing.T) {
	root := repoRoot(t)
	cityPath := t.TempDir()

	stateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	logFile := filepath.Join(stateDir, "dolt.log")
	if err := os.WriteFile(logFile, []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	cmd := exec.Command("sh", filepath.Join(root, logsScript))
	cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT=3311",
		"PATH="+os.Getenv("PATH"),
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("logs exited non-zero: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	for _, want := range []string{"127.0.0.1:3311", logFile} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("logs did not announce %q on stderr; stderr:\n%s", want, stderr.String())
		}
	}
	if !strings.Contains(stdout.String(), "line one") {
		t.Fatalf("logs stopped emitting the tailed log on stdout:\n%s", stdout.String())
	}
}

// TestStatusListsPinnedRigWithoutNameColumn pins the row-splitting contract for
// the rig roster. Rows are `NAME<TAB>PATH`, and the name is empty whenever the
// roster came from jq with no .name field or from the no-jq sed fallback. TAB
// is IFS whitespace, so splitting such a row with `IFS=<tab> read -r name path`
// silently drops the leading empty field and lands the PATH in name — which
// made every pinned rig vanish from the not-checked list without any error.
// Splitting on the first tab by parameter expansion is what keeps the empty
// name a field rather than a gap.
func TestStatusListsPinnedRigWithoutNameColumn(t *testing.T) {
	cityPath := t.TempDir()
	writeFakeBeadsBD(t, cityPath, 0)

	rigParent := t.TempDir()
	pinned := writePinnedRig(t, rigParent)

	binDir := t.TempDir()
	// Roster entry carries a path but no name — the shape both the no-jq
	// fallback and a name-less rig list produce.
	writeExecutable(t, filepath.Join(binDir, "gc"),
		"#!/bin/sh\nif [ \"$1\" = \"rig\" ] && [ \"$2\" = \"list\" ]; then\n  printf '{\"rigs\":[{\"path\": \"%s\"}]}\\n' "+
			strconv.Quote(pinned)+"\n  exit 0\nfi\nexit 1\n")

	out, err := runStatusWithPath(t, cityPath, "127.0.0.1", "3311", binDir)
	if err != nil {
		t.Fatalf("status exited nonzero: %v\n%s", err, out)
	}
	if !strings.Contains(out, "127.0.0.1:3307") {
		t.Fatalf("pinned rig vanished from the not-checked list for a name-less roster row; got:\n%s", out)
	}
	// The name falls back to the path's basename so the entry is still
	// identifiable rather than anonymous.
	if !strings.Contains(out, "winnow") {
		t.Fatalf("pinned rig listed without a usable name; want the path basename; got:\n%s", out)
	}
}
