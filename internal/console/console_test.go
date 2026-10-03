package console

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

func get(t *testing.T, h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func readStatic(t *testing.T, name string) string {
	t.Helper()
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAppRoutesServeIndex(t *testing.T) {
	h := Handler()
	index := readStatic(t, "index.html")
	for _, p := range []string{"/", "/flats/my-blog", "/flats/my-blog/settings", "/flats/my-blog/analytics", "/flats/my-blog/database", "/approvals/apr-abc123", "/settings", "/settings?x=1"} {
		rec := get(t, h, http.MethodGet, p, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", p, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("GET %s: Content-Type %q", p, ct)
		}
		if rec.Body.String() != index {
			t.Errorf("GET %s: body is not index.html", p)
		}
		checkSecurityHeaders(t, p, rec)
	}
	// HEAD answers like GET without a body.
	rec := get(t, h, http.MethodHead, "/flats/x", nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD: status %d, %d body bytes", rec.Code, rec.Body.Len())
	}
}

func checkSecurityHeaders(t *testing.T, p string, rec *httptest.ResponseRecorder) {
	t.Helper()
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "img-src 'self' data:", "style-src 'self'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("%s: CSP %q lacks %q", p, csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("%s: CSP allows unsafe sources: %q", p, csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("%s: missing nosniff", p)
	}
}

func TestAssetsHaveTypes(t *testing.T) {
	h := Handler()
	cases := map[string]string{
		"/assets/app.css":        "text/css; charset=utf-8",
		"/assets/js/app.js":      "text/javascript; charset=utf-8",
		"/assets/js/api.js":      "text/javascript; charset=utf-8",
		"/assets/favicon.svg":    "image/svg+xml",
		"/assets/js/settings.js": "text/javascript; charset=utf-8",
		"/assets/js/approval.js": "text/javascript; charset=utf-8",
	}
	for p, want := range cases {
		rec := get(t, h, http.MethodGet, p, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", p, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != want {
			t.Errorf("GET %s: Content-Type %q, want %q", p, ct, want)
		}
		checkSecurityHeaders(t, p, rec)
		etag := rec.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("GET %s: no ETag", p)
		}
		// Revalidation with the ETag returns 304 without a body.
		rec = get(t, h, http.MethodGet, p, map[string]string{"If-None-Match": etag})
		if rec.Code != http.StatusNotModified {
			t.Errorf("GET %s with If-None-Match: status %d", p, rec.Code)
		}
	}
}

func TestUnknownPaths404(t *testing.T) {
	h := Handler()
	for _, p := range []string{
		"/assets/missing.js", "/assets/js/nope.js", "/assets/", "/assets/js", "/static/index.html",
		"/index.html", "/flats/a/b", "/flats/", "/approvals/", "/settings/x", "/nope",
	} {
		if rec := get(t, h, http.MethodGet, p, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", p, rec.Code)
		}
	}
	// ServeMux redirects unclean paths to their clean form, which is a 404.
	rec := get(t, h, http.MethodGet, "/assets/../index.html", nil)
	if loc := rec.Header().Get("Location"); rec.Code/100 != 3 || loc != "/index.html" {
		t.Errorf("GET /assets/../index.html: status %d, Location %q", rec.Code, loc)
	}
	if rec := get(t, h, http.MethodPost, "/settings", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /settings: status %d, want 405", rec.Code)
	}
}

