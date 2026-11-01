package notebook

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

func newOperation(name string) Operation {
	switch name {
	case "create_page":
		return &CreatePageOp{}
	case "set_page_title":
		return &SetPageTitleOp{}
	case "move_page":
		return &MovePageOp{}
	case "delete_page":
		return &DeletePageOp{}
	case "duplicate_page":
		return &DuplicatePageOp{}
	case "create_block":
		return &CreateBlockOp{}
	case "set_block":
		return &SetBlockOp{}
	case "turn_blocks":
		return &TurnBlocksOp{}
	case "move_blocks":
		return &MoveBlocksOp{}
	case "delete_blocks":
		return &DeleteBlocksOp{}
	case "duplicate_blocks":
		return &DuplicateBlocksOp{}
	case "create_database":
		return &CreateDatabaseOp{}
	case "set_database_title":
		return &SetDatabaseTitleOp{}
	case "create_property":
		return &CreatePropertyOp{}
	case "set_property":
		return &SetPropertyOp{}
	case "delete_property":
		return &DeletePropertyOp{}
	case "set_property_order":
		return &SetPropertyOrderOp{}
	case "create_option":
		return &CreateOptionOp{}
	case "set_option":
		return &SetOptionOp{}
	case "delete_option":
		return &DeleteOptionOp{}
	case "set_option_order":
		return &SetOptionOrderOp{}
	case "create_row":
		return &CreateRowOp{}
	case "set_cell":
		return &SetCellOp{}
	case "move_rows":
		return &MoveRowsOp{}
	case "delete_rows":
		return &DeleteRowsOp{}
	case "create_view":
		return &CreateViewOp{}
	case "set_view":
		return &SetViewOp{}
	case "delete_view":
		return &DeleteViewOp{}
	case "set_view_order":
		return &SetViewOrderOp{}
	case "markdown_to_blocks":
		return &MarkdownToBlocksOp{}
	case "undo":
		return &UndoOp{}
	case "redo":
		return &RedoOp{}
	default:
		return nil
	}
}

func DecodeOperation(data []byte, browser bool) (Operation, error) {
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return nil, err
	}
	var name string
	if err := strictDecode(fields["op"], &name); err != nil {
		return nil, fmt.Errorf("op: %w", err)
	}
	op := newOperation(name)
	if op == nil {
		return nil, fmt.Errorf("unknown operation %q", name)
	}
	delete(fields, "op")
	if !browser {
		for _, key := range []string{"unit_id", "structure", "proposed_id", "block_proposed_id", "title_property_proposed_id", "view_proposed_id", "proposed_ids"} {
			if _, ok := fields[key]; ok {
				return nil, fmt.Errorf("%s is browser-only", key)
			}
		}
	}
	if err := operationRequired(fields, reflect.TypeOf(op).Elem()); err != nil {
		return nil, err
	}
	if err := decodeOperationFields(fields, reflect.ValueOf(op).Elem()); err != nil {
		return nil, err
	}
	if err := validateOperation(op, browser); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return op, nil
}

func EncodeOperation(op Operation, browser bool) ([]byte, error) {
	if op == nil || (reflect.ValueOf(op).Kind() == reflect.Pointer && reflect.ValueOf(op).IsNil()) {
		return nil, fmt.Errorf("nil operation")
	}
	registered := newOperation(op.OperationName())
	if registered == nil {
		return nil, fmt.Errorf("unknown operation")
	}
	typ := reflect.TypeOf(op)
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ != reflect.TypeOf(registered).Elem() {
		return nil, fmt.Errorf("unregistered operation type %T", op)
	}
	if !browser && (op.operationMeta().UnitID != nil || op.operationMeta().Structure != nil) {
		return nil, fmt.Errorf("compound metadata is browser-only")
	}
	value := reflect.Indirect(reflect.ValueOf(op))
	if field := value.FieldByName("ProposedIDs"); !browser && field.IsValid() && !field.IsNil() {
		return nil, fmt.Errorf("proposed_ids is browser-only")
	}
	if reflect.ValueOf(op).Kind() != reflect.Pointer {
		copy := reflect.New(value.Type())
		copy.Elem().Set(value)
		op = copy.Interface().(Operation)
	}
	if err := validateOperation(op, browser); err != nil {
		return nil, err
	}
	body, err := json.Marshal(op)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	fields["op"], _ = json.Marshal(op.OperationName())
	if op.operationMeta().Structure != nil {
		fields["structure"], _ = json.Marshal(op.operationMeta().Structure)
	}
	if browser && op.OperationName() == "markdown_to_blocks" {
		value := reflect.Indirect(reflect.ValueOf(op)).FieldByName("ProposedIDs")
		if !value.IsNil() {
			fields["proposed_ids"], err = json.Marshal(value.Interface())
			if err != nil {
				return nil, err
			}
		}
	}
	body, err = json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	if _, err := DecodeOperation(body, browser); err != nil {
		return nil, err
	}
	return body, nil
}

