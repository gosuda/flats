package formula

import (
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

func TestParseTyped(t *testing.T) {
	day := float64(DateOf(2026, 3, 15))
	cases := []struct {
		typed string
		want  Content
		sets  string
	}{
		{"", Content{}, ""},
		{"=", Content{Kind: ContentText, Text: "="}, ""},
		{"=1+1", Content{Kind: ContentFormula, Text: "=1+1"}, ""},
		{"'", Content{Kind: ContentText}, ""},
		{"'=not a formula", Content{Kind: ContentText, Text: "=not a formula"}, ""},
		{"true", Content{Kind: ContentBool, Num: 1}, ""},
		{"False", Content{Kind: ContentBool}, ""},
		{"1,200.50", Content{Kind: ContentNumber, Num: 1200.5}, ""},
		{"1.5E+20", Content{Kind: ContentNumber, Num: 1.5e20}, ""},
		{"-0.5", Content{Kind: ContentNumber, Num: -0.5}, ""},
		{"12.5%", Content{Kind: ContentNumber, Num: 0.125}, `{"kind":"percent","decimals":2}`},
		{"-₩1,000", Content{Kind: ContentNumber, Num: -1000}, `{"kind":"currency","symbol":"₩","decimals":2}`},
		{"2026-03-15", Content{Kind: ContentDate, Num: day}, `{"kind":"date","pattern":"yyyy-mm-dd"}`},
		{"3/15/2026", Content{Kind: ContentDate, Num: day}, `{"kind":"date","pattern":"m/d/yyyy"}`},
		{"mar 15, 2026", Content{Kind: ContentDate, Num: day}, `{"kind":"date","pattern":"mmm d, yyyy"}`},
		{"14:30", Content{Kind: ContentTime, Num: 52200.0 / 86400}, `{"kind":"time","pattern":"hh:mm"}`},
		{"14:30:05", Content{Kind: ContentTime, Num: 52205.0 / 86400}, `{"kind":"time","pattern":"hh:mm:ss"}`},
		{"2:30 pm", Content{Kind: ContentTime, Num: 52200.0 / 86400}, `{"kind":"time","pattern":"h:mm AM/PM"}`},
		{"12:00 AM", Content{Kind: ContentTime, Num: 0}, `{"kind":"time","pattern":"h:mm AM/PM"}`},
		{"Mar 15, 2026 2:30 PM", Content{Kind: ContentDateTime, Num: day + 52200.0/86400}, `{"kind":"datetime","date":"mmm d, yyyy","time":"h:mm AM/PM"}`},
		{"2026-03-15 14:30:05", Content{Kind: ContentDateTime, Num: day + 52205.0/86400}, `{"kind":"datetime","date":"yyyy-mm-dd","time":"hh:mm:ss"}`},
	}
	for _, c := range cases {
		got, sets := ParseTyped(c.typed)
		if got != c.want {
			t.Errorf("ParseTyped(%q) = %+v, want %+v", c.typed, got, c.want)
		}
		switch {
		case c.sets == "" && sets != nil:
			t.Errorf("ParseTyped(%q) sets %+v", c.typed, *sets)
		case c.sets != "" && (sets == nil || *sets != numberFormat(t, c.sets)):
			t.Errorf("ParseTyped(%q) sets %+v, want %s", c.typed, sets, c.sets)
		}
		if sets != nil && !sets.Valid() {
			t.Errorf("ParseTyped(%q) sets the format %+v, which is not valid", c.typed, *sets)
		}
	}
	// Everything else is text, as typed.
	for _, typed := range []string{
		" 12", "12 ", "1,2", "1,2345", "12,345,6", "1.", ".5", "+5", "1e", "1E+", "5%%", "$", "$-5", "5$", "€ 5", "1 000",
		"2026-3-15", "2026-02-30", "1899-12-31", "10000-01-01", "13/1/2026", "2/30/2026", "3/15/26", "March 15, 2026",
		"Mar 15 2026", "Mar  15, 2026", "24:00", "14:60", "14:30:60", "2:30:05 PM", "0:30 AM", "13:00 PM", "2:30PM",
		"2:30 XM", "14:3", "2026-03-15T14:30", "2026-03-15  14:30", "TRUE ", "yes", "#N/A", "1/2", "٥", "１２",
		"1e400", "Rent", "a\nb", "\t5",
	} {
		got, sets := ParseTyped(typed)
		if got != (Content{Kind: ContentText, Text: typed}) || sets != nil {
			t.Errorf("ParseTyped(%q) = %+v, %v, want the text", typed, got, sets)
		}
	}
}

// A number form that is whole as typed, before any %, and that has more
// than 15 digits as typed stays text on entry, so that no digit is lost
// (rule 5, T-01). Leading zeros and zeros at the end after the decimal
// point do not count, and the sign, commas and a currency sign change
// nothing. VALUE, arithmetic and criteria still read such text as a number.
func TestLongTypedNumbersStayText(t *testing.T) {
	texts := []string{
		"1234567890123456", "-1,234,567,890,123,456", "1234567890123456.0", "1.234567890123456E15", "1234567890123456%",
		"$1234567890123456", "-$1234567890123456", "1,234,567,890,123,456.00", "1000000000000000", "100000000000000000000",
		"0.1234567890123456E16", "1234567890123450E-1", "12345678901234567890" + strings.Repeat("0", 280), strings.Repeat("9", 300),
	}
	for _, typed := range texts {
		got, sets := ParseTyped(typed)
		if got != (Content{Kind: ContentText, Text: typed}) || sets != nil {
			t.Errorf("ParseTyped(%.40q) = %+v, %v, want the text", typed, got, sets)
		}
		if edit := EditText(got, Pos{}); edit != typed {
			t.Errorf("the text %.40q is edited as %.40q", typed, edit)
		}
		if _, ok := textToNumber(typed); !ok {
			t.Errorf("%.40q is no number where a number is needed", typed)
		}
	}
	for typed, want := range map[string]float64{
		"0001234567890123":     1234567890123,
		"123456789012345":      123456789012345,
		"-123,456,789,012,345": -123456789012345,
		"1.5E+20":              1.5e20,
		"1E+20":                1e20,
		"12345678901234567.5":  12345678901234567.5,
		"1234567890123456E-1":  123456789012345.6,
		"1234567890123456.5%":  12345678901234.565,
		"1.0E+15":              1e15,
		"0.000000000000000":    0,
		"0000000000000000000":  0,
		"999999999999999.000":  999999999999999,
	} {
		if got, _ := ParseTyped(typed); got.Kind != ContentNumber || got.Num != want {
			t.Errorf("ParseTyped(%q) = %+v, want the number %v", typed, got, want)
		}
	}
	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", map[string]*testCell{
		"A1": textCell("1234567890123456"), "A2": numberCell(1234567890123456),
	})}}
	for formula, want := range map[string]Value{
		`=VALUE("1234567890123456")`:          NumberValue(1234567890123456),
		`=VALUE("-1,234,567,890,123,456")`:    NumberValue(-1234567890123456),
		`=VALUE("1234567890123456%")`:         NumberValue(12345678901234.56),
		`=VALUE("$1234567890123456")`:         NumberValue(1234567890123456),
		`=A1+0`:                               NumberValue(1234567890123456),
		`=COUNTIF(A1:A2,"1234567890123456")`:  NumberValue(2),
		`=COUNTIF(A1:A2,">1234567890123400")`: NumberValue(1),
		`=SUMIF(A1:A2,"=1234567890123456")`:   NumberValue(1234567890123456),
	} {
		if got := evalOn(t, sheet, formula); got != want {
			t.Errorf("%s = %v, want %v", formula, got, want)
		}
	}
}

