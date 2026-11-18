package sheet

import (
	"bytes"
	"cmp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/gosuda/flats/internal/sheet/formula"
)

const (
	captureBudget = 1 << 30
	captureTTL    = 60 * time.Second
	maxCaptures   = 4
	// capCellBytes, captureRowCost and capFormatBytes are what a capture
	// counts for one cell record, one row and one entry of the format list
	// it reads its cells against. They cover the records as captureRange
	// allocates them, which a test checks. A format's currency symbol is
	// counted beside it.
	capCellBytes   = 120
	captureRowCost = 40
	capFormatBytes = 64
	// capFixedBytes is what a capture counts for its own record and the
	// cursor and state texts a read gives it, each under 64 bytes.
	capFixedBytes = 512
	// capFormulaBytes is what a capture counts beside the size of each
	// formula it keeps: its entry in the map that finds the formulas the
	// capture already keeps while it is built. capFormsBytes is counted
	// with the first one, for the map itself. They cover everything the
	// map allocates as it grows, a test checks.
	capFormulaBytes = 96
	capFormsBytes   = 1 << 10
)

// captureBytes is the host-wide count of bytes that captures hold: their
// records, the text those reference and the formulas they keep (technical
// design, section 13). A capture is charged while it is built, before each
// allocation.
var captureBytes atomic.Int64

// captureRoom tells those who wait for the capture budget that a capture
// gave bytes back.
var captureRoom = struct {
	mu      sync.Mutex
	freed   chan struct{}
	waiters int
}{freed: make(chan struct{})}

// awaitCaptureRoom returns a channel that is closed when a capture next
// gives bytes back. The caller takes it before it tries a capture, so no
// release between the attempt and the wait is missed, and calls done when
// it stops waiting.
func awaitCaptureRoom() (freed <-chan struct{}, done func()) {
	r := &captureRoom
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waiters++
	return r.freed, func() {
		r.mu.Lock()
		r.waiters--
		r.mu.Unlock()
	}
}

func signalCaptureRoom() {
	r := &captureRoom
	r.mu.Lock()
	if r.waiters > 0 {
		close(r.freed)
		r.freed = make(chan struct{})
	}
	r.mu.Unlock()
}

// capCell is the copy of one cell record a capture holds. Strings are
// shared with the workbook; they are immutable.
type capCell struct {
	col     int32 // position
	id      ColID
	content formula.Content
	f       *formula.Formula // the text form of the cell's formula
	val     formula.Value
	style   int32 // index in the capture's format list
}

type capRow struct {
	pos   int32
	id    RowID
	cells []capCell // by position
}

// capture is a consistent copy of a range of one tab: taken under the read
// lock, encoded outside it.
//
// Ownership: a capture is charged to the host-wide budget from its first
// reservation until its last holder lets go, and for no shorter time. Its
// holders are the request that built it, the set that keeps it for later
// pages, and every request that reads a page of it. hold adds a holder and
// release takes one away; only the last release gives the bytes back. So a
// capture that leaves the kept set, by eviction, expiry or a stop, stays
// charged while a page request still reads it: leaving the set only ends
// its cursor.
type capture struct {
	id      string
	state   string
	seq     uint64
	date    formula.Date
	tab     TabID
	name    string
	first   formula.Pos // top-left of the range
	last    formula.Pos // bottom-right after trimming, or first minus one when empty
	rows    []capRow    // rows of the range that hold records, by position
	formats []formula.Format
	widths  []int // of the columns first.Col to last.Col, when asked
	bytes   int64
	used    time.Time
	csv     bool
	include [3]bool // shown, formats, widths
	limit   int
	rev     int
	holders atomic.Int32 // beside the request that built it

	// paging is held by the request that builds a page: one page of a
	// capture is built at a time, so requests that repeat a cursor take
	// turns and share the one field buffer instead of each building a
	// page beside the others.
	paging sync.Mutex
	// field is the working buffer of one CSV field, of fieldMax bytes,
	// which reserveCSV charged to the capture.
	field    []byte
	fieldMax int
}

// reserve charges n more bytes to the capture. It reports false, and
// charges nothing, when the host-wide budget has no room for them.
func (c *capture) reserve(n int64) bool {
	if captureBytes.Add(n) > captureBudget {
		captureBytes.Add(-n)
		return false
	}
	c.bytes += n
	return true
}

