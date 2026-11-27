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
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
)

var ErrSheetUnversioned = fmt.Errorf("%w: sheets have no versions, drafts, previews or publishing; create one with create_flat {type: sheet}, write it with update_sheet, and use the revision tools (list_revisions, get_revision, rollback_revision, pin_revision) for history", ErrInvalid)
var ErrNotSheet = fmt.Errorf("%w: flat is not a sheet", ErrInvalid)
var ErrSheetCapacity = errors.New("sheet capacity")

// sheetLoadWait bounds the wait for a sheet that core just opened to load
// before its creating write runs.
const sheetLoadWait = 5 * time.Second

// sheetErr preserves the original refusal so transports can inspect its detail.
func sheetErr(err error) error {
	var problem *sheet.WriteError
	if !errors.As(err, &problem) {
		if errors.Is(err, ErrUnavailable) {
			return fmt.Errorf("%w: %w", err, &sheet.WriteError{Code: "unavailable", Reason: "sheet_unavailable", Retry: true, Detail: err.Error()})
		}
		return err
	}
	kind := map[string]error{"invalid": ErrInvalid, "conflict": ErrConflict, "sheet_capacity": ErrSheetCapacity, "unavailable": ErrUnavailable, "not_found": store.ErrNotFound}[problem.Code]
	if kind == nil {
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}

func toSheetError(err error) error {
	var problem *sheet.WriteError
	if err == nil || errors.As(err, &problem) {
		return err
	}
	for _, m := range []struct {
		kind error
		code string
	}{{store.ErrNotFound, "not_found"}, {ErrSheetCapacity, "sheet_capacity"}, {ErrConflict, "conflict"}, {ErrInvalid, "invalid"}, {ErrUnavailable, "unavailable"}} {
		if errors.Is(err, m.kind) {
			return &sheet.WriteError{Code: m.code, Reason: err.Error(), Retry: m.code == "unavailable"}
		}
	}
	return err
}

func (s *Service) sheetDir(slugName string) string {
	return filepath.Join(s.flatDir(slugName), "sheet")
}

func (s *Service) sheetDB(slugName string) string {
	return filepath.Join(s.sheetDir(slugName), "sheet.db")
}

func (s *Service) sheetOther(slugName string) (int64, error) {
	return s.sheetOtherCtx(context.Background(), slugName)
}

// sheetOtherCtx measures like walkSize and ends between two files when ctx
// does. The sheet's background measurement gives it a time limit, so a
// walk of a slow directory does not hold up the measurements after it.
func (s *Service) sheetOtherCtx(ctx context.Context, slugName string) (int64, error) {
	db := s.sheetDB(slugName)
	skip := [3]string{db, db + "-wal", db + "-shm"}
	var n int64
	var first error
	stopped := filepath.Walk(s.flatDir(slugName), func(p string, info os.FileInfo, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if first == nil && !errors.Is(err, os.ErrNotExist) {
				first = err
			}
			return nil
		}
		if info.Mode().IsRegular() && p != skip[0] && p != skip[1] && p != skip[2] {
			n += info.Size()
		}
		return nil
	})
	if stopped != nil {
		return 0, stopped
	}
	return n, first
}

// sheetLockPatience is how long a call to a sheet waits for its flat lock
// before it asks whether the sheet calculates.
const sheetLockPatience = 250 * time.Millisecond

// lockSheetCtx takes the flat lock of a sheet for a call. While the sheet
// calculates without an edit, a call that holds the lock waits for the
// writer up to the sheet's patience and is refused. A call that waits for
// the lock meanwhile is refused as calculating at once, as the sheet would
// refuse it.
func (s *Service) lockSheetCtx(ctx context.Context, slugName string) (func(), error) {
	for {
		wait, cancel := context.WithTimeout(ctx, sheetLockPatience)
		unlock, err := s.lockStorageCtx(wait, slugName)
		cancel()
		if err == nil || ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
			return unlock, err
		}
		if in := s.liveSheet(slugName); in != nil && in.Stats().Calculating {
			return nil, sheetErr(&sheet.WriteError{Code: "unavailable", Reason: "calculating", Retry: true})
		}
	}
}

// sheetHandle is the running sheet of slugName, loaded or not, or nil.
func (s *Service) sheetHandle(slugName string) *sheetInstance {
	s.mu.Lock()
	lf := s.live[slugName]
	s.mu.Unlock()
	if lf == nil {
		return nil
	}
	if d := lf.cur.Load(); d != nil {
		if h, ok := d.inst.(*sheetInstance); ok {
			return h
		}
	}
	return nil
}

