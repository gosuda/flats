package sheet

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// fillNumbers writes rows x cols numbers into a tab from A1, in calls
// under the cell limit of one call. The cell in row r and column c holds
// r*1000+c, counted from 1.
func fillNumbers(e *env, tab string, rows, cols int) {
	e.t.Helper()
	per := maxCallCells / cols
	for from := 0; from < rows; from += per {
		var sb strings.Builder
		for r := from; r < min(rows, from+per); r++ {
			if r > from {
				sb.WriteByte(',')
			}
			sb.WriteByte('[')
			for c := 0; c < cols; c++ {
				if c > 0 {
					sb.WriteByte(',')
				}
				fmt.Fprint(&sb, (r+1)*1000+c+1)
			}
			sb.WriteByte(']')
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":%q,"at":"A%d","rows":[%s]}]}`, tab, from+1, sb.String()))
	}
}

func (e *env) clearOp(tab string, row, rows, col, cols int) string {
	return fmt.Sprintf(`{"op":"clear","tab":%d,"rows":%s,"cols":%s}`, e.tabID(tab), e.rowsOf(tab, row, rows), e.colsOf(tab, col, cols))
}

func (e *env) blanks() int {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	return e.in.wb.count.blanks
}

func (e *env) filled(tab string) int {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	n := 0
	for _, rd := range e.in.wb.tab(tab).data {
		for _, s := range rd.cells {
			if !s.c.blank() {
				n++
			}
		}
	}
	return n
}

// The collection of blank records does not take a retained undo away: a
// clear whose blank records were collected is undone in full.
func TestUndoAfterBlankCollection(t *testing.T) {
	// collect runs the maintenance tick. The instance's own ticker may have
	// run it already, so the check is only that the blank records made by
	// the clear did not all stay.
	collect := func(t *testing.T, e *env, made int) {
		t.Helper()
		e.tick()
		if left := e.blanks(); left > made/2 {
			t.Fatalf("%d of %d blank records are left after the tick", left, made)
		}
	}
	// The undo reads the stamps of 500,000 cells from the store of
	// collected records, a range of the store at a time. Its operations
	// take about a second of the 10 s an action has on a loaded reference
	// machine (BenchmarkClearRange). The cases run side by side and have an
	// hour each (unhurried), so the benchmark is what measures that time.
	t.Run("a clear of 500,000 cells", func(t *testing.T) {
		heavy(t, "writes 500,000 cells")
		t.Parallel()
		e := start(t, unhurried)
		const rows, cols = 5000, 100
		fillNumbers(e, "Sheet1", rows, cols)
		alice := e.person("Alice")
		if ack := alice.do(e.clearOp("Sheet1", 1, rows, 1, cols)); ack.Results[0].Applied != rows*cols {
			t.Fatalf("clear = %+v", ack.Results[0])
		}
		collect(t, e, rows*cols)
		if u := undo(t, alice); u.Applied != rows*cols || u.Skipped != 0 {
			t.Fatalf("undo = %+v", u)
		}
		if n := e.filled("Sheet1"); n != rows*cols {
			t.Fatalf("%d cells are filled after the undo, want %d", n, rows*cols)
		}
		e.want("A1", "1001")
		e.want("CV5000", "5000100")
		e.restart()
		if n := e.filled("Sheet1"); n != rows*cols {
			t.Fatalf("%d cells are filled after a restart, want %d", n, rows*cols)
		}
	})
	const rows, cols = 1100, 100
	// Each case fills its own sheet with more cells than the blank records a
	// sheet keeps, so the cases run side by side.
	setup := func(t *testing.T) (*env, *person, *person) {
		heavy(t, "writes 110,000 cells")
		t.Parallel()
		e := start(t, unhurried)
		fillNumbers(e, "Sheet1", rows, cols)
		return e, e.person("Alice"), e.person("Bob")
	}
	t.Run("redo and a second undo", func(t *testing.T) {
		e, alice, _ := setup(t)
		alice.do(e.clearOp("Sheet1", 1, rows, 1, cols))
		collect(t, e, rows*cols)
		if u := undo(t, alice); u.Applied != rows*cols || u.Skipped != 0 {
			t.Fatalf("undo = %+v", u)
		}
		if u := alice.do(`{"op":"redo"}`).Undo; u == nil || u.Applied != rows*cols || u.Skipped != 0 {
			t.Fatalf("redo = %+v", u)
		}
		collect(t, e, rows*cols)
		if u := undo(t, alice); u.Applied != rows*cols || u.Skipped != 0 || e.filled("Sheet1") != rows*cols {
			t.Fatalf("second undo = %+v, %d cells filled", u, e.filled("Sheet1"))
		}
		e.want("B2", "2002")
	})
	// Bob's later clear of one of Alice's cells is collected with hers. Her
	// undo restores every cell but that one.
	t.Run("another writer's collected clear", func(t *testing.T) {
		e, alice, bob := setup(t)
		half := rows / 2
		alice.do(e.clearOp("Sheet1", 1, half, 1, cols))
		bob.set("A1", "bob")
		bob.do(e.clearOp("Sheet1", 1, 1, 1, 1))
		bob.do(e.clearOp("Sheet1", half+1, half, 1, cols))
		collect(t, e, rows*cols)
		u := undo(t, alice)
		if e.shown("A1") != "" {
			t.Fatalf("the undo wrote over Bob's clear: A1 shows %q", e.shown("A1"))
		}
		if u.Applied != half*cols-1 || u.Skipped != 1 {
			t.Fatalf("undo = %+v", u)
		}
		e.want("B1", "1002")
	})
	// A cell written again after its blank record was collected keeps its
	// place in the person's history.
	t.Run("the person's own clears", func(t *testing.T) {
		e, alice, _ := setup(t)
		half := rows / 2
		alice.do(e.clearOp("Sheet1", 1, half, 1, cols))
		alice.do(e.clearOp("Sheet1", half+1, half, 1, cols))
		collect(t, e, rows*cols)
		alice.set("A1", "again")
		for i, want := range []int{1, half * cols, half * cols} {
			if u := undo(t, alice); u.Applied != want || u.Skipped != 0 {
				t.Fatalf("undo %d = %+v, want %d applied", i+1, u, want)
			}
		}
		if n := e.filled("Sheet1"); n != rows*cols {
			t.Fatalf("%d cells are filled, want %d", n, rows*cols)
		}
	})
	t.Run("a format reset on blank cells", func(t *testing.T) {
		heavy(t, "formats 110,000 cells")
		t.Parallel()
		e := start(t, unhurried)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"CV1101","rows":[["end"]]}]}`)
		alice := e.person("Alice")
		tab := e.tabID("Sheet1")
		rowIDs, colIDs := e.rowsOf("Sheet1", 1, rows), e.colsOf("Sheet1", 1, cols)
		alice.do(jsonf(`{"op":"format","tab":%d,"rows":%s,"cols":%s,"set":{"bold":true}}`, tab, rowIDs, colIDs))
		alice.do(jsonf(`{"op":"format","tab":%d,"rows":%s,"cols":%s,"set":{"bold":null}}`, tab, rowIDs, colIDs))
		collect(t, e, rows*cols)
		if u := undo(t, alice); u.Applied != rows*cols || u.Skipped != 0 {
			t.Fatalf("undo of the reset = %+v", u)
		}
		bold := func() int {
			e.in.mu.RLock()
			defer e.in.mu.RUnlock()
			n := 0
			for _, rd := range e.in.wb.tabs[0].data {
				for _, s := range rd.cells {
					if e.in.wb.styles.format(s.c.style).Bold {
						n++
					}
				}
			}
			return n
		}
		if n := bold(); n != rows*cols {
			t.Fatalf("%d cells are bold after the undo, want %d", n, rows*cols)
		}
		if u := undo(t, alice); u.Applied != rows*cols || u.Skipped != 0 || bold() != 0 {
			t.Fatalf("undo of the format = %+v, %d cells bold", u, bold())
		}
		alice.do(`{"op":"redo"}`)
		e.restart()
		if n := bold(); n != rows*cols {
			t.Fatalf("%d cells are bold after a restart, want %d", n, rows*cols)
		}
	})
}

// blankRows returns the number of collected blank records the instance
// keeps on disk.
func (e *env) blankRows() int {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	b := &e.in.scratch.blanks
	if b.db == nil {
		return 0
	}
	var n int
	if err := b.db.QueryRow(`SELECT count(*) FROM blanks`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) hasRecord(addr string) bool {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t, row, col := e.at(addr)
	return t.cell(row, col) != nil
}

// The stamps of a collected blank record are kept exactly, on disk, for as
// long as an undo entry can ask for them.
func TestUndoKeepsCollectedStamps(t *testing.T) {
	const rows, cols, limit = 40, 50, 300
	// setup fills a sheet whose blank records are collected above a small
	// number, and lets Alice clear A1 and then every other cell.
	// between runs after Alice's first clear, as Bob.
	spill := false
	var between func(e *env, bob *person)
	setup := func(t *testing.T) (*env, *person, *person) {
		e := start(t)
		e.in.mu.Lock()
		e.in.wb.collected.limit = limit
		if spill {
			e.in.undoPool().entryMax = -1
		}
		e.in.mu.Unlock()
		fillNumbers(e, "Sheet1", rows, cols)
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.do(e.clearOp("Sheet1", 1, 1, 1, 1))
		if between != nil {
			between(e, bob)
		}
		ack := alice.do(e.clearOp("Sheet1", 1, 1, 2, cols-1), e.clearOp("Sheet1", 2, rows-1, 1, cols))
		if status(ack) != "applied,applied" {
			t.Fatalf("clear: %s", status(ack))
		}
		return e, alice, bob
	}
	// collect ticks until no more than the limit of blank records is left:
	// each tick takes the older half.
	collect := func(t *testing.T, e *env) {
		t.Helper()
		for i := 0; e.blanks() > limit; i++ {
			if i == 20 {
				t.Fatalf("%d blank records are left after %d ticks", e.blanks(), i)
			}
			e.tick()
		}
	}
	const rest = rows*cols - 1
	for _, name := range []string{"in memory", "in scratch files"} {
		spill = name != "in memory"
		t.Run(name+", a cell written again", func(t *testing.T) {
			e, alice, _ := setup(t)
			collect(t, e)
			if e.hasRecord("A1") || e.blankRows() < rest-limit {
				t.Fatalf("A1 has a record: %v; %d records on disk", e.hasRecord("A1"), e.blankRows())
			}
			alice.set("A1", "again")
			wantUndo(t, "undo of the write", undo(t, alice), undoAck{Applied: 1})
			wantUndo(t, "undo of the large clear", undo(t, alice), undoAck{Applied: rest})
			wantUndo(t, "undo of the first clear", undo(t, alice), undoAck{Applied: 1})
			e.want("A1", "1001")
			if n := e.filled("Sheet1"); n != rows*cols {
				t.Fatalf("%d cells are filled, want %d", n, rows*cols)
			}
			// Redo takes the three back, through another collection.
			for i, want := range []int{1, rest, 1} {
				wantUndo(t, fmt.Sprintf("redo %d", i+1), redo(t, alice), undoAck{Applied: want})
			}
			collect(t, e)
			e.want("A1", "again")
			wantUndo(t, "second undo of the write", undo(t, alice), undoAck{Applied: 1})
			wantUndo(t, "second undo of the large clear", undo(t, alice), undoAck{Applied: rest})
			wantUndo(t, "second undo of the first clear", undo(t, alice), undoAck{Applied: 1})
			e.want("A1", "1001")
			e.restart()
			if n := e.filled("Sheet1"); n != rows*cols {
				t.Fatalf("%d cells are filled after a restart, want %d", n, rows*cols)
			}
		})
		t.Run(name+", a cell another writer cleared in between", func(t *testing.T) {
			between = func(e *env, bob *person) {
				bob.set("A1", "bob")
				bob.do(e.clearOp("Sheet1", 1, 1, 1, 1))
			}
			e, alice, _ := setup(t)
			between = nil
			collect(t, e)
			if e.hasRecord("A1") {
				t.Fatal("A1 has a record")
			}
			alice.set("A1", "again")
			wantUndo(t, "undo of the write", undo(t, alice), undoAck{Applied: 1})
			wantUndo(t, "undo of the large clear", undo(t, alice), undoAck{Applied: rest})
			// The blank under Alice's write was Bob's.
			wantUndo(t, "undo of the first clear", undo(t, alice), undoAck{Skipped: 1, Reason: "changed"})
			e.want("A1", "")
			e.want("B1", "1002")
		})
		t.Run(name+", a cell another writer cleared, undone without a write", func(t *testing.T) {
			between = func(e *env, bob *person) {
				bob.set("A1", "bob")
				bob.do(e.clearOp("Sheet1", 1, 1, 1, 1))
			}
			e, alice, _ := setup(t)
			between = nil
			collect(t, e)
			if e.hasRecord("A1") {
				t.Fatal("A1 has a record")
			}
			wantUndo(t, "undo of the large clear", undo(t, alice), undoAck{Applied: rest})
			wantUndo(t, "undo of the first clear", undo(t, alice), undoAck{Skipped: 1, Reason: "changed"})
			e.want("A1", "")
		})
	}
	spill = false
	t.Run("records nobody can undo are not kept", func(t *testing.T) {
		e := start(t)
		e.in.mu.Lock()
		e.in.wb.collected.limit = limit
		e.in.mu.Unlock()
		fillNumbers(e, "Sheet1", rows, cols)
		e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:AX40"]}]}`)
		collect(t, e)
		if files, _ := os.ReadDir(e.in.scratch.dir); e.in.scratch.dir != "" && len(files) != 0 {
			t.Fatalf("%d scratch files for blank records of an agent", len(files))
		}
	})
	t.Run("records leave with the entries", func(t *testing.T) {
		e, alice, _ := setup(t)
		collect(t, e)
		if e.blankRows() == 0 {
			t.Fatal("no record on disk")
		}
		// The large clear leaves the stack: its records go, and those of
		// the first clear stay.
		e.in.mu.Lock()
		u := &alice.s.undo
		u.undo[1].release()
		u.undo = u.undo[:1]
		e.in.mu.Unlock()
		e.tick()
		if n := e.blankRows(); n == 0 {
			t.Fatalf("%d records on disk", n)
		}
		e.in.mu.Lock()
		alice.s.undo.drop()
		e.in.mu.Unlock()
		e.tick()
		if n := e.blankRows(); n != 0 {
			t.Fatalf("%d records on disk with no undo entry left", n)
		}
	})
	fail := func(e *env, what string, failing *bool) {
		e.in.mu.Lock()
		e.in.undoPool().fault = func(op string) error {
			if *failing && op == what {
				return errors.New("input/output error")
			}
			return nil
		}
		e.in.mu.Unlock()
	}
	t.Run("the records cannot be written", func(t *testing.T) {
		e, alice, _ := setup(t)
		failing := true
		fail(e, "blank-write", &failing)
		before := e.blanks()
		e.tick()
		if e.blanks() != before || e.blankRows() != 0 {
			t.Fatalf("%d of %d blank records are left, %d on disk: the tick collected without keeping the stamps", e.blanks(), before, e.blankRows())
		}
		failing = false
		collect(t, e)
		alice.set("A1", "again")
		for i, want := range []int{1, rest, 1} {
			wantUndo(t, fmt.Sprintf("undo %d", i+1), undo(t, alice), undoAck{Applied: want})
		}
	})
	t.Run("the records cannot be read", func(t *testing.T) {
		e, alice, _ := setup(t)
		collect(t, e)
		failing := true
		fail(e, "blank-read", &failing)
		// A write over a collected cell needs the stamp under it, and an
		// undo the stamps of the cells it restores. Both are refused and
		// can be sent again.
		seq := e.seq()
		for _, op := range []string{e.setOp("Sheet1!A1", "again"), `{"op":"undo"}`} {
			_, err := alice.try(seq, op)
			werr := refusal(err)
			if err == nil || werr.Code != "unavailable" || werr.Reason != "undo_scratch" || !werr.Retry {
				t.Fatalf("%s = %v, want an unavailable refusal that can be retried", op, err)
			}
		}
		if e.seq() != seq || e.hasRecord("A1") {
			t.Fatalf("a refused action changed the sheet: sequence %d", e.seq())
		}
		failing = false
		alice.set("A1", "again")
		for i, want := range []int{1, rest, 1} {
			wantUndo(t, fmt.Sprintf("undo %d", i+1), undo(t, alice), undoAck{Applied: want})
		}
		e.want("A1", "1001")
	})
	t.Run("the records are gone", func(t *testing.T) {
		e, alice, _ := setup(t)
		collect(t, e)
		removeBlankFileOnRead(e)
		// Nothing is known about the cells without a record: they are
		// skipped, and the cells that kept theirs are restored.
		u := undo(t, alice)
		if u.Skipped < rest-limit || u.Applied+u.Skipped != rest || u.Reason != "lost" {
			t.Fatalf("undo of the large clear = %+v", u)
		}
		wantUndo(t, "undo of the first clear", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
	})
}

