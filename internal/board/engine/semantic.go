package engine

import (
	"cmp"
	"maps"
	"slices"

	"github.com/gosuda/flats/internal/board/model"
)

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// setRel changes a membership bundle of an alive element when it differs.
func (a *applier) setRel(id, g, val string) {
	cur := a.s.rows[id]
	if cur.El.Bundle(g) == val {
		return
	}
	r := cur.clone()
	a.setBundle(r, g, val)
	a.put(r)
}

// reparent applies {op:"reparent", ids, inc_of, frame, frame_inc}.
func (a *applier) reparent(op Op) any {
	frame, _ := decodeFrame(op.Frame)
	res := &MultiResult{}
	if frame != "" {
		f, _ := a.resolve(frame, op.FrameInc)
		if f == nil || f.El.Type != model.TypeFrame {
			for _, id := range dedupe(op.IDs) {
				res.fail(id, VoidGone)
			}
			return res
		}
	}
	var moving []string
	for _, id := range dedupe(op.IDs) {
		r, reason := a.resolve(id, op.IncOf[id])
		switch {
		case r == nil:
			res.fail(id, reason)
		case r.El.Locked:
			res.fail(id, VoidLocked)
		case frame != "" && r.El.Type == model.TypeFrame:
			res.fail(id, VoidRef)
		default:
			res.OK = append(res.OK, id)
			moving = append(moving, id)
			if r.El.Group != "" {
				moving = append(moving, slices.Sorted(maps.Keys(a.s.groups[r.El.Group]))...)
			}
		}
	}
	for _, id := range dedupe(moving) {
		a.setRel(id, model.BundleFrame, frame)
	}
	return res
}

// group applies {op:"group", ids, inc_of, group}: all or nothing (rule 34).
func (a *applier) group(op Op) any {
	ids := dedupe(op.IDs)
	res := &MultiResult{}
	reasons := map[string]string{}
	frame, first := "", true
	for _, id := range ids {
		r, reason := a.resolve(id, op.IncOf[id])
		switch {
		case r == nil:
			reasons[id] = reason
		case r.El.Locked:
			reasons[id] = VoidLocked
		case r.El.Group != "" || r.El.Type == model.TypeFrame:
			reasons[id] = VoidInvalidGroup
		case !first && r.El.Frame != frame:
			reasons[id] = VoidInvalidGroup
		default:
			frame, first = r.El.Frame, false
		}
	}
	if len(reasons) > 0 || len(ids) < 2 || a.groupUsed(op.Group) {
		for _, id := range ids {
			res.fail(id, cmp.Or(reasons[id], VoidInvalidGroup))
		}
		return res
	}
	for _, id := range ids {
		a.setRel(id, model.BundleGroup, op.Group)
	}
	a.useGroup(op.Group)
	res.OK = ids
	return res
}

// usedGroups is the created-rows session under which the board records
// every group id it has used, with the seq that first used it. No session
// has an empty id, so the rows never expire. Ungroup carries no
// incarnation, so a group id must stay unavailable after ungroup, after
// its deleted members' rows are purged and after a reload (4.3).
const usedGroups = ""

// useGroup records group g as used.
func (a *applier) useGroup(g string) {
	if _, ok := a.s.created[createdKey{usedGroups, g}]; g != "" && !ok {
		a.s.setCreated(createdKey{usedGroups, g}, a.seq)
	}
}

// groupUsed reports whether group g was ever used: recorded, or named by
// any row, alive or deleted.
func (a *applier) groupUsed(g string) bool {
	if _, ok := a.s.created[createdKey{usedGroups, g}]; ok || len(a.s.groups[g]) > 0 {
		return true
	}
	for _, r := range a.s.rows {
		if r.El.Group == g {
			return true
		}
	}
	return false
}

// ungroup applies {op:"ungroup", group}, including locked members.
func (a *applier) ungroup(op Op) any {
	members := slices.Sorted(maps.Keys(a.s.groups[op.Group]))
	if len(members) == 0 {
		return void(VoidGone)
	}
	for _, id := range members {
		a.setRel(id, model.BundleGroup, "")
	}
	return ResultOK
}

