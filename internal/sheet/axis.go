package sheet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sort"
)

// maxID bounds every tab, row and column id, so that an id is exact as a
// JSON number (technical design, section 4.1).
const maxID = 1 << 53

// stampRun gives the ids first to first+count-1 one born stamp.
type stampRun struct{ first, count, seq uint64 }

// runStamps holds the born stamp of every live id of an axis as runs sorted
// by id. Memory follows the breaks in the stamps, not the ids.
type runStamps []stampRun

func (r runStamps) get(id uint64) uint64 {
	i := sort.Search(len(r), func(i int) bool { return r[i].first+r[i].count > id })
	if i < len(r) && r[i].first <= id {
		return r[i].seq
	}
	return 0
}

// set stamps ids that the runs do not hold yet.
func (r *runStamps) set(first, count, seq uint64) {
	if count == 0 {
		return
	}
	s := *r
	i := sort.Search(len(s), func(i int) bool { return s[i].first > first })
	if i > 0 && s[i-1].seq == seq && s[i-1].first+s[i-1].count == first {
		s[i-1].count += count
		if i < len(s) && s[i].seq == seq && s[i].first == first+count {
			s[i-1].count += s[i].count
			s = slices.Delete(s, i, i+1)
		}
		*r = s
		return
	}
	if i < len(s) && s[i].seq == seq && s[i].first == first+count {
		s[i].first, s[i].count = first, s[i].count+count
		return
	}
	*r = slices.Insert(s, i, stampRun{first, count, seq})
}

// pacer is the check of a loop of bookkeeping that can be as long as its
// input: it is called with the steps of the work done since its last call,
// as txn.spend counts them, and the loop ends when it returns false. A nil
// pacer never ends a loop: the undo of a journal runs to its end.
type pacer func(steps int) bool

// paceEvery is how many ids or runs such a loop goes through between two
// calls of its pacer, and paceSteps what each of them counts: an eighth of
// a unit, as the edit of an axis counts an id (txn.axisEdit).
const (
	paceEvery = 1024
	paceSteps = 8
)

// ticker counts the steps of one loop and calls its pacer after every
// paceEvery of them. Each counts cost steps, or paceSteps when cost is 0.
type ticker struct {
	more pacer
	cost int
	n    int
}

func (t *ticker) steps(n int) int {
	if t.cost == 0 {
		return n * paceSteps
	}
	return n * t.cost
}

func (t *ticker) tick() bool {
	if t.more == nil {
		return true
	}
	if t.n++; t.n < paceEvery {
		return true
	}
	t.n = 0
	return t.more(t.steps(paceEvery))
}

// add counts n steps at once, for work the loop does in one piece, such as
// a copy.
func (t *ticker) add(n int) bool {
	if t.more == nil || n == 0 {
		return true
	}
	return t.more(t.steps(n))
}

// done counts the steps since the last call of the pacer.
func (t *ticker) done() bool {
	if t.more == nil || t.n == 0 {
		return true
	}
	n := t.n
	t.n = 0
	return t.more(t.steps(n))
}

// push appends a run, joined to the last one where it continues it.
func (r *runStamps) push(run stampRun) {
	if n := len(*r); n > 0 && (*r)[n-1].seq == run.seq && (*r)[n-1].first+(*r)[n-1].count == run.first {
		(*r)[n-1].count += run.count
		return
	}
	*r = append(*r, run)
}

// after reports whether every id of add lies after every id of r.
func (r runStamps) after(add runStamps) bool {
	return len(add) == 0 || len(r) == 0 || r[len(r)-1].first+r[len(r)-1].count <= add[0].first
}

// with returns r with the runs of add, whose ids r does not hold. Both are
// sorted by id. It walks the two lists once into a new list and leaves r
// as it is, so that a walk its pacer ends returns false and changes
// nothing.
func (r runStamps) with(add runStamps, more pacer) (runStamps, bool) {
	// The runs before the first one added are copied in one piece.
	i := 0
	if len(add) > 0 {
		i = sort.Search(len(r), func(i int) bool { return r[i].first > add[0].first })
	}
	t := ticker{more: more}
	if !t.add(i) {
		return nil, false
	}
	out := make(runStamps, i, len(r)+len(add))
	copy(out, r[:i])
	for j := 0; i < len(r) || j < len(add); {
		if !t.tick() {
			return nil, false
		}
		if j == len(add) || (i < len(r) && r[i].first < add[j].first) {
			out.push(r[i])
			i++
		} else {
			out.push(add[j])
			j++
		}
	}
	return out, t.done()
}

