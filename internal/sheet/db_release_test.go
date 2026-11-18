package sheet

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sqliteuri"
)

// dirFiles returns the name and a hash of the content of every file in dir.
func dirFiles(t *testing.T, dir string) map[string][sha256.Size]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][sha256.Size]byte{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = sha256.Sum256(data)
	}
	return files
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteuri.File(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func execAll(t *testing.T, db *sql.DB, queries ...string) {
	t.Helper()
	for _, query := range queries {
		if _, err := db.Exec(query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

// sameFiles fails the test unless dir holds exactly the files of before,
// with the same contents.
func sameFiles(t *testing.T, dir string, before map[string][sha256.Size]byte, what string) {
	t.Helper()
	after := dirFiles(t, dir)
	for name, sum := range before {
		if got, ok := after[name]; !ok {
			t.Fatalf("%s removed %s", what, name)
		} else if got != sum {
			t.Fatalf("%s changed %s", what, name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Fatalf("%s created %s", what, name)
		}
	}
}

// wantDevelopmentRefusal fails the test unless err is the refusal of a
// development build's database that names differs.
func wantDevelopmentRefusal(t *testing.T, err error, differs, what string) {
	t.Helper()
	var refusal *WriteError
	if !errors.As(err, &refusal) || refusal.Code != "unavailable" || refusal.Reason != "load" ||
		!strings.Contains(refusal.Detail, "development build") || !strings.Contains(refusal.Detail, differs) {
		t.Fatalf("%s failed with %v, want a load refusal naming %q", what, err, differs)
	}
}

// ownTempDir gives the test a temporary directory of its own, which the
// probe's private copies go to.
func ownTempDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, dir)
	}
}

// noPrivateCopies fails the test when a private copy of the probe is
// left in the temporary directory (ownTempDir).
func noPrivateCopies(t *testing.T) {
	t.Helper()
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "flats-sheet-inspect-*")); len(left) != 0 {
		t.Fatalf("the probe left %v", left)
	}
}

// refusedTwice opens dir and checks it, twice each, and fails the test
// unless each is refused with a detail naming differs, the directory
// holds the files it held before, with their contents, and no private
// copy is left. A sheet.db with a log a crash left opens, and its load
// refuses it; the check does not read it.
func refusedTwice(t *testing.T, dir string, b *fakeBackend, opts Options, differs string) {
	t.Helper()
	before := dirFiles(t, dir)
	_, err := os.Stat(filepath.Join(dir, dbName+"-wal"))
	logged := err == nil
	for attempt := 0; attempt < 2; attempt++ {
		in, _, err := Open(dir, b, opts)
		if logged {
			if err != nil {
				t.Fatalf("the open read the log a crash left: %v", err)
			}
			err = in.await(context.Background(), 0)
			in.Stop()
		} else if err == nil {
			in.Stop()
			t.Fatal("the database of a development build opened")
		}
		wantDevelopmentRefusal(t, err, differs, "opening it")
		sameFiles(t, dir, before, "the refused open")
		if logged {
			if err := CheckDB(dir); err != nil {
				t.Fatalf("the check read the log a crash left: %v", err)
			}
		} else {
			wantDevelopmentRefusal(t, CheckDB(dir), differs, "checking it")
		}
		sameFiles(t, dir, before, "the check")
		noPrivateCopies(t)
	}
}

