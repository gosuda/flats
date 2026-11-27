package sheet

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	_ "modernc.org/sqlite"

	"github.com/gosuda/flats/internal/sheet/formula"
	"github.com/gosuda/flats/internal/sqliteuri"
)

const (
	dbName        = "sheet.db"
	schemaVersion = 1
	// releaseMark is the SQLite application_id of every sheet.db this
	// release creates. No development build set one, and some of their
	// databases have the schema of this release while their stored
	// commands and receipts hash requests in other forms, so the mark is
	// what tells them apart.
	releaseMark = 0x46536874
)

// The schema of the technical design, section 5.
var schema = []string{
	`CREATE TABLE meta (singleton INTEGER PRIMARY KEY CHECK (singleton = 1), store TEXT NOT NULL,
		seq INTEGER NOT NULL, reset_seq INTEGER NOT NULL, tab_change_seq INTEGER NOT NULL,
		guard_floor INTEGER NOT NULL, calc_date TEXT NOT NULL,
		next_tab INTEGER NOT NULL, next_row INTEGER NOT NULL, next_col INTEGER NOT NULL,
		tab_order TEXT NOT NULL, tab_order_seq INTEGER NOT NULL, files BLOB NOT NULL)`,
	`CREATE TABLE tabs (id INTEGER PRIMARY KEY, name TEXT NOT NULL, name_key TEXT NOT NULL UNIQUE,
		born INTEGER NOT NULL, name_seq INTEGER NOT NULL, settings_seq INTEGER NOT NULL, struct_seq INTEGER NOT NULL,
		frozen_rows INTEGER NOT NULL, frozen_cols INTEGER NOT NULL,
		row_ids BLOB NOT NULL, row_born BLOB NOT NULL, col_ids BLOB NOT NULL, col_born BLOB NOT NULL,
		chunks BLOB NOT NULL)`,
	`CREATE TABLE cols (tab INTEGER NOT NULL, col INTEGER NOT NULL, width INTEGER NOT NULL, seq INTEGER NOT NULL,
		PRIMARY KEY (tab, col)) WITHOUT ROWID`,
	`CREATE TABLE styles (id INTEGER PRIMARY KEY, format TEXT NOT NULL, stamps BLOB NOT NULL, refs INTEGER NOT NULL)`,
	`CREATE TABLE cells (tab INTEGER NOT NULL, row INTEGER NOT NULL, col INTEGER NOT NULL,
		kind INTEGER NOT NULL, num REAL, str TEXT, style INTEGER NOT NULL DEFAULT 0,
		seq INTEGER NOT NULL, text_seq INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (tab, row, col)) WITHOUT ROWID`,
	`CREATE TABLE sessions (token_hash BLOB PRIMARY KEY, id TEXT NOT NULL UNIQUE, editor TEXT NOT NULL,
		label TEXT NOT NULL, verified INTEGER NOT NULL, max_n INTEGER NOT NULL, last_at INTEGER NOT NULL)`,
	// Maintenance finds expired sessions by this index, not by reading all.
	`CREATE INDEX sessions_last ON sessions(last_at)`,
	`CREATE TABLE receipts (session TEXT NOT NULL, n INTEGER NOT NULL, hash BLOB NOT NULL, seq INTEGER NOT NULL,
		outcome BLOB NOT NULL, PRIMARY KEY (session, n)) WITHOUT ROWID`,
	`CREATE TABLE commands (op_id TEXT PRIMARY KEY, hash BLOB NOT NULL, outcome BLOB NOT NULL, seq INTEGER NOT NULL,
		outbox_seq INTEGER NOT NULL, imported INTEGER NOT NULL, expires INTEGER NOT NULL)`,
	`CREATE TABLE groups (editor TEXT PRIMARY KEY, label TEXT NOT NULL, verified INTEGER NOT NULL,
		started INTEGER NOT NULL, last INTEGER NOT NULL, dirty INTEGER NOT NULL)`,
	`CREATE TABLE files (path TEXT PRIMARY KEY, hash TEXT NOT NULL, size INTEGER NOT NULL,
		dirty_seq INTEGER NOT NULL, reserve INTEGER NOT NULL)`,
	`CREATE TABLE outbox (seq INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, authors TEXT NOT NULL,
		message TEXT NOT NULL, rollback_of INTEGER, op_id TEXT NOT NULL DEFAULT '', state_seq INTEGER NOT NULL,
		calc_date TEXT NOT NULL, tabs TEXT NOT NULL, files BLOB NOT NULL, created INTEGER NOT NULL)`,
	`CREATE TABLE snapshots (hash TEXT PRIMARY KEY, data BLOB NOT NULL, refs INTEGER NOT NULL)`,
}

// CheckDB reports why the sheet.db in dir cannot be opened, without
// writing: a newer schema, or a database of version 1 that this release
// did not create. It returns nil for a database that opens, and for none.
// The host asks it when a sheet did not start. It does not read a sheet.db
// with a -wal file, which a crash or a stop with a long log left, whose
// load checks it at the gate (openLog), and returns nil for one.
func CheckDB(dir string) error {
	path := filepath.Join(dir, dbName)
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	if logged, err := hasLog(path); err != nil || logged {
		return err
	}
	_, err := inspectDB(path)
	return err
}

// inspectDB reads the schema version of sheet.db, creating the file when
// there is none, and refuses one that this release cannot open, without a
// write to it or to its directory.
func inspectDB(path string) (version int, err error) {
	preflight := sqliteuri.File(path)
	if _, statErr := os.Stat(path); statErr == nil {
		// A read-only connection to a WAL database builds a -shm file and
		// leaves it and an empty -wal file behind. Without a -wal file the
		// database is whole in its main file, and nothing writes it before
		// the instance opens, so the probe reads it as immutable. A -wal
		// file that a crash left holds frames the probe must read, and
		// reading them builds the -shm index, so the probe reads them in a
		// private directory (privateCopy).
		switch _, walErr := os.Stat(path + "-wal"); {
		case walErr == nil:
			copied, err := privateCopy(path, nil)
			if err != nil {
				return 0, fmt.Errorf("sheet.db inspect: %w", err)
			}
			defer os.RemoveAll(filepath.Dir(copied))
			preflight = sqliteuri.File(copied) + "?mode=ro"
		case errors.Is(walErr, os.ErrNotExist):
			preflight += "?mode=ro&immutable=1"
		default:
			return 0, fmt.Errorf("sheet.db stat: %w", walErr)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return 0, fmt.Errorf("sheet.db stat: %w", statErr)
	}
	return probe(preflight)
}

// probe opens the database at the URI read-only and checks its schema
// (checkSchema).
func probe(uri string) (version int, err error) {
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return 0, fmt.Errorf("sheet.db open: %w", err)
	}
	version, err = checkSchema(context.Background(), db)
	if closeErr := db.Close(); err == nil && closeErr != nil {
		return 0, fmt.Errorf("sheet.db inspect: %w", closeErr)
	}
	return version, err
}

// checkSchema reads the schema version of sheet.db and refuses a database
// this release cannot open: a newer schema, tables without a version, or a
// database of version 1 that this release did not create. Version 0 is a
// database without tables.
func checkSchema(ctx context.Context, q querier) (version int, err error) {
	var tables int
	var differs string
	err = q.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version)
	if err == nil {
		err = q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables)
	}
	if err == nil && version == schemaVersion {
		differs, err = releaseDiffers(ctx, q)
	}
	switch {
	case err != nil:
		return 0, fmt.Errorf("sheet.db inspect: %w", err)
	case version > schemaVersion:
		return 0, fmt.Errorf("sheet.db schema %d is newer than supported version %d", version, schemaVersion)
	case version == 0 && tables != 0:
		return 0, fmt.Errorf("sheet.db has tables without a schema version")
	case differs != "":
		// No released build had the sheet type, so a database of version
		// 1 that this release did not create comes from an unreleased
		// development build.
		return 0, &WriteError{Code: "unavailable", Reason: "load", Retry: true,
			Detail: "sheet.db was written by an unreleased development build, and this release opens only the databases it creates (" + differs + "); create the sheet again"}
	}
	return version, nil
}

// privateCopy puts sheet.db and a copy of its -wal file into a new private
// directory and returns the path of sheet.db there. The caller removes the
// directory. sheet.db is linked when the file system allows it, so that a
// large sheet is not copied each time it is checked; the probe opens it
// read-only, and only the log's index is built, beside the copied log.
// point, when set, is called every copyChunk bytes, and the copy ends when
// it returns false.
func privateCopy(path string, point func() bool) (string, error) {
	dir, err := os.MkdirTemp("", "flats-sheet-inspect-")
	if err != nil {
		return "", err
	}
	copied := filepath.Join(dir, dbName)
	if err = linkFile(path, copied); err != nil {
		err = copyNew(path, copied, point)
	}
	if err == nil {
		err = copyNew(path+"-wal", copied+"-wal", point)
	}
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return copied, nil
}

// linkFile is os.Link, which a test replaces to take the copying path.
var linkFile = os.Link

// copyChunk is how many bytes privateCopy copies between two check points.
const copyChunk = 1 << 20

// copyNew copies the file from to the new file to, which only its owner
// reads, calling point, when set, before each copyChunk bytes.
func copyNew(from, to string, point func() bool) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	for {
		if point != nil && !point() {
			return errors.Join(errLoadStopped, dst.Close())
		}
		if _, err := io.CopyN(dst, src, copyChunk); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return errors.Join(err, dst.Close())
		}
	}
	return dst.Close()
}

