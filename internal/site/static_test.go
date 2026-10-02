package site

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestOnlyHashedNamesAreImmutable(t *testing.T) {
	for name, want := range map[string]bool{
		"app.3f9a1c2b.js": true, "index-BqZ2x8Ka.css": true, "chunk-5JQ4ZQ2N.js": true,
		"style.css": false, "app.js": false, "photo-20240101.jpg": false,
		"icon-background1.png": false, "jquery-3.7.1.min.js": false, "LICENSE": false,
	} {
		if got := hashedName(name); got != want {
			t.Errorf("hashedName(%q) = %v, want %v", name, got, want)
		}
	}
}

// writeVersion creates a version directory holding assets/style.css.
func writeVersion(t *testing.T, css string) *Static {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("home"), 0o644)
	os.WriteFile(filepath.Join(dir, "assets", "style.css"), []byte(css), 0o644)
	return &Static{Dir: dir, Entry: "index.html", ModTime: time.Now()}
}

func TestUnhashedAssetsRevalidateAcrossDeploys(t *testing.T) {
	v1, v2, v3 := writeVersion(t, "body{color:red}"), writeVersion(t, "body{color:blue}"), writeVersion(t, "body{color:red}")
	r1 := do(v1, "GET", "/assets/style.css")
	if cc := r1.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("unhashed asset must revalidate, got Cache-Control %q", cc)
	}
	tag := r1.Header().Get("ETag")
	if tag == "" || tag[0] != '"' {
		t.Fatalf("want a strong ETag, got %q", tag)
	}
	if r1.Header().Get("Last-Modified") != "" {
		t.Fatal("Last-Modified must not be sent: a rollback would make an older time current")
	}

	cond := func(h http.Handler) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/assets/style.css", nil)
		req.Header.Set("If-None-Match", tag)
		req.Header.Set("If-Modified-Since", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := cond(v1); rec.Code != http.StatusNotModified {
		t.Fatalf("same version must answer 304, got %d", rec.Code)
	}
	if rec := cond(v2); rec.Code != 200 || rec.Body.String() != "body{color:blue}" {
		t.Fatalf("a deploy that changes the file must send it again, got %d %q", rec.Code, rec.Body.String())
	}
	// A rollback to the same bytes revalidates without a download.
	if rec := cond(v3); rec.Code != http.StatusNotModified {
		t.Fatalf("identical content must keep its ETag, got %d", rec.Code)
	}
}
