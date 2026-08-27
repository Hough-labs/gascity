package git

import (
	"fmt"
	"strconv"
	"strings"
)

// RefExists reports whether fullRef names an existing ref in this repository.
// fullRef must be fully qualified ("refs/heads/topic",
// "refs/remotes/origin/topic").
//
// It is implemented with for-each-ref rather than `rev-parse --verify` so that
// an absent ref stays distinguishable from a broken repository: `rev-parse
// --verify --quiet` reports both as exit 1, while for-each-ref exits 0 with no
// output for an absent ref and non-zero only when git itself fails. Callers
// deciding whether committed work is at risk must not read "the repository is
// unreadable" as "the branch is not there".
//
// git matches ref patterns literally or up to a slash, so the pattern
// refs/heads/topic also matches refs/heads/topic/sub; only an exact refname
// counts as a hit.
func (g *Git) RefExists(fullRef string) (bool, error) {
	fullRef = strings.TrimSpace(fullRef)
	if fullRef == "" {
		return false, nil
	}
	out, err := g.run("for-each-ref", "--format=%(refname)", fullRef)
	if err != nil {
		return false, fmt.Errorf("checking ref %q: %w", fullRef, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == fullRef {
			return true, nil
		}
	}
	return false, nil
}

// CountCommitsAhead returns the number of commits reachable from tip but not
// from base — zero exactly when tip is fully merged into base. Both revisions
// must resolve; an unknown one is an error rather than a zero count, so a
// mistyped or missing branch cannot masquerade as "nothing unmerged here".
func (g *Git) CountCommitsAhead(base, tip string) (int, error) {
	base, tip = strings.TrimSpace(base), strings.TrimSpace(tip)
	if base == "" || tip == "" {
		return 0, fmt.Errorf("counting commits ahead: base %q and tip %q are both required", base, tip)
	}
	out, err := g.run("rev-list", "--count", base+".."+tip)
	if err != nil {
		return 0, fmt.Errorf("counting commits %s..%s: %w", base, tip, err)
	}
	count := strings.TrimSpace(out)
	n, err := strconv.Atoi(count)
	if err != nil {
		return 0, fmt.Errorf("parsing commit count %q for %s..%s: %w", count, base, tip, err)
	}
	return n, nil
}
