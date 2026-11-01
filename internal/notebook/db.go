package notebook

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"modernc.org/sqlite"
)

const (
	dbName        = "notebook.db"
	schemaVersion = 1
	busyTimeoutMS = 5000
)

var schema = []string{
	`CREATE TABLE meta(singleton INTEGER PRIMARY KEY CHECK(singleton = 1), store TEXT NOT NULL,
		seq TEXT COLLATE notebook_u64 NOT NULL, reset_seq TEXT COLLATE notebook_u64 NOT NULL, files TEXT NOT NULL, wal_bytes INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE allocated_ids(id TEXT PRIMARY KEY, kind TEXT NOT NULL)`,
	`CREATE TABLE items(id TEXT PRIMARY KEY REFERENCES allocated_ids(id), kind TEXT NOT NULL,
		inc TEXT COLLATE notebook_u64 NOT NULL, alive INTEGER NOT NULL CHECK(alive IN (0,1)), deleted_seq TEXT COLLATE notebook_u64 NOT NULL,
		own_version TEXT COLLATE notebook_u64 NOT NULL, aggregate_version TEXT COLLATE notebook_u64 NOT NULL, data TEXT NOT NULL)`,
	`CREATE TABLE pages(id TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE, parent TEXT, position TEXT NOT NULL)`,
	`CREATE INDEX pages_place ON pages(parent, position, id)`,
	`CREATE TABLE blocks(id TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
		page TEXT NOT NULL, parent TEXT, position TEXT NOT NULL)`,
	`CREATE INDEX blocks_place ON blocks(page, parent, position, id)`,
	`CREATE TABLE databases(id TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE, block TEXT NOT NULL UNIQUE)`,
	`CREATE TABLE properties(id TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
		database_id TEXT NOT NULL, name_key TEXT COLLATE BINARY NOT NULL, alive INTEGER NOT NULL CHECK(alive IN (0,1)))`,
	`CREATE UNIQUE INDEX properties_live_name ON properties(database_id, name_key) WHERE alive = 1`,
	`CREATE TABLE options(id TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
		property_id TEXT NOT NULL, name_key TEXT COLLATE BINARY NOT NULL, alive INTEGER NOT NULL CHECK(alive IN (0,1)))`,
	`CREATE UNIQUE INDEX options_live_name ON options(property_id, name_key) WHERE alive = 1`,
	`CREATE TABLE rows(id TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
		database_id TEXT NOT NULL, position TEXT NOT NULL)`,
	`CREATE INDEX rows_place ON rows(database_id, position, id)`,
	`CREATE TABLE views(id TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE, database_id TEXT NOT NULL)`,
	`CREATE TABLE cells(row_id TEXT NOT NULL REFERENCES rows(id) ON DELETE CASCADE,
		property_id TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY(row_id, property_id))`,
	`CREATE TABLE group_versions(item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
		group_key TEXT NOT NULL, property_id TEXT NOT NULL DEFAULT '', version TEXT COLLATE notebook_u64 NOT NULL,
		PRIMARY KEY(item_id, group_key, property_id), CHECK((group_key = 'cell') = (property_id <> '')))`,
	`CREATE TABLE orders(owner_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
		kind TEXT NOT NULL CHECK(kind IN ('property_order','view_order','option_order')),
		ids TEXT NOT NULL, PRIMARY KEY(owner_id, kind))`,
	`CREATE TABLE assets(hash TEXT PRIMARY KEY, mime TEXT NOT NULL, width INTEGER NOT NULL,
		height INTEGER NOT NULL, bytes INTEGER NOT NULL, live_refs INTEGER NOT NULL,
		deleted_refs INTEGER NOT NULL, protected_until INTEGER NOT NULL)`,
	`CREATE TABLE sessions(token_hash TEXT PRIMARY KEY, editor TEXT NOT NULL, label TEXT NOT NULL,
		verified INTEGER NOT NULL, max_n TEXT COLLATE notebook_u64 NOT NULL, last_at INTEGER NOT NULL)`,
	`CREATE TABLE created(session TEXT NOT NULL REFERENCES sessions(token_hash) ON DELETE CASCADE,
		id TEXT NOT NULL REFERENCES allocated_ids(id), inc TEXT COLLATE notebook_u64 NOT NULL, PRIMARY KEY(session, id))`,
	`CREATE TABLE receipts(session TEXT NOT NULL REFERENCES sessions(token_hash) ON DELETE CASCADE,
		n TEXT COLLATE notebook_u64 NOT NULL, request_hash TEXT NOT NULL, outcome TEXT NOT NULL, PRIMARY KEY(session, n))`,
	`CREATE TABLE commands(op_id TEXT PRIMARY KEY, request_hash TEXT NOT NULL, outcome TEXT NOT NULL,
		mapping_cursor TEXT NOT NULL, state TEXT NOT NULL, outbox_seq TEXT COLLATE notebook_u64, import_status TEXT NOT NULL,
		retain_until INTEGER)`,
	`CREATE TABLE command_mappings(op_id TEXT NOT NULL REFERENCES commands(op_id) ON DELETE CASCADE,
		offset INTEGER NOT NULL, operation_index INTEGER NOT NULL, tmp TEXT, source TEXT,
		assigned TEXT NOT NULL, version TEXT COLLATE notebook_u64 NOT NULL, PRIMARY KEY(op_id, offset),
		CHECK((tmp IS NULL) <> (source IS NULL)))`,
	`CREATE TABLE deltas(seq TEXT COLLATE notebook_u64 PRIMARY KEY, effects TEXT NOT NULL, source TEXT NOT NULL,
		bytes INTEGER NOT NULL, at INTEGER NOT NULL)`,
	`CREATE TABLE undo(session TEXT NOT NULL REFERENCES sessions(token_hash) ON DELETE CASCADE,
		stack TEXT NOT NULL, entry_order INTEGER NOT NULL, components TEXT NOT NULL,
		bytes INTEGER NOT NULL, PRIMARY KEY(session, stack, entry_order))`,
	`CREATE TABLE groups(editor TEXT PRIMARY KEY, started_at INTEGER NOT NULL, last_at INTEGER NOT NULL,
		dirty INTEGER NOT NULL, label TEXT NOT NULL)`,
	`CREATE TABLE files(path TEXT PRIMARY KEY, page TEXT, bytes INTEGER NOT NULL,
		dirty_seq TEXT COLLATE notebook_u64 NOT NULL, hash TEXT)`,
	`CREATE INDEX files_page ON files(page)`,
	`CREATE TABLE outbox(seq INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, authors TEXT NOT NULL,
		message TEXT NOT NULL, rollback_target INTEGER, op_id TEXT, files TEXT NOT NULL, hashes TEXT NOT NULL)`,
	`CREATE TABLE snapshots(hash TEXT PRIMARY KEY, data BLOB NOT NULL, refs INTEGER NOT NULL)`,
	`CREATE TABLE view_notices(view_id TEXT NOT NULL REFERENCES views(id) ON DELETE CASCADE,
		seq TEXT COLLATE notebook_u64 NOT NULL, removed TEXT NOT NULL, PRIMARY KEY(view_id, seq))`,
}

