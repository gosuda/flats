//go:build unix

package backup

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aliasCase lists the sheet.db of the populated host's copy under another
// spelling of its path. link is the name, below the copy, that this system
// must hold for the spelling to resolve to the sheet.db as Windows or a file
// system that ignores case resolves it; "" when the spelling resolves to it
// here already.
type aliasCase struct {
	spelling, link, reason string
}

// sheetAliases are spellings of the sheet.db of the populated host that a
// writer of a copy never produces. Each names the sheet.db on some supported
// system: the backslashes on Windows, the capital letters on a file system
// that ignores case, the trailing dot on Windows, the "..", "./" and leading
// slash everywhere, through filepath.Join. A link stands in for the system
// that resolves it, so that this system opens the sheet.db through the
// spelling as that one would.
var sheetAliases = map[string]aliasCase{
	"backslashes":          {`data\flats\h\sheet\sheet.db`, `data\flats\h\sheet\sheet.db`, "backslash"},
	"a capital file name":  {"data/flats/h/sheet/Sheet.db", "data/flats/h/sheet/Sheet.db", "holds no database"},
	"a capital flat name":  {"data/flats/H/sheet/sheet.db", "data/flats/H/sheet/sheet.db", ""},
	"a trailing dot":       {"data/flats/h/sheet/sheet.db.", "data/flats/h/sheet/sheet.db.", "holds no database"},
	"a .. element":         {"data/flats/h/sheet/../sheet/sheet.db", "", ".."},
	"a . element":          {"data/flats/h/sheet/./sheet.db", "", "not clean"},
	"a leading slash":      {"/data/flats/h/sheet/sheet.db", "", "starts with a slash"},
	"a doubled separator":  {"data/flats/h//sheet/sheet.db", "", "not clean"},
	"a .. through the top": {"data/../data/flats/h/sheet/sheet.db", "", ".."},
}

// alias makes the spelling of c resolve to the sheet.db of the copy in out,
// and lists the sheet.db in the manifest under that spelling and kind only,
// while its file entry keeps the canonical path. It returns the link it
// made, or "".
func (c aliasCase) alias(t *testing.T, out, kind string) string {
	t.Helper()
	made := ""
	if c.link != "" {
		target := filepath.Join(out, "data", filepath.FromSlash(sheetRel))
		link := filepath.Join(out, filepath.FromSlash(c.link))
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatal(err)
		}
		if same, err := sameFileAs(link, target); err != nil || !same {
			// The file system already resolves the spelling, as one that
			// ignores case does, or the link stands in for that.
			if err := os.Link(target, link); err != nil {
				t.Fatal(err)
			}
			made = link
		}
	}
	rewriteManifest(t, out, func(m *Manifest) {
		for i, d := range m.Databases {
			if d.Path == "data/"+sheetRel {
				m.Databases[i] = Database{Path: c.spelling, Kind: kind}
			}
		}
	})
	return made
}

// sameFileAs reports whether p exists and is the file target.
func sameFileAs(p, target string) (bool, error) {
	a, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	b, err := os.Stat(target)
	if err != nil {
		return false, err
	}
	return os.SameFile(a, b), nil
}

// opensOfFile runs the check of the copy in out and counts the database
// connections opened on the file at target, through any path.
func opensOfFile(t *testing.T, out, target string) ([]Failure, int) {
	t.Helper()
	n := 0
	open := openCopy
	openCopy = func(path string) (*sql.DB, error) {
		if same, err := sameFileAs(path, target); err == nil && same {
			n++
		}
		return open(path)
	}
	defer func() { openCopy = open }()
	return failuresOf(t, out), n
}

// A manifest that lists the sheet.db of a copy as a board database, or as a
// sheet database, under a spelling of its path that a copy never holds is
// refused before anything opens the file through that spelling, and the
// sheet.db, which the manifest then lists under no path of the copy, is read
// by no check either.
func TestAnAliasOfASheetIsRefusedBeforeItIsOpened(t *testing.T) {
	for name, c := range sheetAliases {
		for _, kind := range []string{KindBoard, KindSheet} {
			t.Run(name+" as a "+kind, func(t *testing.T) {
				_, out := goodCopy(t)
				target := filepath.Join(out, "data", filepath.FromSlash(sheetRel))
				(&fixture{t: t, data: filepath.Join(out, "data")}).sql(sheetRel, `ALTER TABLE meta ADD COLUMN extra INTEGER`)
				rehash(t, out)
				c.alias(t, out, kind)
				fails, n := opensOfFile(t, out, target)
				reason := c.reason
				if reason == "" {
					reason = "database"
				}
				if name == "a capital flat name" && kind == KindSheet {
					reason = "not as a file of the copy"
				}
				wantFailure(t, fails, c.spelling, reason)
				wantFailure(t, fails, "data/"+sheetRel, "not listed as a sheet database")
				if n != 0 {
					t.Errorf("the checks opened the sheet.db %d times: %v", n, fails)
				}
			})
		}
	}
}

