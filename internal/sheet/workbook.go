package sheet

import (
	"fmt"
	"hash/maphash"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
	"weak"

	"github.com/gosuda/flats/internal/sheet/formula"
)

type (
	TabID uint64
	RowID uint64
	ColID uint64
)

const (
	newTabRows   = 1000
	newTabCols   = 26
	defaultWidth = 104
)

// Cell is one cell record: what the cell holds, its calculated value, its
// style and the stamps of its content.
type Cell struct {
	content formula.Content
	f       *formula.Formula // set when the content is a formula
	val     formula.Value    // the content's value, or the calculated value of a formula
	node    *fnode           // dependency node of a formula cell
	style   uint32
	by      uint32 // session that last wrote the content; 0 for an agent
	seq     uint64 // content stamp: a writer wrote the cell
	textSeq uint64 // text stamp: a rewrite changed the stored formula
}

func (c *Cell) blank() bool { return c.content.Kind == formula.ContentBlank }

type cellSlot struct {
	col ColID
	c   *Cell
}

// rowData holds the cell records of one row, sorted by column id.
type rowData struct {
	cells []cellSlot
	nf    int // formula cells
}

func (rd *rowData) find(col ColID) (int, bool) {
	if len(rd.cells) <= 8 {
		for i, s := range rd.cells {
			if s.col >= col {
				return i, s.col == col
			}
		}
		return len(rd.cells), false
	}
	i := sort.Search(len(rd.cells), func(i int) bool { return rd.cells[i].col >= col })
	return i, i < len(rd.cells) && rd.cells[i].col == col
}

func (rd *rowData) get(col ColID) *Cell {
	if i, ok := rd.find(col); ok {
		return rd.cells[i].c
	}
	return nil
}

type width struct {
	px  int
	seq uint64
}

// Tab is one tab: its axes, cells, widths and settings.
type Tab struct {
	id                         TabID
	name                       string
	born, nameSeq, settingsSeq uint64
	structSeq                  uint64
	rows, cols                 axis
	widths                     map[ColID]width
	frozenRows, frozenCols     int
	data                       map[RowID]*rowData
	chunks                     []RowID // first row id of each row file

	// byPos is data in position order. The writer rebuilds it before it
	// releases the lock, so readers never build it.
	byPos   []*rowData
	byPosOK bool

	// Undo state, in memory only (technical design, section 14).
	marks    map[uint64]writeMark // by row: the latest writes into it, for undo
	colMarks map[uint64]writeMark
	mark     writeMark           // the latest writes into the tab
	lastSort map[RowID]sortStamp // the last sort that ordered the row
	// marksMost and colMarksMost are the most marks each map held since it
	// was made, whose room it keeps (markBytes).
	marksMost, colMarksMost int
	// dataMost, widthsMost and sortsMost are the most entries data, widths
	// and lastSort held before they last lost one, whose room each keeps
	// until the tick makes it again (compactRoom).
	dataMost, widthsMost, sortsMost int
	// recovered is the sequence of the tab recovery that brought the tab
	// back under its id. Undo entries older than it do not apply to the tab.
	recovered uint64
}

type sortStamp struct {
	seq uint64
	by  uint32
}

func (t *Tab) rowsByPos() []*rowData {
	if !t.byPosOK {
		if cap(t.byPos) < len(t.rows.ids) {
			t.byPos = make([]*rowData, len(t.rows.ids))
		}
		// The places behind the new length name no row that left the tab.
		clear(t.byPos[min(len(t.byPos), len(t.rows.ids)):])
		t.byPos = t.byPos[:len(t.rows.ids)]
		for i, id := range t.rows.ids {
			t.byPos[i] = t.data[RowID(id)]
		}
		t.byPosOK = true
	}
	return t.byPos
}

func (t *Tab) cell(row RowID, col ColID) *Cell {
	if rd := t.data[row]; rd != nil {
		return rd.get(col)
	}
	return nil
}

func (t *Tab) cellAt(p formula.Pos) *Cell {
	if p.Row < 0 || p.Col < 0 || int(p.Row) >= len(t.rows.ids) || int(p.Col) >= len(t.cols.ids) {
		return nil
	}
	rd := t.rowsByPos()[p.Row]
	if rd == nil {
		return nil
	}
	return rd.get(ColID(t.cols.ids[p.Col]))
}

