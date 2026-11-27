package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/store"
)

func TestSheetMetrics(t *testing.T) {
	for _, perFlat := range []bool{false, true} {
		t.Run(strconv.FormatBool(perFlat), func(t *testing.T) {
			snap := testSnapshot()
			// Unknown results must not turn tab names or addresses into labels.
			snap.Sheet.Batches["Budget!B2"] = 99
			snap.Counts = append(snap.Counts, core.FlatCount{Type: "sheet", Visibility: "private", Running: true, N: 1})
			snap.Flats = append(snap.Flats, core.FlatMetrics{Slug: "budget", Type: "sheet", Traffic: true, DiskBytes: 42, DiskMeasuredAt: diskAt})
			m := New(Options{Source: &fakeSource{snap: snap}, PerFlat: perFlat})
			defer m.Close()
			code, body := scrape(t, m.Handler(), nil)
			if code != 200 {
				t.Fatalf("scrape = %d", code)
			}
			vec := samples(t, body)
			want := func(name string, labels map[string]string, expected float64) {
				t.Helper()
				if got, ok := value(vec, name, labels); !ok || got != expected {
					t.Errorf("%s%v = %v, %v; want %v", name, labels, got, ok, expected)
				}
			}
			for result, count := range map[string]float64{"applied": 13, "rejected": 4} {
				want("flats_sheet_batches_total", map[string]string{"result": result}, count)
			}
			if got := series(vec, "flats_sheet_batches_total"); got != 2 {
				t.Errorf("sheet result series = %d, want 2", got)
			}
			want("flats_sheet_connections", nil, 8)
			for _, bound := range core.SheetImportBuckets {
				count := float64(2)
				if bound >= 0.25 {
					count = 3
				}
				want("flats_sheet_import_seconds_bucket", map[string]string{"le": strconv.FormatFloat(bound, 'g', -1, 64)}, count)
			}
			want("flats_sheet_import_seconds_bucket", map[string]string{"le": "+Inf"}, 5)
			want("flats_sheet_import_seconds_count", nil, 5)
			want("flats_sheet_import_seconds_sum", nil, 1.5)
			for start, counts := range map[string]struct{ low, mid, top, all, sum float64 }{
				"edit": {3, 5, 6, 6, 0.25}, "other": {0, 0, 1, 2, 40.5},
			} {
				labels := func(le string) map[string]string { return map[string]string{"start": start, "le": le} }
				want("flats_sheet_recalculation_seconds_bucket", labels("0.001"), counts.low)
				want("flats_sheet_recalculation_seconds_bucket", labels("0.005"), counts.mid)
				want("flats_sheet_recalculation_seconds_bucket", labels("30"), counts.top)
				want("flats_sheet_recalculation_seconds_bucket", labels("+Inf"), counts.all)
				want("flats_sheet_recalculation_seconds_count", map[string]string{"start": start}, counts.all)
				want("flats_sheet_recalculation_seconds_sum", map[string]string{"start": start}, counts.sum)
			}
			if got, bounds := series(vec, "flats_sheet_recalculation_seconds_bucket"), len(core.SheetRecalcBuckets)+1; got != 2*bounds {
				t.Errorf("recalculation bucket series = %d, want %d for each of the two starts", got, bounds)
			}
			want("flats_notebook_connections", nil, 6)
			want("flats_flats", map[string]string{"type": "sheet", "visibility": "private", "running": "true"}, 1)
			if perFlat {
				want("flats_disk_bytes", map[string]string{"flat": "budget", "type": "sheet"}, 42)
			}
			for _, sample := range vec {
				name := string(sample.Metric[model.MetricNameLabel])
				if string(sample.Metric["flat"]) == "budget" && (!perFlat || strings.HasPrefix(name, "flats_worker_")) {
					t.Errorf("unexpected sheet per-flat or worker series: %s", sample.Metric)
				}
				if strings.HasPrefix(name, "flats_sheet_") {
					for key := range sample.Metric {
						if key != model.MetricNameLabel && key != "result" && key != "le" && key != "start" {
							t.Errorf("unbounded sheet label: %s", sample.Metric)
						}
					}
				}
			}
		})
	}
}

