package sheet

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/sheet/formula"
)

var seedCount = flag.Int("sheet.seeds", 6, "number of random histories the state-machine test runs")

// smOp is one step of a history. It holds numbers and text only, and every
// number is read modulo what the sheet holds when the step runs, so a
// history stays valid when steps are taken out of it.
type smOp struct {
	Kind    string
	Who     int    // the session of a person's action
	Tab     int    // tab, by position
	R, C    int    // a cell of the edited area, or the first of a range
	N, M    int    // sizes, positions and choices
	Text    string // typed text, or a name
	Hold    bool   // a person's action that stays unsent until a flush
	Flag    bool
	State   int // an agent call: 0 for the current state, else a saved one
	Guard   int // an agent call: 0 none, 1 unchanged, 2 a range
	Formats uint16
}

func (op smOp) String() string {
	out := op.Kind
	add := func(name string, v any) { out += fmt.Sprintf(" %s=%v", name, v) }
	if op.Who != 0 {
		add("who", op.Who)
	}
	add("tab", op.Tab)
	add("r", op.R)
	add("c", op.C)
	add("n", op.N)
	add("m", op.M)
	if op.Text != "" {
		add("text", strconv.Quote(op.Text))
	}
	for name, on := range map[string]bool{"hold": op.Hold, "flag": op.Flag} {
		if on {
			out += " " + name
		}
	}
	if op.State != 0 {
		add("state", op.State)
	}
	if op.Guard != 0 {
		add("guard", op.Guard)
	}
	if op.Formats != 0 {
		add("formats", op.Formats)
	}
	return out
}

const (
	smAreaRows, smAreaCols = 10, 6
	smPeople               = 3
	smMaxTabs              = 3
)

var smFormulas = []string{
	"=%[1]s+%[2]s", "=SUM(%[1]s:%[2]s)", "=$%[3]s$%[4]d*2+%[1]s", "=%[5]s!%[1]s-%[2]s", "=IF(%[1]s>2,%[2]s,0)", "=COUNT(%[1]s:%[2]s)&\"x\"",
	"=MAX(%[5]s!%[1]s:%[2]s)", "=%[1]s", "=SUM(%[3]s:%[3]s)", "=SUM(%[4]d:%[4]d)", "=Gone!%[1]s+1", "=AVERAGE(%[1]s:%[2]s",
}

var smTexts = []string{"", "1", "2.5", "word", "-3", "TRUE", "7", "Zed", "2026-11-13", "'=text", "0"}

// smPersonKinds and smAgentKinds are the steps of people and of the agent,
// with how often the generator picks each.
var smPersonKinds = []struct {
	kind   string
	weight int
}{
	{"set", 22}, {"formula", 16}, {"clear", 5}, {"format", 10}, {"width", 3}, {"insert", 7}, {"append", 2}, {"delete", 6}, {"sort", 6},
	{"settings", 2}, {"add_tab", 2}, {"rename_tab", 2}, {"move_tab", 2}, {"delete_tab", 1}, {"undo", 18}, {"redo", 7}, {"recover", 3},
}

var smAgentKinds = []struct {
	kind   string
	weight int
}{
	{"write", 8}, {"fill", 2}, {"clear", 2}, {"format", 3}, {"widths", 1}, {"insert", 3}, {"delete", 3}, {"sort", 2}, {"add_tab", 1},
	{"rename_tab", 1}, {"move_tab", 1}, {"delete_tab", 1}, {"tab_settings", 1}, {"several", 5},
}

func genOp(rng *rand.Rand) smOp {
	op := smOp{Tab: rng.Intn(8), R: rng.Intn(smAreaRows), C: rng.Intn(smAreaCols), N: rng.Intn(1000), M: rng.Intn(1000)}
	pick := func(kinds []struct {
		kind   string
		weight int
	}) string {
		total := 0
		for _, k := range kinds {
			total += k.weight
		}
		n := rng.Intn(total)
		for _, k := range kinds {
			if n -= k.weight; n < 0 {
				return k.kind
			}
		}
		return ""
	}
	switch n := rng.Intn(100); {
	case n < 62:
		op.Kind, op.Who, op.Hold = pick(smPersonKinds), 1+rng.Intn(smPeople), rng.Intn(2) == 0
		op.Flag = rng.Intn(2) == 0
		op.Text = smTexts[rng.Intn(len(smTexts))]
		op.Formats = uint16(1 + rng.Intn(1<<propCount-1))
	case n < 82:
		op.Kind = "agent " + pick(smAgentKinds)
		op.Flag = rng.Intn(2) == 0
		op.Text = smTexts[rng.Intn(len(smTexts))]
		op.Formats = uint16(1 + rng.Intn(1<<propCount-1))
		if rng.Intn(3) == 0 {
			op.State = 1 + rng.Intn(4)
		}
		if rng.Intn(4) == 0 {
			op.Guard = 1 + rng.Intn(2)
		}
	case n < 89:
		op.Kind, op.Flag = "flush", rng.Intn(4) != 0
	case n < 92:
		op.Kind = "read" // the agent reads and keeps the state
	case n < 94:
		op.Kind = "tick"
	case n < 95:
		op.Kind = "rollback"
	case n < 96:
		op.Kind = "restart"
	case n < 97:
		op.Kind, op.Who = "resend", 1+rng.Intn(smPeople)
	case n < 98:
		// An action whose commit fails, and the same action again.
		op.Kind, op.Who = "commit fails", 1+rng.Intn(smPeople)
		op.Text, op.Flag = smTexts[rng.Intn(len(smTexts))], rng.Intn(2) == 0
		op.Formats = uint16(1 + rng.Intn(1<<propCount-1))
	case n < 99:
		op.Kind = []string{"quota", "collect"}[rng.Intn(2)] // edits at a full disk quota, or a tick that collects blank records
	default:
		op.Kind = "limit" // an edit that a limit of the sheet refuses
	}
	return op
}

// smGen makes the steps of a history. Most are drawn one by one. One draw
// in four starts a short script instead, which lines up what a history of
// independent steps seldom does: an edit or a deletion of one person, then
// structural edits of others, some of them taken back, then that person's
// undo. A script is steps like any other, so a failing history is reduced
// the same way.
type smGen struct {
	rng   *rand.Rand
	queue []smOp
}

func (g *smGen) next() smOp {
	if len(g.queue) == 0 && g.rng.Intn(4) == 0 {
		g.queue = g.script()
	}
	if len(g.queue) > 0 {
		op := g.queue[0]
		g.queue = g.queue[1:]
		return op
	}
	return genOp(g.rng)
}

func (g *smGen) script() []smOp {
	rng := g.rng
	a := 1 + rng.Intn(smPeople)
	b := 1 + (a+rng.Intn(smPeople-1))%smPeople
	tab, row, col := rng.Intn(8), rng.Intn(smAreaRows), rng.Intn(smAreaCols)
	op := func(kind string, who int) smOp {
		o := genOp(rng)
		o.Kind, o.Who, o.Tab, o.Hold, o.State, o.Guard = kind, who, tab, false, 0, 0
		return o
	}
	at := func(o smOp) smOp { o.R, o.C = row, col; return o }
	// What others do to the structure in between: inserts and deletions
	// near the cell, a sort, a rename, and an undo that takes one back.
	between := func() []smOp {
		var out []smOp
		for n := 1 + rng.Intn(3); n > 0; n-- {
			switch k := rng.Intn(10); {
			case k < 3:
				o := op("insert", b)
				o.R, o.C = rng.Intn(smAreaRows), rng.Intn(smAreaCols)
				out = append(out, o)
			case k < 6:
				o := op("delete", b)
				o.R, o.C = rng.Intn(smAreaRows), rng.Intn(smAreaCols)
				out = append(out, o)
				if rng.Intn(2) == 0 {
					out = append(out, op("undo", b))
				}
			case k < 7:
				out = append(out, op("sort", b))
			case k < 8:
				out = append(out, op("rename_tab", b))
			case k < 9:
				out = append(out, op([]string{"agent insert", "agent delete", "agent sort"}[rng.Intn(3)], 0))
			default:
				out = append(out, at(op("set", b)))
			}
		}
		return out
	}
	var out []smOp
	switch rng.Intn(13) {
	case 12:
		// A person sorts rows, and someone sorts the same rows by the same
		// column again, which moves none of them. When that is another
		// person or the agent, the second sort protects the order
		// (Appendix B, engine item 16): the first person's undo is
		// skipped. The second person may undo first, which finds the rows
		// in order and stamps them for that person's own history. When it
		// is the same person, two undos walk back through both sorts
		// (engine item 23), and two redos forward again.
		first := op("sort", a)
		first.M = 4*rng.Intn(250) + 1 + rng.Intn(3) // named rows, not "all"
		again := first
		switch k := rng.Intn(4); {
		case k == 0:
			out = append(out, first, first, op("undo", a), op("undo", a), op("redo", a), op("redo", a))
			if rng.Intn(2) == 0 {
				out = append(out, op("undo", a), op("undo", a))
			}
			return out
		case k == 1:
			again.Kind, again.Who = "agent sort", 0
		default:
			again.Who = b
		}
		out = append(out, first, again)
		if again.Who != 0 && rng.Intn(2) == 0 {
			out = append(out, op("undo", b))
		}
		out = append(out, op("undo", a), op("redo", a))
	case 11:
		// A person's insert beside a row or column stays unsent while
		// another person deletes that row, and more is inserted and
		// deleted around the place. The insert arrives late and goes
		// where the row was.
		ins := at(op("insert", a))
		ins.Hold = true
		del := at(op("delete", b))
		del.M, del.N = ins.M, rng.Intn(3)
		out = append(out, ins, del)
		for n := rng.Intn(3); n > 0; n-- {
			o := op([]string{"insert", "delete"}[rng.Intn(2)], b)
			o.R, o.C, o.M = (row+rng.Intn(3))%smAreaRows, (col+rng.Intn(2))%smAreaCols, ins.M
			out = append(out, o)
		}
		if rng.Intn(4) == 0 {
			out = append(out, op("undo", b))
		}
		out = append(out, smOp{Kind: "flush", Flag: rng.Intn(2) == 0})
	case 9:
		// A formula names "Gone", which no tab is called, and is
		// overwritten. Rows of its tab are deleted, and the tab is renamed
		// to "Gone": the deletion must not reach the references that had
		// the name before.
		// The formula lies below row 3 and reads row 2 of "Gone"; the
		// deletion takes row 1.
		row = 3 + rng.Intn(smAreaRows-3)
		f := at(op("formula", a))
		f.N = 10 // =Gone!E2+1
		del := op("delete", b)
		del.R, del.N, del.M, del.Flag = 0, 0, 1, false
		gone := op("rename_tab", b)
		gone.N, gone.M = 3, 0
		out = append(out, f, at(op("set", a)), del, gone)
		if rng.Intn(2) == 0 {
			out = append(out, op("undo", b))
		}
		out = append(out, op("undo", a))
	case 10:
		// A formula reads another tab and is overwritten. Rows of that tab
		// are deleted, the tab is deleted, and a new tab takes its name
		// and gets rows: the deletion counts for the references that
		// named the first tab, and the insert for what they read now.
		f := at(op("formula", a))
		f.N, f.M = 42, tab+1 // =MAX(<tab>!A7:<a cell of rows 1 or 2>)
		other := func(kind string) smOp {
			o := op(kind, b)
			o.Tab = tab + 1
			return o
		}
		del := other("delete")
		del.R, del.N, del.M, del.Flag = 0, 0, 1, false
		pick := rng.Intn(4)
		if col%3 == 0 {
			// Or the tab is renamed by a third person, in a way that
			// mixes no references: to the name it has, to that name in
			// other capitals, or to a name no formula holds. The rows
			// return, and the formula reads them again.
			ren := other("rename_tab")
			ren.Who, ren.N = 6-a-b, []int{1, 5, 5, 2}[pick]
			out = append(out, f, at(op("clear", a)), del, ren, op("undo", b), op("undo", a), op("redo", a))
			break
		}
		again := other("add_tab")
		again.N, again.M = 3, 1+pick
		ins := other("insert")
		ins.Tab, ins.R, ins.M, ins.Flag = 7, 4, 1, false // the last tab, above its row 5
		out = append(out, f, at(op("clear", a)), del, other("delete_tab"), again, ins, op("undo", a), op("redo", a))
	case 7, 8:
		// A person writes a cell, and that person or another clears it.
		// Others clear more cells, the tick collects the older blank
		// records, and the person's undos meet a cell without a record:
		// their own blank, which returns, or another writer's.
		out = append(out, smOp{Kind: "read"}, at(op("set", a)), at(op("clear", []int{a, b}[rng.Intn(2)])))
		for n := 1 + rng.Intn(3); n > 0; n-- {
			o := op("clear", b)
			o.R, o.C = rng.Intn(smAreaRows-2), rng.Intn(smAreaCols-2)
			out = append(out, o)
		}
		out = append(out, smOp{Kind: "collect"}, op("undo", a), op("undo", a), op("redo", a))
		if rng.Intn(2) == 0 {
			// The agent guards a range with a state from before.
			o := op("agent write", 0)
			o.State, o.Guard = 1+rng.Intn(4), 2
			out = append(out, o)
		}
	case 5, 6:
		// A person has an entry on a tab: a formula, and rows, columns or
		// a sort. A revision closes. The tab is deleted, renamed or left
		// as it is, and someone brings it back from the revision. The
		// undos after that meet the recovered tab, and one of them takes
		// the recovery itself back.
		out = append(out, at(op("formula", a)), at(op([]string{"delete", "insert", "set", "sort"}[rng.Intn(4)], a)), smOp{Kind: "tick"})
		switch rng.Intn(3) {
		case 0:
			out = append(out, op("delete_tab", b))
		case 1:
			out = append(out, op("rename_tab", b))
		}
		rec := op("recover", []int{a, b}[rng.Intn(2)])
		rec.Flag = true // from the newest revision
		out = append(out, rec, op("undo", a), at(op("set", b)), op("undo", a), op("undo", b), op("redo", a))
	case 0, 1:
		// A formula is overwritten, the structure changes, the overwrite
		// is undone.
		out = append(out, at(op("formula", a)), at(op([]string{"set", "formula", "clear"}[rng.Intn(3)], a)))
		out = append(out, between()...)
		out = append(out, op("undo", a))
	case 2, 3:
		// Rows or columns with a formula in them are deleted, the
		// structure changes, the deletion is undone and redone.
		out = append(out, at(op("formula", b)), at(op("delete", a)))
		out = append(out, between()...)
		out = append(out, op("undo", a))
		if rng.Intn(2) == 0 {
			out = append(out, op("redo", a))
		}
	default:
		// Rows are inserted, someone else writes into them or not, and
		// the insert is undone.
		ins := at(op("insert", a))
		ins.M, ins.Flag = 1, false
		out = append(out, ins)
		if rng.Intn(2) == 0 {
			out = append(out, at(op("set", b)))
		}
		out = append(out, between()...)
		out = append(out, op("undo", a))
	}
	return out
}