// Every module the app imports and every file index.html references must be
// served, or the page breaks at load time.
func TestReferencedAssetsExist(t *testing.T) {
	h := Handler()
	index := readStatic(t, "index.html")
	refs := regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`).FindAllStringSubmatch(index, -1)
	if len(refs) < 3 {
		t.Fatalf("index.html references only %d assets", len(refs))
	}
	for _, m := range refs {
		if rec := get(t, h, http.MethodGet, m[1], nil); rec.Code != http.StatusOK {
			t.Errorf("index.html references %s: status %d", m[1], rec.Code)
		}
	}
	importRe := regexp.MustCompile(`(?m)^\s*import\s[^;]*?from\s+'([^']+)'`)
	err := fs.WalkDir(staticFS, "static/assets/js", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		src := readStatic(t, strings.TrimPrefix(p, "static/"))
		for _, m := range importRe.FindAllStringSubmatch(src, -1) {
			if !strings.HasPrefix(m[1], "./") {
				t.Errorf("%s imports %q: only relative module imports are allowed", p, m[1])
				continue
			}
			url := "/assets/js/" + strings.TrimPrefix(m[1], "./")
			if rec := get(t, h, http.MethodGet, url, nil); rec.Code != http.StatusOK {
				t.Errorf("%s imports %s: status %d", p, url, rec.Code)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

var bareConfirm = regexp.MustCompile(`(^|[^A-Za-z_.])(confirm|alert)\(`)

// The page must work under the CSP: no inline scripts, styles or handlers,
// and nothing loaded from other origins.
func TestCSPFriendlyMarkup(t *testing.T) {
	index := readStatic(t, "index.html")
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(index) {
		t.Error("index.html has an inline event handler")
	}
	if regexp.MustCompile(`(?i)\sstyle\s*=`).MatchString(index) || strings.Contains(index, "<style") {
		t.Error("index.html has inline styles")
	}
	for _, m := range regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`).FindAllStringSubmatch(index, -1) {
		if !strings.Contains(m[1], "src=") || strings.TrimSpace(m[2]) != "" {
			t.Error("index.html has an inline script")
		}
	}
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		f, err := staticFS.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		s := string(b)
		for _, bad := range []string{"@import", "fonts.googleapis", "cdn.", "unpkg", "innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "setAttribute('style'", "window.confirm", "window.alert"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
		if bareConfirm.MatchString(s) {
			t.Errorf("%s calls the browser's confirm(); use confirmDialog", p)
		}
		if regexp.MustCompile(`url\(\s*['"]?https?:`).MatchString(s) {
			t.Errorf("%s loads a remote url()", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// All requests go through api.js, which sends the console header and stays on
// /console/api.
func TestRequestsUseConsoleAPI(t *testing.T) {
	apiJS := readStatic(t, "assets/js/api.js")
	if !strings.Contains(apiJS, `'X-Flats-Console': '1'`) {
		t.Error("api.js does not send X-Flats-Console: 1")
	}
	if !strings.Contains(apiJS, `const BASE = '/console/api';`) {
		t.Error("api.js does not target /console/api")
	}
	for _, m := range regexp.MustCompile(`fetch\(([^,)]+)`).FindAllStringSubmatch(apiJS, -1) {
		if !strings.HasPrefix(strings.TrimSpace(m[1]), "BASE +") {
			t.Errorf("api.js fetches %s outside /console/api", m[1])
		}
	}
	err := fs.WalkDir(staticFS, "static/assets/js", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, "/api.js") {
			return err
		}
		src := readStatic(t, strings.TrimPrefix(p, "static/"))
		if strings.Contains(src, "fetch(") || strings.Contains(src, "XMLHttpRequest") {
			t.Errorf("%s makes requests directly; use api.js", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Lifecycle copy must not promise owner-only access or present Serve as public.
func TestLifecycleCopy(t *testing.T) {
	src := strings.ToLower(readStatic(t, "assets/js/lifecycle.js") + readStatic(t, "assets/js/list.js") + readStatic(t, "assets/js/flat.js"))
	for _, banned := range []string{"only me", "only you", "tailscale serve", "this flat is public"} {
		if strings.Contains(src, banned) {
			t.Errorf("console lifecycle copy contains %q", banned)
		}
	}
	if !strings.Contains(src, "tailscale funnel") {
		t.Error("console does not name Tailscale Funnel")
	}
}

// The confirm dialog shows the public notice before widening; it must be the
// same text the API returns afterwards.
func TestNoticesMatchCore(t *testing.T) {
	dom := readStatic(t, "assets/js/dom.js")
	for name, want := range map[string]string{"UNLISTED_NOTICE": core.UnlistedNotice, "LISTED_NOTICE": core.ListedNotice, "PUBLIC_ACCESS_NOTICE": core.PublicAccessNotice} {
		decl := "export const " + name + " = '" + want + "';"
		if !strings.Contains(dom, decl) {
			t.Errorf("dom.js %s differs from internal/core; want %s", name, decl)
		}
	}
}

// The limits form edits exactly the settings core knows.
func TestSettingsFieldsMatchCore(t *testing.T) {
	src := readStatic(t, "assets/js/settings.js")
	for key := range core.Defaults {
		if !strings.Contains(src, "'"+key+"'") && !strings.Contains(src, "."+key) {
			t.Errorf("settings.js has no field for setting %s", key)
		}
	}
}

// The mux's own 404, 405 and redirect responses get the same headers as the
// pages: a response without frame-ancestors could be framed.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := Handler()
	cases := []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/nope", http.StatusNotFound},
		{http.MethodGet, "/assets/missing.js", http.StatusNotFound},
		{http.MethodPost, "/settings", http.StatusMethodNotAllowed},
		{http.MethodGet, "/assets/../index.html", http.StatusTemporaryRedirect},
		{http.MethodGet, "/flats/x", http.StatusOK},
	}
	for _, c := range cases {
		rec := get(t, h, c.method, c.path, nil)
		if rec.Code != c.code {
			t.Errorf("%s %s: status %d, want %d", c.method, c.path, rec.Code, c.code)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != CSP {
			t.Errorf("%s %s: CSP %q, want %q", c.method, c.path, got, CSP)
		}
		if rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s %s: missing X-Frame-Options", c.method, c.path)
		}
		checkSecurityHeaders(t, c.method+" "+c.path, rec)
	}
}

// TestJSHelpers runs the console's pure helpers under Node when it is
// installed. They handle server data that becomes link targets and request
// paths, so a regression is a security or loading bug.
func TestJSHelpers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"type":"module"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := staticFS.ReadDir("static/assets/js")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b := readStatic(t, "assets/js/"+e.Name())
		if err := os.WriteFile(filepath.Join(dir, e.Name()), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := `
import assert from 'node:assert/strict';
import { thumbPath } from './api.js';
import { safeHref } from './ui.js';
import { isNet } from './settings.js';

assert.equal(thumbPath('/api/flats/blog/versions/2/files/shot.png'), '/flats/blog/versions/2/files/shot.png');
assert.equal(thumbPath('/api/flats/blog/versions/2/files/shots/a #1?.png'), '/flats/blog/versions/2/files/shots/a%20%231%3F.png');
assert.equal(thumbPath('/api/flats/blog/versions/2/files/100%.png'), '/flats/blog/versions/2/files/100%25.png');
assert.equal(thumbPath('/console/api/flats/b/versions/1/files/x.png'), '/flats/b/versions/1/files/x.png');
assert.equal(thumbPath('https://evil.example/x.png'), null);
assert.equal(thumbPath('/api/approvals/x'), null);

assert.equal(safeHref('https://blog.tail1.ts.net'), 'https://blog.tail1.ts.net/');
assert.equal(safeHref('http://127.0.0.1:7878/x'), 'http://127.0.0.1:7878/x');
assert.equal(safeHref('javascript:alert(1)'), null);
assert.equal(safeHref(' JavaScript:alert(1)'), null);
assert.equal(safeHref('data:text/html,hi'), null);
assert.equal(safeHref('/relative'), null);
assert.equal(safeHref(undefined), null);

assert.equal(isNet({ kind: 'local', enabled: true, hosts: null }), true);
assert.equal(isNet({ kind: 'tailscale', enabled: true, hosts: [] }), true);
assert.equal(isNet({ hosts: null }), false);
assert.equal(isNet({ version: '1' }), false);
assert.equal(isNet([{ hosts: [] }]), false);
assert.equal(isNet(null), false);
console.log('ok');
`
	cmd := exec.Command(node, "--input-type=module", "-e", script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
