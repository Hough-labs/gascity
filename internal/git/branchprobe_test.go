package git

import "testing"

// commitFile adds one commit on the current branch so tests can build a branch
// that is genuinely ahead of another.
func commitFile(t *testing.T, dir, message string) {
	t.Helper()
	runGit(t, dir, "commit", "--allow-empty", "-m", message)
}

func TestRefExistsFindsLocalAndRemoteTrackingRefs(t *testing.T) {
	dir := initTestRepo(t)
	runGit(t, dir, "branch", "polecat/gc-1")
	runGit(t, dir, "update-ref", "refs/remotes/origin/polecat/gc-1", "HEAD")
	g := New(dir)

	cases := []struct {
		ref  string
		want bool
	}{
		{"refs/heads/polecat/gc-1", true},
		{"refs/remotes/origin/polecat/gc-1", true},
		{"refs/heads/polecat/gc-2", false},
		{"refs/remotes/origin/polecat/gc-2", false},
		{"", false},
	}
	for _, tc := range cases {
		got, err := g.RefExists(tc.ref)
		if err != nil {
			t.Fatalf("RefExists(%q): %v", tc.ref, err)
		}
		if got != tc.want {
			t.Errorf("RefExists(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
}

func TestRefExistsRequiresAWholeRefNotAPathPrefix(t *testing.T) {
	// git's ref patterns match from the beginning up to a slash, so the pattern
	// refs/heads/polecat also matches refs/heads/polecat/gc-1. Treating that as a
	// hit would report a branch the repository does not have.
	dir := initTestRepo(t)
	runGit(t, dir, "branch", "polecat/gc-1")
	g := New(dir)

	got, err := g.RefExists("refs/heads/polecat")
	if err != nil {
		t.Fatalf("RefExists: %v", err)
	}
	if got {
		t.Error("RefExists(refs/heads/polecat) = true, want false — only refs/heads/polecat/gc-1 exists")
	}
}

func TestRefExistsFailsOutsideARepository(t *testing.T) {
	// A broken repository must not be flattened into "the ref is absent" — the
	// two mean opposite things to a caller deciding whether work is at risk.
	if _, err := New(t.TempDir()).RefExists("refs/heads/main"); err == nil {
		t.Error("RefExists outside a repository returned no error, want the git failure surfaced")
	}
}

func TestCountCommitsAheadCountsOnlyUnmergedCommits(t *testing.T) {
	dir := initTestRepo(t)
	base, err := New(dir).CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	runGit(t, dir, "checkout", "-b", "polecat/gc-1")
	commitFile(t, dir, "work 1")
	commitFile(t, dir, "work 2")
	g := New(dir)

	ahead, err := g.CountCommitsAhead(base, "polecat/gc-1")
	if err != nil {
		t.Fatalf("CountCommitsAhead: %v", err)
	}
	if ahead != 2 {
		t.Errorf("CountCommitsAhead(%s, polecat/gc-1) = %d, want 2", base, ahead)
	}

	// Merging the branch into its base leaves nothing stranded on it.
	runGit(t, dir, "checkout", base)
	runGit(t, dir, "merge", "--ff-only", "polecat/gc-1")
	ahead, err = g.CountCommitsAhead(base, "polecat/gc-1")
	if err != nil {
		t.Fatalf("CountCommitsAhead after merge: %v", err)
	}
	if ahead != 0 {
		t.Errorf("CountCommitsAhead after merge = %d, want 0", ahead)
	}
}

func TestCountCommitsAheadRejectsUnresolvableRevisions(t *testing.T) {
	dir := initTestRepo(t)
	g := New(dir)

	if _, err := g.CountCommitsAhead("main", "polecat/does-not-exist"); err == nil {
		t.Error("CountCommitsAhead with an unknown tip returned no error")
	}
	if _, err := g.CountCommitsAhead("", "polecat/gc-1"); err == nil {
		t.Error("CountCommitsAhead with an empty base returned no error")
	}
}
