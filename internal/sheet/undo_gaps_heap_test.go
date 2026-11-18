package sheet

import (
	"fmt"
	"strings"
	"testing"
)

// reversedTab adds a tab of at least rows rows whose first rows rows have
// ids that descend down the tab: deleting rows from the top takes ids of
// which no two follow each other, and a deletion an undo entry keeps apart
// holds one run for each.
func reversedTab(e *env, name string, rows int) {
	e.t.Helper()
	e.mustAgent(jsonf(`{"ops":[{"op":"add_tab","name":%q}]}`, name))
	if rows > newTabRows {
		e.mustAgent(jsonf(`{"ops":[{"op":"insert_rows","tab":%q,"at":%d,"count":%d}]}`, name, newTabRows+1, rows-newTabRows))
	}
	for first := 0; first < rows; first += maxCallCells {
		var b strings.Builder
		for i := first; i < min(first+maxCallCells, rows); i++ {
			if i > first {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "[%d]", i)
		}
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":%q,"at":"A%d","rows":[%s]}]}`, name, first+1, b.String()))
	}
	e.mustAgent(jsonf(`{"ops":[{"op":"sort","tab":%q,"rows":"1:%d","by":"A","desc":true},{"op":"delete_cols","tab":%q,"at":"A","count":1}]}`, name, rows, name))
}

// undoHeap returns the heap the sheet holds, without what the fake host
// keeps of its files, read with little allocation during the collection:
// two readings differ by at most twice the slack from what they hold.
func undoHeap(t *testing.T, e *env) int64 {
	t.Helper()
	return quietHeap(t, 64<<10) - e.b.blobBytes()
}

// wantUndoHeap drops the undo history of the people and checks that the
// heap that lets go is no more than undo counted for it, with the write
// marks that go with the history.
func wantUndoHeap(t *testing.T, e *env, people ...*person) {
	t.Helper()
	e.b.importAll(t)
	e.b.forgetRevisions()
	e.tick()
	m := e.undoMemory()
	if m.pool != m.stacks {
		t.Fatalf("the pool counts %d bytes and the stacks hold %d", m.pool, m.stacks)
	}
	marks, _, _ := e.marks()
	held := undoHeap(t, e)
	for _, p := range people {
		dropUndo(e, p)
	}
	e.tick()
	after, _, _ := e.marks()
	kept := held - undoHeap(t, e)
	counted := m.stacks + marks - after
	t.Logf("undo counted %d KiB in memory and kept %d KiB of the heap", counted>>10, kept>>10)
	if measures && kept > counted+256<<10 {
		t.Errorf("undo kept %d KiB of the heap and counts %d KiB", kept>>10, counted>>10)
	}
}

// TestUndoCountsTheDeletionsItKeepsApart has undo entries keep deletions
// apart that others made, with the ordinary bounds of undo history, and
// compares what the heap lets go when the history is dropped with what
// undo counted for it.
//
// In the first case each entry keeps a deletion of 47,105 rows whose ids
// follow no other: as many runs, a number at which a list grown one run at
// a time has a quarter of its room to spare. The deletions are of
// different tabs, so no two entries share their runs, and a sixth one
// takes the session past its bound, so that payloads go to scratch files.
//
// In the second case Bob deletes 32 rows, one at a time, below the cells
// of 300 entries of three people, and then undoes the deletions. Each
// entry kept every deletion apart and has none left. The entries still
// apply.
func TestUndoCountsTheDeletionsItKeepsApart(t *testing.T) {
	slow(t, "deletes 280,000 rows of ids that follow no other")
	t.Run("long runs of ids", func(t *testing.T) {
		const entries = 6
		runs := raced(47105, 500)
		e := start(t)
		alice := e.person("Alice")
		for k := range entries {
			name := fmt.Sprint("Gone_", k)
			reversedTab(e, name, runs+1)
			e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[["=%s!A1"]]}]}`, k+1, name))
			if ack := alice.set(fmt.Sprintf("Sheet1!A%d", k+1), fmt.Sprintf("=%s!A2", name)); status(ack) != "applied" {
				t.Fatalf("Alice's formula %d: %s", k, status(ack))
			}
			e.mustAgent(jsonf(`{"ops":[{"op":"delete_rows","tab":%q,"at":1,"count":%d}]}`, name, runs))
		}
		e.in.mu.RLock()
		var kept, spilled int
		for _, entry := range alice.s.undo.undo {
			switch {
			case entry.file != nil:
				spilled++
			case len(entry.gaps) == 1 && len(entry.gaps[0].runs) == runs:
				kept++
			}
		}
		e.in.mu.RUnlock()
		if kept+spilled != entries || measures && (spilled == 0 || kept == 0) {
			t.Fatalf("%d entries keep a deletion of %d runs in memory and %d went to scratch files, of %d", kept, runs, spilled, entries)
		}
		wantCharges(t, e)
		// The last entry brings Alice's formula back, with a reference the
		// deletion took.
		if u := undo(t, alice); u.Applied != 1 {
			t.Fatalf("the undo of Alice's formula = %+v", u)
		}
		if got := e.text(fmt.Sprintf("Sheet1!A%d", entries)); !strings.Contains(got, "#REF!") {
			t.Fatalf("the undo brought back %q", got)
		}
		wantUndoHeap(t, e, alice)
	})

	t.Run("deletions that came back", func(t *testing.T) {
		const people, entries, deletions = 3, maxUndoEntries, gapsKept
		e := start(t)
		var list []*person
		for k := range people {
			col := string(rune('A' + k))
			e.fillColumn("Sheet1", col, entries, func(row int) string { return fmt.Sprintf("=Z%d+%d", row, k) })
			p := e.person(fmt.Sprint("P", k))
			for row := 1; row <= entries; row++ {
				if ack := p.set(fmt.Sprintf("Sheet1!%s%d", col, row), "1"); status(ack) != "applied" {
					t.Fatalf("%s%d: %s", col, row, status(ack))
				}
			}
			list = append(list, p)
		}
		bob := e.person("Bob")
		for range deletions {
			if ack := bob.do(e.deleteRowsOp("Sheet1", entries+100, 1)); status(ack) != "applied" {
				t.Fatalf("Bob's deletion: %s", status(ack))
			}
		}
		gaps := func() (most int) {
			e.in.mu.RLock()
			defer e.in.mu.RUnlock()
			for _, p := range list {
				for _, entry := range p.s.undo.undo {
					most = max(most, len(entry.gaps))
				}
			}
			return most
		}
		if got := gaps(); got != deletions {
			t.Fatalf("the entries keep at most %d deletions apart, want %d", got, deletions)
		}
		for range deletions {
			if u := undo(t, bob); u.Applied != 1 {
				t.Fatalf("the undo of Bob's deletion = %+v", u)
			}
		}
		if got := gaps(); got != 0 {
			t.Fatalf("the entries keep %d deletions apart after they came back", got)
		}
		wantCharges(t, e)
		for _, p := range list {
			if u := undo(t, p); u.Applied != 1 {
				t.Fatalf("the undo of %s = %+v", p.s.label, u)
			}
		}
		e.wantText("Sheet1!A100", "=Z100+0")
		wantUndoHeap(t, e, list...)
	})
}
