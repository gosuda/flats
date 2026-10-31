package notebook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// Recover restores one historical subtree without changing collision owners.
func (in *Instance) Recover(ctx context.Context, src RevisionSource, page ID, place Placement, session string, requestID ...string) (WriteResult, error) {
	if len(requestID) > 1 {
		return WriteResult{}, &WriteError{Code: "invalid", Reason: "request_id"}
	}
	identity := ""
	if len(requestID) == 1 {
		identity = requestID[0]
	}
	op, hash, err := recoveryIdentity(src.Number, page, session, identity)
	if err != nil {
		return WriteResult{}, err
	}
	// Recovery chooses placement from current structure, not a stale browser tree.
	if place.Page.Ref != nil || place.Page.Tmp != "" || place.Parent != nil || place.Before != nil || place.After != nil {
		return WriteResult{}, &WriteError{Code: "invalid", Reason: "recovery_placement"}
	}
	if result, found, err := in.replayUpdate(ctx, op, hash); err != nil || found {
		return result, err
	}
	headers, err := openRevisionStore(ctx, src, func(p revisionPath) bool { return p.kind == "page" })
	if err != nil {
		return WriteResult{}, err
	}
	selected := map[ID]bool{}
	query, err := headers.store.db.QueryContext(ctx, `WITH RECURSIVE chosen(id) AS (SELECT id FROM pages WHERE id=? UNION SELECT p.id FROM pages p JOIN chosen c ON p.parent=c.id) SELECT id FROM chosen`, page)
	if err != nil {
		_ = headers.close()
		return WriteResult{}, err
	}
	for query.Next() {
		var id ID
		if err := query.Scan(&id); err != nil {
			_ = query.Close()
			_ = headers.close()
			return WriteResult{}, err
		}
		selected[id] = true
	}
	err = errors.Join(query.Err(), query.Close(), headers.close())
	if err != nil {
		return WriteResult{}, err
	}
	if !selected[page] {
		return WriteResult{}, &WriteError{Code: "not_found", Reason: "page"}
	}
	source, err := openRevisionStore(ctx, src, func(p revisionPath) bool { return selected[p.page] })
	if err != nil {
		return WriteResult{}, err
	}
	defer source.close()
	var records []itemRecord
	err = source.store.transaction(ctx, func(tx *sql.Tx) error {
		root, err := loadRecord(ctx, tx, page)
		if errors.Is(err, sql.ErrNoRows) {
			return &WriteError{Code: "not_found", Reason: "page"}
		}
		if err != nil {
			return err
		}
		e := &edit{ctx: ctx, tx: tx}
		records, err = e.subtree(root)
		if err != nil {
			return err
		}
		blocks := slices.Clone(records)
		for _, r := range blocks {
			b, ok := r.Item.Data.(Block)
			if !ok || b.Content.Kind != InlineDatabase {
				continue
			}
			db, err := loadRecord(ctx, tx, b.Content.Database)
			if err != nil {
				return err
			}
			records = append(records, db)
			for _, kind := range []Kind{PropertyKind, ViewKind, RowKind} {
				children, err := e.dbMembers(db.Item.ID(), kind)
				if err != nil {
					return err
				}
				records = append(records, children...)
				if kind == PropertyKind {
					for _, p := range children {
						options, err := e.records(`SELECT id FROM options WHERE property_id=? AND alive=1 ORDER BY id`, p.Item.ID())
						if err != nil {
							return err
						}
						records = append(records, options...)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return WriteResult{}, err
	}
	// A historical image is already in the shared blob store. Keep it leased
	// until the live reference and its quota reservation have committed.
	hashes := []string{}
	for _, r := range records {
		if b, ok := r.Item.Data.(Block); ok && b.Content.Kind == Image {
			hashes = append(hashes, b.Content.Asset)
		}
	}
	release := in.b.Lease(hashes)
	defer release()
	if err := in.Remeasure(ctx); err != nil {
		return WriteResult{}, err
	}
	var result WriteResult
	err = in.write(ctx, func(tx *sql.Tx) error {
		s, err := readSession(ctx, tx, contentHash([]byte(session)), in.now())
		if err != nil {
			return err
		}
		if replay, found, err := loadRecovery(ctx, tx, op, hash); err != nil || found {
			result = replay
			return err
		}
		other, valid := in.measure.get()
		if !valid {
			return unavailable("measurement")
		}
		if err := in.checkQuota(ctx, tx, other, 0); err != nil {
			return err
		}
		if err := in.outboxAdmission(ctx, tx); err != nil {
			return err
		}
		if err := in.closeGroups(ctx, tx, true); err != nil {
			return err
		}
		before, err := measureEditUsage(ctx, tx)
		if err != nil {
			return err
		}
		seq, err := nextSequence(ctx, tx)
		if err != nil {
			return err
		}
		e := &edit{ctx: ctx, tx: tx, seq: seq, session: s.hash, browser: true, refs: map[string]Ref{}, changes: map[ID]itemChange{}, oldPlaces: map[ID]Item{}}
		current, err := loadRecord(ctx, tx, page)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		copyAll := err == nil && current.Alive
		ids := make(map[ID]ID, len(records))
		for _, r := range records {
			id := r.Item.ID()
			var alive bool
			err := tx.QueryRowContext(ctx, `SELECT alive FROM items WHERE id=?`, id).Scan(&alive)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if copyAll || alive {
				id, err = NewID(r.Item.Kind())
				if err != nil {
					return err
				}
			}
			ids[r.Item.ID()] = id
		}
		rootPage := records[0].Item.Data.(Page)
		parent := rootPage.Parent
		var after *Target
		if copyAll {
			parent = current.Item.Data.(Page).Parent
			ref := current.Item.Ref
			after = &Target{Ref: &ref}
			rootPage.Title += " (restored)"
		} else if parent != nil {
			var alive bool
			err := tx.QueryRowContext(ctx, `SELECT alive FROM items WHERE id=?`, *parent).Scan(&alive)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if !alive {
				parent = nil
			}
		}
		position, err := e.place(PageKind, "", parent, nil, after, nil)
		if err != nil {
			return err
		}
		copies, err := remapCopies(records, ids, map[ID]copyPlacement{page: {Parent: parent, Position: position}}, seq, databaseRemapper{})
		if err != nil {
			return err
		}
		for i := range copies {
			if copies[i].Item.ID() == ids[page] {
				p := copies[i].Item.Data.(Page)
				p.Title = rootPage.Title
				copies[i].Item.Data = p
			}
		}
		// Parent and database rows must exist before aggregate propagation.
		for _, kind := range []Kind{PageKind, BlockKind, DatabaseKind, PropertyKind, OptionKind, RowKind, ViewKind} {
			for _, r := range copies {
				if r.Item.Kind() == kind {
					if err := e.write(r); err != nil {
						return err
					}
				}
			}
		}
		if err := e.validateStructure(); err != nil {
			return err
		}
		if err := e.validateDatabases(); err != nil {
			return err
		}
		if err := checkEditUsage(ctx, tx, before, nil); err != nil {
			return err
		}
		if err := installRevisionAssets(ctx, tx, source.store, hashes); err != nil {
			return err
		}
		if err := refreshHistoryAssets(ctx, tx); err != nil {
			return err
		}
		if err := e.refreshChanges(); err != nil {
			return err
		}
		if err := recordUndo(e, editOptions{Browser: true, Session: s.hash, Seq: seq, ActionID: op, Close: true, Now: in.now(), UndoBudget: undoBudget(in.b.Limits().DiskQuota)}); err != nil {
			return err
		}
		if err := syncContentFiles(ctx, tx, seq); err != nil {
			return err
		}
		if err := touchGroup(ctx, tx, s, in.now()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_at=? WHERE token_hash=?`, in.now().Unix(), s.hash); err != nil {
			return err
		}
		if err := setSequence(ctx, tx, seq); err != nil {
			return err
		}
		if err := saveDelta(ctx, tx, seq, editResult{Records: e.effects}, sessionSource(s.hash), in.now()); err != nil {
			return err
		}
		if err := in.checkQuota(ctx, tx, other, 0); err != nil {
			return err
		}
		root, err := loadRecord(ctx, tx, ids[page])
		if err != nil {
			return err
		}
		counts := [7]int{}
		kinds := []Kind{PageKind, BlockKind, DatabaseKind, PropertyKind, OptionKind, RowKind, ViewKind}
		for _, r := range copies {
			counts[slices.Index(kinds, r.Item.Kind())]++
		}
		counts[0]--
		outbox, err := recordOutbox(ctx, tx, "person", "", op, []string{s.label}, in.now(), true)
		if err != nil {
			return err
		}
		result = WriteResult{State: State{Store: in.storeID, Seq: seq}, RevisionPending: true, Mapping: ResultMapping{Complete: true}, Applied: 1, Refs: map[string]AssignedRef{"root": {Ref: root.Item.Ref, Ver: root.Item.Ver}}, Warnings: []WriteWarning{}, Samples: WriteSamples{Warnings: SampleInfo{Complete: true}}, Copies: []CopySummary{{RootsTotal: 1, OtherCounts: counts, Roots: []RootMapping{{Source: records[0].Item.Ref, Copy: root.Item.Ref, Ver: root.Item.Ver}}, RootsComplete: true}}}
		outcome, err := json.Marshal(commandOutcome{Result: &result})
		if err != nil {
			return err
		}
		state, err := json.Marshal(result.State)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO commands(op_id,request_hash,outcome,mapping_cursor,state,outbox_seq,import_status) VALUES(?,?,?,'',?,?,'pending')`, op, hash, string(outcome), string(state), strconv.FormatInt(outbox, 10))
		if err != nil {
			return err
		}
		return in.checkQuota(ctx, tx, other, 0)
	})
	return result, permanentWriteErrorOrNil(err)
}

func recoveryIdentity(revision int, page ID, session, requestID string) (string, string, error) {
	if revision < 1 || page.ValidateKind(PageKind) != nil || session == "" || requestID != "" && !operationID.MatchString(requestID) {
		return "", "", &WriteError{Code: "invalid", Reason: "recovery_identity"}
	}
	raw, err := json.Marshal(struct {
		Revision int
		Page     ID
	}{revision, page})
	if err != nil {
		return "", "", err
	}
	hash := contentHash(raw)
	if requestID == "" {
		requestID = hash
	}
	return "recover-" + contentHash([]byte(session+"\x00"+requestID)), hash, nil
}

func loadRecovery(ctx context.Context, tx *sql.Tx, op, hash string) (WriteResult, bool, error) {
	var storedHash, raw string
	err := tx.QueryRowContext(ctx, `SELECT request_hash,outcome FROM commands WHERE op_id=?`, op).Scan(&storedHash, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return WriteResult{}, false, nil
	}
	if err != nil {
		return WriteResult{}, false, err
	}
	if storedHash != hash {
		return WriteResult{}, true, &WriteError{Code: "conflict", Reason: "op_id_conflict"}
	}
	var outcome commandOutcome
	if err := json.Unmarshal([]byte(raw), &outcome); err != nil {
		return WriteResult{}, true, err
	}
	if outcome.Result == nil {
		return WriteResult{}, true, unavailable("receipt_corrupt")
	}
	result := *outcome.Result
	result.Replay = true
	return result, true, nil
}

func permanentWriteErrorOrNil(err error) error {
	if err == nil {
		return nil
	}
	return permanentWriteError(err)
}

func installRevisionAssets(ctx context.Context, tx *sql.Tx, s *store, hashes []string) error {
	for _, hash := range slices.Compact(slices.Sorted(slices.Values(hashes))) {
		var mime string
		var width, height int
		var size int64
		if err := s.db.QueryRowContext(ctx, `SELECT mime,width,height,bytes FROM assets WHERE hash=?`, hash).Scan(&mime, &width, &height, &size); err != nil {
			return fmt.Errorf("historical image metadata: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO assets(hash,mime,width,height,bytes,live_refs,deleted_refs,protected_until) VALUES(?,?,?,?,?,0,0,0) ON CONFLICT(hash) DO NOTHING`, hash, mime, width, height, size); err != nil {
			return err
		}
	}
	return nil
}

func refreshHistoryAssets(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE assets SET live_refs=(SELECT count(*) FROM items i WHERE i.kind='block' AND i.alive=1 AND json_extract(i.data,'$.content.kind')='image' AND json_extract(i.data,'$.content.asset')=assets.hash),deleted_refs=(SELECT count(*) FROM items i WHERE i.kind='block' AND i.alive=0 AND json_extract(i.data,'$.content.kind')='image' AND json_extract(i.data,'$.content.asset')=assets.hash)`)
	return err
}
