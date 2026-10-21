package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/proxy"
)

// proxyFixture is a Manager with a real reverse proxy listener whose
// template a test can change.
type proxyFixture struct {
	m        *Manager
	net      *proxy.Net
	mu       sync.Mutex
	template string
}

func (f *proxyFixture) setTemplate(s string) {
	f.mu.Lock()
	f.template = s
	f.mu.Unlock()
}

func (f *proxyFixture) current() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.template
}

func (f *proxyFixture) parsed() config.Template {
	t, _ := config.ParseTemplate(f.current())
	return t
}

func newProxyFixture(t *testing.T, granted ...ID) *proxyFixture {
	t.Helper()
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	portalLn, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { portalLn.Close() })
	f := &proxyFixture{template: "https://{host}.site.example.org"}
	f.net, err = proxy.Listen("127.0.0.1:0", f.parsed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.net.Close() })
	f.m, err = New(t.TempDir(), Options{Local: ln, Portal: &fakePortal{Public: local.NewPublic(portalLn)},
		Proxy: f.net, ProxyTemplate: f.current, Grants: &File{Permitted: granted}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// proxyGet sends a request for host label to the proxy listener as a
// reverse proxy would, and returns the status and body.
func (f *proxyFixture) proxyGet(t *testing.T, label string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+f.net.Addr()+"/", nil)
	req.Host = label + ".site.example.org"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// marked answers with its name and the provider the request arrived on.
func marked(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := core.RouteProvider(r.Context())
		w.Write([]byte(name + " via " + string(id)))
	})
}

func endpointOf(res ExposureResult, id ID) (ExposureEndpoint, bool) {
	for _, ep := range res.Endpoints {
		if ep.Provider == id {
			return ep, true
		}
	}
	return ExposureEndpoint{}, false
}

// The Private kind serves a flat's current site, its previews and the
// Private route a Public flat keeps, each with the Private handler.
func TestProxyServesPrivateRoutes(t *testing.T) {
	f := newProxyFixture(t, Proxy, Portal)
	ctx := context.Background()
	priv, err := f.m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes", Visibility: "private",
		Audience: AudienceCurrent, Handler: marked("notes"), Permitted: []ID{Proxy}})
	if err != nil {
		t.Fatal(err)
	}
	ep, ok := endpointOf(priv, Proxy)
	if !ok || ep.URL != "https://notes.site.example.org" || ep.State != stateReady || ep.Detail != proxy.Detail || !ep.Configured || !ep.Permitted || !ep.Ready {
		t.Fatalf("proxy endpoint: %+v", ep)
	}
	if _, err := f.m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes-abcdefgh", Visibility: "private",
		Audience: AudienceDraft, Handler: marked("preview"), Ephemeral: true, Permitted: []ID{Proxy}}); err != nil {
		t.Fatalf("preview on proxy: %v", err)
	}
	if _, err := f.m.ServeExposure(ctx, ExposureRequest{Slug: "site", Host: "site", Visibility: "public",
		Audience: AudienceCurrent, Handler: marked("public"), PrivateHandler: marked("private"), Permitted: []ID{Proxy, Portal}}); err != nil {
		t.Fatal(err)
	}
	for label, want := range map[string]string{
		"notes":          "notes via proxy",
		"notes-abcdefgh": "preview via proxy",
		"site":           "private via proxy",
	} {
		if code, body := f.proxyGet(t, label); code != 200 || body != want {
			t.Errorf("%s: %d %q, want %q", label, code, body, want)
		}
	}
	// A flat that does not list the provider is not served there.
	if _, err := f.m.ServeExposure(ctx, ExposureRequest{Slug: "other", Host: "other", Visibility: "private",
		Audience: AudienceCurrent, Handler: marked("other")}); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.proxyGet(t, "other"); code != http.StatusNotFound {
		t.Errorf("unlisted flat: %d", code)
	}
	st, err := f.m.ExposureStatus(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if ep, ok := endpointOf(st, Proxy); !ok || !ep.Configured || ep.State != stateReady {
		t.Fatalf("status: %+v", st)
	}
	// A template change applies to reported addresses at once.
	f.setTemplate("http://{host}.lan.example:8080")
	st, _ = f.m.ExposureStatus(ctx, "notes")
	if ep, _ := endpointOf(st, Proxy); ep.URL != "http://notes.lan.example:8080" {
		t.Fatalf("after a template change: %+v", ep)
	}
}

