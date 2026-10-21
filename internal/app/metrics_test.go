package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/metrics"
)

// /metrics answers on the loopback management server with the guards of
// /api, and the management requests are counted outside the host guard.
func TestManagementServerServesMetrics(t *testing.T) {
	h := startLocal(t)
	_, port, _ := strings.Cut(h.Addr(), ":")
	do := func(method, host, path string, header map[string]string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://"+h.Addr()+path, nil)
		req.Host = host
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	local := "localhost:" + port
	code, body := do("GET", local, "/metrics", nil)
	if code != 200 || !strings.Contains(body, "flats_build_info") || strings.Contains(body, "<html") {
		t.Fatalf("GET /metrics = %d %.200s", code, body)
	}
	if code, _ := do("POST", local, "/metrics", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics = %d", code)
	}
	if code, _ := do("GET", local, "/metrics", map[string]string{"Sec-Fetch-Site": "cross-site"}); code != 403 {
		t.Errorf("cross-site /metrics = %d", code)
	}
	if code, _ := do("GET", local, "/metrics", map[string]string{"Origin": "http://127.0.0.1:" + port}); code != 403 {
		t.Errorf("/metrics with another loopback Origin = %d", code)
	}
	if code, _ := do("GET", "attacker.example:"+port, "/metrics", nil); code != 403 {
		t.Errorf("rebound Host /metrics = %d", code)
	}
	if code, _ := do("GET", local, "/", nil); code != 200 {
		t.Errorf("console page = %d", code)
	}
	if code, _ := do("GET", "attacker.example:"+port, "/api/flats", nil); code != 403 {
		t.Errorf("rebound Host /api = %d", code)
	}
	m := h.metrics
	for _, tc := range []struct {
		surface, class string
		want           uint64
	}{
		{"metrics", "2xx", 1}, {"metrics", "4xx", 4}, {"other", "2xx", 1}, {"api", "4xx", 1},
	} {
		if got := m.ManagementCount(tc.surface, tc.class); got != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.surface, tc.class, got, tc.want)
		}
	}
	_, body = do("GET", local, "/metrics", nil)
	if !strings.Contains(body, `flats_management_requests_total{code="4xx",surface="metrics"} 4`) {
		t.Errorf("management counts are not exposed:\n%s", grepLines(body, "flats_management_requests_total"))
	}
}

