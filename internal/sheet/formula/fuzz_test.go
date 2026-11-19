package formula

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// fuzzSheet is a small sheet with a value of every kind, and a budget that
// ends an evaluation after 4,096 units of work.
func fuzzSheet() (*testSheet, *Env) {
	cells := map[Pos]*testCell{
		{0, 0}: {content: Content{Kind: ContentNumber, Num: 3}},
		{1, 0}: {content: Content{Kind: ContentText, Text: "apple"}},
		{2, 0}: {content: Content{Kind: ContentBool, Num: 1}},
		{0, 1}: {content: Content{Kind: ContentNumber, Num: -1.5}},
		{1, 1}: {content: Content{Kind: ContentText, Text: "12"}},
		{2, 1}: {content: Content{Kind: ContentDate, Num: 46000}},
		{0, 2}: {content: Content{Kind: ContentText, Text: ""}},
	}
	sheet := &testSheet{today: 46339, tabs: []*testTab{
		{name: "Sheet1", rows: 40, cols: 12, cells: cells},
		{name: "Costs", rows: 5, cols: 5, cells: map[Pos]*testCell{{0, 0}: {content: Content{Kind: ContentNumber, Num: 7}}}},
	}}
	budget := NewBudget(func() bool { return true })
	return sheet, &Env{Grid: sheet, Today: sheet.today, Budget: budget}
}

var parseSeeds = []string{
	"=C5*D5", "=SUM(B$2:B4)", "=sum( Costs!B:D )", "='Q1 costs'!$B$3", "=SUM(B3", "=1+2*3^2%", `="a"&"b""c"`,
	"=IF(A1>1,TODAY(),#N/A)", "=VLOOKUP(A1,A:C,2,FALSE)", "=-2^2", "=A100001", "=A0", "=2:5", `="abc`, "={1}",
	"=TEXTJOIN(\",\",TRUE,A1:C3)", "=MEDIAN(A:A,1,2)", "=COUNTIF(A1:A3,\">1\")", "=1E5", "=.5", "=", "=#REF!*2",
	"=예산!A1+Übersicht!$B:$C", "='Q1 예산'!A1", "=예산",
	"=SUMPRODUCT(A1:A3,B1:B3)", "=REPT(\"a\",40000)", "=TEXT(A1,\"#,##0.00\")", "=DATE(2026,1,31)+TIME(1,2,3)",
}

// FuzzParse checks that every text parses without a panic, that the stored
// form and the A1 text of a formula read back as the same formula, and
// that evaluation and every transform cope with it.
func FuzzParse(f *testing.F) {
	for _, seed := range parseSeeds {
		f.Add(seed, uint16(4), uint16(4))
	}
	f.Fuzz(func(t *testing.T, text string, row, col uint16) {
		at := Pos{Row: int32(row) % MaxRows, Col: int32(col) % MaxCols}
		if !utf8.ValidString(text) {
			return // the host hands over valid text only
		}
		fm := checkRoundTrip(t, text, at)
		if fm == nil {
			return
		}
		sheet, env := fuzzSheet()
		env.At = Pos{Row: at.Row % 40, Col: at.Col % 12}
		v := fm.Eval(env)
		if v.Kind() == KindText && v.TextLen() > MaxTextLen {
			t.Fatalf("%q gives a text of %d characters", text, v.TextLen())
		}
		Show(v, ContentFormula, NumberFormat{})
		reads := int64(0)
		fm.Refs(at, func(r Ref) {
			if !r.First.InGrid() || !r.Last.InGrid() || r.First.Row > r.Last.Row || r.First.Col > r.Last.Col {
				t.Fatalf("%q reports the reference %+v", text, r)
			}
			reads += r.Cells(MaxRows, MaxCols)
		})
		dims := func(tab string) (int32, int32, bool) {
			ref, ok := sheet.Tab(tab)
			if tab == "" {
				ref, ok = 0, true
			}
			if !ok {
				return 0, 0, false
			}
			rows, cols := sheet.Dims(ref)
			return rows, cols, true
		}
		if got := fm.ReadCells(at, dims); got < 0 || got > reads {
			t.Fatalf("%q reads %d cells, its references hold %d", text, got, reads)
		}
		for _, edit := range []Transform{
			{Kind: InsertRows, Tab: "Sheet1", At: 2, Count: 3}, {Kind: DeleteRows, Tab: "sheet1", At: 1, Count: 2},
			{Kind: InsertCols, Tab: "Costs", At: 0, Count: 1}, {Kind: DeleteCols, Tab: "Sheet1", At: 1, Count: 1},
			{Kind: RenameTab, Tab: "Costs", Name: "Q1 costs"},
		} {
			g, newAt, changed := fm.Apply("Sheet1", at, edit)
			if changed != (g.Stored() != fm.Stored()) {
				t.Fatalf("%q: Apply(%+v) reports changed = %v for %q", text, edit, changed, g.Stored())
			}
			checkStored(t, g, newAt)
		}
		checkStored(t, fm.Moved(at, Pos{Row: at.Col % MaxRows, Col: at.Row % MaxCols}), Pos{Row: at.Col % MaxRows, Col: at.Row % MaxCols})
		checkStored(t, fm.Cut(at, Pos{Row: 7, Col: 7}, "Sheet1", "Costs"), Pos{Row: 7, Col: 7})
	})
}

