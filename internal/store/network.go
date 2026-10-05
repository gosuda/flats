package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// NetworkPolicy is desired operator-approved outbound access. No row means deny all.
type NetworkPolicy struct {
	Origins   []string  `json:"origins"`
	UpdatedAt time.Time `json:"updated_at"`
}

func decodeNetworkOrigins(value string) ([]string, error) {
	var origins []string
	if err := json.Unmarshal([]byte(value), &origins); err != nil {
		return nil, errors.New("invalid stored network policy")
	}
	if origins == nil {
		origins = []string{}
	}
	return origins, nil
}

func (s *Store) NetworkPolicy(ctx context.Context, flat string) (NetworkPolicy, error) {
	var value string
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT origins,updated_at FROM network_settings WHERE flat=?`, flat).Scan(&value, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return NetworkPolicy{Origins: []string{}}, nil
	}
	if err != nil {
		return NetworkPolicy{}, err
	}
	origins, err := decodeNetworkOrigins(value)
	return NetworkPolicy{Origins: origins, UpdatedAt: fromUnix(at)}, err
}

// SetNetworkPolicy atomically replaces the entire allowlist; an empty list revokes it.
func (s *Store) SetNetworkPolicy(ctx context.Context, flat string, p NetworkPolicy) error {
	if p.Origins == nil {
		p.Origins = []string{}
	}
	b, err := json.Marshal(p.Origins)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO network_settings(flat,origins,updated_at) VALUES(?,?,?) ON CONFLICT(flat) DO UPDATE SET origins=excluded.origins,updated_at=excluded.updated_at`, flat, string(b), unix(p.UpdatedAt))
	return err
}

func addNetworkSettings(ctx context.Context, x executor) error {
	_, err := x.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS network_settings (
 flat TEXT PRIMARY KEY REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
 origins TEXT NOT NULL,
 updated_at INTEGER NOT NULL
 );`)
	return err
}
