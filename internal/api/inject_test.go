package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

// The secrets route with the injected secrets of docs/spec/injected-fetch-secrets.md (rules 24 and 27).
func TestInjectedSecretRoute(t *testing.T) {
	srv, svc := setup(t)
	if _, err := svc.CreateFlat(context.Background(), "app", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	cli := map[string]string{"X-Flats-Client": "cli"}
	base := srv.URL + "/api/flats/app/secrets/"
	bound := `{"value":"Bearer route-value","inject":{"origin":"https://api.example.com","header":"authorization"}}`
	if code, out := req(t, "PUT", base+"TOKEN", strings.NewReader(bound), nil); code != 403 {
		t.Fatalf("a caller that is not the operator = %d %v", code, out)
	}
	for _, body := range []string{
		`{"value":"v","inject":{}}`,
		`{"value":"v","inject":{"origin":"https://api.example.com"}}`,
		`{"value":"v","inject":{"header":"X-Key"}}`,
		`{"value":"v","inject":{"origin":3,"header":"X-Key"}}`,
		`{"value":"v","inject":{"origin":"https://api.example.com","header":"X-Key","extra":1}}`,
		`{"value":"v","inject":"https://api.example.com"}`,
	} {
		if code, out := req(t, "PUT", base+"TOKEN", strings.NewReader(body), cli); code != 400 {
			t.Fatalf("%s = %d %v", body, code, out)
		}
	}
	wantInject := "map[header:Authorization origin:https://api.example.com]"
	code, out := req(t, "PUT", base+"TOKEN", strings.NewReader(bound), cli)
	if code != 200 || fmt.Sprint(out["inject"]) != wantInject {
		t.Fatalf("set = %d %v", code, out)
	}
	// "inject": null names no binding, and the stored binding stays.
	if code, out := req(t, "PUT", base+"TOKEN", strings.NewReader(`{"value":"Bearer second","inject":null}`), cli); code != 200 || fmt.Sprint(out["inject"]) != wantInject {
		t.Fatalf("inject null = %d %v", code, out)
	}
	if code, out := req(t, "PUT", base+"PLAIN", strings.NewReader(`{"value":"plain"}`), cli); code != 200 || out["inject"] != nil {
		t.Fatalf("readable = %d %v", code, out)
	}
	code, out = req(t, "GET", srv.URL+"/api/flats/app/secrets", nil, nil)
	list, _ := out["secrets"].([]any)
	if code != 200 || len(list) != 2 {
		t.Fatalf("list = %d %v", code, out)
	}
	for _, item := range list {
		m := item.(map[string]any)
		if got := fmt.Sprint(m["inject"]); m["name"] == "TOKEN" && got != wantInject || m["name"] == "PLAIN" && m["inject"] != nil {
			t.Errorf("listed %v", m)
		}
	}
	if strings.Contains(fmt.Sprint(out), "route-value") || strings.Contains(fmt.Sprint(out), "Bearer second") {
		t.Fatalf("the list holds a value: %v", out)
	}
	for name, tc := range map[string]struct {
		secret, body string
		code         int
	}{
		"a binding on a readable secret": {"PLAIN", bound, 409},
		"another binding":                {"TOKEN", `{"value":"Bearer v","inject":{"origin":"https://other.example.com","header":"Authorization"}}`, 409},
		"a binding that TOKEN has":       {"NEW", bound, 409},
		"an http origin":                 {"NEW", `{"value":"Bearer v","inject":{"origin":"http://api.example.com","header":"X-Key"}}`, 400},
		"an empty value of TOKEN":        {"TOKEN", `{"value":""}`, 400},
		"a trailing space":               {"TOKEN", `{"value":"Bearer v "}`, 400},
		"a control character":            {"TOKEN", `{"value":"Bearer v\u0001"}`, 400},
	} {
		for _, route := range []string{"/api", "/console/api"} {
			hdr := cli
			if route == "/console/api" {
				hdr = consoleHdr(t, srv)
			}
			if code, out := req(t, "PUT", srv.URL+route+"/flats/app/secrets/"+tc.secret, strings.NewReader(tc.body), hdr); code != tc.code {
				t.Errorf("%s through %s = %d %v, want %d", name, route, code, out, tc.code)
			}
		}
	}
}
