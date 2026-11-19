package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/portal-tunnel/v2/portal/identity"
	"github.com/gosuda/portal-tunnel/v2/sdk"
	"github.com/gosuda/portal-tunnel/v2/sdk/appauth"
	"github.com/gosuda/portal-tunnel/v2/types"
	"github.com/gosuda/portal-tunnel/v2/utils"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/ostest"
)

// The gate on the Portal route (docs/spec/portal-sign-in.md, rules 33 to 40), with a handler in place of the flat.

const testHost = "app-0xabc.relay.test"

// flatHit is one request that reached the flat behind the gate.
type flatHit struct {
	path      string
	seq       int64
	gated     bool
	principal appauth.Principal
}

// flatRecorder stands in for the public handler of a flat and records what reaches it.
type flatRecorder struct {
	mu   sync.Mutex
	hits []flatHit
}

func (f *flatRecorder) handler(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seq, gated := core.SignInGateSeq(r.Context())
		p, _ := appauth.PrincipalFromContext(r.Context())
		f.mu.Lock()
		f.hits = append(f.hits, flatHit{path: r.URL.Path, seq: seq, gated: gated, principal: p})
		f.mu.Unlock()
		io.WriteString(w, name+" "+r.URL.Path)
	})
}

func (f *flatRecorder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hits)
}

func (f *flatRecorder) last(t *testing.T) flatHit {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hits) == 0 {
		t.Fatal("no request reached the flat")
	}
	return f.hits[len(f.hits)-1]
}

func credentialGate(seq int64, allow ...string) *core.PortalSignIn {
	return &core.PortalSignIn{Seq: seq, Gate: core.SignInGate{Provider: appauth.ProviderCredential, Allow: allow}}
}

// logLines collects the host log of a Net.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logLines) with(s string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.Contains(line, s) {
			out = append(out, line)
		}
	}
	return out
}

// serveGated serves h for app behind si on a relay that reports testHost, and returns a visitor of that host.
func serveGated(t *testing.T, n *Net, ff *fakeFactory, h http.Handler, si *core.PortalSignIn) (*TestVisitor, *fakeExposure) {
	t.Helper()
	if _, err := n.ServeSignIn(t.Context(), "app", h, false, si); err != nil {
		t.Fatal(err)
	}
	f := ff.last(t)
	f.set(ReadyRelay("relay.test", testHost))
	return NewTestVisitor(f.Addr().String(), testHost), f
}

