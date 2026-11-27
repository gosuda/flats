package sheet

import (
	"slices"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/gosuda/flats/internal/sheet/formula"
)

type cellRef struct {
	tab *Tab
	row RowID
	col ColID
}

// jentry is one reversible change of an action: a cell record with the
// value before it, or a function that puts anything else back.
type jentry struct {
	fn    func()
	ref   cellRef
	had   bool
	fresh bool // the entry put the record on the action's list of records to stage
	old   Cell
}

// journal is the change journal of the write path (technical design,
// section 6): every change of an action with the value before it.
//
// The entries lie in chunks of journalChunk, so that a long journal grows
// without being copied and holds no more than it counts. bytes is what the
// action holds until it ends: the entries, and what they or the action's
// own lists keep alive that the sheet did not hold before the action
// (txn.keep). It is bounded at journalMax.
type journal struct {
	chunks [][]jentry
	n      int
	bytes  int64
	// start holds the counters of the sheet as the action found them, for
	// a journal that has an action (counted).
	start   counters
	counted bool
}

const (
	journalChunk = 1024
	// What the journal counts for an entry, for a record the action puts
	// on its list of records to stage, and for a record it creates: the
	// sizes of jentry, of a place in the list, which is a hash table that
	// is up to half empty, and of Cell with its slot in a row.
	journalEntryBytes  = 144
	journalStagedBytes = 72
	journalRecordBytes = 120
	// journalMax bounds what one action holds until it ends (technical
	// design, section 6): for every record a sheet may hold, with content,
	// a format or only its stamps, two entries, one mark for staging and
	// one new record. That is one operation that creates or replaces
	// every record, and a second pass over all of them.
	journalMax = (maxCells + maxBlankRecords) * (2*journalEntryBytes + journalStagedBytes + journalRecordBytes)
)

func (j *journal) len() int { return j.n }

func (j *journal) at(i int) *jentry { return &j.chunks[i/journalChunk][i%journalChunk] }

func (j *journal) add(e jentry) {
	if j.n == len(j.chunks)*journalChunk {
		// The first chunk grows with a small action; the others are made
		// whole.
		size := journalChunk
		if len(j.chunks) == 0 {
			size = 0
		}
		j.chunks = append(j.chunks, make([]jentry, 0, size))
	}
	last := &j.chunks[len(j.chunks)-1]
	*last = append(*last, e)
	j.n++
	j.bytes += journalEntryBytes
}

// push adds a function that takes a change back.
func (j *journal) push(fn func()) { j.pushHolding(fn, 0) }

// pushHolding is push for a function that holds extra bytes beyond a few
// words: saved ids, stamps or names.
func (j *journal) pushHolding(fn func(), extra int) {
	j.add(jentry{fn: fn})
	j.bytes += 64 + int64(extra)
}

// truncate drops the entries from mark on and lets go of what they hold.
func (j *journal) truncate(mark int) {
	keep := (mark + journalChunk - 1) / journalChunk
	clear(j.chunks[keep:])
	j.chunks = j.chunks[:keep]
	if rest := mark % journalChunk; rest != 0 {
		last := &j.chunks[keep-1]
		clear((*last)[rest:])
		*last = (*last)[:rest]
	}
	j.n = mark
}

// revert undoes every entry of the journal, newest first. The journal of
// an action puts the counters back as the action found them.
//
// The action gives its sequence back, and the next one takes it. The marks
// a recalculation left on the formulas whose value the journal kept name
// that sequence (fnode.kept), so they go too: the next action would count
// values it did not make.
func (wb *Workbook) revert(j *journal) {
	if j.counted {
		wb.revertTo(j, 0, &j.start)
	} else {
		wb.revertTo(j, 0, nil)
	}
	for _, t := range wb.tabs {
		for _, rd := range t.data {
			if rd.nf == 0 {
				continue
			}
			for _, s := range rd.cells {
				if n := s.c.node; n != nil {
					n.kept = 0
				}
			}
		}
	}
}

// revertTo undoes the entries after mark, newest first. was holds the
// counters of the sheet as they were at mark. With it the revert puts them
// back whole and weighs no record: it goes through no text to take away
// what the action counted. The count of the cells the formulas read is not
// among them: the index, which is stale from here on, counts it again.
func (wb *Workbook) revertTo(j *journal, mark int, was *counters) {
	if j.n == mark {
		return
	}
	if was != nil {
		wb.restoring = true
		defer func() {
			reads := wb.count.reads
			wb.count, wb.restoring = *was, false
			wb.count.reads = reads
		}()
	}
	for i := j.n - 1; i >= mark; i-- {
		e := j.at(i)
		if e.fn != nil {
			e.fn()
			continue
		}
		wb.revertCell(e)
	}
	j.truncate(mark)
	if mark == 0 {
		j.bytes = 0
	}
	wb.deps.stale = true
	wb.formulaTextOK = false
}

func (wb *Workbook) revertCell(e *jentry) {
	t := e.ref.tab
	c := t.cell(e.ref.row, e.ref.col)
	if c != nil {
		wb.weighCell(c, -1)
		if !e.had {
			t.drop(e.ref.row, e.ref.col)
			return
		}
		if c.f != nil {
			t.data[e.ref.row].nf--
		}
	} else {
		if !e.had {
			return
		}
		c, _ = t.ensure(e.ref.row, e.ref.col)
	}
	*c = e.old
	if c.f != nil {
		t.data[e.ref.row].nf++
	}
	wb.weighCell(c, 1)
}

// weighCell counts a record with everything that hangs on it: the limit
// counters, its style reference and its calculated text.
func (wb *Workbook) weighCell(c *Cell, sign int) {
	wb.styles.ref(c.style, sign)
	if wb.restoring {
		// A revert puts the counters back from what it saved (revertTo).
		return
	}
	wb.weigh(c, sign)
	if c.f != nil {
		wb.count.calcText += int64(sign) * int64(c.val.TextLen())
	}
}

func (wb *Workbook) weighRow(rd *rowData, sign int) {
	for _, s := range rd.cells {
		wb.weighCell(s.c, sign)
	}
}

func (wb *Workbook) weighTab(t *Tab, sign int) {
	for _, rd := range t.data {
		wb.weighRow(rd, sign)
	}
}

type tabTouch struct {
	meta, axes, created, deleted bool
	widths                       []ColID // columns whose width to stage; a column can repeat
	delRows                      []RowID
}

// touchSave is what a tab's touch was before a change of it, so that a
// reverted operation leaves no staging mark behind.
type touchSave struct {
	tab   *Tab
	fresh bool
	was   tabTouch
}

const (
	editInsert = iota + 1
	editDelete
	editSort
	editRename
	editTabAdd
	editTabDelete
	editTabMove
)

