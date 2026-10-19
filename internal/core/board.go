package core

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/board"
	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
)

// Boards (docs/spec/board.md, docs/spec/board-TECH.md): a board runs as a
// board.Instance inside the host process, behind an internal shell version
// like a design. Its current state lives in flats/<slug>/board/board.db;
// core imports the revision boundaries the instance records in its outbox
// into the revision store, under the flat lock.

// ErrBoardUnversioned refuses version, Draft, preview, publish and rollback
// operations on a board, and board manifests on the bundle paths.
var ErrBoardUnversioned = fmt.Errorf("%w: boards have no versions, drafts, previews or publishing; create one with create_flat {type: board}, write it with update_board, and use the revision tools (list_revisions, get_revision, rollback_revision, pin_revision) for history", ErrInvalid)

// ErrNotBoard refuses a board tool on a flat that is not a board.
var ErrNotBoard = fmt.Errorf("%w: flat is not a board", ErrInvalid)

// ErrBoardCapacity refuses a board write or import that would put the board
// over its disk quota. Nothing changed.
var ErrBoardCapacity = errors.New("board capacity")

// boardImportBatch is how many outbox entries one import step reads.
const boardImportBatch = 100

// boardTickTimeout bounds one background import, so a wedged board cannot
// hold the loop that also closes edit groups and sweeps every other flat.
const boardTickTimeout = 30 * time.Second

// After a failed background import the ticker waits boardRetryMin before it
// tries that board again, doubling up to boardRetryMax while it keeps
// failing. Writes, reads and rollbacks still import at once, and any
// successful import ends the backoff.
const (
	boardRetryMin = 5 * time.Second
	boardRetryMax = 5 * time.Minute
)

// boardBackoff is when the ticker next tries a failing board import.
type boardBackoff struct {
	next  time.Time
	delay time.Duration
}

func (s *Service) boardDir(slugName string) string {
	return filepath.Join(s.flatDir(slugName), "board")
}

func (s *Service) boardDB(slugName string) string {
	return filepath.Join(s.boardDir(slugName), "board.db")
}

// boardOther measures the flat directory without board.db and its log
// files: the "other" term of the board's disk quota (board-TECH 5.4).
func (s *Service) boardOther(slugName string) (int64, error) {
	db := s.boardDB(slugName)
	return walkSize(s.flatDir(slugName), []string{db, db + "-wal", db + "-shm"})
}

// liveBoard returns the running board instance of slug, or nil.
func (s *Service) liveBoard(slugName string) *board.Instance {
	s.mu.Lock()
	lf := s.live[slugName]
	s.mu.Unlock()
	if lf == nil {
		return nil
	}
	if d := lf.cur.Load(); d != nil {
		if in, ok := d.inst.(*board.Instance); ok {
			return in
		}
	}
	return nil
}

// stopInstance stops a live instance. A board first tells its open canvases
// why: code is deleted or renamed.
func stopInstance(inst Instance, code string) {
	if in, ok := inst.(*board.Instance); ok {
		in.Shutdown(code)
		return
	}
	inst.Stop()
}

// buildBoard opens the board instance of a board's shell version. Only the
// live build opens board.db; health checks and trial runs get a plain
// handler, since one instance owns the database.
func (s *Service) buildBoard(slugName string, v store.Version, title, dataDir string, env *runtimeEnvironment) (*deployed, error) {
	identity := func(text string) string { return text }
	if dataDir != s.dataDirOf(slugName) {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
		return &deployed{version: v, handler: h, redact: identity, environment: env}, nil
	}
	in, _, err := board.Open(s.boardDir(slugName), boardBackend{s: s, slug: slugName}, board.Options{Slug: slugName, Title: title, Now: s.now})
	if err != nil {
		return nil, fmt.Errorf("open the board: %w", err)
	}
	// Import what the board recorded while it was not running.
	s.boardPending.Store(slugName, true)
	return &deployed{version: v, handler: in, inst: in, redact: identity, environment: env}, nil
}

// --- errors ---

// boardErr maps a board refusal to the core error of its category.
func boardErr(err error) error {
	var be *board.Error
	if !errors.As(err, &be) {
		return err
	}
	kind := map[string]error{
		board.CategoryInvalid: ErrInvalid, board.CategoryConflict: ErrConflict, board.CategoryCapacity: ErrBoardCapacity,
		board.CategoryUnavailable: ErrUnavailable, board.CategoryNotFound: store.ErrNotFound,
	}[be.Category]
	if kind == nil {
		return err
	}
	return fmt.Errorf("%w: %s", kind, be.Msg)
}

// toBoardError maps a core error to the board refusal its routes answer
// with; other errors are internal.
func toBoardError(err error) error {
	var be *board.Error
	if err == nil || errors.As(err, &be) {
		return err
	}
	for _, m := range []struct {
		kind     error
		category string
	}{{store.ErrNotFound, board.CategoryNotFound}, {ErrBoardCapacity, board.CategoryCapacity}, {ErrConflict, board.CategoryConflict},
		{ErrInvalid, board.CategoryInvalid}, {ErrUnavailable, board.CategoryUnavailable}} {
		if errors.Is(err, m.kind) {
			return &board.Error{Category: m.category, Msg: err.Error()}
		}
	}
	return err
}

// --- creation ---

// CreateBoard creates an empty board flat and starts it at once, Private,
// with revision 1 (created). An existing slug is refused.
func (s *Service) CreateBoard(ctx context.Context, slugName, name string, via Via) (FlatView, error) {
	if err := slug.Validate(slugName); err != nil {
		return FlatView{}, invalid(err)
	}
	unlock, err := s.lockCtx(ctx, slugName)
	if err != nil {
		return FlatView{}, err
	}
	defer unlock()
	if _, err := s.createBoardLocked(ctx, slugName, name, via); err != nil {
		return FlatView{}, err
	}
	return s.GetFlat(ctx, slugName)
}

func (s *Service) createBoardLocked(ctx context.Context, slugName, name string, via Via) (*board.Instance, error) {
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
	return s.initBoardLocked(ctx, slugName, title, via, func() { _ = s.st.DeleteFlat(ctx, slugName) })
}

