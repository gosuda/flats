package formula

import (
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"testing"
)

// evalOn calculates a formula written in Z9 of the sheet's first tab.
func evalOn(t testing.TB, sheet *testSheet, formula string) Value {
	t.Helper()
	at := pos(t, "Z9")
	return mustParse(t, formula, "Z9").Eval(&Env{Grid: sheet, At: at, Budget: NewBudget(nil)})
}

// A cell outside the tab's rows or columns is blank, and a direct value is
// a range of one cell, whatever the order of the arguments.
func TestConditionalTotalsOutsideTheTab(t *testing.T) {
	sheet := &testSheet{tabs: []*testTab{
		newTab(t, "Sheet1", map[string]*testCell{"A1": numberCell(3), "B1": numberCell(9), "A2": textCell("x")}),
		{name: "Small", rows: 2, cols: 2, cells: map[Pos]*testCell{{0, 0}: numberCell(4), {1, 1}: numberCell(6)}},
	}}
	for formula, want := range map[string]Value{
		// under the rows
		`=SUMIF(A1001,"",5)`:       NumberValue(5),
		`=SUMIF(A1001,"x",5)`:      NumberValue(0),
		`=COUNTIF(A1001,"")`:       NumberValue(1),
		`=COUNTIFS(A1001,"",1,1)`:  NumberValue(1),
		`=COUNTIFS(1,1,A1001,"")`:  NumberValue(1),
		`=COUNTIFS(A1001,"",1,2)`:  NumberValue(0),
		`=COUNTIFS(1,2,A1001,"")`:  NumberValue(0),
		`=SUMIFS(5,A1001,"")`:      NumberValue(5),
		`=SUMIFS(5,A1001,"",1,1)`:  NumberValue(5),
		`=SUMIFS(5,1,1,A1001,"")`:  NumberValue(5),
		`=AVERAGEIF(A1001,"",7)`:   NumberValue(7),
		`=AVERAGEIFS(7,A1001,"")`:  NumberValue(7),
		`=MAXIFS(7,A1001,"")`:      NumberValue(7),
		`=MINIFS(7,A1001,"",1,1)`:  NumberValue(7),
		`=AVERAGEIF(A1001,"x",7)`:  ErrorValue(ErrDiv0),
		`=SUMIF(A1,">0",A1001)`:    NumberValue(0),
		`=SUMIF(A1001,"",B1)`:      NumberValue(9),
		`=SUMIFS(B1,A1001,"",1,1)`: NumberValue(9),
		// beside the columns
		`=SUMIF(AB1,"",5)`:       NumberValue(5),
		`=COUNTIFS(AB1,"",1,1)`:  NumberValue(1),
		`=COUNTIFS(1,1,AB1,"")`:  NumberValue(1),
		`=SUMIFS(5,AB1,"")`:      NumberValue(5),
		`=AVERAGEIFS(7,AB1,"")`:  NumberValue(7),
		`=MAXIFS(7,1,1,AB1,"")`:  NumberValue(7),
		`=MINIFS(7,AB1,"")`:      NumberValue(7),
		`=SUMIF(AB1,"",B1)`:      NumberValue(9),
		`=SUMIF(A1,">0",AB1)`:    NumberValue(0),
		`=COUNTIFS(AB1,"x",1,1)`: NumberValue(0),
		// under the rows and beside the columns, with cells inside and direct values
		`=SUMIF(AB1001,"",5)`:                       NumberValue(5),
		`=COUNTIFS(A1,">0",A1001,"",AB1,"",1,1)`:    NumberValue(1),
		`=COUNTIFS(1,1,AB1001,"",A1,3)`:             NumberValue(1),
		`=COUNTIFS(A1,">5",A1001,"",AB1,"",1,1)`:    NumberValue(0),
		`=SUMIFS(B1,A1,3,A1001,"",AB1,"<>x")`:       NumberValue(9),
		`=SUMIFS(5,A1,3,A1001,"",AB1,"<>x",2,">1")`: NumberValue(5),
		`=AVERAGEIFS(B1,AB1001,"",A1,3)`:            NumberValue(9),
		`=MAXIFS(AB1001,A1,3)`:                      NumberValue(0),
		// ranges that leave their tabs at different places
		`=COUNTIFS(A1:B3,"",Small!A1:B3,"")`:          NumberValue(2),
		`=COUNTIFS(Small!A1:B3,"",A1:B3,"")`:          NumberValue(2),
		`=COUNTIFS(Small!A1:B3,"<>4",A1:B3,"<>x")`:    NumberValue(4),
		`=SUMIFS(A1:B3,Small!A1:B3,"")`:               NumberValue(9),
		`=SUMIFS(Small!A1:B3,A1:B3,"<>x")`:            NumberValue(10),
		`=COUNTIFS(A999:B1002,"",Small!A1:B4,"")`:     NumberValue(6),
		`=COUNTIFS(Y1:AB2,"",Small!A1:D2,"")`:         NumberValue(6),
		`=SUMIFS(Small!A1:D2,Y1:AB2,"",Z1:AC2,"<>x")`: NumberValue(10),
	} {
		if got := evalOn(t, sheet, formula); got != want {
			t.Errorf("%s = %v, want %v", formula, got, want)
		}
	}
}

