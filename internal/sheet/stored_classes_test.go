package sheet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sqliteuri"
)

// classBase is a stopped sheet with a row in every table of sheet.db, as
// the writer stores them, and what its readers need: the token and the id
// of its session, a stored call and the hash of a snapshot.
type classBase struct {
	e        *env
	Token    string `json:"token"`
	SID      string `json:"sid"`
	Call     string `json:"call"`
	Snapshot string `json:"snapshot"`
}

func newClassBase(t *testing.T) *classBase {
	t.Helper()
	ctx := context.Background()
	e := start(t)
	s, token, err := e.in.newSession(ctx, "editor:Ann", "Ann", false)
	if err != nil {
		t.Fatal(err)
	}
	(&person{e: e, s: s}).set("A1", "ann")
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Empty"},{"op":"write","tab":"Sheet1","at":"B1","rows":[[1,"text","=B1+1",true]]},
		{"op":"format","tab":"Sheet1","ranges":["B1"],"format":{"bold":true}},{"op":"widths","tab":"Sheet1","cols":"B:B","width":220}]}`)
	rev := closeRevision(e)
	files, _, release, err := e.b.RevisionFiles(ctx, rev)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.in.Rollback(ctx, files, "Ann", "sheet-rollback:1:x"); err != nil {
		t.Fatal(err)
	}
	release()
	call := `{"state":"` + e.state() + `","op_id":"call-1","ops":[{"op":"write","tab":"Sheet1","at":"C2","rows":[["=1/0"]]}]}`
	if _, err := e.agent(call); err != nil {
		t.Fatal(err)
	}
	e.in.Stop()
	b := &classBase{e: e, Token: token, SID: s.id, Call: call}
	// The writer's groups close when the instance stops; this one is as
	// the writer stores an open group.
	execDB(t, e, func(db *sql.DB) error {
		now := e.clock.Now().Unix()
		if _, err := db.Exec(`INSERT INTO groups(editor,label,verified,started,last,dirty) VALUES('editor:Bob','Bob',0,?,?,1)`, now, now); err != nil {
			return err
		}
		if err := db.QueryRow(`SELECT hash FROM snapshots LIMIT 1`).Scan(&b.Snapshot); err != nil {
			return err
		}
		for table := range storedTables {
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n == 0 {
				return fmt.Errorf("the base sheet holds %d rows in %s: %v", n, table, err)
			}
		}
		return nil
	})
	return b
}

// liveColumn is a column of a table of a sheet.db as SQLite lists it.
type liveColumn struct {
	table, name, typ string
	pk               bool
}

// liveColumns lists every column of every table of the sheet.db in dir,
// SQLite's own sqlite_sequence among them, from the database itself.
func liveColumns(t *testing.T, dir string) []liveColumn {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteuri.File(filepath.Join(dir, dbName))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tables []string
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := closeRows(rows); err != nil {
		t.Fatal(err)
	}
	var cols []liveColumn
	for _, table := range tables {
		rows, err := db.Query(`SELECT name, type, pk FROM pragma_table_info(?) ORDER BY cid`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			c := liveColumn{table: table}
			var pk int
			if err := rows.Scan(&c.name, &c.typ, &pk); err != nil {
				t.Fatal(err)
			}
			c.pk = pk > 0
			cols = append(cols, c)
		}
		if err := closeRows(rows); err != nil {
			t.Fatal(err)
		}
	}
	return cols
}

// The list of what the writer stores names every table and column of
// sheet.db and nothing else, in the order of the schema, and the load
// reads each table either row by row or checks it whole first.
func TestStoredTablesNameEveryColumn(t *testing.T) {
	e := start(t)
	e.in.Stop()
	live := map[string][]string{}
	for _, c := range liveColumns(t, e.dir) {
		live[c.table] = append(live[c.table], c.name)
	}
	listed := map[string][]string{}
	for table, cols := range storedTables {
		for _, c := range cols {
			listed[table] = append(listed[table], c.name)
		}
	}
	if !maps.EqualFunc(live, listed, slices.Equal) {
		t.Fatalf("sheet.db has the columns\n%v\nand the list of what the writer stores\n%v", live, listed)
	}
	for _, table := range loadedTables {
		if _, ok := storedTables[table]; !ok {
			t.Errorf("the load reads %s, which sheet.db does not have", table)
		}
	}
}

// Every value of a sheet that holds a row in every table, written by the
// writer, a rollback, a person and an agent call stored under its op_id
// among them, fits its column, and the sheet loads.
func TestStoredValuesOfTheWriterFit(t *testing.T) {
	b := newClassBase(t)
	db := readOnly(t, b.e)
	defer db.Close()
	for _, table := range slices.Sorted(maps.Keys(storedTables)) {
		for _, c := range storedTables[table] {
			var bad int
			var classes string
			err := db.QueryRow(`SELECT count(*) FROM ` + table + ` WHERE NOT ` + c.fits(c.name)).Scan(&bad)
			if err == nil {
				err = db.QueryRow(`SELECT group_concat(DISTINCT typeof(` + c.name + `)) FROM ` + table).Scan(&classes)
			}
			if err != nil {
				t.Fatal(err)
			}
			if bad != 0 {
				t.Errorf("%d values of %s.%s do not fit it (classes %s)", bad, table, c.name, classes)
			}
		}
	}
	e := copied(t, b.e)
	e.open()
	e.in.Stop()
}

// substitution is a value put in one row of a column of sheet.db.
type substitution struct {
	Table, Column, Kind string
	// Value is the SQL of the value.
	Value string
}

// substitutions are an oversized text and blob of n bytes in each column
// of the live schema.
func substitutions(t *testing.T, dir string, n int) []substitution {
	t.Helper()
	var out []substitution
	for _, c := range liveColumns(t, dir) {
		out = append(out,
			substitution{c.table, c.name, "text", fmt.Sprintf("CAST(zeroblob(%d) AS TEXT)", n)},
			substitution{c.table, c.name, "blob", fmt.Sprintf("zeroblob(%d)", n)})
	}
	return out
}

// smallSubstitutions are short values of each class in each column of the
// live schema.
func smallSubstitutions(t *testing.T, dir string) []substitution {
	t.Helper()
	var out []substitution
	for _, c := range liveColumns(t, dir) {
		for kind, value := range map[string]string{"an integer": "7", "a real": "1.5", "a text": "'x'", "a blob": "x'00'", "null": "NULL"} {
			out = append(out, substitution{c.table, c.name, kind, value})
		}
	}
	return out
}

// apply puts the substitution in the first row of its table in the
// sheet.db of dir, through a connection that keeps no log, and returns the
// class SQLite stored, or false when the column cannot hold the value: an
// integer primary key, a constraint, or an affinity that turns the value
// into one the writer stores.
func (s substitution) apply(t *testing.T, dir string) (string, bool) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteuri.File(filepath.Join(dir, dbName))+"?_pragma=journal_mode(DELETE)&_pragma=synchronous(OFF)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var keys []string
	for _, c := range liveColumns(t, dir) {
		if c.table == s.Table && c.pk {
			keys = append(keys, c.name)
		}
	}
	key := "rowid"
	if len(keys) > 0 {
		key = "(" + strings.Join(keys, ",") + ")"
	}
	row := key + ` IN (SELECT ` + strings.Trim(key, "()") + ` FROM ` + s.Table + ` LIMIT 1)`
	var class string
	err = db.QueryRow(`UPDATE ` + s.Table + ` SET ` + s.Column + `=` + s.Value + ` WHERE ` + row + ` RETURNING typeof(` + s.Column + `)`).Scan(&class)
	if err != nil {
		if strings.Contains(err.Error(), "datatype mismatch") || strings.Contains(err.Error(), "constraint failed") {
			return "", false
		}
		t.Fatalf("%s.%s=%s: %v", s.Table, s.Column, s.Kind, err)
	}
	return class, true
}

// fitsClass reports whether a class is one the writer stores in the column.
func fitsClass(table, column, class string) bool {
	c := columnOf(table, column)
	k := map[string]storageClass{"integer": classInteger, "real": classReal, "text": classText, "blob": classBlob, "null": classNull}[class]
	return c.classes&k != 0
}

// readerResult is what each reader of a sheet.db did with it: "refused",
// "read", or "" for a reader that did not run because the sheet did not
// open, and under the reader's name and " error" its refusal.
type readerResult map[string]string

// blobColumns are the columns the backup check reads (BlobHashes).
var blobColumns = []string{"meta.files", "outbox.files", "snapshots.hash", "snapshots.refs"}

// runStoredReaders opens the sheet in dir and runs every reader of
// sheet.db on it: the open and the load, the outbox, the references, a
// snapshot, a stored call sent again, a receipt, a session that comes
// back, a tick that closes the groups, an agent write that would record a
// revision, and the backup check of its file lists. groups is the number
// of groups before and after the tick.
func runStoredReaders(dir string, b *classBase) readerResult {
	out := readerResult{}
	note := func(name string, err error) {
		out[name] = "read"
		if err != nil {
			out[name] = "refused"
			out[name+" error"] = err.Error()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sql.Open("sqlite", sqliteuri.File(filepath.Join(dir, dbName))+"?mode=ro")
	if err == nil {
		_, err = BlobHashes(ctx, db)
		db.Close()
	}
	note("blobs", err)
	clock := newClock()
	in, _, err := Open(dir, newBackend(), Options{Slug: "sheet", Now: clock.Now, Location: time.UTC})
	note("open", err)
	if err != nil {
		return out
	}
	defer in.Stop()
	note("load", in.await(ctx, 0))
	_, err = in.Outbox(ctx, 0, 0)
	note("outbox", err)
	_, err = in.References(ctx)
	note("references", err)
	_, err = in.SnapshotData(ctx, b.Snapshot)
	note("snapshot", err)
	u, err := DecodeUpdate([]byte(b.Call))
	if err == nil {
		_, err = in.Update(ctx, u, 0)
	}
	note("replay", err)
	_, err = in.committedAck(ctx, &session{id: b.SID}, 1, nil)
	note("receipt", err)
	_, err = in.resumeSession(ctx, b.Token)
	note("resume", err)
	count := func() string {
		n, err := groupCount(ctx, dir)
		if err != nil {
			return "unknown: " + err.Error()
		}
		return fmt.Sprint(n)
	}
	before := count()
	clock.Add(time.Hour)
	note("tick", in.execute(ctx, &work{run: func() error { in.maintain(); return nil }}))
	out["groups"] = before + " " + count()
	state := ""
	in.mu.RLock()
	if in.wb != nil {
		state = `"state":"` + stateToken(in.wb.store, in.wb.seq) + `",`
	}
	in.mu.RUnlock()
	write, err := DecodeUpdate([]byte(`{` + state + `"ops":[{"op":"write","tab":"Sheet1","at":"D1","rows":[[1]]}]}`))
	if err == nil {
		_, err = in.Update(ctx, write, 0)
	}
	note("write", err)
	return out
}

// groupCount counts the groups of the sheet.db in dir through a connection
// of its own: a load that refused the sheet closed the instance's.
func groupCount(ctx context.Context, dir string) (int, error) {
	db, err := sql.Open("sqlite", sqliteuri.File(filepath.Join(dir, dbName))+"?mode=ro")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM groups`).Scan(&n)
	return n, err
}

