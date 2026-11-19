package formula

import (
	"math"
	"strconv"
	"strings"
)

// AppendCSVField appends the CSV field of a cell (spec rule 60) from its
// value, the kind of its content and its number format, never from the
// text it shows. kind is passed as for Show, ContentDate for a formula
// whose DateResult is true.
//
// A blank value is an empty field, a boolean TRUE or FALSE and an error its
// name. A number that Show draws as a date, a time or a date with time is
// written in ISO 8601 form, rounded to the millisecond, and any other
// number with up to 15 significant digits, rounded as Show rounds it. Text
// is written as it is, with a leading
// apostrophe where a program could read it as a formula. A field that
// needs them is put in quotation marks. Nothing but dst is allocated.
func AppendCSVField(dst []byte, v Value, kind ContentKind, nf NumberFormat) []byte {
	switch v.kind {
	case KindText:
		return appendCSVText(dst, v.str)
	case KindBool:
		if v.num != 0 {
			return append(dst, "TRUE"...)
		}
		return append(dst, "FALSE"...)
	case KindError:
		return append(dst, v.Err().String()...)
	case KindNumber:
		if nf.Kind == NumberGeneral {
			switch kind {
			case ContentDate:
				nf = NumberFormat{Kind: NumberDate}
			case ContentTime:
				nf = NumberFormat{Kind: NumberTime}
			case ContentDateTime:
				nf = NumberFormat{Kind: NumberDateTime}
			}
		}
		switch x := v.num; nf.Kind {
		case NumberDate, NumberTime, NumberDateTime:
			// Show draws a negative number, or one past the last date, in
			// General.
			if x >= 0 && x < lastDateDay+1 {
				return appendISO(dst, x, nf.Kind)
			}
		}
		return appendCSVNumber(dst, v.num)
	}
	return dst
}

// dayMillis is the number of milliseconds in a day.
const dayMillis = daySeconds * 1000

// appendISO writes a value that Show draws with a date or time pattern of
// the given kind in ISO 8601 form. The value is rounded to the millisecond,
// and a carry goes on into the seconds, minutes, hours and days. A value
// below one day under a time format stays a time, so a carry to a whole day
// writes midnight. A date with a fraction of a day, and a value of a whole
// day or more under a time format, are written as a date with time.
func appendISO(dst []byte, x float64, kind NumberKind) []byte {
	days := math.Floor(x)
	ms := int64(math.Round((x - days) * dayMillis))
	day := int64(days)
	if ms >= dayMillis {
		day, ms = day+1, ms-dayMillis
	}
	if clock := kind == NumberTime && x < 1; !clock {
		dst = appendDate(dst, day, DateISO)
		if kind == NumberDate && ms == 0 {
			return dst
		}
		dst = append(dst, ' ')
	}
	secs := ms / 1000
	dst = appendTime(dst, int(secs), TimeHMS)
	if milli := ms % 1000; milli != 0 {
		dst = append(dst, '.')
		dst = appendPadded(dst, milli, 3)
	}
	return dst
}

// appendCSVNumber writes x rounded to 15 significant digits, half away from
// zero as Show rounds it, with no zeros at the end: plain with at most 15
// digits before the point and 9 zeros after it, and otherwise with an
// exponent, as in 1.23456789012346E+15 and 1E-11. Zero of either sign is 0.
func appendCSVNumber(dst []byte, x float64) []byte {
	d := toDecimal(x)
	if d.zero || (d.exp < sigDigits && d.exp > -11) {
		return d.appendPlain(dst)
	}
	exp := d.exp
	d.exp = 0
	dst = d.appendPlain(dst)
	dst = append(dst, 'E')
	if exp < 0 {
		dst = append(dst, '-')
		exp = -exp
	} else {
		dst = append(dst, '+')
	}
	return strconv.AppendInt(dst, int64(exp), 10)
}

// csvFormulaStarts are the first characters, after leading spaces, of a
// text that a program could read as a formula: = + - @, a tab, a carriage
// return, a line feed, and the full-width forms of the first four.
var csvFormulaStarts = [...]string{"=", "+", "-", "@", "\t", "\r", "\n", "＝", "＋", "－", "＠"}

// appendCSVText writes a text field: with a leading apostrophe where a
// program could read the text as a formula, and in quotation marks where
// the field needs them.
func appendCSVText(dst []byte, s string) []byte {
	t := strings.TrimLeft(s, " ")
	apostrophe := false
	for _, start := range csvFormulaStarts {
		if strings.HasPrefix(t, start) {
			apostrophe = true
			break
		}
	}
	if !strings.ContainsAny(s, ",;\t\"\r\n") {
		if apostrophe {
			dst = append(dst, '\'')
		}
		return append(dst, s...)
	}
	dst = append(dst, '"')
	if apostrophe {
		dst = append(dst, '\'')
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			dst = append(dst, '"')
		}
		dst = append(dst, s[i])
	}
	return append(dst, '"')
}
