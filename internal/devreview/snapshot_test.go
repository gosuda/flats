package devreview

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repo creates a Git repository with files committed on main and returns
// its resolved root.
func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run(t, root, "git", "init", "-q", "-b", "main")
	write(t, root, files)
	run(t, root, "git", "add", "-A")
	commit(t, root, "initial")
	return root
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, root, msg string) {
	t.Helper()
	run(t, root, "git", "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false",
		"commit", "-q", "--allow-empty", "-m", msg)
}

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func entry(t *testing.T, m *Manifest, p string) Entry {
	t.Helper()
	for _, e := range m.Files {
		if e.Path == p {
			return e
		}
	}
	t.Fatalf("no manifest entry for %s; have %v", p, paths(m))
	return Entry{}
}

func paths(m *Manifest) []string {
	var out []string
	for _, e := range m.Files {
		out = append(out, e.Path)
	}
	return out
}

func changed(m *Manifest) []string {
	var out []string
	for _, e := range m.Files {
		if e.Changed() {
			out = append(out, e.Path)
		}
	}
	return out
}

func snap(t *testing.T, root, base string, o Options) *Manifest {
	t.Helper()
	m, err := Snapshot(root, base, o)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSnapshotCandidateSet(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{
		"keep.txt": "keep\n", "edit.txt": "v1\n", "gone.txt": "gone\n", "renamed.txt": "move me\n",
		".gitignore": "*.log\nbuild/\n",
	})
	run(t, root, "git", "switch", "-q", "-c", "topic")
	write(t, root, map[string]string{"edit.txt": "v2\n", "added.txt": "new\n"})
	run(t, root, "git", "rm", "-q", "gone.txt")
	run(t, root, "git", "mv", "renamed.txt", "moved.txt")
	run(t, root, "git", "add", "-A")
	commit(t, root, "topic")
	write(t, root, map[string]string{
		"keep.txt": "unstaged\n", "untracked.txt": "u\n", "noise.log": "log\n", "build/out.bin": "bin\n",
	})

	m := snap(t, root, "main", Options{Include: []string{"build"}})
	want := []string{"added.txt", "build/out.bin", "edit.txt", "gone.txt", "keep.txt", "moved.txt", "renamed.txt", "untracked.txt"}
	if got := changed(m); !slices.Equal(got, want) {
		t.Fatalf("changed paths = %v, want %v", got, want)
	}
	if got := paths(m); !slices.Equal(got, append([]string{".gitignore"}, want...)) {
		t.Fatalf("entries = %v: every tracked file and no ignored noise.log", got)
	}
	if e := entry(t, m, ".gitignore"); e.Changed() || e.SHA256 == "" {
		t.Errorf("unchanged tracked file = %+v, want unchanged with its hash bound", e)
	}
	checks := []struct {
		path, base, head string
		mode             string
	}{
		{"added.txt", Added, Unchanged, "100644"},
		{"edit.txt", Modified, Unchanged, "100644"},
		{"gone.txt", Deleted, Unchanged, ""},
		{"renamed.txt", Deleted, Unchanged, ""},
		{"moved.txt", Added, Unchanged, "100644"},
		{"keep.txt", Modified, Modified, "100644"},
		{"untracked.txt", Untracked, Untracked, "100644"},
		{"build/out.bin", Untracked, Untracked, "100644"},
	}
	for _, c := range checks {
		e := entry(t, m, c.path)
		if e.Base != c.base || e.Head != c.head || e.Mode != c.mode {
			t.Errorf("%s: base %s head %s mode %q, want %s %s %q", c.path, e.Base, e.Head, e.Mode, c.base, c.head, c.mode)
		}
	}
	if !entry(t, m, "build/out.bin").Included {
		t.Error("build/out.bin should be marked included")
	}
	if e := entry(t, m, "gone.txt"); e.SHA256 != "" || e.Size != 0 {
		t.Errorf("deleted entry has identity %+v", e)
	}
	if m.Base.MergeBase != run(t, root, "git", "rev-parse", "main") || m.Head.Commit != run(t, root, "git", "rev-parse", "HEAD") {
		t.Errorf("base/head = %+v %+v", m.Base, m.Head)
	}
}

