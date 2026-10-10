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
// the tracked gate still runs and cannot drift out of sync with it. Forwarders
// were how .githooks ran while another tool owned core.hooksPath; .githooks now
// owns the path itself, but a forwarder left in place still runs the hook.
const hookForwarderMarker = "gascity-hook-forwarder:"

// beadsIntegrationMarker opens the hook block beads' installer writes into the
// hooks it owns. A forwarder installed over a beads hook kept that block and
// ran it after the tracked hook.
const beadsIntegrationMarker = "# --- BEGIN BEADS INTEGRATION"

// beadsChainMarkers identify a tracked hook that calls beads itself, directly
// or through the repo's chain helper (.githooks/lib/beads-chain.sh).
var beadsChainMarkers = []string{"beads-chain", "bd hooks run"}

// RigGitHooksCheck warns when the git hooks a rig's repo actually runs are not
// the hooks it tracks under .githooks/ — a stale copy, a missing install, or a
// hooks directory another tool owns. Such a repo's documented commit and push
// gates are silently inert: git commit and git push both still succeed. It
// also warns when a forwarder runs beads twice: once through the tracked hook,
// which chains to beads, and again through the beads block the forwarder kept.
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

	var shadowed, doubled []string
	for _, name := range tracked {
		trackedPath, effectivePath := filepath.Join(trackedDir, name), filepath.Join(hooksDir, name)
		if detail := inertTrackedHook(trackedPath, effectivePath, name); detail != "" {
			shadowed = append(shadowed, detail)
			continue
		}
		if detail := doubleChainedHook(trackedPath, effectivePath, name); detail != "" {
			doubled = append(doubled, detail)
		}
	}
	if len(shadowed) == 0 && len(doubled) == 0 {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("rig %q: git runs the tracked %s hooks (%s)", c.rig.Name, trackedHooksDirName, strings.Join(tracked, ", "))
		return r
	}

	r.Status = StatusWarning
	if len(shadowed) > 0 {
		r.Message = fmt.Sprintf("rig %q: %d of %d tracked %s hooks never run — git runs %s instead",
			c.rig.Name, len(shadowed), len(tracked), trackedHooksDirName, hooksDir)
	} else {
		r.Message = fmt.Sprintf("rig %q: %d of %d tracked %s hooks run beads twice — the forwarders in %s still carry beads' own hook block",
			c.rig.Name, len(doubled), len(tracked), trackedHooksDirName, hooksDir)
	}
	r.Details = append(append([]string{}, shadowed...), doubled...)
	r.FixHint = fmt.Sprintf("run `make setup` in %q, which points core.hooksPath at %s and verifies the claim (or: git -C %q config core.hooksPath %s)",
		c.rig.Path, trackedHooksDirName, c.rig.Path, trackedHooksDirName)
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

// doubleChainedHook reports why the hook git runs at effectivePath calls beads
// twice, or "" when it does not. That happens when effectivePath is a forwarder
// that kept beads' own integration block and the tracked hook it runs already
// chains to beads: every `bd hooks run` for that hook then fires once from the
// tracked hook and again from the block.
func doubleChainedHook(trackedPath, effectivePath, name string) string {
	trackedRef := trackedHooksDirName + "/" + name
	effective, err := os.ReadFile(effectivePath) //nolint:gosec // path derived from git's own hooks dir
	if err != nil || !isHookForwarder(effective, trackedRef) || !bytes.Contains(effective, []byte(beadsIntegrationMarker)) {
		return ""
	}
	trackedBody, err := os.ReadFile(trackedPath) //nolint:gosec // path derived from the rig's tracked hooks dir
	if err != nil || !chainsToBeads(trackedBody) {
		return ""
	}
	return fmt.Sprintf("%s: %s runs %s, which already chains to beads, and then beads' own hook block, so bd runs this hook twice", name, effectivePath, trackedRef)
}

// chainsToBeads reports whether a tracked hook body calls beads itself.
func chainsToBeads(body []byte) bool {
	for _, marker := range beadsChainMarkers {
		if bytes.Contains(body, []byte(marker)) {
			return true
		}
	}
	return false
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
