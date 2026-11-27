package sheet

import (
	"strings"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// The reference model of the technical design, section 22.1: tabs as plain
// grids of cells by address, the same operations in receive order, and a
// full recalculation after each one. It shares the formula package with
// the engine and nothing else: no ids, no journal, no index, no database.

type modelCell struct {
	content formula.Content
	f       *formula.Formula
	val     formula.Value

	// What the state-machine model adds (statemodel_test.go): the format
	// and the stamps of the cell's value groups.
	format         formula.Format
	cstamp, tstamp uint64
	pstamp         [propCount]uint64
}

type modelTab struct {
	name string
	grid [][]*modelCell // rows of cells; nil for a blank cell
	cols int
}

type model struct {
	tabs []*modelTab
	date formula.Date
}

func (m *model) clone() *model {
	c := &model{date: m.date}
	for _, t := range m.tabs {
		nt := &modelTab{name: t.name, cols: t.cols}
		for _, row := range t.grid {
			nr := make([]*modelCell, len(row))
			for i, cell := range row {
				if cell != nil {
					copied := *cell
					nr[i] = &copied
				}
			}
			nt.grid = append(nt.grid, nr)
		}
		c.tabs = append(c.tabs, nt)
	}
	return c
}

// Grid of the formula package.
func (m *model) Tab(name string) (formula.TabRef, bool) {
	for i, t := range m.tabs {
		if strings.EqualFold(t.name, name) {
			return formula.TabRef(i), true
		}
	}
	return 0, false
}

func (m *model) Dims(ref formula.TabRef) (int32, int32) {
	t := m.tabs[ref]
	return int32(len(t.grid)), int32(t.cols)
}

func (m *model) cell(ref formula.TabRef, p formula.Pos) *modelCell {
	t := m.tabs[ref]
	if p.Row < 0 || p.Col < 0 || int(p.Row) >= len(t.grid) || int(p.Col) >= t.cols {
		return nil
	}
	return t.grid[p.Row][p.Col]
}

func (m *model) Value(ref formula.TabRef, p formula.Pos) formula.Value {
	if c := m.cell(ref, p); c != nil {
		return c.val
	}
	return formula.Value{}
}

func (m *model) Filled(ref formula.TabRef, p formula.Pos) bool {
	c := m.cell(ref, p)
	return c != nil && c.content.Kind != formula.ContentBlank
}

type modelAt struct {
	tab int
	pos formula.Pos
}

// recalc evaluates every formula from scratch: strongly connected groups
// of the read graph, found by a plain recursive walk, are cycles.
func (m *model) recalc() {
	var nodes []modelAt
	index := map[modelAt]int{}
	for ti, t := range m.tabs {
		for r, row := range t.grid {
			for c, cell := range row {
				if cell != nil && cell.f != nil {
					at := modelAt{ti, formula.Pos{Row: int32(r), Col: int32(c)}}
					index[at] = len(nodes)
					nodes = append(nodes, at)
				}
			}
		}
	}
	reads := make([][]int, len(nodes))
	for i, at := range nodes {
		seen := map[int]bool{}
		m.cell(formula.TabRef(at.tab), at.pos).f.Refs(at.pos, func(r formula.Ref) {
			ti := at.tab
			if r.Tab != "" {
				ref, ok := m.Tab(r.Tab)
				if !ok {
					return
				}
				ti = int(ref)
			}
			t := m.tabs[ti]
			for row := r.First.Row; row <= min(r.Last.Row, int32(len(t.grid))-1); row++ {
				for col := r.First.Col; col <= min(r.Last.Col, int32(t.cols)-1); col++ {
					if j, ok := index[modelAt{ti, formula.Pos{Row: row, Col: col}}]; ok && !seen[j] {
						seen[j] = true
						reads[i] = append(reads[i], j)
					}
				}
			}
		})
	}
	// Tarjan, recursive: the model's sheets are small.
	low, num, onStack := make([]int, len(nodes)), make([]int, len(nodes)), make([]bool, len(nodes))
	cyclic := make([]bool, len(nodes))
	var stack, order []int
	next := 0
	var visit func(i int)
	visit = func(i int) {
		next++
		num[i], low[i] = next, next
		stack = append(stack, i)
		onStack[i] = true
		for _, j := range reads[i] {
			switch {
			case j == i:
				cyclic[i] = true
			case num[j] == 0:
				visit(j)
				low[i] = min(low[i], low[j])
			case onStack[j]:
				low[i] = min(low[i], num[j])
			}
		}
		if low[i] != num[i] {
			return
		}
		at := len(stack) - 1
		for stack[at] != i {
			at--
		}
		group := stack[at:]
		for _, j := range group {
			onStack[j] = false
			cyclic[j] = cyclic[j] || len(group) > 1
		}
		order = append(order, group...)
		stack = stack[:at]
	}
	for i := range nodes {
		if num[i] == 0 {
			visit(i)
		}
	}
	for _, i := range order {
		at := nodes[i]
		cell := m.cell(formula.TabRef(at.tab), at.pos)
		if cyclic[i] {
			cell.val = formula.ErrorValue(formula.ErrCycle)
			continue
		}
		cell.val = cell.f.Eval(&formula.Env{Grid: m, Tab: formula.TabRef(at.tab), At: at.pos, Today: m.date})
	}
}