// deleteOp applies {op:"delete", ids, inc_of, if_v}.
func (a *applier) deleteOp(op Op) any {
	guard, _ := decodeIfV(op.IfV)
	res := &MultiResult{}
	var set []string
	for _, id := range dedupe(op.IDs) {
		r, reason := a.resolve(id, op.IncOf[id])
		switch {
		case r == nil:
			res.fail(id, reason)
		case r.El.Locked:
			res.fail(id, VoidLocked)
		case !versionsMatch(r, guard[id]):
			res.fail(id, VoidStale)
		default:
			set = append(set, id)
		}
	}
	a.deleteSet(set)
	res.OK = set
	return res
}

func versionsMatch(r *Row, want map[string]int64) bool {
	for g, v := range want {
		if r.V[g] != v {
			return false
		}
	}
	return true
}

// deleteFrameContents applies {op:"delete_frame_contents", frame, inc}.
// Locked members stay; only their membership ends.
func (a *applier) deleteFrameContents(op Op) any {
	frame, _ := decodeFrame(op.Frame)
	res := &MultiResult{}
	f, reason := a.resolve(frame, op.Inc)
	switch {
	case f == nil:
		res.fail(frame, reason)
		return res
	case f.El.Type != model.TypeFrame:
		res.fail(frame, VoidRef)
		return res
	case f.El.Locked:
		res.fail(frame, VoidLocked)
		return res
	}
	set := []string{frame}
	for _, id := range slices.Sorted(maps.Keys(a.s.members[frame])) {
		if a.s.rows[id].El.Locked {
			res.Locked++
		} else {
			set = append(set, id)
		}
	}
	a.deleteSet(set)
	res.OK = set
	return res
}

// deleteSet deletes alive elements (4.3 delete): it captures every pre_rel
// first, then marks the rows deleted, ends the membership of members left
// outside the set and frees connector ends attached to deleted targets.
func (a *applier) deleteSet(ids []string) { a.deleteSetAt(ids, nil) }

// deleteSetAt is deleteSet with the end_last of some connectors in ids
// already taken: a reset takes them from the board before it installs the
// new content (4.12).
func (a *applier) deleteSetAt(ids []string, last map[string][2][2]float64) {
	if len(ids) == 0 {
		return
	}
	ids = slices.Sorted(slices.Values(ids))
	pre := make(map[string]*PreRel, len(ids))
	for _, id := range ids {
		pre[id] = a.preRel(id)
	}
	for _, id := range ids {
		r := a.s.rows[id].clone()
		if l, ok := last[id]; ok {
			r.EndLast = l
		} else if r.El.Type == model.TypeConnector {
			for k, end := range r.El.Ends {
				r.EndLast[k] = a.endWorld(end)
			}
		}
		r.Deleted, r.DelSeq, r.PreRel = true, a.seq, pre[id]
		a.put(r)
	}
	for _, id := range ids {
		row := a.s.rows[id]
		if row.El.Type == model.TypeFrame {
			for _, m := range slices.Sorted(maps.Keys(a.s.members[id])) {
				a.setRel(m, model.BundleFrame, "")
			}
		}
		for _, ce := range sortedEnds(a.s.attached[id]) {
			c := a.s.rows[ce.conn].clone()
			a.setBundle(c, model.EndBundle(ce.end), model.End{Pt: a.endWorld(c.El.Ends[ce.end])})
			a.put(c)
		}
	}
}

func sortedEnds(m map[connEnd]struct{}) []connEnd {
	out := slices.Collect(maps.Keys(m))
	slices.SortFunc(out, func(x, y connEnd) int { return cmp.Or(cmp.Compare(x.conn, y.conn), x.end-y.end) })
	return out
}

