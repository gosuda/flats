package sheet

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// boundedSheet starts an instance whose sheet keeps bound bytes of undo
// memory. Its own ticks sweep no notes, so that only the test's ticks
// change what the sheet holds.
func boundedSheet(t *testing.T, bound int64) *env {
	e := start(t)
	holdSweeps(e)
	e.in.mu.Lock()
	e.in.undoPool().instanceMax = bound
	e.in.mu.Unlock()
	return e
}

// sheetBound is the sheet's undo memory in the tests of its bound: the
// engine's own with FLATS_SHEET_FULL=1, and a smaller one that takes the
// same paths otherwise and under the race detector.
func sheetBound() int64 {
	if fullSize && !raceEnabled {
		return undoInstanceBytes
	}
	return int64(raced(8<<20, 512<<10))
}

// failWrites makes every write of an undo payload to a scratch file fail.
func failWrites(e *env, fail bool) {
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	if !fail {
		e.in.undoPool().fault = nil
		return
	}
	e.in.undoPool().fault = func(op string) error {
		if op == "write" {
			return errors.New("no space left on device")
		}
		return nil
	}
}

// sheetUndo returns what the sheet's undo memory counts: payloads and what
// spilled payloads leave in memory, stand-in notes with an unfinished
// sweep, and write marks.
func (e *env) sheetUndo() (payloads, notes, marks int64) {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	p := &e.in.scratch
	return p.resident, p.notes.bytes, e.in.wb.markBytes()
}

// fillMarks has an agent write rows in tab and delete them again until the
// sheet's undo memory holds target bytes, within a few hundred.
func fillMarks(e *env, tab string, target int64) {
	e.t.Helper()
	for {
		payloads, notes, marks := e.sheetUndo()
		left := target - payloads - notes - marks
		if left < 300 {
			return
		}
		markCycleIn(e, tab, int(min(left/100, 20000)))
	}
}

// wantSheetBound fails when the sheet's undo memory is over its bound.
func wantSheetBound(t *testing.T, e *env, when string) {
	t.Helper()
	payloads, notes, marks := e.sheetUndo()
	e.in.mu.RLock()
	bound := e.in.scratch.instanceMax
	e.in.mu.RUnlock()
	if payloads+notes+marks > bound {
		t.Fatalf("%s the sheet holds %d bytes of payloads, %d of notes and %d of write marks, over its bound of %d", when, payloads, notes, marks, bound)
	}
}

