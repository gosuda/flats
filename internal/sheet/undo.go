package sheet

import (
	"errors"
	"maps"
	"math"
	"math/bits"
	"slices"

	"github.com/gosuda/flats/internal/sheet/formula"
)

const maxUndoEntries = 100

const (
	stepInserted = iota + 1
	stepDeleted
	stepSorted
	stepWidth
	stepSettings
	stepTabName
	stepTabOrder
	stepTabAdded
	stepTabDeleted
)

// Stamp groups of an undo guard besides the cell content (-1) and the nine
// format properties.
const (
	groupContent  = -1
	groupWidth    = 20
	groupSettings = 21
	groupName     = 22
	groupOrder    = 23
	groupSort     = 24 // the last sort of one row
)

type savedRow struct {
	id  RowID
	rd  *rowData
	pos int32 // the row's place among the rows its step deleted
}

type savedCell struct {
	row RowID
	col ColID
	c   *Cell
}

type savedWidth struct {
	col ColID
	w   width
}

// repair is a formula that a deletion changed: undoing the deletion gives
// it this stored form back when nothing touched it since (spec rule 14).
type repair struct {
	tab    TabID
	row    RowID
	col    ColID
	before *formula.Formula
	tabs   []TabID
}

// undoRun is what one undo or redo knows across its steps.
type undoRun struct {
	u    *undoState
	seq  uint64 // the sequence of the entry's action
	redo bool   // the entry comes from the redo stack
	// pool is the scratch storage of the sheet, whose limits the stand-ins
	// of the undo count in.
	pool *undoScratch
	// links are the stand-ins the undo sets up for the stamps it writes.
	// They take effect once the action's own entry is built (install).
	links linkSet
	// again holds the groups in which the undo wrote over a value the
	// entry's action had written itself: an earlier step of the entry on
	// the same group finds the undo's stamp there.
	again map[groupKey]struct{}
	// structBefore is the structure stamp each tab had before the undo
	// began. The undo's own steps stamp the tabs they restore, and that is
	// not a change by anyone else.
	structBefore map[TabID]uint64
	// blocked holds the tabs in which a later structural step of the entry
	// was skipped: their grid is not the one an earlier deletion left.
	blocked map[TabID]bool
	// styles are the style states the entry kept, and styleIDs the live
	// ids they have got in this undo.
	styles   map[uint32]styleState
	styleIDs map[uint32]uint32
}

// undoStep is one structural or tab-level change of an action, with what
// its undo needs.
type undoStep struct {
	kind  int
	tab   TabID
	rows  bool
	ids   []uint64
	above uint64
	below uint64
	// aboveGone says that the row above a deletion was deleted since.
	aboveGone bool
	// colAt holds, for each saved row with a formula, the position of the
	// column of each of its cells, and rowAt, for each saved cell of a
	// deleted column, the position of its row (undo_follow.go).
	colAt    [][]int32
	rowAt    []int32
	at       int
	saved    []savedRow
	cells    []savedCell
	widths   []savedWidth
	repairs  []repair
	col      ColID
	width    width
	had      bool
	frozen   [2]int
	name     string
	order    []TabID
	savedTab *Tab
	seq      uint64
	// prev is the stamp the step's group had before the action, and
	// stamps, for a sort, the last-sort stamp of each id before the sort.
	// Like the stamps of a cellRestore they are canonical.
	prev   uint64
	stamps []uint64
	// sorted holds the last-sort stamps of deleted rows. They return with
	// the rows.
	sorted []savedSort
}

// cellRestore is the value before an action of the groups of one cell that
// the action wrote.
type cellRestore struct {
	tab     TabID
	row     RowID
	col     ColID
	content bool
	before  formula.Content
	f       *formula.Formula
	pos     formula.Pos // where f is valid
	// beforeSeq and beforeStamps are the stamps of the values before, in
	// canonical form: where a stamp stood in for an entry of the session,
	// the sequence of that entry (undo_notes.go).
	beforeSeq    uint64
	beforeBy     uint32 // the writer of the content before
	props        uint16
	format       formula.Format
	beforeStamps [propCount]uint64
}

// undoEntry is one action on a session's undo or redo stack: for each group
// it changed the value before, and the stamp the action wrote (section 14).
type undoEntry struct {
	seq   uint64
	cells []cellRestore
	steps []undoStep
	// styles holds the style state of every cell the steps saved, by the id
	// the cell names. The style table forgets a state that no live cell
	// uses, so the entry cannot rely on the ids alone.
	styles map[uint32]styleState
	// gaps are the rows and columns other actions deleted since, which the
	// formulas of the entry still name (undo_follow.go).
	gaps []undoGap

	// cells, steps, styles and gaps are the payload. A large payload lives
	// in a scratch file instead (undo_spill.go): file is then set and the
	// four are nil.
	// size is what the entry takes in memory: its payload, or what its
	// payload in a scratch file leaves there (left).
	size int64
	pool *undoScratch
	// owner is the history of the session whose stacks hold the entry.
	owner   *undoState
	counted bool // size is part of the pool's resident bytes
	placed  bool // the entry reached a stack: its action committed
	file    *undoFile
	// deps names the structural edits that can change the payload: those
	// of the tabs it lies in and of the tabs its formulas name. It is nil
	// for a payload that follows no structure (undo_follow.go).
	deps *undoDeps
	// wait is set for a spilled payload that follows structure: it reads
	// the edits it waits for from the scratch log when it is undone.
	wait *undoWait
	// lost is set when the payload is given up: its scratch file is gone
	// or damaged, or the entry passed a limit of undo history. The entry
	// stays on its stack and its undo is skipped.
	lost bool
	// canon is set once the stamps before are in canonical form.
	canon bool
	// keeps names the rows, columns and tabs the entry can bring back
	// (undo_blanks.go). Unlike the payload it stays in memory.
	keeps []keptIDs
	// asks says that a step of the entry asks about the write marks of
	// what it removes: an insert or a new tab (undoStep.writtenSince).
	// Only such an entry keeps marks alive (pruneMarks).
	asks bool
	// blanksLost says that a collected blank record whose stamp was the
	// entry's could not be kept. Where the entry then finds no such stamp,
	// the step is lost, not changed by someone else.
	blanksLost bool
	// For the entry of an undo or a redo, until its action is admitted:
	// takes is the entry it takes back, which leaves its stack when the
	// action commits, redoes says that this is the redo stack, and pushes
	// holds the entries its notes pushed out of the limits, which expire
	// then. admitUndo counts them as gone before that, and marks what
	// leaves as leaving while it runs.
	takes   *undoEntry
	redoes  bool
	pushes  []expiry
	leaving bool
}

