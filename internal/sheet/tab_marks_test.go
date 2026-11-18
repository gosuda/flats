package sheet

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// revisionContent returns the content of revision n, calculated, without
// stamps.
func revisionContent(t *testing.T, e *env, n int) string {
	t.Helper()
	files, _, release, err := e.b.RevisionFiles(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	wb, err := loadRevision(files, func(hash string) (io.ReadCloser, error) { return e.b.OpenBlob(context.Background(), hash) }, nil)
	if err != nil {
		t.Fatalf("revision %d: %v", n, err)
	}
	calculate(t, wb)
	return dumpWorkbook(wb, false)
}

// liveContent returns the content of the live sheet without stamps.
func liveContent(e *env) string {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	return dumpWorkbook(e.in.wb, false)
}

// marks counts the dirty marks the sheet holds in memory and in sheet.db,
// and the tabs it counts as changed.
func marks(t *testing.T, e *env) (memory, stored, changed int) {
	t.Helper()
	e.in.mu.RLock()
	fs := &e.in.wb.files
	memory, changed = len(fs.chunks)+len(fs.axes), len(fs.changed)
	e.in.mu.RUnlock()
	if err := e.in.rdb.QueryRow(`SELECT count(*) FROM files WHERE dirty_seq<>0`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	return memory, stored, changed
}

// Tabs that a person creates, writes and deletes between two boundaries
// leave no dirty marks behind, in memory, in sheet.db or among the changed
// tabs, however many they are; the reserves of the quota count none of
// them. A tab of the last revision that is deleted keeps the mark of its
// axes, so the next revision lists it as changed also after a crash, and
// that revision holds the live sheet without it.
func TestTabChurnLeavesNoMarks(t *testing.T) {
	e := start(t)
	alice := e.person("Alice")
	alice.do(`{"op":"add_tab","name":"Old"}`)
	alice.set("Old!A1", "old")
	closeRevision(e)
	old := e.tabID("Old")
	for i := range 200 {
		name := jsonf("x_%d", i)
		alice.do(jsonf(`{"op":"add_tab","name":%q}`, name))
		alice.set(name+"!A1", "churn")
		alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID(name)))
	}
	alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, old))
	alice.set("A1", "live")
	memory, stored, changed := marks(t, e)
	// Sheet1's chunk and the axes of Old, the tabs Sheet1 and Old.
	if memory > 2 || stored > 3 || changed > 2 {
		t.Fatalf("after 200 tabs came and went the sheet holds %d marks, sheet.db %d and %d changed tabs", memory, stored, changed)
	}
	// A copy of the files as a crash leaves them opens with the same marks.
	crashed := &env{t: t, dir: t.TempDir(), b: e.b, clock: e.clock, opts: e.opts}
	t.Cleanup(func() {
		if crashed.in != nil {
			crashed.in.Stop()
		}
	})
	for _, name := range []string{dbName, dbName + "-wal"} {
		copyFile(t, filepath.Join(e.dir, name), filepath.Join(crashed.dir, name))
	}
	dump := e.dump()
	e.in.Stop()
	crashed.open()
	if after := crashed.dump(); after != dump {
		t.Fatalf("the sheet opens differently after a crash:\n%s", dumpDiff(dump, after))
	}
	if m, s, c := marks(t, crashed); m != memory || s != stored || c != changed {
		t.Fatalf("after a crash the sheet holds %d marks, sheet.db %d and %d changed tabs, before %d, %d and %d", m, s, c, memory, stored, changed)
	}
	e = crashed
	rev := closeRevision(e)
	if !slices.Contains(e.b.entry(rev).Tabs, uint64(old)) {
		t.Fatalf("the revision lists %v as changed, not the deleted tab %d", e.b.entry(rev).Tabs, old)
	}
	if got, want := revisionContent(t, e, rev), liveContent(e); got != want {
		t.Fatalf("the revision differs from the sheet:\n%s", dumpDiff(want, got))
	}
	if m, s, _ := marks(t, e); m != 0 || s != 0 {
		t.Fatalf("after the boundary the sheet holds %d marks and sheet.db %d", m, s)
	}
}

// A tab of the last revision whose rows that held cells are deleted, and
// that is then deleted and brought back by undo, has every chunk written
// at the next boundary, though no cell names any of them: the revision
// holds the tab as it came back, without the cells of the deleted rows,
// and so does the sheet after a restart.
func TestUndoneTabDeleteIsInTheNextRevision(t *testing.T) {
	e := start(t)
	rows := make([]string, 300)
	for i := range rows {
		rows[i] = jsonf("[%d]", i+1)
	}
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"T"},{"op":"write","tab":"T","at":"A1","rows":[` + strings.Join(rows, ",") + `]}]}`)
	e.b.importAll(t)
	e.in.mu.RLock()
	chunks := len(e.in.wb.tab("T").chunks)
	e.in.mu.RUnlock()
	if chunks != 2 {
		t.Fatalf("the tab of 300 filled rows has %d chunks", chunks)
	}
	alice := e.person("Alice")
	alice.do(e.deleteRowsOp("T", 1, 300))
	alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("T")))
	if u := undo(t, alice); u.Applied != 1 {
		t.Fatalf("the undo of the delete = %+v", u)
	}
	e.want("T!A1", "")
	dump := e.dump()
	e.restart()
	if after := e.dump(); after != dump {
		t.Fatalf("the sheet opens differently:\n%s", dumpDiff(dump, after))
	}
	rev := closeRevision(e)
	if got, want := revisionContent(t, e, rev), liveContent(e); got != want {
		t.Fatalf("the revision differs from the sheet:\n%s", dumpDiff(want, got))
	}
}

// A load takes the dirty marks the writer leaves and no other: a mark of a
// chunk of a tab that is gone, of a chunk the tab's list lacks, of a part
// past the first, and of the axes of a gone tab that the last revision
// does not hold are refused, and so is a list of more files than a
// revision has. A list of as many loads.
func TestLoadTakesOnlyTheMarksTheWriterLeaves(t *testing.T) {
	mark := `INSERT INTO files(path,hash,size,dirty_seq,reserve) VALUES('%s','',0,1,0)`
	listed := func(n int) string {
		return jsonf(`WITH RECURSIVE i(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM i WHERE n<%d)
			INSERT INTO files(path,hash,size,dirty_seq,reserve) SELECT 'tabs/1/rows/0-'||n||'.json',printf('%%064x',n),1,0,0 FROM i`, n)
	}
	for name, damage := range map[string]string{
		"a chunk of a gone tab":      jsonf(mark, "tabs/9/rows/0-0.json"),
		"a chunk the list lacks":     jsonf(mark, "tabs/1/rows/5-0.json"),
		"a part past the first":      jsonf(mark, "tabs/1/rows/0-1.json"),
		"the axes of a gone tab":     jsonf(mark, "tabs/9/axes.json"),
		"more files than a revision": listed(maxRevisionFiles + 1),
	} {
		t.Run(name, func(t *testing.T) {
			e := storedAxisSheet(t)
			execDB(t, e, func(db *sql.DB) error {
				_, err := db.Exec(`DELETE FROM files`)
				if err == nil {
					_, err = db.Exec(damage)
				}
				return err
			})
			wantLoadRefused(t, e, name)
		})
	}
	e := storedAxisSheet(t)
	execDB(t, e, func(db *sql.DB) error {
		_, err := db.Exec(`DELETE FROM files`)
		if err == nil {
			_, err = db.Exec(listed(maxRevisionFiles))
		}
		return err
	})
	e.open()
}
