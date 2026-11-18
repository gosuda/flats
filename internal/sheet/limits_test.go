package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// The limit cases of the technical design, section 22.2: one over each
// limit is refused whole with the limit's name, and the write at the limit
// applies.

func wantLimit(t *testing.T, e *env, call, limit string) {
	t.Helper()
	seq := e.seq()
	_, err := e.agent(call)
	e.wantRefusal(err, "sheet_capacity", limit)
	if e.seq() != seq {
		t.Fatalf("the refused write took a sequence")
	}
}

func TestLimitTabs(t *testing.T) {
	// L-01
	e := start(t)
	var ops []string
	for i := 2; i <= 50; i++ {
		ops = append(ops, fmt.Sprintf(`{"op":"add_tab","name":"T%d"}`, i))
	}
	e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	wantLimit(t, e, `{"ops":[{"op":"add_tab","name":"T51"}]}`, "tabs")
	alice := e.person("Alice")
	if ack := alice.do(`{"op":"add_tab","name":"T51"}`); status(ack) != "refused:tabs" {
		t.Fatal(status(ack))
	}
	// Undo of a tab deletion does not pass the limit either.
	alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("T50")))
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"T50"}]}`)
	if ack := alice.do(`{"op":"undo"}`); status(ack) != "refused:tabs" {
		t.Fatal(status(ack))
	}
}

func TestLimitRowsAndColumns(t *testing.T) {
	e := start(t)
	// L-02: row 100,001 by write, insert, append and CSV import.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A100000","rows":[["last"]]}]}`)
	wantLimit(t, e, `{"ops":[{"op":"write","tab":"Sheet1","at":"A100000","rows":[[1],[2]]}]}`, "rows")
	wantLimit(t, e, `{"ops":[{"op":"insert_rows","tab":"Sheet1","at":5,"count":1}]}`, "rows")
	wantLimit(t, e, `{"ops":[{"op":"import_csv","tab":"Sheet1","at":"A100000","csv":"1\n2\n"}]}`, "rows")
	alice := e.person("Alice")
	if ack := alice.do(jsonf(`{"op":"append_rows","tab":%d,"count":1}`, e.tabID("Sheet1"))); status(ack) != "refused:rows" {
		t.Fatal(status(ack))
	}
	e.want("A100000", "last")
	// L-03: column 703.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"ZZ1","rows":[["edge"]]}]}`)
	wantLimit(t, e, `{"ops":[{"op":"write","tab":"Sheet1","at":"ZZ1","rows":[[1,2]]}]}`, "columns")
	wantLimit(t, e, `{"ops":[{"op":"insert_cols","tab":"Sheet1","at":"B","count":1}]}`, "columns")
	if ack := alice.do(jsonf(`{"op":"append_cols","tab":%d,"count":1}`, e.tabID("Sheet1"))); status(ack) != "refused:columns" {
		t.Fatal(status(ack))
	}
	// Undo of a deletion does not pass the limit: delete a row, fill the
	// tab again, undo.
	alice.do(e.deleteRowsOp("Sheet1", 7, 1))
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":3,"count":1}]}`)
	if ack := alice.do(`{"op":"undo"}`); status(ack) != "refused:rows" {
		t.Fatal(status(ack))
	}
}

func TestLimitCells(t *testing.T) {
	// L-04: cell 500,001 by content and by a format on a range.
	heavy(t, "holds 500,000 cells")
	e := start(t, unhurried)
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1001,"count":19000},
		{"op":"format","tab":"Sheet1","ranges":["A1:Y20000"],"format":{"bold":true}}]}`)
	wantLimit(t, e, `{"ops":[{"op":"format","tab":"Sheet1","ranges":["Z1"],"format":{"italic":true}}]}`, "cells")
	wantLimit(t, e, `{"ops":[{"op":"write","tab":"Sheet1","at":"Z1","rows":[[1]]}]}`, "cells")
	// Content in a cell that already counts is no new cell.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]]}]}`)
	alice := e.person("Alice")
	if ack := alice.set("Z9", "x"); status(ack) != "refused:cells" {
		t.Fatal(status(ack))
	}
	// A cleared cell without a format stops counting.
	e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1"],"formats":true}]}`)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"Z1","rows":[[1]]}]}`)
}

