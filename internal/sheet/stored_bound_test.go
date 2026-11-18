package sheet

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// storedAt returns the stored form of the formula in the cell of the
// first tab.
func storedAt(e *env, addr string) string {
	e.t.Helper()
	_, row, col := e.ids(addr)
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	return e.in.wb.tabs[0].cell(row, col).f.Stored()
}

// A copy one column to the right takes each reference to ZZZ1 past what a
// formula can name, and each becomes #REF! for good, with no tab name and
// no position: B1 holds a sum of #REF! whose stored form is its text. A
// rename of the tab changes nothing in it, through a revision view, a
// rollback, a restart and a restore from a backup, and neither a copy
// back nor a rename back brings the references back.
func TestCopyPastWhatAFormulaCanNameEndsTheReferences(t *testing.T) {
	for _, c := range []struct {
		name string
		refs int
	}{
		{strings.Repeat("a", maxTabName), 1000},
		// The most references such a formula holds, renamed to the name
		// of the most bytes.
		{strings.Repeat("😀", maxTabName), formula.MaxFormulaChars / len("+S!ZZZ1")},
	} {
		t.Run(c.name[:4], func(t *testing.T) {
			ctx := context.Background()
			e := start(t)
			typed := "=S!ZZZ1" + strings.Repeat("+S!ZZZ1", c.refs-1)
			shown := "=#REF!" + strings.Repeat("+#REF!", c.refs-1)
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"S"},{"op":"write","tab":"Sheet1","at":"A1","rows":[["` + typed + `"]]}]}`)
			alice := e.person("Alice")
			tab := e.tabID("Sheet1")
			copyOp := func(from, to int) string {
				return jsonf(`{"op":"copy","from":{"tab":%d,"rows":%s,"cols":%s},"to":{"tab":%d,"rows":%s,"cols":%s},"mode":"all","cut":false}`,
					tab, e.rowsOf("Sheet1", 1, 1), e.colsOf("Sheet1", from, 1), tab, e.rowsOf("Sheet1", 1, 1), e.colsOf("Sheet1", to, 1))
			}
			if ack := alice.do(copyOp(1, 2)); status(ack) != "applied" {
				t.Fatalf("the copy: %s", status(ack))
			}
			if ack := alice.do(e.clearOp("Sheet1", 1, 1, 1, 1)); status(ack) != "applied" {
				t.Fatalf("the clear: %s", status(ack))
			}
			check := func(when string) {
				t.Helper()
				if got := storedAt(e, "B1"); got != shown {
					t.Fatalf("%s: the stored form has %d bytes, want %d", when, len(got), len(shown))
				}
				e.wantText("B1", shown)
				e.want("B1", "#REF!")
			}
			check("after the copy")

			seq := e.seq()
			e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"S","name":"` + c.name + `"}]}`)
			check("after the rename")
			_, row, col := e.ids("B1")
			e.in.mu.RLock()
			textSeq := e.in.wb.tabs[0].cell(row, col).textSeq
			e.in.mu.RUnlock()
			if textSeq > seq {
				t.Errorf("the rename rewrote B1 at %d, after %d", textSeq, seq)
			}

			n := closeRevision(e)
			e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["B1"]}]}`)
			files, date, release, err := e.b.RevisionFiles(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if err := e.viewRevision(n, files, date); err != nil {
				t.Fatalf("the view: %v", err)
			}
			if err := e.in.Rollback(ctx, files, "Alice", "renamed"); err != nil {
				t.Fatalf("the rollback: %v", err)
			}
			check("after the rollback")
			e.restart()
			check("after a restart")
			e.in.Stop()
			e.dir = restored(t, e.dir)
			e.open()
			check("after a restore from a backup")

			alice = e.person("Alice")
			if ack := alice.do(copyOp(2, 1)); status(ack) != "applied" {
				t.Fatalf("the copy back: %s", status(ack))
			}
			e.wantText("A1", shown)
			e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"` + c.name + `","name":"S"}]}`)
			check("after the rename back")
			e.wantText("A1", shown)
		})
	}
}