func operationRequired(fields map[string]json.RawMessage, typ reflect.Type) error {
	schema := make(map[string]reflect.StructField)
	operationFields(typ, schema)
	for name := range fields {
		if _, ok := schema[name]; !ok {
			return fmt.Errorf("unknown field %q", name)
		}
	}
	for name, field := range schema {
		raw, ok := fields[name]
		if !ok && !strings.Contains(field.Tag.Get("json"), ",omitempty") {
			return fmt.Errorf("missing %s", name)
		}
		if ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && field.Tag.Get("wire") != "null" {
			return fmt.Errorf("%s cannot be null", name)
		}
	}
	return nil
}

func operationFields(typ reflect.Type, fields map[string]reflect.StructField) {
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Anonymous {
			operationFields(field.Type, fields)
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			fields[name] = field
		}
	}
}

func decodeOperationObject(data []byte, out any) error {
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return fmt.Errorf("expected object")
	}
	if err := operationRequired(fields, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	return decodeOperationFields(fields, reflect.ValueOf(out).Elem())
}

func decodeOperationFields(fields map[string]json.RawMessage, out reflect.Value) error {
	schema := make(map[string]reflect.StructField)
	operationFields(out.Type(), schema)
	for name, raw := range fields {
		field := out.FieldByName(schema[name].Name)
		var err error
		if field.Type() == reflect.TypeOf([]byte{}) {
			var encoded string
			err = strictDecode(raw, &encoded)
			if err == nil {
				var decoded []byte
				decoded, err = base64.StdEncoding.Strict().DecodeString(encoded)
				if err == nil {
					field.SetBytes(decoded)
				}
			}
		} else {
			err = strictDecode(raw, field.Addr().Interface())
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func (p *Placement) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return err
	}
	_, before := fields["before"]
	_, after := fields["after"]
	if before == after {
		return fmt.Errorf("placement requires exactly one of before or after")
	}
	type wire Placement
	var value wire
	// Before is required only in the before arm of the placement union.
	if after {
		fields["before"] = json.RawMessage("null")
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if err := decodeOperationObject(raw, &value); err != nil {
		return err
	}
	*p = Placement(value)
	return nil
}

func (p Placement) MarshalJSON() ([]byte, error) {
	if p.Before != nil && p.After != nil {
		return nil, fmt.Errorf("placement has both anchors")
	}
	if p.After != nil {
		return json.Marshal(struct {
			Page   Target  `json:"page"`
			Parent *Target `json:"parent"`
			After  *Target `json:"after"`
		}{p.Page, p.Parent, p.After})
	}
	type wire Placement
	return json.Marshal(wire(p))
}

func (s *StructureCheck) UnmarshalJSON(data []byte) error {
	type wire StructureCheck
	var v wire
	if err := decodeOperationObject(data, &v); err != nil {
		return err
	}
	*s = StructureCheck(v)
	return nil
}

func (v *CellInputValue) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return fmt.Errorf("missing cell value")
	}
	var result CellInputValue
	switch data[0] {
	case 'n':
		if string(data) != "null" {
			return fmt.Errorf("invalid cell value")
		}
		result.Null = true
	case '"':
		var text string
		if err := strictDecode(data, &text); err != nil {
			return err
		}
		if err := validateText(text); err != nil {
			return err
		}
		result.Text = &text
	case 't', 'f':
		var value bool
		if err := strictDecode(data, &value); err != nil {
			return err
		}
		result.Bool = &value
	case '{':
		var option Target
		if err := strictDecode(data, &option); err != nil {
			return err
		}
		if err := option.ValidateKind(OptionKind); err != nil {
			return err
		}
		result.Option = &option
	case '[':
		var options []Target
		if err := strictDecode(data, &options); err != nil {
			return err
		}
		seen := make(map[string]bool)
		for _, option := range options {
			if err := option.ValidateKind(OptionKind); err != nil {
				return err
			}
			key := operationTargetKey(option)
			if seen[key] {
				return fmt.Errorf("duplicate cell option")
			}
			seen[key] = true
		}
		result.Options = options
	default:
		return fmt.Errorf("cell value must be a string, bool, null, option target, or option target array; decimals use strings")
	}
	*v = result
	return nil
}

