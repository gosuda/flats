package sheet

import (
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// watchWrites returns a budget whose deadline passes once limit units are
// counted, or never when limit is negative. At each look at the deadline
// it notes how many entries of the backing arrays changed since the look
// before: moved, copied or cleared. writes returns the most entries that
// changed between two looks, the end of the work included, and those that
// changed after the last look, which is the look that found the deadline
// passed when there was one.
func watchWrites[E comparable](backing [][]E, limit int64) (b *formula.Budget, writes func() (widest, after int)) {
	last := make([][]E, len(backing))
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
		return limit >= 0 && b.Used() >= limit
	})
	return b, func() (int, int) {
		after := changed()
		return max(widest, after), after
	}
}

// pruneUnits is the units a block of a list of single references counts:
// two steps an entry, for going through it and moving a dead one.
const pruneUnits = 2 * pruneBlock / 64

// mostBetweenLooks is the most entries that work may change between two
// looks at the deadline when its blocks count units each. That is the
// units of a look and one block, a step an entry changed.
func mostBetweenLooks(units int) int { return (budgetCheck + units) * 64 }

// TestPruningStopsWithNothingToFinish stops the dropping of dead entries
// of one list of 9,828,000 single references at blocks from the first to
// the last, as a formula that joins the list and a compaction drop them.
// The default run has a tenth of the entries. After the look that finds
// the deadline passed no entry of the list is moved or cleared. Between two looks no more entries change than the units
// counted in between allow, also when the work ends without a stop, and
// the index holds every live entry and counts what it holds.
func TestPruningStopsWithNothingToFinish(t *testing.T) {
	const refs = 1638
	formulas := sized(6000, 600)
	blocks := int64((formulas*refs + pruneBlock - 1) / pruneBlock)
	for name, prune := range map[string]func(wb *Workbook, b *formula.Budget){
		"entering": func(wb *Workbook, b *formula.Budget) { wb.register(wb.newFormulaCell(t, "=$A$1"), b) },
		"compacting": func(wb *Workbook, b *formula.Budget) {
			wb.deps.compact(b)
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, at := range []int64{1, blocks / 2, blocks - 2, -1} {
				wb, nodes := singlesSheet(t, formulas, refs)
				b, writes := watchWrites(wb.a1Parts(), at*pruneUnits)
				prune(wb, b)
				widest, after := writes()
				t.Logf("with the deadline at %d of %d blocks: %d entries changed after the stop, at most %d between two looks", at, blocks, after, widest)
				if stopped := at >= 0; b.Exceeded() != stopped {
					t.Fatalf("with the deadline at %d blocks: stopped %v", at, b.Exceeded())
				}
				if at >= 0 && after != 0 {
					t.Errorf("with the deadline at %d blocks: %d entries changed after the look that found it", at, after)
				}
				if most := mostBetweenLooks(pruneUnits); widest > most {
					t.Errorf("with the deadline at %d blocks: %d entries changed between two looks, want at most %d", at, widest, most)
				}
				if at < 0 {
					// The new formula's entry joins the list, or the
					// compaction gives it a list of its own.
					if got := wb.deps.total - wb.deps.live; got != 0 {
						t.Errorf("after the whole work %d dead entries are counted", got)
					}
				}
				checkSingles(t, wb, nodes, refs)
			}
		})
	}
}

