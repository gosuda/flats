package doccheck

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"testing"
)

// TestRepositoryDocs runs the `make docs-check` gate under go test: every
// relative Markdown link and anchor in the repository resolves, and
// docs/agent/ topics use no relative links. CI also runs `make docs-check`
// uncached, because a Git index change alone (git rm --cached) can leave
// this test's cached result stale.
func TestRepositoryDocs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root, err := RepoRoot(".")
	if err != nil {
		t.Skipf("not in a Git work tree: %v", err)
	}
	files, err := GitFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	trackInputs(t, root, files)
	r, err := Check(root, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Errors {
		t.Error(f)
	}
}

// trackInputs opens what the git subprocess read, so that go test's result
// cache, which only sees files the test itself opens, reruns this test when
// a file is added, removed or newly ignored. Check reads every Markdown and
// code file itself.
func trackInputs(t *testing.T, root string, files []string) {
	t.Helper()
	dirs := map[string]bool{".": true}
	for _, f := range files {
		for d := path.Dir(f); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
		if path.Base(f) == ".gitignore" {
			if _, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f))); err != nil {
				t.Fatal(err)
			}
		}
	}
	for d := range dirs {
		if _, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(d))); err != nil {
			t.Fatal(err)
		}
	}
}