// proxy-public serves only a Public flat's current site with the Public
// handler; drafts, previews and private requests are refused.
func TestProxyPublicServesPublicFlatsOnly(t *testing.T) {
	f := newProxyFixture(t, ProxyPublic)
	ctx := context.Background()
	res, err := f.m.ServeExposure(ctx, ExposureRequest{Slug: "site", Host: "site", Visibility: "public",
		Audience: AudienceCurrent, Handler: marked("public"), PrivateHandler: marked("private"), Permitted: []ID{ProxyPublic}})
	if err != nil {
		t.Fatal(err)
	}
	if ep, ok := endpointOf(res, ProxyPublic); !ok || ep.URL != "https://site.site.example.org" || !ep.Ready {
		t.Fatalf("proxy-public endpoint: %+v", res.Endpoints)
	}
	if code, body := f.proxyGet(t, "site"); code != 200 || body != "public via proxy-public" {
		t.Fatalf("public flat: %d %q", code, body)
	}
	for name, req := range map[string]ExposureRequest{
		"private flat": {Slug: "notes", Host: "notes", Visibility: "private", Audience: AudienceCurrent, Permitted: []ID{ProxyPublic}},
		"draft":        {Slug: "site", Host: "site-abcdefgh", Visibility: "private", Audience: AudienceDraft, Ephemeral: true, Permitted: []ID{ProxyPublic}},
		"public preview": {Slug: "site", Host: "site-bcdefghi", Visibility: "public", Audience: AudienceCurrent, Ephemeral: true,
			Permitted: []ID{ProxyPublic}},
	} {
		req.Handler = marked(name)
		if _, err := f.m.ServeExposure(ctx, req); !errors.Is(err, ErrProviderNotPermitted) {
			t.Errorf("%s: %v", name, err)
		}
		if code, _ := f.proxyGet(t, req.Host); code != http.StatusNotFound {
			t.Errorf("%s served: %d", name, code)
		}
	}
	stop, err := f.m.StopPublicRoutes(ctx, "site")
	if err != nil || !slices.Contains(stop.Stopped, ProxyPublic) || len(stop.Unconfirmed) != 0 {
		t.Fatalf("stop public: %+v %v", stop, err)
	}
	if code, _ := f.proxyGet(t, "site"); code != http.StatusNotFound {
		t.Fatalf("after the public stop: %d", code)
	}
	if active, _ := f.m.HasProviderRoute(ctx, "site", ProxyPublic); active {
		t.Fatal("public proxy route still registered")
	}
}

