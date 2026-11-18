package mcpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gosuda/flats/internal/sheet"
)

// TestSheetUpdateSchemaStatesTheReader sends update_sheet calls at the
// edges of what the sheet's reader takes, through the tool as an agent
// does. The published schema and the reader agree wherever a schema can
// check: what the schema passes is applied, and what the reader refuses
// for its JSON type does not pass the schema. A schema reads a number by
// its value, so it cannot refuse a whole number written as 2.0 or -0. For
// those the reader's refusal names the field, and the description of that
// field in the schema and the guide state the rule, so that an agent can
// correct the call from what it was served.
func TestSheetUpdateSchemaStatesTheReader(t *testing.T) {
	e := newEnv(t)
	cs := e.local
	if text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "edges", "ops": []any{map[string]any{"op": "add_tab", "name": "Aside"}}}, nil); bad {
		t.Fatalf("create: %s", text)
	}
	state := func() string {
		t.Helper()
		var read map[string]any
		if text, bad := call(t, cs, "get_sheet", map[string]any{"slug": "edges"}, &read); bad {
			t.Fatalf("get_sheet: %s", text)
		}
		return read["state"].(string)
	}
	// send applies one operation and reports how it ended: "applied", or
	// "schema" for arguments that did not pass the tool's input schema, or
	// the refusal of the sheet as category, reason and detail.
	send := func(op string) (outcome, reason, detail string) {
		t.Helper()
		args := fmt.Sprintf(`{"slug":"edges","state":%q,"ops":[%s]}`, state(), op)
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "update_sheet", Arguments: json.RawMessage(args)})
		if err != nil {
			return "schema", "", err.Error()
		}
		if !res.IsError {
			return "applied", "", ""
		}
		var text strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text.WriteString(tc.Text)
			}
		}
		_, last, _ := strings.Cut(text.String(), "\n{")
		var refusal struct{ Category, Reason, Detail string }
		if json.Unmarshal([]byte("{"+last), &refusal) != nil || refusal.Category == "" {
			return "schema", "", text.String()
		}
		return refusal.Category, refusal.Reason, refusal.Detail
	}
	write := func(cell string) string {
		return `{"op":"write","tab":"Sheet1","at":"A1","rows":[[` + cell + `]]}`
	}
	settings := func(field, value string) string {
		return `{"op":"tab_settings","tab":"Sheet1","` + field + `":` + value + `}`
	}
	percent := func(decimals string) string {
		return `{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"number":{"kind":"percent","decimals":` + decimals + `}}}`
	}

	// What the schema shows as allowed applies.
	for _, op := range []string{
		write(`{"formula":"=1+1","value":2}`), write(`{"formula":"=\"a\"","value":"a"}`),
		write(`{"formula":"=TRUE","value":true}`), write(`{"formula":"=A2","value":null}`),
		write(`{"formula":"=1/0","error":"#DIV/0!"}`), write(`{"formula":"=1","value":1,"error":"#N/A"}`),
		// Any character may follow the = of a formula, in both forms.
		write(`{"formula":"=\n1+1"}`), write(`"=\n1+1"`),
		write(`1.0`), write(`1e3`), write(`-0`), write(`12345678901234567890123`),
		settings("frozen_rows", "0"), settings("frozen_rows", "20"), settings("frozen_cols", "10"),
		`{"op":"widths","tab":"Sheet1","cols":"A:B","width":40}`, `{"op":"widths","tab":"Sheet1","cols":"A:B","width":1000}`,
		`{"op":"insert_rows","tab":"Sheet1","at":1,"count":1}`, `{"op":"delete_rows","tab":"Sheet1","at":1,"count":1}`,
		`{"op":"move_tab","tab":"Aside","to":1}`, `{"op":"add_tab","name":"Third","at":1}`,
		percent("0"), percent("10"),
	} {
		if outcome, reason, detail := send(op); outcome != "applied" {
			t.Errorf("%s = %s %s: %s", op, outcome, reason, detail)
		}
	}

	// A value or an error of another type than a read returns passes
	// neither the schema nor the reader, which names the cell.
	for _, cell := range []string{
		`{"formula":"=1","value":{}}`, `{"formula":"=1","value":[1]}`, `{"formula":"=1","value":{"date":"2026-03-15"}}`,
		`{"formula":"=1","error":5}`, `{"formula":"=1","error":null}`, `{"formula":"=1","error":{}}`, `{"formula":"=1","error":true}`,
	} {
		before := state()
		if outcome, reason, detail := send(write(cell)); outcome != "schema" {
			t.Errorf("%s passes the schema: %s %s: %s", cell, outcome, reason, detail)
		}
		if state() != before {
			t.Fatalf("%s changed the sheet", cell)
		}
		_, err := sheet.DecodeUpdate([]byte(`{"ops":[` + write(cell) + `]}`))
		var refusal *sheet.WriteError
		field := "value"
		if strings.Contains(cell, `"error"`) {
			field = "error"
		}
		if !errors.As(err, &refusal) || refusal.Code != "invalid" || refusal.Reason != "cell" || !strings.Contains(refusal.Detail, "rows[0][0]."+field) {
			t.Errorf("the reader takes %s as %v, want an invalid cell that names %s", cell, err, field)
		}
	}

	// A whole number in another written form passes the schema, which
	// reads its value, and the reader refuses it with its field.
	for _, c := range []struct{ op, reason, names string }{
		{settings("frozen_rows", "-0"), "frozen_rows", "frozen_rows"},
		{settings("frozen_rows", "1.0"), "frozen_rows", "frozen_rows"},
		{settings("frozen_cols", "1e0"), "frozen_cols", "frozen_cols"},
		{`{"op":"insert_rows","tab":"Sheet1","at":1,"count":2.0}`, "count", "count"},
		{`{"op":"insert_rows","tab":"Sheet1","at":1.0,"count":2}`, "at", "at"},
		{`{"op":"insert_rows","tab":"Sheet1","at":1,"count":1000000000000000000}`, "count", "count"},
		{`{"op":"widths","tab":"Sheet1","cols":"A:B","width":40.0}`, "width", "width"},
		{`{"op":"move_tab","tab":"Aside","to":1.0}`, "to", "to"},
		{`{"op":"add_tab","name":"Fourth","at":1.0}`, "at", "at"},
		{percent("2.0"), "format", "format.number.decimals"},
		{percent("-0"), "format", "format.number.decimals"},
	} {
		before := state()
		outcome, reason, detail := send(c.op)
		if outcome != "invalid" || reason != c.reason || !strings.Contains(detail, c.names+" takes a whole number") || !strings.Contains(detail, "operation 1") {
			t.Errorf("%s = %s %s: %s, want invalid %s with a detail that names %s", c.op, outcome, reason, detail, c.reason, c.names)
		}
		if state() != before {
			t.Fatalf("%s changed the sheet", c.op)
		}
	}
	// 18 digits are a whole number: this count is refused for its size.
	if outcome, _, _ := send(`{"op":"insert_rows","tab":"Sheet1","at":1,"count":100000000000000000}`); outcome != "sheet_capacity" {
		t.Errorf("a count of 18 digits = %s, want sheet_capacity", outcome)
	}

	// The schema says of each of these fields what it cannot check.
	listed, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Description string `json:"description"`
		Defs        struct {
			Cell      struct{ OneOf []json.RawMessage }
			Format    json.RawMessage
			Operation struct {
				OneOf []struct {
					Properties map[string]struct {
						Type        any
						Enum        []string
						Description string
					}
				}
			}
		} `json:"$defs"`
	}
	for _, tool := range listed.Tools {
		if tool.Name == "update_sheet" {
			raw, _ := json.Marshal(tool.InputSchema)
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
		}
	}
	const rule = "not 2.0, 2e0 or -0, and at most 18 digits"
	whole := 0
	for _, op := range schema.Defs.Operation.OneOf {
		for name, field := range op.Properties {
			if field.Type == "integer" {
				whole++
				if !strings.Contains(field.Description, rule) {
					t.Errorf("the schema of %s in %v does not say how a whole number is written: %q", name, op.Properties["op"].Enum, field.Description)
				}
			}
		}
	}
	if whole != 11 {
		t.Errorf("the operations of the schema hold %d whole numbers, want the 11 of section 18.2", whole)
	}
	if !strings.Contains(string(schema.Defs.Format), rule) {
		t.Error("the schema of decimals does not say how a whole number is written")
	}
	if !strings.Contains(schema.Description, "UTF-8") {
		t.Errorf("the schema does not name the encoding: %q", schema.Description)
	}
	guide := strings.Join(strings.Fields(sheetGuide(t, cs)), " ")
	for _, said := range []string{
		"`value` (a number, a string, a boolean or `null`) and `error` (a string)",
		"`2`, not `2.0`, `2e0` or `\"2\"`, and `0`, not `-0`", "at most 18 digits", "takes a whole number written in plain digits",
		"`1e999` is `invalid`", "The call is UTF-8", "reason `json`", "U+FFFD",
	} {
		if !strings.Contains(guide, said) {
			t.Errorf("the guide does not say %q", said)
		}
	}

	// A read takes its page size by its value, as its schema does.
	var page map[string]any
	if text, bad := call(t, cs, "get_sheet", map[string]any{"slug": "edges", "tab": "Sheet1", "limit": json.RawMessage("2.0")}, &page); bad {
		t.Errorf("get_sheet with limit 2.0: %s", text)
	}

	// A call whose bytes are not UTF-8 is refused whole, with reason json.
	before := state()
	bad := "{\"slug\":\"edges\",\"ops\":[{\"op\":\"add_tab\",\"name\":\"a\xffb\"}]}"
	if res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "update_sheet", Arguments: json.RawMessage(bad)}); err == nil && !res.IsError {
		t.Error("a call that is not UTF-8 was applied")
	}
	if state() != before {
		t.Fatal("a call that is not UTF-8 changed the sheet")
	}
	var refusal *sheet.WriteError
	if _, err := sheet.DecodeUpdate([]byte(bad)); !errors.As(err, &refusal) || refusal.Code != "invalid" || refusal.Reason != "json" {
		t.Errorf("the reader takes a call that is not UTF-8 as %v, want invalid json", err)
	}
	// Half of a surrogate pair is stored as the replacement character.
	if outcome, reason, detail := send(write(`"a\ud800b"`)); outcome != "applied" {
		t.Fatalf("a lone surrogate = %s %s: %s", outcome, reason, detail)
	}
	var read map[string]any
	call(t, cs, "get_sheet", map[string]any{"slug": "edges", "tab": "Sheet1", "range": "A1"}, &read)
	if got := read["rows"].([]any)[0].([]any)[0]; got != "a�b" {
		t.Errorf("a lone surrogate is stored as %q, want U+FFFD in its place", got)
	}
}

