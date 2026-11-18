package formula

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

// liveHeap returns the bytes of the heap that are in use after a
// collection.
func liveHeap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// A result that is kept takes the memory of its own text and no more: a
// function may cut it from a long text it built on the way, or build it
// with room to spare, and none of that stays alive with the result.
func TestResultsKeepOnlyTheirOwnText(t *testing.T) {
	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", map[string]*testCell{
		"A1": textCell(strings.Repeat("가", MaxTextLen)),
		"A2": textCell(strings.Repeat("A", MaxTextLen)),
		"A3": numberCell(5),
	})}}
	long := `REPT("가",32767)`
	const runs = 400
	for _, c := range []struct {
		formula string
		chars   int // the characters of the result
	}{
		{"=LEFT(" + long + ",1)", 1},
		{"=LEFT(" + long + ")", 1},
		{"=RIGHT(" + long + ",2)", 2},
		{"=MID(" + long + ",5,3)", 3},
		{`=TRIM(REPT(" ",32766)&"a")`, 1},
		{`=TRIM(" "&REPT("a ",16000)&"  b")`, 32001}, // built with room for the text before trimming
		{"=LEFT(" + long + `,1)&""`, 1},
		{`=""&MID(` + long + ",2,1)", 1},
		{"=IF(TRUE,MID(" + long + ",2,1))", 1},
		{"=IFS(FALSE,1,TRUE,RIGHT(" + long + "))", 1},
		{"=IFERROR(LEFT(" + long + ",1),0)", 1},
		{"=IFNA(LEFT(" + long + ",1),0)", 1},
		{"=CHOOSE(2,0,LEFT(" + long + ",1))", 1},
		{"=SWITCH(1,1,LEFT(" + long + ",1))", 1},
		{"=INDEX(LEFT(" + long + ",1),1)", 1},
		{"=XLOOKUP(5,5,LEFT(" + long + ",1))", 1},
		{"=VLOOKUP(LEFT(" + long + ",1),LEFT(" + long + ",1),1,FALSE)", 1},
		{"=SUBSTITUTE(LEFT(" + long + `,1),"x","y")`, 1},
		{"=SUBSTITUTE(LEFT(" + long + `,1),"","y")`, 1},
		{"=UPPER(LEFT(" + long + ",1))", 1},
		{"=LOWER(LEFT(" + long + ",1))", 1},
		{"=PROPER(LEFT(" + long + ",1))", 1},
		{"=TEXT(LEFT(" + long + `,1),"0")`, 1},
		{"=REPT(LEFT(" + long + ",1),1)", 1},
		{"=CONCAT(LEFT(" + long + ",1))", 1},
		{"=CONCATENATE(LEFT(" + long + ",1))", 1},
		{`=TEXTJOIN("",TRUE,LEFT(` + long + ",1))", 1},
		{`=TEXTJOIN(",",TRUE,LEFT(A1,1),LEFT(A1,1),LEFT(A1,1))`, 5},
		{`=REPLACE(LEFT(` + long + `,2),1,1,"")`, 1},
		{"=LEFT(TRIM(A1),1)", 1},
		{`=LEFT(UPPER(REPT("a",32767)),2)`, 2},
		{`=MID(SUBSTITUTE(A2,"A","b"),7,1)`, 1},
		{"=LEFT(A1,3)", 3},
		{"=RIGHT(A2,3)", 3},
		// A text of no characters may still be cut from a long one, or be
		// a builder's room, and keep that alive.
		{"=LEFT(" + long + ",0)", 0},
		{"=RIGHT(" + long + ",0)", 0},
		{"=MID(" + long + ",5,0)", 0},
		{"=MID(" + long + ",40000,1)", 0},
		{`=TRIM(REPT(" ",32767))`, 0},
		{`=SUBSTITUTE(REPT("a",32767),"a","")`, 0},
		{"=REPLACE(" + long + `,1,32767,"")`, 0},
		{"=IF(TRUE,LEFT(" + long + ",0))", 0},
		{"=LEFT(" + long + `,0)&""`, 0},
		{"=CONCAT(LEFT(" + long + ",0))", 0},
		{`=TEXTJOIN(",",TRUE,LEFT(` + long + ",0))", 0},
		{"=UPPER(LEFT(" + long + ",0))", 0},
		{"=LOWER(MID(" + long + ",5,0))", 0},
		{"=PROPER(RIGHT(" + long + ",0))", 0},
		{"=REPT(LEFT(" + long + ",1),0)", 0},
		{`=TEXT(LEFT(` + long + `,0),"0")`, 0},
	} {
		f := mustParse(t, c.formula, "Z9")
		env := &Env{Grid: sheet, At: pos(t, "Z9")}
		kept := make([]Value, 0, runs)
		before := liveHeap()
		for i := 0; i < runs; i++ {
			kept = append(kept, f.Eval(env))
		}
		grown := int64(liveHeap()) - int64(before)
		if got := kept[runs-1]; got.Kind() != KindText || got.TextLen() != c.chars {
			t.Errorf("%s = %v, want a text of %d characters", c.formula, got, c.chars)
		}
		// A character takes at most 4 bytes, and the allocator rounds a
		// text up. The rest is room for what the test itself keeps.
		if most := int64(runs)*(int64(c.chars)*5+64) + 1<<20; grown > most {
			t.Errorf("%s: %d results of %d characters keep %d bytes alive, more than %d", c.formula, runs, c.chars, grown, most)
		}
		runtime.KeepAlive(kept)
	}
}

