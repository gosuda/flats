package notebook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type PropertyType string

const (
	TitleType       PropertyType = "title"
	TextType        PropertyType = "text"
	NumberType      PropertyType = "number"
	SelectType      PropertyType = "select"
	MultiSelectType PropertyType = "multi_select"
	DateType        PropertyType = "date"
	CheckboxType    PropertyType = "checkbox"
	URLType         PropertyType = "url"
)

func (t PropertyType) Validate() error {
	switch t {
	case TitleType, TextType, NumberType, SelectType, MultiSelectType, DateType, CheckboxType, URLType:
		return nil
	}
	return fmt.Errorf("unknown property type %q", t)
}

type NumberFormat string

const (
	PlainFormat   NumberFormat = "plain"
	CommaFormat   NumberFormat = "comma"
	PercentFormat NumberFormat = "percent"
)

type PropertyDef struct {
	Name   string       `json:"name"`
	Type   PropertyType `json:"type"`
	Format NumberFormat `json:"format,omitempty"`
}

func DecodePropertyDef(data []byte) (PropertyDef, error) {
	var v PropertyDef
	err := v.UnmarshalJSON(data)
	return v, err
}
func (p PropertyDef) Validate() error {
	if err := validateText(p.Name); err != nil {
		return err
	}
	if err := p.Type.Validate(); err != nil {
		return err
	}
	if p.Type != NumberType && p.Format != "" {
		return fmt.Errorf("format is only valid for number")
	}
	switch p.Format {
	case "", PlainFormat, CommaFormat, PercentFormat:
		return nil
	}
	return fmt.Errorf("unknown number format %q", p.Format)
}
func (p PropertyDef) MarshalJSON() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if p.Format == PlainFormat {
		p.Format = ""
	}
	type wire PropertyDef
	return json.Marshal(wire(p))
}
func (p *PropertyDef) UnmarshalJSON(data []byte) error {
	if err := requiredFields(data, "name", "type"); err != nil {
		return err
	}
	type wire PropertyDef
	var v wire
	if err := strictDecode(data, &v); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return err
	}
	if raw, ok := fields["format"]; ok {
		if v.Type != NumberType || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || v.Format == "" {
			return fmt.Errorf("invalid format field")
		}
	}
	if err := PropertyDef(v).Validate(); err != nil {
		return err
	}
	if v.Format == PlainFormat {
		v.Format = ""
	}
	*p = PropertyDef(v)
	return nil
}

type Color string

const (
	Gray   Color = "gray"
	Brown  Color = "brown"
	Orange Color = "orange"
	Yellow Color = "yellow"
	Green  Color = "green"
	Blue   Color = "blue"
	Purple Color = "purple"
	Pink   Color = "pink"
	Red    Color = "red"
)

func (c Color) Validate() error {
	switch c {
	case Gray, Brown, Orange, Yellow, Green, Blue, Purple, Pink, Red:
		return nil
	}
	return fmt.Errorf("unknown option color %q", c)
}

type Decimal string

var decimalPattern = regexp.MustCompile(`^([+-]?)([0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE]([+-]?[0-9]+))?$`)

