package board

import (
	"os/exec"
	"testing"
)

// TestWebClient runs the canvas app's tests (internal/board/web/testdata)
// under Node.
func TestWebClient(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "--test", "web/testdata/").CombinedOutput()
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}
