package mcpx

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

var sheetTestRows = []any{
	[]any{"Item", "Amount", "Due"},
	[]any{"Venue", 1200, map[string]any{"date": "2026-03-15"}},
	[]any{"Food", 900, nil},
	[]any{"Total", "=SUM(B2:B3)", "=B3/0"},
}

func sheetTestWrite(state string) map[string]any {
	args := map[string]any{"slug": "budget", "op_id": "rows-1", "message": "Budget rows", "ops": []any{
		map[string]any{"op": "write", "tab": "Sheet1", "at": "A1", "rows": sheetTestRows},
		map[string]any{"op": "format", "tab": "Sheet1", "ranges": []any{"B2:B4"}, "format": map[string]any{"number": map[string]any{"kind": "currency", "symbol": "$", "decimals": 2}}},
	}}
	if state != "" {
		args["state"] = state
	}
	return args
}

func TestSheetToolsRoundTrip(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("autopilot-%t", on), func(t *testing.T) {
			e := newEnv(t)
			if on {
				confirmed := e.svc.Autopilot().Allowed
				if _, err := e.svc.SetAutopilotEnabled(t.Context(), true, &confirmed, "test"); err != nil {
					t.Fatal(err)
				}
			}
			cs := connect(t, e.localURL)
			listed, err := cs.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			found := map[string]*mcp.Tool{}
			for _, tool := range listed.Tools {
				found[tool.Name] = tool
			}
			for _, name := range []string{"get_sheet", "update_sheet"} {
				tool := found[name]
				if tool == nil || !strings.Contains(tool.Description, "topic.sheet") || tool.OutputSchema == nil || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
					t.Fatalf("missing contract for %s: %+v", name, tool)
				}
			}
			if !found["get_sheet"].Annotations.ReadOnlyHint || found["update_sheet"].Annotations.ReadOnlyHint {
				t.Fatal("wrong read-only annotation")
			}
			for _, name := range []string{"create_flat", "list_revisions", "get_revision", "pin_revision", "rollback_revision"} {
				if !strings.Contains(found[name].Description, "sheet") {
					t.Errorf("%s does not mention sheets", name)
				}
			}

			var flat FlatInfo
			text, bad := call(t, cs, "create_flat", map[string]any{"slug": "budget", "type": "sheet", "name": "Offsite budget"}, &flat)
			if bad || flat.Type != "sheet" || !flat.Running || flat.Draft != nil || flat.LiveVersion != 0 || !strings.Contains(text, "one empty tab named Sheet1") || !strings.Contains(text, "sheet, running") {
				t.Fatalf("create: %s %+v", text, flat)
			}
			var out map[string]any
			text, bad = call(t, cs, "get_sheet", map[string]any{"slug": "budget"}, &out)
			tabs, _ := out["tabs"].([]any)
			state, _ := out["state"].(string)
			if bad || len(tabs) != 1 || tabs[0].(map[string]any)["name"] != "Sheet1" || state == "" || out["date"] == nil {
				t.Fatalf("tab list: %s %+v", text, out)
			}
			out = nil
			text, bad = call(t, cs, "update_sheet", sheetTestWrite(state), &out)
			if bad || out["revision"] != float64(2) || out["cells_written"] != float64(11) || out["formulas_written"] != float64(2) || out["formula_errors"] != float64(1) ||
				!strings.Contains(text, "Revision 2.") || !strings.Contains(text, "1 formulas of this call show an error") || !strings.Contains(text, "no publish step") {
				t.Fatalf("write: %s %+v", text, out)
			}
			if errs := out["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["at"] != "Sheet1!C4" || errs[0].(map[string]any)["error"] != "#DIV/0!" {
				t.Fatalf("formula errors: %+v", out["errors"])
			}
			next, _ := out["state"].(string)
			if next == "" || next == state {
				t.Fatalf("the write returned no new state: %+v", out)
			}
			out = nil
			text, bad = call(t, cs, "update_sheet", sheetTestWrite(state), &out)
			if bad || out["replay"] != true || out["revision"] != float64(2) || !strings.Contains(text, "already applied") {
				t.Fatalf("replay: %s %+v", text, out)
			}

			out = nil
			_, bad = call(t, cs, "get_sheet", map[string]any{"slug": "budget", "tab": "Sheet1", "range": "A2:C4", "include": []any{"shown", "formats"}}, &out)
			rows, _ := out["rows"].([]any)
			if bad || len(rows) != 3 || out["range"] != "A2:C4" || out["state"] != next {
				t.Fatalf("read: %+v", out)
			}
			venue, total := rows[0].([]any), rows[2].([]any)
			if venue[1] != float64(1200) || venue[2].(map[string]any)["date"] != "2026-03-15" || rows[1].([]any)[2] != nil ||
				total[1].(map[string]any)["value"] != float64(2100) || total[2].(map[string]any)["error"] != "#DIV/0!" {
				t.Fatalf("typed values: %+v", rows)
			}
			if shown := out["shown"].([]any)[2].([]any); shown[1] != "$2,100.00" || shown[2] != "#DIV/0!" {
				t.Fatalf("shown values: %+v", out["shown"])
			}
			if formats, _ := out["formats"].([]any); len(formats) == 0 {
				t.Fatalf("formats: %+v", out)
			}
			// A value a read returns can be written back unchanged.
			back := map[string]any{"slug": "budget", "state": next, "ops": []any{map[string]any{"op": "write", "tab": "Sheet1", "at": "E2", "rows": rows}}}
			out = nil
			if text, bad = call(t, cs, "update_sheet", back, &out); bad || out["formulas_written"] != float64(2) {
				t.Fatalf("write back: %s %+v", text, out)
			}

			out = nil
			_, bad = call(t, cs, "get_sheet", map[string]any{"slug": "budget", "tab": "Sheet1", "range": "A1:B4", "limit": 4}, &out)
			page, _ := out["next"].(map[string]any)
			if bad || len(out["rows"].([]any)) != 2 || page == nil || page["start"] != "A3" {
				t.Fatalf("first page: %+v", out)
			}
			out = nil
			text, bad = call(t, cs, "get_sheet", map[string]any{"slug": "budget", "cursor": page["cursor"]}, &out)
			if bad || len(out["rows"].([]any)) != 2 || out["next"] != nil || out["start"] != "A3" || strings.Contains(text, "More:") {
				t.Fatalf("second page: %s %+v", text, out)
			}
			out = nil
			// The CSV holds values (spec rule 60): the currency cells and
			// the formula's result are written as plain numbers.
			_, bad = call(t, cs, "get_sheet", map[string]any{"slug": "budget", "tab": "Sheet1", "range": "A1:B4", "format": "csv"}, &out)
			if csv, _ := out["csv"].(string); bad || !strings.Contains(csv, "\r\nVenue,1200\r\n") || !strings.Contains(csv, "\r\nTotal,2100\r\n") || out["rows"] != nil {
				t.Fatalf("csv: %+v", out)
			}

			var revisions struct {
				Revisions []store.Revision `json:"revisions"`
			}
			out = nil
			_, bad = call(t, cs, "list_revisions", map[string]any{"slug": "budget"}, &revisions)
			if bad || len(revisions.Revisions) != 3 || len(revisions.Revisions[1].SheetTabs) != 1 || revisions.Revisions[1].SheetTabs[0].NameAfter != "Sheet1" || revisions.Revisions[1].Message != "Budget rows" {
				t.Fatalf("revisions: %+v", revisions)
			}
			var historical RevisionOut
			historical = RevisionOut{}
			text, bad = call(t, cs, "get_revision", map[string]any{"slug": "budget", "revision": 2, "tab": "Sheet1", "range": "E1:F4"}, &historical)
			if bad || historical.Sheet["revision"] != float64(2) || historical.Sheet["range"] != nil || !strings.Contains(text, "as of revision 2") {
				t.Fatalf("historical blank range: %s %+v", text, historical)
			}
			historical = RevisionOut{}
			_, bad = call(t, cs, "get_revision", map[string]any{"slug": "budget", "revision": 2, "tab": "Sheet1", "range": "A1:B4", "limit": 4}, &historical)
			old, _ := historical.Sheet["next"].(map[string]any)
			if bad || old == nil {
				t.Fatalf("historical first page: %+v", historical)
			}
			historical = RevisionOut{}
			_, bad = call(t, cs, "get_revision", map[string]any{"slug": "budget", "revision": 2, "cursor": old["cursor"]}, &historical)
			if bad || historical.Sheet["start"] != "A3" || len(historical.Sheet["rows"].([]any)) != 2 {
				t.Fatalf("historical continuation: %+v", historical)
			}
			historical = RevisionOut{}
			_, bad = call(t, cs, "get_revision", map[string]any{"slug": "budget", "revision": 1, "format": "json"}, &historical)
			if first := historical.Sheet["tabs"].([]any); bad || len(first) != 1 || first[0].(map[string]any)["used"] != nil {
				t.Fatalf("historical tab list: %+v", historical)
			}
			historical = RevisionOut{}
			_, bad = call(t, cs, "get_revision", map[string]any{"slug": "budget", "revision": 2}, &historical)
			if bad || historical.Revision == nil || historical.Sheet != nil || len(historical.Files) == 0 {
				t.Fatalf("revision metadata: %+v", historical)
			}

			var rolled map[string]any
			out = nil
			text, bad = call(t, cs, "rollback_revision", map[string]any{"slug": "budget", "revision": 2}, &rolled)
			if bad {
				t.Fatalf("rollback: %s", text)
			}
			out = nil
			_, bad = call(t, cs, "get_sheet", map[string]any{"slug": "budget"}, &out)
			if used := out["tabs"].([]any)[0].(map[string]any)["used"]; bad || used != "A1:C4" {
				t.Fatalf("sheet after the rollback: %+v", out)
			}
			out = nil
			if text, bad = call(t, cs, "pin_revision", map[string]any{"slug": "budget", "revision": 2, "pinned": true}, nil); bad {
				t.Fatalf("pin: %s", text)
			}
		})
	}
}

