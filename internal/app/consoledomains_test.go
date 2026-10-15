package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// fakeTailnet records the console nodes a host serves and stops.
type fakeTailnet struct {
	mu       sync.Mutex
	suffix   string
	served   map[string]http.Handler
	stops    []string
	stopErr  map[string]error
	serveErr error
	onStop   map[string]func() // what stopping a node closes
}

func newFakeTailnet() *fakeTailnet {
	return &fakeTailnet{suffix: "tail1234.ts.net", served: map[string]http.Handler{}, stopErr: map[string]error{}}
}

func (n *fakeTailnet) Serve(_ context.Context, host string, h http.Handler, _ bool) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.serveErr != nil {
		return "", n.serveErr
	}
	n.served[host] = h
	return "https://" + host + "." + n.suffix, nil
}

func (n *fakeTailnet) Stop(host string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.stops = append(n.stops, host)
	if f := n.onStop[host]; f != nil {
		go f()
	}
	if err := n.stopErr[host]; err != nil {
		return err
	}
	delete(n.served, host)
	return nil
}

func (n *fakeTailnet) URL(host string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return "https://" + host + "." + n.suffix
}

func (n *fakeTailnet) Status() core.NetStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	var st core.NetStatus
	for h := range n.served {
		st.Hosts = append(st.Hosts, core.HostInfo{Host: h, URL: "https://" + h + "." + n.suffix, State: "ready"})
	}
	return st
}

func (n *fakeTailnet) Close() error { return nil }

func (n *fakeTailnet) isServed(host string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.served[host]
	return ok
}

func (n *fakeTailnet) stopped() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.stops)
}

// tailnetHost starts a host whose console nodes run on a fake tailnet, with
// Tailscale permitted.
func tailnetHost(t *testing.T) (*Host, *fakeTailnet, *http.Client) {
	t.Helper()
	h, client := consoleHost(t)
	tail := newFakeTailnet()
	h.pmu.Lock()
	h.testTailnet = tail
	h.pmu.Unlock()
	if _, err := h.Svc.SetPermittedNetworks(append(h.Svc.PermittedNetworks(), "tailscale"), ""); err != nil {
		t.Fatal(err)
	}
	return h, tail, client
}

func viaDomain(name string) context.Context {
	return context.WithValue(context.Background(), consoleDomainKey{}, name)
}

func hosts(h *Host) []string { l, _ := h.Svc.ConsoleHosts(); return l }