// The leftmost argument that is an error gives the result. An error in a
// cell of the value range counts only when the cell's criteria match.
func TestConditionalTotalsErrorOrder(t *testing.T) {
	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", map[string]*testCell{
		"A1": numberCell(1), "A2": numberCell(2), "B1": numberCell(9), "C1": numberCell(5),
		"D1": {f: mustParse(t, "=1/0", "D1")},
	})}}
	na, ref, div, val := ErrorValue(ErrNA), ErrorValue(ErrRef), ErrorValue(ErrDiv0), ErrorValue(ErrValue)
	for formula, want := range map[string]Value{
		`=SUMIF(A1,NA(),Nope!A1)`:           na,
		`=AVERAGEIF(A1,NA(),Nope!A1)`:       na,
		`=SUMIF(Nope!A1,NA(),B1)`:           ref,
		`=AVERAGEIF(Nope!A1,NA())`:          ref,
		`=COUNTIF(Nope!A1,NA())`:            ref,
		`=COUNTIF(A1,NA())`:                 na,
		`=COUNTIFS(A1,NA(),Nope!A1,1)`:      na,
		`=COUNTIFS(Nope!A1,NA(),A1,1/0)`:    ref,
		`=COUNTIFS(A1,1,Nope!A1,NA())`:      ref,
		`=COUNTIFS(A1,1,B1,NA(),C1,1/0)`:    na,
		`=COUNTIFS(A1,1,B1,1/0,Nope!A1,1)`:  div,
		`=SUMIFS(Nope!A1,A1,NA())`:          ref,
		`=SUMIFS(B1,Nope!A1,NA())`:          ref,
		`=SUMIFS(B1,A1,NA(),Nope!A1,1)`:     na,
		`=SUMIFS(B1,A1,1,Nope!A1,NA())`:     ref,
		`=AVERAGEIFS(B1,A1,NA(),Nope!A1,1)`: na,
		`=AVERAGEIFS(Nope!A1,A1,NA())`:      ref,
		`=MAXIFS(B1,A1,NA(),Nope!A1,1)`:     na,
		`=MAXIFS(Nope!A1,A1,NA())`:          ref,
		`=MINIFS(B1,A1,NA(),Nope!A1,1)`:     na,
		`=MINIFS(B1,A1,1,C1,1/0,Nope!A1,1)`: div,
		// an error among the arguments comes before ranges of different sizes
		`=COUNTIFS(A1:A2,1,B1,NA())`:        na,
		`=COUNTIFS(A1:A2,1,B1,1,C1,NA())`:   na,
		`=COUNTIFS(A1:A2,1,B1,1,Nope!A1,1)`: ref,
		`=SUMIF(A1:A2,NA(),B1)`:             na,
		`=SUMIFS(B1,A1:A2,NA())`:            na,
		`=COUNTIFS(A1:A2,1,B1,1)`:           val,
		`=SUMIF(A1:A2,1,B1)`:                val,
		`=SUMIFS(B1,A1:A2,1)`:               val,
		// An error value where a range is read is an argument that is an
		// error, like a range on a tab that does not exist: an error
		// written in the formula, which a deleted reference leaves, or any
		// other direct value that is an error.
		`=COUNTIF(#REF!,1)`:              ref,
		`=COUNTIF(#REF!,"")`:             ref,
		`=COUNTIF(A0,1)`:                 ref,
		`=COUNTIFS(#REF!,1)`:             ref,
		`=COUNTIFS(A1,1,#REF!,1)`:        ref,
		`=COUNTIFS(A1,2,#REF!,1)`:        ref,
		`=COUNTIFS(A1,NA(),#REF!,1)`:     na,
		`=COUNTIFS(#REF!,NA(),A1,1/0)`:   ref,
		`=COUNTIFS(A1:A2,1,#REF!,1)`:     ref,
		`=COUNTIFS(#N/A,1,Nope!A1,1)`:    na,
		`=COUNTIFS(Nope!A1,1,#N/A,1)`:    ref,
		`=SUMIF(#REF!,1)`:                ref,
		`=SUMIF(#REF!,1,B1)`:             ref,
		`=SUMIF(A1,1,#REF!)`:             ref,
		`=SUMIF(A1,2,#REF!)`:             ref,
		`=SUMIF(A1,NA(),#REF!)`:          na,
		`=SUMIF(A1:A2,1,#REF!)`:          ref,
		`=SUMIFS(#REF!,A1,1)`:            ref,
		`=SUMIFS(#REF!,A1,2)`:            ref,
		`=SUMIFS(#REF!,A1,NA())`:         ref,
		`=SUMIFS(B1,#REF!,1)`:            ref,
		`=SUMIFS(B1,A1,1,#REF!,1)`:       ref,
		`=SUMIFS(B1,A1,NA(),#REF!,1)`:    na,
		`=SUMIFS(#N/A,#REF!,1)`:          na,
		`=SUMIFS(#N/A,A1:A2,1)`:          na,
		`=AVERAGEIF(#REF!,1)`:            ref,
		`=AVERAGEIF(A1,5,#REF!)`:         ref,
		`=AVERAGEIFS(#REF!,A1,5)`:        ref,
		`=AVERAGEIFS(B1,A1,1,#REF!,1)`:   ref,
		`=MAXIFS(#REF!,A1,1)`:            ref,
		`=MAXIFS(B1,#REF!,1)`:            ref,
		`=MAXIFS(B1,A1,2,#REF!,1)`:       ref,
		`=MINIFS(#REF!,A1,1)`:            ref,
		`=MINIFS(B1,A1,1,C1,5,#REF!,"")`: ref,
		`=IFERROR(COUNTIF(#REF!,1),"x")`: TextValue("x"),
		`=SUMIF(A1,1,NA())`:              na,
		`=SUMIF(A1,2,NA())`:              na,
		`=SUMIFS(NA(),A1,1)`:             na,
		`=SUMIFS(NA(),A1,2)`:             na,
		`=SUMIFS(NA(),A1,1/0)`:           na,
		`=COUNTIF(NA(),1/0)`:             na,
		`=COUNTIF(NA(),1)`:               na,
		`=COUNTIFS(NA(),1,A1,1)`:         na,
		`=COUNTIFS(A1,1,NA(),1)`:         na,
		`=MAXIFS(1/0,A1,1,B1,8)`:         div,
		`=MINIFS(1/0,A1,1,B1,9)`:         div,
		`=AVERAGEIFS(NA(),A1,1)`:         na,
		`=AVERAGEIFS(NA(),A1,5)`:         na,
		`=SUMIF(NA(),NA())`:              na,
		`=SUMIF(1/0,"<>x")`:              div,
		`=SUMIF(A1,"<>x",NA())`:          na,
		// An error in a cell of a range stays an error of that cell.
		`=COUNTIF(D1,"<>x")`:       NumberValue(0),
		`=COUNTIFS(A1,1,D1,"<>x")`: NumberValue(0),
		`=SUMIF(A1,2,D1)`:          NumberValue(0),
		`=SUMIF(A1,1,D1)`:          div,
		`=SUMIFS(D1,A1,2)`:         NumberValue(0),
		`=MAXIFS(D1,A1,1)`:         div,
	} {
		if got := evalOn(t, sheet, formula); got != want {
			t.Errorf("%s = %v, want %v", formula, got, want)
		}
	}
}

