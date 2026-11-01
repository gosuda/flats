package core

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

func TestPublicInstanceOffUnpublishedExposure(t *testing.T) {
	for _, backend := range []string{"legacy", "lifecycle"} {
		for _, marked := range []bool{false, true} {
			name := "unmarked"
			if marked {
				name = "mode-created"
			}
			t.Run(backend+"/"+name, func(t *testing.T) {
				dir := t.TempDir()
				st, err := store.Open(filepath.Join(dir, "flats.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { st.Close() })
				s, err := New(t.Context(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if s != nil {
						s.Close()
					}
				})
				network := newLifecycleRouteNet()
				public := &memPublic{memNet{hosts: map[string]http.Handler{}}}
				if backend == "lifecycle" {
					s.cfg.Lifecycle = network
				} else {
					s.cfg.Public = public
				}
				piCreationConfigure(t, s, marked)
				if marked {
					if _, err := s.CreateFlat(t.Context(), "site", "", ViaMCP); err != nil {
						t.Fatal(err)
					}
					if _, err := s.SetPublicInstance(t.Context(), false, "", "test"); err != nil {
						t.Fatal(err)
					}
				} else {
					// An unmarked record must keep main's behavior even in this unusual state.
					if err := s.st.CreateFlat(t.Context(), store.Flat{Slug: "site", Name: "site", Visibility: store.Public, CreatedAt: apNow, UpdatedAt: apNow}); err != nil {
						t.Fatal(err)
					}
					if err := s.st.SetProviderPermission(t.Context(), "site", store.ProviderPortal, true); err != nil {
						t.Fatal(err)
					}
				}
				check := func(s *Service) {
					t.Helper()
					f, err := s.st.GetFlat(t.Context(), "site")
					if err != nil {
						t.Fatal(err)
					}
					if s.PublicInstance().Enabled || f.CreatedPublicInstance != marked || f.LiveVersion != 0 {
						t.Fatalf("unexpected fixture: %+v", f)
					}
					err = s.ensureExposure(t.Context(), f)
					if marked {
						if err != nil {
							t.Fatalf("mode-created unpublished exposure = %v", err)
						}
					} else if !errors.Is(err, ErrNotDeployed) || !strings.Contains(err.Error(), "no current runtime is serving") {
						t.Fatalf("unmarked exposure = %v, want main's no-runtime error", err)
					}
					v := s.view(t.Context(), f)
					if marked {
						if v.PublicURL != "" || v.ConnectionState != "unavailable" || v.Publication != "unpublished" {
							t.Fatalf("mode-created unpublished view after Turn off: %+v", v)
						}
					} else if backend == "legacy" && (v.PublicURL != public.URL("site") || v.ConnectionState != "unavailable") {
						t.Fatalf("unmarked view differs from main: %+v", v)
					}
				}
				check(s)
				cfg := s.cfg
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s = nil
				restarted, err := New(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				s = restarted
				check(restarted)
				restarted.serveRedirect(t.Context(), "old-site", "site")
				served := network.serves("old-site", ProviderPortal)
				if backend == "legacy" {
					public.mu.Lock()
					_, served = public.hosts["old-site"]
					public.mu.Unlock()
				}
				if served == marked {
					t.Fatalf("public redirect served = %t, want %t", served, !marked)
				}
			})
		}
	}
}
