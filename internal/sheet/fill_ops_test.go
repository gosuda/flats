package sheet

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// fillRange is a fill of Sheet1 from the A1 range source over the A1
// range target, with mode when it is not empty.
func (e *env) fillRange(source, target, mode string) string {
	e.t.Helper()
	ids := func(rng string) string {
		r, err := formula.ParseRange(rng)
		if err != nil {
			e.t.Fatalf("range %q: %v", rng, err)
		}
		rows := e.rowsOf("Sheet1", int(r.First.Row)+1, int(r.Last.Row-r.First.Row)+1)
		cols := e.colsOf("Sheet1", int(r.First.Col)+1, int(r.Last.Col-r.First.Col)+1)
		return jsonf(`{"rows":%s,"cols":%s}`, rows, cols)
	}
	op := jsonf(`{"op":"fill","tab":%d,"source":%s,"target":%s`, e.tabID("Sheet1"), ids(source), ids(target))
	if mode != "" {
		op += jsonf(`,"mode":%q`, mode)
	}
	return op + "}"
}

// rangeTexts is what the cells of a range of Sheet1 hold, as the formula bar
// shows them, row by row.
func (e *env) rangeTexts(rng string) []string {
	e.t.Helper()
	r, err := formula.ParseRange(rng)
	if err != nil {
		e.t.Fatalf("range %q: %v", rng, err)
	}
	var out []string
	for row := r.First.Row; row <= r.Last.Row; row++ {
		for col := r.First.Col; col <= r.Last.Col; col++ {
			out = append(out, e.text(formula.ColumnName(col)+fmt.Sprint(row+1)))
		}
	}
	return out
}

// kindOf is the kind of content of a cell of Sheet1.
func (e *env) kindOf(addr string) formula.ContentKind {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t, row, col := e.at(addr)
	if c := t.cell(row, col); c != nil {
		return c.content.Kind
	}
	return formula.ContentBlank
}

