package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
)

// SheetWriteResult distinguishes a committed write from pending host import.
type SheetWriteResult struct {
	Result   json.RawMessage `json:"result"`
	Revision int             `json:"revision"`
	Pending  bool            `json:"pending"`
	Created  bool            `json:"created,omitempty"`
	// Flat is the sheet the call created, with the address that opens it
	// (spec rule 53). A write to an existing sheet leaves it nil.
	Flat *FlatView `json:"flat,omitempty"`
}

// UpdateSheet applies one atomic sheet call, adopting a new or untyped slug
// only on success.
func (s *Service) UpdateSheet(ctx context.Context, slugName string, update []byte, via Via) (SheetWriteResult, error) {
	if err := slug.Validate(slugName); err != nil {
		return SheetWriteResult{}, invalid(err)
	}
	// The sheet reads the bytes of the call, once. Nothing here reads them
	// first: another reader could take a field twice or in another case
	// for something else than the sheet does.
	u, err := sheet.DecodeUpdate(update)
	if err != nil {
		return SheetWriteResult{}, sheetErr(err)
	}
	if u.HasSlug() {
		return SheetWriteResult{}, invalidf("update must not contain slug")
	}
	return s.UpdateSheetCall(ctx, slugName, u, via)
}

// UpdateSheetCall applies a call that sheet.DecodeUpdate read, to the
// sheet slugName names.
func (s *Service) UpdateSheetCall(ctx context.Context, slugName string, u sheet.Update, via Via) (SheetWriteResult, error) {
	if err := slug.Validate(slugName); err != nil {
		return SheetWriteResult{}, invalid(err)
	}
	u.Slug = slugName
	unlock, err := s.lockSheetCtx(ctx, slugName)
	if err != nil {
		return SheetWriteResult{}, err
	}
	defer unlock()
	var result sheet.WriteResult
	in, created, committed, err := s.writeSheetLocked(ctx, slugName, via, func(in *sheet.Instance, other int64, create bool) error {
		call := u
		call.Create = create
		var err error
		result, err = in.Update(ctx, call, other)
		return err
	})
	if err != nil && !committed {
		return SheetWriteResult{}, err
	}
	out := SheetWriteResult{Created: created}
	// A committed write must not become an apparent failure if finishing
	// the creation or the import fails.
	if err != nil {
		out.Pending = true
		s.Event(context.WithoutCancel(ctx), slugName, "warn", "sheet", "write committed; sheet initialization is pending: "+err.Error(), nil)
	} else {
		s.sheetRevisionLocked(context.WithoutCancel(ctx), slugName, in, result.OutboxSeq, &out)
	}
	result.Created = created
	result.Revision = nil
	result.RevisionPending = out.Pending
	if out.Revision > 0 {
		result.Revision = &out.Revision
	}
	if created {
		if view, err := s.GetFlat(context.WithoutCancel(ctx), slugName); err == nil {
			out.Flat = &view
		}
	}
	out.Result, err = json.Marshal(result)
	return out, err
}

// sheetRevisionLocked imports the outbox and names the revision of entry seq.
func (s *Service) sheetRevisionLocked(ctx context.Context, slugName string, in *sheet.Instance, seq int64, out *SheetWriteResult) {
	_, err := s.importSheetLocked(ctx, slugName, in)
	if err != nil {
		s.Event(ctx, slugName, "warn", "revision", "could not record the sheet revision yet: "+err.Error(), nil)
	}
	n, ok, lerr := s.st.SheetImportRevision(ctx, slugName, in.StoreID(), seq)
	if lerr == nil && ok {
		out.Revision = n
		return
	}
	st, serr := s.st.ContentState(ctx, slugName)
	out.Pending = !(lerr == nil && serr == nil && st.ImportStore == in.StoreID() && st.ImportSeq >= seq)
}