// structEdit is one structural edit of an action, as the delta, the
// structural log and the undo entry need it. The edits of an action stay
// in the order of its operations, renames and tab edits among the edits of
// rows and columns, because a formula in flight is adjusted by replaying
// them in that order (technical design, section 8.4).
type structEdit struct {
	tab       *Tab
	rows      bool
	kind      int
	ids       []uint64 // inserted or deleted ids, or the new ids of a sorted run
	after     uint64   // id before an insert, 0 at the start
	above     uint64   // delete: the id before the deleted ones, 0 at the start
	below     uint64   // delete: the id after them, 0 at the end
	positions []int32  // sort: the positions it wrote
	before    []uint64 // sort: the ids those positions held
	appended  bool
	tr        formula.Transform // a rename carries the earlier and the new name
}

// axisEdit reports whether the edit changed the rows or columns of its tab.
func (e *structEdit) axisEdit() bool { return e.kind <= editSort }

type overwrite struct {
	session  uint32
	ref      cellRef
	replaced uint64
}

// txn is one action or agent call on its way through the write path.
type txn struct {
	wb     *Workbook
	seq    uint64
	base   uint64
	by     uint32 // the acting session; 0 for an agent
	now    time.Time
	log    *structLog
	budget *formula.Budget
	spent  int // steps of the action's own work that are not yet a whole unit of the budget (spend)
	// overdue reads the clock against the deadline of the budget, and
	// passed says that stop found the deadline passed (Instance.begin).
	overdue func() bool
	passed  bool
	j       journal

	// cells holds the records to stage and to send, each with the
	// operation that last put it into the journal (epoch).
	cells map[cellRef]int32
	// epoch numbers the stretch of the action since its last checkpoint
	// (writer.go). A revert goes back to a checkpoint or to the start, so
	// one journal entry restores a record however often the stretch
	// writes it.
	epoch    int32
	tabs     map[*Tab]*tabTouch
	touches  []touchSave // every change of tabs, for the revert of one operation
	pending  []cellAt    // content written since the last recalculation
	dirtyAll bool        // the next recalculation covers every formula
	scan     bool        // limits look at every formula
	bulk     bool        // the action writes more cells than the index follows one by one
	tabList  bool        // the tab list changed
	order    bool        // the tab order changed
	changed  bool        // something applied
	values   []*fnode    // formulas whose value a recalculation changed
	structs  []structEdit
	steps    []undoStep
	repairs  []repair
	over     []overwrite
	fwrote   []cellRef // formulas written, for an agent's result
	nwrote   int       // cells written
	wrote    *Tab      // the tab of the cells written, nil when they lie in several
	passes   int
	wireMark int        // length of the wire format list before the action
	styleIDs int        // where the action's style ids begin in the table's log (styleTable.forgetFrom)
	entry    *undoEntry // the undo entry of a person's action
	run      *undoRun   // the undo or redo the action applies

	// recalls holds collected blank records the action has read, by cell,
	// with nil for a cell that has none, and recalled the cells whose new
	// record took one (undo_blanks.go). held is what the undo stacks hold,
	// read once, and blankErr an error of the store, which refuses the
	// action.
	recalls  map[cellID]*collectedBlank
	recalled []cellID
	held     *stampHolders
	blankErr error
	// replaced counts, for the stand-in notes of every session, the notes
	// whose group the action wrote, and expired holds the entries its own
	// notes pushed out of the undo limits (undo_notes.go). Both take
	// effect when the action has committed.
	replaced map[*noteSet]noteHits
	expired  []expiry

	calcTime time.Duration // time the action's recalculations took
	calcRan  bool
}

func newTxn(wb *Workbook, seq, base uint64, by uint32) *txn {
	x := &txn{wb: wb, seq: seq, base: base, by: by, cells: map[cellRef]int32{}, tabs: map[*Tab]*tabTouch{}, wireMark: len(wb.wire.list), styleIDs: wb.styles.mark()}
	x.j.start, x.j.counted = wb.count, true
	return x
}

// touch returns the staging marks of a tab for a change. Every call saves
// the marks as they were.
func (x *txn) touch(t *Tab) *tabTouch {
	tt := x.tabs[t]
	save := touchSave{tab: t, fresh: tt == nil}
	if tt == nil {
		tt = &tabTouch{}
		x.tabs[t] = tt
	} else {
		save.was = *tt
	}
	x.touches = append(x.touches, save)
	return tt
}

// note puts a record on the list of records to stage, under the stretch
// of the action in hand, and reports whether it was not there yet.
func (x *txn) note(ref cellRef) bool {
	_, known := x.cells[ref]
	x.cells[ref] = x.epoch
	if !known {
		x.j.bytes += journalStagedBytes
	}
	return !known
}

// journaled reports whether the journal already holds what the record was
// when the stretch of the action in hand began. Then it needs no further
// entry: reverting the stretch restores the record from that one.
func (x *txn) journaled(ref cellRef) bool {
	last, known := x.cells[ref]
	return known && last == x.epoch
}

// journalCell adds the entry of a record that is about to change. had is
// false for a record the action creates.
func (x *txn) journalCell(ref cellRef, c *Cell, had bool) {
	fresh := x.note(ref)
	x.j.add(jentry{ref: ref, had: had, old: *c, fresh: fresh})
	switch {
	case !had:
		x.j.bytes += journalRecordBytes
	case !fresh:
		// The first entry of a record holds what the sheet held before
		// the action. A later one may hold what the action made itself
		// and nothing else keeps.
		x.j.bytes += x.made(c)
	}
}

// made returns the bytes of what a record holds that the action made: text
// and a formula it wrote, and the text a recalculation of it gave.
func (x *txn) made(c *Cell) int64 {
	var n int64
	if c.seq == x.seq || c.textSeq == x.seq {
		if c.f != nil {
			n += formulaBytes(c.f)
		} else if k := c.content.Kind; k == formula.ContentText || k == formula.ContentUnreadable {
			n += int64(len(c.content.Text))
		}
	}
	if c.f != nil && c.node != nil && c.node.kept == x.seq {
		n += int64(len(c.val.Text()))
	}
	return n
}

// keep counts n bytes that the action holds until it ends beside the
// entries of its journal: ids, steps and values of its own lists.
func (x *txn) keep(n int) { x.j.bytes += int64(n) }

// journalOpBytes is what one operation holds whatever it does: its
// structural edit, its undo step and its staging marks.
const journalOpBytes = 1024

// stop returns why the action must not go on: its deadline has passed, or
// it holds more than the journal may. It is nil while the action may go
// on. Work that meets it ends, and the action is refused whole.
//
// stop reads the clock, so it also finds a deadline that passed while
// little was counted: time the action waited for storage, or work that
// takes longer than its count says. A loop that counts every step may ask
// late instead, which reads no clock.
func (x *txn) stop() *WriteError {
	if !x.late() && x.overdue != nil {
		x.passed = x.overdue()
	}
	if x.late() {
		return calcTimeout()
	}
	return x.fits()
}

// fits refuses an action that holds more than the journal may.
func (x *txn) fits() *WriteError {
	if x.j.bytes > journalMax {
		return capacity("action_changes", journalMax, x.j.bytes)
	}
	return nil
}

