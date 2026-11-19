package sheet

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// behindLog returns what a crash leaves of a running sheet with a log of at
// least min bytes (loggedSheet), opened with an import cursor of flats.db
// past the end of its outbox, as when flats.db comes from a later moment
// than sheet.db.
func behindLog(t *testing.T, min int64) *env {
	t.Helper()
	var store string
	var last int64
	e, _ := loggedSheet(t, "logged", min, func(e *env) {
		store = e.in.StoreID()
		if err := e.in.rdb.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='outbox'`).Scan(&last); err != nil {
			t.Fatal(err)
		}
	})
	e.opts.ImportStore, e.opts.ImportSeq = store, last+5
	return e
}

// storeBytes returns the hashes of sheet.db and its -wal file in dir.
func storeBytes(t *testing.T, dir string) map[string][sha256.Size]byte {
	t.Helper()
	out := map[string][sha256.Size]byte{}
	for _, name := range []string{dbName, dbName + "-wal"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = sha256.Sum256(data)
	}
	return out
}

// refusedRows returns every row of every table that sheet.db in dir holds
// with the commits of its -wal file, read from a copy of the two files.
func refusedRows(t *testing.T, dir string) string {
	t.Helper()
	copied := t.TempDir()
	for _, name := range []string{dbName, dbName + "-wal"} {
		if _, err := os.Stat(filepath.Join(dir, name)); errors.Is(err, os.ErrNotExist) {
			continue
		}
		copyFile(t, filepath.Join(dir, name), filepath.Join(copied, name))
	}
	db := rawDB(t, filepath.Join(copied, dbName))
	defer db.Close()
	var tables []string
	rows, err := db.Query(`SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name`)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var out strings.Builder
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, fmt.Sprintf("%#v", values))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		slices.Sort(lines)
		fmt.Fprintf(&out, "%s: %d rows\n%s\n", table, len(lines), strings.Join(lines, "\n"))
	}
	return out.String()
}

// loadRefused is a sheet whose load refused it.
type loadRefused struct {
	in    *Instance
	err   error         // the error OnLoadError heard
	calls *atomic.Int32 // how often OnLoadError was called
	log   *logSink
}

// openRefused opens e, whose load refuses it, waits for OnLoadError and
// checks that it has its refusal.
func openRefused(t *testing.T, e *env, cause string) *loadRefused {
	t.Helper()
	r := &loadRefused{calls: &atomic.Int32{}, log: &logSink{}}
	heard := make(chan error, 4)
	e.opts.Log = slog.New(slog.NewTextHandler(r.log, nil))
	e.opts.OnLoadError = func(err error) {
		r.calls.Add(1)
		heard <- err
	}
	in, _, err := Open(e.dir, e.b, e.opts)
	if err != nil {
		t.Fatalf("the open read the log a crash left: %v", err)
	}
	e.in, e.b.in, r.in = in, in, in
	t.Cleanup(in.Stop)
	select {
	case r.err = <-heard:
	case <-time.After(60 * time.Second):
		t.Fatal("OnLoadError was not called")
	}
	if !strings.Contains(fmt.Sprint(r.err), cause) {
		t.Fatalf("OnLoadError heard %v, want %q", r.err, cause)
	}
	return r
}

// wantRefusals checks that every read and write of the refused sheet is
// refused with the load's refusal. The ones that wait for the load carry
// its error; the writer refuses its work, a person's action among it, with
// the same code and reason, and a receipt read is refused as unavailable
// with reason storage (technical design, section 5).
func (r *loadRefused) wantRefusals(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	in := r.in
	update := func(call string) Update {
		u, err := DecodeUpdate([]byte(call))
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	write := update(`{"state":"s_000000000000.1","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["late"]]}]}`)
	stored := update(`{"state":"s_000000000000.1","op_id":"late","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["late"]]}]}`)
	var action []personOp
	if err := strictJSON([]byte(`[{"op":"set","tab":1,"row":1,"col":1,"text":"late","at":[0,0]}]`), &action); err != nil {
		t.Fatal(err)
	}
	late := &session{id: "late"}
	for _, c := range []struct {
		name   string
		reason string
		cause  bool
		call   func() error
	}{
		{"a read", "load", true, func() error { _, err := in.Read(ctx, ReadQuery{Tab: "Sheet1", Range: "A1"}); return err }},
		{"a write", "load", true, func() error { _, err := in.Update(ctx, write, 0); return err }},
		{"a stored call", "load", true, func() error { _, err := in.Update(ctx, stored, 0); return err }},
		{"the outbox", "load", true, func() error { _, err := in.Outbox(ctx, 0, 10); return err }},
		{"the references", "load", true, func() error { _, err := in.References(ctx); return err }},
		{"a snapshot", "load", true, func() error { _, err := in.SnapshotData(ctx, contentHash(nil)); return err }},
		{"the check", "load", true, func() error { return in.AwaitChecked(ctx) }},
		{"a rollback", "load", true, func() error { return in.Rollback(ctx, nil, "Ann", "") }},
		{"an acknowledgement", "load", false, func() error { return in.Acknowledge(ctx, 1, 0) }},
		{"a barrier", "load", false, func() error { return in.Barrier(ctx) }},
		{"a person's session", "load", false, func() error { _, _, err := in.newSession(ctx, "editor:ann", "Ann", false); return err }},
		{"a person's action", "load", false, func() error { _, err := in.submit(ctx, late, 1, 0, action, nil); return err }},
		{"a receipt read", "storage", false, func() error { _, err := in.committedAck(ctx, late, 1, nil); return err }},
	} {
		err := c.call()
		if p := refusal(err); p.Code != "unavailable" || p.Reason != c.reason || c.cause && !errors.Is(err, r.err) {
			t.Errorf("%s answered %v, want the load's refusal with reason %s", c.name, err, c.reason)
		}
	}
}

// noHandle fails the test while the instance holds a handle of sheet.db:
// its connections are closed, and the sheet's directory can be moved,
// which an open handle keeps from happening on some systems.
func (r *loadRefused) noHandle(t *testing.T, dir string) {
	t.Helper()
	for name, db := range map[string]*sql.DB{"writing": r.in.db, "reading": r.in.rdb} {
		if db == nil {
			continue
		}
		if n := db.Stats().OpenConnections; n != 0 {
			t.Fatalf("the refused sheet holds %d %s connections", n, name)
		}
		if err := db.Ping(); err == nil {
			t.Fatalf("the %s connections of the refused sheet are open", name)
		}
	}
	moved := dir + ".moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatalf("the refused sheet keeps its directory from moving: %v", err)
	}
	if err := os.Rename(moved, dir); err != nil {
		t.Fatal(err)
	}
}

// logLengths are the logs a crash leaves in the tests of a refusal: one
// short enough that closeDB checkpoints it on close (walTruncateBytes),
// and one longer.
var logLengths = []struct {
	name string
	min  int64
	long bool
}{{"a short log", copyChunk, false}, {"a long log", 6 * copyChunk, true}}

// A sheet.db behind the import cursor of flats.db, with the log a crash
// left, opens, and its load refuses it. It calls OnLoadError once, after
// it closed its connections, and every read and write is refused with its
// error until the instance stops. The refusal closes the connections
// without a checkpoint, whatever the length of the log, so sheet.db and its
// -wal file keep their bytes through the refusal and the stop, and the
// instance holds no handle of them.
func TestLoadRefusalKeepsTheLog(t *testing.T) {
	ownTempDir(t)
	for _, tc := range logLengths {
		t.Run(tc.name, func(t *testing.T) {
			e := behindLog(t, tc.min)
			if size, _ := logState(t, e.dir); size > walTruncateBytes != tc.long {
				t.Fatalf("the log holds %d bytes", size)
			}
			before := storeBytes(t, e.dir)
			r := openRefused(t, e, "behind flats.db")
			if after := storeBytes(t, e.dir); !maps.Equal(after, before) {
				t.Fatal("the refusal changed sheet.db or its log")
			}
			r.noHandle(t, e.dir)
			r.wantRefusals(t)
			r.wantRefusals(t)
			r.in.Stop()
			if after := storeBytes(t, e.dir); !maps.Equal(after, before) {
				t.Fatal("the stop of the refused sheet changed sheet.db or its log")
			}
			if n := r.calls.Load(); n != 1 {
				t.Fatalf("OnLoadError was called %d times", n)
			}
			if lines := r.log.lines("sheet close"); len(lines) != 0 {
				t.Fatalf("the refusal warns %q", lines)
			}
			noPrivateCopies(t)
		})
	}
}

// When keepLogOnClose fails, as it does once the driver no longer has the
// fields it needs, the refusal still closes its connections, so the
// instance holds no handle. SQLite may then checkpoint the log into
// sheet.db: the rows stay the same. The host's log has the warning.
func TestLoadRefusalWhenKeepingTheLogFails(t *testing.T) {
	ownTempDir(t)
	fault := errors.New("the driver has no SQLite handle")
	for _, tc := range logLengths {
		t.Run(tc.name, func(t *testing.T) {
			e := behindLog(t, tc.min)
			rows := refusedRows(t, e.dir)
			keepLogFault.Store(&fault)
			t.Cleanup(func() { keepLogFault.Store(nil) })
			r := openRefused(t, e, "behind flats.db")
			if got := refusedRows(t, e.dir); got != rows {
				t.Fatalf("the refusal changed the rows:\n%s", dumpDiff(rows, got))
			}
			r.noHandle(t, e.dir)
			r.wantRefusals(t)
			lines := r.log.lines("sheet close")
			if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], fault.Error()) {
				t.Fatalf("the log lines about the close are %q, want a warning with %q", lines, fault)
			}
			r.in.Stop()
			keepLogFault.Store(nil)
			if got := refusedRows(t, e.dir); got != rows {
				t.Fatalf("the stop changed the rows:\n%s", dumpDiff(rows, got))
			}
			if n := r.calls.Load(); n != 1 {
				t.Fatalf("OnLoadError was called %d times", n)
			}
			noPrivateCopies(t)
		})
	}
}

