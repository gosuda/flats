package formula

import (
	"math"
	"strconv"
)

// FillSeries returns the series that a drag of the fill handle writes on one
// line of target cells (spec rule 21a, technical design, section 8.5). src
// holds the contents of the line's source cells in order, n is the number
// of target cells, back tells that the target lies before the source, above
// or left of it, and step is the series mode, in which one plain number adds
// 1 for each cell. The series is listed from the target cell next to the
// source outward, so for a target before the source its first content is
// that of the cell just before the first source cell.
//
// It returns false, and no contents, when the source makes no series, and
// the line is then a copy. The work is linear in the source and the target.
// A text of the series is not checked against the length limit.
func FillSeries(src []Content, n int, back, step bool) ([]Content, bool) {
	if len(src) == 0 || n <= 0 {
		return nil, false
	}
	kind := src[0].Kind
	for _, c := range src[1:] {
		if c.Kind != kind {
			return nil, false
		}
	}
	switch kind {
	case ContentNumber, ContentDate, ContentTime, ContentDateTime:
		return fillNumbers(src, n, back, step)
	case ContentText:
		if out, ok := fillNames(src, n, back); ok {
			return out, true
		}
		return fillNumbered(src, n, back)
	}
	return nil, false
}

// trend is the line fitted by least squares through the values y at the
// positions 1 to m. Its value at position p is
//
//	((m²-1)·Σy + 3·(2p-m-1)·Σ(2x-m-1)·y) / (m·(m²-1))
//
// which keeps the integers apart from the values and divides once, so that
// small whole values give the correctly rounded result: 1, 2, 4 gives 16/3
// and 41/6.
type trend struct {
	m        float64
	sum, mom float64 // Σy and Σ(2x-m-1)·y
}

func fitTrend(y func(i int) float64, m int) trend {
	t := trend{m: float64(m)}
	for i := 0; i < m; i++ {
		v := y(i)
		t.sum += v
		t.mom += float64(2*(i+1)-m-1) * v
	}
	return t
}

func (t trend) at(p float64) float64 {
	sq := t.m*t.m - 1
	return (sq*t.sum + 3*(2*p-t.m-1)*t.mom) / (t.m * sq)
}

// position returns the position of the k-th target cell, counted from 0
// next to the source, for a source of m cells.
func position(k, m int, back bool) float64 {
	if back {
		return float64(-k)
	}
	return float64(m + 1 + k)
}

// fillNumbers continues numbers, dates, times and dates with time.
func fillNumbers(src []Content, n int, back, step bool) ([]Content, bool) {
	kind, m := src[0].Kind, len(src)
	if kind == ContentNumber && m == 1 && !step {
		return nil, false // one plain number is copied
	}
	var next func(k int) float64
	if m == 1 {
		// One cell steps by 1, a day for a date, and an hour for a time.
		unit := 1.0
		if kind == ContentTime {
			unit = 1.0 / 24
		}
		if back {
			unit = -unit
		}
		x := src[0].Num
		next = func(k int) float64 { return x + float64(k+1)*unit }
	} else {
		t := fitTrend(func(i int) float64 { return src[i].Num }, m)
		next = func(k int) float64 { return t.at(position(k, m, back)) }
	}
	out := make([]Content, n)
	for k := range out {
		v := next(k)
		switch kind {
		case ContentDate:
			v = math.Round(v)
		case ContentTime:
			if v -= math.Floor(v); v >= 1 {
				v = 0 // a value just under 0 that the subtraction rounds to a day
			}
		}
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return nil, false
		}
		if (kind == ContentDate || kind == ContentDateTime) && (v < 0 || v >= lastDateDay+1) {
			return nil, false // a series stays within the dates of rule 12
		}
		out[k] = Content{Kind: kind, Num: v}
	}
	return out, true
}

var (
	weekdayNames   = [7]string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
	monthFullNames = [12]string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
)

// nameForm is how a name of the fill is written.
type nameForm struct {
	months bool // a month name, not a weekday
	full   bool // the full name, not its first three letters
	both   bool // a name that is both, as May is
}

// maxFillDigits is the most digits of the number at the end of a text that
// a fill continues.
const maxFillDigits = 15

// readName reads a weekday or month name in its full or three-letter
// English form, capitals aside, and returns its place in its cycle.
func readName(s string) (int, nameForm, bool) {
	for i, name := range weekdayNames {
		switch {
		case asciiEqualFold(s, name):
			return i, nameForm{full: true}, true
		case asciiEqualFold(s, name[:3]):
			return i, nameForm{}, true
		}
	}
	for i, name := range monthFullNames {
		full, short := asciiEqualFold(s, name), asciiEqualFold(s, monthNames[i])
		if full || short {
			return i, nameForm{months: true, full: full, both: full && short}, true
		}
	}
	return 0, nameForm{}, false
}

