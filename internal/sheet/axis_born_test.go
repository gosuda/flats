package sheet

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// fragmentRows grows Sheet1 to rows rows and sorts them so that the rows
// of even id come first, in descending id order, and the rows of odd id
// follow. Deleting the first half then takes every other id out of the
// born runs, from the largest down, and each one splits a run in two.
func fragmentRows(e *env, rows int) {
	e.t.Helper()
	e.mustAgent(jsonf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":%d,"count":%d}]}`, newTabRows+1, rows-newTabRows))
	e.in.mu.RLock()
	ids := slices.Clone(e.in.wb.tab("Sheet1").rows.ids)
	e.in.mu.RUnlock()
	// A call writes at most maxCallCells cells.
	for first := 0; first < len(ids); first += maxCallCells {
		var b strings.Builder
		for i, id := range ids[first:min(first+maxCallCells, len(ids))] {
			if i > 0 {
				b.WriteByte(',')
			}
			key := int64(id)
			if id%2 == 0 {
				key = -key
			} else {
				key += 1 << 40
			}
			fmt.Fprintf(&b, "[%d]", key)
		}
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, first+1, b.String()))
	}
	// The keys go with their column, so that the rows hold no cells: the
	// deletion's own work is then mostly its bookkeeping of the axis.
	e.mustAgent(jsonf(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:%d","by":"A"},{"op":"delete_cols","tab":"Sheet1","at":"A","count":1}]}`, rows))
}

// rowAxis returns the row ids of Sheet1 in position order with the born
// stamp of each, and its born runs.
func rowAxis(e *env) (ids []uint64, born map[uint64]uint64, runs runStamps) {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	a := &e.in.wb.tab("Sheet1").rows
	born = make(map[uint64]uint64, len(a.ids))
	for _, id := range a.ids {
		born[id] = a.born.get(id)
	}
	return slices.Clone(a.ids), born, slices.Clone(a.born)
}

// wantRuns checks that born runs are sorted, apart, not empty, stamp no id
// twice and none that the axis does not hold.
func wantRuns(t *testing.T, what string, ids []uint64, runs runStamps) {
	t.Helper()
	var n uint64
	for i, r := range runs {
		if r.count == 0 || r.seq == 0 || (i > 0 && runs[i-1].first+runs[i-1].count > r.first) {
			t.Fatalf("%s: born run %d %+v after %+v", what, i, r, runs[max(i-1, 0)])
		}
		n += r.count
	}
	if n != uint64(len(ids)) {
		t.Fatalf("%s: the born runs stamp %d ids, the axis holds %d", what, n, len(ids))
	}
}

// TestDeletingFragmentedRowsStaysLinear has Alice delete 50,000 of 100,000
// rows whose ids alternate in the born runs, from the largest down: each
// one splits a run in two. The deletion keeps the order and the born stamps
// of every other row, stops at its deadline in its bookkeeping and in the
// rollback of its refusal, and its undo brings every row back in place.
func TestDeletingFragmentedRowsStaysLinear(t *testing.T) {
	slow(t, "sorts and deletes 50,000 of 100,000 rows")
	rows := raced(formula.MaxRows, formula.MaxRows/2)
	e := start(t, unhurried)
	fragmentRows(e, rows)
	alice := e.person("Alice")
	ids, born, runs := rowAxis(e)
	wantRuns(t, "before", ids, runs)
	half := rows / 2
	for _, id := range ids[:half] {
		if id%2 != 0 {
			t.Fatalf("row id %d among the first half", id)
		}
	}
	del := e.deleteRowsOp("Sheet1", 1, half)
	deadline := e.workDeadline()

	got, whole := deadline.run(never, func() string { return status(alice.do(del)) })
	if got != "applied" {
		t.Fatalf("the deletion: %s", got)
	}
	after, afterBorn, afterRuns := rowAxis(e)
	wantRuns(t, "after the deletion", after, afterRuns)
	if !slices.Equal(after, ids[half:]) {
		t.Fatal("the deletion changed the order of the rows it left")
	}
	for _, id := range after {
		if afterBorn[id] != born[id] {
			t.Fatalf("row %d was born at %d and is at %d after the deletion", id, born[id], afterBorn[id])
		}
	}

	if u := undo(t, alice); u.Applied != 1 {
		t.Fatalf("the undo of the deletion = %+v", u)
	}
	back, backBorn, backRuns := rowAxis(e)
	wantRuns(t, "after the undo", back, backRuns)
	if !slices.Equal(back, ids) {
		t.Fatal("the undo did not bring the rows back in place")
	}
	for _, id := range ids[half:] {
		if backBorn[id] != born[id] {
			t.Fatalf("row %d was born at %d and is at %d after the undo", id, born[id], backBorn[id])
		}
	}

	// With its deadline at a quarter of that work, the deletion is refused
	// and leaves the sheet, its born runs and the undo stacks as they were.
	before, kept := e.dump(), e.memory("Sheet1", alice)
	deadline.stopsAtDeadline(t, whole, func() string { return status(alice.do(del)) })
	if got := e.dump(); got != before {
		t.Fatalf("the refused deletion changed the sheet: %s", dumpDiff(before, got))
	}
	if !kept.equal(e.memory("Sheet1", alice)) {
		t.Fatal("the refused deletion changed the undo stacks or the structural log")
	}
	// After the refusals the deletion still applies.
	if got, _ := deadline.run(never, func() string { return status(alice.do(del)) }); got != "applied" {
		t.Fatalf("the deletion after its refusals: %s", got)
	}
	if got, _, _ := rowAxis(e); !slices.Equal(got, ids[half:]) {
		t.Fatal("the deletion after its refusals left other rows")
	}
}
