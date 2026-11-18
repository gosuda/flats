package sheet

import (
	"encoding/json"
	"testing"
)

const offsite = `{"message":"Offsite budget","ops":[
 {"op":"rename_tab","tab":"Sheet1","name":"Budget"},
 {"op":"write","tab":"Budget","at":"A1","rows":[
   ["Item","Amount","Share"],
   ["Venue",1200,"=B2/$B$9"],
   ["Food",900,"=B3/$B$9"]]},
 {"op":"write","tab":"Budget","at":"A9","rows":[["Total","=SUM(B2:B8)"]]},
 {"op":"format","tab":"Budget","ranges":["B2:B9"],"format":{"number":{"kind":"currency","symbol":"$","decimals":2}}},
 {"op":"format","tab":"Budget","ranges":["C2:C3"],"format":{"number":{"kind":"percent","decimals":2}}},
 {"op":"format","tab":"Budget","ranges":["A1:C1"],"format":{"bold":true}}]}`

func TestTypicalFlow(t *testing.T) {
	e := start(t)
	res := e.mustAgent(offsite)
	if res.CellsWritten != 11 || res.FormulasWritten != 3 || res.FormulaErrors != 0 {
		t.Fatalf("result = %+v", res)
	}
	e.want("Budget!B9", "$2,100.00")
	e.want("Budget!C2", "57.14%")
	e.wantText("Budget!C3", "=B3/$B$9")

	alice, bob := e.person("Alice"), e.person("Bob")
	if ack := alice.set("Budget!B2", "1500"); status(ack) != "applied" {
		t.Fatalf("set: %s", status(ack))
	}
	e.want("Budget!B9", "$2,400.00")
	e.want("Budget!C2", "62.50%")

	// Bob inserts a row above row 3 while Alice's edit of B3 is under way.
	base := e.seq()
	edit := e.setOp("Budget!B3", "1000")
	tab, row, _ := e.ids("Budget!A3")
	ack := bob.do(jsonf(`{"op":"insert_rows","tab":%d,"beside":%d,"side":"before","count":1}`, tab, row))
	if status(ack) != "applied" {
		t.Fatalf("insert: %s", status(ack))
	}
	if ack := alice.act(base, edit); status(ack) != "applied" {
		t.Fatalf("stale edit: %s", status(ack))
	}
	e.want("Budget!A4", "Food")
	e.want("Budget!B4", "$1,000.00")
	e.wantText("Budget!C4", "=B4/$B$10")
	e.wantText("Budget!B10", "=SUM(B2:B9)")
	e.want("Budget!B10", "$2,500.00")

	// Undo of Alice's last edit gives the earlier amount back.
	if ack := alice.do(`{"op":"undo"}`); ack.Undo == nil || ack.Undo.Applied != 1 {
		t.Fatalf("undo: %+v", ack)
	}
	e.want("Budget!B4", "$900.00")

	out := e.read(ReadQuery{Tab: "Budget", Include: []string{"shown", "formats"}})
	raw, _ := json.Marshal(out)
	if out["range"] != "A1:C10" {
		t.Fatalf("read = %s", raw)
	}

	e.restart()
	e.want("Budget!B10", "$2,400.00")
	e.wantText("Budget!C4", "=B4/$B$10")
	if n := e.b.importAll(t); n < 2 {
		t.Fatalf("revisions = %d", n)
	}
}
