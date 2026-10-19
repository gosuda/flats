package engine

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"

	"github.com/gosuda/flats/internal/board/model"
)

// MaxUndoEntries is the number of entries kept per session and stack.
const MaxUndoEntries = 100

// UndoEntry is one entry of a session's server-side undo or redo stack
// (4.10): what one batch actually changed. Applying an entry reverts it and
// produces the inverse entry for the opposite stack.
type UndoEntry struct {
	Idx       int64       `json:"idx"`
	Bundles   []BundleRec `json:"bundles,omitempty"`
	Creates   []CreateRec `json:"creates,omitempty"`
	Deletes   *DeleteRec  `json:"deletes,omitempty"`
	Relations []RelRec    `json:"relations,omitempty"`
	Orders    []OrderRec  `json:"orders,omitempty"`
}

// BundleRec is a bundle value change. TInc is the incarnation of the
// target a previous attached end value points to.
type BundleRec struct {
	ID    string          `json:"id"`
	Inc   int64           `json:"inc"`
	G     string          `json:"g"`
	Prev  json.RawMessage `json:"prev"`
	TInc  int64           `json:"tinc,omitempty"`
	PrevV int64           `json:"prev_v"`
	MyV   int64           `json:"my_v"`
}

// CreateRec is a creation, with the version of every bundle at that moment.
type CreateRec struct {
	ID  string           `json:"id"`
	Inc int64            `json:"inc"`
	MyV map[string]int64 `json:"my_v"`
}

// DeleteRec is a deletion; the relations are in the rows' pre_rel.
type DeleteRec struct {
	IDs    []string `json:"ids"`
	DelSeq int64    `json:"del_seq"`
}

// RelRec is a membership change of one bundle (frame or group).
type RelRec struct {
	ID      string `json:"id"`
	Inc     int64  `json:"inc"`
	G       string `json:"g"`
	Prev    string `json:"prev"`
	PrevInc int64  `json:"prev_inc,omitempty"`
	PrevV   int64  `json:"prev_v"`
	MyV     int64  `json:"my_v"`
}

// OrderRec is a stacking order change, reverted by the previous neighbor
// below (After, "" for the bottom).
type OrderRec struct {
	ID       string `json:"id"`
	Inc      int64  `json:"inc"`
	After    string `json:"after"`
	AfterInc int64  `json:"after_inc,omitempty"`
	PrevV    int64  `json:"prev_v"`
	MyV      int64  `json:"my_v"`
}

func (e *UndoEntry) empty() bool {
	return len(e.Bundles) == 0 && len(e.Creates) == 0 && e.Deletes == nil && len(e.Relations) == 0 && len(e.Orders) == 0
}

func (e *UndoEntry) clone() *UndoEntry {
	c := *e
	c.Bundles = slices.Clone(e.Bundles)
	c.Creates = slices.Clone(e.Creates)
	for i := range c.Creates {
		c.Creates[i].MyV = maps.Clone(c.Creates[i].MyV)
	}
	if e.Deletes != nil {
		d := *e.Deletes
		c.Deletes = &d
	}
	c.Relations = slices.Clone(e.Relations)
	c.Orders = slices.Clone(e.Orders)
	return &c
}

// myV returns the my_v this entry holds for (id, inc, g).
func (e *UndoEntry) myV(id string, inc int64, g string) (int64, bool) {
	for _, b := range e.Bundles {
		if b.ID == id && b.Inc == inc && b.G == g {
			return b.MyV, true
		}
	}
	for _, r := range e.Relations {
		if r.ID == id && r.Inc == inc && r.G == g {
			return r.MyV, true
		}
	}
	for _, o := range e.Orders {
		if o.ID == id && o.Inc == inc && g == model.BundleZ {
			return o.MyV, true
		}
	}
	for _, c := range e.Creates {
		if v, ok := c.MyV[g]; ok && c.ID == id && c.Inc == inc {
			return v, true
		}
	}
	return 0, false
}

