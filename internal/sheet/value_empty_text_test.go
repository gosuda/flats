package sheet

import (
	"fmt"
	"strings"
	"testing"
)

// emptyOfEmojis is a formula whose value is an empty text that LEFT cuts
// from 131,068 bytes of calculated text.
const emptyOfEmojis = `=LEFT(REPT("😀",32767),0)`

// writeColumn writes formula into rows 1 to n of a column, 100 rows at a
// time.
func writeColumn(e *env, col string, n int, formula func(row int) string) {
	e.t.Helper()
	for from := 1; from <= n; from += 100 {
		rows := make([]string, 0, 100)
		for r := from; r < from+100 && r <= n; r++ {
			rows = append(rows, fmt.Sprintf("[%q]", formula(r)))
		}
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"%s%d","rows":[%s]}]}`, col, from, strings.Join(rows, ",")))
	}
}

// TestEmptyResultsKeepNoTextInTheSheet writes 1,000 formulas whose values
// are empty texts cut from 131,068 bytes each. The default run has a fifth
// of them. An empty text counts nothing in the limit of calculated text,
// so it keeps no memory: the sheet holds the formulas and no more, and
// what it holds goes when they are cleared.
func TestEmptyResultsKeepNoTextInTheSheet(t *testing.T) {
	slow(t, "calculates hundreds of long texts")
	n := sized(1000, 200)
	e := start(t)
	e.b.importAll(t)
	e.b.forgetRevisions()
	sheetHeap := func() int64 { return quietHeap(t, 1<<20) - e.b.blobBytes() }
	before := sheetHeap()
	writeColumn(e, "A", n, func(int) string { return emptyOfEmojis })
	e.wantText("A2", emptyOfEmojis)
	e.want("A2", "")
	if text := e.in.wb.count.calcText; text != 0 {
		t.Fatalf("the empty results count %d characters of calculated text", text)
	}
	e.b.importAll(t)
	e.b.forgetRevisions()
	held := sheetHeap()
	e.mustAgent(jsonf(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:A%d"]}]}`, n))
	e.b.importAll(t)
	e.b.forgetRevisions()
	after := sheetHeap()
	t.Logf("%d formulas of empty results: the sheet holds %d KiB with them and lets %d KiB go when they are cleared", n, (held-before)>>10, (held-after)>>10)
	// A formula takes well under 1 KiB of the heap. A reading of the heap
	// is up to 1 MiB above what is reachable.
	if most := int64(n)<<10 + 1<<20; measures && (held-before > most || held-after > most) {
		t.Errorf("%d formulas of empty results keep %d KiB of the heap, and clearing them lets %d KiB go, more than %d KiB", n, (held-before)>>10, (held-after)>>10, most>>10)
	}
}

