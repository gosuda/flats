package backup

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// rewriteManifest changes the manifest of the copy in out.
func rewriteManifest(t *testing.T, out string, change func(m *Manifest)) {
	t.Helper()
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

// A file entry whose path leaves the copy, here through ".." to a file of
// the same bytes beside the copy, or that a copy never writes, fails the
// check of the copy: the check looks for it under no spelling of its own.
func TestAFileEntryOfAnotherSpellingFailsTheCheck(t *testing.T) {
	for name, spelling := range map[string]string{
		"a .. that leaves the copy": "data/../../escape.txt",
		"a leading slash":           "/escape.txt",
		"a backslash":               `data\escape.txt`,
		"a . element":               "data/./flats.db",
		"a volume":                  "C:/escape.txt",
	} {
		t.Run(name, func(t *testing.T) {
			_, out := goodCopy(t)
			if err := os.WriteFile(filepath.Join(filepath.Dir(out), "escape.txt"), []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			rewriteManifest(t, out, func(m *Manifest) {
				m.Files = append(m.Files, File{Path: spelling, Size: 7, SHA256: hashOf("outside")})
			})
			wantFailure(t, failuresOf(t, out), spelling, "is not a path a copy holds")
		})
	}
}

// Every path of the manifest of a backup of a host with every kind of flat
// is canonical and lies in the layout of a copy, and each database is listed
// with the kind that the layout gives its path.
func TestACopyListsOnlyCanonicalPaths(t *testing.T) {
	_, out := goodCopy(t)
	m := readCopyManifest(t, out)
	for _, f := range m.Files {
		if err := checkManifestPath(f.Path); err != nil {
			t.Errorf("file %s: %v", f.Path, err)
		}
	}
	kinds := map[string]bool{}
	for _, d := range m.Databases {
		kinds[d.Kind] = true
		if err := checkDatabasePath(d); err != nil {
			t.Errorf("database %s: %v", d.Path, err)
		}
	}
	for _, k := range []string{KindFlats, KindData, KindPreview, KindBoard, KindNotebook, KindSheet} {
		if !kinds[k] {
			t.Errorf("the copy holds no %s database", k)
		}
	}
}

// The check of a manifest path does not depend on the system: a spelling
// that Windows reads as a path of the copy is refused on every system.
func TestManifestPathsAreCheckedTheSameEverywhere(t *testing.T) {
	for _, p := range []string{"data/flats.db", "data/flats/h/sheet/sheet.db", "config.json", "config-history/c0.json", "data/flats/s/data/files/a:b.txt"} {
		if err := checkManifestPath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for p, want := range map[string]string{
		"":                              "empty",
		`data\flats\h\sheet\sheet.db`:   "backslash",
		`data/flats\h/sheet/sheet.db`:   "backslash",
		`\\server\share\sheet.db`:       "backslash",
		"//server/share/sheet.db":       "slash",
		"/data/flats.db":                "slash",
		"C:/data/flats.db":              "volume",
		"c:data/flats.db":               "volume",
		"data/../flats.db":              "..",
		"../data/flats.db":              "..",
		"data/flats/h/sheet/./sheet.db": "not clean",
		"data//flats.db":                "not clean",
		"data/flats.db/":                "not clean",
		".":                             "not clean",
		"data":                          "layout",
		"other/flats.db":                "layout",
		"config-history/a/b.json":       "layout",
		"data/\xff.txt":                 "UTF-8",
	} {
		err := checkManifestPath(p)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want an error with %q", p, err, want)
		}
	}
	for d, want := range map[Database]string{
		{Path: "data/flats/h/sheet/sheet.db", Kind: KindBoard}:  "makes it a sheet database",
		{Path: "data/flats/h/sheet/Sheet.db", Kind: KindSheet}:  "holds no database",
		{Path: "data/flats/h/sheet/sheet.db.", Kind: KindSheet}: "holds no database",
		{Path: "config.json", Kind: KindFlats}:                  "holds no database",
		{Path: "data/flats/b/board/board.db", Kind: KindSheet}:  "makes it a board database",
		{Path: `data\flats.db`, Kind: KindFlats}:                "backslash",
	} {
		err := checkDatabasePath(d)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want an error with %q", d, err, want)
		}
	}
}

// The later checks build paths from names that flats.db and the outbox
// databases of the copy hold, and read only what those paths name in the
// layout: a flat whose name is no folder of a copy fails the check, a blob
// hash that is no SHA-256 is not looked up, and a board.db the manifest lists
// only as a file is read by no check, whose reads hold for a database that
// passed quick_check.
func TestVerifyBuildsPathsOnlyFromNamesOfTheLayout(t *testing.T) {
	const board = "flats/b/board/board.db"
	opens := func(t *testing.T, out string) ([]Failure, int) {
		t.Helper()
		target := filepath.Join(out, "data", filepath.FromSlash(board))
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
	t.Run("a flat name with a .. element", func(t *testing.T) {
		_, out := goodCopy(t)
		(&fixture{t: t, data: filepath.Join(out, "data")}).sql(nameFlatsDB,
			`INSERT INTO content_state(flat, type, phase, seq, gen, import_store, import_seq) VALUES('x/../b', 'board', 'ready', 1, 0, 'sb', 5)`)
		rehash(t, out)
		wantFailure(t, failuresOf(t, out), "data/flats.db", `names the flat "x/../b"`)
	})
	t.Run("a blob hash that leaves the copy", func(t *testing.T) {
		_, out := goodCopy(t)
		(&fixture{t: t, data: filepath.Join(out, "data")}).sql(board, `INSERT INTO assets(hash) VALUES('../../../../manifest.json')`)
		rehash(t, out)
		wantFailure(t, failuresOf(t, out), "data/flats/b", "malformed blob hash")
	})
	t.Run("a board.db listed only as a file", func(t *testing.T) {
		_, out := goodCopy(t)
		rewriteManifest(t, out, func(m *Manifest) {
			m.Databases = slices.DeleteFunc(m.Databases, func(d Database) bool { return d.Path == "data/"+board })
		})
		fails, n := opens(t, out)
		wantFailure(t, fails, "data/"+board, "not listed as a board database")
		if n != 0 {
			t.Errorf("the checks opened the board.db %d times: %v", n, fails)
		}
	})
}
