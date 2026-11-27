package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// formatted is a sheet whose look depends on stored formats: number
// formats that change the shown text, a mark, a fill and a column width.
const formatted = `{"message":"Formatted","ops":[
 {"op":"rename_tab","tab":"Sheet1","name":"Budget"},
 {"op":"write","tab":"Budget","at":"A1","rows":[["Item","Amount","Share","Due"],["Venue",1200,0.25,{"date":"2026-03-01"}],["Food",900.5,0.75,{"date":"2026-03-15"}],["Total","=SUM(B2:B3)"]]},
 {"op":"format","tab":"Budget","ranges":["A1:D1"],"format":{"bold":true,"fill":"yellow-100"}},
 {"op":"format","tab":"Budget","ranges":["B2:B4"],"format":{"number":{"kind":"currency","symbol":"$","decimals":2}}},
 {"op":"format","tab":"Budget","ranges":["C2:C3"],"format":{"number":{"kind":"percent","decimals":0}}},
 {"op":"format","tab":"Budget","ranges":["D2:D3"],"format":{"number":{"kind":"date","pattern":"mmm d, yyyy"}}},
 {"op":"widths","tab":"Budget","cols":"B:C","width":180}]}`

// formattedView is what a reader of that sheet gets: content, shown text,
// formats and widths.
const formattedView = `{"formats":[{"format":{"bold":true,"fill":"yellow-100"},"range":"A1:D1"},{"format":{"number":{"decimals":0,"kind":"percent"}},"range":"C2:C3"},{"format":{"number":{"kind":"date","pattern":"mmm d, yyyy"}},"range":"D2:D3"},{"format":{"number":{"decimals":2,"kind":"currency","symbol":"$"}},"range":"B2:B4"}],` +
	`"rows":[["Item","Amount","Share","Due"],["Venue",1200,0.25,{"date":"2026-03-01"}],["Food",900.5,0.75,{"date":"2026-03-15"}],["Total",{"formula":"=SUM(B2:B3)","value":2100.5},null,null]],` +
	`"shown":[["Item","Amount","Share","Due"],["Venue","$1,200.00","25%","Mar 1, 2026"],["Food","$900.50","75%","Mar 15, 2026"],["Total","$2,100.50","",""]],` +
	`"widths":[{"cols":"B:C","width":180}]}`

