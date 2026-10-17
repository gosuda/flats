package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
)

// preferredHost starts a host with Tailscale console domains one and two and
// the NetBird console domain ops, all served.
func preferredHost(t *testing.T) (*Host, *fakeTailnet, *fakeNetbirdNet) {
	t.Helper()
	h, tail, nb := netbirdConsoleHost(t)
	for _, d := range []cdKey{{providerTailscale, "one"}, {providerTailscale, "two"}, {providerNetbird, "ops"}} {
		if _, err := h.AddConsoleDomain(context.Background(), d.prov, d.name, ""); err != nil {
			t.Fatal(err)
		}
	}
	return h, tail, nb
}

func viaNetbird(name string) context.Context {
	return context.WithValue(context.Background(), consoleDomainKey{}, cdKey{providerNetbird, name})
}

func preferred(h *Host) string { v, _, _, _ := h.Svc.ConsolePreferred(); return v }

func setPreferred(t *testing.T, h *Host, v string) {
	t.Helper()
	if _, err := h.SetPreferredConsole(context.Background(), v, h.Svc.SettingsETag()); err != nil {
		t.Fatalf("prefer %q: %v", v, err)
	}
}

// Without a preference, links are what they were before the setting existed:
// the request's console domain, else the first served Tailscale domain, then
// NetBird, then loopback.
func TestPreferredConsoleUnsetKeepsLinks(t *testing.T) {
	h, _, _ := netbirdConsoleHost(t)
	ctx := context.Background()
	loop := h.console
	check := func(step string, ctx context.Context, want string) {
		t.Helper()
		if got := h.ConsoleURLFor(ctx); got != want {
			t.Fatalf("%s: link %q, want %q", step, got, want)
		}
	}
	check("no domains", ctx, loop)
	if _, err := h.AddConsoleDomain(ctx, "netbird", "ops", ""); err != nil {
		t.Fatal(err)
	}
	check("NetBird only", ctx, "http://ops.netbird.cloud/")
	for _, n := range []string{"one", "two"} {
		if _, err := h.AddConsoleDomain(ctx, "tailscale", n, ""); err != nil {
			t.Fatal(err)
		}
	}
	check("loopback", ctx, "https://one.tail1234.ts.net")
	check("via two", viaDomain("two"), "https://two.tail1234.ts.net")
	check("via ops", viaNetbird("ops"), "http://ops.netbird.cloud/")
	check("via an unknown domain", viaDomain("unknown"), "https://one.tail1234.ts.net")
	h.setServed(cdKey{providerTailscale, "one"}, false, "")
	check("first Tailscale domain not served", ctx, "https://two.tail1234.ts.net")
	h.setServed(cdKey{providerTailscale, "two"}, false, "")
	check("no Tailscale domain served", ctx, "http://ops.netbird.cloud/")
	if want := []string{"http://ops.netbird.cloud/"}; !slices.Equal(h.ConsoleURLs(), want) {
		t.Fatalf("console URLs %q", h.ConsoleURLs())
	}
	view, err := h.consoleDomains(ctx)
	if err != nil || view.Preferred != "" || view.PreferredState != "" || view.LinkURL != "http://ops.netbird.cloud/" {
		t.Fatalf("view %+v %v", view, err)
	}
}

