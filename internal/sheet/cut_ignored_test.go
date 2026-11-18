package sheet

import (
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// cutSheet is the sheet of the cut tests: formulas and text in A1:B2, each
// with a format of its own, reading numbers in H1:H2.
const cutSheet = `{"ops":[
	{"op":"write","tab":"Sheet1","at":"A1","rows":[["=H1*2","two"],["three","=H2+1"]]},
	{"op":"write","tab":"Sheet1","at":"H1","rows":[[5],[7]]},
	{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"bold":true,"fill":"green-100"}},
	{"op":"format","tab":"Sheet1","ranges":["B1"],"format":{"italic":true}},
	{"op":"format","tab":"Sheet1","ranges":["A2"],"format":{"fill":"red-200"}},
	{"op":"format","tab":"Sheet1","ranges":["B2"],"format":{"number":{"kind":"percent","decimals":1}}}]}`

// cutCell is what a test checks of one cell: its text, what it shows and its
// format.
type cutCell struct {
	text, shown string
	format      formula.Format
}

func (e *env) cutCell(addr string) cutCell {
	e.t.Helper()
	return cutCell{e.text(addr), e.shown(addr), e.styleAt(addr)}
}

// TestCutKeepsSourcesWhoseDestinationWasDeleted cuts and pastes from the
// real client into the real engine while another editor deletes rows or
// columns of the destination before the paste arrives. A source cell
// moves only to a destination that applied: one whose destination was
// deleted keeps its content, formula and format (rule 32). The ack counts
// the destinations that applied and those ignored, and undo takes back
// only what moved.
func TestCutKeepsSourcesWhoseDestinationWasDeleted(t *testing.T) {
	node := nodeOrSkip(t)
	type moved struct{ from, to string }
	cases := []struct {
		name string
		// select and paste are positions of the client's selection.
		cut, paste []int
		// delete lists Bob's actions, sent after the client prepared the
		// paste and before the host received it.
		delete           func(e *env) []string
		applied, ignored int
		// moved lists the sources that moved and the address their
		// content has after Bob's deletion; kept lists those that stay.
		moved []moved
		kept  []string
		// blank lists addresses that the cut must leave empty: where the
		// deleted destinations' neighbours now are.
		blank []string
	}{{
		name: "destination row deleted", cut: []int{0, 0, 0, 0}, paste: []int{2, 2, 2, 2},
		delete:  func(e *env) []string { return []string{e.deleteRowsOp("Sheet1", 3, 1)} },
		applied: 0, ignored: 1, kept: []string{"A1", "B1", "A2", "B2"}, blank: []string{"C2", "C3"},
	}, {
		name: "destination column deleted", cut: []int{0, 0, 0, 0}, paste: []int{0, 3, 0, 3},
		delete:  func(e *env) []string { return []string{e.deleteColsOp("Sheet1", 4, 1)} },
		applied: 0, ignored: 1, kept: []string{"A1", "B1", "A2", "B2"}, blank: []string{"C1", "D1"},
	}, {
		name: "destination rows partly deleted", cut: []int{0, 0, 1, 0}, paste: []int{3, 2, 4, 2},
		delete:  func(e *env) []string { return []string{e.deleteRowsOp("Sheet1", 5, 1)} },
		applied: 1, ignored: 1, moved: []moved{{"A1", "C4"}}, kept: []string{"B1", "A2", "B2"}, blank: []string{"C5"},
	}, {
		name: "destination columns partly deleted", cut: []int{0, 0, 0, 1}, paste: []int{3, 2, 3, 3},
		delete:  func(e *env) []string { return []string{e.deleteColsOp("Sheet1", 4, 1)} },
		applied: 1, ignored: 1, moved: []moved{{"A1", "C4"}}, kept: []string{"B1", "A2", "B2"}, blank: []string{"D4"},
	}, {
		name: "both lines of a block partly deleted", cut: []int{0, 0, 1, 1}, paste: []int{3, 2, 4, 3},
		delete: func(e *env) []string {
			return []string{e.deleteRowsOp("Sheet1", 5, 1), e.deleteColsOp("Sheet1", 4, 1)}
		},
		applied: 1, ignored: 3, moved: []moved{{"A1", "C4"}}, kept: []string{"B1", "A2", "B2"}, blank: []string{"C5", "D4", "D5"},
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(cutSheet)
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(e.tabID("Sheet1"))
			steps, _ := r.taken()
			actions := gestures(t, node, steps,
				map[string]any{"select": c.cut}, map[string]any{"cut": true},
				map[string]any{"select": c.paste}, map[string]any{"paste": "all"},
			)
			if len(actions) != 1 {
				t.Fatalf("the client sent %d actions, want 1: %s", len(actions), actions)
			}

			bob := e.person("Bob")
			for _, op := range c.delete(e) {
				if ack := bob.do(op); status(ack) != "applied" {
					t.Fatalf("Bob's deletion was answered %s", status(ack))
				}
			}
			// The sources as the host holds them when the paste arrives: a
			// deleted column moves the references of their formulas.
			before := map[string]cutCell{}
			for _, a := range []string{"A1", "B1", "A2", "B2"} {
				before[a] = e.cutCell(a)
				if before[a].format == (formula.Format{}) {
					t.Fatalf("%s has no format to keep", a)
				}
			}
			filled := e.filled("Sheet1")
			ann := e.person("Ann")
			ack := sendAll(e, ann, actions)[0]
			if len(ack.Results) != 1 || ack.Results[0].Status != "applied" || ack.Results[0].Applied != c.applied || ack.Results[0].Ignored != c.ignored {
				t.Fatalf("the cut was answered %+v, want %d applied and %d ignored", ack.Results, c.applied, c.ignored)
			}
			for _, a := range c.kept {
				if got := e.cutCell(a); got != before[a] {
					t.Errorf("%s is %+v after a cut whose destination was deleted, want it kept as %+v", a, got, before[a])
				}
			}
			for _, m := range c.moved {
				if got := e.cutCell(m.from); got != (cutCell{}) {
					t.Errorf("the moved source %s is %+v, want it empty without a format", m.from, got)
				}
				if got := e.cutCell(m.to); got != before[m.from] {
					t.Errorf("%s is %+v, want what %s held: %+v", m.to, got, m.from, before[m.from])
				}
			}
			for _, a := range c.blank {
				if got := e.cutCell(a); got != (cutCell{}) {
					t.Errorf("%s is %+v after the cut, want it empty", a, got)
				}
			}
			if got := e.filled("Sheet1"); got != filled {
				t.Errorf("the sheet has %d filled cells after the cut, want %d as before it", got, filled)
			}

			// Undo takes back the destinations that applied and the
			// sources that moved to them, and nothing else: the content and
			// every format property of each.
			u := undo(t, ann)
			if want := 2 * c.applied * (1 + propCount); u.Applied != want || u.Skipped != 0 {
				t.Errorf("undo of the cut = %+v, want %d applied and none skipped", u, want)
			}
			for _, a := range append(c.kept, "A1") {
				if got := e.cutCell(a); got != before[a] {
					t.Errorf("%s is %+v after undo, want %+v", a, got, before[a])
				}
			}
			for _, m := range c.moved {
				if got := e.cutCell(m.to); got != (cutCell{}) {
					t.Errorf("%s is %+v after undo, want it empty", m.to, got)
				}
			}
			if u := undo(t, ann); u.Applied != 0 || u.Skipped != 0 {
				t.Errorf("a second undo = %+v, want no earlier undo", u)
			}
		})
	}
}

