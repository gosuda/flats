package sheet

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// The reference model of the state-machine test (technical design, section
// 22.1). It extends the grid model of model_test.go with what the spec
// gives every row, column, tab and value group: an identity, and the stamp
// of the last write (section 4.1). On those it runs the rules of the spec
// for edits that arrive late (section 7.1), agent states and guards
// (sections 7.3 and 7.4) and undo (section 14, rule 23). It is written from
// the design text and shares only the formula package with the engine.
//
// Where the design leaves an outcome open, the model takes the engine's
// answer, checks it against what the design does say, and counts the case
// under a name (smRunner.adopted), so that a run shows how much it took on
// trust. Those places are marked "adopt" below, and the table at the top
// of statemachine_test.go lists them. Nothing else is taken from the
// engine: whether an action changes the sheet, which steps of an undo
// apply, how many are skipped and why are the model's own.

// smMark is the other-writer mark of a row, a column or a tab that an
// action created (section 14).
type smMark struct {
	by      int // the session that created it
	touched bool
}

// smTab is what the model keeps of a tab beside its grid.
type smTab struct {
	id                                          uint64
	rows, cols                                  []uint64 // ids in position order
	rowBorn, colBorn                            map[uint64]uint64
	born, nameStamp, settingsStamp, structStamp uint64
	frozenRows, frozenCols                      int
	sorts                                       int // sorts of the tab so far
	widths                                      map[uint64]int
	wstamp                                      map[uint64]uint64 // recovered is the stamp of the tab recovery that made the tab, for
	// a tab that a recovery brought back (Appendix B, engine item 26). It
	// stays with the tab when the tab is deleted and returns.
	recovered uint64
}

func (st *smTab) clone() *smTab {
	c := *st
	c.rows, c.cols = slices.Clone(st.rows), slices.Clone(st.cols)
	c.rowBorn, c.colBorn = maps.Clone(st.rowBorn), maps.Clone(st.colBorn)
	c.widths, c.wstamp = maps.Clone(st.widths), maps.Clone(st.wstamp)
	return &c
}

func (st *smTab) axis(rows bool) []uint64 {
	if rows {
		return st.rows
	}
	return st.cols
}

func (st *smTab) bornOf(rows bool) map[uint64]uint64 {
	if rows {
		return st.rowBorn
	}
	return st.colBorn
}

type smModel struct {
	*model
	meta                                         []*smTab // by position, like model.tabs
	seq, resetSeq, tabOrderStamp, tabChangeStamp uint64
	nextTab, nextRow, nextCol                    uint64
	rowMark, colMark, tabMark                    map[uint64][]*smMark // by id, one mark for each action that created it
	lastSort                                     map[uint64]uint64    // row id to the stamp of the last sort that ordered it
	// inPlace holds the stamps of the sorts that ordered rows and moved
	// none of them, for the summary.
	inPlace map[uint64]bool
	renames []formula.Transform
	shadows []*smShadow
	// worlds are the worlds of the undo entries that keep formulas
	// (stateworld_test.go).
	worlds []*smWorld
	// paths counts what kept formulas went through, for the summary.
	paths map[string]int
	lost  []string
	// log is the model's own list of the structural edits of the recent
	// past, with the bounds of section 8.4, and now its clock in seconds.
	// An insert beside a row that is gone is placed by it.
	log      []smLogged
	logFloor uint64
	logBytes int
	now      int64
	// fault is set when a history takes the model where it does not
	// follow the engine: the comparison after the step fails with it.
	fault error
	// collected holds the blank records the tick removed, with their
	// stamps, by tab, row and column id, and guardFloor is the newest stamp
	// among them (section 7.5; Appendix B, engine item 19).
	collected  map[[3]uint64]modelCell
	guardFloor uint64
	// holder reports whether an undo or redo entry of some session holds a
	// stamp of a collected record: a step of the entry wrote that group of
	// the cell, and the record's stamp is the one the step wrote, or
	// stands in for it. The runner, which has the stacks, sets it.
	holder func(tab, row, col uint64, rec *modelCell) bool
}

func newSMModel(date formula.Date) *smModel {
	m := &smModel{model: &model{date: date}, nextTab: 1, nextRow: 1, nextCol: 1,
		rowMark: map[uint64][]*smMark{}, colMark: map[uint64][]*smMark{}, tabMark: map[uint64][]*smMark{}, lastSort: map[uint64]uint64{},
		collected: map[[3]uint64]modelCell{}}
	m.seq = 1
	m.newTab("Sheet1", 0, 1)
	return m
}

func (m *smModel) clone() *smModel {
	c := *m
	c.model = m.model.clone()
	c.meta = make([]*smTab, len(m.meta))
	for i, st := range m.meta {
		c.meta[i] = st.clone()
	}
	// The marks themselves are shared with the undo entries that own
	// them: a mark is only ever set.
	cloneMarks := func(src map[uint64][]*smMark) map[uint64][]*smMark {
		out := make(map[uint64][]*smMark, len(src))
		for id, marks := range src {
			out[id] = slices.Clone(marks)
		}
		return out
	}
	c.rowMark, c.colMark, c.tabMark = cloneMarks(m.rowMark), cloneMarks(m.colMark), cloneMarks(m.tabMark)
	c.lastSort = maps.Clone(m.lastSort)
	c.inPlace = maps.Clone(m.inPlace)
	c.collected = maps.Clone(m.collected)
	c.renames = slices.Clone(m.renames)
	c.shadows = nil
	return &c
}

// revived turns the copy of a tab of a revision into a tab of the live
// sheet, as a rollback and a tab recovery do (sections 16.3 and 16.4):
// every stamp is the given one, and what a revision does not hold, a blank
// record without a format and a default width, is gone.
func revived(mt *modelTab, st *smTab, stamp uint64) {
	st.nameStamp, st.settingsStamp, st.structStamp = stamp, stamp, stamp
	for id, px := range st.widths {
		st.wstamp[id] = stamp
		if px == defaultWidth {
			delete(st.widths, id)
		}
	}
	for id := range st.wstamp {
		if _, has := st.widths[id]; !has {
			delete(st.wstamp, id)
		}
	}
	for _, line := range mt.grid {
		for c, cell := range line {
			if cell == nil {
				continue
			}
			if cell.content.Kind == formula.ContentBlank && cell.format == (formula.Format{}) {
				line[c] = nil
				continue
			}
			cell.cstamp, cell.tstamp = stamp, stamp
			for prop := range cell.pstamp {
				cell.pstamp[prop] = 0
				if cell.format != (formula.Format{}) {
					cell.pstamp[prop] = stamp
				}
			}
		}
	}
}

