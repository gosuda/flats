package sheet

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// liveFixture is a sheet with a revision to view and roll back: Alice wrote
// A1 before it and A2 after it.
type liveFixture struct {
	e     *env
	alice *person
	files []File
	date  string
	rows  string // the path of the revision's row file
	views int
}

func newLiveFixture(t *testing.T, opts ...func(*Options)) *liveFixture {
	t.Helper()
	e := start(t, opts...)
	f := &liveFixture{e: e, alice: e.person("Alice")}
	f.alice.set("A1", "before")
	n := closeRevision(e)
	f.alice.set("A2", "live")
	files, date, release, err := e.b.RevisionFiles(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	f.files, f.date, f.rows = files, date, rowFileOf(t, files)
	return f
}

// revision returns the revision's files with Sheet1 rows rows long and
// its row file holding rowFile.
func (f *liveFixture) revision(rows int, rowFile string) []File {
	axes := fmt.Sprintf(`{"cols":[[1,26]],"rows":[[1,%d]],"widths":{}}`, rows)
	return f.e.withFile(f.e.withFile(f.files, axesPath(1), axes), f.rows, rowFile)
}

// view reads A1 of a view of the files.
func (f *liveFixture) view(files []File) error {
	f.views++
	return f.e.viewRevision(5000+f.views, files, f.date)
}

// wantRefused views and rolls back to the files and wants both refused as
// a damaged revision. The live sheet, its sequence, id counters, counts,
// edit groups, undo history and outbox stay as they were, and the sheet
// opens again as it was.
func (f *liveFixture) wantRefused(name string, files []File) {
	e := f.e
	e.t.Helper()
	snapshot := func() string {
		e.in.mu.RLock()
		count := e.in.wb.count
		e.in.mu.RUnlock()
		entries, lost := f.alice.undoDepth()
		return fmt.Sprintf("seq %d\ncounters %v\ncount %+v\ngroups %s\nundo %d %d\noutbox %v\n%s",
			e.seq(), e.counters(), count, e.groupRows(), entries, lost, e.outboxAfter(0), e.dump())
	}
	before := snapshot()
	if err := f.view(files); refusal(err).Reason != "revision" {
		e.t.Errorf("%s: the view answered %v, want a refusal of the revision", name, err)
	}
	if err := e.in.Rollback(context.Background(), files, "Alice", name); refusal(err).Reason != "revision" {
		e.t.Errorf("%s: the rollback answered %v, want a refusal of the revision", name, err)
	}
	if after := snapshot(); after != before {
		e.t.Fatalf("%s: the refusals changed the sheet:\n%s", name, dumpDiff(before, after))
	}
	dump := e.dump()
	e.restart()
	if after := e.dump(); after != dump {
		e.t.Fatalf("%s: the sheet opens differently after the refusals:\n%s", name, dumpDiff(dump, after))
	}
}

// cellsFile returns a row file whose cells, 26 to a row from row 1 on,
// hold the entries in order.
func cellsFile(entries []string) string {
	var sb strings.Builder
	sb.WriteString(`{"rows":[`)
	for i, entry := range entries {
		col := i%26 + 1
		if col == 1 {
			if i > 0 {
				sb.WriteString(`}},`)
			}
			fmt.Fprintf(&sb, `{"id":%d,"cells":{`, i/26+1)
		} else {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"%d":%s`, col, entry)
	}
	if len(entries) > 0 {
		sb.WriteString(`}}`)
	}
	sb.WriteString(`],"styles":[]}`)
	return sb.String()
}

// chunkedCells returns the row files that hold the entries, 26 to a row
// from row 1 on, as the writer lays them out: a chunk for each 256 rows,
// the first from 0 and each other from the id of its first row, and the
// parts of a chunk each ended at the first cell at or past chunkPartSize
// (encodeChunk).
func chunkedCells(entries []string) map[string]string {
	files := map[string]string{}
	rows := rowsFor(len(entries))
	for lo := 0; lo < rows; lo += chunkRows {
		first := RowID(0)
		if lo > 0 {
			first = RowID(lo + 1)
		}
		var body strings.Builder
		part := 0
		closePart := func() {
			files[chunkPrefix(1, first)+strconv.Itoa(part)+".json"] = `{"rows":[` + body.String() + `],"styles":[]}`
			body.Reset()
			part++
		}
		for row := lo; row < min(lo+chunkRows, rows); row++ {
			open := false
			for col := 0; col < 26 && row*26+col < len(entries); col++ {
				switch {
				case open:
					body.WriteByte(',')
				case body.Len() > 0:
					body.WriteString(`,{"cells":{`)
				default:
					body.WriteString(`{"cells":{`)
				}
				open = true
				fmt.Fprintf(&body, `"%d":%s`, col+1, entries[row*26+col])
				if body.Len() >= chunkPartSize {
					fmt.Fprintf(&body, `},"id":%d}`, row+1)
					closePart()
					open = false
				}
			}
			if open {
				fmt.Fprintf(&body, `},"id":%d}`, row+1)
			}
		}
		if body.Len() > 0 {
			closePart()
		}
	}
	return files
}

// revisionOf returns the revision's files with Sheet1 as long as the
// entries need and its row files holding them (chunkedCells).
func (f *liveFixture) revisionOf(entries []string) []File {
	files := f.e.withFile(f.files, axesPath(1), fmt.Sprintf(`{"cols":[[1,26]],"rows":[[1,%d]],"widths":{}}`, rowsFor(len(entries))))
	for path, data := range chunkedCells(entries) {
		if path == f.rows {
			files = f.e.withFile(files, path, data)
		} else {
			files = f.e.addFile(files, path, data)
		}
	}
	return files
}

// rowsFor returns the rows that 26 cells to a row take for n cells.
func rowsFor(n int) int { return max((n+25)/26, 1) }

// damageDB runs statements on the sheet.db of a stopped instance, each of
// which must change one row.
func damageDB(t *testing.T, e *env, statements ...string) {
	t.Helper()
	db, _, _, err := openDB(filepath.Join(e.dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statements {
		res, xerr := db.Exec(s)
		if xerr == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				xerr = fmt.Errorf("%.80s changed %d rows", s, n)
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
}

// wantLoadRefused opens the sheet of a stopped instance and wants the load
// refused, or the open, which checks the row of meta and the length of the
// rows of cells first.
func wantLoadRefused(t *testing.T, e *env, name string) {
	t.Helper()
	in, _, err := Open(e.dir, e.b, e.opts)
	if err != nil {
		t.Logf("%s: the open refused the sheet: %v", name, err)
		return
	}
	defer in.Stop()
	if err := in.await(context.Background(), 0); err == nil {
		t.Errorf("%s: the damaged sheet.db was loaded", name)
	} else if werr := refusal(err); werr.Code != "unavailable" || werr.Reason != "load" {
		t.Errorf("%s: the load failed with %v, want unavailable load", name, err)
	}
}

// An unreadable formula is stored as typed, in the x field of a row file
// and under content kind 8 in sheet.db. Text there that the lexer can
// split is no unreadable formula: read at A1 it would be a formula that
// refers to cells, and far from A1 its text can be longer than a formula
// may be. farSum in Z1000 reads as 2,730 references to Z1000, 16,380
// characters. Both loaders refuse such text, and still load an unreadable
// formula, which reads as it was typed wherever it stands.
func TestStoredUnreadableFormulaIsUnreadable(t *testing.T) {
	ctx := context.Background()
	f := newLiveFixture(t)
	e := f.e
	for name, file := range map[string]string{
		"a readable formula in A1":  rowFile(`{"x":"=A1+A1"}`),
		"farSum in Z1000":           `{"rows":[{"cells":{"26":{"x":` + strconv.Quote(farSum) + `}},"id":1000}],"styles":[]}`,
		"a formula and its x field": rowFile(`{"f":"=1","x":"=1+@"}`),
		"an x field without =":      rowFile(`{"x":"A1+@"}`),
	} {
		f.wantRefused(name, f.revision(1000, file))
	}
	const typed = "=A1+@"
	legit := f.revision(1000, `{"rows":[{"cells":{"1":{"x":"=A1+@"}},"id":1},{"cells":{"26":{"x":"=A1+@"}},"id":1000}],"styles":[]}`)
	if err := f.view(legit); err != nil {
		t.Fatalf("the view of unreadable formulas: %v", err)
	}
	if err := e.in.Rollback(ctx, legit, "Alice", "unreadable"); err != nil {
		t.Fatalf("the rollback to unreadable formulas: %v", err)
	}
	e.restart()
	for _, addr := range []string{"A1", "Z1000"} {
		e.wantText(addr, typed)
		e.want(addr, "#ERROR!")
	}

	// Sheet1 has the row and column ids of its positions, as the writer
	// gives them out from 1 on.
	if _, row, col := e.ids("Z1000"); row != 1000 || col != 26 {
		t.Fatalf("Z1000 has the ids %d/%d", row, col)
	}
	for name, damage := range map[string]string{
		"a readable formula in A1": `UPDATE cells SET kind=8,num=NULL,str='=A1+A1' WHERE tab=1 AND row=1 AND col=1`,
		"text without =":           `UPDATE cells SET kind=8,num=NULL,str='A1+@' WHERE tab=1 AND row=1 AND col=1`,
		"farSum in Z1000": `INSERT INTO cells(tab,row,col,kind,num,str,style,seq,text_seq) VALUES(1,1000,26,8,NULL,'` +
			farSum + `',0,1,0)`,
	} {
		t.Run(name, func(t *testing.T) {
			e := storedAxisSheet(t, damage)
			wantLoadRefused(t, e, name)
		})
	}
}

