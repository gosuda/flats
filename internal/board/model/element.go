package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"unicode/utf8"
)

// Element types.
const (
	TypeSticky    = "sticky"
	TypeText      = "text"
	TypeShape     = "shape"
	TypeConnector = "connector"
	TypePen       = "pen"
	TypeImage     = "image"
	TypeFrame     = "frame"
	// TypeBoard is the type of the board metadata pseudo-element (3.3).
	TypeBoard = "_board"
)

// BoardID is the id of the board metadata pseudo-element.
const BoardID = "_board"

// Types lists the element types in a fixed order.
var Types = []string{TypeSticky, TypeText, TypeShape, TypeConnector, TypePen, TypeImage, TypeFrame}

// ErrFormat marks a malformed element, bundle value or operation.
var ErrFormat = errors.New("invalid board data")

var (
	idPattern    = regexp.MustCompile(`^e_[0-9A-Za-z]{10}$`)
	groupPattern = regexp.MustCompile(`^g_[0-9A-Za-z]{10}$`)
	assetPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidID reports whether id has the form of an element id.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// ValidGroupID reports whether id has the form of a group id.
func ValidGroupID(id string) bool { return groupPattern.MatchString(id) }

// ValidAsset reports whether h has the form of an asset hash.
func ValidAsset(h string) bool { return assetPattern.MatchString(h) }

// ValidType reports whether typ is an element type.
func ValidType(typ string) bool { return slices.Contains(Types, typ) }

// HasText reports whether elements of typ carry text.
func HasText(typ string) bool {
	switch typ {
	case TypeSticky, TypeText, TypeShape, TypeConnector, TypeFrame:
		return true
	}
	return false
}

func formatErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrFormat, fmt.Sprintf(format, args...))
}

// Round1 rounds v to one decimal place, half away from zero, and turns
// negative zero into zero. Every stored number goes through it.
func Round1(v float64) float64 {
	r := math.Round(v*10) / 10
	if r == 0 {
		return 0
	}
	return r
}

// Geom is the geom bundle: position, size and rotation in canvas pixels.
type Geom struct {
	X   float64 `json:"x"`
	Y   float64 `json:"y"`
	W   float64 `json:"w"`
	H   float64 `json:"h"`
	Rot float64 `json:"rot"`
}

// Round rounds every field to one decimal place.
func (g Geom) Round() Geom {
	return Geom{Round1(g.X), Round1(g.Y), Round1(g.W), Round1(g.H), Round1(g.Rot)}
}

func (g Geom) validate() error {
	for _, v := range []float64{g.X, g.Y, g.W, g.H, g.Rot} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return formatErr("numbers must be finite")
		}
	}
	if math.Abs(g.X) > MaxCoord || math.Abs(g.Y) > MaxCoord {
		return formatErr("coordinates must be within ±%d", MaxCoord)
	}
	if g.W < MinSize || g.W > MaxSize || g.H < MinSize || g.H > MaxSize {
		return formatErr("sizes must be %d to %d", MinSize, MaxSize)
	}
	if g.Rot < 0 || g.Rot > MaxRot {
		return formatErr("rotation must be 0 to %d", MaxRot)
	}
	return nil
}

// End is one connector end: attached to element El at the normalized point
// At, or free at the canvas coordinate Pt.
type End struct {
	El string
	At [2]float64
	Pt [2]float64
}

// Attached reports whether the end is attached to an element.
func (e End) Attached() bool { return e.El != "" }

func (e End) content() map[string]any {
	if e.El != "" {
		return map[string]any{"el": e.El, "at": []float64{e.At[0], e.At[1]}}
	}
	return map[string]any{"pt": []float64{e.Pt[0], e.Pt[1]}}
}

// MarshalJSON writes {"el", "at"} or {"pt"}.
func (e End) MarshalJSON() ([]byte, error) { return Canonical(e.content()), nil }

