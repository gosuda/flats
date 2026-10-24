package core

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gosuda/flats/internal/board"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/site"
	"github.com/gosuda/flats/internal/store"
)

// DocsApp is the immutable module tree injected by app wiring (or tests).
type DocsApp struct {
	FS            fs.FS
	Hash          string
	Entry         string
	ContentModule string
}

type docsContent struct {
	Format    int            `json:"format"`
	Hash      string         `json:"hash"`
	Entry     string         `json:"entry"`
	Title     string         `json:"title"`
	Documents []docsDocument `json:"documents"`
	Assets    []string       `json:"assets"`
}
type docsDocument struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Text string `json:"text"`
}

// withRouteHeaders clones the request and replaces access and strips all
// spellings of the host-only health signal, public access level and access
// epoch.
func withRouteHeaders(r *http.Request, public bool) *http.Request {
	r = r.Clone(r.Context())
	for key := range r.Header {
		if strings.EqualFold(key, "X-Flats-Access") || strings.EqualFold(key, "X-Flats-Health") || strings.EqualFold(key, "X-Flats-Host-Op") ||
			strings.EqualFold(key, board.PublicAccessHeader) || strings.EqualFold(key, AccessEpochHeader) {
			delete(r.Header, key)
		}
	}
	access := "private"
	if public {
		access = "public"
	}
	r.Header.Set("X-Flats-Access", access)
	return r
}

// docsAssetCSP keeps every bundle asset inert: no scripts (sandbox, no
// script-src), no plugins, frames or connections, and an opaque origin. Inline
// styles and same-site or data: images stay allowed so an SVG or image opened
// directly still renders.
const docsAssetCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; sandbox"

// docsMediaCSP is for audio and video. Browsers do not play a media file opened
// directly in a sandboxed document, so it omits sandbox; with a fixed media type
// and nosniff the browser's media document has nothing scriptable, and
// default-src 'none' still forbids scripts.
const docsMediaCSP = "default-src 'none'; media-src 'self'; frame-ancestors 'none'"

func docsAssets(worker http.Handler, dir string, assets []string) http.Handler {
	listed := make(map[string]bool, len(assets))
	for _, p := range assets {
		listed["/"+p] = true
	}
	static := &site.Static{Dir: dir}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && listed[r.URL.Path] {
			// Bundle assets are untrusted, including assets saved before this policy.
			// Never let an asset become an active document on the docs origin.
			typ := docsInlineMIME(r.URL.Path)
			if strings.HasPrefix(typ, "audio/") || strings.HasPrefix(typ, "video/") {
				w.Header().Set("Content-Security-Policy", docsMediaCSP)
			} else {
				w.Header().Set("Content-Security-Policy", docsAssetCSP)
			}
			if typ != "" {
				w.Header().Set("Content-Type", typ)
			} else {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Disposition", "attachment")
			}
			static.ServeHTTP(w, r)
			return
		}
		worker.ServeHTTP(w, r)
	})
}

// docsInlineMIME returns a fixed MIME type for assets that may appear inline in
// a document. Every other file must be downloaded instead. SVG stays inline so
// Markdown images keep rendering: an <img> never runs SVG scripts, and the
// asset CSP (no scripts, sandboxed) also covers opening the file directly.
func docsInlineMIME(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".apng":
		return "image/apng"
	case ".avif":
		return "image/avif"
	case ".bmp":
		return "image/bmp"
	case ".gif":
		return "image/gif"
	case ".ico":
		return "image/x-icon"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".svg":
		return "image/svg+xml"
	case ".tif", ".tiff":
		return "image/tiff"
	case ".webp":
		return "image/webp"
	case ".flac":
		return "audio/flac"
	case ".m4a":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".oga", ".ogg":
		return "audio/ogg"
	case ".wav":
		return "audio/wav"
	case ".mov":
		return "video/quicktime"
	case ".mp4":
		return "video/mp4"
	case ".ogv":
		return "video/ogg"
	case ".webm":
		return "video/webm"
	case ".otf":
		return "font/otf"
	case ".ttf":
		return "font/ttf"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".csv", ".log", ".txt":
		return "text/plain; charset=utf-8"
	default:
		return ""
	}
}

