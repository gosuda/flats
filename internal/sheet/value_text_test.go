package sheet

import (
	"fmt"
	"strings"
	"testing"
)

// paddedText is a formula of 8,000 characters whose value is the short
// text of i: the text written in it, followed by spaces.
func paddedText(i int) string {
	f := fmt.Sprintf(`="%d"`, i)
	return f + strings.Repeat(" ", 8000-len(f))
}

// writeSources writes paddedText into A1:An and, when refs, =Ai beside
// each into column B.
func writeSources(e *env, n int, refs bool) {
	e.t.Helper()
	const batch = 100
	for from := 0; from < n; from += batch {
		rows := make([]string, min(batch, n-from))
		for i := range rows {
			if refs {
				rows[i] = fmt.Sprintf("[%q,%q]", paddedText(from+i+1), fmt.Sprintf("=A%d", from+i+1))
			} else {
				rows[i] = fmt.Sprintf("[%q]", paddedText(from+i+1))
			}
		}
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, from+1, strings.Join(rows, ",")))
	}
	e.b.importAll(e.t)
	e.b.forgetRevisions()
}

// TestCaptureKeepsNoFormulaTextOfValues writes 6,000 formulas of 8,000
// characters whose values are short texts written in them, and beside
// each a formula that reads it. A capture of the reading formulas alone,
// with a limit of one cell a page, keeps their values. The default run
// has a tenth of the formulas. After both columns are cleared the cursor
// still reads the values, the capture is charged for what it keeps of the
// heap, and the heap lets it go with the capture. A value that was a part
// of the stored text of the formula it came from would keep that text
// alive, 8,000 bytes for a value of a few.
func TestCaptureKeepsNoFormulaTextOfValues(t *testing.T) {
	slow(t, "writes thousands of long formulas")
	n := sized(6000, 600)
	e := start(t)
	writeSources(e, n, true)
	e.want("B2", "2")
	base := captureBytes.Load()
	first := e.read(ReadQuery{Tab: "Sheet1", Range: fmt.Sprintf("B1:B%d", n), Limit: 1})
	cursor := cursorOf(t, first)
	charged := captureBytes.Load() - base
	e.mustAgent(jsonf(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:B%d"]}]}`, n))
	e.want("B2", "")
	page := e.read(ReadQuery{Cursor: cursor})
	if got := fmt.Sprint(page["rows"]); got != "[[map[formula:=A2 value:2]]]" {
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
	t.Logf("%d values: the capture is charged %d KiB and kept %d KiB of the heap", n, charged>>10, kept>>10)
	if !measures {
		return
	}
	// A reading of the heap is up to 1 MiB above what is reachable.
	if kept > charged+1<<20 || kept > int64(n)*1<<10 {
		t.Errorf("the capture of %d short values kept %d KiB of the heap and is charged %d KiB", n, kept>>10, charged>>10)
	}
}

// TestPastedValuesKeepNoFormulaText pastes as values 6,000 formulas of
// 8,000 characters whose values are short texts written in them, and the
// sources are cleared. The default run has a tenth of them. The pasted
// cells hold their texts in memory of their own: when the person then
// clears them, the undo payload that keeps them is counted for what it
// keeps of the heap.
func TestPastedValuesKeepNoFormulaText(t *testing.T) {
	slow(t, "writes thousands of long formulas")
	n := sized(6000, 600)
	e := start(t)
	writeSources(e, n, false)
	alice := e.person("Alice")
	tab := e.tabID("Sheet1")
	rect := func(col int) string {
		return fmt.Sprintf(`"rows":%s,"cols":%s`, e.rowsOf("Sheet1", 1, n), e.colsOf("Sheet1", col, 1))
	}
	if ack := alice.do(jsonf(`{"op":"copy","from":{"tab":%d,%s},"to":{"tab":%d,%s},"mode":"values","cut":false}`, tab, rect(1), tab, rect(3))); status(ack) != "applied" {
		t.Fatalf("paste values: %s", status(ack))
	}
	e.wantText("C2", "'2")
	e.mustAgent(jsonf(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:A%d"]}]}`, n))
	if ack := alice.do(jsonf(`{"op":"clear","tab":%d,%s}`, tab, rect(3))); status(ack) != "applied" {
		t.Fatalf("clear: %s", status(ack))
	}
	e.want("C2", "")
	if u := undo(t, alice); u.Applied != n {
		t.Fatalf("undo of the clear = %+v", u)
	}
	e.wantText("C2", "'2")
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
	t.Logf("%d pasted values: undo counts %d KiB in memory and kept %d KiB of the heap", n, stacks>>10, kept>>10)
	if measures && (kept > stacks+1<<20 || kept > int64(n)*1<<10) {
		t.Errorf("the undo payloads of %d short pasted values kept %d KiB of the heap and count %d KiB", n, kept>>10, stacks>>10)
	}
}

// dropUndo drops the undo history of a person. The pool keeps the entries
// of the last action it admitted until it admits the next, so the person
// then sets a cell of its own, whose entry holds next to nothing.
func dropUndo(e *env, p *person) {
	e.t.Helper()
	e.in.mu.Lock()
	p.s.undo.drop()
	e.in.mu.Unlock()
	if ack := p.set("Sheet1!A50", "1"); status(ack) != "applied" {
		e.t.Fatalf("set: %s", status(ack))
	}
}
