package formula

import (
	"math"
	"strconv"
	"strings"
)

// typedKind is how rule 5 reads a piece of text.
type typedKind uint8

const (
	typedText typedKind = iota
	typedNumber
	typedPercent
	typedCurrency
	typedDate
	typedTime
	typedDateTime
)

// typedValue is text read as a number, a date or a time.
type typedValue struct {
	kind     typedKind
	num      float64
	decimals uint8       // of a percent
	symbol   string      // of a currency amount, one of currencySigns
	date     DatePattern // of a date
	time     TimePattern // of a time
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// currencySigns are the currency signs of rule 5. The format of a typed
// amount holds one of these constants, not a part of the amount, which
// may be thousands of digits long and would stay alive with the format.
var currencySigns = [...]string{"$", "€", "£", "¥", "₩"}

// currencySign returns the currency sign of rule 5 at the start of s, or
// the empty string.
func currencySign(s string) string {
	for _, sign := range currencySigns {
		if strings.HasPrefix(s, sign) {
			return sign
		}
	}
	return ""
}

// numberParts are the parts of the number form of rule 5.
type numberParts struct {
	neg      bool
	symbol   string
	whole    string // may hold commas
	frac     string // without the period
	exponent string // without the E
	percent  bool
}

// scanNumber reads the number form of rule 5: an optional minus sign, at
// most one currency sign, digits with optional commas between groups of
// three, optional decimals after a period, and then an exponent or a
// percent sign, or neither.
func scanNumber(s string) (p numberParts, ok bool) {
	if s != "" && s[0] == '-' {
		p.neg = true
		s = s[1:]
	}
	if sign := currencySign(s); sign != "" {
		p.symbol, s = sign, s[len(sign):]
	}
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	if i == 0 {
		return p, false
	}
	if i <= 3 && i < len(s) && s[i] == ',' {
		for i+3 < len(s) && s[i] == ',' && isDigit(s[i+1]) && isDigit(s[i+2]) && isDigit(s[i+3]) {
			i += 4
		}
		if i < len(s) && (s[i] == ',' || isDigit(s[i])) {
			return p, false
		}
	}
	p.whole, s = s[:i], s[i:]
	if s != "" && s[0] == '.' {
		i = 1
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		if i == 1 {
			return p, false
		}
		p.frac, s = s[1:i], s[i:]
	}
	switch {
	case s == "":
		return p, true
	case s == "%":
		p.percent = true
		return p, true
	case s[0] == 'E' || s[0] == 'e':
		i = 1
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		digits := i
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		if i == digits || i != len(s) {
			return p, false
		}
		p.exponent = s[1:]
		return p, true
	}
	return p, false
}

// readNumber reads the number forms of rule 5.
func readNumber(s string) (typedValue, bool) {
	p, ok := scanNumber(s)
	if !ok {
		return typedValue{}, false
	}
	var x float64
	var err error
	if p.symbol == "" && !p.percent && strings.IndexByte(p.whole, ',') < 0 {
		// The text is the number as strconv reads it: a sign, digits,
		// decimals and an exponent. It is read in place, however long.
		x, err = strconv.ParseFloat(s, 64)
	} else {
		var stack [48]byte
		buf := stack[:0]
		if p.neg {
			buf = append(buf, '-')
		}
		for i := 0; i < len(p.whole); i++ {
			if p.whole[i] != ',' {
				buf = append(buf, p.whole[i])
			}
		}
		if p.frac != "" {
			buf = append(buf, '.')
			buf = append(buf, p.frac...)
		}
		switch {
		case p.percent:
			buf = append(buf, "e-2"...)
		case p.exponent != "":
			buf = append(buf, 'e')
			buf = append(buf, p.exponent...)
		}
		x, err = strconv.ParseFloat(string(buf), 64)
	}
	if err != nil && math.IsInf(x, 0) {
		return typedValue{}, false
	}
	v := typedValue{kind: typedNumber, num: x}
	switch {
	case p.percent:
		v.kind = typedPercent
		if p.frac != "" {
			v.decimals = 2
		}
	case p.symbol != "":
		v.kind, v.symbol = typedCurrency, p.symbol
	}
	return v, true
}

// maxTypedDigits is the most digits of a whole number that typed entry
// reads as a number (rule 5). Excel keeps 15 and changes later ones to 0.
const maxTypedDigits = 15

// keepsDigits reports whether typed entry keeps a number form as text: its
// value before any % is whole, and it has more than maxTypedDigits digits
// as typed, without leading zeros and without zeros at the end after the
// decimal point. The sign, commas and a currency sign change nothing. It
// works on the digits as typed, since a double above 2^53 holds no
// fraction that would tell whether the typed number had one.
func keepsDigits(p numberParts) bool {
	frac := strings.TrimRight(p.frac, "0")
	digits, zeros := 0, 0 // the digits counted, and the zeros they end in
	for _, s := range [2]string{p.whole, frac} {
		for i := 0; i < len(s); i++ {
			switch c := s[i]; {
			case c == ',' || (c == '0' && digits == 0):
			case c == '0':
				digits++
				zeros++
			default:
				digits++
				zeros = 0
			}
		}
	}
	if digits <= maxTypedDigits {
		return false
	}
	// The value is the digits times ten to the power of the exponent less
	// the decimals. The zeros the digits end in make up for a power below
	// 0 as far as they go.
	return exponentOf(p.exponent)-len(frac)+zeros >= 0
}

// exponentOf reads the exponent of a number form, such as "+20" or "-5".
// One far beyond what a double holds is held at a million either way.
func exponentOf(s string) int {
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg, s = s[0] == '-', s[1:]
	}
	e := 0
	for i := 0; i < len(s) && e < 1e6; i++ {
		e = e*10 + int(s[i]-'0')
	}
	e = min(e, 1e6)
	if neg {
		return -e
	}
	return e
}

