package notebook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

func (e *edit) applySchema(op Operation) (bool, error) {
	previous := e.component
	defer func() { e.component = previous }()
	switch op := op.(type) {
	case *CreatePropertyOp:
		return true, e.createProperty(op)
	case *SetPropertyOp:
		return true, e.setProperty(op)
	case *DeletePropertyOp:
		return true, e.deleteProperty(op)
	case *SetPropertyOrderOp:
		return true, e.schemaOrder(op.Database, op.Properties, PropertyKind)
	case *CreateOptionOp:
		return true, e.createOption(op)
	case *SetOptionOp:
		return true, e.setOption(op)
	case *DeleteOptionOp:
		return true, e.deleteOption(op)
	case *SetOptionOrderOp:
		return true, e.schemaOrder(op.Property, op.Options, OptionKind)
	case *CreateViewOp:
		return true, e.createView(op)
	case *SetViewOp:
		return true, e.setView(op)
	case *DeleteViewOp:
		return true, e.deleteView(op)
	case *SetViewOrderOp:
		return true, e.schemaOrder(op.Database, op.Views, ViewKind)
	default:
		return false, nil
	}
}

func schemaFailure(reason string) error { return &editFailure{Status: "refused", Reason: reason} }

func (e *edit) schemaLimit(before, after, limit int) error {
	if e.deferAdmission {
		return nil
	}
	if err := CheckAdmission(int64(before), int64(after), int64(limit)); err != nil {
		return schemaFailure("limit")
	}
	return nil
}

func (e *edit) schemaName(owner, except ID, name string, kind Kind) error {
	table, column := "properties", "database_id"
	if kind == OptionKind {
		table, column = "options", "property_id"
	}
	var count int
	if err := e.tx.QueryRowContext(e.ctx, `SELECT count(*) FROM `+table+` WHERE `+column+`=? AND alive=1 AND name_key=? AND id<>?`, owner, name, except).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return schemaFailure("name")
	}
	return nil
}

func (e *edit) createProperty(op *CreatePropertyOp) error {
	owner, err := e.resolve(op.Database, DatabaseKind)
	if err != nil {
		return err
	}
	e.component = owner.Item.ID()
	if op.Definition.Type == TitleType {
		return invalidKind()
	}
	if err := op.Definition.Validate(); err != nil {
		return schemaFailure("definition")
	}
	if err := e.schemaLimit(0, utf8.RuneCountInString(op.Definition.Name), MaxNameCodePoints); err != nil {
		return err
	}
	if err := e.schemaName(owner.Item.ID(), "", op.Definition.Name, PropertyKind); err != nil {
		return err
	}
	db := owner.Item.Data.(Database)
	if err := e.schemaLimit(len(db.PropertyOrder), len(db.PropertyOrder)+1, MaxPropertiesPerDatabase); err != nil {
		return err
	}
	property, err := e.newItem(PropertyKind, op.CreateFields, Property{Database: owner.Item.ID(), Definition: op.Definition, OptionOrder: []ID{}})
	if err != nil {
		return err
	}
	db.PropertyOrder = append(slices.Clone(db.PropertyOrder), property.Item.ID())
	owner.Item.Data = db
	if err := e.dbWrite(owner, PropertyOrderGroup); err != nil {
		return err
	}
	return e.repairViews(owner.Item.ID())
}

