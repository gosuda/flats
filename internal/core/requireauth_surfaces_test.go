package core

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

// These tests cover the completion criteria of docs/spec/portal-require-auth.md that the other tests of the setting do
// not reach: the flat view of a refused flat (rule 12), public instance mode (rule 22), and the routes that the setting
// does not touch (scope).

// Rule 12: a flat whose only public route is the refused Portal route has the connection state unavailable and no address
// in its view, and the view holds the endpoint with the detail of rule 10. The list of flats shows the same.
func TestRequireAuthFlatView(t *testing.T) {
	ctx := t.Context()
	s, network, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "solo", store.ProviderPortal)
	if fv, err := s.GetFlat(ctx, "solo"); err != nil || fv.ConnectionState != "ready" || fv.PublicURL == "" {
		t.Fatalf("the view before the setting: connection %q, public URL %q, %v", fv.ConnectionState, fv.PublicURL, err)
	}
	setRequireAuth(t, s, true)
	if network.serves("solo", ProviderPortal) {
		t.Fatal("the Portal route of solo still serves")
	}
	check := func(where string, fv FlatView) {
		t.Helper()
		if fv.ConnectionState != "unavailable" || fv.PublicURL != "" {
			t.Fatalf("%s: connection %q, public URL %q, want unavailable and no address", where, fv.ConnectionState, fv.PublicURL)
		}
		for _, ep := range fv.Endpoints {
			if ep.Provider == ProviderPortal && ep.Host == "solo" {
				if ep.State != "unavailable" || ep.Ready || ep.Detail != requireAuthDetailOf("solo") {
					t.Fatalf("%s: the Portal endpoint = %+v", where, ep)
				}
				return
			}
		}
		t.Fatalf("%s: no Portal endpoint in %+v", where, fv.Endpoints)
	}
	fv, err := s.GetFlat(ctx, "solo")
	if err != nil {
		t.Fatal(err)
	}
	check("GetFlat", fv)
	list, err := s.ListFlats(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListFlats = %+v, %v", list, err)
	}
	check("ListFlats", list[0])
}

// Rule 22: a flat that public instance mode creates Public on Portal has no Portal route while the setting is on, its view
// promises no address, and its endpoint says what to do. The mode and the setting stay independent: turning the setting
// off opens the route, and neither one changes the other.
func TestRequireAuthWithPublicInstanceMode(t *testing.T) {
	ctx := t.Context()
	s, network := piCreationService(t, true)
	setRequireAuth(t, s, true)
	if _, err := s.CreateDocument(ctx, "created", "Document", ViaMCP); err != nil {
		t.Fatal(err)
	}
	row, err := s.st.GetFlat(ctx, "created")
	if err != nil {
		t.Fatal(err)
	}
	if !row.CreatedPublicInstance || !row.Visibility.Public() || row.SignInProvider != "" {
		t.Fatalf("the flat is not Public without sign-in through the mode: %+v", row)
	}
	if granted, err := s.st.ProviderPermitted(ctx, "created", store.ProviderPortal); err != nil || !granted {
		t.Fatalf("Portal granted = %t, %v", granted, err)
	}
	if network.serves("created", ProviderPortal) {
		t.Fatal("the Portal route of a flat without sign-in opened while the setting is on")
	}
	fv, err := s.GetFlat(ctx, "created")
	if err != nil {
		t.Fatal(err)
	}
	if !fv.Running || fv.PublicURL != "" || fv.ConnectionState != "unavailable" {
		t.Fatalf("the view of the refused flat: running %t, public URL %q, connection %q", fv.Running, fv.PublicURL, fv.ConnectionState)
	}
	if ep := portalEndpoint(t, s, "created"); ep.State != "unavailable" || ep.Ready || ep.Detail != requireAuthDetailOf("created") {
		t.Fatalf("the Portal endpoint = %+v", ep)
	}
	if !s.PublicInstance().Enabled || !s.requireAuthOn() {
		t.Fatalf("the mode is %t and the setting is %t, both stay on", s.PublicInstance().Enabled, s.requireAuthOn())
	}
	setRequireAuth(t, s, false)
	if !s.PublicInstance().Enabled || !network.serves("created", ProviderPortal) {
		t.Fatalf("with the setting off: mode %t, Portal serves %t", s.PublicInstance().Enabled, network.serves("created", ProviderPortal))
	}
	if fv, err := s.GetFlat(ctx, "created"); err != nil || fv.ConnectionState != "ready" || fv.PublicURL == "" {
		t.Fatalf("the view after the setting is off: connection %q, public URL %q, %v", fv.ConnectionState, fv.PublicURL, err)
	}
}

