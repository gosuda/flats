package sheet

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// WriteResult is the result of an agent write call (section 18.2). Core
// fills Revision once it has imported the call's outbox entry.
type WriteResult struct {
	State           string         `json:"state"`
	Revision        *int           `json:"revision"`
	RevisionPending bool           `json:"revision_pending,omitempty"`
	CellsWritten    int            `json:"cells_written"`
	FormulasWritten int            `json:"formulas_written"`
	FormulaErrors   int            `json:"formula_errors"`
	Errors          []FormulaError `json:"errors"`
	Created         bool           `json:"created,omitempty"`
	Replay          bool           `json:"replay,omitempty"`
	// OutboxSeq names the outbox entry of the call's revision.
	OutboxSeq int64 `json:"-"`
}

// FormulaError names a formula of a call whose value is an error.
type FormulaError struct {
	At    string `json:"at"`
	Error string `json:"error"`
}

// stateToken returns the opaque state of section 7.3: the store and the
// sequence.
func stateToken(store string, seq uint64) string {
	return "s_" + store[:12] + "." + strconv.FormatUint(seq, 10)
}

func parseState(store, token string) (uint64, bool) {
	rest, ok := strings.CutPrefix(token, "s_"+store[:12]+".")
	if !ok {
		return 0, false
	}
	seq, err := strconv.ParseUint(rest, 10, 64)
	return seq, err == nil && strconv.FormatUint(seq, 10) == rest
}

// tabView is a call's own view of tab names (section 7.3): it starts as the
// live names and follows the call's own tab operations. It also keeps what
// each tab was before the call, for the checks against the state.
type tabView struct {
	state    uint64
	names    map[string]*Tab
	before   map[*Tab]tabBefore
	identity map[*Tab]bool
}

type tabBefore struct{ born, nameSeq, structSeq uint64 }

func newTabView(x *txn, state uint64) *tabView {
	v := &tabView{state: state, names: map[string]*Tab{}, before: map[*Tab]tabBefore{}, identity: map[*Tab]bool{}}
	for _, t := range x.wb.tabs {
		v.names[formula.FoldName(t.name)] = t
		v.before[t] = tabBefore{t.born, t.nameSeq, t.structSeq}
	}
	return v
}

// find resolves a tab name for an operation. address says that the
// operation names an address in the tab, which adds the structure check.
func (v *tabView) find(name string, address bool) (*Tab, *WriteError) {
	t := v.names[formula.FoldName(name)]
	if t == nil {
		return nil, conflict("tab_changed", name)
	}
	b, existed := v.before[t]
	if !existed {
		return t, nil
	}
	if !v.identity[t] {
		if b.born > v.state || b.nameSeq > v.state {
			return nil, conflict("tab_changed", name)
		}
		v.identity[t] = true
	}
	if address && b.structSeq > v.state {
		return nil, conflict("structure_changed", name)
	}
	return t, nil
}

// applyAgent applies an agent call operation by operation. Any failed check
// refuses the whole call; the caller reverts the journal (spec rule 36).
func (x *txn) applyAgent(u *Update) *WriteError {
	wb := x.wb
	onlyAdds := true
	for i := range u.Ops {
		onlyAdds = onlyAdds && u.Ops[i].Op == "add_tab"
	}
	var state uint64
	if u.State == "" {
		if !onlyAdds || u.Guard != nil {
			return conflict("state_required", "")
		}
		state = wb.seq
	} else {
		var ok bool
		if state, ok = parseState(wb.store, u.State); !ok || state > wb.seq {
			return conflict("state_unknown", "")
		}
		if wb.resetSeq > state {
			return conflict("rolled_back", "")
		}
	}
	x.base = state
	view := newTabView(x, state)
	if g := u.Guard; g != nil {
		if g.Unchanged && wb.seq != state {
			return conflict("guard", "")
		}
		for _, text := range g.Ranges {
			r, _ := formula.ParseRange(text)
			t, err := view.find(r.Tab, true)
			if err != nil {
				return err
			}
			if state < wb.guardFloor {
				return conflict("state_too_old", "")
			}
			at, changed, err := x.guardRange(t, r, state)
			if err != nil {
				return err
			}
			if changed {
				return conflict("guard", formula.Range{Tab: t.name, First: at, Last: at}.String())
			}
		}
	}
	x.bulk = u.cells > fullDirtyCells
	for i := range u.Ops {
		if err := x.agentOp(view, &u.Ops[i]); err != nil {
			if err.Code == "invalid" || err.Code == "sheet_capacity" {
				err.Detail = strings.TrimSuffix(fmt.Sprintf("operation %d (%s): %s", i+1, u.Ops[i].Op, err.Detail), ": ")
			}
			return err
		}
	}
	return nil
}

