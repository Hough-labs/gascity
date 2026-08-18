package doctor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func installStubLsof(t *testing.T, script string) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "lsof"), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(lsof): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
}

func shortenPortHolderLsofTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	previous := doltPortHolderLsofTimeout
	doltPortHolderLsofTimeout = d
	t.Cleanup(func() { doltPortHolderLsofTimeout = previous })
}

// Doctor's port-holder lookup is the only path to port ownership on hosts
// without /proc. A lookup that times out must report an unanswered probe, not
// an unheld port: the caller falls back to ownership evidence on the former and
// would draw a conclusion from the latter.
func TestManagedDoltDoctorPortHolderFromLsofSeparatesProbeFailureFromNoHolder(t *testing.T) {
	t.Run("timeout is unknown, not a negative", func(t *testing.T) {
		shortenPortHolderLsofTimeout(t, 150*time.Millisecond)
		installStubLsof(t, "#!/bin/sh\nexec sleep 30\n")

		pid, probed := managedDoltDoctorPortHolderFromLsof(3306)
		if probed {
			t.Fatal("managedDoltDoctorPortHolderFromLsof reported a completed probe after a timeout")
		}
		if pid != 0 {
			t.Fatalf("pid = %d, want 0 on probe failure", pid)
		}
	})

	t.Run("no match is a genuine negative", func(t *testing.T) {
		shortenPortHolderLsofTimeout(t, 5*time.Second)
		installStubLsof(t, "#!/bin/sh\nexit 1\n")

		pid, probed := managedDoltDoctorPortHolderFromLsof(3306)
		if !probed {
			t.Fatal("managedDoltDoctorPortHolderFromLsof reported probe failure for an lsof that matched nothing")
		}
		if pid != 0 {
			t.Fatalf("pid = %d, want 0 when nothing matched", pid)
		}
	})

	t.Run("missing lsof is unknown", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())

		if _, probed := managedDoltDoctorPortHolderFromLsof(3306); probed {
			t.Fatal("managedDoltDoctorPortHolderFromLsof reported a completed probe with no lsof on PATH")
		}
	})

	t.Run("a live holder is found", func(t *testing.T) {
		shortenPortHolderLsofTimeout(t, 5*time.Second)
		installStubLsof(t, "#!/bin/sh\necho "+strconv.Itoa(os.Getpid())+"\n")

		pid, probed := managedDoltDoctorPortHolderFromLsof(3306)
		if !probed {
			t.Fatal("managedDoltDoctorPortHolderFromLsof reported probe failure for a successful lsof")
		}
		if pid != os.Getpid() {
			t.Fatalf("pid = %d, want %d", pid, os.Getpid())
		}
	})
}

// The Darwin budget must match the sibling command timeout rather than the old
// 250ms, which was 8x tighter than the path the failure was measured on.
func TestManagedDoltDoctorPortHolderLsofBudgetMatchesSiblingCommands(t *testing.T) {
	if doltPortHolderLsofTimeout != doltVersionCommandTimeout {
		t.Fatalf("doltPortHolderLsofTimeout = %s, want %s to match the sibling command budget",
			doltPortHolderLsofTimeout, doltVersionCommandTimeout)
	}
}

func TestManagedDoltDoctorPortHolderPIDReportsUnprobedPort(t *testing.T) {
	if _, probed := managedDoltDoctorPortHolderPID(0); probed {
		t.Fatal("managedDoltDoctorPortHolderPID(0) reported a completed probe")
	}
}
