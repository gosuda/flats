package formula

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func textCell(s string) *testCell {
	return &testCell{content: Content{Kind: ContentText, Text: s}}
}

func numberCell(x float64) *testCell {
	return &testCell{content: Content{Kind: ContentNumber, Num: x}}
}

// newTab returns a tab of the size of a new tab with the given cells.
func newTab(t testing.TB, name string, cells map[string]*testCell) *testTab {
	t.Helper()
	tab := &testTab{name: name, rows: 1000, cols: 26, cells: map[Pos]*testCell{}}
	for a1, c := range cells {
		tab.cells[pos(t, a1)] = c
	}
	return tab
}

// expiring returns a budget whose deadline passes at the given check, and a
// counter of the checks made.
func expiring(at int) (*Budget, *int) {
	checks := new(int)
	return NewBudget(func() bool {
		*checks++
		return *checks >= at
	}), checks
}

// Every function that goes through a range stops soon after the deadline,
// each on its own: none relies on another part of the formula to stop.
func TestEachRangeFunctionMeetsTheDeadline(t *testing.T) {
	tab := &testTab{name: "Sheet1", rows: MaxRows, cols: MaxCols, cells: map[Pos]*testCell{}}
	sheet := &testSheet{tabs: []*testTab{tab}}
	for _, formula := range []string{
		"=SUM(A:ZZ)", "=SUM(1,A:ZZ)", "=PRODUCT(A:ZZ)", "=SUMPRODUCT(A:ZZ,A:ZZ)", "=SUMPRODUCT(A:ZZ)",
		"=AVERAGE(A:ZZ)", "=COUNT(A:ZZ)", "=COUNTA(A:ZZ)", "=COUNTBLANK(A:ZZ)", "=MAX(A:ZZ)", "=MIN(A:ZZ)",
		"=MEDIAN(A:ZZ)", "=LARGE(A:ZZ,1)", "=SMALL(A:ZZ,1)", "=RANK(1,A:ZZ)",
		"=STDEV(A:ZZ)", "=STDEVP(A:ZZ)", "=VAR(A:ZZ)", "=VARP(A:ZZ)",
		`=COUNTIF(A:ZZ,1)`, `=COUNTIF(A:ZZ,"")`, `=COUNTIF(A:ZZ,"<>a*")`,
		`=COUNTIFS(A:ZZ,1,A:ZZ,1)`, `=COUNTIFS(A:ZZ,"",A:ZZ,"<>x")`, `=COUNTIFS(A:ZZ,"",A:ZZ,"",A:ZZ,"")`,
		`=SUMIF(A:ZZ,1)`, `=SUMIF(A:ZZ,"",A:ZZ)`, `=SUMIFS(A:ZZ,A:ZZ,"")`, `=SUMIFS(A:ZZ,A:ZZ,"",A:ZZ,"<>x")`,
		`=AVERAGEIF(A:ZZ,"")`, `=AVERAGEIF(A:ZZ,"",A:ZZ)`, `=AVERAGEIFS(A:ZZ,A:ZZ,"",A:ZZ,"")`,
		`=MAXIFS(A:ZZ,A:ZZ,"")`, `=MINIFS(A:ZZ,A:ZZ,"",A:ZZ,"")`,
		"=AND(A:ZZ)", "=OR(A:ZZ)", "=XOR(A:ZZ)",
		"=CONCAT(A:ZZ)", `=TEXTJOIN("",TRUE,A:ZZ)`, `=TEXTJOIN(",",FALSE,A:ZZ)`,
		"=MATCH(1,A:A,0)", `=MATCH("a*",A:A,0)`, "=VLOOKUP(1,A:ZZ,2,FALSE)", `=VLOOKUP("a*",A:ZZ,2,FALSE)`,
		"=XLOOKUP(1,A:A,B:B)", `=XLOOKUP("a",A:A,B:B)`,
	} {
		over, checks := expiring(1)
		mustParse(t, formula, "A1").Eval(&Env{Grid: sheet, Budget: over})
		if *checks == 0 || !over.Exceeded() || over.Used() > 4*budgetStep {
			t.Errorf("%s: %d checks of the deadline, %d units of work, exceeded %v", formula, *checks, over.Used(), over.Exceeded())
		}
	}
}

