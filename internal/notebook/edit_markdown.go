package notebook

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type markdownEditState struct {
	next     int
	reserved map[string]bool
}

type markdownCreation struct {
	Index int
	Name  string
}

func (e *edit) reserveMarkdownNames(ops []Operation) error {
	e.markdown.reserved = map[string]bool{}
	for _, op := range ops {
		if op.OperationName() == "markdown_to_blocks" {
			continue
		}
		creates, err := operationCreates(op)
		if err != nil {
			return err
		}
		for _, create := range creates {
			e.markdown.reserved[create.Name] = true
		}
	}
	return nil
}

func (e *edit) markdownToBlocks(op *MarkdownToBlocksOp) error {
	place, depth, err := e.markdownDestination(op.Place)
	if err != nil {
		return err
	}
	aliases := map[string]string{}
	for _, asset := range op.AssetRefs {
		if asset.Asset.Tmp != "" {
			return &editFailure{Status: "refused", Reason: "assets_unprepared", Detail: fmt.Sprintf("Markdown image path %q requires a prepared asset", asset.Path)}
		}
		aliases[asset.Path] = asset.Asset.Hash
	}
	var assetErr error
	resolve := func(path string) (string, bool) {
		hash, ok := aliases[path]
		if !ok {
			hash = strings.TrimPrefix(path, "assets/")
		}
		if (Content{Kind: Image, Asset: hash, Caption: RichText{}}).Validate() != nil {
			return "", false
		}
		var found string
		err := e.tx.QueryRowContext(e.ctx, `SELECT hash FROM assets WHERE hash=?`, hash).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return "", false
		}
		if err != nil {
			assetErr = err
			return "", false
		}
		return found, true
	}
	ops, warnings, err := parseMarkdownAtDepth(op.Markdown, place, resolve, depth)
	if assetErr != nil {
		return assetErr
	}
	if err != nil {
		return &editFailure{Status: "refused", Reason: "markdown", Detail: err.Error()}
	}
	ops, err = e.nameMarkdownOperations(ops, op.ProposedIDs)
	if err != nil {
		return err
	}
	creates := []markdownCreation{}
	for _, child := range ops {
		items, err := operationCreates(child)
		if err != nil {
			return err
		}
		for _, item := range items {
			creates = append(creates, markdownCreation{Index: e.index, Name: item.Name})
		}
		if handled, err := e.applyDatabase(child); handled {
			if err != nil {
				return err
			}
		} else if err := e.apply(child); err != nil {
			return err
		}
	}
	e.result.MarkdownCreates = append(e.result.MarkdownCreates, creates...)
	e.result.Warnings = append(e.result.Warnings, warnings...)
	return nil
}

func (e *edit) markdownDestination(place Placement) (Placement, int, error) {
	page, parent, err := e.blockDestination(place)
	if err != nil {
		return Placement{}, 0, err
	}
	pageRecord, err := loadRecord(e.ctx, e.tx, page)
	if err != nil {
		return Placement{}, 0, err
	}
	resolved := Placement{Page: Target{Ref: &pageRecord.Item.Ref}}
	depth := 0
	for ancestor := parent; ancestor != nil; {
		depth++
		if depth >= MaxBlockDepth {
			return Placement{}, 0, invalidStructure()
		}
		record, err := loadRecord(e.ctx, e.tx, *ancestor)
		if err != nil {
			return Placement{}, 0, err
		}
		block := record.Item.Data.(Block)
		if !record.Alive || block.Page != page || !block.Content.Kind.HoldsChildren() {
			return Placement{}, 0, invalidStructure()
		}
		if resolved.Parent == nil {
			resolved.Parent = &Target{Ref: &record.Item.Ref}
		}
		ancestor = block.Parent
	}
	if place.Before != nil && place.After != nil {
		return Placement{}, 0, invalidStructure()
	}
	for _, anchor := range []struct {
		input  *Target
		output **Target
	}{{place.Before, &resolved.Before}, {place.After, &resolved.After}} {
		if anchor.input == nil {
			continue
		}
		record, err := e.resolve(*anchor.input, BlockKind)
		if err != nil {
			return Placement{}, 0, err
		}
		block := record.Item.Data.(Block)
		if block.Page != page || !sameParent(block.Parent, parent) {
			return Placement{}, 0, invalidStructure()
		}
		*anchor.output = &Target{Ref: &record.Item.Ref}
	}
	return resolved, depth, nil
}

