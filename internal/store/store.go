// Package store keeps Flats metadata in a single SQLite database.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrExists is returned when a flat slug is already taken.
var ErrExists = errors.New("already exists")

// Visibility of a flat. Only private and public are stored. The listed and
// unlisted values remain accepted on input so older clients keep working;
// Canonical maps both to public.
type Visibility string

const (
	Private        Visibility = "private"
	Public         Visibility = "public"
	PublicListed   Visibility = "public-listed"
	PublicUnlisted Visibility = "public-unlisted"
)

// Valid reports whether v is a known visibility, including legacy aliases.
func (v Visibility) Valid() bool {
	return v == Private || v == Public || v == PublicListed || v == PublicUnlisted
}

// Canonical returns private or public. Legacy listed and unlisted values are public.
func (v Visibility) Canonical() Visibility {
	if v.Public() {
		return Public
	}
	return Private
}

// Public reports whether v exposes the flat beyond the private network.
func (v Visibility) Public() bool {
	return v == Public || v == PublicListed || v == PublicUnlisted
}

// Rank orders visibilities by exposure. Private is 0 and every public value is 1,
// so neither direction is a silent narrowing.
func (v Visibility) Rank() int {
	if v.Public() {
		return 1
	}
	return 0
}

// Flat is one hosted site.
type Flat struct {
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	Visibility  Visibility `json:"visibility"`
	LiveVersion int        `json:"live_version"` // 0 when never deployed
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	OldSlug     string     `json:"old_slug,omitempty"`
	OldSlugTill *time.Time `json:"old_slug_until,omitempty"`
}

// Version is an immutable uploaded build.
type Version struct {
	Flat       string          `json:"flat"`
	Number     int             `json:"number"`
	Hash       string          `json:"hash"`
	Size       int64           `json:"size"`
	Files      int             `json:"files"`
	Kind       string          `json:"kind"` // static | server
	Manifest   json.RawMessage `json:"manifest"`
	GitSHA     string          `json:"git_sha,omitempty"`
	GitDirty   bool            `json:"git_dirty"`
	Message    string          `json:"message,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	Screenshot string          `json:"screenshot,omitempty"`
	Pruned     bool            `json:"pruned"`
	// Published is true for an immutable activated version. Draft snapshots
	// use Role "draft", Number 0 and Revision.
	Published bool   `json:"published"`
	Role      string `json:"role,omitempty"`
	Revision  int    `json:"revision,omitempty"`
}

// Event is a log line attached to a flat (deploys, health checks, runtime).
type Event struct {
	ID      int64           `json:"id"`
	Flat    string          `json:"flat"`
	Time    time.Time       `json:"time"`
	Level   string          `json:"level"` // info | warn | error
	Kind    string          `json:"kind"`  // deploy, health, runtime, visibility, ...
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Approval is a request that needs the operator.
type Approval struct {
	ID             string          `json:"id"`
	Flat           string          `json:"flat"`
	Action         string          `json:"action"` // set_visibility | delete
	Params         json.RawMessage `json:"params"`
	Status         string          `json:"status"` // pending | applying | approved | rejected | failed
	Via            string          `json:"via"`    // mcp | cli | api
	Reason         string          `json:"reason,omitempty"`
	Result         string          `json:"result,omitempty"`
	RequestedAt    time.Time       `json:"requested_at"`
	DecidedAt      *time.Time      `json:"decided_at,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	ResultData     json.RawMessage `json:"result_data,omitempty"`
	DecidedBy      string          `json:"decided_by,omitempty"`
	AuthorizedAt   *time.Time      `json:"authorized_at,omitempty"`
}

// Preview is an ephemeral address for a saved version.
type Preview struct {
	Host       string    `json:"host"`
	Flat       string    `json:"flat"`
	Version    int       `json:"version"`
	CreatedAt  time.Time `json:"created_at"`
	LastAccess time.Time `json:"last_access"`
	Target     string    `json:"target,omitempty"` // version | draft
	Revision   int       `json:"revision,omitempty"`
}