// groupKey names one stamp group: the content or one format property of a
// cell, the width of a column, a tab's settings or name, the tab order, or
// the last sort of a row.
type groupKey struct {
	tab  TabID
	row  RowID
	col  ColID
	prop int8
}

// undoState is the undo history of one session, in memory only.
type undoState struct {
	undo, redo []*undoEntry
	// notes holds the stand-in notes of the session, by the stamp that
	// stands in (undo_notes.go). The value an undo brings back gets a new
	// stamp, and an older entry of the session that wrote the value must
	// still find it. In one group a stamp stands for at most two entries,
	// one on each stack: the entry of the action that wrote the stamp, and
	// the entry a note names.
	notes     map[uint64][]*noteSet
	notesMost int         // the most stamps notes held since it was made
	noteBytes int64       // of the sets in notes, and of the index (noteIndexBytes)
	budget    *noteBudget // the sheet's count of note bytes
	// indexBudget is the budget that counts the index, which is budget
	// when the index was made.
	indexBudget *noteBudget
	// pushing counts the entries, and clearing says that the redo stack
	// goes, when the actions of the session that wait for their commit
	// have committed (writer.go). What they take off the stacks holds no
	// stamp any more for the actions behind them.
	pushing  int
	clearing bool
}

// canonical turns the stamps before of a new entry into canonical form. It
// reads the stand-in notes as they were before the action. redo says that
// the entry joins the redo stack.
func (e *undoEntry) canonical(u *undoState, redo bool) {
	e.canon = true
	if len(u.notes) == 0 {
		return
	}
	for i := range e.cells {
		r := &e.cells[i]
		g := groupKey{r.tab, r.row, r.col, groupContent}
		if r.content {
			r.beforeSeq = u.standsFor(g, r.beforeSeq, redo)
		}
		for prop := 0; prop < propCount && r.props != 0; prop++ {
			if r.props&(1<<prop) != 0 {
				g.prop = int8(prop)
				r.beforeStamps[prop] = u.standsFor(g, r.beforeStamps[prop], redo)
			}
		}
	}
	for i := range e.steps {
		s := &e.steps[i]
		switch s.kind {
		case stepWidth:
			s.width.seq = u.standsFor(groupKey{tab: s.tab, col: s.col, prop: groupWidth}, s.width.seq, redo)
		case stepSettings:
			s.prev = u.standsFor(groupKey{tab: s.tab, prop: groupSettings}, s.prev, redo)
		case stepTabName:
			s.prev = u.standsFor(groupKey{tab: s.tab, prop: groupName}, s.prev, redo)
		case stepTabOrder, stepTabAdded, stepTabDeleted:
			s.prev = u.standsFor(groupKey{prop: groupOrder}, s.prev, redo)
		case stepSorted:
			g := groupKey{tab: s.tab, prop: groupSort}
			for k, id := range s.ids {
				g.row = RowID(id)
				s.stamps[k] = u.standsFor(g, s.stamps[k], redo)
			}
		case stepDeleted:
			// The widths of deleted columns return through a link, too.
			for k := range s.widths {
				w := &s.widths[k]
				w.w.seq = u.standsFor(groupKey{tab: s.tab, col: w.col, prop: groupWidth}, w.w.seq, redo)
			}
		}
	}
}

// holds reports whether stamp cur of group g is still the entry's.
func (run *undoRun) holds(x *txn, g groupKey, cur uint64) bool {
	if cur == x.seq {
		_, ok := run.again[g]
		return ok
	}
	return run.u.holds(g, run.seq, cur, run.redo)
}

// link notes that the stamp the undo wrote into group g stands in for the
// entry canon.
func (run *undoRun) link(g groupKey, canon uint64) {
	run.links.add(g, canon)
	if canon == run.seq {
		if run.again == nil {
			run.again = map[groupKey]struct{}{}
		}
		run.again[g] = struct{}{}
	}
}

