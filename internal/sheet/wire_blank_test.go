package sheet

import "testing"

// Blank records hold style states of the default format, with the stamps
// of the action that took their last format away: a state for each such
// action. Between two ticks a sheet can hold more of them than the tick
// keeps. They must not let the format list keep formats no cell uses past
// its bound, so the list a snapshot copies stays within twice the states
// that carry a format and wireSlack more. Here the cells of Sheet1 drop
// 1,500 formats while 300 blank records hold states of their own. A
// snapshot taken with the blank records above the limit and no tick
// between, under a budget that leaves only what the tab's capture is
// charged at most with a list that size, gets its capture at once.
func TestBlankRecordsLeaveTheFormatListBounded(t *testing.T) {
	const states, formats = 300, 1500
	e := start(t)
	e.mustAgent(jsonf(`{"ops":[{"op":"add_tab","name":"Blanks"},{"op":"format","tab":"Blanks","ranges":["A1:A%d"],"format":{"bold":true}}]}`, states))
	for i := range states {
		e.mustAgent(jsonf(`{"ops":[{"op":"format","tab":"Blanks","ranges":["A%d"],"format":{"bold":false}}]}`, i+1))
		if i%50 == 49 {
			e.b.importAll(t) // the revisions of the actions, which the host imports
		}
	}
	e.symbolFormats(0, formats)
	e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:Z1000"],"formats":true}]}`)
	tab := e.tabID("Sheet1")

	// No cell carries a format, so the list holds at most the default
	// format and wireSlack more.
	most := wireSlack + 1
	e.in.mu.RLock()
	wb := e.in.wb
	size := captureSize{rows: int64(wb.byID[tab].rows.len()), cols: int64(wb.byID[tab].cols.len()), name: int64(len("Sheet1")),
		formats: int64(most), symbols: int64(12 * most), heads: int64(headTabsSize(wb)), snapshot: true}
	held := len(wb.styles.byID)
	e.in.mu.RUnlock()
	release := fillCaptures(t, size.bound())
	defer release()

	// The writer runs no tick while it runs this.
	var before, after, copied int
	var err error
	onWriter(e, func() {
		e.in.mu.Lock()
		wb.collected.limit = 10
		before = wb.count.blanks
		e.in.mu.Unlock()
		var src *snapSource
		if src, err = e.in.openSnapshot(tab, false, ""); err == nil {
			copied = len(src.enc.c.formats)
			src.close(e.in.hub)
		}
		e.in.mu.Lock()
		after = wb.count.blanks
		wb.collected.limit = 0
		e.in.mu.Unlock()
	})
	if before <= 10 || held < states || after != before {
		t.Fatalf("the sheet held %d blank records and then %d, and %d style states", before, after, held)
	}
	if err != nil {
		t.Fatalf("a snapshot with %d blank records above the limit: %v", before, err)
	}
	if copied > most {
		t.Errorf("the snapshot copies %d formats with no cell formatted, %d blank records and %d states", copied, before, held)
	}
}