// initBoardLocked marks an existing flat row as a board and starts it. When
// the flat cannot be marked, undo removes what the caller created for it, so
// the slug stays usable; a failure after marking leaves phase init, which
// startPendingBoard and the next write finish.
func (s *Service) initBoardLocked(ctx context.Context, slugName, title string, via Via, undo func()) (*board.Instance, error) {
	err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.InitState(ctx, contenttype.Board) })
	if err != nil {
		undo()
		return nil, err
	}
	return s.finishBoardLocked(ctx, slugName, title, via)
}

// finishBoardLocked publishes the board's shell, which opens board.db with
// its created entry, imports that entry as revision 1 and marks the board
// ready. It is idempotent, for recovery of an interrupted creation.
func (s *Service) finishBoardLocked(ctx context.Context, slugName, title string, via Via) (*board.Instance, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return nil, err
	}
	if f.LiveVersion == 0 {
		manifest, _ := json.Marshal(map[string]string{"type": contenttype.Board, "name": title})
		v, err := s.saveDraftLocked(ctx, slugName, []bundle.File{{Path: bundle.ManifestName, Data: manifest}}, SaveMeta{Message: "board shell", quiet: true}, via)
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
	in := s.liveBoard(slugName)
	if in == nil {
		return nil, fmt.Errorf("%w: board %s is not running; get_logs has details", ErrUnavailable, slugName)
	}
	res, err := s.importBoardLocked(ctx, slugName, in)
	if err != nil {
		return in, err
	}
	if err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.SetPhase(ctx, "ready") }); err != nil {
		return in, err
	}
	if len(res.revisions) > 0 {
		s.Event(ctx, slugName, "info", "board", fmt.Sprintf("created board (revision %d) via %s", res.revisions[0], via), map[string]int{"revision": res.revisions[0]})
	}
	return in, nil
}

// startPendingBoard finishes a board whose creation was interrupted.
func (s *Service) startPendingBoard(ctx context.Context, slugName string) error {
	unlock, err := s.lockStorage(slugName)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return err
	}
	_, err = s.finishBoardLocked(ctx, slugName, f.Name, ViaConsole)
	return err
}

// writeBoardLocked runs write, an agent call, on slug's board and returns
// the instance it ran on. A missing slug or an untyped flat becomes a board
// only when write succeeds (rule 33): write first runs on a board.db opened
// in place before anything else exists, and a refusal removes that database,
// the blobs write stored and a flat row this call created, leaving an
// adopted flat untyped. The caller holds the flat lock.
func (s *Service) writeBoardLocked(ctx context.Context, slugName string, via Via, write func(in *board.Instance, other int64) error) (in *board.Instance, created bool, err error) {
	run := func(in *board.Instance) error {
		other, err := s.boardOther(slugName)
		if err != nil {
			return fmt.Errorf("%w: the board's disk usage cannot be measured: %v", ErrUnavailable, err)
		}
		return boardErr(write(in, other))
	}
	f, err := s.st.GetFlat(ctx, slugName)
	fresh := errors.Is(err, store.ErrNotFound)
	if fresh {
		if _, err = s.createFlatLocked(ctx, slugName, slugName, via); err == nil {
			f, err = s.st.GetFlat(ctx, slugName)
		}
	}
	if err != nil {
		return nil, false, err
	}
	t, err := s.flatType(ctx, f)
	if err != nil {
		return nil, false, err
	}
	switch {
	case t != "" && t != contenttype.Board:
		return nil, false, fmt.Errorf("%w: %s is a %s", ErrNotBoard, slugName, typeNoun(t))
	case t == contenttype.Board:
		in := s.liveBoard(slugName)
		if in == nil {
			if st, err := s.st.ContentState(ctx, slugName); err != nil || st.Phase != "init" {
				return nil, false, fmt.Errorf("%w: board %s is not running; get_logs has details", ErrUnavailable, slugName)
			}
			if in, err = s.finishBoardLocked(ctx, slugName, f.Name, via); err != nil {
				return nil, false, err
			}
		}
		return in, false, run(in)
	}
	blobs := s.docsStoreDir(slugName)
	hadBlobs, err := pathExists(blobs)
	if err != nil {
		return nil, false, err
	}
	undo := func() {
		_ = os.RemoveAll(s.boardDir(slugName))
		if !hadBlobs {
			_ = os.RemoveAll(blobs)
		}
		if fresh {
			if _, err := s.deleteLocked(context.WithoutCancel(ctx), slugName, via, ""); err != nil {
				s.logf("remove board %s after a refused first write: %v", slugName, err)
			}
		}
	}
	first, _, err := board.Open(s.boardDir(slugName), boardBackend{s: s, slug: slugName}, board.Options{Slug: slugName, Title: f.Name, Now: s.now})
	if err != nil {
		undo()
		return nil, false, fmt.Errorf("open the board: %w", err)
	}
	err = run(first)
	first.Stop()
	if err != nil {
		undo()
		return nil, false, err
	}
	// A flat that cannot be marked as a board is undone like a refused
	// write: its board.db, the blobs the write stored and a flat row this
	// call created would otherwise block the slug (requireUnusedStorage).
	in, err = s.initBoardLocked(ctx, slugName, f.Name, via, undo)
	return in, err == nil, err
}

// --- agent calls ---

// BoardAsset is an image an update_board call adds. Ref, when set, names it
// for image elements of the same call ("asset": "$ref").
type BoardAsset struct {
	Ref     string `json:"ref,omitempty"`
	Content []byte `json:"content"`
}

// BoardUpdate is one update_board call (board-TECH 8.1), as MCP, the HTTP
// API and the CLI send it.
type BoardUpdate struct {
	OpID    string           `json:"op_id"`
	Ops     []engine.AgentOp `json:"ops"`
	Layout  []engine.Layout  `json:"layout,omitempty"`
	Assets  []BoardAsset     `json:"assets,omitempty"`
	IfSeq   *int64           `json:"if_seq,omitempty"`
	Message string           `json:"message,omitempty"`
	Replace bool             `json:"replace,omitempty"`
}