// undoEntry builds the undo entry of the action from its journal and its
// steps. It runs before the style table is swept.
func (x *txn) undoEntry() *undoEntry {
	wb := x.wb
	e := &undoEntry{seq: x.seq, steps: x.steps}
	seen := map[cellRef]bool{}
	for i := 0; i < x.j.len(); i++ {
		je := x.j.at(i)
		if je.fn != nil || seen[je.ref] {
			continue
		}
		seen[je.ref] = true
		t := je.ref.tab
		if wb.byID[t.id] != t {
			continue
		}
		cur := t.cell(je.ref.row, je.ref.col)
		if cur == nil {
			continue
		}
		r := cellRestore{tab: t.id, row: je.ref.row, col: je.ref.col}
		before, old := je.old, wb.styles.state(je.old.style)
		if cur.seq == x.seq && before.seq != x.seq {
			r.content, r.before, r.f, r.beforeSeq, r.beforeBy = true, before.content, before.f, before.seq, before.by
			if r.f != nil {
				// The text of formula content is the formula's stored text,
				// which a structural edit can replace while the entry
				// waits. The undo writes the formula it holds then, so the
				// entry keeps no text that could outlive that formula.
				r.before.Text = ""
			}
			r.pos, _ = t.posOf(je.ref.row, je.ref.col)
		}
		now := wb.styles.state(cur.style)
		for prop := 0; prop < propCount; prop++ {
			if now.stamps[prop] == x.seq && old.stamps[prop] != x.seq {
				r.props |= 1 << prop
				copyProp(&r.format, old.f, prop)
				r.beforeStamps[prop] = old.stamps[prop]
			}
		}
		if r.content || r.props != 0 {
			e.cells = append(e.cells, r)
		}
	}
	if run := x.run; run != nil {
		// The entry of an undo joins the redo stack, and that of a redo the
		// undo stack. Its stamps are read against the notes as they were
		// before this undo, which then puts its own in place.
		e.canonical(run.u, !run.redo)
		x.expired = run.install(x)
		e.pushes, e.takes, e.redoes = x.expired, run.u.entry(run.seq, run.redo), run.redo
	}
	if len(e.cells) == 0 && len(e.steps) == 0 {
		return nil
	}
	e.keeps = keptBy(e.steps)
	e.asks = slices.ContainsFunc(e.steps, func(s undoStep) bool { return s.kind == stepInserted || s.kind == stepTabAdded })
	e.keepSaved(wb)
	e.place(wb)
	return e
}

// keepSaved copies the style states of the saved cells into the entry and
// cuts the cells loose from the dependency index. A saved cell is the
// record the action deleted, and its node reaches the tab it lay in and
// that tab's other records: once the tab is deleted, nothing but the entry
// would hold them, outside what the entry counts. An undo makes new
// records of the saved ones (revive), and when the action is refused and
// its journal puts the records back, the index is built again for them.
// A deleted tab also gives up its rows in position order, which an undo
// does not read.
func (e *undoEntry) keepSaved(wb *Workbook) {
	keep := func(c *Cell) {
		if c == nil {
			return
		}
		c.node = nil
		if c.style == 0 {
			return
		}
		if _, ok := e.styles[c.style]; !ok {
			if e.styles == nil {
				e.styles = map[uint32]styleState{}
			}
			e.styles[c.style] = wb.styles.state(c.style)
		}
	}
	for i := range e.steps {
		s := &e.steps[i]
		for _, saved := range s.saved {
			for _, slot := range saved.rd.cells {
				keep(slot.c)
			}
		}
		for _, saved := range s.cells {
			keep(saved.c)
		}
		if s.kind == stepTabDeleted {
			s.savedTab.byPos, s.savedTab.byPosOK = nil, false
			for _, rd := range s.savedTab.data {
				for _, slot := range rd.cells {
					keep(slot.c)
				}
			}
		}
	}
}

// style returns the live id of a style an entry kept, interning its state
// again where the table has forgotten it.
func (run *undoRun) style(x *txn, id uint32) uint32 {
	if id == 0 {
		return 0
	}
	if live, ok := run.styleIDs[id]; ok {
		return live
	}
	state := run.styles[id]
	x.wireIndex(state.f)
	live := x.wb.styles.intern(state)
	if run.styleIDs == nil {
		run.styleIDs = map[uint32]uint32{}
	}
	run.styleIDs[id] = live
	return live
}

// restorableFormula checks a formula that an undo is about to put into the
// cell at pos against the limit of one cell that it can have passed while
// the entry held it: a rename or a structural edit it followed can have
// made its text longer (spec rule 9). Saved text and the nesting of a
// formula do not change while an entry holds them.
func restorableFormula(f *formula.Formula, pos formula.Pos) *WriteError {
	if f == nil || len(f.Stored()) <= formula.MaxFormulaChars/2 {
		return nil
	}
	if n := f.TextLen(pos); n > formula.MaxFormulaChars {
		return capacity("formula_length", formula.MaxFormulaChars, int64(n))
	}
	return nil
}

// restorable checks the formulas of a row an undo brings back, and ends
// the references that the row's place takes past what a formula can name
// (placeFormula). Measuring a formula is its bytes on the action's budget,
// and the deadline is looked at before each: a row can hold a long formula
// in every column.
func (x *txn) restorable(t *Tab, row RowID, rd *rowData) *WriteError {
	if rd.nf == 0 {
		return nil
	}
	for _, slot := range rd.cells {
		if slot.c.f == nil {
			continue
		}
		x.spend(len(slot.c.f.Stored()))
		if err := x.paced(); err != nil {
			return err
		}
		pos := x.posOf(t, row, slot.col)
		x.placeFormula(slot.c, pos)
		if err := restorableFormula(slot.c.f, pos); err != nil {
			return err
		}
	}
	return x.paced()
}

// placeFormula gives a returning cell of its own the formula for its
// place: a row or a column can return to another place than the one its
// formulas were kept for, and a reference that place takes past what a
// formula can name ends as a copy would end it (formula.Formula.Placed).
// The new formula is shared with the cells that hold the same one.
func (x *txn) placeFormula(c *Cell, pos formula.Pos) {
	if g := c.f.Placed(pos); g != c.f {
		x.rewrote(c.f, g)
		g = x.wb.intern(g)
		c.f, c.content = g, formulaContent(g)
	}
}

