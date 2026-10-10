package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

// Regression: granting Tailscale to a live flat used to record the permission
// only, so the route (and the tailnet private_url) appeared after the next
// deploy, visibility change or restart.
func TestProviderGrantOpensRouteOnLiveFlat(t *testing.T) {
	for _, vis := range []store.Visibility{store.Private, store.Public} {
		t.Run(string(vis), func(t *testing.T) {
			s, _ := newTestService(t)
			network := newLifecycleRouteNet()
			s.cfg.Lifecycle = network
			lifecycleSave(t, s, "grant", "one")
			lifecycleApprove(t, s, lifecycleRequest(t, s, "grant"))
			if vis.Public() {
				if err := s.SetProviderPermission(t.Context(), "grant", store.ProviderPortal, true, ViaConsole); err != nil {
					t.Fatal(err)
				}
				req, err := s.SetVisibility(t.Context(), "grant", store.Public, ViaAPI, "")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.Decide(t.Context(), req.Approval.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			if network.serves("grant", ProviderTailscale) {
				t.Fatal("tailscale route served before it was permitted")
			}
			fv, err := s.GetFlat(t.Context(), "grant")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(fv.PrivateURL, "tailscale") {
				t.Fatalf("private_url before grant = %q", fv.PrivateURL)
			}

			if err := s.SetProviderPermission(t.Context(), "grant", store.ProviderTailscale, true, ViaConsole); err != nil {
				t.Fatal(err)
			}
			if !network.serves("grant", ProviderTailscale) {
				t.Fatal("grant did not open the tailscale route without a redeploy or restart")
			}
			fv, err = s.GetFlat(t.Context(), "grant")
			if err != nil {
				t.Fatal(err)
			}
			if want := "https://grant.tailscale.test"; fv.PrivateURL != want {
				t.Fatalf("private_url = %q, want %q", fv.PrivateURL, want)
			}
			found := false
			for _, ep := range fv.Endpoints {
				found = found || (ep.Provider == ProviderTailscale && ep.Audience == AudienceCurrent && ep.Permitted)
			}
			if !found {
				t.Fatalf("no permitted current tailscale endpoint: %+v", fv.Endpoints)
			}
			if vis.Public() && !network.serves("grant", ProviderPortal) {
				t.Fatal("re-exposure dropped the approved public route")
			}
			if fv.Visibility.Canonical() != vis {
				t.Fatalf("visibility changed to %s", fv.Visibility)
			}
		})
	}
}

// Granting a public provider to a private flat records the opt-in only: it
// must not open a public route or change visibility.
func TestProviderGrantToPrivateFlatOpensNoPublicRoute(t *testing.T) {
	s, _ := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	lifecycleSave(t, s, "quiet", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "quiet"))
	for _, p := range []string{store.ProviderPortal, store.ProviderFunnel} {
		if err := s.SetProviderPermission(t.Context(), "quiet", p, true, ViaConsole); err != nil {
			t.Fatal(err)
		}
	}
	if network.serves("quiet", ProviderPortal) || network.serves("quiet", ProviderFunnel) {
		t.Fatal("grant opened a public route on a private flat")
	}
	if !network.serves("quiet", ProviderLocal) {
		t.Fatal("re-exposure dropped the local route")
	}
	fv, err := s.GetFlat(t.Context(), "quiet")
	if err != nil {
		t.Fatal(err)
	}
	if fv.Visibility.Public() || fv.PublicURL != "" {
		t.Fatalf("private flat became public: visibility=%s public_url=%q", fv.Visibility, fv.PublicURL)
	}
}

// A grant on a flat without a live runtime only records the permission.
func TestProviderGrantWithoutRuntimeRecordsOnly(t *testing.T) {
	s, _ := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	lifecycleSave(t, s, "idle", "one")
	if err := s.SetProviderPermission(t.Context(), "idle", store.ProviderTailscale, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if network.serves("idle", ProviderTailscale) || network.serves("idle", ProviderLocal) {
		t.Fatal("grant served a flat with no live version")
	}
	if ok, err := s.st.ProviderPermitted(t.Context(), "idle", store.ProviderTailscale); err != nil || !ok {
		t.Fatalf("grant not recorded: %t %v", ok, err)
	}
}

// When the route fails to open, the grant stays recorded, an exposure error
// event is logged and the caller sees ErrProviderNotReady.
func TestProviderGrantExposureFailureKeepsPermission(t *testing.T) {
	s, _ := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	lifecycleSave(t, s, "flaky", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "flaky"))
	network.failPrivateAfterLocal = fmt.Errorf("tailnet login required: %w", ErrProviderUnavailable)
	err := s.SetProviderPermission(t.Context(), "flaky", store.ProviderTailscale, true, ViaConsole)
	if !errors.Is(err, ErrProviderNotReady) || !strings.Contains(err.Error(), "tailnet login required") {
		t.Fatalf("exposure failure not reported: %v", err)
	}
	if ok, err := s.st.ProviderPermitted(t.Context(), "flaky", store.ProviderTailscale); err != nil || !ok {
		t.Fatalf("grant rolled back: %t %v", ok, err)
	}
	if !network.serves("flaky", ProviderLocal) {
		t.Fatal("failed grant dropped the local route")
	}
	evs, err := s.st.ListEvents(t.Context(), "flaky", "exposure", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	logged := false
	for _, e := range evs {
		logged = logged || (e.Kind == "exposure" && e.Level == "error" && strings.Contains(e.Message, "tailscale permitted"))
	}
	if !logged {
		t.Fatal("no exposure error event for the failed grant")
	}
	network.failPrivateAfterLocal = nil
	// Re-granting is idempotent and retries the route.
	if err := s.SetProviderPermission(t.Context(), "flaky", store.ProviderTailscale, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if !network.serves("flaky", ProviderTailscale) {
		t.Fatal("re-grant did not retry the route")
	}
}
