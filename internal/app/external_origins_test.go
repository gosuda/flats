package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestExternalOriginsReverseProxy(t *testing.T) {
	h, client := consoleHost(t)
	const origin = "https://flats.example.org"
	if _, err := h.Svc.UpdateSettings(context.Background(), map[string]string{core.SetExternalOrigins: origin}); err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("http://" + h.Addr())
	proxy := httptest.NewServer(&httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Host = "flats.example.org"
		r.Out.Header.Set("X-Forwarded-Proto", "http")
		r.Out.Header.Set("X-Forwarded-Host", "untrusted.example.org")
	}})
	defer proxy.Close()
	for _, path := range []string{"/api/client", "/api/status", "/llms.txt", "/llms-full.txt", "/docs/agent-guide.md"} {
		resp, err := client.Get(proxy.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), origin) || strings.Contains(string(b), "http://flats.example.org") || strings.Contains(string(b), "untrusted.example.org") {
			t.Fatalf("%s: %d %.400s", path, resp.StatusCode, b)
		}
	}
	// MCP stays remote even if a proxy sends no forwarding identity headers.
	session, err := mcp.NewClient(&mcp.Implementation{Name: "external-test", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: proxy.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "save_draft_from_dir", Arguments: map[string]any{"slug": "remote-dir", "dir": "/nonexistent"}})
	if err != nil || !result.IsError {
		t.Fatalf("host directory tool not refused: %+v %v", result, err)
	}
	raw, _ := json.Marshal(result)
	if !strings.Contains(string(raw), "only accepts callers on the Flats host itself") {
		t.Fatalf("wrong directory refusal: %s", raw)
	}
	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "update_settings", Arguments: map[string]any{"external_origins": "https://evil.example.org"}})
	if err == nil && !result.IsError {
		t.Fatal("MCP exposed settings mutation")
	}
	if _, err := h.Svc.CreateFlat(context.Background(), "external-site", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	for _, action := range []struct{ method, path, body string }{
		{"PUT", "/api/flats/external-site/network", `{}`},
		{"PUT", "/api/flats/external-site/secrets/EXAMPLE", `{"value":"synthetic"}`},
		{"DELETE", "/api/flats/external-site/secrets/EXAMPLE", ""},
	} {
		r, _ := http.NewRequest(action.method, proxy.URL+action.path, strings.NewReader(action.body))
		r.Header.Set("X-Flats-Client", "cli")
		r.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("external local-CLI route %s: %d", action.path, resp.StatusCode)
		}
	}
	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "delete_flat", Arguments: map[string]any{"slug": "external-site"}})
	if err != nil || result.IsError {
		t.Fatalf("MCP approval: %+v %v", result, err)
	}
	raw, _ = json.Marshal(result.StructuredContent)
	var approval struct {
		URL string `json:"approval_url"`
	}
	if err := json.Unmarshal(raw, &approval); err != nil || !strings.HasPrefix(approval.URL, origin+"/approvals/") {
		t.Fatalf("MCP approval URL: %s %v", raw, err)
	}
	id := strings.TrimPrefix(approval.URL, origin+"/approvals/")
	decision, _ := http.NewRequest("POST", proxy.URL+"/console/api/approvals/"+id+"/reject", nil)
	decision.Header.Set("Origin", origin)
	decision.Header.Set("Sec-Fetch-Site", "same-origin")
	decision.Header.Set("X-Flats-Console", "1")
	decided, err := client.Do(decision)
	if err != nil {
		t.Fatal(err)
	}
	decided.Body.Close()
	if decided.StatusCode != 200 {
		t.Fatalf("reject via external console: %d", decided.StatusCode)
	}
	events, err := h.Svc.Events(context.Background(), "external-site", "approval", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	audit, _ := json.Marshal(events)
	if !strings.Contains(string(audit), "the console via external origin "+origin) || strings.Contains(string(audit), "the console on the loopback listener") {
		t.Fatalf("incorrect decision provenance: %s", audit)
	}
	r, _ := http.NewRequest("GET", proxy.URL+"/api/status", nil)
	r.Header.Set("Origin", "https://evil.example.org")
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("foreign origin: %d", resp.StatusCode)
	}
	// Approval URLs consume precisely the same validated request context.
	var retained context.Context
	guard := &api.HostGuard{ExternalOrigins: h.Svc.ExternalConsoleOrigins, Next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		retained = r.Context()
		if got := h.Svc.ApprovalURL(r.Context(), "test-id"); got != origin+"/approvals/test-id" {
			t.Errorf("approval URL: %s", got)
		}
		if requestOnLoopback(r.Context()) {
			t.Error("proxied console classified as loopback")
		}
	})}
	guard.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", origin+"/", nil))
	if _, err := h.Svc.UpdateSettings(context.Background(), map[string]string{core.SetExternalOrigins: ""}); err != nil {
		t.Fatal(err)
	}
	if got := h.ConsoleURLFor(retained); got != h.ConsoleURL() {
		t.Fatalf("removed origin retained: %s", got)
	}
	resp, err = client.Get(proxy.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("removed origin accepted: %d", resp.StatusCode)
	}
}

func TestExternalOriginsOnlyConsoleSettings(t *testing.T) {
	h, client := consoleHost(t)
	for _, path := range []string{"/api/settings", "/console/api/settings"} {
		r, _ := http.NewRequest("PUT", "http://"+h.Addr()+path, strings.NewReader(`{"external_origins":"https://evil.example.org"}`))
		r.Header.Set("X-Flats-Client", "cli")
		r.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("agent settings accepted at %s", path)
		}
	}
	if len(h.Svc.ExternalConsoleOrigins()) != 0 {
		t.Fatal("agent changed external origins")
	}
}

// Metrics wrapping must retain external-origin admission and count refusals.
func TestExternalOriginMetrics(t *testing.T) {
	h, client := consoleHost(t)
	if _, err := h.Svc.UpdateSettings(context.Background(), map[string]string{core.SetExternalOrigins: "https://metrics.example.org"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host, origin, fetch string
		want                int
	}{
		{"metrics.example.org", "", "", 200},
		{"metrics.example.org", "https://metrics.example.org", "same-origin", 200},
		{"metrics.example.org", "http://metrics.example.org", "", 403},
		{"metrics.example.org", "https://other.example.org", "", 403},
		{"metrics.example.org", "", "cross-site", 403},
		{"undeclared.example.org", "", "", 403},
	} {
		r, _ := http.NewRequest("GET", "http://"+h.Addr()+"/metrics", nil)
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", tc.fetch)
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("metrics %s %s %s: %d", tc.host, tc.origin, tc.fetch, resp.StatusCode)
		}
		if tc.want == 200 && !strings.Contains(string(body), "flats_build_info") {
			t.Fatal("metrics not served")
		}
	}
	if got := h.metrics.ManagementCount("metrics", "4xx"); got != 4 {
		t.Fatalf("guard refusals counted %d want4", got)
	}
}