// tabName returns the name for a new or renamed tab. Mostly it is a name
// no tab ever had. One time in three it is "Gone", which formulas of the
// generator name while no tab has it, or the name a deleted tab had: a
// name that formulas hold passes to another tab then.
func (r *smRunner) tabName(op smOp) string {
	if op.N%3 == 0 {
		names := append([]string{"Gone"}, r.m.lost...)
		if name := names[op.M%len(names)]; r.m.byName(name) < 0 {
			return name
		}
	}
	r.names++
	return fmt.Sprint("Page", r.names)
}

// renamed returns the new name for a rename of the tab that has the name
// now. One time in six it is that name, as it is or in other capitals.
// After such a rename every formula reads the tab it read.
func (r *smRunner) renamed(op smOp, now string) string {
	switch op.N % 12 {
	case 1:
		return now
	case 5:
		if upper := strings.ToUpper(now); upper != now {
			return upper
		}
		return strings.ToLower(now)
	}
	return r.tabName(op)
}

// smPOp is an operation of a person's action as the model applies it when
// the action arrives: its targets are the ids the person's screen showed.
type smPOp struct {
	kind       string
	tab        uint64
	row, col   uint64
	rows, cols []uint64
	all        bool
	text       string
	shadow     *smShadow
	mask       uint16
	format     formula.Format
	px, count  int
	rowsAxis   bool
	after      bool
	beside     uint64
	by         uint64
	desc       bool
	fr, fc     int
	name       string
}

type smAction struct {
	s    *smSession
	n    uint64
	base uint64
	ops  []string
	mops []smPOp
	desc string
	ack  string // the ack the engine gave, once delivered
}

type smSession struct {
	ord        int
	name       string
	p          *person
	token      string
	n          uint64
	undo, redo []*smEntry
	link       map[smLink]uint64
	queue      []*smAction
	last       *smAction
}

type smSaved struct {
	token string
	seq   uint64
	names []string // the tab names the agent read
}

type smRunner struct {
	t        *testing.T
	e        *env
	m        *smModel
	sessions []*smSession
	saved    []smSaved
	snaps    map[int]*smModel
	revs     int
	names    int
	ticks    int
	adopted  map[string]int
	counts   map[string]int
	site     *site
	views    []*smView
	steps    int
}

// smView is a browser that watches a tab: a connection, and the screen it
// builds from one snapshot and the deltas after it.
type smView struct {
	route string
	c     *client
	sc    *screen
	tab   uint64
	gen   int
	seq   uint64
}

func newSMRunner(t *testing.T) *smRunner {
	e := start(t)
	r := &smRunner{t: t, e: e, snaps: map[int]*smModel{}, adopted: map[string]int{}, counts: map[string]int{}}
	r.m = newSMModel(formula.DateOf(2026, 11, 13))
	r.m.paths, r.m.holder = r.counts, r.holder
	r.m.nextRow, r.m.nextCol = uint64(newTabRows)+1, uint64(newTabCols)+1
	for i := 1; i <= smPeople; i++ {
		r.sessions = append(r.sessions, &smSession{ord: i, name: fmt.Sprint("P", i), link: map[smLink]uint64{}})
	}
	r.connect()
	r.site = serve(e)
	r.views = []*smView{{route: "private"}, {route: "public"}, {route: "private"}}
	r.revs = e.b.importAll(t)
	r.snaps[r.revs] = r.m.clone()
	return r
}

// holder reports whether an entry on some session's undo or redo stack
// holds a stamp of a collected blank record (smModel.holder).
func (r *smRunner) holder(tab, row, col uint64, rec *modelCell) bool {
	for _, s := range r.sessions {
		w := smWriter{link: s.link}
		for _, stack := range [2][]*smEntry{s.undo, s.redo} {
			for _, e := range stack {
				for k := range e.steps {
					x := &e.steps[k]
					if (x.kind != smCell && x.kind != smProp) || x.tab != tab || x.row != row || x.col != col {
						continue
					}
					stamp := rec.cstamp
					if x.kind == smProp {
						stamp = rec.pstamp[x.prop]
					}
					if stamp == w.eff(x.group(), x.wrote) {
						return true
					}
				}
			}
		}
	}
	return false
}

// connect opens a session for every person, as a page does after a load.
func (r *smRunner) connect() {
	for _, s := range r.sessions {
		session, token, err := r.e.in.newSession(context.Background(), "editor:"+s.name, s.name, false)
		if err != nil {
			r.t.Fatal(err)
		}
		s.p, s.token, s.n = &person{e: r.e, s: session}, token, 0
		s.undo, s.redo, s.link, s.queue, s.last = nil, nil, map[smLink]uint64{}, nil, nil
	}
	// No entry is left, so no world is, and no collected stamp an entry
	// could ask for.
	r.m.worlds, r.m.collected = nil, map[[3]uint64]modelCell{}
}

// ---- the engine's state, for the comparison ----

func (r *smRunner) compare() error {
	m := r.m
	if m.fault != nil {
		return m.fault
	}
	m.recalc()
	e := r.e
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	wb := e.in.wb
	if wb.seq != m.seq || wb.resetSeq != m.resetSeq {
		return fmt.Errorf("the engine is at sequence %d with reset %d, the model at %d with %d", wb.seq, wb.resetSeq, m.seq, m.resetSeq)
	}
	if len(wb.tabs) != len(m.tabs) {
		return fmt.Errorf("the engine has %d tabs, the model %d", len(wb.tabs), len(m.tabs))
	}
	if wb.nextTab != m.nextTab || wb.nextRow != m.nextRow || wb.nextCol != m.nextCol {
		return fmt.Errorf("the engine's next ids are %d/%d/%d, the model's %d/%d/%d", wb.nextTab, wb.nextRow, wb.nextCol, m.nextTab, m.nextRow, m.nextCol)
	}
	if wb.tabOrderSeq != m.tabOrderStamp || wb.tabChangeSeq != m.tabChangeStamp {
		return fmt.Errorf("the engine's tab order stamp is %d and its tab change stamp %d, the model's %d and %d", wb.tabOrderSeq, wb.tabChangeSeq, m.tabOrderStamp, m.tabChangeStamp)
	}
	formulas := 0
	for i, mt := range m.tabs {
		st, t := m.meta[i], wb.tabs[i]
		if uint64(t.id) != st.id || t.name != mt.name {
			return fmt.Errorf("tab %d is %d %q in the engine and %d %q in the model", i+1, t.id, t.name, st.id, mt.name)
		}
		if !slices.Equal(t.rows.ids, st.rows) || !slices.Equal(t.cols.ids, st.cols) {
			return fmt.Errorf("%s: rows %v and columns %v in the engine, rows %v and columns %v in the model", mt.name, idRuns(t.rows.ids), idRuns(t.cols.ids), idRuns(st.rows), idRuns(st.cols))
		}
		if t.frozenRows != st.frozenRows || t.frozenCols != st.frozenCols {
			return fmt.Errorf("%s: frozen %d/%d in the engine, %d/%d in the model", mt.name, t.frozenRows, t.frozenCols, st.frozenRows, st.frozenCols)
		}
		if t.born != st.born || t.nameSeq != st.nameStamp || t.settingsSeq != st.settingsStamp || t.structSeq != st.structStamp {
			return fmt.Errorf("%s: stamps born %d name %d settings %d struct %d in the engine, %d %d %d %d in the model", mt.name,
				t.born, t.nameSeq, t.settingsSeq, t.structSeq, st.born, st.nameStamp, st.settingsStamp, st.structStamp)
		}
		for _, id := range st.rows {
			if got := t.rows.born.get(id); got != st.rowBorn[id] {
				return fmt.Errorf("%s: row %d was born at %d in the engine, at %d in the model", mt.name, id, got, st.rowBorn[id])
			}
		}
		for _, id := range st.cols {
			if got := t.cols.born.get(id); got != st.colBorn[id] {
				return fmt.Errorf("%s: column %d was born at %d in the engine, at %d in the model", mt.name, id, got, st.colBorn[id])
			}
			w, has := t.widths[ColID(id)]
			px, modelHas := st.widths[id]
			if !has {
				w.px = defaultWidth
			}
			if !modelHas {
				px = defaultWidth
			}
			if w.px != px || w.seq != st.wstamp[id] {
				return fmt.Errorf("%s: column %d is %d px wide, stamp %d, in the engine and %d, stamp %d, in the model", mt.name, id, w.px, w.seq, px, st.wstamp[id])
			}
		}
		for row, line := range mt.grid {
			for col, want := range line {
				at := formula.Pos{Row: int32(row), Col: int32(col)}
				got := t.cellAt(at)
				if got == nil && want == nil {
					continue
				}
				var gotText, wantText string
				var gotVal, wantVal formula.Value
				var gotFormat, wantFormat formula.Format
				var gotStamps, wantStamps [2 + propCount]uint64
				if got != nil {
					gotVal = got.val
					if got.f != nil {
						gotText = got.f.Text(at) + " stored as " + got.f.Stored()
					} else {
						gotText = formula.EditText(got.content, at)
					}
					state := wb.styles.state(got.style)
					gotFormat = state.f
					gotStamps[0], gotStamps[1] = got.seq, got.textSeq
					copy(gotStamps[2:], state.stamps[:])
				}
				if want != nil {
					wantVal, wantFormat = want.val, want.format
					if want.f != nil {
						formulas++
						wantText = want.f.Text(at) + " stored as " + want.f.Stored()
					} else {
						wantText = formula.EditText(want.content, at)
					}
					wantStamps[0], wantStamps[1] = want.cstamp, want.tstamp
					copy(wantStamps[2:], want.pstamp[:])
				}
				if gotText != wantText || gotVal != wantVal || gotFormat != wantFormat {
					return fmt.Errorf("%s!%s holds %q = %+v with format %+v in the engine and %q = %+v with %+v in the model", mt.name, at.A1(), gotText, gotVal, gotFormat, wantText, wantVal, wantFormat)
				}
				if gotStamps != wantStamps {
					return fmt.Errorf("%s!%s (%q) has the stamps %v in the engine and %v in the model (content, text, then the format properties)", mt.name, at.A1(), gotText, gotStamps, wantStamps)
				}
			}
		}
	}
	if wb.count.formulas != formulas {
		return fmt.Errorf("the engine counts %d formulas and the model %d", wb.count.formulas, formulas)
	}
	if err := r.counters(wb); err != nil {
		return err
	}
	// The undo and redo stacks hold the entries the model says they hold,
	// and none of them has expired (product rule 23, last clause): no
	// history of the test is large enough for a limit of undo history, so
	// an entry that the engine dropped or marked as lost is a failure
	// here, whether or not anybody undoes it later.
	for _, s := range r.sessions {
		u := &s.p.s.undo
		if len(u.undo) != len(s.undo) || len(u.redo) != len(s.redo) {
			return fmt.Errorf("session %d has %d undo and %d redo entries in the engine, %d and %d in the model", s.ord, len(u.undo), len(u.redo), len(s.undo), len(s.redo))
		}
		for _, stack := range [2][]*undoEntry{u.undo, u.redo} {
			for _, e := range stack {
				if e.lost {
					return fmt.Errorf("an entry of session %d, of sequence %d, has expired", s.ord, e.seq)
				}
			}
		}
	}
	r.counts["undo stacks compared, none of their entries expired"]++
	return wholeWorkbook(wb)
}

// counters compares the counters the limits of section 11 are checked
// against with what the model counts in its own grids: cell records with
// content or a format, blank records without a format, formulas, the
// characters of typed text, the cells all formulas read, and the
// characters of the texts formulas calculate. The model calculates no
// limited mode, and no history of the test is large enough for one.
func (r *smRunner) counters(wb *Workbook) error {
	m := r.m
	var cells, blanks, formulas int
	var text, formulaText, reads, calcText int64
	for _, mt := range m.tabs {
		dims := func(name string) (int32, int32, bool) {
			t := mt
			if name != "" {
				i := m.byName(name)
				if i < 0 {
					return 0, 0, false
				}
				t = m.tabs[i]
			}
			return int32(len(t.grid)), int32(t.cols), true
		}
		for row, line := range mt.grid {
			for col, cell := range line {
				if cell == nil {
					continue
				}
				if cell.content.Kind == formula.ContentBlank && cell.format == (formula.Format{}) {
					blanks++
				} else {
					cells++
				}
				at := formula.Pos{Row: int32(row), Col: int32(col)}
				switch cell.content.Kind {
				case formula.ContentText, formula.ContentUnreadable:
					text += int64(utf8.RuneCountInString(cell.content.Text))
				case formula.ContentFormula:
					formulaText += int64(cell.f.TextLen(at))
				}
				if cell.f != nil {
					formulas++
					reads += cell.f.ReadCells(at, dims)
					calcText += int64(cell.val.TextLen())
				}
			}
		}
	}
	c := wb.count
	if c.cells != cells || c.blanks != blanks || c.formulas != formulas {
		return fmt.Errorf("the engine counts %d cells, %d blank records and %d formulas, the model %d, %d and %d", c.cells, c.blanks, c.formulas, cells, blanks, formulas)
	}
	if c.typed != text {
		return fmt.Errorf("the engine counts %d characters of typed text outside formulas, the model %d", c.typed, text)
	}
	// The engine counts the text of the formulas when a limit check asks,
	// and keeps the sum until a formula or the structure changes.
	if wb.formulaTextOK && wb.formulaText != formulaText {
		return fmt.Errorf("the engine counts %d characters of formula text, the model %d", wb.formulaText, formulaText)
	}
	// The count of read cells belongs to the index of what formulas read.
	// An action that is taken back leaves the index to be built again by
	// the next recalculation, and the count is built with it.
	if !wb.deps.stale && c.reads != reads {
		return fmt.Errorf("the engine counts %d cells that formulas read, the model %d", c.reads, reads)
	}
	if c.calcText != calcText {
		return fmt.Errorf("the engine counts %d characters of calculated text, the model %d", c.calcText, calcText)
	}
	if wb.limited {
		return fmt.Errorf("the engine is in limited mode")
	}
	r.counts["limit counters compared"]++
	if wb.formulaTextOK {
		r.counts["limit counters compared, the sum of formula text included"]++
	}
	if !wb.deps.stale {
		r.counts["limit counters compared, the count of read cells included"]++
	}
	return nil
}

