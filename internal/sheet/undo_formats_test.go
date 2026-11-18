package sheet

import (
	"strconv"
	"strings"
	"testing"
)

func (e *env) formatOp(addr, set string) string {
	e.t.Helper()
	tab, row, col := e.ids(addr)
	return jsonf(`{"op":"format","tab":%d,"rows":[%d],"cols":[%d],"set":%s}`, tab, row, col, set)
}

func wantFormats(t *testing.T, e *env, tab string, want map[string]string) {
	t.Helper()
	for addr, format := range want {
		if got := e.formatOf(tab, addr); got != format {
			t.Errorf("%s!%s has the format %s, want %s", tab, addr, got, format)
		}
	}
}

// Formats come back with the rows, columns and tabs a deletion undo
// restores, and the sheet opens again right after.
func TestUndoDeleteKeepsFormats(t *testing.T) {
	const bold, money = `{"bold":true}`, `{"fill":"yellow-100","number":{"kind":"currency","symbol":"$","decimals":2}}`
	setup := func(t *testing.T) (*env, *person) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},
			{"op":"write","tab":"Data","at":"A1","rows":[[1,2,3],[4,5,6],[7,8,9]]},
			{"op":"format","tab":"Data","ranges":["A2"],"format":` + bold + `},
			{"op":"format","tab":"Data","ranges":["B2:B3"],"format":` + money + `},
			{"op":"widths","tab":"Data","cols":"B:B","width":180}]}`)
		return e, e.person("Alice")
	}
	width := func(e *env) int {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		tab := e.in.wb.tab("Data")
		return tab.widthOf(ColID(tab.cols.ids[1]))
	}
	formats := map[string]string{"A2": bold, "B2": money, "B3": money, "C2": "{}"}
	check := func(t *testing.T, e *env) {
		t.Helper()
		wantFormats(t, e, "Data", formats)
		e.want("Data!B2", "$5.00")
		e.restart()
		wantFormats(t, e, "Data", formats)
		e.want("Data!B2", "$5.00")
		if got := width(e); got != 180 {
			t.Errorf("column B is %d wide after the restart, want 180", got)
		}
	}
	t.Run("rows", func(t *testing.T) {
		e, alice := setup(t)
		alice.do(e.deleteRowsOp("Data", 2, 2))
		if u := undo(t, alice); u.Applied != 1 {
			t.Fatalf("undo = %+v", u)
		}
		check(t, e)
	})
	t.Run("rows through redo and a second undo", func(t *testing.T) {
		e, alice := setup(t)
		alice.do(e.deleteRowsOp("Data", 2, 1))
		undo(t, alice)
		alice.do(`{"op":"redo"}`)
		undo(t, alice)
		check(t, e)
	})
	t.Run("columns", func(t *testing.T) {
		e, alice := setup(t)
		alice.do(e.deleteColsOp("Data", 1, 2))
		if u := undo(t, alice); u.Applied != 1 {
			t.Fatalf("undo = %+v", u)
		}
		check(t, e)
	})
	t.Run("a tab", func(t *testing.T) {
		e, alice := setup(t)
		alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Data")))
		if u := undo(t, alice); u.Applied != 1 {
			t.Fatalf("undo = %+v", u)
		}
		check(t, e)
	})
	t.Run("a tab through redo and a second undo", func(t *testing.T) {
		e, alice := setup(t)
		alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Data")))
		undo(t, alice)
		alice.do(`{"op":"redo"}`)
		undo(t, alice)
		check(t, e)
	})
	t.Run("a restart between the deletion and nothing else", func(t *testing.T) {
		e, alice := setup(t)
		alice.do(e.deleteRowsOp("Data", 2, 2))
		e.restart()
		wantFormats(t, e, "Data", map[string]string{"A2": "{}", "B2": "{}"})
	})
	t.Run("the person's earlier format undo still applies", func(t *testing.T) {
		e, alice := setup(t)
		alice.do(e.formatOp("Data!C2", `{"italic":true}`))
		alice.do(e.deleteRowsOp("Data", 2, 1))
		undo(t, alice)
		wantFormats(t, e, "Data", map[string]string{"C2": `{"italic":true}`})
		if u := undo(t, alice); u.Applied != 1 || u.Skipped != 0 {
			t.Fatalf("undo of the format = %+v", u)
		}
		check(t, e)
	})
	t.Run("a format another writer gave the same look", func(t *testing.T) {
		e, alice := setup(t)
		bob := e.person("Bob")
		alice.do(e.deleteRowsOp("Data", 2, 2))
		// The style of the deleted cells is gone from the table; Bob's new
		// bold cell does not bring back the stamps of the deleted one.
		bob.do(e.formatOp("Data!A1", bold))
		undo(t, alice)
		formats := map[string]string{"A1": bold, "A2": bold, "B2": money, "B3": money}
		wantFormats(t, e, "Data", formats)
		e.restart()
		wantFormats(t, e, "Data", formats)
	})
}

// A browser that applies only deltas ends with the formats of restored
// rows, in the same format list as a fresh snapshot, although the style
// table forgot those formats between the deletion and its undo.
func TestUndoDeleteFormatsOverTheWire(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,2],[3,4],[5,6],[7,8]]},
		{"op":"format","tab":"Sheet1","ranges":["A2"],"format":{"bold":true}},
		{"op":"format","tab":"Sheet1","ranges":["B2:B3"],"format":{"fill":"yellow-100","number":{"kind":"currency","symbol":"$","decimals":2}}}]}`)
	s := serve(e)
	tab := e.tabID("Sheet1")
	watcher := s.dial("private", `{"type":"hello","version":1,"name":"W","pending":0}`)
	watcher.next("welcome")
	sc := newScreen(watcher.watch(1, tab))
	seq := e.seq()
	bold, money := cellKey(e, "A2"), cellKey(e, "B3")

	alice, bob := e.person("Alice"), e.person("Bob")
	alice.do(e.deleteRowsOp("Sheet1", 2, 2))
	bob.do(e.formatOp("A1", `{"italic":true}`))
	if u := undo(t, alice); u.Applied != 1 {
		t.Fatalf("undo = %+v", u)
	}
	alice.do(`{"op":"redo"}`)
	bob.do(e.formatOp("B1", `{"underline":true}`))
	undo(t, alice)

	for final := e.seq(); seq < final; {
		d := watcher.next("delta")
		next, _ := strconv.ParseUint(d["seq"].(string), 10, 64)
		if next != seq+1 {
			t.Fatalf("delta %d after %d", next, seq)
		}
		seq = next
		sc.apply(d)
	}
	fresh := s.dial("private", `{"type":"hello","version":1,"name":"F","pending":0}`)
	fresh.next("welcome")
	if diff := sc.diff(newScreen(fresh.watch(1, tab))); diff != "" {
		t.Fatalf("the screen that applied the deltas: %s", diff)
	}
	if got := sc.brief(bold); !strings.Contains(got, `"bold":true`) {
		t.Fatalf("A2 arrived as %s", got)
	}
	if got := sc.brief(money); !strings.Contains(got, `"fill":"yellow-100"`) {
		t.Fatalf("B3 arrived as %s", got)
	}
}