// without returns r without the ids of the runs of gone, which are sorted
// by id and whose stamps do not matter. Like with it walks both lists once
// into a new list and leaves r as it is.
func (r runStamps) without(gone runStamps, more pacer) (runStamps, bool) {
	i := 0
	if len(gone) > 0 {
		i = sort.Search(len(r), func(i int) bool { return r[i].first+r[i].count > gone[0].first })
	}
	t := ticker{more: more}
	if !t.add(i) {
		return nil, false
	}
	out := make(runStamps, i, len(r)+len(gone))
	copy(out, r[:i])
	// next is the first id of gone[j] that is not taken out yet.
	j, next := 0, uint64(0)
	if len(gone) > 0 {
		next = gone[0].first
	}
	for ; i < len(r); i++ {
		run := r[i]
		start, end := run.first, run.first+run.count
		for j < len(gone) && next < end {
			if !t.tick() {
				return nil, false
			}
			if next > start {
				out = append(out, stampRun{start, next - start, run.seq})
			}
			last := gone[j].first + gone[j].count
			if last > end {
				// The rest of the gone run lies beyond this run.
				start, next = end, end
				break
			}
			start = max(start, last)
			if j++; j < len(gone) {
				next = gone[j].first
			}
		}
		if !t.tick() {
			return nil, false
		}
		if start < end {
			out = append(out, stampRun{start, end - start, run.seq})
		}
	}
	return out, t.done()
}

// stampsOf returns the ids of sorted, which ascend and which r holds, as
// runs with the stamps r gives them. It walks both once.
func (r runStamps) stampsOf(sorted []uint64, more pacer) (runStamps, bool) {
	var out runStamps
	t := ticker{more: more}
	i := 0
	for _, id := range sorted {
		if !t.tick() {
			return nil, false
		}
		for i < len(r) && r[i].first+r[i].count <= id {
			if !t.tick() {
				return nil, false
			}
			i++
		}
		if i < len(r) && r[i].first <= id {
			out.push(stampRun{id, 1, r[i].seq})
		}
	}
	return out, t.done()
}

// stamped returns the ids of sorted, which ascend, as runs of one stamp.
// Each id is a step on more, as in the other walks of an axis.
func stamped(sorted []uint64, seq uint64, more pacer) (runStamps, bool) {
	var out runStamps
	t := ticker{more: more}
	for i, id := range sorted {
		if !t.tick() {
			return nil, false
		}
		if i > 0 && id == sorted[i-1]+1 {
			out[len(out)-1].count++
			continue
		}
		out = append(out, stampRun{id, 1, seq})
	}
	return out, t.done()
}

// bornAfter returns the born stamps of the ids of sorted, which ascend, for
// a rollback whose sequence is seq: an id that old holds keeps its stamp
// there, and every other id gets seq. old is nil for a tab that is not
// live. It walks both once, with a step of pace for each id and run.
func bornAfter(sorted []uint64, old *axis, seq uint64, pace *loadPace) (runStamps, bool) {
	var out runStamps
	i := 0
	for _, id := range sorted {
		if !pace.step(1) {
			return nil, false
		}
		born := seq
		if old != nil {
			if _, live := old.pos[id]; live {
				for i < len(old.born) && old.born[i].first+old.born[i].count <= id {
					if !pace.step(1) {
						return nil, false
					}
					i++
				}
				if i < len(old.born) && old.born[i].first <= id {
					born = old.born[i].seq
				}
			}
		}
		out.push(stampRun{id, 1, born})
	}
	return out, true
}

// sortedIDs returns ids in ascending order: ids itself where they are, and
// else a sorted copy. Each place the sort writes counts on more, as the
// sort of a row's positions counts them (txn.sortPositions).
func sortedIDs(ids []uint64, more pacer) ([]uint64, bool) {
	t := ticker{more: more}
	sorted := true
	for i := 1; i < len(ids) && sorted; i++ {
		if !t.tick() {
			return nil, false
		}
		sorted = ids[i-1] < ids[i]
	}
	if !t.done() {
		return nil, false
	}
	if sorted {
		return ids, true
	}
	out := slices.Clone(ids)
	ok := mergeSort(out, func(a, b uint64) bool { return a < b }, func(writes int) bool {
		return more == nil || more(paceSteps*writes)
	})
	return out, ok
}

// axis is the rows or the columns of one tab: the live ids in position
// order, the position of each, and their born stamps.
type axis struct {
	ids  []uint64
	pos  map[uint64]int32
	born runStamps
}