// actionShape checks what a person's action holds: content and format
// operations, or one operation that changes rows, columns or tabs, alone
// (technical design, section 7.1). An undo entry takes back either values
// in cells that stay, or one structural edit, never a mix whose order it
// would have to reconstruct.
func actionShape(ops []personOp) *WriteError {
	if len(ops) < 2 {
		return nil
	}
	for i := range ops {
		switch ops[i].Op {
		case "insert_rows", "insert_cols", "append_rows", "append_cols", "delete_rows", "delete_cols", "sort",
			"add_tab", "rename_tab", "move_tab", "delete_tab", "undo", "redo":
			return invalidf("action", "%s is an action of its own", ops[i].Op)
		}
	}
	return nil
}

// revive returns a saved cell as a new record for the live sheet. The
// saved one stays as it is, so that a refused undo can run again.
func (x *txn) revive(run *undoRun, c *Cell) *Cell {
	live := *c
	live.node = nil
	live.style = run.style(x, c.style)
	return &live
}

// applyUndo applies an undo or redo entry as part of the session's action.
// Each group is restored only where its stamp is still the one the entry's
// action wrote; the rest is skipped and counted.
//
// An undo is an operation of its action and has the checkpoints of one
// (technical design, section 10.4). It starts only before the deadline,
// counts every saved cell it looks at, whether it restores or skips it,
// and ends between two cells, rows, steps or formulas once the deadline
// has passed. The deadline then refuses the action, also when every group
// was skipped, and the entry stays on its stack as it was.
func (x *txn) applyUndo(u *undoState, e *undoEntry) (applied, skipped int, reason string, err *WriteError) {
	if err := x.stop(); err != nil {
		return 0, 0, "", err
	}
	x.keep(journalOpBytes)
	wb := x.wb
	skip := func(why string) {
		skipped++
		if reason == "" {
			reason = why
		}
	}
	redo := len(u.redo) > 0 && u.redo[len(u.redo)-1] == e
	pool, lostStamps := e.pool, e.blanksLost
	defer func() {
		if err != nil {
			x.run = nil
		}
	}()
	if e.file != nil || e.lost {
		// The payload is read for this undo only. The entry keeps its file
		// until it leaves the stack, so a refused undo can run again.
		loaded, lerr := e.load(wb, x.budget)
		if lerr != nil {
			return 0, 0, "", lerr
		}
		if loaded == nil {
			return 0, 1, "lost", nil
		}
		e = loaded
	} else if len(e.gaps) > 0 {
		// Settling changes the formulas the entry holds, and the undo can
		// still be refused. It works on a copy, as for a spilled payload.
		// The copy goes through the bytes the payload is counted at once,
		// and is counted before it is made.
		x.spend(int(e.size))
		if err := x.stop(); err != nil {
			return 0, 0, "", err
		}
		copied, err := e.copyPayload(wb, func() bool { return !x.budget.Check() })
		switch {
		case errors.Is(err, errSpillLate):
			return 0, 0, "", calcTimeout()
		case err != nil:
			return 0, 0, "", scratchRefusal(err)
		}
		e = copied
	}
	if len(e.gaps) > 0 {
		// What others deleted and did not bring back is gone for the
		// formulas the entry holds, too. That is a pass of following, on
		// the action's budget. One that the deadline ends leaves the copy
		// half settled, and the copy is dropped.
		c := &followRun{e: e, wb: wb, intern: true, metered: true, budget: x.budget, left: math.MaxInt64}
		if e.settleIn(c); c.stop {
			return 0, 0, "", calcTimeout()
		}
	}
	run := &undoRun{u: u, pool: pool, seq: e.seq, redo: redo, structBefore: make(map[TabID]uint64, len(wb.tabs)), blocked: map[TabID]bool{}, styles: e.styles}
	x.run = run
	for i := range e.cells {
		// The stamps of the cells whose blank record was collected are
		// read from the store, a stretch of the entry at a time. The
		// checks before and after each read look at the clock, which also
		// finds the time the read waited for storage.
		if i%blankFetch == 0 {
			if err := x.stop(); err != nil {
				return 0, 0, "", err
			}
			lost, ferr := x.collectedFor(e.cells[i:min(i+blankFetch, len(e.cells))])
			if ferr != nil {
				return 0, 0, "", ferr
			}
			lostStamps = lostStamps || lost
		}
		// Looking at a saved cell is a unit, whether its groups are
		// restored or skipped. A restored group counts the record it
		// writes on top (txn.update). The clock is read every 64 cells.
		x.spend(64)
		check := x.paced
		if i%64 == 0 {
			check = x.stop
		}
		if err := check(); err != nil {
			return 0, 0, "", err
		}
		r := &e.cells[i]
		// A stamp below the entry's own sequence is older than what the
		// entry wrote: the record with the entry's stamp was collected.
		// Where the store could not keep it, nobody knows whether the
		// value is still the session's, and the group is lost.
		why := func(cur uint64) string {
			if lostStamps && cur < e.seq {
				return "lost"
			}
			return "changed"
		}
		// A cell that is gone skips every group the entry holds of it.
		gone := func(why string) {
			skip(why)
			skipped += bits.OnesCount16(r.props)
			if !r.content {
				skipped--
			}
		}
		t := wb.byID[r.tab]
		if t == nil {
			gone("changed")
			continue
		}
		if t.recovered > e.seq {
			gone("recovered")
			continue
		}
		if _, ok := t.posOf(r.row, r.col); !ok {
			gone("changed")
			continue
		}
		var seq uint64
		var state styleState
		if c := t.cell(r.row, r.col); c != nil {
			seq, state = c.seq, wb.styles.state(c.style)
		} else if rec := x.recalls[cellID{r.tab, r.row, r.col}]; rec != nil {
			// The blank record of the cell was collected. The guards read
			// the stamps it had, and a group that applies brings the
			// record back with them (txn.update).
			seq = rec.seq
			if rec.stamps != nil {
				state.stamps = *rec.stamps
			}
		}
		g := groupKey{r.tab, r.row, r.col, groupContent}
		if r.content {
			if !u.holds(g, e.seq, seq, redo) {
				skip(why(seq))
			} else {
				if r.f != nil {
					x.spend(len(r.f.Stored()))
					if n := r.f.TextLen(x.posOf(t, r.row, r.col)); n > formula.MaxFormulaChars {
						return 0, 0, "", capacity("formula_length", formula.MaxFormulaChars, int64(n))
					}
				}
				x.setContent(t, r.row, r.col, r.before, r.f)
				t.cell(r.row, r.col).by = authorOf(r, x.by)
				run.links.add(g, r.beforeSeq)
				applied++
			}
		}
		var patch formatPatch
		for prop := 0; prop < propCount; prop++ {
			if r.props&(1<<prop) == 0 {
				continue
			}
			g.prop = int8(prop)
			if !u.holds(g, e.seq, state.stamps[prop], redo) {
				skip(why(state.stamps[prop]))
				continue
			}
			patch.mask |= 1 << prop
			copyProp(&patch.f, r.format, prop)
			run.links.add(g, r.beforeStamps[prop])
			applied++
		}
		if patch.mask != 0 {
			x.setStyle(t, r.row, r.col, patch)
		}
	}
	for _, t := range wb.tabs {
		run.structBefore[t.id] = t.structSeq
	}
	for i := len(e.steps) - 1; i >= 0; i-- {
		// Each step is a structural edit of its own: it holds what one
		// operation holds, and the deadline is looked at before it.
		if err := x.stop(); err != nil {
			return 0, 0, "", err
		}
		x.keep(journalOpBytes)
		s := &e.steps[i]
		ok, why, serr := x.undoStep(run, s)
		if serr != nil {
			return 0, 0, "", serr
		}
		if ok {
			applied++
			continue
		}
		skip(why)
		if s.kind == stepInserted || s.kind == stepDeleted || s.kind == stepSorted {
			run.blocked[s.tab] = true
		}
	}
	// An undo that changed nothing reaches no later check of its action:
	// the deadline is looked at once more before its outcome takes the
	// entry off its stack.
	if err := x.stop(); err != nil {
		return 0, 0, "", err
	}
	return applied, skipped, reason, nil
}