// Store wraps the metadata database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS flats (
  slug TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  visibility TEXT NOT NULL DEFAULT 'private',
  live_version INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  old_slug TEXT,
  old_slug_until INTEGER
);
CREATE TABLE IF NOT EXISTS versions (
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  number INTEGER NOT NULL,
  hash TEXT NOT NULL,
  size INTEGER NOT NULL,
  files INTEGER NOT NULL,
  kind TEXT NOT NULL,
  manifest TEXT NOT NULL,
  git_sha TEXT,
  git_dirty INTEGER NOT NULL DEFAULT 0,
  message TEXT,
  created_at INTEGER NOT NULL,
  screenshot TEXT,
  pruned INTEGER NOT NULL DEFAULT 0,
  published INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (flat, number)
);
CREATE TABLE IF NOT EXISTS deployments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  version INTEGER NOT NULL,
  previous INTEGER NOT NULL,
  kind TEXT NOT NULL,
  at INTEGER NOT NULL,
  approval_id TEXT
);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  flat TEXT NOT NULL,
  at INTEGER NOT NULL,
  level TEXT NOT NULL,
  kind TEXT NOT NULL,
  message TEXT NOT NULL,
  data TEXT
);
CREATE INDEX IF NOT EXISTS events_flat ON events(flat, id);
CREATE TABLE IF NOT EXISTS approvals (
  id TEXT PRIMARY KEY,
  flat TEXT NOT NULL,
  action TEXT NOT NULL,
  params TEXT NOT NULL,
  status TEXT NOT NULL,
  via TEXT NOT NULL,
  reason TEXT,
  result TEXT,
  requested_at INTEGER NOT NULL,
  decided_at INTEGER,
  idempotency_key TEXT,
  result_data TEXT,
  decided_by TEXT,
  authorized_at INTEGER
);
CREATE TABLE IF NOT EXISTS previews (
  host TEXT PRIMARY KEY,
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  version INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  last_access INTEGER NOT NULL,
  target TEXT NOT NULL DEFAULT 'version',
  revision INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS drafts (
  flat TEXT PRIMARY KEY REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  revision INTEGER NOT NULL,
  hash TEXT NOT NULL,
  base_version INTEGER NOT NULL DEFAULT 0,
  dirty INTEGER NOT NULL DEFAULT 1,
  size INTEGER NOT NULL,
  files INTEGER NOT NULL,
  kind TEXT NOT NULL,
  manifest TEXT NOT NULL,
  git_sha TEXT,
  git_dirty INTEGER NOT NULL DEFAULT 0,
  message TEXT,
  updated_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS draft_revisions (
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  revision INTEGER NOT NULL,
  hash TEXT NOT NULL,
  size INTEGER NOT NULL,
  files INTEGER NOT NULL,
  kind TEXT NOT NULL,
  manifest TEXT NOT NULL,
  git_sha TEXT,
  git_dirty INTEGER NOT NULL DEFAULT 0,
  message TEXT,
  screenshot TEXT,
  created_at INTEGER NOT NULL,
  pruned INTEGER NOT NULL DEFAULT 0,
  legacy_number INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (flat, revision)
);
CREATE TABLE IF NOT EXISTS flat_providers (
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  provider TEXT NOT NULL,
  permitted INTEGER NOT NULL,
  PRIMARY KEY (flat, provider)
);
CREATE TABLE IF NOT EXISTS runtime_generation (
  id INTEGER PRIMARY KEY CHECK(id=1),
  generation INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS pageviews (
  flat TEXT NOT NULL,
  day TEXT NOT NULL,
  count INTEGER NOT NULL,
  PRIMARY KEY (flat, day)
);
CREATE TABLE IF NOT EXISTS pagepaths (
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  day TEXT NOT NULL,
  path TEXT NOT NULL,
  count INTEGER NOT NULL,
  PRIMARY KEY (flat, day, path)
);
CREATE TABLE IF NOT EXISTS redirects (
  old TEXT PRIMARY KEY,
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  until INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS secrets (
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  name TEXT NOT NULL,
  nonce BLOB NOT NULL,
  ciphertext BLOB NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (flat, name)
);
`

// Open opens the database at path, migrating it to the latest schema first.
//
// It inspects the file read-only before anything is written: a database
// from a newer Flats or a file that is not a Flats database is rejected
// unchanged. An existing database that needs migration is backed up with
// VACUUM INTO, then each pending step commits in its own transaction. The
// returned store uses a separate connection opened after migration.
func Open(path string) (*Store, error) {
	ctx := context.Background()
	info, err := Inspect(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if info.Version < info.Latest {
		if err := migrate(ctx, path, info); err != nil {
			return nil, fmt.Errorf("migrate %s: %w", path, err)
		}
	}
	db, err := sql.Open("sqlite", fileURI(path)+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // serialize writers; metadata traffic is small
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func unix(t time.Time) int64 { return t.UnixMilli() }
func fromUnix(ms int64) time.Time {
	return time.UnixMilli(ms).UTC()
}

// --- flats ---

// ProviderGrant is a non-local provider a new flat is allowed on from the
// start, with the event that records why.
type ProviderGrant struct {
	Provider string
	Event    Event
}

// CreateFlat inserts a new flat, failing if the slug exists. Each grant's
// permission and audit event are written in the same transaction, so a
// flat never exists with a grant that has no record.
func (s *Store) CreateFlat(ctx context.Context, f Flat, grants ...ProviderGrant) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO flats(slug,name,visibility,live_version,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		f.Slug, f.Name, string(f.Visibility), f.LiveVersion, unix(f.CreatedAt), unix(f.UpdatedAt))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("flat %q %w", f.Slug, ErrExists)
	}
	if err != nil {
		return err
	}
	for _, g := range grants {
		if g.Provider == ProviderLocal {
			continue // always permitted
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO flat_providers(flat, provider, permitted) VALUES(?,?,1)
			ON CONFLICT(flat, provider) DO UPDATE SET permitted=1`, f.Slug, g.Provider); err != nil {
			return err
		}
		e := g.Event
		var data any
		if len(e.Data) > 0 {
			data = string(e.Data)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(flat,at,level,kind,message,data) VALUES(?,?,?,?,?,?)`,
			f.Slug, unix(e.Time), e.Level, e.Kind, e.Message, data); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const flatCols = `slug,name,visibility,live_version,created_at,updated_at,old_slug,old_slug_until`

func scanFlat(sc interface{ Scan(...any) error }) (Flat, error) {
	var f Flat
	var vis string
	var created, updated int64
	var old sql.NullString
	var till sql.NullInt64
	if err := sc.Scan(&f.Slug, &f.Name, &vis, &f.LiveVersion, &created, &updated, &old, &till); err != nil {
		return f, err
	}
	f.Visibility = Visibility(vis)
	f.CreatedAt, f.UpdatedAt = fromUnix(created), fromUnix(updated)
	f.OldSlug = old.String
	if till.Valid {
		t := fromUnix(till.Int64)
		f.OldSlugTill = &t
	}
	return f, nil
}

// GetFlat returns one flat.
func (s *Store) GetFlat(ctx context.Context, slug string) (Flat, error) {
	f, err := scanFlat(s.db.QueryRowContext(ctx, `SELECT `+flatCols+` FROM flats WHERE slug=?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return f, fmt.Errorf("flat %q: %w", slug, ErrNotFound)
	}
	return f, err
}

// FlatByOldSlug returns the flat that was renamed away from old, if the
// redirect window is still open. Redirects follow later renames.
func (s *Store) FlatByOldSlug(ctx context.Context, old string, now time.Time) (Flat, error) {
	r, err := s.RedirectFor(ctx, old, now)
	if err != nil {
		return Flat{}, err
	}
	return s.GetFlat(ctx, r.Flat)
}

// ListFlats returns all flats ordered by most recently updated.
func (s *Store) ListFlats(ctx context.Context) ([]Flat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+flatCols+` FROM flats ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Flat
	for rows.Next() {
		f, err := scanFlat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpdateFlat writes name, visibility and live version.
func (s *Store) UpdateFlat(ctx context.Context, f Flat) error {
	res, err := s.db.ExecContext(ctx, `UPDATE flats SET name=?,visibility=?,live_version=?,updated_at=? WHERE slug=?`,
		f.Name, string(f.Visibility), f.LiveVersion, unix(f.UpdatedAt), f.Slug)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("flat %q: %w", f.Slug, ErrNotFound)
	}
	return nil
}

// Touch bumps updated_at.
func (s *Store) Touch(ctx context.Context, slug string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE flats SET updated_at=? WHERE slug=?`, unix(now), slug)
	return err
}

// RenameFlat changes a slug. When till is not zero, the old slug is recorded
// as a redirect to the flat until till. Earlier redirects follow the flat;
// a redirect from the new slug itself is dropped.
func (s *Store) RenameFlat(ctx context.Context, from, to string, till, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM redirects WHERE old=?`, to); err != nil {
		return err
	}
	q, args := `UPDATE flats SET slug=?, updated_at=? WHERE slug=?`, []any{to, unix(now), from}
	if !till.IsZero() {
		q, args = `UPDATE flats SET slug=?, old_slug=?, old_slug_until=?, updated_at=? WHERE slug=?`, []any{to, from, unix(till), unix(now), from}
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("flat %q %w", to, ErrExists)
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("flat %q: %w", from, ErrNotFound)
	}
	if !till.IsZero() {
		if _, err := tx.ExecContext(ctx, `INSERT INTO redirects(old,flat,until) VALUES(?,?,?) ON CONFLICT(old) DO UPDATE SET flat=excluded.flat, until=excluded.until`, from, to, unix(till)); err != nil {
			return err
		}
	}
	for _, q := range []string{`UPDATE events SET flat=? WHERE flat=?`, `UPDATE approvals SET flat=? WHERE flat=?`, `UPDATE pageviews SET flat=? WHERE flat=?`} {
		if _, err := tx.ExecContext(ctx, q, to, from); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteFlat removes a flat and everything attached to it.
func (s *Store) DeleteFlat(ctx context.Context, slug string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM flats WHERE slug=?`, slug)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("flat %q: %w", slug, ErrNotFound)
	}
	for _, q := range []string{`DELETE FROM events WHERE flat=?`, `DELETE FROM pageviews WHERE flat=?`} {
		if _, err := tx.ExecContext(ctx, q, slug); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- redirects ---

// Redirect sends an old slug to the flat that now owns its addresses.
type Redirect struct {
	Old   string    `json:"old"`
	Flat  string    `json:"flat"`
	Until time.Time `json:"until"`
}

// RedirectFor returns the open redirect from old.
func (s *Store) RedirectFor(ctx context.Context, old string, now time.Time) (Redirect, error) {
	r := Redirect{Old: old}
	var until int64
	err := s.db.QueryRowContext(ctx, `SELECT flat,until FROM redirects WHERE old=? AND until>?`, old, unix(now)).Scan(&r.Flat, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("redirect from %q: %w", old, ErrNotFound)
	}
	r.Until = fromUnix(until)
	return r, err
}

// ActiveRedirects returns every redirect whose window is still open.
func (s *Store) ActiveRedirects(ctx context.Context, now time.Time) ([]Redirect, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT old,flat,until FROM redirects WHERE until>? ORDER BY old`, unix(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Redirect
	for rows.Next() {
		var r Redirect
		var until int64
		if err := rows.Scan(&r.Old, &r.Flat, &until); err != nil {
			return nil, err
		}
		r.Until = fromUnix(until)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteExpiredRedirects removes redirects whose window has closed.
func (s *Store) DeleteExpiredRedirects(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM redirects WHERE until<=?`, unix(now))
	return err
}