func (t *Tab) ensure(row RowID, col ColID) (*Cell, bool) {
	rd := t.data[row]
	if rd == nil {
		rd = &rowData{}
		t.data[row] = rd
		t.byPosOK = false
	}
	i, ok := rd.find(col)
	if ok {
		return rd.cells[i].c, false
	}
	c := &Cell{}
	rd.cells = append(rd.cells, cellSlot{})
	copy(rd.cells[i+1:], rd.cells[i:])
	rd.cells[i] = cellSlot{col, c}
	return c, true
}

// put places a cell record, replacing any record the cell has.
func (t *Tab) put(row RowID, col ColID, c *Cell) {
	rd := t.data[row]
	if rd == nil {
		rd = &rowData{}
		t.data[row] = rd
		t.byPosOK = false
	}
	i, ok := rd.find(col)
	if ok {
		if rd.cells[i].c.f != nil {
			rd.nf--
		}
	} else {
		rd.cells = append(rd.cells, cellSlot{})
		copy(rd.cells[i+1:], rd.cells[i:])
	}
	rd.cells[i] = cellSlot{col, c}
	if c.f != nil {
		rd.nf++
	}
}

func (t *Tab) drop(row RowID, col ColID) {
	rd := t.data[row]
	if rd == nil {
		return
	}
	i, ok := rd.find(col)
	if !ok {
		return
	}
	if rd.cells[i].c.f != nil {
		rd.nf--
	}
	rd.cells = cut(rd.cells, i, i+1)
	if len(rd.cells) == 0 {
		t.dropRow(row)
		t.byPosOK = false
	} else {
		rd.cells = clipped(rd.cells)
	}
}

func (t *Tab) posOf(row RowID, col ColID) (formula.Pos, bool) {
	r, ok := t.rows.pos[uint64(row)]
	if !ok {
		return formula.Pos{}, false
	}
	c, ok := t.cols.pos[uint64(col)]
	return formula.Pos{Row: r, Col: c}, ok
}

func (t *Tab) widthOf(col ColID) int {
	if w, ok := t.widths[col]; ok {
		return w.px
	}
	return defaultWidth
}

// chunkOf returns the first row id of the row file that holds row.
func (t *Tab) chunkOf(row RowID) RowID {
	i := sort.Search(len(t.chunks), func(i int) bool { return t.chunks[i] > row })
	if i == 0 {
		return 0
	}
	return t.chunks[i-1]
}

type counters struct {
	cells    int   // cell records with content or a format
	blanks   int   // blank records without a format, kept for their stamps
	formulas int   // cells that hold a formula
	typed    int64 // code points of typed text, without the text of formulas
	reads    int64 // static reads of all formulas
	calcText int64 // code points of the text results of all formulas
}

// Workbook is a sheet in memory.
type Workbook struct {
	store                       string
	seq, resetSeq, tabChangeSeq uint64
	tabOrderSeq, guardFloor     uint64
	nextTab, nextRow, nextCol   uint64
	limited                     bool
	date                        formula.Date
	tabs                        []*Tab
	byID                        map[TabID]*Tab
	byName                      map[string]*Tab // by folded name
	styles                      styleTable
	formulas                    map[uint64]weak.Pointer[formula.Formula] // shared formulas by a hash of their text (intern)
	formulaSeed                 maphash.Seed
	freed                       *freedFormulas
	deps                        depIndex
	count                       counters
	formulaText                 int64 // code points of every formula's A1 text, while formulaTextOK
	formulaTextOK               bool
	wire                        wireFormats
	pass                        uint32
	files                       fileState
	collected                   blankLog // in memory, for undo guards
	stale                       loadStale
	// restoring is set while a revert takes an action back and puts the
	// counters back from saved values (ops.go).
	restoring bool
}

// loadStale is what a load of sheet.db passed over and the next commit
// deletes (stageStale): with blanks, the blank records no newer than floor,
// which the load took out as the tick would (loadCells), and with styles,
// the style rows no cell of the workbook uses.
type loadStale struct {
	blanks bool
	floor  uint64
	styles bool
}

