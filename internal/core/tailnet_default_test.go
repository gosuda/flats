package core

import (
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

func newBackendService(t *testing.T, backend string) (*Service, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(t.Context(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}},
		Lifecycle: newLifecycleRouteNet(), PrivateBackend: backend, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	return s, st
}

// On a host whose console and private routes run on the tailnet, a new flat
// is reachable there from the start, whether an agent creates it or a first
// save does; its previews use the tailnet address.
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
	}
	events, err := st.ListEvents(ctx, "saved", "provider", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events, func(e store.Event) bool {
		return e.Kind == "provider" && strings.Contains(e.Message, "tailscale permitted=true by default")
	}) {
		t.Fatalf("no default-permission event in %+v", events)
	}

	if got := previewEventURL(t, s, st, "saved"); !strings.HasSuffix(got, ".tailscale.test") {
		t.Fatalf("preview URL %q, want the tailnet address", got)
	}

	// The operator can still take a flat off the tailnet; its previews then
	// fall back to loopback.
	if err := s.SetProviderPermission(ctx, "saved", store.ProviderTailscale, false, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if got := previewEventURL(t, s, st, "saved"); !strings.HasSuffix(got, ".local.test") {
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
		if got := previewEventURL(t, s, st, "page"); !strings.HasSuffix(got, ".local.test") {
			t.Fatalf("backend %q: preview %q", backend, got)
		}
	}
}

// previewEventURL opens a Draft preview and returns the address it was served
// at, as recorded in the flat's event log. (The view's URL comes from the
// provider manager's status, which this test network does not report.)
func previewEventURL(t *testing.T, s *Service, st *store.Store, slugName string) string {
	t.Helper()
	if _, err := s.OpenPreview(t.Context(), slugName, 0); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(t.Context(), slugName, "preview", 0, 100)
	if err != nil || len(events) == 0 {
		t.Fatalf("preview events %v %v", events, err)
	}
	msg := events[len(events)-1].Message
	return msg[strings.LastIndex(msg, " at ")+len(" at "):]
}
