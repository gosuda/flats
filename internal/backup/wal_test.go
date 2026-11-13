package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/flats/internal/sqliteuri"
)

// A host that stopped without a checkpoint leaves flats.db-wal with commits that
// flats.db does not hold. The copy of that directory holds them (rule 4, rule 11).
func TestRunOfAStoppedHostThatLeftAWalFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
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
	o := f.options("c1")
	o.DataDir, o.ConfigPath = stopped, ""
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
