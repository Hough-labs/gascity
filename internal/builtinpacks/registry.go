// Package builtinpacks describes the packs bundled into the gc binary.
package builtinpacks

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	gascitypacks "github.com/gastownhall/gascity-packs"

	"github.com/gastownhall/gascity/examples/bd"
	"github.com/gastownhall/gascity/examples/bd/dolt"
	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/fsys"
	gitutil "github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/remotesource"
)

const (
	// Repository is the canonical clone URL for bundled pack imports.
	Repository = "https://github.com/gastownhall/gascity.git"

	// PublicRepository is the wave-one public pack repository. The gc binary
	// can serve its bundled public-pack aliases from the embedded pack set
	// when the network is unavailable during init or doctor repair.
	PublicRepository = "https://github.com/gastownhall/gascity-packs.git"

	// SyntheticCacheNamespace separates bundled synthetic repo caches from
	// ordinary git checkouts that point at the same repository and commit.
	SyntheticCacheNamespace = "bundled-synthetic-v1"

	// canonicalBrowseRef is the branch ref embedded in the dereferenceable
	// GitHub tree URLs CanonicalImportSource authors. It is the browse ref
	// only — the exact pinned commit travels in the import's version field —
	// and matches the ref the public gascity-packs tree sources use.
	canonicalBrowseRef = "main"

	syntheticMarkerFile = ".gc-bundled-pack-cache.toml"
)

// Pack describes a bundled pack and its canonical import source. Bundled
// sources resolve to the pack content embedded in the running gc binary.
type Pack struct {
	Name    string
	Subpath string
	FS      fs.FS
}

// All returns every pack bundled with gc in deterministic order.
//
// The gastown content comes from the gascity-packs Go module (the
// registry repository), not a checked-in copy; its Subpath is retained
// only so legacy gascity.git//examples/... import sources keep resolving
// from the bundled synthetic cache. The canonical gastown source is the
// public gascity-packs one.
func All() []Pack {
	return []Pack{
		{Name: "core", Subpath: "internal/bootstrap/packs/core", FS: core.PackFS},
		{Name: "bd", Subpath: "examples/bd", FS: bd.PackFS},
		{Name: "dolt", Subpath: "examples/bd/dolt", FS: dolt.PackFS},
		{Name: "gastown", Subpath: "examples/gastown/packs/gastown", FS: gascitypacks.Gastown()},
		// The gascity planning pack never lived in gascity.git: it is
		// public-registry-only (empty Subpath), served solely through the
		// PublicRepository alias.
		{Name: "gascity", Subpath: "", FS: gascitypacks.Gascity()},
	}
}

// Source returns the canonical remote import source for a bundled pack.
// Packs that never lived in gascity.git (empty Subpath) are addressed by
// their public registry source.
func Source(name string) (string, bool) {
	pack, ok := ByName(name)
	if !ok {
		return "", false
	}
	if pack.Subpath == "" {
		publicSubpath, ok := publicSubpathForPack(pack.Name)
		if !ok {
			return "", false
		}
		return PublicRepository + "//" + publicSubpath, true
	}
	return Repository + "//" + pack.Subpath, true
}

// CanonicalImportSource returns the source spelling gc writes for NEW
// imports of a bundled pack: a dereferenceable GitHub tree URL pinned to the
// canonical browse ref, matching the authored form documented for
// Import.Source and the form "gc import add" expects. Packs published in the
// public gascity-packs repository resolve to its tree URL (identical to the
// config.PublicGastownPackSource / config.PublicGascityPackSource
// constants); the remaining bundled packs resolve to the gascity.git tree
// URL. The //subpath spelling returned by Source stays the internal
// recognition/cache form; only the authored text changes.
//
// Resolution treats both spellings identically (remotesource.Parse and
// IsSource normalize tree URLs and //subpath forms to the same clone URL +
// subpath), so this only affects how the source reads in pack.toml. The
// FormatGitHubTreeSource fallback to Source keeps a non-GitHub bundled
// repository (should one ever be added) authorable.
func CanonicalImportSource(name string) (string, bool) {
	// Resolve registry identity first: generation must stay tied to an
	// actually-bundled pack, so an unregistered name returns ok=false even
	// if it happens to match publicSubpathForPack (which keys off the name
	// string, not the registry).
	pack, ok := ByName(name)
	if !ok {
		return "", false
	}
	if publicSubpath, ok := publicSubpathForPack(pack.Name); ok {
		if tree, ok := remotesource.FormatGitHubTreeSource(PublicRepository, canonicalBrowseRef, publicSubpath); ok {
			return tree, true
		}
		return PublicRepository + "//" + publicSubpath, true
	}
	if pack.Subpath != "" {
		if tree, ok := remotesource.FormatGitHubTreeSource(Repository, canonicalBrowseRef, pack.Subpath); ok {
			return tree, true
		}
	}
	return Source(name)
}

