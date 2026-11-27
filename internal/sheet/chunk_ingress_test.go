package sheet

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// chunkRevision returns the files of a revision of the tabs t_0, t_1 and
// on, with ids from 1, whose chunks hold the counts of filled rows sizes
// gives, each row its id as a number in column A, row ids from 1 on. The
// first chunk of a tab starts at 0, every other at its first row.
func chunkRevision(e *env, sizes [][]int) []File {
	var files []File
	put := func(path, data string) {
		files = e.addFile(files, path, data)
	}
	manifest := []string{}
	for i, chunks := range sizes {
		tab := TabID(i + 1)
		manifest = append(manifest, fmt.Sprintf(`{"frozen_cols":0,"frozen_rows":0,"id":%d,"name":"t_%d"}`, tab, i))
		row := 1
		for k, n := range chunks {
			first := RowID(row)
			if k == 0 {
				first = 0
			}
			rows := make([]string, n)
			for j := range rows {
				rows[j] = fmt.Sprintf(`{"cells":{"1":{"v":%d}},"id":%d}`, row, row)
				row++
			}
			if n > 0 {
				put(chunkPrefix(tab, first)+"0.json", `{"rows":[`+strings.Join(rows, ",")+`],"styles":[]}`)
			}
		}
		put(axesPath(tab), fmt.Sprintf(`{"cols":[[1,26]],"rows":[[1,%d]],"widths":{}}`, max(row-1, 1)))
	}
	put(manifestPath, `{"format":1,"tabs":[`+strings.Join(manifest, ",")+`]}`)
	return files
}

// tightChunks returns the chunk sizes of a tab of count chunks that holds
// as few filled rows as a list rebalance leaves can: one row and a full
// chunk in turn, so that every two neighbors hold chunkRows+1 rows.
func tightChunks(count int) []int {
	sizes := make([]int, count)
	for i := range sizes {
		sizes[i] = 1
		if i%2 == 1 {
			sizes[i] = chunkRows
		}
	}
	return sizes
}

// chunkTotal returns the chunks of every tab of the live sheet.
func chunkTotal(e *env) int {
	all, _ := sheetChunks(e, "")
	return all
}

// A revision is taken only with chunk lists rebalance leaves: every two
// neighboring chunks hold more than chunkRows filled rows. Six tabs of
// 657, 657, 657, 657, 656 and 656 chunks of one row each hold the 3,940
// chunks of a sheet with 3,940 cells; a sheet that took them could not add
// a tab that a load would take again. The view and the rollback refuse it,
// and a revision with one pair of neighbors too sparse. A revision whose
// lists are tight, at full size five tabs of 779 chunks, which is as many
// chunks beyond one for each tab as the cells of a sheet allow, is rolled
// back to. After it a new tab, inserted rows and deleted rows, a crash and
// the next boundary keep the sheet within the chunks of a sheet and its
// lists tight, and it opens again the same.
func TestRevisionChunkListsAreTakenAsRebalanceLeavesThem(t *testing.T) {
	tabs, count := sized(5, 2), sized(maxTabChunks, 9)
	if fullSize {
		full(t, "rolls back to five tabs of 99,974 rows")
	}
	f := newLiveFixture(t, unhurried)
	e := f.e
	ones := func(n int) []int {
		sizes := make([]int, n)
		for i := range sizes {
			sizes[i] = 1
		}
		return sizes
	}
	sparse := [][]int{ones(657), ones(657), ones(657), ones(657), ones(656), ones(656)}
	total := 0
	for _, sizes := range sparse {
		total += len(sizes)
	}
	if total != maxSheetChunks {
		t.Fatalf("the sparse tabs hold %d chunks", total)
	}
	f.wantRefused("chunks of one row", chunkRevision(e, sparse))
	f.wantRefused("a sparse pair", chunkRevision(e, [][]int{{1, chunkRows, 1, 1}}))

	lists := make([][]int, tabs)
	for i := range lists {
		lists[i] = tightChunks(count)
	}
	if fullSize && tabs*(count-1) != maxExtraChunks {
		t.Fatalf("the tight tabs hold %d chunks beyond one each, not the %d of a sheet", tabs*(count-1), maxExtraChunks)
	}
	if err := e.in.Rollback(context.Background(), chunkRevision(e, lists), "Alice", "tight"); err != nil {
		t.Fatal(err)
	}
	if got := chunkTotal(e); got != tabs*count {
		t.Fatalf("the rolled back tabs hold %d chunks, want %d", got, tabs*count)
	}
	e.want("t_0!A2", "2")
	alice := e.person("Alice")
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"added"},{"op":"write","tab":"added","at":"A1","rows":[[1]]}]}`)
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"t_0","at":1,"count":3}]}`)
	// The second chunk of t_0 holds the rows at 2 to 257, now 5 to 260.
	alice.do(e.deleteRowsOp("t_0", 5, chunkRows))
	if got := chunkTotal(e); got != tabs*count+1 || got > maxSheetChunks {
		t.Fatalf("after the edits the tabs hold %d chunks, want %d", got, tabs*count+1)
	}
	crashed := &env{t: t, dir: t.TempDir(), b: e.b, clock: e.clock, opts: e.opts}
	for _, name := range []string{dbName, dbName + "-wal"} {
		copyFile(t, filepath.Join(e.dir, name), filepath.Join(crashed.dir, name))
	}
	dump := e.dump()
	e.in.Stop()
	crashed.open()
	t.Cleanup(func() { crashed.in.Stop() })
	if after := crashed.dump(); after != dump {
		t.Fatalf("the sheet opens differently after a crash:\n%s", dumpDiff(dump, after))
	}
	rev := closeRevision(crashed)
	if got, want := revisionContent(t, crashed, rev), liveContent(crashed); got != want {
		t.Fatalf("the revision differs from the sheet:\n%s", dumpDiff(want, got))
	}
	// The emptied chunk and its two neighbors of one row each became one.
	if all, n := sheetChunks(crashed, "t_0"); n != count-2 || all != tabs*count-1 {
		t.Fatalf("after the boundary t_0 holds %d chunks and the sheet %d, want %d and %d", n, all, count-2, tabs*count-1)
	}
	crashed.in.mu.RLock()
	for _, tab := range crashed.in.wb.tabs {
		if _, err := crashed.in.wb.checkChunks(tab, nil, &loadPace{}); err != nil {
			t.Errorf("after the boundary: %v", err)
		}
	}
	crashed.in.mu.RUnlock()
	dump = crashed.dump()
	crashed.restart()
	if after := crashed.dump(); after != dump {
		t.Fatalf("the sheet opens differently after the boundary:\n%s", dumpDiff(dump, after))
	}
}

