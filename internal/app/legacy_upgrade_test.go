package app

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/store"
)

func TestLegacyTailscaleDefaultUpgradeRequiresExplicitChoiceBeforeNetworking(t *testing.T) {
	for _, choice := range []string{"unspecified", "tailscale", "local", "stored-tailscale"} {
		t.Run(choice, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.sqlite")
			st, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.CreateFlat(t.Context(), store.Flat{Slug: "legacy", Name: "legacy", Visibility: store.Private, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			st.Close()
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`PRAGMA user_version=1`); err != nil {
				t.Fatal(err)
			}
			db.Close()
			st, err = store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			grants := provider.File{Version: 1, PrivateBackend: "local", Migration: provider.Migration{HistoricalTSNet: true}}
			opts := Options{Network: "local"}
			if choice == "stored-tailscale" {
				grants.PrivateBackend = "tailscale"
				grants.Permitted = []provider.ID{provider.Tailscale}
			} else if choice != "unspecified" {
				opts.Network, opts.NetworkSet = choice, true
			}
			err = prepareLegacyPrivateUpgrade(t.Context(), st, opts, grants)
			pending, perr := st.LegacyPrivateUpgradePending(t.Context())
			if perr != nil {
				t.Fatal(perr)
			}
			if choice == "unspecified" {
				if err == nil || !strings.Contains(err.Error(), "explicit upgrade choice") || !pending {
					t.Fatalf("silent legacy narrowing pending=%t err=%v", pending, err)
				}
			} else if err != nil || pending {
				t.Fatalf("explicit choice not recorded pending=%t err=%v", pending, err)
			}
			for _, id := range []string{store.ProviderTailscale, store.ProviderFunnel, store.ProviderPortal} {
				permitted, err := st.ProviderPermitted(t.Context(), "legacy", id)
				if err != nil || permitted != ((choice == "tailscale" || choice == "stored-tailscale") && id == store.ProviderTailscale) {
					t.Fatalf("choice %s provider %s permitted=%t %v", choice, id, permitted, err)
				}
			}
			if choice != "unspecified" {
				if err := prepareLegacyPrivateUpgrade(t.Context(), st, Options{Network: "local"}, grants); err != nil {
					t.Fatal("restart forgot explicit choice", err)
				}
			}
		})
	}
}

func TestNonHistoricalHostLaterTailscaleDoesNotOptInLegacyFlats(t *testing.T) {

	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateFlat(t.Context(), store.Flat{Slug: "legacy", Name: "legacy", Visibility: store.Private, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	grants := provider.File{Version: 1, Migration: provider.Migration{HistoricalTSNet: false}}
	if err := prepareLegacyPrivateUpgrade(t.Context(), st, Options{Network: "local"}, grants); err != nil {
		t.Fatal(err)
	}
	pending, err := st.LegacyPrivateUpgradePending(t.Context())
	if err != nil || pending {
		t.Fatalf("Local startup retained eligibility: %t %v", pending, err)
	}
	grants.PrivateBackend, grants.Permitted = "tailscale", []provider.ID{provider.Tailscale}
	if err := prepareLegacyPrivateUpgrade(t.Context(), st, Options{Network: "tailscale", NetworkSet: true}, grants); err != nil {
		t.Fatal(err)
	}
	permitted, err := st.ProviderPermitted(t.Context(), "legacy", store.ProviderTailscale)
	if err != nil || permitted {
		t.Fatalf("later host grant opted in Local-only flat: %t %v", permitted, err)
	}
}
