package sheet

import (
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// freedPending returns the hashes of freed formulas that intern has still
// to drop.
func (wb *Workbook) freedPending() int {
	wb.freed.mu.Lock()
	defer wb.freed.mu.Unlock()
	return len(wb.freed.hashes)
}

// waitFreed collects until the cleanups of the collector have noted at
// least n freed formulas.
func waitFreed(t *testing.T, wb *Workbook, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for wb.freedPending() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the collector noted %d freed formulas, want %d", wb.freedPending(), n)
		}
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
}

// blobBytes returns the bytes of the revision files the fake host holds in
// memory.
func (b *fakeBackend) blobBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	var n int64
	for _, data := range b.blobs {
		n += int64(cap(data))
	}
	return n
}

// forgetRevisions drops the records of the revisions the fake host has
// imported, which a host keeps on disk. The files stay for later imports.
func (b *fakeBackend) forgetRevisions() {
	b.mu.Lock()
	b.revs = slices.Clone(b.revs[len(b.revs)-1:])
	b.mu.Unlock()
}

// TestInternNeedsNoWalk takes a new formula into the table of shared
// formulas of a sheet at the limit of formulas, after as many other
// formulas were freed. The intern looks at no more entries of the table
// than it drops, a bounded part of them, and visits no formula of the
// sheet (workSteps).
func TestInternNeedsNoWalk(t *testing.T) {
	n := sized(maxFormulas, 20_000)
	wb, next := formulaSheet(t, n)
	for i := range n {
		f, err := formula.Parse("="+strconv.Itoa(i), formula.Pos{}, formula.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		wb.intern(f)
	}
	waitFreed(t, wb, n)
	pending := wb.freedPending()
	steps := countWork(func() { wb.intern(next) })
	t.Logf("the intern looked at %d entries of the table of %d and visited %d formulas", steps.shared.Load(), len(wb.formulas), steps.formulas.Load())
	if looked := steps.shared.Load(); looked > freedStep {
		t.Errorf("the intern looked at %d entries of the table, more than the %d it may drop", looked, freedStep)
	}
	if visited := steps.formulas.Load(); visited != 0 {
		t.Errorf("the intern visited %d formulas of the sheet", visited)
	}
	if left := wb.freedPending(); left != pending-freedStep {
		t.Errorf("the intern dropped %d entries of freed formulas, want %d", pending-left, freedStep)
	}
	if len(wb.formulas) > maxShared {
		t.Errorf("the table holds %d entries", len(wb.formulas))
	}
}

// TestSharingSurvivesChurn writes a cell again and again with a new
// formula each time, beside a filled range of one formula and far more
// often than the sheet holds formulas. Cells written with the filled
// formula afterwards share the parsed formula of the cells the writes left
// alone.
func TestSharingSurvivesChurn(t *testing.T) {
	e := start(t)
	alice := e.person("Alice")
	alice.set("A1", "=B1+1")
	ack := alice.do(jsonf(`{"op":"fill","tab":%d,"source":{"rows":%s,"cols":%s},"target":{"rows":%s,"cols":%s}}`, e.tabID("Sheet1"),
		e.rowsOf("Sheet1", 1, 1), e.colsOf("Sheet1", 1, 1), e.rowsOf("Sheet1", 1, 100), e.colsOf("Sheet1", 1, 1)))
	if status(ack) != "applied" {
		t.Fatalf("the fill: %s", status(ack))
	}
	for round := range 50 {
		rows := make([]string, 100)
		for j := range rows {
			rows[j] = fmt.Sprintf(`["=%d+C1"]`, round*100+j)
		}
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"D1","rows":[` + strings.Join(rows, ",") + `]}]}`)
		if round%10 == 9 {
			runtime.GC()
		}
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"E1","rows":[["=F1+1"],["=F2+1"]]}]}`)
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	tab, a1, colA := e.at("A1")
	_, a100, _ := e.at("A100")
	_, e1, colE := e.at("E1")
	_, e2, _ := e.at("E2")
	untouched := tab.cell(a1, colA).f
	if tab.cell(a100, colA).f != untouched {
		t.Fatal("the filled cells hold two parsed formulas")
	}
	if tab.cell(e1, colE).f != untouched || tab.cell(e2, colE).f != untouched {
		t.Error("cells written with the filled formula after the churn hold a parsed formula of their own")
	}
	if n := len(e.in.wb.formulas); n > maxShared {
		t.Errorf("the table holds %d entries", n)
	}
}

// TestChurnKeepsNoParsedFormula fills 99,999 cells with one short
// formula, one less than the limit of formulas, and then writes one cell again and again, each time with a
// formula of its own of about 8,000 characters, through separate agent
// calls. The parsed formulas of the earlier writes are not kept, and
// clearing the cells frees the rest.
func TestChurnKeepsNoParsedFormula(t *testing.T) {
	if !measures {
		t.Skip("measures what the heap keeps")
	}
	slow(t, "writes thousands of long formulas")
	writes := sized(5000, 300)
	e := start(t, unhurried)
	for half := range 2 {
		rows := make([]string, 50_000-half)
		for i := range rows {
			rows[i] = `["=1"]`
		}
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[%s]}]}`, half*50_000+1, strings.Join(rows, ",")))
	}
	long := func(i int) string { return "=" + strconv.Itoa(i) + strings.Repeat("+0", 4000) }
	// The heap that one parsed long formula takes, kept alive by kept.
	before := heapInUse()
	kept, err := formula.Parse(long(-1), formula.Pos{}, formula.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	size := int64(heapInUse()) - int64(before)
	runtime.KeepAlive(kept)

	// The fake host keeps the revision files of the writes in memory; a
	// host keeps them on disk.
	e.b.importAll(t)
	e.b.forgetRevisions()
	sheetHeap := func() int64 { return quietHeap(t, 1<<20) - e.b.blobBytes() }
	base := sheetHeap()
	for i := range writes {
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[[%q]]}]}`, long(i)))
		if i%20 == 19 {
			e.b.importAll(t) // as the host's tick does, so the outbox takes every write
			e.b.forgetRevisions()
		}
	}
	churned := sheetHeap()
	// The sheet keeps the formula of B1, and the instance up to 16 MiB of
	// recent deltas, each with the text of a write.
	bound := 16<<20 + 8*size
	t.Logf("one parsed formula takes %d KiB; after %d writes the heap grew by %d KiB, %d entries in the table",
		size>>10, writes, (churned-base)>>10, len(e.in.wb.formulas))
	if grown := churned - base; grown > bound {
		t.Errorf("after %d writes of formulas of %d KiB each the heap grew by %d MiB; want under %d MiB",
			writes, size>>10, grown>>20, bound>>20)
	}

	e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:B100000"]}]}`)
	cleared := sheetHeap()
	t.Logf("after the clear the heap is %d KiB from where the writes began", (cleared-base)>>10)
	if grown := cleared - base; grown > bound {
		t.Errorf("after the clear the heap is %d MiB above where the writes began; want under %d MiB", grown>>20, bound>>20)
	}
	// The entries go once the collector has freed their formulas.
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.in.mu.Lock()
		e.in.wb.pruneFormulas()
		held := len(e.in.wb.formulas)
		e.in.mu.Unlock()
		if held <= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the clear the table holds %d entries", held)
		}
		time.Sleep(time.Millisecond)
	}
}
