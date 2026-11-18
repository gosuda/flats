package formula

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// NumberKind is the kind of a number format (spec rule 6).
type NumberKind uint8

const (
	NumberGeneral  NumberKind = iota // also a cell without a number format
	NumberFixed                      // "number": decimals and a thousands separator
	NumberPercent                    // "percent"
	NumberCurrency                   // "currency"
	NumberDate                       // "date"
	NumberTime                       // "time"
	NumberDateTime                   // "datetime"
	NumberText                       // "text"
)

// DatePattern is one of the date forms of rule 6.
type DatePattern uint8

const (
	DateISO   DatePattern = iota // yyyy-mm-dd
	DateMDY                      // m/d/yyyy
	DateMonth                    // mmm d, yyyy
)

// TimePattern is one of the time forms of rule 6.
type TimePattern uint8

const (
	TimeHM   TimePattern = iota // hh:mm
	TimeHMS                     // hh:mm:ss
	TimeAMPM                    // h:mm AM/PM
)

// NumberFormat is the number format of a cell. The zero value is General,
// which is also what a cell without a number format has. NumberFormat
// values compare with ==.
//
// Decimals (0 to 10) counts for the number, percent and currency kinds,
// Thousands for the number kind, Symbol (1 to 3 characters, each a letter
// or a currency sign) for the currency kind, Date for the date and datetime
// kinds and Time for the time and datetime kinds. A currency format always
// separates thousands. Fields a kind does not use are zero.
//
// As JSON a NumberFormat is the object of the reference table's README,
// such as {"kind":"currency","symbol":"$","decimals":2}.
type NumberFormat struct {
	Kind      NumberKind
	Decimals  uint8
	Thousands bool
	Symbol    string
	Date      DatePattern
	Time      TimePattern
}

// Valid reports whether nf is a format rule 6 allows, with the fields its
// kind does not use zero.
func (nf NumberFormat) Valid() bool {
	want := NumberFormat{Kind: nf.Kind}
	switch nf.Kind {
	case NumberGeneral, NumberText:
	case NumberFixed:
		want.Decimals, want.Thousands = nf.Decimals, nf.Thousands
	case NumberPercent:
		want.Decimals = nf.Decimals
	case NumberCurrency:
		want.Decimals, want.Symbol = nf.Decimals, nf.Symbol
		if !validSymbol(nf.Symbol) {
			return false
		}
	case NumberDate:
		want.Date = nf.Date
	case NumberTime:
		want.Time = nf.Time
	case NumberDateTime:
		want.Date, want.Time = nf.Date, nf.Time
	default:
		return false
	}
	return nf == want && nf.Decimals <= 10 && nf.Date <= DateMonth && nf.Time <= TimeAMPM
}

// validSymbol reports whether s is a currency symbol: 1 to 3 characters,
// each a letter or a currency sign.
func validSymbol(s string) bool {
	n := 0
	for _, r := range s {
		if !isSymbolRune(r) {
			return false
		}
		n++
	}
	return n >= 1 && n <= 3
}

var numberKindText = [...]string{
	NumberGeneral: "general", NumberFixed: "number", NumberPercent: "percent", NumberCurrency: "currency",
	NumberDate: "date", NumberTime: "time", NumberDateTime: "datetime", NumberText: "text",
}

// numberFormatJSON is the object of the reference table's README. The
// fields are in the order the README writes them for each kind.
type numberFormatJSON struct {
	Kind      string  `json:"kind"`
	Symbol    *string `json:"symbol,omitempty"`
	Decimals  *uint8  `json:"decimals,omitempty"`
	Thousands *bool   `json:"thousands,omitempty"`
	Pattern   *string `json:"pattern,omitempty"`
	Date      *string `json:"date,omitempty"`
	Time      *string `json:"time,omitempty"`
}

// MarshalJSON writes nf as the object of the reference table's README.
func (nf NumberFormat) MarshalJSON() ([]byte, error) {
	if !nf.Valid() {
		return nil, errNumberFormat
	}
	out := numberFormatJSON{Kind: numberKindText[nf.Kind]}
	date, clock := nf.Date.String(), nf.Time.String()
	switch nf.Kind {
	case NumberFixed:
		out.Decimals, out.Thousands = &nf.Decimals, &nf.Thousands
	case NumberPercent:
		out.Decimals = &nf.Decimals
	case NumberCurrency:
		out.Symbol, out.Decimals = &nf.Symbol, &nf.Decimals
	case NumberDate:
		out.Pattern = &date
	case NumberTime:
		out.Pattern = &clock
	case NumberDateTime:
		out.Date, out.Time = &date, &clock
	}
	return json.Marshal(out)
}

var errNumberFormat = errors.New("formula: not a number format")

