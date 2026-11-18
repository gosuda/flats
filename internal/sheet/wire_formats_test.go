package sheet

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// symbols lists the currency symbols of a head's style list.
func symbols(head map[string]any) []string {
	var out []string
	for _, style := range head["styles"].([]any) {
		if number, ok := style.(map[string]any)["number"].(map[string]any); ok && number["symbol"] != nil {
			out = append(out, number["symbol"].(string))
		}
	}
	return out
}

// styleOf returns the format a snapshot gives a cell: the entry of the
// head's style list that the cell names.
func styleOf(e *env, head map[string]any, cells map[string][]any, addr string) string {
	cell := cells[cellKey(e, addr)]
	if len(cell) < 6 {
		return "{}"
	}
	data, _ := json.Marshal(head["styles"].([]any)[int(cell[5].(float64))])
	return string(data)
}

// TestPublicSnapshotHasNoRemovedFormats covers the format list across
// publication. A format that was used and removed while the sheet was
// Private is in no Public snapshot, also after a withdrawal and a second
// publication, and every connection keeps reading the right format for
// every cell while the list is rebuilt under it.
func TestPublicSnapshotHasNoRemovedFormats(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	e := start(t)
	currency := func(addr, symbol string) string {
		return fmt.Sprintf(`{"op":"format","tab":"Sheet1","ranges":["%s"],"format":{"number":{"kind":"currency","symbol":"%s","decimals":2}}}`, addr, symbol)
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[5,6,7,8]]},` + currency("A1", "VIP") + `,` + currency("B1", "EUR") + `]}`)
	s := serve(e)
	tab := e.tabID("Sheet1")
	// An editor has the tab open through all of it. Its frames go to the
	// real client at the end.
	editor := s.record("private", `{"type":"hello","version":1,"name":"Ed","pending":0}`)
	editor.watch(tab)
	// The format leaves the sheet while it is Private.
	e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1"],"formats":true},{"op":"write","tab":"Sheet1","at":"A1","rows":[[5]]}]}`)
	private := s.dial("private", helloV1)
	private.next("welcome")
	if head, _ := private.watch(1, tab); !strings.Contains(fmt.Sprint(symbols(head)), "VIP") {
		t.Fatalf("a Private snapshot before the publication lists %v; the test expects the removed format still in the list", symbols(head))
	}

	publish := func(epoch string, gone ...string) {
		t.Helper()
		s.epoch.Store(epoch)
		viewer := s.dial("public", helloV1)
		viewer.next("welcome")
		head, cells := viewer.watch(1, tab)
		for _, symbol := range gone {
			if strings.Contains(fmt.Sprint(symbols(head)), symbol) {
				t.Fatalf("epoch %s: a Public snapshot lists the removed format %s: %v", epoch, symbol, symbols(head))
			}
		}
		if got := styleOf(e, head, cells, "B1"); !strings.Contains(got, `"EUR"`) {
			t.Fatalf("epoch %s: the Public snapshot gives B1 the format %s", epoch, got)
		}
		if got := styleOf(e, head, cells, "A1"); got != "{}" {
			t.Fatalf("epoch %s: the Public snapshot gives A1 the format %s", epoch, got)
		}
		// A format added while the sheet is Public reaches the viewer as a
		// delta on top of the list it holds.
		symbol := "P" + string(rune('A'+epoch[0]-'0'))
		e.mustAgent(`{"ops":[` + currency("C1", symbol) + `]}`)
		d := viewer.next("delta")
		added, _ := json.Marshal(d["styles"])
		row := d["cells"].([]any)[0].([]any)[1].([]any)[0].([]any)
		if index := int(row[5].(float64)); index != len(head["styles"].([]any)) || !strings.Contains(string(added), `"`+symbol+`"`) {
			t.Fatalf("epoch %s: the delta names style %d of a list of %d and adds %s", epoch, index, len(head["styles"].([]any)), added)
		}
		viewer.ws.Close(1000, "")
	}
	publish("2", "VIP")

	// Withdrawn, the sheet gets and loses another format, and the format
	// that was added while it was Public is removed as well.
	e.in.WithdrawPublic()
	e.mustAgent(`{"ops":[` + currency("D1", "TMP") + `]}`)
	e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["C1:D1"],"formats":true},{"op":"write","tab":"Sheet1","at":"C1","rows":[[7,8]]}]}`)
	publish("3", "VIP", "TMP", "PC")

	// The editor was resynced when the list changed under it, and holds
	// every cell with the format the engine has.
	e.mustAgent(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"bold":true}}]}`)
	steps, kinds := editor.taken()
	if kinds["resync"] < 2 {
		t.Fatalf("the editor got %d resyncs over two rebuilt lists: %v", kinds["resync"], kinds)
	}
	if diffs := compareClient(replay(t, node, steps), e.clientView("Sheet1")); len(diffs) > 0 {
		t.Fatalf("the editor's client differs from the engine:\n%s", strings.Join(diffs, "\n"))
	}
	// A connection that resumes from before the rebuilt list gets a new
	// snapshot, not a replay on top of the old list.
	e.in.hub.mu.Lock()
	rebuiltAt, epoch := e.in.hub.wireAt, e.in.hub.wireEpoch
	e.in.hub.mu.Unlock()
	if epoch < 2 {
		t.Fatalf("the format list is at epoch %d after two rebuilds", epoch)
	}
	first := s.dial("private", `{"type":"hello","version":1,"name":"Re","pending":0}`)
	token := first.next("welcome")["session"].(string)
	first.ws.Close(1000, "")
	for mark, want := range map[uint64]string{rebuiltAt: "tab_head", e.seq(): "none"} {
		resumed := s.dial("private", fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"watermark":"%d","tab":%d,"pending":0}`, token, mark, tab))
		welcome := resumed.next("welcome")
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"F1","rows":[[1]]}]}`)
		got := "none"
		for _, f := range append(resumed.seen, resumed.next("delta")) {
			if f["type"] == "tab_head" {
				got = "tab_head"
			}
		}
		if got != want {
			t.Fatalf("a resume from %d (welcome at %v) got %s, want %s", mark, welcome["seq"], got, want)
		}
		resumed.ws.Close(1000, "")
	}
}