// update changes one cell record under the journal and keeps the counters.
func (x *txn) update(t *Tab, row RowID, col ColID, fn func(c *Cell)) *Cell {
	wb := x.wb
	c, created := t.ensure(row, col)
	if created {
		x.recall(t, row, col, c)
	}
	if ref := (cellRef{t, row, col}); !x.journaled(ref) {
		x.journalCell(ref, c, !created)
	}
	if !created {
		wb.weighCell(c, -1)
	}
	x.spend(64 + weighed(c))
	hadF := c.f != nil
	fn(c)
	x.spend(weighed(c))
	if hadF != (c.f != nil) {
		if hadF {
			t.data[row].nf--
		} else {
			t.data[row].nf++
		}
	}
	wb.weighCell(c, 1)
	x.changed = true
	return c
}

// weighed returns the bytes of text weighCell goes through for a record:
// typed text and the text a formula gives, whose code points it counts.
func weighed(c *Cell) int {
	n := 0
	if k := c.content.Kind; k == formula.ContentText || k == formula.ContentUnreadable {
		n = len(c.content.Text)
	}
	if c.f != nil {
		n += len(c.val.Text())
	}
	return n
}

// setContent writes what a cell holds as a writer's edit. f is the parsed
// formula of formula content, already adjusted to the cell.
func (x *txn) setContent(t *Tab, row RowID, col ColID, content formula.Content, f *formula.Formula) {
	wb := x.wb
	if f != nil {
		f = wb.intern(f)
		content = formulaContent(f)
	}
	// A person's content that a writer replaces without having seen it, and
	// any person's content an agent replaces, is reported to its author
	// (section 7.2).
	if old := t.cell(row, col); old != nil && old.by != x.by && old.by != 0 && !old.blank() && (x.by == 0 || old.seq > x.base) {
		x.keep(40)
		x.over = append(x.over, overwrite{old.by, cellRef{t, row, col}, old.seq})
	}
	x.update(t, row, col, func(c *Cell) {
		x.setFormula(t, row, col, c, f)
		c.content, c.seq, c.by = content, x.seq, x.by
		if f == nil {
			c.val = content.Value()
		}
	})
	x.mark(t, row, col)
	x.keep(16)
	x.pending = append(x.pending, cellAt{t, x.posOf(t, row, col)})
	if x.nwrote == 0 {
		x.wrote = t
	} else if x.wrote != t {
		x.wrote = nil
	}
	x.nwrote++
	if f != nil {
		x.keep(24)
		x.fwrote = append(x.fwrote, cellRef{t, row, col})
	}
	if x.nwrote > fullDirtyCells {
		x.dirtyAll, wb.deps.stale = true, true
	}
}

// setFormula keeps the dependency node of a cell in step with its formula.
func (x *txn) setFormula(t *Tab, row RowID, col ColID, c *Cell, f *formula.Formula) {
	wb := x.wb
	had := c.f != nil
	// An action known to pass fullDirtyCells recalculates every formula,
	// which builds the index again. Entries made on the way would be thrown
	// away, and the build checks the read limits before it makes any.
	if x.bulk {
		wb.deps.stale = true
	}
	if c.node != nil && !wb.deps.stale {
		wb.unregister(c.node)
		if f == nil {
			wb.dropDead(x.budget)
		}
	}
	c.f = f
	if f == nil {
		c.node = nil
		return
	}
	if !had {
		// The value a formula cell had as plain content is not a result.
		c.val = formula.Value{}
	}
	if c.node == nil {
		c.node = &fnode{tab: t, row: row, col: col, cell: c}
	}
	if !wb.deps.stale {
		wb.register(c.node, x.budget)
	}
}

func (x *txn) posOf(t *Tab, row RowID, col ColID) formula.Pos {
	p, _ := t.posOf(row, col)
	return p
}

// rewrite stores the formula a structural edit or a rename gave a cell. It
// sets the text stamp and leaves the content stamp alone (section 4.1).
func (x *txn) rewrite(ref cellRef, f *formula.Formula) {
	f = x.wb.intern(f)
	x.update(ref.tab, ref.row, ref.col, func(c *Cell) {
		x.setFormula(ref.tab, ref.row, ref.col, c, f)
		c.content, c.textSeq = formulaContent(f), x.seq
	})
}

// clearContent blanks a cell and keeps its record for the stamps.
func (x *txn) clearContent(t *Tab, row RowID, col ColID) {
	c := t.cell(row, col)
	if c == nil || (c.blank() && c.seq == 0 && c.style == 0) {
		return
	}
	x.setContent(t, row, col, formula.Content{}, nil)
}

// setStyle writes the properties of a patch on a cell.
func (x *txn) setStyle(t *Tab, row RowID, col ColID, p formatPatch) {
	wb := x.wb
	x.update(t, row, col, func(c *Cell) {
		state := p.apply(wb.styles.state(c.style), x.seq)
		x.wireIndex(state.f)
		c.style = wb.styles.intern(state)
	})
	x.mark(t, row, col)
}

func (x *txn) removeCell(t *Tab, row RowID, col ColID) *Cell {
	c := t.cell(row, col)
	if c == nil {
		return nil
	}
	if ref := (cellRef{t, row, col}); !x.journaled(ref) {
		x.journalCell(ref, c, true)
	}
	x.wb.weighCell(c, -1)
	t.drop(row, col)
	return c
}

func (x *txn) detachRow(t *Tab, row RowID) *rowData {
	rd := t.data[row]
	if rd == nil {
		return nil
	}
	wb := x.wb
	x.spend(rowSteps(rd))
	wb.weighRow(rd, -1)
	t.dropRow(row)
	t.byPosOK = false
	x.j.push(func() {
		t.data[row] = rd
		t.byPosOK = false
		wb.weighRow(rd, 1)
	})
	return rd
}

// rowSteps is the work of counting the records of a row in or out
// (weighRow): a unit for each record and the bytes of the text it holds.
func rowSteps(rd *rowData) int {
	n := 64 * len(rd.cells)
	for _, s := range rd.cells {
		n += weighed(s.c)
	}
	return n
}

func (x *txn) attachRow(t *Tab, row RowID, rd *rowData) {
	wb := x.wb
	t.data[row] = rd
	t.byPosOK = false
	x.spend(rowSteps(rd))
	wb.weighRow(rd, 1)
	var fresh []cellRef
	for _, s := range rd.cells {
		if ref := (cellRef{t, row, s.col}); x.note(ref) {
			fresh = append(fresh, ref)
		}
	}
	x.j.push(func() {
		wb.weighRow(rd, -1)
		t.dropRow(row)
		t.byPosOK = false
		for _, ref := range fresh {
			delete(x.cells, ref)
		}
	})
}

// structural notes that rows, columns or tabs changed: the index is rebuilt
// and every formula recalculated (section 10.2).
func (x *txn) structural() {
	x.wb.deps.stale = true
	x.dirtyAll, x.scan, x.changed = true, true, true
}

