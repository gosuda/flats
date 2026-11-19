package portal_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/portal"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/mcpx"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/portal-sign-in.md through the host, as signin_flow_test.go builds it, for the outputs that hold no entry
// (rule 58), a stop after step 2 of rule 20 (criterion B7), an asset of a document (criterion B8) and the stop of
// rule 35 that the provider manager makes when a read fails.

// docsApp is a docs app whose worker flatRuntime serves; the host serves the assets of a document itself.
func docsApp() core.DocsApp {
	return core.DocsApp{FS: fstest.MapFS{"server.js": &fstest.MapFile{Data: []byte("export default {}")}}, Hash: strings.Repeat("d", 64), Entry: "server.js", ContentModule: "content.js"}
}

// mcpText runs an MCP tool and returns its text and its structured output.
func mcpText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	raw, _ := json.Marshal(res.StructuredContent)
	return b.String() + string(raw)
}

// mcpSessions connects an agent on the loopback address and a remote one to the MCP endpoint of h.
func mcpSessions(t *testing.T, h *host) (local, remote *mcp.ClientSession) {
	t.Helper()
	mh := mcpx.Handler(h.svc, mcpx.Options{Version: "test"})
	localSrv := httptest.NewServer(mh)
	remoteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "100.64.0.7:41641" // a tailnet peer
		mh.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { localSrv.Close(); remoteSrv.Close() })
	connect := func(url string) *mcp.ClientSession {
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "v0"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cs.Close() })
		return cs
	}
	return connect(localSrv.URL), connect(remoteSrv.URL)
}