// A sheet.db holds the chunk lists of the last boundary while its rows
// change, so a load checks the neighbors that no edit marked since then:
// they hold the filled rows rebalance left, more than chunkRows together.
// Three rows in the three chunks [0 2 3] without marks are refused. With
// the chunks 2 and 3 marked, as edits since the boundary would mark them,
// the sheet loads, and the next boundary, after an edit of row 3, merges
// them back into one chunk.
func TestStoredChunkListsAreCheckedByTheirMarks(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1],[2],[3]]}]}`)
	closeRevision(e)
	if ids := e.rowsOf("Sheet1", 1, 3); ids != "[[1,3]]" {
		t.Fatalf("the rows have the ids %s", ids)
	}
	e.in.Stop()
	split := fmt.Sprintf(`UPDATE tabs SET chunks=x'%x' WHERE name='Sheet1'`, encodeRowIDs([]RowID{0, 2, 3}))
	wantLoadRefused(t, copied(t, e, split), "three chunks of one row")

	execDB(t, e, func(db *sql.DB) error {
		if _, err := db.Exec(split); err != nil {
			return err
		}
		for _, first := range []RowID{2, 3} {
			if _, err := db.Exec(`INSERT INTO files(path,hash,size,dirty_seq,reserve) VALUES(?,'',0,1,0)`,
				chunkPrefix(1, first)+strconv.Itoa(0)+".json"); err != nil {
				return err
			}
		}
		return nil
	})
	e.open()
	if _, n := sheetChunks(e, "Sheet1"); n != 3 {
		t.Fatalf("the marked tab holds %d chunks", n)
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"C3","rows":[[4]]}]}`)
	rev := closeRevision(e)
	if got, want := revisionContent(t, e, rev), liveContent(e); got != want {
		t.Fatalf("the revision differs from the sheet:\n%s", dumpDiff(want, got))
	}
	if _, n := sheetChunks(e, "Sheet1"); n != 1 {
		t.Fatalf("after the boundary the tab holds %d chunks", n)
	}
	dump := e.dump()
	e.restart()
	if after := e.dump(); after != dump {
		t.Fatalf("the sheet opens differently:\n%s", dumpDiff(dump, after))
	}
}

// A boundary that records no revision still saves the chunk lists it
// rebalanced, because it takes away the marks that let the old list load.
// Under the marked list [0 2 3] of the three rows A1:A3, Alice writes the
// 1 that A1 holds, which marks chunk 0, and her group closes. The merged
// chunk 0 encodes to the bytes of its recorded file, so no file changes and
// no revision is recorded, but sheet.db holds the list [0] without marks,
// and the sheet opens as it was.
func TestABoundaryWithoutARevisionSavesItsChunkLists(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1],[2],[3]]}]}`)
	recorded := closeRevision(e)
	e.in.Stop()
	execDB(t, e, func(db *sql.DB) error {
		if _, err := db.Exec(fmt.Sprintf(`UPDATE tabs SET chunks=x'%x' WHERE name='Sheet1'`, encodeRowIDs([]RowID{0, 2, 3}))); err != nil {
			return err
		}
		for _, first := range []RowID{2, 3} {
			if _, err := db.Exec(`INSERT INTO files(path,hash,size,dirty_seq,reserve) VALUES(?,'',0,1,0)`,
				chunkPrefix(1, first)+"0.json"); err != nil {
				return err
			}
		}
		return nil
	})
	e.open()
	content := revisionContent(t, e, recorded)
	stored := func() (chunks string, marks int) {
		t.Helper()
		if err := e.in.rdb.QueryRow(`SELECT hex(chunks) FROM tabs WHERE name='Sheet1'`).Scan(&chunks); err != nil {
			t.Fatal(err)
		}
		if err := e.in.rdb.QueryRow(`SELECT count(*) FROM files WHERE dirty_seq<>0 OR hash=''`).Scan(&marks); err != nil {
			t.Fatal(err)
		}
		return chunks, marks
	}
	alice := e.person("Alice")
	alice.set("A1", "1")
	if _, marks := stored(); marks == 0 {
		t.Fatal("the edit marks no chunk")
	}
	if n := closeRevision(e); n != recorded {
		t.Fatalf("the boundary recorded revision %d after %d", n, recorded)
	}
	if got := revisionContent(t, e, recorded); got != content {
		t.Fatalf("the revision changed:\n%s", dumpDiff(content, got))
	}
	if chunks, marks := stored(); chunks != fmt.Sprintf("%X", encodeRowIDs([]RowID{0})) || marks != 0 {
		t.Fatalf("sheet.db holds the chunks %s with %d marks after the boundary", chunks, marks)
	}
	dump := e.dump()
	e.restart()
	if after := e.dump(); after != dump {
		t.Fatalf("the sheet opens differently:\n%s", dumpDiff(dump, after))
	}
}