func TestPreferredConsoleLinks(t *testing.T) {
	h, tail, nb := preferredHost(t)
	ctx := context.Background()
	all := []string{"https://one.tail1234.ts.net", "https://two.tail1234.ts.net", "http://ops.netbird.cloud/"}
	for _, tc := range []struct{ pref, loopback string }{
		{"local", h.console},
		{"tailscale:two", "https://two.tail1234.ts.net"},
		{"netbird:ops", "http://ops.netbird.cloud/"},
		{"tailscale:one", "https://one.tail1234.ts.net"},
	} {
		setPreferred(t, h, tc.pref)
		if got := h.ConsoleURLFor(ctx); got != tc.loopback {
			t.Errorf("%s: loopback link %q, want %q", tc.pref, got, tc.loopback)
		}
		// A request through a served console domain keeps its own.
		if got := h.ConsoleURLFor(viaDomain("two")); got != "https://two.tail1234.ts.net" {
			t.Errorf("%s: link via two %q", tc.pref, got)
		}
		if got := h.ConsoleURLFor(viaNetbird("ops")); got != "http://ops.netbird.cloud/" {
			t.Errorf("%s: link via ops %q", tc.pref, got)
		}
		// Every served console URL is listed whatever the preference.
		if !slices.Equal(h.ConsoleURLs(), all) {
			t.Errorf("%s: console URLs %q", tc.pref, h.ConsoleURLs())
		}
		st := h.Status(ctx).(SystemStatus)
		if st.ConsoleURL != tc.loopback || st.MCPURL != strings.TrimSuffix(tc.loopback, "/")+"/mcp" || !slices.Equal(st.ConsoleURLs, all) {
			t.Errorf("%s: status %q %q %q", tc.pref, st.ConsoleURL, st.MCPURL, st.ConsoleURLs)
		}
		view, err := h.consoleDomains(ctx)
		if err != nil || view.Preferred != tc.pref || view.PreferredState != "served" || view.LinkURL != tc.loopback {
			t.Errorf("%s: view %q %q %q %v", tc.pref, view.Preferred, view.PreferredState, view.LinkURL, err)
		}
	}

	// A preferred domain whose node is served but not ready yet (logging in,
	// getting its certificate) falls back in the fixed order, and the console
	// says it is starting. This is the state right after a domain is added:
	// the served marker is set and the node has a URL.
	setPreferred(t, h, "tailscale:two")
	setStarting := func(name string, on bool) {
		tail.mu.Lock()
		tail.states[name] = map[bool]string{true: "starting"}[on]
		tail.mu.Unlock()
	}
	setStarting("two", true)
	if !h.isServed(cdKey{providerTailscale, "two"}) || h.consoleURLOf(cdKey{providerTailscale, "two"}) == "" {
		t.Fatal("the starting node must keep its served marker and URL, as in production")
	}
	if got := h.ConsoleURLFor(ctx); got != "https://one.tail1234.ts.net" {
		t.Fatalf("preferred domain starting: %q", got)
	}
	if view, _ := h.consoleDomains(ctx); view.PreferredState != "starting" || view.LinkURL != "https://one.tail1234.ts.net" {
		t.Fatalf("starting view %q %q", view.PreferredState, view.LinkURL)
	}
	// Without a preference the fallback keeps today's served test: a
	// starting first domain still gets the links, exactly as on main.
	setPreferred(t, h, "")
	setStarting("one", true)
	if got := h.ConsoleURLFor(ctx); got != "https://one.tail1234.ts.net" {
		t.Fatalf("unset with a starting first domain: %q", got)
	}
	setStarting("one", false)
	setStarting("two", false)
	// The same for a NetBird peer.
	setPreferred(t, h, "netbird:ops")
	nb.mu.Lock()
	nb.starting["ops"] = true
	nb.mu.Unlock()
	if got := h.ConsoleURLFor(ctx); got != "https://one.tail1234.ts.net" {
		t.Fatalf("preferred NetBird peer starting: %q", got)
	}
	if view, _ := h.consoleDomains(ctx); view.PreferredState != "starting" {
		t.Fatalf("NetBird starting view %q", view.PreferredState)
	}
	nb.mu.Lock()
	nb.starting["ops"] = false
	nb.mu.Unlock()
	if got := h.ConsoleURLFor(ctx); got != "http://ops.netbird.cloud/" {
		t.Fatalf("preferred NetBird peer ready: %q", got)
	}

	// A node whose Tailscale key expires soon still serves with its
	// certificate (key-expiring): the preference keeps the links, the console
	// reports it served, not starting.
	setPreferred(t, h, "tailscale:two")
	tail.mu.Lock()
	tail.states["two"] = "key-expiring"
	tail.mu.Unlock()
	if got := h.ConsoleURLFor(ctx); got != "https://two.tail1234.ts.net" {
		t.Fatalf("preferred domain key-expiring: %q", got)
	}
	if view, _ := h.consoleDomains(ctx); view.PreferredState != "served" || view.LinkURL != "https://two.tail1234.ts.net" {
		t.Fatalf("key-expiring view %q %q", view.PreferredState, view.LinkURL)
	}
	tail.mu.Lock()
	tail.states["two"] = ""
	tail.mu.Unlock()

	// A preferred domain that is not served falls back in the fixed order,
	// and the console says so.
	setPreferred(t, h, "tailscale:two")
	h.setServed(cdKey{providerTailscale, "two"}, false, "")
	if got := h.ConsoleURLFor(ctx); got != "https://one.tail1234.ts.net" {
		t.Fatalf("not served: %q", got)
	}
	if view, _ := h.consoleDomains(ctx); view.PreferredState != "not_served" || view.LinkURL != "https://one.tail1234.ts.net" {
		t.Fatalf("not served view %q %q", view.PreferredState, view.LinkURL)
	}
	h.setServed(cdKey{providerTailscale, "one"}, false, "")
	if got := h.ConsoleURLFor(ctx); got != "http://ops.netbird.cloud/" {
		t.Fatalf("no Tailscale domain served: %q", got)
	}
	setPreferred(t, h, "netbird:ops")
	h.setServed(cdKey{providerNetbird, "ops"}, false, "")
	if got := h.ConsoleURLFor(ctx); got != h.console {
		t.Fatalf("nothing served: %q", got)
	}
	h.setServed(cdKey{providerTailscale, "one"}, true, "")
	h.setServed(cdKey{providerTailscale, "two"}, true, "")
	h.setServed(cdKey{providerNetbird, "ops"}, true, "")

	// A request through a console domain being deleted uses the preference.
	if _, _, _, err := h.DeleteConsoleDomain(ctx, "tailscale", "two", ""); err != nil {
		t.Fatal(err)
	}
	if got := h.ConsoleURLFor(viaDomain("two")); got != "http://ops.netbird.cloud/" {
		t.Fatalf("via a deleted domain: %q", got)
	}
	if preferred(h) != "netbird:ops" {
		t.Fatalf("deleting another domain changed the preference: %q", preferred(h))
	}
}