func (e *UndoEntry) setMyV(id string, inc int64, g string, v int64) {
	for i := range e.Bundles {
		if b := &e.Bundles[i]; b.ID == id && b.Inc == inc && b.G == g {
			b.MyV = v
		}
	}
	for i := range e.Relations {
		if r := &e.Relations[i]; r.ID == id && r.Inc == inc && r.G == g {
			r.MyV = v
		}
	}
	for i := range e.Orders {
		if o := &e.Orders[i]; o.ID == id && o.Inc == inc && g == model.BundleZ {
			o.MyV = v
		}
	}
	for _, c := range e.Creates {
		if _, ok := c.MyV[g]; ok && c.ID == id && c.Inc == inc {
			c.MyV[g] = v
		}
	}
}

// relink replaces incarnation old of id with new everywhere in the entry.
// It reports whether anything changed.
func (e *UndoEntry) relink(id string, old, new int64) bool {
	changed := false
	fix := func(eid string, inc *int64) {
		if eid == id && *inc == old {
			*inc, changed = new, true
		}
	}
	for i := range e.Bundles {
		b := &e.Bundles[i]
		fix(b.ID, &b.Inc)
		if b.TInc != 0 {
			var end model.End
			if json.Unmarshal(b.Prev, &end) == nil {
				fix(end.El, &b.TInc)
			}
		}
	}
	for i := range e.Creates {
		fix(e.Creates[i].ID, &e.Creates[i].Inc)
	}
	for i := range e.Relations {
		r := &e.Relations[i]
		fix(r.ID, &r.Inc)
		if r.G == model.BundleFrame {
			fix(r.Prev, &r.PrevInc)
		}
	}
	for i := range e.Orders {
		o := &e.Orders[i]
		fix(o.ID, &o.Inc)
		fix(o.After, &o.AfterInc)
	}
	return changed
}

// record builds the undo entry of an ordinary batch from the rows it
// changed.
func (a *applier) record() *UndoEntry {
	e := &UndoEntry{}
	var deleted []string
	for _, id := range slices.Sorted(maps.Keys(a.sp.rows)) {
		old, cur := a.sp.rows[id].v, a.s.rows[id]
		switch {
		case !old.alive() && cur.alive():
			e.Creates = append(e.Creates, CreateRec{ID: id, Inc: cur.Inc, MyV: maps.Clone(cur.V)})
			continue
		case old.alive() && !cur.alive():
			deleted = append(deleted, id)
		case !old.alive() || old.Inc != cur.Inc:
			continue
		}
		a.recordBundles(e, old, cur)
	}
	if len(deleted) > 0 {
		e.Deletes = &DeleteRec{IDs: deleted, DelSeq: a.seq}
	}
	slices.SortFunc(e.Orders, func(x, y OrderRec) int {
		return cmp.Compare(a.startPos(x.ID), a.startPos(y.ID))
	})
	return e
}

func (a *applier) recordBundles(e *UndoEntry, old, cur *Row) {
	id := cur.El.ID
	for _, g := range model.Bundles(cur.El.Type) {
		if old.V[g] == cur.V[g] {
			continue
		}
		switch {
		case model.IsRelation(g):
			prev := old.El.Bundle(g).(string)
			rec := RelRec{ID: id, Inc: old.Inc, G: g, Prev: prev, PrevV: old.V[g], MyV: cur.V[g]}
			if f := a.batchRow(prev); g == model.BundleFrame && prev != "" && f != nil {
				rec.PrevInc = f.Inc
			}
			e.Relations = append(e.Relations, rec)
		case g == model.BundleZ:
			after, afterInc := a.startNeighbor(id)
			e.Orders = append(e.Orders, OrderRec{ID: id, Inc: old.Inc, After: after, AfterInc: afterInc, PrevV: old.V[g], MyV: cur.V[g]})
		default:
			rec := BundleRec{ID: id, Inc: old.Inc, G: g, Prev: model.BundleJSON(g, old.El.Bundle(g)), PrevV: old.V[g], MyV: cur.V[g]}
			if k := model.EndIndex(g); k >= 0 && old.El.Ends[k].El != "" {
				rec.TInc = old.EndInc[k]
			}
			e.Bundles = append(e.Bundles, rec)
		}
	}
}

