package formula

func (ev *evaluator) fnChoose(a []*node) Value {
	k, err := ev.whole(a[0])
	switch {
	case err != ErrNone:
		return errValue(err)
	case k < 1 || k > float64(len(a)-1):
		return errValue(ErrNum)
	}
	return ev.eval(a[int(k)])
}

// fnRowColumn is ROW and COLUMN: the formula's own cell without an
// argument, and the first row or column of a reference.
func (ev *evaluator) fnRowColumn(a []*node, row bool) Value {
	at := Pos{Row: ev.env.At.Row, Col: ev.env.At.Col}
	if len(a) > 0 {
		if a[0].kind != nodeRef {
			// A value has no row. A value that is an error is that error.
			if v := ev.eval(a[0]); v.kind == KindError {
				return v
			}
			return errValue(ErrValue)
		}
		rg, err := ev.rangeOf(a[0])
		if err != ErrNone {
			return errValue(err)
		}
		at = Pos{Row: rg.r1, Col: rg.c1}
	}
	if row {
		return NumberValue(float64(at.Row) + 1)
	}
	return NumberValue(float64(at.Col) + 1)
}

func (ev *evaluator) fnRowsColumns(a []*node, rows bool) Value {
	if a[0].kind != nodeRef {
		// A value is one cell. A value that is an error is that error.
		if v := ev.eval(a[0]); v.kind == KindError {
			return v
		}
		return NumberValue(1)
	}
	rg, err := ev.rangeOf(a[0])
	switch {
	case err != ErrNone:
		return errValue(err)
	case rows:
		return NumberValue(float64(rg.nrows()))
	}
	return NumberValue(float64(rg.ncols()))
}

// fnIndex returns one value of a range. One index is the row on a range of
// one column and the column on a range of one row.
func (ev *evaluator) fnIndex(a []*node) Value {
	rg, err := ev.rangeArgOf(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	nrows, ncols := float64(rg.nrows()), float64(rg.ncols())
	row, err := ev.whole(a[1])
	if err != ErrNone {
		return errValue(err)
	}
	col := 1.0
	switch {
	case len(a) > 2:
		if col, err = ev.whole(a[2]); err != ErrNone {
			return errValue(err)
		}
	case ncols == 1:
	case nrows == 1:
		row, col = 1, row
	default:
		return errValue(ErrValue)
	}
	switch {
	case row == 0 || col == 0:
		return errValue(ErrValue)
	case row < 0 || col < 0 || row > nrows || col > ncols:
		return errValue(ErrNum)
	}
	return ev.cell(&rg, int32(row)-1, int32(col)-1)
}

// line is a row or a column of a range that a lookup searches.
type line struct {
	rg       *rangeArg
	vertical bool
}

func (ev *evaluator) lineValue(l line, k int32) Value {
	if l.vertical {
		return ev.cell(l.rg, k, 0)
	}
	return ev.cell(l.rg, 0, k)
}

// inTab returns how many cells of the line lie in the tab. The cells after
// them are blank and match no key.
func (l line) inTab() int32 {
	if l.rg.direct {
		return 1
	}
	if l.vertical {
		if l.rg.c1 >= l.rg.cols {
			return 0
		}
		return max(l.rg.lastRow()-l.rg.r1+1, 0)
	}
	if l.rg.r1 >= l.rg.rows {
		return 0
	}
	return max(l.rg.lastCol()-l.rg.c1+1, 0)
}

// findExact returns the first place in the line that equals the key, or
// -1. A key from a blank cell matches the number 0 and empty text. Blank
// cells and errors match no key, and a value of another kind never equals
// the key. With wild set a text key is a wildcard pattern, and one that is
// too long for a pattern is #VALUE!. Once the deadline has passed the
// search stops with #LIMIT!.
func (ev *evaluator) findExact(key Value, l line, wild bool) (int32, ErrorCode) {
	if ev.late() {
		return -1, ErrLimit // a key that is text is gone through, as a pattern or cell by cell
	}
	wild = wild && key.kind == KindText
	var pattern wildcard
	if wild {
		var err ErrorCode
		if pattern, err = ev.wildcardOf(key.str); err != ErrNone {
			return -1, err
		}
	}
	n := l.inTab()
	for k := int32(0); k < n; k++ {
		v := ev.lineValue(l, k)
		if ev.late() {
			return -1, ErrLimit
		}
		switch {
		case v.kind == KindError || v.kind == KindBlank:
		case key.kind == KindBlank:
			if (v.kind == KindText && v.str == "") || (v.kind == KindNumber && compareNumbers(v.num, 0) == 0) {
				return k, ErrNone
			}
		case v.kind != key.kind:
		case wild:
			if pattern.match(ev, v.str) {
				return k, ErrNone
			}
		case ev.compare(v, key) == 0:
			return k, ErrNone
		}
	}
	if ev.late() {
		return -1, ErrLimit // the match of the last cell may have stopped early
	}
	return -1, ErrNone
}

// lookupRank orders the kinds as a sorted column holds them: numbers, then
// text, then booleans, then errors.
func lookupRank(k ValueKind) int {
	if k == KindError {
		return 3
	}
	return kindRank(k)
}

// findSorted is the approximate match on sorted data, as a binary search.
// On ascending data it returns the last value that is not above the key,
// and with descending set, on descending data, the last value that is not
// under it. Only a value of the key's kind is a match. Blank cells count
// as the end of the data. It returns -1 when there is no match.
func (ev *evaluator) findSorted(key Value, l line, descending bool) int32 {
	if key.kind == KindBlank {
		key = Value{kind: KindNumber}
	}
	keyRank := lookupRank(key.kind)
	// before reports whether the value at k lies at or before the place
	// the key has in the order of the data.
	before := func(k int32) bool {
		v := ev.lineValue(l, k)
		if v.kind == KindBlank {
			return false
		}
		if r := lookupRank(v.kind); r != keyRank {
			return (r < keyRank) != descending
		}
		if descending {
			return ev.compare(v, key) >= 0
		}
		return ev.compare(v, key) <= 0
	}
	lo, hi := int32(0), l.inTab() // the answer is the last k with before(k)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if before(mid) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return -1
	}
	if v := ev.lineValue(l, lo-1); v.kind != key.kind {
		return -1
	}
	return lo - 1
}

