package board

import (
	"cmp"
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
)

// Query narrows a get_board read (8.1).
type Query struct {
	Frame   string
	Rect    *[4]float64 // x, y, w, h
	IDs     []string
	Outline bool
	Cursor  string
	Limit   int // default 500, at most 2,000
}

// Page is one page of a get_board read. Core adds the revision number.
type Page struct {
	Seq      int64             `json:"seq,omitempty"`
	Pending  bool              `json:"pending"`
	Meta     json.RawMessage   `json:"meta,omitempty"`
	Elements []json.RawMessage `json:"elements"`
	Next     string            `json:"next,omitempty"`
}

const (
	pageDefault = 500
	pageMax     = 2000
	pageBytes   = 1 << 20
)

// GetBoard reads the current board (8.1). A read that needs more than one
// page takes one consistent snapshot; its later pages come from that
// snapshot through the next cursor.
func (in *Instance) GetBoard(ctx context.Context, q Query) (Page, error) {
	if q.Cursor != "" {
		// Everything about the snapshot comes from the cache under its
		// lock: concurrent reads of one cursor may advance it meanwhile.
		got, ok := in.snaps.next(q.Cursor, in.now())
		if !ok {
			return Page{}, errorf(CategoryConflict, "cursor expired; read again")
		}
		var p Page
		if err := json.Unmarshal(got.page, &p); err != nil {
			return Page{}, err
		}
		p.Seq, p.Pending = got.seq, got.pending
		if got.more {
			p.Next = q.Cursor
		}
		return p, nil
	}
	var all []json.RawMessage
	var page Page
	err := in.do(ctx, func() error {
		page.Seq = in.st.Seq()
		page.Pending = len(in.sd.outbox) > 0
		for _, g := range in.sd.groups {
			page.Pending = page.Pending || g.Dirty
		}
		page.Meta, _ = json.Marshal(model.PublicElement(in.st.Board().Element))
		els := make([]model.Element, 0)
		vers := map[string]int64{}
		for _, e := range in.st.Alive() {
			els = append(els, e.Element)
			v, _ := in.st.Ver(e.ID)
			vers[e.ID] = v
		}
		var err error
		all, err = selectElements(els, elementsByID(els), q, vers)
		return err
	})
	if err != nil {
		return Page{}, err
	}
	pages := paginate(all, q.Limit)
	page.Elements = pages[0]
	if len(pages) > 1 {
		snap := &pageSnap{seq: page.Seq, pending: page.Pending}
		for _, p := range pages[1:] {
			b, _ := json.Marshal(Page{Elements: p})
			snap.pages = append(snap.pages, b)
			snap.bytes += int64(len(b))
		}
		if page.Next, err = in.snaps.put(snap, in.now()); err != nil {
			return Page{}, err
		}
	}
	return page, nil
}

// ReadContent reads a revision's content with the same filters and pages as
// GetBoard. Revisions never change, so the cursor is the last id read and
// no paging snapshot is taken (8.1): each call encodes only its own page,
// so a revision of any size can be read page by page.
func ReadContent(c engine.Content, q Query) (Page, error) {
	els := slices.Clone(c.Elements)
	slices.SortFunc(els, func(a, b model.Element) int { return strings.Compare(a.ID, b.ID) })
	// Connector bounds resolve against the whole revision, not only the
	// elements after the cursor.
	byID := elementsByID(els)
	if q.Cursor != "" {
		after := strings.TrimPrefix(q.Cursor, "after:")
		i, _ := slices.BinarySearchFunc(els, after, func(e model.Element, id string) int { return strings.Compare(e.ID, id) })
		for i < len(els) && els[i].ID <= after {
			i++
		}
		els = els[i:]
	}
	meta, _ := json.Marshal(model.PublicElement(model.Element{ID: model.BoardID, Type: model.TypeBoard, Text: c.Title, Grid: c.Grid}))
	p := Page{Meta: meta, Elements: []json.RawMessage{}}
	limit := pageLimit(q.Limit)
	f := newElementFilter(q)
	size, last := 0, ""
	for _, e := range els {
		if !f.match(e, byID) {
			continue
		}
		b := encodeElement(e, q, nil)
		if pageFull(len(p.Elements), size, len(b), limit) {
			p.Next = "after:" + last
			break
		}
		p.Elements = append(p.Elements, b)
		size += len(b) + 1
		last = e.ID
	}
	return p, nil
}

