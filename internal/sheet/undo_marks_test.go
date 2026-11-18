package sheet

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// bothWays runs a case with undo payloads in memory and with every payload
// in a scratch file.
func bothWays(t *testing.T, run func(t *testing.T, e *env)) {
	for _, spill := range []bool{false, true} {
		name := "in memory"
		if spill {
			name = "in scratch files"
		}
		t.Run(name, func(t *testing.T) {
			e := start(t)
			if spill {
				e.in.mu.Lock()
				e.in.undoPool().entryMax = -1
				e.in.mu.Unlock()
			}
			run(t, e)
		})
	}
}

func redo(t *testing.T, p *person) *undoAck {
	t.Helper()
	ack := p.do(`{"op":"redo"}`)
	if ack.Undo == nil {
		t.Fatalf("redo: %s", status(ack))
	}
	return ack.Undo
}

func wantUndo(t *testing.T, what string, got *undoAck, want undoAck) {
	t.Helper()
	if *got != want {
		t.Fatalf("%s = %+v, want %+v", what, *got, want)
	}
}

// An undo that would remove a row, a column or a tab is skipped once
// another writer has written into it since the action the undo takes back,
// whoever deleted it and brought it back in between (spec rule 23).
func TestUndoKeepsOtherWritersWorkAcrossDeletion(t *testing.T) {
	written := undoAck{Skipped: 1, Reason: "written"}
	// Each axis case works on line 2 of the axis: row 2, or column B.
	for _, axis := range []struct {
		name                 string
		insert, remove, cell func(e *env) string
		lines                func(e *env) int
	}{
		{"rows",
			func(e *env) string { return e.insertRowsOp("Sheet1", 2, 1) },
			func(e *env) string { return e.deleteRowsOp("Sheet1", 2, 1) },
			func(e *env) string { return "C2" },
			func(e *env) int { return len(e.in.wb.tabs[0].rows.ids) }},
		{"columns",
			func(e *env) string { return e.insertColsOp("Sheet1", 2, 1) },
			func(e *env) string { return e.deleteColsOp("Sheet1", 2, 1) },
			func(e *env) string { return "B3" },
			func(e *env) int { return len(e.in.wb.tabs[0].cols.ids) }},
	} {
		t.Run(axis.name, func(t *testing.T) {
			setup := func(e *env) (alice, bob, carol *person, lines int) {
				e.mustAgent(numbers)
				alice, bob, carol = e.person("Alice"), e.person("Bob"), e.person("Carol")
				alice.do(axis.insert(e))
				return alice, bob, carol, axis.lines(e)
			}
			cycle := func(t *testing.T, e *env, p *person) {
				t.Helper()
				p.do(axis.remove(e))
				wantUndo(t, "undo of the deletion", undo(t, p), undoAck{Applied: 1})
			}
			t.Run("the creator deletes and restores", func(t *testing.T) {
				bothWays(t, func(t *testing.T, e *env) {
					alice, bob, _, lines := setup(e)
					cycle(t, e, alice)
					bob.set(axis.cell(e), "bob")
					wantUndo(t, "undo of the insert", undo(t, alice), written)
					if e.shown(axis.cell(e)) != "bob" || axis.lines(e) != lines {
						t.Fatalf("Bob's cell shows %q in %d lines, want his text in %d", e.shown(axis.cell(e)), axis.lines(e), lines)
					}
				})
			})
			t.Run("another writer deletes and restores, twice", func(t *testing.T) {
				bothWays(t, func(t *testing.T, e *env) {
					alice, bob, carol, lines := setup(e)
					cycle(t, e, carol)
					cycle(t, e, carol)
					bob.do(e.formatOp(axis.cell(e), `{"bold":true}`))
					cycle(t, e, bob)
					wantUndo(t, "undo of the insert", undo(t, alice), written)
					if axis.lines(e) != lines {
						t.Fatalf("%d lines, want %d", axis.lines(e), lines)
					}
				})
			})
			t.Run("written before the deletion", func(t *testing.T) {
				bothWays(t, func(t *testing.T, e *env) {
					alice, bob, carol, _ := setup(e)
					bob.set(axis.cell(e), "bob")
					cycle(t, e, carol)
					cycle(t, e, alice)
					wantUndo(t, "undo of the insert", undo(t, alice), written)
					if got := e.shown(axis.cell(e)); got != "bob" {
						t.Fatalf("Bob's cell shows %q", got)
					}
				})
			})
			t.Run("the creator writes after another writer's restore", func(t *testing.T) {
				bothWays(t, func(t *testing.T, e *env) {
					alice, _, carol, lines := setup(e)
					cycle(t, e, carol)
					alice.set(axis.cell(e), "alice")
					// Carol's redo would delete the line with Alice's text.
					wantUndo(t, "redo of the deletion", redo(t, carol), written)
					if e.shown(axis.cell(e)) != "alice" || axis.lines(e) != lines {
						t.Fatalf("Alice's cell shows %q in %d lines", e.shown(axis.cell(e)), axis.lines(e))
					}
					// Alice wrote there herself, so her own history still runs.
					wantUndo(t, "undo of Alice's text", undo(t, alice), undoAck{Applied: 1})
					wantUndo(t, "undo of the insert", undo(t, alice), undoAck{Applied: 1})
					if axis.lines(e) != lines-1 {
						t.Fatalf("%d lines after the creator's undo, want %d", axis.lines(e), lines-1)
					}
				})
			})
			t.Run("one person's own chain", func(t *testing.T) {
				bothWays(t, func(t *testing.T, e *env) {
					alice, _, _, lines := setup(e)
					alice.set(axis.cell(e), "mine")
					cycle(t, e, alice)
					wantUndo(t, "undo of the text", undo(t, alice), undoAck{Applied: 1})
					wantUndo(t, "undo of the insert", undo(t, alice), undoAck{Applied: 1})
					if axis.lines(e) != lines-1 {
						t.Fatalf("%d lines, want %d", axis.lines(e), lines-1)
					}
					wantUndo(t, "redo of the insert", redo(t, alice), undoAck{Applied: 1})
					if axis.lines(e) != lines {
						t.Fatalf("%d lines after the redo, want %d", axis.lines(e), lines)
					}
				})
			})
			t.Run("an agent writes after the restore", func(t *testing.T) {
				bothWays(t, func(t *testing.T, e *env) {
					alice, _, carol, _ := setup(e)
					cycle(t, e, carol)
					e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"` + axis.cell(e) + `","rows":[["agent"]]}]}`)
					wantUndo(t, "undo of the insert", undo(t, alice), written)
					wantUndo(t, "redo of the deletion", redo(t, carol), written)
				})
			})
		})
	}
	t.Run("tabs", func(t *testing.T) {
		setup := func(e *env) (alice, bob, carol *person, tab TabID) {
			alice, bob, carol = e.person("Alice"), e.person("Bob"), e.person("Carol")
			alice.do(`{"op":"add_tab","name":"New"}`)
			return alice, bob, carol, e.tabID("New")
		}
		cycle := func(t *testing.T, e *env, p *person, tab TabID) {
			t.Helper()
			p.do(jsonf(`{"op":"delete_tab","tab":%d}`, tab))
			wantUndo(t, "undo of the deletion", undo(t, p), undoAck{Applied: 1})
		}
		t.Run("another writer deletes and restores", func(t *testing.T) {
			bothWays(t, func(t *testing.T, e *env) {
				alice, bob, carol, tab := setup(e)
				cycle(t, e, carol, tab)
				cycle(t, e, alice, tab)
				bob.set("New!B2", "bob")
				wantUndo(t, "undo of the new tab", undo(t, alice), written)
				e.want("New!B2", "bob")
			})
		})
		t.Run("the creator writes after another writer's restore", func(t *testing.T) {
			bothWays(t, func(t *testing.T, e *env) {
				alice, _, carol, tab := setup(e)
				cycle(t, e, carol, tab)
				alice.set("New!B2", "alice")
				wantUndo(t, "redo of the deletion", redo(t, carol), written)
				e.want("New!B2", "alice")
			})
		})
		t.Run("rows written inside a deleted tab", func(t *testing.T) {
			bothWays(t, func(t *testing.T, e *env) {
				alice, bob, carol, tab := setup(e)
				alice.do(e.insertRowsOp("New", 2, 1))
				bob.do(e.formatOp("New!A2", `{"italic":true}`))
				cycle(t, e, carol, tab)
				wantUndo(t, "undo of the row", undo(t, alice), written)
				wantUndo(t, "undo of the new tab", undo(t, alice), written)
			})
		})
		t.Run("one person's own chain", func(t *testing.T) {
			bothWays(t, func(t *testing.T, e *env) {
				alice, _, _, tab := setup(e)
				alice.set("New!A1", "mine")
				cycle(t, e, alice, tab)
				wantUndo(t, "undo of the text", undo(t, alice), undoAck{Applied: 1})
				wantUndo(t, "undo of the new tab", undo(t, alice), undoAck{Applied: 1})
				if got := tabOrder(e); got != "Sheet1" {
					t.Fatalf("tabs = %s", got)
				}
			})
		})
	})
}

// An action that created several rows or columns is taken back whole or
// not at all: a write by another writer into one of them counts, also when
// that one is deleted by now.
func TestUndoKeepsCreatedLinesTogether(t *testing.T) {
	bothWays(t, func(t *testing.T, e *env) {
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.insertColsOp("Sheet1", 2, 2))
		alice.do(e.insertRowsOp("Sheet1", 2, 2))
		bob.set("B1", "bob")
		bob.set("A2", "bob")
		bob.do(e.deleteColsOp("Sheet1", 2, 1))
		bob.do(e.deleteRowsOp("Sheet1", 2, 1))
		wantUndo(t, "undo of the rows", undo(t, alice), undoAck{Skipped: 1, Reason: "written"})
		wantUndo(t, "undo of the columns", undo(t, alice), undoAck{Skipped: 1, Reason: "written"})
		e.in.mu.RLock()
		rows, cols := len(e.in.wb.tabs[0].rows.ids), len(e.in.wb.tabs[0].cols.ids)
		e.in.mu.RUnlock()
		if rows != 1001 || cols != 27 {
			t.Fatalf("the tab has %d rows and %d columns, want one of the created ones left on each axis", rows, cols)
		}
	})
}

// The marks of rows nobody's undo can ask about are dropped again.
func TestUndoWriteMarksArePruned(t *testing.T) {
	e := start(t)
	rows := make([]string, 3000)
	for i := range rows {
		rows[i] = "[1]"
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.Join(rows, ",") + `]}]}`)
	marks := func() int {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		return len(e.in.wb.tabs[0].marks)
	}
	if marks() != 3000 {
		t.Fatalf("%d row marks after 3,000 rows were written", marks())
	}
	e.tick()
	if marks() != 0 {
		t.Fatalf("%d row marks are kept while no session has an undo entry", marks())
	}
	// With an entry on a stack, the writes after it stay.
	alice, bob := e.person("Alice"), e.person("Bob")
	alice.do(e.insertRowsOp("Sheet1", 2, 1))
	bob.set("C2", "bob")
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[` + strings.Join(rows, ",") + `]}]}`)
	e.tick()
	if marks() != 3000 {
		t.Fatalf("%d row marks while Alice's insert can be undone", marks())
	}
	wantUndo(t, "undo of the insert", undo(t, alice), undoAck{Skipped: 1, Reason: "written"})
}

