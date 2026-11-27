package sheet

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func (e *env) formatRange(tab string, row, rows, col, cols int, set string) string {
	return fmt.Sprintf(`{"op":"format","tab":%d,"rows":%s,"cols":%s,"set":%s}`, e.tabID(tab), e.rowsOf(tab, row, rows), e.colsOf(tab, col, cols), set)
}

func (e *env) bold(addr string) bool {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t, row, col := e.at(addr)
	c := t.cell(row, col)
	return c != nil && e.in.wb.styles.format(c.style).Bold
}

// blankBytes returns the bytes the store of collected blank records takes
// on disk: its file and the journal beside it.
func (e *env) blankBytes() int64 {
	e.t.Helper()
	e.in.mu.RLock()
	dir := e.in.scratch.dir
	e.in.mu.RUnlock()
	if dir == "" {
		return 0
	}
	list, err := os.ReadDir(dir)
	if err != nil {
		e.t.Fatal(err)
	}
	var n int64
	for _, f := range list {
		if !isBlankFile(f.Name()) {
			continue
		}
		info, err := f.Info()
		if err != nil {
			e.t.Fatal(err)
		}
		n += info.Size()
	}
	return n
}

// collectTo runs the maintenance tick until no more than limit blank
// records are left.
func (e *env) collectTo(limit int) {
	e.t.Helper()
	for i := 0; e.blanks() > limit; i++ {
		if i == 30 {
			e.t.Fatalf("%d blank records are left after %d ticks", e.blanks(), i)
		}
		e.tick()
	}
}

