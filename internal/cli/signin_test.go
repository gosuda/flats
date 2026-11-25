package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/store"
)

// `flats signin` of docs/spec/portal-sign-in.md ("Requests and approvals", rules 13, 25, 54, 56, 59 and 71).

const signInPending = `{"status":"pending_approval","approval":{"id":"apr-signin","flat":"blog","action":"set_signin","status":"pending"},"approval_url":"http://console.test/approvals/apr-signin","message":"This action needs the operator's approval."}`

func TestSignInCommandsSendTheirRequests(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.handle("POST /api/flats/blog/signin", 202, signInPending)
	f.handle("PUT /api/flats/blog/signin/secret", 200, `{"status":"done","message":"sign-in client secret set via cli for issuer https://idp.example.com and client ID app"}`)
	f.handle("DELETE /api/flats/blog/signin/secret", 200, `{"status":"done","message":"sign-in client secret removed via cli"}`)
	f.handle("POST /api/flats/blog/signin/revoke", 200, `{"status":"done","message":"every sign-in session and access link of blog ended via cli"}`)
	for _, tc := range []struct {
		args  []string
		stdin string
		route string
		body  string
		code  int
		out   string
	}{
		{[]string{"signin", "set", "blog", "oidc", "--issuer", "https://idp.example.com", "--client-id", "app", "--allow", "a@b.example", "--allow", "@c.example", "--reason", "why"}, "",
			"POST /api/flats/blog/signin", `{"allow":["a@b.example","@c.example"],"client_id":"app","issuer":"https://idp.example.com","provider":"oidc","reason":"why"}`, ExitPending, "Approval needed: http://console.test/approvals/apr-signin"},
		{[]string{"signin", "set", "blog", "credential", "--allow", "guest one", "--allow", "guest two"}, "",
			"POST /api/flats/blog/signin", `{"allow":["guest one","guest two"],"provider":"credential"}`, ExitPending, "Approval apr-signin is waiting for the operator"},
		{[]string{"signin", "off", "blog", "--reason", "done"}, "", "POST /api/flats/blog/signin", `{"provider":"","reason":"done"}`, ExitPending, "Status: pending_approval"},
		{[]string{"signin", "secret", "set", "blog", "--issuer", "https://idp.example.com", "--client-id", "app"}, "cs-cli-distinct-61\n",
			"PUT /api/flats/blog/signin/secret", `{"client_id":"app","issuer":"https://idp.example.com","value":"cs-cli-distinct-61"}`, ExitOK, "sign-in client secret set via cli"},
		{[]string{"signin", "secret", "rm", "blog"}, "", "DELETE /api/flats/blog/signin/secret", ``, ExitOK, "sign-in client secret removed"},
		{[]string{"signin", "revoke", "blog"}, "", "POST /api/flats/blog/signin/revoke", ``, ExitOK, "every sign-in session and access link of blog ended"},
	} {
		r := run(t, srv.URL, tc.stdin, tc.args...)
		if r.code != tc.code || !strings.Contains(r.stdout, tc.out) || strings.Contains(r.stdout+r.stderr, "cs-cli-distinct-61") {
			t.Errorf("%v: exit %d\n%s\n%s", tc.args, r.code, r.stdout, r.stderr)
			continue
		}
		sent := f.last(tc.route)
		var got any
		if len(sent.body) > 0 {
			_ = json.Unmarshal(sent.body, &got)
		}
		b, _ := json.Marshal(got)
		if want := tc.body; want == "" && len(sent.body) != 0 || want != "" && string(b) != want || sent.client != "cli" {
			t.Errorf("%v sent %s (client %q), want %s", tc.args, sent.body, sent.client, tc.body)
		}
	}
	// --json prints the reply as it is.
	if r := run(t, srv.URL, "", "--json", "signin", "revoke", "blog"); r.code != ExitOK || !strings.Contains(r.stdout, `"status":"done"`) {
		t.Errorf("--json revoke: %d %s", r.code, r.stdout)
	}
	// Rule 59: show prints the setting, the host names and the callback URLs; --json the reply of rule 56.
	const view = `{"provider":"oidc","issuer":"https://idp.example.com","client_id":"app","allow":["a@b.example"],"client_secret":null,` +
		`"hosts":["blog.portal.example","blog-x.relay.example"],"callback_urls":["https://blog.portal.example/_portal/auth/callback","https://blog-x.relay.example/_portal/auth/callback"],"seq":2}`
	f.handle("GET /api/flats/blog/signin", 200, view)
	r := run(t, srv.URL, "", "signin", "show", "blog")
	for _, want := range []string{"Sign-in of blog: OpenID Connect\n  issuer: https://idp.example.com\n  client ID: app\n  allowlist (1):\n    a@b.example\n",
		"  client secret: none (the gate signs in as a public client with PKCE)\n", "  Portal host names: blog.portal.example, blog-x.relay.example\n",
		"  callback URLs to register at the provider:\n    https://blog.portal.example/_portal/auth/callback\n    https://blog-x.relay.example/_portal/auth/callback\n"} {
		if r.code != ExitOK || !strings.Contains(r.stdout, want) {
			t.Errorf("show lacks %q: %d\n%s", want, r.code, r.stdout)
		}
	}
	if r := run(t, srv.URL, "", "signin", "show", "blog", "--json"); r.code != ExitOK || r.stdout != view+"\n" {
		t.Errorf("show --json: %d %s", r.code, r.stdout)
	}
	f.handle("GET /api/flats/blog/signin", 200, `{"provider":"credential","issuer":"","client_id":"","allow":["guest one"],"client_secret":null,"hosts":[],"callback_urls":[],"seq":3}`)
	if r := run(t, srv.URL, "", "signin", "show", "blog"); r.code != ExitOK || !strings.Contains(r.stdout, "Sign-in of blog: access links (credential)\n  allowlist (1):\n    guest one\n") ||
		strings.Contains(r.stdout, "callback") || strings.Contains(r.stdout, "client secret") || !strings.Contains(r.stdout, "The console issues the access links.") {
		t.Errorf("show of credential: %d %s", r.code, r.stdout)
	}
}

