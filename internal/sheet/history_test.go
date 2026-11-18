package sheet

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// TestHistoryCellText reads the content of one cell of a revision as it
// would be typed: the formula text of the revision, never the live cell's
// (rules 18 and 51).
func TestHistoryCellText(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[2,"=A1*3","plain"],[7]]}]}`)
	rev := e.b.importAll(t)
	// The live sheet moves on: a row above changes the formula's text, and
	// the plain cell is overwritten.
	p := e.person("Ann")
	p.do(e.insertRowsOp("Sheet1", 1, 1))
	p.set("C2", "newer")
	e.wantText("B2", "=A2*3")
	tab, formulaRow, formulaCol := e.ids("B2")
	_, _, plainCol := e.ids("C2")
	_, newRow, _ := e.ids("A1")
	_, sevenRow, _ := e.ids("A3")
	s := serve(e)
	call := func(route, path string) (int, map[string]any) {
		t.Helper()
		for {
			resp, err := http.Get(s.server.URL + "/" + route + path)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var out map[string]any
			_ = json.Unmarshal(data, &out)
			if resp.StatusCode == http.StatusServiceUnavailable && out["reason"] == "calculating" {
				continue
			}
			return resp.StatusCode, out
		}
	}
	cell := func(row RowID, col ColID) string {
		return fmt.Sprintf("/_sheet/api/revisions/%d/tabs/%d/cell?row=%d&col=%d", rev, tab, row, col)
	}
	if code, out := call("private", cell(formulaRow, formulaCol)); code != http.StatusOK || out["edit"] != "=A1*3" || out["formula"] != true {
		t.Fatalf("formula cell of the revision: %d %v", code, out)
	}
	if code, out := call("private", cell(formulaRow, plainCol)); code != http.StatusOK || out["edit"] != "plain" || out["formula"] != false {
		t.Fatalf("plain cell of the revision: %d %v", code, out)
	}
	// A row the revision does not have, and a blank cell of a row it has,
	// read as empty.
	if code, out := call("private", cell(newRow, formulaCol)); code != http.StatusOK || out["edit"] != "" || out["formula"] != false {
		t.Fatalf("cell outside the revision: %d %v", code, out)
	}
	if code, out := call("private", cell(sevenRow, formulaCol)); code != http.StatusOK || out["edit"] != "" || out["formula"] != false {
		t.Fatalf("blank cell of the revision: %d %v", code, out)
	}
	for _, path := range []string{
		fmt.Sprintf("/_sheet/api/revisions/%d/tabs/%d/cell", rev, tab),
		fmt.Sprintf("/_sheet/api/revisions/%d/tabs/%d/cell?row=x&col=1", rev, tab),
		fmt.Sprintf("/_sheet/api/revisions/%d/tabs/%d/cell?row=1&col=1&also=1", rev, tab),
		fmt.Sprintf("/_sheet/api/revisions/%d/tabs/%d/cell?row=1&row=2&col=1", rev, tab),
	} {
		if code, out := call("private", path); code != http.StatusBadRequest || out["code"] != "invalid" {
			t.Errorf("GET %s: %d %v", path, code, out)
		}
	}
	if code, out := call("private", fmt.Sprintf("/_sheet/api/revisions/%d/tabs/999/cell?row=1&col=1", rev)); code != http.StatusNotFound || out["code"] != "not_found" {
		t.Errorf("cell of a missing tab: %d %v", code, out)
	}
	if code, _ := call("private", fmt.Sprintf("/_sheet/api/revisions/%d/tabs/%d/cells", rev, tab)); code != http.StatusNotFound {
		t.Errorf("unknown History path: %d", code)
	}
	// A Public route reads no revision content.
	if code, out := call("public", cell(formulaRow, formulaCol)); code != http.StatusForbidden || out["edit"] != nil {
		t.Errorf("Public read of a revision cell: %d %v", code, out)
	}
}

// TestRecoverEveryTab views and restores each tab of a retained revision
// through the History routes: a tab that is unchanged since, one that was
// renamed since and one that was deleted since. Each view holds the tab's
// cells as the revision has them, and each recovery adds the tab with the
// revision's name, or with "(restored)" when that name is taken. A deleted
// tab comes back under its own id.
func TestRecoverEveryTab(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"One"},{"op":"add_tab","name":"Two"},{"op":"add_tab","name":"Three"},
		{"op":"write","tab":"One","at":"A1","rows":[["one",10,"=B1*2"]]},
		{"op":"write","tab":"Two","at":"A1","rows":[["two",20,"=B1+One!B1"]]},
		{"op":"write","tab":"Three","at":"A1","rows":[["three",30,"=SUM(B1:B1)"]]}]}`)
	rev := e.b.importAll(t)
	ids := map[string]TabID{"One": e.tabID("One"), "Two": e.tabID("Two"), "Three": e.tabID("Three")}
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Two","name":"Second"},{"op":"delete_tab","tab":"Three"},
		{"op":"write","tab":"Second","at":"A1","rows":[["changed",21]]}]}`)
	s := serve(e)
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		for {
			req, _ := http.NewRequest(method, s.server.URL+"/private"+path, strings.NewReader(body))
			req.Header.Set("X-Flats-Sheet", "1")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var out map[string]any
			_ = json.Unmarshal(data, &out)
			if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-ndjson") {
				frames, err := ndjsonFrames(data)
				if err != nil {
					t.Fatal(err)
				}
				out = map[string]any{"frames": frames}
			}
			if resp.StatusCode == http.StatusServiceUnavailable && out["reason"] == "calculating" {
				continue
			}
			return resp.StatusCode, out
		}
	}
	code, out := call(http.MethodGet, fmt.Sprintf("/_sheet/api/revisions/%d", rev), "")
	tabs, _ := out["tabs"].([]any)
	if code != http.StatusOK || len(tabs) != 3 {
		t.Fatalf("the tabs of the revision: %d %v", code, out)
	}
	restored := map[string]TabID{}
	for _, info := range tabs {
		name := info.(map[string]any)["name"].(string)
		id := TabID(info.(map[string]any)["id"].(float64))
		if id != ids[name] {
			t.Fatalf("the revision lists %q as tab %d, want %d", name, id, ids[name])
		}
		// The view: one head, the row with its three cells, and the end.
		code, view := call(http.MethodGet, fmt.Sprintf("/_sheet/api/revisions/%d/tabs/%d", rev, id), "")
		frames, _ := view["frames"].([]any)
		if code != http.StatusOK || len(frames) != 3 {
			t.Fatalf("the view of %q: %d %v", name, code, view)
		}
		row := frames[1].(map[string]any)["rows"].([]any)[0].([]any)[1].([]any)
		if len(row) != 3 || row[0].([]any)[2] != strings.ToLower(name) {
			t.Fatalf("the view of %q holds the row %v", name, row)
		}
		code, got := call(http.MethodPost, "/_sheet/api/recover", fmt.Sprintf(`{"revision":%d,"tab":%d}`, rev, id))
		if code != http.StatusOK || got["tab"] == nil {
			t.Fatalf("recover %q: %d %v", name, code, got)
		}
		restored[name] = TabID(got["tab"].(float64))
	}
	if got := e.tabNames(); !slices.Equal(got, []string{"One", "Second", "One (restored)", "Two", "Three"}) {
		t.Fatalf("the tabs after recovering every tab = %v", got)
	}
	// A tab that is still live comes back as a copy under a new id, and
	// the deleted one under its own.
	if restored["One"] == ids["One"] || restored["Two"] == ids["Two"] || restored["Three"] != ids["Three"] {
		t.Fatalf("the recovered tabs have the ids %v, the revision's are %v", restored, ids)
	}
	wantTexts(t, e, map[string]string{
		"'One (restored)'!A1": "one", "'One (restored)'!C1": "=B1*2",
		"Two!A1": "two", "Two!B1": "20", "Two!C1": "=B1+One!B1",
		"Three!A1": "three", "Three!C1": "=SUM(B1:B1)",
		"Second!A1": "changed", "One!A1": "one",
	})
	e.want("'One (restored)'!C1", "20")
	e.want("Two!C1", "30")
	e.want("Three!C1", "30")
}
