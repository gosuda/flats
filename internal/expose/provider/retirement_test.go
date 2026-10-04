package provider

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	return startManagerControlMode(t, false)
}

func startHTTPSManagerControl(t *testing.T) *testcontrol.Server {
	return startManagerControlMode(t, true)
}

func startManagerControlMode(t *testing.T, https bool) *testcontrol.Server {
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
	if https {
		control.DNSConfig = &tailcfg.DNSConfig{Proxied: true}
	}
	control.HTTPTestServer = httptest.NewUnstartedServer(control)
	control.HTTPTestServer.Start()
	t.Cleanup(control.HTTPTestServer.Close)
	return control
}

func managerTestCertificate(t *testing.T) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "provider test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "*.tail-scale.ts.net"},
		DNSNames:  []string{"*.tail-scale.ts.net"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert := &tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert, nil }
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

func TestManagerStopSlugRetiresFunnelOnlyIdentityBeforeRecreate(t *testing.T) {
	control := startHTTPSManagerControl(t)
	dir := t.TempDir()
	tailnet, err := tsnet.New(tsnet.Config{
		Dir: filepath.Join(dir, "tsnet"), ControlURL: control.HTTPTestServer.URL,
		GetCertificate: managerTestCertificate(t), Logf: t.Logf,
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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	serve := func() {
		t.Helper()
		if _, err := m.ServeExposure(ctx, ExposureRequest{
			Slug: "recreated", Host: "recreated", Visibility: "public", Audience: AudienceCurrent,
			Handler: text("public"), Permitted: []ID{Funnel},
		}); err != nil {
			t.Fatal(err)
		}
	}
	waitState := func(wantNodes int) []byte {
		t.Helper()
		statePath := filepath.Join(dir, "tsnet", "recreated", "tailscaled.state")
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			state, readErr := os.ReadFile(statePath)
			status, statusErr := m.ExposureStatus(ctx, "recreated")
			ready := false
			for _, ep := range status.Endpoints {
				ready = ready || ep.Provider == Funnel && ep.State == stateReady
			}
			if readErr == nil && len(state) > 0 && control.NumNodes() >= wantNodes && statusErr == nil && ready {
				return state
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("Funnel-only identity never became ready: nodes=%d", control.NumNodes())
		return nil
	}

	serve()
	before := waitState(1)
	nodesBefore := control.NumNodes()
	if err := m.StopSlug(ctx, "recreated"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tsnet", "recreated")); !os.IsNotExist(err) {
		t.Fatalf("confirmed Funnel-only retirement retained state: %v", err)
	}
	if tracked, err := m.HasProviderRoute(ctx, "recreated", Funnel); err != nil || tracked {
		t.Fatalf("confirmed Funnel-only retirement stayed registered: tracked=%v err=%v", tracked, err)
	}

	serve()
	after := waitState(nodesBefore + 1)
	if bytes.Equal(before, after) {
		t.Fatal("recreated slug inherited the deleted Funnel-only identity")
	}
	if err := m.StopSlug(ctx, "recreated"); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tailnet.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerPublicToPrivateRetiresFunnelOnlyIdentity(t *testing.T) {
	control := startHTTPSManagerControl(t)
	dir := t.TempDir()
	tsDir := filepath.Join(dir, "tsnet")
	tailnet, err := tsnet.New(tsnet.Config{
		Dir: tsDir, ControlURL: control.HTTPTestServer.URL,
		GetCertificate: managerTestCertificate(t), Logf: t.Logf,
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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	serve := func(body string) (ExposureResult, []byte) {
		t.Helper()
		res, err := m.ServeExposure(ctx, ExposureRequest{
			Slug: "public-private", Host: "public-private", Visibility: "public", Audience: AudienceCurrent,
			Handler: text(body), Permitted: []ID{Funnel},
		})
		if err != nil {
			t.Fatal(err)
		}
		waitManagerFunnelState(t, ctx, m, "public-private", stateReady)
		state, err := os.ReadFile(filepath.Join(tsDir, "public-private", "tailscaled.state"))
		if err != nil {
			t.Fatal(err)
		}
		return res, state
	}

	res, before := serve("private-stays-live")
	localURL := endpointURL(res, Local)
	stopped, err := m.StopPublicRoutes(ctx, "public-private")
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped.Unconfirmed) != 0 || len(stopped.Stopped) != 1 || stopped.Stopped[0] != Funnel {
		t.Fatalf("public stop = %+v", stopped)
	}
	if got := get(t, localURL); got != "private-stays-live" {
		t.Fatalf("Public -> Private removed Local: %q", got)
	}
	if _, err := os.Stat(filepath.Join(tsDir, "public-private")); !os.IsNotExist(err) {
		t.Fatalf("Public -> Private retained Funnel-only identity: %v", err)
	}
	if err := m.StopSlug(ctx, "public-private"); err != nil {
		t.Fatal(err)
	}
	_, after := serve("recreated")
	if bytes.Equal(before, after) {
		t.Fatal("recreated slug reused the pre-transition Funnel identity")
	}
	if err := m.StopSlug(ctx, "public-private"); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tailnet.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerPublicStopControlOutageClosesFunnelAndDefersIdentityRetirement(t *testing.T) {
	control := startHTTPSManagerControl(t)
	u, err := url.Parse(control.HTTPTestServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := startManagerControlProxy(t, u.Host)
	dir := t.TempDir()
	tsDir := filepath.Join(dir, "tsnet")
	tailnet, err := tsnet.New(tsnet.Config{
		Dir: tsDir, ControlURL: "http://" + proxy.ln.Addr().String(),
		GetCertificate: managerTestCertificate(t), Logf: t.Logf,
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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := m.ServeExposure(ctx, ExposureRequest{
		Slug: "offline-transition", Host: "offline-transition", Visibility: "public", Audience: AudienceCurrent,
		Handler: text("private after stop"), Permitted: []ID{Funnel},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitManagerFunnelState(t, ctx, m, "offline-transition", stateReady)
	proxy.pause()
	stopped, err := m.StopPublicRoutes(ctx, "offline-transition")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stopped.Stopped, []ID{Funnel}) || len(stopped.Unconfirmed) != 0 {
		t.Fatalf("closed Funnel was reported reachable: %+v", stopped)
	}
	if state := tailnet.FunnelStatus("offline-transition"); state.State != tsnet.FunnelUnavailable {
		t.Fatalf("Funnel remained reachable after confirmed listener close: %+v", state)
	}
	if _, tracked := m.take("offline-transition", Funnel, false); tracked {
		t.Fatal("closed Funnel stayed registered as a public route")
	}
	if pending, err := m.hasPendingTailnet("offline-transition", "offline-transition"); err != nil || !pending {
		t.Fatalf("logout failure was not retained as a non-public obligation: pending=%v err=%v", pending, err)
	}
	if got := get(t, endpointURL(res, Local)); got != "private after stop" {
		t.Fatalf("transition removed Local: %q", got)
	}
	statePath := filepath.Join(tsDir, "offline-transition", "tailscaled.state")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("failed logout discarded retry identity: %v", err)
	}

	proxy.resume()
	if err := m.StopSlug(ctx, "offline-transition"); err != nil {
		t.Fatalf("StopSlug did not retry deferred identity retirement: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tsDir, "offline-transition")); !os.IsNotExist(err) {
		t.Fatalf("confirmed retry retained identity: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tailnet.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerWithoutTailnetDeletesLegacyIdentityBeforeSlugRecreation(t *testing.T) {
	control := startHTTPSManagerControl(t)
	u, err := url.Parse(control.HTTPTestServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := startManagerControlProxy(t, u.Host)
	dir := t.TempDir()
	tsDir := filepath.Join(dir, "tsnet")
	getCert := managerTestCertificate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	newTailnet := func() *tsnet.Net {
		t.Helper()
		n, err := tsnet.New(tsnet.Config{
			Dir: tsDir, ControlURL: "http://" + proxy.ln.Addr().String(),
			GetCertificate: getCert, Logf: t.Logf,
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	newLocal := func() *local.Net {
		t.Helper()
		n, err := local.Listen("127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	servePublic := func(m *Manager) []byte {
		t.Helper()
		if _, err := m.ServeExposure(ctx, ExposureRequest{
			Slug: "legacy-recreate", Host: "legacy-recreate", Visibility: "public", Audience: AudienceCurrent,
			Handler: text("public"), Permitted: []ID{Funnel},
		}); err != nil {
			t.Fatal(err)
		}
		waitManagerFunnelState(t, ctx, m, "legacy-recreate", stateReady)
		state, err := os.ReadFile(filepath.Join(tsDir, "legacy-recreate", "tailscaled.state"))
		if err != nil {
			t.Fatal(err)
		}
		return state
	}

	if err := Save(dir, File{Version: 1, Permitted: []ID{Funnel}}); err != nil {
		t.Fatal(err)
	}
	firstTail := newTailnet()
	firstLocal := newLocal()
	first, err := New(dir, Options{Local: firstLocal, Tailscale: TSNet{firstTail}})
	if err != nil {
		t.Fatal(err)
	}
	before := servePublic(first)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := firstTail.Close(); err != nil {
		t.Fatal(err)
	}
	if err := firstLocal.Close(); err != nil {
		t.Fatal(err)
	}

	if err := Save(dir, File{Version: 1, Permitted: []ID{}}); err != nil {
		t.Fatal(err)
	}
	localOnly := newLocal()
	withoutTailnet, err := New(dir, Options{Local: localOnly})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutTailnet.ServeExposure(ctx, ExposureRequest{
		Slug: "legacy-recreate", Host: "legacy-recreate", Visibility: "private", Audience: AudienceCurrent,
		Handler: text("local only"),
	}); err != nil {
		t.Fatal(err)
	}
	proxy.pause()
	start := time.Now()
	if err := withoutTailnet.StopSlug(ctx, "legacy-recreate"); err != nil {
		t.Fatalf("local-only legacy cleanup: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("local-only cleanup contacted unavailable control for %s", elapsed)
	}
	if _, err := os.Stat(filepath.Join(tsDir, "legacy-recreate")); !os.IsNotExist(err) {
		t.Fatalf("local-only delete retained legacy identity: %v", err)
	}
	if err := withoutTailnet.Close(); err != nil {
		t.Fatal(err)
	}
	if err := localOnly.Close(); err != nil {
		t.Fatal(err)
	}

	proxy.resume()
	if err := Save(dir, File{Version: 1, Permitted: []ID{Funnel}}); err != nil {
		t.Fatal(err)
	}
	recreatedTail := newTailnet()
	recreatedLocal := newLocal()
	recreated, err := New(dir, Options{Local: recreatedLocal, Tailscale: TSNet{recreatedTail}})
	if err != nil {
		t.Fatal(err)
	}
	after := servePublic(recreated)
	if bytes.Equal(before, after) {
		t.Fatal("recreated slug inherited the legacy node identity")
	}
	if err := recreated.StopSlug(ctx, "legacy-recreate"); err != nil {
		t.Fatal(err)
	}
	if err := recreated.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recreatedTail.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recreatedLocal.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerFunnelNeedsMachineAuthIsNotPermissionFailure(t *testing.T) {
	control := startHTTPSManagerControl(t)
	control.RequireMachineAuth = true
	dir := t.TempDir()
	tailnet, err := tsnet.New(tsnet.Config{
		Dir: filepath.Join(dir, "tsnet"), ControlURL: control.HTTPTestServer.URL,
		GetCertificate: managerTestCertificate(t), Logf: t.Logf,
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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, serveErr := m.ServeExposure(ctx, ExposureRequest{
		Slug: "needs-approval", Host: "needs-approval", Visibility: "public", Audience: AudienceCurrent,
		Handler: text("approval"), Permitted: []ID{Funnel},
	})
	if errors.Is(serveErr, ErrProviderNotPermitted) {
		t.Fatalf("machine approval was classified as missing permission: %v", serveErr)
	}
	var ep ExposureEndpoint
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, err := m.ExposureStatus(ctx, "needs-approval")
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range status.Endpoints {
			if candidate.Provider == Funnel {
				ep = candidate
			}
		}
		if ep.State == "needs-login" && strings.Contains(ep.Detail, "approval") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ep.State != "needs-login" || !strings.Contains(ep.Detail, "approval") {
		t.Fatalf("machine approval detail = %+v", ep)
	}
	if tracked, err := m.HasProviderRoute(ctx, "needs-approval", Funnel); err != nil || !tracked {
		t.Fatalf("needs-login Funnel route was not tracked: tracked=%v err=%v", tracked, err)
	}
	if err := m.StopSlug(ctx, "needs-approval"); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tailnet.Close(); err != nil {
		t.Fatal(err)
	}
}

func waitManagerFunnelState(t *testing.T, ctx context.Context, m *Manager, slug, want string) ExposureEndpoint {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, err := m.ExposureStatus(ctx, slug)
		if err != nil {
			t.Fatal(err)
		}
		for _, ep := range status.Endpoints {
			if ep.Provider == Funnel && ep.State == want {
				return ep
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Funnel %q did not reach state %q", slug, want)
	return ExposureEndpoint{}
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
