package formula

import (
	"strconv"
	"strings"
	"unicode/utf8"
	"unsafe"
)

type slotKind uint8

const (
	slotCell  slotKind = iota // B3
	slotRange                 // B3:D9
	slotCols                  // B:D
	slotRows                  // 2:5
)

// corner is one end of a reference in the stored form. A fixed part, which
// was written with "$", holds a zero-based row or column. Any other part
// holds an offset from the formula's cell.
type corner struct {
	row, col       int32
	rowAbs, colAbs bool
}

// slot is one reference of a stored formula. A slotCell uses a only, a
// slotCols only the column parts and a slotRows only the row parts.
type slot struct {
	tab  string // as written, apostrophes included; empty for the formula's own tab
	kind slotKind
	a, b corner
}

// tabName returns the tab name without its apostrophes.
func (s *slot) tabName() string { return unquoteTabName(s.tab) }

func unquoteTabName(tab string) string {
	if tab == "" || tab[0] != '\'' {
		return tab
	}
	return strings.ReplaceAll(tab[1:len(tab)-1], "''", "'")
}

// TabNameNeedsQuotes reports whether a tab name must be written in
// apostrophes in a formula. A name needs none when it starts with a letter
// of any script or an underscore, goes on with letters, decimal digits,
// underscores and periods, and reads neither as a cell address nor as TRUE
// or FALSE. The lexer reads exactly those names without apostrophes.
func TabNameNeedsQuotes(name string) bool {
	return !bareTabName(name)
}

// appendTabName appends a tab name as a formula names it, in apostrophes
// when the name needs them.
func appendTabName(dst []byte, name string) []byte {
	if !TabNameNeedsQuotes(name) {
		return append(dst, name...)
	}
	dst = append(dst, '\'')
	for i := 0; i < len(name); i++ {
		if name[i] == '\'' {
			dst = append(dst, '\'')
		}
		dst = append(dst, name[i])
	}
	return append(dst, '\'')
}

func quoteTabName(name string) string {
	if !TabNameNeedsQuotes(name) {
		return name
	}
	return string(appendTabName(nil, name))
}

// resolved is a reference resolved at a cell: zero-based rows and columns
// in the order they were written.
type resolved struct {
	r1, c1, r2, c2 int32
}

// resolve returns the rows and columns the slot names for a formula in the
// cell at. ok is false when a part lies outside what the lexer keeps.
func (s *slot) resolve(at Pos) (r resolved, ok bool) {
	r.r1, r.c1 = s.a.row, s.a.col
	if !s.a.rowAbs {
		r.r1 += at.Row
	}
	if !s.a.colAbs {
		r.c1 += at.Col
	}
	if s.kind == slotCell {
		r.r2, r.c2 = r.r1, r.c1
		return r, r.r1 >= 0 && r.r1 < rowCap && r.c1 >= 0 && r.c1 < colCap
	}
	r.r2, r.c2 = s.b.row, s.b.col
	if !s.b.rowAbs {
		r.r2 += at.Row
	}
	if !s.b.colAbs {
		r.c2 += at.Col
	}
	switch s.kind {
	case slotCols:
		r.r1, r.r2 = 0, 0
	case slotRows:
		r.c1, r.c2 = 0, 0
	}
	return r, r.r1 >= 0 && r.r1 < rowCap && r.r2 >= 0 && r.r2 < rowCap &&
		r.c1 >= 0 && r.c1 < colCap && r.c2 >= 0 && r.c2 < colCap
}

// bounds returns the rectangle of a resolved reference with its corners in
// order. A whole-column reference spans every row of the grid and a
// whole-row reference every column. ok is false when the reference leaves
// the grid limits.
func (s *slot) bounds(at Pos) (r Range, ok bool) {
	res, ok := s.resolve(at)
	if !ok {
		return Range{}, false
	}
	r.First = Pos{Row: min(res.r1, res.r2), Col: min(res.c1, res.c2)}
	r.Last = Pos{Row: max(res.r1, res.r2), Col: max(res.c1, res.c2)}
	switch s.kind {
	case slotCols:
		r.FullCols = true
		r.Last.Row = MaxRows - 1
	case slotRows:
		r.FullRows = true
		r.Last.Col = MaxCols - 1
	}
	return r, r.Last.Row < MaxRows && r.Last.Col < MaxCols
}

// appendStoredPart appends "R3" or "R[-2]" for a row part, and the same
// with "C" for a column part.
func appendStoredPart(dst []byte, letter byte, v int32, abs bool) []byte {
	dst = append(dst, letter)
	if abs {
		return strconv.AppendInt(dst, int64(v)+1, 10)
	}
	dst = append(dst, '[')
	dst = strconv.AppendInt(dst, int64(v), 10)
	return append(dst, ']')
}

