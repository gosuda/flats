package formula

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func insertRows(tab string, before, count int32) Transform {
	return Transform{Kind: InsertRows, Tab: tab, At: before - 1, Count: count}
}

func deleteRows(tab string, first, count int32) Transform {
	return Transform{Kind: DeleteRows, Tab: tab, At: first - 1, Count: count}
}

func insertCols(t *testing.T, tab, before string, count int32) Transform {
	return Transform{Kind: InsertCols, Tab: tab, At: pos(t, before+"1").Col, Count: count}
}

func deleteCols(t *testing.T, tab, first string, count int32) Transform {
	return Transform{Kind: DeleteCols, Tab: tab, At: pos(t, first+"1").Col, Count: count}
}

// applyCase is one formula in Sheet1 and what a structural edit makes of
// it. An empty wantAt means the cell does not move, and an empty want that
// the text and the stored form stay.
type applyCase struct {
	name    string
	at      string
	typed   string
	edit    Transform
	wantAt  string
	want    string
	sameRef bool // the text stays although the stored form changes
}

func runApplyCases(t *testing.T, cases []applyCase) {
	t.Helper()
	for _, c := range cases {
		at := pos(t, c.at)
		f := mustParse(t, c.typed, c.at)
		g, newAt, changed := f.Apply("Sheet1", at, c.edit)
		wantAt := at
		if c.wantAt != "" {
			wantAt = pos(t, c.wantAt)
		}
		if newAt != wantAt {
			t.Errorf("%s: the cell moves to %s, want %s", c.name, newAt.A1(), wantAt.A1())
		}
		want := c.want
		if want == "" {
			want = c.typed
		}
		if got := g.Text(newAt); got != want {
			t.Errorf("%s: %s becomes %s, want %s", c.name, c.typed, got, want)
		}
		if changed != (g.Stored() != f.Stored()) || changed != (g != f) {
			t.Errorf("%s: changed = %v with the stored forms %q and %q", c.name, changed, f.Stored(), g.Stored())
		}
		if c.want == "" && !c.sameRef && changed {
			t.Errorf("%s: the stored form changes from %q to %q", c.name, f.Stored(), g.Stored())
		}
		// The stored form after the edit is the one a fresh parse of the
		// new text gives at the new cell.
		if fresh := mustParse(t, want, newAt.A1()); fresh.Stored() != g.Stored() {
			t.Errorf("%s: stored %q, a fresh parse of %s gives %q", c.name, g.Stored(), want, fresh.Stored())
		}
		if got := g.TextLen(newAt); got != utf8.RuneCountInString(want) {
			t.Errorf("%s: TextLen = %d for %s", c.name, got, want)
		}
	}
}

