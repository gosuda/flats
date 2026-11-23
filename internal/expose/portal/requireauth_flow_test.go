package portal_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/portal"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/portal-require-auth.md through the host: core, the provider manager and a Portal Net whose exposures are
// loopback listeners (the harness of the sign-in tests).

const gatedHost = "gated-0xfed.relay.test"

func setRequireAuth(t *testing.T, h *host, on bool) {
	t.Helper()
	v := "false"
	if on {
		v = "true"
	}
	if _, err := h.svc.UpdateSettings(context.Background(), map[string]string{core.SetRequireAuth: v}); err != nil {
		t.Fatal(err)
	}
}

func portalEndpointOf(t *testing.T, h *host, slug string) core.ExposureEndpoint {
	t.Helper()
	fv, err := h.svc.GetFlat(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range fv.Endpoints {
		if ep.Provider == core.ProviderPortal && ep.Host == slug {
			return ep
		}
	}
	t.Fatalf("%s has no Portal endpoint: %+v", slug, fv.Endpoints)
	return core.ExposureEndpoint{}
}

func TestRequireAuthOnThePortalRoute(t *testing.T) {
	ctx := context.Background()
	h := startHost(t, t.TempDir())
	deployServer(t, h, "app")
	publicOnPortal(t, h, "app")
	exp := h.px.Of(t, "app")
	exp.SetRelays(portal.ReadyRelay("relay.test", appHost))
	anon := portal.NewTestVisitor(exp.Addr(), appHost)
	wantPage(t, anon, "/", "v1 /")
	open := openSocketOK(t, anon)

	deployServer(t, h, "gated")
	publicOnPortal(t, h, "gated")
	approveSignIn(t, h, "gated", credential("guest-7Qx"))
	gexp := h.px.Of(t, "gated")
	gexp.SetRelays(portal.ReadyRelay("relay.test", gatedHost))
	gated := portal.NewTestVisitor(gexp.Addr(), gatedHost)
	wantSignInPage(t, gated, "/")

	// A flat served on Portal without sign-in stops being served when the setting turns on, and the connection that
	// was open on it ends (rule 16).
	setRequireAuth(t, h, true)
	waitClosed(t, open)
	if exp.Closes() != 1 {
		t.Fatalf("the Portal route of app closed %d times, want once", exp.Closes())
	}
	want := "portal.require_auth is on and app has no sign-in. Run flats signin set app to turn sign-in on, or turn the setting off in the console."
	if ep := portalEndpointOf(t, h, "app"); ep.State != "unavailable" || ep.Ready || ep.Detail != want {
		t.Fatalf("the Portal endpoint of app = %+v", ep)
	}
	events, err := h.st.ListEvents(ctx, "app", "exposure", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[len(events)-1].Level != "warn" || events[len(events)-1].Message != want {
		t.Fatalf("events = %+v", events)
	}
	if !strings.Contains(h.logs.all(), "portal.require_auth: app: "+want) {
		t.Fatalf("the host log has no line for app: %s", h.logs.all())
	}
	// The private route stays, and a flat with sign-in keeps its Portal route and its gate (rules 6 and 13).
	wantPage(t, privateVisitor(t, h, "app"), "/", "v1 /")
	wantSignInPage(t, gated, "/")
	if refused, err := h.svc.PortalRefused(ctx, true); err != nil || len(refused) != 1 || refused[0] != "app" {
		t.Fatalf("the refused flats = %q, %v", refused, err)
	}

	// A new flat is not served on Portal either: the approval fails with the text, and no exposure opens (rule 19).
	deployServer(t, h, "fresh")
	if err := h.svc.SetProviderPermission(ctx, "fresh", store.ProviderPortal, true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	count := h.px.Count()
	r, err := h.svc.SetVisibility(ctx, "fresh", store.Public, core.ViaConsole, "")
	if err != nil || r.Approval == nil {
		t.Fatalf("request = %+v, %v", r, err)
	}
	if _, err := h.svc.Decide(ctx, r.Approval.ID, true); err == nil || !strings.Contains(err.Error(), "portal.require_auth is on and fresh has no sign-in") {
		t.Fatalf("the approval of fresh = %v, want the refusal", err)
	}
	if h.px.Count() != count {
		t.Fatal("a refused open made an exposure")
	}
	if f, _ := h.svc.GetFlat(ctx, "fresh"); f.Visibility != store.Private {
		t.Fatalf("fresh is %s, want Private", f.Visibility)
	}

	// Turning the setting off serves app on Portal again (rule 17).
	setRequireAuth(t, h, false)
	if h.px.Count() != count+1 {
		t.Fatalf("exposures = %d, want one more for app", h.px.Count())
	}
	exp2 := h.px.Of(t, "app")
	exp2.SetRelays(portal.ReadyRelay("relay.test", appHost))
	wantPage(t, portal.NewTestVisitor(exp2.Addr(), appHost), "/", "v1 /")
	if ep := portalEndpointOf(t, h, "app"); ep.State == "unavailable" {
		t.Fatalf("the Portal endpoint after the setting is off = %+v", ep)
	}
	if refused, err := h.svc.PortalRefused(ctx, true); err != nil || len(refused) != 0 {
		t.Fatalf("the refused flats = %q, %v", refused, err)
	}
}