func ParseDecimal(input string) (Decimal, error) {
	match := decimalPattern.FindStringSubmatch(input)
	if match == nil {
		return "", fmt.Errorf("invalid decimal %q", input)
	}
	mantissa := match[2]
	point := strings.IndexByte(mantissa, '.')
	if point < 0 {
		point = len(mantissa)
	}
	digits := strings.ReplaceAll(mantissa, ".", "")
	leading := len(digits) - len(strings.TrimLeft(digits, "0"))
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", nil
	}
	exponent := int64(0)
	if match[3] != "" {
		var err error
		exponent, err = strconv.ParseInt(match[3], 10, 64)
		if err != nil {
			return "", fmt.Errorf("decimal exponent too large")
		}
	}
	decimalPoint := int64(point - leading)
	if exponent > 0 && decimalPoint > math.MaxInt64-exponent ||
		exponent < 0 && decimalPoint < math.MinInt64-exponent {
		return "", fmt.Errorf("decimal position overflows int64")
	}
	decimalPoint += exponent
	digits = strings.TrimRight(digits, "0")
	// An exponent can make a short input expand without bound, so expansion
	// is capped at twice the cell bound; admission enforces the bound itself.
	// A canonical decimal has no exponent and never grows, so retained values
	// longer than the cap still load.
	limit := 2 * int64(max(len(input), MaxCellCodePoints))
	if decimalPoint < -limit || decimalPoint > limit {
		return "", fmt.Errorf("expanded decimal exceeds %d characters", limit)
	}
	length := int64(len(digits))
	switch {
	case decimalPoint <= 0:
		length += 2 - decimalPoint
	case decimalPoint >= length:
		length = decimalPoint
	default:
		length++
	}
	if match[1] == "-" {
		length++
	}
	if length > limit {
		return "", fmt.Errorf("expanded decimal exceeds %d characters", limit)
	}
	var canonical string
	switch {
	case decimalPoint <= 0:
		canonical = "0." + strings.Repeat("0", int(-decimalPoint)) + digits
	case decimalPoint >= int64(len(digits)):
		canonical = digits + strings.Repeat("0", int(decimalPoint)-len(digits))
	default:
		canonical = digits[:int(decimalPoint)] + "." + digits[int(decimalPoint):]
	}
	if match[1] == "-" {
		canonical = "-" + canonical
	}
	return Decimal(canonical), nil
}
func (d Decimal) MarshalJSON() ([]byte, error) {
	v, err := ParseDecimal(string(d))
	if err != nil {
		return nil, err
	}
	return json.Marshal(string(v))
}
func (d *Decimal) UnmarshalJSON(data []byte) error {
	var s string
	if err := strictDecode(data, &s); err != nil {
		return err
	}
	v, err := ParseDecimal(s)
	if err == nil {
		*d = v
	}
	return err
}

type CalendarDate string

func ParseCalendarDate(s string) (CalendarDate, error) {
	layout := "2006-01-02"
	if len(s) == 19 {
		layout = "2006-01-02T15:04:05"
	} else if len(s) != 10 {
		return "", fmt.Errorf("invalid calendar date")
	}
	parsed, err := time.Parse(layout, s)
	if err != nil || parsed.Year() < 1 || parsed.Year() > 9999 || parsed.Format(layout) != s {
		return "", fmt.Errorf("invalid calendar date %q", s)
	}
	return CalendarDate(s), nil
}
func (d CalendarDate) MarshalJSON() ([]byte, error) {
	if _, err := ParseCalendarDate(string(d)); err != nil {
		return nil, err
	}
	return json.Marshal(string(d))
}
func (d *CalendarDate) UnmarshalJSON(data []byte) error {
	var s string
	if err := strictDecode(data, &s); err != nil {
		return err
	}
	v, err := ParseCalendarDate(s)
	if err == nil {
		*d = v
	}
	return err
}

type Operator string

const (
	Contains       Operator = "contains"
	DoesNotContain Operator = "does not contain"
	Is             Operator = "is"
	IsNot          Operator = "is not"
	IsEmpty        Operator = "is empty"
	IsNotEmpty     Operator = "is not empty"
	Equal          Operator = "="
	NotEqual       Operator = "≠"
	Less           Operator = "<"
	LessEqual      Operator = "≤"
	Greater        Operator = ">"
	GreaterEqual   Operator = "≥"
	IsAnyOf        Operator = "is any of"
	Before         Operator = "before"
	After          Operator = "after"
	OnOrBefore     Operator = "on or before"
	OnOrAfter      Operator = "on or after"
	IsChecked      Operator = "is checked"
	IsNotChecked   Operator = "is not checked"
)

