package sheet

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// edited lists the editor text each edit gesture of a run found, with nil
// for an editor that was not open.
func edited(run clientRun) []any {
	var out []any
	for _, text := range run.Edits {
		if text == nil {
			out = append(out, nil)
		} else {
			out = append(out, *text)
		}
	}
	return out
}

// sameFrame says whether two frames hold the same JSON.
func sameFrame(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}

// sentRequest finds the request of a kind and id the client sent.
func sentRequest(t *testing.T, run clientRun, kind string, id int) string {
	t.Helper()
	for _, m := range run.Requests {
		if m["type"] == kind && m["id"] == float64(id) {
			data, _ := json.Marshal(m)
			return string(data)
		}
	}
	t.Fatalf("the client sent no %s %d: %v", kind, id, run.Requests)
	return ""
}

// TestTypedReferencesWaitingForTheFormulaFollowAnInsert opens A1, which
// holds =B5, in the cell and in the formula bar, and types +B5 while its
// formula text is on its way. Another person then inserts a row above
// row 5. The recorded answers come from the real engine: the formula text
// of the screen the keys were typed on, and the exchange that adjusts the
// whole text through the insert. Both references name the cell that held
// 10, so the engine stores =B6+B6 and shows 20.
func TestTypedReferencesWaitingForTheFormulaFollowAnInsert(t *testing.T) {
	node := nodeOrSkip(t)
	for _, surface := range []string{"cell", "bar"} {
		t.Run(surface, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["=B5"]]},{"op":"write","tab":"Sheet1","at":"B5","rows":[[10]]}]}`)
			tab, row, col := e.ids("Sheet1!A1")
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(tab)
			steps, _ := r.taken()
			base := e.seq()
			peek := fmt.Sprintf(`{"type":"peek","id":1,"tab":%d,"row":%d,"col":%d}`, tab, row, col)
			r.ask(peek, "peeked", 1)
			got, _ := r.taken()
			peeked := got[len(steps):]
			e.person("Bob").do(e.insertRowsOp("Sheet1", 5, 1))
			got, _ = r.taken()
			inserted := got[len(steps)+len(peeked):]
			rebase := fmt.Sprintf(`{"type":"rebase","id":2,"tab":%d,"row":%d,"col":%d,"text":"=B5+B5","at":[0,0],"base":"%d","through":"%d"}`, tab, row, col, base, e.seq())
			r.ask(rebase, "rebased", 2)
			got, _ = r.taken()
			rebased := got[len(steps)+len(peeked)+len(inserted):]

			open := any(map[string]any{"key": "F2"})
			if surface == "bar" {
				open = map[string]any{"bar": true}
			}
			run := play(t, node, steps,
				map[string]any{"select": []int{0, 0, 0, 0}},
				open,
				map[string]any{"type": "+B5"},
				map[string]any{"steps": inserted},
				map[string]any{"edit": true},
				map[string]any{"steps": peeked},
				map[string]any{"edit": true},
				map[string]any{"steps": rebased},
				map[string]any{"edit": true},
				map[string]any{"key": "Enter"},
			)
			if want := []any{nil, "=B5+B5", "=B6+B6"}; !reflect.DeepEqual(edited(run), want) {
				t.Fatalf("the editor held %v, want %v: nothing until the text of the screen the keys were typed on, then the text adjusted with the grid", edited(run), want)
			}
			if !sameFrame(t, sentRequest(t, run, "peek", 1), peek) || !sameFrame(t, sentRequest(t, run, "rebase", 2), rebase) {
				t.Fatalf("the client's requests differ from the recorded ones: %v", run.Requests)
			}
			if len(run.Frames) != 1 {
				t.Fatalf("the client sent %d actions, want 1: %s", len(run.Frames), run.Frames)
			}
			if ack := r.answer(run.Frames[0]); len(ack.Results) != 1 || ack.Results[0].Status != "applied" {
				t.Fatalf("the edit was answered %+v", ack.Results)
			}
			e.wantText("A1", "=B6+B6")
			e.want("A1", "20")
		})
	}
}

// place is where a cell lies on screen now, by its ids.
func (e *env) place(tab TabID, row RowID, col ColID) []int {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	pos, ok := e.in.wb.byID[tab].posOf(row, col)
	if !ok {
		e.t.Fatalf("no cell %d/%d", row, col)
	}
	return []int{int(pos.Row), int(pos.Col)}
}

// TestReopenedPendingFormulaFollowsStructuralChanges commits a formula in
// the real client and reopens it before its action is answered, after
// another person changed the structure. The client adjusts the pending
// text from the base it was typed at through the real engine's exchange,
// and the keys typed meanwhile follow the adjusted text. The first action
// is confirmed while the second edit is open, as the host sends it once it
// has adjusted that action from its own base. The engine stores the
// adjusted text with +1 after it, so the value is one more than the first
// action's.
func TestReopenedPendingFormulaFollowsStructuralChanges(t *testing.T) {
	node := nodeOrSkip(t)
	cases := []struct {
		name   string
		setup  string
		addr   string
		typed  string
		change func(e *env) string
		want   string // the text after both actions
		value  string // what the cell shows then
	}{
		{"insert", `{"op":"write","tab":"Sheet1","at":"B5","rows":[[10]]}`, "A1", "=B5+1",
			func(e *env) string { return e.insertRowsOp("Sheet1", 5, 1) }, "=B6+1+1", "12"},
		{"delete", `{"op":"write","tab":"Sheet1","at":"B5","rows":[[10]]}`, "A1", "=B5+1",
			func(e *env) string { return e.deleteRowsOp("Sheet1", 2, 1) }, "=B4+1+1", "12"},
		// The row of A3 holds 30 and moves to row 4 with it.
		{"sort", `{"op":"write","tab":"Sheet1","at":"B2","rows":[[40],[30],[20],[10]]}`, "A3", "=B3+1",
			func(e *env) string { return e.sortOp("Sheet1", 2, 4, 2, false) }, "=B4+1+1", "32"},
		{"rename", `{"op":"add_tab","name":"Data"},{"op":"write","tab":"Data","at":"B5","rows":[[10]]}`, "A1", "=Data!B5+1",
			func(e *env) string { return jsonf(`{"op":"rename_tab","tab":%d,"name":"Info"}`, e.tabID("Data")) }, "=Info!B5+1+1", "12"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[` + c.setup + `]}`)
			tab, row, col := e.ids("Sheet1!" + c.addr)
			before := e.place(tab, row, col)
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(tab)
			steps, _ := r.taken()
			base := e.seq()
			typed, _ := json.Marshal(c.typed)
			first := fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":%s,"at":[%d,%d]}]}`, base, tab, row, col, typed, before[0], before[1])
			e.person("Bob").do(c.change(e))
			after := e.place(tab, row, col)
			got, _ := r.taken()
			changed := got[len(steps):]
			rebase := fmt.Sprintf(`{"type":"rebase","id":1,"tab":%d,"row":%d,"col":%d,"text":%s,"at":[%d,%d],"base":"%d","through":"%d"}`, tab, row, col, typed, before[0], before[1], base, e.seq())
			r.ask(rebase, "rebased", 1)
			got, _ = r.taken()
			rebased := got[len(steps)+len(changed):]
			var answer struct{ Text string }
			if err := json.Unmarshal([]byte(rebased[len(rebased)-1].Recv), &answer); err != nil || answer.Text == "" {
				t.Fatalf("the engine's rebase answer %s: %v", rebased[len(rebased)-1].Recv, err)
			}
			if ack := r.answer(json.RawMessage(first)); len(ack.Results) != 1 || ack.Results[0].Status != "applied" {
				t.Fatalf("the first edit was answered %+v", ack.Results)
			}
			got, _ = r.taken()
			confirmed := got[len(steps)+len(changed)+len(rebased):]
			addr := fmt.Sprintf("Sheet1!%s%d", formula.ColumnName(int32(after[1])), after[0]+1)
			if adjusted := e.text(addr); adjusted != answer.Text {
				t.Fatalf("the first action stored %q and the exchange gave %q", adjusted, answer.Text)
			}

			run := play(t, node, steps,
				map[string]any{"select": []int{before[0], before[1], before[0], before[1]}},
				map[string]any{"type": c.typed},
				map[string]any{"key": "Enter"},
				map[string]any{"steps": changed},
				map[string]any{"select": []int{after[0], after[1], after[0], after[1]}},
				map[string]any{"key": "F2"},
				map[string]any{"edit": true},
				map[string]any{"type": "+1"},
				map[string]any{"steps": rebased},
				map[string]any{"edit": true},
				map[string]any{"steps": confirmed},
				map[string]any{"edit": true},
				map[string]any{"key": "Enter"},
			)
			if want := []any{nil, answer.Text + "+1", answer.Text + "+1"}; !reflect.DeepEqual(edited(run), want) {
				t.Fatalf("the editor held %v, want %v", edited(run), want)
			}
			if !sameFrame(t, sentRequest(t, run, "rebase", 1), rebase) {
				t.Fatalf("the client's exchange differs from the recorded one: %v", run.Requests)
			}
			if len(run.Frames) != 2 || !sameFrame(t, string(run.Frames[0]), first) {
				t.Fatalf("the client sent %s, want the first edit as recorded and one more", run.Frames)
			}
			var second struct{ Base string }
			_ = json.Unmarshal(run.Frames[1], &second)
			if second.Base != strconv.FormatUint(e.seq(), 10) {
				t.Fatalf("the second edit has base %s, want %d, the screen after the first was confirmed", second.Base, e.seq())
			}
			if ack := r.answer(run.Frames[1]); len(ack.Results) != 1 || ack.Results[0].Status != "applied" {
				t.Fatalf("the second edit was answered %+v", ack.Results)
			}
			e.wantText(addr, c.want)
			e.want(addr, c.value)
		})
	}
}
