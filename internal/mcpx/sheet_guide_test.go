package mcpx

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sheetGuide returns topic.sheet as an agent reads it.
func sheetGuide(t *testing.T, cs *mcp.ClientSession) string {
	t.Helper()
	text, bad := call(t, cs, "guide", map[string]any{"items": []string{"topic.sheet"}}, nil)
	if bad || !strings.Contains(text, "## Sheets") {
		t.Fatalf("guide topic.sheet: %s", text)
	}
	return text
}

// TestSheetGuideNumberFormats applies every number format the guide writes
// to a cell that holds 1234.5 and compares what the cell shows with what
// the guide says it shows.
func TestSheetGuideNumberFormats(t *testing.T) {
	e := newEnv(t)
	cs := e.local
	rows := regexp.MustCompile("(?m)^\\| `(\\{\"kind\"[^`]*\\})` \\| `([^`]*)` \\|").FindAllStringSubmatch(sheetGuide(t, cs), -1)
	kinds := map[string]bool{}
	var out map[string]any
	if text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "formats", "ops": []any{}}, &out); bad {
		t.Fatalf("create: %s", text)
	}
	for i, row := range rows {
		var number map[string]any
		if err := json.Unmarshal([]byte(row[1]), &number); err != nil {
			t.Fatalf("the guide's number format %s is no JSON: %v", row[1], err)
		}
		kinds[fmt.Sprint(number["kind"])] = true
		at := fmt.Sprintf("A%d", i+1)
		state := out["state"]
		out = nil
		text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "formats", "state": state, "ops": []any{
			map[string]any{"op": "write", "tab": "Sheet1", "at": at, "rows": []any{[]any{1234.5}}},
			map[string]any{"op": "format", "tab": "Sheet1", "ranges": []any{at}, "format": map[string]any{"number": number}},
		}}, &out)
		if bad {
			t.Errorf("the guide's number format %s is refused: %s", row[1], text)
			out = nil
			call(t, cs, "get_sheet", map[string]any{"slug": "formats"}, &out)
			continue
		}
		var read map[string]any
		if text, bad := call(t, cs, "get_sheet", map[string]any{"slug": "formats", "tab": "Sheet1", "range": at, "include": []any{"shown", "formats"}}, &read); bad {
			t.Fatalf("read %s: %s", at, text)
		}
		if shown := read["shown"].([]any)[0].([]any)[0]; shown != row[2] {
			t.Errorf("1234.5 under %s shows %q, and the guide says %q", row[1], shown, row[2])
		}
		// The format a read returns is the object the guide writes. General
		// is no number format, so the read returns none.
		formats, _ := read["formats"].([]any)
		if number["kind"] == "general" {
			if len(formats) != 0 {
				t.Errorf("formats of %s under the general format = %+v", at, formats)
			}
			continue
		}
		if len(formats) != 1 {
			t.Fatalf("formats of %s = %+v", at, read["formats"])
		}
		got, _ := json.Marshal(formats[0].(map[string]any)["format"].(map[string]any)["number"])
		want, _ := json.Marshal(number)
		if string(got) != string(want) {
			t.Errorf("the read returns number format %s for %s", got, want)
		}
	}
	for _, kind := range []string{"general", "number", "percent", "currency", "date", "time", "datetime", "text"} {
		if !kinds[kind] {
			t.Errorf("the guide shows no %s number format object", kind)
		}
	}
}