// When a person's action takes the sheet past its undo memory with the
// write marks, the payload it brings goes to a scratch file like those on
// the stacks, before any entry expires. Alice keeps her insert, whose undo
// asks about the marks; Bob's clear of long texts goes to a file.
func TestUndoSpillsTheIncomingPayloadBeforeHistoryExpires(t *testing.T) {
	slow(t, "writes rows that are deleted again until the marks nearly fill the sheet")
	bound := sheetBound()
	cells := raced(10, 1)
	text := strings.Repeat("가", formula.MaxTextLen-7)
	payload := int64(cells * len(text))
	e := boundedSheet(t, bound)
	alice, bob := e.person("Alice"), e.person("Bob")
	if ack := alice.do(e.insertRowsOp("Sheet1", 1, 1)); status(ack) != "applied" {
		t.Fatalf("Alice's insert: %s", status(ack))
	}
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"T"}]}`)
	e.fillColumn("T", "A", cells, func(int) string { return text })
	fillMarks(e, "Sheet1", bound-payload/2)
	wantSheetBound(t, e, "before Bob's clear")
	if ack := bob.do(e.clearOp("T", 1, cells, 1, 1)); status(ack) != "applied" {
		t.Fatalf("Bob's clear: %s", status(ack))
	}
	wantSheetBound(t, e, "after Bob's clear")
	if alice.lost(0) {
		t.Fatal("Alice's insert expired while Bob's payload could go to a scratch file")
	}
	if bob.spilled(0) == nil || alice.spilled(0) != nil {
		t.Fatalf("Bob's payload in a scratch file: %v, Alice's: %v; want only Bob's", bob.spilled(0) != nil, alice.spilled(0) != nil)
	}
	wantCharges(t, e)
	wantUndo(t, "undo of Bob's clear", undo(t, bob), undoAck{Applied: cells})
	e.want("T!A1", text)
	wantUndo(t, "undo of Alice's insert", undo(t, alice), undoAck{Applied: 1})
}

// An action whose payload has to go to a scratch file for the sheet's
// bound, with the write marks, and cannot be written there is refused as
// unavailable before it commits: the sheet, its sequence and the action's
// number stay as they were, and so does every entry. Sent again once the
// file can be written, the action applies once and its undo works. Alice
// keeps her insert, whose undo asks about the marks; Bob clears long
// texts.
func TestUndoRefusesAnActionWhosePayloadCannotGoToAFile(t *testing.T) {
	slow(t, "writes rows that are deleted again until the marks nearly fill the sheet")
	bound := sheetBound()
	cells := raced(10, 1)
	text := strings.Repeat("가", formula.MaxTextLen-7)
	payload := int64(cells * len(text))
	e := boundedSheet(t, bound)
	alice, bob := e.person("Alice"), e.person("Bob")
	if ack := alice.do(e.insertRowsOp("Sheet1", 1, 1)); status(ack) != "applied" {
		t.Fatalf("Alice's insert: %s", status(ack))
	}
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"T"}]}`)
	e.fillColumn("T", "A", cells, func(int) string { return text })
	fillMarks(e, "Sheet1", bound-payload/2)
	// The payload fits one entry and Bob's session. With the marks it does
	// not fit the sheet.
	if payload > undoSpillBytes || payload > undoSessionBytes {
		t.Fatalf("the payload of %d bytes passes an entry's or a session's bound", payload)
	}
	failWrites(e, true)
	bob.n++
	n, base, clear := bob.n, e.seq(), e.clearOp("T", 1, cells, 1, 1)
	seq, before := e.seq(), e.dump()
	_, err := bob.send(n, base, clear)
	if werr := refusal(err); werr.Code != "unavailable" || werr.Reason != "undo_scratch" || !werr.Retry {
		t.Fatalf("Bob's clear with no scratch file answered %v", err)
	}
	if e.seq() != seq || e.dump() != before {
		t.Fatal("the refused clear changed the sheet")
	}
	wantSheetBound(t, e, "after the refused clear")
	if alice.lost(0) || alice.spilled(0) != nil {
		t.Fatalf("Alice's insert expired: %v, in a scratch file: %v", alice.lost(0), alice.spilled(0) != nil)
	}
	wantCharges(t, e)
	failWrites(e, false)
	for range 2 {
		ack, err := bob.send(n, base, clear)
		if err != nil || status(ack) != "applied" {
			t.Fatalf("Bob's clear sent again: %v %s", err, status(ack))
		}
		if e.seq() != seq+1 {
			t.Fatalf("Bob's clear moved the sequence from %d to %d", seq, e.seq())
		}
	}
	e.want("T!A1", "")
	wantSheetBound(t, e, "after Bob's clear")
	if bob.spilled(0) == nil || alice.lost(0) {
		t.Fatalf("Bob's payload in a scratch file: %v; Alice's insert expired: %v", bob.spilled(0) != nil, alice.lost(0))
	}
	wantCharges(t, e)
	wantUndo(t, "undo of Bob's clear", undo(t, bob), undoAck{Applied: cells})
	e.want("T!A1", text)
	wantUndo(t, "undo of Alice's insert", undo(t, alice), undoAck{Applied: 1})
}