// endWorld is the canvas coordinate of a connector end now, as a valid free
// end: an attached end's point is clamped to the coordinate range, which
// a target at the edge of the range can reach past. The target row keeps its
// geometry even when it is deleted.
func (a *applier) endWorld(end model.End) [2]float64 {
	if end.El == "" {
		return end.Pt
	}
	return model.ClampPoint(model.EndpointWorld(a.s.rows[end.El].El.Geom, end.At))
}

// preRel captures the relations of id from the start of this change (4.1).
func (a *applier) preRel(id string) *PreRel {
	b := a.batchRow(id)
	if !b.alive() {
		b = a.s.rows[id] // created or revived during this change
	}
	p := &PreRel{Frame: b.El.Frame, Group: b.El.Group}
	if f := a.batchRow(b.El.Frame); b.El.Frame != "" && f != nil {
		p.FrameInc = f.Inc
	}
	if b.El.Type == model.TypeConnector {
		p.Ends = []EndRel{{b.El.Ends[0], b.EndInc[0]}, {b.El.Ends[1], b.EndInc[1]}}
	}
	ends := maps.Clone(a.s.attached[id])
	for _, ce := range a.sp.startAttach[id] {
		if ends == nil {
			ends = map[connEnd]struct{}{}
		}
		ends[ce] = struct{}{}
	}
	for _, ce := range sortedEnds(ends) {
		c := a.batchRow(ce.conn)
		if c.alive() && c.El.Type == model.TypeConnector && c.El.Ends[ce.end].El == id {
			p.Attached = append(p.Attached, AttachRel{ce.conn, c.Inc, ce.end, c.El.Ends[ce.end].At})
		}
	}
	if b.El.Type == model.TypeFrame {
		members := slices.Collect(maps.Keys(a.s.members[id]))
		members = append(members, a.sp.startMembers[id]...)
		for _, m := range dedupe(slices.Sorted(slices.Values(members))) {
			if r := a.batchRow(m); r.alive() && r.El.Frame == id {
				p.Members = append(p.Members, MemberRel{m, r.Inc})
			}
		}
	}
	return p
}

// reviveSet revives deleted elements in two phases (4.1): every element of
// the set first gets its new incarnation, then relations come back from
// pre_rel, and last the group invariant is checked.
func (a *applier) reviveSet(ids []string, ifDel int64) *MultiResult {
	res, fix := a.revivePhases(ids, ifDel)
	fix.apply(a)
	return res
}

// groupFix is the last step of a revive, the group invariant check. An
// undo runs it after its relation step (4.10), so a revived member whose
// mate's frame that step restores keeps its group.
type groupFix struct {
	revived  []string
	restored []restoredMember
}

// restoredMember is an outside frame member a revive put back into the
// revived frame, with its row from before.
type restoredMember struct {
	old   *Row
	frame string
}

func (f groupFix) apply(a *applier) { a.fixGroups(f.revived, f.restored) }

// revivePhases runs both revive phases but leaves the group invariant
// check to the caller.
func (a *applier) revivePhases(ids []string, ifDel int64) (*MultiResult, groupFix) {
	res := &MultiResult{}
	inS := map[string]bool{}
	var set []string
	for _, id := range dedupe(ids) {
		r := a.s.rows[id]
		switch {
		case r == nil || id == model.BoardID:
			res.fail(id, VoidGone)
		case !r.Deleted || r.DelSeq != ifDel:
			res.fail(id, VoidStale)
		default:
			set = append(set, id)
			inS[id] = true
		}
	}
	slices.Sort(set)
	atDel := make(map[string]*Row, len(set))
	for _, id := range set {
		old := a.s.rows[id]
		atDel[id] = old
		r := old.clone()
		r.Deleted, r.DelSeq, r.PreRel, r.Inc = false, 0, nil, a.seq
		r.El.Frame, r.El.Group = "", ""
		if r.El.Type == model.TypeConnector {
			r.El.Ends = [2]model.End{{Pt: model.ClampPoint(old.EndLast[0])}, {Pt: model.ClampPoint(old.EndLast[1])}}
			r.EndInc = [2]int64{}
		}
		a.put(r)
	}
	for _, id := range set {
		a.restoreOwnRelations(id, atDel[id], inS)
	}
	var restored []restoredMember
	for _, id := range set {
		restored = append(restored, a.restoreOutside(id, atDel[id].PreRel, inS, ifDel)...)
	}
	res.OK = set
	return res, groupFix{set, restored}
}