func TestLimitFormulas(t *testing.T) {
	// L-05: formula 100,001.
	heavy(t, "holds 100,000 formulas")
	e := start(t)
	row := `[` + strings.TrimSuffix(strings.Repeat(`"=1",`, 25), ",") + `]`
	block := `{"op":"write","tab":"Sheet1","at":"A%d","rows":[` + strings.TrimSuffix(strings.Repeat(row+",", 2000), ",") + `]}`
	e.mustAgent(`{"ops":[` + fmt.Sprintf(block, 1) + `]}`)
	e.mustAgent(`{"ops":[` + fmt.Sprintf(block, 2001) + `]}`)
	wantLimit(t, e, `{"ops":[{"op":"write","tab":"Sheet1","at":"Z1","rows":[["=2"]]}]}`, "formulas")
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["=2"]]}]}`)
}

func TestLimitTextAndFormula(t *testing.T) {
	e := start(t)
	alice := e.person("Alice")
	at, over := strings.Repeat("가", 32767), strings.Repeat("가", 32768)
	// L-06: text of 32,768 code points by typing, agent write, paste and CSV.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["` + at + `"]]}]}`)
	wantLimit(t, e, `{"ops":[{"op":"write","tab":"Sheet1","at":"A2","rows":[["`+over+`"]]}]}`, "text_length")
	wantLimit(t, e, `{"ops":[{"op":"import_csv","tab":"Sheet1","at":"A2","csv":"`+over+`"}]}`, "text_length")
	e.mustAgent(`{"ops":[{"op":"import_csv","tab":"Sheet1","at":"A2","csv":"` + at + `"}]}`)
	if ack := alice.set("A3", over); status(ack) != "refused:text_length" {
		t.Fatal(status(ack))
	}
	if ack := alice.set("A3", at); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	paste := jsonf(`{"op":"write","tab":%d,"rows":%s,"cols":%s,"values":[["ok"],[%q]]}`, e.tabID("Sheet1"), e.rowsOf("Sheet1", 4, 2), e.colsOf("Sheet1", 1, 1), over)
	if ack := alice.do(paste); status(ack) != "refused:text_length" {
		t.Fatal(status(ack))
	}
	e.want("A4", "")
	// L-07: a formula of 8,193 characters.
	long := "=1" + strings.Repeat("+1", 4095)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[["` + long + `"]]}]}`)
	wantLimit(t, e, `{"ops":[{"op":"write","tab":"Sheet1","at":"B2","rows":[["`+long+` "]]}]}`, "formula_length")
	if ack := alice.set("B2", long+" "); status(ack) != "refused:formula_length" {
		t.Fatal(status(ack))
	}
	// L-08: nesting of 65.
	nest := func(n int) string { return "=" + strings.Repeat("(", n) + "1" + strings.Repeat(")", n) }
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"C1","rows":[["` + nest(64) + `"]]}]}`)
	wantLimit(t, e, `{"ops":[{"op":"write","tab":"Sheet1","at":"C2","rows":[["`+nest(65)+`"]]}]}`, "formula_depth")
	e.want("C1", "1")
}

func TestLimitReads(t *testing.T) {
	e := start(t)
	// L-09: a formula that reads 1,000,000 cells is at the limit, and one
	// that reads more is refused, directly.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"Z1","rows":[["=SUM(A1:J100000)"]]}]}`)
	wantLimit(t, e, `{"ops":[{"op":"write","tab":"Sheet1","at":"Z2","rows":[["=SUM(A1:K90910)"]]}]}`, "formula_reads")
	// And after a row insert under a whole-column reference.
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Wide"},{"op":"write","tab":"Wide","at":"CV10000","rows":[[1]]},
		{"op":"write","tab":"Sheet1","at":"Y1","rows":[["=SUM(Wide!A:CV)"]]}]}`)
	wantLimit(t, e, `{"ops":[{"op":"insert_rows","tab":"Wide","at":5,"count":1}]}`, "formula_reads")
	alice := e.person("Alice")
	if ack := alice.do(jsonf(`{"op":"append_rows","tab":%d,"count":1}`, e.tabID("Wide"))); status(ack) != "refused:formula_reads" {
		t.Fatal(status(ack))
	}
	// L-10: formulas that read 20,000,001 cells together.
	e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["Y1"]}]}`)
	var cells []string
	for i := 0; i < 19; i++ {
		cells = append(cells, `"=SUM(A1:J100000)"`)
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"AA1","rows":[[` + strings.Join(cells, ",") + `]]}]}`)
	wantLimit(t, e, `{"ops":[{"op":"write","tab":"Sheet1","at":"Z3","rows":[["=A1"]]}]}`, "sheet_reads")
}

func TestLimitSheetText(t *testing.T) {
	// L-11: typed text that passes 50,000,000 characters.
	heavy(t, "holds 50,000,000 characters")
	e := start(t)
	text := strings.Repeat("t", 32767)
	row := `["` + text + `"]`
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1001,"count":1000}]}`)
	// 1,525 cells are 49,969,675 characters.
	for start := 0; start < 1525; start += 200 {
		n := min(200, 1525-start)
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, start+1, strings.TrimSuffix(strings.Repeat(row+",", n), ",")))
		e.b.importAll(t)
	}
	wantLimit(t, e, `{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[`+row+`]}]}`, "sheet_text")

	// A formula counts as the text its cell shows. The sheet is filled to
	// the limit exactly with two formulas among the text; their stored
	// forms are longer than their text and must not count.
	const sum, ref = "=SUM(Sheet1!$A$1:$J$20)", "=A9"
	rest := 50_000_000 - 1525*32767 - len(sum) - len(ref)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[["` + text[:rest] + `","` + sum + `"]]},
		{"op":"write","tab":"Sheet1","at":"C9","rows":[["` + ref + `"]]}]}`)
	e.in.mu.Lock()
	typed, _ := e.in.wb.typedText(nil)
	e.in.mu.Unlock()
	if typed != 50_000_000 {
		t.Fatalf("the sheet counts %d characters of typed text, want the limit", typed)
	}
	wantLimit(t, e, `{"ops":[{"op":"write","tab":"Sheet1","at":"D1","rows":[["x"]]}]}`, "sheet_text")
	// An edit that makes a formula's text longer without touching its cell
	// passes the limit too: =A9 becomes =A10, and Sheet1 becomes Sheet12.
	wantLimit(t, e, `{"ops":[{"op":"insert_rows","tab":"Sheet1","at":5,"count":1}]}`, "sheet_text")
	wantLimit(t, e, `{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"Sheet12"}]}`, "sheet_text")
	e.wantText("C9", ref)
	// A shorter tab name leaves room for the longer reference.
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"Sheet"}]}`)
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet","at":5,"count":1}]}`)
	e.wantText("C10", "=A10")
	e.wantText("C1", "=SUM(Sheet!$A$1:$J$21)")
}

