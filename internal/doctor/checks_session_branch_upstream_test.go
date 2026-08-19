package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// Tests for SessionBranchUpstreamCheck (gascity-rwu). The defect these cover is
// silent by construction: a session branch tracking the wrong remote branch
// looks completely normal until someone runs `git pull --rebase` in the
// worktree, at which point git reports the backwards replay as an ordinary
// conflict and the agent resolves it rather than aborting.

// sessionBranchRepo builds a rig repo with an "origin" remote that carries both
// a stale mainline and the rig's real default branch, mirroring the gascity
// topology where origin/main lags origin/edge-integration by months.
func sessionBranchRepo(t *testing.T) (rigPath, worktreesRoot string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	rigPath = filepath.Join(root, "rig")
	worktreesRoot = filepath.Join(root, "city", ".gc", "worktrees")

	seed := filepath.Join(root, "seed")
	doctorRunGit(t, root, "init", "--bare", origin)
	doctorRunGit(t, root, "init", seed)
	doctorRunGit(t, seed, "config", "user.name", "Session Branch Test")
	doctorRunGit(t, seed, "config", "user.email", "session-branch@example.invalid")
	doctorRunGit(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("stale\n"), 0o600); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	doctorRunGit(t, seed, "add", "README.md")
	doctorRunGit(t, seed, "commit", "-m", "stale mainline")
	doctorRunGit(t, seed, "checkout", "-b", "edge-integration")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("current\n"), 0o600); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	doctorRunGit(t, seed, "commit", "-am", "current mainline")
	doctorRunGit(t, seed, "remote", "add", "origin", origin)
	doctorRunGit(t, seed, "push", "origin", "main", "edge-integration")

	doctorRunGit(t, root, "clone", origin, rigPath)
	doctorRunGit(t, rigPath, "config", "user.name", "Session Branch Test")
	doctorRunGit(t, rigPath, "config", "user.email", "session-branch@example.invalid")
	return rigPath, worktreesRoot
}

// addSessionWorktree creates an agent worktree under worktreesRoot on a new
// branch tracking origin/<upstream>, the way a pre_start provisioner does.
func addSessionWorktree(t *testing.T, rigPath, worktreesRoot, branch, upstream string) {
	t.Helper()
	wt := filepath.Join(worktreesRoot, "testrig", branch)
	doctorRunGit(t, rigPath, "worktree", "add", wt, "-b", branch, "refs/remotes/origin/"+upstream)
}

func TestSessionBranchUpstreamCheck_AllTrackDefault_OK(t *testing.T) {
	rigPath, worktreesRoot := sessionBranchRepo(t)
	addSessionWorktree(t, rigPath, worktreesRoot, "gc-polecat-abc", "edge-integration")

	c := NewSessionBranchUpstreamCheck(
		config.Rig{Name: "testrig", Path: rigPath, DefaultBranch: "edge-integration"},
		worktreesRoot,
	)
	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if r.FixHint != "" {
		t.Errorf("FixHint = %q, want empty for OK result", r.FixHint)
	}
}

func TestSessionBranchUpstreamCheck_TracksStaleMainline_WarnsAdvisory(t *testing.T) {
	rigPath, worktreesRoot := sessionBranchRepo(t)
	addSessionWorktree(t, rigPath, worktreesRoot, "gc-polecat-abc", "main")
	addSessionWorktree(t, rigPath, worktreesRoot, "gc-witness-def", "edge-integration")

	c := NewSessionBranchUpstreamCheck(
		config.Rig{Name: "testrig", Path: rigPath, DefaultBranch: "edge-integration"},
		worktreesRoot,
	)
	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Errorf("severity = %d, want SeverityAdvisory", r.Severity)
	}
	if !strings.Contains(r.Message, "edge-integration") {
		t.Errorf("message = %q, want the expected default branch named", r.Message)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "gc-polecat-abc") {
		t.Errorf("details = %v, want the mistracked branch named", r.Details)
	}
	if strings.Contains(joined, "gc-witness-def") {
		t.Errorf("details = %v, want the correctly-tracking branch omitted", r.Details)
	}
	if !strings.Contains(r.FixHint, "branch.gc-polecat-abc.merge") {
		t.Errorf("FixHint = %q, want a git config repoint command", r.FixHint)
	}
}

// The rig root itself is not an agent worktree. Its branch legitimately tracks
// whatever the operator checked out, so flagging it would be a false positive
// on every city.
func TestSessionBranchUpstreamCheck_IgnoresWorktreesOutsideCityRoot(t *testing.T) {
	rigPath, worktreesRoot := sessionBranchRepo(t)
	doctorRunGit(t, rigPath, "checkout", "-b", "local-main", "refs/remotes/origin/main")

	outside := filepath.Join(t.TempDir(), "manual")
	doctorRunGit(t, rigPath, "worktree", "add", outside, "-b", "manual-branch", "refs/remotes/origin/main")

	c := NewSessionBranchUpstreamCheck(
		config.Rig{Name: "testrig", Path: rigPath, DefaultBranch: "edge-integration"},
		worktreesRoot,
	)
	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK — only agent worktrees are in scope", r.Status, r.Message)
	}
}

// A branch with no upstream at all fails loudly on `git pull` instead of
// succeeding catastrophically, so it is not the defect this check hunts.
func TestSessionBranchUpstreamCheck_NoUpstreamIsNotAFinding(t *testing.T) {
	rigPath, worktreesRoot := sessionBranchRepo(t)
	wtBranch := "gc-detachedish"
	wt := filepath.Join(worktreesRoot, "testrig", wtBranch)
	doctorRunGit(t, rigPath, "worktree", "add", wt, "-b", wtBranch, "refs/remotes/origin/edge-integration")
	doctorRunGit(t, rigPath, "branch", "--unset-upstream", wtBranch)

	c := NewSessionBranchUpstreamCheck(
		config.Rig{Name: "testrig", Path: rigPath, DefaultBranch: "edge-integration"},
		worktreesRoot,
	)
	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
}

// Without a recorded default_branch the check has nothing to compare against.
// It must stay inert rather than guess "main" — guessing is what produced the
// bug in the first place.
func TestSessionBranchUpstreamCheck_NoDefaultBranchConfigured_Inert(t *testing.T) {
	rigPath, worktreesRoot := sessionBranchRepo(t)
	addSessionWorktree(t, rigPath, worktreesRoot, "gc-polecat-abc", "main")

	c := NewSessionBranchUpstreamCheck(config.Rig{Name: "testrig", Path: rigPath}, worktreesRoot)
	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "default_branch") {
		t.Errorf("message = %q, want it to say why the check is inert", r.Message)
	}
}

func TestSessionBranchUpstreamCheck_FixRepointsUpstream(t *testing.T) {
	rigPath, worktreesRoot := sessionBranchRepo(t)
	addSessionWorktree(t, rigPath, worktreesRoot, "gc-polecat-abc", "main")

	c := NewSessionBranchUpstreamCheck(
		config.Rig{Name: "testrig", Path: rigPath, DefaultBranch: "edge-integration"},
		worktreesRoot,
	)
	if !c.CanFix() {
		t.Fatal("CanFix() = false, want true — repointing an upstream never touches a working tree")
	}
	if r := c.Run(&CheckContext{}); r.Status != StatusWarning {
		t.Fatalf("pre-fix status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("post-fix status = %d (%s), want StatusOK", r.Status, r.Message)
	}

	got := doctorGitOutput(t, rigPath, "config", "--get", "branch.gc-polecat-abc.merge")
	if got != "refs/heads/edge-integration" {
		t.Errorf("branch.gc-polecat-abc.merge = %q, want refs/heads/edge-integration", got)
	}
}

// Repointing must not move the branch tip. The agent's commits stay exactly
// where they were; only the future rebase target changes.
func TestSessionBranchUpstreamCheck_FixLeavesBranchTipUntouched(t *testing.T) {
	rigPath, worktreesRoot := sessionBranchRepo(t)
	addSessionWorktree(t, rigPath, worktreesRoot, "gc-polecat-abc", "main")

	before := doctorGitOutput(t, rigPath, "rev-parse", "gc-polecat-abc")

	c := NewSessionBranchUpstreamCheck(
		config.Rig{Name: "testrig", Path: rigPath, DefaultBranch: "edge-integration"},
		worktreesRoot,
	)
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	after := doctorGitOutput(t, rigPath, "rev-parse", "gc-polecat-abc")
	if before != after {
		t.Errorf("branch tip moved: %q -> %q", before, after)
	}
}

// With no agent worktrees at all the check must not claim that session branches
// track the default — there are none to track it.
func TestSessionBranchUpstreamCheck_NoAgentWorktrees_ReportsZeroScanned(t *testing.T) {
	rigPath, worktreesRoot := sessionBranchRepo(t)

	c := NewSessionBranchUpstreamCheck(
		config.Rig{Name: "testrig", Path: rigPath, DefaultBranch: "edge-integration"},
		worktreesRoot,
	)
	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "0 agent session branch") {
		t.Errorf("message = %q, want it to report zero branches scanned", r.Message)
	}
}

// A detached worktree has no branch, so it has no upstream to mistrack.
func TestSessionBranchUpstreamCheck_DetachedWorktreeSkipped(t *testing.T) {
	rigPath, worktreesRoot := sessionBranchRepo(t)
	detached := filepath.Join(worktreesRoot, "testrig", "detached")
	doctorRunGit(t, rigPath, "worktree", "add", "--detach", detached, "refs/remotes/origin/main")

	c := NewSessionBranchUpstreamCheck(
		config.Rig{Name: "testrig", Path: rigPath, DefaultBranch: "edge-integration"},
		worktreesRoot,
	)
	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "0 agent session branch") {
		t.Errorf("message = %q, want the detached worktree excluded from the scan", r.Message)
	}
}
