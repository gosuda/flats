package formula

import "strings"

type criterionKind uint8

const (
	critNumber  criterionKind = iota // compare with a number
	critBool                         // equal to a boolean
	critText                         // match a wildcard pattern, or order against text
	critEmpty                        // "=" or "<>" alone: blank cells and empty text
	critBlank                        // criteria from a blank cell: blank, empty text and 0
	critNothing                      // a comparison sign alone matches nothing
)

// criterion is the test of a conditional total (technical design, section
// 9.4): one of = <> < <= > >= and an operand, without regard to capitals.
type criterion struct {
	kind criterionKind
	op   opCode
	num  float64
	text wildcard // the operand of critText, a pattern for = and <>
}

// set reads criteria from a value that is no error. Criteria whose text
// is a wildcard pattern that is too long for one are #VALUE!. Criteria that
// are text are #LIMIT! once the deadline has passed.
func (c *criterion) set(ev *evaluator, v Value) ErrorCode {
	c.op = opEq
	switch v.kind {
	case KindBlank:
		c.kind = critBlank
		return ErrNone
	case KindNumber:
		c.kind, c.num = critNumber, v.num
		return ErrNone
	case KindBool:
		c.kind, c.num = critBool, v.num
		return ErrNone
	}
	if ev.late() {
		return ErrLimit // criteria that are text are gone through
	}
	operand := v.str
	switch {
	case strings.HasPrefix(operand, "<>"):
		c.op, operand = opNe, operand[2:]
	case strings.HasPrefix(operand, "<="):
		c.op, operand = opLe, operand[2:]
	case strings.HasPrefix(operand, ">="):
		c.op, operand = opGe, operand[2:]
	case strings.HasPrefix(operand, "="):
		c.op, operand = opEq, operand[1:]
	case strings.HasPrefix(operand, "<"):
		c.op, operand = opLt, operand[1:]
	case strings.HasPrefix(operand, ">"):
		c.op, operand = opGt, operand[1:]
	}
	switch {
	case operand == "" && (c.op == opEq || c.op == opNe):
		c.kind = critEmpty
		return ErrNone
	case operand == "":
		c.kind = critNothing
		return ErrNone
	}
	if operand[0] != '\'' && operand[0] != '=' {
		if b, ok := isBoolText(operand); ok {
			c.kind = critBool
			if b {
				c.num = 1
			}
			return ErrNone
		}
		ev.spend(len(operand)) // reading it as a number goes through it
		if tv, ok := readValue(operand); ok {
			c.kind, c.num = critNumber, tv.num
			return ErrNone
		}
	}
	c.kind, c.text = critText, wildcard{pattern: operand}
	if c.op == opEq || c.op == opNe {
		var err ErrorCode
		if c.text, err = ev.wildcardOf(operand); err != ErrNone {
			return err
		}
	}
	return ErrNone
}

// match reports whether a cell value meets the criteria. An error never
// does.
func (c *criterion) match(ev *evaluator, v Value) bool {
	if v.kind == KindError {
		return false
	}
	switch c.kind {
	case critEmpty:
		empty := v.kind == KindBlank || (v.kind == KindText && v.str == "")
		return empty == (c.op == opEq)
	case critBlank:
		return v.kind == KindBlank || (v.kind == KindText && v.str == "") ||
			(v.kind == KindNumber && compareNumbers(v.num, 0) == 0)
	case critNothing:
		return false
	}
	if c.op == opEq || c.op == opNe {
		return c.equal(ev, v) == (c.op == opEq)
	}
	var order int
	switch {
	case c.kind == critNumber && v.kind == KindNumber:
		order = compareNumbers(v.num, c.num)
	case c.kind == critText && v.kind == KindText:
		order = ev.compareText(v.str, c.text.pattern)
	default:
		return false
	}
	switch c.op {
	case opLt:
		return order < 0
	case opLe:
		return order <= 0
	case opGt:
		return order > 0
	}
	return order >= 0
}

