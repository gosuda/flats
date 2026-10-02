// Package site serves the files of one flat version.
package site

import (
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Static serves a static version directory according to its manifest.
type Static struct {
	Dir      string
	Entry    string // index document, e.g. index.html
	SPA      bool   // serve Entry for unknown paths without an extension
	NotFound string // optional 404 page
	ModTime  time.Time
}

func init() {
	// Make common web types deterministic regardless of the host mime database.
	for ext, typ := range map[string]string{
		".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
		".css": "text/css; charset=utf-8", ".html": "text/html; charset=utf-8",
		".json": "application/json", ".svg": "image/svg+xml", ".wasm": "application/wasm",
		".webmanifest": "application/manifest+json", ".txt": "text/plain; charset=utf-8",
		".woff2": "font/woff2", ".woff": "font/woff", ".webp": "image/webp", ".avif": "image/avif",
	} {
		_ = mime.AddExtensionType(ext, typ)
	}
}

func (s *Static) open(p string) (*os.File, fs.FileInfo, error) {
	full := filepath.Join(s.Dir, filepath.FromSlash(p))
	if !strings.HasPrefix(full, filepath.Clean(s.Dir)+string(filepath.Separator)) && full != filepath.Clean(s.Dir) {
		return nil, nil, fs.ErrNotExist
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, st, nil
}

// ServeHTTP implements http.Handler.
func (s *Static) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := path.Clean("/" + r.URL.Path)
	rel := strings.TrimPrefix(p, "/")
	if rel == "" {
		rel = s.Entry
	}
	if rel == "flats.json" {
		s.notFound(w, r)
		return
	}
	f, st, err := s.open(rel)
	if err == nil && st.IsDir() {
		f.Close()
		// Directory: redirect to the trailing-slash form, then serve its index.
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/"+queryOf(r), http.StatusMovedPermanently)
			return
		}
		f, st, err = s.open(path.Join(rel, "index.html"))
	}
	if err != nil && !strings.Contains(path.Base(rel), ".") {
		// Pretty URLs: /about -> about.html
		f, st, err = s.open(rel + ".html")
		if err != nil && s.SPA {
			f, st, err = s.open(s.Entry)
		}
	}
	if err != nil || st.IsDir() {
		if f != nil {
			f.Close()
		}
		s.notFound(w, r)
		return
	}
	defer f.Close()
	if strings.HasPrefix(rel, "assets/") || strings.Contains(path.Base(rel), ".") && hashedName(path.Base(rel)) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, st.Name(), s.ModTime, f)
}

func (s *Static) notFound(w http.ResponseWriter, r *http.Request) {
	if s.NotFound != "" {
		if f, st, err := s.open(s.NotFound); err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusNotFound)
			if r.Method != http.MethodHead {
				buf := make([]byte, st.Size())
				n, _ := f.Read(buf)
				w.Write(buf[:n])
			}
			return
		}
	}
	http.Error(w, "404 page not found", http.StatusNotFound)
}

func queryOf(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

// hashedName reports whether a file name looks content-hashed
// (e.g. app.3f9a1c2b.js or index-BqZ2x8Ka.css).
func hashedName(name string) bool {
	base := strings.TrimSuffix(name, path.Ext(name))
	for _, sep := range []string{".", "-"} {
		if i := strings.LastIndex(base, sep); i >= 0 {
			h := base[i+1:]
			if len(h) >= 8 && len(h) <= 32 && isAlnum(h) && hasDigit(h) {
				return true
			}
		}
	}
	return false
}

func isAlnum(s string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func hasDigit(s string) bool {
	for _, c := range s {
		if c >= '0' && c <= '9' {
			return true
		}
	}
	return false
}

// ErrNoEntry is returned by Check when the entry document is missing.
var ErrNoEntry = errors.New("entry document missing")
