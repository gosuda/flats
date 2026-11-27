package sheet

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// overlapSheet writes values into the block at the top-left address at,
// each cell with a fill of its own, and numbers in J6:J7 that the formulas
// read. No row or column a case deletes holds J6 or J7.
func overlapSheet(at string, values [][]string) string {
	fills := []string{"red-100", "orange-200", "yellow-300", "green-400", "teal-600", "blue-800"}
	cells, _ := json.Marshal(values)
	ops := []string{
		fmt.Sprintf(`{"op":"write","tab":"Sheet1","at":%q,"rows":%s}`, at, cells),
		`{"op":"write","tab":"Sheet1","at":"J6","rows":[[5],[7]]}`,
	}
	col, row := at[0], int(at[1]-'0')
	k := 0
	for i, line := range values {
		for j := range line {
			addr := fmt.Sprintf("%c%d", col+byte(j), row+i)
			ops = append(ops, fmt.Sprintf(`{"op":"format","tab":"Sheet1","ranges":[%q],"format":{"fill":%q,"bold":%t}}`, addr, fills[k], k%2 == 0))
			k++
		}
	}
	return `{"ops":[` + strings.Join(ops, ",") + `]}`
}

// answer sends an action frame on the recorded connection, as the client
// sent it, and returns the host's ack. The deltas of the action are
// recorded before it returns.
func (r *recorder) answer(frame json.RawMessage) ackBody {
	r.t.Helper()
	var a struct{ N string }
	if err := json.Unmarshal(frame, &a); err != nil {
		r.t.Fatal(err)
	}
	if err := r.ws.Send(string(frame)); err != nil {
		r.t.Fatal(err)
	}
	var ack ackBody
	r.until("the ack of action "+a.N, func() bool {
		for _, s := range r.steps {
			var f ackBody
			if json.Unmarshal([]byte(s.Recv), &f) == nil && f.Type == "ack" && f.N == a.N {
				ack = f
				return true
			}
		}
		return false
	})
	r.sync()
	return ack
}

// cutNotice is what the client says of a cut whose paste moved only some
// of its cells.
var cutNotice = regexp.MustCompile(`^Your paste moved (\d+) of its (\d+) cells\. Rows or columns were deleted before the host received it, so the other (\d+) did not move\. A cut cell that did not move keeps its content, unless another edit deleted its own row or column and the cell with it\. The text:$`)

