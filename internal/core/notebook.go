package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
)

var ErrNotebookUnversioned = fmt.Errorf("%w: notebooks have no versions, drafts, previews or publishing; create one with create_flat {type: notebook}, write it with update_notebook, and use the revision tools (list_revisions, get_revision, rollback_revision, pin_revision) for history", ErrInvalid)
var ErrNotNotebook = fmt.Errorf("%w: flat is not a notebook", ErrInvalid)
var ErrNotebookCapacity = errors.New("notebook capacity")

// notebookErr preserves the original refusal so transports can inspect its detail.
func notebookErr(err error) error {
	var problem *notebook.WriteError
	if !errors.As(err, &problem) {
		if errors.Is(err, ErrUnavailable) {
			return fmt.Errorf("%w: %w", err, &notebook.WriteError{Code: "unavailable", Reason: "notebook_unavailable", Retry: true, Detail: err.Error()})
		}
		return err
	}
	kind := map[string]error{"invalid": ErrInvalid, "conflict": ErrConflict, "notebook_capacity": ErrNotebookCapacity, "unavailable": ErrUnavailable, "not_found": store.ErrNotFound}[problem.Code]
	if kind == nil {
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}
func toNotebookError(err error) error {
	var problem *notebook.WriteError
	if err == nil || errors.As(err, &problem) {
		return err
	}
	for _, m := range []struct {
		kind error
		code string
	}{{store.ErrNotFound, "not_found"}, {ErrNotebookCapacity, "notebook_capacity"}, {ErrConflict, "conflict"}, {ErrInvalid, "invalid"}, {ErrUnavailable, "unavailable"}} {
		if errors.Is(err, m.kind) {
			return &notebook.WriteError{Code: m.code, Reason: err.Error(), Retry: m.code == "unavailable"}
		}
	}
	return err
}

func (s *Service) refuseNotebookVersion(ctx context.Context, slugName string) error {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return err
	}
	typ, err := s.flatType(ctx, f)
	if err != nil {
		return err
	}
	if typ == contenttype.Notebook {
		return ErrNotebookUnversioned
	}
	return nil
}

func (s *Service) notebookDir(slugName string) string {
	return filepath.Join(s.flatDir(slugName), "notebook")
}

func (s *Service) notebookDB(slugName string) string {
	return filepath.Join(s.notebookDir(slugName), "notebook.db")
}

func (s *Service) notebookOther(slugName string) (int64, error) {
	db := s.notebookDB(slugName)
	return walkSize(s.flatDir(slugName), []string{db, db + "-wal", db + "-shm"})
}

func (s *Service) liveNotebook(slugName string) *notebook.Instance {
	s.mu.Lock()
	lf := s.live[slugName]
	s.mu.Unlock()
	if lf == nil {
		return nil
	}
	if d := lf.cur.Load(); d != nil {
		if in, ok := d.inst.(*notebook.Instance); ok {
			return in
		}
	}
	return nil
}

func (s *Service) buildNotebook(slugName string, v store.Version, title, dataDir string, env *runtimeEnvironment) (*deployed, error) {
	identity := func(text string) string { return text }
	if dataDir != s.dataDirOf(slugName) {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
		return &deployed{version: v, handler: h, redact: identity, environment: env}, nil
	}
	in, _, err := notebook.Open(s.notebookDir(slugName), notebookBackend{s: s, slug: slugName}, notebook.Options{Slug: slugName, Title: title, Now: s.now, Spills: s.notebookSpills()})
	if err != nil {
		return nil, fmt.Errorf("open the notebook: %w", err)
	}
	s.notebookPending.Store(slugName, true)
	return &deployed{version: v, handler: in, inst: in, redact: identity, environment: env}, nil
}

func (s *Service) CreateNotebook(ctx context.Context, slugName, name string, via Via) (FlatView, error) {
	if err := slug.Validate(slugName); err != nil {
		return FlatView{}, invalid(err)
	}
	unlock, err := s.lockCtx(ctx, slugName)
	if err != nil {
		return FlatView{}, err
	}
	defer unlock()
	if _, err := s.createNotebookLocked(ctx, slugName, name, via); err != nil {
		return FlatView{}, err
	}
	return s.GetFlat(ctx, slugName)
}

func (s *Service) createNotebookLocked(ctx context.Context, slugName, name string, via Via) (*notebook.Instance, error) {
	title := strings.Join(strings.Fields(name), " ")
	if title == "" {
		title = slugName
	}
	if _, err := s.st.GetFlat(ctx, slugName); err == nil {
		return nil, fmt.Errorf("%w: flat %q already exists", ErrConflict, slugName)
	}
	if _, err := s.createFlatLocked(ctx, slugName, title, via); err != nil {
		return nil, err
	}
	return s.initNotebookLocked(ctx, slugName, title, via, func() { _ = s.st.DeleteFlat(ctx, slugName) })
}

func (s *Service) initNotebookLocked(ctx context.Context, slugName, title string, via Via, undo func()) (*notebook.Instance, error) {
	err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.InitState(ctx, contenttype.Notebook) })
	if err != nil {
		undo()
		return nil, err
	}
	return s.finishNotebookLocked(ctx, slugName, title, via)
}