// transform rewrites every formula of the sheet for a structural edit,
// before the edit changes the axis. A formula that would render longer than
// the limit refuses the edit.
func (x *txn) transform(tr formula.Transform) *WriteError {
	wb := x.wb
	type change struct {
		ref cellRef
		f   *formula.Formula
	}
	var changes []change
	var bad *WriteError
	deletes := tr.Kind == formula.DeleteRows || tr.Kind == formula.DeleteCols
	done := wb.eachFormulaPaced(x.budget, func(t *Tab, row RowID, col ColID, pos formula.Pos, c *Cell) bool {
		// Once the deadline has passed, or the action holds more than its
		// journal may, no further formula is gone through.
		if bad = x.stop(); bad != nil {
			return false
		}
		if _, alive := tr.Map(t.name, pos); !alive {
			return true
		}
		nf, npos, changed := c.f.Apply(t.name, pos, tr)
		x.rewrote(c.f, nf)
		if changed {
			changes = append(changes, change{cellRef{t, row, col}, nf})
			if deletes {
				tabs := wb.tabsRead(t, c.f, pos)
				x.keep(int(unsafe.Sizeof(repair{})) + 8*len(tabs))
				x.repairs = append(x.repairs, repair{tab: t.id, row: row, col: col, before: c.f, tabs: tabs})
			}
		}
		if changed || len(nf.Stored()) > formula.MaxFormulaChars/2 {
			x.spend(len(nf.Stored()))
			if n := nf.TextLen(npos); n > formula.MaxFormulaChars {
				bad = capacity("formula_length", formula.MaxFormulaChars, int64(n))
			}
		}
		return bad == nil
	})
	if bad != nil {
		return bad
	}
	if !done {
		return calcTimeout()
	}
	if len(changes) > 0 {
		// The edit that follows makes the index stale (structural), and
		// the next recalculation builds it again. Until then every
		// formula written here would be entered into it by itself, and
		// entering one goes through the others that read the same cell:
		// all of them each time, for a cell that every formula reads.
		wb.deps.stale = true
	}
	for _, ch := range changes {
		// Each formula written again is counted where it is written
		// (update). The journal takes back the ones before it.
		if err := x.stop(); err != nil {
			return err
		}
		x.rewrite(ch.ref, ch.f)
	}
	return nil
}

// tabsRead returns the ids of the tabs a formula reads, its own included.
func (wb *Workbook) tabsRead(own *Tab, f *formula.Formula, at formula.Pos) []TabID {
	ids := []TabID{own.id}
	f.Refs(at, func(r formula.Ref) {
		if r.Tab == "" {
			return
		}
		if t := wb.tab(r.Tab); t != nil && !slices.Contains(ids, t.id) {
			ids = append(ids, t.id)
		}
	})
	return ids
}

func (x *txn) axisOf(t *Tab, rows bool) *axis {
	if rows {
		return &t.rows
	}
	return &t.cols
}

// axisEdit journals a structural edit of an axis before it is made: what
// it changes beside the axis, and undo, which takes the edit itself back.
// An insert is undone by removing what it put in, a deletion by putting
// its ids back with their stamps, a sort by writing the earlier ids into
// its positions. The journal therefore holds what the edit moved, held
// bytes of it, and never a copy of the axis. The deadline is looked at
// before anything is journaled: an edit it refuses changes nothing.
func (x *txn) axisEdit(t *Tab, rows bool, held int, undo func(a *axis)) *WriteError {
	wb := x.wb
	a := x.axisOf(t, rows)
	// The edit goes through the axis to place its ids, eight bytes each.
	x.spend(8 * a.len())
	if err := x.stop(); err != nil {
		return err
	}
	next, structSeq := [2]uint64{wb.nextRow, wb.nextCol}, t.structSeq
	x.j.pushHolding(func() {
		undo(a)
		wb.nextRow, wb.nextCol, t.structSeq = next[0], next[1], structSeq
		t.byPosOK = false
	}, held)
	tt := x.touch(t)
	tt.meta, tt.axes = true, true
	t.byPosOK = false
	return nil
}

// pace is the pacer of the action's own bookkeeping: each step counts on
// the action's budget, and a loop ends once the deadline has passed. It
// reads the clock at each call, which a loop makes every paceEvery ids or
// runs: the steps of that many reach the budget's own look at the clock
// only every 32,768 of them.
func (x *txn) pace(steps int) bool {
	x.spend(steps)
	if !x.late() && x.overdue != nil {
		x.passed = x.overdue()
	}
	return !x.late()
}

// putIDs puts ids, which are not live, into an axis before position at,
// with the born stamps of add, runs sorted by id: an insert, or the undo
// of a deletion. Its bookkeeping goes through the axis and the stamps once
// and ends at the deadline (technical design, section 10.4). The stamps
// are merged before the edit is journaled, so a deadline there changes
// nothing; one that ends the edit later leaves positions to give, and the
// undo puts the axis back from there.
func (x *txn) putIDs(t *Tab, rows bool, at int, ids []uint64, add runStamps, held int) *WriteError {
	a := x.axisOf(t, rows)
	var born runStamps
	if len(add) > 1 || !a.born.after(add) {
		var ok bool
		if born, ok = a.born.with(add, x.pace); !ok {
			return calcTimeout()
		}
	}
	held += int(unsafe.Sizeof(stampRun{})) * len(add)
	if err := x.axisEdit(t, rows, held, func(a *axis) {
		rest, _ := a.born.without(add, nil)
		a.take(at, ids, rest, nil)
	}); err != nil {
		return err
	}
	if born == nil {
		// New ids at the end of the axis join its last run in place.
		born = a.born
		born.push(add[0])
	}
	if !a.put(at, ids, born, x.pace) {
		return calcTimeout()
	}
	return nil
}

// takeIDs takes the ids from position at out of an axis. gone is a copy of
// them that the edit keeps; they return with the stamps they had. Like
// putIDs it works out the stamps before the edit is journaled, and goes
// through the axis and the stamps once.
func (x *txn) takeIDs(t *Tab, rows bool, at int, gone []uint64, held int) *WriteError {
	a := x.axisOf(t, rows)
	sorted, ok := sortedIDs(gone, x.pace)
	var runs, rest runStamps
	if ok {
		runs, ok = a.born.stampsOf(sorted, x.pace)
	}
	if ok {
		rest, ok = a.born.without(runs, x.pace)
	}
	if !ok {
		return calcTimeout()
	}
	held += int(unsafe.Sizeof(stampRun{})) * len(runs)
	if err := x.axisEdit(t, rows, held, func(a *axis) { a.insert(at, gone, runs) }); err != nil {
		return err
	}
	if !a.take(at, gone, rest, x.pace) {
		return calcTimeout()
	}
	return nil
}