// BoardWriteResult reports an applied update_board or import_board call.
// Revision is nil while its import is pending (Warning says why).
type BoardWriteResult struct {
	Revision *int                  `json:"revision"`
	Seq      int64                 `json:"seq"`
	IDs      any                   `json:"ids"`
	Layout   map[string][2]float64 `json:"layout,omitempty"`
	Results  json.RawMessage       `json:"results,omitempty"`
	Assets   map[string]string     `json:"assets,omitempty"`
	Created  bool                  `json:"created,omitempty"`
	Replayed bool                  `json:"replayed,omitempty"`
	Warning  string                `json:"warning,omitempty"`
}

// UpdateBoard applies an agent write to a board, creating the board when
// the slug is missing or untyped. It applies whole or not at all and is one
// revision (rule 33).
func (s *Service) UpdateBoard(ctx context.Context, slugName string, u BoardUpdate, via Via) (BoardWriteResult, error) {
	if err := slug.Validate(slugName); err != nil {
		return BoardWriteResult{}, invalid(err)
	}
	if len(u.OpID) < 8 || len(u.OpID) > 120 {
		return BoardWriteResult{}, invalidf("op_id must be 8 to 120 characters")
	}
	ops, refs, err := boardAssetRefs(u.Ops, u.Assets)
	if err != nil {
		return BoardWriteResult{}, err
	}
	unlock, err := s.lockStorageCtx(ctx, slugName)
	if err != nil {
		return BoardWriteResult{}, err
	}
	defer unlock()
	images := make([][]byte, len(u.Assets))
	for i, a := range u.Assets {
		images[i] = a.Content
	}
	var res board.UpdateResult
	in, created, err := s.writeBoardLocked(ctx, slugName, via, func(in *board.Instance, other int64) error {
		var err error
		res, err = in.UpdateBoard(ctx, board.UpdateRequest{OpID: u.OpID, Ops: ops, Layout: u.Layout, IfSeq: u.IfSeq,
			Message: u.Message, Replace: u.Replace, Label: agentLabel(via), Assets: images}, other)
		return err
	})
	if err != nil {
		return BoardWriteResult{}, err
	}
	out := BoardWriteResult{Seq: res.Seq, IDs: res.IDs, Layout: res.Layout, Results: res.Results, Assets: refs, Created: created, Replayed: res.Replayed}
	if out.IDs == nil {
		out.IDs = map[string]string{}
	}
	s.boardRevisionLocked(ctx, slugName, in, res.OutboxSeq, &out)
	if !res.Replayed && res.OutboxSeq > 0 {
		s.Event(ctx, slugName, "info", "board", fmt.Sprintf("board written via %s", via), map[string]any{"revision": out.Revision, "seq": out.Seq})
	}
	return out, nil
}

// boardAssetRefs checks the images of an update_board call and replaces
// each "$ref" an image element's asset names with that image's hash,
// filling the element's natural size when it is missing. It returns the
// rewritten operations and the hash of every ref.
func boardAssetRefs(ops []engine.AgentOp, assets []BoardAsset) ([]engine.AgentOp, map[string]string, error) {
	if len(assets) == 0 {
		return ops, nil, nil
	}
	type info struct {
		hash string
		w, h int
	}
	byRef := map[string]info{}
	refs := map[string]string{}
	for i, a := range assets {
		if len(a.Content) == 0 {
			return nil, nil, invalidf("assets[%d] has no content", i)
		}
		in := info{hash: hashBytes(a.Content)}
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(a.Content)); err == nil {
			in.w, in.h = cfg.Width, cfg.Height
		}
		refs[cmp.Or(a.Ref, in.hash)] = in.hash
		if a.Ref == "" {
			continue
		}
		if !strings.HasPrefix(a.Ref, "$") || len(a.Ref) < 2 || len(a.Ref) > 64 {
			return nil, nil, invalidf("assets[%d].ref must start with $ (at most 64 characters)", i)
		}
		if _, dup := byRef[a.Ref]; dup {
			return nil, nil, invalidf("asset ref %s is used twice", a.Ref)
		}
		byRef[a.Ref] = in
	}
	out := slices.Clone(ops)
	for i, op := range out {
		if op.Op != "add" || len(op.El) == 0 {
			continue
		}
		var el map[string]json.RawMessage
		if json.Unmarshal(op.El, &el) != nil {
			continue // the board refuses the malformed element
		}
		var ref string
		if json.Unmarshal(el["asset"], &ref) != nil || !strings.HasPrefix(ref, "$") {
			continue
		}
		a, ok := byRef[ref]
		if !ok {
			return nil, nil, invalidf("operation %d names asset %s, which no entry of assets declares", i, ref)
		}
		el["asset"], _ = json.Marshal(a.hash)
		if _, ok := el["natural"]; !ok && a.w > 0 {
			el["natural"], _ = json.Marshal([2]int{a.w, a.h})
		}
		raw, err := json.Marshal(el)
		if err != nil {
			return nil, nil, err
		}
		out[i].El = raw
	}
	return out, refs, nil
}

// BoardImport is one import_board call: a board data file (board-TECH 9).
type BoardImport struct {
	OpID    string
	Mode    string // add or replace
	Archive io.ReaderAt
	Size    int64
}

// ImportBoard imports a board data file into a board, creating the board
// when the slug is missing or untyped.
func (s *Service) ImportBoard(ctx context.Context, slugName string, req BoardImport, via Via) (BoardWriteResult, error) {
	if err := slug.Validate(slugName); err != nil {
		return BoardWriteResult{}, invalid(err)
	}
	unlock, err := s.lockStorageCtx(ctx, slugName)
	if err != nil {
		return BoardWriteResult{}, err
	}
	defer unlock()
	var res board.ImportResult
	in, created, err := s.writeBoardLocked(ctx, slugName, via, func(in *board.Instance, other int64) error {
		var err error
		res, err = in.ImportBoard(ctx, board.ImportRequest{OpID: req.OpID, Mode: req.Mode, Archive: req.Archive, Size: req.Size, Label: agentLabel(via)}, other)
		return err
	})
	if err != nil {
		return BoardWriteResult{}, err
	}
	ids := res.IDs
	if ids == nil {
		ids = []string{}
	}
	out := BoardWriteResult{Seq: res.Seq, IDs: ids, Created: created, Replayed: res.Replayed}
	s.boardRevisionLocked(ctx, slugName, in, res.OutboxSeq, &out)
	if !res.Replayed && res.OutboxSeq > 0 {
		s.Event(ctx, slugName, "info", "board", fmt.Sprintf("board data file imported (%s) via %s", req.Mode, via), map[string]any{"revision": out.Revision, "seq": out.Seq})
	}
	return out, nil
}

