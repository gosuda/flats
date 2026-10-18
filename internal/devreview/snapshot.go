package devreview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// ManifestFormat identifies the manifest layout written by Snapshot.
const ManifestFormat = "flats-review-snapshot/1"

// Status of a path against a comparison point (the merge base or HEAD).
const (
	Unchanged   = "unchanged"
	Added       = "added"
	Modified    = "modified"
	Deleted     = "deleted"
	TypeChanged = "type-changed"
	// Untracked marks a path that exists in the working tree but not in
	// the index. Its content is identified by the hash alone.
	Untracked = "untracked"
)

// Options select the candidate.
type Options struct {
	// Include names ignored or unchanged inputs that belong to the
	// candidate: clean slash-separated paths relative to the repository
	// root. A directory means every file beneath it.
	Include []string `json:"include,omitempty"`
	// Exclude holds patterns for paths that are not candidate content,
	// such as review reports, logs and probe output. A pattern ending in
	// "/" is a directory prefix; any other pattern uses path.Match against
	// the whole path, and a pattern without "/" also against the base name.
	// Exclusion takes precedence over every other rule.
	Exclude []string `json:"exclude,omitempty"`
}

// Manifest binds a review to a base, a HEAD commit, the index and the exact
// content of every candidate path.
type Manifest struct {
	Format      string     `json:"format"`
	Created     string     `json:"created"`
	Base        Base       `json:"base"`
	Head        Head       `json:"head"`
	IndexDigest string     `json:"index_digest"`
	Options     Options    `json:"options"`
	Files       []Entry    `json:"files"`
	Excluded    []Excluded `json:"excluded,omitempty"`
	Digest      string     `json:"digest"`
}

// Base is the comparison point. Ref is what the caller named; MergeBase is
// the commit the candidate is compared against.
type Base struct {
	Ref       string `json:"ref"`
	Commit    string `json:"commit"`
	MergeBase string `json:"merge_base"`
}

// Head is the checked-out commit.
type Head struct {
	Commit   string `json:"commit"`
	Tree     string `json:"tree"`
	Detached bool   `json:"detached"`
}

// Entry is one path of the candidate: every tracked path, untracked file Git
// does not ignore, included input, and path deleted since the merge base.
// Mode, Size and SHA256 describe the working tree: a regular file's bytes
// and executable bit, or a symbolic link's target string. A path absent
// from the working tree has none of them. Base, Head and IndexDiffers come
// from Git and say which entries are changes; the hashes bind every byte
// whether or not Git reports a change.
type Entry struct {
	Path         string `json:"path"`
	Base         string `json:"base"`
	Head         string `json:"head"`
	IndexDiffers bool   `json:"index_differs,omitempty"`
	Included     bool   `json:"included,omitempty"`
	Mode         string `json:"mode,omitempty"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256,omitempty"`
}

// Changed reports whether Git sees the entry as a change against the merge
// base, HEAD or the index.
func (e Entry) Changed() bool {
	return e.Base != Unchanged || e.Head != Unchanged || e.IndexDiffers
}

// Excluded is a candidate path left out by a pattern.
type Excluded struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern"`
}

// Snapshot records the candidate in the repository at root. The candidate
// is the working tree; it is compared against the merge base of base and
// HEAD. An empty base means HEAD.
func Snapshot(root, base string, opts Options) (*Manifest, error) {
	if base == "" {
		base = "HEAD"
	}
	commit, err := git(root, "rev-parse", "--verify", "--end-of-options", base+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("base %q is not a commit: %w", base, err)
	}
	head, err := readHead(root)
	if err != nil {
		return nil, err
	}
	mb, err := git(root, "merge-base", commit, head.Commit)
	if err != nil {
		return nil, fmt.Errorf("no merge base between %s and HEAD (a shallow clone may need more history): %w", base, err)
	}
	opts, err = normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	m := &Manifest{
		Format:  ManifestFormat,
		Created: time.Now().UTC().Format(time.RFC3339),
		Base:    Base{Ref: base, Commit: commit, MergeBase: mb},
		Head:    head,
		Options: opts,
	}
	if m.IndexDigest, m.Files, m.Excluded, err = collect(root, mb, opts); err != nil {
		return nil, err
	}
	m.Digest = m.computeDigest()
	return m, nil
}

