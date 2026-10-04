package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ErrSchemaTooNew is returned for a database written by a newer Flats. The
// file is left untouched; restore a backup or run the newer binary.
var ErrSchemaTooNew = errors.New("metadata schema is newer than this Flats supports")

// ErrNotFlats is returned for a file that is not a Flats metadata database.
var ErrNotFlats = errors.New("not a Flats metadata database")

// executor runs statements on one connection. *sql.Tx and *sql.Conn satisfy
// it, so a migration step and every helper it calls share one transaction.
type executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// migration is one schema step. up runs inside the transaction that also
// sets user_version to version, so a failed step leaves no trace.
type migration struct {
	version int
	name    string
	up      func(ctx context.Context, x executor) error
}

// migrations is ordered by version. Steps before 5 predate the registry and
// are folded into the legacy baseline. Tests replace it to inject failures.
var migrations = []migration{
	{5, "legacy baseline 5", legacyBaseline},
	{6, "host binding", addHostBinding},
	{7, "application environment variables", addEnvVars},
}

func latestVersion() int { return migrations[len(migrations)-1].version }

// backupsKept is how many pre-migration backups survive pruning.
const backupsKept = 3

// Info describes a metadata database as found on disk.
type Info struct {
	Exists  bool         `json:"exists"`  // the file (or its WAL) holds data
	New     bool         `json:"new"`     // no tables yet: Open creates the schema
	Version int          `json:"version"` // PRAGMA user_version
	Latest  int          `json:"latest"`  // newest schema this build supports
	IsFlats bool         `json:"is_flats"`
	HasData bool         `json:"has_data"`          // some table other than host_binding has a row
	Binding *HostBinding `json:"binding,omitempty"` // nil when no host is bound
}

// Inspect reports the state of the database at path without writing to it.
// It sets no pragmas and runs no DDL; see openReadOnly for how sidecar
// files are avoided. A missing or empty file is a new database. The
// returned Info is filled as far as it could be read, also on error.
func Inspect(path string) (Info, error) {
	info := Info{Latest: latestVersion(), IsFlats: true, New: true}
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return info, nil
	}
	if err != nil {
		return info, err
	}
	if !fi.Mode().IsRegular() {
		return info, fmt.Errorf("%s is not a regular file", path)
	}
	walSize := sidecarSize(path + "-wal")
	if fi.Size() == 0 && walSize == 0 {
		return info, nil
	}
	info.Exists = true
	if fi.Size() > 0 {
		if err := checkHeader(path); err != nil {
			info.IsFlats = false
			return info, err
		}
	}

	ctx := context.Background()
	db, err := openReadOnly(path)
	if err != nil {
		return info, err
	}
	defer db.Close()
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&info.Version); err != nil {
		return info, err
	}
	tables, err := tableNames(ctx, db)
	if err != nil {
		return info, err
	}
	info.New = len(tables) == 0
	info.IsFlats = info.New || slices.Contains(tables, "flats")
	switch {
	case !info.IsFlats:
		return info, fmt.Errorf("%w: no flats table among %d tables", ErrNotFlats, len(tables))
	case info.Version > info.Latest:
		return info, fmt.Errorf("%w: schema %d, supported %d", ErrSchemaTooNew, info.Version, info.Latest)
	case info.New && info.Version != 0:
		info.IsFlats = false
		return info, fmt.Errorf("%w: no tables but schema version %d", ErrNotFlats, info.Version)
	}
	for _, t := range tables {
		if t == "host_binding" || info.HasData {
			continue
		}
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "`+strings.ReplaceAll(t, `"`, `""`)+`")`).Scan(&info.HasData); err != nil {
			return info, err
		}
	}
	if slices.Contains(tables, "host_binding") {
		b, ok, err := readHostBinding(ctx, db)
		if err != nil {
			return info, err
		}
		if ok {
			info.Binding = &b
		}
	}
	return info, nil
}

