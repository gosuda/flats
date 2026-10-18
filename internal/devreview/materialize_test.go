package devreview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func outside(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// cleanupCopy makes a copy writable so the test's temporary directories
// can be removed, and unregisters its worktree.
func cleanupCopy(t *testing.T, root, dir string) {
	t.Cleanup(func() {
		makeWritable(dir)
		git(root, "worktree", "remove", "--force", dir)
	})
}

func TestMaterialize(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{"a.txt": "a\n", "gone.txt": "g\n", "dir/link-target.txt": "t\n", ".gitignore": "*.log\n"})
	if err := os.Symlink("link-target.txt", filepath.Join(root, "dir", "rel-link")); err != nil {
		t.Fatal(err)
	}
	write(t, root, map[string]string{"a.txt": "changed\n", "new.txt": "n\n", "in.log": "ignored input\n", "noise.log": "x\n"})
	os.Remove(filepath.Join(root, "gone.txt"))
	os.Chmod(filepath.Join(root, "new.txt"), 0o755)
	m := snap(t, root, "", Options{Include: []string{"in.log"}})

	dest := filepath.Join(outside(t), "copy")
	cleanupCopy(t, root, dest)
	got, err := Materialize(root, m, dest)
	if err != nil {
		t.Fatal(err)
	}
	if got != dest {
		t.Errorf("copy path %s, want %s", got, dest)
	}
	for name, want := range map[string]string{"a.txt": "changed\n", "new.txt": "n\n", "in.log": "ignored input\n"} {
		b, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || string(b) != want {
			t.Errorf("%s in copy = %q, %v", name, b, err)
		}
	}
	for _, name := range []string{"gone.txt", "noise.log"} {
		if _, err := os.Lstat(filepath.Join(dest, name)); !os.IsNotExist(err) {
			t.Errorf("%s exists in the copy", name)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dest, "new.txt")); fi.Mode().Perm()&0o111 == 0 || fi.Mode().Perm()&0o222 != 0 {
		t.Errorf("new.txt mode %v, want executable and read-only", fi.Mode())
	}
	if err := CheckCopy(dest, m); err != nil {
		t.Fatalf("fresh copy: %v", err)
	}

	// A write to an entry or a new file is drift.
	p := filepath.Join(dest, "a.txt")
	os.Chmod(p, 0o644)
	os.WriteFile(p, []byte("tampered\n"), 0o644)
	if err := CheckCopy(dest, m); err == nil {
		t.Error("a changed entry passed")
	}
	os.WriteFile(p, []byte("changed\n"), 0o644)
	if err := CheckCopy(dest, m); err != nil {
		t.Fatalf("restored copy: %v", err)
	}
	os.WriteFile(filepath.Join(dest, "dir", "extra.txt"), []byte("x\n"), 0o644)
	if err := CheckCopy(dest, m); err == nil || !strings.Contains(err.Error(), "extra.txt") {
		t.Errorf("an extra file passed: %v", err)
	}
	os.Remove(filepath.Join(dest, "dir", "extra.txt"))
	os.WriteFile(filepath.Join(dest, "later.log"), []byte("x\n"), 0o644)
	if err := CheckCopy(dest, m); err == nil {
		t.Error("an extra ignored file passed")
	}
}

func TestMaterializeRefusesEscapingLinks(t *testing.T) {
	t.Parallel()
	for name, target := range map[string]func(root string) string{
		"absolute": func(root string) string { return filepath.Join(root, "a.txt") },
		"climbing": func(string) string { return "../../outside.txt" },
	} {
		t.Run(name, func(t *testing.T) {
			root := repo(t, map[string]string{"a.txt": "a\n", "sub/keep.txt": "k\n"})
			if err := os.Symlink(target(root), filepath.Join(root, "sub", "link")); err != nil {
				t.Fatal(err)
			}
			m := snap(t, root, "", Options{})
			dest := filepath.Join(outside(t), "copy")
			cleanupCopy(t, root, dest)
			if _, err := Materialize(root, m, dest); err == nil || !strings.Contains(err.Error(), "leave the copy") {
				t.Fatalf("err = %v, want an escaping link refusal", err)
			}
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Error("a refused copy was left behind")
			}
			if list, _ := git(root, "worktree", "list"); strings.Contains(list, dest) {
				t.Error("a refused copy stayed registered as a worktree")
			}
		})
	}
}

