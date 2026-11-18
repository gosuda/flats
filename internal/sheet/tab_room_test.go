package sheet

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// tabSpare returns the heap that the maps and arrays of the sheet's tabs
// and its style states keep beyond what they hold: what the heap lets go when each is made
// again for what it holds now.
func tabSpare(t *testing.T, e *env) int64 {
	t.Helper()
	e.b.importAll(t)
	e.b.forgetRevisions()
	held := undoHeap(t, e)
	e.in.mu.Lock()
	styles := &e.in.wb.styles
	styles.byID, _ = compactMap(styles.byID, &ticker{})
	styles.ids, _ = compactMap(styles.ids, &ticker{})
	styles.touched, _ = compactMap(styles.touched, &ticker{})
	for _, tab := range e.in.wb.tabs {
		for _, a := range []*axis{&tab.rows, &tab.cols} {
			fresh, _ := a.compacted(&ticker{})
			*a = fresh
		}
		tab.data, _ = compactMap(tab.data, &ticker{})
		tab.widths, _ = compactMap(tab.widths, &ticker{})
		tab.lastSort, _ = compactMap(tab.lastSort, &ticker{})
		for _, rd := range tab.data {
			rd.cells = slices.Clone(rd.cells)
		}
		tab.chunks = slices.Clone(tab.chunks)
		tab.byPos, tab.byPosOK = nil, false
		tab.rowsByPos()
	}
	e.in.mu.Unlock()
	return held - undoHeap(t, e)
}

