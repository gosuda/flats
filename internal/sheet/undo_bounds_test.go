package sheet

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// settledHeap returns the heap in use once the table of shared formulas
// holds only those of the live sheet. What earlier states left in that
// table is not undo's memory.
func settledHeap(e *env) int64 {
	e.in.mu.Lock()
	e.in.wb.pruneFormulas()
	e.in.mu.Unlock()
	return int64(heapInUse())
}

// longRefs returns a formula of n references that fn writes, joined by "+".
func longRefs(n int, fn func(i int) string) string {
	var sb strings.Builder
	sb.WriteString("=")
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte('+')
		}
		sb.WriteString(fn(i))
	}
	return sb.String()
}

// fillColumn writes what text returns for each 1-based row into count cells
// of a column, from row 1.
func (e *env) fillColumn(tab, col string, count int, text func(row int) string) {
	e.t.Helper()
	rows := make([]string, count)
	for i := range rows {
		rows[i] = fmt.Sprintf("[%q]", text(i+1))
	}
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":%q,"at":"%s1","rows":[%s]}]}`, tab, col, strings.Join(rows, ",")))
}

func (e *env) texts(tab, col string, count int) []string {
	e.t.Helper()
	out := make([]string, count)
	for i := range out {
		out[i] = e.text(fmt.Sprintf("%s!%s%d", tab, col, i+1))
	}
	return out
}

// wantUndoMemory checks the heap against the undo budgets of the sessions
// that hold entries, and the pool's count against the payloads.
func wantUndoMemory(t *testing.T, e *env, grown int64, sessions int) {
	t.Helper()
	pool, stacks := e.residentUndo()
	t.Logf("the heap grew by %d KiB; the payloads in memory count %d KiB", grown>>10, stacks>>10)
	if pool != stacks {
		t.Errorf("the pool counts %d bytes of payloads in memory and the stacks hold %d", pool, stacks)
	}
	if limit := int64(sessions) * undoSessionBytes; stacks > limit {
		t.Errorf("%d sessions hold %d bytes of payloads in memory, over %d", sessions, stacks, limit)
	}
	// The heap may hold what the budgets allow, and as much again for what
	// is not undo's: the sheet's own records of the edits.
	if limit := 2 * int64(sessions) * undoSessionBytes; measures && grown > limit {
		t.Errorf("the heap grew by %d KiB while the undo budgets of %d sessions allow %d KiB, and the payloads in memory count %d KiB",
			grown>>10, sessions, int64(sessions)*undoSessionBytes>>10, stacks>>10)
	}
}

// An entry that was small when its action committed stays inside the undo
// budgets when later structural edits change what it holds: the heap is
// measured, not the entry's own count.
func TestUndoFollowingKeepsMemoryBounds(t *testing.T) {
	slow(t, "parses hundreds of formulas of 7,000 characters")
	// The formulas of one column share one stored text, and one parsed
	// formula with it. Under the race detector the payloads are counted,
	// spilled and followed as otherwise; the heap and what a step
	// allocates are not measured.
	const cells = 120
	t.Run("a rename of the tab that the formulas a deletion changed name", func(t *testing.T) {
		e := start(t)
		long := longRefs(800, func(i int) string { return fmt.Sprintf("D!$A$%d", 5+i) })
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"D"},{"op":"write","tab":"D","at":"A1","rows":[[1],[2],[3],[4],[5],[6],[7],[8],[9]]}]}`)
		e.fillColumn("Sheet1", "A", cells, func(int) string { return long })
		alice, bob := e.person("Alice"), e.person("Bob")
		// The deletion changes every formula, and its entry holds the
		// earlier form of each: one parsed formula, shared.
		if ack := alice.do(e.deleteRowsOp("D", 1, 1)); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		if alice.spilled(0) != nil {
			t.Fatal("the entry is in a scratch file from the start")
		}
		before := settledHeap(e)
		if ack := bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"E"}`, e.tabID("D"))); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		wantUndoMemory(t, e, settledHeap(e)-before, 2)
		wantUndo(t, "undo of the deletion", undo(t, alice), undoAck{Applied: 1})
		want := strings.ReplaceAll(long, "D!", "E!")
		for _, addr := range []string{"Sheet1!A1", "Sheet1!A60", "Sheet1!A120"} {
			e.wantText(addr, want)
		}
	})
	t.Run("an insert that gives every held formula a text of its own", func(t *testing.T) {
		// Filled down: the cell in row r reads rows r to r+899 of D, and
		// all the cells have one stored text.
		build := func(t *testing.T) *env {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"D"},{"op":"insert_rows","tab":"D","at":1,"count":1500}]}`)
			e.fillColumn("Sheet1", "A", cells, func(row int) string {
				return longRefs(900, func(i int) string { return fmt.Sprintf("D!A%d", row+i) })
			})
			return e
		}
		e := build(t)
		alice, bob := e.person("Alice"), e.person("Bob")
		if ack := alice.do(e.clearOp("Sheet1", 1, cells, 1, 1)); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		if alice.spilled(0) != nil {
			t.Fatal("the entry is in a scratch file from the start")
		}
		before := settledHeap(e)
		// A row put before row 600 of D moves another number of the rows
		// each cell reads.
		insert := e.insertRowsOp("D", 600, 1)
		work := allocated()
		if ack := bob.do(insert); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		work = allocated() - work
		wantUndoMemory(t, e, settledHeap(e)-before, 2)
		// Giving each of the cells its own formula in memory allocates
		// about 0.3 MiB for each. The payload goes to its file before that,
		// and follows the insert at its undo.
		if alice.spilled(0) == nil || (measures && work > undoSessionBytes) {
			t.Errorf("the insert allocated %d KiB; the payload is in a scratch file: %v", work>>10, alice.spilled(0) != nil)
		}
		wantUndo(t, "undo of the clear", undo(t, alice), undoAck{Applied: cells})
		// The same insert on a sheet whose formulas stayed in their cells.
		control := build(t)
		control.person("Bob").do(control.insertRowsOp("D", 600, 1))
		got, want := e.texts("Sheet1", "A", cells), control.texts("Sheet1", "A", cells)
		distinct := map[string]bool{}
		for i := range want {
			distinct[want[i]] = true
			if got[i] != want[i] {
				t.Fatalf("A%d came back as a formula of %d characters that differs from the one that stayed in its cell", i+1, len(got[i]))
			}
		}
		if len(distinct) != cells {
			t.Fatalf("the insert left %d texts for %d formulas; the case needs one each", len(distinct), cells)
		}
	})
	t.Run("deletions of rows whose ids lie apart", func(t *testing.T) {
		e := start(t)
		rows, entries, deletions, each := raced(20000, 10000), raced(100, 20), raced(20, 5), raced(800, 400)
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"add_tab","name":"D"},{"op":"insert_rows","tab":"D","at":1,"count":%d}]}`, rows))
		rng := rand.New(rand.NewPCG(7, 7))
		for from := 0; from < rows; from += 10000 {
			values := make([]string, 10000)
			for i := range values {
				values[i] = fmt.Sprintf("[%d]", rng.IntN(1<<30))
			}
			e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"D","at":"A%d","rows":[%s]}]}`, from+1, strings.Join(values, ",")))
		}
		e.fillColumn("Sheet1", "A", entries, func(int) string { return "=D!$A$1" })
		alice, bob := e.person("Alice"), e.person("Bob")
		// The sort leaves neighbouring rows with ids that lie apart.
		if ack := bob.do(e.sortOp("D", 1, rows, 1, false)); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		// Each of Alice's entries holds one formula that names D.
		for i := 1; i <= entries; i++ {
			alice.do(e.clearOp("Sheet1", i, 1, 1, 1))
		}
		before := settledHeap(e)
		for i := 0; i < deletions; i++ {
			if ack := bob.do(e.deleteRowsOp("D", 10, each)); status(ack) != "applied" {
				t.Fatal(status(ack))
			}
		}
		wantUndoMemory(t, e, settledHeap(e)-before, 2)
		for i := entries; i >= 1; i-- {
			wantUndo(t, fmt.Sprintf("undo %d", i), undo(t, alice), undoAck{Applied: 1})
		}
		// Row 1 of D stayed, so every formula reads it as before.
		for _, addr := range []string{"Sheet1!A1", fmt.Sprintf("Sheet1!A%d", entries/2), fmt.Sprintf("Sheet1!A%d", entries)} {
			e.wantText(addr, "=D!$A$1")
		}
	})
}

// stalls measures how long reads of the sheet wait while fn runs: the
// longest time one read took. The workbook lock is what a read waits for.
func stalls(t *testing.T, e *env, fn func()) (longest time.Duration) {
	t.Helper()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			if _, err := e.in.Read(context.Background(), ReadQuery{Tab: "Sheet1", Range: "A1"}); err != nil {
				t.Errorf("read: %v", err)
				return
			}
			if took := time.Since(start); took > longest {
				longest = took
			}
			time.Sleep(time.Millisecond)
		}
	}()
	fn()
	close(stop)
	wg.Wait()
	return longest
}

// allocated returns the bytes the process has allocated so far. Work on
// formulas allocates in proportion to the text it rewrites.
func allocated() int64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.TotalAlloc)
}

// catchUp builds the sheet of the catch-up cases: a column of long formulas
// that name the tab D, each with a text of its own, which Alice clears. The
// payload of the clear goes to a scratch file by its size. Before it,
// Alice has made a small edit.
func catchUp(t *testing.T, formulas int) (e *env, alice, bob *person, texts []string) {
	t.Helper()
	e = start(t, unhurried)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"D"},{"op":"write","tab":"D","at":"A1","rows":[[1],[2],[3]]}]}`)
	texts = make([]string, formulas)
	for row := range texts {
		texts[row] = longRefs(800, func(i int) string { return fmt.Sprintf("D!$A$%d", 5+(i+row)%850) })
	}
	e.fillColumn("Sheet1", "A", formulas, func(row int) string { return texts[row-1] })
	alice, bob = e.person("Alice"), e.person("Bob")
	alice.set("C1", "before")
	if ack := alice.do(e.clearOp("Sheet1", 1, formulas, 1, 1)); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	if alice.spilled(1) == nil {
		t.Fatalf("the clear of %d long formulas stays in memory", formulas)
	}
	return e, alice, bob, texts
}