type store struct {
	db       *sql.DB
	path     string
	pageSize int64
}

// SQLite INTEGER is signed; this collation keeps full uint64 decimal keys
// ordered numerically, including their indexes and watermark comparisons.
var registerSequenceCollation = sync.OnceValue(func() error {
	return sqlite.RegisterCollationUtf8("notebook_u64", func(left, right string) int {
		if len(left) < len(right) {
			return -1
		}
		if len(left) > len(right) {
			return 1
		}
		return strings.Compare(left, right)
	})
})

func openStore(path string) (*store, bool, error) {
	if err := registerSequenceCollation(); err != nil {
		return nil, false, fmt.Errorf("notebook.db sequence collation: %w", err)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, false, fmt.Errorf("notebook.db path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("notebook.db directory: %w", err)
	}
	u := url.URL{Scheme: "file", Path: path}
	// Check the version before connection pragmas that can write the database.
	preflightDSN := u.String()
	if _, err := os.Stat(path); err == nil {
		preflightDSN += "?mode=ro"
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("notebook.db stat: %w", err)
	}
	db, err := sql.Open("sqlite", preflightDSN)
	if err != nil {
		return nil, false, fmt.Errorf("notebook.db open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	fail := func(err error) (*store, bool, error) {
		return nil, false, errors.Join(err, db.Close())
	}
	ctx := context.Background()
	var version, tables int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fail(fmt.Errorf("notebook.db version: %w", err))
	}
	if version > schemaVersion {
		return fail(fmt.Errorf("notebook.db schema %d is newer than supported version %d", version, schemaVersion))
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
		return fail(fmt.Errorf("notebook.db inspect: %w", err))
	}
	fresh := version == 0 && tables == 0
	if version == 0 && !fresh {
		return fail(fmt.Errorf("notebook.db has tables without a schema version"))
	}
	if err := db.Close(); err != nil {
		return nil, false, fmt.Errorf("notebook.db close preflight: %w", err)
	}
	dsn := u.String() + fmt.Sprintf("?_pragma=busy_timeout(%d)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)&_txlock=immediate", busyTimeoutMS)
	db, err = sql.Open("sqlite", dsn)
	if err != nil {
		return nil, false, fmt.Errorf("notebook.db open writable connection: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if fresh {
		if _, err := db.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
			return fail(fmt.Errorf("notebook.db auto vacuum: %w", err))
		}
	}
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&mode); err != nil || mode != "wal" {
		return fail(fmt.Errorf("notebook.db journal mode %q: %v", mode, err))
	}
	for _, pragma := range []string{`PRAGMA synchronous = FULL`, `PRAGMA foreign_keys = ON`, `PRAGMA busy_timeout = 5000`} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return fail(fmt.Errorf("notebook.db pragma: %w", err))
		}
	}
	if fresh {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fail(fmt.Errorf("notebook.db initialize: %w", err))
		}
		initialize := func() error {
			for _, query := range schema {
				if _, err := tx.ExecContext(ctx, query); err != nil {
					return err
				}
			}
			var token [16]byte
			if _, err := rand.Read(token[:]); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO meta(singleton,store,seq,reset_seq,files) VALUES(1,?,'0','0','[]')`, hex.EncodeToString(token[:])); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `PRAGMA user_version = 1`)
			return err
		}
		if err := initialize(); err != nil {
			return fail(errors.Join(fmt.Errorf("notebook.db schema: %w", err), tx.Rollback()))
		}
		if err := tx.Commit(); err != nil {
			return fail(fmt.Errorf("notebook.db initialize commit: %w", err))
		}
	}
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return fail(fmt.Errorf("notebook.db integrity %q: %v", integrity, err))
	}
	s := &store{db: db, path: path}
	if err := db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&s.pageSize); err != nil {
		return fail(fmt.Errorf("notebook.db page size: %w", err))
	}
	if _, err := s.state(ctx); err != nil {
		return fail(err)
	}
	if err := initializeAssetReferences(ctx, db); err != nil {
		return fail(err)
	}
	return s, fresh, nil
}

func (s *store) close() error    { return s.db.Close() }
func (s *store) walPath() string { return s.path + "-wal" }

type storeState struct {
	Store string
	Seq   Sequence
	Reset Sequence
}

func (s *store) state(ctx context.Context) (storeState, error) {
	var state storeState
	var seq, reset string
	if err := s.db.QueryRowContext(ctx, `SELECT store, seq, reset_seq FROM meta WHERE singleton = 1`).Scan(&state.Store, &seq, &reset); err != nil {
		return state, fmt.Errorf("notebook.db state: %w", err)
	}
	if len(state.Store) != 32 {
		return state, fmt.Errorf("notebook.db invalid store identity")
	}
	if _, err := hex.DecodeString(state.Store); err != nil {
		return state, fmt.Errorf("notebook.db store identity: %w", err)
	}
	n, err := parseDBDecimal(seq)
	if err != nil {
		return state, err
	}
	state.Seq = Sequence(n)
	n, err = parseDBDecimal(reset)
	if err != nil {
		return state, err
	}
	state.Reset = Sequence(n)
	if state.Reset > state.Seq {
		return state, fmt.Errorf("notebook.db reset exceeds sequence")
	}
	return state, nil
}

func parseDBDecimal(s string) (uint64, error) {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != s {
		return 0, fmt.Errorf("notebook.db invalid decimal %q", s)
	}
	return n, nil
}

func (s *store) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("notebook.db begin: %w", err)
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("notebook.db commit: %w", err)
	}
	return nil
}

type itemRecord struct {
	Item      Item
	Alive     bool
	DeletedAt Sequence
	Aggregate Version
}

func putItem(ctx context.Context, tx *sql.Tx, item Item) error {
	return putRecord(ctx, tx, itemRecord{Item: item, Alive: true, Aggregate: item.Ver})
}

func putRecord(ctx context.Context, tx *sql.Tx, record itemRecord) error {
	encoded, err := EncodeItem(record.Item)
	if err != nil {
		return fmt.Errorf("notebook.db item: %w", err)
	}
	item, err := DecodeItem(encoded)
	if err != nil {
		return err
	}
	if record.Alive != (record.DeletedAt == 0) {
		return fmt.Errorf("notebook.db inconsistent deletion stamp")
	}
	if record.Aggregate > item.Ver {
		return fmt.Errorf("notebook.db aggregate exceeds item version")
	}
	own := Version(item.Ref.Inc)
	for _, ver := range item.V {
		own = max(own, ver)
	}
	if item.Ver != max(own, record.Aggregate) {
		return fmt.Errorf("notebook.db inconsistent item version")
	}
	if !record.Alive && record.DeletedAt < Sequence(item.Ref.Inc) {
		return fmt.Errorf("notebook.db deletion precedes incarnation")
	}
	var fields map[string]json.RawMessage
	if err := strictDecode(encoded, &fields); err != nil {
		return err
	}
	for _, key := range []string{"kind", "ref", "ver", "v", "cells", "property_order", "view_order", "option_order"} {
		delete(fields, key)
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO allocated_ids(id,kind) VALUES(?,?) ON CONFLICT(id) DO NOTHING`, item.ID(), item.Kind()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO items(id,kind,inc,alive,deleted_seq,own_version,aggregate_version,data)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET inc=excluded.inc,alive=excluded.alive,
		deleted_seq=excluded.deleted_seq,own_version=excluded.own_version,aggregate_version=excluded.aggregate_version,data=excluded.data`,
		item.ID(), item.Kind(), strconv.FormatUint(uint64(item.Ref.Inc), 10), record.Alive,
		strconv.FormatUint(uint64(record.DeletedAt), 10), strconv.FormatUint(uint64(own), 10),
		strconv.FormatUint(uint64(record.Aggregate), 10), string(data)); err != nil {
		return fmt.Errorf("notebook.db store item %s: %w", item.ID(), err)
	}
	if err := putSubtype(ctx, tx, item, record.Alive); err != nil {
		return fmt.Errorf("notebook.db store %s: %w", item.Kind(), err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_versions WHERE item_id=?`, item.ID()); err != nil {
		return err
	}
	for key, version := range item.V {
		group, property := splitGroup(key)
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_versions(item_id,group_key,property_id,version) VALUES(?,?,?,?)`,
			item.ID(), group, property, strconv.FormatUint(uint64(version), 10)); err != nil {
			return err
		}
	}
	return nil
}

func splitGroup(key GroupKey) (string, string) {
	if strings.HasPrefix(string(key), "cell:") {
		return "cell", strings.TrimPrefix(string(key), "cell:")
	}
	return string(key), ""
}

func putSubtype(ctx context.Context, tx *sql.Tx, item Item, alive bool) error {
	var query string
	var args []any
	switch data := item.Data.(type) {
	case Page:
		query = `INSERT INTO pages(id,parent,position) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET parent=excluded.parent,position=excluded.position`
		args = []any{item.ID(), data.Parent, data.Position}
	case Block:
		query = `INSERT INTO blocks(id,page,parent,position) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET page=excluded.page,parent=excluded.parent,position=excluded.position`
		args = []any{item.ID(), data.Page, data.Parent, data.Position}
	case Database:
		query = `INSERT INTO databases(id,block) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET block=excluded.block`
		args = []any{item.ID(), data.Block}
		if err := putOrder(ctx, tx, item.ID(), PropertyOrderGroup, data.PropertyOrder); err != nil {
			return err
		}
		if err := putOrder(ctx, tx, item.ID(), ViewOrderGroup, data.ViewOrder); err != nil {
			return err
		}
	case Property:
		query = `INSERT INTO properties(id,database_id,name_key,alive) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET database_id=excluded.database_id,name_key=excluded.name_key,alive=excluded.alive`
		args = []any{item.ID(), data.Database, data.Definition.Name, alive}
		if err := putOrder(ctx, tx, item.ID(), OptionOrderGroup, data.OptionOrder); err != nil {
			return err
		}
	case Option:
		query = `INSERT INTO options(id,property_id,name_key,alive) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET property_id=excluded.property_id,name_key=excluded.name_key,alive=excluded.alive`
		args = []any{item.ID(), data.Property, data.Name, alive}
	case Row:
		query = `INSERT INTO rows(id,database_id,position) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET database_id=excluded.database_id,position=excluded.position`
		args = []any{item.ID(), data.Database, data.Position}
	case View:
		query = `INSERT INTO views(id,database_id) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET database_id=excluded.database_id`
		args = []any{item.ID(), data.Database}
	default:
		return fmt.Errorf("unknown item subtype")
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	if row, ok := item.Data.(Row); ok {
		if _, err := tx.ExecContext(ctx, `DELETE FROM cells WHERE row_id=?`, item.ID()); err != nil {
			return err
		}
		for property, value := range row.Cells {
			encoded, err := EncodeCellValue(value)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO cells(row_id,property_id,value) VALUES(?,?,?)`, item.ID(), property, string(encoded)); err != nil {
				return err
			}
		}
	}
	return nil
}

