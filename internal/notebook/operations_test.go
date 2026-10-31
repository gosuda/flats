package notebook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func operationFixtures(t testing.TB, browser bool) []json.RawMessage {
	t.Helper()
	name := "agent"
	if browser {
		name = "browser"
	}
	data, err := os.ReadFile("testdata/operations_" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var ops []json.RawMessage
	if err := json.Unmarshal(data, &ops); err != nil {
		t.Fatal(err)
	}
	return ops
}

func operationFieldsForTest(t testing.TB, data []byte) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func operationJSONForTest(t testing.TB, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOperationVariants(t *testing.T) {
	t.Parallel()
	for _, browser := range []bool{false, true} {
		ops := operationFixtures(t, browser)
		if len(ops) != 30 {
			t.Fatalf("got %d operation variants, want 30", len(ops))
		}
		seen := make(map[string]bool)
		for _, raw := range ops {
			fields := operationFieldsForTest(t, raw)
			name := string(fields["op"])
			if seen[name] {
				t.Fatalf("duplicate fixture %s", name)
			}
			seen[name] = true
			t.Run(fmt.Sprintf("browser=%v/%s", browser, name), func(t *testing.T) {
				op, err := DecodeOperation(raw, browser)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := EncodeOperation(op, browser)
				if err != nil {
					t.Fatal(err)
				}
				redecoded, err := DecodeOperation(encoded, browser)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(op, redecoded) {
					t.Fatalf("round trip changed operation: %#v / %#v", op, redecoded)
				}
				out := operationFieldsForTest(t, encoded)
				for _, field := range []string{"op", "proposed_id", "block_proposed_id", "title_property_proposed_id", "view_proposed_id", "proposed_ids"} {
					if !bytes.Equal(compactOperationJSON(t, fields[field]), compactOperationJSON(t, out[field])) {
						t.Fatalf("round trip changed %s", field)
					}
				}
				fields["unrecognized"] = json.RawMessage(`true`)
				if _, err := DecodeOperation(operationJSONForTest(t, fields), browser); err == nil {
					t.Fatal("accepted unknown field")
				}
			})
		}
	}
}

func compactOperationJSON(t testing.TB, raw []byte) []byte {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestOperationRequiredFields(t *testing.T) {
	t.Parallel()
	for _, browser := range []bool{false, true} {
		for _, raw := range operationFixtures(t, browser) {
			fields := operationFieldsForTest(t, raw)
			name := string(fields["op"])
			for key := range fields {
				t.Run(fmt.Sprintf("browser=%v/%s/%s", browser, name, key), func(t *testing.T) {
					copy := operationFieldsForTest(t, raw)
					copy[key] = json.RawMessage(`null`)
					if key != "parent" && key != "before" {
						if _, err := DecodeOperation(operationJSONForTest(t, copy), browser); err == nil {
							t.Fatal("accepted null")
						}
					}
					delete(copy, key)
					optional := key == "confirmed_preview" || key == "title_property_name" || key == "asset_refs" || key == "view" || (key == "base_v" && !browser)
					if optional {
						return
					}
					if _, err := DecodeOperation(operationJSONForTest(t, copy), browser); err == nil {
						t.Fatal("accepted missing field")
					}
				})
			}
		}
	}
}

const operationPage = `{"id":"p_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"42"}`
const operationBlock = `{"id":"b_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"42"}`
const operationDatabase = `{"id":"d_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"42"}`
const operationProperty = `{"id":"pr_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"42"}`
const operationOption = `{"id":"o_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"42"}`
const operationRow = `{"id":"r_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"42"}`
const operationView = `{"id":"v_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"42"}`
const operationPlace = `{"page":` + operationPage + `,"parent":null,"before":null}`
const operationParagraph = `{"kind":"paragraph","text":[]}`

func TestOperationRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, data string
		browser    bool
	}{
		{"unknown op", `{"op":"invent_database"}`, false},
		{"null op", `null`, false},
		{"wrong page kind", `{"op":"delete_page","page":` + operationBlock + `}`, false},
		{"bare id", `{"op":"delete_page","page":"p_aaaaaaaaaaaaaaaaaaaaaaaaaa"}`, false},
		{"missing inc", `{"op":"delete_page","page":{"id":"p_aaaaaaaaaaaaaaaaaaaaaaaaaa"}}`, false},
		{"bad inc", `{"op":"delete_page","page":{"id":"p_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"01"}}`, false},
		{"numeric inc", `{"op":"delete_page","page":{"id":"p_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":42}}`, false},
		{"unknown target field", `{"op":"delete_page","page":{"id":"p_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"42","unknown":0}}`, false},
		{"reference and tmp", `{"op":"delete_page","page":{"id":"p_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"42","tmp":"x"}}`, false},
		{"zero agent inc", `{"op":"delete_page","page":{"id":"p_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"0"}}`, false},
		{"metadata agent", `{"op":"delete_page","page":` + operationPage + `,"unit_id":"u"}`, false},
		{"structure agent", `{"op":"delete_page","page":` + operationPage + `,"structure":[]}`, false},
		{"structure without unit", `{"op":"delete_page","page":` + operationPage + `,"structure":[]}`, true},
		{"empty unit", `{"op":"delete_page","page":` + operationPage + `,"unit_id":""}`, true},
		{"nonascii unit", `{"op":"delete_page","page":` + operationPage + `,"unit_id":"한글"}`, true},
		{"duplicate field", `{"op":"delete_page","page":` + operationPage + `,"page":` + operationPage + `}`, false},
		{"case alias", `{"op":"delete_page","page":` + operationPage + `,"Page":` + operationPage + `}`, false},
		{"trailing JSON", `{"op":"delete_page","page":` + operationPage + `} {}`, false},
		{"browser base version missing", `{"op":"set_block","block":` + operationBlock + `,"content":` + operationParagraph + `}`, true},
		{"unknown content field", `{"op":"set_block","block":` + operationBlock + `,"content":{"kind":"paragraph","text":[],"checked":true}}`, false},
		{"create database via block", `{"op":"create_block","ref":"b","place":` + operationPlace + `,"content":{"kind":"database","database":"d_aaaaaaaaaaaaaaaaaaaaaaaaaa"}}`, false},
		{"bad conversion", `{"op":"turn_blocks","blocks":[` + operationBlock + `],"kind":"database"}`, false},
		{"empty selection", `{"op":"delete_blocks","blocks":[]}`, false},
		{"null selection member", `{"op":"delete_blocks","blocks":[null]}`, false},
		{"duplicate selection", `{"op":"delete_blocks","blocks":[` + operationBlock + `,` + operationBlock + `]}`, false},
		{"page after not allowed", `{"op":"move_page","page":` + operationPage + `,"parent":null,"after":` + operationPage + `}`, false},
		{"placement missing anchor", `{"op":"move_blocks","blocks":[` + operationBlock + `],"place":{"page":` + operationPage + `,"parent":null}}`, false},
		{"placement both anchors", `{"op":"move_blocks","blocks":[` + operationBlock + `],"place":{"page":` + operationPage + `,"parent":null,"before":null,"after":` + operationBlock + `}}`, false},
		{"placement null after", `{"op":"move_blocks","blocks":[` + operationBlock + `],"place":{"page":` + operationPage + `,"parent":null,"after":null}}`, false},
		{"missing placement parent", `{"op":"move_blocks","blocks":[` + operationBlock + `],"place":{"page":` + operationPage + `,"before":null}}`, false},
		{"wrong anchor kind", `{"op":"move_rows","rows":[` + operationRow + `],"before":` + operationBlock + `}`, false},
		{"invalid color", `{"op":"set_option","option":` + operationOption + `,"name":"x","color":"cyan"}`, false},
		{"malformed rich URL", `{"op":"set_block","block":` + operationBlock + `,"content":{"kind":"paragraph","text":[{"text":"x","marks":[],"link":{"url":"javascript:alert(1)"}}]}}`, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodeOperation([]byte(tt.data), tt.browser); err == nil {
				t.Fatalf("accepted %s", tt.data)
			}
		})
	}
}

