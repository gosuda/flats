package sheet

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// An undo or a redo is an operation of its action, with the checkpoints of
// one (technical design, section 10.4). It is refused at its deadline like
// any action, and the entry stays on its stack as it was: also an entry
// whose every group another writer has changed since, which the undo would
// only skip.

// top returns the newest entry of a person's undo or redo stack.
func (p *person) top(redo bool) *undoEntry {
	p.e.in.mu.RLock()
	defer p.e.in.mu.RUnlock()
	stack := p.s.undo.undo
	if redo {
		stack = p.s.undo.redo
	}
	if len(stack) == 0 {
		return nil
	}
	return stack[len(stack)-1]
}

// An undo that comes after its action's deadline is refused before it looks
// at the entry. Bob has overwritten the one cell Alice's entry holds, so
// the undo would skip it, change nothing and take the entry off the stack.
func TestUndoAfterItsDeadlineKeepsTheEntry(t *testing.T) {
	for _, c := range []struct {
		name string
		redo bool
	}{{"undo", false}, {"redo", true}} {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			alice, bob := e.person("Alice"), e.person("Bob")
			alice.set("A1", "alice")
			op := `{"op":"undo"}`
			if c.redo {
				wantUndo(t, "the undo before the redo", undo(t, alice), undoAck{Applied: 1})
				op = `{"op":"redo"}`
			}
			bob.set("A1", "bob")
			entry := alice.top(c.redo)
			before, kept := e.dump(), e.memory("Sheet1", alice)
			deadline := e.workDeadline()
			var ack ackBody
			deadline.refusedAt(t, 0, 0, func() string {
				ack = alice.do(op)
				return status(ack)
			})
			if ack.Undo != nil {
				t.Fatalf("the refused %s returned an undo result: %+v", c.name, ack.Undo)
			}
			if after := e.dump(); after != before {
				t.Fatalf("the refused %s changed the sheet: %s", c.name, dumpDiff(before, after))
			}
			if !kept.equal(e.memory("Sheet1", alice)) || alice.top(c.redo) != entry {
				t.Fatalf("the refused %s changed the undo stacks or the structural log", c.name)
			}
			// On retry, the same entry is skipped and leaves.
			ack = alice.do(op)
			if ack.Undo == nil {
				t.Fatalf("the retried %s: %s", c.name, status(ack))
			}
			wantUndo(t, "the retried "+c.name, ack.Undo, undoAck{Skipped: 1, Reason: "changed"})
			if alice.top(c.redo) == entry {
				t.Errorf("the skipped %s left its entry on the stack", c.name)
			}
			e.want("A1", "bob")
		})
	}
}

// An undo that restores many cells meets the deadline while it restores
// them, and is refused there: not after it has restored the rest. Alice
// clears 499,800 numbers at the full size, Bob writes one of the cells
// again, and the tick collects half of the blank records the clear left,
// so the undo also reads stamps from the store. The redo of that undo,
// and the undo of the redo, are refused the same way. Each has its
// deadline at a quarter of the units it counts without one.
func TestUndoStopsAtTheDeadline(t *testing.T) {
	slow(t, "clears and brings back 499,800 cells")
	// Under the race detector the case still counts several times the
	// 65,536 units that stopsAtDeadline needs to tell a stop from the end.
	rows, cols := raced(sized(4998, 2500), 500), 100
	cells := rows * cols
	e := start(t, unhurried)
	fillNumbers(e, "Sheet1", rows, cols)
	alice, bob := e.person("Alice"), e.person("Bob")
	if ack := alice.do(e.clearOp("Sheet1", 1, rows, 1, cols)); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	bob.set("A1", "bob")
	e.collectBlanks(0)
	deadline := e.workDeadline()
	// whole runs op without a deadline and checks its outcome.
	whole := func(op string, want undoAck) budgetWork {
		t.Helper()
		var u *undoAck
		got, work := deadline.run(never, func() string {
			ack := alice.do(op)
			u = ack.Undo
			return status(ack)
		})
		if got != "applied" || u == nil {
			t.Fatalf("%s: %s", op, got)
		}
		wantUndo(t, op, u, want)
		// The blank records a clear leaves are collected now, not while
		// a refused action is compared with the sheet before it.
		e.collectBlanks(0)
		return work
	}
	// refused runs op with its deadline at a quarter of the work of the
	// whole one.
	refused := func(op string, whole budgetWork) {
		t.Helper()
		before, kept := e.dump(), e.memory("Sheet1", alice)
		deadline.stopsAtDeadline(t, whole, func() string { return status(alice.do(op)) })
		if after := e.dump(); after != before {
			t.Fatalf("the refused %s changed the sheet: %s", op, dumpDiff(before, after))
		}
		if !kept.equal(e.memory("Sheet1", alice)) {
			t.Fatalf("the refused %s changed the undo stacks or the structural log", op)
		}
	}
	took := whole(`{"op":"undo"}`, undoAck{Applied: cells - 1, Skipped: 1, Reason: "changed"})
	e.want("Sheet1!B1", "1002")
	refused(`{"op":"redo"}`, took)
	took = whole(`{"op":"redo"}`, undoAck{Applied: cells - 1})
	e.want("Sheet1!B1", "")
	refused(`{"op":"undo"}`, took)
	whole(`{"op":"undo"}`, undoAck{Applied: cells - 1})
	e.want("Sheet1!B1", "1002")
	e.want("Sheet1!A1", "bob")
}