// TestFillOpsModes drags the fill handle in auto and series modes over
// one line and over several, down, up, right and left (spec rule 21a,
// technical design, section 8.5). Each line is filled on its own from its
// own source cells; a line without a series is a copy of them.
func TestFillOpsModes(t *testing.T) {
	cases := []struct {
		name           string
		cells          string // the rows of an agent write at A1
		source, target string
		mode           string
		rng            string // the range read after the fill
		want           []string
	}{
		{"numbers down", `[[1],[2]]`, "A1:A2", "A3:A5", "auto", "A1:A5", []string{"1", "2", "3", "4", "5"}},
		{"numbers up", `[[null],[null],[null],[1],[2]]`, "A4:A5", "A1:A3", "auto", "A1:A5", []string{"-2", "-1", "0", "1", "2"}},
		// 16/3 and 41/6 as doubles, in full in the formula bar.
		{"a trend of three numbers", `[[1],[2],[4]]`, "A1:A3", "A4:A5", "auto", "A4:A5", []string{"5.333333333333333", "6.833333333333333"}},
		{"one number in auto is copied", `[[7]]`, "A1", "A2:A3", "auto", "A1:A3", []string{"7", "7", "7"}},
		{"one number in series adds 1", `[[7]]`, "A1", "A2:A3", "series", "A1:A3", []string{"7", "8", "9"}},
		{"one number in series up takes 1", `[[null],[null],[7]]`, "A3", "A1:A2", "series", "A1:A3", []string{"5", "6", "7"}},
		{"numbers right", `[[100,95]]`, "A1:B1", "C1:D1", "auto", "A1:D1", []string{"100", "95", "90", "85"}},
		{"numbers left", `[[null,null,1,2]]`, "C1:D1", "A1:B1", "auto", "A1:D1", []string{"-1", "0", "1", "2"}},
		{"a date down", `[[{"date":"2026-01-01"}]]`, "A1", "A2:A3", "auto", "A1:A3", []string{"2026-01-01", "2026-01-02", "2026-01-03"}},
		{"a date up", `[[null],[{"date":"2026-01-01"}]]`, "A2", "A1", "auto", "A1:A2", []string{"2025-12-31", "2026-01-01"}},
		{"weekday names", `[["Monday"],["Wednesday"]]`, "A1:A2", "A3:A4", "auto", "A3:A4", []string{"Friday", "Sunday"}},
		{"month names left", `[[null,null,"Jan","Apr"]]`, "C1:D1", "A1:B1", "auto", "A1:B1", []string{"Jul", "Oct"}},
		{"a text with a number down", `[["Item1"]]`, "A1", "A2:A3", "auto", "A2:A3", []string{"Item2", "Item3"}},
		{"a text with a number up", `[[null],[null],["Item1"]]`, "A3", "A1:A2", "auto", "A1:A2", []string{"Item1", "Item0"}},
		{"a formula line is copied with moved formulas", `[["=B1*2"]]`, "A1", "A2:A3", "auto", "A1:A3", []string{"=B1*2", "=B2*2", "=B3*2"}},
		{"a mixed line is copied in order", `[[1],["x"]]`, "A1:A2", "A3:A5", "auto", "A3:A5", []string{"1", "x", "1"}},
		{"a line with a blank is copied", `[[1],[null],[3]]`, "A1:A3", "A4:A6", "series", "A4:A6", []string{"1", "", "3"}},
		// Each column is a line of its own.
		{"three lines down", `[[1,"Mon","=C1"],[2,"Tue","=C2"]]`, "A1:C2", "A3:C4", "auto", "A3:C4",
			[]string{"3", "Wed", "=C3", "4", "Thu", "=C4"}},
		// Each row is a line of its own.
		{"two lines right", `[[1,2],["Item1","Item3"]]`, "A1:B2", "C1:D2", "auto", "C1:D2",
			[]string{"3", "4", "Item5", "Item7"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":%s}]}`, c.cells))
			alice := e.person("Alice")
			if ack := alice.do(e.fillRange(c.source, c.target, c.mode)); status(ack) != "applied" {
				t.Fatalf("the fill was answered %s", status(ack))
			}
			if got := e.rangeTexts(c.rng); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("%s holds %q, want %q", c.rng, got, c.want)
			}
		})
	}
	t.Run("a date series keeps its kind", func(t *testing.T) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[{"date":"2026-01-01"}]]}]}`)
		if ack := e.person("Alice").do(e.fillRange("A1", "A2", "auto")); status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		if k := e.kindOf("A2"); k != formula.ContentDate {
			t.Fatalf("A2 holds content of kind %d, want a date", k)
		}
	})
}

// TestFillOpsRepeatFormats fills two formatted numbers down and up: the
// series takes each cell's format from the source in the order a copy
// repeats it.
func TestFillOpsRepeatFormats(t *testing.T) {
	for _, c := range []struct {
		name, at, source, target string
		formats                  map[string]string
	}{
		{"down", "A1", "A1:A2", "A3:A5", map[string]string{"A3": `{"bold":true}`, "A4": `{"italic":true}`, "A5": `{"bold":true}`}},
		{"up", "A4", "A4:A5", "A1:A3", map[string]string{"A1": `{"bold":true}`, "A2": `{"italic":true}`, "A3": `{"bold":true}`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			first, err := formula.ParseRange(c.at)
			if err != nil {
				t.Fatal(err)
			}
			second := "A" + fmt.Sprint(first.First.Row+2)
			e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":%q,"rows":[[1],[2]]},
				{"op":"format","tab":"Sheet1","ranges":[%q],"format":{"bold":true}},
				{"op":"format","tab":"Sheet1","ranges":[%q],"format":{"italic":true}}]}`, c.at, c.at, second))
			if ack := e.person("Alice").do(e.fillRange(c.source, c.target, "auto")); status(ack) != "applied" {
				t.Fatal(status(ack))
			}
			for addr, want := range c.formats {
				if got := e.formatOf("Sheet1", addr); !sameJSON([]byte(got), []byte(want)) {
					t.Errorf("%s has the format %s, want %s", addr, got, want)
				}
			}
		})
	}
}