func elementsByID(els []model.Element) map[string]model.Element {
	byID := make(map[string]model.Element, len(els))
	for _, e := range els {
		byID[e.ID] = e
	}
	return byID
}

// elementFilter is the frame, area and ids narrowing of a read.
type elementFilter struct {
	q   Query
	ids map[string]bool
}

func newElementFilter(q Query) elementFilter {
	f := elementFilter{q: q}
	if len(q.IDs) > 0 {
		f.ids = map[string]bool{}
		for _, id := range q.IDs {
			f.ids[id] = true
		}
	}
	return f
}

// match reports whether e passes the filter; byID resolves connector ends.
func (f elementFilter) match(e model.Element, byID map[string]model.Element) bool {
	if f.ids != nil && !f.ids[e.ID] {
		return false
	}
	if f.q.Frame != "" && e.Frame != f.q.Frame && e.ID != f.q.Frame {
		return false
	}
	return f.q.Rect == nil || intersects(bounds(e, byID), *f.q.Rect)
}

// encodeElement encodes e, full or as an outline. vers adds its guard
// version (ver) when set.
func encodeElement(e model.Element, q Query, vers map[string]int64) []byte {
	var m map[string]any
	if q.Outline {
		m = map[string]any{"id": e.ID, "type": e.Type}
		if e.Text != "" {
			m["text"] = e.Text
		}
		if e.Frame != "" {
			m["frame"] = e.Frame
		}
		if e.Type == model.TypeImage && e.Alt != "" {
			m["alt"] = e.Alt
		}
		if e.Type == model.TypeConnector {
			m["conn"] = endName(e.Ends[0]) + " → " + endName(e.Ends[1])
		}
	} else {
		m = e.Content()
		if v, ok := vers[e.ID]; ok {
			m["ver"] = v
		}
	}
	return model.Canonical(m)
}

// selectElements filters elements (sorted by id) and encodes them for a
// paged read of the current state, refusing a result larger than one
// paging snapshot (pageSnapMax); byID resolves connector ends for area
// filters and holds at least els. vers adds each element's guard version.
func selectElements(els []model.Element, byID map[string]model.Element, q Query, vers map[string]int64) ([]json.RawMessage, error) {
	f := newElementFilter(q)
	var out []json.RawMessage
	var total int64
	for _, e := range els {
		if !f.match(e, byID) {
			continue
		}
		b := encodeElement(e, q, vers)
		total += int64(len(b))
		if total > pageSnapMax {
			return nil, errorf(CategoryInvalid, "the read is larger than 32 MiB; narrow it by frame, area or ids, or read a revision")
		}
		out = append(out, b)
	}
	return out, nil
}

func endName(e model.End) string {
	if e.El != "" {
		return e.El
	}
	return "(free)"
}

// pageLimit is the element count of a page for a requested limit.
func pageLimit(limit int) int {
	if limit <= 0 {
		limit = pageDefault
	}
	return min(limit, pageMax)
}

// pageFull reports whether a page holding n elements of size bytes has no
// room for one more of next bytes: limit elements or 1 MiB.
func pageFull(n, size, next, limit int) bool {
	return n > 0 && (n >= limit || size+next > pageBytes)
}

// paginate cuts elements into pages of at most limit elements or 1 MiB.
func paginate(all []json.RawMessage, limit int) [][]json.RawMessage {
	limit = pageLimit(limit)
	pages := [][]json.RawMessage{{}}
	size := 0
	for _, e := range all {
		if pageFull(len(pages[len(pages)-1]), size, len(e), limit) {
			pages = append(pages, []json.RawMessage{})
			size = 0
		}
		pages[len(pages)-1] = append(pages[len(pages)-1], e)
		size += len(e) + 1
	}
	return pages
}

