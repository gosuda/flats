package metrics

import (
	"context"
	"errors"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/gosuda/flats/internal/buildinfo"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/provider"
)

type fakeSource struct {
	mu    sync.Mutex
	snap  core.MetricsSnapshot
	calls atomic.Int64
}

func (f *fakeSource) Metrics() core.MetricsSnapshot {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

type fakeEndpoints struct {
	mu     sync.Mutex
	counts []provider.EndpointCount
	calls  atomic.Int64
}

func (f *fakeEndpoints) EndpointStates() []provider.EndpointCount {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.counts)
}

var (
	snapAt    = time.Unix(1_800_000_000, 0)
	memAt     = time.Unix(1_800_000_010, 500_000_000)
	diskAt    = time.Unix(1_800_000_020, 0)
	durations = []uint64{1, 2, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0, 1, 3} // 13 bounds + Inf
)

func testSnapshot() core.MetricsSnapshot {
	return core.MetricsSnapshot{
		SnapshotAt: snapAt, DBBytes: 4096, PendingApprovals: 2,
		Board: core.BoardMetrics{Batches: map[string]int64{"applied": 7, "rejected": 1, "retry": 3}, Connections: 4,
			ImportCount: 3, ImportSum: 0.75, ImportCounts: []uint64{0, 1, 0, 0, 0, 1, 0, 1, 0, 0, 0, 0}},
		Counts: []core.FlatCount{
			{Type: "server", Visibility: "public", Running: true, N: 1},
			{Type: "docs", Visibility: "private", Running: true, N: 1},
			{Type: "flat", Visibility: "private", Running: false, N: 3},
		},
		Flats: []core.FlatMetrics{
			{
				Slug: "shop", Traffic: true, Type: "server",
				Requests: map[string]map[string]uint64{
					core.RoutePublic:  {core.Class2xx: 10, core.Class5xx: 2, core.ClassClosed: 1},
					core.RoutePrivate: {core.Class4xx: 3},
				},
				RateLimited: 5, ResponseBytes: 12345, OpenStreams: 2,
				DurationCounts: durations, DurationSum: 42.5, DurationCount: 11,
				HasWorker: true, CPUSeconds: 1.25, MemoryOK: true, MemoryBytes: 52428800, MemorySampledAt: memAt,
				State: core.WorkerRunning, UnexpectedExits: 1, DiskBytes: 777, DiskMeasuredAt: diskAt,
			},
			{
				// A document without memory figures (not Linux), never measured on disk.
				Slug: "notes", Traffic: true, Type: "docs", HasWorker: true, CPUSeconds: 0.5, MemoryOK: false,
				MemoryBytes: 99, State: core.WorkerSleeping,
			},
			{Slug: "site", Traffic: true, Type: "flat", DiskBytes: 10, DiskMeasuredAt: diskAt},
			// Never labels: not a slug, and a duplicate.
			{Slug: "bad/slug.example:80", Traffic: true, Type: "flat"},
			{Slug: "shop", Traffic: true, Type: "server", RateLimited: 999},
			// A type that is not an enum word is reported as other.
			{Slug: "odd", Traffic: true, Type: "x/y:z"},
			// No live state (a rename in progress): disk figures only.
			{Slug: "moving", Type: "server", DiskBytes: 5, DiskMeasuredAt: diskAt, RateLimited: 9, HasWorker: true, CPUSeconds: 3},
		},
		RoleCPU: []core.RoleCPU{
			{Slug: "shop", Type: "server", Role: core.UsageRolePreview, CPUSeconds: 0.25},
			{Slug: "shop", Type: "server", Role: core.UsageRoleCheck, CPUSeconds: 0.75},
			{Slug: "shop", Type: "server", Role: core.UsageRolePreview, CPUSeconds: 0.25},
			{Slug: "shop", Type: "server", Role: core.UsageRoleLive, CPUSeconds: 100}, // live comes from Flats
			{Slug: "gone", Type: "server", Role: core.UsageRoleCheck, CPUSeconds: 2},
		},
	}
}

