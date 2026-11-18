package sheet

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// indexHeap returns the bytes of the heap that the dependency index of the
// sheet holds: what a collection leaves allocated with the index, less
// what it leaves once the index is let go. The index is stale afterwards,
// so the next action builds it again.
func (e *env) indexHeap() int64 {
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	with := liveHeap()
	e.in.wb.deps = newDepIndex()
	e.in.wb.deps.stale = true
	return with - liveHeap()
}

// retained returns the entries the parts of a list have room for.
func (l parted[E]) retained() int {
	n := cap(l.head)
	for _, part := range l.rest {
		n += cap(part)
	}
	return n
}

// TestCompactionLetsGoOfWhatTheIndexNoLongerNeeds grows a list of the
// index past one part with ordinary writes, lets a lookup put it in order,
// and shrinks it again, for one list after another, each keeping one
// reference. Every action stays under the cell count that builds the index
// in full. Afterwards the heap the index holds follows the references
// that are left, not the most each list held, and every lookup still finds
// its formula.
func TestCompactionLetsGoOfWhatTheIndexNoLongerNeeds(t *testing.T) {
	const temporaries, refs = 66, 1000 // 66,000 entries, past a part of 65,536
	lists := raced(16, 4)
	// A column of ranges: the temporary formulas each read the range
	// L1:L2 a thousand times, and Y holds one formula that reads it once.
	// A lookup of L1 puts the column in order and builds its tree.
	// Cells read singly: the temporary formulas each read L1 a thousand
	// times, and the formula in Y reads it once. Clearing the temporary
	// formulas drops the dead entries of the list of L1.
	for name, read := range map[string]func(col string) (temporary, kept string){
		"ranges": func(col string) (string, string) {
			return "=SUM(" + strings.TrimSuffix(strings.Repeat(col+"1:"+col+"2,", refs), ",") + ")", "=SUM(" + col + "1:" + col + "2)"
		},
		"singles": func(col string) (string, string) {
			return "=" + strings.TrimSuffix(strings.Repeat(col+"1+", refs), "+"), "=" + col + "1+0"
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := start(t, unhurried)
			for k := 0; k < lists; k++ {
				col := columnLetters(k)
				temporary, kept := read(col)
				rows := make([]string, temporaries)
				for i := range rows {
					rows[i] = jsonf("[%q]", temporary)
				}
				e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"Z1","rows":[%s]},{"op":"write","tab":"Sheet1","at":"Y%d","rows":[[%q]]}]}`,
					strings.Join(rows, ","), k+1, kept))
				e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"%s1","rows":[[%d]]}]}`, col, k+1))
				e.want(jsonf("Y%d", k+1), fmt.Sprint(k+1))
				tab, _, _ := e.ids(col + "1")
				e.in.mu.RLock()
				d := &e.in.wb.deps
				grown := 0
				if name == "ranges" {
					grown = d.ranges[colKey{tab, int32(k)}].maxLast.len()
				} else {
					grown = d.refs(posKey{tab, 0, int32(k)}).len()
				}
				stale := d.stale
				e.in.mu.RUnlock()
				// The list grew past a part, through the index of
				// content edits; a full build would say nothing.
				if stale || grown <= partLen {
					t.Fatalf("list %d: the index is stale=%v and holds %d entries", k, stale, grown)
				}
				blank := make([]string, temporaries)
				for i := range blank {
					blank[i] = `[""]`
				}
				e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"Z1","rows":[%s]},{"op":"write","tab":"Sheet1","at":"Y%d","rows":[[%q]]}]}`,
					strings.Join(blank, ","), k+1, kept+"+0"))
			}
			e.in.mu.RLock()
			d := &e.in.wb.deps
			// The clears drop the dead entries once they outnumber the
			// live ones and depSlack; fewer may stay.
			if d.stale || d.total-d.live > depSlack || d.live != int64(lists) {
				e.in.mu.RUnlock()
				t.Fatalf("the index is stale=%v and counts %d entries, %d live; want %d", d.stale, d.total, d.live, lists)
			}
			// Room the lists keep alive, in entries.
			room := 0
			for _, iv := range d.ranges {
				room += iv.sorted.retained() + iv.maxLast.retained() + cap(iv.tail)
			}
			for key := range d.singles {
				room += d.refs(key).retained()
			}
			dead := int(d.total - d.live)
			e.in.mu.RUnlock()
			t.Logf("the lists keep room for %d entries, %d of them dead", room, dead)
			// A list that grows starts with room for eight, and a dead
			// entry keeps its place and that of its column's tree.
			if room > 8*lists+2*dead {
				t.Errorf("the lists of %d references and %d dead entries keep room for %d entries", lists, dead, room)
			}
			// Every formula still follows its cell.
			for k := 0; k < lists; k++ {
				col := columnLetters(k)
				e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"%s1","rows":[[%d]]}]}`, col, 100+k))
				e.want(jsonf("Y%d", k+1), fmt.Sprint(100+k))
			}
			if !measures {
				return
			}
			// A part of a list is 256 KiB at the least; the index holds a
			// few hundred bytes for each reference left.
			held, limit := e.indexHeap(), int64(64<<10+lists*2<<10)
			t.Logf("for %d references the index holds %d bytes of heap", lists, held)
			if held > limit {
				t.Errorf("for %d references the index holds %d KiB of heap, more than %d KiB", lists, held>>10, limit>>10)
			}
		})
	}
}