// S-06 and S-07: insert rows, then columns, before the first, between, and
// after the last cell of a range.
func TestInsertAroundRange(t *testing.T) {
	runApplyCases(t, []applyCase{
		{name: "S-06 rows before the first", at: "F1", typed: "=SUM(B3:B6)", edit: insertRows("Sheet1", 2, 2), want: "=SUM(B5:B8)"},
		{name: "S-06 rows at the first", at: "F1", typed: "=SUM(B3:B6)", edit: insertRows("Sheet1", 3, 2), want: "=SUM(B5:B8)"},
		{name: "S-06 rows between", at: "F1", typed: "=SUM(B3:B6)", edit: insertRows("Sheet1", 5, 2), want: "=SUM(B3:B8)"},
		{name: "S-06 rows at the last", at: "F1", typed: "=SUM(B3:B6)", edit: insertRows("Sheet1", 6, 1), want: "=SUM(B3:B7)"},
		{name: "S-06 rows after the last", at: "F1", typed: "=SUM(B3:B6)", edit: insertRows("Sheet1", 7, 2)},
		{name: "S-06 the formula moves with its range", at: "F9", typed: "=SUM(B3:B6)", edit: insertRows("Sheet1", 1, 3), wantAt: "F12", want: "=SUM(B6:B9)"},
		{name: "S-06 the formula moves, its range stays", at: "F9", typed: "=SUM(B3:B6)", edit: insertRows("Sheet1", 8, 1), wantAt: "F10", sameRef: true},
		{name: "S-06 a row formula keeps its stored form", at: "E5", typed: "=C5*D5", edit: insertRows("Sheet1", 1, 1), wantAt: "E6", want: "=C6*D6"},
		{name: "S-06 a single cell", at: "F1", typed: "=B3*2", edit: insertRows("Sheet1", 3, 1), want: "=B4*2"},
		{name: "S-06 a range written backwards", at: "F1", typed: "=SUM(B6:B3)", edit: insertRows("Sheet1", 5, 2), want: "=SUM(B8:B3)"},
		{name: "S-07 columns before the first", at: "A9", typed: "=SUM(C3:E3)", edit: insertCols(t, "Sheet1", "B", 2), want: "=SUM(E3:G3)"},
		{name: "S-07 columns between", at: "A9", typed: "=SUM(C3:E3)", edit: insertCols(t, "Sheet1", "D", 1), want: "=SUM(C3:F3)"},
		{name: "S-07 columns after the last", at: "A9", typed: "=SUM(C3:E3)", edit: insertCols(t, "Sheet1", "F", 2)},
		{name: "S-07 the formula moves with its range", at: "H9", typed: "=SUM(C3:E3)", edit: insertCols(t, "Sheet1", "A", 1), wantAt: "I9", want: "=SUM(D3:F3)"},
		{name: "S-07 rows do not move columns apart", at: "F1", typed: "=SUM(B3:D6)", edit: insertRows("Sheet1", 4, 1), want: "=SUM(B3:D7)"},
	})
	f := mustParse(t, "=C5*D5", "E5")
	if g, _, changed := f.Apply("Sheet1", pos(t, "E5"), insertRows("Sheet1", 1, 1)); changed || g != f {
		t.Error("a formula that reads only its own row changes its stored form when a row is inserted above")
	}
}

// S-08: the same for references with "$".
func TestInsertDollarReferences(t *testing.T) {
	runApplyCases(t, []applyCase{
		{name: "S-08 fixed cell", at: "F1", typed: "=$B$3", edit: insertRows("Sheet1", 2, 2), want: "=$B$5"},
		{name: "S-08 fixed row", at: "F1", typed: "=B$3+$B3", edit: insertRows("Sheet1", 3, 1), want: "=B$4+$B4"},
		{name: "S-08 fixed range grows", at: "F1", typed: "=SUM($B$3:$B$6)", edit: insertRows("Sheet1", 5, 2), want: "=SUM($B$3:$B$8)"},
		{name: "S-08 fixed range after the insert stays", at: "F1", typed: "=SUM($B$3:$B$6)", edit: insertRows("Sheet1", 7, 2)},
		{name: "S-08 a fixed reference does not move with its formula", at: "F9", typed: "=$B$3+B3", edit: insertRows("Sheet1", 8, 1), wantAt: "F10", sameRef: true},
		{name: "S-08 fixed column", at: "A9", typed: "=$C3+C$3", edit: insertCols(t, "Sheet1", "B", 1), want: "=$D3+D$3"},
		{name: "S-08 mixed range", at: "A9", typed: "=SUM($C3:E$3)", edit: insertCols(t, "Sheet1", "D", 2), want: "=SUM($C3:G$3)"},
	})
}