// A damaged file that the preflight passes is refused by the load once the
// reads outside the writer may use the reading connections. The refusal
// closes those too, without a checkpoint, and every read is refused with
// the load's error from then on, as every write is.
func TestLoadRefusalOfADamagedFileKeepsTheLog(t *testing.T) {
	ownTempDir(t)
	e, _ := loggedSheet(t, "logged", copyChunk, func(e *env) {
		// The preflight reads no tab name; the load reads it only when it
		// is no longer than the writer stores one, and a name of one code
		// point more than a tab name has is damage.
		if _, err := e.in.db.Exec(`UPDATE tabs SET name=?`, strings.Repeat("a", maxTabName+1)); err != nil {
			t.Fatal(err)
		}
	})
	if size, _ := logState(t, e.dir); size > walTruncateBytes {
		t.Fatalf("the log of %d bytes is not short", size)
	}
	before := storeBytes(t, e.dir)
	passed := make(chan struct{}, 1)
	ended := func(in *Instance, err error) {
		if err == nil {
			passed <- struct{}{}
		}
	}
	preflightEnded.Store(&ended)
	t.Cleanup(func() { preflightEnded.Store(nil) })
	r := openRefused(t, e, "is damaged")
	select {
	case <-passed:
	default:
		t.Fatal("the preflight refused the damage")
	}
	if r.in.rdb == nil {
		t.Fatal("the load refused the sheet before it opened its reading connections")
	}
	if after := storeBytes(t, e.dir); !maps.Equal(after, before) {
		t.Fatal("the refusal changed sheet.db or its log")
	}
	r.noHandle(t, e.dir)
	r.wantRefusals(t)
	r.in.Stop()
	if after := storeBytes(t, e.dir); !maps.Equal(after, before) {
		t.Fatal("the stop of the refused sheet changed sheet.db or its log")
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("OnLoadError was called %d times", n)
	}
	noPrivateCopies(t)
}