func (v CellInputValue) MarshalJSON() ([]byte, error) {
	arms := 0
	if v.Null {
		arms++
	}
	if v.Text != nil {
		arms++
	}
	if v.Bool != nil {
		arms++
	}
	if v.Option != nil {
		arms++
	}
	if v.Options != nil {
		arms++
	}
	if arms != 1 {
		return nil, fmt.Errorf("cell value requires exactly one arm")
	}
	switch {
	case v.Null:
		return []byte("null"), nil
	case v.Text != nil:
		return json.Marshal(*v.Text)
	case v.Bool != nil:
		return json.Marshal(*v.Bool)
	case v.Option != nil:
		return json.Marshal(v.Option)
	default:
		return json.Marshal(v.Options)
	}
}

func (c *CellInput) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return err
	}
	if err := operationRequired(fields, reflect.TypeOf(CellInput{})); err != nil {
		return err
	}
	type wire CellInput
	var value wire
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	if err := value.Property.ValidateKind(PropertyKind); err != nil {
		return err
	}
	*c = CellInput(value)
	return nil
}

var operationHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
var operationID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func (a *AssetTarget) UnmarshalJSON(data []byte) error {
	var result AssetTarget
	if len(bytes.TrimSpace(data)) > 0 && bytes.TrimSpace(data)[0] == '"' {
		if err := strictDecode(data, &result.Hash); err != nil {
			return err
		}
		if !operationHash.MatchString(result.Hash) {
			return fmt.Errorf("invalid asset hash")
		}
	} else {
		var tmp struct {
			Tmp string `json:"tmp"`
		}
		if err := decodeOperationObject(data, &tmp); err != nil {
			return err
		}
		if err := (Target{Tmp: tmp.Tmp}).Validate(); err != nil {
			return err
		}
		result.Tmp = tmp.Tmp
	}
	*a = result
	return nil
}
func (a AssetTarget) MarshalJSON() ([]byte, error) {
	if (a.Hash == "") == (a.Tmp == "") {
		return nil, fmt.Errorf("asset requires hash or temporary name")
	}
	if a.Hash != "" {
		if !operationHash.MatchString(a.Hash) {
			return nil, fmt.Errorf("invalid asset hash")
		}
		return json.Marshal(a.Hash)
	}
	if err := (Target{Tmp: a.Tmp}).Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Tmp string `json:"tmp"`
	}{a.Tmp})
}
func (a *AssetInput) UnmarshalJSON(data []byte) error {
	type wire AssetInput
	var v wire
	if err := decodeOperationObject(data, &v); err != nil {
		return err
	}
	if v.Path == "" {
		return fmt.Errorf("empty asset path")
	}
	*a = AssetInput(v)
	return nil
}
func (p *ProposedItem) UnmarshalJSON(data []byte) error {
	type wire ProposedItem
	var v wire
	if err := decodeOperationObject(data, &v); err != nil {
		return err
	}
	if v.Ref == "" {
		return fmt.Errorf("empty proposed temporary name")
	}
	*p = ProposedItem(v)
	return nil
}
func (s *State) UnmarshalJSON(data []byte) error {
	type wire State
	var v wire
	if err := decodeOperationObject(data, &v); err != nil {
		return err
	}
	if v.Store == "" {
		return fmt.Errorf("empty store")
	}
	*s = State(v)
	return nil
}
func (g *ItemGuard) UnmarshalJSON(data []byte) error {
	type wire ItemGuard
	var v wire
	if err := decodeOperationObject(data, &v); err != nil {
		return err
	}
	*g = ItemGuard(v)
	return nil
}
func (g *Guards) UnmarshalJSON(data []byte) error {
	type wire Guards
	var v wire
	if err := decodeOperationObject(data, &v); err != nil {
		return err
	}
	*g = Guards(v)
	return nil
}
func (a *UploadAsset) UnmarshalJSON(data []byte) error {
	type wire UploadAsset
	var v wire
	if err := decodeOperationObject(data, &v); err != nil {
		return err
	}
	if err := (Target{Tmp: v.Ref}).Validate(); err != nil {
		return err
	}
	*a = UploadAsset(v)
	return nil
}