// recoverTab adds a tab of a revision to the sheet (section 16.4) and
// returns its id. A tab whose id is not live keeps it, with its row and
// column ids; a tab that is live comes back as a copy with new ids. It is
// named as in the revision, or "<name> (restored)" when that name is
// taken, and then its own formulas follow the rename. The model is the
// snapshot the runner kept of the revision, tab by position.
func (m *smModel) recoverTab(w *smWriter, snap *smModel, i int) uint64 {
	mt, st := snap.model.clone().tabs[i], snap.meta[i].clone()
	revived(mt, st, w.stamp)
	// Section 16.4 names no stamps for the cells of a recovered tab. The
	// engine writes them like new content: a cell has a text stamp only
	// when the recovery's own rename rewrites its formula.
	for _, line := range mt.grid {
		for _, cell := range line {
			if cell != nil {
				cell.tstamp = 0
			}
		}
	}
	st.born = w.stamp
	same := m.index(st.id) < 0
	if !same {
		st.id, m.nextTab = m.nextTab, m.nextTab+1
		for k := range st.rows {
			st.rows[k], m.nextRow = m.nextRow, m.nextRow+1
		}
		widths, wstamp := map[uint64]int{}, map[uint64]uint64{}
		for k, old := range st.cols {
			if px, ok := st.widths[old]; ok {
				widths[m.nextCol], wstamp[m.nextCol] = px, w.stamp
			}
			st.cols[k], m.nextCol = m.nextCol, m.nextCol+1
		}
		st.widths, st.wstamp = widths, wstamp
	}
	st.rowBorn, st.colBorn = map[uint64]uint64{}, map[uint64]uint64{}
	for _, id := range st.rows {
		st.rowBorn[id] = w.stamp
	}
	for _, id := range st.cols {
		st.colBorn[id] = w.stamp
	}
	if name := m.restoredName(mt.name); name != mt.name {
		tr := formula.Transform{Kind: formula.RenameTab, Tab: mt.name, Name: name}
		for r, line := range mt.grid {
			for c, cell := range line {
				if cell != nil && cell.f != nil {
					f, _, _ := cell.f.Apply(mt.name, formula.Pos{Row: int32(r), Col: int32(c)}, tr)
					if f.Stored() != cell.f.Stored() {
						cell.tstamp = w.stamp // the rename rewrote it
					}
					cell.f = f
					cell.content.Text = f.Stored()
				}
			}
		}
		mt.name = name
	}
	m.tabs, m.meta = append(m.tabs, mt), append(m.meta, st)
	m.tabOrderStamp = w.stamp
	st.recovered = w.stamp
	// The undo entries that knew the tab do not apply to it any more, and
	// their worlds follow the tab of the revision. What they knew of the
	// tab is set aside, with the deletions they hold open, for when the
	// recovery is undone and that tab comes back (Appendix B, engine item
	// 26). A tab that a recovery put there before is no tab that can come
	// back that way, so the one set aside stays.
	for _, world := range m.worlds {
		shelf := world.tabs[st.id]
		if shelf != nil && shelf.replaced {
			shelf = shelf.shelf
		}
		world.tabs[st.id] = &smWorldTab{name: mt.name, rows: slices.Clone(st.rows), cols: slices.Clone(st.cols), replaced: true, shelf: shelf}
	}
	mark := &smMark{by: w.by}
	m.tabMark[st.id] = append(m.tabMark[st.id], mark)
	w.record(smStep{kind: smTabAdded, tab: st.id, marks: []*smMark{mark}})
	return st.id
}

// smLogged is one structural edit in the model's log: an insert or a
// deletion of rows or columns at a position, a sort, or a rename.
type smLogged struct {
	seq     uint64
	at      int64 // the model's clock when it was made
	tab     uint64
	rows    bool
	kind    formula.TransformKind // 0 for a sort
	pos, n  int
	gone    []uint64
	entries int // ids and moved rows, for the size
}

func (e *smLogged) size() int {
	if e.kind == 0 {
		return 112 + 24*e.entries
	}
	return 112 + 8*e.entries
}

func (m *smModel) logged(e smLogged) {
	e.at = m.now
	m.log = append(m.log, e)
	m.logBytes += e.size()
}

// trimLog drops what the log no longer holds (section 8.4): it keeps 256
// entries, 4 MiB and one hour, and the edits of one action leave together.
func (m *smModel) trimLog() {
	drop := 0
	for drop < len(m.log) && (len(m.log)-drop > 256 || m.logBytes > 4<<20 || m.now-m.log[drop].at > 3600) {
		m.logBytes -= m.log[drop].size()
		m.logFloor = m.log[drop].seq
		drop++
	}
	for drop < len(m.log) && m.log[drop].seq == m.logFloor {
		m.logBytes -= m.log[drop].size()
		drop++
	}
	m.log = slices.Clone(m.log[drop:])
}

func (m *smModel) clearLog(seq uint64) { m.log, m.logBytes, m.logFloor = nil, 0, seq }

// besideGone returns where an insert goes whose row or column is gone
// (section 8.1): where the stretch that was deleted with it was, moved by
// the inserts and deletions of its tab since. Rows inserted at that very
// place since lie above it. ok is false when the log does not reach back
// to the action's base or holds no deletion of the id after it; the
// operation is ignored then.
func (m *smModel) besideGone(tab uint64, rows bool, id, base uint64) (int, bool) {
	m.trimLog()
	if base < m.logFloor {
		return 0, false
	}
	for i := range m.log {
		e := &m.log[i]
		if e.seq <= base || e.tab != tab || e.rows != rows || !slices.Contains(e.gone, id) {
			continue
		}
		pos := e.pos
		for _, later := range m.log[i+1:] {
			if later.tab != tab || later.rows != rows {
				continue
			}
			switch later.kind {
			case formula.InsertRows, formula.InsertCols:
				if later.pos <= pos {
					pos += later.n
				}
			case formula.DeleteRows, formula.DeleteCols:
				if later.pos+later.n <= pos {
					pos -= later.n
				} else if later.pos < pos {
					pos = later.pos
				}
			}
		}
		return pos, true
	}
	return 0, false
}

// scratch returns a copy that shares nothing the model changes, for trying
// operations that may be refused as a whole. It has no undo entries, so it
// keeps none of their worlds.
func (m *smModel) scratch() *smModel {
	c := m.clone()
	for _, marks := range []map[uint64][]*smMark{c.rowMark, c.colMark, c.tabMark} {
		for _, list := range marks {
			for i, mark := range list {
				copied := *mark
				list[i] = &copied
			}
		}
	}
	c.worlds, c.paths = nil, nil
	return c
}

// newTab places a tab of the default size with the next ids.
func (m *smModel) newTab(name string, at int, stamp uint64) *smTab {
	// A new tab gets its name with it.
	st := &smTab{id: m.nextTab, born: stamp, nameStamp: stamp, rowBorn: map[uint64]uint64{}, colBorn: map[uint64]uint64{}, widths: map[uint64]int{}, wstamp: map[uint64]uint64{}}
	m.nextTab++
	mt := &modelTab{name: name, cols: newTabCols}
	for i := 0; i < newTabRows; i++ {
		mt.grid = append(mt.grid, make([]*modelCell, newTabCols))
		st.rows = append(st.rows, m.nextRow)
		st.rowBorn[m.nextRow] = stamp
		m.nextRow++
	}
	for i := 0; i < newTabCols; i++ {
		st.cols = append(st.cols, m.nextCol)
		st.colBorn[m.nextCol] = stamp
		m.nextCol++
	}
	m.tabs = slices.Insert(m.tabs, at, mt)
	m.meta = slices.Insert(m.meta, at, st)
	return st
}

func (m *smModel) index(tab uint64) int {
	for i, st := range m.meta {
		if st.id == tab {
			return i
		}
	}
	return -1
}

func (m *smModel) byName(name string) int {
	for i, t := range m.tabs {
		if strings.EqualFold(t.name, name) {
			return i
		}
	}
	return -1
}

// restoredName is the name a tab gets back: its own, or "<name>
// (restored)", numbered while that is taken too (rules 23 and 52).
func (m *smModel) restoredName(name string) string {
	if m.byName(name) < 0 {
		return name
	}
	for i := 1; ; i++ {
		candidate := name + " (restored)"
		if i > 1 {
			candidate = fmt.Sprintf("%s (restored %d)", name, i)
		}
		if m.byName(candidate) < 0 {
			return candidate
		}
	}
}

// ---- undo entries ----

const (
	smCell = iota
	smProp
	smWidth
	smSettings
	smName
	smOrder
	smInserted
	smDeleted
	smSorted
	smTabAdded
	smTabDeleted
)