// An undo entry keeps a formula that renames make longer than a formula
// may be, past formula.MaxStoredBytes of stored form:
// it keeps its references, so its undo is refused while the name is long
// and applies once a rename makes it short again, in memory and in a
// scratch file. Without a try in between, the undo after the rename back
// applies as well.
func TestUndoAfterARenamePastTheStoredBound(t *testing.T) {
	typed := "=D!A1" + strings.Repeat("+D!A1", 799)
	long := strings.Repeat("d", maxTabName)
	f, err := formula.Parse(typed, formula.Pos{Row: 1, Col: 1}, formula.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if g, _, _ := f.Apply("Sheet1", formula.Pos{Row: 1, Col: 1}, formula.Transform{Kind: formula.RenameTab, Tab: "D", Name: long}); len(g.Stored()) <= formula.MaxStoredBytes {
		t.Fatalf("the renamed formula has %d stored bytes", len(g.Stored()))
	}
	rename := func(e *env, from, to string) {
		e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"` + from + `","name":"` + to + `"}]}`)
	}
	for _, try := range []bool{true, false} {
		name := "with a try while long"
		if !try {
			name = "without a try"
		}
		t.Run(name, func(t *testing.T) {
			bothWays(t, func(t *testing.T, e *env) {
				e.mustAgent(`{"ops":[{"op":"add_tab","name":"D"},{"op":"write","tab":"D","at":"A1","rows":[[1]]},
					{"op":"write","tab":"Sheet1","at":"B2","rows":[["` + typed + `"]]}]}`)
				alice := e.person("Alice")
				alice.set("B2", "short")
				rename(e, "D", long)
				if try {
					seq := e.seq()
					if ack := alice.do(`{"op":"undo"}`); status(ack) != "refused:formula_length" || e.seq() != seq {
						t.Fatalf("undo while the name is long = %s", status(ack))
					}
				}
				rename(e, long, "D")
				wantUndo(t, "undo after the rename back", undo(t, alice), undoAck{Applied: 1})
				e.wantText("B2", typed)
				e.want("B2", "800")
				e.restart()
				e.wantText("B2", typed)
			})
		})
	}
}

// The loaders of sheet.db and of revision files read every stored form a
// cell can hold, up to a text of 8,192 code points at its cell, and refuse
// what no cell holds without building it: a form longer than
// formula.MaxStoredBytes, one of that length that holds more tokens than a
// formula can, and one with references past what a formula can name at
// its cell, which a copy turns into #REF!. A rename could make the stored
// form of the last one many times longer than its text.
func TestStoredBoundAtTheLoaders(t *testing.T) {
	ctx := context.Background()
	at := `="` + strings.Repeat("😀", formula.MaxFormulaChars-3) + `"`
	refused := map[string]string{
		"a form past the bound": `="` + strings.Repeat("😀", formula.MaxStoredBytes/4) + `"`,
		"a sum of ones":         "=1" + strings.Repeat("+1", (formula.MaxStoredBytes-2)/2),
		"references past what a formula names": "={'" + strings.Repeat("x", maxTabName) + "'!R[-1]C[0]}" +
			strings.Repeat("+{'"+strings.Repeat("x", maxTabName)+"'!R[-1]C[0]}", 99),
	}
	cell := func(form string) string {
		return fmt.Sprintf(`UPDATE cells SET kind=%d,num=NULL,str='%s' WHERE tab=1 AND row=1 AND col=1`,
			formula.ContentFormula, strings.ReplaceAll(form, "'", "''"))
	}
	t.Run("sheet.db", func(t *testing.T) {
		e := storedAxisSheet(t, cell(at))
		e.open()
		e.wantText("A1", at)
		e.in.Stop()
		for name, form := range refused {
			e := storedAxisSheet(t, cell(form))
			in, _, err := Open(e.dir, e.b, e.opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := in.await(ctx, 0); err == nil || refusal(err).Reason != "load" {
				t.Errorf("the load of %s answered %v", name, err)
			}
			in.Stop()
		}
	})
	t.Run("revision", func(t *testing.T) {
		e := start(t)
		e.person("Alice").set("A1", "before")
		n := closeRevision(e)
		files, date, release, err := e.b.RevisionFiles(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		rows := rowFileOf(t, files)
		view := 1000
		for name, form := range refused {
			view++
			bad := e.withFile(files, rows, rowFile(`{"f":`+strconv.Quote(form)+`}`))
			if err := e.viewRevision(view, bad, date); refusal(err).Reason != "revision" {
				t.Errorf("the view of %s answered %v", name, err)
			}
		}
		atFiles := e.withFile(files, rows, rowFile(`{"f":`+strconv.Quote(at)+`}`))
		if err := e.viewRevision(view+1, atFiles, date); err != nil {
			t.Fatalf("the view of a form at the bound: %v", err)
		}
		if err := e.in.Rollback(ctx, atFiles, "Alice", "bound"); err != nil {
			t.Fatalf("the rollback to a form at the bound: %v", err)
		}
		e.restart()
		e.wantText("A1", at)
	})
}

// writeDistinctFarSums writes n distinct formulas of almost 8,192
// characters into column Z of rows that end at row 1000, with 1 in A1:
// sums of A1 whose stored forms are more than five times their text, the
// longest stored forms a cell holds for its text.
func writeDistinctFarSums(e *env, n int) {
	e.t.Helper()
	rows := make([]string, n)
	for i := range rows {
		rows[i] = `[` + strconv.Quote("=A1"+strings.Repeat("+A1", 2727)+"+"+strconv.Itoa(i)) + `]`
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1]]},` +
		fmt.Sprintf(`{"op":"write","tab":"Sheet1","at":"Z%d","rows":[%s]}]}`, 1001-n, strings.Join(rows, ",")))
}

