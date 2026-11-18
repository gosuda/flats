package sheet

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// ReadQuery selects what a read returns (technical design, section 18.1).
// A cursor takes no other selector.
type ReadQuery struct {
	Tab     string   `json:"tab,omitempty"`
	Range   string   `json:"range,omitempty"`
	Format  string   `json:"format,omitempty"`  // "json" or "csv"
	Include []string `json:"include,omitempty"` // "shown", "formats", "widths"
	Limit   int      `json:"limit,omitempty"`   // cells per page, at most 10,000
	Cursor  string   `json:"cursor,omitempty"`
}

const pageEnvelope = 64 << 10

// Read reads the live sheet: the tab list, or the cells of a range in
// pages that all come from one state (spec rule 42).
func (in *Instance) Read(ctx context.Context, q ReadQuery) (json.RawMessage, error) {
	if err := in.enter(); err != nil {
		return nil, err
	}
	defer in.leave()
	if err := in.await(ctx, agentPatience); err != nil {
		return nil, err
	}
	if q.Cursor != "" {
		return in.readPage(q, 0)
	}
	if err := in.syncDate(ctx); err != nil {
		return nil, err
	}
	in.mu.RLock()
	wb := in.wb
	if q.Tab == "" {
		defer in.mu.RUnlock()
		return readTabs(wb, `"state":`+string(appendString(nil, stateToken(wb.store, wb.seq))))
	}
	c, err := wb.capture(q)
	in.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	c.state = `"state":` + string(appendString(nil, stateToken(wb.store, c.seq)))
	return in.firstPage(c)
}

// ReadRevision reads an earlier revision as Read reads the live sheet,
// calculated with the revision's closing date (spec rule 13). core has
// leased the revision's files.
func (in *Instance) ReadRevision(ctx context.Context, n int, files []File, date string, q ReadQuery) (json.RawMessage, error) {
	if err := in.enter(); err != nil {
		return nil, err
	}
	defer in.leave()
	if q.Cursor != "" {
		return in.readPage(q, n)
	}
	wb, err := in.revision(ctx, n, files, date)
	if err != nil {
		return nil, err
	}
	head := `"revision":` + strconv.Itoa(n)
	if q.Tab == "" {
		return readTabs(wb, head)
	}
	c, err := wb.capture(q)
	if err != nil {
		return nil, err
	}
	c.state, c.rev = head, n
	return in.firstPage(c)
}

// firstPage encodes the first page of a capture and keeps the capture for
// its later pages when there are any. The caller's hold on the capture
// ends here.
func (in *Instance) firstPage(c *capture) (json.RawMessage, error) {
	defer c.release()
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	c.id = token
	page, more, err := c.page(c.first.Row, c.first.Col, 0, in.probe)
	if err != nil || !more {
		return page, err
	}
	in.captures.keep(c, in.now())
	return page, nil
}

// parseCursor reads a cursor: "c_", the capture's token, and the row, the
// column and the byte offset of the position the next page starts at.
func parseCursor(s string) (id string, row, col int32, skip int, ok bool) {
	rest, found := strings.CutPrefix(s, "c_")
	parts := strings.Split(rest, "_")
	if !found || len(parts) != 4 || parts[0] == "" {
		return "", 0, 0, 0, false
	}
	var at [3]int64
	for i := range at {
		n, err := strconv.ParseInt(parts[i+1], 10, 64)
		if err != nil || n < 0 || n > math.MaxInt32 {
			return "", 0, 0, 0, false
		}
		at[i] = n
	}
	return parts[0], int32(at[0]), int32(at[1]), int(at[2]), true
}

var errCursor = invalidf("cursor", "the cursor cannot be read")

// readPage returns the page a cursor names. Repeating a cursor returns the
// same page. A client can change a cursor's text, so its position counts
// only inside the capture.
func (in *Instance) readPage(q ReadQuery, rev int) (json.RawMessage, error) {
	if q.Tab != "" || q.Range != "" || q.Format != "" || q.Include != nil || q.Limit != 0 {
		return nil, invalidf("cursor", "a cursor takes no other selector")
	}
	id, row, col, skip, ok := parseCursor(q.Cursor)
	if !ok {
		return nil, errCursor
	}
	c := in.captures.find(id, in.now())
	if c == nil {
		return nil, conflict("read_expired", "")
	}
	defer c.release()
	if c.rev != rev {
		return nil, conflict("read_expired", "")
	}
	if c.empty() || row < c.first.Row || row > c.last.Row || col < c.first.Col || col > c.last.Col || (skip != 0 && !c.csv) {
		return nil, errCursor
	}
	in.probe("read_page")
	page, _, err := c.page(row, col, skip, in.probe)
	return page, err
}