// smRun is one run of rows or columns that a deletion took, with what
// brings them back.
type smRun struct {
	ids    []uint64
	across []uint64       // the ids of the other axis when they were deleted
	cells  [][]*modelCell // rows: per deleted row, its cells by across; columns: per row of across, the cells of the deleted columns
	order  []uint64       // the axis before the deletion
	widths map[uint64]int
	wstamp map[uint64]uint64
	kept   map[[2]uint64]*smKept // the formulas of the cells, by row and column id
	// world is the world the formulas are kept in, made when this run
	// left: the runs of the action that left before it are gone from it.
	world *smWorld
	// repairs are the formulas of the sheet that this run's leaving
	// rewrote, with their form before.
	repairs []smRepair
}

// smRepair is a formula a deletion rewrote, with its form before.
type smRepair struct {
	tab, row, col uint64
	f             *formula.Formula
	reads         []uint64 // the tabs it read
}

type smSavedTab struct {
	mt    *modelTab
	st    *smTab
	order []uint64 // the tab order before the deletion
	kept  map[[2]uint64]*smKept
}

// smStep is one piece of an undo entry: the value a group had before the
// action with the stamps before and after, or a structural edit to take
// back.
type smStep struct {
	kind          int
	tab, row, col uint64
	rows          bool // smInserted, smDeleted: rows or columns
	cell          modelCell
	prop          int
	px            int
	had           bool
	frozen        [2]int
	name          string
	order         []uint64
	before, wrote uint64
	ids           []uint64
	marks         []*smMark // smInserted, smTabAdded: the marks of what the step created
	befores       []uint64
	runs          []smRun
	saved         *smSavedTab
	seq           uint64 // the action's sequence
	renames       int    // the model's count of renames when the step was made
	// world is the world of the step's entry, and kept the formula of an
	// smCell step in it (stateworld_test.go).
	world *smWorld
	kept  *smKept
}

type smEntry struct{ steps []smStep }

// smWriter is who applies an edit and what it records.
type smWriter struct {
	by    int // session, 0 for an agent
	stamp uint64
	rec   *smEntry // nil for an agent
	world *smWorld // the world of rec, once the entry keeps a formula
	// worlds are all the worlds the action made: a deletion makes one for
	// each run.
	worlds []*smWorld
	// structBefore is, for an undo or redo, the structure stamp each tab
	// had before it.
	structBefore map[uint64]uint64
	// settled and tracked are, for an undo, the formulas and the places of
	// the stretches it brings back (prepare).
	settled map[*smKept]*formula.Formula
	tracked map[*smRun]int
	changed bool
	// The link of stamps of the session that undoes (section 14): for a
	// value group, the stamp an undo of the session replaced and the one
	// it wrote instead.
	link map[smLink]uint64
	// applied and skipped count the steps of an undo, and reason is why
	// the first skipped one was skipped.
	applied, skipped int
	reason           string
	// followed counts the kept formulas the model followed by itself.
	followed int
	// groups are the cell groups the action has recorded a step for.
	groups map[string]bool
}

func (w *smWriter) record(s smStep) {
	w.changed = true
	if w.rec != nil && (s.kind == smCell || s.kind == smProp) {
		// An action that writes a group of a cell twice has one value
		// before to give back: the first.
		if w.groups == nil {
			w.groups = map[string]bool{}
		}
		if g := s.group(); w.groups[g] {
			return
		} else {
			w.groups[g] = true
		}
	}
	if w.rec != nil {
		s.seq = w.stamp
		w.rec.steps = append(w.rec.steps, s)
	}
}

// smLink names a stamp of one value group.
type smLink struct {
	group string
	stamp uint64
}

// eff follows the links of a stamp the session wrote on a group to the
// stamp that stands for it now.
func (w *smWriter) eff(group string, stamp uint64) uint64 {
	for range 1000 {
		next, ok := w.link[smLink{group, stamp}]
		if !ok {
			return stamp
		}
		stamp = next
	}
	return stamp
}

func (w *smWriter) relink(group string, from uint64) { w.link[smLink{group, from}] = w.stamp }

// group names the value group of a step.
func (s *smStep) group() string {
	switch s.kind {
	case smCell:
		return fmt.Sprint("content/", s.tab, "/", s.row, "/", s.col)
	case smProp:
		return fmt.Sprint("format/", s.tab, "/", s.row, "/", s.col, "/", s.prop)
	case smWidth:
		return fmt.Sprint("width/", s.tab, "/", s.col)
	case smSettings:
		return fmt.Sprint("settings/", s.tab)
	case smName:
		return fmt.Sprint("name/", s.tab)
	}
	return "order"
}

// ---- cells ----

func (m *smModel) pos(ti int, rows bool, id uint64) int {
	return slices.Index(m.meta[ti].axis(rows), id)
}

func (m *smModel) at(ti int, row, col uint64) (r, c int, ok bool) {
	r, c = m.pos(ti, true, row), m.pos(ti, false, col)
	return r, c, r >= 0 && c >= 0
}

// touch sets the other-writer mark of what holds a cell.
func (m *smModel) touch(w *smWriter, ti int, row, col uint64) {
	for _, marks := range [][]*smMark{m.rowMark[row], m.colMark[col], m.tabMark[m.meta[ti].id]} {
		for _, mark := range marks {
			if mark.by != w.by {
				mark.touched = true
			}
		}
	}
}

// recollected returns the blank record the tick collected from a cell
// that has no record, when a write gives the cell a record again: the cell
// is then as if its record had never gone, with every stamp it had
// (section 7.5; Appendix B, engine item 19). That holds while an undo
// entry holds a stamp of the record, and not in a tab that a recovery
// replaced since the record was written. Otherwise the new record starts
// empty. The record is the cell's from then on, and no collected one.
func (m *smModel) recollected(ti int, row, col uint64) *modelCell {
	key := [3]uint64{m.meta[ti].id, row, col}
	rec, ok := m.collected[key]
	if !ok {
		return nil
	}
	newest := rec.cstamp
	for _, stamp := range rec.pstamp {
		newest = max(newest, stamp)
	}
	if newest < m.meta[ti].recovered || m.holder == nil || !m.holder(key[0], row, col, &rec) {
		return nil
	}
	delete(m.collected, key)
	if m.paths != nil {
		m.paths["a collected blank record that a write brought back"]++
	}
	return &rec
}

// writeContent replaces the content of a cell. f is the formula, already
// at the cell's position, when the content is one.
func (m *smModel) writeContent(w *smWriter, ti int, row, col uint64, content formula.Content, f *formula.Formula) {
	r, c, _ := m.at(ti, row, col)
	mt := m.tabs[ti]
	old := mt.grid[r][c]
	step := smStep{kind: smCell, tab: m.meta[ti].id, row: row, col: col, wrote: w.stamp, renames: len(m.renames)}
	cell := &modelCell{}
	if old != nil {
		step.cell, step.before = *old, old.cstamp
		*cell = *old
		step.kept = m.keep(w, step.tab, row, col, old.f)
		step.world = w.world
	} else if rec := m.recollected(ti, row, col); rec != nil {
		// The write changes the content. The format stamps of the
		// collected record stay the cell's.
		step.before = rec.cstamp
		*cell = *rec
	}
	cell.content, cell.f, cell.val, cell.cstamp = content, f, formula.Value{}, w.stamp
	if f != nil {
		cell.content = formula.Content{Kind: formula.ContentFormula, Text: f.Stored()}
		if f.Unreadable() {
			cell.content.Kind = formula.ContentUnreadable
		}
	} else {
		cell.val = content.Value()
	}
	mt.grid[r][c] = cell
	m.touch(w, ti, row, col)
	w.record(step)
}

