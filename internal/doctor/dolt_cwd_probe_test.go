package doctor

import (
	"strings"
	"testing"
	"time"
)

func shortenCWDLsofTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	previous := doltCWDLsofTimeout
	doltCWDLsofTimeout = d
	t.Cleanup(func() { doltCWDLsofTimeout = previous })
}

// probeTestPID is an arbitrary PID; every test below routes its /proc lookup
// through an empty root, so the number never has to name a real process.
const probeTestPID = 4242

// withoutProcEntries points the probe's /proc lookup at a directory holding no
// process entries, so the lsof fallback is exercised on Linux exactly as it is
// on Darwin. Without it these tests would pass on macOS and silently assert
// nothing on Linux, where /proc/<pid>/cwd answers before the fallback is ever
// reached.
func withoutProcEntries(t *testing.T) {
	t.Helper()
	previous := doltDoctorProcRoot
	doltDoctorProcRoot = t.TempDir()
	t.Cleanup(func() { doltDoctorProcRoot = previous })
}

// The cwd probe is doctor's only route to a process working directory on hosts
// without /proc. A probe that never completed must be distinguishable from one
// that completed and found nothing: the caller may only conclude ownership from
// the latter. This is the same contract managedDoltDoctorPortHolderPID carries.
func TestManagedDoltDoctorProcessCWDSeparatesProbeFailureFromUnreadableCWD(t *testing.T) {
	t.Run("timeout is unknown, not a negative", func(t *testing.T) {
		withoutProcEntries(t)
		shortenCWDLsofTimeout(t, 150*time.Millisecond)
		installStubLsof(t, "#!/bin/sh\nexec sleep 30\n")

		cwd, probed := managedDoltDoctorProcessCWD(probeTestPID)
		if probed {
			t.Fatal("managedDoltDoctorProcessCWD reported a completed probe after a timeout")
		}
		if cwd != "" {
			t.Fatalf("cwd = %q, want empty on probe failure", cwd)
		}
	})

	t.Run("missing lsof is unknown", func(t *testing.T) {
		withoutProcEntries(t)
		t.Setenv("PATH", t.TempDir())

		if _, probed := managedDoltDoctorProcessCWD(probeTestPID); probed {
			t.Fatal("managedDoltDoctorProcessCWD reported a completed probe with no lsof on PATH")
		}
	})

	t.Run("no cwd entry is a genuine negative", func(t *testing.T) {
		withoutProcEntries(t)
		shortenCWDLsofTimeout(t, 5*time.Second)
		installStubLsof(t, "#!/bin/sh\nexit 1\n")

		cwd, probed := managedDoltDoctorProcessCWD(probeTestPID)
		if !probed {
			t.Fatal("managedDoltDoctorProcessCWD reported probe failure for an lsof that matched nothing")
		}
		if cwd != "" {
			t.Fatalf("cwd = %q, want empty when lsof reported no cwd", cwd)
		}
	})

	t.Run("a cwd is read back", func(t *testing.T) {
		withoutProcEntries(t)
		shortenCWDLsofTimeout(t, 5*time.Second)
		installStubLsof(t, "#!/bin/sh\nprintf 'p1234\\nn/private/var/gascity/dolt\\n'\n")

		cwd, probed := managedDoltDoctorProcessCWD(probeTestPID)
		if !probed {
			t.Fatal("managedDoltDoctorProcessCWD reported probe failure for a successful lsof")
		}
		if cwd != "/var/gascity/dolt" {
			t.Fatalf("cwd = %q, want /var/gascity/dolt (the /private alias normalised away)", cwd)
		}
	})

	t.Run("a non-positive pid is unknown", func(t *testing.T) {
		if _, probed := managedDoltDoctorProcessCWD(0); probed {
			t.Fatal("managedDoltDoctorProcessCWD(0) reported a completed probe")
		}
	})
}

