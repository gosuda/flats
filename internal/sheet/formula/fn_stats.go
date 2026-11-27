package formula

import "math"

func (ev *evaluator) fnAverage(a []*node) Value {
	var s sum
	n := 0
	err := ev.numbers(a, nil, func(x float64) {
		s.add(x)
		n++
	})
	switch {
	case err != ErrNone:
		return errValue(err)
	case n == 0:
		return errValue(ErrDiv0)
	}
	return NumberValue(s.value() / float64(n))
}

// fnCount counts numbers. In a range it passes over everything else,
// errors included. A direct boolean and direct text that reads as a number
// count.
func (ev *evaluator) fnCount(a []*node) Value {
	n := 0
	for _, arg := range a {
		if ev.late() {
			return errValue(ErrLimit) // an error of a direct value does not end the count
		}
		if arg.kind != nodeRef {
			switch v := ev.eval(arg); v.kind {
			case KindNumber, KindBool:
				n++
			case KindText:
				switch _, err := ev.textNumber(v.str); err {
				case ErrNone:
					n++
				case ErrLimit:
					return errValue(err)
				}
			}
			continue
		}
		rg, err := ev.rangeOf(arg)
		if err != ErrNone {
			return errValue(err)
		}
		done := ev.scan(&rg, func(_, _ int32, v Value) bool {
			if v.kind == KindNumber {
				n++
			}
			return true
		})
		if !done {
			return errValue(ErrLimit)
		}
	}
	return NumberValue(float64(n))
}

// fnCountA counts filled cells, whatever their value, and counts each
// direct argument as one.
func (ev *evaluator) fnCountA(a []*node) Value {
	n := 0
	g, b := ev.env.Grid, ev.env.Budget
	for _, arg := range a {
		if b.Exceeded() {
			return errValue(ErrLimit)
		}
		if arg.kind != nodeRef {
			// The argument is not calculated. It counts as a cell does.
			b.Add(1)
			n++
			continue
		}
		rg, err := ev.rangeOf(arg)
		if err != ErrNone {
			return errValue(err)
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
					if g.Filled(rg.tab, Pos{Row: r, Col: c}) {
						n++
					}
				}
			}
		}
	}
	return NumberValue(float64(n))
}

