package sheet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"weak"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// A rollback drops every undo entry, and with them at once what they held:
// right after it, before any tick, the sheet's undo memory counts nothing
// and the scratch directory holds no payload file. An action whose payload
// fits the sheet's bound then applies and undoes while no scratch file can
// be written, as on a sheet without history. Before the rollback, the
// payloads in memory and that action's payload together pass the bound,
// so an action admitted against the dropped entries would have to spill
// and be refused.
//
// The instance ticks by itself every few seconds, and a tick counts the
// pool again. A file that no entry owns marks whether a tick came between
// the rollback and the action; such a run says nothing and is made again.
func TestRollbackReleasesUndoAtOnce(t *testing.T) {
	const bound = 2 << 20
	text := strings.Repeat("가", formula.MaxTextLen-7)
	cells := 8
	payload := int64(cells * len(text))
	ctx := context.Background()
	for attempt := 0; ; attempt++ {
		if attempt == 3 {
			t.Fatal("a tick came between the rollback and the action in every run")
		}
		e := boundedSheet(t, bound)
		n := e.b.importAll(t)
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"T"}]}`)
		e.fillColumn("T", "A", cells, func(int) string { return text })
		e.fillColumn("T", "B", cells, func(int) string { return text })
		e.fillColumn("T", "C", 12, func(row int) string {
			if row == 1 {
				return "=A5"
			}
			return text
		})
		alice, bob, carol := e.person("Alice"), e.person("Bob"), e.person("Carol")
		if ack := alice.do(e.clearOp("T", 1, cells, 1, 1)); status(ack) != "applied" {
			t.Fatalf("Alice's clear: %s", status(ack))
		}
		if ack := bob.do(e.clearOp("T", 1, cells, 2, 1)); status(ack) != "applied" {
			t.Fatalf("Bob's clear: %s", status(ack))
		}
		// Carol's payload is larger than an entry keeps in memory.
		if ack := carol.do(e.clearOp("T", 1, 12, 3, 1)); status(ack) != "applied" {
			t.Fatalf("Carol's clear: %s", status(ack))
		}
		if carol.spilled(0) == nil || alice.spilled(0) != nil || bob.spilled(0) != nil {
			t.Fatal("want Carol's payload alone in a scratch file")
		}
		// Her payload in the file holds a formula, so it waits for the
		// structural edits of its tab.
		e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"T","at":1,"count":1}]}`)
		if segments, open := e.editLog(); segments+open == 0 {
			t.Fatal("the edit log holds nothing for Carol's payload")
		}
		payloads, notes, marks := e.sheetUndo()
		if payloads+notes+marks+payload <= bound || payload >= bound {
			t.Fatalf("the sheet holds %d bytes of undo memory and the next payload is %d, which do not pass the bound %d together", payloads+notes+marks, payload, bound)
		}
		e.in.mu.RLock()
		dir := e.in.scratch.dir
		e.in.mu.RUnlock()
		unowned := filepath.Join(dir, "unowned")
		if err := os.WriteFile(unowned, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.b.Rollback(ctx, n, "Alice"); err != nil {
			t.Fatal(err)
		}
		files, _ := e.scratch()
		pool, stacks := e.residentUndo()
		payloads, notes, marks = e.sheetUndo()
		segments, open := e.editLog()
		e.fillColumn("Sheet1", "A", cells, func(int) string { return text })
		failWrites(e, true)
		ack, err := alice.try(e.seq(), e.clearOp("Sheet1", 1, cells, 1, 1))
		failWrites(e, false)
		if _, err := os.Stat(unowned); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if files != 1 || pool != 0 || stacks != 0 || payloads+notes+marks != 0 || segments+open != 0 {
			t.Errorf("right after the rollback the scratch directory holds %d files besides the unowned one, the pool counts %d bytes, the stacks %d, the sheet %d of payloads, %d of notes and %d of marks, and the edit log %d segments and %d open edits",
				files-1, pool, stacks, payloads, notes, marks, segments, open)
		}
		if err != nil || status(ack) != "applied" {
			t.Fatalf("Alice's clear of %d bytes after the rollback, with no scratch file to write: %s, %v", payload, status(ack), err)
		}
		if alice.spilled(0) != nil {
			t.Fatal("Alice's payload went to a scratch file")
		}
		wantUndo(t, "undo of Alice's clear", undo(t, alice), undoAck{Applied: cells})
		e.wantText("Sheet1!A1", text)
		wantCharges(t, e)
		return
	}
}

