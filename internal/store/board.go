package store

import (
	"context"
	"database/sql"
	"errors"
)

// Board revisions (docs/spec/board-TECH.md 5.2, 5.3): a board records its
// revision boundaries in the outbox of its board.db and core imports them
// into the revision store. board_imports maps each imported outbox entry
// (and its operation id) to its revision; board_rev_stats keeps the element
// counts the History panel and list_revisions report. Both rows go with
// their revision.

// ElementCounts are the elements a board revision added, changed and
// removed compared with the revision before it.
type ElementCounts struct {
	Added   int `json:"added"`
	Changed int `json:"changed"`
	Removed int `json:"removed"`
}

func addBoardRevisions(ctx context.Context, x executor) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS board_imports (
 flat TEXT NOT NULL,
 store TEXT NOT NULL,
 outbox_seq INTEGER NOT NULL,
 op_id TEXT NOT NULL DEFAULT '',
 revision INTEGER NOT NULL,
 PRIMARY KEY (flat, store, outbox_seq),
 FOREIGN KEY (flat, revision) REFERENCES revisions(flat, number) ON DELETE CASCADE ON UPDATE CASCADE
 )`,
		`CREATE INDEX IF NOT EXISTS board_imports_revision ON board_imports(flat, revision)`,
		`CREATE TABLE IF NOT EXISTS board_rev_stats (
 flat TEXT NOT NULL,
 number INTEGER NOT NULL,
 added INTEGER NOT NULL,
 changed INTEGER NOT NULL,
 removed INTEGER NOT NULL,
 PRIMARY KEY (flat, number),
 FOREIGN KEY (flat, number) REFERENCES revisions(flat, number) ON DELETE CASCADE ON UPDATE CASCADE
 )`,
	} {
		if _, err := x.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// PutBoardImport records that outbox entry seq of board.db outbox became
// revision n.
func (c *ContentTx) PutBoardImport(ctx context.Context, outbox string, seq int64, op string, n int) error {
	_, err := c.tx.ExecContext(ctx, `INSERT OR REPLACE INTO board_imports(flat,store,outbox_seq,op_id,revision) VALUES(?,?,?,?,?)`, c.flat, outbox, seq, op, n)
	return err
}

// PutBoardRevStats stores the element counts of revision n.
func (c *ContentTx) PutBoardRevStats(ctx context.Context, n int, counts ElementCounts) error {
	return putBoardRevStats(ctx, c.tx, c.flat, n, counts)
}

// PutBoardRevStats stores the element counts of revision n of flat.
func (s *Store) PutBoardRevStats(ctx context.Context, flat string, n int, counts ElementCounts) error {
	return putBoardRevStats(ctx, s.db, flat, n, counts)
}

func putBoardRevStats(ctx context.Context, x executor, flat string, n int, c ElementCounts) error {
	_, err := x.ExecContext(ctx, `INSERT OR REPLACE INTO board_rev_stats(flat,number,added,changed,removed) VALUES(?,?,?,?,?)`, flat, n, c.Added, c.Changed, c.Removed)
	return err
}

// BoardRevStats returns the element counts stored for revisions of flat
// numbered from lo to hi; a revision without a row is absent.
func (s *Store) BoardRevStats(ctx context.Context, flat string, lo, hi int) (map[int]ElementCounts, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT number,added,changed,removed FROM board_rev_stats WHERE flat=? AND number BETWEEN ? AND ?`, flat, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]ElementCounts{}
	for rows.Next() {
		var n int
		var c ElementCounts
		if err := rows.Scan(&n, &c.Added, &c.Changed, &c.Removed); err != nil {
			return nil, err
		}
		out[n] = c
	}
	return out, rows.Err()
}

// BoardImportRevision returns the revision that outbox entry seq of board.db
// outbox became; ok is false when no retained revision records it.
func (s *Store) BoardImportRevision(ctx context.Context, flat, outbox string, seq int64) (n int, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT revision FROM board_imports WHERE flat=? AND store=? AND outbox_seq=?`, flat, outbox, seq).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}

// RevisionAssetPath returns the path of a retained revision of flat that
// holds an asset file (assets/<hash>.<ext>) with this hash.
func (s *Store) RevisionAssetPath(ctx context.Context, flat, hash string) (string, bool, error) {
	var p string
	err := s.db.QueryRowContext(ctx, `SELECT path FROM revision_files WHERE flat=? AND hash=? AND path LIKE 'assets/%' LIMIT 1`, flat, hash).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return p, err == nil, err
}

// CountRevisions returns the number of retained revisions of flat.
func (s *Store) CountRevisions(ctx context.Context, flat string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM revisions WHERE flat=?`, flat).Scan(&n)
	return n, err
}