// TestSheetNumberFormatSchema covers the values of each number format the
// schema of update_sheet publishes: every allowed value applies, and a
// format the sheet would refuse does not pass the schema.
func TestSheetNumberFormatSchema(t *testing.T) {
	e := newEnv(t)
	cs := e.local
	var out map[string]any
	if text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "formats", "ops": []any{}}, &out); bad {
		t.Fatalf("create: %s", text)
	}
	apply := func(number any) (string, bool) {
		t.Helper()
		var read map[string]any
		call(t, cs, "get_sheet", map[string]any{"slug": "formats"}, &read)
		return call(t, cs, "update_sheet", map[string]any{"slug": "formats", "state": read["state"], "ops": []any{
			map[string]any{"op": "format", "tab": "Sheet1", "ranges": []any{"A1"}, "format": map[string]any{"number": number}},
		}}, nil)
	}
	dates := []string{"yyyy-mm-dd", "m/d/yyyy", "mmm d, yyyy"}
	times := []string{"hh:mm", "hh:mm:ss", "h:mm AM/PM"}
	valid := []any{
		nil,
		map[string]any{"kind": "general"}, map[string]any{"kind": "text"},
		map[string]any{"kind": "number", "decimals": 0, "thousands": false},
		map[string]any{"kind": "number", "decimals": 10, "thousands": true},
		map[string]any{"kind": "percent", "decimals": 0}, map[string]any{"kind": "percent", "decimals": 10},
		map[string]any{"kind": "currency", "symbol": "€", "decimals": 0},
		map[string]any{"kind": "currency", "symbol": "USD", "decimals": 10},
	}
	for _, pattern := range dates {
		valid = append(valid, map[string]any{"kind": "date", "pattern": pattern})
		for _, clock := range times {
			valid = append(valid, map[string]any{"kind": "datetime", "date": pattern, "time": clock})
		}
	}
	for _, clock := range times {
		valid = append(valid, map[string]any{"kind": "time", "pattern": clock})
	}
	for _, number := range valid {
		if text, bad := apply(number); bad {
			t.Errorf("number format %v is refused: %s", number, text)
		}
	}
	for _, number := range []any{
		map[string]any{"kind": "number", "decimals": 2},
		map[string]any{"kind": "number", "thousands": true},
		map[string]any{"kind": "number", "decimals": 11, "thousands": true},
		map[string]any{"kind": "percent"},
		map[string]any{"kind": "percent", "decimals": 2, "thousands": true},
		map[string]any{"kind": "currency", "decimals": 2},
		map[string]any{"kind": "currency", "symbol": "", "decimals": 2},
		map[string]any{"kind": "currency", "symbol": "EURO", "decimals": 2},
		map[string]any{"kind": "currency", "symbol": "1", "decimals": 2},
		map[string]any{"kind": "date"},
		map[string]any{"kind": "date", "pattern": "dd.mm.yyyy"},
		map[string]any{"kind": "date", "pattern": "hh:mm"},
		map[string]any{"kind": "time", "pattern": "yyyy-mm-dd"},
		map[string]any{"kind": "datetime", "date": "yyyy-mm-dd"},
		map[string]any{"kind": "datetime", "pattern": "yyyy-mm-dd"},
		map[string]any{"kind": "general", "decimals": 2},
		map[string]any{"kind": "text", "pattern": "@"},
		map[string]any{"kind": "scientific"},
		map[string]any{"decimals": 2},
	} {
		text, bad := apply(number)
		if !bad {
			t.Errorf("number format %v was applied", number)
		} else if strings.Contains(text, `"category"`) {
			// The schema states the rule, so the sheet never has to refuse.
			t.Errorf("number format %v passes the schema and is refused by the sheet: %s", number, text)
		}
	}
}

// TestSheetGuideWriteExample sends the write call the guide prints, with
// the slug and the state of a real sheet.
func TestSheetGuideWriteExample(t *testing.T) {
	e := newEnv(t)
	cs := e.local
	guide := sheetGuide(t, cs)
	_, block, ok := strings.Cut(guide, "```json\n{\"slug\": \"offsite\"")
	if !ok {
		t.Fatal("the guide has no update_sheet example")
	}
	block, _, _ = strings.Cut(block, "```")
	var args map[string]any
	if err := json.Unmarshal([]byte(`{"slug": "offsite"`+block), &args); err != nil {
		t.Fatalf("the guide's update_sheet example is no JSON: %v", err)
	}
	var flat FlatInfo
	if text, bad := call(t, cs, "create_flat", map[string]any{"slug": "offsite", "type": "sheet"}, &flat); bad {
		t.Fatalf("create: %s", text)
	}
	var read map[string]any
	call(t, cs, "get_sheet", map[string]any{"slug": "offsite"}, &read)
	args["state"] = read["state"]
	var out map[string]any
	if text, bad := call(t, cs, "update_sheet", args, &out); bad || out["revision"] != float64(2) {
		t.Fatalf("the guide's update_sheet example: %s %+v", text, out)
	}
	// The two calls the guide says need no state.
	if text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "offsite", "ops": []any{map[string]any{"op": "add_tab", "name": "Notes"}}}, nil); bad {
		t.Errorf("a call that only adds tabs, without state: %s", text)
	}
	for _, guard := range []string{`{"unchanged": true}`, `{"ranges": ["Budget!B2:B8"]}`} {
		if !strings.Contains(guide, guard) {
			t.Fatalf("the guide does not show the guard %s", guard)
		}
		var g map[string]any
		_ = json.Unmarshal([]byte(guard), &g)
		read = nil
		call(t, cs, "get_sheet", map[string]any{"slug": "offsite"}, &read)
		if text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "offsite", "state": read["state"], "guard": g, "ops": []any{
			map[string]any{"op": "write", "tab": "Budget", "at": "D1", "rows": []any{[]any{"guarded"}}}}}, nil); bad {
			t.Errorf("a write with the guide's guard %s: %s", guard, text)
		}
	}
}

