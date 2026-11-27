package sheet

import (
	"context"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// watchAllocs returns a budget whose deadline passes once stop reports
// true at a look. At each look it notes the bytes the heap allocated since
// the look before. allocs returns the most bytes allocated between two
// looks, the end of the work included, and those allocated after the last
// look, which is the look that found the deadline passed when there was
// one.
func watchAllocs(stop func(b *formula.Budget) bool) (b *formula.Budget, allocs func() (widest, after int64)) {
	last := allocated()
	var widest int64
	b = formula.NewBudget(func() bool {
		now := allocated()
		widest = max(widest, now-last)
		last = now
		return stop(b)
	})
	return b, func() (int64, int64) {
		after := allocated() - last
		return max(widest, after), after
	}
}

// mostAllocsBetweenLooks is the most bytes the index may allocate between
// two looks at the deadline: a part of a list made at its full length, or
// the copy of the first part, which holds up to partLen entries, as it
// grows, and room for the nodes and maps of the formulas in between.
const mostAllocsBetweenLooks = 4 << 20

// atUnits returns a stop at units of the budget, or none for a negative
// limit.
func atUnits(limit int64) func(*formula.Budget) bool {
	return func(b *formula.Budget) bool { return limit >= 0 && b.Used() >= limit }
}

// TestBuildGrowsListsAPartAtATime builds the index of a sheet whose
// formulas name one cell 9,828,000 times, and of one whose formulas put
// 4,800,000 ranges in one column. The default run has a tenth and a
// quarter of them. A list grows a part at a time, so between two looks at
// the deadline the build allocates no more than a part, and a deadline
// found on the way ends the build there: after that look it allocates
// nothing more than the empty index it leaves.
func TestBuildGrowsListsAPartAtATime(t *testing.T) {
	sheets := map[string]func() *Workbook{
		"singles": func() *Workbook { wb, _ := singlesSheet(t, sized(6000, 600), 1638); return wb },
		"ranges":  func() *Workbook { wb, _ := rangesSheet(t, sized(6000, 1500), 800); return wb },
	}
	for name, sheet := range sheets {
		t.Run(name, func(t *testing.T) {
			wb := sheet()
			whole, allocs := watchAllocs(atUnits(-1))
			if err := wb.buildDeps(whole); err != nil {
				t.Fatal(err)
			}
			widest, _ := allocs()
			parts := int64(wb.deps.size()) / partLen
			t.Logf("a whole build of %d entries in about %d parts: %d units, at most %d KiB allocated between two looks", wb.deps.size(), parts, whole.Used(), widest>>10)
			if parts < 8 {
				t.Fatalf("the index holds %d entries, less than eight parts", wb.deps.size())
			}
			if widest > mostAllocsBetweenLooks {
				t.Errorf("the build allocated %d KiB between two looks, want at most %d", widest>>10, mostAllocsBetweenLooks>>10)
			}
			for _, part := range []float64{0.1, 0.5, 0.95} {
				limit := int64(part * float64(whole.Used()))
				b, allocs := watchAllocs(atUnits(limit))
				err := wb.buildDeps(b)
				widest, after := allocs()
				t.Logf("with the deadline at %d of %d units: %d KiB allocated after the stop, at most %d KiB between two looks", limit, whole.Used(), after>>10, widest>>10)
				if err == nil || err.Limit != "recalculation_time" {
					t.Fatalf("with the deadline at %d units: %v", limit, err)
				}
				if after > 1<<20 || widest > mostAllocsBetweenLooks {
					t.Errorf("with the deadline at %d units: %d KiB allocated after the stop, %d KiB between two looks", limit, after>>10, widest>>10)
				}
			}
		})
	}
}

// TestGrowingListHandsTheGateOn builds the index of the sheet of 9,828,000
// references to one cell as a pass at a gate of one slot. Once the list
// holds two parts another pass comes to wait in line. The build hands the
// slot on at its next check point, with its list half grown, the waiting
// pass gets it, and a cancellation that follows ends the build at the
// check point after.
func TestGrowingListHandsTheGateOn(t *testing.T) {
	formulas := sized(6000, 600)
	wb, _ := singlesSheet(t, formulas, 1638)
	g := newCalcGate(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turn, ok := g.enter(nil, ctx)
	if !ok {
		t.Fatal("the build got no slot")
	}
	defer turn.leave()
	key := posKey{wb.tab("T").id, 0, 0}
	got := make(chan int, 1)
	wait := func() {
		other, ok := g.enter(nil, context.Background())
		if !ok {
			got <- -1
			return
		}
		// The build waits in line while this pass holds the slot, so the
		// list is as the build left it.
		got <- wb.deps.refs(key).len()
		other.leave()
	}
	var waiting bool
	handed, entries := 0, 0
	b, allocs := watchAllocs(func(*formula.Budget) bool {
		if !waiting && wb.deps.refs(key).len() >= 2*partLen {
			waiting = true
			go wait()
			for g.waiting.Load() == 0 {
				time.Sleep(time.Millisecond)
			}
			// The build has held the slot for its quantum by now.
			time.Sleep(calcQuantum)
		}
		if !turn.check() {
			return true
		}
		select {
		case entries = <-got:
			handed++
			cancel()
		default:
		}
		return false
	})
	err := wb.buildDeps(b)
	widest, after := allocs()
	t.Logf("the waiting pass got the slot with %d entries of the list made; the build allocated at most %d KiB between two looks and %d KiB after the stop", entries, widest>>10, after>>10)
	if handed != 1 || entries < 2*partLen || entries >= 1638*formulas {
		t.Fatalf("the waiting pass got the slot %d times, with %d entries made", handed, entries)
	}
	if err == nil || err.Limit != "recalculation_time" {
		t.Fatalf("the cancelled build: %v", err)
	}
	if after > 1<<20 || widest > mostAllocsBetweenLooks {
		t.Errorf("%d KiB allocated after the stop, %d KiB between two looks", after>>10, widest>>10)
	}
}

// TestCompactionAllocatesAPartAtMost compacts an index whose dead entries
// lie in one long list of single references, and one whose dead entries
// lie in one column of ranges, half of either dead. The parts that the
// dead entries took are let go and only the last part left is copied, so
// between two looks the compaction allocates no more than a part, also
// when it stops on the way, and it allocates nothing after the look that
// stops it. The stopped index still holds every live entry.
func TestCompactionAllocatesAPartAtMost(t *testing.T) {
	const singles, ranges = 1638, 800
	sw, singleNodes := singlesSheet(t, sized(6000, 600), singles)
	rw, rangeNodes := rangesSheet(t, sized(6000, 1500), ranges)
	for name, c := range map[string]struct {
		wb    *Workbook
		check func(wb *Workbook)
	}{
		"singles": {sw, func(wb *Workbook) { checkSingles(t, wb, singleNodes, singles) }},
		"ranges":  {rw, func(wb *Workbook) { checkRanges(t, wb, rangeNodes, ranges) }},
	} {
		t.Run(name, func(t *testing.T) {
			whole := formula.NewBudget(nil)
			cloneIndex(&c.wb.deps).compact(whole)
			// The last stop comes where a look still follows.
			for _, limit := range []int64{whole.Used() / 5, whole.Used() / 2, whole.Used() - budgetCheck, -1} {
				d := cloneIndex(&c.wb.deps)
				b, allocs := watchAllocs(atUnits(limit))
				d.compact(b)
				widest, after := allocs()
				t.Logf("with the deadline at %d of %d units: %d KiB allocated after the stop, at most %d KiB between two looks", limit, whole.Used(), after>>10, widest>>10)
				if stopped := limit >= 0; b.Exceeded() != stopped {
					t.Fatalf("with the deadline at %d units: stopped %v", limit, b.Exceeded())
				}
				if widest > mostAllocsBetweenLooks || limit >= 0 && after > 1<<20 {
					t.Errorf("with the deadline at %d units: %d KiB allocated after the stop, %d KiB between two looks", limit, after>>10, widest>>10)
				}
				saved := c.wb.deps
				c.wb.deps = *d
				c.check(c.wb)
				c.wb.deps = saved
			}
		})
	}
}
