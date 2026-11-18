package sheet

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// The work an action does itself, outside its recalculations, is counted
// on the action's budget and ends at its deadline (technical design,
// section 10.4): comparing the keys of a sort, the text of the records it
// writes, and the formulas a structural edit, a sort, a copy or a fill goes
// through. The least work of each case is worked out here from the sizes
// of its input. Nothing of it is taken from the writer.

// workSize returns the rows and the code points of the long texts of a
// case. The race detector slows every byte down, and gets shorter texts:
// still many checks of the deadline.
func workSize() (rows, length int) {
	if raceEnabled {
		return 1000, 16384
	}
	return 1000, formula.MaxTextLen
}

// longKeys fills column A of Sheet1 with n texts of length code points
// that are the same without regard to capitals up to their last character,
// as the values of formulas, and column B with the numbers 1 to n. The
// rows with an even number hold the text that sorts first. upper and
// lower are the two letters the texts are made of.
func longKeys(t *testing.T, e *env, n, length int, upper, lower string) {
	t.Helper()
	first := strings.Repeat(upper, length-1) + "b"
	second := strings.Repeat(lower, length-1) + "a"
	var rows strings.Builder
	for i := 1; i <= n; i++ {
		if i > 1 {
			rows.WriteByte(',')
		}
		fmt.Fprintf(&rows, `["=Src!$A$%d",%d]`, 2-i%2, i)
	}
	e.mustAgent(jsonf(`{"ops":[{"op":"add_tab","name":"Src"},
		{"op":"write","tab":"Src","at":"A1","rows":[[%q],[%q]]},
		{"op":"write","tab":"Sheet1","at":"A1","rows":[%s]}]}`, first, second, rows.String()))
	if got := e.shown("Sheet1!B" + strconv.Itoa(n)); got != strconv.Itoa(n) {
		t.Fatalf("the fixture's last row shows %q", got)
	}
}

// workDeadline puts the deadline of the instance's actions on the work
// their budgets count instead of on the clock (Instance.deadline): the
// deadline of an action has passed once its budget has counted at units.
// It therefore passes at the same step of the work however busy the
// machine is, and the tests read how far the work went past it in units.
type workDeadline struct {
	mu   sync.Mutex
	at   int64
	x    *txn // the newest action
	work budgetWork
}

// budgetWork is what the budget of an action counted: all its units, the
// units it had counted at each look at its deadline, and those of the look
// that found the deadline passed, or -1.
type budgetWork struct {
	used, found int64
	looks       []int64
}

// never is a deadline that no action reaches.
const never = math.MaxInt64

func (e *env) workDeadline() *workDeadline {
	d := &workDeadline{at: never}
	e.in.mu.Lock()
	e.in.deadline = func(x *txn) func() bool {
		d.mu.Lock()
		d.x, d.work = x, budgetWork{found: -1}
		d.mu.Unlock()
		return func() bool {
			used := x.budget.Used()
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.x == x {
				d.work.looks = append(d.work.looks, used)
				if used >= d.at && d.work.found < 0 {
					d.work.found = used
				}
			}
			return used >= d.at
		}
	}
	e.in.mu.Unlock()
	return d
}

// run runs action with the deadline at units and returns its status and
// the work of the newest action it began. The actions after it have no
// deadline.
func (d *workDeadline) run(at int64, action func() string) (string, budgetWork) {
	d.mu.Lock()
	d.at, d.x, d.work = at, nil, budgetWork{found: -1}
	d.mu.Unlock()
	got := action()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.at = never
	w := d.work
	if d.x != nil {
		w.used = d.x.budget.Used()
	}
	w.looks = slices.Clone(w.looks)
	return got, w
}

// longest returns the most units the action counted between two looks at
// its deadline, with its start and its end as looks.
func (w budgetWork) longest() int64 {
	var most, last int64
	for _, used := range w.looks {
		most, last = max(most, used-last), used
	}
	return max(most, w.used-last)
}

