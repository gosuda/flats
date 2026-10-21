package core_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/store"
)

// handlerTail is a tailnet without nodes: it keeps each host's handler so a
// test can send it a request.
type handlerTail struct {
	mu       sync.Mutex
	handlers map[string]http.Handler
}

func (n *handlerTail) Serve(_ context.Context, host string, h http.Handler, _ bool) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.handlers[host] = h
	return n.URL(host), nil
}

func (n *handlerTail) Stop(host string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.handlers, host)
	return nil
}

func (n *handlerTail) URL(host string) string { return "https://" + host + ".tail.test" }

func (n *handlerTail) Status() core.NetStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	st := core.NetStatus{Kind: "tailscale", Enabled: true}
	for host := range n.handlers {
		st.Hosts = append(st.Hosts, core.HostInfo{Host: host, URL: n.URL(host), State: "ready"})
	}
	return st
}

func (n *handlerTail) Close() error { return nil }

func (n *handlerTail) ServeFunnel(context.Context, string, http.Handler) (string, error) {
	return "", core.ErrProviderNotPermitted
}

func (n *handlerTail) StopFunnel(string) error { return nil }

func (n *handlerTail) FunnelState(string) core.ExposureEndpoint {
	return core.ExposureEndpoint{Provider: core.ProviderFunnel, State: "unavailable"}
}

func (n *handlerTail) location(t *testing.T, host, path string) string {
	t.Helper()
	n.mu.Lock()
	h := n.handlers[host]
	n.mu.Unlock()
	if h == nil {
		t.Fatalf("%s is not served on the tailnet", host)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("tailnet %s%s: %d", host, path, rec.Code)
	}
	return rec.Header().Get("Location")
}

// A rename redirect keeps the visitor on the network the request arrived
// on: an old loopback link leads to the new loopback address even though
// the flat reports its tailnet address, and the tailnet and public routes
// lead to their own new addresses.
func TestRenameRedirectStaysOnArrivalNetwork(t *testing.T) {
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
	pub := local.NewPublic(pubLn)
	tail := &handlerTail{handlers: map[string]http.Handler{}}
	mgr, err := provider.New(filepath.Join(dir, "providers"), provider.Options{Local: loop, Tailscale: tail, Portal: pub,
		Grants: &provider.File{Permitted: []provider.ID{provider.Tailscale, provider.Portal}}})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := core.New(ctx, core.Config{DataDir: dir, Store: st, Private: loop, Lifecycle: mgr,
		ConsoleURL: func() string { return "http://console.test" }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close(); mgr.Close(); loop.Close(); pubLn.Close(); st.Close() })

	if _, err := svc.SaveVersion(ctx, "old-name", files("index.html", "page"), core.SaveMeta{}, core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	publish(t, svc, "old-name")
	if err := svc.SetProviderPermission(ctx, "old-name", store.ProviderTailscale, true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	permitPortal(t, svc, "old-name")
	r, err := svc.SetVisibility(ctx, "old-name", store.Public, core.ViaConsole, "")
	if err != nil || r.Approval == nil {
		t.Fatalf("visibility: %v %+v", err, r)
	}
	if _, err := svc.Decide(ctx, r.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	f, err := svc.RenameSlug(ctx, "old-name", "new-name", core.ViaConsole)
	if err != nil {
		t.Fatal(err)
	}
	if f.PrivateURL != tail.URL("new-name") || f.PublicURL != pub.URL("new-name") {
		t.Fatalf("renamed flat reports %q and %q", f.PrivateURL, f.PublicURL)
	}

	if code, _, h := get(t, loop.URL("old-name")+"/a?b=1"); code != http.StatusTemporaryRedirect || h.Get("Location") != loop.URL("new-name")+"/a?b=1" {
		t.Fatalf("loopback redirect: %d %q", code, h.Get("Location"))
	}
	if got := tail.location(t, "old-name", "/a?b=1"); got != tail.URL("new-name")+"/a?b=1" {
		t.Fatalf("tailnet redirect: %q", got)
	}
	if code, _, h := get(t, pub.URL("old-name")+"/a?b=1"); code != http.StatusTemporaryRedirect || h.Get("Location") != pub.URL("new-name")+"/a?b=1" {
		t.Fatalf("public redirect: %d %q", code, h.Get("Location"))
	}
}
