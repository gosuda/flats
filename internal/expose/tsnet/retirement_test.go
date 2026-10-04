package tsnet

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// controlProxy can make the disposable test control server unreachable while
// leaving its listening address stable. That reproduces a real logout failure
// and lets a later backend retry against the same control URL.
type controlProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []net.Conn
	paused bool
}

func startControlProxy(t *testing.T, target string) *controlProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &controlProxy{ln: ln, target: target}
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			if p.paused {
				p.mu.Unlock()
				client.Close()
				continue
			}
			upstream, err := net.Dial("tcp", target)
			if err != nil {
				p.mu.Unlock()
				client.Close()
				continue
			}
			p.conns = append(p.conns, client, upstream)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
			go func() { _, _ = io.Copy(client, upstream); _ = client.Close() }()
		}
	}()
	t.Cleanup(func() {
		p.pause()
		_ = ln.Close()
	})
	return p
}

func (p *controlProxy) pause() {
	p.mu.Lock()
	p.paused = true
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (p *controlProxy) resume() {
	p.mu.Lock()
	p.paused = false
	p.mu.Unlock()
}

func newProxiedNet(t *testing.T) (*Net, *controlProxy, string) {
	t.Helper()
	control := startControl(t, false)
	u, err := url.Parse(control.HTTPTestServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := startControlProxy(t, u.Host)
	dir := t.TempDir()
	n, err := New(Config{Dir: dir, ControlURL: "http://" + proxy.ln.Addr().String(), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return n, proxy, dir
}

func TestStopPrivateRetriesLogoutWithFreshBackend(t *testing.T) {
	n, proxy, dir := newProxiedNet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := n.Serve(ctx, "revoke", echo("revoke"), false); err != nil {
		t.Fatal(err)
	}
	if err := n.WaitReady(ctx, "revoke"); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	oldClient, err := n.nodes["revoke"].srv.LocalClient()
	n.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	proxy.pause()
	if err := n.StopPrivate("revoke"); err == nil {
		t.Fatal("StopPrivate succeeded while control was unavailable")
	}
	if _, err := os.Stat(filepath.Join(dir, "revoke", "tailscaled.state")); err != nil {
		t.Fatalf("failed logout discarded the persistent node key: %v", err)
	}
	if _, err := n.Serve(ctx, "revoke", echo("must-stay-closed"), false); err == nil {
		t.Fatal("failed retirement allowed the private route to reopen")
	}
	statusCtx, statusCancel := context.WithTimeout(context.Background(), time.Second)
	_, statusErr := oldClient.StatusWithoutPeers(statusCtx)
	statusCancel()
	if statusErr == nil {
		t.Fatal("failed logout left the consumed backend running")
	}

	proxy.resume()
	if err := n.StopPrivate("revoke"); err != nil {
		t.Fatalf("same-process retry after control recovery: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "revoke")); !os.IsNotExist(err) {
		t.Fatalf("confirmed retirement retained state: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStopRetriesLogoutWithFreshBackend(t *testing.T) {
	for _, ephemeral := range []bool{false, true} {
		name := "persistent"
		if ephemeral {
			name = "ephemeral"
		}
		t.Run(name, func(t *testing.T) {
			n, proxy, dir := newProxiedNet(t)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			if _, err := n.Serve(ctx, name, echo(name), ephemeral); err != nil {
				t.Fatal(err)
			}
			if err := n.WaitReady(ctx, name); err != nil {
				t.Fatal(err)
			}
			proxy.pause()
			if err := n.Stop(name); err == nil {
				t.Fatal("Stop succeeded while control was unavailable")
			}
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Fatalf("failed Stop discarded retry state: %v", err)
			}
			proxy.resume()
			if err := n.Stop(name); err != nil {
				t.Fatalf("same-process Stop retry after control recovery: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Fatalf("confirmed Stop retained state: %v", err)
			}
			if err := n.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExpiredKeyRetirementRetriesWithoutReachingRunning(t *testing.T) {
	control := startControl(t, false)
	u, err := url.Parse(control.HTTPTestServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := startControlProxy(t, u.Host)
	dir := t.TempDir()
	n, err := New(Config{Dir: dir, ControlURL: "http://" + proxy.ln.Addr().String(), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := n.Serve(ctx, "expired", echo("expired"), false); err != nil {
		t.Fatal(err)
	}
	if err := n.WaitReady(ctx, "expired"); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	identityBytes := len(n.nodes["expired"].identityState)
	n.mu.Unlock()
	if identityBytes == 0 {
		t.Fatal("running node did not capture its persisted identity state")
	}
	control.SetExpireAllNodes(true)
	deadline := time.Now().Add(20 * time.Second)
	for hostInfo(n, "expired").State != StateNeedsLogin {
		if time.Now().After(deadline) {
			t.Fatalf("expired node state = %+v", hostInfo(n, "expired"))
		}
		time.Sleep(50 * time.Millisecond)
	}

	proxy.pause()
	if err := n.StopPrivate("expired"); err == nil {
		t.Fatal("expired-key logout succeeded while control was unavailable")
	}
	proxy.resume()
	if err := n.StopPrivate("expired"); err != nil {
		t.Fatalf("fresh backend could not retire an expired key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "expired")); !os.IsNotExist(err) {
		t.Fatalf("expired identity remained after confirmed retry: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStopRetiresPersistedOnlyFunnelIdentityAfterRestart(t *testing.T) {
	control := startControl(t, true)
	u, err := url.Parse(control.HTTPTestServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := startControlProxy(t, u.Host)
	dir := t.TempDir()
	getCert, _ := testCA(t)
	newNet := func() *Net {
		n, err := New(Config{
			Dir: dir, ControlURL: "http://" + proxy.ln.Addr().String(),
			GetCertificate: getCert, Logf: t.Logf,
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	first := newNet()
	if _, err := first.ServeFunnel(ctx, "persisted-only", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	waitFunnelState(t, first, "persisted-only", FunnelReady)
	before := stableNodeID(t, first, "persisted-only")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "persisted-only", "tailscaled.state")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("graceful restart discarded the persisted identity: %v", err)
	}

	restarted := newNet()
	proxy.pause()
	if err := restarted.Stop("persisted-only"); err == nil {
		t.Fatal("persisted-only retirement succeeded while control was unavailable")
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("failed persisted-only retirement discarded the retry identity: %v", err)
	}
	if _, err := restarted.ServeFunnel(ctx, "persisted-only", http.NotFoundHandler()); err == nil {
		t.Fatal("failed persisted-only retirement allowed Funnel to reopen")
	}
	proxy.resume()
	if err := restarted.Stop("persisted-only"); err != nil {
		t.Fatalf("persisted-only retirement retry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "persisted-only")); !os.IsNotExist(err) {
		t.Fatalf("confirmed persisted-only retirement retained state: %v", err)
	}

	if _, err := restarted.ServeFunnel(ctx, "persisted-only", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	waitFunnelState(t, restarted, "persisted-only", FunnelReady)
	if after := stableNodeID(t, restarted, "persisted-only"); after == before {
		t.Fatalf("recreated slug reused retired identity %q", after)
	}
	if err := restarted.Stop("persisted-only"); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseDoesNotDeadlockWithPrivateListenCancellation(t *testing.T) {
	control := startControl(t, false)
	n, err := New(Config{Dir: t.TempDir(), ControlURL: control.HTTPTestServer.URL, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	n.beforePrivateListen = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	if _, err := n.Serve(t.Context(), "close-listen-race", echo("race"), false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("private listen did not reach the forced interleaving")
	}
	n.mu.Lock()
	nd := n.nodes["close-listen-race"]
	n.mu.Unlock()
	if nd == nil {
		t.Fatal("node disappeared before close")
	}
	closed := make(chan error, 1)
	go func() { closed <- n.Close() }()
	select {
	case <-nd.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the node")
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked with private listener setup")
	}
}

func TestNeverStartedNodeWithoutStateRetiresConfirmed(t *testing.T) {
	n, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	host := "never-started"
	dir := filepath.Join(n.cfg.Dir, host)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "acme-account.key.pem"), []byte("not identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started, done := make(chan struct{}), make(chan struct{})
	close(started)
	close(done)
	nd := &node{host: host, dir: dir, ctx: ctx, cancel: cancel, started: started, done: done}
	n.nodes[host] = nd
	if err := n.Stop(host); err != nil {
		t.Fatalf("no-state retirement was not confirmed: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("no-state directory remained: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFailedRetirementRefusesFunnelUntilRetry(t *testing.T) {
	n, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	host := "retiring"
	dir := filepath.Join(n.cfg.Dir, host)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tailscaled.state"), []byte("identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started, done := make(chan struct{}), make(chan struct{})
	close(started)
	close(done)
	nd := &node{host: host, dir: dir, ctx: ctx, cancel: cancel, started: started, done: done, backendStarted: true}
	n.nodes[host] = nd
	n.logoutNode = func(context.Context, *node) error { return errors.New("control unavailable") }
	if err := n.Stop(host); err == nil {
		t.Fatal("failed retirement reported success")
	}
	if _, err := n.ServeFunnel(t.Context(), host, http.NotFoundHandler()); err == nil {
		t.Fatal("Funnel reopened while retirement was failed")
	}
	n.logoutNode = func(context.Context, *node) error { return nil }
	if err := n.Stop(host); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseWaitsForAndClosesFailedRetirementBackend(t *testing.T) {
	n, proxy, dir := newProxiedNet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := n.Serve(ctx, "closing", echo("closing"), false); err != nil {
		t.Fatal(err)
	}
	if err := n.WaitReady(ctx, "closing"); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	oldClient, err := n.nodes["closing"].srv.LocalClient()
	n.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	proxy.pause()
	if err := n.Stop("closing"); err == nil {
		t.Fatal("Stop succeeded while control was unavailable")
	}
	if err := n.Close(); err == nil || !strings.Contains(err.Error(), "logout") {
		t.Fatalf("Close did not report the retained failed retirement: %v", err)
	}
	statusCtx, statusCancel := context.WithTimeout(context.Background(), time.Second)
	_, statusErr := oldClient.StatusWithoutPeers(statusCtx)
	statusCancel()
	if statusErr == nil {
		t.Fatal("Net.Close left a retired backend running")
	}
	if _, err := os.Stat(filepath.Join(dir, "closing", "tailscaled.state")); err != nil {
		t.Fatalf("unconfirmed retirement discarded its retry key: %v", err)
	}
}

func TestConcurrentStopsUseOneTeardown(t *testing.T) {
	n, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started, done := make(chan struct{}), make(chan struct{})
	close(started)
	close(done)
	nd := &node{host: "shared", dir: filepath.Join(n.cfg.Dir, "shared"), ctx: ctx, cancel: cancel, started: started, done: done}
	n.nodes[nd.host] = nd
	nd.privateMu.Lock()
	var logouts atomic.Int32
	n.logoutNode = func(context.Context, *node) error {
		logouts.Add(1)
		return nil
	}
	errs := make(chan error, 2)
	go func() { errs <- n.Stop("shared") }()
	go func() { errs <- n.StopPrivate("shared") }()
	time.Sleep(20 * time.Millisecond)
	nd.privateMu.Unlock()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := logouts.Load(); got != 1 {
		t.Fatalf("concurrent Stop and StopPrivate performed %d logouts, want 1", got)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
}
