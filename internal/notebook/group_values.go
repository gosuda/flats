package notebook

import (
	"encoding/json"
	"fmt"
)

type GroupValue interface{ notebookGroupValue() }

type TitleValue string
type PagePlace struct {
	Parent   *ID    `json:"parent"`
	Position string `json:"position"`
}
type BlockPlace struct {
	Page     ID     `json:"page"`
	Parent   *ID    `json:"parent"`
	Position string `json:"position"`
}
type RowPlace struct {
	Position string `json:"position"`
}
type OptionDefinition struct {
	Name  string `json:"name"`
	Color Color  `json:"color"`
}
type PropertyOrder []ID
type OptionOrder []ID
type ViewOrder []ID

func (TitleValue) notebookGroupValue()       {}
func (PagePlace) notebookGroupValue()        {}
func (BlockPlace) notebookGroupValue()       {}
func (RowPlace) notebookGroupValue()         {}
func (OptionDefinition) notebookGroupValue() {}
func (PropertyOrder) notebookGroupValue()    {}
func (OptionOrder) notebookGroupValue()      {}
func (ViewOrder) notebookGroupValue()        {}
func (Content) notebookGroupValue()          {}
func (PropertyDef) notebookGroupValue()      {}
func (StoredView) notebookGroupValue()       {}
func (CellValue) notebookGroupValue()        {}

func DecodeGroup(kind Kind, key GroupKey, data []byte) (GroupValue, error) {
	if err := ValidateGroup(kind, key); err != nil {
		return nil, err
	}
	switch key {
	case TitleGroup:
		var title string
		if err := strictDecode(data, &title); err != nil {
			return nil, err
		}
		if err := validateText(title); err != nil {
			return nil, err
		}
		return TitleValue(title), nil
	case ContentGroup:
		return DecodeContent(data)
	case DefinitionGroup:
		switch kind {
		case PropertyKind:
			return DecodePropertyDef(data)
		case OptionKind:
			var def OptionDefinition
			if err := requiredFields(data, "name", "color"); err != nil {
				return nil, err
			}
			if err := strictDecode(data, &def); err != nil {
				return nil, err
			}
			if err := validateText(def.Name); err != nil {
				return nil, err
			}
			return def, def.Color.Validate()
		case ViewKind:
			var def StoredView
			err := strictDecode(data, &def)
			return def, err
		}
	case PlaceGroup:
		switch kind {
		case PageKind:
			var place PagePlace
			if err := requiredNullableFields(data, []string{"parent"}, "position"); err != nil {
				return nil, err
			}
			if err := strictDecode(data, &place); err != nil {
				return nil, err
			}
			return place, (Page{Parent: place.Parent, Position: place.Position}).Validate()
		case BlockKind:
			var place BlockPlace
			if err := requiredNullableFields(data, []string{"parent"}, "page", "position"); err != nil {
				return nil, err
			}
			if err := strictDecode(data, &place); err != nil {
				return nil, err
			}
			return place, (Block{Page: place.Page, Parent: place.Parent, Position: place.Position, Content: Content{Kind: Divider}}).Validate()
		case RowKind:
			var place RowPlace
			if err := requiredFields(data, "position"); err != nil {
				return nil, err
			}
			if err := strictDecode(data, &place); err != nil {
				return nil, err
			}
			return place, validatePosition(place.Position)
		}
	case PropertyOrderGroup, OptionOrderGroup, ViewOrderGroup:
		var ids []ID
		if err := strictDecode(data, &ids); err != nil {
			return nil, err
		}
		switch key {
		case PropertyOrderGroup:
			return PropertyOrder(ids), validateIDs(ids, PropertyKind)
		case OptionOrderGroup:
			return OptionOrder(ids), validateIDs(ids, OptionKind)
		case ViewOrderGroup:
			return ViewOrder(ids), validateIDs(ids, ViewKind)
		}
	default:
		return DecodeCellValue(data)
	}
	return nil, fmt.Errorf("invalid group %s/%s", kind, key)
}

func EncodeGroup(kind Kind, key GroupKey, value GroupValue) ([]byte, error) {
	if err := ValidateGroup(kind, key); err != nil {
		return nil, err
	}
	valid := false
	switch value := value.(type) {
	case TitleValue:
		if err := validateText(string(value)); err != nil {
			return nil, err
		}
		valid = key == TitleGroup
	case PagePlace:
		valid = kind == PageKind && key == PlaceGroup
	case BlockPlace:
		valid = kind == BlockKind && key == PlaceGroup
	case RowPlace:
		valid = kind == RowKind && key == PlaceGroup
	case Content:
		valid = key == ContentGroup
	case PropertyDef:
		valid = kind == PropertyKind && key == DefinitionGroup
	case OptionDefinition:
		if err := validateText(value.Name); err != nil {
			return nil, err
		}
		valid = kind == OptionKind && key == DefinitionGroup
	case StoredView:
		valid = kind == ViewKind && key == DefinitionGroup
	case CellValue:
		valid = kind == RowKind && key != PlaceGroup
	case PropertyOrder:
		valid = key == PropertyOrderGroup
	case OptionOrder:
		valid = key == OptionOrderGroup
	case ViewOrder:
		valid = key == ViewOrderGroup
	}
	if !valid {
		return nil, fmt.Errorf("wrong value for %s/%s", kind, key)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	canonical, err := DecodeGroup(kind, key, data)
	if err != nil {
		return nil, err
	}
	return CanonicalJSON(canonical)
}

func requiredNullableFields(data []byte, nullable []string, required ...string) error {
	if err := requiredFields(data, required...); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return err
	}
	for _, name := range nullable {
		if _, ok := fields[name]; !ok {
			return fmt.Errorf("missing %s", name)
		}
	}
	return nil
}