// readBack is what typed entry gives for the edit text of c: c itself,
// except for a whole number of more than 15 significant digits, whose edit
// text is its plain digits, which entry keeps as text (technical design,
// section 9.5). A whole number of at most 15 significant digits is edited
// with an exponent once it needs more than 15 digits before the point.
func readBack(c Content, edit string) Content {
	if c.Kind != ContentNumber || c.Num != math.Trunc(c.Num) || math.Abs(c.Num) < 1e15 {
		return c
	}
	mant := strconv.FormatFloat(math.Abs(c.Num), 'e', -1, 64)
	mant, _, _ = strings.Cut(mant, "e")
	if len(strings.Replace(mant, ".", "", 1)) <= 15 {
		return c
	}
	return Content{Kind: ContentText, Text: edit}
}

// TestEditText checks rule 18: the formula bar shows content as it would
// be typed, so that typing it again gives the same content.
func TestEditText(t *testing.T) {
	day := float64(DateOf(2026, 3, 15))
	tenth := 0.1 // a variable, so that the sum below is rounded as a double
	cases := map[string]Content{
		"":                    {},
		"1200.5":              {Kind: ContentNumber, Num: 1200.5},
		"-0.5":                {Kind: ContentNumber, Num: -0.5},
		"0.30000000000000004": {Kind: ContentNumber, Num: tenth + 0.2},
		"123456789012345680":  {Kind: ContentNumber, Num: 123456789012345678},
		"1.5E+21":             {Kind: ContentNumber, Num: 1.5e21},
		// A whole number of at most 15 significant digits with more than
		// 15 digits before the point has an exponent, and one of more
		// significant digits its plain digits below 10^21 and an exponent
		// of all its digits from 10^21 on.
		"1.5E+20":                {Kind: ContentNumber, Num: 1.5e20},
		"1E+15":                  {Kind: ContentNumber, Num: 1e15},
		"-1.23456789012345E+15":  {Kind: ContentNumber, Num: -1234567890123450},
		"999999999999999":        {Kind: ContentNumber, Num: 999999999999999},
		"12345678901234568":      {Kind: ContentNumber, Num: 12345678901234568},
		"1.2345678901234568E+22": {Kind: ContentNumber, Num: 1.2345678901234568e22},
		"1234567890123456.5":     {Kind: ContentNumber, Num: 1234567890123456.5},
		"1E-10":                  {Kind: ContentNumber, Num: 1e-10},
		"0.000001":               {Kind: ContentNumber, Num: 1e-6},
		"Rent":                   {Kind: ContentText, Text: "Rent"},
		"'007":                   {Kind: ContentText, Text: "007"},
		"'=not a formula":        {Kind: ContentText, Text: "=not a formula"},
		"''tis":                  {Kind: ContentText, Text: "'tis"},
		"'TRUE":                  {Kind: ContentText, Text: "TRUE"},
		"'2026-03-15":            {Kind: ContentText, Text: "2026-03-15"},
		"'50%":                   {Kind: ContentText, Text: "50%"},
		"1234567890123456":       {Kind: ContentText, Text: "1234567890123456"},
		"'123456789012345":       {Kind: ContentText, Text: "123456789012345"},
		"'":                      {Kind: ContentText},
		"=":                      {Kind: ContentText, Text: "="},
		"TRUE":                   {Kind: ContentBool, Num: 1},
		"FALSE":                  {Kind: ContentBool},
		"2026-03-15":             {Kind: ContentDate, Num: day},
		"14:30":                  {Kind: ContentTime, Num: 52200.0 / 86400},
		"14:30:05":               {Kind: ContentTime, Num: 52205.0 / 86400},
		"00:00":                  {Kind: ContentTime},
		"2026-03-15 14:30":       {Kind: ContentDateTime, Num: day + 52200.0/86400},
		"2026-03-15 00:00:01":    {Kind: ContentDateTime, Num: day + 1.0/86400},
		// Content that typed entry does not give is written as its number.
		"46096.5": {Kind: ContentDate, Num: day + 0.5},
		"1.5":     {Kind: ContentTime, Num: 1.5},
		"-1":      {Kind: ContentDate, Num: -1},
		"1":       {Kind: ContentDateTime, Num: 1},
	}
	for want, c := range cases {
		got := EditText(c, Pos{})
		if got != want {
			t.Errorf("EditText(%+v) = %q, want %q", c, got, want)
		}
		back, _ := ParseTyped(got)
		if c.Kind <= ContentDateTime && c.Kind >= ContentDate && back.Kind == ContentNumber {
			back.Kind = c.Kind // such content has no typed form of its own kind
		}
		if back != readBack(c, got) {
			t.Errorf("EditText(%+v) = %q, which reads back as %+v", c, got, back)
		}
	}

	at := Pos{Row: 4, Col: 4}
	f, _ := Parse("=SUM(B$2:B4)*'Q1 costs'!C5", at, Limits{})
	if got := EditText(Content{Kind: ContentFormula, Text: f.Stored()}, at); got != "=SUM(B$2:B4)*'Q1 costs'!C5" {
		t.Errorf("EditText of a formula = %q", got)
	}
	if got := EditText(Content{Kind: ContentFormula, Text: f.Stored()}, Pos{Row: 5, Col: 5}); got != "=SUM(C$2:C5)*'Q1 costs'!D6" {
		t.Errorf("EditText of a formula one cell further = %q", got)
	}
	if got := EditText(Content{Kind: ContentUnreadable, Text: `="abc`}, at); got != `="abc` {
		t.Errorf("EditText of an unreadable formula = %q", got)
	}

	// Numbers and texts of every shape read back as themselves.
	rng := rand.New(rand.NewSource(3))
	for _, x := range sampleNumbers(rng, 20000) {
		c := Content{Kind: ContentNumber, Num: x}
		if x == 0 {
			c.Num = 0
		}
		edit := EditText(c, Pos{})
		if back, _ := ParseTyped(edit); back != readBack(c, edit) {
			t.Fatalf("the number %v is shown as %q and reads back as %+v", x, edit, back)
		}
	}
	texts := []string{"1", "-1", "1e5", "1,000", "$5", "5%", "3/15/2026", "Mar 15, 2026", "2:30 PM", "true", "=1", "'", "''", "x", " 1", "1 ", "-", "2026-03-15 14:30",
		"1234567890123456", "1234567890123456%", "$1234567890123456", "1.234567890123456E15", "123456789012345", "12345678901234567.5"}
	for _, s := range texts {
		c := Content{Kind: ContentText, Text: s}
		if back, _ := ParseTyped(EditText(c, Pos{})); back != c {
			t.Errorf("the text %q is shown as %q and reads back as %+v", s, EditText(c, Pos{}), back)
		}
	}
	for s := 0; s < daySeconds; s += 61 {
		c := Content{Kind: ContentDateTime, Num: float64(46000+s) + float64(s)/daySeconds}
		if back, _ := ParseTyped(EditText(c, Pos{})); back != c {
			t.Fatalf("the date with time %v is shown as %q and reads back as %+v", c.Num, EditText(c, Pos{}), back)
		}
	}
	if strings.Contains(EditText(Content{Kind: ContentNumber, Num: math.MaxFloat64}, Pos{}), "e") {
		t.Error("EditText writes a small e, which is fine to read but not the form General shows")
	}
}

