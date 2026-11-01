package mcpx

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func notebookResolveSchema(t *testing.T, schema map[string]any) *jsonschema.Resolved {
	t.Helper()
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func notebookSchemaSample(s map[string]any, defs map[string]any) any {
	if ref, ok := s["$ref"].(string); ok {
		return notebookSchemaSample(defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any), defs)
	}
	if alternatives, ok := s["oneOf"].([]map[string]any); ok {
		return notebookSchemaSample(alternatives[0], defs)
	}
	if values, ok := s["enum"].([]string); ok {
		return values[0]
	}
	switch s["type"] {
	case "object":
		v := map[string]any{}
		for _, key := range s["required"].([]string) {
			v[key] = notebookSchemaSample(s["properties"].(map[string]any)[key].(map[string]any), defs)
		}
		return v
	case "array":
		return []any{}
	case "boolean":
		return false
	case "integer":
		if n, ok := s["minimum"].(int); ok {
			return n
		}
		return 1
	case "null":
		return nil
	case "string":
		pattern, _ := s["pattern"].(string)
		if strings.Contains(pattern, "{25}") {
			prefix := strings.SplitN(strings.TrimPrefix(pattern, "^"), "_", 2)[0]
			if strings.HasPrefix(prefix, "(") {
				prefix = "p"
			}
			return prefix + "_" + strings.Repeat("a", 26)
		}
		switch pattern {
		case "^[0-9a-f]{64}$":
			return strings.Repeat("0", 64)
		case "^[1-9][0-9]*$", "^(0|[1-9][0-9]*)$":
			return "1"
		case `^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?$`:
			return "1.25"
		case `^[0-9]{4}-[0-9]{2}-[0-9]{2}(T[0-9]{2}:[0-9]{2}:[0-9]{2})?$`:
			return "2026-10-30"
		}
		return "x"
	}
	panic("unknown schema")
}

func TestNotebookSchemaOperations(t *testing.T) {
	schema := notebookUpdateSchema()
	resolved := notebookResolveSchema(t, schema)
	defs := schema["$defs"].(map[string]any)
	variants := schema["properties"].(map[string]any)["ops"].(map[string]any)["items"].(map[string]any)["oneOf"].([]map[string]any)
	if len(variants) != 30 {
		t.Fatalf("got %d operations", len(variants))
	}
	for _, variant := range variants {
		op := notebookSchemaSample(variant, defs).(map[string]any)
		t.Run(op["op"].(string), func(t *testing.T) {
			request := map[string]any{"slug": "notes", "op_id": "test-1", "ops": []any{op}}
			if err := resolved.Validate(request); err != nil {
				t.Fatal(err)
			}
			for _, key := range variant["required"].([]string) {
				value := op[key]
				delete(op, key)
				if resolved.Validate(request) == nil {
					t.Errorf("accepted missing %s", key)
				}
				op[key] = value
			}
			for _, key := range []string{"unknown", "unit_id", "structure", "proposed_id", "block_proposed_id", "title_property_proposed_id", "view_proposed_id", "proposed_ids"} {
				op[key] = "x"
				if resolved.Validate(request) == nil {
					t.Errorf("accepted %s", key)
				}
				delete(op, key)
			}
		})
	}
}

func TestNotebookSchemaRead(t *testing.T) {
	resolved := notebookResolveSchema(t, notebookReadSchema())
	id := "p_" + strings.Repeat("a", 26)
	database := "d_" + strings.Repeat("a", 26)
	view := "v_" + strings.Repeat("a", 26)
	cases := []struct {
		raw   string
		valid bool
	}{
		{`{"slug":"notes"}`, true},
		{`{"slug":"notes","revision":1}`, true},
		{`{"slug":"notes","cursor":"next"}`, true},
		{`{"slug":"notes","op_id":"write-1","cursor":"next"}`, true},
		{`{"slug":"notes","cursor":"next","revision":1}`, false},
		{`{"slug":"notes","cursor":"next","limit":10}`, false},
		{`{"slug":"notes","op_id":"write-1"}`, false},
		{`{"slug":"notes","limit":2001}`, false},
		{`{"slug":"notes","revision":0}`, false},
		{`{"slug":"notes","page":"` + id + `","format":"markdown"}`, true},
		{`{"slug":"notes","page":{"id":"` + id + `","inc":"1"}}`, true},
		{`{"slug":"notes","page":{"id":"` + id + `","inc":"0"}}`, false},
		{`{"slug":"notes","page":"` + database + `"}`, false},
		{`{"slug":"notes","database":"` + database + `","sort":[],"filter":[]}`, true},
		{`{"slug":"notes","database":"` + database + `","view":"` + view + `"}`, true},
		{`{"slug":"notes","database":"` + database + `","view":"` + view + `","sort":[]}`, false},
		{`{"slug":"notes","database":"` + database + `","page":"` + id + `"}`, false},
		{`{"slug":"notes","sort":[]}`, false},
		{`{"slug":"notes","page":{"tmp":"new"}}`, false},
		{`{"slug":"notes","extra":true}`, false},
	}
	for _, c := range cases {
		var value any
		if err := json.Unmarshal([]byte(c.raw), &value); err != nil {
			t.Fatal(err)
		}
		err := resolved.Validate(value)
		if (err == nil) != c.valid {
			t.Errorf("valid=%v request=%s err=%v", c.valid, c.raw, err)
		}
	}
}

func TestNotebookSchemaClosedObjects(t *testing.T) {
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if value["type"] == "object" && value["additionalProperties"] != false {
				t.Fatal("open object")
			}
			for _, child := range value {
				walk(child)
			}
		case []map[string]any:
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(notebookReadSchema())
	walk(notebookUpdateSchema())
}
