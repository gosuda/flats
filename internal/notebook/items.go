package notebook

import (
	"encoding/json"
	"fmt"
	"reflect"
)

type ItemData interface {
	itemKind() Kind
	Validate() error
}
type Page struct {
	Title    string `json:"title"`
	Parent   *ID    `json:"parent"`
	Position string `json:"position"`
}
type Block struct {
	Page     ID      `json:"page"`
	Parent   *ID     `json:"parent"`
	Position string  `json:"position"`
	Content  Content `json:"content"`
}
type Database struct {
	Block         ID     `json:"block"`
	Title         string `json:"title"`
	PropertyOrder []ID   `json:"property_order"`
	ViewOrder     []ID   `json:"view_order"`
}
type Property struct {
	Database    ID          `json:"database"`
	Definition  PropertyDef `json:"definition"`
	OptionOrder []ID        `json:"option_order"`
}
type Option struct {
	Property ID     `json:"property"`
	Name     string `json:"name"`
	Color    Color  `json:"color"`
}
type Row struct {
	Database ID               `json:"database"`
	Position string           `json:"position"`
	Cells    map[ID]CellValue `json:"cells"`
}
type View struct {
	Database   ID         `json:"database"`
	Definition StoredView `json:"definition"`
}

func (Page) itemKind() Kind     { return PageKind }
func (Block) itemKind() Kind    { return BlockKind }
func (Database) itemKind() Kind { return DatabaseKind }
func (Property) itemKind() Kind { return PropertyKind }
func (Option) itemKind() Kind   { return OptionKind }
func (Row) itemKind() Kind      { return RowKind }
func (View) itemKind() Kind     { return ViewKind }
func (p Page) Validate() error {
	if err := validateText(p.Title); err != nil {
		return err
	}
	if p.Parent != nil {
		if err := p.Parent.ValidateKind(PageKind); err != nil {
			return err
		}
	}
	return validatePosition(p.Position)
}
func (b Block) Validate() error {
	if err := b.Page.ValidateKind(PageKind); err != nil {
		return err
	}
	if b.Parent != nil {
		if err := b.Parent.ValidateKind(BlockKind); err != nil {
			return err
		}
	}
	if err := validatePosition(b.Position); err != nil {
		return err
	}
	return b.Content.Validate()
}
func (d Database) Validate() error {
	if err := d.Block.ValidateKind(BlockKind); err != nil {
		return err
	}
	if err := validateText(d.Title); err != nil {
		return err
	}
	if err := validateIDs(d.PropertyOrder, PropertyKind); err != nil {
		return err
	}
	return validateIDs(d.ViewOrder, ViewKind)
}
func (p Property) Validate() error {
	if err := p.Database.ValidateKind(DatabaseKind); err != nil {
		return err
	}
	if err := p.Definition.Validate(); err != nil {
		return err
	}
	if err := validateIDs(p.OptionOrder, OptionKind); err != nil {
		return err
	}
	if p.Definition.Type != SelectType && p.Definition.Type != MultiSelectType && len(p.OptionOrder) > 0 {
		return fmt.Errorf("options require select or multi_select")
	}
	return nil
}
func (o Option) Validate() error {
	if err := o.Property.ValidateKind(PropertyKind); err != nil {
		return err
	}
	if err := validateText(o.Name); err != nil {
		return err
	}
	return o.Color.Validate()
}
func (r Row) Validate() error {
	if err := r.Database.ValidateKind(DatabaseKind); err != nil {
		return err
	}
	if err := validatePosition(r.Position); err != nil {
		return err
	}
	if r.Cells == nil {
		return fmt.Errorf("cells must be an object")
	}
	for id, value := range r.Cells {
		if err := id.ValidateKind(PropertyKind); err != nil {
			return err
		}
		if err := value.Validate(); err != nil {
			return fmt.Errorf("cell %s: %w", id, err)
		}
	}
	return nil
}
func (v View) Validate() error {
	if err := v.Database.ValidateKind(DatabaseKind); err != nil {
		return err
	}
	return v.Definition.Validate()
}
func validatePosition(s string) error {
	if len(s) == 0 {
		return fmt.Errorf("invalid position")
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			return fmt.Errorf("invalid position")
		}
	}
	return nil
}
func validateIDs(ids []ID, kind Kind) error {
	if ids == nil {
		return fmt.Errorf("%s order must be an array", kind)
	}
	seen := map[ID]bool{}
	for _, id := range ids {
		if err := id.ValidateKind(kind); err != nil {
			return err
		}
		if seen[id] {
			return fmt.Errorf("duplicate id %s", id)
		}
		seen[id] = true
	}
	return nil
}

