package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixtureNow is the fixtures' era; their redirects run until 2100.
var fixtureNow = time.UnixMilli(1700000000000).UTC()

// loadFixture builds a database from testdata/schema/<name>.sql the way the
// historical Open left it: WAL mode, then the commit's DDL and sample rows.
func loadFixture(t *testing.T, name string) string {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join("testdata", "schema", name+".sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "flats.db")
	db, err := sql.Open("sqlite", fileURI(path)+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(name, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// withMigrations swaps the registry for one test.
func withMigrations(t *testing.T, ms []migration) {
	t.Helper()
	saved := migrations
	migrations = ms
	t.Cleanup(func() { migrations = saved })
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// snapshot is everything Inspect or a rejected Open must leave unchanged.
func snapshot(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s %v %v", fileHash(t, path), fi.ModTime(), dirNames(t, filepath.Dir(path)))
}

// queryRO reads one value through a read-only connection.
func queryRO(t *testing.T, path, q string, dst any) {
	t.Helper()
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(q).Scan(dst); err != nil {
		t.Fatal(q, err)
	}
}

// schemaShape lists every table's columns and every named index, so a
// migrated database can be compared with a newly created one.
func schemaShape(t *testing.T, path string) []string {
	t.Helper()
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	tables, err := tableNames(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, table := range tables {
		rows, err := db.Query(`SELECT name, type, "notnull", COALESCE(dflt_value,''), pk FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name, typ, dflt string
			var notnull, pk int
			if err := rows.Scan(&name, &typ, &notnull, &dflt, &pk); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s.%s %s notnull=%d default=%s pk=%d", table, name, typ, notnull, dflt, pk))
		}
		rows.Close()
	}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND sql IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, "index "+name)
	}
	slices.Sort(out)
	return out
}

func backupFiles(t *testing.T, path string) []string {
	t.Helper()
	es, err := os.ReadDir(filepath.Join(filepath.Dir(path), "backups"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if backupName.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestMigrationRegistryIsOrdered(t *testing.T) {
	if migrations[0].version != 5 || migrations[0].name != "legacy baseline 5" {
		t.Fatalf("first step %d %q", migrations[0].version, migrations[0].name)
	}
	for i, m := range migrations {
		if m.name == "" || m.up == nil {
			t.Fatalf("step %d incomplete", m.version)
		}
		if i > 0 && m.version <= migrations[i-1].version {
			t.Fatalf("step %d after %d", m.version, migrations[i-1].version)
		}
	}
	if latestVersion() != 6 {
		t.Fatalf("latest %d", latestVersion())
	}
}

// legacyChecks covers rows written before the lifecycle split (v0 and v1).
func legacyChecks(t *testing.T, s *Store) {
	ctx := context.Background()
	if r, err := s.RedirectFor(ctx, "oldblog", fixtureNow); err != nil || r.Flat != "blog" {
		t.Fatalf("rename redirect %+v %v", r, err)
	}
	if _, err := s.RedirectFor(ctx, "blog", fixtureNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("redirect over a live slug: %v", err)
	}
	for slug, want := range map[string][]int{"blog": {2, 1}, "docs": {1}, "notes": nil} {
		vs, err := s.ListVersions(ctx, slug)
		if err != nil {
			t.Fatal(err)
		}
		var got []int
		for _, v := range vs {
			got = append(got, v.Number)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s published %v, want %v", slug, got, want)
		}
	}
	un, err := s.UnpublishedVersions(ctx)
	if err != nil || len(un) != 2 || un[0].Flat != "blog" || un[0].Number != 3 || un[1].Flat != "notes" {
		t.Fatalf("unpublished %+v %v", un, err)
	}
	if v, _ := s.GetVersion(ctx, "blog", 2); v.Hash != "h-blog-2" || !v.GitDirty || v.Screenshot != "shot.png" {
		t.Fatalf("version row %+v", v)
	}
	for slug, want := range map[string]Visibility{"blog": Public, "notes": Private, "docs": Private, "shop": Private} {
		if f, err := s.GetFlat(ctx, slug); err != nil || f.Visibility != want {
			t.Fatalf("%s visibility %q %v", slug, f.Visibility, err)
		}
	}
	a, err := s.GetApproval(ctx, "a-vis")
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]string
	if err := json.Unmarshal(a.Params, &p); err != nil || p["visibility"] != "public" || p["from"] != "private" || a.Status != "pending" {
		t.Fatalf("pending visibility request %s %s", a.Params, a.Status)
	}
	if old, _ := s.GetApproval(ctx, "a-old"); !strings.Contains(string(old.Params), "public-unlisted") {
		t.Fatalf("decided approval rewritten: %s", old.Params)
	}
	if pv, err := s.GetPreview(ctx, "p-blog-3"); err != nil || pv.Target != "version" || pv.Version != 3 {
		t.Fatalf("preview %+v %v", pv, err)
	}
	if pending, err := s.LegacyPrivateUpgradePending(ctx); err != nil || !pending {
		t.Fatalf("legacy eligibility %t %v", pending, err)
	}
	var eligible int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM legacy_private_upgrade WHERE pending=1`).Scan(&eligible); err != nil || eligible != 4 {
		t.Fatalf("legacy_private_upgrade rows %d %v", eligible, err)
	}
	if ps, _ := s.PermittedProviders(ctx, "blog"); !slices.Equal(ps, []string{ProviderLocal}) {
		t.Fatalf("migration invented grants %v", ps)
	}
	if ds, _ := s.ListDeployments(ctx, "blog", 10); len(ds) != 2 || ds[0].Version != 2 || ds[0].ApprovalID != "" {
		t.Fatalf("deployments %+v", ds)
	}
	commonChecks(t, s)
}

// commonChecks covers rows every fixture has.
func commonChecks(t *testing.T, s *Store) {
	ctx := context.Background()
	if secs, _ := s.ListSecrets(ctx, "blog"); len(secs) != 1 || secs[0].Name != "API_KEY" || string(secs[0].Ciphertext) != "\xa1\xb2\xc3" {
		t.Fatalf("secrets %+v", secs)
	}
	if v, _ := s.GetSetting(ctx, "portal.relays", ""); v != "https://relay.example.com" {
		t.Fatalf("setting %q", v)
	}
	if pv, _ := s.PageViewsSince(ctx, "blog", "2023-11-01"); len(pv) != 1 || pv[0].Count != 42 {
		t.Fatalf("page views %+v", pv)
	}
	if evs, _ := s.ListEvents(ctx, "blog", "", 0, 10); len(evs) == 0 {
		t.Fatal("events lost")
	}
}

func TestHistoricalFixturesMigrateToLatest(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	s, err := Open(fresh)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	want := schemaShape(t, fresh)

	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		version int
		check   func(*testing.T, *Store)
	}{
		{"v0-e02e9d8", 0, legacyChecks},
		{"v1-84a4535", 1, func(t *testing.T, s *Store) {
			legacyChecks(t, s)
			if r, err := s.RedirectFor(ctx, "ancient", fixtureNow); err != nil || r.Flat != "docs" {
				t.Fatalf("existing redirect %+v %v", r, err)
			}
		}},
		{"v1-0dfc1be", 1, func(t *testing.T, s *Store) {
			legacyChecks(t, s)
			if r, err := s.RedirectFor(ctx, "ancient", fixtureNow); err != nil || r.Flat != "docs" {
				t.Fatalf("existing redirect %+v %v", r, err)
			}
			if top, _ := s.TopPages(ctx, "blog", "2023-11-01"); len(top) != 2 || top[0].Path != "/" || top[0].Count != 30 {
				t.Fatalf("page paths %+v", top)
			}
		}},
		{"v5-80b206b", 5, func(t *testing.T, s *Store) {
			if vs, _ := s.ListVersions(ctx, "blog"); len(vs) != 2 || vs[0].Number != 2 {
				t.Fatalf("versions %+v", vs)
			}
			if d, err := s.GetDraft(ctx, "blog"); err != nil || d.Revision != 3 || d.BaseVersion != 2 {
				t.Fatalf("draft %+v %v", d, err)
			}
			if r, err := s.GetDraftRevision(ctx, "blog", 3); err != nil || r.LegacyNumber != 3 {
				t.Fatalf("draft revision %+v %v", r, err)
			}
			if a, err := s.FindOpenApprovalByKey(ctx, "k-open"); err != nil || a.ID != "a-open" {
				t.Fatalf("open approval %+v %v", a, err)
			}
			if a, _ := s.GetApproval(ctx, "a-pub"); a.DecidedBy != "operator" || a.AuthorizedAt == nil || string(a.ResultData) != `{"version":2}` {
				t.Fatalf("approval audit %+v", a)
			}
			if d, ok, err := s.DeploymentByApproval(ctx, "a-pub"); err != nil || !ok || d.Version != 2 {
				t.Fatalf("deployment by approval %+v %t %v", d, ok, err)
			}
			if pv, _ := s.GetPreview(ctx, "p-blog-draft"); pv.Target != "draft" || pv.Revision != 3 {
				t.Fatalf("preview %+v", pv)
			}
			ts, _ := s.ProviderPermitted(ctx, "blog", ProviderTailscale)
			portal, _ := s.ProviderPermitted(ctx, "blog", ProviderPortal)
			if !ts || portal {
				t.Fatalf("provider rows tailscale=%t portal=%t", ts, portal)
			}
			if pending, _ := s.LegacyPrivateUpgradePending(ctx); !pending {
				t.Fatal("legacy eligibility lost")
			}
			if r, err := s.RedirectFor(ctx, "oldblog", fixtureNow); err != nil || r.Flat != "docs" {
				t.Fatalf("redirect %+v %v", r, err)
			}
			if top, _ := s.TopPages(ctx, "blog", "2023-11-01"); len(top) != 1 {
				t.Fatalf("page paths %+v", top)
			}
			commonChecks(t, s)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := loadFixture(t, tc.name)
			before, err := Inspect(path)
			if err != nil || !before.Exists || before.New || !before.IsFlats || before.Version != tc.version || before.Binding != nil {
				t.Fatalf("inspect fixture %+v %v", before, err)
			}
			var flats int
			queryRO(t, path, `SELECT COUNT(*) FROM flats`, &flats)

			for attempt := range 2 {
				s, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				tc.check(t, s)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				// The second Open finds nothing to migrate and backs up nothing.
				if bs := backupFiles(t, path); len(bs) != 1 || !strings.HasPrefix(bs[0], fmt.Sprintf("flats.v%d.", tc.version)) {
					t.Fatalf("attempt %d backups %v", attempt, bs)
				}
			}
			after, err := Inspect(path)
			if err != nil || after.Version != latestVersion() {
				t.Fatalf("migrated %+v %v", after, err)
			}
			if got := schemaShape(t, path); !slices.Equal(got, want) {
				t.Fatalf("migrated schema differs from a new database:\n got %q\nwant %q", got, want)
			}

			// The backup is the pre-migration database, readable and private.
			bk := filepath.Join(filepath.Dir(path), "backups", backupFiles(t, path)[0])
			info, err := Inspect(bk)
			if err != nil || info.Version != tc.version {
				t.Fatalf("backup %+v %v", info, err)
			}
			var backed int
			queryRO(t, bk, `SELECT COUNT(*) FROM flats`, &backed)
			if backed != flats {
				t.Fatalf("backup has %d flats, want %d", backed, flats)
			}
			if fi, _ := os.Stat(bk); fi.Mode().Perm() != 0o600 {
				t.Fatalf("backup mode %v", fi.Mode())
			}
		})
	}
}

func TestNewDatabaseReachesLatestWithoutBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flats.db")
	info, err := Inspect(path)
	if err != nil || info.Exists || !info.New || !info.IsFlats || info.Version != 0 || info.Latest != latestVersion() {
		t.Fatalf("missing file %+v %v", info, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Inspect created the database file")
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	info, err = Inspect(path)
	if err != nil || !info.Exists || info.New || info.Version != latestVersion() || info.Binding != nil {
		t.Fatalf("new database %+v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "backups")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new database was backed up")
	}

	// A zero-length file, as left by a crashed create, is also new.
	empty := filepath.Join(t.TempDir(), "flats.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := Inspect(empty); err != nil || info.Exists || !info.New {
		t.Fatalf("empty file %+v %v", info, err)
	}
	s, err = Open(empty)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if bs := backupFiles(t, empty); bs != nil {
		t.Fatalf("empty file backed up %v", bs)
	}
}

func TestConcurrentOpenOfNewDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flats.db")
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() {
			s, err := Open(path)
			if err == nil {
				err = s.Close()
			}
			errs[i] = err
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if info, err := Inspect(path); err != nil || info.Version != latestVersion() {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestOpenRejectsNewerSchemaWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flats.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := sql.Open("sqlite", fileURI(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if names := dirNames(t, filepath.Dir(path)); !slices.Equal(names, []string{"flats.db"}) {
		t.Fatalf("setup left %v", names)
	}
	before := snapshot(t, path)
	info, err := Inspect(path)
	if !errors.Is(err, ErrSchemaTooNew) || info.Version != 99 || !info.IsFlats {
		t.Fatalf("inspect %+v %v", info, err)
	}
	if _, err := Open(path); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("open newer schema: %v", err)
	}
	if after := snapshot(t, path); after != before {
		t.Fatalf("rejected database changed:\n%s\n%s", before, after)
	}
}

func TestOpenRejectsForeignFilesWithoutWriting(t *testing.T) {
	for name, setup := range map[string]func(path string) error{
		"other sqlite": func(path string) error {
			db, err := sql.Open("sqlite", fileURI(path))
			if err != nil {
				return err
			}
			defer db.Close()
			_, err = db.Exec(`CREATE TABLE notes(body TEXT); INSERT INTO notes VALUES('hi')`)
			return err
		},
		"tableless with version": func(path string) error {
			db, err := sql.Open("sqlite", fileURI(path))
			if err != nil {
				return err
			}
			defer db.Close()
			_, err = db.Exec(`PRAGMA user_version = 3`)
			return err
		},
		"text file": func(path string) error {
			return os.WriteFile(path, []byte("not a database at all, just some notes\n"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "flats.db")
			if err := setup(path); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, path)
			if info, err := Inspect(path); !errors.Is(err, ErrNotFlats) || info.IsFlats {
				t.Fatalf("inspect %+v %v", info, err)
			}
			if _, err := Open(path); !errors.Is(err, ErrNotFlats) {
				t.Fatalf("open foreign: %v", err)
			}
			if after := snapshot(t, path); after != before {
				t.Fatalf("rejected file changed:\n%s\n%s", before, after)
			}
		})
	}
}

func TestFailedStepRollsBack(t *testing.T) {
	boom := errors.New("injected failure")

	t.Run("new step", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "flats.db")
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := s.CreateFlat(context.Background(), Flat{Slug: "blog", Name: "Blog", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		s.Close()
		withMigrations(t, append(slices.Clone(migrations), migration{7, "broken", func(ctx context.Context, x executor) error {
			if _, err := x.ExecContext(ctx, `CREATE TABLE step7(x)`); err != nil {
				return err
			}
			if _, err := x.ExecContext(ctx, `UPDATE flats SET name='changed'`); err != nil {
				return err
			}
			return boom
		}}))
		if _, err := Open(path); !errors.Is(err, boom) {
			t.Fatalf("open with failing step: %v", err)
		}
		if info, err := Inspect(path); err != nil || info.Version != 6 {
			t.Fatalf("version after failed step %+v %v", info, err)
		}
		var name string
		var step7 int
		queryRO(t, path, `SELECT name FROM flats WHERE slug='blog'`, &name)
		queryRO(t, path, `SELECT COUNT(*) FROM sqlite_master WHERE name='step7'`, &step7)
		if name != "Blog" || step7 != 0 {
			t.Fatalf("failed step leaked: name=%q step7=%d", name, step7)
		}
	})

	t.Run("legacy baseline", func(t *testing.T) {
		path := loadFixture(t, "v0-e02e9d8")
		withMigrations(t, []migration{
			{5, "legacy baseline 5", func(ctx context.Context, x executor) error {
				if err := legacyBaseline(ctx, x); err != nil {
					return err
				}
				return boom
			}},
			{6, "host binding", addHostBinding},
		})
		if _, err := Open(path); !errors.Is(err, boom) {
			t.Fatalf("open with failing baseline: %v", err)
		}
		if info, err := Inspect(path); err != nil || info.Version != 0 {
			t.Fatalf("version after failed baseline %+v %v", info, err)
		}
		var redirects, published, drafts int
		var vis string
		queryRO(t, path, `SELECT COUNT(*) FROM sqlite_master WHERE name='redirects'`, &redirects)
		queryRO(t, path, `SELECT COUNT(*) FROM pragma_table_info('versions') WHERE name='published'`, &published)
		queryRO(t, path, `SELECT COUNT(*) FROM sqlite_master WHERE name='drafts'`, &drafts)
		queryRO(t, path, `SELECT visibility FROM flats WHERE slug='blog'`, &vis)
		if redirects != 0 || published != 0 || drafts != 0 || vis != "public-listed" {
			t.Fatalf("baseline leaked: redirects=%d published=%d drafts=%d visibility=%q", redirects, published, drafts, vis)
		}
	})

	t.Run("later step keeps earlier commits", func(t *testing.T) {
		path := loadFixture(t, "v0-e02e9d8")
		withMigrations(t, []migration{
			{5, "legacy baseline 5", legacyBaseline},
			{6, "host binding", func(ctx context.Context, x executor) error {
				if err := addHostBinding(ctx, x); err != nil {
					return err
				}
				return boom
			}},
		})
		if _, err := Open(path); !errors.Is(err, boom) {
			t.Fatalf("open with failing step 6: %v", err)
		}
		info, err := Inspect(path)
		if err != nil || info.Version != 5 {
			t.Fatalf("version %+v %v", info, err)
		}
		var binding int
		queryRO(t, path, `SELECT COUNT(*) FROM sqlite_master WHERE name='host_binding'`, &binding)
		if binding != 0 {
			t.Fatal("rolled back step left host_binding")
		}
	})
}

func TestBackupsArePrunedToNewestThree(t *testing.T) {
	path := loadFixture(t, "v5-80b206b")
	dir := filepath.Join(filepath.Dir(path), "backups")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := []string{
		"flats.v1.20200101T000000.000000000Z.db",
		"flats.v5.20240101T000000.000000000Z.db",
		"flats.v2.20230101T000000.000000000Z.db",
		"flats.v5.20250101T000000.000000000Z.db",
	}
	keep := []string{"notes.txt", "flats.v5.manual.db"}
	for _, name := range append(slices.Clone(old), keep...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	got := backupFiles(t, path)
	if len(got) != 3 || !slices.Contains(got, old[3]) || !slices.Contains(got, old[1]) {
		t.Fatalf("kept %v", got)
	}
	names := dirNames(t, dir)
	for _, name := range keep {
		if !slices.Contains(names, name) {
			t.Fatalf("pruned unrelated %s: %v", name, names)
		}
	}
}

func TestBackupFailureAbortsMigration(t *testing.T) {
	for name, block := range map[string]func(dir string) error{
		"backups is a file": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "backups"), []byte("x"), 0o600)
		},
		"backups not writable": func(dir string) error {
			return os.Mkdir(filepath.Join(dir, "backups"), 0o500)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if os.Geteuid() == 0 && strings.Contains(name, "writable") {
				t.Skip("root ignores directory permissions")
			}
			path := loadFixture(t, "v5-80b206b")
			if err := block(filepath.Dir(path)); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, path)
			if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "backup") {
				t.Fatalf("open without backup: %v", err)
			}
			if after := snapshot(t, path); after != before {
				t.Fatalf("database changed without backup:\n%s\n%s", before, after)
			}
			if info, err := Inspect(path); err != nil || info.Version != 5 {
				t.Fatalf("%+v %v", info, err)
			}
		})
	}
}

func TestHostBinding(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flats.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok, err := s.HostBinding(ctx); ok || err != nil {
		t.Fatalf("new database bound: %t %v", ok, err)
	}
	if err := s.BindHost(ctx, HostBinding{InstanceID: "inst-a"}); err == nil {
		t.Fatal("incomplete binding accepted")
	}
	bound := time.Date(2026, 10, 4, 12, 0, 0, 500, time.UTC)
	migrated := bound.Add(-time.Minute)
	want := HostBinding{InstanceID: "inst-a", ConfigPath: "/etc/flats/config.json", BoundAt: bound, MigratedAt: &migrated}
	if err := s.BindHost(ctx, want); err != nil {
		t.Fatal(err)
	}
	check := func(got HostBinding) {
		t.Helper()
		if got.InstanceID != "inst-a" || got.ConfigPath != want.ConfigPath || !got.BoundAt.Equal(bound.Truncate(time.Second)) ||
			got.MigratedAt == nil || !got.MigratedAt.Equal(migrated.Truncate(time.Second)) {
			t.Fatalf("binding %+v", got)
		}
	}
	got, ok, err := s.HostBinding(ctx)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	check(got)

	// The same instance again is a no-op: the first record stays.
	if err := s.BindHost(ctx, HostBinding{InstanceID: "inst-a", ConfigPath: "/elsewhere.json", BoundAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.HostBinding(ctx)
	check(got)

	if err := s.BindHost(ctx, HostBinding{InstanceID: "inst-b", ConfigPath: "/x.json", BoundAt: time.Now()}); !errors.Is(err, ErrHostBindingMismatch) {
		t.Fatalf("other instance: %v", err)
	}
	got, _, _ = s.HostBinding(ctx)
	check(got)

	// Inspect reads the binding while the store is open and the row is
	// still only in the WAL, without changing any file.
	before := snapshot(t, path)
	info, err := Inspect(path)
	if err != nil || info.Binding == nil {
		t.Fatalf("inspect %+v %v", info, err)
	}
	check(*info.Binding)
	if after := snapshot(t, path); after != before {
		t.Fatalf("Inspect changed files:\n%s\n%s", before, after)
	}
	s.Close()
	info, err = Inspect(path)
	if err != nil || info.Binding == nil {
		t.Fatalf("inspect closed %+v %v", info, err)
	}
	check(*info.Binding)
}

func TestInspectDoesNotWrite(t *testing.T) {
	for _, name := range []string{"v0-e02e9d8", "v5-80b206b"} {
		path := loadFixture(t, name)
		before := snapshot(t, path)
		for range 2 {
			if _, err := Inspect(path); err != nil {
				t.Fatal(err)
			}
		}
		if after := snapshot(t, path); after != before {
			t.Fatalf("%s: Inspect changed files:\n%s\n%s", name, before, after)
		}
	}

	path := filepath.Join(t.TempDir(), "flats.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindHost(context.Background(), HostBinding{InstanceID: "i", ConfigPath: "/c", BoundAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before := snapshot(t, path)
	if info, err := Inspect(path); err != nil || info.Binding == nil || info.Binding.InstanceID != "i" {
		t.Fatalf("%+v %v", info, err)
	}
	if after := snapshot(t, path); after != before {
		t.Fatalf("Inspect changed files:\n%s\n%s", before, after)
	}
}

// TestMigrationCrashHelper is the child process of
// TestKilledMigrationOfRestoredBackupStillOpens: it dies inside a step.
func TestMigrationCrashHelper(t *testing.T) {
	path := os.Getenv("FLATS_STORE_CRASH_DB")
	if path == "" {
		t.Skip("helper process only")
	}
	migrations = append(slices.Clone(migrations), migration{latestVersion() + 1, "crash", func(ctx context.Context, x executor) error {
		if _, err := x.ExecContext(ctx, `UPDATE flats SET name='half'`); err != nil {
			return err
		}
		os.Exit(3)
		return nil
	}})
	_, err := Open(path)
	t.Fatalf("migration returned: %v", err)
}

// A database restored from a VACUUM INTO backup is in rollback-journal
// mode. A process killed in the middle of migrating it must not leave a hot
// journal that the read-only probe cannot get past.
func TestKilledMigrationOfRestoredBackupStillOpens(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.db")
	s, err := Open(live)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.CreateFlat(context.Background(), Flat{Slug: "blog", Name: "Blog", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`VACUUM INTO ?`, filepath.Join(dir, "flats.db")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	path := filepath.Join(dir, "flats.db")
	var mode string
	queryRO(t, path, `PRAGMA journal_mode`, &mode)
	if mode != "delete" {
		t.Fatalf("restored backup journal mode %q, want delete", mode)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMigrationCrashHelper$")
	cmd.Env = append(os.Environ(), "FLATS_STORE_CRASH_DB="+path)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("helper did not die in the step: %v\n%s", err, out)
	}
	if sidecarSize(path+"-journal") > 0 {
		t.Fatal("killed migration left a hot rollback journal")
	}
	info, err := Inspect(path)
	if err != nil || info.Version != latestVersion() {
		t.Fatalf("inspect after killed migration: %+v %v", info, err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("open after killed migration: %v", err)
	}
	defer s.Close()
	f, err := s.GetFlat(context.Background(), "blog")
	if err != nil || f.Name != "Blog" {
		t.Fatalf("killed step leaked: %+v %v", f, err)
	}
}
