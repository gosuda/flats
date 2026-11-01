package notebook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func assetReferenceAdd(t *testing.T, s *store, hashes ...string) {
	t.Helper()
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		for _, hash := range hashes {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO assets(hash,mime,width,height,bytes,live_refs,deleted_refs,protected_until) VALUES(?,'image/png',1,1,99,0,0,1234)`, hash); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assetReferenceCheck(ctx context.Context, q dbReader, hash string, live, deleted int) error {
	var gotLive, gotDeleted int
	if err := q.QueryRowContext(ctx, `SELECT live_refs,deleted_refs FROM assets WHERE hash=?`, hash).Scan(&gotLive, &gotDeleted); err != nil {
		return err
	}
	if gotLive != live || gotDeleted != deleted {
		return fmt.Errorf("asset %s refs=(%d,%d), want (%d,%d)", hash, gotLive, gotDeleted, live, deleted)
	}
	return nil
}

func assetReferenceCounts(t *testing.T, q dbReader, hash string, live, deleted int) {
	t.Helper()
	if err := assetReferenceCheck(t.Context(), q, hash, live, deleted); err != nil {
		t.Fatal(err)
	}
}

func assetReferenceAudit(t *testing.T, s *store) {
	t.Helper()
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		for _, query := range []string{
			`CREATE TABLE asset_update_audit(hash TEXT NOT NULL)`,
			`CREATE TRIGGER audit_asset_update AFTER UPDATE ON assets BEGIN INSERT INTO asset_update_audit(hash) VALUES(NEW.hash); END`,
		} {
			if _, err := tx.ExecContext(t.Context(), query); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAssetReferencesIncremental(t *testing.T) {
	t.Parallel()
	s := writerOutboxStore(t)
	a, b, unrelated := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	assetReferenceAdd(t, s, a, b, unrelated)
	assetReferenceAudit(t, s)
	id := vocabID(BlockKind, 0)
	set := func(content Content) {
		t.Helper()
		writerOutboxChange(t, s, id, func(r *itemRecord) {
			block := r.Item.Data.(Block)
			block.Content = content
			r.Item.Data = block
		})
	}
	set(Content{Kind: Image, Asset: a, Caption: RichText{}})
	assetReferenceCounts(t, s.db, a, 1, 0)
	set(Content{Kind: Image, Asset: b, Caption: RichText{}})
	assetReferenceCounts(t, s.db, a, 0, 0)
	assetReferenceCounts(t, s.db, b, 1, 0)
	writerOutboxChange(t, s, id, func(r *itemRecord) { r.Alive, r.DeletedAt = false, 2 })
	assetReferenceCounts(t, s.db, b, 0, 1)
	set(Content{Kind: Image, Asset: a, Caption: RichText{}})
	assetReferenceCounts(t, s.db, a, 0, 1)
	assetReferenceCounts(t, s.db, b, 0, 0)
	writerOutboxChange(t, s, id, func(r *itemRecord) { r.Alive, r.DeletedAt = true, 0 })
	assetReferenceCounts(t, s.db, a, 1, 0)
	set(editText(Paragraph, "not an image"))
	assetReferenceCounts(t, s.db, a, 0, 0)

	// Items can predate restored metadata, and tombstone eviction releases only
	// that image's deleted reference. Both paths must work on a new connection.
	s.db.SetMaxIdleConns(0)
	late := strings.Repeat("d", 64)
	set(Content{Kind: Image, Asset: late, Caption: RichText{}})
	writerOutboxChange(t, s, id, func(r *itemRecord) { r.Alive, r.DeletedAt = false, 3 })
	assetReferenceAdd(t, s, late)
	assetReferenceCounts(t, s.db, late, 0, 1)
	if _, err := s.db.ExecContext(t.Context(), `DELETE FROM items WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	assetReferenceCounts(t, s.db, late, 0, 0)
	assetReferenceCounts(t, s.db, unrelated, 0, 0)
	var unrelatedWrites int
	if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM asset_update_audit WHERE hash=?`, unrelated).Scan(&unrelatedWrites); err != nil {
		t.Fatal(err)
	}
	if unrelatedWrites != 0 {
		t.Fatalf("unrelated asset rewritten %d times", unrelatedWrites)
	}
}

func TestAssetReferencesTypingWriterWork(t *testing.T) {
	t.Parallel()
	workByAssets := map[int]int64{}
	for _, extraAssets := range []int{0, 2048} {
		t.Run(fmt.Sprintf("unrelated-assets=%d", extraAssets), func(t *testing.T) {
			s := writerOutboxStore(t)
			if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
				// The same many-image, many-block notebook in both cases; only
				// the unrelated asset population changes.
				if _, err := tx.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS
                    (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
                    INSERT INTO assets(hash,mime,width,height,bytes,live_refs,deleted_refs,protected_until)
                    SELECT printf('%064x',i),'image/png',1,1,99,0,0,0 FROM n`, 64+extraAssets); err != nil {
					return err
				}
				for i := 0; i < 192; i++ {
					content := editText(Paragraph, "unchanged")
					if i < 64 {
						content = Content{Kind: Image, Asset: fmt.Sprintf("%064x", i+1), Caption: RichText{}}
					}
					item := Item{Ref: Ref{ID: vocabID(BlockKind, byte(i+2)), Inc: 1}, Ver: 1, V: map[GroupKey]Version{},
						Data: Block{Page: vocabID(PageKind, 0), Position: fmt.Sprint(i + 2), Content: content}}
					if err := putItem(t.Context(), tx, item); err != nil {
						return err
					}
				}
				return syncContentFiles(t.Context(), tx, 1)
			}); err != nil {
				t.Fatal(err)
			}
			assetReferenceAudit(t, s)
			var before, after int64
			if err := s.db.QueryRowContext(t.Context(), `SELECT total_changes()`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
				r, err := loadRecord(t.Context(), tx, vocabID(BlockKind, 0))
				if err != nil {
					return err
				}
				block := r.Item.Data.(Block)
				block.Content = editText(Paragraph, "hello!")
				r.Item.Data = block
				if err := putRecord(t.Context(), tx, r); err != nil {
					return err
				}
				// A same-asset caption edit is also a non-reference mutation.
				image, err := loadRecord(t.Context(), tx, vocabID(BlockKind, 2))
				if err != nil {
					return err
				}
				b := image.Item.Data.(Block)
				b.Content.Caption = RichText{{Text: "caption!", Marks: []Mark{}}}
				image.Item.Data = b
				if err := putRecord(t.Context(), tx, image); err != nil {
					return err
				}
				return syncContentFiles(t.Context(), tx, 2)
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRowContext(t.Context(), `SELECT total_changes()`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			var rewrites int
			if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM asset_update_audit`).Scan(&rewrites); err != nil {
				t.Fatal(err)
			}
			if rewrites != 0 {
				t.Fatalf("typing/caption edit rewrote %d assets", rewrites)
			}
			work := after - before
			if work <= 0 {
				t.Fatal("writer-work oracle did not observe the edit")
			}
			workByAssets[extraAssets] = work
		})
	}
	if len(workByAssets) == 2 && workByAssets[0] != workByAssets[2048] {
		t.Fatalf("writer row changes grew with assets: %d != %d", workByAssets[2048], workByAssets[0])
	}
}

func TestAssetReferencesSavepointAndTransactionRollback(t *testing.T) {
	t.Parallel()
	s := writerOutboxStore(t)
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	assetReferenceAdd(t, s, a, b)
	writerOutboxChange(t, s, vocabID(BlockKind, 0), func(r *itemRecord) {
		block := r.Item.Data.(Block)
		block.Content = Content{Kind: Image, Asset: a, Caption: RichText{}}
		r.Item.Data = block
	})
	assetReferenceAudit(t, s)
	rollback := errors.New("rollback asset edit")
	for _, savepoint := range []bool{true, false} {
		t.Run(fmt.Sprintf("savepoint=%t", savepoint), func(t *testing.T) {
			err := s.transaction(t.Context(), func(tx *sql.Tx) error {
				if savepoint {
					if _, err := tx.ExecContext(t.Context(), `SAVEPOINT image_edit`); err != nil {
						return err
					}
				}
				r, err := loadRecord(t.Context(), tx, vocabID(BlockKind, 0))
				if err != nil {
					return err
				}
				block := r.Item.Data.(Block)
				block.Content.Asset = b
				r.Item.Data = block
				r.Alive, r.DeletedAt = false, 2
				if err := putRecord(t.Context(), tx, r); err != nil {
					return err
				}
				if err := assetReferenceCheck(t.Context(), tx, a, 0, 0); err != nil {
					return err
				}
				if err := assetReferenceCheck(t.Context(), tx, b, 0, 1); err != nil {
					return err
				}
				if !savepoint {
					return rollback
				}
				if _, err := tx.ExecContext(t.Context(), `ROLLBACK TO image_edit`); err != nil {
					return err
				}
				if err := assetReferenceCheck(t.Context(), tx, a, 1, 0); err != nil {
					return err
				}
				if err := assetReferenceCheck(t.Context(), tx, b, 0, 0); err != nil {
					return err
				}
				_, err = tx.ExecContext(t.Context(), `RELEASE image_edit`)
				return err
			})
			if savepoint && err != nil || !savepoint && !errors.Is(err, rollback) {
				t.Fatal(err)
			}
			assetReferenceCounts(t, s.db, a, 1, 0)
			assetReferenceCounts(t, s.db, b, 0, 0)
			var writes int
			if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM asset_update_audit`).Scan(&writes); err != nil {
				t.Fatal(err)
			}
			if writes != 0 {
				t.Fatalf("rolled-back asset writes survived: %d", writes)
			}
		})
	}
}

