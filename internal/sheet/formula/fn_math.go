package formula

import "math"

// maxWhole bounds the whole numbers the date and rounding functions take
// as arguments, so that their integer arithmetic cannot overflow.
const maxWhole = 1e12

func sign(x float64) float64 {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	}
	return 0
}

func (ev *evaluator) math1(a []*node, fn func(float64) float64) Value {
	x, err := ev.num(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	return NumberValue(fn(x))
}

// num2 reads two numbers, the left one first.
func (ev *evaluator) num2(a []*node) (x, y float64, err ErrorCode) {
	if x, err = ev.num(a[0]); err != ErrNone {
		return 0, 0, err
	}
	y, err = ev.num(a[1])
	return x, y, err
}

// floorDiv returns the largest whole number that is not above x/s, decided
// on the exact quotient and not on the rounded one.
func floorDiv(x, s float64) float64 {
	q := math.Floor(x / s)
	r := math.FMA(-q, s, x) // the sign of x - q*s is exact
	switch {
	case s > 0 && r < 0, s < 0 && r > 0:
		q--
	case s > 0 && r >= s, s < 0 && r <= s:
		q++
	}
	return q
}

// fnCeilFloor is CEILING and FLOOR. With equal signs CEILING moves away
// from zero and FLOOR toward it; a negative number with a positive
// significance moves up for CEILING and down for FLOOR.
func (ev *evaluator) fnCeilFloor(a []*node, up bool) Value {
	x, err := ev.num(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	s := 1.0
	if len(a) > 1 {
		if s, err = ev.num(a[1]); err != ErrNone {
			return errValue(err)
		}
	}
	// A significance of 0 decides before the number does: FLOOR divides by
	// it also when the number is 0.
	switch {
	case s == 0 && up:
		return NumberValue(0)
	case s == 0:
		return errValue(ErrDiv0)
	case x == 0:
		return NumberValue(0)
	case x > 0 && s < 0:
		return errValue(ErrNum)
	}
	if q := math.Abs(x / s); q >= 1<<53 {
		return NumberValue(x) // every double this far from the significance is a multiple of it
	}
	if up {
		return NumberValue(-floorDiv(-x, s) * s)
	}
	return NumberValue(floorDiv(x, s) * s)
}

func (ev *evaluator) fnLog(a []*node, base float64, baseArg bool) Value {
	x, err := ev.num(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	if baseArg && len(a) > 1 {
		if base, err = ev.num(a[1]); err != ErrNone {
			return errValue(err)
		}
	}
	// The number is checked before the base: LOG(0,1) is #NUM!.
	switch {
	case x <= 0 || base <= 0:
		return errValue(ErrNum)
	case base == 1:
		return errValue(ErrDiv0)
	case base == math.E:
		return NumberValue(math.Log(x))
	}
	return NumberValue(math.Log(x) / math.Log(base))
}

func (ev *evaluator) fnMod(a []*node) Value {
	x, y, err := ev.num2(a)
	switch {
	case err != ErrNone:
		return errValue(err)
	case y == 0:
		return errValue(ErrDiv0)
	}
	return NumberValue(x - float64(y*math.Floor(x/y))) // the conversion keeps the product from being fused into the subtraction
}

func (ev *evaluator) fnPower(a []*node) Value {
	x, y, err := ev.num2(a)
	if err != ErrNone {
		return errValue(err)
	}
	return power(x, y)
}

func (ev *evaluator) fnQuotient(a []*node) Value {
	x, y, err := ev.num2(a)
	switch {
	case err != ErrNone:
		return errValue(err)
	case y == 0:
		return errValue(ErrDiv0)
	}
	return NumberValue(math.Trunc(x / y))
}

func (ev *evaluator) fnRound(a []*node, mode roundMode) Value {
	x, err := ev.num(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	digits := 0.0
	if len(a) > 1 {
		if digits, err = ev.whole(a[1]); err != ErrNone {
			return errValue(err)
		}
	}
	// A double has no digit past these places, so larger counts round the same.
	digits = math.Max(-400, math.Min(400, digits))
	return NumberValue(roundNumber(x, int(digits), mode))
}

func (ev *evaluator) fnSqrt(a []*node) Value {
	x, err := ev.num(a[0])
	switch {
	case err != ErrNone:
		return errValue(err)
	case x < 0:
		return errValue(ErrNum)
	}
	return NumberValue(math.Sqrt(x))
}

// fnSum has its own loop over a range: it is the function a sheet calls
// most, on the longest ranges.
func (ev *evaluator) fnSum(a []*node) Value {
	var s sum
	g, b := ev.env.Grid, ev.env.Budget
	for _, arg := range a {
		if arg.kind != nodeRef {
			x, err := ev.num(arg)
			if err != ErrNone {
				return errValue(err)
			}
			s.add(x)
			continue
		}
		rg, err := ev.rangeOf(arg)
		if err != ErrNone {
			return errValue(err)
		}
		if b.Exceeded() {
			return errValue(ErrLimit)
		}
		lastRow, lastCol := rg.lastRow(), rg.lastCol()
		width := lastCol - rg.c1 + 1
		if width <= 0 {
			continue // the range lies beside the tab
		}
		block := rowBlock(width)
		for first := rg.r1; first <= lastRow; first += block {
			last := min(first+block-1, lastRow)
			if !b.Add(int64(last-first+1) * int64(width)) {
				return errValue(ErrLimit)
			}
			for r := first; r <= last; r++ {
				for c := rg.c1; c <= lastCol; c++ {
					switch v := g.Value(rg.tab, Pos{Row: r, Col: c}); v.kind {
					case KindNumber:
						s.add(v.num)
					case KindError:
						return v
					}
				}
			}
		}
	}
	return NumberValue(s.value())
}

func (ev *evaluator) fnProduct(a []*node) Value {
	p, n := 1.0, 0
	err := ev.numbers(a, nil, func(x float64) {
		p *= x
		n++
	})
	switch {
	case err != ErrNone:
		return errValue(err)
	case n == 0:
		return NumberValue(0)
	}
	return NumberValue(p)
}

// fnSumProduct multiplies ranges of one size cell by cell and adds the
// products. Text, booleans and blank cells count as 0, and a direct value
// is a range of one cell. It reads each range twice: once for errors, once
// for the products.
func (ev *evaluator) fnSumProduct(a []*node) Value {
	var stack [4]rangeArg
	ranges := stack[:0]
	for _, n := range a {
		rg, err := ev.rangeArgOf(n)
		if err != ErrNone {
			return errValue(err)
		}
		done := ev.scan(&rg, func(_, _ int32, v Value) bool {
			err = v.Err()
			return err == ErrNone
		})
		if err != ErrNone {
			return errValue(err)
		}
		if !done {
			return errValue(ErrLimit)
		}
		if rg.direct && rg.val.kind != KindNumber {
			// Text counts as 0, so it is not kept while the other
			// arguments are calculated.
			rg.val = Value{}
		}
		ranges = append(ranges, rg)
	}
	// A cell outside its tab is blank and makes its product 0, so only the
	// cells that lie in every tab count.
	rows, cols := ranges[0].nrows(), ranges[0].ncols()
	for i := range ranges {
		rg := &ranges[i]
		if !rg.sameSize(&ranges[0]) {
			return errValue(ErrValue)
		}
		if !rg.direct {
			rows, cols = min(rows, rg.lastRow()-rg.r1+1), min(cols, rg.lastCol()-rg.c1+1)
		}
	}
	// One product may read a thousand ranges and one row hundreds of
	// thousands of cells, so the loop ends at the first cell that the
	// deadline keeps from being read.
	var total sum
	for i := int32(0); i < rows; i++ {
		for j := int32(0); j < cols; j++ {
			p := 1.0
			for k := range ranges {
				v := ev.cell(&ranges[k], i, j)
				if ev.late() {
					return errValue(ErrLimit)
				}
				if v.kind != KindNumber {
					p = 0
					break
				}
				p = float64(p * v.num) // not fused into the sum, so every host gives the same total
			}
			total.add(p)
		}
	}
	return NumberValue(total.value())
}
