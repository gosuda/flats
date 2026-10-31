package notebook

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

func NewID(kind Kind) (ID, error) {
	prefix, ok := kindPrefixes[kind]
	if !ok {
		return "", fmt.Errorf("unknown item kind %q", kind)
	}
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return ID(prefix + idEncoding.EncodeToString(data[:])), nil
}

func (e *edit) resolve(target Target, kind Kind) (itemRecord, error) {
	var ref Ref
	if target.Ref != nil {
		ref = *target.Ref
	} else {
		var ok bool
		ref, ok = e.refs[target.Tmp]
		if !ok {
			return itemRecord{}, gone()
		}
	}
	if kind != "" && ref.ID.Kind() != kind {
		return itemRecord{}, invalidStructure()
	}
	if ref.Inc == 0 {
		if !e.browser || e.session == "" {
			return itemRecord{}, gone()
		}
		var inc string
		err := e.tx.QueryRowContext(e.ctx, `SELECT inc FROM created WHERE session=? AND id=?`, e.session, ref.ID).Scan(&inc)
		if errors.Is(err, sql.ErrNoRows) {
			return itemRecord{}, gone()
		}
		if err != nil {
			return itemRecord{}, err
		}
		n, err := parseDBDecimal(inc)
		if err != nil {
			return itemRecord{}, err
		}
		ref.Inc = Incarnation(n)
	}
	record, err := loadRecord(e.ctx, e.tx, ref.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return itemRecord{}, gone()
	}
	if err != nil {
		return itemRecord{}, err
	}
	if !record.Alive || record.Item.Ref != ref {
		return itemRecord{}, gone()
	}
	return record, nil
}

func (e *edit) parent(target *Target, kind Kind) (*ID, error) {
	if target == nil {
		return nil, nil
	}
	record, err := e.resolve(*target, kind)
	if err != nil {
		return nil, err
	}
	id := record.Item.ID()
	return &id, nil
}

func (e *edit) newItem(kind Kind, fields CreateFields, data ItemData) (itemRecord, error) {
	var id ID
	if fields.ProposedID != nil {
		id = *fields.ProposedID
	} else {
		var err error
		id, err = NewID(kind)
		if err != nil {
			return itemRecord{}, err
		}
	}
	var count int
	if err := e.tx.QueryRowContext(e.ctx, `SELECT count(*) FROM allocated_ids WHERE id=?`, id).Scan(&count); err != nil {
		return itemRecord{}, err
	}
	if count != 0 {
		return itemRecord{}, &editFailure{Status: "refused", Reason: "allocated_id"}
	}
	record := itemRecord{Item: Item{Ref: Ref{ID: id, Inc: Incarnation(e.seq)}, Ver: Version(e.seq), V: map[GroupKey]Version{}, Data: data}, Alive: true, Aggregate: Version(e.seq)}
	if err := e.write(record); err != nil {
		return itemRecord{}, err
	}
	e.refs[fields.Ref] = record.Item.Ref
	if e.browser {
		if _, err := e.tx.ExecContext(e.ctx, `INSERT INTO created(session,id,inc) VALUES(?,?,?)`, e.session, id, strconv.FormatUint(uint64(e.seq), 10)); err != nil {
			return itemRecord{}, err
		}
	}
	return record, nil
}