// A load reads the longest stored forms a cell holds one cell at a time
// between its check points: with every slot of the gate taken by loads of
// such forms held in their cells, small sheets load, and a stop ends the
// held loads there. Between two check points a load allocates no more
// than parsing the cells of a step takes, as the parse of each form is
// bounded by the text it has at its cell. It runs with a gate of one slot
// and with several.
func TestLongStoredFormulasTakeTurns(t *testing.T) {
	slow(t, "loads sheets of long formulas while others wait")
	const n = 40
	t.Run("steps", func(t *testing.T) {
		e := start(t, unhurried)
		writeDistinctFarSums(e, n)
		form := storedAt(e, "Z1000")
		before := heapAllocs()
		if _, err := formula.ParseStored(form); err != nil {
			t.Fatal(err)
		}
		one := heapAllocs() - before
		perStep := loadCheckCells/(1+len(form)/64) + 1
		var mu sync.Mutex
		var points []uint64
		see := func(in *Instance, phase string) {
			if phase == "cells" {
				mu.Lock()
				points = append(points, heapAllocs())
				mu.Unlock()
			}
		}
		if !loadPoints.CompareAndSwap(nil, &see) {
			t.Fatal("another test watches loads")
		}
		defer loadPoints.Store(nil)
		e.restart()
		loadPoints.Store(nil)
		e.want("Z1000", "2767")
		mu.Lock()
		defer mu.Unlock()
		if len(points) < n/perStep-1 {
			t.Fatalf("the load of %d forms of %d bytes reached %d check points in its cells", n, len(form), len(points))
		}
		// Two parses of each form of a step, as the load measures it at its
		// cell, and room for what the rows and the cells take.
		most := uint64(2*perStep)*one + 4<<20
		for i := 1; i < len(points); i++ {
			if got := points[i] - points[i-1]; measures && got > most {
				t.Errorf("the load allocated %d bytes between two check points, want at most %d", got, most)
			}
		}
	})
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			long := make([]*env, slots)
			for i := range long {
				long[i] = start(t, unhurried, func(o *Options) { o.Slug = heavySlug })
				writeDistinctFarSums(long[i], n)
			}
			w := watchLoads(t)
			w.holdAt("cells")
			for _, e := range long {
				e.in.Stop()
				in, _, err := Open(e.dir, e.b, e.opts)
				if err != nil {
					t.Fatal(err)
				}
				e.in, e.b.in = in, in
			}
			handOverAndStop(t, w, slots, "cells", []string{"cells", "files"}, long)
			gateSettles(t, "after the loads", slots, 0)
		})
	}
}