func readTabs(wb *Workbook, head string) (json.RawMessage, error) {
	dst := append([]byte{'{'}, head...)
	dst = append(dst, `,"date":`...)
	dst = appendString(dst, wb.date.String())
	dst = append(dst, `,"tabs":[`...)
	for i, t := range wb.tabs {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, `{"id":`...)
		dst = strconv.AppendUint(dst, uint64(t.id), 10)
		dst = append(dst, `,"name":`...)
		dst = appendString(dst, t.name)
		dst = append(dst, `,"rows":`...)
		dst = strconv.AppendInt(dst, int64(t.rows.len()), 10)
		dst = append(dst, `,"cols":`...)
		dst = strconv.AppendInt(dst, int64(t.cols.len()), 10)
		dst = append(dst, `,"used":`...)
		if rows, cols := t.used(); rows > 0 {
			dst = appendString(dst, formula.Range{Last: formula.Pos{Row: rows - 1, Col: cols - 1}}.String())
		} else {
			dst = append(dst, "null"...)
		}
		dst = append(dst, `,"frozen_rows":`...)
		dst = strconv.AppendInt(dst, int64(t.frozenRows), 10)
		dst = append(dst, `,"frozen_cols":`...)
		dst = strconv.AppendInt(dst, int64(t.frozenCols), 10)
		dst = append(dst, '}')
	}
	return append(dst, ']', '}'), nil
}

// capture validates a query with a tab and copies its range. The caller
// holds the read lock of a live workbook.
func (wb *Workbook) capture(q ReadQuery) (*capture, error) {
	t := wb.tab(q.Tab)
	if t == nil {
		return nil, &WriteError{Code: "not_found", Reason: "tab", Name: q.Tab}
	}
	if q.Format != "" && q.Format != "json" && q.Format != "csv" {
		return nil, invalidf("format", "format is \"json\" or \"csv\"")
	}
	var include [3]bool
	for _, name := range q.Include {
		switch name {
		case "shown":
			include[0] = true
		case "formats":
			include[1] = true
		case "widths":
			include[2] = true
		default:
			return nil, invalidf("include", "include takes \"shown\", \"formats\" and \"widths\"")
		}
	}
	if q.Limit < 0 || q.Limit > maxPageCells {
		return nil, invalidf("limit", "limit is at most %d cells", maxPageCells)
	}
	r := formula.Range{}
	if q.Range == "" {
		rows, cols := t.used()
		r.Last = formula.Pos{Row: rows - 1, Col: cols - 1}
	} else {
		var err error
		if r, err = formula.ParseRange(q.Range); err != nil || r.Tab != "" {
			return nil, invalidf("range", "%q is not a range of the tab", q.Range)
		}
	}
	c, err := wb.captureRange(t, r, true)
	if err != nil {
		return nil, err
	}
	c.csv, c.include, c.limit = q.Format == "csv", include, q.Limit
	if c.csv && !c.reserveCSV() {
		c.release()
		return nil, unavailable("capture_budget")
	}
	if c.limit == 0 {
		c.limit = maxPageCells
	}
	if include[2] && !c.copyWidths(t) {
		c.release()
		return nil, unavailable("capture_budget")
	}
	return c, nil
}

func (c *capture) empty() bool { return c.last.Row < c.first.Row || c.last.Col < c.first.Col }

// rowAt returns the captured row at a position, or nil for a blank row.
func (c *capture) rowAt(pos int32, hint *int) *capRow {
	for *hint < len(c.rows) && c.rows[*hint].pos < pos {
		*hint++
	}
	if *hint < len(c.rows) && c.rows[*hint].pos == pos {
		return &c.rows[*hint]
	}
	return nil
}

