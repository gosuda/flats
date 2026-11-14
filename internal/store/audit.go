package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// The audit log of docs/spec/audit-events.md: an append-only table that no flat deletion, rename or trimming
// touches. Every change of a sealed value writes its event in the transaction of the change.

// Kinds of audit events (rules 8 to 15 of the spec).
const (
	AuditSecretSet    = "secret.set"
	AuditSecretDelete = "secret.delete"
	AuditKeySet       = "key.set"
	AuditKeyDelete    = "key.delete"
	AuditKeyBackup    = "key.backup"
	AuditKeyRestore   = "key.restore"
	AuditKeyReset     = "key.reset"
	AuditKeyRotate    = "key.rotate" // reserved: nothing writes it yet
)

// Channels of audit events (rule 4).
const (
	AuditViaConsole    = "console"
	AuditViaCLI        = "cli"
	AuditViaCommand    = "command"
	AuditViaStart      = "start"
	AuditViaMigration  = "migration"
	AuditViaFlatDelete = "flat-delete"
)

// Limits of one row (rule 7).
const (
	MaxAuditActor = 512
	MaxAuditData  = 4096
)

// Audit says how, by whom and why a sealed value changed. The functions that change one take it, so that no change
// is made without saying how. The zero value is allowed and records an empty channel.
type Audit struct {
	Time     time.Time // zero means now
	Via      string    // one of the AuditVia constants
	Actor    string    // the identity that the channel knows, or empty
	From     string    // key.set of a start or a migration: flag, env or config
	Reason   string    // key.delete: removed (the default) or connected
	Approval string    // secret.delete of a flat deletion: the approval that deleted the flat
}

// AuditEvent is one row of audit_events.
type AuditEvent struct {
	ID     int64
	Time   time.Time
	Kind   string
	Flat   string // the slug of the flat when the event was written
	FlatID string
	Name   string
	Via    string
	Actor  string
	Data   json.RawMessage
}

