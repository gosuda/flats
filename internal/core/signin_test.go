package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
)

// docs/spec/portal-sign-in.md in the service: the setting, its request and its approval, the refusals of rule 8 and
// the client secret.

// signInValue is a client secret that no text of the host may hold (rule 30).
const signInValue = "cs-distinctive-7Qx9"

var oidcRequest = SignInRequest{Provider: "oidc", Issuer: " https://idp.example.com/ ", ClientID: " flats-app ",
	Allow: []string{" Ann@Example.COM ", "@Example.org", "SUB:AbC", "ann@example.com"}}

// oidcStored is oidcRequest as the host stores it: trimmed, the issuer without its trailing /, emails and domains in
// lower case, the subject byte for byte, without duplicates, in byte order.
var oidcStored = store.SignIn{Provider: "oidc", Issuer: "https://idp.example.com", ClientID: "flats-app", Allow: []string{"@example.org", "ann@example.com", "sub:AbC"}}

var guestRequest = SignInRequest{Provider: "credential", Allow: []string{"guest one", "guest two"}}

// noSignInText fails when text holds an entry of oidcStored or the client secret.
func noSignInText(t *testing.T, what, text string) {
	t.Helper()
	for _, s := range []string{"ann@example.com", "@example.org", "sub:AbC", "SUB:AbC", "guest one", signInValue} {
		if strings.Contains(text, s) {
			t.Errorf("%s holds %q: %s", what, s, text)
		}
	}
}

// requestSignIn requests req for slug through the CLI and returns the pending approval.
func requestSignIn(t *testing.T, s *Service, slug string, req SignInRequest) store.Approval {
	t.Helper()
	r, err := s.RequestSignIn(t.Context(), slug, req, ViaCLI)
	if err != nil || r.Status != "pending_approval" || r.Approval == nil {
		t.Fatalf("RequestSignIn(%s) = %+v, %v", slug, r, err)
	}
	return *r.Approval
}

// approveSignIn requests req for slug and approves it in the console.
func approveSignIn(t *testing.T, s *Service, slug string, req SignInRequest) store.Approval {
	t.Helper()
	a, err := s.Decide(t.Context(), requestSignIn(t, s, slug, req).ID, true)
	if err != nil || a.Status != "approved" {
		t.Fatalf("Decide = %+v, %v", a, err)
	}
	return a
}

func signInService(t *testing.T) *Service {
	t.Helper()
	s, _ := newTestService(t)
	if _, err := s.CreateFlat(t.Context(), "app", "", ViaCLI); err != nil {
		t.Fatal(err)
	}
	return s
}