// keepsText reports whether typed entry keeps text that readValue reads as
// v as the text itself: a number form with more digits than a number keeps.
// VALUE, criteria and arithmetic read it as v (technical design, section
// 9.5).
func keepsText(v typedValue, s string) bool {
	switch v.kind {
	case typedNumber, typedPercent, typedCurrency:
		p, _ := scanNumber(s)
		return keepsDigits(p)
	}
	return false
}

// digitRun reads between lo and hi digits at the start of s.
func digitRun(s string, lo, hi int) (v int64, rest string, ok bool) {
	i := 0
	for i < len(s) && i < hi && isDigit(s[i]) {
		v = v*10 + int64(s[i]-'0')
		i++
	}
	if i < lo {
		return 0, s, false
	}
	return v, s[i:], true
}

func expect(s string, c byte) (string, bool) {
	if s == "" || s[0] != c {
		return s, false
	}
	return s[1:], true
}

// maxDateLen is the longest text readDate reads as a date, in bytes: one
// like Mar 15, 2026.
const maxDateLen = 12

// readDate reads a date as 2026-03-15, 3/15/2026 or Mar 15, 2026.
func readDate(s string) (serial int64, pattern DatePattern, ok bool) {
	var y, m, d int64
	switch {
	case len(s) == 10 && s[4] == '-' && s[7] == '-':
		var r string
		if y, r, ok = digitRun(s, 4, 4); !ok {
			return 0, 0, false
		}
		if m, r, ok = digitRun(r[1:], 2, 2); !ok {
			return 0, 0, false
		}
		if d, r, ok = digitRun(r[1:], 2, 2); !ok || r != "" {
			return 0, 0, false
		}
		pattern = DateISO
	case s != "" && isDigit(s[0]):
		r := s
		if m, r, ok = digitRun(r, 1, 2); !ok {
			return 0, 0, false
		}
		if r, ok = expect(r, '/'); !ok {
			return 0, 0, false
		}
		if d, r, ok = digitRun(r, 1, 2); !ok {
			return 0, 0, false
		}
		if r, ok = expect(r, '/'); !ok {
			return 0, 0, false
		}
		if y, r, ok = digitRun(r, 4, 4); !ok || r != "" {
			return 0, 0, false
		}
		pattern = DateMDY
	case len(s) >= 11 && s[3] == ' ':
		for i, name := range monthNames {
			if strings.EqualFold(s[:3], name) && isASCIILetters(s[:3]) {
				m = int64(i) + 1
			}
		}
		if m == 0 {
			return 0, 0, false
		}
		r := s[4:]
		if d, r, ok = digitRun(r, 1, 2); !ok {
			return 0, 0, false
		}
		if r, ok = expect(r, ','); !ok {
			return 0, 0, false
		}
		if r, ok = expect(r, ' '); !ok {
			return 0, 0, false
		}
		if y, r, ok = digitRun(r, 4, 4); !ok || r != "" {
			return 0, 0, false
		}
		pattern = DateMonth
	default:
		return 0, 0, false
	}
	if !validDate(y, m, d) {
		return 0, 0, false
	}
	return serialOf(y, m, d), pattern, true
}