// openDB opens sheet.db with one writing connection, WAL, synchronous FULL
// and incremental auto-vacuum. What inspectDB refuses fails without a
// write. The instance closes it with closeDB.
//
// The connection does not checkpoint the log on its own after a commit:
// SQLite's automatic checkpoint would copy the whole log into sheet.db in
// any commit, the final one of a stop included. The instance bounds the
// log itself after its commits (Instance.commit) and leaves it alone once
// it stops.
func openDB(path string) (db *sql.DB, fresh bool, pageSize int64, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, 0, fmt.Errorf("sheet.db directory: %w", err)
	}
	version, err := inspectDB(path)
	if err != nil {
		return nil, false, 0, err
	}
	return openInspected(path, version, true)
}

// openInspected is openDB after inspectDB found version. Unless create is
// set, a sheet.db that is not there is not created.
func openInspected(path string, version int, create bool) (db *sql.DB, fresh bool, pageSize int64, err error) {
	uri := sqliteuri.File(path) + "?_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_pragma=wal_autocheckpoint(0)&_txlock=immediate"
	if !create {
		uri += "&mode=rw"
	}
	ctx := context.Background()
	fresh = version == 0
	db, err = sql.Open("sqlite", uri)
	if err != nil {
		return nil, false, 0, fmt.Errorf("sheet.db open writable connection: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	fail := func(err error) (*sql.DB, bool, int64, error) {
		return nil, false, 0, errors.Join(err, closeDB(path, db, nil))
	}
	if fresh {
		if _, err := db.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
			return fail(fmt.Errorf("sheet.db auto vacuum: %w", err))
		}
	}
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&mode); err != nil || mode != "wal" {
		return fail(fmt.Errorf("sheet.db journal mode %q: %v", mode, err))
	}
	if fresh {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fail(fmt.Errorf("sheet.db initialize: %w", err))
		}
		for _, query := range schema {
			if _, err := tx.ExecContext(ctx, query); err != nil {
				return fail(errors.Join(fmt.Errorf("sheet.db schema: %w", err), tx.Rollback()))
			}
		}
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return fail(errors.Join(err, tx.Rollback()))
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO meta(singleton,store,seq,reset_seq,tab_change_seq,guard_floor,calc_date,
			next_tab,next_row,next_col,tab_order,tab_order_seq,files) VALUES(1,?,0,0,0,0,'',1,1,1,'[]',0,'[]')`, hex.EncodeToString(token[:]))
		if err == nil {
			_, err = tx.ExecContext(ctx, `PRAGMA user_version = 1`)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `PRAGMA application_id = `+strconv.Itoa(releaseMark))
		}
		if err != nil {
			return fail(errors.Join(fmt.Errorf("sheet.db schema: %w", err), tx.Rollback()))
		}
		if err := tx.Commit(); err != nil {
			return fail(fmt.Errorf("sheet.db initialize commit: %w", err))
		}
	}
	if err := db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return fail(fmt.Errorf("sheet.db page size: %w", err))
	}
	return db, fresh, pageSize, nil
}

// releaseSchema is the schema this release creates, as sqlite_master
// lists it, read once from an empty database in memory.
var releaseSchema = sync.OnceValues(func() (map[string]string, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, query := range schema {
		if _, err := db.Exec(query); err != nil {
			return nil, err
		}
	}
	return schemaObjects(context.Background(), db)
})

// schemaObjects lists the tables and indexes of a database by type and
// name, with the statement that created each. SQLite's own objects follow
// from the others and are left out.
func schemaObjects(ctx context.Context, q querier) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_master WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := map[string]string{}
	for rows.Next() {
		var typ, name, table, text string
		if err := rows.Scan(&typ, &name, &table, &text); err != nil {
			return nil, err
		}
		objects[typ+" "+name] = table + "\n" + text
	}
	return objects, rows.Err()
}

// releaseDiffers names how a database of version 1 differs from the ones
// this release creates: the release mark it lacks and the objects whose
// schema is not this release's. It returns "" for a database this release
// created.
func releaseDiffers(ctx context.Context, q querier) (string, error) {
	want, err := releaseSchema()
	if err != nil {
		return "", fmt.Errorf("the schema of this release: %w", err)
	}
	var mark int64
	if err := q.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&mark); err != nil {
		return "", err
	}
	have, err := schemaObjects(ctx, q)
	if err != nil {
		return "", err
	}
	var differs []string
	if mark != releaseMark {
		differs = append(differs, "the application_id is not the mark of this release")
	}
	for key, text := range want {
		switch got, ok := have[key]; {
		case !ok:
			differs = append(differs, "the "+key+" is missing")
		case got != text:
			differs = append(differs, "the "+key+" is defined differently")
		}
	}
	for key := range have {
		if _, ok := want[key]; !ok {
			differs = append(differs, "the "+key+" is not in this release")
		}
	}
	if len(differs) == 0 {
		return "", nil
	}
	slices.Sort(differs)
	return strings.Join(differs, ", "), nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func encodeRowIDs(ids []RowID) []byte {
	var out []byte
	for _, id := range ids {
		out = binary.AppendUvarint(out, uint64(id))
	}
	return out
}

// decodeRowIDs reads a chunk list, with a step of pace for each id. The
// first ids of the chunks ascend and lie below maxID, as the binary
// searches of the chunks need, and there are at most limit of them.
func decodeRowIDs(data []byte, limit int, pace *loadPace) ([]RowID, error) {
	var ids []RowID
	for len(data) > 0 {
		if !pace.step(1) {
			return nil, errLoadStopped
		}
		v, n := binary.Uvarint(data)
		if n <= 0 || v >= maxID || (len(ids) > 0 && RowID(v) <= ids[len(ids)-1]) {
			return nil, fmt.Errorf("sheet.db chunk list is damaged")
		}
		if len(ids) == limit {
			return nil, fmt.Errorf("sheet.db chunk list holds more than %d chunks", limit)
		}
		ids, data = append(ids, RowID(v)), data[n:]
	}
	return ids, nil
}

// The longest fields the writer stores in sheet.db, in bytes. A field is
// read only when its class and its length, which SQLite reads from the
// row's header, are within its column's (storedTables), so a damaged field
// of any size is refused without being read into memory.
const (
	// The store id is the hex of 16 random bytes, and a calculation date
	// is spelled yyyy-mm-dd.
	maxStoreBytes = 32
	maxDateBytes  = len("9999-12-31")
	// maxNameBytes holds a tab name of maxTabName code points of four
	// bytes each.
	maxNameBytes = 4 * maxTabName
	// maxIDVarint is the length of a varint below maxID, which holds 53
	// bits at seven a byte.
	maxIDVarint = 8
	// maxStampsBytes holds the nine stamps of a style, each a varint.
	maxStampsBytes = propCount * binary.MaxVarintLen64
	// A revision file's path is at most tabs/<id>/rows/<first>-<part>.json
	// with ids below 2^53 and a part number of an int, and its hash 64
	// hex digits.
	maxPathBytes = len("tabs//rows/-.json") + 16 + 14 + 19
	maxHashBytes = 64
	// maxChunkBytes holds the chunk list of a tab.
	maxChunkBytes = maxTabChunks * maxIDVarint
	// maxFormatBytes holds the canonical spelling of a format with every
	// property set (appendFormat); a test spells the longest.
	maxFormatBytes = 512
)

// maxIDRunBytes holds the id runs of an axis of limit ids: at most one run
// for each id, a first id and a count below maxID.
func maxIDRunBytes(limit int) int { return limit * 2 * maxIDVarint }

// maxBornBytes holds the born stamps of an axis of limit ids: at most one
// run for each id, a first id and a count below maxID and a sequence.
func maxBornBytes(limit int) int { return limit * (2*maxIDVarint + binary.MaxVarintLen64) }

// maxCellTextBytes is the longest str of a cell of the kind, compared as
// stored since a narrowed kind could read as another (loadCells): a text
// of formula.MaxTextLen code points, a stored formula of at most
// formula.MaxStoredBytes (ParseStored) or the typed text of an unreadable
// formula of at most formula.MaxFormulaChars code points, each code point
// at most four bytes. The writer stores no str for another kind.
func maxCellTextBytes(kind int64) int {
	switch kind {
	case int64(formula.ContentText):
		return maxTextBytes
	case int64(formula.ContentFormula):
		return formula.MaxStoredBytes
	case int64(formula.ContentUnreadable):
		return maxUnreadableBytes
	}
	return 0
}

const (
	maxTextBytes       = 4 * formula.MaxTextLen
	maxUnreadableBytes = 4 * formula.MaxFormulaChars
	// maxStrBytes is the longest str of any kind.
	maxStrBytes = max(maxTextBytes, formula.MaxStoredBytes, maxUnreadableBytes)
)

// boundedField is the SQL of a field and of its length in bytes: the field
// when it holds at most max bytes and NULL otherwise. octet_length reads
// the length from the row's header without the field, and the CASE reads
// the field only when it fits.
func boundedField(field string, max int) string {
	return fmt.Sprintf("CASE WHEN octet_length(%[1]s)<=%[2]d THEN %[1]s END,octet_length(%[1]s)", field, max)
}

// fieldFits checks the length that boundedField read beside a field. An
// absent field has no length and fits.
func fieldFits(name string, length sql.NullInt64, max int) error {
	if length.Valid && length.Int64 > int64(max) {
		return fmt.Errorf("sheet.db %s holds %d bytes, more than the %d the writer stores", name, length.Int64, max)
	}
	return nil
}

// A storage class of SQLite, as typeof names it. The tables of sheet.db are
// not STRICT, so a column holds a value of any class; the writer stores
// only some in each.
type storageClass uint8

const (
	classInteger storageClass = 1 << iota
	classReal
	classText
	classBlob
	classNull
)

// storedColumn is a column of sheet.db with the classes the writer stores
// in it and, for text and blobs, the most bytes it stores there.
type storedColumn struct {
	name    string
	classes storageClass
	max     int
}

const (
	textOrBlob = classText | classBlob
	// sessionIDBytes is the hex of the 16 bytes of a session id
	// (sessionID).
	sessionIDBytes = 32
)

func intCol(name string) storedColumn { return storedColumn{name, classInteger, 0} }

func textCol(name string, max int) storedColumn { return storedColumn{name, classText, max} }

func blobCol(name string, max int) storedColumn { return storedColumn{name, classBlob, max} }

// storedTables lists every table of sheet.db, SQLite's sqlite_sequence of
// the outbox among them, and every column of each, with what the writer
// stores there. A file list is spelled '[]' in a new sheet and a stored
// rollback names its request in text, so those columns hold text as well.
// A load checks every value of every table against it (loadWorkbook), and
// a read outside the writer checks the tables it reads first
// (checkTables), each by the class and the length SQLite reads from the
// row's header. After that no statement reads a value the writer would not
// store, in its result, its order, its filter or the keys an index
// compares, and the writer stores only such values.
var storedTables = map[string][]storedColumn{
	"meta": {intCol("singleton"), textCol("store", maxStoreBytes), intCol("seq"), intCol("reset_seq"), intCol("tab_change_seq"),
		intCol("guard_floor"), textCol("calc_date", maxDateBytes), intCol("next_tab"), intCol("next_row"), intCol("next_col"),
		textCol("tab_order", maxTabOrderBytes), intCol("tab_order_seq"), {"files", textOrBlob, maxFileListBytes}},
	"tabs": {intCol("id"), textCol("name", maxNameBytes), textCol("name_key", maxNameBytes), intCol("born"), intCol("name_seq"),
		intCol("settings_seq"), intCol("struct_seq"), intCol("frozen_rows"), intCol("frozen_cols"),
		blobCol("row_ids", maxIDRunBytes(formula.MaxRows)), blobCol("row_born", maxBornBytes(formula.MaxRows)),
		blobCol("col_ids", maxIDRunBytes(formula.MaxCols)), blobCol("col_born", maxBornBytes(formula.MaxCols)), blobCol("chunks", maxChunkBytes)},
	"cols":   {intCol("tab"), intCol("col"), intCol("width"), intCol("seq")},
	"styles": {intCol("id"), textCol("format", maxFormatBytes), blobCol("stamps", maxStampsBytes), intCol("refs")},
	"cells": {intCol("tab"), intCol("row"), intCol("col"), intCol("kind"), {"num", classReal | classNull, 0},
		{"str", classText | classNull, maxStrBytes}, intCol("style"), intCol("seq"), intCol("text_seq")},
	"sessions": {blobCol("token_hash", sha256.Size), textCol("id", sessionIDBytes), textCol("editor", maxEditorBytes),
		textCol("label", maxLabelBytes), intCol("verified"), intCol("max_n"), intCol("last_at")},
	"receipts": {textCol("session", sessionIDBytes), intCol("n"), blobCol("hash", sha256.Size), intCol("seq"), blobCol("outcome", maxOutcomeBytes)},
	"commands": {textCol("op_id", maxOpIDBytes), textCol("hash", maxHashBytes), {"outcome", textOrBlob, maxCommandOutcomeBytes},
		intCol("seq"), intCol("outbox_seq"), intCol("imported"), intCol("expires")},
	"groups": {textCol("editor", maxEditorBytes), textCol("label", maxLabelBytes), intCol("verified"), intCol("started"),
		intCol("last"), intCol("dirty")},
	"files": {textCol("path", maxPathBytes), textCol("hash", maxHashBytes), intCol("size"), intCol("dirty_seq"), intCol("reserve")},
	"outbox": {intCol("seq"), textCol("kind", maxKindBytes), textCol("authors", maxAuthorsBytes), textCol("message", maxMessageBytes),
		{"rollback_of", classNull, 0}, textCol("op_id", maxOpIDBytes), intCol("state_seq"), textCol("calc_date", maxDateBytes),
		textCol("tabs", maxOutboxTabsBytes), {"files", textOrBlob, maxFileListBytes}, intCol("created")},
	"snapshots":       {textCol("hash", maxHashBytes), blobCol("data", maxSnapshotBytes), intCol("refs")},
	"sqlite_sequence": {textCol("name", len("outbox")), intCol("seq")},
}

// loadedTables are the tables a load reads row by row, checking each
// value as it reads it. It checks the others whole first (checkStored).
var loadedTables = []string{"meta", "tabs", "cols", "styles", "cells", "files"}

// columnOf returns the column of a table of sheet.db.
func columnOf(table, name string) storedColumn {
	for _, c := range storedTables[table] {
		if c.name == name {
			return c
		}
	}
	panic("sheet.db has no column " + table + "." + name)
}

// fits is the SQL that holds when the value of the column at expr has a
// class the writer stores there and no more bytes. typeof and
// octet_length read the class and the length from the row's header,
// without the value, in every column but one of real affinity, whose rows
// the open bounds first (checkRecords).
func (c storedColumn) fits(expr string) string {
	var classes, terms []string
	for _, k := range []struct {
		class storageClass
		name  string
	}{{classInteger, "'integer'"}, {classReal, "'real'"}, {classNull, "'null'"}} {
		if c.classes&k.class != 0 {
			classes = append(classes, k.name)
		}
	}
	if len(classes) > 0 {
		terms = append(terms, "typeof("+expr+") IN ("+strings.Join(classes, ",")+")")
	}
	var sized []string
	if c.classes&classText != 0 {
		sized = append(sized, "'text'")
	}
	if c.classes&classBlob != 0 {
		sized = append(sized, "'blob'")
	}
	if len(sized) > 0 {
		terms = append(terms, fmt.Sprintf("(typeof(%s) IN (%s) AND octet_length(%[1]s)<=%[3]d)", expr, strings.Join(sized, ","), c.max))
	}
	return "(" + strings.Join(terms, " OR ") + ")"
}

// value is the SQL of the column at expr: its value when it fits and
// otherwise NULL, or 0 in a column of integers only, so that a row whose
// fault (storedFault) is read beside it still scans.
func (c storedColumn) value(expr string) string {
	if c.classes == classInteger {
		return "CASE WHEN " + c.fits(expr) + " THEN " + expr + " ELSE 0 END"
	}
	return "CASE WHEN " + c.fits(expr) + " THEN " + expr + " END"
}

// storedValue is column.value for the column of a table under the alias,
// or the table's own name when the alias is "".
func storedValue(table, alias, name string) string {
	if alias == "" {
		alias = table
	}
	return columnOf(table, name).value(alias + "." + name)
}

// storedValues is storedValue of each column named.
func storedValues(table, alias string, names ...string) string {
	values := make([]string, len(names))
	for i, name := range names {
		values[i] = storedValue(table, alias, name)
	}
	return strings.Join(values, ",")
}

// storedFault is the SQL of a row of a table under the alias that is 0
// when every value of the row fits its column, and otherwise the place in
// storedTables, from 1, of the first column whose value does not. With
// names, it checks those columns only.
func storedFault(table, alias string, names ...string) string {
	if alias == "" {
		alias = table
	}
	var b strings.Builder
	b.WriteString("CASE")
	for i, c := range storedTables[table] {
		if len(names) == 0 || slices.Contains(names, c.name) {
			fmt.Fprintf(&b, " WHEN NOT %s THEN %d", c.fits(alias+"."+c.name), i+1)
		}
	}
	b.WriteString(" ELSE 0 END")
	return b.String()
}

// faultError is the refusal of a row whose fault (storedFault) is not 0.
func faultError(table string, fault int64) error {
	if fault == 0 {
		return nil
	}
	cols := storedTables[table]
	name := "a column"
	if fault > 0 && fault <= int64(len(cols)) {
		name = cols[fault-1].name
	}
	return fmt.Errorf("sheet.db %s.%s holds a value of a class or a length the writer does not store there", table, name)
}

// storedRows are the most rows the writer leaves in a table of sheet.db
// whose count no other limit bounds (maxOutboxRows). The rows of the
// tables the load reads row by row are bounded as the load reads them,
// and those of the stored calls, the sessions, their receipts and the
// groups by the time they are kept: a day, a day, the newest of each
// session and minutes.
var storedRows = map[string]int{"outbox": maxOutboxRows, "snapshots": maxSnapshotRows, "sqlite_sequence": 1}

// checkRows refuses a table of sheet.db with more rows than the writer
// leaves (storedRows), counting one more at most.
func checkRows(ctx context.Context, q querier, table string) error {
	limit, ok := storedRows[table]
	if !ok {
		return nil
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM `+table+` LIMIT ?)`, limit+1).Scan(&n); err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("sheet.db %s holds more than the %d rows the writer leaves", table, limit)
	}
	return nil
}

