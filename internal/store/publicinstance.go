package store

import (
	"context"
	"time"
)

func addPublicInstance(ctx context.Context, x executor) error {
	_, err := x.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS public_instance (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
  updated_at INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO public_instance (id) VALUES (1);`)
	return err
}

// PublicInstanceRow is the stored public instance settings.
type PublicInstanceRow struct {
	Enabled   bool
	UpdatedAt time.Time // zero while the row was never changed
}

// GetPublicInstance returns the stored settings.
func (s *Store) GetPublicInstance(ctx context.Context) (PublicInstanceRow, error) {
	return getPublicInstance(ctx, s.db)
}

func getPublicInstance(ctx context.Context, x executor) (PublicInstanceRow, error) {
	var r PublicInstanceRow
	var at int64
	if err := x.QueryRowContext(ctx, `SELECT enabled, updated_at FROM public_instance WHERE id=1`).Scan(&r.Enabled, &at); err != nil {
		return PublicInstanceRow{}, err
	}
	if at != 0 {
		r.UpdatedAt = fromUnix(at)
	}
	return r, nil
}

// UpdatePublicInstance reads and changes the settings under the write lock,
// committing the row and line to the Experiments change log together. The
// caller supplies a line starting with "public instance:". It returns the
// committed row and whether it wrote. Unchanged settings write nothing;
// an error rolls back and returns the stored row, with change errors as is.
func (s *Store) UpdatePublicInstance(ctx context.Context, now time.Time,
	change func(PublicInstanceRow) (PublicInstanceRow, string, error)) (PublicInstanceRow, bool, error) {
	var stored, next PublicInstanceRow
	wrote, err := s.immediate(ctx, func(x executor) (bool, error) {
		var err error
		stored, err = getPublicInstance(ctx, x)
		if err != nil {
			return false, err
		}
		var line string
		next, line, err = change(stored)
		if err != nil || next.Enabled == stored.Enabled {
			return false, err
		}
		next.UpdatedAt = fromUnix(unix(now))
		if _, err := x.ExecContext(ctx, `UPDATE public_instance SET enabled=?, updated_at=? WHERE id=1`, next.Enabled, unix(now)); err != nil {
			return false, err
		}
		_, err = x.ExecContext(ctx, `INSERT INTO autopilot_changes (at, line) VALUES (?, ?)`, unix(now), line)
		return err == nil, err
	})
	if err != nil || !wrote {
		return stored, false, err
	}
	return next, true, nil
}