// typedOf counts the typed text of the sheet cell by cell, from the text a
// formula bar would show for each.
func typedOf(e *env) (counted, want int64) {
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	wb := e.in.wb
	for _, t := range wb.tabs {
		for row, rd := range t.data {
			for _, s := range rd.cells {
				pos, _ := t.posOf(row, s.col)
				switch {
				case s.c.f != nil:
					want += int64(utf8.RuneCountInString(s.c.f.Text(pos)))
				case s.c.content.Kind == formula.ContentText:
					want += int64(utf8.RuneCountInString(s.c.content.Text))
				}
			}
		}
	}
	typed, _ := wb.typedText(nil)
	return typed, want
}

// TestTypedTextCountsFormulaText checks the counter behind the limit on
// typed text (spec rule 9): a formula counts as the text its cell shows,
// also after an insert, a delete, a sort or a tab rename changed that text
// without touching the cell.
func TestTypedTextCountsFormulaText(t *testing.T) {
	e := start(t)
	last := int64(0)
	step := func(when string, wantDelta int64, exact bool) {
		t.Helper()
		got, want := typedOf(e)
		if got != want {
			t.Fatalf("%s: the sheet counts %d characters of typed text, its cells hold %d", when, got, want)
		}
		if exact && got-last != wantDelta {
			t.Fatalf("%s: typed text changed by %d, want %d", when, got-last, wantDelta)
		}
		last = got
	}
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},
		{"op":"write","tab":"Data","at":"A1","rows":[[3],[1],[2]]},
		{"op":"write","tab":"Sheet1","at":"A1","rows":[[5,"text","=SUM("],[6,"=A1+Data!A1"],[7,"=SUM(A$1:A3)"]]},
		{"op":"write","tab":"Sheet1","at":"C9","rows":[["=A9"]]}]}`)
	// "text" 4, "=SUM(" 5, "=A1+Data!A1" 11, "=SUM(A$1:A3)" 12, "=A9" 3.
	step("after the first write", 35, true)
	alice := e.person("Alice")
	// Row 9 becomes row 10: "=A10" is one character longer, and the range
	// of the sum grows without getting longer.
	alice.do(e.insertRowsOp("Sheet1", 5, 1))
	e.wantText("C10", "=A10")
	step("after an insert above", 1, true)
	// A longer tab name that needs quotes: Data!A1 becomes 'Q1 costs'!A1.
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Data","name":"Q1 costs"}]}`)
	e.wantText("B2", "=A1+'Q1 costs'!A1")
	step("after a rename", 6, true)
	alice.do(e.sortOp("Sheet1", 1, 3, 1, true))
	step("after a sort", 0, false)
	alice.do(e.deleteRowsOp("Sheet1", 5, 1))
	e.wantText("C9", "=A9")
	step("after a delete", 0, false)
	alice.do(`{"op":"undo"}`)
	e.wantText("C10", "=A10")
	step("after the undo of the delete", 1, true)
	alice.do(`{"op":"undo"}`)
	step("after the undo of the sort", 0, false)
	alice.do(e.insertColsOp("Sheet1", 1, 30))
	e.wantText("AG10", "=AE10")
	step("after columns inserted", 0, false)
	// A refused action leaves the counter as it was.
	if _, err := e.agent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A20","rows":[["=A1"]]},{"op":"add_tab","name":"Q1 costs"}]}`); err == nil {
		t.Fatal("a second tab of the same name was accepted")
	}
	step("after a refused call", 0, true)
	e.restart()
	step("after a restart", 0, true)
	rev := e.b.importAll(t)
	e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:AZ30"]}]}`)
	step("after a clear", 0, false)
	if _, err := e.b.Rollback(context.Background(), rev, "Alice"); err != nil {
		t.Fatal(err)
	}
	step("after a rollback", 0, false)
	e.wantText("AG10", "=AE10")
}

func TestCalculatedTextAndLimitedMode(t *testing.T) {
	// L-12
	e := start(t)
	res := e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"D1","rows":[["=REPT(\"a\",32768)","=LEN(REPT(\"a\",40000))","=LEN(REPT(\"a\",32767))"]]}]}`)
	if res.FormulaErrors != 2 {
		t.Fatalf("result = %+v", res)
	}
	e.want("D1", "#VALUE!")
	e.want("E1", "#VALUE!")
	e.want("F1", "32767")

	// 1,526 results of 32,767 characters are 50,002,442 characters.
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1001,"count":1000}]}`)
	cells := strings.TrimSuffix(strings.Repeat(`["=REPT(\"a\",32767)"],`, 1526), ",")
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + cells + `]},
		{"op":"write","tab":"Sheet1","at":"B1","rows":[["=IF(ISERROR(A1),\"short\",REPT(\"b\",300))","=LEN(A1)"]]}]}`)
	limited := func(want bool) {
		t.Helper()
		e.in.mu.RLock()
		got := e.in.wb.limited
		e.in.mu.RUnlock()
		if got != want {
			t.Fatalf("limited mode = %v, want %v", got, want)
		}
	}
	limited(true)
	for _, addr := range []string{"A1", "A777", "A1526"} {
		e.want(addr, "#LIMIT!")
	}
	e.want("B1", "short")
	e.want("C1", "#LIMIT!")
	e.want("F1", "32767")
	out := e.read(ReadQuery{Tab: "Sheet1", Range: "A1:C1", Include: []string{"shown"}})
	if got := fmt.Sprint(out["shown"]); got != "[[#LIMIT! short #LIMIT!]]" {
		t.Fatalf("get_sheet shows %s", got)
	}
	if passes := e.in.lastPasses.Load(); passes != 2 {
		t.Fatalf("the recalculation took %d passes", passes)
	}
	// The same after a restart.
	e.restart()
	limited(true)
	e.want("A9", "#LIMIT!")
	// An edit that brings the ordinary total under the limit shows every
	// result again.
	alice := e.person("Alice")
	if ack := alice.set("A1", "1"); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	limited(false)
	e.want("C1", "1")
	if got := e.shown("B1"); len(got) != 300 {
		t.Fatalf("B1 shows %d characters", len(got))
	}
	if got := e.shown("A2"); len(got) != 32767 {
		t.Fatalf("A2 shows %d characters", len(got))
	}
}

