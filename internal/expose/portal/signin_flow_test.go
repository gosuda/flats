package portal_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/portal-tunnel/v2/portal/identity"
	"github.com/gosuda/portal-tunnel/v2/sdk/appauth"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/portal"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/webhook"
)

// docs/spec/portal-sign-in.md through the host: core, the provider manager and a Portal Net whose exposures are
// loopback listeners. A server flat runs in the process.

const (
	appHost = "app-0xabc.relay.test"
	newHost = "new-0xdef.relay.test"
)

// flatRuntime runs server flats in the process: a page names its version and path, /headers lists the headers that
// reached flat code, and /ws holds an upgraded socket open until its request ends, as the worker proxy does.
type flatRuntime struct{}

func (flatRuntime) Start(_ context.Context, spec core.RuntimeSpec) (core.Instance, error) {
	return flatInstance{version: spec.Version}, nil
}

type flatInstance struct{ version int }

func (flatInstance) Stop() {}

func (in flatInstance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/ws":
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		<-r.Context().Done()
		c.Close()
	case "/headers":
		seen := map[string]string{}
		for k, v := range r.Header {
			if lk := strings.ToLower(k); strings.Contains(lk, "visitor") || strings.Contains(lk, "portal") || lk == "x-flats-access" {
				seen[k] = strings.Join(v, ",")
			}
		}
		json.NewEncoder(w).Encode(seen)
	default:
		fmt.Fprintf(w, "v%d %s", in.version, r.URL.Path)
	}
}

// notes records the webhook events.
type notes struct {
	mu     sync.Mutex
	bodies []webhook.Payload
}

func (n *notes) Notify(p webhook.Payload) {
	n.mu.Lock()
	n.bodies = append(n.bodies, p)
	n.mu.Unlock()
}

func (n *notes) signIn() []webhook.Payload {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []webhook.Payload
	for _, p := range n.bodies {
		if p.Type == "signin.changed" {
			out = append(out, p)
		}
	}
	return out
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logLines) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

type host struct {
	dir   string
	st    *store.Store
	svc   *core.Service
	mgr   *provider.Manager
	loop  *local.Net
	px    *portal.TestExposures
	notes *notes
	logs  *logLines // the log of the service and of the Portal Net
	stop  func()
}

// startHost starts a host on dir with Portal granted to the host. A second start on the same dir is a restart.
func startHost(t *testing.T, dir string) *host {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	loop, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logs := &logLines{}
	pn, px := portal.NewTestNet(t, portal.Config{Dir: filepath.Join(dir, "portal"), Discovery: true, Logf: logs.logf})
	mgr, err := provider.New(dir, provider.Options{Local: loop, Portal: pn, Grants: &provider.File{Permitted: []provider.ID{provider.Portal}},
		Permission: func(ctx context.Context, slug string, id provider.ID) (bool, error) {
			return st.ProviderPermitted(ctx, slug, string(id))
		}})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := core.New(ctx, core.Config{DataDir: dir, Store: st, Private: loop, Lifecycle: mgr, Runtime: flatRuntime{}, DocsApp: docsApp(),
		ConsoleURL: func() string { return "http://console.test" }, Logf: func(format string, args ...any) { t.Logf(format, args...); logs.logf(format, args...) }})
	if err != nil {
		t.Fatal(err)
	}
	h := &host{dir: dir, st: st, svc: svc, mgr: mgr, loop: loop, px: px, notes: &notes{}, logs: logs}
	svc.SetNotifier(h.notes)
	var once sync.Once
	h.stop = func() { once.Do(func() { svc.Close(); mgr.Close(); pn.Close(); loop.Close(); st.Close() }) }
	t.Cleanup(h.stop)
	return h
}