// refusedAt runs action with its deadline at units and checks that it is
// refused a step of at most step units after it: the look that found the
// deadline passed comes at most step units after at, and the action counts
// at most step units after that look. It returns the work of the action.
func (d *workDeadline) refusedAt(t *testing.T, at, step int64, action func() string) budgetWork {
	t.Helper()
	got, w := d.run(at, action)
	if got != "refused:recalculation_time" {
		t.Fatalf("the action with its deadline at %d units: %s after %d units", at, got, w.used)
	}
	t.Logf("with its deadline at %d units the action found it passed at %d and was refused after %d", at, w.found, w.used)
	if w.found < at || w.found-at > step || w.used-w.found > step {
		t.Errorf("the action with its deadline at %d units found it passed at %d and went on to %d: more than %d units late", at, w.found, w.used, step)
	}
	return w
}

// stopsAtDeadline checks the deadline of an action whose work inside its
// limit was whole: that action looked at its deadline at least every eighth
// of its units, and a like action whose deadline passes at a quarter of
// them is refused within an eighth of them after it. The budget looks
// every 4,096 units, so whole must be many times that for an eighth of it
// to tell a stop from the end.
func (d *workDeadline) stopsAtDeadline(t *testing.T, whole budgetWork, again func() string) budgetWork {
	t.Helper()
	if whole.used < 16*budgetCheck {
		t.Fatalf("the action counted %d units, too few to tell a stop at its deadline from its end", whole.used)
	}
	gap := whole.longest()
	t.Logf("the whole action counted %d units, at most %d between two looks at its deadline", whole.used, gap)
	if gap > whole.used/8 {
		t.Errorf("the action counted %d of its %d units between two looks at its deadline", gap, whole.used)
	}
	return d.refusedAt(t, whole.used/4, whole.used/8, again)
}

// countWork returns the steps of work fn does (workSteps). The counts are
// the package's, so nothing else may work meanwhile.
func countWork(fn func()) *workSteps {
	w := &workSteps{}
	counting.Store(w)
	defer counting.Store(nil)
	fn()
	return w
}

// evenThenOdd is column B after an ascending, stable sort of longKeys.
func evenThenOdd(n int) string {
	var want []string
	for i := 2; i <= n; i += 2 {
		want = append(want, strconv.Itoa(i))
	}
	for i := 1; i <= n; i += 2 {
		want = append(want, strconv.Itoa(i))
	}
	return fmt.Sprint(want)
}

// memory is the state of a sheet that lives in memory only and that an
// action which is refused must leave alone: the last sort of each row, the
// structural log and the undo stacks of a session.
type memory struct {
	sorts      map[RowID]sortStamp
	log        int
	undo, redo int
}

func (e *env) memory(tab string, p *person) memory {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	m := memory{sorts: maps.Clone(e.in.wb.tab(tab).lastSort), log: len(e.in.structs.entries)}
	if p != nil {
		m.undo, m.redo = len(p.s.undo.undo), len(p.s.undo.redo)
	}
	return m
}

func (m memory) equal(o memory) bool {
	return maps.Equal(m.sorts, o.sorts) && m.log == o.log && m.undo == o.undo && m.redo == o.redo
}