func (e *edit) write(record itemRecord, groups ...GroupKey) error {
	previous, err := loadRecord(e.ctx, e.tx, record.Item.ID())
	var before *itemRecord
	if err == nil {
		before = &previous
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	record.Item.V = maps.Clone(record.Item.V)
	for _, group := range groups {
		record.Item.V[group] = Version(e.seq)
	}
	record.Item.Ver = max(Version(record.Item.Ref.Inc), record.Aggregate)
	for _, v := range record.Item.V {
		record.Item.Ver = max(record.Item.Ver, v)
	}
	if err := putRecord(e.ctx, e.tx, record); err != nil {
		return err
	}
	if e.changes == nil {
		e.changes = map[ID]itemChange{}
	}
	change, ok := e.changes[record.Item.ID()]
	if !ok {
		if before != nil {
			if original, found := e.oldPlaces[record.Item.ID()]; found {
				_, _, position := undoPlace(original)
				switch data := before.Item.Data.(type) {
				case Page:
					data.Position = position
					before.Item.Data = data
				case Block:
					data.Position = position
					before.Item.Data = data
				}
			}
		}
		change.Before = before
	}
	change.After = record
	if change.Component != "" && e.component != "" && change.Component != e.component {
		// A shared write joins the complete inverse components, not just this item.
		for id, related := range e.changes {
			if related.Component == change.Component {
				related.Component = e.component
				e.changes[id] = related
			}
		}
		change.Component = e.component
	} else if change.Component == "" {
		change.Component = e.component
	}
	change.Groups = append(change.Groups, groups...)
	slices.Sort(change.Groups)
	change.Groups = slices.Compact(change.Groups)
	e.changes[record.Item.ID()] = change
	e.noteEffect(record)
	pages := map[ID]bool{}
	if before != nil {
		if b, ok := before.Item.Data.(Block); ok {
			pages[b.Page] = true
		}
	}
	if b, ok := record.Item.Data.(Block); ok {
		pages[b.Page] = true
	}
	for page := range pages {
		if err := e.bumpPage(page); err != nil {
			return err
		}
	}
	return nil
}

func (e *edit) bumpPage(id ID) error {
	record, err := loadRecord(e.ctx, e.tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	record.Aggregate = max(record.Aggregate, Version(e.seq))
	record.Item.Ver = max(record.Item.Ver, record.Aggregate)
	if err := putRecord(e.ctx, e.tx, record); err != nil {
		return err
	}
	e.noteEffect(record)
	return nil
}

func (e *edit) noteEffect(record itemRecord) {
	if e.effects == nil {
		e.effects = map[ID]itemRecord{}
	}
	e.effects[record.Item.ID()] = record
}

func (e *edit) records(query string, args ...any) ([]itemRecord, error) {
	rows, err := e.tx.QueryContext(e.ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var ids []ID
	for rows.Next() {
		var id ID
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	records := make([]itemRecord, 0, len(ids))
	for _, id := range ids {
		r, err := loadRecord(e.ctx, e.tx, id)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, nil
}

func (e *edit) siblings(kind Kind, page ID, parent *ID, exclude map[ID]bool) ([]itemRecord, error) {
	var list []itemRecord
	var err error
	if kind == PageKind {
		list, err = e.records(`SELECT p.id FROM pages p JOIN items i ON p.id=i.id WHERE i.alive=1 AND p.parent IS ? ORDER BY p.position,p.id`, parent)
	} else {
		list, err = e.records(`SELECT b.id FROM blocks b JOIN items i ON b.id=i.id WHERE i.alive=1 AND b.page=? AND b.parent IS ? ORDER BY b.position,b.id`, page, parent)
	}
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(list, func(r itemRecord) bool { return exclude[r.Item.ID()] }), nil
}

func (e *edit) siblingPlaces(kind Kind, page ID, parent *ID, exclude map[ID]bool) ([]itemRecord, error) {
	query := `SELECT p.id,i.inc,p.position FROM pages p JOIN items i ON i.id=p.id WHERE i.alive=1 AND p.parent IS ? ORDER BY p.position,p.id`
	args := []any{parent}
	if kind == BlockKind {
		query = `SELECT b.id,i.inc,b.position FROM blocks b JOIN items i ON i.id=b.id WHERE i.alive=1 AND b.page=? AND b.parent IS ? ORDER BY b.position,b.id`
		args = []any{page, parent}
	}
	rows, err := e.tx.QueryContext(e.ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []itemRecord
	for rows.Next() {
		var id ID
		var inc, position string
		if err := rows.Scan(&id, &inc, &position); err != nil {
			return nil, err
		}
		if exclude[id] {
			continue
		}
		n, err := parseDBDecimal(inc)
		if err != nil {
			return nil, err
		}
		var data ItemData = Page{Parent: parent, Position: position}
		if kind == BlockKind {
			data = Block{Page: page, Parent: parent, Position: position}
		}
		result = append(result, itemRecord{Item: Item{Ref: Ref{ID: id, Inc: Incarnation(n)}, Data: data}, Alive: true})
	}
	return result, rows.Err()
}

func (e *edit) place(kind Kind, page ID, parent *ID, before, after *Target, exclude map[ID]bool) (string, error) {
	keys, err := e.places(kind, page, parent, before, after, exclude, 1)
	if err != nil {
		return "", err
	}
	return keys[0], nil
}

func (e *edit) places(kind Kind, page ID, parent *ID, before, after *Target, exclude map[ID]bool, count int) ([]string, error) {
	if count == 0 {
		return nil, nil
	}
	siblings, err := e.siblingPlaces(kind, page, parent, exclude)
	if err != nil {
		return nil, err
	}
	anchor := before
	if after != nil {
		anchor = after
	}
	index := len(siblings)
	if anchor != nil {
		r, err := e.resolve(*anchor, kind)
		if err != nil {
			return nil, err
		}
		index = -1
		for i, s := range siblings {
			if s.Item.ID() == r.Item.ID() {
				index = i
				if after != nil {
					index++
				}
				break
			}
		}
		if index < 0 {
			return nil, invalidStructure()
		}
	}
	bounds := func() (string, string) {
		left, right := "", ""
		if index > 0 {
			_, _, left = undoPlace(siblings[index-1].Item)
		}
		if index < len(siblings) {
			_, _, right = undoPlace(siblings[index].Item)
		}
		return left, right
	}
	left, right := bounds()
	positions, err := positionsBetween(left, right, count)
	if err == nil {
		return positions, nil
	}
	keys := rebalancePositions(len(siblings))
	for i, sibling := range siblings {
		record, err := loadRecord(e.ctx, e.tx, sibling.Item.ID())
		if err != nil {
			return nil, err
		}
		if e.oldPlaces == nil {
			e.oldPlaces = map[ID]Item{}
		}
		if _, written := e.changes[record.Item.ID()]; !written {
			if _, ok := e.oldPlaces[record.Item.ID()]; !ok {
				e.oldPlaces[record.Item.ID()] = record.Item
			}
		}
		switch data := record.Item.Data.(type) {
		case Page:
			data.Position = keys[i]
			record.Item.Data = data
		case Block:
			data.Position = keys[i]
			record.Item.Data = data
		}
		// A physical rekey is not an assignment to the logical place group.
		if err := putRecord(e.ctx, e.tx, record); err != nil {
			return nil, err
		}
		e.noteEffect(record)
		siblings[i] = record
	}
	left, right = bounds()
	return positionsBetween(left, right, count)
}

func (e *edit) subtree(root itemRecord) ([]itemRecord, error) {
	var result []itemRecord
	if root.Item.Kind() == BlockKind {
		var visit func(itemRecord) error
		visit = func(r itemRecord) error {
			result = append(result, r)
			b := r.Item.Data.(Block)
			children, err := e.siblings(BlockKind, b.Page, idPointer(r.Item.ID()), nil)
			if err != nil {
				return err
			}
			for _, child := range children {
				if err := visit(child); err != nil {
					return err
				}
			}
			return nil
		}
		if err := visit(root); err != nil {
			return nil, err
		}
		return result, nil
	}
	var pages func(itemRecord) error
	pages = func(r itemRecord) error {
		result = append(result, r)
		blocks, err := e.siblings(BlockKind, r.Item.ID(), nil, nil)
		if err != nil {
			return err
		}
		for _, block := range blocks {
			tree, err := e.subtree(block)
			if err != nil {
				return err
			}
			result = append(result, tree...)
		}
		children, err := e.siblings(PageKind, "", idPointer(r.Item.ID()), nil)
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := pages(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := pages(root); err != nil {
		return nil, err
	}
	return result, nil
}

func idPointer(id ID) *ID { return &id }

func (e *edit) selected(targets []Target, kind Kind) ([]itemRecord, error) {
	byID := map[ID]itemRecord{}
	for _, target := range targets {
		r, err := e.resolve(target, kind)
		if err != nil {
			return nil, err
		}
		byID[r.Item.ID()] = r
	}
	paths := map[ID][]placeStep{}
	var roots []itemRecord
	for id, r := range byID {
		path, nested, err := e.documentPath(r, byID)
		if err != nil {
			return nil, err
		}
		if nested {
			continue
		}
		paths[id] = path
		roots = append(roots, r)
	}
	slices.SortFunc(roots, func(a, b itemRecord) int {
		return slices.CompareFunc(paths[a.Item.ID()], paths[b.Item.ID()], func(a, b placeStep) int {
			// A page's own blocks precede content on its subpages; their position keys are unrelated.
			if a.ID.Kind() != b.ID.Kind() {
				if a.ID.Kind() == BlockKind {
					return -1
				}
				return 1
			}
			if cmp := strings.Compare(a.Position, b.Position); cmp != 0 {
				return cmp
			}
			return strings.Compare(string(a.ID), string(b.ID))
		})
	})
	return roots, nil
}

type placeStep struct {
	Position string
	ID       ID
}

func (e *edit) documentPath(record itemRecord, selected map[ID]itemRecord) ([]placeStep, bool, error) {
	var path []placeStep
	current := record
	for {
		_, parent, position := undoPlace(current.Item)
		path = append(path, placeStep{Position: position, ID: current.Item.ID()})
		if parent == nil {
			if current.Item.Kind() == BlockKind {
				page := current.Item.Data.(Block).Page
				var err error
				current, err = loadRecord(e.ctx, e.tx, page)
				if err != nil {
					return nil, false, err
				}
				continue
			}
			break
		}
		if _, nested := selected[*parent]; nested {
			return nil, true, nil
		}
		var err error
		current, err = loadRecord(e.ctx, e.tx, *parent)
		if err != nil {
			return nil, false, err
		}
	}
	slices.Reverse(path)
	return path, false, nil
}

type treeNode struct {
	parent *ID
	page   ID
	holds  bool
}

func (e *edit) validateStructure() error {
	nodes := map[ID]treeNode{}
	rows, err := e.tx.QueryContext(e.ctx, `SELECT p.id,p.parent FROM pages p JOIN items i ON i.id=p.id WHERE i.alive=1`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id ID
		var parent *ID
		if err := rows.Scan(&id, &parent); err != nil {
			return errors.Join(err, rows.Close())
		}
		nodes[id] = treeNode{parent: parent}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(nodes) == 0 {
		return &editFailure{Status: "refused", Reason: "last_page"}
	}
	for id, node := range nodes {
		seen := map[ID]bool{id: true}
		depth := 1
		for node.parent != nil {
			p := *node.parent
			if seen[p] {
				return invalidStructure()
			}
			seen[p] = true
			var ok bool
			node, ok = nodes[p]
			if !ok {
				return invalidStructure()
			}
			depth++
			if depth > MaxPageDepth {
				return invalidStructure()
			}
		}
	}
	blocks := map[ID]treeNode{}
	rows, err = e.tx.QueryContext(e.ctx, `SELECT b.id,b.page,b.parent,json_extract(i.data,'$.content.kind') FROM blocks b JOIN items i ON i.id=b.id WHERE i.alive=1`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, page ID
		var parent *ID
		var kind ContentKind
		if err := rows.Scan(&id, &page, &parent, &kind); err != nil {
			return errors.Join(err, rows.Close())
		}
		blocks[id] = treeNode{parent: parent, page: page, holds: kind.HoldsChildren()}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for id, node := range blocks {
		if _, ok := nodes[node.page]; !ok {
			return invalidStructure()
		}
		page := node.page
		seen := map[ID]bool{id: true}
		depth := 1
		for node.parent != nil {
			p := *node.parent
			if seen[p] {
				return invalidStructure()
			}
			seen[p] = true
			var ok bool
			node, ok = blocks[p]
			if !ok || !node.holds || node.page != page {
				return invalidStructure()
			}
			depth++
			if depth > MaxBlockDepth {
				return invalidStructure()
			}
		}
	}
	return nil
}
