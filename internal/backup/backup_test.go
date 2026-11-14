package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/ostest"
)

func TestClassify(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rel  string
		want class
		kind string
	}{
		{"flats.db", classDatabase, KindFlats},
		{"flats.db-wal", classExcluded, ""},
		{"flats.db-shm", classExcluded, ""},
		{"logs", classExcluded, ""},
		{"backups/flats.v1.db", classExcluded, ""},
		{"flats.lock", classExcluded, ""},
		{"secret.key", classHost, ""},
		{"tsnet/n.json", classHost, ""},
		{"flats/.lifecycle/rename.a.b.json", classHost, ""},
		{"flats/a/revisions/blobs/ab/abc", classWriteOnce, ""},
		{"flats/a/design/blobs/ab/abc", classWriteOnce, ""},
		{"flats/a/revisions/tmp/x", classExcluded, ""},
		{"flats/a/versions/1/index.html", classWriteOnce, ""},
		{"flats/a/draft-revs/2/x", classWriteOnce, ""},
		{"flats/a/snapshots/x.sqlite", classWriteOnce, ""},
		{"flats/a/data/db.sqlite", classDatabase, KindData},
		{"flats/a/data/db.sqlite-wal", classExcluded, ""},
		{"flats/a/data/files/f.txt", classGuest, ""},
		{"flats/a/board/board.db", classDatabase, KindBoard},
		{"flats/a/board/board.db-shm", classExcluded, ""},
		{"flats/a/notebook/notebook.db", classDatabase, KindNotebook},
		{"flats/a/previews/h.example/db.sqlite", classDatabase, KindPreview},
		{"flats/a/previews/h.example/files/p", classGuest, ""},
		{"flats/a/restore-journal.json", classHost, ""},
		{"flats/a/health-check/x", classExcluded, ""},
		{"flats/a/restore-trial", classExcluded, ""},
		{"flats/a/data.restore", classExcluded, ""},
		{"flats/a/data.restore-old", classExcluded, ""},
		{"flats/a/unknown/x", classHost, ""},
		{"unknown.txt", classHost, ""},
	} {
		got := classify(tc.rel)
		if got.class != tc.want || got.kind != tc.kind {
			t.Errorf("classify(%q) = %v %q, want %v %q", tc.rel, got.class, got.kind, tc.want, tc.kind)
		}
	}
}

// populated builds a host with every kind of flat and every class of file.
func populated(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.board("b", "sb", 5, 5)
	f.notebook("n", "sn", 3, 3)
	f.docs("d", "sd", 2, 2)
	f.server("s")
	h := f.blob("b", "revisions", "asset bytes")
	f.head("b", h)
	f.sql("flats/b/board/board.db", `INSERT INTO assets(hash) VALUES('`+h+`')`)
	f.write("flats/b/versions/1/index.html", "<h1>hello</h1>")
	f.write("flats/b/snapshots/before-v1-1.sqlite", "legacy database bytes")
	f.write("flats/s/snapshots/before-v1-2.sqlite", "db")
	f.write("flats/s/snapshots/before-v1-2.sqlite.data/db.sqlite", "db")
	f.write("flats/s/snapshots/before-v1-2.sqlite.data/"+nameSnapshotMark, "1")
	f.write("flats/s/snapshots/failed-v1-3.sqlite", "partial")
	f.write("flats/s/snapshots/failed-v1-3.sqlite.data/db.sqlite", "partial")
	f.write("flats/s/restore-journal.json", "{}")
	f.sql("flats/s/previews/h1.example/db.sqlite", `CREATE TABLE p(a)`, `INSERT INTO p VALUES(1)`)
	f.write("flats/s/previews/h1.example/files/pf.txt", "preview file")
	f.write("tsnet/node.json", "node")
	f.write("portal/x.json", "portal")
	f.write("config-history/c0.json", "{}")
	f.write("config-history/c1.json", "{}")
	f.write(nameRecovery, "recovery copy")
	f.write(nameRecoveryID, "id")
	for _, p := range []string{"logs/a.log", "cache/x", "run/s.sock", "runtime/docs/m.js", "backups/flats.v1.db", nameLock,
		"flats/s/health-check/x", "flats/s/data.restore/x", "flats/s/data.restore-old/x", "flats/b/revisions/tmp/t", "flats/s/restore-trial/x"} {
		f.write(p, "excluded")
	}
	return f
}

