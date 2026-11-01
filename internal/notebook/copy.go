package notebook

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

type copyPlacement struct {
	Page     ID
	Parent   *ID
	Position string
}

// Database copying must extend the same identity pass rather than retain live owners.
type databaseCopyExtension interface {
	Remap(ItemData, map[ID]ID) (ItemData, error)
}

func remapCopies(source []itemRecord, ids map[ID]ID, roots map[ID]copyPlacement, seq Sequence, extension databaseCopyExtension) ([]itemRecord, error) {
	byID := make(map[ID]itemRecord, len(source))
	used := make(map[ID]bool, len(source))
	for _, record := range source {
		id := record.Item.ID()
		mapped, ok := ids[id]
		if !ok {
			return nil, fmt.Errorf("copy missing identity for %s", id)
		}
		if err := mapped.ValidateKind(record.Item.Kind()); err != nil {
			return nil, err
		}
		if used[mapped] {
			return nil, fmt.Errorf("copy identity collision at %s", mapped)
		}
		if _, exists := byID[id]; exists {
			return nil, fmt.Errorf("copy repeats source %s", id)
		}
		byID[id], used[mapped] = record, true
	}
	remap := func(id ID) ID {
		if mapped, ok := ids[id]; ok {
			return mapped
		}
		return id
	}
	parent := func(id *ID) *ID {
		if id == nil {
			return nil
		}
		mapped := remap(*id)
		return &mapped
	}
	copies := make([]itemRecord, 0, len(source))
	for _, record := range source {
		var data ItemData
		switch value := record.Item.Data.(type) {
		case Page:
			value.Parent = parent(value.Parent)
			if place, ok := roots[record.Item.ID()]; ok {
				value.Parent = cloneCopyParent(place.Parent)
				value.Position = place.Position
			}
			data = value
		case Block:
			value.Page = remap(value.Page)
			value.Parent = parent(value.Parent)
			// All descendants of a selected block root move to its destination page.
			cursor := record
			seen := map[ID]bool{}
			for {
				if place, ok := roots[cursor.Item.ID()]; ok {
					value.Page = place.Page
					break
				}
				ancestor, ok := cursor.Item.Data.(Block)
				if !ok || ancestor.Parent == nil {
					break
				}
				if seen[cursor.Item.ID()] {
					return nil, invalidStructure()
				}
				seen[cursor.Item.ID()] = true
				cursor, ok = byID[*ancestor.Parent]
				if !ok {
					break
				}
			}
			if place, ok := roots[record.Item.ID()]; ok {
				value.Parent = cloneCopyParent(place.Parent)
				value.Position = place.Position
			}
			value.Content.Text = remapCopyText(value.Content.Text, remap)
			value.Content.Caption = remapCopyText(value.Content.Caption, remap)
			if value.Content.Kind == PageLink {
				value.Content.Target = remap(value.Content.Target)
			}
			if value.Content.Kind == InlineDatabase {
				if extension == nil {
					return nil, &editFailure{Status: "refused", Reason: "unsupported"}
				}
				if mapped, ok := ids[value.Content.Database]; !ok || mapped.Kind() != DatabaseKind {
					return nil, fmt.Errorf("copy missing inline database %s", value.Content.Database)
				}
				value.Content.Database = remap(value.Content.Database)
			}
			data = value
		default:
			if extension == nil {
				return nil, &editFailure{Status: "refused", Reason: "unsupported"}
			}
			var err error
			data, err = extension.Remap(record.Item.Data, ids)
			if err != nil {
				return nil, err
			}
		}
		copy := itemRecord{
			Item:      Item{Ref: Ref{ID: ids[record.Item.ID()], Inc: Incarnation(seq)}, Ver: Version(seq), V: map[GroupKey]Version{}, Data: data},
			Alive:     true,
			Aggregate: Version(seq),
		}
		if err := copy.Item.Validate(); err != nil {
			return nil, err
		}
		copies = append(copies, copy)
	}
	return copies, nil
}

func cloneCopyParent(id *ID) *ID {
	if id == nil {
		return nil
	}
	copy := *id
	return &copy
}