// fnMatch finds a key in one row or one column. The type defaults to 1,
// the sorted approximate match; any type above 0 is 1 and any type under 0
// is -1, for descending data.
func (ev *evaluator) fnMatch(a []*node) Value {
	key := ev.eval(a[0])
	if key.kind == KindError {
		return key
	}
	rg, err := ev.rangeArgOf(a[1])
	if err != ErrNone {
		return errValue(err)
	}
	mode := 1.0
	if len(a) > 2 {
		if mode, err = ev.num(a[2]); err != ErrNone {
			return errValue(err)
		}
	}
	if rg.nrows() != 1 && rg.ncols() != 1 {
		return errValue(ErrNA)
	}
	l := line{rg: &rg, vertical: rg.ncols() == 1}
	var k int32
	if mode == 0 {
		if k, err = ev.findExact(key, l, true); err != ErrNone {
			return errValue(err)
		}
	} else {
		k = ev.findSorted(key, l, mode < 0)
	}
	if k < 0 {
		return errValue(ErrNA)
	}
	return NumberValue(float64(k) + 1)
}

// fnTableLookup is VLOOKUP, and HLOOKUP when vertical is false. Sorted
// defaults to TRUE.
func (ev *evaluator) fnTableLookup(a []*node, vertical bool) Value {
	key := ev.eval(a[0])
	if key.kind == KindError {
		return key
	}
	rg, err := ev.rangeArgOf(a[1])
	if err != ErrNone {
		return errValue(err)
	}
	index, err := ev.whole(a[2])
	if err != ErrNone {
		return errValue(err)
	}
	sorted := true
	if len(a) > 3 {
		if sorted, err = ev.truth(a[3]); err != ErrNone {
			return errValue(err)
		}
	}
	l := line{rg: &rg, vertical: vertical}
	across := rg.ncols()
	if !vertical {
		across = rg.nrows()
	}
	switch {
	case index < 1:
		return errValue(ErrValue)
	case index > float64(across):
		return errValue(ErrRef)
	}
	var k int32
	if sorted {
		k = ev.findSorted(key, l, false)
	} else if k, err = ev.findExact(key, l, true); err != ErrNone {
		return errValue(err)
	}
	switch {
	case k < 0:
		return errValue(ErrNA)
	case vertical:
		return ev.cell(&rg, k, int32(index)-1)
	}
	return ev.cell(&rg, int32(index)-1, k)
}

// fnXLookup is the exact match of a key in one row or column, with the
// value at the same place of another. It has no wildcards.
func (ev *evaluator) fnXLookup(a []*node) Value {
	key := ev.eval(a[0])
	if key.kind == KindError {
		return key
	}
	look, err := ev.rangeArgOf(a[1])
	if err != ErrNone {
		return errValue(err)
	}
	ret, err := ev.rangeArgOf(a[2])
	if err != ErrNone {
		return errValue(err)
	}
	if (look.nrows() != 1 && look.ncols() != 1) || !look.sameSize(&ret) {
		return errValue(ErrValue)
	}
	l := line{rg: &look, vertical: look.ncols() == 1}
	k, err := ev.findExact(key, l, false)
	switch {
	case err != ErrNone:
		return errValue(err)
	case k >= 0 && l.vertical:
		return ev.cell(&ret, k, 0)
	case k >= 0:
		return ev.cell(&ret, 0, k)
	case len(a) > 3:
		return ev.eval(a[3])
	}
	return errValue(ErrNA)
}
