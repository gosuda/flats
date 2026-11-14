package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gosuda/flats/internal/sqliteuri"
	"github.com/gosuda/flats/internal/store"
)

// fixture is a data directory with the parts that a backup reads, built from a
// real flats.db and from hand-made outbox databases that have the tables and
// columns that the backup reads.
type fixture struct {
	t    *testing.T
	root string
	data string
	out  string // the folder that holds the copies
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, root: root, data: filepath.Join(root, "data"), out: filepath.Join(root, "copies")}
	for _, d := range []string{f.data, f.out} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(f.data, nameFlatsDB))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	f.write(nameSecretKey, string(make([]byte, 32)))
	f.write(nameConfig, "{}")
	return f
}

// write puts a file below the data directory.
func (f *fixture) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.data, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// sql runs statements against a database below the data directory, creating it
// when it is not there.
func (f *fixture) sql(rel string, stmts ...string) {
	f.t.Helper()
	p := filepath.Join(f.data, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteuri.File(p))
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			f.t.Fatalf("%s: %v", s, err)
		}
	}
}

// flat adds a row to flats.db.
func (f *fixture) flat(slug string) {
	f.t.Helper()
	st, err := store.Open(filepath.Join(f.data, nameFlatsDB))
	if err != nil {
		f.t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateFlat(context.Background(), store.Flat{Slug: slug, Name: slug, Visibility: store.Private}); err != nil {
		f.t.Fatal(err)
	}
	if err := st.Sync(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// state adds a content_state row with an import cursor.
func (f *fixture) state(slug, typ, storeID string, seq int64) {
	f.t.Helper()
	f.sql(nameFlatsDB, `INSERT INTO content_state(flat, type, phase, seq, gen, import_store, import_seq) VALUES('`+slug+`', '`+typ+`', 'ready', 1, 0, '`+storeID+`', `+strconv.FormatInt(seq, 10)+`)`)
}

func (f *fixture) setCursor(slug string, seq int64) {
	f.t.Helper()
	f.sql(nameFlatsDB, `UPDATE content_state SET import_seq = `+strconv.FormatInt(seq, 10)+` WHERE flat = '`+slug+`'`)
}

// outboxSQL makes an outbox with entries up to last, then acknowledges the entries
// through cursor, as a host does once flats.db has imported them. sqlite_sequence
// stays at last, and the entries after cursor stay in the table.
func outboxSQL(table string, last, cursor int64) []string {
	s := []string{`CREATE TABLE ` + table + `(seq INTEGER PRIMARY KEY AUTOINCREMENT, created_at INTEGER NOT NULL DEFAULT 0)`}
	for i := int64(0); i < last; i++ {
		s = append(s, `INSERT INTO `+table+`(created_at) VALUES(0)`)
	}
	return append(s, `DELETE FROM `+table+` WHERE seq <= `+strconv.FormatInt(cursor, 10))
}

// board adds a board flat whose board.db has the store id and an outbox that
// reached last, and whose flats.db cursor is at cursor.
func (f *fixture) board(slug, storeID string, last, cursor int64) {
	f.t.Helper()
	f.flat(slug)
	f.state(slug, typeBoard, storeID, cursor)
	stmts := []string{
		`CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO meta VALUES('store', '` + storeID + `')`,
		`CREATE TABLE assets(hash TEXT PRIMARY KEY, mime TEXT NOT NULL DEFAULT '')`,
	}
	f.sql("flats/"+slug+"/board/board.db", append(stmts, outboxSQL("outbox", last, cursor)...)...)
}

// notebook adds a notebook flat in the same way.
func (f *fixture) notebook(slug, storeID string, last, cursor int64) {
	f.t.Helper()
	f.flat(slug)
	f.state(slug, typeNotebook, storeID, cursor)
	stmts := []string{
		`CREATE TABLE meta(singleton INTEGER PRIMARY KEY CHECK(singleton = 1), store TEXT NOT NULL)`,
		`INSERT INTO meta VALUES(1, '` + storeID + `')`,
		`CREATE TABLE assets(hash TEXT PRIMARY KEY, mime TEXT NOT NULL DEFAULT '')`,
	}
	f.sql("flats/"+slug+"/notebook/notebook.db", append(stmts, outboxSQL("outbox", last, cursor)...)...)
}

// docs adds a document flat. The docs worker keeps its store id as JSON.
func (f *fixture) docs(slug, storeID string, last, cursor int64) {
	f.t.Helper()
	f.flat(slug)
	f.state(slug, typeDocs, storeID, cursor)
	stmts := []string{
		`CREATE TABLE flats_docs_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO flats_docs_meta VALUES('store', '"` + storeID + `"')`,
	}
	f.sql("flats/"+slug+"/data/db.sqlite", append(stmts, outboxSQL("flats_docs_outbox", last, cursor)...)...)
}

// server adds a server flat with its own database and a file.
func (f *fixture) server(slug string) {
	f.t.Helper()
	f.flat(slug)
	f.sql("flats/"+slug+"/data/db.sqlite", `CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`)
	f.write("flats/"+slug+"/data/files/notes.txt", "guest file")
}

// blob stores content as a blob of the flat and returns its hash.
func (f *fixture) blob(slug, root, content string) string {
	f.t.Helper()
	sum := sha256.Sum256([]byte(content))
	h := hex.EncodeToString(sum[:])
	f.write("flats/"+slug+"/"+root+"/blobs/"+h[:2]+"/"+h, content)
	return h
}

// head makes flats.db say that the flat's head holds the hash.
func (f *fixture) head(slug, hash string) {
	f.t.Helper()
	f.sql(nameFlatsDB, `INSERT INTO content_head(flat, path, hash, size, gen) VALUES('`+slug+`', 'a', '`+hash+`', 1, 0)`)
}

func (f *fixture) options(name string) Options {
	return Options{
		DataDir: f.data, ConfigPath: filepath.Join(f.data, nameConfig),
		Out: filepath.Join(f.out, name), FlatsVersion: "vtest",
	}
}

func (f *fixture) run(o Options) (*Result, error) {
	f.t.Helper()
	return Run(context.Background(), o)
}

func (f *fixture) mustRun(name string) (*Result, string) {
	f.t.Helper()
	res, err := f.run(f.options(name))
	if err != nil {
		f.t.Fatalf("backup: %v", err)
	}
	return res, res.Out
}

// nothingLeft fails the test when the folder of the copies holds anything.
func (f *fixture) nothingLeft() {
	f.t.Helper()
	entries, err := os.ReadDir(f.out)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, e := range entries {
		f.t.Errorf("%s is left in the folder of the copies", e.Name())
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func mustNotExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s exists or cannot be read: %v", p, err)
	}
}
