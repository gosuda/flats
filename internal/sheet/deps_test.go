package sheet

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// eachFormula visits every formula cell, tabs in order and rows by position.
func (wb *Workbook) eachFormula(fn func(t *Tab, row RowID, col ColID, c *Cell)) {
	wb.eachFormulaPaced(nil, func(t *Tab, row RowID, col ColID, _ formula.Pos, c *Cell) bool {
		fn(t, row, col, c)
		return true
	})
}

// size returns the entries the index holds, dead ones included.
func (d *depIndex) size() int64 {
	var n int64
	for key := range d.singles {
		l := d.refs(key)
		n += int64(l.len())
	}
	for _, iv := range d.ranges {
		n += int64(iv.sorted.len() + len(iv.tail))
	}
	return n
}

// TestReadLimitBeforeIndex sends a call whose formulas each stay under the
// limit of one formula and together read far more cells than a sheet may.
// It is refused before the index gets an entry for those reads.
func TestReadLimitBeforeIndex(t *testing.T) {
	e := start(t)
	// Data is one row of 702 columns, so Data!A:ZZ reads 702 cells.
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Data","at":"ZZ1","rows":[[1]]},
		{"op":"delete_rows","tab":"Data","at":2,"count":999},{"op":"write","tab":"Sheet1","at":"B1","rows":[["=SUM(Data!A:ZZ)"]]}]}`)
	e.want("B1", "1")
	sum := "=SUM(" + strings.TrimSuffix(strings.Repeat("Data!A:ZZ,", 10), ",") + ")"
	const formulas = 3000 // 3,000 * 10 * 702 = 21,060,000 cells
	rows := strings.TrimSuffix(strings.Repeat(fmt.Sprintf("[%q],", sum), formulas), ",")
	call := `{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + rows + `]}]}`
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := e.agent(call)
	runtime.ReadMemStats(&after)
	if werr := refusal(err); werr.Limit != "sheet_reads" || werr.Value != formulas*10*702+702 {
		t.Fatalf("the call answered %v", err)
	}
	// An entry of the index is 24 bytes. The refused reads would need half
	// a gigabyte of them.
	if spent := after.TotalAlloc - before.TotalAlloc; spent > 128<<20 {
		t.Errorf("the refused call allocated %d MiB", spent>>20)
	}
	e.in.mu.RLock()
	size := e.in.wb.deps.size()
	e.in.mu.RUnlock()
	if size > 702 {
		t.Errorf("the index holds %d entries after the refusal", size)
	}
	// The sheet is as before and calculates on.
	e.want("A1", "")
	e.mustAgent(`{"ops":[{"op":"write","tab":"Data","at":"A1","rows":[[41]]}]}`)
	e.want("B1", "42")
	// A person's formula over the limit of one formula gets no entries
	// either: 600 times a range of 100,000 rows and 26 columns.
	alice := e.person("Alice")
	if ack := alice.set("C1", "=SUM("+strings.TrimSuffix(strings.Repeat("A1:Z100000,", 600), ",")+")"); status(ack) != "refused:formula_reads" {
		t.Fatal(status(ack))
	}
	e.in.mu.RLock()
	size = e.in.wb.deps.size()
	e.in.mu.RUnlock()
	if size > 702 {
		t.Errorf("the index holds %d entries after the refused formula", size)
	}
	e.wantStored("after the refusals")
}

// TestIndexBuildUnderBudget checks that the build of the index stops at
// the deadline of the recalculation it belongs to.
func TestIndexBuildUnderBudget(t *testing.T) {
	e := start(t)
	rows := strings.TrimSuffix(strings.Repeat(`["=SUM(B1:D4)"],`, 6000), ",")
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + rows + `]}]}`)
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	wb := e.in.wb
	passed := formula.NewBudget(func() bool { return true })
	if err := wb.buildDeps(passed); err == nil || err.Limit != "recalculation_time" {
		t.Fatalf("a build past its deadline answered %v", err)
	}
	if !wb.deps.stale || wb.deps.size() != 0 {
		t.Fatalf("after the stopped build the index is stale=%v with %d entries", wb.deps.stale, wb.deps.size())
	}
	if passed.Used() > 2*6000 {
		t.Fatalf("the stopped build counted %d units for 6,000 formulas", passed.Used())
	}
	if err := wb.buildDeps(nil); err != nil || wb.deps.stale || wb.deps.size() != 3*6000 {
		t.Fatalf("the build without a deadline: %v, stale=%v, %d entries", err, wb.deps.stale, wb.deps.size())
	}
}