// TestSheetRevisionSelectors sends every selector combination get_revision
// documents for a sheet through tools/call.
func TestSheetRevisionSelectors(t *testing.T) {
	e := newEnv(t)
	cs := e.local
	var out map[string]any
	if text, bad := call(t, cs, "update_sheet", sheetTestWrite(""), &out); bad {
		t.Fatalf("create: %s", text)
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"limit alone", map[string]any{"limit": 10}, "tabs"},
		{"format json alone", map[string]any{"format": "json"}, "tabs"},
		{"format json with limit", map[string]any{"format": "json", "limit": 10}, "tabs"},
		{"tab", map[string]any{"tab": "Sheet1"}, "rows"},
		{"tab with limit", map[string]any{"tab": "Sheet1", "limit": 4}, "rows"},
		{"tab with range", map[string]any{"tab": "Sheet1", "range": "A1:B2"}, "rows"},
		{"tab with include", map[string]any{"tab": "Sheet1", "include": []any{"shown", "formats", "widths"}}, "shown"},
		{"tab with csv", map[string]any{"tab": "Sheet1", "format": "csv"}, "csv"},
		{"every selector", map[string]any{"tab": "Sheet1", "range": "A1:C4", "format": "json", "include": []any{"shown"}, "limit": 100}, "rows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"slug": "budget", "revision": 2}
			for name, value := range tc.args {
				args[name] = value
			}
			var historical RevisionOut
			text, bad := call(t, cs, "get_revision", args, &historical)
			if bad || historical.Sheet[tc.want] == nil || historical.Sheet["revision"] != float64(2) {
				t.Fatalf("get_revision %v: %s %+v", tc.args, text, historical)
			}
		})
	}
	// A page of a revision read continues with revision and cursor alone.
	var historical RevisionOut
	if _, bad := call(t, cs, "get_revision", map[string]any{"slug": "budget", "revision": 2, "tab": "Sheet1", "limit": 4}, &historical); bad {
		t.Fatal("first page")
	}
	next, _ := historical.Sheet["next"].(map[string]any)
	if next == nil {
		t.Fatalf("the first page has no next: %+v", historical.Sheet)
	}
	historical = RevisionOut{}
	if text, bad := call(t, cs, "get_revision", map[string]any{"slug": "budget", "revision": 2, "cursor": next["cursor"]}, &historical); bad || historical.Sheet["rows"] == nil {
		t.Fatalf("continuation: %s", text)
	}
}