// TestFillOpsUndoAsOneAction fills three lines of different kinds in one
// fill, and one undo takes every cell of it back.
func TestFillOpsUndoAsOneAction(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,"Mon","=A1"],[2,"Tue","x"]]}]}`)
	alice := e.person("Alice")
	before := e.contents()
	if ack := alice.do(e.fillRange("A1:C2", "A3:C5", "auto")); status(ack) != "applied" || ack.Results[0].Applied != 9 {
		t.Fatalf("the fill was answered %+v", ack.Results)
	}
	if got, want := e.rangeTexts("A3:C5"), []string{"3", "Wed", "=A3", "4", "Thu", "x", "5", "Fri", "=A5"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("A3:C5 holds %q, want %q", got, want)
	}
	if u := undo(t, alice); u.Applied == 0 {
		t.Fatalf("undo of the fill = %+v", u)
	}
	if got := e.rangeTexts("A3:C5"); !reflect.DeepEqual(got, make([]string, 9)) {
		t.Fatalf("after one undo A3:C5 holds %q", got)
	}
	if after := e.contents(); after != before {
		t.Fatalf("one undo left the sheet changed: %s", dumpDiff(before, after))
	}
}

// TestFillOpsF04 is case F-04 of section 22.2: a fill whose target is
// larger than the limit of an action is refused whole, and one whose
// source row is deleted before it applies leaves every target cell as
// ignored, in every mode as a copy does.
func TestFillOpsF04(t *testing.T) {
	for _, mode := range []string{"", "copy", "auto", "series"} {
		t.Run("over the limit/"+mode, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1],[2]]}]}`)
			before := e.dump()
			// The target names more cells than an action may write: the
			// most rows of a tab, of ids that are not there, in six columns.
			op := jsonf(`{"op":"fill","tab":%d,"source":{"rows":%s,"cols":%s},"target":{"rows":[[%d,%d]],"cols":[[%d,6]]}`,
				e.tabID("Sheet1"), e.rowsOf("Sheet1", 1, 2), e.colsOf("Sheet1", 1, 1), 1<<20, formula.MaxRows, 1<<20)
			if mode != "" {
				op += jsonf(`,"mode":%q`, mode)
			}
			if ack := e.person("Alice").do(op + "}"); status(ack) != "refused:cells" {
				t.Fatalf("the fill was answered %s, want refused:cells", status(ack))
			}
			if after := e.dump(); after != before {
				t.Fatalf("the refused fill changed the sheet: %s", dumpDiff(before, after))
			}
		})
		t.Run("source deleted/"+mode, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1],[2],[3]]}]}`)
			alice := e.person("Alice")
			fill := e.fillRange("A1", "A2:A3", mode)
			alice.do(e.deleteRowsOp("Sheet1", 1, 1))
			seq := e.seq()
			ack := alice.do(fill)
			if r := ack.Results; len(r) != 1 || r[0].Status != "applied" || r[0].Applied != 0 || r[0].Ignored != 2 {
				t.Fatalf("the fill was answered %+v, want both target cells ignored", ack.Results)
			}
			if e.seq() != seq {
				t.Fatalf("the fill that wrote nothing took sequence %d", e.seq())
			}
			if got := e.rangeTexts("A1:A2"); !reflect.DeepEqual(got, []string{"2", "3"}) {
				t.Fatalf("A1:A2 holds %q", got)
			}
		})
	}
}

// TestFillOpsF05 is case F-05 of section 22.2: a fill in auto whose target
// is both below and beside its source is refused as invalid, and one whose
// series makes a text of 32,768 characters is refused with text_length.
// The same target in copy applies as a copy does.
func TestFillOpsF05(t *testing.T) {
	sides := []struct{ name, source, target string }{
		{"below and beside", "A1:A2", "B3:B4"},
		{"over its source", "A1:A2", "A2:A4"},
		{"around its source", "A2", "A1:A3"},
		{"its own source", "A1:A2", "A1:A2"},
	}
	for _, c := range sides {
		for _, mode := range []string{"auto", "series"} {
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				e := start(t)
				e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1],[2]]}]}`)
				before := e.dump()
				if ack := e.person("Alice").do(e.fillRange(c.source, c.target, mode)); status(ack) != "refused:invalid" {
					t.Fatalf("the fill was answered %s, want refused:invalid", status(ack))
				}
				if after := e.dump(); after != before {
					t.Fatalf("the refused fill changed the sheet: %s", dumpDiff(before, after))
				}
			})
		}
		t.Run(c.name+"/copy", func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1],[2]]}]}`)
			alice := e.person("Alice")
			if ack := alice.do(e.fillRange(c.source, c.target, "copy")); status(ack) != "applied" {
				t.Fatalf("the fill in copy was answered %s", status(ack))
			}
			copied := e.contents()
			undo(t, alice)
			if ack := alice.do(jsonf(`{"op":"copy","from":{"tab":%d,%s},"to":{"tab":%d,%s},"mode":"all"}`,
				e.tabID("Sheet1"), rangeMembers(e, c.source), e.tabID("Sheet1"), rangeMembers(e, c.target))); status(ack) != "applied" {
				t.Fatalf("the copy was answered %s", status(ack))
			}
			if got := e.contents(); got != copied {
				t.Fatalf("the fill in copy and the copy differ: %s", dumpDiff(copied, got))
			}
		})
	}
	t.Run("text_length", func(t *testing.T) {
		e := start(t)
		long := strings.Repeat("x", formula.MaxTextLen-1) + "9"
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[%q]]}]}`, long))
		alice := e.person("Alice")
		before := e.dump()
		for _, mode := range []string{"auto", "series"} {
			if ack := alice.do(e.fillRange("A1", "A2:A3", mode)); status(ack) != "refused:text_length" {
				t.Fatalf("the fill in %s was answered %s, want refused:text_length", mode, status(ack))
			}
			if after := e.dump(); after != before {
				t.Fatalf("the refused fill changed the sheet: %s", dumpDiff(before, after))
			}
		}
		if ack := alice.do(e.fillRange("A1", "A2:A3", "copy")); status(ack) != "applied" {
			t.Fatalf("the fill in copy was answered %s", status(ack))
		}
		if e.text("A3") != long {
			t.Fatal("the fill in copy did not repeat the source")
		}
	})
}