// TestIndexFollowsLiveReferences rewrites formulas many times without any
// structural edit. The index keeps to the references the sheet holds, and
// still finds the dependents of a cell.
func TestIndexFollowsLiveReferences(t *testing.T) {
	e := start(t)
	alice := e.person("Alice")
	tab, row1, colZ := e.ids("Z1")
	_, row2, _ := e.ids("Z2")
	set := func(row RowID, pos int, text string) string {
		return jsonf(`{"op":"set","tab":%d,"row":%d,"col":%d,"text":%q,"at":[%d,25]}`, tab, row, colZ, text, pos)
	}
	alice.do(set(row1, 0, "=A1"), set(row2, 1, "=SUM(A1:C2)"))
	e.in.mu.RLock()
	node := e.in.wb.tabs[0].cell(row1, colZ).node
	e.in.mu.RUnlock()
	// 20,000 rewrites of Z1, each reading another single cell, and of Z2,
	// each reading another range.
	const rounds, perAction = 40, 500
	for round := 0; round < rounds; round++ {
		ops := make([]string, 0, perAction)
		for i := 0; i < perAction/2; i++ {
			k := round*perAction/2 + i
			col, row := formula.ColumnName(int32(k%25)), k/25+1
			ops = append(ops, set(row1, 0, fmt.Sprintf("=%s%d", col, row)),
				set(row2, 1, fmt.Sprintf("=SUM(%s%d:%s%d)", col, row, formula.ColumnName(int32(k%25)+1), row+2)))
		}
		if ack := alice.do(ops...); strings.Contains(status(ack), "refused") {
			t.Fatal(status(ack))
		}
	}
	index := func() (size, total, live int64, stale bool) {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		d := &e.in.wb.deps
		return d.size(), d.total, d.live, d.stale
	}
	size, total, live, stale := index()
	e.in.mu.RLock()
	same := e.in.wb.tabs[0].cell(row1, colZ).node == node
	e.in.mu.RUnlock()
	if stale || !same {
		t.Fatal("the index was built again, so the test says nothing about content edits")
	}
	// One entry for Z1 and one per column of the range of Z2.
	if live != 3 || size > depSlack+16 || total != size {
		t.Fatalf("after %d rewrites the index holds %d entries (%d counted) for %d live ones", rounds*perAction, size, total, live)
	}
	// The last rewrite of Z1 reads Y400, and that of Z2 the range Y400:Z402.
	e.wantText("Z1", "=Y400")
	e.wantText("Z2", "=SUM(Y400:Z402)")
	alice.set("Y400", "5")
	e.want("Z1", "5")
	e.want("Z2", "5")
	alice.set("Y402", "2")
	e.want("Z2", "7")
	// Formulas that leave their cells leave the index.
	alice.do(set(row1, 0, ""), set(row2, 1, "plain"))
	for round := 0; round < 5; round++ {
		ops := make([]string, 0, perAction)
		for i := 0; i < perAction; i++ {
			ops = append(ops, set(row1, 0, fmt.Sprintf("=B%d", (round*perAction+i)%newTabRows+1)))
		}
		alice.do(ops...)
	}
	alice.do(set(row1, 0, ""))
	if size, _, live, _ = index(); live != 0 || size > depSlack+16 {
		t.Fatalf("with no formula left the index holds %d entries, %d live", size, live)
	}
}

// compactSheet is a sheet whose index compacts inside a small action. Data
// is one row of 702 columns. Three formulas of Sheet1 each read a range
// over all of it refs times, an entry per column each time, and B1 holds
// one more such formula that the test writes again and again, so that
// each write leaves its old entries dead.
type compactSheet struct {
	e      *env
	refs   int
	big    [2]string
	writes int
}