// UnmarshalJSON reads an end. A missing (or null) "at" reads as the center
// of the target; agent calls replace it with DefaultAt (4.4). A null field
// reads as missing, and a null coordinate is refused: encoding/json would
// read it as 0. An empty "el" is refused too, since it names no element.
func (e *End) UnmarshalJSON(raw []byte) error {
	var f struct {
		El *string         `json:"el"`
		At json.RawMessage `json:"at"`
		Pt json.RawMessage `json:"pt"`
	}
	if err := strictDecode(raw, &f); err != nil {
		return formatErr("connector end: %v", err)
	}
	for _, m := range []*json.RawMessage{&f.At, &f.Pt} {
		if string(bytes.TrimSpace(*m)) == "null" {
			*m = nil
		}
	}
	switch {
	case f.El != nil && f.Pt == nil:
		if *f.El == "" {
			return formatErr("connector end el must name an element")
		}
		*e = End{El: *f.El, At: [2]float64{0.5, 0.5}}
		if f.At != nil {
			at, err := pair("at", f.At)
			if err != nil {
				return err
			}
			e.At = at
		}
	case f.El == nil && f.Pt != nil && f.At == nil:
		pt, err := pair("pt", f.Pt)
		if err != nil {
			return err
		}
		*e = End{Pt: pt}
	default:
		return formatErr("a connector end is {el, at} or {pt}")
	}
	return nil
}

// pair reads [x, y]. Slices of pointers, not arrays of numbers:
// encoding/json pads a short array, drops the extra items of a long one and
// reads null as 0.
func pair(name string, raw json.RawMessage) ([2]float64, error) {
	var v []*float64
	if err := json.Unmarshal(raw, &v); err != nil || len(v) != 2 || v[0] == nil || v[1] == nil {
		return [2]float64{}, formatErr("connector end %s is [x, y] numbers", name)
	}
	return [2]float64{*v[0], *v[1]}, nil
}

// Round rounds the end's numbers.
func (e End) Round() End {
	if e.El != "" {
		return End{El: e.El, At: [2]float64{Round1(e.At[0]), Round1(e.At[1])}}
	}
	return End{Pt: [2]float64{Round1(e.Pt[0]), Round1(e.Pt[1])}}
}

func (e End) validate() error {
	if e.El != "" {
		if !ValidID(e.El) {
			return formatErr("connector end target %q is not an element id", e.El)
		}
		for _, a := range e.At {
			if !(a >= 0 && a <= 1) {
				return formatErr("connector end points must be 0 to 1")
			}
		}
		return nil
	}
	for _, p := range e.Pt {
		if !(math.Abs(p) <= MaxCoord) {
			return formatErr("connector end coordinates must be within ±%d", MaxCoord)
		}
	}
	return nil
}

// Element holds the content fields of an element (3.1). The board metadata
// pseudo-element uses Type TypeBoard, Text as its title and Grid.
type Element struct {
	ID      string
	Type    string
	Geom    Geom // not for connectors
	Z       string
	Frame   string
	Group   string
	Locked  bool
	Text    string
	Style   Style
	Ends    [2]End // connectors
	Pts     []int  // pens; never changed after creation
	Asset   string // images
	Alt     string // images
	Natural [2]int // images
	By      string
	At      int64
	Grid    bool // board metadata only
	// TA records who last wrote each text bundle and LK the kind of the
	// last write of any bundle (board-public-edit-TECH.md 7). Neither is a
	// bundle; old elements have neither. TA is replaced, never changed in
	// place, so clones share it.
	TA map[string]TextAuthor
	LK string
}

// Clone copies e. Pts is shared because it never changes.
func (e Element) Clone() Element {
	e.Style = e.Style.Clone()
	return e
}

// Content is the element's content fields as a JSON-ready map. It is the
// form of bucket files, data files and the private element.
func (e Element) Content() map[string]any {
	if e.Type == TypeBoard {
		return map[string]any{"format": 1, "title": e.Text, "grid": e.Grid}
	}
	m := map[string]any{
		"id":     e.ID,
		"type":   e.Type,
		"z":      e.Z,
		"locked": e.Locked,
		"by":     e.By,
		"at":     e.At,
	}
	if e.Type != TypeConnector {
		m["x"], m["y"], m["w"], m["h"], m["rot"] = e.Geom.X, e.Geom.Y, e.Geom.W, e.Geom.H, e.Geom.Rot
	}
	if e.Frame != "" {
		m["frame"] = e.Frame
	}
	if e.Group != "" {
		m["group"] = e.Group
	}
	if HasText(e.Type) {
		m["text"] = e.Text
	}
	if len(styleSchema[e.Type]) > 0 {
		m["style"] = map[string]any(e.Style)
	}
	switch e.Type {
	case TypeConnector:
		m["ends"] = []any{e.Ends[0].content(), e.Ends[1].content()}
	case TypePen:
		m["pts"] = e.Pts
	case TypeImage:
		m["asset"], m["alt"], m["natural"] = e.Asset, e.Alt, []int{e.Natural[0], e.Natural[1]}
	}
	if len(e.TA) > 0 {
		m["ta"] = e.TA
	}
	if e.LK != "" {
		m["lk"] = e.LK
	}
	return m
}