// checkStored refuses a table of sheet.db that holds more rows than the
// writer leaves (checkRows) or a value of another class or longer than
// its column takes (storedTables), reading only the class and the length
// of each value. point is called every loadCheckCells rows, and the check
// ends when it returns false.
func checkStored(ctx context.Context, q querier, table string, point func() bool) error {
	if err := checkRows(ctx, q, table); err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, `SELECT `+storedFault(table, "")+` FROM `+table)
	if err != nil {
		return err
	}
	for read := 1; rows.Next(); read++ {
		if read%loadCheckCells == 0 && !point() {
			rows.Close()
			return errLoadStopped
		}
		var fault int64
		if err := rows.Scan(&fault); err != nil {
			rows.Close()
			return err
		}
		if err := faultError(table, fault); err != nil {
			rows.Close()
			return err
		}
	}
	return closeRows(rows)
}

// largestRecord is the most bytes a row of the table takes as SQLite
// stores it: each value at its longest, a number in nine bytes at most,
// and a header of a serial type of nine bytes for each value.
func largestRecord(table string) int {
	n := binary.MaxVarintLen64
	for _, c := range storedTables[table] {
		n += 2 * binary.MaxVarintLen64
		if c.classes&textOrBlob != 0 {
			n += c.max
		}
	}
	return n
}

