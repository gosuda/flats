package sheet

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
	"unsafe"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// captureSize is what the largest capture of a tab can be charged, as the
// technical design works it out in section 13 from what the tab holds.
// Each text is counted at most a quarter above its bytes and 16 bytes
// besides (TestTextBytesBound).
type captureSize struct {
	rows, cols, cells, formulas int64
	name                        int64 // bytes of the tab's name
	text, textCells             int64 // bytes of the texts of text cells, and their number
	stored                      int64 // bytes of the stored forms of the formulas
	values                      int64 // bytes of the text values of the formulas
	formats, symbols            int64 // the format list the capture keeps, and the bytes of its symbols
	heads                       int64 // what headTabsSize gives for the sheet
	snapshot, csv               bool
}

func roundedText(bytes, count int64) int64 { return bytes + bytes/4 + 16*count }

// formulaRecord is what a text form takes beside its stored text.
var formulaRecord = formula.HeapBytes(unsafe.Sizeof(formula.Formula{}), true)

func (s captureSize) bound() int64 {
	n := capFixedBytes + roundedText(s.name, 1) + captureRowCost*s.rows + capCellBytes*s.cells
	n += roundedText(s.text, s.textCells)
	n += s.formulas*(formulaRecord+capFormulaBytes) + roundedText(s.stored, s.formulas) + capFormsBytes
	n += roundedText(s.values, s.formulas)
	n += capFormatBytes*s.formats + roundedText(s.symbols, 1)
	if !s.snapshot {
		n += formula.HeapBytes(uintptr(s.cells)*4, false) // the sorted format indices while it is built
	} else {
		n += formula.HeapBytes(uintptr(8*s.rows), false) + formula.HeapBytes(uintptr(64*s.cols+256+s.heads), false) + tabHeadBytes
		for _, buf := range []int{serverFrameMax, serverFrameMax, cellMax, 2*4*formula.MaxTextLen + shownMax + 64} {
			n += formula.HeapBytes(uintptr(buf), false)
		}
	}
	if s.csv {
		n += formula.HeapBytes(uintptr(2*4*formula.MaxTextLen+8), false)
	}
	return n
}

// TestTextBytesBound checks the bound captureSize counts each text at,
// for every length a cell's text, a value or a stored form can have.
func TestTextBytesBound(t *testing.T) {
	for n := 1; n <= max(4*formula.MaxTextLen, formula.MaxStoredBytes); n++ {
		if got, most := formula.TextBytes(n), roundedText(int64(n), 1); got > most {
			t.Fatalf("a text of %d bytes takes %d, past %d", n, got, most)
		}
	}
}

// TestLargestCaptureFitsTheBudget works out the largest capture at every
// limit of the sheet at once (technical design, section 13) and checks
// that it fits the host-wide budget with room to spare. A formula's stored
// form takes at most nine bytes for each code point of its text
// (formula.MaxStoredBytes), a text cell four for each code point of its
// text, which its value shares, and a text value four. The format list a
// snapshot copies holds at most twice the style states in use that carry
// a format and wireSlack more, and only cells hold such states, at most
// one each.
func TestLargestCaptureFitsTheBudget(t *testing.T) {
	name := int64(4 * maxTabName)
	limits := captureSize{
		rows: formula.MaxRows, cols: formula.MaxCols, cells: maxCells, formulas: maxFormulas, name: name,
		textCells: maxCells - maxFormulas,
		// All typed text is formula text, which takes more than text.
		stored: formula.MaxStoredBytes / formula.MaxFormulaChars * maxSheetText,
		values: 4 * maxSheetText,
		heads:  64 + maxTabs*(192+6*name),
	}
	snapshot := limits
	snapshot.snapshot = true
	snapshot.formats = 2*maxCells + wireSlack
	snapshot.symbols = 12 * snapshot.formats // three characters of four bytes
	read := limits
	read.csv = true
	read.formats = maxCells + 1
	read.symbols = 12 * read.formats
	// A page being built is charged beside its capture.
	page := pageBound(t)
	t.Logf("a JSON page is charged at most %d bytes while it is built", page)
	const room = 64 << 20
	for _, c := range []struct {
		name string
		n    int64
	}{{"snapshot", snapshot.bound()}, {"read", read.bound() + page}} {
		t.Logf("the largest %s capture is charged at most %d bytes of the budget of %d", c.name, c.n, int64(captureBudget))
		if c.n+room > captureBudget {
			t.Errorf("the largest %s capture is charged up to %d bytes, which leaves less than %d of the budget of %d", c.name, c.n, room, int64(captureBudget))
		}
	}
}

