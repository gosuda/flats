package sheet

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// runSort sorts every row of Sheet1 by column A in a transaction of its own
// with the given budget, and takes the transaction back.
func runSort(e *env, b *formula.Budget) *WriteError {
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	wb := e.in.wb
	t := wb.tab("Sheet1")
	x := newTxn(wb, wb.seq+1, wb.seq, 0)
	e.in.begin(x)
	x.overdue = nil
	x.budget = b
	positions := make([]int32, len(t.rows.ids))
	for i := range positions {
		positions[i] = int32(i)
	}
	err := x.sortRows(t, positions, ColID(t.cols.ids[0]), false)
	wb.revert(&x.j)
	wb.settle()
	return err
}

// TestSortReadsTheClockAsItGoes sorts 100,000 rows whose keys are runs of
// 20 ascending numbers, the runs in descending order. Each comparison and
// each place the sort writes counts on the budget, which looks at the
// deadline every 4,096 units: no stretch between two looks counts more than
// a tenth of the sort's units, so a sort that held its counts back and
// handed them on at the end would show as a long stretch. With the
// deadline passed it stops within a few hundred units, and the refused
// sort leaves the sheet as it was. Without a deadline it puts the rows in
// order.
func TestSortReadsTheClockAsItGoes(t *testing.T) {
	slow(t, "sorts 100,000 rows")
	n := raced(formula.MaxRows, formula.MaxRows/10)
	e := start(t)
	key := func(i int) int { return (n/20-1-i/20)*20 + i%20 }
	for first := 0; first < n; first += 10000 {
		rows := make([]string, min(10000, n-first))
		for i := range rows {
			rows[i] = "[" + strconv.Itoa(key(first+i)) + "]"
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, first+1, strings.Join(rows, ",")))
	}

	// The first sort also brings the calculation up to date.
	runSort(e, nil)
	var looks []int64
	var b *formula.Budget
	b = formula.NewBudget(func() bool { looks = append(looks, b.Used()); return false })
	if err := runSort(e, b); err != nil {
		t.Fatal(err)
	}
	sorted := budgetWork{used: b.Used(), looks: looks}
	longest := sorted.longest()
	t.Logf("the sort counted %d units, at most %d between two looks at the deadline", sorted.used, longest)
	if longest > sorted.used/10 {
		t.Errorf("the sort counted %d of its %d units between two looks at the deadline", longest, sorted.used)
	}
	// Each comparison is a unit and each place written a quarter of one: a
	// merge sort of these rows makes about n log n of both.
	if least := int64(n) * int64(bits.Len(uint(n))) / 2; sorted.used < least {
		t.Errorf("the sort of %d rows counted %d units, for at least %d", n, sorted.used, least)
	}

	var whole int64
	{
		b := unitBudget(1<<62, nil)
		if err := runSort(e, b); err != nil {
			t.Fatal(err)
		}
		whole = b.Used()
	}
	stops := 0
	before := e.dump()
	for deadline := whole / 8; deadline < whole; deadline += whole / 8 {
		var found int64 = -1
		b := unitBudget(deadline, &found)
		err := runSort(e, b)
		if err == nil {
			continue
		}
		if err.Limit != "recalculation_time" {
			t.Fatalf("the sort stopped after %d units: %v", deadline, err)
		}
		stops++
		if after := b.Used() - found; after > 256 {
			t.Errorf("the sort with the deadline at %d units of %d counted %d units after the budget found it passed", deadline, whole, after)
		}
		if after := e.dump(); after != before {
			t.Fatalf("the refused sort changed the sheet: %s", dumpDiff(before, after))
		}
	}
	if stops < 5 {
		t.Fatalf("the sort of %d units stopped %d times, want at least 5", whole, stops)
	}

	alice := e.person("Alice")
	if ack := alice.do(e.sortOp("Sheet1", 1, n, 1, false)); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	shown := e.column("Sheet1", "A", 1, n)
	for i, v := range shown {
		if v != strconv.Itoa(i) {
			t.Fatalf("after the sort row %d shows %s", i+1, v)
		}
	}
}
