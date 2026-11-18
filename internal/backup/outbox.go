package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/sqliteuri"
)

// Content types that keep an outbox in a database of their own.
const (
	typeDocs     = "docs"
	typeBoard    = "board"
	typeNotebook = "notebook"
	typeSheet    = "sheet"
)

// cursor is the import cursor of one flat in flats.db (content_state).
type cursor struct {
	flat, typ, store string
	seq              int64
}

// outboxPath is the database that holds a flat's outbox, below the data
// directory of the copy (rule 5): the docs worker's data database, board.db,
// notebook.db or sheet.db. It is "" for another content type.
func outboxPath(flat, typ string) string {
	base := join(nameFlats, flat)
	switch typ {
	case typeDocs:
		return join(base, "data", nameDataDB)
	case typeBoard:
		return join(base, "board", nameBoardDB)
	case typeNotebook:
		return join(base, "notebook", nameNotebookDB)
	case typeSheet:
		return join(base, "sheet", nameSheetDB)
	}
	return ""
}

// openCopy opens a finished copy of a database read-only and immutable, which
// creates no file and changes no byte. A test replaces it to see which checks
// read a file.
var openCopy = func(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteuri.File(path)+"?mode=ro&immutable=1&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// readCursors lists the flats of a copy of flats.db that have an import cursor.
func readCursors(ctx context.Context, flatsDB string) ([]cursor, error) {
	db, err := openCopy(flatsDB)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT flat, type, COALESCE(import_store, ''), COALESCE(import_seq, 0) FROM content_state WHERE COALESCE(import_store, '') <> '' ORDER BY flat`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cursor
	for rows.Next() {
		var c cursor
		if err := rows.Scan(&c.flat, &c.typ, &c.store, &c.seq); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// outboxState is what a copy of an outbox database says about itself.
type outboxState struct {
	present bool // the database has an outbox and a store id
	store   string
	last    int64 // the highest outbox sequence ever assigned
	acked   int64 // the entries through this sequence are acknowledged: gone from the outbox
}

// readOutbox reads the store id and the sequence numbers of a copy of a board,
// notebook, sheet or docs worker database, as the host's checkImportCursor does
// (rule 16).
func readOutbox(ctx context.Context, path, typ string) (outboxState, error) {
	if typ == typeNotebook {
		if err := notebook.RegisterCollation(); err != nil {
			return outboxState{}, err
		}
	}
	db, err := openCopy(path)
	if err != nil {
		return outboxState{}, err
	}
	defer db.Close()
	// The row of the store id is found by a key that is compared only when
	// it is of the class and a length the writer stores, so a damaged key is
	// not read whole either.
	const key = `CASE WHEN typeof(key)='text' AND octet_length(key)<=64 THEN key END = 'store'`
	var column, from, table string
	switch typ {
	case typeBoard:
		column, from, table = "value", `meta WHERE `+key, "outbox"
	case typeNotebook, typeSheet:
		column, from, table = "store", `meta WHERE CASE WHEN typeof(singleton)='integer' THEN singleton END = 1`, "outbox"
	case typeDocs:
		column, from, table = "value", `flats_docs_meta WHERE `+key, "flats_docs_outbox"
	default:
		return outboxState{}, nil
	}
	// The store id is read only when it is no longer than any store id, so
	// a damaged one of any size is refused without being read.
	var st outboxState
	var raw sql.NullString
	var size sql.NullInt64
	err = db.QueryRowContext(ctx, fmt.Sprintf(`SELECT CASE WHEN octet_length(%[1]s)<=%[2]d THEN %[1]s END,octet_length(%[1]s) FROM %[3]s`,
		column, maxStoreBytes, from)).Scan(&raw, &size)
	switch {
	case errors.Is(err, sql.ErrNoRows) || isNoTable(err):
		return outboxState{}, nil
	case err != nil:
		return outboxState{}, err
	case size.Int64 > maxStoreBytes:
		return outboxState{}, fmt.Errorf("the store id of %s holds %d bytes", path, size.Int64)
	case !raw.Valid:
		return outboxState{}, fmt.Errorf("the store id of %s is NULL", path)
	}
	st.store = raw.String
	if typ == typeDocs { // the docs worker stores its values as JSON
		var s string
		if json.Unmarshal([]byte(raw.String), &s) == nil {
			st.store = s
		}
	}
	// The sequence is read only as a number, from the row of a name no
	// longer than a table's, so a damaged value of either is not read
	// whole.
	var last sql.NullInt64
	err = db.QueryRowContext(ctx, `SELECT CASE WHEN typeof(seq)='integer' THEN seq END FROM sqlite_sequence
		WHERE CASE WHEN typeof(name)='text' AND octet_length(name)<=64 THEN name END = ?`, table).Scan(&last)
	switch {
	case errors.Is(err, sql.ErrNoRows) || isNoTable(err):
	case err != nil:
		return outboxState{}, err
	case !last.Valid:
		return outboxState{}, fmt.Errorf("the outbox sequence of %s is not a number", path)
	default:
		st.last = last.Int64
	}
	// The oldest entry is taken from the sequences that are numbers, as the
	// writer stores them, and an entry with another is refused, so that no
	// sequence is read whole.
	var oldest sql.NullInt64
	var other int64
	err = db.QueryRowContext(ctx, `SELECT MIN(CASE WHEN typeof(seq)='integer' THEN seq END),count(CASE WHEN typeof(seq)<>'integer' THEN 1 END) FROM `+
		table).Scan(&oldest, &other)
	switch {
	case isNoTable(err):
	case err != nil:
		return outboxState{}, err
	case other > 0:
		return outboxState{}, fmt.Errorf("the outbox of %s holds %d entries whose sequence is not a number", path, other)
	}
	st.acked = st.last
	if oldest.Valid {
		st.acked = oldest.Int64 - 1
	}
	st.present = st.store != ""
	return st, nil
}

// maxStoreBytes is room for the store id of any outbox database: 32 hex
// digits, JSON-quoted in a docs database.
const maxStoreBytes = 64

func isNoTable(err error) bool { return err != nil && strings.Contains(err.Error(), "no such table") }

// incoherent is a flat whose outbox database and import cursor cannot come from
// one moment of a running host.
type incoherent struct {
	cursor
	last, acked int64
}

func (b incoherent) String() string {
	if b.seq > b.last {
		return fmt.Sprintf("%s (its outbox reached %d, flats.db imported through %d)", b.flat, b.last, b.seq)
	}
	return fmt.Sprintf("%s (its outbox acknowledged entries through %d, flats.db imported through %d)", b.flat, b.acked, b.seq)
}

func (b incoherent) reason() string {
	if b.seq > b.last {
		return fmt.Sprintf("is below the import cursor %d of flats.db: its outbox reached %d", b.seq, b.last)
	}
	return fmt.Sprintf("has acknowledged entries through %d, and the import cursor of flats.db is %d, so revisions are missing", b.acked, b.seq)
}

// incoherentCursors returns the flats whose outbox database in the copy dataRoot
// does not fit its cursor in flats.db (rule 16). The cursor of a running host is
// never below the entries that the outbox has acknowledged and never above the
// highest entry that it has made: acknowledged <= cursor <= last. A flat with no
// cursor, a missing database, an outbox that is not there and another store id
// have nothing to compare.
func incoherentCursors(ctx context.Context, dataRoot string, cursors []cursor) ([]incoherent, error) {
	var bad []incoherent
	for _, c := range cursors {
		rel := outboxPath(c.flat, c.typ)
		if rel == "" {
			continue
		}
		path := filepath.Join(dataRoot, filepath.FromSlash(rel))
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		st, err := readOutbox(ctx, path, c.typ)
		if err != nil {
			return nil, fmt.Errorf("read the outbox of %s: %w", c.flat, err)
		}
		if st.present && st.store == c.store && (c.seq > st.last || st.acked > c.seq) {
			bad = append(bad, incoherent{c, st.last, st.acked})
		}
	}
	return bad, nil
}

// liveCursors lists the flats that have an import cursor in the live flats.db
// and their content types. A flat whose cursor appears later is found by the
// check of the copy and snapshotted again.
func liveCursors(ctx context.Context, flatsDB string) (map[string]string, error) {
	db, err := sql.Open("sqlite", sqliteuri.File(flatsDB)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, `SELECT flat, type FROM content_state WHERE COALESCE(import_store, '') <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var flat, typ string
		if err := rows.Scan(&flat, &typ); err != nil {
			return nil, err
		}
		out[flat] = typ
	}
	return out, rows.Err()
}