// A cells row of sheet.db holds the columns cellColumns writes for its
// kind, and a number a cell can hold. Any other row is damage, which the
// load refuses: the text of a number would be kept and counted nowhere, a
// kind of no content would be read as a blank, and a number past the
// largest one could not be written to a revision.
func TestStoredCellHoldsTheColumnsOfItsKind(t *testing.T) {
	for name, set := range map[string]string{
		"a kind of no content":   "kind=9,num=1,str=NULL",
		"a text without text":    "kind=2,num=1,str=NULL",
		"a formula and a number": "kind=7,num=1,str='=1'",
		"a number with text":     "kind=1,num=1,str='" + strings.Repeat("x", 1000) + "'",
		"an infinite number":     "kind=1,num=9e999,str=NULL",
		"a boolean of 2":         "kind=3,num=2,str=NULL",
		"a blank with a number":  "kind=0,num=1,str=NULL",
	} {
		t.Run(name, func(t *testing.T) {
			e := storedAxisSheet(t, `UPDATE cells SET `+set+` WHERE tab=1 AND row=1 AND col=1`)
			wantLoadRefused(t, e, name)
		})
	}
}

// testStoredLimit checks that the cells of a stored state count toward a
// limit of the whole sheet as written ones do. A revision of the cells
// over, past the limit, is refused as a view and as a rollback without
// changing the sheet. One of the cells boundary, at the limit, is viewed,
// rolled back to and opens again, and want checks it. The statement of
// grow then takes the sheet.db past the limit, and it does not load.
func testStoredLimit(t *testing.T, limit string, boundary, over []string, grow func(e *env) string, want func(e *env)) {
	t.Helper()
	ctx := context.Background()
	f := newLiveFixture(t, unhurried)
	e := f.e
	f.wantRefused(limit+" past the limit", f.revisionOf(over))
	at := f.revisionOf(boundary)
	if err := f.view(at); err != nil {
		t.Fatalf("the view of the %s at the limit: %v", limit, err)
	}
	if err := e.in.Rollback(ctx, at, "Alice", limit); err != nil {
		t.Fatalf("the rollback to the %s at the limit: %v", limit, err)
	}
	e.restart()
	want(e)
	damage := grow(e)
	e.in.Stop()
	damageDB(t, e, damage)
	wantLoadRefused(t, e, limit+" past the limit")
}