// checkStored checks that a formula a transform returned is a formula like
// any other: its stored form reads back, and its text has the length
// TextLen reports.
func checkStored(t *testing.T, f *Formula, at Pos) {
	t.Helper()
	if got, want := f.TextLen(at), utf8.RuneCountInString(f.Text(at)); got != want {
		t.Fatalf("TextLen = %d for the text %q of %d characters", got, f.Text(at), want)
	}
	if f.Unreadable() {
		return
	}
	g, err := ParseStored(f.Stored())
	if err != nil || g.Stored() != f.Stored() || g.Text(at) != f.Text(at) {
		t.Fatalf("the stored form %q does not read back: %v", f.Stored(), err)
	}
}

// FuzzParseStored checks that no stored text makes the parser panic, and
// that text it accepts is a stored form it writes back unchanged.
func FuzzParseStored(f *testing.F) {
	for _, seed := range []string{
		"={R[0]C[-2]}*{R[0]C[-1]}", "=SUM({R2C[-3]:R[-1]C[-3]})", "=sum( {Costs!C[-3]:C[-1]} )", "={'Q1 costs'!R3C2}",
		"=SUM({R[-2]C[-3]}", "={R2:R[0]}", "=#REF!*2", `="{R[0]C[0]}"&{R[-2]C[-2]}`, "={'It''s'!R[-2]C[-2]}", "=A0", "={예산!R[0]C[1]}",
		"={R[999999999]C[18277]}", "=1+", "={", "={R[0]C[0]}{R[0]C[0]}", "=IF({R[0]C[1]},TODAY(),)",
	} {
		f.Add(seed, uint16(4), uint16(4))
	}
	f.Fuzz(func(t *testing.T, stored string, row, col uint16) {
		fm, err := ParseStored(stored)
		if err != nil {
			return
		}
		at := Pos{Row: int32(row) % MaxRows, Col: int32(col) % MaxCols}
		if fm.Stored() != stored || fm.Unreadable() {
			t.Fatalf("ParseStored(%q) is stored as %q", stored, fm.Stored())
		}
		checkStored(t, fm, at)
		_, env := fuzzSheet()
		env.At = Pos{Row: at.Row % 40, Col: at.Col % 12}
		fm.Eval(env)
		fm.Refs(at, func(Ref) {})
		g, newAt, _ := fm.Apply("Sheet1", at, Transform{Kind: DeleteRows, Tab: "Sheet1", At: 3, Count: 2})
		checkStored(t, g, newAt)
		checkStored(t, fm.Moved(at, Pos{}), Pos{})
	})
}

// modelRef is a reference of the model that FuzzApply checks the transforms
// against. It holds addresses, as a person reads them, and no offsets.
type modelRef struct {
	tab            string  // as a formula writes it, or empty
	kind           int     // 0 cell, 1 range, 2 whole columns, 3 whole rows
	r1, c1, r2, c2 int32   // zero-based
	abs            [4]bool // c1, r1, c2, r2
	dead           bool
}