// removeBlankFileOnRead closes SQLite before unlinking, as Windows requires.
// The failed read still finds the missing file and gives up the store itself.
func removeBlankFileOnRead(e *env) {
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	p := e.in.undoPool()
	p.fault = func(op string) error {
		if op != "blank-read" {
			return nil
		}
		p.fault = nil
		b := &p.blanks
		if err := b.db.Close(); err != nil {
			e.t.Errorf("close the scratch database before removal: %v", err)
			return err
		}
		if err := os.Remove(b.path); err != nil {
			e.t.Errorf("remove the scratch database: %v", err)
			return err
		}
		return errors.New("input/output error")
	}
}

// A read of the collected blank records scans a range of the store where
// that is cheaper than looking each cell up, and looks cells up where the
// rows of other cells lie between them. Either way it finds what a lookup
// of each cell finds, the row of the first entry that still holds it.
func TestBlankStoreReadsAsCellByCell(t *testing.T) {
	const rows, cols = 120, 30
	e := start(t)
	fillNumbers(e, "Sheet1", rows, cols)
	alice, bob := e.person("Alice"), e.person("Bob")
	// The tick collects every blank record.
	e.in.mu.Lock()
	e.in.wb.collected.limit = 1
	e.in.mu.Unlock()
	// Two entries hold the stamps of the rows Bob clears again after
	// Alice's undo, so the store has two rows for some cells.
	alice.do(e.clearOp("Sheet1", 1, rows, 1, cols))
	e.tick()
	undo(t, alice)
	bob.do(e.clearOp("Sheet1", 1, rows/2, 1, cols))
	e.tick()
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	b, tab := e.in.wb.collected.store, e.in.wb.tabs[0]
	if b == nil || b.rows < rows*cols/4 {
		t.Fatalf("the store holds %v rows", b)
	}
	h := e.in.holders()
	// byCell is the plain read: one lookup for each cell.
	byCell := func(ids []cellID) map[cellID]collectedBlank {
		out := map[cellID]collectedBlank{}
		for _, id := range ids {
			r, err := b.db.Query(`SELECT holder,seq,text_seq,by,stamps FROM blanks WHERE tab=? AND row=? AND col=? ORDER BY holder`, int64(id.tab), int64(id.row), int64(id.col))
			if err != nil {
				t.Fatal(err)
			}
			for r.Next() {
				var holder, seq, textSeq, by int64
				var stamps []byte
				if err := r.Scan(&holder, &seq, &textSeq, &by, &stamps); err != nil {
					t.Fatal(err)
				}
				if _, ok := out[id]; ok || h.entries[uint64(holder)] == nil {
					continue
				}
				rec := collectedBlank{seq: uint64(seq), textSeq: uint64(textSeq), by: uint32(by)}
				rec.stamps, _ = decodeBlankStamps(stamps)
				out[id] = rec
			}
			r.Close()
		}
		return out
	}
	// The cells the store has a row for, in key order.
	var stored []cellID
	r, err := b.db.Query(`SELECT DISTINCT tab,row,col FROM blanks ORDER BY tab,row,col`)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var tab, row, col int64
		if err := r.Scan(&tab, &row, &col); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, cellID{TabID(tab), RowID(row), ColID(col)})
	}
	r.Close()
	if len(stored) < 500 {
		t.Fatalf("the store has rows for %d cells", len(stored))
	}
	rng := rand.New(rand.NewPCG(5, 9))
	// Batches whose range the scan reads whole, and batches where it stops
	// and looks the rest up.
	var ranged, lookedUp int
	for round := 0; round < raced(60, 24); round++ {
		// Every cell the store has, every k-th of them, or cells at random,
		// with cells the store does not have and repeats among them.
		var ids []cellID
		k := 1 + round%12
		for i, id := range stored {
			if round%3 == 2 && rng.IntN(7) == 0 || round%3 != 2 && i%k == 0 {
				ids = append(ids, id)
			}
		}
		for i := 0; i < 50; i++ {
			ids = append(ids, cellID{tab.id, RowID(tab.rows.ids[rng.IntN(rows)]), ColID(tab.cols.ids[rng.IntN(cols)])})
		}
		ids = append(ids, cellID{tab.id, 1 << 40, 1}, ids[0])
		rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
		got, err := b.read(ids, h)
		if err != nil {
			t.Fatal(err)
		}
		want := byCell(ids)
		if len(got) != len(want) {
			t.Fatalf("round %d: %d records, want %d", round, len(got), len(want))
		}
		for id, w := range want {
			g := got[id]
			if g == nil || g.seq != w.seq || g.textSeq != w.textSeq || g.by != w.by || !reflect.DeepEqual(g.stamps, w.stamps) {
				t.Fatalf("round %d, %+v: %+v, want %+v", round, id, g, w)
			}
		}
		// Whether a batch of the read could scan its range: the rows the
		// store holds from its first cell to its last.
		var asked []cellID
		for _, id := range ids {
			if b.filter.may(id) {
				asked = append(asked, id)
			}
		}
		sorted := sortedCells(asked)
		for len(sorted) > 0 {
			batch := sorted[:min(len(sorted), blankBatch)]
			sorted = sorted[len(batch):]
			lo, hi := batch[0], batch[len(batch)-1]
			var n int
			if err := b.db.QueryRow(`SELECT count(*) FROM blanks WHERE (tab,row,col)>=(?,?,?) AND (tab,row,col)<=(?,?,?)`,
				int64(lo.tab), int64(lo.row), int64(lo.col), int64(hi.tab), int64(hi.row), int64(hi.col)).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n < blankScanRows*len(batch)+blankScanExtra {
				ranged++
			} else {
				lookedUp++
			}
		}
	}
	if ranged == 0 || lookedUp == 0 {
		t.Fatalf("%d batches had room to scan their range and %d did not", ranged, lookedUp)
	}
}