func newWorkbook(store string) *Workbook {
	return &Workbook{store: store, nextTab: 1, nextRow: 1, nextCol: 1,
		byID: map[TabID]*Tab{}, byName: map[string]*Tab{}, styles: newStyleTable(),
		formulas: map[uint64]weak.Pointer[formula.Formula]{}, formulaSeed: maphash.MakeSeed(), freed: &freedFormulas{},
		deps: newDepIndex(), wire: newWireFormats(), files: newFileState()}
}

func (wb *Workbook) tab(name string) *Tab { return wb.byName[formula.FoldName(name)] }

func (wb *Workbook) tabIndex(t *Tab) int {
	for i, other := range wb.tabs {
		if other == t {
			return i
		}
	}
	return -1
}

// newTab allocates a tab of the given size without placing it.
func (wb *Workbook) newTab(name string, rows, cols int, seq uint64) *Tab {
	t := &Tab{id: TabID(wb.nextTab), name: name, born: seq, nameSeq: seq,
		rows: newAxis(wb.nextRow, rows, seq), cols: newAxis(wb.nextCol, cols, seq),
		widths: map[ColID]width{}, data: map[RowID]*rowData{}, chunks: []RowID{0}}
	wb.nextTab++
	wb.nextRow += uint64(rows)
	wb.nextCol += uint64(cols)
	return t
}

func (wb *Workbook) placeTab(t *Tab, at int) {
	wb.tabs = append(wb.tabs, nil)
	copy(wb.tabs[at+1:], wb.tabs[at:])
	wb.tabs[at] = t
	wb.byID[t.id] = t
	wb.byName[formula.FoldName(t.name)] = t
}

func (wb *Workbook) unplaceTab(t *Tab) {
	i := wb.tabIndex(t)
	wb.tabs = cut(wb.tabs, i, i+1)
	delete(wb.byID, t.id)
	delete(wb.byName, formula.FoldName(t.name))
}

// freeTabName returns the first of "Sheet1", "Sheet2", ... no tab has.
func (wb *Workbook) freeTabName() string {
	for i := 1; ; i++ {
		name := "Sheet" + strconv.Itoa(i)
		if wb.tab(name) == nil {
			return name
		}
	}
}

// restoredName returns name, or "<name> (restored)", numbered when that is
// taken too (spec rules 23 and 52). The result is a valid, free name.
func (wb *Workbook) restoredName(name string) string {
	if wb.tab(name) == nil {
		return name
	}
	for i := 1; ; i++ {
		suffix := " (restored)"
		if i > 1 {
			suffix = " (restored " + strconv.Itoa(i) + ")"
		}
		// The name is cut so that the result keeps to the length of rule 2.
		base := []rune(name)
		if keep := maxTabName - len(suffix); len(base) > keep {
			base = base[:keep]
		}
		if candidate := string(base) + suffix; wb.tab(candidate) == nil {
			return candidate
		}
	}
}

