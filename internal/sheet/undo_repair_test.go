package sheet

import (
	"fmt"
	"strings"
	"testing"
)

// deleteRowsAt deletes the rows with the given 1-based numbers in one
// operation, as a selection of separate rows does.
func (e *env) deleteRowsAt(tab string, rows ...int) string {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t := e.in.wb.tab(tab)
	ids := ""
	for i, row := range rows {
		if i > 0 {
			ids += ","
		}
		ids += fmt.Sprint(t.rows.ids[row-1])
	}
	return fmt.Sprintf(`{"op":"delete_rows","tab":%d,"ids":[%s]}`, t.id, ids)
}

func (e *env) deleteColsAt(tab string, cols ...int) string {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t := e.in.wb.tab(tab)
	ids := ""
	for i, col := range cols {
		if i > 0 {
			ids += ","
		}
		ids += fmt.Sprint(t.cols.ids[col-1])
	}
	return fmt.Sprintf(`{"op":"delete_cols","tab":%d,"ids":[%s]}`, t.id, ids)
}

func wantTexts(t *testing.T, e *env, want map[string]string) {
	t.Helper()
	for addr, text := range want {
		e.wantText(addr, text)
	}
}

// Undoing a deletion gives formulas their text back under the names their
// tabs have now.
func TestUndoDeleteRepairsAfterRename(t *testing.T) {
	setup := func(t *testing.T) (*env, *person, *person) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"add_tab","name":"Summary"},
			{"op":"write","tab":"Data","at":"A1","rows":[[1],[2],[3],[4],[5]]},
			{"op":"write","tab":"Summary","at":"A1","rows":[["=Data!A3+Data!A4","=SUM(Data!A1:A3)","=Summary!A1+Data!A3"]]},
			{"op":"write","tab":"Data","at":"C1","rows":[["=A3*2","=Data!A3"]]}]}`)
		return e, e.person("Alice"), e.person("Bob")
	}
	rename := func(e *env, from, to string) string {
		return jsonf(`{"op":"rename_tab","tab":%d,"name":%q}`, e.tabID(from), to)
	}
	t.Run("the tab that lost the row", func(t *testing.T) {
		e, alice, bob := setup(t)
		alice.do(e.deleteRowsOp("Data", 3, 1))
		e.wantText("Summary!A1", "=#REF!+Data!A3")
		bob.do(rename(e, "Data", "Info"))
		if u := undo(t, alice); u.Applied != 1 {
			t.Fatalf("undo = %+v", u)
		}
		wantTexts(t, e, map[string]string{
			"Summary!A1": "=Info!A3+Info!A4", "Summary!B1": "=SUM(Info!A1:A3)", "Summary!C1": "=Summary!A1+Info!A3",
			"Info!C1": "=A3*2", "Info!D1": "=Info!A3",
		})
		e.want("Summary!A1", "7")
		e.want("Info!D1", "3")
	})
	t.Run("the tab of the formula", func(t *testing.T) {
		e, alice, bob := setup(t)
		alice.do(e.deleteRowsOp("Data", 3, 1))
		bob.do(rename(e, "Summary", "Totals"))
		bob.do(rename(e, "Data", "Info"))
		bob.do(rename(e, "Info", "Facts"))
		if u := undo(t, alice); u.Applied != 1 {
			t.Fatalf("undo = %+v", u)
		}
		wantTexts(t, e, map[string]string{
			"Totals!A1": "=Facts!A3+Facts!A4", "Totals!B1": "=SUM(Facts!A1:A3)", "Totals!C1": "=Totals!A1+Facts!A3",
		})
		e.want("Totals!C1", "10")
	})
	t.Run("a column, and a redo after the rename", func(t *testing.T) {
		e, alice, bob := setup(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Summary","at":"A2","rows":[["=Data!A1+Data!B1"]]},{"op":"write","tab":"Data","at":"B1","rows":[[10]]}]}`)
		alice.do(e.deleteColsOp("Data", 2, 1))
		e.wantText("Summary!A2", "=Data!A1+#REF!")
		bob.do(rename(e, "Data", "Info"))
		undo(t, alice)
		e.wantText("Summary!A2", "=Info!A1+Info!B1")
		e.want("Summary!A2", "11")
		alice.do(`{"op":"redo"}`)
		e.wantText("Summary!A2", "=Info!A1+#REF!")
		bob.do(rename(e, "Info", "Data"))
		undo(t, alice)
		e.wantText("Summary!A2", "=Data!A1+Data!B1")
	})
}