// signInWith redeems a fresh credential of subject, so v holds a session of the gate.
func signInWith(t *testing.T, n *Net, v *TestVisitor, subject string) {
	t.Helper()
	link, err := n.IssuePortalCredential(t.Context(), "app", subject, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if code := v.Redeem(t, CredentialOf(t, link.URL)); code != http.StatusOK {
		t.Fatalf("redeem = %d", code)
	}
}

func wantLogin(t *testing.T, v *TestVisitor, path string) {
	t.Helper()
	resp := v.Do(t, http.MethodGet, path, nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/_portal/auth/login?next=") {
		t.Fatalf("GET %s = %d to %q, want the sign-in page", path, resp.StatusCode, resp.Header.Get("Location"))
	}
}

func wantFlat(t *testing.T, v *TestVisitor, path, body string) {
	t.Helper()
	if code, got := v.Get(t, path, nil); code != http.StatusOK || got != body {
		t.Fatalf("GET %s = %d %q, want %q", path, code, got, body)
	}
}

func TestSignInGateAroundTheHandlerBox(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	var flat flatRecorder
	v, f := serveGated(t, n, ff, flat.handler("v1"), credentialGate(1, "guest", "other"))

	// Rule 33: a request without a session goes to the sign-in page and never reaches the flat.
	wantLogin(t, v, "/page")
	if resp := v.Do(t, http.MethodPost, "/form", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST without a session = %d", resp.StatusCode)
	}
	if flat.count() != 0 {
		t.Fatal("a request without a session reached the flat")
	}
	signInWith(t, n, v, "guest")
	wantFlat(t, v, "/page", "v1 /page")
	if hit := flat.last(t); !hit.gated || hit.seq != 1 || hit.principal != (appauth.Principal{Provider: "credential", Subject: "guest"}) {
		t.Fatalf("the flat saw %+v, want the principal of a gate of sequence 1", hit)
	}

	// Rule 38: a redeploy puts a new handler into the box and keeps the gate and its sessions.
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("v2"), false, credentialGate(1, "guest", "other")); err != nil {
		t.Fatal(err)
	}
	wantFlat(t, v, "/page", "v2 /page")
	// An open that carries no setting keeps the gate.
	if _, err := n.Serve(t.Context(), "app", flat.handler("v3"), false); err != nil {
		t.Fatal(err)
	}
	wantLogin(t, NewTestVisitor(v.Addr, v.Host), "/")
	wantFlat(t, v, "/page", "v3 /page")
	// An open of a lower sequence keeps the gate of the higher one, also when it would turn sign-in off.
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("v4"), false, &core.PortalSignIn{Seq: 0}); err != nil {
		t.Fatal(err)
	}
	wantLogin(t, NewTestVisitor(v.Addr, v.Host), "/")
	wantFlat(t, v, "/page", "v4 /page")
	if hit := flat.last(t); hit.seq != 1 {
		t.Fatalf("the flat saw sequence %d, want the gate of sequence 1", hit.seq)
	}

	// Rule 39: a removed entry loses access at the next request.
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("v5"), false, credentialGate(2, "other")); err != nil {
		t.Fatal(err)
	}
	wantLogin(t, v, "/page")

	// Rule 53: a machine client sends the credential in X-Portal-Access-Credential instead of a session.
	link, err := n.IssuePortalCredential(t.Context(), "app", "other", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	machine := NewTestVisitor(v.Addr, v.Host)
	if code, body := machine.Get(t, "/api", http.Header{"X-Portal-Access-Credential": {CredentialOf(t, link.URL)}}); code != http.StatusOK || body != "v5 /api" {
		t.Fatalf("credential header = %d %q", code, body)
	}

	// Sign-in off serves the handler box without a gate.
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("v6"), false, &core.PortalSignIn{Seq: 3}); err != nil {
		t.Fatal(err)
	}
	wantFlat(t, NewTestVisitor(v.Addr, v.Host), "/open", "v6 /open")
	if hit := flat.last(t); hit.gated {
		t.Fatal("sign-in off still marks the requests of a gate")
	}
	if f.closeCount() != 0 || ff.count() != 1 {
		t.Fatal("a change of the gate registered the exposure again")
	}
}

func TestSignInGateRedirectMark(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	var flat flatRecorder
	v, _ := serveGated(t, n, ff, flat.handler("flat"), credentialGate(4, "guest"))
	wantLogin(t, v, "/")
	// Rule 41: the open of the redirect of an old slug removes the gate and serves only the redirect.
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("redirect"), true, &core.PortalSignIn{Redirect: true}); err != nil {
		t.Fatal(err)
	}
	wantFlat(t, v, "/x", "redirect /x")
	// An open without a setting keeps it so.
	if _, err := n.Serve(t.Context(), "app", flat.handler("redirect2"), true); err != nil {
		t.Fatal(err)
	}
	wantFlat(t, v, "/x", "redirect2 /x")
}