// validTabName checks a name against spec rule 2, uniqueness aside.
func validTabName(name string) bool {
	n := utf8.RuneCountInString(name)
	if n < 1 || n > maxTabName || !utf8.ValidString(name) {
		return false
	}
	if strings.ContainsAny(name, `[]:*?/\`) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	first, last := name[0], name[len(name)-1]
	return first != ' ' && first != '\'' && last != ' ' && last != '\''
}

// formulaKey is the key of a formula among the shared ones: its stored
// text, apart from the readable formula of the same text when it cannot be
// read.
func formulaKey(f *formula.Formula) string {
	if f.Unreadable() {
		return "\x00" + f.Stored()
	}
	return f.Stored()
}

// maxShared bounds the entries of the table of shared formulas: twice the
// formula cells a sheet may hold, and room for formulas that the collector
// is still to free.
const maxShared = 2*maxFormulas + 1024

// freedStep is the most entries of freed formulas that one intern drops.
const freedStep = 256

// intern returns the shared formula for a stored text: the parsed formula
// of that text that a cell or another holder in the sheet still keeps, or
// else f, which becomes the shared one.
//
// The table keeps no parsed formula alive (technical design, section 4.2).
// An entry is a hash of the text and a weak pointer, so neither the formula
// nor its text outlives what holds it, whatever was written before. Once
// the collector has freed a formula, a cleanup notes its hash, and intern
// drops that entry, at most freedStep of them a call. A text that finds the
// table full, or its hash taken by another live text, gets no entry. Its
// cells keep the formula they were given and share it with nothing written
// later.
func (wb *Workbook) intern(f *formula.Formula) *formula.Formula {
	wb.dropFreed(freedStep)
	h := maphash.String(wb.formulaSeed, f.Stored())
	if f.Unreadable() {
		h = ^h // the readable formula of the same text is another one
	}
	if w, ok := wb.formulas[h]; ok {
		if g := w.Value(); g != nil {
			if g.Stored() == f.Stored() && g.Unreadable() == f.Unreadable() {
				return g
			}
			return f
		}
	} else if len(wb.formulas) >= maxShared {
		return f
	}
	wb.formulas[h] = weak.Make(f)
	runtime.AddCleanup(f, wb.freed.add, h)
	return f
}

// dropFreed drops up to most entries whose formulas the collector freed.
func (wb *Workbook) dropFreed(most int) {
	var hashes [freedStep]uint64
	for most > 0 {
		n := wb.freed.take(hashes[:min(most, freedStep)])
		if n == 0 {
			return
		}
		for _, h := range hashes[:n] {
			// A formula of the same text may have taken the entry since.
			if wb.sharedFreed(h) {
				delete(wb.formulas, h)
			}
		}
		most -= n
	}
}

// sharedFreed reports whether the entry of h in the table of shared formulas
// names a formula the collector freed.
func (wb *Workbook) sharedFreed(h uint64) bool {
	if w := counting.Load(); w != nil {
		w.shared.Add(1)
	}
	w, ok := wb.formulas[h]
	return ok && w.Value() == nil
}

// freedFormulas holds the hashes of shared formulas the collector has
// freed, for intern to drop their entries. The collector's cleanups add to
// it from their own goroutine.
type freedFormulas struct {
	mu     sync.Mutex
	hashes []uint64
}

func (q *freedFormulas) add(h uint64) {
	q.mu.Lock()
	q.hashes = append(q.hashes, h)
	q.mu.Unlock()
}

// take moves up to len(into) hashes into into and returns how many.
func (q *freedFormulas) take(into []uint64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := copy(into, q.hashes[max(len(q.hashes)-len(into), 0):])
	q.hashes = q.hashes[:len(q.hashes)-n]
	if len(q.hashes) == 0 && cap(q.hashes) > freedStep {
		q.hashes = nil // the room of a burst of frees goes too
	}
	return n
}

func formulaContent(f *formula.Formula) formula.Content {
	if f.Unreadable() {
		return formula.Content{Kind: formula.ContentUnreadable, Text: f.Stored()}
	}
	return formula.Content{Kind: formula.ContentFormula, Text: f.Stored()}
}

// counted reports whether a record counts toward the cell limit: it holds
// content or a format (spec rule 9).
func (wb *Workbook) counted(c *Cell) bool {
	return !c.blank() || (c.style != 0 && wb.styles.format(c.style) != formula.Format{})
}

// weigh adds a record to the counters with sign 1 and takes it out with -1.
// Every change of a record's content or style is bracketed by the two.
func (wb *Workbook) weigh(c *Cell, sign int) {
	if wb.counted(c) {
		wb.count.cells += sign
	} else {
		wb.count.blanks += sign
	}
	switch c.content.Kind {
	case formula.ContentText, formula.ContentUnreadable:
		wb.count.typed += int64(sign) * int64(utf8.RuneCountInString(c.content.Text))
		if w := counting.Load(); w != nil {
			w.weighed.Add(int64(len(c.content.Text)))
		}
	case formula.ContentFormula:
		wb.formulaTextOK = false
	}
	if c.f != nil {
		wb.count.formulas += sign
	}
}

// loadCell adds a cell that a load of sheet.db or a revision build read to
// its tab, at pos, and refuses the state it builds once that is past a
// limit of the whole sheet (spec rule 9): the cells, the formulas or the
// typed text, a formula counted as its text at its cell as typedText
// counts it. checkLimits refuses an action that would leave such a state,
// so only damage stores one, and the load refuses it before anything
// calculates, serves or stores it. A check looks at the cell's own formula
// once, which the pace of the load counts with the cell's bytes. The
// formulas' text is summed in formulaText, and loaded marks the sum as
// derived once every cell is in.
func (wb *Workbook) loadCell(t *Tab, row RowID, col ColID, pos formula.Pos, c *Cell) error {
	t.put(row, col, c)
	wb.weighCell(c, 1)
	if c.content.Kind == formula.ContentFormula {
		wb.formulaText += int64(c.f.TextLen(pos))
	}
	switch n := wb.count; {
	case n.cells > maxCells:
		return fmt.Errorf("the sheet holds more than %d cells", maxCells)
	case n.formulas > maxFormulas:
		return fmt.Errorf("the sheet holds more than %d formulas", maxFormulas)
	case n.typed+wb.formulaText > maxSheetText:
		return fmt.Errorf("the sheet holds more than %d characters of typed text", maxSheetText)
	}
	return nil
}

// loaded notes that formulaText holds the text of every formula, which
// loadCell summed as the cells came in.
func (wb *Workbook) loaded() { wb.formulaTextOK = true }

// typedText returns the characters of typed text in all cells (spec rule
// 9). A formula counts as its A1 text at its cell, not as its stored form.
// An insert, a delete, a sort and a tab rename change that text without
// touching the cell, so the formulas' part is derived from the state and
// kept until a formula cell or the structure changes.
//
// Deriving it looks through every formula, which counts on the budget b as
// a formula looked through for a structural edit does (technical design,
// section 10.4). ok is false when the deadline ended the look first.
func (wb *Workbook) typedText(b *formula.Budget) (n int64, ok bool) {
	if !wb.formulaTextOK {
		var text, steps int64
		done := wb.eachFormulaPaced(b, func(_ *Tab, _ RowID, _ ColID, pos formula.Pos, c *Cell) bool {
			if c.content.Kind == formula.ContentFormula {
				text += int64(c.f.TextLen(pos))
			}
			if steps += 8 + int64(len(c.f.Stored()))/4; steps >= 64*64 {
				b.Add(steps / 64)
				steps %= 64
			}
			return true
		})
		if !done || !b.Add(steps/64) {
			return 0, false
		}
		wb.formulaText, wb.formulaTextOK = text, true
	}
	return wb.count.typed + wb.formulaText, true
}

// used returns the size of the smallest range from A1 that holds every
// filled cell of the tab: the cells with content.
func (t *Tab) used() (rows, cols int32) {
	for pos, rd := range t.rowsByPos() {
		if rd == nil {
			continue
		}
		for _, s := range rd.cells {
			if s.c.blank() {
				continue
			}
			rows = int32(pos) + 1
			if c := t.cols.pos[uint64(s.col)] + 1; c > cols {
				cols = c
			}
		}
	}
	return rows, cols
}

// grid is the formula.Grid of a workbook. With a pass it reads the values
// that pass has calculated so far in place of the stored ones.
type grid struct {
	wb   *Workbook
	pass uint32
	last *Tab
}

func (g *grid) tabOf(ref formula.TabRef) *Tab {
	if g.last == nil || g.last.id != TabID(ref) {
		g.last = g.wb.byID[TabID(ref)]
	}
	return g.last
}

func (g *grid) Tab(name string) (formula.TabRef, bool) {
	t := g.wb.tab(name)
	if t == nil {
		return 0, false
	}
	return formula.TabRef(t.id), true
}

func (g *grid) Dims(ref formula.TabRef) (rows, cols int32) {
	t := g.tabOf(ref)
	return int32(len(t.rows.ids)), int32(len(t.cols.ids))
}

func (g *grid) Value(ref formula.TabRef, p formula.Pos) formula.Value {
	c := g.tabOf(ref).cellAt(p)
	if c == nil {
		return formula.Value{}
	}
	if n := c.node; n != nil && n.done == g.pass {
		return n.ov
	}
	return c.val
}

func (g *grid) Filled(ref formula.TabRef, p formula.Pos) bool {
	c := g.tabOf(ref).cellAt(p)
	return c != nil && !c.blank()
}

// dims is the formula.Dims of the workbook for a formula in tab own.
func (wb *Workbook) dims(own *Tab) formula.Dims {
	return func(name string) (int32, int32, bool) {
		t := own
		if name != "" {
			if t = wb.tab(name); t == nil {
				return 0, 0, false
			}
		}
		return int32(len(t.rows.ids)), int32(len(t.cols.ids)), true
	}
}