// Canonical encodes v as JSON with sorted object keys and no HTML escaping,
// so equal content always gives equal bytes.
func Canonical(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(fmt.Sprintf("board: canonical encoding failed: %v", err))
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// CanonicalJSON is the canonical JSON of e's content fields.
func (e Element) CanonicalJSON() []byte { return Canonical(e.Content()) }

// Equal reports whether a and b have equal content.
func Equal(a, b Element) bool { return bytes.Equal(a.CanonicalJSON(), b.CanonicalJSON()) }

// MarshalJSON writes the content fields.
func (e Element) MarshalJSON() ([]byte, error) { return e.CanonicalJSON(), nil }

// UnmarshalJSON reads content fields, refusing unknown and server-managed
// fields. Missing optional fields take their defaults; use DecodeElement to
// learn which fields were present.
func (e *Element) UnmarshalJSON(raw []byte) error {
	el, _, err := DecodeElement(raw)
	if err != nil {
		return err
	}
	*e = el
	return nil
}

// serverFields are runtime fields a client may never send.
var serverFields = []string{"inc", "v", "deleted", "del_seq", "pre_rel", "end_inc", "end_last", "ver"}

var fieldsByType = map[string][]string{
	TypeSticky:    {"text", "x", "y", "w", "h", "rot"},
	TypeText:      {"text", "x", "y", "w", "h", "rot"},
	TypeShape:     {"text", "x", "y", "w", "h", "rot"},
	TypeConnector: {"text", "ends"},
	TypePen:       {"pts", "x", "y", "w", "h", "rot"},
	TypeImage:     {"asset", "alt", "natural", "x", "y", "w", "h", "rot"},
	TypeFrame:     {"text", "x", "y", "w", "h", "rot"},
}

var commonFields = []string{"id", "type", "z", "frame", "group", "locked", "style", "by", "at", "ta", "lk"}

// Present records which fields a decoded element carried.
type Present map[string]bool

// HasGeom reports whether every one of x, y, w and h was present.
func (p Present) HasGeom() bool { return p["x"] && p["y"] && p["w"] && p["h"] }

// DecodeElement reads content fields without checking ranges or limits.
// Missing style keys take their defaults. Call Validate before storing.
func DecodeElement(raw []byte) (Element, Present, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil || all == nil {
		return Element{}, nil, formatErr("an element is a JSON object")
	}
	var e Element
	if err := decodeField(all, "type", &e.Type); err != nil {
		return e, nil, err
	}
	if !ValidType(e.Type) {
		return e, nil, formatErr("unknown element type %q", e.Type)
	}
	present := Present{}
	for k := range all {
		if slices.Contains(serverFields, k) {
			return e, nil, formatErr("%q is managed by the server", k)
		}
		if !slices.Contains(commonFields, k) && !slices.Contains(fieldsByType[e.Type], k) {
			return e, nil, formatErr("unknown field %q for a %s", k, e.Type)
		}
		if string(all[k]) != "null" {
			present[k] = true
		}
	}
	// Pointers: encoding/json reads a null number as 0.
	var natural, pts []*int
	var ends []End
	for _, f := range []struct {
		key string
		dst any
	}{
		{"id", &e.ID}, {"z", &e.Z}, {"frame", &e.Frame}, {"group", &e.Group},
		{"locked", &e.Locked}, {"text", &e.Text}, {"by", &e.By}, {"at", &e.At}, {"ta", &e.TA}, {"lk", &e.LK},
		{"x", &e.Geom.X}, {"y", &e.Geom.Y}, {"w", &e.Geom.W}, {"h", &e.Geom.H}, {"rot", &e.Geom.Rot},
		{"pts", &pts}, {"asset", &e.Asset}, {"alt", &e.Alt}, {"natural", &natural}, {"ends", &ends},
	} {
		if err := decodeField(all, f.key, f.dst); err != nil {
			return e, nil, err
		}
	}
	if present["natural"] {
		if len(natural) != 2 || natural[0] == nil || natural[1] == nil {
			return e, nil, formatErr("natural is [w, h]")
		}
		e.Natural = [2]int{*natural[0], *natural[1]}
	}
	if present["pts"] {
		e.Pts = make([]int, len(pts))
		for i, p := range pts {
			if p == nil {
				return e, nil, formatErr("pts holds numbers")
			}
			e.Pts[i] = *p
		}
	}
	if present["ends"] {
		if len(ends) != 2 {
			return e, nil, formatErr("a connector has exactly two ends")
		}
		e.Ends = [2]End{ends[0], ends[1]}
	}
	style, err := DecodeStyle(e.Type, all["style"])
	if err != nil {
		return e, nil, err
	}
	e.Style = style
	return e, present, nil
}

func decodeField(all map[string]json.RawMessage, key string, dst any) error {
	raw, ok := all[key]
	if !ok || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		if errors.Is(err, ErrFormat) {
			return err
		}
		return formatErr("field %q: %v", key, err)
	}
	return nil
}