func isASCIILetters(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i] | 0x20; c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// readTime reads a time as 14:30, 14:30:05 or 2:30 PM.
func readTime(s string) (secs int, pattern TimePattern, ok bool) {
	h, r, ok := digitRun(s, 1, 2)
	if !ok {
		return 0, 0, false
	}
	if r, ok = expect(r, ':'); !ok {
		return 0, 0, false
	}
	mi, r, ok := digitRun(r, 2, 2)
	if !ok {
		return 0, 0, false
	}
	var sec int64
	hasSec := false
	if r != "" && r[0] == ':' {
		if sec, r, ok = digitRun(r[1:], 2, 2); !ok {
			return 0, 0, false
		}
		hasSec = true
	}
	if len(r) == 3 && r[0] == ' ' && (r[1]|0x20 == 'a' || r[1]|0x20 == 'p') && r[2]|0x20 == 'm' {
		if h < 1 || h > 12 || mi > 59 || hasSec {
			return 0, 0, false
		}
		h %= 12
		if r[1]|0x20 == 'p' {
			h += 12
		}
		return int(h*3600 + mi*60), TimeAMPM, true
	}
	if r != "" || h > 23 || mi > 59 || sec > 59 {
		return 0, 0, false
	}
	if hasSec {
		return int(h*3600 + mi*60 + sec), TimeHMS, true
	}
	return int(h*3600 + mi*60), TimeHM, true
}

// readValue reads text as a number, a date, a time or a date with time by
// rule 5. It is what typed entry, VALUE, criteria and the coercion of text
// to a number share.
func readValue(s string) (typedValue, bool) {
	if v, ok := readNumber(s); ok {
		return v, true
	}
	if serial, p, ok := readDate(s); ok {
		return typedValue{kind: typedDate, num: float64(serial), date: p}, true
	}
	if secs, p, ok := readTime(s); ok {
		return typedValue{kind: typedTime, num: float64(secs) / daySeconds, time: p}, true
	}
	// A date with a time has its date before a space that is no further
	// in than a date is long, so a long text is not gone through for one.
	for i := 0; i < len(s) && i <= maxDateLen; i++ {
		if s[i] != ' ' {
			continue
		}
		serial, dp, ok := readDate(s[:i])
		if !ok {
			continue
		}
		if secs, tp, ok := readTime(s[i+1:]); ok {
			return typedValue{kind: typedDateTime, num: float64(serial) + float64(secs)/daySeconds, date: dp, time: tp}, true
		}
	}
	return typedValue{}, false
}

// textToNumber is the number that text reads as where an operator or a
// function needs a number (technical design, section 9.3).
func textToNumber(s string) (float64, bool) {
	if s == "" || s[0] == '\'' || s[0] == '=' {
		return 0, false
	}
	v, ok := readValue(s)
	return v.num, ok
}

func isBoolText(s string) (value, ok bool) {
	switch {
	case len(s) == 4 && strings.EqualFold(s, "TRUE"):
		return true, true
	case len(s) == 5 && strings.EqualFold(s, "FALSE"):
		return false, true
	}
	return false, false
}

// ParseTyped reads what a person typed into a cell (spec rule 5). It
// returns the content and the number format the entry sets on a cell
// without a number format, or nil when it sets none. A whole number typed
// with more than 15 digits stays text, so that no digit is lost.
//
// A formula comes back with its typed text in Content.Text; the caller
// parses it with Parse. Text content is text itself, or a copy of it
// after an apostrophe; nothing else that comes back keeps a part of text
// alive. ParseTyped does not check the length of text.
func ParseTyped(text string) (Content, *NumberFormat) {
	switch {
	case text == "":
		return Content{}, nil
	case text == "'":
		return Content{Kind: ContentText}, nil // an empty text keeps nothing of text alive
	case text[0] == '\'':
		// Without the apostrophe the text may fit a smaller block of the
		// allocator than the typed text, and a holder is charged for that.
		return Content{Kind: ContentText, Text: strings.Clone(text[1:])}, nil
	case text[0] == '=' && len(text) > 1:
		return Content{Kind: ContentFormula, Text: text}, nil
	}
	if b, ok := isBoolText(text); ok {
		if b {
			return Content{Kind: ContentBool, Num: 1}, nil
		}
		return Content{Kind: ContentBool}, nil
	}
	v, ok := readValue(text)
	if !ok || keepsText(v, text) {
		return Content{Kind: ContentText, Text: text}, nil
	}
	switch v.kind {
	case typedPercent:
		return Content{Kind: ContentNumber, Num: v.num}, &NumberFormat{Kind: NumberPercent, Decimals: v.decimals}
	case typedCurrency:
		return Content{Kind: ContentNumber, Num: v.num}, &NumberFormat{Kind: NumberCurrency, Symbol: v.symbol, Decimals: 2}
	case typedDate:
		return Content{Kind: ContentDate, Num: v.num}, &NumberFormat{Kind: NumberDate, Date: v.date}
	case typedTime:
		return Content{Kind: ContentTime, Num: v.num}, &NumberFormat{Kind: NumberTime, Time: v.time}
	case typedDateTime:
		return Content{Kind: ContentDateTime, Num: v.num}, &NumberFormat{Kind: NumberDateTime, Date: v.date, Time: v.time}
	}
	return Content{Kind: ContentNumber, Num: v.num}, nil
}

