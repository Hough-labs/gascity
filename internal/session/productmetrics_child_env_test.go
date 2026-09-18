package session

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestProductMetricsDirectChildEnvSessionSubmitPoller(t *testing.T) {
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "child.env")
	spy := filepath.Join(dir, "gc-child-spy")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$GC_DISABLE_USAGE_METRICS\" \"$BD_DISABLE_METRICS\" \"$OTEL_SERVICE_NAME\" > \"$GC_TEST_PRODUCT_METRICS_CHILD_ENV_SPY\"\n"
	if err := os.WriteFile(spy, []byte(script), 0o700); err != nil {
		t.Fatalf("write child spy: %v", err)
	}
	t.Setenv("GC_TEST_PRODUCT_METRICS_CHILD_ENV_SPY", snapshot)
	t.Setenv(execenv.UsageMetricsDisableEnv, "0")
	t.Setenv("BD_DISABLE_METRICS", "keep-beads-setting")
	t.Setenv("OTEL_SERVICE_NAME", "keep-otel-setting")

	previous := sessionSubmitPollerExecutable
	sessionSubmitPollerExecutable = func() (string, error) { return spy, nil }
	t.Cleanup(func() { sessionSubmitPollerExecutable = previous })

	if err := ensureSessionSubmitPoller(dir, "worker", "session-worker"); err != nil {
		t.Fatalf("ensureSessionSubmitPoller: %v", err)
	}
	want := []string{execenv.UsageMetricsDisableValue, "keep-beads-setting", "keep-otel-setting"}
	deadline := time.Now().Add(testutil.ExecRaceTimeout)
	var got []string
	for {
		// Poll for a COMPLETE snapshot, not merely a readable one. The spy is a
		// shell script and its `>` redirect creates the file before printf has
		// written any of the three lines, so a read can land mid-write and
		// return a prefix. Breaking on the first successful ReadFile compared
		// that prefix and failed with "environment = [1 keep-beads-setting],
		// want [1 keep-beads-setting keep-otel-setting]" under a parallel sweep
		// on a loaded host, while passing 20/20 in isolation (gascity-hpqe).
		// A short read is treated exactly like a missing file.
		//
		// This still catches a genuinely dropped variable: the child then
		// writes an empty third line, which parses as a complete three-element
		// read and mismatches on VALUE rather than on length.
		data, err := os.ReadFile(snapshot)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read child environment snapshot: %v", err)
		}
		if err == nil {
			got = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if len(got) == len(want) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("child environment snapshot was not completely written within %s (last read %#v)", testutil.ExecRaceTimeout, got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !slices.Equal(got, want) {
		t.Fatalf("session submit poller environment = %#v, want %#v", got, want)
	}
}