func grepLines(body, prefix string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// Console metrics are hidden by default, and both hidden and invalid requests count.
func TestConsoleDomainHidesAndCountsMetrics(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	if _, err := h.AddConsoleDomain(context.Background(), "tailscale", "admin", ""); err != nil {
		t.Fatal(err)
	}
	tail.mu.Lock()
	handler := tail.served["admin"]
	tail.mu.Unlock()
	before := h.metrics.ManagementCount("metrics", "4xx")
	for host, want := range map[string]int{"admin.tail1234.ts.net": 404, "other.tail1234.ts.net": 403} {
		req, _ := http.NewRequest("GET", "http://"+host+"/metrics", nil)
		rec := &recorder{header: http.Header{}}
		handler.ServeHTTP(rec, req)
		if rec.code != want {
			t.Errorf("Host %s: %d, want %d", host, rec.code, want)
		}
	}
	if got := h.metrics.ManagementCount("metrics", "4xx") - before; got != 2 {
		t.Errorf("refused console-domain requests counted %d, want 2", got)
	}
}

// metricsHost configures the real startup path, including sparse defaults.
func metricsHost(t *testing.T, settings map[string]string) (*Host, *http.Client) {
	t.Helper()
	dir := t.TempDir()
	doc, err := config.New(config.NewInstanceID(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set("host.server_runtime", "false"); err != nil {
		t.Fatal(err)
	}
	for key, value := range settings {
		if err := doc.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, doc.Encode(), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := Start(context.Background(), Options{ConfigPath: path, Overrides: map[string]string{
		"host.console_addr": "127.0.0.1:0", "host.local_addr": "127.0.0.1:0",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	return h, &http.Client{Transport: transport}
}

func TestMetricsExposureSelection(t *testing.T) {
	accessPoints := []string{"localhost", "console", "external"}
	for mask := 0; mask < 8; mask++ {
		selected := []string{}
		for i, access := range accessPoints {
			if mask&(1<<i) != 0 {
				selected = append(selected, access)
			}
		}
		for _, enabled := range []bool{false, true} {
			t.Run(strconv.Itoa(mask)+"/"+strconv.FormatBool(enabled), func(t *testing.T) {
				h, client := metricsHost(t, map[string]string{
					"metrics.enabled": strconv.FormatBool(enabled), "metrics.expose_on": strings.Join(selected, ","),
				})
				if _, err := h.Svc.UpdateSettings(context.Background(), map[string]string{core.SetExternalOrigins: "https://metrics.example.org"}); err != nil {
					t.Fatal(err)
				}
				target, _ := url.Parse("http://" + h.Addr())
				proxy := httptest.NewServer(&httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
					r.SetURL(target)
					r.Out.Host = "metrics.example.org"
					r.Out.Header.Set("X-Forwarded-Host", "localhost")
				}})
				defer proxy.Close()
				tail := newFakeTailnet()
				h.pmu.Lock()
				h.testTailnet = tail
				h.pmu.Unlock()
				if _, err := h.Svc.SetPermittedNetworks([]string{"tailscale"}, ""); err != nil {
					t.Fatal(err)
				}
				if _, err := h.AddConsoleDomain(context.Background(), "tailscale", "admin", ""); err != nil {
					t.Fatal(err)
				}
				tail.mu.Lock()
				console := tail.served["admin"]
				tail.mu.Unlock()
				for _, access := range accessPoints {
					for _, method := range []string{"GET", "HEAD", "POST"} {
						req := httptest.NewRequest(method, "http://admin.tail1234.ts.net/metrics", nil)
						var response *http.Response
						if access == "console" {
							rec := httptest.NewRecorder()
							console.ServeHTTP(rec, req)
							response = rec.Result()
						} else {
							base := "http://" + h.Addr()
							if access == "external" {
								base = proxy.URL
							}
							req, _ = http.NewRequest(method, base+"/metrics", nil)
							var err error
							response, err = client.Do(req)
							if err != nil {
								t.Fatal(err)
							}
						}
						body, _ := io.ReadAll(response.Body)
						response.Body.Close()
						want := http.StatusNotFound
						if enabled && slices.Contains(selected, access) {
							want = http.StatusOK
							if method == "POST" {
								want = http.StatusMethodNotAllowed
							}
						}
						if response.StatusCode != want {
							t.Errorf("%s %s=%d, want%d", access, method, response.StatusCode, want)
						}
						if want == http.StatusMethodNotAllowed && response.Header.Get("Allow") != "GET, HEAD" {
							t.Errorf("Allow missing: %s", access)
						}
						if want == http.StatusNotFound && response.Header.Get("Allow") != "" {
							t.Errorf("hidden %s has Allow header", access)
						}
						if method == "GET" && want == 200 && !strings.Contains(string(body), "flats_build_info") {
							t.Errorf("%s has no metrics", access)
						}
						if method == "HEAD" && len(body) != 0 && access != "console" {
							t.Errorf("HEAD %s has body", access)
						}
					}
				}
			})
		}
	}
}

func TestMetricsHiddenGuards(t *testing.T) {
	for _, settings := range []map[string]string{{"metrics.enabled": "false"}, {"metrics.expose_on": ""}} {
		h, client := metricsHost(t, settings)
		for _, tc := range []struct {
			method, host, origin, fetch string
			want                        int
		}{
			{"GET", "attacker.example", "", "", 403},
			{"POST", "attacker.example", "", "", 403},
			{"GET", h.Addr(), "http://attacker.example", "", 403},
			{"POST", h.Addr(), "http://attacker.example", "", 403},
			{"GET", h.Addr(), "", "cross-site", 403},
			{"HEAD", h.Addr(), "", "same-site", 403},
			{"POST", h.Addr(), "", "cross-site", 404},
		} {
			req, _ := http.NewRequest(tc.method, "http://"+h.Addr()+"/metrics", nil)
			req.Host = tc.host
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.fetch)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("%+v got%d", tc, resp.StatusCode)
			}
		}
	}
}

func TestMetricsControlsKeepAnalyticsUsage(t *testing.T) {
	for _, tc := range []struct {
		name        string
		enabled     bool
		explicitOff bool
	}{
		{name: "enabled default per-flat", enabled: true},
		{name: "disabled default per-flat"},
		{name: "enabled per-flat off", enabled: true, explicitOff: true},
		{name: "disabled per-flat off", explicitOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := map[string]string{"host.server_runtime": "true", "metrics.enabled": strconv.FormatBool(tc.enabled)}
			if tc.explicitOff {
				settings["metrics.per_flat"] = "false"
			}
			h, client := metricsHost(t, settings)
			if h.Config.Metrics.PerFlat != !tc.explicitOff {
				t.Fatalf("loaded per_flat = %v, want %v", h.Config.Metrics.PerFlat, !tc.explicitOff)
			}
			h.workers.Env = append([]string{"FLATS_CONTENT_TEST_WORKER=1"}, testImageEnv()...)
			h.workers.NoSpare = true
			h.workers.UsageInterval = 10 * time.Millisecond
			ctx := context.Background()
			_, err := h.Svc.SaveVersion(ctx, "analytics", []bundle.File{
				{Path: "flats.json", Data: []byte(`{"kind":"server","entry":"server.js"}`)},
				{Path: "server.js", Data: []byte(`export default { fetch() { let n=0; for(let i=0;i<100000;i++) n+=i; return new Response("usage:"+n); } };`)},
			}, core.SaveMeta{}, core.ViaAPI)
			if err != nil {
				t.Fatal(err)
			}
			published, err := h.Svc.Publish(ctx, "analytics", 0, "", core.ViaAPI)
			if err != nil || published.Approval == nil {
				t.Fatalf("publish: %+v %v", published, err)
			}
			if _, err := h.Svc.Decide(ctx, published.Approval.ID, true); err != nil {
				t.Fatal(err)
			}
			flat, err := h.Svc.GetFlat(ctx, "analytics")
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.Svc.Deploy(ctx, "analytics", flat.LatestVersion, core.ViaAPI)
			var pending *core.PendingApproval
			if !errors.As(err, &pending) || pending.Approval == nil {
				t.Fatalf("deploy: %v", err)
			}
			if _, err := h.Svc.Decide(ctx, pending.Approval.ID, true); err != nil {
				t.Fatal(err)
			}
			flat, err = h.Svc.GetFlat(ctx, "analytics")
			if err != nil {
				t.Fatal(err)
			}
			if code, body := fetch(t, flat.PrivateURL); code != 200 || !strings.HasPrefix(body, "usage:") {
				t.Fatalf("traffic: %d %s", code, body)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				usage, err := h.Svc.Usage(ctx, "analytics", 7)
				if err != nil {
					t.Fatal(err)
				}
				if usage.Totals.CPUSeconds != nil && *usage.Totals.CPUSeconds > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("worker CPU never sampled: %+v", usage)
				}
				time.Sleep(10 * time.Millisecond)
			}
			for _, prefix := range []string{"/api", "/console/api"} {
				req, _ := http.NewRequest("GET", "http://"+h.Addr()+prefix+"/flats/analytics/stats?days=7", nil)
				req.Header.Set("X-Flats-Console", "1")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				var stats struct {
					Usage core.UsageReport `json:"usage"`
				}
				err = json.NewDecoder(resp.Body).Decode(&stats)
				resp.Body.Close()
				if err != nil || resp.StatusCode != 200 {
					t.Fatalf("stats: %d %v", resp.StatusCode, err)
				}
				if stats.Usage.Totals.Requests < 1 || stats.Usage.Totals.CPUSeconds == nil || *stats.Usage.Totals.CPUSeconds <= 0 || stats.Usage.Now.CPU != core.UsageMeasured {
					t.Errorf("%s lost Analytics usage: %+v", prefix, stats.Usage)
				}
			}
			code, body := fetch(t, "http://"+h.Addr()+"/metrics")
			if !tc.enabled && code != http.StatusNotFound {
				t.Errorf("disabled metrics=%d", code)
			}
			if tc.enabled {
				if code != http.StatusOK {
					t.Fatalf("metrics=%d %.200s", code, body)
				}
				present := strings.Contains(body, `flat="analytics"`)
				if present != !tc.explicitOff {
					t.Errorf("per-flat series present=%v, want %v", present, !tc.explicitOff)
				}
			}
		})
	}
}

// localhost names the listener category, not a bind or caller-IP restriction.
func TestMetricsLocalhostSelectionPreservesNonLoopbackBind(t *testing.T) {
	m := metrics.New(metrics.Options{})
	defer m.Close()
	handler := loopbackGuard(&net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 7878},
		metricsHandler(m.Handler(), config.MetricsConfig{Enabled: true, ExposeOn: []string{"localhost"}}))
	for _, tc := range []struct {
		host, origin string
		want         int
	}{
		{"192.0.2.10:7878", "", http.StatusOK},
		{"192.0.2.10:7878", "http://192.0.2.10:7878", http.StatusOK},
		{"attacker.example:7878", "", http.StatusForbidden},
		{"192.0.2.10:7878", "http://attacker.example:7878", http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/metrics", nil)
		req.RemoteAddr = "198.51.100.20:54321"
		req.Header.Set("Origin", tc.origin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("host %s origin %s: %d, want%d", tc.host, tc.origin, rec.Code, tc.want)
		}
		if tc.want == http.StatusOK && !strings.Contains(rec.Body.String(), "flats_build_info") {
			t.Error("admitted non-loopback listener has no metrics")
		}
	}
}