// paced is the check of a loop that counts every step it takes: it ends
// the loop once the budget found the deadline passed, or once the action
// holds more than its journal may. It reads no clock.
func (x *txn) paced() *WriteError {
	if x.late() {
		return calcTimeout()
	}
	return x.fits()
}

func (x *txn) undoStep(run *undoRun, s *undoStep) (bool, string, *WriteError) {
	wb, seq := x.wb, run.seq
	// The tab order is one group: a move, a new tab and a deleted tab all
	// write it. ordered reports whether its stamp is still the one this
	// step wrote, and relinkOrder makes the stamp the undo writes stand in
	// for the session's entry before.
	order := groupKey{prop: groupOrder}
	ordered := run.holds(x, order, wb.tabOrderSeq)
	relinkOrder := func() {
		if ordered {
			run.link(order, s.prev)
		}
	}
	if s.kind == stepTabOrder {
		if !ordered {
			return false, "changed", nil
		}
		tabs := make([]*Tab, 0, len(wb.tabs))
		for _, id := range s.order {
			if t := wb.byID[id]; t != nil {
				tabs = append(tabs, t)
			}
		}
		for _, t := range wb.tabs {
			if !slices.Contains(tabs, t) {
				tabs = append(tabs, t)
			}
		}
		x.orderTabs(tabs)
		relinkOrder()
		return true, "", nil
	}
	if s.kind == stepTabDeleted {
		// A tab whose id is live is never brought back a second time.
		if live := wb.byID[s.savedTab.id]; live != nil {
			if live.recovered > seq {
				return false, "recovered", nil
			}
			return false, "changed", nil
		}
		if err := x.restoreTab(run, s); err != nil {
			return false, "", err
		}
		relinkOrder()
		return true, "", nil
	}
	t := wb.byID[s.tab]
	if t == nil {
		return false, "changed", nil
	}
	// A recovery from a revision replaced the tab the entry knew: its rows
	// and columns are back under their ids with other content. Nothing the
	// entry saved fits that tab, and nothing in it is the entry's to remove.
	if t.recovered > seq {
		return false, "recovered", nil
	}
	switch s.kind {
	case stepWidth:
		g := groupKey{tab: t.id, col: s.col, prop: groupWidth}
		if _, live := t.cols.pos[uint64(s.col)]; !live {
			return false, "changed", nil
		}
		cur := t.widths[s.col]
		if !run.holds(x, g, cur.seq) {
			return false, "changed", nil
		}
		px := defaultWidth
		if s.had {
			px = s.width.px
		}
		x.setWidth(t, s.col, px)
		run.link(g, s.width.seq)
	case stepSettings:
		g := groupKey{tab: t.id, prop: groupSettings}
		if !run.holds(x, g, t.settingsSeq) {
			return false, "changed", nil
		}
		x.setSettings(t, s.frozen[0], s.frozen[1])
		run.link(g, s.prev)
	case stepTabName:
		g := groupKey{tab: t.id, prop: groupName}
		if !run.holds(x, g, t.nameSeq) {
			return false, "changed", nil
		}
		if other := wb.tab(s.name); other != nil && other != t {
			return false, "tab_name_taken", nil
		}
		if err := x.renameTab(t, s.name); err != nil {
			return false, "", err
		}
		run.link(g, s.prev)
	case stepTabAdded:
		if written, _ := s.writtenSince(t, x.by, seq, nil); written {
			return false, "written", nil
		}
		// The sheet keeps one tab. The step cannot apply, now or later, as
		// long as that holds, so it is skipped like any other.
		if len(wb.tabs) == 1 {
			return false, "last_tab", nil
		}
		if err := x.deleteTab(t); err != nil {
			return false, "", err
		}
		relinkOrder()
	case stepInserted:
		// The marks of the ids are looked up once to see whether another
		// writer wrote into them, and the ids once more to find the live
		// ones: a unit for each.
		written, ok := s.writtenSince(t, x.by, seq, x.pace)
		if !ok {
			return false, "", calcTimeout()
		}
		if written {
			return false, "written", nil
		}
		a := x.axisOf(t, s.rows)
		var live []uint64
		tk := ticker{more: x.pace, cost: 64}
		for _, id := range s.ids {
			if !tk.tick() {
				return false, "", calcTimeout()
			}
			if _, ok := a.pos[id]; ok {
				live = append(live, id)
			}
		}
		if !tk.done() {
			return false, "", calcTimeout()
		}
		if len(live) == 0 {
			return false, "changed", nil
		}
		// A tab keeps one row and one column.
		if len(live) == a.len() {
			return false, "empty_tab", nil
		}
		if _, err := x.deleteIDs(t, s.rows, live); err != nil {
			return false, "", err
		}
	case stepDeleted:
		// An id is in its axis once. Rows that are live again, by whatever
		// way, are not brought back.
		a := x.axisOf(t, s.rows)
		for _, id := range s.ids {
			x.spend(64)
			if err := x.paced(); err != nil {
				return false, "", err
			}
			if _, live := a.pos[id]; live {
				return false, "changed", nil
			}
		}
		return true, "", x.restoreAxis(run, t, s)
	case stepSorted:
		var live, before []uint64
		var positions []int32
		g := groupKey{tab: t.id, prop: groupSort}
		for i, id := range s.ids {
			x.spend(64)
			if err := x.paced(); err != nil {
				return false, "", err
			}
			p, ok := t.rows.pos[id]
			if !ok {
				continue
			}
			// Every row must still carry the stamp this session's sort gave
			// it: any other stamp is a later sort by someone else.
			if g.row = RowID(id); !run.holds(x, g, t.lastSort[g.row].seq) {
				return false, "sorted", nil
			}
			live, positions, before = append(live, id), append(positions, p), append(before, s.stamps[i])
		}
		if err := x.sortPositions(positions); err != nil {
			return false, "", err
		}
		steps := len(x.steps)
		if err := x.reorder(t, positions, live); err != nil {
			return false, "", err
		}
		if len(x.steps) > steps {
			// The rows the reorder ordered carry the undo's stamp now, also
			// when none of them moved, and it stands in for the stamp each
			// had before the sort. A row that was frozen since stays out of
			// it. Linking them is a unit each, counted before.
			x.spend(64 * len(live))
			if err := x.paced(); err != nil {
				return false, "", err
			}
			for i, id := range live {
				if g.row = RowID(id); t.lastSort[g.row].seq == x.seq {
					run.link(g, before[i])
				}
			}
		}
	}
	return true, "", nil
}

