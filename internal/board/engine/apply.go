package engine

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/gosuda/flats/internal/board/model"
)

// Fatal codes of a batch the engine cannot answer (4.8).
const (
	FatalSessionExpired = "session_expired"
	FatalReceiptExpired = "receipt_expired"
	FatalProtocol       = "protocol"
)

// Reject codes.
const (
	CodeInvalid = "invalid"
	CodeLimit   = "limit"
)

// ErrLimit marks a change that grows an item's excess over a limit (4.5).
var ErrLimit = errors.New("over a board limit")

// Batch is one person batch.
type Batch struct {
	Session string
	N       int64
	Ops     []Op
	Now     int64
}

// Outcome is the result of applying a batch, a rollback or a replacement.
type Outcome struct {
	// Seq is the seq this change received, or 0 when nothing changed.
	Seq int64
	// AckSeq is the seq an ack reports: Seq, or the current seq.
	AckSeq   int64
	Results  json.RawMessage
	Reject   *Reject
	Fatal    string
	Replayed bool // answered from the receipt of an earlier identical batch
	Delta    *model.Delta
	Changed  Changed
}

// applier applies one change inside a savepoint of its own. Every value it
// writes is stamped with seq, which the state adopts only if something
// changed.
type applier struct {
	s         *State
	sp        *savepoint
	seq       int64
	session   string
	by        string
	now       int64
	overwrote map[string]int64
	topZ      string
	topZKnown bool
	startOrd  map[string]int // batch-start stacking positions, built on demand
	startList []string

	// backZ is the lowest stacking key of the alive elements other than
	// the frames backKey placed, or "" when there is no such element (no
	// bound); backLast is the highest key backKey gave below it. Valid
	// while backKnown; put invalidates it when an element lands below
	// the bound, or anywhere while there is none.
	backZ, backLast string
	backKnown       bool
	backFrames      map[string]bool
}

func (s *State) newApplier(session, by string, now int64) (*applier, Mark) {
	m := s.Savepoint()
	return &applier{
		s: s, sp: s.sps[m], seq: s.seq + 1, session: session, by: by, now: now,
		overwrote: map[string]int64{},
	}, m
}

// ApplyBatch applies a person batch of a live session (4.1 to 4.10). A
// format or limit error refuses the whole batch permanently: the refusal is
// recorded as the receipt. Duplicate batch numbers are answered from their
// receipts.
func (s *State) ApplyBatch(b Batch) Outcome {
	se := s.sessions[b.Session]
	if se == nil {
		return Outcome{Fatal: FatalSessionExpired}
	}
	if b.N <= se.MaxN {
		rc := s.receipts[ReceiptKey{b.Session, b.N}]
		if rc == nil {
			return Outcome{Fatal: FatalReceiptExpired}
		}
		return Outcome{Replayed: true, AckSeq: rc.Seq, Results: rc.Results, Reject: rc.Reject}
	}
	if b.N != se.MaxN+1 {
		return Outcome{Fatal: FatalProtocol}
	}
	a, m := s.newApplier(b.Session, se.Key, b.Now)
	results, err := a.applyOps(b.Ops)
	if err != nil {
		s.RollbackTo(m)
		code := CodeInvalid
		if errors.Is(err, ErrLimit) {
			code = CodeLimit
		}
		return s.RecordReject(b.Session, b.N, b.Now, Reject{Code: code, Detail: err.Error()})
	}
	isUndo := b.Ops[0].Op == OpUndo || b.Ops[0].Op == OpRedo
	delta := a.delta()
	if delta != nil && !isUndo {
		a.pushUndo(a.record())
	}
	out := Outcome{AckSeq: s.seq, Results: EncodeResults(results)}
	if delta != nil {
		delta.From = fmt.Sprintf("%s:%d", b.Session, b.N)
		delta.By = &model.By{Kind: "person", Label: se.Label}
		s.seq = a.seq
		out.Seq, out.AckSeq, out.Delta = a.seq, a.seq, delta
	}
	s.putReceipt(&Receipt{Session: b.Session, N: b.N, Seq: out.AckSeq, Results: out.Results})
	a.touchSession(b.N)
	out.Changed = s.changedSince(a.sp)
	s.Release(m)
	return out
}