func TestSignInUsageErrors(t *testing.T) {
	f, srv := newFakeAPI(t)
	for _, args := range [][]string{
		{"signin"},
		{"signin", "bogus", "blog"},
		{"signin", "show"},
		{"signin", "show", "blog", "extra"},
		{"signin", "set", "blog"},
		{"signin", "set", "blog", "saml", "--allow", "x"},
		{"signin", "set", "blog", "oidc", "--client-id", "c", "--allow", "a@b.example"},
		{"signin", "set", "blog", "oidc", "--issuer", "https://idp.example.com", "--allow", "a@b.example"},
		{"signin", "set", "blog", "oidc", "--issuer", "https://idp.example.com", "--client-id", "c"},
		{"signin", "set", "blog", "oidc", "extra", "--issuer", "https://idp.example.com", "--client-id", "c", "--allow", "a@b.example"},
		{"signin", "set", "blog", "credential"},
		{"signin", "set", "blog", "credential", "--allow", "x", "--issuer", "https://idp.example.com"},
		{"signin", "set", "blog", "credential", "--allow", "x", "--client-id", "c"},
		{"signin", "set", "blog", "credential", "extra", "--allow", "x"},
		{"signin", "off", "blog", "extra"},
		{"signin", "off", "blog", "--allow", "x"},
		{"signin", "off", "blog", "--issuer", "https://idp.example.com"},
		{"signin", "secret"},
		{"signin", "secret", "bogus", "blog"},
		{"signin", "secret", "set", "blog", "--client-id", "c"},
		{"signin", "secret", "set", "blog", "--issuer", "https://idp.example.com"},
		{"signin", "secret", "set", "blog", "extra", "--issuer", "https://idp.example.com", "--client-id", "c"},
		{"signin", "secret", "set", "blog", "--issuer", "https://idp.example.com", "--client-id", "c", "--allow", "x"},
		{"signin", "secret", "rm", "blog", "extra"},
		{"signin", "secret", "rm", "blog", "--issuer", "https://idp.example.com"},
		{"signin", "revoke", "blog", "extra"},
		{"signin", "revoke", "blog", "--reason", "x"},
		{"signin", "links", "blog"},
		{"signin", "link", "blog", "guest"},
	} {
		if r := run(t, srv.URL, "value\n", args...); r.code != ExitUsage || !strings.Contains(r.stderr, "usage: flats signin") {
			t.Errorf("%v: exit %d %s", args, r.code, r.stderr)
		}
	}
	// An empty piped value is refused before a request.
	if r := run(t, srv.URL, "", "signin", "secret", "set", "blog", "--issuer", "https://idp.example.com", "--client-id", "c"); r.code != ExitError ||
		!strings.Contains(r.stderr, "| flats signin secret set blog --issuer https://idp.example.com --client-id c`") {
		t.Errorf("empty value: exit %d %s", r.code, r.stderr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) != 0 {
		t.Fatalf("a usage error sent %d requests", len(f.reqs))
	}
}

func TestSignInHelp(t *testing.T) {
	r := run(t, "", "", "help", "signin")
	if r.code != ExitOK || !strings.HasPrefix(r.stdout, "usage: flats signin show <slug> [--json]\n") {
		t.Fatalf("help signin: %d\n%s", r.code, r.stdout)
	}
	for _, want := range []string{
		"flats signin show <slug> [--json]",
		"flats signin set <slug> oidc --issuer URL --client-id ID --allow ENTRY [--allow ENTRY ...] [--reason TEXT]",
		"flats signin set <slug> credential --allow SUBJECT [--allow SUBJECT ...] [--reason TEXT]",
		"flats signin off <slug> [--reason TEXT]",
		"flats signin secret set <slug> --issuer URL --client-id ID",
		"flats signin secret rm <slug>",
		"flats signin revoke <slug>",
		"Sign-in protects the Portal URL of the flat only",
		"The console issues the access links",
		"This command cannot\nissue one.",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("help signin lacks %q:\n%s", want, r.stdout)
		}
	}
	if r := run(t, "", "", "help"); !strings.Contains(r.stdout, "  signin      manage the Portal sign-in of a flat (it protects the Portal URL only)\n") {
		t.Errorf("usage: %s", r.stdout)
	}
}

