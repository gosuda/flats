package site

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newSite(t *testing.T, spa bool, notFound string) *Static {
	dir := t.TempDir()
	for p, body := range map[string]string{
		"index.html": "home", "about.html": "about", "docs/index.html": "docs",
		"assets/app-3f9a1c2b.js": "js", "404.html": "missing", "flats.json": `{"secret":"no"}`,
	} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644)
	}
	return &Static{Dir: dir, Entry: "index.html", SPA: spa, NotFound: notFound}
}

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestStaticRouting(t *testing.T) {
	s := newSite(t, false, "404.html")
	cases := []struct {
		path string
		code int
		body string
	}{
		{"/", 200, "home"},
		{"/about", 200, "about"},
		{"/about.html", 200, "about"},
		{"/docs/", 200, "docs"},
		{"/nope", 404, "missing"},
		{"/flats.json", 404, "missing"},
		{"/../../etc/passwd", 404, "missing"},
	}
	for _, c := range cases {
		rec := do(s, "GET", c.path)
		if rec.Code != c.code || rec.Body.String() != c.body {
			t.Errorf("GET %s = %d %q, want %d %q", c.path, rec.Code, rec.Body.String(), c.code, c.body)
		}
	}
	if rec := do(s, "GET", "/docs"); rec.Code != http.StatusMovedPermanently {
		t.Errorf("directory without slash should redirect, got %d", rec.Code)
	}
	if rec := do(s, "POST", "/"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", rec.Code)
	}
	if cc := do(s, "GET", "/assets/app-3f9a1c2b.js").Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("hashed asset cache-control %q", cc)
	}
	if cc := do(s, "GET", "/").Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("html cache-control %q", cc)
	}
}

func TestSPAFallback(t *testing.T) {
	s := newSite(t, true, "")
	if rec := do(s, "GET", "/app/route/42"); rec.Code != 200 || rec.Body.String() != "home" {
		t.Fatalf("spa fallback: %d %q", rec.Code, rec.Body.String())
	}
	if rec := do(s, "GET", "/missing.png"); rec.Code != 404 {
		t.Fatalf("missing asset with extension must 404 in SPA mode, got %d", rec.Code)
	}
}
