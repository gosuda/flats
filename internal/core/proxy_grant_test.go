package core_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/expose/proxy"
	"github.com/gosuda/flats/internal/store"
)

const proxyTemplate = "https://{host}.site.example.org"

// stoppableProxy is a real proxy listener whose Stop can be made to fail.
type stoppableProxy struct {
	*proxy.Net
	failStop atomic.Bool
}

func (p *stoppableProxy) Stop(host string) error {
	if p.failStop.Load() {
		return errors.New("stop refused")
	}
	return p.Net.Stop(host)
}

type proxyHost struct {
	svc *core.Service
	st  *store.Store
	net *stoppableProxy
}

// newProxyHost starts core with a provider manager that grants kind and
// serves it on a real proxy listener.
func newProxyHost(t *testing.T, kind provider.ID) *proxyHost {
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
	h := &proxyHost{st: st}
	n, err := proxy.Listen("127.0.0.1:0", func() config.Template { return h.svc.ProxyConfig().Template })
	if err != nil {
		t.Fatal(err)
	}
	h.net = &stoppableProxy{Net: n}
	mgr, err := provider.New(filepath.Join(dir, "providers"), provider.Options{Local: loop, Proxy: h.net,
		ProxyTemplate: func() string { return h.svc.ProxyConfig().Template.String() },
		Grants:        &provider.File{Permitted: []provider.ID{kind}}})
	if err != nil {
		t.Fatal(err)
	}
	h.svc, err = core.New(ctx, core.Config{DataDir: dir, Store: st, Private: loop, Lifecycle: mgr,
		ConsoleURL: func() string { return "http://console.test" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.svc.Close(); mgr.Close(); n.Close(); loop.Close(); st.Close() })
	if _, err := h.svc.UpdateSettings(ctx, map[string]string{core.SetProxyTemplate: proxyTemplate}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetPermittedNetworks([]string{string(kind)}, ""); err != nil {
		t.Fatal(err)
	}
	return h
}

// fetch sends a request for label to the proxy listener as the reverse
// proxy would and returns the status and body.
func (h *proxyHost) fetch(t *testing.T, label string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+h.net.Addr()+"/", nil)
	req.Host = label + ".site.example.org"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func (h *proxyHost) permitted(t *testing.T, slug, id string) bool {
	t.Helper()
	ok, err := h.st.ProviderPermitted(context.Background(), slug, id)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// Rule 22: allowing proxy serves a running flat at once, including its
// previews; revoking it stops those routes before the revocation is saved.
func TestProxyGrantAndRevoke(t *testing.T) {
	ctx := context.Background()
	h := newProxyHost(t, provider.Proxy)
	if _, err := h.svc.SaveVersion(ctx, "notes", files("index.html", "<p>notes</p>"), core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	publish(t, h.svc, "notes")
	if code, _ := h.fetch(t, "notes"); code != http.StatusNotFound {
		t.Fatalf("served before the grant: %d", code)
	}
	if err := h.svc.SetProviderPermission(ctx, "notes", store.ProviderProxy, true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if code, body := h.fetch(t, "notes"); code != 200 || !strings.Contains(body, "notes") {
		t.Fatalf("after the grant: %d %.200s", code, body)
	}
	f, err := h.svc.GetFlat(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if f.PrivateURL != "https://notes.site.example.org" || f.PrivateDetail != proxy.Detail {
		t.Fatalf("private_url %q (%s)", f.PrivateURL, f.PrivateDetail)
	}
	kinds := map[core.ProviderID]string{}
	for _, ep := range f.Endpoints {
		if ep.Host == "notes" {
			kinds[ep.Provider] = ep.URL
		}
	}
	if kinds[core.ProviderProxy] != "https://notes.site.example.org" || !strings.Contains(kinds[core.ProviderLocal], "notes.localhost") {
		t.Fatalf("endpoints: %+v", f.Endpoints)
	}
	p, err := h.svc.OpenPreview(ctx, "notes", 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != "https://"+p.Host+".site.example.org" {
		t.Fatalf("preview URL %q", p.URL)
	}
	if code, _ := h.fetch(t, p.Host); code != 200 {
		t.Fatalf("preview through the proxy: %d", code)
	}

	// A route that cannot be confirmed stopped keeps the grant.
	h.net.failStop.Store(true)
	if err := h.svc.SetProviderPermission(ctx, "notes", store.ProviderProxy, false, core.ViaConsole); !errors.Is(err, core.ErrProviderInUse) {
		t.Fatalf("revoke with a failing stop: %v", err)
	}
	if !h.permitted(t, "notes", store.ProviderProxy) {
		t.Fatal("revocation saved before its routes stopped")
	}
	h.net.failStop.Store(false)
	if err := h.svc.SetProviderPermission(ctx, "notes", store.ProviderProxy, false, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if h.permitted(t, "notes", store.ProviderProxy) {
		t.Fatal("revocation not saved")
	}
	for _, label := range []string{"notes", p.Host} {
		if code, _ := h.fetch(t, label); code != http.StatusNotFound {
			t.Errorf("%s still served after the revocation: %d", label, code)
		}
	}
}

// Rule 22: proxy-public serves a granted Public flat, and its grant cannot
// be revoked until the flat is Private again.
func TestProxyPublicRevokeNeedsPrivate(t *testing.T) {
	ctx := context.Background()
	h := newProxyHost(t, provider.ProxyPublic)
	if _, err := h.svc.SaveVersion(ctx, "site", files("index.html", "<p>site</p>"), core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	publish(t, h.svc, "site")
	if err := h.svc.SetProviderPermission(ctx, "site", store.ProviderProxyPublic, true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.fetch(t, "site"); code != http.StatusNotFound {
		t.Fatalf("Private flat served on proxy-public: %d", code)
	}
	setVisibility(t, h.svc, "site", store.Public)
	if code, body := h.fetch(t, "site"); code != 200 || !strings.Contains(body, "site") {
		t.Fatalf("Public flat: %d %.200s", code, body)
	}
	f, err := h.svc.GetFlat(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	if f.PublicURL != "https://site.site.example.org" || f.ConnectionState != "ready" {
		t.Fatalf("public_url %q (%s)", f.PublicURL, f.ConnectionState)
	}
	if err := h.svc.SetProviderPermission(ctx, "site", store.ProviderProxyPublic, false, core.ViaConsole); !errors.Is(err, core.ErrProviderInUse) {
		t.Fatalf("revoked on a Public flat: %v", err)
	}
	if !h.permitted(t, "site", store.ProviderProxyPublic) {
		t.Fatal("grant removed from a Public flat")
	}
	setVisibility(t, h.svc, "site", store.Private)
	if code, _ := h.fetch(t, "site"); code != http.StatusNotFound {
		t.Fatalf("Private flat still served on proxy-public: %d", code)
	}
	if err := h.svc.SetProviderPermission(ctx, "site", store.ProviderProxyPublic, false, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
}

func setVisibility(t *testing.T, svc *core.Service, slug string, vis store.Visibility) {
	t.Helper()
	r, err := svc.SetVisibility(context.Background(), slug, vis, core.ViaConsole, "")
	if err != nil || r.Approval == nil {
		t.Fatalf("visibility %s: %v %+v", vis, err, r)
	}
	if _, err := svc.Decide(context.Background(), r.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
}

// Rule 23: a new flat inherits a proxy default with its provider event;
// existing flats keep their providers.
func TestProxyDefaultsForNewFlats(t *testing.T) {
	for _, c := range []struct {
		kind    provider.ID
		setting string
	}{
		{provider.Proxy, core.SetDefaultPrivate},
		{provider.ProxyPublic, core.SetDefaultPublic},
	} {
		t.Run(string(c.kind), func(t *testing.T) {
			ctx := context.Background()
			h := newProxyHost(t, c.kind)
			if _, err := h.svc.CreateFlat(ctx, "before", "", core.ViaAPI); err != nil {
				t.Fatal(err)
			}
			if _, err := h.svc.UpdateSettings(ctx, map[string]string{c.setting: string(c.kind)}); err != nil {
				t.Fatal(err)
			}
			if _, err := h.svc.CreateFlat(ctx, "after", "", core.ViaAPI); err != nil {
				t.Fatal(err)
			}
			if h.permitted(t, "before", string(c.kind)) {
				t.Fatal("an existing flat gained the default")
			}
			if !h.permitted(t, "after", string(c.kind)) {
				t.Fatal("a new flat did not get the default")
			}
			events, err := h.st.ListEvents(ctx, "after", "provider", 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(events, func(e store.Event) bool {
				return strings.Contains(e.Message, string(c.kind)+" permitted=true by default")
			}) {
				t.Fatalf("provider events: %+v", events)
			}
			if private, public := h.svc.DefaultProviders(); private != string(c.kind) && public != string(c.kind) {
				t.Fatalf("DefaultProviders = %q, %q", private, public)
			}
		})
	}
}

// The proxy listener reads the template on every request, URL and policy
// call; none of them waits for a settings save in progress.
func TestProxyTemplateReadsDoNotWaitForSaves(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	doc, err := config.New(config.NewInstanceID(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set(config.ProxyTemplateKey, proxyTemplate); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	src, err := core.NewSettingsSource(doc, &core.SettingsFile{Mode: "config", Hash: "h0", Save: func(_ string, apply func(*config.Document) error) (string, error) {
		close(entered)
		<-release
		return "h1", apply(doc.Clone())
	}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc, err := core.New(ctx, core.Config{DataDir: dir, Store: st, Settings: src, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	template := func() config.Template { return src.Proxy().Template }
	n, err := proxy.Listen("127.0.0.1:0", template)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if _, err := n.Serve(ctx, "blog", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("blog")) })); err != nil {
		t.Fatal(err)
	}
	loop, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer loop.Close()
	mgr, err := provider.New(filepath.Join(dir, "providers"), provider.Options{Local: loop, Proxy: n,
		ProxyTemplate: func() string { return template().String() }, Grants: &provider.File{Permitted: []provider.ID{provider.Proxy}}})
	if err != nil {
		t.Fatal(err)
	}

	saved := make(chan error, 1)
	go func() {
		_, err := svc.UpdateSettings(ctx, map[string]string{core.SetUploadMaxBytes: "4096"})
		saved <- err
	}()
	<-entered
	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, "http://"+n.Addr()+"/", nil)
		req.Host = "blog.site.example.org"
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
			if res.StatusCode != 200 {
				err = errors.New(res.Status)
			}
		}
		if err == nil && n.URL("blog") != "https://blog.site.example.org" {
			err = errors.New("URL " + n.URL("blog"))
		}
		if err == nil {
			_, err = mgr.ExposurePolicy(ctx)
		}
		done <- err
	}()
	waited := false
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(10 * time.Second):
		waited = true
		t.Error("reading the template waited for the settings save")
	}
	close(release)
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	if waited {
		<-done
	}
}
