package sheet

import (
	"fmt"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// A person's action whose format operation finds no free style id is
// refused as a whole, also when a later operation of the same action is
// ignored or refused on its own and taken back. That later revert does not
// take the failure of the earlier operation with it: the earlier one stays
// applied, with the default format standing in for the state it could not
// intern. Each case runs with no id left, and with one, which an earlier
// format of the action takes. The refusal leaves the sheet, its stamps, its
// sequence, the ids and sheet.db as they were, and a load finds them so.
// After the ids are found again the same action, with the same number,
// applies.
func TestStyleExhaustionBelongsToItsAction(t *testing.T) {
	later := map[string]func(e *env) (op, status string){
		"ignored": func(e *env) (string, string) {
			op := e.setOp("Sheet1!A5", "stale")
			e.mustAgent(`{"ops":[{"op":"delete_rows","tab":"Sheet1","at":5,"count":1}]}`)
			return op, "ignored:gone"
		},
		"refused": func(e *env) (string, string) {
			return jsonf(`{"op":"width","tab":%d,"cols":%s,"width":10}`, e.tabID("Sheet1"), e.colsOf("Sheet1", 1, 1)), "refused:invalid"
		},
	}
	for name, trailing := range later {
		for _, earlier := range []bool{false, true} {
			t.Run(fmt.Sprintf("a later %s operation, earlier ids taken %v", name, earlier), func(t *testing.T) {
				e := start(t)
				e.mustAgent(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"bold":true}},{"op":"format","tab":"Sheet1","ranges":["A2"],"format":` + symbolFormat(0) + `}]}`)
				kept := e.sentinels("Sheet1!A1", "Sheet1!A2")
				last, lastStatus := trailing(e)
				alice := e.person("Alice")
				var ops []string
				left, want := uint64(0), "applied,"+lastStatus
				if earlier {
					ops = append(ops, e.formatOp("Sheet1!B1", `{"underline":true}`))
					left, want = 1, "applied,"+want
				}
				ops = append(ops, e.formatOp("Sheet1!A1", `{"italic":true}`), last)
				seq, before := e.seq(), e.dump()
				ids := styleIDs{next: maxStyleID + 1 - left}
				leaveIDs(e, left)
				_, err := alice.try(seq, ops...)
				if werr := refusal(err); werr == nil || werr.Code != "unavailable" || werr.Reason != "ids_exhausted" || !werr.Retry {
					t.Fatalf("the action was not refused for its style ids: %v", err)
				}
				if e.seq() != seq || e.dump() != before {
					t.Fatal("the refused action changed the sheet")
				}
				if got := e.styleIDs(); !sameIDs(got, ids) {
					t.Fatalf("the refused action left the ids at %+v, before %+v", got, ids)
				}
				e.in.mu.RLock()
				full := e.in.wb.styles.full
				e.in.mu.RUnlock()
				if full {
					t.Fatal("the table is still marked full after the revert")
				}
				kept.check(e, "after the refusal")
				e.wantStored("after the refusal")
				// The tick finds the ids again, and the action applies with
				// the number it had.
				e.tick()
				ack, err := alice.send(alice.n, seq, ops...)
				if err != nil {
					t.Fatalf("the action sent again: %v", err)
				}
				if got := status(ack); got != want {
					t.Fatalf("the action sent again: %s, want %s", got, want)
				}
				wantA1 := `{"bold":true,"italic":true}`
				if got := e.cellFormat("Sheet1!A1"); got != wantA1 {
					t.Fatalf("A1 has the format %s, want %s", got, wantA1)
				}
				e.wantStored("after the action")
				applied := e.dump()
				e.restart()
				if e.dump() != applied {
					t.Fatal("the sheet loads otherwise than it was")
				}
				if got := e.cellFormat("Sheet1!A1"); got != wantA1 {
					t.Fatalf("after the load A1 has the format %s", got)
				}
				if earlier {
					if got := e.cellFormat("Sheet1!B1"); got != `{"underline":true}` {
						t.Fatalf("after the load B1 has the format %s", got)
					}
				}
			})
		}
	}
}

// cellFormat returns the format of a cell's style as JSON, also for a
// cell with no content.
func (e *env) cellFormat(addr string) string {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t, row, col := e.at(addr)
	var f formula.Format
	if c := t.cell(row, col); c != nil {
		f = e.in.wb.styles.format(c.style)
	}
	data, err := appendFormat(nil, f)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(data)
}

// A refused call and a refused action do not leave the style table's log
// of given ids with the room of the thousands of states they interned:
// after the revert, and after the tick, the log keeps no more room than
// it had before them, or the room for 1,024 ids that a commit keeps.
func TestStyleLogRoomGoesWithARefusal(t *testing.T) {
	const cells = 3000
	e := start(t)
	e.mustAgent(jsonf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d},`, cells) + newFormats("C", cells, 1) + `]}`)
	room := func() int {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		return cap(e.in.wb.styles.log)
	}
	wantRoom := func(when string, before int) {
		t.Helper()
		if got := room(); got > max(before, 1024) {
			t.Fatalf("%s the log keeps room for %d ids, before %d", when, got, before)
		}
		e.tick()
		if got := room(); got > max(before, 1024) {
			t.Fatalf("after the tick %s the log keeps room for %d ids, before %d", when, got, before)
		}
	}
	before := room()
	if _, err := e.agent(`{"ops":[` + newFormats("D", cells, cells+1) + `,{"op":"delete_tab","tab":"Nowhere"}]}`); err == nil {
		t.Fatal("the call was applied")
	}
	wantRoom("after the refused call", before)
	// The person's format makes a state for each of the column's formats
	// and finds no id for the last.
	alice := e.person("Alice")
	before = room()
	leaveIDs(e, cells-1)
	op := jsonf(`{"op":"format","tab":%d,"rows":%s,"cols":%s,"set":{"italic":true}}`, e.tabID("Sheet1"), e.rowsOf("Sheet1", 1, cells), e.colsOf("Sheet1", 3, 1))
	_, err := alice.try(e.seq(), op)
	if werr := refusal(err); werr == nil || werr.Reason != "ids_exhausted" {
		t.Fatalf("the action was not refused for its ids: %v", err)
	}
	wantRoom("after the refused action", before)
}
