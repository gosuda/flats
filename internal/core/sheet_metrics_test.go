package core

import (
	"context"
	"testing"
	"time"
)

// TestSheetBatchTotalsAcrossAnUnload runs an idle unload inside a metrics
// scrape, after the scrape read the open instances: every batch is counted
// once in that scrape and in the next.
func TestSheetBatchTotalsAcrossAnUnload(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	offset := sheetClock(s)
	newSheet(t, s, "budget")
	writeSheet(t, s, "budget", "rows", sheetBudgetRows)
	if _, err := s.UpdateSheet(ctx, "budget", []byte(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["stateless"]]}]}`), ViaMCP); err == nil {
		t.Fatal("a write without a state was applied")
	}
	want := s.sheetMetrics().Batches
	if want["applied"] < 1 || want["rejected"] < 1 {
		t.Fatalf("batches before the unload = %v", want)
	}
	s.sheetStats.mu.Lock()
	s.sheetStats.scraping = func() {
		s.sheetStats.mu.Lock()
		s.sheetStats.scraping = nil
		s.sheetStats.mu.Unlock()
		unloadSheetNow(t, s, offset, "budget")
	}
	s.sheetStats.mu.Unlock()
	during := s.sheetMetrics().Batches
	if s.liveSheet("budget") != nil {
		t.Fatal("the idle sheet stayed loaded")
	}
	after := s.sheetMetrics().Batches
	for _, result := range []string{"applied", "rejected"} {
		if during[result] != want[result] || after[result] != want[result] {
			t.Errorf("%s batches = %d during the unload and %d after it, want %d both times", result, during[result], after[result], want[result])
		}
	}
}

// TestSheetBatchTotalsCountACreatingWrite covers the write that creates a
// sheet: it runs in an instance that stops before the sheet is served.
func TestSheetBatchTotalsCountACreatingWrite(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	before := s.sheetMetrics().Batches
	out, err := s.UpdateSheet(ctx, "fresh", []byte(`{"ops":[`+sheetBudgetRows+`]}`), ViaMCP)
	if err != nil || !out.Created {
		t.Fatalf("creating write = %+v, %v", out, err)
	}
	after := s.sheetMetrics().Batches
	if got := after["applied"] - before["applied"]; got != 1 {
		t.Errorf("the creating write added %d applied batches, want 1", got)
	}
}

// TestSheetRecalculationTimesAcrossAnUnload covers the recalculation times
// of a sheet that is unloaded and loaded again: the host keeps them, the
// reload adds one that no edit started, and a write adds one that an edit
// started.
func TestSheetRecalculationTimesAcrossAnUnload(t *testing.T) {
	s, _ := newTestService(t)
	offset := sheetClock(s)
	newSheet(t, s, "budget")
	writeSheet(t, s, "budget", "rows", sheetBudgetRows)
	counts := func() (edit, other uint64) {
		m := s.sheetMetrics().Recalc
		for _, start := range SheetRecalcStarts {
			var total uint64
			for _, n := range m[start].Counts {
				total += n
			}
			if len(m[start].Counts) != len(SheetRecalcBuckets)+1 || total != m[start].Count {
				t.Fatalf("recalculations started by %s = %+v", start, m[start])
			}
		}
		return m["edit"].Count, m["other"].Count
	}
	edit, other := counts()
	if edit != 1 || other < 1 {
		t.Fatalf("after a load and a write: %d recalculations by an edit, %d otherwise", edit, other)
	}
	unloadSheetNow(t, s, offset, "budget")
	if e, o := counts(); e != edit || o != other {
		t.Fatalf("the unload changed the recalculation counts: %d and %d, were %d and %d", e, o, edit, other)
	}
	writeSheet(t, s, "budget", "after", `{"op":"write","tab":"Sheet1","at":"B2","rows":[[1300]]}`)
	if e, o := counts(); e != edit+1 || o <= other {
		t.Fatalf("after the reload and a write: %d by an edit and %d otherwise, were %d and %d", e, o, edit, other)
	}
}

func TestSheetRecalculationDurationTotals(t *testing.T) {
	s, _ := newTestService(t)
	s.sheetStats.observeRecalc(true, time.Millisecond)
	s.sheetStats.observeRecalc(false, 0)
	s.sheetStats.observeRecalc(true, 2*time.Millisecond)
	s.sheetStats.observeRecalc(false, 25*time.Millisecond)

	recalc := s.Metrics().Sheet.Recalc
	for start, want := range map[string]time.Duration{
		"edit":  3 * time.Millisecond,
		"other": 25 * time.Millisecond,
	} {
		if got, ok := recalc[start]; !ok || got.Count != 2 || got.Sum != want.Seconds() {
			t.Errorf("recalculations started by %s = %+v, want 2 totaling %v", start, got, want)
		}
	}
}