// startPos is the position of id in the stacking order at the start of
// this change.
func (a *applier) startPos(id string) int {
	if a.startOrd == nil {
		ids := slices.Collect(maps.Keys(a.s.rows))
		for id := range a.sp.rows {
			if a.s.rows[id] == nil {
				ids = append(ids, id)
			}
		}
		ids = slices.DeleteFunc(ids, func(id string) bool { return !a.batchRow(id).alive() || id == model.BoardID })
		slices.SortFunc(ids, func(x, y string) int {
			return cmp.Or(cmp.Compare(a.batchRow(x).El.Z, a.batchRow(y).El.Z), cmp.Compare(x, y))
		})
		a.startOrd = make(map[string]int, len(ids))
		a.startList = ids
		for i, id := range ids {
			a.startOrd[id] = i
		}
	}
	return a.startOrd[id]
}

func (a *applier) startNeighbor(id string) (string, int64) {
	i := a.startPos(id)
	if i == 0 {
		return "", 0
	}
	nb := a.startList[i-1]
	return nb, a.batchRow(nb).Inc
}

// pushUndo pushes the entry of an ordinary batch and clears redo.
func (a *applier) pushUndo(e *UndoEntry) {
	if e.empty() {
		return
	}
	st := a.s.editStacks(a.session)
	e.Idx = st.next
	st.next++
	st.undo = trimStack(append(st.undo, e))
	st.redo = nil
}

func trimStack(s []*UndoEntry) []*UndoEntry {
	if len(s) > MaxUndoEntries {
		return slices.Clone(s[len(s)-MaxUndoEntries:])
	}
	return s
}

// undoRun applies one popped entry.
type undoRun struct {
	a       *applier
	st      *stacks
	src     *[]*UndoEntry // the stack the entry came from
	relink  map[string][2]int64
	res     *UndoResult
	inv     *UndoEntry
	revived []string
}

// undo applies {op:"undo"} or {op:"redo"} (4.10).
func (a *applier) undo(redo bool) any {
	res := newUndoResult()
	name := StackUndo
	if redo {
		name = StackRedo
	}
	if cur := a.s.undo[a.session]; cur == nil || len(cur.stack(name)) == 0 {
		return res
	}
	st := a.s.editStacks(a.session)
	src, dst := &st.undo, &st.redo
	if redo {
		src, dst = dst, src
	}
	e := (*src)[len(*src)-1]
	*src = (*src)[:len(*src)-1]
	u := &undoRun{a: a, st: st, src: src, relink: map[string][2]int64{}, res: res, inv: &UndoEntry{}}
	u.run(e)
	if !u.inv.empty() {
		u.inv.Idx = st.next
		st.next++
		*dst = trimStack(append(*dst, u.inv))
	}
	return res
}

func (u *undoRun) linked(id string, inc int64) int64 {
	if l, ok := u.relink[id]; ok && inc == l[0] {
		return l[1]
	}
	return inc
}

func (u *undoRun) run(e *UndoEntry) {
	var deferred []BundleRec
	for _, rec := range e.Bundles {
		if u.ready(rec) {
			u.bundle(rec)
		} else {
			deferred = append(deferred, rec)
		}
	}
	u.creates(e.Creates)
	if e.Deletes != nil {
		u.deletes(*e.Deletes)
	}
	for _, rec := range deferred {
		u.bundle(rec)
	}
	u.relations(e.Relations)
	for _, rec := range e.Orders {
		u.order(rec)
	}
	for _, id := range u.revived {
		r := u.a.s.rows[id]
		u.inv.Creates = append(u.inv.Creates, CreateRec{ID: id, Inc: r.Inc, MyV: maps.Clone(r.V)})
	}
}