// The instance's own maintenance tick runs every few seconds and can come
// between the two looks at the sheet around a refused undo. While the
// sheet holds more blank records than its limit, each tick removes the
// older half again, and the comparison would count that among the undo's
// changes. collectBlanks leaves that tick nothing to remove. Here a redo
// leaves ten times the limit, the tick of collectBlanks takes half of
// them, the instance's tick runs right after the refused undo, and the
// sheet is as it was.
func TestUndoRefusedBesideTheTick(t *testing.T) {
	const rows, cols, limit = 100, 10, 100
	e := start(t)
	fillNumbers(e, "Sheet1", rows, cols)
	alice := e.person("Alice")
	if ack := alice.do(e.clearOp("Sheet1", 1, rows, 1, cols)); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	e.collectBlanks(limit)
	wantUndo(t, "the undo", undo(t, alice), undoAck{Applied: rows * cols})
	e.collectBlanks(limit)
	wantUndo(t, "the redo", redo(t, alice), undoAck{Applied: rows * cols})
	e.collectBlanks(limit)
	if n := e.blanks(); n <= limit {
		t.Fatalf("the sheet holds %d blank records after the redo, want more than %d", n, limit)
	}
	before, kept := e.dump(), e.memory("Sheet1", alice)
	deadline := e.workDeadline()
	deadline.refusedAt(t, 0, 0, func() string {
		return status(alice.do(`{"op":"undo"}`))
	})
	e.tick()
	if after := e.dump(); after != before {
		t.Fatalf("the refused undo and the tick changed the sheet: %s", dumpDiff(before, after))
	}
	if !kept.equal(e.memory("Sheet1", alice)) {
		t.Fatal("the refused undo changed the undo stacks or the structural log")
	}
	wantUndo(t, "the undo", undo(t, alice), undoAck{Applied: rows * cols})
	e.want("Sheet1!B1", "1002")
}

