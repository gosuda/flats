package provider

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/store"
)

// asyncPortal models Portal's documented contract: Serve registers the route
// but returns no URL; readiness arrives independently after Serve returns.
// advance is an explicit event, avoiding wall-clock timing in regressions.
type asyncPortal struct {
	*local.Public
	mu           sync.Mutex
	ready        bool
	routes       map[string]bool
	opens, stops int
	stopErr      error
}

func (p *asyncPortal) Serve(ctx context.Context, slug string, h http.Handler, hidden bool) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.routes[slug] {
		p.opens++
	}
	p.routes[slug] = true
	_, err := p.Public.Serve(ctx, slug, h, hidden)
	return "", err
}
func (p *asyncPortal) Stop(slug string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopErr != nil {
		return p.stopErr
	}
	delete(p.routes, slug)
	p.stops++
	return p.Public.Stop(slug)
}
func (p *asyncPortal) Status() core.NetStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := core.NetStatus{Kind: "portal", Enabled: true}
	for slug := range p.routes {
		h := core.HostInfo{Host: slug, State: "starting"}
		if p.ready {
			h.State, h.URL = "ready", p.Public.URL(slug)
		}
		st.Hosts = append(st.Hosts, h)
	}
	return st
}
func (p *asyncPortal) URL(slug string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ready || !p.routes[slug] {
		return ""
	}
	return p.Public.URL(slug)
}
func (p *asyncPortal) advance() { p.mu.Lock(); p.ready = true; p.mu.Unlock() }

type asyncFunnel struct {
	*local.Net
	public *asyncPortal
}

func (f *asyncFunnel) ServeFunnel(ctx context.Context, host string, h http.Handler) (string, error) {
	return f.public.Serve(ctx, host, h, false)
}
func (f *asyncFunnel) StopFunnel(host string) error { return f.public.Stop(host) }
func (f *asyncFunnel) FunnelState(host string) ExposureEndpoint {
	st := f.public.Status()
	for _, h := range st.Hosts {
		if h.Host == host {
			return ExposureEndpoint{Provider: Funnel, URL: h.URL, State: h.State}
		}
	}
	return ExposureEndpoint{Provider: Funnel, State: "unavailable"}
}

type reviewFixture struct {
	clock   *atomic.Int64
	svc     *core.Service
	manager *Manager
	public  *asyncPortal
	local   *local.Net
	tail    *local.Net
}

