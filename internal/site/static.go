// Package site serves the files of one flat version.
package site

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Static serves a static version directory according to its manifest.
type Static struct {
	Dir      string
	Entry    string // index document, e.g. index.html
	SPA      bool   // serve Entry for unknown paths without an extension
	NotFound string // optional 404 page
	// ModTime is the version's creation time. It is not sent as
	// Last-Modified: after a rollback an older version becomes current, and
	// a browser's newer If-Modified-Since would wrongly get a 304. Responses
	// are validated with content ETags instead.
	ModTime time.Time

	etags sync.Map // file path -> strong ETag; version files never change
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
	// Only content-hashed names may be cached forever: any other file can
	// change on the next deploy or rollback, so browsers must revalidate it.
	if hashedName(st.Name()) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if tag, err := s.etag(f); err == nil {
		w.Header().Set("ETag", tag)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, st.Name(), time.Time{}, f)
}

// etag returns a strong ETag derived from the file's content, so it changes
// exactly when a deploy or rollback changes the bytes served at a path.
func (s *Static) etag(f *os.File) (string, error) {
	if v, ok := s.etags.Load(f.Name()); ok {
		return v.(string), nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	tag := `"` + hex.EncodeToString(h.Sum(nil)[:16]) + `"`
	s.etags.Store(f.Name(), tag)
	return tag, nil
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

// hashedName reports whether a file name looks content-hashed, as bundlers
// emit them: app.3f9a1c2b.js (hex), index-BqZ2x8Ka.css (mixed case) or
// chunk-5JQ4ZQ2N.js (upper case). The hash part must mix letters and digits
// and be hex or contain an upper-case letter, so dated names such as
// photo-20240101.jpg or words such as icon-background1.png don't qualify.
// A miss only costs a revalidation; a false hit would pin a stale file.
func hashedName(name string) bool {
	ext := path.Ext(name)
	if ext == "" {
		return false
	}
	base := strings.TrimSuffix(name, ext)
	for _, sep := range []string{".", "-"} {
		if i := strings.LastIndex(base, sep); i >= 0 && hashLike(base[i+1:]) {
			return true
		}
	}
	return false
}

func hashLike(h string) bool {
	if len(h) < 8 || len(h) > 64 {
		return false
	}
	var digit, letter, upper, nonHex bool
	for _, c := range h {
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case c >= 'a' && c <= 'f':
			letter = true
		case c >= 'a' && c <= 'z':
			letter, nonHex = true, true
		case c == '_':
			nonHex = true
		case c >= 'A' && c <= 'Z':
			letter, upper, nonHex = true, true, true
		default:
			return false
		}
	}
	return digit && letter && (!nonHex || upper)
}

// ErrNoEntry is returned by Check when the entry document is missing.
var ErrNoEntry = errors.New("entry document missing")