// restoreOwnRelations sets a revived element's frame, group and ends from
// its pre_rel. Its versions change only where the value differs from the
// one it had when deleted.
func (a *applier) restoreOwnRelations(id string, old *Row, inS map[string]bool) {
	pr := old.PreRel
	if pr == nil {
		pr = &PreRel{}
	}
	r := a.s.rows[id].clone()
	set := func(g string, val any) {
		r.El.SetBundle(g, val)
		if !model.BundleEqual(g, old.El.Bundle(g), val) {
			r.V[g] = a.seq
		}
	}
	frame := ""
	if pr.Frame != "" {
		f := a.s.rows[pr.Frame]
		if inS[pr.Frame] || (f.alive() && f.Inc == pr.FrameInc && f.El.Type == model.TypeFrame) {
			frame = pr.Frame
		}
	}
	set(model.BundleFrame, frame)
	set(model.BundleGroup, pr.Group)
	if r.El.Type == model.TypeConnector {
		for k := range 2 {
			end := model.End{Pt: model.ClampPoint(old.EndLast[k])}
			if k < len(pr.Ends) {
				er := pr.Ends[k]
				t := a.s.rows[er.End.El]
				switch {
				case er.End.El == "":
					end = er.End
				case inS[er.End.El] || (t.alive() && t.Inc == er.Inc && t.El.Type != model.TypeConnector):
					end = er.End
					r.EndInc[k] = t.Inc
				}
			}
			set(model.EndBundle(k), end)
		}
	}
	a.put(r)
}

// restoreOutside changes elements outside the revived set back: connector
// ends the deletion detached and frame members it released, each only when
// that element is alive, unlocked, the same incarnation and its bundle
// version is still the deletion seq. It returns the restored members with
// their rows from before the restore.
func (a *applier) restoreOutside(id string, pr *PreRel, inS map[string]bool, delSeq int64) []restoredMember {
	if pr == nil {
		return nil
	}
	for _, at := range pr.Attached {
		c := a.s.rows[at.Conn]
		g := model.EndBundle(at.End)
		if inS[at.Conn] || !c.alive() || c.El.Locked || c.Inc != at.ConnInc || c.V[g] != delSeq {
			continue
		}
		r := c.clone()
		a.setBundle(r, g, model.End{El: id, At: at.At})
		a.put(r)
	}
	var restored []restoredMember
	for _, m := range pr.Members {
		r := a.s.rows[m.ID]
		if inS[m.ID] || !r.alive() || r.El.Locked || r.Inc != m.Inc || r.V[model.BundleFrame] != delSeq || r.El.Frame != "" {
			continue
		}
		a.setRel(m.ID, model.BundleFrame, id)
		restored = append(restored, restoredMember{r, id})
	}
	return restored
}

// fixGroups restores the group invariant after a revive: in a group whose
// members ended up in different frames, outside members put back into a
// frame are reverted first, then revived elements leave the group. Groups
// are judged by their current members, so an undo that ran its relation
// step in between is judged with those relations.
func (a *applier) fixGroups(revived []string, restored []restoredMember) {
	groups := map[string]bool{}
	for _, id := range revived {
		if r := a.s.rows[id]; r.alive() && r.El.Group != "" {
			groups[r.El.Group] = true
		}
	}
	for _, m := range restored {
		if r := a.s.rows[m.old.El.ID]; r.alive() && r.El.Group != "" {
			groups[r.El.Group] = true
		}
	}
	for _, g := range slices.Sorted(maps.Keys(groups)) {
		if a.groupConsistent(g) {
			continue
		}
		for _, m := range restored {
			// Only the membership the revive put back is reverted; a
			// later change of the same row (an undo's relation step) stays.
			r := a.s.rows[m.old.El.ID]
			if r.alive() && r.Inc == m.old.Inc && r.El.Group == g && r.El.Frame == m.frame && r.V[model.BundleFrame] == a.seq {
				c := r.clone()
				c.El.Frame, c.V[model.BundleFrame] = m.old.El.Frame, m.old.V[model.BundleFrame]
				a.put(c)
			}
		}
		if a.groupConsistent(g) {
			continue
		}
		for _, id := range revived {
			if r := a.s.rows[id]; r.alive() && r.El.Group == g {
				a.setRel(id, model.BundleGroup, "")
			}
		}
	}
}

