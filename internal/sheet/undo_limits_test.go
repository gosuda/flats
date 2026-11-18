package sheet

import (
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// A person's action holds content and format operations, or one operation
// on rows, columns or tabs alone. A mix is refused before it runs, so no
// undo entry ever holds a write into a row its own action deleted.
func TestActionShape(t *testing.T) {
	e := start(t)
	e.mustAgent(numbers)
	alice := e.person("Alice")
	seq := e.seq()
	mixed := [][]string{
		{e.setOp("A2", "new"), e.deleteRowsOp("Sheet1", 2, 1)},
		{e.deleteRowsOp("Sheet1", 2, 1), e.setOp("A2", "new")},
		{e.formatOp("A2", `{"bold":true}`), e.deleteColsOp("Sheet1", 1, 1)},
		{e.insertRowsOp("Sheet1", 1, 1), e.insertRowsOp("Sheet1", 1, 1)},
		{e.sortOp("Sheet1", 1, 3, 1, true), e.setOp("A1", "x")},
		{`{"op":"add_tab","name":"T"}`, e.setOp("A1", "x")},
		{e.setOp("A1", "x"), jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Sheet1"))},
		{e.setOp("A1", "x"), `{"op":"undo"}`},
	}
	for _, ops := range mixed {
		_, err := alice.try(seq, ops...)
		if werr := refusal(err); err == nil || werr.Code != "invalid" || werr.Reason != "action" {
			t.Fatalf("the action %v = %v, want it refused as invalid", ops, err)
		}
		alice.n--
	}
	if e.seq() != seq || e.shown("A2") != "2" {
		t.Fatalf("a refused action changed the sheet")
	}
	// Content and format operations go together, and each structural
	// operation alone.
	if ack := alice.do(e.setOp("A2", "new"), e.formatOp("A2", `{"bold":true}`), e.clearOp("Sheet1", 3, 1, 1, 1)); status(ack) != "applied,applied,applied" {
		t.Fatal(status(ack))
	}
	alice.do(e.deleteRowsOp("Sheet1", 2, 1))
	undo(t, alice)
	wantUndo(t, "undo of the content action", undo(t, alice), undoAck{Applied: 3})
	e.want("A2", "2")
	e.want("A3", "3")
}

// A formula that grew while an undo entry held it is checked like any
// written formula when the undo puts it back. Over the limit, the undo is
// refused whole and can be sent again later. The rename makes the stored
// form longer than nine bytes for each character of the limit.
func TestUndoRefusesFormulaOverLimit(t *testing.T) {
	long := "=" + strings.TrimSuffix(strings.Repeat("D!A1+", 1600), "+")
	longName := strings.Repeat("L", maxTabName)
	if f, err := formula.Parse(long, formula.Pos{Row: 1, Col: 1}, formula.Limits{}); err != nil {
		t.Fatal(err)
	} else if g, _, _ := f.Apply("Sheet1", formula.Pos{Row: 1, Col: 1}, formula.Transform{Kind: formula.RenameTab, Tab: "D", Name: longName}); len(g.Stored()) <= formula.MaxStoredBytes {
		t.Fatalf("the renamed formula has %d stored bytes", len(g.Stored()))
	}
	rename := func(e *env, from, to string) {
		e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"` + from + `","name":"` + to + `"}]}`)
	}
	for name, c := range map[string]struct {
		setup func(e *env, alice *person)
		cell  string
	}{
		"a deleted row":    {func(e *env, alice *person) { alice.do(e.deleteRowsOp("Sheet1", 2, 1)) }, "B2"},
		"a deleted column": {func(e *env, alice *person) { alice.do(e.deleteColsOp("Sheet1", 2, 1)) }, "B2"},
		"a deleted tab": {func(e *env, alice *person) {
			alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Sheet1")))
		}, "B2"},
		"a replaced cell": {func(e *env, alice *person) { alice.set("B2", "short") }, "B2"},
	} {
		t.Run(name, func(t *testing.T) {
			bothWays(t, func(t *testing.T, e *env) {
				e.mustAgent(`{"ops":[{"op":"add_tab","name":"D"},{"op":"write","tab":"D","at":"A1","rows":[[1]]},
					{"op":"write","tab":"Sheet1","at":"A1","rows":[[1],[2,"` + long + `"],[3]]}]}`)
				alice := e.person("Alice")
				c.setup(e, alice)
				text := long
				rename(e, "D", longName)
				seq := e.seq()
				ack := alice.do(`{"op":"undo"}`)
				if status(ack) != "refused:formula_length" || ack.Undo != nil || e.seq() != seq {
					t.Fatalf("undo over the limit = %s %+v at sequence %d, want a refusal at %d", status(ack), ack.Undo, e.seq(), seq)
				}
				e.wantStored("after the refused undo")
				// Every stored formula is within the limit.
				e.in.mu.RLock()
				e.in.wb.eachFormula(func(tab *Tab, row RowID, col ColID, cell *Cell) {
					pos, _ := tab.posOf(row, col)
					if n := cell.f.TextLen(pos); n > formula.MaxFormulaChars {
						t.Errorf("a stored formula has %d characters", n)
					}
				})
				e.in.mu.RUnlock()
				// The entry is whole: once the name is short again, it applies.
				rename(e, longName, "D")
				wantUndo(t, "undo after the rename back", undo(t, alice), undoAck{Applied: 1})
				e.wantText(c.cell, text)
				e.restart()
				e.wantText(c.cell, text)
			})
		})
	}
}

// Rows or columns that return at the end of a tab are back for the
// formulas other entries hold, like rows anywhere else.
func TestUndoFollowsRestoreAtTheEnd(t *testing.T) {
	t.Run("the last row", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1000","rows":[[7]]},{"op":"write","tab":"Sheet1","at":"B1","rows":[["=A1000","=SUM(A999:A1000)"]]}]}`)
			alice, bob := e.person("Alice"), e.person("Bob")
			alice.do(e.setOp("B1", "text"), e.setOp("C1", "more"))
			bob.do(e.deleteRowsOp("Sheet1", 1000, 1))
			wantUndo(t, "undo of the deletion", undo(t, bob), undoAck{Applied: 1})
			wantUndo(t, "undo of the texts", undo(t, alice), undoAck{Applied: 2})
			e.wantText("B1", "=A1000")
			e.want("B1", "7")
			e.wantText("C1", "=SUM(A999:A1000)")
		})
	})
	t.Run("the last columns, twice", func(t *testing.T) {
		bothWays(t, func(t *testing.T, e *env) {
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"Y1","rows":[[3,4]]},{"op":"write","tab":"Sheet1","at":"A2","rows":[["=Y1+Z1"]]}]}`)
			alice, bob := e.person("Alice"), e.person("Bob")
			alice.set("A2", "text")
			bob.do(e.deleteColsOp("Sheet1", 25, 2))
			undo(t, bob)
			redo(t, bob)
			undo(t, bob)
			wantUndo(t, "undo of the text", undo(t, alice), undoAck{Applied: 1})
			e.wantText("A2", "=Y1+Z1")
			e.want("A2", "7")
		})
	})
}