// An entry that expires because its payload cannot go to a scratch file
// can be the oldest that asks about the write marks. The marks only it
// could ask about go in the same step, so the sheet is inside its bound
// when the change that took it there returns, and no other entry expires
// for marks that nothing asks about any more. The agent's calls fill the
// marks; Alice's insert asks about all of them, Carol's, which is in a
// scratch file already, about the newest.
func TestUndoExpiryLetsGoOfTheMarksOfItsEntry(t *testing.T) {
	slow(t, "writes rows that are deleted again until the marks fill the sheet")
	bound := sheetBound()
	over := func(e *env) {
		e.t.Helper()
		for {
			if payloads, notes, marks := e.sheetUndo(); payloads+notes+marks > bound/2 {
				return
			}
			markCycleIn(e, "Sheet1", int(min(bound/400, 20000)))
		}
	}
	t.Run("the last entry that asks", func(t *testing.T) {
		e := boundedSheet(t, bound)
		alice := e.person("Alice")
		if ack := alice.do(e.insertRowsOp("Sheet1", 1, 1)); status(ack) != "applied" {
			t.Fatalf("Alice's insert: %s", status(ack))
		}
		fillMarks(e, "Sheet1", bound-1024)
		failWrites(e, true)
		markCycleIn(e, "Sheet1", int(min(bound/400, 20000)))
		wantSheetBound(t, e, "right after the agent's call")
		if !alice.lost(0) {
			t.Fatal("Alice's insert did not expire")
		}
		if _, n, _ := e.marks(); n != 0 {
			t.Fatalf("%d write marks are left that no entry asks about", n)
		}
		wantCharges(t, e)
	})
	t.Run("one of several entries that ask", func(t *testing.T) {
		e := boundedSheet(t, bound)
		alice, carol := e.person("Alice"), e.person("Carol")
		if ack := alice.do(e.insertRowsOp("Sheet1", 1, 1)); status(ack) != "applied" {
			t.Fatalf("Alice's insert: %s", status(ack))
		}
		over(e)
		e.in.mu.Lock()
		e.in.undoPool().entryMax = -1
		e.in.mu.Unlock()
		if ack := carol.do(e.insertRowsOp("Sheet1", 1, 1)); status(ack) != "applied" {
			t.Fatalf("Carol's insert: %s", status(ack))
		}
		e.in.mu.Lock()
		e.in.undoPool().entryMax = undoSpillBytes
		e.in.mu.Unlock()
		if carol.spilled(0) == nil {
			t.Fatal("Carol's payload is in memory")
		}
		fillMarks(e, "Sheet1", bound-1024)
		failWrites(e, true)
		markCycleIn(e, "Sheet1", int(min(bound/400, 20000)))
		wantSheetBound(t, e, "right after the agent's call")
		if !alice.lost(0) || carol.lost(0) {
			t.Fatalf("Alice's insert expired: %v, Carol's: %v; want only Alice's", alice.lost(0), carol.lost(0))
		}
		wantCharges(t, e)
		failWrites(e, false)
		wantUndo(t, "undo of Carol's insert", undo(t, carol), undoAck{Applied: 1})
	})
}

