package sheet

import (
	"maps"
	"slices"
	"strconv"
	"unsafe"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// An undo entry holds formulas outside the live sheet: the content before
// an edit, and the cells of deleted rows, columns and tabs. They follow the
// structure that changes while the entry waits, as if they had been in
// their cells all the time (technical design, section 14).
//
// A formula of a saved row lies on a grid that the live sheet no longer
// has: the grid with the saved rows in it. An edit of the live grid is
// therefore rewritten for that grid before it is applied (liftEdits). The
// same holds for rows that other actions delete later. Their undo brings
// them back, and with them the references a held formula has to them, so
// such a deletion is not applied when it happens. The entry keeps it as a
// gap, and its formulas stay on the grid with the gap filled. The gap
// closes when the rows return to it. Where they do not, the deletion is
// applied after all: when the entry is undone, when the rows return
// elsewhere, when a sort moves the rows around it, when the entry holds
// more gaps than gapsKept, or before an edit would mix the references the
// deletion concerns with others (part).
//
// Following is a function of the entry and the edits alone. An edit carries
// what its transform needs: the tab and its name in force, the ids it
// brought or took and the ids around them. The entry carries the rest: the
// position of every formula it holds and whether the row above a deletion
// still stands. Nothing is read from the live sheet, so an entry that
// follows each edit as it commits and one that reads them from the
// scratch log at its undo (undo_spill.go) come to the same formulas.
//
// An entry follows only the edits that can change it: those of the tabs it
// lies in and of the tabs its formulas name (undoDeps). Every other edit
// is passed over without a look at the payload.

// undoDeps names the edits an entry depends on: the tabs it lies in, by
// id, and the tabs its formulas name, by their folded names. A formula
// reads a tab by its name, so a tab that takes a name the entry names
// counts from then on. An entry with more than depsKept of either depends
// on every tab.
type undoDeps struct {
	tabs              map[TabID]struct{}
	names             map[string]struct{}
	allTabs, allNames bool
	// own is the folded name of the deleted tab, for the entry of a tab
	// deletion. The formulas of that tab do not follow the edits of a tab
	// with that name, or its renames: the name can pass to another tab,
	// and their references to their own tab are not references to that
	// one (followRenames, pass). The entry does not depend on them.
	own string
	// apart is what the entry's formulas would take in memory beyond what
	// they take now if every place that holds one had a formula of its
	// own: an insert, a deletion or a sort can leave each cell of a column
	// that shared one formula with another text.
	apart int64
	// most is the most names held at one time. A rename takes one name out
	// and puts another in, and the map keeps the room of the most.
	most int
}

const depsKept = 32

func (d *undoDeps) addTab(id TabID) {
	if _, ok := d.tabs[id]; ok || d.allTabs {
		return
	}
	if len(d.tabs) >= depsKept {
		d.tabs, d.allTabs = nil, true
		return
	}
	if d.tabs == nil {
		d.tabs = map[TabID]struct{}{}
	}
	d.tabs[id] = struct{}{}
}

func (d *undoDeps) addName(folded string) {
	if _, ok := d.names[folded]; ok || d.allNames {
		return
	}
	if len(d.names) >= depsKept {
		d.names, d.allNames = nil, true
		return
	}
	if d.names == nil {
		d.names = map[string]struct{}{}
	}
	d.names[folded] = struct{}{}
	d.most = max(d.most, len(d.names))
}

func (d *undoDeps) clone() *undoDeps {
	if d == nil {
		return nil
	}
	return &undoDeps{tabs: maps.Clone(d.tabs), names: maps.Clone(d.names), allTabs: d.allTabs, allNames: d.allNames, own: d.own, apart: d.apart, most: d.most}
}

// parts reports whether one of the edits is an insert, a deletion or a sort
// that the entry depends on. It changes nothing.
func (d *undoDeps) parts(edits []logEntry) bool {
	probe := d.clone()
	for i := range edits {
		if probe.take(&edits[i]) && edits[i].tr.Kind != formula.RenameTab {
			return true
		}
	}
	return false
}

// bytes returns the memory the dependencies take: the record, its two
// maps, the names map at the most names it held, and the text of each name
// and of own.
func (d *undoDeps) bytes() int64 {
	if d == nil {
		return 0
	}
	n := formula.HeapBytes(unsafe.Sizeof(undoDeps{}), true) + formula.TextBytes(len(d.own)) + mapBytes(d.tabs)
	if d.names != nil {
		n += mapRoom[string, struct{}](max(d.most, len(d.names)))
	}
	for name := range d.names {
		n += formula.TextBytes(len(name))
	}
	return n
}

// take reports whether an edit can change what the entry holds, and
// brings the dependencies up to the edit: a rename gives a name the entry
// names another spelling, and a deletion the entry keeps as a gap makes
// its tab one the entry lies in. For every edit but a rename, taking it
// twice is taking it once.
func (d *undoDeps) take(ed *logEntry) bool {
	if d == nil {
		return false
	}
	_, lies := d.tabs[ed.tab]
	lies = lies || d.allTabs
	named, old := false, ""
	// A sort carries no name: it moves the rows of its tab and nothing a
	// formula elsewhere reads.
	if ed.tr.Kind != 0 {
		if named = d.allNames; !named {
			old = formula.FoldName(ed.tr.Tab)
			if _, named = d.names[old]; old == d.own {
				named = false
			}
		}
	}
	if !lies && !named {
		return false
	}
	switch ed.tr.Kind {
	case formula.RenameTab:
		if named && !d.allNames {
			delete(d.names, old)
			d.addName(formula.FoldName(ed.tr.Name))
		}
	case formula.DeleteRows, formula.DeleteCols:
		d.addTab(ed.tab)
	}
	return true
}

// eachFormula calls visit for every formula the entry holds, once for
// each place that holds it, until visit returns false. placed is false
// for the earlier form of a formula a deletion changed: it has no place
// the entry follows, and only a rename changes it.
func (e *undoEntry) eachFormula(visit func(f *formula.Formula, placed bool) bool) {
	for i := range e.cells {
		if f := e.cells[i].f; f != nil && !visit(f, true) {
			return
		}
	}
	row := func(rd *rowData) bool {
		for _, slot := range rd.cells {
			if slot.c.f != nil && !visit(slot.c.f, true) {
				return false
			}
		}
		return true
	}
	for i := range e.steps {
		s := &e.steps[i]
		for k := range s.repairs {
			if !visit(s.repairs[k].before, false) {
				return
			}
		}
		for _, saved := range s.saved {
			if saved.rd.nf != 0 && !row(saved.rd) {
				return
			}
		}
		for _, saved := range s.cells {
			if saved.c != nil && saved.c.f != nil && !visit(saved.c.f, true) {
				return
			}
		}
		if s.savedTab != nil {
			for _, rd := range s.savedTab.data {
				if rd.nf != 0 && !row(rd) {
					return
				}
			}
		}
	}
}

// places counts the cells, rows and deletions a look through the entry
// passes over, formula or not.
func (e *undoEntry) places() int64 {
	n := int64(len(e.cells) + len(e.gaps) + len(e.steps))
	for i := range e.steps {
		s := &e.steps[i]
		n += int64(len(s.repairs) + len(s.saved) + len(s.cells))
		if s.savedTab != nil {
			n += int64(len(s.savedTab.data))
		}
	}
	return n
}

// depend works out what the entry depends on from what it holds now.
func (e *undoEntry) depend() { e.dependIn(&followRun{}) }

// dependIn is depend as work of the run c, in the units of followRun. It
// reports whether the run may go on. A run that stops leaves the entry
// without its dependencies.
func (e *undoEntry) dependIn(c *followRun) bool {
	e.deps = nil
	// Finding whether the entry holds anything passes over it too.
	if !c.charge(e.places() / 16) {
		return false
	}
	if !e.holds() {
		return true
	}
	d := &undoDeps{}
	placed := false
	for i := range e.cells {
		if e.cells[i].f != nil {
			d.addTab(e.cells[i].tab)
			placed = true
		}
	}
	for i := range e.steps {
		switch s := &e.steps[i]; s.kind {
		case stepDeleted:
			d.addTab(s.tab)
			placed = true
		case stepTabDeleted:
			// An action deletes one tab and does nothing else. Should an
			// entry ever hold more beside the tab, it depends on every
			// name, which is always safe.
			if own := formula.FoldName(s.savedTab.name); placed || (d.own != "" && d.own != own) {
				d.allNames, d.own = true, ""
			} else if !d.allNames {
				d.own = own
			}
		}
	}
	if placed && d.own != "" {
		d.allNames, d.own = true, ""
	}
	for i := range e.gaps {
		d.addTab(e.gaps[i].tab)
	}
	seen := map[*formula.Formula]struct{}{}
	e.eachFormula(func(f *formula.Formula, placed bool) bool {
		if _, ok := seen[f]; ok {
			if placed {
				d.apart += formulaBytes(f)
			}
			return true
		}
		seen[f] = struct{}{}
		if !c.charge(1 + int64(len(f.Stored()))/256) {
			return false
		}
		if d.allNames {
			return true
		}
		last := ""
		f.Tabs(func(name string) {
			if name != last {
				d.addName(formula.FoldName(name))
				last = name
			}
		})
		return true
	})
	if c.stop {
		return false
	}
	e.deps = d
	return true
}

// names reports whether a formula the entry holds at a place has a
// reference written with a tab name that folds to folded. The earlier
// forms of the formulas a deletion changed do not count, because no kept
// deletion reaches them. Looking through the formulas is work of the run
// c, counted as depend counts it.
func (e *undoEntry) names(c *followRun, folded string) (found bool) {
	if !c.charge(e.places() / 16) {
		return false
	}
	seen := map[*formula.Formula]struct{}{}
	e.eachFormula(func(f *formula.Formula, placed bool) bool {
		if !placed {
			return true
		}
		if _, ok := seen[f]; ok {
			return true
		}
		seen[f] = struct{}{}
		if !c.charge(1 + int64(len(f.Stored()))/256) {
			return false
		}
		last := ""
		f.Tabs(func(name string) {
			if name != last && !found {
				found, last = formula.FoldName(name) == folded, name
			}
		})
		return !found
	})
	return found
}

// followRun is one call of follow for one entry: the edits of an action
// for an entry in memory, or the edits a spilled payload catches up on at
// its undo.
type followRun struct {
	e  *undoEntry
	wb *Workbook
	// plain makes the run take every edit one by one, whatever the entry
	// depends on. Tests compare it with the ordinary way.
	plain bool
	// intern shares the formulas of saved cells with the live sheet as they
	// change. A payload that catches up does that once, at the end.
	intern bool
	// A catch-up is metered: it may take left units of work, counted like
	// the work of a calculation (technical design, section 10.4), and its
	// action's budget checks the deadline. stop is set once either is
	// passed; the entry the run worked on is then left half followed and
	// must not be used.
	//
	// A stopped run starts nothing more. Passing over a stretch of cells
	// or rows and reading a log file are counted before they are done,
	// and are not done when the count passes the bound. A rewrite is
	// counted after it is done, and every loop of following looks at stop
	// before each formula it would rewrite. A run therefore does the work
	// of one rewrite beyond its bound at most, which is one unit and one
	// for each 16 bytes of that formula's stored text. The budget reads
	// the clock every 4,096 units, so a run that meets the deadline ends
	// within the time of that many units and one rewrite.
	metered bool
	budget  *formula.Budget
	left    int64
	looked  int64 // formulas looked through without a change
	passed  int64 // edits passed over
	stop    bool
	// late counts the rewrites a run made after it stopped, and over the
	// units charged after that. Both stay zero; tests read them.
	late int
	over int64
	// spare is for the entries in memory, which follow the edits of an
	// action as it commits. It points at the units of work the action has
	// left for all of them together. Such a run is not cut off inside an
	// edit, because the entry has no other copy. It ends before the first
	// edit it finds the units used up at, and its entry follows the rest
	// from a scratch file (followUndo).
	spare *int64
	// changed says that a formula, a gap or a name of the entry changed
	// during the run, so its size has to be worked out again. stale says
	// that it changed since its dependencies were last worked out: follow
	// works them out again before it decides about the next edit. What an
	// entry depends on is therefore a function of what it holds, and never
	// hangs on how many edits it follows in one run.
	changed, stale bool
	// canon holds one parsed formula for each stored text of the entry,
	// for the pass under way.
	canon map[string]*formula.Formula
	// runs holds the ids of each deletion as runs, for the entries that
	// follow the same edits to share.
	runs map[*logEntry][][2]uint64
}

// followWatch, when a test sets it, sees each catch-up as it ends.
var followWatch func(c *followRun)

// gone returns the ids a deletion took, as runs.
func (c *followRun) gone(ed *logEntry) [][2]uint64 {
	if runs, ok := c.runs[ed]; ok {
		return runs
	}
	runs := idRuns(ed.gone)
	if c.runs != nil {
		c.runs[ed] = runs
	}
	return runs
}

// charge counts units of work and reports whether the run may go on.
func (c *followRun) charge(units int64) bool {
	if c.spare != nil {
		*c.spare -= units
	}
	if !c.metered {
		return true
	}
	if c.stop {
		c.over += units
		return false
	}
	c.left -= units
	if c.left < 0 || !c.budget.Add(units) {
		c.stop = true
	}
	return !c.stop
}

// rewrote counts the work of one Apply or Moved on f that returned g. A
// formula that was written and read again counts one unit and one for each
// 16 bytes of its stored text. One that was only looked through costs a
// small part of that: one unit for each eight of them, and one for each
// 256 bytes.
func (c *followRun) rewrote(f, g *formula.Formula) {
	if c.stop {
		c.late++
	}
	if g != f {
		c.charge(1 + int64(len(g.Stored()))/16)
		return
	}
	units := int64(len(f.Stored())) / 256
	if c.looked++; c.looked%8 == 0 {
		units++
	}
	if units > 0 {
		c.charge(units)
	}
}

// share returns the one parsed formula the entry holds for the stored text
// of g. Each cell that an edit changes gets a formula parsed for itself;
// without this, cells that shared a formula would each keep their own.
func (c *followRun) share(g *formula.Formula, intern bool) *formula.Formula {
	if c.stop {
		return g
	}
	if c.canon == nil {
		if !c.charge(c.e.places() / 16) {
			return g
		}
		canon := map[string]*formula.Formula{}
		seen := map[*formula.Formula]struct{}{}
		c.e.eachFormula(func(f *formula.Formula, _ bool) bool {
			if _, ok := seen[f]; ok {
				return true
			}
			seen[f] = struct{}{}
			if key := formulaKey(f); canon[key] == nil {
				canon[key] = f
			}
			return c.charge(1 + int64(len(f.Stored()))/256)
		})
		if c.stop {
			return g
		}
		c.canon = canon
	}
	key := formulaKey(g)
	if shared := c.canon[key]; shared != nil {
		return shared
	}
	if intern {
		g = c.wb.intern(g)
	}
	c.canon[key] = g
	return g
}

// touched notes that the entry holds something else than before.
func (c *followRun) touched() { c.changed, c.stale = true, true }

// hold stores a followed formula in a cell an entry keeps.
func (c *followRun) hold(cell *Cell, f *formula.Formula) {
	if f != cell.f {
		c.touched()
		cell.f = c.share(f, c.intern)
		cell.content = formulaContent(cell.f)
	}
}

// keep returns a followed formula for a place that is no saved cell: the
// value before an edit, or the earlier form of a formula a deletion
// changed.
func (c *followRun) keep(was, f *formula.Formula) *formula.Formula {
	if f == was {
		return was
	}
	c.touched()
	return c.share(f, false)
}

// undoGap is a run of rows or columns that an action deleted after the
// entry was made. at is where they return, on the grid with the newer gaps
// filled.
type undoGap struct {
	tab   TabID
	rows  bool
	name  string // the name of the tab, in force
	at, n int32
	runs  [][2]uint64 // the deleted ids in their order, as runs
	// above and below are the rows that lay around the deleted ones, and
	// aboveGone says that the one above was deleted since.
	above, below uint64
	aboveGone    bool
}

const gapsKept = 32

func (g *undoGap) first() uint64 {
	if len(g.runs) == 0 {
		return 0
	}
	return g.runs[0][0]
}

// index returns the place of an id among the deleted ones.
func (g *undoGap) index(id uint64) (int32, bool) {
	var before uint64
	for _, run := range g.runs {
		if id >= run[0] && id < run[0]+run[1] {
			return int32(before + id - run[0]), true
		}
		before += run[1]
	}
	return 0, false
}

// gapBytes returns the memory of the deletions the entry keeps apart: the
// list at the capacity of its array, and for each its tab's name and its
// runs at the capacity of theirs. Entries that followed the same deletion
// share its runs, and each counts them.
func (e *undoEntry) gapBytes() int64 {
	n := sliceBytes(cap(e.gaps), unsafe.Sizeof(undoGap{}))
	for i := range e.gaps {
		g := &e.gaps[i]
		n += formula.TextBytes(len(g.name)) + numsBytes(cap(g.runs), unsafe.Sizeof([2]uint64{}))
	}
	return n
}

// closeGap takes gap i out of the entry. The rest go into an array of
// their own size, and none when no gap is left, so that the room of the
// most gaps the entry kept does not stay with it.
func (e *undoEntry) closeGap(i int) {
	e.gaps = slices.Delete(e.gaps, i, i+1)
	if len(e.gaps) == 0 {
		e.gaps = nil
	} else if cap(e.gaps) > len(e.gaps) {
		e.gaps = slices.Clone(e.gaps)
	}
}

// applyEdits applies structural edits in order to a formula that lies at
// pos of the tab named name. An edit that deletes the formula's own cell
// ends it: the formula stays as it was then, and alive is false.
func (c *followRun) applyEdits(f *formula.Formula, name string, pos formula.Pos, list []formula.Transform) (nf *formula.Formula, at formula.Pos, alive bool) {
	for _, tr := range list {
		if c.stop {
			break
		}
		if _, alive := tr.Map(name, pos); !alive {
			return f, pos, false
		}
		g, to, _ := f.Apply(name, pos, tr)
		c.rewrote(f, g)
		f, pos = g, to
	}
	return f, pos, true
}

// gonePos marks a saved formula whose row or column, on the axis its step
// did not delete, is deleted for good. It follows nothing after that.
const gonePos = -1

// liftEdits rewrites edits of one axis for a grid that still holds n rows
// or columns which the grid of the edits lacks: those that return at
// position at. It also returns where they return after the edits. after
// says that rows inserted exactly at that place come after the returning
// ones.
func liftEdits(list []formula.Transform, at, n int32, after bool) ([]formula.Transform, int32) {
	out := make([]formula.Transform, 0, len(list)+1)
	for _, tr := range list {
		switch tr.Kind {
		case formula.InsertRows, formula.InsertCols:
			if tr.At < at || (tr.At == at && !after) {
				at += tr.Count
			} else {
				tr.At += n
			}
			out = append(out, tr)
		case formula.DeleteRows, formula.DeleteCols:
			switch {
			case tr.At+tr.Count <= at:
				at -= tr.Count
				out = append(out, tr)
			case tr.At >= at:
				tr.At += n
				out = append(out, tr)
			default:
				// The deleted run lies on both sides of the returning rows.
				high, low := tr, tr
				high.At, high.Count = at+n, tr.At+tr.Count-at
				low.Count = at - tr.At
				out = append(out, high, low)
				at = tr.At
			}
		}
	}
	return out, at
}

func (s *undoStep) hole(tab TabID, rows bool) bool {
	return s.kind == stepDeleted && s.tab == tab && s.rows == rows
}

// holds reports whether the entry has anything that follows structure.
func (e *undoEntry) holds() bool {
	for i := range e.steps {
		if k := e.steps[i].kind; k == stepDeleted || k == stepTabDeleted {
			return true
		}
	}
	for i := range e.cells {
		if e.cells[i].f != nil {
			return true
		}
	}
	return false
}

// place notes where the formulas in the rows and columns a new entry saved
// lie on the other axis. It runs when the entry is built, on the sheet its
// action left; from then on the positions follow the edits.
func (e *undoEntry) place(wb *Workbook) {
	for i := range e.steps {
		s := &e.steps[i]
		t := wb.byID[s.tab]
		if s.kind != stepDeleted || t == nil {
			continue
		}
		if s.rows {
			for k, saved := range s.saved {
				if saved.rd.nf == 0 {
					continue
				}
				if s.colAt == nil {
					s.colAt = make([][]int32, len(s.saved))
				}
				at := make([]int32, len(saved.rd.cells))
				for c, slot := range saved.rd.cells {
					at[c] = t.cols.pos[uint64(slot.col)]
				}
				s.colAt[k] = at
			}
			continue
		}
		for k, saved := range s.cells {
			if saved.c == nil || saved.c.f == nil {
				continue
			}
			if s.rowAt == nil {
				s.rowAt = make([]int32, len(s.cells))
			}
			s.rowAt[k] = t.rows.pos[uint64(saved.row)]
		}
	}
}

// follow applies structural edits, in commit order, to the formulas and
// positions the entry holds. It passes over the edits the entry does not
// depend on. c.wb only shares the formulas the run makes. It returns the
// number of edits it has taken, which is all of them unless the run has
// spare units of work to stay inside and found them used up.
func (e *undoEntry) follow(c *followRun, edits []logEntry) int {
	d := e.deps
	for i := 0; i < len(edits) && !c.stop; i++ {
		if c.spare != nil && *c.spare < 0 {
			return i
		}
		if c.stale {
			if !e.dependIn(c) {
				return i
			}
			d, c.stale = e.deps, false
		}
		ed := &edits[i]
		// An entry that depends on every name follows renames in a row one
		// by one in any case. A run that has to stay inside spare units
		// takes each as an edit of its own, so that it can end between two.
		single := c.plain || (c.spare != nil && d != nil && d.allNames)
		if ed.tr.Kind == formula.RenameTab && !single && !e.meets(ed) {
			// Renames with nothing between them that the entry depends on
			// are followed as one edit. A rename that meets a kept
			// deletion is followed by itself (part).
			var before map[string]struct{}
			if !d.allNames {
				before = maps.Clone(d.names)
			}
			var run []*logEntry
			j := i
			for ; j < len(edits); j++ {
				next := &edits[j]
				// Looking at an edit is one unit for each 16, as passing
				// over one is.
				if c.passed++; c.passed%16 == 0 && !c.charge(1) {
					return i
				}
				if next.tr.Kind != formula.RenameTab {
					if d.take(next) {
						break
					}
					continue
				}
				if e.meets(next) {
					break
				}
				if d.take(next) {
					run = append(run, next)
				}
			}
			i = j - 1
			e.followRenames(c, run, before, d.allNames)
			continue
		}
		var holds func(folded string) bool
		if ed.tr.Kind != 0 && len(e.gaps) > 0 {
			// Taking the edit gives the names a rename's new name, and
			// part asks for the names before it.
			holds = e.held(c, d, ed.tr.Kind == formula.RenameTab)
		}
		depends := d.take(ed)
		if !depends && !c.plain {
			// Passing over an edit is one unit for each 16.
			if c.passed++; c.passed%16 == 0 && !c.charge(1) {
				return i
			}
			continue
		}
		if !c.charge(1) {
			return i
		}
		if holds != nil {
			e.part(c, ed, holds)
		}
		switch ed.tr.Kind {
		case formula.RenameTab:
			e.followRenames(c, []*logEntry{ed}, nil, true)
		case 0:
			e.followSort(c, ed)
		case formula.DeleteRows, formula.DeleteCols:
			if !depends {
				// Only a deletion the entry depends on when it happens is
				// kept as a gap. A tab can take a name the entry's formulas
				// hold later, and the rows it lost before that are not rows
				// those formulas named.
				break
			}
			if e.neighbors(c, ed, true); !c.charge(int64(len(ed.gone)) / 16) {
				return i
			}
			e.gaps = append(e.gaps, undoGap{tab: ed.tab, rows: ed.rows, name: ed.tr.Tab, at: ed.tr.At, n: ed.tr.Count,
				runs: c.gone(ed), above: ed.above, below: ed.below})
			c.touched()
			if len(e.gaps) > gapsKept {
				e.resolve(c, 0)
			}
		default:
			e.followInsert(c, ed)
			e.neighbors(c, ed, false)
		}
	}
	return len(edits)
}

// held returns the answer to what part asks, which is whether a formula
// the entry holds at a place has a reference written with a tab name that
// folds to the given one, as the entry is now. The names the entry keeps
// are those of every formula it holds, and an entry that names more tabs
// than it keeps depends on every name. Neither says that a formula a kept
// deletion reaches holds the name, so the formulas are looked through,
// once for each name asked that the entry can hold. rename says that the
// edit about to be taken is a rename, which changes the names the entry
// keeps.
func (e *undoEntry) held(c *followRun, d *undoDeps, rename bool) func(folded string) bool {
	var names map[string]struct{}
	all := d != nil && d.allNames
	if d != nil && !all {
		if names = d.names; rename {
			names = maps.Clone(names)
		}
	}
	asked := map[string]bool{}
	return func(folded string) bool {
		found, ok := asked[folded]
		if ok {
			return found
		}
		if _, can := names[folded]; can || all {
			found = e.names(c, folded)
		}
		asked[folded] = found
		return found
	}
}

// meets reports whether a rename concerns a deletion the entry keeps as a
// gap: it renames the gap's tab, or it takes a name from or gives a name
// to another tab that is the name the gap's tab has.
func (e *undoEntry) meets(ed *logEntry) bool {
	if len(e.gaps) == 0 {
		return false
	}
	old, to := formula.FoldName(ed.tr.Tab), formula.FoldName(ed.tr.Name)
	for i := range e.gaps {
		g := &e.gaps[i]
		if name := formula.FoldName(g.name); g.tab == ed.tab || name == old || name == to {
			return true
		}
	}
	return false
}

// part applies the kept deletions that an edit would mix up with other
// references. A gap is a deletion of its tab under the name the tab had,
// and it is applied by that name, to the references that are written with
// the name then. Those have to be the references that named the tab when
// the rows went, so the gap is applied before an edit that would leave it
// otherwise:
//
//   - before a rename gives the gap's tab a name the formulas hold already.
//     Those references named no tab, or a deleted one, when the rows went,
//     and from the rename on they read as the ones the deletion concerns;
//   - before a rename gives the gap's name to references that have another
//     name. The tab that is renamed is another one, or the gap's own tab
//     after it came back under another name;
//   - before an insert or a deletion in another tab that has the gap's
//     name, or a rename of that tab to another name, rewrites the
//     references the deletion concerns. The gap's tab is deleted then, or
//     came back under another name, and the references read the other tab.
//
// These edits mix nothing and leave the gap: a rename to the same name or
// to the same name in other capitals, a rename to a name no formula
// holds, and an edit of another tab under a name that no formula of the
// entry holds. holds says whether the formulas hold a folded name, before
// the edit and before any gap is applied for it.
func (e *undoEntry) part(c *followRun, ed *logEntry, holds func(folded string) bool) {
	rename := ed.tr.Kind == formula.RenameTab
	old, to := formula.FoldName(ed.tr.Tab), ""
	if rename {
		if to = formula.FoldName(ed.tr.Name); to == old {
			return
		}
	}
	var mixed []int
	for i := len(e.gaps) - 1; i >= 0 && !c.stop; i-- {
		g := &e.gaps[i]
		name := formula.FoldName(g.name)
		mixes := false
		switch {
		case name == old && g.tab == ed.tab:
			mixes = rename && holds(to)
		case name == old:
			mixes = holds(name)
		case rename && name == to:
			mixes = holds(old)
		}
		if mixes {
			mixed = append(mixed, i)
		}
	}
	// The newest first, so the places of the others stay.
	for _, i := range mixed {
		if c.stop {
			return
		}
		e.resolve(c, i)
	}
}

// followSort applies a sort of the rows of a tab the entry lies in.
func (e *undoEntry) followSort(c *followRun, ed *logEntry) {
	// A sort leaves no place for the rows of a gap to return to.
	for g := len(e.gaps) - 1; g >= 0 && !c.stop; g-- {
		if e.gaps[g].tab == ed.tab && e.gaps[g].rows {
			e.resolve(c, g)
		}
	}
	c.canon = nil
	// A held formula moves with its row as a formula of the sheet does: a
	// reference that leaves the grid there is #REF! from then on (spec
	// rule 15). Only the row of the place matters to a move along the rows.
	if !c.charge(int64(len(e.cells)) / 16) {
		return
	}
	for k := range e.cells {
		if c.stop {
			return
		}
		r := &e.cells[k]
		if r.tab != ed.tab || r.f == nil || r.pos.Row == gonePos {
			continue
		}
		if mv, ok := ed.moved[r.row]; ok {
			to := formula.Pos{Row: mv[1], Col: r.pos.Col}
			g := r.f.Moved(r.pos, to)
			c.rewrote(r.f, g)
			r.f, r.pos = c.keep(r.f, g), to
		}
	}
	if !c.charge(int64(len(e.steps)) / 16) {
		return
	}
	for k := range e.steps {
		s := &e.steps[k]
		if s.kind != stepDeleted || s.tab != ed.tab || s.rowAt == nil {
			continue
		}
		if !c.charge(int64(len(s.cells)) / 16) {
			return
		}
		for i := range s.cells {
			if c.stop {
				return
			}
			mv, ok := ed.moved[s.cells[i].row]
			if !ok {
				continue
			}
			if saved := s.cells[i].c; saved != nil && saved.f != nil && s.rowAt[i] != gonePos {
				g := saved.f.Moved(formula.Pos{Row: s.rowAt[i]}, formula.Pos{Row: mv[1]})
				c.rewrote(saved.f, g)
				c.hold(saved, g)
			}
			s.rowAt[i] = mv[1]
		}
	}
}

// neighbors keeps track of whether the row above each deletion the entry
// holds still stands: a delete takes it, and an insert brings it back.
func (e *undoEntry) neighbors(c *followRun, ed *logEntry, gone bool) {
	ids := ed.came
	if gone {
		ids = ed.gone
	}
	among := func(id uint64) bool {
		return c.charge(int64(len(ids))/64) && slices.Contains(ids, id)
	}
	if !c.charge(int64(len(e.steps)) / 16) {
		return
	}
	for i := 0; i < len(e.steps) && !c.stop; i++ {
		if s := &e.steps[i]; s.hole(ed.tab, ed.rows) && s.above != 0 && s.aboveGone != gone && among(s.above) {
			s.aboveGone = gone
		}
	}
	for i := 0; i < len(e.gaps) && !c.stop; i++ {
		if g := &e.gaps[i]; g.tab == ed.tab && g.rows == ed.rows && g.above != 0 && g.aboveGone != gone && among(g.above) {
			g.aboveGone = gone
		}
	}
}

// composeRenames turns renames, applied in order, into at most two renames
// for each name: of the names a formula can hold at the start, into names
// no formula holds, and of those into the last name each one got. own is
// the folded name whose renames do not count (followRenames); names holds
// the folded names the formulas can hold at the start.
//
// A rename changes the references that carry its old name at that moment,
// whatever name they had at the start. The loop keeps, for each name the
// formulas hold at the start that was renamed, where the references that
// had it are now. It follows no other name, so a rename costs at most one
// step for each name the formulas hold, however many names the renames
// pass around.
func composeRenames(run []*logEntry, own string, names map[string]struct{}) []formula.Transform {
	last := map[string]string{} // folded name at the start: the name it has now
	at := map[string][]string{} // folded name now: the names at the start that got it
	left := map[string]bool{}   // names at the start whose references were renamed
	for _, ed := range run {
		old := formula.FoldName(ed.tr.Tab)
		if own != "" && old == own {
			continue
		}
		carried := at[old]
		delete(at, old)
		if _, held := names[old]; held && !left[old] {
			carried, left[old] = append(carried, old), true
		}
		if len(carried) == 0 {
			continue
		}
		for _, name := range carried {
			last[name] = ed.tr.Name
		}
		to := formula.FoldName(ed.tr.Name)
		at[to] = append(at[to], carried...)
	}
	from := make([]string, 0, len(last))
	for name := range last {
		from = append(from, name)
	}
	slices.Sort(from)
	if len(from) == 1 {
		return []formula.Transform{{Kind: formula.RenameTab, Tab: from[0], Name: last[from[0]]}}
	}
	// Two names can trade places, so every name first gets one that
	// nothing has: none a formula holds and none a rename gave.
	taken := make(map[string]struct{}, len(names)+len(from))
	for name := range names {
		taken[name] = struct{}{}
	}
	for _, name := range from {
		taken[formula.FoldName(last[name])] = struct{}{}
	}
	out := make([]formula.Transform, 2*len(from))
	n := 0
	for i, name := range from {
		var spare string
		for {
			n++
			spare = "_" + strconv.Itoa(n)
			if _, ok := taken[spare]; !ok {
				break
			}
		}
		out[i] = formula.Transform{Kind: formula.RenameTab, Tab: name, Name: spare}
		out[len(from)+i] = formula.Transform{Kind: formula.RenameTab, Tab: spare, Name: last[name]}
	}
	return out
}

// followRenames applies renames of tabs, in order, to every formula the
// entry holds. names holds the folded names the formulas could hold before
// the first of them; it is not read when all is set.
func (e *undoEntry) followRenames(c *followRun, run []*logEntry, names map[string]struct{}, all bool) {
	if len(run) == 0 || !c.charge(int64(len(run))) {
		return
	}
	// A gap has the name its tab had when the rows went. A rename of the
	// tab from that name renames the references the deletion concerns, and
	// the gap with them. A tab that came back under another name has left
	// its gaps the old one.
	for _, ed := range run {
		for g := range e.gaps {
			if gap := &e.gaps[g]; gap.tab == ed.tab && gap.name != ed.tr.Name && formula.FoldName(gap.name) == formula.FoldName(ed.tr.Tab) {
				gap.name = ed.tr.Name
				c.touched()
			}
		}
	}
	// lists returns the renames for the formulas of a deleted tab that had
	// the folded name own, and for every other formula when own is empty.
	// The name a deleted tab had can pass to another tab, and the deleted
	// tab's references to its own name are not references to that one.
	lists := func(own string) []formula.Transform {
		if len(run) > 2 && !all {
			return composeRenames(run, own, names)
		}
		list := make([]formula.Transform, 0, len(run))
		for _, ed := range run {
			if own == "" || formula.FoldName(ed.tr.Tab) != own {
				list = append(list, ed.tr)
			}
		}
		return list
	}
	c.canon = nil
	// A formula gets the same names wherever it lies, so each parsed
	// formula is renamed once, and the cells that shared it share the
	// result. A list can hold as many renames as the run, and each is a
	// rewrite of its own, so the run can stop between any two.
	renamer := func(list []formula.Transform) func(f *formula.Formula) *formula.Formula {
		done := map[*formula.Formula]*formula.Formula{}
		return func(f *formula.Formula) *formula.Formula {
			if g, ok := done[f]; ok {
				return g
			}
			g := f
			for _, tr := range list {
				if c.stop {
					return f
				}
				next, _, _ := g.Apply("", formula.Pos{}, tr)
				c.rewrote(g, next)
				g = next
			}
			done[f] = g
			return g
		}
	}
	general := lists("")
	if len(general) == 0 && !slices.ContainsFunc(e.steps, func(s undoStep) bool { return s.kind == stepTabDeleted }) {
		return
	}
	renamed := renamer(general)
	if !c.charge(int64(len(e.cells)+len(e.steps)) / 16) {
		return
	}
	for i := range e.cells {
		if c.stop {
			return
		}
		if r := &e.cells[i]; r.f != nil {
			r.f = c.keep(r.f, renamed(r.f))
		}
	}
	row := func(rd *rowData, renamed func(f *formula.Formula) *formula.Formula) {
		if rd.nf == 0 || !c.charge(int64(len(rd.cells))/16) {
			return
		}
		for _, slot := range rd.cells {
			if c.stop {
				return
			}
			if slot.c.f != nil {
				c.hold(slot.c, renamed(slot.c.f))
			}
		}
	}
	for i := 0; i < len(e.steps) && !c.stop; i++ {
		s := &e.steps[i]
		// The formulas a deletion changed come back under the names their
		// tabs have at the undo.
		if !c.charge(int64(len(s.repairs)) / 16) {
			return
		}
		for k := 0; k < len(s.repairs) && !c.stop; k++ {
			s.repairs[k].before = c.keep(s.repairs[k].before, renamed(s.repairs[k].before))
		}
		switch s.kind {
		case stepDeleted:
			if !c.charge(int64(len(s.saved)+len(s.cells)) / 16) {
				return
			}
			for k := 0; k < len(s.saved) && !c.stop; k++ {
				row(s.saved[k].rd, renamed)
			}
			for k := 0; k < len(s.cells) && !c.stop; k++ {
				if saved := s.cells[k]; saved.c != nil && saved.c.f != nil {
					c.hold(saved.c, renamed(saved.c.f))
				}
			}
		case stepTabDeleted:
			own := renamed
			if list := lists(formula.FoldName(s.savedTab.name)); !slices.Equal(list, general) {
				own = renamer(list)
			}
			if !c.charge(int64(len(s.savedTab.data)) / 16) {
				return
			}
			for _, rd := range s.savedTab.data {
				if c.stop {
					return
				}
				row(rd, own)
			}
		}
	}
}

// axisPass is one insert or delete on its way through an entry.
type axisPass struct {
	c    *followRun
	tab  TabID  // the tab of the edit
	rows bool   // the axis of the edit
	name string // the name the tab had at the edit
	// ats are the places of the steps' deletions as they were before the
	// edit.
	ats []int32
}

func (e *undoEntry) newPass(c *followRun, tab TabID, rows bool, name string) *axisPass {
	p := &axisPass{c: c, tab: tab, rows: rows, name: name, ats: make([]int32, len(e.steps))}
	for i := range e.steps {
		p.ats[i] = int32(e.steps[i].at)
	}
	return p
}

// followInsert applies an insert.
func (e *undoEntry) followInsert(c *followRun, ed *logEntry) {
	// Rows that come back to another place than their gap leave the gap a
	// deletion.
	for i := len(e.gaps) - 1; i >= 0 && !c.stop; i-- {
		if g := &e.gaps[i]; g.tab == ed.tab && g.rows == ed.rows && ed.brought(c, g.first()) && !e.fills(i, ed) {
			e.resolve(c, i)
		}
	}
	if c.stop {
		return
	}
	e.pass(e.newPass(c, ed.tab, ed.rows, ed.tr.Tab), []formula.Transform{ed.tr}, len(e.gaps), ed)
}

// fills reports whether an insert, given for the live grid, puts exactly
// the rows of gap i back into it.
func (e *undoEntry) fills(i int, ed *logEntry) bool {
	g := &e.gaps[i]
	cur := []formula.Transform{ed.tr}
	for k := len(e.gaps) - 1; k > i; k-- {
		if newer := &e.gaps[k]; newer.tab == g.tab && newer.rows == g.rows {
			cur, _ = liftEdits(cur, newer.at, newer.n, insertsAfter(nil, cur, newer.at, newer.above, newer.below, newer.aboveGone, ed))
		}
	}
	return cur[0].At == g.at && cur[0].Count == g.n
}

// resolve applies the deletion of gap i after all.
func (e *undoEntry) resolve(c *followRun, i int) {
	if c.stop {
		return
	}
	g := e.gaps[i]
	p := e.newPass(c, g.tab, g.rows, g.name)
	e.closeGap(i)
	c.touched()
	kind := formula.DeleteCols
	if g.rows {
		kind = formula.DeleteRows
	}
	e.pass(p, []formula.Transform{{Kind: kind, Tab: g.name, At: g.at, Count: g.n}}, i, nil)
}

// settleIn applies every deletion the entry still keeps as a gap. The
// entry is a copy made for one undo.
func (e *undoEntry) settleIn(c *followRun) {
	for i := len(e.gaps) - 1; i >= 0 && !c.stop; i-- {
		e.resolve(c, i)
	}
}

// pass takes an edit through the entry: through the gaps older than upto,
// the newest first, then to the formulas of live cells, then through the
// steps from the last one back. cur is the edit on the grid that has the
// gaps from upto on filled. insert is the edit when it is a real insert,
// for which a gap can close.
func (e *undoEntry) pass(p *axisPass, cur []formula.Transform, upto int, insert *logEntry) {
	c := p.c
	c.canon = nil
	for i := upto - 1; i >= 0 && !c.stop; i-- {
		g := &e.gaps[i]
		if g.tab != p.tab || g.rows != p.rows {
			continue
		}
		if insert != nil && cur[0].At == g.at && cur[0].Count == g.n && insert.brought(c, g.first()) {
			// The deleted rows are back where they were. For everything
			// older than the gap nothing happened.
			e.closeGap(i)
			c.touched()
			return
		}
		cur, g.at = liftEdits(cur, g.at, g.n, insertsAfter(c, cur, g.at, g.above, g.below, g.aboveGone, insert))
	}
	if !c.charge(int64(len(e.cells)+len(e.steps)) / 16) {
		return
	}
	for i := range e.cells {
		if c.stop {
			return
		}
		r := &e.cells[i]
		// A formula whose own cell a deletion took has ended: it stays as
		// it was then, also when an undo brings the cell back later. Only
		// renames still reach it.
		if r.f == nil || r.pos.Row == gonePos {
			continue
		}
		name := ""
		if r.tab == p.tab {
			name = p.name
		}
		g, pos, alive := c.applyEdits(r.f, name, r.pos, cur)
		if !alive {
			pos = formula.Pos{Row: gonePos, Col: gonePos}
			c.touched()
		}
		r.f, r.pos = c.keep(r.f, g), pos
	}
	// A step saved its formulas on the grid that the later steps of its
	// action had not changed yet, so the edit is rewritten for the grid with
	// their rows in it, from the last step back.
	for j := len(e.steps) - 1; j >= 0 && !c.stop; j-- {
		s := &e.steps[j]
		switch s.kind {
		case stepTabDeleted:
			if formula.FoldName(p.name) == formula.FoldName(s.savedTab.name) {
				continue
			}
			t := s.savedTab
			if !c.charge(int64(len(t.data)) / 16) {
				return
			}
			for row, rd := range t.data {
				if rd.nf == 0 {
					continue
				}
				if !c.charge(int64(len(rd.cells)) / 16) {
					return
				}
				for _, slot := range rd.cells {
					if c.stop {
						return
					}
					if slot.c.f != nil {
						nf, _, _ := c.applyEdits(slot.c.f, "", formula.Pos{Row: t.rows.pos[uint64(row)], Col: t.cols.pos[uint64(slot.col)]}, cur)
						c.hold(slot.c, nf)
					}
				}
			}
		case stepDeleted:
			list := cur
			if s.hole(p.tab, p.rows) {
				var at int32
				list, at = liftEdits(cur, p.ats[j], int32(len(s.ids)), insertsAfter(c, cur, p.ats[j], s.above, s.below, s.aboveGone, insert))
				s.at = int(at)
			}
			e.followSaved(p, j, list)
			cur = list
		}
	}
}

// brought reports whether an insert put the row or column id into its tab.
func (ed *logEntry) brought(c *followRun, id uint64) bool {
	if id == 0 {
		return false
	}
	if c != nil && !c.charge(int64(len(ed.came))/64) {
		return false
	}
	return slices.Contains(ed.came, id)
}

// insertsAfter reports, for an insert exactly at the place at where deleted
// rows or columns return, whether the inserted ones come after the
// returning ones. The row that was below them does and the row that was
// above them does not. New rows go where the returning ones stay next to
// their surviving neighbor: the one above while it stands.
func insertsAfter(c *followRun, cur []formula.Transform, at int32, above, below uint64, aboveGone bool, insert *logEntry) bool {
	if len(cur) != 1 || cur[0].At != at || (cur[0].Kind != formula.InsertRows && cur[0].Kind != formula.InsertCols) {
		return false
	}
	if insert != nil {
		if insert.brought(c, below) {
			return true
		}
		if insert.brought(c, above) {
			return false
		}
	}
	return above != 0 && !aboveGone
}

// followSaved applies the edits, given for the grid of step j, to the
// formulas in the rows or columns the step saved, and to their places on
// the other axis.
func (e *undoEntry) followSaved(p *axisPass, j int, list []formula.Transform) {
	c := p.c
	s := &e.steps[j]
	name := ""
	if s.tab == p.tab {
		name = p.name
	}
	if s.rows {
		if s.colAt == nil || !c.charge(int64(len(s.saved))/16) {
			return
		}
		for k := range s.saved {
			saved := &s.saved[k]
			if s.colAt[k] == nil {
				continue
			}
			if !c.charge(int64(len(saved.rd.cells)) / 16) {
				return
			}
			row := p.ats[j] + saved.pos
			for i, slot := range saved.rd.cells {
				if c.stop {
					return
				}
				if slot.c.f == nil || s.colAt[k][i] == gonePos {
					continue
				}
				nf, pos, alive := c.applyEdits(slot.c.f, name, formula.Pos{Row: row, Col: s.colAt[k][i]}, list)
				c.hold(slot.c, nf)
				if s.colAt[k][i] = pos.Col; !alive {
					s.colAt[k][i] = gonePos
				}
			}
		}
		return
	}
	if s.rowAt == nil || !c.charge(int64(len(s.cells))/16) {
		return
	}
	// The place of a saved cell's column among the deleted ones.
	var among map[uint64]int32
	for k := range s.cells {
		if c.stop {
			return
		}
		saved := &s.cells[k]
		if saved.c == nil || saved.c.f == nil || s.rowAt[k] == gonePos {
			continue
		}
		if among == nil {
			among = make(map[uint64]int32, len(s.ids))
			for i, id := range s.ids {
				among[id] = int32(i)
			}
		}
		at, ok := among[uint64(saved.col)]
		if !ok {
			at = -1
		}
		nf, pos, alive := c.applyEdits(saved.c.f, name, formula.Pos{Row: s.rowAt[k], Col: p.ats[j] + at}, list)
		c.hold(saved.c, nf)
		if s.rowAt[k] = pos.Row; !alive {
			s.rowAt[k] = gonePos
		}
	}
}