// The deleted preferred domain unsets the preference in the same change.
func TestPreferredConsoleDeleted(t *testing.T) {
	h, tail, client := tailnetHost(t)
	ctx := context.Background()
	for _, n := range []string{"one", "two"} {
		if _, err := h.AddConsoleDomain(ctx, "tailscale", n, ""); err != nil {
			t.Fatal(err)
		}
	}
	setPreferred(t, h, "tailscale:two")
	var out struct{ ETag string }
	if code := consoleCall(t, h, client, "DELETE", "/console/api/console-domains/tailscale/two", nil, &out); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if preferred(h) != "" {
		t.Fatalf("preference after deleting its domain: %q", preferred(h))
	}
	raw, err := os.ReadFile(h.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("preferred_host")) {
		t.Fatalf("config.json keeps the preference:\n%s", raw)
	}
	if out.ETag != h.Svc.SettingsETag() {
		t.Fatal("the list and the preference were not one change")
	}
	if got := h.ConsoleURLFor(ctx); got != "https://one.tail1234.ts.net" {
		t.Fatalf("link after the preference went: %q", got)
	}
	// Deleting the domain the console is open on moves the page to a domain
	// that serves, and a node on an expiring Tailscale key (key-expiring)
	// serves: first without a preference, in the fixed order...
	for _, n := range []string{"three", "four"} {
		if _, err := h.AddConsoleDomain(ctx, "tailscale", n, ""); err != nil {
			t.Fatal(err)
		}
	}
	tail.mu.Lock()
	tail.states["one"] = "key-expiring"
	tail.states["three"] = "starting"
	tail.states["four"] = "key-expiring"
	tail.mu.Unlock()
	_, redirect, _, err := h.DeleteConsoleDomain(viaDomain("three"), "tailscale", "three", "")
	if err != nil || redirect != "https://one.tail1234.ts.net" {
		t.Fatalf("self-delete redirect to a key-expiring domain %q %v", redirect, err)
	}
	// ...then to the preferred domain when it serves, ahead of the order.
	// (The nodes stay up: the test does not run the retirements.)
	setPreferred(t, h, "tailscale:four")
	tail.mu.Lock()
	tail.states["one"] = ""
	tail.mu.Unlock()
	_, redirect, _, err = h.DeleteConsoleDomain(viaDomain("one"), "tailscale", "one", "")
	if err != nil || redirect != "https://four.tail1234.ts.net" {
		t.Fatalf("self-delete redirect to the key-expiring preferred domain %q %v", redirect, err)
	}
}