// S-09: references to another tab, with a plain and a quoted name.
func TestInsertOtherTab(t *testing.T) {
	runApplyCases(t, []applyCase{
		{name: "S-09 edit in the named tab", at: "F1", typed: "=Costs!B3+B3", edit: insertRows("Costs", 2, 2), want: "=Costs!B5+B3"},
		{name: "S-09 capitals of the name do not count", at: "F1", typed: "=costs!B3", edit: insertRows("COSTS", 2, 2), want: "=costs!B5"},
		{name: "S-09 quoted name", at: "F1", typed: "=SUM('Q1 costs'!B3:B6)", edit: insertRows("Q1 costs", 5, 1), want: "=SUM('Q1 costs'!B3:B7)"},
		{name: "S-09 edit in the formula's tab", at: "F1", typed: "=Costs!B3+B3", edit: insertRows("Sheet1", 2, 2), want: "=Costs!B3+B5"},
		{name: "S-09 the formula moves, the other tab does not", at: "F9", typed: "=Costs!B3", edit: insertRows("Sheet1", 1, 1), wantAt: "F10", sameRef: true},
		{name: "S-09 edit in a third tab", at: "F1", typed: "=Costs!B3+B3", edit: insertRows("Other", 1, 5)},
		{name: "S-09 its own tab by name", at: "F1", typed: "=Sheet1!B3", edit: insertRows("Sheet1", 2, 1), want: "=Sheet1!B4"},
		{name: "S-09 fixed reference to the other tab", at: "F1", typed: "='Q1 costs'!$B$3", edit: insertCols(t, "Q1 costs", "A", 1), want: "='Q1 costs'!$C$3"},
		{name: "S-09 delete in the named tab", at: "F1", typed: "=Costs!B3*2", edit: deleteRows("Costs", 3, 1), want: "=#REF!*2"},
	})
}

// S-10: whole-column and whole-row references.
func TestInsertWholeColumnsAndRows(t *testing.T) {
	runApplyCases(t, []applyCase{
		{name: "S-10 rows under whole columns", at: "F1", typed: "=SUM(B:D)", edit: insertRows("Sheet1", 2, 5)},
		{name: "S-10 whole columns, the formula moves", at: "F9", typed: "=SUM(B:D)", edit: insertRows("Sheet1", 1, 5), wantAt: "F14"},
		{name: "S-10 columns before whole columns", at: "F1", typed: "=SUM(B:D)", edit: insertCols(t, "Sheet1", "A", 1), wantAt: "G1", want: "=SUM(C:E)"},
		{name: "S-10 columns inside whole columns", at: "F1", typed: "=SUM(B:D)", edit: insertCols(t, "Sheet1", "C", 2), wantAt: "H1", want: "=SUM(B:F)"},
		{name: "S-10 columns after whole columns", at: "A1", typed: "=SUM(B:D)", edit: insertCols(t, "Sheet1", "E", 2)},
		{name: "S-10 columns beside whole rows", at: "F9", typed: "=SUM(2:5)", edit: insertCols(t, "Sheet1", "A", 3), wantAt: "I9"},
		{name: "S-10 rows inside whole rows", at: "F9", typed: "=SUM(2:5)", edit: insertRows("Sheet1", 4, 2), wantAt: "F11", want: "=SUM(2:7)"},
		{name: "S-10 rows before whole rows", at: "F9", typed: "=SUM($2:$5)", edit: insertRows("Sheet1", 1, 1), wantAt: "F10", want: "=SUM($3:$6)"},
		{name: "S-10 delete inside whole columns", at: "A1", typed: "=SUM(B:D)", edit: deleteCols(t, "Sheet1", "C", 1), want: "=SUM(B:C)"},
		{name: "S-10 delete all whole columns", at: "A1", typed: "=SUM(B:D)", edit: deleteCols(t, "Sheet1", "B", 3), want: "=SUM(#REF!)"},
		{name: "S-10 delete all whole rows", at: "F9", typed: "=SUM(2:5)", edit: deleteRows("Sheet1", 1, 6), wantAt: "F3", want: "=SUM(#REF!)"},
		{name: "S-10 whole columns of another tab", at: "A1", typed: "=SUM(Costs!B:D)", edit: insertCols(t, "Costs", "C", 1), want: "=SUM(Costs!B:E)"},
	})
}

