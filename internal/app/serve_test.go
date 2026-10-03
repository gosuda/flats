package app

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/store"
)

// Server-flat workers run in "/", so a relative --data or FLATS_DATA must be
// made absolute before anything uses it.
func TestServeFlagsDoNotGrantByDefault(t *testing.T) {
	o, err := ParseServeFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.Network != "local" || o.NetworkSet || o.Portal || o.PortalSet || len(o.Permit) != 0 {
		t.Fatalf("defaults grant a provider: %+v", o)
	}
	o, err = ParseServeFlags([]string{"--network", "tailscale", "--permit", "tailscale-funnel"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.NetworkSet || o.Network != "tailscale" || len(o.Permit) != 1 || o.Permit[0] != "tailscale-funnel" {
		t.Fatalf("explicit grants: %+v", o)
	}
	if _, err := ParseServeFlags([]string{"--permit", "funnel"}); err == nil {
		t.Fatal("funnel was accepted as an alias")
	}
}

func TestStartDoesNotInferGrantsFromOldDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tsnet", "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	h, err := Start(context.Background(), Options{
		DataDir: dir, Listen: "127.0.0.1:0", Network: "local", LocalAddr: "127.0.0.1:0", ConsoleHost: "flats",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.Public != nil || h.tsNet != nil {
		t.Fatal("historical directories started tailscale or portal")
	}
	if h.Providers == nil || len(h.Providers.File().Permitted) != 0 {
		t.Fatalf("grants = %+v", h.Providers.File())
	}
	if !h.Providers.File().Migration.HistoricalTSNet {
		t.Fatalf("migration = %+v", h.Providers.File().Migration)
	}
}

func TestExplicitPortalFalseKeepsGrantAndDoesNotStart(t *testing.T) {
	dir := t.TempDir()
	start := func(portal, set bool) *Host {
		t.Helper()
		h, err := Start(context.Background(), Options{
			DataDir: dir, Listen: "127.0.0.1:0", Network: "local", LocalAddr: "127.0.0.1:0",
			ConsoleHost: "flats", Portal: portal, PortalSet: set,
		})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := start(true, true)
	if h.Public == nil || h.portalNet == nil || !h.Providers.File().Allows(provider.Portal) {
		t.Fatalf("portal grant did not start portal: public=%v net=%v file=%+v", h.Public != nil, h.portalNet != nil, h.Providers.File())
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	h = start(false, true)
	if h.Public != nil || h.portalNet != nil {
		t.Fatal("explicit --portal=false started portal")
	}
	if !h.Providers.File().Allows(provider.Portal) {
		t.Fatal("explicit --portal=false revoked the stored grant")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	h = start(false, false)
	if h.Public == nil || h.portalNet == nil || !h.Providers.File().Allows(provider.Portal) {
		t.Fatal("omitted --portal ignored the stored portal grant")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDataDirIsAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	o, err := ParseServeFlags([]string{"--data", "./data"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(wd, "data"); o.DataDir != want {
		t.Fatalf("--data ./data = %q, want %q", o.DataDir, want)
	}
	t.Setenv("FLATS_DATA", "rel/flats")
	if d, want := DefaultDataDir(), filepath.Join(wd, "rel", "flats"); d != want {
		t.Fatalf("FLATS_DATA=rel/flats: %q, want %q", d, want)
	}
	o, err = ParseServeFlags(nil)
	if err != nil || o.DataDir != filepath.Join(wd, "rel", "flats") {
		t.Fatalf("default data dir = %q, %v", o.DataDir, err)
	}
}

func storeWith(t *testing.T, settings map[string]string) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range settings {
		if err := st.SetSetting(context.Background(), k, v); err != nil {
			t.Fatal(err)
		}
	}
	return dir, st
}

// A bad relay saved in the console must not keep `flats serve` from
// starting after a restart: it is logged and Portal defaults are used.
func TestStartIgnoresBadStoredRelays(t *testing.T) {
	dir, st := storeWith(t, map[string]string{core.SetPortalRelays: "ftp://bad host"})
	st.Close()
	o := Options{DataDir: dir, Listen: "127.0.0.1:0", Network: "local", LocalAddr: "127.0.0.1:0", ConsoleHost: "flats", Portal: true}
	h, err := Start(context.Background(), o)
	if err != nil {
		t.Fatalf("serve exits on a bad stored relay: %v", err)
	}
	defer h.Close()
	if d := h.Public.Status().Detail; !strings.Contains(d, "Portal defaults") {
		t.Fatalf("public detail = %q", d)
	}
}

func TestPortalConfigFromSettings(t *testing.T) {
	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, f) }
	o := Options{DataDir: "/data"}

	_, st := storeWith(t, map[string]string{core.SetPortalRelays: "https://relay.example/, ftp://bad host"})
	cfg := portalConfig(context.Background(), st, o, logf)
	st.Close()
	if len(cfg.Relays) != 0 || !cfg.Discovery || len(logged) != 1 {
		t.Fatalf("bad relay list: %+v logs=%q", cfg, logged)
	}

	logged = nil
	_, st = storeWith(t, map[string]string{core.SetPortalRelays: "https://relay.example/", core.SetPortalDiscover: "false", core.SetPortalMaxRelay: "5"})
	cfg = portalConfig(context.Background(), st, o, logf)
	st.Close()
	if !slices.Equal(cfg.Relays, []string{"https://relay.example"}) || cfg.Discovery || cfg.MaxActiveRelays != 5 || len(logged) != 0 {
		t.Fatalf("own relay only: %+v logs=%q", cfg, logged)
	}

	// --relays wins over the stored list; discovery still follows settings.
	_, st = storeWith(t, map[string]string{core.SetPortalRelays: "https://relay.example", core.SetPortalDiscover: "false"})
	cfg = portalConfig(context.Background(), st, Options{DataDir: "/data", Relays: []string{"https://flag.example"}}, logf)
	st.Close()
	if !slices.Equal(cfg.Relays, []string{"https://flag.example"}) || cfg.Discovery {
		t.Fatalf("flag relays: %+v", cfg)
	}

	logged = nil
	_, st = storeWith(t, map[string]string{core.SetPortalDiscover: "false", core.SetPortalMaxRelay: "-1"})
	cfg = portalConfig(context.Background(), st, o, logf)
	st.Close()
	if !cfg.Discovery || cfg.MaxActiveRelays != 0 || len(logged) != 2 {
		t.Fatalf("discovery off without relays: %+v logs=%q", cfg, logged)
	}
}

// The management listener answers only to its own loopback names, so a page
// that rebinds its DNS name to 127.0.0.1 cannot use it.
func TestManagementServerRejectsForeignHost(t *testing.T) {
	h := startLocal(t)
	_, port, _ := strings.Cut(h.Addr(), ":")
	get := func(host, path string) int {
		req, _ := http.NewRequest("GET", "http://"+h.Addr()+path, nil)
		req.Host = host
		req.Header.Set("X-Flats-Console", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, path := range []string{"/console/api/settings", "/api/flats", "/"} {
		if code := get("attacker.example:"+port, path); code != 403 {
			t.Errorf("rebound Host on %s = %d", path, code)
		}
		for _, host := range []string{h.Addr(), "localhost:" + port, "[::1]:" + port} {
			if code := get(host, path); code != 200 {
				t.Errorf("Host %s on %s = %d", host, path, code)
			}
		}
	}
	req, _ := http.NewRequest("PUT", "http://"+h.Addr()+"/console/api/settings", strings.NewReader(`{"keep_versions":"3"}`))
	req.Header.Set("X-Flats-Console", "1")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://attacker.example:"+port)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("foreign Origin settings change = %d", resp.StatusCode)
	}
}
