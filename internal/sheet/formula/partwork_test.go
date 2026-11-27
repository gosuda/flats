package formula

import (
	"strconv"
	"strings"
	"testing"
)

// countParts returns the parts of a formula: its values, operators, calls
// and references, as the parser built them.
func countParts(n *node) int {
	total := 1
	for _, a := range n.args {
		total += countParts(a)
	}
	return total
}

// repeated returns a formula as long as a formula may be: copies of piece
// with sep between them, inside prefix and suffix.
func repeated(prefix, piece, sep, suffix string) string {
	n := (MaxFormulaChars - len(prefix) - len(suffix) + len(sep)) / (len(piece) + len(sep))
	return prefix + strings.TrimSuffix(strings.Repeat(piece+sep, n), sep) + suffix
}

// values writes n copies of a value with commas between.
func values(n int, value string) string {
	return strings.TrimSuffix(strings.Repeat(value+",", n), ",")
}

// partWork is a formula that reads no cell and goes through no text of a
// cell, with the least work it is: the units of the budget that its
// numbers, operators and calls come to, counted by hand from the formula.
type partWork struct {
	name, formula string
	today         Date
	want          Value
	least         int64
}

// The day numbers of 2026-11-14 and of the day after.
const (
	dayBefore Date = 46340
	dayAfter  Date = 46341
)

func partWorkCases(t testing.TB) []partWork {
	t.Helper()
	if v := evalOn(t, &testSheet{}, "=DATE(2026,11,14)"); v != NumberValue(float64(dayBefore)) {
		t.Fatalf("DATE(2026,11,14) = %v, want the day %d", v, dayBefore)
	}
	const n = 4000
	num, yes, no := NumberValue, BoolValue(true), BoolValue(false)
	// The formula of a sheet whose long work starts with a change of the
	// date: a median of 4,000 numbers written in the formula. A median
	// goes through its numbers once to count them and eight times to find
	// the middle ones.
	median := "=IF(TODAY()>DATE(2026,11,14),MEDIAN(" + values(n, "0") + "),-1)"
	cases := []partWork{
		{name: "a median of 4,000 numbers", formula: median, today: dayAfter, want: num(0), least: 9 * n},
		{name: "a median of 4,000 numbers and 3", formula: "=MEDIAN(" + values(n, "2") + ",3,4,5)", want: num(2), least: 9 * (n + 3)},
		// A deviation goes through its numbers twice: for the mean, and
		// for the distances from it.
		{name: "STDEV", formula: "=STDEV(" + values(n, "3") + ")", want: num(0), least: 2 * n},
		{name: "STDEVP", formula: "=STDEVP(" + values(n, "3") + ")", want: num(0), least: 2 * n},
		{name: "VAR", formula: "=VAR(" + values(n, "3") + ")", want: num(0), least: 2 * n},
		{name: "VARP", formula: "=VARP(" + values(n, "3") + ")", want: num(0), least: 2 * n},
		// Every other function goes through each of its arguments once.
		{name: "SUM", formula: "=SUM(" + values(n, "1") + ")", want: num(n), least: n},
		{name: "PRODUCT", formula: "=PRODUCT(" + values(n, "1") + ")", want: num(1), least: n},
		{name: "AVERAGE", formula: "=AVERAGE(" + values(n, "1") + ")", want: num(1), least: n},
		{name: "MAX", formula: "=MAX(" + values(n, "1") + ")", want: num(1), least: n},
		{name: "MIN", formula: "=MIN(" + values(n, "1") + ")", want: num(1), least: n},
		{name: "COUNT", formula: "=COUNT(" + values(n, "1") + ")", want: num(n), least: n},
		{name: "COUNTA", formula: "=COUNTA(" + values(n, "1") + ")", want: num(n), least: n},
		{name: "AND", formula: "=AND(" + values(n, "1") + ")", want: yes, least: n},
		{name: "OR", formula: "=OR(" + values(n, "0") + ")", want: no, least: n},
		{name: "XOR", formula: "=XOR(" + values(n, "1") + ")", want: no, least: n},
		{name: "SUMPRODUCT", formula: "=SUMPRODUCT(" + values(n, "1") + ")", want: num(1), least: n},
		{name: "CONCAT", formula: "=LEN(CONCAT(" + values(n, "1") + "))", want: num(n), least: n},
		{name: "CONCATENATE", formula: "=LEN(CONCATENATE(" + values(n, "1") + "))", want: num(n), least: n},
		{name: "TEXTJOIN", formula: `=LEN(TEXTJOIN("",TRUE,` + values(n, "1") + "))", want: num(n), least: n},
		// A pair of a conditional total is a range of one cell and its
		// criteria; the totals with a range of numbers have it first.
		{name: "COUNTIFS", formula: "=COUNTIFS(" + values(n, "1") + ")", want: num(1), least: n},
		{name: "SUMIFS", formula: "=SUMIFS(5," + values(n, "1") + ")", want: num(5), least: n},
		{name: "AVERAGEIFS", formula: "=AVERAGEIFS(5," + values(n, "1") + ")", want: num(5), least: n},
		{name: "MAXIFS", formula: "=MAXIFS(5," + values(n, "1") + ")", want: num(5), least: n},
		{name: "MINIFS", formula: "=MINIFS(5," + values(n, "1") + ")", want: num(5), least: n},
		// IFS calculates its conditions up to the first that holds, and
		// SWITCH its cases up to the first that is the value.
		{name: "IFS", formula: "=IFS(" + values(n/2, "0,7") + ",1,8)", want: num(8), least: n / 2},
		{name: "SWITCH", formula: "=SWITCH(1," + values(n/2, "2,7") + ",8)", want: num(8), least: n / 2},
		// CHOOSE calculates its index and the value it names, whatever
		// the number of its values.
		{name: "CHOOSE", formula: "=CHOOSE(" + values(n, "2") + ")", want: num(2), least: 2},
		// 4,096 numbers and the 4,095 additions between them.
		{name: "additions", formula: repeated("=", "1", "+", ""), want: num(4096), least: 8191},
		// 2,046 comparisons of two numbers each, and the call around them.
		{name: "comparisons", formula: repeated("=SUM(", "1=1", ",", ")"), want: num(2046), least: 3*2046 + 1},
		// 8,190 divisions by 100, 64 steps to a unit.
		{name: "percent signs", formula: "=0" + strings.Repeat("%", MaxFormulaChars-2), want: num(0), least: (MaxFormulaChars - 2) / 64},
		// A call is a part as a number is: 1,170 calls of EXP with a
		// number each and the additions between them.
		{name: "calls", formula: repeated("=", "EXP(0)", "+", ""), want: num(1170), least: 3*1170 - 1},
		// Finding a number by its rank goes through two tables of 256
		// counts eight times, also for one number: 4,096 steps, which are
		// 64 units, for each call.
		{name: "medians of one number", formula: repeated("=", "MEDIAN(1)", "+", ""), want: num(819), least: 64 * 819},
		{name: "LARGE of one number", formula: repeated("=", "LARGE(1,1)", "+", ""), want: num(744), least: 64 * 744},
		{name: "SMALL of one number", formula: repeated("=", "SMALL(1,1)", "+", ""), want: num(744), least: 64 * 744},
	}
	for i := range cases {
		if cases[i].today == 0 {
			cases[i].today = dayAfter
		}
	}
	return cases
}

