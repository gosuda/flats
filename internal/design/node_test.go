package design

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runNodeScript runs testdata/<script> under Node in a temporary module
// directory that also holds the canvas app's web/app.js and web/wheel.js.
func runNodeScript(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"type":"module"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app.js", "wheel.js"} {
		data, err := webFS.ReadFile("web/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join("testdata", script))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, script), data, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// TestWheelPan runs testdata/wheel_test.mjs against the canvas's wheel.js and
// the viewport handler, including gesture boundaries and zoom.
func TestWheelPan(t *testing.T) { runNodeScript(t, "wheel_test.mjs") }

// TestFocusFrameLoading runs testdata/focus_test.mjs: the canvas app booted
// on a phone-sized viewport creates no canvas artboard frame for the first
// time while an artboard is open full window, loads the boards in view when
// it closes, keeps reloading frames it already loaded, and keeps an error
// reported in the full-window view on its canvas board.
func TestFocusFrameLoading(t *testing.T) { runNodeScript(t, "focus_test.mjs") }
