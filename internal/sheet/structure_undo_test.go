package sheet

import (
	"context"
	"slices"
	"testing"
)

func undo(t *testing.T, p *person) *undoAck {
	t.Helper()
	ack := p.do(`{"op":"undo"}`)
	if ack.Undo == nil {
		t.Fatalf("undo: %s", status(ack))
	}
	return ack.Undo
}

func TestStructureUndoInsert(t *testing.T) {
	t.Run("S-18", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.insertRowsOp("Sheet1", 2, 1))
		bob.set("C2", "bob was here")
		if u := undo(t, alice); u.Applied != 0 || u.Skipped != 1 || u.Reason != "written" {
			t.Fatalf("undo = %+v", u)
		}
		e.want("C2", "bob was here")
		e.want("A3", "2")
	})
	t.Run("S-19", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice := e.person("Alice")
		alice.do(e.insertRowsOp("Sheet1", 2, 1))
		alice.set("C2", "mine")
		undo(t, alice)
		if u := undo(t, alice); u.Applied != 1 || u.Skipped != 0 {
			t.Fatalf("undo = %+v", u)
		}
		e.want("A2", "2")
		// Redo brings the row back, and undo removes it again.
		if u := redo(t, alice); u.Applied != 1 {
			t.Fatalf("redo = %+v", u)
		}
		e.want("A3", "2")
		undo(t, alice)
		e.want("A2", "2")
	})
}

func TestStructureUndoSort(t *testing.T) {
	t.Run("S-20", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.sortOp("Sheet1", 1, 6, 1, true))
		bob.do(e.sortOp("Sheet1", 2, 3, 2, false))
		if u := undo(t, alice); u.Applied != 0 || u.Reason != "sorted" {
			t.Fatalf("undo = %+v", u)
		}
		if got := e.column("Sheet1", "A", 1, 6); !slices.Equal(got, []string{"6", "3", "4", "5", "2", "1"}) {
			t.Fatalf("rows = %v", got)
		}
	})
	t.Run("S-21", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.sortOp("Sheet1", 1, 6, 1, true)) // 6 5 4 3 2 1
		bob.do(e.insertRowsOp("Sheet1", 3, 1))      // 6 5 _ 4 3 2 1
		bob.do(e.deleteRowsOp("Sheet1", 1, 1))      // 5 _ 4 3 2 1
		if u := undo(t, alice); u.Applied != 1 {
			t.Fatalf("undo = %+v", u)
		}
		// The remaining moved rows are back in their earlier order, in the
		// places they hold now; the inserted row stays where it is.
		if got := e.column("Sheet1", "A", 1, 6); !slices.Equal(got, []string{"1", "", "2", "3", "4", "5"}) {
			t.Fatalf("rows = %v", got)
		}
	})
}