// TestSheetGuideCellValues writes every cell object of the guide's typing
// table and reads it back, and follows the guide's statements about growing
// a tab, tab names and references to another tab.
func TestSheetGuideCellValues(t *testing.T) {
	e := newEnv(t)
	cs := e.local
	guide := sheetGuide(t, cs)
	_, table, _ := strings.Cut(guide, "| JSON | Cell |")
	table, _, _ = strings.Cut(table, "\n\n")
	objects := regexp.MustCompile("(?m)^\\s*\\| `(\\{[^`]*\\})` \\|").FindAllStringSubmatch(table, -1)
	if len(objects) != 4 {
		t.Fatalf("the typing table shows %d cell objects, want a date, a time, a datetime and a formula: %s", len(objects), table)
	}
	row := make([]any, len(objects))
	for i, m := range objects {
		if err := json.Unmarshal([]byte(m[1]), &row[i]); err != nil {
			t.Fatalf("cell object %s is no JSON: %v", m[1], err)
		}
	}
	var out map[string]any
	text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "cells", "ops": []any{
		map[string]any{"op": "write", "tab": "Sheet1", "at": "A1", "rows": []any{row}},
		// A write past the last row and column grows the tab.
		map[string]any{"op": "write", "tab": "Sheet1", "at": "AB1500", "rows": []any{[]any{"far"}}},
		// A name with spaces is quoted in a reference.
		map[string]any{"op": "add_tab", "name": "Q1 costs"},
		map[string]any{"op": "write", "tab": "Q1 costs", "at": "B3", "rows": []any{[]any{7}}},
		map[string]any{"op": "write", "tab": "Sheet1", "at": "A2", "rows": []any{[]any{"='Q1 costs'!B3*2", "'=not a formula"}}},
	}}, &out)
	if bad {
		t.Fatalf("the guide's cell values are refused: %s", text)
	}
	var read map[string]any
	if text, bad = call(t, cs, "get_sheet", map[string]any{"slug": "cells", "tab": "Sheet1", "range": "A1:D2"}, &read); bad {
		t.Fatalf("read: %s", text)
	}
	rows := read["rows"].([]any)
	for i, want := range row[:3] {
		got, _ := json.Marshal(rows[0].([]any)[i])
		wantText, _ := json.Marshal(want)
		if string(got) != string(wantText) {
			t.Errorf("the read returns %s for the written %s", got, wantText)
		}
	}
	second := rows[1].([]any)
	if second[0].(map[string]any)["value"] != float64(14) || second[1] != "'=not a formula" {
		t.Errorf("row 2 reads %+v", second)
	}
	read = nil
	call(t, cs, "get_sheet", map[string]any{"slug": "cells"}, &read)
	first := read["tabs"].([]any)[0].(map[string]any)
	if first["rows"].(float64) < 1500 || first["cols"].(float64) < 28 {
		t.Errorf("the tab did not grow to the far write: %+v", first)
	}
	// A name the guide rules out is refused, and one it allows applies.
	for name, ok := range map[string]bool{"Plan 2026 (draft)": true, "a/b": false, "q1 COSTS": false, strings.Repeat("n", 31): true, strings.Repeat("n", 32): false,
		"'24 plan": false, "plan ": false, " plan": false, "plan\tb": false, "it's fine": true,
		"History": false, "HISTORY": false, "history": false, "History 2026": true} {
		read = nil
		call(t, cs, "get_sheet", map[string]any{"slug": "cells"}, &read)
		text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "cells", "state": read["state"], "ops": []any{map[string]any{"op": "add_tab", "name": name}}}, nil)
		if bad == ok {
			t.Errorf("add_tab %q: refused=%t: %.300s", name, bad, text)
		}
	}
	// A sheet keeps a tab, and a tab keeps a row and a column.
	if text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "last", "ops": []any{}}, &out); bad {
		t.Fatalf("create: %s", text)
	}
	for _, op := range []map[string]any{
		{"op": "delete_tab", "tab": "Sheet1"},
		{"op": "delete_rows", "tab": "Sheet1", "at": 1, "count": 1000},
		{"op": "delete_cols", "tab": "Sheet1", "at": "A", "count": 26},
	} {
		if text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "last", "state": out["state"], "ops": []any{op}}, nil); !bad || !strings.Contains(text, `"category":"invalid"`) {
			t.Errorf("%v: %.300s", op, text)
		}
	}
}

// TestRevisionLimitAloneIsASheetRead covers get_revision with limit as its
// only selector on flats that are no sheet: the selector reads sheet
// content, so the call is refused as invalid and names what the flat takes.
func TestRevisionLimitAloneIsASheetRead(t *testing.T) {
	e := newEnv(t)
	cs := e.local
	for _, typ := range []string{"notebook", "design", "board"} {
		t.Run(typ, func(t *testing.T) {
			slug := "limit-" + typ
			if text, bad := call(t, cs, "create_flat", map[string]any{"slug": slug, "type": typ}, nil); bad {
				t.Fatalf("create: %s", text)
			}
			text, bad := call(t, cs, "get_revision", map[string]any{"slug": slug, "revision": 1, "limit": 10}, nil)
			if !bad || !strings.Contains(text, `"category":"invalid"`) || !strings.Contains(text, "limit") || strings.Contains(text, "not_notebook") {
				t.Fatalf("limit alone on a %s: refused=%t %s", typ, bad, text)
			}
			// The same revision without limit still answers.
			var out RevisionOut
			if text, bad := call(t, cs, "get_revision", map[string]any{"slug": slug, "revision": 1}, &out); bad || out.Revision == nil {
				t.Fatalf("metadata of a %s revision: %s", typ, text)
			}
		})
	}
	if text, bad := call(t, cs, "get_revision", map[string]any{"slug": "absent", "revision": 1, "limit": 10}, nil); !bad || !strings.Contains(text, `"category":"not_found"`) {
		t.Fatalf("limit alone on a missing flat: %s", text)
	}
}