// A blank cell can hold the stamps of twenty entries: for its content and
// for each of its nine format properties, the entry of an undo and the
// entry the undo's stamp stands in for. A record then takes twenty rows in
// the file of collected blank records. When the file fills while such
// records are stored, the records it has no room for are collected without
// their rows and their holders skip the cells as lost. That is no error of
// the file. Every tick after it goes on as before, the records stored
// earlier stay, and the entries that lost nothing keep all they had.
func TestCollectedStampStoreFillsWithManyHolders(t *testing.T) {
	const rows, cols, limit = 100, 40, 100
	// Carol clears A1:J40. The twenty holders share K1:AD30, and an agent
	// clears the rows from 41 on after them.
	const carolCells, cells = 40 * 10, 30 * 20
	props := [propCount][2]string{
		{`{"bold":true}`, `{"bold":null}`},
		{`{"italic":true}`, `{"italic":null}`},
		{`{"underline":true}`, `{"underline":null}`},
		{`{"strikethrough":true}`, `{"strikethrough":null}`},
		{`{"text_color":"blue-600"}`, `{"text_color":null}`},
		{`{"fill":"teal-200"}`, `{"fill":null}`},
		{`{"align":"right"}`, `{"align":null}`},
		{`{"wrap":true}`, `{"wrap":null}`},
		{`{"number":{"kind":"percent","decimals":1}}`, `{"number":null}`},
	}
	for _, how := range []string{"the file reaches its reserve", "SQLite refuses to grow the file"} {
		t.Run(how, func(t *testing.T) {
			e := start(t)
			fillNumbers(e, "Sheet1", rows, cols)
			carol, dave := e.person("Carol"), e.person("Dave")
			// Dave's entry holds no stamp of a blank record.
			dave.set("AN1", "Dave's")
			if ack := carol.do(e.clearOp("Sheet1", 1, 40, 1, 10)); status(ack) != "applied" {
				t.Fatalf("Carol's clear: %s", status(ack))
			}
			e.in.mu.Lock()
			e.in.wb.collected.limit = limit
			e.in.mu.Unlock()
			e.collectTo(limit)
			// No tick collects again until the twenty holders are in place,
			// the instance's own ticks included: a record collected in
			// between makes another case.
			e.in.mu.Lock()
			e.in.wb.collected.limit = 0
			e.in.mu.Unlock()
			carolRows := e.blankRows()
			if carolRows < carolCells-limit {
				t.Fatalf("%d of Carol's records are on disk", carolRows)
			}
			// The file has more room than the reserve, and less than the
			// records of the twenty holders take.
			e.in.mu.Lock()
			b := &e.in.scratch.blanks
			size, err := b.bytes()
			if err != nil {
				t.Fatal(err)
			}
			pages := size / blankPage
			if how == "the file reaches its reserve" {
				b.maxBytes = size + 384<<10
				pages = b.maxBytes / blankPage
			} else {
				pages += 32
			}
			if _, err := b.db.Exec(fmt.Sprintf(`PRAGMA max_page_count=%d`, pages)); err != nil {
				t.Fatal(err)
			}
			e.in.mu.Unlock()

			region := func(op string) string {
				return jsonf(`{"op":%q,"tab":%d,"rows":%s,"cols":%s}`, op, e.tabID("Sheet1"), e.rowsOf("Sheet1", 1, 30), e.colsOf("Sheet1", 11, 20))
			}
			values := make([]string, 30)
			for i := range values {
				values[i] = "[" + strings.TrimSuffix(strings.Repeat(`"x",`, 20), ",") + "]"
			}
			content := e.person("Content")
			write := strings.TrimSuffix(region("write"), "}") + `,"values":[` + strings.Join(values, ",") + `]}`
			for _, op := range []string{region("clear"), write} {
				if ack := content.do(op); status(ack) != "applied" {
					t.Fatalf("%s: %s", op, status(ack))
				}
			}
			wantUndo(t, "undo of the write", undo(t, content), undoAck{Applied: cells})
			for prop, set := range props {
				p := e.person(propNames[prop])
				e.mustAgent(jsonf(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["K1:AD30"],"format":%s}]}`, set[0]))
				for _, s := range []string{set[1], set[0]} {
					if ack := p.do(e.formatRange("Sheet1", 1, 30, 11, 20, s)); status(ack) != "applied" {
						t.Fatalf("%s: %s", s, status(ack))
					}
				}
				wantUndo(t, "undo of the second format of "+propNames[prop], undo(t, p), undoAck{Applied: cells})
			}
			e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A41:AN100"]}]}`)
			e.in.mu.Lock()
			k1, row1, colK := e.at("K1")
			c := k1.cell(row1, colK)
			if c == nil || !c.blank() || e.in.wb.counted(c) {
				e.in.mu.Unlock()
				t.Fatal("K1 is not a blank record without a format")
			}
			rec := collectedBlank{seq: c.seq, textSeq: c.textSeq, by: c.by}
			stamps := e.in.wb.styles.state(c.style).stamps
			rec.stamps = &stamps
			held := map[uint64]bool{}
			e.in.holders().each(cellID{k1.id, row1, colK}, &rec, func(u *undoEntry) { held[u.seq] = true })
			e.in.mu.Unlock()
			if len(held) != 20 {
				t.Fatalf("K1 has %d holders, want 20", len(held))
			}

			e.in.mu.Lock()
			e.in.wb.collected.limit = limit
			e.in.mu.Unlock()
			// None of many ticks may count the full file as an error.
			for tick := 0; tick < 2*blankFails; tick++ {
				e.tick()
				e.in.mu.RLock()
				lost, fails := b.lost, b.fails
				e.in.mu.RUnlock()
				if lost || fails != 0 {
					t.Fatalf("after tick %d the store is lost: %v, with %d errors in a row", tick+1, lost, fails)
				}
			}
			if how == "the file reaches its reserve" {
				// The tick uses SQLite's page count, so it stops within a
				// few pages of the reserve below its bound.
				e.in.mu.RLock()
				n, err := b.bytes()
				bound := b.maxBytes
				e.in.mu.RUnlock()
				if err != nil {
					t.Fatal(err)
				}
				if n > bound-(blankReserve-8)*blankPage || n < bound-(blankReserve+8)*blankPage {
					t.Fatalf("the file takes %d bytes, bound %d with a reserve of %d", n, bound, blankReserve*blankPage)
				}
			}
			if e.hasRecord("K1") {
				t.Fatal("the records of the twenty holders were not collected")
			}
			// Each record of the region is stored whole, with its twenty
			// rows, or not at all.
			e.in.mu.RLock()
			var ids []string
			for _, id := range k1.cols.ids[10:30] {
				ids = append(ids, fmt.Sprint(id))
			}
			regionCols := "(" + strings.Join(ids, ",") + ")"
			var stored, partial int
			q, err := b.db.Query(`SELECT count(*) FROM blanks WHERE tab=? AND col IN `+regionCols+` GROUP BY row,col`, int64(k1.id))
			if err == nil {
				for q.Next() {
					var n int
					q.Scan(&n)
					if n == 20 {
						stored++
					} else {
						partial++
					}
				}
				err = q.Err()
				q.Close()
			}
			e.in.mu.RUnlock()
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%d of %d records of twenty holders stored, %d of Carol's", stored, cells, carolRows)
			if stored == 0 || stored == cells || partial != 0 {
				t.Fatalf("%d records stored whole and %d in part, of %d", stored, partial, cells)
			}
			// Only the entries whose records were not stored lost cells.
			e.in.mu.RLock()
			for _, s := range e.in.sessions {
				for _, stack := range [2][]*undoEntry{s.undo.undo, s.undo.redo} {
					for _, u := range stack {
						if u.blanksLost && !held[u.seq] {
							t.Errorf("an entry of %s that held no lost record skips its cells as lost", s.id)
						}
						if !u.blanksLost && held[u.seq] {
							t.Errorf("an entry of %s that held lost records is not marked", s.id)
						}
					}
				}
			}
			e.in.mu.RUnlock()
			wantUndo(t, "Carol's undo of her clear", undo(t, carol), undoAck{Applied: carolCells})
			wantUndo(t, "Dave's undo", undo(t, dave), undoAck{Applied: 1})
			// The clear of the content is undone where its record was
			// stored, and skipped where it was not.
			wantUndo(t, "undo of the clear", undo(t, content), undoAck{Applied: stored, Skipped: cells - stored, Reason: "lost"})
		})
	}
}