func (m modelRef) text() string {
	if m.dead {
		return "#REF!"
	}
	corner := func(row, col int32, colAbs, rowAbs bool) string {
		s := ""
		if m.kind != 3 {
			if colAbs {
				s += "$"
			}
			s += ColumnName(col)
		}
		if m.kind != 2 {
			if rowAbs {
				s += "$"
			}
			s += fmt.Sprint(row + 1)
		}
		return s
	}
	s := ""
	if m.tab != "" {
		s = m.tab + "!"
	}
	s += corner(m.r1, m.c1, m.abs[0], m.abs[1])
	if m.kind != 0 {
		s += ":" + corner(m.r2, m.c2, m.abs[2], m.abs[3])
	}
	return s
}

// modelTabs are the tab names of the model with the way a formula writes
// each.
var modelTabs = []struct{ name, written string }{
	{"Sheet1", "Sheet1"}, {"Costs", "Costs"}, {"Q1 costs", "'Q1 costs'"}, {"It's", "'It''s'"}, {"Budget", "Budget"},
}

func modelTabName(written string) string {
	for _, tab := range modelTabs {
		if strings.EqualFold(tab.written, written) {
			return tab.name
		}
	}
	return ""
}

// shift moves one address for an insert or a delete of count lines at at.
// ok is false for a deleted line.
func shift(v, at, count int32, insert bool) (int32, bool) {
	switch {
	case insert && v >= at:
		return v + count, true
	case !insert && v >= at+count:
		return v - count, true
	case !insert && v >= at:
		return v, false
	}
	return v, true
}

// shiftEnds moves the two ends of a range written in any order. A delete
// cuts the range to the lines that remain.
func shiftEnds(a, b, at, count int32, insert bool) (int32, int32, bool) {
	lo, hi := min(a, b), max(a, b)
	newLo, okLo := shift(lo, at, count, insert)
	newHi, okHi := shift(hi, at, count, insert)
	if !okLo {
		newLo = at
	}
	if !okHi {
		newHi = at - 1
	}
	if newLo > newHi {
		return 0, 0, false
	}
	if a <= b {
		return newLo, newHi, true
	}
	return newHi, newLo, true
}

// apply is rule 14 for one reference of a formula in the tab own.
func (m modelRef) apply(own string, t Transform) modelRef {
	tab := own
	if m.tab != "" {
		tab = modelTabName(m.tab)
	}
	if m.dead || !strings.EqualFold(tab, t.Tab) {
		return m
	}
	ok := true
	switch t.Kind {
	case RenameTab:
		if m.tab != "" {
			for _, mt := range modelTabs {
				if mt.name == t.Name {
					m.tab = mt.written
				}
			}
		}
	case InsertRows, DeleteRows:
		switch m.kind {
		case 0:
			m.r1, ok = shift(m.r1, t.At, t.Count, t.Kind == InsertRows)
			m.r2 = m.r1
		case 1, 3:
			m.r1, m.r2, ok = shiftEnds(m.r1, m.r2, t.At, t.Count, t.Kind == InsertRows)
		}
	case InsertCols, DeleteCols:
		switch m.kind {
		case 0:
			m.c1, ok = shift(m.c1, t.At, t.Count, t.Kind == InsertCols)
			m.c2 = m.c1
		case 1, 2:
			m.c1, m.c2, ok = shiftEnds(m.c1, m.c2, t.At, t.Count, t.Kind == InsertCols)
		}
	}
	m.dead = !ok
	return m
}

// moved is rule 15 for one reference: the parts without "$" shift, and a
// part that leaves the grid makes the reference #REF!.
func (m modelRef) moved(dRow, dCol int32) modelRef {
	if m.dead {
		return m
	}
	rows, cols := m.kind != 2, m.kind != 3
	part := func(v *int32, abs bool, delta, limit int32, used bool) {
		if abs || !used {
			return
		}
		*v += delta
		if *v < 0 || *v >= limit {
			m.dead = true
		}
	}
	part(&m.c1, m.abs[0], dCol, MaxCols, cols)
	part(&m.r1, m.abs[1], dRow, MaxRows, rows)
	if m.kind != 0 {
		part(&m.c2, m.abs[2], dCol, MaxCols, cols)
		part(&m.r2, m.abs[3], dRow, MaxRows, rows)
	}
	return m
}