// fnCountBlank counts the cells whose value is blank or empty text, filled
// or not, the cells outside the tab included.
func (ev *evaluator) fnCountBlank(a []*node) Value {
	rg, err := ev.rangeArgOf(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	held := int64(0)
	done := ev.scan(&rg, func(_, _ int32, v Value) bool {
		if v.kind != KindBlank && !(v.kind == KindText && v.str == "") {
			held++
		}
		return true
	})
	if !done {
		return errValue(ErrLimit)
	}
	if rg.direct {
		return NumberValue(float64(1 - held))
	}
	return NumberValue(float64(rg.cells() - held))
}

func (ev *evaluator) fnMaxMin(a []*node, largest bool) Value {
	best, n := 0.0, 0
	err := ev.numbers(a, nil, func(x float64) {
		if n == 0 || (largest && x > best) || (!largest && x < best) {
			best = x
		}
		n++
	})
	if err != ErrNone {
		return errValue(err)
	}
	return NumberValue(best)
}

// orderKey maps a number to bits that order as the numbers do.
func orderKey(x float64) uint64 {
	if x == 0 {
		return 1 << 63 // both zeros
	}
	b := math.Float64bits(x)
	if b>>63 != 0 {
		return ^b
	}
	return b | 1<<63
}

func keyNumber(k uint64) float64 {
	if k>>63 != 0 {
		return math.Float64frombits(k &^ (1 << 63))
	}
	return math.Float64frombits(^k)
}

// selectRanks returns the numbers at the zero-based ranks k1 and k2 of the
// ascending order of the numbers pass delivers. It keeps no copy of them:
// it runs pass eight times, once for each byte of the order-preserving
// bits, and counts into a 256-entry table for each rank.
//
// Clearing the two tables and going through them is work of each pass,
// however few the numbers are, so each pass counts a step for each of
// their 512 counts. After the deadline no further pass starts, and what
// is returned is not the answer.
func (ev *evaluator) selectRanks(pass func(fn func(x float64)), k1, k2 int) (float64, float64) {
	var prefix [2]uint64
	var counts [2][256]int
	rank := [2]int{k1, k2}
	for shift := 56; shift >= 0 && !ev.late(); shift -= 8 {
		ev.spend(len(counts) * len(counts[0]))
		counts = [2][256]int{}
		high := ^uint64(0) << uint(shift) << 8 // the bytes already fixed
		pass(func(x float64) {
			k := orderKey(x)
			b := byte(k >> uint(shift))
			if k&high == prefix[0] {
				counts[0][b]++
			}
			if k&high == prefix[1] {
				counts[1][b]++
			}
		})
		for i := range rank {
			for b := 0; b < 256; b++ {
				if rank[i] < counts[i][b] {
					prefix[i] |= uint64(b) << uint(shift)
					break
				}
				rank[i] -= counts[i][b]
			}
		}
	}
	return keyNumber(prefix[0]), keyNumber(prefix[1])
}

// fnKth is LARGE and SMALL.
func (ev *evaluator) fnKth(a []*node, largest bool) Value {
	rg, err := ev.rangeArgOf(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	n := 0
	done := ev.scan(&rg, func(_, _ int32, v Value) bool {
		switch v.kind {
		case KindNumber:
			n++
		case KindError:
			err = v.Err()
			return false
		}
		return true
	})
	if err != ErrNone {
		return errValue(err)
	}
	k, err := ev.whole(a[1])
	switch {
	case err != ErrNone:
		return errValue(err)
	case !done:
		return errValue(ErrLimit)
	case k < 1 || k > float64(n):
		return errValue(ErrNum)
	}
	rank := int(k) - 1
	if largest {
		rank = n - int(k)
	}
	x, _ := ev.selectRanks(func(fn func(float64)) {
		ev.scan(&rg, func(_, _ int32, v Value) bool {
			if v.kind == KindNumber {
				fn(v.num)
			}
			return true
		})
	}, rank, rank)
	if ev.late() {
		return errValue(ErrLimit) // a pass stopped early
	}
	return NumberValue(x)
}

func (ev *evaluator) fnMedian(a []*node) Value {
	var stack [8]float64
	direct := stack[:0]
	n := 0
	if err := ev.numbers(a, &direct, func(float64) { n++ }); err != ErrNone {
		return errValue(err)
	}
	if n == 0 {
		return errValue(ErrNum)
	}
	lo, hi := ev.selectRanks(func(fn func(float64)) {
		ev.numbersAgain(a, direct, fn)
	}, (n-1)/2, n/2)
	if ev.late() {
		return errValue(ErrLimit) // a pass stopped early
	}
	if n%2 == 1 {
		return NumberValue(lo)
	}
	return NumberValue((lo + hi) / 2)
}

// fnRank is the equal rank of a number among the numbers of a range,
// compared at 15 significant digits.
func (ev *evaluator) fnRank(a []*node) Value {
	x, err := ev.num(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	rg, err := ev.rangeArgOf(a[1])
	if err != ErrNone {
		return errValue(err)
	}
	found, below, above := false, 0, 0
	done := ev.scan(&rg, func(_, _ int32, v Value) bool {
		switch v.kind {
		case KindNumber:
			switch c := compareNumbers(v.num, x); {
			case c < 0:
				below++
			case c > 0:
				above++
			default:
				found = true
			}
		case KindError:
			err = v.Err()
			return false
		}
		return true
	})
	switch {
	case err != ErrNone:
		return errValue(err)
	case !done:
		return errValue(ErrLimit)
	}
	ascending := false
	if len(a) > 2 {
		order, err := ev.num(a[2])
		if err != ErrNone {
			return errValue(err)
		}
		ascending = order != 0
	}
	switch {
	case !found:
		return errValue(ErrNA)
	case ascending:
		return NumberValue(float64(1 + below))
	}
	return NumberValue(float64(1 + above))
}

// fnSpread is VAR, VARP, STDEV and STDEVP. It reads its ranges twice: for
// the mean, and for the squares of the distances from it.
func (ev *evaluator) fnSpread(a []*node, sample, root bool) Value {
	var stack [8]float64
	direct := stack[:0]
	var s sum
	n := 0
	err := ev.numbers(a, &direct, func(x float64) {
		s.add(x)
		n++
	})
	switch {
	case err != ErrNone:
		return errValue(err)
	case n < 1 || (sample && n < 2):
		return errValue(ErrDiv0)
	}
	mean := s.value() / float64(n)
	var squares sum
	ev.numbersAgain(a, direct, func(x float64) {
		squares.add(float64((x - mean) * (x - mean))) // not fused into the sum
	})
	if ev.late() {
		return errValue(ErrLimit) // the second pass stopped early
	}
	div := float64(n)
	if sample {
		div--
	}
	v := squares.value() / div
	if root {
		v = math.Sqrt(v)
	}
	return NumberValue(v)
}