// boardRevisionLocked imports what a write recorded and sets the result's
// revision: that of its outbox entry, or the latest when it changed nothing.
// A failed import leaves the revision nil with a warning; the write itself
// committed, and resending its op_id returns the final result.
func (s *Service) boardRevisionLocked(ctx context.Context, slugName string, in *board.Instance, outboxSeq int64, out *BoardWriteResult) {
	imp, err := s.importBoardLocked(ctx, slugName, in)
	if err != nil {
		s.Event(ctx, slugName, "warn", "revision", "could not record the board revision yet: "+err.Error(), nil)
	}
	if outboxSeq == 0 {
		if revs, lerr := s.st.ListRevisions(ctx, slugName, 0, 1); lerr == nil && len(revs) == 1 {
			out.Revision = &revs[0].Number
		}
		return
	}
	n, ok := imp.bySeq[outboxSeq]
	if ok {
		// Retention in the same import may already have deleted it.
		if _, err := s.st.Revision(ctx, slugName, n); errors.Is(err, store.ErrNotFound) {
			ok = false
		}
	} else {
		n, ok, _ = s.st.BoardImportRevision(ctx, slugName, in.StoreID(), outboxSeq)
	}
	if ok {
		out.Revision = &n
		return
	}
	if st, serr := s.st.ContentState(ctx, slugName); serr == nil && st.ImportStore == in.StoreID() && st.ImportSeq >= outboxSeq {
		out.Warning = "the revision this call created was already deleted by retention"
		return
	}
	out.Warning = "the change is applied, but its revision is not recorded yet; list_revisions shows it shortly"
}

// BoardPage is one page of a get_board read (board-TECH 8.1).
type BoardPage struct {
	Seq      int64             `json:"seq,omitempty"`
	Revision int               `json:"revision"`
	Pending  bool              `json:"pending"`
	Meta     json.RawMessage   `json:"meta,omitempty"`
	Elements []json.RawMessage `json:"elements"`
	Next     string            `json:"next,omitempty"`
}

// BoardQuery checks the filters of a get_board read.
func BoardQuery(frame string, rect []float64, ids []string, format, cursor string, limit int) (board.Query, error) {
	q := board.Query{Frame: frame, IDs: ids, Cursor: cursor, Limit: limit}
	switch format {
	case "", "full":
	case "outline":
		q.Outline = true
	default:
		return q, invalidf("format is full or outline")
	}
	if len(rect) > 0 {
		if len(rect) != 4 || rect[2] < 0 || rect[3] < 0 {
			return q, invalidf("rect is [x, y, w, h] with w and h at least 0")
		}
		q.Rect = &[4]float64{rect[0], rect[1], rect[2], rect[3]}
	}
	if limit < 0 || limit > 2000 {
		return q, invalidf("limit is 1 to 2000, or 0 for the default")
	}
	return q, nil
}

// GetBoard reads a board's current state, or revision rev when rev > 0,
// narrowed and paged by q.
func (s *Service) GetBoard(ctx context.Context, slugName string, rev int, q board.Query) (BoardPage, error) {
	if rev > 0 {
		if _, err := s.boardState(ctx, slugName); err != nil {
			return BoardPage{}, err
		}
		if err := s.catchUpBoard(ctx, slugName); err != nil {
			return BoardPage{}, err
		}
		_, c, err := s.boardRevisionContent(ctx, slugName, rev)
		if err != nil {
			return BoardPage{}, err
		}
		p, err := board.ReadContent(c, q)
		if err != nil {
			return BoardPage{}, boardErr(err)
		}
		return BoardPage{Revision: rev, Meta: p.Meta, Elements: p.Elements, Next: p.Next}, nil
	}
	in, err := s.runningBoard(ctx, slugName)
	if err != nil {
		return BoardPage{}, err
	}
	p, err := in.GetBoard(ctx, q)
	if err != nil {
		return BoardPage{}, boardErr(err)
	}
	out := BoardPage{Seq: p.Seq, Pending: p.Pending, Meta: p.Meta, Elements: p.Elements, Next: p.Next}
	if revs, err := s.st.ListRevisions(ctx, slugName, 0, 1); err == nil && len(revs) == 1 {
		out.Revision = revs[0].Number
	}
	return out, nil
}

// boardState returns the state row of a board, refusing other flats.
func (s *Service) boardState(ctx context.Context, slugName string) (store.ContentState, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return store.ContentState{}, err
	}
	st, err := s.st.ContentState(ctx, slugName)
	if errors.Is(err, store.ErrNotFound) || (err == nil && st.Type != contenttype.Board) {
		t, terr := s.flatType(ctx, f)
		if terr != nil {
			return st, terr
		}
		if t == "" {
			return st, fmt.Errorf("%w: %s has no content yet", ErrNotBoard, slugName)
		}
		return st, fmt.Errorf("%w: %s is a %s", ErrNotBoard, slugName, typeNoun(t))
	}
	return st, err
}

// runningBoard returns the running instance of a board.
func (s *Service) runningBoard(ctx context.Context, slugName string) (*board.Instance, error) {
	if _, err := s.boardState(ctx, slugName); err != nil {
		return nil, err
	}
	if in := s.liveBoard(slugName); in != nil {
		return in, nil
	}
	return nil, fmt.Errorf("%w: board %s is not running; get_logs has details", ErrUnavailable, slugName)
}

// BoardShares reads selections people sent to the agent: share n, or the
// recent ones when n is 0 (rule 26).
func (s *Service) BoardShares(ctx context.Context, slugName string, n int64) ([]board.ShareView, error) {
	in, err := s.runningBoard(ctx, slugName)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		v, err := in.GetShare(ctx, n)
		if err != nil {
			return nil, boardErr(err)
		}
		return []board.ShareView{v}, nil
	}
	out := []board.ShareView{}
	for _, sh := range in.Shares() {
		v, err := in.GetShare(ctx, sh.N)
		if err != nil {
			continue // expired meanwhile
		}
		out = append(out, v)
	}
	return out, nil
}

