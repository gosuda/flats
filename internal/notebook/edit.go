package notebook

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"reflect"
	"slices"
)

// The writer supplies a never-reused sequence; this executor never commits its transaction.
func applyEdit(ctx context.Context, tx *sql.Tx, opts editOptions, ops []Operation) (editResult, error) {
	result := editResult{Results: []operationResult{}, Refs: map[string]Ref{}}
	raw, err := encodeOperationList(ops, opts.Browser)
	if err != nil {
		return result, err
	}
	ops, err = decodeOperationList(raw, opts.Browser, nil)
	if err != nil {
		return result, err
	}
	if opts.Seq == 0 {
		return result, &editFailure{Status: "refused", Reason: "sequence"}
	}
	if opts.Browser && opts.Session == "" {
		return result, &editFailure{Status: "refused", Reason: "session"}
	}
	e := &edit{ctx: ctx, tx: tx, seq: opts.Seq, session: opts.Session, browser: opts.Browser, refs: map[string]Ref{}, result: result, deferAdmission: true}
	if err := e.reserveMarkdownNames(ops); err != nil {
		return result, err
	}
	if err := e.checkGuards(opts.Guards); err != nil {
		return result, err
	}
	before, err := measureEditUsage(ctx, tx)
	if err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notebook_action`); err != nil {
		return result, err
	}
	rollback := func(err error) (editResult, error) {
		_, rerr := tx.ExecContext(ctx, `ROLLBACK TO notebook_action`)
		_, relerr := tx.ExecContext(ctx, `RELEASE notebook_action`)
		return result, errors.Join(err, rerr, relerr)
	}
	for start := 0; start < len(ops); {
		end := start + 1
		meta := ops[start].operationMeta()
		if meta.UnitID != nil {
			for end < len(ops) && ops[end].operationMeta().UnitID != nil && *ops[end].operationMeta().UnitID == *meta.UnitID {
				end++
			}
		}
		if _, err := tx.ExecContext(ctx, `SAVEPOINT notebook_unit`); err != nil {
			return rollback(err)
		}
		savedRefs := maps.Clone(e.refs)
		copies, overwrites := len(e.result.Copies), len(e.result.Overwrote)
		results, roots := len(e.result.Results), len(e.result.CopyRoots)
		warnings, markdownCreates := len(e.result.Warnings), len(e.result.MarkdownCreates)
		e.changes = map[ID]itemChange{}
		e.dbLineage = map[ID]undoAggregate{}
		e.pageBefore = map[ID]Version{}
		e.oldPlaces = map[ID]Item{}
		savedEffects := maps.Clone(e.effects)
		failureIndex := start
		failure := e.checkStructure(meta.Structure)
		for i := start; i < end && failure == nil; i++ {
			failureIndex = i
			e.index = i
			if _, err := tx.ExecContext(ctx, `SAVEPOINT notebook_operation`); err != nil {
				return rollback(err)
			}
			if !databaseResolvesDependencies(ops[i]) {
				failure = walkOperationRefs(reflect.ValueOf(ops[i]), "", opts.Browser, func(t Target, k Kind) error {
					record, err := e.resolve(t, k)
					if err != nil {
						return err
					}
					return e.dbRememberPage(record)
				})
			}
			if failure == nil {
				failure = e.apply(ops[i])
			}
			if failure == nil {
				failure = e.validateStructure()
			}
			if failure == nil {
				failure = e.validateDatabases()
			}
			if failure != nil {
				if _, err := tx.ExecContext(ctx, `ROLLBACK TO notebook_operation`); err != nil {
					return rollback(errors.Join(failure, err))
				}
			}
			if _, err := tx.ExecContext(ctx, `RELEASE notebook_operation`); err != nil {
				return rollback(err)
			}
		}
		if failure != nil {
			var problem *editFailure
			if !errors.As(failure, &problem) {
				return rollback(failure)
			}
			if !opts.Browser {
				return rollback(failure)
			}
			if _, err := tx.ExecContext(ctx, `ROLLBACK TO notebook_unit`); err != nil {
				return rollback(err)
			}
			e.refs = savedRefs
			e.effects = savedEffects
			e.result.Results = e.result.Results[:results]
			e.result.CopyRoots = e.result.CopyRoots[:roots]
			e.result.Warnings = e.result.Warnings[:warnings]
			e.result.MarkdownCreates = e.result.MarkdownCreates[:markdownCreates]
			e.result.Copies = e.result.Copies[:copies]
			e.result.Overwrote = e.result.Overwrote[:overwrites]
			for i := start; i < end; i++ {
				r := operationResult{Index: i, UnitID: meta.UnitID, Status: problem.Status, Reason: problem.Reason}
				if meta.UnitID != nil {
					r.FailedIndex = &failureIndex
				}
				e.result.Results = append(e.result.Results, r)
			}
		} else {
			if err := e.refreshChanges(); err != nil {
				return rollback(err)
			}
			if meta.UnitID != nil {
				for id, ch := range e.changes {
					ch.Component = ""
					e.changes[id] = ch
				}
			}
			if err := recordUndo(e, opts); err != nil {
				return rollback(err)
			}
			adjusted := map[int]operationResult{}
			for _, notice := range e.result.Results[results:] {
				adjusted[notice.Index] = notice
			}
			e.result.Results = e.result.Results[:results]
			for i := start; i < end; i++ {
				outcome, ok := adjusted[i]
				if !ok {
					outcome = operationResult{Index: i, Status: "applied"}
				}
				outcome.UnitID = meta.UnitID
				e.result.Results = append(e.result.Results, outcome)
			}
		}
		if _, err := tx.ExecContext(ctx, `RELEASE notebook_unit`); err != nil {
			return rollback(err)
		}
		start = end
	}
	if err := checkEditUsage(ctx, tx, before, opts.Limits); err != nil {
		_, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO notebook_action`)
		_, releaseErr := tx.ExecContext(ctx, `RELEASE notebook_action`)
		if rollbackErr != nil || releaseErr != nil || !opts.Browser {
			return result, errors.Join(err, rollbackErr, releaseErr)
		}
		var failure *editFailure
		if !errors.As(err, &failure) {
			return result, err
		}
		result.Results = make([]operationResult, len(ops))
		for i, op := range ops {
			result.Results[i] = operationResult{Index: i, UnitID: op.operationMeta().UnitID, Status: "refused", Reason: failure.Reason}
		}
		return result, nil
	}
	if opts.Close && opts.Session != "" {
		if err := closeUndoAction(ctx, tx, opts.Session, opts.ActionID); err != nil {
			return rollback(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `RELEASE notebook_action`); err != nil {
		return result, err
	}
	e.result.Refs = maps.Clone(e.refs)
	e.result.Records = e.effects
	return e.result, nil
}

func (e *edit) refreshChanges() error {
	for id, ch := range e.changes {
		record, err := loadRecord(e.ctx, e.tx, id)
		if err != nil {
			return err
		}
		ch.After = record
		e.changes[id] = ch
	}
	return nil
}

func (e *edit) checkGuards(guards *Guards) error {
	if guards == nil {
		return nil
	}
	if guards.State != nil {
		var store, seq string
		if err := e.tx.QueryRowContext(e.ctx, `SELECT store,seq FROM meta WHERE singleton=1`).Scan(&store, &seq); err != nil {
			return err
		}
		if store != guards.State.Store || seq != sequenceString(guards.State.Seq) {
			return &editFailure{Status: "refused", Reason: "guard"}
		}
	}
	for _, guard := range guards.Items {
		record, err := e.resolve(Target{Ref: &guard.Ref}, "")
		if err != nil {
			var f *editFailure
			if errors.As(err, &f) {
				return &editFailure{Status: "refused", Reason: "guard"}
			}
			return err
		}
		if record.Item.Ver != guard.Ver {
			return &editFailure{Status: "refused", Reason: "guard"}
		}
	}
	return nil
}

func (e *edit) checkStructure(checks []StructureCheck) error {
	for _, check := range checks {
		block, err := e.resolve(check.Block, BlockKind)
		if err != nil {
			return err
		}
		page, err := e.resolve(check.Page, PageKind)
		if err != nil {
			return err
		}
		parent, err := e.parent(check.Parent, BlockKind)
		if err != nil {
			return err
		}
		b := block.Item.Data.(Block)
		if b.Page != page.Item.ID() || !sameParent(b.Parent, parent) {
			return invalidStructure()
		}
		if check.After != nil {
			after, err := e.resolve(*check.After, BlockKind)
			if err != nil {
				return err
			}
			siblings, err := e.siblings(BlockKind, b.Page, b.Parent, nil)
			if err != nil {
				return err
			}
			index := slices.IndexFunc(siblings, func(r itemRecord) bool { return r.Item.ID() == block.Item.ID() })
			if index < 1 || siblings[index-1].Item.ID() != after.Item.ID() {
				return invalidStructure()
			}
		}
	}
	return nil
}

func sameParent(a, b *ID) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func (e *edit) apply(op Operation) error {
	switch op := op.(type) {
	case *CreatePageOp:
		parent, err := e.parent(op.Parent, PageKind)
		if err != nil {
			return err
		}
		pos, err := e.place(PageKind, "", parent, op.Before, nil, nil)
		if err != nil {
			return err
		}
		_, err = e.newItem(PageKind, op.CreateFields, Page{Title: op.Title, Parent: parent, Position: pos})
		return err
	case *SetPageTitleOp:
		record, err := e.resolve(op.Page, PageKind)
		if err != nil {
			return err
		}
		page := record.Item.Data.(Page)
		page.Title = op.Title
		record.Item.Data = page
		return e.write(record, TitleGroup)
	case *MovePageOp:
		record, err := e.resolve(op.Page, PageKind)
		if err != nil {
			return err
		}
		parent, err := e.parent(op.Parent, PageKind)
		if err != nil {
			return err
		}
		pos, err := e.place(PageKind, "", parent, op.Before, nil, map[ID]bool{record.Item.ID(): true})
		if err != nil {
			return err
		}
		page := record.Item.Data.(Page)
		page.Parent, page.Position = parent, pos
		record.Item.Data = page
		return e.write(record, PlaceGroup)
	case *DeletePageOp:
		record, err := e.resolve(op.Page, PageKind)
		if err != nil {
			return err
		}
		return e.deleteSubtrees([]itemRecord{record})
	case *DuplicatePageOp:
		return e.duplicatePage(op)
	case *CreateBlockOp:
		page, parent, err := e.blockDestination(op.Place)
		if err != nil {
			return err
		}
		pos, err := e.place(BlockKind, page, parent, op.Place.Before, op.Place.After, nil)
		if err != nil {
			return err
		}
		_, err = e.newItem(BlockKind, op.CreateFields, Block{Page: page, Parent: parent, Position: pos, Content: op.Content})
		return err
	case *SetBlockOp:
		record, err := e.resolve(op.Block, BlockKind)
		if err != nil {
			return err
		}
		return e.setContent(record, op.Content, op.BaseV)
	case *TurnBlocksOp:
		defer func() { e.component = "" }()
		records := make([]itemRecord, 0, len(op.Blocks))
		for _, target := range op.Blocks {
			r, err := e.resolve(target, BlockKind)
			if err != nil {
				return err
			}
			if !canConvert(r.Item.Data.(Block).Content.Kind, op.Kind) {
				return &editFailure{Status: "refused", Reason: "kind"}
			}
			records = append(records, r)
		}
		for _, record := range records {
			// Earlier selected conversions can hoist this block, so reload its current place.
			record, err := loadRecord(e.ctx, e.tx, record.Item.ID())
			if err != nil {
				return err
			}
			content := turnContent(record.Item.Data.(Block).Content, op.Kind)
			e.component = record.Item.ID()
			if existing, ok := e.changes[record.Item.ID()]; ok && existing.Component != "" {
				e.component = existing.Component
			}
			if err := e.setContent(record, content, nil); err != nil {
				return err
			}
		}
		return nil
	case *MoveBlocksOp:
		return e.moveBlocks(op)
	case *DeleteBlocksOp:
		roots, err := e.selected(op.Blocks, BlockKind)
		if err != nil {
			return err
		}
		return e.deleteSubtrees(roots)
	case *DuplicateBlocksOp:
		return e.duplicateBlocks(op)
	case *MarkdownToBlocksOp:
		return e.markdownToBlocks(op)
	default:
		if handled, err := e.applyDatabase(op); handled {
			return err
		}
		return &editFailure{Status: "refused", Reason: "unsupported"}
	}
}

func (e *edit) blockDestination(place Placement) (ID, *ID, error) {
	page, err := e.resolve(place.Page, PageKind)
	if err != nil {
		return "", nil, err
	}
	parent, err := e.parent(place.Parent, BlockKind)
	if err != nil {
		return "", nil, err
	}
	if parent != nil {
		record, err := loadRecord(e.ctx, e.tx, *parent)
		if err != nil {
			return "", nil, err
		}
		b := record.Item.Data.(Block)
		if b.Page != page.Item.ID() || !b.Content.Kind.HoldsChildren() {
			return "", nil, invalidStructure()
		}
	}
	return page.Item.ID(), parent, nil
}

func (e *edit) moveBlocks(op *MoveBlocksOp) error {
	roots, err := e.selected(op.Blocks, BlockKind)
	if err != nil {
		return err
	}
	page, parent, err := e.blockDestination(op.Place)
	if err != nil {
		return err
	}
	excluded := map[ID]bool{}
	trees := make([][]itemRecord, len(roots))
	for i, root := range roots {
		tree, err := e.subtree(root)
		if err != nil {
			return err
		}
		trees[i] = tree
		for _, r := range tree {
			excluded[r.Item.ID()] = true
		}
	}
	if parent != nil && excluded[*parent] {
		return invalidStructure()
	}
	before, after := op.Place.Before, op.Place.After
	for i, root := range roots {
		pos, err := e.place(BlockKind, page, parent, before, after, excluded)
		if err != nil {
			return err
		}
		for _, record := range trees[i] {
			block := record.Item.Data.(Block)
			crossPage := block.Page != page
			block.Page = page
			if record.Item.ID() == root.Item.ID() {
				block.Parent, block.Position = parent, pos
			}
			record.Item.Data = block
			if crossPage || record.Item.ID() == root.Item.ID() {
				if err := e.write(record, PlaceGroup); err != nil {
					return err
				}
			}
		}
		delete(excluded, root.Item.ID())
		ref := root.Item.Ref
		after = &Target{Ref: &ref}
		before = nil
	}
	return nil
}

func canConvert(from, to ContentKind) bool {
	return from == to || (from.IsText() || from == Code) && (to.IsText() || to == Code)
}

func turnContent(current Content, kind ContentKind) Content {
	if current.Kind == kind {
		return current
	}
	result := Content{Kind: kind}
	if kind == Callout {
		result.Tone = NeutralTone
	}
	if current.Kind == Code {
		result.Text = RichText{{Text: current.Code, Marks: []Mark{}}}
	} else if kind == Code {
		for _, run := range current.Text {
			result.Code += run.Text
		}
	} else {
		result.Text = current.Text
	}
	return result
}

func (e *edit) setContent(record itemRecord, content Content, base *Version) error {
	block := record.Item.Data.(Block)
	if !canConvert(block.Content.Kind, content.Kind) {
		return invalidKind()
	}
	if content.Kind == InlineDatabase && content.Database != block.Content.Database {
		return invalidKind()
	}
	oldVersion, err := record.Item.GroupVersion(ContentGroup)
	if err != nil {
		return err
	}
	if base != nil && *base != oldVersion {
		e.result.Overwrote = append(e.result.Overwrote, overwrite{Block: record.Item.Ref, Version: oldVersion, Base: *base})
	}
	block.Content = content
	record.Item.Data = block
	if err := e.write(record, ContentGroup); err != nil {
		return err
	}
	if content.Kind.HoldsChildren() {
		return nil
	}
	children, err := e.siblings(BlockKind, block.Page, idPointer(record.Item.ID()), nil)
	if err != nil {
		return err
	}
	anchor := record.Item.Ref
	for _, child := range children {
		after := Target{Ref: &anchor}
		pos, err := e.place(BlockKind, block.Page, block.Parent, nil, &after, nil)
		if err != nil {
			return err
		}
		b := child.Item.Data.(Block)
		b.Parent, b.Position = block.Parent, pos
		child.Item.Data = b
		if err := e.write(child, PlaceGroup); err != nil {
			return err
		}
		anchor = child.Item.Ref
	}
	return nil
}

func (e *edit) deleteSubtrees(roots []itemRecord) error {
	previous := e.component
	defer func() { e.component = previous }()
	if e.component == "" && len(roots) > 0 {
		e.component = roots[0].Item.ID()
	}
	for _, root := range roots {
		tree, err := e.subtree(root)
		if err != nil {
			return err
		}
		for _, record := range tree {
			if block, ok := record.Item.Data.(Block); ok && block.Content.Kind == InlineDatabase {
				if err := e.deleteDatabaseOf(record.Item.ID()); err != nil {
					return err
				}
			}
			record.Alive, record.DeletedAt = false, e.seq
			if err := e.write(record); err != nil {
				return err
			}
		}
	}
	return nil
}