// A scratch file system that fills up costs no stored record. Every
// statement of the store needs room for its rollback journal, a deletion
// too, and SQLite takes back the statement or the transaction that finds
// none. The records stored before stay, however many ticks find no room,
// and the undo of an entry that held them restores its cells once there is
// room again.
func TestCollectedStampStoreOnAFullFileSystem(t *testing.T) {
	const rows, cols, limit = 40, 30, 100
	const half = rows / 2 * cols
	cellName := func(c, r int) string { return columnLetters(c-1) + strconv.Itoa(r) }
	// setup lets Alice clear the upper half of the numbers and Bob the
	// lower, and collects their records with the store's file on disk when
	// collect is set. Without it no tick collects before the limit is
	// lowered, the instance's own ticks included.
	setup := func(t *testing.T, collect bool) (*env, *fullDisk, *person, *person) {
		root, disk := newFullDisk(t)
		e := start(t, func(o *Options) { o.ScratchDir = root })
		e.in.mu.Lock()
		if collect {
			e.in.wb.collected.limit = limit
		}
		e.in.scratch.blanks.vfs = fullVFSName
		e.in.mu.Unlock()
		fillNumbers(e, "Sheet1", rows, cols)
		alice, bob := e.person("Alice"), e.person("Bob")
		if ack := alice.do(e.clearOp("Sheet1", 1, rows/2, 1, cols)); status(ack) != "applied" {
			t.Fatalf("Alice's clear: %s", status(ack))
		}
		if ack := bob.do(e.clearOp("Sheet1", rows/2+1, rows/2, 1, cols)); status(ack) != "applied" {
			t.Fatalf("Bob's clear: %s", status(ack))
		}
		if collect {
			e.collectTo(limit)
		}
		return e, disk, alice, bob
	}
	// held returns the rows of the store that an entry holds.
	held := func(e *env, p *person, i int) int {
		e.t.Helper()
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		b := &e.in.scratch.blanks
		var n int
		if err := b.db.QueryRow(`SELECT count(*) FROM blanks WHERE holder=?`, int64(p.s.undo.undo[i].seq)).Scan(&n); err != nil {
			e.t.Fatal(err)
		}
		return n
	}
	// intact fails the test when the store counted an error or was given
	// up.
	intact := func(t *testing.T, e *env, when string) {
		t.Helper()
		e.in.mu.RLock()
		b := &e.in.scratch.blanks
		lost, fails := b.lost, b.fails
		e.in.mu.RUnlock()
		if lost || fails != 0 {
			t.Fatalf("%s the store is lost: %v, with %d errors in a row", when, lost, fails)
		}
	}
	// refused fails the test unless writes to the rollback journal failed
	// since the count was read.
	refused := func(t *testing.T, disk *fullDisk, since int64, what string) int64 {
		t.Helper()
		n := disk.journal.Load()
		if n <= since {
			t.Fatalf("%s wrote its journal on a full file system", what)
		}
		return n
	}
	// noted returns how many cells of count rows from the given one have a
	// record in the sheet.
	noted := func(e *env, from, count int) int {
		e.t.Helper()
		n := 0
		for r := from; r < from+count; r++ {
			for c := 1; c <= cols; c++ {
				if e.hasRecord(cellName(c, r)) {
					n++
				}
			}
		}
		return n
	}

	t.Run("pruning", func(t *testing.T) {
		e, disk, alice, bob := setup(t, true)
		aliceRows, bobRows := held(e, alice, 0), held(e, bob, 0)
		if aliceRows < half-limit || bobRows < half-limit {
			t.Fatalf("%d of Alice's and %d of Bob's records are on disk", aliceRows, bobRows)
		}
		// Bob's entry leaves his stack, and its rows are to go.
		e.in.mu.Lock()
		bobSeq := bob.s.undo.undo[0].seq
		bob.s.undo.drop()
		e.in.mu.Unlock()
		disk.full.Store(true)
		var journal int64
		for tick := 0; tick < 2*blankFails; tick++ {
			e.tick()
			journal = refused(t, disk, journal, fmt.Sprintf("the deletion of tick %d", tick+1))
			intact(t, e, fmt.Sprintf("after tick %d", tick+1))
		}
		e.in.mu.RLock()
		var left int
		err := e.in.scratch.blanks.db.QueryRow(`SELECT count(*) FROM blanks WHERE holder=?`, int64(bobSeq)).Scan(&left)
		e.in.mu.RUnlock()
		if err != nil {
			t.Fatal(err)
		}
		if left != bobRows || held(e, alice, 0) != aliceRows {
			t.Fatalf("with no room %d of Bob's %d rows and %d of Alice's %d are left", left, bobRows, held(e, alice, 0), aliceRows)
		}
		// With room again, the next tick deletes Bob's rows.
		disk.full.Store(false)
		e.tick()
		if n := e.blankRows(); n != aliceRows {
			t.Fatalf("%d rows on disk, want Alice's %d", n, aliceRows)
		}
		wantUndo(t, "Alice's undo", undo(t, alice), undoAck{Applied: half})
		e.want("A1", "1001")
	})

	t.Run("consumption", func(t *testing.T) {
		e, disk, alice, bob := setup(t, true)
		aliceRows, bobRows := held(e, alice, 0), held(e, bob, 0)
		disk.full.Store(true)
		// Bob writes into cells whose records were collected, with no tick
		// between them. Each write commits and takes the stamps of the
		// cell's row, which then cannot be deleted.
		var journal int64
		var written []string
		first := 0
		for r := rows; len(written) < 2*blankFails; r-- {
			addr := cellName(1, r)
			if e.hasRecord(addr) {
				continue
			}
			if ack := bob.set(addr, "bob"); status(ack) != "applied" {
				t.Fatalf("Bob's write into %s: %s", addr, status(ack))
			}
			if written = append(written, addr); first == 0 {
				first = r
			}
			journal = refused(t, disk, journal, "the deletion after the write into "+addr)
			intact(t, e, "after the write into "+addr)
		}
		for tick := 0; tick < 2*blankFails; tick++ {
			e.tick()
			intact(t, e, fmt.Sprintf("after tick %d", tick+1))
		}
		if held(e, alice, 0) != aliceRows || held(e, bob, 0) != bobRows {
			t.Fatal("rows went with no room")
		}
		disk.full.Store(false)
		wantUndo(t, "Alice's undo", undo(t, alice), undoAck{Applied: half})
		for i := range written {
			wantUndo(t, fmt.Sprintf("Bob's undo %d", i+1), undo(t, bob), undoAck{Applied: 1})
		}
		wantUndo(t, "Bob's undo of his clear", undo(t, bob), undoAck{Applied: half})
		e.want("A1", "1001")
		e.want(written[0], fmt.Sprint(first*1000+1))
	})

	t.Run("collection", func(t *testing.T) {
		e, disk, alice, bob := setup(t, true)
		aliceRows := held(e, alice, 0)
		disk.full.Store(true)
		// Alice writes into a cell whose record was collected and clears it
		// again. Its row from the first collection is left, so the cell is
		// not collected while that row cannot be deleted.
		x, xr := "", 0
		for r := 1; x == ""; r++ {
			if addr := cellName(1, r); !e.hasRecord(addr) {
				x, xr = addr, r
			}
		}
		alice.set(x, "again")
		if ack := alice.do(e.clearOp("Sheet1", xr, 1, 1, 1)); status(ack) != "applied" {
			t.Fatalf("Alice's clear of %s: %s", x, status(ack))
		}
		// Bob writes and clears rows of his at each tick, more cells than
		// the limit, so that every tick collects records that do not fit.
		const more = 4
		values := strings.TrimSuffix(strings.Repeat("["+strings.TrimSuffix(strings.Repeat(`"7",`, cols), ",")+"],", more), ",")
		var journal int64
		for tick := 0; tick < 2*blankFails; tick++ {
			row := rows + 1 + more*tick
			write := jsonf(`{"op":"write","tab":%d,"rows":%s,"cols":%s,"values":[%s]}`,
				e.tabID("Sheet1"), e.rowsOf("Sheet1", row, more), e.colsOf("Sheet1", 1, cols), values)
			for _, op := range []string{write, e.clearOp("Sheet1", row, more, 1, cols)} {
				if ack := bob.do(op); status(ack) != "applied" {
					t.Fatalf("%s: %s", op, status(ack))
				}
			}
			before := e.blanks()
			e.tick()
			if before > limit && e.blanks() >= before {
				t.Fatalf("tick %d collected nothing: %d blank records", tick+1, e.blanks())
			}
			journal = refused(t, disk, journal, fmt.Sprintf("the collection of tick %d", tick+1))
			intact(t, e, fmt.Sprintf("after tick %d", tick+1))
			if !e.hasRecord(x) {
				t.Fatalf("tick %d collected %s, whose earlier row is left", tick+1, x)
			}
		}
		e.in.mu.RLock()
		var lostAlice, lostBob bool
		for _, u := range alice.s.undo.undo {
			lostAlice = lostAlice || u.blanksLost
		}
		// Bob's clear at the first tick, after his first clear and write.
		lostBob = bob.s.undo.undo[2].blanksLost
		e.in.mu.RUnlock()
		if lostAlice || !lostBob {
			t.Fatalf("Alice's entries skip cells as lost: %v; Bob's first clear at a tick: %v", lostAlice, lostBob)
		}
		if held(e, alice, 0) != aliceRows {
			t.Fatalf("%d of Alice's %d rows are left", held(e, alice, 0), aliceRows)
		}
		disk.full.Store(false)
		// With room, the next collection takes the cell, whose rows are
		// those of Alice's last clear.
		e.in.mu.Lock()
		e.in.wb.collected.limit = 1
		e.in.mu.Unlock()
		e.tick()
		if e.hasRecord(x) {
			t.Fatalf("%s is not collected with room", x)
		}
		wantUndo(t, "Alice's undo of her clear of "+x, undo(t, alice), undoAck{Applied: 1})
		wantUndo(t, "Alice's undo of her write into "+x, undo(t, alice), undoAck{Applied: 1})
		wantUndo(t, "Alice's undo of her clear", undo(t, alice), undoAck{Applied: half})
		e.want("A1", "1001")
		e.want(x, fmt.Sprint(xr*1000+1))
	})

	t.Run("the file cannot be made", func(t *testing.T) {
		e, disk, alice, bob := setup(t, false)
		disk.full.Store(true)
		before := noted(e, 1, rows/2)
		e.in.mu.Lock()
		e.in.wb.collected.limit = limit
		e.in.mu.Unlock()
		e.collectTo(limit)
		if disk.refused.Load() == 0 {
			t.Fatal("the store made its file on a full file system")
		}
		intact(t, e, "after the collection")
		// Every record was collected without rows: the entries skip those
		// cells as lost, and restore the cells that kept their records.
		kept := noted(e, 1, rows/2)
		if before != half || kept >= half {
			t.Fatalf("%d and then %d of Alice's cells have records", before, kept)
		}
		disk.full.Store(false)
		wantUndo(t, "Alice's undo", undo(t, alice), undoAck{Applied: kept, Skipped: half - kept, Reason: "lost"})
		kept = noted(e, rows/2+1, rows/2)
		wantUndo(t, "Bob's undo", undo(t, bob), undoAck{Applied: kept, Skipped: half - kept, Reason: "lost"})
		// With room, the store keeps what is collected from then on.
		fillNumbers(e, "Sheet1", rows/2, cols)
		if ack := alice.do(e.clearOp("Sheet1", 1, rows/2, 1, cols)); status(ack) != "applied" {
			t.Fatalf("Alice's second clear: %s", status(ack))
		}
		e.collectTo(limit)
		if e.blankRows() == 0 {
			t.Fatal("no record on disk with room")
		}
		wantUndo(t, "Alice's undo of her second clear", undo(t, alice), undoAck{Applied: half})
	})
}