// A structural edit that the payloads in memory follow can make them
// larger. With the write marks, that can take the sheet past its undo
// memory while every entry and session is inside its own bound: the
// payloads go to scratch files as the edit commits, before the next tick,
// and Alice keeps her insert. Carol's entry saved a formula that names a
// tab, and keeps apart a deletion of that tab's rows whose ids follow no
// other, one run for each.
func TestUndoFitsTheSheetAfterStructuralEdits(t *testing.T) {
	slow(t, "deletes rows of ids that follow no other while marks nearly fill the sheet")
	bound := sheetBound()
	runs := raced(20000, 1000)
	for _, who := range []string{"an agent", "a person"} {
		t.Run(who+" deletes", func(t *testing.T) {
			e := boundedSheet(t, bound)
			reversedTab(e, "D", runs+1)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["=D!A1"]]}]}`)
			// No entry asks about the marks of the setup.
			e.tick()
			alice, bob, carol := e.person("Alice"), e.person("Bob"), e.person("Carol")
			if ack := alice.do(e.insertRowsOp("Sheet1", 3, 1)); status(ack) != "applied" {
				t.Fatalf("Alice's insert: %s", status(ack))
			}
			if ack := carol.set("Sheet1!A1", "=D!A2"); status(ack) != "applied" {
				t.Fatalf("Carol's formula: %s", status(ack))
			}
			// The deletion adds 16 bytes for each run to Carol's entry.
			growth := int64(16 * runs)
			fillMarks(e, "Sheet1", bound-growth/2)
			wantSheetBound(t, e, "before the deletion")
			if who == "an agent" {
				e.mustAgent(jsonf(`{"ops":[{"op":"delete_rows","tab":"D","at":1,"count":%d}]}`, runs))
			} else if ack := bob.do(e.deleteRowsOp("D", 1, runs)); status(ack) != "applied" {
				t.Fatalf("Bob's deletion: %s", status(ack))
			}
			wantSheetBound(t, e, "after the deletion")
			if alice.lost(0) || carol.lost(0) {
				t.Fatalf("Alice's insert expired: %v, Carol's formula: %v", alice.lost(0), carol.lost(0))
			}
			if carol.spilled(0) == nil {
				t.Fatal("Carol's payload, the largest, stayed in memory")
			}
			wantCharges(t, e)
			wantUndo(t, "undo of Carol's formula", undo(t, carol), undoAck{Applied: 1})
			if got := e.text("Sheet1!A1"); !strings.Contains(got, "#REF!") {
				t.Fatalf("the undo brought back %q", got)
			}
			wantUndo(t, "undo of Alice's insert", undo(t, alice), undoAck{Applied: 1})
		})
	}
}

// The tick builds a set of notes again a stretch at a time, and what it
// builds counts in the sheet's undo memory with the write marks. With the
// marks nearly filling the sheet, the sweep finds no room and leaves its
// set as it is: the sheet is inside its bound after the tick, and no entry
// expired for it.
func TestUndoNoteSweepLeavesRoomForWriteMarks(t *testing.T) {
	slow(t, "formats every other row of a large range")
	f := newSweepFixture(t, true)
	e := f.e
	n, gone, bytes := f.set().n, f.set().gone, f.set().bytes
	carol := e.person("Carol")
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"M"}]}`)
	if ack := carol.do(e.insertRowsOp("M", 1, 1)); status(ack) != "applied" {
		t.Fatalf("Carol's insert: %s", status(ack))
	}
	// The marks of the fixture, older than Carol's insert, go at a tick
	// that sweeps no notes (holdSweeps). Then the marks take more than the
	// sweep could build beside the set.
	e.tick()
	for {
		if _, _, marks := e.sheetUndo(); marks > 3*bytes {
			break
		}
		markCycleIn(e, "M", int(min(bytes/32+1, 20000)))
	}
	payloads, notes, marks := e.sheetUndo()
	e.in.mu.Lock()
	e.in.undoPool().instanceMax = payloads + notes + marks + 1<<10
	e.in.mu.Unlock()
	sweepTick(e, max(1, f.notes/3))
	wantSheetBound(t, e, "after the tick")
	if m := f.alice.noteMemory(); m.swept != nil {
		t.Fatalf("the sweep holds %d bytes beside its set", m.swept.held())
	}
	if s := f.set(); s.n != n || s.gone != gone || s.bytes != bytes {
		t.Fatalf("the set: %d notes, %d gone, %d bytes; want it as it was, %d, %d and %d", s.n, s.gone, s.bytes, n, gone, bytes)
	}
	if carol.lost(0) || f.alice.lost(0) {
		t.Fatalf("an entry expired: Carol's %v, Alice's %v", carol.lost(0), f.alice.lost(0))
	}
	wantUndo(t, "undo of Carol's insert", undo(t, carol), undoAck{Applied: 1})
}

