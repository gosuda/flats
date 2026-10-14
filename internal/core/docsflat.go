package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/store"
)

var (
	// ErrDocsUnversioned refuses version, Draft, preview, publish and
	// rollback operations on a docs flat: a document has none of them. Its
	// saves apply at once and live edits change its text directly.
	ErrDocsUnversioned = fmt.Errorf("%w: documents have no versions, drafts, previews or publishing; save_document applies at once and update_document edits the live text", ErrInvalid)
)

// isDocs reports whether a flat is a document: its live version is docs,
// or, while nothing runs, its Draft is.
func (s *Service) isDocs(ctx context.Context, f store.Flat) (bool, error) {
	t, err := s.flatType(ctx, f)
	return t == contenttype.Docs, err
}

// refuseDocs returns ErrDocsUnversioned for a docs flat.
func (s *Service) refuseDocs(ctx context.Context, f store.Flat) error {
	docs, err := s.isDocs(ctx, f)
	if err != nil {
		return err
	}
	if docs {
		return ErrDocsUnversioned
	}
	return nil
}

// applyDocsLocked makes a just saved docs Draft revision the running
// document without approval. Unchanged content leaves the running document
// as it is. The caller holds s.lock(slugName).
func (s *Service) applyDocsLocked(ctx context.Context, slugName string, revision int, via Via) error {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return err
	}
	rev, err := s.st.GetDraftRevision(ctx, slugName, revision)
	if err != nil {
		return err
	}
	if f.LiveVersion > 0 {
		if v, err := s.st.GetVersion(ctx, slugName, f.LiveVersion); err == nil && v.Hash == rev.Hash {
			if lf := s.state(slugName); lf.cur.Load() != nil {
				return nil
			}
		}
	}
	if err := s.verifyContent(slugName, versionFromRevision(rev)); err != nil {
		return err
	}
	_, err = s.publishRevision(ctx, f, rev, "", via, "save")
	var de *DeployError
	if errors.As(err, &de) {
		de.Document = true
	}
	return err
}

// startSavedDocs starts documents saved before saves applied at once: a
// docs flat that has a Draft but has never run. Failures are logged; the
// next save retries.
func (s *Service) startSavedDocs(ctx context.Context) {
	flats, err := s.st.ListFlats(ctx)
	if err != nil {
		s.logf("start saved documents: %v", err)
		return
	}
	for _, f := range flats {
		if f.LiveVersion != 0 {
			continue
		}
		d, err := s.st.GetDraft(ctx, f.Slug)
		if err != nil || contenttype.FromManifest(d.Manifest) != contenttype.Docs {
			continue
		}
		var unlock func()
		if unlock, err = s.lockStorage(f.Slug); err == nil {
			err = s.applyDocsLocked(ctx, f.Slug, d.Revision, ViaConsole)
			unlock()
		}
		if err != nil {
			s.Event(ctx, f.Slug, "error", "deploy", "could not start the saved document: "+err.Error(), nil)
		}
	}
}

// CreateDocument creates a docs flat holding one index.md headed by name
// and starts it at once: a document is Private and running without a
// publish step. An existing slug is refused.
func (s *Service) CreateDocument(ctx context.Context, slugName, name string, via Via) (FlatView, error) {
	if _, err := s.CreateFlat(ctx, slugName, name, via); err != nil {
		return FlatView{}, err
	}
	title := strings.Join(strings.Fields(name), " ")
	if title == "" {
		title = slugName
	}
	manifest, err := json.Marshal(map[string]string{"type": contenttype.Docs, "name": title})
	if err != nil {
		return FlatView{}, err
	}
	files := []bundle.File{{Path: "index.md", Data: []byte("# " + title + "\n")}, {Path: bundle.ManifestName, Data: manifest}}
	if _, err := s.SaveVersion(ctx, slugName, files, SaveMeta{Message: "created"}, via); err != nil {
		return FlatView{}, err
	}
	return s.GetFlat(ctx, slugName)
}