// ---- screens ----

// watch opens a connection of the view when it has none, and a snapshot of
// a tab when the view has none or its tab is gone.
func (r *smRunner) watch(v *smView, pick int) {
	if v.c == nil {
		hello := helloV1
		if v.route == "private" {
			hello = `{"type":"hello","version":1,"name":"Viewer","pending":0}`
		}
		v.c = r.site.dial(v.route, hello)
		welcome := v.c.next("welcome")
		v.seq, _ = strconv.ParseUint(welcome["seq"].(string), 10, 64)
		v.sc, v.gen = nil, 0
	}
	if v.sc != nil && r.m.index(v.tab) >= 0 {
		return
	}
	v.gen++
	v.tab = r.m.meta[pick%len(r.m.meta)].id
	head, cells := v.c.watch(v.gen, TabID(v.tab))
	v.sc = newScreen(head, cells)
	if seq, _ := strconv.ParseUint(head["seq"].(string), 10, 64); seq > v.seq {
		v.seq = seq
	}
}

// screens brings every view to the sheet's sequence through the deltas it
// was sent. With check set it also compares each screen with a snapshot a
// new connection gets: a screen that applied only deltas must show what a
// page that loads now shows (section 12.3).
func (r *smRunner) screens(check bool) error {
	for i, v := range r.views {
		r.watch(v, i)
		for v.seq < r.m.seq {
			f, ok := v.c.wait("delta", "reset", "resync")
			if !ok {
				return fmt.Errorf("the %s connection got no delta after sequence %d; the sheet is at %d", v.route, v.seq, r.m.seq)
			}
			if f["type"] != "delta" {
				// After a rollback, or when the connection fell behind,
				// a page starts over.
				v.c.ws.Close(1000, "")
				v.c = nil
				r.watch(v, i)
				continue
			}
			seq, _ := strconv.ParseUint(f["seq"].(string), 10, 64)
			if seq != v.seq+1 {
				return fmt.Errorf("the %s connection got delta %d after %d", v.route, seq, v.seq)
			}
			v.seq = seq
			v.sc.apply(f)
			// A page whose tab is deleted leaves it and watches another.
			// It does not come back by itself when a recovery brings a
			// tab with that id back later in the same batch of deltas.
			if tabs, ok := f["tabs"].([]any); ok {
				here := false
				for _, t := range tabs {
					if tab, ok := t.(map[string]any); ok && fmt.Sprint(tab["id"]) == fmt.Sprint(v.tab) {
						here = true
					}
				}
				if !here {
					v.sc = nil
					r.watch(v, i)
					r.counts["a screen left its tab when the tab was deleted"]++
				}
			}
		}
		if r.m.index(v.tab) < 0 {
			r.watch(v, i)
		}
		if !check {
			continue
		}
		fresh := r.site.dial(v.route, helloV1)
		fresh.next("welcome")
		head, cells := fresh.watch(1, TabID(v.tab))
		fresh.ws.Close(1000, "")
		if head["seq"] != fmt.Sprint(r.m.seq) {
			return fmt.Errorf("a new %s connection got a snapshot at %v; the sheet is at %d", v.route, head["seq"], r.m.seq)
		}
		if diff := v.sc.diff(newScreen(head, cells)); diff != "" {
			return fmt.Errorf("the %s screen of tab %d, which applied deltas through %d: %s", v.route, v.tab, v.seq, diff)
		}
		r.counts["screens compared with a new snapshot"]++
	}
	return nil
}

// ---- people ----

func runsOf(ids []uint64) string {
	data, _ := json.Marshal(idRuns(ids))
	return string(data)
}

func (r *smRunner) tabOf(op smOp) (int, *modelTab, *smTab) {
	ti := op.Tab % len(r.m.tabs)
	return ti, r.m.tabs[ti], r.m.meta[ti]
}

// smFormat returns the format a step writes and its JSON: the properties
// of the step's mask, set to values or reset to their defaults.
func smFormat(op smOp) (formula.Format, string) {
	var f formula.Format
	fields := []string{}
	add := func(prop int, name, value string, set func()) {
		if op.Formats&(1<<prop) == 0 {
			return
		}
		if op.Flag {
			set()
			fields = append(fields, fmt.Sprintf("%q:%s", name, value))
		} else {
			fields = append(fields, fmt.Sprintf("%q:null", name))
		}
	}
	add(propBold, "bold", "true", func() { f.Bold = true })
	add(propItalic, "italic", "true", func() { f.Italic = true })
	add(propUnderline, "underline", "true", func() { f.Underline = true })
	add(propStrike, "strikethrough", "true", func() { f.Strikethrough = true })
	color := formula.Color(1 + op.N%formula.PaletteColors)
	add(propTextColor, "text_color", strconv.Quote(color.String()), func() { f.TextColor = color })
	add(propFill, "fill", strconv.Quote(color.String()), func() { f.Fill = color })
	align := formula.Align(1 + op.M%3)
	add(propAlign, "align", strconv.Quote(alignNames[align]), func() { f.Align = align })
	add(propWrap, "wrap", "true", func() { f.Wrap = true })
	number := formula.NumberFormat{Kind: formula.NumberPercent, Decimals: uint8(op.N % 4)}
	data, _ := number.MarshalJSON()
	add(propNumber, "number", string(data), func() { f.Number = number })
	return f, "{" + strings.Join(fields, ",") + "}"
}

// compose turns a step into an action against the screen of now: the ids
// it names are the ones at the positions the step gives.
func (r *smRunner) compose(op smOp) *smAction {
	m := r.m
	s := r.sessions[(op.Who-1)%len(r.sessions)]
	ti, _, st := r.tabOf(op)
	rowAt := func(i int) uint64 { return st.rows[i%min(len(st.rows), smAreaRows)] }
	colAt := func(i int) uint64 { return st.cols[i%min(len(st.cols), smAreaCols)] }
	span := func(axis []uint64, from, n, area int) []uint64 {
		from %= min(len(axis), area)
		return slices.Clone(axis[from:min(len(axis), from+1+n%3)])
	}
	a := &smAction{s: s, base: m.seq, desc: op.String()}
	one := func(json string, mop smPOp) {
		a.ops, a.mops = append(a.ops, json), append(a.mops, mop)
	}
	switch op.Kind {
	case "set":
		row, col := rowAt(op.R), colAt(op.C)
		text, _ := json.Marshal(op.Text)
		one(fmt.Sprintf(`{"op":"set","tab":%d,"row":%d,"col":%d,"text":%s}`, st.id, row, col, text), smPOp{kind: "set", tab: st.id, row: row, col: col, text: op.Text})
		if op.Flag && op.Formats != 0 {
			// A second operation of the same action, on the same cell.
			f, set := smFormat(op)
			one(fmt.Sprintf(`{"op":"format","tab":%d,"rows":[%d],"cols":[%d],"set":%s}`, st.id, row, col, set),
				smPOp{kind: "format", tab: st.id, rows: []uint64{row}, cols: []uint64{col}, mask: op.Formats, format: f})
		}
	case "formula":
		row, col := rowAt(op.R), colAt(op.C)
		r1, c1, _ := m.at(ti, row, col)
		name := func(k int) string {
			return formula.Pos{Row: int32(k / 7 % smAreaRows), Col: int32(k % smAreaCols)}.A1()
		}
		text := fmt.Sprintf(smFormulas[op.N%len(smFormulas)], name(op.N), name(op.M), formula.ColumnName(int32(op.M%smAreaCols)), op.N%smAreaRows+1, quoteTab(m.tabs[op.M%len(m.tabs)].name))
		if op.N%9 == 8 {
			// A formula that reads the last row and the last column.
			text = fmt.Sprintf("=%s+%s", formula.Pos{Row: int32(len(st.rows) - 1), Col: int32(op.M % smAreaCols)}.A1(),
				formula.Pos{Row: int32(op.M % smAreaRows), Col: int32(len(st.cols) - 1)}.A1())
		}
		pos := formula.Pos{Row: int32(r1), Col: int32(c1)}
		f, err := formula.Parse(text, pos, formula.Limits{})
		if err != nil {
			return nil
		}
		sh := &smShadow{tab: st.id, row: row, col: col, f: f, pos: pos}
		m.shadows = append(m.shadows, sh)
		data, _ := json.Marshal(text)
		one(fmt.Sprintf(`{"op":"set","tab":%d,"row":%d,"col":%d,"text":%s,"at":[%d,%d]}`, st.id, row, col, data, r1, c1), smPOp{kind: "formula", tab: st.id, row: row, col: col, shadow: sh})
	case "clear", "format":
		rows, cols := span(st.rows, op.R, op.N, smAreaRows), span(st.cols, op.C, op.M, smAreaCols)
		mop := smPOp{kind: op.Kind, tab: st.id, rows: rows, cols: cols}
		extra := ""
		if op.Kind == "format" {
			var set string
			mop.format, set = smFormat(op)
			mop.mask = op.Formats
			extra = `,"set":` + set
		}
		one(fmt.Sprintf(`{"op":%q,"tab":%d,"rows":%s,"cols":%s%s}`, op.Kind, st.id, runsOf(rows), runsOf(cols), extra), mop)
	case "width":
		cols, px := span(st.cols, op.C, op.M, smAreaCols), 40+op.N%400
		one(fmt.Sprintf(`{"op":"width","tab":%d,"cols":%s,"width":%d}`, st.id, runsOf(cols), px), smPOp{kind: "width", tab: st.id, cols: cols, px: px})
	case "insert":
		rows, count := op.M%3 != 0, 1+op.N%2
		beside, side := colAt(op.C), "before"
		if rows {
			beside = rowAt(op.R)
		}
		if op.Flag {
			side = "after"
		}
		kind := map[bool]string{true: "insert_rows", false: "insert_cols"}[rows]
		one(fmt.Sprintf(`{"op":%q,"tab":%d,"beside":%d,"side":%q,"count":%d}`, kind, st.id, beside, side, count),
			smPOp{kind: "insert", tab: st.id, rowsAxis: rows, beside: beside, after: op.Flag, count: count})
	case "append":
		rows, count := op.M%2 == 0, 1+op.N%2
		kind := map[bool]string{true: "append_rows", false: "append_cols"}[rows]
		one(fmt.Sprintf(`{"op":%q,"tab":%d,"count":%d}`, kind, st.id, count), smPOp{kind: "append", tab: st.id, rowsAxis: rows, count: count})
	case "delete":
		rows := op.M%3 != 0
		ids := span(st.cols, op.C, op.N, smAreaCols)
		if rows {
			ids = span(st.rows, op.R, op.N, smAreaRows)
		}
		if op.Flag && rows && len(st.rows) > 8 {
			// Two stretches of one deletion.
			ids = append(ids, st.rows[(op.R+4)%smAreaRows])
			slices.Sort(ids)
			ids = slices.Compact(ids)
		}
		if axis := st.axis(rows); op.N%7 == 6 && len(axis) > smAreaRows+6 {
			// The last rows or columns of the tab.
			ids = slices.Clone(axis[len(axis)-1-op.N%2:])
		}
		kind := map[bool]string{true: "delete_rows", false: "delete_cols"}[rows]
		one(fmt.Sprintf(`{"op":%q,"tab":%d,"ids":%s}`, kind, st.id, runsOf(ids)), smPOp{kind: "delete", tab: st.id, rowsAxis: rows, rows: ids})
	case "sort":
		by := colAt(op.C)
		mop := smPOp{kind: "sort", tab: st.id, by: by, desc: op.Flag}
		rows := `"all"`
		if op.M%4 == 0 {
			mop.all = true
		} else {
			mop.rows = slices.Clone(st.rows[op.R%4 : min(len(st.rows), op.R%4+2+op.N%5)])
			rows = runsOf(mop.rows)
		}
		one(fmt.Sprintf(`{"op":"sort","tab":%d,"rows":%s,"by":%d,"desc":%v}`, st.id, rows, by, op.Flag), mop)
	case "settings":
		fr, fc := op.N%3, op.M%2
		one(fmt.Sprintf(`{"op":"settings","tab":%d,"frozen_rows":%d,"frozen_cols":%d}`, st.id, fr, fc), smPOp{kind: "settings", tab: st.id, fr: fr, fc: fc})
	case "add_tab":
		if len(m.tabs) >= smMaxTabs {
			return nil
		}
		name := r.tabName(op)
		one(fmt.Sprintf(`{"op":"add_tab","name":%q}`, name), smPOp{kind: "add_tab", name: name})
	case "rename_tab":
		name := r.renamed(op, m.tabs[ti].name)
		one(fmt.Sprintf(`{"op":"rename_tab","tab":%d,"name":%q}`, st.id, name), smPOp{kind: "rename_tab", tab: st.id, name: name})
	case "move_tab":
		before := m.meta[op.N%len(m.meta)].id
		field := fmt.Sprintf(`,"before":%d`, before)
		if op.Flag {
			before, field = 0, ""
		}
		one(fmt.Sprintf(`{"op":"move_tab","tab":%d%s}`, st.id, field), smPOp{kind: "move_tab", tab: st.id, beside: before})
	case "delete_tab":
		if len(m.tabs) < 2 {
			return nil
		}
		one(fmt.Sprintf(`{"op":"delete_tab","tab":%d}`, st.id), smPOp{kind: "delete_tab", tab: st.id})
	case "undo", "redo":
		one(fmt.Sprintf(`{"op":%q}`, op.Kind), smPOp{kind: op.Kind})
	default:
		return nil
	}
	s.n++
	a.n = s.n
	return a
}

func (m *smModel) live(ti int, rows bool, id uint64, base uint64) bool {
	born, ok := m.meta[ti].bornOf(rows)[id]
	return ok && born <= base
}

