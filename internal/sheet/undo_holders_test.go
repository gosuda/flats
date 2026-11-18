package sheet

import (
	"fmt"
	"testing"
)

// A stamp that an undo wrote belongs to two entries of the session: to the
// entry of the undo itself, on the redo stack, and to the older entry it
// stands in for, on the undo stack. A collected blank record is kept for
// both, so that either can go without taking the stamp from the other.
func TestCollectedStampIsKeptForEveryHolder(t *testing.T) {
	const rows, cols, limit = 20, 20, 60
	groups := []struct {
		name string
		// setup gives the cell what first takes away. first is the action
		// of the person whose undo is checked, and second the one that is
		// undone at once.
		setup         func(e *env)
		first, second func(e *env) string
		// old reports whether the cell shows what it held before first.
		old func(e *env) bool
		// redone reports whether the cell shows what second gave it.
		redone func(e *env) bool
	}{
		{
			name:   "content",
			first:  func(e *env) string { return e.clearOp("Sheet1", 1, 1, 1, 1) },
			second: func(e *env) string { return e.setOp("A1", "new") },
			old:    func(e *env) bool { return e.shown("A1") == "1001" },
			redone: func(e *env) bool { return e.shown("A1") == "new" },
		},
		{
			// V1 lies outside the filled cells: its record is blank, and
			// without a format the tick collects it.
			name: "a format",
			setup: func(e *env) {
				e.mustAgent(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["V1"],"format":{"bold":true}}]}`)
			},
			first:  func(e *env) string { return e.formatRange("Sheet1", 1, 1, 22, 1, `{"bold":null}`) },
			second: func(e *env) string { return e.formatRange("Sheet1", 1, 1, 22, 1, `{"bold":true}`) },
			old:    func(e *env) bool { return e.bold("V1") },
			redone: func(e *env) bool { return e.bold("V1") },
		},
	}
	for _, mode := range []string{"in memory", "in scratch files"} {
		for _, g := range groups {
			cell := "A1"
			if g.name != "content" {
				cell = "V1"
			}
			// collected brings the sheet to the point where the record of
			// the cell is collected under the stamp of the person's undo.
			collected := func(t *testing.T) (*env, *person) {
				e := start(t)
				e.in.mu.Lock()
				e.in.wb.collected.limit = limit
				if mode != "in memory" {
					e.in.undoPool().entryMax = -1
				}
				e.in.mu.Unlock()
				fillNumbers(e, "Sheet1", rows, cols)
				alice := e.person("Alice")
				if g.setup != nil {
					g.setup(e)
				}
				if ack := alice.do(g.first(e)); status(ack) != "applied" {
					t.Fatalf("the first action: %s", status(ack))
				}
				if ack := alice.do(g.second(e)); status(ack) != "applied" {
					t.Fatalf("the second action: %s", status(ack))
				}
				wantUndo(t, "undo of the second action", undo(t, alice), undoAck{Applied: 1})
				// Later clears by an agent make the cell's record one of
				// the older half.
				e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A2:T%d"]}]}`, rows))
				e.collectTo(limit)
				if e.hasRecord(cell) {
					t.Fatalf("%s kept its record through the collection", cell)
				}
				return e, alice
			}
			// expire makes the entry of the undo, or the entry its stamp
			// stands in for, pass a limit of undo history.
			expire := func(e *env, alice *person, redo bool) {
				e.in.mu.Lock()
				defer e.in.mu.Unlock()
				u := &alice.s.undo
				if redo {
					u.redo[len(u.redo)-1].expire(nil)
				} else {
					u.undo[len(u.undo)-1].expire(nil)
				}
			}
			name := mode + ", " + g.name
			t.Run(name+", the redo stack is cleared", func(t *testing.T) {
				e, alice := collected(t)
				// A new edit clears the redo stack, and the tick drops what
				// only the entry of the undo held.
				alice.set("B1", "x")
				e.tick()
				wantUndo(t, "undo of the edit", undo(t, alice), undoAck{Applied: 1})
				wantUndo(t, "undo of the first action", undo(t, alice), undoAck{Applied: 1})
				if !g.old(e) {
					t.Fatalf("%s shows %q, bold %v after the undo of the first action", cell, e.shown(cell), e.bold(cell))
				}
			})
			t.Run(name+", the entry of the undo expires", func(t *testing.T) {
				e, alice := collected(t)
				expire(e, alice, true)
				e.tick()
				wantUndo(t, "undo of the first action", undo(t, alice), undoAck{Applied: 1})
				if !g.old(e) {
					t.Fatalf("%s shows %q, bold %v after the undo of the first action", cell, e.shown(cell), e.bold(cell))
				}
			})
			t.Run(name+", the entry the stamp stands in for expires", func(t *testing.T) {
				e, alice := collected(t)
				expire(e, alice, false)
				e.tick()
				wantUndo(t, "redo of the second action", redo(t, alice), undoAck{Applied: 1})
				if !g.redone(e) {
					t.Fatalf("%s shows %q, bold %v after the redo", cell, e.shown(cell), e.bold(cell))
				}
			})
		}
	}
}