// A copied sheet.db with a text of 16 MiB in meta.seq, listed as a board
// database under a spelling that resolves to it on some system, fails the
// check of the copy, and the peak memory of the checking process stays within a few MiB of the check of
// an undamaged copy. The same file read by quick_check without the sheet
// check takes more than the value, which shows that the measurement sees it.
func TestAnAliasOfASheetStaysOutOfTheVerifiersMemory(t *testing.T) {
	if raceEnabled {
		t.Skip("measures the memory of a process")
	}
	const n = 16 << 20
	_, out := goodCopy(t)
	copied := filepath.Join(out, "data", filepath.FromSlash(sheetRel))
	_, baseline := runVerifier(t, verifyMode+"=verify", verifyPath+"="+out)
	if err := damage(copied, fmt.Sprintf(`UPDATE meta SET seq = CAST(zeroblob(%d) AS TEXT)`, n)); err != nil {
		t.Fatal(err)
	}
	rehash(t, out)
	_, control := runVerifier(t, verifyMode+"=verify", verifyPath+"="+out, verifyControl+"=1")
	t.Logf("the damaged sheet.db read by quick_check: a peak of %d MiB, %d MiB for the undamaged copy", control>>20, baseline>>20)
	if control < baseline+n {
		t.Fatalf("quick_check of the damaged sheet.db took %d MiB, %d MiB for the undamaged copy: the measurement does not see the value", control>>20, baseline>>20)
	}
	damaged, err := readManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range sheetAliases {
		t.Run(name, func(t *testing.T) {
			defer rewriteManifest(t, out, func(m *Manifest) { *m = *damaged })
			if link := c.alias(t, out, KindBoard); link != "" {
				defer os.Remove(link)
			}
			res, peak := runVerifier(t, verifyMode+"=verify", verifyPath+"="+out)
			reason := c.reason
			if reason == "" {
				reason = "database"
			}
			wantFailure(t, res.Failures, c.spelling, reason)
			t.Logf("refused at a peak of %d MiB", peak>>20)
			if peak > baseline+8<<20 {
				t.Errorf("the check took %d MiB, %d MiB for the undamaged copy", peak>>20, baseline>>20)
			}
		})
	}
}

// A name that a copy cannot hold under a canonical path, here a guest file
// and a folder of the data directory and a file of the history of a
// configuration outside it, each with a backslash in its name, is left out of
// the copy and listed as skipped, so the manifest names only canonical paths
// and the copy passes its own check and a later one.
func TestANameACopyCannotSpellIsSkipped(t *testing.T) {
	f := populated(t)
	f.write(`flats/s/data/files/a\b.txt`, "guest file")
	f.write(`flats/s/data/files/c\d/e.txt`, "guest file")
	cfg := filepath.Join(f.root, "etc", nameConfig)
	for p, c := range map[string]string{cfg: "{}", filepath.Join(f.root, "etc", nameConfigHist, `h\1.json`): "{}"} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	o := f.options("names")
	o.ConfigPath = cfg
	res, err := f.run(o)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Out
	for _, p := range []string{`data/flats/s/data/files/a\b.txt`, `data/flats/s/data/files/c\d`, `config-history/h\1.json`} {
		found := false
		for _, s := range res.Skipped {
			found = found || s.Path == p && strings.Contains(s.Reason, "backslash")
		}
		if !found {
			t.Errorf("%s is not listed as skipped: %v", p, res.Skipped)
		}
	}
	m := readCopyManifest(t, out)
	for _, file := range m.Files {
		if strings.Contains(file.Path, `\`) {
			t.Errorf("the manifest lists %s", file.Path)
		}
	}
	for _, p := range []string{`data/flats/s/data/files/a\b.txt`, `data/flats/s/data/files/c\d/e.txt`, `config-history/h\1.json`} {
		mustNotExist(t, filepath.Join(out, p))
	}
	if fails := failuresOf(t, out); len(fails) > 0 {
		t.Errorf("the copy fails its check: %v", fails)
	}
}