// hold adds a holder to a capture that has one.
func (c *capture) hold() { c.holders.Add(1) }

// release takes one holder away. The last one gives the capture's bytes
// back and wakes those who wait for room. It also lets go of the records,
// so that a capture someone still points at holds no cell and no text once
// the budget counts none.
func (c *capture) release() {
	if c.holders.Add(-1) >= 0 {
		return
	}
	c.rows, c.formats, c.widths, c.field = nil, nil, nil, nil
	if c.bytes != 0 {
		captureBytes.Add(-c.bytes)
		c.bytes = 0
		signalCaptureRoom()
	}
}

// captureRange copies the records of a range. trim cuts blank rows and
// columns off its end: a cell with only a format is blank here. The caller
// holds the read lock.
//
// The copy is charged to the host-wide budget before each allocation: the
// row list, then the cells of each row. When the budget has no room the
// part built so far is given back and the read is refused, so readers that
// are refused together never hold more than the budget allows.
//
// The capture holds a format list of its own with the formats its cells
// use, and its cells name their formats by their index there.
func (wb *Workbook) captureRange(t *Tab, r formula.Range, trim bool) (*capture, error) {
	return wb.captureCells(t, r, trim, false)
}

// captureWire is captureRange for a snapshot, whose cells name their
// formats by their index in the workbook's format list as the stream does.
// The capture holds a copy of the whole list.
func (wb *Workbook) captureWire(t *Tab, r formula.Range) (*capture, error) {
	return wb.captureCells(t, r, false, true)
}

// captureCells is captureRange, and captureWire when wire is true.
func (wb *Workbook) captureCells(t *Tab, r formula.Range, trim, wire bool) (*capture, error) {
	c := &capture{seq: wb.seq, date: wb.date, tab: t.id, first: r.First,
		last: formula.Pos{Row: r.First.Row - 1, Col: r.First.Col - 1}}
	refuse := func() (*capture, error) {
		// Waiting readers are not woken for a part that was never whole:
		// refused readers would otherwise wake each other in turn.
		captureBytes.Add(-c.bytes)
		c.bytes = 0
		return nil, unavailable("capture_budget")
	}
	// The tab may be renamed while the capture lives, so the capture keeps
	// a copy of the name.
	if !c.reserve(capFixedBytes + formula.TextBytes(len(t.name))) {
		return refuse()
	}
	c.name = strings.Clone(t.name)
	// The workbook's list grows into its spare room and is replaced when it
	// is rebuilt, so a capture never shares it. It copies the formats it
	// names, with their symbols, into memory it alone holds.
	c.formats = noFormats
	if wire {
		if !c.reserve(formatsBytes(wb.wire.list, nil)) {
			return refuse()
		}
		c.formats = copyFormats(wb.wire.list, nil)
	}
	clipped, ok := r.Clip(int32(t.rows.len()), int32(t.cols.len()))
	if !ok {
		return c, nil
	}
	byPos := t.rowsByPos()
	// A capture shows formulas and never calculates them, so it keeps the
	// text form of each, once for all the cells that share it. A parsed
	// tree takes up to hundreds of times its text, and the capture would
	// keep it alive after the workbook let go of it.
	forms := map[*formula.Formula]*formula.Formula{}
	held := 0
	for pos := clipped.First.Row; pos <= clipped.Last.Row; pos++ {
		if byPos[pos] != nil {
			held++
		}
	}
	if !c.reserve(int64(held) * captureRowCost) {
		return refuse()
	}
	c.rows = make([]capRow, 0, held)
	for pos := clipped.First.Row; pos <= clipped.Last.Row; pos++ {
		rd := byPos[pos]
		if rd == nil {
			continue
		}
		// A blank record without a format is left out and gets no place.
		// Between two ticks a sheet can hold any number of them, while its
		// cells with content or a format are within the cell limit.
		n := 0
		for _, s := range rd.cells {
			if col := t.cols.pos[uint64(s.col)]; col >= clipped.First.Col && col <= clipped.Last.Col && wb.counted(s.c) {
				n++
			}
		}
		if n == 0 {
			continue
		}
		if !c.reserve(int64(n) * capCellBytes) {
			return refuse()
		}
		row := capRow{pos: pos, id: RowID(t.rows.ids[pos]), cells: make([]capCell, 0, n)}
		var text int64
		for _, s := range rd.cells {
			col := t.cols.pos[uint64(s.col)]
			if col < clipped.First.Col || col > clipped.Last.Col || !wb.counted(s.c) {
				continue
			}
			cell := capCell{col: col, id: s.col, content: s.c.content, val: s.c.val}
			if s.c.style != 0 {
				cell.style = int32(wb.wire.idx[wb.styles.format(s.c.style)])
			}
			if f := s.c.f; f != nil {
				if cell.f = forms[f]; cell.f == nil {
					size := f.TextFormSize() + capFormulaBytes
					if len(forms) == 0 {
						size += capFormsBytes
					}
					if !c.reserve(size) {
						return refuse()
					}
					cell.f = f.TextForm()
					forms[f] = cell.f
				}
				// The content keeps the text form's text, which the size
				// counts, whatever string the cell's content holds.
				cell.content = formulaContent(cell.f)
			}
			row.cells = append(row.cells, cell)
			// The capture counts what the allocator took for each text of
			// a cell. The value of a text cell is its content's text
			// (Content.Value), which counts once; any other value text has
			// memory of its own (Formula.Eval).
			if cell.f == nil {
				text += formula.TextBytes(len(s.c.content.Text))
			}
			if v := s.c.val.Text(); cell.f != nil || !sameText(v, s.c.content.Text) {
				text += formula.TextBytes(len(v))
			}
			if !s.c.blank() || !trim {
				c.last.Row = max(c.last.Row, pos)
				c.last.Col = max(c.last.Col, col)
			}
		}
		// The text is shared with the workbook; a capture keeps it alive.
		if !c.reserve(text) {
			return refuse()
		}
		if len(row.cells) == 0 {
			continue
		}
		sortCells(row.cells)
		c.rows = append(c.rows, row)
	}
	if !trim {
		c.last = clipped.Last
	}
	if !wire && !c.ownFormats(wb.wire.list) {
		return refuse()
	}
	return c, nil
}

