package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
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

// A console domain serves /metrics, and its refused requests are counted.
func TestConsoleDomainServesAndCountsMetrics(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	if _, err := h.AddConsoleDomain(context.Background(), "tailscale", "admin", ""); err != nil {
		t.Fatal(err)
	}
	tail.mu.Lock()
	handler := tail.served["admin"]
	tail.mu.Unlock()
	before := h.metrics.ManagementCount("metrics", "4xx")
	for host, want := range map[string]int{"admin.tail1234.ts.net": 200, "other.tail1234.ts.net": 403} {
		req, _ := http.NewRequest("GET", "http://"+host+"/metrics", nil)
		rec := &recorder{header: http.Header{}}
		handler.ServeHTTP(rec, req)
		if rec.code != want {
			t.Errorf("Host %s: %d, want %d", host, rec.code, want)
		}
		if want == 200 && !strings.Contains(rec.body.String(), "flats_build_info") {
			t.Errorf("Host %s: no exposition", host)
		}
	}
	if got := h.metrics.ManagementCount("metrics", "4xx") - before; got != 1 {
		t.Errorf("refused console-domain requests counted %d, want 1", got)
	}
}