// wantRefused checks what the readers of a sheet.db with the substitution
// did: the open or the load refuses it, every reader of the table refuses
// it, the writer runs none of the work it is given, and the damaged group
// stays.
func wantRefused(t *testing.T, s substitution, got readerResult) {
	t.Helper()
	name := s.Table + "." + s.Column
	fail := func(format string, args ...any) {
		t.Helper()
		t.Errorf("%s holding %s: %s (%v)", name, s.Kind, fmt.Sprintf(format, args...), got)
	}
	if slices.Contains(blobColumns, name) && got["blobs"] != "refused" {
		fail("the backup check read it")
	}
	if got["open"] == "refused" {
		return
	}
	if got["load"] != "refused" {
		fail("the sheet loaded")
	}
	for reader, tables := range map[string][]string{"outbox": {"outbox"}, "references": {"meta", "outbox"},
		"snapshot": {"snapshots"}, "replay": {"commands"}, "receipt": {"receipts"}} {
		if slices.Contains(tables, s.Table) && got[reader] != "refused" {
			fail("the %s reader read it", reader)
		}
	}
	for _, reader := range []string{"resume", "tick", "write"} {
		if got[reader] != "refused" {
			fail("the writer ran the %s", reader)
		}
	}
	if counts := strings.Fields(got["groups"]); len(counts) != 2 || counts[0] != counts[1] {
		fail("the groups went from %s", got["groups"])
	}
}

