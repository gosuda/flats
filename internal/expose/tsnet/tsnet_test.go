package tsnet

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/derp/derpserver"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/netns"
	"tailscale.com/net/stun/stuntest"
	"tailscale.com/tailcfg"
	ts "tailscale.com/tsnet"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/nettype"

	"github.com/gosuda/flats/internal/core"
)

const testDomain = "tail-scale.ts.net"

// startControl runs testcontrol plus DERP/STUN on loopback. With https the
// tailnet has MagicDNS and certificate domains; without it, neither.
func startControl(t *testing.T, https bool) *testcontrol.Server {
	t.Helper()
	if testing.Short() {
		t.Skip("starts a tailnet; skipped in -short mode")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback, testcontrol cannot start: %v", err)
	}
	ln.Close()
	netns.SetEnabled(false)
	derpMap := runDERPAndSTUN(t)
	control := &testcontrol.Server{DERPMap: derpMap, MagicDNSDomain: testDomain, Logf: logger.Discard}
	if https {
		control.DNSConfig = &tailcfg.DNSConfig{Proxied: true}
	}
	control.HTTPTestServer = httptest.NewUnstartedServer(control)
	control.HTTPTestServer.Start()
	t.Cleanup(control.HTTPTestServer.Close)
	return control
}

// runDERPAndSTUN mirrors integration.RunDERPAndSTUN without importing that
// package, whose dependency graph would need extra go.mod entries.
func runDERPAndSTUN(t *testing.T) *tailcfg.DERPMap {
	d := derpserver.New(key.NewNode(), logger.Discard)
	srv := httptest.NewUnstartedServer(derpserver.Handler(d))
	srv.Config.ErrorLog = logger.StdLogger(logger.Discard)
	srv.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	srv.StartTLS()
	stunAddr, stunCleanup := stuntest.ServeWithPacketListener(t, nettype.Std{})
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		d.Close()
		stunCleanup()
	})
	return &tailcfg.DERPMap{Regions: map[tailcfg.DERPRegionID]*tailcfg.DERPRegion{
		1: {RegionID: 1, RegionCode: "test", Nodes: []*tailcfg.DERPNode{{
			Name: "t1", RegionID: 1, HostName: "127.0.0.1", IPv4: "127.0.0.1", IPv6: "none",
			STUNPort: stunAddr.Port, DERPPort: srv.Listener.Addr().(*net.TCPAddr).Port,
			InsecureForTests: true, STUNTestIP: "127.0.0.1",
		}}},
	}}
}

// startClient brings up a separate tailnet node to make requests from.
func startClient(t *testing.T, ctx context.Context, controlURL string) (*ts.Server, string) {
	t.Helper()
	c := &ts.Server{
		Dir: filepath.Join(t.TempDir(), "client"), Hostname: "client", ControlURL: controlURL,
		Store: new(mem.Store), Ephemeral: true, UserLogf: logger.Discard,
	}
	t.Cleanup(func() { c.Close() })
	st, err := c.Up(ctx)
	if err != nil {
		t.Fatalf("client Up: %v", err)
	}
	u, ok := st.User[st.Self.UserID]
	if !ok {
		t.Fatalf("client user profile missing from status")
	}
	return c, u.LoginName
}

// testCA returns a cert source for *.testDomain and a pool trusting it.
func testCA(t *testing.T) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "flats test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "*." + testDomain},
		DNSNames:  []string{"*." + testDomain},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert := &tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	var mu sync.Mutex
	var snis []string
	t.Cleanup(func() { t.Logf("certificate requests (SNI): %v", snis) })
	return func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
		mu.Lock()
		snis = append(snis, hi.ServerName)
		mu.Unlock()
		return cert, nil
	}, pool
}

// echo reports the host it serves and the identity headers it received.
func echo(host string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "host=%s\nlogin=%s\nname=%s\nspoof=%s\n", host,
			r.Header.Get("Tailscale-User-Login"), r.Header.Get("Tailscale-User-Name"),
			r.Header.Get("Tailscale-User-Profile-Pic"))
	})
}

func rssMB(t *testing.T) float64 {
	runtime.GC()
	debug.FreeOSMemory()
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Logf("ps: %v", err)
		return 0
	}
	kb, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return kb / 1024
}

