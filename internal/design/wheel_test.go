package design

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWheelPan runs testdata/wheel_test.mjs under Node against the canvas's
// wheel.js and the viewport handler, including gesture boundaries and zoom.
func TestWheelPan(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"type":"module"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	wheel, err := webFS.ReadFile("web/wheel.js")
	if err != nil {
		t.Fatal(err)
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), app, 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join("testdata", "wheel_test.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wheel.js"), wheel, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wheel_test.mjs"), script, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "wheel_test.mjs")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