// longTexts is a sheet for matching patterns against long texts: 1,000
// rows that hold a text of the greatest length and a number, the same text
// across row 1 of the tab Wide, and patterns in column D that almost match
// the text. A pattern with a wildcard is as long as one may be.
func longTexts(t testing.TB) (sheet *testSheet, patterns map[string]string) {
	t.Helper()
	text := strings.Repeat("a", MaxTextLen)
	patterns = map[string]string{
		"D1": "*" + strings.Repeat("a", 253) + "b",                      // the end of the text
		"D2": "*" + strings.Repeat("a", 252) + "b*",                     // anywhere in the text: a failure table
		"D3": strings.Repeat("a", 253) + "b*",                           // the start of the text
		"D4": strings.Repeat("a", MaxTextLen/2) + "b",                   // no wildcard: a long text to equal
		"D5": "*" + strings.Repeat("a?", 126) + "b*",                    // single wildcards anywhere: bits
		"D6": strings.Repeat("*a", 126) + "*b*",                         // many pieces
		"D7": "*" + strings.Repeat("?", 252) + "b*",                     // nothing but single wildcards before the end
		"D8": strings.Repeat("?", 254) + "b",                            // no "*"
		"D9": strings.Repeat("*", 127) + "b" + strings.Repeat("*", 127), // runs of "*"
	}
	for cell, pattern := range patterns {
		if n := utf8.RuneCountInString(pattern); n > MaxPatternLen && strings.ContainsAny(pattern, "*?") {
			t.Fatalf("the pattern in %s has %d characters", cell, n)
		}
	}
	cells := map[string]*testCell{}
	sheet1 := newTab(t, "Sheet1", cells)
	for r := int32(0); r < 1000; r++ {
		sheet1.cells[Pos{Row: r, Col: 0}] = textCell(text)
		sheet1.cells[Pos{Row: r, Col: 1}] = numberCell(1)
	}
	for a1, p := range patterns {
		sheet1.cells[pos(t, a1)] = textCell(p)
	}
	wide := &testTab{name: "Wide", rows: 2, cols: MaxCols, cells: map[Pos]*testCell{}}
	for c := int32(0); c < MaxCols; c++ {
		wide.cells[Pos{Row: 0, Col: c}] = textCell(text)
	}
	return &testSheet{tabs: []*testTab{sheet1, wide}}, patterns
}

// Matching a pattern against long texts is work like any other: it counts
// toward the budget, the deadline is checked during it, and it stops soon
// after the deadline has passed.
func TestWildcardMatchingMeetsTheDeadline(t *testing.T) {
	sheet, patterns := longTexts(t)
	forms := []string{
		"=COUNTIF(A1:A1000,%s)", "=COUNTIFS(A1:A1000,%s)", "=COUNTIFS(B1:B1000,1,A1:A1000,%s)",
		`=COUNTIF(A1:A1000,"<>"&%s)`,
		"=SUMIF(A1:A1000,%s,B1:B1000)", "=SUMIFS(B1:B1000,A1:A1000,%s)", "=SUMIFS(B1:B1000,B1:B1000,1,A1:A1000,%s)",
		"=AVERAGEIF(A1:A1000,%s,B1:B1000)", "=AVERAGEIFS(B1:B1000,A1:A1000,%s)",
		"=MAXIFS(B1:B1000,A1:A1000,%s)", "=MINIFS(B1:B1000,A1:A1000,%s)",
		"=MATCH(%s,A1:A1000,0)", "=VLOOKUP(%s,A1:B1000,2,FALSE)", "=HLOOKUP(%s,Wide!A1:ZZ2,2,FALSE)",
		// one row of 702 texts: the deadline is met within the row
		"=COUNTIF(Wide!A1:ZZ1,%s)", "=SUMIFS(Wide!A2:ZZ2,Wide!A2:ZZ2,\"\",Wide!A1:ZZ1,%s)", "=MATCH(%s,Wide!A1:ZZ1,0)",
	}
	run := func(formula string, at int) (checks int, b *Budget) {
		b, n := expiring(at)
		mustParse(t, formula, "Z9").Eval(&Env{Grid: sheet, At: pos(t, "Z9"), Budget: b})
		return *n, b
	}
	// The patterns that go through all of each text reach any deadline.
	// The others look at one end of a text, or stop at its first letter:
	// they are little work for a thousand texts.
	through := map[string]bool{"D2": true, "D4": true, "D5": true, "D6": true, "D7": true, "D9": true}
	for cell, pattern := range patterns {
		for _, form := range forms {
			formula := strings.ReplaceAll(form, "%s", cell)
			if !through[cell] {
				if checks, b := run(formula, 1<<30); b.Exceeded() || b.Used() > 4*budgetStep {
					t.Errorf("%s with the pattern %.12q is %d units of work and %d checks of the deadline", formula, pattern, b.Used(), checks)
				}
				continue
			}
			for _, at := range []int{1, 3} {
				checks, b := run(formula, at)
				if checks != at || !b.Exceeded() || b.Used() > int64(at+3)*budgetStep {
					t.Errorf("%s with the pattern %.12q: %d checks of the deadline, want %d, %d units of work, exceeded %v",
						formula, pattern, checks, at, b.Used(), b.Exceeded())
				}
			}
		}
	}
}