type Item struct {
	Ref  Ref                  `json:"ref"`
	Ver  Version              `json:"ver"`
	V    map[GroupKey]Version `json:"v"`
	Data ItemData             `json:"-"`
}

func (i Item) ID() ID                   { return i.Ref.ID }
func (i Item) Kind() Kind               { return i.Ref.ID.Kind() }
func (i Item) Incarnation() Incarnation { return i.Ref.Inc }
func (i Item) Version() Version         { return i.Ver }

// Creation assigns every group the incarnation; v only needs explicit later writes.
func (i Item) GroupVersion(key GroupKey) (Version, error) {
	if err := ValidateGroup(i.Kind(), key); err != nil {
		return 0, err
	}
	if version, ok := i.V[key]; ok {
		return version, nil
	}
	return Version(i.Ref.Inc), nil
}
func (i Item) Validate() error {
	if err := i.Ref.Validate(); err != nil {
		return err
	}
	if i.Ref.Inc == 0 {
		return fmt.Errorf("stored item incarnation must be positive")
	}
	if i.Ver < Version(i.Ref.Inc) {
		return fmt.Errorf("item version precedes incarnation")
	}
	if i.Data == nil || (reflect.ValueOf(i.Data).Kind() == reflect.Pointer && reflect.ValueOf(i.Data).IsNil()) {
		return fmt.Errorf("missing item data")
	}
	if i.Data.itemKind() != i.Kind() {
		return fmt.Errorf("item data does not match id kind")
	}
	if err := i.Data.Validate(); err != nil {
		return err
	}
	if i.V == nil {
		return fmt.Errorf("group versions must be an object")
	}
	for key, version := range i.V {
		if err := ValidateGroup(i.Kind(), key); err != nil {
			return err
		}
		if version < Version(i.Ref.Inc) || version > i.Ver {
			return fmt.Errorf("group %s version outside item lifetime", key)
		}
	}
	return nil
}
func DecodeItem(data []byte) (Item, error) { var i Item; err := i.UnmarshalJSON(data); return i, err }
func EncodeItem(i Item) ([]byte, error)    { return CanonicalJSON(i) }
func (i Item) MarshalJSON() ([]byte, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(i.Data)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return nil, err
	}
	add := func(name string, v any) error {
		raw, err := json.Marshal(v)
		if err == nil {
			fields[name] = raw
		}
		return err
	}
	if err := add("ref", i.Ref); err != nil {
		return nil, err
	}
	if err := add("ver", i.Ver); err != nil {
		return nil, err
	}
	if err := add("v", i.V); err != nil {
		return nil, err
	}
	if err := add("kind", i.Kind()); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}
