package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/portal-tunnel/v2/portal/identity"
	"github.com/gosuda/portal-tunnel/v2/sdk/appauth"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/portal-sign-in.md on the routes of a flat: the other public providers stay closed (rule 10), the check of
// each request (rule 44), the headers (rules 45 and 46) and the new token secret of a rename and a deletion (rules 41
// and 42). The gate itself is the Portal provider's; internal/expose/portal tests it with this service.

// signInRouteNet is lifecycleRouteNet with the sign-in part of the Portal route. It records the opens and lets a test
// fail the write of a new token secret.
type signInRouteNet struct {
	*lifecycleRouteNet
	mu       sync.Mutex
	reqs     []ExposureRequest
	file     bool  // the slug has an identity file
	renewErr error // the write of a new token secret fails
	renewed  []string
}

func newSignInRouteNet() *signInRouteNet {
	return &signInRouteNet{lifecycleRouteNet: newLifecycleRouteNet()}
}

func (n *signInRouteNet) ServeExposure(ctx context.Context, req ExposureRequest) (ExposureResult, error) {
	n.mu.Lock()
	n.reqs = append(n.reqs, req)
	n.mu.Unlock()
	return n.lifecycleRouteNet.ServeExposure(ctx, req)
}

func (n *signInRouteNet) RenewPortalIdentity(_ context.Context, slug string) (string, bool, error) {
	path := filepath.Join("portal", slug+".json")
	if n.renewErr != nil {
		return path, false, n.renewErr
	}
	n.renewed = append(n.renewed, slug)
	return path, n.file, nil
}

func (n *signInRouteNet) IssuePortalCredential(context.Context, string, string, time.Time) (PortalCredential, error) {
	return PortalCredential{}, ErrNoPortalRoute
}

func (n *signInRouteNet) PortalHosts(string) []string { return nil }

func (n *signInRouteNet) handler(host string, id ProviderID) http.Handler {
	n.lifecycleRouteNet.mu.Lock()
	defer n.lifecycleRouteNet.mu.Unlock()
	return n.routes[lifecycleRouteKey{host: host, provider: id}]
}

// headerRuntime runs server flats that answer with the headers that reached flat code.
type headerRuntime struct{}

type headerInstance struct{}

func (headerRuntime) Start(context.Context, RuntimeSpec) (Instance, error) {
	return headerInstance{}, nil
}
func (headerInstance) Stop() {}
func (headerInstance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	seen := map[string]string{}
	for k, v := range r.Header {
		if lk := strings.ToLower(k); strings.Contains(lk, "visitor") || strings.Contains(lk, "portal") || lk == "x-flats-access" {
			seen[k] = strings.Join(v, ",")
		}
	}
	json.NewEncoder(w).Encode(seen)
}

// writeSignInRow writes a sign-in setting into flats.db, as a hand-written database would hold it.
func writeSignInRow(t *testing.T, dir, slug, provider string, seq int64, allow ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw, _ := json.Marshal(allow)
	if _, err := db.Exec(`UPDATE flats SET signin_provider=?, signin_allow=?, signin_seq=? WHERE slug=?`, provider, string(raw), seq, slug); err != nil {
		t.Fatal(err)
	}
}