// insertAxis adds n rows or columns before position at and returns their
// new ids. An insert at the end adds no structure stamp (section 7.3).
func (x *txn) insertAxis(t *Tab, rows bool, at, n int) ([]uint64, *WriteError) {
	wb := x.wb
	a := x.axisOf(t, rows)
	limit, name, next, kind := formula.MaxCols, "columns", &wb.nextCol, formula.InsertCols
	if rows {
		limit, name, next, kind = formula.MaxRows, "rows", &wb.nextRow, formula.InsertRows
	}
	if n <= 0 {
		return nil, invalidf("count", "count is the number of %s, from 1", name)
	}
	if at < 0 || at > a.len() {
		return nil, invalidf("at", "the position is outside the tab")
	}
	if a.len()+n > limit {
		return nil, capacity(name, int64(limit), int64(a.len()+n))
	}
	if *next+uint64(n) >= maxID {
		return nil, unavailable("ids_exhausted")
	}
	edit := structEdit{tab: t, rows: rows, kind: editInsert, appended: at == a.len()}
	if !edit.appended {
		edit.tr = formula.Transform{Kind: kind, Tab: t.name, At: int32(at), Count: int32(n)}
		if err := x.transform(edit.tr); err != nil {
			return nil, err
		}
	}
	ids := make([]uint64, n)
	for i := range ids {
		ids[i] = *next + uint64(i)
	}
	if at > 0 {
		edit.after = a.ids[at-1]
	}
	// The new ids stay with the edit and its undo step.
	if err := x.putIDs(t, rows, at, ids, runStamps{{ids[0], uint64(n), x.seq}}, 8*n); err != nil {
		return nil, err
	}
	*next += uint64(n)
	if !edit.appended {
		t.structSeq = x.seq
	}
	edit.ids = ids
	x.structs = append(x.structs, edit)
	x.steps = append(x.steps, undoStep{kind: stepInserted, tab: t.id, rows: rows, ids: ids})
	x.structural()
	x.tabList = true
	return ids, nil
}

// growTo adds rows and columns at the end so that the tab holds the cell.
func (x *txn) growTo(t *Tab, rows, cols int) *WriteError {
	if n := rows - t.rows.len(); n > 0 {
		if _, err := x.insertAxis(t, true, t.rows.len(), n); err != nil {
			return err
		}
	}
	if n := cols - t.cols.len(); n > 0 {
		if _, err := x.insertAxis(t, false, t.cols.len(), n); err != nil {
			return err
		}
	}
	return nil
}

// deleteAxis removes the n rows or columns from position at with their
// cells. A tab keeps at least one row and one column.
func (x *txn) deleteAxis(t *Tab, rows bool, at, n int) *WriteError {
	a := x.axisOf(t, rows)
	switch {
	case n <= 0:
		return invalidf("count", "count is the number of rows or columns, from 1")
	case at < 0 || at >= a.len():
		return invalidf("at", "the position is outside the tab")
	case at+n > a.len():
		return invalidf("count", "the rows or columns to delete are outside the tab")
	}
	if n == a.len() {
		return invalidf("empty_tab", "a tab keeps at least one row and one column")
	}
	kind := formula.DeleteCols
	if rows {
		kind = formula.DeleteRows
	}
	edit := structEdit{tab: t, rows: rows, kind: editDelete, ids: slices.Clone(a.ids[at : at+n]),
		tr: formula.Transform{Kind: kind, Tab: t.name, At: int32(at), Count: int32(n)}}
	repaired := len(x.repairs)
	if err := x.transform(edit.tr); err != nil {
		return err
	}
	// The step owns the repairs of its transform, in an array of their
	// own: the next deletion's do not join it.
	step := undoStep{kind: stepDeleted, tab: t.id, rows: rows, ids: edit.ids, seq: x.seq, at: at, repairs: x.repairs[repaired:]}
	x.repairs = x.repairs[:repaired:repaired]
	if at > 0 {
		step.above = a.ids[at-1]
	}
	if at+n < a.len() {
		step.below = a.ids[at+n]
	}
	edit.above, edit.below = step.above, step.below
	tt := x.touch(t)
	if rows {
		for i, id := range edit.ids {
			if err := x.stop(); err != nil {
				return err
			}
			if rd := x.detachRow(t, RowID(id)); rd != nil {
				x.keep(int(unsafe.Sizeof(savedRow{})))
				step.saved = append(step.saved, savedRow{RowID(id), rd, int32(i)})
			}
			tt.delRows = append(tt.delRows, RowID(id))
		}
	} else {
		gone := make(map[ColID]bool, n)
		for _, id := range edit.ids {
			gone[ColID(id)] = true
			if w, ok := t.widths[ColID(id)]; ok {
				col := ColID(id)
				t.dropWidth(col)
				x.j.pushHolding(func() { t.widths[col] = w }, int(unsafe.Sizeof(savedWidth{})))
				step.widths = append(step.widths, savedWidth{col, w})
				tt.widths = append(tt.widths, col)
			}
		}
		for row, rd := range t.data {
			// Every record of the tab is looked at for its column, a
			// unit each. One that goes counts the bytes of its text as
			// well, which the counters go through.
			if err := x.stop(); err != nil {
				return err
			}
			x.spend(64 * (1 + len(rd.cells)))
			for i := len(rd.cells) - 1; i >= 0; i-- {
				if col := rd.cells[i].col; gone[col] {
					x.spend(weighed(rd.cells[i].c))
					x.keep(int(unsafe.Sizeof(savedCell{})))
					step.cells = append(step.cells, savedCell{row, col, x.removeCell(t, row, col)})
				}
			}
		}
	}
	if err := x.takeSorts(t, &step); err != nil {
		return err
	}
	// The deleted ids are the edit's own copy.
	if err := x.takeIDs(t, rows, at, edit.ids, 8*n); err != nil {
		return err
	}
	t.structSeq = x.seq
	x.structs = append(x.structs, edit)
	x.steps = append(x.steps, step)
	x.structural()
	x.tabList = true
	return nil
}

// unfrozen returns the positions under the frozen rows. Frozen rows never
// move (spec rule 22), whichever rows a sort names.
func unfrozen(t *Tab, positions []int32) []int32 {
	for len(positions) > 0 && int(positions[0]) < t.frozenRows {
		positions = positions[1:]
	}
	return positions
}

