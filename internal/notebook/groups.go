package notebook

import (
	"fmt"
	"strings"
)

type GroupKey string

const (
	TitleGroup         GroupKey = "title"
	PlaceGroup         GroupKey = "place"
	ContentGroup       GroupKey = "content"
	PropertyOrderGroup GroupKey = "property_order"
	ViewOrderGroup     GroupKey = "view_order"
	DefinitionGroup    GroupKey = "definition"
	OptionOrderGroup   GroupKey = "option_order"
)

func CellGroup(property ID) GroupKey { return GroupKey("cell:" + string(property)) }
func ValidateGroup(kind Kind, key GroupKey) error {
	valid := false
	switch kind {
	case PageKind:
		valid = key == TitleGroup || key == PlaceGroup
	case BlockKind:
		valid = key == ContentGroup || key == PlaceGroup
	case DatabaseKind:
		valid = key == TitleGroup || key == PropertyOrderGroup || key == ViewOrderGroup
	case PropertyKind:
		valid = key == DefinitionGroup || key == OptionOrderGroup
	case OptionKind, ViewKind:
		valid = key == DefinitionGroup
	case RowKind:
		if key == PlaceGroup {
			return nil
		}
		if strings.HasPrefix(string(key), "cell:") {
			return ID(strings.TrimPrefix(string(key), "cell:")).ValidateKind(PropertyKind)
		}
	}
	if !valid {
		return fmt.Errorf("invalid %s group %q", kind, key)
	}
	return nil
}
