package formula

import (
	"strconv"
	"strings"
	"testing"
)

// countingGrid counts the cells a formula reads.
type countingGrid struct {
	Grid
	reads int
}

func (g *countingGrid) Value(tab TabRef, p Pos) Value {
	g.reads++
	return g.Grid.Value(tab, p)
}

func (g *countingGrid) Filled(tab TabRef, p Pos) bool {
	g.reads++
	return g.Grid.Filled(tab, p)
}

// stopAt is a budget whose deadline passes at the first look once ready
// reports true, with the cells read and the units counted at that look.
type stopAt struct {
	b           *Budget
	reads, used int
	stopped     bool
}

func newStopAt(g *countingGrid, ready func() bool) *stopAt {
	s := &stopAt{}
	s.b = NewBudget(func() bool {
		if !s.stopped && ready() {
			s.stopped, s.reads, s.used = true, g.reads, int(s.b.Used())
		}
		return s.stopped
	})
	return s
}

// TestAWideProductStopsAtItsDeadline evaluates SUMPRODUCT over a thousand
// copies of a row of 702 numbers. Its second pass is one row of 702,000
// reads. A deadline that passes anywhere, near the start of that pass
// included, ends the pass at the next cell.
func TestAWideProductStopsAtItsDeadline(t *testing.T) {
	tab := &testTab{name: "Sheet1", rows: 1, cols: MaxCols, cells: map[Pos]*testCell{}}
	for c := int32(0); c < MaxCols; c++ {
		tab.cells[Pos{Col: c}] = numberCell(1)
	}
	f := mustParse(t, "=SUMPRODUCT("+list(1000, "A1:ZZ1", ",")+")", "A2")
	const first = 1000 * MaxCols // reads of the pass that looks for errors
	for _, from := range []int{0, first / 2, first, first + 1, first + 5000, first + first/2} {
		g := &countingGrid{Grid: &testSheet{tabs: []*testTab{tab}}}
		s := newStopAt(g, func() bool { return g.reads >= from })
		if got := f.Eval(&Env{Grid: g, At: pos(t, "A2"), Budget: s.b}); got != ErrorValue(ErrLimit) {
			t.Fatalf("with the deadline after %d reads the product is %v", from, got)
		}
		if !s.stopped {
			t.Fatalf("the deadline after %d reads was never looked at", from)
		}
		if after := g.reads - s.reads; after > 0 {
			t.Errorf("with the deadline after %d reads, %d cells were read after it", from, after)
		}
		// The evaluator settles the small steps it went before the
		// deadline at its end, at most a unit for each 64.
		if after := int(s.b.Used()) - s.used; after > spendStep/64 {
			t.Errorf("with the deadline after %d reads, %d units were counted after it", from, after)
		}
	}
}

// TestBandsStopAtTheDeadline evaluates COUNTIFS over 600 ranges of a
// 1,000-row tab that start one row apart, so that their parts in the tab
// have 600 heights and the area they cover is 600 bands. The budget is
// shared with the formulas before, so its deadline may be looked at near
// the first band. Finding the bands ends there.
func TestBandsStopAtTheDeadline(t *testing.T) {
	tab := newTab(t, "Sheet1", nil)
	var b strings.Builder
	b.WriteString("=COUNTIFS(")
	for k := 1; k <= 600; k++ {
		if k > 1 {
			b.WriteByte(',')
		}
		b.WriteString("A" + strconv.Itoa(k) + ":A" + strconv.Itoa(k+999) + ",1")
	}
	b.WriteString(")")
	f := mustParse(t, b.String(), "C1")
	g := &countingGrid{Grid: &testSheet{tabs: []*testTab{tab}}}
	whole := NewBudget(nil)
	f.Eval(&Env{Grid: g, At: pos(t, "C1"), Budget: whole})
	// A band is 601 steps, about ten units. Earlier formulas leave the
	// budget at every offset before its next look, here up to one look
	// after the bands began.
	for left := int64(1); left < whole.Used()+budgetStep && left <= 2*budgetStep; left += 37 {
		g.reads = 0
		s := newStopAt(g, func() bool { return true })
		s.b.Add(budgetStep - left)
		f.Eval(&Env{Grid: g, At: pos(t, "C1"), Budget: s.b})
		if !s.stopped {
			continue
		}
		if after := int(s.b.Used()) - s.used; after > spendStep/64 {
			t.Fatalf("with %d units to the next look, %d units were counted after the deadline", left, after)
		}
		if after := g.reads - s.reads; after > 0 {
			t.Fatalf("with %d units to the next look, %d cells were read after the deadline", left, after)
		}
	}
}

// TestNoRangeIsReadOnAfterTheDeadline stops each function that goes
// through a range at several points of its work. Once the deadline is
// found, it reads at most the rest of the block of about 1,024 cells that
// it counted before.
func TestNoRangeIsReadOnAfterTheDeadline(t *testing.T) {
	tab := &testTab{name: "Sheet1", rows: 1000, cols: MaxCols, cells: map[Pos]*testCell{}}
	for r := int32(0); r < tab.rows; r += 3 {
		for c := int32(0); c < MaxCols; c += 5 {
			tab.cells[Pos{Row: r, Col: c}] = numberCell(1)
			tab.cells[Pos{Row: r + 1, Col: c}] = textCell("ab")
		}
	}
	for _, formula := range []string{
		"=SUM(A:ZZ)", "=PRODUCT(A:ZZ)", "=SUMPRODUCT(A:ZZ,A:ZZ)", "=AVERAGE(A:ZZ)", "=COUNT(A:ZZ)",
		"=COUNTA(A:ZZ)", "=COUNTBLANK(A:ZZ)", "=MAX(A:ZZ)", "=MEDIAN(A:ZZ)", "=LARGE(A:ZZ,1)", "=RANK(1,A:ZZ)",
		"=STDEV(A:ZZ)", `=COUNTIF(A:ZZ,"a*")`, `=COUNTIFS(A:ZZ,1,A:ZZ,"<>x")`, `=SUMIFS(A:ZZ,A:ZZ,"",A:ZZ,"<>x")`,
		`=MAXIFS(A:ZZ,A:ZZ,"")`, "=AND(A:ZZ)", "=XOR(A:ZZ)", "=CONCAT(A:ZZ)", `=TEXTJOIN(",",FALSE,A:ZZ)`,
		`=MATCH("x",A:A,0)`, `=VLOOKUP("x",A:ZZ,2,FALSE)`, `=XLOOKUP("x",A:A,B:B)`,
	} {
		f := mustParse(t, formula, "A1")
		g := &countingGrid{Grid: &testSheet{tabs: []*testTab{tab}}}
		f.Eval(&Env{Grid: g, Budget: NewBudget(nil)})
		total := g.reads
		for _, part := range []float64{0, 0.01, 0.3, 0.7} {
			from := int(part * float64(total))
			g.reads = 0
			s := newStopAt(g, func() bool { return g.reads >= from })
			f.Eval(&Env{Grid: g, Budget: s.b})
			if !s.stopped {
				continue
			}
			if after := g.reads - s.reads; after > 1024+MaxCols {
				t.Errorf("%s, stopped after %d of %d reads: %d cells were read after the deadline", formula, from, total, after)
			}
		}
	}
}