func (e *edit) setProperty(op *SetPropertyOp) error {
	record, err := e.resolve(op.Property, PropertyKind)
	if err != nil {
		return err
	}
	e.component = record.Item.ID()
	p := record.Item.Data.(Property)
	if err := op.Definition.Validate(); err != nil {
		return schemaFailure("definition")
	}
	if (p.Definition.Type == TitleType || op.Definition.Type == TitleType) && p.Definition.Type != op.Definition.Type {
		return invalidKind()
	}
	if err := e.schemaLimit(utf8.RuneCountInString(p.Definition.Name), utf8.RuneCountInString(op.Definition.Name), MaxNameCodePoints); err != nil {
		return err
	}
	if err := e.schemaName(p.Database, record.Item.ID(), op.Definition.Name, PropertyKind); err != nil {
		return err
	}
	if p.Definition.Type != op.Definition.Type {
		preview, err := e.previewProperty(op)
		if err != nil {
			return err
		}
		if e.browser && (op.ConfirmedPreview == nil || *op.ConfirmedPreview != preview.Token) {
			detail, err := json.Marshal(preview)
			if err != nil {
				return err
			}
			return &editFailure{Status: "refused", Reason: "confirmation", Detail: string(detail)}
		}
		if err := e.convertProperty(&record, op.Definition.Type); err != nil {
			return err
		}
		record, err = loadRecord(e.ctx, e.tx, record.Item.ID())
		if err != nil {
			return err
		}
		p = record.Item.Data.(Property)
	}
	p.Definition = op.Definition
	record.Item.Data = p
	if err := e.dbWrite(record, DefinitionGroup); err != nil {
		return err
	}
	return e.repairViews(p.Database)
}

func (e *edit) deleteProperty(op *DeletePropertyOp) error {
	record, err := e.resolve(op.Property, PropertyKind)
	if err != nil {
		return err
	}
	e.component = record.Item.ID()
	p := record.Item.Data.(Property)
	if p.Definition.Type == TitleType {
		return invalidKind()
	}
	rows, err := e.dbRows(p.Database)
	if err != nil {
		return err
	}
	for _, r := range rows {
		row := r.Item.Data.(Row)
		if _, ok := row.Cells[record.Item.ID()]; !ok {
			continue
		}
		row.Cells = maps.Clone(row.Cells)
		delete(row.Cells, record.Item.ID())
		r.Item.Data = row
		if err := e.dbWrite(r, CellGroup(record.Item.ID())); err != nil {
			return err
		}
	}
	for _, id := range p.OptionOrder {
		option, err := loadRecord(e.ctx, e.tx, id)
		if err != nil {
			return err
		}
		if err := e.dbDelete(option); err != nil {
			return err
		}
	}
	if err := e.dbDelete(record); err != nil {
		return err
	}
	owner, err := loadRecord(e.ctx, e.tx, p.Database)
	if err != nil {
		return err
	}
	db := owner.Item.Data.(Database)
	db.PropertyOrder = slices.DeleteFunc(slices.Clone(db.PropertyOrder), func(id ID) bool { return id == record.Item.ID() })
	owner.Item.Data = db
	if err := e.dbWrite(owner, PropertyOrderGroup); err != nil {
		return err
	}
	return e.repairViews(p.Database)
}

func (e *edit) createOption(op *CreateOptionOp) error {
	record, err := e.resolve(op.Property, PropertyKind)
	if err != nil {
		return err
	}
	e.component = record.Item.ID()
	p := record.Item.Data.(Property)
	if p.Definition.Type != SelectType && p.Definition.Type != MultiSelectType {
		return invalidKind()
	}
	if err := op.Color.Validate(); err != nil {
		return schemaFailure("definition")
	}
	if err := e.schemaLimit(0, utf8.RuneCountInString(op.Name), MaxNameCodePoints); err != nil {
		return err
	}
	if err := e.schemaName(record.Item.ID(), "", op.Name, OptionKind); err != nil {
		return err
	}
	if err := e.schemaLimit(len(p.OptionOrder), len(p.OptionOrder)+1, MaxOptionsPerProperty); err != nil {
		return err
	}
	option, err := e.newItem(OptionKind, op.CreateFields, Option{Property: record.Item.ID(), Name: op.Name, Color: op.Color})
	if err != nil {
		return err
	}
	p.OptionOrder = append(slices.Clone(p.OptionOrder), option.Item.ID())
	record.Item.Data = p
	if err := e.dbWrite(record, OptionOrderGroup); err != nil {
		return err
	}
	return e.repairViews(p.Database)
}

