package sheet

import (
	"slices"
	"strings"
	"testing"
)

// sortable has three rows whose three columns each order them differently.
const sortable = `{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["a",2,30],["b",3,10],["c",1,20]]}]}`

func rowsOrder(e *env) string { return strings.Join(e.column("Sheet1", "A", 1, 3), "") }

// Another writer's sort stays protected from a sort undo, also after the
// person sorted the same rows again and undid that.
func TestUndoSortKeepsOtherWritersSort(t *testing.T) {
	t.Run("after a later sort of the person's own", func(t *testing.T) {
		e := start(t)
		e.mustAgent(sortable)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.sortOp("Sheet1", 1, 3, 2, false)) // c a b
		bob.do(e.sortOp("Sheet1", 1, 3, 3, false))   // b c a
		// Alice's second sort, and its undo, move every row.
		alice.do(e.sortOp("Sheet1", 1, 3, 1, false)) // a b c
		if u := undo(t, alice); u.Applied != 1 || rowsOrder(e) != "bca" {
			t.Fatalf("undo of the latest sort = %+v, rows %s", u, rowsOrder(e))
		}
		if u := undo(t, alice); u.Applied != 0 || u.Skipped != 1 || u.Reason != "sorted" || rowsOrder(e) != "bca" {
			t.Fatalf("undo over Bob's sort = %+v, rows %s", u, rowsOrder(e))
		}
	})
	t.Run("a sort that left the shared rows in place", func(t *testing.T) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["a",2,2],["b",3,3],["c",1,1],["d",9,5],["e",9,4]]}]}`)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.sortOp("Sheet1", 1, 3, 2, false)) // c a b d e
		// Bob's sort of all five rows confirms the order of the first three
		// and swaps the last two.
		bob.do(e.sortOp("Sheet1", 1, 5, 3, false))
		order := func() string { return strings.Join(e.column("Sheet1", "A", 1, 5), "") }
		if order() != "cabed" {
			t.Fatalf("rows = %s", order())
		}
		if u := undo(t, alice); u.Applied != 0 || u.Reason != "sorted" || order() != "cabed" {
			t.Fatalf("undo over Bob's sort = %+v, rows %s", u, order())
		}
	})
}

// A sort protects the order of the rows it names also when they are in that
// order already and nothing moves (Appendix B, engine item 16): Bob's sort,
// by a person or by an agent, confirms the order Alice's sort made, and
// her undo is skipped. The sort takes a sequence, as an action that stamps
// its rows. An undo of such a sort finds the rows in the earlier order and
// moves none of them, but stamps them and takes a sequence too (engine item
// 23).
func TestUndoSortThatMovedNothingProtects(t *testing.T) {
	const twoRows = `{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[2],[1]]}]}`
	order := func(e *env) string { return strings.Join(e.column("Sheet1", "A", 1, 2), "") }
	for _, by := range []string{"person", "agent"} {
		t.Run(by, func(t *testing.T) {
			e := start(t)
			e.mustAgent(twoRows)
			alice, bob := e.person("Alice"), e.person("Bob")
			if ack := alice.do(e.sortOp("Sheet1", 1, 2, 1, false)); status(ack) != "applied" || order(e) != "12" {
				t.Fatalf("Alice's sort: %s, rows %s", status(ack), order(e))
			}
			seq := e.seq()
			if by == "person" {
				if ack := bob.do(e.sortOp("Sheet1", 1, 2, 1, false)); status(ack) != "applied" {
					t.Fatalf("Bob's sort: %s", status(ack))
				}
			} else {
				e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:2","by":"A"}]}`)
			}
			if order(e) != "12" || e.seq() == seq {
				t.Fatalf("the second sort: rows %s, sequence %d after %d", order(e), e.seq(), seq)
			}
			wantUndo(t, "Alice's undo", undo(t, alice), undoAck{Skipped: 1, Reason: "sorted"})
			if order(e) != "12" {
				t.Fatalf("Alice's undo over the second sort left rows %s", order(e))
			}
			if by != "person" {
				return
			}
			seq = e.seq()
			wantUndo(t, "Bob's undo", undo(t, bob), undoAck{Applied: 1})
			if order(e) != "12" || e.seq() == seq {
				t.Fatalf("Bob's undo of a sort that moved nothing: rows %s, sequence %d after %d", order(e), e.seq(), seq)
			}
		})
	}
}

// Consecutive sort undos and redos walk the person's own history.
func TestUndoRepeatedSorts(t *testing.T) {
	e := start(t)
	e.mustAgent(sortable)
	alice := e.person("Alice")
	alice.do(e.sortOp("Sheet1", 1, 3, 2, false)) // c a b
	alice.do(e.sortOp("Sheet1", 1, 3, 3, false)) // b c a
	alice.do(e.sortOp("Sheet1", 1, 3, 1, true))  // c b a
	for _, want := range []string{"bca", "cab", "abc"} {
		if u := undo(t, alice); u.Applied != 1 || rowsOrder(e) != want {
			t.Fatalf("undo = %+v, rows %s, want %s", u, rowsOrder(e), want)
		}
	}
	for _, want := range []string{"cab", "bca", "cba"} {
		if u := alice.do(`{"op":"redo"}`).Undo; u == nil || u.Applied != 1 || rowsOrder(e) != want {
			t.Fatalf("redo = %+v, rows %s, want %s", u, rowsOrder(e), want)
		}
	}
	for _, want := range []string{"bca", "cab", "abc"} {
		if u := undo(t, alice); u.Applied != 1 || rowsOrder(e) != want {
			t.Fatalf("second undo = %+v, rows %s, want %s", u, rowsOrder(e), want)
		}
	}
	if got := e.column("Sheet1", "C", 1, 3); !slices.Equal(got, []string{"30", "10", "20"}) {
		t.Fatalf("rows = %v", got)
	}
}

