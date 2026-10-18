package devreview

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Materialize writes a read-only copy of the candidate described by m to
// dir, which must be outside the repository and not exist yet. The copy is
// a detached Git worktree at the manifest's HEAD with every entry applied,
// so a reviewer can run Git there; its registration lives in the
// repository's Git directory and is removed with `git worktree remove
// --force <dir>`. It returns the resolved copy path.
func Materialize(root string, m *Manifest, dir string) (string, error) {
	d, err := Verify(root, m)
	if err != nil {
		return "", err
	}
	if !d.None() {
		return "", fmt.Errorf("the working tree drifted from the snapshot: %s", d)
	}
	dest, err := OutsideRepo(root, dir)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(dest); !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s already exists", dest)
	}
	if _, err := git(root, "worktree", "add", "--detach", "--quiet", dest, m.Head.Commit); err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		_ = makeWritable(dest)
		_, _ = git(root, "worktree", "remove", "--force", dest)
		return "", err
	}
	if err := checkLinks(dest); err != nil {
		return fail(err)
	}
	for _, e := range m.Files {
		if err := applyEntry(root, dest, e); err != nil {
			return fail(err)
		}
	}
	if err := CheckCopy(dest, m); err != nil {
		return fail(err)
	}
	if err := makeReadOnly(dest); err != nil {
		return fail(err)
	}
	return dest, nil
}

// applyEntry makes the copy's path match the working tree's entry, then
// checks the copy against the manifest so a change during copying fails.
func applyEntry(root, dest string, e Entry) error {
	src := filepath.Join(root, filepath.FromSlash(e.Path))
	dst := filepath.Join(dest, filepath.FromSlash(e.Path))
	if err := noLinkAncestors(dest, e.Path); err != nil {
		return err
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if e.Mode == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	switch e.Mode {
	case "120000":
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
	default:
		perm := os.FileMode(0o644)
		if e.Mode == "100755" {
			perm = 0o755
		}
		if err := copyFile(src, dst, perm); err != nil {
			return err
		}
	}
	return matchEntry(dest, e)
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm)
}

// matchEntry checks one path of a copy against its manifest entry.
func matchEntry(dir string, e Entry) error {
	full := filepath.Join(dir, filepath.FromSlash(e.Path))
	fi, err := os.Lstat(full)
	if e.Mode == "" {
		if err == nil {
			return fmt.Errorf("%s exists in the copy but not in the snapshot", e.Path)
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err != nil {
		return fmt.Errorf("%s: %w", e.Path, err)
	}
	mode, size, sum, err := identify(full, fi)
	if err != nil {
		return fmt.Errorf("%s: %w", e.Path, err)
	}
	if mode != e.Mode || size != e.Size || sum != e.SHA256 {
		return fmt.Errorf("%s in the copy differs from the snapshot", e.Path)
	}
	return nil
}

// CheckCopy reports whether a materialized copy still matches m: HEAD is
// the manifest's, every entry matches, nothing else differs from HEAD, and
// no symbolic link leaves the copy.
func CheckCopy(dir string, m *Manifest) error {
	if err := m.check(); err != nil {
		return err
	}
	head, err := git(dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	if head != m.Head.Commit {
		return fmt.Errorf("copy HEAD is %s, snapshot HEAD is %s", head, m.Head.Commit)
	}
	entries := map[string]bool{}
	for _, e := range m.Files {
		entries[e.Path] = true
		if err := matchEntry(dir, e); err != nil {
			return err
		}
	}
	recs, err := gitZ(dir, "status", "--porcelain=v1", "-z", "--no-renames", "--untracked-files=all", "--ignored=matching", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if len(rec) < 4 {
			return fmt.Errorf("unexpected git status record %q", rec)
		}
		p := rec[3:]
		if strings.HasSuffix(p, "/") {
			if err := filesUnder(dir, p, entries); err != nil {
				return err
			}
			continue
		}
		if !entries[p] {
			return fmt.Errorf("%s in the copy differs from HEAD but is not in the snapshot", p)
		}
	}
	return checkLinks(dir)
}

// filesUnder requires every file beneath a directory Git reports as a
// whole to be a manifest entry.
func filesUnder(dir, prefix string, entries map[string]bool) error {
	return filepath.WalkDir(filepath.Join(dir, filepath.FromSlash(prefix)), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); !entries[rel] {
			return fmt.Errorf("%s in the copy is not in the snapshot", rel)
		}
		return nil
	})
}

// checkLinks refuses symbolic links that resolve outside dir, following
// intermediate links as the file system does.
func checkLinks(dir string) error {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	var bad []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" && p != dir {
			return filepath.SkipDir
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(p)
		if err != nil {
			return err
		}
		res, err := resolvePath(p)
		if err != nil || filepath.IsAbs(target) || !within(dir, res) {
			rel, _ := filepath.Rel(dir, p)
			bad = append(bad, filepath.ToSlash(rel)+" -> "+target)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("symbolic links leave the copy, so reads through them are not bound to the snapshot: %s", strings.Join(bad, ", "))
	}
	return nil
}

func makeReadOnly(dir string) error {
	return chmodFiles(dir, func(m fs.FileMode) fs.FileMode { return m &^ 0o222 })
}

func makeWritable(dir string) error {
	return chmodFiles(dir, func(m fs.FileMode) fs.FileMode { return m | 0o200 })
}

// chmodFiles changes the permission bits of every regular file under dir.
func chmodFiles(dir string, f func(fs.FileMode) fs.FileMode) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		return os.Chmod(p, f(fi.Mode().Perm()))
	})
}