func operationKind(name string) Kind {
	switch name {
	case "page":
		return PageKind
	case "block":
		return BlockKind
	case "database":
		return DatabaseKind
	case "property":
		return PropertyKind
	case "option":
		return OptionKind
	case "row":
		return RowKind
	case "view":
		return ViewKind
	}
	return ""
}

func operationTargetKey(t Target) string {
	if t.Ref != nil {
		return string(t.Ref.ID)
	}
	return "tmp:" + t.Tmp
}

func walkOperationRefs(value reflect.Value, kind Kind, browser bool, visit func(Target, Kind) error) error {
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		return walkOperationRefs(value.Elem(), kind, browser, visit)
	}
	if value.Type() == reflect.TypeOf(Target{}) {
		target := value.Interface().(Target)
		if kind == "" && target.Ref != nil {
			kind = target.Ref.ID.Kind()
		}
		if err := target.ValidateKind(kind); err != nil {
			return err
		}
		if target.Ref != nil && target.Ref.Inc == 0 && !browser {
			return fmt.Errorf("agent reference has zero incarnation")
		}
		if visit != nil {
			return visit(target, kind)
		}
		return nil
	}
	if value.Type() == reflect.TypeOf(Ref{}) {
		ref := value.Interface().(Ref)
		return walkOperationRefs(reflect.ValueOf(Target{Ref: &ref}), kind, browser, visit)
	}
	if value.Type() == reflect.TypeOf(Filter{}) {
		filter := value.Interface().(Filter)
		if err := walkOperationRefs(reflect.ValueOf(filter.Property), PropertyKind, browser, visit); err != nil {
			return err
		}
		switch filter.Op {
		case Equal, NotEqual, Less, LessEqual, Greater, GreaterEqual:
			return filter.ValidateFor(NumberType)
		case Before, After, OnOrBefore, OnOrAfter:
			return filter.ValidateFor(DateType)
		}
		if len(filter.Value) == 0 {
			return nil
		}
		var operand CellInputValue
		if err := operand.UnmarshalJSON(filter.Value); err != nil {
			return err
		}
		return walkOperationRefs(reflect.ValueOf(operand), "", browser, visit)
	}
	switch value.Kind() {
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			field := value.Type().Field(i)
			if !value.Field(i).CanInterface() {
				continue
			}
			fieldKind := operationKind(field.Tag.Get("kind"))
			switch value.Interface().(type) {
			case Display:
				if field.Name == "Visible" || field.Name == "GroupBy" {
					fieldKind = PropertyKind
				}
			case Width, Sort:
				if field.Name == "Property" {
					fieldKind = PropertyKind
				}
			}
			if err := walkOperationRefs(value.Field(i), fieldKind, browser, visit); err != nil {
				return fmt.Errorf("%s: %w", field.Name, err)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if err := walkOperationRefs(value.Index(i), kind, browser, visit); err != nil {
				return err
			}
		}
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return fmt.Errorf("invalid UTF-8 string")
		}
	}
	return nil
}