// pageBound is the most a JSON page of a read is charged while it is built
// (technical design, section 13). Everything the page charges stays
// charged until the page is handed on, and only one buffer grows at a
// time, so the most is held either while its cells are added or while its
// result is written.
//
// The rows, the shown text and the formats the page measures together
// take at most the page limit less the envelope, with the brackets of the
// last row besides: a cell that passes it is taken back out, and the
// first cell of a page always fits (checked below). Each buffer grows to
// at most twice what it needed, or 4 KiB, and it needed at most what it
// keeps and the room of the cell it grew for. The row being added and its
// shown text keep no more together than the page's rows and shown text:
// each row of the page is in them, and a row that is left out was
// measured with them. A cell's row takes most for a formula of 8,192 code
// points of four bytes at its cell (section 9.1) with a value of 32,767
// such code points, its shown text most for that value, and every byte
// of a text six as JSON.
//
// The result is allocated at what the page measured, with the widths, the
// head and the cursor, and grows once more to twice that when the last
// formats need more room than they took to measure: no format takes more
// than formatMax. A CSV page grows one exact field at a time under the
// page limit, so it takes less, at most three times 4 MiB.
func pageBound(t testing.TB) int64 {
	t.Helper()
	heap := func(n int) int64 { return formula.HeapBytes(uintptr(n), false) }
	// Two buffers that together needed need take at most this: each twice
	// what it needed or 4 KiB, which the allocator rounds up by under 8
	// KiB.
	pair := func(need int) int64 { return int64(2*need + 2*(4<<10) + 2*(8<<10)) }
	limit := maxPageBytes - pageEnvelope
	value := 4 * formula.MaxTextLen
	cell := 1 + max(3+6*value, 64+6*(4*formula.MaxFormulaChars+value))
	shown := 3 + 6*max(value, shownMax)
	format := formatMax(formula.Format{Number: formula.NumberFormat{Kind: formula.NumberCurrency, Symbol: "\U00020000\U00020000\U00020000"}})
	if cell+shown+formatSlack+format > limit {
		t.Fatalf("the first cell of a page can take %d bytes of its limit of %d", cell+shown+formatSlack+format, limit)
	}
	// From the first cell on: the widths, the map of the lengths of the
	// formats with an entry for each cell, a format and a shown text being
	// written, the row and its shown text, and the rows and the shown text.
	kept := heap(max(2+48*formula.MaxCols, 4<<10)) + formatLenBase + formatLenEntry*maxPageCells +
		heap(max(2*format, 4<<10)) + heap(max(2*max(value, shownMax), 4<<10)) +
		pair(limit+cell+shown) + pair(limit+6+2)
	// While a cell is added: the A1 text of the longest stored form, and
	// the buffer that grows for it, which needed at most the page limit
	// and a cell's room.
	longest, err := formula.ParseStored(longestStored())
	if err != nil {
		t.Fatalf("the longest stored form: %v", err)
	}
	adding := kept + longest.TextForm().TextWork() + heap(limit+cell)
	// While the result is written: the three lists of format runs, with
	// one run for each cell at most, and the result before and after it
	// grows.
	result := limit + 6 + (2 + 48*formula.MaxCols) + 64 + 6*4*maxTabName + 64 + 512
	run := unsafe.Sizeof(struct{ c0, c1, style, r0, r1 int32 }{})
	writing := kept + 3*formula.HeapBytes(uintptr(maxPageCells)*run, false) + heap(result) + heap(2*result)
	return max(adding, writing)
}

// longestStored returns a stored form of MaxStoredBytes. Writing the A1
// text of a form takes most for the longest stored text (Formula.TextWork).
func longestStored() string {
	const slot = "{R[-99999]C[-701]}"
	n := (formula.MaxStoredBytes - 1) / (len(slot) + 1)
	s := "=" + slot + strings.Repeat("+"+slot, n-1)
	return s + strings.Repeat(" ", formula.MaxStoredBytes-len(s))
}

