package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
	gitutil "github.com/gastownhall/gascity/internal/git"
)

// remoteCacheValidationMarkerName is the sidecar recording the last successful
// validation of a git-backed remote pack cache checkout. It lives INSIDE the
// checkout's .git directory on purpose: `git status --porcelain` never reports
// paths under .git, so the marker cannot make the very check it accelerates
// fail, and it is discarded with the checkout it describes.
const remoteCacheValidationMarkerName = "gc-remote-cache-validation.toml"

// remoteCacheValidationMarkerSchema versions the marker. A marker written by a
// different schema is ignored rather than migrated: the only cost of a miss is
// one full validation, which rewrites it.
const remoteCacheValidationMarkerSchema = 1

// remoteCacheValidationMarker records what was true of a remote pack cache the
// last time the full git validation passed for it.
type remoteCacheValidationMarker struct {
	Schema int `toml:"schema"`
	// Commit is the pinned commit that was validated.
	Commit string `toml:"commit"`
	// StatFingerprint is remoteCacheFingerprint: the checkout root and the git
	// index. It catches a reinstall or a git checkout/reset.
	StatFingerprint string `toml:"stat_fingerprint"`
	// TreeFingerprint is remoteCacheTreeFingerprint: a stat-only fingerprint of
	// every path in the worktree. It is what stands in for the `git status
	// --porcelain` tree walk.
	TreeFingerprint string `toml:"tree_fingerprint"`
}

// remoteCacheValidationMarkerPath returns the marker path for a cache checkout.
func remoteCacheValidationMarkerPath(cacheDir string) string {
	return filepath.Join(cacheDir, ".git", remoteCacheValidationMarkerName)
}

// remoteCacheTreeFingerprint returns a stat-only fingerprint of a checkout's
// worktree: each entry's relative path, type, permissions, size and
// modification time. It never opens a file, so it costs a directory walk rather
// than the two git executions (`rev-parse HEAD` plus a `status --porcelain`
// tree walk) it stands in for — measured on a 1.5k-file pack checkout, ~35-60ms
// against ~115-320ms for the git pair.
//
// The .git directory is excluded for two reasons: `git status --porcelain`
// creates and removes a lock file inside it on every run, which would make the
// fingerprint differ from itself; and this marker lives there.
//
// This is deliberately the same shape as the bundled synthetic cache's
// TreeFingerprint (internal/builtinpacks), which is the sanctioned way to skip
// a byte-for-byte check across processes. It is kept separate because the two
// exclude different things: a synthetic cache has no .git to skip.
//
// Detection power matches what it replaces. `git status` consults the index's
// own stat cache first and reports a path as unmodified when size and mtime
// agree, so a same-size same-mtime edit is invisible to both. Anything git
// would report — an edited, added or removed path — changes this fingerprint.
func remoteCacheTreeFingerprint(cacheDir string) (string, error) {
	var entries []string
	err := filepath.WalkDir(cacheDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == cacheDir {
			return nil
		}
		rel, err := filepath.Rel(cacheDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel == ".git" {
				return filepath.SkipDir
			}
			entries = append(entries, "d "+rel)
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		entries = append(entries, fmt.Sprintf("f %s %04o %d %d %d",
			rel, info.Mode().Perm(), info.Mode().Type(), info.Size(), info.ModTime().UnixNano()))
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("fingerprinting remote pack cache %q: %w", cacheDir, err)
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return fmt.Sprintf("sha256:%x", sum[:]), nil
}

// remoteCacheHeadCommit returns the commit a checkout's HEAD names, and whether
// it could be read from HEAD alone. gc's pack caches are detached checkouts, so
// HEAD holds a raw object name; a symbolic ref reports false and sends the
// caller back to git rather than being resolved here.
func remoteCacheHeadCommit(cacheDir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(cacheDir, ".git", "HEAD"))
	if err != nil {
		return "", false
	}
	head := strings.TrimSpace(string(data))
	if head == "" || strings.HasPrefix(head, "ref:") {
		return "", false
	}
	for _, r := range head {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return "", false
		}
	}
	return head, true
}

// remoteCacheValidationRecorded reports whether the recorded validation for
// cacheDir still describes the checkout as it stands, for the pinned commit and
// the caller's already-computed stat fingerprint. Every uncertainty — an
// unreadable marker, a symbolic HEAD, an unwalkable tree — reports false and
// costs one full validation.
func remoteCacheValidationRecorded(cacheDir, commit, statFingerprint string) bool {
	head, ok := remoteCacheHeadCommit(cacheDir)
	if !ok || !gitutil.SameCommit(head, commit) {
		return false
	}
	data, err := os.ReadFile(remoteCacheValidationMarkerPath(cacheDir))
	if err != nil {
		return false
	}
	var marker remoteCacheValidationMarker
	if _, err := toml.Decode(string(data), &marker); err != nil {
		return false
	}
	if marker.Schema != remoteCacheValidationMarkerSchema ||
		!gitutil.SameCommit(marker.Commit, commit) ||
		marker.StatFingerprint != statFingerprint ||
		marker.TreeFingerprint == "" {
		return false
	}
	treeFingerprint, err := remoteCacheTreeFingerprint(cacheDir)
	if err != nil {
		return false
	}
	return treeFingerprint == marker.TreeFingerprint
}

// recordRemoteCacheValidation writes the marker that lets a later process reuse
// a validation this one just passed. It is best-effort: a cache the running
// user cannot write to still validates, it just keeps paying git every time.
func recordRemoteCacheValidation(cacheDir, commit, statFingerprint string) {
	gitDir := filepath.Join(cacheDir, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		return
	}
	treeFingerprint, err := remoteCacheTreeFingerprint(cacheDir)
	if err != nil {
		return
	}
	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(remoteCacheValidationMarker{
		Schema:          remoteCacheValidationMarkerSchema,
		Commit:          commit,
		StatFingerprint: statFingerprint,
		TreeFingerprint: treeFingerprint,
	}); err != nil {
		return
	}
	//nolint:errcheck // best-effort cache bookkeeping; a miss only costs a revalidation
	_ = fsys.WriteFileAtomic(fsys.OSFS{}, remoteCacheValidationMarkerPath(cacheDir), []byte(buf.String()), 0o644)
}