func validateOperation(op Operation, browser bool) error {
	if isUndoControl(op) {
		if !browser {
			return fmt.Errorf("%s is browser-only", op.OperationName())
		}
		if meta := op.operationMeta(); meta.UnitID != nil || meta.Structure != nil {
			return fmt.Errorf("%s cannot be part of a compound unit", op.OperationName())
		}
	}
	if err := walkOperationRefs(reflect.ValueOf(op), "", browser, nil); err != nil {
		return err
	}
	meta := op.operationMeta()
	if meta.UnitID != nil {
		if !validUnitID(*meta.UnitID) {
			return fmt.Errorf("invalid unit_id")
		}
	}
	if meta.Structure != nil && meta.UnitID == nil {
		return fmt.Errorf("structure requires unit_id")
	}
	if field := reflect.Indirect(reflect.ValueOf(op)).FieldByName("CreateFields"); field.IsValid() {
		create := field.Interface().(CreateFields)
		if create.Ref == "" {
			return fmt.Errorf("empty temporary name")
		}
		kind := operationKind(strings.TrimPrefix(op.OperationName(), "create_"))
		if err := validateProposedID(create.ProposedID, kind, browser); err != nil {
			return err
		}
	}
	switch v := op.(type) {
	case *CreateBlockOp:
		if v.Content.Kind == InlineDatabase {
			return fmt.Errorf("database content requires create_database")
		}
	case *SetBlockOp:
		if browser && v.BaseV == nil {
			return fmt.Errorf("browser set_block requires base_v")
		}
	case *TurnBlocksOp:
		switch v.Kind {
		case "paragraph", "heading1", "heading2", "heading3", "bullet", "numbered", "todo", "toggle", "quote", "callout", "code":
		default:
			return fmt.Errorf("invalid conversion kind")
		}
	case *CreateDatabaseOp:
		if v.BlockRef == "" {
			return fmt.Errorf("empty block_ref")
		}
		for _, entry := range []struct {
			ID   *ID
			Kind Kind
		}{{v.BlockProposedID, BlockKind}, {v.TitlePropertyProposedID, PropertyKind}, {v.ViewProposedID, ViewKind}} {
			if err := validateProposedID(entry.ID, entry.Kind, browser); err != nil {
				return err
			}
		}
	case *CreateOptionOp:
		if err := v.Color.Validate(); err != nil {
			return err
		}
	case *SetOptionOp:
		if err := v.Color.Validate(); err != nil {
			return err
		}
	case *CreateRowOp:
		seen := make(map[string]bool)
		for _, cell := range v.Cells {
			key := operationTargetKey(cell.Property)
			if seen[key] {
				return fmt.Errorf("duplicate row cell property")
			}
			seen[key] = true
		}
	case *MarkdownToBlocksOp:
		if browser && v.ProposedIDs == nil {
			return fmt.Errorf("browser markdown requires proposed_ids")
		}
		seen := make(map[string]bool)
		for _, item := range v.ProposedIDs {
			if item.Ref == "" || seen[item.Ref] {
				return fmt.Errorf("empty or duplicate proposed temporary name")
			}
			seen[item.Ref] = true
			if _, err := ParseID(string(item.ProposedID)); err != nil {
				return err
			}
		}
		paths := make(map[string]bool)
		for _, asset := range v.AssetRefs {
			if paths[asset.Path] {
				return fmt.Errorf("duplicate asset path")
			}
			paths[asset.Path] = true
		}
	}
	value := reflect.Indirect(reflect.ValueOf(op))
	for _, name := range []string{"Blocks", "Rows", "Properties", "Options", "Views"} {
		field := value.FieldByName(name)
		if !field.IsValid() {
			continue
		}
		if (name == "Blocks" || name == "Rows") && field.Len() == 0 {
			return fmt.Errorf("empty %s", name)
		}
		seen := make(map[string]bool)
		for i := 0; i < field.Len(); i++ {
			target := field.Index(i).Interface().(Target)
			key := operationTargetKey(target)
			if seen[key] {
				return fmt.Errorf("duplicate %s target", name)
			}
			seen[key] = true
		}
	}
	_, err := operationCreates(op)
	return err
}

func validUnitID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, b := range []byte(id) {
		if b > 127 {
			return false
		}
	}
	return true
}

func validateProposedID(id *ID, kind Kind, browser bool) error {
	if id == nil {
		if browser {
			return fmt.Errorf("missing proposed id for %s", kind)
		}
		return nil
	}
	if !browser {
		return fmt.Errorf("proposed id is browser-only")
	}
	parsed, err := ParseID(string(*id))
	if err != nil {
		return err
	}
	if parsed.Kind() != kind {
		return fmt.Errorf("wrong proposed id kind: want %s", kind)
	}
	return nil
}

type operationCreation struct {
	Name string
	Kind Kind
	ID   *ID
}

