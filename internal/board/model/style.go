package model

import (
	"encoding/json"
	"maps"
	"math"
	"slices"
	"sort"
)

// Style holds an element's style keys. Values are strings or integral
// float64 numbers. A stored style always holds every key of its type.
type Style map[string]any

// Clone copies s.
func (s Style) Clone() Style { return maps.Clone(s) }

// Color names (Appendix A). The client maps each name to theme colors.
var (
	StickyColors = []string{"yellow", "orange", "pink", "blue", "green", "gray"}
	InkColors    = []string{"ink", "gray", "red", "orange", "green", "blue", "purple"}
)

// styleSpec is one style key: a default and the allowed values.
type styleSpec struct {
	def  any
	enum []string  // allowed strings
	nums []float64 // allowed numbers
	min  int       // integer range when max > 0
	max  int
}

func (s styleSpec) decode(raw json.RawMessage) (any, bool) {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str, slices.Contains(s.enum, str)
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil || n != math.Trunc(n) {
		return nil, false
	}
	if slices.Contains(s.nums, n) || (s.max > 0 && n >= float64(s.min) && n <= float64(s.max)) {
		return n, true
	}
	return nil, false
}

func (s styleSpec) valid(v any) bool {
	switch v := v.(type) {
	case string:
		return slices.Contains(s.enum, v)
	case float64:
		return v == math.Trunc(v) && (slices.Contains(s.nums, v) || (s.max > 0 && v >= float64(s.min) && v <= float64(s.max)))
	}
	return false
}

func with(list []string, more ...string) []string { return append(slices.Clone(list), more...) }

var (
	strokeSpec      = styleSpec{def: "ink", enum: InkColors}
	strokeWidthSpec = styleSpec{def: 2.0, nums: []float64{1, 2, 4}}
	dashSpec        = styleSpec{def: "solid", enum: []string{"solid", "dashed", "dotted"}}
)

var styleSchema = map[string]map[string]styleSpec{
	TypeSticky: {
		"color":    {def: "yellow", enum: StickyColors},
		"fontSize": {def: "auto", enum: []string{"auto"}, min: 10, max: 72},
	},
	TypeText: {
		"color":    {def: "ink", enum: InkColors},
		"fontSize": {def: 20.0, min: 12, max: 144},
		"align":    {def: "left", enum: []string{"left", "center", "right"}},
	},
	TypeShape: {
		"shape":       {def: "rect", enum: []string{"rect", "round", "ellipse", "diamond"}},
		"fill":        {def: "white", enum: with(StickyColors, "none", "white")},
		"stroke":      strokeSpec,
		"strokeWidth": strokeWidthSpec,
		"dash":        dashSpec,
		"fontSize":    {def: 14.0, min: 10, max: 72},
	},
	TypeConnector: {
		"stroke":      strokeSpec,
		"strokeWidth": strokeWidthSpec,
		"dash":        dashSpec,
		"route":       {def: "straight", enum: []string{"straight", "elbow"}},
		"head0":       {def: "none", enum: []string{"none", "arrow"}},
		"head1":       {def: "arrow", enum: []string{"arrow", "none"}},
		"fontSize":    {def: 12.0, min: 10, max: 32},
	},
	TypePen: {
		"stroke": {def: "ink", enum: with(InkColors, "highlight")},
		"width":  {def: "pen", enum: []string{"pen", "marker"}},
	},
	TypeImage: {},
	TypeFrame: {
		"fill": {def: "white", enum: []string{"white", "none", "gray"}},
	},
}

// StyleKeys lists the style keys of typ in sorted order.
func StyleKeys(typ string) []string {
	keys := make([]string, 0, len(styleSchema[typ]))
	for k := range styleSchema[typ] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// DefaultStyle is the style of typ with every key at its default.
func DefaultStyle(typ string) Style {
	s := Style{}
	for k, spec := range styleSchema[typ] {
		s[k] = spec.def
	}
	return s
}

// DecodeStyle reads a style object for typ: unknown keys and values are
// refused, missing keys take their defaults. raw may be empty.
func DecodeStyle(typ string, raw json.RawMessage) (Style, error) {
	s := DefaultStyle(typ)
	if len(raw) == 0 || string(raw) == "null" {
		return s, nil
	}
	part, err := DecodeStylePart(typ, raw)
	if err != nil {
		return nil, err
	}
	maps.Copy(s, part)
	return s, nil
}

// DecodeStylePart reads only the style keys present in raw.
func DecodeStylePart(typ string, raw json.RawMessage) (Style, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil || all == nil {
		return nil, formatErr("style is an object")
	}
	s := Style{}
	for k, v := range all {
		val, err := DecodeStyleValue(typ, k, v)
		if err != nil {
			return nil, err
		}
		s[k] = val
	}
	return s, nil
}

// DecodeStyleValue reads one style value.
func DecodeStyleValue(typ, key string, raw json.RawMessage) (any, error) {
	spec, ok := styleSchema[typ][key]
	if !ok {
		return nil, formatErr("unknown style key %q for a %s", key, typ)
	}
	v, ok := spec.decode(raw)
	if !ok {
		return nil, formatErr("style %s value %s is not allowed for a %s", key, raw, typ)
	}
	return v, nil
}

// ValidateStyle checks that s holds exactly the keys of typ with allowed values.
func ValidateStyle(typ string, s Style) error {
	schema := styleSchema[typ]
	if len(s) != len(schema) {
		return formatErr("style of a %s must hold %v", typ, StyleKeys(typ))
	}
	for k, v := range s {
		spec, ok := schema[k]
		if !ok {
			return formatErr("unknown style key %q for a %s", k, typ)
		}
		if !spec.valid(v) {
			return formatErr("style %s value %v is not allowed for a %s", k, v, typ)
		}
	}
	return nil
}
