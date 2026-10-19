// Package schema holds the JSON Schema of board data (v1.json, TECH
// Appendix A). Its test keeps the schema and the Go model in agreement.
package schema

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
)

// validator checks the subset of JSON Schema v1.json uses.
type validator struct{ defs map[string]any }

func (v *validator) check(s any, x any) error {
	switch s := s.(type) {
	case bool:
		if !s {
			return fmt.Errorf("not allowed")
		}
		return nil
	case map[string]any:
		return v.object(s, x)
	}
	return fmt.Errorf("bad schema %T", s)
}

func (v *validator) object(s map[string]any, x any) error {
	if r, ok := s["$ref"].(string); ok {
		return v.check(v.defs[strings.TrimPrefix(r, "#/$defs/")], x)
	}
	if c, ok := s["const"]; ok && !equal(c, x) {
		return fmt.Errorf("%v is not %v", x, c)
	}
	if e, ok := s["enum"].([]any); ok {
		found := false
		for _, c := range e {
			found = found || equal(c, x)
		}
		if !found {
			return fmt.Errorf("%v is not one of %v", x, e)
		}
	}
	if t, ok := s["type"].(string); ok && !hasType(t, x) {
		return fmt.Errorf("%v is not %s", x, t)
	}
	if alts, ok := s["oneOf"].([]any); ok {
		n := 0
		for _, a := range alts {
			if v.check(a, x) == nil {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("%d of oneOf match", n)
		}
	}
	if alts, ok := s["anyOf"].([]any); ok {
		ok := false
		for _, a := range alts {
			ok = ok || v.check(a, x) == nil
		}
		if !ok {
			return fmt.Errorf("no anyOf matches")
		}
	}
	switch x := x.(type) {
	case float64:
		if m, ok := s["minimum"].(float64); ok && x < m {
			return fmt.Errorf("%v < %v", x, m)
		}
		if m, ok := s["maximum"].(float64); ok && x > m {
			return fmt.Errorf("%v > %v", x, m)
		}
	case string:
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(x) {
			return fmt.Errorf("%q does not match %s", x, p)
		}
		if m, ok := s["maxLength"].(float64); ok && float64(utf8.RuneCountInString(x)) > m {
			return fmt.Errorf("too long")
		}
	case []any:
		if m, ok := s["minItems"].(float64); ok && float64(len(x)) < m {
			return fmt.Errorf("too few items")
		}
		if m, ok := s["maxItems"].(float64); ok && float64(len(x)) > m {
			return fmt.Errorf("too many items")
		}
		if it, ok := s["items"]; ok {
			for i, e := range x {
				if err := v.check(it, e); err != nil {
					return fmt.Errorf("item %d: %w", i, err)
				}
			}
		}
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, k := range req {
				if _, ok := x[k.(string)]; !ok {
					return fmt.Errorf("missing %s", k)
				}
			}
		}
		for k, e := range x {
			ps, ok := props[k]
			if !ok {
				ps, ok = s["additionalProperties"]
				if !ok {
					continue
				}
			}
			if err := v.check(ps, e); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	return nil
}

func hasType(t string, x any) bool {
	switch t {
	case "string":
		_, ok := x.(string)
		return ok
	case "number":
		_, ok := x.(float64)
		return ok
	case "integer":
		f, ok := x.(float64)
		return ok && f == math.Trunc(f)
	case "boolean":
		_, ok := x.(bool)
		return ok
	case "object":
		_, ok := x.(map[string]any)
		return ok
	case "array":
		_, ok := x.([]any)
		return ok
	case "null":
		return x == nil
	}
	return false
}

func equal(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func load(t *testing.T) *validator {
	t.Helper()
	raw, err := os.ReadFile("v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs map[string]any `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return &validator{defs: doc.Defs}
}

// modelAccepts is the server's verdict on a stored element.
func modelAccepts(raw string) error {
	el, _, err := model.DecodeElement([]byte(raw))
	if err != nil {
		return err
	}
	el.Normalize()
	if err := el.Validate(); err != nil {
		return err
	}
	if !model.ValidKey(el.Z) {
		return fmt.Errorf("z %q is not a stored key", el.Z)
	}
	return nil
}

const geo = `"x":10,"y":-20.5,"w":200,"h":100,"rot":0`

func TestSchemaMatchesModel(t *testing.T) {
	v := load(t)
	hash := strings.Repeat("ab", 32)
	samples := map[string]string{
		"sticky":            `{"id":"e_7Hk2pQ9xLm","type":"sticky",` + geo + `,"z":"a0V","text":"Onboarding","style":{"color":"yellow"},"by":"ts:a@b","at":1}`,
		"sticky font":       `{"id":"e_7Hk2pQ9xLm","type":"sticky",` + geo + `,"z":"V","style":{"fontSize":18}}`,
		"text":              `{"id":"e_aaaaaaaaaa","type":"text",` + geo + `,"z":"V","text":"t","style":{"color":"purple","fontSize":144,"align":"right"}}`,
		"shape":             `{"id":"e_aaaaaaaaaa","type":"shape",` + geo + `,"z":"V","text":"s","style":{"shape":"diamond","fill":"pink","strokeWidth":4,"dash":"dotted"}}`,
		"connector":         `{"id":"e_aaaaaaaaaa","type":"connector","z":"V","ends":[{"el":"e_bbbbbbbbbb","at":[1,0.5]},{"pt":[3,4]}],"style":{"route":"elbow","head0":"arrow"},"text":"label"}`,
		"pen":               `{"id":"e_aaaaaaaaaa","type":"pen",` + geo + `,"z":"V","pts":[0,0,10000,5000],"style":{"stroke":"highlight","width":"marker"}}`,
		"image":             `{"id":"e_aaaaaaaaaa","type":"image",` + geo + `,"z":"V","asset":"` + hash + `","alt":"a cat","natural":[640,480]}`,
		"frame":             `{"id":"e_aaaaaaaaaa","type":"frame",` + geo + `,"z":"V","text":"Ideas","style":{"fill":"gray"},"locked":true}`,
		"member":            `{"id":"e_aaaaaaaaaa","type":"sticky",` + geo + `,"z":"V","frame":"e_bbbbbbbbbb","group":"g_cccccccccc"}`,
		"bad id":            `{"id":"e_short","type":"sticky",` + geo + `,"z":"V"}`,
		"bad type":          `{"id":"e_aaaaaaaaaa","type":"table",` + geo + `,"z":"V"}`,
		"unknown field":     `{"id":"e_aaaaaaaaaa","type":"sticky",` + geo + `,"z":"V","color":"red"}`,
		"server field":      `{"id":"e_aaaaaaaaaa","type":"sticky",` + geo + `,"z":"V","inc":3}`,
		"style key":         `{"id":"e_aaaaaaaaaa","type":"sticky",` + geo + `,"z":"V","style":{"fill":"red"}}`,
		"style value":       `{"id":"e_aaaaaaaaaa","type":"text",` + geo + `,"z":"V","style":{"fontSize":11}}`,
		"sticky ink":        `{"id":"e_aaaaaaaaaa","type":"sticky",` + geo + `,"z":"V","style":{"color":"ink"}}`,
		"fractional size":   `{"id":"e_aaaaaaaaaa","type":"shape",` + geo + `,"z":"V","style":{"fontSize":12.5}}`,
		"nested frame":      `{"id":"e_aaaaaaaaaa","type":"frame",` + geo + `,"z":"V","frame":"e_bbbbbbbbbb"}`,
		"grouped frame":     `{"id":"e_aaaaaaaaaa","type":"frame",` + geo + `,"z":"V","group":"g_bbbbbbbbbb"}`,
		"rotation":          `{"id":"e_aaaaaaaaaa","type":"sticky","x":0,"y":0,"w":10,"h":10,"rot":400,"z":"V"}`,
		"zero size":         `{"id":"e_aaaaaaaaaa","type":"sticky","x":0,"y":0,"w":0,"h":10,"rot":0,"z":"V"}`,
		"far away":          `{"id":"e_aaaaaaaaaa","type":"sticky","x":20000000,"y":0,"w":10,"h":10,"rot":0,"z":"V"}`,
		"end both":          `{"id":"e_aaaaaaaaaa","type":"connector","z":"V","ends":[{"el":"e_bbbbbbbbbb","pt":[1,1]},{"pt":[3,4]}]}`,
		"end outside":       `{"id":"e_aaaaaaaaaa","type":"connector","z":"V","ends":[{"el":"e_bbbbbbbbbb","at":[1.5,0]},{"pt":[3,4]}]}`,
		"one end":           `{"id":"e_aaaaaaaaaa","type":"connector","z":"V","ends":[{"pt":[3,4]}]}`,
		"connector geom":    `{"id":"e_aaaaaaaaaa","type":"connector","z":"V","x":1,"ends":[{"pt":[0,0]},{"pt":[3,4]}]}`,
		"pen point":         `{"id":"e_aaaaaaaaaa","type":"pen",` + geo + `,"z":"V","pts":[0,0,10001,5]}`,
		"pen odd":           `{"id":"e_aaaaaaaaaa","type":"pen",` + geo + `,"z":"V","pts":[0,0,1]}`,
		"image hash":        `{"id":"e_aaaaaaaaaa","type":"image",` + geo + `,"z":"V","asset":"xyz","natural":[1,1]}`,
		"image natural":     `{"id":"e_aaaaaaaaaa","type":"image",` + geo + `,"z":"V","asset":"` + hash + `","natural":[0,1]}`,
		"z ends in zero":    `{"id":"e_aaaaaaaaaa","type":"sticky",` + geo + `,"z":"V0"}`,
		"z too long":        `{"id":"e_aaaaaaaaaa","type":"sticky",` + geo + `,"z":"` + strings.Repeat("V", 33) + `"}`,
		"text on image":     `{"id":"e_aaaaaaaaaa","type":"image",` + geo + `,"z":"V","asset":"` + hash + `","natural":[1,1],"text":"x"}`,
		"bad group":         `{"id":"e_aaaaaaaaaa","type":"sticky",` + geo + `,"z":"V","group":"e_bbbbbbbbbb"}`,
		"self-attached end": `{"id":"e_aaaaaaaaaa","type":"connector","z":"V","ends":[{"el":"e_aaaaaaaaaa","at":[0,0]},{"pt":[3,4]}]}`,
	}
	// The schema cannot compare two fields or require an even count: the
	// model alone refuses a connector attached to itself and a pen point
	// list of odd length.
	schemaOnlyGap := map[string]bool{"self-attached end": true, "pen odd": true}
	for name, raw := range samples {
		var x any
		if err := json.Unmarshal([]byte(raw), &x); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		serr := v.check(v.defs["Element"], x)
		merr := modelAccepts(raw)
		if schemaOnlyGap[name] {
			if serr != nil || merr == nil {
				t.Errorf("%s: schema %v, model %v", name, serr, merr)
			}
			continue
		}
		if (serr == nil) != (merr == nil) {
			t.Errorf("%s: schema says %v, model says %v", name, serr, merr)
		}
	}
}

func TestSchemaOperations(t *testing.T) {
	v := load(t)
	for name, c := range map[string]struct {
		raw string
		ok  bool
	}{
		"add":       {`[{"op":"add","el":{"id":"e_aaaaaaaaaa","type":"sticky"},"refs":{"e_bbbbbbbbbb":0}}]`, true},
		"set":       {`[{"op":"set","id":"e_aaaaaaaaaa","inc":0,"g":"geom","val":{"x":1,"y":2,"w":3,"h":4,"rot":0},"carry":"members"}]`, true},
		"semantic":  {`[{"op":"reparent","ids":["e_aaaaaaaaaa"],"inc_of":{"e_aaaaaaaaaa":3},"frame":null},{"op":"zorder","ids":["e_aaaaaaaaaa"],"inc_of":{"e_aaaaaaaaaa":3},"to":{"after":"e_bbbbbbbbbb","inc":2}}]`, true},
		"undo":      {`[{"op":"undo"}]`, true},
		"unknown":   {`[{"op":"set","id":"e_aaaaaaaaaa","inc":0,"g":"text","val":"x","colour":1}]`, false},
		"bad carry": {`[{"op":"set","id":"e_aaaaaaaaaa","inc":0,"g":"geom","val":{},"carry":"all"}]`, false},
	} {
		var x any
		json.Unmarshal([]byte(c.raw), &x)
		serr := v.check(v.defs["Batch"], x)
		if (serr == nil) != c.ok {
			t.Errorf("%s: schema %v", name, serr)
		}
		ops, derr := engine.DecodeOps([]byte(c.raw))
		if c.ok && (derr != nil || len(ops) == 0) {
			t.Errorf("%s: the engine does not decode it: %v", name, derr)
		}
		if !c.ok && name == "unknown" && derr == nil {
			t.Errorf("%s: the engine decodes an unknown field", name)
		}
	}
	var b any
	json.Unmarshal([]byte(`{"format":1,"title":"Retro","grid":true}`), &b)
	if err := v.check(v.defs["Board"], b); err != nil {
		t.Error(err)
	}
}
