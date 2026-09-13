package doctor

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// trackedHooksDirName is the conventional in-repo directory holding a
// project's tracked git hooks. Git never runs it by name: it runs exactly one
// hooks directory — core.hooksPath when set, otherwise $GIT_DIR/hooks — so a
// repo that tracks hooks here only gets them when something wires them up.
const trackedHooksDirName = ".githooks"

// hookForwarderMarker is the sentinel comment an installed forwarder carries.
// A forwarder is not a copy of the tracked hook; it execs the tracked hook, so
// the tracked gate still runs and cannot drift out of sync with it.
const hookForwarderMarker = "gascity-hook-forwarder:"

// RigGitHooksCheck warns when the git hooks a rig's repo actually runs are not
// the hooks it tracks under .githooks/ — a stale copy, a missing install, or a
// hooks directory another tool owns. Such a repo's documented commit and push
// gates are silently inert: git commit and git push both still succeed.
// SeverityAdvisory; not WarmupEligible.
type RigGitHooksCheck struct {
	rig     config.Rig
	gitPath func(name string) (string, error) // injectable for tests
}

// NewRigGitHooksCheck creates a tracked-git-hooks check for the given rig.
func NewRigGitHooksCheck(rig config.Rig) *RigGitHooksCheck {
	return &RigGitHooksCheck{rig: rig, gitPath: exec.LookPath}
}

// Name returns the check identifier.
func (c *RigGitHooksCheck) Name() string { return "rig:" + c.rig.Name + ":git-hooks" }

// WarmupEligible returns false; hook wiring does not gate city startup.
func (c *RigGitHooksCheck) WarmupEligible() bool { return false }

// CanFix returns false: which directory owns core.hooksPath is an operator
// decision, and repointing it can disable another tool's hooks.
func (c *RigGitHooksCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *RigGitHooksCheck) Fix(_ *CheckContext) error { return nil }

// Run compares each tracked hook against the file git would actually run.
func (c *RigGitHooksCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}

	trackedDir := filepath.Join(c.rig.Path, trackedHooksDirName)
	tracked := trackedHookNames(trackedDir)
	if len(tracked) == 0 {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("rig %q tracks no %s/ hooks — nothing to enforce", c.rig.Name, trackedHooksDirName)
		return r
	}

	gitBin, err := c.gitPath("git")
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("rig %q: cannot resolve the effective hooks directory — git unavailable", c.rig.Name)
		return r
	}
	hooksDir, err := effectiveHooksDir(gitBin, c.rig.Path)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("rig %q: cannot resolve the effective hooks directory — git could not read %s as a repo", c.rig.Name, c.rig.Path)
		return r
	}

	var shadowed []string
	for _, name := range tracked {
		if detail := inertTrackedHook(filepath.Join(trackedDir, name), filepath.Join(hooksDir, name), name); detail != "" {
			shadowed = append(shadowed, detail)
		}
	}
	if len(shadowed) == 0 {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("rig %q: git runs the tracked %s hooks (%s)", c.rig.Name, trackedHooksDirName, strings.Join(tracked, ", "))
		return r
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf("rig %q: %d of %d tracked %s hooks never run — git runs %s instead",
		c.rig.Name, len(shadowed), len(tracked), trackedHooksDirName, hooksDir)
	r.Details = shadowed
	r.FixHint = fmt.Sprintf("git -C %q config core.hooksPath %s — or, when another tool owns %s, install forwarders there that exec %s/<hook>",
		c.rig.Path, trackedHooksDirName, hooksDir, trackedHooksDirName)
	return r
}

// trackedHookNames returns the sorted names of executable hook files tracked
// in dir. The executable bit is the filter: a README or a fixture alongside
// the hooks is not something git would ever run.
func trackedHookNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || strings.HasSuffix(entry.Name(), ".sample") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// effectiveHooksDir returns the absolute directory git resolves hooks from for
// repoPath. `git rev-parse --git-path hooks` honors core.hooksPath, so this is
// the one answer that accounts for another tool having claimed it.
func effectiveHooksDir(gitBin, repoPath string) (string, error) {
	out, err := runGitCommand(gitBin, repoPath, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(out)
	if dir == "" {
		return "", fmt.Errorf("git reported no hooks path for %s", repoPath)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoPath, dir)
	}
	return filepath.Clean(dir), nil
}

// inertTrackedHook reports why the tracked hook at trackedPath never runs,
// or "" when git running effectivePath does run it. A byte-identical copy
// counts as running it today; a forwarder counts as running it permanently.
func inertTrackedHook(trackedPath, effectivePath, name string) string {
	trackedRef := trackedHooksDirName + "/" + name
	if sameHookFile(trackedPath, effectivePath) {
		return ""
	}
	info, err := os.Stat(effectivePath)
	if err != nil {
		return fmt.Sprintf("%s: %s is not installed, so %s never runs", name, effectivePath, trackedRef)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Sprintf("%s: %s is not executable, so %s never runs", name, effectivePath, trackedRef)
	}
	effective, err := os.ReadFile(effectivePath) //nolint:gosec // path derived from git's own hooks dir
	if err != nil {
		return fmt.Sprintf("%s: cannot read %s to compare it against %s: %v", name, effectivePath, trackedRef, err)
	}
	trackedBody, err := os.ReadFile(trackedPath) //nolint:gosec // path derived from the rig's tracked hooks dir
	if err != nil {
		return fmt.Sprintf("%s: cannot read %s to compare it against %s: %v", name, trackedRef, effectivePath, err)
	}
	if bytes.Equal(effective, trackedBody) {
		return ""
	}
	if isHookForwarder(effective, trackedRef) {
		return ""
	}
	return fmt.Sprintf("%s: %s is a stale copy — it differs from %s, which is what git should run", name, effectivePath, trackedRef)
}

// isHookForwarder reports whether body is a forwarder chaining to trackedRef.
func isHookForwarder(body []byte, trackedRef string) bool {
	text := string(body)
	return strings.Contains(text, hookForwarderMarker) && strings.Contains(text, trackedRef)
}

// sameHookFile reports whether both paths name the same file on disk, which is
// the case when core.hooksPath points straight at the tracked hooks directory.
func sameHookFile(a, b string) bool {
	infoA, err := os.Stat(a)
	if err != nil {
		return false
	}
	infoB, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(infoA, infoB)
}