func TestSheetMetricsEmpty(t *testing.T) {
	m := New(Options{Source: &fakeSource{}})
	defer m.Close()
	_, body := scrape(t, m.Handler(), nil)
	vec := samples(t, body)
	for _, name := range []string{"flats_sheet_connections", "flats_sheet_import_seconds_count", "flats_sheet_import_seconds_sum"} {
		if got, ok := value(vec, name, nil); !ok || got != 0 {
			t.Errorf("%s = %v, %v; want zero", name, got, ok)
		}
	}
	for _, result := range []string{"applied", "rejected"} {
		if got, ok := value(vec, "flats_sheet_batches_total", map[string]string{"result": result}); !ok || got != 0 {
			t.Errorf("%s = %v, %v; want zero", result, got, ok)
		}
	}
	for _, start := range core.SheetRecalcStarts {
		if got, ok := value(vec, "flats_sheet_recalculation_seconds_count", map[string]string{"start": start}); !ok || got != 0 {
			t.Errorf("recalculations started by %s = %v, %v; want zero", start, got, ok)
		}
	}
}

// TestSheetRecalculationTimesOfAHost scrapes the collector of a running
// host: loading a sheet and editing it each record one recalculation under
// their start.
func TestSheetRecalculationTimesOfAHost(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := core.New(context.Background(), core.Config{DataDir: filepath.Join(dir, "data"), Store: st, Private: priv, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close(); priv.Close(); st.Close() })
	m := New(Options{Source: svc})
	defer m.Close()
	counts := func() (edit, other float64) {
		t.Helper()
		vec := samplesOf(t, m)
		for start, dst := range map[string]*float64{"edit": &edit, "other": &other} {
			n, ok := value(vec, "flats_sheet_recalculation_seconds_count", map[string]string{"start": start})
			inf, hasInf := value(vec, "flats_sheet_recalculation_seconds_bucket", map[string]string{"start": start, "le": "+Inf"})
			if !ok || !hasInf || n != inf {
				t.Fatalf("recalculations started by %s: count %v, %v; +Inf bucket %v, %v", start, n, ok, inf, hasInf)
			}
			*dst = n
		}
		return edit, other
	}
	if edit, other := counts(); edit != 0 || other != 0 {
		t.Fatalf("recalculations before any sheet = %v by an edit, %v otherwise", edit, other)
	}
	if _, err := svc.CreateSheet(t.Context(), "budget", "Budget", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	var state struct {
		State string `json:"state"`
	}
	read := func() {
		t.Helper()
		// A read waits for the load, which calculates the sheet.
		deadline := time.Now().Add(20 * time.Second)
		for {
			raw, err := svc.GetSheet(t.Context(), "budget", 0, nil)
			if err == nil {
				if err := json.Unmarshal(raw, &state); err != nil {
					t.Fatal(err)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	read()
	edit, loaded := counts()
	if edit != 0 || loaded < 1 {
		t.Fatalf("after the load: %v recalculations by an edit, %v otherwise", edit, loaded)
	}
	body := fmt.Sprintf(`{"state":%q,"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1200,900,"=SUM(A1:B1)"]]}]}`, state.State)
	if _, err := svc.UpdateSheet(t.Context(), "budget", []byte(body), core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	edit, other := counts()
	// A change of the date while the test runs adds one that no edit started.
	if edit != 1 || other < loaded {
		t.Fatalf("after one agent write: %v recalculations by an edit and %v otherwise, want 1 and at least %v", edit, other, loaded)
	}
	if sum, ok := value(samplesOf(t, m), "flats_sheet_recalculation_seconds_sum", map[string]string{"start": "edit"}); !ok || sum < 0 || math.IsNaN(sum) || math.IsInf(sum, 0) {
		t.Fatalf("time of the edit's recalculation = %v, %v", sum, ok)
	}
}