// measured runs an undo and returns its ack with the time it took, the
// longest wait of a concurrent read and the bytes the process allocated.
func measured(t *testing.T, e *env, p *person) (u *undoAck, took, stall time.Duration, work int64) {
	t.Helper()
	before, begun := allocated(), time.Now()
	stall = stalls(t, e, func() { u = undo(t, p) })
	return u, time.Since(begun), stall, allocated() - before
}

// An undo whose payload waits in a scratch file catches up on the edits
// made since inside a bound of work, however long the history is.
func TestUndoCatchUpIsBounded(t *testing.T) {
	slow(t, "rewrites formulas of 7,000 characters")
	// 10,000 renames over 400 formulas at full size, 1,500 over 60 by
	// default. Putting 1,000 such formulas into a sheet takes most of the
	// 10 seconds an action has on a busy machine, with no history to
	// follow, so the full size stays well below that. Under the race
	// detector it is 300 renames over 12 formulas: the outcomes are the
	// same, and what the undo allocates and how long it takes are not
	// measured.
	formulas, renames := raced(sized(400, 60), 12), raced(sized(10000, 1500), 300)
	t.Run("a tab renamed back and forth", func(t *testing.T) {
		e, alice, bob, texts := catchUp(t, formulas)
		id := e.tabID("D")
		for i := 0; i < renames+1; i++ {
			name := "E"
			if i%2 == 1 {
				name = "D"
			}
			if ack := bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":%q}`, id, name)); status(ack) != "applied" {
				t.Fatal(status(ack))
			}
		}
		u, took, stall, work := measured(t, e, alice)
		t.Logf("undo after %d renames over %d formulas: %v, the longest read waited %v, %d MiB allocated", renames+1, formulas, took, stall, work>>20)
		wantUndo(t, "undo of the clear", u, undoAck{Applied: formulas})
		for _, row := range []int{1, formulas / 2, formulas} {
			e.wantText(fmt.Sprintf("Sheet1!A%d", row), strings.ReplaceAll(texts[row-1], "D!", "E!"))
		}
		// Rewriting one formula once allocates about 0.3 MiB. Loading the
		// payload and following the renames as one edit stays near that for
		// each formula; following each rename takes it once per rename.
		if limit := int64(formulas) * (8 << 20); measures && work > limit {
			t.Errorf("the undo allocated %d MiB for %d formulas, over %d MiB", work>>20, formulas, limit>>20)
		}
		if limit := 30 * time.Second; measures && (took > limit || stall > limit) {
			t.Errorf("the undo took %v and a read waited %v, over %v", took, stall, limit)
		}
	})
	// A rename and an insert in turn cannot be followed as one edit: every
	// insert rewrites every formula. The work passes its bound and the
	// entry expires (spec rule 23).
	t.Run("renames and inserts in turn", func(t *testing.T) {
		e, alice, bob, _ := catchUp(t, formulas)
		id := e.tabID("D")
		// Fewer formulas need more rounds to pass the bound of work.
		rounds := raced(sized(2000, 300), 1200)
		for i := 0; i < rounds; i++ {
			name := "E"
			if i%2 == 1 {
				name = "D"
			}
			if ack := bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":%q}`, id, name)); status(ack) != "applied" {
				t.Fatal(status(ack))
			}
			if ack := bob.do(e.insertRowsOp(name, 1, 1)); status(ack) != "applied" {
				t.Fatal(status(ack))
			}
		}
		seq := e.seq()
		u, took, stall, work := measured(t, e, alice)
		t.Logf("undo after %d renames and inserts over %d formulas: %v, the longest read waited %v, %d MiB allocated", rounds, formulas, took, stall, work>>20)
		wantUndo(t, "undo of the clear", u, undoAck{Skipped: 1, Reason: "lost"})
		if e.seq() != seq || e.text("Sheet1!A1") != "" {
			t.Fatalf("an undo that expired changed the sheet: sequence %d, was %d; A1 %q", e.seq(), seq, e.text("Sheet1!A1"))
		}
		// The bound is 2 MiB units, each at most 16 bytes of formula text
		// written and read again, which allocates about 25 times the text:
		// near 0.8 GiB. Loading the payload comes to 0.3 MiB a formula.
		// Following every edit would allocate that for each insert.
		if limit := int64(2<<30) + int64(formulas)<<20; measures && work > limit {
			t.Errorf("the undo allocated %d MiB before it expired, over %d MiB", work>>20, limit>>20)
		}
		// The deadline of the action, 10 seconds, ends the work wherever
		// the units have not. A read waits for the workbook lock, which the
		// undo holds for that long at most, on a machine that is busy too.
		if limit := 30 * time.Second; measures && (took > limit || stall > limit) {
			t.Errorf("the undo took %v and a read waited %v, over %v", took, stall, limit)
		}
		// The entry has left its stack, and its file is gone. The next undo
		// reaches the entry before it and does none of that work again.
		if files, _ := e.scratch(); files != 0 {
			t.Errorf("%d scratch files after the entry expired", files)
		}
		u, took, _, work = measured(t, e, alice)
		wantUndo(t, "undo of the edit before", u, undoAck{Applied: 1})
		if e.text("Sheet1!C1") != "" {
			t.Fatalf("C1 = %q after its undo", e.text("Sheet1!C1"))
		}
		if measures && (work > 64<<20 || took > 5*time.Second) {
			t.Errorf("the undo after the expired one took %v and allocated %d MiB", took, work>>20)
		}
	})
}