func (i *Item) UnmarshalJSON(data []byte) error {
	if err := requiredFields(data, "ref", "ver", "v", "kind"); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return err
	}
	v := Item{}
	if err := strictDecode(fields["ref"], &v.Ref); err != nil {
		return err
	}
	if err := strictDecode(fields["ver"], &v.Ver); err != nil {
		return err
	}
	if err := strictDecode(fields["v"], &v.V); err != nil {
		return err
	}
	var kind Kind
	if err := strictDecode(fields["kind"], &kind); err != nil {
		return err
	}
	if kind != v.Kind() {
		return fmt.Errorf("item kind does not match ref")
	}
	for _, name := range []string{"ref", "ver", "v", "kind"} {
		delete(fields, name)
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	var required []string
	switch kind {
	case PageKind:
		v.Data = &Page{}
		required = []string{"title", "position"}
		if _, ok := fields["parent"]; !ok {
			return fmt.Errorf("missing parent")
		}
	case BlockKind:
		v.Data = &Block{}
		required = []string{"page", "position", "content"}
		if _, ok := fields["parent"]; !ok {
			return fmt.Errorf("missing parent")
		}
	case DatabaseKind:
		v.Data = &Database{}
		required = []string{"block", "title", "property_order", "view_order"}
	case PropertyKind:
		v.Data = &Property{}
		required = []string{"database", "definition", "option_order"}
	case OptionKind:
		v.Data = &Option{}
		required = []string{"property", "name", "color"}
	case RowKind:
		v.Data = &Row{}
		required = []string{"database", "position", "cells"}
	case ViewKind:
		v.Data = &View{}
		required = []string{"database", "definition"}
	default:
		return fmt.Errorf("unknown item kind %q", kind)
	}
	if err := requiredFields(payload, required...); err != nil {
		return err
	}
	if err := strictDecode(payload, v.Data); err != nil {
		return err
	}
	if err := v.Validate(); err != nil {
		return err
	}
	// Keep one representation regardless of whether the caller decoded or constructed an item.
	switch p := v.Data.(type) {
	case *Page:
		v.Data = *p
	case *Block:
		v.Data = *p
	case *Database:
		v.Data = *p
	case *Property:
		v.Data = *p
	case *Option:
		v.Data = *p
	case *Row:
		v.Data = *p
	case *View:
		v.Data = *p
	}
	*i = v
	return nil
}

type StoredWidth struct {
	Property ID  `json:"property"`
	PX       int `json:"px"`
}
type StoredDisplay struct {
	Layout    string        `json:"layout"`
	Visible   []ID          `json:"visible"`
	Widths    []StoredWidth `json:"widths,omitempty"`
	GroupBy   *ID           `json:"group_by,omitempty"`
	ShowEmpty bool          `json:"show_empty,omitempty"`
}
type StoredSort struct {
	Property  ID     `json:"property"`
	Direction string `json:"direction"`
}
type StoredFilter struct {
	Property ID              `json:"property"`
	Op       Operator        `json:"op"`
	Value    json.RawMessage `json:"value,omitempty"`
}
type StoredView struct {
	Name    string         `json:"name"`
	Display StoredDisplay  `json:"display"`
	Sort    []StoredSort   `json:"sort"`
	Filter  []StoredFilter `json:"filter"`
}

func (d StoredDisplay) MarshalJSON() ([]byte, error) {
	if err := (StoredView{Display: d, Sort: []StoredSort{}, Filter: []StoredFilter{}}).Validate(); err != nil {
		return nil, err
	}
	v := map[string]any{"layout": d.Layout, "visible": d.Visible}
	if d.Layout == "table" {
		widths := d.Widths
		if widths == nil {
			widths = []StoredWidth{}
		}
		v["widths"] = widths
	} else {
		v["group_by"] = d.GroupBy
		v["show_empty"] = d.ShowEmpty
	}
	return json.Marshal(v)
}
func (d *StoredDisplay) UnmarshalJSON(data []byte) error {
	if err := requiredFields(data, "layout", "visible"); err != nil {
		return err
	}
	type wire StoredDisplay
	var v wire
	if err := strictDecode(data, &v); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return err
	}
	switch v.Layout {
	case "table":
		if err := onlyFields(fields, "layout", "visible", "widths"); err != nil {
			return err
		}
		if err := requiredFields(data, "widths"); err != nil {
			return err
		}
	case "kanban":
		if err := onlyFields(fields, "layout", "visible", "group_by", "show_empty"); err != nil {
			return err
		}
		if err := requiredFields(data, "group_by", "show_empty"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown stored layout %q", v.Layout)
	}
	if err := (StoredView{Display: StoredDisplay(v), Sort: []StoredSort{}, Filter: []StoredFilter{}}).Validate(); err != nil {
		return err
	}
	*d = StoredDisplay(v)
	return nil
}
func (w *StoredWidth) UnmarshalJSON(data []byte) error {
	if err := requiredFields(data, "property", "px"); err != nil {
		return err
	}
	type wire StoredWidth
	var v wire
	if err := strictDecode(data, &v); err != nil {
		return err
	}
	*w = StoredWidth(v)
	return nil
}
func (s *StoredSort) UnmarshalJSON(data []byte) error {
	if err := requiredFields(data, "property", "direction"); err != nil {
		return err
	}
	type wire StoredSort
	var v wire
	if err := strictDecode(data, &v); err != nil {
		return err
	}
	*s = StoredSort(v)
	return nil
}
func (f *StoredFilter) UnmarshalJSON(data []byte) error {
	if err := requiredFields(data, "property", "op"); err != nil {
		return err
	}
	type wire StoredFilter
	var v wire
	if err := strictDecode(data, &v); err != nil {
		return err
	}
	raw, err := canonicalFilterValue(v.Op, v.Value)
	if err != nil {
		return err
	}
	v.Value = raw
	*f = StoredFilter(v)
	return nil
}
func (f StoredFilter) MarshalJSON() ([]byte, error) {
	raw, err := canonicalFilterValue(f.Op, f.Value)
	if err != nil {
		return nil, err
	}
	f.Value = raw
	type wire StoredFilter
	return json.Marshal(wire(f))
}

