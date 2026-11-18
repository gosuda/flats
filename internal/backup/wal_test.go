package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gosuda/flats/internal/sqliteuri"
)

// stoppedHostWithAWalFile returns a data directory as a crashed host leaves it: flats.db and a flats.db-wal
// that holds one commit (the table wal_marker), and no -shm file.
func stoppedHostWithAWalFile(t *testing.T, f *fixture) string {
	t.Helper()
	live := filepath.Join(f.data, nameFlatsDB)
	db, err := sql.Open("sqlite", sqliteuri.File(live)+"?_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range []string{`CREATE TABLE wal_marker(a)`, `INSERT INTO wal_marker VALUES(42)`} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	// The commits are in the -wal file now. Take both files, and no -shm, as a crash leaves them.
	stopped := filepath.Join(f.root, "stopped")
	if err := os.Mkdir(stopped, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{nameFlatsDB, nameFlatsDB + "-wal"} {
		b, err := os.ReadFile(filepath.Join(f.data, n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stopped, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return stopped
}

// A host that stopped without a checkpoint leaves flats.db-wal with commits that flats.db does not hold.
// The copy of that directory holds them (rule 4, rule 11).
func TestRunOfAStoppedHostThatLeftAWalFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := f.options("c1")
	o.DataDir, o.ConfigPath = stoppedHostWithAWalFile(t, f), ""
	res, err := f.run(o)
	if err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, filepath.Join(res.Out, "data", nameFlatsDB+"-wal"))
	copyDB, err := openCopy(filepath.Join(res.Out, "data", nameFlatsDB))
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var got int
	if err := copyDB.QueryRowContext(context.Background(), `SELECT a FROM wal_marker`).Scan(&got); err != nil || got != 42 {
		t.Errorf("the copy holds %d, %v; want the commit of the -wal file", got, err)
	}
	if v, err := Verify(context.Background(), res.Out); err != nil || len(v.Failures) > 0 {
		t.Errorf("Verify = %+v, %v", v, err)
	}
}

// A refusal of rule 12 happens before the command opens a database, so it creates no -shm file beside the
// database of a stopped host (rule 11 allows that file for the snapshot, not for a refusal).
func TestRefusalLeavesTheDataDirectoryOfAStoppedHostAsItWas(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	stopped := stoppedHostWithAWalFile(t, f)
	before := dirNames(t, stopped)
	o := f.options("taken")
	o.DataDir, o.ConfigPath = stopped, ""
	if err := os.Mkdir(o.Out, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := f.run(o)
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if after := dirNames(t, stopped); !slices.Equal(before, after) {
		t.Errorf("the refusal changed the data directory: %v -> %v", before, after)
	}
}

// A running sheet keeps its newest commits in sheet.db-wal, and a new sheet keeps nearly all of its content
// there. The copy takes sheet.db as a snapshot that holds those commits, and none of its -wal and -shm files
// (rule 3, rule 4).
func TestRunTakesTheCommitsOfASheetFromItsWalFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.sheet("h", "sh", 1, 1)
	const rel = "flats/h/sheet/sheet.db"
	live := filepath.Join(f.data, filepath.FromSlash(rel))
	// The connection stays open for the whole backup, as the one of a running host does, and never checkpoints.
	db, err := sql.Open("sqlite", sqliteuri.File(live)+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	pending := hashOf("rows of the pending entry")
	for _, s := range []string{
		sheetEntry(`'` + sheetFiles(pending) + `'`),
		`INSERT INTO snapshots(hash, data, refs) VALUES('` + pending + `', x'726f7773', 1)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if fi, err := os.Stat(live + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("the sheet has no commits in a -wal file: %v, %v", fi, err)
	}
	_, out := f.mustRun("c1")
	m := readCopyManifest(t, out)
	kind := ""
	for _, d := range m.Databases {
		if d.Path == "data/"+rel {
			kind = d.Kind
		}
	}
	if kind != KindSheet {
		t.Errorf("the manifest lists sheet.db with the kind %q, want %q", kind, KindSheet)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if m.has("data/" + rel + suffix) {
			t.Errorf("the manifest lists sheet.db%s as a file of the copy", suffix)
		}
		mustNotExist(t, filepath.Join(out, "data", filepath.FromSlash(rel)+suffix))
		if !slices.Contains(m.Excluded, "data/"+rel+suffix) {
			t.Errorf("the manifest does not list sheet.db%s as excluded: %v", suffix, m.Excluded)
		}
	}
	st, err := readOutbox(context.Background(), filepath.Join(out, "data", filepath.FromSlash(rel)), typeSheet)
	if err != nil || !st.present || st.store != "sh" || st.last != 2 || st.acked != 1 {
		t.Errorf("the outbox of the copy = %+v, %v; want the store sh with entry 2 pending", st, err)
	}
	copyDB, err := openCopy(filepath.Join(out, "data", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var data []byte
	if err := copyDB.QueryRowContext(context.Background(), `SELECT data FROM snapshots WHERE hash = ?`, pending).Scan(&data); err != nil || string(data) != "rows" {
		t.Errorf("the copy holds %q, %v; want the bytes that wait for their import", data, err)
	}
	if v, err := Verify(context.Background(), out); err != nil || len(v.Failures) > 0 {
		t.Errorf("Verify = %+v, %v", v, err)
	}
}