func normalizeOptions(o Options) (Options, error) {
	clean := func(in []string) []string {
		var out []string
		seen := map[string]bool{}
		for _, s := range in {
			s = filepath.ToSlash(s)
			if s != "" && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
		sort.Strings(out)
		return out
	}
	o = Options{Include: clean(o.Include), Exclude: clean(o.Exclude)}
	for _, inc := range o.Include {
		if path.IsAbs(inc) || inc == ".." || strings.HasPrefix(inc, "../") || path.Clean(inc) != inc || inc == "." {
			return o, fmt.Errorf("include %q must be a clean path relative to the repository root", inc)
		}
	}
	for _, p := range o.Exclude {
		if _, err := path.Match(strings.TrimSuffix(p, "/"), ""); err != nil {
			return o, fmt.Errorf("exclude pattern %q: %w", p, err)
		}
	}
	return o, nil
}

func readHead(root string) (Head, error) {
	c, err := git(root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return Head{}, fmt.Errorf("HEAD has no commit: %w", err)
	}
	t, err := git(root, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		return Head{}, err
	}
	_, symErr := git(root, "symbolic-ref", "-q", "HEAD")
	return Head{Commit: c, Tree: t, Detached: symErr != nil}, nil
}

// collect lists and hashes the candidate relative to mergeBase.
func collect(root, mergeBase string, opts Options) (string, []Entry, []Excluded, error) {
	index, err := gitBytes(root, "ls-files", "--stage", "-z")
	if err != nil {
		return "", nil, nil, err
	}
	inIndex := map[string]bool{}
	for _, rec := range strings.Split(strings.TrimSuffix(string(index), "\x00"), "\x00") {
		meta, p, _ := strings.Cut(rec, "\t")
		if f := strings.Fields(meta); len(f) == 3 {
			inIndex[p] = true
			if f[0] == "160000" {
				return "", nil, nil, fmt.Errorf("%s is a submodule; the helper does not snapshot submodules", p)
			}
			if f[2] != "0" {
				return "", nil, nil, fmt.Errorf("%s has an unresolved merge conflict", p)
			}
		}
	}
	tags, err := gitZ(root, "ls-files", "-v", "-z")
	if err != nil {
		return "", nil, nil, err
	}
	for _, rec := range tags {
		tag, p, _ := strings.Cut(rec, " ")
		if tag == "S" || tag == "s" || (tag != "" && tag != strings.ToUpper(tag)) {
			return "", nil, nil, fmt.Errorf("%s is marked skip-worktree or assume-unchanged, which hides its changes from Git; clear the flag (sparse checkouts are not supported)", p)
		}
	}
	sum := sha256.Sum256(index)
	indexDigest := "sha256:" + hex.EncodeToString(sum[:])

	vsBase, err := diffRaw(root, mergeBase)
	if err != nil {
		return "", nil, nil, err
	}
	vsHead, err := diffRaw(root, "HEAD")
	if err != nil {
		return "", nil, nil, err
	}
	staged, err := diffRaw(root, "--cached", "HEAD")
	if err != nil {
		return "", nil, nil, err
	}
	unstaged, err := diffRaw(root)
	if err != nil {
		return "", nil, nil, err
	}
	untracked, err := gitZ(root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", nil, nil, err
	}
	if err := refuseSpecialFiles(root, opts); err != nil {
		return "", nil, nil, err
	}

	paths := map[string]bool{}
	for p := range inIndex {
		paths[p] = true
	}
	for _, m := range []map[string]string{vsBase, vsHead, staged, unstaged} {
		for p := range m {
			paths[p] = true
		}
	}
	for _, p := range untracked {
		if strings.HasSuffix(p, "/") {
			return "", nil, nil, fmt.Errorf("untracked %s is a nested repository; the helper does not snapshot it", p)
		}
		paths[p] = true
	}
	included := map[string]bool{}
	for _, inc := range opts.Include {
		files, err := expandInclude(root, inc)
		if err != nil {
			return "", nil, nil, err
		}
		for _, p := range files {
			paths[p] = true
			included[p] = true
		}
	}

	var entries []Entry
	var excluded []Excluded
	for p := range paths {
		if pat := matchExclude(opts.Exclude, p); pat != "" {
			excluded = append(excluded, Excluded{Path: p, Pattern: pat})
			continue
		}
		e := Entry{Path: p, Base: status(vsBase, p), Head: status(vsHead, p), Included: included[p]}
		_, e.IndexDiffers = unstaged[p]
		if err := hashEntry(root, &e); err != nil {
			return "", nil, nil, err
		}
		if e.Mode != "" && !inIndex[p] {
			// Git diffs only tracked paths; a file outside the index is
			// identified by its hash alone.
			e.Base, e.Head = Untracked, Untracked
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	sort.Slice(excluded, func(i, j int) bool { return excluded[i].Path < excluded[j].Path })
	return indexDigest, entries, excluded, nil
}

// refuseSpecialFiles finds objects Git never lists, such as FIFOs and
// sockets, and refuses those that are neither ignored nor excluded, so they
// are not silently left out of the snapshot. Directories Git ignores as a
// whole and excluded directory prefixes are not entered, so their size or
// readability does not matter.
func refuseSpecialFiles(root string, opts Options) error {
	ignoredDirs := map[string]bool{}
	others, err := gitZ(root, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return err
	}
	for _, p := range others {
		if strings.HasSuffix(p, "/") {
			ignoredDirs[p] = true
		}
	}
	var special []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			if dir := filepath.ToSlash(rel) + "/"; isGitName(d.Name()) || ignoredDirs[dir] || excludedDir(opts.Exclude, dir) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); matchExclude(opts.Exclude, rel) == "" {
			special = append(special, rel)
		}
		return nil
	})
	if err != nil || len(special) == 0 {
		return err
	}
	ignored, err := checkIgnore(root, special)
	if err != nil {
		return err
	}
	for _, p := range special {
		if !ignored[p] {
			return fmt.Errorf("%s is a special file (such as a FIFO or socket) that Git does not ignore; the helper cannot identify it", p)
		}
	}
	return nil
}

