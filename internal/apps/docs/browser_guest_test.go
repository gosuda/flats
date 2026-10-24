package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// Opt-in browser tests of anonymous public editing
// (docs/spec/docs-public-edit-TECH.md 8, rows "Refusal" and "Rendering").
// Each runs a script in testdata with the installed Playwright; they skip
// unless FLATS_DOCS_BROWSER_TEST=1 and FLATS_PLAYWRIGHT_MODULE are set.
// FLATS_DOCS_BROWSER_EVIDENCE keeps screenshots and downloaded files.

// browserFlat is a docs app behind a proxy that sets the host's trusted
// access headers: Private for pages with the flats-test-private cookie,
// otherwise Public at the flat's current level and epoch. POST
// /test-access?level=&epoch= stores a new level through the host access
// operation, as core does.
type browserFlat struct {
	a     *app
	srv   *httptest.Server
	mu    sync.Mutex
	level string
	epoch int64
}

func startBrowserFlat(t *testing.T, source, level string, epoch int64) *browserFlat {
	t.Helper()
	f := &browserFlat{a: startAppModule(t, t.TempDir(), source, "v1", map[string]string{})}
	if err := f.access(level, epoch); err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/test-access" {
			var epoch int64
			fmt.Sscan(r.URL.Query().Get("epoch"), &epoch)
			if err := f.access(r.URL.Query().Get("level"), epoch); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(204)
			return
		}
		for _, h := range []string{"X-Flats-Access", "X-Flats-Public-Access", "X-Flats-Access-Epoch"} {
			r.Header.Del(h)
		}
		if c, err := r.Cookie("flats-test-private"); err == nil && c.Value == "1" {
			r.Header.Set("X-Flats-Access", "private")
		} else {
			f.mu.Lock()
			r.Header.Set("X-Flats-Access", "public")
			r.Header.Set("X-Flats-Public-Access", f.level)
			r.Header.Set("X-Flats-Access-Epoch", fmt.Sprint(f.epoch))
			f.mu.Unlock()
		}
		f.a.inst.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// access stores the level in the worker, then serves it on Public requests.
func (f *browserFlat) access(level string, epoch int64) error {
	b, _ := json.Marshal(map[string]any{"level": level, "epoch": epoch})
	req, err := http.NewRequest("POST", f.a.srv.URL+"/_docs/host/revisions/access", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-Flats-Access", "private")
	req.Header.Set("X-Flats-Host-Op", "revisions")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		out, _ := io.ReadAll(res.Body)
		return fmt.Errorf("access %s %d: %d %s", level, epoch, res.StatusCode, out)
	}
	f.mu.Lock()
	f.level, f.epoch = level, epoch
	f.mu.Unlock()
	return nil
}

// browserEvidence is the directory for screenshots and downloads.
func browserEvidence(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if root := os.Getenv("FLATS_DOCS_BROWSER_EVIDENCE"); root != "" {
		dir = root + "/" + name
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func skipWithoutBrowser(t *testing.T) {
	if os.Getenv("FLATS_DOCS_BROWSER_TEST") != "1" || os.Getenv("FLATS_PLAYWRIGHT_MODULE") == "" {
		t.Skip("opt in with FLATS_DOCS_BROWSER_TEST=1 and FLATS_PLAYWRIGHT_MODULE")
	}
}

// runBrowserScript runs a testdata script and fails on a non-zero exit.
func runBrowserScript(t *testing.T, script string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", append([]string{"testdata/" + script}, args...)...)
	cmd.Env = os.Environ()
	cmd.WaitDelay = 10 * time.Second
	output, err := cmd.CombinedOutput()
	for _, line := range strings.Split(strings.TrimRight(string(output), "\n"), "\n") {
		t.Log(line)
	}
	if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
}

// A guest's refused update in a real browser: the page stops editing with
// the reason, keeps its text for download, and Rejoin edits again as the
// same guest from the server's text (spec rule 22).
func TestBrowserGuestRefusal(t *testing.T) {
	skipWithoutBrowser(t)
	f := startBrowserFlat(t, module(initialText, "v1"), "edit", 1)
	runBrowserScript(t, "guest_refusal.mjs", f.srv.URL, browserEvidence(t, "refusal"))
	if md := f.a.markdown(t, "index.md"); strings.Contains(md, "PASTE-BEGIN") || strings.Contains(md, "typed after the paste") || !strings.Contains(md, "typed after rejoining") {
		t.Fatalf("server text after the refusal: %q", md)
	}
}

// Hostile guest Markdown runs no script and loads nothing from another
// origin on member, viewer and guest pages; links are safe (TECH 3.10).
func TestBrowserHostileContent(t *testing.T) {
	skipWithoutBrowser(t)
	f := startBrowserFlat(t, module(initialText, "v1"), "edit", 1)
	runBrowserScript(t, "hostile_content.mjs", f.srv.URL, browserEvidence(t, "hostile"))
}

// A member page draws diagrams automatically while the flat is Public
// view only; once the level becomes edit, new diagrams, a guest's too, draw
// only when the reader asks (TECH 3.10).
func TestBrowserDiagramPolicy(t *testing.T) {
	skipWithoutBrowser(t)
	text := "# Diagrams\n\n```mermaid\ngraph TD\n  Start --> Finish\n```\n\nEnd line\n"
	f := startBrowserFlat(t, module(text, "v1"), "view", 1)
	runBrowserScript(t, "diagram_policy.mjs", f.srv.URL, browserEvidence(t, "diagrams"))
}