func TestLimitedModeFromDate(t *testing.T) {
	// L-12: a sheet that passes the limit only on a new calculation date.
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1001,"count":1000}]}`)
	cells := strings.TrimSuffix(strings.Repeat(`["=IF(TODAY()>=DATE(2026,11,14),REPT(\"a\",32767),\"\")"],`, 1526), ",")
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + cells + `]},{"op":"write","tab":"Sheet1","at":"B1","rows":[["=LEN(A1)"]]}]}`)
	e.want("B1", "0")
	e.clock.Add(24 * time.Hour)
	out := e.read(ReadQuery{Tab: "Sheet1", Range: "A1:B1", Include: []string{"shown"}})
	if got := fmt.Sprint(out["shown"]); got != "[[#LIMIT! #LIMIT!]]" || out["date"] != "2026-11-14" {
		t.Fatalf("after the date change get_sheet shows %s on %v", got, out["date"])
	}
}

// TestLimitCallWithoutSlug measures calls with a slug member wherever it
// stands and however it is written. The byte bound leaves out exactly that
// member, so a call through MCP meets the bound of the same call through
// the management API, which names the sheet in its path.
func TestLimitCallWithoutSlug(t *testing.T) {
	// body is a call without a slug member of exactly size bytes.
	body := func(size int) string {
		head, tail := `{"ops":[`, `],"message":"m"}`
		return head + strings.Repeat(" ", size-len(head)-len(tail)) + tail
	}
	for _, place := range []struct {
		name string
		with func(call string) string
	}{
		{"none", func(c string) string { return c }},
		{"first", func(c string) string { return `{"slug":"budget",` + c[1:] }},
		{"first with white space", func(c string) string { return "{\"slug\" :\t\"budget\"\n," + c[1:] }},
		{"between, written with escapes", func(c string) string {
			at := strings.Index(c, `"message"`)
			return c[:at] + `"\u0073lug":"b\u0075dget",` + c[at:]
		}},
		{"last", func(c string) string { return c[:len(c)-1] + `, "slug" : "budget"}` }},
	} {
		at, err := DecodeUpdate([]byte(place.with(body(maxCallBytes))))
		if err != nil || place.name != "none" && at.Slug != "budget" {
			t.Errorf("%s: a call of the bound = %q, %v", place.name, at.Slug, err)
		}
		_, err = DecodeUpdate([]byte(place.with(body(maxCallBytes + 1))))
		if r := refusal(err); r.Limit != "call_bytes" || r.Max != maxCallBytes || r.Value != maxCallBytes+1 {
			t.Errorf("%s: a call one byte over the bound = %v %+v, want call_bytes of %d", place.name, err, r, maxCallBytes+1)
		}
	}
	// Bytes past the room for a slug member are not read, and their size
	// is not the size of a call.
	_, err := DecodeUpdate([]byte(body(maxCallBytes + maxSlugMemberBytes + 1)))
	if r := refusal(err); r.Limit != "call_bytes" || r.Value != 0 {
		t.Errorf("bytes past the room = %+v, want call_bytes without a value", r)
	}
	if got := MaxUpdateReadBytes(); got != maxCallBytes+maxSlugMemberBytes {
		t.Errorf("read bound = %d", got)
	}
}

func TestLimitCall(t *testing.T) {
	// L-13: an agent call of 8 MiB plus one byte, and of 50,001 cells.
	pad := strings.Repeat(" ", maxCallBytes)
	if _, err := DecodeUpdate([]byte(`{"ops":[]}` + pad)); refusal(err).Limit != "call_bytes" {
		t.Fatalf("8 MiB plus: %v", err)
	}
	if got := MaxUpdateBodyBytes(); got != 8<<20 {
		t.Fatalf("body limit = %d", got)
	}
	row := "[" + strings.TrimSuffix(strings.Repeat("1,", 100), ",") + "]"
	call := func(rows int, extra string) string {
		return `{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.TrimSuffix(strings.Repeat(row+",", rows), ",") + extra + `]}]}`
	}
	if _, err := DecodeUpdate([]byte(call(500, ""))); err != nil {
		t.Fatalf("50,000 cells: %v", err)
	}
	if _, err := DecodeUpdate([]byte(call(500, ",[1]"))); refusal(err).Limit != "call_cells" {
		t.Fatalf("50,001 cells: %v", err)
	}
	csv := strings.Repeat("1,2,3,4,5\\n", 10000) + "6"
	if _, err := DecodeUpdate([]byte(`{"ops":[{"op":"import_csv","tab":"Sheet1","at":"A1","csv":"` + csv + `"}]}`)); refusal(err).Limit != "call_cells" {
		t.Fatalf("CSV of 50,001 fields: %v", err)
	}
	for _, bad := range []string{
		`{"ops":[{"op":"explode","tab":"Sheet1"}]}`,
		`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]],"count":2}]}`,
		`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[{"formula":"=1","note":1}]]}]}`,
		`{"ops":[{"op":"write","tab":"Sheet1","at":"A0","rows":[[1]]}]}`,
		`{"ops":[{"op":"add_tab","name":"a/b"}]}`,
		`{"ops":[],"surprise":1}`,
		`{"op_id":"has space","ops":[]}`,
	} {
		if _, err := DecodeUpdate([]byte(bad)); refusal(err).Code != "invalid" {
			t.Errorf("%s: %v, want invalid", bad, err)
		}
	}
}