// One action that deletes separate rows or columns is undone whole: every
// formula it changed gets its text back.
func TestUndoDeleteSeparateRuns(t *testing.T) {
	setup := func(t *testing.T) (*env, *person, *person) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"add_tab","name":"Summary"},
			{"op":"write","tab":"Data","at":"A1","rows":[[1],[2],[3],[4,"=A2*10","=SUM(A1:A3)"],[5],[6]]},
			{"op":"write","tab":"Data","at":"E1","rows":[["=SUM(A1:A4)","=A2+A4","=A5"]]},
			{"op":"write","tab":"Summary","at":"A1","rows":[["=SUM(Data!A1:A4)","=Data!A2+Data!A4","=SUM(Data!A2:A5)"]]}]}`)
		return e, e.person("Alice"), e.person("Bob")
	}
	before := map[string]string{
		"Data!E1": "=SUM(A1:A4)", "Data!F1": "=A2+A4", "Data!G1": "=A5", "Data!B4": "=A2*10", "Data!C4": "=SUM(A1:A3)",
		"Summary!A1": "=SUM(Data!A1:A4)", "Summary!B1": "=Data!A2+Data!A4", "Summary!C1": "=SUM(Data!A2:A5)",
	}
	t.Run("rows", func(t *testing.T) {
		e, alice, _ := setup(t)
		alice.do(e.deleteRowsAt("Data", 2, 4))
		wantTexts(t, e, map[string]string{"Data!E1": "=SUM(A1:A2)", "Data!F1": "=#REF!+#REF!", "Summary!A1": "=SUM(Data!A1:A2)", "Summary!C1": "=SUM(Data!A2:A3)"})
		if u := undo(t, alice); u.Skipped != 0 {
			t.Fatalf("undo = %+v", u)
		}
		wantTexts(t, e, before)
		e.want("Data!E1", "10")
		e.want("Data!B4", "20")
		e.want("Summary!C1", "14")
		// Redo deletes them again and a second undo repairs again.
		alice.do(`{"op":"redo"}`)
		e.wantText("Data!E1", "=SUM(A1:A2)")
		undo(t, alice)
		wantTexts(t, e, before)
	})
	t.Run("three runs", func(t *testing.T) {
		e, alice, _ := setup(t)
		alice.do(e.deleteRowsAt("Data", 5, 2, 4))
		undo(t, alice)
		wantTexts(t, e, before)
		e.want("Summary!C1", "14")
	})
	t.Run("columns", func(t *testing.T) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,2,3,4,5,"=SUM(A1:D1)","=B1+D1"],["=SUM(A1:D1)"]]}]}`)
		alice := e.person("Alice")
		alice.do(e.deleteColsAt("Sheet1", 2, 4))
		wantTexts(t, e, map[string]string{"D1": "=SUM(A1:B1)", "E1": "=#REF!+#REF!", "A2": "=SUM(A1:B1)"})
		undo(t, alice)
		wantTexts(t, e, map[string]string{"F1": "=SUM(A1:D1)", "G1": "=B1+D1", "A2": "=SUM(A1:D1)"})
		e.want("F1", "10")
	})
	t.Run("another writer's row still stops the repair", func(t *testing.T) {
		e, alice, bob := setup(t)
		alice.do(e.deleteRowsAt("Data", 2, 4))
		bob.do(e.insertRowsOp("Data", 1, 1))
		undo(t, alice)
		// The ranges are left as the deletion and the insert made them; the
		// restored rows only grow a range they land inside.
		e.wantText("Data!F2", "=#REF!+#REF!")
		e.wantText("Summary!B1", "=#REF!+#REF!")
		e.want("Data!A3", "2")
		e.want("Data!A5", "4")
	})
	t.Run("rows between the runs after another writer's insert", func(t *testing.T) {
		e, alice, bob := setup(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Data","at":"B5","rows":[["=A3+A6","=A4+A1","=SUM(A3:A6)"]]}]}`)
		alice.do(e.deleteRowsAt("Data", 2, 5))
		// Bob inserts a row above the row that holds 4.
		bob.do(e.insertRowsOp("Data", 3, 1))
		undo(t, alice)
		e.want("Data!A6", "5")
		wantTexts(t, e, map[string]string{"Data!B6": "=A3+A7", "Data!C6": "=A5+A1", "Data!D6": "=SUM(A3:A7)"})
		e.want("Data!B6", "9")
		e.want("Data!C6", "5")
	})
	t.Run("formulas of a deleted row after another writer's column", func(t *testing.T) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,2,3,4,5],[],[10,20,"=A3+$A$1+E3",40,50]]}]}`)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.deleteRowsOp("Sheet1", 3, 1))
		bob.do(e.insertColsOp("Sheet1", 2, 1))
		bob.do(e.insertRowsOp("Sheet1", 1, 2))
		undo(t, alice)
		e.wantText("D5", "=A5+$A$3+F5")
		e.want("D5", "61")
	})
	t.Run("formulas of deleted columns after another writer's row", func(t *testing.T) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,2,3,4,5,6],[10,"=C1+A2",30,40,"=C2+F1+B1",60]]}]}`)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.deleteColsAt("Sheet1", 2, 5))
		// Bob inserts a column before the one that holds 4 and a row between
		// the two rows.
		bob.do(e.insertColsOp("Sheet1", 3, 1))
		bob.do(e.insertRowsOp("Sheet1", 2, 1))
		undo(t, alice)
		wantTexts(t, e, map[string]string{"B3": "=C1+A3", "F3": "=C3+G1+B1"})
		e.want("B3", "13")
		e.want("F3", "38")
	})
}