// fileURI turns a path into an SQLite URI, so Inspect and Open always name
// the same file even when the path contains URI syntax.
func fileURI(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return "file:" + strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(path))
}

// openReadOnly opens path without write access. When no WAL or rollback
// journal holds unapplied pages, the file is opened immutable so SQLite
// creates no -wal or -shm file. Otherwise the pending pages must be read,
// so it uses a plain read-only open; a hot rollback journal then fails
// instead of being replayed. That open never changes the database or its
// WAL, but SQLite may create or update the -shm index it reads the WAL
// through.
func openReadOnly(path string) (*sql.DB, error) {
	query := "?mode=ro&immutable=1"
	if sidecarSize(path+"-wal") > 0 || sidecarSize(path+"-journal") > 0 {
		query = "?mode=ro"
	}
	db, err := sql.Open("sqlite", fileURI(path)+query)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func sidecarSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// checkHeader rejects a non-empty file that is not an SQLite database
// before the driver touches it.
func checkHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 16)
	if _, err := io.ReadFull(f, head); err != nil {
		return fmt.Errorf("%w: %v", ErrNotFlats, err)
	}
	if !bytes.Equal(head, []byte("SQLite format 3\x00")) {
		return fmt.Errorf("%w: not an SQLite file", ErrNotFlats)
	}
	return nil
}

