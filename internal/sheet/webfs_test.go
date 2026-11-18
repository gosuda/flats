package sheet

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
			t.Fatal("node is not installed, so the sheet web client tests cannot run in CI")
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

// TestWebFiles checks that the embedded client is complete and contains no
// tests: the shell, the files it names and every module they import.
func TestWebFiles(t *testing.T) {
	// The files of technical design section 19.1, and protocol.js.
	names := strings.Fields(`index.html app.css boot.js app.js theme.js net.js protocol.js model.js grid.js virtual.js
		select.js keys.js editor.js clipboard.js fill.js toolbar.js menu.js tabs.js find.js status.js notices.js
		presence.js history.js a11y.js dom.js`)
	for _, name := range names {
		if _, err := fs.Stat(webFS, "web/"+name); err != nil {
			t.Errorf("%s is not embedded: %v", name, err)
		}
	}
	shell, err := fs.ReadFile(webFS, "web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`(?:src|href)="/_sheet/app/([^"]+)"`).FindAllSubmatch(shell, -1) {
		if _, err := fs.Stat(webFS, "web/"+string(m[1])); err != nil {
			t.Errorf("the shell names %s, which is not embedded", m[1])
		}
	}
	err = fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
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
		if !regexp.MustCompile(`(?m)^'use strict';$`).Match(src) {
			t.Errorf("%s does not declare 'use strict'", p)
		}
		for _, m := range importRE.FindAllStringSubmatch(string(src), -1) {
			spec := m[1] + m[2]
			if _, err := fs.Stat(webFS, path.Join(path.Dir(p), spec)); err != nil {
				t.Errorf("%s imports %s, which is not embedded", p, spec)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