func repeated(entry string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = entry
	}
	return out
}

// nthCell returns the ids of the tab, row and column of cell i of cells
// written 26 to a row from A1 on.
func nthCell(e *env, i int) (TabID, RowID, ColID) {
	return e.ids(fmt.Sprintf("%s%d", string(rune('A'+i%26)), i/26+1))
}

func TestStoredFormulasCountTowardTheLimit(t *testing.T) {
	slow(t, "loads and calculates 100,000 formulas a few times")
	testStoredLimit(t, "formulas",
		repeated(`{"f":"=1"}`, maxFormulas), repeated(`{"f":"=1"}`, maxFormulas+1),
		func(e *env) string {
			tab, row, col := nthCell(e, maxFormulas)
			return fmt.Sprintf(`INSERT INTO cells(tab,row,col,kind,num,str,style,seq,text_seq) VALUES(%d,%d,%d,7,NULL,'=1',0,1,0)`, tab, row, col)
		},
		func(e *env) {
			e.in.mu.RLock()
			n := e.in.wb.count.formulas
			e.in.mu.RUnlock()
			if n != maxFormulas {
				t.Errorf("the sheet holds %d formulas", n)
			}
			e.want("A1", "1")
		})
}

func TestStoredCellsCountTowardTheLimit(t *testing.T) {
	full(t, "holds 500,000 cells")
	testStoredLimit(t, "cells",
		repeated(`{"v":1}`, maxCells), repeated(`{"v":1}`, maxCells+1),
		func(e *env) string {
			tab, row, col := nthCell(e, maxCells)
			return fmt.Sprintf(`INSERT INTO cells(tab,row,col,kind,num,str,style,seq,text_seq) VALUES(%d,%d,%d,1,2,NULL,0,1,0)`, tab, row, col)
		},
		func(e *env) {
			e.in.mu.RLock()
			n := e.in.wb.count.cells
			e.in.mu.RUnlock()
			if n != maxCells {
				t.Errorf("the sheet holds %d cells", n)
			}
		})
}