// The bound of a catch-up is the smaller of its units of work and the
// deadline of the undo's action. Either way the entry expires: its undo is
// skipped as lost, nothing in the sheet changes, and the person's other
// entries stay.
func TestUndoCatchUpExpires(t *testing.T) {
	for name, bound := range map[string]func(e *env){
		"the units of work": func(e *env) { e.in.undoPool().followMax = 20000 },
		// The deadline passes after the undo has read the payload, before
		// the catch-up. An undo that comes after its deadline is refused
		// before it reads anything, and one whose deadline passes while it
		// reads the payload is refused there; in both its entry stays
		// (TestUndoStopsWhileItReadsItsPayload).
		"the deadline of the action": func(e *env) {
			e.in.undoPool().followMax = 1 << 60
			e.in.opts.RecalcLimit = 20 * time.Millisecond
			spillReadWatch = func(int64, int64) { time.Sleep(40 * time.Millisecond) }
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { spillReadWatch = nil })
			e, alice, bob, _ := catchUp(t, 8)
			for i := 0; i < 6; i++ {
				bob.do(e.insertRowsOp("D", 1, 1))
			}
			limit := e.in.opts.RecalcLimit
			e.in.mu.Lock()
			bound(e)
			e.in.mu.Unlock()
			seq := e.seq()
			wantUndo(t, "undo of the clear", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
			if e.seq() != seq || e.text("Sheet1!A1") != "" {
				t.Fatalf("an undo that expired changed the sheet")
			}
			e.in.mu.Lock()
			e.in.opts.RecalcLimit, e.in.undoPool().fault = limit, nil
			e.in.mu.Unlock()
			wantUndo(t, "undo of the edit before", undo(t, alice), undoAck{Applied: 1})
			wantUndo(t, "undo with nothing left", undo(t, alice), undoAck{})
		})
	}
	// Inside the bound the same history is followed.
	t.Run("inside both", func(t *testing.T) {
		e, alice, bob, _ := catchUp(t, 8)
		for i := 0; i < 6; i++ {
			bob.do(e.insertRowsOp("D", 1, 1))
		}
		wantUndo(t, "undo of the clear", undo(t, alice), undoAck{Applied: 8})
		// Every reference is to a row 6 further down.
		e.wantText("Sheet1!A1", longRefs(800, func(i int) string { return fmt.Sprintf("D!$A$%d", 11+i%850) }))
	})
	// A payload in memory that cannot go to a scratch file when following
	// makes it too large expires as well: the edit that made it grow has
	// committed and cannot be refused.
	t.Run("a payload that cannot leave memory", func(t *testing.T) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"D"},{"op":"insert_rows","tab":"D","at":1,"count":1500}]}`)
		e.fillColumn("Sheet1", "A", 40, func(row int) string {
			return longRefs(900, func(i int) string { return fmt.Sprintf("D!A%d", row+i) })
		})
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.set("C1", "before")
		alice.do(e.clearOp("Sheet1", 1, 40, 1, 1))
		if alice.spilled(1) != nil {
			t.Fatal("the entry is in a scratch file from the start")
		}
		e.in.mu.Lock()
		e.in.undoPool().fault = func(op string) error {
			if op == "write" {
				return errors.New("no space left on device")
			}
			return nil
		}
		e.in.mu.Unlock()
		if ack := bob.do(e.insertRowsOp("D", 600, 1)); status(ack) != "applied" {
			t.Fatalf("the insert: %s", status(ack))
		}
		if pool, stacks := e.residentUndo(); pool != stacks || stacks > undoSpillBytes {
			t.Fatalf("the pool counts %d bytes and the stacks hold %d after the payload expired", pool, stacks)
		}
		wantUndo(t, "undo of the clear", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
		wantUndo(t, "undo of the edit before", undo(t, alice), undoAck{Applied: 1})
	})
}

// watchCatchUps collects the run of every catch-up that ends while the
// test goes on.
func watchCatchUps(t *testing.T) *[]*followRun {
	t.Helper()
	runs := &[]*followRun{}
	followWatch = func(c *followRun) { *runs = append(*runs, c) }
	t.Cleanup(func() { followWatch = nil })
	return runs
}

// storedLen returns the bytes of the stored form of a formula text.
func storedLen(t *testing.T, text string) int64 {
	t.Helper()
	f, err := formula.Parse(text, formula.Pos{}, formula.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(f.Stored()))
}

// wantStopped checks a catch-up that passed a bound: it stopped, it made
// no rewrite and charged no work after that, and it went past the bound by
// the work of one rewrite at most. byWork says which bound it was: the
// units of work, or the deadline of the action.
func wantStopped(t *testing.T, c *followRun, byWork bool, longest int64) {
	t.Helper()
	if !c.stop {
		t.Fatal("the catch-up did not stop")
	}
	if c.late != 0 || c.over != 0 {
		t.Errorf("the catch-up made %d rewrites and charged %d units after it had stopped", c.late, c.over)
	}
	if byWork != (c.left < 0) || byWork == c.budget.Exceeded() {
		t.Errorf("the catch-up stopped with %d units left and the deadline passed: %v; want it stopped by the units: %v", c.left, c.budget.Exceeded(), byWork)
	}
	if limit := 1 + longest/16; -c.left > limit {
		t.Errorf("the catch-up went %d units past its bound, and one rewrite is %d at most", -c.left, limit)
	}
}

// manyNames builds a sheet for catch-ups that cannot follow renames as one
// edit. It holds a column of long formulas that name the tab D and 40 tabs
// that do not exist, which is more names than an entry keeps. Alice clears the
// column, and the payload goes to a scratch file by its size. Before it,
// Alice has made a small edit.
func manyNames(t *testing.T, formulas int) (e *env, alice *person, texts []string) {
	t.Helper()
	e = start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"D"},{"op":"write","tab":"D","at":"A1","rows":[[1],[2],[3]]}]}`)
	texts = make([]string, formulas)
	for row := range texts {
		texts[row] = longRefs(700, func(i int) string {
			if i < 40 {
				return fmt.Sprintf("None%d!A1", i)
			}
			return fmt.Sprintf("D!$A$%d", 5+(i+row)%850)
		})
	}
	e.fillColumn("Sheet1", "A", formulas, func(row int) string { return texts[row-1] })
	alice = e.person("Alice")
	alice.set("C1", "before")
	if ack := alice.do(e.clearOp("Sheet1", 1, formulas, 1, 1)); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	e.in.mu.RLock()
	entry := alice.s.undo.undo[1]
	spilled, all := entry.file != nil, entry.deps != nil && entry.deps.allNames
	e.in.mu.RUnlock()
	if !spilled || !all {
		t.Fatalf("the clear of %d formulas of 41 names: in a scratch file %v, depends on every name %v", formulas, spilled, all)
	}
	return e, alice, texts
}

