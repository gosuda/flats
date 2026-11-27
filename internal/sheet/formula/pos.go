package formula

import (
	"errors"
	"strconv"
)

// Pos is a cell position. Row and Col count from zero, so A1 is Pos{0, 0}.
type Pos struct{ Row, Col int32 }

// InGrid reports whether p lies within the grid limits of spec rule 3.
func (p Pos) InGrid() bool {
	return p.Row >= 0 && p.Row < MaxRows && p.Col >= 0 && p.Col < MaxCols
}

// A1 returns the address of p, such as "B7".
func (p Pos) A1() string { return string(p.AppendA1(nil)) }

// AppendA1 appends the address of p to dst.
func (p Pos) AppendA1(dst []byte) []byte {
	dst = AppendColumnName(dst, p.Col)
	return strconv.AppendInt(dst, int64(p.Row)+1, 10)
}

// ColumnName returns the letters of a zero-based column: A for 0, AA for 26.
func ColumnName(col int32) string { return string(AppendColumnName(nil, col)) }

// AppendColumnName appends the letters of a zero-based column to dst.
func AppendColumnName(dst []byte, col int32) []byte {
	var buf [8]byte
	i := len(buf)
	for n := int64(col) + 1; n > 0; n = (n - 1) / 26 {
		i--
		buf[i] = byte('A' + (n-1)%26)
	}
	return append(dst, buf[i:]...)
}

// Range is a rectangle of cells with both corners included, First at or
// before Last in both parts.
//
// FullCols marks whole columns, as in "B:D": the rows run from 0 to
// MaxRows-1 and stand for every row the tab has. FullRows marks whole rows,
// as in "2:5", in the same way. Tab is the tab name as written, without
// quotes, and empty when no tab is named.
type Range struct {
	Tab         string
	First, Last Pos
	FullRows    bool
	FullCols    bool
}

// Single reports whether r is one cell.
func (r Range) Single() bool {
	return r.First == r.Last && !r.FullRows && !r.FullCols
}

// Contains reports whether p lies in r. It does not look at Tab.
func (r Range) Contains(p Pos) bool {
	return p.Row >= r.First.Row && p.Row <= r.Last.Row &&
		p.Col >= r.First.Col && p.Col <= r.Last.Col
}

// Clip returns the part of r inside a tab of the given size. A whole-column
// or whole-row range gets the size of the tab. The result is false when
// nothing of r lies inside.
func (r Range) Clip(rows, cols int32) (Range, bool) {
	if r.Last.Row >= rows {
		r.Last.Row = rows - 1
	}
	if r.Last.Col >= cols {
		r.Last.Col = cols - 1
	}
	return r, r.First.Row <= r.Last.Row && r.First.Col <= r.Last.Col
}

// Cells returns the number of cells of r in a tab of the given size: the
// written area of a cell range, and the rows or columns the tab has for a
// whole-column or whole-row range.
func (r Range) Cells(rows, cols int32) int64 {
	h := int64(r.Last.Row) - int64(r.First.Row) + 1
	w := int64(r.Last.Col) - int64(r.First.Col) + 1
	if r.FullCols {
		h = int64(rows)
	}
	if r.FullRows {
		w = int64(cols)
	}
	return h * w
}

// String returns r as A1 text, such as "B2:D9", "B:D", "2:5" or
// "'Q1 costs'!B3".
func (r Range) String() string {
	var dst []byte
	if r.Tab != "" {
		dst = appendTabName(dst, r.Tab)
		dst = append(dst, '!')
	}
	switch {
	case r.FullCols:
		dst = AppendColumnName(dst, r.First.Col)
		dst = append(dst, ':')
		dst = AppendColumnName(dst, r.Last.Col)
	case r.FullRows:
		dst = strconv.AppendInt(dst, int64(r.First.Row)+1, 10)
		dst = append(dst, ':')
		dst = strconv.AppendInt(dst, int64(r.Last.Row)+1, 10)
	default:
		dst = r.First.AppendA1(dst)
		if r.First != r.Last {
			dst = append(dst, ':')
			dst = r.Last.AppendA1(dst)
		}
	}
	return string(dst)
}

// Ref is one reference of a formula, resolved at the formula's cell.
// Formula.Refs reports only references that lie within the grid limits.
type Ref struct {
	Range
}

// ErrRange is the error of ParseRange.
var ErrRange = errors.New("formula: not a cell, a range, whole columns or whole rows")

// ParseRange reads A1 text that names a cell ("B3"), a range ("B3:D9"),
// whole columns ("B:D") or whole rows ("2:5"), with an optional tab name in
// front ("Costs!B3", "'Q1 costs'!B3:D9"). Capitals and "$" signs are
// ignored, and a range written from its last cell to its first is put in
// order. A row or column outside the grid limits is an error.
func ParseRange(a1 string) (Range, error) {
	end, ref, ok := matchA1(a1, 0)
	if !ok || end != len(a1) || ref.bad {
		return Range{}, ErrRange
	}
	sl := slot{
		tab:  ref.tab,
		kind: ref.kind,
		a:    corner{row: ref.r1 - 1, col: ref.c1 - 1, rowAbs: true, colAbs: true},
		b:    corner{row: ref.r2 - 1, col: ref.c2 - 1, rowAbs: true, colAbs: true},
	}
	r, ok := sl.bounds(Pos{})
	if !ok {
		return Range{}, ErrRange
	}
	r.Tab = sl.tabName()
	return r, nil
}