// bounds is an element's axis-aligned box in canvas coordinates.
func bounds(e model.Element, byID map[string]model.Element) model.Geom {
	if e.Type == model.TypeConnector {
		var pts [2][2]float64
		for k, end := range e.Ends {
			pts[k] = end.Pt
			if t, ok := byID[end.El]; end.El != "" && ok {
				pts[k] = model.EndpointWorld(t.Geom, end.At)
			}
		}
		return model.ConnectorBounds(pts[0], pts[1])
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, at := range [][2]float64{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
		p := model.EndpointWorld(e.Geom, at)
		minX, minY = math.Min(minX, p[0]), math.Min(minY, p[1])
		maxX, maxY = math.Max(maxX, p[0]), math.Max(maxY, p[1])
	}
	return model.Geom{X: minX, Y: minY, W: maxX - minX, H: maxY - minY}
}

func intersects(g model.Geom, r [4]float64) bool {
	return g.X <= r[0]+r[2] && g.X+g.W >= r[0] && g.Y <= r[1]+r[3] && g.Y+g.H >= r[1]
}

// UpdateRequest is one update_board call (8.1).
type UpdateRequest struct {
	OpID    string
	Ops     []engine.AgentOp
	Layout  []engine.Layout
	IfSeq   *int64
	Message string
	Replace bool
	Label   string   // the agent's label
	Assets  [][]byte // images the call adds, checked like uploads
}

// UpdateResult is an applied update_board call. Core adds the revision of
// OutboxSeq after its import.
type UpdateResult struct {
	Seq       int64                 `json:"seq"`
	IDs       map[string]string     `json:"ids"`
	Layout    map[string][2]float64 `json:"layout"`
	Results   json.RawMessage       `json:"results"`
	Assets    []AssetInfo           `json:"assets,omitempty"`
	OutboxSeq int64                 `json:"outbox_seq"`
	Replayed  bool                  `json:"-"`
}

func checkOpID(id string) error {
	if len(id) < 8 || len(id) > 120 {
		return errorf(CategoryInvalid, "op_id must be 8 to 120 characters")
	}
	return nil
}

// UpdateBoard applies an agent write whole or not at all (rule 33) with the
// agent boundary of 5.3. Core holds the flat lock and passes its fresh
// measurement of "other". A repeated op_id with the same request returns
// the stored result; with another request it is a conflict.
func (in *Instance) UpdateBoard(ctx context.Context, req UpdateRequest, other int64) (UpdateResult, error) {
	if err := checkOpID(req.OpID); err != nil {
		return UpdateResult{}, err
	}
	mark := in.meas.mark()
	assets, err := checkImages(req.Assets, in.b.Limits().UploadMax)
	if err != nil {
		return UpdateResult{}, err
	}
	hashes := make([]string, len(assets))
	blobs := make([]newBlob, len(assets))
	for i, a := range assets {
		hashes[i] = a.Hash
		blobs[i] = newBlob{a, req.Assets[i]}
	}
	reqHash := sha256Hex(model.Canonical(map[string]any{"ops": req.Ops, "layout": req.Layout, "if_seq": req.IfSeq,
		"message": req.Message, "replace": req.Replace, "assets": hashes}))
	in.meas.install(ctx, other, mark)
	// The images are stored only once a dry run admits the call, so a
	// refused call leaves no blob behind (5.4).
	store, release, err := in.admitBlobs(ctx, blobs, func(t *txn) error {
		var res UpdateResult
		if done, err := in.replay(req.OpID, reqHash, &res); done || err != nil {
			return cmp.Or(err, errReplay)
		}
		return asError(in.agentWrite(t, req, reqHash, assets, &res))
	})
	defer release()
	if err != nil {
		return UpdateResult{}, err
	}
	if store {
		if err := in.putBlobs(ctx, blobs, true); err != nil {
			return UpdateResult{}, err
		}
	}
	release() // the stored blobs count from here on
	var res UpdateResult
	err = in.do(ctx, func() error {
		if done, err := in.replay(req.OpID, reqHash, &res); done || err != nil {
			return err
		}
		t, err := in.begin()
		if err != nil {
			return err
		}
		if err := in.agentWrite(t, req, reqHash, assets, &res); err != nil {
			t.rollback()
			return asError(err)
		}
		return t.commit()
	})
	return res, err
}

// replay answers a repeated op id from ops.
func (in *Instance) replay(opID, hash string, into any) (bool, error) {
	op := in.sd.ops[opID]
	if op == nil {
		return false, nil
	}
	if op.hash != hash {
		return true, errorf(CategoryConflict, "op_id %s was used for a different request", opID)
	}
	if err := json.Unmarshal(op.result, into); err != nil {
		return true, err
	}
	switch r := into.(type) {
	case *UpdateResult:
		r.Replayed, r.OutboxSeq = true, op.outboxSeq
	case *ImportResult:
		r.Replayed, r.OutboxSeq = true, op.outboxSeq
	case *RollbackResult:
		r.Replayed, r.OutboxSeq = true, op.outboxSeq
	}
	return true, nil
}

func (in *Instance) agentWrite(t *txn, req UpdateRequest, reqHash string, assets []AssetInfo, res *UpdateResult) error {
	st := in.st
	now := in.now().Unix()
	if err := in.holdAssets(t, assets, now); err != nil {
		return err
	}
	if err := in.agentBoundary(t); err != nil {
		return err
	}
	out, err := st.ApplyAgent(engine.AgentCall{Ops: req.Ops, Layout: req.Layout, IfSeq: req.IfSeq, Replace: req.Replace,
		Label: req.Label, Now: now, NewID: newID})
	if err != nil {
		return err
	}
	res.Seq, res.IDs, res.Layout, res.Assets = out.Seq, out.IDs, out.Layout, assets
	res.Results, _ = json.Marshal(out.Results)
	if out.Seq == 0 {
		res.Seq = st.Seq()
	}
	kind := "agent"
	if req.Replace {
		kind = "import"
	}
	if err := in.commitAgentChange(t, out.Changed, out.Delta, recordSpec{kind: kind, forced: true, all: req.Replace,
		authors: []string{req.Label}, message: req.Message, op: req.OpID}, &res.OutboxSeq); err != nil {
		return err
	}
	if err := t.putOp(req.OpID, "agent", reqHash, res.OutboxSeq, res); err != nil {
		return err
	}
	return in.checkQuota(t)
}

// agentBoundary closes open edit groups before an agent write, an import
// or a rollback: a person entry when dirty, then no groups (5.3).
func (in *Instance) agentBoundary(t *txn) error {
	if t.anyDirtyGroup() {
		if _, err := t.record(recordSpec{kind: "person", authors: t.dirtyAuthors()}); err != nil {
			return err
		}
	}
	return t.deleteGroups()
}

// commitAgentChange persists a change, logs and broadcasts its delta and
// records its outbox entry when something changed.
func (in *Instance) commitAgentChange(t *txn, c engine.Changed, d *model.Delta, rs recordSpec, outboxSeq *int64) error {
	if err := t.persist(c, in.st.Seq()); err != nil {
		return err
	}
	if d == nil {
		return nil
	}
	rec, err := t.addDelta(d, "", 0)
	if err != nil {
		return err
	}
	t.after = append(t.after, func() { in.broadcast(rec, d) })
	*outboxSeq, err = t.record(rs)
	return err
}

// checkQuota refuses a whole agent write or import that leaves the board
// over its disk quota (5.4); "other" comes from core's measurement.
func (in *Instance) checkQuota(t *txn) error {
	quota := in.b.Limits().DiskQuota
	if quota <= 0 {
		return nil
	}
	other, ok := in.meas.get()
	if !ok {
		return errorf(CategoryUnavailable, "the board's disk usage cannot be measured right now; try again")
	}
	u, err := t.usage(other)
	if err != nil {
		return err
	}
	if u > quota {
		return errorf(CategoryCapacity, "the board would use %d bytes, over its disk quota of %d bytes", u, quota)
	}
	return nil
}

func (in *Instance) holdAssets(t *txn, assets []AssetInfo, now int64) error {
	for _, a := range assets {
		c := in.st.HoldAsset(engine.Asset{Hash: a.Hash, Mime: a.Mime, W: a.W, H: a.H, Bytes: a.Bytes}, now+3600)
		if err := t.persist(c, in.st.Seq()); err != nil {
			return err
		}
	}
	return nil
}

// RollbackResult is a committed whole-board rollback.
type RollbackResult struct {
	Seq       int64 `json:"seq"`
	OutboxSeq int64 `json:"outbox_seq"`
	Replayed  bool  `json:"-"`
}

// Rollback sets the board to revision of's content c (4.11 step 2) as one
// apply-loop item and records a forced rollback entry with opID. Core holds
// the flat lock and the hold on of. It is never refused for disk quota.
func (in *Instance) Rollback(ctx context.Context, opID string, of int, c engine.Content, label string) (RollbackResult, error) {
	var res RollbackResult
	hash := sha256Hex([]byte("rollback:" + label))
	err := in.do(ctx, func() error {
		if op := in.sd.ops[opID]; op != nil {
			_, err := in.replay(opID, op.hash, &res)
			return err
		}
		t, err := in.begin()
		if err != nil {
			return err
		}
		fail := func(err error) error { t.rollback(); return asError(err) }
		if t.anyDirtyGroup() {
			if _, err := t.record(recordSpec{kind: "person", authors: t.dirtyAuthors()}); err != nil {
				return fail(err)
			}
		}
		out, err := in.st.Rollback(int64(of), c, in.now().Unix())
		if err != nil {
			return fail(err)
		}
		out.Delta.By = &model.By{Kind: "system", Label: label}
		res.Seq = out.Seq
		if err := in.commitAgentChange(t, out.Changed, out.Delta, recordSpec{kind: "rollback", forced: true, all: true,
			authors: []string{label}, rollbackOf: of, op: opID}, &res.OutboxSeq); err != nil {
			return fail(err)
		}
		if err := t.putOp(opID, "rollback", hash, res.OutboxSeq, res); err != nil {
			return fail(err)
		}
		return t.commit()
	})
	return res, err
}

// Remeasure starts a new measurement of the flat directory, keeping the old
// value until it arrives: after a blob sweep or a disk quota change (5.4).
func (in *Instance) Remeasure() { in.meas.start() }

// Barrier runs fn as an apply-loop item: no write commits while it runs.
// Core takes the flat lock first (5.5 "Serialization order").
func (in *Instance) Barrier(ctx context.Context, fn func() error) error { return in.do(ctx, fn) }

// References lists what the blob sweep must keep for this board (5.5): the
// hashes of every asset row (referenced, kept for deletion undo or
// protected) and of every snapshot an unacknowledged entry holds.
func (in *Instance) References(ctx context.Context) (assets, snapshots []string, err error) {
	err = in.do(ctx, func() error {
		assets = in.assets.all()
		snapshots = sortedKeys(in.sd.snaps)
		return nil
	})
	return assets, snapshots, err
}

// ShareView is a share with the elements it names as they are now.
type ShareView struct {
	Share
	Elements []json.RawMessage `json:"elements"`
	Missing  []string          `json:"missing"`
}

// Shares lists the recent shares, newest first.
func (in *Instance) Shares() []Share {
	list := in.shares.snapshot()
	slices.Reverse(list)
	return list
}

// GetShare reads share n (rule 26): its elements now, with deleted ones
// (or ones a rollback removed) listed as missing.
func (in *Instance) GetShare(ctx context.Context, n int64) (ShareView, error) {
	in.shares.expire(in.now().Unix())
	var v ShareView
	found := false
	for _, s := range in.shares.snapshot() {
		if s.N == n {
			v.Share, found = s, true
		}
	}
	if !found {
		return v, errorf(CategoryNotFound, "share #%d does not exist or expired (shares last 24 hours or until the board restarts)", n)
	}
	err := in.do(ctx, func() error {
		v.Elements, v.Missing = []json.RawMessage{}, []string{}
		for _, id := range v.IDs {
			e, ok := in.st.Get(id)
			if !ok {
				v.Missing = append(v.Missing, id)
				continue
			}
			m := e.Content()
			m["ver"], _ = in.st.Ver(id)
			v.Elements = append(v.Elements, model.Canonical(m))
		}
		return nil
	})
	return v, err
}