// renames sends n renames of the tab D, to E and back in turn, in calls of
// 500. n is even, so the tab is D again at the end.
func (e *env) renames(n int) {
	e.t.Helper()
	for done := 0; done < n; {
		var ops []string
		for ; len(ops) < 500 && done < n; done++ {
			from, to := "D", "E"
			if done%2 == 1 {
				from, to = "E", "D"
			}
			ops = append(ops, fmt.Sprintf(`{"op":"rename_tab","tab":%q,"name":%q}`, from, to))
		}
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	}
}

// A catch-up that passes its bound stops between two rewrites, wherever it
// is: in a run of renames that an entry of more names than it keeps
// follows one by one, each a rewrite of every formula, and among the cells
// a sort moves. It does not finish the formula, the edit or the batch it
// is at. The entry expires, and the next undo reaches the entry before it
// without that work.
func TestUndoCatchUpStopsBetweenRewrites(t *testing.T) {
	slow(t, "rewrites formulas of 7,000 characters")
	// after checks the undo that expired and the one after it.
	after := func(t *testing.T, e *env, alice *person, runs *[]*followRun, seq uint64, u *undoAck) {
		t.Helper()
		wantUndo(t, "undo of the clear", u, undoAck{Skipped: 1, Reason: "lost"})
		if e.seq() != seq || e.text("Sheet1!A1") != "" {
			t.Fatalf("an undo that expired changed the sheet: sequence %d, was %d; A1 %q", e.seq(), seq, e.text("Sheet1!A1"))
		}
		if files, _ := e.scratch(); files != 0 {
			t.Errorf("%d scratch files after the entry expired", files)
		}
		before := allocated()
		wantUndo(t, "undo of the edit before", undo(t, alice), undoAck{Applied: 1})
		if e.text("Sheet1!C1") != "" {
			t.Fatalf("C1 = %q after its undo", e.text("Sheet1!C1"))
		}
		if len(*runs) != 1 {
			t.Errorf("%d catch-ups ran for the two undos, want the one that expired", len(*runs))
		}
		if work := allocated() - before; measures && work > 16<<20 {
			t.Errorf("the undo after the expired one allocated %d MiB", work>>20)
		}
	}
	// The renames wait in memory as one batch of the log, 9,000 at the
	// full size. Each is a rewrite of each formula, so the first formula
	// alone takes 9,000, of which the bound of work allows about 5,000.
	const formulas = 8
	renames := raced(sized(9000, 3000), 1000)
	for _, c := range []struct {
		name   string
		byWork bool
		bound  func(e *env)
	}{
		// The default run has a smaller bound of work, for fewer renames.
		{"a run of renames, the units of work", true, func(e *env) {
			if !fullSize {
				e.in.undoPool().followMax = 1 << 17
			}
		}},
		{"a run of renames, the deadline of the action", false, func(e *env) {
			e.in.undoPool().followMax = 1 << 60
			e.in.opts.RecalcLimit = 20 * time.Millisecond
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, alice, texts := manyNames(t, formulas)
			e.renames(renames)
			if segments, open := e.editLog(); segments != 0 || open != renames {
				t.Fatalf("the log holds %d files and %d edits in memory, want the %d renames in memory", segments, open, renames)
			}
			runs := watchCatchUps(t)
			e.in.mu.Lock()
			c.bound(e)
			bound := e.in.undoPool().followMax
			e.in.mu.Unlock()
			seq := e.seq()
			u, _, _, work := measured(t, e, alice)
			if len(*runs) != 1 {
				t.Fatalf("%d catch-ups ran, want 1", len(*runs))
			}
			run := (*runs)[0]
			longest := int64(0)
			for _, text := range texts {
				longest = max(longest, storedLen(t, text))
			}
			wantStopped(t, run, c.byWork, longest)
			// Following every rename takes one rewrite of each formula for
			// each of them. The run stopped long before that.
			whole := int64(renames) * formulas * (1 + longest/16)
			used := run.budget.Used()
			t.Logf("the catch-up used %d units of %d for the whole history and allocated %d MiB", used, whole, work>>20)
			if c.byWork {
				// A rename of a formula allocates under 1 KiB for each unit
				// of its work. Reading the payload comes on top.
				if limit := (bound+1+longest/16)<<10 + formulas<<20; measures && work > limit {
					t.Errorf("the undo allocated %d MiB before it expired, over %d MiB", work>>20, limit>>20)
				}
			} else if used > whole/2 {
				t.Errorf("the catch-up used %d units of the %d the whole history takes before the deadline stopped it", used, whole)
			}
			after(t, e, alice, runs, seq, u)
		})
	}
	// A sort moves every row of the tab the formulas lay in, and following
	// it looks through each formula. The bound of work ends that between
	// two formulas.
	t.Run("a sort, the units of work", func(t *testing.T) {
		const formulas = 60
		e, alice, _, texts := catchUp(t, formulas)
		bob := e.person("Bob")
		e.fillColumn("Sheet1", "B", formulas, func(row int) string { return fmt.Sprint(row) })
		for i := 0; i < 4; i++ {
			if ack := bob.do(e.sortOp("Sheet1", 1, formulas, 2, i%2 == 0)); status(ack) != "applied" {
				t.Fatal(status(ack))
			}
		}
		runs := watchCatchUps(t)
		e.in.mu.Lock()
		// One look through a formula of 7,000 characters is some 30
		// units, and a sort looks through 60.
		e.in.undoPool().followMax = 1000
		e.in.mu.Unlock()
		seq := e.seq()
		u := undo(t, alice)
		if len(*runs) != 1 {
			t.Fatalf("%d catch-ups ran, want 1", len(*runs))
		}
		longest := int64(0)
		for _, text := range texts {
			longest = max(longest, storedLen(t, text))
		}
		wantStopped(t, (*runs)[0], true, longest)
		after(t, e, alice, runs, seq, u)
	})
	// Inside the bounds the same histories are followed.
	t.Run("inside the bounds", func(t *testing.T) {
		e, alice, texts := manyNames(t, formulas)
		e.renames(40)
		runs := watchCatchUps(t)
		wantUndo(t, "undo of the clear", undo(t, alice), undoAck{Applied: formulas})
		if run := (*runs)[0]; run.stop || run.late != 0 || run.over != 0 {
			t.Fatalf("the catch-up stopped: %v, with %d rewrites and %d units after", run.stop, run.late, run.over)
		}
		for _, row := range []int{1, formulas} {
			e.wantText(fmt.Sprintf("Sheet1!A%d", row), texts[row-1])
		}
	})
}

