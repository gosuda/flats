package sheet

import (
	"fmt"
	"testing"
)

// Fitting the write marks into the sheet's bound looks at each mark and
// each history entry a bounded number of times, however many entries
// expire. Many sessions each keep inserts whose undo asks about marks,
// and an agent then writes marks newer than all of them on another tab:
// none of those marks can go before every older entry has expired. Carol
// inserts after the marks; her entry stays, and the marks older than it
// go once the older entries have. A fit that looked at every mark after
// each expiry would look at them as many times as entries expire.
func TestUndoFitLooksAtEachMarkOnce(t *testing.T) {
	slow(t, "keeps the inserts of many sessions while marks fill the sheet")
	sessions, each := raced(16, 4), raced(25, 10)
	if fullSize && !raceEnabled {
		sessions, each = 128, maxUndoEntries
	}
	bound := sheetBound()
	e := boundedSheet(t, bound)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Big"}]}`)
	var people []*person
	for i := range sessions {
		p := e.person(fmt.Sprintf("P%d", i))
		for range each {
			if ack := p.do(e.insertRowsOp("Sheet1", 1, 1)); status(ack) != "applied" {
				t.Fatalf("an insert: %s", status(ack))
			}
		}
		people = append(people, p)
	}
	fillMarks(e, "Big", bound*3/4)
	carol := e.person("Carol")
	if ack := carol.do(e.insertRowsOp("Sheet1", 1, 1)); status(ack) != "applied" {
		t.Fatalf("Carol's insert: %s", status(ack))
	}
	_, before, _ := e.marks()
	rows := int(min(bound/200, 20000))
	var work int
	e.in.mu.Lock()
	e.in.undoPool().markWork = func(n int) { work += n }
	e.in.mu.Unlock()
	// The newest marks take the sheet past its bound.
	for {
		markCycleIn(e, "Big", rows)
		if people[0].lost(0) {
			break
		}
		if _, _, within := e.marks(); !within {
			t.Fatal("the marks are over the bound after a call")
		}
	}
	e.in.mu.Lock()
	e.in.undoPool().markWork = nil
	e.in.mu.Unlock()
	wantSheetBound(t, e, "after the marks")
	for i, p := range people {
		for k := range each {
			if !p.lost(k) {
				t.Fatalf("entry %d of session %d did not expire", k, i)
			}
		}
	}
	if carol.lost(0) {
		t.Fatal("Carol's insert expired")
	}
	// The work counts each look at a mark: when a map is made again, the
	// look that counts what it keeps and the one that copies it.
	entries := sessions*each + 1
	if limit := 12*(before+rows+entries) + 4096; work > limit {
		t.Fatalf("the fit looked at %d marks and entries for %d marks and %d entries; at most %d", work, before+rows, entries, limit)
	}
	t.Logf("%d marks and entries looked at for %d marks and %d entries", work, before+rows, entries)
	wantUndo(t, "undo of Carol's insert", undo(t, carol), undoAck{Applied: 1})
}

// marksAt returns what the marks of the sheet would count once those whose
// stamps are all at or below oldest went, from a look at each of them.
func (e *env) marksAt(oldest uint64) int64 {
	var n int64
	for _, t := range e.in.wb.tabs {
		for _, marks := range [2]map[uint64]writeMark{t.marks, t.colMarks} {
			kept := 0
			for _, m := range marks {
				if m.last > oldest || m.other > oldest {
					kept++
				}
			}
			if kept > 0 {
				n += mapRoom[uint64, writeMark](kept)
			}
		}
	}
	return n
}

// What the fit counts for the marks at each oldest entry it can reach is
// what the maps hold once the others went: with maps of fewer than 8 marks
// and of more, and maps that lose all their marks. Entries that ask and
// marks between them alternate: each sequence lets some marks go.
func TestUndoFitCountsTheMarksItLetsGo(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Big"},{"op":"add_tab","name":"Small"}]}`)
	people := []*person{e.person("Alice"), e.person("Bob")}
	for i := range 12 {
		p := people[i%2]
		if ack := p.do(e.insertRowsOp("Sheet1", 1, 1)); status(ack) != "applied" {
			t.Fatalf("an insert: %s", status(ack))
		}
		markCycleIn(e, "Big", 3+i*5)
		if i%3 == 0 {
			markCycleIn(e, "Small", 1+i%4)
		}
	}
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	e.in.smu.Lock()
	defer e.in.smu.Unlock()
	f := &markFit{in: e.in}
	f.begin()
	if got, want := f.bytes(), e.marksAt(f.below); got != want {
		t.Fatalf("at the start the fit counts %d bytes of marks, the maps %d", got, want)
	}
	for steps := 0; ; steps++ {
		front := f.front()
		if front == nil {
			break
		}
		front.expire(nil)
		f.follow()
		if got, want := f.bytes(), e.marksAt(f.below); got != want {
			t.Fatalf("after %d expiries the fit counts %d bytes of marks, the maps %d", steps+1, got, want)
		}
	}
	f.finish()
	if got, want := e.in.wb.markBytes(), e.marksAt(^uint64(0)); got != want || got != 0 {
		t.Fatalf("after the fit the maps count %d bytes, want %d", got, want)
	}
}

// When the sheet is over its bound with every payload in a scratch file,
// the entries whose files leave the most in memory expire one after the
// other. Choosing each looks at the entries once in all, not once for each
// entry that expires.
func TestUndoExpiryLooksAtEachEntryOnce(t *testing.T) {
	sessions, each := raced(16, 4), raced(40, 10)
	e := start(t)
	holdSweeps(e)
	e.in.mu.Lock()
	e.in.undoPool().entryMax = -1
	e.in.mu.Unlock()
	for i := range sessions {
		p := e.person(fmt.Sprintf("P%d", i))
		for k := range each {
			if ack := p.set(fmt.Sprintf("Sheet1!A%d", k+1), fmt.Sprintf("v%d-%d", i, k)); status(ack) != "applied" {
				t.Fatalf("a write: %s", status(ack))
			}
		}
	}
	var work int
	e.in.mu.Lock()
	p := e.in.undoPool()
	// The records of expired entries stay on their stacks: the bound lies
	// halfway between them and what the files leave now.
	held := p.resident
	bound := held - (held-int64(sessions*each)*entryBytes)/2
	p.instanceMax = bound
	p.markWork = func(n int) { work += n }
	e.in.mu.Unlock()
	e.tick()
	e.in.mu.Lock()
	p.markWork = nil
	resident := p.resident
	e.in.mu.Unlock()
	if resident > bound {
		t.Fatalf("after the tick the sheet holds %d bytes, over its bound of %d", resident, bound)
	}
	entries := sessions * each
	if limit := 6*entries + 64; work > limit {
		t.Fatalf("choosing the entries to expire looked at %d entries for %d; at most %d", work, entries, limit)
	}
	t.Logf("%d entries looked at for %d", work, entries)
}
