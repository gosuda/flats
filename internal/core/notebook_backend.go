package core

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/store"
)

type notebookBackend struct {
	s    *Service
	slug string
}

var _ notebook.Backend = notebookBackend{}
var _ notebook.HistoryBackend = notebookBackend{}

func (b notebookBackend) NotebookReadRevision(ctx context.Context, identity string, seq int64) (*int, bool, error) {
	state, err := b.s.st.ContentState(ctx, b.slug)
	if err != nil {
		return nil, false, err
	}
	imported := state.ImportStore == identity && state.ImportSeq >= seq
	if !imported {
		return nil, false, nil
	}
	n, ok, err := b.s.st.NotebookImportRevision(ctx, b.slug, identity, seq)
	if err != nil || !ok {
		return nil, imported, err
	}
	return &n, imported, nil
}

func (b notebookBackend) Limits() notebook.Limits {
	return notebook.Limits{DiskQuota: max(b.s.diskQuota(), 0), UploadMax: b.s.UploadLimit()}
}
func (b notebookBackend) MeasureOther(context.Context) (int64, error) {
	return b.s.notebookOther(b.slug)
}
func (b notebookBackend) Lease(hashes []string) func() { return b.s.lease(b.slug, hashes) }
func (b notebookBackend) OutboxReady()                 { b.s.notebookPending.Store(b.slug, true) }
func (b notebookBackend) PutBlob(_ context.Context, hash string, data []byte) (bool, error) {
	if hashBytes(data) != hash {
		return false, fmt.Errorf("blob content does not match its hash %.12s", hash)
	}
	release := b.s.lease(b.slug, []string{hash})
	defer release()
	root := b.s.docsStoreDir(b.slug)
	p, ok := blobPath(root, hash)
	if !ok {
		return false, fmt.Errorf("blob hash %q is not a SHA-256", hash)
	}
	if _, err := os.Stat(p); err == nil {
		now := time.Now()
		if err := os.Chtimes(p, now, now); err != nil {
			return false, err
		}
		return false, nil
	}
	_, err := putBlob(root, data)
	return err == nil, err
}

func (b notebookBackend) ReadBlob(_ context.Context, hash string) ([]byte, error) {
	release := b.s.lease(b.slug, []string{hash})
	defer release()
	data, err := readBlob(b.s.docsStoreDir(b.slug), hash)
	return data, toNotebookError(err)
}

func (b notebookBackend) RevisionAsset(ctx context.Context, hash string) (string, bool, error) {
	return b.s.st.RevisionAssetPath(ctx, b.slug, hash)
}

func (b notebookBackend) Revisions(ctx context.Context, before, limit int) (notebook.RevisionPage, error) {
	page, err := b.s.ListRevisions(ctx, b.slug, before, limit)
	if err != nil {
		return notebook.RevisionPage{}, toNotebookError(err)
	}
	out := notebook.RevisionPage{Revisions: []notebook.RevisionInfo{}, Next: page.Next}
	for _, r := range page.Revisions {
		info := notebook.RevisionInfo{Number: r.Number, Kind: r.Kind, Message: r.Message, Authors: r.Authors, Pinned: r.Pinned, Created: r.CreatedAt}
		for _, p := range r.NotebookPages {
			title := p.TitleAfter
			if p.Change == "removed" {
				title = p.TitleBefore
			}
			info.Pages = append(info.Pages, notebook.PageChange{Page: notebook.ID(p.Page), Title: title, Change: p.Change})
		}
		out.Revisions = append(out.Revisions, info)
	}
	return out, nil
}

func (b notebookBackend) RevisionSource(ctx context.Context, n int) (notebook.RevisionSource, func(), error) {
	if err := b.s.catchUpNotebook(ctx, b.slug); err != nil {
		return notebook.RevisionSource{}, func() {}, toNotebookError(err)
	}
	unlock, err := b.s.lockStorageCtx(ctx, b.slug)
	if err != nil {
		return notebook.RevisionSource{}, func() {}, toNotebookError(err)
	}
	src, release, err := b.s.notebookRevisionSourceLocked(ctx, b.slug, n)
	unlock()
	return src, release, toNotebookError(err)
}

func (s *Service) notebookRevisionSourceLocked(ctx context.Context, slugName string, n int) (notebook.RevisionSource, func(), error) {
	if _, err := s.st.Revision(ctx, slugName, n); err != nil {
		return notebook.RevisionSource{}, func() {}, err
	}
	files, err := s.st.RevisionFiles(ctx, slugName, n)
	if err != nil {
		return notebook.RevisionSource{}, func() {}, err
	}
	hashes := make([]string, 0, len(files))
	allowed := map[string]bool{}
	for _, f := range files {
		hashes = append(hashes, f.Hash)
		allowed[f.Hash] = true
	}
	release := s.lease(slugName, hashes)
	src := notebook.RevisionSource{Number: n, Files: notebookFiles(files), Read: func(ctx context.Context, hash string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !allowed[hash] {
			return nil, store.ErrNotFound
		}
		return readBlob(s.docsStoreDir(slugName), hash)
	}, Open: func(ctx context.Context, hash string) (io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !allowed[hash] {
			return nil, store.ErrNotFound
		}
		releaseReader := s.lease(slugName, []string{hash})
		reader, err := openBlob(ctx, s.docsStoreDir(slugName), hash)
		if err != nil {
			releaseReader()
			return nil, err
		}
		return &revisionBlobReader{ReadCloser: reader, release: releaseReader}, nil
	}}
	return src, release, nil
}