func (c *capture) seek(pos int32) int {
	lo, hi := 0, len(c.rows)
	for lo < hi {
		mid := (lo + hi) / 2
		if c.rows[mid].pos < pos {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// agentText returns the text appendAgentCell writes for the formula of a
// cell at a position, or "" for a cell without one.
func agentText(cell *capCell, at formula.Pos) string {
	if cell == nil || cell.content.Kind != formula.ContentFormula && cell.content.Kind != formula.ContentUnreadable {
		return ""
	}
	return cell.f.Text(at)
}

// agentTextWork returns the most bytes of heap agentText allocates for a
// cell at any position.
func agentTextWork(cell *capCell) int64 {
	if cell == nil || cell.content.Kind != formula.ContentFormula && cell.content.Kind != formula.ContentUnreadable {
		return 0
	}
	return cell.f.TextWork()
}

// give gives back n bytes that work reserved for something it no longer
// holds. The page gives back the rest, and lets waiters know, when it is
// done.
func (w *pageWork) give(n int64) {
	captureBytes.Add(-n)
	w.held -= n
}

// agentCellMax is the most bytes appendAgentCell appends for a cell whose
// formula text is text. A byte of a text takes at most six as JSON, and
// what a cell carries beside its texts is under 64 bytes: the keys, a
// number, a date or a time, and an error's name.
func agentCellMax(cell *capCell, text string) int {
	switch {
	case cell == nil:
		return 4
	case cell.content.Kind == formula.ContentText:
		return 3 + 6*len(cell.content.Text)
	}
	return 64 + 6*(len(text)+len(cell.val.Text()))
}

// appendAgentCell appends a cell as get_sheet returns it, so that every
// value can be written back unchanged (spec rules 42 and 43). text is the
// cell's formula text (agentText).
func appendAgentCell(dst []byte, cell *capCell, text string) []byte {
	if cell == nil {
		return append(dst, "null"...)
	}
	k := cell.content
	switch k.Kind {
	case formula.ContentNumber:
		return appendNumber(dst, k.Num)
	case formula.ContentBool:
		return strconv.AppendBool(dst, k.Num != 0)
	case formula.ContentText:
		if strings.HasPrefix(k.Text, "=") || strings.HasPrefix(k.Text, "'") {
			// The apostrophe goes in front inside the string, without a
			// copy of the text that has it in front.
			mark := len(dst)
			dst = append(appendString(dst, k.Text), 0)
			copy(dst[mark+2:], dst[mark+1:len(dst)-1])
			dst[mark+1] = '\''
			return dst
		}
		return appendString(dst, k.Text)
	case formula.ContentDate:
		dst = append(dst, `{"date":"`...)
		dst = append(dst, formula.Date(math.Floor(k.Num)).String()...)
		return append(dst, '"', '}')
	case formula.ContentTime:
		dst = append(dst, `{"time":"`...)
		dst = appendClock(dst, k.Num)
		return append(dst, '"', '}')
	case formula.ContentDateTime:
		dst = append(dst, `{"datetime":"`...)
		dst = append(dst, formula.Date(math.Floor(k.Num)).String()...)
		dst = append(dst, 'T')
		dst = appendClock(dst, k.Num)
		return append(dst, '"', '}')
	case formula.ContentFormula, formula.ContentUnreadable:
		dst = append(dst, `{"formula":`...)
		dst = appendString(dst, text)
		if code := cell.val.Err(); code != formula.ErrNone {
			dst = append(dst, `,"error":`...)
			dst = appendString(dst, code.String())
		} else {
			dst = append(dst, `,"value":`...)
			dst = appendValue(dst, cell.val)
		}
		return append(dst, '}')
	}
	return append(dst, "null"...)
}

// formatSlack is the most a format takes in a page beside its own text:
// the range, the keys and the separators.
const formatSlack = 48

// formatMax is the most bytes appendFormat appends for f: its keys and
// fixed values take under 512, and its symbol at most six bytes a byte as
// JSON.
func formatMax(f formula.Format) int { return 512 + 6*len(f.Number.Symbol) }

// formatLenEntry is what a page counts for an entry of the map of the
// lengths of its formats, and formatLenBase for the map itself. They cover
// everything the map allocates as it grows, a test checks, so also what it
// holds at once while it moves to larger groups.
const (
	formatLenEntry = 96
	formatLenBase  = 1 << 10
)

// page encodes one page of the capture from the cell at (row, col). skip is
// a byte offset inside the first field of a CSV page, the last resort for a
// field that alone passes a page. A page ends at a row boundary when it
// can; a row that alone passes the page is cut at a cell. The page is
// measured with everything it carries: rows, shown text, the formats of its
// cells and the widths.
//
// Everything a JSON page is built in is charged to the capture budget
// before it is allocated, at what the allocator takes for it, and given
// back when the page is handed to the caller (pageWork). A page that meets
// a full budget is refused as unavailable and gives back what it holds;
// the capture and its cursor stay. probe is called once the page is built.
func (c *capture) page(row, col int32, skip int, probe func(string)) (json.RawMessage, bool, error) {
	c.paging.Lock()
	defer c.paging.Unlock()
	if c.csv {
		return c.csvPage(row, col, skip)
	}
	var work pageWork
	defer work.done()
	refuse := func() (json.RawMessage, bool, error) { return nil, false, unavailable("capture_budget") }
	budget := maxPageBytes - pageEnvelope
	var widths []byte
	if c.include[2] && row == c.first.Row && col == c.first.Col {
		// A run of widths takes under 48 bytes.
		var ok bool
		if widths, ok = work.grow(nil, 2+48*len(c.widths)); !ok {
			return refuse()
		}
		widths = c.appendWidths(widths)
		budget -= len(widths)
	}
	// formats bounds the bytes of the page's formats from above: one range
	// for each run of cells along a row. Joining runs down the rows only
	// makes the list shorter.
	formats := 0
	var formatLen map[int32]int
	var formatBuf []byte
	runCost := func(style int32) (int, bool) {
		n, ok := formatLen[style]
		if ok {
			return n, true
		}
		size := int64(formatLenEntry)
		if formatLen == nil {
			size += formatLenBase
		}
		f := c.formats[style]
		if !work.reserve(size) {
			return 0, false
		}
		if formatBuf, ok = work.grow(formatBuf[:0], formatMax(f)); !ok {
			return 0, false
		}
		if formatLen == nil {
			formatLen = map[int32]int{}
		}
		formatBuf, _ = appendFormat(formatBuf, f)
		n = len(formatBuf) + formatSlack
		formatLen[style] = n
		return n, true
	}
	var rows, shown, rowBuf, shownBuf, text []byte
	var ok bool
	cells := 0
	startRow, startCol := row, col
	var next formula.Pos
	more := false
	hint := c.seek(row)
	for !c.empty() && row <= c.last.Row {
		captured := c.rowAt(row, &hint)
		rowBuf, shownBuf = rowBuf[:0], shownBuf[:0]
		ci, end := 0, col
		runStyle, rowFormats := int32(0), formats
		for ; end <= c.last.Col; end++ {
			// A page that holds its cells ends before any work for the
			// next one, which belongs to the next page.
			if cells >= c.limit {
				break
			}
			var cell *capCell
			if captured != nil {
				for ci < len(captured.cells) && captured.cells[ci].col < end {
					ci++
				}
				if ci < len(captured.cells) && captured.cells[ci].col == end {
					cell = &captured.cells[ci]
				}
			}
			mark, shownMark, formatMark := len(rowBuf), len(shownBuf), formats
			at := formula.Pos{Row: row, Col: end}
			// Writing a formula's A1 text works its references out of
			// its stored text. That work and the text are charged until
			// the cell is encoded.
			textWork := agentTextWork(cell)
			if textWork > 0 && !work.reserve(textWork) {
				return refuse()
			}
			ftext := agentText(cell, at)
			if textWork > 0 {
				probe("formula_text")
			}
			if rowBuf, ok = work.grow(rowBuf, 1+agentCellMax(cell, ftext)); !ok {
				return refuse()
			}
			if end > col {
				rowBuf = append(rowBuf, ',')
			}
			rowBuf = appendAgentCell(rowBuf, cell, ftext)
			work.give(textWork)
			if c.include[0] {
				text = text[:0]
				if cell != nil {
					// No format shows a number in more than shownMax
					// bytes, and a text shows as itself.
					if text, ok = work.grow(text, max(len(cell.val.Text()), shownMax)); !ok {
						return refuse()
					}
					text = cell.appendShown(text, c.formats)
				}
				if shownBuf, ok = work.grow(shownBuf, 3+6*len(text)); !ok {
					return refuse()
				}
				if end > col {
					shownBuf = append(shownBuf, ',')
				}
				shownBuf = append(appendJSONText(append(shownBuf, '"'), text), '"')
			}
			style := int32(0)
			if cell != nil && c.include[1] {
				style = cell.style
			}
			if style != 0 && style != runStyle {
				n, ok := runCost(style)
				if !ok {
					return refuse()
				}
				formats += n
			}
			if cells > 0 && len(rows)+len(shown)+len(rowBuf)+len(shownBuf)+formats > budget {
				rowBuf, shownBuf, formats = rowBuf[:mark], shownBuf[:shownMark], formatMark
				break
			}
			runStyle = style
			cells++
		}
		whole := end > c.last.Col
		if !whole && col == c.first.Col && len(rows) > 0 {
			next, more, formats = formula.Pos{Row: row, Col: col}, true, rowFormats
			break
		}
		if rows, ok = work.grow(rows, 3+len(rowBuf)); !ok {
			return refuse()
		}
		if shown, ok = work.grow(shown, 3+len(shownBuf)); !ok {
			return refuse()
		}
		if len(rows) > 0 {
			rows = append(rows, ',')
			shown = append(shown, ',')
		}
		rows = append(append(append(rows, '['), rowBuf...), ']')
		shown = append(append(append(shown, '['), shownBuf...), ']')
		if !whole {
			next, more = formula.Pos{Row: row, Col: end}, true
			break
		}
		row, col = row+1, c.first.Col
		if cells >= c.limit && row <= c.last.Row {
			next, more = formula.Pos{Row: row, Col: col}, true
			break
		}
	}
	// The page's cells end at (endRow, endCol).
	endRow, endCol := c.last.Row, c.last.Col
	if more {
		if endRow, endCol = next.Row, next.Col-1; next.Col == c.first.Col {
			endRow, endCol = next.Row-1, c.last.Col
		}
	}
	// The head and the cursor take the state, the escaped name of the tab
	// and the capture's token beside under 512 bytes.
	size := len(rows) + len(shown) + len(widths) + formats + len(c.state) + 6*len(c.name) + len(c.id) + 512
	dst, ok := work.grow(nil, size)
	if !ok {
		return refuse()
	}
	dst = c.appendHead(dst, startRow, startCol)
	dst = append(dst, `,"rows":[`...)
	dst = append(dst, rows...)
	dst = append(dst, ']')
	if c.include[0] {
		dst = append(dst, `,"shown":[`...)
		dst = append(dst, shown...)
		dst = append(dst, ']')
	}
	if c.include[1] {
		dst = append(dst, `,"formats":`...)
		var err error
		if dst, ok, err = c.appendFormats(dst, formula.Pos{Row: startRow, Col: startCol}, formula.Pos{Row: endRow, Col: endCol}, &work); err != nil || !ok {
			if err != nil {
				return nil, false, err
			}
			return refuse()
		}
	}
	if widths != nil {
		dst = append(dst, `,"widths":`...)
		dst = append(dst, widths...)
	}
	dst = c.appendNext(dst, more, next.Row, next.Col, 0)
	probe("page_built")
	return dst, more, nil
}

func (c *capture) appendHead(dst []byte, row, col int32) []byte {
	dst = append(dst, '{')
	dst = append(dst, c.state...)
	dst = append(dst, `,"date":`...)
	dst = appendString(dst, c.date.String())
	dst = append(dst, `,"tab":`...)
	dst = appendString(dst, c.name)
	dst = append(dst, `,"range":`...)
	if c.empty() {
		dst = append(dst, "null"...)
	} else {
		dst = appendString(dst, formula.Range{First: c.first, Last: c.last}.String())
	}
	dst = append(dst, `,"start":`...)
	return appendString(dst, formula.Pos{Row: row, Col: col}.A1())
}

func (c *capture) appendNext(dst []byte, more bool, row, col int32, skip int) []byte {
	if more {
		dst = append(dst, `,"next":{"cursor":"c_`...)
		dst = append(dst, c.id...)
		for _, n := range [3]int64{int64(row), int64(col), int64(skip)} {
			dst = append(dst, '_')
			dst = strconv.AppendInt(dst, n, 10)
		}
		dst = append(dst, `","start":`...)
		dst = appendString(dst, formula.Pos{Row: row, Col: col}.A1())
		dst = append(dst, '}')
	}
	return append(dst, '}')
}

// appendFormats appends the formats of the cells of a page, from the cell
// first to the cell last in reading order, grouped into ranges that share a
// format: runs along a row, joined down the rows while the next row has the
// same run. Its lists of runs, and the room each format needs in dst, are
// charged to work before they are allocated; ok is false when the budget
// has no room.
func (c *capture) appendFormats(dst []byte, first, last formula.Pos, work *pageWork) (out []byte, ok bool, err error) {
	type run struct {
		c0, c1 int32
		style  int32
		r0, r1 int32
	}
	// A run starts at a cell with a format, so no list holds more runs
	// than the page has such cells, and none grows.
	styled := c.styledCells(first, last)
	if !work.reserve(3 * formula.HeapBytes(uintptr(styled)*unsafe.Sizeof(run{}), false)) {
		return dst, false, nil
	}
	open, done, runs := make([]run, 0, styled), make([]run, 0, styled), make([]run, 0, styled)
	flush := func(keep func(run) bool) {
		kept := open[:0]
		for _, r := range open {
			if keep(r) {
				kept = append(kept, r)
			} else {
				done = append(done, r)
			}
		}
		open = kept
	}
	hint := c.seek(first.Row)
	for row := first.Row; row <= last.Row; row++ {
		lo, hi := c.pageCols(row, first, last)
		runs = runs[:0]
		if captured := c.rowAt(row, &hint); captured != nil {
			for _, cell := range captured.cells {
				if cell.style == 0 || cell.col < lo || cell.col > hi {
					continue
				}
				if n := len(runs); n > 0 && runs[n-1].style == cell.style && runs[n-1].c1 == cell.col-1 {
					runs[n-1].c1 = cell.col
				} else {
					runs = append(runs, run{c0: cell.col, c1: cell.col, style: cell.style, r0: row, r1: row})
				}
			}
		}
		flush(func(r run) bool {
			for i, next := range runs {
				if next.c0 == r.c0 && next.c1 == r.c1 && next.style == r.style {
					runs = append(runs[:i], runs[i+1:]...)
					return true
				}
			}
			return false
		})
		for i := range open {
			open[i].r1 = row
		}
		open = append(open, runs...)
	}
	flush(func(run) bool { return false })
	dst = append(dst, '[')
	for i, r := range done {
		f := c.formats[r.style]
		if dst, ok = work.grow(dst, formatSlack+formatMax(f)); !ok {
			return dst, false, nil
		}
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, `{"range":`...)
		dst = appendString(dst, formula.Range{First: formula.Pos{Row: r.r0, Col: r.c0}, Last: formula.Pos{Row: r.r1, Col: r.c1}}.String())
		dst = append(dst, `,"format":`...)
		if dst, err = appendFormat(dst, f); err != nil {
			return nil, false, err
		}
		dst = append(dst, '}')
	}
	return append(dst, ']'), true, nil
}

// pageCols returns the first and the last column of a row of the page
// from the cell first to the cell last in reading order.
func (c *capture) pageCols(row int32, first, last formula.Pos) (lo, hi int32) {
	lo, hi = c.first.Col, c.last.Col
	if row == first.Row {
		lo = first.Col
	}
	if row == last.Row {
		hi = last.Col
	}
	return lo, hi
}

// styledCells returns the cells with a format of the page from the cell
// first to the cell last in reading order.
func (c *capture) styledCells(first, last formula.Pos) int {
	n := 0
	hint := c.seek(first.Row)
	for row := first.Row; row <= last.Row; row++ {
		lo, hi := c.pageCols(row, first, last)
		if captured := c.rowAt(row, &hint); captured != nil {
			for _, cell := range captured.cells {
				if cell.style != 0 && cell.col >= lo && cell.col <= hi {
					n++
				}
			}
		}
	}
	return n
}

func (c *capture) appendWidths(dst []byte) []byte {
	dst = append(dst, '[')
	first := true
	for i := 0; i < len(c.widths); {
		j := i + 1
		for j < len(c.widths) && c.widths[j] == c.widths[i] {
			j++
		}
		if c.widths[i] != defaultWidth {
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = append(dst, `{"cols":`...)
			dst = appendString(dst, formula.Range{FullCols: true, First: formula.Pos{Col: c.first.Col + int32(i)},
				Last: formula.Pos{Row: formula.MaxRows - 1, Col: c.first.Col + int32(j-1)}}.String())
			dst = append(dst, `,"width":`...)
			dst = strconv.AppendInt(dst, int64(c.widths[i]), 10)
			dst = append(dst, '}')
		}
		i = j
	}
	return append(dst, ']')
}

// revisionCache keeps at most two calculated revision workbooks per sheet,
// each for 60 seconds after its last use (section 10.5).
type revisionCache struct {
	mu   sync.Mutex
	list []*revisionBook
}

type revisionBook struct {
	n     int
	ready chan struct{}
	wb    *Workbook
	err   error
	used  time.Time
	// drop ends the build of a workbook that left the cache before it was
	// ready: nobody can get it any more.
	drop context.CancelFunc
}

// live reports whether a revision workbook is being built or was used
// within its lifetime.
func (rc *revisionCache) live(now time.Time) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, b := range rc.list {
		if now.Sub(b.used) <= captureTTL {
			return true
		}
		select {
		case <-b.ready:
		default:
			return true
		}
	}
	return false
}

func (rc *revisionCache) expire(now time.Time) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	// DeleteFunc clears the places it frees: an expired workbook is not
	// reachable through the list's memory.
	rc.list = slices.DeleteFunc(rc.list, func(b *revisionBook) bool {
		if now.Sub(b.used) <= captureTTL {
			return false
		}
		b.drop()
		return true
	})
}