// guardRange reports the first cell of the range whose content or formula
// text changed after state (section 7.4).
//
// A call may name the same cells in any number of guards, so reading a
// range is work of the call (section 10.4): a unit for the guard, one for
// each row of the range and one for each record of those rows. The
// deadline ends it before a guard and between two rows.
func (x *txn) guardRange(t *Tab, r formula.Range, state uint64) (formula.Pos, bool, *WriteError) {
	x.spend(64)
	if err := x.stop(); err != nil {
		return formula.Pos{}, false, err
	}
	r, ok := r.Clip(int32(t.rows.len()), int32(t.cols.len()))
	if !ok {
		return formula.Pos{}, false, nil
	}
	rows := t.rowsByPos()
	for row := r.First.Row; row <= r.Last.Row; row++ {
		rd := rows[row]
		steps := 64
		if rd != nil {
			steps += 64 * len(rd.cells)
		}
		x.spend(steps)
		if x.late() {
			return formula.Pos{}, false, calcTimeout()
		}
		if rd == nil {
			continue
		}
		best, found := int32(0), false
		for _, s := range rd.cells {
			col := t.cols.pos[uint64(s.col)]
			if col < r.First.Col || col > r.Last.Col || max(s.c.seq, s.c.textSeq) <= state {
				continue
			}
			if !found || col < best {
				best, found = col, true
			}
		}
		if found {
			return formula.Pos{Row: row, Col: best}, true, nil
		}
	}
	return formula.Pos{}, false, nil
}