func TestStructureUndoDelete(t *testing.T) {
	// S-22: undo a row deletion with formulas that read it.
	setup := func(t *testing.T) (*env, *person, *person) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"add_tab","name":"Summary"},
			{"op":"write","tab":"Data","at":"A1","rows":[[1],[2],[3],[4],[5]]},
			{"op":"write","tab":"Summary","at":"A1","rows":[["=Data!A3+Data!A4","=SUM(Data!A1:A3)"]]},
			{"op":"write","tab":"Data","at":"C1","rows":[["=A3*2"]]}]}`)
		return e, e.person("Alice"), e.person("Bob")
	}
	t.Run("S-22a", func(t *testing.T) {
		e, alice, _ := setup(t)
		alice.do(e.deleteRowsOp("Data", 3, 1))
		e.wantText("Summary!A1", "=#REF!+Data!A3")
		e.wantText("Summary!B1", "=SUM(Data!A1:A2)")
		if u := undo(t, alice); u.Applied != 1 {
			t.Fatalf("undo = %+v", u)
		}
		e.wantText("Summary!A1", "=Data!A3+Data!A4")
		e.wantText("Summary!B1", "=SUM(Data!A1:A3)")
		e.wantText("Data!C1", "=A3*2")
		e.want("Summary!A1", "7")
		e.want("Data!A3", "3")
	})
	t.Run("S-22b", func(t *testing.T) {
		e, alice, bob := setup(t)
		alice.do(e.deleteRowsOp("Data", 3, 1))
		bob.do(jsonf(`{"op":"append_rows","tab":%d,"count":5}`, e.tabID("Data")))
		undo(t, alice)
		e.wantText("Summary!A1", "=Data!A3+Data!A4")
	})
	t.Run("S-22c", func(t *testing.T) {
		e, alice, bob := setup(t)
		alice.do(e.deleteRowsOp("Data", 3, 1))
		bob.set("Summary!A1", "=Data!A1")
		undo(t, alice)
		e.wantText("Summary!A1", "=Data!A1")
		e.wantText("Summary!B1", "=SUM(Data!A1:A3)")
	})
	t.Run("S-22d", func(t *testing.T) {
		e, alice, bob := setup(t)
		alice.do(e.deleteRowsOp("Data", 3, 1))
		e.wantText("Data!C1", "=#REF!*2")
		bob.do(e.insertRowsOp("Data", 1, 1))
		undo(t, alice)
		e.wantText("Data!C2", "=#REF!*2")
	})
	t.Run("S-22e", func(t *testing.T) {
		e, alice, bob := setup(t)
		alice.do(e.deleteRowsOp("Data", 3, 1))
		bob.do(e.insertRowsOp("Data", 1, 1))
		undo(t, alice)
		e.wantText("Summary!A1", "=#REF!+Data!A5")
		e.want("Data!A4", "3")
	})
	t.Run("S-22f", func(t *testing.T) {
		e, alice, bob := setup(t)
		alice.do(e.deleteRowsOp("Data", 2, 1))
		e.wantText("Summary!B1", "=SUM(Data!A1:A2)")
		bob.do(e.insertRowsOp("Data", 2, 1))
		e.wantText("Summary!B1", "=SUM(Data!A1:A3)")
		undo(t, alice)
		// The range that had lost a row is left as it is: only the insert
		// of the restored row grows it.
		e.wantText("Summary!B1", "=SUM(Data!A1:A4)")
	})
	t.Run("S-22g", func(t *testing.T) {
		e, alice, bob := setup(t)
		alice.do(e.deleteRowsOp("Data", 3, 1))
		bob.do(e.sortOp("Data", 1, 4, 1, true))
		undo(t, alice)
		e.wantText("Summary!A1", "=#REF!+Data!A3")
	})
	t.Run("S-23", func(t *testing.T) {
		e, alice, bob := setup(t)
		alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Data")))
		e.want("Summary!A1", "#REF!")
		bob.do(`{"op":"add_tab","name":"Data"}`)
		if u := undo(t, alice); u.Applied != 1 {
			t.Fatalf("undo = %+v", u)
		}
		if got := e.tabNames(); !slices.Contains(got, "Data (restored)") {
			t.Fatalf("tabs = %v", got)
		}
		e.want("'Data (restored)'!A5", "5")
		e.wantText("'Data (restored)'!C1", "=A3*2")
	})
}

func TestStructureStaleAgent(t *testing.T) {
	setup := func(t *testing.T) (*env, string, *person) {
		e := start(t)
		e.mustAgent(numbers)
		return e, e.state(), e.person("Bob")
	}
	write := func(state string) string {
		return `{"state":"` + state + `","ops":[{"op":"write","tab":"Sheet1","at":"C1","rows":[["agent"]]}]}`
	}
	t.Run("S-24", func(t *testing.T) {
		e, state, bob := setup(t)
		bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"Renamed"}`, e.tabID("Sheet1")))
		_, err := e.agent(write(state))
		e.wantRefusal(err, "conflict", "tab_changed")
	})
	t.Run("S-25", func(t *testing.T) {
		e, state, bob := setup(t)
		bob.do(`{"op":"add_tab","name":"Other"}`)
		bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Sheet1")))
		_, err := e.agent(write(state))
		e.wantRefusal(err, "conflict", "tab_changed")
	})
	t.Run("S-26", func(t *testing.T) {
		e, state, bob := setup(t)
		bob.do(`{"op":"add_tab","name":"Other"}`)
		bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Sheet1")))
		bob.do(`{"op":"add_tab","name":"Sheet1"}`)
		_, err := e.agent(write(state))
		e.wantRefusal(err, "conflict", "tab_changed")
	})
	t.Run("S-27", func(t *testing.T) {
		e, state, bob := setup(t)
		bob.do(e.insertRowsOp("Sheet1", 1, 1))
		_, err := e.agent(write(state))
		e.wantRefusal(err, "conflict", "structure_changed")
		e.want("C1", "")
	})
	t.Run("S-28", func(t *testing.T) {
		e, state, bob := setup(t)
		bob.set("D1", "bob")
		bob.do(jsonf(`{"op":"append_rows","tab":%d,"count":3}`, e.tabID("Sheet1")))
		if _, err := e.agent(write(state)); err != nil {
			t.Fatal(err)
		}
		e.want("C1", "agent")
		e.want("D1", "bob")
	})
	t.Run("S-29", func(t *testing.T) {
		e, _, _ := setup(t)
		if _, err := e.agent(`{"state":"","ops":[{"op":"add_tab","name":"Notes"}]}`); err != nil {
			t.Fatal(err)
		}
		_, err := e.agent(`{"state":"","ops":[{"op":"add_tab","name":"More"},{"op":"write","tab":"More","at":"A1","rows":[[1]]}]}`)
		e.wantRefusal(err, "conflict", "state_required")
	})
}

