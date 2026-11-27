package sheet

import (
	"encoding/json"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// responseWriteWait bounds each write of a streamed response. Tests
// shorten it.
var responseWriteWait = 15 * time.Second

var csvBuffers = sync.Pool{New: func() any { b := make([]byte, 0, 64<<10); return &b }}

// csvBOM starts a CSV file, so that a program opening it directly reads it
// as UTF-8 (spec rule 60). get_sheet returns the text without it.
const csvBOM = "\xEF\xBB\xBF"

// csvField appends the CSV field of the cell at col of a captured row,
// which is nil for a blank row, followed by its comma, or by CR LF after
// the last column of the capture. ci is the caller's place in the row's
// cells; it only moves forward.
func (c *capture) csvField(dst []byte, row *capRow, ci *int, col int32) []byte {
	if row != nil {
		for *ci < len(row.cells) && row.cells[*ci].col < col {
			*ci++
		}
		if *ci < len(row.cells) && row.cells[*ci].col == col {
			cell := &row.cells[*ci]
			if cell.val.Kind() == formula.KindText {
				// A text shows as it is: the field is built from the
				// cell's own text, with no copy between.
				dst = formula.AppendCSVField(dst, cell.val.Text())
			} else {
				var shown [shownMax]byte
				dst = formula.AppendCSVField(dst, string(cell.appendShown(shown[:0], c.formats)))
			}
		}
	}
	if col < c.last.Col {
		return append(dst, ',')
	}
	return append(dst, '\r', '\n')
}

// csvFieldMax is the most bytes csvField appends for one cell: every
// character doubled, two quotation marks, the apostrophe and the
// separator.
func csvFieldMax(cell *capCell) int {
	return 2*max(len(cell.val.Text()), shownMax) + 8
}

// reserveCSV charges the capture for the one working buffer its CSV text
// is built in: the largest field of the capture. It reports false, and
// charges nothing, when the budget has no room. The caller is the only
// holder of the capture.
func (c *capture) reserveCSV() bool {
	size := 2*shownMax + 8
	for i := range c.rows {
		for k := range c.rows[i].cells {
			size = max(size, csvFieldMax(&c.rows[i].cells[k]))
		}
	}
	if !c.reserve(formula.HeapBytes(uintptr(size), false)) {
		return false
	}
	c.fieldMax = size
	return true
}

// fieldBuf returns the capture's working buffer for one CSV field. One
// request at a time uses it: the request that owns the capture, or the one
// that holds its page lock.
func (c *capture) fieldBuf() []byte {
	if c.field == nil {
		c.field = make([]byte, 0, c.fieldMax)
	}
	return c.field[:0]
}

// writeCSV streams the capture as CSV, field by field through a pooled
// buffer of fixed size and the capture's field buffer. It never holds a
// row or the file in memory (spec rule 9): a field that does not fit the
// pooled buffer is written from its own.
func (c *capture) writeCSV(w io.Writer) error {
	bp := csvBuffers.Get().(*[]byte)
	defer csvBuffers.Put(bp)
	buf := (*bp)[:0]
	field := c.fieldBuf()
	defer func() {
		if cap(field) > c.fieldMax {
			encoderGrowths.Add(1)
		}
	}()
	hint := 0
	for row := c.first.Row; !c.empty() && row <= c.last.Row; row++ {
		captured, ci := c.rowAt(row, &hint), 0
		for col := c.first.Col; col <= c.last.Col; col++ {
			field = c.csvField(field[:0], captured, &ci, col)
			if len(buf)+len(field) > cap(buf) {
				if _, err := w.Write(buf); err != nil {
					return err
				}
				buf = buf[:0]
			}
			if len(field) > cap(buf) {
				if _, err := w.Write(field); err != nil {
					return err
				}
				continue
			}
			buf = append(buf, field...)
		}
	}
	_, err := w.Write(buf)
	return err
}

// jsonLen returns the bytes of b as the content of a JSON string.
func jsonLen(b []byte) int {
	n := len(b)
	for _, ch := range b {
		switch {
		case ch == '"' || ch == '\\' || ch == '\n' || ch == '\r' || ch == '\t':
			n++
		case ch < 0x20:
			n += 5
		}
	}
	return n
}

// appendJSONText appends b as the content of a JSON string, without the
// quotation marks. It writes jsonLen(b) bytes.
func appendJSONText(dst, b []byte) []byte {
	const hexDigits = "0123456789abcdef"
	start := 0
	for i, ch := range b {
		if ch >= 0x20 && ch != '"' && ch != '\\' {
			continue
		}
		dst = append(dst, b[start:i]...)
		switch ch {
		case '"', '\\':
			dst = append(dst, '\\', ch)
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[ch>>4], hexDigits[ch&0xf])
		}
		start = i + 1
	}
	return append(dst, b[start:]...)
}

// pageWork is the memory a request holds while it builds a page: the page
// itself, charged to the host-wide capture budget before each growth and
// given back when the page is handed to the caller.
type pageWork struct{ held int64 }

