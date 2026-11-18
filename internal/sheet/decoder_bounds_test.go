package sheet

import (
	"context"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// storedAxisSheet returns a stopped sheet whose sheet.db holds Sheet1
// with rows 1 to 1000 and columns 1 to 26, and the tab Empty with rows
// 1001 to 2000 and columns 27 to 52, below the counters 2001 and 53, after
// the statements of damage ran on it, each changing one row.
func storedAxisSheet(t *testing.T, damage ...string) *env {
	t.Helper()
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Empty"},{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]]}]}`)
	e.in.Stop()
	db, _, _, err := openDB(filepath.Join(e.dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range damage {
		res, xerr := db.Exec(d)
		if xerr == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				xerr = fmt.Errorf("%s changed %d rows", d, n)
			}
		}
		if xerr != nil {
			err = xerr
			break
		}
	}
	if cerr := db.Close(); err != nil || cerr != nil {
		t.Fatal(err, cerr)
	}
	return e
}

// setBorn returns the statement that gives one axis of a tab the stamp
// runs.
func setBorn(tab, column string, runs ...stampRun) string {
	return fmt.Sprintf(`UPDATE tabs SET %s=x'%x' WHERE name='%s'`, column, encodeStamps(runs), tab)
}

// TestStoredAxisNeedsIdsAndStamps opens sheet.db files whose axis columns
// are damaged in ways that still decode: no ids, no born stamps, a stamp
// run of no ids, stamps that cover only some ids, and stamps for ids that
// are not live on the axis, id 0 among them, in the rows and in the
// columns; a chunk list without chunk 0 or whose first ids do not ascend
// below 2^53; and a revision file under a path the writer does not spell
// so, which a later change of the file would not find. The load fails. A workbook built from such axes could not be
// stored again, so a later structural edit would fail in the middle of its
// transaction: an id past the counter gets its stamp when it is given out,
// beside the run that holds it already.
func TestStoredAxisNeedsIdsAndStamps(t *testing.T) {
	for _, c := range []struct{ name, damage string }{
		{"no row ids", `UPDATE tabs SET row_ids=x'' WHERE name='Empty'`},
		{"no column ids", `UPDATE tabs SET col_ids=x'' WHERE name='Empty'`},
		{"no row stamps", `UPDATE tabs SET row_born=x'' WHERE name='Empty'`},
		{"no column stamps", `UPDATE tabs SET col_born=x'' WHERE name='Empty'`},
		{"a stamp run of no ids", `UPDATE tabs SET row_born=x'010001'||row_born WHERE name='Sheet1'`},
		{"stamps for the first row only", `UPDATE tabs SET row_born=x'010101' WHERE name='Sheet1'`},
		{"a stamp run that wraps", `UPDATE tabs SET row_born=x'01ffffffffffffffffff0101' WHERE name='Sheet1'`},
		{"a row stamp run from id 0", setBorn("Sheet1", "row_born", stampRun{0, 1001, 1})},
		{"a column stamp run from id 0", setBorn("Sheet1", "col_born", stampRun{0, 27, 1})},
		{"a row stamp for the id of the counter", setBorn("Empty", "row_born", stampRun{1001, 1001, 1})},
		{"a column stamp for the id of the counter", setBorn("Empty", "col_born", stampRun{27, 27, 1})},
		{"a row stamp for a row of another tab", setBorn("Sheet1", "row_born", stampRun{1, 1001, 1})},
		{"a column stamp for a column of another tab", setBorn("Sheet1", "col_born", stampRun{1, 27, 1})},
		{"a row stamp run past every row", setBorn("Sheet1", "row_born", stampRun{1, 1000, 1}, stampRun{5000, 1, 1})},
		{"a column stamp run past every column", setBorn("Sheet1", "col_born", stampRun{1, 26, 1}, stampRun{5000, 1, 1})},
		{"no chunk 0", `UPDATE tabs SET chunks=x'05' WHERE name='Sheet1'`},
		{"chunks out of order", `UPDATE tabs SET chunks=x'000503' WHERE name='Sheet1'`},
		{"a chunk twice", `UPDATE tabs SET chunks=x'000505' WHERE name='Sheet1'`},
		{"a chunk from 2^53", `UPDATE tabs SET chunks=x'00` + fmt.Sprintf("%x", binary.AppendUvarint(nil, maxID)) + `' WHERE name='Sheet1'`},
		{"a row file under another spelling", `UPDATE files SET path='tabs/1/rows/0-00.json' WHERE path='tabs/1/rows/0-0.json'`},
		{"axes under another tab spelling", `UPDATE files SET path='tabs/01/axes.json' WHERE path='tabs/1/axes.json'`},
		{"a file the writer never makes", `UPDATE files SET path='notes.json' WHERE path='tabs/1/axes.json'`},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := storedAxisSheet(t, c.damage)
			in, _, err := Open(e.dir, e.b, e.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer in.Stop()
			if err := in.await(context.Background(), 0); err == nil {
				t.Fatal("the damaged sheet.db was loaded")
			} else if werr := refusal(err); werr.Code != "unavailable" || werr.Reason != "load" {
				t.Fatalf("the load failed with %v, want unavailable load", err)
			}
		})
	}
}