// condModelArg is an argument of a conditional total in the model: a
// range of a tab that starts at row and col, a direct value, or a
// reference to a tab that does not exist.
type condModelArg struct {
	text   string
	tab    int // -1 for a tab that does not exist
	row    int32
	col    int32
	h, w   int32
	direct bool
	val    Value
}

func (a *condModelArg) cell(s *testSheet, i, j int32) Value {
	if a.direct {
		return a.val
	}
	tab := s.tabs[a.tab]
	p := Pos{Row: a.row + i, Col: a.col + j}
	if p.Row >= tab.rows || p.Col >= tab.cols {
		return Value{}
	}
	return s.Value(TabRef(a.tab), p)
}

// condModel calculates a conditional total the plain way: it reads every
// argument in the order it is written, and then every cell of the ranges,
// inside a tab or not, one by one.
func condModel(s *testSheet, kind condKind, valueFirst bool, value *condModelArg, ranges []condModelArg, criteria []Value) Value {
	// bad is the error an argument read as a range is: a tab that does
	// not exist, or a direct value that is an error.
	bad := func(a *condModelArg) (Value, bool) {
		switch {
		case a.tab < 0:
			return ErrorValue(ErrRef), true
		case a.direct && a.val.Kind() == KindError:
			return a.val, true
		}
		return Value{}, false
	}
	if valueFirst && value != nil {
		if v, ok := bad(value); ok {
			return v
		}
	}
	ev := &evaluator{env: &Env{Grid: s}}
	crits := make([]criterion, len(ranges))
	for k := range ranges {
		if v, ok := bad(&ranges[k]); ok {
			return v
		}
		if criteria[k].Kind() == KindError {
			return criteria[k]
		}
		if err := crits[k].set(ev, criteria[k]); err != ErrNone {
			return ErrorValue(err)
		}
	}
	if value != nil {
		if v, ok := bad(value); ok {
			return v
		}
	}
	h, w := ranges[0].h, ranges[0].w
	for k := range ranges {
		if ranges[k].h != h || ranges[k].w != w {
			return ErrorValue(ErrValue)
		}
	}
	if value != nil && (value.h != h || value.w != w) {
		return ErrorValue(ErrValue)
	}
	var total sum
	count, best := 0, 0.0
	for i := int32(0); i < h; i++ {
	cell:
		for j := int32(0); j < w; j++ {
			for k := range ranges {
				if !crits[k].match(ev, ranges[k].cell(s, i, j)) {
					continue cell
				}
			}
			if value == nil {
				count++
				continue
			}
			switch v := value.cell(s, i, j); v.Kind() {
			case KindError:
				return v
			case KindNumber:
				if count == 0 || (kind == condMax && v.Number() > best) || (kind == condMin && v.Number() < best) {
					best = v.Number()
				}
				total.add(v.Number())
				count++
			}
		}
	}
	switch kind {
	case condCount:
		return NumberValue(float64(count))
	case condSum:
		return NumberValue(total.value())
	case condAverage:
		if count == 0 {
			return ErrorValue(ErrDiv0)
		}
		return NumberValue(total.value() / float64(count))
	}
	return NumberValue(best)
}