// A short value of a class the writer does not store in a column, in any
// column of any table, is refused by the open or the load and by every
// reader of its table, and the writer runs nothing. A value the column
// cannot hold, or that its affinity turns into one of the writer's class,
// is passed over.
func TestStoredValuesOfAnotherClassAreRefused(t *testing.T) {
	b := newClassBase(t)
	checked := 0
	for _, s := range smallSubstitutions(t, b.e.dir) {
		dir := t.TempDir()
		copyFile(t, filepath.Join(b.e.dir, dbName), filepath.Join(dir, dbName))
		class, ok := s.apply(t, dir)
		if !ok || fitsClass(s.Table, s.Column, class) {
			continue
		}
		checked++
		wantRefused(t, s, runStoredReaders(dir, b))
	}
	t.Logf("%d substitutions of another class", checked)
	if checked == 0 {
		t.Fatal("no substitution changed a class")
	}
}

// A sheet with no first commit holds nothing beside meta, so its first
// commit writes no row next to one it has not checked: one with a row is
// refused when it opens.
func TestRowsBeforeTheFirstCommitAreRefused(t *testing.T) {
	e := start(t)
	e.in.Stop()
	execDB(t, e, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE meta SET seq=0`)
		return err
	})
	in, _, err := Open(e.dir, e.b, e.opts)
	if err == nil {
		in.Stop()
		t.Fatal("the sheet opened")
	}
	if !strings.Contains(err.Error(), "before its first commit") {
		t.Fatalf("the open refused with %v", err)
	}
}

// The writer's own sqlite_sequence has one row, the outbox's. A second row
// is refused when the sheet opens, which reads two rows of it at most.
func TestSecondSequenceRowIsRefused(t *testing.T) {
	e := start(t)
	e.in.Stop()
	execDB(t, e, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO sqlite_sequence(name,seq) VALUES('other',1)`)
		return err
	})
	if in, _, err := Open(e.dir, e.b, e.opts); err == nil {
		in.Stop()
		t.Fatal("the sheet opened")
	}
}