func (c *criterion) equal(ev *evaluator, v Value) bool {
	switch c.kind {
	case critNumber:
		switch v.kind {
		case KindNumber:
			return compareNumbers(v.num, c.num) == 0
		case KindText:
			x, err := ev.textNumber(v.str)
			return err == ErrNone && compareNumbers(x, c.num) == 0
		}
		return false
	case critBool:
		return v.kind == KindBool && v.num == c.num
	}
	return v.kind == KindText && c.text.match(ev, v.str)
}

type condKind uint8

const (
	condCount condKind = iota
	condSum
	condAverage
	condMax
	condMin
)

// condValueArg returns the range SUMIF and AVERAGEIF take their numbers
// from: the third argument, or the criteria range itself.
func condValueArg(a []*node) *node {
	if len(a) > 2 {
		return a[2]
	}
	return a[0]
}

// part returns the rows and columns of the range that lie in its tab,
// counted from its first cell. A direct value is a part of one cell.
func (rg *rangeArg) part() (rows, cols int32) {
	if rg.direct {
		return 1, 1
	}
	rows, cols = rg.lastRow()-rg.r1+1, rg.lastCol()-rg.c1+1
	if rows <= 0 || cols <= 0 {
		return 0, 0
	}
	return rows, cols
}

// condBand is a run of rows of the area of a conditional total: the rows
// before the offset end that no earlier band holds, each width cells wide.
type condBand struct {
	end, width int32
}

// condBands returns the area that the ranges cover together, as offsets
// from their first cells. The ranges have one size, and the part of each
// inside its tab starts at its first cell, so the area is a staircase: the
// rows up to the shortest part are as wide as the widest part, and so on.
// A cell outside the area is blank in every range and is never read.
//
// Finding a band looks at every range, a step each. A call may have
// hundreds of ranges and as many bands, so each band counts before it is
// found, and none is looked for after the deadline, which the second
// result reports.
func (ev *evaluator) condBands(bands []condBand, ranges []rangeArg, value *rangeArg) ([]condBand, bool) {
	for row := int32(0); ; {
		if ev.late() {
			return bands, false
		}
		ev.spend(len(ranges) + 1)
		band := condBand{end: MaxRows + 1}
		reach := func(rg *rangeArg) {
			if rows, cols := rg.part(); rows > row {
				band.end, band.width = min(band.end, rows), max(band.width, cols)
			}
		}
		for i := range ranges {
			reach(&ranges[i])
		}
		if value != nil {
			reach(value)
		}
		if band.width == 0 {
			return bands, true
		}
		bands = append(bands, band)
		row = band.end
	}
}

