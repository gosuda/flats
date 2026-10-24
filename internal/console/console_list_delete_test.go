package console

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsoleListDelete runs testdata/list_delete_test.mjs: the list's Delete
// menu item, the slide control and each outcome of the request
// (docs/spec/console-row-delete.md).
func TestConsoleListDelete(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"type":"module"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := staticFS.ReadDir("static/assets/js")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.WriteFile(filepath.Join(dir, e.Name()), []byte(readStatic(t, "assets/js/"+e.Name())), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"ui_test.mjs", "list_delete_test.mjs"} {
		script, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), script, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(node, "list_delete_test.mjs")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "list delete ok") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
