package notebook

import (
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWebClient runs the browser client's tests (web/testdata) under Node.
// Without node it skips locally but fails in CI, so the client tests cannot
// silently stop running there.
func TestWebClient(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is not installed, so the notebook web client tests cannot run in CI")
		}
		t.Skip("node is not installed")
	}
	// go test caches a pass by the files this process opens; node's reads
	// are invisible to it, so read the client sources here too.
	err = filepath.WalkDir("web", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		_, err = os.ReadFile(p)
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

var importRE = regexp.MustCompile(`(?m)^\s*(?:import|export)\b[^'"]*?\bfrom\s+'(\.[^']+)'|^\s*import\s+'(\.[^']+)'`)

// TestWebEmbed checks that the embedded client is complete and contains
// no tests: the shell and every module it imports are embedded.
func TestWebEmbed(t *testing.T) {
	for _, name := range []string{"web/index.html", "web/boot.js", "web/app.js", "web/app.css"} {
		if _, err := fs.Stat(webFS, name); err != nil {
			t.Errorf("%s is not embedded: %v", name, err)
		}
	}
	err := fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(p, "testdata") {
			t.Errorf("test file %s is embedded", p)
		}
		if d.IsDir() || path.Ext(p) != ".js" {
			return nil
		}
		src, err := fs.ReadFile(webFS, p)
		if err != nil {
			return err
		}
		for _, m := range importRE.FindAllStringSubmatch(string(src), -1) {
			spec := m[1] + m[2]
			target := path.Join(path.Dir(p), spec)
			if _, err := fs.Stat(webFS, target); err != nil {
				t.Errorf("%s imports %s, which is not embedded", p, spec)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