// A formula can be long work without reading a cell: thousands of numbers
// written in it, which a median goes through nine times, or hundreds of
// calls. All of it is counted, so the deadline is looked at while such a
// formula is calculated, and a sheet of such formulas reaches a check
// point as a sheet of ranges does.
func TestPartsOfAFormulaAreCounted(t *testing.T) {
	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", nil)}}
	cases := partWorkCases(t)
	covered := map[string]bool{}
	for _, c := range cases {
		for id := range funcTable {
			if strings.Contains(c.formula, funcTable[id].name+"(") {
				covered[funcTable[id].name] = true
			}
		}
		wantCounted(t, sheet, c)
	}
	// Every function that takes any number of arguments has a case here
	// with thousands of them.
	for id := range funcTable {
		if spec := &funcTable[id]; spec.max == anyCount && !covered[spec.name] {
			t.Errorf("%s takes any number of arguments and has no case", spec.name)
		}
	}

	// A branch that is not calculated is not counted.
	b := NewBudget(nil)
	f := mustParse(t, cases[0].formula, "A1")
	if got := f.Eval(&Env{Grid: sheet, Today: dayBefore, Budget: b}); got != NumberValue(-1) || b.Used() > 16 {
		t.Errorf("before the day the formula is %v and counts %d units", got, b.Used())
	}
}