// ready reports whether a bundle record can apply before deletions are
// undone: its element is alive and so is the target of an attached value.
func (u *undoRun) ready(rec BundleRec) bool {
	if !u.a.s.rows[rec.ID].alive() {
		return false
	}
	if model.EndIndex(rec.G) < 0 {
		return true
	}
	var end model.End
	return json.Unmarshal(rec.Prev, &end) == nil && (end.El == "" || u.a.s.rows[end.El].alive())
}

// guard checks the version of bundle g of r against my_v. A bundle this
// same undo already set back to prev counts as undone.
func (u *undoRun) guard(r *Row, g string, myV int64, prev any) bool {
	if r.V[g] == myV {
		return true
	}
	if r.V[g] == u.a.seq && model.BundleEqual(g, r.El.Bundle(g), prev) {
		u.res.Undone++
	} else {
		u.res.skip(VoidStale)
	}
	return false
}

func (u *undoRun) bundle(rec BundleRec) {
	a := u.a
	r := a.s.rows[rec.ID]
	if !r.alive() || r.Inc != u.linked(rec.ID, rec.Inc) {
		u.res.skip(VoidGone)
		return
	}
	prev, err := model.DecodeBundle(r.El.Type, rec.G, rec.Prev)
	if err != nil {
		u.res.skip(VoidGone)
		return
	}
	if !u.guard(r, rec.G, rec.MyV, prev) {
		return
	}
	if r.El.Locked && rec.G != model.BundleLocked {
		u.res.skip(VoidLocked)
		return
	}
	if end, ok := prev.(model.End); ok && end.El != "" {
		t := a.s.rows[end.El]
		if !t.alive() || t.Inc != u.linked(end.El, rec.TInc) || t.El.Type == model.TypeConnector {
			u.res.skip(VoidGone)
			return
		}
	}
	cur := r.El.Bundle(rec.G)
	inv := BundleRec{ID: rec.ID, Inc: r.Inc, G: rec.G, Prev: model.BundleJSON(rec.G, cur), PrevV: r.V[rec.G], MyV: a.seq}
	if k := model.EndIndex(rec.G); k >= 0 && r.El.Ends[k].El != "" {
		inv.TInc = r.EndInc[k]
	}
	nr := r.clone()
	a.setBundle(nr, rec.G, prev)
	a.put(nr)
	u.inv.Bundles = append(u.inv.Bundles, inv)
	u.lineage(rec.ID, r.Inc, rec.G, rec.PrevV)
	u.res.Undone++
}

// creates undoes creations: an element is deleted only when nobody changed
// any of its bundles since.
func (u *undoRun) creates(recs []CreateRec) {
	var del []string
	for _, rec := range recs {
		r := u.a.s.rows[rec.ID]
		switch {
		case !r.alive() || r.Inc != u.linked(rec.ID, rec.Inc):
			u.res.skip(VoidGone)
		case r.El.Locked:
			u.res.skip(VoidLocked)
		case !versionsMatch(r, rec.MyV):
			u.res.skip(VoidStale)
		default:
			del = append(del, rec.ID)
		}
	}
	if len(del) == 0 {
		return
	}
	u.a.deleteSet(del)
	u.res.Undone += len(del)
	u.inv.Deletes = &DeleteRec{IDs: slices.Sorted(slices.Values(del)), DelSeq: u.a.seq}
}

// deletes undoes a deletion with the two-phase revive, then links the new
// incarnations into this session's stacks.
func (u *undoRun) deletes(rec DeleteRec) {
	a := u.a
	res := a.reviveSet(rec.IDs, rec.DelSeq)
	for _, reason := range res.Void {
		u.res.skip(reason)
	}
	for _, id := range res.OK {
		old, cur := a.batchRow(id).Inc, a.s.rows[id].Inc
		u.link(id, old, cur)
		a.s.setCreated(createdKey{a.session, id}, cur)
		u.revived = append(u.revived, id)
	}
	u.res.Undone += len(res.OK)
}