// MustSource returns the canonical remote import source for a bundled pack.
func MustSource(name string) string {
	source, ok := Source(name)
	if !ok {
		panic("unknown bundled pack " + name)
	}
	return source
}

// ByName returns the bundled pack for name.
func ByName(name string) (Pack, bool) {
	for _, pack := range All() {
		if pack.Name == name {
			return pack, true
		}
	}
	return Pack{}, false
}

// SourceLayout reports the bundled pack name and repository a source
// addresses, normalizing source spellings (tree URLs, //subpath forms)
// the same way IsSource does.
func SourceLayout(source string) (name, repository string, ok bool) {
	normalizedRepo, subpath := splitSource(source)
	for _, layout := range syntheticPackLayouts() {
		if normalizedRepo == layout.Repository && subpath == layout.Subpath {
			return layout.Pack.Name, layout.Repository, true
		}
	}
	return "", "", false
}

// NameForSource reports the bundled pack addressed by source.
func NameForSource(source string) (string, bool) {
	normalizedRepo, subpath := splitSource(source)
	for _, layout := range syntheticPackLayouts() {
		if normalizedRepo == layout.Repository && subpath == layout.Subpath {
			return layout.Pack.Name, true
		}
	}
	return "", false
}

type syntheticPackLayout struct {
	Repository string
	Subpath    string
	Pack       Pack
}

func syntheticPackLayouts() []syntheticPackLayout {
	packs := All()
	layouts := make([]syntheticPackLayout, 0, len(packs)+3)
	for _, pack := range packs {
		if pack.Subpath != "" {
			layouts = append(layouts, syntheticPackLayout{
				Repository: Repository,
				Subpath:    pack.Subpath,
				Pack:       pack,
			})
		}
		for _, legacySubpath := range legacySubpathsForPack(pack.Name) {
			layouts = append(layouts, syntheticPackLayout{
				Repository: Repository,
				Subpath:    legacySubpath,
				Pack:       pack,
			})
		}
		if publicSubpath, ok := publicSubpathForPack(pack.Name); ok {
			layouts = append(layouts, syntheticPackLayout{
				Repository: PublicRepository,
				Subpath:    publicSubpath,
				Pack:       pack,
			})
		}
	}
	return layouts
}

func legacySubpathsForPack(name string) []string {
	switch name {
	case "dolt":
		return []string{"examples/dolt"}
	default:
		return nil
	}
}

func publicSubpathForPack(name string) (string, bool) {
	switch name {
	case "gastown", "gascity":
		return name, true
	default:
		return "", false
	}
}

// IsSource reports whether source addresses one of gc's bundled packs.
func IsSource(source string) bool {
	_, ok := NameForSource(source)
	return ok
}

// MaterializeSyntheticRepo writes the running binary's bundled pack tree to dst
// as a synthetic repository cache for commit. Callers pass only a source's
// CANONICAL pin commit (config.IsBundledSourceAtCanonicalPin gates every
// production call site — any other commit on a bundled source is fetched
// from git for real); the marker content hash is what binds the cache to
// the current binary content. The cache is repo-shaped so relative
// imports between bundled pack subpaths resolve like a real checkout. Callers
// must hold any repo-cache write lock for dst and pass only a disposable cache
// directory; existing contents are removed unconditionally before writing.
func MaterializeSyntheticRepo(dst, commit string) error {
	if strings.TrimSpace(commit) == "" {
		return fmt.Errorf("commit is required")
	}
	if err := validateSyntheticDestination(dst); err != nil {
		return err
	}
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("removing stale bundled pack cache %q: %w", dst, err)
	}
	for _, layout := range syntheticPackLayouts() {
		target := filepath.Join(dst, filepath.FromSlash(layout.Subpath))
		if err := materializeFS(layout.Pack.FS, target); err != nil {
			return fmt.Errorf("materializing bundled pack %q at %s: %w", layout.Pack.Name, layout.Subpath, err)
		}
	}
	hash, err := SyntheticContentHash()
	if err != nil {
		return err
	}
	// Fingerprint the tree we just wrote from the embedded packs — it is
	// verified by construction — so the next validation can confirm it is
	// untouched without re-reading every file. Computed before the marker
	// exists; syntheticTreeFingerprint excludes the marker for that reason.
	fingerprint, err := syntheticTreeFingerprint(dst)
	if err != nil {
		return err
	}
	return writeSyntheticMarker(dst, syntheticMarker{
		Schema:          1,
		Repository:      Repository,
		Commit:          commit,
		ContentHash:     hash,
		TreeFingerprint: fingerprint,
	})
}

