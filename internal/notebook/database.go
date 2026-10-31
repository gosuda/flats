package notebook

import (
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

func (e *edit) applyDatabase(op Operation) (bool, error) {
	previous := e.component
	defer func() { e.component = previous }()
	switch op := op.(type) {
	case *CreateDatabaseOp:
		return true, e.createDatabase(op)
	case *SetDatabaseTitleOp:
		record, err := e.resolve(op.Database, DatabaseKind)
		if err != nil {
			return true, err
		}
		data := record.Item.Data.(Database)
		if err := e.schemaLimit(utf8.RuneCountInString(data.Title), utf8.RuneCountInString(op.Title), MaxTitleCodePoints); err != nil {
			return true, err
		}
		data.Title = op.Title
		record.Item.Data = data
		return true, e.dbWrite(record, TitleGroup)
	case *CreateRowOp:
		return true, e.createRow(op)
	case *SetCellOp:
		record, err := e.resolve(op.Row, RowKind)
		if err != nil {
			return true, err
		}
		return true, e.setCell(record, op.Cell)
	case *MoveRowsOp:
		return true, e.moveRows(op)
	case *DeleteRowsOp:
		records, err := e.dbSelectedRows(op.Rows)
		if err != nil {
			return true, err
		}
		if len(records) > 0 {
			e.component = records[0].Item.ID()
		}
		for _, record := range records {
			if err := e.dbDelete(record); err != nil {
				return true, err
			}
		}
		return true, nil
	default:
		return e.applySchema(op)
	}
}

func (e *edit) createDatabase(op *CreateDatabaseOp) error {
	page, parent, err := e.blockDestination(op.Place)
	if err != nil {
		return err
	}
	if e.session != "" {
		owner, err := loadRecord(e.ctx, e.tx, page)
		if err != nil {
			return err
		}
		if err := e.dbRememberPage(owner); err != nil {
			return err
		}
	}
	position, err := e.place(BlockKind, page, parent, op.Place.Before, op.Place.After, nil)
	if err != nil {
		return err
	}
	name := "Name"
	if op.TitlePropertyName != nil {
		name = *op.TitlePropertyName
	}
	if err := e.schemaLimit(0, utf8.RuneCountInString(name), MaxNameCodePoints); err != nil {
		return err
	}
	if err := e.schemaLimit(0, utf8.RuneCountInString(op.Title), MaxTitleCodePoints); err != nil {
		return err
	}
	var count int
	if err := e.tx.QueryRowContext(e.ctx, `SELECT count(*) FROM items WHERE kind='database' AND alive=1`).Scan(&count); err != nil {
		return err
	}
	if err := e.schemaLimit(count, count+1, MaxDatabases); err != nil {
		return err
	}
	databaseID := op.ProposedID
	if databaseID == nil {
		id, err := NewID(DatabaseKind)
		if err != nil {
			return err
		}
		databaseID = &id
	}
	e.component = *databaseID
	block, err := e.newItem(BlockKind, CreateFields{Ref: op.BlockRef, ProposedID: op.BlockProposedID}, Block{Page: page, Parent: parent, Position: position, Content: Content{Kind: InlineDatabase, Database: *databaseID}})
	if err != nil {
		return err
	}
	database, err := e.newItem(DatabaseKind, CreateFields{Ref: op.Ref, ProposedID: databaseID}, Database{Block: block.Item.ID(), Title: op.Title, PropertyOrder: []ID{}, ViewOrder: []ID{}})
	if err != nil {
		return err
	}
	title, err := e.newItem(PropertyKind, CreateFields{Ref: op.Ref + ".title", ProposedID: op.TitlePropertyProposedID}, Property{Database: database.Item.ID(), Definition: PropertyDef{Name: name, Type: TitleType}, OptionOrder: []ID{}})
	if err != nil {
		return err
	}
	view, err := e.newItem(ViewKind, CreateFields{Ref: op.Ref + ".view", ProposedID: op.ViewProposedID}, View{Database: database.Item.ID(), Definition: StoredView{Name: "Table", Display: StoredDisplay{Layout: "table", Visible: []ID{title.Item.ID()}, Widths: []StoredWidth{}}, Sort: []StoredSort{}, Filter: []StoredFilter{}}})
	if err != nil {
		return err
	}
	data := database.Item.Data.(Database)
	data.PropertyOrder, data.ViewOrder = []ID{title.Item.ID()}, []ID{view.Item.ID()}
	database.Item.Data = data
	return e.dbWrite(database, PropertyOrderGroup, ViewOrderGroup)
}

func dbEmpty(typ PropertyType) CellValue {
	value := CellValue{Type: typ}
	switch typ {
	case TitleType, TextType:
		value.Value = ""
	case MultiSelectType:
		value.Value = []ID{}
	case CheckboxType:
		value.Value = false
	}
	return value
}

func (e *edit) dbMembers(database ID, kind Kind) ([]itemRecord, error) {
	switch kind {
	case PropertyKind:
		return e.records(`SELECT p.id FROM properties p JOIN items i ON i.id=p.id WHERE p.database_id=? AND i.alive=1 ORDER BY p.id`, database)
	case ViewKind:
		return e.records(`SELECT v.id FROM views v JOIN items i ON i.id=v.id WHERE v.database_id=? AND i.alive=1 ORDER BY v.id`, database)
	case RowKind:
		return e.dbRows(database)
	default:
		return nil, invalidStructure()
	}
}

func (e *edit) dbRows(database ID) ([]itemRecord, error) {
	return e.records(`SELECT r.id FROM rows r JOIN items i ON i.id=r.id WHERE r.database_id=? AND i.alive=1 ORDER BY r.position,r.id`, database)
}

func (e *edit) dbOwner(record itemRecord) (ID, error) {
	switch data := record.Item.Data.(type) {
	case Database:
		return record.Item.ID(), nil
	case Property:
		return data.Database, nil
	case Row:
		return data.Database, nil
	case View:
		return data.Database, nil
	case Option:
		property, err := loadRecord(e.ctx, e.tx, data.Property)
		if err != nil {
			return "", err
		}
		return property.Item.Data.(Property).Database, nil
	}
	return "", invalidStructure()
}

func (e *edit) dbWrite(record itemRecord, groups ...GroupKey) error {
	database, err := e.dbOwner(record)
	if err != nil {
		return err
	}
	if err := e.dbTrackLineage(database); err != nil {
		return err
	}
	if previous, err := loadRecord(e.ctx, e.tx, record.Item.ID()); err == nil {
		record.Aggregate = max(record.Aggregate, previous.Aggregate)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := e.write(record, groups...); err != nil {
		return err
	}
	if original, ok := e.oldPlaces[record.Item.ID()]; ok && record.Item.Kind() == RowKind {
		change := e.changes[record.Item.ID()]
		if change.Before != nil {
			before := change.Before.Item.Data.(Row)
			before.Position = original.Data.(Row).Position
			change.Before.Item.Data = before
			e.changes[record.Item.ID()] = change
		}
	}
	return e.dbBump(database)
}

func (e *edit) dbBump(database ID) error {
	if err := e.dbTrackLineage(database); err != nil {
		return err
	}
	owner, err := loadRecord(e.ctx, e.tx, database)
	if err != nil {
		return err
	}
	owner.Aggregate = max(owner.Aggregate, Version(e.seq))
	owner.Item.Ver = max(owner.Item.Ver, owner.Aggregate)
	if err := putRecord(e.ctx, e.tx, owner); err != nil {
		return err
	}
	e.noteEffect(owner)
	block, err := loadRecord(e.ctx, e.tx, owner.Item.Data.(Database).Block)
	if err != nil {
		return err
	}
	return e.bumpPage(block.Item.Data.(Block).Page)
}

func (e *edit) dbRememberPage(record itemRecord) error {
	if e.session == "" {
		return nil
	}
	var page ID
	switch data := record.Item.Data.(type) {
	case Page:
		page = record.Item.ID()
	case Block:
		page = data.Page
	default:
		return nil
	}
	if _, ok := e.pageBefore[page]; ok {
		return nil
	}
	if record.Item.Kind() != PageKind {
		var err error
		record, err = loadRecord(e.ctx, e.tx, page)
		if err != nil {
			return err
		}
	}
	if e.pageBefore == nil {
		e.pageBefore = map[ID]Version{}
	}
	e.pageBefore[page] = record.Item.Ver
	return nil
}

func (e *edit) dbTrackLineage(database ID) error {
	if e.session == "" {
		return nil
	}
	if _, ok := e.dbLineage[database]; ok {
		return nil
	}
	owner, err := loadRecord(e.ctx, e.tx, database)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if e.dbLineage == nil {
		e.dbLineage = map[ID]undoAggregate{}
	}
	e.dbLineage[database] = undoAggregate{Ref: owner.Item.Ref, Before: owner.Item.Ver}
	block, err := loadRecord(e.ctx, e.tx, owner.Item.Data.(Database).Block)
	if err != nil {
		return err
	}
	pageID := block.Item.Data.(Block).Page
	if _, ok := e.dbLineage[pageID]; ok {
		return nil
	}
	page, err := loadRecord(e.ctx, e.tx, pageID)
	if err != nil {
		return err
	}
	before, remembered := e.pageBefore[pageID]
	if !remembered {
		before = page.Item.Ver
		if change, ok := e.changes[pageID]; ok && change.Before != nil {
			before = change.Before.Item.Ver
		}
	}
	e.dbLineage[pageID] = undoAggregate{Ref: page.Item.Ref, Before: before}
	return nil
}

func (e *edit) dbAttachLineage(entry *undoEntry) error {
	entry.Aggregates = nil
	if len(e.dbLineage) == 0 {
		return nil
	}
	contributors := map[ID]map[int]bool{}
	add := func(id ID, ci int) {
		if contributors[id] == nil {
			contributors[id] = map[int]bool{}
		}
		contributors[id][ci] = true
	}
	pages := map[ID]ID{}
	for ci, component := range entry.Components {
		for _, part := range component.Parts {
			switch data := part.Written.Item.Data.(type) {
			case Page:
				add(part.Written.Item.ID(), ci)
			case Block:
				add(data.Page, ci)
			default:
				database, err := e.dbOwner(part.Written)
				if err != nil {
					return err
				}
				add(database, ci)
				page, ok := pages[database]
				if !ok {
					owner, err := loadRecord(e.ctx, e.tx, database)
					if err != nil {
						return err
					}
					block, err := loadRecord(e.ctx, e.tx, owner.Item.Data.(Database).Block)
					if err != nil {
						return err
					}
					page = block.Item.Data.(Block).Page
					pages[database] = page
				}
				add(page, ci)
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(e.dbLineage)) {
		edge := e.dbLineage[id]
		owner, err := loadRecord(e.ctx, e.tx, id)
		if err != nil {
			return err
		}
		if !owner.Alive || owner.Item.Ref != edge.Ref {
			continue
		}
		edge.Written = owner.Item.Ver
		edge.Components = slices.Sorted(maps.Keys(contributors[id]))
		if len(edge.Components) != 0 {
			entry.Aggregates = append(entry.Aggregates, edge)
		}
	}
	return nil
}

func (e *edit) dbDelete(record itemRecord) error {
	record.Alive, record.DeletedAt = false, e.seq
	return e.dbWrite(record)
}

func (e *edit) dbOrder(owner itemRecord, targets []Target, current []ID, kind Kind) ([]ID, []string, error) {
	order, removed := []ID{}, []string{}
	seen := map[ID]bool{}
	keys := map[string]bool{}
	for _, target := range targets {
		key := targetKey(target)
		if keys[key] {
			return nil, nil, invalidStructure()
		}
		keys[key] = true
		record, err := e.resolve(target, kind)
		if err != nil {
			var failure *editFailure
			if e.browser && errors.As(err, &failure) && failure.Reason == "gone" {
				removed = append(removed, "gone:"+targetKey(target))
				continue
			}
			return nil, nil, err
		}
		id := record.Item.ID()
		if seen[id] || !slices.Contains(current, id) {
			return nil, nil, invalidStructure()
		}
		seen[id] = true
		order = append(order, id)
	}
	for _, id := range current {
		if !seen[id] {
			order = append(order, id)
		}
	}
	if kind == PropertyKind {
		title := ID("")
		for _, id := range current {
			property, err := loadRecord(e.ctx, e.tx, id)
			if err != nil {
				return nil, nil, err
			}
			if property.Item.Data.(Property).Definition.Type == TitleType {
				title = id
				break
			}
		}
		index := slices.Index(order, title)
		if index != 0 {
			if !e.browser && seen[title] {
				return nil, nil, invalidStructure()
			}
			order = append([]ID{title}, slices.Delete(slices.Clone(order), index, index+1)...)
			if e.browser {
				removed = append(removed, "title_position")
			}
		}
	}
	return order, removed, nil
}

func (e *edit) dbNotice(view ID, removed []string) error {
	if len(removed) == 0 {
		return nil
	}
	if view != "" {
		existing := []string{}
		var raw string
		err := e.tx.QueryRowContext(e.ctx, `SELECT removed FROM view_notices WHERE view_id=? AND seq=?`, view, sequenceString(e.seq)).Scan(&raw)
		if err == nil {
			if err := json.Unmarshal([]byte(raw), &existing); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		existing = append(existing, removed...)
		encoded, err := json.Marshal(existing)
		if err != nil {
			return err
		}
		if _, err := e.tx.ExecContext(e.ctx, `INSERT INTO view_notices(view_id,seq,removed) VALUES(?,?,?) ON CONFLICT(view_id,seq) DO UPDATE SET removed=excluded.removed`, view, sequenceString(e.seq), string(encoded)); err != nil {
			return err
		}
	}
	if !e.browser {
		e.result.Warnings = append(e.result.Warnings, Warning{Code: "view_adjusted", Path: string(view), Message: strings.Join(removed, ", ")})
		return nil
	}
	if !slices.ContainsFunc(e.result.Results, func(r operationResult) bool { return r.Index == e.index && r.Status == "adjusted" }) {
		e.result.Results = append(e.result.Results, operationResult{Index: e.index, Status: "adjusted", Reason: "schema"})
	}
	return nil
}

func (e *edit) dbInput(database ID, input CellInput) (ID, CellValue, error) {
	property, err := e.resolve(input.Property, PropertyKind)
	if err != nil {
		return "", CellValue{}, err
	}
	data := property.Item.Data.(Property)
	if data.Database != database {
		return "", CellValue{}, invalidStructure()
	}
	value := CellValue{Type: data.Definition.Type}
	switch {
	case input.Value.Null:
		value.Value = nil
	case input.Value.Text != nil:
		value.Value = *input.Value.Text
	case input.Value.Bool != nil:
		value.Value = *input.Value.Bool
	case input.Value.Option != nil:
		option, err := e.resolve(*input.Value.Option, OptionKind)
		if err != nil {
			return "", value, err
		}
		if option.Item.Data.(Option).Property != property.Item.ID() {
			return "", value, invalidStructure()
		}
		value.Value = option.Item.ID()
	case input.Value.Options != nil:
		ids := []ID{}
		for _, target := range input.Value.Options {
			option, err := e.resolve(target, OptionKind)
			if err != nil {
				return "", value, err
			}
			if option.Item.Data.(Option).Property != property.Item.ID() {
				return "", value, invalidStructure()
			}
			ids = append(ids, option.Item.ID())
		}
		value.Value = ids
	default:
		return "", value, schemaFailure("value")
	}
	canonical, err := value.Canonical()
	if err != nil {
		return "", value, schemaFailure("value")
	}
	return property.Item.ID(), canonical, nil
}

func (e *edit) setCell(record itemRecord, input CellInput) error {
	row := record.Item.Data.(Row)
	property, value, err := e.dbInput(row.Database, input)
	if err != nil {
		return err
	}
	previous, ok := row.Cells[property]
	if !ok {
		previous = dbEmpty(value.Type)
	}
	if err := e.dbCellLimit(previous, value); err != nil {
		return err
	}
	row.Cells = maps.Clone(row.Cells)
	row.Cells[property] = value
	record.Item.Data = row
	return e.dbWrite(record, CellGroup(property))
}

func (e *edit) dbCellLimit(before, after CellValue) error {
	length := func(value CellValue) int {
		switch text := value.Value.(type) {
		case string:
			return utf8.RuneCountInString(text)
		case Decimal:
			return len(text)
		}
		return 0
	}
	if err := e.schemaLimit(length(before), length(after), MaxCellCodePoints); err != nil {
		return err
	}
	if after.Type == URLType && after.Value != nil {
		old := 0
		if before.Type == URLType && before.Value != nil {
			old = len(before.Value.(string))
		}
		return e.schemaLimit(old, len(after.Value.(string)), MaxURLBytes)
	}
	return nil
}

func (e *edit) createRow(op *CreateRowOp) error {
	owner, err := e.resolve(op.Database, DatabaseKind)
	if err != nil {
		return err
	}
	rows, err := e.dbRows(owner.Item.ID())
	if err != nil {
		return err
	}
	if err := e.schemaLimit(len(rows), len(rows)+1, MaxRowsPerDatabase); err != nil {
		return err
	}
	position, err := e.dbPlace(owner.Item.ID(), op.Before, nil, nil)
	if err != nil {
		return err
	}
	cells := map[ID]CellValue{}
	if op.View != nil {
		view, err := e.resolve(*op.View, ViewKind)
		if err != nil {
			return err
		}
		data := view.Item.Data.(View)
		if data.Database != owner.Item.ID() {
			return invalidStructure()
		}
		defaults, err := e.dbDefaults(data)
		if err != nil {
			return err
		}
		cells = defaults
	}
	seen := map[ID]bool{}
	for _, input := range op.Cells {
		property, value, err := e.dbInput(owner.Item.ID(), input)
		if err != nil {
			return err
		}
		if seen[property] {
			return invalidStructure()
		}
		seen[property] = true
		if err := e.dbCellLimit(dbEmpty(value.Type), value); err != nil {
			return err
		}
		cells[property] = value
	}
	row, err := e.newItem(RowKind, op.CreateFields, Row{Database: owner.Item.ID(), Position: position, Cells: cells})
	if err != nil {
		return err
	}
	e.component = row.Item.ID()
	// Creation was recorded before the component identity was known.
	change := e.changes[row.Item.ID()]
	change.Component = e.component
	e.changes[row.Item.ID()] = change
	return e.dbWrite(row)
}

func (e *edit) dbDefaults(view View) (map[ID]CellValue, error) {
	defaults := map[ID]CellValue{}
	conflicts := map[ID]bool{}
	for _, filter := range view.Definition.Filter {
		if filter.Op != Is && filter.Op != IsChecked {
			continue
		}
		property, err := loadRecord(e.ctx, e.tx, filter.Property)
		if err != nil {
			return nil, err
		}
		typ := property.Item.Data.(Property).Definition.Type
		value := CellValue{Type: typ}
		switch typ {
		case TitleType, TextType, URLType:
			var text string
			if err := json.Unmarshal(filter.Value, &text); err != nil {
				return nil, err
			}
			value.Value = text
		case SelectType:
			var option struct {
				Option ID `json:"option"`
			}
			if err := json.Unmarshal(filter.Value, &option); err != nil {
				return nil, err
			}
			value.Value = option.Option
		case DateType:
			var date CalendarDate
			if err := json.Unmarshal(filter.Value, &date); err != nil {
				return nil, err
			}
			value.Value = date
		case CheckboxType:
			value.Value = true
		default:
			continue
		}
		if value.Validate() != nil {
			conflicts[filter.Property] = true
			continue
		}
		if _, exists := defaults[filter.Property]; !exists {
			defaults[filter.Property] = value
		}
	}
	for property, value := range defaults {
		column := dbQueryColumn{id: property, typ: value.Type}
		for _, filter := range view.Definition.Filter {
			if filter.Property != property {
				continue
			}
			raw, err := storedFilterToWire(filter.Value, func(id ID) (Target, error) { return Target{Ref: &Ref{ID: id, Inc: 1}}, nil })
			if err != nil {
				return nil, err
			}
			wire := Filter{Property: Target{Ref: &Ref{ID: property, Inc: 1}}, Op: filter.Op, Value: raw}
			if !column.matches(column.value(value), dbQueryFilter{op: wire.Op, value: column.operand(wire)}) {
				conflicts[property] = true
				break
			}
		}
	}
	for id := range conflicts {
		delete(defaults, id)
	}
	return defaults, nil
}

func (e *edit) dbSelectedRows(targets []Target) ([]itemRecord, error) {
	records := []itemRecord{}
	seen := map[ID]bool{}
	var database ID
	for _, target := range targets {
		record, err := e.resolve(target, RowKind)
		if err != nil {
			return nil, err
		}
		owner := record.Item.Data.(Row).Database
		if database != "" && database != owner {
			return nil, invalidStructure()
		}
		database = owner
		if seen[record.Item.ID()] {
			return nil, invalidStructure()
		}
		seen[record.Item.ID()] = true
		records = append(records, record)
	}
	slices.SortFunc(records, func(a, b itemRecord) int {
		if cmp := strings.Compare(a.Item.Data.(Row).Position, b.Item.Data.(Row).Position); cmp != 0 {
			return cmp
		}
		return strings.Compare(string(a.Item.ID()), string(b.Item.ID()))
	})
	return records, nil
}

func (e *edit) dbPlace(database ID, before, after *Target, exclude map[ID]bool) (string, error) {
	rows, err := e.dbRows(database)
	if err != nil {
		return "", err
	}
	rows = slices.DeleteFunc(rows, func(r itemRecord) bool { return exclude[r.Item.ID()] })
	index := len(rows)
	anchor := before
	if after != nil {
		anchor = after
	}
	if anchor != nil {
		record, err := e.resolve(*anchor, RowKind)
		if err != nil {
			return "", err
		}
		index = slices.IndexFunc(rows, func(r itemRecord) bool { return r.Item.ID() == record.Item.ID() })
		if index < 0 {
			return "", invalidStructure()
		}
		if after != nil {
			index++
		}
	}
	bounds := func() (string, string) {
		left, right := "", ""
		if index > 0 {
			left = rows[index-1].Item.Data.(Row).Position
		}
		if index < len(rows) {
			right = rows[index].Item.Data.(Row).Position
		}
		return left, right
	}
	left, right := bounds()
	position, err := positionBetween(left, right)
	if err == nil && len(position) <= MaxPositionBytes {
		return position, nil
	}
	positions := rebalancePositions(len(rows))
	for i, record := range rows {
		if e.oldPlaces == nil {
			e.oldPlaces = map[ID]Item{}
		}
		if _, exists := e.oldPlaces[record.Item.ID()]; !exists {
			e.oldPlaces[record.Item.ID()] = record.Item
		}
		row := record.Item.Data.(Row)
		row.Position = positions[i]
		record.Item.Data = row
		if err := putRecord(e.ctx, e.tx, record); err != nil {
			return "", err
		}
		e.noteEffect(record)
		rows[i] = record
	}
	left, right = bounds()
	return positionBetween(left, right)
}

func (e *edit) moveRows(op *MoveRowsOp) error {
	records, err := e.dbSelectedRows(op.Rows)
	if err != nil || len(records) == 0 {
		return err
	}
	e.component = records[0].Item.ID()
	database := records[0].Item.Data.(Row).Database
	exclude := map[ID]bool{}
	for _, r := range records {
		exclude[r.Item.ID()] = true
	}
	before, after := op.Before, (*Target)(nil)
	for _, record := range records {
		position, err := e.dbPlace(database, before, after, exclude)
		if err != nil {
			return err
		}
		// A rekey of this row in an earlier placement is not a logical move.
		record, err = loadRecord(e.ctx, e.tx, record.Item.ID())
		if err != nil {
			return err
		}
		row := record.Item.Data.(Row)
		row.Position = position
		record.Item.Data = row
		if err := e.dbWrite(record, PlaceGroup); err != nil {
			return err
		}
		delete(exclude, record.Item.ID())
		ref := record.Item.Ref
		after = &Target{Ref: &ref}
		before = nil
	}
	return nil
}

func (e *edit) deleteDatabaseOf(block ID) error {
	record, err := loadRecord(e.ctx, e.tx, block)
	if err != nil {
		return err
	}
	data := record.Item.Data.(Block)
	if data.Content.Kind != InlineDatabase {
		return nil
	}
	previous := e.component
	if e.component == "" {
		e.component = block
	}
	defer func() { e.component = previous }()
	database, err := loadRecord(e.ctx, e.tx, data.Content.Database)
	if err != nil {
		return err
	}
	for _, kind := range []Kind{PropertyKind, RowKind, ViewKind} {
		members, err := e.dbMembers(database.Item.ID(), kind)
		if err != nil {
			return err
		}
		for _, member := range members {
			if property, ok := member.Item.Data.(Property); ok {
				for _, id := range property.OptionOrder {
					option, err := loadRecord(e.ctx, e.tx, id)
					if err != nil {
						return err
					}
					if err := e.dbDelete(option); err != nil {
						return err
					}
				}
			}
			if err := e.dbDelete(member); err != nil {
				return err
			}
		}
	}
	return e.dbDelete(database)
}

func (e *edit) copyDatabase(source ID, ownerCopy ID, remap map[ID]ID) error {
	database, err := loadRecord(e.ctx, e.tx, source)
	if err != nil {
		return err
	}
	if !database.Alive || database.Item.Kind() != DatabaseKind {
		return gone()
	}
	owner, err := loadRecord(e.ctx, e.tx, ownerCopy)
	if err != nil {
		return err
	}
	if !owner.Alive || owner.Item.Kind() != BlockKind {
		return invalidStructure()
	}
	records := []itemRecord{database}
	for _, kind := range []Kind{PropertyKind, RowKind, ViewKind} {
		members, err := e.dbMembers(source, kind)
		if err != nil {
			return err
		}
		records = append(records, members...)
		if kind == PropertyKind {
			for _, member := range members {
				for _, id := range member.Item.Data.(Property).OptionOrder {
					option, err := loadRecord(e.ctx, e.tx, id)
					if err != nil {
						return err
					}
					records = append(records, option)
				}
			}
		}
	}
	for _, record := range records {
		if _, ok := remap[record.Item.ID()]; ok {
			continue
		}
		id, err := NewID(record.Item.Kind())
		if err != nil {
			return err
		}
		remap[record.Item.ID()] = id
	}
	remap[database.Item.Data.(Database).Block] = ownerCopy
	copies, err := remapCopies(records, remap, nil, e.seq, databaseRemapper{})
	if err != nil {
		return err
	}
	previous := e.component
	e.component = ownerCopy
	defer func() { e.component = previous }()
	for i, copy := range copies {
		var allocated int
		if err := e.tx.QueryRowContext(e.ctx, `SELECT count(*) FROM allocated_ids WHERE id=?`, copy.Item.ID()).Scan(&allocated); err != nil {
			return err
		}
		if allocated != 0 {
			return schemaFailure("allocated_id")
		}
		if err := e.dbWrite(copy); err != nil {
			return err
		}
		e.result.Copies = append(e.result.Copies, copyMapping{Index: e.index, Source: records[i].Item.Ref, Ref: copy.Item.Ref, Ver: copy.Item.Ver})
	}
	block := owner.Item.Data.(Block)
	block.Content = Content{Kind: InlineDatabase, Database: remap[source]}
	owner.Item.Data = block
	return e.write(owner, ContentGroup)
}

func (e *edit) validateDatabases() error {
	databases, err := e.records(`SELECT id FROM items WHERE kind='database' AND alive=1 ORDER BY id`)
	if err != nil {
		return err
	}
	for _, record := range databases {
		database := record.Item.Data.(Database)
		owner, err := loadRecord(e.ctx, e.tx, database.Block)
		if err != nil {
			return err
		}
		block, ok := owner.Item.Data.(Block)
		if !ok || !owner.Alive || block.Content.Kind != InlineDatabase || block.Content.Database != record.Item.ID() {
			return invalidStructure()
		}
		properties, err := e.dbMembers(record.Item.ID(), PropertyKind)
		if err != nil {
			return err
		}
		defs := map[ID]PropertyDef{}
		options := map[ID]map[ID]bool{}
		title := ID("")
		for _, property := range properties {
			data := property.Item.Data.(Property)
			defs[property.Item.ID()] = data.Definition
			if data.Definition.Type == TitleType {
				if title != "" {
					return invalidStructure()
				}
				title = property.Item.ID()
			}
			options[property.Item.ID()] = map[ID]bool{}
			for _, id := range data.OptionOrder {
				option, err := loadRecord(e.ctx, e.tx, id)
				if err != nil {
					return err
				}
				if !option.Alive || option.Item.Data.(Option).Property != property.Item.ID() {
					return invalidStructure()
				}
				options[property.Item.ID()][id] = true
			}
		}
		if title == "" || len(database.PropertyOrder) != len(properties) || database.PropertyOrder[0] != title {
			return invalidStructure()
		}
		for _, id := range database.PropertyOrder {
			if _, ok := defs[id]; !ok {
				return invalidStructure()
			}
		}
		views, err := e.dbMembers(record.Item.ID(), ViewKind)
		if err != nil {
			return err
		}
		if len(views) == 0 || len(views) != len(database.ViewOrder) {
			return invalidStructure()
		}
		for _, view := range views {
			if !slices.Contains(database.ViewOrder, view.Item.ID()) {
				return invalidStructure()
			}
			wire, err := view.Item.Data.(View).Definition.Wire(func(id ID) (Ref, error) { return Ref{ID: id, Inc: 1}, nil })
			if err != nil {
				return err
			}
			if err := wire.ValidateSchema(defs); err != nil {
				return invalidStructure()
			}
			for _, filter := range wire.Filter {
				typ := defs[filter.Property.Ref.ID].Type
				if !filter.Op.NeedsValue() || typ != SelectType && typ != MultiSelectType {
					continue
				}
				targets := []Target{}
				if filter.Op == IsAnyOf {
					if err := json.Unmarshal(filter.Value, &targets); err != nil {
						return err
					}
				} else {
					var target Target
					if err := json.Unmarshal(filter.Value, &target); err != nil {
						return err
					}
					targets = append(targets, target)
				}
				for _, target := range targets {
					if !options[filter.Property.Ref.ID][target.Ref.ID] {
						return invalidStructure()
					}
				}
			}
		}
		rows, err := e.dbRows(record.Item.ID())
		if err != nil {
			return err
		}
		for _, row := range rows {
			for property, cell := range row.Item.Data.(Row).Cells {
				definition, ok := defs[property]
				if !ok || definition.Type != cell.Type {
					return invalidStructure()
				}
				switch value := cell.Value.(type) {
				case ID:
					if !options[property][value] {
						return invalidStructure()
					}
				case []ID:
					for _, id := range value {
						if !options[property][id] {
							return invalidStructure()
						}
					}
				}
			}
		}
	}
	return nil
}

func makeDatabaseUndoEntry(e *edit, changes map[ID]itemChange) (undoEntry, error) {
	return makeUndoComponents(e, changes, map[undoList]map[ID]undoNeighbors{})
}

func makeDatabaseUndoComponents(e *edit, changes map[ID]itemChange, neighbors map[undoList]map[ID]undoNeighbors, bound bool) (undoEntry, error) {
	entry := undoEntry{}
	var component undoComponent
	for _, id := range slices.Sorted(maps.Keys(changes)) {
		change := changes[id]
		part := undoPart{Desired: change.Before, Written: change.After, Groups: slices.Clone(change.Groups)}
		structural := bound || len(changes) > 1 || change.Before == nil || !change.Before.Alive || !change.After.Alive || slices.Contains(change.Groups, PlaceGroup)
		if structural && change.Before != nil && change.Before.Alive && (id.Kind() == RowKind || id.Kind() == PageKind || id.Kind() == BlockKind) {
			page, parent, _ := undoPlace(change.Before.Item)
			key := undoList{Kind: id.Kind(), Page: page}
			if parent != nil {
				key.Parent = *parent
			}
			if row, ok := change.Before.Item.Data.(Row); ok {
				key.Page = row.Database
			}
			if neighbors[key] == nil {
				var list map[ID]undoNeighbors
				var err error
				if id.Kind() == RowKind {
					list, err = e.dbOldNeighbors(change.Before.Item, e.changes)
				} else {
					list, err = undoOldNeighbors(e, change.Before.Item, e.changes)
				}
				if err != nil {
					return entry, err
				}
				neighbors[key] = list
			}
			part.Neighbors = neighbors[key][id]
		}
		if !structural {
			for _, group := range change.Groups {
				single := part
				single.Groups = []GroupKey{group}
				entry.Components = append(entry.Components, undoComponent{Parts: []undoPart{single}})
			}
		} else {
			component.Parts = append(component.Parts, part)
		}
	}
	if len(component.Parts) > 0 {
		entry.Components = append(entry.Components, component)
	}
	return entry, nil
}

func (e *edit) dbOldNeighbors(item Item, changes map[ID]itemChange) (map[ID]undoNeighbors, error) {
	row := item.Data.(Row)
	records, err := e.dbRows(row.Database)
	if err != nil {
		return nil, err
	}
	byID := map[ID]Item{}
	for _, record := range records {
		byID[record.Item.ID()] = record.Item
	}
	for id, change := range changes {
		delete(byID, id)
		if change.Before != nil && change.Before.Alive {
			if before, ok := change.Before.Item.Data.(Row); ok && before.Database == row.Database {
				byID[id] = change.Before.Item
			}
		}
	}
	for id, original := range e.oldPlaces {
		if before, ok := original.Data.(Row); ok && before.Database == row.Database {
			if _, changed := changes[id]; !changed {
				byID[id] = original
			}
		}
	}
	list := slices.Collect(maps.Values(byID))
	slices.SortFunc(list, func(a, b Item) int {
		if cmp := strings.Compare(a.Data.(Row).Position, b.Data.(Row).Position); cmp != 0 {
			return cmp
		}
		return strings.Compare(string(a.ID()), string(b.ID()))
	})
	neighbors := make(map[ID]undoNeighbors, len(list))
	for i, value := range list {
		var result undoNeighbors
		if i > 0 {
			ref := list[i-1].Ref
			result.After = &ref
		}
		if i+1 < len(list) {
			ref := list[i+1].Ref
			result.Before = &ref
		}
		neighbors[value.ID()] = result
	}
	return neighbors, nil
}

func databaseUndoGroups(item Item) []GroupKey {
	switch data := item.Data.(type) {
	case Database:
		return []GroupKey{TitleGroup, PropertyOrderGroup, ViewOrderGroup}
	case Property:
		return []GroupKey{DefinitionGroup, OptionOrderGroup}
	case Option, View:
		return []GroupKey{DefinitionGroup}
	case Row:
		groups := []GroupKey{PlaceGroup}
		for _, id := range slices.Sorted(maps.Keys(data.Cells)) {
			groups = append(groups, CellGroup(id))
		}
		return groups
	default:
		return undoOwnGroups(item)
	}
}

func (e *edit) databaseUndoGuard(component undoComponent, start map[ID]itemRecord, missing map[ID]bool) (string, error) {
	deleting := map[ID]bool{}
	for _, part := range component.Parts {
		id := part.Written.Item.ID()
		current, ok := start[id]
		if missing[id] || !ok || current.Item.Ref.Inc != part.incarnation() || current.Alive != part.Written.Alive {
			return "missing", nil
		}
		if !current.Alive {
			if current.DeletedAt != part.Written.DeletedAt {
				return "later", nil
			}
			continue
		}
		groups := part.Groups
		if part.Desired == nil || !part.Desired.Alive {
			groups = databaseUndoGroups(current.Item)
			deleting[id] = true
			version := part.GuardVersion
			if version == 0 {
				version = part.Written.Item.Ver
			}
			if current.Item.Ver != version {
				return "later", nil
			}
		}
		for _, group := range groups {
			got, err := current.Item.GroupVersion(group)
			if err != nil {
				return "", err
			}
			want, err := part.version(group)
			if err != nil {
				return "", err
			}
			if got != want {
				return "later", nil
			}
		}
	}
	for id := range deleting {
		var query string
		args := []any{id}
		switch id.Kind() {
		case PageKind:
			query = `SELECT p.id FROM pages p JOIN items i ON i.id=p.id WHERE i.alive=1 AND p.parent=? UNION ALL SELECT b.id FROM blocks b JOIN items i ON i.id=b.id WHERE i.alive=1 AND b.page=?`
			args = append(args, id)
		case BlockKind:
			query = `SELECT b.id FROM blocks b JOIN items i ON i.id=b.id WHERE i.alive=1 AND b.parent=? UNION ALL SELECT d.id FROM databases d JOIN items i ON i.id=d.id WHERE i.alive=1 AND d.block=?`
			args = append(args, id)
		case DatabaseKind:
			query = `SELECT p.id FROM properties p JOIN items i ON i.id=p.id WHERE i.alive=1 AND p.database_id=? UNION ALL SELECT r.id FROM rows r JOIN items i ON i.id=r.id WHERE i.alive=1 AND r.database_id=? UNION ALL SELECT v.id FROM views v JOIN items i ON i.id=v.id WHERE i.alive=1 AND v.database_id=?`
			args = append(args, id, id)
		case PropertyKind:
			query = `SELECT o.id FROM options o JOIN items i ON i.id=o.id WHERE i.alive=1 AND o.property_id=?`
		default:
			continue
		}
		children, err := e.records(query, args...)
		if err != nil {
			return "", err
		}
		for _, child := range children {
			if !deleting[child.Item.ID()] {
				return "later", nil
			}
		}
	}
	return "", nil
}

func databaseUndoAssignGroup(current *Item, desired Item, group GroupKey) error {
	switch data := current.Data.(type) {
	case Database:
		before := desired.Data.(Database)
		switch group {
		case TitleGroup:
			data.Title = before.Title
		case PropertyOrderGroup:
			data.PropertyOrder = slices.Clone(before.PropertyOrder)
		case ViewOrderGroup:
			data.ViewOrder = slices.Clone(before.ViewOrder)
		default:
			return invalidStructure()
		}
		current.Data = data
	case Property:
		before := desired.Data.(Property)
		switch group {
		case DefinitionGroup:
			data.Definition = before.Definition
		case OptionOrderGroup:
			data.OptionOrder = slices.Clone(before.OptionOrder)
		default:
			return invalidStructure()
		}
		current.Data = data
	case Option:
		if group != DefinitionGroup {
			return invalidStructure()
		}
		current.Data = desired.Data.(Option)
	case View:
		if group != DefinitionGroup {
			return invalidStructure()
		}
		current.Data = desired.Data.(View)
	case Row:
		if !strings.HasPrefix(string(group), "cell:") {
			return invalidStructure()
		}
		id := ID(strings.TrimPrefix(string(group), "cell:"))
		data.Cells = maps.Clone(data.Cells)
		if value, ok := desired.Data.(Row).Cells[id]; ok {
			data.Cells[id] = value
		} else {
			delete(data.Cells, id)
		}
		current.Data = data
	default:
		return undoAssignGroup(current, desired, group)
	}
	return nil
}

func (e *edit) applyDatabaseUndoComponent(component undoComponent) error {
	// Temporarily tombstone name-bearing items so swaps never collide in the live index.
	// Keep the real before-images; staging is not an inverse effect.
	staged := map[ID]itemRecord{}
	for _, part := range component.Parts {
		kind := part.Written.Item.Kind()
		if databaseUndoKind(kind) {
			database, err := e.dbOwner(part.Written)
			if err != nil {
				return err
			}
			if err := e.dbTrackLineage(database); err != nil {
				return err
			}
		}
		if kind == PropertyKind || kind == OptionKind {
			record, err := loadRecord(e.ctx, e.tx, part.Written.Item.ID())
			if err != nil {
				return err
			}
			staged[record.Item.ID()] = record
			record.Alive, record.DeletedAt = false, e.seq
			if err := putRecord(e.ctx, e.tx, record); err != nil {
				return err
			}
		}
	}
	names := map[struct {
		Owner ID
		Name  string
	}]bool{}
	for _, part := range component.Parts {
		if kind := part.Written.Item.Kind(); kind != PropertyKind && kind != OptionKind {
			continue
		}
		if part.Desired == nil || !part.Desired.Alive {
			continue
		}
		current, err := loadRecord(e.ctx, e.tx, part.Written.Item.ID())
		if err != nil {
			return err
		}
		item := current.Item
		if !part.Written.Alive {
			item = part.Desired.Item
		} else if slices.Contains(part.Groups, DefinitionGroup) {
			if err := databaseUndoAssignGroup(&item, part.Desired.Item, DefinitionGroup); err != nil {
				return err
			}
		}
		var owner ID
		var name string
		switch data := item.Data.(type) {
		case Property:
			owner, name = data.Database, data.Definition.Name
		case Option:
			owner, name = data.Property, data.Name
		default:
			continue
		}
		key := struct {
			Owner ID
			Name  string
		}{owner, name}
		if names[key] {
			return invalidStructure()
		}
		names[key] = true
		if err := e.schemaName(owner, item.ID(), name, item.Kind()); err != nil {
			return err
		}
	}

	reviving := map[ID]bool{}
	for _, part := range component.Parts {
		if part.Desired != nil && part.Desired.Alive && !part.Written.Alive {
			reviving[part.Written.Item.ID()] = true
		}
	}
	for _, part := range component.Parts {
		current, err := loadRecord(e.ctx, e.tx, part.Written.Item.ID())
		if err != nil {
			return err
		}
		if original, ok := staged[current.Item.ID()]; ok {
			current = original
		}
		if part.Desired == nil || !part.Desired.Alive {
			current.Alive, current.DeletedAt = false, e.seq
		} else if !part.Written.Alive {
			current = *part.Desired
			current.Alive, current.DeletedAt = true, 0
			current.Item.Ref.Inc = Incarnation(e.seq)
			current.Item.V = map[GroupKey]Version{}
			current.Item.Ver, current.Aggregate = Version(e.seq), Version(e.seq)
		} else {
			for _, group := range part.Groups {
				if group != PlaceGroup {
					if err := databaseUndoAssignGroup(&current.Item, part.Desired.Item, group); err != nil {
						return err
					}
				}
			}
		}
		groups := slices.DeleteFunc(slices.Clone(part.Groups), func(group GroupKey) bool { return group == PlaceGroup })
		if err := e.write(current, groups...); err != nil {
			return err
		}
		if original, ok := staged[current.Item.ID()]; ok {
			change := e.changes[current.Item.ID()]
			change.Before = &original
			e.changes[current.Item.ID()] = change
		}
	}
	places := slices.DeleteFunc(slices.Clone(component.Parts), func(part undoPart) bool {
		return part.Desired == nil || !part.Desired.Alive || (part.Written.Alive && !slices.Contains(part.Groups, PlaceGroup)) || (part.Written.Item.Kind() != RowKind && part.Written.Item.Kind() != BlockKind && part.Written.Item.Kind() != PageKind)
	})
	slices.SortStableFunc(places, func(a, b undoPart) int {
		position := func(part undoPart) string {
			if row, ok := part.Desired.Item.Data.(Row); ok {
				return row.Position
			}
			_, _, p := undoPlace(part.Desired.Item)
			return p
		}
		if cmp := strings.Compare(position(a), position(b)); cmp != 0 {
			return cmp
		}
		return strings.Compare(string(a.Written.Item.ID()), string(b.Written.Item.ID()))
	})
	for _, part := range places {
		if part.Written.Item.Kind() != RowKind {
			if err := undoRestorePlace(e, part, reviving); err != nil {
				return err
			}
			continue
		}
		row := part.Desired.Item.Data.(Row)
		owner, err := loadRecord(e.ctx, e.tx, row.Database)
		if err != nil {
			return err
		}
		if !owner.Alive {
			return invalidStructure()
		}
		anchor := func(ref *Ref) (*Target, error) {
			if ref == nil {
				return nil, nil
			}
			record, err := loadRecord(e.ctx, e.tx, ref.ID)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			if !record.Alive || record.Item.Data.(Row).Database != row.Database || (!reviving[ref.ID] && record.Item.Ref != *ref) {
				return nil, nil
			}
			return &Target{Ref: &record.Item.Ref}, nil
		}
		after, err := anchor(part.Neighbors.After)
		if err != nil {
			return err
		}
		var before *Target
		if after == nil {
			before, err = anchor(part.Neighbors.Before)
			if err != nil {
				return err
			}
		}
		position, err := e.dbPlace(row.Database, before, after, map[ID]bool{part.Written.Item.ID(): true})
		if err != nil {
			return err
		}
		current, err := loadRecord(e.ctx, e.tx, part.Written.Item.ID())
		if err != nil {
			return err
		}
		data := current.Item.Data.(Row)
		data.Position = position
		current.Item.Data = data
		if err := e.dbWrite(current, PlaceGroup); err != nil {
			return err
		}
	}
	databases := map[ID]bool{}
	for _, part := range component.Parts {
		record, err := loadRecord(e.ctx, e.tx, part.Written.Item.ID())
		if err != nil {
			return err
		}
		if record.Item.Kind() != PageKind && record.Item.Kind() != BlockKind {
			database, err := e.dbOwner(record)
			if err != nil {
				return err
			}
			databases[database] = true
		}
	}
	for database := range databases {
		record, err := loadRecord(e.ctx, e.tx, database)
		if err != nil {
			return err
		}
		if err := e.dbBump(record.Item.ID()); err != nil {
			return err
		}
	}
	return e.validateDatabases()
}

type databaseRemapper struct{}

func (databaseRemapper) Remap(data ItemData, ids map[ID]ID) (ItemData, error) {
	id := func(source ID) ID {
		if target, ok := ids[source]; ok {
			return target
		}
		return source
	}
	order := func(source []ID) []ID {
		result := make([]ID, len(source))
		for i, value := range source {
			result[i] = id(value)
		}
		return result
	}
	switch value := data.(type) {
	case Database:
		value.Block = id(value.Block)
		value.PropertyOrder = order(value.PropertyOrder)
		value.ViewOrder = order(value.ViewOrder)
		return value, nil
	case Property:
		value.Database = id(value.Database)
		value.OptionOrder = order(value.OptionOrder)
		return value, nil
	case Option:
		value.Property = id(value.Property)
		return value, nil
	case Row:
		value.Database = id(value.Database)
		cells := map[ID]CellValue{}
		for property, cell := range value.Cells {
			if cell.Type == SelectType && cell.Value != nil {
				cell.Value = id(cell.Value.(ID))
			}
			if cell.Type == MultiSelectType {
				mapped := order(cell.Value.([]ID))
				slices.Sort(mapped)
				cell.Value = mapped
			}
			cells[id(property)] = cell
		}
		value.Cells = cells
		return value, nil
	case View:
		value.Database = id(value.Database)
		wire, err := value.Definition.Wire(func(source ID) (Ref, error) { return Ref{ID: source, Inc: 1}, nil })
		if err != nil {
			return nil, err
		}
		remapTarget := func(target Target) Target { ref := *target.Ref; ref.ID = id(ref.ID); return Target{Ref: &ref} }
		for i, target := range wire.Display.Visible {
			wire.Display.Visible[i] = remapTarget(target)
		}
		for i := range wire.Display.Widths {
			wire.Display.Widths[i].Property = remapTarget(wire.Display.Widths[i].Property)
		}
		if wire.Display.GroupBy != nil {
			target := remapTarget(*wire.Display.GroupBy)
			wire.Display.GroupBy = &target
		}
		for i := range wire.Sort {
			wire.Sort[i].Property = remapTarget(wire.Sort[i].Property)
		}
		for i := range wire.Filter {
			wire.Filter[i].Property = remapTarget(wire.Filter[i].Property)
			stored, err := wireFilterToStored(wire.Filter[i].Value, func(target Target) (ID, error) { return id(target.Ref.ID), nil })
			if err != nil {
				return nil, err
			}
			wire.Filter[i].Value, err = storedFilterToWire(stored, func(source ID) (Target, error) { return Target{Ref: &Ref{ID: source, Inc: 1}}, nil })
			if err != nil {
				return nil, err
			}
		}
		definition, err := StoreView(wire)
		if err != nil {
			return nil, err
		}
		value.Definition = definition
		return value, nil
	}
	return nil, invalidStructure()
}