func newCompactSheet(t *testing.T, refs int) *compactSheet {
	e := start(t, unhurried)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Data","at":"ZZ1","rows":[[1]]},
		{"op":"delete_rows","tab":"Data","at":2,"count":999},{"op":"write","tab":"Data","at":"A1","rows":[[1]]}]}`)
	sum := "=SUM(" + strings.TrimSuffix(strings.Repeat("Data!A1:ZZ1,", refs), ",") + ")"
	e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[%q],[%q],[%q]]}]}`, sum, sum+"+1", sum+"+2"))
	return &compactSheet{e: e, refs: refs, big: [2]string{sum, sum + "+0"}}
}

func (s *compactSheet) write(text string) string {
	return agentStatus(s.e, jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[[%q]]}]}`, text))
}

// index returns the entries the index holds and counts, and the live ones.
func (s *compactSheet) index() (size, total, live int64) {
	s.e.in.mu.RLock()
	defer s.e.in.mu.RUnlock()
	d := &s.e.in.wb.deps
	return d.size(), d.total, d.live
}

// ready writes the large formula into B1 again and again, until a write
// of a formula of one entry there would compact the index, and returns
// the entries the index holds then.
func (s *compactSheet) ready() int64 {
	t := s.e.t
	t.Helper()
	for {
		s.writes++
		if got := s.write(s.big[s.writes%2]); got != "applied" {
			t.Fatalf("a write of B1: %s", got)
		}
		_, row, col := s.e.ids("B1")
		s.e.in.mu.RLock()
		entries := s.e.in.wb.tab("Sheet1").cell(row, col).node.entries
		s.e.in.mu.RUnlock()
		size, total, live := s.index()
		if total != size {
			t.Fatalf("the index counts %d entries and holds %d", total, size)
		}
		// Writing one entry in place of those of B1 leaves these dead
		// and live.
		if total-live+entries > live-entries+1 {
			return size
		}
	}
}

// TestCompactionCountsAndStops reaches the compaction of the index through
// the write of one small formula, after many writes of a large one left
// their entries dead. The compaction goes through every entry the index
// holds, so it counts on the budget of the action, and the deadline ends
// it as it ends the other work of the action: a write whose deadline
// passes at a quarter of the units of a whole compaction is refused within
// an eighth of them after it.
func TestCompactionCountsAndStops(t *testing.T) {
	// 500 references of 702 columns are 351,000 entries a formula.
	refs := raced(500, 250)
	s := newCompactSheet(t, refs)
	e := s.e
	deadline := e.workDeadline()

	held := s.ready()
	got, whole := deadline.run(never, func() string { return s.write("=Data!A1") })
	if got != "applied" {
		t.Fatalf("the write that compacts: %s", got)
	}
	size, total, live := s.index()
	if size >= held || size != total || size != live {
		t.Fatalf("after the compaction of %d entries the index holds %d, counts %d, %d live", held, size, total, live)
	}
	// Each entry is a step on the way through, 64 to a unit.
	if work := e.in.lastWork.Load(); work < held/64 {
		t.Errorf("the compaction of %d entries counted %d units", held, work)
	}
	// The index answers from the compacted entries.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Data","at":"A1","rows":[[2]]}]}`)
	e.want("B1", "2")
	e.want("A1", fmt.Sprint(3*refs))
	e.want("A3", fmt.Sprint(3*refs+2))

	// The write has no other work that the deadline could stop: the
	// compaction is most of its units.
	s.ready()
	deadline.stopsAtDeadline(t, whole, func() string { return s.write("=Data!A1") })
	// The refused write left B1 as it was.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Data","at":"A1","rows":[[5]]}]}`)
	e.want("B1", fmt.Sprint(6*refs))
	e.want("A1", fmt.Sprint(6*refs))
}

// budgetCheck is the number of units after which a budget first reads the
// clock.
const budgetCheck = 4096