func TestSignInGateBuildFailure(t *testing.T) {
	cases := map[string]struct {
		si   *core.PortalSignIn
		want string
	}{
		"the setting cannot be read": {&core.PortalSignIn{Seq: -1, Err: errors.New("the sign-in setting of app cannot be read: disk I/O error")}, "cannot be read"},
		"the client secret has another binding": {&core.PortalSignIn{Seq: 2, Err: fmt.Errorf("%w: set it again with `flats signin secret set app --issuer https://new.example --client-id c`", core.ErrSignInSecretBinding)},
			"flats signin secret set app"},
		"the client secret does not open": {&core.PortalSignIn{Seq: 2, Err: errors.New("open the sign-in client secret of app: cipher: message authentication failed")}, "cipher"},
		"appauth.New refuses": {&core.PortalSignIn{Seq: 2, Gate: core.SignInGate{Provider: appauth.ProviderOIDC, Issuer: "http://idp.example", ClientID: "c", Allow: []string{"a@example.com"}}},
			"the gate refuses the stored setting"},
	}
	for name, tc := range cases {
		t.Run(name+"/new entry", func(t *testing.T) {
			var logs logLines
			n, ff := newTestNet(t, Config{Discovery: true, Logf: logs.logf})
			var runs atomic.Int32
			n.runHTTP = func(ctx context.Context, ln net.Listener, h http.Handler) error {
				runs.Add(1)
				<-ctx.Done()
				return nil
			}
			var flat flatRecorder
			_, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, tc.si)
			var ge *core.SignInGateError
			if !errors.As(err, &ge) || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "sign-in gate: ") {
				t.Fatalf("ServeSignIn = %v, want a sign-in gate error with %q", err, tc.want)
			}
			if ff.count() != 0 || runs.Load() != 0 {
				t.Fatalf("a gate that failed opened a route: %d exposures, %d HTTP runs", ff.count(), runs.Load())
			}
			wantGateError(t, n, tc.want)
			if lines := logs.with("sign-in gate"); len(lines) != 1 || !strings.Contains(lines[0], "app") {
				t.Fatalf("log = %q, want one line with the slug and the reason", lines)
			}
			// The next build after the cause is removed opens the route.
			if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, credentialGate(3, "guest")); err != nil {
				t.Fatal(err)
			}
			// RunHTTP starts in the background.
			for deadline := time.Now().Add(5 * time.Second); runs.Load() == 0 && time.Now().Before(deadline); {
				time.Sleep(5 * time.Millisecond)
			}
			if ff.count() != 1 || runs.Load() != 1 {
				t.Fatal("the next build did not open the route")
			}
			for _, h := range n.Status().Hosts {
				if strings.Contains(h.Detail, "sign-in gate") {
					t.Fatalf("status still holds the failure: %+v", h)
				}
			}
		})
		t.Run(name+"/served entry", func(t *testing.T) {
			var logs logLines
			n, ff := newTestNet(t, Config{Discovery: true, Logf: logs.logf})
			var flat flatRecorder
			v, f := serveGated(t, n, ff, flat.handler("flat"), credentialGate(1, "guest"))
			signInWith(t, n, v, "guest")
			wantFlat(t, v, "/", "flat /")
			before := flat.count()
			if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("other"), false, tc.si); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ServeSignIn = %v, want %q", err, tc.want)
			}
			// The route stopped and unregistered; it never served the flat without the gate.
			if f.closeCount() != 1 || n.entry("app") != nil {
				t.Fatalf("the served route did not stop: %d closes", f.closeCount())
			}
			if flat.count() != before {
				t.Fatal("a request reached the flat after the gate failed")
			}
			wantGateError(t, n, tc.want)
			if lines := logs.with("sign-in gate"); len(lines) != 1 {
				t.Fatalf("log = %q, want one line", lines)
			}
		})
	}
}

// wantGateError checks the Portal endpoint of app: state error and the detail of rule 35.
func wantGateError(t *testing.T, n *Net, want string) {
	t.Helper()
	for _, h := range n.Status().Hosts {
		if h.Host == "app" {
			if h.State != "error" || !strings.HasPrefix(h.Detail, "sign-in gate: ") || !strings.Contains(h.Detail, want) {
				t.Fatalf("status = %+v, want error with the detail sign-in gate: ...%s", h, want)
			}
			return
		}
	}
	t.Fatalf("status has no host app: %+v", n.Status().Hosts)
}

// writeIdentityWithoutSecret writes an identity file of slug in dir as an older SDK did: without a token secret.
func writeIdentityWithoutSecret(t *testing.T, dir, slug string) types.Identity {
	t.Helper()
	id, err := identity.Generate(slug)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := identity.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "token_secret")
	raw, _ = json.Marshal(m)
	if err := utils.CreatePrivateFile(filepath.Join(dir, slug+".json"), raw); err != nil {
		t.Fatal(err)
	}
	return id
}

