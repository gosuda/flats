package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Portal relay listing of a public flat. ListingDefault (no row) follows the
// host's portal.hide setting; the others override it for one flat. Hiding a
// flat is not access control: anyone with its URL can still open it.
const (
	ListingDefault = "default"
	ListingHidden  = "hidden"
	ListingListed  = "listed"
)

// ValidListing reports whether v is a known listing mode.
func ValidListing(v string) bool {
	return v == ListingDefault || v == ListingHidden || v == ListingListed
}

// PortalListing returns the listing mode of flat, ListingDefault without a row.
func (s *Store) PortalListing(ctx context.Context, flat string) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, `SELECT mode FROM portal_listing WHERE flat=?`, flat).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return ListingDefault, nil
	}
	if err != nil {
		return "", err
	}
	if mode != ListingHidden && mode != ListingListed {
		return "", fmt.Errorf("invalid stored portal listing %q", mode)
	}
	return mode, nil
}

// SetPortalListing stores the listing mode of flat; ListingDefault removes
// the override.
func (s *Store) SetPortalListing(ctx context.Context, flat, mode string, now time.Time) error {
	switch mode {
	case ListingDefault:
		_, err := s.db.ExecContext(ctx, `DELETE FROM portal_listing WHERE flat=?`, flat)
		return err
	case ListingHidden, ListingListed:
		_, err := s.db.ExecContext(ctx, `INSERT INTO portal_listing(flat,mode,updated_at) VALUES(?,?,?) ON CONFLICT(flat) DO UPDATE SET mode=excluded.mode,updated_at=excluded.updated_at`, flat, mode, unix(now))
		return err
	}
	return fmt.Errorf("invalid portal listing %q", mode)
}

func addPortalListing(ctx context.Context, x executor) error {
	_, err := x.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS portal_listing (
 flat TEXT PRIMARY KEY REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
 mode TEXT NOT NULL CHECK(mode IN ('hidden','listed')),
 updated_at INTEGER NOT NULL
 );`)
	return err
}
