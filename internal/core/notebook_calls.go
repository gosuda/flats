package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
)

// NotebookWriteResult distinguishes a committed edit from pending host import.
type NotebookWriteResult struct {
	Result   json.RawMessage `json:"result"`
	Revision int             `json:"revision"`
	Pending  bool            `json:"pending"`
	Created  bool            `json:"created,omitempty"`
}

// UpdateNotebook applies one atomic notebook call, adopting a new or untyped slug only on success.
func (s *Service) UpdateNotebook(ctx context.Context, slugName string, update []byte, via Via) (NotebookWriteResult, error) {
	if err := slug.Validate(slugName); err != nil {
		return NotebookWriteResult{}, invalid(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(update, &fields); err != nil || fields == nil {
		return NotebookWriteResult{}, invalidf("update must be a JSON object")
	}
	if _, ok := fields["slug"]; ok {
		return NotebookWriteResult{}, invalidf("update must not contain slug")
	}
	fields["slug"], _ = json.Marshal(slugName)
	raw, err := json.Marshal(fields)
	if err != nil {
		return NotebookWriteResult{}, err
	}
	u, err := notebook.DecodeUpdate(raw)
	if err != nil {
		return NotebookWriteResult{}, invalid(notebookErr(err))
	}
	unlock, err := s.lockStorageCtx(ctx, slugName)
	if err != nil {
		return NotebookWriteResult{}, err
	}
	defer unlock()
	var result notebook.WriteResult
	in, created, committed, err := s.writeNotebookLocked(ctx, slugName, via, func(in *notebook.Instance, other int64) error {
		var err error
		result, err = in.Update(ctx, u, other)
		return err
	})
	if err != nil && !committed {
		return NotebookWriteResult{}, notebookErr(err)
	}
	out := NotebookWriteResult{Created: created}
	// A committed write must not become an apparent failure if initialization or import fails.
	if err != nil {
		out.Pending = true
		s.Event(context.WithoutCancel(ctx), slugName, "warn", "notebook", "edit committed; notebook initialization is pending: "+err.Error(), nil)
	} else if seq, err := notebookCommandSeq(context.WithoutCancel(ctx), in, u.OpID); err != nil {
		out.Pending = true
	} else {
		s.notebookRevisionLocked(context.WithoutCancel(ctx), slugName, in, seq, &out)
	}
	result.Created = created
	result.Revision = nil
	result.RevisionPending = out.Pending
	if out.Revision > 0 {
		result.Revision = &out.Revision
	}
	warnings := result.Warnings[:0]
	for _, w := range result.Warnings {
		if w.Code != "revision_pending" {
			warnings = append(warnings, w)
		} else {
			result.Samples.Warnings.Total--
		}
	}
	result.Warnings = warnings
	if out.Pending {
		result.Warnings = append(result.Warnings, notebook.WriteWarning{Code: "revision_pending", Message: "edit committed; revision import is pending; do not repeat the change"})
		result.Samples.Warnings.Total++
	} else if out.Revision == 0 {
		result.Warnings = append(result.Warnings, notebook.WriteWarning{Code: "revision_pruned", Message: "edit committed; its revision was already deleted by retention"})
		result.Samples.Warnings.Total++
	}
	out.Result, err = json.Marshal(result)
	return out, err
}

func notebookCommandSeq(ctx context.Context, in *notebook.Instance, op string) (int64, error) {
	var seq int64
	err := in.ReadTx(ctx, func(tx *sql.Tx, _ notebook.Sequence) error {
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT outbox_seq FROM commands WHERE op_id=?`, op).Scan(&raw); err != nil {
			return err
		}
		var err error
		seq, err = strconv.ParseInt(raw, 10, 64)
		return err
	})
	return seq, err
}

func (s *Service) notebookRevisionLocked(ctx context.Context, slugName string, in *notebook.Instance, seq int64, out *NotebookWriteResult) {
	_, err := s.importNotebookLocked(ctx, slugName, in)
	if err != nil {
		s.Event(ctx, slugName, "warn", "revision", "could not record the notebook revision yet: "+err.Error(), nil)
	}
	n, ok, lerr := s.st.NotebookImportRevision(ctx, slugName, in.StoreID(), seq)
	if lerr == nil && ok {
		out.Revision = n
		return
	}
	st, serr := s.st.ContentState(ctx, slugName)
	out.Pending = !(lerr == nil && serr == nil && st.ImportStore == in.StoreID() && st.ImportSeq >= seq)
}

func (s *Service) runningNotebook(ctx context.Context, slugName string) (*notebook.Instance, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return nil, err
	}
	typ, err := s.flatType(ctx, f)
	if err != nil {
		return nil, err
	}
	if typ != contenttype.Notebook {
		return nil, fmt.Errorf("%w: %s is a %s", ErrNotNotebook, slugName, typeNoun(typ))
	}
	in := s.liveNotebook(slugName)
	if in == nil {
		return nil, notebookErr(fmt.Errorf("%w: notebook %s is not running; get_logs has details", ErrUnavailable, slugName))
	}
	return in, nil
}

// GetNotebook reads current content or a leased retained revision.
func (s *Service) GetNotebook(ctx context.Context, slugName string, rev int, query []byte) (json.RawMessage, error) {
	if err := slug.Validate(slugName); err != nil {
		return nil, invalid(err)
	}
	if rev < 0 {
		return nil, invalidf("revision must be positive")
	}
	in, err := s.runningNotebook(ctx, slugName)
	if err != nil {
		return nil, err
	}
	var continuation struct {
		Cursor string `json:"cursor"`
	}
	hasCursor := json.Unmarshal(query, &continuation) == nil && continuation.Cursor != ""
	if rev == 0 {
		if !hasCursor {
			if err := s.catchUpNotebook(ctx, slugName); err != nil {
				return nil, err
			}
		}
		data, err := in.Read(ctx, query, false)
		return data, notebookErr(err)
	}
	if hasCursor {
		data, err := in.ReadRevision(ctx, notebook.RevisionSource{Number: rev}, query)
		return data, notebookErr(err)
	}
	backend := notebookBackend{s: s, slug: slugName}
	src, release, err := backend.RevisionSource(ctx, rev)
	if err != nil {
		return nil, notebookErr(err)
	}
	defer release()
	data, err := in.ReadRevision(ctx, src, query)
	return data, notebookErr(err)
}

func (s *Service) notebookRevisionPages(ctx context.Context, slugName string, revs []store.Revision) {
	if len(revs) == 0 {
		return
	}
	lo, hi := revs[0].Number, revs[0].Number
	for _, r := range revs {
		lo = min(lo, r.Number)
		hi = max(hi, r.Number)
	}
	pages, err := s.st.NotebookRevPages(ctx, slugName, lo, hi)
	if err != nil {
		return
	}
	for i := range revs {
		revs[i].NotebookPages = pages[revs[i].Number]
		if revs[i].NotebookPages == nil {
			revs[i].NotebookPages = []store.NotebookPageChange{}
		}
		revs[i].Changes = store.Changes{Added: []string{}, Changed: []string{}, Removed: []string{}}
	}
}