// --- import of the outbox ---

// boardImport reports one import of a board's outbox.
type boardImport struct {
	revisions []int
	bySeq     map[int64]int // outbox seq -> revision
}

// importBoardLocked imports a board's outbox into the revision store and
// acknowledges it (board-TECH 5.3), then settles the holds of rollbacks
// that never committed. The caller holds the flat lock.
func (s *Service) importBoardLocked(ctx context.Context, slugName string, in *board.Instance) (boardImport, error) {
	res := boardImport{bySeq: map[int64]int{}}
	start := time.Now()
	st, err := s.st.ContentState(ctx, slugName)
	if err != nil {
		return res, err
	}
	if st.Type != contenttype.Board {
		return res, fmt.Errorf("%w (%s)", ErrNotBoard, slugName)
	}
	outbox := in.StoreID()
	cursor := int64(0)
	if st.ImportStore == outbox {
		cursor = st.ImportSeq
	}
	// Entries the cursor passed were imported before a stop between the
	// flats.db commit and the acknowledgement: acknowledge them again.
	if cursor > 0 {
		old, err := in.Outbox(ctx, 0, 1)
		if err != nil {
			return res, boardErr(err)
		}
		if len(old) > 0 && old[0].Seq <= cursor {
			if err := in.Acknowledge(ctx, cursor, 0); err != nil {
				return res, boardErr(err)
			}
		}
	}
	root := s.docsStoreDir(slugName)
	for {
		entries, err := in.Outbox(ctx, cursor, boardImportBatch)
		if err != nil {
			return res, boardErr(err)
		}
		if len(entries) == 0 {
			break
		}
		var stored int64
		for _, e := range entries {
			for _, h := range e.Snapshots {
				if _, err := os.Stat(blobPath(root, h)); err == nil {
					continue
				}
				data, err := in.SnapshotData(ctx, h)
				if err != nil {
					return res, boardErr(err)
				}
				if hashBytes(data) != h {
					return res, fmt.Errorf("%w: a board snapshot does not match its hash %.12s", ErrUnavailable, h)
				}
				if _, err := putBlob(root, data); err != nil {
					return res, err
				}
				stored += int64(len(data))
			}
			for _, f := range e.Files {
				if len(f.Hash) != 64 {
					return res, fmt.Errorf("%w: invalid board revision entry", ErrUnavailable)
				}
				if _, err := os.Stat(blobPath(root, f.Hash)); err != nil {
					return res, fmt.Errorf("%w: board file %s has no stored blob %.12s", ErrUnavailable, f.Path, f.Hash)
				}
			}
		}
		if err := s.commitBoardEntries(ctx, slugName, outbox, entries, &res); err != nil {
			return res, err
		}
		cursor = entries[len(entries)-1].Seq
		if err := in.Acknowledge(ctx, cursor, stored); err != nil {
			// The cursor is durable; the next import acknowledges again.
			return res, boardErr(err)
		}
	}
	if len(res.revisions) > 0 {
		s.boardStats.observeImport(time.Since(start))
		_ = s.st.Touch(ctx, slugName, s.now())
	}
	s.settleBoardHoldsLocked(ctx, slugName, in)
	// Any successful import, a foreground write, read or rollback as much
	// as the ticker's, ends the ticker's backoff after an earlier failure.
	s.boardRetry.Delete(slugName)
	return res, nil
}

// commitBoardEntries inserts imported outbox entries as revisions in one
// transaction, with their element counts and import mappings, moves the
// head to the last entry's files, advances the cursor and runs retention.
// Entries the cursor already passed are skipped.
func (s *Service) commitBoardEntries(ctx context.Context, slugName, outbox string, entries []board.OutboxEntry, res *boardImport) error {
	// The previous revision, read before the transaction: the store has
	// one connection, and the flat lock keeps it current.
	var prev []store.RevisionFile
	if revs, err := s.st.ListRevisions(ctx, slugName, 0, 1); err != nil {
		return err
	} else if len(revs) == 1 {
		if prev, err = s.st.RevisionFiles(ctx, slugName, revs[0].Number); err != nil {
			return err
		}
	}
	root := s.docsStoreDir(slugName)
	type prepared struct {
		files  []store.RevisionFile
		counts store.ElementCounts
		size   int64
		change store.Changes
	}
	preps := make([]prepared, len(entries))
	for i, e := range entries {
		var p prepared
		for _, f := range e.Files {
			p.files = append(p.files, store.RevisionFile{Path: f.Path, Hash: f.Hash, Size: f.Size})
			p.size += f.Size
		}
		sort.Slice(p.files, func(a, b int) bool { return p.files[a].Path < p.files[b].Path })
		counts, err := boardElementCounts(root, prev, p.files)
		if err != nil {
			return err
		}
		p.counts, p.change = counts, diffFiles(prev, p.files)
		preps[i], prev = p, p.files
	}
	var imported []int
	bySeq := map[int64]int{}
	deleted := 0
	err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
		st, err := tx.State(ctx)
		if err != nil {
			return err
		}
		latest, err := tx.LatestRevision(ctx)
		if err != nil {
			return err
		}
		head := -1
		for i, e := range entries {
			if st.ImportStore == outbox && e.Seq <= st.ImportSeq {
				continue
			}
			p := preps[i]
			latest++
			kind := e.Kind
			if kind == "" {
				kind = "person"
			}
			r := store.Revision{
				Number: latest, CreatedAt: time.Unix(e.CreatedAt, 0), Kind: kind, Authors: dedupe(e.Authors),
				Message: truncate(e.Message, 500), RollbackOf: e.RollbackOf, Changes: p.change, Files: len(p.files), Size: p.size,
			}
			if err := tx.InsertRevision(ctx, r, p.files); err != nil {
				return err
			}
			if err := tx.PutBoardRevStats(ctx, latest, p.counts); err != nil {
				return err
			}
			if err := tx.PutBoardImport(ctx, outbox, e.Seq, e.Op, latest); err != nil {
				return err
			}
			if e.Kind == "rollback" && e.Op != "" {
				if err := tx.DeleteHold(ctx, e.Op); err != nil {
					return err
				}
			}
			imported = append(imported, latest)
			bySeq[e.Seq] = latest
			st.ImportStore, st.ImportSeq, head = outbox, e.Seq, i
		}
		if head >= 0 {
			if err := tx.ReplaceHead(ctx, preps[head].files); err != nil {
				return err
			}
		}
		if err := tx.SetImport(ctx, st.ImportStore, st.ImportSeq); err != nil {
			return err
		}
		if len(imported) > 0 {
			deleted, err = s.retainTx(ctx, tx)
		}
		return err
	})
	if err != nil {
		return err
	}
	res.revisions = append(res.revisions, imported...)
	for k, v := range bySeq {
		res.bySeq[k] = v
	}
	if deleted > 0 {
		go s.sweepBlobs(context.WithoutCancel(ctx), slugName, contenttype.Board)
	}
	return nil
}