// asciiEqualFold reports whether s is name apart from the capitals of ASCII
// letters. name holds letters only.
func asciiEqualFold(s, name string) bool {
	if len(s) != len(name) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i]|0x20 != name[i]|0x20 || s[i]|0x20 < 'a' || s[i]|0x20 > 'z' {
			return false
		}
	}
	return true
}

// fillNames continues weekday and month names. One name steps by one, and
// two or more by the distance from one to the next in the cycle, which must
// be the same each time; equal names step by none and repeat the name.
func fillNames(src []Content, n int, back bool) ([]Content, bool) {
	at := make([]int, len(src))
	var form nameForm // the three-letter form unless a cell says otherwise
	for i, c := range src {
		place, f, ok := readName(c.Text)
		if !ok || (i > 0 && f.months != form.months) {
			return nil, false
		}
		at[i], form.months = place, f.months
		// The form is that of the last cell whose name has one form. May is
		// both forms, and leaves the form of a cell before it, or the
		// three-letter form when no cell has one.
		if !f.both {
			form.full = f.full
		}
	}
	cycle := 7
	if form.months {
		cycle = 12
	}
	stride := 1
	if len(at) > 1 {
		stride = (at[1] - at[0] + cycle) % cycle
		for i := 2; i < len(at); i++ {
			if (at[i]-at[i-1]+cycle)%cycle != stride {
				return nil, false
			}
		}
	}
	from, dir := at[len(at)-1], stride
	if back {
		from, dir = at[0], -stride
	}
	caps := capitalsOf(src[len(src)-1].Text)
	out := make([]Content, n)
	for k := range out {
		place := ((from+(k+1)*dir)%cycle + cycle) % cycle
		var name string
		switch {
		case form.months && form.full:
			name = monthFullNames[place]
		case form.months:
			name = monthNames[place]
		case form.full:
			name = weekdayNames[place]
		default:
			name = weekdayNames[place][:3]
		}
		out[k] = Content{Kind: ContentText, Text: caps(name)}
	}
	return out, true
}

// capitalsOf returns how a name is written in the capitals of s: all
// capitals when every letter of s is one, an initial capital when only the
// first letter is, and small letters otherwise.
func capitalsOf(s string) func(name string) string {
	upper := 0
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			upper++
		}
	}
	return func(name string) string {
		b := []byte(name)
		for i := range b {
			b[i] |= 0x20
		}
		switch {
		case upper == len(s):
			for i := range b {
				b[i] &^= 0x20
			}
		case upper == 1 && s[0] >= 'A' && s[0] <= 'Z':
			b[0] &^= 0x20
		}
		return string(b)
	}
}

// fillNumbered continues texts that end in a number: all with the same
// text before it and with a number of at most 15 digits.
func fillNumbered(src []Content, n int, back bool) ([]Content, bool) {
	stem, width := "", 0
	values := make([]float64, len(src))
	for i, c := range src {
		end := len(c.Text)
		start := end
		for start > 0 && isDigit(c.Text[start-1]) {
			start--
		}
		digits := end - start
		if digits == 0 || digits > maxFillDigits || (i > 0 && c.Text[:start] != stem) {
			return nil, false
		}
		stem, width = c.Text[:start], digits
		v, _ := strconv.ParseInt(c.Text[start:], 10, 64)
		values[i] = float64(v)
	}
	m := len(values)
	var next func(k int) float64
	if m == 1 {
		dir := 1.0
		if back {
			dir = -1
		}
		next = func(k int) float64 { return values[0] + float64(k+1)*dir }
	} else {
		t := fitTrend(func(i int) float64 { return values[i] }, m)
		next = func(k int) float64 { return math.Round(t.at(position(k, m, back))) }
	}
	out := make([]Content, n)
	var buf []byte
	for k := range out {
		v := math.Abs(next(k)) // a number below 0 is written without its sign
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return nil, false
		}
		buf = append(buf[:0], stem...)
		if v < 1e18 {
			buf = appendPadded(buf, int64(v), width)
		} else {
			buf = strconv.AppendFloat(buf, v, 'f', 0, 64) // more digits than width
		}
		out[k] = Content{Kind: ContentText, Text: string(buf)}
	}
	return out, true
}