// applyPerson applies one operation of an action as section 7.1 says and
// returns the status the server must answer with.
func (r *smRunner) applyPerson(w *smWriter, a *smAction, op *smPOp) (string, error) {
	m, s := r.m, a.s
	const gone = "ignored:gone"
	switch op.kind {
	case "add_tab":
		if m.byName(op.name) >= 0 {
			return "refused:invalid", nil // another action took the name first
		}
		m.addTab(w, op.name, len(m.tabs))
		return "applied", nil
	case "undo", "redo":
		stack := &s.undo
		if op.kind == "redo" {
			stack = &s.redo
		}
		if len(*stack) == 0 {
			return "applied", nil
		}
		entry := (*stack)[len(*stack)-1]
		*stack = (*stack)[:len(*stack)-1]
		return "applied", m.undo(w, entry)
	}
	ti := m.index(op.tab)
	if ti < 0 || m.meta[ti].born > a.base {
		return gone, nil
	}
	st, mt := m.meta[ti], m.tabs[ti]
	liveIDs := func(rows bool, ids []uint64) []uint64 {
		var out []uint64
		for _, id := range ids {
			if m.live(ti, rows, id, a.base) {
				out = append(out, id)
			}
		}
		return out
	}
	switch op.kind {
	case "set", "formula":
		if !m.live(ti, true, op.row, a.base) || !m.live(ti, false, op.col, a.base) {
			return gone, nil
		}
		if op.kind == "set" {
			m.typed(w, ti, op.row, op.col, op.text)
			return "applied", nil
		}
		if a.base < m.resetSeq || op.shadow.stale {
			return "refused:stale_formula", nil
		}
		m.writeContent(w, ti, op.row, op.col, formula.Content{}, op.shadow.f)
	case "clear", "format":
		for _, row := range liveIDs(true, op.rows) {
			for _, col := range liveIDs(false, op.cols) {
				rr, cc, _ := m.at(ti, row, col)
				cell := mt.grid[rr][cc]
				switch {
				case op.kind == "format":
					// A format that only resets properties makes no record.
					if cell != nil || op.format != (formula.Format{}) {
						m.writeProps(w, ti, row, col, op.mask, op.format)
					}
				case cell != nil:
					m.writeContent(w, ti, row, col, formula.Content{}, nil)
				}
			}
		}
	case "width":
		// A person's operation that leaves what it names as it is changes
		// nothing (technical design, Appendix B, engine item 6).
		for _, col := range liveIDs(false, op.cols) {
			if px, had := st.widths[col]; (had && px == op.px) || (!had && op.px == defaultWidth) {
				continue
			}
			m.setWidth(w, ti, col, op.px)
		}
	case "insert":
		at := m.pos(ti, op.rowsAxis, op.beside)
		if at < 0 {
			// The row is gone: the insert goes where it was, which the
			// model's own log of structural edits says, or it is ignored
			// when that log does not reach back.
			var ok bool
			if at, ok = m.besideGone(op.tab, op.rowsAxis, op.beside, a.base); !ok {
				r.counts["placement: an insert beside a row that is gone, ignored"]++
				return gone, nil
			}
			r.counts["placement: an insert beside a row that is gone, placed by the model's log"]++
			at = min(at, len(st.axis(op.rowsAxis)))
		} else if op.after {
			at++
		}
		m.create(w, ti, op.rowsAxis, at, op.count)
	case "append":
		m.create(w, ti, op.rowsAxis, len(st.axis(op.rowsAxis)), op.count)
	case "delete":
		var positions []int
		for _, id := range liveIDs(op.rowsAxis, op.rows) {
			positions = append(positions, m.pos(ti, op.rowsAxis, id))
		}
		slices.Sort(positions)
		if len(positions) == len(st.axis(op.rowsAxis)) {
			return "refused:invalid", nil
		}
		m.remove(w, ti, op.rowsAxis, positions)
	case "sort":
		if !m.live(ti, false, op.by, a.base) {
			return gone, nil
		}
		ids := op.rows
		if op.all {
			ids = st.rows
		}
		var positions []int
		for _, id := range liveIDs(true, ids) {
			positions = append(positions, m.pos(ti, true, id))
		}
		slices.Sort(positions)
		m.sortRows(w, ti, positions, m.pos(ti, false, op.by), op.desc)
	case "settings":
		if op.fr != st.frozenRows || op.fc != st.frozenCols {
			m.setSettings(w, ti, op.fr, op.fc)
		}
	case "rename_tab":
		if i := m.byName(op.name); i >= 0 && i != ti {
			return "refused:invalid", nil
		}
		if op.name != m.tabs[ti].name {
			m.rename(w, ti, op.name)
		}
	case "move_tab":
		to := len(m.tabs) - 1
		if op.beside != 0 {
			bi := m.index(op.beside)
			if bi < 0 {
				return gone, nil
			}
			if to = bi; bi > ti {
				to--
			}
		}
		if to != ti {
			m.moveTab(w, ti, to)
		}
	case "delete_tab":
		if len(m.tabs) == 1 {
			return "refused:invalid", nil
		}
		m.deleteTab(w, ti)
	}
	return "applied", nil
}

// arrived applies a delivered action to the model and checks the ack.
// last says that the engine's state is the one right after this action,
// so the model may read it for what the design leaves open.
func (r *smRunner) arrived(a *smAction, ack ackBody, last bool) error {
	m, s := r.m, a.s
	w := &smWriter{by: s.ord, stamp: m.seq + 1, rec: &smEntry{}, link: s.link}
	statuses := make([]string, len(a.mops))
	undo, redo := false, false
	for i := range a.mops {
		op := &a.mops[i]
		undo, redo = undo || op.kind == "undo", redo || op.kind == "redo"
		status, err := r.applyPerson(w, a, op)
		if err != nil {
			return fmt.Errorf("%s: %w", a.desc, err)
		}
		statuses[i] = status
		if op.shadow != nil {
			m.shadows = slices.DeleteFunc(m.shadows, func(sh *smShadow) bool { return sh == op.shadow })
		}
	}
	data, _ := json.Marshal(ack)
	a.ack, s.last = string(data), a
	got := status(ack)
	// No action of a history is near the time an action has or the bound
	// of what it may change, so neither refusal is one the model words
	// differently: it is a failure.
	if strings.Contains(got, "recalculation_time") || strings.Contains(got, "action_changes") {
		return fmt.Errorf("%s was answered with %q; no action of the test is that large", a.desc, got)
	}
	if want := strings.Join(statuses, ","); got != want {
		// A status the model words differently from the engine is not a
		// difference when it names the same outcome.
		if !(strings.HasPrefix(want, "refused:invalid") && strings.HasPrefix(got, "refused:")) {
			return fmt.Errorf("%s was answered with %q, the model says %q", a.desc, got, want)
		}
	}
	advanced := ack.Seq != fmt.Sprint(m.seq)
	switch {
	case w.changed && !advanced && (undo || redo):
		return fmt.Errorf("%s took no sequence, the model applied %d steps of it and skipped %d", a.desc, w.applied, w.skipped)
	case w.changed && !advanced:
		// An accepted write takes a sequence also when it writes an equal
		// value (section 4.1). The model decides by itself what changes;
		// an action the engine answered without a sequence dropped it.
		return fmt.Errorf("%s took no sequence, the model says it changes the sheet", a.desc)
	case !w.changed && advanced:
		return fmt.Errorf("%s took sequence %s, the model says it changes nothing", a.desc, ack.Seq)
	case !w.changed:
		if undo || redo {
			r.counts[fmt.Sprintf("undo or redo without a sequence: applied %v, skipped %v", w.applied > 0, w.skipped > 0)]++
			if w.skipped > 0 {
				r.counts["undo or redo: the first skipped step: "+w.reason]++
			}
			// An undo with nothing on its stack answers without counts.
			if got := ack.Undo; got != nil && (got.Applied != w.applied || got.Skipped != w.skipped || got.Reason != w.reason) || got == nil && w.applied+w.skipped > 0 {
				return fmt.Errorf("%s: the engine answered %+v without a sequence, the model applied %d steps and skipped %d (%s)", a.desc, ack.Undo, w.applied, w.skipped, w.reason)
			}
		}
		return nil
	}
	m.seq++
	if ack.Seq != fmt.Sprint(m.seq) {
		return fmt.Errorf("%s took sequence %s, the model is at %d", a.desc, ack.Seq, m.seq)
	}
	if last {
	}
	if len(w.rec.steps) > 0 {
		switch {
		case undo:
			s.redo = append(s.redo, w.rec)
		case redo:
			s.undo = append(s.undo, w.rec)
		default:
			s.undo, s.redo = append(s.undo, w.rec), nil
		}
	}
	if undo || redo {
		r.counts[fmt.Sprintf("undo or redo with a sequence: applied %v, skipped %v", w.applied > 0, w.skipped > 0)]++
		if ack.Undo == nil || ack.Undo.Applied != w.applied || ack.Undo.Skipped != w.skipped || ack.Undo.Reason != w.reason {
			return fmt.Errorf("%s: the engine answered %+v, the model applied %d steps and skipped %d (%s)", a.desc, ack.Undo, w.applied, w.skipped, w.reason)
		}
		r.counts["kept formulas the model followed by itself"] += w.followed
		if w.skipped > 0 {
			r.counts["undo or redo: the first skipped step: "+w.reason]++
		}
	}
	return nil
}

// deliver sends actions, one of each session at most, to the engine: alone,
// or together behind a held writer so that they can share a transaction.
func (r *smRunner) deliver(batch []*smAction) error {
	acks := make([]ackBody, len(batch))
	errs := make([]error, len(batch))
	if len(batch) == 1 {
		acks[0], errs[0] = batch[0].s.p.send(batch[0].n, batch[0].base, batch[0].ops...)
	} else {
		acts := make([]func(), len(batch))
		var mu sync.Mutex
		for i, a := range batch {
			acts[i] = func() {
				ack, err := a.s.p.send(a.n, a.base, a.ops...)
				mu.Lock()
				acks[i], errs[i] = ack, err
				mu.Unlock()
			}
		}
		r.counts[fmt.Sprintf("%d actions in %d transactions", len(batch), r.e.group(acts...))]++
	}
	for i, a := range batch {
		if errs[i] != nil {
			return fmt.Errorf("%s: %v", a.desc, errs[i])
		}
		if err := r.arrived(a, acks[i], i == len(batch)-1); err != nil {
			return err
		}
	}
	return r.compare()
}

// alone says whether an action must arrive by itself: the model reads the
// engine's answer for it (adopt), which has to be the state right after.
func (r *smRunner) alone(a *smAction) bool {
	for _, op := range a.mops {
		if op.kind == "undo" || op.kind == "redo" {
			return true
		}
		if ti := r.m.index(op.tab); op.kind == "insert" && ti >= 0 && r.m.pos(ti, op.rowsAxis, op.beside) < 0 {
			return true
		}
	}
	return false
}

// flush delivers every unsent action. together sends the heads of the
// sessions' queues behind a held writer; otherwise they go one by one, the
// sessions taking turns.
func (r *smRunner) flush(together bool) error {
	for {
		var heads []*smAction
		for _, s := range r.sessions {
			if len(s.queue) > 0 {
				heads = append(heads, s.queue[0])
			}
		}
		if len(heads) == 0 {
			return nil
		}
		if together && len(heads) > 1 && !slices.ContainsFunc(heads, r.alone) {
			for _, a := range heads {
				a.s.queue = a.s.queue[1:]
			}
			if err := r.deliver(heads); err != nil {
				return err
			}
			continue
		}
		for _, a := range heads {
			a.s.queue = a.s.queue[1:]
			if err := r.deliver([]*smAction{a}); err != nil {
				return err
			}
		}
	}
}

// ---- the agent ----

// agent sends one call and applies it to the model, or checks its refusal
// (sections 7.3 and 7.4).
// smCall is what the model knows of an agent call beside its operations:
// the state it names and the stamps the tabs had before it. The checks of
// section 7.3 read those, so that the call's own operations do not count.
type smCall struct {
	state   uint64
	names   []string // the tab names the agent read with an old state
	before  map[uint64][3]uint64
	refusal string
	// failed says that the operation checked last is one the call is
	// refused for, also when a guard has refused the call already.
	failed bool
}

// check is the identity check of a tab an operation names and, for an
// operation with an address, the structure check. A tab the call made
// itself has neither.
func (c *smCall) check(m *smModel, ti int, address bool) {
	why := ""
	if ti < 0 {
		why = "conflict tab_changed"
	} else if b, old := c.before[m.meta[ti].id]; old {
		switch {
		case b[0] > c.state, b[1] > c.state:
			why = "conflict tab_changed"
		case address && b[2] > c.state:
			why = "conflict structure_changed"
		}
	}
	if why != "" {
		c.failed = true
		if c.refusal == "" {
			c.refusal = why
		}
	}
}

// several returns the operations of a call of several: structural and
// content operations in any order, on one tab or on more. Every other such
// call starts with a rename of a tab that the next operation then inserts
// rows or columns into, under the name the call gave it.
func (r *smRunner) several(op smOp) []smOp {
	rng := rand.New(rand.NewSource(int64(op.N)*1000 + int64(op.M)))
	kinds := []string{"write", "write", "clear", "format", "widths", "insert", "insert", "delete", "delete", "sort", "add_tab", "rename_tab",
		"move_tab", "delete_tab", "tab_settings"}
	ops := make([]smOp, 2+rng.Intn(3))
	for i := range ops {
		o := op
		o.Kind = kinds[rng.Intn(len(kinds))]
		o.R, o.C, o.N, o.M = rng.Intn(smAreaRows), rng.Intn(smAreaCols), rng.Intn(1000), rng.Intn(1000)
		o.Text, o.Flag = smTexts[rng.Intn(len(smTexts))], rng.Intn(2) == 0
		o.Formats = uint16(1 + rng.Intn(1<<propCount-1))
		if rng.Intn(3) != 0 {
			o.Tab = rng.Intn(8)
		}
		ops[i] = o
	}
	if op.Flag {
		ops[0].Kind, ops[1].Kind, ops[1].Tab = "rename_tab", "insert", ops[0].Tab
	}
	return ops
}

