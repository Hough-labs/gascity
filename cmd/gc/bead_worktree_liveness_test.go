package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestWorktreeIsLive_ProcessCWDEqualsWorktree(t *testing.T) {
	wt := t.TempDir()
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(wt)}}
	got, reason := worktreeIsLive(wt, live, nil)
	if !got {
		t.Fatalf("worktreeIsLive = false, want true when a process cwd equals the worktree (reason %q)", reason)
	}
}

func TestWorktreeIsLive_ProcessCWDUnderWorktree(t *testing.T) {
	wt := t.TempDir()
	nested := filepath.Join(wt, "test", "integration")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(nested)}}
	got, reason := worktreeIsLive(wt, live, nil)
	if !got {
		t.Fatalf("worktreeIsLive = false, want true when a process cwd is a nested subdir (reason %q)", reason)
	}
}

func TestWorktreeIsLive_ProcessCWDAboveWorktreeIsNotLive(t *testing.T) {
	parent := t.TempDir()
	wt := filepath.Join(parent, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}
	// A process sitting in the PARENT of the worktree is not "working in" it.
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(parent)}}
	if got, _ := worktreeIsLive(wt, live, nil); got {
		t.Fatal("worktreeIsLive = true, want false when the only live cwd is an ancestor of the worktree")
	}
}

func TestWorktreeIsLive_SiblingCWDIsNotLive(t *testing.T) {
	base := t.TempDir()
	wt := filepath.Join(base, "wt")
	sibling := filepath.Join(base, "other")
	for _, d := range []string{wt, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(sibling)}}
	if got, _ := worktreeIsLive(wt, live, nil); got {
		t.Fatal("worktreeIsLive = true, want false for a sibling directory cwd")
	}
}

func TestWorktreeIsLive_SessionDirProtects(t *testing.T) {
	wt := t.TempDir()
	live := liveWorktreeState{scanned: true} // no live process cwds
	got, reason := worktreeIsLive(wt, live, []string{wt})
	if !got {
		t.Fatalf("worktreeIsLive = false, want true when an active session dir equals the worktree (reason %q)", reason)
	}
}

func TestWorktreeIsLive_NothingMatches(t *testing.T) {
	wt := t.TempDir()
	other := t.TempDir()
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(other)}}
	if got, _ := worktreeIsLive(wt, live, []string{other}); got {
		t.Fatal("worktreeIsLive = true, want false when no live signal is at or under the worktree")
	}
}

func TestCollectLiveWorktreeState_IncludesOwnCWD(t *testing.T) {
	// Both linux (/proc) and darwin (lsof) have a real implementation; every
	// other GOOS deliberately reports scanned=false so the reaper fails closed.
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("collectLiveWorktreeState has no process table on GOOS=%s", runtime.GOOS)
	}
	live := collectLiveWorktreeState()
	if !live.scanned {
		t.Fatalf("collectLiveWorktreeState scanned = false on %s, want true", runtime.GOOS)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	want := pathutil.NormalizePathForCompare(cwd)
	for _, c := range live.cwds {
		if c == want {
			return // found this process's own cwd in the live set
		}
	}
	t.Fatalf("collectLiveWorktreeState did not include this process's cwd %q in %d entries", want, len(live.cwds))
}

// TestCollectLiveWorktreeState_ProtectsDirWithLiveProcess is the end-to-end
// guarantee the reaper actually depends on: a directory with a live process
// working in it must come back as live, so worktreeIsLive protects it.
//
// The own-cwd test above proves the scanner enumerates SOMETHING. This one
// proves it enumerates a foreign process's cwd — the real case, since the
// reaper runs in the witness while the process to protect is an agent. On
// Darwin that is the difference between reading this process's state and
// successfully parsing the whole lsof process table.
func TestCollectLiveWorktreeState_ProtectsDirWithLiveProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("collectLiveWorktreeState has no process table on GOOS=%s", runtime.GOOS)
	}
	dir := t.TempDir()

	// A real child process parked in dir, standing in for an agent mid-stage
	// in its worktree.
	cmd := exec.Command("sleep", "30")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// The process table is not guaranteed to reflect a just-forked child
	// instantly. Poll briefly rather than racing it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		live := collectLiveWorktreeState()
		if !live.scanned {
			t.Fatalf("collectLiveWorktreeState scanned = false on %s, want true", runtime.GOOS)
		}
		if isLive, why := worktreeIsLive(dir, live, nil); isLive {
			t.Logf("protected as expected: %s", why)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("live process cwd %q never appeared in the scan; the reaper would treat it as reapable", dir)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestLiveSessionWorktreeDirs_CollectsAndDedups(t *testing.T) {
	abs1 := t.TempDir()
	abs2 := t.TempDir()
	snapshot := newSessionBeadSnapshotFromInfos([]sessionpkg.Info{
		{ID: "s1", WorkerDir: abs1},
		{ID: "s2", WorkDir: abs2},
		{ID: "s3", WorkerDir: abs1},          // duplicate of s1 → deduped
		{ID: "s4", WorkDir: "relative/path"}, // non-absolute → dropped
		{ID: "s5"},                           // empty → dropped
	})
	got := liveSessionWorktreeDirs(snapshot)

	want := map[string]bool{
		pathutil.NormalizePathForCompare(abs1): false,
		pathutil.NormalizePathForCompare(abs2): false,
	}
	for _, d := range got {
		nd := pathutil.NormalizePathForCompare(d)
		if _, ok := want[nd]; !ok {
			t.Errorf("unexpected dir %q (normalized %q)", d, nd)
			continue
		}
		want[nd] = true
	}
	for d, seen := range want {
		if !seen {
			t.Errorf("expected dir %q missing from result %v", d, got)
		}
	}
}

func TestLiveSessionWorktreeDirs_NilSnapshot(t *testing.T) {
	if got := liveSessionWorktreeDirs(nil); got != nil {
		t.Fatalf("liveSessionWorktreeDirs(nil) = %v, want nil", got)
	}
}
