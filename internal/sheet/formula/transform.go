package formula

import "unicode/utf8"

// TransformKind names a structural edit.
type TransformKind uint8

const (
	InsertRows TransformKind = iota + 1
	DeleteRows
	InsertCols
	DeleteCols
	RenameTab
)

// Transform is a structural edit that rewrites references (technical
// design, section 8.2).
//
// For the insert and delete kinds Tab is the tab whose rows or columns
// change, At the zero-based first row or column inserted or deleted and
// Count their number: an insert puts Count rows before row At. For
// RenameTab, Tab is the old name and Name the new one. Tab names compare
// without regard to capitals.
//
// A sort, a new tab and a deleted tab change no stored form and need no
// Transform.
type Transform struct {
	Kind  TransformKind
	Tab   string
	At    int32
	Count int32
	Name  string
}

func (t Transform) rows() bool { return t.Kind == InsertRows || t.Kind == DeleteRows }
func (t Transform) cols() bool { return t.Kind == InsertCols || t.Kind == DeleteCols }

// index returns where row or column i of the edited tab lies after the
// edit. ok is false when the edit deletes it.
func (t Transform) index(i int32) (j int32, ok bool) {
	switch t.Kind {
	case InsertRows, InsertCols:
		if i >= t.At {
			return i + t.Count, true
		}
	case DeleteRows, DeleteCols:
		if i >= t.At+t.Count {
			return i - t.Count, true
		}
		if i >= t.At {
			return i, false
		}
	}
	return i, true
}

// span maps the rows or columns lo to hi, lo at or before hi. A range
// grows when the edit inserts between its ends and is cut to what a delete
// leaves. ok is false when nothing is left.
func (t Transform) span(lo, hi int32) (newLo, newHi int32, ok bool) {
	newLo, okLo := t.index(lo)
	newHi, okHi := t.index(hi)
	if !okLo {
		newLo = t.At // the first row after the deleted ones
	}
	if !okHi {
		newHi = t.At - 1 // the last row before them
	}
	return newLo, newHi, newLo <= newHi
}

// Map returns where the cell p of the tab named tab lies after the edit.
// ok is false when the edit deletes the cell.
func (t Transform) Map(tab string, p Pos) (q Pos, ok bool) {
	if t.Kind == RenameTab || !equalFold(tab, t.Tab) {
		return p, true
	}
	if t.rows() {
		p.Row, ok = t.index(p.Row)
		return p, ok
	}
	p.Col, ok = t.index(p.Col)
	return p, ok
}

// rewrite returns the formula with each slot replaced by what edit
// returns. A slot edit reports as dead becomes the literal #REF!. When no
// slot changes it returns f itself.
func (f *Formula) rewrite(edit func(s slot) (slot, bool)) *Formula {
	var slots []slot
	var dead []bool
	for i := range f.slots {
		s, d := edit(f.slots[i])
		if !d && s == f.slots[i] {
			continue
		}
		if slots == nil {
			slots = append([]slot(nil), f.slots...)
			dead = make([]bool, len(f.slots))
		}
		slots[i], dead[i] = s, d
	}
	if slots == nil {
		return f
	}
	dst := make([]byte, 0, len(f.stored)+16)
	for i := range slots {
		dst = append(dst, f.lits[i]...)
		if dead[i] {
			dst = append(dst, "#REF!"...)
		} else {
			dst = slots[i].appendStored(dst)
		}
	}
	dst = append(dst, f.lits[len(slots)]...)
	stored := string(dst)
	g, err := parseStored(stored)
	if err != nil {
		// The lexer read the same text around the slots before, and a slot
		// or #REF! between two pieces never joins them.
		return newUnreadable(stored, utf8.RuneCountInString(stored))
	}
	return g
}

// place sets the corners of s to the resolved rows and columns r for a
// formula in the cell at, keeping which parts are fixed.
func (s *slot) place(r resolved, at Pos) {
	set := func(c *corner, row, col int32) {
		c.row, c.col = row, col
		if !c.rowAbs {
			c.row -= at.Row
		}
		if !c.colAbs {
			c.col -= at.Col
		}
	}
	set(&s.a, r.r1, r.c1)
	switch s.kind {
	case slotCell:
		return
	case slotCols:
		s.a.row = 0
	case slotRows:
		s.a.col = 0
	}
	set(&s.b, r.r2, r.c2)
	switch s.kind {
	case slotCols:
		s.b.row = 0
	case slotRows:
		s.b.col = 0
	}
}

// mapEnds maps the two ends of a reference along one axis, in whichever
// order they were written.
func (t Transform) mapEnds(a, b int32) (newA, newB int32, ok bool) {
	if a <= b {
		return t.span(a, b)
	}
	newB, newA, ok = t.span(b, a)
	return newA, newB, ok
}