func (w *pageWork) reserve(n int64) bool {
	if captureBytes.Add(n) > captureBudget {
		captureBytes.Add(-n)
		return false
	}
	w.held += n
	return true
}

func (w *pageWork) done() {
	if w.held != 0 {
		captureBytes.Add(-w.held)
		w.held = 0
		signalCaptureRoom()
	}
}

// grow returns b with room for n more bytes. New memory is charged before
// it is allocated, at what the allocator takes for it; ok is false when the
// budget has no room for it. b is nil or a result of grow.
func (w *pageWork) grow(b []byte, n int) (out []byte, ok bool) {
	if cap(b)-len(b) >= n {
		return b, true
	}
	size := max(2*cap(b), len(b)+n, 4<<10)
	if !w.reserve(formula.HeapBytes(uintptr(size), false)) {
		return b, false
	}
	out = append(make([]byte, 0, size), b...)
	old := formula.HeapBytes(uintptr(cap(b)), false)
	captureBytes.Add(-old)
	w.held -= old
	return out, true
}

// csvPage returns an exact piece of the CSV text of the capture. It ends
// after a row's CR LF while the next row still fits. A row that alone
// passes a page ends after the comma that follows a field. Joining the csv
// strings of all pages gives the whole text (section 13).
//
// The page is built field by field, straight into its result: each field
// in the capture's field buffer, then as JSON text in the page, which
// grows under the page limit and is charged as it grows. A row that turns
// out not to fit after the rows before it is taken back out of the page.
// The caller holds the capture's page lock.
func (c *capture) csvPage(row, col int32, skip int) (json.RawMessage, bool, error) {
	budget := maxPageBytes - pageEnvelope
	var work pageWork
	defer work.done()
	refuse := func() (json.RawMessage, bool, error) { return nil, false, unavailable("capture_budget") }
	// The head and the tail of the page are short: the state, the date,
	// the tab's name, two ranges and a cursor.
	dst, ok := work.grow(nil, 8<<10)
	if !ok {
		return refuse()
	}
	dst = c.appendHead(dst, row, col)
	dst = append(dst, `,"csv":"`...)
	field := c.fieldBuf()
	defer func() {
		if cap(field) > c.fieldMax {
			encoderGrowths.Add(1)
		}
	}()
	used, cells := 0, 0
	var next formula.Pos
	nextSkip, more := 0, false
	hint := c.seek(row)
rows:
	for first := true; !c.empty() && row <= c.last.Row; first = false {
		// A row after the first is in the page whole or not at all.
		if !first && cells+int(c.last.Col-col)+1 > c.limit {
			more, next = true, formula.Pos{Row: row, Col: col}
			break
		}
		captured, ci := c.rowAt(row, &hint), 0
		mark, usedMark, cellsMark := len(dst), used, cells
		for at := col; at <= c.last.Col; at++ {
			if cells+1 > c.limit {
				more, next = true, formula.Pos{Row: row, Col: at}
				break rows
			}
			field = c.csvField(field[:0], captured, &ci, at)
			piece := field
			if skip != 0 {
				// An offset this side issued lies at a code point inside
				// the first field of the page, before the end of its
				// separator.
				if skip >= len(field) || !utf8.RuneStart(field[skip]) {
					return nil, false, errCursor
				}
				piece = field[skip:]
			}
			size := jsonLen(piece)
			if used+size > budget {
				more = true
				switch {
				case !first:
					dst, used, cells = dst[:mark], usedMark, cellsMark
					next = formula.Pos{Row: row, Col: col}
					break rows
				case at > col:
					next = formula.Pos{Row: row, Col: at}
					break rows
				}
				// Not even one field fits: cut inside it, at a code point.
				cut := skip
				for cut < len(field) {
					_, n := utf8.DecodeRune(field[cut:])
					if used+jsonLen(field[cut:cut+n]) > budget && cut > skip {
						break
					}
					used += jsonLen(field[cut : cut+n])
					cut += n
				}
				piece = field[skip:cut]
				if dst, ok = work.grow(dst, jsonLen(piece)); !ok {
					return refuse()
				}
				dst = appendJSONText(dst, piece)
				next, nextSkip = formula.Pos{Row: row, Col: at}, cut
				if cut == len(field) {
					next.Col, nextSkip = at+1, 0
				}
				break rows
			}
			if dst, ok = work.grow(dst, size); !ok {
				return refuse()
			}
			dst = appendJSONText(dst, piece)
			used, cells, skip = used+size, cells+1, 0
		}
		row, col = row+1, c.first.Col
	}
	if more && next.Col > c.last.Col {
		next = formula.Pos{Row: next.Row + 1, Col: c.first.Col}
		more = next.Row <= c.last.Row
	}
	if dst, ok = work.grow(dst, 1<<10); !ok {
		return refuse()
	}
	dst = append(dst, '"')
	return c.appendNext(dst, more, next.Row, next.Col, nextSkip), more, nil
}