func TestSheetToolRefusals(t *testing.T) {
	e := newEnv(t)
	cs := e.local
	var out map[string]any
	text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "budget", "ops": []any{map[string]any{"op": "write", "tab": "Sheet1", "at": "A1", "rows": []any{[]any{1}}}}}, &out)
	if bad || out["created"] != true || !strings.Contains(text, "Created sheet budget") {
		t.Fatalf("creating write: %s %+v", text, out)
	}
	// The creating call returns where the sheet opens.
	if flat, _ := out["flat"].(map[string]any); flat["private_url"] == nil || flat["private_url"] == "" || flat["type"] != "sheet" || !strings.Contains(text, flat["private_url"].(string)) {
		t.Fatalf("the creating write returns no address: %s %+v", text, out["flat"])
	}
	state := out["state"].(string)
	call(t, cs, "create_flat", map[string]any{"slug": "notes", "type": "notebook"}, nil)
	for _, tc := range []struct {
		name, tool string
		args       map[string]any
		want       []string
	}{
		{"schema: unknown read field", "get_sheet", map[string]any{"slug": "budget", "extra": 1}, nil},
		{"schema: cursor with selector", "get_sheet", map[string]any{"slug": "budget", "cursor": "c_x", "tab": "Sheet1"}, nil},
		{"schema: range without tab", "get_sheet", map[string]any{"slug": "budget", "range": "A1"}, nil},
		{"schema: limit over the page", "get_sheet", map[string]any{"slug": "budget", "tab": "Sheet1", "limit": 10001}, nil},
		{"schema: unknown operation", "update_sheet", map[string]any{"slug": "budget", "state": state, "ops": []any{map[string]any{"op": "erase", "tab": "Sheet1"}}}, nil},
		{"schema: field of another operation", "update_sheet", map[string]any{"slug": "budget", "state": state, "ops": []any{map[string]any{"op": "delete_tab", "tab": "Sheet1", "name": "x"}}}, nil},
		{"schema: object cell of another kind", "update_sheet", map[string]any{"slug": "budget", "state": state, "ops": []any{map[string]any{"op": "write", "tab": "Sheet1", "at": "A1", "rows": []any{[]any{map[string]any{"text": "x"}}}}}}, nil},
		{"schema: sheet selector with path", "get_revision", map[string]any{"slug": "budget", "revision": 1, "tab": "Sheet1", "path": "sheet.json"}, nil},
		{"missing state", "update_sheet", map[string]any{"slug": "budget", "ops": []any{map[string]any{"op": "clear", "tab": "Sheet1", "ranges": []any{"A1"}}}},
			[]string{`"category":"conflict"`, `"reason":"state_required"`, "refusal.conflict", "get_sheet"}},
		{"unknown state", "update_sheet", map[string]any{"slug": "budget", "state": "s_other.1", "ops": []any{}},
			[]string{`"category":"conflict"`, `"reason":"state_unknown"`}},
		{"missing tab", "update_sheet", map[string]any{"slug": "budget", "state": state, "ops": []any{map[string]any{"op": "delete_tab", "tab": "Gone"}}},
			[]string{`"category":"`, "Gone"}},
		{"last tab", "update_sheet", map[string]any{"slug": "budget", "state": state, "ops": []any{map[string]any{"op": "delete_tab", "tab": "Sheet1"}}},
			[]string{`"category":"invalid"`}},
		{"too many tabs", "update_sheet", map[string]any{"slug": "budget", "state": state, "ops": sheetTestTabs(50)},
			[]string{`"category":"sheet_capacity"`, `"limit":"tabs"`, `"max":50`, "refusal.sheet_capacity"}},
		{"bad address", "update_sheet", map[string]any{"slug": "budget", "state": state, "ops": []any{map[string]any{"op": "write", "tab": "Sheet1", "at": "1A", "rows": []any{[]any{1}}}}},
			[]string{`"category":"invalid"`, "refusal.invalid"}},
		{"expired cursor", "get_sheet", map[string]any{"slug": "budget", "cursor": "c_0000000000000000_0_0_0"},
			[]string{`"category":"conflict"`, `"reason":"read_expired"`}},
		{"missing tab read", "get_sheet", map[string]any{"slug": "budget", "tab": "Gone"},
			[]string{`"category":"not_found"`}},
		{"revision through get_sheet", "get_sheet", map[string]any{"slug": "budget", "revision": 1}, nil},
		{"sheet read on a notebook", "get_sheet", map[string]any{"slug": "notes"},
			[]string{`"category":"not_sheet"`, "create_flat {type: sheet}", "refusal.not_sheet"}},
		{"sheet write on a notebook", "update_sheet", map[string]any{"slug": "notes", "ops": []any{}},
			[]string{`"category":"not_sheet"`}},
		{"notebook write on a sheet", "update_notebook", map[string]any{"slug": "budget", "op_id": "x", "ops": []any{}},
			[]string{`"category":"not_notebook"`}},
		{"missing revision", "get_revision", map[string]any{"slug": "budget", "revision": 9, "tab": "Sheet1"},
			[]string{`"category":"not_found"`}},
		{"save draft", "save_draft", map[string]any{"slug": "budget", "files": []any{file("index.html", "x", "")}},
			[]string{`"category":"conflict"`, "update_sheet"}},
		{"get draft", "get_draft", map[string]any{"slug": "budget"},
			[]string{`"category":"invalid"`, "sheets have no Draft, versions, previews or deployment", "topic.sheet"}},
		{"create version", "create_version", map[string]any{"slug": "budget", "revision": 1},
			[]string{`"category":"invalid"`, "update_sheet", "topic.sheet"}},
		{"deploy", "deploy", map[string]any{"slug": "budget", "version": 1},
			[]string{`"category":"invalid"`, "topic.sheet"}},
		{"version rollback", "rollback", map[string]any{"slug": "budget", "version": 1},
			[]string{`"category":"invalid"`, "topic.sheet"}},
		{"preview", "open_preview", map[string]any{"slug": "budget", "version": 0},
			[]string{`"category":"invalid"`, "topic.sheet"}},
		{"list versions", "list_versions", map[string]any{"slug": "budget"},
			[]string{`"category":"invalid"`, "topic.sheet"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, bad := call(t, cs, tc.tool, tc.args, nil)
			if !bad {
				t.Fatalf("the call was accepted: %s", text)
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("the refusal lacks %q: %s", want, text)
				}
			}
		})
	}
	if _, bad := call(t, cs, "get_sheet", map[string]any{"slug": "budget"}, &out); bad || out["state"] != state {
		t.Fatalf("a refused call changed the sheet: %+v", out)
	}
	var page struct {
		Revisions []store.Revision `json:"revisions"`
	}
	if _, bad := call(t, cs, "list_revisions", map[string]any{"slug": "budget"}, &page); bad || len(page.Revisions) != 2 {
		t.Fatalf("a refused call recorded a revision: %+v", page)
	}
	var flat FlatInfo
	if text, bad := call(t, cs, "get_flat", map[string]any{"slug": "budget"}, &flat); bad || !strings.Contains(text, "sheet, running") || strings.Contains(text, "version(s)") || strings.Contains(text, "save_document") {
		t.Fatalf("flat text: %s", text)
	}
}

