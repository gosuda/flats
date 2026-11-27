package sheet

import (
	"math"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// personOp is one operation of a person's action (technical design, section
// 7.1), as walk built it from the frame (protocol.go). Every operation
// shares this one shape; each reads the members its row of that table
// names, and has says which members the frame carried.
type personOp struct {
	Op  string
	has uint32

	Tab, Row, Col   int64
	Text            string
	At              [2]int64
	Rows, Cols, IDs idList
	Values          [][]string
	Set             formatPatch
	Width           int64
	From, To        *rectSpec
	Source, Target  *rectSpec
	Mode            string
	Cut             bool
	Beside          int64
	Side            string
	Count           int64
	By              int64
	Desc            bool
	FrozenRows      int64
	FrozenCols      int64
	Name            string
	Before          int64
	beforeNull      bool
	setBad          *WriteError // why Set is no format of rule 6
	bad             *WriteError // why the operation is not one of section 7.1
}

// carries reports whether the frame gave the operation the member of slot.
func (op *personOp) carries(slot int) bool { return op.has&opBit(slot) != 0 }

// rectSpec is a range of a copy or a fill.
type rectSpec struct {
	Tab        int64
	Rows, Cols idList
}

// opResult is the Result of Appendix A.
type opResult struct {
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Applied int    `json:"applied,omitempty"`
	Ignored int    `json:"ignored,omitempty"`
}

type target struct {
	id  uint64
	new int // k for the k-th added row or column
}

// idSpec is an idList read for an axis: every row or column, or ids and
// {"new": k} items one by one.
type idSpec struct {
	all   bool
	items []target
}

// spec reads the list for an axis of at most limit rows or columns. It
// refuses a text other than "all", a run that has not two numbers, an id
// or a count that is no positive integer, and more ids than the axis can
// hold.
func (l *idList) spec(limit int) (idSpec, *WriteError) {
	bad := invalidf("ids", "ids are \"all\" or a list of ids, [first, count] runs and {\"new\": k}")
	if l.all {
		return idSpec{all: true}, nil
	}
	if l.bad {
		return idSpec{}, bad
	}
	var spec idSpec
	for _, it := range l.items {
		switch it.kind {
		case idRun:
			if it.a <= 0 || it.a >= maxID || it.b <= 0 || it.b > int64(limit-len(spec.items)) {
				return idSpec{}, bad
			}
			for i := int64(0); i < it.b; i++ {
				spec.items = append(spec.items, target{id: uint64(it.a + i)})
			}
		case idNew:
			if it.a < 1 || it.a > int64(limit) {
				return idSpec{}, bad
			}
			spec.items = append(spec.items, target{new: int(it.a)})
		case idOne:
			if it.a <= 0 || it.a >= maxID {
				return idSpec{}, bad
			}
			spec.items = append(spec.items, target{id: uint64(it.a)})
		default:
			return idSpec{}, bad
		}
		if len(spec.items) > limit {
			return idSpec{}, bad
		}
	}
	return spec, nil
}

func (x *txn) live(a *axis, id uint64) bool {
	_, ok := a.pos[id]
	return ok && a.born.get(id) <= x.base
}

// resolve maps a spec to ids in its order, with 0 for an id that is gone or
// was restored after the action's base (spec rules 32 and 34). grow adds
// rows or columns at the end for {"new": k} items.
func (x *txn) resolve(t *Tab, rows bool, spec idSpec, grow bool) (ids []uint64, gone int, err *WriteError) {
	a := x.axisOf(t, rows)
	// Each id that is looked up is a unit of the action's work: a list
	// of a few bytes names up to every row of a tab.
	if spec.all {
		for _, id := range a.ids {
			x.spend(64)
			if x.late() {
				return nil, 0, calcTimeout()
			}
			if a.born.get(id) <= x.base {
				ids = append(ids, id)
			}
		}
		return ids, 0, nil
	}
	added := 0
	for _, it := range spec.items {
		added = max(added, it.new)
	}
	var fresh []uint64
	if added > 0 {
		if !grow {
			return nil, 0, invalidf("ids", "this operation cannot add rows or columns")
		}
		if fresh, err = x.insertAxis(t, rows, a.len(), added); err != nil {
			return nil, 0, err
		}
	}
	ids = make([]uint64, len(spec.items))
	for i, it := range spec.items {
		x.spend(64)
		if x.late() {
			return nil, 0, calcTimeout()
		}
		switch {
		case it.new > 0:
			ids[i] = fresh[it.new-1]
		case x.live(a, it.id):
			ids[i] = it.id
		default:
			gone++
		}
	}
	return ids, gone, nil
}

func (x *txn) personTab(id uint64) (*Tab, error) {
	t := x.wb.byID[TabID(id)]
	if t == nil || x.base < t.born {
		return nil, errGone
	}
	return t, nil
}

func (x *txn) rect(t *Tab, rowIDs, colIDs *idList, grow bool) (rows, cols []uint64, gone int, err *WriteError) {
	rs, err := rowIDs.spec(formula.MaxRows)
	if err != nil {
		return nil, nil, 0, err
	}
	cs, err := colIDs.spec(formula.MaxCols)
	if err != nil {
		return nil, nil, 0, err
	}
	rows, goneRows, err := x.resolve(t, true, rs, grow)
	if err != nil {
		return nil, nil, 0, err
	}
	cols, goneCols, err := x.resolve(t, false, cs, grow)
	if err != nil {
		return nil, nil, 0, err
	}
	// Cells of a gone row or column, counted once where both are gone.
	gone = goneRows*len(cols) + goneCols*len(rows) - goneRows*goneCols
	return rows, cols, gone, nil
}

// typed writes typed entry into a cell (spec rule 5). f is set when the
// caller already parsed and adjusted a formula.
func (x *txn) typed(t *Tab, row RowID, col ColID, text string, f *formula.Formula) *WriteError {
	content, nf := formula.ParseTyped(text)
	switch {
	case content.Kind == formula.ContentFormula:
		if f == nil {
			var err *WriteError
			if f, err = x.parse(content.Text, x.posOf(t, row, col)); err != nil {
				return err
			}
		}
	case content.Kind == formula.ContentText:
		if err := checkText(content.Text); err != nil {
			return err
		}
	case content.Kind == formula.ContentBlank:
		x.clearContent(t, row, col)
		return nil
	}
	// Every other format comes in through decodeFormatPatch, which checks
	// it. With this check each format a cell holds can be written.
	if nf != nil && !nf.Valid() {
		return invalidf("format", "the entry sets a number format outside rule 6")
	}
	x.setContent(t, row, col, content, f)
	if nf != nil && x.wb.styles.format(t.cell(row, col).style).Number == (formula.NumberFormat{}) {
		x.setStyle(t, row, col, formatPatch{mask: 1 << propNumber, f: formula.Format{Number: *nf}})
	}
	return nil
}

// committed writes what a person typed or pasted into a cell. A formula was
// written against the screen of the action's base, so it is adjusted from
// there as if it had been in its cell since that screen (spec rule 14). at
// is the address that screen showed, when the client sent it; a paste
// names its cells by ids only, and the address is then worked out from the
// structural log.
func (x *txn) committed(t *Tab, row RowID, col ColID, text string, at *pair[int32]) *WriteError {
	content, _ := formula.ParseTyped(text)
	if content.Kind != formula.ContentFormula {
		return x.typed(t, row, col, text, nil)
	}
	cur := x.posOf(t, row, col)
	shown := x.screenPos(t, row, cur)
	if at != nil {
		shown = formula.Pos{Row: at[0], Col: at[1]}
	}
	f, pos, err := x.adjust(t, row, content.Text, shown, math.MaxUint64)
	if err != nil {
		return err
	}
	if pos != cur {
		return staleFormula()
	}
	return x.typed(t, row, col, text, f)
}

// applyPerson applies one operation of a person's action. It returns
// errGone for a target that is gone and a *WriteError for a refusal.
func (x *txn) applyPerson(op *personOp) (applied, ignored int, err error) {
	if op.bad != nil {
		return 0, 0, op.bad
	}
	// An action whose earlier operations met the deadline, or the bound of
	// what it holds, starts no other.
	if err := x.stop(); err != nil {
		return 0, 0, err
	}
	x.keep(journalOpBytes)
	wb := x.wb
	switch op.Op {
	case "add_tab":
		name := wb.freeTabName()
		if op.carries(sOpName) {
			name = op.Name
		}
		at := len(wb.tabs)
		if op.carries(sOpBefore) && !op.beforeNull {
			before := wb.byID[TabID(op.Before)]
			if before == nil {
				return 0, 0, errGone
			}
			at = wb.tabIndex(before)
		}
		if _, err := x.addTab(name, at); err != nil {
			return 0, 0, err
		}
		return 1, 0, nil
	case "copy":
		return x.copyOp(op.From, op.To, op.Mode, op.Cut)
	}
	t, gerr := x.personTab(uint64(op.Tab))
	if gerr != nil {
		return 0, 0, gerr
	}
	switch op.Op {
	case "set":
		if !x.live(&t.rows, uint64(op.Row)) || !x.live(&t.cols, uint64(op.Col)) {
			return 0, 0, errGone
		}
		var at *pair[int32]
		if op.carries(sOpAt) {
			at = &pair[int32]{int32(op.At[0]), int32(op.At[1])}
		}
		if err := x.committed(t, RowID(op.Row), ColID(op.Col), op.Text, at); err != nil {
			return 0, 0, err
		}
		return 1, 0, nil
	case "write":
		rowsBefore, colsBefore := t.rows.len(), t.cols.len()
		rows, cols, _, err := x.rect(t, &op.Rows, &op.Cols, true)
		if err != nil {
			return 0, 0, err
		}
		if len(op.Values) != len(rows) {
			return 0, 0, invalidf("values", "values has %d rows for %d row ids", len(op.Values), len(rows))
		}
		for i, line := range op.Values {
			if len(line) != len(cols) {
				return 0, 0, invalidf("values", "row %d of values has %d cells for %d column ids", i, len(line), len(cols))
			}
			if err := x.stop(); err != nil {
				return 0, 0, err
			}
			for j, text := range line {
				if rows[i] == 0 || cols[j] == 0 {
					ignored++
					continue
				}
				if err := x.committed(t, RowID(rows[i]), ColID(cols[j]), text, nil); err != nil {
					return 0, 0, err
				}
				applied++
			}
		}
		// As for a copy: added rows and columns stay only with a cell.
		if applied == 0 && (t.rows.len() != rowsBefore || t.cols.len() != colsBefore) {
			return 0, 0, errGone
		}
		return applied, ignored, nil
	case "clear", "format":
		rows, cols, gone, err := x.rect(t, &op.Rows, &op.Cols, false)
		if err != nil {
			return 0, 0, err
		}
		patch := op.Set
		if op.Op == "format" && op.setBad != nil {
			refusal := *op.setBad
			return 0, 0, &refusal
		}
		n, err := x.overCells(t, rows, cols, op.Op == "format" && patch.f != (formula.Format{}), func(row RowID, col ColID) {
			if op.Op == "format" {
				x.setStyle(t, row, col, patch)
			} else {
				x.clearContent(t, row, col)
			}
		})
		return n, gone, errOrNil(err)
	case "width":
		cs, err := op.Cols.spec(formula.MaxCols)
		if err != nil {
			return 0, 0, err
		}
		if op.Width < minWidth || op.Width > maxWidth {
			return 0, 0, invalidf("width", "a width is 40 to 1,000 pixels")
		}
		cols, gone, err := x.resolve(t, false, cs, false)
		if err != nil {
			return 0, 0, err
		}
		for _, col := range cols {
			if col == 0 {
				continue
			}
			// A column that shows the width already keeps it: nothing
			// changes, and nothing is written for it (Appendix B, engine
			// item 6).
			if w, had := t.widths[ColID(col)]; (had && w.px != int(op.Width)) || (!had && int(op.Width) != defaultWidth) {
				x.setWidth(t, ColID(col), int(op.Width))
			}
			applied++
		}
		return applied, gone, nil
	case "fill":
		// The shape of a fill gives it both ranges, on the operation's tab,
		// and the decoder left only the modes of section 8.5. A fill
		// without a mode is a copy.
		from, to := *op.Source, *op.Target
		from.Tab, to.Tab = op.Tab, op.Tab
		if op.Mode == "auto" || op.Mode == "series" {
			return x.fillLines(t, &from, &to, op.Mode == "series")
		}
		return x.copyOp(&from, &to, "all", false)
	case "insert_rows", "insert_cols":
		rows := op.Op == "insert_rows"
		a := x.axisOf(t, rows)
		at, ok := 0, false
		count := countOf(op.Count)
		if pos, live := a.pos[uint64(op.Beside)]; live {
			at, ok = int(pos), true
			if op.Side == "after" {
				at++
			}
		} else {
			at, ok = x.log.placeBeside(t, rows, uint64(op.Beside), x.base)
		}
		if !ok {
			return 0, 0, errGone
		}
		if _, err := x.insertAxis(t, rows, min(at, a.len()), count); err != nil {
			return 0, 0, err
		}
		return count, 0, nil
	case "append_rows", "append_cols":
		rows := op.Op == "append_rows"
		count := countOf(op.Count)
		if _, err := x.insertAxis(t, rows, x.axisOf(t, rows).len(), count); err != nil {
			return 0, 0, err
		}
		return count, 0, nil
	case "delete_rows", "delete_cols":
		rows := op.Op == "delete_rows"
		limit := formula.MaxCols
		if rows {
			limit = formula.MaxRows
		}
		spec, err := op.IDs.spec(limit)
		if err != nil {
			return 0, 0, err
		}
		ids, gone, err := x.resolve(t, rows, spec, false)
		if err != nil {
			return 0, 0, err
		}
		n, err := x.deleteIDs(t, rows, ids)
		return n, gone, errOrNil(err)
	case "sort":
		spec, err := op.Rows.spec(formula.MaxRows)
		if err != nil {
			return 0, 0, err
		}
		if !x.live(&t.cols, uint64(op.By)) {
			return 0, 0, errGone
		}
		ids, gone, err := x.resolve(t, true, spec, false)
		if err != nil {
			return 0, 0, err
		}
		var positions []int32
		for _, id := range ids {
			if id != 0 {
				positions = append(positions, t.rows.pos[id])
			}
		}
		if err := x.sortPositions(positions); err != nil {
			return 0, 0, err
		}
		positions = unfrozen(t, slices.Compact(positions))
		if err := x.sortRows(t, positions, ColID(op.By), op.Desc); err != nil {
			return 0, 0, err
		}
		return len(positions), gone, nil
	case "settings":
		// The shape of settings gives it both counts.
		fr, fc := countOf(op.FrozenRows), countOf(op.FrozenCols)
		if err := checkFrozen(&fr, &fc); err != nil {
			return 0, 0, err
		}
		// Settings, a name and a place in the tab order that the tab has
		// already change nothing.
		if fr != t.frozenRows || fc != t.frozenCols {
			x.setSettings(t, fr, fc)
		}
		return 1, 0, nil
	case "rename_tab":
		if op.Name == t.name {
			return 1, 0, nil
		}
		return 1, 0, errOrNil(x.renameTab(t, op.Name))
	case "move_tab":
		at := len(wb.tabs)
		if op.carries(sOpBefore) && !op.beforeNull {
			before := wb.byID[TabID(op.Before)]
			if before == nil {
				return 0, 0, errGone
			}
			if at = wb.tabIndex(before); at > wb.tabIndex(t) {
				at--
			}
		}
		if min(at, len(wb.tabs)-1) != wb.tabIndex(t) {
			x.moveTab(t, at)
		}
		return 1, 0, nil
	case "delete_tab":
		return 1, 0, errOrNil(x.deleteTab(t))
	}
	return 0, 0, invalidf("op", "unknown operation %q", op.Op)
}

// countOf is a count of an operation as an int. A count past every limit
// of a tab stays past them, without wrapping the sums it is added to.
func countOf(n int64) int { return int(max(math.MinInt32, min(n, math.MaxInt32))) }

func errOrNil(err *WriteError) error {
	if err == nil {
		return nil
	}
	return err
}

// overCells calls fn for the cells of rows and cols. With create false it
// visits only the cells that have a record, so that clearing or resetting a
// large selection costs what it holds.
func (x *txn) overCells(t *Tab, rows, cols []uint64, create bool, fn func(RowID, ColID)) (int, *WriteError) {
	n := 0
	if create {
		if len(rows)*len(cols) > maxCells {
			return 0, capacity("cells", maxCells, int64(len(rows)*len(cols)))
		}
		for _, row := range rows {
			if err := x.stop(); err != nil {
				return 0, err
			}
			for _, col := range cols {
				if row != 0 && col != 0 {
					fn(RowID(row), ColID(col))
					n++
				}
			}
		}
		return n, nil
	}
	want := make(map[ColID]bool, len(cols))
	for _, col := range cols {
		want[ColID(col)] = true
	}
	var hit []ColID
	for _, row := range rows {
		// Looking a row up is a unit, and so is each record of it that
		// is looked at for its column.
		rd := t.data[RowID(row)]
		if rd == nil {
			x.spend(64)
			if x.late() {
				return 0, calcTimeout()
			}
			continue
		}
		x.spend(64 * (1 + len(rd.cells)))
		if err := x.stop(); err != nil {
			return 0, err
		}
		hit = hit[:0]
		for _, s := range rd.cells {
			if want[s.col] {
				hit = append(hit, s.col)
			}
		}
		for _, col := range hit {
			fn(RowID(row), col)
			n++
		}
	}
	return n, nil
}

// deleteIDs deletes the live ids, run by run from the end of the axis so
// that positions stay valid.
func (x *txn) deleteIDs(t *Tab, rows bool, ids []uint64) (int, *WriteError) {
	a := x.axisOf(t, rows)
	var positions []int
	for _, id := range ids {
		if id != 0 {
			positions = append(positions, int(a.pos[id]))
		}
	}
	sort.Ints(positions)
	positions = slices.Compact(positions)
	if len(positions) == a.len() {
		return 0, invalidf("empty_tab", "a tab keeps at least one row and one column")
	}
	for end := len(positions); end > 0; {
		// Each run is a structural edit of its own: it holds what one
		// operation holds, and the deadline is looked at before it.
		if err := x.stop(); err != nil {
			return 0, err
		}
		x.keep(journalOpBytes)
		start := end - 1
		for start > 0 && positions[start-1] == positions[start]-1 {
			start--
		}
		if err := x.deleteAxis(t, rows, positions[start], end-start); err != nil {
			return 0, err
		}
		end = start
	}
	return len(positions), nil
}

// copyOp copies, fills or cuts a source rectangle over a target, repeating
// the source when the target is larger (spec rule 15).
func (x *txn) copyOp(from, to *rectSpec, mode string, cut bool) (applied, ignored int, err error) {
	switch {
	case from == nil:
		return 0, 0, invalidf("from", "copy needs the cells to copy from")
	case to == nil:
		return 0, 0, invalidf("to", "copy needs the cells to copy to")
	case mode != "all" && mode != "values":
		return 0, 0, invalidf("mode", "mode is all or values")
	}
	if cut {
		// A second write to one destination replaces the first while the
		// cut clears both sources, which loses a cell.
		if werr := x.namesOnce(&to.Rows, formula.MaxRows); werr != nil {
			return 0, 0, werr
		}
		if werr := x.namesOnce(&to.Cols, formula.MaxCols); werr != nil {
			return 0, 0, werr
		}
	}
	ft, gerr := x.personTab(uint64(from.Tab))
	if gerr != nil {
		return 0, 0, gerr
	}
	tt, gerr := x.personTab(uint64(to.Tab))
	if gerr != nil {
		return 0, 0, gerr
	}
	srcRows, srcCols, _, werr := x.rect(ft, &from.Rows, &from.Cols, false)
	if werr != nil {
		return 0, 0, werr
	}
	if len(srcRows) == 0 || len(srcCols) == 0 {
		return 0, 0, invalidf("from", "the source names no cell")
	}
	if n := len(srcRows) * len(srcCols); n > maxCells {
		return 0, 0, capacity("cells", maxCells, int64(n))
	}
	// Values are those of this point of the action: an earlier operation
	// of it may have written what a source formula reads.
	if mode == "values" {
		if err := x.calc(); err != nil {
			return 0, 0, err
		}
	}
	src := make([]copySource, len(srcRows)*len(srcCols))
	for i, row := range srcRows {
		// Reading a source cell is a unit.
		x.spend(64 * len(srcCols))
		if x.late() {
			return 0, 0, calcTimeout()
		}
		for j, col := range srcCols {
			src[i*len(srcCols)+j] = x.readSource(ft, row, col)
		}
	}
	rowsBefore, colsBefore := tt.rows.len(), tt.cols.len()
	dstRows, dstCols, _, werr := x.rect(tt, &to.Rows, &to.Cols, true)
	if werr != nil {
		return 0, 0, werr
	}
	if len(dstRows)*len(dstCols) > maxCells {
		return 0, 0, capacity("cells", maxCells, int64(len(dstRows)*len(dstCols)))
	}
	var plan *cutPlan
	if cut {
		if plan, werr = x.planCut(ft == tt, srcRows, srcCols, dstRows, dstCols, func(k int) bool { return src[k].ok }); werr != nil {
			return 0, 0, werr
		}
	}
	for i, row := range dstRows {
		for j, col := range dstCols {
			// A target is the source many times over. Each cell of it is
			// a unit, what is written to it is counted where it is
			// written, and the deadline, or the bound of what the action
			// holds, ends the copy.
			x.spend(64)
			if err := x.stop(); err != nil {
				return 0, 0, err
			}
			k := (i%len(srcRows))*len(srcCols) + j%len(srcCols)
			s := &src[k]
			if row == 0 || col == 0 || !s.ok || cut && plan.held(i*len(dstCols)+j) {
				ignored++
				continue
			}
			applied++
			if werr := x.writeCopy(ft, tt, RowID(row), ColID(col), s, mode, cut); werr != nil {
				return 0, 0, werr
			}
		}
	}
	// Rows and columns the target added stay only for a cell written into
	// them. When no cell applied the operation is ignored as gone, which
	// takes them back.
	if applied == 0 && (tt.rows.len() != rowsBefore || tt.cols.len() != colsBefore) {
		return 0, 0, errGone
	}
	if cut {
		none := formatPatch{mask: 1<<propCount - 1}
		for i, row := range srcRows {
			x.spend(64 * len(srcCols))
			if err := x.stop(); err != nil {
				return 0, 0, err
			}
			for j, col := range srcCols {
				r, c := RowID(row), ColID(col)
				if !plan.clears(i*len(srcCols)+j) || ft.cell(r, c) == nil {
					continue
				}
				x.clearContent(ft, r, c)
				if mode == "all" && ft.cell(r, c).style != 0 {
					x.setStyle(ft, r, c, none)
				}
			}
		}
	}
	return applied, ignored, nil
}

// copySource is a source cell of a copy, a fill or a cut, as the operation
// read it. It is not ok for a cell of a row or a column that is gone.
type copySource struct {
	ok      bool
	content formula.Content
	f       *formula.Formula
	val     formula.Value
	format  formula.Format
	pos     formula.Pos
}

// readSource reads the source cell of row and col of t, ids that resolve
// gave, with 0 for one that is gone.
func (x *txn) readSource(t *Tab, row, col uint64) copySource {
	if row == 0 || col == 0 {
		return copySource{}
	}
	s := copySource{ok: true, pos: x.posOf(t, RowID(row), ColID(col))}
	if c := t.cell(RowID(row), ColID(col)); c != nil {
		s.content, s.f, s.val, s.format = c.content, c.f, c.val, x.wb.styles.format(c.style)
	}
	return s
}

// writeCopy writes source s, read from tab ft, into the cell of tt at row r
// and column c as a copy does (spec rule 15): its content or, in mode
// values, its value, a formula moved or cut from its source cell, and in
// mode all its format.
func (x *txn) writeCopy(ft, tt *Tab, r RowID, c ColID, s *copySource, mode string, cut bool) *WriteError {
	if tt.cell(r, c) == nil && s.content.Kind == formula.ContentBlank && s.format == (formula.Format{}) {
		return nil
	}
	content, f := s.content, s.f
	if mode == "values" {
		content, f = valueContent(s.content, s.val), nil
	} else if f != nil {
		dst := x.posOf(tt, r, c)
		if cut {
			f = f.Cut(s.pos, dst, ft.name, tt.name)
		} else {
			f = f.Moved(s.pos, dst)
		}
		// The copy is also measured at its cell and looked up among the
		// formulas of the sheet: its text twice.
		x.rewrote(s.f, f)
		x.spend(2 * len(f.Stored()))
		if n := f.TextLen(dst); n > formula.MaxFormulaChars {
			return capacity("formula_length", formula.MaxFormulaChars, int64(n))
		}
	}
	if content.Kind == formula.ContentBlank {
		x.clearContent(tt, r, c)
	} else {
		x.setContent(tt, r, c, content, f)
	}
	if mode == "all" {
		x.copyFormat(tt, r, c, s.format)
	}
	return nil
}

// copyFormat gives a cell the format of its source, as a copy in mode all
// does. A cell without a record and a format of its own keeps none.
func (x *txn) copyFormat(t *Tab, r RowID, c ColID, format formula.Format) {
	if format != (formula.Format{}) || t.cell(r, c) != nil && t.cell(r, c).style != 0 {
		x.setStyle(t, r, c, formatPatch{mask: 1<<propCount - 1, f: format})
	}
}

// fillLines is a fill in mode auto or, with step, in mode series (spec rule
// 21a, technical design, section 8.5). The target lies on one side of the
// source: below or above it in the same columns, and each column is a
// line, or right or left of it in the same rows, and each row is a line.
// Any other target is invalid. Where the target lies is read from the rows
// or columns that are there; one that is gone is left out.
//
// The source cells of a line, in the order the operation names them, go to
// formula.FillSeries. A line without a series is a copy of its own source
// cells, which repeat in order along the line, each formula moved from its
// source cell. A line with a series takes the content of each target cell
// from it, counted from the cell next to the source outward. Either way a
// target cell takes the format of the source cell a copy writes there. A
// line whose source has a cell that is gone has no series, and the target
// cells of that source cell are ignored, as in a copy.
//
// A fill is checked and counted as a copy over the same target: each
// target cell is a unit, and each text of a series counts its bytes.
func (x *txn) fillLines(t *Tab, source, target *rectSpec, step bool) (applied, ignored int, err error) {
	srcRows, srcCols, _, werr := x.rect(t, &source.Rows, &source.Cols, false)
	if werr != nil {
		return 0, 0, werr
	}
	if len(srcRows) == 0 || len(srcCols) == 0 {
		return 0, 0, invalidf("source", "the source names no cell")
	}
	if n := len(srcRows) * len(srcCols); n > maxCells {
		return 0, 0, capacity("cells", maxCells, int64(n))
	}
	rowsBefore, colsBefore := t.rows.len(), t.cols.len()
	dstRows, dstCols, _, werr := x.rect(t, &target.Rows, &target.Cols, true)
	if werr != nil {
		return 0, 0, werr
	}
	if n := len(dstRows) * len(dstCols); n > maxCells {
		return 0, 0, capacity("cells", maxCells, int64(n))
	}
	down := slices.Equal(srcCols, dstCols)
	var back, ok bool
	switch {
	case down:
		back, ok = x.side(&t.rows, srcRows, dstRows)
	case slices.Equal(srcRows, dstRows):
		back, ok = x.side(&t.cols, srcCols, dstCols)
	}
	if x.late() {
		return 0, 0, calcTimeout()
	}
	if !ok {
		return 0, 0, invalidf("target", "a fill in auto or series fills below or above its source in its columns, or right or left of it in its rows")
	}
	// A line has m source cells and n target cells.
	m, n, lines := len(srcRows), len(dstRows), len(srcCols)
	if !down {
		m, n, lines = len(srcCols), len(dstCols), len(srcRows)
	}
	at := func(rows, cols []uint64, line, i int) (uint64, uint64) {
		if down {
			return rows[i], cols[line]
		}
		return rows[line], cols[i]
	}
	src := make([]copySource, m)
	contents := make([]formula.Content, m)
	var text int64 // code points the series of texts write, at least
	for line := 0; line < lines; line++ {
		// Reading a source cell is a unit.
		x.spend(64 * m)
		if x.late() {
			return 0, 0, calcTimeout()
		}
		whole := true
		for i := range src {
			row, col := at(srcRows, srcCols, line, i)
			src[i] = x.readSource(t, row, col)
			contents[i], whole = src[i].content, whole && src[i].ok
		}
		var series []formula.Content
		if whole {
			if series, werr = x.series(contents, n, back, step, &text); werr != nil {
				return 0, 0, werr
			}
		}
		for i := 0; i < n; i++ {
			// Each target cell is a unit, what is written to it is
			// counted where it is written, and the deadline, or the bound
			// of what the action holds, ends the fill.
			x.spend(64)
			if err := x.stop(); err != nil {
				return 0, 0, err
			}
			row, col := at(dstRows, dstCols, line, i)
			s := &src[i%m]
			if row == 0 || col == 0 || !s.ok {
				ignored++
				continue
			}
			r, c := RowID(row), ColID(col)
			applied++
			if series == nil {
				if werr := x.writeCopy(t, t, r, c, s, "all", false); werr != nil {
					return 0, 0, werr
				}
				continue
			}
			k := i
			if back {
				k = n - 1 - i // the target is named top down or left to right
			}
			content := series[k]
			if content.Kind == formula.ContentText {
				// The text was built for this cell.
				x.spend(len(content.Text))
				if werr := checkText(content.Text); werr != nil {
					return 0, 0, werr
				}
			}
			x.setContent(t, r, c, content, nil)
			x.copyFormat(t, r, c, s.format)
		}
	}
	// As for a copy: added rows and columns stay only with a cell.
	if applied == 0 && (t.rows.len() != rowsBefore || t.cols.len() != colsBefore) {
		return 0, 0, errGone
	}
	return applied, ignored, nil
}

// side tells where the target ids lie on an axis beside the source ids:
// after them, or before them with back. It is not ok when a target id lies
// between the source ids or on one of them. Ids that are gone are left
// out. Each id looked up is a unit, as in resolve.
func (x *txn) side(a *axis, src, dst []uint64) (back, ok bool) {
	lo, hi := int32(math.MaxInt32), int32(-1)
	for _, id := range src {
		x.spend(64)
		if p, live := a.pos[id]; live {
			lo, hi = min(lo, p), max(hi, p)
		}
	}
	before, after := false, false
	for _, id := range dst {
		x.spend(64)
		p, live := a.pos[id]
		switch {
		case !live:
		case p > hi:
			after = true
		case p < lo:
			before = true
		default:
			return false, false
		}
	}
	return before, !(before && after)
}

// series is formula.FillSeries for the source contents of one line and n
// target cells, or nil for a line without a series. Every text of a series
// of texts that end in a number holds the text before that number, and
// text counts those code points over the lines of the fill. A fill whose
// series pass the typed text of a sheet is refused before they are built,
// with the limit its end would find.
func (x *txn) series(src []formula.Content, n int, back, step bool, text *int64) ([]formula.Content, *WriteError) {
	if _, ok := formula.FillSeries(src, 1, back, step); !ok {
		return nil, nil
	}
	if last := src[len(src)-1].Text; src[len(src)-1].Kind == formula.ContentText && last != "" && last[len(last)-1] >= '0' && last[len(last)-1] <= '9' {
		stem := strings.TrimRight(last, "0123456789")
		x.spend(len(stem))
		if *text += int64(n) * int64(utf8.RuneCountInString(stem)); *text > maxSheetText {
			return nil, capacity("sheet_text", maxSheetText, *text)
		}
	}
	// A date series can pass the last date further out: no series then.
	out, _ := formula.FillSeries(src, n, back, step)
	return out, nil
}

// namesOnce refuses a list that names a row or a column twice: an id,
// whether it is still there or not, or the same {"new": k}. The list as it
// was sent decides, so the answer does not depend on what other writers
// deleted meanwhile. A list that is no list of ids is left to rect.
func (x *txn) namesOnce(l *idList, limit int) *WriteError {
	spec, err := l.spec(limit)
	if err != nil || spec.all {
		return nil
	}
	seen := make(map[target]struct{}, len(spec.items))
	for _, it := range spec.items {
		// Each id looked up is a unit, as in resolve.
		x.spend(64)
		if x.late() {
			return calcTimeout()
		}
		if _, twice := seen[it]; twice {
			return invalidf("to", "a cut names each row and column of its destination once")
		}
		seen[it] = struct{}{}
	}
	return nil
}

// cutPlan says which cells a cut moves (spec rule 32). A source cell moves
// when one of its destinations applies, and one whose every destination
// was deleted stays where it was. The cut writes nothing over a cell that
// stays. A destination on it does not apply, which can leave the source of
// that destination with none, so that one stays too, until nothing
// changes. Sources and destinations are numbered row by row.
type cutPlan struct {
	srcRows, srcCols, dstCols int
	// first is, for each source, the first source on the same cell, which
	// stands for the cell, or -1 for a source that is gone. A list may name
	// a row or a column twice, so a cell can be more than one source.
	first []int32
	// moves counts, for each cell, its destinations that apply.
	moves []int32
	// onto and next list, for each cell, the destinations on it, numbered
	// from 1 with 0 for none. hold marks a destination that does not apply
	// because a cell that stays is under it. They are nil for a cut into
	// another tab, where no destination is on a source.
	onto, next []int32
	hold       []bool
}

// planCut works out the plan of a cut. same says that the source and the
// target are in one tab, and ok whether a source is still there.
func (x *txn) planCut(same bool, srcRows, srcCols, dstRows, dstCols []uint64, ok func(k int) bool) (*cutPlan, *WriteError) {
	p := &cutPlan{srcRows: len(srcRows), srcCols: len(srcCols), dstCols: len(dstCols)}
	n := len(srcRows) * len(srcCols)
	p.first, p.moves = make([]int32, n), make([]int32, n)
	// firstAt maps each id of a list to its first place. Each id looked up
	// is a unit, as in resolve.
	firstAt := func(ids []uint64) map[uint64]int32 {
		at := make(map[uint64]int32, len(ids))
		for i, id := range ids {
			x.spend(64)
			if _, seen := at[id]; !seen && id != 0 {
				at[id] = int32(i)
			}
		}
		return at
	}
	rowAt, colAt := firstAt(srcRows), firstAt(srcCols)
	if x.late() {
		return nil, calcTimeout()
	}
	for i, row := range srcRows {
		x.spend(64 * len(srcCols))
		if x.late() {
			return nil, calcTimeout()
		}
		for j, col := range srcCols {
			k := i*len(srcCols) + j
			p.first[k] = -1
			if ok(k) {
				p.first[k] = rowAt[row]*int32(len(srcCols)) + colAt[col]
			}
		}
	}
	if same {
		p.onto, p.next, p.hold = make([]int32, n), make([]int32, len(dstRows)*len(dstCols)), make([]bool, len(dstRows)*len(dstCols))
	}
	for i, row := range dstRows {
		x.spend(64 * len(dstCols))
		if x.late() {
			return nil, calcTimeout()
		}
		for j, col := range dstCols {
			d := i*len(dstCols) + j
			u := p.first[p.source(d)]
			if row == 0 || col == 0 || u < 0 {
				continue
			}
			p.moves[u]++
			if !same {
				continue
			}
			ri, onRow := rowAt[row]
			ci, onCol := colAt[col]
			if onRow && onCol {
				on := ri*int32(len(srcCols)) + ci
				p.next[d], p.onto[on] = p.onto[on], int32(d)+1
			}
		}
	}
	if !same {
		return p, nil
	}
	// A look at a cell's count is a step.
	x.spend(n)
	if x.late() {
		return nil, calcTimeout()
	}
	var stay []int32
	for u, f := range p.first {
		if f == int32(u) && p.moves[u] == 0 {
			stay = append(stay, f)
		}
	}
	// Each destination is held at most once, so this ends after as many
	// steps as the cut has destinations.
	for len(stay) > 0 {
		u := stay[len(stay)-1]
		stay = stay[:len(stay)-1]
		for d := p.onto[u]; d != 0; d = p.next[d-1] {
			x.spend(64)
			if x.late() {
				return nil, calcTimeout()
			}
			p.hold[d-1] = true
			v := p.first[p.source(int(d-1))]
			if p.moves[v]--; p.moves[v] == 0 {
				stay = append(stay, v)
			}
		}
	}
	return p, nil
}

// source is the source a destination copies: the source repeats over a
// larger target.
func (p *cutPlan) source(d int) int {
	return (d/p.dstCols%p.srcRows)*p.srcCols + d%p.dstCols%p.srcCols
}

// held reports whether destination d does not apply because a cell that
// stays is under it.
func (p *cutPlan) held(d int) bool { return p.hold != nil && p.hold[d] }

// clears reports whether the cut empties source k: its cell moved and no
// destination of the cut was written over it.
func (p *cutPlan) clears(k int) bool {
	u := p.first[k]
	return u >= 0 && p.moves[u] > 0 && (p.onto == nil || p.onto[u] == 0)
}

// valueContent is what "paste values only" writes for a cell: the content
// of a plain cell, and the calculated value of a formula.
func valueContent(c formula.Content, v formula.Value) formula.Content {
	if !c.IsFormula() {
		return c
	}
	switch v.Kind() {
	case formula.KindNumber:
		return formula.Content{Kind: formula.ContentNumber, Num: v.Number()}
	case formula.KindText:
		return formula.Content{Kind: formula.ContentText, Text: v.Text()}
	case formula.KindBool:
		n := 0.0
		if v.Bool() {
			n = 1
		}
		return formula.Content{Kind: formula.ContentBool, Num: n}
	case formula.KindError:
		return formula.Content{Kind: formula.ContentText, Text: v.Err().String()}
	}
	return formula.Content{}
}