// restoreAxis brings deleted rows or columns back with their cells and
// ids: after the surviving row that was above them, else before the one
// that was below, else at the nearest position. They get a new born stamp.
func (x *txn) restoreAxis(run *undoRun, t *Tab, s *undoStep) *WriteError {
	wb := x.wb
	a := x.axisOf(t, s.rows)
	limit, name, kind := formula.MaxCols, "columns", formula.InsertCols
	if s.rows {
		limit, name, kind = formula.MaxRows, "rows", formula.InsertRows
	}
	if a.len()+len(s.ids) > limit {
		return capacity(name, int64(limit), int64(a.len()+len(s.ids)))
	}
	// The place the entry kept in step with later structure is behind the
	// row that was above while that row lives, and else before the row that
	// was below, unless rows were put between the two while the upper one
	// still stood. The formulas of the saved rows were kept for that place.
	// A sort moves rows without regard to it, so after one the neighbors
	// say where the place is.
	at := min(s.at, a.len())
	if p, ok := a.pos[s.above]; ok {
		at = int(p) + 1
	} else if p, ok := a.pos[s.below]; ok && s.rows && t.lastSort[RowID(s.below)].seq > s.seq {
		at = int(p)
	}
	// A formula gets its earlier text back only while the grid of every tab
	// it concerns is as the deletion left it. Other steps of this undo do
	// not count: they put back what the same action took away.
	var repairs []repair
	for _, r := range s.repairs {
		x.spend(64 * (1 + len(r.tabs)))
		if err := x.paced(); err != nil {
			return err
		}
		rt := wb.byID[r.tab]
		if rt == nil {
			continue
		}
		c := rt.cell(r.row, r.col)
		if c == nil || c.f == nil || c.seq > s.seq {
			continue
		}
		clean := true
		for _, id := range r.tabs {
			before, live := run.structBefore[id]
			if wb.byID[id] == nil || !live || before > s.seq || run.blocked[id] {
				clean = false
			}
		}
		if clean {
			repairs = append(repairs, r)
		}
	}
	// Rows that return at the end of the tab are no appended rows: other
	// entries hold formulas that name them, and learn through the log that
	// they are back.
	edit := structEdit{tab: t, rows: s.rows, kind: editInsert, ids: s.ids}
	edit.tr = formula.Transform{Kind: kind, Tab: t.name, At: int32(at), Count: int32(len(s.ids))}
	if err := x.transform(edit.tr); err != nil {
		return err
	}
	if at > 0 {
		edit.after = a.ids[at-1]
	}
	// The ids return with a new born stamp. The step holds them, and the
	// journal what their runs are.
	sorted, ok := sortedIDs(s.ids, x.pace)
	var add runStamps
	if ok {
		add, ok = stamped(sorted, x.seq, x.pace)
	}
	if !ok {
		return calcTimeout()
	}
	if err := x.putIDs(t, s.rows, at, s.ids, add, 0); err != nil {
		return err
	}
	t.structSeq = x.seq
	// The last-sort stamps go back one by one, a unit each.
	if err := x.giveSorts(t, s); err != nil {
		return err
	}
	if s.rows {
		for _, saved := range s.saved {
			// A row is brought back whole or not at all, after the check of
			// the clock that lies between two rows that are written. Its
			// records are counted as it is put back (txn.attachRow).
			if err := x.stop(); err != nil {
				return err
			}
			x.spend(64)
			rd := &rowData{cells: make([]cellSlot, 0, len(saved.rd.cells))}
			for _, slot := range saved.rd.cells {
				if _, live := t.cols.pos[uint64(slot.col)]; live {
					rd.cells = append(rd.cells, cellSlot{slot.col, x.revive(run, slot.c)})
					if slot.c.f != nil {
						rd.nf++
					}
				}
			}
			if err := x.restorable(t, saved.id, rd); err != nil {
				return err
			}
			if len(rd.cells) > 0 {
				x.attachRow(t, saved.id, rd)
			}
		}
	} else {
		for _, saved := range s.cells {
			x.spend(64)
			if err := x.paced(); err != nil {
				return err
			}
			if _, live := t.rows.pos[uint64(saved.row)]; !live || saved.c == nil {
				continue
			}
			cell := *x.revive(run, saved.c)
			pos := x.posOf(t, saved.row, saved.col)
			if cell.f != nil {
				// Measuring a formula is its bytes, as in a row
				// (txn.restorable): a column can hold thousands of long
				// formulas between two looks at the clock.
				x.spend(len(cell.f.Stored()))
				if err := x.paced(); err != nil {
					return err
				}
				x.placeFormula(&cell, pos)
			}
			if err := restorableFormula(cell.f, pos); err != nil {
				return err
			}
			x.update(t, saved.row, saved.col, func(c *Cell) {
				node := c.node
				*c = cell
				c.node = node
				x.setFormula(t, saved.row, saved.col, c, cell.f)
			})
		}
		// A width returns as part of its column. As a step of its own, a
		// redo that leaves the column, because another writer wrote into
		// it, would still take the width away.
		// The undo writes the width, so its stamp stands in for the
		// session's entry that had written it, as for every group an undo
		// restores.
		for _, w := range s.widths {
			if err := x.paced(); err != nil {
				return err
			}
			x.putWidth(t, w.col, w.w.px)
			run.link(groupKey{tab: t.id, col: w.col, prop: groupWidth}, w.w.seq)
		}
	}
	x.structs = append(x.structs, edit)
	x.steps = append(x.steps, undoStep{kind: stepInserted, tab: t.id, rows: s.rows, ids: s.ids})
	x.structural()
	x.tabList = true
	for _, r := range repairs {
		// Each formula is written again where it is counted (txn.update),
		// after the check of the clock that lies between two of them.
		if err := x.stop(); err != nil {
			return err
		}
		rt := wb.byID[r.tab]
		x.spend(len(r.before.Stored()))
		if err := restorableFormula(r.before, x.posOf(rt, r.row, r.col)); err != nil {
			return err
		}
		x.rewrite(cellRef{rt, r.row, r.col}, r.before)
	}
	return nil
}