func sheetTestTabs(n int) []any {
	ops := make([]any, n)
	for i := range ops {
		ops[i] = map[string]any{"op": "add_tab", "name": fmt.Sprintf("Tab %d", i)}
	}
	return ops
}

func TestSheetPublicInstanceMCP(t *testing.T) {
	e := newEnv(t)
	publicInstanceOn(t, e)
	cs := connect(t, e.localURL)
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "update_sheet" && !strings.Contains(tool.Description, publicCreationNote) {
			t.Fatal("sheet first-write creation omits public instance consent")
		}
	}
	var created FlatInfo
	text, failed := call(t, cs, "create_flat", map[string]any{"slug": "public-budget", "type": "sheet"}, &created)
	if failed || created.Visibility != string(store.Public) || !created.Running || created.PrivateURL != "" || strings.Contains(text, "Private URL") || !strings.Contains(text, core.PublicAccessNotice) {
		t.Fatalf("public sheet creation = %+v, %s", created, text)
	}
	var out map[string]any
	text, failed = call(t, cs, "update_sheet", map[string]any{"slug": "first-write", "ops": []any{map[string]any{"op": "write", "tab": "Sheet1", "at": "A1", "rows": []any{[]any{1}}}}}, &out)
	if failed || out["created"] != true || !strings.Contains(text, "This sheet is Public") {
		t.Fatalf("public first write = %s %+v", text, out)
	}
	view, err := e.svc.GetFlat(t.Context(), "first-write")
	if err != nil || view.Visibility != store.Public || view.PrivateURL != "" || view.PublicURL == "" {
		t.Fatalf("first-write sheet = %+v, %v", view, err)
	}
	// The creating call itself returns the Public address and no private link.
	flat, _ := out["flat"].(map[string]any)
	if flat["public_url"] != view.PublicURL || flat["visibility"] != string(store.Public) || flat["connection_state"] != "ready" {
		t.Fatalf("the creating write returns flat %+v, want the ready Public address %s", flat, view.PublicURL)
	}
	if private, _ := flat["private_url"].(string); private != "" || strings.Contains(text, "Private URL") {
		t.Fatalf("the creating write hands out a private link: %s %+v", text, flat)
	}
	if !strings.Contains(text, view.PublicURL) || !strings.Contains(text, core.PublicAccessNotice) {
		t.Fatalf("the creating write's text lacks the Public address or its notice: %s", text)
	}
	// A later write is no creation and returns no flat.
	out = nil
	call(t, cs, "get_sheet", map[string]any{"slug": "first-write"}, &out)
	state := out["state"]
	out = nil
	if text, failed = call(t, cs, "update_sheet", map[string]any{"slug": "first-write", "state": state, "ops": []any{}}, &out); failed || out["flat"] != nil || out["created"] == true {
		t.Fatalf("second write = %s %+v", text, out)
	}
	var guide GuideOut
	text, failed = call(t, cs, "guide", map[string]any{"items": []string{"topic.sheet", "topic.index"}}, &guide)
	if failed || strings.Contains(text, "A new sheet is Private") || strings.Contains(text, "`private_url`: it opens the sheet") || !strings.Contains(text, "Browser History is disabled") {
		t.Fatalf("sheet public instance guidance: %s", text)
	}
}