// Deleted rows return to the place they had among the rows that remain,
// and the formulas in them read the cells they read before, whatever was
// inserted or deleted beside that place meanwhile.
func TestUndoDeletePlace(t *testing.T) {
	setup := func(t *testing.T, rows string) (*env, *person, *person) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":` + rows + `}]}`)
		return e, e.person("Alice"), e.person("Bob")
	}
	column := func(e *env, n int) string { return fmt.Sprint(e.column("Sheet1", "A", 1, n)) }
	t.Run("rows deleted one after another from the top", func(t *testing.T) {
		e, alice, bob := setup(t, `[[1,"=A1+A9"],[2,"=A2+A9"],[3,"=A3+A9"],[4],[5],[6],[7],[8],[9]]`)
		for range 3 {
			alice.do(e.deleteRowsOp("Sheet1", 1, 1))
		}
		bob.do(e.insertRowsOp("Sheet1", 1, 5))
		for i := 3; i >= 1; i-- {
			if u := undo(t, alice); u.Applied != 1 || u.Skipped != 0 {
				t.Fatalf("undo %d = %+v", i, u)
			}
			e.want("A6", fmt.Sprint(i))
			e.wantText("B6", fmt.Sprintf("=A6+A%d", 15-i))
		}
		if got := column(e, 9); got != "[     1 2 3 4]" {
			t.Fatalf("rows = %s", got)
		}
		wantTexts(t, e, map[string]string{"B6": "=A6+A14", "B7": "=A7+A14", "B8": "=A8+A14"})
		e.want("B8", "12")
	})
	t.Run("a row put under the row above", func(t *testing.T) {
		e, alice, bob := setup(t, `[[1],[2,"=A1+A3"],[3]]`)
		alice.do(e.deleteRowsOp("Sheet1", 2, 1))
		bob.do(e.insertRowsOp("Sheet1", 2, 1))
		bob.set("A2", "new")
		undo(t, alice)
		if got := column(e, 4); got != "[1 2 new 3]" {
			t.Fatalf("rows = %s", got)
		}
		e.wantText("B2", "=A1+A4")
		e.want("B2", "4")
	})
	t.Run("rows put before the row below after the row above went", func(t *testing.T) {
		e, alice, bob := setup(t, `[[1],[2,"=A3+A4"],[3],[4]]`)
		alice.do(e.deleteRowsOp("Sheet1", 2, 1))
		bob.do(e.deleteRowsOp("Sheet1", 1, 1))
		bob.do(e.insertRowsOp("Sheet1", 1, 2))
		undo(t, alice)
		if got := column(e, 5); got != "[  2 3 4]" {
			t.Fatalf("rows = %s", got)
		}
		e.wantText("B3", "=A4+A5")
		e.want("B3", "7")
	})
	t.Run("the row above deleted after a row was put under it", func(t *testing.T) {
		e, alice, bob := setup(t, `[[1],[2,"=A3*2"],[3]]`)
		alice.do(e.deleteRowsOp("Sheet1", 2, 1))
		bob.do(e.insertRowsOp("Sheet1", 2, 1))
		bob.set("A2", "new")
		bob.do(e.deleteRowsOp("Sheet1", 1, 1))
		undo(t, alice)
		// The row keeps its place over the row that was put under it while
		// its upper neighbor stood.
		if got := column(e, 3); got != "[2 new 3]" {
			t.Fatalf("rows = %s", got)
		}
		e.wantText("B1", "=A3*2")
		e.want("B1", "6")
	})
	t.Run("the row above comes back", func(t *testing.T) {
		e, alice, bob := setup(t, `[[1],[2,"=A1+A3"],[3]]`)
		alice.do(e.deleteRowsOp("Sheet1", 2, 1))
		bob.do(e.deleteRowsOp("Sheet1", 1, 1))
		undo(t, bob)
		undo(t, alice)
		if got := column(e, 3); got != "[1 2 3]" {
			t.Fatalf("rows = %s", got)
		}
		// The reference to the row Bob deleted came back with it.
		e.wantText("B2", "=A1+A3")
		e.want("B2", "4")
	})
	t.Run("a sort of the rows around the place", func(t *testing.T) {
		e, alice, bob := setup(t, `[[1],[2,"=A2*10"],[3],[4]]`)
		alice.do(e.deleteRowsOp("Sheet1", 2, 1))
		bob.do(e.sortOp("Sheet1", 1, 3, 1, true)) // 4 3 1
		undo(t, alice)
		if got := column(e, 4); got != "[4 3 1 2]" {
			t.Fatalf("rows = %s", got)
		}
		e.want("B4", "20")
	})
	t.Run("columns deleted one after another", func(t *testing.T) {
		e, alice, bob := setup(t, `[[1,2,3,4,5],["=A1+E1","=B1+E1","=C1+E1"]]`)
		for range 3 {
			alice.do(e.deleteColsOp("Sheet1", 1, 1))
		}
		bob.do(e.insertColsOp("Sheet1", 1, 2))
		for range 3 {
			undo(t, alice)
		}
		wantTexts(t, e, map[string]string{"C2": "=C1+G1", "D2": "=D1+G1", "E2": "=E1+G1"})
		e.want("E2", "8")
	})
}

