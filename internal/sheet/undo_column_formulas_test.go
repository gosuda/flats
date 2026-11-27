package sheet

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// TestUndoOfAColumnCountsItsFormulas has Alice delete a column of 3,000
// formulas of 7,168 characters, each a sum of 1,024 references to a tab
// that does not exist, and undo the deletion. Such references make no
// entries of the dependency index, so nothing but the formulas' text
// counts the work of checking their length on the way back. The undo
// reads the clock at least once for each 4,096 units of 64 bytes of that
// text, so a deadline that passes stops it within that much work, with
// the sheet and the entry as they were. The same entry then applies.
func TestUndoOfAColumnCountsItsFormulas(t *testing.T) {
	slow(t, "brings back 21 MB of formulas")
	count := raced(3000, 600)
	text := longRefs(1024, func(int) string { return "Z!$A$1" })
	if len(text) != 7168 || len(text) <= formula.MaxFormulaChars/2 {
		t.Fatalf("the formula has %d characters", len(text))
	}
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
			for first := 1; first <= count; first += 1000 {
				rows := make([]string, min(1000, count-first+1))
				for i := range rows {
					rows[i] = fmt.Sprintf("[%q]", text)
				}
				e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"B%d","rows":[%s]}]}`, first, strings.Join(rows, ",")))
			}
			alice := e.person("Alice")
			if ack := alice.do(e.deleteColsOp("Sheet1", 2, 1)); status(ack) != "applied" {
				t.Fatal(status(ack))
			}
			if (alice.spilled(0) != nil) != spilled {
				t.Fatalf("the deletion's payload in a scratch file: %v", alice.spilled(0) != nil)
			}

			// The clock is read each time the budget has counted 4,096
			// more units.
			reads := 0
			err, whole := stoppedUndoWith(t, e, alice, false, 1<<62, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			e.in.mu.Lock()
			wb, u := e.in.wb, &alice.s.undo
			x := newTxn(wb, wb.seq+1, wb.seq, alice.s.ord)
			e.in.begin(x)
			x.overdue = nil
			x.budget = formula.NewBudget(func() bool { reads++; return false })
			_, _, _, uerr := x.applyUndo(u, u.undo[len(u.undo)-1])
			wb.revert(&x.j)
			wb.settle()
			e.in.mu.Unlock()
			if uerr != nil {
				t.Fatal(uerr)
			}
			measured := int64(count * len(text))
			if want := measured/64/4096 - 1; int64(reads) < want {
				t.Errorf("the undo read the clock %d times over %d bytes of formulas, want at least %d", reads, measured, want)
			}

			oneFormula := int64(len(text)+63) / 64
			stops := 0
			for deadline := int64(4096); deadline < whole; deadline += max(4096, whole/24) {
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
			e.wantText(fmt.Sprintf("Sheet1!B%d", count), text)
		})
	}
}