// tabContents renders what the reads of a tab see: every row with its
// cells, in order, and the tab's widths and row files.
func tabContents(e *env, name string) string {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	tab := e.in.wb.tab(name)
	var b strings.Builder
	for i, rd := range tab.rowsByPos() {
		if rd == nil {
			continue
		}
		fmt.Fprintf(&b, "%d %d:", i, tab.rows.ids[i])
		for _, s := range rd.cells {
			fmt.Fprintf(&b, " %d=%v@%d", s.col, s.c.content, s.c.style)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "cols %v widths %v chunks %v", tab.cols.ids, tab.widths, tab.chunks)
	return b.String()
}

// A tab's maps and arrays keep the room of the most they held. When what
// they hold falls well below that, the tick makes them again for what is
// left: after calls that formatted every row and were refused, an action
// whose commit failed, and a deletion of most of a tab's rows. Each tab
// has 50,000 rows. What reads see, the files of the rows and undo stay as
// they were.
func TestTabsGiveBackTheRoomTheyNoLongerUse(t *testing.T) {
	slow(t, "formats and deletes 50,000 rows of several tabs")
	rows, tabs := raced(50000, 2000), raced(8, 2)
	setup := func(t *testing.T) *env {
		e := start(t)
		for k := range tabs {
			name := fmt.Sprint("Rows", k)
			e.mustAgent(jsonf(`{"ops":[{"op":"add_tab","name":%q},{"op":"insert_rows","tab":%q,"at":%d,"count":%d}]}`, name, name, newTabRows+1, rows-newTabRows))
		}
		e.tick()
		return e
	}
	// wantSpare ticks and checks that the tabs keep little room besides
	// what they hold, and that the reads of each tab see the same.
	wantSpare := func(t *testing.T, e *env) {
		t.Helper()
		before := map[string]string{}
		for _, name := range e.tabNames() {
			before[name] = tabContents(e, name)
		}
		e.tick()
		for _, name := range e.tabNames() {
			if got := tabContents(e, name); got != before[name] {
				t.Fatalf("tab %s reads differently after the tick", name)
			}
		}
		// Under the race detector the heap says nothing; the room that the
		// tabs note and their arrays keep says it there too.
		e.in.mu.RLock()
		for _, tab := range e.in.wb.tabs {
			if roomDue(tab.dataMost, len(tab.data)) || roomDue(cap(tab.rows.ids), len(tab.rows.ids)) || roomDue(cap(tab.byPos), len(tab.rows.ids)) {
				e.in.mu.RUnlock()
				t.Fatalf("after the tick tab %s keeps room for %d rows of data and %d ids, and holds %d and %d", tab.name, tab.dataMost, cap(tab.rows.ids), len(tab.data), len(tab.rows.ids))
			}
		}
		e.in.mu.RUnlock()
		kept := tabSpare(t, e)
		t.Logf("the tabs keep %d KiB of the heap beside what they hold", kept>>10)
		if measures && kept > 256<<10 {
			t.Errorf("the tabs keep %d KiB of the heap beside what they hold", kept>>10)
		}
	}
	t.Run("agent calls that a later operation refuses", func(t *testing.T) {
		e := setup(t)
		for round := range 2 {
			for k := range tabs {
				call := jsonf(`{"ops":[{"op":"format","tab":"Rows%d","ranges":["A1:A%d"],"format":{"bold":true}},{"op":"delete_tab","tab":"Nowhere"}]}`, k, rows)
				if _, err := e.agent(call); err == nil {
					t.Fatalf("round %d, tab %d: the call was applied", round, k)
				}
			}
		}
		wantSpare(t, e)
	})
	t.Run("an action whose commit fails", func(t *testing.T) {
		e := setup(t)
		alice := e.person("Alice")
		seq := e.seq()
		e.in.mu.Lock()
		e.in.fault = func() error { return errors.New("injected commit failure") }
		e.in.mu.Unlock()
		for k := range tabs {
			if _, err := alice.try(seq, e.formatRange(fmt.Sprint("Rows", k), 1, rows, 1, 3, `{"bold":true}`)); refusal(err).Reason != "commit" {
				t.Fatalf("tab %d: a failed commit answered %v", k, err)
			}
		}
		e.in.mu.Lock()
		e.in.fault = nil
		e.in.mu.Unlock()
		wantSpare(t, e)
	})
	t.Run("formats of their own that a refused call gave cells", func(t *testing.T) {
		e := setup(t)
		for round := range 2 {
			ops := make([]string, 0, 3000)
			for i := range 3000 {
				ops = append(ops, jsonf(`{"op":"format","tab":"Rows0","ranges":["A%d"],"format":%s}`, i+1, symbolFormat(round*3000+i)))
			}
			if _, err := e.agent(`{"ops":[` + strings.Join(ops, ",") + `,{"op":"delete_tab","tab":"Nowhere"}]}`); err == nil {
				t.Fatalf("round %d: the call was applied", round)
			}
		}
		// The states no cell uses went with each revert; the tick gives the
		// room of their maps back. No change commits in between.
		wantSpare(t, e)
		e.in.mu.RLock()
		left, most := len(e.in.wb.styles.ids), e.in.wb.styles.most
		e.in.mu.RUnlock()
		if left > 1 || roomDue(most, left) {
			t.Fatalf("the style states are %d, noted at %d", left, most)
		}
	})
	t.Run("a deletion of most rows of a tab", func(t *testing.T) {
		e := setup(t)
		alice := e.person("Alice")
		for k := range tabs {
			name := fmt.Sprint("Rows", k)
			for first := 0; first < rows; first += maxCallCells {
				var b strings.Builder
				for i := first; i < min(first+maxCallCells, rows); i++ {
					if i > first {
						b.WriteByte(',')
					}
					fmt.Fprintf(&b, "[%d]", i+1)
				}
				e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":%q,"at":"A%d","rows":[%s]}]}`, name, first+1, b.String()))
			}
			if ack := alice.do(e.deleteRowsOp(name, 11, rows-20)); status(ack) != "applied" {
				t.Fatalf("the deletion in %s: %s", name, status(ack))
			}
		}
		wantSpare(t, e)
		e.want("Rows0!A10", "10")
		e.want("Rows0!A11", fmt.Sprint(rows-9))
		// The undo puts the rows back with their values.
		wantUndo(t, "undo of the last deletion", undo(t, alice), undoAck{Applied: 1})
		last := fmt.Sprint("Rows", tabs-1)
		e.want(last+"!A11", "11")
		e.want(fmt.Sprintf("%s!A%d", last, rows), fmt.Sprint(rows))
	})
}

// A row's list of cells is made again as it shrinks to a quarter of its
// array: here a refused call widens 200 rows that hold one cell each to
// 250 cells, and its revert takes them out again.
func TestRowsGiveBackTheRoomOfTheirCells(t *testing.T) {
	const rows, cols = 200, 250
	e := start(t)
	e.fillColumn("Sheet1", "A", rows, func(row int) string { return fmt.Sprint(row) })
	var b strings.Builder
	for r := range rows {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('[')
		for c := range cols - 1 {
			if c > 0 {
				b.WriteByte(',')
			}
			fmt.Fprint(&b, c)
		}
		b.WriteByte(']')
	}
	call := `{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[` + b.String() + `]},{"op":"delete_tab","tab":"Nowhere"}]}`
	if _, err := e.agent(call); err == nil {
		t.Fatal("the call was applied")
	}
	e.in.mu.RLock()
	for row, rd := range e.in.wb.tab("Sheet1").data {
		if len(rd.cells) != 1 || cap(rd.cells) > 16 {
			e.in.mu.RUnlock()
			t.Fatalf("row %d holds %d cells in an array of %d", row, len(rd.cells), cap(rd.cells))
		}
	}
	e.in.mu.RUnlock()
	kept := tabSpare(t, e)
	t.Logf("the tabs keep %d KiB of the heap beside what they hold", kept>>10)
	if measures && kept > 128<<10 {
		t.Errorf("the tabs keep %d KiB of the heap beside what they hold", kept>>10)
	}
	e.want("Sheet1!A7", "7")
	e.want("Sheet1!B7", "")
}

// The tick spends a bounded amount of work on making tabs again. A share
// too small for a map leaves it as it was, to a later tick; the reads see
// the same in between.
func TestTabRoomWorkIsBounded(t *testing.T) {
	rows := raced(50000, 4000)
	e := start(t)
	e.mustAgent(jsonf(`{"ops":[{"op":"add_tab","name":"R"},{"op":"insert_rows","tab":"R","at":%d,"count":%d}]}`, newTabRows+1, rows-newTabRows))
	call := jsonf(`{"ops":[{"op":"format","tab":"R","ranges":["A1:A%d"],"format":{"bold":true}},{"op":"delete_tab","tab":"Nowhere"}]}`, rows)
	if _, err := e.agent(call); err == nil {
		t.Fatal("the call was applied")
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"R","at":"C3","rows":[["kept"]]}]}`)
	before := tabContents(e, "R")
	due := func() int {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		return e.in.wb.tab("R").dataMost
	}
	if got := due(); got < rows {
		t.Fatalf("the rows map was noted at %d entries, want %d", got, rows)
	}
	// The map holds one row: a share of 2,048 entries is enough to copy it,
	// and the work counts the map's entries as it copies them.
	onWriter(e, func() {
		e.in.mu.Lock()
		e.in.compactTabs(2 * paceEvery * paceSteps)
		e.in.mu.Unlock()
	})
	if got := due(); got != 1 {
		t.Fatalf("after a tick with room for its one row the map is noted at %d entries", got)
	}
	if tabContents(e, "R") != before {
		t.Fatal("the tab reads differently after it was made again")
	}
	// With a value in every row, a deletion of 70% of them leaves the rows
	// map and the axis due, each with many entries left. A share smaller
	// than those leaves both as they were. The work is counted every 1,024
	// entries, so the share is one step short of that.
	for first := 0; first < rows; first += maxCallCells {
		var b strings.Builder
		for i := first; i < min(first+maxCallCells, rows); i++ {
			if i > first {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "[%d]", i+1)
		}
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"R","at":"A%d","rows":[%s]}]}`, first+1, b.String()))
	}
	left := rows * 3 / 10
	e.mustAgent(jsonf(`{"ops":[{"op":"delete_rows","tab":"R","at":%d,"count":%d}]}`, left+1, rows-left))
	before = tabContents(e, "R")
	room := func() (most, ids int) {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		tab := e.in.wb.tab("R")
		return tab.dataMost, cap(tab.rows.ids)
	}
	onWriter(e, func() {
		e.in.mu.Lock()
		e.in.compactTabs(paceEvery*paceSteps - 1)
		e.in.mu.Unlock()
	})
	if most, ids := room(); most < rows || ids < rows {
		t.Fatalf("a share of less than 1,024 entries made the tab of %d rows again: the map noted at %d, the axis in an array of %d", left, most, ids)
	}
	if tabContents(e, "R") != before {
		t.Fatal("the tab reads differently after a share that stopped")
	}
	e.tick()
	if most, ids := room(); most != left || ids > 2*left {
		t.Fatalf("after a whole tick the map of %d rows is noted at %d, the axis lies in an array of %d", left, most, ids)
	}
	if tabContents(e, "R") != before {
		t.Fatal("the tab reads differently after it was made again")
	}
	e.want("R!C3", "kept")
	e.want(fmt.Sprintf("R!A%d", left), fmt.Sprint(left))
}