func (e *edit) setOption(op *SetOptionOp) error {
	record, err := e.resolve(op.Option, OptionKind)
	if err != nil {
		return err
	}
	e.component = record.Item.ID()
	option := record.Item.Data.(Option)
	if err := op.Color.Validate(); err != nil {
		return schemaFailure("definition")
	}
	if err := e.schemaLimit(utf8.RuneCountInString(option.Name), utf8.RuneCountInString(op.Name), MaxNameCodePoints); err != nil {
		return err
	}
	if err := e.schemaName(option.Property, record.Item.ID(), op.Name, OptionKind); err != nil {
		return err
	}
	option.Name, option.Color = op.Name, op.Color
	record.Item.Data = option
	if err := e.dbWrite(record, DefinitionGroup); err != nil {
		return err
	}
	property, err := loadRecord(e.ctx, e.tx, option.Property)
	if err != nil {
		return err
	}
	return e.repairViews(property.Item.Data.(Property).Database)
}

func (e *edit) deleteOption(op *DeleteOptionOp) error {
	record, err := e.resolve(op.Option, OptionKind)
	if err != nil {
		return err
	}
	e.component = record.Item.ID()
	property, err := loadRecord(e.ctx, e.tx, record.Item.Data.(Option).Property)
	if err != nil {
		return err
	}
	p := property.Item.Data.(Property)
	rows, err := e.dbRows(p.Database)
	if err != nil {
		return err
	}
	for _, r := range rows {
		row := r.Item.Data.(Row)
		cell, ok := row.Cells[property.Item.ID()]
		if !ok {
			continue
		}
		changed := false
		if cell.Type == SelectType && cell.Value == record.Item.ID() {
			cell.Value, changed = nil, true
		} else if cell.Type == MultiSelectType {
			ids := cell.Value.([]ID)
			if slices.Contains(ids, record.Item.ID()) {
				cell.Value = slices.DeleteFunc(slices.Clone(ids), func(id ID) bool { return id == record.Item.ID() })
				changed = true
			}
		}
		if !changed {
			continue
		}
		row.Cells = maps.Clone(row.Cells)
		row.Cells[property.Item.ID()] = cell
		r.Item.Data = row
		if err := e.dbWrite(r, CellGroup(property.Item.ID())); err != nil {
			return err
		}
	}
	if err := e.dbDelete(record); err != nil {
		return err
	}
	p.OptionOrder = slices.DeleteFunc(slices.Clone(p.OptionOrder), func(id ID) bool { return id == record.Item.ID() })
	property.Item.Data = p
	if err := e.dbWrite(property, OptionOrderGroup); err != nil {
		return err
	}
	return e.repairViews(p.Database)
}

func (e *edit) schemaOrder(target Target, targets []Target, kind Kind) error {
	ownerKind := DatabaseKind
	if kind == OptionKind {
		ownerKind = PropertyKind
	}
	owner, err := e.resolve(target, ownerKind)
	if err != nil {
		return err
	}
	e.component = owner.Item.ID()
	var current []ID
	var database ID
	switch data := owner.Item.Data.(type) {
	case Database:
		database = owner.Item.ID()
		current = data.PropertyOrder
		if kind == ViewKind {
			current = data.ViewOrder
		}
	case Property:
		database, current = data.Database, data.OptionOrder
		if data.Definition.Type != SelectType && data.Definition.Type != MultiSelectType {
			return invalidKind()
		}
	}
	order, removed, err := e.dbOrder(owner, targets, current, kind)
	if err != nil {
		return err
	}
	group := PropertyOrderGroup
	switch data := owner.Item.Data.(type) {
	case Database:
		if kind == ViewKind {
			data.ViewOrder, group = order, ViewOrderGroup
		} else {
			data.PropertyOrder = order
		}
		owner.Item.Data = data
	case Property:
		data.OptionOrder, group = order, OptionOrderGroup
		owner.Item.Data = data
	}
	if err := e.dbWrite(owner, group); err != nil {
		return err
	}
	if len(removed) > 0 {
		// Order sanitization has no view target; the empty id records only its outcome.
		if err := e.dbNotice("", removed); err != nil {
			return err
		}
	}
	return e.repairViews(database)
}