// A sort compares the calculated values of its key column. Each comparison
// of two long texts goes through what they have in common, so a sort of
// rows that differ in their last character does as much work as a large
// recalculation. It is counted, and the action is refused at its deadline
// with nothing reordered.
func TestSortCountsItsWorkAndMeetsTheDeadline(t *testing.T) {
	n, length := workSize()
	// Any way of ordering n rows compares at least n-1 pairs, and a pair of
	// these texts has length-1 bytes in common: 64 bytes to a unit.
	least := int64(n-1) * int64(length-1) / 64
	if least < 4*4096 {
		t.Fatalf("the case is too little work to meet a deadline: %d units", least)
	}
	type sorter struct {
		sort    func(e *env, p *person, desc bool) string // the status of the sort
		undoing bool
	}
	ways := map[string]sorter{
		"a person's sort": {undoing: true, sort: func(e *env, p *person, desc bool) string {
			return status(p.do(e.sortOp("Sheet1", 1, n, 1, desc)))
		}},
		"an agent's sort": {sort: func(e *env, _ *person, desc bool) string {
			_, err := e.agent(jsonf(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:%d","by":"A","desc":%v}]}`, n, desc))
			if err != nil {
				werr := refusal(err)
				return "refused:" + werr.reason()
			}
			return "applied"
		}},
	}
	for name, way := range ways {
		t.Run(name, func(t *testing.T) {
			e := start(t)
			longKeys(t, e, n, length, "A", "a")
			alice := e.person("Alice")
			deadline := e.workDeadline()

			got, whole := deadline.run(never, func() string { return way.sort(e, alice, false) })
			if got != "applied" {
				t.Fatalf("the sort: %s", got)
			}
			if got := fmt.Sprint(e.column("Sheet1", "B", 1, n)); got != evenThenOdd(n) {
				t.Fatalf("the sort is not the stable order of its keys")
			}
			if work := e.in.lastWork.Load(); work < least {
				t.Errorf("the sort of %d rows of %d characters counted %d units of work, for at least %d", n, length, work, least)
			}

			// The same sort the other way, with a deadline that passes
			// while it compares. The looks at the deadline lie 4,096 units
			// apart: a few comparisons out of thousands.
			before, kept := e.dump(), e.memory("Sheet1", alice)
			deadline.stopsAtDeadline(t, whole, func() string { return way.sort(e, alice, true) })
			if after := e.dump(); after != before {
				t.Fatalf("the refused sort changed the sheet: %s", dumpDiff(before, after))
			}
			if !kept.equal(e.memory("Sheet1", alice)) {
				t.Errorf("the refused sort changed the sort stamps, the structural log or the undo stacks")
			}
			if !way.undoing {
				return
			}
			// The first sort is still the newest entry of the undo stack.
			wantUndo(t, "undo of the first sort", undo(t, alice), undoAck{Applied: 1})
			if got := e.shown("Sheet1!B1") + " " + e.shown("Sheet1!B2"); got != "1 2" {
				t.Errorf("after the undo the first rows are %s", got)
			}
		})
	}
}

// Letters outside ASCII take far longer to compare without regard to
// capitals, and each of their bytes is a step. The deadline ends a sort of
// such keys while it compares them: the action is refused a step after the
// deadline, long before the sort could have ended.
func TestSortOfSlowKeysStopsAtTheDeadline(t *testing.T) {
	n, length := workSize()
	// A pair of these keys has length-1 code points of two bytes in common.
	least := int64(n-1) * int64(length-1) * 2 / 64
	at, step := int64(4*budgetCheck), int64(2*budgetCheck)
	if least < 8*(at+2*step) {
		t.Fatalf("the whole sort counts at least %d units, too little to tell a stop from its end", least)
	}
	e := start(t)
	longKeys(t, e, n, length, "Σ", "σ")
	alice := e.person("Alice")
	deadline := e.workDeadline()
	before, kept := e.dump(), e.memory("Sheet1", alice)
	deadline.refusedAt(t, at, step, func() string { return status(alice.do(e.sortOp("Sheet1", 1, n, 1, false))) })
	if after := e.dump(); after != before {
		t.Fatalf("the refused sort changed the sheet: %s", dumpDiff(before, after))
	}
	if !kept.equal(e.memory("Sheet1", alice)) {
		t.Errorf("the refused sort changed the sort stamps, the structural log or the undo stacks")
	}
}

// fillOp fills the cell at the 1-based row and column over count rows from
// the 1-based row to of the 1-based column col.
func (e *env) fillOp(row, column, to, count, col int) string {
	e.t.Helper()
	return jsonf(`{"op":"fill","tab":%d,"source":{"rows":%s,"cols":%s},"target":{"rows":%s,"cols":%s}}`, e.tabID("Sheet1"),
		e.rowsOf("Sheet1", row, 1), e.colsOf("Sheet1", column, 1), e.rowsOf("Sheet1", to, count), e.colsOf("Sheet1", col, 1))
}

func (e *env) setRecalcLimit(d time.Duration) time.Duration {
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	was := e.in.opts.RecalcLimit
	e.in.opts.RecalcLimit = d
	return was
}

// one is the status and reason all operations of an action have, whatever
// each applied, or the statuses of all of them when they differ.
func one(ack ackBody) string {
	all := strings.Split(status(ack), ",")
	for _, s := range all {
		if s != all[0] {
			return status(ack)
		}
	}
	return all[0]
}

func agentStatus(e *env, call string) string {
	if _, err := e.agent(call); err != nil {
		return "refused:" + refusal(err).reason()
	}
	return "applied"
}

// The operations below multiply text that the sheet holds: by the cells of
// a target, by the operations of one action, or by the structural edits a
// formula is brought through. Each counts what it goes through, and each
// is refused at the deadline with nothing changed.
//
// Every case runs its action once without a deadline, which must count at
// least the work worked out from its sizes, and then a like action with a
// deadline that passes while it works, at a quarter of the units the first
// counted, which must be refused within an eighth of them after it. The
// deadlines are on the units counted (workDeadline).
func TestActionsCountTheirOwnWorkAndMeetTheDeadline(t *testing.T) {
	n, length := workSize()
	// Texts of 20,000 code points, so that two columns of them fit the
	// typed text of a sheet.
	length = min(length, 20000)
	text := strings.Repeat("x", length)
	// A formula of the same length that reads no cell and whose
	// calculation goes through none of its text.
	long := `=IF(TRUE,1,"` + strings.Repeat("y", min(length, formula.MaxFormulaChars-100)) + `")`
	const edits = 200 // the structural edits of the cases that repeat one
	type work struct {
		build func(e *env, alice, bob *person)
		do    func(e *env, alice *person) string // the status of the action
		again func(e *env, alice *person) string // a like action, for the deadline
		least int64
		// most bounds the units of the action inside the limit, for a case
		// whose work must stay in proportion to its size.
		most int64
	}
	formats := func(e *env, _ *person) string {
		op := jsonf(`{"op":"format","tab":"Sheet1","ranges":["A1:A%d"],"format":{"bold":true}}`, n)
		return agentStatus(e, `{"ops":[`+strings.TrimSuffix(strings.Repeat(op+",", 20), ",")+`]}`)
	}
	inserts := func(e *env, _ *person) string {
		op := `{"op":"insert_rows","tab":"Sheet1","at":1,"count":1}`
		return agentStatus(e, `{"ops":[`+strings.TrimSuffix(strings.Repeat(op+",", edits), ",")+`]}`)
	}
	const longTab = 50000 // rows
	churn := func(e *env, _ *person) string {
		pair := `{"op":"insert_rows","tab":"Sheet1","at":1,"count":1},{"op":"delete_rows","tab":"Sheet1","at":1,"count":1}`
		return agentStatus(e, `{"ops":[`+strings.TrimSuffix(strings.Repeat(pair+",", edits/2), ",")+`]}`)
	}
	var stale uint64 // the sequence of the screen a late formula was typed on
	late := func(col int) func(e *env, alice *person) string {
		return func(e *env, alice *person) string {
			// The rows that were the first n on that screen, by their ids.
			rows := e.rowsOf("Sheet1", edits+1, n)
			values := strings.TrimSuffix(strings.Repeat(`["=$A$900+1"],`, n), ",")
			return status(alice.act(stale, jsonf(`{"op":"write","tab":%d,"rows":%s,"cols":%s,"values":[%s]}`,
				e.tabID("Sheet1"), rows, e.colsOf("Sheet1", col, 1), values)))
		}
	}
	// The sizes of the cases that go over rows and records without writing
	// them: the rows of a tall tab, the rows that lie apart, the formulas
	// of a sheet and the rows of a target that is as large as one may be.
	// The formulas count about a unit each, so under the race detector too
	// there are enough of them to meet a deadline.
	tall, apart, many, wide := raced(50000, 5000), raced(2000, 500), raced(50000, 20000), raced(19000, 2000)
	// An action counts its work where it ends with a change, so each of
	// these ends with a small edit.
	clears := func(e *env, alice *person) string {
		ops := make([]string, 100, 101)
		for i := range ops {
			ops[i] = jsonf(`{"op":"clear","tab":%d,"rows":"all","cols":"all"}`, e.tabID("Sheet1"))
		}
		return one(alice.do(append(ops, e.setOp("A1", "x"))...))
	}
	everyOther := func(e *env, alice *person) string {
		e.in.mu.RLock()
		ids := make([]string, apart)
		for i := range ids {
			ids[i] = strconv.FormatUint(e.in.wb.tab("Sheet1").rows.ids[2*i], 10)
		}
		e.in.mu.RUnlock()
		return status(alice.do(jsonf(`{"op":"delete_rows","tab":%d,"ids":[%s]}`, e.tabID("Sheet1"), strings.Join(ids, ","))))
	}
	columns := func(e *env, _ *person) string {
		pair := `{"op":"insert_cols","tab":"Sheet1","at":"A","count":1},{"op":"delete_cols","tab":"Sheet1","at":"A","count":1}`
		return agentStatus(e, `{"ops":[`+strings.TrimSuffix(strings.Repeat(pair+",", edits), ",")+`]}`)
	}
	blanks := func(e *env, alice *person) string {
		tab := e.tabID("Sheet1")
		blank := jsonf(`"rows":%s,"cols":%s`, e.rowsOf("Sheet1", 1, 1), e.colsOf("Sheet1", newTabCols, 1))
		target := jsonf(`"rows":"all","cols":%s`, e.colsOf("Sheet1", 1, newTabCols-1))
		op := jsonf(`{"op":"copy","from":{"tab":%d,%s},"to":{"tab":%d,%s},"mode":"all","cut":false}`, tab, blank, tab, target)
		return one(alice.do(op, op, op, e.setOp("Z2", "x")))
	}
	cases := map[string]work{
		// Each new record is weighed: the code points of its text are
		// counted for the limit of typed text.
		"a fill of a long text": {
			build: func(e *env, alice, _ *person) { alice.set("A1", text) },
			do:    func(e *env, alice *person) string { return status(alice.do(e.fillOp(1, 1, 2, n-1, 1))) },
			again: func(e *env, alice *person) string { return status(alice.do(e.fillOp(1, 1, 1, n, 2))) },
			least: int64(n-1) * int64(length) / 64,
		},
		// Each copy of a formula is measured at its new cell and looked
		// up among the formulas of the sheet.
		// The copies share one stored formula, so a cell takes little
		// time. The fill has twenty times the rows of the other cases,
		// of a formula that two such columns fit the typed text with.
		"a fill of a long formula": {
			build: func(e *env, alice, _ *person) {
				tallTab(e, 20*n)
				alice.set("A1", long[:1000]+`")`)
			},
			do:    func(e *env, alice *person) string { return status(alice.do(e.fillOp(1, 1, 2, 20*n-1, 1))) },
			again: func(e *env, alice *person) string { return status(alice.do(e.fillOp(1, 1, 1, 20*n, 2))) },
			least: int64(20*n-1) * 2 * 1000 / 64,
		},
		// A format weighs the record before and after: twenty formats
		// over the same cells go through their text forty times.
		"formats over long texts in one call": {
			build: func(e *env, alice, _ *person) {
				alice.set("A1", text)
				alice.do(e.fillOp(1, 1, 2, n-1, 1))
			},
			do: formats, again: formats,
			least: 20 * int64(n) * 2 * int64(length) / 64,
		},
		// Each insert above a formula with a fixed reference writes the
		// formula again.
		"inserts above many formulas in one call": {
			build: func(e *env, _, _ *person) {
				e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[%s]}]}`,
					strings.TrimSuffix(strings.Repeat(`["=$A$900+1"],`, n), ",")))
			},
			do: inserts, again: inserts,
			least: edits * int64(n),
		},
		// Each edit of the rows goes through the row ids to place its
		// own: eight bytes a row.
		"inserts and deletes in a long tab in one call": {
			build: func(e *env, _, _ *person) {
				e.mustAgent(jsonf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, longTab-newTabRows))
			},
			do: churn, again: churn,
			least: edits * longTab * 8 / 64,
		},
		// A formula typed on an older screen is brought through every
		// structural edit since.
		"formulas typed before many inserts": {
			build: func(e *env, _, bob *person) {
				stale = e.seq()
				for i := 0; i < edits; i++ {
					if ack := bob.do(e.insertRowsOp("Sheet1", 1, 1)); status(ack) != "applied" {
						t.Fatal(status(ack))
					}
				}
			},
			do: late(2), again: late(3),
			least: edits * int64(n),
		},
		// "all" is a few bytes that name every row of a tab. Each row is
		// looked up twice: for its id, and for the records to clear.
		"clears of every row of an empty tab in one action": {
			build: func(e *env, _, _ *person) { tallTab(e, tall) },
			do:    clears, again: clears,
			least: 100 * 2 * int64(tall),
		},
		// The rows that go lie apart, so each is an edit of its own that
		// goes through the ids of the rows that are left.
		"a deletion of every other row": {
			build: func(e *env, _, _ *person) { tallTab(e, 3*apart) },
			do:    everyOther, again: everyOther,
			least: int64(apart) * int64(apart) * 8 / 64,
		},
		// One insert writes every formula of the sheet again, and they
		// all read one cell. The insert counts a few units for each
		// formula, and not a unit for each of the others that read the
		// cell as well: that is tens of millions for these.
		"an insert above every formula": {
			build: func(e *env, _, _ *person) {
				tallTab(e, many+1000)
				e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[%s]}]}`,
					strings.TrimSuffix(strings.Repeat(`["=$A$900+1"],`, many), ",")))
			},
			do: func(e *env, _ *person) string {
				return agentStatus(e, `{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":1}]}`)
			},
			again: func(e *env, _ *person) string {
				return agentStatus(e, `{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":1}]}`)
			},
			least: int64(many),
			most:  20 * int64(many),
		},
		// A deleted column takes its cells with it: every record of the
		// tab is looked at for its column.
		"deletes of a column beside many records in one call": {
			build: func(e *env, alice, _ *person) {
				alice.set("A1", "1")
				ack := alice.do(jsonf(`{"op":"fill","tab":%d,"source":{"rows":%s,"cols":%s},"target":{"rows":%s,"cols":"all"}}`, e.tabID("Sheet1"),
					e.rowsOf("Sheet1", 1, 1), e.colsOf("Sheet1", 1, 1), e.rowsOf("Sheet1", 1, n)))
				if status(ack) != "applied" {
					t.Fatal(status(ack))
				}
			},
			do: columns, again: columns,
			least: edits * int64(n) * newTabCols,
		},
		// Every cell of a target is a step, also where a blank source
		// leaves it as it is.
		"copies of a blank cell over a large target": {
			build: func(e *env, _, _ *person) { tallTab(e, wide) },
			do:    blanks, again: blanks,
			least: 3 * int64(wide) * (newTabCols - 1),
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if c.least < 4*4096 {
				t.Fatalf("the case is too little work to meet a deadline: %d units", c.least)
			}
			e := start(t)
			alice, bob := e.person("Alice"), e.person("Bob")
			c.build(e, alice, bob)
			deadline := e.workDeadline()
			got, whole := deadline.run(never, func() string { return c.do(e, alice) })
			if got != "applied" {
				t.Fatalf("the action: %s", got)
			}
			if work := e.in.lastWork.Load(); work < c.least {
				t.Errorf("the action counted %d units of work, for at least %d", work, c.least)
			}
			if c.most > 0 && whole.used > c.most {
				t.Errorf("the action counted %d units of work, over %d", whole.used, c.most)
			}

			before, kept := e.dump(), e.memory("Sheet1", alice)
			deadline.stopsAtDeadline(t, whole, func() string { return c.again(e, alice) })
			if after := e.dump(); after != before {
				t.Fatalf("the refused action changed the sheet: %s", dumpDiff(before, after))
			}
			if !kept.equal(e.memory("Sheet1", alice)) {
				t.Errorf("the refused action changed the sort stamps, the structural log or the undo stacks")
			}
			// Without the deadline again, the sheet takes the action.
			if got, _ := deadline.run(never, func() string { return c.again(e, alice) }); got != "applied" {
				t.Errorf("the action with the limit back: %s", got)
			}
		})
	}
}