// UnmarshalJSON reads the object MarshalJSON writes. It refuses unknown
// kinds, unknown and missing fields and values outside rule 6.
func (nf *NumberFormat) UnmarshalJSON(data []byte) error {
	var in numberFormatJSON
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return errNumberFormat
	}
	out := NumberFormat{Kind: NumberKind(len(numberKindText))}
	for k, name := range numberKindText {
		if in.Kind == name {
			out.Kind = NumberKind(k)
		}
	}
	// Each kind takes exactly its own fields.
	var want numberFormatJSON
	ok := true
	switch out.Kind {
	case NumberFixed:
		want.Decimals, want.Thousands = in.Decimals, in.Thousands
		ok = in.Decimals != nil && in.Thousands != nil
		if ok {
			out.Decimals, out.Thousands = *in.Decimals, *in.Thousands
		}
	case NumberPercent:
		want.Decimals = in.Decimals
		if ok = in.Decimals != nil; ok {
			out.Decimals = *in.Decimals
		}
	case NumberCurrency:
		want.Symbol, want.Decimals = in.Symbol, in.Decimals
		if ok = in.Symbol != nil && in.Decimals != nil; ok {
			out.Symbol, out.Decimals = *in.Symbol, *in.Decimals
		}
	case NumberDate:
		want.Pattern = in.Pattern
		if ok = in.Pattern != nil; ok {
			out.Date, ok = parseDatePattern(*in.Pattern)
		}
	case NumberTime:
		want.Pattern = in.Pattern
		if ok = in.Pattern != nil; ok {
			out.Time, ok = parseTimePattern(*in.Pattern)
		}
	case NumberDateTime:
		want.Date, want.Time = in.Date, in.Time
		if ok = in.Date != nil && in.Time != nil; ok {
			var okDate, okTime bool
			out.Date, okDate = parseDatePattern(*in.Date)
			out.Time, okTime = parseTimePattern(*in.Time)
			ok = okDate && okTime
		}
	}
	want.Kind = in.Kind
	if !ok || want != in || !out.Valid() {
		return errNumberFormat
	}
	*nf = out
	return nil
}

// Align is the horizontal alignment of a cell.
type Align uint8

const (
	AlignGeneral Align = iota // text left, numbers and dates right, booleans and errors centered
	AlignLeft
	AlignCenter
	AlignRight
)

// Color is none or one of the 54 palette colors of rule 6: nine hues in six
// shades. The zero value is none.
type Color uint8

// PaletteColors is the number of palette colors. Colors run from 1 to
// PaletteColors.
const PaletteColors = 54

var (
	colorHues   = [...]string{"gray", "red", "orange", "yellow", "green", "teal", "blue", "purple", "magenta"}
	colorShades = [...]string{"100", "200", "300", "400", "600", "800"}
)

// ParseColor reads a palette name such as "red-600".
func ParseColor(name string) (Color, bool) {
	hue, shade, ok := strings.Cut(name, "-")
	if !ok {
		return 0, false
	}
	for h, hn := range colorHues {
		if hn != hue {
			continue
		}
		for s, sn := range colorShades {
			if sn == shade {
				return Color(h*len(colorShades) + s + 1), true
			}
		}
	}
	return 0, false
}

// String returns the palette name of c, and "" for none or for a number
// that is no palette color.
func (c Color) String() string {
	if c < 1 || c > PaletteColors {
		return ""
	}
	i := int(c) - 1
	return colorHues[i/len(colorShades)] + "-" + colorShades[i%len(colorShades)]
}

// Format is the format of a cell (rule 6): four marks, two colors,
// alignment, wrap and the number format. The zero value is the default
// format. Format values compare with ==.
type Format struct {
	Bold, Italic, Underline, Strikethrough bool
	TextColor, Fill                        Color
	Align                                  Align
	Wrap                                   bool
	Number                                 NumberFormat
}

// Show returns what a cell shows for the value v (rule 6). kind is the kind
// of the cell's content and nf its number format.
//
// A blank value shows nothing, text shows as it is, a boolean as TRUE or
// FALSE and an error as its name. A number shows under nf. Under General a
// number shows as a date, a time or a date with time when kind is
// ContentDate, ContentTime or ContentDateTime. For a formula cell pass
// ContentDate when Formula.DateResult is true and ContentFormula otherwise.
// A date or time format on a negative number shows the number in General.
// Show does not know column widths.
func Show(v Value, kind ContentKind, nf NumberFormat) string {
	if v.kind == KindText {
		return v.str
	}
	var buf [48]byte
	return string(AppendShow(buf[:0], v, kind, nf))
}

// AppendShow appends what Show returns to dst.
func AppendShow(dst []byte, v Value, kind ContentKind, nf NumberFormat) []byte {
	switch v.kind {
	case KindText:
		return append(dst, v.str...)
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
				nf = NumberFormat{Kind: NumberTime, Time: TimeHMS}
			case ContentDateTime:
				nf = NumberFormat{Kind: NumberDateTime, Time: TimeHMS}
			}
		}
		return appendNumber(dst, v.num, nf, true)
	}
	return dst
}

// appendNumber appends a number as nf shows it. currencyThousands tells
// whether a currency amount separates thousands: always in a cell, and as
// the pattern says in TEXT.
func appendNumber(dst []byte, x float64, nf NumberFormat, currencyThousands bool) []byte {
	switch nf.Kind {
	case NumberFixed:
		return appendFixed(dst, x, int(nf.Decimals), nf.Thousands, 0, "")
	case NumberPercent:
		return append(appendFixed(dst, x, int(nf.Decimals), false, 2, ""), '%')
	case NumberCurrency:
		return appendFixed(dst, x, int(nf.Decimals), currencyThousands, 0, nf.Symbol)
	case NumberDate, NumberTime, NumberDateTime:
		// A date or time format on a negative number, or on one past the
		// year 9999, shows the number in General.
		if x < 0 || x >= lastDateDay+1 {
			return appendGeneral(dst, x)
		}
		days, secs := splitDayTime(x)
		if nf.Kind != NumberTime {
			dst = appendDate(dst, days, nf.Date)
		}
		if nf.Kind == NumberDateTime {
			dst = append(dst, ' ')
		}
		if nf.Kind != NumberDate {
			dst = appendTime(dst, secs, nf.Time)
		}
		return dst
	}
	return appendGeneral(dst, x)
}