func TestHostSheetDiscoveryAndBodyLimit(t *testing.T) {
	if !strings.Contains(guideDescription, "topic.sheet") {
		t.Error("guide description omits topic.sheet")
	}
	if got := maxBody(1 << 20); got < 9<<20 {
		t.Errorf("the request limit %d does not admit an 8 MiB update_sheet call", got)
	}
	e := newEnv(t)
	srv := httptest.NewServer(LLMsHandler(e.svc, Options{Version: "test"}))
	defer srv.Close()
	for _, tc := range []struct {
		name string
		path string
		want []string
	}{
		{"index", LLMsPath, []string{"spreadsheets", "get_sheet", "update_sheet", "topic.sheet"}},
		{"guide", AgentGuidePath, []string{"flats sheet get", "flats sheet apply", "topic.sheet", "Sheets", "not_sheet", "sheet_capacity"}},
		{"full", LLMsFullPath, []string{"spreadsheets", "flats sheet get", "flats sheet apply"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, text := fetch(t, srv.Client(), http.MethodGet, srv.URL+tc.path)
			if code != 200 {
				t.Fatalf("discovery status = %d", code)
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("discovery omits %q", want)
				}
			}
		})
	}
}

// Every operation of section 18.2 passes the input schema and applies.
func TestSheetEveryOperationPassesTheSchema(t *testing.T) {
	e := newEnv(t)
	ops := []any{
		map[string]any{"op": "rename_tab", "tab": "Sheet1", "name": "Budget"},
		map[string]any{"op": "write", "tab": "Budget", "at": "A1", "rows": []any{
			[]any{"Item", "Amount", "Share", "When"},
			[]any{"Venue", 1200.5, "=B2/$B$4", map[string]any{"datetime": "2026-03-15T14:30:00"}},
			[]any{"Food", true, map[string]any{"formula": "=B3/0", "error": "#DIV/0!"}, map[string]any{"time": "14:30"}},
			[]any{"'=text", nil, map[string]any{"formula": "=1+1", "value": 2}, map[string]any{"date": "2026-03-15"}},
		}},
		map[string]any{"op": "format", "tab": "Budget", "ranges": []any{"A1:D1", "B2:B4"}, "format": map[string]any{
			"bold": true, "italic": nil, "underline": false, "strikethrough": false, "wrap": true,
			"text_color": "red-600", "fill": nil, "align": "center", "number": map[string]any{"kind": "percent", "decimals": 1}}},
		map[string]any{"op": "widths", "tab": "Budget", "cols": "B:C", "width": 180},
		map[string]any{"op": "insert_rows", "tab": "Budget", "at": 2, "count": 2},
		map[string]any{"op": "delete_rows", "tab": "Budget", "at": 2, "count": 1},
		map[string]any{"op": "insert_cols", "tab": "Budget", "at": "B", "count": 1},
		map[string]any{"op": "delete_cols", "tab": "Budget", "at": "B", "count": 1},
		map[string]any{"op": "sort", "tab": "Budget", "rows": "3:5", "by": "A", "desc": true},
		map[string]any{"op": "clear", "tab": "Budget", "ranges": []any{"D1:D9"}, "formats": true},
		map[string]any{"op": "add_tab", "name": "Notes", "at": 1},
		map[string]any{"op": "add_tab", "name": "Scratch"},
		map[string]any{"op": "move_tab", "tab": "Notes", "to": 3},
		map[string]any{"op": "tab_settings", "tab": "Budget", "frozen_rows": 1},
		map[string]any{"op": "import_csv", "tab": "Notes", "at": "A1", "csv": "a,b\r\n1,2026-03-15\r\n"},
		map[string]any{"op": "delete_tab", "tab": "Scratch"},
	}
	var out map[string]any
	text, bad := call(t, e.local, "update_sheet", map[string]any{"slug": "every-op", "op_id": "all", "message": "Every operation", "ops": ops}, &out)
	if bad || out["created"] != true {
		t.Fatalf("the call with every operation was refused: %s", text)
	}
	out = nil
	if _, bad = call(t, e.local, "get_sheet", map[string]any{"slug": "every-op"}, &out); bad || len(out["tabs"].([]any)) != 2 {
		t.Fatalf("tabs after every operation: %+v", out)
	}
	state := out["state"]
	out = nil
	guarded := map[string]any{"slug": "every-op", "state": state, "guard": map[string]any{"ranges": []any{"Budget!A1:B2"}},
		"ops": []any{map[string]any{"op": "write", "tab": "Budget", "at": "A9", "rows": []any{[]any{"guarded"}}}}}
	if text, bad = call(t, e.local, "update_sheet", guarded, &out); bad {
		t.Fatalf("guarded write: %s", text)
	}
}

