package sheet

import "testing"

// A column that an undo brings back has its width again, and the width is
// part of the column: when the redo of the deletion is skipped because
// another writer has written into the column since, the column keeps the
// width as well. The width was another writer's here, too.
func TestSkippedRedoKeepsColumnWidth(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,2],[3,4]]}]}`)
	alice, bob := e.person("Alice"), e.person("Bob")
	bob.do(jsonf(`{"op":"width","tab":%d,"cols":%s,"width":300}`, e.tabID("Sheet1"), e.colsOf("Sheet1", 2, 1)))
	_, _, col := e.ids("B1")
	width := func() int {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		return e.in.wb.tabs[0].widthOf(col)
	}
	alice.do(e.deleteColsOp("Sheet1", 2, 1))
	if u := undo(t, alice); u.Applied != 1 || u.Skipped != 0 || width() != 300 {
		t.Fatalf("undo of the deletion = %+v, the column is %d px wide", u, width())
	}
	bob.set("B1", "other")
	ack := alice.do(`{"op":"redo"}`)
	if ack.Undo == nil || ack.Undo.Applied != 0 || ack.Undo.Skipped != 1 || ack.Undo.Reason != "written" {
		t.Fatalf("redo of the deletion after another writer's edit = %+v", ack.Undo)
	}
	if got := width(); got != 300 {
		t.Fatalf("the skipped redo left the column %d px wide, want 300", got)
	}
	e.want("B1", "other")
	// Without another writer the redo takes the column and its width, and
	// the undo after it brings both back.
	e2 := start(t)
	e2.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,2]]},{"op":"widths","tab":"Sheet1","cols":"B:B","width":300}]}`)
	carol := e2.person("Carol")
	carol.do(e2.deleteColsOp("Sheet1", 2, 1))
	undo(t, carol)
	if ack := carol.do(`{"op":"redo"}`); ack.Undo == nil || ack.Undo.Applied != 1 || ack.Undo.Skipped != 0 {
		t.Fatalf("redo of the deletion = %+v", ack.Undo)
	}
	e2.want("B1", "")
	undo(t, carol)
	e2.in.mu.RLock()
	got := e2.in.wb.tabs[0].widthOf(ColID(e2.in.wb.tabs[0].cols.ids[1]))
	e2.in.mu.RUnlock()
	if got != 300 || e2.shown("B1") != "2" {
		t.Fatalf("after undo, redo and undo the column is %d px wide and B1 shows %q", got, e2.shown("B1"))
	}
}

// A width that returns with its column is written by the undo that brings
// the column back. Like every group an undo restores, the new stamp stands
// in for the session's entry that had written the width, so the session's
// undo of the width still applies. Another session's undo of it does not.
func TestUndoWidthAfterItsColumnReturned(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,2,3],[4,5,6]]}]}`)
	alice, bob := e.person("Alice"), e.person("Bob")
	_, _, colB := e.ids("B1")
	_, _, colC := e.ids("C1")
	width := func(col ColID) int {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		return e.in.wb.tabs[0].widthOf(col)
	}
	alice.do(jsonf(`{"op":"width","tab":%d,"cols":%s,"width":300}`, e.tabID("Sheet1"), e.colsOf("Sheet1", 2, 1)))
	bob.do(jsonf(`{"op":"width","tab":%d,"cols":%s,"width":200}`, e.tabID("Sheet1"), e.colsOf("Sheet1", 3, 1)))
	alice.do(e.deleteColsOp("Sheet1", 2, 2))
	wantUndo(t, "undo of the deletion", undo(t, alice), undoAck{Applied: 1})
	if width(colB) != 300 || width(colC) != 200 {
		t.Fatalf("the columns returned %d and %d px wide, want 300 and 200", width(colB), width(colC))
	}
	wantUndo(t, "Bob's undo of a width Alice's undo wrote", undo(t, bob), undoAck{Skipped: 1, Reason: "changed"})
	wantUndo(t, "Alice's undo of her width", undo(t, alice), undoAck{Applied: 1})
	if width(colB) != defaultWidth || width(colC) != 200 {
		t.Fatalf("after the undo of the width the columns are %d and %d px wide, want %d and 200", width(colB), width(colC), defaultWidth)
	}
	if ack := alice.do(`{"op":"redo"}`); ack.Undo == nil || ack.Undo.Applied != 1 || width(colB) != 300 {
		t.Fatalf("redo of the width = %+v, the column is %d px wide", ack.Undo, width(colB))
	}
	wantUndo(t, "undo of the width again", undo(t, alice), undoAck{Applied: 1})
	if width(colB) != defaultWidth {
		t.Fatalf("after the second undo of the width the column is %d px wide", width(colB))
	}
}