// A committed change that the working tree reverts, and a staged change an
// unstaged edit masks, are both invisible to a merge-base-only diff.
func TestSnapshotListsRevertedAndMaskedChanges(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{"reverted.txt": "A\n", "masked.txt": "A\n"})
	run(t, root, "git", "switch", "-q", "-c", "topic")
	write(t, root, map[string]string{"reverted.txt": "B\n"})
	run(t, root, "git", "add", "-A")
	commit(t, root, "B")
	write(t, root, map[string]string{"reverted.txt": "A\n", "masked.txt": "staged\n"})
	run(t, root, "git", "add", "masked.txt")
	write(t, root, map[string]string{"masked.txt": "A\n"})

	m := snap(t, root, "main", Options{})
	if e := entry(t, m, "reverted.txt"); e.Base != Unchanged || e.Head != Modified {
		t.Errorf("reverted.txt = %+v, want unchanged against base, modified against HEAD", e)
	}
	if e := entry(t, m, "masked.txt"); !e.IndexDiffers || e.Head != Unchanged {
		t.Errorf("masked.txt = %+v, want index_differs and unchanged against HEAD", e)
	}
}

func TestVerifyReportsDrift(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	write(t, root, map[string]string{"a.txt": "a2\n", "new.txt": "n\n"})
	m := snap(t, root, "", Options{Exclude: []string{"*.out"}})

	drift := func() *Drift {
		t.Helper()
		d, err := Verify(root, m)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := drift(); !d.None() {
		t.Fatalf("fresh snapshot drifted: %s", d)
	}
	write(t, root, map[string]string{"probe.out": "excluded output\n"})
	if d := drift(); !d.None() {
		t.Errorf("an excluded new file drifted: %s", d)
	}
	write(t, root, map[string]string{"a.txt": "a3\n", "c.txt": "c\n"})
	if err := os.Remove(filepath.Join(root, "new.txt")); err != nil {
		t.Fatal(err)
	}
	d := drift()
	if !slices.Equal(d.Changed, []string{"a.txt"}) || !slices.Equal(d.Added, []string{"c.txt"}) || !slices.Equal(d.Removed, []string{"new.txt"}) {
		t.Errorf("drift = %s", d)
	}
	write(t, root, map[string]string{"a.txt": "a2\n", "new.txt": "n\n"})
	os.Remove(filepath.Join(root, "c.txt"))
	if d := drift(); !d.None() {
		t.Fatalf("restored tree drifted: %s", d)
	}
	if err := os.Chmod(filepath.Join(root, "a.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if d := drift(); !slices.Equal(d.Changed, []string{"a.txt"}) {
		t.Errorf("mode change: drift = %s", d)
	}
	os.Chmod(filepath.Join(root, "a.txt"), 0o644)
	run(t, root, "git", "add", "a.txt")
	if d := drift(); !d.IndexChanged {
		t.Errorf("staging did not drift: %s", d)
	}
	commit(t, root, "move head")
	if d := drift(); d.HeadMoved == "" {
		t.Errorf("a new commit did not drift: %s", d)
	}
}

func TestSymlinkIdentityIsTarget(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{"one.txt": "same\n", "two.txt": "same\n"})
	if err := os.Symlink("one.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing.txt", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	m := snap(t, root, "", Options{})
	if e := entry(t, m, "link"); e.Mode != "120000" || e.Size != int64(len("one.txt")) {
		t.Errorf("link entry = %+v", e)
	}
	entry(t, m, "dangling")
	os.Remove(filepath.Join(root, "link"))
	if err := os.Symlink("two.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	d, err := Verify(root, m)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.Changed, []string{"link"}) {
		t.Errorf("retargeting to identical content: drift = %s", d)
	}
}

func TestSnapshotRefusesUnidentifiableObjects(t *testing.T) {
	t.Parallel()
	t.Run("submodule", func(t *testing.T) {
		root := repo(t, map[string]string{"a.txt": "a\n"})
		head := run(t, root, "git", "rev-parse", "HEAD")
		run(t, root, "git", "update-index", "--add", "--cacheinfo", "160000,"+head+",sub")
		commit(t, root, "add gitlink")
		if _, err := Snapshot(root, "", Options{}); err == nil || !strings.Contains(err.Error(), "submodule") {
			t.Fatalf("err = %v, want a submodule refusal even when unchanged", err)
		}
	})
	t.Run("nested repository", func(t *testing.T) {
		root := repo(t, map[string]string{"a.txt": "a\n"})
		nested := filepath.Join(root, "nested")
		os.Mkdir(nested, 0o755)
		run(t, nested, "git", "init", "-q")
		write(t, nested, map[string]string{"x.txt": "x\n"})
		if _, err := Snapshot(root, "", Options{}); err == nil || !strings.Contains(err.Error(), "nested repository") {
			t.Fatalf("err = %v, want a nested repository refusal", err)
		}
	})
	t.Run("unresolved conflict", func(t *testing.T) {
		root := repo(t, map[string]string{"a.txt": "base\n"})
		run(t, root, "git", "switch", "-q", "-c", "other")
		write(t, root, map[string]string{"a.txt": "other\n"})
		run(t, root, "git", "add", "-A")
		commit(t, root, "other")
		run(t, root, "git", "switch", "-q", "main")
		write(t, root, map[string]string{"a.txt": "main\n"})
		run(t, root, "git", "add", "-A")
		commit(t, root, "main")
		merge := exec.Command("git", "-c", "user.name=test", "-c", "user.email=test@example.com", "merge", "-q", "other")
		merge.Dir = root
		if merge.Run() == nil {
			t.Fatal("the merge did not conflict")
		}
		if _, err := Snapshot(root, "", Options{}); err == nil || !strings.Contains(err.Error(), "conflict") {
			t.Fatalf("err = %v, want a conflict refusal", err)
		}
	})
	t.Run("no merge base", func(t *testing.T) {
		root := repo(t, map[string]string{"a.txt": "a\n"})
		run(t, root, "git", "switch", "-q", "--orphan", "other")
		commit(t, root, "unrelated")
		if _, err := Snapshot(root, "main", Options{}); err == nil || !strings.Contains(err.Error(), "merge base") {
			t.Fatalf("err = %v, want a merge base error", err)
		}
	})
	t.Run("include with a linked worktree", func(t *testing.T) {
		root := repo(t, map[string]string{"a.txt": "a\n", ".gitignore": "nested/\n"})
		run(t, root, "git", "worktree", "add", "-q", "--detach", filepath.Join(root, "nested"))
		for _, inc := range []string{"nested", "nested/.git", "nested/.GIT"} {
			if _, err := Snapshot(root, "", Options{Include: []string{inc}}); err == nil {
				t.Errorf("include %q with a .git file was accepted", inc)
			}
		}
	})
	t.Run("include with a case variant of .git", func(t *testing.T) {
		root := repo(t, map[string]string{"a.txt": "a\n", ".gitignore": "vendor/\n"})
		write(t, root, map[string]string{"vendor/sub/.Git": "gitdir: /elsewhere\n"})
		if _, err := Snapshot(root, "", Options{Include: []string{"vendor"}}); err == nil {
			t.Error("a .Git file beneath an include was accepted")
		}
	})
	t.Run("bad include", func(t *testing.T) {
		root := repo(t, map[string]string{"a.txt": "a\n"})
		for _, inc := range []string{"../x", "/etc", "./a.txt", "."} {
			if _, err := Snapshot(root, "", Options{Include: []string{inc}}); err == nil {
				t.Errorf("include %q accepted", inc)
			}
		}
	})
}

func TestOptionsBindIdentity(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{"a.txt": "a\n", ".gitignore": "*.log\n"})
	write(t, root, map[string]string{"a.txt": "b\n", "x.log": "log\n"})
	plain := snap(t, root, "", Options{})
	other := snap(t, root, "", Options{Exclude: []string{"*.out"}})
	if plain.Digest == other.Digest {
		t.Error("a different exclusion policy kept the same digest")
	}
	m := snap(t, root, "", Options{Include: []string{"x.log"}, Exclude: []string{"*.log"}})
	if slices.Contains(paths(m), "x.log") || len(m.Excluded) != 1 || m.Excluded[0].Pattern != "*.log" {
		t.Errorf("exclusion did not take precedence over include: files %v excluded %v", paths(m), m.Excluded)
	}
	if got := matchExclude([]string{"logs/"}, "logs/a/b.txt"); got != "logs/" {
		t.Errorf("directory prefix pattern matched %q", got)
	}
	if got := matchExclude([]string{"docs/*.md"}, "a/docs/x.md"); got != "" {
		t.Errorf("a pattern with a slash matched a base name: %q", got)
	}
}

func TestManifestRejectsEdits(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{"a.txt": "a\n"})
	write(t, root, map[string]string{"a.txt": "b\n"})
	dir := t.TempDir()
	file := filepath.Join(dir, "snap.json")
	if err := WriteJSON(root, file, snap(t, root, "", Options{})); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(root, file, snap(t, root, "", Options{})); err == nil {
		t.Error("WriteJSON replaced an existing manifest")
	}
	if _, err := ReadManifest(file); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	os.WriteFile(file, []byte(strings.Replace(string(b), `"modified"`, `"unchanged"`, 1)), 0o600)
	if _, err := ReadManifest(file); err == nil || !strings.Contains(err.Error(), "edited") {
		t.Errorf("err = %v, want a digest mismatch", err)
	}
}