// The payloads in memory follow the structural edits of an action as it
// commits, under the workbook lock. One action can hold thousands of edits,
// each a rewrite of every formula a payload holds, so that work has a bound
// for the action. The payloads follow inside it. One that has not followed
// every edit by then goes to a scratch file and follows the rest at its
// undo, with the same result.
func TestUndoFollowingInMemoryIsBoundedForAnAction(t *testing.T) {
	slow(t, "rewrites formulas of 7,000 characters")
	const formulas = 4
	// setup leaves Alice a small edit and a clear of long formulas that
	// stays in memory, and bounds the work of an action at 65,536 units:
	// one rewrite of the four formulas is about 2,000.
	setup := func(t *testing.T, text func(row, i int) string) (e *env, alice *person, texts []string) {
		t.Helper()
		e = start(t, unhurried)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"D"},{"op":"write","tab":"D","at":"A1","rows":[[1],[2],[3]]}]}`)
		texts = make([]string, formulas)
		for row := range texts {
			texts[row] = longRefs(700, func(i int) string { return text(row, i) })
		}
		e.fillColumn("Sheet1", "A", formulas, func(row int) string { return texts[row-1] })
		alice = e.person("Alice")
		alice.set("C1", "before")
		if ack := alice.do(e.clearOp("Sheet1", 1, formulas, 1, 1)); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		if alice.spilled(1) != nil {
			t.Fatalf("the clear of %d long formulas is in a scratch file from the start", formulas)
		}
		e.in.mu.Lock()
		e.in.undoPool().followMax = 1 << 16
		e.in.mu.Unlock()
		return e, alice, texts
	}
	// waits returns the edits the entry of the clear waits for in the log.
	waits := func(e *env, alice *person) int {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		entry := alice.s.undo.undo[1]
		if entry.file == nil || entry.wait == nil {
			return -1
		}
		return entry.wait.reads
	}
	// undone restores the bound of a catch-up and undoes the clear.
	undone := func(t *testing.T, e *env, alice *person) {
		t.Helper()
		e.in.mu.Lock()
		e.in.undoPool().followMax = undoFollowUnits
		e.in.mu.Unlock()
		wantUndo(t, "undo of the clear", undo(t, alice), undoAck{Applied: formulas})
	}
	t.Run("a call of many inserts", func(t *testing.T) {
		e, alice, _ := setup(t, func(row, i int) string { return fmt.Sprintf("D!$A$%d", 5+(i+row)%850) })
		// Few enough for the catch-up at the undo, which has the full
		// bound of 2,097,152 units.
		const inserts = 400
		ops := make([]string, inserts)
		for i := range ops {
			ops[i] = `{"op":"insert_rows","tab":"D","at":1,"count":1}`
		}
		before := allocated()
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
		work := allocated() - before
		rest := waits(e, alice)
		t.Logf("a call of %d inserts allocated %d MiB; the payload waits for %d of them in a scratch file", inserts, work>>20, rest)
		// Each insert rewrites the four formulas, about 2,000 units. The
		// payload follows some 30 of them in memory and waits for the rest.
		if rest < inserts*9/10 || rest >= inserts {
			t.Fatalf("the payload waits for %d of %d inserts, want all but the first few", rest, inserts)
		}
		// Following every insert in memory allocates about 1.2 MiB for
		// each, near 500 MiB for the call.
		if limit := int64(128 << 20); measures && work > limit {
			t.Errorf("the call allocated %d MiB, over %d MiB", work>>20, limit>>20)
		}
		undone(t, e, alice)
		for _, row := range []int{1, formulas} {
			e.wantText(fmt.Sprintf("Sheet1!A%d", row), longRefs(700, func(i int) string { return fmt.Sprintf("D!$A$%d", inserts+5+(i+row-1)%850) }))
		}
	})
	t.Run("a call of many renames, for a payload of more names than it keeps", func(t *testing.T) {
		e, alice, texts := setup(t, func(row, i int) string {
			if i < 40 {
				return fmt.Sprintf("None%d!A1", i)
			}
			return fmt.Sprintf("D!$A$%d", 5+(i+row)%850)
		})
		const renames = 600
		ops := make([]string, renames)
		for i := range ops {
			from, to := "D", "E"
			if i%2 == 1 {
				from, to = "E", "D"
			}
			ops[i] = fmt.Sprintf(`{"op":"rename_tab","tab":%q,"name":%q}`, from, to)
		}
		before := allocated()
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
		work := allocated() - before
		rest := waits(e, alice)
		t.Logf("a call of %d renames allocated %d MiB; the payload waits for %d of them in a scratch file", renames, work>>20, rest)
		if rest < renames*9/10 || rest >= renames {
			t.Fatalf("the payload waits for %d of %d renames, want all but the first few", rest, renames)
		}
		// Following every rename in memory allocates about 1.4 MiB for
		// each, over 800 MiB for the call.
		if limit := int64(128 << 20); measures && work > limit {
			t.Errorf("the call allocated %d MiB, over %d MiB", work>>20, limit>>20)
		}
		undone(t, e, alice)
		for _, row := range []int{1, formulas} {
			e.wantText(fmt.Sprintf("Sheet1!A%d", row), texts[row-1])
		}
	})
	// The work is one amount for all payloads in memory. Once it is used
	// up, the payloads that have not followed yet go to their files too.
	t.Run("the payloads of two people", func(t *testing.T) {
		ref := func(row, i int) string { return fmt.Sprintf("D!$A$%d", 5+(i+row)%850) }
		e, alice, _ := setup(t, ref)
		bob := e.person("Bob")
		// Bob's payload takes as much work as Alice's. The sessions come in
		// no fixed order, and whichever payload follows first uses up the
		// work for both.
		e.fillColumn("Sheet1", "B", formulas, func(row int) string { return longRefs(700, func(i int) string { return ref(row-1, i) }) })
		if ack := bob.do(e.clearOp("Sheet1", 1, formulas, 2, 1)); status(ack) != "applied" || bob.spilled(0) != nil {
			t.Fatalf("Bob's clear: %s, in a scratch file: %v", status(ack), bob.spilled(0) != nil)
		}
		const inserts = 200
		ops := make([]string, inserts)
		for i := range ops {
			ops[i] = `{"op":"insert_rows","tab":"D","at":1,"count":1}`
		}
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
		if alice.spilled(1) == nil || bob.spilled(0) == nil {
			t.Fatalf("after the call: Alice's payload in a scratch file %v, Bob's %v", alice.spilled(1) != nil, bob.spilled(0) != nil)
		}
		undone(t, e, alice)
		wantUndo(t, "Bob's undo", undo(t, bob), undoAck{Applied: formulas})
		moved := func(row, i int) string { return fmt.Sprintf("D!$A$%d", inserts+5+(i+row)%850) }
		for _, row := range []int{1, formulas} {
			want := longRefs(700, func(i int) string { return moved(row-1, i) })
			e.wantText(fmt.Sprintf("Sheet1!A%d", row), want)
			e.wantText(fmt.Sprintf("Sheet1!B%d", row), want)
		}
	})
	// An action inside the bound leaves the payloads in memory.
	t.Run("a call inside the bound", func(t *testing.T) {
		e, alice, _ := setup(t, func(row, i int) string { return fmt.Sprintf("D!$A$%d", 5+(i+row)%850) })
		e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"D","at":1,"count":1},{"op":"insert_rows","tab":"D","at":1,"count":1}]}`)
		if alice.spilled(1) != nil {
			t.Fatal("two inserts sent the payload to a scratch file")
		}
		undone(t, e, alice)
		e.wantText("Sheet1!A1", longRefs(700, func(i int) string { return fmt.Sprintf("D!$A$%d", 7+i%850) }))
	})
}