// A preview token binds only conversion inputs, not unrelated database cells.
// The writer owns the pre-conversion revision boundary before calling apply.
type propertyConversionPreview struct {
	Property       Ref    `json:"property"`
	State          State  `json:"state"`
	InputVersion   string `json:"input_version"`
	EmptiedValues  int    `json:"emptied_values"`
	OptionsCreated int    `json:"options_created"`
	Token          string `json:"token"`
}

func (e *edit) previewProperty(op *SetPropertyOp) (propertyConversionPreview, error) {
	record, err := e.resolve(op.Property, PropertyKind)
	if err != nil {
		return propertyConversionPreview{}, err
	}
	p := record.Item.Data.(Property)
	preview := propertyConversionPreview{Property: record.Item.Ref}
	var seq string
	if err := e.tx.QueryRowContext(e.ctx, `SELECT store,seq FROM meta WHERE singleton=1`).Scan(&preview.State.Store, &seq); err != nil {
		return preview, err
	}
	n, err := parseDBDecimal(seq)
	if err != nil {
		return preview, err
	}
	preview.State.Seq = Sequence(n)
	rows, err := e.dbRows(p.Database)
	if err != nil {
		return preview, err
	}
	type inputRow struct {
		Ref      Ref
		Cell     CellValue
		Version  Version
		Position string
	}
	inputs := []inputRow{}
	names := map[string]bool{}
	for _, r := range rows {
		row := r.Item.Data.(Row)
		cell, ok := row.Cells[record.Item.ID()]
		if !ok {
			cell = dbEmpty(p.Definition.Type)
		}
		version, err := r.Item.GroupVersion(CellGroup(record.Item.ID()))
		if err != nil {
			return preview, err
		}
		inputs = append(inputs, inputRow{r.Item.Ref, cell, version, row.Position})
		if p.Definition.Type == TextType && op.Definition.Type == SelectType {
			text := cell.Value.(string)
			if text != "" {
				if !e.deferAdmission && utf8.RuneCountInString(text) > MaxNameCodePoints {
					return preview, schemaFailure("limit")
				}
				names[text] = true
			}
			continue
		}
		converted := schemaConvert(cell, op.Definition.Type, p.OptionOrder)
		if !schemaEmpty(cell) && schemaEmpty(converted) {
			preview.EmptiedValues++
		}
	}
	if p.Definition.Type == TextType && op.Definition.Type == SelectType {
		preview.OptionsCreated = len(names)
		if err := e.schemaLimit(len(p.OptionOrder), len(names), MaxOptionsPerProperty); err != nil {
			return preview, err
		}
	}
	options := []itemRecord{}
	for _, id := range p.OptionOrder {
		option, err := loadRecord(e.ctx, e.tx, id)
		if err != nil {
			return preview, err
		}
		options = append(options, option)
	}
	raw, err := CanonicalJSON(struct {
		Property   Item
		Definition PropertyDef
		Rows       []inputRow
		Options    []itemRecord
	}{record.Item, op.Definition, inputs, options})
	if err != nil {
		return preview, err
	}
	hash := sha256.Sum256(raw)
	preview.InputVersion = hex.EncodeToString(hash[:])
	preview.Token = preview.InputVersion
	return preview, nil
}

func schemaEmpty(cell CellValue) bool {
	switch value := cell.Value.(type) {
	case nil:
		return true
	case string:
		return value == ""
	case bool:
		return !value
	case []ID:
		return len(value) == 0
	}
	return false
}