func TestLimitReadPage(t *testing.T) {
	// L-14: a read page ends at 10,000 cells and at 4 MiB.
	e := start(t)
	row := "[" + strings.TrimSuffix(strings.Repeat("1,", 30), ",") + "]"
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.TrimSuffix(strings.Repeat(row+",", 400), ",") + `]}]}`)
	out := e.read(ReadQuery{Tab: "Sheet1"})
	rows := out["rows"].([]any)
	next := out["next"].(map[string]any)
	if len(rows) != 333 || next["start"] != "A334" {
		t.Fatalf("the first page has %d rows and continues at %v", len(rows), next["start"])
	}
	rest := e.read(ReadQuery{Cursor: next["cursor"].(string)})
	if _, more := rest["next"]; more || len(rest["rows"].([]any)) != 67 || rest["start"] != "A334" {
		t.Fatalf("the second page has %d rows", len(rest["rows"].([]any)))
	}
	// A page of large text ends early, before 4 MiB.
	text := strings.Repeat("x", 32767)
	big := `["` + text + `","` + text + `"]`
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Big"},{"op":"write","tab":"Big","at":"A1","rows":[` + strings.TrimSuffix(strings.Repeat(big+",", 100), ",") + `]}]}`)
	data, err := e.in.Read(context.Background(), ReadQuery{Tab: "Big"})
	if err != nil || len(data) > maxPageBytes || len(data) < maxPageBytes-(200<<10) {
		t.Fatalf("a page of large text has %d bytes: %v", len(data), err)
	}
	// A fifth capture evicts the oldest, whose cursor is then refused.
	cursors := make([]string, 5)
	for i := range cursors {
		cursors[i] = e.read(ReadQuery{Tab: "Sheet1", Limit: 10})["next"].(map[string]any)["cursor"].(string)
	}
	_, err = e.in.Read(context.Background(), ReadQuery{Cursor: cursors[0]})
	e.wantRefusal(err, "conflict", "read_expired")
	e.read(ReadQuery{Cursor: cursors[4]})
	e.clock.Add(2 * time.Minute)
	_, err = e.in.Read(context.Background(), ReadQuery{Cursor: cursors[4]})
	e.wantRefusal(err, "conflict", "read_expired")
}

func TestLimitRecalculationTime(t *testing.T) {
	// L-15: an action over the time limit, lowered by a test option. The
	// limit is on the clock, so with it passed the sheet takes no action,
	// whatever its size, and takes one again when the limit is back.
	const limit = 50 * time.Millisecond
	e := start(t, func(o *Options) { o.RecalcLimit = limit })
	expireNextAction := func() {
		onWriter(e, func() {
			e.in.hook = func(step string) {
				if step != "begin" {
					return
				}
				e.in.hook = nil
				// The writer has started this action's clock.
				time.Sleep(2 * limit)
			}
		})
	}
	expireNextAction()
	wantLimit(t, e, `{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[["=SUM(D1:M1000)"]]}]}`, "recalculation_time")
	e.want("B1", "")
	alice := e.person("Alice")
	expireNextAction()
	if ack := alice.set("B1", "=SUM(D1:M1000)"); status(ack) != "refused:recalculation_time" {
		t.Fatal(status(ack))
	}
	expireNextAction()
	if ack := alice.set("B1", "plain"); status(ack) != "refused:recalculation_time" {
		t.Fatal(status(ack))
	}
	e.setRecalcLimit(defaultRecalcLimit)
	if ack := alice.set("B1", "plain"); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
}

func chain(t *testing.T, e *env) {
	t.Helper()
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A100000","rows":[[null]]},{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]]}]}`)
	for start := 2; start <= 100000; start += 50000 {
		var b strings.Builder
		for row := start; row < start+50000 && row <= 100000; row++ {
			fmt.Fprintf(&b, `["=SUM(A%d:A%d)"],`, row-1, row-1)
		}
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, start, strings.TrimSuffix(b.String(), ",")))
	}
}

