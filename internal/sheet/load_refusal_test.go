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
// its error; the writer refuses its work with the same code and reason.
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
	for _, c := range []struct {
		name  string
		cause bool
		call  func() error
	}{
		{"a read", true, func() error { _, err := in.Read(ctx, ReadQuery{Tab: "Sheet1", Range: "A1"}); return err }},
		{"a write", true, func() error { _, err := in.Update(ctx, write, 0); return err }},
		{"a stored call", true, func() error { _, err := in.Update(ctx, stored, 0); return err }},
		{"the outbox", true, func() error { _, err := in.Outbox(ctx, 0, 10); return err }},
		{"the references", true, func() error { _, err := in.References(ctx); return err }},
		{"a snapshot", true, func() error { _, err := in.SnapshotData(ctx, contentHash(nil)); return err }},
		{"the check", true, func() error { return in.AwaitChecked(ctx) }},
		{"a rollback", true, func() error { return in.Rollback(ctx, nil, "Ann", "") }},
		{"an acknowledgement", false, func() error { return in.Acknowledge(ctx, 1, 0) }},
		{"a barrier", false, func() error { return in.Barrier(ctx) }},
		{"a person's session", false, func() error { _, _, err := in.newSession(ctx, "editor:ann", "Ann", false); return err }},
	} {
		err := c.call()
		if p := refusal(err); p.Code != "unavailable" || p.Reason != "load" || c.cause && !errors.Is(err, r.err) {
			t.Errorf("%s answered %v, want the load's refusal", c.name, err)
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
		// is no longer than the writer stores one.
		if _, err := e.in.db.Exec(`UPDATE tabs SET name=?`, strings.Repeat("a", 200)); err != nil {
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
