package board

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestWebClient runs the canvas app's tests (internal/board/web/testdata)
// under Node. Without node it skips locally but fails in CI, so the client
// tests cannot silently stop running there.
func TestWebClient(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is not installed, so the board web client tests cannot run in CI")
		}
		t.Skip("node is not installed")
	}
	// go test caches a pass by the files this process opens; node's reads
	// are invisible to it, so read the client sources here too.
	err = filepath.WalkDir("web", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		_, err = os.ReadFile(path)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join("web", "testdata", "*.test.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no web client tests in web/testdata")
	}
	out, err := exec.Command(node, append([]string{"--test"}, files...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}