func TestRangeChain(t *testing.T) {
	// L-17: A2 = SUM(A1:A1) to A100000 = SUM(A99999:A99999).
	heavy(t, "holds a chain of 99,999 formulas")
	e := start(t, unhurried)
	chain(t, e)
	e.want("A100000", "1")
	alice := e.person("Alice")
	// The criterion of one second is for the reference machine. A busy
	// machine stretches the time, so the bound here only finds an edit
	// that takes many times that; the count of units is what bounds the
	// work.
	op := e.setOp("A1", "7")
	began := time.Now()
	if ack := alice.do(op); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	took := time.Since(began)
	e.want("A100000", "7")
	e.want("A50000", "7")
	work := e.in.lastWork.Load()
	t.Logf("one edit of A1 updated 99,999 formulas in %v with %d units of work", took, work)
	if took > 10*time.Second {
		t.Errorf("the edit took %v", took)
	}
	// The work is linear in the chain: a few units per formula.
	if work < 99999 || work > 20*99999 {
		t.Errorf("the budget counted %d units for 99,999 formulas", work)
	}
	// With the limit lowered the same edit is refused while it
	// recalculates: the limit has not passed when the edit starts.
	e.in.opts.RecalcLimit = time.Millisecond
	if ack := alice.set("A1", "8"); status(ack) != "refused:recalculation_time" {
		t.Fatal(status(ack))
	}
	e.want("A100000", "7")
}

func TestLayeredLimitedMode(t *testing.T) {
	// L-18: eight layers of 1,600 formulas, each reading the layer before
	// with COUNTIF and giving 32,767 characters from a set date on.
	// The case sits at the read limit of the sheet, which no smaller
	// sheet reaches. TestCalculatedTextAndLimitedMode takes the limited
	// mode of one layer in the default run.
	full(t, "12,800 formulas with 17,920,000 static reads")
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1001,"count":600}]}`)
	for layer := 0; layer < 8; layer++ {
		col := columnLetters(layer)
		text := `=IF(TODAY()>=DATE(2026,11,14),REPT(\"a\",32767),\"\")`
		if layer > 0 {
			prev := columnLetters(layer - 1)
			text += fmt.Sprintf(`&IF(COUNTIF(%s1:%s1600,\"x\")>0,\"\",\"\")`, prev, prev)
		}
		cells := strings.TrimSuffix(strings.Repeat(`["`+text+`"],`, 1600), ",")
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":"Sheet1","at":"%s1","rows":[%s]}]}`, col, cells))
	}
	e.in.mu.RLock()
	reads := e.in.wb.count.reads
	e.in.mu.RUnlock()
	if reads != 17_920_000 {
		t.Fatalf("static reads = %d", reads)
	}
	ordinary := e.in.lastWork.Load()
	check := func(when string) {
		t.Helper()
		work, passes := e.in.lastWork.Load(), e.in.lastPasses.Load()
		t.Logf("%s: %d passes, %d units of work (one ordinary pass: about %d)", when, passes, work, ordinary)
		if passes > 2 {
			t.Errorf("%s: %d passes", when, passes)
		}
	}
	e.clock.Add(24 * time.Hour)
	began := time.Now()
	out := e.read(ReadQuery{Tab: "Sheet1", Range: "A1:H1", Include: []string{"shown"}})
	t.Logf("date change with limited mode took %v", time.Since(began))
	if got := fmt.Sprint(out["shown"]); got != "[[#LIMIT! #LIMIT! #LIMIT! #LIMIT! #LIMIT! #LIMIT! #LIMIT! #LIMIT!]]" {
		t.Fatalf("after the date change: %s", got)
	}
	check("date change")
	e.b.importAll(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"J1","rows":[["note"]]}]}`)
	rev := e.b.importAll(t)
	e.restart()
	e.want("H1600", "#LIMIT!")
	check("restart")
	old := e.readRevision(rev, ReadQuery{Tab: "Sheet1", Range: "A1:H1", Include: []string{"shown"}})
	if got := fmt.Sprint(old["shown"]); got != "[[#LIMIT! #LIMIT! #LIMIT! #LIMIT! #LIMIT! #LIMIT! #LIMIT! #LIMIT!]]" {
		t.Fatalf("the revision shows %s", got)
	}
	check("revision")
}

// TestRefusalCategories sends operations that are refused and reads the
// category of each: a count over a stored limit is sheet_capacity with the
// limit's name, and a malformed operation is invalid with the field at
// fault as its reason (section 18.3).
func TestRefusalCategories(t *testing.T) {
	e := start(t)
	for _, c := range []struct{ op, code, reason string }{
		{`{"op":"insert_rows","tab":"Sheet1","at":1,"count":0}`, "invalid", "count"},
		{`{"op":"insert_rows","tab":"Sheet1","at":0,"count":1}`, "invalid", "at"},
		{`{"op":"insert_rows","tab":"Sheet1","at":1002,"count":1}`, "invalid", "at"},
		{`{"op":"insert_rows","tab":"Sheet1","at":1,"count":100000}`, "sheet_capacity", "rows"},
		{`{"op":"insert_cols","tab":"Sheet1","at":"A","count":0}`, "invalid", "count"},
		{`{"op":"insert_cols","tab":"Sheet1","at":"1","count":1}`, "invalid", "at"},
		{`{"op":"insert_cols","tab":"Sheet1","at":"AB","count":1}`, "invalid", "at"},
		{`{"op":"insert_cols","tab":"Sheet1","at":"A","count":700}`, "sheet_capacity", "columns"},
		{`{"op":"insert_rows","tab":"Sheet1","at":1,"count":100001}`, "sheet_capacity", "rows"},
		{`{"op":"insert_cols","tab":"Sheet1","at":"A","count":703}`, "sheet_capacity", "columns"},
		{`{"op":"delete_rows","tab":"Sheet1","at":1,"count":100001}`, "invalid", "count"},
		{`{"op":"delete_rows","tab":"Sheet1","at":1001,"count":1}`, "invalid", "at"},
		{`{"op":"delete_rows","tab":"Sheet1","at":999,"count":3}`, "invalid", "count"},
		{`{"op":"delete_rows","tab":"Sheet1","at":1,"count":-1}`, "invalid", "count"},
		{`{"op":"delete_cols","tab":"Sheet1","at":"Y","count":3}`, "invalid", "count"},
		{`{"op":"delete_rows","tab":"Sheet1","at":1,"count":1000}`, "invalid", "empty_tab"},
		{`{"op":"tab_settings","tab":"Sheet1","frozen_rows":21}`, "invalid", "frozen_rows"},
		{`{"op":"tab_settings","tab":"Sheet1","frozen_rows":1,"frozen_cols":11}`, "invalid", "frozen_cols"},
		{`{"op":"tab_settings","tab":"Sheet1","frozen_cols":-1}`, "invalid", "frozen_cols"},
		{`{"op":"widths","tab":"Sheet1","cols":"B:B","width":39}`, "invalid", "width"},
		{`{"op":"widths","tab":"Sheet1","cols":"AB:AC","width":100}`, "invalid", "cols"},
		{`{"op":"sort","tab":"Sheet1","rows":"2:9","by":"AB"}`, "invalid", "by"},
		{`{"op":"add_tab","name":""}`, "invalid", "tab_name"},
	} {
		seq := e.seq()
		_, err := e.agent(`{"ops":[` + c.op + `]}`)
		werr := refusal(err)
		if err == nil || werr.Code != c.code || (werr.Reason != c.reason && werr.Limit != c.reason) || e.seq() != seq {
			t.Errorf("%s: %v, want %s %s", c.op, err, c.code, c.reason)
		}
	}
	// A copy from more cells than a sheet holds passes the same limit as a
	// copy to that many.
	e.mustAgent(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":99000}]}`)
	alice := e.person("Alice")
	tab := e.tabID("Sheet1")
	whole := `"rows":"all","cols":"all"`
	one := fmt.Sprintf(`"rows":%s,"cols":%s`, e.rowsOf("Sheet1", 1, 1), e.colsOf("Sheet1", 1, 1))
	for _, c := range []struct{ name, from, to, want string }{
		{"a source over the limit", whole, one, "refused:cells"},
		{"a target over the limit", one, whole, "refused:cells"},
		{"a source without rows", `"rows":[],"cols":"all"`, one, "refused:invalid"},
	} {
		op := jsonf(`{"op":"copy","from":{"tab":%d,%s},"to":{"tab":%d,%s},"mode":"all","cut":false}`, tab, c.from, tab, c.to)
		if ack := alice.do(op); status(ack) != c.want {
			t.Errorf("%s: %s, want %s", c.name, status(ack), c.want)
		}
	}
}