func fetch(ctx context.Context, c *http.Client, url string, hdr map[string]string) (*http.Response, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return res, string(b), err
}

func TestServeHTTPSIdentityStop(t *testing.T) {
	control := startControl(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()

	client, login := startClient(t, ctx, control.HTTPTestServer.URL)
	getCert, pool := testCA(t)

	dir := t.TempDir()
	n, err := New(Config{Dir: dir, ControlURL: control.HTTPTestServer.URL, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	n.getCert = getCert
	var warmMu sync.Mutex
	warmed := map[string]bool{}
	n.warmCert = func(_ context.Context, domain string) error {
		warmMu.Lock()
		warmed[domain] = true
		warmMu.Unlock()
		return nil
	}
	defer n.Close()

	if got := n.URL("blog"); got != "https://blog.<tailnet>.ts.net" {
		t.Errorf("URL before any node = %q", got)
	}

	before := rssMB(t)
	hosts := map[string]bool{"blog": false, "shop": false, "blog-preview1": true}
	var wg sync.WaitGroup
	for h, eph := range hosts {
		wg.Go(func() {
			if _, err := n.Serve(ctx, h, echo(h), eph); err != nil {
				t.Errorf("Serve %s: %v", h, err)
			}
		})
	}
	wg.Wait()
	start := time.Now()
	for h := range hosts {
		if err := n.WaitReady(ctx, h); err != nil {
			t.Fatalf("WaitReady: %v (status %+v)", err, n.Status())
		}
	}
	t.Logf("3 nodes ready in %v", time.Since(start))
	shared, err := os.ReadFile(filepath.Join(dir, acmeKeyName))
	if err != nil {
		t.Fatalf("shared ACME key: %v", err)
	}
	for h := range hosts {
		if b, err := os.ReadFile(filepath.Join(dir, h, "certs", acmeKeyName)); err != nil || string(b) != string(shared) {
			t.Errorf("%s does not use the shared ACME account key: %v", h, err)
		}
	}
	after := rssMB(t)
	t.Logf("memory gate: RSS %.1f MB -> %.1f MB for 3 nodes = %.1f MB per node (incl. first-node code paging)",
		before, after, (after-before)/3)

	if got := n.Tailnet(); got != testDomain {
		t.Errorf("Tailnet() = %q, want %q", got, testDomain)
	}
	st := n.Status()
	if len(st.Hosts) != 3 {
		t.Fatalf("Status hosts = %+v", st.Hosts)
	}
	for _, hi := range st.Hosts {
		if hi.State != StateReady {
			t.Errorf("%s state = %s (%s)", hi.Host, hi.State, hi.Detail)
		}
		if want := "https://" + hi.Host + "." + testDomain; hi.URL != want {
			t.Errorf("%s URL = %q, want %q", hi.Host, hi.URL, want)
		}
		if hi.Ephemeral != hosts[hi.Host] {
			t.Errorf("%s ephemeral = %v", hi.Host, hi.Ephemeral)
		}
	}

	hc := &http.Client{
		Transport:     &http.Transport{DialContext: client.Dial, TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer hc.CloseIdleConnections()
	for h := range hosts {
		res, body, err := fetch(ctx, hc, n.URL(h)+"/x", map[string]string{
			"Tailscale-User-Login":       "attacker@example.com",
			"Tailscale-User-Profile-Pic": "spoofed",
		})
		if err != nil {
			t.Fatalf("GET %s: %v", h, err)
		}
		if res.StatusCode != 200 || res.TLS == nil || res.ProtoMajor != 2 {
			t.Errorf("GET %s: status %d tls=%v proto=%s", h, res.StatusCode, res.TLS != nil, res.Proto)
		}
		for _, want := range []string{"host=" + h + "\n", "login=" + login + "\n", "spoof=\n"} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s body %q missing %q", h, body, want)
			}
		}
		if strings.Contains(body, "name=\n") {
			t.Errorf("GET %s: Tailscale-User-Name not set: %q", h, body)
		}
	}

	warmMu.Lock()
	for h := range hosts {
		if !warmed[h+"."+testDomain] {
			t.Errorf("certificate for %s was not fetched at startup (warmed %v)", h, warmed)
		}
	}
	warmMu.Unlock()

	// Port 80 redirects to the HTTPS name.
	res, _, err := fetch(ctx, hc, "http://shop."+testDomain+"/a?b=1", nil)
	if err != nil {
		t.Fatalf("GET http: %v", err)
	}
	if loc := res.Header.Get("Location"); res.StatusCode != http.StatusTemporaryRedirect || loc != "https://shop."+testDomain+"/a?b=1" {
		t.Errorf("http redirect = %d %q", res.StatusCode, loc)
	}

	// Re-serving swaps the handler in place.
	if _, err := n.Serve(ctx, "shop", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "v2") }), false); err != nil {
		t.Fatal(err)
	}
	if _, body, err := fetch(ctx, hc, n.URL("shop"), nil); err != nil || body != "v2" {
		t.Errorf("after handler swap: %q %v", body, err)
	}

	// Stop removes the node: it stops answering and its state is gone.
	if err := n.Stop("blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := n.Stop("blog"); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blog")); !os.IsNotExist(err) {
		t.Errorf("state dir after Stop: %v", err)
	}
	for _, hi := range n.Status().Hosts {
		if hi.Host == "blog" {
			t.Errorf("stopped host still in Status")
		}
	}
	hc.CloseIdleConnections()
	sctx, scancel := context.WithTimeout(ctx, 3*time.Second)
	if res, body, err := fetch(sctx, hc, "https://blog."+testDomain+"/", nil); err == nil {
		t.Errorf("stopped host still answers: %d %q", res.StatusCode, body)
	}
	scancel()
	if _, body, err := fetch(ctx, hc, n.URL("blog-preview1"), nil); err != nil || !strings.Contains(body, "host=blog-preview1") {
		t.Errorf("other host after Stop: %q %v", body, err)
	}

	// Close keeps persistent state (the node returns after a restart) and
	// drops ephemeral state.
	if err := n.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "shop", "tailscaled.state")); err != nil {
		t.Errorf("persistent state after Close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blog-preview1")); !os.IsNotExist(err) {
		t.Errorf("ephemeral dir after Close: %v", err)
	}
	if _, err := n.Serve(ctx, "late", echo("late"), false); err == nil {
		t.Errorf("Serve after Close succeeded")
	}
}

