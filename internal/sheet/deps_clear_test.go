package sheet

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// clearRefs is the number of times each formula of clearingSheet reads A1.
const clearRefs = 1000

// clearingSheet writes n formulas into B1 and the cells below that each
// read A1 a thousand times, and =A1*2 into C1, in a call over the cells
// that build the index in full. The list of A1 then holds a thousand live
// entries for each formula and one for C1.
func clearingSheet(t *testing.T, n int) *env {
	t.Helper()
	e := start(t, unhurried)
	f := "=" + strings.TrimSuffix(strings.Repeat("A1+", clearRefs), "+")
	rows := make([]string, n)
	for i := range rows {
		rows[i] = jsonf("[%q]", f)
	}
	ops := []string{
		`{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]]}`,
		jsonf(`{"op":"write","tab":"Sheet1","at":"B1","rows":[%s]}`, strings.Join(rows, ",")),
		`{"op":"write","tab":"Sheet1","at":"C1","rows":[["=A1*2"]]}`,
	}
	if pad := fullDirtyCells + 1 - n; pad > 0 {
		ops = append(ops, jsonf(`{"op":"write","tab":"Sheet1","at":"D1","rows":[%s]}`, strings.TrimSuffix(strings.Repeat("[0],", pad), ",")))
	}
	e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	e.in.mu.RLock()
	d := &e.in.wb.deps
	stale, total, live := d.stale, d.total, d.live
	e.in.mu.RUnlock()
	if want := int64(n*clearRefs + 1); stale || total != want || live != want {
		t.Fatalf("the index is stale=%v and counts %d entries, %d live; want %d", stale, total, live, want)
	}
	return e
}

// clearCalls returns the calls that clear the formulas of clearingSheet
// in two parts, each under the cells that build the index in full.
func clearCalls(n int) []string {
	part := func(from, to int) string {
		return jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"B%d","rows":[%s]}]}`, from, strings.TrimSuffix(strings.Repeat(`[""],`, to-from+1), ","))
	}
	half := (n + 1) / 2
	return []string{part(1, half), part(half+1, n)}
}

// a1Room returns the entries the list of A1 has room for, and what the
// index counts.
func (e *env) a1Room() (room int, total, live int64, stale bool) {
	tab, _, _ := e.ids("A1")
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	d := &e.in.wb.deps
	return d.refs(posKey{tab, 0, 0}).retained(), d.total, d.live, d.stale
}

// TestClearingFormulasDropsTheirEntries writes formulas that read A1 a
// thousand times each and clears them again in two calls, each under the
// cell count that builds the index in full, with no formula written
// after them. Each call that leaves more dead entries than live ones and
// 1,024 drops them itself: the index and the room its list keeps follow
// the one formula that is left, and every lookup still finds it.
func TestClearingFormulasDropsTheirEntries(t *testing.T) {
	n := sized(1001, raced(301, 101))
	e := clearingSheet(t, n)
	for i, call := range clearCalls(n) {
		e.mustAgent(call)
		room, total, live, stale := e.a1Room()
		t.Logf("after clearing part %d: the index counts %d entries, %d live, and the list of A1 has room for %d", i+1, total, live, room)
		if stale || total-live > max(live, depSlack) {
			t.Fatalf("after clearing part %d: the index is stale=%v and counts %d entries, %d live", i+1, stale, total, live)
		}
		if room > int(2*live)+depSlack {
			t.Fatalf("after clearing part %d: the list of A1 keeps room for %d entries, %d live", i+1, room, live)
		}
	}
	e.in.mu.RLock()
	if size := e.in.wb.deps.size(); e.in.wb.deps.total != size {
		e.in.mu.RUnlock()
		t.Fatalf("the index counts %d entries and holds %d", e.in.wb.deps.total, size)
	}
	e.in.mu.RUnlock()
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[21]]}]}`)
	e.want("C1", "42")
	e.want("B1", "")
	e.want(fmt.Sprintf("B%d", n), "")
	if _, _, _, stale := e.a1Room(); stale {
		t.Fatal("the lookup left the index stale")
	}
	if !measures {
		return
	}
	// The thousand references of a cleared formula take 16 KiB.
	held := e.indexHeap()
	t.Logf("for one reference the index holds %d bytes of heap", held)
	if held > 128<<10 {
		t.Errorf("for one reference the index holds %d KiB of heap", held>>10)
	}
}

// TestClearingFormulasStopsInTheCompaction clears the first part of the
// formulas of clearingSheet with the deadline passing at a look after
// the cleared formulas left more dead entries than live ones, while
// their compaction goes through the list of A1. The clear is refused and
// taken back whole, the index is built again for the next action, which
// finds every formula, and the clear goes through afterwards.
func TestClearingFormulasStopsInTheCompaction(t *testing.T) {
	// The compaction goes through 601,000 entries or more, over two
	// looks at the deadline and a block, so some deadline falls inside.
	n := sized(1001, 601)
	e := clearingSheet(t, n)
	call := clearCalls(n)[0]
	partly := false
	for looks := 1; ; looks++ {
		before, seq := e.dump(), e.seq()
		u, err := DecodeUpdate([]byte(strings.Replace(call, `{"ops"`, jsonf(`{"state":%q,"ops"`, e.state()), 1)))
		if err != nil {
			t.Fatal(err)
		}
		werr, dropped, left := func() (*WriteError, bool, bool) {
			e.in.mu.Lock()
			defer e.in.mu.Unlock()
			wb := e.in.wb
			d := &wb.deps
			x := newTxn(wb, wb.seq+1, wb.seq, 0)
			e.in.begin(x)
			x.overdue = nil
			seen := 0
			x.budget = formula.NewBudget(func() bool {
				if d.total-d.live > max(d.live, depSlack) {
					seen++
				}
				return seen >= looks
			})
			total := d.total
			werr := x.applyAgent(&u)
			if werr == nil {
				werr = x.calc()
			}
			// A compaction that stopped dropped some of the dead entries.
			dropped, left := d.total < total, d.total > d.live
			wb.revert(&x.j)
			wb.settle()
			return werr, dropped, left
		}()
		if werr == nil {
			t.Logf("the clear goes through with %d looks after the dead entries outnumbered the live ones", looks-1)
			break
		}
		if werr.Limit != "recalculation_time" {
			t.Fatalf("the clear stopped at look %d: %v", looks, werr)
		}
		partly = partly || dropped && left
		if after := e.dump(); after != before || e.seq() != seq {
			t.Fatalf("the clear stopped at look %d changed the sheet: %s", looks, dumpDiff(before, after))
		}
		// The next action builds the index again and finds every formula.
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[%d]]}]}`, looks+1))
		e.want("B1", fmt.Sprint(clearRefs*(looks+1)))
		e.want(fmt.Sprintf("B%d", n), fmt.Sprint(clearRefs*(looks+1)))
		e.want("C1", fmt.Sprint(2*(looks+1)))
		if looks > 64 {
			t.Fatal("the clear never went through")
		}
	}
	if !partly {
		t.Fatal("no deadline stopped the compaction part way through the list of A1")
	}
	for _, call := range clearCalls(n) {
		e.mustAgent(call)
	}
	room, total, live, stale := e.a1Room()
	if stale || live != 1 || total-live > depSlack || room > 2+depSlack {
		t.Fatalf("after the clears the index is stale=%v and counts %d entries, %d live, with room for %d", stale, total, live, room)
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[7]]}]}`)
	e.want("C1", "14")
	if measures {
		if held := e.indexHeap(); held > 128<<10 {
			t.Errorf("for one reference the index holds %d KiB of heap", held>>10)
		}
	}
}