// rangesSheet is a workbook of n formulas in column B of tab T, each the
// sum of $A$1:$A$2 written refs times, so that the index holds one column
// of n*refs ranges, loose as a full build leaves it. The entries of every
// other formula are dead.
func rangesSheet(t *testing.T, n, refs int) (*Workbook, []*fnode) {
	t.Helper()
	wb := newWorkbook("")
	tab := wb.newTab("T", n, 2, 0)
	wb.placeTab(tab, 0)
	col := ColID(tab.cols.ids[1])
	f, err := formula.Parse("=SUM("+strings.TrimSuffix(strings.Repeat("$A$1:$A$2,", refs), ",")+")", formula.Pos{}, formula.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for r := 0; r < n; r++ {
		tab.put(RowID(tab.rows.ids[r]), col, &Cell{f: f})
	}
	wb.count.formulas = n
	if err := wb.buildDeps(nil); err != nil {
		t.Fatal(err)
	}
	var nodes []*fnode
	for r := 0; r < n; r++ {
		node := tab.cell(RowID(tab.rows.ids[r]), col).node
		if r%2 == 1 {
			wb.unregister(node) // its entries die
		}
		nodes = append(nodes, node)
	}
	return wb, nodes
}

// a1Column returns the interval index of column A of tab T.
func (wb *Workbook) a1Column() *intervals { return wb.deps.ranges[colKey{wb.tab("T").id, 0}] }

// checkRanges checks that the column of $A$1:$A$2 holds every live entry,
// that the index counts the entries it holds, and that a lookup finds the
// formulas that read A1.
func checkRanges(t *testing.T, wb *Workbook, nodes []*fnode, refs int) {
	t.Helper()
	iv := wb.a1Column()
	live := map[*fnode]int{}
	for _, list := range [][]interval{iv.sorted.entries(), iv.tail} {
		for _, e := range list {
			if e.n.gen == e.gen {
				live[e.n]++
			}
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
	found := map[*fnode]bool{}
	wb.deps.dependents(wb.tab("T").id, formula.Pos{}, func(n *fnode) { found[n] = true }, nil)
	if len(found) != (len(nodes)+1)/2 {
		t.Fatalf("a lookup of A1 finds %d formulas, want %d", len(found), (len(nodes)+1)/2)
	}
}

// TestMergeStopsWithNothingToFinish stops the merge of a column of
// 4,800,000 loose ranges, half of them dead, at blocks from the first to
// the last, in the lookup of a recalculation without an edit and in a
// compaction. The default run has a quarter of the ranges. After the look
// that finds the deadline passed the column is not written to, between
// two looks no more entries change than the units counted in between
// allow, and the column holds every live entry, loose, for the next
// lookup to merge.
func TestMergeStopsWithNothingToFinish(t *testing.T) {
	const refs = 800
	formulas := sized(6000, 1500)
	const mergeUnits = 2 * mergeBlock / 64
	blocks := int64((formulas*refs + mergeBlock - 1) / mergeBlock)
	for name, merge := range map[string]func(wb *Workbook, b *formula.Budget){
		"looking up": func(wb *Workbook, b *formula.Budget) {
			wb.deps.dependents(wb.tab("T").id, formula.Pos{}, func(*fnode) {}, b)
		},
		"compacting": func(wb *Workbook, b *formula.Budget) { wb.deps.compact(b) },
	} {
		t.Run(name, func(t *testing.T) {
			for _, at := range []int64{1, blocks / 2, blocks - 2, -1} {
				wb, nodes := rangesSheet(t, formulas, refs)
				iv := wb.a1Column()
				b, writes := watchWrites(iv.sorted.backing(), at*mergeUnits)
				merge(wb, b)
				widest, after := writes()
				t.Logf("with the deadline at %d of %d blocks: %d entries changed after the stop, at most %d between two looks", at, blocks, after, widest)
				if stopped := at >= 0; b.Exceeded() != stopped {
					t.Fatalf("with the deadline at %d blocks: stopped %v", at, b.Exceeded())
				}
				if at >= 0 {
					if after != 0 {
						t.Errorf("with the deadline at %d blocks: %d entries changed after the look that found it", at, after)
					}
					if !wb.a1Column().loose {
						t.Errorf("with the deadline at %d blocks: the stopped column is not loose", at)
					}
				}
				if most := mostBetweenLooks(mergeUnits); widest > most {
					t.Errorf("with the deadline at %d blocks: %d entries changed between two looks, want at most %d", at, widest, most)
				}
				checkRanges(t, wb, nodes, refs)
			}
		})
	}
}

// TestCopyPiecesWritesBlocks copies a list with a take that looks at what
// was written so far each time it is asked. Each block is counted before
// it is written, and nothing is written after a refusal.
func TestCopyPiecesWritesBlocks(t *testing.T) {
	const n, block = 10_000, 512
	src := make([]int, n)
	for i := range src {
		src[i] = i + 1
	}
	for _, refuse := range []int{-1, 0, 3, n / block} {
		dst := make([]int, 0, n)
		backing := dst[:n]
		written := func() int {
			k := 0
			for _, v := range backing {
				if v != 0 {
					k++
				}
			}
			return k
		}
		calls, prev := 0, 0
		got, ok := appendPieces(dst, src, block, func(steps int) bool {
			now := written()
			if now-prev > block || steps > block {
				t.Fatalf("%d entries written between two counts, %d counted", now-prev, steps)
			}
			prev = now
			calls++
			return calls-1 != refuse
		})
		if refuse < 0 {
			if !ok || !slices.Equal(got, src) {
				t.Fatalf("the copy without a refusal: %v, %d entries", ok, len(got))
			}
			continue
		}
		if ok || written() != refuse*block {
			t.Fatalf("refused at block %d: %v, %d entries written, want %d", refuse, ok, written(), refuse*block)
		}
	}
}
