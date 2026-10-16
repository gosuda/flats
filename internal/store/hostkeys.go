package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// addHostKeys adds the operator keys and the console node ownership record.
func addHostKeys(ctx context.Context, x executor) error {
	_, err := x.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS host_keys (
  id TEXT PRIMARY KEY CHECK (id IN ('tailscale', 'zrok', 'netbird')),
  nonce BLOB,
  ciphertext BLOB,
  source TEXT NOT NULL CHECK (source IN ('console', 'cli', 'flag', 'env', 'config')),
  updated_at INTEGER NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  CHECK ((nonce IS NULL) = (ciphertext IS NULL))
);
CREATE TABLE IF NOT EXISTS console_nodes (
  name TEXT PRIMARY KEY,
  retiring INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT ''
);`)
	return err
}

// consoleNodeProviders keys console nodes by network as well as name: the
// same name may serve the console on Tailscale and on NetBird. Existing rows
// are Tailscale nodes.
func consoleNodeProviders(ctx context.Context, x executor) error {
	_, err := x.ExecContext(ctx, `
CREATE TABLE console_nodes_v14 (
  provider TEXT NOT NULL DEFAULT 'tailscale' CHECK (provider IN ('tailscale', 'netbird')),
  name TEXT NOT NULL,
  retiring INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (provider, name)
);
INSERT INTO console_nodes_v14 (provider, name, retiring, last_error)
  SELECT 'tailscale', name, retiring, last_error FROM console_nodes;
DROP TABLE console_nodes;
ALTER TABLE console_nodes_v14 RENAME TO console_nodes;`)
	return err
}

// HostKey is one operator key record. Nonce and Ciphertext are nil when the
// row only records a connection (zrok, NetBird) without a value.
type HostKey struct {
	ID         string
	Nonce      []byte
	Ciphertext []byte
	Source     string
	UpdatedAt  time.Time
	LastError  string
}

// HasValue reports whether the record holds a sealed value.
func (k HostKey) HasValue() bool { return k.Ciphertext != nil }

// GetHostKey returns the record of id, or ErrNotFound.
func (s *Store) GetHostKey(ctx context.Context, id string) (HostKey, error) {
	return getHostKey(ctx, s.db, id)
}

func getHostKey(ctx context.Context, x executor, id string) (HostKey, error) {
	k := HostKey{ID: id}
	var at int64
	err := x.QueryRowContext(ctx, `SELECT nonce, ciphertext, source, updated_at, last_error FROM host_keys WHERE id=?`, id).
		Scan(&k.Nonce, &k.Ciphertext, &k.Source, &at, &k.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return HostKey{}, ErrNotFound
	}
	if err != nil {
		return HostKey{}, err
	}
	k.UpdatedAt = fromUnix(at)
	return k, nil
}

// ListHostKeys returns every key record, ordered by id.
func (s *Store) ListHostKeys(ctx context.Context) ([]HostKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, nonce, ciphertext, source, updated_at, last_error FROM host_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostKey
	for rows.Next() {
		var k HostKey
		var at int64
		if err := rows.Scan(&k.ID, &k.Nonce, &k.Ciphertext, &k.Source, &at, &k.LastError); err != nil {
			return nil, err
		}
		k.UpdatedAt = fromUnix(at)
		out = append(out, k)
	}
	return out, rows.Err()
}

// PutHostKey replaces the record of k.ID.
func (s *Store) PutHostKey(ctx context.Context, k HostKey) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO host_keys (id, nonce, ciphertext, source, updated_at, last_error) VALUES (?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET nonce=excluded.nonce, ciphertext=excluded.ciphertext, source=excluded.source,
  updated_at=excluded.updated_at, last_error=excluded.last_error`,
		k.ID, k.Nonce, k.Ciphertext, k.Source, unix(k.UpdatedAt), k.LastError)
	return err
}

// SetHostKeyError records the last failed use of id's waiting value.
func (s *Store) SetHostKeyError(ctx context.Context, id, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE host_keys SET last_error=? WHERE id=?`, msg, id)
	return err
}

// DeleteHostKey removes the record of id; a missing one is not an error.
func (s *Store) DeleteHostKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM host_keys WHERE id=?`, id)
	return err
}

// HasSealedHostKeys reports whether any key record holds a sealed value.
func (s *Store) HasSealedHostKeys(ctx context.Context) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM host_keys WHERE ciphertext IS NOT NULL)`).Scan(&ok)
	return ok, err
}

// ConsoleNode records a tailnet node Flats runs for the console.
type ConsoleNode struct {
	// Provider is the network the node runs on: "tailscale" or "netbird".
	// "" means "tailscale".
	Provider  string
	Name      string
	Retiring  bool
	LastError string
}

// ListConsoleNodes returns the console node records, ordered by name.
func (s *Store) ListConsoleNodes(ctx context.Context) ([]ConsoleNode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, name, retiring, last_error FROM console_nodes ORDER BY provider, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConsoleNode
	for rows.Next() {
		var n ConsoleNode
		if err := rows.Scan(&n.Provider, &n.Name, &n.Retiring, &n.LastError); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// PutConsoleNode inserts or replaces a console node record.
func (s *Store) PutConsoleNode(ctx context.Context, n ConsoleNode) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO console_nodes (provider, name, retiring, last_error) VALUES (?,?,?,?)
ON CONFLICT(provider, name) DO UPDATE SET retiring=excluded.retiring, last_error=excluded.last_error`, nodeProvider(n.Provider), n.Name, n.Retiring, n.LastError)
	return err
}

// DeleteConsoleNode removes a console node record of provider ("" is
// Tailscale).
func (s *Store) DeleteConsoleNode(ctx context.Context, provider, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM console_nodes WHERE provider=? AND name=?`, nodeProvider(provider), name)
	return err
}

func nodeProvider(p string) string {
	if p == "" {
		return "tailscale"
	}
	return p
}