func (r *smRunner) agent(op smOp) error {
	m := r.m
	kind := strings.TrimPrefix(op.Kind, "agent ")
	c := &smCall{state: m.seq, before: map[uint64][3]uint64{}}
	token := ""
	if op.State != 0 && len(r.saved) > 0 {
		sv := r.saved[(op.State-1)%len(r.saved)]
		c.state, token, c.names = sv.seq, sv.token, sv.names
	}
	for _, st := range m.meta {
		c.before[st.id] = [3]uint64{st.born, st.nameStamp, st.structStamp}
	}
	state := c.state
	if token != "" && m.resetSeq > state {
		c.refusal = "conflict rolled_back"
	}
	guard := ""
	if op.Guard == 1 {
		guard = `,"guard":{"unchanged":true}`
		if c.refusal == "" && m.seq != state {
			c.refusal = "conflict guard"
		}
	} else if op.Guard == 2 {
		ti, mt, _ := r.tabOf(op)
		name := mt.name
		if c.names != nil {
			name = c.names[op.Tab%len(c.names)]
			ti = m.byName(name)
		}
		r1, c1 := op.R, op.C
		r2, c2 := r1+op.N%3, c1+op.M%2
		a1 := formula.Pos{Row: int32(r1), Col: int32(c1)}.A1() + ":" + formula.Pos{Row: int32(r2), Col: int32(c2)}.A1()
		ranges, _ := json.Marshal([]string{quoteTab(name) + "!" + a1})
		guard = fmt.Sprintf(`,"guard":{"ranges":%s}`, ranges)
		if c.check(m, ti, true); c.refusal == "" && state < m.guardFloor {
			// A blank record the guard would have read may be gone.
			c.refusal = "conflict state_too_old"
		} else if c.refusal == "" {
			gt := m.tabs[ti]
		cells:
			for row := r1; row <= min(r2, len(gt.grid)-1); row++ {
				for col := c1; col <= min(c2, gt.cols-1); col++ {
					if cell := gt.grid[row][col]; cell != nil && max(cell.cstamp, cell.tstamp) > state {
						c.refusal = "conflict guard"
						break cells
					}
				}
			}
		}
	}
	ops := []smOp{op}
	if kind == "several" {
		ops = r.several(op)
	}
	// build makes the operations one after the other on r.m, each on the
	// sheet as the ones before left it, and stops at the first the model
	// refuses.
	build := func(w *smWriter, call smCall) ([]string, string) {
		var bodies []string
		for _, sub := range ops {
			call.failed = false
			body, apply := r.agentOp(sub, w, &call)
			if body == "" {
				continue
			}
			bodies = append(bodies, body)
			if call.failed {
				break
			}
			apply()
		}
		return bodies, call.refusal
	}
	w := &smWriter{by: 0, stamp: m.seq + 1}
	var bodies []string
	refusal := c.refusal
	if len(ops) == 1 {
		body, apply := r.agentOp(op, w, c)
		if body == "" {
			return nil
		}
		bodies, refusal = []string{body}, c.refusal
		build = func(*smWriter, smCall) ([]string, string) { apply(); return bodies, "" }
	} else {
		// A call of several is made on a copy of the model first: a
		// refusal at a later operation leaves the model as it was.
		names := r.names
		r.m = m.scratch()
		bodies, refusal = build(&smWriter{by: 0, stamp: m.seq + 1}, *c)
		r.m, r.names = m, names
		if len(bodies) < 2 {
			return nil
		}
		r.counts[fmt.Sprintf("agent call of several: %d operations", len(bodies))]++
		if refusal == "" && strings.Contains(bodies[0], `"rename_tab"`) && strings.Contains(bodies[1], `"insert_`) {
			r.counts["agent call of several: a rename, then an insert under the new name, applied"]++
		}
	}
	body := strings.Join(bodies, ",")
	call := fmt.Sprintf(`{"ops":[%s]%s}`, body, guard)
	if token != "" {
		call = fmt.Sprintf(`{"state":%q,"ops":[%s]%s}`, token, body, guard)
	}
	r.counts["agent call: "+map[bool]string{true: "applied", false: refusal}[refusal == ""]]++
	_, err := r.e.agent(call)
	if refusal != "" {
		werr := refusalOf(err)
		if got := werr.Code + " " + werr.reason(); err == nil || got != refusal {
			return fmt.Errorf("the call %s was answered with %v, the model says %s", call, err, refusal)
		}
		return r.compare()
	}
	if err != nil {
		return fmt.Errorf("the call %s was refused: %v", call, err)
	}
	if again, _ := build(w, *c); !slices.Equal(again, bodies) {
		return fmt.Errorf("the model made the call %s as %s the second time", call, strings.Join(again, ","))
	}
	if advanced := r.e.seq() != m.seq; w.changed && !advanced {
		return fmt.Errorf("the call %s took no sequence, the model says it changes the sheet", call)
	} else if w.changed || advanced {
		m.seq++
	}
	if err := r.compare(); err != nil {
		return err
	}
	return r.revision()
}

// agentOp makes one operation of an agent call on the sheet as it is: its
// JSON and what it does to the model. An operation that the sheet has no
// room for has no JSON and is left out.
func (r *smRunner) agentOp(op smOp, w *smWriter, c *smCall) (body string, apply func()) {
	m := r.m
	kind := strings.TrimPrefix(op.Kind, "agent ")
	ti, mt, _ := r.tabOf(op)
	// With an old state the agent names the tab as it read it.
	name := mt.name
	if c.names != nil {
		name = c.names[op.Tab%len(c.names)]
		ti = m.byName(name)
	}
	check := func(address bool) { c.check(m, ti, address) }

	a1 := formula.Pos{Row: int32(op.R), Col: int32(op.C)}.A1()
	rowsIn := func() int { return len(m.tabs[ti].grid) }
	if kind == "fill" {
		// The whole edited area gets values to sort and to read.
		op.R, op.C, a1 = 0, 0, "A1"
	}
	if ti >= 0 && (len(m.tabs[ti].grid) < smAreaRows+4 || m.tabs[ti].cols < smAreaCols+2) {
		return "", nil // the tab has become smaller than the edited area
	}
	switch kind {
	case "write", "fill":
		// Two rows of two cells: a value, a formula, and a blank.
		texts := [][]string{{op.Text, fmt.Sprintf("=%s+1", formula.Pos{Row: int32(op.N % smAreaRows), Col: int32(op.M % smAreaCols)}.A1())}, {"", smTexts[op.N%len(smTexts)]}}
		if kind == "fill" {
			texts = make([][]string, smAreaRows-2)
			for i := range texts {
				texts[i] = make([]string, smAreaCols-2)
				for j := range texts[i] {
					texts[i][j] = fmt.Sprint((i*7 + j*3 + op.N) % 5)
					if (i+j+op.M)%7 == 0 {
						texts[i][j] = smTexts[(i+op.M)%len(smTexts)]
					}
				}
			}
		}
		if ti >= 0 && (op.R+len(texts) > rowsIn() || op.C+len(texts[0]) > m.tabs[ti].cols) {
			return "", nil // the write would grow the tab
		}
		var lines []string
		for _, line := range texts {
			var cells []string
			for _, text := range line {
				data, _ := json.Marshal(text)
				if text == "" {
					data = []byte("null")
				}
				cells = append(cells, string(data))
			}
			lines = append(lines, "["+strings.Join(cells, ",")+"]")
		}
		body = fmt.Sprintf(`{"op":"write","tab":%q,"at":%q,"rows":[%s]}`, name, a1, strings.Join(lines, ","))
		check(true)
		apply = func() {
			for i, line := range texts {
				for j, text := range line {
					row, col := m.meta[ti].rows[op.R+i], m.meta[ti].cols[op.C+j]
					if text != "" {
						r.agentCell(w, ti, row, col, text)
					} else if m.tabs[ti].grid[op.R+i][op.C+j] != nil {
						m.writeContent(w, ti, row, col, formula.Content{}, nil)
					}
				}
			}
		}
	case "clear", "format":
		r2, c2 := op.R+op.N%3, op.C+op.M%2
		rng := a1 + ":" + formula.Pos{Row: int32(r2), Col: int32(c2)}.A1()
		f, set := smFormat(op)
		if kind == "format" {
			body = fmt.Sprintf(`{"op":"format","tab":%q,"ranges":[%q],"format":%s}`, name, rng, set)
		} else {
			body = fmt.Sprintf(`{"op":"clear","tab":%q,"ranges":[%q],"formats":%v}`, name, rng, op.Flag)
		}
		check(true)
		apply = func() {
			for row := op.R; row <= min(r2, rowsIn()-1); row++ {
				for col := op.C; col <= min(c2, m.tabs[ti].cols-1); col++ {
					cell := m.tabs[ti].grid[row][col]
					rid, cid := m.meta[ti].rows[row], m.meta[ti].cols[col]
					switch {
					case kind == "format":
						if cell != nil || f != (formula.Format{}) {
							m.writeProps(w, ti, rid, cid, op.Formats, f)
						}
					case cell != nil:
						m.writeContent(w, ti, rid, cid, formula.Content{}, nil)
						// With formats, every property of a cell that has
						// had a format written is reset.
						if op.Flag && (cell.format != (formula.Format{}) || cell.pstamp != [propCount]uint64{}) {
							m.writeProps(w, ti, rid, cid, 1<<propCount-1, formula.Format{})
						}
					}
				}
			}
		}
	case "widths":
		px := 40 + op.N%400
		letter := formula.ColumnName(int32(op.C))
		body = fmt.Sprintf(`{"op":"widths","tab":%q,"cols":"%s:%s","width":%d}`, name, letter, letter, px)
		check(true)
		apply = func() {
			if op.C < m.tabs[ti].cols {
				m.setWidth(w, ti, m.meta[ti].cols[op.C], px)
			}
		}
	case "insert", "delete":
		rows, count := op.M%3 != 0, 1+op.N%2
		at, where := op.C, strconv.Quote(formula.ColumnName(int32(op.C)))
		if rows {
			at, where = op.R, strconv.Itoa(op.R+1)
		}
		body = fmt.Sprintf(`{"op":"%s_%s","tab":%q,"at":%s,"count":%d}`, kind, map[bool]string{true: "rows", false: "cols"}[rows], name, where, count)
		check(true)
		apply = func() {
			if kind == "insert" {
				m.create(w, ti, rows, at, count)
				return
			}
			positions := make([]int, count)
			for i := range positions {
				positions[i] = at + i
			}
			m.remove(w, ti, rows, positions)
		}
	case "sort":
		from, to := op.R%4, op.R%4+1+op.N%5
		body = fmt.Sprintf(`{"op":"sort","tab":%q,"rows":"%d:%d","by":%q,"desc":%v}`, name, from+1, to+1, formula.ColumnName(int32(op.C)), op.Flag)
		check(true)
		apply = func() {
			var positions []int
			for p := from; p <= min(to, rowsIn()-1); p++ {
				positions = append(positions, p)
			}
			m.sortRows(w, ti, positions, op.C, op.Flag)
		}
	case "add_tab":
		if len(m.tabs) >= smMaxTabs {
			return "", nil
		}
		added := r.tabName(op)
		body = fmt.Sprintf(`{"op":"add_tab","name":%q}`, added)
		apply = func() { m.addTab(w, added, len(m.tabs)) }
	case "rename_tab":
		renamed := r.renamed(op, name)
		body = fmt.Sprintf(`{"op":"rename_tab","tab":%q,"name":%q}`, name, renamed)
		check(false)
		apply = func() { m.rename(w, ti, renamed) }
	case "move_tab":
		to := op.N % len(m.tabs)
		body = fmt.Sprintf(`{"op":"move_tab","tab":%q,"to":%d}`, name, to+1)
		check(false)
		apply = func() { m.moveTab(w, ti, to) }
	case "delete_tab":
		if len(m.tabs) < 2 {
			return "", nil
		}
		body = fmt.Sprintf(`{"op":"delete_tab","tab":%q}`, name)
		check(false)
		apply = func() { m.deleteTab(w, ti) }
	case "tab_settings":
		fr, fc := op.N%3, op.M%2
		body = fmt.Sprintf(`{"op":"tab_settings","tab":%q,"frozen_rows":%d,"frozen_cols":%d}`, name, fr, fc)
		check(false)
		apply = func() { m.setSettings(w, ti, fr, fc) }
	default:
		return "", nil
	}
	return body, apply
}

// refusalOf is refusal, for the places where a variable has that name.
func refusalOf(err error) *WriteError { return refusal(err) }

// agentCell writes one cell of an agent's write: a string is typed text,
// read at the cell.
func (r *smRunner) agentCell(w *smWriter, ti int, row, col uint64, text string) {
	m := r.m
	if strings.HasPrefix(text, "=") && len(text) > 1 {
		m.typed(w, ti, row, col, text)
		return
	}
	// An agent's string is text, not typed entry (rule 43): only the
	// apostrophe is taken off.
	text = strings.TrimPrefix(text, "'")
	m.writeContent(w, ti, row, col, formula.Content{Kind: formula.ContentText, Text: text}, nil)
}

// revision imports what the instance recorded and keeps the model's state
// for the newest revision, to roll back to.
func (r *smRunner) revision() error {
	n := r.e.b.importAll(r.t)
	if n > r.revs {
		r.snaps[n] = r.m.clone()
		r.revs = n
	}
	return nil
}

// ---- the other steps ----

func (r *smRunner) step(op smOp) error {
	r.counts["step: "+op.Kind]++
	if err := r.act(op); err != nil {
		return err
	}
	// Every screen follows through its deltas. Every tenth step, and at
	// the end, each is compared with a new snapshot. A view also moves to
	// another tab now and then.
	r.steps++
	if op.Kind == "restart" {
		for _, v := range r.views {
			v.c = nil
		}
	}
	if r.steps%17 == 0 {
		v := r.views[r.steps/17%len(r.views)]
		v.sc = nil
		r.watch(v, op.N)
	}
	return r.screens(r.steps%10 == 0)
}