func putOrder(ctx context.Context, tx *sql.Tx, owner ID, kind GroupKey, ids []ID) error {
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO orders(owner_id,kind,ids) VALUES(?,?,?) ON CONFLICT(owner_id,kind) DO UPDATE SET ids=excluded.ids`, owner, kind, string(data))
	return err
}

type dbReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Read preparation needs identity, ownership and query types, not payloads or orders.
func loadReadSelection(ctx context.Context, q dbReader, id ID) (Item, bool, error) {
	var item Item
	var kind Kind
	var alive bool
	var inc string
	if err := id.Validate(); err != nil {
		return item, false, err
	}
	if err := q.QueryRowContext(ctx, `SELECT kind,inc,alive FROM items WHERE id=?`, id).Scan(&kind, &inc, &alive); err != nil {
		return item, false, err
	}
	if kind != id.Kind() {
		return item, false, fmt.Errorf("notebook.db stored kind mismatch")
	}
	incarnation, err := parseDBDecimal(inc)
	if err != nil {
		return item, false, err
	}
	item.Ref = Ref{ID: id, Inc: Incarnation(incarnation)}
	if !alive {
		return item, false, nil
	}
	source := func() io.Reader {
		return &spillColumnReader{ctx: ctx, q: q, query: `SELECT substr(CAST(data AS BLOB),?,?) FROM items WHERE id=?`, args: []any{id}}
	}
	switch kind {
	case PageKind:
		var data Page
		err = q.QueryRowContext(ctx, `SELECT parent FROM pages WHERE id=?`, id).Scan(&data.Parent)
		item.Data = data
	case DatabaseKind:
		var data Database
		err = q.QueryRowContext(ctx, `SELECT block FROM databases WHERE id=?`, id).Scan(&data.Block)
		item.Data = data
	case PropertyKind:
		var data Property
		err = q.QueryRowContext(ctx, `SELECT database_id FROM properties WHERE id=?`, id).Scan(&data.Database)
		if err == nil {
			var definition io.Reader
			definition, err = spillFieldReader(source(), "definition")
			if err == nil {
				err = readBoundedField(definition, "type", &data.Definition.Type)
			}
		}
		item.Data = data
	case OptionKind:
		var data Option
		err = q.QueryRowContext(ctx, `SELECT property_id FROM options WHERE id=?`, id).Scan(&data.Property)
		item.Data = data
	case ViewKind:
		var data View
		err = q.QueryRowContext(ctx, `SELECT database_id FROM views WHERE id=?`, id).Scan(&data.Database)
		if err == nil {
			data.Definition, err = readStoredQuery(source)
		}
		item.Data = data
	case BlockKind:
		item.Data = Block{}
	default:
		err = fmt.Errorf("unsupported read selection")
	}
	return item, alive, err
}

func loadRecord(ctx context.Context, q dbReader, id ID) (itemRecord, error) {
	var record itemRecord
	if err := id.Validate(); err != nil {
		return record, err
	}
	var kind Kind
	var inc, deleted, own, aggregate, raw string
	if err := q.QueryRowContext(ctx, `SELECT kind,inc,alive,deleted_seq,own_version,aggregate_version,data FROM items WHERE id=?`, id).
		Scan(&kind, &inc, &record.Alive, &deleted, &own, &aggregate, &raw); err != nil {
		return record, err
	}
	if kind != id.Kind() {
		return record, fmt.Errorf("notebook.db stored kind mismatch")
	}
	var fields map[string]json.RawMessage
	if err := strictDecode([]byte(raw), &fields); err != nil {
		return record, err
	}
	incarnation, err := parseDBDecimal(inc)
	if err != nil {
		return record, err
	}
	delSeq, err := parseDBDecimal(deleted)
	if err != nil {
		return record, err
	}
	ownVersion, err := parseDBDecimal(own)
	if err != nil {
		return record, err
	}
	aggregateVersion, err := parseDBDecimal(aggregate)
	if err != nil {
		return record, err
	}
	record.DeletedAt = Sequence(delSeq)
	record.Aggregate = Version(aggregateVersion)
	if record.Alive != (record.DeletedAt == 0) || (!record.Alive && delSeq < incarnation) {
		return record, fmt.Errorf("notebook.db invalid deletion stamp")
	}
	versions, err := loadVersions(ctx, q, id)
	if err != nil {
		return record, err
	}
	computedOwn := Version(incarnation)
	for _, v := range versions {
		computedOwn = max(computedOwn, v)
	}
	if computedOwn != Version(ownVersion) {
		return record, fmt.Errorf("notebook.db own version mismatch")
	}
	add := func(key string, value any) error {
		encoded, err := json.Marshal(value)
		if err == nil {
			fields[key] = encoded
		}
		return err
	}
	for key, value := range map[string]any{"kind": kind, "ref": Ref{ID: id, Inc: Incarnation(incarnation)}, "ver": max(computedOwn, record.Aggregate), "v": versions} {
		if err := add(key, value); err != nil {
			return record, err
		}
	}
	var orderKinds []GroupKey
	switch kind {
	case DatabaseKind:
		orderKinds = []GroupKey{PropertyOrderGroup, ViewOrderGroup}
	case PropertyKind:
		orderKinds = []GroupKey{OptionOrderGroup}
	case RowKind:
		cells, err := loadCells(ctx, q, id)
		if err != nil {
			return record, err
		}
		if err := add("cells", cells); err != nil {
			return record, err
		}
	}
	for _, key := range orderKinds {
		var data string
		if err := q.QueryRowContext(ctx, `SELECT ids FROM orders WHERE owner_id=? AND kind=?`, id, key).Scan(&data); err != nil {
			return record, err
		}
		fields[string(key)] = json.RawMessage(data)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return record, err
	}
	record.Item, err = DecodeItem(encoded)
	if err != nil {
		return record, err
	}
	if err := checkSubtype(ctx, q, record.Item, record.Alive); err != nil {
		return record, err
	}
	return record, nil
}

func loadSpillRecord(ctx context.Context, q dbReader, id ID, out io.Writer, public bool) error {
	if err := id.Validate(); err != nil {
		return err
	}
	var kind Kind
	var alive bool
	var inc, deleted, own, aggregate string
	if err := q.QueryRowContext(ctx, `SELECT kind,inc,alive,deleted_seq,own_version,aggregate_version FROM items WHERE id=?`, id).
		Scan(&kind, &inc, &alive, &deleted, &own, &aggregate); err != nil {
		return err
	}
	if kind != id.Kind() {
		return fmt.Errorf("notebook.db stored kind mismatch")
	}
	incarnation, err := parseDBDecimal(inc)
	if err != nil {
		return err
	}
	delSeq, err := parseDBDecimal(deleted)
	if err != nil {
		return err
	}
	ownVersion, err := parseDBDecimal(own)
	if err != nil {
		return err
	}
	aggregateVersion, err := parseDBDecimal(aggregate)
	if err != nil {
		return err
	}
	if !alive || delSeq != 0 || incarnation == 0 {
		return fmt.Errorf("inactive snapshot item")
	}
	versions, err := loadVersions(ctx, q, id)
	if err != nil {
		return err
	}
	computedOwn := Version(incarnation)
	for _, v := range versions {
		computedOwn = max(computedOwn, v)
	}
	if computedOwn != Version(ownVersion) {
		return fmt.Errorf("notebook.db own version mismatch")
	}
	w := bufio.NewWriterSize(out, spillChunk)
	if _, err := w.WriteString(`{"kind":`); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	if err := enc.Encode(kind); err != nil {
		return err
	}
	field := func(name string, value any) error {
		if _, err := fmt.Fprintf(w, ",%q:", name); err != nil {
			return err
		}
		return enc.Encode(value)
	}
	if public {
		if err := field("id", id); err != nil {
			return err
		}
	} else {
		if err := field("ref", Ref{ID: id, Inc: Incarnation(incarnation)}); err != nil {
			return err
		}
		if err := field("ver", max(computedOwn, Version(aggregateVersion))); err != nil {
			return err
		}
		if err := field("v", versions); err != nil {
			return err
		}
	}
	source := &spillColumnReader{ctx: ctx, q: q, query: `SELECT substr(CAST(data AS BLOB),?,?) FROM items WHERE id=?`, args: []any{id}}
	if err := streamSpillFields(w, source, kind); err != nil {
		return err
	}
	var orders []GroupKey
	switch kind {
	case DatabaseKind:
		orders = []GroupKey{PropertyOrderGroup, ViewOrderGroup}
	case PropertyKind:
		orders = []GroupKey{OptionOrderGroup}
	case RowKind:
		if _, err := w.WriteString(`,"cells":{`); err != nil {
			return err
		}
		rows, err := q.QueryContext(ctx, `SELECT property_id FROM cells WHERE row_id=? ORDER BY property_id`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		first := true
		for rows.Next() {
			var property ID
			if err := rows.Scan(&property); err != nil {
				return err
			}
			if err := property.ValidateKind(PropertyKind); err != nil {
				return err
			}
			if !first {
				if err := w.WriteByte(','); err != nil {
					return err
				}
			}
			first = false
			if err := enc.Encode(property); err != nil {
				return err
			}
			if err := w.WriteByte(':'); err != nil {
				return err
			}
			cell := &spillColumnReader{ctx: ctx, q: q, query: `SELECT substr(CAST(value AS BLOB),?,?) FROM cells WHERE row_id=? AND property_id=?`, args: []any{id, property}}
			if _, err := io.Copy(w, cell); err != nil {
				return err
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := w.WriteByte('}'); err != nil {
			return err
		}
	}
	for _, order := range orders {
		if _, err := fmt.Fprintf(w, ",%q:", order); err != nil {
			return err
		}
		ids := &spillColumnReader{ctx: ctx, q: q, query: `SELECT substr(CAST(ids AS BLOB),?,?) FROM orders WHERE owner_id=? AND kind=?`, args: []any{id, order}}
		if _, err := io.Copy(w, ids); err != nil {
			return err
		}
	}
	if err := w.WriteByte('}'); err != nil {
		return err
	}
	return w.Flush()
}

func checkSubtype(ctx context.Context, q dbReader, item Item, alive bool) error {
	text := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	parent := func(id *ID) sql.NullString {
		if id == nil {
			return sql.NullString{}
		}
		return text(string(*id))
	}
	aliveText := "0"
	if alive {
		aliveText = "1"
	}
	var query string
	var want []sql.NullString
	switch data := item.Data.(type) {
	case Page:
		query = `SELECT parent,position FROM pages WHERE id=?`
		want = []sql.NullString{parent(data.Parent), text(data.Position)}
	case Block:
		query = `SELECT page,parent,position FROM blocks WHERE id=?`
		want = []sql.NullString{text(string(data.Page)), parent(data.Parent), text(data.Position)}
	case Database:
		query = `SELECT block FROM databases WHERE id=?`
		want = []sql.NullString{text(string(data.Block))}
	case Property:
		query = `SELECT database_id,name_key,alive FROM properties WHERE id=?`
		want = []sql.NullString{text(string(data.Database)), text(data.Definition.Name), text(aliveText)}
	case Option:
		query = `SELECT property_id,name_key,alive FROM options WHERE id=?`
		want = []sql.NullString{text(string(data.Property)), text(data.Name), text(aliveText)}
	case Row:
		query = `SELECT database_id,position FROM rows WHERE id=?`
		want = []sql.NullString{text(string(data.Database)), text(data.Position)}
	case View:
		query = `SELECT database_id FROM views WHERE id=?`
		want = []sql.NullString{text(string(data.Database))}
	}
	got := make([]sql.NullString, len(want))
	dest := make([]any, len(want))
	for i := range dest {
		dest[i] = &got[i]
	}
	if err := q.QueryRowContext(ctx, query, item.ID()).Scan(dest...); err != nil {
		return fmt.Errorf("notebook.db subtype %s: %w", item.ID(), err)
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("notebook.db subtype disagrees with item %s", item.ID())
		}
	}
	return nil
}

func loadVersions(ctx context.Context, q dbReader, id ID) (map[GroupKey]Version, error) {
	rows, err := q.QueryContext(ctx, `SELECT group_key,property_id,version FROM group_versions WHERE item_id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := map[GroupKey]Version{}
	for rows.Next() {
		var group, property, raw string
		if err := rows.Scan(&group, &property, &raw); err != nil {
			return nil, err
		}
		key := GroupKey(group)
		if group == "cell" {
			key = CellGroup(ID(property))
		}
		if err := ValidateGroup(id.Kind(), key); err != nil {
			return nil, err
		}
		version, err := parseDBDecimal(raw)
		if err != nil {
			return nil, err
		}
		versions[key] = Version(version)
	}
	return versions, rows.Err()
}

func loadCells(ctx context.Context, q dbReader, row ID) (map[ID]CellValue, error) {
	rows, err := q.QueryContext(ctx, `SELECT property_id,value FROM cells WHERE row_id=?`, row)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cells := map[ID]CellValue{}
	for rows.Next() {
		var property ID
		var raw string
		if err := rows.Scan(&property, &raw); err != nil {
			return nil, err
		}
		if err := property.ValidateKind(PropertyKind); err != nil {
			return nil, err
		}
		value, err := DecodeCellValue([]byte(raw))
		if err != nil {
			return nil, err
		}
		cells[property] = value
	}
	return cells, rows.Err()
}

func (s *store) loadItem(ctx context.Context, id ID) (Item, error) {
	var item Item
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		record, err := loadRecord(ctx, tx, id)
		item = record.Item
		return err
	})
	return item, err
}
