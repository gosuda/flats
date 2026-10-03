// Package console serves the operator's web console: a single-page app with
// no build step whose assets are embedded in the binary. The app talks only
// to /console/api on the same origin.
package console

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed static
var staticFS embed.FS

// CSP is the Content-Security-Policy of every console response. The
// frame-ancestors directive keeps the console (and its approve buttons) out
// of other sites' frames.
const CSP = "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// asset is one embedded file, prepared once at startup.
type asset struct {
	data  []byte
	ctype string
	etag  string
}

var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".ico":  "image/x-icon",
	".json": "application/json",
}

// load reads the embedded tree. Files with unknown extensions are not served.
func load() (index *asset, assets map[string]*asset) {
	assets = map[string]*asset{}
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ctype, ok := contentTypes[path.Ext(p)]
		if !ok {
			return nil
		}
		b, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		a := &asset{data: b, ctype: ctype, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		rel := strings.TrimPrefix(p, "static/")
		if rel == "index.html" {
			index = a
		} else {
			assets[rel] = a
		}
		return nil
	})
	if err != nil || index == nil {
		// The tree is compiled in; failing here is a build defect.
		panic("console: embedded assets are broken")
	}
	return index, assets
}

// Handler serves the console. Mount it at "/" next to /console/api and /mcp:
// it answers the app routes (/, /flats/{slug}, /approvals/{id}, /settings)
// with index.html and /assets/... with embedded files, and 404s anything else.
func Handler() http.Handler {
	index, assets := load()
	mux := http.NewServeMux()
	page := func(w http.ResponseWriter, r *http.Request) { serve(w, r, index) }
	mux.HandleFunc("GET /{$}", page)
	mux.HandleFunc("GET /flats/{slug}", page)
	mux.HandleFunc("GET /flats/{slug}/settings", page)
	mux.HandleFunc("GET /flats/{slug}/analytics", page)
	mux.HandleFunc("GET /flats/{slug}/database", page)
	mux.HandleFunc("GET /approvals/{id}", page)
	mux.HandleFunc("GET /settings", page)
	mux.HandleFunc("GET /assets/{path...}", func(w http.ResponseWriter, r *http.Request) {
		a, ok := assets["assets/"+r.PathValue("path")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		serve(w, r, a)
	})
	// Set the headers before routing so the mux's own 404, 405 and redirect
	// responses carry them too.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		mux.ServeHTTP(w, r)
	})
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
}

func serve(w http.ResponseWriter, r *http.Request, a *asset) {
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	// Revalidate every time: the binary can be replaced under the same URLs.
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", a.etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.data))
}
