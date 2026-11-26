package store

import (
	"context"
	"errors"
	"io/fs"
	"slices"
)

// The key IDs of docs/spec/secret-key-rotation.md: each sealed value carries the ID of the key that sealed it. The ID
// is a label. Nothing in this package uses it to open a value.

// addKeyIDs is the migration of rule 33. It writes no key ID, because it has no key: a value from before it keeps an
// empty ID until it is sealed the next time.
func addKeyIDs(ctx context.Context, x executor) error {
	if err := addColumn(ctx, x, "secrets", "key_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return addColumn(ctx, x, "host_keys", "key_id", "TEXT NOT NULL DEFAULT ''")
}

// KeyIDCounts says how many sealed values carry each key ID. The sealed values are the flat secrets and the operator
// key records that hold a value. The empty ID counts the values without a label, which were sealed before the key
// IDs. The methods work on a nil map, which holds no value.
type KeyIDCounts map[string]int

// Total is the number of sealed values.
func (c KeyIDCounts) Total() int {
	n := 0
	for _, v := range c {
		n += v
	}
	return n
}

// Carries reports whether at least one sealed value carries id. No value carries the empty ID: a value without a label
// says nothing about its key.
func (c KeyIDCounts) Carries(id string) bool { return id != "" && c[id] > 0 }

// AllLabelled reports whether there is a sealed value and every sealed value carries a key ID.
func (c KeyIDCounts) AllLabelled() bool { return c.Total() > 0 && c[""] == 0 }

// Single returns the key ID that every sealed value carries, when there is a sealed value and all of them carry the
// same ID.
func (c KeyIDCounts) Single() (string, bool) {
	ids := c.IDs()
	if len(ids) != 1 || !c.AllLabelled() {
		return "", false
	}
	return ids[0], true
}

// IDs lists the key IDs that sealed values carry, sorted, without the empty ID.
func (c KeyIDCounts) IDs() []string {
	ids := make([]string, 0, len(c))
	for id, n := range c {
		if id != "" && n > 0 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// SealedKeyIDs counts the sealed values by the key ID that they carry.
func (s *Store) SealedKeyIDs(ctx context.Context) (KeyIDCounts, error) {
	return sealedKeyIDs(ctx, s.db)
}

func sealedKeyIDs(ctx context.Context, x executor) (KeyIDCounts, error) {
	rows, err := x.QueryContext(ctx, `SELECT key_id, COUNT(*) FROM (
  SELECT key_id FROM secrets
  UNION ALL SELECT key_id FROM host_keys WHERE ciphertext IS NOT NULL
) GROUP BY key_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := KeyIDCounts{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		ids[id] = n
	}
	return ids, rows.Err()
}

// SealedKeyIDsBeside is SealedKeyIDs for a command that holds no host lock and must not migrate flats.db (rules 25
// and 26). It opens the database at path read-only, as OpenAuditReader does, and never creates it. ok is false, with
// no error, when there is no database or when its schema is not the one of this binary: the key IDs cannot be read
// then, and the caller makes no check with them.
func SealedKeyIDsBeside(ctx context.Context, path string) (ids KeyIDCounts, ok bool, err error) {
	db, err := openBeside(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer db.Close()
	// One read transaction, so that a host that migrates meanwhile cannot change the schema between the two reads.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return nil, false, err
	}
	if version != latestVersion() {
		return nil, false, nil
	}
	if ids, err = sealedKeyIDs(ctx, tx); err != nil {
		return nil, false, err
	}
	return ids, true, nil
}
