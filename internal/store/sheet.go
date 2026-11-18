package store

import (
	"context"
	"database/sql"
	"errors"
)

// SheetTabChange describes one tab changed by a sheet revision. Change is
// added, removed, or changed; an absent tab has an empty name.
type SheetTabChange struct {
	Tab        uint64 `json:"tab"`
	Change     string `json:"change"`
	NameBefore string `json:"name_before"`
	NameAfter  string `json:"name_after"`
}

func addSheetRevisions(ctx context.Context, x executor) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS sheet_imports (
 flat TEXT NOT NULL,
 store TEXT NOT NULL,
 outbox_seq INTEGER NOT NULL,
 op_id TEXT NOT NULL DEFAULT '',
 revision INTEGER NOT NULL,
 calc_date TEXT NOT NULL,
 PRIMARY KEY (flat, store, outbox_seq),
 FOREIGN KEY (flat, revision) REFERENCES revisions(flat, number) ON DELETE CASCADE ON UPDATE CASCADE
 )`,
		`CREATE UNIQUE INDEX IF NOT EXISTS sheet_imports_revision ON sheet_imports(flat, revision)`,
		`CREATE TABLE IF NOT EXISTS sheet_rev_tabs (
 flat TEXT NOT NULL,
 revision INTEGER NOT NULL,
 tab INTEGER NOT NULL,
 change TEXT NOT NULL CHECK (change IN ('added', 'removed', 'changed')),
 name_before TEXT NOT NULL,
 name_after TEXT NOT NULL,
 PRIMARY KEY (flat, revision, tab),
 FOREIGN KEY (flat, revision) REFERENCES revisions(flat, number) ON DELETE CASCADE ON UPDATE CASCADE
 )`,
	} {
		if _, err := x.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// PutSheetImport records that outbox entry seq of sheet store became
// revision n, closed with calculation date calcDate, in the transaction
// that imports that revision. A revision has one import row.
func (c *ContentTx) PutSheetImport(ctx context.Context, outbox string, seq int64, op string, n int, calcDate string) error {
	_, err := c.tx.ExecContext(ctx, `INSERT INTO sheet_imports(flat,store,outbox_seq,op_id,revision,calc_date) VALUES(?,?,?,?,?,?)`, c.flat, outbox, seq, op, n, calcDate)
	return err
}

// PutSheetRevTabs replaces the tab summaries of revision n.
func (c *ContentTx) PutSheetRevTabs(ctx context.Context, n int, tabs []SheetTabChange) error {
	if _, err := c.tx.ExecContext(ctx, `DELETE FROM sheet_rev_tabs WHERE flat=? AND revision=?`, c.flat, n); err != nil {
		return err
	}
	for _, t := range tabs {
		if _, err := c.tx.ExecContext(ctx, `INSERT INTO sheet_rev_tabs(flat,revision,tab,change,name_before,name_after) VALUES(?,?,?,?,?,?)`, c.flat, n, int64(t.Tab), t.Change, t.NameBefore, t.NameAfter); err != nil {
			return err
		}
	}
	return nil
}

// SheetRevTabs returns summaries for revisions lo through hi, each in
// tab-id order. Revisions without summaries are absent from the map.
func (s *Store) SheetRevTabs(ctx context.Context, flat string, lo, hi int) (map[int][]SheetTabChange, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT revision,tab,change,name_before,name_after FROM sheet_rev_tabs WHERE flat=? AND revision BETWEEN ? AND ? ORDER BY revision,tab`, flat, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int][]SheetTabChange{}
	for rows.Next() {
		var n int
		var tab int64
		var t SheetTabChange
		if err := rows.Scan(&n, &tab, &t.Change, &t.NameBefore, &t.NameAfter); err != nil {
			return nil, err
		}
		t.Tab = uint64(tab)
		out[n] = append(out[n], t)
	}
	return out, rows.Err()
}

// SheetImportRevision returns the retained revision for this outbox entry;
// ok is false when the entry was never imported or its revision was pruned.
func (s *Store) SheetImportRevision(ctx context.Context, flat, outbox string, seq int64) (n int, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT revision FROM sheet_imports WHERE flat=? AND store=? AND outbox_seq=?`, flat, outbox, seq).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}

// SheetImportByOp returns the retained revision of the outbox entry of this
// store that carries the operation id op; ok is false when no such entry was
// imported or its revision was pruned.
func (s *Store) SheetImportByOp(ctx context.Context, flat, outbox, op string) (n int, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT revision FROM sheet_imports WHERE flat=? AND store=? AND op_id=?`, flat, outbox, op).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}

// SheetRevisionDate returns the calculation date revision n closed with;
// ok is false when the revision has no import row.
func (s *Store) SheetRevisionDate(ctx context.Context, flat string, n int) (date string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT calc_date FROM sheet_imports WHERE flat=? AND revision=?`, flat, n).Scan(&date)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return date, err == nil, err
}