// fnCond is the eight conditional totals. pairs holds a range and its
// criteria in turn. value is the range of the numbers, nil for a count; it
// is written before the pairs when valueFirst is set, otherwise after them
// or not at all, and is then the first range itself.
//
// An argument that is an error gives the result, the leftmost first: a
// reference that is #REF!, a direct value that is an error where a range
// is read, such as the #REF! a deleted reference leaves, or criteria that
// are an error. Then ranges of different sizes are #VALUE!. Only cells
// whose criteria all match are read from the value range, so an error in a
// cell that does not match is passed over. A direct value that is no error
// is a range of one cell.
//
// The pairs are gone through one after another, and only a bit for each
// cell of the area is kept between them: which cells have failed a
// criteria so far. So the criteria of a call are never held together, and
// a call with many long criteria needs the memory of one.
func (ev *evaluator) fnCond(kind condKind, value *node, valueFirst bool, pairs []*node) Value {
	if ev.late() {
		return errValue(ErrLimit) // criteria are text that is gone through
	}
	n := len(pairs) / 2
	var rangeStack [4]rangeArg
	ranges := rangeStack[:0]
	badRange, badErr := -1, ErrNone
	for i := 0; i < n; i++ {
		if ev.late() {
			return errValue(ErrLimit)
		}
		rg := rangeArg{direct: true}
		if arg := pairs[2*i]; arg.kind == nodeRef {
			var err ErrorCode
			if rg, err = ev.rangeOf(arg); err != ErrNone && badRange < 0 {
				badRange, badErr = i, err
			}
		}
		ranges = append(ranges, rg)
	}
	first := &ranges[0]
	values, valueErr := first, ErrNone
	var valueRange rangeArg
	if value != nil && value != pairs[0] {
		values, valueRange.direct = &valueRange, true
		if value.kind == nodeRef {
			valueRange, valueErr = ev.rangeOf(value)
		}
	}
	// later is set while a direct value of the numbers is still to be
	// calculated, in its turn after the pairs.
	later := values == &valueRange && valueRange.direct
	if valueFirst {
		if valueErr != ErrNone {
			return errValue(valueErr)
		}
		if later {
			if valueRange.val = ev.eval(value); valueRange.val.kind == KindError {
				return valueRange.val
			}
			later = false
		}
	}
	sized := values.sameSize(first)
	for i := range ranges {
		sized = sized && ranges[i].sameSize(first)
	}
	if badRange >= 0 || valueErr != ErrNone || !sized {
		// The result is an error: the leftmost argument that is one, or
		// else the sizes. No cell is read for it.
		for i := 0; i < n; i++ {
			if i == badRange {
				return errValue(badErr)
			}
			if ranges[i].direct {
				if v := ev.eval(pairs[2*i]); v.kind == KindError {
					return v
				}
			}
			v := ev.eval(pairs[2*i+1])
			if v.kind == KindError {
				return v
			}
			var crit criterion
			if err := crit.set(ev, v); err != ErrNone {
				return errValue(err)
			}
		}
		if valueErr != ErrNone {
			return errValue(valueErr)
		}
		if later {
			if v := ev.eval(value); v.kind == KindError {
				return v
			}
		}
		return errValue(ErrValue)
	}

	var bandStack [4]condBand
	var bands []condBand
	var found bool
	if value == nil {
		bands, found = ev.condBands(bandStack[:0], ranges, nil)
	} else {
		bands, found = ev.condBands(bandStack[:0], ranges, values)
	}
	if !found {
		return errValue(ErrLimit)
	}
	inArea := int64(0)
	for i, row := 0, int32(0); i < len(bands); i++ {
		inArea += int64(bands[i].end-row) * int64(bands[i].width)
		row = bands[i].end
	}
	// failed has a bit for each cell of the area, row by row, set once a
	// pair has not matched the cell. One pair needs none.
	var failedStack [16]uint64
	var failed, taken []uint64
	if n > 1 {
		if words := int((inArea + 63) / 64); words <= len(failedStack) {
			failed = failedStack[:words]
		} else {
			taken = ev.env.takeBits(words)
			failed = taken
		}
	}
	total := condTotal{kind: kind, pairs: pairs, ranges: ranges, values: values, bands: bands, failed: failed, outside: first.cells() - inArea}
	if value != nil {
		total.value, total.later = value, later
	}
	v := total.calculate(ev)
	if taken != nil {
		ev.env.giveBits(taken)
	}
	return v
}

// condTotal is a conditional total whose arguments have one size and name
// tabs that exist.
type condTotal struct {
	kind    condKind
	pairs   []*node
	ranges  []rangeArg // the range of each pair; a direct one gets its value in its turn
	value   *node      // nil for a count
	values  *rangeArg  // the range of the numbers
	later   bool       // the numbers are a direct value that is calculated after the pairs
	bands   []condBand // the area
	failed  []uint64   // nil for one pair
	outside int64      // the cells of a range outside the area

	total sum
	count int64
	best  float64
}