func TestStructureRecovery(t *testing.T) {
	// A sheet with Budget (A1 = 2) and Costs, recorded as a revision.
	setup := func(t *testing.T) (*env, *person, int, TabID) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"Budget"},{"op":"add_tab","name":"Costs"},{"op":"add_tab","name":"Other"},
			{"op":"write","tab":"Budget","at":"A1","rows":[[2,"=Budget!A1+A1","=Costs!A1"]]},
			{"op":"write","tab":"Costs","at":"A1","rows":[[10]]},
			{"op":"write","tab":"Other","at":"A1","rows":[["=Budget!A1","=Budget!A5"]]},
			{"op":"widths","tab":"Budget","cols":"B:B","width":200},
			{"op":"format","tab":"Budget","ranges":["A1"],"format":{"bold":true}}]}`)
		return e, e.person("Alice"), e.b.importAll(t), e.tabID("Budget")
	}
	t.Run("S-30", func(t *testing.T) {
		e, alice, rev, budget := setup(t)
		e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Budget"}]}`)
		id, err := alice.recover(rev, budget)
		if err != nil {
			t.Fatal(err)
		}
		if id != budget || !slices.Equal(e.tabNames(), []string{"Costs", "Other", "Budget"}) {
			t.Fatalf("recovered tab %d, tabs %v", id, e.tabNames())
		}
		e.want("Budget!B1", "4")
	})
	t.Run("S-31", func(t *testing.T) {
		e, alice, rev, budget := setup(t)
		e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Budget"},{"op":"add_tab","name":"Budget"}]}`)
		if _, err := alice.recover(rev, budget); err != nil {
			t.Fatal(err)
		}
		if _, err := alice.recover(rev, budget); err != nil {
			t.Fatal(err)
		}
		if got := e.tabNames(); !slices.Equal(got[len(got)-2:], []string{"Budget (restored)", "Budget (restored 2)"}) {
			t.Fatalf("tabs = %v", got)
		}
	})
	t.Run("S-32", func(t *testing.T) {
		e, alice, rev, budget := setup(t)
		id, err := alice.recover(rev, budget)
		if err != nil {
			t.Fatal(err)
		}
		if id == budget || e.tabNames()[3] != "Budget (restored)" {
			t.Fatalf("a tab that still exists comes back as a copy: id %d, tabs %v", id, e.tabNames())
		}
		e.in.mu.RLock()
		live, copied := e.in.wb.byID[budget], e.in.wb.byID[id]
		shared := live.rows.ids[0] == copied.rows.ids[0] || live.cols.ids[0] == copied.cols.ids[0]
		width := copied.widthOf(ColID(copied.cols.ids[1]))
		e.in.mu.RUnlock()
		if shared || width != 200 {
			t.Fatalf("copy shares ids (%v) or lost its width (%d)", shared, width)
		}
	})
	t.Run("S-33", func(t *testing.T) {
		e, alice, rev, budget := setup(t)
		e.mustAgent(`{"ops":[{"op":"delete_rows","tab":"Budget","at":5,"count":1}]}`)
		e.wantText("Other!B1", "=#REF!")
		e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Budget"}]}`)
		e.want("Other!A1", "#REF!")
		if _, err := alice.recover(rev, budget); err != nil {
			t.Fatal(err)
		}
		e.want("Other!A1", "2")
		e.wantText("Other!A1", "=Budget!A1")
		e.wantText("Other!B1", "=#REF!")
	})
	t.Run("S-34", func(t *testing.T) {
		e, alice, rev, budget := setup(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Budget","at":"A1","rows":[[1]]}]}`)
		if _, err := alice.recover(rev, budget); err != nil {
			t.Fatal(err)
		}
		e.wantText("'Budget (restored)'!B1", "='Budget (restored)'!A1+A1")
		e.want("'Budget (restored)'!B1", "4")
		e.want("'Budget (restored)'!C1", "10")
		e.wantText("Other!A1", "=Budget!A1")
		e.want("Other!A1", "1")
		// S-35: the rewritten reference follows a later rename.
		alice.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"Plan"}`, e.tabID("Budget (restored)")))
		e.wantText("Plan!B1", "=Plan!A1+A1")
		e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Costs"}]}`)
		e.want("Plan!C1", "#REF!")
	})
	t.Run("S-36", func(t *testing.T) {
		e, alice, rev, budget := setup(t)
		bob := e.person("Bob")
		if _, err := alice.recover(rev, budget); err != nil {
			t.Fatal(err)
		}
		if u := undo(t, alice); u.Applied != 1 || len(e.tabNames()) != 3 {
			t.Fatalf("undo = %+v, tabs %v", u, e.tabNames())
		}
		if _, err := alice.recover(rev, budget); err != nil {
			t.Fatal(err)
		}
		bob.set("'Budget (restored)'!D4", "bob")
		if u := undo(t, alice); u.Applied != 0 || u.Reason != "written" || len(e.tabNames()) != 4 {
			t.Fatalf("undo = %+v, tabs %v", u, e.tabNames())
		}
	})
}

