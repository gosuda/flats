package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

// docs/spec/portal-require-auth.md: the setting is a console setting. It applies at once, no agent route changes it,
// and the console reads whether it is on for a set_signin approval.

func TestRequireAuthSettingIsConsoleOnly(t *testing.T) {
	srv, _ := setup(t)
	// The agent API has no route for the settings.
	if code, _ := req(t, "PUT", srv.URL+"/api/settings", strings.NewReader(`{"portal_require_auth":"true"}`), nil); code != 404 && code != 405 {
		t.Fatalf("agent settings change = %d", code)
	}
	// A request that carries the CLI header is not a console request.
	hdr := consoleHdr(t, srv)
	hdr["X-Flats-Client"] = "cli"
	if code, _ := req(t, "PUT", srv.URL+"/console/api/settings", strings.NewReader(`{"portal_require_auth":"true"}`), hdr); code != 403 {
		t.Fatalf("a console route with the CLI header = %d, want 403", code)
	}
	if code, _ := req(t, "PUT", srv.URL+"/console/api/settings", strings.NewReader(`{"portal_require_auth":"maybe"}`), consoleHdr(t, srv)); code != 400 {
		t.Fatalf("a value that is not a boolean = %d, want 400", code)
	}

	code, out := req(t, "GET", srv.URL+"/console/api/settings", nil, consoleHdr(t, srv))
	settings, _ := out["settings"].(map[string]any)
	defaults, _ := out["defaults"].(map[string]any)
	if code != 200 || settings["portal_require_auth"] != "false" || defaults["portal_require_auth"] != "false" {
		t.Fatalf("settings: %d %v", code, out)
	}
	code, out = req(t, "PUT", srv.URL+"/console/api/settings", strings.NewReader(`{"portal_require_auth":"true"}`), consoleHdr(t, srv))
	if code != 200 {
		t.Fatalf("settings: %d %v", code, out)
	}
	applied, _ := out["applied"].([]any)
	restart, _ := out["restart_required"].([]any)
	if len(applied) != 1 || applied[0] != "portal_require_auth" || len(restart) != 0 {
		t.Fatalf("portal_require_auth should apply without a restart: %v", out)
	}
	if code, out := req(t, "GET", srv.URL+"/console/api/settings", nil, consoleHdr(t, srv)); code != 200 || out["settings"].(map[string]any)["portal_require_auth"] != "true" {
		t.Fatalf("settings after the change: %d %v", code, out)
	}
}

// A set_signin approval tells the console whether portal.require_auth is on, because the title and the warning of its
// page say what turning sign-in off does (rule 21). No other caller gets the member, and an approval of another
// action never has it.
func TestRequireAuthApprovalMember(t *testing.T) {
	srv, svc, _ := signInSetup(t)
	saveAndPublish(t, srv, "app", "ok")
	console := consoleHdr(t, srv)
	code, out := call(t, "POST", srv.URL+"/api/flats/app/signin", `{"provider":"credential","allow":["guest-7Qx"]}`, map[string]string{"X-Flats-Client": "cli"})
	var res struct {
		Approval struct {
			ID string `json:"id"`
		} `json:"approval"`
	}
	if code != 202 || json.Unmarshal([]byte(out), &res) != nil || res.Approval.ID == "" {
		t.Fatalf("request = %d %s", code, out)
	}
	id := res.Approval.ID
	has := func(url string, hdr map[string]string) bool {
		t.Helper()
		code, body := call(t, "GET", url, "", hdr)
		if code != 200 {
			t.Fatalf("GET %s = %d %s", url, code, body)
		}
		return strings.Contains(strings.Join(strings.Fields(body), ""), `"portal_require_auth":true`)
	}
	if has(srv.URL+"/console/api/approvals/"+id, console) || has(srv.URL+"/console/api/approvals?status=pending", console) {
		t.Fatal("the member is present with the setting off")
	}
	if _, err := svc.UpdateSettings(context.Background(), map[string]string{core.SetRequireAuth: "true"}); err != nil {
		t.Fatal(err)
	}
	if !has(srv.URL+"/console/api/approvals/"+id, console) || !has(srv.URL+"/console/api/approvals?status=pending", console) {
		t.Fatal("the console does not read that the setting is on")
	}
	if has(srv.URL+"/api/approvals/"+id, nil) || has(srv.URL+"/api/approvals?status=pending", nil) {
		t.Fatal("the agent API carries the member")
	}
	code, out = call(t, "POST", srv.URL+"/api/flats/app/visibility", `{"visibility":"public"}`, map[string]string{"X-Flats-Client": "cli"})
	var other struct {
		Approval struct {
			ID string `json:"id"`
		} `json:"approval"`
	}
	if code != 202 || json.Unmarshal([]byte(out), &other) != nil || other.Approval.ID == "" {
		t.Fatalf("visibility request = %d %s", code, out)
	}
	if has(srv.URL+"/console/api/approvals/"+other.Approval.ID, console) {
		t.Fatal("an approval of another action carries the member")
	}
}
