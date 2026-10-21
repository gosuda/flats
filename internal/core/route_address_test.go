package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

// staticRoutes reports a fixed endpoint list per flat, so address selection
// can be checked for any combination of providers and states.
type staticRoutes struct {
	mu  sync.Mutex
	eps map[string][]ExposureEndpoint
}

func (n *staticRoutes) ServeExposure(context.Context, ExposureRequest) (ExposureResult, error) {
	return ExposureResult{}, nil
}

func (n *staticRoutes) StopPublicRoutes(context.Context, string) (PublicStopResult, error) {
	return PublicStopResult{}, nil
}

func (n *staticRoutes) ExposurePolicy(context.Context) (string, error) { return "static", nil }

func (n *staticRoutes) ExposureStatus(_ context.Context, slugName string) (ExposureResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return ExposureResult{Endpoints: slices.Clone(n.eps[slugName])}, nil
}

func (n *staticRoutes) set(slugName string, eps ...ExposureEndpoint) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.eps[slugName] = eps
}

// routeEndpoint is a registered route of host on id.
func routeEndpoint(id ProviderID, host string, audience ExposureAudience, state string) ExposureEndpoint {
	return ExposureEndpoint{Provider: id, URL: "https://" + host + "." + string(id) + ".test", State: state,
		Configured: true, Permitted: true, Ready: state == "ready", Audience: audience, Host: host}
}

type addressHost struct {
	s    *Service
	st   *store.Store
	net  *staticRoutes
	slug string
}