// Only listed names and the loopback address may be preferred, only from the
// console.
func TestPreferredConsoleRefusals(t *testing.T) {
	h, _, client := tailnetHost(t)
	ctx := context.Background()
	if _, err := h.AddConsoleDomain(ctx, "tailscale", "one", ""); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"https://x", "foo", "tailscale:", "evil.example", "tailscale:two", "netbird:one", "tailscale:One", "http://127.0.0.1:1"} {
		if _, err := h.SetPreferredConsole(ctx, v, ""); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("prefer %q: %v", v, err)
		}
	}
	if preferred(h) != "" {
		t.Fatalf("a refused value was stored: %q", preferred(h))
	}
	if _, err := h.SetPreferredConsole(ctx, "local", "stale-etag"); !errors.Is(err, core.ErrConfigChanged) {
		t.Fatalf("stale ETag: %v", err)
	}
	// Over HTTP: the console sets it; malformed bodies and agents cannot.
	put := func(body string) (int, map[string]any) {
		var out map[string]any
		code := consoleCall(t, h, client, "PUT", "/console/api/console-domains/preferred", strings.NewReader(body), &out)
		return code, out
	}
	if code, out := put(`{"preferred": "evil.example"}`); code != 400 || out["category"] == nil {
		t.Fatalf("malformed: %d %v", code, out)
	}
	if code, out := put(`{"preferred": "tailscale:two"}`); code != 400 || !strings.Contains(out["error"].(string), "two") {
		t.Fatalf("unlisted: %d %v", code, out)
	}
	if code, _ := put(`{}`); code != 400 {
		t.Fatalf("missing value: %d", code)
	}
	if code, out := put(`{"preferred": "tailscale:one"}`); code != 200 || out["etag"] != h.Svc.SettingsETag() || preferred(h) != "tailscale:one" {
		t.Fatalf("set: %d %v %q", code, out, preferred(h))
	}
	if code, _ := put(`{"preferred": ""}`); code != 200 || preferred(h) != "" {
		t.Fatalf("unset: %d %q", code, preferred(h))
	}
	for _, hdr := range []map[string]string{{"X-Flats-Client": "cli"}, {}} {
		req, _ := http.NewRequest("PUT", "http://"+h.Addr()+"/api/console-domains/preferred", strings.NewReader(`{"preferred": "local"}`))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 || preferred(h) != "" {
			t.Fatalf("agent API %v: %d %q", hdr, resp.StatusCode, preferred(h))
		}
	}
	// With the console header but as the CLI, the console route refuses.
	req, _ := http.NewRequest("PUT", "http://"+h.Addr()+"/console/api/console-domains/preferred", strings.NewReader(`{"preferred": "local"}`))
	req.Header.Set("X-Flats-Console", "1")
	req.Header.Set("X-Flats-Client", "cli")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || preferred(h) != "" {
		t.Fatalf("CLI on the console route: %d", resp.StatusCode)
	}
}