// An empty result lies in no memory at all: not in the text of the cell it
// was cut from, which the result outlives. Each run reads a cell of its
// own, which is let go after it.
func TestEmptyResultsKeepNoCellText(t *testing.T) {
	for _, formula := range []string{"=LEFT(A1,0)", "=RIGHT(A1,0)", "=MID(A1,1,0)", "=MID(A1,40000,1)", `=SUBSTITUTE(A1,"가","")`, "=IF(TRUE,LEFT(A1,0))"} {
		f := mustParse(t, formula, "Z9")
		const runs = 400
		kept := make([]Value, 0, runs)
		before := liveHeap()
		for range runs {
			sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", map[string]*testCell{"A1": textCell(strings.Repeat("가", MaxTextLen))})}}
			kept = append(kept, f.Eval(&Env{Grid: sheet, At: pos(t, "Z9")}))
		}
		grown := int64(liveHeap()) - int64(before)
		if got := kept[runs-1]; got.Kind() != KindText || got.Text() != "" {
			t.Errorf("%s = %v, want an empty text", formula, got)
		}
		if most := int64(runs)*64 + 1<<20; grown > most {
			t.Errorf("%s: %d empty results keep %d bytes alive, more than %d", formula, runs, grown, most)
		}
		runtime.KeepAlive(kept)
	}
}

// An empty text value is the empty string constant, whose data is no
// memory at all, whatever text it was cut from or built in.
func TestEmptyTextsLieInNoMemory(t *testing.T) {
	long := strings.Repeat("a", 1000)
	if v := TextValue(long[500:500]); unsafe.StringData(v.Text()) != nil {
		t.Error("an empty text cut from a longer one keeps its memory")
	}
	typed := ("'" + long)[:1]
	if c, _ := ParseTyped(typed); c.Kind != ContentText || unsafe.StringData(c.Text) != nil {
		t.Error("the text after an apostrophe alone keeps the memory of what was typed")
	}
	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", map[string]*testCell{"A1": textCell(long)})}}
	env := &Env{Grid: sheet, At: pos(t, "Z9")}
	for _, formula := range []string{`=""`, `=""` + strings.Repeat(" ", 100), `=("")`, `=IF(TRUE,"")`, "=LEFT(A1,0)", `=TRIM(" ")`, `=SUBSTITUTE(A1,"a","")`, `=LEFT(REPT("a",100),0)`} {
		v := mustParse(t, formula, "Z9").Eval(env)
		if v.Kind() != KindText || v.Text() != "" {
			t.Fatalf("%s = %v", formula, v)
		}
		if unsafe.StringData(v.Text()) != nil {
			t.Errorf("%s is an empty text that keeps memory", formula)
		}
	}
}

// A result that is the text of a cell, or text written in the formula, is
// that very text: it is not copied for each formula that gives it.
func TestResultsShareTheTextOfCells(t *testing.T) {
	text := strings.Repeat("가", MaxTextLen)
	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", map[string]*testCell{"A1": textCell(text)})}}
	env := &Env{Grid: sheet, At: pos(t, "Z9")}
	for _, formula := range []string{"=A1", "=(A1)", `="written in the formula"`} {
		f := mustParse(t, formula, "Z9")
		if n := testing.AllocsPerRun(20, func() { f.Eval(env) }); n != 0 {
			t.Errorf("%s allocates %v times", formula, n)
		}
	}
	if got := mustParse(t, "=A1", "Z9").Eval(env); got != TextValue(text) {
		t.Errorf("=A1 is not the text of A1")
	}
}