// TestLargestCaptureCountsWhatItHolds builds one tab that holds, for each
// part of a capture, what is charged most for its size: text cells and
// text values of 8,193 code points just past 32 KiB, which the allocator
// rounds up by a quarter; formulas whose references take the longest slot;
// a number in every row with formats of their own whose symbols take
// twelve bytes, and blank records. At full size the rows reach the row
// limit, and the cells, the text cells and the text values a part of
// their limits that keeps the test's memory well under 500 MB: the
// charge of each part grows in proportion to it, which
// TestLargestCaptureFitsTheBudget works out at the limits. Formulas at the
// formula limit with their parsed trees would not fit a test either; each
// text form is charged at its size, which other tests check against the
// heap. The default size takes the same paths with less.
//
// A snapshot's capture and a CSV read's capture are charged within what
// captureSize works out for the tab. Once the tab is deleted, the
// snapshot's capture alone keeps its texts, and the heap gives back what
// it is charged when it lets go: no less, so nothing is left uncounted,
// and not much more, so a text that a cell's content and value share is
// counted once.
func TestLargestCaptureCountsWhatItHolds(t *testing.T) {
	slow(t, "builds a tab at several limits")
	cells := sized(100_000, 4000)
	texts, long, formats, blanks := sized(400, 40), sized(30, 2), sized(5000, 200), sized(10000, 500)
	numbers := cells - 2*texts - long
	e := start(t, unhurried)
	e.mustAgent(jsonf(`{"ops":[{"op":"add_tab","name":"Big"},{"op":"insert_rows","tab":"Big","at":1,"count":%d},{"op":"insert_cols","tab":"Big","at":"Z","count":%d}]}`,
		formula.MaxRows-newTabRows, formula.MaxCols-newTabCols))

	text := strings.Repeat("\U00010400", 8192) + "a"
	for start := 0; start < texts; start += 200 {
		rows := make([]string, 0, 200)
		for i := start; i < min(start+200, texts); i++ {
			rows = append(rows, `[`+strconv.Quote(text)+`]`)
		}
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Big","at":"A%d","rows":[%s]}]}`, start+1, strings.Join(rows, ",")))
	}
	rows := make([]string, texts)
	for i := range rows {
		rows[i] = `[` + strconv.Quote(fmt.Sprintf(`=A%d&"x"`, i+1)) + `]`
	}
	e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Big","at":"B1","rows":[%s]}]}`, strings.Join(rows, ",")))
	// From the last column of the last rows, A1 takes the slot
	// {R[-99999]C[-701]} of 18 bytes for its 2 code points.
	sum := "=A1" + strings.Repeat("+A1", (formula.MaxFormulaChars-3)/3)
	rows = make([]string, long)
	for i := range rows {
		rows[i] = `[` + strconv.Quote(sum) + `]`
	}
	last := formula.Pos{Row: formula.MaxRows - int32(long), Col: formula.MaxCols - 1}
	e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Big","at":%q,"rows":[%s]}]}`, last.A1(), strings.Join(rows, ",")))

	// Numbers fill column C down to the last row, then the columns after
	// it. The first blanks of them are cleared and written again in the
	// columns after the others, so the tab keeps blank records beside a
	// full count of cells.
	place := func(i int) formula.Pos {
		return formula.Pos{Row: int32(i % formula.MaxRows), Col: int32(2 + i/formula.MaxRows)}
	}
	write := func(from, to int) {
		for start := from; start < to; start += maxCallCells {
			end := min(start+maxCallCells, to)
			var ops []string
			for i := start; i < end; {
				j := min(end, (i/formula.MaxRows+1)*formula.MaxRows)
				ops = append(ops, jsonf(`{"op":"write","tab":"Big","at":%q,"rows":[%s]}`, place(i).A1(), strings.TrimSuffix(strings.Repeat("[1],", j-i), ",")))
				i = j
			}
			e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
		}
	}
	write(0, numbers)
	for start := 0; start < blanks; start += maxCallCells {
		end := min(start+maxCallCells, blanks)
		e.mustAgent(jsonf(`{"ops":[{"op":"clear","tab":"Big","ranges":["C%d:C%d"]}]}`, start+1, end))
	}
	write(numbers, numbers+blanks)
	for start := 0; start < formats; start += 3000 {
		ops := make([]string, 0, 3000)
		for i := start; i < min(start+3000, formats); i++ {
			symbol := string([]rune{rune(0x10400 + i%80), rune(0x10400 + i/80%80), rune(0x20000 + i/6400)})
			ops = append(ops, jsonf(`{"op":"format","tab":"Big","ranges":[%q],"format":{"number":{"kind":"currency","symbol":%q,"decimals":2}}}`, place(blanks+i).A1(), symbol))
		}
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	}

	e.in.mu.RLock()
	wb := e.in.wb
	tab := wb.tab("Big")
	size := captureSize{rows: int64(tab.rows.len()), cols: int64(tab.cols.len()), name: int64(len(tab.name)), heads: int64(headTabsSize(wb))}
	records, typed, slots := 0, 0, 0
	for pos, rd := range tab.rowsByPos() {
		if rd == nil {
			continue
		}
		for _, s := range rd.cells {
			records++
			if !wb.counted(s.c) {
				continue
			}
			size.cells++
			if s.c.f != nil {
				size.formulas++
				size.stored += int64(len(s.c.f.Stored()))
				size.values += int64(len(s.c.val.Text()))
				n := s.c.f.TextLen(formula.Pos{Row: int32(pos), Col: tab.cols.pos[uint64(s.col)]})
				typed += n
				if stored := len(s.c.f.Stored()); stored > 9*n {
					t.Errorf("a formula has a stored form of %d bytes for %d code points", stored, n)
				} else if stored > 6*n {
					slots++
				}
			} else if s.c.content.Kind == formula.ContentText {
				size.textCells++
				size.text += int64(len(s.c.content.Text))
				typed += utf8.RuneCountInString(s.c.content.Text)
			}
		}
	}
	for _, f := range wb.wire.list {
		size.symbols += int64(len(f.Number.Symbol))
	}
	snapSize := size
	snapSize.snapshot, snapSize.formats = true, int64(len(wb.wire.list))
	csv, err := wb.capture(ReadQuery{Tab: "Big", Format: "csv"})
	if err != nil {
		e.in.mu.RUnlock()
		t.Fatalf("a CSV read of the tab alone: %v", err)
	}
	csvSize := size
	csvSize.csv, csvSize.formats, csvSize.symbols = true, int64(len(csv.formats)), 0
	for _, f := range csv.formats {
		csvSize.symbols += int64(len(f.Number.Symbol))
	}
	csvCharged := csv.bytes
	csv.release()
	c, head, err := wb.captureTab(tab)
	var enc *snapEncoder
	if err == nil {
		enc, err = newSnapEncoder(c, head)
	}
	e.in.mu.RUnlock()
	if err != nil {
		t.Fatalf("a snapshot of the tab alone: %v", err)
	}
	t.Logf("%d records of %d cells in %d rows, %d formulas, %d characters of typed text, %d bytes of text values, %d formats",
		records, size.cells, size.rows, size.formulas, typed, size.values, snapSize.formats)
	if records <= int(size.cells) || size.cells != int64(cells) || typed > maxSheetText || slots != long {
		t.Fatalf("the tab holds %d records, %d cells, %d characters of typed text and %d formulas of six bytes or more a code point", records, size.cells, typed, slots)
	}
	places := 0
	for _, row := range c.rows {
		places += cap(row.cells)
	}
	if places != int(size.cells) {
		t.Errorf("the snapshot keeps %d places for %d cells", places, size.cells)
	}
	if most := snapSize.bound(); c.bytes > most {
		t.Errorf("the snapshot's capture is charged %d bytes, past %d", c.bytes, most)
	}
	if most := csvSize.bound(); csvCharged > most {
		t.Errorf("the CSV read's capture is charged %d bytes, past %d", csvCharged, most)
	}
	if !measures {
		c.release()
		return
	}

	e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Big"}]}`)
	charged := c.bytes
	held := steadyHeap()
	runtime.KeepAlive(enc)
	c.release()
	freed := held - steadyHeap()
	t.Logf("the snapshot's capture gave back %d bytes and was charged %d", freed, charged)
	if freed > charged+1<<20 {
		t.Errorf("the snapshot's capture keeps %d bytes of the heap and is charged %d", freed, charged)
	}
	if charged > freed+freed/8+1<<20 {
		t.Errorf("the snapshot's capture is charged %d bytes for %d it keeps", charged, freed)
	}
}