// A reader keeps its own lease even if the source owner releases first.
type revisionBlobReader struct {
	io.ReadCloser
	release func()
	once    sync.Once
	err     error
}

func (r *revisionBlobReader) Close() error {
	r.once.Do(func() {
		r.err = r.ReadCloser.Close()
		r.release()
	})
	return r.err
}

func (b notebookBackend) Pin(ctx context.Context, n int, pinned bool) error {
	return toNotebookError(b.s.PinRevision(ctx, b.slug, n, pinned))
}
func (b notebookBackend) Rollback(ctx context.Context, n int, label string) (int, error) {
	rev, err := b.s.RollbackRevision(ctx, b.slug, n, label, ViaConsole)
	return rev, toNotebookError(err)
}
func (b notebookBackend) Exclusive(ctx context.Context, fn func(context.Context, int64) error) error {
	unlock, err := b.s.lockStorageCtx(ctx, b.slug)
	if err != nil {
		return toNotebookError(err)
	}
	defer unlock()
	other, err := b.s.notebookOther(b.slug)
	if err != nil {
		return &notebook.WriteError{Code: "unavailable", Reason: "measurement", Retry: true}
	}
	if err := fn(ctx, other); err != nil {
		return err
	}
	if in := b.s.liveNotebook(b.slug); in != nil {
		if _, err := b.s.importNotebookLocked(context.WithoutCancel(ctx), b.slug, in); err != nil {
			b.s.Event(ctx, b.slug, "warn", "revision", "could not record the notebook revision yet: "+err.Error(), nil)
		}
	}
	return nil
}

func (s *Service) rollbackNotebook(ctx context.Context, slugName string, n int, label string, via Via) (RevisionRollback, error) {
	unlock, err := s.lockStorageCtx(ctx, slugName)
	if err != nil {
		return RevisionRollback{}, err
	}
	defer unlock()
	in, err := s.runningNotebook(ctx, slugName)
	if err != nil {
		return RevisionRollback{}, err
	}
	if _, err := s.importNotebookLocked(ctx, slugName, in); err != nil {
		s.logf("notebook revisions of %s: %v", slugName, err)
	}
	var boundary int64
	err = in.ReadTx(ctx, func(tx *sql.Tx, _ notebook.Sequence) error {
		return tx.QueryRowContext(ctx, `SELECT coalesce((SELECT seq FROM sqlite_sequence WHERE name='outbox'),0)`).Scan(&boundary)
	})
	if err != nil {
		return RevisionRollback{}, notebookErr(err)
	}
	op := fmt.Sprintf("notebook-rollback:%s:%d", in.StoreID(), boundary)
	err = s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
		if _, err := tx.Revision(ctx, n); err != nil {
			return err
		}
		return tx.AddHold(ctx, op, n)
	})
	if err != nil {
		return RevisionRollback{}, err
	}
	settle := func() { s.settleNotebookHoldsLocked(context.WithoutCancel(ctx), slugName, in) }
	src, release, err := s.notebookRevisionSourceLocked(ctx, slugName, n)
	if err != nil {
		settle()
		return RevisionRollback{}, err
	}
	defer release()
	_, err = in.Rollback(ctx, src, label)
	if err != nil {
		settle()
		return RevisionRollback{}, notebookErr(err)
	}
	var seq int64
	err = in.ReadTx(context.WithoutCancel(ctx), func(tx *sql.Tx, _ notebook.Sequence) error {
		return tx.QueryRowContext(context.WithoutCancel(ctx), `SELECT seq FROM outbox WHERE seq>? AND kind='rollback' AND rollback_target=? ORDER BY seq LIMIT 1`, boundary, n).Scan(&seq)
	})
	out := NotebookWriteResult{Pending: true}
	if err == nil {
		s.notebookRevisionLocked(context.WithoutCancel(ctx), slugName, in, seq, &out)
	}
	warning := ""
	if out.Pending {
		warning = "the rollback applied, but its revision is not recorded yet; do not repeat the rollback"
	} else if out.Revision == 0 {
		warning = "the rollback applied; its revision was already deleted by retention"
	}
	s.Event(ctx, slugName, "info", "revision", fmt.Sprintf("restored revision %d via %s", n, via), map[string]int{"rollback_of": n, "revision": out.Revision})
	return RevisionRollback{Revision: out.Revision, Warning: warning}, nil
}
