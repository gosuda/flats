package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

// sheetCall sends one management request for a sheet and decodes its reply.
func sheetCall(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	r, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %s = %d %s", method, url, resp.StatusCode, raw)
	}
	return resp.StatusCode, out
}

func TestHostSheetManagementRoutes(t *testing.T) {
	srv, svc := setup(t)
	base := srv.URL + "/api/flats/budget/sheet"
	rows := `{"op":"write","tab":"Sheet1","at":"A1","rows":[["Item","Amount"],["Venue",1200],["Total","=SUM(B2:B2)"]]}`
	code, out := sheetCall(t, "POST", base, `{"op_id":"create","ops":[`+rows+`]}`)
	result, _ := out["result"].(map[string]any)
	if code != 200 || out["created"] != true || out["revision"] != float64(2) || out["pending"] != false || result["cells_written"] != float64(6) || result["revision"] != float64(2) {
		t.Fatalf("creating write = %d %+v", code, out)
	}
	state, _ := result["state"].(string)
	view, err := svc.GetFlat(t.Context(), "budget")
	if err != nil || view.Type != "sheet" || !view.Running {
		t.Fatalf("created flat = %+v, %v", view, err)
	}
	// The creating write answers with the flat and its address.
	if flat, _ := out["flat"].(map[string]any); flat["slug"] != "budget" || flat["type"] != "sheet" || flat["private_url"] != view.PrivateURL || view.PrivateURL == "" {
		t.Fatalf("the creating write answers flat %+v, want the address %q", out["flat"], view.PrivateURL)
	}
	if code, out = sheetCall(t, "GET", base, ""); code != 200 || out["state"] != state || len(out["tabs"].([]any)) != 1 {
		t.Fatalf("tab list = %d %+v", code, out)
	}
	if code, out = sheetCall(t, "GET", base+"?tab=Sheet1&range=B3&include=shown", ""); code != 200 || out["rows"].([]any)[0].([]any)[0].(map[string]any)["value"] != float64(1200) || out["shown"].([]any)[0].([]any)[0] != "1200" {
		t.Fatalf("cell read = %d %+v", code, out)
	}
	if code, out = sheetCall(t, "GET", base+"?tab=Sheet1&format=csv", ""); code != 200 || out["csv"] != "Item,Amount\r\nVenue,1200\r\nTotal,1200\r\n" {
		t.Fatalf("csv = %d %+v", code, out)
	}
	if code, out = sheetCall(t, "POST", base, `{"state":"`+state+`","ops":[{"op":"write","tab":"Sheet1","at":"B2","rows":[[1500]]}]}`); code != 200 || out["revision"] != float64(3) || out["flat"] != nil {
		t.Fatalf("second write = %d %+v", code, out)
	}
	if code, out = sheetCall(t, "GET", base+"?tab=Sheet1&range=B3&revision=2", ""); code != 200 || out["revision"] != float64(2) || out["rows"].([]any)[0].([]any)[0].(map[string]any)["value"] != float64(1200) {
		t.Fatalf("revision read = %d %+v", code, out)
	}
	for _, tt := range []struct {
		name, method, url, body string
		code                    int
		category, want          string
	}{
		{"write without state", "POST", base, `{"ops":[` + rows + `]}`, 409, "conflict", "state_required"},
		{"slug in the body", "POST", base, `{"slug":"other","ops":[]}`, 400, "invalid", "slug"},
		{"unknown field", "POST", base, `{"state":"` + state + `","ops":[],"extra":1}`, 400, "invalid", "extra"},
		{"missing tab", "GET", base + "?tab=Gone", "", 404, "not_found", "Gone"},
		{"missing revision", "GET", base + "?tab=Sheet1&revision=9", "", 404, "not_found", ""},
		{"bad selector", "GET", base + "?limit=0", "", 400, "invalid", "limit"},
		{"missing flat", "GET", srv.URL + "/api/flats/absent/sheet", "", 404, "not_found", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			code, out := sheetCall(t, tt.method, tt.url, tt.body)
			text, _ := json.Marshal(out)
			if code != tt.code || (tt.category != "" && out["category"] != tt.category) || !strings.Contains(string(text), tt.want) {
				t.Fatalf("reply = %d %s", code, text)
			}
		})
	}
	if _, err := svc.CreateNotebook(t.Context(), "notes", "Notes", core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	if code, out = sheetCall(t, "GET", srv.URL+"/api/flats/notes/sheet", ""); code != 400 || out["category"] != "not_sheet" {
		t.Fatalf("sheet read of a notebook = %d %+v", code, out)
	}
	if code, out = sheetCall(t, "POST", srv.URL+"/api/flats/notes/sheet", `{"ops":[]}`); code != 400 || out["category"] != "not_sheet" {
		t.Fatalf("sheet write of a notebook = %d %+v", code, out)
	}
	if code, out = sheetCall(t, "GET", srv.URL+"/api/flats/budget/notebook", ""); code != 400 || out["category"] != "not_notebook" {
		t.Fatalf("notebook read of a sheet = %d %+v", code, out)
	}
	if code, out = sheetCall(t, "GET", srv.URL+"/api/flats/budget/versions", ""); code != 400 || out["category"] != "invalid" || !strings.Contains(out["error"].(string), "update_sheet") {
		t.Fatalf("version list of a sheet = %d %+v", code, out)
	}
}

// TestHostSheetCallBytesLimit sends update_sheet bodies at and just over the
// 8 MiB limit of one call through the management route.
func TestHostSheetCallBytesLimit(t *testing.T) {
	srv, _ := setup(t)
	base := srv.URL + "/api/flats/budget/sheet"
	if code, out := sheetCall(t, "POST", base, `{"ops":[]}`); code != 200 {
		t.Fatalf("create = %d %+v", code, out)
	}
	_, out := sheetCall(t, "GET", base, "")
	state, _ := out["state"].(string)
	// One cell holds at most 32,767 characters: the body is rows of such
	// cells, padded with white space to the size under test.
	const limit = 8 << 20
	cell := `["` + strings.Repeat("x", 30_000) + `"]`
	rows := strings.Repeat(cell+",", 100) + cell
	body := func(size int) string {
		text := `{"state":"` + state + `","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + rows + `]}]`
		return text + strings.Repeat(" ", size-len(text)-1) + "}"
	}
	code, out := sheetCall(t, "POST", base, body(limit+1))
	if code != 422 || out["category"] != "sheet_capacity" || out["limit"] != "call_bytes" || out["max"] != float64(limit) || out["value"] != float64(limit+1) || out["retry"] != false {
		t.Fatalf("a body one byte over the limit = %d %+v, want 422 sheet_capacity call_bytes", code, out)
	}
	if code, out = sheetCall(t, "GET", base, ""); code != 200 || out["state"] != state {
		t.Fatalf("the refused call changed the sheet: %d %+v", code, out)
	}
	if code, out = sheetCall(t, "POST", base, body(limit)); code != 200 || out["revision"] != float64(3) {
		t.Fatalf("a body of exactly the limit = %d %+v", code, out)
	}
}