func storedTokenSecret(t *testing.T, path string) (string, string) {
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

func TestSignInKeepsTheTokenSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.json")
	old := writeIdentityWithoutSecret(t, dir, "app")
	n, ff := newTestNet(t, Config{Dir: dir, Discovery: true})
	var flat flatRecorder
	v, _ := serveGated(t, n, ff, flat.handler("flat"), credentialGate(1, "guest"))

	// Rule 36: the file holds the token secret of the identity that was loaded, with the same address.
	address, secret := storedTokenSecret(t, path)
	if secret == "" || address != old.Address || ff.last(t).id.Address != old.Address {
		t.Fatalf("identity file: address %q token secret %q, want %q with a token secret", address, secret, old.Address)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if ostest.WrongMode(fi, 0o600) {
		t.Fatalf("identity mode = %v, want 0600", fi.Mode().Perm())
	}
	identityFileACL(t, path)
	// The written token secret is the one the gate uses: a credential made from the file is accepted.
	raw, _ := os.ReadFile(path)
	stored, err := identity.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := appauth.IssueCredential(stored, testHost, "guest", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if code := v.Redeem(t, cred); code != http.StatusOK {
		t.Fatalf("redeem a credential of the written identity = %d", code)
	}
	wantFlat(t, v, "/", "flat /")

	// Survival: a new Net on the same folder accepts a session of the canonical host name from before the restart.
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	n2, ff2 := newTestNet(t, Config{Dir: dir, Discovery: true})
	v2, _ := serveGated(t, n2, ff2, flat.handler("restarted"), credentialGate(1, "guest"))
	v2.Cookies = v.Cookies
	wantFlat(t, v2, "/", "restarted /")
	if _, again := storedTokenSecret(t, path); again != secret {
		t.Fatal("a restart changed the token secret")
	}
}

func TestSignInTokenSecretCannotBeWritten(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder that the test cannot write: Unix, not as the superuser")
	}
	dir := t.TempDir()
	writeIdentityWithoutSecret(t, dir, "app")
	n, ff := newTestNet(t, Config{Dir: dir, Discovery: true})
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	var flat flatRecorder
	_, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, credentialGate(1, "guest"))
	if ge := (*core.SignInGateError)(nil); !errors.As(err, &ge) || !strings.Contains(err.Error(), filepath.Join(dir, "app.json")) {
		t.Fatalf("ServeSignIn = %v, want a gate error that names the identity file", err)
	}
	if ff.count() != 0 {
		t.Fatal("a gate without a kept token secret opened a route")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, credentialGate(1, "guest")); err != nil || ff.count() != 1 {
		t.Fatalf("the next build = %v", err)
	}
}

func TestSignInProviderCheck(t *testing.T) {
	SetCheckTimeout(t, 5*time.Second)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "https://" + ln.Addr().String()
	ln.Close()
	var logs logLines
	n, ff := newTestNet(t, Config{Discovery: true, Logf: logs.logf})
	var flat flatRecorder
	v, _ := serveGated(t, n, ff, flat.handler("flat"), &core.PortalSignIn{Seq: 1, Gate: core.SignInGate{Provider: appauth.ProviderOIDC, Issuer: closed, ClientID: "c", Allow: []string{"ann@example.com"}}})
	// Rule 37: the check fails in the background, the endpoint says so, and the gate stays and refuses.
	waitDetail(t, n, func(d string) bool { return strings.Contains(d, "sign-in provider check failed: ") })
	if lines := logs.with("sign-in provider check failed"); len(lines) != 1 {
		t.Fatalf("log = %q, want one warning line", lines)
	}
	wantLogin(t, v, "/")
	if flat.count() != 0 {
		t.Fatal("a failed check let a visitor in")
	}
	// A later build that passes its check clears the detail.
	idp := NewTestIdP(t, "c")
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, &core.PortalSignIn{Seq: 2, Gate: core.SignInGate{Provider: appauth.ProviderOIDC, Issuer: idp.URL, ClientID: "c", Allow: []string{"ann@example.com"}}}); err != nil {
		t.Fatal(err)
	}
	waitDetail(t, n, func(d string) bool { return !strings.Contains(d, "check failed") })
	// A gate without a check leaves no failure of an earlier one in the detail.
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, &core.PortalSignIn{Seq: 3, Gate: core.SignInGate{Provider: appauth.ProviderOIDC, Issuer: closed, ClientID: "c", Allow: []string{"ann@example.com"}}}); err != nil {
		t.Fatal(err)
	}
	waitDetail(t, n, func(d string) bool { return strings.Contains(d, "sign-in provider check failed: ") })
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, credentialGate(4, "guest")); err != nil {
		t.Fatal(err)
	}
	for _, h := range n.Status().Hosts {
		if strings.Contains(h.Detail, "check failed") {
			t.Fatalf("a credential gate shows the failure of an oidc one: %+v", h)
		}
	}
}

