package notebook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
)

// Rollback installs exact retained content without creating content blobs or
// consulting quota and item limits. The host owns the revision hold.
func (in *Instance) Rollback(ctx context.Context, src RevisionSource, label string) (WriteResult, error) {
	source, err := openRevisionStore(ctx, src, nil)
	if err != nil {
		return WriteResult{}, err
	}
	defer source.close()
	var result WriteResult
	err = in.write(ctx, func(tx *sql.Tx) error {
		if err := in.closeGroups(ctx, tx, true); err != nil {
			return err
		}
		seq, err := nextSequence(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE history_before(id TEXT PRIMARY KEY,record BLOB NOT NULL)`); err != nil {
			return err
		}
		defer tx.ExecContext(context.Background(), `DROP TABLE IF EXISTS history_before`)
		if err := visitHistoryRecords(ctx, tx, func(r itemRecord) error {
			data, err := json.Marshal(r)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO history_before(id,record) VALUES(?,?)`, r.Item.ID(), data)
			return err
		}); err != nil {
			return err
		}
		// Deactivate unique live names before replacing schema; two surviving
		// properties can exchange names between revisions.
		for _, query := range []string{`UPDATE properties SET alive=0`, `UPDATE options SET alive=0`, `DELETE FROM databases`} {
			if _, err := tx.ExecContext(ctx, query); err != nil {
				return err
			}
		}
		loadBefore := func(id ID) (itemRecord, error) {
			var raw []byte
			if err := tx.QueryRowContext(ctx, `SELECT record FROM history_before WHERE id=?`, id).Scan(&raw); err != nil {
				return itemRecord{}, err
			}
			var r itemRecord
			err := json.Unmarshal(raw, &r)
			return r, err
		}
		// Install tombstones first so removed database ownership remains represented.
		var after ID
		for {
			var id ID
			err := tx.QueryRowContext(ctx, `SELECT id FROM history_before WHERE id>? ORDER BY id LIMIT 1`, after).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return err
			}
			after = id
			r, err := loadBefore(id)
			if err != nil {
				return err
			}
			var present int
			if err := source.store.db.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE id=?`, id).Scan(&present); err != nil {
				return err
			}
			if present > 0 {
				continue
			}
			if r.Alive {
				r.Alive = false
				r.DeletedAt = seq
			}
			if err := putRecord(ctx, tx, r); err != nil {
				return err
			}
		}
		changedPages := map[ID]bool{}
		changedDatabases := map[ID]bool{}
		err = visitHistoryRecords(ctx, source.store.db, func(target itemRecord) error {
			old, err := loadBefore(target.Item.ID())
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil && old.Alive {
				target.Item.Ref = old.Item.Ref
				target.Item.V = maps.Clone(old.Item.V)
				target.Item.Ver = old.Item.Ver
				target.Aggregate = old.Aggregate
				groups := slices.Concat(databaseUndoGroups(old.Item), databaseUndoGroups(target.Item))
				slices.Sort(groups)
				groups = slices.Compact(groups)
				for _, group := range groups {
					if !reflect.DeepEqual(historyGroup(old.Item, group), historyGroup(target.Item, group)) {
						target.Item.V[group] = Version(seq)
					}
				}
				if !reflect.DeepEqual(old.Item.Data, target.Item.Data) {
					target.Aggregate = Version(seq)
				}
			} else {
				target.Item.Ref.Inc = Incarnation(seq)
				target.Item.V = map[GroupKey]Version{}
				target.Aggregate = Version(seq)
			}
			target.Item.Ver = max(Version(target.Item.Ref.Inc), target.Aggregate)
			for _, v := range target.Item.V {
				target.Item.Ver = max(target.Item.Ver, v)
			}
			if err != nil || !old.Alive || !reflect.DeepEqual(old.Item.Data, target.Item.Data) {
				historyChangedOwners(old.Item, changedPages, changedDatabases)
				historyChangedOwners(target.Item, changedPages, changedDatabases)
			}
			return putRecord(ctx, tx, target)
		})
		if err != nil {
			return err
		}
		// Deleted descendants also advance their surviving owners' aggregates.
		after = ""
		for {
			var id ID
			err := tx.QueryRowContext(ctx, `SELECT id FROM history_before WHERE id>? ORDER BY id LIMIT 1`, after).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return err
			}
			after = id
			old, err := loadBefore(id)
			if err != nil {
				return err
			}
			if old.Alive {
				var alive bool
				if err := tx.QueryRowContext(ctx, `SELECT alive FROM items WHERE id=?`, id).Scan(&alive); err != nil {
					return err
				}
				if !alive {
					historyChangedOwners(old.Item, changedPages, changedDatabases)
				}
			}
		}
		options, err := tx.QueryContext(ctx, `SELECT DISTINCT p.database_id FROM options o JOIN properties p ON p.id=o.property_id JOIN items i ON i.id=o.id WHERE i.own_version=? OR i.deleted_seq=?`, sequenceString(seq), sequenceString(seq))
		if err != nil {
			return err
		}
		for options.Next() {
			var id ID
			if err := options.Scan(&id); err != nil {
				return errors.Join(err, options.Close())
			}
			changedDatabases[id] = true
		}
		if err := errors.Join(options.Err(), options.Close()); err != nil {
			return err
		}
		for id := range changedDatabases {
			r, err := loadRecord(ctx, tx, id)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if !r.Alive {
				continue
			}
			r.Aggregate = Version(seq)
			r.Item.Ver = Version(seq)
			if err := putRecord(ctx, tx, r); err != nil {
				return err
			}
			block, err := loadRecord(ctx, tx, r.Item.Data.(Database).Block)
			if err != nil {
				return err
			}
			changedPages[block.Item.Data.(Block).Page] = true
		}
		for id := range changedPages {
			r, err := loadRecord(ctx, tx, id)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if !r.Alive {
				continue
			}
			r.Aggregate = Version(seq)
			r.Item.Ver = Version(seq)
			if err := putRecord(ctx, tx, r); err != nil {
				return err
			}
		}
		hashes := []string{}
		for _, f := range source.files {
			p, _ := parseRevisionPath(f.Path)
			if p.kind == "asset" {
				hashes = append(hashes, f.Hash)
			}
		}
		if err := installRevisionAssets(ctx, tx, source.store, hashes); err != nil {
			return err
		}
		if err := refreshHistoryAssets(ctx, tx); err != nil {
			return err
		}
		for _, query := range []string{`DELETE FROM undo`, `DELETE FROM created`, `DELETE FROM view_notices`, `DELETE FROM deltas`, `DELETE FROM files`} {
			if _, err := tx.ExecContext(ctx, query); err != nil {
				return err
			}
		}
		for _, f := range source.files {
			p, _ := parseRevisionPath(f.Path)
			var owner any
			if p.page != "" {
				owner = p.page
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO files(path,page,bytes,dirty_seq,hash) VALUES(?,?,?,'0',?)`, f.Path, owner, f.Size, f.Hash); err != nil {
				return err
			}
		}
		hashesRaw, err := CanonicalJSON(outboxHashes{CreatedAt: in.now().Unix(), Hashes: []string{}})
		if err != nil {
			return err
		}
		authors, err := CanonicalJSON([]string{label})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO outbox(kind,authors,message,rollback_target,files,hashes) VALUES('rollback',?,?,?,(SELECT json_group_array(json_object('hash',hash,'path',path,'size',bytes)) FROM (SELECT path,hash,bytes FROM files ORDER BY path)),?)`, string(authors), "Rollback", src.Number, string(hashesRaw))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE meta SET seq=?,reset_seq=?,files=(SELECT files FROM outbox ORDER BY seq DESC LIMIT 1) WHERE singleton=1`, sequenceString(seq), sequenceString(seq)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO deltas(seq,effects,source,bytes,at) VALUES(?,?,'rollback',?,?)`, sequenceString(seq), `{"changes":[],"reset":true}`, len(`{"changes":[],"reset":true}`), in.now().Unix()); err != nil {
			return err
		}
		result = WriteResult{State: State{Store: in.storeID, Seq: seq}, RevisionPending: true, Mapping: ResultMapping{Complete: true}, Applied: 1, Warnings: []WriteWarning{}, Samples: WriteSamples{Warnings: SampleInfo{Complete: true}}}
		return nil
	})
	return result, permanentWriteErrorOrNil(err)
}

func visitHistoryRecords(ctx context.Context, q dbReader, fn func(itemRecord) error) error {
	var after ID
	for {
		var id ID
		err := q.QueryRowContext(ctx, `SELECT id FROM items WHERE id>? ORDER BY id LIMIT 1`, after).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		after = id
		record, err := loadRecord(ctx, q, id)
		if err != nil {
			return err
		}
		if err := fn(record); err != nil {
			return err
		}
	}
}

func historyChangedOwners(item Item, pages, databases map[ID]bool) {
	switch d := item.Data.(type) {
	case Page:
		pages[item.ID()] = true
	case Block:
		pages[d.Page] = true
	case Database:
		databases[item.ID()] = true
	case Property:
		databases[d.Database] = true
	case Row:
		databases[d.Database] = true
	case View:
		databases[d.Database] = true
	}
}

func historyGroup(item Item, g GroupKey) any {
	switch d := item.Data.(type) {
	case Page:
		if g == TitleGroup {
			return d.Title
		}
		return PagePlace{d.Parent, d.Position}
	case Block:
		if g == ContentGroup {
			return d.Content
		}
		return BlockPlace{d.Page, d.Parent, d.Position}
	case Database:
		switch g {
		case TitleGroup:
			return d.Title
		case PropertyOrderGroup:
			return d.PropertyOrder
		case ViewOrderGroup:
			return d.ViewOrder
		}
	case Property:
		if g == DefinitionGroup {
			return d.Definition
		}
		return d.OptionOrder
	case Option:
		return OptionDefinition{d.Name, d.Color}
	case View:
		return d.Definition
	case Row:
		if g == PlaceGroup {
			return d.Position
		}
		return d.Cells[ID(string(g)[len("cell:"):])]
	}
	return nil
}