func schemaConvert(cell CellValue, to PropertyType, order []ID) CellValue {
	if cell.Type == to {
		return cell
	}
	result := dbEmpty(to)
	switch {
	case cell.Type == NumberType && to == TextType:
		if cell.Value != nil {
			result.Value = string(cell.Value.(Decimal))
		}
	case cell.Type == TextType && to == NumberType:
		value, err := ParseDecimal(strings.Trim(cell.Value.(string), " \t\n\r\v\f"))
		if err == nil && utf8.RuneCountInString(string(value)) <= MaxCellCodePoints {
			result.Value = value
		}
	case cell.Type == SelectType && to == MultiSelectType:
		if cell.Value != nil {
			result.Value = []ID{cell.Value.(ID)}
		}
	case cell.Type == MultiSelectType && to == SelectType:
		for _, id := range order {
			if slices.Contains(cell.Value.([]ID), id) {
				result.Value = id
				break
			}
		}
	}
	return result
}

func (e *edit) convertProperty(record *itemRecord, to PropertyType) error {
	p := record.Item.Data.(Property)
	rows, err := e.dbRows(p.Database)
	if err != nil {
		return err
	}
	keepOptions := (p.Definition.Type == SelectType || p.Definition.Type == MultiSelectType) && (to == SelectType || to == MultiSelectType)
	names := map[string]ID{}
	order := []ID{}
	if keepOptions {
		order = slices.Clone(p.OptionOrder)
	}
	for _, r := range rows {
		row := r.Item.Data.(Row)
		cell, ok := row.Cells[record.Item.ID()]
		if !ok {
			cell = dbEmpty(p.Definition.Type)
		}
		converted := schemaConvert(cell, to, p.OptionOrder)
		if p.Definition.Type == TextType && to == SelectType {
			text := cell.Value.(string)
			if text != "" {
				id, ok := names[text]
				if !ok {
					option, err := e.newItem(OptionKind, CreateFields{}, Option{Property: record.Item.ID(), Name: text, Color: Gray})
					if err != nil {
						return err
					}
					id, names[text] = option.Item.ID(), option.Item.ID()
					order = append(order, id)
				}
				converted.Value = id
			}
		}
		row.Cells = maps.Clone(row.Cells)
		row.Cells[record.Item.ID()] = converted
		r.Item.Data = row
		if err := e.dbWrite(r, CellGroup(record.Item.ID())); err != nil {
			return err
		}
	}
	if !keepOptions {
		for _, id := range p.OptionOrder {
			option, err := loadRecord(e.ctx, e.tx, id)
			if err != nil {
				return err
			}
			if err := e.dbDelete(option); err != nil {
				return err
			}
		}
	}
	p.OptionOrder = order
	p.Definition.Type = to
	p.Definition.Format = ""
	record.Item.Data = p
	return e.dbWrite(*record, DefinitionGroup, OptionOrderGroup)
}

func (e *edit) createView(op *CreateViewOp) error {
	owner, err := e.resolve(op.Database, DatabaseKind)
	if err != nil {
		return err
	}
	e.component = owner.Item.ID()
	db := owner.Item.Data.(Database)
	if err := e.schemaLimit(len(db.ViewOrder), len(db.ViewOrder)+1, MaxViewsPerDatabase); err != nil {
		return err
	}
	if err := e.schemaLimit(0, utf8.RuneCountInString(op.Definition.Name), MaxNameCodePoints); err != nil {
		return err
	}
	definition, removed, err := e.schemaView(owner.Item.ID(), op.Definition, e.browser, false)
	if err != nil {
		return err
	}
	record, err := e.newItem(ViewKind, op.CreateFields, View{Database: owner.Item.ID(), Definition: definition})
	if err != nil {
		return err
	}
	db.ViewOrder = append(slices.Clone(db.ViewOrder), record.Item.ID())
	owner.Item.Data = db
	if err := e.dbWrite(owner, ViewOrderGroup); err != nil {
		return err
	}
	if len(removed) != 0 {
		return e.dbNotice(record.Item.ID(), removed)
	}
	return nil
}

func (e *edit) setView(op *SetViewOp) error {
	record, err := e.resolve(op.View, ViewKind)
	if err != nil {
		return err
	}
	e.component = record.Item.ID()
	view := record.Item.Data.(View)
	if err := e.schemaLimit(utf8.RuneCountInString(view.Definition.Name), utf8.RuneCountInString(op.Definition.Name), MaxNameCodePoints); err != nil {
		return err
	}
	definition, removed, err := e.schemaView(view.Database, op.Definition, e.browser, false)
	if err != nil {
		return err
	}
	view.Definition = definition
	record.Item.Data = view
	if err := e.dbWrite(record, DefinitionGroup); err != nil {
		return err
	}
	if len(removed) != 0 {
		return e.dbNotice(record.Item.ID(), removed)
	}
	return nil
}

