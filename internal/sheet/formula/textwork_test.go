package formula

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// each returns a formula that adds the piece once for each of the numbers 1
// to n. "#" in the piece stands for the number, so "LEN(A#)" names the
// cells A1 to An.
func each(n int, piece string) string {
	return "=" + list(n, piece, "+")
}

// list writes the piece for the numbers 1 to n with sep between.
func list(n int, piece, sep string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteString(sep)
		}
		b.WriteString(strings.ReplaceAll(piece, "#", strconv.Itoa(i)))
	}
	return b.String()
}

// longWork is a sheet of texts of the greatest length for the work that
// functions other than the wildcard matcher do on them.
//
//	Fold   A1:A1000 hold 32,766 capital sigmas and "a", B1 holds 32,766
//	       small sigmas and "b". The two are equal without regard to
//	       capitals up to the last character. C1:C1000 hold numbers.
//	Zeros  A1:A1000 hold the text of 32,767 zeros, which reads as the
//	       number 0. B1:B1000 hold 1 and C1 holds 0.
//	Long   A1:A1000 hold 32,766 "a" and "y". B1 holds 32,766 "A" and "z",
//	       D1 two "A"s less, and E1 holds 32,766 "a" and "x". C1:C1000
//	       hold the numbers 1 to 1,000.
//	Wide   A1:ZZ1 hold the text of Long!A1, A2:ZZ2 the numbers 1 to 702,
//	       and A3 holds the text of Long!B1.
func longWork(t testing.TB) *testSheet {
	t.Helper()
	const n = MaxTextLen - 1
	fold := newTab(t, "Fold", map[string]*testCell{"B1": textCell(strings.Repeat("σ", n) + "b")})
	zeros := newTab(t, "Zeros", map[string]*testCell{"C1": numberCell(0)})
	long := newTab(t, "Long", map[string]*testCell{
		"B1": textCell(strings.Repeat("A", n) + "z"),
		"D1": textCell(strings.Repeat("A", n-2) + "z"),
		"E1": textCell(strings.Repeat("a", n) + "x"),
	})
	capitals, digits, letters := strings.Repeat("Σ", n)+"a", strings.Repeat("0", n+1), strings.Repeat("a", n)+"y"
	for r := int32(0); r < 1000; r++ {
		fold.cells[Pos{Row: r, Col: 0}] = textCell(capitals)
		fold.cells[Pos{Row: r, Col: 2}] = numberCell(float64(r + 1))
		zeros.cells[Pos{Row: r, Col: 0}] = textCell(digits)
		zeros.cells[Pos{Row: r, Col: 1}] = numberCell(1)
		long.cells[Pos{Row: r, Col: 0}] = textCell(letters)
		long.cells[Pos{Row: r, Col: 2}] = numberCell(float64(r + 1))
	}
	wide := &testTab{name: "Wide", rows: 3, cols: MaxCols, cells: map[Pos]*testCell{{Row: 2}: textCell(strings.Repeat("A", n) + "z")}}
	for c := int32(0); c < MaxCols; c++ {
		wide.cells[Pos{Row: 0, Col: c}] = textCell(letters)
		wide.cells[Pos{Row: 1, Col: c}] = numberCell(float64(c + 1))
	}
	return &testSheet{tabs: []*testTab{fold, zeros, long, wide}}
}

