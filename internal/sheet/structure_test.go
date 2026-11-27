package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The structure cases of the technical design, section 22.2, through the Go
// API. A subtest that covers one case of that list is named after its id.

const numbers = `{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,10],[2,20],[3,30],[4,40],[5,50],[6,60]]}]}`

func TestStructureFormulaInFlight(t *testing.T) {
	t.Run("S-01", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		base, edit := e.seq(), e.setOp("D5", "=A3+$A$4+B5")
		bob.do(e.insertRowsOp("Sheet1", 2, 1))
		if ack := alice.act(base, edit); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		e.wantText("D6", "=A4+$A$5+B6")
		e.want("D6", "57")
	})
	t.Run("S-02", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		base, edit := e.seq(), e.setOp("D5", "=A3+A4")
		bob.do(e.deleteRowsOp("Sheet1", 3, 1))
		if ack := alice.act(base, edit); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		e.wantText("D4", "=#REF!+A3")
		e.want("D4", "#REF!")
	})
	t.Run("S-03", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		base, edit := e.seq(), e.setOp("D2", "=A2*2+$B$1")
		bob.do(e.sortOp("Sheet1", 1, 6, 1, true))
		if ack := alice.act(base, edit); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		// Row 2 held 2 and is now row 5 of the descending order.
		e.want("A5", "2")
		e.wantText("D5", "=A5*2+$B$1")
	})
	t.Run("S-04", func(t *testing.T) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Data","at":"A1","rows":[[7]]}]}`)
		alice, bob := e.person("Alice"), e.person("Bob")
		base, edit := e.seq(), e.setOp("Sheet1!A1", "=Data!A1+1")
		bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"Q1 costs"}`, e.tabID("Data")))
		if ack := alice.act(base, edit); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		e.wantText("Sheet1!A1", "='Q1 costs'!A1+1")
		e.want("Sheet1!A1", "8")
	})
	t.Run("S-05", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		base, edit := e.seq(), e.setOp("D5", "=A3+A4")
		bob.do(e.insertRowsOp("Sheet1", 2, 1))
		e.in.mu.Lock()
		e.in.structs.clear(e.in.wb.seq)
		e.in.mu.Unlock()
		if ack := alice.act(base, edit); status(ack) != "refused:stale_formula" {
			t.Fatal(status(ack))
		}
		e.wantText("D6", "")
		// A plain value prepared against the same screen still lands in its cell.
		plain := strings.Replace(edit, `"=A3+A4"`, `"7"`, 1)
		if ack := alice.act(base, plain); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		e.want("D6", "7")
	})
}

// TestStructurePasteInFlight is spec rule 14 for the formulas a person
// commits without an address: a paste names its cells by ids only, and so
// does a set without `at`. Each formula is read against the screen of the
// action's base, as if it had been in its cell since then.
func TestStructurePasteInFlight(t *testing.T) {
	// paste builds a write of values into the cells from the 1-based row
	// and column, as the screen shows them now.
	paste := func(e *env, row, col int, values string) string {
		var lines [][]string
		if err := json.Unmarshal([]byte(values), &lines); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf(`{"op":"write","tab":%d,"rows":%s,"cols":%s,"values":%s}`, e.tabID("Sheet1"),
			e.rowsOf("Sheet1", row, len(lines)), e.colsOf("Sheet1", col, len(lines[0])), values)
	}
	t.Run("a row inserted above", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		base, write := e.seq(), paste(e, 4, 4, `[["=A4*2","x"],["=A5*2","=SUM(A$1:A5)"]]`)
		bob.do(e.insertRowsOp("Sheet1", 2, 1))
		if ack := alice.act(base, write); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		// The formulas read the cells Alice saw, which lie a row lower now.
		e.wantText("D5", "=A5*2")
		e.want("D5", "8")
		e.want("E5", "x")
		e.wantText("D6", "=A6*2")
		e.want("D6", "10")
		e.wantText("E6", "=SUM(A$1:A6)")
		e.want("E6", "15")
	})
	t.Run("a row deleted", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		base, write := e.seq(), paste(e, 5, 4, `[["=A3+A4"]]`)
		bob.do(e.deleteRowsOp("Sheet1", 3, 1))
		if ack := alice.act(base, write); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		e.wantText("D4", "=#REF!+A3")
	})
	t.Run("a column inserted and one deleted", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		base, write := e.seq(), paste(e, 4, 5, `[["=A4+B4+C4"]]`)
		bob.do(e.insertColsOp("Sheet1", 1, 2))
		bob.do(e.deleteColsOp("Sheet1", 5, 1))
		if ack := alice.act(base, write); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		// A and B moved two columns right, C is gone, and E became F.
		e.wantText("F4", "=C4+D4+#REF!")
	})
	t.Run("a sort", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		base, write := e.seq(), paste(e, 2, 4, `[["=A2*2+$B$1"]]`)
		bob.do(e.sortOp("Sheet1", 1, 6, 1, true))
		if ack := alice.act(base, write); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		e.want("A5", "2")
		e.wantText("D5", "=A5*2+$B$1")
	})
	t.Run("rows the paste adds at the end", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		_, last, col := e.ids("A1000")
		base := e.seq()
		write := jsonf(`{"op":"write","tab":%d,"rows":[%d,{"new":1}],"cols":[%d],"values":[["5"],["=A1000*2"]]}`, e.tabID("Sheet1"), last, col)
		bob.do(e.insertRowsOp("Sheet1", 2, 1))
		if ack := alice.act(base, write); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		e.want("A1001", "5")
		e.wantText("A1002", "=A1001*2")
		e.want("A1002", "10")
	})
	t.Run("a set without its address", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		tab, row, col := e.ids("D5")
		base, edit := e.seq(), jsonf(`{"op":"set","tab":%d,"row":%d,"col":%d,"text":"=A3+$A$4+B5"}`, tab, row, col)
		bob.do(e.insertRowsOp("Sheet1", 2, 1))
		if ack := alice.act(base, edit); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		e.wantText("D6", "=A4+$A$5+B6")
		e.want("D6", "57")
	})
	t.Run("refused when the log no longer reaches back", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		base, write := e.seq(), paste(e, 4, 4, `[["7","=A4*2"]]`)
		plain := paste(e, 4, 4, `[["7","8"]]`)
		bob.do(e.insertRowsOp("Sheet1", 2, 1))
		e.in.mu.Lock()
		e.in.structs.clear(e.in.wb.seq)
		e.in.mu.Unlock()
		seq := e.seq()
		if ack := alice.act(base, write); status(ack) != "refused:stale_formula" {
			t.Fatal(status(ack))
		}
		// Nothing of the paste was written, the plain value included.
		e.want("D5", "")
		e.want("E5", "")
		if got := e.seq(); got != seq {
			t.Fatalf("the refused paste took sequence %d", got)
		}
		// Values without a formula still land in their cells.
		if ack := alice.act(base, plain); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		e.want("D5", "7")
		e.want("E5", "8")
		// Without a structural edit since the base the paste is accepted
		// as typed, also with the log gone.
		base, write = e.seq(), paste(e, 6, 4, `[["=A6*2"]]`)
		bob.set("F1", "1")
		e.in.mu.Lock()
		e.in.structs.clear(e.in.wb.seq)
		e.in.mu.Unlock()
		if ack := alice.act(base, write); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		e.wantText("D6", "=A6*2")
	})
	t.Run("refused after a rollback", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		rev := e.b.importAll(t)
		alice := e.person("Alice")
		base, write := e.seq(), paste(e, 4, 4, `[["=A4*2"]]`)
		if _, err := e.b.Rollback(context.Background(), rev, "Bob"); err != nil {
			t.Fatal(err)
		}
		if ack := alice.act(base, write); status(ack) != "refused:stale_formula" {
			t.Fatal(status(ack))
		}
		e.want("D4", "")
	})
}

// refs writes formulas, applies agent operations and compares the texts.
func refs(t *testing.T, formulas map[string]string, ops string, want map[string]string) {
	t.Helper()
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Costs"},{"op":"add_tab","name":"Q1 costs"}]}`)
	var writes []string
	for addr, text := range formulas {
		tab, cell, _ := strings.Cut(addr, "!")
		writes = append(writes, fmt.Sprintf(`{"op":"write","tab":%q,"at":%q,"rows":[[%q]]}`, tab, cell, text))
	}
	e.mustAgent(`{"ops":[` + strings.Join(writes, ",") + `]}`)
	e.mustAgent(`{"ops":[` + ops + `]}`)
	for addr, text := range want {
		e.wantText(addr, text)
	}
}