func TestConsoleDomainsAddAndDelete(t *testing.T) {
	h, tail, client := tailnetHost(t)
	ctx := context.Background()

	var added struct{ ETag string }
	if code := consoleCall(t, h, client, "POST", "/console/api/console-domains", strings.NewReader(`{"name": "flats"}`), &added); code != 200 || added.ETag == "" {
		t.Fatalf("add: %d %+v", code, added)
	}
	if !tail.isServed("flats") || !slices.Equal(hosts(h), []string{"flats"}) {
		t.Fatalf("after add: served %v list %v", tail.isServed("flats"), hosts(h))
	}
	if _, err := h.AddConsoleDomain(ctx, "flats-2", h.Svc.SettingsETag()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hosts(h), []string{"flats", "flats-2"}) {
		t.Fatalf("order: %v", hosts(h))
	}
	var view ConsoleDomainsView
	if code := consoleCall(t, h, client, "GET", "/console/api/console-domains", nil, &view); code != 200 {
		t.Fatalf("view: %d", code)
	}
	if len(view.Domains) != 2 || view.Domains[0].URL != "https://flats.tail1234.ts.net" || view.Domains[0].State != "ready" || !view.Loopback || !view.TailscaleOn {
		t.Fatalf("view = %+v", view)
	}
	nodes, _ := h.Store.ListConsoleNodes(ctx)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %+v", nodes)
	}

	// Refusals change nothing.
	for name, want := range map[string]error{"Flats": core.ErrInvalid, "a.b": core.ErrInvalid, "flats": core.ErrConflict} {
		if _, err := h.AddConsoleDomain(ctx, name, ""); !errors.Is(err, want) {
			t.Errorf("add %q: %v", name, err)
		}
	}
	if _, err := h.AddConsoleDomain(ctx, "other", "stale-etag"); !errors.Is(err, core.ErrConfigChanged) || tail.isServed("other") {
		t.Fatalf("stale ETag: %v served=%v", err, tail.isServed("other"))
	}

	// Delete from loopback: the list first, then the node.
	if code := consoleCall(t, h, client, "DELETE", "/console/api/console-domains/flats", nil, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if tail.isServed("flats") || !slices.Equal(hosts(h), []string{"flats-2"}) || !slices.Contains(tail.stopped(), "flats") {
		t.Fatalf("after delete: list %v stops %v", hosts(h), tail.stopped())
	}
	if nodes, _ := h.Store.ListConsoleNodes(ctx); len(nodes) != 1 || nodes[0].Name != "flats-2" {
		t.Fatalf("nodes = %+v", nodes)
	}
	// The last domain: a console reached through it refuses, loopback may.
	if _, _, _, err := h.DeleteConsoleDomain(viaDomain("flats-2"), "flats-2", ""); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("last via domain: %v", err)
	}
	if _, _, _, err := h.DeleteConsoleDomain(ctx, "flats-2", ""); err != nil || len(hosts(h)) != 0 {
		t.Fatalf("last via loopback: %v %v", err, hosts(h))
	}
	if _, _, _, err := h.DeleteConsoleDomain(ctx, "flats-2", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestConsoleDomainNamesAndFlats(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	if _, err := h.Svc.CreateFlat(ctx, "blog", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddConsoleDomain(ctx, "blog", ""); !errors.Is(err, core.ErrConflict) || tail.isServed("blog") {
		t.Fatalf("a flat's name: %v", err)
	}
	if _, err := h.AddConsoleDomain(ctx, "admin", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Svc.CreateFlat(ctx, "admin", "", core.ViaAPI); err == nil {
		t.Fatal("a flat took a console domain")
	}
	if _, err := h.Svc.RenameSlug(ctx, "blog", "admin", core.ViaAPI); err == nil {
		t.Fatal("a rename took a console domain")
	}
	// Turning Tailscale off is refused while the console runs on it.
	if _, err := h.SetNetworkProvider(ctx, "tailscale", false, ""); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("tailscale off: %v", err)
	}
}

// Deleting the domain a request came through answers first and logs the node
// out afterwards, and points the browser at a remaining domain.
func TestConsoleDomainSelfDelete(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	for _, n := range []string{"a", "b"} {
		if _, err := h.AddConsoleDomain(ctx, n, ""); err != nil {
			t.Fatal(err)
		}
	}
	_, redirect, after, err := h.DeleteConsoleDomain(viaDomain("a"), "a", "")
	if err != nil || redirect != "https://b.tail1234.ts.net" || after == nil {
		t.Fatalf("self delete: %q %v %v", redirect, after != nil, err)
	}
	if slices.Contains(tail.stopped(), "a") {
		t.Fatal("node stopped before the response")
	}
	after()
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(tail.stopped(), "a") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !slices.Contains(tail.stopped(), "a") || !slices.Equal(hosts(h), []string{"b"}) {
		t.Fatalf("after: stops %v list %v", tail.stopped(), hosts(h))
	}
}

// Deleting a domain through itself over HTTP: the browser gets the whole
// response, with the remaining domain, before the node goes away.
func TestConsoleDomainSelfDeleteOverHTTP(t *testing.T) {
	old := selfRetireDelay
	selfRetireDelay = 200 * time.Millisecond
	t.Cleanup(func() { selfRetireDelay = old })
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	for _, n := range []string{"a", "b"} {
		if _, err := h.AddConsoleDomain(ctx, n, ""); err != nil {
			t.Fatal(err)
		}
	}
	tail.mu.Lock()
	handler := tail.served["a"]
	tail.mu.Unlock()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	tail.mu.Lock()
	// Like tsnet's retirement, stopping closes active connections at once, so
	// a node stopped before the response is out loses the response.
	tail.onStop = map[string]func(){"a": func() { srv.Config.Close() }}
	tail.mu.Unlock()
	req, _ := http.NewRequest("DELETE", srv.URL+"/console/api/console-domains/a", nil)
	req.Host = "a.tail1234.ts.net"
	req.Header.Set("X-Flats-Console", "1")
	req.Header.Set("Origin", "https://a.tail1234.ts.net")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Redirect string }
	err = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || out.Redirect != "https://b.tail1234.ts.net" {
		t.Fatalf("response %d %+v %v", resp.StatusCode, out, err)
	}
	// The node is still up when the browser has the answer, and stops after.
	if slices.Contains(tail.stopped(), "a") {
		t.Fatal("the node was stopped before the response could reach the browser")
	}
	waitFor(t, func() bool { return slices.Contains(tail.stopped(), "a") })
}

// A node whose logout fails stays reserved and is retried.
func TestConsoleDomainFailedLogout(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	for _, n := range []string{"keep", "gone"} {
		if _, err := h.AddConsoleDomain(ctx, n, ""); err != nil {
			t.Fatal(err)
		}
	}
	tail.mu.Lock()
	tail.stopErr["gone"] = errors.New("logout refused")
	tail.mu.Unlock()
	if _, _, _, err := h.DeleteConsoleDomain(ctx, "gone", ""); !errors.Is(err, core.ErrUnavailable) {
		t.Fatalf("failed logout: %v", err)
	}
	if !slices.Equal(hosts(h), []string{"keep"}) {
		t.Fatalf("list = %v", hosts(h))
	}
	nodes, _ := h.Store.ListConsoleNodes(ctx)
	if len(nodes) != 2 || nodes[0].Name != "gone" || !nodes[0].Retiring || !strings.Contains(nodes[0].LastError, "logout refused") {
		t.Fatalf("nodes = %+v", nodes)
	}
	if _, err := h.Svc.CreateFlat(ctx, "gone", "", core.ViaAPI); err == nil {
		t.Fatal("a flat took a retiring console node's name")
	}
	if _, err := h.AddConsoleDomain(ctx, "gone", ""); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("re-adding a retiring name: %v", err)
	}
	var view ConsoleDomainsView
	v, _ := h.ConsoleDomains(ctx)
	view = v.(ConsoleDomainsView)
	if len(view.Retiring) != 1 || view.Retiring[0].Error == "" {
		t.Fatalf("view retiring = %+v", view.Retiring)
	}
	tail.mu.Lock()
	delete(tail.stopErr, "gone")
	tail.mu.Unlock()
	if _, _, _, err := h.DeleteConsoleDomain(ctx, "gone", ""); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if nodes, _ := h.Store.ListConsoleNodes(ctx); len(nodes) != 1 {
		t.Fatalf("after retry: %+v", nodes)
	}
	if _, err := h.Svc.CreateFlat(ctx, "gone", "", core.ViaAPI); err != nil {
		t.Fatalf("name free again: %v", err)
	}
}