// A cell that is written again after its blank record was collected keeps
// the stamps of the groups the write does not touch: another writer's
// format on a cell a person cleared leaves the person's undo of the clear
// whole, and another writer's content leaves the undo of a format.
func TestUndoKeepsStampsOfUntouchedGroups(t *testing.T) {
	const rows, cols, limit = 20, 20, 60
	for _, mode := range []string{"in memory", "in scratch files"} {
		setup := func(t *testing.T) (*env, *person, *person) {
			e := start(t)
			e.in.mu.Lock()
			e.in.wb.collected.limit = limit
			if mode != "in memory" {
				e.in.undoPool().entryMax = -1
			}
			e.in.mu.Unlock()
			fillNumbers(e, "Sheet1", rows, cols)
			return e, e.person("Alice"), e.person("Bob")
		}
		// others makes an agent clear every cell but those of row 1, so
		// that the tick collects the older records.
		others := func(e *env) {
			e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A2:T%d"]}]}`, rows))
		}
		t.Run(mode+", a format over a cleared cell", func(t *testing.T) {
			e, alice, bob := setup(t)
			alice.do(e.clearOp("Sheet1", 1, 1, 1, 1))
			others(e)
			e.collectTo(limit)
			if e.hasRecord("A1") || e.blankRows() != 1 {
				t.Fatalf("A1 has a record: %v; %d records on disk, want the one of A1", e.hasRecord("A1"), e.blankRows())
			}
			if ack := bob.do(e.formatRange("Sheet1", 1, 1, 1, 1, `{"bold":true}`)); status(ack) != "applied" {
				t.Fatalf("format: %s", status(ack))
			}
			if n := e.blankRows(); n != 0 {
				t.Fatalf("%d records on disk for a cell that has a record again", n)
			}
			wantUndo(t, "undo of the clear", undo(t, alice), undoAck{Applied: 1})
			if e.shown("A1") != "1001" || !e.bold("A1") {
				t.Fatalf("A1 shows %q, bold %v; want Alice's value with Bob's format", e.shown("A1"), e.bold("A1"))
			}
			wantUndo(t, "redo of the clear", redo(t, alice), undoAck{Applied: 1})
			if e.shown("A1") != "" || !e.bold("A1") {
				t.Fatalf("after the redo A1 shows %q, bold %v", e.shown("A1"), e.bold("A1"))
			}
			wantUndo(t, "undo of the format", undo(t, bob), undoAck{Applied: 1})
			wantUndo(t, "second undo of the clear", undo(t, alice), undoAck{Applied: 1})
			if e.shown("A1") != "1001" || e.bold("A1") {
				t.Fatalf("A1 shows %q, bold %v; want Alice's value without the format", e.shown("A1"), e.bold("A1"))
			}
			wantUndo(t, "redo of the format", redo(t, bob), undoAck{Applied: 1})
			if e.shown("A1") != "1001" || !e.bold("A1") {
				t.Fatalf("after Bob's redo A1 shows %q, bold %v", e.shown("A1"), e.bold("A1"))
			}
			e.restart()
			e.want("A1", "1001")
		})
		// The undo of the format takes the cell's record away again when
		// the format was all it held; the clear is still undone.
		t.Run(mode+", a format that is undone before the clear", func(t *testing.T) {
			e, alice, bob := setup(t)
			alice.do(e.clearOp("Sheet1", 1, 1, 1, 1))
			others(e)
			e.collectTo(limit)
			bob.do(e.formatRange("Sheet1", 1, 1, 1, 1, `{"bold":true}`))
			wantUndo(t, "undo of the format", undo(t, bob), undoAck{Applied: 1})
			// The cell is a blank record without a format again. Another
			// collection takes it, with both writers' stamps.
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A2","rows":[[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20]]}]}`)
			e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A2:T2"]}]}`)
			e.in.mu.Lock()
			e.in.wb.collected.limit = 2
			e.in.mu.Unlock()
			e.collectTo(2)
			if e.hasRecord("A1") {
				t.Fatal("A1 kept its record through the second collection")
			}
			wantUndo(t, "redo of the format", redo(t, bob), undoAck{Applied: 1})
			wantUndo(t, "undo of the clear", undo(t, alice), undoAck{Applied: 1})
			if e.shown("A1") != "1001" || !e.bold("A1") {
				t.Fatalf("A1 shows %q, bold %v; want Alice's value with Bob's format", e.shown("A1"), e.bold("A1"))
			}
		})
		t.Run(mode+", content over a cell whose format was reset", func(t *testing.T) {
			e, alice, bob := setup(t)
			// V1 lies outside the filled cells and has no record.
			alice.do(e.formatRange("Sheet1", 1, 1, 22, 1, `{"bold":true,"italic":true}`))
			alice.do(e.formatRange("Sheet1", 1, 1, 22, 1, `{"bold":null,"italic":null}`))
			others(e)
			e.collectTo(limit)
			if e.hasRecord("V1") || e.blankRows() != 1 {
				t.Fatalf("V1 has a record: %v; %d records on disk, want the one of V1", e.hasRecord("V1"), e.blankRows())
			}
			bob.set("V1", "bob")
			wantUndo(t, "undo of the reset", undo(t, alice), undoAck{Applied: 2})
			if e.shown("V1") != "bob" || !e.bold("V1") {
				t.Fatalf("V1 shows %q, bold %v; want Bob's text with Alice's format", e.shown("V1"), e.bold("V1"))
			}
			wantUndo(t, "undo of the format", undo(t, alice), undoAck{Applied: 2})
			if e.shown("V1") != "bob" || e.bold("V1") {
				t.Fatalf("V1 shows %q, bold %v; want Bob's text without a format", e.shown("V1"), e.bold("V1"))
			}
			wantUndo(t, "redo of the format", redo(t, alice), undoAck{Applied: 2})
			wantUndo(t, "redo of the reset", redo(t, alice), undoAck{Applied: 2})
			wantUndo(t, "undo of Bob's text", undo(t, bob), undoAck{Applied: 1})
			if e.shown("V1") != "" || e.bold("V1") {
				t.Fatalf("V1 shows %q, bold %v after every redo and Bob's undo", e.shown("V1"), e.bold("V1"))
			}
			wantUndo(t, "second undo of the reset", undo(t, alice), undoAck{Applied: 2})
			if !e.bold("V1") {
				t.Fatal("V1 is not bold after the second undo of the reset")
			}
		})
		// An agent's write counts like a person's: the groups it does not
		// touch stay the person's.
		t.Run(mode+", an agent's format and a paste over cleared cells", func(t *testing.T) {
			e, alice, bob := setup(t)
			alice.do(e.clearOp("Sheet1", 1, 1, 1, 3))
			others(e)
			e.collectTo(limit)
			e.mustAgent(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"bold":true}}]}`)
			tab, row, col := e.ids("B1")
			if ack := bob.do(jsonf(`{"op":"format","tab":%d,"rows":[[%d,1]],"cols":[[%d,2]],"set":{"italic":true}}`, tab, row, col)); status(ack) != "applied" {
				t.Fatalf("format: %s", status(ack))
			}
			wantUndo(t, "undo of the clear", undo(t, alice), undoAck{Applied: 3})
			e.want("A1", "1001")
			e.want("B1", "1002")
			e.want("C1", "1003")
			if !e.bold("A1") {
				t.Fatal("A1 lost the agent's format")
			}
		})
	}
}

// What writers clear and the tick collects costs no scratch storage while
// no retained undo entry holds a stamp of it, however long another
// person's entry stays.
func TestCollectedStampStoreStaysBounded(t *testing.T) {
	const limit = 20
	setup := func(t *testing.T) (*env, *person) {
		e := start(t)
		e.in.mu.Lock()
		e.in.wb.collected.limit = limit
		e.in.mu.Unlock()
		alice := e.person("Alice")
		alice.set("A1", "kept")
		alice.set("A1", "kept longer")
		return e, alice
	}
	row := func(from int) string {
		cells := make([]string, 30)
		for i := range cells {
			cells[i] = fmt.Sprint(from + i)
		}
		return "[" + strings.Join(cells, ",") + "]"
	}
	t.Run("an agent on new tabs", func(t *testing.T) {
		e, alice := setup(t)
		cycles := sized(2000, 300)
		var peak int64
		for i := 0; i < cycles; i++ {
			e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"add_tab","name":"T%d"},{"op":"write","tab":"T%d","at":"A1","rows":[%s,%s]}]}`, i, i, row(1), row(100)))
			e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"clear","tab":"T%d","ranges":["A1:AD2"]}]}`, i))
			e.collectTo(limit)
			e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"delete_tab","tab":"T%d"}]}`, i))
			e.b.importAll(t)
			peak = max(peak, e.blankBytes())
		}
		if peak != 0 || e.blankRows() != 0 {
			t.Fatalf("the store took %d bytes and holds %d records for cells no undo entry names", peak, e.blankRows())
		}
		wantUndo(t, "Alice's undo", undo(t, alice), undoAck{Applied: 1})
		e.want("A1", "kept")
	})
	// A person's own clears are kept while the person can undo them, and
	// only so long: the file stops growing once the oldest actions leave
	// the person's history.
	t.Run("a person on new rows", func(t *testing.T) {
		e, alice := setup(t)
		bob := e.person("Bob")
		cycles := sized(1200, 300)
		sizes := make([]int64, cycles)
		for i := 0; i < cycles; i++ {
			e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":2,"count":2}]}`)
			e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A2","rows":[%s,%s]}]}`, row(1), row(100)))
			if ack := bob.do(e.clearOp("Sheet1", 2, 2, 1, 30)); status(ack) != "applied" {
				t.Fatalf("clear %d: %s", i, status(ack))
			}
			e.collectTo(limit)
			// The rows stay, empty, so the sheet grows by rows only.
			e.b.importAll(t)
			sizes[i] = e.blankBytes()
		}
		// Bob keeps 100 actions. The size after 150 cycles is all the
		// file ever needs.
		early, late := sizes[149], int64(0)
		for _, size := range sizes[150:] {
			late = max(late, size)
		}
		t.Logf("%d cycles: %d bytes after 150, at most %d after that; %d records", cycles, early, late, e.blankRows())
		if early == 0 || late > early+8*blankPage {
			t.Fatalf("the file grew from %d bytes after 150 cycles to %d", early, late)
		}
		if n := e.blankRows(); n > maxUndoEntries*60 {
			t.Fatalf("%d records on disk for %d retained actions of 60 cells", n, maxUndoEntries)
		}
		// The newest clear is undone from the kept stamps.
		wantUndo(t, "Bob's undo", undo(t, bob), undoAck{Applied: 60})
		e.want("A2", "1")
		e.want("AD3", "129")
		wantUndo(t, "Alice's undo", undo(t, alice), undoAck{Applied: 1})
		e.want("A1", "kept")
		// When the actions leave Bob's history, their records leave the
		// file, and the file gives the space back.
		before := e.blankBytes()
		for i := 0; i < maxUndoEntries+1; i++ {
			bob.set("B1", fmt.Sprint(i))
		}
		e.tick()
		after := e.blankBytes()
		if n := e.blankRows(); n != 0 || after > 8*blankPage || after >= before {
			t.Fatalf("%d records and %d bytes are left of %d bytes after the actions left the history", n, after, before)
		}
	})
	// The records of rows, columns and tabs that are gone for good go at
	// the next tick, and those an undo entry can bring back stay.
	t.Run("rows, columns and tabs that left", func(t *testing.T) {
		e, _ := setup(t)
		bob := e.person("Bob")
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"add_tab","name":"Other"},{"op":"write","tab":"Other","at":"A1","rows":[%s,%s,%s]},{"op":"write","tab":"Sheet1","at":"A2","rows":[%s,%s,%s]}]}`,
			row(1), row(100), row(200), row(1), row(100), row(200)))
		if ack := bob.do(e.clearOp("Sheet1", 2, 3, 1, 30), e.clearOp("Other", 1, 3, 1, 30)); status(ack) != "applied,applied" {
			t.Fatalf("clear: %s", status(ack))
		}
		e.collectTo(limit)
		all := e.blankRows()
		if all < 180-limit {
			t.Fatalf("%d records on disk, want at least %d", all, 180-limit)
		}
		count := func(where string) int {
			e.in.mu.RLock()
			defer e.in.mu.RUnlock()
			var n int
			if err := e.in.scratch.blanks.db.QueryRow(`SELECT count(*) FROM blanks WHERE ` + where).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		tab, row2, colB := e.ids("B2")
		inRow, inCol := count(fmt.Sprintf("tab=%d AND row=%d", tab, row2)), count(fmt.Sprintf("tab=%d AND col=%d", tab, colB))
		if inRow == 0 || inCol == 0 {
			t.Fatalf("%d records of row 2 and %d of column B", inRow, inCol)
		}
		// Carol deletes row 2 and can bring it back: its records stay.
		carol := e.person("Carol")
		carol.do(e.deleteRowsOp("Sheet1", 2, 1))
		e.tick()
		if n := count(fmt.Sprintf("tab=%d AND row=%d", tab, row2)); n != inRow {
			t.Fatalf("%d of %d records of a row that an undo can bring back are left", n, inRow)
		}
		wantUndo(t, "undo of the deletion", undo(t, carol), undoAck{Applied: 1})
		// An agent deletes it, column B and the other tab: nothing brings
		// them back.
		carol.set("A1", "clears Carol's redo")
		other := e.tabID("Other")
		e.mustAgent(`{"ops":[{"op":"delete_rows","tab":"Sheet1","at":2,"count":1},{"op":"delete_cols","tab":"Sheet1","at":"B","count":1},{"op":"delete_tab","tab":"Other"}]}`)
		e.tick()
		for what, where := range map[string]string{"the deleted row": fmt.Sprintf("tab=%d AND row=%d", tab, row2),
			"the deleted column": fmt.Sprintf("tab=%d AND col=%d", tab, colB), "the deleted tab": fmt.Sprintf("tab=%d", other)} {
			if n := count(where); n != 0 {
				t.Errorf("%d records of %s are left", n, what)
			}
		}
		if n := e.blankRows(); n == 0 || n >= all-90 {
			t.Fatalf("%d of %d records are left", n, all)
		}
		// What is left of Bob's clear is still undone: the cells of the
		// rows and columns that stayed.
		u := undo(t, bob)
		if u.Applied != 2*29 || u.Reason != "changed" {
			t.Fatalf("Bob's undo = %+v, want the 58 cells that are still in the sheet", u)
		}
		e.want("A2", "100")
	})
}

// The file of collected blank records has a largest size. Past it a
// record is not kept, and the undo that needed its stamp skips the cell as
// lost: nothing is restored on a guess, and the rest of the undo applies.
func TestCollectedStampStoreAtItsBound(t *testing.T) {
	slow(t, "clears 60,000 cells")
	const rows, cols, limit = 600, 100, 1000
	const bound = 256 * blankPage
	e := start(t)
	e.in.mu.Lock()
	e.in.wb.collected.limit = limit
	e.in.undoPool().blanks.maxBytes = bound
	e.in.mu.Unlock()
	fillNumbers(e, "Sheet1", rows, cols)
	alice, bob := e.person("Alice"), e.person("Bob")
	bob.set("A700", "Bob's")
	if ack := alice.do(e.clearOp("Sheet1", 1, rows, 1, cols)); status(ack) != "applied" {
		t.Fatalf("clear: %s", status(ack))
	}
	var peak int64
	for i := 0; e.blanks() > limit; i++ {
		if i == 30 {
			t.Fatalf("%d blank records are left", e.blanks())
		}
		e.tick()
		peak = max(peak, e.blankBytes())
	}
	kept := e.blankRows()
	t.Logf("%d of %d records kept in %d bytes, bound %d", kept, rows*cols, peak, bound)
	if peak > bound || kept == 0 || kept > rows*cols/2 {
		t.Fatalf("the file took %d bytes, bound %d, and holds %d records", peak, bound, kept)
	}
	// A cell whose record could not be kept gets a format from Bob. The
	// undo must not take the new record's empty stamp for a change by Bob.
	e.in.mu.RLock()
	var lostAddr string
	tb := e.in.wb.tabs[0]
	for pos := 0; pos < rows && lostAddr == ""; pos++ {
		id := cellID{tb.id, RowID(tb.rows.ids[pos]), ColID(tb.cols.ids[0])}
		if tb.cell(id.row, id.col) != nil {
			continue
		}
		recs, err := e.in.scratch.blanks.read([]cellID{id}, e.in.holders())
		if err != nil {
			t.Fatal(err)
		}
		if recs[id] == nil {
			lostAddr = fmt.Sprintf("A%d", pos+1)
		}
	}
	e.in.mu.RUnlock()
	if lostAddr == "" {
		t.Fatal("no cell of column A lost its record")
	}
	bob.do(e.formatOp(lostAddr, `{"bold":true}`))
	u := undo(t, alice)
	left := e.blanksAndEmpty("Sheet1", rows, cols)
	t.Logf("undo = %+v; %d cells stayed empty", u, left)
	if u.Reason != "lost" || u.Skipped == 0 || u.Applied+u.Skipped != rows*cols || u.Applied < kept {
		t.Fatalf("undo = %+v, want every cell applied or skipped as lost, and at least the %d kept ones applied", u, kept)
	}
	if left != u.Skipped {
		t.Fatalf("%d cells are empty after the undo, %d were skipped", left, u.Skipped)
	}
	if e.shown(lostAddr) != "" || !e.bold(lostAddr) {
		t.Fatalf("%s shows %q, bold %v: a lost cell keeps what it holds now", lostAddr, e.shown(lostAddr), e.bold(lostAddr))
	}
	// Alice's other actions stay available, and so do Bob's.
	alice.set("B700", "next")
	wantUndo(t, "Alice's next undo", undo(t, alice), undoAck{Applied: 1})
	wantUndo(t, "Bob's undo of the format", undo(t, bob), undoAck{Applied: 1})
	wantUndo(t, "Bob's undo of his text", undo(t, bob), undoAck{Applied: 1})
}

// blanksAndEmpty counts the cells of the first rows and columns of a tab
// that show nothing.
func (e *env) blanksAndEmpty(tab string, rows, cols int) int {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t := e.in.wb.tab(tab)
	n := 0
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			if cell := t.cell(RowID(t.rows.ids[r]), ColID(t.cols.ids[c])); cell == nil || cell.blank() {
				n++
			}
		}
	}
	return n
}

func (p *person) noteBytes() int64 {
	p.e.in.mu.RLock()
	defer p.e.in.mu.RUnlock()
	return p.s.undo.noteBytes
}

// The stand-in notes of undo count in the undo memory of the session and
// of the sheet. Over ranges they take next to nothing; where they do not
// fit, the oldest action they stand for can no longer be undone, and
// nothing else is lost.
func TestUndoNotesStayInBudget(t *testing.T) {
	nine := [2]string{
		`{"bold":true,"italic":true,"underline":true,"strikethrough":true,"text_color":"blue-600","fill":"teal-200","align":"right","wrap":true,"number":{"kind":"percent","decimals":1}}`,
		`{"bold":null,"italic":null,"underline":null,"strikethrough":null,"text_color":"gray-100","fill":"red-100","align":"center","wrap":null,"number":{"kind":"number","decimals":1,"thousands":false}}`,
	}
	// Every session formats the same cells twice, in all nine properties,
	// and undoes the second change. Each undo leaves a note for each
	// property of each cell.
	t.Run("ranges, across sessions", func(t *testing.T) {
		slow(t, "formats every cell of a large sheet many times")
		// Under the race detector the sheet is smaller and the heap is
		// not measured; the counts of notes and the budgets are checked.
		// At full size, 500,000 cells, the heaviest step is an undo that
		// finds its stamps through notes. Its operations take about 0.6 s
		// of the 10 s an action has on the reference machine, so a host
		// many times slower or busier still passes (BenchmarkFormatRange).
		rows, cols := raced(sized(5000, 400), 80), raced(sized(100, 50), 20)
		cells := rows * cols
		e := start(t)
		fillNumbers(e, "Sheet1", rows, cols)
		people := make([]*person, 4)
		for i := range people {
			people[i] = e.person(fmt.Sprint("P", i))
		}
		before := int64(heapInUse())
		for i, p := range people {
			for k := range nine {
				if ack := p.do(e.formatRange("Sheet1", 1, rows, 1, cols, nine[k])); status(ack) != "applied" {
					t.Fatalf("format %d of session %d: %s", k, i, status(ack))
				}
			}
			wantUndo(t, "undo of the second format", undo(t, p), undoAck{Applied: 9 * cells})
			if n := p.stampLinks(); n != 9*cells {
				t.Fatalf("session %d holds %d notes, want %d", i, n, 9*cells)
			}
			// The notes of the session before are gone: this session wrote
			// over every stamp of theirs.
			if i > 0 {
				if n, held := people[i-1].stampLinks(), people[i-1].noteBytes(); n != 0 || held != 0 {
					t.Fatalf("session %d still holds %d notes in %d bytes after another writer replaced their stamps", i-1, n, held)
				}
			}
		}
		grown := int64(heapInUse()) - before
		last := people[len(people)-1]
		e.in.mu.RLock()
		counted, resident := e.in.scratch.notes.bytes, e.in.scratch.resident
		e.in.mu.RUnlock()
		t.Logf("%d notes of the last session take %d bytes; the heap grew by %d KiB over %d sessions", 9*cells, last.noteBytes(), grown>>10, len(people))
		if last.noteBytes() > undoSessionBytes || counted != last.noteBytes() || counted+resident > undoInstanceBytes {
			t.Fatalf("notes: %d bytes in the session, %d counted for the sheet, %d bytes of payloads", last.noteBytes(), counted, resident)
		}
		// The heap holds what the budgets say, and the style states of the
		// format changes; nothing grows with sessions times cells.
		if measures && grown > undoInstanceBytes+(16<<20) {
			t.Fatalf("the heap grew by %d MiB, the undo budget of the sheet is %d MiB", grown>>20, undoInstanceBytes>>20)
		}
		// Conflict protection: the first session's earlier format was
		// written over by the others, and its undo leaves their values.
		wantUndo(t, "undo of an overwritten format", undo(t, people[0]), undoAck{Skipped: 9 * cells, Reason: "changed"})
		// The retained action of the last session is undone through its notes.
		wantUndo(t, "undo of the first format", undo(t, last), undoAck{Applied: 9 * cells})
		wantUndo(t, "redo of the first format", redo(t, last), undoAck{Applied: 9 * cells})
		wantUndo(t, "redo of the second format", redo(t, last), undoAck{Applied: 9 * cells})
	})
	// Notes of cells that lie apart cannot be kept as ranges. Each session
	// writes every other row of its own columns twice and undoes the
	// second write.
	t.Run("scattered cells, at the bound", func(t *testing.T) {
		slow(t, "formats scattered cells of a large sheet")
		rows, cols := sized(20000, 12000), 6
		e := start(t)
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, rows))
		tab := e.tabID("Sheet1")
		e.in.mu.RLock()
		tb := e.in.wb.tabs[0]
		var runs []string
		for pos := 0; pos < rows; pos += 2 {
			runs = append(runs, fmt.Sprintf("[%d,1]", tb.rows.ids[pos]))
		}
		e.in.mu.RUnlock()
		rowIDs := "[" + strings.Join(runs, ",") + "]"
		cells := len(runs) * cols
		people := make([]*person, 4)
		for i := range people {
			people[i] = e.person(fmt.Sprint("P", i))
			people[i].set(fmt.Sprintf("Z%d", i+1), "older")
		}
		for i, p := range people {
			colIDs := e.colsOf("Sheet1", 1+i*cols, cols)
			for k := range nine {
				if ack := p.do(jsonf(`{"op":"format","tab":%d,"rows":%s,"cols":%s,"set":%s}`, tab, rowIDs, colIDs, nine[k])); status(ack) != "applied" {
					t.Fatalf("format %d of session %d: %s", k, i, status(ack))
				}
			}
		}
		// The cells and their formats are in the sheet. What the undos add
		// to the heap is undo state.
		before := int64(heapInUse())
		for i, p := range people {
			wantUndo(t, "undo of the second format", undo(t, p), undoAck{Applied: 9 * cells})
			if held := p.noteBytes(); held > undoSessionBytes {
				t.Fatalf("session %d holds %d bytes of notes, its budget is %d", i, held, undoSessionBytes)
			}
		}
		grown := int64(heapInUse()) - before
		e.in.mu.RLock()
		counted := e.in.scratch.notes.bytes
		e.in.mu.RUnlock()
		t.Logf("%d notes for each of %d sessions: %d bytes counted, the heap grew by %d KiB", 9*cells, len(people), counted, grown>>10)
		// 9 notes of 16 bytes for each cell do not fit a session's 4 MiB.
		if int64(9*cells)*noteRunBytes <= undoSessionBytes {
			t.Fatalf("%d notes fit the budget: the case does not reach the bound", 9*cells)
		}
		if measures && grown > int64(len(people))*undoSessionBytes+(8<<20) {
			t.Fatalf("the heap grew by %d MiB over the undos of %d sessions with %d MiB each", grown>>20, len(people), undoSessionBytes>>20)
		}
		for i, p := range people {
			// The first format is the action the notes stood for. It no
			// longer fits the limits: its undo says so and changes nothing.
			wantUndo(t, "undo of the first format", undo(t, p), undoAck{Skipped: 1, Reason: "lost"})
			if !e.bold(fmt.Sprintf("%c1", 'A'+i*cols)) {
				t.Fatalf("session %d: a lost undo changed the sheet", i)
			}
			// The action before it is still there, and the redo too.
			wantUndo(t, "undo of the older action", undo(t, p), undoAck{Applied: 1})
			wantUndo(t, "redo of the older action", redo(t, p), undoAck{Applied: 1})
		}
	})
	// The notes of all sessions share the sheet's budget: the session whose
	// undo passes it gives up its own oldest action, and the others keep
	// theirs.
	t.Run("the sheet's budget", func(t *testing.T) {
		const rows = 4000
		e := start(t)
		e.in.mu.Lock()
		p := e.in.undoPool()
		p.instanceMax = 48 << 10
		e.in.mu.Unlock()
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, rows))
		tab := e.tabID("Sheet1")
		e.in.mu.RLock()
		var runs []string
		for pos := 0; pos < rows; pos += 2 {
			runs = append(runs, fmt.Sprintf("[%d,1]", e.in.wb.tabs[0].rows.ids[pos]))
		}
		e.in.mu.RUnlock()
		alice, bob := e.person("Alice"), e.person("Bob")
		for i, p := range []*person{alice, bob} {
			op := func(set string) string {
				return jsonf(`{"op":"format","tab":%d,"rows":[%s],"cols":%s,"set":%s}`, tab, strings.Join(runs, ","), e.colsOf("Sheet1", 1+i, 1), set)
			}
			p.do(op(`{"bold":true}`))
			p.do(op(`{"bold":null}`))
			wantUndo(t, "undo of the reset", undo(t, p), undoAck{Applied: rows / 2})
		}
		// 2,000 notes of 16 bytes: Alice's fit 48 KiB, Bob's on top do not.
		e.in.mu.RLock()
		counted := e.in.scratch.notes.bytes
		e.in.mu.RUnlock()
		if alice.noteBytes() == 0 || bob.noteBytes() != 0 || counted != alice.noteBytes() || counted > 48<<10 {
			t.Fatalf("notes: Alice %d bytes, Bob %d, the sheet %d", alice.noteBytes(), bob.noteBytes(), counted)
		}
		wantUndo(t, "Bob's undo of the format", undo(t, bob), undoAck{Skipped: 1, Reason: "lost"})
		wantUndo(t, "Alice's undo of the format", undo(t, alice), undoAck{Applied: rows / 2})
		if e.bold("A1") || !e.bold("B1") {
			t.Fatalf("bold: A1 %v, B1 %v", e.bold("A1"), e.bold("B1"))
		}
	})
	// An A to B to A history still guards another writer's B when the
	// notes are kept as ranges.
	t.Run("another writer's value under a note", func(t *testing.T) {
		e := start(t)
		fillNumbers(e, "Sheet1", 4, 4)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.formatRange("Sheet1", 1, 4, 1, 4, `{"bold":true}`))
		bob.do(e.formatRange("Sheet1", 2, 1, 2, 1, `{"bold":null}`))
		alice.do(e.formatRange("Sheet1", 1, 4, 1, 4, `{"bold":null}`))
		wantUndo(t, "undo of the reset", undo(t, alice), undoAck{Applied: 16})
		// B2 holds Bob's value again, under Alice's stamp, and no note.
		if n := alice.stampLinks(); n != 15 {
			t.Fatalf("%d notes, want 15", n)
		}
		wantUndo(t, "undo of the format", undo(t, alice), undoAck{Applied: 15, Skipped: 1, Reason: "changed"})
		if e.bold("B2") || e.bold("A1") {
			t.Fatalf("bold: B2 %v, A1 %v", e.bold("B2"), e.bold("A1"))
		}
	})
	// A note whose group another writer stamps is taken out when that
	// write commits, and the bytes of its set when the tick has rebuilt it.
	t.Run("notes another writer replaced", func(t *testing.T) {
		e := start(t)
		const rows = 64
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, 2*rows))
		alice, bob := e.person("Alice"), e.person("Bob")
		tab := e.tabID("Sheet1")
		var runs []string
		e.in.mu.RLock()
		for pos := 0; pos < 2*rows; pos += 2 {
			runs = append(runs, fmt.Sprintf("[%d,1]", e.in.wb.tabs[0].rows.ids[pos]))
		}
		e.in.mu.RUnlock()
		op := func(set string) string {
			return jsonf(`{"op":"format","tab":%d,"rows":[%s],"cols":%s,"set":%s}`, tab, strings.Join(runs, ","), e.colsOf("Sheet1", 1, 1), set)
		}
		alice.do(op(`{"bold":true}`))
		alice.do(op(`{"bold":null}`))
		wantUndo(t, "undo", undo(t, alice), undoAck{Applied: rows})
		full := alice.noteBytes()
		if n := alice.stampLinks(); n != rows || full == 0 {
			t.Fatalf("%d notes in %d bytes", n, full)
		}
		for i := 0; i < rows/2; i++ {
			bob.do(e.formatOp(fmt.Sprintf("A%d", 1+2*i), `{"bold":null}`))
		}
		if n := alice.stampLinks(); n != rows/2 {
			t.Fatalf("%d notes after Bob wrote over %d of them, want %d", n, rows/2, rows/2)
		}
		e.tick()
		if held := alice.noteBytes(); held >= full || alice.stampLinks() != rows/2 {
			t.Fatalf("the notes take %d bytes after the tick, %d before; %d notes", held, full, alice.stampLinks())
		}
		u := undo(t, alice)
		if u.Applied != rows/2 || u.Skipped != rows/2 || u.Reason != "changed" {
			t.Fatalf("undo of the format = %+v", u)
		}
		if n, held := alice.stampLinks(), alice.noteBytes(); n != 0 || held != 0 {
			t.Fatalf("%d notes in %d bytes after their entry left", n, held)
		}
	})
}

// logSink collects the lines a test instance logs.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) lines(sub string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range strings.Split(l.buf.String(), "\n") {
		if strings.Contains(line, sub) {
			out = append(out, line)
		}
	}
	return out
}

// An error of the undo scratch storage refuses the action as unavailable,
// which tells the person nothing about the host. The host's log names the
// storage error and the sheet, once for a burst of attempts.
func TestUndoScratchErrorsAreLogged(t *testing.T) {
	const line = "sheet undo scratch storage failed"
	setup := func(t *testing.T, fail string) (*env, *logSink, *person) {
		sink := &logSink{}
		e := start(t, func(o *Options) {
			o.Slug = "budget-2026"
			o.Log = slog.New(slog.NewTextHandler(sink, nil))
		})
		e.in.mu.Lock()
		p := e.in.undoPool()
		p.entryMax = -1
		p.fault = func(op string) error {
			if op == fail {
				return errors.New("no space left on device")
			}
			return nil
		}
		e.in.mu.Unlock()
		return e, sink, e.person("Alice")
	}
	check := func(t *testing.T, sink *logSink, n int) {
		t.Helper()
		got := sink.lines(line)
		if len(got) != n {
			t.Fatalf("%d log lines about the scratch storage, want %d: %q", len(got), n, got)
		}
		for _, l := range got {
			if !strings.Contains(l, "slug=budget-2026") || !strings.Contains(l, "no space left on device") || !strings.Contains(l, "level=WARN") {
				t.Fatalf("the log line names no sheet or no cause: %s", l)
			}
		}
	}
	t.Run("a payload cannot be written", func(t *testing.T) {
		e, sink, alice := setup(t, "write")
		for i := 0; i < 5; i++ {
			seq := e.seq()
			_, err := alice.try(seq, e.setOp("A1", "text"))
			if werr := refusal(err); err == nil || werr.Code != "unavailable" || werr.Reason != "undo_scratch" {
				t.Fatalf("attempt %d = %v, want an unavailable refusal", i, err)
			}
		}
		// Five attempts in a row are one line.
		check(t, sink, 1)
		// After the pause the next failure is logged again, with the count
		// of those in between.
		e.in.mu.Lock()
		e.in.scratch.said.last = e.in.scratch.said.last.Add(-2 * scratchLogEvery)
		e.in.mu.Unlock()
		alice.try(e.seq(), e.setOp("A1", "text"))
		check(t, sink, 2)
		if last := sink.lines(line)[1]; !strings.Contains(last, "suppressed=4") {
			t.Fatalf("the second line does not count the attempts in between: %s", last)
		}
	})
	t.Run("a payload cannot be read", func(t *testing.T) {
		e, sink, alice := setup(t, "read")
		alice.set("A1", "text")
		_, err := alice.try(e.seq(), `{"op":"undo"}`)
		if werr := refusal(err); err == nil || werr.Reason != "undo_scratch" {
			t.Fatalf("undo = %v, want an unavailable refusal", err)
		}
		check(t, sink, 1)
	})
	// Every structural edit tries the edit log again while a payload waits
	// for edits it could not write. The attempts of a burst are one line.
	t.Run("the edit log cannot be written", func(t *testing.T) {
		e, sink, alice := setup(t, "edits-write")
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1],[2],[3,"=A1+A5"],[4],[5]]}]}`)
		alice.do(e.deleteRowsOp("Sheet1", 3, 1))
		e.in.mu.Lock()
		// A flush starts at 512 bytes of edits, and the payload expires
		// when eight times that wait. An insert counts about 130 bytes.
		e.in.undoPool().edits.segment = 512
		e.in.mu.Unlock()
		bob := e.person("Bob")
		for i := 0; i < 20; i++ {
			bob.do(e.insertRowsOp("Sheet1", 1, 1))
		}
		if alice.lost(0) {
			t.Fatal("the payload expired: the case needs one that still waits")
		}
		check(t, sink, 1)
		if got := sink.lines(line)[0]; !strings.Contains(got, `op="edit log"`) {
			t.Fatalf("the line does not say what failed: %s", got)
		}
		e.in.mu.Lock()
		e.in.scratch.said.last = e.in.scratch.said.last.Add(-2 * scratchLogEvery)
		e.in.mu.Unlock()
		bob.do(e.insertRowsOp("Sheet1", 1, 1))
		check(t, sink, 2)
		if last := sink.lines(line)[1]; strings.Contains(last, "suppressed=0") {
			t.Fatalf("the second line counts no attempt in between: %s", last)
		}
	})
	t.Run("collected blank records cannot be written", func(t *testing.T) {
		e, sink, alice := setup(t, "blank-write")
		e.in.mu.Lock()
		e.in.wb.collected.limit = 4
		e.in.mu.Unlock()
		fillNumbers(e, "Sheet1", 4, 4)
		alice.do(e.clearOp("Sheet1", 1, 4, 1, 4))
		e.tick()
		if e.blanks() != 16 {
			t.Fatalf("%d blank records are left: the tick collected without the store", e.blanks())
		}
		check(t, sink, 1)
		if got := sink.lines(line)[0]; !strings.Contains(got, `op="blank records"`) {
			t.Fatalf("the line does not say what failed: %s", got)
		}
	})
	t.Run("collected blank records cannot be read", func(t *testing.T) {
		e, sink, alice := setup(t, "blank-read")
		e.in.mu.Lock()
		e.in.wb.collected.limit = 4
		e.in.mu.Unlock()
		fillNumbers(e, "Sheet1", 4, 4)
		alice.do(e.clearOp("Sheet1", 1, 4, 1, 4))
		e.collectTo(8)
		_, err := alice.try(e.seq(), `{"op":"undo"}`)
		if werr := refusal(err); err == nil || werr.Reason != "undo_scratch" {
			t.Fatalf("undo = %v, want an unavailable refusal", err)
		}
		check(t, sink, 1)
	})
}

// The store's file is where the scratch directory of the instance is, and
// nowhere else.
func TestCollectedStampStoreFiles(t *testing.T) {
	e := start(t)
	e.in.mu.Lock()
	e.in.wb.collected.limit = 4
	e.in.mu.Unlock()
	fillNumbers(e, "Sheet1", 4, 4)
	alice := e.person("Alice")
	alice.do(e.clearOp("Sheet1", 1, 4, 1, 4))
	e.collectTo(4)
	e.in.mu.RLock()
	dir := e.in.scratch.dir
	e.in.mu.RUnlock()
	info, err := os.Stat(filepath.Join(dir, blankFile))
	if err != nil || info.Size() == 0 || info.Size()%blankPage != 0 {
		t.Fatalf("the store's file: %v, %v", info, err)
	}
	if journal, err := os.Stat(filepath.Join(dir, blankFile+"-journal")); err == nil && journal.Size() != 0 {
		t.Fatalf("the journal holds %d bytes between transactions", journal.Size())
	}
}