// reorder writes ids into the row positions and moves the formulas of the
// rows that moved (spec rule 15). positions ascend. A frozen row keeps its
// position: it leaves the positions and the ids, and the other ids keep
// their order in the positions that remain.
//
// A sort stamps every row it orders, also when none of them moves: the
// order it confirmed is protected from older sort undos (Appendix B,
// engine item 16). An undo that finds its rows in the earlier order
// already stamps them as well, so that the person's older sort undos find
// stamps that stand in for theirs (engine item 23). A sort of no row that
// may move changes nothing.
func (x *txn) reorder(t *Tab, positions []int32, ids []uint64) *WriteError {
	if free := unfrozen(t, positions); len(free) < len(positions) {
		frozen := t.rows.ids[:min(t.frozenRows, len(t.rows.ids))]
		ids = slices.DeleteFunc(slices.Clone(ids), func(id uint64) bool { return slices.Contains(frozen, id) })
		positions = free
	}
	edit := structEdit{tab: t, rows: true, kind: editSort, positions: positions, ids: ids, before: make([]uint64, len(positions))}
	type move struct {
		ref cellRef
		f   *formula.Formula
	}
	var moves []move
	moved := false
	for i, p := range positions {
		// Each row is a unit, and nothing has moved yet.
		x.spend(64)
		if err := x.paced(); err != nil {
			return err
		}
		edit.before[i] = t.rows.ids[p]
		if ids[i] == edit.before[i] {
			continue
		}
		moved = true
		rd := t.data[RowID(ids[i])]
		if rd == nil || rd.nf == 0 {
			continue
		}
		// Nothing has moved yet: the deadline, or the bound of what the
		// action holds, refuses the sort whole.
		if err := x.stop(); err != nil {
			return err
		}
		from := t.rows.pos[ids[i]]
		for _, s := range rd.cells {
			if s.c.f == nil {
				continue
			}
			// A row can hold a long formula in every column: the deadline
			// is looked at before each.
			if err := x.paced(); err != nil {
				return err
			}
			col := t.cols.pos[uint64(s.col)]
			a, b := formula.Pos{Row: from, Col: col}, formula.Pos{Row: p, Col: col}
			nf := s.c.f.Moved(a, b)
			x.rewrote(s.c.f, nf)
			if nf != s.c.f || len(nf.Stored()) > formula.MaxFormulaChars/2 {
				x.spend(len(nf.Stored()))
				if n := nf.TextLen(b); n > formula.MaxFormulaChars {
					return capacity("formula_length", formula.MaxFormulaChars, int64(n))
				}
			}
			if nf != s.c.f {
				moves = append(moves, move{cellRef{t, RowID(ids[i]), s.col}, nf})
			}
		}
	}
	if len(positions) == 0 {
		return nil
	}
	// The sort is undone from what it holds anyway: its positions and the
	// ids they held. With the new ids, the stamps for the undo step and
	// the last sorts of the rows that is 45 bytes a row, and the entry of
	// the structural log holds 40 more for each (structlog.go). A sort
	// that moves nothing has no structural edit.
	before := edit.before
	if moved {
		if err := x.axisEdit(t, true, 85*len(positions), func(a *axis) {
			for i, p := range positions {
				a.ids[p] = before[i]
				a.pos[before[i]] = p
			}
		}); err != nil {
			return err
		}
	} else {
		x.keep(45 * len(positions))
	}
	if t.lastSort == nil {
		t.lastSort = map[RowID]sortStamp{}
	}
	// The two passes over the rows below are a unit a row each. The
	// journal takes back what the second wrote when the deadline ends it.
	stamps := make([]uint64, len(edit.before))
	for i, id := range edit.before {
		x.spend(64)
		if err := x.paced(); err != nil {
			return err
		}
		stamps[i] = t.lastSort[RowID(id)].seq
	}
	// The sort ordered every row it names, also those it left in place.
	olds, had := make([]sortStamp, len(ids)), make([]bool, len(ids))
	done := 0
	x.j.push(func() {
		for i, id := range ids[:done] {
			if had[i] {
				t.lastSort[RowID(id)] = olds[i]
			} else {
				t.dropSort(RowID(id))
			}
		}
	})
	for i, p := range positions {
		x.spend(64)
		if err := x.paced(); err != nil {
			return err
		}
		id := ids[i]
		if id != edit.before[i] {
			t.rows.ids[p] = id
			t.rows.pos[id] = p
		}
		row := RowID(id)
		olds[i], had[i] = t.lastSort[row]
		t.lastSort[row] = sortStamp{x.seq, x.by}
		done = i + 1
	}
	if !moved {
		// The stamps are the change: the action takes a sequence for them,
		// and its undo step finds the rows in this order.
		x.steps = append(x.steps, undoStep{kind: stepSorted, tab: t.id, ids: edit.before, seq: x.seq, stamps: stamps})
		x.changed = true
		return nil
	}
	if len(moves) > 0 {
		// The index is built again after the sort (transform).
		x.wb.deps.stale = true
	}
	for _, m := range moves {
		// The rows have moved. The journal takes the sort back if the
		// deadline, or the bound of what the action holds, ends it here.
		if err := x.stop(); err != nil {
			return err
		}
		x.rewrite(m.ref, m.f)
	}
	t.structSeq = x.seq
	x.structs = append(x.structs, edit)
	x.steps = append(x.steps, undoStep{kind: stepSorted, tab: t.id, ids: edit.before, seq: x.seq, stamps: stamps})
	x.structural()
	return nil
}

// spend counts work the action does itself, outside its recalculations, on
// the action's budget (technical design, section 10.4): n small steps, 64
// to a unit, which is how the evaluator counts the bytes of a text. A
// record, a formula or a comparison counts 64 steps and the bytes of the
// text that is gone through for it. The steps reach the budget, which
// checks the deadline, up to spendUnits units at a time.
func (x *txn) spend(n int) {
	x.spent += n
	if x.spent >= 64*spendUnits {
		x.settle()
	}
}

const spendUnits = 64

// settle adds the whole units of the steps counted so far to the budget.
func (x *txn) settle() {
	x.budget.Add(int64(x.spent >> 6))
	x.spent &= 63
}

// rewrote counts one Apply, Moved or Cut on f that returned g, as the
// catch-up of an undo counts it (followRun.rewrote): a formula that was
// written and read again is a unit and one more for each 16 bytes of its
// stored text, and one that was only looked through an eighth of a unit
// and one for each 256 bytes.
func (x *txn) rewrote(f, g *formula.Formula) {
	if g != f {
		x.spend(64 + 4*len(g.Stored()))
		return
	}
	x.spend(8 + len(f.Stored())/4)
}

// late reports whether the action is known to be past its deadline: the
// budget found so at one of its checks, or stop did. Work that meets it
// stops and refuses the action as a recalculation over its time does.
func (x *txn) late() bool { return x.passed || x.budget.Exceeded() }

// sortRows orders the rows at the given positions by the calculated values
// of one column (spec rule 22). The sort is stable and blank cells come
// last in both directions.
//
// Each comparison counts on the action's budget: a unit, and the bytes two
// texts have in common before they differ. Each place the sort writes is a
// quarter of a unit (mergeSort). Once the deadline has passed no further
// pair is compared and no place is written, and the sort is refused before
// a row moves.
func (x *txn) sortRows(t *Tab, positions []int32, by ColID, desc bool) *WriteError {
	if err := x.calc(); err != nil {
		return err
	}
	type keyed struct {
		id uint64
		v  formula.Value
	}
	rows := make([]keyed, len(positions))
	order := make([]int32, len(positions))
	for i, p := range positions {
		// Reading the key of a row is a unit.
		x.spend(64)
		if x.late() {
			return calcTimeout()
		}
		id := t.rows.ids[p]
		rows[i].id = id
		if c := t.cell(RowID(id), by); c != nil {
			rows[i].v = c.val
		}
		order[i] = int32(i)
	}
	// The steps are kept here and handed on a batch at a time, so that a
	// comparison of two numbers stays a few instructions.
	steps, late := 0, x.late()
	hand := func() {
		if steps >= 64*spendUnits {
			x.spend(steps)
			steps, late = 0, x.late()
		}
	}
	less := func(i, j int32) bool {
		if hand(); late {
			return false
		}
		a, b := rows[i].v, rows[j].v
		if a.IsBlank() || b.IsBlank() {
			steps += 64
			return !a.IsBlank() && b.IsBlank()
		}
		var d, walked int
		if a.Kind() == formula.KindText && b.Kind() == formula.KindText {
			d, walked = formula.CompareSortCounted(a, b)
		} else {
			d = formula.CompareSort(a, b)
		}
		steps += 64 + walked + 1
		if desc {
			return d > 0
		}
		return d < 0
	}
	if !late {
		mergeSort(order, less, func(writes int) bool {
			steps += 16 * writes
			hand()
			return !late
		})
	}
	x.spend(steps)
	if late {
		return calcTimeout()
	}
	if err := x.stop(); err != nil {
		return err
	}
	ids := make([]uint64, len(rows))
	for i, k := range order {
		ids[i] = rows[k].id
	}
	return x.reorder(t, positions, ids)
}