// orderTabs puts the tabs into the given order. One move does it when the
// orders differ by one tab, and also when they are equal: the undo of a
// move writes the tab order as that move did.
func (x *txn) orderTabs(tabs []*Tab) {
	wb := x.wb
	without := func(list []*Tab, skip *Tab) []*Tab {
		out := make([]*Tab, 0, len(list))
		for _, t := range list {
			if t != skip {
				out = append(out, t)
			}
		}
		return out
	}
	for i, t := range tabs {
		if wb.tabs[i] != t && slices.Equal(without(wb.tabs, t), without(tabs, t)) {
			x.moveTab(t, i)
			return
		}
	}
	if slices.Equal(wb.tabs, tabs) {
		x.moveTab(tabs[0], 0)
		return
	}
	for i, t := range tabs {
		if wb.tabs[i] != t {
			x.moveTab(t, i)
		}
	}
}

// tabPlace returns where a deleted tab goes back into the tab order: after
// the surviving tab that was before it, else before the one that was after
// it, else at the position it had.
func (wb *Workbook) tabPlace(s *undoStep) int {
	i := slices.Index(s.order, s.tab)
	for k := i - 1; k >= 0; k-- {
		if t := wb.byID[s.order[k]]; t != nil {
			return wb.tabIndex(t) + 1
		}
	}
	for k := i + 1; i >= 0 && k < len(s.order); k++ {
		if t := wb.byID[s.order[k]]; t != nil {
			return wb.tabIndex(t)
		}
	}
	return min(s.at, len(wb.tabs))
}