// readerView keeps the fields of a read that a reader sees.
func readerView(t testing.TB, out map[string]any) string {
	t.Helper()
	view := map[string]any{}
	for _, name := range []string{"rows", "shown", "formats", "widths"} {
		view[name] = out[name]
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var formattedQuery = ReadQuery{Tab: "Budget", Range: "A1:D4", Include: []string{"shown", "formats", "widths"}}

// liveView reads a range of the live sheet as a reader does.
func (e *env) liveView(tab, rng string) string {
	e.t.Helper()
	return readerView(e.t, e.read(ReadQuery{Tab: tab, Range: rng, Include: formattedQuery.Include}))
}

// wantSnapshot checks the tab as a browser gets it: every cell names its
// format in the style list of the snapshot, with the shown text that
// format gives.
func wantSnapshot(t *testing.T, e *env, head map[string]any, cells map[string][]any) {
	t.Helper()
	styles, _ := head["styles"].([]any)
	format := func(addr string) string {
		arr := cells[cellKey(e, "Budget!"+addr)]
		if len(arr) < 6 {
			return "{}"
		}
		i := int(arr[5].(float64))
		if i >= len(styles) {
			return fmt.Sprintf("style %d of %d", i, len(styles))
		}
		data, _ := json.Marshal(styles[i])
		return string(data)
	}
	shown := func(addr string) string {
		arr := cells[cellKey(e, "Budget!"+addr)]
		if len(arr) < 4 {
			return ""
		}
		return fmt.Sprint(arr[3])
	}
	for _, c := range []struct{ addr, shown, format string }{
		{"A1", "<nil>", `{"bold":true,"fill":"yellow-100"}`},
		{"B2", "$1,200.00", `{"number":{"decimals":2,"kind":"currency","symbol":"$"}}`},
		{"C3", "75%", `{"number":{"decimals":0,"kind":"percent"}}`},
		{"D2", "Mar 1, 2026", `{"number":{"kind":"date","pattern":"mmm d, yyyy"}}`},
		{"B4", "$2,100.50", `{"number":{"decimals":2,"kind":"currency","symbol":"$"}}`},
	} {
		if got := shown(c.addr); got != c.shown {
			t.Errorf("the snapshot shows %q in %s, want %q", got, c.addr, c.shown)
		}
		if got := format(c.addr); got != c.format {
			t.Errorf("the snapshot gives %s the format %s, want %s", c.addr, got, c.format)
		}
	}
	widths, _ := json.Marshal(head["widths"])
	_, _, colB := e.ids("Budget!B1")
	_, _, colC := e.ids("Budget!C1")
	if want := fmt.Sprintf(`{"%d":180,"%d":180}`, colB, colC); string(widths) != want {
		t.Errorf("the snapshot has the widths %s, want %s", widths, want)
	}
}

// TestFormatsSurviveStorage reads a formatted sheet the way readers do,
// after each path that builds a workbook from storage: a restart, a
// revision read, a rollback and a tab recovery. A cell's format reaches a
// reader through the shared format list of the wire, which every such
// workbook has to fill.
func TestFormatsSurviveStorage(t *testing.T) {
	seed := func(t *testing.T) (*env, int) {
		e := start(t)
		e.mustAgent(formatted)
		if got := e.liveView("Budget", "A1:D4"); got != formattedView {
			t.Fatalf("the live sheet reads\n%s\nwant\n%s", got, formattedView)
		}
		return e, e.b.importAll(t)
	}
	csv := func(t *testing.T, s *site, e *env) string {
		t.Helper()
		resp, err := http.Get(fmt.Sprintf("%s/public/_sheet/csv?tab=%d", s.server.URL, e.tabID("Budget")))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return strings.TrimPrefix(string(data), csvBOM)
	}
	const wantCSV = "Item,Amount,Share,Due\r\nVenue,\"$1,200.00\",25%,\"Mar 1, 2026\"\r\nFood,$900.50,75%,\"Mar 15, 2026\"\r\nTotal,\"$2,100.50\",,\r\n"

	t.Run("restart", func(t *testing.T) {
		e, _ := seed(t)
		e.restart()
		if got := e.liveView("Budget", "A1:D4"); got != formattedView {
			t.Fatalf("after the restart the sheet reads\n%s\nwant\n%s", got, formattedView)
		}
		s := serve(e)
		if got := csv(t, s, e); got != wantCSV {
			t.Errorf("after the restart the CSV is %q, want %q", got, wantCSV)
		}
		alice := s.dial("private", `{"type":"hello","version":1,"name":"Alice","pending":0}`)
		alice.next("welcome")
		head, cells := alice.watch(1, e.tabID("Budget"))
		wantSnapshot(t, e, head, cells)
		// An edit after the restart sends the cell with its stored format.
		e.person("Bob").set("Budget!B2", "1500")
		delta := alice.next("delta")
		cell := delta["cells"].([]any)[0].([]any)[1].([]any)[0].([]any)
		styles, _ := head["styles"].([]any)
		if len(cell) < 6 || cell[3] != "$1,500.00" || int(cell[5].(float64)) >= len(styles) {
			t.Fatalf("the delta of the edited cell = %v", cell)
		}
		if data, _ := json.Marshal(styles[int(cell[5].(float64))]); string(data) != `{"number":{"decimals":2,"kind":"currency","symbol":"$"}}` {
			t.Errorf("the edited cell names the format %s", data)
		}
	})

	t.Run("revision", func(t *testing.T) {
		e, rev := seed(t)
		if got := readerView(t, e.readRevision(rev, formattedQuery)); got != formattedView {
			t.Fatalf("revision %d reads\n%s\nwant\n%s", rev, got, formattedView)
		}
		// The History panel reads the same revision as snapshot frames.
		s := serve(e)
		resp, err := http.Get(fmt.Sprintf("%s/private/_sheet/api/revisions/%d/tabs/%d", s.server.URL, rev, e.tabID("Budget")))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		frames, err := ndjsonFrames(data)
		if err != nil || len(frames) < 3 {
			t.Fatalf("revision frames: %d %v %s", resp.StatusCode, err, data)
		}
		cells := map[string][]any{}
		for _, f := range frames[1 : len(frames)-1] {
			for _, row := range f.(map[string]any)["rows"].([]any) {
				pair := row.([]any)
				for _, cell := range pair[1].([]any) {
					arr := cell.([]any)
					cells[fmt.Sprint(pair[0], "/", arr[0])] = arr
				}
			}
		}
		wantSnapshot(t, e, frames[0].(map[string]any), cells)
	})

	t.Run("rollback", func(t *testing.T) {
		e, rev := seed(t)
		e.mustAgent(`{"ops":[{"op":"clear","tab":"Budget","ranges":["A1:D4"],"formats":true},{"op":"widths","tab":"Budget","cols":"B:C","width":60},
			{"op":"write","tab":"Budget","at":"A1","rows":[["x",1]]}]}`)
		if got := e.liveView("Budget", "A1:D4"); got == formattedView {
			t.Fatal("the sheet did not change before the rollback")
		}
		if _, err := e.b.Rollback(context.Background(), rev, "Alice"); err != nil {
			t.Fatal(err)
		}
		if got := e.liveView("Budget", "A1:D4"); got != formattedView {
			t.Fatalf("after the rollback the sheet reads\n%s\nwant\n%s", got, formattedView)
		}
		s := serve(e)
		alice := s.dial("private", `{"type":"hello","version":1,"name":"Alice","pending":0}`)
		alice.next("welcome")
		head, cells := alice.watch(1, e.tabID("Budget"))
		wantSnapshot(t, e, head, cells)
		e.restart()
		if got := e.liveView("Budget", "A1:D4"); got != formattedView {
			t.Fatalf("after the rollback and a restart the sheet reads\n%s\nwant\n%s", got, formattedView)
		}
	})

	t.Run("tab recovery", func(t *testing.T) {
		e, rev := seed(t)
		// The recovering instance has never held these formats: the tab is
		// gone and the host has restarted since.
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Other"},{"op":"delete_tab","tab":"Budget"}]}`)
		e.restart()
		if _, err := e.person("Alice").recover(rev, 1); err != nil {
			t.Fatal(err)
		}
		if got := e.liveView("Budget", "A1:D4"); got != formattedView {
			t.Fatalf("the recovered tab reads\n%s\nwant\n%s", got, formattedView)
		}
		s := serve(e)
		alice := s.dial("private", `{"type":"hello","version":1,"name":"Alice","pending":0}`)
		alice.next("welcome")
		head, cells := alice.watch(1, e.tabID("Budget"))
		wantSnapshot(t, e, head, cells)
		e.restart()
		if got := e.liveView("Budget", "A1:D4"); got != formattedView {
			t.Fatalf("after a restart the recovered tab reads\n%s\nwant\n%s", got, formattedView)
		}
	})
}
