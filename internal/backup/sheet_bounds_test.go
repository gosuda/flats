package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// allocatedBy returns the bytes fn allocates.
func allocatedBy(fn func()) int64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return int64(after.TotalAlloc - before.TotalAlloc)
}

// The check of a copy reads the file lists and the snapshot hashes of a
// sheet.db, and the outbox's sequence, only when each is of a class and a
// length the writer stores. A file list or a hash of 8 MiB, as text or as
// a blob, a count of references or a sequence that is no number, fails
// the check of that sheet.db, which allocates far less than the value.
func TestVerifyRefusesSheetFieldsPastTheirBounds(t *testing.T) {
	const n = 8 << 20
	for name, statement := range map[string]string{
		"the file list of meta as a blob":        `UPDATE meta SET files = zeroblob(%d)`,
		"the file list of meta as text":          `UPDATE meta SET files = CAST(zeroblob(%d) AS TEXT)`,
		"the file list of an outbox entry":       sheetEntry(`zeroblob(%d)`),
		"the file list of an outbox entry, text": sheetEntry(`CAST(zeroblob(%d) AS TEXT)`),
		"a snapshot hash":                        `INSERT INTO snapshots(hash, data, refs) VALUES(CAST(zeroblob(%d) AS TEXT), x'00', 1)`,
		"a snapshot hash as a blob":              `INSERT INTO snapshots(hash, data, refs) VALUES(zeroblob(%d), x'00', 1)`,
		"a count of references":                  `INSERT INTO snapshots(hash, data, refs) VALUES('` + hashOf("x") + `', x'00', zeroblob(%d))`,
		"the outbox sequence":                    `UPDATE sqlite_sequence SET seq = zeroblob(%d)`,
		"a file list of a number":                `UPDATE meta SET files = 7 -- %d`,
	} {
		t.Run(name, func(t *testing.T) {
			_, out := goodCopy(t)
			baseline := allocatedBy(func() { failuresOf(t, out) })
			(&fixture{t: t, data: filepath.Join(out, "data")}).sql("flats/h/sheet/sheet.db", fmt.Sprintf(statement, n))
			rehash(t, out)
			var fails []Failure
			got := allocatedBy(func() { fails = failuresOf(t, out) })
			wantFailure(t, fails, "data/flats/h/sheet/sheet.db", "")
			t.Logf("refused after %d KiB, %d KiB for the undamaged copy: %v", got>>10, baseline>>10, fails)
			if !raceEnabled && got > baseline+n/16 {
				t.Errorf("the check allocated %d KiB for a field of %d KiB, %d KiB without it", got>>10, n>>10, baseline>>10)
			}
		})
	}
}