// A file list of the most files of a revision, each at the longest path
// the writer spells, in meta and in an outbox entry, reads, and a snapshot
// of a pending entry keeps its file out of the hashes a backup needs. A
// list of one file more, of short paths that fit the list's length, is
// refused by its count before the file past it is kept.
func TestFileListsAtTheirLimitsRead(t *testing.T) {
	b := newClassBase(t)
	list := func(n int, path func(i int) string) ([]File, string) {
		files := make([]File, n)
		for i := range files {
			files[i] = File{Path: path(i), Hash: contentHash([]byte{byte(i), byte(i >> 8), byte(i >> 16)}), Size: int64(maxPartBytes)}
		}
		raw, _ := json.Marshal(files)
		return files, string(raw)
	}
	longest := func(i int) string { return chunkPrefix(maxID-1, RowID(maxID-1-uint64(i))) + "9223372036854775807.json" }
	files, raw := list(maxRevisionFiles, longest)
	if len(raw) > maxFileListBytes {
		t.Fatalf("the longest list takes %d bytes, past its bound of %d", len(raw), maxFileListBytes)
	}
	e := copied(t, b.e, `UPDATE meta SET files='`+raw+`'`, `UPDATE outbox SET files=CAST('`+raw+`' AS BLOB)`,
		`INSERT INTO snapshots(hash,data,refs) VALUES('`+files[0].Hash+`',x'00',1)`)
	db := readOnly(t, e)
	defer db.Close()
	got, err := BlobHashes(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < maxRevisionFiles-1 || got[files[0].Hash] || !got[files[1].Hash] {
		t.Fatalf("the hashes a backup needs are %d, with the held one %v", len(got), got[files[0].Hash])
	}
	e.open()
	if refs, err := e.in.References(context.Background()); err != nil || len(refs) < maxRevisionFiles {
		t.Fatalf("the references are %d: %v", len(refs), err)
	}
	if entries, err := e.in.Outbox(context.Background(), 0, 0); err != nil || len(entries[0].Files) != maxRevisionFiles {
		t.Fatalf("the outbox reads %v", err)
	}
	e.in.Stop()

	_, more := list(maxRevisionFiles+1, func(i int) string { return chunkPrefix(1, RowID(i)) + "0.json" })
	if len(more) > maxFileListBytes {
		t.Fatalf("a list of short paths takes %d bytes, past the bound of %d", len(more), maxFileListBytes)
	}
	for _, statement := range []string{`UPDATE meta SET files='` + more + `'`, `UPDATE outbox SET files=CAST('` + more + `' AS BLOB)`} {
		db := readOnly(t, copied(t, b.e, statement))
		if _, err := BlobHashes(context.Background(), db); err == nil || !strings.Contains(err.Error(), "more than") {
			t.Errorf("%.30s: a list of a file more read: %v", statement, err)
		}
		db.Close()
	}
}

// recordsSheet returns a stopped small sheet under heavySlug whose stored
// calls take a load past several check points while it checks them, the
// last of them with an outcome of 8 MiB.
func recordsSheet(t *testing.T) *env {
	t.Helper()
	e := start(t, unhurried, func(o *Options) { o.Slug = heavySlug })
	e.in.Stop()
	execDB(t, e, func(db *sql.DB) error {
		_, err := db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
			INSERT INTO commands(op_id,hash,outcome,seq,outbox_seq,imported,expires) SELECT 'op-'||i,'','{}',1,0,1,0 FROM n`, 3*loadCheckCells)
		if err == nil {
			_, err = db.Exec(`INSERT INTO commands(op_id,hash,outcome,seq,outbox_seq,imported,expires) VALUES('zz',?,zeroblob(?),1,0,1,0)`, "", 8<<20)
		}
		return err
	})
	return e
}

// A load takes turns at the gate while it checks the tables it does not
// read row by row: with every slot taken by loads held there, two small
// loads get a slot from them, and a stop ends them there. Opened again,
// each is refused at the oversized stored call. Meanwhile a sheet whose
// meta is damaged is refused when it opens, without a slot: what an open
// reads before the load is a row of meta and two of sqlite_sequence. It
// runs with a gate of one slot and with several.
func TestLoadsTakeTurnsWhileTheyCheckRecords(t *testing.T) {
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			long := make([]*env, slots)
			for i := range long {
				long[i] = recordsSheet(t)
			}
			damaged := fieldSheet(t)
			execDB(t, damaged, func(db *sql.DB) error {
				_, err := db.Exec(`UPDATE meta SET seq=zeroblob(?)`, 8<<20)
				return err
			})
			w := watchLoads(t)
			w.holdAt("records")
			for _, e := range long {
				in, _, err := Open(e.dir, e.b, e.opts)
				if err != nil {
					t.Fatal(err)
				}
				e.in, e.b.in = in, in
			}
			w.heldAt(t, slots, "records")
			if in, _, err := Open(damaged.dir, damaged.b, damaged.opts); err == nil {
				in.Stop()
				t.Fatal("the sheet with a damaged meta opened")
			}
			handOverAndStop(t, w, slots, "records", []string{"axes", "widths", "styles", "cells", "formats", "refill", "files"}, long)
			for _, e := range long {
				wantLoadRefused(t, e, "a stored call of 8 MiB")
			}
			gateSettles(t, "after the loads", slots, 0)
		})
	}
}

// The orders the load reads cells and styles in are those of the tables'
// keys, which the scans follow: SQLite sorts no unchecked value for them.
func TestLoadOrdersNeedNoSort(t *testing.T) {
	e := start(t)
	e.in.Stop()
	db := readOnly(t, e)
	defer db.Close()
	sorts := func(query string) bool {
		rows, err := db.Query(`EXPLAIN QUERY PLAN ` + query)
		if err != nil {
			t.Fatal(err)
		}
		sorted := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			sorted = sorted || strings.Contains(detail, "TEMP B-TREE")
		}
		if err := closeRows(rows); err != nil {
			t.Fatal(err)
		}
		return sorted
	}
	if !sorts(`SELECT tab FROM cells ORDER BY num`) {
		t.Fatal("an order by a column without an index shows no sort")
	}
	for _, query := range []string{cellsQuery(), `SELECT id FROM styles ORDER BY id`, `SELECT seq FROM outbox ORDER BY seq`} {
		if sorts(query) {
			t.Errorf("%.60s sorts", query)
		}
	}
}
