package sheet

import (
	"cmp"
	"math"
	"slices"
	"sort"
	"unsafe"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// A stand-in note says that the stamp an undo or a redo wrote into a group
// stands in for an entry of the session: the undo brought back the value
// that entry had written, under a new stamp, and the entry must still find
// it. An A to B to A history then guards against another writer's B and
// yet undoes the first A (technical design, section 14).
//
// A note is valid only while its group carries the note's stamp. Stamps
// never come back, so a note of a group that was written since matches
// nothing, and a group has at most one note that can match: the one of the
// stamp it carries now.
//
// The notes of one undo for one entry are a set. A set keeps its groups by
// tab, column and property, as runs of consecutive row ids: an undo over a
// range of cells takes a few bytes for each column of the range, whatever
// the number of rows. The bytes of every set count in the undo memory
// budgets of the session and of the sheet (undo_spill.go).

// noteRun is a run of consecutive row ids.
type noteRun struct {
	from RowID
	n    uint64
}

// noteCol holds the rows of one column and property that a set names. A
// group that is no cell leaves the ids it does not have at zero.
type noteCol struct {
	tab  TabID
	col  ColID
	prop int8
	runs []noteRun
}

type noteSet struct {
	owner *undoState
	// stamp is the stamp the undo wrote, and canon the sequence of the
	// entry it stands in for, on the undo or on the redo stack.
	stamp, canon uint64
	redo         bool
	// aux says that the set holds the groups that are no cells: widths,
	// settings, names, the tab order and the last sorts of rows.
	aux bool
	// at is the column the last lookup found (has).
	at   int32
	cols []noteCol
	n    int // notes
	// gone counts the notes whose group was written since. They match
	// nothing; the maintenance tick takes them out where that does not
	// make the set larger (sweepNotes). tried is what gone was when the
	// tick last found that it would.
	gone, tried int
	bytes       int64
	// seen is the sequence of the sheet when an aux set was checked last.
	seen uint64
}

// noteBudget counts the bytes of the notes of every session of a sheet,
// and of the set a sweep builds beside its set (noteSweep.held). What the
// sweep holds counts in the notes of the set's session as well, from the
// moment it is built, so that the limits of both see it.
type noteBudget struct {
	bytes int64
	// sweep is the set the tick builds again and has not finished.
	sweep *noteSweep
}

// hold makes w the sweep that goes on at the next tick, and counts what it
// holds now.
func (b *noteBudget) hold(w *noteSweep) {
	b.sweep = w
	n := w.held() - w.counted
	b.bytes += n
	w.set.owner.noteBytes += n
	w.counted += n
}

// end ends sweep w and stops counting what it holds.
func (b *noteBudget) end(w *noteSweep) {
	if b.sweep == w {
		b.sweep = nil
	}
	b.bytes -= w.counted
	w.set.owner.noteBytes -= w.counted
	w.counted = 0
}

// yield ends the unfinished sweep, of the session u or of any session when
// u is nil, and reports whether there was one. It runs where a limit would
// otherwise send payloads to scratch files or make entries expire, so that
// the sweep gives its room back first. Its set stays as it was, and the tick
// builds it again only after another share of its notes is gone
// (noteSet.due).
func (b *noteBudget) yield(u *undoState) bool {
	w := b.sweep
	if w == nil || u != nil && w.set.owner != u {
		return false
	}
	b.end(w)
	w.set.tried = w.set.gone
	return true
}

// noteSetBytes is the record of a set with its place in the session's
// index: the list of the sets of its stamp, two at most, and six slots of
// the index's table with their control bytes. The table keeps at most
// twice the stamps it holds (removeNotes), and has three slots for each
// of those just after it doubled.
var noteSetBytes = formula.HeapBytes(unsafe.Sizeof(noteSet{}), true) + 16 + 6*int64(unsafe.Sizeof(noteIndexSlot{})+1)

// noteIndexBytes is the smallest table of a session's index with the
// index's record, which the index keeps while it holds a set, beside the
// places of the sets. It counts with the notes of the session.
const noteIndexBytes = 512

type noteIndexSlot struct {
	stamp uint64
	sets  []*noteSet
}

const (
	noteColBytes = int64(unsafe.Sizeof(noteCol{}))
	noteRunBytes = int64(unsafe.Sizeof(noteRun{}))
)

func (c *noteCol) compare(tab TabID, col ColID, prop int8) int {
	if d := cmp.Compare(c.tab, tab); d != 0 {
		return d
	}
	if d := cmp.Compare(c.col, col); d != 0 {
		return d
	}
	return cmp.Compare(c.prop, prop)
}

// has reports whether the set holds a note of group g.
func (s *noteSet) has(g groupKey) bool {
	// An undo over a range asks column after column: the column of the
	// last lookup or the one after it is the likely one.
	i := int(s.at)
	if i >= len(s.cols) || s.cols[i].compare(g.tab, g.col, g.prop) != 0 {
		if i++; i >= len(s.cols) || s.cols[i].compare(g.tab, g.col, g.prop) != 0 {
			var ok bool
			i, ok = slices.BinarySearchFunc(s.cols, g, func(c noteCol, g groupKey) int { return c.compare(g.tab, g.col, g.prop) })
			if !ok {
				return false
			}
		}
	}
	s.at = int32(i)
	runs := s.cols[i].runs
	k := sort.Search(len(runs), func(k int) bool { return runs[k].from > g.row }) - 1
	return k >= 0 && uint64(g.row-runs[k].from) < runs[k].n
}

func (s *noteSet) measure() int64 {
	n := noteSetBytes + sliceBytes(cap(s.cols), unsafe.Sizeof(noteCol{}))
	for i := range s.cols {
		n += numsBytes(cap(s.cols[i].runs), unsafe.Sizeof(noteRun{}))
	}
	return n
}

// filter builds the set again with only the notes of the groups for which
// keep returns true, in storage of their size, and reports whether it did.
// A set never grows by it. Notes that go from the middle of a run leave
// the rows around them as runs of their own, and a set over a range can
// then take many times what it took. Where the notes that are left would
// take more than the set does now, filter changes nothing and stops
// looking as soon as that is known. The notes that should go match
// nothing while they stay, and the set keeps the bytes it was admitted
// with (install).
func (s *noteSet) filter(keep func(g groupKey) bool) bool {
	w := s.sweep()
	_, _, built := w.step(keep, -1)
	return built
}

// noteSweep builds a set again a stretch of its notes at a time, so that
// one tick of a sheet looks through a bounded number of notes however many
// one set holds (sweepNotes). The set itself stays as it is until the
// sweep has looked through all of it. The notes built so far are kept
// beside it, in at most limit bytes with the spare room of their lists,
// and count in the budgets of notes until the sweep ends (held).
type noteSweep struct {
	set *noteSet
	// The next note to look at: run of column col, k rows into it.
	col, run int
	k        uint64
	cols     []noteCol // the columns built so far
	runs     []noteRun // of column col, so far
	size     int64     // what the set built so far takes, without spare room
	built    int64     // the bytes of the runs of cols
	n        int       // its notes
	// gone counts the notes built so far whose group an action wrote
	// since they were looked at. They become the set's gone notes when
	// the sweep ends.
	gone int
	// limit is the most the sweep may keep, twice the set. room is the
	// most it may hold at any moment, counting the old and the new storage
	// of a list it grows; sweepNotes sets it to what the limits of memory
	// leave. counted is what the budgets count of it now (noteBudget.hold).
	limit, room, counted int64
}

func (s *noteSet) sweep() *noteSweep {
	return &noteSweep{set: s, size: noteSetBytes, limit: 2 * s.bytes, room: math.MaxInt64}
}

// grown returns list with room for one more element of size bytes. ok is
// false where the larger storage would take the sweep past its limit, or
// past its room beside the old storage while the elements are copied.
func grown[T any](w *noteSweep, list []T, size int64) (_ []T, ok bool) {
	if len(list) < cap(list) {
		return list, true
	}
	held := w.held()
	n := min(int64(2*cap(list)+4), (w.limit-held)/size+int64(cap(list)), (w.room-held)/size)
	if n <= int64(len(list)) {
		return list, false
	}
	out := make([]T, len(list), n)
	copy(out, list)
	return out, true
}

// step looks through at most work notes, all that are left when work is
// negative. It returns how many it looked through, and done once the set
// is built again (built) or found to need more room than it has.
func (w *noteSweep) step(keep func(g groupKey) bool, work int) (used int, done, built bool) {
	s := w.set
	for ; w.col < len(s.cols); w.col++ {
		c := &s.cols[w.col]
		g := groupKey{tab: c.tab, col: c.col, prop: c.prop}
		for ; w.run < len(c.runs); w.run++ {
			r := c.runs[w.run]
			for ; w.k < r.n; w.k++ {
				if used == work {
					return used, false, false
				}
				used++
				g.row = r.from + RowID(w.k)
				if !keep(g) {
					continue
				}
				w.n++
				if last := len(w.runs) - 1; last >= 0 && w.runs[last].from+RowID(w.runs[last].n) == g.row {
					w.runs[last].n++
					continue
				}
				if w.size += noteRunBytes; w.size > s.bytes {
					return used, true, false
				}
				runs, ok := grown(w, w.runs, noteRunBytes)
				if !ok {
					return used, true, false
				}
				w.runs = append(runs, noteRun{g.row, 1})
			}
			w.k = 0
		}
		w.run = 0
		if len(w.runs) > 0 {
			if w.size += noteColBytes; w.size > s.bytes {
				return used, true, false
			}
			if held := w.held() + int64(len(w.runs))*noteRunBytes; held > w.limit || held > w.room {
				return used, true, false
			}
			runs := make([]noteRun, len(w.runs))
			copy(runs, w.runs)
			w.built += int64(len(runs)) * noteRunBytes
			cols, ok := grown(w, w.cols, noteColBytes)
			if !ok {
				return used, true, false
			}
			w.cols = append(cols, noteCol{c.tab, c.col, c.prop, runs})
			w.runs = w.runs[:0]
		}
	}
	// The set takes its columns in storage of their number.
	if w.held()+int64(len(w.cols))*noteColBytes > w.room {
		return used, true, false
	}
	set := noteSet{cols: make([]noteCol, len(w.cols))}
	copy(set.cols, w.cols)
	if set.measure() > s.bytes {
		return used, true, false
	}
	s.cols, s.at = set.cols, 0
	s.n, s.gone, s.tried = w.n, w.gone, 0
	return used, true, true
}

// holds reports whether the notes built so far hold the note of group g.
func (w *noteSweep) holds(g groupKey) bool {
	built := noteSet{cols: w.cols}
	if built.has(g) {
		return true
	}
	if w.col >= len(w.set.cols) || w.set.cols[w.col].compare(g.tab, g.col, g.prop) != 0 {
		return false
	}
	runs := w.runs
	k := sort.Search(len(runs), func(k int) bool { return runs[k].from > g.row }) - 1
	return k >= 0 && uint64(g.row-runs[k].from) < runs[k].n
}

// held returns the bytes the sweep keeps beside its set.
func (w *noteSweep) held() int64 {
	return w.built + int64(cap(w.cols))*noteColBytes + int64(cap(w.runs))*noteRunBytes
}

// due reports whether the tick should build the set again: one in
// noteGoneShare of its notes is gone since it was built or since the last
// time that building it again would have made it larger. A set is
// therefore looked through at most noteGoneShare times without a gain,
// each time after other writers stamped that share of its groups.
func (s *noteSet) due() bool {
	return (s.gone-s.tried)*noteGoneShare >= s.n
}

func isAux(prop int8) bool { return prop >= groupWidth }

// linkKey names the notes of one undo that share an entry, a column and a
// property. Only the row differs between them.
type linkKey struct {
	canon uint64
	tab   TabID
	col   ColID
	prop  int8
}

type linkCol struct {
	linkKey
	runs []noteRun
	// mixed says that a row came that does not lie above the rows before
	// it, so the runs are not in order yet (ordered).
	mixed bool
	// next is the place, counted from 1, of the column that the link
	// after one of this column went to last.
	next int32
}

// linkSet collects the stand-in notes an undo sets up while it restores
// its groups: the stamp the undo writes into a group stands in for the
// entry canon. It keeps them the way a note set does, by column and
// property as runs of row ids, so an undo over a range of cells takes a
// few bytes for each column while it runs, too.
type linkSet struct {
	cols  []linkCol
	index map[linkKey]int32
	last  int32 // the place of the column of the link before, from 1
}

func (l *linkSet) add(g groupKey, canon uint64) {
	key := linkKey{canon, g.tab, g.col, g.prop}
	// An undo over a range comes to the same columns in the same order,
	// row after row: the column after the last one is known without the
	// index.
	i := int32(-1)
	if l.last > 0 {
		if next := l.cols[l.last-1].next; next > 0 && l.cols[next-1].linkKey == key {
			i = next - 1
		}
	}
	if i < 0 {
		at, ok := l.index[key]
		if !ok {
			if l.index == nil {
				l.index = map[linkKey]int32{}
			}
			at = int32(len(l.cols))
			l.index[key] = at
			l.cols = append(l.cols, linkCol{linkKey: key})
		}
		if i = at; l.last > 0 {
			l.cols[l.last-1].next = i + 1
		}
	}
	l.last = i + 1
	c := &l.cols[i]
	if last := len(c.runs) - 1; last >= 0 {
		r := &c.runs[last]
		switch end := r.from + RowID(r.n); {
		case g.row == end:
			r.n++
			return
		case g.row < end:
			c.mixed = true
		}
	}
	c.runs = append(c.runs, noteRun{g.row, 1})
}

// ordered puts runs in the order of their rows and joins those that meet
// or cover the same rows. It works in place.
func ordered(runs []noteRun) []noteRun {
	slices.SortFunc(runs, func(a, b noteRun) int { return cmp.Compare(a.from, b.from) })
	out := runs[:1]
	for _, r := range runs[1:] {
		last := &out[len(out)-1]
		if r.from > last.from+RowID(last.n) {
			out = append(out, r)
			continue
		}
		last.n = max(last.n, uint64(r.from-last.from)+r.n)
	}
	return out
}

// sets builds the note sets of the undo from the links that name an entry
// in onStack. A set holds the links of one entry, and the groups that are
// no cells are in a set of their own.
func (l *linkSet) sets(u *undoState, stamp uint64, redo bool, onStack map[uint64]struct{}) []*noteSet {
	var cols []*linkCol
	for i := range l.cols {
		if _, ok := onStack[l.cols[i].canon]; ok {
			cols = append(cols, &l.cols[i])
		}
	}
	slices.SortFunc(cols, func(a, b *linkCol) int {
		if d := cmp.Compare(a.canon, b.canon); d != 0 {
			return d
		}
		if x, y := isAux(a.prop), isAux(b.prop); x != y {
			if y {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.tab, b.tab), cmp.Compare(a.col, b.col), cmp.Compare(a.prop, b.prop))
	})
	var sets []*noteSet
	var s *noteSet
	for _, c := range cols {
		if c.mixed {
			c.runs, c.mixed = ordered(c.runs), false
		}
		if aux := isAux(c.prop); s == nil || s.canon != c.canon || s.aux != aux {
			s = &noteSet{owner: u, stamp: stamp, canon: c.canon, redo: redo, aux: aux}
			sets = append(sets, s)
		}
		s.cols = append(s.cols, noteCol{c.tab, c.col, c.prop, slices.Clone(c.runs)})
		for _, r := range c.runs {
			s.n += int(r.n)
		}
	}
	for _, s := range sets {
		s.cols = slices.Clone(s.cols)
		s.bytes = s.measure()
	}
	return sets
}

// note returns the set that holds the note of group g for the stamp, or nil.
func (u *undoState) note(g groupKey, stamp uint64) *noteSet {
	for _, s := range u.notes[stamp] {
		if s.has(g) {
			return s
		}
	}
	return nil
}

// holds reports whether stamp cur of group g is the one the entry seq of
// the given stack wrote, or stands in for it.
func (u *undoState) holds(g groupKey, seq, cur uint64, redo bool) bool {
	if cur == seq {
		return true
	}
	s := u.note(g, cur)
	return s != nil && s.redo == redo && s.canon == seq
}

// standsFor returns the canonical form of a stamp of group g for an entry
// that joins the given stack: the entry of that stack the stamp stands in
// for, or the stamp itself.
func (u *undoState) standsFor(g groupKey, stamp uint64, redo bool) uint64 {
	if s := u.note(g, stamp); s != nil && s.redo == redo {
		return s.canon
	}
	return stamp
}

func (u *undoState) addNotes(s *noteSet) {
	if u.notes == nil {
		u.notes = map[uint64][]*noteSet{}
		u.indexBudget = u.budget
		u.charge(noteIndexBytes)
	}
	u.notes[s.stamp] = append(u.notes[s.stamp], s)
	u.notesMost = max(u.notesMost, len(u.notes))
	u.noteBytes += s.bytes
	if u.budget != nil {
		u.budget.bytes += s.bytes
	}
}

func (u *undoState) removeNotes(s *noteSet) {
	sets := u.notes[s.stamp]
	i := slices.Index(sets, s)
	if i < 0 {
		return
	}
	// What a sweep of the set holds goes with it.
	if b := u.budget; b != nil && b.sweep != nil && b.sweep.set == s {
		b.end(b.sweep)
	}
	if len(sets) == 1 {
		delete(u.notes, s.stamp)
	} else {
		u.notes[s.stamp] = slices.Delete(sets, i, i+1)
	}
	// A map keeps the room of the most entries it held. The index is made
	// anew once it holds less than half of that, so that the place of
	// each set counts what the index keeps.
	if len(u.notes) == 0 {
		u.notes, u.notesMost = nil, 0
		u.charge(-noteIndexBytes)
	} else if 2*len(u.notes) < u.notesMost {
		fresh := make(map[uint64][]*noteSet, len(u.notes))
		for stamp, sets := range u.notes {
			fresh[stamp] = sets
		}
		u.notes, u.notesMost = fresh, len(fresh)
	}
	u.noteBytes -= s.bytes
	if u.budget != nil {
		u.budget.bytes -= s.bytes
	}
}

// charge counts the bytes of the session's index in its notes and in the
// sheet's budget that counted it when it was made.
func (u *undoState) charge(n int64) {
	u.noteBytes += n
	if b := u.indexBudget; b != nil {
		b.bytes += n
	}
}

// entry returns the entry with the sequence on the given stack that can
// still be undone, or nil.
func (u *undoState) entry(seq uint64, redo bool) *undoEntry {
	stack := u.undo
	if redo {
		stack = u.redo
	}
	for _, e := range stack {
		if e.seq == seq && !e.lost {
			return e
		}
	}
	return nil
}

// prune drops the sets that name no entry on their stack.
func (u *undoState) prune() {
	for _, sets := range u.notes {
		for _, s := range slices.Clone(sets) {
			if u.entry(s.canon, s.redo) == nil {
				u.removeNotes(s)
			}
		}
	}
}

// expiry is an entry that an undo pushed out of the undo limits. why is
// the cause for the host's log, and nil when the session filled its own
// budget (undoEntry.expire).
type expiry struct {
	e   *undoEntry
	why error
}

// install puts the notes of the undo in place: for every group it
// restored, the stamp it wrote stands in for the entry that had written
// the restored value, if the stack still holds that entry. The change is
// journaled, because a reverted undo must not leave a note of a sequence
// that another action may take.
//
// The notes count in the undo memory of the session and of the sheet.
// Where the new ones do not fit, the oldest entries that notes of the
// session stand for can no longer be undone (spec rule 23): they are
// returned, and expire when the action has committed. Their notes are not
// kept.
func (run *undoRun) install(x *txn) []expiry {
	if len(run.links.cols) == 0 {
		return nil
	}
	u := run.u
	stack := u.undo
	if run.redo {
		stack = u.redo
	}
	onStack := make(map[uint64]struct{}, len(stack))
	for _, e := range stack {
		if e.seq != run.seq && !e.lost {
			onStack[e.seq] = struct{}{}
		}
	}
	sets := run.links.sets(u, x.seq, run.redo, onStack)
	run.links = linkSet{}
	if len(sets) == 0 {
		return nil
	}
	var out []expiry
	if p := run.pool; p != nil {
		u.budget = &p.notes
		sums := func() (session, sheet int64) {
			session, sheet = u.noteBytes, p.notes.bytes
			if u.notes == nil {
				// The index is made for the first set.
				session, sheet = session+noteIndexBytes, sheet+noteIndexBytes
			}
			for _, s := range sets {
				session, sheet = session+s.bytes, sheet+s.bytes
			}
			return session, sheet
		}
		session, sheet := sums()
		// An unfinished sweep gives its room back before any entry expires.
		if (session > p.sessionMax || sheet > p.instanceMax) && p.notes.yield(nil) {
			session, sheet = sums()
		}
		if session > p.sessionMax || sheet > p.instanceMax {
			type holder struct {
				seq   uint64
				redo  bool
				bytes int64
			}
			var held []holder
			add := func(s *noteSet) {
				i := slices.IndexFunc(held, func(h holder) bool { return h.seq == s.canon && h.redo == s.redo })
				if i < 0 {
					held, i = append(held, holder{seq: s.canon, redo: s.redo}), len(held)
				}
				held[i].bytes += s.bytes
			}
			for _, old := range u.notes {
				for _, s := range old {
					add(s)
				}
			}
			for _, s := range sets {
				add(s)
			}
			slices.SortFunc(held, func(a, b holder) int { return cmp.Compare(a.seq, b.seq) })
			for _, h := range held {
				if session <= p.sessionMax && sheet <= p.instanceMax {
					break
				}
				e := u.entry(h.seq, h.redo)
				if e == nil {
					continue
				}
				// Inside its own budget the session loses the entry to the
				// notes of other sessions.
				var why error
				if session <= p.sessionMax {
					why = errUndoNotes
				}
				out = append(out, expiry{e, why})
				session, sheet = session-h.bytes, sheet-h.bytes
				sets = slices.DeleteFunc(sets, func(s *noteSet) bool { return s.canon == h.seq && s.redo == h.redo })
			}
		}
	}
	for _, s := range sets {
		u.addNotes(s)
	}
	x.j.push(func() {
		for _, s := range sets {
			u.removeNotes(s)
		}
	})
	return out
}

// noteHits counts the notes of a set whose group an action wrote: of the
// set, and of what a sweep of the set has built so far.
type noteHits struct{ set, swept int }

// replacedNotes finds the notes of every session whose group the action
// wrote: their stamp is gone from the group. It runs while the journal
// still has the styles before.
func (in *Instance) replacedNotes(x *txn) map[*noteSet]noteHits {
	if in.scratch.notes.bytes == 0 {
		return nil
	}
	byStamp := map[uint64][]*noteSet{}
	in.smu.Lock()
	for _, s := range in.sessions {
		for stamp, sets := range s.undo.notes {
			byStamp[stamp] = sets
		}
	}
	in.smu.Unlock()
	if len(byStamp) == 0 {
		return nil
	}
	wb := x.wb
	var out map[*noteSet]noteHits
	sweep := in.scratch.notes.sweep
	hit := func(g groupKey, stamp uint64) {
		for _, s := range byStamp[stamp] {
			if !s.aux && s.has(g) {
				if out == nil {
					out = map[*noteSet]noteHits{}
				}
				h := out[s]
				h.set++
				if sweep != nil && sweep.set == s && sweep.holds(g) {
					h.swept++
				}
				out[s] = h
			}
		}
	}
	for i := 0; i < x.j.len(); i++ {
		je := x.j.at(i)
		// The first entry of a record has what it was before the action.
		if je.fn != nil || !je.fresh {
			continue
		}
		t := je.ref.tab
		if wb.byID[t.id] != t {
			continue
		}
		cur := t.cell(je.ref.row, je.ref.col)
		if cur == nil {
			continue
		}
		g := groupKey{t.id, je.ref.row, je.ref.col, groupContent}
		if je.old.seq != cur.seq {
			hit(g, je.old.seq)
		}
		if je.old.style == cur.style {
			continue
		}
		old, now := wb.styles.state(je.old.style), wb.styles.state(cur.style)
		for prop := 0; prop < propCount; prop++ {
			if old.stamps[prop] != now.stamps[prop] {
				g.prop = int8(prop)
				hit(g, old.stamps[prop])
			}
		}
	}
	return out
}

// stampOf returns the stamp group g carries. known is false where that
// cannot be told now: the group's tab, row or column is not in the sheet,
// or its cell has no record.
func (wb *Workbook) stampOf(g groupKey) (stamp uint64, known bool) {
	if g.prop == groupOrder {
		return wb.tabOrderSeq, true
	}
	t := wb.byID[g.tab]
	if t == nil {
		return 0, false
	}
	switch g.prop {
	case groupSettings:
		return t.settingsSeq, true
	case groupName:
		return t.nameSeq, true
	case groupWidth:
		_, live := t.cols.pos[uint64(g.col)]
		return t.widths[g.col].seq, live
	case groupSort:
		_, live := t.rows.pos[uint64(g.row)]
		return t.lastSort[g.row].seq, live
	}
	c := t.cell(g.row, g.col)
	if c == nil {
		return 0, false
	}
	if g.prop == groupContent {
		return c.seq, true
	}
	return wb.styles.state(c.style).stamps[g.prop], true
}

// The tick builds a set again without its gone notes once one in
// noteGoneShare of its notes is gone (noteSet.due).
const noteGoneShare = 8

// noteSweepWork is the number of notes one tick looks through at most,
// under the sheet's lock: a fifth of a second or so.
const noteSweepWork = 1 << 20

// sweepNotes runs with the maintenance tick. It drops the sets whose entry
// left its stack, takes the notes out whose group another write has
// stamped since, and counts the bytes again. No set takes more bytes
// after it than before (noteSet.filter), so the notes stay inside the
// budgets they were admitted in whatever other writers stamp.
//
// One tick looks through at most noteWork notes. A set it does not finish
// is swept on from where it stopped by the next ticks, before any other
// (noteSweep). The sets it does not reach stay as they are, due, for a
// later tick; their gone notes match nothing until then.
//
// What a sweep holds beside its set fits in what the limits of the
// session's memory and of the sheet's leave, at every moment, and counts
// in both until the sweep ends. A sweep that would pass either ends and
// leaves its set as it is, as one that would make the set larger does;
// one that an action needs the room of gives it back first (yield). The
// sweep therefore never makes an entry expire or a payload go to a file.
func (in *Instance) sweepNotes() {
	wb := in.wb
	p := in.undoPool()
	work := p.noteWork
	budget := &p.notes
	// The write marks count in the sheet's bound beside the notes, and a
	// sweep writes none.
	marks := wb.markBytes()
	in.smu.Lock()
	defer in.smu.Unlock()
	for _, sn := range in.sessions {
		sn.undo.budget = budget
		sn.undo.prune()
	}
	// sweep goes on with a set and reports whether the tick has work left
	// for another.
	sweep := func(w *noteSweep) bool {
		s, u := w.set, w.set.owner
		w.room = min(p.sessionMax-(u.resident()-w.counted), p.instanceMax-(p.resident+budget.bytes+marks-w.counted))
		// A recovery replaced the tab a note knew. The tab can return as
		// it was, with the stamp of the note.
		used, done, built := w.step(func(g groupKey) bool {
			if c := counting.Load(); c != nil {
				c.notes.Add(1)
			}
			if t := wb.byID[g.tab]; t != nil && t.recovered > s.stamp {
				return true
			}
			stamp, known := wb.stampOf(g)
			return !known || stamp == s.stamp
		}, work)
		work -= used
		if !done {
			budget.hold(w)
			return false
		}
		budget.end(w)
		s.seen = wb.seq
		if !built {
			s.tried = s.gone
			return work > 0
		}
		n := s.measure() - s.bytes
		s.bytes += n
		u.noteBytes += n
		budget.bytes += n
		if s.n == 0 {
			u.removeNotes(s)
		}
		return work > 0
	}
	left := work > 0
	if w := budget.sweep; w != nil {
		// The sweep of a set that leaves its session ends at once
		// (removeNotes). A state that was replaced whole took its count
		// with it.
		if slices.Contains(w.set.owner.notes[w.set.stamp], w.set) {
			left = sweep(w)
		} else {
			budget.sweep = nil
		}
	}
sessions:
	for _, sn := range in.sessions {
		for _, sets := range sn.undo.notes {
			for _, s := range slices.Clone(sets) {
				if !left {
					break sessions
				}
				if s.aux && s.seen == wb.seq || !s.aux && !s.due() {
					continue
				}
				left = sweep(s.sweep())
			}
		}
	}
	var bytes int64
	for _, sn := range in.sessions {
		bytes += sn.undo.noteBytes
	}
	budget.bytes = bytes
}