func operationCreates(op Operation) ([]operationCreation, error) {
	var result []operationCreation
	value := reflect.Indirect(reflect.ValueOf(op))
	if field := value.FieldByName("CreateFields"); field.IsValid() {
		create := field.Interface().(CreateFields)
		result = append(result, operationCreation{create.Ref, operationKind(strings.TrimPrefix(op.OperationName(), "create_")), create.ProposedID})
	}
	if v, ok := op.(*CreateDatabaseOp); ok {
		result = append(result, operationCreation{v.BlockRef, BlockKind, v.BlockProposedID}, operationCreation{v.Ref + ".title", PropertyKind, v.TitlePropertyProposedID}, operationCreation{v.Ref + ".view", ViewKind, v.ViewProposedID})
	}
	if v, ok := op.(*MarkdownToBlocksOp); ok {
		for i := range v.ProposedIDs {
			item := &v.ProposedIDs[i]
			result = append(result, operationCreation{item.Ref, item.ProposedID.Kind(), &item.ProposedID})
		}
	}
	seenNames, seenIDs := make(map[string]bool), make(map[ID]bool)
	for _, item := range result {
		if err := (Target{Tmp: item.Name}).ValidateKind(item.Kind); err != nil {
			return nil, err
		}
		if seenNames[item.Name] {
			return nil, fmt.Errorf("duplicate temporary name %q", item.Name)
		}
		seenNames[item.Name] = true
		if item.ID != nil {
			if seenIDs[*item.ID] {
				return nil, fmt.Errorf("duplicate proposed id")
			}
			seenIDs[*item.ID] = true
		}
	}
	return result, nil
}