func newAxis(first uint64, n int, seq uint64) axis {
	a := axis{ids: make([]uint64, n), pos: make(map[uint64]int32, n)}
	for i := range a.ids {
		a.ids[i] = first + uint64(i)
		a.pos[first+uint64(i)] = int32(i)
	}
	a.born.set(first, uint64(n), seq)
	return a
}

func (a *axis) len() int { return len(a.ids) }

func (a *axis) reindex(from int) { a.place(from, nil) }

// place gives the ids from position from on their positions. It returns
// false when more ends it, with positions still to give.
func (a *axis) place(from int, more pacer) bool {
	t := ticker{more: more}
	for i := from; i < len(a.ids); i++ {
		if !t.tick() {
			return false
		}
		a.pos[a.ids[i]] = int32(i)
	}
	return t.done()
}

// put puts ids, which are not live, before position at, with born as the
// born stamps of the whole axis afterwards (with). A put that more ends
// leaves the ids and the stamps in place and some positions not given:
// take, the undo of put, puts the axis back from there as from a whole
// put.
func (a *axis) put(at int, ids []uint64, born runStamps, more pacer) bool {
	a.ids = slices.Insert(a.ids, at, ids...)
	a.born = born
	return a.place(at, more)
}

// take takes the n ids from position at out, with born as the born stamps
// of the whole axis afterwards (without). gone is a copy of the ids it
// takes. Like put it leaves positions behind when more ends it, and put
// puts the axis back from there.
func (a *axis) take(at int, gone []uint64, born runStamps, more pacer) bool {
	a.ids = slices.Delete(a.ids, at, at+len(gone))
	a.born = born
	t := ticker{more: more}
	for _, id := range gone {
		if !t.tick() {
			return false
		}
		delete(a.pos, id)
	}
	return t.done() && a.place(at, more)
}

// compacted returns a copy of the axis whose arrays and map are made for
// what it holds now. One that grew and shrank keeps the arrays and the map
// of its largest size, and a saved tab keeps them as long as its entry
// (txn.compactSaved).
func (a *axis) compacted(t *ticker) (axis, bool) {
	pos, ok := compactMap(a.pos, t)
	if !ok || !t.add(len(a.ids)+len(a.born)) {
		return axis{}, false
	}
	return axis{ids: slices.Clone(a.ids), pos: pos, born: slices.Clone(a.born)}, true
}

// compactMap returns a copy of m made for its entries. A map keeps the
// room of the most entries it ever held after they are deleted.
func compactMap[K comparable, V any](m map[K]V, t *ticker) (map[K]V, bool) {
	if m == nil {
		return nil, true
	}
	out := make(map[K]V, len(m))
	for k, v := range m {
		if !t.tick() {
			return nil, false
		}
		out[k] = v
	}
	return out, true
}

// insert is put whole, for the undo of a journal, which runs to its end.
// It gives the ids the stamps of add, runs that are sorted by id.
func (a *axis) insert(at int, ids []uint64, add runStamps) {
	if a.born.after(add) {
		born := a.born
		for _, run := range add {
			born.push(run)
		}
		a.put(at, ids, born, nil)
		return
	}
	born, _ := a.born.with(add, nil)
	a.put(at, ids, born, nil)
}

// forRuns calls fn for each run of consecutive ascending ids, in order.
func forRuns(ids []uint64, fn func(first, count uint64)) {
	for i := 0; i < len(ids); {
		j := i + 1
		for j < len(ids) && ids[j] == ids[j-1]+1 {
			j++
		}
		fn(ids[i], uint64(j-i))
		i = j
	}
}

// idRuns returns ids as [first, count] pairs, the wire and file form, in
// an array of their number: an undo entry can keep it as long as it lives
// (undoGap).
func idRuns(ids []uint64) [][2]uint64 {
	n := 0
	forRuns(ids, func(uint64, uint64) { n++ })
	runs := make([][2]uint64, 0, n)
	forRuns(ids, func(first, count uint64) { runs = append(runs, [2]uint64{first, count}) })
	return runs
}

func encodeIDs(ids []uint64) []byte {
	var out []byte
	forRuns(ids, func(first, count uint64) {
		out = binary.AppendUvarint(out, first)
		out = binary.AppendUvarint(out, count)
	})
	return out
}