// Scope: the setting covers the Portal route of a Public flat. For a flat that the visit refuses, the private route and the
// preview stay: the visit ends the open connections of the public routes, and a private handler that was taken before it still
// answers. The preview serves before and after, with the setting on and off.
func TestRequireAuthLeavesPrivateRoutesAndPreviewsAlone(t *testing.T) {
	ctx := t.Context()
	s, network, _, _ := newRequireAuthService(t)
	publicLifecycleFlat(t, s, "app", store.ProviderPortal, store.ProviderFunnel)
	preview, err := s.OpenPreview(ctx, "app", 1)
	if err != nil {
		t.Fatal(err)
	}
	privateBefore := network.handler("app", ProviderLocal)
	publicEpoch := epochOf(s, "app")
	if privateBefore == nil || publicEpoch == nil || !network.serves(preview.Host, ProviderLocal) {
		t.Fatalf("the test did not set up the routes: private %t, epoch %t, preview %t", privateBefore != nil, publicEpoch != nil, network.serves(preview.Host, ProviderLocal))
	}
	check := func(when string) {
		t.Helper()
		if code := providerRequest(privateBefore, ProviderLocal).Code; code != http.StatusOK {
			t.Fatalf("%s: a private connection that was open before answers %d", when, code)
		}
		if h := network.handler("app", ProviderLocal); h == nil || providerRequest(h, ProviderLocal).Code != http.StatusOK {
			t.Fatalf("%s: the private route of app does not serve", when)
		}
		if !network.serves(preview.Host, ProviderLocal) {
			t.Fatalf("%s: the preview does not serve", when)
		}
	}
	setRequireAuth(t, s, true)
	if network.serves("app", ProviderPortal) || publicEpoch.ctx.Err() == nil {
		t.Fatalf("the visit did not refuse the flat: Portal serves %t, the public epoch is cancelled %t", network.serves("app", ProviderPortal), publicEpoch.ctx.Err() != nil)
	}
	check("with the setting on")
	setRequireAuth(t, s, false)
	check("after the setting turned off")
}

// Rule 3 and decision 2: no approval action changes the setting, so Autopilot has no operation for one. AutopilotNeeds
// refuses an action that it does not know.
func TestRequireAuthHasNoApprovalAction(t *testing.T) {
	for _, action := range []string{"set_setting", "update_settings", "set_portal_require_auth", "portal_require_auth", "set_require_auth"} {
		if need, err := AutopilotNeeds(action, json.RawMessage(`{"portal_require_auth":"true"}`)); err == nil || need != 0 {
			t.Errorf("AutopilotNeeds(%q) = %v, %v, want a refusal", action, need, err)
		}
	}
}

// Rule 21: the result of a sign-in off approval reads the setting when it applies, so a request that was made before the setting
// changed is not stale and its result tells the truth. A flat that is not Public has no Portal route to stop, so its result is
// the one without the setting.
func TestRequireAuthSignInOffDecidedAfterTheSettingChanged(t *testing.T) {
	ctx := t.Context()
	s, network, _, _ := newRequireAuthService(t)
	network.file = true
	publicLifecycleFlat(t, s, "app", store.ProviderPortal)
	approveSignIn(t, s, "app", guestRequest)
	lifecycleSave(t, s, "closed", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "closed"))
	if err := s.SetProviderPermission(ctx, "closed", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	approveSignIn(t, s, "closed", guestRequest)

	// Requested with the setting off, decided with it on: the Portal route stops, and the result says so.
	pending := requestSignIn(t, s, "app", SignInRequest{})
	pendingClosed := requestSignIn(t, s, "closed", SignInRequest{})
	setRequireAuth(t, s, true)
	a, err := s.Decide(ctx, pending.ID, true)
	if err != nil || a.Status != "approved" {
		t.Fatalf("Decide = %+v, %v", a, err)
	}
	if !strings.Contains(a.Result, "portal.require_auth is on, so the Portal route of app is stopped") || network.serves("app", ProviderPortal) {
		t.Fatalf("the result = %q, Portal serves %t", a.Result, network.serves("app", ProviderPortal))
	}
	a, err = s.Decide(ctx, pendingClosed.ID, true)
	if err != nil || a.Status != "approved" || strings.Contains(a.Result, "portal.require_auth") {
		t.Fatalf("a flat that is not Public: %+v, %v", a, err)
	}

	// Requested with the setting on, decided with it off: the route is not stopped, and the result does not say so.
	setRequireAuth(t, s, false)
	approveSignIn(t, s, "app", guestRequest)
	setRequireAuth(t, s, true)
	pending = requestSignIn(t, s, "app", SignInRequest{})
	setRequireAuth(t, s, false)
	a, err = s.Decide(ctx, pending.ID, true)
	if err != nil || a.Status != "approved" || strings.Contains(a.Result, "portal.require_auth") || !network.serves("app", ProviderPortal) {
		t.Fatalf("the result = %+v, %v, Portal serves %t", a, err, network.serves("app", ProviderPortal))
	}
}

// The issue, a restart: with the setting off in config.json, a new service on the same files opens the Portal route of a flat
// without sign-in, so a host that never turned the setting on, or turned it off again, serves as before.
func TestRequireAuthRestartWithTheSettingOff(t *testing.T) {
	s, path := newReloadService(t)
	s.cfg.Lifecycle = newRequireAuthNet()
	publicLifecycleFlat(t, s, "app", store.ProviderPortal, store.ProviderFunnel)
	setRequireAuth(t, s, true)
	setRequireAuth(t, s, false)
	s2, restarted := restartOnFiles(t, path)
	if s2.PortalRequireAuth() {
		t.Fatal("the setting is on after the restart")
	}
	if !restarted.serves("app", ProviderPortal) || !restarted.serves("app", ProviderFunnel) {
		t.Fatalf("after the restart Portal serves %t and Funnel serves %t", restarted.serves("app", ProviderPortal), restarted.serves("app", ProviderFunnel))
	}
}
