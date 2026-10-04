package core

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

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

// trustedAccess clones the request and replaces access and strips all spellings of the host-only health signal.
func trustedAccess(r *http.Request, public bool) *http.Request {
	r = r.Clone(r.Context())
	for key := range r.Header {
		if strings.EqualFold(key, "X-Flats-Access") || strings.EqualFold(key, "X-Flats-Health") {
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

func docsAssets(worker http.Handler, dir string, assets []string) http.Handler {
	listed := make(map[string]bool, len(assets))
	for _, p := range assets {
		listed["/"+p] = true
	}
	static := &site.Static{Dir: dir}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && listed[r.URL.Path] {
			static.ServeHTTP(w, r)
			return
		}
		worker.ServeHTTP(w, r)
	})
}

func (s *Service) docsRuntime(v store.Version, versionDir string) (string, []string, func(), error) {
	app := s.cfg.DocsApp
	if app.FS == nil || len(app.Hash) < 12 || app.Entry == "" || app.ContentModule == "" || len(v.Hash) < 16 {
		return "", nil, nil, fmt.Errorf("%w: docs app is not configured", ErrRuntimeUnavailable)
	}
	var m bundle.Manifest
	if err := json.Unmarshal(v.Manifest, &m); err != nil {
		return "", nil, nil, err
	}
	c := docsContent{Format: 1, Hash: v.Hash, Entry: m.Entry, Title: m.Name, Documents: []docsDocument{}, Assets: []string{}}
	root, err := os.OpenRoot(versionDir)
	if err != nil {
		return "", nil, nil, err
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
		return "", nil, nil, err
	}
	sort.Slice(c.Documents, func(i, j int) bool { return c.Documents[i].Path < c.Documents[j].Path })
	sort.Strings(c.Assets)
	b, err := json.Marshal(c)
	if err != nil {
		return "", nil, nil, err
	}
	dir := filepath.Join(s.cfg.DataDir, "runtime", "docs", app.Hash[:12]+"-"+v.Hash[:16])
	// Materialization and reference acquisition are serialized against final release.
	s.docsMu.Lock()
	defer s.docsMu.Unlock()
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return "", nil, nil, err
		}
		tmp, err := os.MkdirTemp(filepath.Dir(dir), ".docs-")
		if err != nil {
			return "", nil, nil, err
		}
		defer os.RemoveAll(tmp)
		staged, err := os.OpenRoot(tmp)
		if err != nil {
			return "", nil, nil, err
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
			return "", nil, nil, err
		}
		if err := staged.WriteFile(app.ContentModule, append(append([]byte("export default "), b...), ';', '\n'), 0o444); err != nil {
			return "", nil, nil, err
		}
		if err := os.Rename(tmp, dir); err != nil {
			return "", nil, nil, err
		}
	} else if err != nil {
		return "", nil, nil, err
	}
	s.docsRefs[dir]++
	release := func() {
		s.docsMu.Lock()
		defer s.docsMu.Unlock()
		s.docsRefs[dir]--
		if s.docsRefs[dir] == 0 {
			delete(s.docsRefs, dir)
			_ = os.RemoveAll(dir)
		}
	}
	return dir, c.Assets, release, nil
}

// cleanupDocs removes crash leftovers while holding the same lock as acquisition.
func (s *Service) cleanupDocs() {
	s.docsMu.Lock()
	defer s.docsMu.Unlock()
	parent := filepath.Join(s.cfg.DataDir, "runtime", "docs")
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		dir := filepath.Join(parent, e.Name())
		if s.docsRefs[dir] == 0 {
			_ = os.RemoveAll(dir)
		}
	}
}