type byteReader struct {
	data []byte
}

func (r *byteReader) next(n int) int32 {
	if len(r.data) == 0 {
		return 0
	}
	b := r.data[0]
	r.data = r.data[1:]
	return int32(b) % int32(n)
}

// modelFormula builds a formula of references from fuzz bytes. Its text is
// "=F(ref,ref,...)+1", so the model knows every reference without a parser.
func modelFormula(r *byteReader) (refs []modelRef, at Pos) {
	at = Pos{Row: r.next(60), Col: r.next(30)}
	for n := r.next(4) + 1; n > 0; n-- {
		m := modelRef{kind: int(r.next(4)), r1: r.next(60), c1: r.next(30), r2: r.next(60), c2: r.next(30)}
		if tab := r.next(8); tab < 4 {
			m.tab = modelTabs[tab].written
		}
		bits := r.next(16)
		m.abs = [4]bool{bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0}
		switch m.kind {
		case 0:
			m.r2, m.c2 = m.r1, m.c1
		case 2:
			m.r1, m.r2 = 0, 0
		case 3:
			m.c1, m.c2 = 0, 0
		}
		refs = append(refs, m)
	}
	return refs, at
}

func modelText(refs []modelRef) string {
	parts := make([]string, len(refs))
	for i, m := range refs {
		parts[i] = m.text()
	}
	return "=F(" + strings.Join(parts, ",") + ")+1"
}

