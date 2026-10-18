package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
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
	for _, path := range []string{"/api/status", "/llms.txt", "/llms-full.txt", "/docs/agent-guide.md"} {
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
