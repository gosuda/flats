package core_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/store"
)

// fixtureNodes is a tailnet or NetBird mesh without real nodes: it keeps
// each host's handler and reports it ready at https://<host>.<domain>.
type fixtureNodes struct {
	domain   string
	mu       sync.Mutex
	handlers map[string]http.Handler
}

func newFixtureNodes(domain string) *fixtureNodes {
	return &fixtureNodes{domain: domain, handlers: map[string]http.Handler{}}
}

func (n *fixtureNodes) Serve(_ context.Context, host string, h http.Handler, _ bool) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.handlers[host] = h
	return n.URL(host), nil
}

func (n *fixtureNodes) Stop(host string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.handlers, host)
	return nil
}

func (n *fixtureNodes) URL(host string) string { return "https://" + host + "." + n.domain }

func (n *fixtureNodes) Status() core.NetStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	st := core.NetStatus{Kind: n.domain, Enabled: true}
	for host := range n.handlers {
		st.Hosts = append(st.Hosts, core.HostInfo{Host: host, URL: n.URL(host), State: "ready"})
	}
	return st
}

func (n *fixtureNodes) Close() error { return nil }

// redirect sends a request for path to host's handler and returns its
// status and Location.
func (n *fixtureNodes) redirect(host, path string) string {
	n.mu.Lock()
	h := n.handlers[host]
	n.mu.Unlock()
	if h == nil {
		return "not served"
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return strconv.Itoa(rec.Code) + " " + rec.Header().Get("Location")
}

// The tailnet methods. Funnel is not granted in the fixture.
func (n *fixtureNodes) ServeFunnel(context.Context, string, http.Handler) (string, error) {
	return "", core.ErrProviderNotPermitted
}
func (n *fixtureNodes) StopFunnel(string) error { return nil }
func (n *fixtureNodes) FunnelState(string) core.ExposureEndpoint {
	return core.ExposureEndpoint{Provider: core.ProviderFunnel, State: "unavailable"}
}

// fixtureMesh adapts fixtureNodes to the NetBird backend; NetBird Expose and
// console peers are not used.
type fixtureMesh struct{ *fixtureNodes }

func (m fixtureMesh) ServePrivate(ctx context.Context, host string, h http.Handler) (string, error) {
	return m.Serve(ctx, host, h, false)
}
func (m fixtureMesh) StopPrivate(host string) error   { return m.Stop(host) }
func (m fixtureMesh) RetirePrivate(host string) error { return m.Stop(host) }
func (m fixtureMesh) PrivateURL(host string) string   { return m.URL(host) }
func (m fixtureMesh) PrivateStatus() core.NetStatus   { return m.Status() }
func (m fixtureMesh) ServePublic(context.Context, string, http.Handler) (string, error) {
	return "", errors.New("not used")
}
func (m fixtureMesh) StopPublic(string) error       { return nil }
func (m fixtureMesh) RetirePublic(string) error     { return nil }
func (m fixtureMesh) PublicURL(string) string       { return "" }
func (m fixtureMesh) PublicStatus() core.NetStatus  { return core.NetStatus{} }
func (m fixtureMesh) ConsoleURL(string) string      { return "" }
func (m fixtureMesh) ConsoleStatus() core.NetStatus { return core.NetStatus{} }
func (m fixtureMesh) ServeConsole(context.Context, string, http.Handler) (string, error) {
	return "", errors.New("not used")
}
func (m fixtureMesh) StopConsole(string) error            { return nil }
func (m fixtureMesh) RetireConsole(string) error          { return nil }
func (m fixtureMesh) Hostnames(string) []string           { return nil }
func (m fixtureMesh) IdentityForIP(string, string) string { return "" }
func (m fixtureMesh) PeerCount() int                      { return 0 }

// noProxyFixture is a host with Local, Tailscale, NetBird and a public
// provider, and no reverse proxy provider.
type noProxyFixture struct {
	svc   *core.Service
	mgr   *provider.Manager
	pub   *local.Public
	tail  *fixtureNodes
	ports *strings.Replacer
}

// observeNoProxyFixture builds the fixture and returns every output rule 30
// covers, with listener ports and random preview names replaced by fixed
// tokens. It uses only APIs that origin/main d35a4f4 has, so the same
// function runs there unchanged (see TestNoProxyOutputsMatchMain).
func observeNoProxyFixture(t *testing.T) (map[string]string, *noProxyFixture) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	loop, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pubLn, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &noProxyFixture{pub: local.NewPublic(pubLn), tail: newFixtureNodes("tail.test")}
	mesh := fixtureMesh{newFixtureNodes("nb.test")}
	granted := []provider.ID{provider.Tailscale, provider.Netbird, provider.Portal}
	f.mgr, err = provider.New(filepath.Join(dir, "providers"), provider.Options{Local: loop, Tailscale: f.tail, Netbird: mesh, Portal: f.pub,
		Grants: &provider.File{Permitted: granted}})
	if err != nil {
		t.Fatal(err)
	}
	f.svc, err = core.New(ctx, core.Config{DataDir: dir, Store: st, Private: loop, Lifecycle: f.mgr,
		ConsoleURL: func() string { return "http://console.test" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.svc.Close(); f.mgr.Close(); loop.Close(); pubLn.Close(); st.Close() })
	if _, err := f.svc.SetPermittedNetworks([]string{"tailscale", "netbird", "portal"}, ""); err != nil {
		t.Fatal(err)
	}
	port := func(n *local.Net) string { return n.URL("x")[len("http://x.localhost"):] }
	f.ports = strings.NewReplacer(port(loop), ":LOCAL", port(pubLn), ":PUBLIC")

	setDefault := func(private string) {
		t.Helper()
		if _, err := f.svc.UpdateSettings(ctx, map[string]string{core.SetDefaultPrivate: private}); err != nil {
			t.Fatal(err)
		}
	}
	site := func(slug string, grants ...string) {
		t.Helper()
		if _, err := f.svc.SaveVersion(ctx, slug, files("index.html", slug), core.SaveMeta{}, core.ViaMCP); err != nil {
			t.Fatal(err)
		}
		publish(t, f.svc, slug)
		for _, g := range grants {
			if err := f.svc.SetProviderPermission(ctx, slug, g, true, core.ViaConsole); err != nil {
				t.Fatalf("allow %s on %s: %v", g, slug, err)
			}
		}
	}
	makePublicVia := func(slug string) {
		t.Helper()
		r, err := f.svc.SetVisibility(ctx, slug, store.Public, core.ViaConsole, "")
		if err != nil || r.Approval == nil {
			t.Fatalf("visibility: %v %+v", err, r)
		}
		if _, err := f.svc.Decide(ctx, r.Approval.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	out := map[string]string{}
	view := func(name, slug string) {
		t.Helper()
		v, err := f.svc.GetFlat(ctx, slug)
		if err != nil {
			t.Fatal(err)
		}
		var eps []string
		for _, ep := range v.Endpoints {
			eps = append(eps, fmt.Sprintf("%s %s %s %s %s", ep.Provider, ep.Audience, ep.Host, ep.URL, ep.State))
		}
		slices.Sort(eps)
		out[name+".private_url"] = f.ports.Replace(v.PrivateURL)
		out[name+".public_url"] = f.ports.Replace(v.PublicURL)
		out[name+".connection_state"] = v.ConnectionState
		out[name+".endpoints"] = f.ports.Replace(strings.Join(eps, "; "))
	}

	site("solo")
	setDefault("local")
	site("tail", "tailscale")
	view("solo", "solo")
	view("tail", "tail")
	site("mesh", "tailscale", "netbird")
	setDefault("tailscale")
	view("mesh-default-tailscale", "mesh")
	setDefault("netbird")
	view("mesh-default-netbird", "mesh")
	setDefault("local")

	site("pub", "portal")
	makePublicVia("pub")
	view("pub", "pub")

	p, err := f.svc.OpenPreview(ctx, "tail", 0)
	if err != nil {
		t.Fatal(err)
	}
	previews, err := f.svc.ListPreviews(ctx, "tail")
	if err != nil {
		t.Fatal(err)
	}
	hide := strings.NewReplacer(p.Host, "PREVIEW")
	out["preview.open_url"] = hide.Replace(f.ports.Replace(p.URL))
	for _, l := range previews {
		out["preview.list_url"] = hide.Replace(f.ports.Replace(l.URL))
	}

	site("old-name", "tailscale", "portal")
	makePublicVia("old-name")
	if _, err := f.svc.RenameSlug(ctx, "old-name", "new-name", core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	view("renamed", "new-name")
	_, _, hdr := get(t, loop.URL("old-name")+"/a?b=1")
	out["redirect.local"] = f.ports.Replace(hdr.Get("Location"))
	out["redirect.tailscale"] = f.tail.redirect("old-name", "/a?b=1")
	_, _, hdr = get(t, f.pub.URL("old-name")+"/a?b=1")
	out["redirect.public"] = f.ports.Replace(hdr.Get("Location"))
	return out, f
}

// Rule 30: with neither proxy provider on, the flat views, preview
// responses, endpoint lists and redirects equal those of origin/main
// d35a4f4, except for the rule 30 exceptions, which assert the new behavior.
//
// The expected values were recorded by running observeNoProxyFixture,
// unchanged, against a scratch checkout of d35a4f4 outside the repository
// working tree. They agree with main's privateRank (Tailscale, NetBird,
// Local), previewView (Tailscale over Local) and redirectHandler (always the
// new flat's private_url or public_url).
func TestNoProxyOutputsMatchMain(t *testing.T) {
	got, f := observeNoProxyFixture(t)
	// Recorded on d35a4f4. The local.Public stand-in for Portal reports its
	// routes as starting, so main's public_url selection decides there too.
	main := map[string]string{
		"solo.private_url":                        "http://solo.localhost:LOCAL",
		"solo.public_url":                         "",
		"solo.connection_state":                   "ready",
		"solo.endpoints":                          "local current solo http://solo.localhost:LOCAL ready",
		"tail.private_url":                        "https://tail.tail.test",
		"tail.public_url":                         "",
		"tail.connection_state":                   "ready",
		"tail.endpoints":                          "local current tail http://tail.localhost:LOCAL ready; tailscale current tail https://tail.tail.test ready",
		"mesh-default-tailscale.private_url":      "https://mesh.tail.test",
		"mesh-default-tailscale.public_url":       "",
		"mesh-default-tailscale.connection_state": "ready",
		"mesh-default-tailscale.endpoints":        "local current mesh http://mesh.localhost:LOCAL ready; netbird current mesh https://mesh.nb.test ready; tailscale current mesh https://mesh.tail.test ready",
		"mesh-default-netbird.public_url":         "",
		"mesh-default-netbird.connection_state":   "ready",
		"mesh-default-netbird.endpoints":          "local current mesh http://mesh.localhost:LOCAL ready; netbird current mesh https://mesh.nb.test ready; tailscale current mesh https://mesh.tail.test ready",
		"pub.private_url":                         "http://pub.localhost:LOCAL",
		"pub.public_url":                          "http://pub-pub.localhost:PUBLIC",
		"pub.connection_state":                    "starting",
		"pub.endpoints":                           "local current pub http://pub.localhost:LOCAL ready; portal current pub http://pub-pub.localhost:PUBLIC starting",
		"preview.open_url":                        "https://PREVIEW.tail.test",
		"preview.list_url":                        "https://PREVIEW.tail.test",
		"renamed.private_url":                     "https://new-name.tail.test",
		"renamed.public_url":                      "http://pub-new-name.localhost:PUBLIC",
		"renamed.connection_state":                "starting",
		"renamed.endpoints":                       "local current new-name http://new-name.localhost:LOCAL ready; portal current new-name http://pub-new-name.localhost:PUBLIC starting; tailscale current new-name https://new-name.tail.test ready",
		"redirect.tailscale":                      "307 https://new-name.tail.test/a?b=1",
		"redirect.public":                         "http://pub-new-name.localhost:PUBLIC/a?b=1",
	}
	// The rule 30 exceptions, asserting the new behavior. On d35a4f4 the
	// flat reported https://mesh.tail.test (Tailscale always ranked first)
	// and a Local arrival was redirected to the new flat's private_url,
	// https://new-name.tail.test/a?b=1.
	exceptions := map[string]string{
		"mesh-default-netbird.private_url": "https://mesh.nb.test",
		"redirect.local":                   "http://new-name.localhost:LOCAL/a?b=1",
	}
	want := maps.Clone(main)
	maps.Copy(want, exceptions)
	for _, k := range slices.Sorted(maps.Keys(want)) {
		if got[k] != want[k] {
			t.Errorf("%s = %q, want %q", k, got[k], want[k])
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected output %s = %q", k, got[k])
		}
	}
	// No reverse proxy route or listener exists.
	for k, v := range got {
		if strings.Contains(v, "proxy") {
			t.Errorf("%s names a proxy: %q", k, v)
		}
	}
	for _, ep := range f.mgr.HostStatus() {
		if (ep.Provider == core.ProviderProxy || ep.Provider == core.ProviderProxyPublic) && (ep.Configured || ep.Permitted) {
			t.Errorf("proxy provider present: %+v", ep)
		}
	}
	if _, err := f.mgr.ServeExposure(context.Background(), core.ExposureRequest{Slug: "solo", Host: "solo", Visibility: "private",
		Audience: core.AudienceCurrent, Handler: http.NotFoundHandler(), Permitted: []core.ProviderID{core.ProviderProxy}}); !errors.Is(err, core.ErrProviderNotPermitted) {
		t.Errorf("a proxy route opened without the provider: %v", err)
	}
}
