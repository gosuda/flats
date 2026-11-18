package sheet

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// markCycle has an agent insert rows below the first, write into each and
// delete them again. Each written row has a new id, so every cycle leaves a
// mark for each of its rows while an older undo entry can ask about them.
func markCycle(e *env, rows int) {
	e.t.Helper()
	markCycleIn(e, "Sheet1", rows)
}

// markCycleIn is markCycle in another tab.
func markCycleIn(e *env, tab string, rows int) {
	e.t.Helper()
	var b strings.Builder
	for i := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "[%d]", i)
	}
	e.mustAgent(jsonf(`{"ops":[{"op":"insert_rows","tab":%q,"at":2,"count":%d},{"op":"write","tab":%q,"at":"A2","rows":[%s]},{"op":"delete_rows","tab":%q,"at":2,"count":%d}]}`,
		tab, rows, tab, b.String(), tab, rows))
}

// marks returns what the write marks of the sheet count and how many there
// are, and whether the undo memory of the sheet holds its bound with them.
func (e *env) marks() (counted int64, n int, within bool) {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	for _, t := range e.in.wb.tabs {
		n += len(t.marks) + len(t.colMarks)
	}
	p := &e.in.scratch
	counted = e.in.wb.markBytes()
	return counted, n, p.resident+p.notes.bytes+counted <= p.instanceMax
}

// TestUndoCountsWriteMarks has an agent insert rows, write into them and
// delete them again, cycle after cycle, while Alice keeps the entry of an
// insert, whose undo asks whether someone wrote into the rows it removes,
// also into rows that were deleted since. The marks of the agent's rows
// count in the sheet's undo memory.
//
// While they fit, the heap they take is no more than they count, and it is
// free again once no entry can ask about them. When they pass the sheet's
// bound, Alice's entry expires as the oldest, the marks only it could ask
// about go, the host logs the cause, and the bound holds after every call.
func TestUndoCountsWriteMarks(t *testing.T) {
	slow(t, "writes 140,000 rows that are deleted again")
	rows := raced(20000, 500)
	bound := int64(raced(8<<20, 256<<10))
	setup := func(t *testing.T) (*env, *person, *logSink) {
		sink := &logSink{}
		e := start(t, func(o *Options) { o.Log = slog.New(slog.NewTextHandler(sink, nil)) })
		e.in.mu.Lock()
		e.in.undoPool().instanceMax = bound
		e.in.mu.Unlock()
		alice := e.person("Alice")
		if ack := alice.do(e.insertRowsOp("Sheet1", 1, 1)); status(ack) != "applied" {
			t.Fatalf("Alice's insert: %s", status(ack))
		}
		return e, alice, sink
	}
	t.Run("the heap they take", func(t *testing.T) {
		e, alice, _ := setup(t)
		for range 2 {
			markCycle(e, rows)
		}
		counted, n, within := e.marks()
		if !within || n < 2*rows || alice.lost(0) {
			t.Fatalf("after two cycles: %d marks counted at %d bytes, within the bound %v, Alice's entry expired %v", n, counted, within, alice.lost(0))
		}
		e.b.importAll(t)
		e.b.forgetRevisions()
		e.tick()
		held := undoHeap(t, e)
		// Alice's history goes, and her new entry has no insert: no entry
		// can ask about the marks.
		dropUndo(e, alice)
		e.tick()
		after, left, _ := e.marks()
		kept := held - undoHeap(t, e)
		t.Logf("%d marks counted at %d KiB kept %d KiB of the heap; %d marks of %d bytes are left", n, counted>>10, kept>>10, left, after)
		if left > 2 || after > 8<<10 {
			t.Errorf("%d marks of %d bytes are left once no entry can ask about them", left, after)
		}
		if measures && (kept > counted+256<<10 || kept < counted/4) {
			t.Errorf("the marks kept %d KiB of the heap and count %d KiB", kept>>10, counted>>10)
		}
	})
	t.Run("the bound of the sheet", func(t *testing.T) {
		e, alice, sink := setup(t)
		cycles := 0
		for ; cycles < 8 && !alice.lost(0); cycles++ {
			markCycle(e, rows)
			if counted, n, within := e.marks(); !within {
				t.Fatalf("after cycle %d %d marks count %d bytes, past the sheet's undo memory of %d", cycles+1, n, counted, bound)
			}
		}
		counted, n, _ := e.marks()
		t.Logf("Alice's entry expired after %d cycles; %d marks of %d KiB are left", cycles, n, counted>>10)
		if !alice.lost(0) {
			t.Fatalf("after %d cycles Alice's entry still holds %d marks of %d bytes", cycles, n, counted)
		}
		if n > 2 {
			t.Errorf("%d marks are left once the entry that could ask about them expired", n)
		}
		if lines := sink.lines("sheet undo payload lost"); len(lines) != 1 || !strings.Contains(lines[0], "write marks") {
			t.Errorf("the log lines about the expiry are %q", lines)
		}
		wantUndo(t, "undo of the entry that expired", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
		e.in.mu.RLock()
		left := e.in.wb.tab("Sheet1").rows.len()
		e.in.mu.RUnlock()
		if left != newTabRows+1 {
			t.Errorf("Sheet1 has %d rows after the undo that expired, want the %d with Alice's", left, newTabRows+1)
		}
	})
}