// EditText returns what the formula bar shows for the content of the cell
// at (spec rule 18): the formula text for a formula, and otherwise the
// content as it would be typed, so that ParseTyped reads it back as the
// same content.
func EditText(c Content, at Pos) string {
	switch c.Kind {
	case ContentNumber:
		return editNumber(c.Num)
	case ContentText:
		if textReadsAsItself(c.Text) {
			return c.Text
		}
		return "'" + c.Text
	case ContentBool:
		if c.Num != 0 {
			return "TRUE"
		}
		return "FALSE"
	case ContentDate, ContentTime, ContentDateTime:
		return editDateTime(c)
	case ContentFormula:
		f, err := ParseStored(c.Text)
		if err != nil {
			return c.Text
		}
		return f.Text(at)
	case ContentUnreadable:
		return c.Text
	}
	return ""
}

// textReadsAsItself reports whether typed entry reads s as the text s.
func textReadsAsItself(s string) bool {
	if s == "" || s[0] == '\'' || (s[0] == '=' && len(s) > 1) {
		return false
	}
	if _, ok := isBoolText(s); ok {
		return false
	}
	v, ok := readValue(s)
	return !ok || keepsText(v, s)
}

// editNumber writes a number with the fewest digits that read back as the
// same number, in a form typed entry reads. A whole number of more than 15
// digits before the point is written with an exponent when its digits are
// at most 15, so that entry reads it back as the number, and as its plain
// digits otherwise, which entry keeps as text (technical design, section
// 9.5).
func editNumber(x float64) string {
	whole := x == math.Trunc(x)
	if whole && math.Abs(x) < 1e15 {
		return strconv.FormatInt(int64(x), 10)
	}
	var buf [32]byte
	s := strconv.AppendFloat(buf[:0], x, 'e', -1, 64)
	e := 0
	mant := s
	for i, c := range s {
		if c == 'e' {
			e, _ = strconv.Atoi(string(s[i+1:]))
			mant = s[:i]
			break
		}
	}
	plain := e >= -7 && e < 21
	if whole {
		digits := 0
		for _, c := range mant {
			if c >= '0' && c <= '9' {
				digits++
			}
		}
		plain = digits > sigDigits
	}
	if plain {
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	out := append([]byte(nil), mant...)
	out = append(out, 'E')
	if e < 0 {
		out = append(out, '-')
		e = -e
	} else {
		out = append(out, '+')
	}
	return string(strconv.AppendInt(out, int64(e), 10))
}

// editDateTime writes a date, a time or a date with time as typed entry
// reads it. Content outside what typed entry reads is written as its
// number.
func editDateTime(c Content) string {
	if c.Num < 0 || c.Num > lastDateDay+1 {
		return editNumber(c.Num)
	}
	days, secs := splitDayTime(c.Num)
	var buf [32]byte
	dst := buf[:0]
	switch c.Kind {
	case ContentTime:
		if days != 0 {
			return editNumber(c.Num)
		}
	case ContentDate:
		if secs != 0 {
			return editNumber(c.Num)
		}
	}
	if c.Kind != ContentTime {
		if days < 2 || days > lastDateDay {
			return editNumber(c.Num)
		}
		dst = appendDate(dst, days, DateISO)
	}
	if c.Kind == ContentDateTime {
		dst = append(dst, ' ')
	}
	if c.Kind != ContentDate {
		p := TimeHMS
		if secs%60 == 0 {
			p = TimeHM
		}
		dst = appendTime(dst, secs, p)
	}
	return string(dst)
}