func (e *edit) deleteView(op *DeleteViewOp) error {
	record, err := e.resolve(op.View, ViewKind)
	if err != nil {
		return err
	}
	e.component = record.Item.ID()
	owner, err := loadRecord(e.ctx, e.tx, record.Item.Data.(View).Database)
	if err != nil {
		return err
	}
	db := owner.Item.Data.(Database)
	if len(db.ViewOrder) <= 1 {
		return schemaFailure("last_view")
	}
	if err := e.dbDelete(record); err != nil {
		return err
	}
	db.ViewOrder = slices.DeleteFunc(slices.Clone(db.ViewOrder), func(id ID) bool { return id == record.Item.ID() })
	owner.Item.Data = db
	return e.dbWrite(owner, ViewOrderGroup)
}

func (e *edit) repairViews(database ID) error {
	views, err := e.dbMembers(database, ViewKind)
	if err != nil {
		return err
	}
	for _, record := range views {
		view := record.Item.Data.(View)
		// Stored ids deliberately have no incarnation. Apply-time writes below
		// still require exact incarnations; repair resolves these current ids.
		wire, err := view.Definition.Wire(func(id ID) (Ref, error) { return Ref{ID: id, Inc: 1}, nil })
		if err != nil {
			return err
		}
		definition, removed, err := e.schemaView(database, wire, true, true)
		if err != nil {
			return err
		}
		if len(removed) == 0 {
			continue
		}
		view.Definition = definition
		record.Item.Data = view
		if err := e.dbWrite(record, DefinitionGroup); err != nil {
			return err
		}
		if err := e.dbNotice(record.Item.ID(), removed); err != nil {
			return err
		}
	}
	return nil
}

