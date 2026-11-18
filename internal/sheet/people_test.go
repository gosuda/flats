package sheet

import (
	"fmt"
	"slices"
	"testing"
)

// TestPeopleOperations runs every operation of section 7.1 through the
// action path, with the value groups of rules 28 to 30.
func TestPeopleOperations(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=A1*2","x"],[2,"=A2*2","y"],[3,"=A3*2","z"]]}]}`)
	alice, bob := e.person("Alice"), e.person("Bob")
	tab := e.tabID("Sheet1")
	rect := func(row, rows, col, cols int) string {
		return fmt.Sprintf(`"rows":%s,"cols":%s`, e.rowsOf("Sheet1", row, rows), e.colsOf("Sheet1", col, cols))
	}
	apply := func(p *person, op string) ackBody {
		t.Helper()
		ack := p.do(op)
		if status(ack) != "applied" {
			t.Fatalf("%s: %s", op, status(ack))
		}
		return ack
	}
	format := func(addr string) string {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		tb, row, col := e.at(addr)
		c := tb.cell(row, col)
		if c == nil {
			return "{}"
		}
		out, _ := appendFormat(nil, e.in.wb.styles.format(c.style))
		return string(out)
	}

	// Rule 29: different properties of one cell all apply, from two base
	// states that did not see each other.
	base := e.seq()
	alice.act(base, jsonf(`{"op":"format","tab":%d,%s,"set":{"bold":true}}`, tab, rect(1, 2, 1, 2)))
	bob.act(base, jsonf(`{"op":"format","tab":%d,%s,"set":{"fill":"red-600","number":{"kind":"number","decimals":1,"thousands":false}}}`, tab, rect(1, 1, 1, 1)))
	bob.act(base, e.setOp("A1", "5"))
	if got := format("A1"); got != `{"bold":true,"fill":"red-600","number":{"kind":"number","decimals":1,"thousands":false}}` {
		t.Fatalf("A1 format = %s", got)
	}
	e.want("A1", "5.0")
	e.want("B1", "10")
	// Rule 30: in the same group the later write stays. The notice to the
	// earlier writer's session is checked over a connection, in
	// TestOverwriteNotice.
	base = e.seq()
	alice.act(base, e.setOp("C1", "alice"))
	ack := bob.act(base, e.setOp("C1", "bob"))
	e.want("C1", "bob")
	if status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	// Undo of the format restores only the property Alice wrote, and leaves
	// what Bob changed since.
	bob.do(jsonf(`{"op":"format","tab":%d,%s,"set":{"bold":false}}`, tab, rect(2, 1, 1, 1)))
	if u := undo(t, alice); u.Applied != 0 || u.Skipped != 1 {
		t.Fatalf("undo of an overwritten cell = %+v", u)
	}
	if u := undo(t, alice); u.Applied != 3 || u.Skipped != 1 || u.Reason != "changed" {
		t.Fatalf("undo of the format = %+v", u)
	}
	if got := format("A1"); got != `{"fill":"red-600","number":{"kind":"number","decimals":1,"thousands":false}}` {
		t.Fatalf("A1 format after undo = %s", got)
	}
	// A date format names its pattern, and null resets a property.
	apply(alice, jsonf(`{"op":"format","tab":%d,%s,"set":{"number":{"kind":"date","pattern":"m/d/yyyy"},"fill":null,"align":"center"}}`, tab, rect(1, 1, 1, 1)))
	if got := format("A1"); got != `{"align":"center","number":{"kind":"date","pattern":"m/d/yyyy"}}` {
		t.Fatalf("A1 format = %s", got)
	}
	apply(alice, jsonf(`{"op":"format","tab":%d,%s,"set":{"number":{"kind":"general"},"align":null}}`, tab, rect(1, 1, 1, 1)))
	if got := format("A1"); got != `{}` {
		t.Fatalf("A1 format = %s", got)
	}

	// clear keeps formats; width and settings; append and delete.
	apply(alice, jsonf(`{"op":"format","tab":%d,%s,"set":{"italic":true}}`, tab, rect(3, 1, 3, 1)))
	if ack := apply(alice, jsonf(`{"op":"clear","tab":%d,%s}`, tab, rect(1, 3, 3, 1))); ack.Results[0].Applied != 3 {
		t.Fatalf("clear: %+v", ack.Results)
	}
	e.want("C3", "")
	if got := format("C3"); got != `{"italic":true}` {
		t.Fatalf("C3 format after clear = %s", got)
	}
	apply(alice, jsonf(`{"op":"width","tab":%d,"cols":%s,"width":250}`, tab, e.colsOf("Sheet1", 2, 2)))
	if ack := alice.do(jsonf(`{"op":"width","tab":%d,"cols":%s,"width":20}`, tab, e.colsOf("Sheet1", 2, 2))); status(ack) != "refused:invalid" {
		t.Fatal(status(ack))
	}
	apply(alice, jsonf(`{"op":"settings","tab":%d,"frozen_rows":2,"frozen_cols":1}`, tab))
	apply(alice, jsonf(`{"op":"append_rows","tab":%d,"count":5}`, tab))
	apply(alice, jsonf(`{"op":"append_cols","tab":%d,"count":2}`, tab))
	apply(alice, e.deleteColsOp("Sheet1", 27, 1))
	e.in.mu.RLock()
	live := e.in.wb.byID[tab]
	rows, cols, width, frozen := live.rows.len(), live.cols.len(), live.widthOf(ColID(live.cols.ids[2])), [2]int{live.frozenRows, live.frozenCols}
	e.in.mu.RUnlock()
	if rows != 1005 || cols != 27 || width != 250 || frozen != [2]int{2, 1} {
		t.Fatalf("rows %d, cols %d, width %d, frozen %v", rows, cols, width, frozen)
	}

	// copy carries formulas (shifted) and formats; paste values does not.
	apply(alice, jsonf(`{"op":"format","tab":%d,%s,"set":{"underline":true}}`, tab, rect(1, 1, 2, 1)))
	apply(alice, jsonf(`{"op":"copy","from":{"tab":%d,%s},"to":{"tab":%d,%s},"mode":"all","cut":false}`, tab, rect(1, 3, 2, 1), tab, rect(6, 3, 5, 1)))
	e.wantText("E6", "=D6*2")
	e.wantText("E8", "=D8*2")
	if got := format("E6"); got != `{"underline":true}` {
		t.Fatalf("copied format = %s", got)
	}
	apply(alice, jsonf(`{"op":"copy","from":{"tab":%d,%s},"to":{"tab":%d,%s},"mode":"values","cut":false}`, tab, rect(1, 3, 2, 1), tab, rect(6, 3, 7, 1)))
	e.wantText("G6", "10")
	if got := format("G6"); got != `{}` {
		t.Fatalf("paste values carried a format: %s", got)
	}
	// fill repeats the source over the target, and a paste can add rows.
	apply(alice, jsonf(`{"op":"fill","tab":%d,"source":{%s},"target":{%s}}`, tab, rect(1, 2, 1, 2), rect(10, 4, 1, 2)))
	e.wantText("B12", "=A12*2")
	e.wantText("A13", "2")
	e.in.mu.RLock()
	lastRow := live.rows.ids[live.rows.len()-1]
	firstCol := live.cols.ids[0]
	e.in.mu.RUnlock()
	apply(alice, jsonf(`{"op":"write","tab":%d,"rows":[%d,{"new":1},{"new":2}],"cols":[%d],"values":[["end"],["=1+1"],["3%%"]]}`, tab, lastRow, firstCol))
	e.want("A1005", "end")
	e.want("A1006", "2")
	e.want("A1007", "3%")
	if u := undo(t, alice); u.Applied == 0 {
		t.Fatalf("undo of the paste = %+v", u)
	}
	e.in.mu.RLock()
	rows = live.rows.len()
	e.in.mu.RUnlock()
	if rows != 1005 || e.shown("A1005") != "" {
		t.Fatalf("undo of a paste that added rows left %d rows and %q", rows, e.shown("A1005"))
	}

	// Tabs: add before, move, rename, delete, and the last tab stays.
	apply(alice, jsonf(`{"op":"add_tab","name":"First","before":%d}`, tab))
	apply(alice, `{"op":"add_tab"}`)
	apply(alice, jsonf(`{"op":"move_tab","tab":%d,"before":%d}`, e.tabID("Sheet2"), e.tabID("First")))
	apply(alice, jsonf(`{"op":"rename_tab","tab":%d,"name":"Second"}`, e.tabID("Sheet2")))
	if got := e.tabNames(); !slices.Equal(got, []string{"Second", "First", "Sheet1"}) {
		t.Fatalf("tabs = %v", got)
	}
	for _, bad := range []string{"first", "a/b", " lead", ""} {
		if ack := alice.do(jsonf(`{"op":"rename_tab","tab":%d,"name":%q}`, e.tabID("Second"), bad)); status(ack) != "refused:invalid" {
			t.Fatalf("rename to %q: %s", bad, status(ack))
		}
	}
	apply(alice, jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Second")))
	apply(alice, jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("First")))
	if ack := alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, tab)); status(ack) != "refused:invalid" {
		t.Fatalf("deleting the last tab: %s", status(ack))
	}
	// One action can apply, ignore and refuse its operations separately.
	ack = alice.do(e.setOp("D1", "ok"), jsonf(`{"op":"set","tab":%d,"row":999999,"col":%d,"text":"gone"}`, tab, firstCol), jsonf(`{"op":"width","tab":%d,"cols":[%d],"width":5}`, tab, firstCol))
	if status(ack) != "applied,ignored:gone,refused:invalid" {
		t.Fatalf("mixed action: %s", status(ack))
	}
	e.want("D1", "ok")
	// A resend under the same number with other content is a protocol error.
	if _, err := alice.send(alice.n, e.seq(), e.setOp("D1", "other")); refusal(err).Reason != "action_number" {
		t.Fatalf("another action under a used number: %v", err)
	}
}

