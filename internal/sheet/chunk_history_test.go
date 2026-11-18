package sheet

import (
	"path/filepath"
	"strings"
	"testing"
)

// sheetChunks returns the chunks of every tab of the live sheet, and of
// the tab named name.
func sheetChunks(e *env, name string) (all, tab int) {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	for _, t := range e.in.wb.tabs {
		all += len(t.chunks)
		if t.name == name {
			tab = len(t.chunks)
		}
	}
	return all, tab
}

// Tabs that undo brings back hold together no more chunks than a sheet
// does, whatever boundaries their chunk lists were saved at. Each of
// several tabs is filled with a number in every row, recorded, emptied of
// all rows but its last by Alice and deleted by Bob before the next
// boundary, so that Bob's undo entry keeps the tab with its many chunks;
// an agent write of the next tab closes the groups. Bob then undoes every
// deletion before another boundary. A copy of the files as a crash leaves
// them opens the same, and the next revision holds the sheet. At full size
// the tabs are those of 100,000 rows, of 391 chunks each, eleven of which
// with their saved lists would hold 4,302 chunks, more than maxSheetChunks.
func TestRestoredTabsReopenWithinTheChunkBound(t *testing.T) {
	tabs, rows := sized(11, 3), sized(100_000, 600)
	if fullSize {
		full(t, "fills eleven tabs of 100,000 rows")
	}
	e := start(t, unhurried)
	alice, bob := e.person("Alice"), e.person("Bob")
	saved := 0
	for i := range tabs {
		name := jsonf("t_%d", i)
		e.mustAgent(jsonf(`{"ops":[{"op":"add_tab","name":%q}]}`, name))
		for from := 0; from < rows; from += maxCallCells {
			cells := make([]string, min(maxCallCells, rows-from))
			for k := range cells {
				cells[k] = jsonf("[%d]", from+k+1)
			}
			e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":%q,"at":"A%d","rows":[`, name, from+1) + strings.Join(cells, ",") + `]}]}`)
		}
		_, n := sheetChunks(e, name)
		if want := (rows + chunkRows - 1) / chunkRows; n != want {
			t.Fatalf("the recorded tab of %d filled rows has %d chunks, want %d", rows, n, want)
		}
		saved += n
		alice.do(e.deleteRowsOp(name, 1, rows-1))
		bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID(name)))
		if _, n := sheetChunks(e, "Sheet1"); n != 1 {
			t.Fatalf("Sheet1 has %d chunks", n)
		}
	}
	if fullSize && 1+saved <= maxSheetChunks {
		t.Fatalf("the saved lists hold %d chunks with Sheet1, not more than the %d of a sheet", 1+saved, maxSheetChunks)
	}
	for i := range tabs {
		if u := undo(t, bob); u.Applied != 1 {
			t.Fatalf("undo %d of a deletion = %+v", i, u)
		}
	}
	for i := range tabs {
		e.want(jsonf("t_%d!A1", i), jsonf("%d", rows))
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
	if all, _ := sheetChunks(crashed, ""); all != 1+tabs {
		t.Fatalf("Sheet1 and %d tabs that came back hold %d chunks, want one each", tabs, all)
	}
	rev := closeRevision(crashed)
	if got, want := revisionContent(t, crashed, rev), liveContent(crashed); got != want {
		t.Fatalf("the revision differs from the sheet:\n%s", dumpDiff(want, got))
	}
	if all, _ := sheetChunks(crashed, ""); all != 1+tabs {
		t.Fatalf("after the boundary the tabs hold %d chunks, want one each", all)
	}
	crashed.restart()
	if after := crashed.dump(); after != dump {
		t.Fatalf("the sheet opens differently after the boundary:\n%s", dumpDiff(dump, after))
	}
}

// A tab of the last revision that is deleted and then recovered from an
// older revision before the next boundary comes in one chunk, while the
// file list still holds the chunks it had at the last boundary. The next
// revision takes those out: it holds the recovered tab and none of the
// rows the tab had only later, and so does the sheet after a restart.
func TestRecoveredTabLeavesTheFilesOfItsLastBoundary(t *testing.T) {
	e := start(t)
	rows := func(from, to int) string {
		cells := make([]string, 0, to-from+1)
		for i := from; i <= to; i++ {
			cells = append(cells, jsonf("[%d]", i))
		}
		return strings.Join(cells, ",")
	}
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"T"},{"op":"write","tab":"T","at":"A1","rows":[` + rows(1, 300) + `]}]}`)
	older := e.b.importAll(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"T","at":"A301","rows":[` + rows(301, 600) + `]}]}`)
	e.b.importAll(t)
	if _, n := sheetChunks(e, "T"); n != 3 {
		t.Fatalf("the tab of 600 filled rows has %d chunks", n)
	}
	alice := e.person("Alice")
	id := e.tabID("T")
	alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, id))
	if got, err := alice.recover(older, id); err != nil || got != id {
		t.Fatalf("the recovery gave tab %d: %v", got, err)
	}
	if _, n := sheetChunks(e, "T"); n != 1 {
		t.Fatalf("the recovered tab has %d chunks", n)
	}
	e.want("T!A300", "300")
	e.want("T!A301", "")
	rev := closeRevision(e)
	if got, want := revisionContent(t, e, rev), liveContent(e); got != want {
		t.Fatalf("the revision differs from the sheet:\n%s", dumpDiff(want, got))
	}
	dump := e.dump()
	e.restart()
	if after := e.dump(); after != dump {
		t.Fatalf("the sheet opens differently:\n%s", dumpDiff(dump, after))
	}
}