// RecordReject records a permanent refusal of batch n: its receipt, and the
// session's max_n. The store calls it after rolling back a batch it refused
// itself, for example over the disk quota.
func (s *State) RecordReject(session string, n, now int64, rej Reject) Outcome {
	rej.Retry = false
	m := s.Savepoint()
	sp := s.sps[m]
	s.putReceipt(&Receipt{Session: session, N: n, Seq: s.seq, Reject: &rej})
	if se := s.sessions[session]; se != nil {
		c := *se
		c.MaxN, c.LastAt = max(c.MaxN, n), now
		s.putSession(&c)
	}
	out := Outcome{AckSeq: s.seq, Reject: &rej, Changed: s.changedSince(sp)}
	s.Release(m)
	return out
}

func (a *applier) touchSession(n int64) {
	c := *a.s.sessions[a.session]
	c.MaxN, c.LastAt = n, a.now
	a.s.putSession(&c)
}

func (a *applier) applyOps(ops []Op) ([]any, error) {
	if err := checkOps(ops); err != nil {
		return nil, err
	}
	results := make([]any, 0, len(ops))
	for _, op := range ops {
		r, err := a.apply(op)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, a.finish()
}

func (a *applier) apply(op Op) (any, error) {
	switch op.Op {
	case OpAdd:
		return a.add(op)
	case OpSet:
		return a.set(op)
	case OpRevive:
		return a.reviveOp(op), nil
	case OpReparent:
		return a.reparent(op), nil
	case OpGroup:
		return a.group(op), nil
	case OpUngroup:
		return a.ungroup(op), nil
	case OpDelete:
		return a.deleteOp(op), nil
	case OpDeleteFrameContents:
		return a.deleteFrameContents(op), nil
	case OpZorder:
		return a.zorder(op), nil
	case OpMeta:
		return a.meta(op), nil
	case OpUndo:
		return a.undo(false), nil
	case OpRedo:
		return a.undo(true), nil
	}
	return nil, formatErr("unknown operation %q", op.Op)
}

// batchRow is row id as it was when this change started.
func (a *applier) batchRow(id string) *Row {
	if p, ok := a.sp.rows[id]; ok {
		return p.v
	}
	return a.s.rows[id]
}

// resolve finds the alive element id with expected incarnation inc. An inc
// of 0 resolves through this session's created rows (4.1).
func (a *applier) resolve(id string, inc int64) (*Row, string) {
	r := a.s.rows[id]
	if !r.alive() || id == model.BoardID {
		return nil, VoidGone
	}
	if inc == 0 {
		c, ok := a.s.created[createdKey{a.session, id}]
		if a.session == "" || !ok || c != r.Inc {
			return nil, VoidGone
		}
	} else if inc != r.Inc {
		return nil, VoidGone
	}
	return r, ""
}

// put stores a changed copy of a row.
func (a *applier) put(r *Row) {
	if a.backKnown && r.alive() && r.El.ID != model.BoardID && !a.backFrames[r.El.ID] &&
		(a.backZ == "" || r.El.Z < a.backZ) {
		// The element now lies below the bound backKey keeps, or there was
		// no bound yet (no other alive element when it was computed): the
		// next frame must go below this element too (8.1).
		a.backKnown = false
	}
	a.s.putRow(r.El.ID, r)
}

// setBundle writes bundle g of r (a private copy) and stamps its version.
func (a *applier) setBundle(r *Row, g string, val any) {
	r.El.SetBundle(g, val)
	r.V[g] = a.seq
	if k := model.EndIndex(g); k >= 0 {
		r.EndInc[k] = 0
		if t := a.s.rows[r.El.Ends[k].El]; r.El.Ends[k].El != "" && t != nil {
			r.EndInc[k] = t.Inc
		}
	}
	if g == model.BundleZ {
		a.noteZ(val.(string))
		a.backKnown = false
	}
}

func (a *applier) noteZ(z string) {
	if a.topZKnown && z > a.topZ {
		a.topZ = z
	}
}

// topKey returns a stacking key above every alive element.
func (a *applier) topKey() string {
	if !a.topZKnown {
		for _, r := range a.s.rows {
			if r.alive() && r.El.Z > a.topZ {
				a.topZ = r.El.Z
			}
		}
		a.topZKnown = true
	}
	k, _ := model.KeyBetween(a.topZ, "")
	a.topZ = k
	return k
}

// backKey returns a stacking key for a new frame of an agent call (8.1):
// below every alive element except the frames this call already placed
// this way, and above those, so the call's frames keep their order among
// themselves behind everything else.
func (a *applier) backKey(id string) string {
	if !a.backKnown {
		a.backZ, a.backLast = "", ""
		first := true
		for rid, r := range a.s.rows {
			if r.alive() && rid != model.BoardID && !a.backFrames[rid] && (first || r.El.Z < a.backZ) {
				a.backZ, first = r.El.Z, false
			}
		}
		for rid := range a.backFrames {
			if r := a.s.rows[rid]; r.alive() && (first || r.El.Z < a.backZ) && r.El.Z > a.backLast {
				a.backLast = r.El.Z
			}
		}
		a.backKnown = true
	}
	k, _ := model.KeyBetween(a.backLast, a.backZ)
	a.backLast = k
	if a.backFrames == nil {
		a.backFrames = map[string]bool{}
	}
	a.backFrames[id] = true
	return k
}

func (a *applier) assetKnown(hash string) bool {
	as := a.s.assets[hash]
	return as != nil && (as.Refs > 0 || as.DeletedRefs > 0 || as.HeldUntil > a.now)
}

// add applies {op:"add", el, refs}.
func (a *applier) add(op Op) (any, error) {
	el, present, err := model.DecodeElement(op.El)
	if err != nil {
		return nil, err
	}
	if !model.ValidID(el.ID) {
		return void(VoidExists), nil
	}
	if err := requireFields(el, present); err != nil {
		return nil, err
	}
	el.Normalize()
	if err := el.Validate(); err != nil {
		return nil, err
	}
	if a.s.rows[el.ID] != nil {
		return void(VoidExists), nil
	}
	resolveRef := func(id string) (*Row, bool) {
		inc, ok := op.Refs[id]
		if !ok {
			return nil, false
		}
		r, _ := a.resolve(id, inc)
		return r, r != nil
	}
	if reason := a.checkRefs(el, resolveRef); reason != "" {
		return void(reason), nil
	}
	a.insert(el)
	return ResultOK, nil
}

func requireFields(el model.Element, present model.Present) error {
	need := []string{"id"}
	switch el.Type {
	case model.TypeConnector:
		need = append(need, "ends")
	case model.TypePen:
		need = append(need, "pts")
	case model.TypeImage:
		need = append(need, "asset", "natural")
	}
	if el.Type != model.TypeConnector && !present.HasGeom() {
		return formatErr("a %s needs x, y, w and h", el.Type)
	}
	for _, f := range need {
		if !present[f] {
			return formatErr("a %s needs %s", el.Type, f)
		}
	}
	return nil
}

// checkRefs checks the elements, group and asset a new element references.
func (a *applier) checkRefs(el model.Element, ref func(id string) (*Row, bool)) string {
	if el.Frame != "" {
		f, ok := ref(el.Frame)
		if !ok || f.El.Type != model.TypeFrame {
			return VoidRef
		}
	}
	if el.Group != "" {
		mates := a.s.groups[el.Group]
		if len(mates) == 0 {
			return VoidRef
		}
		for m := range mates {
			if a.s.rows[m].El.Frame != el.Frame {
				return VoidRef
			}
		}
	}
	if el.Type == model.TypeConnector {
		for _, end := range el.Ends {
			if end.El == "" {
				continue
			}
			t, ok := ref(end.El)
			if !ok || t.El.Type == model.TypeConnector {
				return VoidRef
			}
		}
	}
	if el.Type == model.TypeImage && !a.assetKnown(el.Asset) {
		return VoidRef
	}
	return ""
}

// insert creates the row of a new element whose references are checked.
func (a *applier) insert(el model.Element) {
	el.By, el.At = a.by, a.now
	if el.Z == "" {
		el.Z = a.topKey()
	}
	r := &Row{El: el, Inc: a.seq, V: map[string]int64{}}
	for _, g := range model.Bundles(el.Type) {
		r.V[g] = a.seq
	}
	if el.Type == model.TypeConnector {
		for k, end := range el.Ends {
			if end.El != "" {
				r.EndInc[k] = a.s.rows[end.El].Inc
			}
		}
	}
	a.noteZ(el.Z)
	a.put(r)
	if a.session != "" {
		a.s.setCreated(createdKey{a.session, el.ID}, a.seq)
	}
}

// set applies {op:"set", id, inc, g, val, if_v, base_v, carry, refs}.
//
// The expected incarnation is resolved before the value is checked against
// the element's type: a reset may have reused the id for another type under
// a new incarnation, and an operation against the old incarnation is then
// void:gone like any other, so the rest of its batch still applies (4.1,
// rule 31). checkOp already refused values no element type accepts; a value
// the resolved incarnation's own type does not accept is a format error,
// since the client named that incarnation and so knew its type.
func (a *applier) set(op Op) (any, error) {
	ifv, hasIfV, _ := decodeV(op.IfV)
	cur, reason := a.resolve(op.ID, op.Inc)
	if cur == nil {
		return void(reason), nil
	}
	if !model.Settable(cur.El.Type, op.G) {
		return nil, formatErr("a %s has no settable %q", cur.El.Type, op.G)
	}
	val, err := model.DecodeBundle(cur.El.Type, op.G, op.Val)
	if err != nil {
		return nil, err
	}
	if hasIfV && cur.V[op.G] != ifv {
		return void(VoidStale), nil
	}
	if cur.El.Locked && op.G != model.BundleLocked {
		return void(VoidLocked), nil
	}
	if end, ok := val.(model.End); ok && end.El != "" {
		inc, given := op.Refs[end.El]
		t, _ := a.resolve(end.El, inc)
		if !given || t == nil || t.El.Type == model.TypeConnector {
			return void(VoidRef), nil
		}
	}
	a.write(cur, op.G, val, op.Carry, op.BaseV)
	return ResultOK, nil
}

// write sets one bundle of an alive element and applies container
// following (4.2) and the overwritten-text mark (4.9).
func (a *applier) write(cur *Row, g string, val any, carry string, baseV int64) {
	r := cur.clone()
	if g == model.BundleText && baseV != 0 && cur.V[g] != baseV {
		a.overwrote[cur.El.ID] = cur.V[g]
	}
	a.setBundle(r, g, val)
	a.put(r)
	if g != model.BundleGeom {
		return
	}
	dx, dy := r.El.Geom.X-cur.El.Geom.X, r.El.Geom.Y-cur.El.Geom.Y
	if dx == 0 && dy == 0 {
		return
	}
	var followers []string
	switch {
	case carry == CarryMembers && r.El.Type == model.TypeFrame:
		followers = slices.Sorted(maps.Keys(a.s.members[r.El.ID]))
	case carry == CarryGroup && r.El.Group != "":
		followers = slices.Sorted(maps.Keys(a.s.groups[r.El.Group]))
	}
	for _, id := range followers {
		if id != r.El.ID {
			a.moveBy(id, dx, dy)
		}
	}
}

// moveBy moves an element that follows its container; a connector moves only
// its free ends. Positions are clamped to the canvas bounds.
func (a *applier) moveBy(id string, dx, dy float64) {
	r := a.s.rows[id].clone()
	shift := func(v, d float64) float64 {
		return model.Round1(math.Max(-model.MaxCoord, math.Min(model.MaxCoord, v+d)))
	}
	if r.El.Type == model.TypeConnector {
		moved := false
		for k, end := range r.El.Ends {
			if end.El == "" {
				end.Pt = [2]float64{shift(end.Pt[0], dx), shift(end.Pt[1], dy)}
				a.setBundle(r, model.EndBundle(k), end)
				moved = true
			}
		}
		if moved {
			a.put(r)
		}
		return
	}
	g := r.El.Geom
	g.X, g.Y = shift(g.X, dx), shift(g.Y, dy)
	a.setBundle(r, model.BundleGeom, g)
	a.put(r)
}

// reviveOp applies {op:"revive", ids, if_del}.
func (a *applier) reviveOp(op Op) any {
	res := a.reviveSet(op.IDs, op.IfDel)
	if a.session != "" {
		for _, id := range res.OK {
			a.s.setCreated(createdKey{a.session, id}, a.s.rows[id].Inc)
		}
	}
	return res
}

// finish rekeys stacking keys that grew too long and checks the per-item
// limits by the "excess grew" rule (4.5).
func (a *applier) finish() error {
	for id := range a.sp.rows {
		if r := a.s.rows[id]; r.alive() && len(r.El.Z) > model.MaxZLen {
			a.rekey()
			break
		}
	}
	return a.checkLimits()
}

func (a *applier) checkLimits() error {
	l := a.s.limits
	excess := func(n, limit int) int { return max(0, n-limit) }
	if excess(a.s.alive, l.Elements) > excess(a.sp.aliveCount, l.Elements) {
		return fmt.Errorf("%w: a board holds at most %d elements", ErrLimit, l.Elements)
	}
	if excess(a.s.live, l.Assets) > excess(a.sp.live, l.Assets) {
		return fmt.Errorf("%w: a board references at most %d images", ErrLimit, l.Assets)
	}
	for _, id := range slices.Sorted(maps.Keys(a.sp.rows)) {
		old, cur := a.sp.rows[id].v, a.s.rows[id]
		if !cur.alive() || id == model.BoardID {
			continue
		}
		// grew compares an item with the same id before the change, alive
		// or not; a replacement may reuse a live id with new content or a
		// new type, so even a stroke's immutable points are compared. Each
		// state is measured against the limit of its own type: a sticky's
		// text that becomes a frame's name is judged by the name limit.
		grew := func(n func(*Row) int, limit func(*Row) int) bool {
			before := 0
			if old.alive() {
				before = excess(n(old), limit(old))
			}
			return excess(n(cur), limit(cur)) > before
		}
		fixed := func(limit int) func(*Row) int { return func(*Row) int { return limit } }
		textLimit := func(r *Row) int { return l.TextLimit(r.El.Type) }
		if grew(func(r *Row) int { return model.TextLen(r.El.Text) }, textLimit) {
			return fmt.Errorf("%w: text of %s is over %d characters", ErrLimit, id, textLimit(cur))
		}
		if grew(func(r *Row) int { return model.TextLen(r.El.Alt) }, fixed(l.Alt)) {
			return fmt.Errorf("%w: alternative text of %s is over %d characters", ErrLimit, id, l.Alt)
		}
		if grew(func(r *Row) int { return r.El.PenPoints() }, fixed(l.PenPoints)) {
			return fmt.Errorf("%w: a pen stroke holds at most %d points", ErrLimit, l.PenPoints)
		}
	}
	return nil
}

// order returns the alive elements sorted by (z, id), the full stacking order.
func (s *State) order() []string {
	ids := make([]string, 0, s.alive)
	for id, r := range s.rows {
		if r.alive() && id != model.BoardID {
			ids = append(ids, id)
		}
	}
	s.sortByZ(ids)
	return ids
}

func (s *State) sortByZ(ids []string) {
	slices.SortFunc(ids, func(x, y string) int {
		return cmp.Or(cmp.Compare(s.rows[x].El.Z, s.rows[y].El.Z), cmp.Compare(x, y))
	})
}

// rekey gives every alive element a short key in the current order. It does
// not change v.z (section 12).
func (a *applier) rekey() {
	ids := a.s.order()
	for i, k := range model.Rekey(len(ids)) {
		if r := a.s.rows[ids[i]]; r.El.Z != k {
			c := r.clone()
			c.El.Z = k
			a.put(c)
		}
	}
	a.topZKnown = false
	a.backKnown = false
}

// delta builds the canonical delta of this change from the rows it touched,
// or returns nil when nothing changed.
func (a *applier) delta() *model.Delta {
	d := &model.Delta{Seq: a.seq}
	for _, id := range slices.Sorted(maps.Keys(a.sp.rows)) {
		if c, ok := diffRow(id, a.sp.rows[id].v, a.s.rows[id]); ok {
			c.Overwrote = a.overwrote[id]
			d.Changes = append(d.Changes, c)
		}
	}
	if len(d.Changes) == 0 {
		return nil
	}
	return d
}

func diffRow(id string, old, cur *Row) (model.Change, bool) {
	switch {
	case cur == nil:
		return model.Change{}, false
	case !old.alive() && cur.alive():
		p := private(cur)
		if old == nil {
			return model.Change{ID: id, Inc: cur.Inc, Add: &p}, true
		}
		return model.Change{ID: id, Inc: cur.Inc, Revive: &p}, true
	case old.alive() && !cur.alive():
		return model.Change{ID: id, Del: cur.DelSeq}, true
	case !cur.alive():
		if old == nil || old.DelSeq != cur.DelSeq {
			return model.Change{ID: id, Del: cur.DelSeq}, true
		}
		return model.Change{}, false
	case old.Inc != cur.Inc || old.El.Type != cur.El.Type || !sameUnversioned(old.El, cur.El) || endIncMoved(old, cur):
		// The author fields and the immutable pen points, image asset and
		// natural size belong to no bundle, so an element whose such field
		// a reset rewrote goes out whole, like a new incarnation (4.6). So
		// does a connector whose end target a reset gave a new incarnation
		// while the end itself stayed: no set would carry its end_inc.
		p := private(cur)
		return model.Change{ID: id, Inc: cur.Inc, Revive: &p}, true
	}
	set := map[string]model.SetVal{}
	for _, g := range model.Bundles(cur.El.Type) {
		// Values change only with their version, except stacking keys a
		// rekey rewrote.
		if old.V[g] != cur.V[g] || (g == model.BundleZ && old.El.Z != cur.El.Z) {
			set[g] = model.SetVal{Val: model.BundleJSON(g, cur.El.Bundle(g)), V: cur.V[g]}
		}
	}
	if len(set) == 0 {
		return model.Change{}, false
	}
	return model.Change{ID: id, Inc: cur.Inc, Set: set}, true
}

// endIncMoved reports whether a connector end's target incarnation changed
// without a change of the end itself. A set of an end carries the end, and
// the client takes end_inc from the target it names, which is alive with
// that incarnation (4.4); only a reset that keeps the end while giving its
// target id a new incarnation changes end_inc alone (4.11, 4.12).
func endIncMoved(old, cur *Row) bool {
	for k := range 2 {
		if old.EndInc[k] != cur.EndInc[k] && old.V[model.EndBundle(k)] == cur.V[model.EndBundle(k)] {
			return true
		}
	}
	return false
}

// sameUnversioned reports whether two states of an element have the same
// fields outside every bundle: the author fields and the immutable pen
// points, image asset and natural size. Only a reset changes them without a
// new incarnation (4.11, 4.12).
func sameUnversioned(x, y model.Element) bool {
	return x.By == y.By && x.At == y.At && x.Asset == y.Asset && x.Natural == y.Natural && samePts(x.Pts, y.Pts)
}

// samePts compares pen points, which clones share, without walking a shared
// list.
func samePts(x, y []int) bool {
	if len(x) != len(y) {
		return false
	}
	return len(x) == 0 || &x[0] == &y[0] || slices.Equal(x, y)
}