// A stored form no cell holds is refused before a tree is built for it.
// A sum of ones at the bound, which has more tokens than a formula can
// hold, fits the row of cells that the open admits; loads of such forms,
// as many as the gate has slots, are refused by the loader and allocate
// little more than the sheet holds, and a small sheet loads beside them.
// One of 1,875,968 bytes, whose tree would take more than 128 MiB, makes
// its row longer than the writer stores. While loads held at the gate take
// every slot, the open returns at once, and the load's preflight refuses
// the row from its length, without reading the form, once a held load
// hands it a slot. It runs with a gate of one slot and with several.
func TestImpossibleStoredFormulasAreRefusedUnbuilt(t *testing.T) {
	ctx := context.Background()
	cell := func(form string) string {
		return fmt.Sprintf(`UPDATE cells SET kind=%d,num=NULL,str='%s' WHERE tab=1 AND row=1 AND col=1`, formula.ContentFormula, form)
	}
	bounded := "=1" + strings.Repeat("+1", (formula.MaxStoredBytes-2)/2)
	const long = 1_875_968
	oversized := "=1" + strings.Repeat("+1", (long-2)/2)
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d bytes, %d slots", formula.MaxStoredBytes, slots), func(t *testing.T) {
			gateSlots(t, slots)
			sheets := make([]*env, slots)
			for i := range sheets {
				sheets[i] = storedAxisSheet(t, cell(bounded))
			}
			start := heapAllocs()
			ins := make([]*Instance, slots)
			for i, e := range sheets {
				in, _, err := Open(e.dir, e.b, e.opts)
				if err != nil {
					t.Fatal(err)
				}
				ins[i] = in
				defer in.Stop()
			}
			small := storedAxisSheet(t)
			small.open()
			small.want("A1", "1")
			small.in.Stop()
			for i, in := range ins {
				if err := in.await(ctx, 0); err == nil || refusal(err).Reason != "load" {
					t.Fatalf("the load %d answered %v", i, err)
				}
			}
			// Each load reads its form from sheet.db a few times over.
			if got, most := heapAllocs()-start, uint64(slots)*(8*uint64(formula.MaxStoredBytes)+8<<20); measures && got > most {
				t.Errorf("the loads allocated %d bytes, want at most %d", got, most)
			}
		})
		t.Run(fmt.Sprintf("%d bytes, %d slots", long, slots), func(t *testing.T) {
			gateSlots(t, slots)
			held := make([]*env, slots)
			for i := range held {
				held[i] = recordsSheet(t)
			}
			damaged := storedAxisSheet(t, cell(oversized))
			w := watchLoads(t)
			w.holdAt("records")
			for _, e := range held {
				in, _, err := Open(e.dir, e.b, e.opts)
				if err != nil {
					t.Fatal(err)
				}
				defer in.Stop()
			}
			w.heldAt(t, slots, "records")
			var err error
			got := allocatedBy(func() {
				in, _, openErr := Open(damaged.dir, damaged.b, damaged.opts)
				if openErr != nil {
					t.Fatalf("the open waited for its check: %v", openErr)
				}
				defer in.Stop()
				err = in.await(ctx, 0)
				if err != nil && refusal(err).cause != nil {
					err = refusal(err).cause
				}
			})
			if err == nil || !strings.Contains(err.Error(), "holds a row of") {
				t.Fatalf("the load answered %v", err)
			}
			t.Logf("refused after %d KiB: %v", got>>10, err)
			if measures && got > long/16 {
				t.Errorf("the refused load allocated %d KiB for a form of %d KiB", got>>10, long>>10)
			}
		})
	}
}