// A formula in a deleted row keeps its references to rows that were deleted
// later and came back before the row itself returns. Rows that stay
// deleted are gone for it as for any formula.
func TestUndoDeleteKeepsLaterDeletedReferences(t *testing.T) {
	setup := func(t *testing.T) (*env, *person, *person) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=A2+A4+A6","=SUM(A2:A5)"],[2,"=A1+A3"],[3],[4],[5],[6]]}]}`)
		return e, e.person("Alice"), e.person("Bob")
	}
	whole := map[string]string{"B1": "=A2+A4+A6", "C1": "=SUM(A2:A5)"}
	t.Run("the next row, undone in turn", func(t *testing.T) {
		e, alice, _ := setup(t)
		alice.do(e.deleteRowsOp("Sheet1", 1, 1))
		alice.do(e.deleteRowsOp("Sheet1", 1, 1))
		undo(t, alice)
		undo(t, alice)
		wantTexts(t, e, whole)
		e.want("B1", "12")
		e.want("C1", "14")
	})
	t.Run("rows further down, undone in turn", func(t *testing.T) {
		e, alice, _ := setup(t)
		alice.do(e.deleteRowsOp("Sheet1", 1, 1))
		alice.do(e.deleteRowsOp("Sheet1", 3, 1)) // the row that holds 4
		alice.do(e.deleteRowsOp("Sheet1", 1, 1)) // the row that holds 2
		for range 3 {
			undo(t, alice)
		}
		wantTexts(t, e, whole)
		e.want("C1", "14")
	})
	t.Run("another writer's rows, deleted and brought back", func(t *testing.T) {
		e, alice, bob := setup(t)
		alice.do(e.deleteRowsOp("Sheet1", 1, 1))
		bob.do(e.deleteRowsOp("Sheet1", 3, 2)) // the rows that hold 4 and 5
		bob.do(e.insertRowsOp("Sheet1", 1, 2))
		undo(t, bob)
		undo(t, bob)
		undo(t, alice)
		wantTexts(t, e, whole)
	})
	t.Run("another writer's row stays deleted", func(t *testing.T) {
		e, alice, bob := setup(t)
		alice.do(e.deleteRowsOp("Sheet1", 1, 1))
		bob.do(e.deleteRowsOp("Sheet1", 3, 1)) // the row that holds 4
		undo(t, alice)
		wantTexts(t, e, map[string]string{"B1": "=A2+#REF!+A5", "C1": "=SUM(A2:A4)"})
		e.want("C1", "10")
	})
	t.Run("a row that came back and went again", func(t *testing.T) {
		e, alice, bob := setup(t)
		alice.do(e.deleteRowsOp("Sheet1", 1, 1))
		bob.do(e.deleteRowsOp("Sheet1", 3, 1))
		undo(t, bob)
		bob.do(`{"op":"redo"}`)
		undo(t, alice)
		wantTexts(t, e, map[string]string{"B1": "=A2+#REF!+A5", "C1": "=SUM(A2:A4)"})
	})
	t.Run("a sort after the later deletion", func(t *testing.T) {
		e, alice, bob := setup(t)
		alice.do(e.deleteRowsOp("Sheet1", 1, 1))
		bob.do(e.deleteRowsOp("Sheet1", 3, 1))
		bob.do(e.sortOp("Sheet1", 1, 4, 1, true))
		undo(t, alice)
		// The sort left the row Bob deleted no place among the others, so
		// it is gone for the formula. The row itself returns before the row
		// that was below it, and its formula moves with it as in a sort.
		e.want("A4", "1")
		e.wantText("B4", "=A5+#REF!+A8")
	})
	t.Run("more later deletions than an entry keeps apart", func(t *testing.T) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["=SUM(A2:A200)"]]},{"op":"write","tab":"Sheet1","at":"A2","rows":[[1],[1],[1]]},{"op":"write","tab":"Sheet1","at":"A150","rows":[[7]]}]}`)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.deleteRowsOp("Sheet1", 1, 1))
		for i := 0; i < gapsKept+8; i++ {
			bob.do(e.deleteRowsOp("Sheet1", 10, 1))
		}
		undo(t, alice)
		e.wantText("A1", fmt.Sprintf("=SUM(A2:A%d)", 200-gapsKept-8))
		e.want("A1", "10")
	})
}

// An entry keeps up to 32 later deletions apart, so that the rows they took
// can return to its formulas. With a 33rd, the oldest is applied: its rows
// are gone for the formulas, also when they return. Deletions in a tab the
// entry does not depend on are not among the 32.
func TestUndoKeepsThirtyTwoLaterDeletions(t *testing.T) {
	// Row 1 holds a formula that reads rows 2 to 41, one by one.
	refs := make([]string, 40)
	values := make([]string, 40)
	for i := range refs {
		refs[i] = fmt.Sprintf("A%d", i+2)
		values[i] = fmt.Sprintf("[%d]", i+2)
	}
	whole := "=" + strings.Join(refs, "+")
	// run deletes row 1, then own rows of the same tab one by one from the
	// top, each a deletion of its own, with other deletions in another tab
	// between them. Everything but the first deletion is taken back, last
	// first, and then the first.
	run := func(t *testing.T, e *env, own, other int) {
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"add_tab","name":"Other"},{"op":"write","tab":"Sheet1","at":"A1","rows":[[%q],%s]}]}`, whole, strings.Join(values, ",")))
		alice, bob, carol := e.person("Alice"), e.person("Bob"), e.person("Carol")
		alice.do(e.deleteRowsOp("Sheet1", 1, 1))
		for i := 0; i < max(own, other); i++ {
			if i < own {
				if ack := bob.do(e.deleteRowsOp("Sheet1", 1, 1)); status(ack) != "applied" {
					t.Fatal(status(ack))
				}
			}
			if i < other {
				carol.do(e.deleteRowsOp("Other", 1, 1))
			}
		}
		for i := 0; i < own; i++ {
			wantUndo(t, fmt.Sprintf("Bob's undo %d", i+1), undo(t, bob), undoAck{Applied: 1})
		}
		wantUndo(t, "Alice's undo", undo(t, alice), undoAck{Applied: 1})
		if got := e.column("Sheet1", "A", 2, 41); got[0] != "2" || got[39] != "41" {
			t.Fatalf("the rows did not return to their places: %v", got)
		}
	}
	t.Run("32 deletions", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			run(t, e, gapsKept, 0)
			e.wantText("A1", whole)
			e.want("A1", "860")
		})
	})
	t.Run("33 deletions", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			run(t, e, gapsKept+1, 0)
			// The first of Bob's deletions took the row that held 2. It was
			// applied when the 33rd came, and the row's return does not
			// give the reference back.
			e.wantText("A1", "=#REF!+"+strings.Join(refs[1:], "+"))
			e.want("A1", "#REF!")
		})
	})
	t.Run("32 deletions and 40 in another tab", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			run(t, e, gapsKept, 40)
			e.wantText("A1", whole)
		})
	})
}