// A rollback lets go of the workbook whose collected blank records the
// store keeps at once, before any tick: with the store's file open, and
// with the file given up after it was lost, which closes it and leaves
// the store naming that workbook. The workbook is then gone from memory.
// A later collection keeps the records of the workbook the rollback
// installed, and an undo restores them.
//
// A tick lets go of the workbook as well. A file that no entry owns marks
// whether one came between the rollback and the check; such a run says
// nothing and is made again.
func TestRollbackLetsGoOfTheCollectedWorkbook(t *testing.T) {
	const rows, cols, limit = 40, 50, 300
	ctx := context.Background()
	for _, lose := range []bool{false, true} {
		name := "the file open"
		if lose {
			name = "the file lost"
		}
		t.Run(name, func(t *testing.T) {
			for attempt := 0; ; attempt++ {
				if attempt == 3 {
					t.Fatal("a tick came between the rollback and the check in every run")
				}
				e := start(t)
				e.in.mu.Lock()
				e.in.wb.collected.limit = limit
				e.in.mu.Unlock()
				fillNumbers(e, "Sheet1", rows, cols)
				n := e.b.importAll(t)
				alice := e.person("Alice")
				if ack := alice.do(e.clearOp("Sheet1", 1, rows, 1, cols)); status(ack) != "applied" {
					t.Fatalf("Alice's clear: %s", status(ack))
				}
				collectBelow(t, e, limit)
				if e.blankRows() == 0 {
					t.Fatal("no record on disk")
				}
				e.in.mu.Lock()
				b := &e.in.scratch.blanks
				dir := e.in.scratch.dir
				e.in.mu.Unlock()
				if lose {
					removeBlankFileOnRead(e)
					if u := undo(t, alice); u.Reason != "lost" {
						t.Fatalf("the undo of the clear over a lost file = %+v", u)
					}
				}
				e.in.mu.RLock()
				closed, owned := b.db == nil, b.owner == e.in.wb
				old := weak.Make(e.in.wb)
				e.in.mu.RUnlock()
				if closed != lose || !owned {
					t.Fatalf("before the rollback the store is closed: %v, and names the live workbook: %v", closed, owned)
				}
				unowned := filepath.Join(dir, "unowned")
				if err := os.WriteFile(unowned, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := e.b.Rollback(ctx, n, "Alice"); err != nil {
					t.Fatal(err)
				}
				e.in.mu.RLock()
				owner := b.owner != nil
				e.in.mu.RUnlock()
				if _, err := os.Stat(unowned); errors.Is(err, os.ErrNotExist) {
					continue
				}
				if owner {
					t.Fatal("right after the rollback the store still names the workbook before it")
				}
				for i := 0; old.Value() != nil; i++ {
					if i == 5 {
						t.Fatal("the workbook before the rollback is still in memory")
					}
					runtime.GC()
				}
				e.want("A1", "1001")
				// The limit is the workbook's, and the rollback installed
				// another one.
				e.in.mu.Lock()
				e.in.wb.collected.limit = limit
				e.in.mu.Unlock()
				bob := e.person("Bob")
				if ack := bob.do(e.clearOp("Sheet1", 1, rows, 1, cols)); status(ack) != "applied" {
					t.Fatalf("Bob's clear: %s", status(ack))
				}
				collectBelow(t, e, limit)
				e.in.mu.RLock()
				owned = b.owner == e.in.wb && b.db != nil
				e.in.mu.RUnlock()
				if !owned || e.blankRows() == 0 {
					t.Fatalf("the collection after the rollback keeps %d records, for the live workbook: %v", e.blankRows(), owned)
				}
				wantUndo(t, "undo of Bob's clear", undo(t, bob), undoAck{Applied: rows * cols})
				e.want("A1", "1001")
				return
			}
		})
	}
}

// collectBelow ticks until no more than limit blank records are left in
// the sheet: each tick takes the older half.
func collectBelow(t *testing.T, e *env, limit int) {
	t.Helper()
	for i := 0; e.blanks() > limit; i++ {
		if i == 20 {
			t.Fatalf("%d blank records are left after %d ticks", e.blanks(), i)
		}
		e.tick()
	}
}