// TestFillOpsSeriesPastTheSheetText fills a column of a long text that ends
// in a number down 1,999 rows: its series would pass the typed text of a
// sheet. The fill is refused with that limit before it builds a text or
// writes a cell, and a copy of the same cells is refused at its end with
// the same limit, after it wrote every one.
func TestFillOpsSeriesPastTheSheetText(t *testing.T) {
	const rows = 2000
	e := start(t)
	tallTab(e, rows)
	long := strings.Repeat("x", 32000) + "1"
	e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[%q]]}]}`, long))
	alice := e.person("Alice")
	deadline := e.workDeadline()
	before := e.dump()
	fill := func(mode string) int64 {
		got, w := deadline.run(never, func() string { return status(alice.do(e.fillRange("A1", fmt.Sprintf("A2:A%d", rows), mode))) })
		if got != "refused:sheet_text" {
			t.Fatalf("the fill in %s was answered %s, want refused:sheet_text", mode, got)
		}
		if after := e.dump(); after != before {
			t.Fatalf("the refused fill in %s changed the sheet: %s", mode, dumpDiff(before, after))
		}
		return w.used
	}
	copied := fill("copy")
	// Writing the cells counts the bytes of their text.
	if least := int64(rows-1) * int64(len(long)) / 64; copied < least {
		t.Fatalf("the copy counted %d units, for at least %d", copied, least)
	}
	for _, mode := range []string{"auto", "series"} {
		if used := fill(mode); used > copied/100 {
			t.Errorf("the fill in %s counted %d units before its refusal, and the copy %d: it built its series", mode, used, copied)
		}
	}
}

// contents is the dump of the sheet without its sequences: two ways to the
// same cells compare equal.
func (e *env) contents() string {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	return dumpWorkbook(e.in.wb, false)
}

// rangeMembers is the rows and cols members of an A1 range of Sheet1.
func rangeMembers(e *env, rng string) string {
	e.t.Helper()
	r, err := formula.ParseRange(rng)
	if err != nil {
		e.t.Fatalf("range %q: %v", rng, err)
	}
	return jsonf(`"rows":%s,"cols":%s`, e.rowsOf("Sheet1", int(r.First.Row)+1, int(r.Last.Row-r.First.Row)+1),
		e.colsOf("Sheet1", int(r.First.Col)+1, int(r.Last.Col-r.First.Col)+1))
}

// TestFillOpsModeIsRead holds the decoder to the modes of a fill: copy,
// auto and series are read, a fill without a mode is a copy, and any other
// mode refuses the operation as invalid while the rest of the action is
// applied.
func TestFillOpsModeIsRead(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1],[2]]}]}`)
	alice := e.person("Alice")
	for _, mode := range []string{"Auto", "all", "values", "", " copy"} {
		op := strings.Replace(e.fillRange("A1:A2", "A3:A4", "auto"), `"mode":"auto"`, jsonf(`"mode":%q`, mode), 1)
		if ack := alice.do(op); status(ack) != "refused:invalid" {
			t.Errorf("a fill in mode %q was answered %s, want refused:invalid", mode, status(ack))
		}
	}
	if got := e.rangeTexts("A3:A4"); !reflect.DeepEqual(got, []string{"", ""}) {
		t.Fatalf("a refused fill wrote %q", got)
	}
	if ack := alice.do(e.fillRange("A1:A2", "A3:A4", "")); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	if got := e.rangeTexts("A3:A4"); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Fatalf("a fill without a mode wrote %q, want the copy", got)
	}
}