// Approval links for agent API, CLI and MCP requests on the loopback address
// use the preference; the Host header never changes them.
func TestPreferredConsoleApprovalLinks(t *testing.T) {
	h, _, client := tailnetHost(t)
	ctx := context.Background()
	for _, n := range []string{"one", "two"} {
		if _, err := h.AddConsoleDomain(ctx, "tailscale", n, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, slug := range []string{"api-site", "cli-site", "mcp-site"} {
		if _, err := h.Svc.CreateFlat(ctx, slug, "", core.ViaAPI); err != nil {
			t.Fatal(err)
		}
	}
	base := "http://" + h.Addr()
	_, port, _ := strings.Cut(h.Addr(), ":")
	deleteVia := func(slug string, cli bool, host string) (int, string) {
		req, _ := http.NewRequest("DELETE", base+"/api/flats/"+slug, nil)
		if cli {
			req.Header.Set("X-Flats-Client", "cli")
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
		if host != "" {
			req.Host = host
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			ApprovalURL string `json:"approval_url"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out.ApprovalURL
	}
	// Unset: the first served domain, as before.
	if code, u := deleteVia("api-site", false, ""); code >= 300 || !strings.HasPrefix(u, "https://one.tail1234.ts.net/approvals/") {
		t.Fatalf("API without a preference: %d %q", code, u)
	}
	setPreferred(t, h, "tailscale:two")
	for _, host := range []string{"", "localhost:" + port} {
		if code, u := deleteVia("api-site", false, host); code >= 300 || !strings.HasPrefix(u, "https://two.tail1234.ts.net/approvals/") {
			t.Fatalf("API (Host %q): %d %q", host, code, u)
		}
		if code, u := deleteVia("cli-site", true, host); code >= 300 || !strings.HasPrefix(u, "https://two.tail1234.ts.net/approvals/") {
			t.Fatalf("CLI (Host %q): %d %q", host, code, u)
		}
	}
	// A Host the management listener does not answer to is refused, not used.
	if code, u := deleteVia("cli-site", true, "evil.example:"+port); code != http.StatusForbidden || u != "" {
		t.Fatalf("foreign Host: %d %q", code, u)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "preferred-console", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: base + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "delete_flat", Arguments: map[string]any{"slug": "mcp-site"}})
	if err != nil || res.IsError {
		t.Fatalf("MCP delete_flat %+v %v", res, err)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var out struct {
		ApprovalURL string `json:"approval_url"`
	}
	if err := json.Unmarshal(b, &out); err != nil || !strings.HasPrefix(out.ApprovalURL, "https://two.tail1234.ts.net/approvals/") {
		t.Fatalf("MCP approval link %s %v", b, err)
	}
	// Through a console domain, the link stays on it.
	r, err := h.Svc.Delete(viaDomain("one"), "api-site", core.ViaAPI, "")
	if err != nil || !strings.HasPrefix(r.ApprovalURL, "https://one.tail1234.ts.net/approvals/") {
		t.Fatalf("approval through one: %q %v", r.ApprovalURL, err)
	}
	// Status over the loopback agrees with the approval links.
	var st struct{ System SystemStatus }
	if code := call(t, "GET", base+"/api/status", nil, &st); code != 200 || st.System.ConsoleURL != "https://two.tail1234.ts.net" || st.System.MCPURL != "https://two.tail1234.ts.net/mcp" ||
		!slices.Equal(st.System.ConsoleURLs, []string{"https://one.tail1234.ts.net", "https://two.tail1234.ts.net"}) {
		t.Fatalf("status %d %+v", code, st.System)
	}
}

// A preference that config.json names but its list does not hold is ignored
// with a warning; it never stops the host from starting.
func TestPreferredConsoleFileEdit(t *testing.T) {
	path := initConfig(t, t.TempDir())
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(raw, []byte("\n}\n"), []byte(",\n  \"console\": {\n    \"preferred_host\": \"tailscale:gone\"\n  }\n}\n"), 1)
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	h, err := Start(context.Background(), configModeOptions(path))
	if err != nil {
		t.Fatalf("a mismatched preference stopped the host: %v", err)
	}
	defer h.Close()
	if !strings.Contains(logs.String(), `warning: console.preferred_host "tailscale:gone"`) {
		t.Fatalf("no warning in the log:\n%s", logs.String())
	}
	if got := h.ConsoleURL(); got != h.console {
		t.Fatalf("link %q, want the loopback address", got)
	}
	view, err := h.consoleDomains(context.Background())
	if err != nil || view.Preferred != "tailscale:gone" || view.PreferredState != "unlisted" {
		t.Fatalf("view %q %q %v", view.Preferred, view.PreferredState, err)
	}
	// The console can clear it.
	if _, err := h.SetPreferredConsole(context.Background(), "", view.ETag); err != nil || preferred(h) != "" {
		t.Fatalf("clear: %v %q", err, preferred(h))
	}
}

// Offline `flats config set` accepts only listed names and unsets the
// preference when its domain leaves the list.
func TestPreferredConsoleOffline(t *testing.T) {
	dir := t.TempDir()
	path := initConfig(t, dir)
	set := func(args ...string) (string, error) {
		return runConfig(t, append(append([]string{"set"}, args...), "--config", path)...)
	}
	stored := func() string {
		l, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		v, _ := l.Doc.Stored(config.PreferredHostKey)
		s, _ := v.(string)
		return s
	}
	for _, v := range []string{"https://x", "foo", "tailscale:", "evil.example", "tailscale:flats"} {
		if _, err := set(config.PreferredHostKey, v); exitCode(err) != ExitConfig {
			t.Errorf("set %q: %v", v, err)
		}
	}
	if _, err := set("network.permitted", "tailscale,netbird"); err != nil {
		t.Fatal(err)
	}
	if _, err := set("tailscale.console_hosts", "flats,admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := set("netbird.console_hosts", "ops"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"local", "netbird:ops", "tailscale:admin"} {
		if _, err := set(config.PreferredHostKey, v); err != nil || stored() != v {
			t.Fatalf("set %q: %v (stored %q)", v, err, stored())
		}
	}
	if _, err := set(config.PreferredHostKey, "netbird:admin"); exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "not in netbird.console_hosts") {
		t.Fatalf("a name of the other network: %v", err)
	}
	// Changing the list without the preferred name unsets it, and says so.
	out, err := set("tailscale.console_hosts", "flats,new")
	if err != nil || stored() != "" || !strings.Contains(out, "unset console.preferred_host (it named tailscale:admin") {
		t.Fatalf("list change: %v %q\n%s", err, stored(), out)
	}
	if _, err := set(config.PreferredHostKey, "netbird:ops"); err != nil {
		t.Fatal(err)
	}
	out, err = runConfig(t, "unset", "netbird.console_hosts", "--config", path)
	if err != nil || stored() != "" || !strings.Contains(out, "it named netbird:ops") {
		t.Fatalf("list unset: %v %q\n%s", err, stored(), out)
	}
	// An unrelated change keeps the preference and prints nothing about it.
	if _, err := set(config.PreferredHostKey, "tailscale:flats"); err != nil {
		t.Fatal(err)
	}
	if out, err := set("system.version_quota", "3"); err != nil || stored() != "tailscale:flats" || strings.Contains(out, "preferred") {
		t.Fatalf("unrelated change: %v %q %s", err, stored(), out)
	}
	if out, err := runConfig(t, "unset", config.PreferredHostKey, "--config", path); err != nil || stored() != "" || strings.Contains(out, "it named") {
		t.Fatalf("unset: %v %q %s", err, stored(), out)
	}
	// validate warns about a hand-edited mismatch but does not fail.
	raw, _ := os.ReadFile(path)
	edited := bytes.Replace(raw, []byte("\n}\n"), []byte(",\n  \"console\": {\n    \"preferred_host\": \"netbird:gone\"\n  }\n}\n"), 1)
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runConfig(t, "validate", "--config", path); err != nil || !strings.Contains(out, "warning: console.preferred_host") || !strings.Contains(out, "is valid") {
		t.Fatalf("validate: %v\n%s", err, out)
	}
}

// A preference change and the deletion of the domain it names never leave a
// preference naming an unlisted domain: with the same ETag one of them is
// refused (412), and without one the deletion either clears the preference
// or the preference is refused as unlisted.
func TestPreferredConsoleRacesDomainDelete(t *testing.T) {
	h, tail, client := tailnetHost(t)
	ctx := context.Background()
	if _, err := h.AddConsoleDomain(ctx, "tailscale", "one", ""); err != nil {
		t.Fatal(err)
	}
	consistent := func(step string) {
		t.Helper()
		l, err := config.Load(h.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		c, err := l.Doc.Effective(nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.PreferredHostProblem() != "" {
			t.Fatalf("%s: dangling preference in config.json: %s", step, c.PreferredHostProblem())
		}
		if v, _, _, ok := h.Svc.ConsolePreferred(); v != "" && !ok {
			t.Fatalf("%s: dangling preference in effect: %q", step, v)
		}
	}
	addTwo := func() {
		t.Helper()
		if _, err := h.AddConsoleDomain(ctx, "tailscale", "two", ""); err != nil {
			t.Fatal(err)
		}
	}

	// Sequentially, through HTTP: a page that loaded the view before the
	// deletion cannot then prefer the deleted domain.
	addTwo()
	var view ConsoleDomainsView
	if code := consoleCall(t, h, client, "GET", "/console/api/console-domains", nil, &view); code != 200 {
		t.Fatalf("view: %d", code)
	}
	if _, _, _, err := h.DeleteConsoleDomain(ctx, "tailscale", "two", view.ETag); err != nil {
		t.Fatal(err)
	}
	base := "http://" + h.Addr()
	req, _ := http.NewRequest("PUT", base+"/console/api/console-domains/preferred", strings.NewReader(`{"preferred": "tailscale:two"}`))
	req.Header.Set("X-Flats-Console", "1")
	req.Header.Set("Origin", base)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"`+view.ETag+`"`)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed || preferred(h) != "" {
		t.Fatalf("stale preference: %d %q", resp.StatusCode, preferred(h))
	}
	consistent("stale page")

	// Concurrently, with and without the ETag.
	for i := 0; i < 20; i++ {
		addTwo()
		etag := ""
		if i%2 == 0 {
			etag = h.Svc.SettingsETag()
		}
		errs := make(chan error, 2)
		start := make(chan struct{})
		go func() {
			<-start
			_, err := h.SetPreferredConsole(ctx, "tailscale:two", etag)
			errs <- err
		}()
		go func() {
			<-start
			_, _, _, err := h.DeleteConsoleDomain(ctx, "tailscale", "two", etag)
			errs <- err
		}()
		close(start)
		e1, e2 := <-errs, <-errs
		failed := 0
		for _, err := range []error{e1, e2} {
			switch {
			case err == nil:
			case etag != "" && errors.Is(err, core.ErrConfigChanged):
				failed++
			case etag == "" && errors.Is(err, core.ErrInvalid): // preferring a name already deleted
				failed++
			default:
				t.Fatalf("round %d: unexpected error %v", i, err)
			}
		}
		if etag != "" && failed != 1 {
			t.Fatalf("round %d: with one ETag exactly one change wins: %v, %v", i, e1, e2)
		}
		if slices.Contains(hosts(h), "two") {
			// The deletion lost: the preference may name two, which is listed.
			if _, _, _, err := h.DeleteConsoleDomain(ctx, "tailscale", "two", ""); err != nil {
				t.Fatal(err)
			}
		}
		consistent("race")
		if preferred(h) != "" {
			t.Fatalf("round %d: preference %q survived the deletion of its domain", i, preferred(h))
		}
		waitFor(t, func() bool { return !tail.isServed("two") && !h.isOwned(cdKey{providerTailscale, "two"}) })
	}
}