func TestMaterializeRefusesDriftAndInsidePaths(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{"a.txt": "a\n"})
	write(t, root, map[string]string{"a.txt": "b\n"})
	m := snap(t, root, "", Options{})
	if _, err := Materialize(root, m, filepath.Join(root, "copy")); err == nil {
		t.Error("a copy inside the repository was accepted")
	}
	write(t, root, map[string]string{"a.txt": "c\n"})
	if _, err := Materialize(root, m, filepath.Join(outside(t), "copy")); err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Errorf("err = %v, want drift", err)
	}
}

// Links that leave the copy only through an intermediate link, and links in
// HEAD that point outside, are refused before anything outside is touched.
func TestMaterializeResolvesIntermediateLinks(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{"a.txt": "a\n", "sub/keep.txt": "k\n"})
	os.Symlink(".", filepath.Join(root, "self"))
	os.Symlink("../self/../outside.txt", filepath.Join(root, "sub", "link"))
	if res, _ := resolvePath(filepath.Join(root, "sub", "link")); within(root, res) {
		t.Fatalf("resolvePath = %s, want a path outside %s", res, root)
	}
	m := snap(t, root, "", Options{})
	dest := filepath.Join(outside(t), "copy")
	cleanupCopy(t, root, dest)
	if _, err := Materialize(root, m, dest); err == nil || !strings.Contains(err.Error(), "sub/link") {
		t.Fatalf("err = %v, want sub/link refused", err)
	}
}

func TestWriteEntryNeverWritesThroughLinks(t *testing.T) {
	t.Parallel()
	ext := outside(t)
	write(t, ext, map[string]string{"file": "precious\n"})
	root := repo(t, map[string]string{"link/file": "candidate\n"})
	m := snap(t, root, "", Options{Include: []string{"link/file"}})
	dest := outside(t)
	os.Symlink(ext, filepath.Join(dest, "link"))
	if err := writeEntry(root, dest, entry(t, m, "link/file")); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if b, err := os.ReadFile(filepath.Join(ext, "file")); err != nil || string(b) != "precious\n" {
		t.Errorf("outside file: %q, %v", b, err)
	}
}

// A candidate that deletes or replaces an escaping link in HEAD can be
// materialized: the copy holds only the candidate's files.
func TestMaterializeCandidateRemovingEscapingLink(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{"a.txt": "a\n"})
	os.Symlink("/etc/hosts", filepath.Join(root, "abs"))
	os.Symlink("../outside", filepath.Join(root, "up"))
	run(t, root, "git", "add", "-A")
	commit(t, root, "escaping links")
	os.Remove(filepath.Join(root, "abs"))
	os.Remove(filepath.Join(root, "up"))
	write(t, root, map[string]string{"up": "now a file\n"})
	m := snap(t, root, "", Options{})
	dest := filepath.Join(outside(t), "copy")
	cleanupCopy(t, root, dest)
	if _, err := Materialize(root, m, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "abs")); !os.IsNotExist(err) {
		t.Error("the deleted link exists in the copy")
	}
}

// The copy is checked by its bytes, so Git flags or normalization that hide
// a change from Git do not hide it from CheckCopy.
func TestCheckCopyReadsBytes(t *testing.T) {
	t.Parallel()
	root := repo(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	write(t, root, map[string]string{"a.txt": "changed\n"})
	m := snap(t, root, "", Options{})
	dest := filepath.Join(outside(t), "copy")
	cleanupCopy(t, root, dest)
	if _, err := Materialize(root, m, dest); err != nil {
		t.Fatal(err)
	}
	run(t, dest, "git", "update-index", "--assume-unchanged", "b.txt")
	p := filepath.Join(dest, "b.txt")
	os.Chmod(p, 0o644)
	os.WriteFile(p, []byte("x\n"), 0o644)
	if err := CheckCopy(dest, m); err == nil || !strings.Contains(err.Error(), "b.txt") {
		t.Errorf("a hidden change in the copy passed: %v", err)
	}
}

func TestWithinExact(t *testing.T) {
	t.Parallel()
	if withinExact("/e/copy", "/e/COPY/secret") {
		t.Error("a differently cased sibling counted as inside")
	}
	if !withinExact("/e/copy", "/e/copy/a") || withinExact("/e/copy", "/e/copy-2/a") {
		t.Error("withinExact containment is wrong")
	}
	if !within("/e/copy", "/e/COPY/a") {
		t.Error("the refusing check must fold case")
	}
}