// FuzzApply checks every transform against a model that rewrites
// addresses: the stored form after a transform must equal a fresh parse of
// the model's A1 text at the cell the formula has then.
func FuzzApply(f *testing.F) {
	f.Add([]byte{4, 4, 1, 0, 2, 1, 0, 0, 9, 0, 0, 0, 1, 3})
	f.Add([]byte{8, 5, 2, 1, 2, 1, 5, 1, 1, 6, 1, 1, 4, 3, 9, 2, 7, 1, 2, 2, 3})
	f.Add([]byte{0, 0, 3, 2, 0, 1, 0, 3, 2, 5, 3, 1, 0, 4, 0, 6, 0, 0, 3, 0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte{59, 29, 0, 0, 59, 29, 0, 0, 7, 0, 4, 0, 1, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &byteReader{data: data}
		refs, at := modelFormula(r)
		const own = "Sheet1"
		text := modelText(refs)
		fm, err := Parse(text, at, Limits{})
		if err != nil || fm.Unreadable() {
			t.Fatalf("Parse(%q): %v", text, err)
		}
		if fm.Text(at) != text {
			t.Fatalf("Parse(%q).Text() = %q", text, fm.Text(at))
		}

		check := func(what string, got *Formula, at Pos, want []modelRef) {
			t.Helper()
			wantText := modelText(want)
			fresh, err := Parse(wantText, at, Limits{})
			if err != nil {
				t.Fatalf("Parse(%q): %v", wantText, err)
			}
			if got.Stored() != fresh.Stored() || got.Text(at) != wantText {
				t.Fatalf("%s: %s at %s becomes %q (%s), the model gives %q (%s) at %s",
					what, text, modelPos(refs, at), got.Stored(), got.Text(at), fresh.Stored(), wantText, at.A1())
			}
			if got.TextLen(at) != len(wantText) {
				t.Fatalf("%s: TextLen = %d for %q", what, got.TextLen(at), wantText)
			}
		}

		// A structural edit.
		edit := Transform{Kind: TransformKind(r.next(5) + 1), Tab: modelTabs[r.next(3)].name, At: r.next(62), Count: r.next(6) + 1}
		if r.next(2) == 1 {
			edit.Tab = strings.ToUpper(edit.Tab)
		}
		if edit.Kind == RenameTab {
			edit.Name = modelTabs[3+r.next(2)].name
		}
		newAt, alive := at, true
		switch {
		case !strings.EqualFold(edit.Tab, own):
		case edit.Kind == InsertRows || edit.Kind == DeleteRows:
			newAt.Row, alive = shift(at.Row, edit.At, edit.Count, edit.Kind == InsertRows)
		case edit.Kind == InsertCols || edit.Kind == DeleteCols:
			newAt.Col, alive = shift(at.Col, edit.At, edit.Count, edit.Kind == InsertCols)
		}
		got, gotAt, changed := fm.Apply(own, at, edit)
		if mapped, ok := edit.Map(own, at); ok != alive || (alive && mapped != newAt) {
			t.Fatalf("Map(%+v, %s) = %s, %v, the model gives %s, %v", edit, at.A1(), mapped.A1(), ok, newAt.A1(), alive)
		}
		if !alive {
			if got != fm || gotAt != at || changed {
				t.Fatalf("Apply(%+v) rewrites a formula whose cell %s is deleted", edit, at.A1())
			}
		} else {
			want := make([]modelRef, len(refs))
			for i, m := range refs {
				want[i] = m.apply(own, edit)
			}
			if gotAt != newAt {
				t.Fatalf("Apply(%+v) moves %s to %s, the model to %s", edit, at.A1(), gotAt.A1(), newAt.A1())
			}
			check(fmt.Sprintf("Apply(%+v)", edit), got, newAt, want)
			if changed != (got.Stored() != fm.Stored()) || changed != (got != fm) {
				t.Fatalf("Apply(%+v) reports changed = %v", edit, changed)
			}
		}

		// A copy, a fill or a sort to another cell.
		to := Pos{Row: r.next(60), Col: r.next(30)}
		if r.next(4) == 0 {
			to = Pos{Row: MaxRows - 1 - r.next(3), Col: MaxCols - 1 - r.next(3)}
		}
		want := make([]modelRef, len(refs))
		for i, m := range refs {
			want[i] = m.moved(to.Row-at.Row, to.Col-at.Col)
		}
		check("Moved", fm.Moved(at, to), to, want)

		// A cut to another cell of the same tab and of another one.
		check("Cut in the tab", fm.Cut(at, to, own, "SHEET1"), to, refs)
		for i, m := range refs {
			want[i] = m
			if m.tab == "" {
				want[i].tab = "'Q1 costs'"
			}
		}
		check("Cut to another tab", fm.Cut(at, to, "Q1 costs", own), to, want)
	})
}

func modelPos(refs []modelRef, at Pos) string {
	return fmt.Sprintf("%s (%d references)", at.A1(), len(refs))
}

// FuzzParseTyped checks that typed entry never panics and that the formula
// bar gives back text that reads as the same content.
func FuzzParseTyped(f *testing.F) {
	for _, seed := range []string{
		"", "=", "=1+1", "'007", "TRUE", "1,200.50", "1.5E+20", "50%", "$5", "-€1,000.5", "2026-03-15", "3/15/2026",
		"Mar 15, 2026", "14:30", "14:30:05", "2:30 PM", "2026-03-15 14:30", "Rent", " 12", "1e400", "''", "-",
		"1234567890123456", "12345678901234567.5", "1.234567890123456E15",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if !utf8.ValidString(text) {
			return
		}
		content, sets := ParseTyped(text)
		if sets != nil && !sets.Valid() {
			t.Fatalf("ParseTyped(%q) sets the format %+v", text, *sets)
		}
		if content.IsFormula() {
			return
		}
		format := NumberFormat{}
		if sets != nil {
			format = *sets
		}
		shown := Show(content.Value(), content.Kind, format)
		AppendCSVField(nil, shown)
		edit := EditText(content, Pos{})
		if back, _ := ParseTyped(edit); back != readBack(content, edit) {
			t.Fatalf("ParseTyped(%q) = %+v is shown as %q, which reads back as %+v", text, content, edit, back)
		}
		// A number form that entry keeps as text is still a number to an
		// operator.
		_, isValue := readValue(text)
		kept := content.Kind == ContentText && content.Text == text && isValue
		if _, ok := textToNumber(text); ok != (kept || content.Kind >= ContentNumber && content.Kind <= ContentDateTime && content.Kind != ContentText && content.Kind != ContentBool) {
			t.Fatalf("textToNumber(%q) = %v for the content %+v", text, ok, content)
		}
	})
}