func (s *Service) finishNotebookLocked(ctx context.Context, slugName, title string, via Via) (*notebook.Instance, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return nil, err
	}
	if f.LiveVersion == 0 {
		manifest, _ := json.Marshal(map[string]string{"type": contenttype.Notebook, "name": title})
		v, err := s.saveDraftLocked(ctx, slugName, []bundle.File{{Path: bundle.ManifestName, Data: manifest}}, SaveMeta{Message: "notebook shell", quiet: true}, via)
		if err != nil {
			return nil, err
		}
		rev, err := s.st.GetDraftRevision(ctx, slugName, v.Revision)
		if err != nil {
			return nil, err
		}
		if f, err = s.st.GetFlat(ctx, slugName); err != nil {
			return nil, err
		}
		if _, err := s.publishRevision(ctx, f, rev, "", via, "save"); err != nil {
			return nil, err
		}
	}
	in := s.liveNotebook(slugName)
	if in == nil {
		return nil, fmt.Errorf("%w: notebook %s is not running; get_logs has details", ErrUnavailable, slugName)
	}
	res, err := s.importNotebookLocked(ctx, slugName, in)
	if err != nil {
		return in, err
	}
	if err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.SetPhase(ctx, "ready") }); err != nil {
		return in, err
	}
	if len(res.revisions) > 0 {
		s.Event(ctx, slugName, "info", "notebook", fmt.Sprintf("created notebook (revision %d) via %s", res.revisions[0], via), map[string]int{"revision": res.revisions[0]})
	}
	return in, nil
}

func (s *Service) startPendingNotebook(ctx context.Context, slugName string) error {
	unlock, err := s.lockStorage(slugName)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return err
	}
	_, err = s.finishNotebookLocked(ctx, slugName, f.Name, ViaConsole)
	return err
}

func (s *Service) writeNotebookLocked(ctx context.Context, slugName string, via Via, write func(in *notebook.Instance, other int64) error) (in *notebook.Instance, created, committed bool, err error) {
	run := func(in *notebook.Instance) error {
		other, err := s.notebookOther(slugName)
		if err != nil {
			return fmt.Errorf("%w: the notebook's disk usage cannot be measured: %v", ErrUnavailable, err)
		}
		return notebookErr(write(in, other))
	}
	f, err := s.st.GetFlat(ctx, slugName)
	fresh := errors.Is(err, store.ErrNotFound)
	if fresh {
		if _, err = s.createFlatLocked(ctx, slugName, slugName, via); err == nil {
			f, err = s.st.GetFlat(ctx, slugName)
		}
	}
	if err != nil {
		return nil, false, false, err
	}
	t, err := s.flatType(ctx, f)
	if err != nil {
		return nil, false, false, err
	}
	switch {
	case t != "" && t != contenttype.Notebook:
		return nil, false, false, fmt.Errorf("%w: %s is a %s", ErrNotNotebook, slugName, typeNoun(t))
	case t == contenttype.Notebook:
		in := s.liveNotebook(slugName)
		if in == nil {
			if st, err := s.st.ContentState(ctx, slugName); err != nil || st.Phase != "init" {
				return nil, false, false, fmt.Errorf("%w: notebook %s is not running; get_logs has details", ErrUnavailable, slugName)
			}
			// Recover the durable outcome before attempting host shell publication.
			pending, _, err := notebook.Open(s.notebookDir(slugName), notebookBackend{s: s, slug: slugName}, notebook.Options{Slug: slugName, Title: f.Name, Now: s.now, Spills: s.notebookSpills()})
			if err != nil {
				return nil, false, false, fmt.Errorf("open the notebook: %w", err)
			}
			err = run(pending)
			pending.Stop()
			if err != nil {
				return nil, false, false, err
			}
			in, err = s.finishNotebookLocked(ctx, slugName, f.Name, via)
			return in, false, true, err
		}
		err = run(in)
		return in, false, err == nil, err
	}
	blobs := s.docsStoreDir(slugName)
	hadBlobs, err := pathExists(blobs)
	if err != nil {
		return nil, false, false, err
	}
	undo := func() {
		_ = os.RemoveAll(s.notebookDir(slugName))
		if !hadBlobs {
			_ = os.RemoveAll(blobs)
		}
		if fresh {
			if _, err := s.deleteLocked(context.WithoutCancel(ctx), slugName, via, ""); err != nil {
				s.logf("remove notebook %s after a refused first write: %v", slugName, err)
			}
		}
	}
	first, _, err := notebook.Open(s.notebookDir(slugName), notebookBackend{s: s, slug: slugName}, notebook.Options{Slug: slugName, Title: f.Name, Now: s.now, Spills: s.notebookSpills()})
	if err != nil {
		undo()
		return nil, false, false, fmt.Errorf("open the notebook: %w", err)
	}
	err = run(first)
	first.Stop()
	if err != nil {
		undo()
		return nil, false, false, err
	}
	if err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.InitState(ctx, contenttype.Notebook) }); err != nil {
		undo()
		return nil, false, false, err
	}
	// Adoption is durable; initialization failures must not undo the committed edit.
	in, err = s.finishNotebookLocked(ctx, slugName, f.Name, via)
	return in, true, true, err
}