func tableNames(ctx context.Context, x executor) ([]string, error) {
	rows, err := x.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// migrate brings the inspected database to the latest schema. An existing
// database is backed up first; without a backup nothing is migrated.
func migrate(ctx context.Context, path string, info Info) error {
	if !info.New {
		if err := backup(ctx, path, info.Version, time.Now()); err != nil {
			return fmt.Errorf("backup before migration: %w", err)
		}
	}
	// _txlock makes every BeginTx a BEGIN IMMEDIATE, so a concurrent opener
	// waits for the write lock instead of failing halfway through a step.
	db, err := sql.Open("sqlite", fileURI(path)+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// WAL before the first step: a process killed mid-step then leaves
	// uncommitted WAL frames, which a read-only Inspect can read past. In
	// rollback-journal mode (a database restored from a VACUUM INTO backup)
	// it would leave a hot journal that only a writer can recover, and every
	// later start would fail its read-only probe.
	if err := useWAL(ctx, db); err != nil {
		return fmt.Errorf("switch to WAL: %w", err)
	}
	for _, m := range migrations {
		if m.version <= info.Version {
			continue
		}
		if err := runMigration(ctx, db, m); err != nil {
			return fmt.Errorf("step %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

// useWAL switches db to WAL. The switch needs an exclusive lock and SQLite
// does not call the busy handler for it, so a concurrent opener is waited
// out here.
func useWAL(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		var mode string
		err := db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode)
		switch {
		case err == nil && mode == "wal":
			return nil
		case err == nil:
			return fmt.Errorf("journal mode is %s", mode)
		case !strings.Contains(err.Error(), "SQLITE_BUSY") || time.Now().After(deadline):
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// runMigration applies one step and its user_version in one transaction.
// The version is read again under the write lock, so a step another opener
// already committed is skipped.
func runMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ver int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&ver); err != nil {
		return err
	}
	if ver > latestVersion() {
		return fmt.Errorf("%w: schema %d, supported %d", ErrSchemaTooNew, ver, latestVersion())
	}
	if ver >= m.version {
		return nil
	}
	if err := m.up(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
		return err
	}
	return tx.Commit()
}

// backupName matches files written by backup; the timestamp sorts as text.
var backupName = regexp.MustCompile(`^flats\.v[0-9]+\.([0-9]{8}T[0-9]{6}\.[0-9]{9}Z)\.db$`)

// backup writes a consistent copy of the database to
// <dir>/backups/flats.v<version>.<UTC time>.db and keeps the newest
// backupsKept copies. It reads through a read-only connection, so a failed
// backup has not modified the database.
func backup(ctx context.Context, path string, version int, now time.Time) error {
	dir := filepath.Join(filepath.Dir(path), "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("flats.v%d.%s.db", version, now.UTC().Format("20060102T150405.000000000Z"))
	target := filepath.Join(dir, name)
	// Create the file first: it gets private permissions from the start, and
	// an existing file is never overwritten. VACUUM INTO accepts an empty one.
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(target)
		return err
	}
	db, err := openReadOnly(path)
	if err != nil {
		os.Remove(target)
		return err
	}
	_, err = db.ExecContext(ctx, `VACUUM INTO ?`, target)
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(target)
		return err
	}
	// The new backup is complete, so failing to remove old ones must not
	// block the migration it protects; the next backup retries the pruning.
	_ = pruneBackups(dir)
	return nil
}

// pruneBackups removes all but the newest backupsKept backup files. Other
// files in the directory are left alone.
func pruneBackups(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type found struct{ stamp, name string }
	var all []found
	for _, e := range entries {
		if m := backupName.FindStringSubmatch(e.Name()); m != nil && e.Type().IsRegular() {
			all = append(all, found{m[1], e.Name()})
		}
	}
	slices.SortFunc(all, func(a, b found) int {
		if c := strings.Compare(b.stamp, a.stamp); c != 0 {
			return c
		}
		return strings.Compare(b.name, a.name)
	})
	var errs []error
	for _, b := range all[min(len(all), backupsKept):] {
		if err := os.Remove(filepath.Join(dir, b.name)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// legacyBaseline is the schema creation and data migration that Open ran
// before the registry, for databases at user_version 0 to 5. It reads the
// starting version itself because its branches depend on it.
func legacyBaseline(ctx context.Context, x executor) error {
	var ver int
	if err := x.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&ver); err != nil {
		return err
	}
	if _, err := x.ExecContext(ctx, schema); err != nil {
		return err
	}
	if ver < 1 {
		// Before the redirects table only the latest rename was kept, on the
		// flat row. A slug that a flat uses again is no longer a redirect.
		if _, err := x.ExecContext(ctx, `INSERT OR IGNORE INTO redirects(old,flat,until)
			SELECT old_slug, slug, old_slug_until FROM flats
			WHERE old_slug IS NOT NULL AND old_slug_until IS NOT NULL AND old_slug NOT IN (SELECT slug FROM flats)`); err != nil {
			return err
		}
	}
	// Persist only eligibility here, never permission. The explicit operator
	// network selection decides whether to retain legacy Private Tailscale.
	if _, err := x.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS legacy_private_upgrade (flat TEXT PRIMARY KEY REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE, pending INTEGER NOT NULL DEFAULT 1)`); err != nil {
		return err
	}
	if ver < 2 {
		if _, err := x.ExecContext(ctx, `INSERT OR IGNORE INTO legacy_private_upgrade(flat) SELECT slug FROM flats`); err != nil {
			return err
		}
		if err := migrateLifecycle(ctx, x); err != nil {
			return err
		}
	}
	if err := addColumn(ctx, x, "approvals", "result_data", "TEXT"); err != nil {
		return err
	}
	if err := addColumn(ctx, x, "approvals", "decided_by", "TEXT"); err != nil {
		return err
	}
	return addColumn(ctx, x, "approvals", "authorized_at", "INTEGER")
}

// addHostBinding creates the single-row table that ties a database to the
// host configuration that owns it.
func addHostBinding(ctx context.Context, x executor) error {
	_, err := x.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS host_binding (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  instance_id TEXT NOT NULL,
  config_path TEXT NOT NULL,
  bound_at INTEGER NOT NULL,
  migrated_at INTEGER
)`)
	return err
}

func hasColumn(ctx context.Context, x executor, table, col string) (bool, error) {
	rows, err := x.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

func addColumn(ctx context.Context, x executor, table, col, decl string) error {
	ok, err := hasColumn(ctx, x, table, col)
	if err != nil || ok {
		return err
	}
	_, err = x.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+col+` `+decl)
	return err
}
