package board

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"testing"
)

// TestAppFilesAreVersioned: the shell names boot.js, app.css, app.js and,
// through its import map, every module by a content version; those URLs may
// be kept forever, the shell and unversioned URLs are revalidated with an
// ETag, and a version the binary does not have is a 404 (11.7). The import
// map runs under the shell's CSP by its hash.
func TestAppFilesAreVersioned(t *testing.T) {
	b := newTB(t)
	do := func(method, url string, h map[string]string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, url, nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}
	get := func(url string, h map[string]string) (*http.Response, string) {
		t.Helper()
		return do(http.MethodGet, url, h)
	}

	resp, shell := get(b.priv.URL+"/", nil)
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("ETag") == "" {
		t.Fatalf("shell %d %v", resp.StatusCode, resp.Header)
	}
	if r, _ := get(b.priv.URL+"/", map[string]string{"If-None-Match": resp.Header.Get("ETag")}); r.StatusCode != http.StatusNotModified {
		t.Fatalf("shell revalidation %d", r.StatusCode)
	}
	m := regexp.MustCompile(`<script type="importmap">(.*?)</script>`).FindStringSubmatch(shell)
	if m == nil {
		t.Fatal("the shell has no import map")
	}
	sum := sha256.Sum256([]byte(m[1]))
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self' 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
		t.Fatalf("the CSP does not allow the import map: %s", csp)
	}
	boot := strings.Index(shell, `<script src="/_board/app/boot.js?v=`)
	if boot < 0 || boot > strings.Index(shell, `<link rel="stylesheet"`) || strings.Index(shell, `<script type="importmap">`) > strings.Index(shell, `<script type="module"`) {
		t.Fatal("the shell must load boot.js first and the import map before the module script")
	}
	var im struct{ Imports map[string]string }
	if err := json.Unmarshal([]byte(m[1]), &im); err != nil {
		t.Fatal(err)
	}

	// Every module an app file imports resolves to a mapped, versioned URL.
	imp := regexp.MustCompile(`(?m)^\s*(?:import|export)\b[^'"]*?from\s*['"]([^'"]+)['"]|^\s*import\s*['"]([^'"]+)['"]`)
	err := fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		src, _ := fs.ReadFile(webFS, p)
		name := strings.TrimPrefix(p, "web/")
		if _, ok := im.Imports[appPrefix+name]; !ok && name != "boot.js" {
			t.Errorf("%s is not in the import map", name)
		}
		for _, s := range imp.FindAllStringSubmatch(string(src), -1) {
			spec := s[1] + s[2]
			target := path.Join(path.Dir(appPrefix+name), spec)
			if _, ok := im.Imports[target]; !ok {
				t.Errorf("%s imports %s (%s), which the import map does not version", name, spec, target)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	urls := []string{
		regexp.MustCompile(`href="(/_board/app/app\.css\?v=[0-9a-f]+)"`).FindStringSubmatch(shell)[1],
		regexp.MustCompile(`src="(/_board/app/boot\.js\?v=[0-9a-f]+)"`).FindStringSubmatch(shell)[1],
	}
	if s := regexp.MustCompile(`<script type="module" src="(/_board/app/app\.js\?v=[0-9a-f]+)"`).FindStringSubmatch(shell); s != nil {
		urls = append(urls, s[1])
	} else {
		t.Fatal("the shell does not load a versioned app.js")
	}
	for _, u := range im.Imports {
		urls = append(urls, u)
	}
	for _, u := range urls {
		r, body := get(b.pub.URL+u, nil)
		if r.StatusCode != 200 || r.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" || body == "" {
			t.Fatalf("%s: %d %v", u, r.StatusCode, r.Header)
		}
		bare := strings.SplitN(u, "?", 2)[0]
		r2, body2 := get(b.pub.URL+bare, nil)
		if r2.StatusCode != 200 || r2.Header.Get("Cache-Control") != "no-cache" || body2 != body {
			t.Fatalf("%s: %d %v", bare, r2.StatusCode, r2.Header)
		}
		if r3, _ := get(b.pub.URL+bare, map[string]string{"If-None-Match": r2.Header.Get("ETag")}); r3.StatusCode != http.StatusNotModified {
			t.Fatalf("%s revalidation %d", bare, r3.StatusCode)
		}
		if r4, body4 := do(http.MethodHead, b.pub.URL+u, nil); r4.StatusCode != 200 || body4 != "" || r4.Header.Get("ETag") != r2.Header.Get("ETag") {
			t.Fatalf("HEAD %s: %d %v", u, r4.StatusCode, r4.Header)
		}
		if r5, _ := do(http.MethodHead, b.pub.URL+bare, map[string]string{"If-None-Match": r2.Header.Get("ETag")}); r5.StatusCode != http.StatusNotModified {
			t.Fatalf("conditional HEAD %s: %d", bare, r5.StatusCode)
		}
	}

	// boot.js runs in any version, so a shell of another binary can reload.
	if r, body := get(b.pub.URL+appPrefix+"boot.js?v=0000000000000000", nil); r.StatusCode != 200 || r.Header.Get("Cache-Control") != "no-store" || !strings.Contains(body, "location.reload") {
		t.Fatalf("boot.js of another version: %d %v", r.StatusCode, r.Header)
	}

	// A version this binary does not have is not served, and not stored.
	// A query that does not parse cleanly counts as another version.
	cur := strings.TrimPrefix(im.Imports[appPrefix+"app.js"], appPrefix+"app.js") // "?v=<version>"
	for _, q := range []string{"?v=0000000000000000", "?v=", cur + "&v=x", cur + ";x", cur + "&v=x;y", "?v=0000;x", cur + "&x=%zz"} {
		r, _ := get(b.pub.URL+appPrefix+"app.js"+q, nil)
		if r.StatusCode != http.StatusNotFound || r.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("app.js%s: %d %v", q, r.StatusCode, r.Header)
		}
	}
	for _, p := range []string{"nope.js", "../board.go", "testdata/render.test.mjs", ""} {
		if r, _ := get(b.pub.URL+appPrefix+p, nil); r.StatusCode != http.StatusNotFound {
			t.Errorf("%q: %d", p, r.StatusCode)
		}
	}
}