// A formula that is one text keeps that text in memory of its own, so a
// value that outlives the formula, in another cell, a capture or an undo
// payload, keeps no more than its own bytes: not the stored text around
// it, which spaces may make 8,000 bytes long for a value of one.
func TestLiteralValueOutlivesItsFormula(t *testing.T) {
	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", nil)}}
	env := &Env{Grid: sheet, At: pos(t, "Z9")}
	for _, text := range []string{`="1"`, `="a""b"`, `=("1")`, `=""`, `=("")`} {
		padded := text + strings.Repeat(" ", 8000-len(text))
		const runs = 400
		kept := make([]Value, 0, runs)
		before := liveHeap()
		for range runs {
			f := mustParse(t, padded, "Z9")
			kept = append(kept, f.Eval(env))
		}
		grown := int64(liveHeap()) - int64(before)
		if got := kept[0]; got.Kind() != KindText || got.TextLen() > 3 {
			t.Fatalf("%s = %v", text, got)
		}
		if most := int64(runs)*64 + 1<<20; grown > most {
			t.Errorf("%s: %d values of its formulas, which are gone, keep %d bytes alive, more than %d", text, runs, grown, most)
		}
		runtime.KeepAlive(kept)
	}
}

// Typed entry keeps no part of the text typed: a currency format that a
// cell's style keeps for an amount of 32,767 characters holds a sign of
// its own, and a text after an apostrophe lies in memory of its own size,
// which is what a holder of it is charged for.
func TestTypedEntryKeepsNoInput(t *testing.T) {
	for _, c := range []struct {
		name  string
		input func(i int) string
		sign  string // of a currency format
		text  int    // bytes of the text the content holds
		runs  int
	}{
		{name: "$", input: amount("$"), sign: "$", runs: 400},
		{name: "-$", input: amount("-$"), sign: "$", runs: 400},
		{name: "€", input: amount("€"), sign: "€", runs: 400},
		{name: "£", input: amount("£"), sign: "£", runs: 400},
		{name: "¥", input: amount("¥"), sign: "¥", runs: 400},
		{name: "₩", input: amount("₩"), sign: "₩", runs: 400},
		// One byte more than a size class of the allocator: the text after
		// the apostrophe fills the class, and the typed text needs the next.
		{name: "apostrophe", input: func(i int) string { return "'" + strings.Repeat("a", 4096) }, text: 4096, runs: 4000},
	} {
		contents := make([]Content, 0, c.runs)
		formats := make([]*NumberFormat, 0, c.runs)
		before := liveHeap()
		for i := 0; i < c.runs; i++ {
			content, nf := ParseTyped(c.input(i))
			contents, formats = append(contents, content), append(formats, nf)
		}
		grown := int64(liveHeap()) - int64(before)
		switch nf := formats[0]; {
		case c.sign != "" && (nf == nil || nf.Kind != NumberCurrency || nf.Symbol != c.sign):
			t.Fatalf("%s: the format is %+v, want a currency format with %s", c.name, nf, c.sign)
		case c.text != 0 && (contents[0].Kind != ContentText || len(contents[0].Text) != c.text):
			t.Fatalf("%s: the content is a %v of %d bytes", c.name, contents[0].Kind, len(contents[0].Text))
		}
		// The rest is room for what the test itself keeps.
		if most := int64(c.runs)*(textBytes(c.text)+64) + 1<<20; grown > most {
			t.Errorf("%s: %d typed entries keep %d bytes alive, more than %d", c.name, c.runs, grown, most)
		}
		runtime.KeepAlive(contents)
		runtime.KeepAlive(formats)
	}
}

// amount returns typed text of MaxTextLen bytes: the prefix, then zeros,
// then a number of its own for each i.
func amount(prefix string) func(i int) string {
	return func(i int) string {
		n := strconv.Itoa(i + 1)
		return prefix + strings.Repeat("0", MaxTextLen-len(prefix)-len(n)) + n
	}
}

// The names Tabs gives are texts of their own: a holder that keeps one
// after the formula is gone, as an undo entry does for the tabs its
// formulas name, keeps the bytes of the name and not the formula's text,
// which spaces may make 8,000 bytes long.
func TestTabNamesKeepNoFormulaText(t *testing.T) {
	for _, c := range []struct{ formula, name string }{
		{"=other!A1", "other"},
		{"=Other!A1", "Other"},
		{"='my tab'!A1", "my tab"},
		{"='it''s'!A1", "it's"},
	} {
		padded := c.formula + strings.Repeat(" ", 8000-len(c.formula))
		const runs = 400
		kept := make([]string, 0, runs)
		before := liveHeap()
		for range runs {
			mustParse(t, padded, "Z9").Tabs(func(name string) { kept = append(kept, FoldName(name)) })
		}
		grown := int64(liveHeap()) - int64(before)
		if len(kept) != runs || !strings.EqualFold(kept[0], c.name) {
			t.Fatalf("%s names %q", c.formula, kept)
		}
		if most := int64(runs)*64 + 1<<20; grown > most {
			t.Errorf("%s: %d names of its formulas, which are gone, keep %d bytes alive, more than %d", c.formula, runs, grown, most)
		}
		runtime.KeepAlive(kept)
	}
}
