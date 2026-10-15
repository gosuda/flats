package config

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Older schemas keep their explicit NetBird management settings without
// inventing console exposure. Schema 3 additionally preserves its domains.
func TestVersionQuotaMigrationPreservesNetbirdSettings(t *testing.T) {
	for _, schema := range []int{1, 2, 3} {
		for _, quota := range []int{-1, 0, 4} {
			t.Run(fmt.Sprintf("v%d/quota%d", schema, quota), func(t *testing.T) {
				host := map[string]any{"instance_id": testID, "data_dir": "/var/lib/flats"}
				netbird := map[string]any{"management_url": "https://netbird.example.com:443"}
				doc := map[string]any{
					"schema_version": schema,
					"host":           host,
					"network": map[string]any{
						"permitted":       []string{"tailscale", "netbird"},
						"private_backend": "tailscale",
					},
					"netbird": netbird,
				}
				if schema == 1 {
					host["console_host"] = "ts-console"
				} else {
					doc["tailscale"] = map[string]any{"console_hosts": []string{"ts-console"}}
				}
				wantDomains := []string{}
				if schema < 3 {
					netbird["device_name"] = "old-shared-peer"
				} else {
					wantDomains = []string{"nb-console"}
					netbird["console_hosts"] = wantDomains
				}
				wantQuota := int64(20)
				if quota >= 0 {
					doc["system"] = map[string]any{"keep_versions": quota}
					wantQuota = int64(quota)
				}
				raw, err := json.Marshal(doc)
				if err != nil {
					t.Fatal(err)
				}
				loaded, err := Parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				cfg, err := loaded.Doc.Effective(nil)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.System.VersionQuota != wantQuota || cfg.Netbird.ManagementURL != "https://netbird.example.com:443" ||
					!slices.Equal(cfg.Netbird.ConsoleHosts, wantDomains) || !slices.Equal(cfg.Tailscale.ConsoleHosts, []string{"ts-console"}) {
					t.Fatalf("migration changed quota or domain policy: %+v %+v %+v", cfg.System, cfg.Netbird, cfg.Tailscale)
				}
				if loaded.Doc.SchemaVersion() != 4 || strings.Contains(string(loaded.Doc.Encode()), "device_name") {
					t.Fatalf("migration did not complete: %s", loaded.Doc.Encode())
				}
			})
		}
	}
}
