package sheet

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// reversingSheet is a workbook of n formulas in column B of tab T, each
// the sum of refs ranges of column A, so that the index holds one column
// of n*refs ranges, loose as a full build leaves it. Every range is A1:A2
// but nine around the quarter points of the list, which go from A10:A11
// down to A2:A3 there. The sort takes its samples there, finds them in
// reverse order and first reverses the whole list, which spans parts. It
// returns the first row of each entry and the node of each formula.
func reversingSheet(t *testing.T, n, refs int) (*Workbook, []int32, []*fnode) {
	t.Helper()
	wb := newWorkbook("")
	tab := wb.newTab("T", n, 2, 0)
	wb.placeTab(tab, 0)
	col := ColID(tab.cols.ids[1])
	total := n * refs
	firsts := make([]int32, total)
	q := total / 4
	for k, at := range []int{q, 2 * q, 3 * q} {
		for d := -1; d <= 1; d++ {
			firsts[at+d] = int32(9 - 3*k - (d + 1))
		}
	}
	parse := func(text string) *formula.Formula {
		f, err := formula.Parse(text, formula.Pos{}, formula.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	plain := parse("=SUM(" + strings.TrimSuffix(strings.Repeat("$A$1:$A$2,", refs), ",") + ")")
	for r := range n {
		f := plain
		if row := firsts[r*refs : (r+1)*refs]; slices.ContainsFunc(row, func(first int32) bool { return first != 0 }) {
			var b strings.Builder
			for i, first := range row {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, "$A$%d:$A$%d", first+1, first+2)
			}
			f = parse("=SUM(" + b.String() + ")")
		}
		tab.put(RowID(tab.rows.ids[r]), col, &Cell{f: f})
	}
	wb.count.formulas = n
	if err := wb.buildDeps(nil); err != nil {
		t.Fatal(err)
	}
	nodes := make([]*fnode, n)
	for r := range n {
		nodes[r] = tab.cell(RowID(tab.rows.ids[r]), col).node
	}
	iv := wb.a1Column()
	if got := iv.sorted.len(); got != total || !iv.loose || len(iv.tail) != 0 {
		t.Fatalf("the column holds %d entries, loose %v, %d in its tail; want %d loose ones", got, iv.loose, len(iv.tail), total)
	}
	for p := range total {
		if e := iv.sorted.at(p); e.first != firsts[p] || e.last != firsts[p]+1 || e.n != nodes[p/refs] {
			t.Fatalf("entry %d is rows %d to %d of %p, want %d to %d of %p", p, e.first, e.last, e.n, firsts[p], firsts[p]+1, nodes[p/refs])
		}
	}
	return wb, firsts, nodes
}

// checkReversingColumn checks that the column holds each entry of the
// reversing sheet once and that a lookup of a few rows finds every entry
// that spans them.
func checkReversingColumn(t *testing.T, iv *intervals, firsts []int32, nodes []*fnode, refs int) {
	t.Helper()
	type entry struct {
		n           *fnode
		first, last int32
		alive       bool
	}
	want := map[entry]int{}
	for p, first := range firsts {
		want[entry{nodes[p/refs], first, first + 1, true}]++
	}
	got := map[entry]int{}
	for _, list := range [][]interval{iv.sorted.entries(), iv.tail} {
		for _, e := range list {
			got[entry{e.n, e.first, e.last, e.n.gen == e.gen}]++
		}
	}
	if len(got) != len(want) {
		t.Fatalf("the column holds %d kinds of entry, want %d", len(got), len(want))
	}
	for e, n := range want {
		if got[e] != n {
			t.Fatalf("the column holds %d of rows %d to %d, want %d", got[e], e.first, e.last, n)
		}
	}
	for _, row := range []int32{0, 1, 5, 9, 10} {
		spans := 0
		for _, first := range firsts {
			if first <= row && row <= first+1 {
				spans++
			}
		}
		visited := 0
		iv.query(row, func(*fnode) { visited++ }, nil)
		if visited != spans {
			t.Fatalf("a lookup of row %d visits %d entries, want %d", row, visited, spans)
		}
	}
}

// watchList returns a budget whose look at the deadline calls stop. At
// each look it notes how many entries of the backing arrays changed since
// the look before. writes returns the most entries that changed between
// two looks, the end of the work included, and those that changed after
// the last look.
func watchList(backing [][]interval, stop func() bool) (b *formula.Budget, writes func() (widest, after int)) {
	last := make([][]interval, len(backing))
	for i, part := range backing {
		last[i] = slices.Clone(part)
	}
	changed := func() int {
		n := 0
		for p, part := range backing {
			for i := range part {
				if part[i] != last[p][i] {
					n++
				}
			}
			copy(last[p], part)
		}
		return n
	}
	widest := 0
	b = formula.NewBudget(func() bool {
		widest = max(widest, changed())
		return stop()
	})
	return b, func() (int, int) {
		after := changed()
		return max(widest, after), after
	}
}

// reversalAt returns how far the reversal of the list has come: m when
// the first m entries and the last m have traded places and the rest is
// as the build left it, or -1 when the list is in no such state.
func reversalAt(iv *intervals, firsts []int32, nodes []*fnode, refs int) int {
	n := len(firsts)
	is := func(at, from int) bool {
		e := iv.sorted.at(at)
		return e.n == nodes[from/refs] && e.first == firsts[from]
	}
	m := 0
	for m < n/2 && is(m, n-1-m) && is(n-1-m, m) {
		m++
	}
	for k := m; k < n-m; k++ {
		if !is(k, k) {
			return -1
		}
	}
	return m
}

// mostSortWrites is the most entries the sort may change between two
// looks at the deadline: a swap is a step and changes two entries, and
// the steps of a block count a unit for every 64 of them.
var mostSortWrites = 2 * mostBetweenLooks(mergeBlock/64)

// TestSortChecksTheDeadlineInAReversal looks up a row of the column of
// 1,120,000 ranges that the sort first reverses. The default run has a
// quarter of the formulas, which is still more than mostSortWrites. The
// reversal swaps half a million pairs without comparing them, and each
// swap is a step of the budget: between two looks at the deadline the
// sort changes no more entries than the steps counted in between, and a
// deadline that passes in the reversal stops it there. The column then
// holds every entry, as the reversal left them, and stays loose, and the
// next lookup puts it in order and finds every entry that spans its row.
func TestSortChecksTheDeadlineInAReversal(t *testing.T) {
	const refs = 800
	wb, firsts, nodes := reversingSheet(t, sized(5600, 1400), refs)
	if n := len(firsts); n <= mostSortWrites {
		t.Fatalf("a reversal of %d entries changes no more than a sort may between two looks", n)
	}
	t.Run("whole", func(t *testing.T) {
		iv := cloneIndex(&wb.deps).ranges[colKey{wb.tab("T").id, 0}]
		b, writes := watchList(iv.sorted.backing(), func() bool { return false })
		iv.query(0, func(*fnode) {}, b)
		widest, _ := writes()
		t.Logf("a lookup that sorts %d entries: %d units, at most %d entries changed between two looks", len(firsts), b.Used(), widest)
		if iv.loose {
			t.Fatal("the column is still loose")
		}
		if widest > mostSortWrites {
			t.Errorf("the sort changed %d entries between two looks, want at most %d", widest, mostSortWrites)
		}
		checkReversingColumn(t, iv, firsts, nodes, refs)
	})
	t.Run("stopped", func(t *testing.T) {
		iv := cloneIndex(&wb.deps).ranges[colKey{wb.tab("T").id, 0}]
		reached := -1
		b, writes := watchList(iv.sorted.backing(), func() bool {
			// The reversal begins with the first entry, which the build
			// left as it is.
			if iv.sorted.at(0).n != nodes[0] {
				reached = reversalAt(iv, firsts, nodes, refs)
				return true
			}
			return false
		})
		visited := 0
		iv.query(0, func(*fnode) { visited++ }, b)
		widest, after := writes()
		t.Logf("the deadline passed with %d of %d pairs swapped; %d entries changed after the look, at most %d between two looks", reached, len(firsts)/2, after, widest)
		if !b.Exceeded() || visited != 0 || !iv.loose {
			t.Fatalf("the stopped lookup: exceeded %v, %d visited, loose %v", b.Exceeded(), visited, iv.loose)
		}
		if reached <= 0 || reached >= len(firsts)/2 {
			t.Fatalf("the look that stopped the sort found the reversal at %d of %d pairs", reached, len(firsts)/2)
		}
		if after != 0 || widest > mostSortWrites {
			t.Errorf("%d entries changed after the stop, %d between two looks", after, widest)
		}
		if got := reversalAt(iv, firsts, nodes, refs); got != reached {
			t.Fatalf("after the stop the reversal is at %d, the look found it at %d", got, reached)
		}
		checkReversingColumn(t, iv, firsts, nodes, refs)
	})
}

// TestReversalHandsTheGateOn looks up a row of the column that the sort
// first reverses as a pass at a gate of one slot. Once the reversal has
// begun another pass comes to wait in line. The sort hands the slot on at
// its next look, in the middle of the reversal, the waiting pass gets it,
// and a cancellation that follows ends the sort at the look after. The
// column then holds every entry and stays loose, and the next lookup finds
// every entry that spans its row.
func TestReversalHandsTheGateOn(t *testing.T) {
	const refs = 800
	wb, firsts, nodes := reversingSheet(t, sized(5600, 1400), refs)
	iv := wb.a1Column()
	g := newCalcGate(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turn, ok := g.enter(nil, ctx)
	if !ok {
		t.Fatal("the lookup got no slot")
	}
	defer turn.leave()
	got := make(chan int, 1)
	wait := func() {
		other, ok := g.enter(nil, context.Background())
		if !ok {
			got <- -2
			return
		}
		// The lookup waits in line while this pass holds the slot, so the
		// column is as the sort left it.
		got <- reversalAt(iv, firsts, nodes, refs)
		other.leave()
	}
	waiting := false
	handed, reached := 0, -1
	b, writes := watchList(iv.sorted.backing(), func() bool {
		if !waiting && iv.sorted.at(0).n != nodes[0] {
			waiting = true
			go wait()
			for g.waiting.Load() == 0 {
				time.Sleep(time.Millisecond)
			}
			// The lookup has held the slot for its quantum by now.
			time.Sleep(calcQuantum)
		}
		if !turn.check() {
			return true
		}
		select {
		case reached = <-got:
			handed++
			cancel()
		default:
		}
		return false
	})
	visited := 0
	iv.query(0, func(*fnode) { visited++ }, b)
	widest, after := writes()
	t.Logf("the waiting pass got the slot with %d of %d pairs swapped; %d entries changed after the stop, at most %d between two looks", reached, len(firsts)/2, after, widest)
	if handed != 1 || reached <= 0 || reached >= len(firsts)/2 {
		t.Fatalf("the waiting pass got the slot %d times, with the reversal at %d of %d pairs", handed, reached, len(firsts)/2)
	}
	if !b.Exceeded() || visited != 0 || !iv.loose {
		t.Fatalf("the cancelled lookup: exceeded %v, %d visited, loose %v", b.Exceeded(), visited, iv.loose)
	}
	if after != 0 || widest > mostSortWrites {
		t.Errorf("%d entries changed after the stop, %d between two looks", after, widest)
	}
	checkReversingColumn(t, iv, firsts, nodes, refs)
}