func TestOutsideRepo(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{"a.txt": "a\n"})
	out, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(out, "into-repo")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		root,
		filepath.Join(root, "new", "deeper", "report.md"),
		filepath.Join(out, "into-repo", "report.md"),
		filepath.Join(out, "into-repo", "not", "yet", "there.json"),
		strings.ToUpper(root[:2]) + root[2:] + "/x", // case variant on case-insensitive file systems
	} {
		if _, err := OutsideRepo(root, p); !errors.Is(err, ErrInsideRepo) {
			t.Errorf("OutsideRepo(%s) err = %v, want ErrInsideRepo", p, err)
		}
	}
	if got, err := OutsideRepo(root, filepath.Join(out, "evidence", "snap.json")); err != nil || got != filepath.Join(out, "evidence", "snap.json") {
		t.Errorf("outside path: %q, %v", got, err)
	}
	if _, err := OutsideRepo(root, root+"-sibling/x"); err != nil {
		t.Errorf("a sibling directory sharing the name prefix was refused: %v", err)
	}
}

// Index flags and settings that make Git ignore a change must not let a
// changed file slip out of the snapshot.
func TestSnapshotCannotHideChanges(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--skip-worktree", "--assume-unchanged"} {
		t.Run(flag, func(t *testing.T) {
			root := repo(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
			run(t, root, "git", "update-index", flag, "b.txt")
			write(t, root, map[string]string{"b.txt": "hidden change\n"})
			if _, err := Snapshot(root, "", Options{}); err == nil || !strings.Contains(err.Error(), "b.txt") {
				t.Fatalf("err = %v, want a refusal naming b.txt", err)
			}
		})
	}
	t.Run("core.fileMode=false", func(t *testing.T) {
		root := repo(t, map[string]string{"a.txt": "a\n"})
		run(t, root, "git", "config", "core.fileMode", "false")
		m := snap(t, root, "", Options{})
		if c := changed(m); len(c) != 0 {
			t.Fatalf("clean tree has changes %v", c)
		}
		os.Chmod(filepath.Join(root, "a.txt"), 0o755)
		if e := entry(t, snap(t, root, "", Options{}), "a.txt"); e.Mode != "100755" {
			t.Errorf("a.txt = %+v", e)
		}
		if d, err := Verify(root, m); err != nil || !slices.Equal(d.Changed, []string{"a.txt"}) {
			t.Errorf("drift = %v, %v", d, err)
		}
	})
	t.Run("include beneath a link", func(t *testing.T) {
		root := repo(t, map[string]string{"a.txt": "a\n"})
		ext := outside(t)
		write(t, ext, map[string]string{"file": "outside\n"})
		os.Symlink(ext, filepath.Join(root, "link"))
		if _, err := Snapshot(root, "", Options{Include: []string{"link/file"}}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("err = %v", err)
		}
	})
}