// boardElementCounts compares the element buckets of two revisions' file
// lists: the elements cur added, changed and removed (board-TECH 5.2).
func boardElementCounts(root string, prev, cur []store.RevisionFile) (store.ElementCounts, error) {
	var c store.ElementCounts
	old := map[string]string{}
	for _, f := range prev {
		if strings.HasPrefix(f.Path, "elements/") {
			old[f.Path] = f.Hash
		}
	}
	read := func(hash string) (map[string]json.RawMessage, error) {
		var m map[string]json.RawMessage
		if hash == "" {
			return m, nil
		}
		data, err := readBlob(root, hash)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("board bucket %.12s: %w", hash, err)
		}
		return m, nil
	}
	compare := func(oldHash, newHash string) error {
		if oldHash == newHash {
			return nil
		}
		a, err := read(oldHash)
		if err != nil {
			return err
		}
		b, err := read(newHash)
		if err != nil {
			return err
		}
		for id, v := range b {
			if w, ok := a[id]; !ok {
				c.Added++
			} else if !bytes.Equal(v, w) {
				c.Changed++
			}
		}
		for id := range a {
			if _, ok := b[id]; !ok {
				c.Removed++
			}
		}
		return nil
	}
	for _, f := range cur {
		if !strings.HasPrefix(f.Path, "elements/") {
			continue
		}
		if err := compare(old[f.Path], f.Hash); err != nil {
			return c, err
		}
		delete(old, f.Path)
	}
	for _, h := range old {
		if err := compare(h, ""); err != nil {
			return c, err
		}
	}
	return c, nil
}

// settleBoardHoldsLocked releases the holds of board rollbacks that never
// committed. A committed rollback keeps its operation in board.db until its
// entry is imported, which deletes the hold, so a missing operation is
// definitive (board-TECH 4.11). The caller holds the flat lock, which every
// rollback holds from its hold to its import.
func (s *Service) settleBoardHoldsLocked(ctx context.Context, slugName string, in *board.Instance) {
	holds, err := s.st.Holds(ctx, slugName)
	if err != nil {
		return
	}
	for _, h := range holds {
		if _, committed, err := in.Op(ctx, h.Op); err != nil || committed {
			continue
		}
		_ = s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.DeleteHold(ctx, h.Op) })
	}
}

// prepareBoardSweepLocked imports a board before its blobs are swept and
// returns the hashes its board.db references: every asset row (referenced,
// kept for deletion undo or protected after upload) and every snapshot an
// unacknowledged entry holds (board-TECH 5.5). It reports false when the
// sweep must wait: a board that is not running, or whose import failed, may
// reference blobs no revision names yet. The caller holds the flat lock, so
// no agent write, import or rollback runs meanwhile; a person's upload that
// reuses a blob refreshes its modification time (boardBackend.PutBlob).
func (s *Service) prepareBoardSweepLocked(ctx context.Context, slugName string) (*board.Instance, []string, bool) {
	in := s.liveBoard(slugName)
	if in == nil {
		return nil, nil, false
	}
	if _, err := s.importBoardLocked(ctx, slugName, in); err != nil {
		return nil, nil, false
	}
	assets, snaps, err := in.References(ctx)
	if err != nil {
		return nil, nil, false
	}
	return in, append(assets, snaps...), true
}

// catchUpBoard imports a board's outbox before a history read or change.
func (s *Service) catchUpBoard(ctx context.Context, slugName string) error {
	unlock, err := s.lockStorageCtx(ctx, slugName)
	if err != nil {
		return err
	}
	defer unlock()
	if in := s.liveBoard(slugName); in != nil {
		if _, err := s.importBoardLocked(ctx, slugName, in); err != nil {
			s.logf("board revisions of %s: %v", slugName, err)
		}
	}
	return nil
}

// catchUpRevisions imports the revisions a document or a board recorded
// before a history read or change. It does nothing for other flats.
func (s *Service) catchUpRevisions(ctx context.Context, slugName string) error {
	st, err := s.st.ContentState(ctx, slugName)
	if err == nil && st.Type == contenttype.Board {
		return s.catchUpBoard(ctx, slugName)
	}
	return s.catchUpDocs(ctx, slugName)
}

// TickBoards imports the outbox of every board that recorded entries,
// skipping boards whose lock is taken until the next tick. A board whose
// import keeps failing is tried less often (boardBackoff), and each import
// has a deadline (boardTickTimeout).
func (s *Service) TickBoards(ctx context.Context) {
	now := time.Now()
	s.boardPending.Range(func(k, _ any) bool {
		slugName := k.(string)
		in := s.liveBoard(slugName)
		if in == nil {
			s.boardPending.Delete(slugName)
			s.boardRetry.Delete(slugName)
			return true
		}
		var backoff boardBackoff
		if v, ok := s.boardRetry.Load(slugName); ok {
			if backoff = v.(boardBackoff); now.Before(backoff.next) {
				return true
			}
		}
		m, _ := s.locks.LoadOrStore(slugName, &sync.Mutex{})
		mu := m.(*sync.Mutex)
		if !mu.TryLock() {
			return true
		}
		defer mu.Unlock()
		if s.checkStorageJournal(slugName) != nil {
			return true
		}
		if in = s.liveBoard(slugName); in == nil {
			return true
		}
		// A board that records an entry during the import marks itself again.
		s.boardPending.Delete(slugName)
		ictx, cancel := context.WithTimeout(ctx, boardTickTimeout)
		_, err := s.importBoardLocked(ictx, slugName, in)
		cancel()
		if err != nil {
			s.boardPending.Store(slugName, true)
			backoff.delay = min(max(2*backoff.delay, boardRetryMin), boardRetryMax)
			backoff.next = time.Now().Add(backoff.delay)
			s.boardRetry.Store(slugName, backoff)
			s.logf("board revisions of %s (next try in %s): %v", slugName, backoff.delay, err)
		}
		return true
	})
}

