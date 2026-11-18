package sheet

import (
	"slices"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// clockedTxn starts a transaction on the sheet whose clock counts its
// readings and never finds the deadline passed. The caller holds the
// workbook's lock and takes the transaction back.
func (e *env) clockedTxn(by uint32) (*txn, *int) {
	wb := e.in.wb
	x := newTxn(wb, wb.seq+1, wb.seq, by)
	e.in.begin(x)
	reads := new(int)
	x.overdue = func() bool { *reads++; return false }
	x.budget = formula.NewBudget(x.overdue)
	return x, reads
}

// TestAxisBookkeepingReadsTheClock goes through the bookkeeping of the
// rows of a tab whose ids alternate in the born runs, on the pacer of an
// action: a deletion of half the rows, the stamps of ids that return, and
// the undo that brings them back. Each walk counts its ids and runs on the
// action's budget and reads the clock for every paceEvery of them, which
// the budget's own look every 4,096 units would not: a walk counts an
// eighth of a unit for each.
func TestAxisBookkeepingReadsTheClock(t *testing.T) {
	slow(t, "sorts and deletes 50,000 of 100,000 rows")
	rows := raced(formula.MaxRows, 8*paceEvery)
	half := rows / 2
	e := start(t)
	fragmentRows(e, rows)
	alice := e.person("Alice")

	func() {
		e.in.mu.Lock()
		defer e.in.mu.Unlock()
		x, reads := e.clockedTxn(alice.s.ord)
		defer func() { e.in.wb.revert(&x.j); e.in.wb.settle() }()
		tab := e.in.wb.tab("Sheet1")
		gone := slices.Clone(tab.rows.ids[:half])
		if err := x.takeIDs(tab, true, 0, gone, 0); err != nil {
			t.Fatalf("the deletion of %d rows: %v", half, err)
		}
		// The ids are put in order, looked up in the born runs, taken out
		// of them and out of the axis, and the rows left get their places.
		walked := 3*half + (rows - half)
		if *reads < walked/paceEvery {
			t.Errorf("the deletion of %d rows went through %d ids and runs and read the clock %d times", half, walked, *reads)
		}
	}()

	// The entry keeps the ids and the last sorts of the rows, over the
	// bound of a payload in memory. It stays there, so that the undo
	// reads no scratch file, whose decoding looks at the clock as well.
	e.in.mu.Lock()
	p := e.in.undoPool()
	p.entryMax, p.sessionMax, p.instanceMax = 64<<20, 64<<20, 64<<20
	e.in.mu.Unlock()
	if ack := alice.do(e.deleteRowsOp("Sheet1", 1, half)); status(ack) != "applied" {
		t.Fatalf("the deletion: %s", status(ack))
	}
	entry := alice.top(false)
	if entry == nil || entry.file != nil || len(entry.steps) != 1 || len(entry.steps[0].ids) != half {
		t.Fatal("the deletion left no entry in memory that holds its ids")
	}

	func() {
		e.in.mu.Lock()
		defer e.in.mu.Unlock()
		x, reads := e.clockedTxn(alice.s.ord)
		defer func() { e.in.wb.revert(&x.j); e.in.wb.settle() }()
		sorted, ok := sortedIDs(entry.steps[0].ids, nil)
		if !ok {
			t.Fatal("the ids could not be put in order")
		}
		if _, ok := stamped(sorted, x.seq, x.pace); !ok {
			t.Fatal("the stamps of the ids that return stopped without a deadline")
		}
		if *reads < half/paceEvery {
			t.Errorf("the stamps of %d ids that return read the clock %d times", half, *reads)
		}
	}()

	func() {
		e.in.mu.Lock()
		defer e.in.mu.Unlock()
		x, reads := e.clockedTxn(alice.s.ord)
		defer func() { e.in.wb.revert(&x.j); e.in.wb.settle() }()
		applied, skipped, _, err := x.applyUndo(&alice.s.undo, entry)
		if err != nil || applied != 1 || skipped != 0 {
			t.Fatalf("the undo of the deletion: applied %d, skipped %d, %v", applied, skipped, err)
		}
		// The undo puts the ids in order, stamps them, joins the stamps to
		// the born runs and gives every row its place.
		walked := 3*half + rows
		if *reads < walked/paceEvery {
			t.Errorf("the undo of the deletion of %d rows went through %d ids and runs and read the clock %d times", half, walked, *reads)
		}
	}()
	if u := undo(t, alice); u.Applied != 1 {
		t.Fatalf("the undo of the deletion = %+v", u)
	}
}