// typed writes typed text that is no formula in flight: a value, or a
// formula read at the cell's own position.
func (m *smModel) typed(w *smWriter, ti int, row, col uint64, text string) {
	content, number := formula.ParseTyped(text)
	r, c, _ := m.at(ti, row, col)
	if content.Kind == formula.ContentBlank && m.tabs[ti].grid[r][c] == nil {
		// Nothing to clear: a cell that never held anything gets no blank
		// record (section 7.5 keeps one for a cell that was cleared).
		return
	}
	if content.Kind != formula.ContentFormula {
		m.writeContent(w, ti, row, col, content, nil)
		// The entry sets its number format on a cell that has none
		// (rule 5).
		if number != nil && m.tabs[ti].grid[r][c].format.Number == (formula.NumberFormat{}) {
			m.writeProps(w, ti, row, col, 1<<propNumber, formula.Format{Number: *number})
		}
		return
	}
	f, err := formula.Parse(content.Text, formula.Pos{Row: int32(r), Col: int32(c)}, formula.Limits{})
	if err != nil {
		panic(err)
	}
	m.writeContent(w, ti, row, col, formula.Content{}, f)
}

var smProps = [propCount]func(f *formula.Format) any{
	propBold: func(f *formula.Format) any { return &f.Bold }, propItalic: func(f *formula.Format) any { return &f.Italic },
	propUnderline: func(f *formula.Format) any { return &f.Underline }, propStrike: func(f *formula.Format) any { return &f.Strikethrough },
	propTextColor: func(f *formula.Format) any { return &f.TextColor }, propFill: func(f *formula.Format) any { return &f.Fill },
	propAlign: func(f *formula.Format) any { return &f.Align }, propWrap: func(f *formula.Format) any { return &f.Wrap },
	propNumber: func(f *formula.Format) any { return &f.Number },
}

// setProp copies one property of a format.
func setProp(dst *formula.Format, src formula.Format, prop int) {
	switch p := smProps[prop](dst).(type) {
	case *bool:
		*p = *smProps[prop](&src).(*bool)
	case *formula.Color:
		*p = *smProps[prop](&src).(*formula.Color)
	case *formula.Align:
		*p = *smProps[prop](&src).(*formula.Align)
	case *formula.NumberFormat:
		*p = *smProps[prop](&src).(*formula.NumberFormat)
	}
}

// writeProps writes the properties of mask from f into a cell's format.
func (m *smModel) writeProps(w *smWriter, ti int, row, col uint64, mask uint16, f formula.Format) {
	r, c, _ := m.at(ti, row, col)
	mt := m.tabs[ti]
	cell := mt.grid[r][c]
	if cell == nil {
		// The properties of the mask change. The content stamp and the
		// other format stamps of a collected record stay the cell's.
		if cell = m.recollected(ti, row, col); cell == nil {
			cell = &modelCell{}
		}
		mt.grid[r][c] = cell
	}
	before := cell.pstamp
	for prop := 0; prop < propCount; prop++ {
		if mask&(1<<prop) == 0 {
			continue
		}
		step := smStep{kind: smProp, tab: m.meta[ti].id, row: row, col: col, prop: prop, before: before[prop], wrote: w.stamp}
		setProp(&step.cell.format, cell.format, prop)
		setProp(&cell.format, f, prop)
		cell.pstamp[prop] = w.stamp
		w.record(step)
	}
	m.touch(w, ti, row, col)
}

func (m *smModel) setWidth(w *smWriter, ti int, col uint64, px int) {
	st := m.meta[ti]
	old, had := st.widths[col]
	w.record(smStep{kind: smWidth, tab: st.id, col: col, px: old, had: had, before: st.wstamp[col], wrote: w.stamp})
	st.widths[col], st.wstamp[col] = px, w.stamp
}

func (m *smModel) setSettings(w *smWriter, ti int, rows, cols int) {
	st := m.meta[ti]
	w.record(smStep{kind: smSettings, tab: st.id, frozen: [2]int{st.frozenRows, st.frozenCols}, before: st.settingsStamp, wrote: w.stamp})
	st.frozenRows, st.frozenCols, st.settingsStamp = rows, cols, w.stamp
}

// ---- formulas under structural edits ----

// smShadow is a formula in flight: typed on a screen and not yet
// committed. Structural edits rewrite it as if it were in its cell
// (rule 14).
type smShadow struct {
	tab, row, col uint64
	f             *formula.Formula
	pos           formula.Pos
	stale         bool
}

// transform rewrites every formula of the sheet, and the formulas in
// flight, for an insert, a delete or a rename. A formula whose stored form
// changes gets the text stamp.
func (m *smModel) transform(tr formula.Transform, stamp uint64) {
	for ti, t := range m.tabs {
		for r, row := range t.grid {
			for c, cell := range row {
				if cell == nil || cell.f == nil {
					continue
				}
				at := formula.Pos{Row: int32(r), Col: int32(c)}
				if _, alive := tr.Map(t.name, at); !alive {
					continue
				}
				f, _, _ := cell.f.Apply(t.name, at, tr)
				if f.Stored() != cell.f.Stored() {
					cell.tstamp = stamp
				}
				cell.f = f
				cell.content.Text = f.Stored()
			}
		}
		for _, sh := range m.shadows {
			if sh.tab != m.meta[ti].id || sh.stale {
				continue
			}
			if _, alive := tr.Map(t.name, sh.pos); !alive {
				sh.stale = true
				continue
			}
			sh.f, sh.pos, _ = sh.f.Apply(t.name, sh.pos, tr)
		}
	}
}

func (m *smModel) kind(rows, insert bool) formula.TransformKind {
	switch {
	case rows && insert:
		return formula.InsertRows
	case rows:
		return formula.DeleteRows
	case insert:
		return formula.InsertCols
	}
	return formula.DeleteCols
}

// insert puts new rows or columns with the given ids before position at.
// end says that they are added after the last one, which is no structural
// change (section 4.1).
// insert puts rows or columns with the given ids at a position. appended
// says that they are new and go to the end of the tab: no address that
// existed changes its meaning then (rule 36), so no formula is rewritten,
// not even one that reads past the end of the tab, and the formulas undo
// entries keep do not follow.
func (m *smModel) insert(w *smWriter, ti int, rows bool, at int, ids []uint64, appended bool) {
	mt, st := m.tabs[ti], m.meta[ti]
	end := at == len(st.axis(rows))
	if appended {
		m.worldAppended(st.id, rows, ids)
	} else {
		m.logged(smLogged{seq: w.stamp, tab: st.id, rows: rows, kind: m.kind(rows, true), pos: at, n: len(ids), entries: len(ids)})
		m.transform(formula.Transform{Kind: m.kind(rows, true), Tab: mt.name, At: int32(at), Count: int32(len(ids))}, w.stamp)
		var before uint64
		if axis := st.axis(rows); at < len(axis) {
			before = axis[at]
		}
		m.worldInserted(st.id, mt.name, rows, ids, before)
	}
	if rows {
		fresh := make([][]*modelCell, len(ids))
		for i := range fresh {
			fresh[i] = make([]*modelCell, mt.cols)
		}
		mt.grid = slices.Insert(mt.grid, at, fresh...)
		st.rows = slices.Insert(st.rows, at, ids...)
	} else {
		for r := range mt.grid {
			mt.grid[r] = slices.Insert(mt.grid[r], at, make([]*modelCell, len(ids))...)
		}
		mt.cols += len(ids)
		st.cols = slices.Insert(st.cols, at, ids...)
	}
	for _, id := range ids {
		st.bornOf(rows)[id] = w.stamp
	}
	if !end {
		st.structStamp = w.stamp
	}
}