// The lsof budget must match its sibling command probes rather than being
// invented locally; doctor already learned this lesson on the port-holder path.
func TestManagedDoltDoctorCWDLsofBudgetMatchesSiblingCommands(t *testing.T) {
	if doltCWDLsofTimeout != doltPortHolderLsofTimeout {
		t.Fatalf("doltCWDLsofTimeout = %s, want %s to match the sibling probe budget",
			doltCWDLsofTimeout, doltPortHolderLsofTimeout)
	}
}

func TestDoltDoctorCWDFromLsofOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
		wantOK bool
	}{
		{
			name:   "field output",
			output: "p4321\nfcwd\nn/Users/x/city/.gc/runtime/packs/dolt\n",
			want:   "/Users/x/city/.gc/runtime/packs/dolt",
			wantOK: true,
		},
		{
			name:   "darwin private var alias",
			output: "p1\nn/private/var/folders/ab/dolt\n",
			want:   "/var/folders/ab/dolt",
			wantOK: true,
		},
		{
			name:   "darwin private tmp alias",
			output: "p1\nn/private/tmp/dolt\n",
			want:   "/tmp/dolt",
			wantOK: true,
		},
		{
			name:   "no name record",
			output: "p1\nfcwd\n",
			wantOK: false,
		},
		{
			name:   "empty name record is skipped, not read as a path",
			output: "p1\nn\nn/var/dolt\n",
			want:   "/var/dolt",
			wantOK: true,
		},
		{
			name:   "empty output",
			output: "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := doltDoctorCWDFromLsofOutput(tt.output)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Fatalf("cwd = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeDoltDoctorLsofPath(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"/private/var", "/var"},
		{"/private/var/folders/ab", "/var/folders/ab"},
		{"/private/tmp", "/tmp"},
		{"/private/tmp/dolt", "/tmp/dolt"},
		{"/private/varnish", "/private/varnish"},
		{"/var/dolt", "/var/dolt"},
		{"/Users/x/city//dolt", "/Users/x/city/dolt"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := normalizeDoltDoctorLsofPath(tt.in); got != tt.want {
				t.Fatalf("normalizeDoltDoctorLsofPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The defect: the cwd arm of the ownership check was a bare /proc readlink, so
// on Darwin it always returned false and a dolt process was never associated
// with its data dir by working directory alone.
func TestManagedDoltDoctorProcessOwnsRuntimeMatchesCWDWithoutProc(t *testing.T) {
	withoutProcEntries(t)
	dataDir := t.TempDir()
	shortenCWDLsofTimeout(t, 5*time.Second)

	// Report the cwd the way lsof does on Darwin, so the /private normalisation
	// is load-bearing rather than incidentally satisfied.
	reported := dataDir
	for _, alias := range []string{"/var/", "/tmp/"} {
		if strings.HasPrefix(dataDir, alias) {
			reported = "/private" + dataDir
			break
		}
	}
	installStubLsof(t, "#!/bin/sh\nprintf 'p1\\nn"+reported+"\\n'\n")

	if !managedDoltDoctorProcessOwnsRuntime(probeTestPID, dataDir, "/nonexistent/dolt-config.yaml") {
		t.Fatalf("managedDoltDoctorProcessOwnsRuntime did not match a process whose cwd is %s (lsof reported %s)", dataDir, reported)
	}
}

// An unanswered cwd probe must not be read as proof of ownership.
func TestManagedDoltDoctorProcessOwnsRuntimeDoesNotClaimOwnershipOnProbeFailure(t *testing.T) {
	withoutProcEntries(t)
	dataDir := t.TempDir()
	shortenCWDLsofTimeout(t, 150*time.Millisecond)
	installStubLsof(t, "#!/bin/sh\nexec sleep 30\n")

	if managedDoltDoctorProcessOwnsRuntime(probeTestPID, dataDir, "/nonexistent/dolt-config.yaml") {
		t.Fatal("managedDoltDoctorProcessOwnsRuntime claimed ownership from a cwd probe that never completed")
	}
}
