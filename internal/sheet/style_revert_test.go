package sheet

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// symbolFormat returns a currency format whose symbol is the n-th of
// 512,000 that no other n has.
func symbolFormat(n int) string {
	symbol := string([]rune{rune(0x10400 + n%80), rune(0x10400 + n/80%80), rune(0x10400 + n/6400%80)})
	return jsonf(`{"number":{"kind":"currency","symbol":%q,"decimals":2}}`, symbol)
}

// numberFormat returns the number format of a cell's style.
func (e *env) numberFormat(addr string) formula.NumberFormat {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t, row, col := e.at(addr)
	if c := t.cell(row, col); c != nil {
		return e.in.wb.styles.format(c.style).Number
	}
	return formula.NumberFormat{}
}

// styleTableState returns how many states the style table holds, the ids
// it has given out and what its maps hold.
func styleTableState(e *env) (states int, next uint64, touched int) {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	s := &e.in.wb.styles
	if len(s.byID) != len(s.ids) {
		e.t.Fatalf("the style table holds %d states by id and %d by state", len(s.byID), len(s.ids))
	}
	return len(s.byID), s.next, len(s.touched)
}

// A call that does not commit leaves no style state behind: neither one a
// later operation refuses nor an action whose commit fails. Each round
// gives cells formats that no earlier round used, and only maintenance
// ticks run between the rounds: no change commits. The table holds the
// states the cells use and its ids are given back. The tick gives back
// the room the maps of the states and the wire list of formats kept, and
// the heap does not grow with the rounds. The states the sheet's cells use
// stay.
func TestStylesOfChangesThatDoNotCommitGo(t *testing.T) {
	slow(t, "gives thousands of cells formats of their own in calls that are refused")
	rounds := raced(8, 3)
	setup := func(t *testing.T) *env {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1001,"count":3000},{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"bold":true}},{"op":"format","tab":"Sheet1","ranges":["B1"],"format":` + symbolFormat(0) + `}]}`)
		e.tick()
		return e
	}
	// check runs the rounds and compares the table, the heap and the sheet
	// after each with what they were after the first.
	check := func(t *testing.T, e *env, round func(t *testing.T, n int)) {
		seq, before := e.seq(), e.dump()
		states, next, _ := styleTableState(e)
		e.in.mu.RLock()
		formats, logBefore := len(e.in.wb.wire.list), cap(e.in.wb.styles.log)
		e.in.mu.RUnlock()
		var heap int64
		for n := range rounds {
			round(t, n)
			e.tick()
			got, gotNext, touched := styleTableState(e)
			if got != states || gotNext != next || touched != 0 {
				t.Fatalf("after round %d the table holds %d states and gives out id %d, with %d touched; before the rounds %d states and id %d", n, got, gotNext, touched, states, next)
			}
			// The maps and the list keep no room for the states and formats
			// the round took out again.
			e.in.mu.RLock()
			st, wire, room := &e.in.wb.styles, len(e.in.wb.wire.list), cap(e.in.wb.wire.list)
			most, touchedMost, logRoom := st.most, st.touchedMost, cap(st.log)
			e.in.mu.RUnlock()
			if roomDue(most, got) || roomDue(touchedMost, touched) || wire != formats || roomDue(room, wire) {
				t.Fatalf("after round %d the states are noted at %d and the touched ids at %d; the wire list holds %d formats with room for %d, before the rounds %d", n, most, touchedMost, wire, room, formats)
			}
			// No action is open, so the log of given ids holds nothing, and
			// keeps no more room than a commit leaves it.
			if logRoom > max(logBefore, 1024) {
				t.Fatalf("after round %d the log of given ids keeps room for %d, before the rounds %d", n, logRoom, logBefore)
			}
			// After the first round the heap does not grow with the rounds.
			if n == 0 {
				heap = undoHeap(t, e)
				continue
			}
			if grown := undoHeap(t, e) - heap; measures && grown > 256<<10 {
				t.Errorf("after round %d the heap holds %d KiB more than after the first", n, grown>>10)
			}
		}
		if e.seq() != seq || e.dump() != before {
			t.Fatal("a change that did not commit changed the sheet")
		}
		if !e.bold("Sheet1!A1") || e.numberFormat("Sheet1!B1").Kind != formula.NumberCurrency {
			t.Fatalf("the formats in use: A1 bold %v, B1 %+v", e.bold("Sheet1!A1"), e.numberFormat("Sheet1!B1"))
		}
		e.wantStored("after the rounds")
	}
	t.Run("agent calls that a later operation refuses", func(t *testing.T) {
		e := setup(t)
		cells := raced(3000, 300)
		check(t, e, func(t *testing.T, n int) {
			ops := make([]string, 0, cells+1)
			for i := range cells {
				ops = append(ops, jsonf(`{"op":"format","tab":"Sheet1","ranges":["C%d"],"format":%s}`, i+1, symbolFormat(1+n*cells+i)))
			}
			ops = append(ops, `{"op":"delete_tab","tab":"Nowhere"}`)
			if _, err := e.agent(`{"ops":[` + strings.Join(ops, ",") + `]}`); err == nil {
				t.Fatalf("round %d: the call was applied", n)
			}
		})
	})
	t.Run("actions whose commit fails", func(t *testing.T) {
		e := setup(t)
		alice := e.person("Alice")
		cells := raced(maxActionOps, 200)
		check(t, e, func(t *testing.T, n int) {
			ops := make([]string, cells)
			for i := range ops {
				ops[i] = e.formatOp(fmt.Sprintf("Sheet1!C%d", i+1), symbolFormat(1+n*cells+i))
			}
			e.in.mu.Lock()
			e.in.fault = func() error { return errors.New("injected commit failure") }
			e.in.mu.Unlock()
			_, err := alice.try(e.seq(), ops...)
			e.in.mu.Lock()
			e.in.fault = nil
			e.in.mu.Unlock()
			if refusal(err).Reason != "commit" {
				t.Fatalf("round %d: a failed commit answered %v", n, err)
			}
		})
	})
	t.Run("a person's action whose undo cannot be written", func(t *testing.T) {
		e := setup(t)
		alice := e.person("Alice")
		cells := raced(maxActionOps, 200)
		// Every payload goes to a scratch file, and none can be written: the
		// action is refused after its formats applied.
		e.in.mu.Lock()
		e.in.undoPool().entryMax = -1
		e.in.undoPool().fault = func(op string) error {
			if op == "write" {
				return errors.New("no space left on device")
			}
			return nil
		}
		e.in.mu.Unlock()
		check(t, e, func(t *testing.T, n int) {
			ops := make([]string, cells)
			for i := range ops {
				ops[i] = e.formatOp(fmt.Sprintf("Sheet1!C%d", i+1), symbolFormat(1+n*cells+i))
			}
			if _, err := alice.try(e.seq(), ops...); refusal(err).Reason != "undo_scratch" {
				t.Fatalf("round %d: the action answered %v", n, err)
			}
		})
	})
}

// The tick forgets a state no cell uses also when no action interned it:
// here the collection of blank records takes the last cells of a state
// away, the stamps that clearing a format left on A1 and A2. Nothing
// commits after the collection.
func TestTickForgetsStylesNoCellUses(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["A1:A2"],"format":` + symbolFormat(5) + `},{"op":"format","tab":"Sheet1","ranges":["B1"],"format":{"bold":true}}]}`)
	e.mustAgent(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["A1:A2"],"format":{"number":null}}]}`)
	// Two newer blank records, so that A1 and A2 are the older half.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"C1","rows":[["x"],["y"]]}]}`)
	e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["C1:C2"]}]}`)
	before, _, _ := styleTableState(e)
	e.collectBlanks(1)
	after, _, _ := styleTableState(e)
	e.in.mu.RLock()
	used := e.in.wb.styles.used
	e.in.mu.RUnlock()
	if after != used || after != before-1 {
		t.Fatalf("after the collection the table holds %d states, %d of them in use; before it %d", after, used, before)
	}
	if !e.bold("Sheet1!B1") {
		t.Fatal("the state of B1 went")
	}
	e.wantStored("after the collection")
	// The next commit deletes the state's row in sheet.db too.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"D1","rows":[[1]]}]}`)
	var rows int
	if err := e.in.rdb.QueryRow(`SELECT count(*) FROM styles`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != used {
		t.Fatalf("sheet.db holds %d styles, the cells use %d", rows, used)
	}
}
