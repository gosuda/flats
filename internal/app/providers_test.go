package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
)

type providersBody struct {
	Providers []ProviderInfo `json:"providers"`
	ETag      string         `json:"etag"`
}

func providerByID(t *testing.T, h *Host, client *http.Client, id string) ProviderInfo {
	t.Helper()
	var out providersBody
	if code := consoleCall(t, h, client, "GET", "/console/api/providers", nil, &out); code != 200 {
		t.Fatalf("providers: %d", code)
	}
	for _, p := range out.Providers {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("provider %s missing: %+v", id, out.Providers)
	return ProviderInfo{}
}

func setHost(t *testing.T, h *Host, client *http.Client, id string, enabled bool) (int, map[string]any) {
	t.Helper()
	body := `{"enabled":false}`
	if enabled {
		body = `{"enabled":true}`
	}
	var out map[string]any
	code := consoleCall(t, h, client, "PUT", "/console/api/providers/"+id, strings.NewReader(body), &out)
	return code, out
}

func configPermitted(t *testing.T, h *Host) []string {
	t.Helper()
	b, err := os.ReadFile(h.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	c, err := loaded.Doc.Effective(nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Network.Permitted
}

// Only Local is on by default. The console turns other providers on for the
// host, saving network.permitted and attaching the backend at once, and
// refuses to turn one off while a flat still allows it.
func TestConsoleTurnsHostProvidersOnAndOff(t *testing.T) {
	h, client := consoleHost(t)
	for _, id := range []string{"tailscale", "tailscale-funnel", "portal"} {
		if p := providerByID(t, h, client, id); p.Enabled || p.Configured {
			t.Fatalf("%s on by default: %+v", id, p)
		}
	}
	if p := providerByID(t, h, client, "local"); !p.Enabled || p.Scope != "private" {
		t.Fatalf("local: %+v", p)
	}
	if p := providerByID(t, h, client, "portal"); p.Scope != "public" {
		t.Fatalf("portal scope: %+v", p)
	}
	if code, _ := setHost(t, h, client, "local", false); code != http.StatusBadRequest {
		t.Fatalf("local turned off: %d", code)
	}
	resp, err := http.Get("http://" + h.Addr() + "/api/providers")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("agent API exposes host providers: %d", resp.StatusCode)
	}

	if code, out := setHost(t, h, client, "portal", true); code != 200 || out["etag"] == "" {
		t.Fatalf("turn on portal: %d %v", code, out)
	}
	if p := providerByID(t, h, client, "portal"); !p.Enabled || !p.Configured {
		t.Fatalf("portal after turning on: %+v", p)
	}
	if got := configPermitted(t, h); !slices.Equal(got, []string{"portal"}) {
		t.Fatalf("config.json network.permitted = %v", got)
	}
	if _, err := h.Svc.CreateFlat(context.Background(), "site", "Site", core.ViaMCP); err != nil {
		t.Fatal(err)
	}
	if err := h.Svc.SetProviderPermission(context.Background(), "site", "portal", true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if p := providerByID(t, h, client, "portal"); len(p.Flats) != 1 || p.Flats[0] != "site" {
		t.Fatalf("portal flats: %+v", p)
	}
	if code, out := setHost(t, h, client, "portal", false); code != http.StatusConflict || out["category"] != "provider_in_use" {
		t.Fatalf("turned off portal in use: %d %v", code, out)
	}
	if err := h.Svc.SetProviderPermission(context.Background(), "site", "portal", false, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if code, out := setHost(t, h, client, "portal", false); code != 200 {
		t.Fatalf("turn off portal: %d %v", code, out)
	}
	if p := providerByID(t, h, client, "portal"); p.Enabled {
		t.Fatalf("portal still on: %+v", p)
	}
	if got := configPermitted(t, h); len(got) != 0 {
		t.Fatalf("config.json network.permitted = %v", got)
	}

	if code, out := setHost(t, h, client, "tailscale-funnel", true); code != 200 {
		t.Fatalf("turn on funnel: %d %v", code, out)
	}
	if p := providerByID(t, h, client, "tailscale"); p.Enabled || !p.Configured {
		t.Fatalf("funnel must attach the tailnet without granting tailscale: %+v", p)
	}
	if !h.Providers.HostAllows("tailscale-funnel") || h.Providers.HostAllows("tailscale") {
		t.Fatalf("grants: %+v", h.Providers.File())
	}
}

// A stale settings ETag refuses the change, like a settings save.
func TestConsoleHostProviderChecksETag(t *testing.T) {
	h, client := consoleHost(t)
	base := "http://" + h.Addr()
	req, _ := http.NewRequest("PUT", base+"/console/api/providers/portal", strings.NewReader(`{"enabled":true}`))
	for k, v := range map[string]string{"X-Flats-Console": "1", "Origin": base, "Sec-Fetch-Site": "same-origin", "Content-Type": "application/json", "If-Match": `"stale"`} {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale ETag: %d", resp.StatusCode)
	}
	if p := providerByID(t, h, client, "portal"); p.Enabled {
		t.Fatalf("portal turned on with a stale ETag: %+v", p)
	}
}

// A host grant made in the console survives a restart.
func TestConsoleHostProviderPersists(t *testing.T) {
	dir := t.TempDir()
	h, client := consoleHostWith(t, dir, nil)
	if code, out := setHost(t, h, client, "portal", true); code != 200 {
		t.Fatalf("turn on portal: %d %v", code, out)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h, client = consoleHostWith(t, dir, nil)
	if p := providerByID(t, h, client, "portal"); !p.Enabled || !p.Configured {
		t.Fatalf("portal after restart: %+v", p)
	}
}

// A legacy service whose --portal flag sets Portal keeps it: the console
// would make the next start disagree with the flag.
func TestConsoleHostProviderPinnedByLegacyFlag(t *testing.T) {
	h, client := consoleHostWith(t, t.TempDir(), func(o *Options) {
		o.Portal = true
		if o.Set == nil {
			o.Set = map[string]bool{}
		}
		o.Set["portal"] = true
	})
	if p := providerByID(t, h, client, "portal"); !p.Enabled || !strings.Contains(p.Locked, "--portal") {
		t.Fatalf("portal: %+v", p)
	}
	if code, out := setHost(t, h, client, "portal", false); code != http.StatusConflict {
		t.Fatalf("turned off a flag-set provider: %d %v", code, out)
	}
	if !slices.Equal(configPermitted(t, h), []string{"portal"}) {
		t.Fatal("config.json changed")
	}
}

// zrok is off by default and public. Turning it on needs an enabled zrok
// environment; without one the console refuses and config.json is unchanged.
func TestConsoleZrokNeedsEnabledEnvironment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h, client := consoleHost(t)
	p := providerByID(t, h, client, "zrok")
	if p.Enabled || p.Configured || p.Scope != "public" || p.Namespace != "public" {
		t.Fatalf("zrok by default: %+v", p)
	}
	code, out := setHost(t, h, client, "zrok", true)
	if code == 200 || !strings.Contains(fmt.Sprint(out["error"]), "zrok2 enable") {
		t.Fatalf("turned on zrok without an environment: %d %v", code, out)
	}
	if got := configPermitted(t, h); len(got) != 0 {
		t.Fatalf("config.json network.permitted = %v", got)
	}
	if p := providerByID(t, h, client, "zrok"); p.Enabled || p.Configured {
		t.Fatalf("zrok after refused turn-on: %+v", p)
	}
}

// The zrok part of the provider configuration token changes with the
// environment and namespace, and is empty without a zrok grant, so hosts
// without zrok keep their token.
func TestZrokConfigurationToken(t *testing.T) {
	base := &config.Config{}
	base.Zrok = config.ZrokConfig{Environment: "/a", Namespace: "public"}
	if got := zrokConfiguration(base, nil); got != "" {
		t.Fatalf("without a grant: %q", got)
	}
	base.Network.Permitted = []string{"zrok"}
	first := zrokConfiguration(base, nil)
	other := *base
	other.Zrok.Environment = "/b"
	if first == "" || zrokConfiguration(&other, nil) == first {
		t.Fatal("changing zrok.environment did not change the token")
	}
	other = *base
	other.Zrok.Namespace = "flats"
	if zrokConfiguration(&other, nil) == first {
		t.Fatal("changing zrok.namespace did not change the token")
	}
}
