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

func publicLifecycleFlat(t *testing.T, s *Service, slugName string, providers ...string) {
	t.Helper()
	lifecycleSave(t, s, slugName, "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, slugName))
	for _, p := range providers {
		if err := s.SetProviderPermission(t.Context(), slugName, p, true, ViaConsole); err != nil {
			t.Fatal(err)
		}
	}
	req, err := s.SetVisibility(t.Context(), slugName, store.Public, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(t.Context(), req.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
}

// A grant must not widen public exposure: a public provider granted to a
// flat that is already Public waits for the next approved transition, and a
// later Tailscale grant does not open it through the back door either.
func TestProviderGrantDoesNotWidenPublicExposure(t *testing.T) {
	s, _ := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	publicLifecycleFlat(t, s, "wide", store.ProviderFunnel)
	if !network.serves("wide", ProviderFunnel) {
		t.Fatal("approved Funnel route not serving")
	}
	if err := s.SetProviderPermission(t.Context(), "wide", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if network.serves("wide", ProviderPortal) {
		t.Fatal("Portal grant opened a new public route without approval")
	}
	if err := s.SetProviderPermission(t.Context(), "wide", store.ProviderTailscale, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if !network.serves("wide", ProviderTailscale) {
		t.Fatal("Tailscale grant did not open its route")
	}
	if network.serves("wide", ProviderPortal) {
		t.Fatal("Tailscale grant opened the unapproved Portal route")
	}
	if !network.serves("wide", ProviderFunnel) {
		t.Fatal("Tailscale grant dropped the approved Funnel route")
	}
	fv, err := s.GetFlat(t.Context(), "wide")
	if err != nil {
		t.Fatal(err)
	}
	if fv.PublicURL != "https://wide.tailscale-funnel.test" || fv.PrivateURL != "https://wide.tailscale.test" {
		t.Fatalf("status lost an endpoint: public=%q private=%q", fv.PublicURL, fv.PrivateURL)
	}
}

// Only the granted provider's own route decides the outcome: a broken
// sibling neither fails a grant nor is blamed on it.
func TestProviderGrantIgnoresSiblingFailures(t *testing.T) {
	t.Run("public sibling", func(t *testing.T) {
		s, _ := newTestService(t)
		network := newLifecycleRouteNet()
		s.cfg.Lifecycle = network
		publicLifecycleFlat(t, s, "sib", store.ProviderPortal, store.ProviderFunnel)
		network.failOpen = map[ProviderID]error{ProviderFunnel: fmt.Errorf("funnel down: %w", ErrProviderUnavailable)}
		if err := s.SetProviderPermission(t.Context(), "sib", store.ProviderTailscale, true, ViaConsole); err != nil {
			t.Fatalf("Tailscale grant failed for a Funnel error: %v", err)
		}
		if !network.serves("sib", ProviderTailscale) {
			t.Fatal("Tailscale route not opened")
		}
		evs, err := s.st.ListEvents(t.Context(), "sib", "exposure", 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		warned := false
		for _, e := range evs {
			warned = warned || (e.Level == "warn" && strings.Contains(e.Message, "funnel down"))
		}
		if !warned {
			t.Fatal("sibling error was not logged")
		}
	})
	t.Run("public grant on private flat", func(t *testing.T) {
		s, _ := newTestService(t)
		network := newLifecycleRouteNet()
		s.cfg.Lifecycle = network
		lifecycleSave(t, s, "priv", "one")
		lifecycleApprove(t, s, lifecycleRequest(t, s, "priv"))
		if err := s.SetProviderPermission(t.Context(), "priv", store.ProviderTailscale, true, ViaConsole); err != nil {
			t.Fatal(err)
		}
		network.failPrivateAfterLocal = fmt.Errorf("tailnet down: %w", ErrProviderNotPermitted)
		if err := s.SetProviderPermission(t.Context(), "priv", store.ProviderPortal, true, ViaConsole); err != nil {
			t.Fatalf("Portal grant on a private flat failed for a Tailscale error: %v", err)
		}
	})
}

// A failed Tailscale grant is categorized as not ready, never as "not
// permitted", even when the backend reports ErrProviderNotPermitted.
func TestProviderGrantFailureCategory(t *testing.T) {
	s, _ := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	lifecycleSave(t, s, "cat", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "cat"))
	network.failPrivateAfterLocal = fmt.Errorf("host provider: %w", ErrProviderNotPermitted)
	err := s.SetProviderPermission(t.Context(), "cat", store.ProviderTailscale, true, ViaConsole)
	if got := ErrorCategory(err); got != "provider_not_ready" {
		t.Fatalf("category = %q (%v)", got, err)
	}
}

// The legacy backend serves its private host whatever the grant, so a grant
// there only records the permission.
func TestProviderGrantLegacyBackendRecordsOnly(t *testing.T) {
	s, _ := newTestService(t)
	lifecycleSave(t, s, "legacy-grant", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "legacy-grant"))
	for _, p := range []string{store.ProviderTailscale, store.ProviderPortal} {
		if err := s.SetProviderPermission(t.Context(), "legacy-grant", p, true, ViaConsole); err != nil {
			t.Fatal(err)
		}
	}
	if s.state("legacy-grant").publicServed {
		t.Fatal("legacy grant opened a public route")
	}
}
