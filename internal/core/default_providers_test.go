package core

import (
	"slices"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

// A new flat inherits only the default provider of each mode, not every
// provider the host has turned on, and stays Private.
func TestNewFlatInheritsDefaultProviders(t *testing.T) {
	s, st := newBackendService(t, "local")
	ctx := t.Context()
	if _, err := s.SetPermittedNetworks([]string{"tailscale", "tailscale-funnel", "zrok"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDefaultPrivate: "tailscale", SetDefaultPublic: "zrok"}); err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateFlat(ctx, "inherits", "", ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	if f.Visibility != store.Private {
		t.Fatalf("visibility = %q, want private", f.Visibility)
	}
	got, err := st.PermittedProviders(ctx, "inherits")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"local", "tailscale", "zrok"}; !slices.Equal(got, want) {
		t.Fatalf("providers = %v, want %v (funnel is on for the host but not the default)", got, want)
	}

	// Turning the default off clears it; later flats get no public provider.
	if _, err := s.SetPermittedNetworks([]string{"tailscale", "tailscale-funnel"}, ""); err != nil {
		t.Fatal(err)
	}
	if priv, pub := s.DefaultProviders(); priv != "tailscale" || pub != "" {
		t.Fatalf("defaults = %q, %q after turning zrok off", priv, pub)
	}
	if _, err := s.CreateFlat(ctx, "after", "", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.PermittedProviders(ctx, "after"); !slices.Equal(got, []string{"local", "tailscale"}) {
		t.Fatalf("providers = %v, want [local tailscale]", got)
	}
}

// A public default must be a provider the host has turned on.
func TestDefaultPublicMustBePermitted(t *testing.T) {
	s, _ := newBackendService(t, "local")
	if _, err := s.UpdateSettings(t.Context(), map[string]string{SetDefaultPublic: "portal"}); err == nil {
		t.Fatal("portal accepted as the default without being turned on")
	}
}

// A Cloudflare quick tunnel can be the public default like the other public
// providers: a new flat is allowed on it and stays Private.
func TestCloudflareQuickCanBeTheDefaultPublic(t *testing.T) {
	s, st := newBackendService(t, "local")
	ctx := t.Context()
	if _, err := s.SetPermittedNetworks([]string{"cloudflare-quick"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDefaultPublic: "cloudflare-quick"}); err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateFlat(ctx, "quick-default", "", ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	if f.Visibility != store.Private {
		t.Fatalf("visibility = %q, want private", f.Visibility)
	}
	got, err := st.PermittedProviders(ctx, "quick-default")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"cloudflare-quick", "local"}; !slices.Equal(got, want) {
		t.Fatalf("providers = %v, want %v", got, want)
	}
}