// Rule 61: the CLI prints the pending notice of the host for a flat with sign-in on, and its own notice only for a host
// that sends none or the notice of a flat that is Public now.
func TestVisibilityKeepsThePendingNoticeOfTheHost(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.handle("POST /api/flats/blog/visibility", 202, `{"status":"pending_approval","approval":{"id":"apr-1","flat":"blog","action":"set_visibility","status":"pending"},`+
		`"approval_url":"http://console.test/approvals/apr-1","message":"This action needs the operator's approval.","notice":"`+core.PendingSignInPublicNotice+`"}`)
	for _, args := range [][]string{{"visibility", "blog", "public"}, {"--json", "visibility", "blog", "public"}} {
		r := run(t, srv.URL, "", args...)
		if r.code != ExitPending || !strings.Contains(r.stdout, core.PendingSignInPublicNotice) || strings.Contains(r.stdout, pendingPublicNotice) {
			t.Errorf("%v: %d %s", args, r.code, r.stdout)
		}
	}
}

// Rule 71: the mux of an older host answers 404 with text; a 404 of this host stays its own error.
func TestSignInOlderHost(t *testing.T) {
	older := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(older.Close)
	for _, args := range [][]string{
		{"signin", "show", "blog"},
		{"signin", "set", "blog", "credential", "--allow", "x"},
		{"signin", "off", "blog"},
		{"signin", "secret", "set", "blog", "--issuer", "https://idp.example.com", "--client-id", "c"},
		{"signin", "secret", "rm", "blog"},
		{"signin", "revoke", "blog"},
	} {
		r := run(t, older.URL, "value\n", args...)
		if r.code != ExitError || !strings.Contains(r.stderr, "the Flats host at "+older.URL+" is older than `flats signin`") {
			t.Errorf("%v: exit %d %s", args, r.code, r.stderr)
		}
	}
	_, srv := newFakeAPI(t)
	if r := run(t, srv.URL, "", "signin", "show", "blog"); r.code != ExitError || strings.Contains(r.stderr, "older") || !strings.Contains(r.stderr, "not found") {
		t.Errorf("a 404 of this host: exit %d %s", r.code, r.stderr)
	}
}

