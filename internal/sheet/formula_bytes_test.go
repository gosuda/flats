package sheet

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// denseSum is a formula of about 8,000 characters that no other call
// returns, a number and 4,000 additions of zero, which parse into a tree
// of 8,001 nodes.
func denseSum(i int) string { return "=" + strconv.Itoa(i) + strings.Repeat("+0", 4000) }

// longReferences is a formula of 950 references, about 7,600 characters,
// that no other call returns.
func longReferences(i int) string {
	var sb strings.Builder
	sb.WriteString("=")
	for k := 0; k < 950; k++ {
		fmt.Fprintf(&sb, "$Z$%d+", 902+(i+k)%7)
	}
	fmt.Fprintf(&sb, "Z%d", 950+i)
	return sb.String()
}

// TestFormulaBytesCoverSharedFormulas shares many distinct formulas
// through the table of a workbook, as every formula a cell gets is
// shared, and compares formulaBytes with what the heap keeps for each.
func TestFormulaBytesCoverSharedFormulas(t *testing.T) {
	if !measures {
		t.Skip("measures what the heap keeps")
	}
	for name, text := range map[string]func(int) string{
		"short":      func(i int) string { return "=A1+" + strconv.Itoa(i) },
		"sum":        denseSum,
		"references": longReferences,
	} {
		t.Run(name, func(t *testing.T) {
			wb := newWorkbook("")
			parse := func(i int) *formula.Formula {
				f, err := formula.Parse(text(i), formula.Pos{}, formula.Limits{})
				if err != nil {
					t.Fatal(err)
				}
				return wb.intern(f)
			}
			const rounds = 3
			copies := int(max(100, (3<<20)/formulaBytes(parse(-1))))
			kept := make([]*formula.Formula, 0, rounds*copies)
			measured := int64(-1)
			for round := range rounds {
				before := heapInUse()
				for i := range copies {
					kept = append(kept, parse(round*copies+i))
				}
				// What the heap took beside the formulas only adds to a
				// round, so the round that grew it least is the measure.
				if grown := (int64(heapInUse()) - int64(before)) / int64(copies); measured < 0 || grown < measured {
					measured = grown
				}
			}
			counted := formulaBytes(kept[0])
			runtime.KeepAlive(kept)
			t.Logf("formulaBytes %d, Size %d, the heap keeps %d", counted, kept[0].Size(), measured)
			if counted < measured || counted > measured+measured/8+sharedFormulaBytes {
				t.Errorf("formulaBytes is %d for a shared formula the heap keeps %d of", counted, measured)
			}
		})
	}
}

// TestCaptureKeepsNoFormulaTree fills a column with distinct long sums,
// reads it one cell a page and clears the cells while the read's capture
// is kept. The capture keeps what its pages show of the formulas, inside
// what it is charged, and gives that back when it goes.
func TestCaptureKeepsNoFormulaTree(t *testing.T) {
	slow(t, "writes thousands of long formulas")
	n := sized(2000, 200)
	e := start(t, unhurried)
	const batch = 100
	for from := 0; from < n; from += batch {
		rows := make([]string, min(batch, n-from))
		for i := range rows {
			rows[i] = fmt.Sprintf("[%q]", denseSum(from+i))
		}
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, from+1, strings.Join(rows, ",")))
	}
	e.b.importAll(t)
	e.b.forgetRevisions()
	base := captureBytes.Load()
	first := e.read(ReadQuery{Tab: "Sheet1", Range: fmt.Sprintf("A1:A%d", n), Limit: 1})
	cursor := cursorOf(t, first)
	charged := captureBytes.Load() - base
	e.mustAgent(jsonf(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:A%d"]}]}`, n))
	e.want("A2", "")
	// The cursor still reads the formulas as they were.
	page := e.read(ReadQuery{Cursor: cursor})
	if got := fmt.Sprint(page); !strings.Contains(got, denseSum(1)) {
		t.Fatalf("the second page after the clear: %.200s", got)
	}
	e.b.importAll(t)
	e.b.forgetRevisions()
	sheetHeap := func() int64 { return quietHeap(t, 1<<20) - e.b.blobBytes() }
	held := sheetHeap()
	e.in.captures.clear()
	if got := captureBytes.Load(); got != base {
		t.Fatalf("after the capture went %d bytes stay charged, want %d", got, base)
	}
	freed := sheetHeap()
	kept := held - freed
	t.Logf("%d formulas: the capture is charged %d KiB and kept %d KiB of the heap", n, charged>>10, kept>>10)
	if !measures {
		return
	}
	// A parsed sum takes over 500 KiB; its text form about 8 KiB. A
	// reading of the heap is up to 1 MiB above what is reachable.
	if kept > charged+1<<20 || kept > int64(n)*16<<10 {
		t.Errorf("the capture of %d formulas kept %d KiB of the heap and is charged %d KiB", n, kept>>10, charged>>10)
	}
}

// TestUndoChargesWhatFormulasKeep has one person write one cell again and
// again, each time with a distinct long formula, so that the earlier
// formulas are held by undo records only. What the records are counted
// at covers what the heap keeps for them, so the session's 4 MiB sends
// the payloads beyond it to scratch files, and each undo still works.
func TestUndoChargesWhatFormulasKeep(t *testing.T) {
	slow(t, "parses dozens of long formulas")
	for name, text := range map[string]func(int) string{
		"sums":       denseSum,
		"references": longReferences,
	} {
		t.Run(name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"Z1","rows":[[1]]}]}`)
			p := e.person("Alice")
			const writes = 21
			e.b.importAll(t)
			e.b.forgetRevisions()
			sheetHeap := func() int64 { return quietHeap(t, 1<<20) - e.b.blobBytes() }
			base := sheetHeap()
			for i := range writes {
				if ack := p.set("A1", text(i)); status(ack) != "applied" {
					t.Fatalf("write %d: %s", i, status(ack))
				}
			}
			e.b.importAll(t)
			e.b.forgetRevisions()
			grown := sheetHeap() - base
			pool, stacks := e.residentUndo()
			files, _ := e.scratch()
			spilled := 0
			for i := range writes {
				if p.spilled(i) != nil {
					spilled++
				}
			}
			// The live cell holds the last formula; the instance keeps the
			// recent deltas, with the text of each write.
			e.in.mu.RLock()
			live := formulaBytes(e.in.wb.tab("Sheet1").cellAt(formula.Pos{}).f)
			e.in.mu.RUnlock()
			t.Logf("%d writes: the heap grew by %d KiB; undo counts %d KiB in memory, %d of %d entries in %d scratch files, the cell %d KiB",
				writes, grown>>10, stacks>>10, spilled, writes, files, live>>10)
			if pool != stacks || stacks > undoSessionBytes {
				t.Fatalf("the pool counts %d bytes and the stacks hold %d", pool, stacks)
			}
			if spilled == 0 {
				t.Errorf("no entry of %d long formulas left memory", writes)
			}
			if measures {
				if slack := int64(2 << 20); grown > stacks+live+slack {
					t.Errorf("the heap grew by %d KiB; undo counts %d KiB in memory and the cell holds %d KiB", grown>>10, stacks>>10, live>>10)
				}
			}
			for i := writes - 2; i >= writes-6; i-- {
				if u := undo(t, p); u.Applied != 1 || u.Skipped != 0 {
					t.Fatalf("undo to write %d = %+v", i, u)
				}
				e.wantText("A1", text(i))
			}
		})
	}
}
