package sheet

import (
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// cycleSheet is a workbook whose column A holds a cycle of n formulas:
// A1=A2, A2=A3, and so on, and the last reads A1. It returns the nodes of
// A1 to An, all dirty under pass.
func cycleSheet(t *testing.T, n int, pass uint32) (*Workbook, []*fnode) {
	t.Helper()
	wb := newWorkbook("")
	tab := wb.newTab("T", n, 1, 0)
	wb.placeTab(tab, 0)
	col := ColID(tab.cols.ids[0])
	next, err := formula.Parse("=A2", formula.Pos{}, formula.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for r := range n - 1 {
		tab.put(RowID(tab.rows.ids[r]), col, &Cell{f: next})
	}
	back, err := formula.Parse("=A1", formula.Pos{Row: int32(n - 1)}, formula.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	tab.put(RowID(tab.rows.ids[n-1]), col, &Cell{f: back})
	wb.count.formulas = n
	if err := wb.buildDeps(nil); err != nil {
		t.Fatal(err)
	}
	nodes := make([]*fnode, n)
	for r := range n {
		nodes[r] = tab.cell(RowID(tab.rows.ids[r]), col).node
		nodes[r].dirty = pass
	}
	return wb, nodes
}

// TestOrderingStopsWhileItUnwinds orders a cycle of 100,000 formulas. Once
// the walk has pushed them all, nothing it does reads a cell or counts a
// unit: it takes the formulas off its stack, marks the cycle and passes
// the formulas it has already ordered. A deadline that passes in any of
// these ends the ordering within the pace of 64 formulas.
func TestOrderingStopsWhileItUnwinds(t *testing.T) {
	n := sized(100_000, 20_000)
	const pass = 7
	for _, c := range []struct {
		name string
		stop func(nodes []*fnode) bool
		done func(nodes []*fnode) int // formulas handled after the stop
	}{
		{"taking the formulas off the stack", func(nodes []*fnode) bool {
			return nodes[len(nodes)-1].seen == pass // all are pushed
		}, func(nodes []*fnode) int {
			// A formula taken off the stack passes the lowest index it
			// reached to the one below it, A1's.
			taken := 0
			for _, m := range nodes[1 : len(nodes)-1] {
				if m.low == nodes[0].index {
					taken++
				}
			}
			return taken
		}},
		{"marking the cycle", func(nodes []*fnode) bool {
			return nodes[len(nodes)-1].seen == pass && !nodes[len(nodes)-1].onStack // marked first
		}, func(nodes []*fnode) int {
			marked := 0
			for _, m := range nodes[:len(nodes)-1] {
				if !m.onStack {
					marked++
				}
			}
			return marked
		}},
		{"passing ordered formulas", func(nodes []*fnode) bool {
			return nodes[0].seen == pass && !nodes[0].onStack // A1 is marked last
		}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			wb, nodes := cycleSheet(t, n, pass)
			stopped := false
			b := formula.NewBudget(func() bool {
				stopped = stopped || c.stop(nodes)
				return stopped
			})
			// A1 is ordered first, so the walk passes A2 to An as roots.
			dirty := append([]*fnode(nil), nodes...)
			order, ok := wb.order(dirty, pass, b)
			if !stopped {
				t.Fatal("the deadline was never looked at")
			}
			if ok || order != nil {
				t.Fatalf("the ordering went on to its end after the deadline: %d formulas", len(order))
			}
			if c.done != nil {
				after := c.done(nodes)
				t.Logf("%d formulas were handled after the deadline", after)
				if after > passStride {
					t.Errorf("%d formulas were handled after the deadline", after)
				}
			}
		})
	}
}

// singlesSheet is a workbook of n formulas in column A of tab T, each
// $A$1 written refs times, so that the index holds one list of n*refs
// entries for that cell. The entries of every other formula are dead.
func singlesSheet(t *testing.T, n, refs int) (*Workbook, []*fnode) {
	t.Helper()
	wb := newWorkbook("")
	tab := wb.newTab("T", n+1, 1, 0)
	wb.placeTab(tab, 0)
	col := ColID(tab.cols.ids[0])
	f, err := formula.Parse("="+strings.TrimSuffix(strings.Repeat("$A$1+", refs), "+"), formula.Pos{}, formula.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for r := 1; r <= n; r++ {
		tab.put(RowID(tab.rows.ids[r]), col, &Cell{f: f})
	}
	wb.count.formulas = n
	if err := wb.buildDeps(nil); err != nil {
		t.Fatal(err)
	}
	var nodes []*fnode
	for r := 1; r <= n; r++ {
		node := tab.cell(RowID(tab.rows.ids[r]), col).node
		if r%2 == 0 {
			wb.unregister(node) // its entries die
		}
		nodes = append(nodes, node)
	}
	return wb, nodes
}

// newFormulaCell puts a formula in A1 of tab T and returns its node.
func (wb *Workbook) newFormulaCell(t *testing.T, text string) *fnode {
	t.Helper()
	tab := wb.tab("T")
	f, err := formula.Parse(text, formula.Pos{}, formula.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	c := &Cell{f: f}
	tab.put(RowID(tab.rows.ids[0]), ColID(tab.cols.ids[0]), c)
	c.node = &fnode{tab: tab, row: RowID(tab.rows.ids[0]), col: ColID(tab.cols.ids[0]), cell: c}
	return c.node
}

// a1List returns the entries of the list of single references of $A$1 in
// tab T.
func (wb *Workbook) a1List() []depRef { return wb.deps.a1List(wb.tab("T").id) }

// a1Parts returns the parts of that list as they are kept, each to its
// capacity.
func (wb *Workbook) a1Parts() [][]depRef { return wb.deps.refs(posKey{wb.tab("T").id, 0, 0}).backing() }

func (d *depIndex) a1List(tab TabID) []depRef { return d.refs(posKey{tab, 0, 0}).entries() }

// TestEnteringStopsInAListOfSingles writes =$A$1 beside 6,000 formulas
// that each read $A$1 1,638 times, so that entering it goes through one
// list of 9,828,000 entries for its dead ones. The default run has a
// tenth of the formulas. The list is gone through a block at a time, each
// block counted before it is gone through, and a deadline that passes on
// the way ends it there. The index then still holds every live entry and
// counts what it holds.
func TestEnteringStopsInAListOfSingles(t *testing.T) {
	const refs = 1638
	formulas := sized(6000, 600)
	for _, at := range []int64{0, 1, 2, 7} {
		wb, nodes := singlesSheet(t, formulas, refs)
		before := int64(len(wb.a1List()))
		// The deadline passes once the units of at blocks are counted.
		limit := at * pruneUnits
		var b *formula.Budget
		b = formula.NewBudget(func() bool { return b.Used() >= limit })
		n := wb.newFormulaCell(t, "=$A$1")
		wb.register(n, b)
		if !b.Exceeded() {
			t.Fatalf("with the deadline at %d blocks: the entry was made without a look at it", at)
		}
		list := wb.a1List()
		// Half the formulas are dead, refs entries each, so the dead
		// entries it dropped tell how far it went through the list.
		dropped := before - int64(len(list))
		looked := 2 * dropped
		t.Logf("with the deadline at %d blocks: it went through %d entries of %d", at, looked, before)
		// The budget looks at the deadline every 4,096 units, two blocks,
		// so the look that finds it may come that much later. No block
		// is gone through after it.
		if most := (at + int64(budgetCheck/pruneUnits) + 1) * pruneBlock; looked > most+2*refs {
			t.Errorf("with the deadline at %d blocks: it went through %d entries of %d, want at most %d", at, looked, before, most)
		}
		if !wb.deps.stale {
			t.Errorf("with the deadline at %d blocks: the index is not stale", at)
		}
		checkSingles(t, wb, nodes, refs)
	}
}

// checkSingles checks that the list of $A$1 holds every live entry and
// that the index counts the entries it holds.
func checkSingles(t *testing.T, wb *Workbook, nodes []*fnode, refs int) {
	t.Helper()
	live := map[*fnode]int{}
	for _, e := range wb.a1List() {
		if e.n.gen == e.gen {
			live[e.n]++
		}
	}
	for i, n := range nodes {
		if want := refs * ((i + 1) % 2); live[n] != want {
			t.Fatalf("formula %d has %d live entries, want %d", i, live[n], want)
		}
	}
	if size := wb.deps.size(); wb.deps.total != size {
		t.Fatalf("the index counts %d entries and holds %d", wb.deps.total, size)
	}
}

// TestCompactionStopsInAListOfSingles compacts an index whose dead entries
// lie in one long list of single references, stopped at many points. Each
// block of the list is counted before it is gone through, and the
// compaction ends at the block where the deadline passes.
func TestCompactionStopsInAListOfSingles(t *testing.T) {
	const refs = 1638
	wb, nodes := singlesSheet(t, sized(6000, 600), refs)
	before := int64(len(wb.a1List()))
	for _, at := range []int64{0, 1, 3, 6} {
		d := cloneIndex(&wb.deps)
		limit := at * pruneUnits
		var b *formula.Budget
		b = formula.NewBudget(func() bool { return b.Used() >= limit })
		d.compact(b)
		list := d.a1List(wb.tab("T").id)
		looked := 2 * (before - int64(len(list)))
		t.Logf("with the deadline at %d blocks: it went through %d entries of %d", at, looked, before)
		if most := (at + int64(budgetCheck/pruneUnits) + 1) * pruneBlock; looked > most+2*refs {
			t.Errorf("with the deadline at %d blocks: it went through %d entries of %d, want at most %d", at, looked, before, most)
		}
		saved := wb.deps
		wb.deps = *d
		checkSingles(t, wb, nodes, refs)
		wb.deps = saved
	}
}