// ValidateSyntheticRepoFast verifies that dir is a synthetic bundled-pack cache
// for the current binary content and the source's canonical pin commit without
// walking the materialized file set. It is the resolution-path variant: callers
// on the hot pack-resolution path use it to gate cache hits cheaply. Full
// file-set and file-content integrity is verified only by ValidateSyntheticRepo.
func ValidateSyntheticRepoFast(dir, commit string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("missing bundled pack cache marker")
		}
		return fmt.Errorf("checking bundled pack cache root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("bundled pack cache root %q is a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("bundled pack cache root %q is not a directory", dir)
	}
	data, err := os.ReadFile(filepath.Join(dir, syntheticMarkerFile))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("missing bundled pack cache marker")
		}
		return fmt.Errorf("reading bundled pack cache marker: %w", err)
	}
	var marker syntheticMarker
	if _, err := toml.Decode(string(data), &marker); err != nil {
		return fmt.Errorf("parsing bundled pack cache marker: %w", err)
	}
	if marker.Schema != 1 {
		return fmt.Errorf("unsupported bundled pack cache marker schema %d", marker.Schema)
	}
	if marker.Repository != Repository {
		return fmt.Errorf("bundled pack cache repository %q does not match %q", marker.Repository, Repository)
	}
	if !gitutil.SameCommit(marker.Commit, commit) {
		return fmt.Errorf("bundled pack cache commit %q does not match %q", marker.Commit, commit)
	}
	wantHash, err := syntheticContentHashOnce()
	if err != nil {
		return err
	}
	if marker.ContentHash != wantHash {
		return fmt.Errorf("bundled pack cache content hash %q does not match current binary %q", marker.ContentHash, wantHash)
	}
	return nil
}

// ValidateSyntheticRepo verifies that dir is a synthetic bundled-pack cache
// created for the current binary content and the source's canonical pin
// commit (the only commit production callers materialize).
func ValidateSyntheticRepo(dir, commit string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("missing bundled pack cache marker")
		}
		return fmt.Errorf("checking bundled pack cache root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("bundled pack cache root %q is a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("bundled pack cache root %q is not a directory", dir)
	}

	data, err := os.ReadFile(filepath.Join(dir, syntheticMarkerFile))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("missing bundled pack cache marker")
		}
		return fmt.Errorf("reading bundled pack cache marker: %w", err)
	}
	var marker syntheticMarker
	if _, err := toml.Decode(string(data), &marker); err != nil {
		return fmt.Errorf("parsing bundled pack cache marker: %w", err)
	}
	if marker.Schema != 1 {
		return fmt.Errorf("unsupported bundled pack cache marker schema %d", marker.Schema)
	}
	if marker.Repository != Repository {
		return fmt.Errorf("bundled pack cache repository %q does not match %q", marker.Repository, Repository)
	}
	if !gitutil.SameCommit(marker.Commit, commit) {
		return fmt.Errorf("bundled pack cache commit %q does not match %q", marker.Commit, commit)
	}
	wantHash, err := syntheticContentHashOnce()
	if err != nil {
		return err
	}
	if marker.ContentHash != wantHash {
		return fmt.Errorf("bundled pack cache content hash %q does not match current binary %q", marker.ContentHash, wantHash)
	}
	// Change-detection gate. The recorded fingerprint describes the tree as it
	// stood when its contents were last compared byte-for-byte, so a match
	// means nothing has been created, removed, resized, re-moded or rewritten
	// since. Re-reading every cached file to reach the same conclusion is what
	// made this call seconds long on the path every gc invocation takes
	// (gascity-i7v). A mismatch — or a marker with no fingerprint — falls
	// through to the full comparison below, which is what detects and reports
	// corruption for the caller to heal.
	if marker.TreeFingerprint != "" {
		if fingerprint, err := syntheticTreeFingerprint(dir); err == nil && fingerprint == marker.TreeFingerprint {
			return nil
		}
	}
	return validateSyntheticRepoContents(dir)
}