func (a *applier) groupConsistent(g string) bool {
	frame, first := "", true
	for id := range a.s.groups[g] {
		f := a.s.rows[id].El.Frame
		if !first && f != frame {
			return false
		}
		frame, first = f, false
	}
	return true
}

// zorder applies {op:"zorder", ids, inc_of, to, if_v}.
func (a *applier) zorder(op Op) any {
	to, _ := decodeTo(op.To)
	guard, _ := decodeIfV(op.IfV)
	res := &MultiResult{}
	var moving []string
	for _, id := range dedupe(op.IDs) {
		r, reason := a.resolve(id, op.IncOf[id])
		switch {
		case r == nil:
			res.fail(id, reason)
		case r.El.Locked:
			res.fail(id, VoidLocked)
		case !versionsMatch(r, guard[id]):
			res.fail(id, VoidStale)
		default:
			moving = append(moving, id)
		}
	}
	if len(moving) == 0 {
		return res
	}
	if to.id != "" {
		anchor, _ := a.resolve(to.id, to.inc)
		if anchor == nil || slices.Contains(moving, to.id) {
			for _, id := range moving {
				res.fail(id, VoidGone)
			}
			return res
		}
	}
	a.placeAt(moving, to)
	res.OK = moving
	return res
}

// placeAt moves elements, keeping their relative order, to the front, the
// back, or just after or before an anchor.
func (a *applier) placeAt(moving []string, to zTarget) {
	a.s.sortByZ(moving)
	in := map[string]bool{}
	for _, id := range moving {
		in[id] = true
	}
	var lo, hi string
	for attempt := 0; ; attempt++ {
		rest := slices.DeleteFunc(a.s.order(), func(id string) bool { return in[id] })
		idx := 0
		switch to.where {
		case "front":
			idx = len(rest)
		case "after":
			idx = slices.Index(rest, to.id) + 1
		case "before":
			idx = slices.Index(rest, to.id)
		}
		lo, hi = "", ""
		if idx > 0 {
			lo = a.s.rows[rest[idx-1]].El.Z
		}
		if idx < len(rest) {
			hi = a.s.rows[rest[idx]].El.Z
		}
		if hi == "" || lo < hi || attempt > 0 {
			break
		}
		a.rekey() // equal neighbor keys leave no room between them
	}
	for i, k := range spread(lo, hi, len(moving)) {
		r := a.s.rows[moving[i]].clone()
		a.setBundle(r, model.BundleZ, k)
		a.put(r)
	}
}

// spread returns n increasing keys between lo and hi by bisection, so the
// key length grows with log(n) rather than n.
func spread(lo, hi string, n int) []string {
	if n == 0 {
		return nil
	}
	mid, _ := model.KeyBetween(lo, hi)
	left := spread(lo, mid, (n-1)/2)
	right := spread(mid, hi, n-1-(n-1)/2)
	return append(append(left, mid), right...)
}

// meta applies {op:"meta", g, val, if_v} to the board metadata.
func (a *applier) meta(op Op) any {
	cur := a.s.rows[model.BoardID]
	val, _ := model.DecodeBundle(model.TypeBoard, op.G, op.Val)
	if v, ok, _ := decodeV(op.IfV); ok && cur.V[op.G] != v {
		return void(VoidStale)
	}
	r := cur.clone()
	a.setBundle(r, op.G, val)
	a.put(r)
	return ResultOK
}