func newTest(t *testing.T) (*Metrics, *fakeSource, *fakeEndpoints) {
	t.Helper()
	src := &fakeSource{snap: testSnapshot()}
	eps := &fakeEndpoints{counts: []provider.EndpointCount{
		{Provider: provider.Local, State: "ready", N: 3},
		{Provider: provider.Tailscale, State: "starting", N: 1},
		{Provider: provider.Portal, State: "weird state", N: 2},
		{Provider: provider.Portal, State: "other", N: 1},
		{Provider: "https://not-a-provider", State: "ready", N: 7},
	}}
	m := New(Options{Source: src, Endpoints: eps, PerFlat: true})
	t.Cleanup(m.Close)
	return m, src, eps
}

func scrape(t *testing.T, h http.Handler, header http.Header) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "http://127.0.0.1:7878/metrics", nil)
	maps.Copy(req.Header, header)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// samples parses a text exposition into samples, failing on invalid output.
func samples(t *testing.T, body string) model.Vector {
	t.Helper()
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(strings.NewReader(body))
	if err != nil {
		t.Fatalf("exposition does not parse: %v\n%s", err, body)
	}
	vec, err := expfmt.ExtractSamples(&expfmt.DecodeOptions{Timestamp: model.Now()}, slices.Collect(maps.Values(fams))...)
	if err != nil {
		t.Fatal(err)
	}
	return vec
}

// value returns the sample of name with exactly labels (besides the name).
func value(vec model.Vector, name string, labels map[string]string) (float64, bool) {
	for _, s := range vec {
		if string(s.Metric[model.MetricNameLabel]) != name || len(s.Metric) != len(labels)+1 {
			continue
		}
		match := true
		for k, v := range labels {
			if string(s.Metric[model.LabelName(k)]) != v {
				match = false
				break
			}
		}
		if match {
			return float64(s.Value), true
		}
	}
	return 0, false
}

func series(vec model.Vector, name string) int {
	n := 0
	for _, s := range vec {
		if string(s.Metric[model.MetricNameLabel]) == name {
			n++
		}
	}
	return n
}