func strictDecode(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// Normalize rounds the element's numbers.
func (e *Element) Normalize() {
	if e.Type == TypeConnector {
		e.Ends = [2]End{e.Ends[0].Round(), e.Ends[1].Round()}
	} else {
		e.Geom = e.Geom.Round()
	}
}

// Validate checks the element's form and ranges. Text length, pen points and
// asset counts are limits, checked separately (Limits).
func (e Element) Validate() error {
	if !ValidID(e.ID) {
		return formatErr("element id %q must be e_ plus 10 base62 characters", e.ID)
	}
	if !ValidType(e.Type) {
		return formatErr("unknown element type %q", e.Type)
	}
	if e.Type != TypeConnector {
		if err := e.Geom.validate(); err != nil {
			return err
		}
	}
	if e.Z != "" && !validKeyInput(e.Z) {
		return formatErr("z %q is not a stacking key", e.Z)
	}
	if e.Frame != "" && !ValidID(e.Frame) {
		return formatErr("frame %q is not an element id", e.Frame)
	}
	if e.Frame != "" && e.Type == TypeFrame {
		return formatErr("frames do not nest")
	}
	if e.Group != "" && !ValidGroupID(e.Group) {
		return formatErr("group %q is not a group id", e.Group)
	}
	if e.Group != "" && e.Type == TypeFrame {
		return formatErr("a frame cannot join a group")
	}
	if !HasText(e.Type) && e.Text != "" {
		return formatErr("a %s has no text", e.Type)
	}
	if !utf8.ValidString(e.Text) || !utf8.ValidString(e.Alt) {
		return formatErr("text must be valid UTF-8")
	}
	if err := ValidateStyle(e.Type, e.Style); err != nil {
		return err
	}
	if err := e.validAuthorship(); err != nil {
		return err
	}
	switch e.Type {
	case TypeConnector:
		for _, end := range e.Ends {
			if err := end.validate(); err != nil {
				return err
			}
			if end.El == e.ID {
				return formatErr("a connector cannot attach to itself")
			}
		}
	case TypePen:
		if len(e.Pts) < 2 || len(e.Pts)%2 != 0 {
			return formatErr("pts holds x, y pairs")
		}
		for _, p := range e.Pts {
			if p < 0 || p > PenScale {
				return formatErr("pen points must be 0 to %d", PenScale)
			}
		}
	case TypeImage:
		if !ValidAsset(e.Asset) {
			return formatErr("asset must be a sha256 hash")
		}
		if e.Natural[0] < 1 || e.Natural[1] < 1 || e.Natural[0] > 1<<20 || e.Natural[1] > 1<<20 {
			return formatErr("natural is the image's [w, h] in pixels")
		}
	}
	return nil
}

// TextLen counts characters (code points) of text.
func TextLen(s string) int { return utf8.RuneCountInString(s) }

// PenPoints is the number of points of a pen element.
func (e Element) PenPoints() int { return len(e.Pts) / 2 }
