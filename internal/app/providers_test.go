package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/portal"
	"github.com/gosuda/flats/internal/expose/provider"
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
	if code == 200 || !strings.Contains(fmt.Sprint(out["error"]), "account token") {
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

// fakeZrokEnvironment writes an enabled zrok environment where Flats manages
// it for the host in dataDir. Loading it contacts nothing.
func fakeZrokEnvironment(t *testing.T, dataDir string) {
	t.Helper()
	dir := filepath.Join(dataDir, "zrok-environment")
	if err := os.MkdirAll(filepath.Join(dir, "identities"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"metadata.json":    `{"v":"v0.4"}`,
		"environment.json": `{"zrok_token":"test-token","ziti_identity":"test-env","api_endpoint":"https://127.0.0.1:1"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Turning zrok on from the console gives the policy token the next start
// computes, so an approval requested meanwhile is not stale after a restart.
func TestConsoleZrokPolicyMatchesRestart(t *testing.T) {
	dir := t.TempDir()
	fakeZrokEnvironment(t, dir)
	h, client := consoleHostWith(t, dir, nil)
	if code, out := setHost(t, h, client, "zrok", true); code != 200 {
		t.Fatalf("turn on zrok: %d %v", code, out)
	}
	if p := providerByID(t, h, client, "zrok"); !p.Enabled || !p.Configured {
		t.Fatalf("zrok after turning on: %+v", p)
	}
	live, err := h.Providers.ExposurePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h, _ = consoleHostWith(t, dir, nil)
	restarted, err := h.Providers.ExposurePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if live != restarted {
		t.Fatal("the policy token after a restart differs from the one after turning zrok on")
	}
}

// Turning zrok off from the console also gives the policy token of the next
// start, which has no zrok grant.
func TestConsoleZrokOffPolicyMatchesRestart(t *testing.T) {
	dir := t.TempDir()
	fakeZrokEnvironment(t, dir)
	h, client := consoleHostWith(t, dir, nil)
	if code, out := setHost(t, h, client, "zrok", true); code != 200 {
		t.Fatalf("turn on zrok: %d %v", code, out)
	}
	if code, out := setHost(t, h, client, "zrok", false); code != 200 {
		t.Fatalf("turn off zrok: %d %v", code, out)
	}
	live, err := h.Providers.ExposurePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h, _ = consoleHostWith(t, dir, nil)
	restarted, err := h.Providers.ExposurePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if live != restarted {
		t.Fatal("the policy token after a restart differs from the one after turning zrok off")
	}
}

// zrokController emulates the zrok v2 endpoints Connect and Disconnect use.
func zrokController(t *testing.T, tokens ...string) *[]string {
	t.Helper()
	var calls []string
	next := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zrok.v1+json")
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v2/clientVersionCheck":
			w.WriteHeader(http.StatusOK)
		case "/api/v2/enable":
			if !slices.Contains(tokens, r.Header.Get("X-TOKEN")) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"identity":"env-%d","cfg":"{}"}`, next)
		case "/api/v2/disable":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZROK2_API_ENDPOINT", srv.URL)
	return &calls
}

// The operator connects a zrok account in the console with its token, and
// Flats does the rest: no zrok CLI is needed.
func TestConsoleConnectsZrokAccount(t *testing.T) {
	calls := zrokController(t, "good-token", "new-token")
	dir := t.TempDir()
	h, client := consoleHostWith(t, dir, nil)
	if p := providerByID(t, h, client, "zrok"); p.Connected || !p.Managed {
		t.Fatalf("zrok before connecting: %+v", p)
	}
	connect := func(token string) (int, map[string]any) {
		var out map[string]any
		code := consoleCall(t, h, client, "PUT", "/console/api/providers/zrok/account", strings.NewReader(`{"token":"`+token+`"}`), &out)
		return code, out
	}
	if code, out := connect("bad-token"); code != http.StatusBadRequest || strings.Contains(fmt.Sprint(out), "bad-token") {
		t.Fatalf("rejected token: %d %v", code, out)
	}
	code, out := connect("good-token")
	if code != 200 || strings.Contains(fmt.Sprint(out), "good-token") {
		t.Fatalf("connect: %d %v", code, out)
	}
	first := providerByID(t, h, client, "zrok")
	if !first.Connected || first.EnvID != "env-1" || first.APIURL == "" || first.Enabled {
		t.Fatalf("zrok after connecting (connecting does not turn it on): %+v", first)
	}
	env, err := os.ReadFile(filepath.Join(dir, "zrok-environment", "environment.json"))
	if err != nil || !strings.Contains(string(env), "good-token") {
		t.Fatalf("the token is kept in the managed environment: %v", err)
	}
	if b, _ := os.ReadFile(h.ConfigPath); strings.Contains(string(b), "good-token") {
		t.Fatal("the token leaked into config.json")
	}
	if code, out := setHost(t, h, client, "zrok", true); code != 200 {
		t.Fatalf("turn on zrok: %d %v", code, out)
	}
	// Replace the account with a new token while zrok is on and no flat is
	// public through it; the backend runs on the new account afterwards.
	if code, out := connect("bad-token"); code != http.StatusBadRequest {
		t.Fatalf("rejected replacement: %d %v", code, out)
	}
	if p := providerByID(t, h, client, "zrok"); p.EnvID != "env-1" || !p.Configured {
		t.Fatalf("a rejected replacement changed the account: %+v", p)
	}
	if code, out := connect("new-token"); code != 200 {
		t.Fatalf("replace: %d %v", code, out)
	}
	if p := providerByID(t, h, client, "zrok"); p.EnvID == "env-1" || !p.Connected || !p.Enabled || !p.Configured {
		t.Fatalf("zrok after replacing the account: %+v", p)
	}
	disconnect := func() int {
		var out map[string]any
		return consoleCall(t, h, client, "DELETE", "/console/api/providers/zrok/account", nil, &out)
	}
	if code := disconnect(); code != http.StatusConflict {
		t.Fatalf("disconnect while zrok is on: %d", code)
	}
	if code, out := setHost(t, h, client, "zrok", false); code != 200 {
		t.Fatalf("turn off zrok: %d %v", code, out)
	}
	if code := disconnect(); code != 200 {
		t.Fatalf("disconnect: %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "zrok-environment")); !os.IsNotExist(err) {
		t.Fatalf("managed environment kept: %v", err)
	}
	if !slices.Contains(*calls, "POST /api/v2/disable") {
		t.Fatalf("the environment was not unregistered: %v", *calls)
	}
	if p := providerByID(t, h, client, "zrok"); p.Connected || p.Configured {
		t.Fatalf("zrok after disconnecting: %+v", p)
	}
}

// A host whose zrok grant has no connected account still starts, so the
// operator can connect one in the console.
func TestHostStartsWithZrokGrantAndNoAccount(t *testing.T) {
	h, client := consoleHostWith(t, t.TempDir(), func(o *Options) {
		o.Permit = []string{"zrok"}
		if o.Set == nil {
			o.Set = map[string]bool{}
		}
		o.Set["permit"] = true
	})
	p := providerByID(t, h, client, "zrok")
	if !p.Enabled || p.Configured || p.Connected {
		t.Fatalf("zrok granted without an account: %+v", p)
	}
}

// Cloudflare quick tunnels need no account: the console turns them on in one
// step, which attaches the backend without contacting Cloudflare, and the
// policy token then matches the one the next start computes. Turning them off
// again also matches a start without the grant.
func TestConsoleCloudflareQuickTurnsOnAndPolicyMatchesRestart(t *testing.T) {
	dir := t.TempDir()
	h, client := consoleHostWith(t, dir, nil)
	if p := providerByID(t, h, client, "cloudflare-quick"); p.Enabled || p.Configured || p.Scope != "public" {
		t.Fatalf("cloudflare-quick by default: %+v", p)
	}
	if code, out := setHost(t, h, client, "cloudflare-quick", true); code != 200 {
		t.Fatalf("turn on cloudflare-quick: %d %v", code, out)
	}
	p := providerByID(t, h, client, "cloudflare-quick")
	if !p.Enabled || !p.Configured || p.Status == nil || p.Status.Kind != "cloudflare-quick" {
		t.Fatalf("cloudflare-quick after turning on: %+v", p)
	}
	if got := configPermitted(t, h); !slices.Equal(got, []string{"cloudflare-quick"}) {
		t.Fatalf("config.json network.permitted = %v", got)
	}
	live, err := h.Providers.ExposurePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h, client = consoleHostWith(t, dir, nil)
	restarted, err := h.Providers.ExposurePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if live != restarted {
		t.Fatal("the policy token after a restart differs from the one after turning cloudflare-quick on")
	}
	if code, out := setHost(t, h, client, "cloudflare-quick", false); code != 200 {
		t.Fatalf("turn off cloudflare-quick: %d %v", code, out)
	}
	off, err := h.Providers.ExposurePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h, _ = consoleHostWith(t, dir, nil)
	again, err := h.Providers.ExposurePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if off != again {
		t.Fatal("the policy token after a restart differs from the one after turning cloudflare-quick off")
	}
}

// The NetBird part of the provider configuration token follows the management
// server and is empty without a NetBird grant, so hosts without NetBird keep
// their token.
func TestNetbirdConfigurationToken(t *testing.T) {
	base := &config.Config{}
	base.Netbird = config.NetbirdConfig{ManagementURL: "https://a.example", DeviceName: "flats"}
	if got := netbirdConfiguration(base, nil); got != "" {
		t.Fatalf("without a grant: %q", got)
	}
	base.Network.Permitted = []string{"netbird-expose"}
	first := netbirdConfiguration(base, nil)
	other := *base
	other.Netbird.ManagementURL = "https://b.example"
	if first == "" || netbirdConfiguration(&other, nil) == first {
		t.Fatal("changing netbird.management_url did not change the token")
	}
	if providerConfiguration(portal.Config{}, "", "") == providerConfiguration(portal.Config{}, "", first) {
		t.Fatal("the NetBird part does not reach the provider token")
	}
}

// A NetBird provider turned on before a peer exists runs without a peer. When
// the peer is enrolled afterwards the policy token carries its fingerprint at
// once, as the next start computes it, so a pending approval is not left
// bound to the empty one.
func TestConsoleNetbirdEnrollmentRefreshesPolicy(t *testing.T) {
	provider.RegisterNetbird(fakeNetbirdDriver{})
	t.Cleanup(func() { provider.RegisterNetbird(nil) })
	dir := t.TempDir()
	h, client := consoleHostWith(t, dir, nil)
	if code, out := setHost(t, h, client, "netbird", true); code != 200 {
		t.Fatalf("turn on netbird: %d %v", code, out)
	}
	before, err := h.Providers.ExposurePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	nbDir := filepath.Join(dir, "netbird")
	if err := os.MkdirAll(nbDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nbDir, "account.json"), []byte(`{"management_url":"https://api.netbird.io:443","device_name":"flats","fingerprint":"abc123"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nbDir, "netbird.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.pmu.Lock()
	err = h.peerEnrolledLocked()
	h.pmu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	live, err := h.Providers.ExposurePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if live == before {
		t.Fatal("enrolling the peer did not change the policy token")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h, _ = consoleHostWith(t, dir, nil)
	restarted, err := h.Providers.ExposurePolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if live != restarted {
		t.Fatal("the policy token after a restart differs from the one after enrolling the peer")
	}
}

// fakeNetbirdDriver stands in for the NetBird module, which the main module
// does not link. Its backend reads the account record like the real one.
type fakeNetbirdDriver struct{}

func (fakeNetbirdDriver) New(c provider.NetbirdConfig) (provider.NetbirdNet, error) {
	return &fakeNetbirdNet{dir: c.Dir}, nil
}

func (fakeNetbirdDriver) LoadAccount(dir string) (provider.NetbirdAccount, bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "account.json"))
	if err != nil {
		return provider.NetbirdAccount{}, false, nil
	}
	var a provider.NetbirdAccount
	if err := json.Unmarshal(raw, &a); err != nil {
		return provider.NetbirdAccount{}, false, err
	}
	return a, true, nil
}

func (fakeNetbirdDriver) Enroll(context.Context, string, string, string, string) (provider.NetbirdAccount, error) {
	return provider.NetbirdAccount{}, errors.New("not used")
}
func (fakeNetbirdDriver) Disconnect(context.Context, string, bool) error { return nil }
func (fakeNetbirdDriver) RetireStored(string, string) error              { return nil }
func (fakeNetbirdDriver) DisconnectHint() string                         { return "" }

type fakeNetbirdNet struct{ dir string }

func (n *fakeNetbirdNet) Account() string {
	a, _, _ := fakeNetbirdDriver{}.LoadAccount(n.dir)
	return a.Fingerprint
}
func (*fakeNetbirdNet) ServePrivate(context.Context, string, http.Handler) (string, error) {
	return "", errors.New("not used")
}
func (*fakeNetbirdNet) StopPrivate(string) error      { return nil }
func (*fakeNetbirdNet) RetirePrivate(string) error    { return nil }
func (*fakeNetbirdNet) PrivateURL(string) string      { return "" }
func (*fakeNetbirdNet) PrivateStatus() core.NetStatus { return core.NetStatus{} }
func (*fakeNetbirdNet) PublicURL(string) string       { return "" }
func (*fakeNetbirdNet) PublicStatus() core.NetStatus  { return core.NetStatus{} }
func (*fakeNetbirdNet) StopPublic(string) error       { return nil }
func (*fakeNetbirdNet) RetirePublic(string) error     { return nil }
func (*fakeNetbirdNet) Close() error                  { return nil }
func (*fakeNetbirdNet) ServePublic(context.Context, string, http.Handler) (string, error) {
	return "", errors.New("not used")
}

// Without a linked NetBird, the providers report why they cannot run and the
// peer cannot be enrolled; the host keeps starting.
func TestConsoleNetbirdNotBuilt(t *testing.T) {
	provider.RegisterNetbird(nil)
	dir := t.TempDir()
	h, client := consoleHostWith(t, dir, nil)
	p := providerByID(t, h, client, "netbird")
	if p.Unsupported == "" || p.Connected {
		t.Fatalf("netbird in a build without it: %+v", p)
	}
	if err := h.ConnectNetbird(context.Background(), "key"); !errors.Is(err, core.ErrUnavailable) {
		t.Fatalf("ConnectNetbird without a driver: %v", err)
	}
}