// A sheet of many distinct formulas whose references a copy took past
// what a formula can name, each to another tab later renamed to a name of
// 100 code points of four bytes, keeps stored forms as long as their text.
// A whole capture of the tab is charged in proportion to that text, and a
// fresh Private and Public snapshot, a CSV download and a History read of
// the tab each succeed with the reader alone. Had the references kept
// their tab names, at full size those alone would take 540,000,000 bytes,
// past the capture budget.
func TestEndedReferencesKeepCapturesSmall(t *testing.T) {
	slow(t, "writes thousands of long formulas")
	tabs, per := sized(30, 3), sized(50, 4)
	const refs = 900
	e := start(t, unhurried)
	var ops []string
	for j := 1; j <= tabs; j++ {
		ops = append(ops, jsonf(`{"op":"add_tab","name":"_%d"}`, j))
	}
	e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	n := tabs * per
	e.mustAgent(jsonf(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":%d}]}`, max(n-newTabRows, 1)))
	for j := 1; j <= tabs; j++ {
		ref := fmt.Sprintf("_%d!ZZZ1", j)
		rows := make([]string, per)
		for i := range rows {
			rows[i] = `[` + strconv.Quote("="+ref+strings.Repeat("+"+ref, refs-1)+"+"+strconv.Itoa(i)) + `]`
		}
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, (j-1)*per+1, strings.Join(rows, ",")))
	}
	alice := e.person("Alice")
	tab := e.tabID("Sheet1")
	for j := 0; j < tabs; j++ {
		rows := e.rowsOf("Sheet1", j*per+1, per)
		copyOp := jsonf(`{"op":"copy","from":{"tab":%d,"rows":%s,"cols":%s},"to":{"tab":%d,"rows":%s,"cols":%s},"mode":"all","cut":false}`,
			tab, rows, e.colsOf("Sheet1", 1, 1), tab, rows, e.colsOf("Sheet1", 2, 1))
		if ack := alice.do(copyOp); status(ack) != "applied" {
			t.Fatalf("the copy of the formulas of _%d: %s", j+1, status(ack))
		}
		if ack := alice.do(e.clearOp("Sheet1", j*per+1, per, 1, 1)); status(ack) != "applied" {
			t.Fatalf("the clear of the formulas of _%d: %s", j+1, status(ack))
		}
	}
	for j := 1; j <= tabs; j++ {
		name := strings.Repeat(string(rune(0x1F600+j)), maxTabName)
		e.mustAgent(jsonf(`{"ops":[{"op":"rename_tab","tab":"_%d","name":"%s"}]}`, j, name))
	}
	shown := 0
	for i := 1; i <= n; i++ {
		addr := fmt.Sprintf("B%d", i)
		want := "=#REF!" + strings.Repeat("+#REF!", refs-1) + "+" + strconv.Itoa((i-1)%per)
		if got := storedAt(e, addr); got != want {
			t.Fatalf("%s has a stored form of %d bytes, want %d", addr, len(got), len(want))
		}
		shown += len(want)
	}

	base := captureBytes.Load()
	e.read(ReadQuery{Tab: "Sheet1", Range: fmt.Sprintf("B1:B%d", n), Limit: 1})
	charged := captureBytes.Load() - base
	e.in.captures.clear()
	if most := int64(2*shown + 1<<20); charged > most {
		t.Fatalf("the capture of %d formulas of %d bytes of text is charged %d bytes, want at most %d", n, shown, charged, most)
	}

	s := serve(e)
	for _, route := range []string{"private", "public"} {
		c := s.dial(route, helloV1)
		c.next("welcome")
		if _, cells := c.watch(1, tab); len(cells) != n {
			t.Fatalf("the %s snapshot has %d cells, want %d", route, len(cells), n)
		}
		c.ws.Close(1000, "")
	}
	rev := e.b.importAll(t)
	for _, path := range []string{
		fmt.Sprintf("/private/_sheet/csv?tab=%d", tab),
		fmt.Sprintf("/private/_sheet/api/revisions/%d/tabs/%d", rev, tab),
	} {
		for {
			res, err := http.Get(s.server.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode == http.StatusServiceUnavailable && strings.Contains(string(body), "calculating") {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "#REF!") {
				t.Fatalf("%s answered %d: %.200s", path, res.StatusCode, body)
			}
			break
		}
	}
}

// A row an undo brings back after a sort returns behind the row that was
// above it, which can be another place than the one its formulas were
// kept for. A reference that place takes past what a formula can name
// ends there as a copy would end it, so the formula keeps no tab name it
// does not show: a rename leaves it as it is and the sheet opens again. In
// memory and in a scratch file.
func TestRowReturnedAfterASortEndsItsReferences(t *testing.T) {
	bothWays(t, func(t *testing.T, e *env) {
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"S"},{"op":"write","tab":"Sheet1","at":"A1","rows":[[5],[1],[3,"=S!A1"],[7],[9]]}]}`)
		alice := e.person("Alice")
		if ack := alice.do(e.deleteRowsOp("Sheet1", 3, 1)); status(ack) != "applied" {
			t.Fatalf("the delete: %s", status(ack))
		}
		e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:4","by":"A"}]}`)
		wantUndo(t, "undo of the delete", undo(t, alice), undoAck{Applied: 1})
		e.want("A2", "3")
		e.wantText("B2", "=#REF!")
		if got := storedAt(e, "B2"); got != "=#REF!" {
			t.Fatalf("the returned formula is stored as %q", got)
		}
		e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"S","name":"` + strings.Repeat("s", maxTabName) + `"}]}`)
		e.restart()
		e.wantText("B2", "=#REF!")
		e.want("B2", "#REF!")
	})
}

// formulaAt returns the parsed formula of a cell.
func formulaAt(e *env, addr string) *formula.Formula {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t, row, col := e.at(addr)
	return t.cell(row, col).f
}

// TestReturnedFormulasShareTheirEndedForm deletes a row and a column that
// each hold two cells of the same formula, then sorts the rows. The row
// returns to another place than the one its formulas were kept for, where
// their reference ends as it is put back; the column's cells move with the
// sorted rows while the entry keeps them, and their reference ends there.
// Either way the two cells share one parsed formula afterwards, as equal
// formulas do (technical design, section 4.2), from an undo entry in
// memory and from one in a scratch file.
func TestReturnedFormulasShareTheirEndedForm(t *testing.T) {
	bothWays(t, func(t *testing.T, e *env) {
		e.mustAgent(`{"ops":[{"op":"add_tab","name":"S"},{"op":"write","tab":"Sheet1","at":"A1","rows":[[5],[1],[3,"=S!$A1","=S!$A1"],[7],[9]]}]}`)
		alice := e.person("Alice")
		if ack := alice.do(e.deleteRowsOp("Sheet1", 3, 1)); status(ack) != "applied" {
			t.Fatalf("the delete of the row: %s", status(ack))
		}
		e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:4","by":"A"}]}`)
		wantUndo(t, "undo of the row's delete", undo(t, alice), undoAck{Applied: 1})
		e.wantText("B2", "=#REF!")
		e.wantText("C2", "=#REF!")
		if b, c := formulaAt(e, "B2"), formulaAt(e, "C2"); b != c {
			t.Errorf("the returned row keeps two formulas of %q", b.Stored())
		}

		e.mustAgent(`{"ops":[{"op":"add_tab","name":"T"},{"op":"write","tab":"T","at":"A1","rows":[[5],[6],[1,"=S!$A1"],[2,"=S!$A2"]]}]}`)
		bob := e.person("Bob")
		if ack := bob.do(e.deleteColsOp("T", 2, 1)); status(ack) != "applied" {
			t.Fatalf("the delete of the column: %s", status(ack))
		}
		e.mustAgent(`{"ops":[{"op":"sort","tab":"T","rows":"1:4","by":"A"}]}`)
		wantUndo(t, "undo of the column's delete", undo(t, bob), undoAck{Applied: 1})
		e.wantText("T!B1", "=#REF!")
		e.wantText("T!B2", "=#REF!")
		if one, two := formulaAt(e, "T!B1"), formulaAt(e, "T!B2"); one != two {
			t.Errorf("the returned column keeps two formulas of %q", one.Stored())
		}
	})
}