// link moves this session's records of id from incarnation old to new.
func (u *undoRun) link(id string, old, new int64) {
	u.relink[id] = [2]int64{old, new}
	for _, stack := range []*[]*UndoEntry{&u.st.undo, &u.st.redo} {
		for i, e := range *stack {
			c := e.clone()
			if c.relink(id, old, new) {
				(*stack)[i] = c
			}
		}
	}
}

// lineage carries my_v down to the nearest lower entry of the same stack
// for (id, g) when nobody changed the bundle in between (4.10).
func (u *undoRun) lineage(id string, inc int64, g string, prevV int64) {
	stack := *u.src
	for i := len(stack) - 1; i >= 0; i-- {
		v, ok := stack[i].myV(id, inc, g)
		if !ok {
			continue
		}
		if v == prevV {
			c := stack[i].clone()
			c.setMyV(id, inc, g, u.a.seq)
			stack[i] = c
		}
		return
	}
}

type relCand struct {
	rec RelRec
	inc int64
}

// relations undoes membership changes. The final relations of every
// candidate are computed first; a set connected by groups that would break
// the group invariant, or point at a dead frame, is skipped (4.10 step 4).
func (u *undoRun) relations(recs []RelRec) {
	a := u.a
	var cands []relCand
	for _, rec := range recs {
		r := a.s.rows[rec.ID]
		inc := u.linked(rec.ID, rec.Inc)
		switch {
		case !r.alive() || r.Inc != inc:
			u.res.skip(VoidGone)
		case !u.guard(r, rec.G, rec.MyV, rec.Prev):
		case r.El.Locked:
			u.res.skip(VoidLocked)
		default:
			cands = append(cands, relCand{rec, inc})
		}
	}
	for {
		bad := u.badCandidates(cands)
		if len(bad) == 0 {
			break
		}
		kept := cands[:0]
		for i, c := range cands {
			if reason, ok := bad[i]; ok {
				u.res.skip(reason)
			} else {
				kept = append(kept, c)
			}
		}
		cands = kept
	}
	for _, c := range cands {
		r := a.s.rows[c.rec.ID]
		cur := r.El.Bundle(c.rec.G).(string)
		inv := RelRec{ID: c.rec.ID, Inc: r.Inc, G: c.rec.G, Prev: cur, PrevV: r.V[c.rec.G], MyV: a.seq}
		if f := a.s.rows[cur]; c.rec.G == model.BundleFrame && cur != "" && f != nil {
			inv.PrevInc = f.Inc
		}
		a.setRel(c.rec.ID, c.rec.G, c.rec.Prev)
		u.inv.Relations = append(u.inv.Relations, inv)
		u.lineage(c.rec.ID, r.Inc, c.rec.G, c.rec.PrevV)
		u.res.Undone++
	}
}

func (u *undoRun) badCandidates(cands []relCand) map[int]string {
	a := u.a
	tent := map[string]map[string]string{} // id → g → value
	for _, c := range cands {
		if tent[c.rec.ID] == nil {
			tent[c.rec.ID] = map[string]string{}
		}
		tent[c.rec.ID][c.rec.G] = c.rec.Prev
	}
	value := func(id, g string) string {
		if v, ok := tent[id][g]; ok {
			return v
		}
		return a.s.rows[id].El.Bundle(g).(string)
	}
	bad := map[int]string{}
	groups := map[string]bool{}
	for i, c := range cands {
		r := a.s.rows[c.rec.ID]
		if c.rec.G == model.BundleFrame && c.rec.Prev != "" {
			f := a.s.rows[c.rec.Prev]
			if !f.alive() || f.Inc != u.linked(c.rec.Prev, c.rec.PrevInc) || f.El.Type != model.TypeFrame || r.El.Type == model.TypeFrame {
				bad[i] = VoidGone
			}
		}
		if c.rec.G == model.BundleGroup && c.rec.Prev != "" && r.El.Type == model.TypeFrame {
			bad[i] = VoidInvalidGroup
		}
		for _, g := range []string{r.El.Group, value(c.rec.ID, model.BundleGroup)} {
			if g != "" {
				groups[g] = true
			}
		}
	}
	for _, g := range slices.Sorted(maps.Keys(groups)) {
		ids := slices.Collect(maps.Keys(a.s.groups[g]))
		ids = append(ids, slices.Collect(maps.Keys(tent))...)
		frames := map[string]bool{}
		for _, id := range ids {
			if value(id, model.BundleGroup) == g {
				frames[value(id, model.BundleFrame)] = true
			}
		}
		if len(frames) <= 1 {
			continue
		}
		for i, c := range cands {
			if _, done := bad[i]; done {
				continue
			}
			if value(c.rec.ID, model.BundleGroup) == g || a.s.rows[c.rec.ID].El.Group == g {
				bad[i] = VoidInvalidGroup
			}
		}
	}
	return bad
}

