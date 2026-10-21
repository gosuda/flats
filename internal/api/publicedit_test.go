package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// The console checks anyone-can-edit first, and a blocked approval answers
// 409 with its category and problems so the console can show why.
func TestPublicEditAPI(t *testing.T) {
	srv, svc := setup(t)
	ctx := context.Background()
	body := `{"op_id":"api-public-1","ops":[{"op":"add","el":{"type":"sticky","text":"alpha"}}]}`
	if code, out := req(t, "POST", srv.URL+"/api/flats/plan/board", strings.NewReader(body), map[string]string{"Content-Type": "application/json"}); code != 200 {
		t.Fatalf("create board = %d %v", code, out)
	}
	if err := svc.SetProviderPermission(ctx, "plan", store.ProviderPortal, true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateSettings(ctx, map[string]string{core.SetDiskQuotaBytes: "0"}); err != nil {
		t.Fatal(err)
	}
	console := consoleHdr(t, srv)
	code, out := req(t, "GET", srv.URL+"/console/api/flats/plan/public-edit", nil, console)
	if code != 200 {
		t.Fatalf("preflight = %d %v", code, out)
	}
	for _, k := range []string{"problems", "disk_quota", "revision_quota", "pin_quota", "pins", "safe_point", "safe_point_pinned", "listed"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("preflight lacks %s: %v", k, out)
		}
	}
	if ps := out["problems"].([]any); len(ps) != 1 || ps[0].(map[string]any)["code"] != core.ProblemDiskQuotaOff {
		t.Fatalf("preflight problems = %v", out["problems"])
	}
	if code, _ := req(t, "GET", srv.URL+"/api/flats/plan/public-edit", nil, nil); code != 404 && code != 405 {
		t.Fatalf("agent preflight route = %d", code)
	}
	code, pending := req(t, "POST", srv.URL+"/console/api/flats/plan/visibility", strings.NewReader(`{"visibility":"public","access":"edit"}`), console)
	if code != 202 {
		t.Fatalf("request = %d %v", code, pending)
	}
	params := pending["approval"].(map[string]any)["params"].(map[string]any)
	if params["access"] != "edit" || params["access_seq"] == nil {
		t.Fatalf("approval params = %v", params)
	}
	id := pending["approval"].(map[string]any)["id"].(string)
	code, out = req(t, "POST", srv.URL+"/console/api/approvals/"+id+"/approve", nil, console)
	if code != 409 || out["category"] != "public_edit_blocked" {
		t.Fatalf("blocked approval = %d %v", code, out)
	}
	if ps, _ := out["problems"].([]any); len(ps) != 1 || ps[0].(map[string]any)["code"] != core.ProblemDiskQuotaOff || ps[0].(map[string]any)["message"] == "" {
		t.Fatalf("blocked problems = %v", out["problems"])
	}
	if code, out := req(t, "GET", srv.URL+"/api/flats/plan", nil, nil); code != 200 || out["visibility"] != "private" || out["public_access"] != nil {
		t.Fatalf("flat after a blocked approval = %d %v", code, out)
	}
	// A website has no edit level, and Private takes none.
	saveAndPublish(t, srv, "site", "hello")
	for _, b := range []string{`{"visibility":"public","access":"edit"}`, `{"visibility":"private","access":"view"}`} {
		if code, out := req(t, "POST", srv.URL+"/api/flats/site/visibility", strings.NewReader(b), map[string]string{"Content-Type": "application/json"}); code != 400 || out["category"] != "invalid" {
			t.Fatalf("%s = %d %v", b, code, out)
		}
	}
}