// TestEditTextOfLongNumbers covers T-04 of the technical design, section
// 22.2: the edit text of a number typed as 1.5E+20 has an exponent and
// reads back as the number. A number typed as 12345678901234567.5 is the
// whole number 12345678901234568, and it and the JSON number
// 1234567890123456 of an agent's write cells are edited as their plain
// digits, which entry keeps as text (rule 5). Such a whole number is
// written with its plain digits below 10^21 and from 10^21 on with an
// exponent of all its significant digits (section 9.5), and entry keeps
// either as text.
func TestEditTextOfLongNumbers(t *testing.T) {
	typed := func(s string) Content {
		t.Helper()
		c, sets := ParseTyped(s)
		if c.Kind != ContentNumber || sets != nil {
			t.Fatalf("ParseTyped(%q) = %+v, want a number", s, c)
		}
		return c
	}
	for _, c := range []struct {
		name    string
		content Content
		edit    string
		back    Content
	}{
		{"typed as 1.5E+20", typed("1.5E+20"), "1.5E+20", Content{Kind: ContentNumber, Num: 1.5e20}},
		{"typed as 12345678901234567.5", typed("12345678901234567.5"), "12345678901234568", Content{Kind: ContentText, Text: "12345678901234568"}},
		{"the JSON number 1234567890123456", Content{Kind: ContentNumber, Num: 1234567890123456}, "1234567890123456", Content{Kind: ContentText, Text: "1234567890123456"}},
		{"the largest number below 10^21", Content{Kind: ContentNumber, Num: math.Nextafter(1e21, 0)}, "999999999999999900000", Content{Kind: ContentText, Text: "999999999999999900000"}},
		{"10^21", Content{Kind: ContentNumber, Num: 1e21}, "1E+21", Content{Kind: ContentNumber, Num: 1e21}},
		{"the smallest number above 10^21", Content{Kind: ContentNumber, Num: math.Nextafter(1e21, math.Inf(1))}, "1.0000000000000001E+21", Content{Kind: ContentText, Text: "1.0000000000000001E+21"}},
		{"the JSON number -12345678901234567890123", Content{Kind: ContentNumber, Num: -12345678901234567890123}, "-1.2345678901234568E+22", Content{Kind: ContentText, Text: "-1.2345678901234568E+22"}},
	} {
		edit := EditText(c.content, Pos{})
		if edit != c.edit {
			t.Errorf("%s: the edit text is %q, want %q", c.name, edit, c.edit)
			continue
		}
		if back, sets := ParseTyped(edit); back != c.back || sets != nil {
			t.Errorf("%s: %q committed as typed is %+v with the format %v, want %+v", c.name, edit, back, sets, c.back)
		}
	}
}