// TestPasteValuesInsideOneAction pastes values from formulas whose inputs
// the same action wrote just before. The paste holds what the formulas give
// with those inputs, also for a formula the action itself wrote.
func TestPasteValuesInsideOneAction(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"=A1*10"]]}]}`)
	e.want("B1", "10")
	alice := e.person("Alice")
	tab := e.tabID("Sheet1")
	cell := func(addr string) string {
		_, row, col := e.ids(addr)
		return fmt.Sprintf(`"rows":[[%d,1]],"cols":[[%d,1]]`, row, col)
	}
	paste := func(from, to string) string {
		return jsonf(`{"op":"copy","from":{"tab":%d,%s},"to":{"tab":%d,%s},"mode":"values","cut":false}`, tab, cell(from), tab, cell(to))
	}
	if ack := alice.do(e.setOp("A1", "5"), paste("B1", "C1"), e.setOp("A2", "=A1+2"), paste("A2", "C2"), e.setOp("A1", "6"), paste("B1", "C3")); status(ack) != "applied,applied,applied,applied,applied,applied" {
		t.Fatal(status(ack))
	}
	wantTexts(t, e, map[string]string{"C1": "50", "C2": "7", "C3": "60", "A1": "6"})
	e.want("B1", "60")
	e.want("A2", "8")
}