func (r *smRunner) act(op smOp) error {
	switch {
	case op.Kind == "commit fails":
		return r.commitFails(op)
	case op.Kind == "quota":
		return r.quota(op)
	case op.Kind == "limit":
		return r.limit(op)
	case op.Kind == "recover":
		return r.recover(op)
	case op.Who != 0 && op.Kind != "resend":
		a := r.compose(op)
		if a == nil {
			return nil
		}
		a.s.queue = append(a.s.queue, a)
		if op.Hold {
			return nil
		}
		// The session's earlier actions go first, in order.
		for len(a.s.queue) > 0 {
			next := a.s.queue[0]
			a.s.queue = a.s.queue[1:]
			if err := r.deliver([]*smAction{next}); err != nil {
				return err
			}
		}
		return nil
	case strings.HasPrefix(op.Kind, "agent "):
		return r.agent(op)
	case op.Kind == "flush":
		return r.flush(op.Flag)
	case op.Kind == "read":
		names := make([]string, len(r.m.tabs))
		for i, t := range r.m.tabs {
			names[i] = t.name
		}
		r.saved = append(r.saved, smSaved{token: r.e.state(), seq: r.m.seq, names: names})
		return nil
	}
	// The steps below end what is in flight: every unsent action arrives
	// first.
	if err := r.flush(false); err != nil {
		return err
	}
	switch op.Kind {
	case "tick":
		// Half a minute without an edit closes the open edit groups as a
		// revision. The clock stays inside the day.
		if r.ticks++; r.ticks > 400 {
			return nil
		}
		r.e.clock.Add(31 * time.Second)
		r.m.now += 31
		r.e.tick()
		r.m.trimLog()
		if err := r.compare(); err != nil {
			return fmt.Errorf("after a tick: %w", err)
		}
		return r.revision()
	case "restart":
		before := r.e.dump()
		r.e.restart()
		r.m.clearLog(r.m.seq)
		if got := r.e.dump(); got != before {
			return fmt.Errorf("a restart changed the stored state:\n%s", dumpDiff(before, got))
		}
		// Undo history is gone, and a page that loads again has a new
		// session.
		resumed := r.sessions[op.N%len(r.sessions)]
		last, token, n := resumed.last, resumed.token, resumed.n
		r.connect()
		if last != nil {
			// One page comes back with its session and sends its last
			// action again: it gets the stored answer and nothing changes.
			session, err := r.e.in.resumeSession(context.Background(), token)
			if err != nil {
				return fmt.Errorf("resuming a session after a restart: %v", err)
			}
			resumed.p, resumed.token, resumed.n, resumed.last = &person{e: r.e, s: session}, token, n, last
			if err := r.resend(resumed); err != nil {
				return err
			}
		}
		return r.compare()
	case "resend":
		return r.resend(r.sessions[(op.Who-1)%len(r.sessions)])
	case "rollback":
		return r.rollback(op)
	case "collect":
		return r.collect(op)
	}
	return nil
}

// collect makes the maintenance tick remove the older half of the blank
// records without a format (section 7.5), by setting the number above which
// it does so just under the number the sheet holds. The model says which
// records go, keeps their stamps as the engine does for the undo entries
// that can ask for them (Appendix B, engine item 19), and raises the guard
// floor to the newest stamp among them.
//
// Records with the same stamp are equal to the tick. When the half ends
// inside such a group, the model takes from the engine which of the group
// went and checks how many did.
func (r *smRunner) collect(op smOp) error {
	m := r.m
	type blank struct {
		ti, r, c int
		stamp    uint64
	}
	var all []blank
	for ti, mt := range m.tabs {
		for row, line := range mt.grid {
			for col, cell := range line {
				if cell != nil && cell.content.Kind == formula.ContentBlank && cell.format == (formula.Format{}) {
					all = append(all, blank{ti, row, col, max(cell.cstamp, cell.tstamp)})
				}
			}
		}
	}
	if r.ticks++; len(all) < 2 || r.ticks > 400 {
		return nil
	}
	slices.SortStableFunc(all, func(a, b blank) int { return cmp.Compare(a.stamp, b.stamp) })
	n := len(all) / 2
	floor := all[n-1].stamp
	r.e.in.mu.Lock()
	r.e.in.wb.collected.limit = len(all) - 1
	r.e.in.mu.Unlock()
	r.e.clock.Add(31 * time.Second)
	m.now += 31
	r.e.tick()
	m.trimLog()
	r.e.in.mu.Lock()
	r.e.in.wb.collected.limit = 0
	gone := func(b blank) bool {
		return r.e.in.wb.tabs[b.ti].cellAt(formula.Pos{Row: int32(b.r), Col: int32(b.c)}) == nil
	}
	var went []blank
	tied := 0
	for i, b := range all {
		switch {
		case b.stamp < floor:
			went = append(went, b)
		case b.stamp == floor && all[n].stamp != floor:
			went = append(went, b) // the whole group is in the older half
		case b.stamp == floor:
			if tied++; gone(b) {
				went = append(went, b)
			}
		}
		_ = i
	}
	r.e.in.mu.Unlock()
	if len(went) != n {
		return fmt.Errorf("the tick removed %d of %d blank records, the model says %d", len(went), len(all), n)
	}
	if tied > 0 {
		r.adopted["which blank records of one stamp the tick removed"]++
	}
	for _, b := range went {
		st, mt := m.meta[b.ti], m.tabs[b.ti]
		m.collected[[3]uint64{st.id, st.rows[b.r], st.cols[b.c]}] = *mt.grid[b.r][b.c]
		mt.grid[b.r][b.c] = nil
	}
	m.guardFloor = max(m.guardFloor, floor)
	r.counts["collection of blank records"]++
	if err := r.compare(); err != nil {
		return fmt.Errorf("after a tick that collects blank records: %w", err)
	}
	return r.revision()
}

// commitFails sends an action whose commit is made to fail. The action is
// answered as unavailable and leaves nothing behind: not in sheet.db, not
// in the workbook in memory, not on the session's undo stack. The page
// sends it again, as it does after that answer, and it applies as it would
// have. An action that takes no sequence commits nothing and is answered
// the first time.
func (r *smRunner) commitFails(op smOp) error {
	if err := r.flush(false); err != nil {
		return err
	}
	sub := op
	sub.Kind = []string{"set", "formula", "insert", "delete", "format", "sort", "undo", "rename_tab", "redo", "clear"}[op.N%10]
	sub.Hold = false
	a := r.compose(sub)
	if a == nil {
		return nil
	}
	m, s := r.m, a.s
	before, seq := r.e.dump(), m.seq
	restore := r.e.failCommits(1)
	ack, err := s.p.send(a.n, a.base, a.ops...)
	restore()
	failed := err != nil
	if failed {
		if werr := refusal(err); werr == nil || werr.Code != "unavailable" || werr.Reason != "commit" {
			return fmt.Errorf("%s, with a commit that fails, was answered with %v", a.desc, err)
		}
		if got := r.e.dump(); got != before {
			return fmt.Errorf("the failed commit of %s changed the stored state:\n%s", a.desc, dumpDiff(before, got))
		}
		if err := r.compare(); err != nil {
			return fmt.Errorf("after the failed commit of %s: %w", a.desc, err)
		}
		if ack, err = s.p.send(a.n, a.base, a.ops...); err != nil {
			return fmt.Errorf("%s, sent again after a failed commit: %v", a.desc, err)
		}
	}
	if err := r.arrived(a, ack, true); err != nil {
		return err
	}
	if m.seq != seq && !failed {
		return fmt.Errorf("%s took a sequence in a commit that was made to fail", a.desc)
	}
	r.counts[fmt.Sprintf("commit fails: the action was refused and applied when sent again: %v", failed)]++
	if err := r.compare(); err != nil {
		return err
	}
	return r.resend(s)
}

// recover brings a tab of a revision back for a person (section 16.4):
// a tab that is live, renamed or deleted since, from any revision the
// model kept.
func (r *smRunner) recover(op smOp) error {
	if err := r.flush(false); err != nil {
		return err
	}
	m := r.m
	if len(m.tabs) > smMaxTabs {
		return nil
	}
	var known []int
	for n := range r.snaps {
		known = append(known, n)
	}
	slices.Sort(known)
	n := known[op.N%len(known)]
	if op.Flag {
		n = known[len(known)-1]
	}
	snap := r.snaps[n]
	i := op.Tab % len(snap.tabs)
	s := r.sessions[(op.Who-1)%len(r.sessions)]
	source := snap.meta[i].id
	state := "live"
	if oi := m.index(source); oi < 0 {
		state = "deleted"
	} else if m.tabs[oi].name != snap.tabs[i].name {
		state = "renamed"
	}
	got, err := s.p.recover(n, TabID(source))
	if err != nil {
		return fmt.Errorf("recovering tab %d of revision %d: %v", source, n, err)
	}
	w := &smWriter{by: s.ord, stamp: m.seq + 1, rec: &smEntry{}, link: s.link}
	want := m.recoverTab(w, snap, i)
	m.seq++
	if uint64(got) != want {
		return fmt.Errorf("the recovery of tab %d of revision %d made tab %d, the model says %d", source, n, got, want)
	}
	s.undo, s.redo = append(s.undo, w.rec), nil
	r.counts["recovery of a tab that is "+state]++
	return r.compare()
}

// quota fills the flat's disk quota. An edit of a person and a call of the
// agent that would change the sheet are refused and change nothing, and
// the person's action is refused again when it is sent again at the full
// quota. Its receipt has no room in sheet.db, so nothing of it is stored
// and its number stays free: the session's last action stays the one
// before it.
func (r *smRunner) quota(op smOp) error {
	if err := r.flush(false); err != nil {
		return err
	}
	text := fmt.Sprint("quota", r.steps) // a value no cell holds
	sub := smOp{Kind: []string{"set", "insert"}[op.M%2], Who: 1 + op.N%smPeople, Tab: op.Tab, R: op.R, C: op.C, N: op.N, M: 1, Text: text}
	a := r.compose(sub)
	if a == nil {
		return nil
	}
	ctx := context.Background()
	r.e.b.setQuota(1)
	defer r.e.b.setQuota(0)
	if err := r.e.in.Remeasure(ctx); err != nil {
		return err
	}
	before := r.e.dump()
	ack, err := a.s.p.send(a.n, a.base, a.ops...)
	if err != nil || status(ack) != "refused:disk_quota" {
		return fmt.Errorf("%s at a full quota was answered with %v %q, want refused:disk_quota", a.desc, err, status(ack))
	}
	data, _ := json.Marshal(ack)
	last := a.s.last
	a.ack, a.s.last = string(data), a
	defer func() { a.s.last = last }()
	_, mt, _ := r.tabOf(op)
	call := fmt.Sprintf(`{"ops":[{"op":"write","tab":%q,"at":"A1","rows":[[%q]]}]}`, mt.name, text)
	if _, err := r.e.agent(call); err == nil || refusal(err).Code != "sheet_capacity" || refusal(err).Reason != "disk_quota" {
		return fmt.Errorf("the call %s at a full quota was answered with %v", call, err)
	}
	if got := r.e.dump(); got != before {
		return fmt.Errorf("edits refused for the quota changed the stored state:\n%s", dumpDiff(before, got))
	}
	if err := r.compare(); err != nil {
		return fmt.Errorf("after edits at a full quota: %w", err)
	}
	if err := r.resend(a.s); err != nil {
		return err
	}
	r.e.b.setQuota(0)
	if err := r.e.in.Remeasure(ctx); err != nil {
		return err
	}
	r.counts["quota: an action and a call refused"]++
	return nil
}

// limit sends an edit that a limit of the sheet refuses (section 11): more
// columns or rows than a tab holds, a text that is too long, or a rename
// that makes a formula longer than a formula may be. It is refused with
// the limit's name and changes nothing.
func (r *smRunner) limit(op smOp) error {
	if err := r.flush(false); err != nil {
		return err
	}
	m := r.m
	ti, mt, st := r.tabOf(op)
	before, seq := r.e.dump(), r.e.seq()
	which := []string{"columns", "rows", "text_length", "formula_length"}[op.N%4]
	person := func(body string) error {
		s := r.sessions[op.M%len(r.sessions)]
		s.n++
		a := &smAction{s: s, n: s.n, base: m.seq, ops: []string{body}, desc: "an edit over the limit " + which}
		ack, err := s.p.send(a.n, a.base, a.ops...)
		if err != nil || len(ack.Results) != 1 || ack.Results[0].Status != "refused" || ack.Results[0].Reason != which {
			return fmt.Errorf("%s (%.80s) was answered with %v %+v", a.desc, body, err, ack.Results)
		}
		data, _ := json.Marshal(ack)
		a.ack, s.last = string(data), a
		return nil
	}
	var err error
	switch which {
	case "columns":
		err = person(fmt.Sprintf(`{"op":"append_cols","tab":%d,"count":%d}`, st.id, formula.MaxCols-len(st.cols)+1))
	case "rows":
		err = person(fmt.Sprintf(`{"op":"insert_rows","tab":%d,"beside":%d,"side":"before","count":%d}`, st.id, st.rows[0], formula.MaxRows-len(st.rows)+1))
	case "text_length":
		text, _ := json.Marshal(strings.Repeat("t", formula.MaxTextLen+1))
		err = person(fmt.Sprintf(`{"op":"set","tab":%d,"row":%d,"col":%d,"text":%s}`, st.id, st.rows[0], st.cols[0], text))
	case "formula_length":
		// The agent writes a formula that names its tab often and is just
		// short enough. A longer name for the tab would take it over the
		// limit, so the rename is refused. The agent clears the cell
		// again, so that no later edit meets the long formula.
		if len(mt.grid) < smAreaRows+4 || mt.cols < smAreaCols+2 || mt.grid[0][0] != nil && mt.grid[0][0].f != nil {
			return nil
		}
		ref := "+" + quoteTab(mt.name) + "!B2"
		text := "=1" + strings.Repeat(ref, (formula.MaxFormulaChars-2)/len(ref))
		w := &smWriter{by: 0, stamp: m.seq + 1}
		data, _ := json.Marshal(text)
		if _, err := r.e.agent(fmt.Sprintf(`{"ops":[{"op":"write","tab":%q,"at":"A1","rows":[[%s]]}]}`, mt.name, data)); err != nil {
			return fmt.Errorf("a formula of %d characters was refused: %v", len(text), err)
		}
		r.agentCell(w, ti, st.rows[0], st.cols[0], text)
		m.seq++
		if err := r.compare(); err != nil {
			return err
		}
		before, seq = r.e.dump(), r.e.seq()
		longer := mt.name + "LongerByALot"
		_, rerr := r.e.agent(fmt.Sprintf(`{"ops":[{"op":"rename_tab","tab":%q,"name":%q}]}`, mt.name, longer))
		if werr := refusal(rerr); rerr == nil || werr.Code != "sheet_capacity" || werr.Limit != which {
			return fmt.Errorf("a rename that takes a formula of %d characters over the limit was answered with %v", len(text), rerr)
		}
		if got := r.e.dump(); got != before || r.e.seq() != seq {
			return fmt.Errorf("the refused rename changed the stored state:\n%s", dumpDiff(before, got))
		}
		if _, err := r.e.agent(fmt.Sprintf(`{"ops":[{"op":"clear","tab":%q,"ranges":["A1"]}]}`, mt.name)); err != nil {
			return err
		}
		m.writeContent(&smWriter{by: 0, stamp: m.seq + 1}, ti, st.rows[0], st.cols[0], formula.Content{}, nil)
		m.seq++
		r.counts["limit: "+which]++
		if err := r.compare(); err != nil {
			return err
		}
		return r.revision()
	}
	if err != nil {
		return err
	}
	if got := r.e.dump(); got != before || r.e.seq() != seq {
		return fmt.Errorf("an edit over the limit %s changed the stored state:\n%s", which, dumpDiff(before, got))
	}
	r.counts["limit: "+which]++
	return r.compare()
}

