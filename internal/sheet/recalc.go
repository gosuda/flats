package sheet

import (
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// maxCalcText bounds the code points of text that all formulas of one state
// calculate (spec rule 9). A state over it is calculated in limited mode.
const maxCalcText = 50_000_000

// fullDirtyCells is the number of written cells from which an action marks
// every formula dirty (technical design, section 10.2).
const fullDirtyCells = 1000

// passStride is the number of formulas a loop of a recalculation handles
// between two looks at the deadline (technical design, section 10.4). For
// a pass that no edit started the look is also where it sees a stop and
// takes its turn at the gate (section 10.5). It does not depend on the
// work the formulas count, so every loop reaches it whatever the formulas
// are.
const passStride = 64

// passedStep is the number of rows and cells without a formula that a loop
// goes past between two additions to the budget, a step each.
const passedStep = 4096

// orderBlock is the number of cells that ordering enumerates in a range
// between two additions to the budget, a unit each.
const orderBlock = 1024

// pace counts the formulas of one loop of a recalculation.
type pace struct {
	b *formula.Budget
	n int
}

// step is called once for each formula of the loop and reports whether the
// pass may go on.
func (p *pace) step() bool {
	if p.n++; p.n < passStride {
		return !p.b.Exceeded()
	}
	p.n = 0
	return p.b.Check()
}

// eachFormulaPaced visits every formula cell with its position, tabs in
// order and rows by position, for a loop of a recalculation or of an
// action over every formula. It looks at the deadline every passStride
// formulas and counts the rows and the cells without a formula that it
// goes past. It stops once the deadline has passed or fn returns false,
// and reports whether it reached the end.
func (wb *Workbook) eachFormulaPaced(b *formula.Budget, fn func(t *Tab, row RowID, col ColID, pos formula.Pos, c *Cell) bool) bool {
	p := pace{b: b}
	passed := 0
	for _, t := range wb.tabs {
		for pos, rd := range t.rowsByPos() {
			if passed++; passed >= passedStep {
				if !b.Add(int64(passed) / 64) {
					return false
				}
				passed = 0
			}
			if rd == nil || rd.nf == 0 {
				continue
			}
			row := RowID(t.rows.ids[pos])
			for _, s := range rd.cells {
				if s.c.f == nil {
					passed++
					continue
				}
				if w := counting.Load(); w != nil {
					w.formulas.Add(1)
				}
				if !fn(t, row, s.col, formula.Pos{Row: int32(pos), Col: t.cols.pos[uint64(s.col)]}, s.c) || !p.step() {
					return false
				}
			}
		}
	}
	return b.Add(int64(passed) / 64)
}

// observeCalc reports one recalculation to the host: whether an edit
// started it and how long it took. Its callers do not hold the workbook
// lock.
func (in *Instance) observeCalc(edit bool, took time.Duration) {
	if fn := in.opts.OnRecalc; fn != nil {
		fn(edit, took)
	}
}

type cellAt struct {
	tab *Tab
	pos formula.Pos
}

// calcReq says what a recalculation starts from.
type calcReq struct {
	all    bool          // every formula is dirty
	cells  []cellAt      // the cells an edit wrote
	today  bool          // the calculation date changed
	date   *formula.Date // the date to calculate with, when it is not the workbook's
	budget *formula.Budget
}

// calcOut is a finished pass: the overlay of section 10.3. Its values sit
// in the nodes under the pass number until apply writes them to the cells.
type calcOut struct {
	nodes   []*fnode
	limited bool
	text    int64
	passes  int
	took    time.Duration
}

func calcTimeout() *WriteError { return capacity("recalculation_time", 0, 0) }

// recalc evaluates the dirty formulas into an overlay. It refuses when the
// budget's deadline passed, or when the state is over a read limit, which
// it finds before it builds the index. The overlay is then dropped by the
// next pass number and no cell has changed. The time it took is reported
// either way.
func (wb *Workbook) recalc(req calcReq) (calcOut, *WriteError) {
	started := time.Now()
	out, err := wb.recalculate(req)
	out.took = time.Since(started)
	return out, err
}

func (wb *Workbook) recalculate(req calcReq) (calcOut, *WriteError) {
	b := req.budget
	if wb.deps.stale {
		if err := wb.buildDeps(b); err != nil {
			return calcOut{}, err
		}
		req.all = true
	}
	wb.pass++
	pass := wb.pass
	var dirty []*fnode
	mark := func(n *fnode) {
		if n.dirty != pass {
			n.dirty = pass
			dirty = append(dirty, n)
		}
	}
	markAll := func() bool {
		return wb.eachFormulaPaced(b, func(_ *Tab, _ RowID, _ ColID, _ formula.Pos, c *Cell) bool {
			mark(c.node)
			return true
		})
	}
	if req.all || wb.limited {
		if !markAll() {
			return calcOut{}, calcTimeout()
		}
	} else {
		p := pace{b: b}
		for _, at := range req.cells {
			if c := at.tab.cellAt(at.pos); c != nil && c.node != nil {
				mark(c.node)
			}
			if !b.Add(wb.deps.dependents(at.tab.id, at.pos, mark, b)) || !p.step() {
				return calcOut{}, calcTimeout()
			}
		}
		if req.today {
			for n := range wb.deps.today {
				mark(n)
				if !p.step() {
					return calcOut{}, calcTimeout()
				}
			}
		}
		for i := 0; i < len(dirty); i++ {
			n := dirty[i]
			pos, ok := n.tab.posOf(n.row, n.col)
			if !ok {
				continue
			}
			if !b.Add(wb.deps.dependents(n.tab.id, pos, mark, b)) || !p.step() {
				return calcOut{}, calcTimeout()
			}
		}
	}
	order, ok := wb.order(dirty, pass, b)
	if !ok {
		return calcOut{}, calcTimeout()
	}
	// The text results the dirty formulas had leave the total. Counting
	// the code points of a text goes through it.
	base := wb.count.calcText
	p := pace{b: b}
	for _, n := range order {
		if s := n.cell.val.Text(); s != "" {
			base -= int64(utf8.RuneCountInString(s))
			b.Add(int64(len(s)+63) / 64)
		}
		if !p.step() {
			return calcOut{}, calcTimeout()
		}
	}
	out := calcOut{nodes: order, passes: 1}
	today := wb.date
	if req.date != nil {
		today = *req.date
	}
	text, fits, ok := wb.evaluate(order, pass, false, base, today, b)
	if !ok {
		return calcOut{}, calcTimeout()
	}
	if fits {
		out.text = text
		return out, nil
	}
	// The ordinary results do not fit. Every formula is evaluated once more
	// in limited mode, in the same order when the first pass covered all.
	if len(order) != wb.count.formulas {
		wb.pass++
		pass = wb.pass
		dirty = dirty[:0]
		if !markAll() {
			return calcOut{}, calcTimeout()
		}
		if order, ok = wb.order(dirty, pass, b); !ok {
			return calcOut{}, calcTimeout()
		}
	} else {
		wb.pass++
		pass = wb.pass
	}
	text, _, ok = wb.evaluate(order, pass, true, 0, today, b)
	if !ok {
		return calcOut{}, calcTimeout()
	}
	return calcOut{nodes: order, limited: true, text: text, passes: 2}, nil
}

// evaluate calculates the nodes in order into the overlay of the pass. The
// ordinary pass stops, with fits false, as soon as the text results pass
// the limit of a state.
//
// The evaluator counts its work on the budget, which looks at the deadline
// every 4,096 units, inside a formula too. The loop looks at it as well
// every passStride formulas, whatever they counted. ok is false once the
// deadline has passed: the overlay is then not used, also when the last
// formula was calculated before the look that found it.
func (wb *Workbook) evaluate(order []*fnode, pass uint32, limited bool, text int64, today formula.Date, b *formula.Budget) (total int64, fits, ok bool) {
	g := &grid{wb: wb, pass: pass}
	env := formula.Env{Grid: g, Today: today, Budget: b, Limited: limited}
	p := pace{b: b}
	for _, n := range order {
		if n.cyc {
			n.ov = formula.ErrorValue(formula.ErrCycle)
		} else {
			env.Tab = formula.TabRef(n.tab.id)
			env.At, _ = n.tab.posOf(n.row, n.col)
			n.ov = n.cell.f.Eval(&env)
		}
		n.done = pass
		// Counting the code points of a text result goes through it. A
		// formula that is one reference has a result that was not copied,
		// so nothing has counted its text yet.
		if s := n.ov.Text(); s != "" {
			text += int64(utf8.RuneCountInString(s))
			b.Add(int64(len(s)+63) / 64)
		}
		if !p.step() {
			return 0, false, false
		}
		if !limited && text > maxCalcText {
			return text, false, true
		}
	}
	return text, true, !b.Exceeded()
}

// apply writes a finished overlay to the cells and returns the nodes whose
// value changed. The journal of the action seq keeps the earlier values.
// The first value it keeps of a cell is what the sheet held before the
// action; a later one is text an earlier pass of the action gave, which
// nothing else holds, so its bytes count in the journal.
func (wb *Workbook) apply(out calcOut, j *journal, seq uint64) []*fnode {
	type undo struct {
		c   *Cell
		old formula.Value
	}
	var changed []*fnode
	var olds []undo
	made := 0
	for _, n := range out.nodes {
		if n.cell.val != n.ov {
			if j != nil {
				if n.kept == seq {
					made += len(n.cell.val.Text())
				}
				n.kept = seq
				olds = append(olds, undo{n.cell, n.cell.val})
			}
			n.cell.val = n.ov
			changed = append(changed, n)
		}
	}
	text, limited := wb.count.calcText, wb.limited
	wb.count.calcText, wb.limited = out.text, out.limited
	if j != nil {
		j.pushHolding(func() {
			for _, u := range olds {
				u.c.val = u.old
			}
			wb.count.calcText, wb.limited = text, limited
		}, cap(olds)*int(unsafe.Sizeof(undo{}))+made)
	}
	return changed
}

type rref struct {
	tab            *Tab
	r0, r1, c0, c1 int32
}

type frame struct {
	n    *fnode
	refs []rref
	ri   int
	r    int32
	si   int
}

// order sorts the dirty nodes so that each comes after the dirty formulas
// it reads, by an iterative walk over precedents with an explicit stack.
// Formulas on a cycle are marked: strongly connected groups of more than
// one formula, and a formula that reads its own cell.
func (wb *Workbook) order(dirty []*fnode, pass uint32, b *formula.Budget) ([]*fnode, bool) {
	order := make([]*fnode, 0, len(dirty))
	var stack []frame
	var open []*fnode
	next := int32(1)
	p := pace{b: b}
	push := func(n *fnode) bool {
		n.seen, n.index, n.low, n.onStack, n.cyc = pass, next, next, true, false
		next++
		open = append(open, n)
		fr := frame{n: n}
		pos, _ := n.tab.posOf(n.row, n.col)
		// The formula is a unit and so is each reference it names, also
		// one that reads no cell of its tab.
		work := int64(1)
		n.cell.f.Refs(pos, func(r formula.Ref) {
			work++
			t := n.tab
			if r.Tab != "" {
				if t = wb.tab(r.Tab); t == nil {
					return
				}
			}
			r1 := min(r.Last.Row, int32(len(t.rows.ids))-1)
			c1 := min(r.Last.Col, int32(len(t.cols.ids))-1)
			if r.First.Row <= r1 && r.First.Col <= c1 {
				fr.refs = append(fr.refs, rref{t, r.First.Row, r1, r.First.Col, c1})
			}
		})
		if len(fr.refs) > 0 {
			fr.r = fr.refs[0].r0
		}
		stack = append(stack, fr)
		return b.Add(work) && p.step()
	}
	// precedent returns the next dirty formula the frame's formula reads.
	// A range may hold no dirty formula in a million cells, so the cells
	// reach the budget a block of rows at a time, as the evaluator's reads
	// of a range do.
	precedent := func(fr *frame) (*fnode, bool) {
		var work int64
		for fr.ri < len(fr.refs) {
			ref := fr.refs[fr.ri]
			rows := ref.tab.rowsByPos()
			for fr.r <= ref.r1 {
				if work >= orderBlock {
					if !b.Add(work) {
						return nil, false
					}
					work = 0
				}
				rd := rows[fr.r]
				work++
				if rd != nil && rd.nf > 0 {
					// A narrow range looks its columns up in the row; a
					// wide one walks the row's cells. Either way the work
					// follows the cells of the range, not the row.
					if width := int(ref.c1 - ref.c0 + 1); width < len(rd.cells) {
						for fr.si < width {
							col := ColID(ref.tab.cols.ids[int(ref.c0)+fr.si])
							fr.si++
							work++
							if c := rd.get(col); c != nil && c.node != nil && c.node.dirty == pass {
								return c.node, b.Add(work)
							}
						}
					} else {
						for fr.si < len(rd.cells) {
							s := rd.cells[fr.si]
							fr.si++
							work++
							m := s.c.node
							if m == nil || m.dirty != pass {
								continue
							}
							if c := ref.tab.cols.pos[uint64(s.col)]; c >= ref.c0 && c <= ref.c1 {
								return m, b.Add(work)
							}
						}
					}
				}
				fr.r++
				fr.si = 0
			}
			fr.ri++
			if fr.ri < len(fr.refs) {
				fr.r = fr.refs[fr.ri].r0
			}
		}
		return nil, b.Add(work)
	}
	// Every formula that the loops below pass, take from the stack or
	// mark is a step of the pace, so taking a group of 100,000 formulas on
	// a cycle apart reaches the deadline as pushing them did.
	for _, root := range dirty {
		if root.seen == pass {
			if !p.step() {
				return nil, false
			}
			continue
		}
		if !push(root) {
			return nil, false
		}
		for len(stack) > 0 {
			fr := &stack[len(stack)-1]
			m, ok := precedent(fr)
			if !ok {
				return nil, false
			}
			if m != nil {
				switch {
				case m.seen != pass:
					if !push(m) {
						return nil, false
					}
				case m == fr.n:
					m.cyc = true
				case m.onStack:
					fr.n.low = min(fr.n.low, m.index)
				}
				continue
			}
			n := fr.n
			stack = stack[:len(stack)-1]
			if !p.step() {
				return nil, false
			}
			if len(stack) > 0 {
				parent := stack[len(stack)-1].n
				parent.low = min(parent.low, n.low)
			}
			if n.low != n.index {
				continue
			}
			// The group is the formulas above n in open. They are marked
			// from the top down, so the search for n is the marking.
			cycle := open[len(open)-1] != n
			i := len(open) - 1
			for ; ; i-- {
				m := open[i]
				m.onStack = false
				m.cyc = m.cyc || cycle
				if m == n {
					break
				}
				if !p.step() {
					return nil, false
				}
			}
			order = append(order, open[i:]...)
			open = open[:i]
		}
	}
	return order, true
}