// TestSheetUpdateSchemaTakesEmptyStateAndOpID sends calls whose state or
// op_id is empty, which the reader takes as left out. The published schema
// takes them too, and each call gets what the same call without the field
// gets: an empty state is enough only for a call that adds tabs, and an
// empty op_id protects no resend. An op_id of 128 characters is the
// longest that either takes.
func TestSheetUpdateSchemaTakesEmptyStateAndOpID(t *testing.T) {
	e := newEnv(t)
	cs := e.local
	if text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "edges", "ops": []any{}}, nil); bad {
		t.Fatalf("create: %s", text)
	}
	// send reports "applied", "schema" for arguments the input schema did
	// not pass, or the reason of the sheet's refusal.
	send := func(args string) string {
		t.Helper()
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "update_sheet", Arguments: json.RawMessage(args)})
		if err != nil {
			return "schema"
		}
		if !res.IsError {
			return "applied"
		}
		raw, _ := json.Marshal(res.Content)
		if strings.Contains(string(raw), `validating \"arguments\"`) {
			return "schema"
		}
		for _, reason := range []string{"state_required", "op_id"} {
			if strings.Contains(string(raw), reason) {
				return reason
			}
		}
		return string(raw)
	}
	revisions := func() int {
		t.Helper()
		var out map[string]any
		if text, bad := call(t, cs, "list_revisions", map[string]any{"slug": "edges"}, &out); bad {
			t.Fatalf("list_revisions: %s", text)
		}
		return len(out["revisions"].([]any))
	}
	write := `{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]]}`
	for _, c := range []struct{ args, want string }{
		{`{"slug":"edges","state":"","ops":[{"op":"add_tab","name":"Aside"}]}`, "applied"},
		{`{"slug":"edges","state":"","ops":[` + write + `]}`, "state_required"},
		{`{"slug":"edges","ops":[` + write + `]}`, "state_required"},
		{`{"slug":"edges","op_id":"","ops":[{"op":"add_tab","name":"Third"}]}`, "applied"},
		{`{"slug":"edges","op_id":"` + strings.Repeat("a", 128) + `","ops":[{"op":"add_tab","name":"Fourth"}]}`, "applied"},
		{`{"slug":"edges","op_id":"` + strings.Repeat("a", 129) + `","ops":[{"op":"add_tab","name":"Fifth"}]}`, "schema"},
		{`{"slug":"edges","op_id":"has space","ops":[{"op":"add_tab","name":"Fifth"}]}`, "schema"},
	} {
		if got := send(c.args); got != c.want {
			t.Errorf("%.120s = %s, want %s", c.args, got, c.want)
		}
		if c.want != "schema" {
			continue
		}
		u, err := sheet.DecodeUpdate([]byte(c.args))
		var refusal *sheet.WriteError
		if !errors.As(err, &refusal) || refusal.Reason != "op_id" {
			t.Errorf("the reader takes %.120s as %+v, %v, want invalid op_id", c.args, u.OpID, err)
		}
	}
	// An empty op_id is no op_id: the same call sent again applies again.
	before := revisions()
	resend := `{"slug":"edges","op_id":"","ops":[{"op":"add_tab","name":"Again"}]}`
	if got := send(resend); got != "applied" {
		t.Fatalf("a call with an empty op_id = %s", got)
	}
	if got := send(resend); got == "applied" {
		t.Fatal("the same tab was added twice")
	}
	if n := revisions(); n != before+1 {
		t.Fatalf("%d revisions more, want the one of the first send", n-before)
	}
}