func (s StoredView) Validate() error {
	_, err := s.Wire(func(id ID) (Ref, error) { return Ref{ID: id, Inc: 1}, nil })
	return err
}
func (s StoredView) Wire(resolve func(ID) (Ref, error)) (ViewDef, error) {
	target := func(id ID) (Target, error) {
		r, err := resolve(id)
		if err != nil {
			return Target{}, err
		}
		if r.ID != id {
			return Target{}, fmt.Errorf("resolved wrong id")
		}
		return Target{Ref: &r}, nil
	}
	v := ViewDef{Name: s.Name, Display: Display{Layout: s.Display.Layout, Visible: []Target{}}, Sort: []Sort{}, Filter: []Filter{}}
	if s.Display.Visible == nil || s.Sort == nil || s.Filter == nil {
		return v, fmt.Errorf("stored view arrays cannot be null")
	}
	for _, id := range s.Display.Visible {
		t, err := target(id)
		if err != nil {
			return v, err
		}
		v.Display.Visible = append(v.Display.Visible, t)
	}
	if s.Display.Layout == "table" {
		v.Display.Widths = []Width{}
		if s.Display.GroupBy != nil || s.Display.ShowEmpty {
			return v, fmt.Errorf("kanban fields on stored table")
		}
	} else if s.Display.Widths != nil {
		return v, fmt.Errorf("widths on stored kanban")
	}
	for _, w := range s.Display.Widths {
		t, err := target(w.Property)
		if err != nil {
			return v, err
		}
		v.Display.Widths = append(v.Display.Widths, Width{Property: t, PX: w.PX})
	}
	if s.Display.GroupBy != nil {
		t, err := target(*s.Display.GroupBy)
		if err != nil {
			return v, err
		}
		v.Display.GroupBy = &t
	}
	v.Display.ShowEmpty = s.Display.ShowEmpty
	for _, sort := range s.Sort {
		t, err := target(sort.Property)
		if err != nil {
			return v, err
		}
		v.Sort = append(v.Sort, Sort{Property: t, Direction: sort.Direction})
	}
	for _, f := range s.Filter {
		t, err := target(f.Property)
		if err != nil {
			return v, err
		}
		value, err := storedFilterToWire(f.Value, target)
		if err != nil {
			return v, err
		}
		v.Filter = append(v.Filter, Filter{Property: t, Op: f.Op, Value: value})
	}
	return v, v.Validate()
}
func StoreView(v ViewDef) (StoredView, error) {
	if err := v.Validate(); err != nil {
		return StoredView{}, err
	}
	id := func(t Target) (ID, error) {
		if t.Ref == nil {
			return "", fmt.Errorf("cannot store unresolved temporary target")
		}
		return t.Ref.ID, nil
	}
	s := StoredView{Name: v.Name, Display: StoredDisplay{Layout: v.Display.Layout, Visible: []ID{}, ShowEmpty: v.Display.ShowEmpty}, Sort: []StoredSort{}, Filter: []StoredFilter{}}
	if v.Display.Layout == "table" {
		s.Display.Widths = []StoredWidth{}
	}
	for _, t := range v.Display.Visible {
		p, err := id(t)
		if err != nil {
			return s, err
		}
		s.Display.Visible = append(s.Display.Visible, p)
	}
	for _, w := range v.Display.Widths {
		p, err := id(w.Property)
		if err != nil {
			return s, err
		}
		s.Display.Widths = append(s.Display.Widths, StoredWidth{Property: p, PX: w.PX})
	}
	if v.Display.GroupBy != nil {
		p, err := id(*v.Display.GroupBy)
		if err != nil {
			return s, err
		}
		s.Display.GroupBy = &p
	}
	for _, sort := range v.Sort {
		p, err := id(sort.Property)
		if err != nil {
			return s, err
		}
		s.Sort = append(s.Sort, StoredSort{Property: p, Direction: sort.Direction})
	}
	for _, f := range v.Filter {
		p, err := id(f.Property)
		if err != nil {
			return s, err
		}
		value, err := wireFilterToStored(f.Value, id)
		if err != nil {
			return s, err
		}
		s.Filter = append(s.Filter, StoredFilter{Property: p, Op: f.Op, Value: value})
	}
	return s, s.Validate()
}
func wireFilterToStored(raw json.RawMessage, id func(Target) (ID, error)) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value any
	if err := strictDecode(raw, &value); err != nil {
		return nil, err
	}
	convert := func(raw []byte) (any, error) {
		var t Target
		if err := strictDecode(raw, &t); err != nil {
			return nil, err
		}
		p, err := id(t)
		if err != nil {
			return nil, err
		}
		return struct {
			Option ID `json:"option"`
		}{p}, nil
	}
	switch value.(type) {
	case map[string]any:
		v, err := convert(raw)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)
	case []any:
		var entries []json.RawMessage
		if err := strictDecode(raw, &entries); err != nil {
			return nil, err
		}
		values := []any{}
		for _, entry := range entries {
			v, err := convert(entry)
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		return json.Marshal(values)
	default:
		return append(json.RawMessage{}, raw...), nil
	}
}
func storedFilterToWire(raw json.RawMessage, target func(ID) (Target, error)) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value any
	if err := strictDecode(raw, &value); err != nil {
		return nil, err
	}
	convert := func(raw []byte) (Target, error) {
		var w struct {
			Option ID `json:"option"`
		}
		if err := requiredFields(raw, "option"); err != nil {
			return Target{}, err
		}
		if err := strictDecode(raw, &w); err != nil {
			return Target{}, err
		}
		if err := w.Option.ValidateKind(OptionKind); err != nil {
			return Target{}, err
		}
		return target(w.Option)
	}
	switch value.(type) {
	case map[string]any:
		v, err := convert(raw)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)
	case []any:
		var entries []json.RawMessage
		if err := strictDecode(raw, &entries); err != nil {
			return nil, err
		}
		values := []Target{}
		for _, entry := range entries {
			v, err := convert(entry)
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		return json.Marshal(values)
	default:
		return append(json.RawMessage{}, raw...), nil
	}
}
func (s StoredView) MarshalJSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	type wire StoredView
	return json.Marshal(wire(s))
}
func (s *StoredView) UnmarshalJSON(data []byte) error {
	if err := requiredFields(data, "name", "display", "sort", "filter"); err != nil {
		return err
	}
	type wire StoredView
	var v wire
	if err := strictDecode(data, &v); err != nil {
		return err
	}
	if err := StoredView(v).Validate(); err != nil {
		return err
	}
	*s = StoredView(v)
	return nil
}