// TestPartialCutNoticeTellsDeletedSources cuts A1:A2 from the real client
// and pastes it at C1 while another editor deletes row 2, the second
// source and its destination, before the paste arrives. A1 moves and the
// cell of A2 is gone with its row. The notice does not say that every
// cell that did not move kept its content, and it offers back the text of
// the cut, which holds the deleted cell's text.
func TestPartialCutNoticeTellsDeletedSources(t *testing.T) {
	node := nodeOrSkip(t)
	e := start(t)
	e.mustAgent(cutSheet)
	s := serve(e)
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(e.tabID("Sheet1"))
	steps, _ := r.taken()
	cut := []any{
		map[string]any{"select": []int{0, 0, 1, 0}}, map[string]any{"cut": true},
		map[string]any{"select": []int{0, 2, 0, 2}}, map[string]any{"paste": "all"},
	}
	sent := play(t, node, steps, cut...).Frames
	if len(sent) != 1 || !strings.Contains(string(sent[0]), `"cut":true`) {
		t.Fatalf("the client sent %s, want one cut", sent)
	}
	if ack := e.person("Bob").do(e.deleteRowsOp("Sheet1", 2, 1)); status(ack) != "applied" {
		t.Fatalf("Bob's deletion was answered %s", status(ack))
	}
	source := e.cutCell("A1")
	r.sync()
	ack := r.answer(sent[0])
	if len(ack.Results) != 1 || ack.Results[0].Applied != 1 || ack.Results[0].Ignored != 1 {
		t.Fatalf("the cut was answered %+v, want 1 applied and 1 ignored", ack.Results)
	}
	if got := e.cutCell("C1"); got.format != source.format || !strings.HasPrefix(got.text, "=") {
		t.Errorf("C1 is %+v after the cut, want the formula of A1 with its format %+v", got, source)
	}
	if got := e.cutCell("A1"); got != (cutCell{}) {
		t.Errorf("A1 is %+v after it moved, want it empty", got)
	}

	later, _ := r.taken()
	run := play(t, node, steps, append(cut, map[string]any{"steps": later[len(steps):]})...)
	if len(run.Notices) == 0 {
		t.Fatal("the client showed no notice for a cut that moved only a part")
	}
	last := len(run.Notices) - 1
	m := cutNotice.FindStringSubmatch(run.Notices[last])
	if m == nil || m[1] != "1" || m[2] != "2" || m[3] != "1" {
		t.Errorf("the client said %q, want that 1 of 2 cells moved and that a cell of a deleted row is gone", run.Notices[last])
	}
	if k := run.Kept[last]; k == nil || *k != "10\nthree" {
		t.Errorf("the notice offers back %v, want the text of the cut with the deleted cell's text", k)
	}
}

