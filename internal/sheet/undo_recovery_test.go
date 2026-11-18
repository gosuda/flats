package sheet

import (
	"errors"
	"strings"
	"testing"
)

// wantIdentities checks what every stored state must hold: a tab id names
// one tab, and a row or column id is in its axis once, at the position the
// axis says.
func wantIdentities(t testing.TB, e *env) {
	t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	tabs := map[TabID]bool{}
	for _, tab := range e.in.wb.tabs {
		if tabs[tab.id] || e.in.wb.byID[tab.id] != tab {
			t.Fatalf("tab id %d names more than one tab", tab.id)
		}
		tabs[tab.id] = true
		for name, a := range map[string]*axis{"row": &tab.rows, "column": &tab.cols} {
			if len(a.pos) != len(a.ids) {
				t.Fatalf("tab %q holds %d %s ids in %d places", tab.name, len(a.pos), name, len(a.ids))
			}
			for i, id := range a.ids {
				if a.pos[id] != int32(i) {
					t.Fatalf("tab %q: %s id %d is at %d and at %d", tab.name, name, id, i, a.pos[id])
				}
			}
		}
	}
}

// A tab recovery replaces what retained undo entries knew of the tab: its
// rows and columns are live again under their ids, with the content of the
// revision. Every step of an older entry that touches the tab is skipped as
// recovered, so no id enters an axis twice and nothing recovered is
// overwritten or removed.
func TestUndoAfterTabRecovery(t *testing.T) {
	recovered := undoAck{Skipped: 1, Reason: "recovered"}
	setup := func(e *env) (alice, bob *person, rev int, data TabID) {
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Data","at":"A1","rows":[[1,"b1"],[2,"b2"],[3,"b3"]]}]}`)
		return e.person("Alice"), e.person("Bob"), e.b.importAll(e.t), e.tabID("Data")
	}
	replace := func(t *testing.T, e *env, bob *person, rev int, data TabID) {
		t.Helper()
		bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, data))
		if id, err := bob.recover(rev, data); err != nil || id != data {
			t.Fatalf("recovery = %d %v, want the tab back under its id %d", id, err, data)
		}
	}
	check := func(t *testing.T, e *env) {
		t.Helper()
		for _, when := range []string{"after the undo", "after a restart"} {
			wantIdentities(t, e)
			if got := strings.Join(e.column("Data", "A", 1, 3), ","); got != "1,2,3" {
				t.Fatalf("%s the recovered rows hold %s", when, got)
			}
			if got := e.shown("Data!B2"); got != "b2" {
				t.Fatalf("%s Data!B2 shows %q", when, got)
			}
			e.restart()
		}
	}
	for name, act := range map[string]func(e *env) string{
		"a deleted row":      func(e *env) string { return e.deleteRowsOp("Data", 2, 1) },
		"a deleted column":   func(e *env) string { return e.deleteColsOp("Data", 2, 1) },
		"an inserted row":    func(e *env) string { return e.insertRowsOp("Data", 2, 1) },
		"an inserted column": func(e *env) string { return e.insertColsOp("Data", 2, 1) },
		"a sort":             func(e *env) string { return e.sortOp("Data", 1, 3, 1, true) },
		"a cell":             func(e *env) string { return e.setOp("Data!A2", "alice") },
		"a format":           func(e *env) string { return e.formatOp("Data!A2", `{"bold":true}`) },
		"a width": func(e *env) string {
			return jsonf(`{"op":"width","tab":%d,"cols":%s,"width":200}`, e.tabID("Data"), e.colsOf("Data", 1, 1))
		},
		"frozen rows": func(e *env) string {
			return jsonf(`{"op":"settings","tab":%d,"frozen_rows":1,"frozen_cols":0}`, e.tabID("Data"))
		},
		"a name":              func(e *env) string { return jsonf(`{"op":"rename_tab","tab":%d,"name":"Mine"}`, e.tabID("Data")) },
		"a redo of a deleted": func(e *env) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			bothWays(t, func(t *testing.T, e *env) {
				alice, bob, rev, data := setup(e)
				if op := act(e); op != "" {
					alice.do(op)
				} else {
					// The entry waits on the redo stack.
					alice.do(e.deleteRowsOp("Data", 2, 1))
					undo(t, alice)
				}
				replace(t, e, bob, rev, data)
				ack := alice.do(`{"op":"undo"}`)
				if name == "a redo of a deleted" {
					ack = alice.do(`{"op":"redo"}`)
				}
				if ack.Undo == nil || *ack.Undo != recovered {
					t.Fatalf("the answer = %s %+v, want %+v", status(ack), ack.Undo, recovered)
				}
				check(t, e)
			})
		})
	}
	t.Run("the creation of the tab", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice, bob := e.person("Alice"), e.person("Bob")
			alice.do(`{"op":"add_tab","name":"Data"}`)
			for i, text := range []string{"1", "2", "3"} {
				bob.set("Data!A"+string(rune('1'+i)), text)
			}
			bob.do(e.setOp("Data!B1", "b1"), e.setOp("Data!B2", "b2"), e.setOp("Data!B3", "b3"))
			rev, data := closeRevision(e), e.tabID("Data")
			replace(t, e, bob, rev, data)
			// Alice's undo of the tab she created must leave what Bob
			// recovered into it.
			wantUndo(t, "undo of the new tab", undo(t, alice), recovered)
			check(t, e)
		})
	})
	t.Run("the deletion of the tab", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice, bob, rev, data := setup(e)
			alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, data))
			if id, err := bob.recover(rev, data); err != nil || id != data {
				t.Fatalf("recovery = %d %v", id, err)
			}
			wantUndo(t, "undo of the deletion", undo(t, alice), recovered)
			if n := len(e.tabNames()); n != 2 {
				t.Fatalf("%d tabs after an undo over a recovered tab", n)
			}
			check(t, e)
		})
	})
	t.Run("entries made after the recovery apply", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice, bob, rev, data := setup(e)
			replace(t, e, bob, rev, data)
			alice.do(e.deleteRowsOp("Data", 2, 1))
			bob.set("Data!C1", "bob")
			wantUndo(t, "undo of a deletion after the recovery", undo(t, alice), undoAck{Applied: 1})
			wantUndo(t, "undo of the recovery after another writer wrote", undo(t, bob), undoAck{Applied: 1})
			check(t, e)
			_ = data
		})
	})
}