// --- versions ---

// NextVersionNumber returns the number the next saved version will get.
func (s *Store) NextVersionNumber(ctx context.Context, flat string) (int, error) {
	var n sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(number) FROM versions WHERE flat=?`, flat).Scan(&n); err != nil {
		return 0, err
	}
	return int(n.Int64) + 1, nil
}

// InsertVersion records a saved version.
func (s *Store) InsertVersion(ctx context.Context, v Version) error {
	published := 0
	if v.Published {
		published = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO versions(flat,number,hash,size,files,kind,manifest,git_sha,git_dirty,message,created_at,screenshot,published) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		v.Flat, v.Number, v.Hash, v.Size, v.Files, v.Kind, string(v.Manifest), v.GitSHA, v.GitDirty, v.Message, unix(v.CreatedAt), v.Screenshot, published)
	return err
}

const versionCols = `flat,number,hash,size,files,kind,manifest,git_sha,git_dirty,message,created_at,screenshot,pruned,published`

func scanVersion(sc interface{ Scan(...any) error }) (Version, error) {
	var v Version
	var manifest string
	var git, msg, shot sql.NullString
	var created int64
	var published int
	if err := sc.Scan(&v.Flat, &v.Number, &v.Hash, &v.Size, &v.Files, &v.Kind, &manifest, &git, &v.GitDirty, &msg, &created, &shot, &v.Pruned, &published); err != nil {
		return v, err
	}
	v.Manifest = json.RawMessage(manifest)
	v.GitSHA, v.Message, v.Screenshot = git.String, msg.String, shot.String
	v.CreatedAt = fromUnix(created)
	v.Published = published == 1
	if v.Published {
		v.Role = "published"
	}
	return v, nil
}

// GetVersion returns one published version.
func (s *Store) GetVersion(ctx context.Context, flat string, n int) (Version, error) {
	v, err := scanVersion(s.db.QueryRowContext(ctx, `SELECT `+versionCols+` FROM versions WHERE flat=? AND number=? AND published=1`, flat, n))
	if errors.Is(err, sql.ErrNoRows) {
		return v, fmt.Errorf("version %d of %q: %w", n, flat, ErrNotFound)
	}
	return v, err
}

// ListVersions returns published versions of a flat, newest first.
func (s *Store) ListVersions(ctx context.Context, flat string) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+versionCols+` FROM versions WHERE flat=? AND published=1 ORDER BY number DESC`, flat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// MarkPruned flags a version whose files were removed by retention.
func (s *Store) MarkPruned(ctx context.Context, flat string, n int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE versions SET pruned=1 WHERE flat=? AND number=?`, flat, n)
	return err
}

