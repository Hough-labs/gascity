package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/strandedwork"
)

// strandedWorkGitProbe is the slice of internal/git.Git the patrol uses.
// Defined as an interface so tests can inject a fake — and, in particular,
// count the subprocesses a pass actually spawns.
type strandedWorkGitProbe interface {
	IsRepo() bool
	RefExists(fullRef string) (bool, error)
	CountCommitsAhead(base, tip string) (int, error)
	DefaultBranch() (string, error)
}

// newStrandedWorkGitProbe returns a probe scoped to workDir. Indirected through
// a package-level var so tests can stub the git invocations.
var newStrandedWorkGitProbe = func(workDir string) strandedWorkGitProbe { return git.New(workDir) }

// gitBranchProber answers strandedwork.BranchProber from one repository.
//
// Every git question it asks is deferred to the first branch actually probed
// and memoized thereafter — whether the directory is a repository at all, and
// what the repository's default branch is (including a failure to resolve it).
// That keeps a patrol pass at zero subprocesses on the healthy path, where no
// bead matches the cheap metadata half and no branch reaches the probe, and at
// one repository check per scope on the path where some do. The controller
// ticks every 30s over every scope, so a question asked before it is needed is
// one paid forever.
type gitBranchProber struct {
	repo             strandedWorkGitProbe
	isRepo           bool
	isRepoResolved   bool
	defaultTarget    string
	defaultTargetErr error
	defaultResolved  bool
}

// newGitBranchProber returns a prober backed by repo.
func newGitBranchProber(repo strandedWorkGitProbe) *gitBranchProber {
	return &gitBranchProber{repo: repo}
}

// ProbeBranch reports the repository's view of branch measured against target,
// resolving the repository default when target is empty.
//
// A directory that is not a git repository, and an absent branch, both
// short-circuit: there is nothing to count and nothing to publish, so the
// remaining probes are skipped. A scope with no repository is the normal shape
// for a city that holds beads but no source, not a failure to investigate.
// Every other failure is returned rather than degraded into a zero value — a
// caller deciding whether committed work is at risk must not read "git could
// not answer" as "nothing is there".
func (p *gitBranchProber) ProbeBranch(branch, target string) (strandedwork.BranchState, error) {
	if !p.repository() {
		return strandedwork.BranchState{}, nil
	}
	branchRef := "refs/heads/" + branch
	localExists, err := p.repo.RefExists(branchRef)
	if err != nil {
		return strandedwork.BranchState{}, err
	}
	if !localExists {
		return strandedwork.BranchState{}, nil
	}
	if target == "" {
		if target, err = p.repositoryDefault(); err != nil {
			return strandedwork.BranchState{}, err
		}
	}
	targetRef, err := p.resolveTargetRef(target)
	if err != nil {
		return strandedwork.BranchState{}, err
	}
	ahead, err := p.repo.CountCommitsAhead(targetRef, branchRef)
	if err != nil {
		return strandedwork.BranchState{}, err
	}
	onOrigin, err := p.repo.RefExists("refs/remotes/origin/" + branch)
	if err != nil {
		return strandedwork.BranchState{}, err
	}
	return strandedwork.BranchState{
		LocalExists:  true,
		Target:       target,
		CommitsAhead: ahead,
		OnOrigin:     onOrigin,
	}, nil
}

// repository reports, once per prober, whether the directory is a git
// repository at all.
func (p *gitBranchProber) repository() bool {
	if !p.isRepoResolved {
		p.isRepo = p.repo.IsRepo()
		p.isRepoResolved = true
	}
	return p.isRepo
}

// repositoryDefault resolves and caches the repository's default branch, used
// as the merge target for work stranded before its submit step recorded one.
func (p *gitBranchProber) repositoryDefault() (string, error) {
	if !p.defaultResolved {
		p.defaultTarget, p.defaultTargetErr = p.repo.DefaultBranch()
		p.defaultResolved = true
	}
	return p.defaultTarget, p.defaultTargetErr
}

// resolveTargetRef returns the ref the commit count is measured against.
// Work branches are cut from a freshly-fetched origin/<base>, so the
// remote-tracking ref is the truthful comparison and the local branch is the
// fallback for a repository that has none. A target neither form resolves is an
// error: counting against the wrong ref answers a different question silently.
func (p *gitBranchProber) resolveTargetRef(target string) (string, error) {
	for _, ref := range []string{"refs/remotes/origin/" + target, "refs/heads/" + target} {
		exists, err := p.repo.RefExists(ref)
		if err != nil {
			return "", err
		}
		if exists {
			return ref, nil
		}
	}
	return "", fmt.Errorf("resolving merge target %q: no such branch locally or on origin", target)
}