// readValueFullScan is readValue with a search for the date of a date with
// time at every space of the text, however far in.
func readValueFullScan(s string) (typedValue, bool) {
	if v, ok := readNumber(s); ok {
		return v, true
	}
	if serial, p, ok := readDate(s); ok {
		return typedValue{kind: typedDate, num: float64(serial), date: p}, true
	}
	if secs, p, ok := readTime(s); ok {
		return typedValue{kind: typedTime, num: float64(secs) / daySeconds, time: p}, true
	}
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			continue
		}
		if serial, dp, ok := readDate(s[:i]); ok {
			if secs, tp, ok := readTime(s[i+1:]); ok {
				return typedValue{kind: typedDateTime, num: float64(serial) + float64(secs)/daySeconds, date: dp, time: tp}, true
			}
		}
	}
	return typedValue{}, false
}

// A date with a time is looked for only as far into a text as a date is
// long. That reads every text as a search at every space would.
func TestDateWithTimeIsReadWithinTheLengthOfADate(t *testing.T) {
	rng := rand.New(rand.NewSource(20261115))
	dates := []string{
		"2026-03-15", "1900-01-01", "9999-12-31", "2026-02-30", "3/15/2026", "12/31/2026", "1/1/2026", "13/1/2026",
		"Mar 15, 2026", "mar 5, 2026", "Dec 31, 9999", "Feb 30, 2026", "Mar 15,2026", "Mars 15, 2026", "Mar  5, 2026",
		"2026-03-1", "20260315", "a", "a a", "Mar", "",
	}
	times := []string{"14:30", "14:30:05", "2:30 PM", "12:00 am", "24:00", "2:30", "2:30:05 PM", "x", "", "1 2"}
	junk := []string{"", " ", "  ", "x", "x ", " x", strings.Repeat("a ", 20), strings.Repeat(" ", 15), "0 ", "12 "}
	check := func(s string) {
		got, ok := readValue(s)
		want, wantOK := readValueFullScan(s)
		if got != want || ok != wantOK {
			t.Fatalf("readValue(%q) = %+v, %v; a search at every space gives %+v, %v", s, got, ok, want, wantOK)
		}
	}
	pick := func(list []string) string { return list[rng.Intn(len(list))] }
	read := 0
	for _, d := range dates {
		if len(d) > maxDateLen {
			if _, _, ok := readDate(d); ok {
				t.Errorf("readDate reads %q, which is longer than maxDateLen", d)
			}
		}
		for _, tm := range times {
			for _, sep := range []string{" ", "  ", "", "T"} {
				for _, before := range junk {
					for _, after := range []string{"", " ", " x"} {
						s := before + d + sep + tm + after
						check(s)
						if v, ok := readValue(s); ok && v.kind == typedDateTime {
							read++
						}
					}
				}
			}
		}
	}
	if read < 20 {
		t.Errorf("only %d of the texts were read as a date with time", read)
	}
	for i := 0; i < 200000; i++ {
		var b strings.Builder
		for k := rng.Intn(5); k > 0; k-- {
			b.WriteString(pick([][]string{dates, times, junk}[rng.Intn(3)]))
			if rng.Intn(2) == 0 {
				b.WriteString(" ")
			}
		}
		check(b.String())
	}
	// Every date that is read is no longer than maxDateLen.
	for y := int64(1900); y <= 9999; y += 97 {
		for m := int64(1); m <= 12; m++ {
			for _, p := range []DatePattern{DateISO, DateMDY, DateMonth} {
				s := string(appendDate(nil, serialOf(y, m, 28), p))
				if _, _, ok := readDate(s); !ok || len(s) > maxDateLen {
					t.Fatalf("the date %q of %d bytes: read %v, maxDateLen is %d", s, len(s), ok, maxDateLen)
				}
			}
		}
	}
}

