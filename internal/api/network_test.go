package api_test

import (
	"bytes"
	"github.com/gosuda/flats/internal/api"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNetworkPermissionsOperatorOnly(t *testing.T) {
	srv, svc := setup(t)
	req(t, "POST", srv.URL+"/api/flats/site/versions", bytes.NewReader(archive(map[string]string{"index.html": "ok"})), nil)
	endpoint := srv.URL + "/api/flats/site/network"
	if code, out := req(t, "GET", endpoint, nil, nil); code != 200 || !strings.Contains(out["note"].(string), "redeploy to revoke") {
		t.Fatalf("initial: %d %v", code, out)
	}
	if code, _ := req(t, "PUT", endpoint, strings.NewReader(`{"origins":["https://api.example.com"]}`), nil); code != 403 {
		t.Fatalf("agent grant=%d", code)
	}
	remote := httptest.NewRequest("PUT", "/api/flats/site/network", strings.NewReader(`{"origins":["https://api.example.com"]}`))
	remote.RemoteAddr = "203.0.113.1:12345"
	remote.Header.Set("X-Flats-Client", "cli")
	remote.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	(&api.Server{Svc: svc}).Handler().ServeHTTP(recorder, remote)
	if recorder.Code != 403 {
		t.Fatalf("remote CLI granted permission: %d", recorder.Code)
	}
	cli := map[string]string{"X-Flats-Client": "cli"}
	if code, out := req(t, "PUT", endpoint, strings.NewReader(`{"origins":["https://api.example.com:443/"]}`), cli); code != 200 || len(out["origins"].([]any)) != 1 || out["origins"].([]any)[0] != "https://api.example.com" {
		t.Fatalf("grant: %d %v", code, out)
	}
	for _, body := range []string{`{}`, `{"origins":null}`, `{"origins":["http://127.0.0.1"]}`, `{"origins":["https://*.example.com"]}`} {
		if code, _ := req(t, "PUT", endpoint, strings.NewReader(body), cli); code != 400 {
			t.Fatalf("invalid=%s status=%d", body, code)
		}
	}
	if code, out := req(t, "GET", endpoint, nil, nil); code != 200 || len(out["origins"].([]any)) != 1 {
		t.Fatalf("invalid mutation changed grants: %d %v", code, out)
	}
	if code, out := req(t, "PUT", endpoint, strings.NewReader(`{"origins":[]}`), cli); code != 200 || len(out["origins"].([]any)) != 0 {
		t.Fatalf("clear: %d %v", code, out)
	}
}