func TestBrowserCreates(t *testing.T) {
	t.Parallel()
	for _, raw := range operationFixtures(t, true) {
		fields := operationFieldsForTest(t, raw)
		var name string
		if err := json.Unmarshal(fields["op"], &name); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(name, "create_") && name != "markdown_to_blocks" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeOperation(raw, false); err == nil {
				t.Fatal("agent accepted browser proposed identities")
			}
			for _, field := range []string{"proposed_id", "block_proposed_id", "title_property_proposed_id", "view_proposed_id"} {
				if _, ok := fields[field]; !ok {
					continue
				}
				copy := operationFieldsForTest(t, raw)
				copy[field] = json.RawMessage(`"o_aaaaaaaaaaaaaaaaaaaaaaaaaa"`)
				if name == "create_option" {
					copy[field] = json.RawMessage(`"p_aaaaaaaaaaaaaaaaaaaaaaaaaa"`)
				}
				if _, err := DecodeOperation(operationJSONForTest(t, copy), true); err == nil {
					t.Fatalf("accepted wrong-kind %s", field)
				}
			}
		})
	}
	for _, name := range []string{"duplicate_page", "duplicate_blocks"} {
		for _, raw := range operationFixtures(t, true) {
			fields := operationFieldsForTest(t, raw)
			if string(fields["op"]) != `"`+name+`"` {
				continue
			}
			fields["proposed_id"] = json.RawMessage(`"p_aaaaaaaaaaaaaaaaaaaaaaaaaa"`)
			if _, err := DecodeOperation(operationJSONForTest(t, fields), true); err == nil {
				t.Fatal("duplicate accepted proposed id")
			}
		}
	}
	data := `{"op":"markdown_to_blocks","place":` + operationPlace + `,"markdown":"","proposed_ids":[]}`
	op, err := DecodeOperation([]byte(data), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeOperation(op, true); err != nil {
		t.Fatal(err)
	}
}