func readCopyManifest(t *testing.T, out string) *Manifest {
	t.Helper()
	m, err := readManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (m *Manifest) has(p string) bool {
	for _, f := range m.Files {
		if f.Path == p {
			return true
		}
	}
	return false
}

func TestRunCopiesEveryClassAndVerifies(t *testing.T) {
	t.Parallel()
	f := populated(t)
	res, out := f.mustRun("c1")
	m := readCopyManifest(t, out)
	for _, p := range []string{
		"data/flats.db", "data/secret.key", "data/secret.key.recovery", "data/secret.key.recovery.id", "data/config.json",
		"data/config-history/c0.json", "data/tsnet/node.json", "data/portal/x.json",
		"data/flats/b/board/board.db", "data/flats/n/notebook/notebook.db", "data/flats/d/data/db.sqlite",
		"data/flats/s/data/db.sqlite", "data/flats/s/data/files/notes.txt",
		"data/flats/s/previews/h1.example/db.sqlite", "data/flats/s/previews/h1.example/files/pf.txt",
		"data/flats/b/versions/1/index.html", "data/flats/b/snapshots/before-v1-1.sqlite",
		"data/flats/s/snapshots/before-v1-2.sqlite", "data/flats/s/snapshots/before-v1-2.sqlite.data/db.sqlite",
		"data/flats/s/snapshots/before-v1-2.sqlite.data/" + nameSnapshotMark, "data/flats/s/restore-journal.json",
	} {
		if !m.has(p) {
			t.Errorf("the copy lacks %s", p)
		}
	}
	for _, p := range []string{
		"data/logs/a.log", "data/cache/x", "data/run/s.sock", "data/runtime/docs/m.js", "data/backups/flats.v1.db", "data/flats.lock",
		"data/flats/s/health-check/x", "data/flats/s/data.restore/x", "data/flats/s/data.restore-old/x", "data/flats/b/revisions/tmp/t",
		"data/flats/s/restore-trial/x", "data/flats/s/snapshots/failed-v1-3.sqlite", "data/flats/s/snapshots/failed-v1-3.sqlite.data/db.sqlite",
	} {
		if m.has(p) || exists(filepath.Join(out, filepath.FromSlash(p))) {
			t.Errorf("the copy holds %s", p)
		}
	}
	for _, p := range []string{"data/logs", "data/cache", "data/run", "data/runtime", "data/backups", "data/flats.lock",
		"data/flats/s/health-check", "data/flats/s/snapshots/failed-v1-3.sqlite", "data/flats/s/snapshots/failed-v1-3.sqlite.data"} {
		if !slices.Contains(m.Excluded, p) {
			t.Errorf("the manifest does not list %s as excluded; it lists %v", p, m.Excluded)
		}
	}
	kinds := map[string]string{}
	for _, d := range m.Databases {
		kinds[d.Path] = d.Kind
	}
	for p, k := range map[string]string{"data/flats.db": KindFlats, "data/flats/b/board/board.db": KindBoard,
		"data/flats/n/notebook/notebook.db": KindNotebook, "data/flats/d/data/db.sqlite": KindData,
		"data/flats/s/previews/h1.example/db.sqlite": KindPreview} {
		if kinds[p] != k {
			t.Errorf("database %s has kind %q, want %q", p, kinds[p], k)
		}
	}
	for _, p := range []string{"data/flats.db-wal", "data/flats.db-shm", "data/flats/b/board/board.db-wal"} {
		mustNotExist(t, filepath.Join(out, filepath.FromSlash(p)))
	}
	if m.SecretKey != SecretKeyIncluded || m.Config != ConfigInData || m.FlatsVersion != "vtest" || m.Format != FormatVersion {
		t.Errorf("manifest header = %+v", m)
	}
	if res.Files != len(m.Files) || res.Attempts != 1 || res.Bytes <= 0 {
		t.Errorf("result = %+v", res)
	}
	if !ostest.Windows {
		for _, p := range []string{"", "data", "data/flats", "data/flats/b/board"} {
			fi, err := os.Stat(filepath.Join(out, filepath.FromSlash(p)))
			if err != nil || fi.Mode().Perm() != 0o700 {
				t.Errorf("directory %q: %v %v, want 0700", p, fi, err)
			}
		}
		for _, p := range []string{"manifest.json", "data/flats.db", "data/secret.key", "data/flats/s/data/files/notes.txt"} {
			fi, err := os.Stat(filepath.Join(out, filepath.FromSlash(p)))
			if err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("file %q: %v %v, want 0600", p, fi, err)
			}
		}
	}
	got, err := Verify(context.Background(), out)
	if err != nil || len(got.Failures) > 0 {
		t.Fatalf("Verify = %+v, %v", got, err)
	}
	f.nothingLeftBut("c1")
}