// --- deployments ---

// SetLive atomically records a deployment and moves the live pointer.
func (s *Store) SetLive(ctx context.Context, flat string, version, previous int, kind, approvalID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE flats SET live_version=?, updated_at=? WHERE slug=? AND live_version=?`, version, unix(now), flat, previous)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("flat %q changed concurrently; retry", flat)
	}
	var approval any
	if approvalID != "" {
		approval = approvalID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO deployments(flat,version,previous,kind,at,approval_id) VALUES(?,?,?,?,?,?)`, flat, version, previous, kind, unix(now), approval); err != nil {
		return err
	}
	return tx.Commit()
}

// Deployment is a row of the deploy history.
type Deployment struct {
	Version    int       `json:"version"`
	Previous   int       `json:"previous"`
	Kind       string    `json:"kind"` // deploy | publish | rollback
	At         time.Time `json:"at"`
	ApprovalID string    `json:"approval_id,omitempty"`
}

// ListDeployments returns the deploy history, newest first.
func (s *Store) ListDeployments(ctx context.Context, flat string, limit int) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version,previous,kind,at,approval_id FROM deployments WHERE flat=? ORDER BY id DESC LIMIT ?`, flat, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		var d Deployment
		var at int64
		var approval sql.NullString
		if err := rows.Scan(&d.Version, &d.Previous, &d.Kind, &at, &approval); err != nil {
			return nil, err
		}
		d.At = fromUnix(at)
		d.ApprovalID = approval.String
		out = append(out, d)
	}
	return out, rows.Err()
}

// --- events ---

// AddEvent appends a log event.
func (s *Store) AddEvent(ctx context.Context, e Event) (int64, error) {
	var data any
	if len(e.Data) > 0 {
		data = string(e.Data)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO events(flat,at,level,kind,message,data) VALUES(?,?,?,?,?,?)`,
		e.Flat, unix(e.Time), e.Level, e.Kind, e.Message, data)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListEvents returns up to limit events for flat with id > after, oldest first