// newAddressHost creates the flat "site" on a host with the given private
// backend and host grants; the flat is allowed on grants.
func newAddressHost(t *testing.T, backend string, permitted []string, grants ...string) *addressHost {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	net := &staticRoutes{eps: map[string][]ExposureEndpoint{}}
	s, err := New(t.Context(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}},
		Lifecycle: net, PrivateBackend: backend, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	if slices.Contains(permitted, "proxy") || slices.Contains(permitted, "proxy-public") {
		if _, err := s.UpdateSettings(t.Context(), map[string]string{SetProxyTemplate: "https://{host}.site.example.org"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetPermittedNetworks(permitted, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFlat(t.Context(), "site", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	for _, p := range grants {
		if err := st.SetProviderPermission(t.Context(), "site", p, true); err != nil {
			t.Fatal(err)
		}
	}
	return &addressHost{s: s, st: st, net: net, slug: "site"}
}

func (h *addressHost) defaults(t *testing.T, private, public string) {
	t.Helper()
	if _, err := h.s.UpdateSettings(t.Context(), map[string]string{SetDefaultPrivate: private, SetDefaultPublic: public}); err != nil {
		t.Fatal(err)
	}
}

func (h *addressHost) makePublic(t *testing.T) {
	t.Helper()
	f, err := h.st.GetFlat(t.Context(), h.slug)
	if err != nil {
		t.Fatal(err)
	}
	f.Visibility = store.Public
	if err := h.st.UpdateFlat(t.Context(), f); err != nil {
		t.Fatal(err)
	}
}

func (h *addressHost) view(t *testing.T) FlatView {
	t.Helper()
	v, err := h.s.GetFlat(t.Context(), h.slug)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPreferredRoute(t *testing.T) {
	ts := ExposureEndpoint{Provider: ProviderTailscale}
	nb := ExposureEndpoint{Provider: ProviderNetbird}
	lo := ExposureEndpoint{Provider: ProviderLocal}
	for _, c := range []struct {
		name  string
		eps   []ExposureEndpoint
		def   ProviderID
		order []ProviderID
		want  ProviderID
	}{
		{"order", []ExposureEndpoint{lo, nb, ts}, "", privateOrder, ProviderTailscale},
		{"default wins", []ExposureEndpoint{lo, nb, ts}, ProviderNetbird, privateOrder, ProviderNetbird},
		{"default absent", []ExposureEndpoint{lo, nb}, ProviderTailscale, privateOrder, ProviderNetbird},
		{"default without order", []ExposureEndpoint{lo, nb}, ProviderNetbird, nil, ProviderNetbird},
		{"nothing without order", []ExposureEndpoint{lo, nb}, ProviderTailscale, nil, ""},
		{"no candidates", nil, ProviderTailscale, privateOrder, ""},
	} {
		got, ok := preferredRoute(c.eps, c.def, c.order)
		if got.Provider != c.want || ok != (c.want != "") {
			t.Errorf("%s: %q %v, want %q", c.name, got.Provider, ok, c.want)
		}
	}
}

// Rule 25: private_url is the route on the host's private default, else the
// first of Tailscale, NetBird and Local among the routes the flat allows.
func TestPrivateURLFollowsHostDefault(t *testing.T) {
	local := routeEndpoint(ProviderLocal, "site", AudienceCurrent, "ready")
	tail := routeEndpoint(ProviderTailscale, "site", AudienceCurrent, "ready")
	mesh := routeEndpoint(ProviderNetbird, "site", AudienceCurrent, "ready")
	for _, c := range []struct {
		name    string
		backend string
		def     string
		grants  []string
		eps     []ExposureEndpoint
		want    ExposureEndpoint
	}{
		{"local default keeps tailscale", "local", "local", []string{"tailscale"}, []ExposureEndpoint{local, tail}, tail},
		{"tailscale backend with local default", "tailscale", "local", []string{"tailscale"}, []ExposureEndpoint{local, tail}, tail},
		{"both meshes, tailscale default", "local", "tailscale", []string{"tailscale", "netbird"}, []ExposureEndpoint{local, tail, mesh}, tail},
		{"both meshes, netbird default", "local", "netbird", []string{"tailscale", "netbird"}, []ExposureEndpoint{local, tail, mesh}, mesh},
		{"both meshes, no default", "local", "", []string{"tailscale", "netbird"}, []ExposureEndpoint{local, mesh, tail}, tail},
		{"both meshes, tailscale backend", "tailscale", "", []string{"tailscale", "netbird"}, []ExposureEndpoint{local, mesh, tail}, tail},
		{"netbird default without the grant", "local", "netbird", []string{"tailscale"}, []ExposureEndpoint{local, tail, mesh}, tail},
		{"tailscale default, only netbird allowed", "local", "tailscale", []string{"netbird"}, []ExposureEndpoint{local, tail, mesh}, mesh},
		{"readiness does not matter", "local", "netbird", []string{"tailscale", "netbird"},
			[]ExposureEndpoint{local, tail, routeEndpoint(ProviderNetbird, "site", AudienceCurrent, "starting")},
			routeEndpoint(ProviderNetbird, "site", AudienceCurrent, "starting")},
		{"tailscale default without an address", "local", "tailscale", []string{"tailscale"},
			[]ExposureEndpoint{local, {Provider: ProviderTailscale, State: "error", Configured: true, Permitted: true, Audience: AudienceCurrent, Host: "site"}}, local},
		{"local only", "local", "", nil, []ExposureEndpoint{local}, local},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newAddressHost(t, c.backend, []string{"tailscale", "netbird"}, c.grants...)
			h.defaults(t, c.def, "")
			h.net.set("site", c.eps...)
			v := h.view(t)
			if v.PrivateURL != c.want.URL || v.PrivateState != c.want.State {
				t.Fatalf("private_url %q (%s), want %q (%s)", v.PrivateURL, v.PrivateState, c.want.URL, c.want.State)
			}
		})
	}
}

// Rule 25: a Public flat reports its route on the host's public default when
// that route is ready, and otherwise keeps the current selection.
func TestPublicURLFollowsReadyHostDefault(t *testing.T) {
	local := routeEndpoint(ProviderLocal, "site", AudienceCurrent, "ready")
	portal := routeEndpoint(ProviderPortal, "site", AudienceCurrent, "ready")
	zrok := routeEndpoint(ProviderZrok, "site", AudienceCurrent, "ready")
	zrokStarting := routeEndpoint(ProviderZrok, "site", AudienceCurrent, "starting")
	quick := routeEndpoint(ProviderCloudflareQuick, "site", AudienceCurrent, "ready")
	all := []string{"portal", "zrok", "cloudflare-quick"}
	for _, c := range []struct {
		name   string
		def    string
		grants []string
		eps    []ExposureEndpoint
		want   ExposureEndpoint
	}{
		// Without a default the last ready route wins, as before.
		{"no default", "", all, []ExposureEndpoint{local, portal, zrok, quick}, quick},
		{"ready default", "portal", all, []ExposureEndpoint{local, portal, zrok, quick}, portal},
		{"ready default listed last", "cloudflare-quick", all, []ExposureEndpoint{local, quick, portal, zrok}, quick},
		{"default not ready", "zrok", all, []ExposureEndpoint{local, portal, zrokStarting}, portal},
		{"default not allowed", "zrok", []string{"portal", "cloudflare-quick"}, []ExposureEndpoint{local, zrok, portal, quick}, quick},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newAddressHost(t, "local", all, c.grants...)
			h.defaults(t, "", c.def)
			h.makePublic(t)
			h.net.set("site", c.eps...)
			v := h.view(t)
			if v.PublicURL != c.want.URL || v.ConnectionState != c.want.State {
				t.Fatalf("public_url %q (%s), want %q (%s)", v.PublicURL, v.ConnectionState, c.want.URL, c.want.State)
			}
		})
	}
}

// Rules 25 and 26 with the reverse proxy: it ranks after the meshes and
// before Local, a proxy default wins, and it serves previews.
func TestProxyAddressSelection(t *testing.T) {
	local := routeEndpoint(ProviderLocal, "site", AudienceCurrent, "ready")
	tail := routeEndpoint(ProviderTailscale, "site", AudienceCurrent, "ready")
	mesh := routeEndpoint(ProviderNetbird, "site", AudienceCurrent, "ready")
	px := routeEndpoint(ProviderProxy, "site", AudienceCurrent, "ready")
	for _, c := range []struct {
		name   string
		def    string
		grants []string
		eps    []ExposureEndpoint
		want   ExposureEndpoint
	}{
		{"proxy before local", "", []string{"proxy"}, []ExposureEndpoint{local, px}, px},
		{"meshes before proxy", "", []string{"tailscale", "netbird", "proxy"}, []ExposureEndpoint{local, px, mesh, tail}, tail},
		{"netbird before proxy", "", []string{"netbird", "proxy"}, []ExposureEndpoint{local, px, mesh}, mesh},
		{"proxy default", "proxy", []string{"tailscale", "proxy"}, []ExposureEndpoint{local, tail, px}, px},
		{"proxy default not allowed", "proxy", []string{"tailscale"}, []ExposureEndpoint{local, tail, px}, tail},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newAddressHost(t, "local", []string{"tailscale", "netbird", "proxy"}, c.grants...)
			h.defaults(t, c.def, "")
			h.net.set("site", c.eps...)
			if v := h.view(t); v.PrivateURL != c.want.URL {
				t.Fatalf("private_url %q, want %q", v.PrivateURL, c.want.URL)
			}
		})
	}

	const host = "site-abcdefgh"
	plocal := routeEndpoint(ProviderLocal, host, AudienceDraft, "ready")
	ptail := routeEndpoint(ProviderTailscale, host, AudienceDraft, "ready")
	pmesh := routeEndpoint(ProviderNetbird, host, AudienceDraft, "ready")
	pproxy := routeEndpoint(ProviderProxy, host, AudienceDraft, "ready")
	for _, c := range []struct {
		name   string
		def    string
		grants []string
		eps    []ExposureEndpoint
		want   ExposureEndpoint
	}{
		{"preview on proxy", "", []string{"proxy"}, []ExposureEndpoint{plocal, pproxy}, pproxy},
		{"preview tailscale before proxy", "", []string{"tailscale", "proxy"}, []ExposureEndpoint{plocal, pproxy, ptail}, ptail},
		{"preview proxy default", "proxy", []string{"tailscale", "proxy"}, []ExposureEndpoint{plocal, ptail, pproxy}, pproxy},
		{"preview netbird default", "netbird", []string{"netbird", "proxy"}, []ExposureEndpoint{plocal, pmesh, pproxy}, pproxy},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newAddressHost(t, "local", []string{"tailscale", "netbird", "proxy"}, c.grants...)
			h.defaults(t, c.def, "")
			h.net.set("site", c.eps...)
			if pv := h.s.previewView(t.Context(), store.Preview{Flat: "site", Host: host}); pv.URL != c.want.URL {
				t.Fatalf("preview %q, want %q", pv.URL, c.want.URL)
			}
		})
	}

	// A Public flat whose only public route is proxy-public reports it.
	h := newAddressHost(t, "local", []string{"proxy-public"}, "proxy-public")
	h.makePublic(t)
	pub := routeEndpoint(ProviderProxyPublic, "site", AudienceCurrent, "ready")
	h.net.set("site", local, pub)
	if v := h.view(t); v.PublicURL != pub.URL || v.ConnectionState != "ready" {
		t.Fatalf("public_url %q (%s)", v.PublicURL, v.ConnectionState)
	}
}