// liveSheet is the loaded instance of a running sheet, or nil. Background
// work uses it, so that it never loads a sheet nobody uses.
func (s *Service) liveSheet(slugName string) *sheet.Instance {
	if h := s.sheetHandle(slugName); h != nil {
		return h.loaded()
	}
	return nil
}

// openLiveSheet is the instance of a running sheet, loaded again when it
// was unloaded, or nil when the sheet is not running. The caller holds the
// flat lock, which keeps the instance loaded.
func (s *Service) openLiveSheet(slugName string) (*sheet.Instance, error) {
	h := s.sheetHandle(slugName)
	if h == nil {
		return nil, nil
	}
	return h.open()
}

// openSheet opens the instance of a sheet. closeSheet stops it.
func (s *Service) openSheet(slugName string, cursor store.ContentState) (*sheet.Instance, error) {
	// A host whose scratch storage could not be made tries again here,
	// where no workbook is locked.
	s.sheetScratchDir()
	in, _, err := sheet.Open(s.sheetDir(slugName), sheetBackend{s: s, slug: slugName}, sheet.Options{Slug: slugName, Now: func() time.Time { return s.now() }, ImportStore: cursor.ImportStore, ImportSeq: cursor.ImportSeq, OnRecalc: s.sheetStats.observeRecalc, ScratchRoot: s.sheetScratchNow})
	if err != nil {
		return nil, err
	}
	s.sheetStats.track(in)
	return in, nil
}

// closeSheet stops an instance for the reason code and keeps its batch
// counts in the host totals.
func (s *Service) closeSheet(in *sheet.Instance, code string) {
	in.Shutdown(code)
	s.sheetStats.retire(in)
}

func (s *Service) buildSheet(slugName string, v store.Version, dataDir string, env *runtimeEnvironment) (*deployed, error) {
	identity := func(text string) string { return text }
	if dataDir != s.dataDirOf(slugName) {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
		return &deployed{version: v, handler: h, redact: identity, environment: env}, nil
	}
	cursor, err := s.importCursor(slugName)
	if err != nil {
		return nil, err
	}
	in, err := s.openSheet(slugName, cursor)
	if err != nil {
		return nil, fmt.Errorf("open the sheet: %w", err)
	}
	s.sheetPending.Store(slugName, true)
	h := &sheetInstance{s: s, slug: slugName, in: in}
	return &deployed{version: v, handler: h, inst: h, redact: identity, environment: env}, nil
}

// CreateSheet creates a sheet with one empty tab. It runs at once.
func (s *Service) CreateSheet(ctx context.Context, slugName, name string, via Via) (FlatView, error) {
	if err := slug.Validate(slugName); err != nil {
		return FlatView{}, invalid(err)
	}
	unlock, err := s.lockCtx(ctx, slugName)
	if err != nil {
		return FlatView{}, err
	}
	defer unlock()
	title := strings.Join(strings.Fields(name), " ")
	if title == "" {
		title = slugName
	}
	if _, err := s.st.GetFlat(ctx, slugName); err == nil {
		return FlatView{}, fmt.Errorf("%w: flat %q already exists", ErrConflict, slugName)
	}
	if _, err := s.createFlatLocked(ctx, slugName, title, via); err != nil {
		return FlatView{}, err
	}
	if err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.InitState(ctx, contenttype.Sheet) }); err != nil {
		_ = s.st.DeleteFlat(ctx, slugName, store.Audit{})
		return FlatView{}, err
	}
	if _, err := s.finishSheetLocked(ctx, slugName, title, via); err != nil {
		return FlatView{}, err
	}
	return s.GetFlat(ctx, slugName)
}

