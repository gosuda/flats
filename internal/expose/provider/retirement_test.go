package provider

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"tailscale.com/derp/derpserver"
	"tailscale.com/net/netns"
	"tailscale.com/net/stun/stuntest"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration/testcontrol"
	tskey "tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/nettype"

	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/expose/tsnet"
)

func startManagerControl(t *testing.T) *testcontrol.Server {
	t.Helper()
	if testing.Short() {
		t.Skip("starts a disposable tailnet; skipped in -short mode")
	}
	netns.SetEnabled(false)
	d := derpserver.New(tskey.NewNode(), logger.Discard)
	derp := httptest.NewUnstartedServer(derpserver.Handler(d))
	derp.Config.ErrorLog = logger.StdLogger(logger.Discard)
	derp.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	derp.StartTLS()
	stunAddr, stunCleanup := stuntest.ServeWithPacketListener(t, nettype.Std{})
	t.Cleanup(func() {
		derp.CloseClientConnections()
		derp.Close()
		d.Close()
		stunCleanup()
	})
	derpMap := &tailcfg.DERPMap{Regions: map[tailcfg.DERPRegionID]*tailcfg.DERPRegion{
		1: {RegionID: 1, RegionCode: "test", Nodes: []*tailcfg.DERPNode{{
			Name: "t1", RegionID: 1, HostName: "127.0.0.1", IPv4: "127.0.0.1", IPv6: "none",
			STUNPort: stunAddr.Port, DERPPort: derp.Listener.Addr().(*net.TCPAddr).Port,
			InsecureForTests: true, STUNTestIP: "127.0.0.1",
		}}},
	}}
	control := &testcontrol.Server{DERPMap: derpMap, MagicDNSDomain: "tail-scale.ts.net", Logf: logger.Discard}
	control.HTTPTestServer = httptest.NewUnstartedServer(control)
	control.HTTPTestServer.Start()
	t.Cleanup(control.HTTPTestServer.Close)
	return control
}

type managerControlProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []net.Conn
	paused bool
}

func startManagerControlProxy(t *testing.T, target string) *managerControlProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &managerControlProxy{ln: ln, target: target}
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

func (p *managerControlProxy) pause() {
	p.mu.Lock()
	p.paused = true
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (p *managerControlProxy) resume() {
	p.mu.Lock()
	p.paused = false
	p.mu.Unlock()
}

func TestManagerServeFunnelReturnsWhileControlIsUnavailable(t *testing.T) {
	control := startManagerControl(t)
	u, err := url.Parse(control.HTTPTestServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := startManagerControlProxy(t, u.Host)
	proxy.pause()
	dir := t.TempDir()
	tailnet, err := tsnet.New(tsnet.Config{
		Dir: filepath.Join(dir, "tsnet"), ControlURL: "http://" + proxy.ln.Addr().String(), Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	loopback, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer loopback.Close()
	if err := Save(dir, File{Version: 1, Permitted: []ID{Funnel}}); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, Options{Local: loopback, Tailscale: TSNet{tailnet}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := m.ServeExposure(context.Background(), ExposureRequest{
		Slug: "offline", Host: "offline", Visibility: "public", Audience: AudienceCurrent,
		Handler: text("offline"), Permitted: []ID{Funnel},
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Funnel registration blocked on unavailable control for %s", elapsed)
	}
	var funnel ExposureEndpoint
	for _, ep := range res.Endpoints {
		if ep.Provider == Funnel {
			funnel = ep
		}
	}
	if funnel.State != stateStarting {
		t.Fatalf("initial Funnel endpoint = %+v", funnel)
	}
	if tracked, err := m.HasProviderRoute(t.Context(), "offline", Funnel); err != nil || !tracked {
		t.Fatalf("starting Funnel was not tracked: tracked=%v err=%v", tracked, err)
	}

	proxy.resume()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.StopSlug(ctx, "offline"); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tailnet.Close(); err != nil {
		t.Fatal(err)
	}
}

// StopExposure is shared by delete, preview close and redirect expiry. A
// failed backend retirement must leave the manager registration in place so
// the next lifecycle pass performs a real retry instead of reporting success.
func TestManagerStopExposureRetriesRealTailnetRetirement(t *testing.T) {
	control := startManagerControl(t)
	u, err := url.Parse(control.HTTPTestServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := startManagerControlProxy(t, u.Host)
	dir := t.TempDir()
	tsDir := filepath.Join(dir, "tsnet")
	tailnet, err := tsnet.New(tsnet.Config{Dir: tsDir, ControlURL: "http://" + proxy.ln.Addr().String(), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	loopback, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer loopback.Close()
	if err := Save(dir, File{Version: 1, Permitted: []ID{Tailscale}}); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, Options{Local: loopback, Tailscale: TSNet{tailnet}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := m.ServeExposure(ctx, ExposureRequest{
		Slug: "doomed", Host: "doomed", Visibility: "private", Audience: AudienceCurrent,
		Handler: text("live"), Permitted: []ID{Local, Tailscale},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tailnet.WaitReady(ctx, "doomed"); err != nil {
		t.Fatal(err)
	}

	proxy.pause()
	if err := m.StopExposure(ctx, "doomed"); err == nil {
		t.Fatal("StopExposure succeeded while control was unavailable")
	}
	if tracked, err := m.HasProviderRoute(ctx, "doomed", Tailscale); err != nil || !tracked {
		t.Fatalf("failed retirement was not retained: tracked=%v err=%v", tracked, err)
	}
	if _, err := os.Stat(filepath.Join(tsDir, "doomed", "tailscaled.state")); err != nil {
		t.Fatalf("failed retirement discarded the retry key: %v", err)
	}

	proxy.resume()
	if err := m.StopExposure(ctx, "doomed"); err != nil {
		t.Fatalf("StopExposure retry after control recovery: %v", err)
	}
	if tracked, err := m.HasProviderRoute(ctx, "doomed", Tailscale); err != nil || tracked {
		t.Fatalf("confirmed retirement stayed registered: tracked=%v err=%v", tracked, err)
	}
	if _, err := os.Stat(filepath.Join(tsDir, "doomed")); !os.IsNotExist(err) {
		t.Fatalf("confirmed retirement retained state: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tailnet.Close(); err != nil {
		t.Fatal(err)
	}
}