// editFiles returns the files of the scratch log of structural edits and
// their bytes on disk.
func (e *env) editFiles() (files int, bytes int64) {
	e.t.Helper()
	e.in.mu.RLock()
	dir := e.in.scratch.dir
	e.in.mu.RUnlock()
	if dir == "" {
		return 0, 0
	}
	list, err := os.ReadDir(dir)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, f := range list {
		if !strings.HasSuffix(f.Name(), ".edits") {
			continue
		}
		info, err := f.Info()
		if err != nil {
			e.t.Fatal(err)
		}
		files++
		bytes += info.Size()
	}
	return files, bytes
}

// unrelated builds the sheet of the cases below: Sheet1 holds formulas that
// read Sheet1 and long texts beside them, and the tab Other holds rows of
// numbers that a sort moves.
func unrelated(t *testing.T, rows int) (e *env, texts []string) {
	t.Helper()
	e = start(t)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"add_tab","name":"Other"},{"op":"insert_rows","tab":"Other","at":1,"count":%d}]}`, rows))
	for from := 0; from < rows; from += 10000 {
		values := make([]string, min(10000, rows-from))
		for i := range values {
			values[i] = fmt.Sprintf("[%d]", from+i)
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Other","at":"A%d","rows":[%s]}]}`, from+1, strings.Join(values, ",")))
	}
	texts = make([]string, 20)
	for i := range texts {
		texts[i] = fmt.Sprintf("=$D$%d+D%d*2", i+1, i+1)
	}
	e.fillColumn("Sheet1", "A", len(texts), func(row int) string { return texts[row-1] })
	for i, col := range []string{"B", "C"} {
		e.fillColumn("Sheet1", col, 40, func(row int) string { return longText(30000, 1000*i+row) })
	}
	return e, texts
}

// sorts sorts the rows of Other up and down, n times. Every sort moves
// every row.
func sorts(t *testing.T, e *env, p *person, rows, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if ack := p.do(e.sortOp("Other", 1, rows, 1, i%2 == 0)); status(ack) != "applied" {
			t.Fatalf("sort %d: %s", i+1, status(ack))
		}
	}
}

