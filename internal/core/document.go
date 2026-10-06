package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/store"
)

var (
	ErrNotDocs          = fmt.Errorf("%w: flat is not a docs flat", ErrInvalid)
	ErrDocumentNotFound = fmt.Errorf("%w: no such document", store.ErrNotFound)
)

// DocumentConflict describes retained private recovery text without including it.
type DocumentConflict struct {
	Generation int64 `json:"generation"`
	Bytes      int64 `json:"bytes"`
}

// Document is exact Markdown from the live app, Current Draft, or preserved conflict.
type Document struct {
	Generation int64              `json:"generation,omitempty"`
	Conflicts  []DocumentConflict `json:"conflicts,omitempty"`
	Format     int                `json:"format"`
	Doc        string             `json:"doc"`
	Markdown   string             `json:"markdown"`
	Epoch      string             `json:"epoch,omitempty"`
	Chain      string             `json:"chain,omitempty"`
	Seq        int64              `json:"seq"`
	Source     string             `json:"source"`
}

// GetDocument prefers the running docs version, including collaborative edits.
// It never starts a worker just to read an unpublished Draft.
func (s *Service) GetDocument(ctx context.Context, slugName, doc string) (Document, error) {
	return s.getDocument(ctx, slugName, doc, 0)
}

// GetDocumentConflict reads preserved text through the trusted host route only.
func (s *Service) GetDocumentConflict(ctx context.Context, slugName, doc string, generation int64) (Document, error) {
	if generation < 1 {
		return Document{}, invalidf("conflict must be a positive generation")
	}
	return s.getDocument(ctx, slugName, doc, generation)
}

func (s *Service) getDocument(ctx context.Context, slugName, doc string, conflict int64) (Document, error) {
	unlock := s.lock(slugName)
	defer unlock()
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return Document{}, err
	}
	if doc != "" && (!fs.ValidPath(doc) || !bundle.IsMarkdown(doc)) {
		return Document{}, invalidf("doc must be a relative .md or .markdown path")
	}
	lf := s.state(slugName)
	live := lf.cur.Load()
	if live != nil && contenttype.FromManifest(live.version.Manifest) == contenttype.Docs {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		path := "/_docs/api/document?doc=" + url.QueryEscape(doc)
		if conflict != 0 {
			path = fmt.Sprintf("/_docs/api/conflict?doc=%s&generation=%d", url.QueryEscape(doc), conflict)
		}
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://docs.internal"+path, nil)
		rec := httptest.NewRecorder()
		live.handler.ServeHTTP(rec, trustedAccess(req, false))
		if rec.Code == http.StatusNotFound {
			return Document{}, ErrDocumentNotFound
		}
		if rec.Code != http.StatusOK {
			return Document{}, fmt.Errorf("%w: docs app returned HTTP %d", ErrUnavailable, rec.Code)
		}
		if conflict != 0 {
			var row struct {
				Generation int64  `json:"generation"`
				Markdown   string `json:"markdown"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
				return Document{}, err
			}
			if row.Generation != conflict {
				return Document{}, fmt.Errorf("invalid conflict response")
			}
			if doc == "" {
				var m bundle.Manifest
				if err := json.Unmarshal(live.version.Manifest, &m); err != nil {
					return Document{}, err
				}
				doc = m.Entry
			}
			return Document{Format: 1, Doc: doc, Markdown: row.Markdown, Source: "conflict", Generation: conflict}, nil
		}
		var out Document
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			return Document{}, fmt.Errorf("docs app document response: %w", err)
		}
		if out.Format != 1 || !fs.ValidPath(out.Doc) || !bundle.IsMarkdown(out.Doc) {
			return Document{}, fmt.Errorf("docs app returned an invalid document response")
		}
		out.Source = "live"
		return out, nil
	}
	if conflict != 0 {
		return Document{}, ErrDocumentNotFound
	}
	draft, err := s.st.GetDraft(ctx, slugName)
	if errors.Is(err, store.ErrNotFound) {
		if live != nil {
			return Document{}, ErrNotDocs
		}
		return Document{}, fmt.Errorf("%w: no docs version is running and no Current Draft exists", ErrNotDeployed)
	}
	if err != nil {
		return Document{}, err
	}
	if contenttype.FromManifest(draft.Manifest) != contenttype.Docs {
		return Document{}, ErrNotDocs
	}
	var m bundle.Manifest
	if err := json.Unmarshal(draft.Manifest, &m); err != nil {
		return Document{}, err
	}
	if doc == "" {
		doc = m.Entry
	}
	if !fs.ValidPath(doc) || !bundle.IsMarkdown(doc) {
		return Document{}, ErrDocumentNotFound
	}
	root, err := os.OpenRoot(s.contentDir(slugName, store.Version{Revision: draft.Revision, Role: "draft"}))
	if err != nil {
		return Document{}, err
	}
	defer root.Close()
	b, err := root.ReadFile(doc)
	if os.IsNotExist(err) {
		return Document{}, ErrDocumentNotFound
	}
	if err != nil {
		return Document{}, err
	}
	return Document{Format: 1, Doc: doc, Markdown: string(b), Source: "draft"}, nil
}