func TestAssetReferencesUndoRedo(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	assetReferenceAdd(t, f.s, a, b)
	id := vocabID(BlockKind, 1)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	requireOutcomes(t, f.execute("a", &CreateBlockOp{CreateFields: CreateFields{Ref: "image", ProposedID: &id},
		Place: Placement{Page: editTarget(page)}, Content: Content{Kind: Image, Asset: a, Caption: RichText{}}}), "applied")
	assetReferenceCounts(t, f.s.db, a, 1, 0)
	ref := f.record(id).Item.Ref
	version, err := f.record(id).Item.GroupVersion(ContentGroup)
	if err != nil {
		t.Fatal(err)
	}
	requireOutcomes(t, f.execute("a", &SetBlockOp{Block: editTarget(ref), BaseV: &version, Content: Content{Kind: Image, Asset: b, Caption: RichText{}}}), "applied")
	assetReferenceCounts(t, f.s.db, a, 0, 0)
	assetReferenceCounts(t, f.s.db, b, 1, 0)
	requireOutcomes(t, f.execute("a", &DeleteBlocksOp{Blocks: []Target{editTarget(ref)}}), "applied")
	assetReferenceCounts(t, f.s.db, b, 0, 1)
	for _, step := range []struct {
		name            string
		redo            bool
		aLive, aDeleted int
		bLive, bDeleted int
	}{
		{name: "undo deletion revives", bLive: 1},
		{name: "undo set", aLive: 1},
		{name: "undo creation", aDeleted: 1},
		{name: "redo creation revives", redo: true, aLive: 1},
		{name: "redo set", redo: true, bLive: 1},
		{name: "redo deletion", redo: true, bDeleted: 1},
	} {
		if got := f.undo("a", step.redo); got.Applied != 1 {
			t.Fatalf("%s: undo/redo=%+v", step.name, got)
		}
		assetReferenceCounts(t, f.s.db, a, step.aLive, step.aDeleted)
		assetReferenceCounts(t, f.s.db, b, step.bLive, step.bDeleted)
	}
}

