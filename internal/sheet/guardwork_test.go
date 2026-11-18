package sheet

import (
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// A range guard reads every cell record of its range, and a call may name
// the same cells in many guards. That work is counted on the budget of the
// call like the work of its operations (technical design, sections 7.4 and
// 10.4), and the deadline ends it between two rows of a range.

// numberSheet fills the first rows of Sheet1, 700 columns wide, with the
// number 1 as one fill of a person, and returns the cells it holds.
func numberSheet(e *env, p *person, rows int) (cells int) {
	e.t.Helper()
	const cols = 700
	e.mustAgent(jsonf(`{"ops":[{"op":"insert_cols","tab":"Sheet1","at":"A","count":%d}]}`, cols-newTabCols))
	p.set("A1", "1")
	ack := p.do(jsonf(`{"op":"fill","tab":%d,"source":{"rows":%s,"cols":%s},"target":{"rows":%s,"cols":"all"}}`, e.tabID("Sheet1"),
		e.rowsOf("Sheet1", 1, 1), e.colsOf("Sheet1", 1, 1), e.rowsOf("Sheet1", 1, rows)))
	if status(ack) != "applied" {
		e.t.Fatalf("the fill of the sheet: %s", status(ack))
	}
	return rows * cols
}

// guarded is a call on the given state with the given guard ranges and one
// small write.
func guarded(state, opID string, ranges []string) string {
	return jsonf(`{"state":%q,"op_id":%q,"guard":{"ranges":["%s"]},"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[2]]}]}`,
		state, opID, strings.Join(ranges, `","`))
}

func TestGuardsCountTheirWorkAndMeetTheDeadline(t *testing.T) {
	// The sheet of 499,800 cells; fewer rows under the race detector.
	rows := raced(714, 100)
	whole := jsonf("Sheet1!A1:ZX%d", rows)
	shapes := map[string]func(n int) []string{
		// The same range again and again.
		"repeated": func(n int) []string {
			ranges := make([]string, n)
			for i := range ranges {
				ranges[i] = whole
			}
			return ranges
		},
		// Ranges that are all different and share nearly all their cells.
		"overlapping": func(n int) []string {
			ranges := make([]string, n)
			for i := range ranges {
				ranges[i] = jsonf("Sheet1!A1:%s%d", columnLetters(699-i%350), rows-i/350%(rows/2))
			}
			return ranges
		},
	}
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			e := start(t, unhurried)
			alice, bob := e.person("Alice"), e.person("Bob")
			cells := numberSheet(e, alice, rows)
			const few, many = 20, 100000

			// Without a deadline the call applies, and its guards counted
			// one unit for each cell record they read: the ranges of
			// both shapes hold at least half the cells.
			deadline := e.workDeadline()
			got, whole := deadline.run(never, func() string { return agentStatus(e, guarded(e.state(), "", shape(few))) })
			if got != "applied" {
				t.Fatalf("the guarded call: %s", got)
			}
			least := int64(few) * int64(cells) / 2
			if work := e.in.lastWork.Load(); work < least {
				t.Errorf("%d guards over %d cells counted %d units of work, for at least %d", few, cells, work, least)
			}
			// The guards looked at the deadline at least every eighth of
			// their work.
			if gap := whole.longest(); gap > whole.used/8 {
				t.Errorf("%d guards counted %d of their %d units between two looks at the deadline", few, gap, whole.used)
			}

			// Bob has an edit that no revision holds yet. A call that
			// applies records it first; a refused one must not.
			bob.set("A2", "3")
			revisions := e.b.importAll(t)
			count := func(query string) (n int) {
				t.Helper()
				if err := e.in.rdb.QueryRow(query).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			before, kept := e.dump(), e.memory("Sheet1", alice)
			deltas, groups, commands := len(e.in.hub.log), count(`SELECT count(*) FROM groups`), count(`SELECT count(*) FROM commands`)
			unchanged := func(what string) {
				t.Helper()
				if after := e.dump(); after != before {
					t.Fatalf("%s changed the sheet: %s", what, dumpDiff(before, after))
				}
				if !kept.equal(e.memory("Sheet1", alice)) {
					t.Errorf("%s changed the sort stamps, the structural log or the undo stacks", what)
				}
				if n := len(e.in.hub.log); n != deltas {
					t.Errorf("%s sent %d deltas", what, n-deltas)
				}
				if n := count(`SELECT count(*) FROM groups`); n != groups {
					t.Errorf("%s left %d edit groups open, of %d", what, n, groups)
				}
				if n := count(`SELECT count(*) FROM commands`); n != commands {
					t.Errorf("%s stored %d results", what, n-commands)
				}
				if n := e.b.importAll(t); n != revisions {
					t.Errorf("%s recorded %d revisions", what, n-revisions)
				}
			}

			// The deadline of the call below, which does many/few times
			// that work, passes once it has done as much, long before the
			// last of its guards.
			deadline.refusedAt(t, whole.used, whole.used/8, func() string {
				return agentStatus(e, guarded(e.state(), "many-"+name, shape(many)))
			})
			unchanged("the refused call")

			// A refused call stores no result under its op_id: the same
			// bytes are taken as a new call once the limit allows it, and
			// answered from the stored result after that.
			call := guarded(e.state(), "few-"+name, shape(few))
			if got, _ := deadline.run(0, func() string { return agentStatus(e, call) }); got != "refused:recalculation_time" {
				t.Fatalf("%d guards after the deadline: %s", few, got)
			}
			unchanged("the second refused call")
			res, err := e.agent(call)
			if err != nil || res.Replay {
				t.Fatalf("the call after its refusal: %+v, %v", res, err)
			}
			seq := e.seq()
			again, err := e.agent(call)
			if err != nil || !again.Replay || again.State != res.State || e.seq() != seq {
				t.Fatalf("the call sent again: %+v, %v", again, err)
			}
			if n := count(`SELECT count(*) FROM commands`); n != commands+1 {
				t.Errorf("the call stored %d results", n-commands)
			}
		})
	}
}

// A single range can hold every record of a sheet, so the deadline ends
// the reading of one range between two of its rows, not only between two
// guards. The clock here says the deadline has passed from its second
// reading on, which the budget takes after its first 4,096 units of work.
func TestGuardStopsInsideItsRange(t *testing.T) {
	rows := raced(714, 100)
	e := start(t, unhurried)
	cells := numberSheet(e, e.person("Alice"), rows)
	r, err := formula.ParseRange(jsonf("Sheet1!A1:ZX%d", rows))
	if err != nil {
		t.Fatal(err)
	}
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	wb := e.in.wb
	x := newTxn(wb, wb.seq+1, wb.seq, 0)
	e.in.begin(x)
	reads := 0
	x.overdue = func() bool { reads++; return reads > 1 }
	x.budget = formula.NewBudget(x.overdue)
	_, changed, werr := x.guardRange(wb.tab("Sheet1"), r, wb.seq)
	if werr == nil || werr.Limit != "recalculation_time" {
		t.Fatalf("a guard over %d cells past its deadline: changed %v, %v", cells, changed, werr)
	}
	if used := x.budget.Used(); used == 0 || used > int64(cells)/2 {
		t.Errorf("the guard was refused after %d units of work, for a range of %d records", used, cells)
	}
}