// nothingLeftBut fails the test when the folder of the copies holds anything but
// the named copies.
func (f *fixture) nothingLeftBut(names ...string) {
	f.t.Helper()
	entries, err := os.ReadDir(f.out)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, e := range entries {
		if !slices.Contains(names, e.Name()) {
			f.t.Errorf("%s is left in the folder of the copies", e.Name())
		}
	}
}

func TestRunRefuses(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	check := func(t *testing.T, name string, o Options) {
		t.Helper()
		before := dirNames(t, f.out)
		_, err := f.run(o)
		var refused *RefusedError
		if !errors.As(err, &refused) {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
		if after := dirNames(t, f.out); !slices.Equal(before, after) {
			t.Errorf("%s: the folder of the copies changed: %v -> %v", name, before, after)
		}
	}
	if err := os.Mkdir(filepath.Join(f.out, "taken"), 0o700); err != nil {
		t.Fatal(err)
	}
	o := f.options("taken")
	check(t, "existing folder", o)
	o = f.options("x")
	o.Out = filepath.Join(f.root, "no-such-folder", "x")
	check(t, "missing folder", o)
	o = f.options("x")
	o.Out = filepath.Join(f.data, "inside")
	check(t, "inside the data directory", o)
	o = f.options("x")
	o.Out = filepath.Join(f.root, "afile")
	if err := os.WriteFile(o.Out, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	check(t, "an existing file", o)
	o = f.options("x")
	o.DataDir = f.root // has no flats.db
	check(t, "no flats.db", o)
	t.Run("a dangling link", func(t *testing.T) { // a symbolic link needs a privilege on Windows, and skips only this case
		link := filepath.Join(f.out, "link")
		if err := ostest.Symlink(t, filepath.Join(f.root, "nowhere"), link); err == nil {
			check(t, "a dangling link", f.options("link"))
		}
	})
}

// A flats.db of another schema version, or one that is no Flats database, is refused before the command
// writes anything: a host of an older release migrates it when it starts.
func TestRunRefusesAFlatsDBOfAnotherRelease(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		prepare func(f *fixture)
		want    string
	}{
		"an older schema": {func(f *fixture) { f.sql(nameFlatsDB, `PRAGMA user_version = 1`) }, "has schema version 1"},
		"a newer schema":  {func(f *fixture) { f.sql(nameFlatsDB, `PRAGMA user_version = 9999`) }, "newer Flats"},
		"not a database":  {func(f *fixture) { f.write(nameFlatsDB, strings.Repeat("not a database ", 20)) }, "not a Flats database"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			tc.prepare(f)
			_, err := f.run(f.options("c1"))
			var refused *RefusedError
			if !errors.As(err, &refused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal with %q", err, tc.want)
			}
			f.nothingLeft()
		})
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestRunExcludesSecretKey(t *testing.T) {
	t.Parallel()
	f := populated(t)
	o := f.options("c1")
	o.ExcludeSecretKey = true
	res, err := f.run(o)
	if err != nil {
		t.Fatal(err)
	}
	m := readCopyManifest(t, res.Out)
	if m.SecretKey != SecretKeyExcluded || m.has("data/secret.key") || !m.has("data/secret.key.recovery") || !m.has("data/secret.key.recovery.id") {
		t.Errorf("secret key = %q, files %v", m.SecretKey, m.Files)
	}
	mustNotExist(t, filepath.Join(res.Out, "data", nameSecretKey))
	got, err := Verify(context.Background(), res.Out)
	if err != nil || len(got.Failures) > 0 || len(got.Notes) != 1 {
		t.Errorf("Verify = %+v, %v; want ok with one note", got, err)
	}
}

func TestRunWithAConfigFileOutsideTheDataDirectory(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(f.root, "etc")
	if err := os.MkdirAll(filepath.Join(dir, nameConfigHist), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, nameConfig)
	for p, c := range map[string]string{cfg: "{\"a\":1}", filepath.Join(dir, nameConfigHist, "h1.json"): "{}"} {
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	os.Remove(filepath.Join(f.data, nameConfig))
	o := f.options("c1")
	o.ConfigPath = cfg
	res, err := f.run(o)
	if err != nil {
		t.Fatal(err)
	}
	m := readCopyManifest(t, res.Out)
	if m.Config != ConfigIncluded || !m.has("config.json") || !m.has("config-history/h1.json") || m.has("data/config.json") {
		t.Errorf("config = %q, files %v", m.Config, m.Files)
	}
	// A host with no configuration file records that.
	o = f.options("c2")
	o.ConfigPath = filepath.Join(f.root, "none.json")
	res, err = f.run(o)
	if err != nil || readCopyManifest(t, res.Out).Config != ConfigAbsent {
		t.Errorf("no config file: %v %+v", err, res)
	}
}

// outboxCase names what a coherence test needs for one content type.
type outboxCase struct {
	typ     string
	add     func(f *fixture, slug string, last, cursor int64)
	rel     string
	table   string
	storeID string
}

var outboxCases = []outboxCase{
	{typeBoard, func(f *fixture, s string, l, c int64) { f.board(s, "st", l, c) }, "flats/x/board/board.db", "outbox", "st"},
	{typeNotebook, func(f *fixture, s string, l, c int64) { f.notebook(s, "st", l, c) }, "flats/x/notebook/notebook.db", "outbox", "st"},
	{typeDocs, func(f *fixture, s string, l, c int64) { f.docs(s, "st", l, c) }, "flats/x/data/db.sqlite", "flats_docs_outbox", "st"},
}

// bump makes the live outbox database reach one more entry, acknowledged, and moves the cursor
// of flats.db to it: an entry that was created, imported and acknowledged.
func (c outboxCase) bump(f *fixture, last int64) {
	f.sql(c.rel, `INSERT INTO `+c.table+`(created_at) VALUES(0)`, `DELETE FROM `+c.table)
	f.setCursor("x", last)
}

// pend adds an entry to the live outbox database that flats.db has not imported.
func (c outboxCase) pend(f *fixture) {
	f.sql(c.rel, `INSERT INTO `+c.table+`(created_at) VALUES(0)`)
}

// The snapshots of an attempt run together in production. A test that must change the host between
// two of them runs them one by one: the outbox databases before flats.db, or after it.
var snapshotOrders = map[string]struct {
	order func(all []dbEntry) []dbEntry // all ends with flats.db
	first func(c outboxCase) string     // the snapshot after which the host changes
}{
	"outbox first": {func(all []dbEntry) []dbEntry { return all }, func(c outboxCase) string { return c.rel }},
	"flats.db first": {func(all []dbEntry) []dbEntry {
		return append([]dbEntry{all[len(all)-1]}, all[:len(all)-1]...)
	}, func(outboxCase) string { return nameFlatsDB }},
}

func TestCoherenceRetriesAndSucceeds(t *testing.T) {
	t.Parallel()
	for name, so := range snapshotOrders {
		for _, tc := range outboxCases {
			t.Run(name+"/"+tc.typ, func(t *testing.T) {
				t.Parallel()
				f := newFixture(t)
				tc.add(f, "x", 4, 4)
				o := f.options("c1")
				o.order = so.order
				bumped := false
				o.afterSnapshot = func(rel string) {
					if rel == so.first(tc) && !bumped { // an entry is created, imported and acknowledged between the two snapshots
						bumped = true
						tc.bump(f, 5)
					}
				}
				res, err := f.run(o)
				if err != nil {
					t.Fatal(err)
				}
				if res.Attempts != 2 || res.Retries != 1 {
					t.Errorf("attempts = %d, retries = %d, want 2 and 1", res.Attempts, res.Retries)
				}
				if got, err := Verify(context.Background(), res.Out); err != nil || len(got.Failures) > 0 {
					t.Errorf("Verify = %+v, %v", got, err)
				}
			})
		}
	}
}

func TestCoherenceFailsAfterThreeAttempts(t *testing.T) {
	t.Parallel()
	for name, so := range snapshotOrders {
		for _, tc := range outboxCases {
			t.Run(name+"/"+tc.typ, func(t *testing.T) {
				t.Parallel()
				f := newFixture(t)
				tc.add(f, "x", 4, 4)
				o := f.options("c1")
				o.order = so.order
				n := int64(4)
				o.afterSnapshot = func(rel string) {
					if rel == so.first(tc) { // the flat is written again between the two snapshots of every attempt
						n++
						tc.bump(f, n)
					}
				}
				_, err := f.run(o)
				var rerun *RerunError
				if !errors.As(err, &rerun) || !strings.Contains(err.Error(), "run the backup again") {
					t.Fatalf("err = %v, want a failure to run again", err)
				}
				f.nothingLeft()
			})
		}
	}
}

// An entry that the host creates or imports in a way that keeps the invariant of rule 16 does not
// make a copy incoherent.
func TestCoherenceAcceptsWhatARunningHostCanProduce(t *testing.T) {
	t.Parallel()
	for _, tc := range outboxCases {
		t.Run("an entry is imported after the snapshot of the database/"+tc.typ, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			tc.add(f, "x", 5, 4) // entry 5 is pending
			o := f.options("c1")
			o.order = snapshotOrders["outbox first"].order
			o.afterSnapshot = func(rel string) {
				if rel == tc.rel {
					f.setCursor("x", 5) // flats.db imports entry 5 before its snapshot: the entry is still in the outbox copy
				}
			}
			res, err := f.run(o)
			if err != nil || res.Attempts != 1 {
				t.Fatalf("Run = %+v, %v; want one attempt", res, err)
			}
		})
		t.Run("an entry is created after the snapshot of flats.db/"+tc.typ, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			tc.add(f, "x", 5, 4)
			o := f.options("c1")
			o.order = snapshotOrders["flats.db first"].order
			o.afterSnapshot = func(rel string) {
				if rel == nameFlatsDB {
					tc.pend(f) // entry 6 is not imported: the outbox copy keeps it, and the cursor stays below it
				}
			}
			res, err := f.run(o)
			if err != nil || res.Attempts != 1 {
				t.Fatalf("Run = %+v, %v; want one attempt", res, err)
			}
		})
	}
}

// incoherentCursors tests the invariant of rule 16 on a copy: the entries through "acked" are gone from
// the outbox, the ones after "last" do not exist, and the cursor must lie between the two.
func TestIncoherentCursors(t *testing.T) {
	t.Parallel()
	for _, tc := range outboxCases {
		t.Run(tc.typ, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			tc.add(f, "x", 5, 3) // entries 4 and 5 are pending: acked 3, last 5
			for seq, want := range map[int64]string{
				2: "acknowledged entries through 3, and the import cursor of flats.db is 2",
				3: "", 4: "", 5: "",
				6: "below the import cursor 6 of flats.db: its outbox reached 5",
			} {
				bad, err := incoherentCursors(context.Background(), f.data, []cursor{{flat: "x", typ: tc.typ, store: tc.storeID, seq: seq}})
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case want == "" && len(bad) != 0:
					t.Errorf("cursor %d: %v, want none", seq, bad)
				case want != "" && (len(bad) != 1 || !strings.Contains(bad[0].reason(), want)):
					t.Errorf("cursor %d: %v, want %q", seq, bad, want)
				}
			}
			bad, err := incoherentCursors(context.Background(), f.data, []cursor{{flat: "x", typ: tc.typ, store: "another store", seq: 99}})
			if err != nil || len(bad) != 0 {
				t.Errorf("another store: %v, %v; want nothing to compare", bad, err)
			}
		})
	}
}

