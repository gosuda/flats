package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
)

const publicInstancePath = "/console/api/experiments/public-instance"

func setupPublicInstance(t *testing.T) (*httptest.Server, *core.Service, *hostLog) {
	t.Helper()
	srv, svc, logs := setupAutopilot(t)
	if _, err := svc.SetPermittedNetworks([]string{"portal", "zrok"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateSettings(t.Context(), map[string]string{core.SetDefaultPublic: "portal"}); err != nil {
		t.Fatal(err)
	}
	return srv, svc, logs
}

func publicInstanceOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	v, ok := out["public_instance"].(map[string]any)
	if !ok {
		t.Fatalf("missing public_instance status: %v", out)
	}
	return v
}

func TestPublicInstanceConsoleRoutes(t *testing.T) {
	srv, svc, _ := setupPublicInstance(t)
	if _, err := svc.CreateFlat(t.Context(), "before", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	code, out := req(t, "GET", srv.URL+publicInstancePath, nil, consoleHdr(t, srv))
	want := map[string]any{"enabled": false, "network": "portal", "usable": true, "reason": "", "private_flats": float64(1)}
	if code != 200 || !reflect.DeepEqual(publicInstanceOf(t, out), want) {
		t.Fatalf("default status: %d %v, want %v", code, out, want)
	}
	for _, enabled := range []bool{true, false} {
		body := `{"enabled":false}`
		if enabled {
			body = `{"enabled":true,"confirmed_network":"portal"}`
		}
		code, out = req(t, "PUT", srv.URL+publicInstancePath, strings.NewReader(body), consoleHdr(t, srv))
		v := publicInstanceOf(t, out)
		if code != 200 || v["enabled"] != enabled || v["network"] != "portal" || v["usable"] != true || v["reason"] != "" || v["private_flats"] != float64(1) {
			t.Fatalf("set enabled=%v: %d %v", enabled, code, out)
		}
		at, ok := v["updated_at"].(string)
		if parsed, err := time.Parse(time.RFC3339, at); !ok || err != nil || parsed.Location() != time.UTC {
			t.Fatalf("updated_at is not RFC3339 UTC: %v (%v)", v["updated_at"], err)
		}
		code, stored := req(t, "GET", srv.URL+publicInstancePath, nil, consoleHdr(t, srv))
		if code != 200 || !reflect.DeepEqual(stored, out) {
			t.Fatalf("read back: %d %v, want %v", code, stored, out)
		}
		if svc.PublicInstance().Enabled != enabled {
			t.Fatalf("service cache did not change to %v", enabled)
		}
	}
}

func TestPublicInstanceConsoleOnly(t *testing.T) {
	srv, svc, _ := setupPublicInstance(t)
	for _, method := range []string{"GET", "PUT"} {
		for name, hdr := range map[string]map[string]string{
			"agent":   nil,
			"console": consoleHdr(t, srv),
			"cli":     {"X-Flats-Client": "cli", "Content-Type": "application/json"},
		} {
			t.Run("agent route/"+method+"/"+name, func(t *testing.T) {
				code, out := req(t, method, srv.URL+"/api/experiments/public-instance", strings.NewReader(`{"enabled":true,"confirmed_network":"portal"}`), hdr)
				if code != 404 {
					t.Fatalf("agent route exists: %d %v", code, out)
				}
			})
		}
	}
	with := func(k, v string) map[string]string {
		hdr := consoleHdr(t, srv)
		hdr[k] = v
		return hdr
	}
	for name, hdr := range map[string]map[string]string{
		"no console header": with("X-Flats-Console", ""),
		"cli":               with("X-Flats-Client", "cli"),
		"agent":             with("X-Flats-Client", "agent"),
		"cross-site":        with("Sec-Fetch-Site", "cross-site"),
		"same-site":         with("Sec-Fetch-Site", "same-site"),
		"foreign origin":    with("Origin", "https://evil.example"),
		"no origin":         with("Origin", ""),
		"no fetch site":     with("Sec-Fetch-Site", ""),
	} {
		t.Run("refused mutation/"+name, func(t *testing.T) {
			code, out := req(t, "PUT", srv.URL+publicInstancePath, strings.NewReader(`{"enabled":true,"confirmed_network":"portal"}`), hdr)
			if code != 403 {
				t.Fatalf("non-console mutation: %d %v", code, out)
			}
		})
	}
	for name, hdr := range map[string]map[string]string{
		"no console header": nil,
		"cli":               with("X-Flats-Client", "cli"),
		"foreign origin":    with("Origin", "https://evil.example"),
	} {
		t.Run("refused read/"+name, func(t *testing.T) {
			if code, out := req(t, "GET", srv.URL+publicInstancePath, nil, hdr); code != 403 {
				t.Fatalf("non-console read: %d %v", code, out)
			}
		})
	}
	if svc.PublicInstance().Enabled {
		t.Fatal("refused mutation enabled public instance mode")
	}
}

func TestPublicInstanceRefusesInvalidRequests(t *testing.T) {
	srv, svc, _ := setupPublicInstance(t)
	for _, tc := range []struct{ name, body string }{
		{"empty", ``},
		{"missing enabled", `{}`},
		{"null", `null`},
		{"array", `[true]`},
		{"null enabled", `{"enabled":null}`},
		{"string enabled", `{"enabled":"yes"}`},
		{"number enabled", `{"enabled":1}`},
		{"missing confirmation", `{"enabled":true}`},
		{"null confirmation", `{"enabled":true,"confirmed_network":null}`},
		{"number confirmation", `{"enabled":true,"confirmed_network":1}`},
		{"array confirmation", `{"enabled":true,"confirmed_network":["portal"]}`},
		{"object confirmation", `{"enabled":true,"confirmed_network":{}}`},
		{"off with confirmation", `{"enabled":false,"confirmed_network":"portal"}`},
		{"off with null confirmation", `{"enabled":false,"confirmed_network":null}`},
		{"unknown field", `{"enabled":false,"extra":1}`},
		{"truncated", `{"enabled":false`},
		{"second object", `{"enabled":false}{"enabled":true}`},
		{"trailing garbage", `{"enabled":false}x`},
		{"oversized", `{"enabled":true,"confirmed_network":"` + strings.Repeat("x", 1<<20) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := req(t, "PUT", srv.URL+publicInstancePath, strings.NewReader(tc.body), consoleHdr(t, srv))
			if code != 400 || out["category"] != "invalid" {
				t.Fatalf("invalid request: %d %v", code, out)
			}
		})
	}
	if svc.PublicInstance().Enabled {
		t.Fatal("invalid body changed the settings")
	}
}

func TestPublicInstanceStaleNetwork(t *testing.T) {
	srv, svc, _ := setupPublicInstance(t)
	if _, err := svc.UpdateSettings(t.Context(), map[string]string{core.SetDefaultPublic: "zrok"}); err != nil {
		t.Fatal(err)
	}
	code, out := req(t, "PUT", srv.URL+publicInstancePath, strings.NewReader(`{"enabled":true,"confirmed_network":"portal"}`), consoleHdr(t, srv))
	if code != 409 || out["category"] != "conflict" || out["public_instance"] != nil {
		t.Fatalf("stale confirmation: %d %v", code, out)
	}
	if svc.PublicInstance().Enabled {
		t.Fatal("stale confirmation changed the settings")
	}
	code, out = req(t, "GET", srv.URL+publicInstancePath, nil, consoleHdr(t, srv))
	if code != 200 || publicInstanceOf(t, out)["network"] != "zrok" {
		t.Fatalf("new confirmation network: %d %v", code, out)
	}
}

func TestPublicInstanceUnusableNetwork(t *testing.T) {
	srv, svc, _ := setupPublicInstance(t)
	if _, err := svc.SetPermittedNetworks(nil, ""); err != nil {
		t.Fatal(err)
	}
	code, out := req(t, "GET", srv.URL+publicInstancePath, nil, consoleHdr(t, srv))
	v := publicInstanceOf(t, out)
	if code != 200 || v["enabled"] != false || v["usable"] != false || v["reason"] == "" {
		t.Fatalf("unusable status: %d %v", code, out)
	}
	code, out = req(t, "PUT", srv.URL+publicInstancePath, strings.NewReader(`{"enabled":true,"confirmed_network":"portal"}`), consoleHdr(t, srv))
	if code != 409 || out["category"] != "conflict" || svc.PublicInstance().Enabled {
		t.Fatalf("unusable turn on: %d %v", code, out)
	}
	if code, out = req(t, "PUT", srv.URL+publicInstancePath, strings.NewReader(`{"enabled":false}`), consoleHdr(t, srv)); code != 200 || publicInstanceOf(t, out)["enabled"] != false {
		t.Fatalf("turn off needs no network: %d %v", code, out)
	}
}

func TestPublicInstanceNetworkBecomesUnusable(t *testing.T) {
	srv, svc, _ := setupPublicInstance(t)
	if code, out := req(t, "PUT", srv.URL+publicInstancePath, strings.NewReader(`{"enabled":true,"confirmed_network":"portal"}`), consoleHdr(t, srv)); code != 200 {
		t.Fatalf("turn on: %d %v", code, out)
	}
	if _, err := svc.SetPermittedNetworks(nil, ""); err != nil {
		t.Fatal(err)
	}
	code, out := req(t, "GET", srv.URL+publicInstancePath, nil, consoleHdr(t, srv))
	v := publicInstanceOf(t, out)
	if code != 200 || v["enabled"] != true || v["usable"] != false || v["reason"] == "" {
		t.Fatalf("enabled but unusable status: %d %v", code, out)
	}
	code, out = req(t, "PUT", srv.URL+publicInstancePath, strings.NewReader(`{"enabled":false}`), consoleHdr(t, srv))
	if code != 200 || publicInstanceOf(t, out)["enabled"] != false || svc.PublicInstance().Enabled {
		t.Fatalf("turn off without a usable network: %d %v", code, out)
	}
}

func TestPublicInstanceChangedBy(t *testing.T) {
	_, svc, logs := setupPublicInstance(t)
	handler := api.TailnetIdentity((&api.Server{Svc: svc}).Handler())
	hdr := map[string]string{
		"X-Flats-Console": "1", "Sec-Fetch-Site": "same-origin", "Origin": "http://console.tailnet.ts.net",
		"Content-Type": "application/json", "Tailscale-User-Login": "alice@example.com",
	}
	code, body := send(t, handler, "PUT", "http://console.tailnet.ts.net"+publicInstancePath, `{"enabled":true,"confirmed_network":"portal"}`, hdr)
	if code != 200 {
		t.Fatalf("turn on through tailnet: %d %s", code, body)
	}
	logs.mu.Lock()
	defer logs.mu.Unlock()
	var changes []string
	for _, line := range logs.lines {
		if strings.HasPrefix(line, "public instance:") {
			changes = append(changes, line)
		}
	}
	if len(changes) != 1 || !strings.Contains(changes[0], `tailnet user "alice@example.com"`) || !changeTime.MatchString(changes[0]) {
		t.Fatalf("change did not record console actor and time: %v", changes)
	}
}

func TestPublicInstanceFlatViewKey(t *testing.T) {
	srv, _, _ := setupPublicInstance(t)
	code, out := req(t, "POST", srv.URL+"/api/flats", strings.NewReader(`{"slug":"key-shape"}`), nil)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	if _, present := out["private_links_off"]; present {
		t.Fatalf("off-mode create gained private_links_off: %v", out)
	}
	for _, enabled := range []bool{false, true, false} {
		body := fmt.Sprintf(`{"enabled":%v}`, enabled)
		if enabled {
			body = `{"enabled":true,"confirmed_network":"portal"}`
		}
		if code, out := req(t, "PUT", srv.URL+publicInstancePath, strings.NewReader(body), consoleHdr(t, srv)); code != 200 {
			t.Fatalf("set mode: %d %v", code, out)
		}
		for _, prefix := range []string{"/api", "/console/api"} {
			for _, path := range []string{"/flats/key-shape", "/flats"} {
				code, out := req(t, "GET", srv.URL+prefix+path, nil, consoleHdr(t, srv))
				if code != http.StatusOK {
					t.Fatalf("read flat: %d %v", code, out)
				}
				if path == "/flats" {
					out = out["flats"].([]any)[0].(map[string]any)
				}
				value, present := out["private_links_off"]
				if present != enabled || (enabled && value != true) {
					t.Fatalf("enabled=%v %s%s key presence=%v value=%v", enabled, prefix, path, present, value)
				}
				if !enabled {
					if privateURL, ok := out["private_url"].(string); !ok || privateURL == "" {
						t.Fatalf("off mode lost private URL: %v", out)
					}
				}
				if enabled {
					b, err := json.Marshal(out)
					if err != nil || strings.Contains(string(b), ".localhost") {
						t.Fatalf("on-mode view leaked a local private link: %s (%v)", b, err)
					}
				}
			}
		}
	}
}