func TestStructureReferences(t *testing.T) {
	t.Run("S-06", func(t *testing.T) {
		// Insert rows before the first, between, and after the last cell of a range.
		refs(t, map[string]string{"Sheet1!H1": "=SUM(B3:B5)", "Sheet1!H2": "=B3"},
			`{"op":"insert_rows","tab":"Sheet1","at":3,"count":2}`,
			map[string]string{"Sheet1!H1": "=SUM(B5:B7)", "Sheet1!H2": "=B5"})
		refs(t, map[string]string{"Sheet1!H1": "=SUM(B3:B5)"},
			`{"op":"insert_rows","tab":"Sheet1","at":4,"count":2}`,
			map[string]string{"Sheet1!H1": "=SUM(B3:B7)"})
		refs(t, map[string]string{"Sheet1!H1": "=SUM(B3:B5)"},
			`{"op":"insert_rows","tab":"Sheet1","at":6,"count":2}`,
			map[string]string{"Sheet1!H1": "=SUM(B3:B5)"})
	})
	t.Run("S-07", func(t *testing.T) {
		refs(t, map[string]string{"Sheet1!A9": "=SUM(C3:E3)"},
			`{"op":"insert_cols","tab":"Sheet1","at":"C","count":1}`,
			map[string]string{"Sheet1!A9": "=SUM(D3:F3)"})
		refs(t, map[string]string{"Sheet1!A9": "=SUM(C3:E3)"},
			`{"op":"insert_cols","tab":"Sheet1","at":"D","count":1}`,
			map[string]string{"Sheet1!A9": "=SUM(C3:F3)"})
		refs(t, map[string]string{"Sheet1!A9": "=SUM(C3:E3)"},
			`{"op":"insert_cols","tab":"Sheet1","at":"F","count":1}`,
			map[string]string{"Sheet1!A9": "=SUM(C3:E3)"})
	})
	t.Run("S-08", func(t *testing.T) {
		refs(t, map[string]string{"Sheet1!H1": "=$B$3+B$4+$B5+SUM($B$3:B$5)"},
			`{"op":"insert_rows","tab":"Sheet1","at":2,"count":1},{"op":"insert_cols","tab":"Sheet1","at":"A","count":1}`,
			map[string]string{"Sheet1!I1": "=$C$4+C$5+$C6+SUM($C$4:C$6)"})
	})
	t.Run("S-09", func(t *testing.T) {
		refs(t, map[string]string{"Sheet1!A1": "=Costs!B3+'Q1 costs'!$B$3+SUM('Q1 costs'!B3:D9)", "Costs!A1": "=Sheet1!B3"},
			`{"op":"insert_rows","tab":"Costs","at":1,"count":1},{"op":"insert_rows","tab":"Q1 costs","at":4,"count":2}`,
			map[string]string{"Sheet1!A1": "=Costs!B4+'Q1 costs'!$B$3+SUM('Q1 costs'!B3:D11)", "Costs!A2": "=Sheet1!B3"})
	})
	t.Run("S-10", func(t *testing.T) {
		refs(t, map[string]string{"Sheet1!H1": "=SUM(B:D)+SUM(2:5)+SUM(Costs!B:B)"},
			`{"op":"insert_rows","tab":"Sheet1","at":3,"count":1},{"op":"insert_cols","tab":"Sheet1","at":"C","count":1},{"op":"insert_rows","tab":"Costs","at":1,"count":3}`,
			map[string]string{"Sheet1!I1": "=SUM(B:E)+SUM(2:6)+SUM(Costs!B:B)"})
	})
	t.Run("S-11", func(t *testing.T) {
		refs(t, map[string]string{"Sheet1!H1": "=SUM(B3:B6)"}, `{"op":"delete_rows","tab":"Sheet1","at":3,"count":1}`,
			map[string]string{"Sheet1!H1": "=SUM(B3:B5)"})
		refs(t, map[string]string{"Sheet1!H1": "=SUM(B3:B6)"}, `{"op":"delete_rows","tab":"Sheet1","at":6,"count":1}`,
			map[string]string{"Sheet1!H1": "=SUM(B3:B5)"})
		refs(t, map[string]string{"Sheet1!H1": "=SUM(B3:B6)"}, `{"op":"delete_rows","tab":"Sheet1","at":4,"count":2}`,
			map[string]string{"Sheet1!H1": "=SUM(B3:B4)"})
	})
	t.Run("S-12", func(t *testing.T) {
		refs(t, map[string]string{"Sheet1!H1": "=SUM(B3:B6)", "Sheet1!H2": "=B4*2"}, `{"op":"delete_rows","tab":"Sheet1","at":3,"count":4}`,
			map[string]string{"Sheet1!H1": "=SUM(#REF!)", "Sheet1!H2": "=#REF!*2"})
	})
	t.Run("S-13", func(t *testing.T) {
		refs(t, map[string]string{"Sheet1!H1": "=B3+C3", "Costs!A1": "=Sheet1!C3"}, `{"op":"delete_cols","tab":"Sheet1","at":"C","count":1}`,
			map[string]string{"Sheet1!G1": "=B3+#REF!", "Costs!A1": "=#REF!"})
	})
}