// docsRuntime materializes the docs app with a version's content and takes a
// reference to it. It also returns the path of the document image, in a
// state directory next to the runtime directory that only core creates and
// removes: a worker never creates directories, so it cannot write an image
// after the last reference released them.
func (s *Service) docsRuntime(v store.Version, versionDir string) (string, string, []string, func(), error) {
	app := s.cfg.DocsApp
	if app.FS == nil || len(app.Hash) < 12 || app.Entry == "" || app.ContentModule == "" || len(v.Hash) < 16 {
		return "", "", nil, nil, fmt.Errorf("%w: docs app is not configured", ErrRuntimeUnavailable)
	}
	var m bundle.Manifest
	if err := json.Unmarshal(v.Manifest, &m); err != nil {
		return "", "", nil, nil, err
	}
	c := docsContent{Format: 1, Hash: v.Hash, Entry: m.Entry, Title: m.Name, Documents: []docsDocument{}, Assets: []string{}}
	root, err := os.OpenRoot(versionDir)
	if err != nil {
		return "", "", nil, nil, err
	}
	defer root.Close()
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("invalid content file %q", p)
		}
		if bundle.IsMarkdown(p) {
			b, err := fs.ReadFile(root.FS(), p)
			if err != nil {
				return err
			}
			c.Documents = append(c.Documents, docsDocument{p, fmt.Sprintf("%x", sha256.Sum256(b)), string(b)})
		} else if p != bundle.ManifestName {
			c.Assets = append(c.Assets, p)
		}
		return nil
	})
	if err != nil {
		return "", "", nil, nil, err
	}
	sort.Slice(c.Documents, func(i, j int) bool { return c.Documents[i].Path < c.Documents[j].Path })
	sort.Strings(c.Assets)
	b, err := json.Marshal(c)
	if err != nil {
		return "", "", nil, nil, err
	}
	dir := filepath.Join(s.cfg.DataDir, "runtime", "docs", app.Hash[:12]+"-"+v.Hash[:16])
	// Materialization and reference acquisition are serialized against final release.
	s.docsMu.Lock()
	defer s.docsMu.Unlock()
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return "", "", nil, nil, err
		}
		tmp, err := os.MkdirTemp(filepath.Dir(dir), ".docs-")
		if err != nil {
			return "", "", nil, nil, err
		}
		defer os.RemoveAll(tmp)
		staged, err := os.OpenRoot(tmp)
		if err != nil {
			return "", "", nil, nil, err
		}
		defer staged.Close()
		err = fs.WalkDir(app.FS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() || p == app.ContentModule {
				return fmt.Errorf("invalid docs app module %q", p)
			}
			b, err := fs.ReadFile(app.FS, p)
			if err != nil {
				return err
			}
			if err := staged.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			return staged.WriteFile(p, b, 0o444)
		})
		if err != nil {
			return "", "", nil, nil, err
		}
		if err := staged.WriteFile(app.ContentModule, append(append([]byte("export default "), b...), ';', '\n'), 0o444); err != nil {
			return "", "", nil, nil, err
		}
		if err := os.Rename(tmp, dir); err != nil {
			return "", "", nil, nil, err
		}
	} else if err != nil {
		return "", "", nil, nil, err
	}
	state := dir + docsStateSuffix
	image := filepath.Join(state, docsImageName)
	// With no reference no worker of this directory runs, so leftovers of an
	// interrupted image write are stale. Without a state directory the
	// document simply starts without an image.
	if err := prepareDocsState(state, s.docsRefs[dir] == 0); err != nil {
		s.logf("docs runtime %s: image directory: %v", filepath.Base(dir), err)
		image = ""
	}
	s.docsRefs[dir]++
	release := func() {
		s.docsMu.Lock()
		defer s.docsMu.Unlock()
		s.docsRefs[dir]--
		if s.docsRefs[dir] == 0 {
			delete(s.docsRefs, dir)
			_ = os.RemoveAll(state)
			_ = os.RemoveAll(dir)
		}
	}
	return dir, image, c.Assets, release, nil
}

// The document image of a runtime directory dir is dir+docsStateSuffix/docsImageName.
const (
	docsStateSuffix = ".state"
	docsImageName   = "image"
)

// prepareDocsState creates the state directory (0700). With clear it also
// removes everything in it but the image.
func prepareDocsState(state string, clear bool) error {
	err := os.Mkdir(state, 0o700)
	if err == nil || !os.IsExist(err) || !clear {
		if os.IsExist(err) {
			err = nil
		}
		return err
	}
	entries, err := os.ReadDir(state)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != docsImageName {
			if err := os.RemoveAll(filepath.Join(state, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// cleanupDocs removes crash leftovers while holding the same lock as acquisition.
func (s *Service) cleanupDocs() {
	s.docsMu.Lock()
	defer s.docsMu.Unlock()
	parent := filepath.Join(s.cfg.DataDir, "runtime", "docs")
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		dir := filepath.Join(parent, e.Name())
		// A state directory belongs to its runtime directory.
		if s.docsRefs[strings.TrimSuffix(dir, docsStateSuffix)] == 0 {
			_ = os.RemoveAll(dir)
		}
	}
}