// TestCaptureOfEmptyResultsKeepsNoSourceText writes 1,000 formulas of
// 131,068 bytes of calculated text and beside each one that cuts an empty
// text from it. The default run has a fifth of them. A capture of the
// empty results alone, a cell a page, keeps them after both columns are
// cleared, and is charged for what it keeps of the heap: an empty result
// that lay in the text it was cut from would keep the whole text of a
// cell that is gone, and be charged nothing for it.
func TestCaptureOfEmptyResultsKeepsNoSourceText(t *testing.T) {
	slow(t, "calculates hundreds of long texts")
	n := sized(1000, 200)
	e := start(t)
	writeColumn(e, "A", n, func(int) string { return emojis })
	writeColumn(e, "B", n, func(r int) string { return fmt.Sprintf("=LEFT(A%d,0)", r) })
	e.want("B2", "")
	base := captureBytes.Load()
	first := e.read(ReadQuery{Tab: "Sheet1", Range: fmt.Sprintf("B1:B%d", n), Limit: 1})
	cursor := cursorOf(t, first)
	charged := captureBytes.Load() - base
	e.mustAgent(jsonf(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:B%d"]}]}`, n))
	e.wantText("B2", "")
	page := e.read(ReadQuery{Cursor: cursor})
	if got := fmt.Sprint(page["rows"]); got != "[[map[formula:=LEFT(A2,0) value:]]]" {
		t.Fatalf("the second page after the clear holds %s", got)
	}
	e.b.importAll(t)
	e.b.forgetRevisions()
	sheetHeap := func() int64 { return quietHeap(t, 1<<20) - e.b.blobBytes() }
	held := sheetHeap()
	e.in.captures.clear()
	if got := captureBytes.Load(); got != base {
		t.Fatalf("after the capture went %d bytes stay charged, want %d", got, base)
	}
	kept := held - sheetHeap()
	t.Logf("%d empty values: the capture is charged %d KiB and kept %d KiB of the heap", n, charged>>10, kept>>10)
	if measures && (kept > charged+1<<20 || kept > int64(n)<<10) {
		t.Errorf("the capture of %d empty values kept %d KiB of the heap and is charged %d KiB", n, kept>>10, charged>>10)
	}
}

// TestPastedEmptyValuesKeepNoText pastes as values 1,000 formulas whose
// values are empty texts cut from 131,068 bytes each, and the sources are
// cleared. The default run has a fifth of them. The pasted cells hold
// nothing of the texts: when the person then clears them, the undo payload
// that keeps them is counted for what it keeps of the heap.
func TestPastedEmptyValuesKeepNoText(t *testing.T) {
	slow(t, "calculates hundreds of long texts")
	n := sized(1000, 200)
	e := start(t)
	writeColumn(e, "A", n, func(int) string { return emptyOfEmojis })
	alice := e.person("Alice")
	tab := e.tabID("Sheet1")
	rect := func(col int) string {
		return fmt.Sprintf(`"rows":%s,"cols":%s`, e.rowsOf("Sheet1", 1, n), e.colsOf("Sheet1", col, 1))
	}
	if ack := alice.do(jsonf(`{"op":"copy","from":{"tab":%d,%s},"to":{"tab":%d,%s},"mode":"values","cut":false}`, tab, rect(1), tab, rect(3))); status(ack) != "applied" {
		t.Fatalf("paste values: %s", status(ack))
	}
	e.mustAgent(jsonf(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:A%d"]}]}`, n))
	if ack := alice.do(jsonf(`{"op":"clear","tab":%d,%s}`, tab, rect(3))); status(ack) != "applied" {
		t.Fatalf("clear: %s", status(ack))
	}
	e.b.importAll(t)
	e.b.forgetRevisions()
	sheetHeap := func() int64 { return quietHeap(t, 1<<20) - e.b.blobBytes() }
	_, stacks := e.residentUndo()
	held := sheetHeap()
	dropUndo(e, alice)
	kept := held - sheetHeap()
	t.Logf("%d pasted empty values: undo counts %d KiB in memory and kept %d KiB of the heap", n, stacks>>10, kept>>10)
	if measures && (kept > stacks+1<<20 || kept > int64(n)<<10) {
		t.Errorf("the undo payloads of %d pasted empty values kept %d KiB of the heap and count %d KiB", n, kept>>10, stacks>>10)
	}
}

// TestUndoOfDeletedEmptyResultsKeepsNoText has a person delete, ten at a
// time, 100 rows whose formulas give empty texts cut from 131,068 bytes
// each. An entry keeps ten such cells. The cells hold nothing of the
// texts, so what the entries are counted at covers what the heap lets go
// when they are dropped, and the last one undoes.
func TestUndoOfDeletedEmptyResultsKeepsNoText(t *testing.T) {
	slow(t, "calculates a hundred long texts and deletes them")
	e := start(t)
	writeColumn(e, "A", 100, func(int) string { return emptyOfEmojis })
	alice := e.person("Alice")
	const deletions = 10
	for i := range deletions {
		if ack := alice.do(e.deleteRowsOp("Sheet1", 1, 10)); status(ack) != "applied" {
			t.Fatalf("deletion %d: %s", i, status(ack))
		}
	}
	if u := undo(t, alice); u.Applied == 0 || u.Skipped != 0 {
		t.Fatalf("undo of the last deletion = %+v", u)
	}
	e.wantText("Sheet1!A1", emptyOfEmojis)
	e.b.importAll(t)
	e.b.forgetRevisions()
	sheetHeap := func() int64 { return quietHeap(t, 1<<20) - e.b.blobBytes() }
	pool, stacks := e.residentUndo()
	held := sheetHeap()
	dropUndo(e, alice)
	kept := held - sheetHeap()
	t.Logf("undo counts %d KiB in memory and kept %d KiB of the heap", stacks>>10, kept>>10)
	if pool != stacks {
		t.Fatalf("the pool counts %d bytes and the stacks hold %d", pool, stacks)
	}
	if measures && kept > stacks+1<<20 {
		t.Errorf("undo kept %d KiB of the heap and counts %d KiB", kept>>10, stacks>>10)
	}
}