// validateSyntheticRepoContents compares the materialized file set and every
// file's content and mode against the packs embedded in this binary. It is the
// authoritative integrity check and reads every cached file.
func validateSyntheticRepoContents(dir string) error {
	if err := validateSyntheticRepoFileSet(dir); err != nil {
		return err
	}
	for _, layout := range syntheticPackLayouts() {
		if err := validatePackFiles(layout.Pack, filepath.Join(dir, filepath.FromSlash(layout.Subpath))); err != nil {
			return err
		}
	}
	return nil
}

// syntheticTreeFingerprint returns a stat-only fingerprint of the materialized
// file set under dir: each entry's relative path, type, mode, size and
// modification time. It never opens a file, so it costs a directory walk
// rather than a full content hash.
//
// The marker file is excluded because it carries the fingerprint itself, and
// because MaterializeSyntheticRepo computes the fingerprint before writing it.
func syntheticTreeFingerprint(dir string) (string, error) {
	var entries []string
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == syntheticMarkerFile {
			return nil
		}
		if entry.IsDir() {
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
	}); err != nil {
		return "", fmt.Errorf("fingerprinting bundled pack cache: %w", err)
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return fmt.Sprintf("sha256:%x", sum[:]), nil
}

// SyntheticTreeFingerprintRecorded reports whether dir's marker carries a tree
// fingerprint at all. It reads the marker and does not walk the tree, so it is
// the cheap guard a caller uses to decide whether stamping is worth the cache
// write lock: ValidateSyntheticRepo has already walked the tree by the time the
// caller asks, and walking it a second time to reach the same answer is pure
// duplicate work on the path every gc invocation takes.
//
// A marker whose fingerprint is recorded but stale — content still valid, but
// some entry's size, mode or mtime changed — is not re-stamped here and keeps
// paying the full comparison until the cache is next materialized. That costs
// time, never correctness, and nothing in normal operation produces it.
func SyntheticTreeFingerprintRecorded(dir string) bool {
	marker, err := readSyntheticMarker(dir)
	return err == nil && marker.TreeFingerprint != ""
}

// SyntheticTreeFingerprintCurrent reports whether dir's marker records a tree
// fingerprint that still matches the materialized file set. Unlike
// SyntheticTreeFingerprintRecorded it walks the tree, so it belongs in
// verification paths rather than on a hot guard.
func SyntheticTreeFingerprintCurrent(dir string) bool {
	marker, err := readSyntheticMarker(dir)
	if err != nil || marker.TreeFingerprint == "" {
		return false
	}
	fingerprint, err := syntheticTreeFingerprint(dir)
	return err == nil && fingerprint == marker.TreeFingerprint
}

// StampSyntheticTreeFingerprint records the current tree fingerprint on dir's
// marker so later validations can take the cheap change-detection path. It
// backfills caches materialized by a gc build that predates the field.
//
// The tree is compared byte-for-byte first: a fingerprint is a statement that
// this exact file set was verified, so it is never written for a cache that
// does not currently validate. Callers hold the repo-cache write lock.
func StampSyntheticTreeFingerprint(dir, commit string) error {
	if err := ValidateSyntheticRepoFast(dir, commit); err != nil {
		return err
	}
	if err := validateSyntheticRepoContents(dir); err != nil {
		return err
	}
	marker, err := readSyntheticMarker(dir)
	if err != nil {
		return err
	}
	fingerprint, err := syntheticTreeFingerprint(dir)
	if err != nil {
		return err
	}
	marker.TreeFingerprint = fingerprint
	return writeSyntheticMarker(dir, marker)
}

// readSyntheticMarker decodes dir's bundled-pack cache marker.
func readSyntheticMarker(dir string) (syntheticMarker, error) {
	var marker syntheticMarker
	data, err := os.ReadFile(filepath.Join(dir, syntheticMarkerFile))
	if err != nil {
		return marker, fmt.Errorf("reading bundled pack cache marker: %w", err)
	}
	if _, err := toml.Decode(string(data), &marker); err != nil {
		return marker, fmt.Errorf("parsing bundled pack cache marker: %w", err)
	}
	return marker, nil
}