func TestExpositionValues(t *testing.T) {
	m, _, _ := newTest(t)
	m.RefreshEndpoints()
	code, body := scrape(t, m.Handler(), nil)
	if code != 200 {
		t.Fatalf("scrape = %d", code)
	}
	vec := samples(t, body)
	want := func(name string, labels map[string]string, v float64) {
		t.Helper()
		got, ok := value(vec, name, labels)
		if !ok {
			t.Errorf("missing %s%v", name, labels)
		} else if math.Abs(got-v) > 1e-9 {
			t.Errorf("%s%v = %v, want %v", name, labels, got, v)
		}
	}
	absent := func(name string, labels map[string]string) {
		t.Helper()
		if _, ok := value(vec, name, labels); ok {
			t.Errorf("unexpected %s%v", name, labels)
		}
	}
	shop := map[string]string{"flat": "shop", "type": "server"}
	with := func(base map[string]string, kv ...string) map[string]string {
		out := maps.Clone(base)
		for i := 0; i < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}

	bi := buildinfo.Get()
	want("flats_build_info", map[string]string{"version": bi.Version, "commit": bi.Commit}, 1)
	want("flats_snapshot_timestamp_seconds", nil, float64(snapAt.Unix()))
	want("flats_db_bytes", nil, 4096)
	want("flats_pending_approvals", nil, 2)
	want("flats_flats", map[string]string{"type": "server", "visibility": "public", "running": "true"}, 1)
	want("flats_flats", map[string]string{"type": "flat", "visibility": "private", "running": "false"}, 3)

	// Every route and class has a series, zero where nothing happened.
	want("flats_requests_total", with(shop, "route", "public", "code", "2xx"), 10)
	want("flats_requests_total", with(shop, "route", "public", "code", "5xx"), 2)
	want("flats_requests_total", with(shop, "route", "public", "code", "closed"), 1)
	want("flats_requests_total", with(shop, "route", "private", "code", "4xx"), 3)
	want("flats_requests_total", with(shop, "route", "private", "code", "1xx"), 0)
	if n := series(vec, "flats_requests_total"); n != 4*12 { // shop, notes, site, odd
		t.Errorf("flats_requests_total has %d series, want 48", n)
	}
	want("flats_rate_limited_total", shop, 5) // not the duplicate's 999
	want("flats_response_bytes_total", shop, 12345)
	want("flats_open_streams", shop, 2)

	// Histogram: per-bucket counts become cumulative buckets.
	cum := []float64{1, 3, 3, 3, 7, 7, 7, 7, 7, 7, 7, 7, 8}
	for i, bound := range core.UsageBuckets {
		want("flats_request_duration_seconds_bucket", with(shop, "le", strconv.FormatFloat(bound, 'g', -1, 64)), cum[i])
	}
	want("flats_request_duration_seconds_bucket", with(shop, "le", "+Inf"), 11)
	want("flats_request_duration_seconds_count", shop, 11)
	want("flats_request_duration_seconds_sum", shop, 42.5)

	want("flats_worker_cpu_seconds_total", with(shop, "role", "live"), 1.25)
	want("flats_worker_cpu_seconds_total", with(shop, "role", "preview"), 0.5)
	want("flats_worker_cpu_seconds_total", with(shop, "role", "check"), 0.75)
	want("flats_worker_cpu_seconds_total", map[string]string{"flat": "gone", "type": "server", "role": "check"}, 2)
	want("flats_worker_memory_bytes", shop, 52428800)
	want("flats_worker_memory_sampled_timestamp_seconds", shop, 1_800_000_010.5)
	want("flats_worker_state", with(shop, "state", "running"), 1)
	want("flats_worker_unexpected_exits_total", shop, 1)
	want("flats_disk_bytes", shop, 777)
	want("flats_disk_measured_timestamp_seconds", shop, float64(diskAt.Unix()))

	notes := map[string]string{"flat": "notes", "type": "docs"}
	want("flats_worker_cpu_seconds_total", with(notes, "role", "live"), 0.5)
	want("flats_worker_state", with(notes, "state", "sleeping"), 1)
	absent("flats_worker_memory_bytes", notes)
	absent("flats_worker_memory_sampled_timestamp_seconds", notes)
	absent("flats_disk_bytes", notes)
	absent("flats_disk_measured_timestamp_seconds", notes)

	site := map[string]string{"flat": "site", "type": "flat"}
	want("flats_disk_bytes", site, 10)
	absent("flats_worker_cpu_seconds_total", with(site, "role", "live"))
	absent("flats_worker_state", with(site, "state", "running"))
	absent("flats_worker_unexpected_exits_total", site)
	want("flats_open_streams", map[string]string{"flat": "odd", "type": "other"}, 0)

	want("flats_route_endpoints", map[string]string{"provider": "local", "state": "ready"}, 3)
	want("flats_route_endpoints", map[string]string{"provider": "tailscale", "state": "starting"}, 1)
	want("flats_route_endpoints", map[string]string{"provider": "portal", "state": "other"}, 3)
	if n := series(vec, "flats_route_endpoints"); n != 3 {
		t.Errorf("flats_route_endpoints has %d series, want 3", n)
	}
	want("flats_management_requests_total", map[string]string{"surface": "api", "code": "2xx"}, 0)
	want("flats_board_batches_total", map[string]string{"result": "applied"}, 7)
	want("flats_board_batches_total", map[string]string{"result": "retry"}, 3)
	want("flats_board_connections", nil, 4)
	want("flats_board_import_seconds_bucket", map[string]string{"le": "0.01"}, 1)
	want("flats_board_import_seconds_bucket", map[string]string{"le": "0.25"}, 2)
	want("flats_board_import_seconds_bucket", map[string]string{"le": "+Inf"}, 3)
	want("flats_board_import_seconds_sum", nil, 0.75)

	if series(vec, "go_goroutines") != 1 {
		t.Error("missing the Go runtime collector")
	}
}