// sameText reports whether a and b are the same bytes in memory, which
// one allocation holds.
func sameText(a, b string) bool {
	return len(a) == len(b) && (len(a) == 0 || unsafe.StringData(a) == unsafe.StringData(b))
}

// copyWidths copies the widths of the capture's columns, after trimming.
// It reports false, and charges nothing, when the budget has no room.
func (c *capture) copyWidths(t *Tab) bool {
	n := int(c.last.Col) - int(c.first.Col) + 1
	if n <= 0 {
		return true
	}
	if !c.reserve(formula.HeapBytes(uintptr(n)*unsafe.Sizeof(int(0)), false)) {
		return false
	}
	c.widths = make([]int, n)
	for i := range c.widths {
		c.widths[i] = t.widthOf(ColID(t.cols.ids[int(c.first.Col)+i]))
	}
	return true
}

// tabHeadBytes is what a snapshot's capture counts for its tabHead record.
var tabHeadBytes = formula.HeapBytes(unsafe.Sizeof(tabHead{}), true)

// noFormats is the format list of a capture whose cells have only the
// default format. No capture changes its list.
var noFormats = []formula.Format{{}}

// formatsBytes returns what a capture counts for a copy of the formats of
// wire that used names (all of wire when used is nil), with the default
// format: each format and, in one text, their symbols.
func formatsBytes(wire []formula.Format, used []int32) int64 {
	n, symbols := 0, 0
	eachFormat(wire, used, func(f formula.Format) {
		n++
		symbols += len(f.Number.Symbol)
	})
	if used != nil {
		n++
	}
	return int64(n)*capFormatBytes + formula.HeapBytes(uintptr(symbols), false)
}

// copyFormats returns a list of exactly the formats of wire that used
// names, after the default format, or of all of wire when used is nil. The
// symbols are copied into one text of the list's own. A symbol the
// workbook holds may be part of a longer text, and an allocation for each
// symbol would take sixteen bytes for a symbol of one.
func copyFormats(wire []formula.Format, used []int32) []formula.Format {
	var out []formula.Format
	if used == nil {
		out = make([]formula.Format, 0, len(wire))
	} else {
		out = make([]formula.Format, 1, len(used)+1)
	}
	symbols := 0
	eachFormat(wire, used, func(f formula.Format) { symbols += len(f.Number.Symbol) })
	var b strings.Builder
	b.Grow(symbols)
	eachFormat(wire, used, func(f formula.Format) { b.WriteString(f.Number.Symbol) })
	all := b.String()
	eachFormat(wire, used, func(f formula.Format) {
		n := len(f.Number.Symbol)
		f.Number.Symbol, all = all[:n], all[n:]
		out = append(out, f)
	})
	return out
}

