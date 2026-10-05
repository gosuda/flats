package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrEnvCollision means a name is already used by the other setting type.
var ErrEnvCollision = errors.New("name is already used by an environment variable or secret")

// EnvVar is ordinary, readable application configuration, never a secret.
type EnvVar struct {
	Name      string    `json:"name"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

func envWriteError(err error) error {
	if err != nil && strings.Contains(err.Error(), "env_secret_collision") {
		return ErrEnvCollision
	}
	return err
}

// PutEnv upserts ordinary configuration. Database triggers prevent a secret collision atomically.
func (s *Store) PutEnv(ctx context.Context, flat string, v EnvVar) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO env_vars(flat,name,value,updated_at) VALUES(?,?,?,?) ON CONFLICT(flat,name) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, flat, v.Name, v.Value, unix(v.UpdatedAt))
	return envWriteError(err)
}

func (s *Store) DeleteEnv(ctx context.Context, flat, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM env_vars WHERE flat=? AND name=?`, flat, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("environment variable %q: %w", name, ErrNotFound)
	}
	return nil
}

func (s *Store) ListEnv(ctx context.Context, flat string) ([]EnvVar, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name,value,updated_at FROM env_vars WHERE flat=? ORDER BY name`, flat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]EnvVar, 0)
	for rows.Next() {
		var v EnvVar
		var at int64
		if err := rows.Scan(&v.Name, &v.Value, &at); err != nil {
			return nil, err
		}
		v.UpdatedAt = fromUnix(at)
		out = append(out, v)
	}
	return out, rows.Err()
}

// EnvironmentSnapshot reads ordinary variables and sealed secrets in a single
// SQLite statement, so concurrent setting writes cannot split the two namespaces.
// It does not decrypt or expose secret values.
func (s *Store) EnvironmentSnapshot(ctx context.Context, flat string) ([]EnvVar, []SealedSecret, error) {
	vars, secs, _, err := s.RuntimeSettingsSnapshot(ctx, flat)
	return vars, secs, err
}

// RuntimeSettingsSnapshot reads all desired runtime settings in one SQLite
// statement. Concurrent writes cannot mix environment and network snapshots.
func (s *Store) RuntimeSettingsSnapshot(ctx context.Context, flat string) ([]EnvVar, []SealedSecret, []string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT 0,name,value,X'',X'' FROM env_vars WHERE flat=?
 UNION ALL SELECT 1,name,'',nonce,ciphertext FROM secrets WHERE flat=?
 UNION ALL SELECT 2,'',origins,X'',X'' FROM network_settings WHERE flat=?`, flat, flat, flat)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	vars := make([]EnvVar, 0)
	secs := make([]SealedSecret, 0)
	origins := make([]string, 0)
	for rows.Next() {
		var kind int
		var name, value string
		var nonce, ciphertext []byte
		if err := rows.Scan(&kind, &name, &value, &nonce, &ciphertext); err != nil {
			return nil, nil, nil, err
		}
		switch kind {
		case 0:
			vars = append(vars, EnvVar{Name: name, Value: value})
		case 1:
			secs = append(secs, SealedSecret{Name: name, Nonce: nonce, Ciphertext: ciphertext})
		case 2:
			origins, err = decodeNetworkOrigins(value)
			if err != nil {
				return nil, nil, nil, err
			}
		}
	}
	return vars, secs, origins, rows.Err()
}

// addEnvVars keeps ordinary configuration separate from encrypted secrets.
// Both directions are protected in SQLite, including across Store instances.
func addEnvVars(ctx context.Context, x executor) error {
	_, err := x.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS env_vars (
 flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
 name TEXT NOT NULL,
 value TEXT NOT NULL,
 updated_at INTEGER NOT NULL,
 PRIMARY KEY(flat,name)
 );
 CREATE TRIGGER IF NOT EXISTS env_vars_secret_insert BEFORE INSERT ON env_vars
 WHEN EXISTS(SELECT 1 FROM secrets WHERE flat=NEW.flat AND name=NEW.name)
 BEGIN SELECT RAISE(ABORT,'env_secret_collision'); END;
 CREATE TRIGGER IF NOT EXISTS env_vars_secret_update BEFORE UPDATE OF flat,name ON env_vars
 WHEN EXISTS(SELECT 1 FROM secrets WHERE flat=NEW.flat AND name=NEW.name)
 BEGIN SELECT RAISE(ABORT,'env_secret_collision'); END;
 CREATE TRIGGER IF NOT EXISTS secrets_env_insert BEFORE INSERT ON secrets
 WHEN EXISTS(SELECT 1 FROM env_vars WHERE flat=NEW.flat AND name=NEW.name)
 BEGIN SELECT RAISE(ABORT,'env_secret_collision'); END;
 CREATE TRIGGER IF NOT EXISTS secrets_env_update BEFORE UPDATE OF flat,name ON secrets
 WHEN EXISTS(SELECT 1 FROM env_vars WHERE flat=NEW.flat AND name=NEW.name)
 BEGIN SELECT RAISE(ABORT,'env_secret_collision'); END;`)
	return err
}