// Comparing texts, reading a text as a number and going through a text to
// count, cut or copy it are work like matching a pattern: every function
// and operator counts at least a unit for every 64 code points it goes
// through, the deadline is checked during the work, and nothing goes
// through another text once the deadline has passed.
//
// The least work of each formula is worked out here from the size of its
// texts and from how many of them the formula has to go through to give
// its value. Nothing of it is taken from the evaluator.
func TestTextWorkIsCountedAndMeetsTheDeadline(t *testing.T) {
	sheet := longWork(t)
	const (
		length = MaxTextLen     // the code points of a long text
		same   = MaxTextLen - 1 // the code points two long texts have in common before they differ
		probes = 9              // the cells a binary search looks at in 702 or 1,000 at the least: 2^9 is 512
	)
	num, yes := NumberValue, BoolValue(true)
	type work struct {
		tab string
		// The formula, or else a piece that is added up for the numbers 1
		// to n, as each writes it.
		formula string
		piece   string
		n       int
		want    Value
		texts   int64 // how many times a long text is gone through at the least
		points  int64 // the code points of each of those
		reads   int64 // the cells that are read besides, a unit each
		// short is set for a formula that needs less work than lies
		// between two checks of the deadline. The work is at most this
		// many units and one for each part of the formula.
		short int64
	}
	cases := []work{
		// The exact match of a lookup without wildcards compares texts.
		{tab: "Fold", formula: "=XLOOKUP(B1,A1:A1000,C1:C1000,0)", want: num(0), texts: 1000, points: same, reads: 1001},
		// A number as criteria reads every text as a number.
		{tab: "Zeros", formula: "=COUNTIF(A1:A1000,0)", want: num(1000), texts: 1000, points: length, reads: 1000},

		// Operators.
		{tab: "Long", n: 100, piece: "(A#=$B$1)", want: num(0), texts: 100, points: same},
		{tab: "Long", n: 100, piece: "(A#<>$B$1)", want: num(100), texts: 100, points: same},
		{tab: "Long", n: 100, piece: "(A#<$B$1)", want: num(100), texts: 100, points: same},
		{tab: "Long", n: 100, piece: "(A#>=$B$1)", want: num(0), texts: 100, points: same},
		{tab: "Long", n: 100, piece: `ISTEXT(A#&"")`, want: num(100), texts: 100, points: length},
		{tab: "Long", n: 100, piece: "ISERROR(A#&A#)", want: num(100), texts: 200, points: length},
		{tab: "Zeros", n: 100, piece: "(0+A#)", want: num(0), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: "-A#", want: num(0), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: "A#%", want: num(0), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: "A#*2", want: num(0), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: "2^A#", want: num(100), texts: 100, points: length},

		// Functions that take a number read a text as one.
		{tab: "Zeros", n: 100, piece: "ABS(A#)", want: num(0), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: "ROUND(A#,2)", want: num(0), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: "MOD(A#,7)", want: num(0), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: "EXP(A#)", want: num(100), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: "CEILING(7,1+A#)", want: num(700), texts: 100, points: length},
		{tab: "Zeros", formula: "=SUM(" + list(100, "IF(TRUE,A#)", ",") + ")", want: num(0), texts: 100, points: length},
		{tab: "Zeros", formula: "=PRODUCT(" + list(100, "IF(TRUE,A#)", ",") + ")", want: num(0), texts: 100, points: length},
		{tab: "Zeros", formula: "=AVERAGE(" + list(100, "IF(TRUE,A#)", ",") + ")", want: num(0), texts: 100, points: length},
		{tab: "Zeros", formula: "=MAX(" + list(100, "IF(TRUE,A#)", ",") + ")", want: num(0), texts: 100, points: length},
		{tab: "Zeros", formula: "=MEDIAN(" + list(100, "IF(TRUE,A#)", ",") + ")", want: num(0), texts: 100, points: length},
		{tab: "Zeros", formula: "=STDEV(" + list(100, "IF(TRUE,A#)", ",") + ")", want: num(0), texts: 100, points: length},
		{tab: "Zeros", formula: "=COUNT(" + list(100, "IF(TRUE,A#)", ",") + ")", want: num(100), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: "RANK(A#,$C$1)", want: num(100), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: "CHOOSE(1+A#,7)", want: num(700), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: "INDEX($B$1:$B$9,1+A#)", want: num(100), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: "VALUE(A#)", want: num(0), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: `ISTEXT(TEXT(A#,"0.00"))`, want: num(100), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: "YEAR(A#)", want: num(189900), texts: 100, points: length}, // day 0 is 1899-12-30
		{tab: "Zeros", n: 50, piece: "DAYS(A#,A#)", want: num(0), texts: 100, points: length},
		{tab: "Zeros", n: 100, piece: "DATE(A#,1,1)", want: num(200), texts: 100, points: length}, // day 2 is 1900-01-01
		{tab: "Zeros", n: 100, piece: "EDATE(A#,1)", want: num(3100), texts: 100, points: length}, // 1900-01-30 is day 31
		{tab: "Zeros", n: 100, piece: "HOUR(A#)", want: num(0), texts: 100, points: length},
		{tab: "Zeros", n: 30, piece: "TIME(A#,A#,A#)", want: num(0), texts: 90, points: length},
		{tab: "Zeros", n: 100, piece: "WEEKDAY(A#)", want: num(700), texts: 100, points: length}, // 1899-12-30 is a Saturday

		// Conditional totals with a number as criteria against texts.
		{tab: "Zeros", formula: `=COUNTIF(A1:A1000,"<>0")`, want: num(0), texts: 1000, points: length, reads: 1000},
		{tab: "Zeros", formula: "=COUNTIFS(B1:B1000,1,A1:A1000,0)", want: num(1000), texts: 1000, points: length, reads: 2000},
		{tab: "Zeros", formula: "=SUMIF(A1:A1000,0,B1:B1000)", want: num(1000), texts: 1000, points: length, reads: 2000},
		{tab: "Zeros", formula: "=SUMIFS(B1:B1000,A1:A1000,0)", want: num(1000), texts: 1000, points: length, reads: 2000},
		{tab: "Zeros", formula: "=AVERAGEIF(A1:A1000,0,B1:B1000)", want: num(1), texts: 1000, points: length, reads: 2000},
		{tab: "Zeros", formula: "=AVERAGEIFS(B1:B1000,A1:A1000,0)", want: num(1), texts: 1000, points: length, reads: 2000},
		{tab: "Zeros", formula: "=MAXIFS(B1:B1000,A1:A1000,0)", want: num(1), texts: 1000, points: length, reads: 2000},
		{tab: "Zeros", formula: `=MINIFS(B1:B1000,A1:A1000,"=0")`, want: num(1), texts: 1000, points: length, reads: 2000},
		// Criteria that are a long text are read as a number first.
		{tab: "Zeros", n: 100, piece: "COUNTIF($C$1,A#)", want: num(100), texts: 100, points: length, reads: 200},
		{tab: "Zeros", n: 100, piece: "SUMIFS($C$1,$C$1,A#)", want: num(0), texts: 100, points: length, reads: 200},
		{tab: "Zeros", n: 100, piece: "COUNTIFS($C$1:$C$2,A#,$C$1,0)", want: ErrorValue(ErrValue), texts: 1, points: length, short: 4 * length / 64},
		// Criteria that order texts. D1 is two characters shorter, so that
		// a sign fits before it.
		{tab: "Long", formula: `=COUNTIF(A1:A1000,"<"&D1)`, want: num(1000), texts: 1000, points: same - 2, reads: 1000},
		{tab: "Long", formula: `=SUMIFS(C1:C1000,A1:A1000,">="&D1)`, want: num(0), texts: 1000, points: same - 2, reads: 1000},
		{tab: "Long", formula: `=MAXIFS(C1:C1000,A1:A1000,"<="&D1)`, want: num(1000), texts: 1000, points: same - 2, reads: 2000},
		// Ranges beside the tab have no cell to read. Their criteria are
		// still long texts, each read as a number and then as a pattern.
		{tab: "Long", formula: "=COUNTIFS(" + list(60, "$AA$1:$AA$9,A#", ",") + ")", want: num(0), texts: 120, points: length, reads: 60},

		// Lookups.
		{tab: "Long", formula: "=XLOOKUP(B1,A1:A1000,C1:C1000,0)", want: num(0), texts: 1000, points: same, reads: 1001},
		{tab: "Long", formula: "=ISNA(MATCH(B1,A1:A1000,0))", want: yes, texts: 1000, points: same, reads: 1001},
		{tab: "Long", formula: "=ISNA(VLOOKUP(B1,A1:C1000,3,FALSE))", want: yes, texts: 1000, points: same, reads: 1001},
		{tab: "Wide", formula: "=ISNA(HLOOKUP(A3,A1:ZZ2,2,FALSE))", want: yes, texts: MaxCols, points: same, reads: MaxCols + 1},
		// A key that is a long text is read as a pattern first.
		{tab: "Long", n: 100, piece: "ISNA(MATCH(A#,$C$1:$C$9,0))", want: num(100), texts: 100, points: length, reads: 900},
		{tab: "Long", n: 100, piece: "ISNA(VLOOKUP(A#,$C$1:$D$9,2,FALSE))", want: num(100), texts: 100, points: length, reads: 900},
		// A sorted match is a binary search. The key lies after every
		// text, or before every text of data that falls.
		{tab: "Long", n: 30, piece: "MATCH($B$1,$A$1:$A$1000)", want: num(30000), texts: 30 * probes, points: same},
		{tab: "Long", n: 30, piece: "ISNA(MATCH($B$1,$A$1:$A$1000,-1))", want: num(30), texts: 30 * probes, points: same},
		{tab: "Long", n: 30, piece: "VLOOKUP($B$1,$A$1:$C$1000,3)", want: num(30000), texts: 30 * probes, points: same},
		{tab: "Wide", n: 30, piece: "HLOOKUP($A$3,$A$1:$ZZ$2,2)", want: num(30 * MaxCols), texts: 30 * probes, points: same},
		{tab: "Long", formula: "=SWITCH($B$1," + list(100, "A#,#", ",") + ",0)", want: num(0), texts: 100, points: same},

		// Text functions. ISTEXT and ISERROR look at no character.
		{tab: "Long", n: 100, piece: "LEN(A#)", want: num(100 * length), texts: 100, points: length},
		{tab: "Long", n: 100, piece: "ISTEXT(LEFT(A#,32766))", want: num(100), texts: 100, points: same},
		{tab: "Long", n: 100, piece: "ISTEXT(RIGHT(A#))", want: num(100), texts: 100, points: length},
		{tab: "Long", n: 100, piece: "ISTEXT(RIGHT(A#,32766))", want: num(100), texts: 100, points: length},
		{tab: "Long", n: 100, piece: "ISTEXT(MID(A#,32767,1))", want: num(100), texts: 100, points: same},
		{tab: "Long", n: 100, piece: "ISTEXT(MID(A#,2,32766))", want: num(100), texts: 100, points: same},
		{tab: "Long", n: 100, piece: `ISTEXT(REPLACE(A#,1,32767,""))`, want: num(100), texts: 100, points: length},
		{tab: "Long", n: 100, piece: `ISERROR(REPLACE(A#,32767,1,"xx"))`, want: num(100), texts: 100, points: same},
		{tab: "Long", n: 100, piece: "ISERROR(REPT(A#,2))", want: num(100), texts: 100, points: length},
		{tab: "Long", n: 100, piece: "EXACT(A#,$E$1)", want: num(0), texts: 100, points: length},
		{tab: "Long", n: 100, piece: "ISTEXT(TRIM(A#))", want: num(100), texts: 100, points: length},
		{tab: "Long", n: 100, piece: "ISTEXT(UPPER(A#))", want: num(100), texts: 100, points: length},
		{tab: "Long", n: 100, piece: "ISTEXT(LOWER(A#))", want: num(100), texts: 100, points: length},
		{tab: "Long", n: 100, piece: "ISTEXT(PROPER(A#))", want: num(100), texts: 100, points: length},
		{tab: "Long", n: 100, piece: `FIND("y",A#)`, want: num(100 * length), texts: 100, points: length},
		{tab: "Long", n: 100, piece: `FIND("y",A#,32000)`, want: num(100 * length), texts: 100, points: length},
		{tab: "Long", n: 100, piece: `ISERROR(FIND("yy",A#))`, want: num(100), texts: 100, points: length},
		{tab: "Long", n: 100, piece: `SEARCH("y",A#)`, want: num(100 * length), texts: 100, points: length},
		{tab: "Long", n: 100, piece: `ISTEXT(SUBSTITUTE(A#,"y","z"))`, want: num(100), texts: 100, points: length},
		{tab: "Long", n: 100, piece: `ISTEXT(SUBSTITUTE(A#,"y","z",1))`, want: num(100), texts: 100, points: length},
		{tab: "Long", n: 100, piece: `ISTEXT(SUBSTITUTE(A#,"yy","z"))`, want: num(100), texts: 100, points: length},
		// A text longer than the one it is looked for in is not gone through.
		{tab: "Long", n: 100, piece: `ISERROR(FIND(A#,"x"))`, want: num(100), reads: 100, short: 300},
		{tab: "Long", n: 100, piece: `ISTEXT(SUBSTITUTE("x",A#,"y"))`, want: num(100), reads: 100, short: 300},
		{tab: "Long", n: 100, piece: "ISTEXT(CONCAT(A#))", want: num(100), texts: 100, points: length},
		// The second text makes the result too long. Both are counted.
		{tab: "Long", n: 100, piece: "ISERROR(CONCAT(A#,A#))", want: num(100), texts: 200, points: length},
		{tab: "Long", n: 100, piece: "ISERROR(CONCATENATE(A#,A#))", want: num(100), texts: 200, points: length},
		{tab: "Long", n: 100, piece: `ISERROR(TEXTJOIN("",TRUE,A#,A#))`, want: num(100), texts: 200, points: length},
		{tab: "Long", n: 100, piece: "ISERROR(TEXTJOIN(A#,FALSE,1,2))", want: num(100), texts: 100, points: length},
		// Once the result is too long, TEXTJOIN looks only for an error
		// in the cells that follow. It goes through none of their texts.
		{tab: "Long", formula: `=ISERROR(TEXTJOIN("",TRUE,A1:A1000))`, want: yes, texts: 2, points: length, reads: 1000, short: 1000 + 4*length/64},
		{tab: "Long", formula: `=ISERROR(TEXTJOIN("",TRUE,` + list(1000, "A#", ",") + "))", want: yes, texts: 2, points: length, reads: 1000, short: 1000 + 4*length/64},
	}
	env := func(c work, b *Budget) *Env {
		tab, ok := sheet.Tab(c.tab)
		if !ok {
			t.Fatalf("no tab %s", c.tab)
		}
		return &Env{Grid: sheet, Tab: tab, At: pos(t, "Z9"), Budget: b}
	}
	// stops calculates the formula with a deadline that passes at its
	// first check and at its third, and wants it to stop there.
	stops := func(c work, name string, f *Formula) {
		for _, at := range []int{1, 3} {
			b, checks := expiring(at)
			f.Eval(env(c, b))
			if *checks != at || !b.Exceeded() || b.Used() > int64(at+3)*budgetStep {
				t.Errorf("%s: %d checks of the deadline, want %d, %d units of work, exceeded %v", name, *checks, at, b.Used(), b.Exceeded())
			}
		}
	}
	for _, c := range cases {
		formula := c.formula
		if c.piece != "" {
			formula = each(c.n, c.piece)
		}
		f := mustParse(t, formula, "Z9")
		name := c.tab + ": " + formula
		if len(name) > 70 {
			name = name[:70] + "..."
		}
		least := c.reads + c.texts*c.points/64
		b, checks := expiring(1 << 30)
		if got := f.Eval(env(c, b)); got != c.want {
			t.Errorf("%s = %v, want %v", name, got, c.want)
		}
		// Work is added to the budget up to 64 units at a time, so two
		// checks of the deadline lie at most that much more than a step
		// apart.
		if used := b.Used(); used < least || int64(*checks) < used/(budgetStep+64) {
			t.Errorf("%s counts %d units of work and checks the deadline %d times, for at least %d units", name, used, *checks, least)
		}
		if c.short > 0 {
			if used, most := b.Used(), c.short+int64(countParts(f.root)); used > most {
				t.Errorf("%s counts %d units of work, more than %d", name, used, most)
			}
			continue
		}
		if least < 4*budgetStep {
			t.Fatalf("%s is too little work to meet a deadline three times: %d units", name, least)
		}
		stops(c, name, f)
		if c.piece == "" {
			continue
		}
		// An error ends a sum at once. With ISERROR around each piece no
		// error does. Every piece is calculated, so each one has to stop
		// on its own after the deadline.
		formula = each(c.n, "ISERROR("+c.piece+")")
		f = mustParse(t, formula, "Z9")
		name = c.tab + ": " + formula[:min(len(formula), 60)] + "..."
		b = NewBudget(nil)
		f.Eval(env(c, b))
		if b.Used() < least {
			t.Errorf("%s counts %d units of work, for at least %d", name, b.Used(), least)
		}
		stops(c, name, f)
	}
}

