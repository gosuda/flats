package sheet

import (
	"fmt"
	"math/bits"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// TestCaptureOrdersReversedRowsQuickly captures 350 rows of 702 cells on
// two tabs under the read lock, which together hold almost the 500,000
// cells of a sheet. The default run has 200 rows. On one tab the columns a
// new tab lacks were inserted one at a time at the front, so each row lies
// in reverse order of its columns' ids, on the other at once at the end,
// so that their ids are in order. Putting a row in order of positions
// takes at most a few comparisons a cell for each doubling of the row
// (workSteps), for the reversed rows as for the ordered ones. Ordering
// them by insertion would compare each pair of cells of a reversed row,
// 245,000 comparisons a row.
func TestCaptureOrdersReversedRowsQuickly(t *testing.T) {
	slow(t, "writes hundreds of thousands of cells")
	rows := sized(350, 200)
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Ordered"},{"op":"add_tab","name":"Reversed"}]}`)
	widen := func(tab string, one bool) {
		e.in.mu.RLock()
		have := e.in.wb.tab(tab).cols.len()
		e.in.mu.RUnlock()
		if !one {
			e.mustAgent(jsonf(`{"ops":[{"op":"insert_cols","tab":%q,"at":%q,"count":%d}]}`, tab, formula.ColumnName(int32(have)), 702-have))
			return
		}
		op := jsonf(`{"op":"insert_cols","tab":%q,"at":"A","count":1},`, tab)
		e.mustAgent(`{"ops":[` + strings.TrimSuffix(strings.Repeat(op, 702-have), ",") + `]}`)
	}
	widen("Ordered", false)
	widen("Reversed", true)
	row := "[" + strings.TrimSuffix(strings.Repeat("1,", 702), ",") + "]"
	for _, tab := range []string{"Ordered", "Reversed"} {
		for from := 1; from <= rows; from += 50 {
			e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":%q,"at":"A%d","rows":[%s]}]}`, tab, from, strings.TrimSuffix(strings.Repeat(row+",", min(50, rows-from+1)), ",")))
		}
	}
	compared := map[string]int64{}
	for _, tab := range []string{"Ordered", "Reversed"} {
		steps := countWork(func() {
			page := e.read(ReadQuery{Tab: tab, Range: fmt.Sprintf("A1:ZZ%d", rows), Limit: 1})
			e.in.captures.clear()
			if got := fmt.Sprint(page["rows"]); got != "[[1]]" {
				t.Fatalf("the first page of %s holds %.40s", tab, got)
			}
		})
		compared[tab] = steps.ordered.Load()
	}
	cells := int64(rows) * 702
	t.Logf("a capture of %d rows of 702 cells compares %d times in order and %d times reversed", rows, compared["Ordered"], compared["Reversed"])
	for tab, n := range compared {
		if n < cells-int64(rows) || n > 2*cells*int64(bits.Len(702)) {
			t.Errorf("a capture of %d cells of %s compared %d times", cells, tab, n)
		}
	}
}