// Apply rewrites the formula for the edit t. tab is the name of the tab
// that holds the formula and at its cell before the edit. It returns the
// formula after the edit, its cell after the edit and whether the stored
// form changed. When nothing changed it returns f itself.
//
// A reference to a deleted row or column, and a range that loses every row
// or column, become the literal #REF!. If the edit deletes the formula's
// own cell, Apply returns f, at and false; check with Transform.Map.
func (f *Formula) Apply(tab string, at Pos, t Transform) (*Formula, Pos, bool) {
	if t.Kind == RenameTab {
		g := f.renameTab(t.Tab, t.Name)
		return g, at, g != f
	}
	newAt, ok := t.Map(tab, at)
	if !ok || !(t.rows() || t.cols()) {
		return f, at, false
	}
	if len(f.slots) == 0 {
		return f, newAt, false
	}
	own := equalFold(tab, t.Tab)
	g := f.rewrite(func(s slot) (slot, bool) {
		hit := own
		if s.tab != "" {
			hit = equalFold(s.tabName(), t.Tab)
		}
		if !hit && newAt == at {
			return s, false
		}
		r, ok := s.resolve(at)
		if !ok {
			return s, true
		}
		if hit {
			switch {
			case t.rows() && s.kind == slotCell:
				r.r1, ok = t.index(r.r1)
			case t.rows() && s.kind != slotCols:
				r.r1, r.r2, ok = t.mapEnds(r.r1, r.r2)
			case t.cols() && s.kind == slotCell:
				r.c1, ok = t.index(r.c1)
			case t.cols() && s.kind != slotRows:
				r.c1, r.c2, ok = t.mapEnds(r.c1, r.c2)
			}
			if !ok || max(r.r1, r.r2) >= rowCap || max(r.c1, r.c2) >= colCap {
				return s, true
			}
		}
		s.place(r, newAt)
		return s, false
	})
	return g, newAt, g != f
}

func (f *Formula) renameTab(old, name string) *Formula {
	var quoted string
	return f.rewrite(func(s slot) (slot, bool) {
		if s.tab == "" || !equalFold(s.tabName(), old) {
			return s, false
		}
		if quoted == "" {
			quoted = quoteTabName(name)
		}
		s.tab = quoted
		return s, false
	})
}

func inRows(r int32) bool { return r >= 0 && r < MaxRows }
func inCols(c int32) bool { return c >= 0 && c < MaxCols }

// Moved returns the formula for a copy, a fill or a sort that takes it from
// the cell from to the cell to (spec rule 15). The stored form stays, so
// every reference without "$" shifts with the formula. A reference with a
// part that lies inside the grid limits at from and would leave them at
// to, or that would lie past what a formula can name at to, becomes the
// literal #REF!. Such a reference keeps no tab name and no position, so a
// move back does not bring it back, as in other spreadsheets, and every
// reference a formula keeps shows its tab name in the formula's text
// (MaxStoredBytes). Only the parts along an axis the move changes are
// looked at. When no reference ends it returns f itself.
func (f *Formula) Moved(from, to Pos) *Formula {
	if from == to {
		return f
	}
	leaves := func(c *corner, rows, cols bool) bool {
		if rows && !c.rowAbs && from.Row != to.Row {
			if r := to.Row + c.row; r < 0 || r >= rowCap || inRows(from.Row+c.row) && !inRows(r) {
				return true
			}
		}
		if cols && !c.colAbs && from.Col != to.Col {
			if col := to.Col + c.col; col < 0 || col >= colCap || inCols(from.Col+c.col) && !inCols(col) {
				return true
			}
		}
		return false
	}
	return f.rewrite(func(s slot) (slot, bool) {
		rows, cols := s.kind != slotCols, s.kind != slotRows
		if leaves(&s.a, rows, cols) {
			return s, true
		}
		return s, s.kind != slotCell && leaves(&s.b, rows, cols)
	})
}

// Placed returns the formula for the cell at, where an undo puts it back
// after other edits moved the place it was kept for: each reference that
// lies past what a formula can name there becomes the literal #REF!, as a
// move would make it (Moved). A reference to a tab that no tab is called
// keeps its slot like any other, since what a formula can name depends on
// rows and columns only. When every reference lies within it returns f
// itself.
func (f *Formula) Placed(at Pos) *Formula {
	return f.rewrite(func(s slot) (slot, bool) {
		_, ok := s.resolve(at)
		return s, !ok
	})
}

// Cut returns the formula for a cut from the cell from of fromTab and a
// paste into the cell to of toTab: it reads the same cells from its new
// place. When the tabs differ, each reference written without a tab name
// gains the name of fromTab. When nothing changes it returns f itself.
func (f *Formula) Cut(from, to Pos, fromTab, toTab string) *Formula {
	name := ""
	if !equalFold(fromTab, toTab) {
		name = quoteTabName(fromTab)
	}
	return f.rewrite(func(s slot) (slot, bool) {
		r, ok := s.resolve(from)
		if !ok {
			return s, true
		}
		s.place(r, to)
		if s.tab == "" {
			s.tab = name
		}
		return s, false
	})
}
