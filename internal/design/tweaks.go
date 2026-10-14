package design

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Tweak is one named design value of an artboard.
type Tweak struct {
	Type    string   `json:"type"`
	Label   string   `json:"label,omitempty"`
	Value   any      `json:"value"`
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Step    *float64 `json:"step,omitempty"`
	Unit    string   `json:"unit,omitempty"`
	Options []string `json:"options,omitempty"`
}

const maxTweaks = 64

var (
	tweakName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	hexColor  = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)
	cssIdent  = regexp.MustCompile(`^[A-Za-z_-][A-Za-z0-9_-]*$`)
)

var tweakUnits = map[string]bool{"": true, "px": true, "rem": true, "em": true, "%": true}

// ParseTweaks parses and validates a tweaks block.
func ParseTweaks(raw []byte) (map[string]Tweak, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return map[string]Tweak{}, nil
	}
	var m map[string]Tweak
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("tweaks block is not a JSON object of tweaks: %w", err)
	}
	if len(m) > maxTweaks {
		return nil, fmt.Errorf("tweaks block has %d tweaks, over the limit of %d", len(m), maxTweaks)
	}
	for name, t := range m {
		if !tweakName.MatchString(name) {
			return nil, fmt.Errorf("tweak name %q must match %s", name, tweakName)
		}
		v, err := normalizeTweak(t, t.Value)
		if err != nil {
			return nil, fmt.Errorf("tweak %q: %w", name, err)
		}
		t.Value = v
		m[name] = t
	}
	return m, nil
}

// normalizeTweak validates value for t and returns it in canonical form:
// string for color, text and select; float64 for number; bool for boolean.
func normalizeTweak(t Tweak, value any) (any, error) {
	if len(t.Label) > 80 {
		return nil, fmt.Errorf("label is longer than 80 characters")
	}
	switch t.Type {
	case "color":
		s, ok := value.(string)
		if !ok || !hexColor.MatchString(s) {
			return nil, fmt.Errorf("a color value is #rgb, #rrggbb or #rrggbbaa")
		}
		return strings.ToLower(s), nil
	case "number":
		f, ok := number(value)
		if !ok {
			return nil, fmt.Errorf("a number value must be a finite number")
		}
		if !tweakUnits[t.Unit] {
			return nil, fmt.Errorf("unit must be px, rem, em, %% or empty")
		}
		if t.Min != nil && f < *t.Min || t.Max != nil && f > *t.Max {
			return nil, fmt.Errorf("value %v is outside min/max", f)
		}
		return f, nil
	case "text":
		s, ok := value.(string)
		if !ok || len([]rune(s)) > 200 {
			return nil, fmt.Errorf("a text value is a string of at most 200 characters")
		}
		return s, nil
	case "select":
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("a select value is a string")
		}
		if len(t.Options) == 0 || len(t.Options) > 32 {
			return nil, fmt.Errorf("a select tweak has 1 to 32 options")
		}
		found := false
		for _, o := range t.Options {
			if len([]rune(o)) > 80 {
				return nil, fmt.Errorf("option %q is longer than 80 characters", o)
			}
			found = found || o == s
		}
		if !found {
			return nil, fmt.Errorf("value %q is not one of the options", s)
		}
		return s, nil
	case "boolean":
		b, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("a boolean value is true or false")
		}
		return b, nil
	}
	return nil, fmt.Errorf("type must be color, number, text, select or boolean")
}

func number(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case json.Number:
		var err error
		if f, err = n.Float64(); err != nil {
			return 0, false
		}
	case float64:
		f = n
	default:
		return 0, false
	}
	return f, !math.IsNaN(f) && !math.IsInf(f, 0)
}

// SetTweak validates value against the named tweak and returns the updated
// set.
func SetTweak(m map[string]Tweak, name string, value any) (map[string]Tweak, error) {
	t, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("artboard has no tweak %q", name)
	}
	v, err := normalizeTweak(t, value)
	if err != nil {
		return nil, fmt.Errorf("tweak %q: %w", name, err)
	}
	out := make(map[string]Tweak, len(m))
	for k, x := range m {
		out[k] = x
	}
	t.Value = v
	out[name] = t
	return out, nil
}

// MarshalTweaks renders a tweaks block body: sorted keys, 2-space indent.
func MarshalTweaks(m map[string]Tweak) ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	// The block sits inside <script>: never let it close the element.
	b = bytes.ReplaceAll(b, []byte("</"), []byte(`<\/`))
	return b, nil
}

// TweakCSS renders the tweaks as CSS custom properties on :root.
func TweakCSS(m map[string]Tweak) string {
	if len(m) == 0 {
		return ""
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(":root{")
	for _, k := range names {
		b.WriteString("--")
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(TweakValueCSS(m[k]))
		b.WriteByte(';')
	}
	b.WriteString("}")
	return b.String()
}

// TweakValueCSS renders one tweak value as a CSS value.
func TweakValueCSS(t Tweak) string {
	switch v := t.Value.(type) {
	case bool:
		if v {
			return "1"
		}
		return "0"
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64) + t.Unit
	case string:
		if t.Type == "color" {
			return v
		}
		if t.Type == "select" && cssIdent.MatchString(v) {
			return v
		}
		return cssString(v)
	}
	return "initial"
}

// cssString quotes s as a CSS string that cannot end a <style> element.
func cssString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\' || r == '<' || r == '>' || r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\%x ", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