// remeasureBoards starts a new disk measurement of every running board,
// after a disk quota change (board-TECH 5.4).
func (s *Service) remeasureBoards() {
	s.mu.Lock()
	var ins []*board.Instance
	for _, lf := range s.live {
		if d := lf.cur.Load(); d != nil {
			if in, ok := d.inst.(*board.Instance); ok {
				ins = append(ins, in)
			}
		}
	}
	s.mu.Unlock()
	for _, in := range ins {
		in.Remeasure()
	}
}

// --- history ---

// rollbackBoard restores revision n of a board as a new revision
// (board-TECH 4.11): it holds n, sets the board to n's content in the
// instance's apply loop and imports the rollback entry, which releases the
// hold. A rollback is never refused for disk quota. Once the board committed
// the rollback it succeeds: when its revision is not recorded yet, the result
// has no revision and a warning, as update_board reports a pending import,
// so a caller does not retry and restore twice.
func (s *Service) rollbackBoard(ctx context.Context, slugName string, n int, label string, via Via) (RevisionRollback, error) {
	unlock, err := s.lockStorageCtx(ctx, slugName)
	if err != nil {
		return RevisionRollback{}, err
	}
	defer unlock()
	in, err := s.runningBoard(ctx, slugName)
	if err != nil {
		return RevisionRollback{}, err
	}
	// n may be a revision the board recorded but core has not imported.
	if _, err := s.importBoardLocked(ctx, slugName, in); err != nil {
		s.logf("board revisions of %s: %v", slugName, err)
	}
	op := newOpID()
	err = s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
		if _, err := tx.Revision(ctx, n); err != nil {
			return err
		}
		return tx.AddHold(ctx, op, n)
	})
	if err != nil {
		return RevisionRollback{}, err
	}
	settle := func() { s.settleBoardHoldsLocked(context.WithoutCancel(ctx), slugName, in) }
	files, err := s.st.RevisionFiles(ctx, slugName, n)
	if err != nil {
		settle()
		return RevisionRollback{}, err
	}
	root := s.docsStoreDir(slugName)
	c, err := board.ContentFromFiles(boardFiles(files), func(h string) ([]byte, error) { return readBlob(root, h) })
	if err != nil {
		settle()
		return RevisionRollback{}, err
	}
	res, err := in.Rollback(ctx, op, n, c, label)
	if err != nil {
		settle()
		return RevisionRollback{}, boardErr(err)
	}
	out := BoardWriteResult{}
	s.boardRevisionLocked(context.WithoutCancel(ctx), slugName, in, res.OutboxSeq, &out)
	if out.Revision == nil {
		s.Event(ctx, slugName, "info", "revision", fmt.Sprintf("restored revision %d via %s; %s", n, via, out.Warning), map[string]int{"rollback_of": n})
		return RevisionRollback{Warning: out.Warning}, nil
	}
	s.Event(ctx, slugName, "info", "revision", fmt.Sprintf("restored revision %d as revision %d via %s", n, *out.Revision, via), map[string]int{"revision": *out.Revision, "rollback_of": n})
	return RevisionRollback{Revision: *out.Revision}, nil
}

func boardFiles(files []store.RevisionFile) []board.File {
	out := make([]board.File, len(files))
	for i, f := range files {
		out[i] = board.File{Path: f.Path, Hash: f.Hash, Size: f.Size}
	}
	return out
}

// boardRevisionContent reads revision n of a board. Its files are chosen
// and leased under the flat lock, so the sweep cannot remove them while
// they are read.
func (s *Service) boardRevisionContent(ctx context.Context, slugName string, n int) (store.Revision, engine.Content, error) {
	r, c, release, err := s.heldBoardRevisionContent(ctx, slugName, n)
	release()
	return r, c, err
}

// heldBoardRevisionContent is boardRevisionContent keeping the read lease
// on the revision's files until the caller calls release, so work that
// goes on using them (an element recovery until its commit) stays
// protected from the sweep (board-TECH 5.5). release is never nil.
func (s *Service) heldBoardRevisionContent(ctx context.Context, slugName string, n int) (store.Revision, engine.Content, func(), error) {
	unlock, err := s.lockCtx(ctx, slugName)
	if err != nil {
		return store.Revision{}, engine.Content{}, func() {}, err
	}
	r, err := s.st.Revision(ctx, slugName, n)
	var files []store.RevisionFile
	if err == nil {
		files, err = s.st.RevisionFiles(ctx, slugName, n)
	}
	var hashes []string
	for _, f := range files {
		hashes = append(hashes, f.Hash)
	}
	release := s.lease(slugName, hashes)
	unlock()
	if err != nil {
		release()
		return r, engine.Content{}, func() {}, err
	}
	root := s.docsStoreDir(slugName)
	c, err := board.ContentFromFiles(boardFiles(files), func(h string) ([]byte, error) { return readBlob(root, h) })
	if err != nil {
		release()
		return r, c, func() {}, err
	}
	return r, c, release, nil
}

// boardRevisionStats sets the element counts of board revisions, computing
// and storing those a row is missing for.
func (s *Service) boardRevisionStats(ctx context.Context, slugName string, revs []store.Revision) {
	if len(revs) == 0 {
		return
	}
	lo, hi := revs[0].Number, revs[0].Number
	for _, r := range revs {
		lo, hi = min(lo, r.Number), max(hi, r.Number)
	}
	stats, err := s.st.BoardRevStats(ctx, slugName, lo, hi)
	if err != nil {
		return
	}
	root := s.docsStoreDir(slugName)
	for i := range revs {
		c, ok := stats[revs[i].Number]
		if !ok {
			cur, err := s.st.RevisionFiles(ctx, slugName, revs[i].Number)
			if err != nil {
				continue
			}
			var prev []store.RevisionFile
			if older, err := s.st.ListRevisions(ctx, slugName, revs[i].Number, 1); err == nil && len(older) == 1 {
				prev, _ = s.st.RevisionFiles(ctx, slugName, older[0].Number)
			}
			if c, err = boardElementCounts(root, prev, cur); err != nil {
				continue
			}
			_ = s.st.PutBoardRevStats(ctx, slugName, revs[i].Number, c)
		}
		revs[i].Elements = &c
		revs[i].Changes = store.Changes{Added: []string{}, Changed: []string{}, Removed: []string{}}
	}
}

