package sheet

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// emojis is a formula whose value is 32,767 characters of four bytes
// each: 131,068 bytes of calculated text.
const emojis = `=REPT("😀",32767)`

// TestUndoChargesCalculatedTextInBytes has one person delete, ten at a
// time, 100 rows, 100 columns or ten tabs of ten cells that each calculate
// 32,767 characters of four bytes. Undo keeps the deleted cells with their
// values, so each entry keeps about 1.25 MiB of calculated text and goes
// to a scratch file by its size. What the undo entries left in memory are
// counted at covers what the heap lets go when they are dropped, and the
// last entries undo, each bringing its cells back with their values.
func TestUndoChargesCalculatedTextInBytes(t *testing.T) {
	slow(t, "calculates 13 MiB of text and deletes it")
	value := strings.Repeat("😀", 32767)
	row := func(n int) string {
		return "[" + strings.TrimSuffix(strings.Repeat(fmt.Sprintf("%q,", emojis), n), ",") + "]"
	}
	for _, c := range []struct {
		name   string
		setup  func(e *env)
		delete func(e *env, i int) string // the op of the i-th deletion
		cell   func(i int) string         // a cell the i-th deletion took
	}{
		{"rows", func(e *env) {
			e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[%s]}]}`, strings.TrimSuffix(strings.Repeat(row(1)+",", 100), ",")))
		}, func(e *env, i int) string { return e.deleteRowsOp("Sheet1", 1, 10) }, func(int) string { return "Sheet1!A1" }},
		{"columns", func(e *env) {
			// A column of its own stays, since a tab keeps one.
			e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[%s]},{"op":"write","tab":"Sheet1","at":"CW1","rows":[[1]]}]}`, row(100)))
		}, func(e *env, i int) string { return e.deleteColsOp("Sheet1", 1, 10) }, func(int) string { return "Sheet1!A1" }},
		{"tabs", func(e *env) {
			for i := range 10 {
				e.mustAgent(jsonf(`{"ops":[{"op":"add_tab","name":"Tab_%d"},{"op":"write","tab":"Tab_%d","at":"A1","rows":[%s]}]}`, i, i, row(10)))
			}
		}, func(e *env, i int) string {
			return jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID(fmt.Sprint("Tab_", i)))
		},
			func(i int) string { return fmt.Sprintf("Tab_%d!A1", i) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			c.setup(e)
			alice := e.person("Alice")
			const deletions = 10
			for i := range deletions {
				if ack := alice.do(c.delete(e, i)); status(ack) != "applied" {
					t.Fatalf("deletion %d: %s", i, status(ack))
				}
			}
			spilled := 0
			for i := range deletions {
				if alice.spilled(i) != nil {
					spilled++
				}
			}
			// The last two deletions come back with their values.
			for i := deletions - 1; i >= deletions-2; i-- {
				if u := undo(t, alice); u.Applied == 0 || u.Skipped != 0 {
					t.Fatalf("undo of deletion %d = %+v", i, u)
				}
				if got := e.shown(c.cell(i)); got != value {
					t.Fatalf("after the undo of deletion %d %s shows %d bytes, %d characters", i, c.cell(i), len(got), utf8.RuneCountInString(got))
				}
			}
			e.b.importAll(t)
			e.b.forgetRevisions()
			sheetHeap := func() int64 { return quietHeap(t, 1<<20) - e.b.blobBytes() }
			pool, stacks := e.residentUndo()
			held := sheetHeap()
			dropUndo(e, alice)
			kept := held - sheetHeap()
			t.Logf("%d of %d entries spilled; undo counts %d KiB in memory and kept %d KiB of the heap", spilled, deletions, stacks>>10, kept>>10)
			if pool != stacks || stacks > undoSessionBytes {
				t.Fatalf("the pool counts %d bytes and the stacks hold %d", pool, stacks)
			}
			// Each entry keeps ten values of 131,068 bytes, over the 1 MiB
			// of an entry.
			if spilled != deletions {
				t.Errorf("%d of %d entries of 1.25 MiB of text went to scratch files", spilled, deletions)
			}
			if measures && kept > stacks+1<<20 {
				t.Errorf("undo kept %d KiB of the heap and counts %d KiB", kept>>10, stacks>>10)
			}
		})
	}
}