// writeSyntheticMarker atomically replaces dir's bundled-pack cache marker.
func writeSyntheticMarker(dir string, marker syntheticMarker) error {
	data, err := toml.Marshal(marker)
	if err != nil {
		return fmt.Errorf("marshaling bundled pack cache marker: %w", err)
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, filepath.Join(dir, syntheticMarkerFile), data, 0o644); err != nil {
		return fmt.Errorf("writing bundled pack cache marker: %w", err)
	}
	return nil
}

// MaterializedFileMode returns the filesystem mode used for bundled pack files
// when they are materialized from embed.FS.
func MaterializedFileMode(path string) os.FileMode {
	for _, suffix := range []string{".sh", ".py", ".bash"} {
		if strings.HasSuffix(path, suffix) {
			return 0o755
		}
	}
	return 0o644
}

// SyntheticContentHash returns a stable hash of all bundled pack file content
// and modes.
func SyntheticContentHash() (string, error) {
	var entries []string
	for _, layout := range syntheticPackLayouts() {
		pack := layout.Pack
		manifest, err := manifestForFS(pack.FS)
		if err != nil {
			return "", fmt.Errorf("hashing bundled pack %q: %w", pack.Name, err)
		}
		paths := make([]string, 0, len(manifest))
		for rel := range manifest {
			paths = append(paths, rel)
		}
		sort.Strings(paths)
		for _, rel := range paths {
			file := manifest[rel]
			sum := sha256.Sum256(file.data)
			entries = append(entries, fmt.Sprintf("%s/%s %04o %x", layout.Subpath, rel, file.perm.Perm(), sum[:]))
		}
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return fmt.Sprintf("sha256:%x", sum[:]), nil
}

// syntheticContentHashOnce memoizes SyntheticContentHash. The hash derives
// entirely from embedded pack data, so it is identical for the life of the
// process and is computed at most once.
var syntheticContentHashOnce = sync.OnceValues(SyntheticContentHash)

// SyntheticCacheKeyComponent returns the running binary's bundled-pack content
// hash for inclusion in the synthetic repo cache key, binding each cache
// directory to the binary content that materialized it. Two gc binaries with
// different embedded pack content therefore resolve to different cache
// directories instead of overwriting one shared marker — the citywide
// "bundled pack cache content hash does not match current binary" wedge that
// recurs whenever a deploy leaves two binary versions running side by side.
//
// It returns "" only when the embedded pack set cannot be hashed, which is a
// build-integrity failure that MaterializeSyntheticRepo and ValidateSyntheticRepo
// surface with full context on the next cache operation. Callers fold the
// component into the key only when non-empty, degrading to the legacy
// (content-independent) key rather than failing to resolve a path; this keeps
// the pure cache-key function panic-free without hiding a genuinely broken
// binary.
func SyntheticCacheKeyComponent() string {
	hash, err := syntheticContentHashOnce()
	if err != nil {
		return ""
	}
	return hash
}

type syntheticMarker struct {
	Schema      int    `toml:"schema"`
	Repository  string `toml:"repository"`
	Commit      string `toml:"commit"`
	ContentHash string `toml:"content_hash"`
	// TreeFingerprint is a stat-only fingerprint of the materialized file set,
	// recorded when the tree was last verified byte-for-byte. It is advisory:
	// an empty value (a marker written before this field existed) simply costs
	// the full comparison, so old and new gc builds share a cache safely.
	TreeFingerprint string `toml:"tree_fingerprint,omitempty"`
}

// syntheticTreeFingerprintTOMLKey is the marker key holding TreeFingerprint.
const syntheticTreeFingerprintTOMLKey = "tree_fingerprint"

type fileEntry struct {
	data []byte
	perm os.FileMode
}

func materializeFS(src fs.FS, dst string) error {
	manifest, err := manifestForFS(src)
	if err != nil {
		return err
	}
	for rel, file := range manifest {
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := fsys.WriteFileIfContentOrModeChangedAtomic(fsys.OSFS{}, target, file.data, file.perm); err != nil {
			return err
		}
	}
	return nil
}

func validatePackFiles(pack Pack, dst string) error {
	manifest, err := manifestForFS(pack.FS)
	if err != nil {
		return fmt.Errorf("reading bundled pack %q manifest: %w", pack.Name, err)
	}
	for rel, want := range manifest {
		target := filepath.Join(dst, filepath.FromSlash(rel))
		info, err := os.Lstat(target)
		if err != nil {
			return fmt.Errorf("checking bundled pack cache %q file %s: %w", pack.Name, rel, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != want.perm.Perm() {
			return fmt.Errorf("bundled pack cache %q file %s has mode %s, expected %s", pack.Name, rel, info.Mode().Perm(), want.perm.Perm())
		}
		got, err := os.ReadFile(target)
		if err != nil {
			return fmt.Errorf("reading bundled pack cache %q file %s: %w", pack.Name, rel, err)
		}
		if !bytes.Equal(got, want.data) {
			return fmt.Errorf("bundled pack cache %q file %s content differs from current binary", pack.Name, rel)
		}
	}
	if err := filepath.WalkDir(dst, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dst, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := manifest[rel]; !ok {
			return fmt.Errorf("bundled pack cache %q contains unexpected file %s", pack.Name, rel)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("validating bundled pack cache %q file set: %w", pack.Name, err)
	}
	return nil
}

func validateSyntheticRepoFileSet(dir string) error {
	allowedFiles, allowedDirs, err := syntheticRepoAllowedPaths()
	if err != nil {
		return err
	}
	firstUnexpectedDir := ""
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("bundled pack cache contains symlink %s", rel)
		}
		if entry.IsDir() {
			if _, ok := allowedDirs[rel]; ok {
				return nil
			}
			if firstUnexpectedDir == "" {
				firstUnexpectedDir = rel
			}
			return nil
		}
		if _, ok := allowedFiles[rel]; ok {
			return nil
		}
		return fmt.Errorf("bundled pack cache contains unexpected file %s", rel)
	}); err != nil {
		return fmt.Errorf("validating bundled pack cache file set: %w", err)
	}
	if firstUnexpectedDir != "" {
		return fmt.Errorf("validating bundled pack cache file set: bundled pack cache contains unexpected directory %s", firstUnexpectedDir)
	}
	return nil
}

func syntheticRepoAllowedPaths() (map[string]struct{}, map[string]struct{}, error) {
	files := map[string]struct{}{syntheticMarkerFile: {}}
	dirs := make(map[string]struct{})
	for _, layout := range syntheticPackLayouts() {
		subpath := filepath.ToSlash(layout.Subpath)
		manifest, err := manifestForFS(layout.Pack.FS)
		if err != nil {
			return nil, nil, fmt.Errorf("reading bundled pack %q manifest: %w", layout.Pack.Name, err)
		}
		for rel := range manifest {
			full := path.Join(subpath, rel)
			files[full] = struct{}{}
			for dir := path.Dir(full); dir != "." && dir != "/"; dir = path.Dir(dir) {
				dirs[dir] = struct{}{}
			}
		}
	}
	return files, dirs, nil
}

func manifestForFS(src fs.FS) (map[string]fileEntry, error) {
	manifest := make(map[string]fileEntry)
	if err := fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(src, path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		manifest[filepath.ToSlash(path)] = fileEntry{
			data: data,
			perm: MaterializedFileMode(path),
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if len(manifest) == 0 {
		return nil, fmt.Errorf("bundled pack manifest is empty")
	}
	if _, ok := manifest["pack.toml"]; !ok {
		return nil, fmt.Errorf("bundled pack manifest is missing pack.toml")
	}
	return manifest, nil
}

func splitSource(source string) (repository, subpath string) {
	parsed := remotesource.Parse(source)
	return normalizeRepository(parsed.CloneURL), strings.Trim(parsed.Subpath, "/")
}

func normalizeRepository(repo string) string {
	repo = strings.TrimRight(strings.TrimSpace(repo), "/")
	if strings.HasPrefix(repo, "github.com/") {
		repo = "https://" + repo
	}
	if repo == "https://github.com/gastownhall/gascity" {
		return Repository
	}
	if repo == "https://github.com/gastownhall/gascity-packs" {
		return PublicRepository
	}
	return repo
}

func validateSyntheticDestination(dst string) error {
	if strings.TrimSpace(dst) == "" {
		return fmt.Errorf("refusing to materialize synthetic repo to unsafe path %q", dst)
	}
	clean := filepath.Clean(dst)
	root := filepath.VolumeName(clean) + string(filepath.Separator)
	if clean == "." || clean == root {
		return fmt.Errorf("refusing to materialize synthetic repo to unsafe path %q", dst)
	}
	return nil
}
