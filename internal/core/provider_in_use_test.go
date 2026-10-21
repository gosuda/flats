package core

import (
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

// Without a lifecycle network, a revocation is refused only for a provider
// the flat's served routes may use. The public list is main's (Portal, zrok,
// a Cloudflare quick tunnel, NetBird Expose) plus proxy-public; Funnel stays
// out of it, as on main.
func TestProviderNotInUseWithoutRouteInspection(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(t.Context(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	for _, served := range []bool{false, true} {
		lf := s.state("site")
		lf.publicServed, lf.privateServed = served, false
		for id, refused := range map[ProviderID]bool{
			ProviderFunnel:          false,
			ProviderPortal:          served,
			ProviderZrok:            served,
			ProviderCloudflareQuick: served,
			ProviderNetbirdExpose:   served,
			ProviderProxyPublic:     served,
		} {
			if err := s.providerNotInUse(t.Context(), "site", id); errors.Is(err, ErrProviderInUse) != refused {
				t.Errorf("public routes served %t, %s: %v", served, id, err)
			}
		}
	}
}