// S-11, S-12 and S-13: delete the head, the tail and the middle of a range,
// all of it, and a single referenced cell.
func TestDelete(t *testing.T) {
	runApplyCases(t, []applyCase{
		{name: "S-11 the head", at: "F1", typed: "=SUM(B3:B6)", edit: deleteRows("Sheet1", 2, 3), want: "=SUM(B2:B3)"},
		{name: "S-11 the first row", at: "F1", typed: "=SUM(B3:B6)", edit: deleteRows("Sheet1", 3, 1), want: "=SUM(B3:B5)"},
		{name: "S-11 the tail", at: "F1", typed: "=SUM(B3:B6)", edit: deleteRows("Sheet1", 5, 4), want: "=SUM(B3:B4)"},
		{name: "S-11 the last row", at: "F1", typed: "=SUM(B3:B6)", edit: deleteRows("Sheet1", 6, 1), want: "=SUM(B3:B5)"},
		{name: "S-11 the middle", at: "F1", typed: "=SUM(B3:B6)", edit: deleteRows("Sheet1", 4, 2), want: "=SUM(B3:B4)"},
		{name: "S-11 rows before the range", at: "F1", typed: "=SUM(B3:B6)", edit: deleteRows("Sheet1", 2, 1), want: "=SUM(B2:B5)"},
		{name: "S-11 rows after the range", at: "F1", typed: "=SUM(B3:B6)", edit: deleteRows("Sheet1", 7, 3)},
		{name: "S-11 a range written backwards", at: "F1", typed: "=SUM(B6:B3)", edit: deleteRows("Sheet1", 3, 2), want: "=SUM(B4:B3)"},
		{name: "S-11 columns of a range", at: "A9", typed: "=SUM(C3:F3)", edit: deleteCols(t, "Sheet1", "D", 2), want: "=SUM(C3:D3)"},
		{name: "S-12 all of a range", at: "F1", typed: "=SUM(B3:B6)*2", edit: deleteRows("Sheet1", 3, 4), want: "=SUM(#REF!)*2"},
		{name: "S-12 more than the range", at: "F1", typed: "=B3:B6", edit: deleteRows("Sheet1", 2, 9), want: "=#REF!"},
		{name: "S-12 a fixed range", at: "F1", typed: "=SUM($B$3:$B$4)", edit: deleteRows("Sheet1", 3, 2), want: "=SUM(#REF!)"},
		{name: "S-13 a single cell", at: "F1", typed: "=B3*2", edit: deleteRows("Sheet1", 3, 1), want: "=#REF!*2"},
		{name: "S-13 a single fixed cell", at: "F1", typed: "=$B$3*2", edit: deleteRows("Sheet1", 2, 5), want: "=#REF!*2"},
		{name: "S-13 one of two cells", at: "F1", typed: "=B3+B4", edit: deleteRows("Sheet1", 3, 1), want: "=#REF!+B3"},
		{name: "S-13 a cell under the deleted rows", at: "F1", typed: "=B9", edit: deleteRows("Sheet1", 3, 2), want: "=B7"},
		{name: "S-13 the column of a cell", at: "A9", typed: "=C3", edit: deleteCols(t, "Sheet1", "C", 1), want: "=#REF!"},
		{name: "S-13 the formula moves up", at: "F9", typed: "=B3+F8", edit: deleteRows("Sheet1", 4, 2), wantAt: "F7", want: "=B3+F6"},
		{name: "S-13 a tab name goes with the reference", at: "F1", typed: "='Q1 costs'!B3+1", edit: deleteRows("Q1 costs", 3, 1), want: "=#REF!+1"},
	})

	// A literal #REF! stays through later edits, and the edit of the
	// formula's own cell has no result.
	f := mustParse(t, "=#REF!*2+B5", "F1")
	g, _, _ := f.Apply("Sheet1", pos(t, "F1"), insertRows("Sheet1", 2, 1))
	if g.Text(pos(t, "F1")) != "=#REF!*2+B6" {
		t.Errorf("a literal #REF! does not stay: %s", g.Text(pos(t, "F1")))
	}
	gone := deleteRows("Sheet1", 1, 1)
	if _, ok := gone.Map("Sheet1", pos(t, "F1")); ok {
		t.Error("Map reports a deleted cell as alive")
	}
	if g, at, changed := f.Apply("Sheet1", pos(t, "F1"), gone); g != f || at != pos(t, "F1") || changed {
		t.Error("Apply rewrites a formula whose own cell is deleted")
	}
	if at, ok := gone.Map("sheet1", pos(t, "F3")); !ok || at != pos(t, "F2") {
		t.Errorf("Map moves F3 to %s", at.A1())
	}
	if at, ok := gone.Map("Other", pos(t, "F1")); !ok || at != pos(t, "F1") {
		t.Error("Map moves a cell of another tab")
	}
}