// --- backend ---

// boardBackend adapts the service to board.Backend for one board.
type boardBackend struct {
	s    *Service
	slug string
}

func (b boardBackend) Limits() board.Limits {
	return board.Limits{DiskQuota: max(b.s.diskQuota(), 0), UploadMax: b.s.UploadLimit()}
}

func (b boardBackend) MeasureOther(context.Context) (int64, error) { return b.s.boardOther(b.slug) }

// PutBlob stores a blob in the board's blob root. A blob that already
// exists gets a fresh modification time, so a sweep that read the board's
// references before this write keeps it for its grace period, until the
// caller's asset row protects it. The lease taken before the file is
// looked up orders this against the sweep's removal (removeUnleasedBlob).
func (b boardBackend) PutBlob(_ context.Context, hash string, data []byte) (bool, error) {
	if hashBytes(data) != hash {
		return false, fmt.Errorf("blob content does not match its hash %.12s", hash)
	}
	release := b.s.lease(b.slug, []string{hash})
	defer release()
	root := b.s.docsStoreDir(b.slug)
	p := blobPath(root, hash)
	if _, err := os.Stat(p); err == nil {
		now := time.Now()
		_ = os.Chtimes(p, now, now)
		return false, nil
	}
	_, err := putBlob(root, data)
	return err == nil, err
}

func (b boardBackend) ReadBlob(_ context.Context, hash string) ([]byte, error) {
	release := b.s.lease(b.slug, []string{hash})
	defer release()
	data, err := readBlob(b.s.docsStoreDir(b.slug), hash)
	return data, toBoardError(err)
}

func (b boardBackend) RevisionAsset(ctx context.Context, hash string) (string, bool, error) {
	return b.s.st.RevisionAssetPath(ctx, b.slug, hash)
}

func (b boardBackend) Revisions(ctx context.Context, before, limit int) (board.RevisionPage, error) {
	page, err := b.s.ListRevisions(ctx, b.slug, before, limit)
	if err != nil {
		return board.RevisionPage{}, toBoardError(err)
	}
	out := board.RevisionPage{Pins: page.Pins, PinQuota: page.PinQuota, RevisionQuota: page.RevisionQuota, More: page.Next > 0}
	out.Count, _ = b.s.st.CountRevisions(ctx, b.slug)
	for _, r := range page.Revisions {
		out.Revisions = append(out.Revisions, boardRevisionInfo(r))
	}
	return out, nil
}

func boardRevisionInfo(r store.Revision) board.RevisionInfo {
	info := board.RevisionInfo{Number: r.Number, Kind: r.Kind, Authors: r.Authors, Message: r.Message, At: r.CreatedAt.Unix(),
		Pinned: r.Pinned, RollbackOf: r.RollbackOf}
	if info.Authors == nil {
		info.Authors = []string{}
	}
	if e := r.Elements; e != nil {
		info.Added, info.Changed, info.Removed = e.Added, e.Changed, e.Removed
	}
	return info
}

func (b boardBackend) RevisionContent(ctx context.Context, n int) (board.RevisionInfo, engine.Content, error) {
	if err := b.s.catchUpBoard(ctx, b.slug); err != nil {
		return board.RevisionInfo{}, engine.Content{}, toBoardError(err)
	}
	r, c, err := b.s.boardRevisionContent(ctx, b.slug, n)
	if err != nil {
		return board.RevisionInfo{}, c, toBoardError(err)
	}
	revs := []store.Revision{r}
	b.s.boardRevisionStats(ctx, b.slug, revs)
	return boardRevisionInfo(revs[0]), c, nil
}

func (b boardBackend) HeldRevisionContent(ctx context.Context, n int) (engine.Content, func(), error) {
	if err := b.s.catchUpBoard(ctx, b.slug); err != nil {
		return engine.Content{}, func() {}, toBoardError(err)
	}
	_, c, release, err := b.s.heldBoardRevisionContent(ctx, b.slug, n)
	return c, release, toBoardError(err)
}

// Lease takes read leases on blobs the board chose in its apply loop. It
// only registers them, so it never waits for the flat lock.
func (b boardBackend) Lease(hashes []string) func() { return b.s.lease(b.slug, hashes) }

func (b boardBackend) Pin(ctx context.Context, n int, pinned bool) error {
	return toBoardError(b.s.PinRevision(ctx, b.slug, n, pinned))
}

func (b boardBackend) Rollback(ctx context.Context, n int, label string) (int, error) {
	// 0 without an error: the rollback applied, its revision is not
	// recorded yet.
	rev, err := b.s.RollbackRevision(ctx, b.slug, n, label, ViaConsole)
	return rev, toBoardError(err)
}

// Exclusive runs fn under the flat lock with a fresh measurement of the flat
// directory, then imports what fn recorded.
func (b boardBackend) Exclusive(ctx context.Context, fn func(ctx context.Context, other int64) error) error {
	unlock, err := b.s.lockStorageCtx(ctx, b.slug)
	if err != nil {
		return toBoardError(err)
	}
	defer unlock()
	other, err := b.s.boardOther(b.slug)
	if err != nil {
		return &board.Error{Category: board.CategoryUnavailable, Msg: "the board's disk usage cannot be measured right now; try again"}
	}
	if err := fn(ctx, other); err != nil {
		return err
	}
	if in := b.s.liveBoard(b.slug); in != nil {
		if _, err := b.s.importBoardLocked(ctx, b.slug, in); err != nil {
			b.s.Event(ctx, b.slug, "warn", "revision", "could not record the board revision yet: "+err.Error(), nil)
		}
	}
	return nil
}

func (b boardBackend) OutboxReady() { b.s.boardPending.Store(b.slug, true) }
