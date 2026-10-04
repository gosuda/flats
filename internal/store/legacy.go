package store

import (
	"context"
	"fmt"
	"slices"
)

// Legacy is the host state that hosts before config.json kept in the
// database. The legacy migration reads it once, before anything is written.
type Legacy struct {
	// Settings are the rows of the settings table.
	Settings map[string]string
	// PrivateUpgradePending reports that, once migrated, some flat still
	// needs the legacy Private Tailscale choice (legacy_private_upgrade).
	PrivateUpgradePending bool
}

// ReadLegacy reads the legacy host state of the database at path without
// writing to it. A missing or empty database has none.
func ReadLegacy(path string) (Legacy, error) {
	out := Legacy{Settings: map[string]string{}}
	info, err := Inspect(path)
	if err != nil || !info.Exists || info.New {
		return out, err
	}
	ctx := context.Background()
	db, err := openReadOnly(path)
	if err != nil {
		return out, err
	}
	defer db.Close()
	tables, err := tableNames(ctx, db)
	if err != nil {
		return out, err
	}
	if slices.Contains(tables, "settings") {
		rows, err := db.QueryContext(ctx, `SELECT key,value FROM settings`)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				rows.Close()
				return out, err
			}
			out.Settings[k] = v
		}
		if err := rows.Close(); err != nil {
			return out, err
		}
	}
	// Mirror legacyBaseline: a row still pending, or, below version 2, a
	// flat the baseline is about to mark pending.
	var q string
	switch {
	case slices.Contains(tables, "legacy_private_upgrade") && info.Version < 2:
		q = `SELECT EXISTS(SELECT 1 FROM legacy_private_upgrade WHERE pending=1) OR EXISTS(SELECT 1 FROM flats WHERE slug NOT IN (SELECT flat FROM legacy_private_upgrade))`
	case slices.Contains(tables, "legacy_private_upgrade"):
		q = `SELECT EXISTS(SELECT 1 FROM legacy_private_upgrade WHERE pending=1)`
	case info.Version < 2:
		q = `SELECT EXISTS(SELECT 1 FROM flats)`
	default:
		return out, nil
	}
	if err := db.QueryRowContext(ctx, q).Scan(&out.PrivateUpgradePending); err != nil {
		return out, fmt.Errorf("read legacy private upgrade: %w", err)
	}
	return out, nil
}

// HasSecrets reports whether any flat has a stored secret.
func (s *Store) HasSecrets(ctx context.Context) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM secrets)`).Scan(&ok)
	return ok, err
}
