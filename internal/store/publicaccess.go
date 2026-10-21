package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gosuda/flats/internal/contenttype"
)

// Public access levels of a Public flat (docs/spec/board-public-edit.md).
// A Private flat has none; only a board can be AccessEdit.
const (
	AccessNone = ""
	AccessView = "view"
	AccessEdit = "edit"
)

// addPublicAccess adds the public access level and its change counter to
// flats, the safe point to content_state and guest credits to revisions
// (board-public-edit-TECH 2.1, 5.2). Every flat that is Public now starts at
// view.
func addPublicAccess(ctx context.Context, x executor) error {
	for _, c := range []struct{ table, col, def string }{
		{"flats", "public_access", "TEXT NOT NULL DEFAULT ''"},
		{"flats", "access_seq", "INTEGER NOT NULL DEFAULT 0"},
		{"content_state", "safe_point_rev", "INTEGER"},
		{"revisions", "guest_authors", "TEXT NOT NULL DEFAULT '[]'"},
		{"revisions", "guests_more", "INTEGER NOT NULL DEFAULT 0"},
		{"revisions", "guest_only", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := addColumn(ctx, x, c.table, c.col, c.def); err != nil {
			return err
		}
	}
	_, err := x.ExecContext(ctx, `UPDATE flats SET public_access='view' WHERE visibility<>'private' AND public_access=''`)
	return err
}

// validAccess reports whether access is a valid level for visibility.
func validAccess(vis Visibility, access string) bool {
	if vis.Canonical() == Private {
		return access == AccessNone
	}
	return access == AccessView || access == AccessEdit
}

// SetPublicAccess changes the public access level of a Public flat from
// from to to, and records ev in the same transaction, when the flat is
// still at from. It never changes visibility; startup uses it to leave a
// level the current settings no longer allow.
func (s *Store) SetPublicAccess(ctx context.Context, slug, from, to string, ev Event) error {
	if to != AccessView && to != AccessEdit {
		return fmt.Errorf("public access %q", to)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE flats SET public_access=?,access_seq=access_seq+1 WHERE slug=? AND public_access=? AND visibility<>'private'`, to, slug, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("public access changed concurrently")
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	var data any
	if len(ev.Data) > 0 {
		data = string(ev.Data)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(flat,at,level,kind,message,data) VALUES(?,?,?,?,?,?)`, slug, unix(ev.Time), ev.Level, ev.Kind, ev.Message, data); err != nil {
		return err
	}
	return tx.Commit()
}

// FlatsWithAccess lists the slugs of flats at public access level access.
func (s *Store) FlatsWithAccess(ctx context.Context, access string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slug FROM flats WHERE public_access=? ORDER BY slug`, access)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		out = append(out, slug)
	}
	return out, rows.Err()
}

// SetSafePoint stores the safe point revision of the flat (0 clears it).
func (c *ContentTx) SetSafePoint(ctx context.Context, n int) error {
	var v any
	if n > 0 {
		v = n
	}
	_, err := c.tx.ExecContext(ctx, `UPDATE content_state SET safe_point_rev=? WHERE flat=?`, v, c.flat)
	return err
}

// RetainCandidate is a revision retention may delete: neither pinned nor
// held by a rollback in progress.
type RetainCandidate struct {
	Number    int
	GuestOnly bool
}

// RetainCandidates lists the revisions retention may delete, oldest first.
func (c *ContentTx) RetainCandidates(ctx context.Context) ([]RetainCandidate, error) {
	rows, err := c.tx.QueryContext(ctx, `SELECT number,guest_only FROM revisions r WHERE flat=? AND pinned=0 AND NOT EXISTS(SELECT 1 FROM content_holds h WHERE h.flat=r.flat AND h.number=r.number) ORDER BY number`, c.flat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetainCandidate
	for rows.Next() {
		var rc RetainCandidate
		var guestOnly int
		if err := rows.Scan(&rc.Number, &guestOnly); err != nil {
			return nil, err
		}
		rc.GuestOnly = guestOnly == 1
		out = append(out, rc)
	}
	return out, rows.Err()
}

// isBoardTx reports whether flat's content state marks it as a board.
func isBoardTx(ctx context.Context, tx *sql.Tx, flat string) (bool, error) {
	var typ string
	err := tx.QueryRowContext(ctx, `SELECT type FROM content_state WHERE flat=?`, flat).Scan(&typ)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return typ == contenttype.Board, err
}