func signInRows(t *testing.T, s *Service, dir, kind string) []store.AuditEvent {
	t.Helper()
	rd, err := store.OpenAuditReader(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	rows, err := rd.List(t.Context(), store.AuditQuery{Kind: kind}, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestDecodeSignInRequest(t *testing.T) {
	t.Parallel()
	for body, want := range map[string]SignInRequest{
		`{"provider":"oidc","issuer":"https://i.example","client_id":"c","allow":["a@b.c"],"reason":"r"}`: {Provider: "oidc", Issuer: "https://i.example", ClientID: "c", Allow: []string{"a@b.c"}, Reason: "r"},
		`{"provider":"credential","allow":["x"]}`:                                                         {Provider: "credential", Allow: []string{"x"}},
		`{"provider":"","reason":"why"}`:                                                                  {Reason: "why"},
	} {
		if got, err := DecodeSignInRequest([]byte(body)); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %+v, %v", body, got, err)
		}
	}
	for body, says := range map[string]string{
		`[]`:                  "JSON object",
		`{}`:                  "member provider is missing",
		`{"provider":1}`:      "member provider must be a string",
		`{"provider":"saml"}`: "member provider must be oidc, credential",
		`{"provider":"credential","allow":["x"],"extra":1}`:                    `member "extra" is not a member`,
		`{"provider":"credential","allow":["x"],"issuer":"https://i.example"}`: "member issuer is not taken by the provider credential",
		`{"provider":"credential","allow":["x"],"client_id":"c"}`:              "member client_id is not taken by the provider credential",
		`{"provider":"","allow":[]}`:                                           "member allow is not taken by a request that turns sign-in off",
		`{"provider":"oidc","client_id":"c","allow":["a@b.c"]}`:                "member issuer is missing",
		`{"provider":"oidc","issuer":"https://i.example","allow":["a@b.c"]}`:   "member client_id is missing",
		`{"provider":"credential"}`:                                            "member allow is missing",
		`{"provider":"credential","allow":"x"}`:                                "member allow must be a list of strings",
		`{"provider":"credential","allow":null}`:                               "member allow must be a list of strings",
		`{"provider":"oidc","issuer":null,"client_id":"c","allow":["a@b.c"]}`:  "member issuer must be a string",
	} {
		if _, err := DecodeSignInRequest([]byte(body)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want %q", body, err, says)
		}
	}
}

// A request is normalized by rules 2 to 4 and frozen by rule 16; a pending one changes nothing, an equal one returns
// it, and a request for the stored setting is done (rule 15).
func TestSignInRequestNormalizesAndFreezes(t *testing.T) {
	t.Parallel()
	s := signInService(t)
	ctx := t.Context()
	if _, err := s.SetSignInSecret(ctx, "app", signInValue, "https://idp.example.com", "flats-app", ViaCLI); err != nil {
		t.Fatal(err)
	}
	a := requestSignIn(t, s, "app", oidcRequest)
	f, _ := s.st.GetFlat(ctx, "app")
	want := fmt.Sprintf(`{"flat_id":"%s","seq":0,"from":{"provider":"","allow":[]},"to":{"provider":"oidc","issuer":"https://idp.example.com","client_id":"flats-app","allow":["@example.org","ann@example.com","sub:AbC"]},"public_grants":[]}`, f.ID)
	if string(a.Params) != want || a.Action != "set_signin" || a.Via != "cli" {
		t.Fatalf("frozen params:\n%s\nwant\n%s", a.Params, want)
	}
	if strings.Contains(string(a.Params), signInValue) {
		t.Fatalf("the frozen params hold the client secret: %s", a.Params)
	}
	if v, _ := s.GetSignIn(ctx, "app"); v.Provider != "" || v.Seq != 0 {
		t.Fatalf("a pending request changed the setting: %+v", v)
	}
	again, err := s.RequestSignIn(ctx, "app", SignInRequest{Provider: "oidc", Issuer: "https://idp.example.com", ClientID: "flats-app",
		Allow: []string{"sub:AbC", "ann@example.com", "@example.org"}}, ViaAPI)
	if err != nil || again.Approval == nil || again.Approval.ID != a.ID {
		t.Fatalf("an equal request: %+v, %v", again, err)
	}
	evs, _ := s.st.ListEvents(ctx, "app", "approval", 0, 100)
	if len(evs) != 1 {
		t.Fatalf("request events = %+v", evs)
	}
	noSignInText(t, "the event of the request", evs[0].Message+string(evs[0].Data))
	if !strings.Contains(string(evs[0].Data), `"to":{"provider":"oidc","issuer":"https://idp.example.com","client_id":"flats-app","count":3}`) {
		t.Errorf("the event of the request lacks the counted setting: %s", evs[0].Data)
	}
	if _, err := s.Decide(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	r, err := s.RequestSignIn(ctx, "app", oidcRequest, ViaConsole)
	if err != nil || r.Status != "done" || r.Message != "sign-in unchanged" || r.Approval != nil {
		t.Fatalf("a request for the stored setting: %+v, %v", r, err)
	}
	if pend, _ := s.ListApprovals(ctx, "pending"); len(pend) != 0 {
		t.Fatalf("pending approvals: %+v", pend)
	}
}

// Each value that rules 2 to 4 or the SDK refuse gives 400 and names the member; the message for an issuer does not
// repeat the text of the caller (rule 14). MCP cannot request a change (rule 13).
func TestSignInRequestRefusals(t *testing.T) {
	t.Parallel()
	s := signInService(t)
	ctx := t.Context()
	oidc := func(issuer string) SignInRequest {
		return SignInRequest{Provider: "oidc", Issuer: issuer, ClientID: "flats-app", Allow: []string{"ann@example.com"}}
	}
	withID := func(id string) SignInRequest { r := oidc("https://idp.example.com"); r.ClientID = id; return r }
	withAllow := func(provider string, allow ...string) SignInRequest {
		r := SignInRequest{Provider: provider, Allow: allow}
		if provider == "oidc" {
			r.Issuer, r.ClientID = "https://idp.example.com", "flats-app"
		}
		return r
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = fmt.Sprintf("guest %d", i)
	}
	for name, tc := range map[string]struct {
		req         SignInRequest
		says, hides string
	}{
		"an http issuer":             {oidc("http://idp.example.com"), "must be an https URL", ""},
		"http on a loopback host":    {oidc("http://127.0.0.1:8080"), "also on a loopback host", ""},
		"an issuer without a host":   {oidc("https:///realm"), "with a host name", ""},
		"user information":           {oidc("https://user:pass-secret@idp.example.com"), "without user information", "pass-secret"},
		"a query":                    {oidc("https://idp.example.com/?q=query-secret"), "without a query", "query-secret"},
		"a fragment":                 {oidc("https://idp.example.com/#frag-secret"), "or a fragment", "frag-secret"},
		"a space in the issuer":      {oidc("https://idp.example.com/a b-secret"), "member issuer has a character", "b-secret"},
		"a long issuer":              {oidc("https://idp.example.com/" + strings.Repeat("p", maxSignInIssuer)), "more than 2048 bytes", ""},
		"no issuer":                  {oidc(" "), "member issuer is missing", ""},
		"no client ID":               {withID(""), "member client_id is missing", ""},
		"a long client ID":           {withID(strings.Repeat("c", 257)), "more than 256 bytes", ""},
		"a client ID with a space":   {withID("flats app"), "visible ASCII", ""},
		"a client ID not ASCII":      {withID("flats-äpp"), "visible ASCII", ""},
		"an empty list":              {withAllow("credential"), "at least one entry", ""},
		"an empty oidc list":         {withAllow("oidc"), "at least one entry", ""},
		"101 entries":                {withAllow("credential", many...), "at most 100", ""},
		"a long subject":             {withAllow("credential", strings.Repeat("s", 257)), "member allow", ""},
		"a long sub: entry":          {withAllow("oidc", "sub:"+strings.Repeat("s", 257)), "member allow", ""},
		"a long email":               {withAllow("oidc", strings.Repeat("e", 250)+"@example.com"), "member allow", ""},
		"a subject not ASCII":        {withAllow("credential", "José"), "not visible ASCII or a space", ""},
		"a sub: entry not ASCII":     {withAllow("oidc", "sub:José"), "not visible ASCII or a space", ""},
		"a control character":        {withAllow("credential", "a\x01b"), "member allow", ""},
		"a bare token":               {withAllow("oidc", "bob"), "must be an email, an @example.com domain, or a sub:<subject> entry", ""},
		"an email not ASCII":         {withAllow("oidc", "ä@example.com"), "must be ASCII", ""},
		"an issuer for credential":   {SignInRequest{Provider: "credential", Issuer: "https://idp.example.com", Allow: []string{"x"}}, "not taken by the provider credential", ""},
		"a client ID for credential": {SignInRequest{Provider: "credential", ClientID: "c", Allow: []string{"x"}}, "not taken by the provider credential", ""},
		"a list for off":             {SignInRequest{Allow: []string{"x"}}, "member allow is not taken", ""},
		"an issuer for off":          {SignInRequest{Issuer: "https://idp.example.com"}, "member issuer is not taken", ""},
		"another provider":           {SignInRequest{Provider: "saml", Allow: []string{"x"}}, "must be oidc, credential", ""},
		"a long reason":              {SignInRequest{Provider: "credential", Allow: []string{"x"}, Reason: strings.Repeat("r", 1025)}, "at most 1024", ""},
	} {
		r, err := s.RequestSignIn(ctx, "app", tc.req, ViaCLI)
		if !errors.Is(err, ErrInvalid) || ErrorCategory(err) != "invalid" || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: %+v %v, want 400 with %q", name, r, err, tc.says)
			continue
		}
		if tc.hides != "" && strings.Contains(err.Error(), tc.hides) {
			t.Errorf("%s: the message repeats %q: %v", name, tc.hides, err)
		}
	}
	if _, err := s.RequestSignIn(ctx, "app", guestRequest, ViaMCP); !errors.Is(err, ErrForbidden) {
		t.Errorf("MCP: %v", err)
	}
	if all, _ := s.ListApprovals(ctx, ""); len(all) != 0 {
		t.Fatalf("refused requests made approvals: %+v", all)
	}
	// The largest values that the rules allow pass.
	subjects := make([]string, 100)
	for i := range subjects {
		subjects[i] = fmt.Sprintf("guest %03d", i)
	}
	issuer := "https://idp.example.com/" + strings.Repeat("p", maxSignInIssuer-len("https://idp.example.com/"))
	for name, req := range map[string]SignInRequest{
		"100 entries":                  withAllow("credential", subjects...),
		"a subject of 256 bytes":       withAllow("credential", strings.Repeat("s", 256)),
		"a sub: entry of 256 bytes":    withAllow("oidc", "sub:"+strings.Repeat("s", 256)),
		"an issuer of 2048 bytes":      oidc(issuer),
		"a client ID of 256 bytes":     withID(strings.Repeat("c", 256)),
		"a subject with a space and !": withAllow("credential", `guest "one" ! ~`),
	} {
		if _, err := s.RequestSignIn(ctx, "app", req, ViaCLI); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A console decision applies the setting by rule 20: the result, the flat event, the audit row of rule 65 and the
// webhook events of rules 68 and 69 hold no entry; a change and its reverse make an earlier request stale (rule 19).
func TestSignInApprovalApplies(t *testing.T) {
	t.Parallel()
	s, dir, rec := webhookService(t)
	ctx := t.Context()
	if _, err := s.CreateFlat(ctx, "app", "", ViaCLI); err != nil {
		t.Fatal(err)
	}
	f, _ := s.st.GetFlat(ctx, "app")
	var calls []string
	s.signInHooks.applied = func(_ context.Context, slug string, in store.SignIn, seq int64) error {
		calls = append(calls, fmt.Sprintf("%s %s %d %d", slug, in.Provider, len(in.Allow), seq))
		return nil
	}
	a := requestSignIn(t, s, "app", oidcRequest)
	const actor = `tailnet user "op@example.net"`
	got, err := s.Decide(WithAuditActor(ctx, actor), a.ID, true)
	if err != nil || got.Status != "approved" || got.Result != "sign-in set to oidc with issuer https://idp.example.com and 3 entries" || execution(t, got).Status != "approved" {
		t.Fatalf("Decide = %+v, %v", got, err)
	}
	if v, _ := s.GetSignIn(ctx, "app"); !slices.Equal(v.Allow, oidcStored.Allow) || v.Issuer != oidcStored.Issuer || v.ClientID != oidcStored.ClientID || v.Seq != 1 {
		t.Fatalf("GetSignIn = %+v", v)
	}
	if !slices.Equal(calls, []string{"app oidc 3 1"}) {
		t.Errorf("steps 3 and 4 ran %q", calls)
	}
	rows := signInRows(t, s, dir, store.AuditSignInSet)
	if len(rows) != 1 || rows[0].Via != "console" || rows[0].Actor != actor || rows[0].FlatID != f.ID || !strings.Contains(string(rows[0].Data), `"approval":"`+a.ID+`"`) {
		t.Fatalf("signin.set rows = %+v", rows)
	}
	evs, _ := s.st.ListEvents(ctx, "app", "", 0, 100)
	sawSignIn := false
	for _, e := range evs {
		noSignInText(t, "a flat event", e.Message+string(e.Data))
		sawSignIn = sawSignIn || e.Kind == "signin" && e.Message == got.Result
	}
	if !sawSignIn {
		t.Errorf("no flat event of the change: %+v", evs)
	}
	noSignInText(t, "the approval", got.Result+string(got.ResultData))
	ps := rec.take()
	if want := []string{"approval.created set_signin pending", "signin.changed set_signin applied by operator", "approval.decided set_signin approved by operator"}; !slices.Equal(shorts(ps), want) {
		t.Fatalf("events = %q, want %q", shorts(ps), want)
	}
	for _, p := range ps {
		b, _ := json.Marshal(p)
		noSignInText(t, "a webhook body", string(b))
		if p.Type == eventSignInChange {
			change, _ := json.Marshal(p.Change)
			if string(change) != `{"from":{"provider":"","count":0},"to":{"provider":"oidc","count":3}}` || p.ApprovalID != a.ID || p.Flat.ID != f.ID || p.ConsoleURL != "http://console.test:9/flats/app" {
				t.Errorf("signin.changed = %s (change %s)", b, change)
			}
		}
	}
	// Rule 19: a request at sequence 1, then a change and its reverse.
	earlier := requestSignIn(t, s, "app", guestRequest)
	off := approveSignIn(t, s, "app", SignInRequest{})
	if off.Result != "sign-in turned off" {
		t.Errorf("off: %q", off.Result)
	}
	approveSignIn(t, s, "app", oidcRequest)
	stale, err := s.Decide(ctx, earlier.ID, true)
	if !errors.Is(err, ErrStaleApproval) || stale.Status != "failed" || execution(t, stale).FailureCode != "stale_approval" || !strings.Contains(err.Error(), "sign-in of app changed after the request") {
		t.Fatalf("the earlier request: %+v, %v", stale, err)
	}
	if v, _ := s.GetSignIn(ctx, "app"); v.Seq != 3 || v.Provider != "oidc" {
		t.Fatalf("after the stale approval: %+v", v)
	}
	if n := len(signInRows(t, s, dir, store.AuditSignInSet)); n != 3 {
		t.Errorf("%d signin.set rows, want 3", n)
	}
}

// A redeploy, a rollback, a visibility change, a client secret change and a change of the host settings of a provider
// leave a sign-in approval valid; a grant of a public provider and a new flat under the slug make it stale (rule 18).
func TestSignInApprovalStaleness(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	t.Run("valid", func(t *testing.T) {
		s, _ := newTestService(t)
		n := &lifecycleNetwork{policy: "first", state: "ready"}
		s.cfg.Lifecycle = n
		lifecycleSave(t, s, "app", "one")
		lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
		if err := s.SetProviderPermission(ctx, "app", store.ProviderPortal, true, ViaConsole); err != nil {
			t.Fatal(err)
		}
		a := requestSignIn(t, s, "app", guestRequest)
		lifecycleSave(t, s, "app", "two")
		lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
		if _, err := s.Rollback(ctx, "app", 1, ViaConsole); err != nil {
			if _, err := s.Decide(ctx, pendingID(t, err), true); err != nil {
				t.Fatalf("rollback: %v", err)
			}
		}
		if _, err := s.Deploy(ctx, "app", 1, ViaConsole); err != nil {
			if _, err := s.Decide(ctx, pendingID(t, err), true); err != nil {
				t.Fatalf("redeploy: %v", err)
			}
		}
		r, err := s.SetVisibility(ctx, "app", store.Public, ViaConsole, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Decide(ctx, r.Approval.ID, true); err != nil {
			t.Fatalf("visibility: %v", err)
		}
		if _, err := s.SetSignInSecret(ctx, "app", signInValue, "https://idp.example.com", "flats-app", ViaCLI); err != nil {
			t.Fatal(err)
		}
		n.policy = "second"
		if got, err := s.Decide(ctx, a.ID, true); err != nil || got.Status != "approved" {
			t.Fatalf("the sign-in approval: %+v, %v", got, err)
		}
		// Rule 6: a redeploy and a rollback keep the setting.
		if _, err := s.Rollback(ctx, "app", 2, ViaConsole); err != nil {
			if _, err := s.Decide(ctx, pendingID(t, err), true); err != nil {
				t.Fatalf("rollback: %v", err)
			}
		}
		if _, err := s.Deploy(ctx, "app", 2, ViaConsole); err != nil {
			if _, err := s.Decide(ctx, pendingID(t, err), true); err != nil {
				t.Fatalf("redeploy: %v", err)
			}
		}
		if v, _ := s.GetSignIn(ctx, "app"); v.Provider != "credential" || v.Seq != 1 || len(v.Allow) != 2 {
			t.Fatalf("after a rollback and a redeploy: %+v", v)
		}
	})
	t.Run("a grant", func(t *testing.T) {
		s := signInService(t)
		a := requestSignIn(t, s, "app", guestRequest)
		if err := s.SetProviderPermission(ctx, "app", store.ProviderPortal, true, ViaConsole); err != nil {
			t.Fatal(err)
		}
		got, err := s.Decide(ctx, a.ID, true)
		if !errors.Is(err, ErrStaleApproval) || !strings.Contains(err.Error(), "public providers granted to app changed after the request (now portal)") || got.Status != "failed" {
			t.Fatalf("after a grant: %+v, %v", got, err)
		}
		// A private provider is no public grant.
		b := requestSignIn(t, s, "app", guestRequest)
		if err := s.SetProviderPermission(ctx, "app", store.ProviderTailscale, true, ViaConsole); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Decide(ctx, b.ID, true); err != nil || got.Status != "approved" {
			t.Fatalf("after a private grant: %+v, %v", got, err)
		}
		// A revocation.
		c := requestSignIn(t, s, "app", SignInRequest{})
		if err := s.SetProviderPermission(ctx, "app", store.ProviderPortal, false, ViaConsole); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Decide(ctx, c.ID, true); !errors.Is(err, ErrStaleApproval) || !strings.Contains(err.Error(), "(now none)") {
			t.Fatalf("after a revocation: %v", err)
		}
	})
	t.Run("another flat", func(t *testing.T) {
		s := signInService(t)
		params, _ := json.Marshal(signInParams{FlatID: "a-deleted-flat", To: store.SignIn{Provider: "credential", Allow: []string{"x"}}, PublicGrants: []string{}})
		if err := s.st.InsertApproval(ctx, store.Approval{ID: "apr-old", Flat: "app", Action: actionSetSignIn, Params: params, Status: "pending", Via: "cli", RequestedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Decide(ctx, "apr-old", true); !errors.Is(err, ErrStaleApproval) || !strings.Contains(err.Error(), "deleted and created again") {
			t.Fatalf("another flat: %v", err)
		}
		if v, _ := s.GetSignIn(ctx, "app"); v.Provider != "" {
			t.Fatalf("the stale approval applied: %+v", v)
		}
	})
}

// The refusals of rule 8 with 409 and their messages, for a Private and a Public flat and a flat of public instance
// mode (rule 73). A refusal changes nothing and opens no route.
func TestSignInRefusesOtherPublicProviders(t *testing.T) {
	t.Parallel()
	s, dir := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	ctx := t.Context()
	refused := func(name string, err error, says []string, hides ...string) {
		t.Helper()
		if !errors.Is(err, ErrConflict) || ErrorCategory(err) != "conflict" {
			t.Errorf("%s: %v, want 409", name, err)
			return
		}
		for _, part := range says {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("%s: the message lacks %q: %v", name, part, err)
			}
		}
		for _, part := range hides {
			if strings.Contains(err.Error(), part) {
				t.Errorf("%s: the message has %q: %v", name, part, err)
			}
		}
	}
	if _, err := s.CreateFlat(ctx, "priv", "", ViaCLI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProviderPermission(ctx, "priv", store.ProviderFunnel, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	_, err := s.RequestSignIn(ctx, "priv", guestRequest, ViaCLI)
	refused("a Private flat", err, []string{"flat priv is granted tailscale-funnel, whose visitors do not sign in", "Revoke tailscale-funnel under Networks on the Settings tab of the flat"}, "Private first")
	publicLifecycleFlat(t, s, "pub", store.ProviderFunnel, store.ProviderZrok)
	_, err = s.RequestSignIn(ctx, "pub", guestRequest, ViaCLI)
	refused("a Public flat", err, []string{"tailscale-funnel, zrok", "Make the flat Private first", "the host refuses a revocation while its route is registered"})
	if _, err := rawDB(t, dir).Exec(`UPDATE flats SET created_public_instance=1 WHERE slug='pub'`); err != nil {
		t.Fatal(err)
	}
	_, err = s.RequestSignIn(ctx, "pub", guestRequest, ViaCLI)
	refused("public instance mode", err, []string{"public instance mode made flat pub Public on tailscale-funnel, zrok", "turn sign-in on before the flat is Public again"})
	if all, _ := s.ListApprovals(ctx, "pending"); len(all) != 0 {
		t.Fatalf("refused requests made approvals: %+v", all)
	}
	// Off stays possible, and it is the stored setting here.
	if r, err := s.RequestSignIn(ctx, "pub", SignInRequest{}, ViaCLI); err != nil || r.Status != "done" {
		t.Fatalf("off: %+v, %v", r, err)
	}

	lifecycleSave(t, s, "signed", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "signed"))
	approveSignIn(t, s, "signed", guestRequest)
	for _, p := range []string{store.ProviderFunnel, store.ProviderZrok, store.ProviderCFQuick, store.ProviderNetbirdExpose, store.ProviderProxyPublic} {
		err := s.SetProviderPermission(ctx, "signed", p, true, ViaConsole)
		refused("a grant of "+p, err, []string{"visitors on " + p + " do not sign in", "`flats signin off signed` and an approval in the console"})
		if ok, _ := s.st.ProviderPermitted(ctx, "signed", p); ok {
			t.Errorf("a refused grant of %s was stored", p)
		}
	}
	if err := s.SetProviderPermission(ctx, "signed", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatalf("a grant of portal: %v", err)
	}
	// A flats.db that a hand wrote with sign-in on and a Funnel grant: the visibility request and the application of a
	// visibility approval that froze no sign-in sequence are refused, before any route opens.
	if _, err := rawDB(t, dir).Exec(`INSERT INTO flat_providers(flat,provider,permitted) VALUES('signed','tailscale-funnel',1)`); err != nil {
		t.Fatal(err)
	}
	_, err = s.SetVisibility(ctx, "signed", store.Public, ViaCLI, "")
	refused("a visibility request", err, []string{"is granted tailscale-funnel, whose visitors do not sign in, so it cannot be made Public", "which works while the flat is Private"})
	f, _ := s.st.GetFlat(ctx, "signed")
	key, err := s.providerKey(ctx, "signed")
	if err != nil {
		t.Fatal(err)
	}
	live, seq := f.LiveVersion, f.AccessSeq
	params, _ := json.Marshal(visibilityParams{Visibility: "public", From: "private", ExpectedLive: &live, Providers: key, Access: store.AccessView, AccessSeq: &seq})
	if err := s.st.InsertApproval(ctx, store.Approval{ID: "apr-vis", Flat: "signed", Action: "set_visibility", Params: params, Status: "pending", Via: "cli", RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Decide(ctx, "apr-vis", true)
	refused("a visibility approval", err, []string{"so it cannot be made Public"})
	if network.serves("signed", ProviderFunnel) || network.serves("signed", ProviderPortal) {
		t.Error("the refused approval opened a public route")
	}
	if f, _ := s.st.GetFlat(ctx, "signed"); f.Visibility != store.Private {
		t.Errorf("the flat is %s", f.Visibility)
	}
}

// Each approval that freezes the visibility of a flat also freezes its sign-in sequence, and a sign-in change makes it
// stale (rule 23).
func TestSignInMakesVisibilityApprovalsStale(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)
	ctx := t.Context()
	lifecycleSave(t, s, "app", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
	lifecycleSave(t, s, "app", "two")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
	_, err := s.Rollback(ctx, "app", 1, ViaAPI)
	rollback := pendingID(t, err)
	_, err = s.Deploy(ctx, "app", 1, ViaAPI)
	activate := pendingID(t, err)
	lifecycleSave(t, s, "app", "three")
	version := resultID(t)(s.CreateVersion(ctx, "app", 0, "", ViaAPI))
	visibility := resultID(t)(s.SetVisibility(ctx, "app", store.Public, ViaAPI, ""))
	for name, id := range map[string]string{"rollback": rollback, "activate": activate, "create_version": version, "set_visibility": visibility} {
		a, _ := s.GetApproval(ctx, id)
		if fp := frozenParams(t, &a); string(fp["signin_seq"]) != "0" {
			t.Errorf("%s froze no sign-in sequence: %s", name, a.Params)
		}
	}
	approveSignIn(t, s, "app", guestRequest)
	for name, id := range map[string]string{"rollback": rollback, "activate": activate, "create_version": version, "set_visibility": visibility} {
		a, err := s.Decide(ctx, id, true)
		if !errors.Is(err, ErrStaleApproval) || a.Status != "failed" || !strings.Contains(err.Error(), "the sign-in of app changed after the request (sequence 0, now 1)") {
			t.Errorf("%s: %+v, %v", name, a, err)
		}
	}
	// A request made after the change applies.
	if _, err := s.Deploy(ctx, "app", 1, ViaAPI); err != nil {
		if _, err := s.Decide(ctx, pendingID(t, err), true); err != nil {
			t.Fatalf("a deploy after the change: %v", err)
		}
	}
}

// AutopilotNeeds refuses set_signin as unknown, so an agent can neither approve nor reject it (rule 17).
func TestAutopilotCannotDecideSignIn(t *testing.T) {
	t.Parallel()
	s, _ := newAutopilotService(t, time.Time{})
	ctx := t.Context()
	if _, err := s.CreateFlat(ctx, "app", "", ViaCLI); err != nil {
		t.Fatal(err)
	}
	a := requestSignIn(t, s, "app", guestRequest)
	need, err := AutopilotNeeds(a.Action, a.Params)
	var u *UnclassifiedError
	if !errors.As(err, &u) || u.Reason != "unknown action" || need != 0 {
		t.Fatalf("AutopilotNeeds = %v, %v", need, err)
	}
	allowAutopilot(t, s, AutopilotOps...)
	before, _ := s.st.GetApproval(ctx, a.ID)
	for _, approve := range []bool{true, false} {
		if _, err := s.DecideAsAgent(ctx, a.ID, approve, bot); !errors.Is(err, ErrAutopilotNotAllowed) {
			t.Errorf("approve=%t: %v", approve, err)
		}
		if after, _ := s.st.GetApproval(ctx, a.ID); !reflect.DeepEqual(before, after) {
			t.Errorf("approve=%t changed the approval: %+v", approve, after)
		}
	}
	if v, _ := s.GetSignIn(ctx, "app"); v.Provider != "" {
		t.Fatalf("an agent decision applied: %+v", v)
	}
}

// stoppedService is a service on dir whose sign-in approval stopped after step 2 of rule 20: the step after the commit
// panics, as a host that stops there leaves the approval applying with its receipt.
func stoppedService(t *testing.T, dir string, st *store.Store) (*Service, store.Approval) {
	t.Helper()
	s, err := New(t.Context(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFlat(t.Context(), "app", "", ViaCLI); err != nil {
		t.Fatal(err)
	}
	a := requestSignIn(t, s, "app", guestRequest)
	s.signInHooks.applied = func(context.Context, string, store.SignIn, int64) error { panic("the host stops") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the step after the commit did not run")
			}
		}()
		_, _ = s.Decide(t.Context(), a.ID, true)
	}()
	a, _ = s.GetApproval(t.Context(), a.ID)
	if a.Status != "applying" || execution(t, a).Status != "applied" {
		t.Fatalf("after the stop: %+v", a)
	}
	return s, a
}

// A stop after step 2 ends, through resumeApplying, as approved with one stored change, one audit row and no webhook
// event; a stop before step 2 ends applies again from step 1 (rule 21).
func TestSignInStopAfterTheCommit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	t.Run("a restart", func(t *testing.T) {
		dir := t.TempDir()
		st, err := store.Open(filepath.Join(dir, "flats.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		first, a := stoppedService(t, dir, st)
		first.Close()
		s, err := New(ctx, Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		rec := &recorder{}
		s.SetNotifier(rec)
		got, _ := s.GetApproval(ctx, a.ID)
		if got.Status != "approved" || got.Result != "sign-in set to credential with 2 entries" || execution(t, got).Status != "approved" {
			t.Fatalf("after the restart: %+v", got)
		}
		if v, _ := s.GetSignIn(ctx, "app"); v.Provider != "credential" || v.Seq != 1 {
			t.Fatalf("the stored change: %+v", v)
		}
		if n := len(signInRows(t, s, dir, store.AuditSignInSet)); n != 1 {
			t.Errorf("%d signin.set rows, want 1", n)
		}
		evs, _ := s.st.ListEvents(ctx, "app", "signin", 0, 10)
		if len(evs) != 1 || evs[0].Message != got.Result {
			t.Errorf("signin events = %+v", evs)
		}
		if ps := rec.take(); len(ps) != 0 {
			t.Errorf("the restart sent %q", shorts(ps))
		}
	})
	t.Run("resumeApplying", func(t *testing.T) {
		dir := t.TempDir()
		st, err := store.Open(filepath.Join(dir, "flats.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		s, a := stoppedService(t, dir, st)
		defer s.Close()
		var calls []string
		s.signInHooks.applied = func(_ context.Context, slug string, in store.SignIn, seq int64) error {
			calls = append(calls, fmt.Sprintf("%s %s %d", slug, in.Provider, seq))
			return nil
		}
		rec := &recorder{}
		s.SetNotifier(rec)
		s.resumeApplying(ctx)
		if got, _ := s.GetApproval(ctx, a.ID); got.Status != "approved" {
			t.Fatalf("after resumeApplying: %+v", got)
		}
		if !slices.Equal(calls, []string{"app credential 1"}) {
			t.Errorf("steps 3 and 4 from the stored setting ran %q", calls)
		}
		for _, p := range rec.take() {
			if p.Type == eventSignInChange {
				t.Errorf("resumeApplying sent %s", short(p))
			}
		}
		if n := len(signInRows(t, s, dir, store.AuditSignInSet)); n != 1 {
			t.Errorf("%d signin.set rows, want 1", n)
		}
	})
	t.Run("a stop before the commit", func(t *testing.T) {
		s, dir := newTestService(t)
		if _, err := s.CreateFlat(ctx, "app", "", ViaCLI); err != nil {
			t.Fatal(err)
		}
		a := requestSignIn(t, s, "app", guestRequest)
		if err := s.st.ClaimApprovalAuthorized(ctx, a.ID, "console", time.Now()); err != nil {
			t.Fatal(err)
		}
		s.resumeApplying(context.Background())
		if got, _ := s.GetApproval(ctx, a.ID); got.Status != "approved" {
			t.Fatalf("after resumeApplying: %+v", got)
		}
		rows := signInRows(t, s, dir, store.AuditSignInSet)
		if len(rows) != 1 || rows[0].Via != "console" || rows[0].Actor != "" {
			t.Fatalf("signin.set rows = %+v", rows)
		}
	})
	t.Run("a failure of step 4", func(t *testing.T) {
		s := signInService(t)
		s.signInHooks.applied = func(context.Context, string, store.SignIn, int64) error {
			return errors.New("the gate cannot be built")
		}
		got := approveSignIn(t, s, "app", guestRequest)
		if got.Result != "sign-in set to credential with 2 entries; the setting is stored, and the Portal route of app is stopped: the gate cannot be built" {
			t.Fatalf("result %q", got.Result)
		}
	})
}

// The client secret: the operator sets and removes it with the CLI, each value that rule 26 refuses gives 400, a
// readable row with its name refuses the set, the commands of ordinary secrets refuse it (rule 31), the lists and the
// capture do not hold it (rule 30), and no text of the host holds the value.
func TestSignInClientSecret(t *testing.T) {
	t.Parallel()
	s, dir, rec := webhookService(t)
	ctx := t.Context()
	if _, err := s.CreateFlat(ctx, "app", "", ViaCLI); err != nil {
		t.Fatal(err)
	}
	var rebuilt int
	s.signInHooks.secretChanged = func(context.Context, string) error { rebuilt++; return nil }
	for _, via := range []Via{ViaConsole, ViaAPI, ViaMCP} {
		if _, err := s.SetSignInSecret(ctx, "app", signInValue, "https://idp.example.com", "flats-app", via); !errors.Is(err, ErrForbidden) {
			t.Errorf("set via %s: %v", via, err)
		}
		if _, err := s.DeleteSignInSecret(ctx, "app", via); !errors.Is(err, ErrForbidden) {
			t.Errorf("rm via %s: %v", via, err)
		}
	}
	for name, tc := range map[string]struct{ value, issuer, id, says string }{
		"empty":              {"", "https://idp.example.com", "flats-app", "is empty"},
		"too long":           {strings.Repeat("v", 4097), "https://idp.example.com", "flats-app", "more than 4096 bytes"},
		"a space":            {signInValue + " x", "https://idp.example.com", "flats-app", "a space"},
		"a trailing newline": {signInValue + "\n", "https://idp.example.com", "flats-app", "control character"},
		"not ASCII":          {signInValue + "é", "https://idp.example.com", "flats-app", "not ASCII"},
		"an http issuer":     {signInValue, "http://idp.example.com", "flats-app", "https"},
		"no client ID":       {signInValue, "https://idp.example.com", "", "client_id is missing"},
	} {
		_, err := s.SetSignInSecret(ctx, "app", tc.value, tc.issuer, tc.id, ViaCLI)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: %v, want %q", name, err, tc.says)
		}
		noSignInText(t, name, fmt.Sprint(err))
	}
	if v, _ := s.GetSignIn(ctx, "app"); v.ClientSecret != nil {
		t.Fatalf("a refused set stored a value: %+v", v.ClientSecret)
	}
	// A readable row with the name, from before the name check.
	g, _ := s.aead()
	nonce := make([]byte, g.NonceSize())
	sealed := g.Seal(nil, nonce, []byte("readable"), []byte(store.SignInSecretName))
	if _, err := rawDB(t, dir).Exec(`INSERT INTO secrets(flat,name,nonce,ciphertext,updated_at) VALUES('app',?,?,?,1)`, store.SignInSecretName, nonce, sealed); err != nil {
		t.Fatal(err)
	}
	_, err := s.SetSignInSecret(ctx, "app", signInValue, "https://idp.example.com", "flats-app", ViaCLI)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "`flats secret rm app oidc-client-secret`") {
		t.Fatalf("with a readable row: %v", err)
	}
	if err := s.DeleteSecret(ctx, "app", store.SignInSecretName, ViaCLI); err != nil {
		t.Fatalf("the readable row: %v", err)
	}
	r, err := s.SetSignInSecret(ctx, "app", signInValue, "https://idp.example.com/", "flats-app", ViaCLI)
	if err != nil || r.Message != "sign-in client secret set via cli for issuer https://idp.example.com and client ID flats-app" || rebuilt != 1 {
		t.Fatalf("set: %+v, %v (rebuilt %d)", r, err, rebuilt)
	}
	v, _ := s.GetSignIn(ctx, "app")
	if v.ClientSecret == nil || v.ClientSecret.Name != store.SignInSecretName || v.ClientSecret.Issuer != "https://idp.example.com" || v.ClientSecret.ClientID != "flats-app" || v.ClientSecret.UpdatedAt.IsZero() {
		t.Fatalf("the view of the secret: %+v", v.ClientSecret)
	}
	err = s.DeleteSecret(ctx, "app", store.SignInSecretName, ViaConsole)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "`flats signin secret rm app`") {
		t.Fatalf("flats secret rm of the client secret: %v", err)
	}
	if err := s.SetSecret(ctx, "app", "PLAIN", "plain-value", ViaCLI); err != nil {
		t.Fatal(err)
	}
	secs, _ := s.SecretNames(ctx, "app")
	if len(secs) != 1 || secs[0].Name != "PLAIN" {
		t.Fatalf("SecretNames = %+v", secs)
	}
	// The capture reads only the rows with the empty use.
	var spec RuntimeSpec
	s.cfg.Runtime = lifecycleRuntime(func(got RuntimeSpec) (Instance, error) {
		spec = got
		return lifecycleInstance{http.NotFoundHandler()}, nil
	})
	d, err := s.build(ctx, "app", store.Version{Kind: "server", Manifest: []byte(`{"entry":"server.js"}`)}, "unused", nil)
	if err != nil {
		t.Fatal(err)
	}
	d.inst.Stop()
	if len(spec.Env) != 1 || spec.Env["PLAIN"] != "plain-value" || len(spec.FetchHeaders) != 0 {
		t.Fatalf("the capture: %v %v", spec.Env, spec.FetchHeaders)
	}
	// Rule 27: the value opens only for the setting of its binding.
	if got, ok, err := s.SignInClientSecret(ctx, "app", oidcStored); err != nil || !ok || got != signInValue {
		t.Fatalf("SignInClientSecret = %q, %v, %v", got, ok, err)
	}
	moved := oidcStored
	moved.Issuer = "https://new.example.com"
	_, ok, err := s.SignInClientSecret(ctx, "app", moved)
	if ok || !errors.Is(err, ErrSignInSecretBinding) || !strings.Contains(err.Error(), "`flats signin secret set app --issuer https://new.example.com --client-id flats-app`") {
		t.Fatalf("another binding: %v, %v", ok, err)
	}
	if _, err := s.SignInGate(ctx, "app", moved); !errors.Is(err, ErrSignInSecretBinding) {
		t.Errorf("SignInGate with another binding: %v", err)
	}
	if _, ok, err := s.SignInClientSecret(ctx, "app", store.SignIn{Provider: "credential", Allow: []string{"x"}}); ok || err != nil {
		t.Errorf("credential reads the secret: %v, %v", ok, err)
	}
	gate, err := s.SignInGate(ctx, "app", oidcStored)
	if err != nil || gate.ClientSecret != signInValue || !slices.Equal(gate.Allow, oidcStored.Allow) {
		t.Fatalf("SignInGate = %v, %v", gate, err)
	}
	noSignInText(t, "a formatted gate", strings.ReplaceAll(fmt.Sprintf("%v %+v %#v %s", gate, gate, gate, gate), "entries", ""))
	// An approved change of the issuer applies; the value stays bound to the old one.
	approveSignIn(t, s, "app", oidcRequest)
	approveSignIn(t, s, "app", SignInRequest{Provider: "oidc", Issuer: "https://new.example.com", ClientID: "flats-app", Allow: []string{"ann@example.com"}})
	if _, ok, err := s.SignInClientSecret(ctx, "app", moved); ok || !errors.Is(err, ErrSignInSecretBinding) {
		t.Errorf("after the change of the issuer: %v, %v", ok, err)
	}
	if _, err := s.DeleteSignInSecret(ctx, "app", ViaCLI); err != nil || rebuilt != 2 {
		t.Fatalf("rm: %v (rebuilt %d)", err, rebuilt)
	}
	if _, err := s.DeleteSignInSecret(ctx, "app", ViaCLI); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a second rm: %v", err)
	}
	// No text of the host holds the value, and a set or a removal sends no event.
	for _, p := range rec.take() {
		b, _ := json.Marshal(p)
		noSignInText(t, "a webhook body", strings.NewReplacer("ann@example.com", "", "@example.org", "").Replace(string(b)))
		if p.Type != eventApprovalCreated && p.Type != eventApprovalDecided && p.Type != eventSignInChange {
			t.Errorf("event %s", short(p))
		}
	}
	evs, _ := s.st.ListEvents(ctx, "app", "", 0, 1000)
	for _, e := range evs {
		if strings.Contains(e.Message+string(e.Data), signInValue) {
			t.Errorf("an event holds the value: %+v", e)
		}
	}
	rd, err := store.OpenAuditReader(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	rows, _ := rd.List(ctx, store.AuditQuery{Flat: "app"}, 0, 1000)
	var secretRows []string
	for _, e := range rows {
		b, _ := json.Marshal(e)
		if strings.Contains(string(b), signInValue) {
			t.Errorf("an audit row holds the value: %s", b)
		}
		if e.Name == store.SignInSecretName {
			secretRows = append(secretRows, e.Kind+" "+e.Via+" "+string(e.Data))
		}
	}
	want := []string{
		`secret.delete cli `,
		`secret.set cli {"client_id":"flats-app","created":true,"issuer":"https://idp.example.com","use":"signin"}`,
		`secret.delete cli {"use":"signin"}`,
	}
	if !slices.Equal(secretRows, want) {
		t.Errorf("the rows of the secret:\n%s\nwant\n%s", strings.Join(secretRows, "\n"), strings.Join(want, "\n"))
	}
	approvals, _ := s.ListApprovals(ctx, "")
	for _, a := range approvals {
		if strings.Contains(string(a.Params)+a.Result+string(a.ResultData), signInValue) {
			t.Errorf("an approval holds the value: %+v", a)
		}
	}
}

// A failure of the rebuild of rule 28 leaves the value stored and says that the Portal route stopped.
func TestSignInSecretRebuildFailure(t *testing.T) {
	t.Parallel()
	s := signInService(t)
	s.signInHooks.secretChanged = func(context.Context, string) error { return errors.New("the gate cannot be built") }
	r, err := s.SetSignInSecret(t.Context(), "app", signInValue, "https://idp.example.com", "flats-app", ViaCLI)
	if err != nil || !strings.HasSuffix(r.Message, "; the Portal route of app is stopped: the gate cannot be built") {
		t.Fatalf("set: %+v, %v", r, err)
	}
	if v, _ := s.GetSignIn(t.Context(), "app"); v.ClientSecret == nil {
		t.Fatal("the value was not stored")
	}
}

// rotate-key seals the client secret again and keeps its use and its binding, and restore-key opens it and counts it
// (rule 32).
func TestSignInSecretAndTheHostKey(t *testing.T) {
	t.Parallel()
	s := signInService(t)
	ctx := t.Context()
	if _, err := s.SetSignInSecret(ctx, "app", signInValue, "https://idp.example.com", "flats-app", ViaCLI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSecret(ctx, "app", "PLAIN", "plain-value", ViaCLI); err != nil {
		t.Fatal(err)
	}
	if c, err := CheckKey(ctx, s.st, s.secretKey); err != nil || c.Total != 2 || !c.All() {
		t.Fatalf("CheckKey with the key = %+v, %v", c, err)
	}
	newKey, err := NewSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	if n, _, err := Reseal(ctx, s.st, s.secretKey, newKey, false, store.Audit{Via: store.AuditViaCommand}); err != nil || n != 2 {
		t.Fatalf("Reseal = %d, %v", n, err)
	}
	if c, err := CheckKey(ctx, s.st, newKey); err != nil || c.Total != 2 || !c.All() {
		t.Fatalf("CheckKey with the new key = %+v, %v", c, err)
	}
	sec, ok, err := s.st.SignInSecret(ctx, "app")
	if err != nil || !ok || sec.Use != store.SecretUseSignIn || sec.SignIn != (store.SignInBinding{Issuer: "https://idp.example.com", ClientID: "flats-app"}) || sec.KeyID != KeyID(newKey) {
		t.Fatalf("after the re-seal: %+v, %v, %v", sec, ok, err)
	}
	s.secretKey, s.secretKeyID = newKey, KeyID(newKey)
	if got, ok, err := s.SignInClientSecret(ctx, "app", oidcStored); err != nil || !ok || got != signInValue {
		t.Fatalf("the value after the re-seal: %q, %v, %v", got, ok, err)
	}
}

// The results of appauth.Config.Normalize and appauth.Validate of portal-tunnel v2.7.4 that signin_rules.go mirrors.
func TestSDKRulesOfV274(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		provider  string
		in, want  []string
		wantError string
	}{
		"oidc":                {"oidc", []string{" Ann@Example.COM ", "SUB:AbC", "sub: x ", "@Example.org", "ann@example.com"}, []string{"ann@example.com", "sub:AbC", "sub:x", "@example.org"}, ""},
		"credential":          {"credential", []string{" b ", "a", "b", "Guest One"}, []string{"b", "a", "Guest One"}, ""},
		"an empty domain":     {"oidc", []string{"a@"}, nil, "must be an email"},
		"a space in a domain": {"oidc", []string{"a@b c"}, nil, "must be an email"},
		"two @":               {"oidc", []string{"a@b@c"}, nil, "must be an email"},
		"an empty subject":    {"oidc", []string{"sub:"}, nil, "1 to 256 printable"},
		"an empty credential": {"credential", []string{" "}, nil, "1 to 256 printable"},
	} {
		got, err := sdkNormalizeAllow(tc.provider, tc.in)
		if tc.wantError != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("%s: %v, want %q", name, err, tc.wantError)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	for name, tc := range map[string]struct {
		in   store.SignIn
		want string
	}{
		"oidc":                       {oidcStored, ""},
		"credential":                 {store.SignIn{Provider: "credential", Allow: []string{"x"}}, ""},
		"an empty credential":        {store.SignIn{Provider: "credential"}, ""},
		"an empty oidc list":         {store.SignIn{Provider: "oidc", Issuer: "https://i.example", ClientID: "c"}, "at least one"},
		"no client ID":               {store.SignIn{Provider: "oidc", Issuer: "https://i.example", Allow: []string{"a@b.c"}}, "client id is required"},
		"an issuer of credential":    {store.SignIn{Provider: "credential", Issuer: "https://i.example", Allow: []string{"x"}}, "require the oidc"},
		"another provider":           {store.SignIn{Provider: "wallet"}, "unsupported"},
		"a list of another provider": {store.SignIn{Provider: "wallet", Allow: []string{"x"}}, "unsupported"},
	} {
		err := sdkValidate(tc.in)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}
