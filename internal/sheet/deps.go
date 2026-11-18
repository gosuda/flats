package sheet

import (
	"sort"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// fnode is the dependency node of one formula cell. The fields below today
// belong to the recalculation pass that last touched the node; only the
// writer reads or writes them.
type fnode struct {
	tab     *Tab
	row     RowID
	col     ColID
	cell    *Cell
	gen     uint32 // entries of the index made under another gen are dead
	entries int64  // entries of the index made under gen
	reads   int64  // static read count of the formula at its cell
	today   bool
	// kept is the sequence of the action whose journal holds the value the
	// cell had before the action. A value the action replaces after that
	// is one it calculated itself.
	kept uint64

	dirty      uint32 // pass that marked the node dirty
	seen       uint32 // pass whose ordering walk reached the node
	done       uint32 // pass whose value ov holds
	ov         formula.Value
	index, low int32
	onStack    bool
	cyc        bool
}

type posKey struct {
	tab      TabID
	row, col int32
}

type colKey struct {
	tab TabID
	col int32
}

type depRef struct {
	n   *fnode
	gen uint32
}

// partLen is the most entries one part of a list of the index holds. A
// cell may be read by millions of references, and a column may hold
// millions of ranges. A list that grew as one slice would copy all of them
// at once into new memory it makes at once, which takes longer than the
// gate and the deadline may wait. A list grows a part at a time instead:
// the first part by copying, as a slice does, and each later one made at
// its full length.
const partLen = 1 << 16

// parted is a list of the index: its first part, head, and the parts after
// it, rest. Every part but the last holds partLen entries.
type parted[E any] struct {
	head []E
	rest [][]E
}

func (l parted[E]) len() int {
	n := len(l.head)
	for _, part := range l.rest {
		n += len(part)
	}
	return n
}

// at returns the place of entry i.
func (l *parted[E]) at(i int) *E {
	if i < partLen {
		return &l.head[i]
	}
	i -= partLen
	return &l.rest[i/partLen][i%partLen]
}

// last returns the last part.
func (l *parted[E]) last() *[]E {
	if len(l.rest) == 0 {
		return &l.head
	}
	return &l.rest[len(l.rest)-1]
}

// push adds an entry. The first part doubles its room as it fills, up to
// partLen entries, so it copies fewer than that at once; a later part is
// made at its full length. A part that fit made shorter grows again by
// doubling. No part has room past partLen entries.
func (l *parted[E]) push(e E) {
	last := l.last()
	switch {
	case len(*last) == partLen:
		l.rest = append(l.rest, make([]E, 0, partLen))
		last = l.last()
	case len(*last) == cap(*last):
		grown := make([]E, len(*last), min(max(2*cap(*last), 8), partLen))
		copy(grown, *last)
		*last = grown
	}
	*last = append(*last, e)
}

// keep keeps the first n entries and lets the parts after them go.
func (l *parted[E]) keep(n int) {
	if n <= partLen {
		l.head = l.head[:n]
		clear(l.rest)
		l.rest = nil
		return
	}
	n -= partLen
	parts := (n + partLen - 1) / partLen
	clear(l.rest[parts:])
	l.rest = l.rest[:parts]
	l.rest[parts-1] = l.rest[parts-1][:n-(parts-1)*partLen]
}

// fit copies the last part into one of its own length, so that the room
// of the entries dropped from it goes, in blocks counted on s
// (copyPieces). The part holds at most partLen entries. It reports false
// when s refused, and the list is then as it was.
func (l *parted[E]) fit(s *steps) bool {
	last := l.last()
	if cap(*last) == len(*last) {
		return true
	}
	fresh, ok := copyPieces(*last, mergeBlock, s.take)
	if ok {
		*last = fresh
	}
	return ok
}

// extend makes the list hold n entries, adding zero ones. Each part it
// makes, and the copy of a part when that grows, is counted on s before, a
// step an entry. A part it makes or grows has room for the entries it
// holds then and no more. It reports false when s refused; the entries it
// added by then are zero.
func (l *parted[E]) extend(n int, s *steps) bool {
	for have := l.len(); have < n; have = l.len() {
		last := l.last()
		if len(*last) == partLen {
			size := min(n-have, partLen)
			if !s.take(size) {
				return false
			}
			l.rest = append(l.rest, make([]E, 0, size))
			last = l.last()
		}
		k := min(n-have, partLen-len(*last))
		if room := len(*last) + k; cap(*last) < room {
			if !s.take(room) {
				return false
			}
			grown := make([]E, len(*last), room)
			copy(grown, *last)
			*last = grown
		}
		*last = (*last)[:len(*last)+k]
		clear((*last)[len(*last)-k:])
	}
	return true
}

type interval struct {
	first, last int32
	n           *fnode
	gen         uint32
}

// intervals is the interval index of one column: range references that
// span it, sorted by first row, with the greatest last row of each subtree
// of the implicit search tree over them (technical design, section 10.1).
type intervals struct {
	sorted  parted[interval]
	maxLast parted[int32]
	tail    []interval
	loose   bool // sorted is not in order yet
}

const intervalTail = 64

// add enters an interval.
func (iv *intervals) add(e interval, bulk bool) {
	if bulk {
		iv.sorted.push(e)
		iv.loose = true
		return
	}
	iv.tail = append(iv.tail, e)
	if len(iv.tail) > intervalTail {
		// An action may enter many thousands of ranges into one column.
		// Merging at every full tail would put the whole column in order
		// each time, so the column goes loose and the next lookup merges
		// it once, on the budget of its recalculation.
		for _, e := range iv.tail {
			iv.sorted.push(e)
		}
		clear(iv.tail)
		iv.tail, iv.loose = iv.tail[:0], true
	}
}

// mergeBlock is the number of entries merge goes through, or compares,
// between two looks at a budget.
const mergeBlock = 4096

// merge drops the dead entries and puts the rest in order. It returns the
// number it dropped.
//
// After a full build a column may hold millions of loose entries, and the
// first lookup merges them. A lookup of a recalculation gives merge its
// budget: an entry that merge goes through, one it moves, a comparison of
// two and a swap of two are a step each, counted before each block of
// them (dropDead, sortIntervals), and so is each entry of a part of the
// tree it makes.
// When the deadline passes merge stops where it is, with nothing left to
// put back. Every live entry is then still in the column, which stays
// loose for the next lookup to merge.
func (iv *intervals) merge(b *formula.Budget) int64 {
	before := iv.sorted.len() + len(iv.tail)
	s := steps{b: b}
	alive := func(e *interval) bool { return e.n.gen == e.gen }
	iv.loose = true // dropping entries changes their order
	gone, done := iv.sorted.dropDead(alive, mergeBlock, s.take)
	kept := iv.sorted.len()
	if done {
		// The tail holds at most intervalTail entries, and lookups go
		// through it as it is.
		iv.tail, done = dropDeadIn(iv.tail, alive, mergeBlock, s.take)
	}
	dropped := int64(before - kept - len(iv.tail))
	// A lookup that drops dead entries lowers the count that starts a
	// compaction, so the room they took goes here.
	if !done || b.Exceeded() || gone > 0 && !iv.sorted.fit(&s) || !sortIntervals(&iv.sorted, b) {
		return dropped
	}
	// The tree is made anew from the entries, so a last part with room
	// past them goes instead of staying for a tree that no longer needs it.
	iv.maxLast.keep(min(iv.maxLast.len(), kept))
	if last := iv.maxLast.last(); cap(*last) > len(*last) {
		*last = nil
	}
	if !iv.maxLast.extend(kept, &s) {
		return dropped
	}
	built := 0
	iv.build(0, kept, b, &built)
	b.Add(int64(built%mergeBlock) / 64)
	iv.loose = b.Exceeded()
	return dropped
}

// dropDead drops the entries of the list that are not alive, and keeps
// the first kept entries. A dead entry takes the last entry of the list,
// whose place is cleared at once, so after each entry the first kept
// entries are the list and no other place names a node. The order of the
// entries means nothing to the index. An entry gone through and one moved
// are a step each, and each block of up to block entries is counted with
// take before it is gone through. When take refuses a block, dropDead
// keeps the entries it holds then, returns done false, and leaves nothing
// to move or clear. It goes through the entries a part at a time, so a
// live entry costs a look at it and nothing more.
func (l *parted[E]) dropDead(alive func(*E) bool, block int, take func(steps int) bool) (dropped int, done bool) {
	var zero E
	n := l.len()
	before := n
	var part []E
	base := 0
	for i := 0; i < n; {
		// Each entry of the block is gone through once, and a dead one
		// moved, at most two steps an entry.
		k := min(block, n-i)
		if !take(2 * k) {
			l.keep(n)
			return before - n, false
		}
		for ; k > 0; k-- {
			if i < base || i >= base+len(part) {
				part, base = l.partOf(i)
			}
			e := &part[i-base]
			if alive(e) {
				i++
				continue
			}
			n--
			last := l.at(n)
			*e, *last = *last, zero
		}
	}
	l.keep(n)
	return before - n, true
}

// partOf returns the part that holds entry i and the index of its first
// entry.
func (l *parted[E]) partOf(i int) ([]E, int) {
	if i < partLen {
		return l.head, 0
	}
	p := (i - partLen) / partLen
	return l.rest[p], partLen * (p + 1)
}

// dropDeadIn drops the dead entries of a slice of at most partLen entries
// as dropDead does.
func dropDeadIn[E any](list []E, alive func(*E) bool, block int, take func(steps int) bool) (kept []E, done bool) {
	l := parted[E]{head: list}
	_, done = l.dropDead(alive, block, take)
	return l.head, done
}

// copyPieces copies list into a new slice of its length, a block of
// entries at a time, each counted with take before it is copied, a step an
// entry. Making the new slice is counted first, a step an entry. When take
// refuses, it returns false. The copy made so far is let go, and list is
// as it was.
func copyPieces[E any](list []E, block int, take func(steps int) bool) ([]E, bool) {
	if !take(len(list)) {
		return nil, false
	}
	return appendPieces(make([]E, 0, len(list)), list, block, take)
}

// appendPieces appends src to dst, which has room for it, a block of
// entries at a time, each counted with take before it is appended.
func appendPieces[E any](dst, src []E, block int, take func(steps int) bool) ([]E, bool) {
	for from := 0; from < len(src); from += block {
		to := min(from+block, len(src))
		if !take(to - from) {
			return nil, false
		}
		dst = append(dst, src[from:to]...)
	}
	return dst, true
}

// sortStopped ends a sort whose deadline passed.
type sortStopped struct{}

// byFirst sorts a list of intervals in the order of their first rows,
// counting each comparison and each swap on b, a step each. The sort may
// swap without comparing: it reverses a list whose samples it takes for
// one in reverse order, half a swap an entry.
type byFirst struct {
	l     *parted[interval]
	b     *formula.Budget
	steps int
}

// step counts a step, and ends the sort when the deadline has passed.
func (x *byFirst) step() {
	if x.steps++; x.steps%mergeBlock == 0 && !x.b.Add(mergeBlock/64) {
		panic(sortStopped{})
	}
}

func (x *byFirst) Len() int { return x.l.len() }

func (x *byFirst) Less(i, j int) bool {
	x.step()
	return x.l.at(i).first < x.l.at(j).first
}

func (x *byFirst) Swap(i, j int) {
	x.step()
	a, b := x.l.at(i), x.l.at(j)
	*a, *b = *b, *a
}

// sortIntervals puts the entries in the order of their first rows. It
// reports false when the deadline of the budget passed first: the sort
// ended there, with every entry still in the list.
func sortIntervals(l *parted[interval], b *formula.Budget) (done bool) {
	// The sort only swaps entries, and a step is counted before its swap,
	// so leaving it loses none.
	x := &byFirst{l: l, b: b}
	defer func() {
		if w := counting.Load(); w != nil {
			w.sorted.Add(int64(x.steps))
		}
		if r := recover(); r != nil {
			if _, stopped := r.(sortStopped); !stopped {
				panic(r)
			}
		}
	}()
	sort.Sort(x)
	return b.Add(int64(x.steps%mergeBlock) / 64)
}

// build fills maxLast for the entries from lo to hi and returns their
// greatest last row. built counts the entries it has done, for the budget;
// after the deadline it does no more.
func (iv *intervals) build(lo, hi int, b *formula.Budget, built *int) int32 {
	if lo >= hi || b.Exceeded() {
		return -1
	}
	if *built++; *built%mergeBlock == 0 {
		b.Add(mergeBlock / 64)
	}
	mid := (lo + hi) / 2
	m := max(iv.sorted.at(mid).last, iv.build(lo, mid, b, built), iv.build(mid+1, hi, b, built))
	*iv.maxLast.at(mid) = m
	return m
}

// query visits every live entry whose rows contain row and returns the
// entries it looked at and the dead entries it dropped. b is the budget of
// the recalculation that looks, which a merge counts on. When its deadline
// passes in the merge, query visits nothing.
func (iv *intervals) query(row int32, visit func(*fnode), b *formula.Budget) (work, dropped int64) {
	if iv.loose {
		if dropped = iv.merge(b); iv.loose {
			return 0, dropped
		}
	}
	iv.walk(0, iv.sorted.len(), row, visit, b, &work)
	for _, e := range iv.tail {
		if e.first <= row && row <= e.last && e.n.gen == e.gen {
			visit(e.n)
		}
	}
	return work + int64(len(iv.tail)), dropped
}

// walk visits the entries from lo to hi whose rows contain row. Each entry
// it looks at is a unit of work. A row may lie in millions of entries, so
// the units reach the budget a block at a time, and work is left with what
// the budget does not hold yet. After the deadline walk looks at no more.
func (iv *intervals) walk(lo, hi int, row int32, visit func(*fnode), b *formula.Budget, work *int64) {
	if lo >= hi || b.Exceeded() {
		return
	}
	if *work++; *work >= entryBlock {
		b.Add(*work)
		*work = 0
	}
	mid := (lo + hi) / 2
	if *iv.maxLast.at(mid) < row {
		return
	}
	iv.walk(lo, mid, row, visit, b, work)
	e := iv.sorted.at(mid)
	if e.first > row {
		return
	}
	if e.last >= row && e.n.gen == e.gen {
		visit(e.n)
	}
	iv.walk(mid+1, hi, row, visit, b, work)
}

// depIndex maps a read cell to the formulas that read it: single-cell
// references by position, range references through an interval index per
// column, and the formulas that read TODAY.
//
// An entry dies when its formula is registered again or leaves its cell.
// total and live count the entries the index holds and those that are
// alive, and compact drops the dead ones once they outnumber the live ones,
// so the index stays in proportion to the references of the formulas the
// sheet holds (technical design, section 10.1).
type depIndex struct {
	// singles holds the first part of the list of single references of
	// each read cell, and more the parts after it of a list longer than
	// partLen (parted).
	singles map[posKey][]depRef
	more    map[posKey][][]depRef
	ranges  map[colKey]*intervals
	today   map[*fnode]struct{}
	stale   bool // the index must be rebuilt in full before its next use
	bulk    bool
	total   int64
	live    int64
	// keys holds the most keys singles and ranges held since they were
	// made (shrinkMap).
	keys struct{ singles, ranges int }
}

// depSlack is the number of dead entries a small index may hold.
const depSlack = 1024

func newDepIndex() depIndex {
	return depIndex{singles: map[posKey][]depRef{}, more: map[posKey][][]depRef{},
		ranges: map[colKey]*intervals{}, today: map[*fnode]struct{}{}}
}

// refs returns the list of single references of a read cell: its first
// part in singles and the parts after it in more.
func (d *depIndex) refs(key posKey) parted[depRef] {
	l := parted[depRef]{head: d.singles[key]}
	if len(l.head) == partLen {
		l.rest = d.more[key]
	}
	return l
}

// setRefs stores the list of a read cell, or forgets it when it is empty.
func (d *depIndex) setRefs(key posKey, l parted[depRef]) {
	if len(l.head) == 0 {
		delete(d.singles, key)
		delete(d.more, key)
		return
	}
	d.singles[key] = l.head
	d.keys.singles = max(d.keys.singles, len(d.singles))
	if len(l.rest) > 0 {
		d.more[key] = l.rest
	} else if len(d.more) > 0 {
		delete(d.more, key)
	}
}

// compact drops the dead entries. It works on the budget of the action
// whose write found too many of them: a step for each entry it goes
// through, moves or copies, counted before each block of them, and the
// steps of a merge (intervals.merge). When the deadline passes it stops
// where it is, with nothing to finish, and the action is refused. The
// entries it has not reached stay as they were, alive or dead, every list
// it reached holds its live entries, and total still counts every entry
// the index holds, so the index answers as before.
//
// Every list it reaches keeps room for its entries and no more, and a map
// that lost most of its keys is copied into one of their size
// (shrinkMap), since a map keeps the room of the keys it lost.
func (d *depIndex) compact(b *formula.Budget) {
	s := steps{b: b}
	for key := range d.singles {
		l := d.refs(key)
		dropped, done := pruneSingles(&l, &s)
		d.total -= dropped
		// The parts the dead entries took are let go, and the last part
		// left is copied into one of its own length. A stop on the way
		// keeps the part it copies.
		if done && dropped > 0 && !l.fit(&s) {
			d.setRefs(key, l)
			return
		}
		d.setRefs(key, l)
		if !done || b.Exceeded() {
			return
		}
	}
	for key, iv := range d.ranges {
		// A merge of a column of a few entries counts less than a unit,
		// and a sheet may have tens of thousands of such columns, so each
		// column is a step besides.
		if !s.take(1) {
			return
		}
		d.total -= iv.merge(b)
		if iv.loose {
			return
		}
		if iv.sorted.len()+len(iv.tail) == 0 {
			delete(d.ranges, key)
			continue
		}
		if !iv.sorted.fit(&s) {
			return
		}
		if cap(iv.tail) > len(iv.tail) {
			if !s.take(len(iv.tail)) {
				return
			}
			iv.tail = fitted(iv.tail)
		}
	}
	d.total = d.live
	d.singles = shrinkMap(d.singles, &d.keys.singles, &s)
	d.ranges = shrinkMap(d.ranges, &d.keys.ranges, &s)
}

// fitted returns a copy of list of its own length, or nil for an empty
// list, which then keeps nothing of list alive.
func fitted[E any](list []E) []E {
	if len(list) == 0 {
		return nil
	}
	return append(make([]E, 0, len(list)), list...)
}

// mapKeySteps is what shrinkMap counts for a key it copies: an insertion
// costs about as much as copying a few entries of a list.
const mapKeySteps = 16

// shrinkMap returns m copied into a new map when m once held more than
// four times its keys and more than depSlack keys: a map keeps the room of
// the most keys it held. most is that number, which the copy makes its
// own. The walk through m and each key are counted on s before, and when
// s refuses, shrinkMap returns m as it was. A map grows a table of at most
// 1,024 places at a time, so the copy never makes more than that at once.
func shrinkMap[K comparable, V any](m map[K]V, most *int, s *steps) map[K]V {
	// Going through m visits every place it has room for, a step for each
	// group of eight.
	if *most <= max(4*len(m), depSlack) || !s.take(*most/8) {
		return m
	}
	fresh := make(map[K]V)
	for k, v := range m {
		if !s.take(mapKeySteps) {
			return m
		}
		fresh[k] = v
	}
	*most = len(fresh)
	return fresh
}

// steps counts small work on a budget, 64 steps to a unit, before the work
// is done.
type steps struct {
	b *formula.Budget
	n int64
}

// take counts n steps of work about to be done and reports whether the
// work may be done.
func (s *steps) take(n int) bool {
	if s.n += int64(n); s.n < 64 {
		return !s.b.Exceeded()
	}
	units := s.n / 64
	s.n %= 64
	return s.b.Add(units)
}

// pruneBlock is the number of entries of a list of single references
// that pruneSingles goes through between two looks at the budget, 2,048
// units at most.
const pruneBlock = 64 * entryBlock

// pruneSingles drops the dead entries of a list of single references,
// which may hold millions of entries, in blocks that are counted on s
// before they are gone through (dropDead). When the deadline keeps it from
// a block, it returns with the list as it is then, done false, with
// nothing left to move or clear: the live entries of the blocks it went
// through and every entry after them. The list holds every entry it does
// not drop, in another order, the room it no longer uses is cleared and
// the parts it no longer uses are let go.
func pruneSingles(l *parted[depRef], s *steps) (dropped int64, done bool) {
	gone, done := l.dropDead(func(e *depRef) bool { return e.n.gen == e.gen }, pruneBlock, s.take)
	return int64(gone), done
}

// countReads sets the static read count of a formula cell at its position and
// what else of it the index keeps beside its entries.
func (wb *Workbook) countReads(n *fnode, pos formula.Pos) {
	f := n.cell.f
	n.reads = f.ReadCells(pos, wb.dims(n.tab))
	wb.count.reads += n.reads
	if n.today = f.ReadsToday(); n.today {
		wb.deps.today[n] = struct{}{}
	}
}

// enter adds the entries of a measured formula cell: one for a single
// reference, and one per column of a range inside the grid. A range never
// gets more entries than it reads cells, because each of its columns holds
// at least one cell of it, so the read limits bound the index.
//
// One formula may get a million entries. They count on the budget b of the
// full build or the action that enters them, a unit each, 1,024 at a time.
// Outside a full build, a list of single references that the formula
// joins is first gone through for its dead entries, once for each list,
// in blocks that count before they are gone through, a step an entry
// (pruneSingles). Once the deadline has passed enter makes no more entries
// and goes through no more of a list: the build then drops the index, and
// an action marks it stale. The entries enter has made and dropped are
// counted either way.
func (wb *Workbook) enter(n *fnode, pos formula.Pos, b *formula.Budget) {
	d := &wb.deps
	var added, dropped, counted int64
	charge := func() bool {
		if added-counted < entryBlock {
			return true
		}
		units := added - counted
		counted = added
		return b.Add(units)
	}
	looked := steps{b: b}
	// No entry of another formula dies while this one is entered, so each
	// list is gone through once.
	var first posKey
	var have bool
	var more map[posKey]struct{}
	pruned := func(key posKey) bool {
		switch {
		case !have:
			first, have = key, true
			return false
		case key == first:
			return true
		}
		if _, ok := more[key]; ok {
			return true
		}
		if more == nil {
			more = map[posKey]struct{}{}
		}
		more[key] = struct{}{}
		return false
	}
	n.cell.f.Refs(pos, func(r formula.Ref) {
		if b.Exceeded() {
			return
		}
		t := n.tab
		if r.Tab != "" {
			if t = wb.tab(r.Tab); t == nil {
				return
			}
		}
		if r.Single() {
			key := posKey{t.id, r.First.Row, r.First.Col}
			l := d.refs(key)
			if !d.bulk && len(l.head) >= 8 && !pruned(key) {
				gone, done := pruneSingles(&l, &looked)
				dropped += gone
				// The dropped entries lower the count that starts a
				// compaction, so the room they took goes here.
				if done && gone > 0 && !l.fit(&looked) {
					done = false
				}
				if !done {
					d.setRefs(key, l)
					return
				}
			}
			l.push(depRef{n, n.gen})
			d.setRefs(key, l)
			added++
			charge()
			return
		}
		// Rows and columns past the end of the tab hold no cell. Every
		// change of an axis builds the index again.
		if int(r.First.Row) >= len(t.rows.ids) {
			return
		}
		lastCol := min(r.Last.Col, int32(len(t.cols.ids))-1)
		for col := r.First.Col; col <= lastCol; col++ {
			key := colKey{t.id, col}
			iv := d.ranges[key]
			if iv == nil {
				iv = &intervals{}
				d.ranges[key] = iv
				d.keys.ranges = max(d.keys.ranges, len(d.ranges))
			}
			iv.add(interval{r.First.Row, r.Last.Row, n, n.gen}, d.bulk)
			added++
			if !charge() {
				return
			}
		}
	})
	n.entries = added
	d.total += added - dropped
	d.live += added
	b.Add(added - counted)
}

// entryBlock is the number of entries a full build makes for one formula
// between two additions to its budget.
const entryBlock = 1024

// register enters the references of a formula cell, resolved at its
// position, and replaces the entries the node had. b is the budget of the
// action that writes the formula, which the entries and a compaction count
// on.
//
// A formula over a read limit gets no entries, and neither does one that
// takes the sheet over its limit: the entries of a range follow the cells
// it reads. The index is then stale, and the write that left the formula is
// refused when its recalculation builds the index again. After the
// deadline the index is stale as well, since the formula may have only
// some of its entries, and the action is refused.
func (wb *Workbook) register(n *fnode, b *formula.Budget) {
	wb.unregister(n)
	pos, _ := n.tab.posOf(n.row, n.col)
	wb.countReads(n, pos)
	d := &wb.deps
	if n.reads > maxFormulaReads || wb.count.reads > maxSheetReads || b.Exceeded() {
		d.stale = true
		return
	}
	if wb.enter(n, pos, b); b.Exceeded() {
		d.stale = true
		return
	}
	wb.dropDead(b)
}

// dropDead compacts the index once its dead entries outnumber the live
// ones and depSlack, on the budget b of the action whose write left them.
// That write may enter a formula, or replace one with plain content and
// enter nothing.
func (wb *Workbook) dropDead(b *formula.Budget) {
	if d := &wb.deps; !d.stale && d.total-d.live > max(d.live, depSlack) {
		d.compact(b)
	}
}

// unregister makes the entries of a node dead. A dead entry names the node
// until a compaction drops it, and the index may hold thousands of them,
// so the node lets go of the value of its last pass: once its cell is
// cleared, no count holds that text. No pass reads the value before it
// calculates the node again (grid.Value).
func (wb *Workbook) unregister(n *fnode) {
	n.ov = formula.Value{}
	n.gen++
	wb.count.reads -= n.reads
	wb.deps.live -= n.entries
	n.reads, n.entries = 0, 0
	delete(wb.deps.today, n)
}

// rebuildDeps builds the index of a workbook that no edit is changing.
func (wb *Workbook) rebuildDeps() *WriteError { return wb.buildDeps(nil) }

// buildDeps builds the index in full and gives every formula cell a
// fresh node. It first counts the cells every formula reads and refuses a
// state over a read limit before it makes one entry, so the index is never
// larger than the limits allow. The budget's deadline ends the build as it
// ends a recalculation. After a refusal the index is empty and stale.
func (wb *Workbook) buildDeps(b *formula.Budget) *WriteError {
	wb.deps = newDepIndex()
	wb.deps.stale = true
	wb.count.reads = 0
	var over *fnode
	wb.eachFormulaPaced(b, func(t *Tab, row RowID, col ColID, pos formula.Pos, c *Cell) bool {
		// What the journal of the action under way holds of the cell
		// stays known (kept).
		var kept uint64
		if c.node != nil {
			kept = c.node.kept
		}
		c.node = &fnode{tab: t, row: row, col: col, cell: c, kept: kept}
		wb.countReads(c.node, pos)
		if over == nil && c.node.reads > maxFormulaReads {
			over = c.node
		}
		return b.Add(1)
	})
	switch {
	case b.Exceeded():
		return calcTimeout()
	case over != nil:
		return capacity("formula_reads", maxFormulaReads, over.reads)
	case wb.count.reads > maxSheetReads:
		return capacity("sheet_reads", maxSheetReads, wb.count.reads)
	}
	wb.deps.bulk = true
	wb.eachFormulaPaced(b, func(_ *Tab, _ RowID, _ ColID, pos formula.Pos, c *Cell) bool {
		wb.enter(c.node, pos, b)
		return b.Add(1)
	})
	if b.Exceeded() {
		wb.deps = newDepIndex()
		wb.deps.stale = true
		return calcTimeout()
	}
	wb.deps.bulk, wb.deps.stale = false, false
	return nil
}

// dependents visits the formulas that read the cell and returns the index
// entries it looked at. b is the budget of the recalculation that looks:
// putting a column in order for the lookup counts on it, and after its
// deadline the lookup may have visited only some of the formulas.
func (d *depIndex) dependents(tab TabID, p formula.Pos, visit func(*fnode), b *formula.Budget) int64 {
	work := int64(1)
	l := d.refs(posKey{tab, p.Row, p.Col})
	for part := -1; part < len(l.rest); part++ {
		list := l.head
		if part >= 0 {
			list = l.rest[part]
		}
		for _, e := range list {
			if work++; work >= entryBlock {
				if !b.Add(work) {
					return 0
				}
				work = 0
			}
			if e.n.gen == e.gen {
				visit(e.n)
			}
		}
	}
	if iv := d.ranges[colKey{tab, p.Col}]; iv != nil {
		n, dropped := iv.query(p.Row, visit, b)
		work += n
		d.total -= dropped
	}
	return work
}