// Rule 26: previews are reported over Local and Tailscale with the same
// selection; a NetBird default, which cannot serve a preview, changes
// nothing.
func TestPreviewURLSelection(t *testing.T) {
	const host = "site-abcdefgh"
	local := routeEndpoint(ProviderLocal, host, AudienceDraft, "ready")
	tail := routeEndpoint(ProviderTailscale, host, AudienceDraft, "starting")
	mesh := routeEndpoint(ProviderNetbird, host, AudienceDraft, "ready")
	for _, c := range []struct {
		name   string
		def    string
		grants []string
		eps    []ExposureEndpoint
		want   ExposureEndpoint
	}{
		{"tailscale before local", "", []string{"tailscale"}, []ExposureEndpoint{tail, local}, tail},
		{"local default", "local", []string{"tailscale"}, []ExposureEndpoint{local, tail}, tail},
		{"netbird default", "netbird", []string{"tailscale", "netbird"}, []ExposureEndpoint{local, tail, mesh}, tail},
		{"tailscale not allowed", "tailscale", nil, []ExposureEndpoint{local, tail}, local},
		{"current routes ignored", "", []string{"tailscale"}, []ExposureEndpoint{local, routeEndpoint(ProviderTailscale, host, AudienceCurrent, "ready")}, local},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newAddressHost(t, "local", []string{"tailscale", "netbird"}, c.grants...)
			h.defaults(t, c.def, "")
			h.net.set("site", c.eps...)
			pv := h.s.previewView(t.Context(), store.Preview{Flat: "site", Host: host})
			if pv.URL != c.want.URL || pv.State != c.want.State {
				t.Fatalf("preview %q (%s), want %q (%s)", pv.URL, pv.State, c.want.URL, c.want.State)
			}
		})
	}
}