// create inserts rows or columns with new ids, as an action or a call does.
func (m *smModel) create(w *smWriter, ti int, rows bool, at, n int) {
	ids := make([]uint64, n)
	next := &m.nextCol
	if rows {
		next = &m.nextRow
	}
	for i := range ids {
		ids[i] = *next
		*next++
	}
	m.insert(w, ti, rows, at, ids, at == len(m.meta[ti].axis(rows)))
	w.record(smStep{kind: smInserted, tab: m.meta[ti].id, rows: rows, ids: ids, marks: m.mark(w, rows, ids)})
}

// mark gives rows or columns that an action created, or brought back, the
// other-writer mark of that action. It is one mark for all of them: the
// undo removes them together, or not at all once another writer has
// written into any of them (section 14).
func (m *smModel) mark(w *smWriter, rows bool, ids []uint64) []*smMark {
	marks := m.colMark
	if rows {
		marks = m.rowMark
	}
	mark := &smMark{by: w.by}
	for _, id := range ids {
		marks[id] = append(marks[id], mark)
	}
	return []*smMark{mark}
}

// reads returns the ids of the tabs a formula reads, its own included.
func (m *smModel) reads(ti int, f *formula.Formula, at formula.Pos) []uint64 {
	out := []uint64{m.meta[ti].id}
	f.Refs(at, func(r formula.Ref) {
		if r.Tab == "" {
			return
		}
		if i := m.byName(r.Tab); i >= 0 && !slices.Contains(out, m.meta[i].id) {
			out = append(out, m.meta[i].id)
		}
	})
	return out
}

// remove deletes the rows or columns at the given positions, which are in
// order. It records one run for each stretch of neighbors.
func (m *smModel) remove(w *smWriter, ti int, rows bool, positions []int) {
	if len(positions) == 0 {
		return
	}
	mt, st := m.tabs[ti], m.meta[ti]
	step := smStep{kind: smDeleted, tab: st.id, rows: rows, renames: len(m.renames)}
	order := slices.Clone(st.axis(rows))
	// From the last stretch to the first, so that positions stay valid.
	// Each stretch is a step of its own: what it does to the formulas of
	// the stretches that leave after it is part of what they are kept as.
	for hi := len(positions); hi > 0; {
		lo := hi - 1
		for lo > 0 && positions[lo-1] == positions[lo]-1 {
			lo--
		}
		at, n := positions[lo], hi-lo
		hi = lo
		run := smRun{ids: slices.Clone(st.axis(rows)[at : at+n]), order: order, widths: map[uint64]int{}, wstamp: map[uint64]uint64{}, kept: map[[2]uint64]*smKept{}}
		tr := formula.Transform{Kind: m.kind(rows, false), Tab: mt.name, At: int32(at), Count: int32(n)}
		m.logged(smLogged{seq: w.stamp, tab: st.id, rows: rows, kind: tr.Kind, pos: at, n: n, gone: run.ids, entries: n})
		// The formulas of the sheet with their form before the stretch
		// leaves and the tabs they read from where they lie now.
		type formulaBefore struct {
			f     *formula.Formula
			reads []uint64
		}
		before := map[*modelCell]formulaBefore{}
		for xi, t := range m.tabs {
			for r, row := range t.grid {
				for c, cell := range row {
					if cell != nil && cell.f != nil {
						before[cell] = formulaBefore{cell.f, m.reads(xi, cell.f, formula.Pos{Row: int32(r), Col: int32(c)})}
					}
				}
			}
		}
		// The formulas of the cells that go are kept in a world made
		// before they leave the sheet.
		if w.rec != nil {
			run.world = m.newWorld(w)
		}
		for r, line := range mt.grid {
			for c, cell := range line {
				if p := map[bool]int{true: r, false: c}[rows]; cell != nil && cell.f != nil && p >= at && p < at+n {
					if k := m.keepIn(run.world, st.id, st.rows[r], st.cols[c], cell.f); k != nil {
						run.kept[[2]uint64{st.rows[r], st.cols[c]}] = k
					}
				}
			}
		}
		var above uint64
		if at > 0 {
			above = st.axis(rows)[at-1]
		}
		var below uint64
		if at+n < len(st.axis(rows)) {
			below = st.axis(rows)[at+n]
		}
		m.worldDeleted(w.worlds, st.id, mt.name, rows, run.ids, above, below)
		if rows {
			run.across = slices.Clone(st.cols)
			for _, row := range mt.grid[at : at+n] {
				run.cells = append(run.cells, slices.Clone(row))
			}
		} else {
			run.across = slices.Clone(st.rows)
			for _, row := range mt.grid {
				run.cells = append(run.cells, slices.Clone(row[at:at+n]))
			}
			for _, id := range run.ids {
				if px, ok := st.widths[id]; ok {
					run.widths[id], run.wstamp[id] = px, st.wstamp[id]
				}
				delete(st.widths, id)
				delete(st.wstamp, id)
			}
		}
		m.transform(tr, w.stamp)
		if rows {
			mt.grid = slices.Delete(mt.grid, at, at+n)
			st.rows = slices.Delete(st.rows, at, at+n)
		} else {
			for r := range mt.grid {
				mt.grid[r] = slices.Delete(mt.grid[r], at, at+n)
			}
			mt.cols -= n
			st.cols = slices.Delete(st.cols, at, at+n)
		}
		for _, id := range run.ids {
			delete(st.bornOf(rows), id)
		}
		// The formulas the stretch rewrote. The form before is right at
		// the place the cell had before, which is where it is when the
		// rows are back.
		for xi, t := range m.tabs {
			for r, row := range t.grid {
				for c, cell := range row {
					was, had := before[cell]
					if cell == nil || cell.f == nil || !had || was.f.Stored() == cell.f.Stored() {
						continue
					}
					xs := m.meta[xi]
					run.repairs = append(run.repairs, smRepair{tab: xs.id, row: xs.rows[r], col: xs.cols[c], f: was.f, reads: was.reads})
				}
			}
		}
		step.runs = append(step.runs, run)
	}
	st.structStamp = w.stamp
	w.record(step)
}

// reorder writes the given rows into the given positions, which are in
// order, and stamps them as sorted. undo is set for the undo of a sort.
func (m *smModel) reorder(w *smWriter, ti int, positions []int, ids []uint64, undo bool) {
	mt, st := m.tabs[ti], m.meta[ti]
	step := smStep{kind: smSorted, tab: st.id, wrote: w.stamp}
	moved := false
	rows := make([][]*modelCell, len(ids))
	from := make([]int, len(ids))
	for i, id := range ids {
		from[i] = slices.Index(st.rows, id)
		rows[i] = mt.grid[from[i]]
	}
	for _, p := range positions {
		step.ids = append(step.ids, st.rows[p])
		step.befores = append(step.befores, m.lastSort[st.rows[p]])
	}
	for i, p := range positions {
		mt.grid[p], st.rows[p] = rows[i], ids[i]
		m.lastSort[ids[i]] = w.stamp
		if from[i] == p {
			continue
		}
		moved = true
		for c, cell := range rows[i] {
			if cell != nil && cell.f != nil {
				f := cell.f.Moved(formula.Pos{Row: int32(from[i]), Col: int32(c)}, formula.Pos{Row: int32(p), Col: int32(c)})
				if f.Stored() != cell.f.Stored() {
					cell.tstamp = w.stamp
				}
				cell.f = f
				cell.content.Text = f.Stored()
			}
		}
		for _, sh := range m.shadows {
			if sh.tab == st.id && sh.row == ids[i] && !sh.stale {
				to := formula.Pos{Row: int32(p), Col: sh.pos.Col}
				sh.f, sh.pos = sh.f.Moved(sh.pos, to), to
			}
		}
	}
	switch {
	case len(step.ids) == 0:
		// A sort of no row that may move stamps nothing.
		return
	case !moved:
		// A sort orders every row it names, also when they are in order
		// already, and protects that order from the sort undos of others
		// (engine item 16). So does the undo of a sort that finds its rows
		// in the earlier order: its stamps stand in for those the sort
		// found, and the person's older sort undos read them (engine item
		// 23). Nothing changes place, so the structure of the tab is as it
		// was.
		w.record(step)
		if m.inPlace == nil {
			m.inPlace = map[uint64]bool{}
		}
		m.inPlace[w.stamp] = true
		if m.paths != nil {
			m.paths[map[bool]string{false: "a sort that moved no row", true: "a sort undo that moved no row"}[undo]]++
		}
		return
	}
	st.structStamp = w.stamp
	st.sorts++
	changed := 0
	for i, id := range step.ids {
		if id != ids[i] {
			changed++
		}
	}
	m.logged(smLogged{seq: w.stamp, tab: st.id, rows: true, entries: changed})
	m.worldSorted(st.id, step.ids, ids)
	w.record(step)
}