// A recovery admits its undo entry before it commits. When the commit
// fails, the entry and its scratch file go at once, before any other
// change or tick: the pool counts what the stacks hold, and the sheet is
// inside its bounds.
func TestUndoForgetsTheEntryOfAFailedRecovery(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Data","at":"A1","rows":[[1],[2]]}]}`)
	rev, data := e.b.importAll(t), e.tabID("Data")
	alice := e.person("Alice")
	alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, data))
	files, _ := e.scratch()
	// The recovery's payload goes to a scratch file.
	e.in.mu.Lock()
	e.in.undoPool().entryMax = -1
	e.in.fault = func() error { return errors.New("injected commit failure") }
	e.in.mu.Unlock()
	seq, before := e.seq(), e.dump()
	if _, err := alice.recover(rev, data); refusal(err).Code != "unavailable" {
		t.Fatalf("the recovery whose commit fails answered %v", err)
	}
	e.in.mu.Lock()
	e.in.fault = nil
	admitted := len(e.in.scratch.admitted)
	e.in.mu.Unlock()
	if e.seq() != seq || e.dump() != before {
		t.Fatal("the failed recovery changed the sheet")
	}
	if got, _ := e.scratch(); got != files || admitted != 0 {
		t.Fatalf("right after the failed recovery %d scratch files and %d admitted entries, want %d and none", got, admitted, files)
	}
	if pool, stacks := e.residentUndo(); pool != stacks {
		t.Fatalf("right after the failed recovery the pool counts %d bytes and the stacks hold %d", pool, stacks)
	}
	wantCharges(t, e)
	wantUndo(t, "undo of the deletion", undo(t, alice), undoAck{Applied: 1})
	e.want("Data!A2", "2")
}