// Rule 29: a redirect sends a visitor to the new flat on the provider the
// request arrived on, and falls back to private_url or public_url.
func TestRedirectFollowsArrivalProvider(t *testing.T) {
	local := routeEndpoint(ProviderLocal, "site", AudienceCurrent, "ready")
	tail := routeEndpoint(ProviderTailscale, "site", AudienceCurrent, "ready")
	portal := routeEndpoint(ProviderPortal, "site", AudienceCurrent, "ready")
	zrok := routeEndpoint(ProviderZrok, "site", AudienceCurrent, "starting")
	h := newAddressHost(t, "local", []string{"tailscale", "netbird", "portal", "zrok"}, "tailscale", "portal", "zrok")
	h.net.set("site", local, tail, portal, zrok)
	redirect := h.s.redirectHandler(func(p ProviderID, public bool) string { return h.s.redirectTarget("site", p, public) })
	location := func(p ProviderID, tag bool) (int, string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/a?b=1", nil)
		if tag {
			r = r.WithContext(WithRouteProvider(r.Context(), p))
		}
		rec := httptest.NewRecorder()
		redirect.ServeHTTP(rec, r)
		return rec.Code, rec.Header().Get("Location")
	}
	want := func(name string, p ProviderID, tag bool, code int, base string) {
		t.Helper()
		gotCode, got := location(p, tag)
		wantLoc := ""
		if base != "" {
			wantLoc = base + "/a?b=1"
		}
		if gotCode != code || got != wantLoc {
			t.Errorf("%s: %d %q, want %d %q", name, gotCode, got, code, wantLoc)
		}
	}

	want("local", ProviderLocal, true, http.StatusTemporaryRedirect, local.URL)
	want("tailscale", ProviderTailscale, true, http.StatusTemporaryRedirect, tail.URL)
	want("netbird without a route", ProviderNetbird, true, http.StatusTemporaryRedirect, tail.URL)
	want("untagged", "", false, http.StatusTemporaryRedirect, tail.URL)
	// A public redirect has no target while the flat is Private.
	want("portal while private", ProviderPortal, true, http.StatusServiceUnavailable, "")

	h.makePublic(t)
	want("portal", ProviderPortal, true, http.StatusTemporaryRedirect, portal.URL)
	// zrok is registered but not ready: the public fallback.
	want("zrok not ready", ProviderZrok, true, http.StatusTemporaryRedirect, portal.URL)
	want("funnel without a route", ProviderFunnel, true, http.StatusTemporaryRedirect, portal.URL)
	want("local on a public flat", ProviderLocal, true, http.StatusTemporaryRedirect, local.URL)

	// With no route left there is no target yet.
	h.net.set("site")
	want("no route", ProviderLocal, true, http.StatusServiceUnavailable, "")
	want("no public route", ProviderPortal, true, http.StatusServiceUnavailable, "")
}