// Each form against a host (rules 13, 25, 54, 56 and 59).
func TestSignInAgainstAHost(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	priv, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { priv.Close() })
	svc, err := core.New(context.Background(), core.Config{DataDir: filepath.Join(dir, "data"), Store: st, Private: priv, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	srv := httptest.NewServer((&api.Server{Svc: svc}).Handler())
	t.Cleanup(srv.Close)
	ctx := context.Background()
	if _, err := svc.CreateFlat(ctx, "app", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	const value = "cs-cli-host-distinct-62"

	if r := run(t, srv.URL, "", "signin", "show", "app"); r.code != ExitOK || !strings.Contains(r.stdout, "Sign-in of app is off") {
		t.Fatalf("show while off: %+v", r)
	}
	r := run(t, srv.URL, "", "signin", "set", "app", "oidc", "--issuer", "https://idp.example.com/", "--client-id", "flats-app", "--allow", "Ann@Example.com", "--allow", "sub:Zed")
	if r.code != ExitPending || !strings.Contains(r.stdout, "Approval needed:") || strings.Contains(r.stdout, "ann@example.com") {
		t.Fatalf("set oidc: %+v", r)
	}
	pending, err := svc.ListApprovals(ctx, "pending")
	if err != nil || len(pending) != 1 || pending[0].Action != "set_signin" || pending[0].Via != "cli" {
		t.Fatalf("pending = %+v %v", pending, err)
	}
	if _, err := svc.Decide(ctx, pending[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if r := run(t, srv.URL, value+"\n", "signin", "secret", "set", "app", "--issuer", "https://idp.example.com", "--client-id", "flats-app"); r.code != ExitOK || strings.Contains(r.stdout+r.stderr, value) ||
		!strings.Contains(r.stdout, "for issuer https://idp.example.com and client ID flats-app") {
		t.Fatalf("secret set: %+v", r)
	}
	// Rule 30: `flats secret ls` lists a plain secret of the flat and not the client secret.
	if r := run(t, srv.URL, "plain-value-4Ty\n", "secret", "set", "app", "PLAIN"); r.code != ExitOK {
		t.Fatalf("secret set PLAIN: %+v", r)
	}
	for _, args := range [][]string{{"secret", "ls", "app"}, {"--json", "secret", "ls", "app"}} {
		if r := run(t, srv.URL, "", args...); r.code != ExitOK || !strings.Contains(r.stdout, "PLAIN") || strings.Contains(r.stdout+r.stderr, "oidc-client-secret") || strings.Contains(r.stdout, value) {
			t.Errorf("%v: %+v, want PLAIN and not the client secret", args, r)
		}
	}
	r = run(t, srv.URL, "", "signin", "show", "app")
	for _, want := range []string{"Sign-in of app: OpenID Connect", "issuer: https://idp.example.com\n", "client ID: flats-app", "allowlist (2):\n    ann@example.com\n    sub:Zed\n",
		"client secret: oidc-client-secret, set ", "for issuer https://idp.example.com and client ID flats-app", "Portal host names: none", "callback URLs: none until",
		"Sign-in protects the Portal URL of the flat only"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("show lacks %q:\n%s", want, r.stdout)
		}
	}
	if r.code != ExitOK || strings.Contains(r.stdout, value) || strings.Contains(r.stdout, "warning") {
		t.Fatalf("show: %+v", r)
	}
	r = run(t, srv.URL, "", "--json", "signin", "show", "app")
	var v core.SignInView
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &v) != nil || v.Provider != "oidc" || len(v.Allow) != 2 || v.ClientSecret == nil || v.Hosts == nil || v.CallbackURLs == nil || strings.Contains(r.stdout, value) {
		t.Fatalf("show --json: %+v", r)
	}
	// A secret of another issuer: show warns that the route stays stopped (rule 27).
	if r := run(t, srv.URL, value+"\n", "signin", "secret", "set", "app", "--issuer", "https://other.example.com", "--client-id", "flats-app"); r.code != ExitOK {
		t.Fatalf("secret set for another issuer: %+v", r)
	}
	if r := run(t, srv.URL, "", "signin", "show", "app"); !strings.Contains(r.stdout, "warning: the client secret belongs to another issuer or client ID") ||
		!strings.Contains(r.stdout, "`flats signin secret set app --issuer https://idp.example.com --client-id flats-app`") {
		t.Fatalf("show with another binding: %+v", r)
	}
	if r := run(t, srv.URL, "", "signin", "revoke", "app"); r.code != ExitOK || !strings.Contains(r.stdout, "no Portal identity") {
		t.Fatalf("revoke: %+v", r)
	}
	if r := run(t, srv.URL, "", "signin", "secret", "rm", "app"); r.code != ExitOK || !strings.Contains(r.stdout, "removed") {
		t.Fatalf("secret rm: %+v", r)
	}
	if r := run(t, srv.URL, "", "signin", "secret", "rm", "app"); r.code != ExitError || !strings.Contains(r.stderr, "no sign-in client secret") {
		t.Fatalf("secret rm of no secret: %+v", r)
	}
	// A value that the host refuses is exit 1 with its message.
	if r := run(t, srv.URL, "", "signin", "set", "app", "oidc", "--issuer", "http://idp.example.com", "--client-id", "c", "--allow", "a@b.example"); r.code != ExitError || !strings.Contains(r.stderr, "https") {
		t.Fatalf("an http issuer: %+v", r)
	}
	if r := run(t, srv.URL, "", "signin", "set", "app", "credential", "--allow", "guest one"); r.code != ExitPending {
		t.Fatalf("set credential: %+v", r)
	}
	if r := run(t, srv.URL, "", "signin", "off", "app", "--reason", "later"); r.code != ExitPending {
		t.Fatalf("off: %+v", r)
	}
	if r := run(t, srv.URL, "", "signin", "show", "missing"); r.code != ExitError || strings.Contains(r.stderr, "older") {
		t.Fatalf("show of a missing flat: %+v", r)
	}
}
