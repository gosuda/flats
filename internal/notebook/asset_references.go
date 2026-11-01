package notebook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Reference changes belong to the item transaction, including savepoint rollback
// and undo. Durable triggers also cover replacement writer connections. Guards
// exclude typing, caption edits, moves and version-only updates before touching
// assets; each changed image reference uses the assets primary key.
var assetReferenceSchema = []string{
	`CREATE INDEX IF NOT EXISTS notebook_image_assets ON items(json_extract(data,'$.content.asset'),alive)
	 WHERE kind='block' AND json_extract(data,'$.content.kind')='image'`,
	`CREATE TRIGGER IF NOT EXISTS notebook_image_insert AFTER INSERT ON items
	 WHEN NEW.kind='block' AND json_extract(NEW.data,'$.content.kind')='image'
	 BEGIN
	  UPDATE assets SET live_refs=live_refs+NEW.alive,deleted_refs=deleted_refs+1-NEW.alive
	  WHERE hash=json_extract(NEW.data,'$.content.asset');
	 END`,
	`CREATE TRIGGER IF NOT EXISTS notebook_image_delete AFTER DELETE ON items
	 WHEN OLD.kind='block' AND json_extract(OLD.data,'$.content.kind')='image'
	 BEGIN
	  UPDATE assets SET live_refs=live_refs-OLD.alive,deleted_refs=deleted_refs-1+OLD.alive
	  WHERE hash=json_extract(OLD.data,'$.content.asset');
	 END`,
	`CREATE TRIGGER IF NOT EXISTS notebook_image_update AFTER UPDATE OF kind,data,alive ON items
	 WHEN (CASE WHEN OLD.kind='block' AND json_extract(OLD.data,'$.content.kind')='image'
	             THEN json_extract(OLD.data,'$.content.asset') END)
	      IS NOT
	      (CASE WHEN NEW.kind='block' AND json_extract(NEW.data,'$.content.kind')='image'
	             THEN json_extract(NEW.data,'$.content.asset') END)
	   OR (OLD.alive IS NOT NEW.alive AND OLD.kind='block' AND json_extract(OLD.data,'$.content.kind')='image')
	 BEGIN
	  UPDATE assets SET live_refs=live_refs-OLD.alive,deleted_refs=deleted_refs-1+OLD.alive
	  WHERE OLD.kind='block' AND json_extract(OLD.data,'$.content.kind')='image'
	    AND hash=json_extract(OLD.data,'$.content.asset');
	  UPDATE assets SET live_refs=live_refs+NEW.alive,deleted_refs=deleted_refs+1-NEW.alive
	  WHERE NEW.kind='block' AND json_extract(NEW.data,'$.content.kind')='image'
	    AND hash=json_extract(NEW.data,'$.content.asset');
	 END`,
	// An upload normally precedes its items. Also handle metadata restored after
	// retained image items, using the image-reference index rather than a scan.
	`CREATE TRIGGER IF NOT EXISTS notebook_asset_insert AFTER INSERT ON assets
	 BEGIN
	  UPDATE assets SET live_refs=c.live_refs,deleted_refs=c.deleted_refs
	  FROM (SELECT coalesce(sum(alive),0) AS live_refs,coalesce(sum(1-alive),0) AS deleted_refs
	        FROM items WHERE kind='block' AND json_extract(data,'$.content.kind')='image'
	          AND json_extract(data,'$.content.asset')=NEW.hash) AS c
	  WHERE assets.hash=NEW.hash
	    AND (assets.live_refs IS NOT c.live_refs OR assets.deleted_refs IS NOT c.deleted_refs);
	 END`,
}

func initializeAssetReferences(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("notebook.db asset references begin: %w", err)
	}
	initialize := func() error {
		for _, query := range assetReferenceSchema {
			if _, err := tx.ExecContext(ctx, query); err != nil {
				return err
			}
		}
		// Reconcile older databases once per open, not once per edit. Group the
		// image references once and rewrite only mismatches, including stale
		// nonzero counts on assets which no longer have any image references.
		_, err := tx.ExecContext(ctx, `WITH counts AS MATERIALIZED (
		 SELECT json_extract(data,'$.content.asset') AS hash,sum(alive) AS live_refs,sum(1-alive) AS deleted_refs
		 FROM items WHERE kind='block' AND json_extract(data,'$.content.kind')='image'
		 GROUP BY json_extract(data,'$.content.asset')
		), expected AS MATERIALIZED (
		 SELECT a.hash,coalesce(c.live_refs,0) AS live_refs,coalesce(c.deleted_refs,0) AS deleted_refs
		 FROM assets a LEFT JOIN counts c ON c.hash=a.hash
		)
		UPDATE assets SET live_refs=e.live_refs,deleted_refs=e.deleted_refs FROM expected e
		WHERE assets.hash=e.hash AND (assets.live_refs IS NOT e.live_refs OR assets.deleted_refs IS NOT e.deleted_refs)`)
		return err
	}
	if err := initialize(); err != nil {
		return errors.Join(fmt.Errorf("notebook.db asset references: %w", err), tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("notebook.db asset references commit: %w", err)
	}
	return nil
}
