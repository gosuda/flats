package core

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

// audienceNet reports every route a flat owns with the audience it was
// registered for, so preview (Draft) endpoints reach the views as they do
// with the real provider manager.
type audienceNet struct {
	*lifecycleRouteNet
	mu       sync.Mutex
	audience map[string]ExposureAudience // host -> audience
}

func newAudienceNet() *audienceNet {
	return &audienceNet{lifecycleRouteNet: newLifecycleRouteNet(), audience: map[string]ExposureAudience{}}
}

func (n *audienceNet) ServeExposure(ctx context.Context, req ExposureRequest) (ExposureResult, error) {
	n.mu.Lock()
	n.audience[req.Host] = req.Audience
	n.mu.Unlock()
	return n.lifecycleRouteNet.ServeExposure(ctx, req)
}

func (n *audienceNet) ExposureStatus(_ context.Context, slugName string) (ExposureResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lifecycleRouteNet.mu.Lock()
	defer n.lifecycleRouteNet.mu.Unlock()
	res := ExposureResult{}
	for key := range n.routes {
		if n.owners[key] != slugName {
			continue
		}
		res.Endpoints = append(res.Endpoints, ExposureEndpoint{Provider: key.provider, URL: fmt.Sprintf("https://%s.%s.test", key.host, key.provider),
			State: "ready", Configured: true, Permitted: true, Ready: true, Audience: n.audience[key.host], Host: key.host})
	}
	return res, nil
}

func newBackendService(t *testing.T, backend string) (*Service, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(t.Context(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}},
		Lifecycle: newAudienceNet(), PrivateBackend: backend, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	return s, st
}

// openPreviewURL opens a Draft preview and returns the URL the caller gets,
// after checking that the preview list and the flat's event log (which
// `flats logs` shows) report the same one.
func openPreviewURL(t *testing.T, s *Service, st *store.Store, slugName string) string {
	t.Helper()
	p, err := s.OpenPreview(t.Context(), slugName, 0)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := s.ListPreviews(t.Context(), slugName)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(listed, func(l PreviewView) bool { return l.Host == p.Host })
	if i < 0 || listed[i].URL != p.URL {
		t.Fatalf("preview %s returned %q, listed %+v", p.Host, p.URL, listed)
	}
	events, err := st.ListEvents(t.Context(), slugName, "preview", 0, 100)
	if err != nil || len(events) == 0 {
		t.Fatalf("preview events %v %v", events, err)
	}
	if msg := events[len(events)-1].Message; !strings.HasSuffix(msg, " at "+p.URL) {
		t.Fatalf("preview event %q, want it to name %q", msg, p.URL)
	}
	return p.URL
}

// On a host whose console and private routes run on the tailnet, a new flat
// is reachable there from the start, whether an agent creates it or a first
// save does; its previews and its published private URL use the tailnet.
func TestNewFlatsFollowTailscaleBackend(t *testing.T) {
	ctx := t.Context()
	s, st := newBackendService(t, "tailscale")
	if _, err := s.CreateFlat(ctx, "created", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	lifecycleSave(t, s, "saved", "<h1>saved</h1>")
	for _, slugName := range []string{"created", "saved"} {
		ps, err := st.PermittedProviders(ctx, slugName)
		if err != nil || !slices.Equal(ps, []string{store.ProviderLocal, store.ProviderTailscale}) {
			t.Fatalf("%s providers %v %v", slugName, ps, err)
		}
		v, err := s.GetFlat(ctx, slugName)
		if err != nil || !slices.Contains(v.Providers, store.ProviderTailscale) {
			t.Fatalf("%s view providers %v %v", slugName, v.Providers, err)
		}
		// The grant is recorded with the flat.
		events, err := st.ListEvents(ctx, slugName, "provider", 0, 20)
		if err != nil || !slices.ContainsFunc(events, func(e store.Event) bool {
			return strings.Contains(e.Message, "tailscale permitted=true by default")
		}) {
			t.Fatalf("%s: no default-permission event in %+v %v", slugName, events, err)
		}
	}

	if got := openPreviewURL(t, s, st, "saved"); !strings.HasSuffix(got, ".tailscale.test") {
		t.Fatalf("preview URL %q, want the tailnet address", got)
	}
	lifecycleApprove(t, s, lifecycleRequest(t, s, "saved"))
	if v, err := s.GetFlat(ctx, "saved"); err != nil || v.LiveVersion != 1 || !strings.HasSuffix(v.PrivateURL, ".tailscale.test") {
		t.Fatalf("published flat %+v %v", v, err)
	}

	// The operator can still take a flat off the tailnet; its new previews
	// then use loopback.
	if err := s.SetProviderPermission(ctx, "saved", store.ProviderTailscale, false, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if got := openPreviewURL(t, s, st, "saved"); !strings.HasSuffix(got, ".local.test") {
		t.Fatalf("preview URL %q after revoking Tailscale, want loopback", got)
	}
}

// A loopback host keeps the existing default: new flats are local only until
// the operator allows another provider.
func TestNewFlatsStayLocalOnLoopbackBackend(t *testing.T) {
	ctx := t.Context()
	for _, backend := range []string{"", "local"} {
		s, st := newBackendService(t, backend)
		lifecycleSave(t, s, "page", "<h1>page</h1>")
		if ps, err := st.PermittedProviders(ctx, "page"); err != nil || !slices.Equal(ps, []string{store.ProviderLocal}) {
			t.Fatalf("backend %q: providers %v %v", backend, ps, err)
		}
		if events, err := st.ListEvents(ctx, "page", "provider", 0, 20); err != nil || len(events) != 0 {
			t.Fatalf("backend %q: provider events %+v %v", backend, events, err)
		}
		if got := openPreviewURL(t, s, st, "page"); !strings.HasSuffix(got, ".local.test") {
			t.Fatalf("backend %q: preview %q", backend, got)
		}
	}
}
