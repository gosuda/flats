package sheet

import (
	"fmt"
	"strings"
	"testing"
)

// shrunkTab adds a tab that an agent grows to every row and column a tab
// can have, gives widths, values and sort stamps to many of them, and then
// shrinks to keep rows and two columns. What the tab held at its largest
// is still in the backing arrays and maps of its axes and of its maps.
func shrunkTab(e *env, name string, keep int) {
	e.t.Helper()
	e.mustAgent(jsonf(`{"ops":[{"op":"add_tab","name":%q},{"op":"insert_rows","tab":%q,"at":%d,"count":%d},{"op":"insert_cols","tab":%q,"at":"AA","count":%d},{"op":"widths","tab":%q,"cols":"A:ZZ","width":60}]}`,
		name, name, newTabRows+1, 100000-newTabRows, name, 702-newTabCols, name))
	const valued = 2000
	var b strings.Builder
	for i := 0; i < valued; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "[%d]", valued-i)
	}
	e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":%q,"at":"A1","rows":[%s]},{"op":"sort","tab":%q,"rows":"1:%d","by":"A"}]}`, name, b.String(), name, valued))
	e.mustAgent(jsonf(`{"ops":[{"op":"delete_rows","tab":%q,"at":%d,"count":%d},{"op":"delete_cols","tab":%q,"at":"C","count":%d}]}`,
		name, keep+1, 100000-keep, name, 700))
}

// TestUndoCountsWhatAShrunkTabKeeps has Alice delete tabs that grew to
// 100,000 rows and 702 columns and then shrank to 1,000 rows and two
// columns, with the ordinary bounds of undo history. Each entry counts
// what the saved tab keeps of the heap, its axes and maps included, so the
// bounds of a session and of the sheet spill entries before what undo
// keeps passes them. After spilling and after the entries are dropped, the
// heap they kept is free again.
func TestUndoCountsWhatAShrunkTabKeeps(t *testing.T) {
	slow(t, "grows and shrinks tabs of 100,000 rows")
	e := start(t)
	alice := e.person("Alice")
	tabs, keep := raced(8, 2), 1000
	for i := 0; i < tabs; i++ {
		name := "Tab" + string(rune('A'+i))
		shrunkTab(e, name, keep)
		if ack := alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID(name))); status(ack) != "applied" {
			t.Fatalf("delete %s: %s", name, status(ack))
		}
	}
	e.b.importAll(t)
	e.b.forgetRevisions()
	e.tick()
	m := e.undoMemory()
	p := e.in.undoPool()
	if m.session > p.sessionMax || m.pool+e.in.scratch.notes.bytes > p.instanceMax {
		t.Fatalf("undo holds %d KiB for Alice and %d KiB for the sheet, past %d KiB and %d KiB", m.session>>10, m.pool>>10, p.sessionMax>>10, p.instanceMax>>10)
	}
	files, _ := e.scratch()
	t.Logf("%d entries, %d spilled, %d KiB counted in memory", tabs, files, m.stacks>>10)
	if measures && (files == 0 || files == tabs) {
		t.Errorf("%d of %d saved tabs of %d rows each were spilled; the bound of a session spills some of them", files, tabs, keep)
	}
	sheetHeap := func() int64 { return quietHeap(t, 1<<20) - e.b.blobBytes() }
	held := sheetHeap()

	// The last tab returns as it was when Alice deleted it.
	if u := undo(t, alice); u.Applied != 1 {
		t.Fatalf("undo of the last deletion = %+v", u)
	}
	last := "Tab" + string(rune('A'+tabs-1))
	e.in.mu.RLock()
	tab := e.in.wb.tab(last)
	rows, cols := tab.rows.len(), tab.cols.len()
	e.in.mu.RUnlock()
	if rows != keep || cols != 2 {
		t.Fatalf("%s came back with %d rows and %d columns, want %d and 2", last, rows, cols, keep)
	}
	e.want(last+"!A1", "1")
	e.want(fmt.Sprintf("%s!A%d", last, keep), fmt.Sprint(keep))
	e.mustAgent(jsonf(`{"ops":[{"op":"delete_tab","tab":%q}]}`, last))

	dropUndo(e, alice)
	e.tick()
	if files, _ := e.scratch(); files != 0 {
		t.Errorf("%d scratch files stay after the history was dropped", files)
	}
	kept := held - sheetHeap()
	t.Logf("undo counted %d KiB in memory and kept %d KiB of the heap", m.stacks>>10, kept>>10)
	if measures && kept > m.stacks+1<<20 {
		t.Errorf("undo kept %d KiB of the heap and counts %d KiB", kept>>10, m.stacks>>10)
	}
}