// revision returns the read-only workbook of a revision, calculated with
// its closing date. A goroutine outside the writer builds it under the
// host-wide semaphore; a reader that does not get it in time is refused as
// calculating and can retry.
func (in *Instance) revision(ctx context.Context, n int, files []File, date string) (*Workbook, error) {
	day, ok := formula.ParseDate(date)
	if !ok {
		return nil, invalidf("date", "%q is no calculation date", date)
	}
	rc := &in.revs
	rc.mu.Lock()
	var book *revisionBook
	for _, b := range rc.list {
		if b.n == n {
			book = b
		}
	}
	if book == nil {
		pass, drop := context.WithCancel(in.ctx)
		book = &revisionBook{n: n, ready: make(chan struct{}), drop: drop}
		if len(rc.list) >= 2 {
			oldest := 0
			if rc.list[1].used.Before(rc.list[0].used) {
				oldest = 1
			}
			rc.list[oldest].drop()
			rc.list = cut(rc.list, oldest, oldest+1)
		}
		rc.list = append(rc.list, book)
		// The build outlives the caller's lease when the caller is refused
		// as calculating, so it takes its own.
		hashes := make([]string, len(files))
		for i, f := range files {
			hashes[i] = f.Hash
		}
		release := in.b.Lease(hashes)
		in.conns.Add(1)
		// The build takes turns at the gate with the passes of other
		// sheets and holds no lock: the workbook is its own until it is
		// ready. A stop of the instance ends it, and so does the cache
		// when it drops the workbook unfinished.
		go func() {
			defer in.conns.Done()
			defer close(book.ready)
			defer release()
			turn, ok := calcSlots.enter(in, pass)
			defer turn.leave()
			if !ok {
				book.err = unavailable("stopped")
				return
			}
			wb, err := loadRevision(files, in.revisionBlobs(pass, turn), turn.point)
			if err != nil {
				book.err = err
				return
			}
			wb.date = day
			budget := turn.budget()
			var out calcOut
			cerr := wb.buildDeps(turn.budget())
			if cerr == nil {
				out, cerr = turn.recalc(wb, calcReq{all: true, budget: budget})
			}
			in.noteCalc(budget, out.passes)
			in.observeCalc(false, out.took)
			if cerr != nil {
				// As at a load: a stored state meets the read limits, so
				// files that do not are damaged and are not served.
				book.err = cerr
				return
			}
			wb.apply(out, nil, 0)
			wb.settle()
			book.wb = wb
		}()
	}
	book.used = in.now()
	rc.mu.Unlock()
	timer := time.NewTimer(agentPatience)
	defer timer.Stop()
	select {
	case <-book.ready:
	case <-timer.C:
		return nil, unavailable("calculating")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if book.err != nil {
		rc.mu.Lock()
		for i, b := range rc.list {
			if b == book {
				b.drop()
				rc.list = cut(rc.list, i, i+1)
				break
			}
		}
		rc.mu.Unlock()
		return nil, &WriteError{Code: "unavailable", Reason: "revision", Retry: true, cause: book.err}
	}
	return book.wb, nil
}