// restored puts a backup of the sheet.db in from into a new directory, as
// flats backup makes one and a restore puts it in place: VACUUM INTO an
// empty file from a read-only connection.
func restored(t *testing.T, from string) string {
	t.Helper()
	dir := t.TempDir()
	dst := filepath.Join(dir, dbName)
	if err := os.WriteFile(dst, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := sql.Open("sqlite", sqliteuri.File(filepath.Join(from, dbName))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec(`VACUUM INTO ?`, dst); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDevelopmentBuildDatabaseIsRefused opens a sheet.db of version 1 in
// each form that unreleased development builds left: without the release
// mark, without the session index, without the group mark, with the group
// mark those builds added on opening, from a backup of such a database,
// and with a log, and a log and its index, that a crash left, also when
// only the log holds the difference and when the probe cannot link
// sheet.db and copies it. No released
// build had the sheet type, so a database of version 1 that this release
// did not create comes from such a build, and opening or checking any of
// them fails with a refusal that names the difference. Neither writes to
// the directory, so it holds the same files with the same contents, and a
// second attempt fails the same way.
func TestDevelopmentBuildDatabaseIsRefused(t *testing.T) {
	ownTempDir(t)
	release := start(t)
	release.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["kept"]]}]}`)
	release.in.Stop()
	if _, err := os.Stat(filepath.Join(release.dir, dbName+"-wal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stopped sheet leaves its log: %v", err)
	}
	// developed copies the release database to a new directory and turns it
	// into what a development build wrote.
	developed := func(t *testing.T, queries ...string) string {
		t.Helper()
		dir := t.TempDir()
		copyFile(t, filepath.Join(release.dir, dbName), filepath.Join(dir, dbName))
		db := rawDB(t, filepath.Join(dir, dbName))
		execAll(t, db, queries...)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	// crashed copies what a crash leaves of a development build's database,
	// made by the queries, while the logged queries are commits in its log:
	// sheet.db, its -wal file and, with index, its -shm file.
	crashed := func(t *testing.T, index bool, queries []string, logged ...string) string {
		t.Helper()
		from := developed(t, queries...)
		db := rawDB(t, filepath.Join(from, dbName))
		defer db.Close()
		// SQLite writes no page whose bytes an update leaves as they
		// were, so a commit adds a row and removes it again.
		execAll(t, db, append([]string{`PRAGMA wal_autocheckpoint = 0`,
			`INSERT INTO snapshots(hash, data, refs) VALUES('crash', x'00', 0)`, `DELETE FROM snapshots WHERE hash = 'crash'`}, logged...)...)
		dir := t.TempDir()
		names := []string{dbName, dbName + "-wal"}
		if index {
			names = append(names, dbName+"-shm")
		}
		for _, name := range names {
			copyFile(t, filepath.Join(from, name), filepath.Join(dir, name))
		}
		if fi, err := os.Stat(filepath.Join(dir, dbName+"-wal")); err != nil || fi.Size() == 0 {
			t.Fatalf("the copy has no log: %v", err)
		}
		return dir
	}
	unmarked := `PRAGMA application_id = 0`
	withoutIndex := `DROP INDEX sessions_last`
	const noMark, noIndex = "the application_id is not the mark of this release", "the index sessions_last is missing"
	for _, tc := range []struct {
		name, differs string
		dir           func(t *testing.T) string
		// unlinked: the file system takes no link, so the probe copies
		// sheet.db too.
		unlinked bool
	}{
		{name: "without the release mark", differs: noMark,
			dir: func(t *testing.T) string { return developed(t, unmarked) }},
		{name: "without the session index", differs: noIndex,
			dir: func(t *testing.T) string { return developed(t, withoutIndex) }},
		{name: "without the group mark", differs: "the table groups is defined differently",
			dir: func(t *testing.T) string { return developed(t, `ALTER TABLE groups DROP COLUMN dirty`) }},
		{name: "with the group mark a development build added", differs: "the table groups is defined differently",
			dir: func(t *testing.T) string {
				return developed(t, `ALTER TABLE groups DROP COLUMN dirty`, `ALTER TABLE groups ADD COLUMN dirty INTEGER NOT NULL DEFAULT 1`)
			}},
		{name: "restored from a backup without the release mark", differs: noMark,
			dir: func(t *testing.T) string { return restored(t, developed(t, unmarked)) }},
		{name: "restored from a backup without the session index", differs: noIndex,
			dir: func(t *testing.T) string { return restored(t, developed(t, withoutIndex)) }},
		{name: "with a log a crash left", differs: noIndex,
			dir: func(t *testing.T) string { return crashed(t, false, []string{withoutIndex}) }},
		{name: "with a log and its index a crash left", differs: noIndex,
			dir: func(t *testing.T) string { return crashed(t, true, []string{withoutIndex}) }},
		{name: "without the release mark, with a log a crash left", differs: noMark,
			dir: func(t *testing.T) string { return crashed(t, false, []string{unmarked}) }},
		{name: "without the release mark, with a log and its index a crash left", differs: noMark,
			dir: func(t *testing.T) string { return crashed(t, true, []string{unmarked}) }},
		// Only the log tells these from the release, so the probe must read it.
		{name: "without the session index in the log a crash left", differs: noIndex,
			dir: func(t *testing.T) string { return crashed(t, false, nil, withoutIndex) }},
		{name: "without the release mark in the log a crash left", differs: noMark,
			dir: func(t *testing.T) string { return crashed(t, true, nil, unmarked) }},
		{name: "without the release mark in the log a crash left, on a file system without links", differs: noMark,
			dir: func(t *testing.T) string { return crashed(t, true, nil, unmarked) }, unlinked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unlinked {
				linkFile = func(string, string) error { return errors.New("no links here") }
				t.Cleanup(func() { linkFile = os.Link })
			}
			refusedTwice(t, tc.dir(t), release.b.clone(), release.opts, tc.differs)
		})
	}
	// The release database opens, with its content, and so do a backup of
	// it and what a crash leaves of it.
	for _, dir := range []string{restored(t, release.dir), release.dir} {
		release.dir = dir
		release.open()
		if got := release.text("A1"); got != "kept" {
			t.Fatalf("A1 holds %q", got)
		}
		release.in.Stop()
	}
	release.dir = crashed(t, true, nil)
	release.open()
	if got := release.text("A1"); got != "kept" {
		t.Fatalf("A1 holds %q after a crash", got)
	}
	noPrivateCopies(t)
}

// TestEarlyDevelopmentDatabaseIsRefusedBeforeReplay follows a sheet.db
// that an early development build wrote, that a later one upgraded to the
// schema of this release, and whose stored command hashes its request in
// the early form, with the op_id in it. Its schema is this release's, but
// it carries no release mark, so opening and checking it are refused,
// and so are opening and checking a backup of it, before a retry of the
// stored command reaches it. The same database with the mark opens, and
// the retry then gets op_id_reuse instead of its stored answer, which is
// what the refusal keeps from happening. A database this release created
// answers the same retry with its stored answer.
func TestEarlyDevelopmentDatabaseIsRefusedBeforeReplay(t *testing.T) {
	ownTempDir(t)
	release := start(t)
	call := `{"state":"` + release.state() + `","op_id":"once","ops":[{"op":"add_tab","name":"Extra"}]}`
	retry := func(e *env) (WriteResult, error) {
		t.Helper()
		u, err := DecodeUpdate([]byte(call))
		if err != nil {
			t.Fatal(err)
		}
		return e.in.Update(context.Background(), u, e.b.other)
	}
	first, err := retry(release)
	if err != nil || first.Replay {
		t.Fatalf("the call: %+v, %v", first, err)
	}
	release.in.Stop()

	early := t.TempDir()
	copyFile(t, filepath.Join(release.dir, dbName), filepath.Join(early, dbName))
	u, err := DecodeUpdate([]byte(call))
	if err != nil {
		t.Fatal(err)
	}
	old := sha256.Sum256([]byte(call))
	if hex.EncodeToString(old[:]) == u.hash {
		t.Fatal("the early hash of the call is its hash now")
	}
	db := rawDB(t, filepath.Join(early, dbName))
	if res, err := db.Exec(`UPDATE commands SET hash = ? WHERE op_id = 'once'`, hex.EncodeToString(old[:])); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("%d stored commands", n)
	}
	execAll(t, db, `PRAGMA application_id = 0`)
	if differs, err := schemaDiffersOnly(db); err != nil || differs {
		t.Fatalf("the early database has another schema: %v, %v", differs, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	const noMark = "the application_id is not the mark of this release"
	refusedTwice(t, early, release.b.clone(), release.opts, noMark)
	refusedTwice(t, restored(t, early), release.b.clone(), release.opts, noMark)

	marked := restored(t, early)
	db = rawDB(t, filepath.Join(marked, dbName))
	execAll(t, db, `PRAGMA application_id = `+strconv.Itoa(releaseMark))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: marked, b: release.b.clone(), clock: release.clock, opts: release.opts}
	e.open()
	_, err = retry(e)
	e.in.Stop()
	if refusal(err) == nil || refusal(err).Reason != "op_id_reuse" {
		t.Fatalf("the retry against the early hash with the mark: %v, want op_id_reuse", err)
	}

	release.open()
	again, err := retry(release)
	if err != nil || !again.Replay || again.State != first.State {
		t.Fatalf("the retry: %+v, %v; want the stored answer %+v", again, err, first)
	}
}

// schemaDiffersOnly reports whether the tables and indexes of db differ
// from the ones this release creates.
func schemaDiffersOnly(db *sql.DB) (bool, error) {
	want, err := releaseSchema()
	if err != nil {
		return false, err
	}
	have, err := schemaObjects(context.Background(), db)
	if err != nil {
		return false, err
	}
	return !maps.Equal(want, have), nil
}