// The typed text of the whole sheet counts texts and formulas, each
// formula as its text at its cell: 1,525 texts of 32,767 characters, one
// of 22,133 and a formula of 8,192 characters make exactly 50,000,000.
func TestStoredTextCountsTowardTheLimit(t *testing.T) {
	full(t, "holds 50,000,000 characters of text")
	long := `{"v":"` + strings.Repeat("x", formula.MaxTextLen) + `"}`
	lit := `="` + strings.Repeat("y", formula.MaxFormulaChars-3) + `"`
	rest := maxSheetText - 1525*formula.MaxTextLen - formula.MaxFormulaChars
	cells := func(rest int) []string {
		return append(repeated(long, 1525), `{"v":"`+strings.Repeat("z", rest)+`"}`, `{"f":`+strconv.Quote(lit)+`}`)
	}
	testStoredLimit(t, "sheet_text", cells(rest), cells(rest+1),
		func(e *env) string {
			tab, row, col := nthCell(e, 1525)
			return fmt.Sprintf(`UPDATE cells SET str=str||'z' WHERE tab=%d AND row=%d AND col=%d`, tab, row, col)
		},
		func(e *env) {
			// The sum the load made, and the one a look through every
			// formula makes.
			e.in.mu.Lock()
			wb := e.in.wb
			summed, derived := wb.count.typed+wb.formulaText, wb.formulaTextOK
			wb.formulaTextOK = false
			looked, ok := wb.typedText(formula.NewBudget(nil))
			e.in.mu.Unlock()
			if !derived || summed != maxSheetText || !ok || looked != maxSheetText {
				t.Errorf("the load summed %d characters of typed text (derived %v), a look through the formulas %d", summed, derived, looked)
			}
		})
}

// TestCaptureOfReloadedFormulasCharges fills a column with one long
// formula of a number, opens the sheet again, reads the column one cell a
// page and clears it while the read's capture is kept. Read from sheet.db,
// each cell's text is a string of its own, so the load gives the cells the
// text of the formula they share. The capture keeps what it is charged for
// and gives it back when it goes.
func TestCaptureOfReloadedFormulasCharges(t *testing.T) {
	slow(t, "writes thousands of long formulas")
	n := sized(6000, 1500)
	text := "=1" + strings.Repeat(" ", 7998)
	e := start(t, unhurried)
	const batch = 900
	for from := 0; from < n; from += batch {
		rows := repeated("["+strconv.Quote(text)+"]", min(batch, n-from))
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, from+1, strings.Join(rows, ",")))
	}
	e.restart()
	e.b.importAll(t)
	e.b.forgetRevisions()
	base := captureBytes.Load()
	first := e.read(ReadQuery{Tab: "Sheet1", Range: fmt.Sprintf("A1:A%d", n), Limit: 1})
	cursor := cursorOf(t, first)
	charged := captureBytes.Load() - base
	e.mustAgent(jsonf(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:A%d"]}]}`, n))
	e.want("A2", "")
	if got := fmt.Sprint(e.read(ReadQuery{Cursor: cursor})); !strings.Contains(got, text) {
		t.Fatalf("the second page after the clear: %.200s", got)
	}
	e.b.importAll(t)
	e.b.forgetRevisions()
	sheetHeap := func() int64 { return quietHeap(t, 1<<20) - e.b.blobBytes() }
	held := sheetHeap()
	e.in.captures.clear()
	if got := captureBytes.Load(); got != base {
		t.Fatalf("after the capture went %d bytes stay charged, want %d", got, base)
	}
	kept := held - sheetHeap()
	t.Logf("%d formulas: the capture is charged %d KiB and kept %d KiB of the heap", n, charged>>10, kept>>10)
	if !measures {
		return
	}
	// A reading of the heap is up to 1 MiB above what is reachable.
	if kept > charged+1<<20 {
		t.Errorf("the capture of %d reloaded formulas kept %d KiB of the heap and is charged %d KiB", n, kept>>10, charged>>10)
	}
}