func TestPlainHTTPFallbackAndKeyExpiry(t *testing.T) {
	control := startControl(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	client, login := startClient(t, ctx, control.HTTPTestServer.URL)

	n, err := New(Config{Dir: t.TempDir(), ControlURL: control.HTTPTestServer.URL, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if _, err := n.Serve(ctx, "notes", echo("notes"), false); err != nil {
		t.Fatal(err)
	}
	if err := n.WaitReady(ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	hi := n.Status().Hosts[0]
	if hi.State != StateReady || hi.Detail != HTTPSUnavailable {
		t.Errorf("status = %+v", hi)
	}
	if want := "http://notes." + testDomain; n.URL("notes") != want || hi.URL != want {
		t.Errorf("URL = %q / %q, want %q", n.URL("notes"), hi.URL, want)
	}
	if got := n.URL("other"); got != "http://other."+testDomain {
		t.Errorf("URL guess for unserved host = %q", got)
	}

	hc := &http.Client{Transport: &http.Transport{DialContext: client.Dial}}
	defer hc.CloseIdleConnections()
	res, body, err := fetch(ctx, hc, n.URL("notes")+"/", map[string]string{"Tailscale-User-Login": "x@y"})
	if err != nil || res.StatusCode != 200 || !strings.Contains(body, "login="+login+"\n") {
		t.Fatalf("plain GET: %v %q", err, body)
	}

	// Stop right after Serve (node still starting) must not hang, and the
	// host can be served again from scratch.
	if _, err := n.Serve(ctx, "tmp", echo("tmp"), false); err != nil {
		t.Fatal(err)
	}
	if err := n.Stop("tmp"); err != nil {
		t.Errorf("Stop while starting: %v", err)
	}
	if u, err := n.Serve(ctx, "tmp", echo("tmp2"), false); err != nil || u != "http://tmp."+testDomain {
		t.Fatalf("Serve in a plain-HTTP tailnet = %q, %v", u, err)
	}
	if err := n.WaitReady(ctx, "tmp"); err != nil {
		t.Fatal(err)
	}
	if _, body, err := fetch(ctx, hc, n.URL("tmp"), nil); err != nil || !strings.Contains(body, "host=tmp2") {
		t.Errorf("re-served host: %q %v", body, err)
	}

	// A node that failed to start is retried by serving it again, without
	// Stop (which would log it out and delete its state).
	dir := n.cfg.Dir
	if err := os.WriteFile(filepath.Join(dir, "broken"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Serve(ctx, "broken", echo("broken"), false); err != nil {
		t.Fatal(err)
	}
	if err := n.WaitReady(ctx, "broken"); err == nil {
		t.Fatalf("node with a file as its state dir became ready")
	}
	if err := os.Remove(filepath.Join(dir, "broken")); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Serve(ctx, "broken", echo("fixed"), false); err != nil {
		t.Fatal(err)
	}
	if err := n.WaitReady(ctx, "broken"); err != nil {
		t.Fatalf("re-served failed node: %v (status %+v)", err, n.Status())
	}
	if _, body, err := fetch(ctx, hc, n.URL("broken"), nil); err != nil || !strings.Contains(body, "host=fixed") {
		t.Errorf("restarted node: %q %v", body, err)
	}

	control.SetExpireAllNodes(true)
	deadline := time.Now().Add(20 * time.Second)
	for {
		hi = hostInfo(n, "notes")
		if hi.State == StateNeedsLogin {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state after key expiry = %+v, want needs-login", hi)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("after expiry: state=%s key_expiry=%s detail=%q", hi.State, hi.KeyExpiry, hi.Detail)
	if hi.Detail == "" {
		t.Errorf("needs-login without detail")
	}
}

func TestServeValidation(t *testing.T) {
	n, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	for _, h := range []string{"", "Blog", "a.b", "../x", "-a", strings.Repeat("a", 64)} {
		if _, err := n.Serve(context.Background(), h, echo(h), false); err == nil {
			t.Errorf("Serve(%q) accepted", h)
		}
	}
	if err := n.Stop("unknown"); err != nil {
		t.Errorf("Stop unknown: %v", err)
	}
	if _, err := New(Config{}); err == nil {
		t.Errorf("New without Dir accepted")
	}
}

func TestHostStates(t *testing.T) {
	n := &Net{nodes: map[string]*node{}, suffix: "tail1.ts.net"}
	now := time.Now()
	soon, far, past := now.Add(3*24*time.Hour), now.Add(100*24*time.Hour), now.Add(-time.Minute)
	cases := []struct {
		nd    *node
		state string
		want  string // detail substring
	}{
		{&node{backend: "NoState"}, StateStarting, ""},
		{&node{backend: "Running", serving: true, certOK: true, keyExpiry: &far}, StateReady, ""},
		{&node{backend: "Running", serving: true, certOK: true, keyExpiry: &soon}, StateKeyExpiring, "expires"},
		{&node{backend: "Running", serving: true, keyExpiry: &far}, StateStarting, "HTTPS certificate"},
		{&node{backend: "Running", serving: true, certErr: "acme: rate limited"}, StateStarting, "last attempt: acme: rate limited"},
		{&node{backend: "Running", serving: true, plain: true}, StateReady, "plain HTTP"},
		{&node{backend: "NeedsLogin", authURL: "https://login.example/a"}, StateNeedsLogin, "https://login.example/a"},
		{&node{backend: "NeedsLogin", keyExpiry: &past}, StateNeedsLogin, "expired"},
		{&node{backend: "NeedsMachineAuth"}, StateNeedsLogin, "approval"},
		{&node{backend: "Running", serving: true, certOK: true, err: fmt.Errorf("boom")}, StateError, "boom"},
	}
	for i, c := range cases {
		c.nd.host = "h"
		n.nodes["h"] = c.nd
		hi := n.hostInfoLocked(c.nd, now)
		if hi.State != c.state || !strings.Contains(hi.Detail, c.want) {
			t.Errorf("case %d: %s %q, want %s ~%q", i, hi.State, hi.Detail, c.state, c.want)
		}
		if c.nd.keyExpiry != nil && hi.KeyExpiry != c.nd.keyExpiry.UTC().Format(time.RFC3339) {
			t.Errorf("case %d: KeyExpiry %q", i, hi.KeyExpiry)
		}
	}
	n.nodes["h"].dnsName = "h-1.tail1.ts.net"
	if got := n.URL("h"); got != "https://h-1.tail1.ts.net" {
		t.Errorf("URL with renamed node = %q", got)
	}
	// Once any node fell back to plain HTTP, hosts not serving yet get the
	// same guess; a serving node reports what it actually does.
	n.plain = true
	n.nodes["s"] = &node{host: "s"}
	n.nodes["t"] = &node{host: "t", serving: true}
	for host, want := range map[string]string{"s": "http://s.tail1.ts.net", "t": "https://t.tail1.ts.net", "u": "http://u.tail1.ts.net"} {
		if got := n.URL(host); got != want {
			t.Errorf("URL(%s) = %q, want %q", host, got, want)
		}
	}
}

func TestHeaderValue(t *testing.T) {
	if got := headerValue("alice@example.com"); got != "alice@example.com" {
		t.Errorf("ascii: %q", got)
	}
	if got := headerValue("송인서"); !strings.HasPrefix(got, "=?utf-8?q?") {
		t.Errorf("utf-8: %q", got)
	}
	if got := headerValue("\xff"); got != "" {
		t.Errorf("invalid: %q", got)
	}
}

func hostInfo(n *Net, host string) core.HostInfo {
	for _, hi := range n.Status().Hosts {
		if hi.Host == host {
			return hi
		}
	}
	return core.HostInfo{}
}

func TestIdentityHeaders(t *testing.T) {
	person := &apitype.WhoIsResponse{
		Node:        &tailcfg.Node{},
		UserProfile: &tailcfg.UserProfile{LoginName: "alice@example.com", DisplayName: "Alice"},
	}
	tagged := &apitype.WhoIsResponse{
		Node:        &tailcfg.Node{Tags: []string{"tag:agent"}},
		UserProfile: &tailcfg.UserProfile{LoginName: "tagged-devices", DisplayName: "tagged-devices"},
	}
	cases := []struct {
		label       string
		who         *apitype.WhoIsResponse
		err         error
		login, user string
	}{
		{"user", person, nil, "alice@example.com", "Alice"},
		{"tagged", tagged, nil, "", ""},
		{"unknown peer", nil, errors.New("peer not found"), "", ""},
	}
	for _, c := range cases {
		var got http.Header
		nd := &node{}
		var h http.Handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header.Clone() })
		nd.handler.Store(&h)
		app := (&Net{}).identity(nd, func(context.Context, string) (*apitype.WhoIsResponse, error) { return c.who, c.err })
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Tailscale-User-Login", "mallory@example.com")
		r.Header.Set("Tailscale-User-Name", "Mallory")
		r.Header.Set("Tailscale-User-Profile-Pic", "https://evil.example/p.png")
		r.Header["tailscale_user_login"] = []string{"mallory@example.com"}
		r.Header["TAILSCALE-USER-GROUPS"] = []string{"admins"}
		r.Header.Set("X-Other", "kept")
		app.ServeHTTP(httptest.NewRecorder(), r)
		if v := got.Get("Tailscale-User-Login"); v != c.login {
			t.Errorf("%s: login %q, want %q", c.label, v, c.login)
		}
		if v := got.Get("Tailscale-User-Name"); v != c.user {
			t.Errorf("%s: name %q, want %q", c.label, v, c.user)
		}
		for k := range got {
			if isIdentityHeader(k) && k != "Tailscale-User-Login" && k != "Tailscale-User-Name" {
				t.Errorf("%s: spoofed header %q survived", c.label, k)
			}
		}
		if c.login == "" && (len(got["Tailscale-User-Login"]) > 0 || len(got["Tailscale-User-Name"]) > 0) {
			t.Errorf("%s: identity headers present: %v", c.label, got)
		}
		if got.Get("X-Other") != "kept" {
			t.Errorf("%s: unrelated header dropped", c.label)
		}
	}
}

func TestHTTPSRedirect(t *testing.T) {
	h := httpsRedirect("shop.tail1.ts.net")
	cases := []struct {
		url  *url.URL
		want string
	}{
		{&url.URL{Path: "/a b", RawQuery: "x=1"}, "https://shop.tail1.ts.net/a%20b?x=1"},
		{&url.URL{Path: "//evil.example/x"}, "https://shop.tail1.ts.net//evil.example/x"},
		{&url.URL{Scheme: "http", Opaque: "@evil.example"}, "https://shop.tail1.ts.net/"},
		{&url.URL{Path: "*"}, "https://shop.tail1.ts.net/"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, &http.Request{Method: http.MethodGet, URL: c.url, Header: http.Header{}})
		if loc := w.Header().Get("Location"); w.Code != http.StatusTemporaryRedirect || loc != c.want {
			t.Errorf("%#v: %d %q, want %q", c.url, w.Code, loc, c.want)
		}
		if u, err := url.Parse(w.Header().Get("Location")); err != nil || u.Host != "shop.tail1.ts.net" {
			t.Errorf("%#v: redirect leaves the host: %v %v", c.url, u, err)
		}
	}
}

// A node stopped while waiting for its predecessor's teardown must not finish
// (and let a third node start) before that predecessor has finished deleting
// the shared state directory.
func TestTeardownWaitsForPredecessor(t *testing.T) {
	n, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	prev := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	nd := &node{host: "h", dir: filepath.Join(n.cfg.Dir, "h"), ctx: ctx, cancel: cancel,
		started: make(chan struct{}), done: make(chan struct{}), prev: prev}
	go n.run(nd) // waits for prev, returns on cancel without starting
	ret := make(chan error, 1)
	go func() { ret <- n.teardown(nd, true) }()
	select {
	case err := <-ret:
		t.Fatalf("teardown returned before its predecessor finished: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if nd.srv != nil {
		t.Errorf("node started while its predecessor was tearing down")
	}
	close(prev)
	select {
	case err := <-ret:
		if err != nil {
			t.Errorf("teardown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("teardown did not return after its predecessor finished")
	}
}

// Regression (real-tailnet gate): Let's Encrypt can take longer than one
// attempt; the certificate fetch is retried until it succeeds, and the host
// is only ready once it has its certificate.
func TestWarmRetriesUntilCertificate(t *testing.T) {
	defer func(d time.Duration) { certRetryMin = d }(certRetryMin)
	certRetryMin = 10 * time.Millisecond
	n, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	n.warmCert = func(context.Context, string) error {
		if calls.Add(1) < 3 {
			return errors.New("timed out waiting for the DNS challenge")
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nd := &node{host: "h", ctx: ctx, cancel: cancel, backend: "Running", serving: true}
	n.nodes["h"] = nd
	done := make(chan struct{})
	go func() { n.warm(nd, nil, "h.tail1.ts.net"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("warm did not finish")
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("certificate fetched %d times, want 3", got)
	}
	n.mu.Lock()
	hi := n.hostInfoLocked(nd, time.Now())
	n.mu.Unlock()
	if hi.State != StateReady {
		t.Errorf("state after certificate = %s (%s)", hi.State, hi.Detail)
	}

	// Stopping the node ends the retries.
	calls.Store(0)
	n.warmCert = func(context.Context, string) error { calls.Add(1); return errors.New("no") }
	ctx2, cancel2 := context.WithCancel(context.Background())
	nd2 := &node{host: "g", ctx: ctx2, cancel: cancel2}
	done2 := make(chan struct{})
	go func() { n.warm(nd2, nil, "g.tail1.ts.net"); close(done2) }()
	time.Sleep(50 * time.Millisecond)
	cancel2()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("warm kept retrying after the node stopped")
	}
}

// Regression (real-tailnet gate): a node whose teardown hangs must not block
// Stop (and with it a deploy or delete) indefinitely.
func TestStopIsBounded(t *testing.T) {
	defer func(d time.Duration) { stopWait = d }(stopWait)
	stopWait = 100 * time.Millisecond
	n, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	close(started)
	stuck := make(chan struct{}) // the lifecycle goroutine never returns
	nd := &node{host: "h", dir: filepath.Join(n.cfg.Dir, "h"), ctx: ctx, cancel: cancel, started: started, done: stuck}
	n.nodes["h"] = nd
	start := time.Now()
	if err := n.Stop("h"); err == nil || !strings.Contains(err.Error(), "background") {
		t.Errorf("Stop of a stuck node = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Stop took %v", d)
	}
	if len(n.Status().Hosts) != 0 {
		t.Errorf("stuck node still served: %+v", n.Status().Hosts)
	}
	close(stuck)
	if err := n.Close(); err != nil {
		t.Errorf("Close after the node finished: %v", err)
	}
}

// Regression (real-tailnet gate): Let's Encrypt allows 10 new accounts per
// IP address in 3 hours, so all nodes share one ACME account key; an
// existing node's key (an already registered account) is adopted.
func TestSharedACMEKey(t *testing.T) {
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	keyOf := func(dir string) string { return filepath.Join(dir, "certs", acmeKeyName) }

	// Fresh data: one key is created and handed to every node.
	n, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	dirs := []string{"a", "b", "c", "d"}
	for _, d := range dirs {
		wg.Go(func() {
			if err := n.shareACMEKey(filepath.Join(n.cfg.Dir, d)); err != nil {
				t.Errorf("share %s: %v", d, err)
			}
		})
	}
	wg.Wait()
	shared := read(filepath.Join(n.cfg.Dir, acmeKeyName))
	if !validACMEKey([]byte(shared)) {
		t.Fatalf("shared key is not a usable private key: %q", shared)
	}
	for _, d := range dirs {
		if got := read(keyOf(filepath.Join(n.cfg.Dir, d))); got != shared {
			t.Errorf("node %s has a different account key", d)
		}
		if fi, err := os.Stat(keyOf(filepath.Join(n.cfg.Dir, d))); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("node %s key mode: %v %v", d, fi.Mode(), err)
		}
	}

	// Existing data: a node's registered key becomes the shared key, and a
	// node's own key is never replaced.
	n2, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(n2.cfg.Dir, "blog")
	os.MkdirAll(filepath.Join(old, "certs"), 0o700)
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalECPrivateKey(priv)
	registered := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	os.WriteFile(keyOf(old), []byte(registered), 0o600)
	if err := n2.shareACMEKey(filepath.Join(n2.cfg.Dir, "shop")); err != nil {
		t.Fatal(err)
	}
	if got := read(keyOf(filepath.Join(n2.cfg.Dir, "shop"))); got != registered {
		t.Errorf("existing account key was not adopted")
	}
	if err := n2.shareACMEKey(old); err != nil || read(keyOf(old)) != registered {
		t.Errorf("node's own key changed: %v", err)
	}
}

func TestAuthKeyLoginIsStarting(t *testing.T) {
	n := &Net{cfg: Config{AuthKey: "k"}, nodes: map[string]*node{}}
	nd := &node{host: "h", backend: "NeedsLogin"}
	if hi := n.hostInfoLocked(nd, time.Now()); hi.State != StateStarting || !strings.Contains(hi.Detail, "auth key") {
		t.Errorf("new node with auth key: %s %q", hi.State, hi.Detail)
	}
	nd.wasUp = true // an expired key later is a real needs-login
	if hi := n.hostInfoLocked(nd, time.Now()); hi.State != StateNeedsLogin {
		t.Errorf("re-auth: %s", hi.State)
	}
}

func TestStopPrivateRetainsUnconfirmedRetirement(t *testing.T) {
	oldWait := stopWait
	stopWait = time.Millisecond
	defer func() { stopWait = oldWait }()
	n, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started, runDone := make(chan struct{}), make(chan struct{})
	close(started)
	nd := &node{host: "held", dir: filepath.Join(n.cfg.Dir, "held"), ctx: ctx, cancel: cancel, started: started, done: runDone}
	n.nodes["held"] = nd
	// The retiring backend's lifecycle is held, proving a second attempt cannot
	// turn absence from n.nodes into a successful stop.
	if err := n.StopPrivate("held"); err == nil {
		t.Fatal("first unconfirmed stop accepted")
	}
	if err := n.StopPrivate("held"); err == nil {
		t.Fatal("retry forgot unconfirmed retirement")
	}
	close(runDone)
	n.mu.Lock()
	pending := n.privateStopping["held"]
	n.mu.Unlock()
	<-pending.done
	if err := n.StopPrivate("held"); err != nil {
		t.Fatal("confirmed stop did not settle", err)
	}
	if err := n.StopPrivate("held"); err != nil {
		t.Fatal("settled retry", err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
}