// Going through the "*"s of a pattern is counted like the rest of a match:
// a pattern of nothing else, matched against many short texts, reaches the
// deadline at every step of the budget.
func TestRepeatedStarsAreCounted(t *testing.T) {
	const cells = 20000
	tab := &testTab{name: "Sheet1", rows: cells, cols: 26, cells: map[Pos]*testCell{}}
	for r := int32(0); r < cells; r++ {
		tab.cells[Pos{Row: r, Col: 0}] = textCell("x")
	}
	tab.cells[pos(t, "B1")] = textCell(strings.Repeat("*", MaxPatternLen))
	tab.cells[pos(t, "B2")] = textCell(strings.Repeat("*", 127) + "x" + strings.Repeat("*", 127))
	tab.cells[pos(t, "B3")] = textCell(strings.Repeat("*", 127) + "y" + strings.Repeat("*", 127))
	sheet := &testSheet{tabs: []*testTab{tab}}
	eval := func(formula string, b *Budget) Value {
		return mustParse(t, formula, "Z9").Eval(&Env{Grid: sheet, At: pos(t, "Z9"), Budget: b})
	}
	// A thousand cells and 255 "*"s are about 4,000 units of matching and
	// 1,000 of reading: the first check of the deadline is reached.
	b, checks := expiring(1)
	eval("=COUNTIF(A1:A1000,B1)", b)
	if *checks != 1 || !b.Exceeded() {
		t.Errorf("=COUNTIF(A1:A1000,B1): %d checks of the deadline, %d units of work", *checks, b.Used())
	}
	for _, c := range []struct {
		formula string
		want    Value
		stars   int64 // the "*"s that each text costs at the least
	}{
		{"=COUNTIF(A1:A20000,B1)", NumberValue(cells), 255},
		{"=COUNTIF(A1:A20000,B2)", NumberValue(cells), 254},
		{"=COUNTIF(A1:A20000,B3)", NumberValue(0), 127},
		{"=IFERROR(MATCH(B3,A1:A20000,0),-1)", NumberValue(-1), 127},
		{"=COUNTIFS(A1:A20000,B1,A1:A20000,B2)", NumberValue(cells), 255 + 254},
		{`=SUMIF(A1:A20000,"<>"&LEFT(B3,253),A1:A20000)`, NumberValue(0), 127},
	} {
		formula := c.formula
		b, checks := expiring(1 << 30)
		if got := eval(formula, b); got != c.want {
			t.Errorf("%s = %v, want %v", formula, got, c.want)
		}
		if used := b.Used(); used < cells*c.stars/64 || int64(*checks) < used/(budgetStep+64) {
			t.Errorf("%s counts %d units of work and checks the deadline %d times", formula, used, *checks)
		}
		for _, at := range []int{1, 3, 10} {
			b, checks := expiring(at)
			eval(formula, b)
			if *checks != at || !b.Exceeded() || b.Used() > int64(at+3)*budgetStep {
				t.Errorf("%s: %d checks of the deadline, want %d, %d units of work, exceeded %v", formula, *checks, at, b.Used(), b.Exceeded())
			}
		}
	}
}

// matchSteps is the most steps one match of a pattern of p code points
// against a text of t code points may take, 64 to a unit of the budget.
func matchSteps(t, p int) int { return 8*(t+p) + 8192 }