func decodeOperationList(raw rawOperations, browser bool, assets []UploadAsset) ([]Operation, error) {
	if len(raw) > MaxOperations {
		return nil, fmt.Errorf("ops exceeds %d explicit operations", MaxOperations)
	}
	ops := make([]Operation, 0, len(raw))
	names := make(map[string]Kind)
	proposed := make(map[ID]bool)
	assetNames := make(map[string]bool)
	for _, asset := range assets {
		if assetNames[asset.Ref] {
			return nil, fmt.Errorf("duplicate asset temporary name")
		}
		assetNames[asset.Ref] = true
	}
	units := make(map[string]bool)
	lastUnit := ""
	for i, data := range raw {
		op, err := DecodeOperation(data, browser)
		if err != nil {
			return nil, fmt.Errorf("ops[%d]: %w", i, err)
		}
		meta := op.operationMeta()
		unit := ""
		if meta.UnitID != nil {
			unit = *meta.UnitID
		}
		if unit != "" {
			if unit != lastUnit && units[unit] {
				return nil, fmt.Errorf("noncontiguous unit_id %q", unit)
			}
			if unit == lastUnit && meta.Structure != nil {
				return nil, fmt.Errorf("structure must appear on the first operation of a unit")
			}
			units[unit] = true
		}
		lastUnit = unit
		err = walkOperationRefs(reflect.ValueOf(op), "", browser, func(t Target, kind Kind) error {
			if t.Tmp == "" {
				return nil
			}
			createdKind, ok := names[t.Tmp]
			if !ok {
				return fmt.Errorf("unknown or forward temporary name %q", t.Tmp)
			}
			if kind != "" && createdKind != kind {
				return fmt.Errorf("temporary name %q has wrong kind", t.Tmp)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("ops[%d]: %w", i, err)
		}
		if markdown, ok := op.(*MarkdownToBlocksOp); ok {
			for _, asset := range markdown.AssetRefs {
				if asset.Asset.Tmp != "" && !assetNames[asset.Asset.Tmp] {
					return nil, fmt.Errorf("unknown asset temporary name %q", asset.Asset.Tmp)
				}
			}
		}
		creates, err := operationCreates(op)
		if err != nil {
			return nil, err
		}
		for _, item := range creates {
			if assetNames[item.Name] {
				return nil, fmt.Errorf("temporary name %q is already an asset", item.Name)
			}
			if _, exists := names[item.Name]; exists {
				return nil, fmt.Errorf("duplicate temporary name %q", item.Name)
			}
			names[item.Name] = item.Kind
			if item.ID != nil {
				if proposed[*item.ID] {
					return nil, fmt.Errorf("duplicate proposed id")
				}
				proposed[*item.ID] = true
			}
		}
		ops = append(ops, op)
	}
	return ops, nil
}

type updateWire struct {
	Slug    string        `json:"slug"`
	OpID    string        `json:"op_id"`
	Message *string       `json:"message,omitempty"`
	Guards  *Guards       `json:"guards,omitempty"`
	Ops     rawOperations `json:"ops"`
	Assets  []UploadAsset `json:"assets,omitempty"`
}

type actionWire struct {
	N           Sequence      `json:"n"`
	ActionID    string        `json:"action_id"`
	TypingBlock *Ref          `json:"typing_block,omitempty" kind:"block"`
	Close       *bool         `json:"close,omitempty"`
	Ops         rawOperations `json:"ops"`
}

func DecodeUpdate(data []byte) (Update, error) {
	var wire updateWire
	if err := decodeOperationObject(data, &wire); err != nil {
		return Update{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Update{}, err
	}
	var assets []map[string]json.RawMessage
	if raw, ok := fields["assets"]; ok {
		if err := json.Unmarshal(raw, &assets); err != nil {
			return Update{}, err
		}
	}
	operationBytes := len(data)
	for _, asset := range assets {
		operationBytes -= len(bytes.TrimSpace(asset["data"])) - 2
	}
	if operationBytes > MaxAgentOperationBytes {
		return Update{}, fmt.Errorf("update exceeds operation JSON limit")
	}
	if wire.Slug == "" {
		return Update{}, fmt.Errorf("empty slug")
	}
	if !operationID.MatchString(wire.OpID) {
		return Update{}, fmt.Errorf("invalid op_id")
	}
	if err := walkOperationRefs(reflect.ValueOf(wire.Guards), "", false, nil); err != nil {
		return Update{}, err
	}
	ops, err := decodeOperationList(wire.Ops, false, wire.Assets)
	if err != nil {
		return Update{}, err
	}
	return Update{Slug: wire.Slug, OpID: wire.OpID, Message: wire.Message, Guards: wire.Guards, Ops: ops, Assets: wire.Assets}, nil
}

func EncodeUpdate(update Update) ([]byte, error) {
	if !utf8.ValidString(update.Slug) || (update.Message != nil && !utf8.ValidString(*update.Message)) {
		return nil, fmt.Errorf("invalid UTF-8 update text")
	}
	ops, err := encodeOperationList(update.Ops, false)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(updateWire{update.Slug, update.OpID, update.Message, update.Guards, ops, update.Assets})
	if err != nil {
		return nil, err
	}
	if _, err := DecodeUpdate(data); err != nil {
		return nil, err
	}
	return data, nil
}

func DecodeAction(data []byte) (Action, error) {
	if len(data) > MaxActionBytes {
		return Action{}, fmt.Errorf("action exceeds assembly limit")
	}
	var wire actionWire
	if err := decodeOperationObject(data, &wire); err != nil {
		return Action{}, err
	}
	if wire.N == 0 || wire.ActionID == "" {
		return Action{}, fmt.Errorf("action requires positive n and nonempty action_id")
	}
	if err := walkOperationRefs(reflect.ValueOf(wire.TypingBlock), BlockKind, true, nil); err != nil {
		return Action{}, err
	}
	ops, err := decodeOperationList(wire.Ops, true, nil)
	if err != nil {
		return Action{}, err
	}
	for _, op := range ops {
		if isUndoControl(op) && (len(ops) != 1 || wire.TypingBlock != nil) {
			return Action{}, fmt.Errorf("undo and redo require their own non-typing action")
		}
	}
	return Action{wire.N, wire.ActionID, wire.TypingBlock, wire.Close, ops}, nil
}

func EncodeAction(action Action) ([]byte, error) {
	if !utf8.ValidString(action.ActionID) {
		return nil, fmt.Errorf("invalid UTF-8 action_id")
	}
	ops, err := encodeOperationList(action.Ops, true)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(actionWire{action.N, action.ActionID, action.TypingBlock, action.Close, ops})
	if err != nil {
		return nil, err
	}
	if _, err := DecodeAction(data); err != nil {
		return nil, err
	}
	return data, nil
}
func encodeOperationList(ops []Operation, browser bool) (rawOperations, error) {
	result := make(rawOperations, len(ops))
	for i, op := range ops {
		data, err := EncodeOperation(op, browser)
		if err != nil {
			return nil, fmt.Errorf("ops[%d]: %w", i, err)
		}
		result[i] = data
	}
	return result, nil
}