// Random conditional totals over tabs of different sizes agree with the
// model: ranges that lie in their tabs, leave them at different places or
// lie outside, direct values, criteria that match blank cells or do not,
// errors in cells, in arguments and where a range is read, and ranges of
// different sizes.
func TestConditionalTotalsAgreeWithTheModel(t *testing.T) {
	rng := rand.New(rand.NewSource(20261113))
	contents := []*testCell{
		nil, nil, nil, numberCell(0), numberCell(1), numberCell(2), numberCell(3), textCell(""), textCell("a"), textCell("ab"),
		{content: Content{Kind: ContentBool, Num: 1}},
		{f: mustParse(t, "=NA()", "A1")}, {f: mustParse(t, "=1/0", "A1")},
	}
	sizes := [][2]int32{{6, 5}, {3, 7}, {8, 2}}
	names := []string{"Sheet1", "Wide", "Tall"}
	criteria := []struct {
		text string
		val  Value
	}{
		{`""`, TextValue("")}, {`"="`, TextValue("=")}, {`"<>"`, TextValue("<>")}, {`"a"`, TextValue("a")}, {`"<>a"`, TextValue("<>a")},
		{`"a*"`, TextValue("a*")}, {`"<>a*"`, TextValue("<>a*")}, {`"?"`, TextValue("?")}, {"1", NumberValue(1)}, {"0", NumberValue(0)},
		{`">0"`, TextValue(">0")}, {`"<>1"`, TextValue("<>1")}, {`"<2"`, TextValue("<2")}, {"TRUE", BoolValue(true)}, {"", Value{}},
		{`">"`, TextValue(">")}, {"NA()", ErrorValue(ErrNA)}, {"1/0", ErrorValue(ErrDiv0)},
	}
	directs := []struct {
		text string
		val  Value
	}{
		{"1", NumberValue(1)}, {"0", NumberValue(0)}, {`"a"`, TextValue("a")}, {`""`, TextValue("")}, {"TRUE", BoolValue(true)},
		{"NA()", ErrorValue(ErrNA)}, {"#REF!", ErrorValue(ErrRef)}, {"", Value{}},
	}
	for n := 0; n < 30000; n++ {
		sheet := &testSheet{}
		for i, size := range sizes {
			tab := &testTab{name: names[i], rows: size[0], cols: size[1], cells: map[Pos]*testCell{}}
			for r := int32(0); r < size[0]; r++ {
				for c := int32(0); c < size[1]; c++ {
					if cell := contents[rng.Intn(len(contents))]; cell != nil {
						tab.cells[Pos{Row: r, Col: c}] = &testCell{content: cell.content, f: cell.f}
					}
				}
			}
			sheet.tabs = append(sheet.tabs, tab)
		}
		h, w := int32(1+rng.Intn(4)), int32(1+rng.Intn(4))
		if rng.Intn(3) == 0 {
			h, w = 1, 1
		}
		arg := func() condModelArg {
			a := condModelArg{h: h, w: w}
			switch k := rng.Intn(40); {
			case k == 0:
				a.tab, a.text = -1, "Nope!A1"
				return a
			case k == 1:
				a.h++ // another size
			case k < 12 && h == 1 && w == 1, k == 2:
				d := directs[rng.Intn(len(directs))]
				return condModelArg{text: d.text, direct: true, val: d.val, h: 1, w: 1}
			}
			a.tab = rng.Intn(len(sizes))
			a.row, a.col = int32(rng.Intn(int(sizes[a.tab][0])+2)), int32(rng.Intn(int(sizes[a.tab][1])+2))
			first := Pos{Row: a.row, Col: a.col}
			last := Pos{Row: a.row + a.h - 1, Col: a.col + a.w - 1}
			a.text = names[a.tab] + "!" + first.A1() + ":" + last.A1()
			return a
		}
		fn := rng.Intn(8)
		kind := []condKind{condCount, condCount, condSum, condSum, condAverage, condAverage, condMax, condMin}[fn]
		name := []string{"COUNTIF", "COUNTIFS", "SUMIF", "SUMIFS", "AVERAGEIF", "AVERAGEIFS", "MAXIFS", "MINIFS"}[fn]
		single := fn == 0 || fn == 2 || fn == 4
		pairs := 1
		if !single {
			pairs = 1 + rng.Intn(4)
		}
		var parts []string
		var value *condModelArg
		if !single && kind != condCount {
			v := arg()
			value = &v
			parts = append(parts, v.text)
		}
		var ranges []condModelArg
		var crits []Value
		for k := 0; k < pairs; k++ {
			c := criteria[rng.Intn(len(criteria))]
			ranges = append(ranges, arg())
			crits = append(crits, c.val)
			parts = append(parts, ranges[k].text, c.text)
		}
		if single && kind != condCount {
			if rng.Intn(2) == 0 {
				v := arg()
				value = &v
				parts = append(parts, v.text)
			} else {
				value = &ranges[0]
			}
		}
		formula := "=" + name + "(" + strings.Join(parts, ",") + ")"
		want := condModel(sheet, kind, !single, value, ranges, crits)
		budget := NewBudget(nil)
		got := mustParse(t, formula, "A1").Eval(&Env{Grid: sheet, Budget: budget})
		if got != want {
			t.Fatalf("%s = %v, the model gives %v\n%s", formula, got, want, dumpTabs(sheet))
		}
	}
}