// TestCutOverARepeatedTarget cuts one cell over a larger target, which
// the wire allows: the source moves when at least one of its destinations
// applied, and stays when every one of them was deleted.
func TestCutOverARepeatedTarget(t *testing.T) {
	for _, c := range []struct {
		name             string
		deleted          int // rows of the target deleted, from its first
		applied, ignored int
	}{
		{"one destination deleted", 1, 1, 1},
		{"every destination deleted", 2, 0, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(cutSheet)
			tab := e.tabID("Sheet1")
			cut := jsonf(`{"op":"copy","from":{"tab":%d,"rows":%s,"cols":%s},"to":{"tab":%d,"rows":%s,"cols":%s},"mode":"all","cut":true}`,
				tab, e.rowsOf("Sheet1", 1, 1), e.colsOf("Sheet1", 1, 1), tab, e.rowsOf("Sheet1", 4, 2), e.colsOf("Sheet1", 3, 1))
			source := e.cutCell("A1")
			e.person("Bob").do(e.deleteRowsOp("Sheet1", 4, c.deleted))
			ack := e.person("Ann").do(cut)
			if ack.Results[0].Applied != c.applied || ack.Results[0].Ignored != c.ignored {
				t.Fatalf("the cut was answered %+v, want %d applied and %d ignored", ack.Results, c.applied, c.ignored)
			}
			if c.applied == 0 {
				if got := e.cutCell("A1"); got != source {
					t.Errorf("A1 is %+v after a cut with no destination left, want %+v", got, source)
				}
				return
			}
			if got := e.cutCell("A1"); got != (cutCell{}) {
				t.Errorf("A1 is %+v after it moved, want it empty", got)
			}
			if got := e.cutCell("C4"); got != source {
				t.Errorf("C4 is %+v, want the source %+v", got, source)
			}
		})
	}
}

// rowCount is the number of rows of a tab.
func (e *env) rowCount(tab string) int {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	return e.in.wb.tab(tab).rows.len()
}

// TestPasteThatAppliesNowhereAddsNoRows pastes past the last row, which
// adds rows for the paste, while another editor deletes what the paste
// needs: the source of a copy from the real client, or the column of a
// paste of text. No cell is written, so no row is added either, and the
// paste is reported as ignored.
func TestPasteThatAppliesNowhereAddsNoRows(t *testing.T) {
	t.Run("copy whose source was deleted", func(t *testing.T) {
		node := nodeOrSkip(t)
		e := start(t)
		e.mustAgent(cutSheet)
		s := serve(e)
		r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
		r.watch(e.tabID("Sheet1"))
		steps, _ := r.taken()
		last := e.rowCount("Sheet1") - 1
		actions := gestures(t, node, steps,
			map[string]any{"select": []int{0, 0, 1, 0}}, map[string]any{"copy": true},
			map[string]any{"select": []int{last, 2, last, 2}}, map[string]any{"paste": "all"},
		)
		if len(actions) != 1 || !strings.Contains(string(actions[0][0]), `{"new":1}`) {
			t.Fatalf("the client sent %s, want one copy that adds a row", actions)
		}
		e.person("Bob").do(e.deleteRowsOp("Sheet1", 1, 2))
		rows, seq := e.rowCount("Sheet1"), e.seq()
		ack := sendAll(e, e.person("Ann"), actions)[0]
		if status(ack) != "ignored:gone" {
			t.Errorf("the paste was answered %s %+v, want it ignored", status(ack), ack.Results)
		}
		if got := e.rowCount("Sheet1"); got != rows || e.seq() != seq {
			t.Errorf("the tab has %d rows at sequence %d after a paste that wrote nothing, want %d at %d", got, e.seq(), rows, seq)
		}
	})
	t.Run("text whose column was deleted", func(t *testing.T) {
		e := start(t)
		e.mustAgent(cutSheet)
		tab := e.tabID("Sheet1")
		last := e.rowCount("Sheet1")
		runs := e.rowsOf("Sheet1", last, 1)
		write := jsonf(`{"op":"write","tab":%d,"rows":[%s,{"new":1}],"cols":%s,"values":[["x"],["y"]]}`,
			tab, runs[1:len(runs)-1], e.colsOf("Sheet1", 3, 1))
		e.person("Bob").do(e.deleteColsOp("Sheet1", 3, 1))
		rows, seq := e.rowCount("Sheet1"), e.seq()
		ack := e.person("Ann").do(write)
		if status(ack) != "ignored:gone" {
			t.Errorf("the paste was answered %s %+v, want it ignored", status(ack), ack.Results)
		}
		if got := e.rowCount("Sheet1"); got != rows || e.seq() != seq {
			t.Errorf("the tab has %d rows at sequence %d after a paste that wrote nothing, want %d at %d", got, e.seq(), rows, seq)
		}
	})
}
