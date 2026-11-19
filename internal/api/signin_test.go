package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
)

// The routes of docs/spec/portal-sign-in.md (rules 13 to 16, 25, 49, 54, 56 to 58 and 60) for each caller: the
// console, a loopback caller with the CLI header and without it, and a remote caller.

// Distinctive values that no answer of rule 58 or rule 60 may hold.
const (
	apiEntry  = "zed-api-entry@example.com"
	apiDomain = "@apidistinct.example"
	apiSub    = "ApiDistinct7"
	apiGuest  = "guest-api-distinct"
	apiSecret = "cs-api-distinct-41"
	apiLink   = "cred-api-distinct-77"
)

// signInPublic is a Portal backend that takes the sign-in that an open carries without running a gate, so that a flat
// with sign-in on can be Public in a test. Its access link holds apiLink.
type signInPublic struct{ *local.Public }

func (p *signInPublic) ServeSignIn(ctx context.Context, slug string, h http.Handler, hidden bool, _ *core.PortalSignIn) (string, error) {
	return p.Serve(ctx, slug, h, hidden)
}

func (p *signInPublic) RenewPortalIdentity(_ context.Context, slug string) (string, bool, error) {
	return "portal/" + slug + ".json", true, nil
}

func (p *signInPublic) IssuePortalCredential(_ context.Context, slug, _ string, _ time.Time) (core.PortalCredential, error) {
	host := slug + ".portal.example"
	return core.PortalCredential{URL: "https://" + host + "/_portal/auth/redeem#" + apiLink, Host: host}, nil
}

func (p *signInPublic) PortalHosts(slug string) []string {
	return []string{slug + ".portal.example", slug + "-alias.relay.example"}
}

func (p *signInPublic) FailPortalGate(_ context.Context, slug, reason string) error {
	return &core.SignInGateError{Reason: reason, StopErr: p.Stop(slug)}
}

func signInSetup(t *testing.T) (*httptest.Server, *core.Service, http.Handler) {
	t.Helper()
	srv, svc := setupWith(t, t.TempDir(), func(c *core.Config) { c.Public = &signInPublic{c.Public.(*local.Public)} })
	return srv, svc, (&api.Server{Svc: svc}).Handler()
}