// A group of snapshots starts together, however many members leave before they start.
func TestBarrierOpensWhenAMemberLeavesEarly(t *testing.T) {
	t.Parallel()
	b := newBarrier(3)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, gate := b.member()
			gate()
		}()
	}
	arrive, _ := b.member()
	arrive()
	arrive() // a member counts once
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the members still wait for a member that left")
	}
}

// One damaged database stops the whole group of snapshots and leaves nothing behind.
func TestRunFailsOnADamagedOutboxDatabase(t *testing.T) {
	t.Parallel()
	f := populated(t)
	if err := os.WriteFile(filepath.Join(f.data, "flats", "b", "board", nameBoardDB), []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := f.run(f.options("c1"))
	if err == nil || !strings.Contains(err.Error(), "snapshot flats/b/board/board.db") {
		t.Fatalf("err = %v, want a failure that names the database", err)
	}
	f.nothingLeft()
}

// A host with more outbox databases than one group holds is still copied coherently.
func TestRunWithMoreOutboxDatabasesThanOneGroupHolds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, s := range []string{"a", "b", "c"} {
		f.board(s, "st"+s, 3, 3)
	}
	o := f.options("c1")
	o.maxGroup = 1
	res, err := f.run(o)
	if err != nil || res.Attempts != 1 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if got, err := Verify(context.Background(), res.Out); err != nil || len(got.Failures) > 0 {
		t.Errorf("Verify = %+v, %v", got, err)
	}
}