// sortPositions puts row positions in ascending order. Each place the sort
// writes is a unit, and the sort ends between two batches of them once the
// deadline has passed.
func (x *txn) sortPositions(positions []int32) *WriteError {
	mergeSort(positions, func(a, b int32) bool { return a < b }, func(writes int) bool {
		x.spend(64 * writes)
		return !x.late()
	})
	if x.late() {
		return calcTimeout()
	}
	return nil
}

// mergeBatch is how many places mergeSort writes between two calls of more.
const mergeBatch = 256

// mergeSort sorts s stably by less. It merges runs of doubling width
// through a buffer as long as s: each pass writes every element once, so a
// sort is about n log n comparisons and writes, and no step is longer than
// one of them. After each mergeBatch writes, and at the end, it calls more
// with the writes since the last call, and stops at once when more returns
// false. s then holds its elements in an order that is no result.
func mergeSort[E any](s []E, less func(a, b E) bool, more func(writes int) bool) bool {
	n := len(s)
	if n < 2 {
		return true
	}
	src, dst := s, make([]E, n)
	written := 0
	write := func() bool {
		if written++; written < mergeBatch {
			return true
		}
		ok := more(written)
		written = 0
		return ok
	}
	for width := 1; width < n; width *= 2 {
		for lo := 0; lo < n; lo += 2 * width {
			mid, hi := min(lo+width, n), min(lo+2*width, n)
			i, j := lo, mid
			for k := lo; k < hi; k++ {
				if i < mid && (j >= hi || !less(src[j], src[i])) {
					dst[k] = src[i]
					i++
				} else {
					dst[k] = src[j]
					j++
				}
				if !write() {
					return false
				}
			}
		}
		src, dst = dst, src
	}
	// After an odd number of passes the result is in the buffer.
	if &src[0] != &s[0] {
		for k := range s {
			s[k] = src[k]
			if !write() {
				return false
			}
		}
	}
	return more(written)
}

func (x *txn) setWidth(t *Tab, col ColID, px int) {
	old, had := x.putWidth(t, col, px)
	// One operation sets up to every width of a tab, each with a step of
	// its own. The list of steps grows by copying, so a step counts twice.
	x.keep(2 * int(unsafe.Sizeof(undoStep{})))
	x.steps = append(x.steps, undoStep{kind: stepWidth, tab: t.id, col: col, width: old, had: had, seq: x.seq})
}

// putWidth writes a width and returns the one it replaces. It records no
// undo step: setWidth does that for a width someone sets, and a width
// that returns with its column belongs to the step that brings the column
// back.
func (x *txn) putWidth(t *Tab, col ColID, px int) (old width, had bool) {
	old, had = t.widths[col]
	x.spend(64)
	t.widths[col] = width{px, x.seq}
	x.j.push(func() {
		if had {
			t.widths[col] = old
		} else {
			t.dropWidth(col)
		}
	})
	// A width the action wrote before is on the list to stage already,
	// so the list holds a column once however often the action sets it.
	if !had || old.seq != x.seq {
		x.keep(int(unsafe.Sizeof(touchSave{})) + 8)
		tt := x.touch(t)
		tt.widths = append(tt.widths, col)
	}
	x.changed = true
	return old, had
}

func (x *txn) setSettings(t *Tab, rows, cols int) {
	old := [2]int{t.frozenRows, t.frozenCols}
	oldSeq := t.settingsSeq
	t.frozenRows, t.frozenCols, t.settingsSeq = rows, cols, x.seq
	x.j.push(func() { t.frozenRows, t.frozenCols, t.settingsSeq = old[0], old[1], oldSeq })
	x.touch(t).meta = true
	x.steps = append(x.steps, undoStep{kind: stepSettings, tab: t.id, frozen: old, seq: x.seq, prev: oldSeq})
	x.tabList, x.changed = true, true
}

func (wb *Workbook) checkTabName(name string, except *Tab) *WriteError {
	if !validTabName(name) {
		return &WriteError{Code: "invalid", Reason: "tab_name", Name: name}
	}
	if other := wb.tab(name); other != nil && other != except {
		return &WriteError{Code: "invalid", Reason: "tab_name_taken", Name: name}
	}
	return nil
}

// addTab creates a tab at position at of the tab order.
func (x *txn) addTab(name string, at int) (*Tab, *WriteError) {
	wb := x.wb
	if err := wb.checkTabName(name, nil); err != nil {
		return nil, err
	}
	if len(wb.tabs) >= maxTabs {
		return nil, capacity("tabs", maxTabs, int64(len(wb.tabs)+1))
	}
	if wb.nextTab+1 >= maxID || wb.nextRow+newTabRows >= maxID || wb.nextCol+newTabCols >= maxID {
		return nil, unavailable("ids_exhausted")
	}
	next := [3]uint64{wb.nextTab, wb.nextRow, wb.nextCol}
	t := wb.newTab(name, newTabRows, newTabCols, x.seq)
	// The tab stays with the action if a later operation deletes it: its
	// ids and their positions.
	x.keep(48 * (newTabRows + newTabCols))
	order := wb.tabOrderSeq
	x.placeTab(t, at)
	x.j.push(func() { wb.nextTab, wb.nextRow, wb.nextCol = next[0], next[1], next[2] })
	x.steps = append(x.steps, undoStep{kind: stepTabAdded, tab: t.id, prev: order})
	return t, nil
}

// placeTab puts a tab into the workbook under the journal.
func (x *txn) placeTab(t *Tab, at int) {
	wb := x.wb
	order := wb.tabOrderSeq
	wb.placeTab(t, at)
	// The records of a tab that returns are gone through once to count
	// them in. That pass is counted before it starts.
	for _, rd := range t.data {
		x.spend(rowSteps(rd))
	}
	wb.weighTab(t, 1)
	// A tab that joins the tab bar is a write to the tab order.
	wb.tabOrderSeq = x.seq
	var fresh []cellRef
	for row, rd := range t.data {
		for _, s := range rd.cells {
			if ref := (cellRef{t, row, s.col}); x.note(ref) {
				fresh = append(fresh, ref)
			}
		}
	}
	x.j.push(func() {
		wb.weighTab(t, -1)
		wb.unplaceTab(t)
		wb.tabOrderSeq = order
		for _, ref := range fresh {
			delete(x.cells, ref)
		}
	})
	tt := x.touch(t)
	tt.created, tt.meta, tt.axes, tt.deleted = true, true, true, false
	for col := range t.widths {
		tt.widths = append(tt.widths, col)
	}
	x.structs = append(x.structs, structEdit{tab: t, kind: editTabAdd})
	x.structural()
	x.tabList = true
}