// The rows of rows that left the sheet go in steps of blankRecheckWork
// rows, columns and tabs, also when a collection prunes to make room: a
// collection does not walk the whole store under the workbook lock.
func TestCollectedStampStorePrunesInSteps(t *testing.T) {
	const rows, more, limit = 3 * blankRecheckWork, 200, 100
	e := start(t)
	fillNumbers(e, "Sheet1", rows+more, 1)
	alice, bob, carol := e.person("Alice"), e.person("Bob"), e.person("Carol")
	if ack := alice.do(e.clearOp("Sheet1", 1, rows, 1, 1)); status(ack) != "applied" {
		t.Fatalf("Alice's clear: %s", status(ack))
	}
	e.in.mu.Lock()
	e.in.wb.collected.limit = limit
	e.in.mu.Unlock()
	e.collectTo(limit)
	// aliceRows counts the rows of Alice's entry, with the lock held.
	aliceRows := func() int {
		var n int
		err := e.in.scratch.blanks.db.QueryRow(`SELECT count(*) FROM blanks WHERE holder=?`, int64(alice.s.undo.undo[0].seq)).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	e.in.mu.RLock()
	stored := aliceRows()
	e.in.mu.RUnlock()
	if stored < rows-limit {
		t.Fatalf("%d of Alice's records are on disk", stored)
	}
	// Bob deletes the rows, and nobody can bring them back.
	if ack := bob.do(e.deleteRowsOp("Sheet1", 1, rows)); status(ack) != "applied" {
		t.Fatalf("Bob's deletion: %s", status(ack))
	}
	if ack := carol.do(e.clearOp("Sheet1", 1, more, 1, 1)); status(ack) != "applied" {
		t.Fatalf("Carol's clear: %s", status(ack))
	}
	// The file has no room left: a collection of Carol's records prunes
	// first. The lock keeps the instance's own ticks out of the count.
	e.in.mu.Lock()
	bob.s.undo.drop()
	b := &e.in.scratch.blanks
	size, err := b.bytes()
	if err != nil {
		e.in.mu.Unlock()
		t.Fatal(err)
	}
	b.maxBytes = size
	before := aliceRows()
	refs, _ := e.in.wb.oldBlanks()
	_, err = e.in.keepBlanks(context.Background(), refs)
	after := aliceRows()
	e.in.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// It prunes at most twice: before it stores and when the file fills.
	if gone := before - after; gone == 0 || gone > 2*blankRecheckWork {
		t.Fatalf("the collection deleted %d of %d rows of deleted rows", gone, before)
	}
	// The ticks delete the rest.
	for tick := 1; ; tick++ {
		e.in.mu.RLock()
		left := aliceRows()
		e.in.mu.RUnlock()
		if left == 0 {
			break
		}
		if tick > rows/blankRecheckWork+2 {
			t.Fatalf("%d rows of deleted rows are left after %d ticks", left, tick-1)
		}
		e.tick()
	}
}