func (s *slot) appendStoredCorner(dst []byte, c *corner) []byte {
	if s.kind != slotCols {
		dst = appendStoredPart(dst, 'R', c.row, c.rowAbs)
	}
	if s.kind != slotRows {
		dst = appendStoredPart(dst, 'C', c.col, c.colAbs)
	}
	return dst
}

// appendStored appends the slot in braces, such as {R2C[-3]:R[-1]C[-3]}.
func (s *slot) appendStored(dst []byte) []byte {
	dst = append(dst, '{')
	if s.tab != "" {
		dst = append(dst, s.tab...)
		dst = append(dst, '!')
	}
	dst = s.appendStoredCorner(dst, &s.a)
	if s.kind != slotCell {
		dst = append(dst, ':')
		dst = s.appendStoredCorner(dst, &s.b)
	}
	return append(dst, '}')
}

// appendA1 appends the slot as A1 text for a formula in the cell at, or
// #REF! when a part lies outside what the lexer keeps.
func (s *slot) appendA1(dst []byte, at Pos) []byte {
	r, ok := s.resolve(at)
	if !ok {
		return append(dst, "#REF!"...)
	}
	if s.tab != "" {
		dst = append(dst, s.tab...)
		dst = append(dst, '!')
	}
	dst = s.appendA1Corner(dst, r.r1, r.c1, &s.a)
	if s.kind != slotCell {
		dst = append(dst, ':')
		dst = s.appendA1Corner(dst, r.r2, r.c2, &s.b)
	}
	return dst
}

func (s *slot) appendA1Corner(dst []byte, row, col int32, c *corner) []byte {
	if s.kind != slotRows {
		if c.colAbs {
			dst = append(dst, '$')
		}
		dst = AppendColumnName(dst, col)
	}
	if s.kind != slotCols {
		if c.rowAbs {
			dst = append(dst, '$')
		}
		dst = strconv.AppendInt(dst, int64(row)+1, 10)
	}
	return dst
}

func columnNameLen(col int32) int {
	switch {
	case col < 26:
		return 1
	case col < 26+26*26:
		return 2
	}
	return 3
}

func decimalLen(n int32) int {
	l := 1
	for n >= 10 {
		n /= 10
		l++
	}
	return l
}

// a1Len returns the code points of what appendA1 writes.
func (s *slot) a1Len(at Pos, tabRunes int) int {
	r, ok := s.resolve(at)
	if !ok {
		return len("#REF!")
	}
	n := 0
	if s.tab != "" {
		n = tabRunes + 1
	}
	n += s.a1CornerLen(r.r1, r.c1, &s.a)
	if s.kind != slotCell {
		n += 1 + s.a1CornerLen(r.r2, r.c2, &s.b)
	}
	return n
}

func (s *slot) a1CornerLen(row, col int32, c *corner) int {
	n := 0
	if s.kind != slotRows {
		n += columnNameLen(col)
		if c.colAbs {
			n++
		}
	}
	if s.kind != slotCols {
		n += decimalLen(row + 1)
		if c.rowAbs {
			n++
		}
	}
	return n
}

// parseStoredPart reads "R3" or "R[-2]" at i in canonical form: no plus
// sign, no leading zero and no "-0".
func parseStoredPart(s string, i int, letter byte, limit int32) (end int, v int32, abs, ok bool) {
	if i >= len(s) || s[i] != letter {
		return 0, 0, false, false
	}
	i++
	rel := i < len(s) && s[i] == '['
	if rel {
		i++
	}
	neg := rel && i < len(s) && s[i] == '-'
	if neg {
		i++
	}
	j := i
	var n int64
	for j < len(s) && isDigit(s[j]) && j-i < 10 {
		n = n*10 + int64(s[j]-'0')
		j++
	}
	if j == i || (s[i] == '0' && (j-i > 1 || neg || !rel)) {
		return 0, 0, false, false
	}
	if rel {
		if j >= len(s) || s[j] != ']' || n >= int64(limit) {
			return 0, 0, false, false
		}
		if neg {
			n = -n
		}
		return j + 1, int32(n), false, true
	}
	if n > int64(limit) {
		return 0, 0, false, false
	}
	return j, int32(n - 1), true, true
}

// parseSlot reads the slot that starts with "{" at i.
func parseSlot(s string, i int) (end int, sl slot, ok bool) {
	i++
	if tab, body, hasTab := scanTabName(s, i); hasTab {
		sl.tab = tab
		i = body
	}
	var hasRow, hasCol bool
	if i, hasRow, hasCol, ok = parseStoredCorner(s, i, &sl.a); !ok {
		return 0, slot{}, false
	}
	if i < len(s) && s[i] == '}' {
		return i + 1, sl, hasRow && hasCol
	}
	if i >= len(s) || s[i] != ':' {
		return 0, slot{}, false
	}
	var hasRow2, hasCol2 bool
	if i, hasRow2, hasCol2, ok = parseStoredCorner(s, i+1, &sl.b); !ok {
		return 0, slot{}, false
	}
	if hasRow != hasRow2 || hasCol != hasCol2 || i >= len(s) || s[i] != '}' {
		return 0, slot{}, false
	}
	switch {
	case hasRow && hasCol:
		sl.kind = slotRange
	case hasCol:
		sl.kind = slotCols
	default:
		sl.kind = slotRows
	}
	return i + 1, sl, true
}