// sortRows orders the rows at the given positions by the values of a
// column, as rule 22 says. Frozen rows stay out.
func (m *smModel) sortRows(w *smWriter, ti int, positions []int, by int, desc bool) {
	mt, st := m.tabs[ti], m.meta[ti]
	positions = slices.DeleteFunc(slices.Clone(positions), func(p int) bool { return p < st.frozenRows })
	m.recalc()
	type keyed struct {
		id uint64
		v  formula.Value
	}
	rows := make([]keyed, len(positions))
	for i, p := range positions {
		rows[i].id = st.rows[p]
		if c := mt.grid[p][by]; c != nil {
			rows[i].v = c.val
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].v, rows[j].v
		if a.IsBlank() || b.IsBlank() {
			return !a.IsBlank() && b.IsBlank()
		}
		if desc {
			return formula.CompareSort(a, b) > 0
		}
		return formula.CompareSort(a, b) < 0
	})
	ids := make([]uint64, len(rows))
	for i, k := range rows {
		ids[i] = k.id
	}
	m.reorder(w, ti, positions, ids, false)
}

// ---- tabs ----

func (m *smModel) addTab(w *smWriter, name string, at int) {
	st := m.newTab(name, at, w.stamp)
	mark := &smMark{by: w.by}
	m.tabMark[st.id] = append(m.tabMark[st.id], mark)
	m.tabOrderStamp = w.stamp
	m.worldTab(st, name, false)
	w.record(smStep{kind: smTabAdded, tab: st.id, marks: []*smMark{mark}})
}

func (m *smModel) rename(w *smWriter, ti int, name string) {
	mt, st := m.tabs[ti], m.meta[ti]
	tr := formula.Transform{Kind: formula.RenameTab, Tab: mt.name, Name: name}
	w.record(smStep{kind: smName, tab: st.id, name: mt.name, before: st.nameStamp, wrote: w.stamp})
	m.transform(tr, w.stamp)
	m.renames = append(m.renames, tr)
	m.logged(smLogged{seq: w.stamp, tab: st.id, kind: formula.RenameTab})
	m.worldRenamed(st.id, tr)
	mt.name, st.nameStamp, m.tabChangeStamp = name, w.stamp, w.stamp
}

func (m *smModel) order() []uint64 {
	out := make([]uint64, len(m.meta))
	for i, st := range m.meta {
		out[i] = st.id
	}
	return out
}

// moveTab writes the tab order. A move to the place the tab has is a write
// of an equal value, which advances the stamp like any other (section 4.5).
func (m *smModel) moveTab(w *smWriter, ti, to int) {
	w.record(smStep{kind: smOrder, order: m.order(), before: m.tabOrderStamp, wrote: w.stamp})
	mt, st := m.tabs[ti], m.meta[ti]
	m.tabs, m.meta = slices.Delete(m.tabs, ti, ti+1), slices.Delete(m.meta, ti, ti+1)
	m.tabs, m.meta = slices.Insert(m.tabs, to, mt), slices.Insert(m.meta, to, st)
	m.tabOrderStamp = w.stamp
}

func (m *smModel) deleteTab(w *smWriter, ti int) {
	mt, st := m.tabs[ti], m.meta[ti]
	saved := &smSavedTab{mt: mt, st: st, order: m.order(), kept: map[[2]uint64]*smKept{}}
	for r, line := range mt.grid {
		for c, cell := range line {
			if cell != nil && cell.f != nil {
				if k := m.keep(w, st.id, st.rows[r], st.cols[c], cell.f); k != nil {
					k.frozen, k.at, k.gone = true, formula.Pos{Row: int32(r), Col: int32(c)}, mt.name
					saved.kept[[2]uint64{st.rows[r], st.cols[c]}] = k
				}
			}
		}
	}
	m.worldTab(st, mt.name, true)
	// The last names deleted tabs had, for the generator to use again.
	if m.lost = append(m.lost, mt.name); len(m.lost) > 4 {
		m.lost = slices.Clone(m.lost[1:])
	}
	w.record(smStep{kind: smTabDeleted, tab: st.id, saved: saved, world: w.world, renames: len(m.renames)})
	m.tabs, m.meta = slices.Delete(m.tabs, ti, ti+1), slices.Delete(m.meta, ti, ti+1)
	m.tabOrderStamp, m.tabChangeStamp = w.stamp, w.stamp
}

// ---- undo ----

// held returns a formula an entry kept as its undo writes it into the cell
// that lies at at now: followed through every structural edit since the
// entry was made, in the entry's world (stateworld_test.go).
func (m *smModel) held(w *smWriter, world *smWorld, k *smKept, f *formula.Formula, at *formula.Pos) (*formula.Formula, error) {
	if f == nil || k == nil {
		return f, nil
	}
	if world.edits > 0 {
		w.followed++
	}
	return m.settled(world, k, at)
}

// place returns where rows or columns that return go (section 14 and
// Appendix B, engine item 17): after the row that was above them while it
// is in the sheet; else, for rows, before the row that was below them when
// a sort has ordered that row since; else at the place the model tracked
// for them (track).
func (m *smModel) place(ti int, rows bool, run *smRun, seq uint64, tracked int) int {
	axis := m.meta[ti].axis(rows)
	first := slices.Index(run.order, run.ids[0])
	if first > 0 {
		if at := slices.Index(axis, run.order[first-1]); at >= 0 {
			m.count("placement: after the row or column that was above")
			return at + 1
		}
	}
	if last := first + len(run.ids); rows && last < len(run.order) {
		below := run.order[last]
		if at := slices.Index(axis, below); at >= 0 && m.lastSort[below] > seq {
			m.count("placement: before the row that was below, after a sort")
			return at
		}
	}
	m.count("placement: at the tracked place, the upper neighbour gone")
	return min(tracked, len(axis))
}

// track returns the place the rows or columns of a run have while they
// are out of the sheet: where they lie in the run's world once every
// deletion the world keeps apart is applied. The stretches the same
// action took after this one count as present: they return first.
func (m *smModel) track(tab uint64, rows bool, run *smRun) int {
	scratch := run.world.copy()
	for id, wt := range scratch.tabs {
		for _, g := range slices.Clone(wt.gaps) {
			if !g.own {
				scratch.resolve(id, g)
			}
		}
	}
	return slices.Index(*scratch.tabs[tab].axis(rows), run.ids[0])
}

func (m *smModel) count(name string) {
	if m.paths != nil {
		m.paths[name]++
	}
}