// TestJSONPagesStayInTheirBound builds pages that grow their buffers the
// most for what they carry: rows of texts of control characters, which
// take six bytes each as JSON, formulas whose values are such texts, and
// texts of four-byte characters, which a page makes room for at six bytes
// a byte, each cell with a format of its own, with and without their
// shown text, formats and widths, and as CSV. For each it finds the least
// room in the capture budget that the page is built in: what the page is
// charged at its most, within 16 KiB. That is within pageBound. A row of
// control characters alone is charged more than three times the page
// limit: the row it is added in, the rows and the result at once.
func TestJSONPagesStayInTheirBound(t *testing.T) {
	slow(t, "builds pages of 4 MiB many times")
	e := start(t, unhurried)
	control := string(appendString(nil, strings.Repeat("\x01", formula.MaxTextLen)))
	wide := string(appendString(nil, strings.Repeat("\U00010400", formula.MaxTextLen)))
	row := func(at string, n int, cell func(i int) string) {
		cells := make([]string, n)
		for i := range cells {
			cells[i] = cell(i)
		}
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":%q,"rows":[[%s]]}]}`, at, strings.Join(cells, ",")))
	}
	row("A1", 21, func(int) string { return control })
	row("A2", 21, func(i int) string { return fmt.Sprintf(`"=%c1"`, 'A'+i) })
	row("A3", 26, func(int) string { return wide })
	for start := 5; start < 45; start += 20 {
		rows := make([]string, 20)
		for i := range rows {
			rows[i] = "[" + control + "]"
		}
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, start, strings.Join(rows, ",")))
	}
	var ops []string
	for i, r := range []string{"A1:U3", "A5:A44"} {
		rg, _ := formula.ParseRange(r)
		for row := rg.First.Row; row <= rg.Last.Row; row++ {
			for col := rg.First.Col; col <= rg.Last.Col; col++ {
				symbol := string([]rune{rune(0x10400 + i), rune(0x10400 + int(row)), rune(0x10400 + int(col))})
				ops = append(ops, jsonf(`{"op":"format","tab":"Sheet1","ranges":[%q],"format":{"number":{"kind":"currency","symbol":%q,"decimals":2}}}`,
					formula.Pos{Row: row, Col: col}.A1(), symbol))
			}
		}
	}
	e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	e.want("A2", strings.Repeat("\x01", formula.MaxTextLen))

	bound := pageBound(t)
	all := []string{"shown", "formats", "widths"}
	for _, c := range []struct {
		name, at, format string
		include          []string
	}{
		{"control characters", "A1:U1", "", nil},
		{"control characters with everything", "A1:U1", "", all},
		{"formulas of control characters with everything", "A2:U2", "", all},
		{"four-byte characters", "A3:Z3", "", nil},
		{"four-byte characters with everything", "A3:Z3", "", all},
		{"rows of control characters with everything", "A5:A44", "", all},
		{"control characters as CSV", "A1:U1", "csv", nil},
	} {
		e.in.mu.RLock()
		capt, err := e.in.wb.capture(ReadQuery{Tab: "Sheet1", Range: c.at, Format: c.format, Include: c.include})
		if err == nil {
			capt.id, capt.state = strings.Repeat("0", 32), `"state":`+string(appendString(nil, stateToken(e.in.wb.store, capt.seq)))
		}
		e.in.mu.RUnlock()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		kept := captureBytes.Load()
		built := func(room int64) bool {
			release := fillCaptures(t, room)
			defer release()
			_, _, err := capt.page(capt.first.Row, capt.first.Col, 0, func(string) {})
			if err != nil {
				e.wantRefusal(err, "unavailable", "capture_budget")
			}
			return err == nil
		}
		if !built(bound) {
			t.Errorf("%s: the page is charged more than the %d bytes of its bound", c.name, bound)
			capt.release()
			continue
		}
		short, enough := int64(0), bound
		for enough-short > 16<<10 {
			if mid := (short + enough) / 2; built(mid) {
				enough = mid
			} else {
				short = mid
			}
		}
		if got := captureBytes.Load(); got != kept {
			t.Errorf("%s: the pages leave %d bytes charged", c.name, got-kept)
		}
		capt.release()
		t.Logf("%s: the page is charged at most %d bytes, more than %d", c.name, enough, short)
		if c.at == "A1:U1" && c.include == nil && c.format == "" && short <= 3*maxPageBytes {
			t.Errorf("%s: the page is charged %d bytes at most, which leaves its buffers out", c.name, enough)
		}
	}
}
