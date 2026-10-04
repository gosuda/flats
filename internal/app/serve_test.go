package app

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/provider"
	"github.com/gosuda/flats/internal/mcpx"
	"github.com/gosuda/flats/internal/store"
)

// Server-flat workers run in "/", so a relative --data or FLATS_DATA must be
// made absolute before anything uses it.
func TestServeFlagsDoNotGrantByDefault(t *testing.T) {
	o, err := ParseServeFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.Network != "local" || len(o.Set) != 0 || o.Portal || len(o.Permit) != 0 || o.ConfigPath != "" {
		t.Fatalf("defaults grant a provider: %+v", o)
	}
	o, err = ParseServeFlags([]string{"--network", "tailscale", "--permit", "tailscale-funnel"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.Set["network"] || o.Network != "tailscale" || len(o.Permit) != 1 || o.Permit[0] != "tailscale-funnel" {
		t.Fatalf("explicit grants: %+v", o)
	}
	if _, err := ParseServeFlags([]string{"--permit", "funnel"}); err == nil {
		t.Fatal("funnel was accepted as an alias")
	}
	o, err = ParseServeFlags([]string{"--config", "relative/config.json"})
	if err != nil || !filepath.IsAbs(o.ConfigPath) || !strings.HasSuffix(o.ConfigPath, filepath.Join("relative", "config.json")) {
		t.Fatalf("relative --config not resolved: %q %v", o.ConfigPath, err)
	}
}

func TestStartDoesNotInferGrantsFromOldDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tsnet", "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	h, err := Start(context.Background(), localOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.Public != nil || h.tsNet != nil {
		t.Fatal("historical directories started tailscale or portal")
	}
	if len(h.Providers.File().Permitted) != 0 || len(h.Config.Network.Permitted) != 0 {
		t.Fatalf("grants = %+v, config = %+v", h.Providers.File(), h.Config.Network)
	}
	if _, err := os.Stat(filepath.Join(dir, provider.FileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a new host wrote the legacy host file: %v", err)
	}
}

// After the first legacy start, flags may only repeat config.json: an
// explicit --portal=false no longer switches a permitted Portal off for one
// run. Omitting the flag runs from the config.
func TestLegacyFlagsMustMatchConfig(t *testing.T) {
	dir := t.TempDir()
	start := func(o Options) (*Host, error) {
		t.Helper()
		return Start(context.Background(), o)
	}
	on := localOptions(dir)
	on.Portal, on.Set = true, map[string]bool{"portal": true}
	h, err := start(on)
	if err != nil {
		t.Fatal(err)
	}
	if h.Public == nil || h.portalNet == nil || !slices.Equal(h.Config.Network.Permitted, []string{"portal"}) {
		t.Fatalf("portal grant did not start portal: public=%v config=%+v", h.Public != nil, h.Config.Network)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	off := localOptions(dir)
	off.Set = map[string]bool{"portal": true}
	if h, err := start(off); err == nil || !strings.Contains(err.Error(), "--portal false") || exitCode(err) != ExitConfig {
		if h != nil {
			h.Close()
		}
		t.Fatalf("explicit --portal=false after the grant: %v", err)
	}

	h, err = start(localOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.Public == nil || h.portalNet == nil {
		t.Fatal("omitted --portal ignored the configured portal grant")
	}
}

func exitCode(err error) int {
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return 1
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
// starting after the upgrade: the migration uses the Portal defaults, as
// serve did, and reports it.
func TestStartIgnoresBadStoredRelays(t *testing.T) {
	dir, st := storeWith(t, map[string]string{core.SetPortalRelays: "ftp://bad host"})
	st.Close()
	o := localOptions(dir)
	o.Portal, o.Set = true, map[string]bool{"portal": true}
	h, err := Start(context.Background(), o)
	if err != nil {
		t.Fatalf("serve exits on a bad stored relay: %v", err)
	}
	defer h.Close()
	if d := h.Public.Status().Detail; !strings.Contains(d, "Portal defaults") {
		t.Fatalf("public detail = %q", d)
	}
	if len(h.Config.Portal.Relays) != 0 {
		t.Fatalf("relays = %q", h.Config.Portal.Relays)
	}
}

// The legacy conversion produces the Portal and system values serve used.
func TestLegacyPlanSettings(t *testing.T) {
	plan := func(settings map[string]string, o Options) (*config.Config, []string, error) {
		t.Helper()
		dir, st := storeWith(t, settings)
		st.Close()
		info, err := store.Inspect(filepath.Join(dir, "flats.db"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := planLegacy(dir, info, o, config.NewInstanceID())
		if err != nil {
			return nil, nil, err
		}
		c, err := p.doc.Effective(nil)
		if err != nil {
			t.Fatal(err)
		}
		return c, p.notes, nil
	}

	c, notes, err := plan(map[string]string{core.SetPortalRelays: "https://relay.example/, ftp://bad host"}, Options{})
	if err != nil || len(c.Portal.Relays) != 0 || !c.Portal.Discovery || len(notes) != 1 {
		t.Fatalf("bad relay list: %+v notes=%q %v", c.Portal, notes, err)
	}

	c, notes, err = plan(map[string]string{core.SetPortalRelays: "https://relay.example/", core.SetPortalDiscover: "false", core.SetPortalMaxRelay: "5"}, Options{})
	if err != nil || !slices.Equal(c.Portal.Relays, []string{"https://relay.example"}) || c.Portal.Discovery || c.Portal.MaxActiveRelays != 5 || len(notes) != 0 {
		t.Fatalf("own relay only: %+v notes=%q %v", c.Portal, notes, err)
	}

	// --relays wins over the stored list; discovery still follows settings.
	c, _, err = plan(map[string]string{core.SetPortalRelays: "https://relay.example", core.SetPortalDiscover: "false"}, Options{Relays: []string{"https://flag.example"}})
	if err != nil || !slices.Equal(c.Portal.Relays, []string{"https://flag.example"}) || c.Portal.Discovery {
		t.Fatalf("flag relays: %+v %v", c.Portal, err)
	}

	c, notes, err = plan(map[string]string{core.SetPortalDiscover: "false", core.SetPortalMaxRelay: "-1"}, Options{})
	if err != nil || !c.Portal.Discovery || c.Portal.MaxActiveRelays != 3 || len(notes) != 2 {
		t.Fatalf("discovery off without relays: %+v notes=%q %v", c.Portal, notes, err)
	}

	c, notes, err = plan(map[string]string{core.SetKeepVersions: "20", core.SetUploadMaxBytes: "lots", core.SetRateLimit: "50"}, Options{})
	if err != nil || c.System.KeepVersions != 20 || c.System.UploadMaxBytes != 20<<20 || len(notes) != 1 {
		t.Fatalf("system settings: %+v notes=%q %v", c.System, notes, err)
	}
	if src := func() config.Source { _, s, _ := c.Lookup("system.rate_limit_rps"); return s }(); src != config.SourceDefault {
		t.Fatalf("a stored default was written to the file: %s", src)
	}

	// Every value serve accepted converts to one with the same effect.
	c, notes, err = plan(map[string]string{
		core.SetUploadMaxBytes: "4294967296",        // above the old 1 GiB cap: kept
		core.SetRateLimit:      "0",                 // limiting off
		core.SetKeepVersions:   "-3",                // no pruning
		core.SetEventsKeep:     "99999999999999999", // above 2^53-1
		core.SetPortalMaxRelay: "0",                 // Portal default
	}, Options{})
	if err != nil || c.System.UploadMaxBytes != 4294967296 || c.System.RateLimitRPS != config.MaxInt || c.System.KeepVersions != 0 ||
		c.System.EventsKeep != config.MaxInt || c.Portal.MaxActiveRelays != 3 || len(notes) != 4 {
		t.Fatalf("converted settings: %+v %+v notes=%q %v", c.System, c.Portal, notes, err)
	}
	if _, src, _ := c.Lookup("portal.max_active_relays"); src != config.SourceDefault {
		t.Fatal("max_active_relays=0 was written")
	}
	c, notes, err = plan(map[string]string{core.SetPortalMaxRelay: "4294967296"}, Options{})
	if err != nil || c.Portal.MaxActiveRelays != 1<<31-1 || len(notes) != 1 {
		t.Fatalf("huge max relays: %+v notes=%q %v", c.Portal, notes, err)
	}

	// Addresses are stored as written; a service name becomes its port, and
	// a flag serve could not have used is ignored with a note.
	flags := func(args ...string) Options {
		o, err := ParseServeFlags(args)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	c, notes, err = plan(nil, flags("--listen", "localhost:http", "--local-addr", ":7999", "--console-host", "My_Console"))
	if err != nil || c.Host.ManagementAddr != "localhost:80" || c.Host.LocalAddr != ":7999" || c.Host.ConsoleHost != "My_Console" || len(notes) != 1 {
		t.Fatalf("host flags: %+v notes=%q %v", c.Host, notes, err)
	}
	c, notes, err = plan(nil, flags("--listen", "nonsense", "--relays", "ftp://bad host"))
	if err != nil || c.Host.ManagementAddr != "127.0.0.1:7878" || len(c.Portal.Relays) != 0 || len(notes) != 2 {
		t.Fatalf("unusable flags: %+v %+v notes=%q %v", c.Host, c.Portal, notes, err)
	}
	if conflicts := flagConflicts(c, flags("--listen", "nonsense", "--relays", "ftp://bad host")); len(conflicts) != 0 {
		t.Fatalf("ignored flags compared: %q", conflicts)
	}
	c, notes, err = plan(nil, flags("--listen", "127.0.0.1:7879"))
	if err != nil || c.Host.ManagementAddr != "127.0.0.1:7878" || len(notes) != 1 {
		t.Fatalf("--listen on the local address: %+v notes=%q %v", c.Host, notes, err)
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
	for _, path := range []string{"/console/api/settings", "/api/flats", "/", "/llms.txt"} {
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

func TestManagementServerServesLLMsTxt(t *testing.T) {
	h := startLocal(t)
	_, port, _ := strings.Cut(h.Addr(), ":")
	do := func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, "http://"+h.Addr()+path, nil)
		req.Host = "localhost:" + port
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, path := range mcpx.LLMsPaths {
		code, body := do("GET", path)
		if code != 200 {
			t.Fatalf("%s = %d", path, code)
		}
		if path != mcpx.RuntimeReferencePath && !strings.Contains(body, "http://localhost:"+port+"/mcp") {
			t.Errorf("%s does not name the MCP endpoint of the requested host", path)
		}
		// Other methods reach the documentation handler, not the console.
		if code, _ := do("POST", path); code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d", path, code)
		}
	}
}