// TestWireListIsBounded replaces one cell's currency format with distinct
// symbols, far more often than the sheet holds formats, while one editor
// has the tab open and no other connection arrives. The format list stays
// within twice the styles in use plus the slack after every call, on the
// host and in the browser: the host rebuilds it when it has grown past
// that, the editor is moved to the new list by a resync and a snapshot,
// and it reads every cell's format right before and after each move.
func TestWireListIsBounded(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	const formats, perCall = 5000, 250
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[5,6]]},{"op":"format","tab":"Sheet1","ranges":["B1"],"format":{"bold":true}}]}`)
	s := serve(e)
	tab := e.tabID("Sheet1")
	editor := s.record("private", `{"type":"hello","version":1,"name":"Ed","pending":0}`)
	editor.watch(tab)
	symbol := func(i int) string {
		return string([]rune{rune('A' + i%26), rune('A' + i/26%26), rune('A' + i/676%26)})
	}
	last := map[uint64]string{} // sequence of a call -> the symbol it left on A1
	most := 0
	for from := 0; from < formats; from += perCall {
		ops := make([]string, 0, perCall)
		for i := from; i < from+perCall; i++ {
			ops = append(ops, fmt.Sprintf(`{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"number":{"kind":"currency","symbol":"%s","decimals":2}}}`, symbol(i)))
		}
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
		last[e.seq()] = symbol(from + perCall - 1)
		e.in.mu.RLock()
		held, styles := len(e.in.wb.wire.list), len(e.in.wb.styles.byID)
		e.in.mu.RUnlock()
		most = max(most, held)
		if held > 2*styles+wireSlack {
			t.Fatalf("after %d formats the list holds %d, with %d styles in use: more than twice those plus %d", from+perCall, held, styles, wireSlack)
		}
	}
	if most < wireSlack/2 {
		t.Fatalf("the list never held more than %d formats; the test must come near the bound", most)
	}
	e.in.hub.mu.Lock()
	rebuilds := int(e.in.hub.wireEpoch)
	e.in.hub.mu.Unlock()
	// A rebuild leaves the formats in use, so the next one needs more than
	// the slack in new formats.
	if rebuilds < 3 || rebuilds > formats/wireSlack {
		t.Fatalf("%d formats rebuilt the list %d times, want 3 to %d", formats, rebuilds, formats/wireSlack)
	}

	// The last call rebuilt the list, and no delta follows it: the editor
	// is moved to the new list all the same, by a resync and a snapshot of
	// the next generation.
	editor.until("the snapshot after the last rebuild", func() bool { return editor.done == 1+rebuilds })
	// What the editor was sent. It holds a list of its own: the styles of
	// a head, and what each delta adds while it has the tab.
	steps, kinds := editor.taken()
	if kinds["resync"] != rebuilds {
		t.Fatalf("the editor got %d resyncs over %d rebuilt lists: %v", kinds["resync"], rebuilds, kinds)
	}
	var list []any
	holds, longest, checked := false, 0, 0
	for _, step := range steps {
		var f map[string]any
		if json.Unmarshal([]byte(step.Recv), &f) != nil {
			t.Fatalf("frame %.80s", step.Recv)
		}
		switch f["type"] {
		case "resync":
			holds = false
		case "tab_head":
			list, holds = f["styles"].([]any), true
		case "delta":
			if !holds {
				continue
			}
			added, _ := f["styles"].([]any)
			list = append(list, added...)
			seq, _ := strconv.ParseUint(f["seq"].(string), 10, 64)
			for _, row := range f["cells"].([]any) {
				for _, cell := range row.([]any)[1].([]any) {
					c := cell.([]any)
					index := int(c[5].(float64))
					if index >= len(list) {
						t.Fatalf("the delta %d names style %d of a list of %d", seq, index, len(list))
					}
					got, _ := json.Marshal(list[index])
					if want := last[seq]; want == "" || !strings.Contains(string(got), `"`+want+`"`) {
						t.Fatalf("the delta %d gives A1 the format %s, want the symbol %q", seq, got, want)
					}
					checked++
				}
			}
		}
		longest = max(longest, len(list))
	}
	if checked < formats/perCall-rebuilds {
		t.Fatalf("only %d deltas carried A1 on top of the list the editor held", checked)
	}
	if limit := 2*3 + wireSlack + perCall; longest > limit {
		t.Fatalf("the editor held a list of %d formats, more than %d", longest, limit)
	}

	// The real client, given the same frames, holds what the engine has
	// and a list within the same bound.
	got := replay(t, node, steps)
	if diffs := compareClient(got, e.clientView("Sheet1")); len(diffs) > 0 {
		t.Fatalf("the editor's client differs from the engine:\n%s", strings.Join(diffs, "\n"))
	}
	held, _ := got["tab"].(map[string]any)["styles"].(float64)
	peak, _ := got["styles_peak"].(float64)
	if limit := float64(2*3 + wireSlack + perCall); held == 0 || held > 8 || peak < held || peak > limit {
		t.Fatalf("the client holds %v formats and held up to %v; the bound is %v", held, peak, limit)
	}
}