// A fill writes its source into every cell of its target before the limit
// of typed text is looked at. The deadline ends it while it runs, a step
// after it passed: here a text of the greatest length into every cell of a
// tab, far more text than a sheet may hold. Each code point of each copy is
// counted.
func TestFillStopsAtTheDeadline(t *testing.T) {
	least := int64(newTabRows*newTabCols-1) * formula.MaxTextLen / 64
	at, step := int64(4*budgetCheck), int64(2*budgetCheck)
	if least < 8*(at+2*step) {
		t.Fatalf("the whole fill counts at least %d units, too little to tell a stop from its end", least)
	}
	e := start(t)
	alice := e.person("Alice")
	alice.set("A1", strings.Repeat("가", formula.MaxTextLen))
	deadline := e.workDeadline()
	before, kept := e.dump(), e.memory("Sheet1", alice)
	op := jsonf(`{"op":"fill","tab":%d,"source":{"rows":%s,"cols":%s},"target":{"rows":"all","cols":"all"}}`,
		e.tabID("Sheet1"), e.rowsOf("Sheet1", 1, 1), e.colsOf("Sheet1", 1, 1))
	deadline.refusedAt(t, at, step, func() string { return status(alice.do(op)) })
	if after := e.dump(); after != before {
		t.Fatalf("the refused fill changed the sheet: %s", dumpDiff(before, after))
	}
	if !kept.equal(e.memory("Sheet1", alice)) {
		t.Errorf("the refused fill changed the structural log or the undo stacks")
	}
}