// restoreTab brings a deleted tab back with its id, rows, columns and
// cells. If its name is taken it returns as "<name> (restored)". The tab
// is built anew from the saved one, which stays as the entry holds it.
//
// It comes back in one chunk, as a new or a recovered tab does, and not
// with the chunks it was saved with: those are of the boundary before it
// left, which may be older than the last one, so that tabs brought back
// with their own lists could hold more chunks than a sheet does
// (maxSheetChunks). The next boundary splits the chunk (rebalance).
func (x *txn) restoreTab(run *undoRun, s *undoStep) *WriteError {
	wb, src := x.wb, s.savedTab
	if len(wb.tabs) >= maxTabs {
		return capacity("tabs", maxTabs, int64(len(wb.tabs)+1))
	}
	// The axes and the maps of the tab are copied in one piece each, which
	// the limits of rows and columns bound. They are counted before, as an
	// edit of an axis counts its ids, and a map entry as a unit.
	x.spend(8*(len(src.rows.ids)+len(src.cols.ids)) + 64*(len(src.widths)+len(src.marks)+len(src.colMarks)+len(src.lastSort)))
	if err := x.stop(); err != nil {
		return err
	}
	cloneAxis := func(a *axis) axis {
		return axis{ids: slices.Clone(a.ids), pos: maps.Clone(a.pos), born: slices.Clone(a.born)}
	}
	t := &Tab{id: src.id, name: src.name, born: x.seq, nameSeq: src.nameSeq, settingsSeq: src.settingsSeq, structSeq: src.structSeq,
		rows: cloneAxis(&src.rows), cols: cloneAxis(&src.cols), widths: maps.Clone(src.widths),
		frozenRows: src.frozenRows, frozenCols: src.frozenCols, data: make(map[RowID]*rowData, len(src.data)), chunks: []RowID{0},
		marks: maps.Clone(src.marks), colMarks: maps.Clone(src.colMarks), mark: src.mark, lastSort: maps.Clone(src.lastSort),
		marksMost: len(src.marks), colMarksMost: len(src.colMarks),
		recovered: src.recovered}
	for row, rd := range src.data {
		// Each row is copied after the check of the clock that lies
		// between two rows that are written, and its records count as
		// records that return.
		if err := x.stop(); err != nil {
			return err
		}
		x.spend(rowSteps(rd))
		live := &rowData{cells: make([]cellSlot, len(rd.cells)), nf: rd.nf}
		for i, slot := range rd.cells {
			live.cells[i] = cellSlot{slot.col, x.revive(run, slot.c)}
		}
		t.data[row] = live
	}
	if name := wb.restoredName(t.name); name != t.name {
		if err := x.renameOwn(t, name); err != nil {
			return err
		}
	}
	for row, rd := range t.data {
		if err := x.restorable(t, row, rd); err != nil {
			return err
		}
	}
	// Placing the tab counts its records in, in one pass that is counted
	// before it starts (txn.placeTab). The rows were counted as they were
	// copied; the clock is looked at once more before that pass.
	if err := x.stop(); err != nil {
		return err
	}
	order := wb.tabOrderSeq
	x.placeTab(t, wb.tabPlace(s))
	x.steps = append(x.steps, undoStep{kind: stepTabAdded, tab: t.id, prev: order})
	return nil
}

// renameOwn renames a tab that is not placed and rewrites the references in
// its own formulas that name it, as a rename does. No other tab changes.
func (x *txn) renameOwn(t *Tab, name string) *WriteError {
	wb := x.wb
	tr := formula.Transform{Kind: formula.RenameTab, Tab: t.name, Name: name}
	type change struct {
		c *Cell
		f *formula.Formula
	}
	var changes []change
	// The rows are gone through as a rename of a placed tab goes through
	// its formulas (txn.transform): each formula is counted as it is
	// brought through the rename, and the loop ends before the next one
	// once the deadline has passed. A row without a formula is 64 to a
	// unit.
	for pos, rd := range t.rowsByPos() {
		if rd == nil || rd.nf == 0 {
			x.spend(1)
			continue
		}
		if err := x.stop(); err != nil {
			return err
		}
		for _, s := range rd.cells {
			if s.c.f == nil {
				continue
			}
			if err := x.paced(); err != nil {
				return err
			}
			at := formula.Pos{Row: int32(pos), Col: t.cols.pos[uint64(s.col)]}
			nf, _, changed := s.c.f.Apply(t.name, at, tr)
			x.rewrote(s.c.f, nf)
			if !changed {
				continue
			}
			x.spend(len(nf.Stored()))
			if n := nf.TextLen(at); n > formula.MaxFormulaChars {
				return capacity("formula_length", formula.MaxFormulaChars, int64(n))
			}
			changes = append(changes, change{s.c, wb.intern(nf)})
		}
	}
	// Writing the changed formulas back is a unit each, counted before.
	x.spend(64 * len(changes))
	if err := x.paced(); err != nil {
		return err
	}
	old := t.name
	olds := make([]change, len(changes))
	for i, ch := range changes {
		olds[i] = change{ch.c, ch.c.f}
		ch.c.f, ch.c.content, ch.c.textSeq = ch.f, formulaContent(ch.f), x.seq
	}
	t.name = name
	x.j.push(func() {
		t.name = old
		for _, o := range olds {
			o.c.f, o.c.content = o.f, formulaContent(o.f)
		}
	})
	return nil
}

// markCreated notes that the entry reached a stack: its action committed.
func (e *undoEntry) markCreated() { e.placed = true }

// release drops what an entry that left its stack holds outside itself:
// its scratch file and its share of the resident bytes.
func (e *undoEntry) release() {
	if e.pool != nil {
		e.pool.forget(e)
		if len(e.keeps) > 0 {
			// The collected blank records of what only this entry could
			// bring back can go.
			e.pool.blanks.gone()
		}
	}
}

func (u *undoState) push(e *undoEntry) {
	e.markCreated()
	u.pushing = max(0, u.pushing-1)
	u.undo = append(u.undo, e)
	if len(u.undo) > maxUndoEntries {
		u.undo[0].release()
		u.undo = cut(u.undo, 0, 1)
	}
	u.prune()
}

func (u *undoState) clearRedo() {
	for _, e := range u.redo {
		e.release()
	}
	u.redo, u.clearing = nil, false
	u.prune()
}

func (u *undoState) drop() {
	for _, e := range u.undo {
		e.release()
	}
	u.clearRedo()
	u.undo = nil
	u.prune()
}