func (t *condTotal) calculate(ev *evaluator) Value {
	n := len(t.ranges)
	// A cell outside liveRows and liveCols has failed as well: it lies
	// outside the tab of a range whose criteria no blank cell matches.
	liveRows, liveCols := int32(MaxRows), int32(MaxCols)
	allBlank := true // every criteria so far matches a blank cell
	for i := 0; i < n; i++ {
		rg := &t.ranges[i]
		if rg.direct {
			if rg.val = ev.eval(t.pairs[2*i]); rg.val.kind == KindError {
				return rg.val
			}
		}
		v := ev.eval(t.pairs[2*i+1])
		if v.kind == KindError {
			return v
		}
		var crit criterion
		if err := crit.set(ev, v); err != ErrNone {
			return errValue(err)
		}
		blank := crit.match(ev, Value{})
		allBlank = allBlank && blank
		last := i == n-1

		// A pair before the last can fail only the cells of its own part.
		// The last pair goes through every cell that has not failed.
		rows, cols := rg.part()
		if !blank {
			liveRows, liveCols = min(liveRows, rows), min(liveCols, cols)
		}
		if last {
			rows, cols = liveRows, liveCols
			if t.later {
				if t.values.val = ev.eval(t.value); t.values.val.kind == KindError {
					return t.values.val
				}
			}
		} else {
			rows, cols = min(rows, liveRows), min(cols, liveCols)
		}
		bit, row := 0, int32(0)
		for b := 0; b < len(t.bands) && row < rows; b++ {
			width, stride, end := min(t.bands[b].width, cols), int(t.bands[b].width), min(t.bands[b].end, rows)
			// A block of rows is about a thousand cells: the deadline is
			// checked after each, and after each text that is matched.
			for block := rowBlock(max(width, 1)); row < end; {
				if ev.env.Budget.Exceeded() {
					return errValue(ErrLimit)
				}
				stop := min(row+block, end)
				if t.failed != nil {
					// Going through the bits is work too, also where no
					// cell is read for them.
					ev.spend(int(width) * int(stop-row))
				}
				if err := t.rows(ev, &crit, rg, last, row, stop, width, bit, stride); err != ErrNone {
					return errValue(err)
				}
				bit += int(stop-row) * stride
				row = stop
			}
		}
		rg.val = Value{}
	}
	switch t.kind {
	case condCount:
		// The cells outside the area are blank in every range.
		if allBlank {
			t.count += t.outside
		}
		return NumberValue(float64(t.count))
	case condSum:
		return NumberValue(t.total.value())
	case condAverage:
		if t.count == 0 {
			return errValue(ErrDiv0)
		}
		return NumberValue(t.total.value() / float64(t.count))
	}
	return NumberValue(t.best)
}

// rows takes the cells of some rows through one pair: the first width
// cells of each row from row to stop. bit is the bit of the first cell and
// stride the bits of a row. Before the last pair it marks the cells that
// fail. With the last pair it counts the cells that have not failed, or
// reads their numbers. It returns the error of a number cell, or #LIMIT!
// when the deadline passes.
func (t *condTotal) rows(ev *evaluator, crit *criterion, rg *rangeArg, last bool, row, stop, width int32, bit, stride int) ErrorCode {
	failed := t.failed
	for ; row < stop; row, bit = row+1, bit+stride {
		for j := int32(0); j < width; j++ {
			at := bit + int(j)
			if failed != nil && failed[at>>6]&(1<<(at&63)) != 0 {
				continue
			}
			matched := crit.match(ev, ev.cell(rg, row, j))
			if ev.late() {
				return ErrLimit // the cell was not read, or its match stopped early
			}
			switch {
			case !matched && !last:
				failed[at>>6] |= 1 << (at & 63)
				continue
			case !matched || !last:
				continue
			case t.value == nil:
				t.count++
				continue
			}
			switch v := ev.cell(t.values, row, j); v.kind {
			case KindError:
				return v.Err()
			case KindNumber:
				if t.count == 0 || (t.kind == condMax && v.num > t.best) || (t.kind == condMin && v.num < t.best) {
					t.best = v.num
				}
				t.total.add(v.num)
				t.count++
			}
		}
	}
	return ErrNone
}
