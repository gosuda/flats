package sheet

import (
	"os"
	"strconv"
	"testing"
)

func (p *person) stampLinks() int {
	p.e.in.mu.RLock()
	defer p.e.in.mu.RUnlock()
	n := 0
	for _, sets := range p.s.undo.notes {
		for _, s := range sets {
			n += s.n - s.gone
		}
	}
	return n
}

// Undoing and redoing one action over and over, with an older action of
// the session below it, keeps one stand-in for each group at most: the
// notes and the heap stay where the first cycles left them. The older
// action is still undone afterwards, and still guards against another
// writer.
func TestUndoStampLinksStayBounded(t *testing.T) {
	slow(t, "undoes and redoes one large action many times")
	// 50,000 cells and 40 cycles at full size, 5,000 cells and 10 cycles
	// by default, 1,500 cells under the race detector. The count of
	// stand-ins is checked after every undo and redo at every size; the
	// heap bound means something at full size and is not checked under
	// the race detector.
	const cols = 25
	rows, first := raced(sized(2000, 200), 60), sized(10, 4)
	cells := rows * cols
	// FLATS_SHEET_UNDO_CYCLES runs more cycles than the default.
	more := sized(30, 6)
	if v, err := strconv.Atoi(os.Getenv("FLATS_SHEET_UNDO_CYCLES")); err == nil && v > 10 {
		more = v - 10
	}
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":1000}]}`)
	alice, bob := e.person("Alice"), e.person("Bob")
	for salt := 1; salt <= 2; salt++ {
		if ack := alice.do(e.writeTexts("Sheet1", rows, cols, 10, salt)); status(ack) != "applied" {
			t.Fatalf("write %d: %s", salt, status(ack))
		}
	}
	cycle := func(n int) {
		for i := 0; i < n; i++ {
			wantUndo(t, "undo", undo(t, alice), undoAck{Applied: cells})
			if links := alice.stampLinks(); links > cells {
				t.Fatalf("%d stand-ins for %d cells", links, cells)
			}
			wantUndo(t, "redo", redo(t, alice), undoAck{Applied: cells})
			if links := alice.stampLinks(); links > cells {
				t.Fatalf("%d stand-ins for %d cells", links, cells)
			}
		}
	}
	cycle(first)
	before := int64(heapInUse())
	cycle(more)
	grown := int64(heapInUse()) - before
	t.Logf("%d more cycles: heap grew by %d KiB, %d stand-ins", more, grown>>10, alice.stampLinks())
	if measures && grown > 16<<20 {
		t.Fatalf("the heap grew by %d MiB over %d cycles of undo and redo", grown>>20, more)
	}

	// The second write is undone once more, and Bob replaces one cell of
	// the first. Alice's undo of the first write leaves that cell alone.
	wantUndo(t, "undo of the second write", undo(t, alice), undoAck{Applied: cells})
	if got := alice.stampLinks(); got != cells {
		t.Fatalf("%d stand-ins, want one for each of the %d cells", got, cells)
	}
	if ack := bob.do(e.writeTexts("Sheet1", 1, 1, 10, 3)); status(ack) != "applied" {
		t.Fatalf("Bob: %s", status(ack))
	}
	wantUndo(t, "undo of the first write", undo(t, alice), undoAck{Applied: cells - 1, Skipped: 1, Reason: "changed"})
	wantTexts(t, e, map[string]string{"Sheet1!A1": longText(10, 3000), "Sheet1!B1": ""})
}

func (e *env) widthPx(tab string, col int) int {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t := e.in.wb.tab(tab)
	if w, ok := t.widths[ColID(t.cols.ids[col-1])]; ok {
		return w.px
	}
	return defaultWidth
}

// A value that an undo or a redo brings back carries a new stamp, and the
// session's other entries on both stacks still find it: the entry below on
// the undo stack, and the redo that was undone before.
func TestUndoStandInsOnBothStacks(t *testing.T) {
	bothWays(t, func(t *testing.T, e *env) {
		alice, bob := e.person("Alice"), e.person("Bob")
		for _, text := range []string{"a", "b", "c"} {
			alice.do(e.setOp("Sheet1!A1", text))
		}
		one := undoAck{Applied: 1}
		step := func(what string, redoing bool, want string) {
			t.Helper()
			if redoing {
				wantUndo(t, what, redo(t, alice), one)
			} else {
				wantUndo(t, what, undo(t, alice), one)
			}
			e.wantText("Sheet1!A1", want)
		}
		step("undo c", false, "b")
		step("undo b", false, "a")
		step("redo b", true, "b")
		// The redo of c was undone two steps ago, and b is back by a redo.
		step("redo c", true, "c")
		step("undo c again", false, "b")
		step("undo b again", false, "a")
		step("redo b again", true, "b")
		step("undo b a third time", false, "a")
		step("undo a", false, "")
		step("redo a", true, "a")
		step("redo b a third time", true, "b")
		step("redo c again", true, "c")
		if links := alice.stampLinks(); links > 1 {
			t.Fatalf("%d stand-ins for one cell", links)
		}

		// Bob writes the cell and takes his write back. The text is the one
		// Alice's entries expect, under his stamp: none of them applies.
		step("undo c", false, "b")
		bob.do(e.setOp("Sheet1!A1", "x"))
		wantUndo(t, "Bob's undo", undo(t, bob), one)
		e.wantText("Sheet1!A1", "b")
		changed := undoAck{Skipped: 1, Reason: "changed"}
		wantUndo(t, "redo c over Bob's stamp", redo(t, alice), changed)
		wantUndo(t, "undo b over Bob's stamp", undo(t, alice), changed)
		e.wantText("Sheet1!A1", "b")
	})
}

// An action that writes one group twice is undone and redone as a whole.
func TestUndoOfRepeatedGroupInOneAction(t *testing.T) {
	bothWays(t, func(t *testing.T, e *env) {
		alice := e.person("Alice")
		tab, col := e.tabID("Sheet1"), e.colsOf("Sheet1", 2, 1)
		alice.do(jsonf(`{"op":"width","tab":%d,"cols":%s,"width":150}`, tab, col))
		ack := alice.do(
			jsonf(`{"op":"width","tab":%d,"cols":%s,"width":200}`, tab, col),
			jsonf(`{"op":"settings","tab":%d,"frozen_rows":1,"frozen_cols":0}`, tab),
			jsonf(`{"op":"width","tab":%d,"cols":%s,"width":300}`, tab, col),
			jsonf(`{"op":"settings","tab":%d,"frozen_rows":2,"frozen_cols":0}`, tab))
		if status(ack) != "applied,applied,applied,applied" {
			t.Fatalf("widths and settings: %s", status(ack))
		}
		frozen := func() int {
			e.in.mu.RLock()
			defer e.in.mu.RUnlock()
			return e.in.wb.tab("Sheet1").frozenRows
		}
		check := func(what string, px, rows int) {
			t.Helper()
			if got := e.widthPx("Sheet1", 2); got != px || frozen() != rows {
				t.Fatalf("%s: width %d and %d frozen rows, want %d and %d", what, got, frozen(), px, rows)
			}
		}
		check("after the action", 300, 2)
		wantUndo(t, "undo", undo(t, alice), undoAck{Applied: 4})
		check("after its undo", 150, 0)
		wantUndo(t, "redo", redo(t, alice), undoAck{Applied: 4})
		check("after its redo", 300, 2)
		wantUndo(t, "undo again", undo(t, alice), undoAck{Applied: 4})
		wantUndo(t, "undo of the first width", undo(t, alice), undoAck{Applied: 1})
		check("after both undos", defaultWidth, 0)
	})
}