// runningHandle is the running sheet of slugName. It refuses a flat of
// another type and a sheet that is not running.
func (s *Service) runningHandle(ctx context.Context, slugName string) (*sheetInstance, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return nil, err
	}
	typ, err := s.flatType(ctx, f)
	if err != nil {
		return nil, err
	}
	if typ != contenttype.Sheet {
		return nil, fmt.Errorf("%w: %s is a %s", ErrNotSheet, slugName, typeNoun(typ))
	}
	h := s.sheetHandle(slugName)
	if h == nil {
		return nil, sheetErr(s.sheetNotRunning(slugName))
	}
	return h, nil
}

// sheetNotRunning is the error of a call to a sheet that is not running.
// A sheet.db this release cannot open keeps the sheet from starting, and
// the error then names the cause.
func (s *Service) sheetNotRunning(slugName string) error {
	if err := sheet.CheckDB(s.sheetDir(slugName)); err != nil {
		return fmt.Errorf("%w: sheet %s is not running: %w", ErrUnavailable, slugName, err)
	}
	return fmt.Errorf("%w: sheet %s is not running; get_logs has details", ErrUnavailable, slugName)
}

// GetSheet reads the current sheet, or a leased retained revision
// calculated with the date it closed with.
func (s *Service) GetSheet(ctx context.Context, slugName string, rev int, query []byte) (json.RawMessage, error) {
	if err := slug.Validate(slugName); err != nil {
		return nil, invalid(err)
	}
	if rev < 0 {
		return nil, invalidf("revision must be positive")
	}
	var q sheet.ReadQuery
	if len(query) > 0 {
		if err := json.Unmarshal(query, &q); err != nil {
			return nil, invalidf("the sheet read selectors are not valid: %v", err)
		}
	}
	data, stopped, err := s.readSheet(ctx, slugName, rev, q)
	if stopped {
		// The instance stopped under the read. The sheet's handle opens a
		// new one. A second stop is reported, so the retry is bounded.
		data, _, err = s.readSheet(ctx, slugName, rev, q)
	}
	return data, err
}

// readSheet runs one read against the instance of the running sheet. It
// reports stopped when that instance refused the read because it stopped,
// after dropping it from its handle.
func (s *Service) readSheet(ctx context.Context, slugName string, rev int, q sheet.ReadQuery) (data json.RawMessage, stopped bool, err error) {
	h, err := s.runningHandle(ctx, slugName)
	if err != nil {
		return nil, false, err
	}
	in, release, err := h.acquire()
	if err != nil {
		return nil, false, sheetErr(err)
	}
	defer release()
	switch {
	case rev == 0:
		data, err = in.Read(ctx, q)
	case q.Cursor != "":
		data, err = in.ReadRevision(ctx, rev, nil, "", q)
	default:
		files, date, done, ferr := sheetBackend{s: s, slug: slugName}.RevisionFiles(ctx, rev)
		if ferr != nil {
			return nil, false, sheetErr(ferr)
		}
		defer done()
		data, err = in.ReadRevision(ctx, rev, files, date, q)
	}
	var problem *sheet.WriteError
	if errors.As(err, &problem) && problem.Code == "unavailable" && problem.Reason == "stopped" {
		h.discard(in)
		return nil, true, sheetErr(err)
	}
	return data, false, sheetErr(err)
}

func (s *Service) sheetRevisionTabs(ctx context.Context, slugName string, revs []store.Revision) {
	if len(revs) == 0 {
		return
	}
	lo, hi := revs[0].Number, revs[0].Number
	for _, r := range revs {
		lo = min(lo, r.Number)
		hi = max(hi, r.Number)
	}
	tabs, err := s.st.SheetRevTabs(ctx, slugName, lo, hi)
	if err != nil {
		return
	}
	for i := range revs {
		revs[i].SheetTabs = tabs[revs[i].Number]
		if revs[i].SheetTabs == nil {
			revs[i].SheetTabs = []store.SheetTabChange{}
		}
		revs[i].Changes = store.Changes{Added: []string{}, Changed: []string{}, Removed: []string{}}
	}
}