// when after > 0, otherwise the newest limit events in chronological order.
func (s *Store) ListEvents(ctx context.Context, flat, kind string, after int64, limit int) ([]Event, error) {
	q := `SELECT id,flat,at,level,kind,message,data FROM events WHERE flat=?`
	args := []any{flat}
	if kind != "" {
		q += ` AND kind=?`
		args = append(args, kind)
	}
	if after > 0 {
		q += ` AND id>? ORDER BY id ASC LIMIT ?`
		args = append(args, after, limit)
	} else {
		q = `SELECT * FROM (` + q + ` ORDER BY id DESC LIMIT ?) ORDER BY id ASC`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		var data sql.NullString
		if err := rows.Scan(&e.ID, &e.Flat, &at, &e.Level, &e.Kind, &e.Message, &data); err != nil {
			return nil, err
		}
		e.Time = fromUnix(at)
		if data.Valid {
			e.Data = json.RawMessage(data.String)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents keeps the newest keep events of flat and returns how many
// were deleted.
func (s *Store) PruneEvents(ctx context.Context, flat string, keep int) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE `+prunedEvents, prunedEventsArgs(flat, keep)...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PrunableEvents counts the events PruneEvents would delete with keep.
func (s *Store) PrunableEvents(ctx context.Context, flat string, keep int) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE `+prunedEvents, prunedEventsArgs(flat, keep)...).Scan(&n)
	return n, err
}

// prunedEvents selects the events of a flat older than its newest keep.
const prunedEvents = `flat=? AND id <= (SELECT id FROM events WHERE flat=? ORDER BY id DESC LIMIT 1 OFFSET ?)`

func prunedEventsArgs(flat string, keep int) []any {
	return []any{flat, flat, max(keep, 0)}
}

// EventFlats returns every flat name that has events.
func (s *Store) EventFlats(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT flat FROM events`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// --- approvals ---

// InsertApproval records a pending approval.
func (s *Store) InsertApproval(ctx context.Context, a Approval) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO approvals(id,flat,action,params,status,via,reason,requested_at,idempotency_key) VALUES(?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Flat, a.Action, string(a.Params), a.Status, a.Via, a.Reason, unix(a.RequestedAt), nullString(a.IdempotencyKey))
	return err
}

func scanApproval(sc interface{ Scan(...any) error }) (Approval, error) {
	var a Approval
	var params string
	var reason, result, idem, data, actor sql.NullString
	var req int64
	var dec, authorized sql.NullInt64
	if err := sc.Scan(&a.ID, &a.Flat, &a.Action, &params, &a.Status, &a.Via, &reason, &result, &req, &dec, &idem, &data, &actor, &authorized); err != nil {
		return a, err
	}
	a.Params = json.RawMessage(params)
	a.ResultData = json.RawMessage(data.String)
	a.DecidedBy = actor.String
	if authorized.Valid {
		at := fromUnix(authorized.Int64)
		a.AuthorizedAt = &at
	}
	a.Reason, a.Result, a.IdempotencyKey = reason.String, result.String, idem.String
	a.RequestedAt = fromUnix(req)
	if dec.Valid {
		t := fromUnix(dec.Int64)
		a.DecidedAt = &t
	}
	return a, nil
}

const approvalCols = `id,flat,action,params,status,via,reason,result,requested_at,decided_at,idempotency_key,result_data,decided_by,authorized_at`

// GetApproval returns one approval.
func (s *Store) GetApproval(ctx context.Context, id string) (Approval, error) {
	a, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM approvals WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("approval %q: %w", id, ErrNotFound)
	}
	return a, err
}

// ListApprovals returns approvals with the given status ("" for all), newest first.
func (s *Store) ListApprovals(ctx context.Context, status string) ([]Approval, error) {
	q := `SELECT ` + approvalCols + ` FROM approvals`
	var args []any
	if status != "" {
		q += ` WHERE status=?`
		args = append(args, status)
	}
	q += ` ORDER BY requested_at DESC LIMIT 200`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideApproval moves a pending approval to a final status. It fails if the
// approval is no longer pending.
func (s *Store) DecideApproval(ctx context.Context, id, status, result string, now time.Time) error {
	return s.moveApproval(ctx, id, "pending", status, result, now)
}

// ClaimApproval moves a pending approval to "applying", so exactly one
// decision can act on it. It fails if the approval is no longer pending.
func (s *Store) ClaimApproval(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status='applying' WHERE id=? AND status='pending'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("approval %q is not pending", id)
	}
	return nil
}

// FinishApproval records the outcome of a claimed approval.
func (s *Store) FinishApproval(ctx context.Context, id, status, result string, now time.Time) error {
	return s.moveApproval(ctx, id, "applying", status, result, now)
}

func (s *Store) moveApproval(ctx context.Context, id, from, status, result string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status=?, result=?, decided_at=? WHERE id=? AND status=?`, status, result, unix(now), id, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("approval %q is not %s", id, from)
	}
	return nil
}