func (op Operator) NeedsValue() bool {
	return op != IsEmpty && op != IsNotEmpty && op != IsChecked && op != IsNotChecked
}
func ValidateOperator(t PropertyType, op Operator) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if t != CheckboxType && (op == IsEmpty || op == IsNotEmpty) {
		return nil
	}
	valid := false
	switch t {
	case TitleType, TextType, URLType:
		valid = op == Contains || op == DoesNotContain || op == Is || op == IsNot
	case NumberType:
		valid = op == Equal || op == NotEqual || op == Less || op == LessEqual || op == Greater || op == GreaterEqual
	case SelectType:
		valid = op == Is || op == IsNot || op == IsAnyOf
	case MultiSelectType:
		valid = op == Contains || op == DoesNotContain
	case DateType:
		valid = op == Is || op == Before || op == After || op == OnOrBefore || op == OnOrAfter
	case CheckboxType:
		valid = op == IsChecked || op == IsNotChecked
	}
	if !valid {
		return fmt.Errorf("operator %q is invalid for %s", op, t)
	}
	return nil
}
func validOperator(op Operator) bool {
	for _, t := range []PropertyType{TitleType, TextType, NumberType, SelectType, MultiSelectType, DateType, CheckboxType, URLType} {
		if ValidateOperator(t, op) == nil {
			return true
		}
	}
	return false
}

type Width struct {
	Property Target `json:"property"`
	PX       int    `json:"px"`
}
type Display struct {
	Layout    string   `json:"-"`
	Visible   []Target `json:"-"`
	Widths    []Width  `json:"-"`
	GroupBy   *Target  `json:"-"`
	ShowEmpty bool     `json:"-"`
}

func (d Display) Validate() error {
	if d.Visible == nil {
		return fmt.Errorf("visible must be an array")
	}
	seen := map[string]bool{}
	for _, p := range d.Visible {
		if err := p.ValidateKind(PropertyKind); err != nil {
			return err
		}
		key := targetKey(p)
		if seen[key] {
			return fmt.Errorf("duplicate visible property")
		}
		seen[key] = true
	}
	switch d.Layout {
	case "table":
		if d.GroupBy != nil || d.ShowEmpty {
			return fmt.Errorf("kanban fields on table")
		}
		if d.Widths == nil {
			return fmt.Errorf("widths must be an array")
		}
		seen = map[string]bool{}
		for _, w := range d.Widths {
			if err := w.Property.ValidateKind(PropertyKind); err != nil {
				return err
			}
			key := targetKey(w.Property)
			if seen[key] {
				return fmt.Errorf("duplicate width property")
			}
			seen[key] = true
			if w.PX < 80 || w.PX > 1200 {
				return fmt.Errorf("width must be 80 through 1200")
			}
		}
	case "kanban":
		if d.Widths != nil {
			return fmt.Errorf("table widths on kanban")
		}
		if d.GroupBy == nil {
			return fmt.Errorf("kanban needs group_by")
		}
		return d.GroupBy.ValidateKind(PropertyKind)
	default:
		return fmt.Errorf("unknown layout %q", d.Layout)
	}
	return nil
}
func (d Display) MarshalJSON() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	v := map[string]any{"layout": d.Layout, "visible": d.Visible}
	if d.Layout == "table" {
		v["widths"] = d.Widths
	} else {
		v["group_by"] = d.GroupBy
		v["show_empty"] = d.ShowEmpty
	}
	return json.Marshal(v)
}
func (d *Display) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return err
	}
	if err := requiredFields(data, "layout", "visible"); err != nil {
		return err
	}
	var layout string
	if err := strictDecode(fields["layout"], &layout); err != nil {
		return err
	}
	v := Display{Layout: layout}
	if err := strictDecode(fields["visible"], &v.Visible); err != nil {
		return err
	}
	switch layout {
	case "table":
		if err := onlyFields(fields, "layout", "visible", "widths"); err != nil {
			return err
		}
		if err := requiredFields(data, "widths"); err != nil {
			return err
		}
		if err := strictDecode(fields["widths"], &v.Widths); err != nil {
			return err
		}
	case "kanban":
		if err := onlyFields(fields, "layout", "visible", "group_by", "show_empty"); err != nil {
			return err
		}
		if err := requiredFields(data, "group_by", "show_empty"); err != nil {
			return err
		}
		var group Target
		if err := strictDecode(fields["group_by"], &group); err != nil {
			return err
		}
		v.GroupBy = &group
		if err := strictDecode(fields["show_empty"], &v.ShowEmpty); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown layout %q", layout)
	}
	if err := v.Validate(); err != nil {
		return err
	}
	*d = v
	return nil
}
func (w *Width) UnmarshalJSON(data []byte) error {
	if err := requiredFields(data, "property", "px"); err != nil {
		return err
	}
	type wire Width
	var v wire
	if err := strictDecode(data, &v); err != nil {
		return err
	}
	*w = Width(v)
	return nil
}
func targetKey(t Target) string {
	if t.Ref != nil {
		return string(t.Ref.ID)
	}
	return "tmp:" + t.Tmp
}