// gated wraps h in a credential gate that marks each request with seq, as the Portal route does, and returns a
// request with a credential that it lets in.
func gated(t *testing.T, h http.Handler, seq int64) (http.Handler, *http.Request) {
	t.Helper()
	id, err := identity.Generate("app")
	if err != nil {
		t.Fatal(err)
	}
	g, err := appauth.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(WithSignInGate(r.Context(), seq)))
	}), id, appauth.Config{Provider: appauth.ProviderCredential, Allow: []string{"guest-7Qx"}})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := appauth.IssueCredential(id, "app.portal.test", "guest-7Qx", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://app.portal.test/headers", nil)
	req.Header.Set("X-Portal-Access-Credential", cred)
	return g, req
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestSignInPortalOnly(t *testing.T) {
	ctx := t.Context()
	s, dir := newTestService(t)
	network := newSignInRouteNet()
	s.cfg.Lifecycle = network
	makeFunnelPublic(t, s, "app", "page")
	if err := s.SetProviderPermission(ctx, "app", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	funnelHandler := network.handler("app", ProviderFunnel)
	if funnelHandler == nil || network.handler("app", ProviderPortal) == nil {
		t.Fatal("the test did not open Funnel and Portal")
	}
	// A flats.db with sign-in on and a Funnel grant, which rule 8 never writes.
	writeSignInRow(t, dir, "app", store.SignInCredential, 1, "guest-7Qx")

	// Rule 10: ensureLifecycle, as at a start, asks no Funnel route; the Portal route carries the setting.
	restarted := newSignInRouteNet()
	s.cfg.Lifecycle = restarted
	f, err := s.st.GetFlat(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ensureLifecycle(ctx, f, restarted); err != nil {
		t.Fatal(err)
	}
	if restarted.serves("app", ProviderFunnel) || !restarted.serves("app", ProviderPortal) {
		t.Fatal("ensureLifecycle asked a Funnel route while sign-in is on")
	}
	for _, req := range restarted.reqs {
		if slices.Contains(req.Permitted, ProviderFunnel) {
			t.Fatalf("a request permits Funnel: %+v", req.Permitted)
		}
		if req.Visibility == "public" && (req.SignIn == nil || req.SignIn.Seq != 1 || req.SignIn.Gate.Provider != "credential") {
			t.Fatalf("the public open carries %v", req.SignIn)
		}
	}
	// A key change and a grant of Funnel open no Funnel route either.
	s.RetryProviderRoutes(ctx, ProviderTailscale, ProviderFunnel)
	if err := s.reapplyExposure(ctx, f, ProviderFunnel, true); err == nil || restarted.serves("app", ProviderFunnel) {
		t.Fatalf("reapplyExposure opened Funnel: %v", err)
	}

	// Rule 44: a request on a route without the gate, here the Funnel route of before, gets 403.
	r := httptest.NewRequest(http.MethodGet, "https://app.funnel.test/", nil)
	rec := serve(funnelHandler, r.WithContext(WithRouteProvider(r.Context(), ProviderFunnel)))
	if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != signInNeededText {
		t.Fatalf("a Funnel request = %d %q", rec.Code, rec.Body)
	}
	portalHandler := restarted.handler("app", ProviderPortal)
	if rec := serve(portalHandler, httptest.NewRequest(http.MethodGet, "https://app.portal.test/", nil)); rec.Code != http.StatusForbidden {
		t.Fatalf("a request without a principal = %d", rec.Code)
	}
	// A request that a gate of another sequence let in gets 503 until the gate of the stored one is in place.
	g, req := gated(t, portalHandler, 0)
	if rec := serve(g, req); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" || strings.TrimSpace(rec.Body.String()) != signInChangingText {
		t.Fatalf("a gate of an older sequence = %d %q %q", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	g, req = gated(t, portalHandler, 1)
	req.URL.Path = "/" // the page of the static flat
	if rec := serve(g, req); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "page") {
		t.Fatalf("a gate of the stored sequence = %d %q", rec.Code, rec.Body)
	}
}

func TestSignInVisitorHeadersOnEveryRoute(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	network := newSignInRouteNet()
	s := newLifecycleService(t, dir, st, network, headerRuntime{})
	t.Cleanup(func() { s.Close() })
	if _, err := s.SaveVersion(ctx, "app", []bundle.File{{Path: "flats.json", Data: []byte(`{"kind":"server"}`)}, {Path: "server.js", Data: []byte("export default {}")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
	if err := s.SetProviderPermission(ctx, "app", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	r, err := s.SetVisibility(ctx, "app", store.Public, ViaConsole, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, r.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	writeSignInRow(t, dir, "app", store.SignInCredential, 0, "guest-7Qx")
	forged := map[string]string{"x-flats-visitor-provider": "oidc", "X-FLATS-VISITOR-SUBJECT": "admin", "X_Flats_Visitor_Email": "boss@example.com",
		"x-portal-user": "admin", "X-Portal-Auth": "oidc", "x_portal_proof": "proof", "X-Portal-Access-Credential": "pcred_forged", "x-flats-visitor_subject": "root"}
	seen := func(h http.Handler, req *http.Request) map[string]string {
		t.Helper()
		for k, v := range forged {
			req.Header[k] = []string{v}
		}
		rec := serve(h, req)
		var m map[string]string
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &m) != nil {
			t.Fatalf("headers = %d %s", rec.Code, rec.Body)
		}
		return m
	}

	// Rule 45 through a gate of the stored sequence; rule 46 for every client copy. The credential header that
	// the client sent is one the gate reads and removes.
	g, req := gated(t, network.handler("app", ProviderPortal), 0)
	cred := req.Header.Get("X-Portal-Access-Credential")
	delete(forged, "X-Portal-Access-Credential")
	got := seen(g, req)
	if want := map[string]string{"X-Flats-Access": "public", "X-Flats-Visitor-Provider": "credential", "X-Flats-Visitor-Subject": "guest-7Qx"}; !maps.Equal(got, want) {
		t.Fatalf("public route: the flat saw %v, want %v", got, want)
	}
	forged["X-Portal-Access-Credential"] = cred
	// The private route keeps the rights of a private visitor, and no copy reaches it (rule 12).
	if got := seen(network.handler("app", ProviderLocal), httptest.NewRequest(http.MethodGet, "http://app.localhost/", nil)); !maps.Equal(got, map[string]string{"X-Flats-Access": "private"}) {
		t.Fatalf("private route: the flat saw %v", got)
	}
	// Rule 48: a preview has no public route, also with sign-in on and Portal granted, and gets no header of rule 45.
	network.mu.Lock()
	network.reqs = nil
	network.mu.Unlock()
	p, err := s.OpenPreview(ctx, "app", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range network.reqs {
		if req.Visibility != "private" || slices.Contains(req.Permitted, ProviderPortal) || req.SignIn != nil {
			t.Fatalf("the preview asked %s %v", req.Visibility, req.Permitted)
		}
	}
	if got := seen(network.handler(p.Host, ProviderLocal), httptest.NewRequest(http.MethodGet, "http://"+p.Host+".localhost/", nil)); !maps.Equal(got, map[string]string{"X-Flats-Access": "private"}) {
		t.Fatalf("preview: the flat saw %v", got)
	}
	// The host operations of the docs worker go through withRouteHeaders, as every route that runs flat code does.
	op := httptest.NewRequest(http.MethodPost, "http://docs.internal/_docs/host/revisions/list", nil)
	for k, v := range forged {
		op.Header[k] = []string{v}
	}
	op = withRouteHeaders(op, false)
	for k := range op.Header {
		if lk := strings.ToLower(k); strings.Contains(lk, "visitor") || strings.Contains(lk, "portal") {
			t.Fatalf("a host operation keeps %s", k)
		}
	}
}

func TestSignInRenewalBeforeARenameAndADeletion(t *testing.T) {
	ctx := t.Context()
	s, dir := newTestService(t)
	network := newSignInRouteNet()
	s.cfg.Lifecycle = network
	lifecycleSave(t, s, "app", "page")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
	network.file = true

	// Rules 41 and 42: when the write fails, the rename and the deletion fail, change nothing and name the file.
	network.renewErr = errors.New("permission denied")
	if _, err := s.RenameSlug(ctx, "app", "moved", ViaCLI); err == nil || !strings.Contains(err.Error(), filepath.Join("portal", "app.json")) || !strings.Contains(err.Error(), "changed nothing") {
		t.Fatalf("rename = %v", err)
	}
	if _, err := s.st.GetFlat(ctx, "app"); err != nil {
		t.Fatalf("the failed rename moved the flat: %v", err)
	}
	if _, err := s.storagePath("app"); err != nil {
		t.Fatal(err)
	}
	if !network.serves("app", ProviderLocal) {
		t.Fatal("the failed rename stopped the route of the flat")
	}
	if _, err := s.Delete(ctx, "app", ViaConsole, ""); err == nil || !strings.Contains(err.Error(), filepath.Join("portal", "app.json")) {
		t.Fatalf("delete = %v", err)
	}
	if _, err := s.st.GetFlat(ctx, "app"); err != nil || !network.serves("app", ProviderLocal) {
		t.Fatalf("the failed deletion changed the flat: %v", err)
	}
	if rows := signInRows(t, s, dir, store.AuditSignInRevoke); len(rows) != 0 {
		t.Fatalf("rows = %+v", rows)
	}

	// The write passes: the row is in the change, with its via, and no webhook event is sent.
	network.renewErr = nil
	var events recorder
	s.SetNotifier(&events)
	if _, err := s.RenameSlug(ctx, "app", "moved", ViaCLI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(ctx, "moved", ViaConsole, ""); err != nil {
		t.Fatal(err)
	}
	rows := signInRows(t, s, dir, store.AuditSignInRevoke)
	if len(rows) != 2 || rows[0].Flat != "app" || rows[0].Via != "cli" || string(rows[0].Data) != `{"cause":"rename"}` ||
		rows[1].Flat != "moved" || rows[1].Via != "console" || string(rows[1].Data) != `{"cause":"delete"}` {
		t.Fatalf("rows = %+v", rows)
	}
	if !slices.Equal(network.renewed, []string{"app", "moved"}) {
		t.Fatalf("renewed = %q", network.renewed)
	}
	for _, p := range events.take() {
		if p.Type == eventSignInChange {
			t.Fatalf("a rename or a deletion sent %s", p.Operation)
		}
	}

	// Without an identity file there is no row (rule 42: "when the identity file exists").
	network.file = false
	lifecycleSave(t, s, "plain", "page")
	if _, err := s.Delete(ctx, "plain", ViaConsole, ""); err != nil {
		t.Fatal(err)
	}
	if rows := signInRows(t, s, dir, store.AuditSignInRevoke); len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
}

// A deletion that an agent approved writes its signin.revoke row with the via autopilot and the text of the agent as
// its actor, as visibility.set does (docs/spec/audit-events.md, rule 5); the operator's deletion keeps the console.
func TestSignInRevokeRowOfAnAgentDeletion(t *testing.T) {
	ctx := t.Context()
	network := newSignInRouteNet()
	s, _ := newAutopilotServiceWith(t, time.Time{}, network)
	network.file = true
	lifecycleSave(t, s, "app", "page")
	allowAutopilot(t, s, OpDeleteFlat)
	id := resultID(t)(s.Delete(ctx, "app", ViaMCP, ""))
	if a, err := s.DecideAsAgent(ctx, id, true, bot); err != nil || a.Status != "approved" {
		t.Fatalf("decide = %+v, %v", a, err)
	}
	lifecycleSave(t, s, "mine", "page")
	if _, err := s.Delete(ctx, "mine", ViaConsole, ""); err != nil {
		t.Fatal(err)
	}
	rows := signInRows(t, s, s.cfg.DataDir, store.AuditSignInRevoke)
	if len(rows) != 2 || rows[0].Flat != "app" || rows[0].Via != "autopilot" || rows[0].Actor != botText ||
		string(rows[0].Data) != `{"cause":"delete"}` || rows[1].Flat != "mine" || rows[1].Via != "console" || rows[1].Actor != "" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestSignInRevokeWithoutAnIdentity(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestService(t)
	network := newSignInRouteNet()
	s.cfg.Lifecycle = network
	if _, err := s.CreateFlat(ctx, "app", "", ViaCLI); err != nil {
		t.Fatal(err)
	}
	// Rule 54: a flat without a Portal identity file.
	r, err := s.RevokeSignIn(ctx, "app", ViaAPI)
	if err != nil || r.Status != "done" || r.Message != "no Portal identity" {
		t.Fatalf("revoke = %+v, %v", r, err)
	}
	// A revocation leaves a pending set_signin approval valid (rule 18).
	a := requestSignIn(t, s, "app", guestRequest)
	network.file = true
	if r, err := s.RevokeSignIn(ctx, "app", ViaConsole); err != nil || r.Status != "done" {
		t.Fatalf("revoke = %+v, %v", r, err)
	}
	if d, err := s.Decide(ctx, a.ID, true); err != nil || d.Status != "approved" {
		t.Fatalf("the approval after a revocation = %+v, %v", d, err)
	}
	if _, err := s.RevokeSignIn(ctx, "app", ViaMCP); !errors.Is(err, ErrForbidden) {
		t.Fatalf("MCP = %v", err)
	}
}