// --- previews ---

// InsertPreview records a preview address.
func (s *Store) InsertPreview(ctx context.Context, p Preview) error {
	target := p.Target
	if target == "" {
		target = "version"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO previews(host,flat,version,created_at,last_access,target,revision) VALUES(?,?,?,?,?,?,?)`,
		p.Host, p.Flat, p.Version, unix(p.CreatedAt), unix(p.LastAccess), target, p.Revision)
	return err
}

// ListPreviews returns previews of flat ("" for all).
func (s *Store) ListPreviews(ctx context.Context, flat string) ([]Preview, error) {
	q := `SELECT host,flat,version,created_at,last_access,target,revision FROM previews`
	var args []any
	if flat != "" {
		q += ` WHERE flat=?`
		args = append(args, flat)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Preview
	for rows.Next() {
		var p Preview
		var c, l int64
		if err := rows.Scan(&p.Host, &p.Flat, &p.Version, &c, &l, &p.Target, &p.Revision); err != nil {
			return nil, err
		}
		p.CreatedAt, p.LastAccess = fromUnix(c), fromUnix(l)
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPreview returns one preview.
func (s *Store) GetPreview(ctx context.Context, host string) (Preview, error) {
	var p Preview
	var c, l int64
	err := s.db.QueryRowContext(ctx, `SELECT host,flat,version,created_at,last_access,target,revision FROM previews WHERE host=?`, host).Scan(&p.Host, &p.Flat, &p.Version, &c, &l, &p.Target, &p.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("preview %q: %w", host, ErrNotFound)
	}
	p.CreatedAt, p.LastAccess = fromUnix(c), fromUnix(l)
	return p, err
}

// TouchPreview updates last access.
func (s *Store) TouchPreview(ctx context.Context, host string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE previews SET last_access=? WHERE host=?`, unix(now), host)
	return err
}