// call sends a request and returns the status and the body as text.
func call(t *testing.T, method, url, body string, hdr map[string]string) (int, string) {
	t.Helper()
	r, _ := http.NewRequest(method, url, strings.NewReader(body))
	if !strings.Contains(url, "/console/api/") {
		r.Header.Set("X-Flats-Client", "test")
	}
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// remote sends a request of a caller that is not on the loopback address, with the CLI header when cli is true.
func remote(h http.Handler, method, path, body string, cli bool) (int, string) {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "203.0.113.1:12345"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Flats-Client", "test")
	if cli {
		r.Header.Set("X-Flats-Client", "cli")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

// noEntry fails when an answer holds an entry of the allowlist, the client secret or the access link.
func noEntry(t *testing.T, what, body string) {
	t.Helper()
	for _, s := range []string{apiEntry, apiDomain, apiSub, apiGuest, apiSecret, apiLink} {
		if strings.Contains(body, s) {
			t.Errorf("%s holds %q: %s", what, s, body)
		}
	}
}

func pendingCount(t *testing.T, svc *core.Service) int {
	t.Helper()
	as, err := svc.ListApprovals(context.Background(), "pending")
	if err != nil {
		t.Fatal(err)
	}
	return len(as)
}

func approveSignIn(t *testing.T, srv *httptest.Server, body string) {
	t.Helper()
	code, out := call(t, "POST", srv.URL+"/api/flats/app/signin", body, map[string]string{"X-Flats-Client": "cli"})
	var res struct {
		Approval struct {
			ID string `json:"id"`
		} `json:"approval"`
	}
	if code != 202 || json.Unmarshal([]byte(out), &res) != nil || res.Approval.ID == "" {
		t.Fatalf("request %s = %d %s", body, code, out)
	}
	approveRequest(t, srv, map[string]any{"approval": map[string]any{"id": res.Approval.ID}})
}

const oidcBody = `{"provider":"oidc","issuer":"https://idp.example.com/","client_id":"flats-app","allow":["` + apiEntry + `","` + apiDomain + `","sub:` + apiSub + `"],"reason":"try it"}`

func TestSignInRoutesByCaller(t *testing.T) {
	srv, svc, h := signInSetup(t)
	ctx := context.Background()
	if _, err := svc.CreateFlat(ctx, "app", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	cli := map[string]string{"X-Flats-Client": "cli"}
	console := consoleHdr(t, srv)
	base := srv.URL + "/api/flats/app/signin"
	consoleBase := srv.URL + "/console/api/flats/app/signin"
	secretBody := `{"value":"` + apiSecret + `","issuer":"https://idp.example.com/","client_id":"flats-app"}`

	// A remote caller gets 403 from every route, also with the CLI header, and changes nothing.
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/flats/app/signin", ""},
		{"POST", "/api/flats/app/signin", oidcBody},
		{"PUT", "/api/flats/app/signin/secret", secretBody},
		{"DELETE", "/api/flats/app/signin/secret", ""},
		{"POST", "/api/flats/app/signin/links", `{"subject":"` + apiGuest + `"}`},
		{"POST", "/api/flats/app/signin/revoke", ""},
	} {
		for _, withCLI := range []bool{true, false} {
			if code, out := remote(h, tc.method, tc.path, tc.body, withCLI); code != 403 {
				t.Errorf("remote %s %s (CLI header %v) = %d %s", tc.method, tc.path, withCLI, code, out)
			} else {
				noEntry(t, "a refusal of a remote caller", out)
			}
		}
	}
	// The client secret: only a loopback caller with the CLI header; the console has no route.
	for name, tc := range map[string]struct {
		method, url string
		hdr         map[string]string
		code        int
	}{
		"PUT without the CLI header":    {"PUT", base + "/secret", nil, 403},
		"DELETE without the CLI header": {"DELETE", base + "/secret", nil, 403},
		"PUT of the console":            {"PUT", consoleBase + "/secret", console, 404},
		"DELETE of the console":         {"DELETE", consoleBase + "/secret", console, 404},
	} {
		if code, out := call(t, tc.method, tc.url, secretBody, tc.hdr); code != tc.code {
			t.Errorf("%s = %d %s, want %d", name, code, out, tc.code)
		}
	}
	// An access link: only the console; /api gives 403 with the CLI header and without it.
	for _, hdr := range []map[string]string{cli, nil} {
		if code, out := call(t, "POST", base+"/links", `{"subject":"`+apiGuest+`"}`, hdr); code != 403 {
			t.Errorf("a link through /api (%v) = %d %s", hdr, code, out)
		}
	}
	if v, err := svc.GetSignIn(ctx, "app"); err != nil || v.ClientSecret != nil || v.Provider != "" || pendingCount(t, svc) != 0 {
		t.Fatalf("a refused request changed something: %+v %v", v, err)
	}

	// Rule 25 and 26: a set through the CLI header on loopback; each refusal is 400, holds no value and stores nothing.
	for _, body := range []string{
		`{"value":"has space","issuer":"https://idp.example.com","client_id":"flats-app"}`,
		`{"value":"` + apiSecret + `","issuer":"http://idp.example.com","client_id":"flats-app"}`,
		`{"value":"` + apiSecret + `","issuer":"https://idp.example.com"}`,
		`{"value":1,"issuer":"https://idp.example.com","client_id":"flats-app"}`,
		`{"value":"` + apiSecret + `","issuer":"https://idp.example.com","client_id":"flats-app","extra":1}`,
		`{"value":"` + apiSecret + `"`,
	} {
		if code, out := call(t, "PUT", base+"/secret", body, cli); code != 400 {
			t.Errorf("secret %s = %d %s", body, code, out)
		} else {
			noEntry(t, "a refused set of the client secret", out)
		}
	}
	if v, _ := svc.GetSignIn(ctx, "app"); v.ClientSecret != nil {
		t.Fatalf("a refused set stored a value: %+v", v.ClientSecret)
	}
	code, out := call(t, "PUT", base+"/secret", secretBody, cli)
	if code != 200 || !strings.Contains(out, `"status": "done"`) || !strings.Contains(out, "https://idp.example.com") {
		t.Fatalf("set the client secret = %d %s", code, out)
	}
	noEntry(t, "the answer of a set", out)

	// Rules 13 to 16: a loopback caller with or without the CLI header and the console request a change. The answer of
	// /api holds the counted settings; the console keeps the entries.
	code, out = call(t, "POST", base, oidcBody, cli)
	if code != 202 || !strings.Contains(out, `"pending_approval"`) || !strings.Contains(out, `"count": 3`) {
		t.Fatalf("a request through the CLI = %d %s", code, out)
	}
	noEntry(t, "the answer of a request", out)
	var first struct {
		Approval struct {
			ID string `json:"id"`
		} `json:"approval"`
	}
	_ = json.Unmarshal([]byte(out), &first)
	id := first.Approval.ID
	code, out = call(t, "POST", base, oidcBody, nil)
	if code != 202 || !strings.Contains(out, id) {
		t.Fatalf("an equal request without the CLI header = %d %s", code, out)
	}
	noEntry(t, "the answer of an equal request", out)
	if code, out = call(t, "POST", consoleBase, oidcBody, console); code != 202 || !strings.Contains(out, id) || !strings.Contains(out, apiEntry) {
		t.Fatalf("an equal request of the console = %d %s", code, out)
	}
	// Rule 14: each refusal is 400, names the member, and never repeats an issuer.
	for body, says := range map[string]string{
		`{"provider":"oidc","issuer":"https://user:pw-distinct@idp.example.com","client_id":"c","allow":["a@b.example"]}`:  "user information",
		`{"provider":"oidc","issuer":"https://idp.example.com/?q=query-distinct","client_id":"c","allow":["a@b.example"]}`: "query",
		`{"provider":"credential","allow":["x"],"extra":1}`:                                                                `extra\" is not a member`,
		`{"provider":"credential","allow":[]}`:                                                                             "member allow",
		`{"provider":"credential","allow":["x"],"issuer":"https://idp.example.com"}`:                                       "member issuer",
		`not json`: "JSON object",
		``:         "JSON object",
	} {
		code, out := call(t, "POST", base, body, cli)
		if code != 400 || !strings.Contains(out, says) || strings.Contains(out, "pw-distinct") || strings.Contains(out, "query-distinct") {
			t.Errorf("request %s = %d %s, want 400 with %q", body, code, out, says)
		}
	}
	if pendingCount(t, svc) != 1 {
		t.Fatal("a refused request made an approval")
	}

	// Rule 58: the approval routes of /api show the counted settings to every caller; the console page keeps them.
	for name, get := range map[string]func(path string) (int, string){
		"cli":           func(p string) (int, string) { return call(t, "GET", srv.URL+p, "", cli) },
		"loopback":      func(p string) (int, string) { return call(t, "GET", srv.URL+p, "", nil) },
		"remote":        func(p string) (int, string) { return remote(h, "GET", p, "", false) },
		"remote as cli": func(p string) (int, string) { return remote(h, "GET", p, "", true) },
	} {
		for _, p := range []string{"/api/approvals", "/api/approvals?status=pending", "/api/approvals/" + id} {
			code, out := get(p)
			if code != 200 || !strings.Contains(out, `\"count\":3`) && !strings.Contains(out, `"count": 3`) {
				t.Errorf("%s GET %s = %d %s", name, p, code, out)
			}
			noEntry(t, name+" GET "+p, out)
		}
	}
	if code, out := call(t, "GET", srv.URL+"/console/api/approvals/"+id, "", console); code != 200 || !strings.Contains(out, apiEntry) || !strings.Contains(out, "sub:"+apiSub) {
		t.Fatalf("the console approval = %d %s", code, out)
	}
	approveRequest(t, srv, map[string]any{"approval": map[string]any{"id": id}})
	// Rule 15: a request for the stored setting is done and makes no approval.
	if code, out := call(t, "POST", base, oidcBody, cli); code != 200 || !strings.Contains(out, `"sign-in unchanged"`) || strings.Contains(out, `"approval"`) || pendingCount(t, svc) != 0 {
		t.Fatalf("a request for the stored setting = %d %s", code, out)
	}

	// Rule 57: the flat view has sign_in without an entry, for every caller.
	for name, get := range map[string]func(path string) (int, string){
		"cli":     func(p string) (int, string) { return call(t, "GET", srv.URL+p, "", cli) },
		"remote":  func(p string) (int, string) { return remote(h, "GET", p, "", false) },
		"console": func(p string) (int, string) { return call(t, "GET", srv.URL+"/console"+p, "", console) },
	} {
		for _, p := range []string{"/api/flats/app", "/api/flats"} {
			code, out := get(p)
			if code != 200 || !strings.Contains(out, `"sign_in": {`) || !strings.Contains(out, `"allow_count": 3`) ||
				!strings.Contains(out, `"issuer": "https://idp.example.com"`) || !strings.Contains(out, `"client_id": "flats-app"`) ||
				!strings.Contains(out, `"name": "oidc-client-secret"`) {
				t.Errorf("%s GET %s = %d %s", name, p, code, out)
			}
			noEntry(t, name+" GET "+p, out)
		}
	}
	// Rule 56: the console and a loopback caller read the setting with its entries, the binding and the host names.
	for name, tc := range map[string]struct {
		url string
		hdr map[string]string
	}{"cli": {base, cli}, "loopback": {base, nil}, "console": {consoleBase, console}} {
		code, out := call(t, "GET", tc.url, "", tc.hdr)
		var v core.SignInView
		if code != 200 || json.Unmarshal([]byte(out), &v) != nil {
			t.Fatalf("%s GET signin = %d %s", name, code, out)
		}
		if v.Provider != "oidc" || v.Issuer != "https://idp.example.com" || v.ClientID != "flats-app" || v.Seq != 1 ||
			fmt.Sprint(v.Allow) != "["+apiDomain+" sub:"+apiSub+" "+apiEntry+"]" ||
			v.ClientSecret == nil || v.ClientSecret.Name != "oidc-client-secret" || v.ClientSecret.Issuer != "https://idp.example.com" || v.ClientSecret.ClientID != "flats-app" ||
			fmt.Sprint(v.Hosts) != "[app.portal.example app-alias.relay.example]" ||
			fmt.Sprint(v.CallbackURLs) != "[https://app.portal.example/_portal/auth/callback https://app-alias.relay.example/_portal/auth/callback]" {
			t.Errorf("%s GET signin = %+v", name, v)
		}
		if strings.Contains(out, apiSecret) {
			t.Errorf("%s GET signin holds the client secret: %s", name, out)
		}
	}
	// Rule 50: a link needs a credential setting.
	if code, out := call(t, "POST", consoleBase+"/links", `{"subject":"`+apiGuest+`"}`, console); code != 409 {
		t.Fatalf("a link of an oidc setting = %d %s", code, out)
	}

	approveSignIn(t, srv, `{"provider":"credential","allow":["`+apiGuest+`"]}`)
	// Rule 49: the console issues the link; the default expiry is 7 days.
	before := time.Now()
	code, out = call(t, "POST", consoleBase+"/links", `{"subject":" `+apiGuest+` "}`, console)
	var link core.SignInLink
	if code != 201 || json.Unmarshal([]byte(out), &link) != nil || !strings.Contains(link.URL, apiLink) || link.Subject != apiGuest || link.Host != "app.portal.example" ||
		link.ExpiresAt.Before(before.Add(7*24*time.Hour-time.Minute)) || link.ExpiresAt.After(time.Now().Add(7*24*time.Hour)) {
		t.Fatalf("a link of the console = %d %s", code, out)
	}
	at := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	for body, want := range map[string]time.Time{
		`{"subject":"` + apiGuest + `","expires":""}`:                                {},
		`{"subject":"` + apiGuest + `","expires":"` + at.Format(time.RFC3339) + `"}`: at,
	} {
		code, out := call(t, "POST", consoleBase+"/links", body, console)
		_ = json.Unmarshal([]byte(out), &link)
		if code != 201 || !want.IsZero() && !link.ExpiresAt.Equal(want) || want.IsZero() && link.ExpiresAt.Before(time.Now().Add(7*24*time.Hour-time.Minute)) {
			t.Errorf("link %s = %d %s", body, code, out)
		}
	}
	for body, code := range map[string]int{
		`{"subject":"` + apiGuest + `","expires":5}`:                                                                    400,
		`{"subject":"` + apiGuest + `","expires":null}`:                                                                 400,
		`{"subject":"` + apiGuest + `","expires":"tomorrow"}`:                                                           400,
		`{"subject":"` + apiGuest + `","expires":"` + time.Now().Add(91*24*time.Hour).UTC().Format(time.RFC3339) + `"}`: 400,
		`{"subject":"` + apiGuest + `","expires":"` + time.Now().Add(30*time.Second).UTC().Format(time.RFC3339) + `"}`:  400,
		`{"subject":"` + apiGuest + `","other":1}`:                                                                      400,
		`{"subject":3}`:        400,
		`{"subject":null}`:     400,
		`{}`:                   400,
		`{"subject":"nobody"}`: 400,
	} {
		if got, out := call(t, "POST", consoleBase+"/links", body, console); got != code {
			t.Errorf("link %s = %d %s, want %d", body, got, out, code)
		} else if strings.Contains(out, apiLink) {
			t.Errorf("a refused link holds the link: %s", out)
		}
	}
	for _, hdr := range []map[string]string{cli, nil} {
		if code, out := call(t, "POST", base+"/links", `{"subject":"`+apiGuest+`"}`, hdr); code != 403 {
			t.Errorf("a link of a credential setting through /api (%v) = %d %s", hdr, code, out)
		}
	}

	// Rule 54: a loopback caller with or without the CLI header and the console end the sessions and links.
	for name, tc := range map[string]struct {
		url string
		hdr map[string]string
	}{"cli": {base + "/revoke", cli}, "loopback": {base + "/revoke", nil}, "console": {consoleBase + "/revoke", console}} {
		if code, out := call(t, "POST", tc.url, "", tc.hdr); code != 200 || !strings.Contains(out, "every sign-in session and access link of app ended") {
			t.Errorf("%s revoke = %d %s", name, code, out)
		}
	}

	// Rule 25: the removal.
	if code, out := call(t, "DELETE", base+"/secret", "", cli); code != 200 || !strings.Contains(out, "removed") {
		t.Fatalf("rm = %d %s", code, out)
	}
	if code, out := call(t, "DELETE", base+"/secret", "", cli); code != 404 {
		t.Fatalf("rm of no secret = %d %s", code, out)
	}

	// No answer to a remote caller and no log holds an entry, the secret or the link (rules 52, 58 and 60).
	for _, p := range []string{"/api/flats/app/logs?limit=1000", "/api/flats/app", "/api/flats", "/api/approvals", "/api/flats/app/secrets"} {
		code, out := remote(h, "GET", p, "", false)
		if code != 200 {
			t.Errorf("remote GET %s = %d %s", p, code, out)
		}
		noEntry(t, "remote GET "+p, out)
	}
	if code, out := call(t, "GET", srv.URL+"/api/flats/app/logs?limit=1000", "", cli); code != 200 || !strings.Contains(out, "access link issued") {
		t.Fatalf("logs = %d %s", code, out)
	} else {
		noEntry(t, "the logs", out)
	}
}

// A request through a declared external origin comes from the reverse proxy of the operator, also when it arrives on
// the loopback address, so each route of sign-in refuses it with 403, with the CLI header and without it, and changes
// nothing (rules 13, 25, 49, 54 and 56).
func TestSignInRoutesThroughAnExternalOrigin(t *testing.T) {
	_, svc, h := signInSetup(t)
	ctx := context.Background()
	if _, err := svc.CreateFlat(ctx, "app", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	const origin = "https://flats.example.org"
	guard := &api.HostGuard{Hosts: func() []string { return []string{"localhost"} }, ExternalOrigins: func() []string { return []string{origin} }, Next: h}
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/flats/app/signin", ""},
		{"POST", "/api/flats/app/signin", oidcBody},
		{"PUT", "/api/flats/app/signin/secret", `{"value":"` + apiSecret + `","issuer":"https://idp.example.com","client_id":"flats-app"}`},
		{"DELETE", "/api/flats/app/signin/secret", ""},
		{"POST", "/api/flats/app/signin/links", `{"subject":"` + apiGuest + `"}`},
		{"POST", "/api/flats/app/signin/revoke", ""},
	} {
		for _, withCLI := range []bool{true, false} {
			r := httptest.NewRequest(tc.method, origin+tc.path, strings.NewReader(tc.body))
			r.Host = "flats.example.org"
			r.RemoteAddr = "127.0.0.1:12345"
			r.Header.Set("Content-Type", "application/json")
			if withCLI {
				r.Header.Set("X-Flats-Client", "cli")
			}
			rec := httptest.NewRecorder()
			guard.ServeHTTP(rec, r)
			if rec.Code != 403 {
				t.Errorf("%s %s through %s (CLI header %v) = %d %s", tc.method, tc.path, origin, withCLI, rec.Code, rec.Body)
			}
			noEntry(t, "a refusal through an external origin", rec.Body.String())
		}
	}
	if v, err := svc.GetSignIn(ctx, "app"); err != nil || v.ClientSecret != nil || v.Provider != "" || v.Seq != 0 || pendingCount(t, svc) != 0 {
		t.Fatalf("a refused request changed the sign-in: %+v %v", v, err)
	}
	events, err := svc.Events(ctx, "app", "", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if strings.Contains(e.Message, "sign-in") || strings.Contains(e.Message, "access link") {
			t.Errorf("a refused request wrote %q", e.Message)
		}
	}
}

// Rule 61: the public notice and the pending notice of a flat with sign-in on.
func TestSignInNoticesOnTheAPI(t *testing.T) {
	srv, _, _ := signInSetup(t)
	saveAndPublish(t, srv, "app", "ok")
	console := consoleHdr(t, srv)
	if code, out := req(t, "POST", srv.URL+"/console/api/flats/app/providers", strings.NewReader(`{"provider":"portal","permitted":true}`), console); code != 200 {
		t.Fatalf("grant: %d %v", code, out)
	}
	approveSignIn(t, srv, `{"provider":"credential","allow":["`+apiGuest+`"]}`)
	code, out := req(t, "POST", srv.URL+"/api/flats/app/visibility", strings.NewReader(`{"visibility":"public"}`), nil)
	if code != 202 || out["notice"] != core.PendingSignInPublicNotice {
		t.Fatalf("pending notice: %d %v", code, out)
	}
	approveRequest(t, srv, out)
	_, f := req(t, "GET", srv.URL+"/api/flats/app", nil, nil)
	if f["visibility"] != "public" || f["public_notice"] != core.SignInPublicNotice || f["sign_in"] == nil {
		t.Fatalf("a Public flat with sign-in on: %v", f)
	}
	approveSignIn(t, srv, `{"provider":""}`)
	_, f = req(t, "GET", srv.URL+"/api/flats/app", nil, nil)
	if _, ok := f["sign_in"]; ok || f["public_notice"] != core.PublicAccessNotice {
		t.Fatalf("a Public flat with sign-in off: %v", f)
	}
}