// TestCompactionShrinksTheMapsOfTheIndex writes formulas that read tens
// of thousands of cells singly, then replaces them by one formula of one
// reference. The compaction that follows drops the lists of those cells,
// and the map that found them lets go of their places as well.
func TestCompactionShrinksTheMapsOfTheIndex(t *testing.T) {
	e := start(t, unhurried)
	const formulas = 25 // a thousand cells each, in rows 1 to 1,000 of A to Y
	rows := make([]string, formulas)
	for f := range rows {
		var b strings.Builder
		b.WriteString("=0")
		for r := 1; r <= 1000; r++ {
			fmt.Fprintf(&b, "+%s%d", columnLetters(f), r)
		}
		rows[f] = jsonf("[%q]", b.String())
	}
	e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"Z1","rows":[%s]}]}`, strings.Join(rows, ",")))
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[5]]}]}`)
	e.want("Z1", "5")
	e.in.mu.RLock()
	keys, stale := len(e.in.wb.deps.singles), e.in.wb.deps.stale
	e.in.mu.RUnlock()
	if stale || keys != formulas*1000 {
		t.Fatalf("the index is stale=%v and lists %d cells", stale, keys)
	}
	blank := make([]string, formulas)
	for i := range blank {
		blank[i] = `[""]`
	}
	e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"Z1","rows":[%s]},{"op":"write","tab":"Sheet1","at":"Z100","rows":[["=A1"]]}]}`, strings.Join(blank, ",")))
	e.in.mu.RLock()
	keys = len(e.in.wb.deps.singles)
	e.in.mu.RUnlock()
	if keys != 1 {
		t.Fatalf("after the compaction the index lists %d cells", keys)
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[7]]}]}`)
	e.want("Z100", "7")
	if !measures {
		return
	}
	// A map of 25,000 places takes over a megabyte.
	held := e.indexHeap()
	t.Logf("for one reference the index holds %d bytes of heap", held)
	if held > 64<<10 {
		t.Errorf("for one reference the index holds %d KiB of heap", held>>10)
	}
}