// connectionFiles are the files that a load that reaches the instance's own
// connection may leave in the sheet's directory: sheet.db, and the -wal and
// -shm files SQLite builds beside it for a connection to a WAL database.
var connectionFiles = []string{dbName, dbName + "-wal", dbName + "-shm"}

// onlyConnectionFiles fails the test when dir holds a file other than
// connectionFiles, or when sheet.db, or a -wal file that before holds,
// does not have the bytes it has in before (dirFiles).
func onlyConnectionFiles(t *testing.T, dir string, before map[string][sha256.Size]byte, what string) {
	t.Helper()
	after := dirFiles(t, dir)
	for name := range after {
		if !slices.Contains(connectionFiles, name) {
			t.Fatalf("%s left %s", what, name)
		}
	}
	for _, name := range []string{dbName, dbName + "-wal"} {
		if sum, ok := before[name]; ok && after[name] != sum {
			t.Fatalf("%s changed %s", what, name)
		}
	}
}

// A load that reaches the instance's own connection, one that succeeds and
// one that the cursor check refuses, leaves no file in the sheet's
// directory other than sheet.db and the -wal and -shm files of that
// connection, for a store without a -wal file and one with the -wal file a
// crash left. sheet.db, and a -wal file that was there, keep their bytes
// until the instance's first commit, and through a refusal and the stop
// after it (technical design, sections 5 and 22.3).
func TestLoadRefusalLeavesOnlyTheFilesOfTheConnection(t *testing.T) {
	ownTempDir(t)
	// stopped is a stopped sheet, which leaves no -wal file, and the cursor
	// of a flats.db past the end of its outbox.
	stopped := func(t *testing.T) (*env, string, int64) {
		t.Helper()
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["kept"]]}]}`)
		var last int64
		if err := e.in.rdb.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='outbox'`).Scan(&last); err != nil {
			t.Fatal(err)
		}
		store := e.in.StoreID()
		e.in.Stop()
		if _, err := os.Stat(filepath.Join(e.dir, dbName+"-wal")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a stopped sheet leaves its log: %v", err)
		}
		return e, store, last + 5
	}
	t.Run("without a -wal file, loaded", func(t *testing.T) {
		e, _, _ := stopped(t)
		before := dirFiles(t, e.dir)
		e.open()
		onlyConnectionFiles(t, e.dir, before, "the load")
		e.in.Stop()
		onlyConnectionFiles(t, e.dir, before, "the stop")
	})
	t.Run("without a -wal file, behind the cursor", func(t *testing.T) {
		e, store, seq := stopped(t)
		e.opts.ImportStore, e.opts.ImportSeq = store, seq
		before := dirFiles(t, e.dir)
		if _, _, err := Open(e.dir, e.b, e.opts); err == nil || !strings.Contains(err.Error(), "behind flats.db") {
			t.Fatalf("a sheet.db behind the import cursor opened: %v", err)
		}
		onlyConnectionFiles(t, e.dir, before, "the refused open")
	})
	t.Run("with a -wal file, loaded", func(t *testing.T) {
		e, _ := loggedSheet(t, "logged", copyChunk)
		before := dirFiles(t, e.dir)
		e.open()
		onlyConnectionFiles(t, e.dir, before, "the load")
		e.in.Stop()
		onlyConnectionFiles(t, e.dir, nil, "the stop")
	})
	t.Run("with a -wal file, behind the cursor", func(t *testing.T) {
		e := behindLog(t, copyChunk)
		before := dirFiles(t, e.dir)
		r := openRefused(t, e, "behind flats.db")
		onlyConnectionFiles(t, e.dir, before, "the refusal")
		r.in.Stop()
		onlyConnectionFiles(t, e.dir, before, "the stop of the refused sheet")
	})
	noPrivateCopies(t)
}

