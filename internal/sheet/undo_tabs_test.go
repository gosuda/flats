package sheet

import (
	"slices"
	"strings"
	"testing"
)

func tabOrder(e *env) string { return strings.Join(e.tabNames(), ",") }

func moveTabOp(e *env, tab, before string) string {
	if before == "" {
		return jsonf(`{"op":"move_tab","tab":%d}`, e.tabID(tab))
	}
	return jsonf(`{"op":"move_tab","tab":%d,"before":%d}`, e.tabID(tab), e.tabID(before))
}

func fourTabs(t *testing.T) (*env, *person, *person) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"A"},{"op":"add_tab","name":"B"},{"op":"add_tab","name":"C"},{"op":"add_tab","name":"D"}]}`)
	return e, e.person("Alice"), e.person("Bob")
}

// Consecutive undos of tab names, settings and tab moves walk back through
// the person's own history, and stop at another writer's change.
func TestUndoRepeatedTabGroups(t *testing.T) {
	t.Run("names", func(t *testing.T) {
		e := start(t)
		alice := e.person("Alice")
		tab := e.tabID("Sheet1")
		for _, name := range []string{"A", "B", "C"} {
			alice.do(jsonf(`{"op":"rename_tab","tab":%d,"name":%q}`, tab, name))
		}
		for _, want := range []string{"B", "A", "Sheet1"} {
			if u := undo(t, alice); u.Applied != 1 || tabOrder(e) != want {
				t.Fatalf("undo = %+v, tabs %s, want %s", u, tabOrder(e), want)
			}
		}
		for _, want := range []string{"A", "B", "C"} {
			if u := alice.do(`{"op":"redo"}`).Undo; u == nil || u.Applied != 1 || tabOrder(e) != want {
				t.Fatalf("redo = %+v, tabs %s, want %s", u, tabOrder(e), want)
			}
		}
		for _, want := range []string{"B", "A", "Sheet1"} {
			if u := undo(t, alice); u.Applied != 1 || tabOrder(e) != want {
				t.Fatalf("second undo = %+v, tabs %s, want %s", u, tabOrder(e), want)
			}
		}
	})
	t.Run("names keep another writer's rename", func(t *testing.T) {
		e := start(t)
		alice, bob := e.person("Alice"), e.person("Bob")
		tab := e.tabID("Sheet1")
		alice.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"A"}`, tab))
		bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"Bob"}`, tab))
		alice.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"C"}`, tab))
		if u := undo(t, alice); u.Applied != 1 || tabOrder(e) != "Bob" {
			t.Fatalf("undo = %+v, tabs %s", u, tabOrder(e))
		}
		if u := undo(t, alice); u.Applied != 0 || u.Skipped != 1 || tabOrder(e) != "Bob" {
			t.Fatalf("undo over Bob's name = %+v, tabs %s", u, tabOrder(e))
		}
	})
	frozen := func(e *env) [2]int {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		return [2]int{e.in.wb.tabs[0].frozenRows, e.in.wb.tabs[0].frozenCols}
	}
	t.Run("settings", func(t *testing.T) {
		e := start(t)
		alice := e.person("Alice")
		tab := e.tabID("Sheet1")
		for rows := 1; rows <= 3; rows++ {
			alice.do(jsonf(`{"op":"settings","tab":%d,"frozen_rows":%d,"frozen_cols":0}`, tab, rows))
		}
		for _, want := range []int{2, 1, 0} {
			if u := undo(t, alice); u.Applied != 1 || frozen(e)[0] != want {
				t.Fatalf("undo = %+v, frozen %v, want %d rows", u, frozen(e), want)
			}
		}
		for _, want := range []int{1, 2, 3} {
			if u := alice.do(`{"op":"redo"}`).Undo; u == nil || u.Applied != 1 || frozen(e)[0] != want {
				t.Fatalf("redo = %+v, frozen %v, want %d rows", u, frozen(e), want)
			}
		}
	})
	t.Run("settings keep another writer's change", func(t *testing.T) {
		e := start(t)
		alice, bob := e.person("Alice"), e.person("Bob")
		tab := e.tabID("Sheet1")
		alice.do(jsonf(`{"op":"settings","tab":%d,"frozen_rows":1,"frozen_cols":0}`, tab))
		bob.do(jsonf(`{"op":"settings","tab":%d,"frozen_rows":1,"frozen_cols":2}`, tab))
		alice.do(jsonf(`{"op":"settings","tab":%d,"frozen_rows":3,"frozen_cols":2}`, tab))
		undo(t, alice)
		if u := undo(t, alice); u.Applied != 0 || u.Skipped != 1 || frozen(e) != [2]int{1, 2} {
			t.Fatalf("undo over Bob's settings = %+v, frozen %v", u, frozen(e))
		}
	})
	t.Run("widths", func(t *testing.T) {
		e := start(t)
		alice := e.person("Alice")
		width := func() int {
			e.in.mu.RLock()
			defer e.in.mu.RUnlock()
			tab := e.in.wb.tabs[0]
			return tab.widthOf(ColID(tab.cols.ids[0]))
		}
		for _, px := range []int{120, 140, 160} {
			alice.do(jsonf(`{"op":"width","tab":%d,"cols":%s,"width":%d}`, e.tabID("Sheet1"), e.colsOf("Sheet1", 1, 1), px))
		}
		for _, want := range []int{140, 120, defaultWidth} {
			if u := undo(t, alice); u.Applied != 1 || width() != want {
				t.Fatalf("undo = %+v, width %d, want %d", u, width(), want)
			}
		}
	})
	t.Run("tab order", func(t *testing.T) {
		e, alice, _ := fourTabs(t)
		alice.do(moveTabOp(e, "D", "A")) // D,A,B,C
		alice.do(moveTabOp(e, "C", "D")) // C,D,A,B
		alice.do(moveTabOp(e, "A", ""))  // C,D,B,A
		for _, want := range []string{"C,D,A,B", "D,A,B,C", "A,B,C,D"} {
			if u := undo(t, alice); u.Applied != 1 || tabOrder(e) != want {
				t.Fatalf("undo = %+v, tabs %s, want %s", u, tabOrder(e), want)
			}
		}
		for _, want := range []string{"D,A,B,C", "C,D,A,B", "C,D,B,A"} {
			if u := alice.do(`{"op":"redo"}`).Undo; u == nil || u.Applied != 1 || tabOrder(e) != want {
				t.Fatalf("redo = %+v, tabs %s, want %s", u, tabOrder(e), want)
			}
		}
	})
	t.Run("tab order across the person's own new tab", func(t *testing.T) {
		e, alice, _ := fourTabs(t)
		alice.do(moveTabOp(e, "D", "A")) // D,A,B,C
		alice.do(`{"op":"add_tab","name":"E"}`)
		alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("B")))
		for _, want := range []string{"D,A,B,C,E", "D,A,B,C", "A,B,C,D"} {
			if u := undo(t, alice); u.Applied != 1 || tabOrder(e) != want {
				t.Fatalf("undo = %+v, tabs %s, want %s", u, tabOrder(e), want)
			}
		}
	})
}

