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
// a detached Git worktree whose index is the manifest's HEAD, so a reviewer
// can run Git there, but its files are written from the manifest only: it
// holds exactly the manifest's files and their bytes. Its registration
// lives in the repository's Git directory and is removed with `git worktree
// remove --force <dir>`. It returns the resolved copy path.
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
	if _, err := git(root, "worktree", "add", "--detach", "--no-checkout", "--quiet", dest, m.Head.Commit); err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		_ = makeWritable(dest)
		_, _ = git(root, "worktree", "remove", "--force", dest)
		return "", err
	}
	if _, err := git(dest, "read-tree", "HEAD"); err != nil {
		return fail(err)
	}
	for _, e := range m.Files {
		if err := writeEntry(root, dest, e); err != nil {
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

// writeEntry writes one manifest entry from the working tree into the copy
// and checks it, so a change during copying fails. It never writes through
// a symbolic link inside the copy.
func writeEntry(root, dest string, e Entry) error {
	if e.Mode == "" {
		return nil
	}
	if err := noLinkAncestors(dest, e.Path); err != nil {
		return err
	}
	src := filepath.Join(root, filepath.FromSlash(e.Path))
	dst := filepath.Join(dest, filepath.FromSlash(e.Path))
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
// the manifest's, the copy holds exactly the manifest's files with their
// recorded identity, and no symbolic link resolves outside it. It reads the
// files themselves and does not rely on Git's change detection.
func CheckCopy(dir string, m *Manifest) error {
	if err := m.check(); err != nil {
		return err
	}
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	head, err := git(dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	if head != m.Head.Commit {
		return fmt.Errorf("copy HEAD is %s, snapshot HEAD is %s", head, m.Head.Commit)
	}
	present := map[string]bool{}
	for _, e := range m.Files {
		if err := matchEntry(dir, e); err != nil {
			return err
		}
		present[e.Path] = e.Mode != ""
	}
	gitFile := filepath.Join(dir, ".git")
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == gitFile || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); !present[rel] {
			return fmt.Errorf("%s in the copy is not in the snapshot", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return checkLinks(dir)
}

// checkLinks refuses symbolic links that resolve outside dir, following
// intermediate links as the file system does. Containment is compared
// exactly, so a differently cased sibling never counts as inside.
func checkLinks(dir string) error {
	var bad []string
	gitFile := filepath.Join(dir, ".git")
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == gitFile || d.Type()&fs.ModeSymlink == 0 {
			return err
		}
		target, err := os.Readlink(p)
		if err != nil {
			return err
		}
		res, err := resolvePath(p)
		if err != nil || filepath.IsAbs(target) || !withinExact(dir, res) {
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