// wantCounted calculates the formula of a case on its own and in a row of
// calculations on one budget, as the engine does. It checks that the work
// counted is at least the case's least, that the deadline is looked at
// once for every 4,096 units, and that after the deadline the work ends
// within the units of one more look.
func wantCounted(t *testing.T, sheet *testSheet, c partWork) {
	t.Helper()
	f := mustParse(t, c.formula, "A1")
	if len(c.formula) > MaxFormulaChars {
		t.Fatalf("%s: the formula has %d characters", c.name, len(c.formula))
	}
	b, checks := expiring(1 << 30)
	env := &Env{Grid: sheet, Today: c.today, Budget: b}
	if got := f.Eval(env); got != c.want {
		t.Errorf("%s = %v, want %v", c.name, got, c.want)
	}
	once := b.Used()
	if once < c.least {
		t.Errorf("%s counts %d units of work, for at least %d", c.name, once, c.least)
		return
	}
	// The engine calculates one formula after another on one budget.
	// Whatever the formulas are, the deadline is looked at once for
	// every 4,096 units, and work reaches the budget at most 64 units
	// at a time.
	for i := 0; i < 20; i++ {
		f.Eval(env)
	}
	if used := b.Used(); used != 21*once || int64(*checks) < used/(budgetStep+64) {
		t.Errorf("%s: 21 calculations count %d units, one counts %d, with %d checks of the deadline", c.name, used, once, *checks)
	}
	// Once the deadline has passed, nothing more of the formula is
	// calculated: the work ends within the units of one more check.
	for _, at := range []int{1, 3} {
		b, checks := expiring(at)
		env := &Env{Grid: sheet, Today: c.today, Budget: b}
		runs := int64(0)
		for ; !b.Exceeded() && runs*once < int64(at+1)*budgetStep; runs++ {
			f.Eval(env)
		}
		if *checks != at || !b.Exceeded() || b.Used() > int64(at+1)*budgetStep {
			t.Errorf("%s: %d checks of the deadline, want %d, %d units of work in %d calculations, exceeded %v",
				c.name, *checks, at, b.Used(), runs, b.Exceeded())
		}
	}
}

// A reference is a part of a formula as a number is, also one whose range
// lies beside the tab and so holds no cell to read, and the name of a tab
// it names is gone through to find the tab. A formula of thousands of such
// references, and a median that goes through them nine times, reach the
// deadline as a formula of numbers does. So do the bands a conditional
// total finds for hundreds of ranges.
func TestReferencesAreCounted(t *testing.T) {
	long := strings.Repeat("Long", 25) // a tab name of 100 code points
	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", nil), newTab(t, long, nil)}}
	// The tabs have 26 columns, so AA1:AB2 lies beside them.
	const beside = "AA1:AB2"
	refs := func(prefix, piece, suffix string) (string, int64) {
		sep := ","
		if prefix == "=" {
			sep = "+"
		}
		f := repeated(prefix, piece, sep, suffix)
		return f, int64(strings.Count(f, piece))
	}
	num := NumberValue
	var cases []partWork
	add := func(name, prefix, piece, suffix string, want Value, least func(n int64) int64) {
		f, n := refs(prefix, piece, suffix)
		cases = append(cases, partWork{name: name, formula: f, today: dayAfter, want: want, least: least(n)})
	}
	each := func(n int64) int64 { return n }
	add("SUM of ranges beside the tab", "=SUM(", beside, ")", num(0), each)
	add("COUNT of ranges beside the tab", "=COUNT(", beside, ")", num(0), each)
	add("COUNTA of ranges beside the tab", "=COUNTA(", beside, ")", num(0), each)
	add("MAX of ranges beside the tab", "=MAX(", beside, ")", num(0), each)
	add("SUMPRODUCT of ranges beside the tab", "=SUMPRODUCT(", beside, ")", num(0), each)
	add("CONCAT of ranges beside the tab", "=LEN(CONCAT(", beside, "))", num(0), each)
	add("OR of ranges beside the tab and a number", "=OR(0,", beside, ")", BoolValue(false), each)
	add("COUNTIFS of ranges beside the tab", "=COUNTIFS(", beside+",1", ")", num(0), each)
	// A median of one number goes through its references once to count
	// and eight times more to find the middle.
	add("MEDIAN of a number and ranges beside the tab", "=MEDIAN(1,", beside, ")", num(1), func(n int64) int64 { return 9 * n })
	// A reference that names a tab goes through its 100 bytes.
	add("SUM of cells of a tab with a long name", "=SUM(", long+"!A1", ")", num(0), func(n int64) int64 { return n + n*100/64 })
	// The additions between those references are a part each as well.
	add("cells of a tab with a long name", "=", long+"!A1", "", num(0), func(n int64) int64 { return 2*n - 1 + n*100/64 })
	// Ranges of one size that start a row apart reach out of the tab of
	// 1,000 rows by a row more each, so each makes a band of its own:
	// finding the bands looks at every range for each band.
	var pairs []string
	for i := 1; len(strings.Join(pairs, ","))+30 < MaxFormulaChars; i++ {
		pairs = append(pairs, "A"+itoa(i)+":A"+itoa(i+999)+",1")
	}
	n := int64(len(pairs))
	cases = append(cases, partWork{name: "COUNTIFS of ranges that make a band each", formula: "=COUNTIFS(" + strings.Join(pairs, ",") + ")",
		today: dayAfter, want: num(0), least: n + n*n/64})
	for _, c := range cases {
		wantCounted(t, sheet, c)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