// The deadline is on the clock, not on the work that was counted. A check
// between two operations reads it, so an action that comes to an operation
// after its deadline is refused there, however little the operation and
// everything before it counted.
func TestDeadlineIsReadWithoutCountedWork(t *testing.T) {
	e := start(t)
	alice := e.person("Alice")
	if ack := alice.set("A1", "1"); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	t.Run("a checkpoint", func(t *testing.T) {
		const limit = 50 * time.Millisecond
		was := e.setRecalcLimit(limit)
		defer e.setRecalcLimit(was)
		e.in.mu.Lock()
		defer e.in.mu.Unlock()
		x := newTxn(e.in.wb, e.in.wb.seq+1, e.in.wb.seq, 0)
		began := time.Now()
		e.in.begin(x)
		if err := x.stop(); err != nil && time.Since(began) < limit {
			t.Fatalf("the action was stopped %v after its start: %v", time.Since(began), err)
		}
		time.Sleep(limit + 10*time.Millisecond)
		if x.budget.Used() != 0 || x.budget.Exceeded() {
			t.Fatalf("the budget counted %d units and met the deadline by itself", x.budget.Used())
		}
		if err := x.stop(); err == nil || err.Limit != "recalculation_time" {
			t.Errorf("a checkpoint %v after the start, with a limit of %v: %v", time.Since(began), limit, err)
		}
		if !x.late() {
			t.Errorf("the action is not late after its checkpoint found the deadline passed")
		}
	})
	t.Run("a small action", func(t *testing.T) {
		deadline := e.workDeadline()
		before, kept := e.dump(), e.memory("Sheet1", alice)
		deadline.refusedAt(t, 0, 0, func() string { return status(alice.set("A1", "2")) })
		deadline.refusedAt(t, 0, 0, func() string {
			return agentStatus(e, `{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[3]]}]}`)
		})
		if after := e.dump(); after != before {
			t.Fatalf("the refused actions changed the sheet: %s", dumpDiff(before, after))
		}
		if !kept.equal(e.memory("Sheet1", alice)) {
			t.Errorf("the refused actions changed the structural log or the undo stacks")
		}
		if got := status(alice.set("A1", "2")); got != "applied" {
			t.Errorf("the next edit: %s", got)
		}
	})
}
