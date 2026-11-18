package sheet

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// TestInferredCurrencyKeepsNoInput imports, in separate calls, amounts of
// 32,767 characters with a dollar sign into fresh cells of a column, so
// that each cell gets a currency format of a style of its own. Alice then
// deletes the column, and her undo entry keeps those styles. The entry
// stays in memory at the ordinary thresholds, and what it keeps of the
// heap is no more than it counts: a format whose sign was a part of its
// amount would keep the whole text of the amount alive, 32 KiB for a sign
// of one byte. When her history goes, the heap gives back what it kept.
func TestInferredCurrencyKeepsNoInput(t *testing.T) {
	slow(t, "imports hundreds of long amounts")
	n := sized(1000, 100)
	e := start(t)
	amount := func(i int) string {
		zeros := formula.MaxTextLen - 2 - len(fmt.Sprint(i))
		return "$" + strings.Repeat("0", zeros) + fmt.Sprint(i)
	}
	for i := 1; i <= n; i++ {
		e.mustAgent(jsonf(`{"ops":[{"op":"import_csv","tab":"Sheet1","at":"A%d","csv":%q}]}`, i, amount(i)))
		if i%100 == 0 {
			// The host takes the revisions, as it does between calls.
			e.b.importAll(t)
		}
	}
	e.want("A1", "$1.00")
	e.want("A99", "$99.00")
	e.in.mu.RLock()
	styles := 0
	for _, s := range e.in.wb.styles.byID {
		if s.f.Number.Kind == formula.NumberCurrency && s.refs > 0 {
			styles++
		}
	}
	e.in.mu.RUnlock()
	if styles != n {
		t.Fatalf("%d amounts imported in separate calls have %d currency styles, want one each", n, styles)
	}

	alice := e.person("Alice")
	if ack := alice.do(e.deleteColsOp("Sheet1", 1, 1)); status(ack) != "applied" {
		t.Fatalf("delete column A: %s", status(ack))
	}
	if alice.spilled(0) != nil {
		t.Fatal("the entry went to a scratch file at the ordinary thresholds")
	}
	e.b.importAll(t)
	e.b.forgetRevisions()
	e.tick()
	sheetHeap := func() int64 { return quietHeap(t, 1<<20) - e.b.blobBytes() }
	_, stacks := e.residentUndo()
	held := sheetHeap()
	dropUndo(e, alice)
	e.tick()
	kept := held - sheetHeap()
	t.Logf("%d currency cells: undo counts %d KiB in memory and kept %d KiB of the heap", n, stacks>>10, kept>>10)
	// A reading of the heap is up to 1 MiB above what is reachable.
	if measures && kept > stacks+1<<20 {
		t.Errorf("the undo entry of %d currency cells kept %d KiB of the heap and counts %d KiB", n, kept>>10, stacks>>10)
	}
}

// TestSpilledEntryKeepsNoFormulaText has Alice clear, in separate actions,
// cells whose long formulas each name a tab of their own. Her session
// holds more than its budget, so the older entries go to scratch files
// and keep in memory only the names of the tabs their formulas name. A
// name that was a part of its formula's text would keep that text of
// 24 KB alive for a name of a few bytes. What the history keeps of the
// heap is no more than it counts.
func TestSpilledEntryKeepsNoFormulaText(t *testing.T) {
	slow(t, "writes hundreds of long formulas")
	const perEntry, entries = 32, 20
	e := start(t)
	alice := e.person("Alice")
	tab := e.tabID("Sheet1")
	pad := strings.Repeat("가", 7950)
	for j := 0; j < entries; j++ {
		first := j*perEntry + 1
		rows := make([]string, perEntry)
		for k := range rows {
			// A name in lowercase folds to itself.
			rows[k] = fmt.Sprintf("[%q]", fmt.Sprintf(`=t%dx%d!A1&"%s"`, j, k, pad))
		}
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, first, strings.Join(rows, ",")))
		op := jsonf(`{"op":"clear","tab":%d,"rows":%s,"cols":%s}`, tab, e.rowsOf("Sheet1", first, perEntry), e.colsOf("Sheet1", 1, 1))
		if ack := alice.do(op); status(ack) != "applied" {
			t.Fatalf("clear %d: %s", j, status(ack))
		}
	}
	spilled := 0
	for i := 0; i < entries; i++ {
		if alice.spilled(i) != nil {
			spilled++
		}
	}
	if spilled < entries/2 {
		t.Fatalf("%d of %d entries went to scratch files; the test needs most of them there", spilled, entries)
	}
	e.b.importAll(t)
	e.b.forgetRevisions()
	e.tick()
	sheetHeap := func() int64 { return quietHeap(t, 1<<20) - e.b.blobBytes() }
	_, stacks := e.residentUndo()
	held := sheetHeap()
	dropUndo(e, alice)
	e.tick()
	kept := held - sheetHeap()
	t.Logf("%d entries, %d in scratch files: undo counts %d KiB in memory and kept %d KiB of the heap", entries, spilled, stacks>>10, kept>>10)
	if measures && kept > stacks+1<<20 {
		t.Errorf("an undo history of %d entries, %d in scratch files, kept %d KiB of the heap and counts %d KiB", entries, spilled, kept>>10, stacks>>10)
	}
}