// quoteTab writes a tab name as a formula names it.
func quoteTab(name string) string {
	if strings.ContainsAny(name, " '!") {
		return "'" + strings.ReplaceAll(name, "'", "''") + "'"
	}
	return name
}

// resend sends the last action of a session again, as a page does when it
// reconnects without having seen the ack.
func (r *smRunner) resend(s *smSession) error {
	a := s.last
	if a == nil {
		return nil
	}
	before := r.e.dump()
	ack, err := s.p.send(a.n, a.base, a.ops...)
	if err != nil {
		return fmt.Errorf("sending %s again: %v", a.desc, err)
	}
	if data, _ := json.Marshal(ack); string(data) != a.ack {
		return fmt.Errorf("%s, sent again, was answered with %s, the first time with %s", a.desc, data, a.ack)
	}
	if got := r.e.dump(); got != before {
		return fmt.Errorf("%s, sent again, changed the sheet:\n%s", a.desc, dumpDiff(before, got))
	}
	return nil
}

// rollback returns the sheet to a revision the model kept (section 16.3):
// the content of the revision, with ids that are live keeping their born
// stamp and every other stamp becoming the rollback's sequence.
func (r *smRunner) rollback(op smOp) error {
	var known []int
	for n := range r.snaps {
		known = append(known, n)
	}
	slices.Sort(known)
	n := known[op.N%len(known)]
	if _, err := r.e.b.Rollback(context.Background(), n, "P1"); err != nil {
		return fmt.Errorf("rollback to revision %d: %v", n, err)
	}
	old := r.m
	m := r.snaps[n].clone()
	stamp := old.seq + 1
	m.seq, m.resetSeq, m.tabOrderStamp, m.tabChangeStamp = stamp, stamp, stamp, stamp
	m.nextTab, m.nextRow, m.nextCol = old.nextTab, old.nextRow, old.nextCol
	m.rowMark, m.colMark, m.tabMark, m.lastSort = old.rowMark, old.colMark, old.tabMark, old.lastSort
	m.now = old.now
	m.clearLog(stamp)
	m.guardFloor, m.collected = old.guardFloor, map[[3]uint64]modelCell{}
	m.renames = old.renames
	m.worlds = nil
	for i, st := range m.meta {
		oi := old.index(st.id)
		keep := func(rows bool) {
			born := st.bornOf(rows)
			for _, id := range st.axis(rows) {
				born[id] = stamp
				if oi >= 0 {
					if was, ok := old.meta[oi].bornOf(rows)[id]; ok {
						born[id] = was
					}
				}
			}
		}
		st.born = stamp
		if oi >= 0 {
			st.born = old.meta[oi].born
		}
		keep(true)
		keep(false)
		revived(m.tabs[i], st, stamp)
	}
	m.paths = r.counts
	r.m = m
	for _, s := range r.sessions {
		s.undo, s.redo, s.link = nil, nil, map[smLink]uint64{}
	}
	r.counts["rollback"]++
	if err := r.compare(); err != nil {
		return fmt.Errorf("after the rollback to revision %d: %w", n, err)
	}
	return r.revision()
}