func (e *edit) nameMarkdownOperations(ops []Operation, proposed []ProposedItem) ([]Operation, error) {
	names := map[string]string{}
	next := e.markdown.next
	allocate := func(old string, database bool) {
		for {
			next++
			name := "$md" + strconv.Itoa(next)
			candidates := []string{name}
			if database {
				candidates = append(candidates, name+".title", name+".view")
			}
			available := true
			for _, candidate := range candidates {
				if _, exists := e.refs[candidate]; exists || e.markdown.reserved[candidate] {
					available = false
				}
			}
			if !available {
				continue
			}
			names[old] = name
			if database {
				names[old+".title"], names[old+".view"] = name+".title", name+".view"
			}
			return
		}
	}
	for _, op := range ops {
		switch op := op.(type) {
		case CreateBlockOp:
			allocate(op.Ref, false)
		case CreateDatabaseOp:
			allocate(op.Ref, true)
			allocate(op.BlockRef, false)
		case CreatePropertyOp:
			allocate(op.Ref, false)
		case CreateRowOp:
			allocate(op.Ref, false)
		default:
			return nil, fmt.Errorf("unexpected Markdown operation %T", op)
		}
	}
	ids := map[string]ID{}
	seenIDs := map[ID]bool{}
	for _, item := range proposed {
		if _, exists := ids[item.Ref]; exists || seenIDs[item.ProposedID] {
			return nil, &editFailure{Status: "refused", Reason: "proposed_ids"}
		}
		ids[item.Ref] = item.ProposedID
		seenIDs[item.ProposedID] = true
	}
	if e.browser && (proposed == nil || len(ids) != len(names)) || !e.browser && proposed != nil {
		return nil, &editFailure{Status: "refused", Reason: "proposed_ids"}
	}
	fields := func(old string) CreateFields {
		name := names[old]
		result := CreateFields{Ref: name}
		if id, ok := ids[name]; ok {
			result.ProposedID = &id
		}
		return result
	}
	target := func(t Target) Target {
		if name, ok := names[t.Tmp]; ok {
			return Target{Tmp: name}
		}
		return t
	}
	placement := func(p Placement) Placement {
		p.Page = target(p.Page)
		for _, slot := range []**Target{&p.Parent, &p.Before, &p.After} {
			if *slot != nil {
				t := target(**slot)
				*slot = &t
			}
		}
		return p
	}
	planned := make([]Operation, 0, len(ops))
	for _, op := range ops {
		var child Operation
		switch op := op.(type) {
		case CreateBlockOp:
			op.CreateFields, op.Place = fields(op.Ref), placement(op.Place)
			child = &op
		case CreateDatabaseOp:
			old := op.Ref
			op.CreateFields, op.Place = fields(old), placement(op.Place)
			op.BlockRef, op.BlockProposedID = names[op.BlockRef], fields(op.BlockRef).ProposedID
			op.TitlePropertyProposedID = fields(old + ".title").ProposedID
			op.ViewProposedID = fields(old + ".view").ProposedID
			child = &op
		case CreatePropertyOp:
			op.CreateFields, op.Database = fields(op.Ref), target(op.Database)
			child = &op
		case CreateRowOp:
			op.CreateFields, op.Database = fields(op.Ref), target(op.Database)
			op.Cells = append([]CellInput{}, op.Cells...)
			for i := range op.Cells {
				op.Cells[i].Property = target(op.Cells[i].Property)
			}
			child = &op
		}
		if err := validateOperation(child, e.browser); err != nil {
			return nil, &editFailure{Status: "refused", Reason: "proposed_ids", Detail: err.Error()}
		}
		planned = append(planned, child)
	}
	e.markdown.next = next
	return planned, nil
}
