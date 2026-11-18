package sheet

import (
	"cmp"
	"container/heap"
	"slices"
	"unsafe"
)

// writeMark is the other-writer mark of a row, a column or a tab (technical
// design, section 14): who changed content or a format in it last, and
// when another writer did so last. From the two, any session's undo can
// tell whether someone else has written there since the action it takes
// back, however often the row was deleted and brought back in between,
// because the mark is saved with a deleted row and returns with it.
//
// A mark is kept for every row and column that was written into, as long
// as an undo entry of some session is older than its stamps; the
// maintenance tick drops the others (pruneMarks). It stays where it is when
// its row is deleted: ids are never used again, an undo of the deletion
// finds the mark as it was, and the undo of an action that created several
// rows still sees a write into one of them that is deleted now.
type writeMark struct {
	by    uint32 // the writer of the latest change; 0 for an agent
	last  uint64 // the sequence of that change
	other uint64 // the sequence of the latest change by another writer than by
}

// note records a change by a writer and reports whether the mark changed.
func (m *writeMark) note(by uint32, seq uint64) bool {
	switch {
	case m.by != by:
		m.by, m.last, m.other = by, seq, m.last
	case m.last != seq:
		m.last = seq
	default:
		return false
	}
	return true
}

// otherThan returns the sequence of the latest change by another writer
// than the session.
func (m writeMark) otherThan(session uint32) uint64 {
	if m.by != session {
		return m.last
	}
	return m.other
}

type savedSort struct {
	row   RowID
	stamp sortStamp
}

// mark records the writer's change of content or a format in the marks of
// the cell's row, column and tab.
func (x *txn) mark(t *Tab, row RowID, col ColID) {
	x.markAxis(&t.marks, &t.marksMost, uint64(row))
	x.markAxis(&t.colMarks, &t.colMarksMost, uint64(col))
	if old := t.mark; t.mark.note(x.by, x.seq) {
		x.j.push(func() { t.mark = old })
	}
}

func (x *txn) markAxis(marks *map[uint64]writeMark, most *int, id uint64) {
	old, had := (*marks)[id]
	m := old
	if !m.note(x.by, x.seq) {
		return
	}
	if *marks == nil {
		*marks = map[uint64]writeMark{}
	}
	(*marks)[id] = m
	*most = max(*most, len(*marks))
	// The revert works on the map the tab holds at that time. Admission
	// can make it again for the marks it keeps (dropMarks), and a deleted
	// tab gets maps of its own size (compactSaved). Both keep this mark,
	// which is newer than any mark they drop.
	x.j.push(func() {
		if had {
			if *marks == nil {
				*marks = map[uint64]writeMark{}
			}
			(*marks)[id] = old
			*most = max(*most, len(*marks))
			return
		}
		delete(*marks, id)
		// A map keeps the room of the marks it lost. One that the revert
		// leaves empty held only marks of the reverted action, and goes
		// with them; maintenance makes the others again (dropMarks).
		if len(*marks) == 0 {
			*marks, *most = nil, 0
		}
	})
}

// takeSorts moves the last-sort stamps of the rows a step deletes into the
// step. They return with the rows (giveSorts).
func (x *txn) takeSorts(t *Tab, s *undoStep) *WriteError {
	if !s.rows || len(t.lastSort) == 0 {
		return nil
	}
	// The journal puts back what the loop took, also when the deadline
	// ends it before the last id. Each id is a step of the deletion's
	// bookkeeping.
	x.j.push(func() {
		for _, saved := range s.sorted {
			t.lastSort[saved.row] = saved.stamp
		}
	})
	tk := ticker{more: x.pace}
	for _, id := range s.ids {
		if !tk.tick() {
			return calcTimeout()
		}
		if stamp, ok := t.lastSort[RowID(id)]; ok {
			s.sorted = append(s.sorted, savedSort{RowID(id), stamp})
			t.dropSort(RowID(id))
		}
	}
	x.keep(int(unsafe.Sizeof(savedSort{})) * cap(s.sorted))
	if !tk.done() {
		return calcTimeout()
	}
	return nil
}