// TestOverlappingCutKeepsCellsThatStay cuts and pastes from the real
// client into the real engine over a destination that overlaps the
// source, while another editor deletes a row or a column of the
// destination before the paste arrives. A cut cell whose destination was
// deleted stays where it was, and the same cut writes nothing over it. The
// cell that would have moved there stays too, and so on (rule 32). No
// content, formula or format of the cut is lost. The ack, the client's
// notice and undo count only the cells that moved.
func TestOverlappingCutKeepsCellsThatStay(t *testing.T) {
	node := nodeOrSkip(t)
	rows := [][]string{{"=J6*2", "two"}, {"three", "=J7+1"}, {"=J6+J7", "six"}}
	cols := [][]string{{"=J6*2", "two", "=J6+J7"}, {"three", "=J7+1", "six"}}
	cases := []struct {
		name   string
		at     string
		values [][]string
		// cut and paste are positions of the client's selection.
		cut, paste []int
		// remove is Bob's action, sent after the client prepared the paste
		// and before the host received it.
		remove           func(e *env) string
		applied, ignored int
		// after maps each address of the region, as it is after Bob's
		// deletion, to the address whose content it holds after the cut, or
		// to "" for an empty cell.
		after map[string]string
	}{{
		name: "rows, down, every cell stays", at: "A1", values: [][]string{{"=J6*2"}, {"two"}, {"three"}},
		cut: []int{0, 0, 2, 0}, paste: []int{1, 0, 1, 0},
		remove:  func(e *env) string { return e.deleteRowsOp("Sheet1", 4, 1) },
		applied: 0, ignored: 3,
		after: map[string]string{"A1": "A1", "A2": "A2", "A3": "A3", "A4": ""},
	}, {
		name: "rows, down and right", at: "A1", values: rows,
		cut: []int{0, 0, 2, 1}, paste: []int{1, 1, 1, 1},
		remove:  func(e *env) string { return e.deleteRowsOp("Sheet1", 4, 1) },
		applied: 3, ignored: 3,
		after: map[string]string{
			"A1": "", "B1": "", "C1": "",
			"A2": "A2", "B2": "A1", "C2": "B1",
			"A3": "A3", "B3": "B3", "C3": "B2",
			"A4": "", "B4": "", "C4": "",
		},
	}, {
		name: "rows, up and left", at: "B2", values: rows,
		cut: []int{1, 1, 3, 2}, paste: []int{0, 0, 0, 0},
		remove:  func(e *env) string { return e.deleteRowsOp("Sheet1", 1, 1) },
		applied: 3, ignored: 3,
		after: map[string]string{
			"A1": "B2", "B1": "B1", "C1": "C1",
			"A2": "B3", "B2": "C3", "C2": "C2",
			"A3": "", "B3": "", "C3": "",
		},
	}, {
		name: "columns, right and down", at: "A1", values: cols,
		cut: []int{0, 0, 1, 2}, paste: []int{1, 1, 1, 1},
		remove:  func(e *env) string { return e.deleteColsOp("Sheet1", 4, 1) },
		applied: 3, ignored: 3,
		after: map[string]string{
			"A1": "", "B1": "B1", "C1": "C1", "D1": "",
			"A2": "", "B2": "A1", "C2": "C2", "D2": "",
			"A3": "", "B3": "A2", "C3": "B2", "D3": "",
		},
	}, {
		name: "columns, left and up", at: "B2", values: cols,
		cut: []int{1, 1, 2, 3}, paste: []int{0, 0, 0, 0},
		remove:  func(e *env) string { return e.deleteColsOp("Sheet1", 1, 1) },
		applied: 3, ignored: 3,
		after: map[string]string{
			"A1": "B2", "B1": "C2", "C1": "",
			"A2": "A2", "B2": "C3", "C2": "",
			"A3": "A3", "B3": "B3", "C3": "",
		},
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(overlapSheet(c.at, c.values))
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(e.tabID("Sheet1"))
			steps, _ := r.taken()
			cut := []any{
				map[string]any{"select": c.cut}, map[string]any{"cut": true},
				map[string]any{"select": c.paste}, map[string]any{"paste": "all"},
			}
			sent := play(t, node, steps, cut...).Frames
			if len(sent) != 1 || !strings.Contains(string(sent[0]), `"cut":true`) {
				t.Fatalf("the client sent %s, want one cut", sent)
			}

			if ack := e.person("Bob").do(c.remove(e)); status(ack) != "applied" {
				t.Fatalf("Bob's deletion was answered %s", status(ack))
			}
			// The cells as the host holds them when the paste arrives: a
			// deleted column moves the references of their formulas.
			before := map[string]cutCell{}
			for addr := range c.after {
				before[addr] = e.cutCell(addr)
			}
			filled := e.filled("Sheet1")
			r.sync()
			ack := r.answer(sent[0])
			if len(ack.Results) != 1 || ack.Results[0].Status != "applied" {
				t.Fatalf("the cut was answered %+v", ack.Results)
			}
			if res := ack.Results[0]; res.Applied != c.applied || res.Ignored != c.ignored {
				t.Errorf("the cut was answered %+v, want %d applied and %d ignored", res, c.applied, c.ignored)
			}
			changed := 0
			for addr, from := range c.after {
				want := cutCell{}
				if from != "" {
					want = before[from]
					if want.text == "" || want.format == (formula.Format{}) {
						t.Fatalf("%s holds nothing to keep before the cut: %+v", from, want)
					}
				}
				if got := e.cutCell(addr); got != want {
					t.Errorf("%s is %+v after the cut, want %+v (what %q held)", addr, got, want, from)
				}
				if want != before[addr] {
					changed++
				}
			}
			if got := e.filled("Sheet1"); got != filled {
				t.Errorf("the sheet has %d filled cells after the cut, want %d as before it", got, filled)
			}

			// The client reads the ack the host sent, after Bob's deletion.
			later, _ := r.taken()
			run := play(t, node, steps, append(cut, map[string]any{"steps": later[len(steps):]}, map[string]any{"undo": true})...)
			if len(run.Notices) == 0 {
				t.Fatal("the client showed no notice for a cut that moved only a part")
			}
			m := cutNotice.FindStringSubmatch(run.Notices[len(run.Notices)-1])
			if m == nil || m[1] != fmt.Sprint(c.applied) || m[2] != fmt.Sprint(c.applied+c.ignored) || m[3] != fmt.Sprint(c.ignored) {
				t.Errorf("the client said %q, want that %d of %d cells moved", run.Notices[len(run.Notices)-1], c.applied, c.applied+c.ignored)
			}

			// Undo from the client takes back the cells that moved and
			// nothing else: the content and every format property of each.
			if len(run.Frames) != 2 || !strings.Contains(string(run.Frames[1]), `"op":"undo"`) {
				t.Fatalf("the client sent %s, want the cut and an undo", run.Frames)
			}
			u := r.answer(run.Frames[1]).Undo
			if want := changed * (1 + propCount); u == nil || u.Applied != want || u.Skipped != 0 {
				t.Errorf("undo of the cut = %+v, want %d applied and none skipped", u, want)
			}
			for addr := range c.after {
				if got := e.cutCell(addr); got != before[addr] {
					t.Errorf("%s is %+v after undo, want %+v", addr, got, before[addr])
				}
			}
		})
	}
}