func TestRenameTab(t *testing.T) {
	rename := func(old, name string) Transform { return Transform{Kind: RenameTab, Tab: old, Name: name} }
	runApplyCases(t, []applyCase{
		{name: "rename a named tab", at: "F1", typed: "=Costs!B3+B3", edit: rename("Costs", "Budget"), want: "=Budget!B3+B3"},
		{name: "rename to a name that needs quotes", at: "F1", typed: "=SUM(costs!B:D)", edit: rename("Costs", "Q1 costs"), want: "=SUM('Q1 costs'!B:D)"},
		{name: "rename a quoted tab to a plain name", at: "F1", typed: "='Q1 costs'!$B$3", edit: rename("q1 COSTS", "Costs"), want: "=Costs!$B$3"},
		{name: "rename to a name with an apostrophe", at: "F1", typed: "=Costs!B3", edit: rename("Costs", "It's"), want: "='It''s'!B3"},
		{name: "rename to a name that reads as a cell", at: "F1", typed: "=Costs!B3", edit: rename("Costs", "Q1"), want: "='Q1'!B3"},
		{name: "rename another tab", at: "F1", typed: "=Costs!B3", edit: rename("Other", "Budget")},
		{name: "rename the formula's own tab", at: "F1", typed: "=B3+Sheet1!B3", edit: rename("Sheet1", "Main"), want: "=B3+Main!B3"},
		{name: "a name inside text stays", at: "F1", typed: `="Costs!B3"&Costs!B3`, edit: rename("Costs", "Budget"), want: `="Costs!B3"&Budget!B3`},
	})
}

// S-14, S-15 and S-16: a sort moves a formula with its row. References
// without "$" shift by the distance the formula moved, and one that would
// leave the grid becomes #REF!. Formulas outside the moved rows are not
// touched.
func TestMoved(t *testing.T) {
	cases := []struct {
		name, typed, from, to, want string
	}{
		{"S-14 its own row", "=C5*D5", "E5", "E9", "=C9*D9"},
		{"S-14 another row", "=B4+B5", "E5", "E2", "=B1+B2"},
		{"S-14 a range", "=SUM(B2:B4)", "E5", "E8", "=SUM(B5:B7)"},
		{"S-15 fixed cell", "=B5/$B$9", "C5", "C7", "=B7/$B$9"},
		{"S-15 fixed row and fixed column", "=B$2+$B4", "E5", "G8", "=D$2+$B7"},
		{"S-15 whole column", "=SUM(B:B)", "E5", "E9", "=SUM(B:B)"},
		{"S-15 another tab", "=Costs!B5", "E5", "E7", "=Costs!B7"},
		{"S-16 above the first row", "=B4+B5", "E5", "E1", "=#REF!+B1"},
		{"S-16 a range leaves", "=SUM(B2:B4)*2", "E5", "E3", "=SUM(#REF!)*2"},
		{"S-16 below the last row", "=B6", "E5", "E100000", "=#REF!"},
		{"S-16 left of the first column", "=A5", "B5", "A5", "=#REF!"},
		{"S-16 right of the last column", "=ZZ1", "A1", "B1", "=#REF!"},
		{"S-16 a fixed part never leaves", "=$A$1+B$1", "B5", "A1", "=$A$1+A$1"},
		{"S-16 whole rows leave", "=SUM(1:2)", "A5", "A1", "=SUM(#REF!)"},
		{"S-16 another tab leaves", "=Costs!B4", "E5", "E1", "=#REF!"},
		{"fill down the last row", "=A100000", "B100000", "B99999", "=A99999"},
		{"a reference outside the grid stays text", "=A100001", "B1", "B2", "=A100002"},
	}
	for _, c := range cases {
		f := mustParse(t, c.typed, c.from)
		g := f.Moved(pos(t, c.from), pos(t, c.to))
		if got := g.Text(pos(t, c.to)); got != c.want {
			t.Errorf("%s: %s moved from %s to %s is %s, want %s", c.name, c.typed, c.from, c.to, got, c.want)
		}
		if !strings.Contains(c.want, "#REF!") && g != f {
			t.Errorf("%s: the stored form changes from %q to %q", c.name, f.Stored(), g.Stored())
		}
		if fresh := mustParse(t, c.want, c.to); fresh.Stored() != g.Stored() {
			t.Errorf("%s: stored %q, a fresh parse of %s gives %q", c.name, g.Stored(), c.want, fresh.Stored())
		}
	}
}

