package store

import (
	"context"
	"database/sql"
	"errors"
)

// NotebookPageChange describes one page changed by a notebook revision.
// Change is added, removed, or changed; an absent page has an empty title.
type NotebookPageChange struct {
	Page        string `json:"page"`
	Change      string `json:"change"`
	TitleBefore string `json:"title_before"`
	TitleAfter  string `json:"title_after"`
}

func addNotebookRevisions(ctx context.Context, x executor) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS notebook_imports (
 flat TEXT NOT NULL,
 store TEXT NOT NULL,
 outbox_seq INTEGER NOT NULL,
 op_id TEXT NOT NULL DEFAULT '',
 revision INTEGER NOT NULL,
 PRIMARY KEY (flat, store, outbox_seq),
 FOREIGN KEY (flat, revision) REFERENCES revisions(flat, number) ON DELETE CASCADE ON UPDATE CASCADE
 )`,
		`CREATE INDEX IF NOT EXISTS notebook_imports_revision ON notebook_imports(flat, revision)`,
		`CREATE TABLE IF NOT EXISTS notebook_rev_pages (
 flat TEXT NOT NULL,
 revision INTEGER NOT NULL,
 page TEXT NOT NULL,
 change TEXT NOT NULL CHECK (change IN ('added', 'removed', 'changed')),
 title_before TEXT NOT NULL,
 title_after TEXT NOT NULL,
 PRIMARY KEY (flat, revision, page),
 FOREIGN KEY (flat, revision) REFERENCES revisions(flat, number) ON DELETE CASCADE ON UPDATE CASCADE
 )`,
	} {
		if _, err := x.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// PutNotebookImport records that outbox entry seq of notebook store became
// revision n, in the transaction that imports that revision.
func (c *ContentTx) PutNotebookImport(ctx context.Context, outbox string, seq int64, op string, n int) error {
	_, err := c.tx.ExecContext(ctx, `INSERT OR REPLACE INTO notebook_imports(flat,store,outbox_seq,op_id,revision) VALUES(?,?,?,?,?)`, c.flat, outbox, seq, op, n)
	return err
}

// PutNotebookRevPages replaces the page summaries of revision n.
func (c *ContentTx) PutNotebookRevPages(ctx context.Context, n int, pages []NotebookPageChange) error {
	if _, err := c.tx.ExecContext(ctx, `DELETE FROM notebook_rev_pages WHERE flat=? AND revision=?`, c.flat, n); err != nil {
		return err
	}
	for _, p := range pages {
		if _, err := c.tx.ExecContext(ctx, `INSERT INTO notebook_rev_pages(flat,revision,page,change,title_before,title_after) VALUES(?,?,?,?,?,?)`, c.flat, n, p.Page, p.Change, p.TitleBefore, p.TitleAfter); err != nil {
			return err
		}
	}
	return nil
}

// PutNotebookRevPages replaces reconstructed page summaries of revision n of flat.
func (s *Store) PutNotebookRevPages(ctx context.Context, flat string, n int, pages []NotebookPageChange) error {
	return s.ContentTx(ctx, flat, func(tx *ContentTx) error {
		return tx.PutNotebookRevPages(ctx, n, pages)
	})
}

// NotebookRevPages returns summaries for revisions lo through hi, each in
// page-id order. Revisions without summaries are absent from the map.
func (s *Store) NotebookRevPages(ctx context.Context, flat string, lo, hi int) (map[int][]NotebookPageChange, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT revision,page,change,title_before,title_after FROM notebook_rev_pages WHERE flat=? AND revision BETWEEN ? AND ? ORDER BY revision,page`, flat, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int][]NotebookPageChange{}
	for rows.Next() {
		var n int
		var p NotebookPageChange
		if err := rows.Scan(&n, &p.Page, &p.Change, &p.TitleBefore, &p.TitleAfter); err != nil {
			return nil, err
		}
		out[n] = append(out[n], p)
	}
	return out, rows.Err()
}

// NotebookImportRevision returns the retained revision for this outbox entry;
// ok is false when the entry was never imported or its revision was pruned.
func (s *Store) NotebookImportRevision(ctx context.Context, flat, outbox string, seq int64) (n int, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT revision FROM notebook_imports WHERE flat=? AND store=? AND outbox_seq=?`, flat, outbox, seq).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}