// Library metrics registered on the default registerer are not exposed, and
// no label carries a path, host or URL.
func TestOnlyFlatsSeriesAndBoundedLabels(t *testing.T) {
	dummy := prometheus.NewCounter(prometheus.CounterOpts{Name: "library_dummy_total", Help: "registered by a library"})
	prometheus.MustRegister(dummy)
	t.Cleanup(func() { prometheus.Unregister(dummy) })
	dummy.Inc()

	m, _, _ := newTest(t)
	m.RefreshEndpoints()
	_, body := scrape(t, m.Handler(), nil)
	vec := samples(t, body)
	for _, s := range vec {
		name := string(s.Metric[model.MetricNameLabel])
		switch {
		case strings.HasPrefix(name, "flats_"):
		case strings.HasPrefix(name, "go_"), strings.HasPrefix(name, "process_"):
			continue
		default:
			t.Errorf("foreign series %s", s.Metric)
			continue
		}
		for k, v := range s.Metric {
			if k == model.MetricNameLabel {
				continue
			}
			val := string(v)
			bad := "/:?#@ \\\"'"
			if (k != "version" || name != "flats_build_info") && k != "le" {
				bad += "."
			}
			if strings.ContainsAny(val, bad) || len(val) > 64 {
				t.Errorf("label %s=%q of %s is not a bounded enum, slug or build string", k, val, name)
			}
		}
	}
	if strings.Contains(body, "library_dummy_total") {
		t.Error("the default registerer's metrics are exposed")
	}
}

// A scrape reads the cached endpoint states; it never calls the provider
// manager, which may call network backends.
func TestScrapeDoesNotCallEndpointSource(t *testing.T) {
	m, src, eps := newTest(t)
	m.RefreshEndpoints()
	if eps.calls.Load() != 1 {
		t.Fatalf("refresh calls = %d", eps.calls.Load())
	}
	for range 3 {
		scrape(t, m.Handler(), nil)
	}
	if eps.calls.Load() != 1 {
		t.Fatalf("scrapes called EndpointStates: %d calls", eps.calls.Load())
	}
	if src.calls.Load() != 3 {
		t.Fatalf("scrapes read the core snapshot %d times, want 3", src.calls.Load())
	}
}