// A row keeps the stamp of its last sort through a deletion and its undo,
// so the undo of that sort still applies, and still sees another writer's
// sort.
func TestUndoSortAcrossDeletion(t *testing.T) {
	const five = `{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[3,1],[1,2],[5,3],[2,4],[4,5]]}]}`
	order := func(e *env) string { return fmt.Sprint(e.column("Sheet1", "A", 1, 5)) }
	t.Run("the sorter deletes a row and restores it", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			e.mustAgent(five)
			alice := e.person("Alice")
			alice.do(e.sortOp("Sheet1", 1, 5, 1, false)) // 1 2 3 4 5
			alice.do(e.deleteRowsOp("Sheet1", 3, 1))
			wantUndo(t, "undo of the deletion", undo(t, alice), undoAck{Applied: 1})
			wantUndo(t, "undo of the sort", undo(t, alice), undoAck{Applied: 1})
			if got := order(e); got != "[3 1 5 2 4]" {
				t.Fatalf("rows = %s", got)
			}
			wantUndo(t, "redo of the sort", redo(t, alice), undoAck{Applied: 1})
			if got := order(e); got != "[1 2 3 4 5]" {
				t.Fatalf("rows after the redo = %s", got)
			}
		})
	})
	t.Run("another writer deletes a row and restores it", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			e.mustAgent(five)
			alice, bob := e.person("Alice"), e.person("Bob")
			alice.do(e.sortOp("Sheet1", 1, 5, 1, false))
			bob.do(e.deleteRowsAt("Sheet1", 2, 4))
			undo(t, bob)
			wantUndo(t, "undo of the sort", undo(t, alice), undoAck{Applied: 1})
			if got := order(e); got != "[3 1 5 2 4]" {
				t.Fatalf("rows = %s", got)
			}
		})
	})
	t.Run("another writer's sort before the deletion", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			e.mustAgent(five)
			alice, bob := e.person("Alice"), e.person("Bob")
			alice.do(e.sortOp("Sheet1", 1, 5, 1, false))
			bob.do(e.sortOp("Sheet1", 2, 3, 1, true)) // 1 4 3 2 5
			// Only the deleted row carries Bob's stamp when it is back, once
			// Alice has sorted the others again.
			alice.do(e.deleteRowsOp("Sheet1", 3, 1)) // 1 4 2 5
			alice.do(e.sortOp("Sheet1", 1, 4, 1, true))
			undo(t, alice)
			wantUndo(t, "undo of the deletion", undo(t, alice), undoAck{Applied: 1})
			wantUndo(t, "undo of the first sort", undo(t, alice), undoAck{Skipped: 1, Reason: "sorted"})
			if got := order(e); got != "[1 4 3 2 5]" {
				t.Fatalf("rows = %s", got)
			}
		})
	})
	t.Run("another writer's sort while the row was deleted", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			e.mustAgent(five)
			alice, bob := e.person("Alice"), e.person("Bob")
			alice.do(e.sortOp("Sheet1", 1, 5, 1, false))
			alice.do(e.deleteRowsOp("Sheet1", 3, 1))
			bob.do(e.sortOp("Sheet1", 1, 4, 1, true)) // 5 4 2 1
			undo(t, alice)
			wantUndo(t, "undo of the sort", undo(t, alice), undoAck{Skipped: 1, Reason: "sorted"})
		})
	})
}