// A failed save after the node started stops it again, and the name is free.
func TestConsoleDomainAddRollsBack(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	tail.serveErr = errors.New("teardown still pending")
	if _, err := h.AddConsoleDomain(ctx, "rolled", ""); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("serve error: %v", err)
	}
	if nodes, _ := h.Store.ListConsoleNodes(ctx); len(nodes) != 0 || len(hosts(h)) != 0 {
		t.Fatalf("left behind: %+v %v", nodes, hosts(h))
	}
	tail.serveErr = nil
	// config.json changed on disk: the save fails after the node started.
	raw, err := os.ReadFile(h.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.ConfigPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddConsoleDomain(ctx, "rolled", ""); err == nil {
		t.Fatal("added although config.json changed on disk")
	}
	if tail.isServed("rolled") || !slices.Contains(tail.stopped(), "rolled") {
		t.Fatalf("node not stopped after the failed save: stops %v", tail.stopped())
	}
	if nodes, _ := h.Store.ListConsoleNodes(ctx); len(nodes) != 0 {
		t.Fatalf("record left: %+v", nodes)
	}
	if err := os.WriteFile(h.ConfigPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Svc.CreateFlat(ctx, "rolled", "", core.ViaAPI); err != nil {
		t.Fatalf("name not released: %v", err)
	}
}

// Deleting a domain that was never served (its name belongs to a flat) only
// removes it from the list: the flat's tailnet node is left alone.
func TestConsoleDomainDeleteConflictKeepsTheFlatsNode(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	if _, err := h.Svc.CreateFlat(ctx, "blog", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Svc.SetConsoleHosts([]string{"keep", "blog"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.startConsoleDomains(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.DeleteConsoleDomain(ctx, "blog", ""); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(tail.stopped(), "blog") || !slices.Equal(hosts(h), []string{"keep"}) {
		t.Fatalf("stops %v list %v", tail.stopped(), hosts(h))
	}
}

// A name listed again offline while its old node's logout is unfinished is
// served only after that logout succeeds.
func TestConsoleDomainRetiringAndListedAtStart(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	if err := h.Store.PutConsoleNode(ctx, store.ConsoleNode{Name: "again", Retiring: true, LastError: "logout refused"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Svc.SetConsoleHosts([]string{"again"}, ""); err != nil {
		t.Fatal(err)
	}
	tail.mu.Lock()
	tail.stopErr["again"] = errors.New("logout refused")
	tail.mu.Unlock()
	if err := h.startConsoleDomains(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return slices.Contains(tail.stopped(), "again") })
	if tail.isServed("again") {
		t.Fatal("served before its old node was logged out")
	}
	tail.mu.Lock()
	delete(tail.stopErr, "again")
	tail.mu.Unlock()
	if err := h.startConsoleDomains(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return tail.isServed("again") && h.isServed("again") })
	if nodes, _ := h.Store.ListConsoleNodes(ctx); len(nodes) != 1 || nodes[0].Retiring {
		t.Fatalf("nodes = %+v", nodes)
	}
	// The host's own state agrees: served, no conflict, and links use it.
	time.Sleep(50 * time.Millisecond)
	v, _ := h.ConsoleDomains(ctx)
	if d := v.(ConsoleDomainsView).Domains[0]; d.Conflict != "" || !h.isServed("again") || h.ConsoleURL() != "https://again.tail1234.ts.net" {
		t.Fatalf("after the logout: %+v served=%v url=%q", d, h.isServed("again"), h.ConsoleURL())
	}
}

// Deleting the domain a request came through needs another ready domain.
func TestConsoleDomainSelfDeleteNeedsReadyDomain(t *testing.T) {
	h, _, _ := tailnetHost(t)
	ctx := context.Background()
	if _, err := h.Svc.CreateFlat(ctx, "blog", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddConsoleDomain(ctx, "admin", ""); err != nil {
		t.Fatal(err)
	}
	// blog is listed but not served: it belongs to a flat.
	if _, err := h.Svc.SetConsoleHosts([]string{"admin", "blog"}, ""); err != nil {
		t.Fatal(err)
	}
	h.setServed("blog", false, "used by the flat")
	if _, _, _, err := h.DeleteConsoleDomain(viaDomain("admin"), "admin", ""); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("self delete without a ready domain: %v", err)
	}
	if !slices.Contains(hosts(h), "admin") {
		t.Fatal("the list changed")
	}
}

// Only a request on a loopback address may empty the list.
func TestRequestOnLoopback(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:7878": true, "[::1]:7878": true, "192.168.0.10:7878": false, "100.64.1.2:7878": false} {
		la, _ := net.ResolveTCPAddr("tcp", addr)
		ctx := context.WithValue(context.Background(), http.LocalAddrContextKey, la)
		if got := requestOnLoopback(ctx); got != want {
			t.Errorf("%s: %v", addr, got)
		}
	}
	if requestOnLoopback(viaDomain("flats")) {
		t.Error("a console domain request counted as loopback")
	}
	h, _, _ := tailnetHost(t)
	if _, err := h.AddConsoleDomain(context.Background(), "only", ""); err != nil {
		t.Fatal(err)
	}
	lan, _ := net.ResolveTCPAddr("tcp", "192.168.0.10:7878")
	if _, _, _, err := h.DeleteConsoleDomain(context.WithValue(context.Background(), http.LocalAddrContextKey, lan), "only", ""); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("emptying the list from a LAN address: %v", err)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// At start, a listed name another owner holds is not served, and a node no
// longer listed (removed offline) is logged out.
func TestConsoleDomainsAtStart(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	if _, err := h.Svc.CreateFlat(ctx, "blog", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := h.Store.PutConsoleNode(ctx, store.ConsoleNode{Name: "old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Svc.SetConsoleHosts([]string{"blog", "fresh"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.startConsoleDomains(ctx); err != nil {
		t.Fatal(err)
	}
	if tail.isServed("blog") || !tail.isServed("fresh") {
		t.Fatalf("served blog=%v fresh=%v", tail.isServed("blog"), tail.isServed("fresh"))
	}
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(tail.stopped(), "old") {
		if time.Now().After(deadline) {
			t.Fatal("the removed node was not logged out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	v, _ := h.ConsoleDomains(ctx)
	view := v.(ConsoleDomainsView)
	if view.Domains[0].Conflict == "" || view.Domains[1].Conflict != "" {
		t.Fatalf("conflicts = %+v", view.Domains)
	}
	if got := h.ConsoleURL(); got != "https://fresh.tail1234.ts.net" {
		t.Fatalf("console URL %q", got)
	}
}

// Links follow the console domain a request came through.
func TestConsoleURLFollowsTheRequest(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	loop := h.ConsoleURL()
	if !strings.HasPrefix(loop, "http://127.0.0.1:") || h.ConsoleURLFor(ctx) != loop {
		t.Fatalf("loopback console %q", loop)
	}
	for _, n := range []string{"one", "two"} {
		if _, err := h.AddConsoleDomain(ctx, n, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.ConsoleURLFor(viaDomain("two")); got != "https://two.tail1234.ts.net" {
		t.Fatalf("via two: %q", got)
	}
	if got := h.ConsoleURLFor(ctx); got != "https://one.tail1234.ts.net" {
		t.Fatalf("loopback request: %q", got)
	}
	if got := h.ConsoleURLFor(viaDomain("unknown")); got != "https://one.tail1234.ts.net" {
		t.Fatalf("unknown domain: %q", got)
	}
	// The tailnet name is learned after start.
	tail.mu.Lock()
	tail.suffix = "tail9.ts.net"
	tail.mu.Unlock()
	if got := h.ConsoleURL(); got != "https://one.tail9.ts.net" {
		t.Fatalf("after the tailnet is known: %q", got)
	}
	st := h.Status(viaDomain("two")).(SystemStatus)
	if st.ConsoleURL != "https://two.tail9.ts.net" || st.MCPURL != "https://two.tail9.ts.net/mcp" || len(st.ConsoleURLs) != 2 {
		t.Fatalf("status = %+v", st)
	}
	// An approval requested through domain two links there.
	if _, err := h.Svc.CreateFlat(ctx, "site", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	res, err := h.Svc.Delete(viaDomain("two"), "site", core.ViaMCP, "")
	if err != nil || !strings.HasPrefix(res.ApprovalURL, "https://two.tail9.ts.net/approvals/") {
		t.Fatalf("approval link %q %v", res.ApprovalURL, err)
	}
}

// The console node handler accepts only that node's names and records the
// domain for the request.
func TestConsoleNodeHandler(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	if _, err := h.AddConsoleDomain(context.Background(), "admin", ""); err != nil {
		t.Fatal(err)
	}
	tail.mu.Lock()
	handler := tail.served["admin"]
	tail.mu.Unlock()
	for host, want := range map[string]int{"admin.tail1234.ts.net": 200, "admin": 200, "other.tail1234.ts.net": 403} {
		req, _ := http.NewRequest("GET", "http://"+host+"/api/status", nil)
		rec := &recorder{header: http.Header{}}
		handler.ServeHTTP(rec, req)
		if rec.code != want {
			t.Errorf("Host %s: %d, want %d", host, rec.code, want)
		}
	}
}

type recorder struct {
	header http.Header
	code   int
	body   strings.Builder
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = 200
	}
	return r.body.Write(b)
}
func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}