func TestEndpointRefreshLoop(t *testing.T) {
	src := &fakeSource{}
	eps := &fakeEndpoints{counts: []provider.EndpointCount{{Provider: provider.Local, State: "starting", N: 1}}}
	m := New(Options{Source: src, Endpoints: eps, EndpointInterval: 10 * time.Millisecond})
	m.Start()
	m.Start() // idempotent
	waitFor := func(ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatal("timed out")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	read := func() (float64, bool) {
		_, body := scrape(t, m.Handler(), nil)
		return value(samples(t, body), "flats_route_endpoints", map[string]string{"provider": "local", "state": "ready"})
	}
	waitFor(func() bool {
		_, ok := value(samplesOf(t, m), "flats_route_endpoints", map[string]string{"provider": "local", "state": "starting"})
		return ok
	})
	eps.mu.Lock()
	eps.counts = []provider.EndpointCount{{Provider: provider.Local, State: "ready", N: 1}}
	eps.mu.Unlock()
	waitFor(func() bool { v, ok := read(); return ok && v == 1 })
	m.Close()
	m.Close()
	n := eps.calls.Load()
	time.Sleep(30 * time.Millisecond)
	if eps.calls.Load() != n {
		t.Fatal("refresh loop kept running after Close")
	}
	// The host-snapshot series wait for the first sweep.
	vec := samplesOf(t, m)
	for _, name := range []string{"flats_snapshot_timestamp_seconds", "flats_db_bytes", "flats_pending_approvals"} {
		if series(vec, name) != 0 {
			t.Errorf("%s before the first sweep", name)
		}
	}
}

func samplesOf(t *testing.T, m *Metrics) model.Vector {
	t.Helper()
	_, body := scrape(t, m.Handler(), nil)
	return samples(t, body)
}

func TestHandlerGuards(t *testing.T) {
	m := New(Options{})
	h := m.Handler()
	for _, tc := range []struct {
		name   string
		method string
		header map[string]string
		want   int
	}{
		{"plain", "GET", nil, 200},
		{"head", "HEAD", nil, 200},
		{"same-origin browser", "GET", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:7878"}, 200},
		{"direct navigation", "GET", map[string]string{"Sec-Fetch-Site": "none"}, 200},
		{"cross-site", "GET", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"same-site", "GET", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"foreign origin", "GET", map[string]string{"Origin": "http://attacker.example"}, 403},
		{"other port", "GET", map[string]string{"Origin": "http://127.0.0.1:7879"}, 403},
		{"post", "POST", nil, 405},
		{"delete", "DELETE", nil, 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://127.0.0.1:7878/metrics", nil)
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s = %d, want %d", tc.name, rec.Code, tc.want)
			}
			if tc.want == 405 && rec.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("Allow = %q", rec.Header().Get("Allow"))
			}
			if tc.want == 200 && tc.method == "GET" && !strings.Contains(rec.Body.String(), "flats_build_info") {
				t.Error("no exposition")
			}
		})
	}
}

// The OpenMetrics format is negotiated when a scraper asks for it.
func TestOpenMetricsNegotiation(t *testing.T) {
	m := New(Options{})
	_, _ = scrape(t, m.Handler(), nil)
	req := httptest.NewRequest("GET", "http://127.0.0.1:7878/metrics", nil)
	req.Header.Set("Accept", "application/openmetrics-text;version=1.0.0")
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/openmetrics-text") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.HasSuffix(strings.TrimSpace(rec.Body.String()), "# EOF") {
		t.Fatal("OpenMetrics output does not end with # EOF")
	}
}

// countingTail is a tailnet backend that counts Status calls.
type countingTail struct {
	mu       sync.Mutex
	state    string
	statuses int
}

func (c *countingTail) Serve(context.Context, string, http.Handler, bool) (string, error) {
	return "https://shop.example.ts.net", nil
}
func (c *countingTail) Stop(string) error       { return nil }
func (c *countingTail) URL(string) string       { return "https://shop.example.ts.net" }
func (c *countingTail) Close() error            { return nil }
func (c *countingTail) StopFunnel(string) error { return nil }
func (c *countingTail) ServeFunnel(context.Context, string, http.Handler) (string, error) {
	return "", errors.New("no funnel")
}
func (c *countingTail) FunnelState(string) provider.ExposureEndpoint {
	return provider.ExposureEndpoint{Provider: provider.Funnel, State: "unavailable"}
}
func (c *countingTail) Status() core.NetStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statuses++
	return core.NetStatus{Hosts: []core.HostInfo{{Host: "shop", State: c.state}}}
}
func (c *countingTail) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statuses
}

