package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
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

// untar returns path -> content of a tar.gz.
func untar(t *testing.T, b []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			t.Fatalf("%s: type %c", h.Name, h.Typeflag)
		}
		data, _ := io.ReadAll(tr)
		out[h.Name] = string(data)
	}
}

func keys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func TestPackDirSkipsAndRelativePaths(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"index.html":                 "<h1>hi</h1>",
		"assets/app.css":             "body{}",
		"assets/deep/x.js":           "1",
		".DS_Store":                  "junk",
		"assets/.DS_Store":           "junk",
		"assets/._app.css":           "appledouble",
		".git/HEAD":                  "ref",
		"node_modules/lib/index.js":  "x",
		"assets/node_modules/a.js":   "x",
		"__MACOSX/whatever":          "x",
		".well-known/security.txt":   "contact",
		"node_modules.txt":           "a file, not the directory",
		"sub/.gitignore":             "keep dotfiles other than .git",
		"sub/.git":                   "gitdir: worktree pointer",
		"sub/node_modules":           "a plain file named node_modules is kept",
		"assets/deep/.DS_Store.html": "kept",
	})
	b, st, err := packDir(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := untar(t, b)
	want := []string{
		".well-known/security.txt",
		"assets/app.css",
		"assets/deep/.DS_Store.html",
		"assets/deep/x.js",
		"index.html",
		"node_modules.txt",
		"sub/.gitignore",
		"sub/node_modules",
	}
	if !reflect.DeepEqual(keys(got), want) {
		t.Fatalf("packed %v\nwant   %v", keys(got), want)
	}
	if got["index.html"] != "<h1>hi</h1>" {
		t.Errorf("content = %q", got["index.html"])
	}
	if st.Files != len(want) || st.Skipped != 8 {
		t.Errorf("stats = %+v", st)
	}
	for name := range got {
		if strings.HasPrefix(name, "/") || strings.Contains(name, root) {
			t.Errorf("non-relative path %q", name)
		}
	}
}

func TestPackDirRejectsLinksAndLimits(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"index.html": "hello"})
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := packDir(root, 0); err == nil || !strings.Contains(err.Error(), "leak.txt is a symbolic link") {
		t.Fatalf("err = %v", err)
	}
	os.Remove(filepath.Join(root, "leak.txt"))

	if _, _, err := packDir(root, 3); err == nil || !strings.Contains(err.Error(), "upload limit") {
		t.Fatalf("limit err = %v", err)
	}
	if _, _, err := packDir(t.TempDir(), 0); err == nil || !strings.Contains(err.Error(), "no files") {
		t.Fatalf("empty err = %v", err)
	}
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{"-c", "user.name=flats-test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestDetectGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	repo := t.TempDir()
	if _, ok := detectGit(ctx, repo); ok {
		t.Fatal("detected git outside a repository")
	}
	gitCmd(t, repo, "init", "-q")
	writeTree(t, repo, map[string]string{"dist/index.html": "v1"})
	if _, ok := detectGit(ctx, repo); ok {
		t.Fatal("detected a commit in an empty repository")
	}
	gitCmd(t, repo, "add", ".")
	gitCmd(t, repo, "commit", "-q", "-m", "first")
	head := gitCmd(t, repo, "rev-parse", "HEAD")

	gi, ok := detectGit(ctx, filepath.Join(repo, "dist"))
	if !ok || gi.SHA != head || gi.Dirty {
		t.Fatalf("clean: %+v ok=%v head=%s", gi, ok, head)
	}
	writeTree(t, repo, map[string]string{"dist/index.html": "v2"})
	gi, ok = detectGit(ctx, filepath.Join(repo, "dist"))
	if !ok || gi.SHA != head || !gi.Dirty {
		t.Fatalf("modified: %+v ok=%v", gi, ok)
	}
}
