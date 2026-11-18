package formula

import (
	"math"
	"strings"
)

// dayNumber reads a date argument as a whole day. A negative number is
// #NUM!.
func (ev *evaluator) dayNumber(n *node) (int64, ErrorCode) {
	x, err := ev.num(n)
	switch {
	case err != ErrNone:
		return 0, err
	case x < 0 || x > maxWhole:
		return 0, ErrNum
	}
	return int64(math.Floor(x)), ErrNone
}

// wholeArg reads an argument as a whole number the calendar can count
// with.
func (ev *evaluator) wholeArg(n *node) (int64, ErrorCode) {
	x, err := ev.whole(n)
	switch {
	case err != ErrNone:
		return 0, err
	case math.Abs(x) > maxWhole:
		return 0, ErrNum
	}
	return int64(x), ErrNone
}

// floorDivMod divides rounding down, so that the remainder is not
// negative for a positive divisor.
func floorDivMod(a, b int64) (q, r int64) {
	q, r = a/b, a%b
	if r < 0 {
		q, r = q-1, r+b
	}
	return q, r
}

// fnDate adds 1900 to a year under 1900, and lets months and days run
// over into the next years and months.
func (ev *evaluator) fnDate(a []*node) Value {
	y, err := ev.wholeArg(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	m, err := ev.wholeArg(a[1])
	if err != ErrNone {
		return errValue(err)
	}
	d, err := ev.wholeArg(a[2])
	switch {
	case err != ErrNone:
		return errValue(err)
	case y < 0:
		return errValue(ErrNum)
	case y < 1900:
		y += 1900
	}
	years, month := floorDivMod(m-1, 12)
	serial := serialOf(y+years, month+1, 1) + d - 1
	if serial < 0 {
		return errValue(ErrNum)
	}
	return NumberValue(float64(serial))
}

// fnDatePart is YEAR (part 0), MONTH (1) and DAY (2).
func (ev *evaluator) fnDatePart(a []*node, part int) Value {
	serial, err := ev.dayNumber(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	y, m, d := civilOf(serial)
	return NumberValue(float64([3]int64{y, m, d}[part]))
}

func (ev *evaluator) fnDays(a []*node) Value {
	end, start, err := ev.num2(a)
	if err != ErrNone {
		return errValue(err)
	}
	return NumberValue(math.Floor(end) - math.Floor(start))
}

func (ev *evaluator) fnWeekday(a []*node) Value {
	serial, err := ev.dayNumber(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	kind := 1.0
	if len(a) > 1 {
		if kind, err = ev.whole(a[1]); err != ErrNone {
			return errValue(err)
		}
	}
	_, monday0 := floorDivMod(serial-2, 7) // day 2, 1900-01-01, is a Monday
	switch kind {
	case 1:
		return NumberValue(float64((monday0+1)%7 + 1))
	case 2:
		return NumberValue(float64(monday0 + 1))
	case 3:
		return NumberValue(float64(monday0))
	}
	return errValue(ErrNum)
}

// fnShiftMonths is EDATE, and EOMONTH when toEnd is set.
func (ev *evaluator) fnShiftMonths(a []*node, toEnd bool) Value {
	serial, err := ev.dayNumber(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	months, err := ev.wholeArg(a[1])
	if err != ErrNone {
		return errValue(err)
	}
	y, m, d := civilOf(serial)
	y, m = floorDivMod(y*12+m-1+months, 12)
	m++
	if last := monthLen(y, m); toEnd || d > last {
		d = last
	}
	shifted := serialOf(y, m, d)
	if shifted < 0 {
		return errValue(ErrNum)
	}
	return NumberValue(float64(shifted))
}

// fnDateDif counts the time from one date to a later one in the unit Y, M,
// D, MD, YM or YD.
func (ev *evaluator) fnDateDif(a []*node) Value {
	start, err := ev.dayNumber(a[0])
	if err != ErrNone {
		return errValue(err)
	}
	end, err := ev.dayNumber(a[1])
	if err != ErrNone {
		return errValue(err)
	}
	unit, err := ev.text(a[2])
	if err != ErrNone {
		return errValue(err)
	}
	// A unit has at most two letters, and no longer text becomes one in
	// capitals, so a long text is not gone through.
	if start > end || len(unit) > 2 {
		return errValue(ErrNum)
	}
	sy, sm, sd := civilOf(start)
	ey, em, ed := civilOf(end)
	months := (ey-sy)*12 + (em - sm)
	if ed < sd {
		months--
	}
	switch strings.ToUpper(unit) {
	case "D":
		return NumberValue(float64(end - start))
	case "M":
		return NumberValue(float64(months))
	case "Y":
		return NumberValue(float64(months / 12))
	case "YM":
		return NumberValue(float64(months % 12))
	case "MD":
		if ed >= sd {
			return NumberValue(float64(ed - sd))
		}
		// From the start day in the month before the end.
		py, pm := ey, em-1
		if em == 1 {
			py, pm = ey-1, 12
		}
		return NumberValue(float64(end - (serialOf(py, pm, 1) + sd - 1)))
	case "YD":
		anchor := serialOf(ey, sm, 1) + sd - 1
		if anchor > end {
			anchor = serialOf(ey-1, sm, 1) + sd - 1
		}
		return NumberValue(float64(end - anchor))
	}
	return errValue(ErrNum)
}

// fnTimePart is HOUR (part 0), MINUTE (1) and SECOND (2).
func (ev *evaluator) fnTimePart(a []*node, part int) Value {
	x, err := ev.num(a[0])
	switch {
	case err != ErrNone:
		return errValue(err)
	case x < 0:
		return errValue(ErrNum)
	}
	_, secs := splitDayTime(x)
	return NumberValue(float64([3]int{secs / 3600, secs / 60 % 60, secs % 60}[part]))
}

// fnTime adds hours, minutes and seconds before the range check, so
// negative minutes count back. A time past midnight wraps.
func (ev *evaluator) fnTime(a []*node) Value {
	var total int64
	for i, unit := range [3]int64{3600, 60, 1} {
		x, err := ev.wholeArg(a[i])
		if err != ErrNone {
			return errValue(err)
		}
		total += x * unit
	}
	if total < 0 {
		return errValue(ErrNum)
	}
	return NumberValue(float64(total%daySeconds) / daySeconds)
}
