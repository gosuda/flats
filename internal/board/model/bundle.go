package model

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"
)

// Bundle keys (3.2). A style key k is the bundle "style.k".
const (
	BundleGeom   = "geom"
	BundleEnd0   = "end0"
	BundleEnd1   = "end1"
	BundleText   = "text"
	BundleAlt    = "alt"
	BundleLocked = "locked"
	BundleZ      = "z"
	BundleFrame  = "frame"
	BundleGroup  = "group"
	BundleTitle  = "title"
	BundleGrid   = "grid"
	StylePrefix  = "style."
)

// VFixed is the key of v that versions the fields outside every bundle: the
// author fields and a pen's points or an image's asset and natural size. It
// is no bundle; only a whole-board rollback or replacement that changes one
// of those fields under a kept incarnation sets it (4.11, 4.12), so an
// element's ver = max(inc, v[*]) grows with that change too (8.1).
const VFixed = "fixed"

// EndBundle is the bundle key of connector end i.
func EndBundle(i int) string {
	if i == 0 {
		return BundleEnd0
	}
	return BundleEnd1
}

// EndIndex is the end index of an end bundle key, or -1.
func EndIndex(g string) int {
	switch g {
	case BundleEnd0:
		return 0
	case BundleEnd1:
		return 1
	}
	return -1
}

// IsRelation reports whether g is a membership bundle, which only semantic
// operations change.
func IsRelation(g string) bool { return g == BundleFrame || g == BundleGroup }

var bundlesByType = func() map[string][]string {
	m := map[string][]string{TypeBoard: bundlesOf(TypeBoard)}
	for _, t := range Types {
		m[t] = bundlesOf(t)
	}
	return m
}()

// Bundles lists every bundle key of typ in a fixed order, including the
// membership bundles. The caller must not change the returned slice.
func Bundles(typ string) []string { return bundlesByType[typ] }

func bundlesOf(typ string) []string {
	if typ == TypeBoard {
		return []string{BundleGrid, BundleTitle}
	}
	var gs []string
	switch typ {
	case TypeConnector:
		gs = append(gs, BundleEnd0, BundleEnd1)
	default:
		gs = append(gs, BundleGeom)
	}
	if HasText(typ) {
		gs = append(gs, BundleText)
	}
	if typ == TypeImage {
		gs = append(gs, BundleAlt)
	}
	gs = append(gs, BundleFrame, BundleGroup, BundleLocked, BundleZ)
	for _, k := range StyleKeys(typ) {
		gs = append(gs, StylePrefix+k)
	}
	return gs
}

// HasBundle reports whether typ has bundle g.
func HasBundle(typ, g string) bool { return slices.Contains(Bundles(typ), g) }

// Settable reports whether an ordinary set may change bundle g of typ.
func Settable(typ, g string) bool { return !IsRelation(g) && HasBundle(typ, g) }

// Bundle returns the value of bundle g: Geom, End, string, bool, or a style
// value. Membership bundles return "" for none.
func (e Element) Bundle(g string) any {
	switch g {
	case BundleGeom:
		return e.Geom
	case BundleEnd0:
		return e.Ends[0]
	case BundleEnd1:
		return e.Ends[1]
	case BundleText, BundleTitle:
		return e.Text
	case BundleAlt:
		return e.Alt
	case BundleLocked:
		return e.Locked
	case BundleZ:
		return e.Z
	case BundleFrame:
		return e.Frame
	case BundleGroup:
		return e.Group
	case BundleGrid:
		return e.Grid
	}
	if k, ok := strings.CutPrefix(g, StylePrefix); ok {
		return e.Style[k]
	}
	return nil
}

// SetBundle sets bundle g to v, a value of the type Bundle returns. The
// caller has cloned e's style when it shares it.
func (e *Element) SetBundle(g string, v any) {
	switch g {
	case BundleGeom:
		e.Geom = v.(Geom)
	case BundleEnd0:
		e.Ends[0] = v.(End)
	case BundleEnd1:
		e.Ends[1] = v.(End)
	case BundleText, BundleTitle:
		e.Text = v.(string)
	case BundleAlt:
		e.Alt = v.(string)
	case BundleLocked:
		e.Locked = v.(bool)
	case BundleZ:
		e.Z = v.(string)
	case BundleFrame:
		e.Frame = v.(string)
	case BundleGroup:
		e.Group = v.(string)
	case BundleGrid:
		e.Grid = v.(bool)
	default:
		k, _ := strings.CutPrefix(g, StylePrefix)
		e.Style[k] = v
	}
}

// BundleJSON encodes a bundle value. Empty membership encodes as null.
func BundleJSON(g string, v any) json.RawMessage {
	if IsRelation(g) && v == "" {
		return json.RawMessage("null")
	}
	if end, ok := v.(End); ok {
		return Canonical(end.content())
	}
	return Canonical(v)
}

// BundleEqual reports whether two values of bundle g are equal.
func BundleEqual(g string, a, b any) bool {
	return bytes.Equal(BundleJSON(g, a), BundleJSON(g, b))
}

// DecodeBundle reads and validates the value of bundle g of typ. Numbers are
// rounded. Text length is a limit and is not checked here.
func DecodeBundle(typ, g string, raw json.RawMessage) (any, error) {
	if !HasBundle(typ, g) {
		return nil, formatErr("a %s has no %q", typ, g)
	}
	// Decoding null into a string or bool leaves its zero value, so null
	// would clear text or unlock; only membership means "none" by null.
	if !IsRelation(g) && string(bytes.TrimSpace(raw)) == "null" {
		return nil, formatErr("%s cannot be null", g)
	}
	switch g {
	case BundleGeom:
		var f struct{ X, Y, W, H, Rot *float64 }
		if err := strictDecode(raw, &f); err != nil || f.X == nil || f.Y == nil || f.W == nil || f.H == nil || f.Rot == nil {
			return nil, formatErr("geom is {x, y, w, h, rot}")
		}
		geom := Geom{*f.X, *f.Y, *f.W, *f.H, *f.Rot}.Round()
		if err := geom.validate(); err != nil {
			return nil, err
		}
		return geom, nil
	case BundleEnd0, BundleEnd1:
		var end End
		if err := json.Unmarshal(raw, &end); err != nil {
			return nil, err
		}
		end = end.Round()
		if err := end.validate(); err != nil {
			return nil, err
		}
		return end, nil
	case BundleText, BundleAlt, BundleTitle:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || !utf8.ValidString(s) {
			return nil, formatErr("%s is a string", g)
		}
		if g == BundleTitle && TextLen(s) > MaxTitle {
			return nil, formatErr("the title is at most %d characters", MaxTitle)
		}
		return s, nil
	case BundleLocked, BundleGrid:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, formatErr("%s is true or false", g)
		}
		return b, nil
	case BundleZ:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || !validKeyInput(s) {
			return nil, formatErr("z is a stacking key")
		}
		return s, nil
	case BundleFrame, BundleGroup:
		var s *string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, formatErr("%s is an id or null", g)
		}
		if s == nil {
			return "", nil
		}
		if (g == BundleFrame && !ValidID(*s)) || (g == BundleGroup && !ValidGroupID(*s)) {
			return nil, formatErr("%s %q has the wrong form", g, *s)
		}
		return *s, nil
	}
	k, _ := strings.CutPrefix(g, StylePrefix)
	return DecodeStyleValue(typ, k, raw)
}