// A map of write marks keeps the room of the marks a revert takes out of
// it. An action that a later operation refuses, or whose commit fails,
// leaves no such room behind: a map it emptied goes at once, and one that
// keeps older marks is made again for them by the next tick. Each tab here
// has 50,000 rows and no entry that asks about them; the agent formats
// every row, then refers to a tab that does not exist.
func TestUndoReleasesWriteMarksARevertLeaves(t *testing.T) {
	slow(t, "formats 50,000 rows of several tabs in calls that are refused")
	rows, tabs := raced(50000, 2000), raced(8, 2)
	format := func(tab string) string {
		return jsonf(`{"op":"format","tab":%q,"ranges":["A1:A%d"],"format":{"bold":true}}`, tab, rows)
	}
	refused := func(tab string) string {
		return `{"ops":[` + format(tab) + `,{"op":"delete_tab","tab":"Nowhere"}]}`
	}
	// counts returns what the marks count and the room their maps keep.
	counts := func(e *env) (charged int64, room int) {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		for _, t := range e.in.wb.tabs {
			room += max(t.marksMost, len(t.marks)) + max(t.colMarksMost, len(t.colMarks))
		}
		return e.in.wb.markBytes(), room
	}
	setup := func(t *testing.T) *env {
		e := start(t)
		for k := range tabs {
			name := fmt.Sprint("Rows", k)
			e.mustAgent(jsonf(`{"ops":[{"op":"add_tab","name":%q},{"op":"insert_rows","tab":%q,"at":%d,"count":%d}]}`, name, name, newTabRows+1, rows-newTabRows))
		}
		e.tick()
		return e
	}
	// spare returns the heap the maps of marks keep beyond what their marks
	// need: what the heap lets go when each map is made again for its
	// marks. A refused call also leaves the room of its rows in the maps of
	// the tab's own records, which this does not read.
	spare := func(t *testing.T, e *env) int64 {
		e.b.importAll(t)
		e.b.forgetRevisions()
		held := undoHeap(t, e)
		e.in.mu.Lock()
		for _, tab := range e.in.wb.tabs {
			for _, m := range []*map[uint64]writeMark{&tab.marks, &tab.colMarks} {
				if *m != nil {
					fresh := make(map[uint64]writeMark, len(*m))
					for id, mark := range *m {
						fresh[id] = mark
					}
					*m = fresh
				}
			}
		}
		e.in.mu.Unlock()
		return held - undoHeap(t, e)
	}
	t.Run("agent calls that a later operation refuses", func(t *testing.T) {
		e := setup(t)
		seq := e.seq()
		for round := range 2 {
			for k := range tabs {
				if _, err := e.agent(refused(fmt.Sprint("Rows", k))); err == nil {
					t.Fatalf("round %d, tab %d: the call was applied", round, k)
				}
				if charged, room := counts(e); charged != 0 || room != 0 {
					t.Fatalf("round %d, tab %d: the marks count %d bytes and their maps keep room for %d", round, k, charged, room)
				}
			}
		}
		kept := spare(t, e)
		t.Logf("the maps of marks keep %d KiB of the heap after the refused calls", kept>>10)
		if measures && kept > 256<<10 {
			t.Errorf("the maps of marks keep %d KiB of the heap after the refused calls", kept>>10)
		}
		if e.seq() != seq {
			t.Fatalf("the sequence moved from %d to %d", seq, e.seq())
		}
	})
	t.Run("an action whose commit fails", func(t *testing.T) {
		e := setup(t)
		alice := e.person("Alice")
		seq := e.seq()
		e.in.mu.Lock()
		e.in.fault = func() error { return errors.New("injected commit failure") }
		e.in.mu.Unlock()
		for k := range tabs {
			if _, err := alice.try(seq, e.formatRange(fmt.Sprint("Rows", k), 1, rows, 1, 1, `{"bold":true}`)); refusal(err).Reason != "commit" {
				t.Fatalf("tab %d: a failed commit answered %v", k, err)
			}
			if charged, room := counts(e); charged != 0 || room != 0 {
				t.Fatalf("tab %d: the marks count %d bytes and their maps keep room for %d", k, charged, room)
			}
		}
		e.in.mu.Lock()
		e.in.fault = nil
		e.in.mu.Unlock()
		kept := spare(t, e)
		t.Logf("the maps of marks keep %d KiB of the heap after the failed commits", kept>>10)
		if measures && kept > 256<<10 {
			t.Errorf("the maps of marks keep %d KiB of the heap after the failed commits", kept>>10)
		}
		if e.seq() != seq {
			t.Fatalf("the sequence moved from %d to %d", seq, e.seq())
		}
	})
	t.Run("a map that keeps older marks", func(t *testing.T) {
		e := setup(t)
		alice, bob := e.person("Alice"), e.person("Bob")
		if ack := alice.do(e.insertRowsOp("Rows0", 1, 1)); status(ack) != "applied" {
			t.Fatalf("Alice's insert: %s", status(ack))
		}
		bob.set("Rows0!B1", "bob")
		e.tick()
		small, _ := counts(e)
		for round := range 2 {
			if _, err := e.agent(refused("Rows0")); err == nil {
				t.Fatalf("round %d: the call was applied", round)
			}
		}
		// The map keeps the room of every row and counts it; the tick makes
		// it again for Bob's mark.
		if charged, room := counts(e); charged <= small || room < rows {
			t.Fatalf("after the refused calls the marks count %d bytes, room for %d; before them %d bytes", charged, room, small)
		}
		e.tick()
		if charged, room := counts(e); charged != small || room > 4 {
			t.Fatalf("after the tick the marks count %d bytes with room for %d, want %d as before", charged, room, small)
		}
		kept := spare(t, e)
		t.Logf("the maps of marks keep %d KiB of the heap after the tick", kept>>10)
		if measures && kept > 256<<10 {
			t.Errorf("the maps of marks keep %d KiB of the heap after the tick", kept>>10)
		}
		wantUndo(t, "undo of Alice's insert", undo(t, alice), undoAck{Skipped: 1, Reason: "written"})
	})
}

