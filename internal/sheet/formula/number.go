package formula

import (
	"math"
	"math/bits"
	"strconv"
)

// sigDigits is the number of significant digits a number is rounded to
// before it is shown, compared or rounded by a function.
const sigDigits = 15

// decimal is a number rounded to 15 significant digits, half away from
// zero. Its value is dig[0].dig[1]...dig[14] times ten to the power exp.
// The zero decimal has zero set and nothing else.
type decimal struct {
	neg  bool
	zero bool
	exp  int
	dig  [sigDigits]byte // digit values 0 to 9, dig[0] is not 0
}

type roundMode uint8

const (
	roundHalfAway roundMode = iota
	roundAway
	roundToZero
)

// toDecimal rounds a finite number to 15 significant digits, half away from
// zero, from its exact binary value.
func toDecimal(x float64) decimal {
	if x == 0 {
		return decimal{zero: true}
	}
	d := decimal{neg: x < 0}
	x = math.Abs(x)
	if x < 1e15 && x == math.Trunc(x) {
		var buf [20]byte
		s := strconv.AppendUint(buf[:0], uint64(x), 10)
		d.exp = len(s) - 1
		for i, c := range s {
			d.dig[i] = c - '0'
		}
		return d
	}
	// 17 digits, correctly rounded. The 16th decides, and it is the true
	// 16th digit unless the text ends in "50", which a value just under
	// the half also rounds to.
	var buf [32]byte
	s := strconv.AppendFloat(buf[:0], x, 'e', 16, 64)
	next := s[16]
	if s[16] == '5' && s[17] == '0' {
		var exact [840]byte
		next = strconv.AppendFloat(exact[:0], x, 'e', 800, 64)[16]
	}
	d.dig[0] = s[0] - '0'
	for i := 1; i < sigDigits; i++ {
		d.dig[i] = s[i+1] - '0'
	}
	d.exp, _ = strconv.Atoi(string(s[20:]))
	if s[19] == '-' {
		d.exp = -d.exp
	}
	if next >= '5' {
		d.increment(sigDigits)
	}
	return d
}

// increment adds one unit to digit n-1 and carries.
func (d *decimal) increment(n int) {
	for i := n - 1; i >= 0; i-- {
		if d.dig[i] < 9 {
			d.dig[i]++
			return
		}
		d.dig[i] = 0
	}
	d.dig[0] = 1
	d.exp++
}

// round rounds d to the given number of digits after the decimal point,
// which may be negative.
func (d *decimal) round(digits int, mode roundMode) {
	if d.zero {
		return
	}
	keep := d.exp + digits + 1 // digits of dig that stay
	if keep >= sigDigits {
		return
	}
	up := false
	switch mode {
	case roundHalfAway:
		up = keep >= 0 && d.dig[keep] >= 5
	case roundAway:
		for i := max(keep, 0); i < sigDigits; i++ {
			if d.dig[i] != 0 {
				up = true
				break
			}
		}
	}
	if keep <= 0 {
		if !up {
			*d = decimal{zero: true}
			return
		}
		neg := d.neg
		*d = decimal{neg: neg, exp: -digits}
		d.dig[0] = 1
		return
	}
	for i := keep; i < sigDigits; i++ {
		d.dig[i] = 0
	}
	if up {
		d.increment(keep)
	}
}

// shift multiplies d by ten to the power n.
func (d *decimal) shift(n int) {
	if !d.zero {
		d.exp += n
	}
}

// digitAt returns the digit of d that counts ten to the power p.
func (d *decimal) digitAt(p int) byte {
	i := d.exp - p
	if d.zero || i < 0 || i >= sigDigits {
		return 0
	}
	return d.dig[i]
}

// float returns the number nearest to d.
func (d *decimal) float() float64 {
	if d.zero {
		return 0
	}
	var buf [32]byte
	s := buf[:0]
	if d.neg {
		s = append(s, '-')
	}
	for _, c := range d.dig {
		s = append(s, '0'+c)
	}
	s = append(s, 'e')
	s = strconv.AppendInt(s, int64(d.exp-(sigDigits-1)), 10)
	x, _ := strconv.ParseFloat(string(s), 64) // an overflow gives an infinity, which callers turn into #NUM!
	return x
}

// round15 returns x rounded to 15 significant digits.
func round15(x float64) float64 {
	if x == math.Trunc(x) && math.Abs(x) < 1e15 {
		return x
	}
	d := toDecimal(x)
	return d.float()
}