func TestAssetReferencesReconcileOnOpen(t *testing.T) {
	t.Parallel()
	s := writerOutboxStore(t)
	a, orphan, stable := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	assetReferenceAdd(t, s, a, orphan, stable)
	// Emulate a version-1 database produced before incremental tracking.
	if err := s.transaction(t.Context(), func(tx *sql.Tx) error {
		for _, name := range []string{"notebook_image_insert", "notebook_image_update", "notebook_image_delete", "notebook_asset_insert"} {
			if _, err := tx.ExecContext(t.Context(), `DROP TRIGGER `+name); err != nil {
				return err
			}
		}
		for i, alive := range []bool{true, false} {
			r := itemRecord{Item: Item{Ref: Ref{ID: vocabID(BlockKind, byte(2+i)), Inc: 1}, Ver: 1, V: map[GroupKey]Version{},
				Data: Block{Page: vocabID(PageKind, 0), Position: fmt.Sprint(i + 2), Content: Content{Kind: Image, Asset: a, Caption: RichText{}}}}, Alive: alive}
			if !alive {
				r.DeletedAt = 2
			}
			if err := putRecord(t.Context(), tx, r); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(t.Context(), `UPDATE assets SET live_refs=99,deleted_refs=88 WHERE hash IN (?,?)`, a, orphan)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assetReferenceAudit(t, s)
	for open := 0; open < 2; open++ {
		if err := s.close(); err != nil {
			t.Fatal(err)
		}
		reopened, fresh, err := openStore(s.path)
		if err != nil {
			t.Fatal(err)
		}
		// writerOutboxStore owns the cleanup pointer.
		s.db = reopened.db
		if fresh {
			t.Fatal("reconciliation replaced the database")
		}
		assetReferenceCounts(t, s.db, a, 1, 1)
		assetReferenceCounts(t, s.db, orphan, 0, 0)
		assetReferenceCounts(t, s.db, stable, 0, 0)
		var writes int
		if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM asset_update_audit`).Scan(&writes); err != nil {
			t.Fatal(err)
		}
		want := 2
		if open > 0 {
			want = 0
		}
		if writes != want {
			t.Fatalf("open %d rewrote %d assets, want %d", open, writes, want)
		}
		var version, protected int
		if err := s.db.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(t.Context(), `SELECT protected_until FROM assets WHERE hash=?`, a).Scan(&protected); err != nil {
			t.Fatal(err)
		}
		if version != 1 || protected != 1234 {
			t.Fatalf("reconcile changed schema/protection: version=%d protection=%d", version, protected)
		}
		if _, err := s.db.ExecContext(t.Context(), `DELETE FROM asset_update_audit`); err != nil {
			t.Fatal(err)
		}
	}
	writerOutboxChange(t, s, vocabID(BlockKind, 2), func(r *itemRecord) { r.Alive, r.DeletedAt = false, 3 })
	assetReferenceCounts(t, s.db, a, 0, 2)
}
