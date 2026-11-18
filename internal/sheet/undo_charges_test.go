package sheet

import (
	"fmt"
	"strings"
	"testing"
)

// wantCharges compares what the pool and each session count for the undo
// entries with what the entries hold, measured here: a payload in memory
// at its measure, one in a scratch file at what it leaves in memory. It
// also checks the counts against the bounds of a session and of the sheet,
// where the write marks count with them.
func wantCharges(t *testing.T, e *env) {
	t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	e.in.smu.Lock()
	defer e.in.smu.Unlock()
	p := &e.in.scratch
	var all int64
	for _, s := range e.in.sessions {
		var held int64
		for _, stack := range [2][]*undoEntry{s.undo.undo, s.undo.redo} {
			for i, entry := range stack {
				measured := entry.left()
				switch {
				case entry.lost:
					measured = entryBytes
				case entry.file == nil:
					measured = entry.measure()
				}
				if entry.size != measured {
					t.Errorf("session %q, entry %d: counted at %d bytes, holds %d", s.label, i, entry.size, measured)
				}
				held += measured
			}
		}
		if got := s.undo.resident() - s.undo.noteBytes; got != held {
			t.Errorf("session %q counts %d bytes of entries, they hold %d", s.label, got, held)
		}
		if held+s.undo.noteBytes > p.sessionMax {
			t.Errorf("session %q holds %d bytes, over its bound of %d", s.label, held+s.undo.noteBytes, p.sessionMax)
		}
		all += held
	}
	if p.resident != all {
		t.Errorf("the pool counts %d bytes, the entries hold %d", p.resident, all)
	}
	if marks := e.in.wb.markBytes(); all+p.notes.bytes+marks > p.instanceMax {
		t.Errorf("the sheet holds %d bytes of undo and %d of write marks, over its bound of %d", all+p.notes.bytes, marks, p.instanceMax)
	}
}

// The payloads in memory follow the edits of an action inside one amount of
// work for all of them. One that finds the work used up goes to a scratch
// file and waits there for the edits it did not follow. What it leaves in
// memory then is counted as it is after it took those edits, and its
// session is brought back inside its bound: also for a payload that
// followed none of the edits, because another one used the work up.
func TestUndoSpilledForWorkIsCountedAsItWaits(t *testing.T) {
	slow(t, "rewrites formulas of 7,000 characters")
	const formulas = 4
	// setup leaves each person a clear of four long formulas in memory and
	// bounds the work of an action at 65,536 units: one rewrite of the four
	// formulas is about 2,000.
	setup := func(t *testing.T, people int, text func(row, i int) string) (*env, []*person) {
		t.Helper()
		e := start(t, unhurried)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"D"},{"op":"write","tab":"D","at":"A1","rows":[[1],[2],[3]]}]}`)
		var list []*person
		for k := 0; k < people; k++ {
			col := string(rune('A' + k))
			e.fillColumn("Sheet1", col, formulas, func(row int) string { return longRefs(700, func(i int) string { return text(row-1, i) }) })
			p := e.person(fmt.Sprint("P", k))
			if ack := p.do(e.clearOp("Sheet1", 1, formulas, k+1, 1)); status(ack) != "applied" || p.spilled(0) != nil {
				t.Fatalf("the clear of %s: %s, in a scratch file: %v", col, status(ack), p.spilled(0) != nil)
			}
			list = append(list, p)
		}
		e.in.mu.Lock()
		e.in.undoPool().followMax = 1 << 16
		e.in.mu.Unlock()
		wantCharges(t, e)
		return e, list
	}
	// waiting checks that every payload went to a scratch file and waits
	// for edits of a tab.
	waiting := func(t *testing.T, e *env, people []*person) {
		t.Helper()
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		for k, p := range people {
			entry := p.s.undo.undo[0]
			if entry.file == nil || entry.wait == nil || len(entry.wait.needs) == 0 {
				t.Fatalf("the payload of person %d is not one that waits in a scratch file", k)
			}
		}
	}
	// undone gives the catch-up at an undo its whole bound back.
	undone := func(t *testing.T, e *env, p *person) {
		t.Helper()
		e.in.mu.Lock()
		e.in.undoPool().followMax = undoFollowUnits
		e.in.mu.Unlock()
		wantUndo(t, "the undo", undo(t, p), undoAck{Applied: formulas})
	}
	ref := func(row, i int) string { return fmt.Sprintf("D!$A$%d", 5+(i+row)%850) }
	t.Run("the payloads of two people", func(t *testing.T) {
		e, people := setup(t, 2, ref)
		ops := make([]string, 200)
		for i := range ops {
			ops[i] = `{"op":"insert_rows","tab":"D","at":1,"count":1}`
		}
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
		waiting(t, e, people)
		wantCharges(t, e)
		for _, p := range people {
			undone(t, e, p)
		}
	})
	t.Run("renames, for a payload of more names than it keeps", func(t *testing.T) {
		e, people := setup(t, 1, func(row, i int) string {
			if i < 40 {
				return fmt.Sprintf("None%d!A1", i)
			}
			return ref(row, i)
		})
		ops := make([]string, 600)
		for i := range ops {
			from, to := "D", "E"
			if i%2 == 1 {
				from, to = "E", "D"
			}
			ops[i] = fmt.Sprintf(`{"op":"rename_tab","tab":%q,"name":%q}`, from, to)
		}
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
		waiting(t, e, people)
		wantCharges(t, e)
		undone(t, e, people[0])
	})
}