// With the real provider manager, endpoint states come from the refresh, and
// a scrape never calls a backend's Status.
func TestScrapeDoesNotCallBackendStatus(t *testing.T) {
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tail := &countingTail{state: "starting"}
	mgr, err := provider.New(t.TempDir(), provider.Options{Local: ln, Tailscale: tail,
		Grants: &provider.File{Permitted: []provider.ID{provider.Tailscale}}})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if _, err := mgr.ServeExposure(context.Background(), provider.ExposureRequest{Slug: "shop", Visibility: "private",
		Audience: core.AudienceCurrent, Handler: http.NotFoundHandler(), Permitted: []provider.ID{provider.Tailscale}}); err != nil {
		t.Fatal(err)
	}
	m := New(Options{Endpoints: mgr})
	ts := map[string]string{"provider": "tailscale", "state": "starting"}
	m.RefreshEndpoints()
	refreshed := tail.calls()
	for range 3 {
		if v, ok := value(samplesOf(t, m), "flats_route_endpoints", ts); !ok || v != 1 {
			t.Fatalf("tailscale starting = %v %v", v, ok)
		}
	}
	if tail.calls() != refreshed {
		t.Fatalf("scrapes called the backend's Status: %d calls after refresh, %d now", refreshed, tail.calls())
	}
	tail.mu.Lock()
	tail.state = "ready"
	tail.mu.Unlock()
	if _, ok := value(samplesOf(t, m), "flats_route_endpoints", map[string]string{"provider": "tailscale", "state": "ready"}); ok {
		t.Fatal("a scrape refreshed the backend state")
	}
	m.RefreshEndpoints()
	if v, ok := value(samplesOf(t, m), "flats_route_endpoints", map[string]string{"provider": "tailscale", "state": "ready"}); !ok || v != 1 {
		t.Fatalf("after the refresh, tailscale ready = %v %v", v, ok)
	}
}

// A flat without live state (a rename in progress) keeps its disk figures but
// exposes no traffic or worker series, so its counters never appear to reset.
func TestFlatWithoutTrafficShowsDiskOnly(t *testing.T) {
	m, _, _ := newTest(t)
	_, body := scrape(t, m.Handler(), nil)
	vec := samples(t, body)
	if v, ok := value(vec, "flats_disk_bytes", map[string]string{"flat": "moving", "type": "server"}); !ok || v != 5 {
		t.Fatalf("disk of moving = %v, %v", v, ok)
	}
	for _, s := range vec {
		name := string(s.Metric[model.MetricNameLabel])
		if string(s.Metric["flat"]) == "moving" && !strings.HasPrefix(name, "flats_disk_") {
			t.Errorf("unexpected series %s for a flat without live state", s.Metric)
		}
	}
}

func TestPerFlatMetricsOptIn(t *testing.T) {
	perFlatFamilies := []string{
		"flats_requests_total", "flats_rate_limited_total", "flats_response_bytes_total",
		"flats_open_streams", "flats_request_duration_seconds", "flats_worker_cpu_seconds_total",
		"flats_worker_memory_bytes", "flats_worker_memory_sampled_timestamp_seconds",
		"flats_worker_state", "flats_worker_unexpected_exits_total", "flats_disk_bytes",
		"flats_disk_measured_timestamp_seconds",
	}
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			src := &fakeSource{snap: testSnapshot()}
			eps := &fakeEndpoints{counts: []provider.EndpointCount{{Provider: provider.Local, State: "ready", N: 2}}}
			m := New(Options{Source: src, Endpoints: eps, PerFlat: enabled})
			defer m.Close()
			m.RefreshEndpoints()
			families, err := m.Registry().Gather()
			if err != nil {
				t.Fatal(err)
			}
			present := map[string]bool{}
			for _, family := range families {
				present[family.GetName()] = true
				if !enabled {
					for _, metric := range family.Metric {
						for _, label := range metric.Label {
							if label.GetName() == "flat" {
								t.Errorf("flat label in %s", family.GetName())
							}
						}
					}
				}
			}
			for _, name := range perFlatFamilies {
				if present[name] != enabled {
					t.Errorf("%s present=%v, per_flat=%v", name, present[name], enabled)
				}
			}
			for _, name := range []string{"flats_build_info", "go_goroutines", "process_cpu_seconds_total", "flats_db_bytes", "flats_flats", "flats_pending_approvals", "flats_snapshot_timestamp_seconds", "flats_route_endpoints", "flats_management_requests_total", "flats_board_batches_total", "flats_board_connections", "flats_board_import_seconds"} {
				if !present[name] {
					t.Errorf("host or aggregate family %s omitted", name)
				}
			}
		})
	}
}