// TestStoppedCompactionKeepsTheIndex stops a compaction at many points
// and checks that the index still counts what it holds and finds the
// formulas that read a cell, as before the compaction.
func TestStoppedCompactionKeepsTheIndex(t *testing.T) {
	s := newCompactSheet(t, 200)
	e := s.e
	s.ready()
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	wb := e.in.wb
	data := wb.tab("Data")
	want := map[*Cell]bool{}
	wb.eachFormula(func(t *Tab, _ RowID, _ ColID, c *Cell) {
		if t.name == "Sheet1" {
			want[c] = true
		}
	})
	if len(want) != 4 {
		t.Fatalf("Sheet1 holds %d formulas", len(want))
	}
	// The work of a whole compaction, on a copy.
	whole := formula.NewBudget(nil)
	cloneIndex(&wb.deps).compact(whole)
	if whole.Used() < 8*budgetCheck {
		t.Fatalf("a whole compaction counts %d units, too few to stop it on the way", whole.Used())
	}
	for _, part := range []float64{0, 0.1, 0.3, 0.6, 0.9} {
		// A copy of the index for each stop, so each starts from the dead
		// entries the writes left.
		d := cloneIndex(&wb.deps)
		stop := int64(part * float64(whole.Used()))
		var b *formula.Budget
		b = formula.NewBudget(func() bool { return b.Used() >= stop })
		before := d.size()
		d.compact(b)
		if !b.Exceeded() || b.Used() > stop+2*budgetCheck {
			t.Fatalf("a compaction told to stop at %d units counted %d (stopped %v)", stop, b.Used(), b.Exceeded())
		}
		if d.total != d.size() || d.size() > before || d.stale {
			t.Fatalf("stopped at %d units: the index holds %d of %d entries and counts %d", stop, d.size(), before, d.total)
		}
		for _, col := range []int32{0, 350, 701} {
			got := map[*Cell]bool{}
			d.dependents(data.id, formula.Pos{Row: 0, Col: col}, func(n *fnode) { got[n.cell] = true }, nil)
			if len(got) != len(want) {
				t.Fatalf("stopped at %d units: %d dependents of column %d, want %d", stop, len(got), col, len(want))
			}
			for c := range want {
				if !got[c] {
					t.Fatalf("stopped at %d units: a formula of Sheet1 is no dependent of column %d", stop, col)
				}
			}
		}
		// The next compaction drops the dead entries left.
		d.compact(nil)
		if d.total != d.live || d.size() != d.live {
			t.Fatalf("after a stop at %d units the compaction left %d entries, %d live", stop, d.size(), d.live)
		}
	}
}

// cloneIndex copies the lists of an index, so that a compaction of the
// copy leaves the original as it is.
func cloneIndex(d *depIndex) *depIndex {
	c := &depIndex{singles: map[posKey][]depRef{}, more: map[posKey][][]depRef{}, ranges: map[colKey]*intervals{}, today: d.today, total: d.total, live: d.live}
	for k := range d.singles {
		c.setRefs(k, d.refs(k).clone())
	}
	for k, iv := range d.ranges {
		c.ranges[k] = &intervals{sorted: iv.sorted.clone(), maxLast: iv.maxLast.clone(),
			tail: append([]interval(nil), iv.tail...), loose: iv.loose}
	}
	return c
}

// clone copies the parts of a list.
func (l parted[E]) clone() parted[E] {
	c := parted[E]{head: slices.Clone(l.head)}
	for _, part := range l.rest {
		c.rest = append(c.rest, append(make([]E, 0, partLen), part...))
	}
	return c
}

// entries returns a copy of the entries of a list.
func (l parted[E]) entries() []E {
	list := slices.Clone(l.head)
	for _, part := range l.rest {
		list = append(list, part...)
	}
	return list
}

// backing returns the parts of a list, each to its capacity.
func (l parted[E]) backing() [][]E {
	parts := [][]E{l.head[:cap(l.head)]}
	for _, part := range l.rest {
		parts = append(parts, part[:cap(part)])
	}
	return parts
}