// strandedWorkScope pairs one bead store with the repository whose refs its
// work branches live in, and the canonical store reference that labels findings
// from it.
type strandedWorkScope struct {
	storeRef string
	repoPath string
	store    beads.Store
}

// scanStrandedWorkScope reports the stranded work beads in one scope.
//
// A scope with no store, no configured path, or a path that is not a git
// repository yields nothing without an error: a city that is not itself a
// checkout holds no work branches, which is the normal case rather than a
// failure to investigate. The repository check belongs to the prober rather
// than to this function so that it, like every other git question, is only
// asked once a bead has matched the cheap metadata half.
func scanStrandedWorkScope(scope strandedWorkScope) ([]strandedwork.Finding, error) {
	if scope.store == nil || strings.TrimSpace(scope.repoPath) == "" {
		return nil, nil
	}
	prober := newGitBranchProber(newStrandedWorkGitProbe(scope.repoPath))
	return strandedwork.Scan(scope.storeRef, scope.store, prober)
}

// strandedWorkScanInterval bounds how often the patrol looks.
//
// A strand is a slow condition: gascity-cgh sat unreachable for six days, and
// gascity-3vr for a day. Detecting one within five minutes is far inside any
// response an operator can act on, while rescanning at the 30s tick rate would
// charge every store a list and every repository whose beads match a probe,
// 2,880 times a day, to learn nothing changed.
const strandedWorkScanInterval = 5 * time.Minute

// strandedWorkThrottle bounds both halves of the patrol's cost: how often it
// scans, and how often an unchanged finding is restated.
//
// The controller ticks every 30s and a stranded bead stays stranded until
// somebody acts on it, so an unthrottled patrol would both re-scan every store
// twice a minute and restate the same finding thousands of times a day, burying
// the signal it exists to raise. State is per-process by design: it is a
// de-duplication window, not a record, and a controller restart both re-scanning
// immediately and re-announcing what is still stranded is the right behavior.
type strandedWorkThrottle struct {
	// reported maps a finding's identity to the fingerprint last emitted for it.
	reported map[string]string
	// interval is the minimum gap between scans.
	interval time.Duration
	// lastScan is when the patrol last scanned. Its zero value lets the first
	// pass through, so a freshly started controller looks immediately.
	lastScan time.Time
}

// newStrandedWorkThrottle returns an empty throttle scanning at the default
// interval.
func newStrandedWorkThrottle() *strandedWorkThrottle {
	return &strandedWorkThrottle{reported: map[string]string{}, interval: strandedWorkScanInterval}
}

// beginScan reports whether this pass should scan, recording the attempt when
// it does. It mutates rather than answering as a pure predicate so the caller
// cannot scan without also charging the interval.
func (t *strandedWorkThrottle) beginScan(now time.Time) bool {
	if now.Sub(t.lastScan) < t.interval {
		return false
	}
	t.lastScan = now
	return true
}

// admit returns the subset of findings worth emitting: those not seen before,
// and those whose facts changed since the last pass (more commits piled onto
// the branch, or the branch reaching origin — both change what an operator
// should do about it).
//
// It also forgets findings absent from this pass, so a bead that is repaired
// and later strands again in the identical shape is announced again rather than
// silently suppressed — that silence is the failure this patrol exists to end.
func (t *strandedWorkThrottle) admit(findings []strandedwork.Finding) []strandedwork.Finding {
	current := make(map[string]string, len(findings))
	var fresh []strandedwork.Finding
	for _, f := range findings {
		key, fingerprint := strandedWorkKey(f), strandedWorkFingerprint(f)
		current[key] = fingerprint
		if t.reported[key] != fingerprint {
			fresh = append(fresh, f)
		}
	}
	t.reported = current
	return fresh
}

// strandedWorkKey identifies the bead a finding is about. The store ref is part
// of it because bead IDs are only unique within their store.
func strandedWorkKey(f strandedwork.Finding) string {
	return f.StoreRef + "\x00" + f.BeadID
}

