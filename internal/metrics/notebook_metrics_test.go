package metrics

import (
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/common/model"

	"github.com/gosuda/flats/internal/core"
)

func TestNotebookMetrics(t *testing.T) {
	for _, perFlat := range []bool{false, true} {
		t.Run(strconv.FormatBool(perFlat), func(t *testing.T) {
			snap := testSnapshot()
			// Unknown results must not turn page names or ids into labels.
			snap.Notebook.Batches["page title"] = 99
			snap.Counts = append(snap.Counts, core.FlatCount{Type: "notebook", Visibility: "private", Running: true, N: 1})
			snap.Flats = append(snap.Flats, core.FlatMetrics{Slug: "wiki", Type: "notebook", Traffic: true, DiskBytes: 42, DiskMeasuredAt: diskAt})
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
			for result, count := range map[string]float64{"applied": 11, "rejected": 2, "retry": 5} {
				want("flats_notebook_batches_total", map[string]string{"result": result}, count)
			}
			if got := series(vec, "flats_notebook_batches_total"); got != 3 {
				t.Errorf("notebook result series = %d, want 3", got)
			}
			want("flats_notebook_connections", nil, 6)
			for i, bound := range core.NotebookImportBuckets {
				count := float64(2)
				if i == 0 {
					count = 1
				} else if bound >= 0.25 {
					count = 3
				}
				want("flats_notebook_import_seconds_bucket", map[string]string{"le": strconv.FormatFloat(bound, 'g', -1, 64)}, count)
			}
			want("flats_notebook_import_seconds_bucket", map[string]string{"le": "+Inf"}, 4)
			want("flats_notebook_import_seconds_count", nil, 4)
			want("flats_notebook_import_seconds_sum", nil, 12.765)
			want("flats_board_batches_total", map[string]string{"result": "applied"}, 7)
			want("flats_board_connections", nil, 4)
			want("flats_board_import_seconds_count", nil, 3)
			want("flats_flats", map[string]string{"type": "notebook", "visibility": "private", "running": "true"}, 1)
			if perFlat {
				want("flats_disk_bytes", map[string]string{"flat": "wiki", "type": "notebook"}, 42)
				want("flats_requests_total", map[string]string{"flat": "wiki", "type": "notebook", "route": "private", "code": "2xx"}, 0)
			}
			for _, sample := range vec {
				name := string(sample.Metric[model.MetricNameLabel])
				if string(sample.Metric["flat"]) == "wiki" && (!perFlat || strings.HasPrefix(name, "flats_worker_")) {
					t.Errorf("unexpected notebook per-flat or worker series: %s", sample.Metric)
				}
				if strings.HasPrefix(name, "flats_notebook_") {
					for key := range sample.Metric {
						if key != model.MetricNameLabel && key != "result" && key != "le" {
							t.Errorf("unbounded notebook label: %s", sample.Metric)
						}
					}
				}
			}
		})
	}
}

func TestNotebookMetricsEmpty(t *testing.T) {
	m := New(Options{Source: &fakeSource{}})
	defer m.Close()
	_, body := scrape(t, m.Handler(), nil)
	vec := samples(t, body)
	for _, name := range []string{"flats_notebook_connections", "flats_notebook_import_seconds_count", "flats_notebook_import_seconds_sum"} {
		if got, ok := value(vec, name, nil); !ok || got != 0 {
			t.Errorf("%s = %v, %v; want zero", name, got, ok)
		}
	}
	for _, result := range []string{"applied", "rejected", "retry"} {
		if got, ok := value(vec, "flats_notebook_batches_total", map[string]string{"result": result}); !ok || got != 0 {
			t.Errorf("%s = %v, %v; want zero", result, got, ok)
		}
	}
	for _, bound := range append(append([]float64{}, core.NotebookImportBuckets...), 0) {
		le := "+Inf"
		if bound != 0 {
			le = strconv.FormatFloat(bound, 'g', -1, 64)
		}
		if got, ok := value(vec, "flats_notebook_import_seconds_bucket", map[string]string{"le": le}); !ok || got != 0 {
			t.Errorf("bucket %s = %v, %v; want zero", le, got, ok)
		}
	}
}
