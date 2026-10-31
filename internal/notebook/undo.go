package notebook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"modernc.org/sqlite"
	"modernc.org/sqlite/lib"
)

type undoSummary struct {
	Applied    int
	Skipped    int
	LaterEdits int
	Missing    int
	Structure  int
	Capacity   int
	Empty      bool
	Evicted    bool
	Notices    []string
	Changes    map[ID]itemChange
	Records    map[ID]itemRecord
}

type undoNeighbors struct {
	Before *Ref `json:"before,omitempty"`
	After  *Ref `json:"after,omitempty"`
}

type undoPart struct {
	Desired       *itemRecord          `json:"desired,omitempty"`
	Written       itemRecord           `json:"written"`
	Groups        []GroupKey           `json:"groups"`
	Neighbors     undoNeighbors        `json:"neighbors"`
	GuardInc      Incarnation          `json:"guard_inc,omitempty"`
	GuardVersion  Version              `json:"guard_version,omitempty"`
	GuardVersions map[GroupKey]Version `json:"guard_versions,omitempty"`
}

func (p undoPart) incarnation() Incarnation {
	if p.GuardInc != 0 {
		return p.GuardInc
	}
	return p.Written.Item.Ref.Inc
}

func (p undoPart) version(group GroupKey) (Version, error) {
	if version, ok := p.GuardVersions[group]; ok {
		return version, nil
	}
	return p.Written.Item.GroupVersion(group)
}

type undoComponent struct {
	Parts []undoPart `json:"parts"`
}

// Aggregate lineage reconnects destructive guards without restoring ancestor data.
type undoAggregate struct {
	Ref        Ref     `json:"ref"`
	Before     Version `json:"before"`
	Written    Version `json:"written"`
	Components []int   `json:"components,omitempty"`
}

type undoEntry struct {
	ActionID   string          `json:"action_id,omitempty"`
	Typing     *Ref            `json:"typing,omitempty"`
	Open       bool            `json:"open"`
	At         int64           `json:"at"`
	Components []undoComponent `json:"components"`
	Aggregates []undoAggregate `json:"aggregates,omitempty"`
}

type undoState struct {
	Evicted bool `json:"evicted"`
}

// Closed identities are session bookkeeping, not retained inverse payloads.
// Separate indexed rows keep eviction from forgetting ids or rewriting a growing set.
func undoActionClosed(ctx context.Context, tx *sql.Tx, session, id string, close bool) (bool, error) {
	if id == "" {
		return true, nil
	}
	key := "closed:" + id
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM undo WHERE session=? AND stack=? AND entry_order=0`, session, key).Scan(&exists); err != nil {
		return false, err
	}
	if close {
		if _, err := tx.ExecContext(ctx, `INSERT INTO undo(session,stack,entry_order,components,bytes) VALUES(?,?,0,'{}',0) ON CONFLICT(session,stack,entry_order) DO NOTHING`, session, key); err != nil {
			return false, err
		}
	}
	return exists != 0, nil
}

func loadUndoState(ctx context.Context, tx *sql.Tx, session string) (undoState, error) {
	var state undoState
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT components FROM undo WHERE session=? AND stack='state' AND entry_order=0`, session).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return state, err
	}
	return state, nil
}

func saveUndoState(ctx context.Context, tx *sql.Tx, session string, state undoState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO undo(session,stack,entry_order,components,bytes) VALUES(?,'state',0,?,?) ON CONFLICT(session,stack,entry_order) DO UPDATE SET components=excluded.components,bytes=excluded.bytes`, session, string(raw), len(raw))
	return err
}

func saveUndoEntry(ctx context.Context, tx *sql.Tx, session, stack string, order int64, entry undoEntry) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO undo(session,stack,entry_order,components,bytes) VALUES(?,?,?,?,?) ON CONFLICT(session,stack,entry_order) DO UPDATE SET components=excluded.components,bytes=excluded.bytes`, session, stack, order, string(raw), len(raw))
	return err
}

type undoRow struct {
	Session string
	Stack   string
	Order   int64
	Entry   undoEntry
}

func readUndoRows(ctx context.Context, tx *sql.Tx, session string) ([]undoRow, error) {
	return queryUndoRows(ctx, tx, `SELECT session,stack,entry_order,components FROM undo WHERE stack IN ('undo','redo') AND (?='' OR session=?) ORDER BY rowid`, session, session)
}