// strandedWorkFingerprint captures every fact an operator acts on, so a change
// in any of them re-announces the finding.
func strandedWorkFingerprint(f strandedwork.Finding) string {
	return strings.Join([]string{
		f.Branch,
		f.Target,
		strconv.Itoa(f.CommitsAhead),
		strconv.FormatBool(f.OnOrigin),
	}, "\x00")
}

// emitStrandedWorkEvents records one bead.stranded event per finding and logs
// the same facts to stderr, so the signal survives whether an operator is
// watching the event stream or the controller log.
//
// It never mutates a bead: a stranded bead may hold an unpublished branch, and
// whether to publish it, hand it to the merge queue, or discard it is a
// judgment call that belongs to an operator or a pack, not to the controller.
func emitStrandedWorkEvents(rec events.Recorder, findings []strandedwork.Finding, logPrefix string, stderr io.Writer) {
	if len(findings) == 0 {
		return
	}
	if stderr == nil {
		stderr = io.Discard
	}
	now := time.Now().UTC()
	for _, f := range findings {
		message := formatStrandedWorkMessage(f)
		fmt.Fprintf(stderr, "%s: stranded work: %s\n", logPrefix, message) //nolint:errcheck // best-effort stderr
		if rec == nil {
			continue
		}
		rec.Record(events.Event{
			Type:    events.BeadStranded,
			Ts:      now,
			Actor:   "gc",
			Subject: f.BeadID,
			Message: message,
			Payload: api.BeadStrandedPayloadJSON(f.BeadID, f.StoreRef, f.Branch, f.Target, f.CommitsAhead, f.OnOrigin),
		})
	}
}

// formatStrandedWorkMessage renders the operator-facing text for one finding.
// The origin clause is spelled out rather than left to a boolean because it is
// the difference between work recoverable from the remote and work one disk
// failure from gone.
func formatStrandedWorkMessage(f strandedwork.Finding) string {
	origin := "not on origin"
	if f.OnOrigin {
		origin = "published to origin"
	}
	return fmt.Sprintf(
		"%s in %s is open, unassigned and unrouted, but %s holds %d commit(s) not on %s (%s); no discovery probe can reach it",
		f.BeadID, f.StoreRef, f.Branch, f.CommitsAhead, f.Target, origin,
	)
}

// strandedWorkScopes returns every store the patrol covers: the city store and
// each non-suspended rig store, each paired with the repository its work
// branches live in. A suspended rig is not being worked, so a bead sitting in
// it is waiting by intent rather than stranded.
func (cr *CityRuntime) strandedWorkScopes() []strandedWorkScope {
	scopes := []strandedWorkScope{{
		storeRef: "city:" + cr.cityName,
		repoPath: cr.cityPath,
		store:    cr.cityBeadStore(),
	}}
	suspended := map[string]bool{}
	if cr.cfg != nil {
		suspended = buildEffectiveSuspendedRigNames(cr.cfg, loadSuspensionStateBestEffort(cr.cityPath))
	}
	for name, store := range cr.rigBeadStores() {
		if suspended[name] {
			continue
		}
		scopes = append(scopes, strandedWorkScope{
			storeRef: "rig:" + name,
			repoPath: rigRootByName(cr.cfg, name),
			store:    store,
		})
	}
	return scopes
}

// patrolStrandedWork reports every work bead holding committed work that no
// discovery probe in the city can reach — open, unassigned, unrouted, with a
// branch carrying unmerged commits. It detects and never repairs: publishing
// the branch, handing it to the merge queue, or discarding it are judgment
// calls that belong to an operator or a pack.
//
// It runs after route recovery so a bead whose route was just restored is no
// longer reported as unrouted, and on its own interval rather than on every
// tick. Best-effort: a scope that cannot be scanned is logged and the remaining
// scopes still run.
func (cr *CityRuntime) patrolStrandedWork(now time.Time) {
	if cr.strandedWork == nil {
		cr.strandedWork = newStrandedWorkThrottle()
	}
	if !cr.strandedWork.beginScan(now) {
		return
	}
	var findings []strandedwork.Finding
	for _, scope := range cr.strandedWorkScopes() {
		scoped, err := scanStrandedWorkScope(scope)
		if err != nil {
			fmt.Fprintf(cr.stderr, "%s: stranded-work patrol (%s): %v\n", cr.logPrefix, scope.storeRef, err) //nolint:errcheck // best-effort stderr
		}
		findings = append(findings, scoped...)
	}
	emitStrandedWorkEvents(cr.rec, cr.strandedWork.admit(findings), cr.logPrefix, cr.stderr)
}