// runHistory plays a history on a new sheet. It returns the number of the
// step that failed, or -1. A panic of the engine or of the model fails its
// step like a wrong answer, so that the seed is reported and the history
// can be shortened.
func runHistory(t *testing.T, history []smOp) (r *smRunner, failed int, err error) {
	r = newSMRunner(t)
	defer r.e.in.Stop()
	at := 0
	defer func() {
		if p := recover(); p != nil {
			failed, err = at, fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	if err := r.compare(); err != nil {
		return r, 0, fmt.Errorf("a new sheet: %w", err)
	}
	for i, op := range history {
		at = i
		if err := r.step(op); err != nil {
			return r, i, err
		}
	}
	at = len(history) - 1
	if err := r.flush(false); err != nil {
		return r, len(history) - 1, err
	}
	if err := r.screens(true); err != nil {
		return r, len(history) - 1, err
	}
	// A restart at the end finds the same sheet.
	r.e.restart()
	if err := r.compare(); err != nil {
		return r, len(history) - 1, fmt.Errorf("after the final restart: %w", err)
	}
	return r, -1, nil
}

// minimize takes steps out of a failing history while it still fails, in
// stretches that halve down to single steps, and returns what is left.
func minimize(t *testing.T, history []smOp, budget time.Duration) []smOp {
	deadline := time.Now().Add(budget)
	for size := max(1, len(history)/4); size >= 1; size /= 2 {
		for at := len(history) - 1 - size; at >= 0 && time.Now().Before(deadline); at -= size {
			shorter := slices.Delete(slices.Clone(history), at, at+size)
			if _, failed, err := runHistory(t, shorter); err != nil {
				history = shorter[:failed+1]
				at = min(at, len(history)-1)
			}
		}
	}
	return history
}

// TestStateMachine plays random histories on the engine and on the
// reference model of statemodel_test.go and compares them after every
// step: content, calculated values, formats, rows and columns with their
// ids, widths, settings, the tab list, and every stamp of section 4.1.
//
// A history mixes three people's sessions and an agent. People's actions
// name ids and can stay unsent while others edit, so that they arrive with
// an old base, one by one or several behind a held writer, where they can
// share a transaction. The agent calls with the current state or one it
// read earlier, with and without guards, with one operation or several,
// and the model predicts each refusal. Undo and redo go any number of
// actions back, by whoever, and the model predicts which steps apply,
// which are skipped and why. Ticks close revisions, rollbacks return to
// them, tabs come back from them, restarts reopen sheet.db in the middle,
// and a session sends an acknowledged action again.
//
// Three connections, one of them Public, follow the sheet through their
// deltas alone and are compared with the snapshot of a new connection
// every tenth step. Every third history keeps each undo payload in a
// scratch file.
//
// After every step the comparison also checks what must hold of any
// workbook (wholeWorkbook): among others that an id is in its axis once
// and that no two tabs share an id.
//
// What the model computes by itself, and the engine has to match:
//
//   - Whether an action changes the sheet and takes a sequence, each
//     refusal of an agent call (sections 7.3 to 7.5, the guard floor
//     included), and the counts and the first reason of every undo and
//     redo, also of one that applies nothing.
//   - The text of every formula an undo writes: the value before an edit,
//     and the formulas of rows, columns and tabs that return. The model
//     follows them through the inserts, deletions, sorts and renames
//     since by replaying those edits on its own lists of ids
//     (stateworld_test.go), for the entries that depend on the edited tab
//     as Appendix B, engine item 23 says. It calls the formula package to
//     rewrite one formula for one edit and nothing of the engine's undo
//     code.
//   - The place of rows and columns an undo brings back, from the same
//     lists of ids, and the place of an insert that arrives after the row
//     it names was deleted, from the model's own log of structural edits
//     (Appendix B, engine item 17).
//   - The counters the limits of section 11 are checked against: cell
//     records, blank records, formulas, the characters of typed text and
//     of formula text, the cells formulas read, and the characters of
//     calculated text. The model counts them in its own grids after every
//     step.
//   - Tab recovery (section 16.4): the id and the name of the tab, its
//     stamps, that entries from before the recovery skip the tab, and that
//     they apply to it again when the recovery is undone and the tab that
//     was deleted before it comes back (Appendix B, engine item 26).
//   - The blank records a tick collects, the guard floor, and what an
//     undo and a later write find at a cell whose record is gone
//     (section 7.5; Appendix B, engine item 19).
//   - That an action whose commit fails leaves nothing and applies when
//     it is sent again, that an edit at a full disk quota is refused and
//     stays refused, and that an edit over a limit of section 11 is
//     refused with the limit's name: columns, rows, text length, and the
//     formula length after a rename.
//   - That no undo entry expires. A history of the test is far below
//     every limit of undo history (product rule 23, last clause), so the
//     comparison after each step fails when the engine has dropped an
//     entry or marked one as lost, and an undo that answers lost fails
//     like any other answer the model did not give.
//
// What the model takes from the engine (adopt), and nothing else. A value
// that is taken is not verified:
//
//	outcome                                          design      Appendix B
//	which blank records of one stamp a tick removes  section 7.5 engine 19
//	  when the older half ends inside the group
//
// That is a free choice: the records of one stamp are equal to the rule,
// which names the older half. The model checks how many the tick removed.
//
// The generator draws most steps one by one. One draw in four starts a
// short script instead (smGen.script): an overwritten formula or deleted
// rows with a formula in them, structural edits of others in between, and
// the undo; a recovery of a tab with retained entries on it; a cleared
// cell whose blank record is collected before the undo; a tab that takes
// a name formulas hold, a new tab that takes the name of a deleted one,
// and a tab that is renamed to its own name, in other capitals or to a
// name no formula holds while an entry keeps a deletion of it; an insert
// that arrives after its row was deleted. The summary of a
// run reports how often each path was taken, as a count and per 1,000
// histories.
//
// Checked by hand, each with an engine changed for the purpose, over the
// seeds from 1. The number is the first seed that fails:
//
//	the engine drops a write of a new value                          1
//	it does not follow an inserted row in a formula an entry keeps   1
//	it gives a kept formula #REF! where the deleted rows are back    1
//	it ignores that another writer wrote into inserted rows          3
//	it undoes an entry from before a recovery in the recovered tab   15
//	it restores over a blank that another writer left and a tick     2
//	  collected
//	it brings rows back at the top when the row above them is gone   49
//	it applies a kept deletion under the name its tab took later     2
//	it applies a kept deletion when its tab is renamed to the name   13
//	  it has, as it is or in other capitals
//	it counts a cell record too many                                 1
//	it counts a blank record too many                                1
//	it counts a formula too many                                     1
//	it counts a character of typed text too many                     1
//	it counts a character of formula text too many                   1
//	it counts a read cell too many                                   1
//	it counts a character of calculated text too many                2
//
// TestStateMachineFollowsKeptFormula fails the second of these as well,
// TestStateMachinePlacesReturningRows the seventh and
// TestStateMachineKeepsDeletionThroughRenames the ninth.
//
// The generator leaves out copy, fill and cut, a paste of a rectangle, a
// formula in flight across a restart or a rollback, an action that mixes
// cell edits with a row, column or tab operation (the engine refuses it),
// formulas near the length limit that an undo would bring back, the cell
// and sheet text limits, which need a large sheet, limited mode, an
// action that runs out of its time or of the bound on what one action may
// change (an answer of that kind fails the test), and everything that
// makes an undo entry expire: the bounds of catch-up work and of waiting
// edits, the bound of the store of collected stamps, and the budgets of
// payloads and stand-in notes. An entry of the test holds a few formulas
// of under a hundred characters and follows a few dozen edits, against
// bounds of megabytes and of two million units of work. Fixed tests cover
// those. The formulas' values are calculated by the formula package on
// both sides, so the test compares the engine's bookkeeping of them, not
// the functions.
//
// The default run plays a few seeds. -sheet.seeds=N or FLATS_SHEET_SEEDS=N
// plays N, from seed 1 or from FLATS_SHEET_FROM, FLATS_SHEET_SEED=K plays
// seed K alone, and FLATS_SHEET_STEPS sets the length of a history. A
// failure prints the seed and a history with every step taken out that the
// failure does not need, as far as 30 seconds, or FLATS_SHEET_MINIMIZE
// seconds, allow. With FLATS_SHEET_PRINT=json it also prints that history
// as JSON, which FLATS_SHEET_HISTORY=<file> plays again.
func TestStateMachine(t *testing.T) {
	seeds, steps := *seedCount, 90
	first := int64(1)
	if v, err := strconv.Atoi(os.Getenv("FLATS_SHEET_SEEDS")); err == nil {
		seeds = v
	}
	if v, err := strconv.Atoi(os.Getenv("FLATS_SHEET_STEPS")); err == nil {
		steps = v
	}
	if v, err := strconv.ParseInt(os.Getenv("FLATS_SHEET_FROM"), 10, 64); err == nil {
		first = v
	}
	if v, err := strconv.ParseInt(os.Getenv("FLATS_SHEET_SEED"), 10, 64); err == nil {
		first, seeds = v, 1
	}
	few := testing.Short() || raceEnabled
	if few {
		seeds = min(seeds, 2)
	}
	defer func() { undoSpillAll = false }()
	adopted, counts := map[string]int{}, map[string]int{}
	if path := os.Getenv("FLATS_SHEET_HISTORY"); path != "" {
		// A history a failure printed with FLATS_SHEET_PRINT=json.
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var history []smOp
		if err := json.Unmarshal(data, &history); err != nil {
			t.Fatal(err)
		}
		if _, failed, err := runHistory(t, history); err != nil {
			t.Fatalf("step %d, %s: %v", failed+1, history[failed], err)
		}
		return
	}
	for seed := first; seed < first+int64(seeds); seed++ {
		// Every third history keeps each undo payload in a scratch file. A
		// short or race run plays two histories, and its last one does.
		undoSpillAll = seed%3 == 0 || (few && seeds > 1 && seed == first+int64(seeds)-1)
		gen := &smGen{rng: rand.New(rand.NewSource(seed))}
		history := make([]smOp, steps)
		for i := range history {
			history[i] = gen.next()
		}
		// A sheet with values in it from the start.
		history[0] = smOp{Kind: "agent fill", N: int(seed), M: int(seed / 7)}
		r, failed, err := runHistory(t, history)
		for name, n := range r.adopted {
			adopted[name] += n
		}
		for name, n := range r.counts {
			if name == smMostGaps {
				counts[name] = max(counts[name], n)
				continue
			}
			counts[name] += n
		}
		if err == nil {
			continue
		}
		budget := 30 * time.Second
		if v, err := strconv.Atoi(os.Getenv("FLATS_SHEET_MINIMIZE")); err == nil {
			budget = time.Duration(v) * time.Second
		}
		short := minimize(t, history[:failed+1], budget)
		_, _, again := runHistory(t, short)
		lines := make([]string, len(short))
		for i, op := range short {
			lines[i] = fmt.Sprintf("%3d  %s", i+1, op)
		}
		if os.Getenv("FLATS_SHEET_PRINT") == "json" {
			data, _ := json.Marshal(short)
			lines = append(lines, string(data))
		}
		t.Fatalf("seed %d, step %d of %d: %v\n\nthe shortest history found that still fails, with %v:\n%s", seed, failed+1, steps, err, again, strings.Join(lines, "\n"))
	}
	report := func(title string, m map[string]int) {
		names := make([]string, 0, len(m))
		for name := range m {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if name == smMostGaps {
				t.Logf("%s: %s: %d", title, name, m[name])
				continue
			}
			t.Logf("%s: %s: %d, %d in 1,000 histories", title, name, m[name], m[name]*1000/seeds)
		}
	}
	t.Logf("%d histories of %d steps", seeds, steps)
	report("ran", counts)
	report("taken from the engine", adopted)
}

// TestStateMachineFollowsKeptFormula plays a fixed history on the engine
// and the model: C3 holds a formula on A1, a person replaces it with a
// number, another person inserts a row above row 2, between the two, and
// the first takes the replacement back. The formula returns in C4 and
// still reads A1. The model follows the formula by itself, so an engine
// that follows it wrongly fails here whatever the random histories reach:
// one that leaves the kept formula alone brings back a formula on A2.
func TestStateMachineFollowsKeptFormula(t *testing.T) {
	history := []smOp{
		{Kind: "set", Who: 1, R: 0, C: 0, Text: "5"},
		{Kind: "formula", Who: 1, R: 2, C: 2}, // =A1+A1
		{Kind: "set", Who: 1, R: 2, C: 2, Text: "0"},
		{Kind: "insert", Who: 2, R: 1, M: 1}, // one row above row 2
		{Kind: "undo", Who: 1},
	}
	r, failed, err := runHistory(t, history)
	if err != nil {
		t.Fatalf("step %d, %s: %v", failed+1, history[failed], err)
	}
	if got := r.counts["kept formulas the model followed by itself"]; got != 1 {
		t.Fatalf("the model followed %d kept formulas by itself, want 1", got)
	}
	for name, n := range r.adopted {
		if strings.HasPrefix(name, "formula text") && n > 0 {
			t.Fatalf("the model took a formula text from the engine: %s", name)
		}
	}
	mc := r.m.tabs[0].grid[3][2]
	if mc == nil || mc.f == nil || mc.f.Text(formula.Pos{Row: 3, Col: 2}) != "=A1+A1" {
		t.Fatalf("the model's C4 is %+v, want the formula =A1+A1", mc)
	}
}

// TestStateMachineKeepsDeletionThroughRenames plays a fixed history on the
// engine and the model: A10 holds a formula that reads rows 1 to 7 of its
// tab by the tab's name, a person clears it, another deletes row 1, a third
// renames the tab, the second takes the deletion back and the first the
// clear. The rename is to the name the tab has, which changes nothing and
// reaches no entry, to that name in other capitals, or to a name no
// formula holds. None of them gives a reference another tab to read, so
// the deletion stays one that can be taken back, and the formula returns
// reading rows 1 to 7. The model decides that from
// the references themselves, so an engine that applies the deletion at
// such a rename fails here whatever the random histories reach.
func TestStateMachineKeepsDeletionThroughRenames(t *testing.T) {
	const stayed = "a kept deletion that stayed through a rename to the same name, or in other capitals"
	for _, c := range []struct {
		what   string
		n      int // of the rename (smRunner.renamed)
		want   string
		stayed int
	}{
		{"to the same name", 1, "=MAX(Sheet1!A7:C1)", 0},
		{"in other capitals", 5, "=MAX(SHEET1!A7:C1)", 1},
		{"to a name no formula holds", 2, "=MAX(Page1!A7:C1)", 0},
	} {
		t.Run(c.what, func(t *testing.T) {
			history := []smOp{
				{Kind: "formula", Who: 1, R: 9, C: 0, N: 42, M: 2}, // =MAX(Sheet1!A7:C1)
				{Kind: "clear", Who: 1, R: 9, C: 0},
				{Kind: "delete", Who: 3, R: 0, M: 1}, // row 1
				{Kind: "rename_tab", Who: 2, N: c.n},
				{Kind: "undo", Who: 3},
				{Kind: "undo", Who: 1},
			}
			r, failed, err := runHistory(t, history)
			if err != nil {
				t.Fatalf("step %d, %s: %v", failed+1, history[failed], err)
			}
			mc := r.m.tabs[0].grid[9][0]
			if mc == nil || mc.f == nil || mc.f.Text(formula.Pos{Row: 9}) != c.want {
				t.Fatalf("the model's A10 is %+v, want the formula %s", mc, c.want)
			}
			if got := r.counts[stayed]; got != c.stayed {
				t.Fatalf("the model kept %d deletions through a rename that changes no name, want %d", got, c.stayed)
			}
			if got := r.counts["a kept formula written by an undo after a deletion that was taken back"]; got != 1 {
				t.Fatalf("the model wrote %d kept formulas after a deletion that was taken back, want 1", got)
			}
		})
	}
}

// TestStateMachinePlacesReturningRows plays a fixed history on the engine
// and the model: of the rows 1 to 4, one person deletes row 3, another
// deletes row 2, and the first takes the deletion back. Row 2, which was
// above row 3, is gone, so row 3 returns at the place the entry tracked
// for it: between rows 1 and 4. The model works that place out from its
// own lists of ids, so an engine that puts the row anywhere else fails
// here whatever the random histories reach.
func TestStateMachinePlacesReturningRows(t *testing.T) {
	history := []smOp{
		{Kind: "delete", Who: 1, R: 2, M: 1},
		{Kind: "delete", Who: 2, R: 1, M: 1},
		{Kind: "undo", Who: 1},
	}
	r, failed, err := runHistory(t, history)
	if err != nil {
		t.Fatalf("step %d, %s: %v", failed+1, history[failed], err)
	}
	if got := r.m.meta[0].rows[:3]; !slices.Equal(got, []uint64{1, 3, 4}) {
		t.Fatalf("the model's rows begin %v, want 1, 3, 4", got)
	}
	if got := r.counts["placement: at the tracked place, the upper neighbour gone"]; got != 1 {
		t.Fatalf("the model placed %d runs at a tracked place, want 1", got)
	}
	for name, n := range r.adopted {
		if strings.Contains(name, "place") && n > 0 {
			t.Fatalf("the model took a place from the engine: %s", name)
		}
	}
}

// TestStateMachineEndsWhatAReturningRowCannotName plays a fixed history on
// the engine and the model: C9 gets =Gone!E1+1, which names a tab no tab
// is called, a person deletes rows 9 and 10 among others, a sort orders
// rows 4 to 8, and the person takes the deletion back. Row 9 returns after
// the row that was above it, now row 5, so the formula lies in C6, where
// its reference names a row above the top of the grid and ends as the
// literal #REF! (Appendix B, engine item 23). The model works that out
// from the place itself, so an engine that keeps the reference there, or
// ends one that it still can name, fails here whatever the random
// histories reach. It runs with the undo payloads in memory and in
// scratch files.
func TestStateMachineEndsWhatAReturningRowCannotName(t *testing.T) {
	defer func() { undoSpillAll = false }()
	for _, spill := range []bool{false, true} {
		t.Run(fmt.Sprintf("in files %v", spill), func(t *testing.T) {
			undoSpillAll = spill
			history := []smOp{
				{Kind: "agent fill", N: 295, M: 42},
				{Kind: "insert", Who: 1, Tab: 6, R: 3, N: 723, M: 674, Text: "word", Formats: 424},
				{Kind: "formula", Who: 3, Tab: 5, R: 8, C: 2, N: 214, M: 550, Text: "2.5", Formats: 457}, // =Gone!E1+1
				{Kind: "delete", Who: 1, Tab: 5, R: 8, C: 2, N: 994, M: 658, Text: "-3", Flag: true, Formats: 77},
				{Kind: "sort", Who: 3, Tab: 5, R: 3, N: 553, M: 634, Text: "Zed", Formats: 313},
				{Kind: "undo", Who: 1, Tab: 5, R: 3, C: 1, N: 336, M: 336, Text: "-3", Flag: true, Formats: 137},
			}
			r, failed, err := runHistory(t, history)
			if err != nil {
				t.Fatalf("step %d, %s: %v", failed+1, history[failed], err)
			}
			mc := r.m.tabs[0].grid[5][2]
			if mc == nil || mc.f == nil || mc.f.Stored() != "=#REF!+1" {
				t.Fatalf("the model's C6 is %+v, want the formula =#REF!+1", mc)
			}
		})
	}
}

// TestStateMachineKnowsTabAfterUndoneRecovery plays a fixed history on the
// engine and the model: a person deletes column E, another deletes column
// D, the first deletes the tab, the second recovers it from a revision and
// undoes the recovery, and the first undoes the deletion of the tab and
// then that of column E. The tab that is back is the one the entries knew,
// so they apply to it again (Appendix B, engine item 26), and column E
// returns at its tracked place, after column C, since column D above it is
// gone (engine item 17). A model that lets the recovery take what its
// entries knew of the tab has no place for the column. It runs with the
// undo payloads in memory and in scratch files.
func TestStateMachineKnowsTabAfterUndoneRecovery(t *testing.T) {
	defer func() { undoSpillAll = false }()
	for _, spill := range []bool{false, true} {
		t.Run(fmt.Sprintf("in files %v", spill), func(t *testing.T) {
			undoSpillAll = spill
			history := []smOp{
				{Kind: "delete", Who: 1, C: 4},        // column E
				{Kind: "add_tab", Who: 2, N: 1},       // a second tab, so the first can go
				{Kind: "delete", Who: 2, C: 3},        // column D
				{Kind: "delete_tab", Who: 1},          // Sheet1
				{Kind: "recover", Who: 2, Flag: true}, // Sheet1 of the latest revision
				{Kind: "undo", Who: 2},                // the recovery
				{Kind: "undo", Who: 1},                // the deletion of Sheet1
				{Kind: "undo", Who: 1},                // the deletion of column E
			}
			r, failed, err := runHistory(t, history)
			if err != nil {
				t.Fatalf("step %d, %s: %v", failed+1, history[failed], err)
			}
			if got := r.m.meta[r.m.byName("Sheet1")].cols[:5]; !slices.Equal(got, []uint64{1, 2, 3, 5, 6}) {
				t.Fatalf("the model's columns of Sheet1 begin %v, want 1, 2, 3, 5, 6", got)
			}
			for name, want := range map[string]int{
				"recovery of a tab that is deleted":                                  1,
				"a world that has the tab it knew again after a recovery was undone": 2,
				"placement: at the tracked place, the upper neighbour gone":          1,
			} {
				if got := r.counts[name]; got != want {
					t.Errorf("%s: %d, want %d", name, got, want)
				}
			}
		})
	}
}

// TestStateMachineSortInPlaceProtects plays a fixed history on the engine
// and the model: A1:A2 holds 2 and 1, a person sorts the two rows, another
// person or the agent sorts them the same way, which moves neither, and
// the first person undoes. The second sort protects the order, so the
// undo is skipped. The model decides that from its own stamps.
func TestStateMachineSortInPlaceProtects(t *testing.T) {
	for _, second := range []smOp{
		{Kind: "sort", Who: 2, M: 1},
		{Kind: "agent sort"},
	} {
		t.Run(second.Kind, func(t *testing.T) {
			history := []smOp{
				{Kind: "set", Who: 1, R: 0, C: 0, Text: "2"},
				{Kind: "set", Who: 1, R: 1, C: 0, Text: "1"},
				{Kind: "sort", Who: 1, M: 1}, // rows 1 and 2 by column A
				second,
				{Kind: "undo", Who: 1},
			}
			r, failed, err := runHistory(t, history)
			if err != nil {
				t.Fatalf("step %d, %s: %v", failed+1, history[failed], err)
			}
			for name, want := range map[string]int{
				"a sort that moved no row":                              1,
				"a sort undo skipped at a later sort that moved no row": 1,
				"undo or redo: the first skipped step: sorted":          1,
			} {
				if got := r.counts[name]; got != want {
					t.Errorf("%s: %d, want %d", name, got, want)
				}
			}
		})
	}
}

// TestStateMachineRepeatedSortUndo plays a fixed history on the engine and
// the model: A1:A2 holds 2 and 1, a person sorts the two rows and sorts
// them the same way again, which moves neither, then undoes twice, redoes
// twice and undoes twice. Each undo of the sort that moved nothing stamps
// the rows for the person's own earlier entry, so every step applies and
// takes a sequence. The model decides that from its own stamps, with the
// payloads in memory and in scratch files.
func TestStateMachineRepeatedSortUndo(t *testing.T) {
	defer func() { undoSpillAll = false }()
	for _, spill := range []bool{false, true} {
		t.Run(fmt.Sprintf("in files %v", spill), func(t *testing.T) {
			undoSpillAll = spill
			history := []smOp{
				{Kind: "set", Who: 1, R: 0, C: 0, Text: "2"},
				{Kind: "set", Who: 1, R: 1, C: 0, Text: "1"},
				{Kind: "sort", Who: 1, M: 1}, // rows 1 and 2 by column A
				{Kind: "sort", Who: 1, M: 1},
				{Kind: "undo", Who: 1}, {Kind: "undo", Who: 1},
				{Kind: "redo", Who: 1}, {Kind: "redo", Who: 1},
				{Kind: "undo", Who: 1}, {Kind: "undo", Who: 1},
			}
			r, failed, err := runHistory(t, history)
			if err != nil {
				t.Fatalf("step %d, %s: %v", failed+1, history[failed], err)
			}
			// The model agreed with the engine after every step.
			if grid := r.m.tabs[0].grid; grid[0][0].content.Num != 2 || grid[1][0].content.Num != 1 {
				t.Fatalf("the model's rows hold %v and %v, want 2 and 1", grid[0][0].content, grid[1][0].content)
			}
			for name, want := range map[string]int{
				"a sort that moved no row":                                  1,
				"a sort undo that moved no row":                             3,
				"undo or redo with a sequence: applied true, skipped false": 6,
			} {
				if got := r.counts[name]; got != want {
					t.Errorf("%s: %d, want %d", name, got, want)
				}
			}
		})
	}
}