// checkIgnore returns which of paths Git ignores.
func checkIgnore(root string, paths []string) (map[string]bool, error) {
	cmd := exec.Command("git", "-C", root, "check-ignore", "-z", "--stdin")
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	out, err := cmd.Output()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) { // 1: nothing ignored
		return nil, fmt.Errorf("git check-ignore: %w", err)
	}
	ignored := map[string]bool{}
	for _, p := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		ignored[p] = true
	}
	return ignored, nil
}

// status returns a path's status from a diffRaw map.
func status(m map[string]string, p string) string {
	if s, ok := m[p]; ok {
		return s
	}
	return Unchanged
}

// diffRaw runs git diff --raw with args and maps each reported path to its
// status. Renames appear as a deletion plus an addition.
func diffRaw(root string, args ...string) (map[string]string, error) {
	cmd := append([]string{"diff", "--raw", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none"}, args...)
	fields, err := gitZ(root, append(cmd, "--")...)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i := 0; i < len(fields); i += 2 {
		meta := fields[i]
		parts := strings.Fields(strings.TrimPrefix(meta, ":"))
		if i+1 >= len(fields) || !strings.HasPrefix(meta, ":") || len(parts) != 5 {
			return nil, fmt.Errorf("unexpected git diff --raw record %q", meta)
		}
		p := fields[i+1]
		switch parts[4][:1] {
		case "A":
			out[p] = Added
		case "D":
			out[p] = Deleted
		case "T":
			out[p] = TypeChanged
		case "M":
			out[p] = Modified
		case "U":
			return nil, fmt.Errorf("%s has an unresolved merge conflict", p)
		default:
			return nil, fmt.Errorf("%s: unsupported git diff status %q", p, parts[4])
		}
	}
	return out, nil
}

func expandInclude(root, inc string) ([]string, error) {
	if slices.ContainsFunc(strings.Split(inc, "/"), isGitName) {
		return nil, fmt.Errorf("include %q names Git administration data", inc)
	}
	if err := noLinkAncestors(root, inc); err != nil {
		return nil, fmt.Errorf("include: %w", err)
	}
	full := filepath.Join(root, filepath.FromSlash(inc))
	fi, err := os.Lstat(full)
	if err != nil {
		return nil, fmt.Errorf("include %q: %w", inc, err)
	}
	if !fi.IsDir() {
		return []string{inc}, nil
	}
	var out []string
	err = filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if isGitName(d.Name()) {
			// A directory or a linked worktree's .git file.
			return fmt.Errorf("include %q contains a nested repository at %s", inc, p)
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out, err
}

// excludedDir reports whether a directory prefix pattern (ending in "/")
// excludes everything beneath dir, itself ending in "/". Other patterns
// match individual paths only, so they never exclude a whole directory.
func excludedDir(patterns []string, dir string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool {
		return strings.HasSuffix(p, "/") && strings.HasPrefix(dir, p)
	})
}

// isGitName reports whether a path component names Git administration
// data, in any case, since a case-insensitive file system treats ".GIT" as
// ".git".
func isGitName(name string) bool { return strings.EqualFold(name, ".git") }

func matchExclude(patterns []string, p string) string {
	for _, pat := range patterns {
		if strings.HasSuffix(pat, "/") {
			if strings.HasPrefix(p, pat) {
				return pat
			}
			continue
		}
		if ok, _ := path.Match(pat, p); ok {
			return pat
		}
		if !strings.Contains(pat, "/") {
			if ok, _ := path.Match(pat, path.Base(p)); ok {
				return pat
			}
		}
	}
	return ""
}

// hashEntry fills mode, size and hash from the working tree, leaving them
// empty when the path does not exist.
func hashEntry(root string, e *Entry) error {
	full := filepath.Join(root, filepath.FromSlash(e.Path))
	fi, err := os.Lstat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", e.Path, err)
	}
	mode, size, sum, err := identify(full, fi)
	if err != nil {
		return fmt.Errorf("%s: %w", e.Path, err)
	}
	e.Mode, e.Size, e.SHA256 = mode, size, sum
	return nil
}

// identify returns the Git-style mode, size and SHA-256 of a regular file's
// bytes or a symbolic link's target string.
func identify(full string, fi fs.FileInfo) (mode string, size int64, sum string, err error) {
	h := sha256.New()
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return "", 0, "", err
		}
		mode, size = "120000", int64(len(target))
		h.Write([]byte(target))
	case fi.Mode().IsRegular():
		mode = "100644"
		if fi.Mode()&0o111 != 0 {
			mode = "100755"
		}
		f, err := os.Open(full)
		if err != nil {
			return "", 0, "", err
		}
		size, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", 0, "", err
		}
	case fi.IsDir():
		return "", 0, "", errors.New("is a directory where Git expects a file (a nested repository or a type change Git cannot represent)")
	default:
		return "", 0, "", fmt.Errorf("unsupported file type %s", fi.Mode().Type())
	}
	return mode, size, hex.EncodeToString(h.Sum(nil)), nil
}

