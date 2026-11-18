package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/benchdata"
	"github.com/gosuda/flats/internal/sheet/formula"
)

// load applies a benchdata fixture through agent calls, importing the
// outbox as it goes, as a host does.
func load(t testing.TB, e *env, fixture func(func([]byte) error) (benchdata.Manifest, error)) benchdata.Manifest {
	t.Helper()
	m, err := fixture(func(call []byte) error {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(call, &probe); err != nil {
			return err
		}
		probe["state"], _ = json.Marshal(e.state())
		raw, _ := json.Marshal(probe)
		u, err := DecodeUpdate(raw)
		if err != nil {
			return err
		}
		if _, err := e.in.Update(context.Background(), u, 0); err != nil {
			return err
		}
		e.b.importAll(t)
		return nil
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return m
}

func percentile(samples []time.Duration, p float64) time.Duration {
	sorted := slices.Clone(samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[min(len(sorted)-1, int(float64(len(sorted))*p))]
}

// TestReferenceSheet builds the reference sheet of the success criteria,
// checks the generator's expected totals against the engine, and measures
// the engine side of the Open, Recalculation and Structure criteria. The
// browser side is measured by the scripts of section 22.4.
func TestReferenceSheet(t *testing.T) {
	// The criteria are measured on the reference sheet as it is: it has
	// no smaller size.
	full(t, "builds the reference sheet of 400,000 cells and measures the criteria on it")
	e := start(t)
	began := time.Now()
	m := load(t, e, benchdata.Reference)
	built := time.Since(began)
	if m.Tabs != len(e.tabNames()) {
		t.Fatalf("%d tabs, manifest says %d", len(e.tabNames()), m.Tabs)
	}
	e.in.mu.RLock()
	count := e.in.wb.count
	distinct := len(e.in.wb.formulas)
	e.in.mu.RUnlock()
	if count.formulas != m.Formulas || count.cells < m.Cells {
		t.Fatalf("the sheet holds %d cells and %d formulas, manifest says %d and %d", count.cells, count.formulas, m.Cells, m.Formulas)
	}
	// Cells that share a stored formula share one parsed formula.
	if distinct > 64 {
		t.Errorf("%d distinct parsed formulas for %d formula cells", distinct, count.formulas)
	}
	totals := func() []float64 {
		e.in.mu.RLock()
		defer e.in.mu.RUnlock()
		t2 := e.in.wb.tab(benchdata.TotalsTab)
		out := make([]float64, benchdata.LargeCols)
		for i := range out {
			out[i] = t2.cell(RowID(t2.rows.ids[i]), ColID(t2.cols.ids[1])).val.Number()
		}
		return out
	}
	if got := totals(); !slices.Equal(got, m.Totals) {
		t.Fatalf("totals = %v\nwant     %v", got, m.Totals)
	}

	// Load after a restart: read sheet.db, parse, build the index, evaluate.
	var ms runtime.MemStats
	began = time.Now()
	e.restart()
	loaded := time.Since(began)
	runtime.GC()
	runtime.ReadMemStats(&ms)
	if got := totals(); !slices.Equal(got, m.Totals) {
		t.Fatal("the totals changed in the restart")
	}

	// Recalculation: change one input cell of the large tab. Its row's two
	// formulas and the column totals follow in the same delta, on a screen
	// of the large tab and on a screen of the totals tab.
	s := serve(e)
	large, sums := e.tabID(benchdata.LargeTab), e.tabID(benchdata.TotalsTab)
	began = time.Now()
	editor := s.dial("private", `{"type":"hello","version":1,"name":"Editor","pending":0}`)
	editor.next("welcome")
	_, cells := editor.watch(1, large)
	snapshot := time.Since(began)
	if len(cells) < benchdata.LargeRows*benchdata.LargeCols {
		t.Fatalf("the snapshot holds %d cells", len(cells))
	}
	other := s.dial("private", `{"type":"hello","version":1,"name":"Other","pending":0}`)
	other.next("welcome")
	other.watch(1, sums)
	alice := e.person("Alice")
	_, row, col := e.ids(benchdata.LargeTab + "!C2")
	var edits []time.Duration
	expect := slices.Clone(m.Totals)
	e.in.mu.RLock()
	lt := e.in.wb.byID[large]
	qty := lt.cell(row, col).val.Number()
	price := lt.cell(row, ColID(lt.cols.ids[3])).val.Number()
	discount := lt.cell(row, ColID(lt.cols.ids[5])).val.Number()
	e.in.mu.RUnlock()
	for i := 0; i < 50; i++ {
		next := float64(1 + (i*7)%90)
		base := e.seq()
		began := time.Now()
		ack := alice.act(base, fmt.Sprintf(`{"op":"set","tab":%d,"row":%d,"col":%d,"text":"%v"}`, large, row, col, next))
		edits = append(edits, time.Since(began))
		if status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		expect[2] += next - qty
		expect[4] += (next - qty) * price
		expect[6] += (next - qty) * price * (1 - discount)
		qty = next
		// Both screens get the edit and what follows from it in one delta.
		own, far := editor.next("delta"), other.next("delta")
		if own["seq"] != ack.Seq || far["seq"] != ack.Seq {
			t.Fatalf("deltas %v and %v for the action at %s", own["seq"], far["seq"], ack.Seq)
		}
		if n := len(own["cells"].([]any)[0].([]any)[1].([]any)); n != 3 {
			t.Fatalf("the large tab's delta carries %d cells of the row, want the input and its two formulas", n)
		}
		if n := len(far["cells"].([]any)); n != 3 {
			t.Fatalf("the totals tab's delta carries %d rows, want three totals", n)
		}
	}
	if got := totals(); !slices.Equal(got, expect) {
		t.Fatalf("totals after the edits = %v\nwant %v", got, expect)
	}

	// Structure: insert a row at the top of the large tab.
	var inserts []time.Duration
	for i := 0; i < 5; i++ {
		op := e.insertRowsOp(benchdata.LargeTab, 2, 1)
		began := time.Now()
		ack := alice.do(op)
		inserts = append(inserts, time.Since(began))
		if status(ack) != "applied" {
			t.Fatal(status(ack))
		}
		d := editor.next("delta")
		if d["struct"] == nil || d["cells"] != nil {
			t.Fatalf("an insert at the top sends structure and no cell: %v", d)
		}
	}
	e.wantText(benchdata.LargeTab+"!E8", "=C8*D8")
	if got := totals(); !slices.Equal(got, expect) {
		t.Fatal("the totals changed with the inserted rows")
	}
	var sortTime time.Duration
	{
		began := time.Now()
		e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"sort","tab":%q,"rows":"7:20006","by":"E","desc":true}]}`, benchdata.LargeTab))
		sortTime = time.Since(began)
	}
	info, _ := os.Stat(e.in.path)
	t.Logf("reference sheet (digest %s): %d cells, %d formulas, %d distinct parsed", m.Digest[:12], count.cells, count.formulas, distinct)
	t.Logf("build through %d agent calls: %v; sheet.db %d MiB", m.Calls, built.Round(time.Millisecond), info.Size()>>20)
	t.Logf("load after restart (read, index, full pass): %v; Go heap in use after load %d MiB", loaded.Round(time.Millisecond), ms.HeapInuse>>20)
	t.Logf("first snapshot of the large tab over a socket: %v", snapshot.Round(time.Millisecond))
	t.Logf("one input change, commit included: p50 %v, p95 %v, max %v (50 edits)", percentile(edits, 0.5).Round(10*time.Microsecond), percentile(edits, 0.95).Round(10*time.Microsecond), percentile(edits, 1).Round(10*time.Microsecond))
	t.Logf("insert a row at the top: p50 %v, max %v (5 inserts)", percentile(inserts, 0.5).Round(time.Millisecond), percentile(inserts, 1).Round(time.Millisecond))
	t.Logf("sort 20,000 rows through an agent call with its revision: %v", sortTime.Round(time.Millisecond))
	if p95 := percentile(edits, 0.95); p95 > 150*time.Millisecond {
		t.Errorf("one input change takes p95 %v on the server alone, over the criterion's 150 ms", p95)
	}
	if worst := percentile(inserts, 1); worst > time.Second {
		t.Errorf("an insert at the top takes %v on the server alone, over the criterion's 1 s", worst)
	}
}

// TestLimitsFixture builds the limits fixture: 100,000 formulas that read
// 20,000,000 cells beside typed text up to its limit, and measures the
// recalculations the limits bound. It holds 50,000,000 characters and has
// no smaller size, so it runs with the full-size cases.
func TestLimitsFixture(t *testing.T) {
	full(t, "builds a sheet at the formula, read and typed text limits")
	e := start(t)
	began := time.Now()
	var cells int
	m := load(t, e, func(emit func([]byte) error) (benchdata.Manifest, error) {
		m, n, err := benchdata.Limits(emit)
		cells = n
		return m, err
	})
	built := time.Since(began)
	e.in.mu.Lock()
	count := e.in.wb.count
	typed, _ := e.in.wb.typedText(nil)
	e.in.mu.Unlock()
	if count.formulas != benchdata.LimitsFormulas || count.reads != benchdata.LimitsReads || typed > maxSheetText || typed < maxSheetText-40000 {
		t.Fatalf("counters = %+v, %d characters of typed text with %d text cells", count, typed, cells)
	}
	e.want("Calc!CV1000", "1100")
	// One more formula passes the limit of formulas: it reads no cell, so
	// it leaves the sheet's reads at their limit. One more cell read by a
	// formula that is there passes the sheet's read limit, and one more
	// character than the typed text has room for its text limit.
	_, err := e.agent(`{"ops":[{"op":"write","tab":"Nums","at":"L1","rows":[["=1"]]}]}`)
	e.wantRefusal(err, "sheet_capacity", "formulas")
	room := int(maxSheetText - typed)
	if room < len("+Nums!A1") || room >= formula.MaxTextLen {
		t.Fatalf("the fixture leaves room for %d characters of typed text", room)
	}
	_, err = e.agent(`{"ops":[{"op":"write","tab":"Calc","at":"A1","rows":[["=SUM(Nums!$A$1:$J$20)+Nums!A1"]]}]}`)
	e.wantRefusal(err, "sheet_capacity", "sheet_reads")
	_, err = e.agent(jsonf(`{"ops":[{"op":"write","tab":"Text","at":"B1","rows":[[%q]]}]}`, strings.Repeat("t", room+1)))
	e.wantRefusal(err, "sheet_capacity", "sheet_text")
	// An edit of a number every formula reads: a full pass of 20,000,000 reads.
	alice := e.person("Alice")
	began = time.Now()
	if ack := alice.set("Nums!A1", "2"); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	edit := time.Since(began)
	e.want("Calc!A1", "1101")
	work := e.in.lastWork.Load()
	began = time.Now()
	if ack := alice.do(e.insertRowsOp("Calc", 1, 1)); status(ack) != "applied" {
		t.Fatal(status(ack))
	}
	insert := time.Since(began)
	began = time.Now()
	e.restart()
	loaded := time.Since(began)
	e.want("Calc!A2", "1101")
	t.Logf("limits fixture (digest %s): %d formulas, %d static reads, %d characters of typed text in %d calls, built in %v", m.Digest[:12], count.formulas, count.reads, typed, m.Calls, built.Round(time.Millisecond))
	t.Logf("edit of a cell every formula reads: %v, %d units of work", edit.Round(time.Millisecond), work)
	t.Logf("insert a row at the top of the formula tab: %v", insert.Round(time.Millisecond))
	t.Logf("load after restart: %v", loaded.Round(time.Millisecond))
}

// The steps of a pass without a check point are bounded by the size of
// the sheet (technical design, section 10.5). These benchmarks measure
// them at the sheet's limits.

// BenchmarkRowsByPosAtLimits puts the rows of every tab in position order,
// for one tab of 100,000 filled rows and for 50 tabs of 100,000 rows that
// hold the 500,000 records of the sheet one to a row.
func BenchmarkRowsByPosAtLimits(b *testing.B) {
	for _, c := range []struct {
		name         string
		tabs, filled int
	}{{"one tab", 1, formula.MaxRows}, {"every tab", maxTabs, maxCells / maxTabs}} {
		b.Run(c.name, func(b *testing.B) {
			wb := newWorkbook("")
			for i := range c.tabs {
				t := wb.newTab(fmt.Sprint("T", i), formula.MaxRows, 1, 0)
				wb.placeTab(t, i)
				step := formula.MaxRows / c.filled
				for r := 0; r < formula.MaxRows; r += step {
					t.data[RowID(t.rows.ids[r])] = &rowData{cells: []cellSlot{{ColID(t.cols.ids[0]), &Cell{}}}}
				}
			}
			runtime.GC()
			b.ResetTimer()
			for b.Loop() {
				for _, t := range wb.tabs {
					t.byPosOK = false
				}
				wb.settle()
			}
		})
	}
}

// BenchmarkApplyAtLimits applies an overlay of 100,000 formulas whose text
// results hold the 50,000,000 code points of the limit of calculated text,
// letters of four bytes, to cells that hold equal text in other memory,
// and to cells whose text differs in its last byte.
func BenchmarkApplyAtLimits(b *testing.B) {
	const runes = maxCalcText / maxFormulas
	text := func(last rune) string { return strings.Repeat("\U0001D538", runes-1) + string(last) }
	for _, changed := range []bool{false, true} {
		b.Run(fmt.Sprint("changed=", changed), func(b *testing.B) {
			wb := newWorkbook("")
			nodes := make([]*fnode, maxFormulas)
			held, other := make([]formula.Value, maxFormulas), make([]formula.Value, maxFormulas)
			for i := range nodes {
				held[i] = formula.TextValue(text('a'))
				if changed {
					other[i] = formula.TextValue(text('b'))
				} else {
					other[i] = formula.TextValue(text('a'))
				}
				nodes[i] = &fnode{cell: &Cell{val: held[i]}, ov: other[i]}
			}
			out := calcOut{nodes: nodes, text: maxCalcText}
			runtime.GC()
			b.ResetTimer()
			for b.Loop() {
				var j journal
				wb.apply(out, &j, 1)
				if changed {
					// The cells now hold other's text: the next run
					// compares them with held's.
					b.StopTimer()
					held, other = other, held
					for i, n := range nodes {
						n.ov = other[i]
					}
					b.StartTimer()
				}
			}
		})
	}
}

// pruneFormulas drops the entries of the table of shared formulas whose
// formulas the collector has freed. The table keeps no formula alive, so
// a test that measures what the sheet keeps needs nothing more.
func (wb *Workbook) pruneFormulas() {
	runtime.GC()
	wb.dropFreed(maxShared)
}

// formulaSheet is a workbook of n formula cells in one tab, each a formula
// of its own whose stored text is 500 bytes, so that at 100,000 formulas
// their texts are the 50,000,000 characters of the limit of typed text.
// The table of shared formulas holds them. next is one more such formula,
// which no cell holds.
func formulaSheet(tb testing.TB, n int) (wb *Workbook, next *formula.Formula) {
	wb = newWorkbook("")
	t := wb.newTab("T", formula.MaxRows, 1, 0)
	wb.placeTab(t, 0)
	col := ColID(t.cols.ids[0])
	parse := func(i int) *formula.Formula {
		head := fmt.Sprintf("=%d", i)
		f, err := formula.Parse(head+strings.Repeat("+1", (maxSheetText/maxFormulas-len(head))/2), formula.Pos{}, formula.Limits{})
		if err != nil {
			tb.Fatal(err)
		}
		return f
	}
	for r := range n {
		t.put(RowID(t.rows.ids[r]), col, &Cell{f: wb.intern(parse(r))})
	}
	wb.count.formulas = n
	return wb, parse(-1)
}

// BenchmarkInternAtLimits takes a new formula into the table of shared
// formulas of a sheet of 100,000 formulas.
func BenchmarkInternAtLimits(b *testing.B) {
	wb, _ := formulaSheet(b, maxFormulas)
	runtime.GC()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		f, err := formula.Parse("="+strconv.Itoa(i), formula.Pos{}, formula.Limits{})
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		wb.intern(f)
	}
}