func TestCoherenceHasNothingToCompare(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.board("noCursor", "sb", 2, 0)
	f.sql(nameFlatsDB, `UPDATE content_state SET import_store = '' WHERE flat = 'noCursor'`)
	f.board("otherStore", "sb2", 2, 9) // the cursor names another store: nothing to compare
	f.sql(nameFlatsDB, `UPDATE content_state SET import_store = 'elsewhere' WHERE flat = 'otherStore'`)
	f.board("noDatabase", "sb3", 2, 9)
	if err := os.Remove(filepath.Join(f.data, "flats", "noDatabase", "board", "board.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(f.options("c1")); err != nil {
		t.Fatal(err)
	}
}

func TestSurveyFailsTheBackupOnAChangeOfLayout(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(f *fixture){
		"a flat is renamed": func(f *fixture) {
			if err := os.Rename(filepath.Join(f.data, "flats", "s"), filepath.Join(f.data, "flats", "s2")); err != nil {
				f.t.Fatal(err)
			}
		},
		"a flat is created": func(f *fixture) { f.write("flats/new/versions/1/index.html", "x") },
		"a flat is deleted": func(f *fixture) {
			if err := os.RemoveAll(filepath.Join(f.data, "flats", "s")); err != nil {
				f.t.Fatal(err)
			}
		},
		"a restore journal appears": func(f *fixture) { f.write("flats/b/restore-journal.json", "{}") },
		"a restore journal changes": func(f *fixture) { f.write("flats/s/restore-journal.json", "{\"changed\":true}") },
		"a lifecycle journal appears": func(f *fixture) {
			f.write("flats/.lifecycle/rename.a.b.json", "{}")
		},
		"the data directory is replaced": func(f *fixture) {
			d := filepath.Join(f.data, "flats", "s", "data")
			if err := os.Rename(d, d+".old"); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Mkdir(d, 0o700); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(d+".old", nameDataDB), filepath.Join(d, nameDataDB)); err != nil {
				f.t.Fatal(err)
			}
			if err := os.RemoveAll(d + ".old"); err != nil {
				f.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := populated(t)
			o := f.options("c1")
			o.afterStep = func(step int) error {
				if step == 14 {
					change(f)
				}
				return nil
			}
			_, err := f.run(o)
			var rerun *RerunError
			if !errors.As(err, &rerun) {
				t.Fatalf("err = %v, want a failure to run again", err)
			}
			f.nothingLeft()
		})
	}
}