func TestStructureSort(t *testing.T) {
	setup := func(t *testing.T) *env {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[
			[3,"=A1*2","=$A$1+A1","=A2"],[1,"=A2*2","=$A$1+A2","=A3"],[2,"=A3*2","=$A$1+A3","=A4"],["outside","=A1","=SUM(A1:A3)"]]}]}`)
		return e
	}
	t.Run("S-14", func(t *testing.T) {
		e := setup(t)
		e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:3","by":"A"}]}`)
		// The row that held 3 is now row 3: its formulas shift with it.
		e.wantText("B3", "=A3*2")
		e.wantText("C3", "=$A$1+A3")
		e.wantText("B1", "=A1*2")
		if got := e.column("Sheet1", "A", 1, 3); !slices.Equal(got, []string{"1", "2", "3"}) {
			t.Fatalf("order = %v", got)
		}
		e.want("C3", "4")
	})
	t.Run("S-15", func(t *testing.T) {
		e := setup(t)
		e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:3","by":"A"}]}`)
		// Formulas outside the moved rows keep their addresses.
		e.wantText("B4", "=A1")
		e.want("B4", "1")
		e.wantText("C4", "=SUM(A1:A3)")
	})
	t.Run("S-16", func(t *testing.T) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[2,"x"],[1,"=A1"]]}]}`)
		e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:2","by":"A"}]}`)
		e.wantText("B1", "=#REF!")
		e.want("B1", "#REF!")
	})
	t.Run("S-17", func(t *testing.T) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[
			["Head","k"],[true,1],["b",2],[null,3],[2,4],["=1/0",5],["A",6],[false,7],[-1,8],["a",9],[{"date":"1900-01-01"},10]]},
			{"op":"tab_settings","tab":"Sheet1","frozen_rows":1,"frozen_cols":0}]}`)
		alice := e.person("Alice")
		tab, _, col := e.ids("A1")
		if ack := alice.do(jsonf(`{"op":"sort","tab":%d,"rows":"all","by":%d,"desc":false}`, tab, col)); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		// Numbers and dates, text without capitals (ties keep their order),
		// FALSE, TRUE, errors, blank last. The frozen row does not move.
		want := []string{"k", "8", "4", "10", "6", "9", "2", "7", "1", "5", "3"}
		if got := e.column("Sheet1", "B", 1, 11); !slices.Equal(got, want) {
			t.Fatalf("ascending = %v, want %v", got, want)
		}
		alice.do(jsonf(`{"op":"sort","tab":%d,"rows":"all","by":%d,"desc":true}`, tab, col))
		// The number 2 and the date of day 2 tie and keep their order.
		want = []string{"k", "5", "1", "7", "2", "6", "9", "4", "10", "8", "3"}
		if got := e.column("Sheet1", "B", 1, 11); !slices.Equal(got, want) {
			t.Fatalf("descending = %v, want %v", got, want)
		}
	})
}

func TestStructureEditsFollowIdentity(t *testing.T) {
	t.Run("S-42", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		base, edit := e.seq(), e.setOp("C4", "mine")
		bob.do(e.insertRowsOp("Sheet1", 1, 2))
		bob.do(e.deleteRowsOp("Sheet1", 3, 1))
		bob.do(e.sortOp("Sheet1", 1, 7, 1, true))
		if ack := alice.act(base, edit); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		// The row that held 4 is where the edit was made.
		found := false
		for row := 1; row <= 7; row++ {
			if e.shown(fmt.Sprintf("A%d", row)) == "4" {
				found = true
				e.want(fmt.Sprintf("C%d", row), "mine")
			}
		}
		if !found {
			t.Fatal("the row of 4 is gone")
		}
	})
	t.Run("S-43", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		base := e.seq()
		paste := jsonf(`{"op":"write","tab":%d,"rows":%s,"cols":%s,"values":[["a"],["b"],["c"]]}`, e.tabID("Sheet1"), e.rowsOf("Sheet1", 2, 3), e.colsOf("Sheet1", 3, 1))
		bob.do(e.deleteRowsOp("Sheet1", 3, 1))
		ack := alice.act(base, paste)
		if status(ack) != "applied" || ack.Results[0].Applied != 2 || ack.Results[0].Ignored != 1 {
			t.Fatalf("paste: %+v", ack.Results)
		}
		e.want("C2", "a")
		e.want("C3", "c")
	})
	t.Run("S-44", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		base, insert := e.seq(), e.insertRowsOp("Sheet1", 3, 1)
		bob.do(e.deleteRowsOp("Sheet1", 3, 1))
		if ack := alice.act(base, insert); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		// The new row is where row 3 was: between 2 and 4.
		if got := e.column("Sheet1", "A", 1, 5); !slices.Equal(got, []string{"1", "2", "", "4", "5"}) {
			t.Fatalf("rows = %v", got)
		}
	})
	t.Run("S-45", func(t *testing.T) {
		e := start(t)
		e.mustAgent(numbers)
		alice, bob := e.person("Alice"), e.person("Bob")
		base := e.seq()
		first, second := e.sortOp("Sheet1", 1, 4, 1, true), e.sortOp("Sheet1", 3, 4, 1, true)
		alice.act(base, first)
		bob.act(base, second)
		// Rows 1 to 4 are 4,3,2,1; the later sort then orders the rows that
		// were rows 3 to 6 (3,4,5,6) in the places they hold now.
		if got := e.column("Sheet1", "A", 1, 6); !slices.Equal(got, []string{"6", "5", "2", "1", "4", "3"}) {
			t.Fatalf("rows = %v", got)
		}
	})
}

func TestStructureCut(t *testing.T) {
	// S-47
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"Source"},{"op":"add_tab","name":"Destination"},{"op":"add_tab","name":"Other"},
		{"op":"write","tab":"Source","at":"A1","rows":[[5,"=A1"],[null,"=$A$1"],[null,"=A$1+Other!B2"],[null,"=Destination!A1"]]},
		{"op":"write","tab":"Other","at":"B2","rows":[[2]]},{"op":"write","tab":"Destination","at":"A1","rows":[[9]]}]}`)
	alice := e.person("Alice")
	cut := jsonf(`{"op":"copy","from":{"tab":%d,"rows":%s,"cols":%s},"to":{"tab":%d,"rows":%s,"cols":%s},"mode":"all","cut":true}`,
		e.tabID("Source"), e.rowsOf("Source", 1, 4), e.colsOf("Source", 2, 1),
		e.tabID("Destination"), e.rowsOf("Destination", 5, 4), e.colsOf("Destination", 3, 1))
	if ack := alice.do(cut); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	for addr, text := range map[string]string{
		"Destination!C5": "=Source!A1", "Destination!C6": "=Source!$A$1",
		"Destination!C7": "=Source!A$1+Other!B2", "Destination!C8": "=Destination!A1", "Source!B1": "",
	} {
		e.wantText(addr, text)
	}
	e.want("Destination!C5", "5")
	e.want("Destination!C7", "7")
	e.want("Destination!C8", "9")
}

