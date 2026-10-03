package console

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsoleUI renders the list and flat pages under Node with a fake DOM
// (testdata/ui_test.mjs): list rows omit repeated public notices, deployment
// results retain their disclosures, and live versions can apply secrets.
func TestConsoleUI(t *testing.T) {
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
	script, err := os.ReadFile(filepath.Join("testdata", "ui_test.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ui_test.mjs"), script, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "ui_test.mjs")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