// An undo entry depends on the structural edits of the tabs it lies in and
// of the tabs its formulas name, and on no others: edits elsewhere neither
// reach the scratch log on its account nor cost it its undo, however many
// they are.
func TestUndoSurvivesUnrelatedHistory(t *testing.T) {
	// Sorts of Other of more than 64 MiB as the log counts them at full
	// size, where the bounds are the real ones: 8 MiB of edits for a
	// payload and 64 MiB for the log. By default they are an eighth and a
	// sixteenth of that, and the tab is a tenth.
	rows, rounds := sized(50000, 5000), sized(60, 40)
	lower := func(e *env) {
		if fullSize {
			return
		}
		e.in.mu.Lock()
		p := e.in.undoPool()
		p.edits.segment, p.edits.entryMax, p.edits.max = 64<<10, 1<<20, 4<<20
		e.in.mu.Unlock()
	}
	t.Run("sorts of another tab", func(t *testing.T) {
		e, texts := unrelated(t, rows)
		lower(e)
		alice, bob := e.person("Alice"), e.person("Bob")
		if ack := alice.do(e.clearOp("Sheet1", 1, 40, 1, 2)); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		if alice.spilled(0) == nil {
			t.Fatal("the clear of 1.2 MiB of text stays in memory")
		}
		sorts(t, e, bob, rows, rounds)
		if files, bytes := e.editFiles(); files != 0 {
			t.Errorf("sorts of a tab that no payload depends on left %d files of %d KiB in the scratch log", files, bytes>>10)
		}
		wantUndo(t, "undo of the clear", undo(t, alice), undoAck{Applied: 60})
		for i, text := range texts {
			e.wantText(fmt.Sprintf("Sheet1!A%d", i+1), text)
		}
		if got := e.shown("Sheet1!B40"); got != longText(30000, 40) {
			t.Fatalf("B40 came back as %q...", got[:16])
		}
	})
	// The same for an entry of the redo stack, of a session that is away
	// and whose history is still kept.
	t.Run("a redo entry of a session that is away", func(t *testing.T) {
		e, texts := unrelated(t, rows)
		lower(e)
		alice, bob := e.person("Alice"), e.person("Bob")
		// Alice puts new formulas and texts into F1:G40 and takes that
		// back: the redo entry holds them.
		var sb strings.Builder
		fmt.Fprintf(&sb, `{"op":"write","tab":%d,"rows":%s,"cols":%s,"values":[`, e.tabID("Sheet1"), e.rowsOf("Sheet1", 1, 40), e.colsOf("Sheet1", 6, 2))
		for row := 1; row <= 40; row++ {
			if row > 1 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `[%q,%q]`, texts[(row-1)%len(texts)], longText(30000, 100+row))
		}
		sb.WriteString("]}")
		if ack := alice.do(sb.String()); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		wantUndo(t, "undo of the write", undo(t, alice), undoAck{Applied: 80})
		e.in.mu.RLock()
		spilled := alice.s.undo.redo[0].file != nil && alice.s.undo.redo[0].wait != nil
		e.in.mu.RUnlock()
		if !spilled {
			t.Fatal("the redo entry of 1.2 MiB of text and 40 formulas is not in a scratch file")
		}
		e.in.smu.Lock()
		alice.s.left = e.clock.Now()
		e.in.smu.Unlock()
		e.clock.Add(undoDisconnected - time.Minute)
		e.tick()
		sorts(t, e, bob, rows, rounds)
		e.tick()
		if files, bytes := e.editFiles(); files != 0 {
			t.Errorf("sorts of a tab that no payload depends on left %d files of %d KiB in the scratch log", files, bytes>>10)
		}
		e.in.smu.Lock()
		alice.s.left = time.Time{}
		e.in.smu.Unlock()
		wantUndo(t, "redo of the write", redo(t, alice), undoAck{Applied: 80})
		e.wantText("Sheet1!F7", "=$D$7+D7*2")
		if got := e.shown("Sheet1!G40"); got != longText(30000, 140) {
			t.Fatalf("G40 came back as %q...", got[:16])
		}
	})
	// A payload that lies in the sorted tab depends on the sorts. They are
	// logged for it, up to what one payload may wait for; then it expires,
	// the log gives its edits up at once, and the others stay as they were.
	t.Run("sorts of a tab the payload lies in", func(t *testing.T) {
		e, texts := unrelated(t, rows)
		lower(e)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Other","at":"B1","rows":[["=A1*2"]]}]}`)
		alice, bob, carol := e.person("Alice"), e.person("Bob"), e.person("Carol")
		alice.do(e.clearOp("Sheet1", 1, 40, 1, 2))
		carol.set("Sheet1!E1", "before")
		// Carol clears the formula in Other and 1.2 MiB of text.
		if ack := carol.do(e.clearOp("Other", 1, 1, 2, 1), e.clearOp("Sheet1", 1, 40, 3, 1)); status(ack) != "applied,applied" {
			t.Fatal(status(ack))
		}
		if alice.spilled(0) == nil || carol.spilled(1) == nil {
			t.Fatal("the clears of 1.2 MiB of text stay in memory")
		}
		e.in.mu.RLock()
		entryMax := e.in.scratch.edits.entryMax
		e.in.mu.RUnlock()
		// One sort counts 24 bytes for each row it moves.
		perSort := int64(24 * rows)
		var most int64
		expired := 0
		for i := 0; i < rounds; i++ {
			if ack := bob.do(e.sortOp("Other", 1, rows, 1, i%2 == 0)); status(ack) != "applied" {
				t.Fatalf("sort %d: %s", i+1, status(ack))
			}
			_, bytes := e.editFiles()
			most = max(most, bytes)
			if carol.lost(1) {
				expired = i + 1
				break
			}
		}
		t.Logf("the payload expired after %d sorts of %d rows; the log held at most %d KiB on disk", expired, rows, most>>10)
		if want := int(entryMax/perSort) + 1; expired != want {
			t.Fatalf("the payload expired after %d sorts, want %d: one payload waits for %d KiB of edits and a sort counts %d KiB", expired, want, entryMax>>10, perSort>>10)
		}
		if most == 0 || most > entryMax+perSort {
			t.Errorf("the log held up to %d KiB on disk for a payload that may wait for %d KiB", most>>10, entryMax>>10)
		}
		if files, bytes := e.editFiles(); files != 0 {
			t.Errorf("the log keeps %d files of %d KiB for a payload that expired", files, bytes>>10)
		}
		if segments, open := e.editLog(); segments != 0 || open != 0 {
			t.Errorf("the log keeps %d segments and %d edits in memory for a payload that expired", segments, open)
		}
		seq := e.seq()
		wantUndo(t, "Carol's undo of the clear", undo(t, carol), undoAck{Skipped: 1, Reason: "lost"})
		if e.seq() != seq {
			t.Fatal("an undo that expired changed the sheet")
		}
		wantUndo(t, "Carol's undo of the edit before", undo(t, carol), undoAck{Applied: 1})
		// Alice's column B is still hers: Carol cleared column C.
		wantUndo(t, "Alice's undo", undo(t, alice), undoAck{Applied: 60})
		for i, text := range texts {
			e.wantText(fmt.Sprintf("Sheet1!A%d", i+1), text)
		}
	})
	// Inside that bound the payload follows the sorts: its formula comes
	// back in the row it lay in, wherever the sorts took that row.
	t.Run("sorts the payload can still follow", func(t *testing.T) {
		e, _ := unrelated(t, rows)
		lower(e)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Other","at":"B1","rows":[["=A1*2"]]}]}`)
		bob, carol := e.person("Bob"), e.person("Carol")
		if ack := carol.do(e.clearOp("Other", 1, 1, 2, 1), e.clearOp("Sheet1", 1, 40, 3, 1)); status(ack) != "applied,applied" {
			t.Fatal(status(ack))
		}
		// Three sorts: down, up, down. The row that was first is last.
		sorts(t, e, bob, rows, 3)
		if files, _ := e.editFiles(); files == 0 {
			t.Error("the sorts of the tab the payload lies in did not reach the scratch log")
		}
		wantUndo(t, "undo of the clear", undo(t, carol), undoAck{Applied: 41})
		e.wantText(fmt.Sprintf("Other!B%d", rows), fmt.Sprintf("=A%d*2", rows))
		e.tick()
		if files, bytes := e.editFiles(); files != 0 {
			t.Errorf("the log keeps %d files of %d KiB after the only payload that waited for them was undone", files, bytes>>10)
		}
	})
	// The log as a whole has a bound on disk. When payloads that are each
	// inside their own bound pass it together, the one that waits for the
	// most expires, and the others keep what they wait for.
	t.Run("payloads that pass the bound of the log together", func(t *testing.T) {
		e, _ := unrelated(t, 5000)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Third"},{"op":"insert_rows","tab":"Third","at":1,"count":4000}]}`)
		values := make([]string, 5000)
		for i := range values {
			values[i] = fmt.Sprintf("[%d]", i)
		}
		e.mustAgent(`{"ops":[{"op":"write","tab":"Third","at":"A1","rows":[` + strings.Join(values, ",") + `]},
			{"op":"write","tab":"Third","at":"B1","rows":[["=A1*2"]]},{"op":"write","tab":"Other","at":"B1","rows":[["=A1*2"]]}]}`)
		// A flush for every 16 KiB of edits, and 256 KiB for the log on disk.
		// A payload may wait for 4 MiB, which neither reaches.
		const logMax = 256 << 10
		e.in.mu.Lock()
		p := e.in.undoPool()
		p.edits.segment, p.edits.entryMax, p.edits.max = 16<<10, 4<<20, logMax
		e.in.mu.Unlock()
		bob, carol, dave := e.person("Bob"), e.person("Carol"), e.person("Dave")
		carol.do(e.clearOp("Other", 1, 1, 2, 1), e.clearOp("Sheet1", 1, 40, 2, 1))
		dave.do(e.clearOp("Third", 1, 1, 2, 1), e.clearOp("Sheet1", 1, 40, 3, 1))
		if carol.spilled(0) == nil || dave.spilled(0) == nil {
			t.Fatal("the clears of 1.2 MiB of text stay in memory")
		}
		// Two sorts of Other for each sort of Third.
		var most int64
		sortsDone := 0
		for ; sortsDone < 60 && !carol.lost(0) && !dave.lost(0); sortsDone++ {
			tab := "Other"
			if sortsDone%3 == 2 {
				tab = "Third"
			}
			if ack := bob.do(e.sortOp(tab, 1, 5000, 1, sortsDone%2 == 0)); status(ack) != "applied" {
				t.Fatalf("sort %d: %s", sortsDone+1, status(ack))
			}
			_, bytes := e.editFiles()
			most = max(most, bytes)
		}
		t.Logf("after %d sorts the log had held at most %d KiB on disk", sortsDone, most>>10)
		if !carol.lost(0) || dave.lost(0) {
			t.Fatalf("after %d sorts Carol's payload expired: %v, Dave's: %v; want the one that waited for twice the edits", sortsDone, carol.lost(0), dave.lost(0))
		}
		if _, bytes := e.editFiles(); bytes > logMax || most > logMax {
			t.Errorf("the log holds %d KiB on disk and held up to %d KiB, over its bound of %d KiB", bytes>>10, most>>10, logMax>>10)
		}
		wantUndo(t, "Carol's undo", undo(t, carol), undoAck{Skipped: 1, Reason: "lost"})
		wantUndo(t, "Dave's undo", undo(t, dave), undoAck{Applied: 41})
		// Third was sorted an even or odd number of times; its first row
		// is first or last.
		e.in.mu.RLock()
		tb := e.in.wb.tab("Third")
		var found []string
		for r, id := range tb.rows.ids {
			if c := tb.cell(RowID(id), ColID(tb.cols.ids[1])); c != nil && c.f != nil {
				found = append(found, fmt.Sprintf("B%d %s", r+1, c.f.Text(formula.Pos{Row: int32(r), Col: 1})))
			}
		}
		e.in.mu.RUnlock()
		if len(found) != 1 || (found[0] != "B1 =A1*2" && found[0] != "B5000 =A5000*2") {
			t.Fatalf("Dave's formula came back as %v", found)
		}
	})
}