// TestSheetCallBytesLimitMCP sends an update_sheet call over the 8 MiB limit
// of one call: the refusal has the category, limit and bound the management
// API answers.
func TestSheetCallBytesLimitMCP(t *testing.T) {
	e := newEnv(t)
	cs := e.local
	var out map[string]any
	if text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "budget", "ops": []any{}}, &out); bad {
		t.Fatalf("create: %s", text)
	}
	rows := make([]any, 290)
	for i := range rows {
		rows[i] = []any{strings.Repeat("x", 30_000)}
	}
	text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "budget", "state": out["state"], "ops": []any{
		map[string]any{"op": "write", "tab": "Sheet1", "at": "A1", "rows": rows}}}, nil)
	if !bad {
		t.Fatal("a call over 8 MiB was applied")
	}
	for _, want := range []string{`"category":"sheet_capacity"`, `"limit":"call_bytes"`, `"max":8388608`, `"retry":false`, "refusal.sheet_capacity"} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal lacks %s: %.600s", want, text)
		}
	}
}

// TestSheetToolErrorsMatchTheAPI covers errors core returns without a sheet
// refusal: the tool error carries what the management API answers for them.
func TestSheetToolErrorsMatchTheAPI(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want []string
	}{
		{fmt.Errorf("%w: sheet budget is not running", core.ErrUnavailable), []string{`"category":"unavailable"`, `"reason":"sheet_unavailable"`, `"retry":true`, "refusal.unavailable"}},
		{fmt.Errorf("%w: budget is a notebook", core.ErrNotSheet), []string{`"category":"not_sheet"`, "refusal.not_sheet"}},
		{store.ErrNotFound, []string{`"category":"not_found"`, "refusal.not_found"}},
		{core.ErrSheetUnversioned, []string{`"category":"invalid"`, "update_sheet"}},
	} {
		text := sheetToolErr(tc.err, "budget").Error()
		for _, want := range tc.want {
			if !strings.Contains(text, want) {
				t.Errorf("tool error of %v lacks %s: %s", tc.err, want, text)
			}
		}
	}
}