// A refusal by the probe, here of a newer schema, adds no file to the
// sheet's directory and changes none: for a store without a -wal file,
// whose open and check refuse it, and one whose -wal file a crash left with
// the newer version in it, whose load refuses it (technical design,
// sections 5 and 22.3). TestDevelopmentBuildDatabaseIsRefused checks the
// same of a sheet from an unreleased build.
func TestLoadRefusalByTheProbeLeavesTheDirectory(t *testing.T) {
	ownTempDir(t)
	const newer = `PRAGMA user_version = 2`
	t.Run("without a -wal file", func(t *testing.T) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["kept"]]}]}`)
		e.in.Stop()
		db := rawDB(t, filepath.Join(e.dir, dbName))
		execAll(t, db, newer)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		before := dirFiles(t, e.dir)
		if _, ok := before[dbName+"-wal"]; ok || len(before) != 1 {
			t.Fatalf("the directory holds %d files", len(before))
		}
		if _, _, err := Open(e.dir, e.b, e.opts); err == nil || !strings.Contains(err.Error(), "newer") {
			t.Fatalf("a newer schema opened: %v", err)
		}
		sameFiles(t, e.dir, before, "the refused open")
		if err := CheckDB(e.dir); err == nil || !strings.Contains(err.Error(), "newer") {
			t.Fatalf("the check of a newer schema: %v", err)
		}
		sameFiles(t, e.dir, before, "the check")
	})
	t.Run("with a -wal file", func(t *testing.T) {
		e, _ := loggedSheet(t, "logged", copyChunk, func(e *env) {
			if _, err := e.in.db.Exec(newer); err != nil {
				t.Fatal(err)
			}
		})
		before := dirFiles(t, e.dir)
		if _, ok := before[dbName+"-wal"]; !ok || len(before) != 2 {
			t.Fatalf("the directory holds %d files", len(before))
		}
		r := openRefused(t, e, "newer")
		sameFiles(t, e.dir, before, "the refusal")
		r.noHandle(t, e.dir)
		r.wantRefusals(t)
		r.in.Stop()
		sameFiles(t, e.dir, before, "the stop of the refused sheet")
	})
	noPrivateCopies(t)
}