// giveSorts puts the last-sort stamps a deletion took back on the rows that
// return.
func (x *txn) giveSorts(t *Tab, s *undoStep) *WriteError {
	if len(s.sorted) == 0 {
		return nil
	}
	if t.lastSort == nil {
		t.lastSort = map[RowID]sortStamp{}
	}
	// The journal takes the stamps away again, also those of a loop the
	// deadline ended. Each stamp is a unit.
	sorted := s.sorted
	x.j.push(func() {
		for _, saved := range sorted {
			t.dropSort(saved.row)
		}
	})
	tk := ticker{more: x.pace, cost: 64}
	for _, saved := range s.sorted {
		if !tk.tick() {
			return calcTimeout()
		}
		t.lastSort[saved.row] = saved.stamp
	}
	if !tk.done() {
		return calcTimeout()
	}
	return nil
}

// writtenSince reports whether another writer than the session changed
// content or a format in what the step created or brought back, after the
// action of the step's entry at seq.
//
// Looking up the mark of an id is a unit, and the deadline ends the loop:
// ok is then false.
func (s *undoStep) writtenSince(t *Tab, session uint32, seq uint64, more pacer) (written, ok bool) {
	if s.kind == stepTabAdded {
		return t.mark.otherThan(session) > seq, true
	}
	marks := t.colMarks
	if s.rows {
		marks = t.marks
	}
	if len(marks) == 0 {
		return false, true
	}
	tk := ticker{more: more, cost: 64}
	for _, id := range s.ids {
		if !tk.tick() {
			return false, false
		}
		if m, ok := marks[id]; ok && m.otherThan(session) > seq {
			return true, tk.done()
		}
	}
	return false, tk.done()
}

// markBytes returns the memory the write marks of the sheet's tabs take:
// each map at the most marks it held since it was made. They count in the
// undo memory of the sheet (fitSheet).
func (wb *Workbook) markBytes() int64 {
	var n int64
	for _, t := range wb.tabs {
		if t.marks != nil {
			n += mapRoom[uint64, writeMark](max(t.marksMost, len(t.marks)))
		}
		if t.colMarks != nil {
			n += mapRoom[uint64, writeMark](max(t.colMarksMost, len(t.colMarks)))
		}
	}
	return n
}

// oldestUndo returns the sequence of the oldest entry of any session that
// can still ask about write marks, and the entry: one that can still be
// undone or redone, with an insert or a new tab among its steps. The
// caller holds the sessions' lock.
func (in *Instance) oldestUndo() (uint64, *undoEntry) {
	oldest, found := ^uint64(0), (*undoEntry)(nil)
	for _, s := range in.sessions {
		for _, stack := range [2][]*undoEntry{s.undo.undo, s.undo.redo} {
			for _, e := range stack {
				in.scratch.count(1)
				if e.asks && !e.lost {
					if e.seq < oldest {
						oldest, found = e.seq, e
					}
					break
				}
			}
		}
	}
	return oldest, found
}

// pruneMarks drops the marks that no undo entry can ask about any more:
// an entry asks for changes after its own sequence, and only about what
// an insert or a new tab of it created, so a mark whose stamps are older
// than every such entry answers no question. It runs with the maintenance
// tick, once the marks have doubled since it last ran or the oldest such
// entry is a newer one. Marks count here at the room their maps keep, so
// a map that a revert left larger than its marks is made again as well.
func (in *Instance) pruneMarks() {
	in.smu.Lock()
	oldest, _ := in.oldestUndo()
	in.smu.Unlock()
	total := 0
	for _, t := range in.wb.tabs {
		total += max(t.marksMost, len(t.marks)) + max(t.colMarksMost, len(t.colMarks))
	}
	if total <= in.scratch.marksAt && oldest <= in.scratch.marksBelow {
		return
	}
	in.dropMarks(oldest)
}