func TestStructureFormulaLength(t *testing.T) {
	// S-48: =A9 padded with spaces to 8,192 characters in B9.
	long := "=A9" + strings.Repeat(" ", 8192-3)
	setup := func(t *testing.T) *env {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"T"},{"op":"write","tab":"Sheet1","at":"A8","rows":[[1],[3,` + fmt.Sprintf("%q", long) + `],[2]]},
			{"op":"write","tab":"Sheet1","at":"D1","rows":[[` + fmt.Sprintf("%q", "=T!A1"+strings.Repeat(" ", 8192-6)) + `]]}]}`)
		return e
	}
	for name, ops := range map[string]string{
		"insert": `{"op":"insert_rows","tab":"Sheet1","at":1,"count":1}`,
		"sort":   `{"op":"sort","tab":"Sheet1","rows":"8:10","by":"A"}`,
		"rename": `{"op":"rename_tab","tab":"T","name":"Longer"}`,
	} {
		t.Run("S-48/"+name, func(t *testing.T) {
			e := setup(t)
			seq := e.seq()
			_, err := e.agent(`{"ops":[` + ops + `]}`)
			e.wantRefusal(err, "sheet_capacity", "formula_length")
			if e.seq() != seq || e.text("B9") != long {
				t.Fatal("the refused write changed the sheet")
			}
		})
	}
	t.Run("S-48/fill", func(t *testing.T) {
		e := setup(t)
		alice := e.person("Alice")
		tab := e.tabID("Sheet1")
		fill := jsonf(`{"op":"fill","tab":%d,"source":{"rows":%s,"cols":%s},"target":{"rows":%s,"cols":%s}}`, tab,
			e.rowsOf("Sheet1", 9, 1), e.colsOf("Sheet1", 2, 1), e.rowsOf("Sheet1", 10, 1), e.colsOf("Sheet1", 2, 1))
		if ack := alice.do(fill); status(ack) != "refused:formula_length" {
			t.Fatal(status(ack))
		}
		e.wantText("B10", "")
	})
}