// S-17: the order of rule 22 for every value kind, in both directions, with
// ties and with blank cells last either way.
func TestCompareSort(t *testing.T) {
	blank := Value{}
	ascending := []Value{
		NumberValue(-2), NumberValue(0), NumberValue(1.5), NumberValue(46000),
		TextValue(""), TextValue("apple"), TextValue("Banana"), TextValue("cherry"), TextValue("ß"),
		BoolValue(false), BoolValue(true),
		ErrorValue(ErrDiv0),
		blank,
	}
	for i, a := range ascending {
		for j, b := range ascending {
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := CompareSort(a, b); got != want {
				t.Errorf("CompareSort(%v, %v) = %d, want %d", a, b, got, want)
			}
		}
	}
	ties := [][2]Value{
		{TextValue("apple"), TextValue("APPLE")},
		{TextValue("straße"), TextValue("STRAẞE")},
		{ErrorValue(ErrDiv0), ErrorValue(ErrNA)},
		{blank, blank},
		{NumberValue(2), NumberValue(2)},
	}
	for _, tie := range ties {
		if CompareSort(tie[0], tie[1]) != 0 || CompareSort(tie[1], tie[0]) != 0 {
			t.Errorf("%v and %v do not tie", tie[0], tie[1])
		}
	}
	// A stable sort in each direction: descending reverses the values that
	// are not blank, and blank cells stay last.
	rows := []Value{blank, TextValue("b"), NumberValue(2), ErrorValue(ErrNA), BoolValue(true), TextValue("B"), NumberValue(1), blank, BoolValue(false)}
	sorted := func(desc bool) string {
		out := append([]Value(nil), rows...)
		for i := 1; i < len(out); i++ { // an insertion sort is stable
			for j := i; j > 0; j-- {
				a, b := out[j-1], out[j]
				c := CompareSort(a, b)
				if desc && !a.IsBlank() && !b.IsBlank() {
					c = -c
				}
				if c <= 0 {
					break
				}
				out[j-1], out[j] = b, a
			}
		}
		var names []string
		for _, v := range out {
			names = append(names, Show(v, ContentBlank, NumberFormat{}))
		}
		return strings.Join(names, ",")
	}
	if got, want := sorted(false), "1,2,b,B,FALSE,TRUE,#N/A,,"; got != want {
		t.Errorf("ascending: %s, want %s", got, want)
	}
	if got, want := sorted(true), "#N/A,TRUE,FALSE,b,B,2,1,,"; got != want {
		t.Errorf("descending: %s, want %s", got, want)
	}
}