// An entry keeps the names of at most 32 tabs its formulas name. One that
// names more depends on every tab: it follows every edit, and loses none.
func TestUndoFollowsWithMoreNamesThanItKeeps(t *testing.T) {
	bothWays(t, func(t *testing.T, e *env) {
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"add_tab","name":"Other"}]}`)
		// 40 names no tab has, and the two tabs at the end.
		text := longRefs(40, func(i int) string { return fmt.Sprintf("None%d!A1", i) }) + "+Data!A5+Other!A5"
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[%q]]}]}`, text))
		alice, bob := e.person("Alice"), e.person("Bob")
		alice.set("Sheet1!A1", "x")
		e.in.mu.RLock()
		deps := alice.s.undo.undo[0].deps
		all := deps != nil && deps.allNames
		e.in.mu.RUnlock()
		if !all {
			t.Fatalf("an entry whose formula names 42 tabs keeps their names: %+v", deps)
		}
		bob.do(e.insertRowsOp("Data", 1, 2))
		bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"Info"}`, e.tabID("Data")))
		bob.do(e.insertRowsOp("Other", 3, 1))
		bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":"None7"}`, e.tabID("Other")))
		wantUndo(t, "undo of the edit", undo(t, alice), undoAck{Applied: 1})
		want := strings.Replace(text, "Data!A5+Other!A5", "Info!A7+None7!A6", 1)
		e.wantText("Sheet1!A1", want)
	})
}

// The deletions an entry keeps apart count with the ids they took. An
// entry that would hold more of them than one entry may wait for expires.
func TestUndoExpiresOverDeletionsKeptApart(t *testing.T) {
	e := start(t)
	const rows = 4000
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"add_tab","name":"D"},{"op":"insert_rows","tab":"D","at":1,"count":%d}]}`, rows))
	rng := rand.New(rand.NewPCG(3, 5))
	values := make([]string, rows)
	for i := range values {
		values[i] = fmt.Sprintf("[%d]", rng.IntN(1<<30))
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"D","at":"A1","rows":[` + strings.Join(values, ",") + `]},
		{"op":"write","tab":"Sheet1","at":"A1","rows":[["=D!$A$1","=Sheet1!$C$1"]]}]}`)
	alice, bob := e.person("Alice"), e.person("Bob")
	// The sort leaves neighbouring rows with ids that lie apart.
	bob.do(e.sortOp("D", 1, rows, 1, false))
	alice.set("Sheet1!A1", "x") // depends on D
	alice.set("Sheet1!B1", "y") // does not
	// 32 KiB for the deletions one entry keeps apart: 2,000 runs of ids.
	e.in.mu.Lock()
	e.in.undoPool().edits.entryMax = 32 << 10
	e.in.mu.Unlock()
	for i := 0; i < 3 && !alice.lost(0); i++ {
		if alice.lost(1) {
			t.Fatal("the entry that does not depend on the tab expired")
		}
		if ack := bob.do(e.deleteRowsOp("D", 10, 800)); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
	}
	if !alice.lost(0) || alice.lost(1) {
		t.Fatalf("after deletions of 2,400 rows with ids apart: the entry that names the tab expired: %v, the other: %v", alice.lost(0), alice.lost(1))
	}
	wantUndo(t, "undo of the entry that does not depend on the tab", undo(t, alice), undoAck{Applied: 1})
	e.wantText("Sheet1!B1", "=Sheet1!$C$1")
	wantUndo(t, "undo of the entry that expired", undo(t, alice), undoAck{Skipped: 1, Reason: "lost"})
	if e.text("Sheet1!A1") != "x" {
		t.Fatalf("A1 = %q after an undo that was skipped", e.text("Sheet1!A1"))
	}
}

// lost reports whether the i-th entry of a person's undo stack has given
// its payload up.
func (p *person) lost(i int) bool {
	p.e.in.mu.RLock()
	defer p.e.in.mu.RUnlock()
	return i < len(p.s.undo.undo) && p.s.undo.undo[i].lost
}