// Every file's bytes are bound, so a change Git does not report still
// drifts: line-ending normalization, or an edit that keeps size and
// timestamps.
func TestVerifyBindsBytesGitIgnores(t *testing.T) {
	t.Parallel()
	t.Run("normalized line endings", func(t *testing.T) {
		root := repo(t, map[string]string{"a.txt": "one\ntwo\n", ".gitattributes": "*.txt text\n"})
		run(t, root, "git", "config", "core.autocrlf", "true")
		m := snap(t, root, "", Options{})
		write(t, root, map[string]string{"a.txt": "one\r\ntwo\r\n"})
		d, err := Verify(root, m)
		if err != nil || !slices.Contains(d.Changed, "a.txt") {
			t.Errorf("drift = %v, %v", d, err)
		}
	})
	t.Run("same size and timestamps", func(t *testing.T) {
		root := repo(t, map[string]string{"a.txt": "aaaa\n"})
		m := snap(t, root, "", Options{})
		p := filepath.Join(root, "a.txt")
		fi, _ := os.Stat(p)
		os.WriteFile(p, []byte("bbbb\n"), 0o644)
		os.Chtimes(p, fi.ModTime(), fi.ModTime())
		d, err := Verify(root, m)
		if err != nil || !slices.Equal(d.Changed, []string{"a.txt"}) {
			t.Errorf("drift = %v, %v", d, err)
		}
	})
}