// DeletePreview removes one preview row.
func (s *Store) DeletePreview(ctx context.Context, host string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM previews WHERE host=?`, host)
	return err
}

// --- settings ---

// GetSetting returns a setting or def when unset.
func (s *Store) GetSetting(ctx context.Context, key, def string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	return v, err
}

// SetSetting writes a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// AllSettings returns every stored setting.
func (s *Store) AllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// --- page views ---

// AddPageViews increments the per-day counter.
func (s *Store) AddPageViews(ctx context.Context, flat, day string, n int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pageviews(flat,day,count) VALUES(?,?,?) ON CONFLICT(flat,day) DO UPDATE SET count=count+excluded.count`, flat, day, n)
	return err
}

// DayCount is one day of page views.
type DayCount struct {
	Day   string `json:"day"`
	Count int64  `json:"count"`
}

// PageViews returns the most recent days of page views, oldest first.
func (s *Store) PageViews(ctx context.Context, flat string, days int) ([]DayCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT day,count FROM (SELECT day,count FROM pageviews WHERE flat=? ORDER BY day DESC LIMIT ?) ORDER BY day ASC`, flat, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayCount
	for rows.Next() {
		var d DayCount
		if err := rows.Scan(&d.Day, &d.Count); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// --- secrets ---

// SealedSecret is an encrypted secret value.
type SealedSecret struct {
	Name       string
	Nonce      []byte
	Ciphertext []byte
	UpdatedAt  time.Time
}

// PutSecret stores an encrypted secret.
func (s *Store) PutSecret(ctx context.Context, flat string, sec SealedSecret) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO secrets(flat,name,nonce,ciphertext,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(flat,name) DO UPDATE SET nonce=excluded.nonce, ciphertext=excluded.ciphertext, updated_at=excluded.updated_at`,
		flat, sec.Name, sec.Nonce, sec.Ciphertext, unix(sec.UpdatedAt))
	return envWriteError(err)
}

// DeleteSecret removes a secret.
func (s *Store) DeleteSecret(ctx context.Context, flat, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE flat=? AND name=?`, flat, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("secret %q: %w", name, ErrNotFound)
	}
	return nil
}

// ListSecrets returns sealed secrets of a flat.
func (s *Store) ListSecrets(ctx context.Context, flat string) ([]SealedSecret, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name,nonce,ciphertext,updated_at FROM secrets WHERE flat=? ORDER BY name`, flat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SealedSecret
	for rows.Next() {
		var sec SealedSecret
		var u int64
		if err := rows.Scan(&sec.Name, &sec.Nonce, &sec.Ciphertext, &u); err != nil {
			return nil, err
		}
		sec.UpdatedAt = fromUnix(u)
		out = append(out, sec)
	}
	return out, rows.Err()
}