// A stored axis whose last id is the highest one, 2^53-1, below a counter
// of 2^53, loads, in the rows and in the columns, with stamp runs that
// hold its ids exactly.
func TestStoredAxisTakesTheHighestIds(t *testing.T) {
	for _, c := range []struct{ axis, counter string }{{"row", "next_row"}, {"col", "next_col"}} {
		t.Run(c.axis+"s", func(t *testing.T) {
			n := map[string]uint64{"row": newTabRows, "col": newTabCols}[c.axis]
			ids := make([]uint64, 0, n)
			for id := uint64(1); id < n; id++ {
				ids = append(ids, id)
			}
			ids = append(ids, maxID-1)
			e := storedAxisSheet(t,
				fmt.Sprintf(`UPDATE tabs SET %s_ids=x'%x' WHERE name='Sheet1'`, c.axis, encodeIDs(ids)),
				setBorn("Sheet1", c.axis+"_born", stampRun{1, n - 1, 1}, stampRun{maxID - 1, 1, 1}),
				fmt.Sprintf(`UPDATE meta SET %s=%d WHERE singleton=1`, c.counter, uint64(maxID)))
			e.open()
			e.want("A1", "1")
			e.in.mu.RLock()
			tab := e.in.wb.tabs[0]
			a := map[string]*axis{"row": &tab.rows, "col": &tab.cols}[c.axis]
			last := a.ids[len(a.ids)-1]
			e.in.mu.RUnlock()
			if last != maxID-1 {
				t.Fatalf("the last %s id is %d, want %d", c.axis, last, uint64(maxID-1))
			}
		})
	}
}

// TestRevisionManifestBounds rolls back to revisions whose manifest names
// more tabs than a sheet may hold or freezes more rows or columns than a
// tab may. Each is refused and the sheet stays as it is: a rollback must
// not bring in a state that no edit could have made.
func TestRevisionManifestBounds(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["kept"]]}]}`)
	n := e.b.importAll(t)
	files, _, release, err := e.b.RevisionFiles(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	release()
	var axes []byte
	for _, f := range files {
		if f.Path == axesPath(1) {
			axes, _ = e.b.ReadBlob(context.Background(), f.Hash)
		}
	}
	if axes == nil {
		t.Fatal("the revision has no axes file")
	}
	// revision returns the files of a revision with the given manifest and
	// the axes of the first tab for every tab the manifest names.
	revision := func(tabs int, first string) []File {
		entries := []string{first}
		for id := 2; id <= tabs; id++ {
			entries = append(entries, fmt.Sprintf(`{"frozen_cols":0,"frozen_rows":0,"id":%d,"name":"T%d"}`, id, id))
		}
		manifest := []byte(`{"format":1,"tabs":[` + strings.Join(entries, ",") + `]}`)
		out := []File{{Path: manifestPath, Hash: contentHash(manifest), Size: int64(len(manifest))}}
		e.b.PutBlob(context.Background(), out[0].Hash, manifest)
		for id := 1; id <= tabs; id++ {
			out = append(out, File{Path: axesPath(TabID(id)), Hash: contentHash(axes), Size: int64(len(axes))})
		}
		e.b.PutBlob(context.Background(), contentHash(axes), axes)
		return out
	}
	tab := func(frozenRows, frozenCols int) string {
		return fmt.Sprintf(`{"frozen_cols":%d,"frozen_rows":%d,"id":1,"name":"Sheet1"}`, frozenCols, frozenRows)
	}
	before := e.dump()
	for _, c := range []struct {
		name  string
		files []File
	}{
		{"51 tabs", revision(maxTabs+1, tab(0, 0))},
		{"21 frozen rows", revision(1, tab(21, 0))},
		{"11 frozen columns", revision(1, tab(0, 11))},
		{"a negative count of frozen rows", revision(1, tab(-1, 0))},
		{"a negative count of frozen columns", revision(1, tab(0, -1))},
	} {
		err := e.in.Rollback(context.Background(), c.files, "", "bounds-"+c.name)
		if werr := refusal(err); err == nil || werr.Code != "unavailable" || werr.Reason != "revision" {
			t.Errorf("a rollback to a revision with %s answered %v, want unavailable revision", c.name, err)
		}
		if got := e.dump(); got != before {
			t.Fatalf("a refused rollback to a revision with %s changed the sheet:\n%s", c.name, dumpDiff(before, got))
		}
	}
	// The bounds themselves load.
	if err := e.in.Rollback(context.Background(), revision(maxTabs, tab(20, 10)), "", "bounds-at"); err != nil {
		t.Fatalf("a rollback to a revision of %d tabs with 20 frozen rows and 10 frozen columns: %v", maxTabs, err)
	}
	if names := e.tabNames(); len(names) != maxTabs {
		t.Fatalf("the rollback left %d tabs", len(names))
	}
}