// addAuditEvents creates the audit log.
func addAuditEvents(ctx context.Context, x executor) error {
	_, err := x.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS audit_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at INTEGER NOT NULL,
  kind TEXT NOT NULL,
  flat TEXT NOT NULL DEFAULT '',
  flat_id TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  via TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT '',
  data TEXT
);
CREATE INDEX IF NOT EXISTS audit_events_kind ON audit_events(kind, id);
CREATE INDEX IF NOT EXISTS audit_events_flat ON audit_events(flat, id);
CREATE INDEX IF NOT EXISTS audit_events_flat_id ON audit_events(flat_id, id);`)
	return err
}

// AuditData encodes the fields of an event, or nothing when there are none.
func AuditData(fields map[string]any) json.RawMessage {
	if len(fields) == 0 {
		return nil
	}
	b, _ := json.Marshal(fields)
	return b
}

// cutActor shortens actor to MaxAuditActor bytes on a character boundary.
func cutActor(actor string) string {
	if len(actor) <= MaxAuditActor {
		return actor
	}
	cut := MaxAuditActor
	for cut > 0 && !utf8.RuneStart(actor[cut]) {
		cut--
	}
	return actor[:cut]
}

// appendAudit writes one event through x, which is the transaction of the change when there is one.
func appendAudit(ctx context.Context, x executor, e AuditEvent) error {
	if len(e.Data) > MaxAuditData {
		return fmt.Errorf("audit event %s: the data is %d bytes, at most %d", e.Kind, len(e.Data), MaxAuditData)
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	var data any
	if len(e.Data) > 0 {
		data = string(e.Data)
	}
	_, err := x.ExecContext(ctx, `INSERT INTO audit_events(at,kind,flat,flat_id,name,via,actor,data) VALUES(?,?,?,?,?,?,?,?)`,
		unix(e.Time), e.Kind, e.Flat, e.FlatID, e.Name, e.Via, cutActor(e.Actor), data)
	return err
}

// event starts an event of this audit with its time, channel and identity.
func (a Audit) event(kind string) AuditEvent {
	t := a.Time
	if t.IsZero() {
		t = time.Now()
	}
	return AuditEvent{Time: t, Kind: kind, Via: a.Via, Actor: a.Actor}
}

// flatUID returns the ID of the flat with slug, or "" when there is none.
func flatUID(ctx context.Context, x executor, slug string) (string, error) {
	var uid sql.NullString
	err := x.QueryRowContext(ctx, `SELECT uid FROM flats WHERE slug=?`, slug).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return uid.String, err
}

// AppendAudit writes an event that is not part of a change of flats.db (key.backup and key.restore). The commands
// that call it hold the host lock or work beside the host, and a single insert is atomic.
func (s *Store) AppendAudit(ctx context.Context, e AuditEvent) error {
	return appendAudit(ctx, s.db, e)
}

// ErrAuditSchema reports that flats.db is not of the schema of this binary, so an event cannot be written or read
// without a migration that the caller must not run.
var ErrAuditSchema = errors.New("the database is not of the schema that this Flats expects")

// AppendAuditBeside appends an event to the flats.db that a host may be using, without the host lock and without
// migrating it (rule 18). One BEGIN IMMEDIATE transaction reads user_version, checks it and inserts the row, so a
// host that starts and migrates meanwhile cannot interleave. It never creates flats.db.
func AppendAuditBeside(ctx context.Context, path string, e AuditEvent) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("append the audit event: %w", err)
	}
	db, err := sql.Open("sqlite", fileURI(path)+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("append the audit event: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("append the audit event: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		}
	}()
	var version int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version != latestVersion() {
		return fmt.Errorf("append the audit event: %w: version %d, expected %d", ErrAuditSchema, version, latestVersion())
	}
	if err := appendAudit(ctx, conn, e); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// AuditReader reads the audit log of a flats.db that a host may be using, without writing a row.
type AuditReader struct {
	db *sql.DB
}

// OpenAuditReader opens path read-only for `flats audit ls` (rule 22). The open is a plain read-only one, which
// sees the write-ahead log, whenever a -wal or -journal file exists, whatever its size, and it is immutable only when
// neither exists. It never migrates. It fails with fs.ErrNotExist for a missing database and with ErrAuditSchema,
// or ErrSchemaTooNew, for another schema version.
func OpenAuditReader(path string) (*AuditReader, error) {
	info, err := Inspect(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if !info.Exists || info.New {
		return nil, fmt.Errorf("open %s: %w", path, fs.ErrNotExist)
	}
	if info.Version != info.Latest {
		return nil, fmt.Errorf("open %s: %w: version %d, expected %d", path, ErrAuditSchema, info.Version, info.Latest)
	}
	query := "?mode=ro&immutable=1"
	for _, side := range []string{path + "-wal", path + "-journal"} {
		if _, err := os.Stat(side); err == nil {
			query = "?mode=ro"
		}
	}
	db, err := sql.Open("sqlite", fileURI(path)+query+"&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &AuditReader{db: db}, nil
}

// Close closes the reader.
func (r *AuditReader) Close() error { return r.db.Close() }

// AuditQuery selects events. FlatID wins over Flat, which matches the slug that was recorded.
type AuditQuery struct {
	Since  time.Time
	Kind   string
	Flat   string
	FlatID string
}

func (q AuditQuery) where() (string, []any) {
	var conds []string
	var args []any
	if !q.Since.IsZero() {
		conds = append(conds, `at>=?`)
		args = append(args, unix(q.Since))
	}
	if q.Kind != "" {
		conds = append(conds, `kind=?`)
		args = append(args, q.Kind)
	}
	switch {
	case q.FlatID != "":
		conds = append(conds, `flat_id=?`)
		args = append(args, q.FlatID)
	case q.Flat != "":
		conds = append(conds, `flat=?`)
		args = append(args, q.Flat)
	}
	if len(conds) == 0 {
		return "1=1", nil
	}
	return strings.Join(conds, " AND "), args
}

// FlatID returns the ID of the flat that has slug now, and whether there is one.
func (r *AuditReader) FlatID(ctx context.Context, slug string) (string, bool, error) {
	var uid sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT uid FROM flats WHERE slug=?`, slug).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return uid.String, err == nil && uid.String != "", err
}

// Start returns the id before the first of the newest limit events that match q, so that the events with a larger id
// are the ones to list. A limit of 0 or less, or fewer matches than the limit, gives 0.
func (r *AuditReader) Start(ctx context.Context, q AuditQuery, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	where, args := q.where()
	var id int64
	err := r.db.QueryRowContext(ctx, `SELECT id FROM audit_events WHERE `+where+` ORDER BY id DESC LIMIT 1 OFFSET ?`,
		append(args, limit-1)...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id - 1, err
}

// List returns up to limit events that match q with an id larger than after, oldest first. Each call is its own read
// transaction, so a caller that writes the page to a slow output holds none.
func (r *AuditReader) List(ctx context.Context, q AuditQuery, after int64, limit int) ([]AuditEvent, error) {
	where, args := q.where()
	rows, err := r.db.QueryContext(ctx, `SELECT id,at,kind,flat,flat_id,name,via,actor,data FROM audit_events WHERE id>? AND `+where+` ORDER BY id ASC LIMIT ?`,
		append(append([]any{after}, args...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		var at int64
		var data sql.NullString
		if err := rows.Scan(&e.ID, &at, &e.Kind, &e.Flat, &e.FlatID, &e.Name, &e.Via, &e.Actor, &data); err != nil {
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