func reviewerService(t *testing.T, id ID) reviewFixture {
	t.Helper()
	return reviewerServiceMode(t, id, false)
}
func reviewerServiceMode(t *testing.T, id ID, legacy bool) reviewFixture {
	t.Helper()
	listen := func() *local.Net {
		n, e := local.Listen("127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { n.Close() })
		return n
	}
	loop, pub, tail := listen(), listen(), listen()
	p := &asyncPortal{Public: local.NewPublic(pub), routes: map[string]bool{}}
	dir := t.TempDir()
	if err := Save(dir, File{Version: 1, Permitted: []ID{id, Tailscale}}); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, Options{Local: loop, Portal: p, Tailscale: &asyncFunnel{Net: tail, public: p}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	var lifecycle core.LifecycleNet = m
	var public core.PublicNet
	if legacy {
		lifecycle = nil
		public = p
	}
	svc, err := core.New(t.Context(), core.Config{Public: public, Now: func() time.Time { return time.Unix(0, clock.Load()).UTC() }, DataDir: dir, Store: st, Private: tail, Lifecycle: lifecycle, ConsoleURL: func() string { return "http://console" }, ValidateOperatorDecision: func(context.Context) error { return nil }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close(); m.Close(); st.Close() })
	_, err = svc.SaveVersion(t.Context(), "site", []bundle.File{{Path: "index.html", Data: []byte("v1")}}, core.SaveMeta{}, core.ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	r, err := svc.Publish(t.Context(), "site", 0, "", core.ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	approveReview(t, svc, r)
	if err := svc.SetProviderPermission(t.Context(), "site", string(id), true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	return reviewFixture{clock, svc, m, p, loop, tail}
}
func approveReview(t *testing.T, s *core.Service, r core.ActionResult) {
	t.Helper()
	if r.Approval == nil {
		t.Fatal("no approval", r)
	}
	a, err := s.Decide(t.Context(), r.Approval.ID, true)
	if err != nil || a.Status != "approved" {
		t.Fatalf("approval: %+v %v", a, err)
	}
}
func publicReview(t *testing.T, f reviewFixture) {
	t.Helper()
	r, err := f.svc.SetVisibility(t.Context(), "site", store.Public, core.ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	approveReview(t, f.svc, r)
}
func TestReviewerReproPublicNeedsSynchronousReadiness(t *testing.T) {
	for _, id := range []ID{Portal, Funnel} {
		t.Run(string(id), func(t *testing.T) {
			f := reviewerService(t, id)
			r, err := f.svc.SetVisibility(t.Context(), "site", store.Public, core.ViaAPI, "")
			if err != nil {
				t.Fatal(err)
			}
			before, _ := f.svc.GetFlat(t.Context(), "site")
			if before.Visibility != store.Private || f.public.opens != 0 {
				t.Fatal("route opened before approval")
			}
			approveReview(t, f.svc, r)
			starting, _ := f.svc.GetFlat(t.Context(), "site")
			if starting.Visibility != store.Public || starting.ConnectionState != "starting" || f.public.stops != 0 {
				t.Fatalf("async route discarded or dishonest state: %+v stops=%d", starting, f.public.stops)
			}
			for _, ep := range starting.Endpoints {
				if ep.Provider == id && ep.Ready {
					t.Fatal("starting claims ready")
				}
			}
			// Repeating a decision and an unchanged visibility request must never
			// stop/reopen the starting route; a real backend can finish registration.
			for i := 0; i < 3; i++ {
				approveReview(t, f.svc, r)
				same, err := f.svc.SetVisibility(t.Context(), "site", store.Public, core.ViaAPI, "")
				if err != nil || same.Status != "done" {
					t.Fatal(same, err)
				}
			}
			f.public.advance()
			ready, _ := f.svc.GetFlat(t.Context(), "site")
			if ready.ConnectionState != "ready" || f.public.opens != 1 || f.public.stops != 0 {
				t.Fatalf("readiness did not converge: %+v opens=%d stops=%d", ready, f.public.opens, f.public.stops)
			}
			if get(t, ready.PublicURL) != "v1" {
				t.Fatal("not current content")
			}
		})
	}
}
func TestReviewerReproRevokeLeavesPublicRoute(t *testing.T) {
	for _, id := range []ID{Portal, Funnel} {
		t.Run(string(id), func(t *testing.T) {
			f := reviewerService(t, id)
			f.public.advance()
			publicReview(t, f)
			err := f.svc.SetProviderPermission(t.Context(), "site", string(id), false, core.ViaConsole)
			if !errors.Is(err, core.ErrProviderInUse) {
				t.Fatal("in-use permission removed without precise refusal", err)
			}
			view, _ := f.svc.GetFlat(t.Context(), "site")
			if view.Visibility != store.Public || get(t, view.PublicURL) != "v1" {
				t.Fatal("refusal changed serving policy")
			}
			for _, ep := range view.Endpoints {
				if ep.Provider == id && (!ep.Permitted || !ep.Ready) {
					t.Fatalf("refusal misreported route %+v", ep)
				}
			}
			r, err := f.svc.SetVisibility(t.Context(), "site", store.Private, core.ViaAPI, "")
			if err != nil {
				t.Fatal(err)
			}
			approveReview(t, f.svc, r)
			if err := f.svc.SetProviderPermission(t.Context(), "site", string(id), false, core.ViaConsole); err != nil {
				t.Fatal(err)
			}
			status, _ := f.manager.ExposureStatus(t.Context(), "site")
			for _, ep := range status.Endpoints {
				if ep.Provider == id && (ep.Ready || ep.URL != "") {
					t.Fatalf("stopped route still ready %+v", ep)
				}
			}
		})
	}
}
func TestReviewerReproDeleteLeavesPublicRoute(t *testing.T) {
	for _, id := range []ID{Portal, Funnel} {
		t.Run(string(id), func(t *testing.T) {
			f := reviewerService(t, id)
			f.public.advance()
			publicReview(t, f)
			view, _ := f.svc.GetFlat(t.Context(), "site")
			publicURL := view.PublicURL
			r, err := f.svc.Delete(t.Context(), "site", core.ViaAPI, "")
			if err != nil {
				t.Fatal(err)
			}
			approveReview(t, f.svc, r)
			if _, ok := f.manager.take("site", id, false); ok || f.public.stops == 0 {
				t.Fatal("deleted route still registered")
			}
			response, err := http.Get(publicURL)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode == 200 {
				t.Fatal("deleted route serves")
			}
			status, _ := f.manager.ExposureStatus(t.Context(), "site")
			for _, ep := range status.Endpoints {
				if ep.Ready {
					t.Fatalf("deleted endpoint ready %+v", ep)
				}
			}
		})
	}
}
func TestDeletePublicStopFailurePreservesFlat(t *testing.T) {
	f := reviewerService(t, Portal)
	f.public.advance()
	publicReview(t, f)
	f.public.stopErr = errors.New("stop not confirmed")
	r, err := f.svc.Delete(t.Context(), "site", core.ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.svc.Decide(t.Context(), r.Approval.ID, true)
	if !errors.Is(err, core.ErrPublicStopUnconfirmed) || a.Status != "failed" {
		t.Fatalf("delete %+v %v", a, err)
	}
	v, err := f.svc.GetFlat(t.Context(), "site")
	if err != nil || v.Visibility != store.Public || get(t, v.PublicURL) != "v1" {
		t.Fatalf("failed delete lost state %+v %v", v, err)
	}
}
func TestPrivateDTOUsesActuallyServedProvider(t *testing.T) {
	f := reviewerService(t, Portal)
	v, _ := f.svc.GetFlat(t.Context(), "site")
	if v.PrivateURL != f.local.URL("site") || v.PrivateState != "ready" {
		t.Fatalf("unpermitted tailscale advertised %+v", v)
	}
	if err := f.svc.SetProviderPermission(t.Context(), "site", string(Tailscale), true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	// A publish reopens the explicitly permitted private provider.
	_, err := f.svc.SaveVersion(t.Context(), "site", []bundle.File{{Path: "index.html", Data: []byte("v2")}}, core.SaveMeta{}, core.ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.Publish(t.Context(), "site", 0, "", core.ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	approveReview(t, f.svc, r)
	v, _ = f.svc.GetFlat(t.Context(), "site")
	if v.PrivateURL != f.tail.URL("site") || v.PrivateState != "ready" {
		t.Fatalf("served tailscale not advertised %+v", v)
	}
	if err := f.svc.SetProviderPermission(t.Context(), "site", string(Tailscale), false, core.ViaConsole); err == nil {
		t.Fatal("private permission removed while serving")
	}
}

func TestManagerRenameRedirectExpiryAndPrivateTeardown(t *testing.T) {
	for _, id := range []ID{Portal, Funnel} {
		t.Run(string(id), func(t *testing.T) {
			f := reviewerService(t, id)
			f.public.advance()
			publicReview(t, f)
			before, _ := f.svc.GetFlat(t.Context(), "site")
			old := before.PublicURL
			moved, err := f.svc.RenameSlug(t.Context(), "site", "moved", core.ViaAPI)
			if err != nil {
				t.Fatal(err)
			}
			if moved.PublicURL == old {
				t.Fatal("new public address missing")
			}
			// No redirects may leak working content; old current route becomes a 307.
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			response, err := client.Get(old)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 307 {
				t.Fatalf("old public route=%d", response.StatusCode)
			}
			f.clock.Add(int64(8 * 24 * time.Hour))
			f.svc.Sweep(t.Context())
			if _, ok := f.manager.take("site", id, false); ok {
				t.Fatal("expired public redirect registered")
			}
			if _, ok := f.manager.take("site", Local, false); ok {
				t.Fatal("expired Private redirect registered")
			}
			response, err = client.Get(old)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 404 {
				t.Fatalf("expired route=%d", response.StatusCode)
			}
			r, err := f.svc.Delete(t.Context(), "moved", core.ViaAPI, "")
			if err != nil {
				t.Fatal(err)
			}
			approveReview(t, f.svc, r)
			if _, ok := f.manager.take("moved", id, false); ok {
				t.Fatal("renamed provider survives deletion")
			}
		})
	}
}

func TestRedirectExpiryRetriesUnconfirmedProviderStop(t *testing.T) {
	f := reviewerService(t, Portal)
	f.public.advance()
	publicReview(t, f)
	if _, err := f.svc.RenameSlug(t.Context(), "site", "moved", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	f.public.stopErr = errors.New("lease unregister unconfirmed")
	f.clock.Add(int64(8 * 24 * time.Hour))
	f.svc.Sweep(t.Context())
	if _, ok := f.manager.take("site", Portal, false); !ok {
		t.Fatal("unconfirmed stop lost route tracking")
	}
	if f.svc.Redirects()["site"] != "moved" {
		t.Fatal("unconfirmed stop lost retry registration")
	}
	if err := f.svc.SetProviderPermission(t.Context(), "moved", string(Portal), false, core.ViaConsole); !errors.Is(err, core.ErrProviderInUse) {
		t.Fatal("permission removed over unconfirmed redirect", err)
	}
	f.public.stopErr = nil
	f.svc.Sweep(t.Context())
	if _, ok := f.manager.take("site", Portal, false); ok {
		t.Fatal("retry left expired lease registered")
	}
	if _, ok := f.svc.Redirects()["site"]; ok {
		t.Fatal("successful expiry retained alias")
	}
}

func TestLegacyPortalCommitsHonestConnectingState(t *testing.T) {
	f := reviewerServiceMode(t, Portal, true)
	publicReview(t, f)
	v, _ := f.svc.GetFlat(t.Context(), "site")
	if v.Visibility != store.Public || v.ConnectionState != "starting" || v.PublicURL != "" || f.public.stops != 0 {
		t.Fatalf("legacy async state %+v stops=%d", v, f.public.stops)
	}
	f.public.advance()
	v, _ = f.svc.GetFlat(t.Context(), "site")
	if v.ConnectionState != "ready" || get(t, v.PublicURL) != "v1" {
		t.Fatalf("legacy readiness %+v", v)
	}
}