func waitDetail(t *testing.T, n *Net, ok func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, h := range n.Status().Hosts {
			if h.Host == "app" && ok(h.Detail) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %+v", n.Status().Hosts)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSignInPublicHosts(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	var flat flatRecorder
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, credentialGate(1, "guest")); err != nil {
		t.Fatal(err)
	}
	f := ff.last(t)
	held := time.Now().Add(-time.Hour)
	a := sdk.RelayStatus{RelayURL: "https://a.test", PublicURL: "https://app-0xabc.a.test", AliasURL: "https://app.a.test", AliasSince: held, State: sdk.RelayReady}
	b := sdk.RelayStatus{RelayURL: "https://b.test", PublicURL: "https://app-0xabc.b.test:443", State: sdk.RelayConnecting}
	f.set(a, b)

	// Rule 34: every relay whatever its state, the canonical URL with a zero Since and the alias with its AliasSince.
	got := publicHosts(n.entry("app"))
	want := []appauth.PublicHost{{URL: a.PublicURL}, {URL: a.AliasURL, Since: held}, {URL: b.PublicURL}}
	if !slices.Equal(got, want) {
		t.Fatalf("PublicHosts = %+v, want %+v", got, want)
	}
	if hosts := n.PortalHosts("app"); !slices.Equal(hosts, []string{"app-0xabc.a.test", "app.a.test", "app-0xabc.b.test"}) {
		t.Fatalf("PortalHosts = %q", hosts)
	}
	if n.PortalHosts("other") != nil {
		t.Fatal("a slug that is not served has host names")
	}
	// Another host name gets 421.
	if code, _ := NewTestVisitor(f.Addr().String(), "elsewhere.test").Get(t, "/", nil); code != http.StatusMisdirectedRequest {
		t.Fatalf("another host name = %d, want 421", code)
	}
	// A relay that reports connecting keeps its host names.
	cred, err := appauth.IssueCredential(n.entry("app").id, "app-0xabc.b.test", "guest", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if code, body := NewTestVisitor(f.Addr().String(), "app-0xabc.b.test").Get(t, "/b", http.Header{"X-Portal-Access-Credential": {cred}}); code != http.StatusOK || body != "flat /b" {
		t.Fatalf("a connecting relay = %d %q", code, body)
	}
	// A session on the alias from before its Since does not count: the alias came back as a new hold.
	alias := NewTestVisitor(f.Addr().String(), "app.a.test")
	cred, _ = appauth.IssueCredential(n.entry("app").id, "app.a.test", "guest", time.Now().Add(time.Hour))
	if code := alias.Redeem(t, cred); code != http.StatusOK {
		t.Fatalf("redeem on the alias = %d", code)
	}
	wantFlat(t, alias, "/a", "flat /a")
	a.AliasSince = time.Now().Add(time.Second)
	f.set(a, b)
	wantLogin(t, alias, "/a")
}

func TestSignInEndpointPaths(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	var flat flatRecorder
	v, _ := serveGated(t, n, ff, flat.handler("flat"), credentialGate(1, "guest"))
	signInWith(t, n, v, "guest")
	// Rule 40: the exact paths that sdk/appauth serves never reach the flat, with a session or without one.
	for _, p := range []string{types.PathApplicationAuthLogin, types.PathApplicationAuthChallenge, types.PathApplicationAuthVerify,
		types.PathApplicationAuthRedeem, types.PathApplicationAuthCallback, types.PathApplicationAuthLogout,
		types.PathApplicationAuthEmailRequest, types.PathApplicationAuthEmailRedeem, types.PathApplicationAuthSAMLACS,
		types.PathApplicationAuthSAMLMetadata} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			for _, who := range []*TestVisitor{v, NewTestVisitor(v.Addr, v.Host)} {
				resp := who.Do(t, method, p, nil, http.Header{"Origin": {"https://" + testHost}})
				resp.Body.Close()
			}
		}
	}
	if flat.count() != 0 {
		t.Fatalf("an endpoint of the gate reached the flat: %+v", flat.hits)
	}
	// Another path under /_portal/auth/ is a path of the flat behind the gate. The logout above ended the session.
	signInWith(t, n, v, "guest")
	wantFlat(t, v, "/_portal/auth/mine", "flat /_portal/auth/mine")
	wantLogin(t, NewTestVisitor(v.Addr, v.Host), "/_portal/auth/mine")
}