// order undoes a stacking change by placing the element back just above
// its previous neighbor.
func (u *undoRun) order(rec OrderRec) {
	a := u.a
	r := a.s.rows[rec.ID]
	if !r.alive() || r.Inc != u.linked(rec.ID, rec.Inc) {
		u.res.skip(VoidGone)
		return
	}
	if r.V[model.BundleZ] != rec.MyV {
		if r.V[model.BundleZ] == a.seq {
			u.res.Undone++
		} else {
			u.res.skip(VoidStale)
		}
		return
	}
	if r.El.Locked {
		u.res.skip(VoidLocked)
		return
	}
	to := zTarget{where: "back"}
	if rec.After != "" {
		t := a.s.rows[rec.After]
		if !t.alive() || t.Inc != u.linked(rec.After, rec.AfterInc) || rec.After == rec.ID {
			u.res.skip(VoidGone)
			return
		}
		to = zTarget{where: "after", id: rec.After}
	}
	ord := a.s.order()
	inv := OrderRec{ID: rec.ID, Inc: r.Inc, PrevV: r.V[model.BundleZ], MyV: a.seq}
	if i := slices.Index(ord, rec.ID); i > 0 {
		inv.After, inv.AfterInc = ord[i-1], a.s.rows[ord[i-1]].Inc
	}
	a.placeAt([]string{rec.ID}, to)
	u.inv.Orders = append(u.inv.Orders, inv)
	u.lineage(rec.ID, r.Inc, model.BundleZ, rec.PrevV)
	u.res.Undone++
}

// entryBytes is the stored size of an undo entry.
func entryBytes(e *UndoEntry) int64 {
	b, _ := json.Marshal(e)
	return int64(len(b))
}

// UndoBytes is the stored size of every undo entry of the board.
func (s *State) UndoBytes() int64 {
	var n int64
	for _, st := range s.undo {
		for _, e := range append(slices.Clone(st.undo), st.redo...) {
			n += entryBytes(e)
		}
	}
	return n
}

// TrimUndo drops undo entries until the board's undo storage is at most
// limit bytes: the oldest entries of the least recently active session go
// first (4.10 "Retention").
func (s *State) TrimUndo(limit int64) Changed {
	m := s.Savepoint()
	sp := s.sps[m]
	total := s.UndoBytes()
	for total > limit {
		victim := ""
		for _, id := range slices.Sorted(maps.Keys(s.undo)) {
			st := s.undo[id]
			if len(st.undo)+len(st.redo) == 0 {
				continue
			}
			if victim == "" || s.lastAt(id) < s.lastAt(victim) {
				victim = id
			}
		}
		if victim == "" {
			break
		}
		st := s.editStacks(victim)
		stack := &st.undo
		if len(st.undo) == 0 || (len(st.redo) > 0 && st.redo[0].Idx < st.undo[0].Idx) {
			stack = &st.redo
		}
		total -= entryBytes((*stack)[0])
		*stack = slices.Clone((*stack)[1:])
	}
	c := s.changedSince(sp)
	s.Release(m)
	return c
}

func (s *State) lastAt(session string) int64 {
	if se := s.sessions[session]; se != nil {
		return se.LastAt
	}
	return 0
}