func dumpTabs(s *testSheet) string {
	var b strings.Builder
	for ti, tab := range s.tabs {
		fmt.Fprintf(&b, "%s, %d rows and %d columns:", tab.name, tab.rows, tab.cols)
		for r := int32(0); r < tab.rows; r++ {
			for c := int32(0); c < tab.cols; c++ {
				p := Pos{Row: r, Col: c}
				if tab.cells[p] != nil {
					fmt.Fprintf(&b, " %s=%v", p.A1(), jsonOf(s.Value(TabRef(ti), p)))
				}
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// heapProbe is a grid that measures the live heap while a formula is
// calculated: at every few cells the formula reads, after a collection.
type heapProbe struct {
	*testSheet
	every, reads int
	peak         uint64
}

func (g *heapProbe) live() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func (g *heapProbe) Value(tab TabRef, p Pos) Value {
	if g.reads++; g.reads%g.every == 0 {
		g.peak = max(g.peak, g.live())
	}
	return g.testSheet.Value(tab, p)
}

// A formula needs the memory of its nesting and of one text value (spec
// rule 9), however many criteria or direct values it has and however long
// they are: arguments are not held together while cells are read.
func TestManyLongArgumentsNeedTheMemoryOfOne(t *testing.T) {
	half := strings.Repeat("가", MaxTextLen/2)
	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", map[string]*testCell{
		"A1": textCell("x"),
		"A2": textCell("y"),
		"B1": textCell(strings.Repeat("*", MaxPatternLen)),
		"C1": textCell(half),
		"D1": textCell(strings.Repeat("*a?", MaxPatternLen/3)),
		"E1": numberCell(2),
	})}}
	repeat := func(head, pair string, n int, tail string) string {
		return head + strings.TrimSuffix(strings.Repeat(pair+",", n), ",") + tail
	}
	for _, c := range []struct {
		name, formula string
		want          Value
		builds        bool // the formula builds a long text for each argument
	}{
		{"1,000 patterns of 255 stars", repeat("=COUNTIFS(", "A1,B1", 1000, ")"), NumberValue(1), false},
		{"900 patterns over two cells", repeat("=COUNTIFS(", "A1:A2,B1", 900, ")"), NumberValue(2), false},
		{"600 calculated criteria", repeat("=COUNTIFS(", "A1:A2,C1&C1", 600, ")"), NumberValue(0), true},
		{"550 calculated criteria that match", repeat("=COUNTIFS(", `A1:A2,"<>"&C1`, 550, ")"), NumberValue(2), true},
		{"500 long patterns for a sum", repeat("=SUMIFS(E1:E2,", "A1:A2,D1", 500, ")"), NumberValue(0), false},
		{"500 long patterns for the least", repeat("=MINIFS(E1:E2,", `A1:A2,"<>"&D1`, 500, ")"), NumberValue(2), true},
		{"600 calculated direct ranges", repeat("=COUNTIFS(", `C1&C1,"<>x"`, 600, ",A1,B1)"), NumberValue(1), true},
		{"600 calculated direct values of a product", repeat("=SUMPRODUCT(", "C1&C1", 600, ",E1)"), NumberValue(0), true},
	} {
		f := mustParse(t, c.formula, "Z9")
		probe := &heapProbe{testSheet: sheet, every: 50}
		before := probe.live()
		var start, end runtime.MemStats
		runtime.ReadMemStats(&start)
		got := f.Eval(&Env{Grid: probe, At: pos(t, "Z9"), Budget: NewBudget(nil)})
		runtime.ReadMemStats(&end)
		if got != c.want {
			t.Errorf("%s: the value is %v, want %v", c.name, got, c.want)
		}
		// Criteria read from cells need no memory of their own at all:
		// what is allocated is the list of the ranges.
		if all := end.TotalAlloc - start.TotalAlloc; !c.builds && all > 1<<20 {
			t.Errorf("%s: calculating the formula allocated %d bytes", c.name, all)
		}
		if probe.reads < 500 {
			t.Fatalf("%s: the formula read %d cells, too few to measure", c.name, probe.reads)
		}
		// One text value is at most 131,068 bytes. The bound leaves room
		// for the test binary's own allocations.
		if grown := int64(probe.peak) - int64(before); grown > 2<<20 {
			t.Errorf("%s: the live heap grew by %d bytes while the formula was calculated", c.name, grown)
		}
	}
}