// Each stop removes the matching proxy routes, and the backend can only be
// detached once none is left.
func TestProxyStops(t *testing.T) {
	f := newProxyFixture(t, Proxy)
	ctx := context.Background()
	serve := func(slug, host string, ephemeral bool) {
		t.Helper()
		audience := AudienceCurrent
		if ephemeral {
			audience = AudienceDraft
		}
		if _, err := f.m.ServeExposure(ctx, ExposureRequest{Slug: slug, Host: host, Visibility: "private",
			Audience: audience, Handler: marked(host), Ephemeral: ephemeral, Permitted: []ID{Proxy}}); err != nil {
			t.Fatal(err)
		}
	}
	served := func(host string) bool {
		t.Helper()
		code, _ := f.proxyGet(t, host)
		return code == 200
	}
	serve("notes", "notes", false)
	serve("notes", "notes-abcdefgh", true)
	if err := f.m.StopExposure(ctx, "notes-abcdefgh"); err != nil || served("notes-abcdefgh") || !served("notes") {
		t.Fatalf("StopExposure: %v", err)
	}
	serve("notes", "notes-abcdefgh", true)
	if f.m.DetachProxy() {
		t.Fatal("detached under registered routes")
	}
	if err := f.m.StopProviderRoutes(ctx, "notes", Proxy); err != nil || served("notes") || served("notes-abcdefgh") {
		t.Fatalf("StopProviderRoutes: %v", err)
	}
	if active, _ := f.m.HasProviderRoute(ctx, "notes", Proxy); active {
		t.Fatal("proxy route still registered")
	}
	serve("blog", "blog", false)
	serve("blog", "blog-abcdefgh", true)
	if err := f.m.StopSlug(ctx, "blog"); err != nil || served("blog") || served("blog-abcdefgh") {
		t.Fatalf("StopSlug: %v", err)
	}
	if !f.m.DetachProxy() {
		t.Fatal("detach refused without routes")
	}
	res, err := f.m.ServeExposure(ctx, ExposureRequest{Slug: "notes", Host: "notes", Visibility: "private",
		Audience: AudienceCurrent, Handler: marked("notes"), Permitted: []ID{Proxy}})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("open without a backend: %v", err)
	}
	if ep, _ := endpointOf(res, Proxy); ep.Configured {
		t.Fatalf("detached backend reported configured: %+v", ep)
	}
}

// A backend whose Serve fails reports the error on the route, and nothing
// is registered.
func TestProxyServeErrorIsReported(t *testing.T) {
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	m, err := New(t.TempDir(), Options{Local: ln, Proxy: brokenProxy{}, Grants: &File{Permitted: []ID{Proxy}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.ServeExposure(context.Background(), ExposureRequest{Slug: "notes", Host: "notes", Visibility: "private",
		Audience: AudienceCurrent, Handler: marked("notes"), Permitted: []ID{Proxy}})
	if !errors.Is(err, core.ErrProviderNotReady) {
		t.Fatalf("err = %v", err)
	}
	if ep, _ := endpointOf(res, Proxy); ep.State != stateError || ep.Detail != "address already in use" || !ep.Configured {
		t.Fatalf("endpoint: %+v", ep)
	}
	if active, _ := m.HasProviderRoute(context.Background(), "notes", Proxy); active {
		t.Fatal("failed route registered")
	}
}

type brokenProxy struct{}

func (brokenProxy) Serve(context.Context, string, http.Handler) (string, error) {
	return "", errors.New("address already in use")
}
func (brokenProxy) Stop(string) error      { return nil }
func (brokenProxy) URL(string) string      { return "" }
func (brokenProxy) Status() core.NetStatus { return core.NetStatus{Kind: proxy.Kind} }
func (brokenProxy) Close() error           { return nil }

// The approval policy names the granted proxy kind and its template, and
// is unchanged for hosts without a proxy provider.
func TestProxyPolicyToken(t *testing.T) {
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ctx := context.Background()
	template := "https://{host}.site.example.org"
	token := func(opts Options, granted ...ID) string {
		t.Helper()
		opts.Local, opts.Grants = ln, &File{Permitted: granted}
		m, err := New(t.TempDir(), opts)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := m.ExposurePolicy(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	withProxy := Options{Proxy: brokenProxy{}, ProxyTemplate: func() string { return template }}
	if token(Options{}, Portal) != token(withProxy, Portal) {
		t.Fatal("a proxy template changed the token of a host without a proxy provider")
	}
	private := token(withProxy, Proxy)
	public := token(withProxy, ProxyPublic)
	if private == public {
		t.Fatal("the token does not name the proxy kind")
	}
	template = "https://{host}.other.example.org"
	if token(withProxy, Proxy) == private {
		t.Fatal("a template change kept the token")
	}
	template = "https://{host}.site.example.org"
	if token(withProxy, Proxy) != private {
		t.Fatal("the token is not stable")
	}
}
