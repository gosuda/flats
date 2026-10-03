package tsnet

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/store/mem"
	ts "tailscale.com/tsnet"
	"tailscale.com/types/logger"
)

func TestServeFunnelRoutesIngressAndLeavesPrivate(t *testing.T) {
	control := startControl(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	getCert, pool := testCA(t)

	n, err := New(Config{Dir: t.TempDir(), ControlURL: control.HTTPTestServer.URL, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	n.getCert = getCert
	n.warmCert = func(context.Context, string) error { return nil }
	defer n.Close()

	private := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "private login=%s", r.Header.Get("Tailscale-User-Login"))
	})
	public := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Tailscale-User-Login") != "" || r.Header.Get("Tailscale-User-Name") != "" {
			http.Error(w, "identity header on funnel", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, "public")
	})
	if _, err := n.Serve(ctx, "flat", private, false); err != nil {
		t.Fatal(err)
	}
	if err := n.WaitReady(ctx, "flat"); err != nil {
		t.Fatalf("private ready: %v\n%+v", err, n.Status())
	}
	n.mu.Lock()
	srv := n.nodes["flat"].srv
	n.mu.Unlock()
	// Pinned tsnet.Server.ListenFunnel without FunnelOnly is listen-on-both.
	// That claims the tailnet :443 key the private listener already holds, so
	// the call fails. It can still write AllowFunnel before listen returns;
	// clear that leak so the later StopFunnel assertion is about our listener.
	if _, err := srv.ListenFunnel("tcp", ":443"); err == nil {
		t.Fatal("ListenFunnel without FunnelOnly claimed :443 already held by the private listener")
	} else if !strings.Contains(err.Error(), "listener already open") {
		t.Fatalf("ListenFunnel without FunnelOnly: %v", err)
	}
	lc, err := srv.LocalClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := clearAllowFunnel(ctx, lc); err != nil {
		t.Fatal(err)
	}
	client, _ := startClient(t, ctx, control.HTTPTestServer.URL)
	tailClient := &http.Client{Transport: &http.Transport{
		DialContext:     client.Dial,
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	if got := fetchBody(t, tailClient, "https://flat."+testDomain+"/"); len(got) < len("private") || got == "public" {
		t.Fatalf("private route after rejected ListenFunnel = %q", got)
	}
	if _, err := n.ServeFunnel(ctx, "flat", public); err != nil {
		t.Fatal(err)
	}
	if got := n.FunnelStatus("flat"); got.State != FunnelReady {
		t.Fatalf("funnel status = %+v", got)
	}

	ingress := &ts.Server{
		Dir: filepath.Join(t.TempDir(), "ingress"), Hostname: "ingress",
		ControlURL: control.HTTPTestServer.URL, Store: new(mem.Store), Ephemeral: true,
		UserLogf: logger.Discard,
	}
	t.Cleanup(func() { ingress.Close() })
	ist, err := ingress.Up(ctx)
	if err != nil {
		t.Fatal(err)
	}
	control.SetUnsignedPeerAPIOnly(ist.Self.PublicKey, true)
	deadline := time.Now().Add(15 * time.Second)
	for {
		res, err := lc.WhoIs(ctx, ist.TailscaleIPs[0].String())
		if err == nil && res.Node != nil && res.Node.UnsignedPeerAPIOnly {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ingress peer not visible: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	target := "flat." + testDomain + ":443"
	funnelClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialFunnelIngress(ingress, n, "flat", target)
		},
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	if got := fetchBody(t, funnelClient, "https://"+target); got != "public" {
		t.Fatalf("ingress body = %q", got)
	}
	if got := fetchBody(t, tailClient, "https://flat."+testDomain+"/"); got == "public" || len(got) < len("private") {
		t.Fatalf("tailnet body = %q", got)
	}

	if err := n.StopFunnel("flat"); err != nil {
		t.Fatal(err)
	}
	if err := funnelAllowCleared(ctx, lc); err != nil {
		t.Fatal(err)
	}
	if got := n.FunnelStatus("flat"); got.State != FunnelUnavailable {
		t.Fatalf("after stop: %+v", got)
	}
	if _, err := funnelClient.Get("https://" + target); err == nil {
		t.Fatal("ingress still reached Funnel after StopFunnel")
	}
	priv := fetchBody(t, tailClient, "https://flat."+testDomain+"/")
	if len(priv) < len("private") || priv == "public" {
		t.Fatalf("private route after Funnel teardown = %q", priv)
	}

	n.afterFunnelListen = func() error { return errors.New("listener setup failed") }
	if _, err := n.ServeFunnel(ctx, "flat", public); err == nil {
		t.Fatal("funnel setup failure was ignored")
	}
	if err := funnelAllowCleared(ctx, lc); err != nil {
		t.Fatal(err)
	}
	if got := fetchBody(t, tailClient, "https://flat."+testDomain+"/"); got == "public" {
		t.Fatal("failed Funnel replaced the private route")
	}
}

func fetchBody(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	res, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("%s: %d %s", url, res.StatusCode, b)
	}
	return string(b)
}

func clearAllowFunnel(ctx context.Context, lc *local.Client) error {
	sc, err := lc.GetServeConfig(ctx)
	if err != nil {
		return err
	}
	if sc == nil || len(sc.AllowFunnel) == 0 {
		return nil
	}
	sc.AllowFunnel = nil
	return lc.SetServeConfig(ctx, sc)
}

func funnelAllowCleared(ctx context.Context, lc *local.Client) error {
	sc, err := lc.GetServeConfig(ctx)
	if err != nil {
		return err
	}
	for hp, on := range sc.AllowFunnel {
		if on {
			return fmt.Errorf("AllowFunnel still set for %s", hp)
		}
	}
	return nil
}

func dialFunnelIngress(from *ts.Server, n *Net, host, target string) (net.Conn, error) {
	n.mu.Lock()
	nd := n.nodes[host]
	n.mu.Unlock()
	if nd == nil || nd.srv == nil {
		return nil, errors.New("funnel node is not running")
	}
	lc, err := nd.srv.LocalClient()
	if err != nil {
		return nil, err
	}
	st, err := lc.StatusWithoutPeers(context.Background())
	if err != nil {
		return nil, err
	}
	if st.Self == nil || len(st.Self.PeerAPIURL) < 2 {
		return nil, fmt.Errorf("peer API URLs = %v", st.Self)
	}
	peer6 := st.Self.PeerAPIURL[1]
	toPeerAPI, ok := stringsCutPrefix(peer6, "http://")
	if !ok {
		return nil, fmt.Errorf("peer API URL %q", peer6)
	}
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 30*time.Second)
	outConn, err := from.Dial(dialCtx, "tcp", toPeerAPI)
	dialCancel()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", "/v0/ingress", nil)
	if err != nil {
		outConn.Close()
		return nil, err
	}
	req.Host = toPeerAPI
	req.Header.Set("Tailscale-Ingress-Src", "127.0.0.1:1234")
	req.Header.Set("Tailscale-Ingress-Target", target)
	req.Header.Set("Tailscale-User-Login", "spoofed")
	if err := req.Write(outConn); err != nil {
		outConn.Close()
		return nil, err
	}
	br := bufio.NewReader(outConn)
	res, err := http.ReadResponse(br, req)
	if err != nil {
		outConn.Close()
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSwitchingProtocols {
		outConn.Close()
		return nil, fmt.Errorf("ingress status %s", res.Status)
	}
	return &bufferedConn{Conn: outConn, reader: br}, nil
}

func stringsCutPrefix(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return s, false
	}
	return s[len(prefix):], true
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }
