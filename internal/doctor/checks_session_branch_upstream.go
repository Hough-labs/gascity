package doctor

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// SessionBranchUpstreamCheck warns when an agent worktree's branch tracks a
// remote branch other than the rig's configured default branch.
//
// The failure it hunts is silent in the worst way. A session branch whose
// upstream points at a stale mainline looks entirely normal until something
// runs `git pull --rebase` in that worktree; git then replays the agent's work
// onto the wrong base and reports it as an ordinary conflict, so the agent
// resolves the conflicts one by one and bakes the backwards replay in. Nothing
// in git or gc says "your base is wrong" (gascity-rwu: two agents wedged, one
// caught 15 commits into a 183-commit backwards replay). This check is the
// thing that says it.
//
// SeverityAdvisory; WarmupEligible, because `gc start` is the moment before
// agents begin pulling.
type SessionBranchUpstreamCheck struct {
	rig           config.Rig
	worktreesRoot string
	gitPath       func(name string) (string, error) // injectable for tests
}

// NewSessionBranchUpstreamCheck creates a session-branch upstream check for the
// given rig. worktreesRoot is the city's agent-worktree root; only worktrees
// beneath it are in scope, so operator checkouts and the rig root itself are
// never flagged.
func NewSessionBranchUpstreamCheck(rig config.Rig, worktreesRoot string) *SessionBranchUpstreamCheck {
	return &SessionBranchUpstreamCheck{rig: rig, worktreesRoot: worktreesRoot, gitPath: exec.LookPath}
}

// Name returns the check identifier.
func (c *SessionBranchUpstreamCheck) Name() string {
	return "rig:" + c.rig.Name + ":session-branch-upstream"
}

// WarmupEligible returns true so this check runs during gc start warm-up.
func (c *SessionBranchUpstreamCheck) WarmupEligible() bool { return true }

// CanFix returns true. Repointing an upstream rewrites one git config key and
// never touches a working tree, an index, or a branch tip — it changes only
// where a future rebase would land, and only to the branch the city is
// configured to merge into.
func (c *SessionBranchUpstreamCheck) CanFix() bool { return true }

// Fix repoints every mistracked agent session branch at the rig's default
// branch. It is the mechanical form of the repair an operator would otherwise
// run by hand, one `git config branch.<name>.merge` at a time.
func (c *SessionBranchUpstreamCheck) Fix(_ *CheckContext) error {
	gitBin, err := c.gitPath("git")
	if err != nil {
		return fmt.Errorf("locating git: %w", err)
	}
	defaultBranch := c.rig.EffectiveDefaultBranch()
	if defaultBranch == "" {
		return nil
	}
	_, findings, err := c.mistrackedBranches(gitBin, defaultBranch)
	if err != nil {
		return err
	}
	want := "refs/heads/" + defaultBranch
	for _, f := range findings {
		if _, err := runGitCommand(gitBin, c.rig.Path, "config", "branch."+f.branch+".merge", want); err != nil {
			return fmt.Errorf("repointing %s upstream to %s: %w", f.branch, want, err)
		}
	}
	return nil
}

// Run reports agent session branches whose upstream disagrees with the rig's
// configured default branch.
func (c *SessionBranchUpstreamCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}

	defaultBranch := c.rig.EffectiveDefaultBranch()
	if defaultBranch == "" {
		// Deliberately inert rather than falling back to "main": guessing a
		// mainline from something other than config is the defect itself.
		r.Status = StatusOK
		r.Message = fmt.Sprintf("rig %q: no default_branch recorded — nothing to compare session branches against", c.rig.Name)
		return r
	}

	gitBin, err := c.gitPath("git")
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("rig %q: unable to read session branches — git unavailable", c.rig.Name)
		return r
	}

	scanned, findings, err := c.mistrackedBranches(gitBin, defaultBranch)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("rig %q: unable to read session branches — %v", c.rig.Name, err)
		return r
	}

	if len(findings) == 0 {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("rig %q: %d agent session branch(es) track %s", c.rig.Name, scanned, defaultBranch)
		return r
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf("rig %q: %d agent session branch(es) track a branch other than %s — one `git pull --rebase` replays that agent's work onto the wrong base",
		c.rig.Name, len(findings), defaultBranch)

	want := "refs/heads/" + defaultBranch
	hints := make([]string, 0, len(findings))
	for _, f := range findings {
		r.Details = append(r.Details, fmt.Sprintf("%s tracks %s (expected %s) — worktree %s", f.branch, f.merge, want, f.worktree))
		hints = append(hints, fmt.Sprintf("git -C %q config branch.%s.merge %s", c.rig.Path, f.branch, want))
	}
	r.FixHint = strings.Join(hints, "; ")
	return r
}

// mistrackedFinding is one agent session branch whose upstream disagrees with
// the rig's default branch.
type mistrackedFinding struct {
	branch   string // local branch name
	merge    string // configured branch.<name>.merge value
	worktree string // worktree path the branch is checked out in
}

// mistrackedBranches returns how many in-scope session branches were examined
// and, of those, the ones whose configured upstream is not the rig's default
// branch. Findings are sorted by branch name so results and fix hints are
// stable across runs.
func (c *SessionBranchUpstreamCheck) mistrackedBranches(gitBin, defaultBranch string) (int, []mistrackedFinding, error) {
	worktrees, err := c.agentWorktreeBranches(gitBin)
	if err != nil {
		return 0, nil, err
	}
	if len(worktrees) == 0 {
		return 0, nil, nil
	}
	merges, err := branchMergeRefs(gitBin, c.rig.Path)
	if err != nil {
		return 0, nil, err
	}

	want := "refs/heads/" + defaultBranch
	var findings []mistrackedFinding
	for branch, worktree := range worktrees {
		merge, tracked := merges[branch]
		// An untracked branch is not this defect: `git pull` on it fails
		// loudly instead of succeeding onto the wrong base.
		if !tracked || merge == want {
			continue
		}
		findings = append(findings, mistrackedFinding{branch: branch, merge: merge, worktree: worktree})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].branch < findings[j].branch })
	return len(worktrees), findings, nil
}

// agentWorktreeBranches maps branch name to worktree path for every linked
// worktree of the rig repo that lives under the city's agent-worktree root.
// Detached worktrees have no branch to mistrack and are skipped.
func (c *SessionBranchUpstreamCheck) agentWorktreeBranches(gitBin string) (map[string]string, error) {
	root := strings.TrimSpace(c.worktreesRoot)
	if root == "" {
		return nil, nil
	}
	out, err := runGitCommand(gitBin, c.rig.Path, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("listing worktrees: %w", err)
	}

	branches := make(map[string]string)
	var current string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			current = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		case strings.HasPrefix(line, "branch "):
			ref := strings.TrimSpace(strings.TrimPrefix(line, "branch "))
			name := strings.TrimPrefix(ref, "refs/heads/")
			if name != "" && isUnderRoot(current, root) {
				branches[name] = current
			}
		}
	}
	return branches, nil
}

// isUnderRoot reports whether path is root or lives beneath it. Both sides are
// resolved through filepath.EvalSymlinks where possible so a symlinked city
// root (a /tmp -> /private/tmp macOS temp dir, for instance) still matches the
// paths git reports.
func isUnderRoot(path, root string) bool {
	if path == "" {
		return false
	}
	path = resolvePathForCompare(path)
	root = resolvePathForCompare(root)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolvePathForCompare returns an absolute, symlink-resolved form of path,
// degrading to a cleaned absolute path when the path does not exist yet.
func resolvePathForCompare(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// branchMergeRefs reads every configured branch.<name>.merge value in dir.
// Branch names may themselves contain dots (gc-gastown.rictus-54e7a1c9cabd),
// so the name is recovered by trimming the fixed prefix and suffix rather than
// by splitting on ".".
func branchMergeRefs(gitBin, dir string) (map[string]string, error) {
	out, err := runGitCommand(gitBin, dir, "config", "--get-regexp", `^branch\..*\.merge$`)
	if err != nil {
		// git config exits 1 when nothing matches, which is a legitimate
		// "no branch has an upstream" state, not a failure.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("reading branch upstreams: %w", err)
	}

	merges := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, "branch."), ".merge")
		if name != "" && name != key {
			merges[name] = strings.TrimSpace(value)
		}
	}
	return merges, nil
}