// A result that a function passes on from a cell is copied for the caller
// to keep (TestResultsKeepOnlyTheirOwnText). The copy is counted.
func TestCopyOfAResultIsCounted(t *testing.T) {
	sheet := longWork(t)
	tab, _ := sheet.Tab("Long")
	const runs = 200
	for _, formula := range []string{"=IF(TRUE,A1)", "=IFERROR(A1,0)", "=CHOOSE(1,A1)", "=INDEX(A1:A9,1)"} {
		f := mustParse(t, formula, "Z9")
		b := NewBudget(nil)
		env := &Env{Grid: sheet, Tab: tab, At: pos(t, "Z9"), Budget: b}
		for i := 0; i < runs; i++ {
			if got := f.Eval(env); got.TextLen() != MaxTextLen {
				t.Fatalf("%s = a value of %d characters", formula, got.TextLen())
			}
		}
		if least := int64(runs * MaxTextLen / 64); b.Used() < least {
			t.Errorf("%d results of %s count %d units of work, for at least %d", runs, formula, b.Used(), least)
		}
	}
}

// longerThan agrees with a count of all the code points, for valid text
// and for bytes that are no UTF-8.
func TestLongerThan(t *testing.T) {
	rng := rand.New(rand.NewSource(20261115))
	pieces := []string{"a", "é", "가", "𝒂", "\xff", "\xe3\x81", ""}
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for k := rng.Intn(40); k > 0; k-- {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		s, n := b.String(), rng.Intn(40)
		if got, want := longerThan(s, n), utf8.RuneCountInString(s) > n; got != want {
			t.Fatalf("longerThan(%q, %d) = %v, want %v", s, n, got, want)
		}
	}
	for _, c := range []struct {
		s    string
		n    int
		want bool
	}{
		{strings.Repeat("𝒂", MaxTextLen), MaxTextLen, false},
		{strings.Repeat("𝒂", MaxTextLen+1), MaxTextLen, true},
		{strings.Repeat("a", LimitedTextLen+1), LimitedTextLen, true},
		{strings.Repeat("𝒂", LimitedTextLen), LimitedTextLen, false},
		{strings.Repeat("a", 4*LimitedTextLen+1), LimitedTextLen, true},
	} {
		if got := longerThan(c.s, c.n); got != c.want {
			t.Errorf("longerThan of %d bytes and %d = %v, want %v", len(c.s), c.n, got, c.want)
		}
	}
}

