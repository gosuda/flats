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
)

// cursor is the import cursor of one flat in flats.db (content_state).
type cursor struct {
	flat, typ, store string
	seq              int64
}

// outboxPath is the database that holds a flat's outbox, below the data
// directory of the copy (rule 5): the docs worker's data database, board.db or
// notebook.db. It is "" for another content type.
func outboxPath(flat, typ string) string {
	base := join(nameFlats, flat)
	switch typ {
	case typeDocs:
		return join(base, "data", nameDataDB)
	case typeBoard:
		return join(base, "board", nameBoardDB)
	case typeNotebook:
		return join(base, "notebook", nameNotebookDB)
	}
	return ""
}

// openCopy opens a finished copy of a database read-only and immutable, which
// creates no file and changes no byte.
func openCopy(path string) (*sql.DB, error) {
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
}

// readOutbox reads the store id and the highest outbox sequence of a copy of a
// board, notebook or docs worker database, as the host's checkImportCursor does
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
	var storeQuery, seqName string
	switch typ {
	case typeBoard:
		storeQuery, seqName = `SELECT value FROM meta WHERE key = 'store'`, "outbox"
	case typeNotebook:
		storeQuery, seqName = `SELECT store FROM meta WHERE singleton = 1`, "outbox"
	case typeDocs:
		storeQuery, seqName = `SELECT value FROM flats_docs_meta WHERE key = 'store'`, "flats_docs_outbox"
	default:
		return outboxState{}, nil
	}
	var st outboxState
	var raw string
	err = db.QueryRowContext(ctx, storeQuery).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows) || isNoTable(err):
		return outboxState{}, nil
	case err != nil:
		return outboxState{}, err
	}
	st.store = raw
	if typ == typeDocs { // the docs worker stores its values as JSON
		var s string
		if json.Unmarshal([]byte(raw), &s) == nil {
			st.store = s
		}
	}
	err = db.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = ?`, seqName).Scan(&st.last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) && !isNoTable(err) {
		return outboxState{}, err
	}
	st.present = st.store != ""
	return st, nil
}

func isNoTable(err error) bool { return err != nil && strings.Contains(err.Error(), "no such table") }

// behindCursor returns the cursors whose outbox database in the copy dataRoot is
// below the cursor (rule 16). A flat with no cursor, a missing database, an
// outbox that is not there and another store id have nothing to compare.
func behindCursor(ctx context.Context, dataRoot string, cursors []cursor) ([]cursor, error) {
	var behind []cursor
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
		if st.present && st.store == c.store && c.seq > st.last {
			behind = append(behind, c)
		}
	}
	return behind, nil
}