// finishSheetLocked publishes the internal shell version of a sheet whose
// content state exists, imports its first revisions and marks it ready.
func (s *Service) finishSheetLocked(ctx context.Context, slugName, title string, via Via) (*sheet.Instance, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return nil, err
	}
	if f.LiveVersion == 0 {
		manifest, _ := json.Marshal(map[string]string{"type": contenttype.Sheet, "name": title})
		v, err := s.saveDraftLocked(ctx, slugName, []bundle.File{{Path: bundle.ManifestName, Data: manifest}}, SaveMeta{Message: "sheet shell", quiet: true}, via)
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
	in, err := s.openLiveSheet(slugName)
	if err != nil {
		return nil, err
	}
	if in == nil {
		return nil, s.sheetNotRunning(slugName)
	}
	res, err := s.importSheetLocked(ctx, slugName, in)
	if err != nil {
		return in, err
	}
	if err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.SetPhase(ctx, "ready") }); err != nil {
		return in, err
	}
	if len(res.revisions) > 0 {
		s.Event(ctx, slugName, "info", "sheet", fmt.Sprintf("created sheet (revision %d) via %s", res.revisions[0], via), map[string]int{"revision": res.revisions[0]})
	}
	return in, nil
}

func (s *Service) startPendingSheet(ctx context.Context, slugName string) error {
	unlock, err := s.lockStorage(slugName)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return err
	}
	_, err = s.finishSheetLocked(ctx, slugName, f.Name, ViaConsole)
	return err
}

// awaitSheetLoad waits until a sheet core just opened has loaded, so that
// the write that creates it is not refused as calculating.
func awaitSheetLoad(ctx context.Context, in *sheet.Instance) {
	deadline := time.NewTimer(sheetLoadWait)
	defer deadline.Stop()
	poll := time.NewTicker(2 * time.Millisecond)
	defer poll.Stop()
	for !in.Stats().Loaded {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-poll.C:
		}
	}
}

// writeSheetLocked runs one write against the sheet of slugName. A write to
// a new or untyped slug adopts it as a sheet only when the write commits;
// a refused first write leaves no flat, shell or database.
func (s *Service) writeSheetLocked(ctx context.Context, slugName string, via Via, write func(in *sheet.Instance, other int64, create bool) error) (in *sheet.Instance, created, committed bool, err error) {
	run := func(in *sheet.Instance, create bool) error {
		other, err := s.sheetOther(slugName)
		if err != nil {
			return fmt.Errorf("%w: the sheet's disk usage cannot be measured: %v", ErrUnavailable, err)
		}
		return sheetErr(write(in, other, create))
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
	case t != "" && t != contenttype.Sheet:
		return nil, false, false, fmt.Errorf("%w: %s is a %s", ErrNotSheet, slugName, typeNoun(t))
	case t == contenttype.Sheet:
		in, err := s.openLiveSheet(slugName)
		if err != nil {
			return nil, false, false, err
		}
		if in != nil {
			err = run(in, false)
			return in, false, err == nil, err
		}
		st, err := s.st.ContentState(ctx, slugName)
		if err != nil || st.Phase != "init" {
			return nil, false, false, s.sheetNotRunning(slugName)
		}
		// The creating write may have committed before the shell was
		// published: recover its durable outcome before finishing.
		pending, err := s.openSheet(slugName, st)
		if err != nil {
			return nil, false, false, fmt.Errorf("%w: open the sheet: %w", ErrUnavailable, err)
		}
		awaitSheetLoad(ctx, pending)
		err = run(pending, true)
		s.closeSheet(pending, "")
		if err != nil {
			return nil, false, false, err
		}
		in, err = s.finishSheetLocked(ctx, slugName, f.Name, via)
		return in, false, true, err
	}
	blobs := s.docsStoreDir(slugName)
	hadBlobs, err := pathExists(blobs)
	if err != nil {
		return nil, false, false, err
	}
	undo := func() {
		_ = os.RemoveAll(s.sheetDir(slugName))
		if !hadBlobs {
			_ = os.RemoveAll(blobs)
		}
		if fresh {
			if _, err := s.deleteLocked(context.WithoutCancel(ctx), slugName, via, ""); err != nil {
				s.logf("remove sheet %s after a refused first write: %v", slugName, err)
			}
		}
	}
	first, err := s.openSheet(slugName, store.ContentState{})
	if err != nil {
		undo()
		return nil, false, false, fmt.Errorf("open the sheet: %w", err)
	}
	awaitSheetLoad(ctx, first)
	err = run(first, true)
	s.closeSheet(first, "")
	if err != nil {
		undo()
		return nil, false, false, err
	}
	if err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.InitState(ctx, contenttype.Sheet) }); err != nil {
		undo()
		return nil, false, false, err
	}
	// Adoption is durable; a failure to finish must not undo the committed edit.
	in, err = s.finishSheetLocked(ctx, slugName, f.Name, via)
	return in, true, true, err
}