func (e *edit) schemaView(database ID, input ViewDef, adjust, stored bool) (StoredView, []string, error) {
	if err := input.Validate(); err != nil {
		return StoredView{}, nil, schemaFailure("definition")
	}
	out := ViewDef{Name: input.Name, Display: Display{Layout: input.Display.Layout, Visible: []Target{}, ShowEmpty: input.Display.ShowEmpty}, Sort: []Sort{}, Filter: []Filter{}}
	removed := []string{}
	resolve := func(t Target, kind Kind) (itemRecord, error) {
		if stored {
			if t.Ref == nil {
				return itemRecord{}, invalidStructure()
			}
			r, err := loadRecord(e.ctx, e.tx, t.Ref.ID)
			if err != nil {
				return r, err
			}
			if !r.Alive {
				return r, gone()
			}
			return r, nil
		}
		return e.resolve(t, kind)
	}
	property := func(t Target) (itemRecord, bool, error) {
		r, err := resolve(t, PropertyKind)
		if err != nil {
			var f *editFailure
			if adjust && errors.As(err, &f) && f.Reason == "gone" {
				return r, false, nil
			}
			return r, false, err
		}
		if r.Item.Data.(Property).Database != database {
			return r, false, invalidStructure()
		}
		return r, true, nil
	}
	for _, target := range input.Display.Visible {
		p, ok, err := property(target)
		if err != nil {
			return StoredView{}, nil, err
		}
		if !ok {
			removed = append(removed, "visible:"+targetKey(target))
			continue
		}
		out.Display.Visible = append(out.Display.Visible, Target{Ref: &p.Item.Ref})
	}
	if input.Display.Layout == "table" {
		out.Display.Widths = []Width{}
	}
	for _, width := range input.Display.Widths {
		p, ok, err := property(width.Property)
		if err != nil {
			return StoredView{}, nil, err
		}
		if !ok {
			removed = append(removed, "width:"+targetKey(width.Property))
			continue
		}
		out.Display.Widths = append(out.Display.Widths, Width{Property: Target{Ref: &p.Item.Ref}, PX: width.PX})
	}
	if input.Display.GroupBy != nil {
		p, ok, err := property(*input.Display.GroupBy)
		if err != nil {
			return StoredView{}, nil, err
		}
		if ok && p.Item.Data.(Property).Definition.Type == SelectType {
			out.Display.GroupBy = &Target{Ref: &p.Item.Ref}
		} else {
			if !adjust {
				return StoredView{}, nil, schemaFailure("view")
			}
			removed = append(removed, "group_by:"+targetKey(*input.Display.GroupBy))
			out.Display.Layout, out.Display.Widths, out.Display.ShowEmpty = "table", []Width{}, false
		}
	}
	for _, key := range input.Sort {
		p, ok, err := property(key.Property)
		if err != nil {
			return StoredView{}, nil, err
		}
		if !ok {
			removed = append(removed, "sort:"+targetKey(key.Property))
			continue
		}
		out.Sort = append(out.Sort, Sort{Property: Target{Ref: &p.Item.Ref}, Direction: key.Direction})
	}
	for _, filter := range input.Filter {
		p, ok, err := property(filter.Property)
		if err != nil {
			return StoredView{}, nil, err
		}
		if !ok || !validDBFilter(p.Item.Data.(Property).Definition.Type, filter) {
			if !adjust {
				return StoredView{}, nil, schemaFailure("view")
			}
			removed = append(removed, "filter:"+targetKey(filter.Property))
			continue
		}
		filter.Property = Target{Ref: &p.Item.Ref}
		typ := p.Item.Data.(Property).Definition.Type
		if filter.Op.NeedsValue() && (typ == SelectType || typ == MultiSelectType) {
			targets := []Target{}
			if filter.Op == IsAnyOf {
				if err := json.Unmarshal(filter.Value, &targets); err != nil {
					return StoredView{}, nil, err
				}
			} else {
				var target Target
				if err := json.Unmarshal(filter.Value, &target); err != nil {
					return StoredView{}, nil, err
				}
				targets = append(targets, target)
			}
			valid := true
			for i, target := range targets {
				option, err := resolve(target, OptionKind)
				if err != nil {
					var failure *editFailure
					if !adjust || !errors.As(err, &failure) || failure.Reason != "gone" {
						return StoredView{}, nil, err
					}
					valid = false
					continue
				}
				if option.Item.Data.(Option).Property != p.Item.ID() {
					if !adjust {
						return StoredView{}, nil, invalidStructure()
					}
					valid = false
					continue
				}
				targets[i] = Target{Ref: &option.Item.Ref}
			}
			if !valid {
				removed = append(removed, "filter-option:"+targetKey(filter.Property))
				continue
			}
			var raw []byte
			if filter.Op == IsAnyOf {
				raw, err = json.Marshal(targets)
			} else {
				raw, err = json.Marshal(targets[0])
			}
			if err != nil {
				return StoredView{}, nil, err
			}
			filter.Value = raw
		}
		out.Filter = append(out.Filter, filter)
	}
	if out.Display.Layout == "table" {
		properties, err := e.dbMembers(database, PropertyKind)
		if err != nil {
			return StoredView{}, nil, err
		}
		for _, p := range properties {
			if p.Item.Data.(Property).Definition.Type != TitleType {
				continue
			}
			if slices.ContainsFunc(out.Display.Visible, func(t Target) bool { return t.Ref.ID == p.Item.ID() }) {
				break
			}
			if !adjust {
				return StoredView{}, nil, schemaFailure("view")
			}
			out.Display.Visible = append([]Target{{Ref: &p.Item.Ref}}, out.Display.Visible...)
			removed = append(removed, "title_visibility")
			break
		}
	}
	definition, err := StoreView(out)
	if err != nil {
		return StoredView{}, nil, fmt.Errorf("store repaired view: %w", err)
	}
	return definition, removed, nil
}