// TestAgentCallFields leaves out each field an operation of a call needs
// and sets each field it may carry to null. Every such call is refused as
// invalid with the field as its reason, and none is read as if the field
// held zero, an empty text or no rows. Null is a value only inside the
// rows of a write, where it is a blank cell.
func TestAgentCallFields(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Sheet1","at":"A1","rows":[[3],[1],[2]]}]}`)
	whole := map[string]string{
		"write":        `{"op":"write","tab":"Sheet1","at":"D1","rows":[[1,null]]}`,
		"clear":        `{"op":"clear","tab":"Sheet1","ranges":["D1"],"formats":true}`,
		"format":       `{"op":"format","tab":"Sheet1","ranges":["D1"],"format":{"bold":true}}`,
		"widths":       `{"op":"widths","tab":"Sheet1","cols":"B:B","width":160}`,
		"insert_rows":  `{"op":"insert_rows","tab":"Data","at":1,"count":1}`,
		"delete_rows":  `{"op":"delete_rows","tab":"Data","at":1,"count":1}`,
		"insert_cols":  `{"op":"insert_cols","tab":"Data","at":"A","count":1}`,
		"delete_cols":  `{"op":"delete_cols","tab":"Data","at":"A","count":1}`,
		"sort":         `{"op":"sort","tab":"Sheet1","rows":"1:3","by":"A","desc":true}`,
		"add_tab":      `{"op":"add_tab","name":"More","at":1}`,
		"rename_tab":   `{"op":"rename_tab","tab":"Data","name":"Info"}`,
		"move_tab":     `{"op":"move_tab","tab":"Data","to":1}`,
		"delete_tab":   `{"op":"delete_tab","tab":"Data"}`,
		"tab_settings": `{"op":"tab_settings","tab":"Data","frozen_rows":1,"frozen_cols":1}`,
		"import_csv":   `{"op":"import_csv","tab":"Data","at":"A1","csv":"a,b"}`,
	}
	if len(whole) != len(agentOps) {
		t.Fatalf("the test knows %d of the %d operations", len(whole), len(agentOps))
	}
	refused := func(what, call, field string) {
		t.Helper()
		seq := e.seq()
		_, err := DecodeUpdate([]byte(call))
		if werr := refusal(err); err == nil || werr.Code != "invalid" || werr.Reason != field {
			t.Errorf("%s: %v, want invalid %s\n\t%s", what, err, field, call)
		}
		if _, err := e.agent(call); err == nil || e.seq() != seq {
			t.Errorf("%s was carried out\n\t%s", what, call)
		}
	}
	for name, op := range whole {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(op), &fields); err != nil {
			t.Fatal(err)
		}
		kind := agentOps[name]
		for field := range fields {
			if field == "op" {
				continue
			}
			without, null := map[string]json.RawMessage{}, map[string]json.RawMessage{}
			for k, v := range fields {
				null[k] = v
				if k != field {
					without[k] = v
				}
			}
			null[field] = json.RawMessage("null")
			a, _ := json.Marshal(without)
			b, _ := json.Marshal(null)
			if slices.Contains(strings.Fields(kind[0]), field) {
				refused(name+" without "+field, `{"ops":[`+string(a)+`]}`, field)
			} else if _, err := DecodeUpdate([]byte(`{"ops":[` + string(a) + `]}`)); err != nil {
				t.Errorf("%s without its optional %s: %v", name, field, err)
			}
			refused(name+" with a null "+field, `{"ops":[`+string(b)+`]}`, field)
		}
		other := `"csv":"a",`
		if name == "import_csv" {
			other = `"width":160,`
		}
		refused(name+" with a field of another operation", `{"ops":[`+strings.Replace(op, `{`, `{`+other, 1)+`]}`, "field")
		if _, err := DecodeUpdate([]byte(`{"ops":[` + op + `]}`)); err != nil {
			t.Errorf("%s as it should be: %v", name, err)
		}
	}
	refused("an operation with a null op", `{"ops":[{"op":null,"tab":"Sheet1"}]}`, "op")
	refused("an operation without op", `{"ops":[{"tab":"Sheet1"}]}`, "op")
	refused("tab_settings without a count", `{"ops":[{"op":"tab_settings","tab":"Sheet1"}]}`, "settings")
	refused("a call without ops", `{"message":"nothing"}`, "ops")
	for _, field := range []string{"slug", "state", "op_id", "message", "guard", "ops"} {
		call := map[string]json.RawMessage{"ops": json.RawMessage(`[{"op":"write","tab":"Sheet1","at":"E1","rows":[[1]]}]`)}
		call[field] = json.RawMessage("null")
		raw, _ := json.Marshal(call)
		refused("a call with a null "+field, string(raw), field)
	}
	refused("a guard with null ranges", `{"guard":{"ranges":null},"ops":[{"op":"write","tab":"Sheet1","at":"E1","rows":[[1]]}]}`, "guard")
	refused("a guard with a null flag", `{"guard":{"unchanged":null},"ops":[{"op":"write","tab":"Sheet1","at":"E1","rows":[[1]]}]}`, "guard")
}