func remapCopyText(text RichText, remap func(ID) ID) RichText {
	copy := slices.Clone(text)
	for index := range copy {
		copy[index].Marks = slices.Clone(copy[index].Marks)
		if copy[index].Link != nil {
			link := *copy[index].Link
			if link.Page != "" {
				link.Page = remap(link.Page)
			}
			copy[index].Link = &link
		}
	}
	return copy
}

func (e *edit) duplicatePage(op *DuplicatePageOp) error {
	root, err := e.resolve(op.Page, PageKind)
	if err != nil {
		return err
	}
	page := root.Item.Data.(Page)
	return e.duplicateSubtrees([]itemRecord{root}, "", page.Parent, root.Item.Ref)
}

func (e *edit) duplicateBlocks(op *DuplicateBlocksOp) error {
	roots, err := e.selected(op.Blocks, BlockKind)
	if err != nil {
		return err
	}
	anchor, err := e.resolve(op.After, BlockKind)
	if err != nil {
		return err
	}
	block := anchor.Item.Data.(Block)
	return e.duplicateSubtrees(roots, block.Page, block.Parent, anchor.Item.Ref)
}

func (e *edit) duplicateSubtrees(roots []itemRecord, page ID, parent *ID, anchor Ref) error {
	var source []itemRecord
	placements := make(map[ID]copyPlacement, len(roots))
	for _, root := range roots {
		subtree, err := e.subtree(root)
		if err != nil {
			return err
		}
		source = append(source, subtree...)
		place := copyPlacement{Page: page, Parent: parent}
		switch value := root.Item.Data.(type) {
		case Page:
			place.Position = value.Position
		case Block:
			place.Position = value.Position
		}
		placements[root.Item.ID()] = place
	}
	ids := make(map[ID]ID, len(source))
	for _, record := range source {
		id, err := NewID(record.Item.Kind())
		if err != nil {
			return err
		}
		ids[record.Item.ID()] = id
	}
	for _, record := range source {
		block, ok := record.Item.Data.(Block)
		if !ok || block.Content.Kind != InlineDatabase {
			continue
		}
		database, err := loadRecord(e.ctx, e.tx, block.Content.Database)
		if errors.Is(err, sql.ErrNoRows) {
			return invalidStructure()
		}
		if err != nil {
			return err
		}
		if !database.Alive || database.Item.Kind() != DatabaseKind {
			return invalidStructure()
		}
		id, err := NewID(DatabaseKind)
		if err != nil {
			return err
		}
		ids[block.Content.Database] = id
	}
	copies, err := remapCopies(source, ids, placements, e.seq, databaseRemapper{})
	if err != nil {
		return err
	}
	after := Target{Ref: &anchor}
	positions, err := e.places(anchor.ID.Kind(), page, parent, nil, &after, nil, len(roots))
	if err != nil {
		return err
	}
	rootIndex := 0
	for index, copy := range copies {
		if _, root := placements[source[index].Item.ID()]; root {
			position := positions[rootIndex]
			rootIndex++
			switch value := copy.Item.Data.(type) {
			case Page:
				value.Position = position
				copy.Item.Data = value
			case Block:
				value.Position = position
				copy.Item.Data = value
			}
		}
		if err := e.write(copy); err != nil {
			return err
		}
		mapping := copyMapping{Index: e.index, Source: source[index].Item.Ref, Ref: copy.Item.Ref, Ver: copy.Item.Ver}
		e.result.Copies = append(e.result.Copies, mapping)
		if _, root := placements[source[index].Item.ID()]; root {
			e.result.CopyRoots = append(e.result.CopyRoots, mapping)
		}
	}
	for _, record := range source {
		if block, ok := record.Item.Data.(Block); ok && block.Content.Kind == InlineDatabase {
			if err := e.copyDatabase(block.Content.Database, ids[record.Item.ID()], ids); err != nil {
				return err
			}
		}
	}
	// A duplicate is one inverse component, including every owned database.
	for id, change := range e.changes {
		change.Component = ""
		e.changes[id] = change
	}
	return e.validateStructure()
}