// computeDigest hashes everything that identifies the candidate; Created
// and Digest itself are left out.
func (m *Manifest) computeDigest() string {
	b, _ := json.Marshal(struct {
		Format      string
		Base        Base
		Head        Head
		IndexDigest string
		Options     Options
		Files       []Entry
		Excluded    []Excluded
	}{m.Format, m.Base, m.Head, m.IndexDigest, m.Options, m.Files, m.Excluded})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Drift lists how the working tree differs from a manifest.
type Drift struct {
	HeadMoved    string   `json:"head_moved,omitempty"`
	IndexChanged bool     `json:"index_changed,omitempty"`
	Added        []string `json:"added,omitempty"`
	Removed      []string `json:"removed,omitempty"`
	Changed      []string `json:"changed,omitempty"`
}

// None reports whether nothing drifted.
func (d *Drift) None() bool {
	return d.HeadMoved == "" && !d.IndexChanged && len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0
}

func (d *Drift) String() string {
	if d.None() {
		return "no drift"
	}
	var parts []string
	if d.HeadMoved != "" {
		parts = append(parts, "HEAD moved to "+d.HeadMoved)
	}
	if d.IndexChanged {
		parts = append(parts, "index changed")
	}
	for _, g := range []struct {
		label string
		paths []string
	}{{"added", d.Added}, {"removed", d.Removed}, {"changed", d.Changed}} {
		if len(g.paths) > 0 {
			parts = append(parts, g.label+": "+strings.Join(g.paths, ", "))
		}
	}
	return strings.Join(parts, "; ")
}

// Verify recomputes m against the working tree at root, using the recorded
// merge base and options, and reports drift.
func Verify(root string, m *Manifest) (*Drift, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	head, err := readHead(root)
	if err != nil {
		return nil, err
	}
	d := &Drift{}
	if head.Commit != m.Head.Commit {
		d.HeadMoved = head.Commit
	}
	if _, err := git(root, "cat-file", "-e", m.Base.MergeBase+"^{commit}"); err != nil {
		return nil, fmt.Errorf("recorded merge base %s is missing from this repository: %w", m.Base.MergeBase, err)
	}
	index, files, _, err := collect(root, m.Base.MergeBase, m.Options)
	if err != nil {
		return nil, err
	}
	d.IndexChanged = index != m.IndexDigest
	d.Added, d.Removed, d.Changed = compareEntries(m.Files, files)
	return d, nil
}

// compareEntries reports paths only in now, only in was, and in both with
// a different status or identity.
func compareEntries(was, now []Entry) (added, removed, changed []string) {
	old := map[string]Entry{}
	for _, e := range was {
		old[e.Path] = e
	}
	seen := map[string]bool{}
	for _, e := range now {
		seen[e.Path] = true
		o, ok := old[e.Path]
		switch {
		case !ok:
			added = append(added, e.Path)
		case o != e:
			changed = append(changed, e.Path)
		}
	}
	for _, e := range was {
		if !seen[e.Path] {
			removed = append(removed, e.Path)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return added, removed, changed
}

func (m *Manifest) check() error {
	if m.Format != ManifestFormat {
		return fmt.Errorf("manifest format %q, want %q", m.Format, ManifestFormat)
	}
	if got := m.computeDigest(); got != m.Digest {
		return fmt.Errorf("manifest digest %s does not match its content (%s); it was edited after the snapshot", m.Digest, got)
	}
	return nil
}

// ReadManifest loads and checks a manifest file.
func ReadManifest(file string) (*Manifest, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if err := m.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &m, nil
}

// WriteJSON writes v as indented JSON to a new file outside the repository
// at root. It refuses to replace an existing file.
func WriteJSON(root, file string, v any) error {
	dest, err := OutsideRepo(root, file)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeNew(dest, append(b, '\n'))
}

// writeNew creates dest with b, failing if it exists.
func writeNew(dest string, b []byte) error {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