func TestSignInOIDCGate(t *testing.T) {
	idp := NewTestIdP(t, "flats-app")
	n, ff := newTestNet(t, Config{Discovery: true})
	var flat flatRecorder
	gate := core.SignInGate{Provider: appauth.ProviderOIDC, Issuer: idp.URL, ClientID: "flats-app", ClientSecret: "cs-one", Allow: []string{"ann@example.com", "sub:bob-1"}}
	ann, _ := serveGated(t, n, ff, flat.handler("flat"), &core.PortalSignIn{Seq: 1, Gate: gate})
	ann.OIDCLogin(t, idp, "ann-1", "Ann@Example.com")
	wantFlat(t, ann, "/", "flat /")
	if p := flat.last(t).principal; p != (appauth.Principal{Provider: "oidc", Subject: "ann@example.com", Email: "ann@example.com"}) {
		t.Fatalf("principal = %+v", p)
	}
	if s := idp.Secrets(); !slices.Equal(s, []string{"cs-one"}) {
		t.Fatalf("the token requests sent the client secrets %q", s)
	}
	bob := NewTestVisitor(ann.Addr, ann.Host)
	bob.OIDCLogin(t, idp, "bob-1", "")
	wantFlat(t, bob, "/", "flat /")
	if p := flat.last(t).principal; p != (appauth.Principal{Provider: "oidc", Subject: "sub:bob-1"}) {
		t.Fatalf("principal = %+v", p)
	}
	// A visitor outside the allowlist gets no session.
	eve := NewTestVisitor(ann.Addr, ann.Host)
	resp := eve.Do(t, http.MethodGet, "/_portal/auth/login", nil, nil)
	resp.Body.Close()
	q, _ := resp.Location()
	code := idp.Code("eve-1", "eve@example.com", q.Query().Get("nonce"))
	resp = eve.Do(t, http.MethodGet, "/_portal/auth/callback?state="+q.Query().Get("state")+"&code="+code, nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || eve.Cookies["__Host-portal_access"] != nil {
		t.Fatalf("eve = %d", resp.StatusCode)
	}

	// Rules 28 and 39: a new client secret builds the gate again and keeps the sessions.
	gate.ClientSecret = "cs-two"
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, &core.PortalSignIn{Seq: 1, Gate: gate, Rebuild: true}); err != nil {
		t.Fatal(err)
	}
	wantFlat(t, ann, "/", "flat /")
	carol := NewTestVisitor(ann.Addr, ann.Host)
	gate.Allow = append(gate.Allow, "carol@example.com")
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, &core.PortalSignIn{Seq: 2, Gate: gate}); err != nil {
		t.Fatal(err)
	}
	carol.OIDCLogin(t, idp, "carol-1", "carol@example.com")
	if s := idp.Secrets(); s[len(s)-1] != "cs-two" {
		t.Fatalf("the rebuilt gate sent %q", s[len(s)-1])
	}
	// A change of the client ID ends every session.
	gate.ClientID = "flats-other"
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, &core.PortalSignIn{Seq: 3, Gate: gate}); err != nil {
		t.Fatal(err)
	}
	wantLogin(t, ann, "/")
}