// A unit of DATEDIF has one or two letters. A longer text is no unit, and
// is not gone through to find that out.
//
// What a formula may cost that reads its unit from a cell is worked out
// here from the formula alone. Its four parts are a unit of work each. It
// reads one cell, which is one unit more, and it may look at two bytes of
// the unit, which is less than another. Going through the text of 32,767
// bytes once would be 511 units more. Writing the text in capitals to compare it would take memory for a
// copy, so the formula must take none.
func TestDateDifUnits(t *testing.T) {
	sheet := longWork(t)
	tab, _ := sheet.Tab("Long")
	env := func(b *Budget) *Env {
		return &Env{Grid: sheet, Tab: tab, At: pos(t, "Z9"), Budget: b}
	}
	for formula, want := range map[string]Value{
		`=DATEDIF(1,400,"d")`:   NumberValue(399),
		`=DATEDIF(1,400,"Yd")`:  NumberValue(34),
		`=DATEDIF(1,400,"ydd")`: ErrorValue(ErrNum),
		`=DATEDIF(1,400,"ſ")`:   ErrorValue(ErrNum),
		`=DATEDIF(1,400,"")`:    ErrorValue(ErrNum),
		`=DATEDIF(1,400,A1)`:    ErrorValue(ErrNum),
		`=DATEDIF(400,1,A1)`:    ErrorValue(ErrNum),
	} {
		if got := mustParse(t, formula, "Z9").Eval(env(NewBudget(nil))); got != want {
			t.Errorf("%s = %v, want %v", formula, got, want)
		}
	}
	const (
		parts = 4                 // the call, its two days and the reference
		reads = 1                 // the cell of the unit
		most  = parts + reads + 1 // and less than a unit for two bytes of it
	)
	// A1 and E1 hold small letters, and B1 capitals with one small letter,
	// so each of the texts differs from itself in capitals.
	for _, cell := range []string{"A1", "B1", "E1"} {
		if s := sheet.Value(tab, pos(t, cell)).Text(); len(s) != MaxTextLen || strings.ToUpper(s) == s {
			t.Fatalf("%s holds a text of %d bytes", cell, len(s))
		}
		formula := "=DATEDIF(1,400," + cell + ")"
		f := mustParse(t, formula, "Z9")
		b := NewBudget(nil)
		if got := f.Eval(env(b)); got != ErrorValue(ErrNum) {
			t.Errorf("%s = %v, want %v", formula, got, ErrorValue(ErrNum))
		}
		if used := b.Used(); used > most {
			t.Errorf("%s counts %d units of work, more than %d, so it goes through its unit", formula, used, most)
		}
		e := env(nil)
		if n := testing.AllocsPerRun(20, func() { f.Eval(e) }); n != 0 {
			t.Errorf("%s takes memory %v times, so it copies its unit", formula, n)
		}
	}
}
