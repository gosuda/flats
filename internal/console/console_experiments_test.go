package console

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

// TestConsoleExperiments renders Settings › Experiments, the Agents page
// under Autopilot and the approval page's "Decided by" under Node with the
// fake DOM of testdata/ui_test.mjs (testdata/experiments_test.mjs).
func TestConsoleExperiments(t *testing.T) {
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
	for _, name := range []string{"ui_test.mjs", "experiments_test.mjs"} {
		script, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), script, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The Experiments page must list exactly the server's operations.
	ops, err := json.Marshal(core.AutopilotOps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "autopilot_ops.json"), ops, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "experiments_test.mjs")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "experiments ok") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