// A number without a currency sign, commas or a percent sign is read from
// the text as it stands. With a currency sign it is copied first. Both
// give the same number, also for digits far past what a number holds.
func TestNumbersAreReadInPlace(t *testing.T) {
	rng := rand.New(rand.NewSource(20261116))
	digits := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('0' + rng.Intn(10))
		}
		return string(b)
	}
	texts := []string{"0", "007", "1.5", "0.1", "1e5", "1E5", "1e+05", "1E-5", "2.5e-3", "1e308", "1e-320", "4.9e-324", "1e-400", "0.0000001E5",
		strings.Repeat("0", 5000), strings.Repeat("9", 300), strings.Repeat("9", 5000), "0." + strings.Repeat("3", 5000),
		strings.Repeat("0", 400) + "1." + strings.Repeat("0", 400) + "1", "1e309", "9" + strings.Repeat("0", 400)}
	for i := 0; i < 20000; i++ {
		s := digits(1 + rng.Intn(40))
		if rng.Intn(2) == 0 {
			s += "." + digits(1+rng.Intn(40))
		}
		if rng.Intn(3) == 0 {
			s += []string{"e", "E", "e+", "E-", "e-"}[rng.Intn(5)] + digits(1+rng.Intn(3))
		}
		texts = append(texts, s)
	}
	for _, s := range texts {
		for _, sign := range []string{"", "-"} {
			plain, ok := readNumber(sign + s)
			copied, copiedOK := readNumber(sign + "$" + s)
			if ok != copiedOK || (ok && (plain.kind != typedNumber || math.Float64bits(plain.num) != math.Float64bits(copied.num))) {
				t.Fatalf("readNumber(%.60q) = %v, %v; with a currency sign %v, %v", sign+s, plain.num, ok, copied.num, copiedOK)
			}
		}
	}
	// A number too large for a number is refused by strconv with an
	// error that holds the text, so only numbers in range are held to this.
	for _, long := range []string{strings.Repeat("0", MaxTextLen), "0." + strings.Repeat("123456789", MaxTextLen/9-1), strings.Repeat("9", 300)} {
		if n := testing.AllocsPerRun(10, func() { readNumber(long) }); n != 0 {
			t.Errorf("reading a number of %d digits allocates %v times", len(long), n)
		}
	}
}