// TestFillOpsMeetTheDeadline fills a series of long texts that end in a
// number: each text it builds is counted, and so is the record it writes,
// and the deadline ends the fill a step after it passed, with nothing
// changed.
func TestFillOpsMeetTheDeadline(t *testing.T) {
	_, length := workSize()
	length = min(length, 16384)
	least := int64(newTabRows-1) * 2 * int64(length) / 64
	at, step := int64(4*budgetCheck), int64(2*budgetCheck)
	if least < 8*(at+2*step) {
		t.Fatalf("the whole fill counts at least %d units, too little to tell a stop from its end", least)
	}
	e := start(t)
	alice := e.person("Alice")
	alice.set("A1", strings.Repeat("x", length-1)+"1")
	deadline := e.workDeadline()
	op := e.fillRange("A1", fmt.Sprintf("A2:A%d", newTabRows), "auto")
	got, whole := deadline.run(never, func() string { return status(alice.do(op)) })
	if got != "applied" {
		t.Fatalf("the fill: %s", got)
	}
	if whole.used < least {
		t.Errorf("the fill counted %d units of work, for at least %d", whole.used, least)
	}
	if e.text(fmt.Sprintf("A%d", newTabRows)) != strings.Repeat("x", length-1)+fmt.Sprint(newTabRows) {
		t.Fatal("the fill did not continue the number")
	}
	undo(t, alice)
	before, kept := e.dump(), e.memory("Sheet1", alice)
	deadline.refusedAt(t, at, step, func() string { return status(alice.do(op)) })
	if after := e.dump(); after != before {
		t.Fatalf("the refused fill changed the sheet: %s", dumpDiff(before, after))
	}
	if !kept.equal(e.memory("Sheet1", alice)) {
		t.Errorf("the refused fill changed the structural log or the undo stacks")
	}
}