// TestRefusedUndoLeavesItsEntryWhole refuses an undo whose entry holds a
// formula that reads a row another person deleted since. The refusal
// leaves the entry as it was: when the row returns and the undo is sent
// again, the formula reads the row as before.
func TestRefusedUndoLeavesItsEntryWhole(t *testing.T) {
	e := start(t)
	// 1,600 references to the tab D: 7,999 characters, inside the limit of
	// 8,192 only while the tab has a name of one letter.
	long := "=" + strings.TrimSuffix(strings.Repeat("D!A1+", 1600), "+")
	e.mustAgent(jsonf(`{"ops":[{"op":"add_tab","name":"D"},{"op":"write","tab":"D","at":"A1","rows":[[1]]},
		{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=A3*2"],[2,%q],[3],[4],[5]]}]}`, long))
	e.want("B1", "6")
	alice, bob := e.person("Alice"), e.person("Bob")
	alice.do(e.setOp("B1", "x"), e.setOp("B2", "y"))
	bob.do(e.deleteRowsOp("Sheet1", 3, 1))
	bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"Dlong"}`, e.tabID("D")))
	seq := e.seq()
	if ack := alice.do(`{"op":"undo"}`); status(ack) != "refused:formula_length" {
		t.Fatalf("the undo of a formula that has grown past the limit: %s", status(ack))
	}
	if e.seq() != seq || e.text("B1") != "x" || e.text("B2") != "y" {
		t.Fatalf("a refused undo changed the sheet: sequence %d, B1 %q, B2 %q", e.seq(), e.text("B1"), e.text("B2"))
	}
	// Bob takes both of his actions back: the tab has its short name and
	// the row is there again.
	for i := 0; i < 2; i++ {
		if u := undo(t, bob); u.Applied != 1 {
			t.Fatalf("Bob's undo %d = %+v", i+1, u)
		}
	}
	if u := undo(t, alice); u.Applied != 2 || u.Skipped != 0 {
		t.Fatalf("the undo sent again = %+v", u)
	}
	e.wantText("B1", "=A3*2")
	e.want("B1", "6")
	e.wantText("B2", long)
}