// remoteAPI sends a GET of a caller of /api that is not on the loopback address.
func remoteAPI(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = "203.0.113.1:12345"
	r.Header.Set("X-Flats-Client", "test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("remote GET %s = %d %s", path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// An entry that the SDK refuses, in a setting that a hand wrote into flats.db and in an approved setting, reaches no
// output that may not hold an entry: no MCP output, no answer to a remote caller of /api, no flat event, no endpoint
// detail and no line of the host log (rule 58). The SDK quotes it (`principal %q`) in the reason of rule 35 and in the
// failure of step 1 of rule 20.
func TestSignInRefusedEntryInNoOutput(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	h := startHost(t, dir)
	deployServer(t, h, "app")
	publicOnPortal(t, h, "app")
	// No @ and no sub:, which the SDK refuses and the host never stores.
	const leak = "leak-entry-6Hd"
	writeSignIn(t, dir, "app", "oidc", "https://idp.example", "flats-app", leak)

	// The stored setting: a client secret set builds the gate again (rule 28), which fails with the reason of rule 35
	// and stops the served route.
	set, err := h.svc.SetSignInSecret(ctx, "app", "cs-outputs-1", "https://idp.example", "flats-app", core.ViaCLI)
	if err != nil || !strings.Contains(set.Message, "the Portal route of app is stopped: sign-in gate: ") || !strings.Contains(set.Message, "principal <entry> must be") {
		t.Fatalf("set = %+v, %v", set, err)
	}
	fv, err := h.svc.GetFlat(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	var detail string
	for _, ep := range fv.Endpoints {
		if ep.Provider == core.ProviderPortal {
			detail = ep.Detail
		}
	}
	if !strings.HasPrefix(detail, "sign-in gate: ") || !strings.Contains(detail, "principal <entry> must be") {
		t.Fatalf("the Portal endpoint detail = %q", detail)
	}
	// A visibility approval that needs the Portal route fails with that reason.
	r, err := h.svc.SetVisibility(ctx, "app", store.Private, core.ViaConsole, "")
	decide(t, h, r, err)
	r, err = h.svc.SetVisibility(ctx, "app", store.Public, core.ViaConsole, "")
	if err != nil {
		t.Fatal(err)
	}
	vis, visErr := h.svc.Decide(ctx, r.Approval.ID, true)
	if vis.Status != "failed" || !strings.Contains(fmt.Sprint(visErr, vis.Result), "principal <entry> must be") {
		t.Fatalf("visibility approval = %s %q, %v", vis.Status, vis.Result, visErr)
	}

	// An approved setting: a request whose frozen target a hand changed in flats.db fails at step 1 of rule 20.
	req, err := h.svc.RequestSignIn(ctx, "app", credential("guest-7Qx"), core.ViaCLI)
	if err != nil || req.Approval == nil {
		t.Fatalf("request = %+v, %v", req, err)
	}
	var params map[string]any
	if err := json.Unmarshal(req.Approval.Params, &params); err != nil {
		t.Fatal(err)
	}
	params["to"] = store.SignIn{Provider: "oidc", Issuer: "https://idp.example", ClientID: "flats-app", Allow: []string{leak}}
	raw, _ := json.Marshal(params)
	db, err := sql.Open("sqlite", filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE approvals SET params=? WHERE id=?`, string(raw), req.Approval.ID); err != nil {
		t.Fatal(err)
	}
	db.Close()
	si, siErr := h.svc.Decide(ctx, req.Approval.ID, true)
	if si.Status != "failed" || !strings.Contains(fmt.Sprint(siErr, si.Result), "the sign-in gate refuses the approved setting: principal <entry> must be") {
		t.Fatalf("sign-in approval = %s %q, %v", si.Status, si.Result, siErr)
	}

	texts := map[string]string{"the visibility approval": fmt.Sprint(visErr, vis.Result), "the sign-in approval": fmt.Sprint(siErr, si.Result),
		"the Portal endpoint": detail, "the host log": h.logs.all()}
	events, err := h.svc.Events(ctx, "app", "", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		texts["a flat event"] += e.Message + string(e.Data) + "\n"
	}
	apiH := (&api.Server{Svc: h.svc}).Handler()
	for _, p := range []string{"/api/flats/app", "/api/flats", "/api/approvals", "/api/approvals/" + vis.ID, "/api/approvals/" + si.ID, "/api/flats/app/logs?limit=1000"} {
		texts["remote GET "+p] = remoteAPI(t, apiH, p)
	}
	var answer struct{ Result string }
	if err := json.Unmarshal([]byte(texts["remote GET /api/approvals/"+si.ID]), &answer); err != nil || !strings.Contains(answer.Result, "principal <entry> must be") {
		t.Fatalf("the approval answer lacks the hidden reason: %q, %v", answer.Result, err)
	}
	local, remote := mcpSessions(t, h)
	for name, cs := range map[string]*mcp.ClientSession{"local": local, "remote": remote} {
		texts[name+" get_flat"] = mcpText(t, cs, "get_flat", map[string]any{"slug": "app"})
		texts[name+" list_flats"] = mcpText(t, cs, "list_flats", map[string]any{})
		texts[name+" get_approval of the visibility"] = mcpText(t, cs, "get_approval", map[string]any{"id": vis.ID})
		texts[name+" get_approval of the sign-in"] = mcpText(t, cs, "get_approval", map[string]any{"id": si.ID})
	}
	texts["get_logs"] = mcpText(t, local, "get_logs", map[string]any{"slug": "app", "limit": 1000})
	for what, text := range texts {
		if strings.Contains(text, leak) {
			t.Errorf("%s holds the entry: %s", what, text)
		}
	}
}

// B7 of the completion criteria at full stack: a host that stopped after step 2 of rule 20 (the setting, the sequence,
// the audit row and the receipt committed, the approval still applying) gets, at the next start, a gate from the
// stored setting, and the approval ends as approved with one change and one audit row, without a webhook event.
func TestSignInStopAfterTheCommitAtFullStack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	h := startHost(t, dir)
	deployServer(t, h, "app")
	publicOnPortal(t, h, "app")
	r, err := h.svc.RequestSignIn(ctx, "app", credential("guest-7Qx"), core.ViaCLI)
	if err != nil || r.Approval == nil {
		t.Fatalf("request = %+v, %v", r, err)
	}
	id := r.Approval.ID
	h.stop()

	// The state of a host that stopped after step 2, built with the functions of the store: the claim of the console,
	// then the commit of step 2 with its receipt.
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.GetApproval(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		FlatID       string       `json:"flat_id"`
		Seq          int64        `json:"seq"`
		To           store.SignIn `json:"to"`
		PublicGrants []string     `json:"public_grants"`
	}
	if err := json.Unmarshal(a.Params, &p); err != nil {
		t.Fatal(err)
	}
	if err := st.ClaimApprovalAuthorized(ctx, id, "console", time.Now()); err != nil {
		t.Fatal(err)
	}
	receipt, _ := json.Marshal(core.ApprovalExecution{Status: "applied", DataImpact: "none", HealthData: "not_run", LiveData: "untouched"})
	if _, err := st.CommitSignIn(ctx, store.SignInCommit{Flat: "app", FlatID: p.FlatID, Seq: p.Seq, Grants: p.PublicGrants, To: p.To,
		Approval: id, Receipt: receipt, Audit: store.Audit{Via: store.AuditViaConsole}}); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.GetApproval(ctx, id); a.Status != "applying" {
		t.Fatalf("the approval before the start = %+v", a)
	}
	st.Close()

	h = startHost(t, dir)
	got, err := h.svc.GetApproval(ctx, id)
	if err != nil || got.Status != "approved" || got.Result != "sign-in set to credential with 1 entry" {
		t.Fatalf("after the start: %+v, %v", got, err)
	}
	if v, _ := h.svc.GetSignIn(ctx, "app"); v.Provider != "credential" || v.Seq != 1 || !slices.Equal(v.Allow, []string{"guest-7Qx"}) {
		t.Fatalf("the stored change = %+v", v)
	}
	if rows := auditRows(t, dir, store.AuditSignInSet); len(rows) != 1 {
		t.Fatalf("signin.set rows = %+v", rows)
	}
	if ps := h.notes.signIn(); len(ps) != 0 {
		t.Fatalf("the start sent %+v", ps)
	}
	// The gate of the stored setting: a visitor without a session goes to the sign-in page, and a link of the stored
	// allowlist opens the flat.
	exp := h.px.Of(t, "app")
	exp.SetRelays(portal.ReadyRelay("relay.test", appHost))
	wantSignInPage(t, portal.NewTestVisitor(exp.Addr(), appHost), "/")
	guest := portal.NewTestVisitor(exp.Addr(), appHost)
	if code := guest.Redeem(t, portal.CredentialOf(t, issue(t, h, "app", "guest-7Qx").URL)); code != http.StatusOK {
		t.Fatalf("redeem = %d", code)
	}
	wantPage(t, guest, "/", "v1 /")
}

// B8 of the completion criteria: an asset of a document flat passes the same gate and the same check as a page
// (rule 47).
func TestSignInDocumentAssetBehindTheGate(t *testing.T) {
	ctx := context.Background()
	h := startHost(t, t.TempDir())
	const notesHost = "notes-0xabc.relay.test"
	if _, err := h.svc.SaveVersion(ctx, "notes", []bundle.File{{Path: "flats.json", Data: []byte(`{"type":"docs","name":"Notes"}`)},
		{Path: "index.md", Data: []byte("# Notes")}, {Path: "images/a.txt", Data: []byte("asset-body-3Kv")}}, core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	// A save of a document applies at once.
	if f, err := h.svc.GetFlat(ctx, "notes"); err != nil || f.LiveVersion == 0 {
		t.Fatalf("the document = %+v, %v", f, err)
	}
	approveSignIn(t, h, "notes", credential("reader-5Tn"))
	publicOnPortal(t, h, "notes")
	exp := h.px.Of(t, "notes")
	exp.SetRelays(portal.ReadyRelay("relay.test", notesHost))
	// Without a sign-in the gate refuses the asset.
	anon := portal.NewTestVisitor(exp.Addr(), notesHost)
	wantSignInPage(t, anon, "/images/a.txt")
	// A visitor of the stored allowlist gets it from the docs handler.
	reader := portal.NewTestVisitor(exp.Addr(), notesHost)
	if code := reader.Redeem(t, portal.CredentialOf(t, issue(t, h, "notes", "reader-5Tn").URL)); code != http.StatusOK {
		t.Fatalf("redeem = %d", code)
	}
	wantPage(t, reader, "/images/a.txt", "asset-body-3Kv")
}

// The provider manager stops the Portal route that FailPortalGate names as servePortal stops one whose gate cannot be
// built (rule 35): the route unregisters, the endpoint shows the reason, the other routes stay, and the next open
// serves it again.
func TestManagerFailPortalGate(t *testing.T) {
	ctx := context.Background()
	h := startHost(t, t.TempDir())
	deployServer(t, h, "app")
	publicOnPortal(t, h, "app")
	approveSignIn(t, h, "app", credential("guest-7Qx"))
	exp := h.px.Of(t, "app")
	exp.SetRelays(portal.ReadyRelay("relay.test", appHost))
	wantSignInPage(t, portal.NewTestVisitor(exp.Addr(), appHost), "/")
	if err := h.mgr.FailPortalGate(ctx, "other", "unused"); err != nil {
		t.Fatalf("a slug without a Portal route = %v", err)
	}
	err := h.mgr.FailPortalGate(ctx, "app", "the routes of app cannot be read")
	if ge := (*core.SignInGateError)(nil); !errors.As(err, &ge) || ge.StopErr != nil {
		t.Fatalf("FailPortalGate = %v", err)
	}
	if exp.Closes() != 1 {
		t.Fatal("the route did not stop and unregister")
	}
	// The manager forgets the stopped route, so no later stop waits for it.
	if routed, err := h.mgr.HasProviderRoute(ctx, "app", provider.Portal); err != nil || routed {
		t.Fatalf("the manager still has the Portal route: %v, %v", routed, err)
	}
	fv, err := h.svc.GetFlat(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	var portalEP, localEP *core.ExposureEndpoint
	for i, ep := range fv.Endpoints {
		switch ep.Provider {
		case core.ProviderPortal:
			portalEP = &fv.Endpoints[i]
		case core.ProviderLocal:
			localEP = &fv.Endpoints[i]
		}
	}
	if portalEP == nil || portalEP.State != "error" || portalEP.Detail != "sign-in gate: the routes of app cannot be read" {
		t.Fatalf("Portal endpoint = %+v", portalEP)
	}
	if localEP == nil || localEP.State != "ready" {
		t.Fatalf("Local endpoint = %+v", localEP)
	}
	wantPage(t, privateVisitor(t, h, "app"), "/", "v1 /")
	// A sign-in change opens it again.
	approveSignIn(t, h, "app", credential("guest-7Qx", "visitor-9Kz"))
	if h.px.Count() != 2 {
		t.Fatalf("exposures = %d, want a new one", h.px.Count())
	}
}
