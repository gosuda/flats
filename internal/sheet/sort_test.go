package sheet

import (
	"fmt"
	"testing"
)

// TestSortKeepsFrozenRows sorts ranges that name frozen rows, through
// every way a sort can start. The frozen rows stay where they are.
func TestSortKeepsFrozenRows(t *testing.T) {
	seed := func(t *testing.T) (*env, *person) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[9,"head"],[3,"c"],[1,"a"],[2,"b"]]},
			{"op":"tab_settings","tab":"Sheet1","frozen_rows":1}]}`)
		return e, e.person("Alice")
	}
	frozen := func(e *env) RowID {
		_, row, _ := e.ids("A1")
		return row
	}
	check := func(t *testing.T, e *env, head RowID, want string) {
		t.Helper()
		if _, row, _ := e.ids("A1"); row != head {
			t.Errorf("the frozen row moved: row %d is at the top, was %d", row, head)
		}
		if got := fmt.Sprint(e.column("Sheet1", "B", 1, 4)); got != want {
			t.Errorf("column B is %s, want %s", got, want)
		}
	}
	t.Run("a person's row ids", func(t *testing.T) {
		e, alice := seed(t)
		head := frozen(e)
		ack := alice.do(e.sortOp("Sheet1", 1, 4, 1, false))
		if status(ack) != "applied" || ack.Results[0].Applied != 3 {
			t.Fatalf("%s %+v", status(ack), ack.Results)
		}
		check(t, e, head, "[head a b c]")
		if ack := alice.do(e.sortOp("Sheet1", 1, 1, 1, true)); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		check(t, e, head, "[head a b c]")
		undo(t, alice)
		check(t, e, head, "[head c a b]")
	})
	t.Run("a person's all", func(t *testing.T) {
		e, alice := seed(t)
		head := frozen(e)
		_, _, col := e.ids("A1")
		alice.do(jsonf(`{"op":"sort","tab":%d,"rows":"all","by":%d,"desc":true}`, e.tabID("Sheet1"), col))
		check(t, e, head, "[head c b a]")
	})
	t.Run("an agent's range", func(t *testing.T) {
		e, _ := seed(t)
		head := frozen(e)
		e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:4","by":"A"}]}`)
		check(t, e, head, "[head a b c]")
		e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:1","by":"A","desc":true}]}`)
		check(t, e, head, "[head a b c]")
	})
	t.Run("an undo after more rows were frozen", func(t *testing.T) {
		e, alice := seed(t)
		head := frozen(e)
		alice.do(e.sortOp("Sheet1", 2, 3, 1, false))
		check(t, e, head, "[head a b c]")
		e.mustAgent(`{"ops":[{"op":"tab_settings","tab":"Sheet1","frozen_rows":2}]}`)
		// The undo puts back the rows that are free to move: b and c.
		undo(t, alice)
		check(t, e, head, "[head a c b]")
	})
}