// One match takes at most 8*(text+pattern) + 8192 steps, whatever the
// pattern and the text: no shape of pattern makes a text cost its length
// times the length of the pattern. A pattern with a wildcard is as long as
// one may be, and a pattern without is as long as a text may be.
func TestMatchingWorkIsBoundedByTextAndPattern(t *testing.T) {
	rng := rand.New(rand.NewSource(20261114))
	pick := func(alphabet []string, n int) string {
		var b strings.Builder
		for ; n > 0; n-- {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		return b.String()
	}
	rep := strings.Repeat
	patterns := []string{
		rep("*", 255),
		"*" + rep("a", 252) + "b*", "*" + rep("a", 253) + "b", rep("a", 253) + "b*", rep("a", 254) + "*",
		"*" + rep("a?", 126) + "b*", "*" + rep("?a", 126) + "b*", "*" + rep("a?", 126) + "b", rep("a?", 126) + "b*",
		"*" + rep("?", 252) + "b*", rep("?", 254) + "b", rep("?", 255), "*a" + rep("?", 251) + "b*",
		"*" + rep("a", 126) + "?" + rep("a", 125) + "b*",
		rep("*a?", 84) + "*b*", rep("*?a", 84) + "b*", rep("*a?b", 63) + "*", rep("*aa?", 63) + "*b",
		rep("*a", 126) + "*b*", rep("a*", 127) + "b", rep("*ab", 84) + "*c*", rep("*", 127) + "b" + rep("*", 127),
		"*" + rep("a", 60) + "b*" + rep("a", 60) + "?b*" + rep("a", 60) + "b*" + rep("a", 60) + "?c*",
		rep("*"+rep("a", 30)+"?b", 7) + "*c*",
		"*" + rep("~?", 126) + "b*", "*" + rep("~*", 126) + "?*", rep("~~", 127) + "*", "*" + rep("~*?", 84) + "b*",
		"*" + rep("가", 252) + "나*", "*" + rep("가?", 126) + "나*", rep("*가?", 84) + "*나*",
		"*" + rep("𝒂?", 126) + "𝒃*", "*" + rep("𝒂", 252) + "𝒃*", rep("*?", 127) + "*", rep("*𝒂?", 84) + "*𝒃*",
		// no wildcard: text of any length
		rep("a", MaxTextLen/2) + "b", rep("a", MaxTextLen-1) + "b", rep("ab", 8000) + "c", rep("가", 16000) + "나",
		rep("~*", 16000), rep("a", 300), "b",
	}
	for i := 0; i < 8; i++ {
		patterns = append(patterns, pick([]string{"a", "a", "a", "a", "?", "?", "*", "b", "~*", "~?", "가"}, 127))
		patterns = append(patterns, pick([]string{"a", "a", "a", "a", "a", "a", "a", "?", "*", "b"}, 255))
	}
	texts := []string{
		rep("a", MaxTextLen), rep("가", MaxTextLen), rep("𝒂", MaxTextLen), rep("a", MaxTextLen-1) + "b", "b" + rep("a", MaxTextLen-1),
		rep(rep("a", 50)+"b", MaxTextLen/51), rep("ab", MaxTextLen/2), rep("aab", MaxTextLen/3),
		rep("?", MaxTextLen), rep("*", MaxTextLen), rep("가나", MaxTextLen/2), rep(rep("a", 252)+"c", MaxTextLen/253),
		pick([]string{"a", "a", "a", "b"}, MaxTextLen), pick([]string{"a", "가", "?", "*"}, MaxTextLen),
		rep("a", 300), "a", "",
	}
	worst, worstOf := 0.0, ""
	for _, pattern := range patterns {
		p := utf8.RuneCountInString(pattern)
		for _, text := range texts {
			n := utf8.RuneCountInString(text)
			// A new Env has no table yet. The second match of each kind
			// finds the tables that the first one left.
			ev := &evaluator{env: &Env{}}
			w, err := ev.wildcardOf(pattern)
			if err != ErrNone {
				t.Fatalf("the pattern %.20q of %d characters is %v", pattern, p, err)
			}
			for _, what := range []string{"match", "search", "search from the middle", "match again", "search again"} {
				before := ev.spent
				switch what {
				case "match", "match again":
					w.match(ev, text)
				case "search", "search again":
					w.search(ev, text, 0)
				default:
					w.search(ev, text, n/2)
				}
				steps := ev.spent - before
				if steps > matchSteps(n, p) {
					t.Fatalf("%s of the pattern %.20q (%d characters) against the text %.10q (%d characters) takes %d steps, more than %d",
						what, pattern, p, text, n, steps, matchSteps(n, p))
				}
				if ratio := float64(steps) / float64(n+p+1); n == MaxTextLen && ratio > worst {
					worst, worstOf = ratio, what+" of "+pattern[:min(len(pattern), 16)]+" against "+text[:min(len(text), 12)]
				}
			}
		}
	}
	t.Logf("the most steps for each code point of a long text and its pattern: %.2f (%s)", worst, worstOf)

	// The same through formulas: every text a formula reads costs one
	// match at the most, and the reading itself.
	sheet, long := longTexts(t)
	for _, c := range []struct {
		formula string
		texts   int
	}{
		{"=COUNTIF(A1:A1000,%s)", 1000}, {`=COUNTIF(A1:A1000,"<>"&%s)`, 1000}, {"=SUMIF(A1:A1000,%s,B1:B1000)", 1000},
		{"=COUNTIFS(B1:B1000,1,A1:A1000,%s)", 1000}, {"=MINIFS(B1:B1000,A1:A1000,%s)", 1000},
		{"=IFERROR(MATCH(%s,A1:A1000,0),-1)", 1000}, {"=IFERROR(VLOOKUP(%s,A1:B1000,2,FALSE),-1)", 1000},
		{"=IFERROR(HLOOKUP(%s,Wide!A1:ZZ2,2,FALSE),-1)", MaxCols},
		{"=IFERROR(SEARCH(%s,A1),-1)", 1}, {"=IFERROR(SEARCH(%s,A1,20000),-1)", 1},
	} {
		for _, cell := range []string{"D2", "D5", "D9"} {
			pattern := long[cell]
			formula := strings.ReplaceAll(c.formula, "%s", cell)
			b := NewBudget(nil)
			mustParse(t, formula, "Z9").Eval(&Env{Grid: sheet, At: pos(t, "Z9"), Budget: b})
			// Two more matches for reading the pattern and for the steps
			// that are rounded up, and three units for each cell read.
			most := int64(c.texts+2)*int64(matchSteps(MaxTextLen, utf8.RuneCountInString(pattern)))/64 + 3*int64(c.texts) + 1024
			if b.Used() > most {
				t.Errorf("%s with the pattern %.12q counts %d units of work for %d texts, more than %d", formula, pattern, b.Used(), c.texts, most)
			}
		}
	}
}

// A sheet in which a date turns on many conditional counts with a pattern
// that almost matches every text at every place: 1,000 texts of 32,767
// characters. With a pattern as long as one may be, each count is about
// 1,000 passes of the bits over a text. A longer pattern is #VALUE! at
// once. The time is logged; the bound is the count of steps above.
func TestCountsTurnedOnByADateAreBounded(t *testing.T) {
	sheet, _ := longTexts(t)
	tab := sheet.tabs[0]
	tab.cells[pos(t, "E1")] = textCell("*" + strings.Repeat("a?", 8191) + "b*")
	today, _ := ParseDate("2026-11-15")
	eval := func(formula string) (Value, int64, time.Duration) {
		f := mustParse(t, formula, "Z9")
		b := NewBudget(nil)
		start := time.Now()
		v := f.Eval(&Env{Grid: sheet, At: pos(t, "Z9"), Today: today, Budget: b})
		return v, b.Used(), time.Since(start)
	}
	v, used, _ := eval("=IF(TODAY()>DATE(2026,11,14),COUNTIF($A$1:$A$1000,$E$1),0)")
	if v != ErrorValue(ErrValue) || used > 1024 {
		t.Errorf("a pattern of 16,384 characters: %v after %d units of work, want #VALUE! at once", v, used)
	}
	const formulas = 3
	var total time.Duration
	for i := 0; i < formulas; i++ {
		v, used, took := eval("=IF(TODAY()>DATE(2026,11,14),COUNTIF($A$1:$A$1000,$D$5),0)")
		total += took
		if most := int64(1002*matchSteps(MaxTextLen, MaxPatternLen)/64 + 4096); v != NumberValue(0) || used > most {
			t.Errorf("the count is %v after %d units of work, want 0 within %d", v, used, most)
		}
	}
	perText := total / (formulas * 1000)
	t.Logf("%d counts over 1,000 texts of 32,767 characters took %v: %v for each text", formulas, total, perText)
	if total > 2*time.Minute {
		t.Errorf("%d counts took %v", formulas, total)
	}
}

// The work of a match is counted whether or not a deadline is near: at
// least one unit for every 64 code points it goes through.
func TestWildcardWorkIsCounted(t *testing.T) {
	text := strings.Repeat("a", 4000)
	cells := map[string]*testCell{
		"B1": textCell("*" + strings.Repeat("a", 200) + "b"),                      // the last 201 characters of each text
		"B2": textCell(strings.Repeat("a", 250) + "b*"),                           // the first 251
		"B3": textCell("*" + strings.Repeat("a?", 100) + "b*"),                    // all 4,000
		"B4": textCell(strings.Repeat("?", 254) + "b"),                            // the first 255
		"B5": textCell(">" + strings.Repeat("a", 3999) + "b"),                     // an order of two texts
		"B6": textCell(strings.Repeat("a", 100) + "*" + strings.Repeat("a", 100)), // a match at both ends
		"B7": textCell("*" + strings.Repeat("a", 200) + "b*"),                     // all 4,000
	}
	for r := 1; r <= 10; r++ {
		cells["A"+strconv.Itoa(r)] = textCell(text)
	}
	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", cells)}}
	for _, c := range []struct {
		formula string
		want    Value
		points  int64 // code points the match goes through at least
	}{
		{"=COUNTIF(A1:A10,B1)", NumberValue(0), 10 * 201},
		{"=COUNTIF(A1:A10,B2)", NumberValue(0), 10 * 251},
		{"=COUNTIF(A1:A10,B3)", NumberValue(0), 10 * 4000},
		{"=COUNTIF(A1:A10,B4)", NumberValue(0), 10 * 255},
		{"=COUNTIF(A1:A10,B5)", NumberValue(0), 10 * 4000},
		{"=COUNTIF(A1:A10,B6)", NumberValue(10), 10 * 200},
		{"=COUNTIF(A1:A10,B7)", NumberValue(0), 10 * 4000},
		{"=SUMIFS(A1:A10,A1:A10,B3)", NumberValue(0), 10 * 4000},
		{"=MATCH(B3,A1:A10,0)", ErrorValue(ErrNA), 10 * 4000},
		{"=MATCH(B6,A1:A10,0)", NumberValue(1), 200},
		{"=IFERROR(VLOOKUP(B7,A1:B10,2,FALSE),-1)", NumberValue(-1), 10 * 4000},
		{"=IFERROR(SEARCH(B3,A1),-1)", NumberValue(-1), 4000},
		{"=SEARCH(B6,A1)", NumberValue(1), 200},
	} {
		b, checks := expiring(1 << 30)
		if got := mustParse(t, c.formula, "Z9").Eval(&Env{Grid: sheet, At: pos(t, "Z9"), Budget: b}); got != c.want {
			t.Errorf("%s = %v, want %v", c.formula, got, c.want)
		}
		// A match adds to the budget 64 units at a time, so two checks of
		// the deadline lie at most 64 units more than a step apart.
		if used := b.Used(); used < c.points/64 || int64(*checks) < used/(budgetStep+64) {
			t.Errorf("%s counts %d units and checks the deadline %d times, for at least %d code points", c.formula, used, *checks, c.points)
		}
	}
}

// A join that the deadline stops is #LIMIT!, whichever way it reads its
// range: with a scan, or cell by cell where the range reaches outside the
// tab and empty pieces count. The first two cells make the text too long.
// TEXTJOIN then looks for an error in the cells that follow, and the last
// cell holds one. A join that did not reach it must not give #VALUE!,
// which says that no error comes first.
func TestAJoinStoppedByTheDeadlineIsLimit(t *testing.T) {
	long := strings.Repeat("a", MaxTextLen)
	tab := newTab(t, "Sheet1", map[string]*testCell{
		"A1":    textCell(long),
		"B1":    textCell(long),
		"Z1000": {f: mustParse(t, "=1/0", "Z1000")},
	})
	sheet := &testSheet{tabs: []*testTab{tab}}
	limit, div0 := ErrorValue(ErrLimit), ErrorValue(ErrDiv0)
	for _, formula := range []string{
		`=TEXTJOIN("",TRUE,A1:Z1000)`,   // a scan, as empty pieces are passed over
		`=TEXTJOIN(",",FALSE,A1:Z1000)`, // a scan, as the range lies in the tab
		`=TEXTJOIN(",",FALSE,A1:Z2000)`, // cell by cell
		`=TEXTJOIN(",",FALSE,A1:Z1000,A1:Z2000)`,
	} {
		f := mustParse(t, formula, "AB1")
		eval := func(b *Budget) Value {
			return f.Eval(&Env{Grid: sheet, At: pos(t, "AB1"), Budget: b})
		}
		b, checks := expiring(1 << 30)
		if got := eval(b); got != div0 {
			t.Errorf("%s = %v, want %v", formula, got, div0)
		}
		total, limits := *checks, 0
		for at := 1; at <= total; at++ {
			b, _ := expiring(at)
			switch got := eval(b); got {
			case limit:
				limits++
			case div0:
			default:
				t.Errorf("%s, with the deadline at check %d of %d = %v, want %v", formula, at, total, got, limit)
			}
		}
		if limits == 0 {
			t.Errorf("%s: no deadline in %d checks stopped it", formula, total)
		}
	}
}

// A function that the deadline stops gives #LIMIT!. It does not give the
// value of the part it went through, or the error of a search or a match
// that it did not finish.
func TestAStoppedFunctionIsLimit(t *testing.T) {
	numbers := newTab(t, "Numbers", nil) // A1:Z1000 hold the numbers 1 to 26,000
	texts := newTab(t, "Texts", nil)     // A1:A40 hold 32,766 "a" and "b", B1:B40 hold 32,767 zeros
	for r := int32(0); r < 1000; r++ {
		for c := int32(0); c < 26; c++ {
			numbers.cells[Pos{Row: r, Col: c}] = numberCell(float64(r*26 + c + 1))
		}
	}
	for r := int32(0); r < 40; r++ {
		texts.cells[Pos{Row: r, Col: 0}] = textCell(strings.Repeat("a", MaxTextLen-1) + "b")
		texts.cells[Pos{Row: r, Col: 1}] = textCell(strings.Repeat("0", MaxTextLen))
	}
	sheet := &testSheet{tabs: []*testTab{numbers, texts}}
	limit := ErrorValue(ErrLimit)
	for _, c := range []struct{ tab, formula string }{
		{"Numbers", "=COUNT(A1:Z1000)"},
		{"Numbers", "=COUNTBLANK(A1:Z1000)"},
		{"Numbers", "=RANK(26000,A1:Z1000)"},
		{"Numbers", "=MEDIAN(A1:Z1000)"},
		{"Numbers", "=LARGE(A1:Z1000,5)"},
		{"Numbers", "=SMALL(A1:Z1000,5)"},
		{"Numbers", "=STDEV(A1:Z1000)"},
		{"Numbers", "=VARP(A1:Z1000)"},
		// Direct text that reads as a number counts.
		{"Texts", "=COUNT(" + list(40, "IF(TRUE,B#)", ",") + ")"},
		// A pattern whose piece is found at the end of each text.
		{"Texts", each(20, `SEARCH("*a?b",A#)`)},
		{"Texts", each(20, `MATCH("*a?b*",A#,0)`)},
	} {
		tab, _ := sheet.Tab(c.tab)
		f := mustParse(t, c.formula, "AB1")
		eval := func(b *Budget) Value {
			return f.Eval(&Env{Grid: sheet, Tab: tab, At: pos(t, "AB1"), Budget: b})
		}
		name := c.formula[:min(len(c.formula), 50)]
		b, checks := expiring(1 << 30)
		want := eval(b)
		if want.Kind() == KindError {
			t.Fatalf("%s = %v", name, want)
		}
		total, limits := *checks, 0
		for at := 1; at <= total; at++ {
			b, _ := expiring(at)
			switch got := eval(b); got {
			case limit:
				limits++
			case want:
			default:
				t.Errorf("%s, with the deadline at check %d of %d = %v, want %v or the value %v", name, at, total, got, limit, want)
			}
		}
		if limits == 0 {
			t.Errorf("%s: no deadline in %d checks stopped it", name, total)
		}
	}
}
