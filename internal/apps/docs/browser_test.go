package docs

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBrowserCollaboration(t *testing.T) {
	if os.Getenv("FLATS_DOCS_BROWSER_TEST") != "1" {
		t.Skip("opt in with FLATS_DOCS_BROWSER_TEST=1 and FLATS_PLAYWRIGHT_MODULE")
	}
	a := startApp(t, t.TempDir(), initialText, "browser-v1")
	var active atomic.Pointer[app]
	active.Store(a)
	// Simulate the host's trusted access header on HTTP and WebSocket requests.
	browserServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/test-activate" {
			next := startApp(t, a.data, strings.Repeat("rewritten line\n", 200), "browser-v2")
			next.document(t)
			active.Store(next)
			w.WriteHeader(204)
			return
		}
		r.Header.Set("X-Flats-Access", "public")
		if c, err := r.Cookie("flats-test-private"); err == nil && c.Value == "1" {
			r.Header.Set("X-Flats-Access", "private")
		}
		active.Load().inst.ServeHTTP(w, r)
	}))
	defer browserServer.Close()
	screenshots := os.Getenv("FLATS_DOCS_BROWSER_EVIDENCE")
	if screenshots == "" {
		screenshots = t.TempDir()
	}
	if err := os.MkdirAll(screenshots, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", "testdata/browser_test.mjs", browserServer.URL, screenshots)
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	t.Logf("screenshots: %s", screenshots)
	if err != nil {
		t.Fatal(err)
	}
}