func TestRenewPortalIdentity(t *testing.T) {
	dir := t.TempDir()
	n, ff := newTestNet(t, Config{Dir: dir, Discovery: true})
	var flat flatRecorder
	v, _ := serveGated(t, n, ff, flat.handler("flat"), credentialGate(1, "guest"))
	signInWith(t, n, v, "guest")
	link, err := n.IssuePortalCredential(t.Context(), "app", "guest", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "app.json")
	address, before := storedTokenSecret(t, path)
	hosts := n.PortalHosts("app")

	// Rule 54: a new token secret ends every session and every link, and the host names stay.
	got, renewed, err := n.RenewPortalIdentity(t.Context(), "app")
	if err != nil || !renewed || got != path {
		t.Fatalf("RenewPortalIdentity = %q %v %v", got, renewed, err)
	}
	after2, after := storedTokenSecret(t, path)
	if after == before || after == "" || after2 != address {
		t.Fatal("the identity file did not get a new token secret with the same address")
	}
	identityFileACL(t, path)
	wantLogin(t, v, "/")
	fresh := NewTestVisitor(v.Addr, v.Host)
	if code := fresh.Redeem(t, CredentialOf(t, link.URL)); code != http.StatusUnauthorized {
		t.Fatalf("a link from before = %d, want 401", code)
	}
	if !slices.Equal(n.PortalHosts("app"), hosts) {
		t.Fatal("the host names changed")
	}
	signInWith(t, n, fresh, "guest")
	wantFlat(t, fresh, "/", "flat /")

	// A slug without an identity file has nothing to renew, with or without a running backend.
	if _, renewed, err := n.RenewPortalIdentity(t.Context(), "none"); err != nil || renewed {
		t.Fatalf("no identity = %v %v", renewed, err)
	}
	if p, renewed, err := RenewTokenSecretFile(dir, "app"); err != nil || !renewed || p != path {
		t.Fatalf("RenewTokenSecretFile = %q %v %v", p, renewed, err)
	}
	if _, again := storedTokenSecret(t, path); again == after {
		t.Fatal("RenewTokenSecretFile kept the token secret")
	}
}

func TestIssuePortalCredential(t *testing.T) {
	n, ff := newTestNet(t, Config{Relays: []string{"https://r1.test", "https://r2.test"}})
	var flat flatRecorder
	if _, err := n.IssuePortalCredential(t.Context(), "app", "guest", time.Now().Add(time.Hour)); !errors.Is(err, core.ErrNoPortalRoute) {
		t.Fatalf("no route = %v", err)
	}
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, credentialGate(1, "guest")); err != nil {
		t.Fatal(err)
	}
	f := ff.last(t)
	f.set(sdk.RelayStatus{RelayURL: "https://r1.test", State: sdk.RelayConnecting})
	if _, err := n.IssuePortalCredential(t.Context(), "app", "guest", time.Now().Add(time.Hour)); !errors.Is(err, core.ErrNoPortalRoute) {
		t.Fatalf("no canonical host name = %v", err)
	}
	r1 := sdk.RelayStatus{RelayURL: "https://r1.test", PublicURL: "https://app-0xabc.r1.test", AliasURL: "https://app.r1.test", AliasSince: time.Now().Add(-time.Hour), State: sdk.RelayReady}
	r2 := sdk.RelayStatus{RelayURL: "https://r2.test", PublicURL: "https://app-0xabc.r2.test", State: sdk.RelayReady}
	f.set(r2, r1)
	// Rule 51: the canonical host name of the first relay, never an alias, in the form of CredentialRedeemURL.
	link, err := n.IssuePortalCredential(t.Context(), "app", "guest", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	cred := CredentialOf(t, link.URL)
	want, _ := appauth.CredentialRedeemURL("app-0xabc.r1.test", cred)
	if link.Host != "app-0xabc.r1.test" || link.URL != want || !strings.HasPrefix(link.URL, "https://app-0xabc.r1.test/_portal/auth/login#credential=pcred_") {
		t.Fatalf("link = %q on %q", link.URL, link.Host)
	}
	if fmt.Sprint(link) == link.URL || strings.Contains(fmt.Sprintf("%v %#v", link, link), cred) {
		t.Fatal("a formatted link holds the credential")
	}
	// Rule 53: it signs a visitor in on its host name only.
	if code := NewTestVisitor(f.Addr().String(), "app-0xabc.r1.test").Redeem(t, cred); code != http.StatusOK {
		t.Fatalf("on its host = %d", code)
	}
	for _, other := range []string{"app.r1.test", "app-0xabc.r2.test"} {
		if code := NewTestVisitor(f.Addr().String(), other).Redeem(t, cred); code != http.StatusUnauthorized {
			t.Fatalf("on %s = %d, want 401", other, code)
		}
	}
}
