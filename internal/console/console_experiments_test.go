package console

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// autopilotOps is the server's list of Autopilot operation IDs in bit order
// (docs/spec/autopilot-TECH.md 2.2), which the Experiments page must match.
var autopilotOps = []string{
	"create_version", "delete_version", "deploy", "rollback", "rollback_restore_data",
	"make_public", "make_private", "public_editing", "delete_flat",
}

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
	ops, err := json.Marshal(autopilotOps)
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