func decide(t *testing.T, h *host, r core.ActionResult, err error) store.Approval {
	t.Helper()
	if err != nil || r.Approval == nil {
		t.Fatalf("request = %+v, %v", r, err)
	}
	a, err := h.svc.Decide(context.Background(), r.Approval.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// deployServer saves, publishes and deploys version n of a server flat.
func deployServer(t *testing.T, h *host, slug string) {
	t.Helper()
	ctx := context.Background()
	f, _ := h.svc.GetFlat(ctx, slug)
	if _, err := h.svc.SaveVersion(ctx, slug, []bundle.File{{Path: "flats.json", Data: []byte(`{"kind":"server"}`)},
		{Path: "server.js", Data: []byte(fmt.Sprintf("export default {} // %d", f.LatestVersion+1))}}, core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	r, err := h.svc.Publish(ctx, slug, 0, "", core.ViaAPI)
	decide(t, h, r, err)
	f, err = h.svc.GetFlat(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.svc.Deploy(ctx, slug, f.LatestVersion, core.ViaAPI)
	var pending *core.PendingApproval
	if !errors.As(err, &pending) {
		t.Fatalf("deploy = %v", err)
	}
	if a, err := h.svc.Decide(ctx, pending.Approval.ID, true); err != nil || a.Status != "approved" {
		t.Fatalf("deploy = %+v, %v", a, err)
	}
}

func publicOnPortal(t *testing.T, h *host, slug string) store.Approval {
	t.Helper()
	ctx := context.Background()
	if err := h.svc.SetProviderPermission(ctx, slug, store.ProviderPortal, true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	r, err := h.svc.SetVisibility(ctx, slug, store.Public, core.ViaConsole, "")
	return decide(t, h, r, err)
}

func approveSignIn(t *testing.T, h *host, slug string, req core.SignInRequest) store.Approval {
	t.Helper()
	r, err := h.svc.RequestSignIn(context.Background(), slug, req, core.ViaCLI)
	a := decide(t, h, r, err)
	if a.Status != "approved" || strings.Contains(a.Result, "stopped") {
		t.Fatalf("sign-in approval = %s: %s", a.Status, a.Result)
	}
	return a
}

func credential(allow ...string) core.SignInRequest {
	return core.SignInRequest{Provider: "credential", Allow: allow}
}

func issue(t *testing.T, h *host, slug, subject string) core.SignInLink {
	t.Helper()
	l, err := h.svc.IssueSignInLink(context.Background(), slug, subject, time.Time{}, core.ViaConsole)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func wantPage(t *testing.T, v *portal.TestVisitor, path, body string) {
	t.Helper()
	if code, got := v.Get(t, path, nil); code != http.StatusOK || got != body {
		t.Fatalf("GET %s on %s = %d %q, want %q", path, v.Host, code, got, body)
	}
}

func wantSignInPage(t *testing.T, v *portal.TestVisitor, path string) {
	t.Helper()
	resp := v.Do(t, http.MethodGet, path, nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/_portal/auth/login") {
		t.Fatalf("GET %s on %s = %d to %q, want the sign-in page", path, v.Host, resp.StatusCode, resp.Header.Get("Location"))
	}
}

// openSocket upgrades /ws as a browser page of origin would, with the cookies of v.
func openSocket(t *testing.T, v *portal.TestVisitor, origin string) (net.Conn, int) {
	t.Helper()
	c, err := net.Dial("tcp", v.Addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	var b strings.Builder
	fmt.Fprintf(&b, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nOrigin: %s\r\n", v.Host, origin)
	for _, ck := range v.Cookies {
		fmt.Fprintf(&b, "Cookie: %s=%s\r\n", ck.Name, ck.Value)
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(c, b.String()); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Time{})
	return c, resp.StatusCode
}

func openSocketOK(t *testing.T, v *portal.TestVisitor) net.Conn {
	t.Helper()
	c, code := openSocket(t, v, "https://"+v.Host)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade on %s = %d", v.Host, code)
	}
	return c
}

func waitClosed(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the open socket did not end: %v", err)
	}
}

// privateVisitor is a visitor of the Local route of slug, which routes by the Host of its loopback URL.
func privateVisitor(t *testing.T, h *host, slug string) *portal.TestVisitor {
	t.Helper()
	fv, err := h.svc.GetFlat(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(fv.PrivateURL)
	if err != nil || u.Port() == "" {
		t.Fatalf("private URL %q", fv.PrivateURL)
	}
	return portal.NewTestVisitor("127.0.0.1:"+u.Port(), u.Host)
}

func auditRows(t *testing.T, dir, kind string) []store.AuditEvent {
	t.Helper()
	rd, err := store.OpenAuditReader(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	rows, err := rd.List(context.Background(), store.AuditQuery{Kind: kind}, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func tokenSecret(t *testing.T, path string) (address, secret string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Address     string `json:"address"`
		TokenSecret string `json:"token_secret"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m.Address, m.TokenSecret
}

// gateOfFile reports whether a credential gate built from the identity file at path lets credential in on host: a
// link of an identity whose token secret changed never does.
func gateOfFile(t *testing.T, path, host, subject, credential string) bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	g, err := appauth.New(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), id, appauth.Config{Provider: "credential", Allow: []string{subject}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/", nil)
	req.Header.Set("X-Portal-Access-Credential", credential)
	rec := &recorder{header: http.Header{}, code: http.StatusOK}
	g.ServeHTTP(rec, req)
	return rec.code == http.StatusOK
}

type recorder struct {
	header http.Header
	code   int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(code int)        { r.code = code }

// noLinkText fails when text holds a link, a credential or one of the subjects.
func noLinkText(t *testing.T, what, text string, links []core.SignInLink, subjects ...string) {
	t.Helper()
	for _, l := range links {
		if strings.Contains(text, l.URL) || strings.Contains(text, strings.TrimPrefix(l.URL, "https://"+l.Host+"/_portal/auth/login#credential=")) {
			t.Errorf("%s holds an access link: %s", what, text)
		}
	}
	if strings.Contains(text, "pcred_") {
		t.Errorf("%s holds a credential: %s", what, text)
	}
	for _, s := range subjects {
		if strings.Contains(text, s) {
			t.Errorf("%s holds the subject %q: %s", what, s, text)
		}
	}
}

func TestSignInOnThePortalRoute(t *testing.T) {
	ctx := context.Background()
	h := startHost(t, t.TempDir())
	deployServer(t, h, "app")
	publicOnPortal(t, h, "app")
	exp := h.px.Of(t, "app")
	exp.SetRelays(portal.ReadyRelay("relay.test", appHost))
	anon := portal.NewTestVisitor(exp.Addr(), appHost)
	wantPage(t, anon, "/", "v1 /")
	open := openSocketOK(t, anon)

	// Rule 20, step 3: sign-in on ends the open connections of the public routes, and a visitor comes back through
	// the gate.
	approveSignIn(t, h, "app", credential("guest-7Qx", "visitor-9Kz"))
	waitClosed(t, open)
	wantSignInPage(t, anon, "/")
	if exp.Closes() != 0 {
		t.Fatal("a sign-in change registered the route again")
	}

	// Rule 51: the console issues a link for the canonical host name, 7 days by default.
	l1, err := h.svc.IssueSignInLink(ctx, "app", " guest-7Qx ", time.Time{}, core.ViaConsole)
	if err != nil {
		t.Fatal(err)
	}
	if l1.Subject != "guest-7Qx" || l1.Host != appHost || l1.ExpiresAt.Sub(time.Now()) < 7*24*time.Hour-time.Minute || l1.ExpiresAt.Sub(time.Now()) > 7*24*time.Hour {
		t.Fatalf("link = %v %s %s", l1, l1.Subject, l1.ExpiresAt)
	}
	guest := portal.NewTestVisitor(exp.Addr(), appHost)
	if code := guest.Redeem(t, portal.CredentialOf(t, l1.URL)); code != http.StatusOK {
		t.Fatalf("redeem = %d", code)
	}
	// Step 4: an allowed visitor gets the flat, not 503; a static file passes the same gate (rule 47).
	wantPage(t, guest, "/", "v1 /")
	wantPage(t, guest, "/style.css", "v1 /style.css")
	wantSignInPage(t, anon, "/style.css")

	// Rules 45 and 46: the headers of the visitor, and no client copy of one, in any spelling.
	forged := http.Header{"x-flats-visitor-subject": {"admin"}, "X_Flats_Visitor_Email": {"boss@example.com"}, "X-FLATS-VISITOR-PROVIDER": {"oidc"},
		"x-portal-user": {"admin"}, "X-Portal-Auth": {"oidc"}, "x_portal_proof": {"proof"}, "x-flats-visitor_subject": {"root"}}
	headers := func(v *portal.TestVisitor) map[string]string {
		t.Helper()
		code, body := v.Get(t, "/headers", forged)
		var m map[string]string
		if code != http.StatusOK || json.Unmarshal([]byte(body), &m) != nil {
			t.Fatalf("headers = %d %s", code, body)
		}
		return m
	}
	if got, want := headers(guest), map[string]string{"X-Flats-Access": "public", "X-Flats-Visitor-Provider": "credential", "X-Flats-Visitor-Subject": "guest-7Qx"}; !maps.Equal(got, want) {
		t.Fatalf("the flat saw %v, want %v", got, want)
	}
	// The private route keeps the rights of a private visitor and never gets a copy either.
	if got := headers(privateVisitor(t, h, "app")); !maps.Equal(got, map[string]string{"X-Flats-Access": "private"}) {
		t.Fatalf("the private route saw %v", got)
	}

	// Rule 47: a same-origin upgrade with a session passes, and a cross-origin one is refused.
	sock := openSocketOK(t, guest)
	if _, code := openSocket(t, guest, "https://elsewhere.example"); code != http.StatusForbidden {
		t.Fatalf("a cross-origin upgrade = %d", code)
	}
	if _, code := openSocket(t, anon, "https://"+appHost); code != http.StatusSeeOther {
		t.Fatalf("an upgrade without a session = %d", code)
	}

	// Rule 39: a visitor who leaves the allowlist loses access at the next request, also on an open connection.
	approveSignIn(t, h, "app", credential("visitor-9Kz"))
	waitClosed(t, sock)
	wantSignInPage(t, guest, "/")
	l2 := issue(t, h, "app", "visitor-9Kz")
	other := portal.NewTestVisitor(exp.Addr(), appHost)
	if code := other.Redeem(t, portal.CredentialOf(t, l2.URL)); code != http.StatusOK {
		t.Fatalf("redeem = %d", code)
	}
	wantPage(t, other, "/", "v1 /")
	// Rule 53: the link of a subject that left fails while the subject stays off the list.
	if code := portal.NewTestVisitor(exp.Addr(), appHost).Redeem(t, portal.CredentialOf(t, l1.URL)); code != http.StatusUnauthorized {
		t.Fatalf("a link of a removed subject = %d", code)
	}

	// A credential of a subject that is not on the list opens no page and no static file.
	if code, _ := portal.NewTestVisitor(exp.Addr(), appHost).Get(t, "/style.css", http.Header{"X-Portal-Access-Credential": {portal.CredentialOf(t, l1.URL)}}); code != http.StatusSeeOther {
		t.Fatalf("a credential of a removed subject = %d", code)
	}

	// Rule 53: when the subject comes back on the allowlist, its earlier link works again, because a credential has
	// no date; the revocation below ends it for good.
	approveSignIn(t, h, "app", credential("guest-7Qx", "visitor-9Kz"))
	back := portal.NewTestVisitor(exp.Addr(), appHost)
	if code := back.Redeem(t, portal.CredentialOf(t, l1.URL)); code != http.StatusOK {
		t.Fatalf("the earlier link of a subject that came back = %d", code)
	}
	wantPage(t, back, "/", "v1 /")
	wantPage(t, other, "/", "v1 /")
	sock = openSocketOK(t, other)

	// A redeploy and a rollback keep the gate and its sessions.
	deployServer(t, h, "app")
	wantPage(t, other, "/", "v2 /")
	wantSignInPage(t, anon, "/")
	_, err = h.svc.RollbackWithData(ctx, "app", 1, false, core.ViaAPI)
	var pending *core.PendingApproval
	if !errors.As(err, &pending) {
		t.Fatalf("rollback = %v", err)
	}
	if a, err := h.svc.Decide(ctx, pending.Approval.ID, true); err != nil || a.Status != "approved" {
		t.Fatalf("rollback = %+v, %v", a, err)
	}
	wantPage(t, other, "/", "v1 /")
	wantSignInPage(t, anon, "/")
	deployServer(t, h, "app")
	wantPage(t, other, "/", "v3 /")

	// Rule 54: a revocation ends every session and link, keeps the host names, and a new link works.
	hosts := h.svc.SignInHosts("app")
	if !slices.Equal(hosts, []string{appHost}) {
		t.Fatalf("hosts = %q", hosts)
	}
	if _, err := h.svc.RevokeSignIn(ctx, "app", core.ViaMCP); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("a revocation through MCP = %v", err)
	}
	r, err := h.svc.RevokeSignIn(ctx, "app", core.ViaCLI)
	if err != nil || r.Status != "done" {
		t.Fatalf("revoke = %+v, %v", r, err)
	}
	waitClosed(t, sock)
	wantSignInPage(t, other, "/")
	wantSignInPage(t, back, "/")
	for _, l := range []core.SignInLink{l1, l2} {
		if code := portal.NewTestVisitor(exp.Addr(), appHost).Redeem(t, portal.CredentialOf(t, l.URL)); code != http.StatusUnauthorized {
			t.Fatalf("a link of %s from before the revocation = %d", l.Subject, code)
		}
	}
	if !slices.Equal(h.svc.SignInHosts("app"), hosts) {
		t.Fatal("the revocation changed the host names")
	}
	l3 := issue(t, h, "app", "visitor-9Kz")
	again := portal.NewTestVisitor(exp.Addr(), appHost)
	if code := again.Redeem(t, portal.CredentialOf(t, l3.URL)); code != http.StatusOK {
		t.Fatalf("a new link = %d", code)
	}
	wantPage(t, again, "/", "v3 /")
	openSocketOK(t, again)

	// Rule 56: the host names and no callback URLs for credential.
	view, err := h.svc.GetSignIn(ctx, "app")
	if err != nil || !slices.Equal(view.Hosts, []string{appHost}) || len(view.CallbackURLs) != 0 {
		t.Fatalf("view = %+v, %v", view, err)
	}

	// Rules 52, 55, 67 and 68: the rows and the events, and no link, credential or subject where none may be.
	links := []core.SignInLink{l1, l2, l3}
	linkRows := auditRows(t, h.dir, store.AuditSignInLink)
	if len(linkRows) != 3 {
		t.Fatalf("signin.link rows = %d", len(linkRows))
	}
	for i, row := range linkRows {
		var data map[string]string
		json.Unmarshal(row.Data, &data)
		l := links[i] // oldest first
		if row.Via != "console" || row.Flat != "app" || row.Name != "" || data["subject"] != l.Subject || data["host"] != appHost || data["expires_at"] != l.ExpiresAt.Format(time.RFC3339) || len(data) != 3 {
			t.Fatalf("signin.link row = %+v %s", row, row.Data)
		}
		noLinkText(t, "an audit row", string(row.Data), links)
	}
	if rows := auditRows(t, h.dir, store.AuditSignInRevoke); len(rows) != 1 || rows[0].Via != "cli" || string(rows[0].Data) != "{}" {
		t.Fatalf("signin.revoke rows = %+v", rows)
	}
	events, err := h.svc.Events(ctx, "app", "", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		noLinkText(t, "a flat event", e.Message+string(e.Data), links, "guest-7Qx", "visitor-9Kz")
	}
	noLinkText(t, "the host log", h.logs.all(), links)
	var ops []string
	for _, p := range h.notes.signIn() {
		raw, _ := json.Marshal(p)
		noLinkText(t, "a webhook body", string(raw), links, "guest-7Qx", "visitor-9Kz")
		ops = append(ops, p.Operation)
		switch p.Operation {
		case "issue_signin_link":
			if c, _ := json.Marshal(p.Change); !strings.HasPrefix(string(c), `{"expires_at":"`) || p.State != "applied" || p.ConsoleURL != "http://console.test/flats/app" {
				t.Fatalf("issue_signin_link = %s", raw)
			}
		case "revoke_signin":
			if c, _ := json.Marshal(p.Change); string(c) != "{}" {
				t.Fatalf("revoke_signin = %s", raw)
			}
		}
	}
	if want := []string{"set_signin", "issue_signin_link", "set_signin", "issue_signin_link", "set_signin", "revoke_signin", "issue_signin_link"}; !slices.Equal(ops, want) {
		t.Fatalf("signin.changed operations = %q, want %q", ops, want)
	}

	// Sign-in off serves the flat without the gate.
	approveSignIn(t, h, "app", core.SignInRequest{})
	wantPage(t, anon, "/", "v3 /")
	if got := headers(anon); !maps.Equal(got, map[string]string{"X-Flats-Access": "public"}) {
		t.Fatalf("without a sign-in the flat saw %v", got)
	}
}

func TestSignInLinkRefusals(t *testing.T) {
	ctx := context.Background()
	h := startHost(t, t.TempDir())
	deployServer(t, h, "app")
	// Rule 50: not credential.
	if _, err := h.svc.IssueSignInLink(ctx, "app", "guest", time.Time{}, core.ViaConsole); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("without credential = %v", err)
	}
	approveSignIn(t, h, "app", credential("guest"))
	// Rule 49: only the console.
	for _, via := range []core.Via{core.ViaCLI, core.ViaAPI, core.ViaMCP} {
		if _, err := h.svc.IssueSignInLink(ctx, "app", "guest", time.Time{}, via); !errors.Is(err, core.ErrForbidden) {
			t.Fatalf("via %s = %v", via, err)
		}
	}
	if _, err := h.svc.IssueSignInLink(ctx, "app", "stranger", time.Time{}, core.ViaConsole); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("a subject off the list = %v", err)
	}
	// No Portal route: the flat is Private.
	_, err := h.svc.IssueSignInLink(ctx, "app", "guest", time.Time{}, core.ViaConsole)
	if !errors.Is(err, core.ErrConflict) || !strings.Contains(err.Error(), "Public on Portal with a ready relay") {
		t.Fatalf("without a route = %v", err)
	}
	publicOnPortal(t, h, "app")
	// A route whose relays report no canonical host name yet.
	if _, err := h.svc.IssueSignInLink(ctx, "app", "guest", time.Time{}, core.ViaConsole); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("without a relay = %v", err)
	}
	h.px.Of(t, "app").SetRelays(portal.ReadyRelay("relay.test", appHost))
	now := time.Now()
	for _, exp := range []time.Time{now.Add(30 * time.Second), now.Add(91 * 24 * time.Hour)} {
		if _, err := h.svc.IssueSignInLink(ctx, "app", "guest", exp, core.ViaConsole); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("expiry %s = %v", exp, err)
		}
	}
	for _, exp := range []time.Time{now.Add(2 * time.Minute), now.Add(89 * 24 * time.Hour)} {
		if l, err := h.svc.IssueSignInLink(ctx, "app", "guest", exp, core.ViaConsole); err != nil || !l.ExpiresAt.Equal(exp.UTC().Truncate(time.Second)) {
			t.Fatalf("expiry %s = %v, %v", exp, l.ExpiresAt, err)
		}
	}
	if rows := auditRows(t, h.dir, store.AuditSignInLink); len(rows) != 2 {
		t.Fatalf("a refused link wrote a row: %d rows", len(rows))
	}
}

// writeSignIn writes a sign-in setting into flats.db as the store keeps it, with the next sequence. A test uses it for
// an oidc issuer on a loopback http server, which the host refuses at a request (rule 2) and the SDK accepts.
func writeSignIn(t *testing.T, dir, slug, provider, issuer, clientID string, allow ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw, _ := json.Marshal(allow)
	if _, err := db.Exec(`UPDATE flats SET signin_provider=?, signin_issuer=?, signin_client_id=?, signin_allow=?, signin_seq=signin_seq+1 WHERE slug=?`,
		provider, issuer, clientID, string(raw), slug); err != nil {
		t.Fatal(err)
	}
}

func TestSignInOIDCVisitorHeaders(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	h := startHost(t, dir)
	idp := portal.NewTestIdP(t, "flats-app")
	deployServer(t, h, "app")
	writeSignIn(t, dir, "app", "oidc", idp.URL, "flats-app", "ann@example.com", "sub:bob-1")
	publicOnPortal(t, h, "app")
	exp := h.px.Of(t, "app")
	exp.SetRelays(portal.ReadyRelay("relay.test", appHost))
	headers := func(v *portal.TestVisitor) map[string]string {
		t.Helper()
		code, body := v.Get(t, "/headers", http.Header{"X-Flats-Visitor-Email": {"forged@example.com"}})
		var m map[string]string
		if code != http.StatusOK || json.Unmarshal([]byte(body), &m) != nil {
			t.Fatalf("headers = %d %s", code, body)
		}
		return m
	}
	// Rule 45 for an email entry: the verified email is the subject and the email.
	ann := portal.NewTestVisitor(exp.Addr(), appHost)
	ann.OIDCLogin(t, idp, "ann-1", "ann@example.com")
	if got, want := headers(ann), map[string]string{"X-Flats-Access": "public", "X-Flats-Visitor-Provider": "oidc",
		"X-Flats-Visitor-Subject": "ann@example.com", "X-Flats-Visitor-Email": "ann@example.com"}; !maps.Equal(got, want) {
		t.Fatalf("ann: the flat saw %v, want %v", got, want)
	}
	// For a sub: entry without an email, the email header is absent.
	bob := portal.NewTestVisitor(exp.Addr(), appHost)
	bob.OIDCLogin(t, idp, "bob-1", "")
	if got, want := headers(bob), map[string]string{"X-Flats-Access": "public", "X-Flats-Visitor-Provider": "oidc", "X-Flats-Visitor-Subject": "sub:bob-1"}; !maps.Equal(got, want) {
		t.Fatalf("bob: the flat saw %v, want %v", got, want)
	}
	// Rule 56: the callback URL of each host name for oidc.
	if v, err := h.svc.GetSignIn(ctx, "app"); err != nil || !slices.Equal(v.CallbackURLs, []string{"https://" + appHost + "/_portal/auth/callback"}) {
		t.Fatalf("view = %+v, %v", v, err)
	}
}

func TestSignInGateFailureOnTheHost(t *testing.T) {
	ctx := context.Background()
	h := startHost(t, t.TempDir())
	deployServer(t, h, "app")
	approveSignIn(t, h, "app", core.SignInRequest{Provider: "oidc", Issuer: "https://idp-a.example", ClientID: "c", Allow: []string{"ann@example.com"}})
	// Rule 27: a client secret bound to another issuer builds no gate.
	if _, err := h.svc.SetSignInSecret(ctx, "app", "cs-distinctive-1", "https://idp-b.example", "c", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	// Rule 35: a visibility approval that needs the Portal route fails with the reason.
	if err := h.svc.SetProviderPermission(ctx, "app", store.ProviderPortal, true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	r, err := h.svc.SetVisibility(ctx, "app", store.Public, core.ViaConsole, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.svc.Decide(ctx, r.Approval.ID, true)
	if (err == nil && a.Status != "failed") || !strings.Contains(fmt.Sprint(err, a.Result), "sign-in gate: ") || !strings.Contains(fmt.Sprint(err, a.Result), "flats signin secret set app") {
		t.Fatalf("visibility approval = %s %q, %v", a.Status, a.Result, err)
	}
	if h.px.Count() != 0 {
		t.Fatal("a gate that failed opened a Portal route")
	}
	if text := fmt.Sprint(err, a.Result, h.logs.all()); strings.Contains(text, "cs-distinctive-1") {
		t.Fatal("a message holds the client secret")
	}

	// The binding of the setting builds the gate, and a set for another issuer stops the route at once (rule 29).
	if _, err := h.svc.SetSignInSecret(ctx, "app", "cs-distinctive-1", "https://idp-a.example", "c", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	publicOnPortal(t, h, "app")
	exp := h.px.Of(t, "app")
	exp.SetRelays(portal.ReadyRelay("relay.test", appHost))
	wantSignInPage(t, portal.NewTestVisitor(exp.Addr(), appHost), "/")
	res, err := h.svc.SetSignInSecret(ctx, "app", "cs-distinctive-2", "https://idp-b.example", "c", core.ViaCLI)
	if err != nil || !strings.Contains(res.Message, "the Portal route of app is stopped") {
		t.Fatalf("set = %+v, %v", res, err)
	}
	if exp.Closes() != 1 {
		t.Fatal("the served route did not stop and unregister")
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
	if portalEP == nil || portalEP.State != "error" || !strings.HasPrefix(portalEP.Detail, "sign-in gate: ") {
		t.Fatalf("Portal endpoint = %+v", portalEP)
	}
	// The other routes of the flat stay.
	if localEP == nil || localEP.State != "ready" {
		t.Fatalf("Local endpoint = %+v", localEP)
	}
	wantPage(t, privateVisitor(t, h, "app"), "/", "v1 /")

	// The next build after the cause is removed opens the route again (rule 35).
	if _, err := h.svc.SetSignInSecret(ctx, "app", "cs-distinctive-1", "https://idp-a.example", "c", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if h.px.Count() != 2 {
		t.Fatalf("exposures = %d, want a new one", h.px.Count())
	}
	exp = h.px.Of(t, "app")
	exp.SetRelays(portal.ReadyRelay("relay.test", appHost))
	wantSignInPage(t, portal.NewTestVisitor(exp.Addr(), appHost), "/")

	// Rule 21: when step 4 of an applied change fails, the approval is approved, the setting is stored, and the
	// result says that the Portal route stopped and why. Here the stored client secret belongs to the old issuer.
	r, err = h.svc.RequestSignIn(ctx, "app", core.SignInRequest{Provider: "oidc", Issuer: "https://idp-c.example", ClientID: "c", Allow: []string{"ann@example.com"}}, core.ViaCLI)
	a = decide(t, h, r, err)
	if a.Status != "approved" || !strings.Contains(a.Result, "; the setting is stored, and the Portal route of app is stopped: sign-in gate: ") {
		t.Fatalf("approval = %s %q", a.Status, a.Result)
	}
	if exp.Closes() != 1 {
		t.Fatal("step 4 did not stop the route")
	}
	if v, _ := h.svc.GetSignIn(ctx, "app"); v.Issuer != "https://idp-c.example" {
		t.Fatalf("the setting = %+v", v)
	}
}

func TestSignInRenameAndDeletion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	h := startHost(t, dir)
	deployServer(t, h, "app")
	publicOnPortal(t, h, "app")
	approveSignIn(t, h, "app", credential("guest-7Qx"))
	exp1 := h.px.Of(t, "app")
	exp1.SetRelays(portal.ReadyRelay("relay.test", appHost))
	l1 := issue(t, h, "app", "guest-7Qx")
	v1 := portal.NewTestVisitor(exp1.Addr(), appHost)
	if code := v1.Redeem(t, portal.CredentialOf(t, l1.URL)); code != http.StatusOK {
		t.Fatalf("redeem = %d", code)
	}
	appFile := filepath.Join(dir, "portal", "app.json")
	_, before := tokenSecret(t, appFile)

	// Rule 41: a rename gives the old identity a new token secret, with its row in the rename.
	if _, err := h.svc.RenameSlug(ctx, "app", "new", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if _, after := tokenSecret(t, appFile); after == before || after == "" {
		t.Fatal("the rename kept the token secret of the old identity")
	}
	if gateOfFile(t, appFile, appHost, "guest-7Qx", portal.CredentialOf(t, l1.URL)) {
		t.Fatal("a link of the old host name still opens a gate of the old identity")
	}
	if rows := auditRows(t, dir, store.AuditSignInRevoke); len(rows) != 1 || rows[0].Flat != "app" || rows[0].Via != "cli" || string(rows[0].Data) != `{"cause":"rename"}` {
		t.Fatalf("signin.revoke rows = %+v", rows)
	}
	exp2 := h.px.Of(t, "new")
	exp2.SetRelays(portal.ReadyRelay("relay.test", newHost))
	// The old slug serves only the redirect, without a gate.
	wantRedirect := func(addr string) {
		t.Helper()
		for _, v := range []*portal.TestVisitor{portal.NewTestVisitor(addr, appHost), {Addr: addr, Host: appHost, Cookies: v1.Cookies}} {
			resp := v.Do(t, http.MethodGet, "/x?y=1", nil, nil)
			resp.Body.Close()
			if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != "https://"+newHost+"/x?y=1" {
				t.Fatalf("the old slug = %d to %q, want the redirect", resp.StatusCode, resp.Header.Get("Location"))
			}
		}
	}
	wantRedirect(exp1.Addr())
	// The setting stays, and the new identity has the gate; a link of the old identity is refused there.
	if v, err := h.svc.GetSignIn(ctx, "new"); err != nil || v.Provider != "credential" || !slices.Equal(v.Allow, []string{"guest-7Qx"}) {
		t.Fatalf("the renamed flat = %+v, %v", v, err)
	}
	v2 := portal.NewTestVisitor(exp2.Addr(), newHost)
	wantSignInPage(t, v2, "/")
	if code := v2.Redeem(t, portal.CredentialOf(t, l1.URL)); code != http.StatusUnauthorized {
		t.Fatalf("a link of the old identity on the new host = %d", code)
	}
	l2 := issue(t, h, "new", "guest-7Qx")
	if l2.Host != newHost || v2.Redeem(t, portal.CredentialOf(t, l2.URL)) != http.StatusOK {
		t.Fatalf("a link of the new identity = %v", l2)
	}
	wantPage(t, v2, "/", "v1 /")
	// A later sign-in change of the flat does not reach the old slug.
	approveSignIn(t, h, "new", credential("guest-7Qx", "extra-3Lp"))
	wantRedirect(exp1.Addr())
	wantPage(t, v2, "/", "v1 /")

	// After a restart: the redirect has no gate, and the session of the new host name survives.
	h.stop()
	h = startHost(t, dir)
	r1 := h.px.Of(t, "app")
	r1.SetRelays(portal.ReadyRelay("relay.test", appHost))
	r2 := h.px.Of(t, "new")
	r2.SetRelays(portal.ReadyRelay("relay.test", newHost))
	wantRedirect(r1.Addr())
	v2.Addr = r2.Addr()
	wantPage(t, v2, "/", "v1 /")

	// Rule 42: a deletion gives the identity a new token secret, with its row in the deletion.
	newFile := filepath.Join(dir, "portal", "new.json")
	address, secret := tokenSecret(t, newFile)
	if r, err := h.svc.Delete(ctx, "new", core.ViaConsole, ""); err != nil || r.Status != "done" {
		t.Fatalf("delete = %+v, %v", r, err)
	}
	if _, after := tokenSecret(t, newFile); after == secret {
		t.Fatal("the deletion kept the token secret")
	}
	rows := auditRows(t, dir, store.AuditSignInRevoke)
	if len(rows) != 2 || rows[1].Flat != "new" || rows[1].Via != "console" || string(rows[1].Data) != `{"cause":"delete"}` {
		t.Fatalf("signin.revoke rows = %+v", rows)
	}
	for _, p := range h.notes.signIn() {
		if p.Operation == "revoke_signin" {
			t.Fatal("a rename or a deletion sent revoke_signin")
		}
	}
	// A new flat with the slug and the subject keeps the host name and refuses a link of the deleted flat.
	deployServer(t, h, "new")
	approveSignIn(t, h, "new", credential("guest-7Qx"))
	publicOnPortal(t, h, "new")
	r3 := h.px.Of(t, "new")
	r3.SetRelays(portal.ReadyRelay("relay.test", newHost))
	if got, _ := tokenSecret(t, newFile); got != address {
		t.Fatal("the new flat got another identity address")
	}
	fresh := portal.NewTestVisitor(r3.Addr(), newHost)
	if code := fresh.Redeem(t, portal.CredentialOf(t, l2.URL)); code != http.StatusUnauthorized {
		t.Fatalf("a link of the deleted flat = %d", code)
	}
	l3 := issue(t, h, "new", "guest-7Qx")
	if code := fresh.Redeem(t, portal.CredentialOf(t, l3.URL)); code != http.StatusOK {
		t.Fatalf("a link of the new flat = %d", code)
	}
}

func TestSignInRenameWhileOffEndsOldLinks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	h := startHost(t, dir)
	deployServer(t, h, "app")
	publicOnPortal(t, h, "app")
	approveSignIn(t, h, "app", credential("guest-7Qx"))
	h.px.Of(t, "app").SetRelays(portal.ReadyRelay("relay.test", appHost))
	l := issue(t, h, "app", "guest-7Qx")
	approveSignIn(t, h, "app", core.SignInRequest{})
	path := filepath.Join(dir, "portal", "app.json")
	if !gateOfFile(t, path, appHost, "guest-7Qx", portal.CredentialOf(t, l.URL)) {
		t.Fatal("the link of the earlier credential setting does not open a gate of the identity")
	}
	// Rule 41: also while sign-in is off.
	if _, err := h.svc.RenameSlug(ctx, "app", "moved", core.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if gateOfFile(t, path, appHost, "guest-7Qx", portal.CredentialOf(t, l.URL)) {
		t.Fatal("a link of an earlier credential setting outlived the rename")
	}
}

func TestSignInIdentityFileCannotBeWritten(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder that the test cannot write: Unix, not as the superuser")
	}
	ctx := context.Background()
	dir := t.TempDir()
	h := startHost(t, dir)
	deployServer(t, h, "app")
	publicOnPortal(t, h, "app")
	portalDir := filepath.Join(dir, "portal")
	if err := os.Chmod(portalDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(portalDir, 0o700) })
	// Rules 41 and 42: the rename and the deletion fail, change nothing, and name the identity file.
	if _, err := h.svc.RenameSlug(ctx, "app", "new", core.ViaCLI); err == nil || !strings.Contains(err.Error(), filepath.Join(portalDir, "app.json")) {
		t.Fatalf("rename = %v", err)
	}
	if _, err := h.svc.GetFlat(ctx, "app"); err != nil {
		t.Fatalf("the failed rename moved the flat: %v", err)
	}
	if _, err := h.svc.GetFlat(ctx, "new"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the failed rename made the new slug: %v", err)
	}
	if _, err := h.svc.Delete(ctx, "app", core.ViaConsole, ""); err == nil || !strings.Contains(err.Error(), filepath.Join(portalDir, "app.json")) {
		t.Fatalf("delete = %v", err)
	}
	if _, err := h.svc.GetFlat(ctx, "app"); err != nil {
		t.Fatalf("the failed deletion removed the flat: %v", err)
	}
	if rows := auditRows(t, dir, store.AuditSignInRevoke); len(rows) != 0 {
		t.Fatalf("a failed write wrote rows: %+v", rows)
	}
	wantPage(t, portal.NewTestVisitor(h.px.Of(t, "app").Addr(), appHost), "/", "v1 /")
}