type Sort struct {
	Property  Target `json:"property"`
	Direction string `json:"direction"`
}

func (s *Sort) UnmarshalJSON(data []byte) error {
	if err := requiredFields(data, "property", "direction"); err != nil {
		return err
	}
	type wire Sort
	var v wire
	if err := strictDecode(data, &v); err != nil {
		return err
	}
	*s = Sort(v)
	return s.Validate()
}
func (s Sort) Validate() error {
	if err := s.Property.ValidateKind(PropertyKind); err != nil {
		return err
	}
	if s.Direction != "asc" && s.Direction != "desc" {
		return fmt.Errorf("invalid sort direction")
	}
	return nil
}

type Filter struct {
	Property Target          `json:"property"`
	Op       Operator        `json:"op"`
	Value    json.RawMessage `json:"value,omitempty"`
}

func (f *Filter) UnmarshalJSON(data []byte) error {
	if err := requiredFields(data, "property", "op"); err != nil {
		return err
	}
	type wire Filter
	var v wire
	if err := strictDecode(data, &v); err != nil {
		return err
	}
	if err := Filter(v).Validate(); err != nil {
		return err
	}
	value, err := canonicalFilterValue(v.Op, v.Value)
	if err != nil {
		return err
	}
	v.Value = value
	*f = Filter(v)
	return nil
}
func canonicalFilterValue(op Operator, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	switch op {
	case Equal, NotEqual, Less, LessEqual, Greater, GreaterEqual:
		var d Decimal
		if err := strictDecode(raw, &d); err != nil {
			return nil, err
		}
		return json.Marshal(d)
	default:
		var value any
		if err := strictDecode(raw, &value); err != nil {
			return nil, err
		}
		return CanonicalJSON(value)
	}
}
func (f Filter) MarshalJSON() ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	raw, err := canonicalFilterValue(f.Op, f.Value)
	if err != nil {
		return nil, err
	}
	f.Value = raw
	type wire Filter
	return json.Marshal(wire(f))
}
func (f Filter) Validate() error {
	if err := f.Property.ValidateKind(PropertyKind); err != nil {
		return err
	}
	if !validOperator(f.Op) {
		return fmt.Errorf("unknown operator %q", f.Op)
	}
	if f.Op.NeedsValue() != (len(f.Value) > 0) {
		return fmt.Errorf("operator %q value presence mismatch", f.Op)
	}
	switch f.Op {
	case Equal, NotEqual, Less, LessEqual, Greater, GreaterEqual:
		var d Decimal
		return strictDecode(f.Value, &d)
	case Before, After, OnOrBefore, OnOrAfter:
		var d CalendarDate
		return strictDecode(f.Value, &d)
	case IsAnyOf:
		var values []Target
		if err := strictDecode(f.Value, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := value.ValidateKind(OptionKind); err != nil {
				return err
			}
		}
		return nil
	}
	if len(f.Value) > 0 {
		var v any
		if err := strictDecode(f.Value, &v); err != nil {
			return err
		}
		switch x := v.(type) {
		case string:
			if err := validateText(x); err != nil {
				return err
			}
		case map[string]any:
			var t Target
			if err := strictDecode(f.Value, &t); err != nil {
				return err
			}
			return t.ValidateKind(OptionKind)
		case []any:
			return fmt.Errorf("option sets require is any of")
		default:
			return fmt.Errorf("filter value must be text, decimal, date, or option reference")
		}
	}
	return nil
}
func (f Filter) ValidateFor(t PropertyType) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if err := ValidateOperator(t, f.Op); err != nil {
		return err
	}
	if !f.Op.NeedsValue() {
		return nil
	}
	switch t {
	case TitleType, TextType, URLType:
		var s string
		return strictDecode(f.Value, &s)
	case NumberType:
		var d Decimal
		return strictDecode(f.Value, &d)
	case DateType:
		var d CalendarDate
		return strictDecode(f.Value, &d)
	case SelectType:
		if f.Op == IsAnyOf {
			var ts []Target
			if err := strictDecode(f.Value, &ts); err != nil {
				return err
			}
			if ts == nil {
				return fmt.Errorf("expected option array")
			}
			return nil
		}
		fallthrough
	case MultiSelectType:
		var target Target
		if err := strictDecode(f.Value, &target); err != nil {
			return err
		}
		return target.ValidateKind(OptionKind)
	}
	return fmt.Errorf("invalid filter operand")
}