func parseStoredCorner(s string, i int, c *corner) (end int, hasRow, hasCol, ok bool) {
	if e, v, abs, ok := parseStoredPart(s, i, 'R', rowCap); ok {
		c.row, c.rowAbs, hasRow, i = v, abs, true, e
	}
	if e, v, abs, ok := parseStoredPart(s, i, 'C', colCap); ok {
		c.col, c.colAbs, hasCol, i = v, abs, true, e
	}
	return i, hasRow, hasCol, hasRow || hasCol
}

// Text returns the formula as A1 text for a formula in the cell at. A
// reference that lies outside the representable grid at that cell is
// written as #REF!. The text is written into one allocation of its exact
// length (TextWork).
func (f *Formula) Text(at Pos) string {
	lits, slots, _ := f.pieces()
	if len(slots) == 0 {
		return f.stored
	}
	n := len(lits[len(slots)])
	for i := range slots {
		// Everything of a reference but its tab name is ASCII.
		n += len(lits[i]) + slots[i].a1Len(at, len(slots[i].tab))
	}
	dst := make([]byte, 0, n)
	for i := range slots {
		dst = append(dst, lits[i]...)
		dst = slots[i].appendA1(dst, at)
	}
	dst = append(dst, lits[len(slots)]...)
	// Nothing writes dst again, so the text can be its bytes.
	return unsafe.String(unsafe.SliceData(dst), len(dst))
}

// TextLen returns the code points of Text(at) without building the text.
func (f *Formula) TextLen(at Pos) int {
	_, slots, n := f.pieces()
	for i := range slots {
		s := &slots[i]
		tabRunes := 0
		if s.tab != "" {
			tabRunes = utf8.RuneCountInString(s.tab)
		}
		n += s.a1Len(at, tabRunes)
	}
	return n
}

// NamesTab reports whether some reference is written with a tab name.
func (f *Formula) NamesTab() bool {
	for i := range f.slots {
		if f.slots[i].tab != "" {
			return true
		}
	}
	return false
}

// Tabs calls visit with the tab name of each reference that is written
// with one, in the order of the text. Unlike Refs it also reports the
// references that lie outside the grid limits, because a rename rewrites
// those too.
//
// Each name is a text of its own. A part of the formula's text would keep
// all of that text alive in a holder that keeps the name after the
// formula is gone, as an undo entry keeps the tabs its formulas name.
func (f *Formula) Tabs(visit func(name string)) {
	for i := range f.slots {
		if f.slots[i].tab != "" {
			visit(strings.Clone(f.slots[i].tabName()))
		}
	}
}

// Refs calls visit for each reference of the formula, resolved for a
// formula in the cell at, in the order of the text. It reports every
// argument, taken or not, and passes over references outside the grid
// limits. Ref.Tab is empty for a reference without a tab name.
//
// A formula whose value is #ERROR! because it cannot be read, or because a
// call has a number of arguments its function does not take, reads nothing
// and has no references here.
func (f *Formula) Refs(at Pos, visit func(Ref)) {
	if f.root == nil {
		return
	}
	for i := range f.slots {
		s := &f.slots[i]
		r, ok := s.bounds(at)
		if !ok {
			continue
		}
		r.Tab = s.tabName()
		visit(Ref{Range: r})
	}
}

// Dims gives the size of a tab a formula reads. The name is empty for the
// formula's own tab. ok is false when no tab has the name.
type Dims func(tab string) (rows, cols int32, ok bool)

// ReadCells returns the static read count of the formula in the cell at
// (technical design, section 11): the area of every reference Refs
// reports. A cell range counts its written area, whatever the size of the
// tab. A whole-column reference counts the rows its tab has for each of
// its columns, a whole-row reference the columns its tab has for each of
// its rows, and both count nothing while no tab has their name.
func (f *Formula) ReadCells(at Pos, dims Dims) int64 {
	if f.root == nil {
		return 0
	}
	var total int64
	for i := range f.slots {
		s := &f.slots[i]
		r, ok := s.bounds(at)
		if !ok {
			continue
		}
		if !r.FullCols && !r.FullRows {
			total += r.Cells(0, 0)
			continue
		}
		if rows, cols, ok := dims(s.tabName()); ok {
			total += r.Cells(rows, cols)
		}
	}
	return total
}
