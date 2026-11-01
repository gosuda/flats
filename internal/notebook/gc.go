package notebook

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// SweepReferences collects expired, unreferenced asset metadata at a writer
// barrier and returns the hashes still rooted in runtime state. The caller holds
// the host storage lock; protect checks retained history, holds, leases and blob
// grace without re-entering the notebook writer or acquiring that storage lock.
func (in *Instance) SweepReferences(ctx context.Context, protect func(string) (bool, error)) ([]string, error) {
	roots := map[string]bool{}
	err := in.write(ctx, func(tx *sql.Tx) error {
		undo, err := readUndoRows(ctx, tx, "")
		if err != nil {
			return err
		}
		addItem := func(item Item) {
			if block, ok := item.Data.(Block); ok && block.Content.Kind == Image {
				roots[block.Content.Asset] = true
			}
		}
		for _, row := range undo {
			for _, component := range row.Entry.Components {
				for _, part := range component.Parts {
					addItem(part.Written.Item)
					if part.Desired != nil {
						addItem(part.Desired.Item)
					}
				}
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT hash FROM assets WHERE live_refs>0 OR deleted_refs>0 OR protected_until>?
			UNION SELECT hash FROM snapshots WHERE refs>0
			UNION SELECT json_extract(f.value,'$.hash') FROM outbox o,json_each(o.files) f`, in.now().Unix())
		if err != nil {
			return err
		}
		for rows.Next() {
			var hash string
			if err := rows.Scan(&hash); err != nil {
				return errors.Join(err, rows.Close())
			}
			roots[hash] = true
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, `SELECT hash FROM assets WHERE live_refs=0 AND deleted_refs=0 AND protected_until<=? ORDER BY hash`, in.now().Unix())
		if err != nil {
			return err
		}
		var candidates []string
		for rows.Next() {
			var hash string
			if err := rows.Scan(&hash); err != nil {
				return errors.Join(err, rows.Close())
			}
			if !roots[hash] {
				candidates = append(candidates, hash)
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, hash := range candidates {
			held, err := protect(hash)
			if err != nil {
				return err
			}
			if held {
				roots[hash] = true
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM assets WHERE hash=?`, hash); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(roots))
	for hash := range roots {
		hashes = append(hashes, hash)
	}
	slices.Sort(hashes)
	return hashes, nil
}

const maxDeletedPayload = 16 << 20

// collectDeleted keeps the allocated-id ledger and every surviving inverse.
// Deletion pressure may evict old undo entries through the normal notice path.
func (in *Instance) collectDeleted(ctx context.Context, tx *sql.Tx) error {
	held := map[ID]bool{}
	rows, err := readUndoRows(ctx, tx, "")
	if err != nil {
		return err
	}
	for _, row := range rows {
		for _, component := range row.Entry.Components {
			for _, part := range component.Parts {
				held[part.Written.Item.ID()] = true
				if part.Desired != nil {
					held[part.Desired.Item.ID()] = true
				}
			}
		}
	}
	// Count all payload tables as well as typed JSON. Identity ledger bytes are
	// intentionally not part of retained deletion payload.
	candidates, err := tx.QueryContext(ctx, `SELECT i.id,length(CAST(i.data AS BLOB))+128+coalesce((SELECT sum(length(CAST(value AS BLOB))+length(property_id)+16) FROM cells WHERE row_id=i.id),0)+coalesce((SELECT sum(length(CAST(ids AS BLOB))+32) FROM orders WHERE owner_id=i.id),0)+coalesce((SELECT sum(length(group_key)+length(property_id)+length(version)+24) FROM group_versions WHERE item_id=i.id),0)+coalesce((SELECT length(CAST(name_key AS BLOB))+length(database_id)+length(id)+32 FROM properties WHERE id=i.id),0)+coalesce((SELECT length(CAST(name_key AS BLOB))+length(property_id)+length(id)+32 FROM options WHERE id=i.id),0) FROM items i WHERE alive=0 ORDER BY deleted_seq,id`)
	if err != nil {
		return err
	}
	type candidate struct {
		id    ID
		bytes int64
	}
	var pending []candidate
	var total int64
	for candidates.Next() {
		var c candidate
		if err := candidates.Scan(&c.id, &c.bytes); err != nil {
			return errors.Join(err, candidates.Close())
		}
		total += c.bytes
		pending = append(pending, c)
	}
	if err := errors.Join(candidates.Err(), candidates.Close()); err != nil {
		return err
	}
	var protected int64
	for _, c := range pending {
		if held[c.id] {
			protected += c.bytes
		}
	}
	if protected > maxDeletedPayload {
		if err := pruneUndo(ctx, tx, maxDeletedPayload/2); err != nil {
			return err
		}
		return in.collectDeleted(ctx, tx)
	}
	for _, c := range pending {
		if total <= maxDeletedPayload {
			break
		}
		if held[c.id] {
			continue
		}
		// A zero-incarnation reference already resolves to gone for these items.
		// Drop its mapping before the payload; immutable command mappings contain
		// their own refs and never depend on the deleted item's data.
		if _, err := tx.ExecContext(ctx, `DELETE FROM created WHERE id=?`, c.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM items WHERE id=? AND alive=0`, c.id); err != nil {
			return err
		}
		total -= c.bytes
	}
	return refreshHistoryAssets(ctx, tx)
}