type ViewDef struct {
	Name    string   `json:"name"`
	Display Display  `json:"display"`
	Sort    []Sort   `json:"sort"`
	Filter  []Filter `json:"filter"`
}

func DecodeViewDef(data []byte) (ViewDef, error) {
	var v ViewDef
	err := v.UnmarshalJSON(data)
	return v, err
}
func (v ViewDef) Validate() error {
	if err := validateText(v.Name); err != nil {
		return err
	}
	if err := v.Display.Validate(); err != nil {
		return err
	}
	if v.Sort == nil || v.Filter == nil {
		return fmt.Errorf("sort and filter must be arrays")
	}
	seen := map[string]bool{}
	for _, s := range v.Sort {
		if err := s.Validate(); err != nil {
			return err
		}
		key := targetKey(s.Property)
		if seen[key] {
			return fmt.Errorf("duplicate sort property")
		}
		seen[key] = true
	}
	for _, f := range v.Filter {
		if err := f.Validate(); err != nil {
			return err
		}
	}
	return nil
}
func (v ViewDef) ValidateSchema(properties map[ID]PropertyDef) error {
	if err := v.Validate(); err != nil {
		return err
	}
	lookup := func(t Target) (PropertyDef, error) {
		if t.Ref == nil {
			return PropertyDef{}, fmt.Errorf("unresolved temporary property")
		}
		p, ok := properties[t.Ref.ID]
		if !ok {
			return p, fmt.Errorf("missing property %s", t.Ref.ID)
		}
		return p, nil
	}
	visible := map[ID]bool{}
	for _, t := range v.Display.Visible {
		if _, err := lookup(t); err != nil {
			return err
		}
		visible[t.Ref.ID] = true
	}
	if v.Display.Layout == "table" {
		for id, p := range properties {
			if p.Type == TitleType && !visible[id] {
				return fmt.Errorf("table title property must be visible")
			}
		}
	}
	for _, w := range v.Display.Widths {
		if _, err := lookup(w.Property); err != nil {
			return err
		}
	}
	if v.Display.GroupBy != nil {
		p, err := lookup(*v.Display.GroupBy)
		if err != nil {
			return err
		}
		if p.Type != SelectType {
			return fmt.Errorf("kanban group must be select")
		}
	}
	for _, s := range v.Sort {
		if _, err := lookup(s.Property); err != nil {
			return err
		}
	}
	for _, f := range v.Filter {
		p, err := lookup(f.Property)
		if err != nil {
			return err
		}
		if err := f.ValidateFor(p.Type); err != nil {
			return err
		}
	}
	return nil
}
func (v ViewDef) MarshalJSON() ([]byte, error) {
	if err := v.Validate(); err != nil {
		return nil, err
	}
	type wire ViewDef
	return json.Marshal(wire(v))
}
func (v *ViewDef) UnmarshalJSON(data []byte) error {
	if err := requiredFields(data, "name", "display", "sort", "filter"); err != nil {
		return err
	}
	type wire ViewDef
	var w wire
	if err := strictDecode(data, &w); err != nil {
		return err
	}
	if err := ViewDef(w).Validate(); err != nil {
		return err
	}
	*v = ViewDef(w)
	return nil
}

type CellValue struct {
	Type  PropertyType `json:"type"`
	Value any          `json:"value"`
}