// TestFormulaTextLookCountsAndStops writes one formula on a sheet of many
// long formulas. The limit of typed text then derives the text of every
// formula again, which counts on the budget of the action and meets its
// deadline as the rest of the action's work does.
func TestFormulaTextLookCountsAndStops(t *testing.T) {
	e := start(t, unhurried)
	alice := e.person("Alice")
	const cols = 700
	// 50,400 formulas, and 28,000 under the race detector: enough work for
	// stopsAtDeadline to tell a stop from the end.
	rows := raced(72, 40)
	e.mustAgent(jsonf(`{"ops":[{"op":"insert_cols","tab":"Sheet1","at":"A","count":%d}]}`, cols-newTabCols))
	// Fifty ranges below the last row of the tab: they read no cell and get
	// no entry in the index, and each is a reference that the length of
	// the formula's text goes through.
	sum := "=SUM(" + strings.TrimSuffix(strings.Repeat("$B$5000:$B$5001,", 50), ",") + ")"
	alice.set("A1", sum)
	ack := alice.do(jsonf(`{"op":"fill","tab":%d,"source":{"rows":%s,"cols":%s},"target":{"rows":%s,"cols":"all"}}`, e.tabID("Sheet1"),
		e.rowsOf("Sheet1", 1, 1), e.colsOf("Sheet1", 1, 1), e.rowsOf("Sheet1", 1, rows)))
	if status(ack) != "applied" {
		t.Fatalf("the fill: %s", status(ack))
	}
	formulas := rows * cols
	put := func(at, text string) string {
		return agentStatus(e, jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":%q,"rows":[[%q]]}]}`, at, text))
	}
	write := func(at string) string { return put(at, "=1") }
	// The first write after the fill waits for the fill's storage.
	if got := write("A899"); got != "applied" {
		t.Fatalf("the write of one formula: %s", got)
	}
	deadline := e.workDeadline()
	got, whole := deadline.run(never, func() string { return put("A900", "=2") })
	if got != "applied" {
		t.Fatalf("the write of one formula: %s", got)
	}
	// Each formula is looked through: an eighth of a unit and one for each
	// 256 bytes of its stored text.
	least := int64(formulas) * int64(8+len(sum)/4) / 64
	if work := e.in.lastWork.Load(); work < least {
		t.Errorf("the write on a sheet of %d formulas counted %d units, for at least %d", formulas, work, least)
	}
	if got := put("A900", "=1"); got != "applied" {
		t.Fatalf("the write of one formula: %s", got)
	}
	deadline.stopsAtDeadline(t, whole, func() string { return write("B900") })
	e.want("B900", "")
	e.want("A900", "1")
}