func TestSurveyIgnoresTheFirstUseOfADatabase(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.flat("fresh")
	f.write("flats/fresh/versions/1/index.html", "x")
	o := f.options("c1")
	o.afterStep = func(step int) error {
		if step == 14 { // the flat's first request creates data/ and its database
			f.sql("flats/fresh/data/db.sqlite", `CREATE TABLE t(a)`)
		}
		return nil
	}
	if _, err := f.run(o); err != nil {
		t.Fatalf("the first use of a database failed the backup: %v", err)
	}
}

func TestKeyChangeFailsTheBackup(t *testing.T) {
	t.Parallel()
	f := populated(t)
	o := f.options("c1")
	o.afterStep = func(step int) error {
		if step == 17 {
			f.write(nameSecretKey, strings.Repeat("k", 32))
		}
		return nil
	}
	_, err := f.run(o)
	var rerun *RerunError
	if !errors.As(err, &rerun) || !strings.Contains(err.Error(), "secret.key changed") {
		t.Fatalf("err = %v", err)
	}
	f.nothingLeft()
}

func TestEntriesThatAreNotRegularFilesAreSkipped(t *testing.T) {
	t.Parallel()
	f := populated(t)
	outside := filepath.Join(f.root, "outside.txt")
	if err := os.WriteFile(outside, []byte("must not be copied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ostest.Symlink(t, outside, filepath.Join(f.data, "flats", "s", "data", "files", "link.txt")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	makeFIFO(t, filepath.Join(f.data, "flats", "s", "data", "files", "pipe"))
	_, out := f.mustRun("c1")
	m := readCopyManifest(t, out)
	skipped := map[string]string{}
	for _, s := range m.Skipped {
		skipped[s.Path] = s.Reason
	}
	if skipped["data/flats/s/data/files/link.txt"] != "symbolic link" {
		t.Errorf("skipped = %v", skipped)
	}
	if m.has("data/flats/s/data/files/link.txt") {
		t.Error("a link was copied")
	}
	for p := range skipped {
		mustNotExist(t, filepath.Join(out, filepath.FromSlash(p)))
	}
}

func TestSecretKeyThatIsALinkIsCopiedByItsTarget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := filepath.Join(f.root, "elsewhere.key")
	if err := os.WriteFile(target, []byte(strings.Repeat("z", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.data, nameSecretKey)); err != nil {
		t.Fatal(err)
	}
	if err := ostest.Symlink(t, target, filepath.Join(f.data, nameSecretKey)); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	_, out := f.mustRun("c1")
	got, err := os.ReadFile(filepath.Join(out, "data", nameSecretKey))
	if err != nil || string(got) != strings.Repeat("z", 32) {
		t.Errorf("secret.key in the copy = %q, %v", got, err)
	}
	fi, err := os.Lstat(filepath.Join(out, "data", nameSecretKey))
	if err != nil || !fi.Mode().IsRegular() {
		t.Errorf("secret.key in the copy is not a regular file: %v %v", fi, err)
	}
}

func TestVanishedFiles(t *testing.T) {
	t.Parallel()
	t.Run("a preview, a history file and a retirement marker are skipped", func(t *testing.T) {
		t.Parallel()
		f := populated(t)
		f.write("network-retirements/r1.json", "{}")
		f.write("network-retirements/r2.json", "{}")
		o := f.options("c1")
		var dbs int
		o.afterSnapshot = func(rel string) {
			if dbs++; dbs == 1 { // after the first database: the preview expires
				os.RemoveAll(filepath.Join(f.data, "flats", "s", "previews", "h1.example"))
			}
		}
		o.midCopy = func(src string) {
			switch filepath.Base(src) {
			case "c0.json": // the host prunes the oldest history file
				os.Remove(filepath.Join(f.data, nameConfigHist, "c1.json"))
			case "r1.json":
				os.Remove(filepath.Join(f.data, "network-retirements", "r2.json"))
			}
		}
		res, err := f.run(o)
		if err != nil {
			t.Fatal(err)
		}
		skipped := map[string]bool{}
		for _, s := range res.Skipped {
			skipped[s.Path] = true
		}
		for _, p := range []string{"data/flats/s/previews/h1.example/db.sqlite", "data/config-history/c1.json", "data/network-retirements/r2.json"} {
			if !skipped[p] {
				t.Errorf("%s is not listed as skipped: %v", p, res.Skipped)
			}
		}
	})
	t.Run("a database that stays gone fails the backup", func(t *testing.T) {
		t.Parallel()
		f := populated(t)
		f.server("a") // its database is the first of the single snapshots, and s comes after it
		o := f.options("c1")
		o.afterSnapshot = func(rel string) { os.Remove(filepath.Join(f.data, "flats", "s", "data", nameDataDB)) }
		start := time.Now()
		_, err := f.run(o)
		var rerun *RerunError
		if !errors.As(err, &rerun) {
			t.Fatalf("err = %v, want a failure to run again", err)
		}
		if time.Since(start) < 2*vanishPause {
			t.Errorf("the command did not wait to look again")
		}
		f.nothingLeft()
	})
}

func TestUnstableFiles(t *testing.T) {
	t.Parallel()
	// changes makes the file look new on every copy, so it is never stable.
	changes := func(name string) func(src string) {
		n := 0
		return func(src string) {
			if filepath.Base(src) == name {
				n++
				os.Chtimes(src, time.Time{}, time.Unix(1_700_000_000+int64(n), 0))
			}
		}
	}
	t.Run("a guest file is kept and listed", func(t *testing.T) {
		t.Parallel()
		f := populated(t)
		o := f.options("c1")
		o.midCopy = changes("notes.txt")
		res, err := f.run(o)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(res.Unstable, "data/flats/s/data/files/notes.txt") || !readCopyManifest(t, res.Out).has("data/flats/s/data/files/notes.txt") {
			t.Errorf("unstable = %v", res.Unstable)
		}
		if res.Retries != 2 {
			t.Errorf("retries = %d, want 2", res.Retries)
		}
	})
	t.Run("a write-once file is skipped", func(t *testing.T) {
		t.Parallel()
		f := populated(t)
		o := f.options("c1")
		o.midCopy = changes("index.html")
		res, err := f.run(o)
		if err != nil {
			t.Fatal(err)
		}
		m := readCopyManifest(t, res.Out)
		if m.has("data/flats/b/versions/1/index.html") || len(m.Skipped) != 1 {
			t.Errorf("skipped = %v", m.Skipped)
		}
	})
	t.Run("a host file fails the backup", func(t *testing.T) {
		t.Parallel()
		f := populated(t)
		o := f.options("c1")
		o.midCopy = changes("node.json")
		_, err := f.run(o)
		var rerun *RerunError
		if !errors.As(err, &rerun) {
			t.Fatalf("err = %v, want a failure to run again", err)
		}
		f.nothingLeft()
	})
}

func TestAnInterruptAtAnyStepLeavesNothing(t *testing.T) {
	t.Parallel()
	for step := 12; step <= 20; step++ {
		t.Run(strconv.Itoa(step), func(t *testing.T) {
			t.Parallel()
			f := populated(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			o := f.options("c1")
			o.afterStep = func(s int) error {
				if s == step {
					cancel()
				}
				return nil
			}
			_, err := Run(ctx, o)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			f.nothingLeft()
		})
	}
}

func TestAnExistingFolderThatAppearsDuringTheCopyIsNotReplaced(t *testing.T) {
	t.Parallel()
	f := populated(t)
	o := f.options("c1")
	o.afterStep = func(step int) error {
		if step == 20 {
			return os.Mkdir(o.Out, 0o700)
		}
		return nil
	}
	_, err := f.run(o)
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if entries, _ := os.ReadDir(o.Out); len(entries) != 0 {
		t.Errorf("the folder that appeared was changed: %v", entries)
	}
	f.nothingLeftBut("c1")
}

// A rollback-journal database that a writer holds exclusively makes the snapshot wait for the busy
// timeout and fail (rule 4). The writer takes the lock just before the first snapshot starts, so the time
// that Run needs to get there does not matter. The test holds a lock for seconds, so it runs alone: the other tests
// of the package wait for it (the playbook of the timing tests), and its two cases run together.
func TestASnapshotThatIsBusy(t *testing.T) {
	for name, tc := range map[string]struct {
		busy, hold time.Duration
		retries    bool // the writer lets go after the first try failed and before the last one ends
	}{
		// The three tries end at about d+2 s, d+4 s and d+6 s after the lock, where d is the time that Run needs to get
		// from step 14 to the snapshot. The lock goes at 3 s: inside the second try for any d from 0 to 1 s, a margin
		// of 1 s on each side that does not depend on timer precision.
		"a later try succeeds":     {2 * time.Second, 3 * time.Second, true},
		"the third try also fails": {200 * time.Millisecond, 3 * time.Second, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := populated(t)
			path := filepath.Join(f.data, "flats", "s", "data", nameDataDB)
			o := f.options("c1")
			o.busyTimeout = tc.busy
			var done chan struct{}
			o.afterStep = func(step int) error {
				if step == 14 {
					locked := make(chan struct{})
					done = make(chan struct{})
					go func() {
						defer close(done)
						holdExclusive(t, path, locked, tc.hold)
					}()
					<-locked
				}
				return nil
			}
			res, err := f.run(o)
			if done != nil {
				<-done
			}
			if !tc.retries {
				if err == nil || !strings.Contains(err.Error(), "snapshot flats/s/data/db.sqlite") {
					t.Fatalf("err = %v, want a failure that names the database", err)
				}
				f.nothingLeft()
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Retries < 1 {
				t.Errorf("retries = %d, want at least 1", res.Retries)
			}
		})
	}
}

// A write-once file that kept changing in the first pass and holds still in the second is copied and listed in
// files, and is not listed as skipped any more.
func TestAWriteOnceFileSkippedInTheFirstPassAndCopiedInTheSecondIsNotListedAsSkipped(t *testing.T) {
	t.Parallel()
	f := populated(t)
	o := f.options("c1")
	n := 0
	o.midCopy = func(src string) {
		if filepath.Base(src) == "index.html" && n < maxTries { // every try of the first pass sees a change
			n++
			os.Chtimes(src, time.Time{}, time.Unix(1_700_000_000+int64(n), 0))
		}
	}
	res, err := f.run(o)
	if err != nil {
		t.Fatal(err)
	}
	const path = "data/flats/b/versions/1/index.html"
	m := readCopyManifest(t, res.Out)
	if !m.has(path) {
		t.Fatalf("the copy lacks %s", path)
	}
	for _, s := range append(slices.Clone(res.Skipped), m.Skipped...) {
		if s.Path == path {
			t.Errorf("%s is listed as skipped although it was copied: %v", path, s)
		}
	}
}

// A preview expires while the host runs, with its directory. The walk can find the directory in a listing and
// lose it before it reads its details, and the copy then skips it, as it skips what lies below it (rule 8).
func TestAPreviewPathThatIsGoneIsSkippedAtAnyDepth(t *testing.T) {
	t.Parallel()
	f := populated(t)
	c := newCopier(context.Background(), f.data, t.TempDir(), false)
	for _, rel := range []string{"flats/b/previews", "flats/b/previews/gone.example", "flats/b/previews/gone.example/files/x"} {
		if err := c.copyFile(rel, classify(rel).class); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
		if _, ok := c.skipped[manifestPath(rel)]; !ok {
			t.Errorf("%s is not listed as skipped: %v", rel, c.skipped)
		}
	}
	// What the host does not remove on purpose still fails.
	var rerun *RerunError
	if err := c.copyFile("tsnet/missing.json", classHost); !errors.As(err, &rerun) {
		t.Errorf("a host file that is gone: err = %v, want a failure to run again", err)
	}
}
