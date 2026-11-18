package sheet

import (
	"testing"
	"time"
)

// A person's operation that leaves the sheet as it is changes nothing: a
// tab moved to the place it has, settings set to the ones the tab has, a
// tab renamed to its own name, a column given the width it shows, and a
// sort of rows that are all frozen. Each applies, and its action takes no
// sequence, adds no undo entry and records no revision, as every action
// that changes nothing (technical design, Appendix B, engine item 6). An
// operation that changes something, in the same way, does all three.
func TestPersonOperationsThatChangeNothing(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Other"},{"op":"write","tab":"Sheet1","at":"A1","rows":[[2],[1],[3]]},{"op":"widths","tab":"Sheet1","cols":"B:B","width":200}]}`)
	alice := e.person("Alice")
	sheet, other := e.tabID("Sheet1"), e.tabID("Other")
	if ack := alice.do(jsonf(`{"op":"settings","tab":%d,"frozen_rows":2,"frozen_cols":1}`, sheet)); status(ack) != "applied" {
		t.Fatalf("the settings: %s", status(ack))
	}
	// revisions closes every edit group and returns the revisions the host
	// has imported.
	revisions := func() int {
		t.Helper()
		e.clock.Add(time.Hour)
		e.tick()
		return e.b.importAll(t)
	}
	entries := func() int {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		return len(alice.s.undo.undo)
	}
	cases := []struct {
		name string
		op   string
	}{
		{"a tab moved before the tab after it", jsonf(`{"op":"move_tab","tab":%d,"before":%d}`, sheet, other)},
		{"the last tab moved to the end", jsonf(`{"op":"move_tab","tab":%d,"before":null}`, other)},
		{"settings the tab has", jsonf(`{"op":"settings","tab":%d,"frozen_rows":2,"frozen_cols":1}`, sheet)},
		{"settings of no frozen panes on a tab without them", jsonf(`{"op":"settings","tab":%d,"frozen_rows":0,"frozen_cols":0}`, other)},
		{"a tab renamed to its name", jsonf(`{"op":"rename_tab","tab":%d,"name":"Sheet1"}`, sheet)},
		{"the width a column has", jsonf(`{"op":"width","tab":%d,"cols":%s,"width":200}`, sheet, e.colsOf("Sheet1", 2, 1))},
		{"the default width of a column without one", jsonf(`{"op":"width","tab":%d,"cols":%s,"width":%d}`, sheet, e.colsOf("Sheet1", 3, 1), defaultWidth)},
		{"a sort of frozen rows", e.sortOp("Sheet1", 1, 2, 1, false)},
	}
	for _, c := range cases {
		seq, held, revs, before := e.seq(), entries(), revisions(), e.dump()
		if ack := alice.do(c.op); status(ack) != "applied" {
			t.Fatalf("%s: %s", c.name, status(ack))
		}
		if e.seq() != seq || entries() != held || e.dump() != before {
			t.Errorf("%s: the sequence went from %d to %d and the undo stack from %d to %d entries", c.name, seq, e.seq(), held, entries())
		}
		if got := revisions(); got != revs {
			t.Errorf("%s: %d revisions after it, %d before", c.name, got, revs)
		}
	}
	// The same operations that change the sheet take a sequence, an entry
	// and a revision each.
	for _, op := range []string{
		jsonf(`{"op":"move_tab","tab":%d,"before":null}`, sheet),
		jsonf(`{"op":"settings","tab":%d,"frozen_rows":1,"frozen_cols":1}`, other),
		jsonf(`{"op":"rename_tab","tab":%d,"name":"sheet1"}`, sheet),
		jsonf(`{"op":"width","tab":%d,"cols":%s,"width":201}`, sheet, e.colsOf("sheet1", 2, 1)),
	} {
		seq, held, revs := e.seq(), entries(), revisions()
		if ack := alice.do(op); status(ack) != "applied" {
			t.Fatalf("%s: %s", op, status(ack))
		}
		if e.seq() != seq+1 || entries() != held+1 || revisions() != revs+1 {
			t.Errorf("%s: the sequence went from %d to %d, the undo stack from %d to %d entries, and the revisions are not one more", op, seq, e.seq(), held, entries())
		}
	}
}