// prepare settles the formulas of every stretch of rows or columns the
// entry brings back, and works out the place each has kept, before any of
// them has a place in the sheet again: an undo applies the deletions the
// entry kept apart to the whole entry first.
func (m *smModel) prepare(w *smWriter, e *smEntry) error {
	w.settled, w.tracked = map[*smKept]*formula.Formula{}, map[*smRun]int{}
	for i := range e.steps {
		s := &e.steps[i]
		ti := m.index(s.tab)
		if s.kind != smDeleted || ti < 0 || m.meta[ti].recovered > s.seq {
			continue
		}
		for k := range s.runs {
			run := &s.runs[k]
			w.tracked[run] = m.track(s.tab, s.rows, run)
			for _, kept := range run.kept {
				f, err := m.held(w, run.world, kept, kept.f, nil)
				if err != nil {
					return err
				}
				w.settled[kept] = f
			}
		}
	}
	return nil
}

// restore brings the rows or columns of a deletion back (rule 23).
func (m *smModel) restore(w *smWriter, s *smStep) error {
	ti := m.index(s.tab)
	if ti < 0 {
		return nil
	}
	mt, st := m.tabs[ti], m.meta[ti]
	// The stretches return one by one, the last that left first. What the
	// undo itself brings back is no change by someone else, so the
	// structure stamps count as they were before the undo.
	structBefore := w.structBefore
	// The formulas and the places of the stretches were worked out before
	// the undo brought anything back (prepare).
	settled, tracked := w.settled, w.tracked
	var back [][]uint64
	for i := len(s.runs) - 1; i >= 0; i-- {
		run := &s.runs[i]
		// A formula gets its form before the stretch left back when nobody
		// edited it since and no tab it read changed its rows or columns
		// since. That is decided on the state before the stretch returns.
		var repair []smRepair
		for _, rp := range run.repairs {
			xi := m.index(rp.tab)
			if xi < 0 {
				continue
			}
			r, c, ok := m.at(xi, rp.row, rp.col)
			if !ok {
				continue
			}
			cell := m.tabs[xi].grid[r][c]
			if cell == nil || cell.f == nil || cell.cstamp > s.seq {
				continue
			}
			clean := true
			for _, id := range rp.reads {
				if before, live := structBefore[id]; !live || before > s.seq {
					clean = false
				}
			}
			if clean {
				repair = append(repair, rp)
			}
		}
		at := m.place(ti, s.rows, run, s.seq, tracked[run])
		m.insert(w, ti, s.rows, at, run.ids, false)
		st.structStamp = w.stamp
		back = append(back, run.ids)
		// A width that returns with its column is written by the undo: it
		// gets the undo's stamp, and the session's earlier writes of it
		// follow (the design names no stamp for it).
		for id, px := range run.widths {
			st.widths[id], st.wstamp[id] = px, w.stamp
			w.relink(fmt.Sprint("width/", s.tab, "/", id), run.wstamp[id])
		}
		for k, line := range run.cells {
			for j, saved := range line {
				if saved == nil {
					continue
				}
				var row, col uint64
				if s.rows {
					row, col = run.ids[k], run.across[j]
				} else {
					row, col = run.across[k], run.ids[j]
				}
				r, c, ok := m.at(ti, row, col)
				if !ok {
					continue
				}
				cell := *saved
				if k := run.kept[[2]uint64{row, col}]; k != nil {
					cell.f = settled[k]
				}
				if cell.f != nil {
					// The stretch can return to another place than the
					// one its formulas were kept for, and a reference
					// that place takes past what a formula can name ends
					// there (Appendix B, engine item 23). A reference
					// that only names a tab no longer there keeps its
					// slot.
					cell.f = cell.f.Placed(formula.Pos{Row: int32(r), Col: int32(c)})
					cell.content.Text = cell.f.Stored()
				}
				mt.grid[r][c] = &cell
			}
		}
		for _, rp := range repair {
			xi := m.index(rp.tab)
			r, c, ok := m.at(xi, rp.row, rp.col)
			if !ok {
				continue
			}
			cell := m.tabs[xi].grid[r][c]
			if cell == nil || cell.f == nil {
				continue
			}
			f := rp.f
			for _, tr := range m.renames[s.renames:] {
				f, _, _ = f.Apply("", formula.Pos{Row: int32(r), Col: int32(c)}, tr)
			}
			if f.Stored() != cell.f.Stored() {
				cell.f, cell.tstamp = f, w.stamp
				cell.content.Text = f.Stored()
			}
		}
	}
	// Each stretch that came back is taken away again on its own: a redo
	// skips the one another writer has written into since and removes the
	// others.
	for _, ids := range back {
		w.record(smStep{kind: smInserted, tab: s.tab, rows: s.rows, ids: ids, marks: m.mark(w, s.rows, ids)})
	}
	return nil
}