// dropMarks drops the marks whose stamps are all at or below oldest. A map
// that lost marks, or keeps the room of more marks than it holds, is made
// again for those it keeps, and one that keeps none goes.
//
// It looks at whether the instance stopped every paceEvery marks, and
// reports false when it did. A map it was making again then stays as it
// was, with marks that answer no question, which count as before; the maps
// made again before stay so.
func (in *Instance) dropMarks(oldest uint64) bool {
	tk := ticker{more: func(int) bool { return in.ctx.Err() == nil }}
	total := 0
	for _, t := range in.wb.tabs {
		for _, pair := range [2]struct {
			marks *map[uint64]writeMark
			most  *int
		}{{&t.marks, &t.marksMost}, {&t.colMarks, &t.colMarksMost}} {
			marks, kept := *pair.marks, 0
			for _, m := range marks {
				in.scratch.count(1)
				if !tk.tick() {
					return false
				}
				if m.last > oldest || m.other > oldest {
					kept++
				}
			}
			if marks != nil && kept < max(len(marks), *pair.most) {
				var fresh map[uint64]writeMark
				if kept > 0 {
					fresh = make(map[uint64]writeMark, kept)
					for id, m := range marks {
						in.scratch.count(1)
						if !tk.tick() {
							return false
						}
						if m.last > oldest || m.other > oldest {
							fresh[id] = m
						}
					}
				}
				*pair.marks, *pair.most = fresh, kept
			}
			total += kept
		}
	}
	in.scratch.marksAt, in.scratch.marksBelow = 2*total+1024, oldest
	return true
}

// fitSheet keeps what undo holds in memory for the sheet inside its bound:
// the payloads in memory, what payloads in scratch files leave there, the
// stand-in notes with what an unfinished sweep holds beside them, and the
// write marks. It runs after the last step of a committed change that can
// make one of them larger (the structural edits the payloads follow, the
// entry that joins its stack, the marks the change wrote), after the
// revert of a change that did not commit, and at the end of the tick,
// after the notes were swept.
//
// It runs between two changes, so an entry that was admitted and reached
// no stack belongs to a change that did not commit, and is forgotten
// first. A session that holds more than its share, which following can
// leave beside the entry that joined after it, is brought back inside it
// as squeeze does when an action commits. Then the sheet is brought
// inside its bound as squeezeSheet does, with payloads that cannot be
// written expiring. What is still over the bound then is the write marks:
// the oldest entry that can still ask about marks expires, whichever
// session owns it, and the marks only it could ask about go with it.
func (in *Instance) fitSheet() {
	p := in.undoPool()
	p.forgetUnplaced()
	in.smu.Lock()
	defer in.smu.Unlock()
	for _, s := range in.sessions {
		if s.undo.resident() > p.sessionMax {
			p.squeeze(&s.undo, nil, 0, p.orExpire)
		}
	}
	f := &markFit{in: in}
	defer f.finish()
	in.squeezeSheetWith(f, nil, nil, 0, p.orExpire)
	for p.resident+p.notes.bytes+f.bytes() > p.instanceMax {
		if !f.started {
			f.begin()
			continue
		}
		e := f.front()
		if e == nil {
			break
		}
		e.expire(errUndoMarks)
		f.follow()
	}
}