// An undo or redo step that cannot apply because the sheet would lose its
// only tab, or a tab its only row or column, or because the name is taken,
// is skipped with its reason. The entry leaves its stack, and the entries
// under it stay reachable.
func TestUndoSkipsWhatCannotApply(t *testing.T) {
	t.Run("the only tab left", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice := e.person("Alice")
			alice.do(`{"op":"add_tab","name":"One"}`)
			alice.set("One!A1", "one")
			alice.do(`{"op":"add_tab","name":"Two"}`)
			alice.set("Two!A1", "two")
			e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Sheet1"},{"op":"delete_tab","tab":"One"}]}`)
			wantUndo(t, "undo of the text", undo(t, alice), undoAck{Applied: 1})
			seq := e.seq()
			ack := alice.do(`{"op":"undo"}`)
			if status(ack) != "applied" || ack.Undo == nil || *ack.Undo != (undoAck{Skipped: 1, Reason: "last_tab"}) {
				t.Fatalf("undo of the only tab = %s %+v", status(ack), ack.Undo)
			}
			if e.seq() != seq || tabOrder(e) != "Two" {
				t.Fatalf("a skipped undo changed the sheet: tabs %s", tabOrder(e))
			}
			// The entries under it are reached: the text in the deleted tab
			// and that tab's creation are skipped as gone, and then the
			// stack is empty.
			wantUndo(t, "undo of the text in a deleted tab", undo(t, alice), undoAck{Skipped: 1, Reason: "changed"})
			wantUndo(t, "undo of a deleted tab's creation", undo(t, alice), undoAck{Skipped: 1, Reason: "changed"})
			wantUndo(t, "undo on an empty stack", undo(t, alice), undoAck{})
			// The skipped undos left nothing to redo but the text.
			wantUndo(t, "redo of the text", redo(t, alice), undoAck{Applied: 1})
			e.want("Two!A1", "two")
			wantUndo(t, "redo on an empty stack", redo(t, alice), undoAck{})
		})
	})
	t.Run("a redo that would delete the only tab left", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice := e.person("Alice")
			alice.set("A1", "first")
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"Other"}]}`)
			alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Sheet1")))
			undo(t, alice)
			e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Other"}]}`)
			wantUndo(t, "redo of the deletion", redo(t, alice), undoAck{Skipped: 1, Reason: "last_tab"})
			wantUndo(t, "undo of the text", undo(t, alice), undoAck{Applied: 1})
			e.want("A1", "")
		})
	})
	t.Run("the only rows of a tab", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice := e.person("Alice")
			alice.set("A1", "first")
			alice.do(e.insertRowsOp("Sheet1", 1, 2))
			e.mustAgent(`{"ops":[{"op":"delete_rows","tab":"Sheet1","at":3,"count":1000}]}`)
			wantUndo(t, "undo of the insert", undo(t, alice), undoAck{Skipped: 1, Reason: "empty_tab"})
			e.in.mu.RLock()
			rows := len(e.in.wb.tabs[0].rows.ids)
			e.in.mu.RUnlock()
			if rows != 2 {
				t.Fatalf("the tab has %d rows", rows)
			}
			wantUndo(t, "undo of the text in a deleted row", undo(t, alice), undoAck{Skipped: 1, Reason: "changed"})
			wantUndo(t, "undo on an empty stack", undo(t, alice), undoAck{})
		})
	})
	t.Run("the only columns of a tab", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice := e.person("Alice")
			alice.do(e.insertColsOp("Sheet1", 1, 1))
			e.mustAgent(`{"ops":[{"op":"delete_cols","tab":"Sheet1","at":"B","count":26}]}`)
			wantUndo(t, "undo of the insert", undo(t, alice), undoAck{Skipped: 1, Reason: "empty_tab"})
			wantUndo(t, "undo on an empty stack", undo(t, alice), undoAck{})
		})
	})
	t.Run("a name another tab took", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice, bob := e.person("Alice"), e.person("Bob")
			alice.set("A1", "first")
			alice.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"Mine"}`, e.tabID("Sheet1")))
			bob.do(`{"op":"add_tab","name":"sheet1"}`)
			wantUndo(t, "undo of the rename", undo(t, alice), undoAck{Skipped: 1, Reason: "tab_name_taken"})
			if got := tabOrder(e); got != "Mine,sheet1" {
				t.Fatalf("tabs = %s", got)
			}
			wantUndo(t, "undo of the text", undo(t, alice), undoAck{Applied: 1})
		})
	})
	t.Run("the tab limit keeps the entry for a later undo", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			alice := e.person("Alice")
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"Gone"},{"op":"write","tab":"Gone","at":"A1","rows":[[7]]}]}`)
			alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Gone")))
			var ops []string
			for i := len(e.tabNames()); i < maxTabs; i++ {
				ops = append(ops, fmt.Sprintf(`{"op":"add_tab","name":"T%d"}`, i))
			}
			e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
			if ack := alice.do(`{"op":"undo"}`); status(ack) != "refused:tabs" || ack.Undo != nil {
				t.Fatalf("undo at the tab limit = %s %+v", status(ack), ack.Undo)
			}
			e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"T5"}]}`)
			wantUndo(t, "undo after a tab was deleted", undo(t, alice), undoAck{Applied: 1})
			if !slices.Contains(e.tabNames(), "Gone") {
				t.Fatalf("tabs = %v", e.tabNames())
			}
			e.want("Gone!A1", "7")
		})
	})
}
