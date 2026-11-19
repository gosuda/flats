package mcpx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/portal-sign-in.md through MCP: sign_in in list_flats and get_flat (rule 57), set_signin in get_approval
// without its entries (rule 58), the notices of a flat with sign-in on (rule 61), and no tool that changes sign-in
// (rules 13, 25, 49 and 54).

// Distinctive values that no MCP output may hold.
const (
	mcpEntry  = "mcp-entry-distinct@example.com"
	mcpSub    = "McpDistinct9"
	mcpDomain = "@mcpdistinct.example"
	mcpSecret = "cs-mcp-distinct-52"
)

func noMCPEntry(t *testing.T, what, text string) {
	t.Helper()
	for _, s := range []string{mcpEntry, mcpSub, mcpDomain, mcpSecret} {
		if strings.Contains(text, s) {
			t.Errorf("%s holds %q: %s", what, s, text)
		}
	}
}

// mcpCall runs a tool and returns its text and its structured output, which must hold no entry.
func mcpCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text := resultText(res)
	raw, _ := json.Marshal(res.StructuredContent)
	noMCPEntry(t, name, text+string(raw))
	return text, string(raw)
}

func TestSignInThroughMCP(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, name := range toolNames(t, e.local) {
		for _, bad := range []string{"signin", "sign_in", "sign-in", "revoke", "link", "credential"} {
			if strings.Contains(strings.ToLower(name), bad) {
				t.Errorf("the tool %q changes sign-in", name)
			}
		}
	}
	tools, err := e.local.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if (tool.Name == "list_flats" || tool.Name == "get_flat") && !strings.Contains(tool.Description, "sign_in") || strings.Contains(tool.Description, "sign_in") && !strings.Contains(tool.Description, "protects its Portal URL only") {
			t.Errorf("the description of %s: %s", tool.Name, tool.Description)
		}
	}

	publishedFixture(t, e)
	if code, out := e.operatorCall(t, "POST", "/flats/census/providers", `{"provider":"portal","permitted":true}`); code != 200 {
		t.Fatalf("grant: %d %v", code, out)
	}
	var vis ActionOut
	if text, failed := call(t, e.local, "set_visibility", map[string]any{"slug": "census", "visibility": "public"}, &vis); failed || vis.ApprovalID == "" {
		t.Fatalf("set_visibility: %s", text)
	}
	e.approve(t, vis.ApprovalID)
	if text, _ := mcpCall(t, e.local, "get_flat", map[string]any{"slug": "census"}); strings.Contains(text, "sign-in") {
		t.Fatalf("a flat without sign-in: %s", text)
	}

	if _, err := e.svc.SetSignInSecret(ctx, "census", mcpSecret, "https://idp.example.com", "flats-app", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	r, err := e.svc.RequestSignIn(ctx, "census", core.SignInRequest{Provider: "oidc", Issuer: "https://idp.example.com", ClientID: "flats-app",
		Allow: []string{mcpEntry, "sub:" + mcpSub, mcpDomain}}, core.ViaCLI)
	if err != nil || r.Approval == nil {
		t.Fatalf("RequestSignIn: %+v %v", r, err)
	}
	id := r.Approval.ID
	// Rule 58: get_approval shows the counted settings, to a local and a remote agent.
	for _, cs := range []*mcp.ClientSession{e.local, e.remote} {
		text, raw := mcpCall(t, cs, "get_approval", map[string]any{"id": id})
		if !strings.Contains(raw, `"action":"set_signin"`) || !strings.Contains(raw, `"to":{"client_id":"flats-app","count":3,"issuer":"https://idp.example.com","provider":"oidc"}`) ||
			!strings.Contains(text, "Waiting for the operator") {
			t.Errorf("get_approval of a pending set_signin: %s %s", text, raw)
		}
	}
	// An agent cannot decide it (rule 17), and the refusal holds no entry.
	setAutopilot(t, e, "", core.AutopilotOps...)
	if res, err := e.local.CallTool(ctx, &mcp.CallToolParams{Name: "approve_request", Arguments: map[string]any{"id": id, "agent": "bot"}}); err != nil || !res.IsError {
		t.Fatalf("approve_request of set_signin: %v %+v", err, res)
	} else {
		noMCPEntry(t, "approve_request", resultText(res))
	}
	e.approve(t, id)

	// Rule 57: sign_in in get_flat and list_flats, without an entry.
	text, raw := mcpCall(t, e.local, "get_flat", map[string]any{"slug": "census"})
	if !strings.Contains(raw, `"sign_in":{"allow_count":3,"client_id":"flats-app","client_secret":{"name":"oidc-client-secret"},"issuer":"https://idp.example.com","provider":"oidc"}`) ||
		!strings.Contains(text, "Portal sign-in: OpenID Connect at https://idp.example.com, client ID flats-app, 3 allowlist entries. It protects the Portal URL only") {
		t.Errorf("get_flat: %s %s", text, raw)
	}
	// Rule 61: the public notice, and the notice of the approved visibility change in get_approval.
	if !strings.Contains(text, "Notice: "+core.SignInPublicNotice) || strings.Contains(text, "anyone on the internet") {
		t.Errorf("get_flat notice: %s", text)
	}
	text, raw = mcpCall(t, e.local, "list_flats", map[string]any{})
	if !strings.Contains(raw, `"allow_count":3`) || !strings.Contains(text, "Portal sign-in oidc (the Portal URL only)") {
		t.Errorf("list_flats: %s %s", text, raw)
	}
	if text, _ := mcpCall(t, e.local, "get_approval", map[string]any{"id": vis.ApprovalID}); !strings.Contains(text, "\nNotice: "+core.SignInPublicNotice) {
		t.Errorf("get_approval of the visibility change: %s", text)
	}
	mcpCall(t, e.remote, "get_approval", map[string]any{"id": id})
	// get_logs holds the counted form only.
	if text, _ := mcpCall(t, e.local, "get_logs", map[string]any{"slug": "census", "limit": 1000}); !strings.Contains(text, "sign-in set to oidc with issuer https://idp.example.com and 3 entries") {
		t.Errorf("get_logs: %s", text)
	}
	mcpCall(t, e.local, "list_secrets", map[string]any{"slug": "census"})
}

// The notices of rule 61 that MCP shows for the visibility of a flat with sign-in on.
func TestSignInNoticeFor(t *testing.T) {
	for _, tc := range []struct {
		vis    string
		signIn bool
		want   string
	}{
		{"public", false, core.PublicAccessNotice},
		{"public", true, core.SignInPublicNotice},
		{"public-unlisted", false, core.UnlistedNotice},
		{"public-unlisted", true, core.SignInUnlistedNotice},
		{"public-listed", false, core.ListedNotice},
		{"public-listed", true, core.SignInListedNotice},
		{"private", true, ""},
	} {
		got := noticeFor(store.Visibility(tc.vis), tc.signIn)
		if got != tc.want {
			t.Errorf("noticeFor(%s, %v) = %q", tc.vis, tc.signIn, got)
		}
		if tc.signIn && got != "" && (strings.Contains(got, "anyone on the internet") || got != core.SignInPublicNotice && !strings.Contains(got, "anyone who passes its sign-in")) {
			t.Errorf("noticeFor(%s, true) = %q", tc.vis, got)
		}
	}
}