func TestCellInputs(t *testing.T) {
	t.Parallel()
	for _, value := range []string{`null`, `""`, `"1e5"`, `"not a number"`, `"javascript:literal text"`, `true`, `false`, operationOption, `[]`, `[` + operationOption + `]`, `{"tmp":"option"}`} {
		t.Run("valid/"+value, func(t *testing.T) {
			data := `{"op":"set_cell","row":` + operationRow + `,"cell":{"property":` + operationProperty + `,"value":` + value + `}}`
			op, err := DecodeOperation([]byte(data), false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := EncodeOperation(op, false); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, value := range []string{`1`, `1e9999`, `NaN`, `Infinity`, `{}`, `{"id":"o_aaaaaaaaaaaaaaaaaaaaaaaaaa"}`, `{"tmp":""}`, operationProperty, `{"id":"o_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"0"}`, `[` + operationOption + `,` + operationOption + `]`, `[null]`, `["o_aaaaaaaaaaaaaaaaaaaaaaaaaa"]`, `{"type":"number","value":"NaN"}`} {
		t.Run("invalid/"+value, func(t *testing.T) {
			data := `{"op":"set_cell","row":` + operationRow + `,"cell":{"property":` + operationProperty + `,"value":` + value + `}}`
			if _, err := DecodeOperation([]byte(data), false); err == nil {
				t.Fatal("accepted invalid cell input")
			}
		})
	}
	for _, cell := range []string{`{}`, `{"property":` + operationProperty + `}`, `{"value":null}`, `{"property":null,"value":null}`, `{"property":` + operationProperty + `,"value":null,"unknown":true}`} {
		if _, err := DecodeOperation([]byte(`{"op":"set_cell","row":`+operationRow+`,"cell":`+cell+`}`), false); err == nil {
			t.Fatalf("accepted invalid cell %s", cell)
		}
	}
}

func TestOperationEnvelopes(t *testing.T) {
	t.Parallel()
	for _, browser := range []bool{false, true} {
		raw := operationFixtures(t, browser)
		if browser {
			data := operationJSONForTest(t, map[string]any{"n": "1", "action_id": "edit-1", "typing_block": json.RawMessage(operationBlock), "close": true, "ops": raw})
			action, err := DecodeAction(data)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := EncodeAction(action)
			if err != nil {
				t.Fatal(err)
			}
			again, err := DecodeAction(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(action, again) {
				t.Fatal("action round trip changed")
			}
		} else {
			data := operationJSONForTest(t, map[string]any{"slug": "notes", "op_id": "test-1", "message": "write", "guards": map[string]any{"state": map[string]string{"store": "store", "seq": "41"}, "items": []any{map[string]any{"ref": json.RawMessage(operationPage), "ver": "40"}}}, "assets": []any{map[string]string{"ref": "image", "data": "YWJj"}}, "ops": raw})
			update, err := DecodeUpdate(data)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := EncodeUpdate(update)
			if err != nil {
				t.Fatal(err)
			}
			again, err := DecodeUpdate(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(update, again) {
				t.Fatal("update round trip changed")
			}
		}
	}
}

func TestTemporaryNamesAndUnits(t *testing.T) {
	t.Parallel()
	create := `{"op":"create_page","ref":"page","parent":null,"before":null,"title":""}`
	retitle := `{"op":"set_page_title","page":{"tmp":"page"},"title":"changed"}`
	db := `{"op":"create_database","ref":"db","block_ref":"owner","place":` + operationPlace + `,"title":""}`
	deleteTitle := `{"op":"delete_property","property":{"tmp":"db.title"}}`
	setView := `{"op":"delete_view","view":{"tmp":"db.view"}}`
	update := func(ops string) []byte { return []byte(`{"slug":"notes","op_id":"test","ops":[` + ops + `]}`) }
	if _, err := DecodeUpdate(update(create + `,` + retitle)); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeUpdate(update(db + `,` + deleteTitle + `,` + setView)); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, ops string }{
		{"forward", retitle + `,` + create},
		{"duplicate", create + `,` + create},
		{"wrong temporary kind", create + `,{"op":"delete_blocks","blocks":[{"tmp":"page"}]}`},
		{"reserved title", db + `,{"op":"create_page","ref":"db.title","parent":null,"before":null,"title":""}`},
		{"reserved view", db + `,{"op":"create_page","ref":"db.view","parent":null,"before":null,"title":""}`},
		{"db duplicate local names", strings.Replace(db, `"block_ref":"owner"`, `"block_ref":"db.title"`, 1)},
		{"self reference", `{"op":"create_page","ref":"page","parent":{"tmp":"page"},"before":null,"title":""}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodeUpdate(update(tt.ops)); err == nil {
				t.Fatal("accepted malformed temporary names")
			}
		})
	}
	plain := `{"op":"set_page_title","page":` + operationPage + `,"title":""}`
	unit := func(name string) string { return strings.TrimSuffix(plain, `}`) + `,"unit_id":"` + name + `"}` }
	structure := strings.TrimSuffix(unit("a"), `}`) + `,"structure":[{"block":` + operationBlock + `,"page":` + operationPage + `,"parent":null}]}`
	action := func(ops string) []byte { return []byte(`{"n":"1","action_id":"x","ops":[` + ops + `]}`) }
	if _, err := DecodeAction(action(structure + `,` + unit("a") + `,` + plain + `,` + unit("b"))); err != nil {
		t.Fatal(err)
	}
	for _, ops := range []string{unit("a") + `,` + plain + `,` + unit("a"), unit("a") + `,` + unit("b") + `,` + unit("a"), unit("a") + `,` + structure} {
		if _, err := DecodeAction(action(ops)); err == nil {
			t.Fatal("accepted malformed compound envelope")
		}
	}
	browserCreate := strings.TrimSuffix(create, `}`) + `,"proposed_id":"p_aaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	if _, err := DecodeAction(action(browserCreate + `,` + strings.Replace(browserCreate, `"ref":"page"`, `"ref":"other"`, 1))); err == nil {
		t.Fatal("accepted reused proposed identity")
	}
}

func TestOperationDecodersRetainOverLimitText(t *testing.T) {
	t.Parallel()
	for _, browser := range []bool{false, true} {
		for _, raw := range operationFixtures(t, browser) {
			fields := operationFieldsForTest(t, raw)
			if string(fields["op"]) != `"set_page_title"` && string(fields["op"]) != `"set_block"` &&
				string(fields["op"]) != `"set_cell"` {
				continue
			}
			t.Run(fmt.Sprintf("browser=%t/%s", browser, fields["op"]), func(t *testing.T) {
				for _, n := range []int{8001, 8500, 9000} {
					text := strings.Repeat("😀", n)
					switch string(fields["op"]) {
					case `"set_page_title"`:
						fields["title"] = operationJSONForTest(t, text)
					case `"set_block"`:
						fields["content"] = operationJSONForTest(t, map[string]any{
							"kind": "paragraph", "text": []any{map[string]any{"text": text, "marks": []any{}}},
						})
					case `"set_cell"`:
						cell := operationFieldsForTest(t, fields["cell"])
						cell["value"] = operationJSONForTest(t, text)
						fields["cell"] = operationJSONForTest(t, cell)
					}
					data := operationJSONForTest(t, fields)
					op, err := DecodeOperation(data, browser)
					if err != nil {
						t.Fatal(err)
					}
					encoded, err := json.Marshal(op)
					if err != nil || !bytes.Contains(encoded, []byte(text)) {
						t.Fatalf("operation lost retained text: %v", err)
					}
				}
			})
		}
	}
}

func TestNestedViewReferences(t *testing.T) {
	t.Parallel()
	view := func(filter string) string {
		return `{"op":"set_view","view":` + operationView + `,"definition":{"name":"Table","display":{"layout":"table","visible":[],"widths":[]},"sort":[],"filter":[` + filter + `]}}`
	}
	for _, filter := range []string{
		`{"property":` + operationProperty + `,"op":"is","value":{"id":"o_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"0"}}`,
		`{"property":` + operationProperty + `,"op":"is any of","value":[{"id":"o_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"0"}]}`,
		`{"property":` + operationProperty + `,"op":"=","value":"NaN"}`,
		`{"property":` + operationProperty + `,"op":"<","value":"1e99999999999999999999"}`,
		`{"property":` + operationProperty + `,"op":"before","value":"2026-02-30"}`,
	} {
		if _, err := DecodeOperation([]byte(view(filter)), false); err == nil {
			t.Fatalf("accepted invalid filter %s", filter)
		}
	}
	filter := `{"property":` + operationProperty + `,"op":"is","value":{"tmp":"future"}}`
	data := `{"slug":"notes","op_id":"x","ops":[` + view(filter) + `]}`
	if _, err := DecodeUpdate([]byte(data)); err == nil {
		t.Fatal("accepted forward temporary option in filter")
	}
}

func TestEnvelopeRefusals(t *testing.T) {
	t.Parallel()
	plain := `{"op":"delete_page","page":` + operationPage + `}`
	for _, data := range []string{
		`{}`, `null`, `{"slug":"x","op_id":"x","ops":null}`,
		`{"slug":"x","op_id":"bad id","ops":[` + plain + `]}`,
		`{"slug":"x","op_id":"x","ops":[` + plain + `],"message":null}`,
		`{"slug":"x","op_id":"x","ops":[` + plain + `],"unknown":1}`,
		`{"slug":"x","op_id":"x","ops":[` + plain + `],"guards":{"state":{"store":"s"}}}`,
		`{"slug":"x","op_id":"x","ops":[` + plain + `],"guards":{"items":[{"ref":{"id":"p_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"0"},"ver":"1"}]}}`,
		`{"slug":"x","op_id":"x","ops":[` + plain + `],"assets":[{"ref":"x","data":"bad!"}]}`,
	} {
		if _, err := DecodeUpdate([]byte(data)); err == nil {
			t.Fatalf("accepted malformed update %s", data)
		}
	}
	for _, data := range []string{
		`{}`, `null`, `{"n":"0","action_id":"x","ops":[` + plain + `]}`,
		`{"n":1,"action_id":"x","ops":[` + plain + `]}`,
		`{"n":"1","action_id":"","ops":[` + plain + `]}`,
		`{"n":"1","action_id":"x","close":null,"ops":[` + plain + `]}`,
		`{"n":"1","action_id":"x","typing_block":` + operationPage + `,"ops":[` + plain + `]}`,
	} {
		if _, err := DecodeAction([]byte(data)); err == nil {
			t.Fatalf("accepted malformed action %s", data)
		}
	}
	tooMany := strings.Repeat(plain+`,`, MaxOperations) + plain
	if _, err := DecodeUpdate([]byte(`{"slug":"x","op_id":"x","ops":[` + tooMany + `]}`)); err == nil {
		t.Fatal("accepted too many explicit operations")
	}
}

func TestBrowserZeroIncarnations(t *testing.T) {
	t.Parallel()
	for _, data := range []string{
		`{"op":"delete_page","page":{"id":"p_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"0"}}`,
		`{"op":"set_cell","row":` + operationRow + `,"cell":{"property":` + operationProperty + `,"value":{"id":"o_aaaaaaaaaaaaaaaaaaaaaaaaaa","inc":"0"}}}`,
	} {
		if _, err := DecodeOperation([]byte(data), true); err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeOperation([]byte(data), false); err == nil {
			t.Fatal("agent accepted zero incarnation")
		}
	}
}

type operationSpoof struct {
	OperationMeta
	name string
}

func (o operationSpoof) OperationName() string { return o.name }

func TestOperationEncoding(t *testing.T) {
	t.Parallel()
	for _, op := range []Operation{
		nil,
		(*DeletePageOp)(nil),
		operationSpoof{name: "delete_page"},
		operationSpoof{name: "unknown"},
		SetPageTitleOp{Page: Target{Ref: &Ref{ID: "p_aaaaaaaaaaaaaaaaaaaaaaaaaa", Inc: 1}}, Title: string([]byte{0xff})},
		MarkdownToBlocksOp{ProposedIDs: []ProposedItem{}},
	} {
		if _, err := EncodeOperation(op, false); err == nil {
			t.Fatalf("accepted invalid in-memory operation %T", op)
		}
	}
	for _, value := range []CellInputValue{{}, {Null: true, Bool: new(bool)}} {
		if _, err := json.Marshal(value); err == nil {
			t.Fatal("accepted invalid cell union")
		}
	}
	after := `{"op":"move_blocks","blocks":[` + operationBlock + `],"place":{"page":` + operationPage + `,"parent":null,"after":` + operationBlock + `}}`
	op, err := DecodeOperation([]byte(after), false)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeOperation(op, false)
	if err != nil {
		t.Fatal(err)
	}
	fields := operationFieldsForTest(t, encoded)
	place := operationFieldsForTest(t, fields["place"])
	if _, exists := place["before"]; exists {
		t.Fatal("after placement emitted before")
	}
	if _, exists := place["after"]; !exists {
		t.Fatal("after anchor was lost")
	}
	closeAction, err := DecodeAction([]byte(`{"n":"1","action_id":"typing","close":true,"ops":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeAction(closeAction); err != nil {
		t.Fatal(err)
	}
}

func FuzzOperationDecoders(f *testing.F) {
	for _, browser := range []bool{false, true} {
		for _, raw := range operationFixtures(f, browser) {
			f.Add([]byte(raw), browser)
		}
	}
	f.Add(operationJSONForTest(f, map[string]any{"slug": "notes", "op_id": "fuzz", "ops": operationFixtures(f, false)}), false)
	f.Add(operationJSONForTest(f, map[string]any{"n": "1", "action_id": "fuzz", "ops": operationFixtures(f, true)}), true)
	f.Add([]byte(`null`), false)
	f.Fuzz(func(t *testing.T, data []byte, browser bool) {
		op, err := DecodeOperation(data, browser)
		if err == nil {
			encoded, err := EncodeOperation(op, browser)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeOperation(encoded, browser)
			if err != nil {
				t.Fatal(err)
			}
			again, err := EncodeOperation(decoded, browser)
			if err != nil || !bytes.Equal(encoded, again) {
				t.Fatalf("unstable operation: %s -> %s: %v", encoded, again, err)
			}
		}
		if update, err := DecodeUpdate(data); err == nil {
			if _, err := EncodeUpdate(update); err != nil {
				t.Fatal(err)
			}
		}
		if action, err := DecodeAction(data); err == nil {
			if _, err := EncodeAction(action); err != nil {
				t.Fatal(err)
			}
		}
	})
}