// compareNumbers compares two numbers after rounding both to 15 significant
// digits, so that 0.1+0.2 equals 0.3.
func compareNumbers(a, b float64) int {
	if a == b {
		return 0
	}
	// Numbers that round to the same 15 digits lie within one unit of the
	// 15th digit, which is at most 1e-14 of the rounded number.
	if diff, lim := math.Abs(a-b), 1.1e-14*math.Max(math.Abs(a), math.Abs(b)); diff > lim {
		if a < b {
			return -1
		}
		return 1
	}
	a, b = round15(a), round15(b)
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// appendPlain appends d in plain notation with its trailing zeros dropped.
func (d *decimal) appendPlain(dst []byte) []byte {
	if d.zero {
		return append(dst, '0')
	}
	if d.neg {
		dst = append(dst, '-')
	}
	last := sigDigits - 1
	for last > 0 && d.dig[last] == 0 {
		last--
	}
	if d.exp < 0 {
		dst = append(dst, '0', '.')
		for i := -1; i > d.exp; i-- {
			dst = append(dst, '0')
		}
		for i := 0; i <= last; i++ {
			dst = append(dst, '0'+d.dig[i])
		}
		return dst
	}
	for i := 0; i <= d.exp; i++ {
		dst = append(dst, '0'+d.digitAt(d.exp-i))
	}
	if last > d.exp {
		dst = append(dst, '.')
		for i := d.exp + 1; i <= last; i++ {
			dst = append(dst, '0'+d.dig[i])
		}
	}
	return dst
}

// appendGeneral appends the General text of a number: up to 15 significant
// digits, and scientific notation with at most 6 significant digits for a
// number that needs more than 15 digits before the decimal point or more
// than 9 zeros after it.
func appendGeneral(dst []byte, x float64) []byte {
	d := toDecimal(x)
	if d.zero {
		return append(dst, '0')
	}
	if d.exp < 15 && d.exp > -11 {
		return d.appendPlain(dst)
	}
	e := d.exp
	d.exp = 0
	d.round(5, roundHalfAway)
	e += d.exp // a carry past the first digit moves the exponent
	d.exp = 0
	dst = d.appendPlain(dst)
	dst = append(dst, 'E')
	if e < 0 {
		dst = append(dst, '-')
		e = -e
	} else {
		dst = append(dst, '+')
	}
	if e < 10 {
		dst = append(dst, '0')
	}
	return strconv.AppendInt(dst, int64(e), 10)
}

func generalText(x float64) string {
	if x == math.Trunc(x) && math.Abs(x) < 1e15 {
		return strconv.FormatInt(int64(x), 10)
	}
	var buf [32]byte
	return string(appendGeneral(buf[:0], x))
}

// appendFixed appends x with a fixed number of decimals, after rounding to
// 15 significant digits and then to the decimals, half away from zero.
// shift multiplies by a power of ten first, 2 for a percent. prefix goes
// between the minus sign and the digits.
func appendFixed(dst []byte, x float64, decimals int, thousands bool, shift int, prefix string) []byte {
	d := toDecimal(x)
	d.shift(shift)
	d.round(decimals, roundHalfAway)
	if d.neg && !d.zero {
		dst = append(dst, '-')
	}
	dst = append(dst, prefix...)
	whole := 1
	if !d.zero && d.exp > 0 {
		whole = d.exp + 1
	}
	for p := whole - 1; p >= 0; p-- {
		dst = append(dst, '0'+d.digitAt(p))
		if thousands && p > 0 && p%3 == 0 {
			dst = append(dst, ',')
		}
	}
	if decimals > 0 {
		dst = append(dst, '.')
		for p := -1; p >= -decimals; p-- {
			dst = append(dst, '0'+d.digitAt(p))
		}
	}
	return dst
}

// roundNumber rounds x as ROUND, ROUNDUP and ROUNDDOWN do: to 15
// significant digits first, then to the given decimals.
func roundNumber(x float64, digits int, mode roundMode) float64 {
	d := toDecimal(x)
	d.round(digits, mode)
	return d.float()
}

// splitDayTime splits a number that is not negative into whole days and the
// seconds of the day, after rounding to 15 significant digits. Half a
// second rounds up, and a full day of seconds becomes the next day.
func splitDayTime(x float64) (days int64, secs int) {
	d := toDecimal(x)
	if d.zero || d.neg {
		return 0, 0
	}
	if d.exp >= sigDigits-1 {
		return int64(d.float()), 0
	}
	var whole, frac uint64
	for i := 0; i < sigDigits; i++ {
		if i <= d.exp {
			whole = whole*10 + uint64(d.dig[i])
		} else {
			frac = frac*10 + uint64(d.dig[i])
		}
	}
	scale := sigDigits - 1 - d.exp // frac counts ten to the power -scale
	var s uint64
	if scale <= 19 {
		den := uint64(1)
		for i := 0; i < scale; i++ {
			den *= 10
		}
		hi, lo := bits.Mul64(frac, 86400)
		q, r := bits.Div64(hi, lo, den)
		if r >= den-r {
			q++
		}
		s = q
	} else {
		s = uint64(math.Floor(float64(d.float()*86400) + 0.5))
	}
	if s >= 86400 {
		return int64(whole) + 1, 0
	}
	return int64(whole), int(s)
}