// markFit follows, through one fitting of the sheet's undo memory, the
// oldest entry that can ask about write marks as entries expire, and what
// the marks count once those that no entry can ask about go. The maps are
// made again twice at most: when the fit begins, for the oldest entry
// then, and when it ends, for the oldest it reached (dropMarks). In
// between, one look at every mark tells how many each newer oldest lets
// go and what the maps would count without them. So the work of a fit is
// in proportion to the marks and the entries, however many expire.
//
// It counts on mapRoom, which is a constant and a share of each mark for
// a map of 8 marks or more: a map's room is the share of its marks and a
// part that its 8 newest marks decide.
type markFit struct {
	in *Instance
	// fronts holds the first entry that can ask of each stack, as a heap
	// by sequence. One that expired is passed over when it comes to the
	// top (front).
	fronts  markFronts
	started bool
	// dropped is the oldest the maps were made again for, and below the
	// oldest the fit has reached.
	dropped, below uint64
	// counting is set once below passed dropped, and stopped when the
	// instance stopped while the marks were dropped or counted.
	counting, stopped bool
	c                 markCounts
}

type markFront struct {
	stack []*undoEntry
	i     int
}

type markFronts []markFront

func (h markFronts) Len() int           { return len(h) }
func (h markFronts) Less(a, b int) bool { return h[a].stack[h[a].i].seq < h[b].stack[h[b].i].seq }
func (h markFronts) Swap(a, b int)      { h[a], h[b] = h[b], h[a] }
func (h *markFronts) Push(x any)        { *h = append(*h, x.(markFront)) }
func (h *markFronts) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// asking reports whether an entry can ask about marks (oldestUndo).
func (e *undoEntry) asking() bool { return e.asks && !e.lost }

// begin finds the first entry that can ask of each stack, and drops the
// marks no entry can ask about.
func (f *markFit) begin() {
	in := f.in
	for _, s := range in.sessions {
		for _, stack := range [2][]*undoEntry{s.undo.undo, s.undo.redo} {
			for i, e := range stack {
				in.scratch.count(1)
				if e.asking() {
					f.fronts = append(f.fronts, markFront{stack, i})
					break
				}
			}
		}
	}
	heap.Init(&f.fronts)
	f.started = true
	f.dropped = f.oldest()
	f.below = f.dropped
	if !in.dropMarks(f.dropped) {
		f.stopped = true
	}
}

// front returns the oldest entry that can ask about marks, nil when none
// is left. Each entry is passed over once.
func (f *markFit) front() *undoEntry {
	for len(f.fronts) > 0 {
		top := &f.fronts[0]
		if e := top.stack[top.i]; e.asking() {
			return e
		}
		for top.i++; top.i < len(top.stack) && !top.stack[top.i].asking(); top.i++ {
			f.in.scratch.count(1)
		}
		if top.i == len(top.stack) {
			heap.Pop(&f.fronts)
		} else {
			heap.Fix(&f.fronts, 0)
		}
	}
	return nil
}

// oldest returns the sequence of front, as oldestUndo does.
func (f *markFit) oldest() uint64 {
	if e := f.front(); e != nil {
		return e.seq
	}
	return ^uint64(0)
}

// follow moves below to the oldest entry that can ask now.
func (f *markFit) follow() {
	if !f.started || f.stopped {
		return
	}
	o := f.oldest()
	if o <= f.below {
		return
	}
	f.below = o
	if !f.counting {
		f.counting = true
		if !f.c.count(f.in, o) {
			f.stopped = true
		}
		return
	}
	f.c.advance(o)
}

// bytes returns what the marks count once those at or below the oldest
// entry that can ask went. It is 0 when the instance stopped the count.
func (f *markFit) bytes() int64 {
	switch {
	case f.stopped:
		return 0
	case f.counting:
		return f.c.per*f.c.kept + f.c.extra
	}
	return f.in.wb.markBytes()
}

// finish drops the marks no entry it reached can ask about. After the
// instance stopped the fit, it looks at no mark again.
func (f *markFit) finish() {
	if f.started && !f.stopped && f.below > f.dropped {
		f.in.dropMarks(f.below)
	}
}