// eachFormat calls fn for the formats of wire that used names, in order, or for
// all of wire when used is nil.
func eachFormat(wire []formula.Format, used []int32, fn func(formula.Format)) {
	if used == nil {
		for _, f := range wire {
			fn(f)
		}
		return
	}
	for _, w := range used {
		fn(wire[w])
	}
}

// ownFormats replaces the workbook's format indices of the capture's cells
// by indices in a list of the formats they use, which it copies. It
// reports false when the budget has no room, and leaves what it charged
// for the caller to give back.
func (c *capture) ownFormats(wire []formula.Format) bool {
	styled := 0
	for i := range c.rows {
		for k := range c.rows[i].cells {
			if c.rows[i].cells[k].style != 0 {
				styled++
			}
		}
	}
	if styled == 0 {
		return true
	}
	// The distinct indices are found by sorting a list of all of them,
	// which lives only while the capture is built.
	scratch := formula.HeapBytes(uintptr(styled)*4, false)
	if !c.reserve(scratch) {
		return false
	}
	used := make([]int32, 0, styled)
	for i := range c.rows {
		for k := range c.rows[i].cells {
			if s := c.rows[i].cells[k].style; s != 0 {
				used = append(used, s)
			}
		}
	}
	slices.Sort(used)
	used = slices.Compact(used)
	if !c.reserve(formatsBytes(wire, used)) {
		return false
	}
	c.formats = copyFormats(wire, used)
	for i := range c.rows {
		for k := range c.rows[i].cells {
			cell := &c.rows[i].cells[k]
			if cell.style != 0 {
				n, _ := slices.BinarySearch(used, cell.style)
				cell.style = int32(n + 1)
			}
		}
	}
	captureBytes.Add(-scratch)
	c.bytes -= scratch
	return true
}

// sortCells orders the cells of a row by position. A row is in the order
// of its column ids, which is the order of positions unless columns were
// inserted: one at a time at the front, they put a row of 702 cells in
// reverse order.
func sortCells(cells []capCell) {
	compared := 0
	slices.SortFunc(cells, func(a, b capCell) int { compared++; return cmp.Compare(a.col, b.col) })
	if w := counting.Load(); w != nil {
		w.ordered.Add(int64(compared))
	}
}

// showKind returns the content kind Show needs: a formula whose outermost
// function gives a date shows as a date without a number format.
func showKind(content formula.Content, f *formula.Formula) formula.ContentKind {
	if f == nil {
		return content.Kind
	}
	if f.DateResult() {
		return formula.ContentDate
	}
	return formula.ContentFormula
}

func (c *capCell) appendShown(dst []byte, formats []formula.Format) []byte {
	return formula.AppendShow(dst, c.val, showKind(c.content, c.f), formats[c.style].Number)
}

// appendValueText appends a value as the wire compares it with the shown
// text: the JSON number, the text, TRUE or FALSE, or the error's name.
func appendValueText(dst []byte, v formula.Value) []byte {
	switch v.Kind() {
	case formula.KindNumber:
		return appendNumber(dst, v.Number())
	case formula.KindText:
		return append(dst, v.Text()...)
	case formula.KindBool:
		if v.Bool() {
			return append(dst, "TRUE"...)
		}
		return append(dst, "FALSE"...)
	case formula.KindError:
		return append(dst, v.Err().String()...)
	}
	return dst
}

func appendValue(dst []byte, v formula.Value) []byte {
	switch v.Kind() {
	case formula.KindNumber:
		return appendNumber(dst, v.Number())
	case formula.KindText:
		return appendString(dst, v.Text())
	case formula.KindBool:
		return strconv.AppendBool(dst, v.Bool())
	case formula.KindError:
		return appendString(dst, v.Err().String())
	}
	return append(dst, "null"...)
}

var kindNames = [...]string{formula.KindBlank: "", formula.KindNumber: "n", formula.KindText: "t", formula.KindBool: "b", formula.KindError: "e"}

