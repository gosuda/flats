package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/store"
)

type sheetBackend struct {
	s    *Service
	slug string
}

var _ sheet.Backend = sheetBackend{}
var _ sheet.HistoryBackend = sheetBackend{}

func (b sheetBackend) Limits() sheet.Limits {
	return sheet.Limits{DiskQuota: max(b.s.diskQuota(), 0)}
}

func (b sheetBackend) MeasureOther(ctx context.Context) (int64, error) {
	return b.s.sheetOtherCtx(ctx, b.slug)
}

func (b sheetBackend) Lease(hashes []string) func() { return b.s.lease(b.slug, hashes) }
func (b sheetBackend) OutboxReady()                 { b.s.sheetPending.Store(b.slug, true) }

func (b sheetBackend) PutBlob(_ context.Context, hash string, data []byte) (bool, error) {
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

// OpenBlob leases the blob until the reader is closed. The reader stops
// with ctx and checks the content against the hash at its end (openBlob).
func (b sheetBackend) OpenBlob(ctx context.Context, hash string) (io.ReadCloser, error) {
	release := b.s.lease(b.slug, []string{hash})
	r, err := openBlob(ctx, b.s.docsStoreDir(b.slug), hash)
	if err != nil {
		release()
		return nil, toSheetError(err)
	}
	return leasedBlob{r, release}, nil
}

// leasedBlob is a blob reader that releases its lease when it is closed.
type leasedBlob struct {
	*blobReader
	release func()
}

func (l leasedBlob) Close() error {
	defer l.release()
	return l.blobReader.Close()
}

func (b sheetBackend) Exclusive(ctx context.Context, fn func(context.Context, int64) error) error {
	unlock, err := b.s.lockStorageCtx(ctx, b.slug)
	if err != nil {
		return toSheetError(err)
	}
	defer unlock()
	other, err := b.s.sheetOther(b.slug)
	if err != nil {
		return &sheet.WriteError{Code: "unavailable", Reason: "measurement", Retry: true}
	}
	if err := fn(ctx, other); err != nil {
		return err
	}
	if in := b.s.liveSheet(b.slug); in != nil {
		if _, err := b.s.importSheetLocked(context.WithoutCancel(ctx), b.slug, in); err != nil {
			b.s.Event(ctx, b.slug, "warn", "revision", "could not record the sheet revision yet: "+err.Error(), nil)
		}
	}
	return nil
}

func (b sheetBackend) Revisions(ctx context.Context, before, limit int) (sheet.RevisionPage, error) {
	page, err := b.s.ListRevisions(ctx, b.slug, before, limit)
	if err != nil {
		return sheet.RevisionPage{}, toSheetError(err)
	}
	out := sheet.RevisionPage{Revisions: []sheet.RevisionInfo{}, Next: page.Next}
	for _, r := range page.Revisions {
		info := sheet.RevisionInfo{Number: r.Number, Kind: r.Kind, Message: r.Message, Authors: r.Authors, Pinned: r.Pinned, CreatedAt: r.CreatedAt, RollbackOf: r.RollbackOf, Tabs: []sheet.RevisionTab{}}
		for _, t := range r.SheetTabs {
			name := t.NameAfter
			if t.Change == "removed" {
				name = t.NameBefore
			}
			tab := sheet.RevisionTab{ID: t.Tab, Name: name, Change: t.Change}
			if t.Change == "changed" && t.NameBefore != t.NameAfter {
				tab.NameBefore, tab.NameAfter = t.NameBefore, t.NameAfter
			}
			info.Tabs = append(info.Tabs, tab)
		}
		out.Revisions = append(out.Revisions, info)
	}
	return out, nil
}

// RevisionFiles returns the files of revision n under a lease and the
// calculation date the revision closed with.
func (b sheetBackend) RevisionFiles(ctx context.Context, n int) ([]sheet.File, string, func(), error) {
	none := func() {}
	if err := b.s.catchUpSheetKeeping(ctx, b.slug, n); err != nil {
		return nil, "", none, toSheetError(err)
	}
	unlock, err := b.s.lockSheetCtx(ctx, b.slug)
	if err != nil {
		return nil, "", none, toSheetError(err)
	}
	files, date, release, err := b.s.sheetRevisionFilesLocked(ctx, b.slug, n)
	unlock()
	return files, date, release, toSheetError(err)
}

func (s *Service) sheetRevisionFilesLocked(ctx context.Context, slugName string, n int) ([]sheet.File, string, func(), error) {
	none := func() {}
	if _, err := s.st.Revision(ctx, slugName, n); err != nil {
		return nil, "", none, err
	}
	date, ok, err := s.st.SheetRevisionDate(ctx, slugName, n)
	if err != nil {
		return nil, "", none, err
	}
	if !ok {
		return nil, "", none, fmt.Errorf("%w: revision %d of %s has no calculation date", ErrUnavailable, n, slugName)
	}
	stored, err := s.sheetRevisionList(ctx, slugName, n)
	if err != nil {
		return nil, "", none, err
	}
	files := sheetFiles(stored)
	hashes := make([]string, len(files))
	for i, f := range files {
		hashes[i] = f.Hash
	}
	return files, date, s.lease(slugName, hashes), nil
}

// sheetRevisionList reads the file list of revision n of a sheet, which
// is refused as a damaged revision before it is held when it lists more
// files, or a longer path or hash, than a sheet revision has.
func (s *Service) sheetRevisionList(ctx context.Context, slugName string, n int) ([]store.RevisionFile, error) {
	files, err := s.st.BoundedRevisionFiles(ctx, slugName, n, store.RevisionFileBounds{
		Files: sheet.MaxRevisionFiles, PathBytes: sheet.MaxFilePathBytes, HashBytes: sheet.MaxFileHashBytes})
	if errors.Is(err, store.ErrRevisionFiles) {
		return nil, &sheet.WriteError{Code: "unavailable", Reason: "revision", Retry: true, Detail: err.Error()}
	}
	return files, err
}

// sheetRevisionEntries returns the file list of revision n of a sheet for
// a generic revision read, within the bounds of sheetRevisionList and in
// path order.
func (s *Service) sheetRevisionEntries(ctx context.Context, slugName string, n int) ([]store.RevisionFile, error) {
	files, err := s.sheetRevisionList(ctx, slugName, n)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(files, func(a, b store.RevisionFile) int { return strings.Compare(a.Path, b.Path) })
	return files, nil
}

// sheetRevisionFileContent reads one file of revision n of a sheet as the
// sheet reads a revision file: from a bounded list, at a path the writer
// writes, within its entry's size and the writer's most there, under a
// lease, ending with ctx and checked against its hash at its end.
func (s *Service) sheetRevisionFileContent(ctx context.Context, slugName string, n int, p string) ([]byte, store.RevisionFile, error) {
	var f store.RevisionFile
	if err := s.catchUpSheetKeeping(ctx, slugName, n); err != nil {
		return nil, f, err
	}
	unlock, err := s.lockSheetCtx(ctx, slugName)
	if err != nil {
		return nil, f, err
	}
	if _, err := s.st.Revision(ctx, slugName, n); err != nil {
		unlock()
		return nil, f, err
	}
	files, err := s.sheetRevisionList(ctx, slugName, n)
	found := false
	for _, x := range files {
		if x.Path == p {
			f, found = x, true
			break
		}
	}
	var release func()
	if found {
		release = s.lease(slugName, []string{f.Hash})
	}
	unlock()
	if err != nil {
		return nil, f, err
	}
	if !found {
		return nil, f, fmt.Errorf("%w: revision %d has no file %q", store.ErrNotFound, n, p)
	}
	defer release()
	backend := sheetBackend{s: s, slug: slugName}
	data, err := sheet.ReadRevisionFile(sheet.File{Path: f.Path, Hash: f.Hash, Size: f.Size},
		func(hash string) (io.ReadCloser, error) { return backend.OpenBlob(ctx, hash) })
	if err != nil {
		if ctx.Err() != nil {
			return nil, f, ctx.Err()
		}
		return nil, f, &sheet.WriteError{Code: "unavailable", Reason: "revision", Retry: true, Detail: err.Error()}
	}
	return data, f, nil
}

func sheetFiles(files []store.RevisionFile) []sheet.File {
	out := make([]sheet.File, len(files))
	for i, f := range files {
		out[i] = sheet.File{Path: f.Path, Hash: f.Hash, Size: f.Size}
	}
	return out
}

func (b sheetBackend) Pin(ctx context.Context, n int, pinned bool) error {
	return toSheetError(b.s.PinRevision(ctx, b.slug, n, pinned))
}

func (b sheetBackend) Rollback(ctx context.Context, n int, label string) (int, error) {
	rev, err := b.s.RollbackRevision(ctx, b.slug, n, label, ViaConsole)
	return rev, toSheetError(err)
}

const sheetRollbackPrefix = "sheet-rollback:"

// sheetRollbackOp names a rollback to revision n. The sheet stores it with
// the rollback's outbox entry, so core knows the target at import, also
// after a crash between the two.
func sheetRollbackOp(n int) string {
	return sheetRollbackPrefix + strconv.Itoa(n) + ":" + newOpID()
}

func sheetRollbackTarget(op string) int {
	rest, ok := strings.CutPrefix(op, sheetRollbackPrefix)
	if !ok {
		return 0
	}
	number, _, _ := strings.Cut(rest, ":")
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// rollbackSheet restores revision n of a sheet as a new revision. Only the
// sheet's content bounds its calculation, so the flat lock is let go for it
// (technical design, section 10.5). Meanwhile the handle keeps the instance
// loaded, sheetRollbacks keeps the hold from settlement and the lease keeps
// the target's blobs. A delete or a rename stops the instance, which ends
// the calculation, so the import after it runs only for the same instance.
func (s *Service) rollbackSheet(ctx context.Context, slugName string, n int, label string, via Via) (RevisionRollback, error) {
	unlock, err := s.lockSheetCtx(ctx, slugName)
	if err != nil {
		return RevisionRollback{}, err
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	h, err := s.runningHandle(ctx, slugName)
	if err != nil {
		return RevisionRollback{}, err
	}
	in, done, err := h.acquire()
	if err != nil {
		return RevisionRollback{}, sheetErr(err)
	}
	defer done()
	// A sheet whose load has not opened and checked sheet.db has no store
	// to import from first, and the rollback would be refused as
	// calculating all the same. A context that has ended asks without
	// waiting; a load that failed refuses with its failure.
	now, stop := context.WithCancel(ctx)
	stop()
	if err := in.AwaitChecked(now); err == now.Err() {
		return RevisionRollback{}, sheetErr(&sheet.WriteError{Code: "unavailable", Reason: "calculating", Retry: true})
	} else if err != nil {
		return RevisionRollback{}, sheetErr(err)
	}
	// n is held before the import, whose retention could delete it.
	op := sheetRollbackOp(n)
	keep, err := s.holdSheetRevisionLocked(ctx, slugName, in, op, n)
	if err != nil {
		return RevisionRollback{}, err
	}
	handed := false
	defer func() {
		if !handed {
			keep()
		}
	}()
	files, _, release, err := s.sheetRevisionFilesLocked(ctx, slugName, n)
	if err != nil {
		return RevisionRollback{}, err
	}
	defer release()

	handed = true
	unlock()
	locked = false
	rerr := in.Rollback(ctx, files, label, op)
	// The rollback committed its outbox entry or nothing, so from here on
	// a settlement keeps its hold exactly while that entry awaits import.
	s.sheetRollbacks.Delete(op)
	if s.sheetRollbackHook != nil {
		s.sheetRollbackHook()
	}
	// The work that holds the flat lock is short, so the import that
	// finishes the call waits for it also after the request has gone.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sheetTickTimeout)
	unlock, err = s.lockStorageCtx(lctx, slugName)
	cancel()
	if err == nil {
		locked = true
	}
	current := err == nil && s.sheetHandle(slugName) == h && h.loaded() == in
	if rerr != nil {
		if current {
			s.settleSheetHoldsLocked(context.WithoutCancel(ctx), slugName, in)
		}
		return RevisionRollback{}, sheetErr(rerr)
	}
	pending := "the rollback applied, but its revision is not recorded yet; do not repeat the rollback"
	if !current {
		// The flat was renamed or deleted, or the host stops. The outbox
		// entry is imported, if at all, by the instance that opens next.
		return RevisionRollback{Warning: pending}, nil
	}
	res, err := s.importSheetLocked(context.WithoutCancel(ctx), slugName, in)
	if err != nil {
		s.Event(ctx, slugName, "warn", "revision", "could not record the sheet revision yet: "+err.Error(), nil)
	}
	revision, warning := res.byOp[op], ""
	if revision == 0 && err == nil {
		// Another import, a tick or a request, may have recorded it while
		// the lock was free.
		revision, _, err = s.st.SheetImportByOp(context.WithoutCancel(ctx), slugName, in.StoreID(), op)
	}
	switch {
	case err != nil:
		warning = pending
	case revision == 0:
		warning = "the rollback applied; its revision was already deleted by retention"
	}
	s.Event(ctx, slugName, "info", "revision", fmt.Sprintf("restored revision %d via %s", n, via), map[string]int{"rollback_of": n, "revision": revision})
	return RevisionRollback{Revision: revision, Warning: warning}, nil
}
