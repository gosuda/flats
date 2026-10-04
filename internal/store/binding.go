package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrHostBindingMismatch is returned when the database is already bound to
// a different host instance.
var ErrHostBindingMismatch = errors.New("metadata database is bound to another host instance")

// HostBinding ties the database to the host configuration that owns it.
// Times are stored in whole seconds.
type HostBinding struct {
	InstanceID string     `json:"instance_id"`
	ConfigPath string     `json:"config_path"`
	BoundAt    time.Time  `json:"bound_at"`
	MigratedAt *time.Time `json:"migrated_at,omitempty"` // set when bound by legacy migration
}

func readHostBinding(ctx context.Context, x executor) (HostBinding, bool, error) {
	var b HostBinding
	var bound int64
	var migrated sql.NullInt64
	err := x.QueryRowContext(ctx, `SELECT instance_id,config_path,bound_at,migrated_at FROM host_binding WHERE id=1`).
		Scan(&b.InstanceID, &b.ConfigPath, &bound, &migrated)
	if errors.Is(err, sql.ErrNoRows) {
		return b, false, nil
	}
	if err != nil {
		return b, false, err
	}
	b.BoundAt = time.Unix(bound, 0).UTC()
	if migrated.Valid {
		t := time.Unix(migrated.Int64, 0).UTC()
		b.MigratedAt = &t
	}
	return b, true, nil
}

// HostBinding returns the bound host, if any.
func (s *Store) HostBinding(ctx context.Context) (HostBinding, bool, error) {
	return readHostBinding(ctx, s.db)
}

// BindHost records b as the owner of this database. Binding the same
// instance again is a no-op and keeps the first record; a different
// instance fails with ErrHostBindingMismatch.
func (s *Store) BindHost(ctx context.Context, b HostBinding) error {
	if b.InstanceID == "" || b.ConfigPath == "" || b.BoundAt.IsZero() {
		return errors.New("host binding needs an instance id, config path and bound time")
	}
	var migrated any
	if b.MigratedAt != nil {
		migrated = b.MigratedAt.Unix()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO host_binding(id,instance_id,config_path,bound_at,migrated_at) VALUES(1,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		b.InstanceID, b.ConfigPath, b.BoundAt.Unix(), migrated); err != nil {
		return err
	}
	cur, _, err := readHostBinding(ctx, tx)
	if err != nil {
		return err
	}
	if cur.InstanceID != b.InstanceID {
		return fmt.Errorf("%w: bound to %s, not %s", ErrHostBindingMismatch, cur.InstanceID, b.InstanceID)
	}
	return tx.Commit()
}

// ReplaceHostBinding records b as the owner of this database, replacing any
// earlier binding. It is for `flats config rebind`, which gives a copied
// data directory its own instance.
func (s *Store) ReplaceHostBinding(ctx context.Context, b HostBinding) error {
	if b.InstanceID == "" || b.ConfigPath == "" || b.BoundAt.IsZero() {
		return errors.New("host binding needs an instance id, config path and bound time")
	}
	var migrated any
	if b.MigratedAt != nil {
		migrated = b.MigratedAt.Unix()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO host_binding(id,instance_id,config_path,bound_at,migrated_at) VALUES(1,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET instance_id=excluded.instance_id, config_path=excluded.config_path, bound_at=excluded.bound_at, migrated_at=excluded.migrated_at`,
		b.InstanceID, b.ConfigPath, b.BoundAt.Unix(), migrated)
	return err
}