// S-47: a cut and paste moves a formula without shifting what it reads.
func TestCut(t *testing.T) {
	from, to := pos(t, "B1"), pos(t, "C5")
	for typed, want := range map[string]string{
		"=A1":             "=Source!A1",
		"=$A$1":           "=Source!$A$1",
		"=A$1+Other!B2":   "=Source!A$1+Other!B2",
		"=Destination!A1": "=Destination!A1",
		"=SUM(A:A,2:3)":   "=SUM(Source!A:A,Source!2:3)",
		"=#REF!+1":        "=#REF!+1",
	} {
		g := mustParse(t, typed, "B1").Cut(from, to, "Source", "Destination")
		if got := g.Text(to); got != want {
			t.Errorf("S-47: %s cut from Source!B1 to Destination!C5 is %s, want %s", typed, got, want)
		}
		if fresh := mustParse(t, want, "C5"); fresh.Stored() != g.Stored() {
			t.Errorf("S-47: stored %q, a fresh parse of %s gives %q", g.Stored(), want, fresh.Stored())
		}
	}
	f := mustParse(t, "=A1+$B2", "B1")
	if g := f.Cut(from, to, "Source", "source"); g.Text(to) != "=A1+$B2" {
		t.Errorf("a cut inside one tab gives %s", g.Text(to))
	}
	if g := f.Cut(from, from, "Source", "Source"); g != f {
		t.Error("a cut to the same cell changes the formula")
	}
	if g := f.Cut(from, to, "Q1 costs", "Destination"); g.Text(to) != "='Q1 costs'!A1+'Q1 costs'!$B2" {
		t.Errorf("a cut from a tab with a space gives %s", g.Text(to))
	}
}

// S-48: a formula at the length limit. Each edit that makes its text longer
// gives a text over the limit, which TextLen reports so that the writer
// refuses the edit.
func TestTextLenAtTheLimit(t *testing.T) {
	padded := func(text string) string { return text + strings.Repeat(" ", MaxFormulaChars-len(text)) }
	at := pos(t, "B9")
	f := mustParse(t, padded("=A9"), "B9")
	if got := f.TextLen(at); got != MaxFormulaChars {
		t.Fatalf("TextLen = %d, want %d", got, MaxFormulaChars)
	}
	check := func(name string, g *Formula, at Pos, want int) {
		t.Helper()
		text := g.Text(at)
		if got := g.TextLen(at); got != want || utf8.RuneCountInString(text) != want {
			t.Errorf("S-48 %s: TextLen = %d and the text has %d characters, want %d", name, got, utf8.RuneCountInString(text), want)
		}
		var le *LimitError
		if _, err := Parse(text, at, Limits{}); !errors.As(err, &le) || le.Name != "formula_length" {
			t.Errorf("S-48 %s: the text of %d characters is not refused: %v", name, want, err)
		}
	}
	g, newAt, _ := f.Apply("Sheet1", at, insertRows("Sheet1", 1, 1))
	check("insert a row above", g, newAt, MaxFormulaChars+1)
	check("sort it to row 10", f.Moved(at, pos(t, "B10")), pos(t, "B10"), MaxFormulaChars+1)
	check("fill it down", f.Moved(at, pos(t, "B10")), pos(t, "B10"), MaxFormulaChars+1)

	named := mustParse(t, padded("=Data!A9"), "B9")
	g, _, _ = named.Apply("Sheet1", at, Transform{Kind: RenameTab, Tab: "Data", Name: "Database"})
	check("rename a tab it names", g, at, MaxFormulaChars+4)
	// The limit counts code points: a quoted name of two Korean letters and
	// a space is as long as the four letters and one space it replaces.
	g, _, _ = named.Apply("Sheet1", at, Transform{Kind: RenameTab, Tab: "Data", Name: "예 산"})
	if got := g.TextLen(at); got != MaxFormulaChars+1 || utf8.RuneCountInString(g.Text(at)) != MaxFormulaChars+1 {
		t.Errorf("S-48 rename to a quoted name: TextLen = %d, want %d", got, MaxFormulaChars+1)
	}
	g, _, _ = named.Apply("Sheet1", at, Transform{Kind: RenameTab, Tab: "Data", Name: "예산"})
	if got := g.TextLen(at); got != MaxFormulaChars-2 || utf8.RuneCountInString(g.Text(at)) != MaxFormulaChars-2 {
		t.Errorf("S-48 rename to a shorter name: TextLen = %d, want %d", got, MaxFormulaChars-2)
	}
}