// A move undo leaves the order alone once another writer placed a tab, and
// a deleted tab comes back between the tabs that were beside it.
func TestUndoTabOrderWithOtherWriters(t *testing.T) {
	t.Run("another writer's new tab keeps its place", func(t *testing.T) {
		e, alice, bob := fourTabs(t)
		alice.do(moveTabOp(e, "D", "A")) // D,A,B,C
		bob.do(jsonf(`{"op":"add_tab","name":"Bob","before":%d}`, e.tabID("D")))
		if u := undo(t, alice); u.Applied != 0 || u.Skipped != 1 || tabOrder(e) != "Bob,D,A,B,C" {
			t.Fatalf("undo = %+v, tabs %s", u, tabOrder(e))
		}
	})
	t.Run("another writer's deletion", func(t *testing.T) {
		e, alice, bob := fourTabs(t)
		alice.do(moveTabOp(e, "D", "A")) // D,A,B,C
		bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("B")))
		if u := undo(t, alice); u.Applied != 0 || u.Skipped != 1 || tabOrder(e) != "D,A,C" {
			t.Fatalf("undo = %+v, tabs %s", u, tabOrder(e))
		}
	})
	t.Run("a deleted tab returns between its neighbors", func(t *testing.T) {
		e, alice, bob := fourTabs(t)
		alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("C")))
		bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("A")))
		if u := undo(t, alice); u.Applied != 1 || tabOrder(e) != "B,C,D" {
			t.Fatalf("undo = %+v, tabs %s", u, tabOrder(e))
		}
	})
	t.Run("a deleted tab returns before the one that followed it", func(t *testing.T) {
		e, alice, bob := fourTabs(t)
		alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("B")))
		bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("A")))
		bob.do(`{"op":"add_tab","name":"Z"}`)
		if u := undo(t, alice); u.Applied != 1 || tabOrder(e) != "B,C,D,Z" {
			t.Fatalf("undo = %+v, tabs %s", u, tabOrder(e))
		}
	})
	t.Run("a deleted tab returns after a moved neighbor", func(t *testing.T) {
		e, alice, bob := fourTabs(t)
		alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("C")))
		bob.do(moveTabOp(e, "B", "")) // A,D,B
		if u := undo(t, alice); u.Applied != 1 || tabOrder(e) != "A,D,B,C" {
			t.Fatalf("undo = %+v, tabs %s", u, tabOrder(e))
		}
		if !slices.Equal(e.tabNames(), []string{"A", "D", "B", "C"}) {
			t.Fatalf("tabs = %v", e.tabNames())
		}
		e.restart()
		if tabOrder(e) != "A,D,B,C" {
			t.Fatalf("tabs after a restart = %s", tabOrder(e))
		}
	})
}
