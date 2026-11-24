package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
)

// docs/spec/portal-require-auth.md, rules 7 to 9, in the provider manager: a Portal open that carries the refusal opens
// nothing on a backend without the gate, and fails no other public provider.

const authDetail = "portal.require_auth is on and notes has no sign-in. Run flats signin set notes to turn sign-in on, or turn the setting off in the console."

func authManager(t *testing.T) (*Manager, *fakePortal, *fakeTail) {
	t.Helper()
	return authManagerWith(t, func(p *fakePortal) PortalNet { return p })
}

// authManagerWith is authManager with a Portal backend that wraps the fake one.
func authManagerWith(t *testing.T, wrap func(*fakePortal) PortalNet) (*Manager, *fakePortal, *fakeTail) {
	t.Helper()
	dir := t.TempDir()
	if err := Save(dir, File{Version: 1, Permitted: []ID{Portal, Funnel}, Migration: Migration{GrantsFromState: []ID{}}}); err != nil {
		t.Fatal(err)
	}
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	pubLn, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pubLn.Close() })
	tail := &fakeTail{}
	pub := &fakePortal{Public: local.NewPublic(pubLn)}
	m, err := New(dir, Options{Local: ln, Tailscale: tail, Portal: wrap(pub)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, pub, tail
}

func authRequest(permitted []ID, si *core.PortalSignIn) ExposureRequest {
	return ExposureRequest{Slug: "notes", Host: "notes", Visibility: "public", Audience: AudienceCurrent,
		Handler: text("public"), Permitted: permitted, SignIn: si}
}

func portalEndpointOf(res ExposureResult) (ExposureEndpoint, bool) {
	for _, ep := range res.Endpoints {
		if ep.Provider == Portal {
			return ep, true
		}
	}
	return ExposureEndpoint{}, false
}

// A refusal is an unavailable endpoint, and the other public provider of the flat opens and serves.
func TestPortalRefusalFailsNoOtherPublicProvider(t *testing.T) {
	m, pub, tail := authManager(t)
	ctx := context.Background()
	res, err := m.ServeExposure(ctx, authRequest([]ID{Portal, Funnel}, &core.PortalSignIn{Seq: 1, Refuse: authDetail}))
	if err != nil {
		t.Fatalf("a refusal failed the open: %v", err)
	}
	ep, ok := portalEndpointOf(res)
	if !ok || ep.State != "unavailable" || ep.Detail != authDetail || ep.Ready {
		t.Fatalf("the Portal endpoint = %+v", ep)
	}
	if pub.serve != 0 || tail.funnel != 1 {
		t.Fatalf("the backend served %d times, Funnel %d times", pub.serve, tail.funnel)
	}
	if has, _ := m.HasProviderRoute(ctx, "notes", Portal); has {
		t.Fatal("a refused open registered a route")
	}
	status, err := m.ExposureStatus(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if ep, ok := portalEndpointOf(status); !ok || ep.State != "unavailable" || ep.Detail != authDetail {
		t.Fatalf("the observed Portal endpoint = %+v", ep)
	}
}

// With no other public route, the open fails with the detail of the refusal.
func TestPortalRefusalIsTheErrorWhenNothingElseOpens(t *testing.T) {
	m, pub, _ := authManager(t)
	res, err := m.ServeExposure(context.Background(), authRequest([]ID{Portal}, &core.PortalSignIn{Seq: 1, Refuse: authDetail}))
	var ae *core.PortalAuthError
	if !errors.As(err, &ae) || ae.Detail != authDetail {
		t.Fatalf("the open = %v, want the refusal", err)
	}
	if ep, ok := portalEndpointOf(res); !ok || ep.State != "unavailable" || pub.serve != 0 {
		t.Fatalf("the Portal endpoint = %+v, served %d times", ep, pub.serve)
	}
}

// A served route stops when the next open carries the refusal, and the next open without it serves again.
func TestPortalRefusalStopsAServedRoute(t *testing.T) {
	m, pub, _ := authManager(t)
	ctx := context.Background()
	if _, err := m.ServeExposure(ctx, authRequest([]ID{Portal}, nil)); err != nil || pub.serve != 1 {
		t.Fatalf("the first open = %v, served %d times", err, pub.serve)
	}
	if has, _ := m.HasProviderRoute(ctx, "notes", Portal); !has {
		t.Fatal("the route is not registered")
	}
	if _, err := m.ServeExposure(ctx, authRequest([]ID{Portal}, &core.PortalSignIn{Seq: 1, Refuse: authDetail})); err == nil {
		t.Fatal("a refusal of the only public route did not fail")
	}
	if has, _ := m.HasProviderRoute(ctx, "notes", Portal); has {
		t.Fatal("the refused route is still registered")
	}
	if pub.serve != 1 {
		t.Fatalf("the backend served %d times", pub.serve)
	}
	if _, served := pub.Hidden("notes"); served {
		t.Fatal("the backend still serves the flat that the refusal stopped")
	}
	if _, err := m.ServeExposure(ctx, authRequest([]ID{Portal}, &core.PortalSignIn{Seq: 1})); err != nil || pub.serve != 2 {
		t.Fatalf("the open after the refusal = %v, served %d times", err, pub.serve)
	}
}

// stopFailsPortal is a Portal backend whose stop fails, as a relay that refuses the unregistration does.
type stopFailsPortal struct {
	*fakePortal
	err error
}

func (p *stopFailsPortal) Stop(string) error { return p.err }

// A stop that is not confirmed is a failure, not a refusal (rule 8): the endpoint is an error, the open fails, and the route
// stays registered.
func TestPortalRefusalWhoseStopIsNotConfirmed(t *testing.T) {
	var backend *stopFailsPortal
	m, _, _ := authManagerWith(t, func(p *fakePortal) PortalNet { backend = &stopFailsPortal{fakePortal: p}; return backend })
	ctx := context.Background()
	if _, err := m.ServeExposure(ctx, authRequest([]ID{Portal}, nil)); err != nil {
		t.Fatal(err)
	}
	backend.err = errors.New("relay refused the unregistration")
	res, err := m.ServeExposure(ctx, authRequest([]ID{Portal}, &core.PortalSignIn{Seq: 1, Refuse: authDetail}))
	if err == nil {
		t.Fatal("a refusal whose stop was not confirmed did not fail the open")
	}
	if ep, ok := portalEndpointOf(res); !ok || ep.State != "error" {
		t.Fatalf("the Portal endpoint = %+v, want the state error", ep)
	}
	if has, _ := m.HasProviderRoute(ctx, "notes", Portal); !has {
		t.Fatal("the route was forgotten although its stop was not confirmed")
	}
}

// The mark Redirect, and an open that carries no refusal, are not refused.
func TestPortalRefusalSparesTheRedirect(t *testing.T) {
	m, pub, _ := authManager(t)
	if _, err := m.ServeExposure(context.Background(), authRequest([]ID{Portal}, &core.PortalSignIn{Redirect: true, Refuse: authDetail})); err != nil || pub.serve != 1 {
		t.Fatalf("the redirect open = %v, served %d times", err, pub.serve)
	}
}