// markCounts counts the marks that each sequence an oldest entry can take
// lets go: a mark goes at the first of them at or above its newest stamp.
type markCounts struct {
	seqs  []uint64 // of the entries that can ask, in order, and ^0
	at    int      // the index in seqs of the oldest reached
	frees []int32  // the marks seqs[i] lets go
	kept  int64    // the marks kept at seqs[at]
	// per is what a map counts for each mark, and extra what the maps count
	// beside that: room(c) for each map of c kept marks, with c up to 8.
	per, extra int64
	maps       []markMapTop
	events     []markEvent // the sequences that change a map's room, in order
	ev         int
}

// markMapTop holds the indexes in seqs at which the 8 newest marks of a
// map go, and how many of them are kept.
type markMapTop struct {
	at   [8]int32
	n    int
	kept int
}

type markEvent struct{ at, m int32 }

// room is what a map of kept marks counts beside per for each: 0 for
// none, which goes.
func (c *markCounts) room(kept int) int64 {
	if kept == 0 {
		return 0
	}
	return mapRoom[uint64, writeMark](kept) - c.per*int64(kept)
}

// count looks at every mark once, for the oldest o. It reports false when
// the instance stopped first.
func (c *markCounts) count(in *Instance, o uint64) bool {
	for _, s := range in.sessions {
		for _, stack := range [2][]*undoEntry{s.undo.undo, s.undo.redo} {
			in.scratch.count(len(stack))
			for _, e := range stack {
				if e.asking() {
					c.seqs = append(c.seqs, e.seq)
				}
			}
		}
	}
	slices.Sort(c.seqs)
	c.seqs = append(slices.Compact(c.seqs), ^uint64(0))
	c.at, _ = slices.BinarySearch(c.seqs, o)
	c.frees = make([]int32, len(c.seqs))
	c.per = mapRoom[uint64, writeMark](9) - mapRoom[uint64, writeMark](8)
	tk := ticker{more: func(int) bool { return in.ctx.Err() == nil }}
	for _, t := range in.wb.tabs {
		for _, marks := range [2]map[uint64]writeMark{t.marks, t.colMarks} {
			if len(marks) == 0 {
				continue
			}
			var top markMapTop
			for _, m := range marks {
				in.scratch.count(1)
				if !tk.tick() {
					return false
				}
				at, _ := slices.BinarySearch(c.seqs, max(m.last, m.other))
				if at <= c.at {
					continue
				}
				c.frees[at]++
				c.kept++
				top.add(int32(at))
			}
			if top.n == 0 {
				continue
			}
			top.kept = top.n
			c.extra += c.room(top.kept)
			for _, at := range top.at[:top.n] {
				c.events = append(c.events, markEvent{at, int32(len(c.maps))})
			}
			c.maps = append(c.maps, top)
		}
	}
	slices.SortFunc(c.events, func(a, b markEvent) int { return cmp.Compare(a.at, b.at) })
	return true
}

// add keeps at among the 8 largest indexes of the map.
func (top *markMapTop) add(at int32) {
	if top.n < len(top.at) {
		top.at[top.n] = at
		top.n++
		return
	}
	low := 0
	for i := range top.at {
		if top.at[i] < top.at[low] {
			low = i
		}
	}
	if at > top.at[low] {
		top.at[low] = at
	}
}

// advance moves the oldest reached on to o, a newer sequence of seqs.
func (c *markCounts) advance(o uint64) {
	j, _ := slices.BinarySearch(c.seqs, o)
	for i := c.at + 1; i <= j; i++ {
		c.kept -= int64(c.frees[i])
	}
	c.at = j
	for ; c.ev < len(c.events) && int(c.events[c.ev].at) <= j; c.ev++ {
		top := &c.maps[c.events[c.ev].m]
		kept := 0
		for _, at := range top.at[:top.n] {
			if int(at) > j {
				kept++
			}
		}
		c.extra += c.room(kept) - c.room(top.kept)
		top.kept = kept
	}
}

// count adds work for markWork.
func (p *undoScratch) count(n int) {
	if p.markWork != nil {
		p.markWork(n)
	}
}