// TestShrinkingMergeStops stops the merge of a column that shrinks to a
// thirty-second of its entries after a lookup built its tree, at points
// from the start to the end, in a compaction and in a lookup. Between two
// looks it allocates no more than a part, after the look that stops it
// little, and the column holds every live entry. A merge that ends leaves
// the column and its tree with room for the live entries and no more.
func TestShrinkingMergeStops(t *testing.T) {
	const refs = 800
	formulas := sized(6000, 1500)
	build := func() (*Workbook, []*fnode) {
		wb, nodes := rangesSheet(t, formulas, refs)
		wb.deps.dependents(wb.tab("T").id, formula.Pos{}, func(*fnode) {}, nil)
		for i, n := range nodes {
			if i%2 == 0 && i%64 != 0 {
				wb.unregister(n)
			}
		}
		return wb, nodes
	}
	check := func(t *testing.T, wb *Workbook, nodes []*fnode, whole bool) {
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
		want := 0
		for i, n := range nodes {
			if i%64 == 0 {
				want++
				if live[n] != refs {
					t.Fatalf("formula %d has %d live entries, want %d", i, live[n], refs)
				}
			}
		}
		if size := wb.deps.size(); wb.deps.total != size {
			t.Fatalf("the index counts %d entries and holds %d", wb.deps.total, size)
		}
		if whole {
			if room, kept := iv.sorted.retained()+iv.maxLast.retained(), 2*iv.sorted.len(); iv.loose || room != kept {
				t.Fatalf("the merged column is loose=%v and keeps room for %d entries and their tree, want %d", iv.loose, room, kept)
			}
		}
		found := map[*fnode]bool{}
		wb.deps.dependents(wb.tab("T").id, formula.Pos{}, func(n *fnode) { found[n] = true }, nil)
		if len(found) != want {
			t.Fatalf("a lookup of A1 finds %d formulas, want %d", len(found), want)
		}
	}
	for name, merge := range map[string]func(wb *Workbook, b *formula.Budget){
		"looking up": func(wb *Workbook, b *formula.Budget) {
			wb.a1Column().loose = true
			wb.deps.dependents(wb.tab("T").id, formula.Pos{}, func(*fnode) {}, b)
		},
		"compacting": func(wb *Workbook, b *formula.Budget) { wb.deps.compact(b) },
	} {
		t.Run(name, func(t *testing.T) {
			wb, _ := build()
			whole := formula.NewBudget(nil)
			merge(wb, whole)
			for _, limit := range []int64{whole.Used() / 5, whole.Used() / 2, whole.Used() - budgetCheck, -1} {
				wb, nodes := build()
				b, allocs := watchAllocs(atUnits(limit))
				merge(wb, b)
				widest, after := allocs()
				t.Logf("with the deadline at %d of %d units: %d KiB allocated after the stop, at most %d KiB between two looks", limit, whole.Used(), after>>10, widest>>10)
				if stopped := limit >= 0; b.Exceeded() != stopped {
					t.Fatalf("with the deadline at %d units: stopped %v", limit, b.Exceeded())
				}
				if measures && (widest > mostAllocsBetweenLooks || limit >= 0 && after > 1<<20) {
					t.Errorf("with the deadline at %d units: %d KiB allocated after the stop, %d KiB between two looks", limit, after>>10, widest>>10)
				}
				check(t, wb, nodes, limit < 0)
			}
		})
	}
}

// TestMapShrinkStops compacts an index whose map of single references
// lost nine in ten of its 400,000 cells, stopped at points from the start
// to the end, also while the map is copied. A stopped compaction leaves an
// index that counts what it holds and answers as before; one that ends
// keeps a map of the cells left.
func TestMapShrinkStops(t *testing.T) {
	const cells = 400_000
	build := func() (*depIndex, *fnode) {
		d := newDepIndex()
		dead, live := &fnode{}, &fnode{}
		for row := int32(0); row < cells; row++ {
			n := dead
			if row%10 == 0 {
				n = live
			}
			d.setRefs(posKey{1, row, 0}, parted[depRef]{head: []depRef{{n, 0}}})
		}
		dead.gen++
		d.total, d.live = cells, cells/10
		return &d, live
	}
	d, _ := build()
	whole := formula.NewBudget(nil)
	d.compact(whole)
	if len(d.singles) != cells/10 || d.keys.singles != cells/10 {
		t.Fatalf("after the compaction the map holds %d cells and counts %d", len(d.singles), d.keys.singles)
	}
	// The copy of the map takes the last third of the units or more, so
	// the last two stops fall into it.
	if copied := int64(cells/10*mapKeySteps+cells/8) / 64; copied < whole.Used()/3 {
		t.Fatalf("the copy counts %d of %d units", copied, whole.Used())
	}
	for _, limit := range []int64{whole.Used() / 5, whole.Used() / 2, whole.Used() * 3 / 4, whole.Used() - budgetCheck, -1} {
		d, live := build()
		b, allocs := watchAllocs(atUnits(limit))
		d.compact(b)
		widest, after := allocs()
		if stopped := limit >= 0; b.Exceeded() != stopped {
			t.Fatalf("with the deadline at %d units: stopped %v", limit, b.Exceeded())
		}
		if measures && (widest > mostAllocsBetweenLooks || limit >= 0 && after > 1<<20) {
			t.Errorf("with the deadline at %d units: %d KiB allocated after the stop, %d KiB between two looks", limit, after>>10, widest>>10)
		}
		if size := d.size(); d.total != size {
			t.Fatalf("with the deadline at %d units: the index counts %d entries and holds %d", limit, d.total, size)
		}
		for _, p := range []formula.Pos{{Row: 0}, {Row: 7}, {Row: cells - 10}} {
			var found []*fnode
			d.dependents(1, p, func(n *fnode) { found = append(found, n) }, nil)
			if want := p.Row%10 == 0; len(found) != 1 && want || len(found) == 1 && (!want || found[0] != live) || len(found) > 1 {
				t.Fatalf("with the deadline at %d units: a lookup of row %d finds %d formulas", limit, p.Row, len(found))
			}
		}
	}
}