// preflightPages is the number of pages of sheet.db the check of the
// rows' lengths reads between two check points: up to 16 MiB at the
// largest page size.
const preflightPages = 256

// preflight checks sheet.db before anything reads its values: the length
// of the rows of the tables with a column of real affinity (checkRecords),
// the class of every value of a column of numbers (checkNumbers), and then
// its structure (quick_check), which reads such values whole when they are
// of another class. point is called with the phase "preflight" every
// preflightPages pages and every loadCheckCells rows, and with
// "integrity" before quick_check, and the check ends when it returns
// false. quick_check is one statement that SQLite can interrupt, when ctx
// ends, but not pause, so it holds the caller's slot at the gate for its
// length, which the size of the file bounds.
func preflight(ctx context.Context, q querier, point func(phase string) bool) error {
	scan := func() bool { return point("preflight") }
	if err := checkRecords(ctx, q, scan); err != nil {
		return err
	}
	if err := checkNumbers(ctx, q, scan); err != nil {
		return err
	}
	if !point("integrity") {
		return errLoadStopped
	}
	var integrity string
	if err := q.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("sheet.db integrity: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("sheet.db integrity %q", integrity)
	}
	return nil
}

// Preflight checks a copy of sheet.db as a load checks it before its
// quick_check: the schema of this release, and no row or number that
// quick_check would read whole. A database without tables, which a crash
// can leave before the schema, passes. The backup check runs it before its
// own quick_check of a copied sheet.db. It ends when ctx does.
func Preflight(ctx context.Context, db *sql.DB) error {
	version, err := checkSchema(ctx, db)
	if err != nil || version == 0 {
		return err
	}
	err = checkRecords(ctx, db, func() bool { return ctx.Err() == nil })
	if err == nil {
		err = checkNumbers(ctx, db, func() bool { return ctx.Err() == nil })
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// newSheetPages bounds the pages of a sheet.db before its first commit:
// its schema and the row of meta take 23.
const newSheetPages = 64

// checkNew checks a sheet.db before its first commit, which Open writes at
// once: a file of at most newSheetPages pages, so that its preflight is
// short, then the preflight.
func checkNew(ctx context.Context, q querier) error {
	var pages int64
	if err := q.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return fmt.Errorf("sheet.db page count: %w", err)
	}
	if pages > newSheetPages {
		return fmt.Errorf("sheet.db holds %d pages before its first commit, more than the %d of a new sheet", pages, newSheetPages)
	}
	return preflight(ctx, q, func(string) bool { return ctx.Err() == nil })
}

// checkRecords refuses a sheet.db with a row longer than the writer
// stores in a table with a column of real affinity. SQLite reads a value
// of such a column whole even for typeof, to apply the affinity, so the
// length of the row bounds it first. dbstat reads the length of each row
// from its page, without the row, one row of its own for each page.
func checkRecords(ctx context.Context, q querier, point func() bool) error {
	for _, table := range slices.Sorted(maps.Keys(storedTables)) {
		if !slices.ContainsFunc(storedTables[table], func(c storedColumn) bool { return c.classes&classReal != 0 }) {
			continue
		}
		rows, err := q.QueryContext(ctx, `SELECT mx_payload FROM dbstat WHERE name=?`, table)
		if err != nil {
			return fmt.Errorf("sheet.db %s: %w", table, err)
		}
		for read := 1; rows.Next(); read++ {
			if read%preflightPages == 0 && !point() {
				rows.Close()
				return errLoadStopped
			}
			var longest int64
			if err := rows.Scan(&longest); err != nil {
				rows.Close()
				return fmt.Errorf("sheet.db %s: %w", table, err)
			}
			if longest > int64(largestRecord(table)) {
				rows.Close()
				return fmt.Errorf("sheet.db %s holds a row of %d bytes, more than the %d the writer stores", table, longest, largestRecord(table))
			}
		}
		if err := closeRows(rows); err != nil {
			return fmt.Errorf("sheet.db %s: %w", table, err)
		}
	}
	return nil
}

// checkNumbers refuses a sheet.db with a value of another class in a
// column the writer stores only numbers or NULL in. quick_check reads a
// text in a column of numeric affinity whole, and a blob in one of real
// affinity, to see whether the affinity would have made it a number; the
// class alone, which SQLite reads from the row's header, tells such a
// value apart first. Like quick_check, it goes through every row once,
// one row of its result for each, so that it reaches its check points.
func checkNumbers(ctx context.Context, q querier, point func() bool) error {
	for _, table := range slices.Sorted(maps.Keys(storedTables)) {
		var fits []string
		for _, c := range storedTables[table] {
			if c.classes&textOrBlob == 0 {
				fits = append(fits, c.fits(c.name))
			}
		}
		if len(fits) == 0 {
			continue
		}
		rows, err := q.QueryContext(ctx, `SELECT NOT (`+strings.Join(fits, " AND ")+`) FROM `+table)
		if err != nil {
			return fmt.Errorf("sheet.db %s: %w", table, err)
		}
		for read := 1; rows.Next(); read++ {
			if read%loadCheckCells == 0 && !point() {
				rows.Close()
				return errLoadStopped
			}
			var other bool
			if err := rows.Scan(&other); err != nil {
				rows.Close()
				return fmt.Errorf("sheet.db %s: %w", table, err)
			}
			if other {
				rows.Close()
				return fmt.Errorf("sheet.db %s holds a value of a class the writer does not store there", table)
			}
		}
		if err := closeRows(rows); err != nil {
			return fmt.Errorf("sheet.db %s: %w", table, err)
		}
	}
	return nil
}

// checkedTables holds the tables of sheet.db whose values have been
// checked (checkStored). The writer stores only values that pass, so a
// table that passed stays so.
type checkedTables struct {
	mu sync.Mutex
	ok map[string]bool
}

func (c *checkedTables) mark(tables ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok == nil {
		c.ok = map[string]bool{}
	}
	for _, t := range tables {
		c.ok[t] = true
	}
}

func (c *checkedTables) markAll() {
	for table := range storedTables {
		c.mark(table)
	}
}

func (c *checkedTables) has(table string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ok[table]
}

// checkTables checks the tables a read outside the writer uses that the
// load has not checked yet. Such a read runs only once sheet.db is attached
// and has passed its preflight (awaitUsable), which it waits for as an
// agent call waits for a load, for agentPatience, before it is refused as
// calculating. It can run while the load reads on or after the load
// failed, and a key of the table's index that is past its bound would be
// read whole by a search of the index as well. The check of a table that
// passes is kept.
func (in *Instance) checkTables(ctx context.Context, tables ...string) error {
	if err := in.awaitUsable(ctx, agentPatience); err != nil {
		return err
	}
	for _, table := range tables {
		if in.checked.has(table) {
			continue
		}
		if err := checkStored(ctx, in.rdb, table, func() bool { return ctx.Err() == nil }); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		in.checked.mark(table)
	}
	return nil
}

// checkUnwritten refuses a sheet.db without a first commit that holds rows
// beside meta: the first commit is the one that writes them.
func checkUnwritten(ctx context.Context, q querier) error {
	tables := slices.Sorted(maps.Keys(storedTables))
	for _, table := range tables {
		if table == "meta" {
			continue
		}
		var found bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+`)`).Scan(&found); err != nil {
			return err
		}
		if found {
			return fmt.Errorf("sheet.db holds rows in %s before its first commit", table)
		}
	}
	return nil
}

// maxTabOrderBytes is room for the longest tab order the writer stores:
// the ids of maxTabs tabs, below 2^53 and so of at most 16 digits, with
// their commas and brackets.
const maxTabOrderBytes = maxTabs*17 + 1

// decodeTabOrder reads the tab order of sheet.db as the writer spells it
// (tabOrderJSON): one to maxTabs distinct ids.
func decodeTabOrder(order []byte) ([]uint64, error) {
	var ids []uint64
	if len(order) > maxTabOrderBytes || json.Unmarshal(order, &ids) != nil || len(ids) == 0 || len(ids) > maxTabs {
		return nil, fmt.Errorf("sheet.db tab order %.40q names no tab, more than %d or cannot be read", order, maxTabs)
	}
	spelled := []byte{'['}
	for i, id := range ids {
		if slices.Contains(ids[:i], id) {
			return nil, fmt.Errorf("sheet.db tab order names tab %d twice", id)
		}
		if i > 0 {
			spelled = append(spelled, ',')
		}
		spelled = strconv.AppendUint(spelled, id, 10)
	}
	if string(append(spelled, ']')) != string(order) {
		return nil, fmt.Errorf("sheet.db tab order %q is not spelled as the writer spells it", order)
	}
	return ids, nil
}

