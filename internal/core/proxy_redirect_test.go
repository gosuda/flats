package core_test

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/expose/proxy"
	"github.com/gosuda/flats/internal/store"
)

// readyPortal is the local.Public stand-in for Portal with every route
// reported ready under its slug, so a Public flat has two ready public
// routes: this one and proxy-public.
type readyPortal struct{ *local.Public }

func (p readyPortal) Status() core.NetStatus {
	st := p.Public.Status()
	for i := range st.Hosts {
		st.Hosts[i].Host = strings.TrimPrefix(st.Hosts[i].Host, "pub-")
		st.Hosts[i].State = "ready"
	}
	return st
}

// Rules 4 and 29 with proxy-public: a Public flat renamed while it allows
// proxy-public redirects its old slug on the proxy listener to its new
// proxy-public address, even though its public_url is on another provider
// (the host's public default), and Portal visitors stay on Portal. A Private
// flat that allows proxy-public is never served there.
func TestProxyPublicRenameRedirect(t *testing.T) {
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
	portal := readyPortal{local.NewPublic(pubLn)}
	var svc *core.Service
	n, err := proxy.Listen("127.0.0.1:0", func() config.Template { return svc.ProxyConfig().Template })
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := provider.New(filepath.Join(dir, "providers"), provider.Options{Local: loop, Portal: portal, Proxy: n,
		ProxyTemplate: func() string { return svc.ProxyConfig().Template.String() },
		Grants:        &provider.File{Permitted: []provider.ID{provider.Portal, provider.ProxyPublic}}})
	if err != nil {
		t.Fatal(err)
	}
	svc, err = core.New(ctx, core.Config{DataDir: dir, Store: st, Private: loop, Lifecycle: mgr,
		ConsoleURL: func() string { return "http://console.test" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		svc.Close()
		mgr.Close()
		n.Close()
		loop.Close()
		pubLn.Close()
		st.Close()
	})
	if _, err := svc.UpdateSettings(ctx, map[string]string{core.SetProxyTemplate: proxyTemplate}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetPermittedNetworks([]string{string(provider.Portal), string(provider.ProxyPublic)}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateSettings(ctx, map[string]string{core.SetDefaultPublic: string(provider.Portal)}); err != nil {
		t.Fatal(err)
	}
	// location sends a request for host to addr without following
	// redirects.
	location := func(addr, host, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
		req.Host = host
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode, res.Header.Get("Location")
	}
	site := func(slug string) {
		t.Helper()
		if _, err := svc.SaveVersion(ctx, slug, files("index.html", "<p>"+slug+"</p>"), core.SaveMeta{}, core.ViaMCP); err != nil {
			t.Fatal(err)
		}
		publish(t, svc, slug)
		if err := svc.SetProviderPermission(ctx, slug, store.ProviderProxyPublic, true, core.ViaConsole); err != nil {
			t.Fatal(err)
		}
	}

	site("old-name")
	permitPortal(t, svc, "old-name")
	setVisibility(t, svc, "old-name", store.Public)
	site("secret")

	f, err := svc.GetFlat(ctx, "old-name")
	if err != nil {
		t.Fatal(err)
	}
	// The host's public default decides public_url, so a redirect that
	// ignored the arrival provider would leave proxy-public.
	if f.PublicURL != portal.URL("old-name") {
		t.Fatalf("public_url %q, want the Portal address", f.PublicURL)
	}
	if code, _ := location(n.Addr(), "old-name.site.example.org", "/"); code != http.StatusOK {
		t.Fatalf("Public flat on proxy-public: %d", code)
	}
	if code, _ := location(n.Addr(), "secret.site.example.org", "/"); code != http.StatusNotFound {
		t.Fatalf("Private flat on proxy-public: %d", code)
	}

	if _, err := svc.RenameSlug(ctx, "old-name", "new-name", core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if code, loc := location(n.Addr(), "old-name.site.example.org", "/a?b=1"); code != http.StatusTemporaryRedirect || loc != "https://new-name.site.example.org/a?b=1" {
		t.Errorf("proxy-public redirect: %d %q", code, loc)
	}
	if code, _ := location(n.Addr(), "new-name.site.example.org", "/"); code != http.StatusOK {
		t.Errorf("renamed flat on proxy-public: %d", code)
	}
	old, err := url.Parse(portal.URL("old-name"))
	if err != nil {
		t.Fatal(err)
	}
	if code, loc := location("127.0.0.1:"+old.Port(), old.Host, "/a?b=1"); code != http.StatusTemporaryRedirect || loc != portal.URL("new-name")+"/a?b=1" {
		t.Errorf("Portal redirect: %d %q", code, loc)
	}
	if code, _ := location(n.Addr(), "secret.site.example.org", "/"); code != http.StatusNotFound {
		t.Errorf("Private flat on proxy-public after the rename: %d", code)
	}
}