// A file list entry the writer would not write, a hash too short to name
// a blob among them, fails the check of the copy rather than the check
// itself.
func TestVerifyRefusesASheetFileOfAnotherSpelling(t *testing.T) {
	_, out := goodCopy(t)
	(&fixture{t: t, data: filepath.Join(out, "data")}).sql("flats/h/sheet/sheet.db",
		`UPDATE meta SET files = '[{"path":"tabs/1/rows/0-0.json","hash":"a","size":1}]'`)
	rehash(t, out)
	got, err := Verify(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	wantFailure(t, got.Failures, "data/flats/h/sheet/sheet.db", "file list")
}

// The check of a copied sheet.db before its quick_check holds for the schema
// of this release only, so a sheet.db of another schema, here with a column
// of numbers more in meta or an outbox whose sequence is a blob, fails the
// check of the copy, and no later check opens it: neither quick_check nor
// the coherence and blob checks, whose reads hold for this schema only. A
// sheet.db that the manifest does not list as a sheet database, or lists as
// a board database or under another spelling of its path, is read by no
// check. The undamaged copy is read by every check.
func TestVerifyRefusesASheetOfAnotherSchema(t *testing.T) {
	const rel = "flats/h/sheet/sheet.db"
	opens := func(t *testing.T, out string) ([]Failure, int) {
		t.Helper()
		target := filepath.Join(out, "data", filepath.FromSlash(rel))
		n := 0
		open := openCopy
		openCopy = func(path string) (*sql.DB, error) {
			if filepath.Clean(path) == target {
				n++
			}
			return open(path)
		}
		defer func() { openCopy = open }()
		return failuresOf(t, out), n
	}
	_, good := goodCopy(t)
	if fails, n := opens(t, good); len(fails) > 0 || n < 4 {
		t.Fatalf("the undamaged copy was opened %d times by the checks: %v", n, fails)
	}
	manifest := func(change func(m *Manifest)) func(t *testing.T, out string) {
		return func(t *testing.T, out string) {
			m, err := readManifest(out)
			if err != nil {
				t.Fatal(err)
			}
			change(m)
			if err := os.Remove(filepath.Join(out, ManifestFile)); err != nil {
				t.Fatal(err)
			}
			if err := m.write(out); err != nil {
				t.Fatal(err)
			}
		}
	}
	listing := func(kind, spelling string) func(t *testing.T, out string) {
		return manifest(func(m *Manifest) {
			for i, d := range m.Databases {
				if d.Path == "data/"+rel {
					m.Databases[i] = Database{Path: spelling, Kind: kind}
				}
			}
		})
	}
	for name, c := range map[string]struct {
		damage   []string
		manifest func(t *testing.T, out string)
		at       string // the path of the failure, if not that of the sheet.db
		reason   string
		want     int
	}{
		"a column more in meta": {damage: []string{`ALTER TABLE meta ADD COLUMN extra INTEGER`}, reason: "sheet check before quick_check", want: 1},
		"an outbox whose sequence is a blob": {damage: []string{`DROP TABLE outbox`, `CREATE TABLE outbox (seq BLOB)`, `INSERT INTO outbox VALUES (zeroblob(1024))`},
			reason: "sheet check before quick_check", want: 1},
		"listed as a board database": {damage: []string{`ALTER TABLE meta ADD COLUMN extra INTEGER`}, manifest: listing(KindBoard, "data/"+rel),
			reason: "makes it a sheet database", want: 0},
		"listed under another spelling": {damage: []string{`ALTER TABLE meta ADD COLUMN extra INTEGER`}, manifest: listing(KindSheet, "data/flats/h/sheet/./sheet.db"),
			at: "data/flats/h/sheet/./sheet.db", reason: "not clean", want: 0},
		"not listed as a database": {manifest: manifest(func(m *Manifest) {
			m.Databases = slices.DeleteFunc(m.Databases, func(d Database) bool { return d.Path == "data/"+rel })
		}), reason: "not listed as a sheet database", want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			_, out := goodCopy(t)
			if len(c.damage) > 0 {
				(&fixture{t: t, data: filepath.Join(out, "data")}).sql(rel, c.damage...)
			}
			rehash(t, out)
			if c.manifest != nil {
				c.manifest(t, out)
			}
			fails, n := opens(t, out)
			if c.at == "" {
				c.at = "data/" + rel
			}
			wantFailure(t, fails, c.at, c.reason)
			if n != c.want {
				t.Errorf("the checks opened the sheet.db %d times, want %d: %v", n, c.want, fails)
			}
		})
	}
	// The check a backup makes of its own copy is the same: a sheet.db of
	// another schema put in place of the copied one before the check is
	// opened once after it.
	t.Run("in the check of a backup", func(t *testing.T) {
		f, out := goodCopy(t)
		damaged := filepath.Join(out, "data", filepath.FromSlash(rel))
		(&fixture{t: t, data: filepath.Join(out, "data")}).sql(rel, `DROP TABLE outbox`, `CREATE TABLE outbox (seq BLOB)`, `INSERT INTO outbox VALUES (zeroblob(1024))`)
		data, err := os.ReadFile(damaged)
		if err != nil {
			t.Fatal(err)
		}
		var target string
		n := 0
		open := openCopy
		openCopy = func(path string) (*sql.DB, error) {
			if target != "" && filepath.Clean(path) == target {
				n++
			}
			return open(path)
		}
		defer func() { openCopy = open }()
		o := Options{DataDir: f.data, ConfigPath: filepath.Join(f.data, nameConfig), Out: filepath.Join(f.out, "self"), FlatsVersion: "vtest"}
		o.afterStep = func(step int) error {
			if step != 19 {
				return nil
			}
			partials, err := filepath.Glob(o.Out + ".partial-*")
			if err != nil || len(partials) != 1 {
				return fmt.Errorf("the copy in progress: %v, %v", partials, err)
			}
			target = filepath.Join(partials[0], "data", filepath.FromSlash(rel))
			return os.WriteFile(target, data, 0o600)
		}
		_, err = Run(context.Background(), o)
		if err == nil || target == "" {
			t.Fatalf("the backup answered %v", err)
		}
		if n != 1 {
			t.Errorf("the check of the backup opened the sheet.db %d times, want 1: %v", n, err)
		}
	})
}

// A live sheet.db that the sheet check refuses, here of another schema, is
// copied by a backup and listed as refused, and the backup succeeds. The
// check runs on the snapshot once in the copy and once in the check of the
// copy, and nothing else opens it, not the coherence test nor quick_check.
// Its check notes it, and a standalone check of the copy does the same. A
// copy that lists as refused a sheet.db that passes the sheet check, or a
// path that is no sheet.db, fails its check.
func TestARefusedSheetIsReadOnlyByItsCheck(t *testing.T) {
	const rel = "flats/h/sheet/sheet.db"
	f := populated(t)
	f.sql(rel, `ALTER TABLE meta ADD COLUMN extra INTEGER`)
	n := 0
	open := openCopy
	openCopy = func(path string) (*sql.DB, error) {
		if strings.HasSuffix(filepath.ToSlash(path), "/data/"+rel) {
			n++
		}
		return open(path)
	}
	defer func() { openCopy = open }()
	res, out := f.mustRun("refused")
	if len(res.Refused) != 1 || res.Refused[0].Path != "data/"+rel || !strings.Contains(res.Refused[0].Reason, "defined differently") {
		t.Fatalf("the backup refused %v", res.Refused)
	}
	if n != 2 {
		t.Errorf("the backup opened the snapshot of the sheet.db %d times, want 2", n)
	}
	n = 0
	got, err := Verify(context.Background(), out)
	if err != nil || len(got.Failures) > 0 || !strings.Contains(strings.Join(got.Notes, " "), "data/"+rel+" is copied as it is") {
		t.Fatalf("the check of the copy: %+v, %v", got, err)
	}
	if n != 1 {
		t.Errorf("the check of the copy opened the sheet.db %d times, want 1", n)
	}
	openCopy = open

	_, good := goodCopy(t)
	for name, c := range map[string]struct{ path, want string }{
		"a sheet.db that passes":     {"data/" + rel, "passes the check"},
		"a path that is no sheet.db": {"data/flats/b/board/board.db", "is listed as a refused sheet database, and it is none"},
	} {
		t.Run(name, func(t *testing.T) {
			m, err := readManifest(good)
			if err != nil {
				t.Fatal(err)
			}
			m.Refused = []Skip{{Path: c.path, Reason: "a test"}}
			if err := os.Remove(filepath.Join(good, ManifestFile)); err != nil {
				t.Fatal(err)
			}
			if err := m.write(good); err != nil {
				t.Fatal(err)
			}
			wantFailure(t, failuresOf(t, good), c.path, c.want)
		})
	}
}