// A person who sorts rows and sorts them the same way again walks back
// through both sorts: the undo of the sort that moved nothing gives the
// rows stamps that stand in for those of the first sort, and takes a
// sequence for them (Appendix B, engine item 23). A confirming sort of
// another writer, before or after the person's second sort, still stops
// the undo of the first. Every case runs with the payloads in memory and
// in scratch files.
func TestUndoRepeatedSortThatMovedNothing(t *testing.T) {
	const twoRows = `{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[2],[1]]}]}`
	order := func(e *env) string { return strings.Join(e.column("Sheet1", "A", 1, 2), "") }
	defer func() { undoSpillAll = false }()
	for _, spill := range []bool{false, true} {
		undoSpillAll = spill
		name := map[bool]string{false: "in memory", true: "in files"}[spill]
		t.Run(name+"/the person's own sorts", func(t *testing.T) {
			e := start(t)
			e.mustAgent(twoRows)
			alice := e.person("Alice")
			alice.do(e.sortOp("Sheet1", 1, 2, 1, false))
			alice.do(e.sortOp("Sheet1", 1, 2, 1, false))
			for round := range 2 {
				seq := e.seq()
				wantUndo(t, "the undo of the second sort", undo(t, alice), undoAck{Applied: 1})
				if order(e) != "12" || e.seq() == seq {
					t.Fatalf("round %d: the undo of the second sort: rows %s, sequence %d after %d", round, order(e), e.seq(), seq)
				}
				wantUndo(t, "the undo of the first sort", undo(t, alice), undoAck{Applied: 1})
				if order(e) != "21" {
					t.Fatalf("round %d: the undo of the first sort left rows %s", round, order(e))
				}
				if round == 1 {
					break
				}
				wantUndo(t, "the redo of the first sort", redo(t, alice), undoAck{Applied: 1})
				if order(e) != "12" {
					t.Fatalf("the redo of the first sort left rows %s", order(e))
				}
				seq = e.seq()
				wantUndo(t, "the redo of the second sort", redo(t, alice), undoAck{Applied: 1})
				if order(e) != "12" || e.seq() == seq {
					t.Fatalf("the redo of the second sort: rows %s, sequence %d after %d", order(e), e.seq(), seq)
				}
			}
			if files, _ := e.scratch(); spill && files == 0 {
				t.Fatal("no payload went to a scratch file")
			}
		})
		for _, by := range []string{"person", "agent"} {
			t.Run(name+"/a confirming sort of another "+by+" after the second", func(t *testing.T) {
				e := start(t)
				e.mustAgent(twoRows)
				alice, bob := e.person("Alice"), e.person("Bob")
				alice.do(e.sortOp("Sheet1", 1, 2, 1, false))
				alice.do(e.sortOp("Sheet1", 1, 2, 1, false))
				if by == "person" {
					bob.do(e.sortOp("Sheet1", 1, 2, 1, false))
				} else {
					e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:2","by":"A"}]}`)
				}
				wantUndo(t, "Alice's undo", undo(t, alice), undoAck{Skipped: 1, Reason: "sorted"})
				wantUndo(t, "Alice's next undo", undo(t, alice), undoAck{Skipped: 1, Reason: "sorted"})
				if order(e) != "12" {
					t.Fatalf("Alice's undos over the confirming sort left rows %s", order(e))
				}
			})
		}
		t.Run(name+"/a confirming sort of another person, undone", func(t *testing.T) {
			e := start(t)
			e.mustAgent(twoRows)
			alice, bob := e.person("Alice"), e.person("Bob")
			alice.do(e.sortOp("Sheet1", 1, 2, 1, false))
			bob.do(e.sortOp("Sheet1", 1, 2, 1, false))
			// Bob's undo stamps the rows. Its stamp stands in for Alice's
			// only in Bob's history, as a value another writer wrote and
			// took back does for a cell.
			wantUndo(t, "Bob's undo", undo(t, bob), undoAck{Applied: 1})
			wantUndo(t, "Alice's undo", undo(t, alice), undoAck{Skipped: 1, Reason: "sorted"})
			if order(e) != "12" {
				t.Fatalf("the undos left rows %s", order(e))
			}
		})
		t.Run(name+"/a confirming sort of another person before the second", func(t *testing.T) {
			e := start(t)
			e.mustAgent(twoRows)
			alice, bob := e.person("Alice"), e.person("Bob")
			alice.do(e.sortOp("Sheet1", 1, 2, 1, false))
			bob.do(e.sortOp("Sheet1", 1, 2, 1, false))
			alice.do(e.sortOp("Sheet1", 1, 2, 1, false))
			// The undo of Alice's second sort gives the rows back Bob's
			// order, which her first sort's undo must not take.
			wantUndo(t, "the undo of the second sort", undo(t, alice), undoAck{Applied: 1})
			wantUndo(t, "the undo of the first sort", undo(t, alice), undoAck{Skipped: 1, Reason: "sorted"})
			if order(e) != "12" {
				t.Fatalf("Alice's undos left rows %s", order(e))
			}
		})
	}
}
