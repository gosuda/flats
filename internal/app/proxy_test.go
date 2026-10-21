package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/store"
)

const testProxyTemplate = "https://{host}.site.example.org"

// proxyConfig initializes a config-mode host directory and applies each
// key/value pair with offline `flats config set`, in order.
func proxyConfig(t *testing.T, kv ...string) string {
	t.Helper()
	path := initConfig(t, t.TempDir())
	setConfig(t, path, kv...)
	return path
}

func setConfig(t *testing.T, path string, kv ...string) {
	t.Helper()
	for i := 0; i < len(kv); i += 2 {
		if out, err := runConfig(t, "set", kv[i], kv[i+1], "--config", path); err != nil {
			t.Fatalf("config set %s: %v %s", kv[i], err, out)
		}
	}
}

// startConfigHost starts the host of config path without server flats.
func startConfigHost(t *testing.T, path string) (*Host, *http.Client) {
	t.Helper()
	h, err := Start(context.Background(), configModeOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	return h, &http.Client{Transport: transport}
}

// freeAddr returns a loopback address no listener holds right now.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// listening reports whether something accepts connections at addr.
func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func (h *Host) proxyBackend() any {
	h.pmu.Lock()
	defer h.pmu.Unlock()
	return h.proxyNet
}

// deploySite saves html as slug's Draft and makes it the live version,
// approving the version and its deployment.
func deploySite(t *testing.T, svc *core.Service, slug, html string) {
	t.Helper()
	deployBundle(t, svc, slug, []bundle.File{{Path: "index.html", Data: []byte(html)}})
}

// deployBundle is deploySite for any website bundle.
func deployBundle(t *testing.T, svc *core.Service, slug string, files []bundle.File) {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.SaveVersion(ctx, slug, files, core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Publish(ctx, slug, 0, "", core.ViaAPI)
	if err != nil || res.Approval == nil {
		t.Fatalf("create version: %v %+v", err, res)
	}
	if _, err := svc.Decide(ctx, res.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	f, err := svc.GetFlat(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Deploy(ctx, slug, f.LatestVersion, core.ViaAPI)
	var pending *core.PendingApproval
	if !errors.As(err, &pending) || pending.Approval == nil {
		t.Fatalf("deploy: %v", err)
	}
	if _, err := svc.Decide(ctx, pending.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
}

// Rules 1, 11 and 14: turning a proxy kind on binds the listener only when
// the change is valid, the other kind is refused while one is on, and
// turning it off is refused while a flat allows it and then frees the port.
func TestProxyProviderTurnsOnAndOff(t *testing.T) {
	h, client := startConfigHost(t, proxyConfig(t, "proxy.addr", "127.0.0.1:0"))
	ctx := context.Background()
	for id, scope := range map[string]string{"proxy": "private", "proxy-public": "public"} {
		if p := providerByID(t, h, client, id); p.Enabled || p.Configured || p.Scope != scope || p.Address != "http://127.0.0.1:0" || p.Template != "" {
			t.Fatalf("%s before: %+v", id, p)
		}
	}
	if code, out := setHost(t, h, client, "proxy", true); code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "proxy.url_template") {
		t.Fatalf("turned on without a template: %d %v", code, out)
	}
	if got := configPermitted(t, h); len(got) != 0 || h.proxyBackend() != nil {
		t.Fatalf("a refused turn-on left %v and %v", got, h.proxyBackend())
	}
	if code := consoleCall(t, h, client, "PUT", "/console/api/settings", strings.NewReader(`{"proxy_url_template":"`+testProxyTemplate+`"}`), nil); code != 200 {
		t.Fatalf("set the template: %d", code)
	}
	if code, out := setHost(t, h, client, "proxy", true); code != 200 {
		t.Fatalf("turn on: %d %v", code, out)
	}
	p := providerByID(t, h, client, "proxy")
	addr := strings.TrimPrefix(p.Address, "http://")
	if !p.Enabled || !p.Configured || p.Template != testProxyTemplate || addr == "127.0.0.1:0" || p.BindError != "" || !listening(addr) {
		t.Fatalf("after turning on: %+v", p)
	}
	if got := configPermitted(t, h); !slices.Equal(got, []string{"proxy"}) {
		t.Fatalf("network.permitted %v", got)
	}
	if p := providerByID(t, h, client, "proxy-public"); p.Configured {
		t.Fatalf("the other kind reports the listener: %+v", p)
	}
	// One kind at a time, and the template stays while one is on.
	if code, _ := setHost(t, h, client, "proxy-public", true); code != http.StatusBadRequest {
		t.Fatalf("second kind: %d", code)
	}
	if code := consoleCall(t, h, client, "PUT", "/console/api/settings", strings.NewReader(`{"proxy_url_template":""}`), nil); code != http.StatusBadRequest {
		t.Fatalf("template cleared while on: %d", code)
	}
	if got := configPermitted(t, h); !slices.Equal(got, []string{"proxy"}) {
		t.Fatalf("network.permitted %v", got)
	}

	if _, err := h.Svc.CreateFlat(ctx, "site", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := h.Svc.SetProviderPermission(ctx, "site", "proxy", true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if code, out := setHost(t, h, client, "proxy", false); code != http.StatusConflict || out["category"] != "provider_in_use" || !listening(addr) {
		t.Fatalf("turned off while allowed: %d %v", code, out)
	}
	if err := h.Svc.SetProviderPermission(ctx, "site", "proxy", false, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if code, out := setHost(t, h, client, "proxy", false); code != 200 {
		t.Fatalf("turn off: %d %v", code, out)
	}
	if listening(addr) || h.proxyBackend() != nil || len(configPermitted(t, h)) != 0 {
		t.Fatal("the listener outlived turning the provider off")
	}
	// The other kind can be turned on once the first is off.
	if code, out := setHost(t, h, client, "proxy-public", true); code != 200 {
		t.Fatalf("turn on proxy-public: %d %v", code, out)
	}
	if p := providerByID(t, h, client, "proxy-public"); !p.Configured || !listening(strings.TrimPrefix(p.Address, "http://")) {
		t.Fatalf("proxy-public: %+v", p)
	}
}

// Rule 14: a taken address or a failed save refuses turning the provider on,
// saves nothing and leaves no listener bound.
func TestProxyTurnOnRefusalsLeaveNoListener(t *testing.T) {
	t.Run("address taken", func(t *testing.T) {
		taken, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer taken.Close()
		h, client := startConfigHost(t, proxyConfig(t, "proxy.url_template", testProxyTemplate, "proxy.addr", taken.Addr().String()))
		if code, out := setHost(t, h, client, "proxy", true); code != http.StatusConflict || !strings.Contains(out["error"].(string), taken.Addr().String()) {
			t.Fatalf("turned on over a taken address: %d %v", code, out)
		}
		if len(configPermitted(t, h)) != 0 || h.proxyBackend() != nil || h.Providers.HostAllows("proxy") {
			t.Fatal("a refused turn-on changed the host")
		}
	})
	t.Run("failed save", func(t *testing.T) {
		addr := freeAddr(t)
		h, client := startConfigHost(t, proxyConfig(t, "proxy.url_template", testProxyTemplate, "proxy.addr", addr))
		base := "http://" + h.Addr()
		req, _ := http.NewRequest("PUT", base+"/console/api/providers/proxy", strings.NewReader(`{"enabled":true}`))
		req.Header.Set("X-Flats-Console", "1")
		req.Header.Set("Origin", base)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", `"an older etag"`)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode < 400 {
			t.Fatalf("saved against a stale ETag: %d", res.StatusCode)
		}
		if len(configPermitted(t, h)) != 0 || h.proxyBackend() != nil || h.Providers.HostAllows("proxy") {
			t.Fatal("a failed save changed the host")
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("the port stayed bound: %v", err)
		}
		ln.Close()
	})
}

// Rule 12: a changed proxy.addr applies the next time the listener binds.
func TestProxyAddrChangeAppliesOnNextTurnOn(t *testing.T) {
	first, second := freeAddr(t), freeAddr(t)
	path := proxyConfig(t, "proxy.url_template", testProxyTemplate, "proxy.addr", first)
	h, client := startConfigHost(t, path)
	if code, out := setHost(t, h, client, "proxy", true); code != 200 {
		t.Fatalf("turn on: %d %v", code, out)
	}
	if p := providerByID(t, h, client, "proxy"); p.Address != "http://"+first || !listening(first) {
		t.Fatalf("first address: %+v", p)
	}
	if code, out := setHost(t, h, client, "proxy", false); code != 200 {
		t.Fatalf("turn off: %d %v", code, out)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	setConfig(t, path, "proxy.addr", second)
	h, client = startConfigHost(t, path)
	if code, out := setHost(t, h, client, "proxy", true); code != 200 {
		t.Fatalf("turn on again: %d %v", code, out)
	}
	if p := providerByID(t, h, client, "proxy"); p.Address != "http://"+second || !listening(second) || listening(first) {
		t.Fatalf("second address: %+v", p)
	}
}

// Rule 15: a host whose proxy address is taken still starts, and every
// proxy route reports the bind error; a restart binds it again.
func TestProxyStartWithTakenAddress(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := taken.Addr().String()
	path := proxyConfig(t, "proxy.addr", addr, "proxy.url_template", testProxyTemplate, "network.permitted", "proxy")
	h, client := startConfigHost(t, path)
	p := providerByID(t, h, client, "proxy")
	if !p.Enabled || !p.Configured || !strings.Contains(p.BindError, addr) || p.Status == nil || p.Status.Detail == "" {
		t.Fatalf("provider: %+v", p)
	}
	ctx := context.Background()
	deploySite(t, h.Svc, "site", "<p>site</p>")
	if err := h.Svc.SetProviderPermission(ctx, "site", "proxy", true, core.ViaConsole); !errors.Is(err, core.ErrProviderNotReady) {
		t.Fatalf("grant: %v", err)
	}
	// failedRoute checks the flat view the console draws the error row from:
	// the flat allows proxy, its one proxy endpoint is the current route of
	// its slug in error with the bind error and no address, and private_url
	// falls back to Local.
	failedRoute := func(h *Host) {
		t.Helper()
		f, err := h.Svc.GetFlat(ctx, "site")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(f.Providers, string(core.ProviderProxy)) {
			t.Fatalf("the flat does not allow proxy: %v", f.Providers)
		}
		var proxied []core.ExposureEndpoint
		for _, ep := range f.Endpoints {
			if ep.Provider == core.ProviderProxy {
				proxied = append(proxied, ep)
			}
		}
		if len(proxied) != 1 {
			t.Fatalf("proxy endpoints: %+v", f.Endpoints)
		}
		ep := proxied[0]
		if ep.Audience != core.AudienceCurrent || ep.Host != "site" || ep.State != "error" || ep.Ready || ep.URL != "" ||
			!strings.Contains(ep.Detail, "cannot bind proxy.addr "+addr) {
			t.Fatalf("failed proxy endpoint: %+v", ep)
		}
		if !strings.Contains(f.PrivateURL, "site.localhost") || f.PrivateState != "ready" {
			t.Fatalf("private_url %q (%s)", f.PrivateURL, f.PrivateState)
		}
	}
	failedRoute(h)
	// A restart with the address free serves the granted flat.
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	taken.Close()
	h, client = startConfigHost(t, path)
	if p := providerByID(t, h, client, "proxy"); p.BindError != "" || !listening(addr) {
		t.Fatalf("after the restart: %+v", p)
	}
	req, _ := http.NewRequest("GET", "http://"+addr+"/", nil)
	req.Host = "site.site.example.org"
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("restarted proxy route: %d", res.StatusCode)
	}
	// A restart with the address taken again: the granted flat's route
	// reports the bind error the same way.
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if taken, err = net.Listen("tcp", addr); err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	h, client = startConfigHost(t, path)
	if p := providerByID(t, h, client, "proxy"); !strings.Contains(p.BindError, addr) {
		t.Fatalf("after the second restart: %+v", p)
	}
	failedRoute(h)
}

// Rule 12: a non-loopback proxy.addr logs a warning at start, here on an
// address this machine cannot bind, so nothing is exposed.
func TestProxyNonLoopbackWarning(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	path := proxyConfig(t, "proxy.addr", "192.0.2.1:0", "proxy.url_template", testProxyTemplate, "network.permitted", "proxy")
	h, client := startConfigHost(t, path)
	if !strings.Contains(buf.String(), "proxy.addr 192.0.2.1:0, which is not a loopback address") {
		t.Fatalf("no warning in:\n%s", buf.String())
	}
	if p := providerByID(t, h, client, "proxy"); p.BindError == "" {
		t.Fatalf("provider: %+v", p)
	}
}

// Rule 13: a template or kind change makes a pending approval stale; a
// proxy.addr change does not.
func TestProxyApprovalPolicy(t *testing.T) {
	ctx := context.Background()
	path := proxyConfig(t, "proxy.addr", freeAddr(t), "proxy.url_template", testProxyTemplate, "network.permitted", "proxy")
	h, _ := startConfigHost(t, path)
	request := func(svc *core.Service, slug string) string {
		t.Helper()
		if _, err := svc.SaveVersion(ctx, slug, []bundle.File{{Path: "index.html", Data: []byte("<p>" + slug + "</p>")}}, core.SaveMeta{}, core.ViaMCP); err != nil {
			t.Fatal(err)
		}
		res, err := svc.Publish(ctx, slug, 0, "", core.ViaAPI)
		if err != nil || res.Approval == nil {
			t.Fatalf("create version: %v %+v", err, res)
		}
		return res.Approval.ID
	}
	stale := request(h.Svc, "template")
	if _, err := h.Svc.UpdateSettings(ctx, map[string]string{core.SetProxyTemplate: "https://{host}.other.example.org"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Svc.Decide(ctx, stale, true); !errors.Is(err, core.ErrStaleApproval) ||
		!strings.Contains(err.Error(), "provider permissions or network configuration changed after the request") {
		t.Fatalf("approval after a template change: %v", err)
	}
	kept := request(h.Svc, "addr")
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	setConfig(t, path, "proxy.addr", freeAddr(t))
	h, client := startConfigHost(t, path)
	if _, err := h.Svc.Decide(ctx, kept, true); err != nil {
		t.Fatalf("approval after a proxy.addr change: %v", err)
	}
	kind := request(h.Svc, "kind")
	if code, out := setHost(t, h, client, "proxy", false); code != 200 {
		t.Fatalf("turn off: %d %v", code, out)
	}
	if code, out := setHost(t, h, client, "proxy-public", true); code != 200 {
		t.Fatalf("turn on proxy-public: %d %v", code, out)
	}
	if _, err := h.Svc.Decide(ctx, kind, true); !errors.Is(err, core.ErrStaleApproval) {
		t.Fatalf("approval after a kind change: %v", err)
	}
}

// Rule 8: only the console sets the template; agents and the API outside
// console routes cannot.
func TestProxyTemplateOnlyConsoleSettings(t *testing.T) {
	h, client := consoleHost(t)
	for _, path := range []string{"/api/settings", "/console/api/settings"} {
		r, _ := http.NewRequest("PUT", "http://"+h.Addr()+path, strings.NewReader(`{"proxy_url_template":"https://{host}.evil.example.org"}`))
		r.Header.Set("X-Flats-Client", "cli")
		r.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("agent settings accepted at %s", path)
		}
		r, _ = http.NewRequest("PUT", "http://"+h.Addr()+strings.Replace(path, "settings", "providers/proxy", 1), strings.NewReader(`{"enabled":true}`))
		r.Header.Set("X-Flats-Client", "cli")
		r.Header.Set("Content-Type", "application/json")
		if resp, err = client.Do(r); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("agent turned a proxy provider on at %s", path)
		}
	}
	if h.Svc.ProxyConfig().Template.String() != "" || h.Providers.HostAllows("proxy") {
		t.Fatal("an agent changed the proxy configuration")
	}
}

// Rule 16: switching from proxy-public to proxy on a host with a Public flat
// succeeds only after the flat is Private, its grant is removed and
// proxy-public is off.
func TestProxyKindSwitchWithPublicFlat(t *testing.T) {
	ctx := context.Background()
	h, client := startConfigHost(t, proxyConfig(t, "proxy.addr", "127.0.0.1:0", "proxy.url_template", testProxyTemplate, "network.permitted", "proxy-public"))
	deploySite(t, h.Svc, "site", "<p>site</p>")
	if err := h.Svc.SetProviderPermission(ctx, "site", "proxy-public", true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	visibility := func(vis store.Visibility) {
		t.Helper()
		r, err := h.Svc.SetVisibility(ctx, "site", vis, core.ViaConsole, "")
		if err != nil || r.Approval == nil {
			t.Fatalf("visibility %s: %v %+v", vis, err, r)
		}
		if _, err := h.Svc.Decide(ctx, r.Approval.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	visibility(store.Public)
	if code, _ := setHost(t, h, client, "proxy", true); code != http.StatusBadRequest {
		t.Fatalf("proxy turned on beside proxy-public: %d", code)
	}
	if code, _ := setHost(t, h, client, "proxy-public", false); code != http.StatusConflict {
		t.Fatalf("proxy-public turned off while allowed: %d", code)
	}
	if err := h.Svc.SetProviderPermission(ctx, "site", "proxy-public", false, core.ViaConsole); !errors.Is(err, core.ErrProviderInUse) {
		t.Fatalf("grant removed from a Public flat: %v", err)
	}
	visibility(store.Private)
	if err := h.Svc.SetProviderPermission(ctx, "site", "proxy-public", false, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if code, out := setHost(t, h, client, "proxy-public", false); code != 200 {
		t.Fatalf("turn off proxy-public: %d %v", code, out)
	}
	if code, out := setHost(t, h, client, "proxy", true); code != 200 {
		t.Fatalf("turn on proxy: %d %v", code, out)
	}
	if err := h.Svc.SetProviderPermission(ctx, "site", "proxy", true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if f, _ := h.Svc.GetFlat(ctx, "site"); f.PrivateURL != "https://site.site.example.org" {
		t.Fatalf("private_url %q", f.PrivateURL)
	}
}

// failGrants makes the host's provider grant changes fail while fail returns
// an error.
func failGrants(h *Host, fail func(id string, permitted bool) error) {
	h.pmu.Lock()
	defer h.pmu.Unlock()
	h.testGrantFailure = nil
	if fail != nil {
		h.testGrantFailure = func(id provider.ID, permitted bool) error { return fail(string(id), permitted) }
	}
}

// Rule 14: a change refused after the settings save, by a failed grant or a
// proxy route left registered, saves nothing, keeps the grant as it was and
// binds or closes nothing.
func TestProxyRefusedChangeKeepsState(t *testing.T) {
	ctx := context.Background()
	addr := freeAddr(t)
	h, client := startConfigHost(t, proxyConfig(t, "proxy.url_template", testProxyTemplate, "proxy.addr", addr))
	refused := errors.New("grant refused")
	state := func(name string, on bool) {
		t.Helper()
		if got := configPermitted(t, h); slices.Contains(got, "proxy") != on {
			t.Errorf("%s: network.permitted %v", name, got)
		}
		if h.Providers.HostAllows("proxy") != on {
			t.Errorf("%s: grant %t", name, !on)
		}
		if (h.proxyBackend() != nil) != on || listening(addr) != on {
			t.Errorf("%s: listener %v, listening %t", name, h.proxyBackend(), listening(addr))
		}
	}

	failGrants(h, func(id string, permitted bool) error { return refused })
	if code, out := setHost(t, h, client, "proxy", true); code < 400 || !strings.Contains(out["error"].(string), "grant refused") {
		t.Fatalf("turn on with a failing grant: %d %v", code, out)
	}
	state("failed grant on", false)

	failGrants(h, nil)
	if code, out := setHost(t, h, client, "proxy", true); code != 200 {
		t.Fatalf("turn on: %d %v", code, out)
	}
	failGrants(h, func(id string, permitted bool) error { return refused })
	if code, _ := setHost(t, h, client, "proxy", false); code < 400 {
		t.Fatalf("turn off with a failing grant: %d", code)
	}
	state("failed grant off", true)

	// A proxy route no flat allows: turning off is refused and the route
	// keeps serving.
	failGrants(h, nil)
	if _, err := h.Providers.ServeExposure(ctx, core.ExposureRequest{Slug: "stray", Host: "stray", Visibility: "private", Audience: core.AudienceCurrent,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("stray")) }), Permitted: []core.ProviderID{core.ProviderProxy}}); err != nil {
		t.Fatal(err)
	}
	if code, out := setHost(t, h, client, "proxy", false); code < 400 || !strings.Contains(out["error"].(string), "route is still registered") {
		t.Fatalf("turn off with a registered route: %d %v", code, out)
	}
	state("registered route", true)
	req, _ := http.NewRequest("GET", "http://"+addr+"/", nil)
	req.Host = "stray.site.example.org"
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("stray route after the refusal: %d", res.StatusCode)
	}
	if err := h.Providers.StopSlug(ctx, "stray"); err != nil {
		t.Fatal(err)
	}
	if code, out := setHost(t, h, client, "proxy", false); code != 200 {
		t.Fatalf("turn off: %d %v", code, out)
	}
	state("off", false)
}

// Rule 11 and the address checks: a host that permits no proxy and uses
// proxy.addr for its Local or console listener starts without a proxy
// listener, and turning a proxy kind on there is refused, saves nothing and
// binds nothing.
func TestProxyAddrSharedWithoutProxy(t *testing.T) {
	for _, key := range []string{"host.local_addr", "host.console_addr"} {
		t.Run(key, func(t *testing.T) {
			addr := freeAddr(t)
			path := proxyConfig(t, "proxy.url_template", testProxyTemplate, key, addr)
			o := configModeOptions(path)
			delete(o.Overrides, key) // keep the stored address that equals proxy.addr
			setConfig(t, path, "proxy.addr", addr)
			h, err := Start(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := h.Close(); err != nil {
					t.Error(err)
				}
			})
			transport := http.DefaultTransport.(*http.Transport).Clone()
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport}
			if key == "host.console_addr" && h.Addr() != addr {
				t.Fatalf("console on %s, want %s", h.Addr(), addr)
			}
			if !listening(addr) || h.proxyBackend() != nil {
				t.Fatalf("listeners: %s listening %t, proxy %v", addr, listening(addr), h.proxyBackend())
			}
			if p := providerByID(t, h, client, "proxy"); p.Enabled || p.Configured || p.BindError != "" {
				t.Fatalf("provider: %+v", p)
			}
			for _, id := range []string{"proxy", "proxy-public"} {
				if code, out := setHost(t, h, client, id, true); code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["error"]), "proxy.addr") {
					t.Fatalf("turn on %s: %d %v", id, code, out)
				}
				if len(configPermitted(t, h)) != 0 || h.proxyBackend() != nil || h.Providers.HostAllows(provider.ID(id)) {
					t.Fatalf("a refused %s changed the host", id)
				}
			}
		})
	}
}

// Rules 10 and 11 through offline `flats config set`: each refused change
// exits with the configuration error, names the key and leaves config.json
// as it was.
func TestProxyOfflineConfigChecks(t *testing.T) {
	addr := freeAddr(t)
	for _, c := range []struct {
		name  string
		setup []string // applied first; each must succeed
		set   [2]string
		want  string
	}{
		{"kind without a template", nil, [2]string{"network.permitted", "proxy"}, "proxy.url_template"},
		{"both kinds", []string{"proxy.url_template", testProxyTemplate, "network.permitted", "proxy"}, [2]string{"network.permitted", "proxy,proxy-public"}, "network.permitted"},
		{"template cleared while on", []string{"proxy.url_template", testProxyTemplate, "network.permitted", "proxy-public"}, [2]string{"proxy.url_template", ""}, "proxy.url_template"},
		{"local moved onto the address", []string{"proxy.url_template", testProxyTemplate, "proxy.addr", addr, "network.permitted", "proxy"}, [2]string{"host.local_addr", addr}, "proxy.addr"},
		{"console moved onto the address", []string{"proxy.url_template", testProxyTemplate, "proxy.addr", addr, "network.permitted", "proxy"}, [2]string{"host.console_addr", addr}, "proxy.addr"},
		{"turned on over the Local address", []string{"proxy.url_template", testProxyTemplate, "host.local_addr", addr, "proxy.addr", addr}, [2]string{"network.permitted", "proxy"}, "proxy.addr"},
		{"template over a console origin", []string{"console.external_origins", "https://flats.site.example.org"}, [2]string{"proxy.url_template", testProxyTemplate}, "console.external_origins"},
		{"console origin over the template", []string{"proxy.url_template", testProxyTemplate}, [2]string{"console.external_origins", "https://flats.site.example.org"}, "console.external_origins"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := proxyConfig(t, c.setup...)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runConfig(t, "set", c.set[0], c.set[1], "--config", path); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("set %s %q: %v", c.set[0], c.set[1], err)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
				t.Fatalf("a refused set changed config.json:\n%s", after)
			}
		})
	}
}
