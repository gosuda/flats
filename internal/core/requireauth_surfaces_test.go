package core

import (
	"encoding/json"
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

// Scope: the setting covers the Portal route of a Public flat. A private route and a preview are not Portal, so the
// setting turning on and off does not stop them and does not end their connections.
func TestRequireAuthLeavesPrivateRoutesAndPreviewsAlone(t *testing.T) {
	ctx := t.Context()
	s, network, _, _ := newRequireAuthService(t)
	setRequireAuth(t, s, true)
	lifecycleSave(t, s, "priv", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "priv"))
	preview, err := s.OpenPreview(ctx, "priv", 1)
	if err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		if !network.serves("priv", ProviderLocal) || !network.serves(preview.Host, ProviderLocal) {
			t.Fatalf("%s: the private route serves %t and the preview serves %t", when, network.serves("priv", ProviderLocal), network.serves(preview.Host, ProviderLocal))
		}
		if epochOf(s, "priv") != nil {
			t.Fatalf("%s: the public routes of a Private flat got an epoch", when)
		}
	}
	check("with the setting on")
	setRequireAuth(t, s, false)
	check("after the setting turned off")
	setRequireAuth(t, s, true)
	check("after the setting turned on again")
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