func TestStructureRollback(t *testing.T) {
	setup := func(t *testing.T) (*env, int) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[3,"=A1*2"],[1,"=A2*2"],[2,"=A3*2"]]}]}`)
		return e, e.b.importAll(t)
	}
	rollback := func(t *testing.T, e *env, rev int) {
		t.Helper()
		if _, err := e.b.Rollback(context.Background(), rev, "Alice"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("S-37", func(t *testing.T) {
		e, rev := setup(t)
		alice := e.person("Alice")
		alice.set("B1", "=A1+100")
		alice.do(e.insertRowsOp("Sheet1", 1, 1))
		alice.set("B4", "text")
		rollback(t, e, rev)
		for addr, text := range map[string]string{"B1": "=A1*2", "B2": "=A2*2", "B3": "=A3*2", "B4": ""} {
			e.wantText(addr, text)
		}
		e.want("B1", "6")
		// The rollback revision names the files of the revision it restored.
		n := e.b.importAll(t)
		if !slices.Equal(e.b.entry(n).Files, e.b.entry(rev).Files) || e.b.entry(n).Kind != "rollback" {
			t.Fatalf("rollback revision %d does not share the files of revision %d", n, rev)
		}
		if u := undo(t, alice); u.Applied != 0 || u.Skipped != 0 {
			t.Fatalf("undo history survived the rollback: %+v", u)
		}
	})
	// A rollback that only reorders rows: sort, then roll back to before it.
	t.Run("S-38", func(t *testing.T) {
		e, rev := setup(t)
		e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:3","by":"A"}]}`)
		state := e.state()
		rollback(t, e, rev)
		_, err := e.agent(`{"state":"` + state + `","ops":[{"op":"write","tab":"Sheet1","at":"C1","rows":[["agent"]]}]}`)
		e.wantRefusal(err, "conflict", "rolled_back")
	})
	t.Run("S-39", func(t *testing.T) {
		e, rev := setup(t)
		alice := e.person("Alice")
		e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:3","by":"A"}]}`)
		base, edit := e.seq(), e.setOp("C1", "=A1+1")
		rollback(t, e, rev)
		if ack := alice.act(base, edit); status(ack) != "refused:stale_formula" {
			t.Fatal(status(ack))
		}
	})
	t.Run("S-40", func(t *testing.T) {
		e, rev := setup(t)
		alice := e.person("Alice")
		e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:3","by":"A"}]}`)
		// The row that holds 1 is row 1 now and row 2 after the rollback.
		base, edit := e.seq(), e.setOp("C1", "note")
		rollback(t, e, rev)
		if ack := alice.act(base, edit); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		e.want("A2", "1")
		e.want("C2", "note")
	})
	t.Run("S-41", func(t *testing.T) {
		e, rev := setup(t)
		alice := e.person("Alice")
		// A row deleted after the revision comes back with the rollback; an
		// edit prepared for it before the deletion is ignored.
		base, edit := e.seq(), e.setOp("C2", "note")
		e.mustAgent(`{"ops":[{"op":"delete_rows","tab":"Sheet1","at":2,"count":1}]}`)
		rollback(t, e, rev)
		if ack := alice.act(base, edit); status(ack) != "ignored:gone" {
			t.Fatal(status(ack))
		}
		e.want("A2", "1")
		e.want("C2", "")
	})
}