// chainCut is a cut down one column of n cells by one row, whose last
// destination was deleted: the longest chain of cells that stay. Ids are
// 1 to n+1, with 0 for the deleted row.
func chainCut(n int) (src, dst []uint64) {
	src, dst = make([]uint64, n), make([]uint64, n)
	for i := range n {
		src[i] = uint64(i + 1)
		if i < n-1 {
			dst[i] = uint64(i + 2)
		}
	}
	return src, dst
}

// TestCutPlanFollowsTheLongestChain plans the longest chain a cut can
// have: every cell stays, each destination is looked at a bounded number
// of times, and the planning stops at the deadline. The least work is
// worked out from the size of the cut.
func TestCutPlanFollowsTheLongestChain(t *testing.T) {
	const n = maxCells
	src, dst := chainCut(n)
	col := []uint64{1}
	x := &txn{budget: formula.NewBudget(nil)}
	p, err := x.planCut(true, src, col, dst, col, func(int) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	for k := range n {
		if p.clears(k) {
			t.Fatalf("source %d of the chain moves", k)
		}
		if d := k; d < n-1 && !p.held(d) {
			t.Fatalf("destination %d of the chain applies over a cell that stays", d)
		}
	}
	// An id, a source, a destination and a hold for each cell of the
	// chain, a unit each, and a look at each cell's count, a step.
	x.settle()
	if least := int64(4*n - 1); x.budget.Used() < least {
		t.Errorf("the plan counted %d units, for at least %d", x.budget.Used(), least)
	}
	if most := int64(4*n + n/64 + 2); x.budget.Used() > most {
		t.Errorf("the plan counted %d units, for at most %d: a destination was looked at again", x.budget.Used(), most)
	}

	// The deadline passes as the planning starts, and ends it.
	var b *formula.Budget
	b = formula.NewBudget(func() bool { return b.Used() > 0 })
	x = &txn{budget: b}
	if _, err := x.planCut(true, src, col, dst, col, func(int) bool { return true }); err == nil || err.Limit != "recalculation_time" {
		t.Errorf("a plan past its deadline returned %v", err)
	}
}

// TestCutPlanNamesACellOnce plans cuts whose source lists name a row
// twice. The cell moves when a destination of either place applies, and
// stays only when none does.
func TestCutPlanNamesACellOnce(t *testing.T) {
	col := []uint64{1}
	all := func(int) bool { return true }
	x := &txn{budget: formula.NewBudget(nil)}
	// Row 1 twice, into row 2 and a deleted row.
	p, err := x.planCut(true, []uint64{1, 1}, col, []uint64{2, 0}, col, all)
	if err != nil {
		t.Fatal(err)
	}
	if !p.clears(0) || !p.clears(1) || p.held(0) {
		t.Errorf("row 1 named twice with one destination left: clears %t %t, held %t", p.clears(0), p.clears(1), p.held(0))
	}
	// Rows 1 and 2 into row 2, twice, and a deleted row: row 2 stays, so
	// row 1 cannot move into it.
	p, err = x.planCut(true, []uint64{1, 2}, col, []uint64{2, 0}, col, all)
	if err != nil {
		t.Fatal(err)
	}
	if p.clears(0) || p.clears(1) || !p.held(0) {
		t.Errorf("a chain over a cell that stays: clears %t %t, held %t", p.clears(0), p.clears(1), p.held(0))
	}
	// Into another tab nothing is under a destination.
	p, err = x.planCut(false, []uint64{1, 2}, col, []uint64{2, 0}, col, all)
	if err != nil {
		t.Fatal(err)
	}
	if !p.clears(0) || p.clears(1) || p.held(0) {
		t.Errorf("a cut into another tab: clears %t %t, held %t", p.clears(0), p.clears(1), p.held(0))
	}
}