// An error value where a function reads a range is the result, as a range
// on a tab that does not exist is: the function does not read it as a cell
// that matches nothing or counts for nothing.
func TestErrorValueWhereARangeIsRead(t *testing.T) {
	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", map[string]*testCell{
		"A1": numberCell(1), "A2": numberCell(2), "B1": numberCell(9),
	})}}
	na, ref, div := ErrorValue(ErrNA), ErrorValue(ErrRef), ErrorValue(ErrDiv0)
	for formula, want := range map[string]Value{
		`=MATCH(1,#REF!,0)`:            ref,
		`=MATCH(1,#REF!)`:              ref,
		`=MATCH(1,Nope!A1:A3,0)`:       ref,
		`=MATCH(NA(),#REF!,0)`:         na,
		`=MATCH(1,#REF!,1/0)`:          ref,
		`=VLOOKUP(1,#REF!,1,FALSE)`:    ref,
		`=VLOOKUP(1,#REF!,1)`:          ref,
		`=VLOOKUP(1,Nope!A1:B3,1)`:     ref,
		`=VLOOKUP(NA(),#REF!,1,FALSE)`: na,
		`=VLOOKUP(1,#REF!,1/0,FALSE)`:  ref,
		`=HLOOKUP(1,#REF!,1,FALSE)`:    ref,
		`=HLOOKUP(1,1/0,1)`:            div,
		`=XLOOKUP(1,#REF!,A1:A2)`:      ref,
		`=XLOOKUP(1,A1:A2,#REF!)`:      ref,
		`=XLOOKUP(5,A1:A2,#REF!,"no")`: ref,
		`=XLOOKUP(1,#REF!,#N/A)`:       ref,
		`=XLOOKUP(NA(),#REF!,A1:A2)`:   na,
		`=XLOOKUP(1,Nope!A1:A2,A1:A2)`: ref,
		`=INDEX(#REF!,1)`:              ref,
		`=INDEX(#REF!,2)`:              ref,
		`=INDEX(#REF!,1/0)`:            ref,
		`=COUNTBLANK(#REF!)`:           ref,
		`=COUNTBLANK(Nope!A1)`:         ref,
		`=ROWS(#REF!)`:                 ref,
		`=COLUMNS(#REF!)`:              ref,
		`=ROWS(1/0)`:                   div,
		`=ROWS(Nope!A1)`:               ref,
		`=ROW(#REF!)`:                  ref,
		`=COLUMN(#REF!)`:               ref,
		`=ROW(NA())`:                   na,
		`=ROW(5)`:                      ErrorValue(ErrValue),
		`=RANK(1,#REF!)`:               ref,
		`=LARGE(#REF!,1)`:              ref,
		`=SMALL(#REF!,1/0)`:            ref,
		`=SUMPRODUCT(A1:A2,#REF!)`:     ref,
		`=SUM(#REF!)`:                  ref,
		`=SUM(A1:A2,#REF!)`:            ref,
		`=AVERAGE(#REF!)`:              ref,
		`=MEDIAN(#REF!)`:               ref,
		`=AND(#REF!)`:                  ref,
		`=CONCAT(#REF!)`:               ref,
		`=IFERROR(MATCH(1,#REF!,0),7)`: NumberValue(7),
		// a value that is no error is still a range of one cell
		`=MATCH(5,5,0)`:   NumberValue(1),
		`=ROWS(5)`:        NumberValue(1),
		`=COUNTBLANK("")`: NumberValue(1),
		`=XLOOKUP(5,5,7)`: NumberValue(7),
		`=INDEX(5,1)`:     NumberValue(5),
	} {
		if got := evalOn(t, sheet, formula); got != want {
			t.Errorf("%s = %v, want %v", formula, got, want)
		}
	}
}
