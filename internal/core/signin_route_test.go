package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// signInRouteNet is lifecycleRouteNet with the sign-in part of the Portal route. It records the opens and the stops of
// rule 35, and lets a test fail the write of a new token secret and the read of the routes, and run a function at
// each of those two calls.
type signInRouteNet struct {
	*lifecycleRouteNet
	mu        sync.Mutex
	reqs      []ExposureRequest
	file      bool  // the slug has an identity file
	renewErr  error // the write of a new token secret fails
	statusErr error // ExposureStatus fails
	renewed   []string
	failed    []string // the reasons of FailPortalGate
	onStatus  func()   // runs at each ExposureStatus
	onRenew   func()   // runs after each write of a new token secret
}

func (n *signInRouteNet) ExposureStatus(ctx context.Context, host string) (ExposureResult, error) {
	if n.onStatus != nil {
		n.onStatus()
	}
	if n.statusErr != nil {
		return ExposureResult{}, n.statusErr
	}
	return n.lifecycleRouteNet.ExposureStatus(ctx, host)
}

// FailPortalGate stops the Portal route of slug, as the provider manager does, when it has one.
func (n *signInRouteNet) FailPortalGate(ctx context.Context, slug, reason string) error {
	if !n.serves(slug, ProviderPortal) {
		return nil
	}
	n.mu.Lock()
	n.failed = append(n.failed, reason)
	n.mu.Unlock()
	return &SignInGateError{Reason: reason, StopErr: n.StopProviderRoutes(ctx, slug, ProviderPortal)}
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
	if n.onRenew != nil {
		n.onRenew()
	}
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
	// A sequence below 0 lets the request in with a principal but without the mark of a Flats gate.
	g, err := appauth.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seq >= 0 {
			r = r.WithContext(WithSignInGate(r.Context(), seq))
		}
		h.ServeHTTP(w, r)
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
	// A principal that no Flats gate marked is no sign-in of this flat: 403, not the 503 of a changing setting.
	g, req = gated(t, portalHandler, -1)
	if rec := serve(g, req); rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != signInNeededText {
		t.Fatalf("a principal without the gate mark = %d %q", rec.Code, rec.Body)
	}

	// Rule 10 also when the host serves the public routes again (rule 54 here) and when a rename serves the
	// redirect of the old slug: the Funnel route that is open from before is not served again.
	s.cfg.Lifecycle = network
	network.file = true
	before := len(network.reqs)
	if _, err := s.RevokeSignIn(ctx, "app", ViaCLI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameSlug(ctx, "app", "moved", ViaCLI); err != nil {
		t.Fatal(err)
	}
	reopened, redirect := false, false
	for _, req := range network.reqs[before:] {
		if req.Visibility != "public" {
			continue
		}
		if slices.Contains(req.Permitted, ProviderFunnel) {
			t.Fatalf("a public open after sign-in permits Funnel: host %s, %+v", req.Host, req.Permitted)
		}
		reopened = reopened || (req.Host == "app" && req.SignIn != nil && !req.SignIn.Redirect)
		redirect = redirect || (req.Host == "app" && req.SignIn != nil && req.SignIn.Redirect)
	}
	if !reopened || !redirect {
		t.Fatalf("the test did not reach both opens: reopened %t, redirect %t", reopened, redirect)
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

// A rename and a deletion that fail after the new token secret, here because a preview route does not stop, leave the
// flat as it was, but its sessions and links ended: the signin.revoke row of the cause, a flat event and the error
// say so (rules 41, 42 and 67).
func TestSignInRenewalOutlivesAFailedRenameAndDeletion(t *testing.T) {
	ctx := t.Context()
	s, dir := newTestService(t)
	network := newSignInRouteNet()
	s.cfg.Lifecycle = network
	lifecycleSave(t, s, "app", "page")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
	network.file = true
	p, err := s.OpenPreview(ctx, "app", 1)
	if err != nil {
		t.Fatal(err)
	}
	network.stopExposure[p.Host] = errors.New("preview route still reachable")
	const ended = "every sign-in session and access link of app ended: its Portal identity got a new token secret before the "
	if _, err := s.RenameSlug(ctx, "app", "moved", ViaCLI); err == nil || !strings.Contains(err.Error(), "close previews before rename") || !strings.Contains(err.Error(), ended+"rename failed") {
		t.Fatalf("rename = %v", err)
	}
	if _, err := s.Delete(ctx, "app", ViaConsole, ""); err == nil || !strings.Contains(err.Error(), "stop previews") || !strings.Contains(err.Error(), ended+"deletion failed") {
		t.Fatalf("delete = %v", err)
	}
	if _, err := s.st.GetFlat(ctx, "app"); err != nil || !network.serves("app", ProviderLocal) {
		t.Fatalf("the failed changes changed the flat: %v", err)
	}
	if !slices.Equal(network.renewed, []string{"app", "app"}) {
		t.Fatalf("renewed = %q", network.renewed)
	}
	rows := signInRows(t, s, dir, store.AuditSignInRevoke)
	if len(rows) != 2 || rows[0].Flat != "app" || rows[0].Via != "cli" || string(rows[0].Data) != `{"cause":"rename"}` ||
		rows[1].Flat != "app" || rows[1].Via != "console" || string(rows[1].Data) != `{"cause":"delete"}` {
		t.Fatalf("rows = %+v", rows)
	}
	evs, err := s.st.ListEvents(ctx, "app", "signin", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var warned []string
	for _, e := range evs {
		if e.Level == "warn" && strings.HasPrefix(e.Message, ended) {
			warned = append(warned, strings.TrimPrefix(e.Message, ended))
		}
	}
	slices.Sort(warned)
	if !slices.Equal(warned, []string{"deletion failed", "rename failed"}) {
		t.Fatalf("signin events = %+v", evs)
	}
	// A flat without an identity file writes nothing more.
	network.file = false
	if _, err := s.RenameSlug(ctx, "app", "moved", ViaCLI); err == nil || strings.Contains(err.Error(), "ended") {
		t.Fatalf("rename without an identity file = %v", err)
	}
	if rows := signInRows(t, s, dir, store.AuditSignInRevoke); len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
}

// When reopenPublic cannot read what the gate needs, the Portal route stops as rule 35 stops it, so no gate of a
// removed client secret or of an old token secret stays in front, and each message says the state of the routes
// (rules 28 and 54).
func TestSignInReopenThatCannotRead(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestService(t)
	network := newSignInRouteNet()
	network.file = true
	s.cfg.Lifecycle = network
	for _, slug := range []string{"app", "other"} {
		publicLifecycleFlat(t, s, slug, store.ProviderPortal)
		approveSignIn(t, s, slug, oidcRequest)
		if !network.serves(slug, ProviderPortal) {
			t.Fatalf("%s has no Portal route", slug)
		}
	}
	publicLifecycleFlat(t, s, "open", store.ProviderFunnel)
	if _, err := s.SetSignInSecret(ctx, "app", signInValue, "https://idp.example.com", "flats-app", ViaCLI); err != nil {
		t.Fatal(err)
	}
	network.statusErr = errors.New("status-unreadable")
	const reason = "sign-in gate: the routes of %s cannot be read to build its gate again: read current routes: status-unreadable"

	// Rule 28: a removal of the client secret stops the Portal route, whose gate held the removed value.
	r, err := s.DeleteSignInSecret(ctx, "app", ViaCLI)
	if err != nil || !strings.Contains(r.Message, "; the Portal route of app is stopped: "+fmt.Sprintf(reason, "app")) {
		t.Fatalf("removal = %+v, %v", r, err)
	}
	if network.serves("app", ProviderPortal) || !network.serves("app", ProviderLocal) {
		t.Fatal("the Portal route of app stayed, or another route stopped")
	}
	evs, _ := s.st.ListEvents(ctx, "app", "signin", 0, 100)
	if len(evs) == 0 || evs[len(evs)-1].Level != "error" || !strings.HasPrefix(evs[len(evs)-1].Message, "the Portal route of app is stopped: "+fmt.Sprintf(reason, "app")) {
		t.Fatalf("signin events = %+v", evs)
	}

	// Rule 54: a revocation stops the Portal route, and its other public routes answer 503 on the cancelled epoch.
	r, err = s.RevokeSignIn(ctx, "other", ViaCLI)
	if err != nil || !strings.Contains(r.Message, "; "+SignInRoutesNotReopened+": the Portal route of other is stopped: "+fmt.Sprintf(reason, "other")) ||
		!strings.Contains(r.Message, "any other public route of other answers 503 until a deploy, a grant or a restart serves it again") {
		t.Fatalf("revoke = %+v, %v", r, err)
	}
	if network.serves("other", ProviderPortal) {
		t.Fatal("the Portal route of other stayed")
	}
	// A flat without a Portal route has nothing to stop, and the message says what its public routes do.
	r, err = s.RevokeSignIn(ctx, "open", ViaCLI)
	if err != nil || !strings.Contains(r.Message, "; "+SignInRoutesNotReopened+": the routes of open cannot be read: read current routes: status-unreadable; it has no Portal route, and its public routes answer 503") ||
		strings.Contains(r.Message, "is stopped") {
		t.Fatalf("revoke without a Portal route = %+v, %v", r, err)
	}
	if code := network.status("open", ProviderFunnel); code != http.StatusServiceUnavailable {
		t.Fatalf("the Funnel route of open = %d, want 503", code)
	}
	if len(network.failed) != 2 {
		t.Fatalf("FailPortalGate = %q", network.failed)
	}
}

// A flat that cannot be read while its Portal route is served stops that route as rule 35 does, at the rebuild of rule
// 28 and at the open of the public routes after a sign-in change or a revocation (rules 20 and 54): no gate of an
// earlier setting, client secret or token secret stays in front.
func TestSignInFlatThatCannotBeRead(t *testing.T) {
	for name, call := range map[string]func(context.Context, *Service) error{
		"the rebuild of rule 28": func(ctx context.Context, s *Service) error { return s.signInSecretChanged(ctx, "app") },
		"the open of the public routes": func(ctx context.Context, s *Service) error {
			return s.reopenPublic(ctx, "app", false)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, dir := newTestService(t)
			network := newSignInRouteNet()
			s.cfg.Lifecycle = network
			publicLifecycleFlat(t, s, "app", store.ProviderPortal)
			approveSignIn(t, s, "app", oidcRequest)
			if !network.serves("app", ProviderPortal) {
				t.Fatal("app has no Portal route")
			}
			// The fake net still serves the routes of the flat whose row is gone.
			if _, err := rawDB(t, dir).Exec(`DELETE FROM flats WHERE slug='app'`); err != nil {
				t.Fatal(err)
			}
			err := call(t.Context(), s)
			stop := `sign-in gate: the routes of app cannot be read to build its gate again: flat "app": ` + store.ErrNotFound.Error()
			if ge := (*SignInGateError)(nil); !errors.As(err, &ge) || ge.StopErr != nil || !strings.HasPrefix(err.Error(), stop) {
				t.Fatalf("%s = %v, want the stop of the Portal route", name, err)
			}
			if len(network.failed) != 1 || network.serves("app", ProviderPortal) || !network.serves("app", ProviderLocal) {
				t.Fatalf("FailPortalGate = %q: the Portal route stayed, or another route stopped", network.failed)
			}
			if text := routesText("app", err, false); !strings.HasPrefix(text, "the Portal route of app is stopped: "+stop) {
				t.Fatalf("the message says %q", text)
			}
		})
	}
}

// A client that drops its request after the commit leaves the Portal route in service with a gate of the stored
// setting: the steps after a set and a removal of the client secret (rule 28), a revocation (rule 54) and an approved
// change (rule 20) do not depend on the request. The fake net cancels the request at its first call after the commit.
func TestSignInRequestCancelledAfterTheCommit(t *testing.T) {
	ctx := t.Context()
	s, dir := newTestService(t)
	network := newSignInRouteNet()
	network.file = true
	s.cfg.Lifecycle = network
	publicLifecycleFlat(t, s, "app", store.ProviderPortal)
	approveSignIn(t, s, "app", oidcRequest)
	// request returns a request context that the next call of hook cancels, and a check that the call came.
	request := func(hook *func()) (context.Context, func()) {
		req, cancel := context.WithCancel(ctx)
		*hook = func() { *hook = nil; cancel() }
		return req, func() {
			t.Helper()
			if req.Err() == nil {
				t.Fatal("the test did not cancel the request after the commit")
			}
		}
	}
	// served checks that nothing stopped the Portal route of app, and that its last open carries the gate of the
	// stored setting, with secret as its client secret.
	served := func(what, secret string) {
		t.Helper()
		st, err := s.st.GetSignIn(ctx, "app")
		if err != nil {
			t.Fatal(err)
		}
		if len(network.failed) != 0 || !network.serves("app", ProviderPortal) {
			t.Fatalf("%s stopped the Portal route: %q", what, network.failed)
		}
		network.mu.Lock()
		last := network.reqs[len(network.reqs)-1]
		network.mu.Unlock()
		if si := last.SignIn; last.Visibility != "public" || si == nil || si.Err != nil || si.Seq != st.Seq ||
			si.Gate.Provider != st.SignIn.Provider || si.Gate.ClientSecret != secret {
			t.Fatalf("%s: the last open carries %v, want the gate of sequence %d", what, last.SignIn, st.Seq)
		}
	}

	req, cancelled := request(&network.onStatus)
	r, err := s.SetSignInSecret(req, "app", signInValue, "https://idp.example.com", "flats-app", ViaCLI)
	cancelled()
	if err != nil || r.Message != "sign-in client secret set via cli for issuer https://idp.example.com and client ID flats-app" {
		t.Fatalf("set = %+v, %v", r, err)
	}
	served("the set", signInValue)
	req, cancelled = request(&network.onStatus)
	r, err = s.DeleteSignInSecret(req, "app", ViaCLI)
	cancelled()
	if err != nil || r.Message != "sign-in client secret removed via cli" {
		t.Fatalf("removal = %+v, %v", r, err)
	}
	served("the removal", "")

	// The write of the new token secret is the commit of a revocation; its audit row is written after it.
	req, cancelled = request(&network.onRenew)
	r, err = s.RevokeSignIn(req, "app", ViaCLI)
	cancelled()
	if err != nil || r.Message != "every sign-in session and access link of app ended via cli" {
		t.Fatalf("revoke = %+v, %v", r, err)
	}
	if rows := signInRows(t, s, dir, store.AuditSignInRevoke); len(rows) != 1 {
		t.Fatalf("signin.revoke rows = %+v", rows)
	}
	served("the revocation", "")

	// The final write of the approval uses the request, so the approval stays applying, and the next start ends it
	// from the stored setting (rule 21).
	a := requestSignIn(t, s, "app", guestRequest)
	req, cancelled = request(&network.onStatus)
	_, err = s.Decide(req, a.ID, true)
	cancelled()
	if got, _ := s.GetApproval(ctx, a.ID); !errors.Is(err, context.Canceled) || got.Status != "applying" {
		t.Fatalf("decide = %v, then %+v", err, got)
	}
	served("the approved change", "")
	const result = "sign-in set to credential with 2 entries"
	if evs, _ := s.st.ListEvents(ctx, "app", "signin", 0, 100); len(evs) == 0 || evs[len(evs)-1].Message != result {
		t.Fatalf("signin events = %+v", evs)
	}
	s.resumeApplying(ctx)
	if got, _ := s.GetApproval(ctx, a.ID); got.Status != "approved" || got.Result != result {
		t.Fatalf("after the next start: %+v", got)
	}
	served("the resume", "")
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

// A revocation whose write of the new token secret fails ends nothing and writes no signin.revoke row, because the row
// follows the write of the file (rule 67); the next revocation that writes it writes the row.
func TestSignInRevokeWithAFailedWrite(t *testing.T) {
	ctx := t.Context()
	s, dir := newTestService(t)
	network := newSignInRouteNet()
	s.cfg.Lifecycle = network
	network.file = true
	if _, err := s.CreateFlat(ctx, "app", "", ViaCLI); err != nil {
		t.Fatal(err)
	}
	var events recorder
	s.SetNotifier(&events)
	network.renewErr = errors.New("permission denied")
	_, err := s.RevokeSignIn(ctx, "app", ViaCLI)
	if err == nil || !strings.Contains(err.Error(), "did not end") || !strings.Contains(err.Error(), filepath.Join("portal", "app.json")) {
		t.Fatalf("revoke = %v", err)
	}
	if rows := signInRows(t, s, dir, store.AuditSignInRevoke); len(rows) != 0 {
		t.Fatalf("a failed write wrote rows: %+v", rows)
	}
	for _, p := range events.take() {
		if p.Type == eventSignInChange {
			t.Fatalf("a failed revocation sent %s", short(p))
		}
	}
	network.renewErr = nil
	if r, err := s.RevokeSignIn(ctx, "app", ViaCLI); err != nil || r.Status != "done" {
		t.Fatalf("revoke = %+v, %v", r, err)
	}
	if rows := signInRows(t, s, dir, store.AuditSignInRevoke); len(rows) != 1 || rows[0].Via != "cli" || string(rows[0].Data) != "{}" {
		t.Fatalf("rows = %+v", rows)
	}
}
