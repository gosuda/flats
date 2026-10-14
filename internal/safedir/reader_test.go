package safedir

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeTestFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRegularUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "index.html"), "safe")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	seen, err := root.Lstat("index.html")
	if err != nil {
		t.Fatal(err)
	}
	f, info, err := OpenRegular(root, "index.html", seen)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "safe" || info.Size() != 4 {
		t.Fatalf("read: data=%q info=%v err=%v", data, info, err)
	}
}

func TestOpenRegularRejectsReplacements(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, dir, file, outside string)
	}{
		{
			name: "equal-size regular file",
			mutate: func(t *testing.T, _, file, _ string) {
				t.Helper()
				// Keep the original linked so the filesystem cannot hand its
				// inode number to the replacement (Linux reuses freed ones).
				if err := os.Rename(file, file+".old"); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, file, "evil")
			},
		},
		{
			name: "symlink to outside root",
			mutate: func(t *testing.T, _, file, outside string) {
				t.Helper()
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, file); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink to same inode within root",
			mutate: func(t *testing.T, _, file, _ string) {
				t.Helper()
				saved := filepath.Join(filepath.Dir(file), "saved.html")
				if err := os.Rename(file, saved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("saved.html", file); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink to other file within root",
			mutate: func(t *testing.T, _, file, _ string) {
				t.Helper()
				writeTestFile(t, filepath.Join(filepath.Dir(file), "other.html"), "evil")
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("other.html", file); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "ancestor symlink outside root",
			mutate: func(t *testing.T, _, file, outside string) {
				t.Helper()
				parent := filepath.Dir(file)
				if err := os.Rename(parent, parent+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(outside), parent); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "sub", "index.html")
			outside := filepath.Join(t.TempDir(), "index.html")
			writeTestFile(t, file, "safe")
			writeTestFile(t, outside, "evil") // Equal-size target must never be read.
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			seen, err := root.Lstat("sub/index.html")
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(t, dir, file, outside)
			f, _, err := OpenRegular(root, "sub/index.html", seen)
			if f != nil {
				f.Close()
			}
			if err == nil {
				t.Fatal("opened a path changed after directory traversal")
			}
		})
	}
}

// TestOpenRegularKeepsRootAfterRename checks that a walk stays on the
// directory it opened even if its pathname is later pointed elsewhere.
func TestOpenRegularKeepsRootAfterRename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cannot rename an open directory on Windows")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "dist")
	writeTestFile(t, filepath.Join(dir, "index.html"), "safe")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	seen, err := root.Lstat("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, filepath.Join(parent, "old-dist")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "index.html"), "evil")
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	f, _, err := OpenRegular(root, "index.html", seen)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "safe" {
		t.Fatalf("read %q (err %v) from the replacement root", data, err)
	}
}