// storedStyle reads a styles row: its format in the canonical spelling
// appendFormat writes, and the nine stamps encodeStamps9 writes. The writer
// stores no state of the default format without stamps, which is style 0.
// A load tells a blank record from a cell with a format by the format's
// spelling (loadCells), so another spelling of a format would be read two
// ways.
func storedStyle(format, stamps []byte) (styleState, error) {
	p, perr := decodeFormatPatch(format, true)
	if perr != nil {
		return styleState{}, perr
	}
	state := styleState{f: p.f}
	spelled, err := appendFormat(nil, p.f)
	if err == nil && !bytes.Equal(spelled, format) {
		err = fmt.Errorf("the format %.80q is not spelled as the writer spells it", format)
	}
	if err == nil {
		state.stamps, err = decodeStamps9(stamps)
	}
	if err == nil && state == (styleState{}) {
		err = fmt.Errorf("a state of the default format without stamps")
	}
	return state, err
}

// loadWorkbook reads the whole sheet from sheet.db. Formulas are parsed
// once per distinct stored text. Nothing is calculated.
//
// What the load builds is bounded by the limits of a sheet before it is
// built: every field is read only when it is of a class and no longer than
// the writer stores it (storedTables), the tables it does not read row by
// row are checked whole first, the tab order is read before any tab, a tab's
// axes are read only for a tab of the order, a style only for a cell that
// uses it, and the blank records it keeps are bounded as the tick bounds
// them (loadCells). The revision file list of meta is not read: the files
// table holds the same list.
func loadWorkbook(ctx context.Context, q querier) (*Workbook, error) {
	wb := newWorkbook("")
	var store, date sql.NullString
	var order []byte
	var fault int64
	err := q.QueryRowContext(ctx, `SELECT `+storedValues("meta", "", "store", "seq", "reset_seq", "tab_change_seq", "guard_floor",
		"calc_date", "next_tab", "next_row", "next_col", "tab_order", "tab_order_seq")+`,`+storedFault("meta", "")+
		` FROM meta WHERE singleton=1`).Scan(&store, &wb.seq, &wb.resetSeq, &wb.tabChangeSeq,
		&wb.guardFloor, &date, &wb.nextTab, &wb.nextRow, &wb.nextCol, &order, &wb.tabOrderSeq, &fault)
	if err != nil {
		return nil, fmt.Errorf("sheet.db state: %w", err)
	}
	if err := faultError("meta", fault); err != nil {
		return nil, err
	}
	// The tables the load does not read row by row are checked whole
	// first, so that nothing reads a value of them past its bound later.
	for _, table := range slices.Sorted(maps.Keys(storedTables)) {
		if slices.Contains(loadedTables, table) {
			continue
		}
		if err := checkStored(ctx, q, table, func() bool { return loadPoint(ctx, "records") }); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
	}
	wb.store = store.String
	if len(wb.store) != 32 || wb.resetSeq > wb.seq {
		return nil, fmt.Errorf("sheet.db state is damaged")
	}
	if date.String != "" {
		d, ok := formula.ParseDate(date.String)
		if !ok {
			return nil, fmt.Errorf("sheet.db calculation date %q", date.String)
		}
		wb.date = d
	}
	// The order bounds the tabs before any of them is read, so that no
	// more axes are built than a sheet may hold.
	tabOrder, err := decodeTabOrder(order)
	if err != nil {
		return nil, err
	}
	// Each blob of a tab is read only when it is no longer than the axes
	// of a tab of the largest size and its chunk list take.
	rows, err := q.QueryContext(ctx, `SELECT `+storedValues("tabs", "", "id", "born", "name_seq", "settings_seq", "struct_seq",
		"frozen_rows", "frozen_cols", "name", "row_ids", "row_born", "col_ids", "col_born", "chunks")+`,`+storedFault("tabs", "")+` FROM tabs`)
	if err != nil {
		return nil, err
	}
	tabs := map[TabID]*Tab{}
	// The axes of every tab share one pace, so that many small axes reach
	// check points as one large one does.
	axes := loadPace{pause: func() bool { return loadPoint(ctx, "axes") }}
	// The chunks beyond one for each tab are bounded as the cells of the
	// sheet at the last boundary bound them (maxExtraChunks), so that any
	// tab added later still fits the chunks of a sheet.
	extraChunks := 0
	for rows.Next() {
		t := &Tab{widths: map[ColID]width{}, data: map[RowID]*rowData{}}
		var name sql.NullString
		var rowIDs, rowBorn, colIDs, colBorn, chunks []byte
		var fault int64
		if err := rows.Scan(&t.id, &t.born, &t.nameSeq, &t.settingsSeq, &t.structSeq, &t.frozenRows, &t.frozenCols,
			&name, &rowIDs, &rowBorn, &colIDs, &colBorn, &chunks, &fault); err != nil {
			rows.Close()
			return nil, err
		}
		if err := faultError("tabs", fault); err != nil {
			rows.Close()
			return nil, fmt.Errorf("tab %d: %w", t.id, err)
		}
		t.name = name.String
		if !slices.Contains(tabOrder, uint64(t.id)) || uint64(t.id) >= wb.nextTab || !validTabName(t.name) ||
			t.frozenRows < 0 || t.frozenRows > maxFrozenRows || t.frozenCols < 0 || t.frozenCols > maxFrozenCols {
			rows.Close()
			return nil, fmt.Errorf("sheet.db tab %d is not in the tab order or is damaged", t.id)
		}
		if t.rows, err = loadAxis(rowIDs, rowBorn, formula.MaxRows, wb.nextRow, &axes); err == nil {
			t.cols, err = loadAxis(colIDs, colBorn, formula.MaxCols, wb.nextCol, &axes)
		}
		if err == nil {
			t.chunks, err = decodeRowIDs(chunks, maxTabChunks, &axes)
		}
		// The first chunk of a tab starts at 0.
		if err == nil && (len(t.chunks) == 0 || t.chunks[0] != 0) {
			err = fmt.Errorf("sheet.db chunk list does not start at 0")
		}
		if extraChunks += len(t.chunks) - 1; err == nil && extraChunks > maxExtraChunks {
			err = fmt.Errorf("the tabs hold more than the %d chunks beyond one for each tab that a sheet holds", maxExtraChunks)
		}
		if ctx.Err() != nil {
			rows.Close()
			return nil, ctx.Err()
		}
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("sheet.db tab %d: %v", t.id, err)
		}
		tabs[t.id] = t
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	if len(tabs) != len(tabOrder) {
		return nil, fmt.Errorf("sheet.db tab order names %d tabs and there are %d", len(tabOrder), len(tabs))
	}
	for _, id := range tabOrder {
		t := tabs[TabID(id)]
		if wb.tab(t.name) != nil {
			return nil, fmt.Errorf("sheet.db holds two tabs named %q", t.name)
		}
		wb.placeTab(t, len(wb.tabs))
	}
	rows, err = q.QueryContext(ctx, `SELECT `+storedValues("cols", "", "tab", "col", "width", "seq")+`,`+storedFault("cols", "")+` FROM cols`)
	if err != nil {
		return nil, err
	}
	for read := 1; rows.Next(); read++ {
		if read%loadCheckCells == 0 && !loadPoint(ctx, "widths") {
			rows.Close()
			return nil, ctx.Err()
		}
		var tab TabID
		var col ColID
		var w width
		var fault int64
		if err := rows.Scan(&tab, &col, &w.px, &w.seq, &fault); err != nil {
			rows.Close()
			return nil, err
		}
		if err := faultError("cols", fault); err != nil {
			rows.Close()
			return nil, err
		}
		// The writer deletes the width of a column that goes with it, so a
		// tab holds at most a width for each of its columns.
		t := wb.byID[tab]
		if t == nil || w.px < minWidth || w.px > maxWidth {
			rows.Close()
			return nil, fmt.Errorf("sheet.db width of column %d of tab %d", col, tab)
		}
		if _, live := t.cols.pos[uint64(col)]; !live {
			rows.Close()
			return nil, fmt.Errorf("sheet.db width of column %d of tab %d", col, tab)
		}
		t.widths[col] = w
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	// The styles are checked here in id order, and kept only as a cell
	// reads them (loadCells).
	rows, err = q.QueryContext(ctx, `SELECT `+storedValues("styles", "", "id", "format", "stamps")+`,`+storedFault("styles", "")+
		` FROM styles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	styleRows := 0
	var format, stamps sql.RawBytes
	for rows.Next() {
		if styleRows%loadCheckCells == loadCheckCells-1 && !loadPoint(ctx, "styles") {
			rows.Close()
			return nil, ctx.Err()
		}
		var id uint32
		var fault int64
		if err := rows.Scan(&id, &format, &stamps, &fault); err != nil {
			rows.Close()
			return nil, err
		}
		if err := faultError("styles", fault); err != nil {
			rows.Close()
			return nil, err
		}
		if _, err := storedStyle(format, stamps); err != nil || id == 0 {
			rows.Close()
			return nil, fmt.Errorf("sheet.db style %d: %v", id, err)
		}
		styleRows++
		wb.styles.next = uint64(id) + 1
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	if err := wb.loadCells(ctx, q); err != nil {
		return nil, err
	}
	wb.loaded()
	clear(wb.styles.touched)
	// The rows that no cell of the workbook uses go with the next commit
	// (stageStale).
	wb.stale.styles = styleRows > len(wb.styles.byID)
	pace := loadPace{pause: func() bool { return loadPoint(ctx, "formats") }}
	styleIDs := make([]uint32, 0, len(wb.styles.byID))
	for id := range wb.styles.byID {
		if !pace.step(1) {
			return nil, ctx.Err()
		}
		styleIDs = append(styleIDs, id)
	}
	// The sort is one step for each id as well, as the tick counts it
	// (refillRoom).
	if !pace.step(len(styleIDs)) {
		return nil, ctx.Err()
	}
	slices.Sort(styleIDs)
	if !wb.indexStyles(styleIDs, func() bool { return loadPoint(ctx, "formats") }) {
		return nil, ctx.Err()
	}
	if wb.styles.refillDue() && !wb.styles.refill(styleIDs, func() bool { return loadPoint(ctx, "refill") }) {
		return nil, ctx.Err()
	}
	rows, err = q.QueryContext(ctx, `SELECT `+storedValues("files", "", "path", "hash", "size", "dirty_seq", "reserve")+`,`+
		storedFault("files", "")+` FROM files`)
	if err != nil {
		return nil, err
	}
	for read := 1; rows.Next(); read++ {
		if read%loadCheckCells == 0 && !loadPoint(ctx, "files") {
			rows.Close()
			return nil, ctx.Err()
		}
		var f File
		var pathText, hashText sql.NullString
		var dirty, reserve, fault int64
		if err := rows.Scan(&pathText, &hashText, &f.Size, &dirty, &reserve, &fault); err != nil {
			rows.Close()
			return nil, err
		}
		if err := faultError("files", fault); err != nil {
			rows.Close()
			return nil, err
		}
		f.Path, f.Hash = pathText.String, hashText.String
		// The writer names files only as parseFilePath reads them. Under
		// another spelling a change of the file would not find it.
		path, ok := parseFilePath(f.Path)
		if !ok {
			rows.Close()
			return nil, fmt.Errorf("sheet.db file path %q", f.Path)
		}
		if f.Hash != "" {
			if len(wb.files.list) == maxRevisionFiles {
				rows.Close()
				return nil, fmt.Errorf("sheet.db lists more than the %d files of a revision", maxRevisionFiles)
			}
			wb.files.list[f.Path] = f
		}
		if dirty != 0 {
			// The writer marks the first part of a chunk of a live tab,
			// and of a tab that is gone only the axes the last revision
			// holds (markFiles), so the marks are bounded as the tabs and
			// their chunks are.
			t := wb.byID[path.tab]
			chunked := false
			if t != nil {
				_, chunked = slices.BinarySearch(t.chunks, path.first)
			}
			if path.tab != 0 && (t == nil && (!path.axes || f.Hash == "") || path.chunk && (path.part != 0 || !chunked)) {
				rows.Close()
				return nil, fmt.Errorf("sheet.db marks the file %q, which the writer does not mark", f.Path)
			}
			wb.files.mark(path, reserve)
		}
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	// With the marks read, the chunk lists are checked against what the
	// writer leaves, so that no later write passes the chunks of a sheet.
	chunks := loadPace{pause: func() bool { return loadPoint(ctx, "chunks") }}
	for _, t := range wb.tabs {
		if done, err := wb.checkChunks(t, wb.files.chunks, &chunks); !done {
			return nil, ctx.Err()
		} else if err != nil {
			return nil, fmt.Errorf("sheet.db %w", err)
		}
	}
	return wb, nil
}

// cellsQuery reads the cells with their styles. The open checked the class
// of every number of cells and bounded the length of its rows (checkNumbers,
// checkRecords), and the load checked every style before the cells, so of a
// cell only the text is checked here, by its class and its length. The
// order is the table's key, which the scan follows without a sort.
func cellsQuery() string {
	return `SELECT c.tab,c.row,c.col,c.kind,c.num,` + storedValue("cells", "c", "str") +
		`,c.style,c.seq,c.text_seq,s.id IS NOT NULL,s.format,s.stamps,` + storedFault("cells", "c", "str") +
		` FROM cells c LEFT JOIN styles s ON c.style<>0 AND s.id=c.style ORDER BY c.tab,c.row,c.col`
}

// loadedBlank is a blank record that a load holds, with its stamp as the
// tick orders blank records (oldBlanks).
type loadedBlank struct {
	t     *Tab
	row   RowID
	col   ColID
	stamp uint64
}

// loadCells reads the cells of sheet.db into the workbook. A row names its
// style, and the style's row comes with it; a style no cell names is not
// kept.
//
// sheet.db can hold more blank records than the tick keeps: the writer adds
// them between two ticks, and a tick that fails collects none. A load keeps
// at most blankLimit of them, the newest, as a tick that found the sheet
// with more would after a few ticks without edits. Whenever it holds twice
// as many it takes out the older ones, and from then on it passes over a
// blank record no newer than the newest it took out. The guard floor rises
// to that stamp (section 7.5), and the rows stay in sheet.db until the next
// commit deletes them (stageStale). So the load holds at most twice the
// blank records the tick keeps, and the styles of the records it holds.
func (wb *Workbook) loadCells(ctx context.Context, q querier) error {
	rows, err := q.QueryContext(ctx, cellsQuery())
	if err != nil {
		return err
	}
	defer rows.Close()
	parsed := map[string]*formula.Formula{}
	// A load takes turns with the passes of other sheets (calcGate). A cell
	// counts one and one for each 64 bytes of its text, which reading,
	// measuring and counting a formula or a text go through a few times.
	cells := loadPace{pause: func() bool { return loadPoint(ctx, "cells") }}
	limit := wb.blankLimit()
	var blanks []loadedBlank
	var floor uint64
	collected := false
	collect := func() bool {
		var ok bool
		blanks, floor, ok = wb.keepNewest(blanks, limit, floor, &cells)
		collected = true
		return ok
	}
	var format, stamps sql.RawBytes
	for rows.Next() {
		var tab TabID
		var row RowID
		var col ColID
		var kind int64
		var num sql.NullFloat64
		var str sql.NullString
		var styled bool
		var fault int64
		c := &Cell{}
		if err := rows.Scan(&tab, &row, &col, &kind, &num, &str, &c.style, &c.seq, &c.textSeq, &styled,
			&format, &stamps, &fault); err != nil {
			return err
		}
		if err := faultError("cells", fault); err != nil {
			return err
		}
		// The text of each kind is checked against its own bound before it
		// is used: the query reads none longer than the longest of them.
		if n := len(str.String); n > maxCellTextBytes(kind) {
			return fmt.Errorf("sheet.db cell %d/%d/%d text holds %d bytes, more than the %d the writer stores", tab, row, col, n, maxCellTextBytes(kind))
		}
		if !cells.step(1 + len(str.String)/64) {
			return ctx.Err()
		}
		t := wb.byID[tab]
		if t == nil {
			return fmt.Errorf("sheet.db cell %d/%d/%d is in no tab", tab, row, col)
		}
		pos, ok := t.posOf(row, col)
		if !ok || (c.style != 0 && !styled) {
			return fmt.Errorf("sheet.db cell %d/%d/%d is off its axes or lacks its style", tab, row, col)
		}
		// The kind is checked as stored: narrowed to a ContentKind first,
		// 257 would read as a number.
		if kind < 0 || kind > int64(formula.ContentUnreadable) {
			return fmt.Errorf("sheet.db cell %d/%d/%d: content of kind %d", tab, row, col, kind)
		}
		// storedStyle reads every format in the writer's spelling, so a
		// record of no content whose style spells no format is blank.
		blank := formula.ContentKind(kind) == formula.ContentBlank && (c.style == 0 || string(format) == "{}")
		if blank && collected && max(c.seq, c.textSeq) <= floor {
			if str.Valid || num.Valid {
				return fmt.Errorf("sheet.db cell %d/%d/%d: a blank with content", tab, row, col)
			}
			continue
		}
		if c.style != 0 && wb.styles.byID[c.style] == nil {
			state, err := storedStyle(format, stamps)
			if err != nil {
				return fmt.Errorf("sheet.db style %d: %w", c.style, err)
			}
			if _, twice := wb.styles.ids[state]; twice {
				return fmt.Errorf("sheet.db holds one state under two style ids")
			}
			wb.styles.byID[c.style] = &styleEntry{styleState: state}
			wb.styles.ids[state] = c.style
		}
		if err := wb.storedCell(c, formula.ContentKind(kind), num, str, pos, parsed); err != nil {
			return fmt.Errorf("sheet.db cell %d/%d/%d: %w", tab, row, col, err)
		}
		if err := wb.loadCell(t, row, col, pos, c); err != nil {
			return fmt.Errorf("sheet.db: %w", err)
		}
		if !wb.counted(c) {
			blanks = append(blanks, loadedBlank{t, row, col, max(c.seq, c.textSeq)})
			if len(blanks) > 2*limit && !collect() {
				return ctx.Err()
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(blanks) > limit && !collect() {
		return ctx.Err()
	}
	if collected {
		wb.guardFloor = max(wb.guardFloor, floor)
		wb.stale.blanks, wb.stale.floor = true, floor
	}
	return nil
}

// keepNewest takes every blank record of list out of the workbook but the
// newest keep, and with them every record as old as the newest it takes
// out, and returns the rest and that stamp. A style that no cell uses then
// leaves the table. The stamps are found by selection, one step of pace for
// each record each round looks at, so the work is linear in the records on
// average and reaches the check points of the load.
func (wb *Workbook) keepNewest(list []loadedBlank, keep int, floor uint64, pace *loadPace) ([]loadedBlank, uint64, bool) {
	if len(list) <= keep {
		return list, floor, true
	}
	cut, ok := nthStamp(list, len(list)-keep-1, pace)
	if !ok {
		return nil, 0, false
	}
	wb.styles.most = max(wb.styles.most, len(wb.styles.ids))
	kept := 0
	for _, b := range list {
		if !pace.step(1) {
			return nil, 0, false
		}
		if b.stamp > cut {
			list[kept] = b
			kept++
			continue
		}
		c := b.t.cell(b.row, b.col)
		wb.weighCell(c, -1)
		b.t.drop(b.row, b.col)
		if e := wb.styles.byID[c.style]; e != nil && e.refs <= 0 {
			delete(wb.styles.ids, e.styleState)
			delete(wb.styles.byID, c.style)
			delete(wb.styles.touched, c.style)
		}
	}
	clear(list[kept:])
	return list[:kept], max(floor, cut), true
}

// nthStamp returns the stamp that would stand at index k of list sorted by
// stamp, and reorders list. Each round partitions the records around the
// stamp of a random one into older, as old and newer, so that a list of
// equal stamps takes one round.
func nthStamp(list []loadedBlank, k int, pace *loadPace) (uint64, bool) {
	lo, hi := 0, len(list)
	for {
		p := list[lo+mathrand.IntN(hi-lo)].stamp
		lt, gt := lo, hi
		for i := lo; i < gt; {
			if !pace.step(1) {
				return 0, false
			}
			switch s := list[i].stamp; {
			case s < p:
				list[lt], list[i] = list[i], list[lt]
				lt++
				i++
			case s > p:
				gt--
				list[gt], list[i] = list[i], list[gt]
			default:
				i++
			}
		}
		switch {
		case k < lt:
			hi = lt
		case k >= gt:
			lo = gt
		default:
			return p, true
		}
	}
}

// storedCell reads the content of a cells row into c, in the columns
// cellColumns writes for its kind: text in str for a text or a formula,
// otherwise a number in num, and neither for a blank. A date or a time is
// a number that a row file holds as the same date or time (durableNumber).
// A formula and a text meet the limits a typed one does (storedFormula,
// storedUnreadable, storedText). A formula's content holds the text of its shared formula,
// so that the string the row was read into, one for each cell, is not
// kept beside it, where nothing that holds the cell would count it.
func (wb *Workbook) storedCell(c *Cell, kind formula.ContentKind, num sql.NullFloat64, str sql.NullString, pos formula.Pos, parsed map[string]*formula.Formula) error {
	var err error
	switch kind {
	case formula.ContentFormula, formula.ContentUnreadable, formula.ContentText:
		if !str.Valid || num.Valid {
			return fmt.Errorf("content of kind %d without its text", kind)
		}
	case formula.ContentBlank:
		if str.Valid || num.Valid {
			return fmt.Errorf("a blank with content")
		}
	case formula.ContentNumber, formula.ContentDate, formula.ContentTime, formula.ContentDateTime, formula.ContentBool:
		n := num.Float64
		if str.Valid || !num.Valid || math.IsNaN(n) || math.IsInf(n, 0) || (kind == formula.ContentBool && n != 0 && n != 1) || !durableNumber(kind, n) {
			return fmt.Errorf("content of kind %d with the number %v", kind, n)
		}
	default:
		return fmt.Errorf("content of kind %d", kind)
	}
	switch kind {
	case formula.ContentFormula:
		c.f, err = wb.storedFormula(str.String, pos, parsed)
	case formula.ContentUnreadable:
		c.f, err = wb.storedUnreadable(str.String, pos)
	case formula.ContentText:
		err = storedText(str.String)
	}
	if err != nil {
		return err
	}
	if c.f != nil {
		c.content = formulaContent(c.f)
		return nil
	}
	c.content = formula.Content{Kind: kind, Num: num.Float64, Text: str.String}
	c.val = c.content.Value()
	return nil
}

// loadPoints lets a test see the check points of a load: the phase of
// loadWorkbook each is in, and the instance whose pass it is. It is nil
// outside tests.
var loadPoints atomic.Pointer[func(in *Instance, phase string)]

// loadPoint is a check point of a load at the gate (calcPause) in one of
// its phases: the preflight and its integrity check, the axes, the widths,
// the styles, the cells, the indexing of the formats and the files.
func loadPoint(ctx context.Context, phase string) bool {
	if t, _ := ctx.Value(calcTurnKey{}).(*calcTurn); t != nil {
		return t.point(phase)
	}
	return true
}

// point is the check point of a pass that reads storage, in one of its
// phases: a load, and a revision build that runs at the gate
// (loadRevision).
func (t *calcTurn) point(phase string) bool {
	if fn := loadPoints.Load(); fn != nil {
		(*fn)(t.in, phase)
	}
	return t.check()
}

func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	return err
}

// mark restores the dirty mark a files row carries.
func (fs *fileState) mark(p filePath, reserve int64) {
	switch {
	case p.tab == 0:
		fs.manifest = true
	case p.axes:
		fs.changed[p.tab] = true
		fs.axes[p.tab] = true
	default:
		fs.changed[p.tab] = true
		fs.chunks[chunkKey{p.tab, p.first}] = reserve
	}
}

// markFiles notes the revision files an action made dirty and adds what it
// wrote to their reserves (section 16.5). It runs under the journal.
func (x *txn) markFiles() {
	wb := x.wb
	fs := &wb.files
	old := fileState{list: fs.list, manifest: fs.manifest, axes: maps.Clone(fs.axes), chunks: maps.Clone(fs.chunks), changed: maps.Clone(fs.changed)}
	x.j.push(func() { wb.files = old })
	chunk := func(t *Tab, row RowID, add int64) {
		key := chunkKey{t.id, t.chunkOf(row)}
		if _, ok := fs.chunks[key]; !ok {
			fs.chunks[key] = fs.chunkSize(key)
		}
		fs.chunks[key] += add
		fs.changed[t.id] = true
	}
	for ref := range x.cells {
		t := ref.tab
		if wb.byID[t.id] != t {
			continue
		}
		add := int64(0)
		if c := t.cell(ref.row, ref.col); c != nil && wb.counted(c) {
			add = cellBound(c) + 32 + 256
		}
		chunk(t, ref.row, add)
	}
	// The manifest holds the tab order, so a move alone makes it dirty. It
	// changes no tab.
	if x.order {
		fs.manifest = true
	}
	for t, tt := range x.tabs {
		fs.changed[t.id] = true
		if tt.created || tt.deleted || tt.meta {
			fs.manifest = true
		}
		if wb.byID[t.id] != t {
			continue
		}
		if tt.axes || len(tt.widths) > 0 {
			fs.axes[t.id] = true
		}
		for _, row := range tt.delRows {
			chunk(t, row, 0)
		}
		// A tab that comes back, by an undo, a redo or a recovery, comes
		// in one chunk (restoreTab), which is written again: the marks of
		// its chunks left with it.
		if tt.created {
			for _, first := range t.chunks {
				chunk(t, first, 0)
			}
		}
	}
	// A tab that is gone needs no mark of its axes or chunks, since the
	// manifest takes its files out at the next boundary, so tabs created
	// and deleted between two boundaries leave no marks behind. A tab of
	// the last revision keeps the mark of its axes, which tells a load
	// that the tab changed (fileState.mark); another leaves the changed
	// tabs too.
	gone := map[TabID]bool{}
	for t := range x.tabs {
		if wb.byID[t.id] == nil {
			gone[t.id] = true
		}
	}
	if len(gone) > 0 {
		// One pass over the marks takes out those of every tab that is gone.
		for key := range fs.chunks {
			if gone[key.tab] {
				delete(fs.chunks, key)
			}
		}
	}
	for t := range x.tabs {
		if !gone[t.id] {
			continue
		}
		if _, listed := fs.list[axesPath(t.id)]; listed {
			fs.axes[t.id] = true
		} else {
			delete(fs.axes, t.id)
			delete(fs.changed, t.id)
		}
	}
}

func cellColumns(c *Cell) (kind int, num, str any) {
	k := c.content
	switch k.Kind {
	case formula.ContentBlank:
		return 0, nil, nil
	case formula.ContentText, formula.ContentFormula, formula.ContentUnreadable:
		return int(k.Kind), nil, k.Text
	}
	return int(k.Kind), k.Num, nil
}

func tabOrderJSON(wb *Workbook) string {
	out := []byte{'['}
	for i, t := range wb.tabs {
		if i > 0 {
			out = append(out, ',')
		}
		out = strconv.AppendUint(out, uint64(t.id), 10)
	}
	return string(append(out, ']'))
}

func stageMeta(ctx context.Context, tx *sql.Tx, wb *Workbook) error {
	_, err := tx.ExecContext(ctx, `UPDATE meta SET seq=?,reset_seq=?,tab_change_seq=?,guard_floor=?,calc_date=?,next_tab=?,next_row=?,next_col=?,
		tab_order=?,tab_order_seq=? WHERE singleton=1`, wb.seq, wb.resetSeq, wb.tabChangeSeq, wb.guardFloor, wb.date.String(),
		wb.nextTab, wb.nextRow, wb.nextCol, tabOrderJSON(wb), wb.tabOrderSeq)
	return err
}

func stageTab(ctx context.Context, tx *sql.Tx, t *Tab) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO tabs(id,name,name_key,born,name_seq,settings_seq,struct_seq,frozen_rows,frozen_cols,
		row_ids,row_born,col_ids,col_born,chunks) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name,name_key=excluded.name_key,born=excluded.born,name_seq=excluded.name_seq,
		settings_seq=excluded.settings_seq,struct_seq=excluded.struct_seq,frozen_rows=excluded.frozen_rows,frozen_cols=excluded.frozen_cols,
		row_ids=excluded.row_ids,row_born=excluded.row_born,col_ids=excluded.col_ids,col_born=excluded.col_born,chunks=excluded.chunks`,
		t.id, t.name, formula.FoldName(t.name), t.born, t.nameSeq, t.settingsSeq, t.structSeq, t.frozenRows, t.frozenCols,
		encodeIDs(t.rows.ids), encodeStamps(t.rows.born), encodeIDs(t.cols.ids), encodeStamps(t.cols.born), encodeRowIDs(t.chunks))
	return err
}

// stage writes the changes of an action to sheet.db: changed cells, tabs,
// styles, widths and the dirty marks of the revision files.
func stage(ctx context.Context, tx *sql.Tx, x *txn) error {
	wb := x.wb
	if err := stageStale(ctx, tx, x); err != nil {
		return err
	}
	if err := stageMeta(ctx, tx, wb); err != nil {
		return err
	}
	for t, tt := range x.tabs {
		live := wb.byID[t.id] == t
		if !live {
			for _, query := range []string{`DELETE FROM cells WHERE tab=?`, `DELETE FROM cols WHERE tab=?`, `DELETE FROM tabs WHERE id=?`} {
				if _, err := tx.ExecContext(ctx, query, t.id); err != nil {
					return err
				}
			}
			// The marks of a tab that is gone leave with it (markFiles);
			// stageDirty writes again the one it keeps.
			if wb.byID[t.id] == nil {
				prefix := "tabs/" + strconv.FormatUint(uint64(t.id), 10) + "/"
				if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE hash='' AND substr(path,1,?)=?`, len(prefix), prefix); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE files SET dirty_seq=0,reserve=0 WHERE hash<>'' AND dirty_seq<>0 AND substr(path,1,?)=?`, len(prefix), prefix); err != nil {
					return err
				}
			}
			continue
		}
		for _, row := range tt.delRows {
			if _, err := tx.ExecContext(ctx, `DELETE FROM cells WHERE tab=? AND row=?`, t.id, row); err != nil {
				return err
			}
			// A row that is live again is written whole below: what memory
			// holds is what sheet.db gets.
			if rd := t.data[row]; rd != nil {
				for _, s := range rd.cells {
					x.cells[cellRef{t, row, s.col}] = x.epoch
				}
			}
		}
		for _, col := range tt.widths {
			var err error
			if w, ok := t.widths[col]; ok {
				_, err = tx.ExecContext(ctx, `INSERT INTO cols(tab,col,width,seq) VALUES(?,?,?,?) ON CONFLICT(tab,col) DO UPDATE SET width=excluded.width,seq=excluded.seq`, t.id, col, w.px, w.seq)
			} else {
				_, err = tx.ExecContext(ctx, `DELETE FROM cols WHERE tab=? AND col=?`, t.id, col)
			}
			if err != nil {
				return err
			}
		}
	}
	// Names can pass from one tab to another inside one action. Free every
	// touched name first so that no order of the rows below breaks the
	// unique key.
	for t, tt := range x.tabs {
		if wb.byID[t.id] == t && (tt.meta || tt.created) {
			if _, err := tx.ExecContext(ctx, `UPDATE tabs SET name_key=? WHERE id=?`, "\x00"+strconv.FormatUint(uint64(t.id), 10), t.id); err != nil {
				return err
			}
		}
	}
	for t, tt := range x.tabs {
		if wb.byID[t.id] == t && (tt.meta || tt.created || tt.axes) {
			if err := stageTab(ctx, tx, t); err != nil {
				return err
			}
		}
	}
	if len(x.cells) > 0 {
		put, err := tx.PrepareContext(ctx, `INSERT INTO cells(tab,row,col,kind,num,str,style,seq,text_seq) VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(tab,row,col) DO UPDATE SET kind=excluded.kind,num=excluded.num,str=excluded.str,style=excluded.style,seq=excluded.seq,text_seq=excluded.text_seq`)
		if err != nil {
			return err
		}
		defer put.Close()
		del, err := tx.PrepareContext(ctx, `DELETE FROM cells WHERE tab=? AND row=? AND col=?`)
		if err != nil {
			return err
		}
		defer del.Close()
		// The rows go in the order of the table's key. In the map's order
		// each row lands on another page of the table, and an action
		// larger than the page cache reads and writes its pages many
		// times over.
		refs := make([]cellRef, 0, len(x.cells))
		for ref := range x.cells {
			if wb.byID[ref.tab.id] == ref.tab {
				refs = append(refs, ref)
			}
		}
		slices.SortFunc(refs, func(a, b cellRef) int {
			return cmp.Or(cmp.Compare(a.tab.id, b.tab.id), cmp.Compare(a.row, b.row), cmp.Compare(a.col, b.col))
		})
		// For a statement under a context that can end, the driver starts a
		// goroutine that waits for that end, which costs more than writing
		// one row. The rows need none: when ctx ends, the transaction that
		// was begun under it is rolled back, and the next row fails.
		rows := context.WithoutCancel(ctx)
		for _, ref := range refs {
			t := ref.tab
			if c := t.cell(ref.row, ref.col); c != nil {
				kind, num, str := cellColumns(c)
				_, err = put.ExecContext(rows, t.id, ref.row, ref.col, kind, num, str, c.style, c.seq, c.textSeq)
			} else {
				_, err = del.ExecContext(rows, t.id, ref.row, ref.col)
			}
			if err != nil {
				return err
			}
		}
	}
	for id := range wb.styles.touched {
		e := wb.styles.byID[id]
		var err error
		if e == nil || e.refs <= 0 {
			_, err = tx.ExecContext(ctx, `DELETE FROM styles WHERE id=?`, id)
		} else {
			var format []byte
			if format, err = appendFormat(nil, e.f); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO styles(id,format,stamps,refs) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET format=excluded.format,stamps=excluded.stamps,refs=excluded.refs`,
				id, string(format), encodeStamps9(e.stamps), e.refs)
		}
		if err != nil {
			return err
		}
	}
	return stageDirty(ctx, tx, wb, x.seq)
}

// stageStale deletes the rows of sheet.db that the load passed over
// (loadStale), with the first commit after the load. The load took a style
// that only those blank records used out of the table, and the table can
// give its id out again; a later load would read the old records with the
// new state. Every commit that gives an id out stages here first, before
// anything else of the commit, so the tables hold what the load read: the
// predicates find the records the load passed over and the styles no
// stored cell uses, and no row the commit writes, whose style may be an
// id given out again that names another format in sheet.db until the
// commit stages it. The work is a look at each row of the tables, once
// after a load that passed over rows.
func stageStale(ctx context.Context, tx *sql.Tx, x *txn) error {
	wb := x.wb
	stale := wb.stale
	if stale == (loadStale{}) {
		return nil
	}
	if stale.blanks {
		if _, err := tx.ExecContext(ctx, `DELETE FROM cells WHERE kind=0 AND num IS NULL AND str IS NULL AND max(seq,text_seq)<=?
			AND (style=0 OR style IN (SELECT id FROM styles WHERE format='{}'))`, stale.floor); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM styles WHERE id NOT IN (SELECT style FROM cells)`); err != nil {
		return err
	}
	wb.stale = loadStale{}
	x.j.push(func() { wb.stale = stale })
	return nil
}

// stageDirty makes the dirty marks and reserves of the file state durable.
func stageDirty(ctx context.Context, tx *sql.Tx, wb *Workbook, seq uint64) error {
	fs := &wb.files
	mark := func(path string, reserve int64) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO files(path,hash,size,dirty_seq,reserve) VALUES(?,'',0,?,?)
			ON CONFLICT(path) DO UPDATE SET dirty_seq=excluded.dirty_seq,reserve=excluded.reserve`, path, seq, reserve)
		return err
	}
	if fs.manifest {
		if err := mark(manifestPath, 0); err != nil {
			return err
		}
	}
	for id := range fs.axes {
		if err := mark(axesPath(id), 0); err != nil {
			return err
		}
	}
	for key, reserve := range fs.chunks {
		if err := mark(chunkPrefix(key.tab, key.first)+"0.json", reserve); err != nil {
			return err
		}
	}
	return nil
}

// writeAll replaces the content tables of sheet.db with the workbook, for a
// rollback (section 16.3).
func writeAll(ctx context.Context, tx *sql.Tx, wb *Workbook) error {
	for _, table := range []string{"cells", "cols", "styles", "tabs", "files"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return err
		}
	}
	if err := stageMeta(ctx, tx, wb); err != nil {
		return err
	}
	put, err := tx.PrepareContext(ctx, `INSERT INTO cells(tab,row,col,kind,num,str,style,seq,text_seq) VALUES(?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer put.Close()
	for _, t := range wb.tabs {
		if err := stageTab(ctx, tx, t); err != nil {
			return err
		}
		for col, w := range t.widths {
			if _, err := tx.ExecContext(ctx, `INSERT INTO cols(tab,col,width,seq) VALUES(?,?,?,?)`, t.id, col, w.px, w.seq); err != nil {
				return err
			}
		}
		for row, rd := range t.data {
			for _, s := range rd.cells {
				kind, num, str := cellColumns(s.c)
				if _, err := put.ExecContext(ctx, t.id, row, s.col, kind, num, str, s.c.style, s.c.seq, s.c.textSeq); err != nil {
					return err
				}
			}
		}
	}
	for id, e := range wb.styles.byID {
		format, err := appendFormat(nil, e.f)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO styles(id,format,stamps,refs) VALUES(?,?,?,?)`, id, string(format), encodeStamps9(e.stamps), e.refs); err != nil {
			return err
		}
	}
	clear(wb.styles.touched)
	wb.styles.settleLog()
	for _, f := range wb.files.list {
		if _, err := tx.ExecContext(ctx, `INSERT INTO files(path,hash,size,dirty_seq,reserve) VALUES(?,?,?,0,0)`, f.Path, f.Hash, f.Size); err != nil {
			return err
		}
	}
	return nil
}
