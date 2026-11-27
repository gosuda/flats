package sheet

import (
	"fmt"
	"slices"
	"strings"
)

// dump renders the durable state of the sheet as text: the counters, every
// tab with its axes, settings and widths, and every cell record with its
// content, its calculated value, its format and its stamps. Two dumps are
// equal exactly when a restart, or any path that claims to leave the sheet
// as it was, kept all of it. The undo stacks, the structural log and the
// delta log live in memory only and are not part of it.
func (e *env) dump() string {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	return dumpWorkbook(e.in.wb, true)
}

// dumpWorkbook renders a workbook. With stamps false it leaves out the
// sequences, so that two workbooks that reached the same content by
// different histories compare equal.
func dumpWorkbook(wb *Workbook, stamps bool) string {
	var b strings.Builder
	stamp := func(format string, args ...any) {
		if stamps {
			fmt.Fprintf(&b, format, args...)
		}
	}
	fmt.Fprintf(&b, "date %s limited %v\n", wb.date, wb.limited)
	stamp("seq %d reset %d tab_change %d tab_order %d guard_floor %d next %d/%d/%d\n",
		wb.seq, wb.resetSeq, wb.tabChangeSeq, wb.tabOrderSeq, wb.guardFloor, wb.nextTab, wb.nextRow, wb.nextCol)
	for _, t := range wb.tabs {
		fmt.Fprintf(&b, "tab %d %q frozen %d/%d rows %v cols %v\n", t.id, t.name, t.frozenRows, t.frozenCols, idRuns(t.rows.ids), idRuns(t.cols.ids))
		stamp("  born %d name %d settings %d struct %d row_born %v col_born %v\n", t.born, t.nameSeq, t.settingsSeq, t.structSeq, t.rows.born, t.cols.born)
		cols := make([]ColID, 0, len(t.widths))
		for col := range t.widths {
			cols = append(cols, col)
		}
		slices.Sort(cols)
		for _, col := range cols {
			if w := t.widths[col]; w.px != defaultWidth || stamps {
				fmt.Fprintf(&b, "  width %d = %d", col, w.px)
				stamp(" @%d", w.seq)
				b.WriteByte('\n')
			}
		}
		for _, row := range t.rows.ids {
			rd := t.data[RowID(row)]
			if rd == nil {
				continue
			}
			for _, s := range rd.cells {
				c := s.c
				if !stamps && c.blank() && c.style == 0 {
					continue // a blank record is kept for its stamps alone
				}
				state := wb.styles.state(c.style)
				format, _ := appendFormat(nil, state.f)
				fmt.Fprintf(&b, "  cell %d/%d kind %d num %v text %q value %+v format %s", row, s.col, c.content.Kind, c.content.Num, c.content.Text, c.val, format)
				stamp(" @%d text@%d format@%v", c.seq, c.textSeq, state.stamps)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// dumpDiff returns the first line at which two dumps differ.
func dumpDiff(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < max(len(x), len(y)); i++ {
		var l, r string
		if i < len(x) {
			l = x[i]
		}
		if i < len(y) {
			r = y[i]
		}
		if l != r {
			return fmt.Sprintf("line %d:\n  %s\n  %s", i+1, l, r)
		}
	}
	return ""
}