// TestSheetReadSchemaAgreesOnInclude sends get_sheet and get_revision reads
// with an empty and a filled include list, as an agent does. The published
// schema refuses the empty list, as the reader does, and the reader's
// refusal is checked beside it. A read the schema passes is answered.
func TestSheetReadSchemaAgreesOnInclude(t *testing.T) {
	e := newEnv(t)
	cs := e.local
	if text, bad := call(t, cs, "update_sheet", map[string]any{"slug": "budget", "ops": []any{map[string]any{"op": "write", "tab": "Sheet1", "at": "A1", "rows": []any{[]any{1}}}}}, nil); bad {
		t.Fatalf("create: %s", text)
	}
	// send reports "answered", "schema" for arguments the input schema did
	// not pass, or the text of the sheet's refusal.
	send := func(tool, args string) string {
		t.Helper()
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: json.RawMessage(args)})
		if err != nil {
			return "schema"
		}
		raw, _ := json.Marshal(res.Content)
		if !res.IsError {
			return "answered"
		}
		if strings.Contains(string(raw), `validating \"arguments\"`) {
			return "schema"
		}
		return string(raw)
	}
	for _, tool := range []struct{ name, args string }{
		{"get_sheet", `{"slug":"budget","tab":"Sheet1"`},
		{"get_revision", `{"slug":"budget","revision":1,"tab":"Sheet1"`},
	} {
		if got := send(tool.name, tool.args+`,"include":[]}`); got != "schema" {
			t.Errorf("%s with an empty include: %s, want a schema refusal", tool.name, got)
		}
		if got := send(tool.name, tool.args+`,"include":["shown"]}`); got != "answered" {
			t.Errorf("%s with include shown: %s", tool.name, got)
		}
	}
	// Behind the schema, the readers of both tools refuse the empty list
	// too, since get_revision passes it on as given, not as absent.
	if _, _, _, err := sheetReadQuery(map[string]json.RawMessage{"slug": json.RawMessage(`"budget"`), "tab": json.RawMessage(`"Sheet1"`), "include": json.RawMessage(`[]`)}); err == nil {
		t.Error("get_sheet's reader takes an empty include")
	}
	var in RevisionIn
	if err := json.Unmarshal([]byte(`{"slug":"budget","revision":1,"tab":"Sheet1","include":[]}`), &in); err != nil {
		t.Fatal(err)
	}
	query, selected, err := (&tools{}).sheetRevisionQuery(t.Context(), in)
	if err != nil || !selected {
		t.Fatalf("get_revision's selectors: %v, %v", selected, err)
	}
	if _, _, _, err := sheetReadQuery(query); err == nil {
		t.Errorf("get_revision's reader takes an empty include as %s", query["include"])
	}
}