// A reference to a tab that no tab is called any more is not past what a
// formula can name: it shows #REF! and keeps its slot, its tab name
// included. A row that holds such a reference is deleted, its tab is
// deleted, a sort orders the rows and the row returns to another place.
// B2 names the deleted tab at a place that exists there and keeps it;
// C2 names a row above the top of the grid there and ends. The kept
// reference stays through a revision view, a rollback and a restart, and
// reads the tab again once the tab is recovered from a revision or the
// tab's deletion is taken back.
func TestReturnedRowKeepsReferencesToADeletedTab(t *testing.T) {
	for _, back := range []string{"recovery", "undo"} {
		t.Run(back, func(t *testing.T) {
			bothWays(t, func(t *testing.T, e *env) {
				ctx := context.Background()
				e.mustAgent(`{"ops":[{"op":"add_tab","name":"S"},{"op":"write","tab":"S","at":"A1","rows":[[10],[20],[30],[40],[50]]},
					{"op":"write","tab":"Sheet1","at":"A1","rows":[[5],[1],[3,"=S!A5","=S!A1"],[7],[9]]}]}`)
				s, rev := e.tabID("S"), e.b.importAll(t)
				alice, bob := e.person("Alice"), e.person("Bob")
				if back == "undo" {
					if ack := bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, s)); status(ack) != "applied" {
						t.Fatalf("the tab's delete: %s", status(ack))
					}
				} else {
					e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"S"}]}`)
				}
				if ack := alice.do(e.deleteRowsOp("Sheet1", 3, 1)); status(ack) != "applied" {
					t.Fatalf("the row's delete: %s", status(ack))
				}
				e.mustAgent(`{"ops":[{"op":"sort","tab":"Sheet1","rows":"1:4","by":"A"}]}`)
				wantUndo(t, "undo of the row's delete", undo(t, alice), undoAck{Applied: 1})
				check := func(when, b2 string) {
					t.Helper()
					e.want("A2", "3")
					if got := storedAt(e, "B2"); got != "={S!R[2]C[-1]}" {
						t.Errorf("%s: B2 is stored as %q", when, got)
					}
					e.wantText("B2", "=S!A4")
					e.want("B2", b2)
					if got := storedAt(e, "C2"); got != "=#REF!" {
						t.Errorf("%s: C2 is stored as %q", when, got)
					}
					e.want("C2", "#REF!")
				}
				check("after the undo", "#REF!")

				if back == "undo" {
					wantUndo(t, "undo of the tab's delete", undo(t, bob), undoAck{Applied: 1})
					check("after the tab came back", "40")
					return
				}
				n := closeRevision(e)
				e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["B2"]}]}`)
				files, date, release, err := e.b.RevisionFiles(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				if err := e.viewRevision(n, files, date); err != nil {
					t.Fatalf("the view: %v", err)
				}
				if err := e.in.Rollback(ctx, files, "Alice", "kept"); err != nil {
					t.Fatalf("the rollback: %v", err)
				}
				check("after the rollback", "#REF!")
				e.restart()
				check("after a restart", "#REF!")
				if _, err := e.person("Carol").recover(rev, s); err != nil {
					t.Fatal(err)
				}
				check("after the recovery", "40")
				e.restart()
				check("after a restart with the tab back", "40")
			})
		})
	}
}
