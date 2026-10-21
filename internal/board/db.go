package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gosuda/flats/internal/board/engine"
	_ "modernc.org/sqlite"
)

const dbName = "board.db"

// busyTimeoutMS is how long a board.db statement waits for a lock.
const busyTimeoutMS = 5000

// schema is board.db (5.1). Only the apply loop writes it.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS elements(id TEXT PRIMARY KEY, inc INTEGER NOT NULL, data TEXT NOT NULL,
		v TEXT NOT NULL, deleted INTEGER NOT NULL, del_seq INTEGER NOT NULL, pre_rel TEXT, end_inc TEXT,
		end_last TEXT, bucket INTEGER NOT NULL, bytes INTEGER NOT NULL, del_key TEXT)`,
	`CREATE TABLE IF NOT EXISTS buckets(bucket INTEGER PRIMARY KEY, dirty_seq INTEGER, bytes INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS deltas(seq INTEGER PRIMARY KEY, from_session TEXT NOT NULL, n INTEGER NOT NULL,
		by TEXT NOT NULL, changes TEXT NOT NULL, at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY, key TEXT NOT NULL, label TEXT NOT NULL,
		verified INTEGER NOT NULL, max_n INTEGER NOT NULL, last_at INTEGER NOT NULL, start_seq INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS created(session TEXT NOT NULL, id TEXT NOT NULL, inc INTEGER NOT NULL,
		PRIMARY KEY(session, id))`,
	`CREATE TABLE IF NOT EXISTS receipts(session TEXT NOT NULL, n INTEGER NOT NULL, seq INTEGER NOT NULL,
		results TEXT NOT NULL, PRIMARY KEY(session, n))`,
	// fixed marks the guests' group, which closes only on fixed windows;
	// names lists the guests who edited since the last entry.
	`CREATE TABLE IF NOT EXISTS groups(editor TEXT PRIMARY KEY, label TEXT NOT NULL, started_at INTEGER NOT NULL,
		last_at INTEGER NOT NULL, dirty INTEGER NOT NULL, fixed INTEGER NOT NULL DEFAULT 0, names TEXT NOT NULL DEFAULT '[]')`,
	`CREATE TABLE IF NOT EXISTS undo(session TEXT NOT NULL, stack TEXT NOT NULL, idx INTEGER NOT NULL,
		data TEXT NOT NULL, bytes INTEGER NOT NULL, PRIMARY KEY(session, stack, idx))`,
	// snaps lists the snapshot hashes the entry added (one reference each).
	`CREATE TABLE IF NOT EXISTS outbox(seq INTEGER PRIMARY KEY AUTOINCREMENT, created_at INTEGER NOT NULL,
		kind TEXT NOT NULL, authors TEXT NOT NULL, message TEXT NOT NULL, rollback_of INTEGER, op TEXT,
		files TEXT NOT NULL, snaps TEXT NOT NULL, guests TEXT NOT NULL DEFAULT '[]', guests_more INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS snapshots(hash TEXT PRIMARY KEY, data BLOB NOT NULL, refs INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS assets(hash TEXT PRIMARY KEY, mime TEXT NOT NULL, w INTEGER NOT NULL,
		h INTEGER NOT NULL, bytes INTEGER NOT NULL, refs INTEGER NOT NULL, deleted_refs INTEGER NOT NULL,
		held_until INTEGER NOT NULL)`,
	// imported_at: when the op's outbox entry was imported (or the op
	// recorded none); the row is kept 24 hours after it.
	`CREATE TABLE IF NOT EXISTS ops(id TEXT PRIMARY KEY, kind TEXT NOT NULL, request_hash TEXT NOT NULL,
		outbox_seq INTEGER NOT NULL, result TEXT NOT NULL, at INTEGER NOT NULL, imported_at INTEGER)`,
}

// addedColumns are the columns later versions added to existing tables
// (anonymous public editing). openStore adds the ones an older board.db
// lacks; the defaults describe a board without guests.
var addedColumns = []struct{ table, column, def string }{
	{"elements", "del_key", "TEXT"},
	{"sessions", "start_seq", "INTEGER NOT NULL DEFAULT 0"},
	{"groups", "fixed", "INTEGER NOT NULL DEFAULT 0"},
	{"groups", "names", "TEXT NOT NULL DEFAULT '[]'"},
	{"outbox", "guests", "TEXT NOT NULL DEFAULT '[]'"},
	{"outbox", "guests_more", "INTEGER NOT NULL DEFAULT 0"},
}

// addColumns adds the addedColumns an older board.db lacks.
func addColumns(ctx context.Context, db *sql.DB) error {
	for _, c := range addedColumns {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, c.table, c.column, c.def)); err != nil {
				return err
			}
		}
	}
	return nil
}

type store struct {
	db       *sql.DB
	path     string
	pageSize int64
}

// openStore opens board.db with WAL, synchronous=FULL and incremental
// auto-vacuum, creating the file and the schema as needed. fresh reports
// that the board was never initialized: the file is new, or an earlier
// creation stopped after the schema but before initialize committed (its
// one transaction writes the store id, so a missing store id means nothing
// of it is durable).
func openStore(path string) (s *store, fresh bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	u := url.URL{Scheme: "file", Path: path}
	dsn := u.String() + fmt.Sprintf("?_pragma=busy_timeout(%d)&_pragma=synchronous(FULL)&_txlock=immediate", busyTimeoutMS)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, false, err
	}
	// One connection: the apply loop is the only user, so a failed
	// truncation is never caused by a reader of our own.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	s = &store{db: db, path: path}
	ctx := context.Background()
	fail := func(err error) (*store, bool, error) {
		db.Close()
		return nil, false, err
	}
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master`).Scan(&tables); err != nil {
		return fail(fmt.Errorf("board.db: %w", err))
	}
	if tables == 0 {
		// auto_vacuum must be chosen before the first table exists.
		if _, err := db.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
			return fail(err)
		}
	}
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&mode); err != nil || mode != "wal" {
		return fail(fmt.Errorf("board.db: journal mode %q: %v", mode, err))
	}
	if tables > 0 {
		var res string
		if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&res); err != nil || res != "ok" {
			return fail(fmt.Errorf("board.db integrity check: %q %v", res, err))
		}
	}
	for _, q := range schema {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fail(fmt.Errorf("board.db schema: %w", err))
		}
	}
	if err := addColumns(ctx, db); err != nil {
		return fail(fmt.Errorf("board.db schema: %w", err))
	}
	if err := db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&s.pageSize); err != nil {
		return fail(err)
	}
	var store string
	switch err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'store'`).Scan(&store); {
	case errors.Is(err, sql.ErrNoRows):
		fresh = true
	case err != nil:
		return fail(err)
	}
	return s, fresh, nil
}

func (s *store) close() { s.db.Close() }

// walPath is the write-ahead log of board.db.
func (s *store) walPath() string { return s.path + "-wal" }

// pages returns page_count − freelist_count inside tx.
func pages(ctx context.Context, tx *sql.Tx) (int64, error) {
	var count, free int64
	if err := tx.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&count); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return 0, err
	}
	return count - free, nil
}

func setMeta(ctx context.Context, tx *sql.Tx, key string, v any) error {
	var s string
	switch v := v.(type) {
	case string:
		s = v
	case int64:
		s = strconv.FormatInt(v, 10)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		s = string(b)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, s)
	return err
}

func nullJSON(v any, empty bool) any {
	if empty {
		return nil
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// persist writes every row c names from the state: present rows are
// upserted, missing ones deleted. Buckets are marked dirty by the caller.
func persist(ctx context.Context, tx *sql.Tx, st *engine.State, c engine.Changed) error {
	for _, id := range c.Elements {
		r, ok := st.Element(id)
		if !ok {
			if _, err := tx.ExecContext(ctx, `DELETE FROM elements WHERE id = ?`, id); err != nil {
				return err
			}
			continue
		}
		v, _ := json.Marshal(r.V)
		preRel := nullJSON(r.PreRel, r.PreRel == nil)
		endInc := nullJSON(r.EndInc, r.EndInc == [2]int64{})
		endLast := nullJSON(r.EndLast, r.EndLast == [2][2]float64{})
		bytes := int64(len(r.Data) + len(v) + len(id) + 48)
		for _, x := range []any{preRel, endInc, endLast} {
			if s, ok := x.(string); ok {
				bytes += int64(len(s))
			}
		}
		var delKey any
		if r.DelKey != "" {
			delKey = r.DelKey
			bytes += int64(len(r.DelKey))
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO elements(id, inc, data, v, deleted, del_seq, pre_rel, end_inc, end_last, bucket, bytes, del_key)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, r.Inc, string(r.Data), string(v), r.Deleted, r.DelSeq, preRel, endInc, endLast, r.Bucket, bytes, delKey); err != nil {
			return err
		}
	}
	for _, k := range c.Created {
		inc, ok := st.CreatedInc(k)
		var err error
		if ok {
			_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO created(session, id, inc) VALUES(?, ?, ?)`, k.Session, k.ID, inc)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM created WHERE session = ? AND id = ?`, k.Session, k.ID)
		}
		if err != nil {
			return err
		}
	}
	for _, k := range c.Undo {
		u, ok := st.UndoRow(k)
		var err error
		if ok {
			data, _ := json.Marshal(u.Entry)
			_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO undo(session, stack, idx, data, bytes) VALUES(?, ?, ?, ?, ?)`,
				k.Session, k.Stack, k.Idx, string(data), len(data))
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM undo WHERE session = ? AND stack = ? AND idx = ?`, k.Session, k.Stack, k.Idx)
		}
		if err != nil {
			return err
		}
	}
	for _, id := range c.Sessions {
		se, ok := st.SessionRow(id)
		var err error
		if ok {
			_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO sessions(id, key, label, verified, max_n, last_at, start_seq) VALUES(?, ?, ?, ?, ?, ?, ?)`,
				se.ID, se.Key, se.Label, se.Verified, se.MaxN, se.LastAt, se.StartSeq)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
		}
		if err != nil {
			return err
		}
	}
	for _, k := range c.Receipts {
		rc, ok := st.ReceiptRow(k)
		var err error
		if ok {
			data, _ := json.Marshal(receiptData{Results: rc.Results, Reject: rc.Reject})
			_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO receipts(session, n, seq, results) VALUES(?, ?, ?, ?)`,
				k.Session, k.N, rc.Seq, string(data))
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM receipts WHERE session = ? AND n = ?`, k.Session, k.N)
		}
		if err != nil {
			return err
		}
	}
	for _, h := range c.Assets {
		a, ok := st.AssetRow(h)
		var err error
		if ok {
			_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO assets(hash, mime, w, h, bytes, refs, deleted_refs, held_until) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
				a.Hash, a.Mime, a.W, a.H, a.Bytes, a.Refs, a.DeletedRefs, a.HeldUntil)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM assets WHERE hash = ?`, h)
		}
		if err != nil {
			return err
		}
	}
	if c.Meta {
		if err := setMeta(ctx, tx, "seq", st.Seq()); err != nil {
			return err
		}
		resets := st.Resets()
		if resets == nil {
			resets = []int64{}
		}
		at := st.ResetTimes()
		if at == nil {
			at = []int64{}
		}
		if err := setMeta(ctx, tx, "resets", resets); err != nil {
			return err
		}
		if err := setMeta(ctx, tx, "reset_at", at); err != nil {
			return err
		}
		if err := setMeta(ctx, tx, "reset_floor", st.ResetFloor()); err != nil {
			return err
		}
	}
	return nil
}

type receiptData struct {
	Results json.RawMessage `json:"results,omitempty"`
	Reject  *engine.Reject  `json:"reject,omitempty"`
}

// loadRows reads the engine rows and the metadata of board.db.
func (s *store) loadRows(ctx context.Context) (engine.Rows, map[string]string, error) {
	var rows engine.Rows
	meta := map[string]string{}
	q := func(query string, scan func(*sql.Rows) error) error {
		rs, err := s.db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			if err := scan(rs); err != nil {
				return err
			}
		}
		return rs.Err()
	}
	err := q(`SELECT key, value FROM meta`, func(r *sql.Rows) error {
		var k, v string
		if err := r.Scan(&k, &v); err != nil {
			return err
		}
		meta[k] = v
		return nil
	})
	if err != nil {
		return rows, nil, err
	}
	rows.Seq, _ = strconv.ParseInt(meta["seq"], 10, 64)
	if v := meta["resets"]; v != "" {
		if err := json.Unmarshal([]byte(v), &rows.Resets); err != nil {
			return rows, nil, fmt.Errorf("meta.resets: %w", err)
		}
	}
	if v := meta["reset_at"]; v != "" {
		if err := json.Unmarshal([]byte(v), &rows.ResetAt); err != nil {
			return rows, nil, fmt.Errorf("meta.reset_at: %w", err)
		}
	}
	rows.ResetFloor, _ = strconv.ParseInt(meta["reset_floor"], 10, 64)
	err = q(`SELECT id, inc, data, v, deleted, del_seq, pre_rel, end_inc, end_last, bucket, del_key FROM elements`, func(r *sql.Rows) error {
		var er engine.ElementRow
		var data, v string
		var preRel, endInc, endLast, delKey sql.NullString
		if err := r.Scan(&er.ID, &er.Inc, &data, &v, &er.Deleted, &er.DelSeq, &preRel, &endInc, &endLast, &er.Bucket, &delKey); err != nil {
			return err
		}
		er.DelKey = delKey.String
		er.Data = json.RawMessage(data)
		if err := json.Unmarshal([]byte(v), &er.V); err != nil {
			return fmt.Errorf("element %s versions: %w", er.ID, err)
		}
		for _, x := range []struct {
			s   sql.NullString
			dst any
		}{{preRel, &er.PreRel}, {endInc, &er.EndInc}, {endLast, &er.EndLast}} {
			if x.s.Valid {
				if err := json.Unmarshal([]byte(x.s.String), x.dst); err != nil {
					return fmt.Errorf("element %s: %w", er.ID, err)
				}
			}
		}
		rows.Elements = append(rows.Elements, er)
		return nil
	})
	if err != nil {
		return rows, nil, err
	}
	err = q(`SELECT id, key, label, verified, max_n, last_at, start_seq FROM sessions`, func(r *sql.Rows) error {
		var se engine.Session
		if err := r.Scan(&se.ID, &se.Key, &se.Label, &se.Verified, &se.MaxN, &se.LastAt, &se.StartSeq); err != nil {
			return err
		}
		rows.Sessions = append(rows.Sessions, se)
		return nil
	})
	if err != nil {
		return rows, nil, err
	}
	err = q(`SELECT session, id, inc FROM created`, func(r *sql.Rows) error {
		var c engine.CreatedRow
		if err := r.Scan(&c.Session, &c.ID, &c.Inc); err != nil {
			return err
		}
		rows.Created = append(rows.Created, c)
		return nil
	})
	if err != nil {
		return rows, nil, err
	}
	err = q(`SELECT session, n, seq, results FROM receipts`, func(r *sql.Rows) error {
		var rc engine.Receipt
		var data string
		if err := r.Scan(&rc.Session, &rc.N, &rc.Seq, &data); err != nil {
			return err
		}
		var d receiptData
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return fmt.Errorf("receipt %s:%d: %w", rc.Session, rc.N, err)
		}
		rc.Results, rc.Reject = d.Results, d.Reject
		rows.Receipts = append(rows.Receipts, rc)
		return nil
	})
	if err != nil {
		return rows, nil, err
	}
	err = q(`SELECT session, stack, idx, data FROM undo`, func(r *sql.Rows) error {
		var u engine.UndoRow
		var data string
		if err := r.Scan(&u.Session, &u.Stack, &u.Idx, &data); err != nil {
			return err
		}
		u.Entry = &engine.UndoEntry{}
		if err := json.Unmarshal([]byte(data), u.Entry); err != nil {
			return fmt.Errorf("undo %s/%s/%d: %w", u.Session, u.Stack, u.Idx, err)
		}
		rows.Undo = append(rows.Undo, u)
		return nil
	})
	if err != nil {
		return rows, nil, err
	}
	err = q(`SELECT hash, mime, w, h, bytes, refs, deleted_refs, held_until FROM assets`, func(r *sql.Rows) error {
		var a engine.Asset
		if err := r.Scan(&a.Hash, &a.Mime, &a.W, &a.H, &a.Bytes, &a.Refs, &a.DeletedRefs, &a.HeldUntil); err != nil {
			return err
		}
		rows.Assets = append(rows.Assets, a)
		return nil
	})
	return rows, meta, err
}