func (x *txn) renameTab(t *Tab, name string) *WriteError {
	wb := x.wb
	if err := wb.checkTabName(name, t); err != nil {
		return err
	}
	tr := formula.Transform{Kind: formula.RenameTab, Tab: t.name, Name: name}
	if err := x.transform(tr); err != nil {
		return err
	}
	old, oldSeq, change := t.name, t.nameSeq, wb.tabChangeSeq
	delete(wb.byName, formula.FoldName(old))
	t.name, t.nameSeq, wb.tabChangeSeq = name, x.seq, x.seq
	wb.byName[formula.FoldName(name)] = t
	x.j.push(func() {
		delete(wb.byName, formula.FoldName(name))
		t.name, t.nameSeq, wb.tabChangeSeq = old, oldSeq, change
		wb.byName[formula.FoldName(old)] = t
	})
	x.touch(t).meta = true
	x.structs = append(x.structs, structEdit{tab: t, kind: editRename, tr: tr})
	x.steps = append(x.steps, undoStep{kind: stepTabName, tab: t.id, name: old, seq: x.seq, prev: oldSeq})
	x.structural()
	x.tabList = true
	return nil
}

func (x *txn) moveTab(t *Tab, at int) {
	wb := x.wb
	order, seq := slices.Clone(wb.tabs), wb.tabOrderSeq
	x.keep(16 * len(order))
	from := wb.tabIndex(t)
	wb.tabs = slices.Delete(wb.tabs, from, from+1)
	at = min(at, len(wb.tabs))
	wb.tabs = slices.Insert(wb.tabs, at, t)
	wb.tabOrderSeq = x.seq
	x.j.push(func() { wb.tabs, wb.tabOrderSeq = order, seq })
	ids := make([]TabID, len(order))
	for i, o := range order {
		ids[i] = o.id
	}
	x.steps = append(x.steps, undoStep{kind: stepTabOrder, order: ids, seq: x.seq, prev: seq})
	x.structs = append(x.structs, structEdit{tab: t, kind: editTabMove})
	x.tabList, x.order, x.changed = true, true, true
}

func (x *txn) deleteTab(t *Tab) *WriteError {
	wb := x.wb
	if len(wb.tabs) == 1 {
		return invalidf("last_tab", "a sheet keeps at least one tab")
	}
	at, change, order := wb.tabIndex(t), wb.tabChangeSeq, wb.tabOrderSeq
	before := make([]TabID, len(wb.tabs))
	for i, o := range wb.tabs {
		before[i] = o.id
	}
	x.keep(8 * len(before))
	// The records of the tab are gone through once to count them out.
	// That pass is counted before it starts, so the deadline refuses the
	// deletion with nothing changed.
	for _, rd := range t.data {
		x.spend(rowSteps(rd))
		if x.late() {
			return calcTimeout()
		}
	}
	if x.by != 0 {
		// A person's undo entry keeps the tab.
		if err := x.compactSaved(t); err != nil {
			return err
		}
	}
	wb.weighTab(t, -1)
	wb.unplaceTab(t)
	wb.tabChangeSeq, wb.tabOrderSeq = x.seq, x.seq
	x.j.push(func() {
		wb.placeTab(t, at)
		wb.weighTab(t, 1)
		wb.tabChangeSeq, wb.tabOrderSeq = change, order
	})
	tt := x.touch(t)
	tt.deleted = true
	x.structs = append(x.structs, structEdit{tab: t, kind: editTabDelete})
	x.steps = append(x.steps, undoStep{kind: stepTabDeleted, tab: t.id, savedTab: t, at: at, order: before, prev: order})
	x.structural()
	x.tabList = true
	return nil
}

// compactSaved gives a tab that an undo entry is about to keep arrays and
// maps of its own size. A tab that grew and shrank keeps, in the arrays of
// its axes and in its maps, the room of the most it ever held, and the
// entry counts a saved tab by what it holds (undoEntry.measure). The
// copies are made first, each entry and id a step on the action's budget,
// and replace the tab's own when all are made: a deadline before that
// changes nothing, and the copies hold what the tab held.
func (x *txn) compactSaved(t *Tab) *WriteError {
	tk := ticker{more: x.pace}
	rows, ok := t.rows.compacted(&tk)
	var cols axis
	var widths map[ColID]width
	var data map[RowID]*rowData
	var marks, colMarks map[uint64]writeMark
	var lastSort map[RowID]sortStamp
	if ok {
		cols, ok = t.cols.compacted(&tk)
	}
	if ok {
		widths, ok = compactMap(t.widths, &tk)
	}
	if ok {
		data, ok = compactMap(t.data, &tk)
	}
	if ok {
		marks, ok = compactMap(t.marks, &tk)
	}
	if ok {
		colMarks, ok = compactMap(t.colMarks, &tk)
	}
	if ok {
		lastSort, ok = compactMap(t.lastSort, &tk)
	}
	if !ok || !tk.add(len(t.chunks)) || !tk.done() {
		return calcTimeout()
	}
	t.rows, t.cols, t.widths, t.data, t.marks, t.colMarks, t.lastSort = rows, cols, widths, data, marks, colMarks, lastSort
	t.chunks = slices.Clone(t.chunks)
	t.byPos, t.byPosOK = nil, false
	return nil
}

// calc recalculates what the action changed so far and applies the result
// under the journal. It refuses when the time limit passes, in the pass or
// in the operations before it, and when the formulas read more cells than
// a read limit allows.
func (x *txn) calc() *WriteError {
	wb := x.wb
	// The work the operations counted so far is part of the same budget.
	x.settle()
	if err := x.stop(); err != nil {
		return err
	}
	if !x.dirtyAll && len(x.pending) == 0 && !wb.deps.stale {
		return nil
	}
	for _, t := range wb.tabs {
		t.rowsByPos()
	}
	out, err := wb.recalc(calcReq{all: x.dirtyAll, cells: x.pending, budget: x.budget})
	x.calcTime, x.calcRan = x.calcTime+out.took, true
	if err != nil {
		return err
	}
	changed := wb.apply(out, &x.j, x.seq)
	x.keep(8 * len(changed))
	x.values = append(x.values, changed...)
	x.passes += out.passes
	x.dirtyAll, x.pending = false, x.pending[:0]
	// The journal keeps the values the pass replaced. After the last pass
	// of an action no operation follows that would look at the bound.
	return x.fits()
}

// checkText refuses text content over the limit of one cell.
func checkText(s string) *WriteError {
	if n := utf8.RuneCountInString(s); n > formula.MaxTextLen {
		return capacity("text_length", formula.MaxTextLen, int64(n))
	}
	return nil
}