func queryUndoRows(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]undoRow, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []undoRow
	for rows.Next() {
		var row undoRow
		var raw string
		if err := rows.Scan(&row.Session, &row.Stack, &row.Order, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &row.Entry); err != nil {
			return nil, fmt.Errorf("decode undo entry: %w", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func closeUndoAction(ctx context.Context, tx *sql.Tx, session, actionID string) error {
	rows, err := queryUndoRows(ctx, tx, `SELECT session,stack,entry_order,components FROM undo WHERE session=? AND stack IN ('undo','redo') AND json_extract(components,'$.open')=1 AND (?='' OR json_extract(components,'$.action_id')=?) ORDER BY rowid`, session, actionID, actionID)
	if err != nil {
		return err
	}
	state, err := loadUndoState(ctx, tx, session)
	if err != nil {
		return err
	}
	for _, row := range rows {
		row.Entry.Open = false
		if _, err := undoActionClosed(ctx, tx, session, row.Entry.ActionID, true); err != nil {
			return err
		}
		if err := saveUndoEntry(ctx, tx, session, row.Stack, row.Order, row.Entry); err != nil {
			return err
		}
	}
	if actionID != "" {
		if _, err := undoActionClosed(ctx, tx, session, actionID, true); err != nil {
			return err
		}
	}
	return saveUndoState(ctx, tx, session, state)
}

func recordUndo(e *edit, opts editOptions) error {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	typing := undoTypingChange(e.changes, opts.TypingBlock)
	rows, err := queryUndoRows(e.ctx, e.tx, `SELECT session,stack,entry_order,components FROM undo WHERE stack IN ('undo','redo') AND json_extract(components,'$.open')=1 ORDER BY rowid`)
	if err != nil {
		return err
	}
	for _, row := range rows {
		close := now.UnixNano()-row.Entry.At >= int64(time.Second)
		if row.Session == opts.Session && (!typing || row.Entry.ActionID != opts.ActionID || row.Entry.Typing == nil || *row.Entry.Typing != *opts.TypingBlock) {
			close = true
		}
		if row.Entry.Typing != nil {
			if ch, ok := e.changes[row.Entry.Typing.ID]; ok && (slices.Contains(ch.Groups, ContentGroup) || !ch.After.Alive || ch.After.Item.Ref != *row.Entry.Typing) {
				if row.Session != opts.Session || opts.TypingBlock == nil || row.Entry.ActionID != opts.ActionID {
					close = true
				}
			}
		}
		if close {
			if err := closeUndoAction(e.ctx, e.tx, row.Session, row.Entry.ActionID); err != nil {
				return err
			}
		}
	}
	if opts.Session == "" || len(e.changes) == 0 {
		return nil
	}
	if _, err := e.tx.ExecContext(e.ctx, `DELETE FROM undo WHERE session=? AND stack='redo'`, opts.Session); err != nil {
		return err
	}
	entry, err := makeUndoEntry(e, e.changes)
	if err != nil {
		return err
	}
	entry.ActionID, entry.At = opts.ActionID, now.UnixNano()
	state, err := loadUndoState(e.ctx, e.tx, opts.Session)
	if err != nil {
		return err
	}
	closed, err := undoActionClosed(e.ctx, e.tx, opts.Session, opts.ActionID, false)
	if err != nil {
		return err
	}
	if typing && !closed {
		entry.Typing = opts.TypingBlock
		entry.Open = !opts.Close
	}
	var order int64
	if err := e.tx.QueryRowContext(e.ctx, `SELECT coalesce(max(entry_order),0)+1 FROM undo WHERE session=? AND stack IN ('undo','redo')`, opts.Session).Scan(&order); err != nil {
		return err
	}
	if typing {
		current, err := queryUndoRows(e.ctx, e.tx, `SELECT session,stack,entry_order,components FROM undo WHERE session=? AND stack IN ('undo','redo') ORDER BY rowid DESC LIMIT 1`, opts.Session)
		if err != nil {
			return err
		}
		if len(current) > 0 {
			last := current[0]
			if last.Stack == "undo" && last.Entry.Open && last.Entry.ActionID == opts.ActionID && last.Entry.Typing != nil && *last.Entry.Typing == *opts.TypingBlock && now.UnixNano()-last.Entry.At < int64(time.Second) {
				old := &last.Entry.Components[0].Parts[0]
				ch := e.changes[opts.TypingBlock.ID]
				beforeVersion, err := ch.Before.Item.GroupVersion(ContentGroup)
				if err != nil {
					return err
				}
				writtenVersion, err := old.Written.Item.GroupVersion(ContentGroup)
				if err != nil {
					return err
				}
				if ch.Before.Item.Ref == old.Written.Item.Ref && beforeVersion == writtenVersion {
					old.Written = ch.After
					last.Entry.At, last.Entry.Open = entry.At, entry.Open
					entry, order = last.Entry, last.Order
				}
			}
		}
	}
	if !entry.Open {
		if _, err := undoActionClosed(e.ctx, e.tx, opts.Session, opts.ActionID, true); err != nil {
			return err
		}
	}
	if err := saveUndoState(e.ctx, e.tx, opts.Session, state); err != nil {
		return err
	}
	if err := saveUndoEntry(e.ctx, e.tx, opts.Session, "undo", order, entry); err != nil {
		return err
	}
	return pruneUndo(e.ctx, e.tx, opts.UndoBudget)
}

func undoTypingChange(changes map[ID]itemChange, ref *Ref) bool {
	if ref == nil || len(changes) != 1 {
		return false
	}
	ch, ok := changes[ref.ID]
	if !ok || ch.Before == nil || !ch.Before.Alive || !ch.After.Alive || ch.Before.Item.Ref != *ref || ch.After.Item.Ref != *ref || len(ch.Groups) != 1 || ch.Groups[0] != ContentGroup {
		return false
	}
	before, ok := ch.Before.Item.Data.(Block)
	after, afterOK := ch.After.Item.Data.(Block)
	return ok && afterOK && before.Content.Kind == after.Content.Kind
}

type undoList struct {
	Kind   Kind
	Page   ID
	Parent ID
}

func makeUndoEntry(e *edit, changes map[ID]itemChange) (undoEntry, error) {
	entry, err := makeUndoComponents(e, changes, map[undoList]map[ID]undoNeighbors{})
	if err != nil {
		return entry, err
	}
	err = e.dbAttachLineage(&entry)
	return entry, err
}

func hasDatabaseChanges(changes map[ID]itemChange) bool {
	for id := range changes {
		if databaseUndoKind(id.Kind()) {
			return true
		}
	}
	return false
}

func databaseUndoKind(kind Kind) bool {
	switch kind {
	case DatabaseKind, PropertyKind, OptionKind, RowKind, ViewKind:
		return true
	default:
		return false
	}
}

func hasDatabaseUndo(component undoComponent) bool {
	for _, part := range component.Parts {
		if databaseUndoKind(part.Written.Item.Kind()) {
			return true
		}
	}
	return false
}

func makeUndoComponents(e *edit, changes map[ID]itemChange, neighbors map[undoList]map[ID]undoNeighbors) (undoEntry, error) {
	entry := undoEntry{}
	components := map[ID]map[ID]itemChange{}
	separated := false
	for id, ch := range changes {
		key := ch.Component
		separated = separated || key != ""
		if components[key] == nil {
			components[key] = map[ID]itemChange{}
		}
		ch.Component = ""
		components[key][id] = ch
	}
	if separated {
		for _, key := range slices.Sorted(maps.Keys(components)) {
			var part undoEntry
			var err error
			if hasDatabaseChanges(components[key]) {
				part, err = makeDatabaseUndoComponents(e, components[key], neighbors, key != "")
			} else {
				part, err = makeUndoComponents(e, components[key], neighbors)
			}
			if err != nil {
				return entry, err
			}
			entry.Components = append(entry.Components, part.Components...)
		}
		return entry, nil
	}
	ids := slices.Sorted(maps.Keys(changes))
	if hasDatabaseChanges(changes) {
		return makeDatabaseUndoComponents(e, changes, neighbors, false)
	}
	structural := len(ids) > 1
	for _, id := range ids {
		ch := changes[id]
		if ch.Before == nil || !ch.Before.Alive || !ch.After.Alive || slices.Contains(ch.Groups, PlaceGroup) {
			structural = true
		}
		if id.Kind() != PageKind && id.Kind() != BlockKind {
			return entry, fmt.Errorf("undo does not yet support %s", id.Kind())
		}
	}
	var component undoComponent
	for _, id := range ids {
		ch := changes[id]
		part := undoPart{Desired: ch.Before, Written: ch.After, Groups: slices.Clone(ch.Groups)}
		if ch.Before != nil && ch.Before.Alive && (structural || slices.Contains(ch.Groups, PlaceGroup)) {
			page, parent, _ := undoPlace(ch.Before.Item)
			key := undoList{Kind: ch.Before.Item.Kind(), Page: page}
			if parent != nil {
				key.Parent = *parent
			}
			if neighbors[key] == nil {
				list, err := undoOldNeighbors(e, ch.Before.Item, e.changes)
				if err != nil {
					return entry, err
				}
				neighbors[key] = list
			}
			part.Neighbors = neighbors[key][id]
		}
		if structural {
			component.Parts = append(component.Parts, part)
		} else {
			for _, group := range part.Groups {
				p := part
				p.Groups = []GroupKey{group}
				entry.Components = append(entry.Components, undoComponent{Parts: []undoPart{p}})
			}
		}
	}
	if structural {
		entry.Components = append(entry.Components, component)
	}
	return entry, nil
}

func undoPlace(item Item) (ID, *ID, string) {
	switch data := item.Data.(type) {
	case Page:
		return "", data.Parent, data.Position
	case Block:
		return data.Page, data.Parent, data.Position
	default:
		return "", nil, ""
	}
}

func undoSameList(item Item, kind Kind, page ID, parent *ID) bool {
	p, par, _ := undoPlace(item)
	return item.Kind() == kind && p == page && ((par == nil && parent == nil) || (par != nil && parent != nil && *par == *parent))
}

func undoOldNeighbors(e *edit, item Item, changes map[ID]itemChange) (map[ID]undoNeighbors, error) {
	page, parent, _ := undoPlace(item)
	siblings, err := e.siblingPlaces(item.Kind(), page, parent, nil)
	if err != nil {
		return nil, err
	}
	byID := map[ID]Item{}
	for _, sibling := range siblings {
		byID[sibling.Item.ID()] = sibling.Item
	}
	for id, original := range e.oldPlaces {
		delete(byID, id)
		if undoSameList(original, item.Kind(), page, parent) {
			byID[id] = original
		}
	}
	for id, ch := range changes {
		delete(byID, id)
		if ch.Before != nil && ch.Before.Alive && undoSameList(ch.Before.Item, item.Kind(), page, parent) {
			byID[id] = ch.Before.Item
		}
	}
	list := slices.Collect(maps.Values(byID))
	slices.SortFunc(list, func(a, b Item) int {
		_, _, pa := undoPlace(a)
		_, _, pb := undoPlace(b)
		if pa < pb {
			return -1
		}
		if pa > pb {
			return 1
		}
		if a.ID() < b.ID() {
			return -1
		}
		if a.ID() > b.ID() {
			return 1
		}
		return 0
	})
	neighbors := make(map[ID]undoNeighbors, len(list))
	for i, sibling := range list {
		var place undoNeighbors
		if i > 0 {
			ref := list[i-1].Ref
			place.After = &ref
		}
		if i+1 < len(list) {
			ref := list[i+1].Ref
			place.Before = &ref
		}
		neighbors[sibling.ID()] = place
	}
	return neighbors, nil
}

func pruneUndo(ctx context.Context, tx *sql.Tx, budget int64) error {
	if budget <= 0 || budget > MaxUndoBytes {
		budget = MaxUndoBytes
	}
	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(bytes),0) FROM undo`).Scan(&total); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT session,stack,entry_order,bytes,coalesce(json_extract(components,'$.action_id'),'') FROM undo WHERE stack IN ('undo','redo') ORDER BY rowid`)
	if err != nil {
		return err
	}
	type candidate struct {
		Session  string
		Stack    string
		Order    int64
		Bytes    int64
		ActionID string
	}
	var candidates []candidate
	counts := map[string]int{}
	for rows.Next() {
		var row candidate
		if err := rows.Scan(&row.Session, &row.Stack, &row.Order, &row.Bytes, &row.ActionID); err != nil {
			return errors.Join(err, rows.Close())
		}
		candidates = append(candidates, row)
		counts[row.Session]++
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	evicted := map[string]bool{}
	for _, row := range candidates {
		if counts[row.Session] <= MaxUndoEntries && total <= budget {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM undo WHERE session=? AND stack=? AND entry_order=?`, row.Session, row.Stack, row.Order); err != nil {
			return err
		}
		counts[row.Session]--
		total -= row.Bytes
		if _, err := undoActionClosed(ctx, tx, row.Session, row.ActionID, true); err != nil {
			return err
		}
		if evicted[row.Session] {
			continue
		}
		state, err := loadUndoState(ctx, tx, row.Session)
		if err != nil {
			return err
		}
		var oldBytes int64
		err = tx.QueryRowContext(ctx, `SELECT bytes FROM undo WHERE session=? AND stack='state' AND entry_order=0`, row.Session).Scan(&oldBytes)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		state.Evicted = true
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if err := saveUndoState(ctx, tx, row.Session, state); err != nil {
			return err
		}
		// State creation or replacement changes the same budget as inverse payloads.
		total += int64(len(raw)) - oldBytes
		evicted[row.Session] = true
	}
	return nil
}

func undoEdit(ctx context.Context, tx *sql.Tx, opts editOptions, redo bool) (undoSummary, error) {
	before, err := beginUndoAdmission(ctx, tx)
	if err != nil {
		return undoSummary{}, err
	}
	opts.admissionBefore = before
	summary, err := undoEditsUnchecked(ctx, tx, opts, redo)
	return finishUndoAdmission(ctx, tx, before, summary, opts, err)
}

func undoEditsUnchecked(ctx context.Context, tx *sql.Tx, opts editOptions, redo bool) (undoSummary, error) {
	summary := undoSummary{Changes: map[ID]itemChange{}}
	if err := closeUndoAction(ctx, tx, opts.Session, ""); err != nil {
		return summary, err
	}
	stack, opposite := "undo", "redo"
	if redo {
		stack, opposite = opposite, stack
	}
	var order int64
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT entry_order,components FROM undo WHERE session=? AND stack=? ORDER BY entry_order DESC LIMIT 1`, opts.Session, stack).Scan(&order, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		state, err := loadUndoState(ctx, tx, opts.Session)
		summary.Empty, summary.Evicted = true, state.Evicted
		if state.Evicted {
			summary.Notices = append(summary.Notices, "No earlier undo is available")
		}
		return summary, err
	}
	if err != nil {
		return summary, err
	}
	var entry undoEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return summary, err
	}
	e := &edit{ctx: ctx, tx: tx, seq: opts.Seq, session: opts.Session, browser: true, changes: map[ID]itemChange{}, refs: map[string]Ref{}}
	start := map[ID]itemRecord{}
	missing := map[ID]bool{}
	for _, component := range entry.Components {
		for _, part := range component.Parts {
			id := part.Written.Item.ID()
			if _, ok := start[id]; ok || missing[id] {
				continue
			}
			record, err := loadRecord(ctx, tx, id)
			if errors.Is(err, sql.ErrNoRows) {
				missing[id] = true
				continue
			}
			if err != nil {
				return summary, err
			}
			start[id] = record
		}
	}
	for _, edge := range entry.Aggregates {
		if _, ok := start[edge.Ref.ID]; ok {
			continue
		}
		owner, err := loadRecord(ctx, tx, edge.Ref.ID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return summary, err
		}
		start[edge.Ref.ID] = owner
	}
	// Decide every guard before any inverse can advance a version or incarnation.
	reasons := make([]string, len(entry.Components))
	for i, component := range entry.Components {
		reason, err := undoGuard(ctx, tx, component, start, missing)
		if err != nil {
			return summary, err
		}
		reasons[i] = reason
	}
	inverse := undoEntry{At: opts.Now.UnixNano()}
	applied := make([]bool, len(entry.Components))
	lineage := map[ID]undoAggregate{}
	for i, component := range entry.Components {
		reason := reasons[i]
		if reason == "" {
			if _, err := tx.ExecContext(ctx, `SAVEPOINT notebook_undo_component`); err != nil {
				return summary, err
			}
			e.changes = map[ID]itemChange{}
			e.dbLineage = map[ID]undoAggregate{}
			e.pageBefore = map[ID]Version{}
			e.oldPlaces = map[ID]Item{}
			savedEffects := maps.Clone(e.effects)
			err := applyUndoComponent(e, component)
			if err == nil && hasDatabaseUndo(component) {
				err = e.refreshChanges()
			}
			if err == nil {
				err = e.validateStructure()
			}
			if err == nil {
				err = e.validateDatabases()
			}
			if err == nil {
				err = checkEditUsage(ctx, tx, opts.admissionBefore, opts.Limits)
			}
			if err != nil {
				if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO notebook_undo_component`); rollbackErr != nil {
					return summary, errors.Join(err, rollbackErr)
				}
				if _, releaseErr := tx.ExecContext(ctx, `RELEASE notebook_undo_component`); releaseErr != nil {
					return summary, errors.Join(err, releaseErr)
				}
				var constraint *sqlite.Error
				if errors.As(err, &constraint) && constraint.Code()&0xff == sqlite3.SQLITE_CONSTRAINT {
					err = invalidStructure()
				}
				var failure *editFailure
				if !errors.As(err, &failure) {
					return summary, err
				}
				e.effects = savedEffects
				reason = "structure"
				if failure.Reason == "item_limit" {
					reason = "capacity"
				}
			} else {
				reverse, err := makeUndoComponents(e, e.changes, map[undoList]map[ID]undoNeighbors{})
				if err != nil {
					return summary, err
				}
				inverse.Components = append(inverse.Components, reverse.Components...)
				if err := relinkUndo(ctx, tx, opts.Session, component, e.changes); err != nil {
					return summary, err
				}
				if _, err := tx.ExecContext(ctx, `RELEASE notebook_undo_component`); err != nil {
					return summary, err
				}
				for id, ch := range e.changes {
					if old, ok := summary.Changes[id]; ok {
						ch.Before = old.Before
						ch.Groups = append(ch.Groups, old.Groups...)
						slices.Sort(ch.Groups)
						ch.Groups = slices.Compact(ch.Groups)
					}
					summary.Changes[id] = ch
				}
				for id, edge := range e.dbLineage {
					if _, ok := lineage[id]; !ok {
						lineage[id] = edge
					}
				}
				applied[i] = true
				summary.Applied++
			}
		}
		if reason != "" {
			summary.Skipped++
			switch reason {
			case "later":
				summary.LaterEdits++
			case "missing":
				summary.Missing++
			case "structure":
				summary.Structure++
			case "capacity":
				summary.Capacity++
			}
		}
	}
	e.dbLineage = lineage
	if err := e.dbAttachLineage(&inverse); err != nil {
		return summary, err
	}
	if err := e.dbRelinkLineage(opts.Session, entry, start, applied); err != nil {
		return summary, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM undo WHERE session=? AND stack=? AND entry_order=?`, opts.Session, stack, order); err != nil {
		return summary, err
	}
	if len(inverse.Components) > 0 {
		var next int64
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(entry_order),0)+1 FROM undo WHERE session=? AND stack=?`, opts.Session, opposite).Scan(&next); err != nil {
			return summary, err
		}
		if err := saveUndoEntry(ctx, tx, opts.Session, opposite, next, inverse); err != nil {
			return summary, err
		}
	}
	if summary.LaterEdits > 0 {
		summary.Notices = append(summary.Notices, "Changes other people made later were not undone")
	}
	if summary.Missing > 0 {
		summary.Notices = append(summary.Notices, "Missing items were not undone")
	}
	if summary.Capacity > 0 {
		summary.Notices = append(summary.Notices, "Changes exceeding current item limits were not undone")
	}
	if summary.Structure > 0 {
		summary.Notices = append(summary.Notices, "Changes that would invalidate the current structure were not undone")
	}
	if err := pruneUndo(ctx, tx, opts.UndoBudget); err != nil {
		return summary, err
	}
	summary.Records = e.effects
	return summary, nil
}

func undoOwnGroups(item Item) []GroupKey {
	if databaseUndoKind(item.Kind()) {
		return databaseUndoGroups(item)
	}
	if item.Kind() == PageKind {
		return []GroupKey{TitleGroup, PlaceGroup}
	}
	return []GroupKey{ContentGroup, PlaceGroup}
}

func undoGuard(ctx context.Context, tx *sql.Tx, component undoComponent, start map[ID]itemRecord, missing map[ID]bool) (string, error) {
	if hasDatabaseUndo(component) {
		e := &edit{ctx: ctx, tx: tx}
		return e.databaseUndoGuard(component, start, missing)
	}
	deleting := map[ID]bool{}
	for _, part := range component.Parts {
		id := part.Written.Item.ID()
		if missing[id] {
			return "missing", nil
		}
		current := start[id]
		if current.Item.Ref.Inc != part.incarnation() {
			return "missing", nil
		}
		if current.Alive != part.Written.Alive {
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
			groups = undoOwnGroups(current.Item)
			deleting[id] = true
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
		query := `SELECT b.id FROM blocks b JOIN items i ON i.id=b.id WHERE i.alive=1 AND b.parent=?`
		if id.Kind() == PageKind {
			query = `SELECT p.id FROM pages p JOIN items i ON i.id=p.id WHERE i.alive=1 AND p.parent=? UNION ALL SELECT b.id FROM blocks b JOIN items i ON i.id=b.id WHERE i.alive=1 AND b.page=?`
		}
		args := []any{id}
		if id.Kind() == PageKind {
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return "", err
		}
		outside := false
		for rows.Next() {
			var child ID
			if err := rows.Scan(&child); err != nil {
				return "", errors.Join(err, rows.Close())
			}
			if !deleting[child] {
				outside = true
			}
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return "", err
		}
		if outside {
			return "later", nil
		}
	}
	return "", nil
}

func applyUndoComponent(e *edit, component undoComponent) error {
	if hasDatabaseUndo(component) {
		return e.applyDatabaseUndoComponent(component)
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
		if part.Desired == nil || !part.Desired.Alive {
			current.Alive, current.DeletedAt = false, e.seq
			if err := e.write(current); err != nil {
				return err
			}
			continue
		}
		if !part.Written.Alive {
			current = *part.Desired
			current.Alive, current.DeletedAt = true, 0
			current.Item.Ref.Inc = Incarnation(e.seq)
			current.Item.V = map[GroupKey]Version{}
			current.Item.Ver, current.Aggregate = Version(e.seq), Version(e.seq)
			if err := e.write(current); err != nil {
				return err
			}
		} else {
			for _, group := range part.Groups {
				if group == PlaceGroup {
					continue
				}
				if err := undoAssignGroup(&current.Item, part.Desired.Item, group); err != nil {
					return err
				}
			}
			groups := slices.DeleteFunc(slices.Clone(part.Groups), func(g GroupKey) bool { return g == PlaceGroup })
			if len(groups) > 0 {
				if err := e.write(current, groups...); err != nil {
					return err
				}
			}
		}
	}
	places := slices.DeleteFunc(slices.Clone(component.Parts), func(part undoPart) bool {
		return part.Desired == nil || !part.Desired.Alive || (part.Written.Alive && !slices.Contains(part.Groups, PlaceGroup))
	})
	slices.SortStableFunc(places, func(a, b undoPart) int {
		_, _, pa := undoPlace(a.Desired.Item)
		_, _, pb := undoPlace(b.Desired.Item)
		if pa < pb {
			return -1
		}
		if pa > pb {
			return 1
		}
		if a.Written.Item.ID() < b.Written.Item.ID() {
			return -1
		}
		if a.Written.Item.ID() > b.Written.Item.ID() {
			return 1
		}
		return 0
	})
	for _, part := range places {
		if err := undoRestorePlace(e, part, reviving); err != nil {
			return err
		}
	}
	return nil
}

func undoAssignGroup(current *Item, desired Item, group GroupKey) error {
	switch data := current.Data.(type) {
	case Page:
		if group != TitleGroup {
			return fmt.Errorf("unsupported page undo group %s", group)
		}
		data.Title = desired.Data.(Page).Title
		current.Data = data
	case Block:
		if group != ContentGroup {
			return fmt.Errorf("unsupported block undo group %s", group)
		}
		data.Content = desired.Data.(Block).Content
		current.Data = data
	default:
		return fmt.Errorf("unsupported undo kind %s", current.Kind())
	}
	return nil
}

func undoRestorePlace(e *edit, part undoPart, reviving map[ID]bool) error {
	id := part.Written.Item.ID()
	page, parent, _ := undoPlace(part.Desired.Item)
	if parent != nil {
		owner, err := loadRecord(e.ctx, e.tx, *parent)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) || !owner.Alive {
			if part.Written.Alive {
				return invalidStructure()
			}
			parent = nil
		}
	}
	if id.Kind() == BlockKind {
		owner, err := loadRecord(e.ctx, e.tx, page)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) || !owner.Alive {
			return invalidStructure()
		}
	}
	anchor := func(ref *Ref) (*Target, error) {
		if ref == nil || ref.ID == id {
			return nil, nil
		}
		record, err := loadRecord(e.ctx, e.tx, ref.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !record.Alive || (!reviving[ref.ID] && record.Item.Ref != *ref) || !undoSameList(record.Item, id.Kind(), page, parent) {
			return nil, nil
		}
		r := record.Item.Ref
		return &Target{Ref: &r}, nil
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
	position, err := e.place(id.Kind(), page, parent, before, after, map[ID]bool{id: true})
	if err != nil {
		return err
	}
	current, err := loadRecord(e.ctx, e.tx, id)
	if err != nil {
		return err
	}
	switch data := current.Item.Data.(type) {
	case Page:
		data.Parent, data.Position = parent, position
		current.Item.Data = data
	case Block:
		data.Page, data.Parent, data.Position = page, parent, position
		current.Item.Data = data
	}
	return e.write(current, PlaceGroup)
}

func (e *edit) dbRelinkLineage(session string, entry undoEntry, start map[ID]itemRecord, applied []bool) error {
	edges := map[ID]undoAggregate{}
	versions := map[ID]Version{}
	for _, edge := range entry.Aggregates {
		owner, ok := start[edge.Ref.ID]
		if !ok || !owner.Alive || owner.Item.Ref != edge.Ref || owner.Item.Ver != edge.Written {
			continue
		}
		if len(edge.Components) == 0 || slices.ContainsFunc(edge.Components, func(ci int) bool { return ci < 0 || ci >= len(applied) || !applied[ci] }) {
			continue
		}
		current, err := loadRecord(e.ctx, e.tx, edge.Ref.ID)
		if err != nil {
			return err
		}
		if !current.Alive || current.Item.Ref != edge.Ref {
			continue
		}
		edges[edge.Ref.ID] = edge
		versions[edge.Ref.ID] = current.Item.Ver
	}
	if len(edges) == 0 {
		return nil
	}
	rows, err := readUndoRows(e.ctx, e.tx, session)
	if err != nil {
		return err
	}
	for _, row := range rows {
		changed := false
		for ci := range row.Entry.Components {
			for pi := range row.Entry.Components[ci].Parts {
				part := &row.Entry.Components[ci].Parts[pi]
				edge, ok := edges[part.Written.Item.ID()]
				if !ok || part.incarnation() != edge.Ref.Inc {
					continue
				}
				version := part.GuardVersion
				if version == 0 {
					version = part.Written.Item.Ver
				}
				if version != edge.Before {
					continue
				}
				part.GuardVersion = versions[edge.Ref.ID]
				changed = true
			}
		}
		for ai := range row.Entry.Aggregates {
			older := &row.Entry.Aggregates[ai]
			edge, ok := edges[older.Ref.ID]
			if !ok || older.Ref != edge.Ref || older.Written != edge.Before {
				continue
			}
			older.Written = versions[edge.Ref.ID]
			changed = true
		}
		if changed {
			if err := saveUndoEntry(e.ctx, e.tx, session, row.Stack, row.Order, row.Entry); err != nil {
				return err
			}
		}
	}
	return nil
}

func relinkUndo(ctx context.Context, tx *sql.Tx, session string, applied undoComponent, changes map[ID]itemChange) error {
	rows, err := readUndoRows(ctx, tx, session)
	if err != nil {
		return err
	}
	for _, row := range rows {
		changed := false
		for ci := range row.Entry.Components {
			for pi := range row.Entry.Components[ci].Parts {
				older := &row.Entry.Components[ci].Parts[pi]
				for _, neighbor := range []*Ref{older.Neighbors.Before, older.Neighbors.After} {
					if neighbor == nil {
						continue
					}
					ch, ok := changes[neighbor.ID]
					if ok && ch.Before != nil && !ch.Before.Alive && ch.After.Alive && *neighbor == ch.Before.Item.Ref {
						*neighbor = ch.After.Item.Ref
						changed = true
					}
				}
				for _, part := range applied.Parts {
					id := part.Written.Item.ID()
					ch, ok := changes[id]
					if !ok || ch.Before == nil || part.Desired == nil || !part.Desired.Alive || older.Written.Item.ID() != id || older.incarnation() != ch.Before.Item.Ref.Inc {
						continue
					}
					guardVersion := older.GuardVersion
					if guardVersion == 0 {
						guardVersion = older.Written.Item.Ver
					}
					appliedVersion := part.GuardVersion
					if appliedVersion == 0 {
						appliedVersion = part.Written.Item.Ver
					}
					if part.Desired.Item.Ver == guardVersion && ch.Before.Item.Ver == appliedVersion && (id.Kind() != DatabaseKind || !part.Written.Alive) {
						older.GuardVersion = ch.After.Item.Ver
						changed = true
					}
					groups := ch.Groups
					if !part.Written.Alive && ch.After.Alive {
						older.GuardInc = ch.After.Item.Ref.Inc
						groups = append(undoOwnGroups(ch.After.Item), older.Groups...)
						slices.Sort(groups)
						groups = slices.Compact(groups)
						changed = true
					}
					for _, group := range groups {
						previous, err := part.Desired.Item.GroupVersion(group)
						if err != nil {
							return err
						}
						written, err := older.version(group)
						if err != nil {
							return err
						}
						if previous != written {
							continue
						}
						version, err := ch.After.Item.GroupVersion(group)
						if err != nil {
							return err
						}
						if older.GuardVersions == nil {
							older.GuardVersions = map[GroupKey]Version{}
						}
						older.GuardVersions[group] = version
						changed = true
					}
				}
			}
		}
		for ai := range row.Entry.Aggregates {
			edge := &row.Entry.Aggregates[ai]
			for _, part := range applied.Parts {
				ch, ok := changes[edge.Ref.ID]
				if !ok || ch.Before == nil || part.Written.Item.ID() != edge.Ref.ID || part.Desired == nil || !part.Desired.Alive || part.Written.Alive || !ch.After.Alive || edge.Ref != ch.Before.Item.Ref {
					continue
				}
				edge.Ref = ch.After.Item.Ref
				if edge.Written == part.Desired.Item.Ver {
					edge.Written = ch.After.Item.Ver
				}
				changed = true
			}
		}
		if changed {
			if err := saveUndoEntry(ctx, tx, session, row.Stack, row.Order, row.Entry); err != nil {
				return err
			}
		}
	}
	for id, ch := range changes {
		if ch.Before != nil && !ch.Before.Alive && ch.After.Alive {
			if _, err := tx.ExecContext(ctx, `UPDATE created SET inc=? WHERE session=? AND id=?`, fmt.Sprint(uint64(ch.After.Item.Ref.Inc)), session, id); err != nil {
				return err
			}
		}
	}
	return nil
}