// An undo or a redo of a clear whose blank records the tick collected in
// part reads stamps from the store and gives the cells it brings back a
// record. Stopped at its deadline at any point, it leaves the sheet, the
// stacks and the rows of the store as they were: it takes no row out of
// the store before it commits. The stop points run through the whole
// undo, less than a cell apart.
func TestUndoStoppedKeepsTheCollectedStamps(t *testing.T) {
	const rows, cols, limit = 250, 20, 300
	e := start(t)
	fillNumbers(e, "Sheet1", rows, cols)
	alice, bob := e.person("Alice"), e.person("Bob")
	if ack := alice.do(e.clearOp("Sheet1", 1, rows, 1, cols)); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	bob.set("A1", "bob")
	e.collectBlanks(limit)
	// stored counts the rows of the store in its file.
	stored := func() int64 {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		var n int64
		if err := e.in.scratch.blanks.db.QueryRow(`SELECT count(*) FROM blanks`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sweep := func(what string) {
		t.Helper()
		if n := stored(); n == 0 {
			t.Fatalf("%s: the store holds no row", what)
		}
		_, whole := stoppedUndo(t, e, alice, false, 1<<62, nil)
		stops := 0
		for deadline := int64(1); deadline < whole; deadline += 61 {
			before, kept, entry, held := e.dump(), e.memory("Sheet1", alice), alice.top(false), stored()
			err, _ := stoppedUndo(t, e, alice, false, deadline, nil)
			if err == nil {
				continue
			}
			if err.Limit != "recalculation_time" {
				t.Fatalf("%s stopped after %d units: %v", what, deadline, err)
			}
			stops++
			if after := e.dump(); after != before {
				t.Fatalf("%s stopped after %d units changed the sheet: %s", what, deadline, dumpDiff(before, after))
			}
			if !kept.equal(e.memory("Sheet1", alice)) || alice.top(false) != entry {
				t.Fatalf("%s stopped after %d units changed the undo stacks", what, deadline)
			}
			if n := stored(); n != held {
				t.Fatalf("%s stopped after %d units left %d rows in the store, it held %d", what, deadline, n, held)
			}
		}
		if stops < 50 {
			t.Fatalf("%s stopped %d times in %d units, want at least 50", what, stops, whole)
		}
	}
	sweep("the undo")
	wantUndo(t, "the undo", undo(t, alice), undoAck{Applied: rows*cols - 1, Skipped: 1, Reason: "changed"})
	e.collectBlanks(limit)
	wantUndo(t, "the redo", redo(t, alice), undoAck{Applied: rows*cols - 1})
	e.collectBlanks(limit)
	sweep("the undo of the redo")
	wantUndo(t, "the undo of the redo", undo(t, alice), undoAck{Applied: rows*cols - 1})
	e.want("Sheet1!B1", "1002")
	e.want("Sheet1!A1", "bob")
}

// An undo of an entry in a scratch file checks and decodes the file before
// it restores a cell. Both look at the deadline as they go: an undo whose
// deadline passes there is refused without reading the rest of the file,
// and the entry and its file stay for the next undo. The deadline here is
// passed at the nth look at the clock: at the start of the check of the
// file, inside it, and inside the decoding.
func TestUndoStopsWhileItReadsItsPayload(t *testing.T) {
	const rows, cols = 1000, 100
	e := start(t)
	e.in.mu.Lock()
	e.in.undoPool().entryMax = -1
	e.in.mu.Unlock()
	fillNumbers(e, "Sheet1", rows, cols)
	alice := e.person("Alice")
	if ack := alice.do(e.clearOp("Sheet1", 1, rows, 1, cols)); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	file := alice.spilled(0)
	if file == nil {
		t.Fatal("the clear's payload is not in a scratch file")
	}
	var at, size int64
	spillReadWatch = func(a, s int64) { at, size = a, s }
	t.Cleanup(func() { spillReadWatch = nil })
	info, err := os.Stat(file.path)
	if err != nil {
		t.Fatal(err)
	}
	// The check of the file looks at the clock before each spillPoll bytes
	// of it but the last four.
	checks := (info.Size() - 4 + spillPoll - 1) / spillPoll
	// most is how far into the file the read may get: up to the look that
	// finds the deadline in the check, and in the decoding the pieces up
	// to its second look, which take a small part of the file, and what
	// its buffer reads ahead.
	for _, c := range []struct{ n, most int64 }{
		{1, 0},
		{checks / 2, (checks/2 - 1) * spillPoll},
		{checks + 2, info.Size() / 4},
	} {
		n := c.n
		before, kept, entry := e.dump(), e.memory("Sheet1", alice), alice.top(false)
		at, size = -1, -1
		err := func() *WriteError {
			e.in.mu.Lock()
			defer e.in.mu.Unlock()
			wb := e.in.wb
			x := newTxn(wb, wb.seq+1, wb.seq, alice.s.ord)
			e.in.begin(x)
			x.overdue = nil
			looks := int64(0)
			x.budget = formula.NewBudget(func() bool { looks++; return looks >= n })
			_, _, _, err := x.applyUndo(&alice.s.undo, entry)
			wb.revert(&x.j)
			wb.settle()
			return err
		}()
		if err == nil || err.Limit != "recalculation_time" {
			t.Fatalf("the undo with the deadline at look %d: %v", n, err)
		}
		if size <= 0 || at > c.most {
			t.Fatalf("the undo with the deadline at look %d read %d bytes of the %d of its file", n, at, size)
		}
		if after := e.dump(); after != before {
			t.Fatalf("the refused undo changed the sheet: %s", dumpDiff(before, after))
		}
		if !kept.equal(e.memory("Sheet1", alice)) || alice.top(false) != entry || alice.spilled(0) != file {
			t.Fatal("the refused undo changed the undo stacks or the entry's file")
		}
		if _, err := os.Stat(file.path); err != nil {
			t.Fatalf("the entry's file: %v", err)
		}
	}
	wantUndo(t, "the undo", undo(t, alice), undoAck{Applied: rows * cols})
	// The decoding ends before the checksum, the last four bytes.
	if at != size-4 {
		t.Fatalf("the undo inside its limit read %d bytes of the %d of its file", at, size)
	}
	e.want("Sheet1!B1", "1002")
}

// The copy an undo makes of an entry in memory that keeps deletions apart,
// and the pass that shares the formulas of a payload with the sheet after
// it followed edits, both look at the deadline as they go. Stopped at
// their first look they do a piece of their work at most: the copy
// allocates a small part of what a whole copy does, and the pass, once the
// budget has found the deadline, charges nothing and ends. Both leave the
// entry as it was.
func TestUndoCopyAndShareStopAtTheDeadline(t *testing.T) {
	const rows, cols = 100, 50
	e := start(t)
	fillNumbers(e, "Sheet1", rows, cols)
	alice := e.person("Alice")
	if ack := alice.do(e.deleteColsOp("Sheet1", 1, cols-1)); status(ack) != "applied" || alice.spilled(0) != nil {
		t.Fatalf("the deletion: %s, in a scratch file: %v", status(ack), alice.spilled(0) != nil)
	}
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	wb, entry := e.in.wb, alice.s.undo.undo[0]
	before := len(entry.steps[0].cells)
	copyAlloc := func(late func() bool) (uint64, error) {
		var was, now runtime.MemStats
		runtime.ReadMemStats(&was)
		copied, err := entry.copyPayload(wb, late)
		runtime.ReadMemStats(&now)
		if err == nil && len(copied.steps[0].cells) != before {
			t.Fatalf("the copy holds %d cells, the entry %d", len(copied.steps[0].cells), before)
		}
		return now.TotalAlloc - was.TotalAlloc, err
	}
	whole, err := copyAlloc(nil)
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	stopped, err := copyAlloc(func() bool { asked++; return true })
	if !errors.Is(err, errSpillLate) || asked != 1 {
		t.Fatalf("the copy stopped at its first look: %v, after %d looks", err, asked)
	}
	t.Logf("a whole copy allocates %d bytes, one stopped at its first look %d", whole, stopped)
	if measures && stopped > whole/2 {
		t.Errorf("the copy stopped at its first look allocated %d bytes, a whole copy %d", stopped, whole)
	}
	if len(entry.steps[0].cells) != before {
		t.Fatal("the stopped copy changed the entry")
	}
	copied, err := entry.copyPayload(wb, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The budget has found the deadline passed, as at a check of an
	// earlier part of the undo.
	b := formula.NewBudget(func() bool { return true })
	b.Check()
	if copied.shareSaved(wb, b) {
		t.Fatal("the sharing went on past the deadline")
	}
	if b.Used() != 0 {
		t.Fatalf("the sharing stopped at its first look charged %d units", b.Used())
	}
	if !copied.shareSaved(wb, nil) {
		t.Fatal("the sharing without a deadline stopped")
	}
}

// The pass that shares the formulas of a spilled payload with the sheet,
// after the payload followed a later insert, reads the clock before every
// spillPoll cells (technical design, section 10.4), with a budget whose
// deadline nothing has found yet. Alice's entry holds 11,000 cells of
// formulas, three pieces. Through her undo the deadline is found at each
// of the last three looks, which are the pass's: the undo is refused and
// the sheet, the stacks and the entry's file stay. Then it applies.
func TestUndoShareReadsTheClock(t *testing.T) {
	const rows, cols = 1100, 10
	e := start(t)
	e.in.mu.Lock()
	e.in.undoPool().entryMax = -1
	e.in.mu.Unlock()
	fillNumbers(e, "Sheet1", rows, 1)
	per := maxCallCells / cols
	for from := 0; from < rows; from += per {
		var lines []string
		for r := from; r < min(rows, from+per); r++ {
			line := make([]string, cols)
			for c := range line {
				line[c] = fmt.Sprintf(`"=$A$%d+%d"`, r+1, c)
			}
			lines = append(lines, "["+strings.Join(line, ",")+"]")
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"B%d","rows":[%s]}]}`, from+1, strings.Join(lines, ",")))
	}
	alice := e.person("Alice")
	if ack := alice.do(e.deleteColsOp("Sheet1", 2, cols)); status(ack) != "applied" || alice.spilled(0) == nil {
		t.Fatalf("the deletion: %s, in a scratch file: %v", status(ack), alice.spilled(0) != nil)
	}
	// The formulas of the payload name row 1, which moves down.
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":1}]}`)
	pieces := int64((rows*cols + spillPoll - 1) / spillPoll)

	// The pass by itself, on the loaded payload: one look a piece, and it
	// ends at the look that finds the deadline, having charged the pieces
	// before it.
	e.in.mu.Lock()
	loaded, werr := alice.s.undo.undo[0].load(e.in.wb, nil)
	if werr != nil || loaded == nil {
		e.in.mu.Unlock()
		t.Fatalf("the payload: %v", werr)
	}
	for _, found := range []int64{0, 1, 2, pieces} {
		looks := int64(0)
		b := formula.NewBudget(func() bool { looks++; return looks == found })
		done := loaded.shareSaved(e.in.wb, b)
		switch {
		case found == 0 && (!done || looks != pieces):
			t.Errorf("the whole pass: done %v after %d looks at the clock, want %d", done, looks, pieces)
		case found > 0 && (done || looks != found || b.Used() != (found-1)*spillPoll/16):
			t.Errorf("the pass with the deadline at look %d: done %v after %d looks, %d units", found, done, looks, b.Used())
		}
	}
	e.in.mu.Unlock()

	// Through the undo. With no deadline the looks are counted up to the
	// end of the pass, which is where the follow watch is called.
	var looks, deadline, atEnd int64
	followWatch = func(*followRun) { atEnd = looks }
	t.Cleanup(func() { followWatch = nil })
	run := func() *WriteError {
		e.in.mu.Lock()
		defer e.in.mu.Unlock()
		wb, u := e.in.wb, &alice.s.undo
		x := newTxn(wb, wb.seq+1, wb.seq, alice.s.ord)
		e.in.begin(x)
		x.overdue = nil
		looks = 0
		x.budget = formula.NewBudget(func() bool { looks++; return looks == deadline })
		_, _, _, err := x.applyUndo(u, u.undo[len(u.undo)-1])
		wb.revert(&x.j)
		wb.settle()
		return err
	}
	if err := run(); err != nil || atEnd < pieces {
		t.Fatalf("the undo without a deadline: %v, %d looks to the end of its pass", err, atEnd)
	}
	last := atEnd
	for deadline = last - pieces + 1; deadline <= last; deadline++ {
		before, kept, entry, file := e.dump(), e.memory("Sheet1", alice), alice.top(false), alice.spilled(0)
		if err := run(); err == nil || err.Limit != "recalculation_time" {
			t.Fatalf("the undo with the deadline at look %d of %d: %v", deadline, last, err)
		}
		if after := e.dump(); after != before {
			t.Fatalf("the refused undo changed the sheet: %s", dumpDiff(before, after))
		}
		if !kept.equal(e.memory("Sheet1", alice)) || alice.top(false) != entry || alice.spilled(0) != file || entry.lost {
			t.Fatal("the refused undo changed the undo stacks or the entry")
		}
		if _, err := os.Stat(file.path); err != nil {
			t.Fatalf("the entry's file: %v", err)
		}
	}
	followWatch = nil
	wantUndo(t, "the undo", undo(t, alice), undoAck{Applied: 1})
	e.want("Sheet1!B2", "1001")
	e.want(fmt.Sprintf("Sheet1!K%d", rows+1), fmt.Sprint(rows*1000+10))
}

// undoAtLook applies the newest entry of a person's undo stack in a
// transaction of its own, with a deadline that is found passed at the
// nth look at the clock, and takes the transaction back. It returns what
// the undo returned and the bytes the run allocated.
func undoAtLook(t *testing.T, e *env, p *person, n int64) (*WriteError, uint64) {
	t.Helper()
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	wb, entry := e.in.wb, p.s.undo.undo[len(p.s.undo.undo)-1]
	var was, now runtime.MemStats
	runtime.ReadMemStats(&was)
	x := newTxn(wb, wb.seq+1, wb.seq, p.s.ord)
	e.in.begin(x)
	x.overdue = nil
	looks := int64(0)
	x.budget = formula.NewBudget(func() bool { looks++; return looks >= n })
	_, _, _, err := x.applyUndo(&p.s.undo, entry)
	runtime.ReadMemStats(&now)
	wb.revert(&x.j)
	wb.settle()
	return err, now.TotalAlloc - was.TotalAlloc
}

// The decoding of a payload looks at the deadline in every list it reads,
// also in the cells a deletion of columns saved, which are a list of the
// deletion's step. Alice deletes five columns of 100,000 rows at the full
// size, and her entry is in a scratch file. Her undo is stopped at a look
// inside the decoding: it reads a few pieces of the file past that look,
// and allocates little, where the whole payload is half a million cells.
// The check of the file before allocates no more than a buffer.
// The sheet, the entry and its file stay, and the undo then applies.
func TestUndoStopsWhileItDecodesDeletedColumns(t *testing.T) {
	slow(t, "deletes and brings back five columns of many rows")
	rows, cols := raced(sized(100000, 20000), 4000), 5
	e := start(t)
	e.in.mu.Lock()
	e.in.undoPool().entryMax = -1
	e.in.mu.Unlock()
	fillNumbers(e, "Sheet1", rows, cols)
	alice := e.person("Alice")
	if ack := alice.do(e.deleteColsOp("Sheet1", 1, cols)); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	file := alice.spilled(0)
	if file == nil {
		t.Fatal("the deletion's payload is not in a scratch file")
	}
	var at, size int64
	spillReadWatch = func(a, s int64) { at, size = a, s }
	t.Cleanup(func() { spillReadWatch = nil })
	info, err := os.Stat(file.path)
	if err != nil {
		t.Fatal(err)
	}
	checks := (info.Size() - 4 + spillPoll - 1) / spillPoll
	// whole is what decoding the whole payload allocates.
	whole := func() uint64 {
		e.in.mu.Lock()
		defer e.in.mu.Unlock()
		var was, now runtime.MemStats
		runtime.ReadMemStats(&was)
		loaded, err := alice.s.undo.undo[0].load(e.in.wb, nil)
		runtime.ReadMemStats(&now)
		if err != nil || loaded == nil {
			t.Fatalf("the payload: %v", err)
		}
		return now.TotalAlloc - was.TotalAlloc
	}()
	for _, k := range []int64{1, 2, 50} {
		n := checks + k
		before, kept, entry := e.dump(), e.memory("Sheet1", alice), alice.top(false)
		at, size = -1, -1
		err, alloc := undoAtLook(t, e, alice, n)
		if err == nil || err.Limit != "recalculation_time" {
			t.Fatalf("the undo with the deadline at look %d: %v", n, err)
		}
		// The look at k comes after k pieces of the payload were decoded;
		// the file's buffer reads ahead of that.
		most := k*spillPoll + 64<<10 + spillPoll
		t.Logf("look %d of the decoding: %d of %d bytes read, %d bytes allocated, the whole payload allocates %d", k, at, size, alloc, whole)
		if size <= 0 || at > most {
			t.Fatalf("the undo with the deadline at look %d of the decoding read %d bytes of the %d of its file, want at most %d", k, at, size, most)
		}
		// A MiB for what any undo of a scratch file allocates, its check
		// of the file included, and at most 64 KiB for each piece decoded.
		if limit := uint64(1<<20 + k*(64<<10)); measures && (alloc > limit || whole < 4*limit && fullSize) {
			t.Errorf("the undo with the deadline at look %d of the decoding allocated %d bytes, want at most %d; the whole payload allocates %d", k, alloc, limit, whole)
		}
		if after := e.dump(); after != before {
			t.Fatalf("the refused undo changed the sheet: %s", dumpDiff(before, after))
		}
		if !kept.equal(e.memory("Sheet1", alice)) || alice.top(false) != entry || alice.spilled(0) != file {
			t.Fatal("the refused undo changed the undo stacks or the entry's file")
		}
		if _, err := os.Stat(file.path); err != nil {
			t.Fatalf("the entry's file: %v", err)
		}
	}
	wantUndo(t, "the undo", undo(t, alice), undoAck{Applied: 1})
	if at != size-4 {
		t.Fatalf("the undo inside its limit read %d bytes of the %d of its file", at, size)
	}
	e.want("Sheet1!A1", "1001")
	e.want(fmt.Sprintf("Sheet1!E%d", rows), fmt.Sprint(rows*1000+5))
}

// An undo that brings back a row checks each formula of it against the
// limit of a formula's length, and looks at the deadline before each: a
// row holds a long formula in each of its 702 columns here, each with
// hundreds of references, and the deadline falls among them. Once the
// budget found the deadline passed, the undo counts at most a formula
// and a few units more. In memory and from a scratch file.
func TestUndoStopsInsideAWideFormulaRow(t *testing.T) {
	slow(t, "brings back a row of 702 long formulas")
	for _, spilled := range []bool{false, true} {
		t.Run(map[bool]string{false: "in memory", true: "in a scratch file"}[spilled], func(t *testing.T) {
			e := start(t, unhurried)
			e.in.mu.Lock()
			if p := e.in.undoPool(); spilled {
				p.entryMax = -1
			} else {
				p.entryMax, p.sessionMax, p.instanceMax = 1<<40, 1<<40, 1<<40
			}
			e.in.mu.Unlock()
			e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_cols","tab":"Sheet1","at":"A","count":%d}]}`, formula.MaxCols-newTabCols))
			fillNumbers(e, "Sheet1", 10, 1)
			// The formulas lie in row 11 and read rows 1 to 10.
			// Under the race detector the setup write alone comes near the
			// edit time limit at 450 references.
			refs := raced(450, 300)
			cells := make([]string, formula.MaxCols)
			for c := range cells {
				cells[c] = strconv.Quote(longRefs(refs, func(i int) string { return fmt.Sprintf("Sheet1!$A$%d", 1+(i+c)%10) }))
			}
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A11","rows":[[` + strings.Join(cells, ",") + `]]}]}`)
			alice := e.person("Alice")
			if ack := alice.do(e.deleteRowsOp("Sheet1", 11, 1)); status(ack) != "applied" {
				t.Fatal(status(ack))
			}
			if (alice.spilled(0) != nil) != spilled {
				t.Fatalf("the deletion's payload in a scratch file: %v", alice.spilled(0) != nil)
			}
			_, whole := stoppedUndo(t, e, alice, false, 1<<62, nil)
			// One formula is its stored bytes, 64 to a unit.
			oneFormula := int64(refs*len("Sheet1!$A$10+")+63) / 64
			stops := 0
			for deadline := int64(4096); deadline < whole; deadline += 4096 {
				before, kept, entry := e.dump(), e.memory("Sheet1", alice), alice.top(false)
				var found int64 = -1
				err, used := stoppedUndoFound(t, e, alice, false, deadline, &found)
				if err == nil {
					continue
				}
				if err.Limit != "recalculation_time" {
					t.Fatalf("the undo stopped after %d units: %v", deadline, err)
				}
				stops++
				if after := used - found; after > 2*oneFormula+64 {
					t.Errorf("the undo with the deadline at %d units of %d counted %d units after the budget found it passed, a formula is %d", deadline, whole, after, oneFormula)
				}
				if after := e.dump(); after != before {
					t.Fatalf("the refused undo changed the sheet: %s", dumpDiff(before, after))
				}
				if !kept.equal(e.memory("Sheet1", alice)) || alice.top(false) != entry {
					t.Fatal("the refused undo changed the undo stacks")
				}
			}
			if stops < 5 {
				t.Fatalf("the undo of %d units stopped %d times, want at least 5", whole, stops)
			}
			wantUndo(t, "the undo", undo(t, alice), undoAck{Applied: 1})
			e.wantText("Sheet1!ZZ11", longRefs(refs, func(i int) string { return fmt.Sprintf("Sheet1!$A$%d", 1+(i+formula.MaxCols-1)%10) }))
		})
	}
}

// A sort prepares the formulas of each row it moves: it brings each to
// its new place and checks it against the limit of a formula's length.
// It looks at the deadline before each formula, as a row can hold a long
// formula in each of its 702 columns. Here row 1 holds such formulas and
// moves below row 2, in a person's sort and in its undo. Once the budget
// found the deadline passed, the sort counts at most a formula and a few
// units more, and it is refused with the sheet as it was.
func TestSortStopsInsideAWideFormulaRow(t *testing.T) {
	slow(t, "sorts a row of 702 long formulas")
	e := start(t, unhurried)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_cols","tab":"Sheet1","at":"A","count":%d}]}`, formula.MaxCols-newTabCols))
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A2","rows":[[-1],[1],[2],[3],[4],[5],[6],[7],[8],[9],[10]]}]}`)
	// Under the race detector the setup write alone comes near the edit
	// time limit at 450 references.
	refs := raced(450, 300)
	text := func(c int) string {
		return longRefs(refs, func(i int) string { return fmt.Sprintf("Sheet1!$A$%d", 3+(i+c)%10) })
	}
	cells := make([]string, formula.MaxCols)
	for c := range cells {
		cells[c] = strconv.Quote(text(c))
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[` + strings.Join(cells, ",") + `]]}]}`)
	alice := e.person("Alice")
	oneFormula := int64(refs*len("Sheet1!$A$10+")+63) / 64
	// sweep stops run at deadlines 4,096 units apart through the whole of
	// it, and checks each refused one.
	sweep := func(what string, run func(deadline int64, found *int64) (*WriteError, int64)) {
		t.Helper()
		_, whole := run(1<<62, nil)
		stops := 0
		for deadline := int64(4096); deadline < whole; deadline += 4096 {
			before, kept := e.dump(), e.memory("Sheet1", alice)
			found := int64(-1)
			err, used := run(deadline, &found)
			if err == nil {
				// The formulas are nearly all the work: a deadline well
				// before the end refuses it.
				if deadline+8192 < whole {
					t.Errorf("%s with the deadline at %d units of %d went on to the end", what, deadline, whole)
				}
				continue
			}
			if err.Limit != "recalculation_time" {
				t.Fatalf("%s stopped after %d units: %v", what, deadline, err)
			}
			stops++
			if after := used - found; after > 2*oneFormula+64 {
				t.Errorf("%s with the deadline at %d units of %d counted %d units after the budget found it passed, a formula is %d", what, deadline, whole, after, oneFormula)
			}
			if after := e.dump(); after != before {
				t.Fatalf("%s refused changed the sheet: %s", what, dumpDiff(before, after))
			}
			if !kept.equal(e.memory("Sheet1", alice)) {
				t.Fatalf("%s refused changed the last sorts or the undo stacks", what)
			}
		}
		if stops < 5 {
			t.Fatalf("%s of %d units stopped %d times, want at least 5", what, whole, stops)
		}
	}
	sweep("the sort", func(deadline int64, found *int64) (*WriteError, int64) {
		e.in.mu.Lock()
		defer e.in.mu.Unlock()
		wb := e.in.wb
		tab := wb.tab("Sheet1")
		x := newTxn(wb, wb.seq+1, wb.seq, alice.s.ord)
		e.in.begin(x)
		x.overdue = nil
		// The sort calculates the sheet first when its index is out of
		// date, which has deadline checks of its own. That is done before
		// the budget starts, so the stops fall in what the sort does.
		x.budget = formula.NewBudget(nil)
		if err := x.calc(); err != nil {
			t.Fatal(err)
		}
		x.budget = unitBudget(deadline, found)
		err := x.sortRows(tab, []int32{0, 1}, ColID(tab.cols.ids[0]), false)
		used := x.budget.Used()
		wb.revert(&x.j)
		wb.settle()
		return err, used
	})
	if ack := alice.do(e.sortOp("Sheet1", 1, 2, 1, false)); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	e.wantText("Sheet1!B2", text(1))
	sweep("the undo of the sort", func(deadline int64, found *int64) (*WriteError, int64) {
		return stoppedUndoFound(t, e, alice, false, deadline, found)
	})
	wantUndo(t, "the undo", undo(t, alice), undoAck{Applied: 1})
	e.wantText("Sheet1!B1", text(1))
}

// unitBudget returns a budget whose deadline is found passed at its first
// check once it has counted deadline units. found, when it is not nil, is
// set to the units it had counted then.
func unitBudget(deadline int64, found *int64) *formula.Budget {
	var b *formula.Budget
	b = formula.NewBudget(func() bool {
		if b.Used() < deadline {
			return false
		}
		if found != nil && *found < 0 {
			*found = b.Used()
		}
		return true
	})
	return b
}

// stoppedUndo applies the newest entry of a person's undo or redo stack
// in a transaction of its own, without the action around it. The deadline
// is found passed at the first check of the budget, which comes every
// 4,096 units, once the budget has counted at least deadline units.
// inspect sees the workbook as the undo left it, before the transaction
// is taken back. stoppedUndo returns what the undo returned and the units
// the budget counted.
func stoppedUndo(t *testing.T, e *env, p *person, redo bool, deadline int64, inspect func(wb *Workbook)) (*WriteError, int64) {
	t.Helper()
	return stoppedUndoWith(t, e, p, redo, deadline, nil, inspect)
}

// stoppedUndoFound is stoppedUndo that also sets found to the units the
// budget had counted when it found the deadline passed.
func stoppedUndoFound(t *testing.T, e *env, p *person, redo bool, deadline int64, found *int64) (*WriteError, int64) {
	t.Helper()
	return stoppedUndoWith(t, e, p, redo, deadline, found, nil)
}

func stoppedUndoWith(t *testing.T, e *env, p *person, redo bool, deadline int64, found *int64, inspect func(wb *Workbook)) (*WriteError, int64) {
	t.Helper()
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	wb, u := e.in.wb, &p.s.undo
	stack := u.undo
	if redo {
		stack = u.redo
	}
	entry := stack[len(stack)-1]
	x := newTxn(wb, wb.seq+1, wb.seq, p.s.ord)
	e.in.begin(x)
	x.overdue = nil
	b := unitBudget(deadline, found)
	x.budget = b
	_, _, _, err := x.applyUndo(u, entry)
	if inspect != nil {
		inspect(wb)
	}
	wb.revert(&x.j)
	wb.settle()
	return err, b.Used()
}

// records counts the cell records of a tab that hold content.
func records(wb *Workbook, tab string) int {
	n := 0
	t := wb.tab(tab)
	if t == nil {
		return 0
	}
	for _, rd := range t.data {
		for _, s := range rd.cells {
			if !s.c.blank() {
				n++
			}
		}
	}
	return n
}

// The work an undo does after its deadline has a bound, in every part of
// it: bringing back cells, rows, columns and a tab, renaming the tab that
// returns under a taken name, ordering rows back and settling the
// deletions an entry in memory keeps apart. The deadline is found passed
// once the undo has counted 4,096 units; a cell, a row or a formula is at
// least one unit, so the undo stops after a few thousand of them at most,
// with the sheet and the stacks as they were. The same entry then applies
// in full.
func TestUndoEndsAtTheDeadlineInEveryPart(t *testing.T) {
	slow(t, "brings back tens of thousands of cells")
	rows := raced(20000, 2000)
	const cols = 10
	refused := func(t *testing.T, e *env, p *person, redo bool, deadline int64, inspect func(wb *Workbook)) {
		t.Helper()
		before, kept := e.dump(), e.memory("Sheet1", p)
		entry := p.top(redo)
		err, _ := stoppedUndo(t, e, p, redo, deadline, inspect)
		if err == nil || err.Limit != "recalculation_time" {
			t.Fatalf("the undo past its deadline: %v", err)
		}
		if after := e.dump(); after != before {
			t.Fatalf("the refused undo changed the sheet: %s", dumpDiff(before, after))
		}
		if !kept.equal(e.memory("Sheet1", p)) || p.top(redo) != entry {
			t.Fatal("the refused undo changed the undo stacks or the structural log")
		}
	}
	// The bound of what an undo brings back once the budget found the
	// deadline: the units up to the check and one unit of each thing.
	const bound = 4096
	t.Run("cells of a clear", func(t *testing.T) {
		e := start(t, unhurried)
		fillNumbers(e, "Sheet1", rows, cols)
		alice := e.person("Alice")
		if ack := alice.do(e.clearOp("Sheet1", 1, rows, 1, cols)); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		// The tick collects the blank records the clear left now, not
		// between the two looks at the sheet.
		e.collectBlanks(0)
		back := -1
		refused(t, e, alice, false, bound, func(wb *Workbook) { back = records(wb, "Sheet1") })
		t.Logf("the undo brought back %d of %d cells before it stopped", back, rows*cols)
		if back > bound {
			t.Errorf("the undo brought back %d cells after its deadline", back)
		}
		wantUndo(t, "the undo", undo(t, alice), undoAck{Applied: rows * cols})
		refused(t, e, alice, true, bound, func(wb *Workbook) { back = rows*cols - records(wb, "Sheet1") })
		if back > bound {
			t.Errorf("the redo cleared %d cells after its deadline", back)
		}
		wantUndo(t, "the redo", redo(t, alice), undoAck{Applied: rows * cols})
	})
	// Fewer rows than the units up to the check, each with many cells:
	// looking for the deleted ids in the axis is a unit each, putting them
	// in order, stamping them and giving them their places an eighth of a
	// unit each, and the undo meets the deadline among the rows it brings
	// back.
	t.Run("rows of a deletion", func(t *testing.T) {
		rows, cols := 2600, raced(100, 20)
		e := start(t, unhurried)
		fillNumbers(e, "Sheet1", rows, cols)
		alice := e.person("Alice")
		if ack := alice.do(e.deleteRowsOp("Sheet1", 1, rows-1)); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		back := -1
		refused(t, e, alice, false, bound, func(wb *Workbook) { back = records(wb, "Sheet1") - cols })
		t.Logf("the undo brought back %d of %d cells before it stopped", back, (rows-1)*cols)
		if back <= 0 || back > bound {
			t.Errorf("the undo brought back %d cells, want some and at most %d", back, bound)
		}
		wantUndo(t, "the undo", undo(t, alice), undoAck{Applied: 1})
		e.want("Sheet1!B1", "1002")
	})
	t.Run("columns of a deletion", func(t *testing.T) {
		e := start(t, unhurried)
		fillNumbers(e, "Sheet1", rows, cols)
		alice := e.person("Alice")
		if ack := alice.do(e.deleteColsOp("Sheet1", 1, cols-1)); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		back := -1
		refused(t, e, alice, false, bound, func(wb *Workbook) { back = records(wb, "Sheet1") - rows })
		t.Logf("the undo brought back %d of %d cells before it stopped", back, rows*(cols-1))
		if back > bound {
			t.Errorf("the undo brought back %d cells after its deadline", back)
		}
		wantUndo(t, "the undo", undo(t, alice), undoAck{Applied: 1})
		e.want("Sheet1!B1", "1002")
	})
	t.Run("a sort", func(t *testing.T) {
		e := start(t, unhurried)
		fillNumbers(e, "Sheet1", rows, 1)
		alice := e.person("Alice")
		if ack := alice.do(e.sortOp("Sheet1", 1, rows, 1, true)); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		refused(t, e, alice, false, bound, nil)
		wantUndo(t, "the undo", undo(t, alice), undoAck{Applied: 1})
		e.want("Sheet1!A1", "1001")
	})
	// The tab returns as "Data (restored)", and its own formulas that name
	// it are rewritten for that name. The rows are few and the formulas
	// long, so the rename is most of the work.
	t.Run("a tab that returns under a taken name", func(t *testing.T) {
		e := start(t, unhurried)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Data","at":"A1","rows":[[1],[2],[3]]}]}`)
		const formulas = 40
		e.fillColumn("Data", "B", formulas, func(row int) string {
			return longRefs(200, func(i int) string { return fmt.Sprintf("Data!$A$%d", 1+(i+row)%3) })
		})
		alice, bob := e.person("Alice"), e.person("Bob")
		if ack := alice.do(fmt.Sprintf(`{"op":"delete_tab","tab":%d}`, e.tabID("Data"))); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		if ack := bob.do(`{"op":"add_tab","name":"Data"}`); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		refused(t, e, alice, false, bound, nil)
		wantUndo(t, "the undo", undo(t, alice), undoAck{Applied: 1})
		want := longRefs(200, func(i int) string { return fmt.Sprintf("'Data (restored)'!$A$%d", 1+(i+1)%3) })
		e.wantText("'Data (restored)'!B1", want)
	})
	// Bob deletes rows of the tab that Alice's cleared formulas read, one
	// by one, after her clear. Her entry stays in memory and keeps each
	// deletion apart; her undo settles them on a copy of the entry.
	t.Run("deletions an entry in memory keeps apart", func(t *testing.T) {
		e := start(t, unhurried)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"D"},{"op":"write","tab":"D","at":"A1","rows":[[1],[2],[3]]}]}`)
		const formulas = 4
		texts := make([]string, formulas)
		for row := range texts {
			texts[row] = longRefs(700, func(i int) string { return fmt.Sprintf("D!$A$%d", 40+(i+row)%850) })
		}
		e.fillColumn("Sheet1", "A", formulas, func(row int) string { return texts[row-1] })
		alice, bob := e.person("Alice"), e.person("Bob")
		if ack := alice.do(e.clearOp("Sheet1", 1, formulas, 1, 1)); status(ack) != "applied" || alice.spilled(0) != nil {
			t.Fatalf("the clear: %s, in a scratch file: %v", status(ack), alice.spilled(0) != nil)
		}
		const deletions = 30
		for i := 0; i < deletions; i++ {
			if ack := bob.do(e.deleteRowsOp("D", 5, 1)); status(ack) != "applied" {
				t.Fatal(status(ack))
			}
		}
		gaps := func() int {
			entry := alice.top(false)
			e.in.mu.RLock()
			defer e.in.mu.RUnlock()
			return len(entry.gaps)
		}
		if n := gaps(); n != deletions || alice.spilled(0) != nil {
			t.Fatalf("the entry keeps %d deletions apart, want %d, in a scratch file: %v", n, deletions, alice.spilled(0) != nil)
		}
		// Settling a deletion rewrites the four formulas, some 1,800 units.
		// Copying the entry comes before, and the deadline falls among
		// the deletions.
		const deadline = 4 * 4096
		if err, whole := stoppedUndo(t, e, alice, false, 1<<62, nil); err != nil || whole < 3*deadline {
			t.Fatalf("the whole undo: %v, after %d units, want more than %d", err, whole, 3*deadline)
		}
		refused(t, e, alice, false, deadline, nil)
		if n := gaps(); n != deletions {
			t.Fatalf("the refused undo left the entry with %d deletions apart, want %d", n, deletions)
		}
		wantUndo(t, "the undo", undo(t, alice), undoAck{Applied: formulas})
		e.wantText("Sheet1!A1", longRefs(700, func(i int) string { return fmt.Sprintf("D!$A$%d", 40-deletions+i%850) }))
	})
}