// appendCellOut appends the CellOut of Appendix A: the positional array
// [col, kind, value, shown, edit, style] with trailing defaults left out.
// scratch is reused between calls.
func appendCellOut(dst []byte, col ColID, content formula.Content, f *formula.Formula, val formula.Value, format formula.Format, style int, scratch *[]byte) []byte {
	dst = append(dst, '[')
	dst = strconv.AppendUint(dst, uint64(col), 10)
	kind := kindNames[val.Kind()]
	if f != nil {
		kind += "f"
	}
	if kind == "" && style == 0 {
		return append(dst, ']')
	}
	dst = append(dst, ',', '"')
	dst = append(dst, kind...)
	dst = append(dst, '"')
	// The value as text and the shown text are compared in the scratch
	// buffer; neither is copied unless the frame carries it.
	buf := (*scratch)[:0]
	buf = appendValueText(buf, val)
	n := len(buf)
	buf = formula.AppendShow(buf, val, showKind(content, f), format.Number)
	valueText, shown := buf[:n], buf[n:]
	*scratch = buf
	shows := !bytes.Equal(shown, valueText)
	edit, edits := "", false
	if f == nil && !val.IsBlank() {
		edit = formula.EditText(content, formula.Pos{})
		edits = edit != string(shown)
	}
	last := 1
	switch {
	case style != 0:
		last = 5
	case edits:
		last = 4
	case shows:
		last = 3
	case !val.IsBlank():
		last = 2
	}
	if last >= 2 {
		dst = append(dst, ',')
		dst = appendValue(dst, val)
	}
	if last >= 3 {
		dst = append(dst, ',')
		if shows {
			dst = appendString(dst, string(shown))
		} else {
			dst = append(dst, "null"...)
		}
	}
	if last >= 4 {
		dst = append(dst, ',')
		if edits {
			dst = appendString(dst, edit)
		} else {
			dst = append(dst, "null"...)
		}
	}
	if last >= 5 {
		dst = append(dst, ',')
		dst = strconv.AppendInt(dst, int64(style), 10)
	}
	return append(dst, ']')
}

// cut removes s[i:j] from a list that lives on and clears the places it
// frees. A list that is compacted in place otherwise keeps, behind its
// length, pointers to what it dropped, and the collector keeps all of that
// alive although no count holds it any more.
func cut[S ~[]E, E any](s S, i, j int) S { return slices.Delete(s, i, j) }

// captureSet keeps the captures of paged agent reads: at most four per
// sheet, each for 60 seconds after its last page.
type captureSet struct {
	mu     sync.Mutex
	list   []*capture
	closed bool
}

// keep holds a capture for its later pages, as a holder of its own. A set
// that was closed keeps nothing.
func (cs *captureSet) keep(c *capture, now time.Time) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.closed {
		return
	}
	c.hold()
	c.used = now
	if len(cs.list) >= maxCaptures {
		oldest := 0
		for i, other := range cs.list {
			if other.used.Before(cs.list[oldest].used) {
				oldest = i
			}
		}
		cs.list[oldest].release()
		cs.list = cut(cs.list, oldest, oldest+1)
	}
	cs.list = append(cs.list, c)
}

// find returns the kept capture a cursor names, held for the caller, who
// releases it when its page is done.
func (cs *captureSet) find(id string, now time.Time) *capture {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, c := range cs.list {
		if c.id == id && now.Sub(c.used) <= captureTTL {
			c.used = now
			c.hold()
			return c
		}
	}
	return nil
}

// live reports whether a capture can still be read.
func (cs *captureSet) live(now time.Time) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, c := range cs.list {
		if now.Sub(c.used) <= captureTTL {
			return true
		}
	}
	return false
}

func (cs *captureSet) expire(now time.Time) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	// DeleteFunc clears the places it frees, so the list's memory names no
	// capture that left it.
	cs.list = slices.DeleteFunc(cs.list, func(c *capture) bool {
		if now.Sub(c.used) <= captureTTL {
			return false
		}
		c.release()
		return true
	})
}

// close releases every capture and keeps none from then on. A read that
// was under way when the instance stopped cannot leave one behind.
func (cs *captureSet) close() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, c := range cs.list {
		c.release()
	}
	cs.list, cs.closed = nil, true
}
