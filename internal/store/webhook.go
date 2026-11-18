package store

import "context"

// addEventWebhook is the migration of docs/spec/event-webhook.md, rule 28. It lets host_keys hold the two webhook
// keys, gives an approval the ID of its flat, and adds the indexes that the export reads by.
func addEventWebhook(ctx context.Context, x executor) error {
	// host_keys has a CHECK on its ids, so a new id needs a new table. The rows are kept as they are.
	if _, err := x.ExecContext(ctx, `
CREATE TABLE host_keys_v26 (
  id TEXT PRIMARY KEY CHECK (id IN ('tailscale', 'zrok', 'netbird', 'webhook-url', 'webhook-secret')),
  nonce BLOB,
  ciphertext BLOB,
  source TEXT NOT NULL CHECK (source IN ('console', 'cli', 'flag', 'env', 'config')),
  updated_at INTEGER NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  CHECK ((nonce IS NULL) = (ciphertext IS NULL))
);
INSERT INTO host_keys_v26 (id, nonce, ciphertext, source, updated_at, last_error)
  SELECT id, nonce, ciphertext, source, updated_at, last_error FROM host_keys;
DROP TABLE host_keys;
ALTER TABLE host_keys_v26 RENAME TO host_keys;`); err != nil {
		return err
	}
	// An approval that outlives its flat (a delete) still says which flat it was about. A flat without an ID, or one that
	// is gone already, leaves the column empty.
	if err := addColumn(ctx, x, "approvals", "flat_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	_, err := x.ExecContext(ctx, `
UPDATE approvals SET flat_id = COALESCE((SELECT uid FROM flats WHERE flats.slug = approvals.flat), '') WHERE flat_id = '';
CREATE INDEX IF NOT EXISTS approvals_requested_at ON approvals(requested_at);
CREATE INDEX IF NOT EXISTS approvals_decided_at ON approvals(decided_at);
CREATE INDEX IF NOT EXISTS audit_events_at ON audit_events(at, id);`)
	return err
}
