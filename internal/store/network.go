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

// SetNetworkPolicy atomically replaces the entire allowlist; an empty list revokes it. It reports whether the list
// changed (no row counts as the empty list). A change and its network.set audit event (docs/spec/event-webhook.md,
// rule 21) are one transaction, and a write of the list that the flat has writes no event.
func (s *Store) SetNetworkPolicy(ctx context.Context, flat string, p NetworkPolicy, a Audit) (bool, error) {
	if p.Origins == nil {
		p.Origins = []string{}
	}
	b, err := json.Marshal(p.Origins)
	if err != nil {
		return false, err
	}
	changed := false
	_, err = s.immediate(ctx, func(x executor) (bool, error) {
		was := "[]"
		if err := x.QueryRowContext(ctx, `SELECT origins FROM network_settings WHERE flat=?`, flat).Scan(&was); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		if _, err := x.ExecContext(ctx, `INSERT INTO network_settings(flat,origins,updated_at) VALUES(?,?,?) ON CONFLICT(flat) DO UPDATE SET origins=excluded.origins,updated_at=excluded.updated_at`, flat, string(b), unix(p.UpdatedAt)); err != nil {
			return false, err
		}
		if was == string(b) {
			return true, nil
		}
		uid, err := flatUID(ctx, x, flat)
		if err != nil {
			return false, err
		}
		e := a.event(AuditNetworkSet)
		e.Flat, e.FlatID = flat, uid
		data := map[string]any{"count": len(p.Origins)}
		kept, truncated := FitOrigins(p.Origins)
		data["origins"] = kept
		if truncated {
			data["truncated"] = true
		}
		e.Data = AuditData(data)
		if err := appendAudit(ctx, x, e); err != nil {
			return false, err
		}
		changed = true
		return true, nil
	})
	return changed && err == nil, err
}

// maxOriginsText is the largest JSON text of the origin list of an audit row or a webhook event, in bytes. The longest
// list that the host accepts (32 origins with the longest host names) has more than 8 KiB, and a row holds at most 4 KiB
// of data, as the body of an event does.
const maxOriginsText = 3000

// FitOrigins returns the first origins whose JSON list has at most maxOriginsText bytes, and whether it left any out
// (docs/spec/event-webhook.md, rules 4 and 21). A list of ordinary length is returned whole.
func FitOrigins(origins []string) (kept []string, truncated bool) {
	size := 2 // the brackets
	for i, o := range origins {
		b, _ := json.Marshal(o)
		size += len(b)
		if i > 0 {
			size++ // the comma
		}
		if size > maxOriginsText {
			return origins[:i], true
		}
	}
	return origins, false
}

func addNetworkSettings(ctx context.Context, x executor) error {
	_, err := x.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS network_settings (
 flat TEXT PRIMARY KEY REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
 origins TEXT NOT NULL,
 updated_at INTEGER NOT NULL
 );`)
	return err
}