// undo takes an entry back as a new action of its session (section 14).
// Its steps run from the last to the first.
func (m *smModel) undo(w *smWriter, e *smEntry) error {
	w.structBefore = map[uint64]uint64{}
	for _, st := range m.meta {
		w.structBefore[st.id] = st.structStamp
	}
	if err := m.prepare(w, e); err != nil {
		return err
	}
	for i := len(e.steps) - 1; i >= 0; i-- {
		s := &e.steps[i]
		ti := m.index(s.tab)
		done := false
		// why is the reason of a step that is skipped (section 14 and
		// Appendix B, engine item 22).
		why := "changed"
		// A recovery replaced the tab the step knew (engine item 26).
		kind := s.kind
		if kind != smOrder && ti >= 0 && m.meta[ti].recovered > s.seq {
			why, kind = "recovered", -1
		}
		switch kind {
		case smCell, smProp:
			if ti < 0 {
				break
			}
			r, c, ok := m.at(ti, s.row, s.col)
			if !ok {
				break
			}
			cell := m.tabs[ti].grid[r][c]
			if rec, ok := m.collected[[3]uint64{s.tab, s.row, s.col}]; ok && cell == nil {
				// The blank record of the cell was collected. It is back
				// with the stamps it had when one of them is still the
				// one this entry wrote: the cell is as the session left
				// it. Another record stays away, and the step is skipped.
				for k := range e.steps {
					x := &e.steps[k]
					if (x.kind != smCell && x.kind != smProp) || x.tab != s.tab || x.row != s.row || x.col != s.col {
						continue
					}
					stamp := rec.cstamp
					if x.kind == smProp {
						stamp = rec.pstamp[x.prop]
					}
					if stamp == w.eff(x.group(), x.wrote) {
						back := rec
						cell = &back
						m.tabs[ti].grid[r][c] = cell
						delete(m.collected, [3]uint64{s.tab, s.row, s.col})
						if m.paths != nil {
							m.paths["a collected blank record that an undo brought back"]++
						}
						break
					}
				}
				if cell == nil && m.paths != nil {
					m.paths["an undo step skipped at a collected blank record of another writer"]++
				}
			}
			var now uint64
			if cell != nil {
				now = cell.cstamp
				if s.kind == smProp {
					now = cell.pstamp[s.prop]
				}
			}
			if now != w.eff(s.group(), s.wrote) {
				break
			}
			if s.kind == smProp {
				m.writeProps(w, ti, s.row, s.col, 1<<s.prop, s.cell.format)
			} else {
				f, err := m.held(w, s.world, s.kept, s.cell.f, &formula.Pos{Row: int32(r), Col: int32(c)})
				if err != nil {
					return err
				}
				m.writeContent(w, ti, s.row, s.col, s.cell.content, f)
			}
			w.relink(s.group(), s.before)
			done = true
		case smWidth:
			if ti < 0 || m.pos(ti, false, s.col) < 0 || m.meta[ti].wstamp[s.col] != w.eff(s.group(), s.wrote) {
				break
			}
			px := s.px
			if !s.had {
				px = defaultWidth
			}
			m.setWidth(w, ti, s.col, px)
			w.relink(s.group(), s.before)
			done = true
		case smSettings:
			if ti < 0 || m.meta[ti].settingsStamp != w.eff(s.group(), s.wrote) {
				break
			}
			m.setSettings(w, ti, s.frozen[0], s.frozen[1])
			w.relink(s.group(), s.before)
			done = true
		case smName:
			if ti < 0 || m.meta[ti].nameStamp != w.eff(s.group(), s.wrote) {
				break
			}
			// A name another tab has taken meanwhile stays with that tab.
			if i := m.byName(s.name); i >= 0 && i != ti {
				why = "tab_name_taken"
				break
			}
			m.rename(w, ti, s.name)
			w.relink(s.group(), s.before)
			done = true
		case smOrder:
			if m.tabOrderStamp != w.eff(s.group(), s.wrote) {
				break
			}
			before := m.order()
			var tabs []*modelTab
			var meta []*smTab
			for _, id := range s.order {
				if i := m.index(id); i >= 0 {
					tabs, meta = append(tabs, m.tabs[i]), append(meta, m.meta[i])
				}
			}
			if len(tabs) != len(m.tabs) {
				break
			}
			w.record(smStep{kind: smOrder, order: before, before: m.tabOrderStamp, wrote: w.stamp})
			m.tabs, m.meta, m.tabOrderStamp = tabs, meta, w.stamp
			w.relink(s.group(), s.before)
			done = true
		case smInserted:
			if ti < 0 {
				break
			}
			var positions []int
			touched := s.marks[0].touched
			for _, id := range s.ids {
				if p := m.pos(ti, s.rows, id); p >= 0 {
					positions = append(positions, p)
				}
			}
			if touched || len(positions) == 0 || len(positions) == len(m.meta[ti].axis(s.rows)) {
				switch {
				case touched:
					why = "written"
				case len(positions) > 0:
					why = "empty_tab"
				}
				break
			}
			slices.Sort(positions)
			m.remove(w, ti, s.rows, positions)
			done = true
		case smDeleted:
			if ti < 0 {
				break
			}
			if err := m.restore(w, s); err != nil {
				return err
			}
			// Each stretch of a deletion is a step of its own.
			w.applied += len(s.runs) - 1
			done = true
		case smSorted:
			if ti < 0 {
				break
			}
			st := m.meta[ti]
			var positions []int
			var ids []uint64
			guarded, inPlace := true, false
			for _, id := range s.ids {
				p := slices.Index(st.rows, id)
				if p < 0 {
					continue
				}
				if m.lastSort[id] != w.eff(fmt.Sprint("sort/", id), s.wrote) {
					guarded, inPlace = false, inPlace || m.inPlace[m.lastSort[id]]
				}
				if p >= st.frozenRows {
					positions, ids = append(positions, p), append(ids, id)
				}
			}
			if !guarded {
				why = "sorted"
				if inPlace && m.paths != nil {
					m.paths["a sort undo skipped at a later sort that moved no row"]++
				}
				break
			}
			// The step applies also when no row is left to move: it changes
			// nothing then, and the undo takes no sequence for it. Rows in
			// the earlier order already stay in place and are stamped.
			done = true
			slices.Sort(positions)
			if len(ids) > 0 {
				m.reorder(w, ti, positions, ids, true)
			}
			// Every row the undo ordered carries its stamp, moved or not,
			// which stands in for the stamp the row had before the sort
			// (section 14, Appendix B, engine item 23). A row frozen since
			// keeps the stamp it has.
			for k, stamp := range s.befores {
				if id := s.ids[k]; m.lastSort[id] == w.stamp {
					w.relink(fmt.Sprint("sort/", id), stamp)
				}
			}
		case smTabAdded:
			if ti < 0 || len(m.tabs) == 1 || s.marks[0].touched {
				switch {
				case ti < 0:
				case s.marks[0].touched:
					why = "written"
				default:
					why = "last_tab"
				}
				break
			}
			m.deleteTab(w, ti)
			done = true
		case smTabDeleted:
			if ti >= 0 {
				// A tab whose id is live is never brought back.
				if m.meta[ti].recovered > s.seq {
					why = "recovered"
				}
				break
			}
			// After the surviving tab that was before it, else before the
			// surviving one that was after it (rule 23).
			at, was := -1, slices.Index(s.saved.order, s.tab)
			for k := was - 1; k >= 0 && at < 0; k-- {
				if i := m.index(s.saved.order[k]); i >= 0 {
					at = i + 1
				}
			}
			for k := was + 1; k < len(s.saved.order) && at < 0; k++ {
				at = m.index(s.saved.order[k])
			}
			if at < 0 {
				// Neither neighbour is left: the position it had.
				at = min(was, len(m.tabs))
			}
			mt, st := s.saved.mt, s.saved.st.clone()
			tab := &modelTab{name: m.restoredName(mt.name), cols: mt.cols}
			for r, row := range mt.grid {
				line := make([]*modelCell, len(row))
				for c, saved := range row {
					if saved == nil {
						continue
					}
					cell := *saved
					f, err := m.held(w, s.world, s.saved.kept[[2]uint64{st.rows[r], st.cols[c]}], cell.f, &formula.Pos{Row: int32(r), Col: int32(c)})
					if err != nil {
						return err
					}
					if f != nil && tab.name != mt.name {
						// The tab returns under another name, and its own
						// formulas name it by that, as those of a
						// recovered tab do (section 16.4).
						tr := formula.Transform{Kind: formula.RenameTab, Tab: mt.name, Name: tab.name}
						renamed, _, _ := f.Apply(mt.name, formula.Pos{Row: int32(r), Col: int32(c)}, tr)
						if renamed.Stored() != f.Stored() {
							cell.tstamp = w.stamp
						}
						f = renamed
					}
					if cell.f = f; f != nil {
						cell.content.Text = f.Stored()
					}
					line[c] = &cell
				}
				tab.grid = append(tab.grid, line)
			}
			st.born = w.stamp
			m.tabs, m.meta = slices.Insert(m.tabs, at, tab), slices.Insert(m.meta, at, st)
			m.worldTab(st, tab.name, false)
			// A recovery can have put another tab under this id since.
			// A world that knew the tab that is back has it again, with
			// its gaps (Appendix B, engine item 26). The others have its
			// rows and columns.
			for _, world := range m.worlds {
				wt := world.tabs[st.id]
				if wt == nil || !wt.replaced {
					continue
				}
				if old := wt.shelf; old != nil && old.knows(st) {
					old.name, old.deleted = tab.name, false
					world.tabs[st.id] = old
					if m.paths != nil {
						m.paths["a world that has the tab it knew again after a recovery was undone"]++
					}
					continue
				}
				// What the world set aside is of another tab, and its
				// deletions are final.
				if wt.shelf != nil {
					for _, g := range slices.Clone(wt.shelf.gaps) {
						world.resolveShelved(st.id, g)
					}
				}
				wt.rows, wt.cols, wt.gaps, wt.replaced, wt.shelf = slices.Clone(st.rows), slices.Clone(st.cols), nil, false, nil
			}
			m.tabOrderStamp = w.stamp
			mark := &smMark{by: w.by}
			m.tabMark[st.id] = append(m.tabMark[st.id], mark)
			w.record(smStep{kind: smTabAdded, tab: st.id, marks: []*smMark{mark}})
			done = true
		}
		if done {
			w.applied++
		} else {
			if w.skipped++; w.reason == "" {
				w.reason = why
			}
			if s.kind == smDeleted {
				// Each stretch of a deletion is a step of its own.
				w.skipped += len(s.runs) - 1
			}
		}
	}
	return nil
}