// A person's own deletion can make the older payloads of their session
// larger after their new entry was admitted beside them. The session is
// brought back inside its bound with the new entry on its stack: a payload
// goes to a scratch file, and no entry expires. Bob's formula keeps the
// deletion apart, which makes it larger, and his session's bound leaves
// room for the formula as it grows and for the new entry, not for both.
func TestUndoFitsTheSessionWithTheEntryThatJoins(t *testing.T) {
	slow(t, "deletes rows of ids that follow no other")
	runs := raced(20000, 1000)
	// setup returns Bob, and what his session holds before the deletion.
	setup := func(t *testing.T) (*env, *person, int64) {
		e := start(t)
		reversedTab(e, "D", runs+1)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["=D!A1"]]}]}`)
		bob := e.person("Bob")
		if ack := bob.set("Sheet1!A1", "=D!A2"); status(ack) != "applied" {
			t.Fatalf("Bob's formula: %s", status(ack))
		}
		return e, bob, resident(e, bob)
	}
	deletes := func(t *testing.T, e *env, bob *person) {
		if ack := bob.do(e.deleteRowsOp("D", 1, runs)); status(ack) != "applied" {
			t.Fatalf("Bob's deletion: %s", status(ack))
		}
	}
	// The sizes the entries take after the deletion, without a bound.
	e, bob, held := setup(t)
	deletes(t, e, bob)
	e.in.mu.RLock()
	grown, joined := bob.s.undo.undo[0].size, bob.s.undo.undo[1].size
	e.in.mu.RUnlock()
	grown -= held
	t.Logf("the formula grows by %d bytes, and the deletion's entry takes %d", grown, joined)

	e, bob, held = setup(t)
	bound := held + max(grown, joined) + min(grown, joined)/2
	e.in.mu.Lock()
	e.in.undoPool().sessionMax = bound
	e.in.mu.Unlock()
	deletes(t, e, bob)
	if got := resident(e, bob); got > bound {
		t.Fatalf("Bob's session holds %d bytes after his deletion, over its bound of %d", got, bound)
	}
	if bob.spilled(0) == nil && bob.spilled(1) == nil || bob.lost(0) || bob.lost(1) {
		t.Fatalf("Bob's entries in scratch files: %v and %v; expired: %v and %v", bob.spilled(0) != nil, bob.spilled(1) != nil, bob.lost(0), bob.lost(1))
	}
	wantCharges(t, e)
	wantUndo(t, "undo of the deletion", undo(t, bob), undoAck{Applied: 1})
	wantUndo(t, "undo of the formula", undo(t, bob), undoAck{Applied: 1})
	e.wantText("Sheet1!A1", "=D!A1")
}

// resident returns what a person's session holds in undo memory.
func resident(e *env, p *person) int64 {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	e.in.smu.Lock()
	defer e.in.smu.Unlock()
	return p.s.undo.resident()
}