func DecodeCellValue(data []byte) (CellValue, error) {
	var v CellValue
	err := v.UnmarshalJSON(data)
	return v, err
}
func EncodeCellValue(v CellValue) ([]byte, error) { return CanonicalJSON(v) }
func (v CellValue) Canonical() (CellValue, error) {
	if err := v.Type.Validate(); err != nil {
		return CellValue{}, err
	}
	if v.Value == nil {
		switch v.Type {
		case NumberType, SelectType, DateType, URLType:
			return v, nil
		default:
			return CellValue{}, fmt.Errorf("null is not valid for %s", v.Type)
		}
	}
	switch v.Type {
	case TitleType, TextType:
		s, ok := v.Value.(string)
		if !ok {
			return CellValue{}, fmt.Errorf("text value must be a string")
		}
		if err := validateText(s); err != nil {
			return CellValue{}, err
		}
	case NumberType:
		var s string
		switch x := v.Value.(type) {
		case string:
			s = x
		case Decimal:
			s = string(x)
		default:
			return CellValue{}, fmt.Errorf("number value must be a decimal string")
		}
		d, err := ParseDecimal(s)
		if err != nil {
			return CellValue{}, err
		}
		v.Value = d
	case SelectType:
		var id ID
		switch x := v.Value.(type) {
		case ID:
			id = x
		case string:
			id = ID(x)
		default:
			return CellValue{}, fmt.Errorf("select value must be an option id")
		}
		if err := id.ValidateKind(OptionKind); err != nil {
			return CellValue{}, err
		}
		v.Value = id
	case MultiSelectType:
		ids, ok := v.Value.([]ID)
		if !ok {
			return CellValue{}, fmt.Errorf("multi-select value must be option ids")
		}
		ids = append([]ID{}, ids...)
		for _, id := range ids {
			if err := id.ValidateKind(OptionKind); err != nil {
				return CellValue{}, err
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		unique := ids[:0]
		for _, id := range ids {
			if len(unique) == 0 || unique[len(unique)-1] != id {
				unique = append(unique, id)
			}
		}
		v.Value = unique
	case DateType:
		var s string
		switch x := v.Value.(type) {
		case string:
			s = x
		case CalendarDate:
			s = string(x)
		default:
			return CellValue{}, fmt.Errorf("date value must be a calendar string")
		}
		d, err := ParseCalendarDate(s)
		if err != nil {
			return CellValue{}, err
		}
		v.Value = d
	case CheckboxType:
		if _, ok := v.Value.(bool); !ok {
			return CellValue{}, fmt.Errorf("checkbox value must be boolean")
		}
	case URLType:
		s, ok := v.Value.(string)
		if !ok {
			return CellValue{}, fmt.Errorf("URL value must be a string")
		}
		if err := validateURL(s, false); err != nil {
			return CellValue{}, err
		}
	}
	return v, nil
}
func (v CellValue) Validate() error { _, err := v.Canonical(); return err }
func (v CellValue) MarshalJSON() ([]byte, error) {
	c, err := v.Canonical()
	if err != nil {
		return nil, err
	}
	type wire CellValue
	return json.Marshal(wire(c))
}
func (v *CellValue) UnmarshalJSON(data []byte) error {
	if err := requiredFields(data, "type"); err != nil {
		return err
	}
	var w struct {
		Type  PropertyType    `json:"type"`
		Value json.RawMessage `json:"value"`
	}
	if err := strictDecode(data, &w); err != nil {
		return err
	}
	if len(w.Value) == 0 {
		return fmt.Errorf("missing value")
	}
	c := CellValue{Type: w.Type}
	if !bytes.Equal(bytes.TrimSpace(w.Value), []byte("null")) {
		var err error
		switch w.Type {
		case TitleType, TextType, URLType:
			var x string
			err = strictDecode(w.Value, &x)
			c.Value = x
		case NumberType:
			var x Decimal
			err = strictDecode(w.Value, &x)
			c.Value = x
		case SelectType:
			var x ID
			err = strictDecode(w.Value, &x)
			c.Value = x
		case MultiSelectType:
			var x []ID
			err = strictDecode(w.Value, &x)
			c.Value = x
		case DateType:
			var x CalendarDate
			err = strictDecode(w.Value, &x)
			c.Value = x
		case CheckboxType:
			var x bool
			err = strictDecode(w.Value, &x)
			c.Value = x
		default:
			return fmt.Errorf("unknown cell type %q", w.Type)
		}
		if err != nil {
			return err
		}
	}
	canonical, err := c.Canonical()
	if err != nil {
		return err
	}
	*v = canonical
	return nil
}