// decodeIDs reads id runs, with a step of pace for each id.
func decodeIDs(data []byte, limit int, pace *loadPace) ([]uint64, error) {
	var ids []uint64
	for len(data) > 0 {
		first, n := binary.Uvarint(data)
		if n <= 0 {
			return nil, fmt.Errorf("sheet.db id runs are damaged")
		}
		count, m := binary.Uvarint(data[n:])
		// The ids of a run lie below maxID, its last one included.
		if m <= 0 || count == 0 || first == 0 || first >= maxID || count > uint64(limit-len(ids)) || count > maxID-first {
			return nil, fmt.Errorf("sheet.db id runs are damaged")
		}
		data = data[n+m:]
		for i := uint64(0); i < count; i++ {
			if !pace.step(1) {
				return nil, errLoadStopped
			}
			ids = append(ids, first+i)
		}
	}
	return ids, nil
}

func encodeStamps(r runStamps) []byte {
	var out []byte
	for _, run := range r {
		out = binary.AppendUvarint(out, run.first)
		out = binary.AppendUvarint(out, run.count)
		out = binary.AppendUvarint(out, run.seq)
	}
	return out
}

// decodeStamps reads born stamps, with a step of pace for each run. Each
// run holds a live id, so an axis of limit ids has at most limit runs, and
// the blob is read no further.
func decodeStamps(data []byte, limit int, pace *loadPace) (runStamps, error) {
	var r runStamps
	for len(data) > 0 {
		if !pace.step(1) {
			return nil, errLoadStopped
		}
		if len(r) == limit {
			return nil, fmt.Errorf("sheet.db axis has born stamps for ids that are not live")
		}
		var v [3]uint64
		for i := range v {
			x, n := binary.Uvarint(data)
			if n <= 0 {
				return nil, fmt.Errorf("sheet.db born stamps are damaged")
			}
			v[i], data = x, data[n:]
		}
		// A run names live ids, so it is not empty and its ids lie from 1
		// to maxID-1. The bound also keeps first+count from wrapping.
		if v[0] == 0 || v[1] == 0 || v[0] >= maxID || v[1] > maxID-v[0] {
			return nil, fmt.Errorf("sheet.db born stamps are damaged")
		}
		if len(r) > 0 && r[len(r)-1].first+r[len(r)-1].count > v[0] {
			return nil, fmt.Errorf("sheet.db born stamps are out of order")
		}
		r = append(r, stampRun{v[0], v[1], v[2]})
	}
	return r, nil
}

// loadPace is the pace of a load or a revision build between two check
// points: it calls pause once for every loadCheckCells units of work, and
// reports false once pause did. With a nil pause it never stops the work.
type loadPace struct {
	pause func() bool
	n     int
}

func (p *loadPace) step(units int) bool {
	if p.pause == nil {
		return true
	}
	if p.n += units; p.n < loadCheckCells {
		return true
	}
	p.n = 0
	return p.pause()
}

// errLoadStopped ends a load of sheet.db whose pause reported false.
var errLoadStopped = errors.New("sheet.db load stopped")

// loadAxis builds an axis from its stored blobs and checks that the stamps
// name exactly the live ids, each of which lies below the counter. Each id
// it reads and places, and each stamp run it reads, is a step of pace,
// which the axes of a load share; it stops the work when pace does.
func loadAxis(ids, born []byte, limit int, next uint64, pace *loadPace) (axis, error) {
	list, err := decodeIDs(ids, limit, pace)
	if err != nil {
		return axis{}, err
	}
	if len(list) == 0 {
		return axis{}, fmt.Errorf("sheet.db axis is empty")
	}
	// Runs are sorted and apart, and each holds an id, so they name the
	// live ids and no other exactly when every live id has a stamp and
	// the runs hold as many ids as the axis. A run for an id that is not
	// live would meet the stamp of that id when it is given out.
	stamps, err := decodeStamps(born, len(list), pace)
	if err != nil {
		return axis{}, err
	}
	a := axis{ids: list, pos: make(map[uint64]int32, len(list)), born: stamps}
	stamped := uint64(0)
	for i, id := range list {
		if !pace.step(1) {
			return axis{}, errLoadStopped
		}
		if _, dup := a.pos[id]; dup || id >= next {
			return axis{}, fmt.Errorf("sheet.db axis holds id %d twice or above its counter", id)
		}
		// Every stamp the engine writes is a sequence, which starts at 1.
		if stamps.get(id) == 0 {
			return axis{}, fmt.Errorf("sheet.db axis holds id %d without a born stamp", id)
		}
		if i < len(stamps) {
			stamped += stamps[i].count
		}
		a.pos[id] = int32(i)
	}
	if stamped != uint64(len(list)) {
		return axis{}, fmt.Errorf("sheet.db axis has born stamps for ids that are not live")
	}
	return a, nil
}