func (x *txn) agentOp(v *tabView, op *AgentOp) *WriteError {
	wb := x.wb
	// A call whose earlier operations met the deadline, or the bound of
	// what it holds, starts no other.
	if err := x.stop(); err != nil {
		return err
	}
	x.keep(journalOpBytes)
	if op.Op == "add_tab" {
		at := len(wb.tabs)
		if op.place > 0 {
			at = min(op.place-1, at)
		}
		t, err := x.addTab(op.Name, at)
		if err != nil {
			return err
		}
		v.names[formula.FoldName(t.name)] = t
		return nil
	}
	address := true
	switch op.Op {
	case "rename_tab", "move_tab", "delete_tab", "tab_settings":
		address = false
	}
	t, err := v.find(op.Tab, address)
	if err != nil {
		return err
	}
	switch op.Op {
	case "write":
		width := 0
		for _, line := range op.values {
			width = max(width, len(line))
		}
		if len(op.values) == 0 || width == 0 {
			return nil
		}
		if err := x.growTo(t, int(op.at.Row)+len(op.values), int(op.at.Col)+width); err != nil {
			return err
		}
		for i, line := range op.values {
			if err := x.stop(); err != nil {
				return err
			}
			row := RowID(t.rows.ids[int(op.at.Row)+i])
			for j, cell := range line {
				col := ColID(t.cols.ids[int(op.at.Col)+j])
				switch {
				case cell.formula != "":
					f, err := x.parse(cell.formula, formula.Pos{Row: op.at.Row + int32(i), Col: op.at.Col + int32(j)})
					if err != nil {
						return err
					}
					x.setContent(t, row, col, formula.Content{}, f)
				case cell.content.Kind == formula.ContentBlank:
					x.clearContent(t, row, col)
				default:
					x.setContent(t, row, col, cell.content, nil)
				}
			}
		}
	case "import_csv":
		width := 0
		for _, line := range op.fields {
			width = max(width, len(line))
		}
		if width == 0 {
			return nil
		}
		if err := x.growTo(t, int(op.at.Row)+len(op.fields), int(op.at.Col)+width); err != nil {
			return err
		}
		for i, line := range op.fields {
			if err := x.stop(); err != nil {
				return err
			}
			row := RowID(t.rows.ids[int(op.at.Row)+i])
			for j, text := range line {
				if err := x.typed(t, row, ColID(t.cols.ids[int(op.at.Col)+j]), text, nil); err != nil {
					return err
				}
			}
		}
	case "clear", "format":
		reset := formatPatch{mask: 1<<propCount - 1}
		for _, r := range op.ranges {
			r, ok := r.Clip(int32(t.rows.len()), int32(t.cols.len()))
			if !ok {
				continue
			}
			rows := t.rows.ids[r.First.Row : r.Last.Row+1]
			cols := t.cols.ids[r.First.Col : r.Last.Col+1]
			create := op.Op == "format" && op.patch.f != (formula.Format{})
			_, err := x.overCells(t, rows, cols, create, func(row RowID, col ColID) {
				if op.Op == "format" {
					x.setStyle(t, row, col, op.patch)
					return
				}
				x.clearContent(t, row, col)
				if c := t.cell(row, col); op.Formats && c != nil && c.style != 0 {
					x.setStyle(t, row, col, reset)
				}
			})
			if err != nil {
				return err
			}
		}
	case "widths":
		r, ok := op.ranges[0].Clip(int32(t.rows.len()), int32(t.cols.len()))
		if !ok {
			return invalidf("cols", "the columns are outside the tab")
		}
		for col := r.First.Col; col <= r.Last.Col; col++ {
			x.setWidth(t, ColID(t.cols.ids[col]), op.Width)
		}
	case "insert_rows", "insert_cols":
		rows := op.Op == "insert_rows"
		at := int(op.at.Col)
		if rows {
			at = int(op.at.Row)
		}
		if at > x.axisOf(t, rows).len() {
			return invalidf("at", "the position is past the end of the tab")
		}
		_, err := x.insertAxis(t, rows, at, op.Count)
		return err
	case "delete_rows", "delete_cols":
		rows := op.Op == "delete_rows"
		at := int(op.at.Col)
		if rows {
			at = int(op.at.Row)
		}
		return x.deleteAxis(t, rows, at, op.Count)
	case "sort":
		r := op.ranges[0]
		last := min(r.Last.Row, int32(t.rows.len())-1)
		if int(r.First.Col) >= t.cols.len() {
			return invalidf("by", "the column is outside the tab")
		}
		var positions []int32
		for p := max(r.First.Row, int32(t.frozenRows)); p <= last; p++ {
			positions = append(positions, p)
		}
		return x.sortRows(t, positions, ColID(t.cols.ids[r.First.Col]), op.Desc)
	case "rename_tab":
		old := formula.FoldName(t.name)
		if err := x.renameTab(t, op.Name); err != nil {
			return err
		}
		delete(v.names, old)
		v.names[formula.FoldName(t.name)] = t
	case "move_tab":
		x.moveTab(t, *op.To-1)
	case "delete_tab":
		if err := x.deleteTab(t); err != nil {
			return err
		}
		delete(v.names, formula.FoldName(t.name))
	case "tab_settings":
		fr, fc := t.frozenRows, t.frozenCols
		if op.FrozenRows != nil {
			fr = *op.FrozenRows
		}
		if op.FrozenCols != nil {
			fc = *op.FrozenCols
		}
		x.setSettings(t, fr, fc)
	}
	return nil
}

// maxResultErrors is the most formula errors a result lists.
const maxResultErrors = 100

// result builds the result of an applied call, after its recalculation.
func (x *txn) result() WriteResult {
	wb := x.wb
	res := WriteResult{State: stateToken(wb.store, x.seq), CellsWritten: x.nwrote, Errors: []FormulaError{}}
	seen := map[cellRef]bool{}
	for _, ref := range x.fwrote {
		c := ref.tab.cell(ref.row, ref.col)
		if c == nil || c.f == nil || seen[ref] || wb.byID[ref.tab.id] != ref.tab {
			continue
		}
		seen[ref] = true
		res.FormulasWritten++
		if code := c.val.Err(); code != formula.ErrNone {
			res.FormulaErrors++
			if len(res.Errors) < maxResultErrors {
				pos := x.posOf(ref.tab, ref.row, ref.col)
				res.Errors = append(res.Errors, FormulaError{At: formula.Range{Tab: ref.tab.name, First: pos, Last: pos}.String(), Error: code.String()})
			}
		}
	}
	return res
}
