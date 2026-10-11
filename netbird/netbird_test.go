package netbird

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSetupKey = "SETUPKEY-do-not-leak-0123456789"

type fakeSession struct {
	url        string
	fail       chan error
	terminated chan struct{}
	once       sync.Once
}

func (s *fakeSession) URL() string { return s.url }
func (s *fakeSession) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		s.once.Do(func() { close(s.terminated) })
		return ctx.Err()
	case err := <-s.fail:
		return err
	}
}

type fakeClient struct {
	mu        sync.Mutex
	opts      clientOptions
	started   bool
	stopped   int
	startErr  error
	exposeErr error
	listeners []net.Listener
	addrs     []string
	requests  []exposeRequest
	sessions  []*fakeSession
	fqdn, ip  string
	pub       string
}

func (f *fakeClient) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = true
	if f.opts.ConfigPath != "" && f.opts.SetupKey != "" {
		// embed persists the generated identity next to ConfigPath.
		if err := os.WriteFile(f.opts.ConfigPath, []byte(`{"PrivateKey":"stored"}`), 0o600); err != nil {
			return err
		}
	}
	return nil
}
func (f *fakeClient) Stop(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped++
	return nil
}
func (f *fakeClient) ListenTCP(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.listeners = append(f.listeners, ln)
	f.addrs = append(f.addrs, addr)
	f.mu.Unlock()
	return ln, nil
}
func (f *fakeClient) Expose(ctx context.Context, req exposeRequest) (session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exposeErr != nil {
		return nil, f.exposeErr
	}
	f.requests = append(f.requests, req)
	s := &fakeSession{url: fmt.Sprintf("%s-%d.proxy.example", req.NamePrefix, len(f.requests)),
		fail: make(chan error, 1), terminated: make(chan struct{})}
	f.sessions = append(f.sessions, s)
	return s, nil
}
func (f *fakeClient) Identity() (string, string, error) { return f.fqdn, f.ip, nil }
func (f *fakeClient) PublicKey() string                 { return f.pub }

func (f *fakeClient) session(i int) *fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < len(f.sessions) {
		return f.sessions[i]
	}
	return nil
}

// install replaces the client factory with one returning fakes.
func install(t *testing.T) (clients *[]*fakeClient, mu *sync.Mutex) {
	t.Helper()
	old := newClient
	var cs []*fakeClient
	var m sync.Mutex
	newClient = func(o clientOptions) (client, error) {
		m.Lock()
		defer m.Unlock()
		f := &fakeClient{opts: o, fqdn: "host.netbird.cloud", ip: "100.64.0.7", pub: "PUBKEY=="}
		cs = append(cs, f)
		return f, nil
	}
	t.Cleanup(func() { newClient = old })
	return &cs, &m
}

func last(cs *[]*fakeClient, mu *sync.Mutex) *fakeClient {
	mu.Lock()
	defer mu.Unlock()
	if len(*cs) == 0 {
		return nil
	}
	return (*cs)[len(*cs)-1]
}

func enrolled(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := Enroll(context.Background(), dir, "", testSetupKey, "dev"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newNet(t *testing.T, dir string) *Net {
	t.Helper()
	n, err := New(Config{Dir: dir, ShutdownTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	n.retryMin, n.retryMax = 10*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { n.Close() })
	return n
}

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "hi %s|%s", r.Header.Get("Tailscale-User-Login"), r.Header.Get("Tailscale_User_Name"))
})

func get(t *testing.T, ln net.Listener, hdr map[string]string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/", nil)
	for k, v := range hdr {
		req.Header[k] = []string{v}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func TestEnrollSuccessStoresNoSecret(t *testing.T) {
	cs, mu := install(t)
	dir := t.TempDir()
	acct, err := Enroll(context.Background(), dir, "https://nb.example.com", testSetupKey, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if acct.ManagementURL != "https://nb.example.com" || acct.DeviceName != "laptop" || acct.Fingerprint == "" {
		t.Fatalf("account %+v", acct)
	}
	f := last(cs, mu)
	if f.opts.SetupKey != testSetupKey || f.stopped != 1 {
		t.Fatalf("setup key used once, client stopped: %+v stopped=%d", f.opts, f.stopped)
	}
	got, ok, err := LoadAccount(dir)
	if err != nil || !ok || got != acct {
		t.Fatalf("LoadAccount = %+v %v %v", got, ok, err)
	}
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), testSetupKey) {
			t.Errorf("%s contains the setup key", p)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", p, info.Mode().Perm())
		}
		return nil
	})
	// A second enrollment is refused until Disconnect.
	if _, err := Enroll(context.Background(), dir, "", testSetupKey, ""); err == nil {
		t.Fatal("second Enroll succeeded")
	}
}

func TestEnrollFailureCleansAndScrubsKey(t *testing.T) {
	old := newClient
	t.Cleanup(func() { newClient = old })
	newClient = func(o clientOptions) (client, error) {
		return &fakeClient{opts: o, startErr: fmt.Errorf("login with %s rejected", o.SetupKey)}, nil
	}
	dir := t.TempDir()
	_, err := Enroll(context.Background(), dir, "", testSetupKey, "x")
	if err == nil || strings.Contains(err.Error(), testSetupKey) {
		t.Fatalf("err = %v", err)
	}
	if _, ok, _ := LoadAccount(dir); ok {
		t.Fatal("account recorded after failure")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("leftovers: %v", entries)
	}
	if _, err := Enroll(context.Background(), dir, "ftp://x", testSetupKey, ""); err == nil {
		t.Fatal("bad management URL accepted")
	}
	if _, err := Enroll(context.Background(), dir, "", " ", ""); err == nil {
		t.Fatal("empty setup key accepted")
	}
}

func TestDisconnect(t *testing.T) {
	install(t)
	dir := enrolled(t)
	if err := Disconnect(context.Background(), dir, false); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := LoadAccount(dir); ok {
		t.Fatal("still connected")
	}
	if err := Disconnect(context.Background(), dir, false); err == nil {
		t.Fatal("disconnect of a disconnected host succeeded")
	}
	if err := Disconnect(context.Background(), dir, true); err != nil {
		t.Fatal(err)
	}
}

func TestServeBeforeEnrollmentFails(t *testing.T) {
	install(t)
	n := newNet(t, t.TempDir())
	if n.Account() != "" {
		t.Fatal("fingerprint without enrollment")
	}
	if st := n.PublicStatus(); st.Detail != "not connected" || st.Kind != "netbird-expose" {
		t.Fatalf("status %+v", st)
	}
	if _, err := n.ServePrivate(context.Background(), "h", okHandler); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
	if _, err := n.ServePublic(context.Background(), "s", okHandler); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
	// Enrolling afterwards works without a restart.
	if _, err := Enroll(context.Background(), n.cfg.Dir, "", testSetupKey, "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.ServePrivate(context.Background(), "h", okHandler); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateServeStopRetireStablePorts(t *testing.T) {
	cs, mu := install(t)
	dir := enrolled(t)
	n := newNet(t, dir)
	url, err := n.ServePrivate(context.Background(), "alpha", okHandler)
	if err != nil {
		t.Fatal(err)
	}
	if url != "http://host.netbird.cloud:21000" {
		t.Fatalf("url = %q", url)
	}
	f := last(cs, mu)
	if f.opts.SetupKey != "" || f.opts.ConfigPath != filepath.Join(dir, configFile) {
		t.Fatalf("restart must not use a setup key: %+v", f.opts)
	}
	if got := get(t, f.listeners[0], map[string]string{"Tailscale-User-Login": "evil", "Tailscale_User_Name": "evil"}); got != "hi |" {
		t.Fatalf("identity headers leaked: %q", got)
	}
	if f.addrs[0] != ":21000" {
		t.Fatalf("addr = %q", f.addrs[0])
	}
	u2, err := n.ServePrivate(context.Background(), "beta", okHandler)
	if err != nil || u2 != "http://host.netbird.cloud:21001" {
		t.Fatalf("beta %q %v", u2, err)
	}
	if n.PrivateURL("alpha") != url || n.PublicURL("alpha") != "" {
		t.Fatal("urls")
	}
	// Replacing the handler keeps the listener.
	if u, _ := n.ServePrivate(context.Background(), "alpha", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "new") })); u != url {
		t.Fatalf("replace url %q", u)
	}
	if got := get(t, f.listeners[0], nil); got != "new" {
		t.Fatalf("handler not replaced: %q", got)
	}
	if st := n.PrivateStatus(); st.Kind != "netbird" || len(st.Hosts) != 2 || st.Hosts[0].State != "ready" {
		t.Fatalf("status %+v", st)
	}
	if err := n.StopPrivate("alpha"); err != nil {
		t.Fatal(err)
	}
	if n.PrivateURL("alpha") != "" {
		t.Fatal("still served")
	}
	if _, err := http.Get("http://" + f.listeners[0].Addr().String()); err == nil {
		t.Fatal("listener still open after stop")
	}
	n.Close()

	// A new process keeps the ports.
	n2 := newNet(t, dir)
	if u, err := n2.ServePrivate(context.Background(), "beta", okHandler); err != nil || u != u2 {
		t.Fatalf("beta after restart %q %v", u, err)
	}
	if u, _ := n2.ServePrivate(context.Background(), "alpha", okHandler); u != url {
		t.Fatalf("alpha after restart %q", u)
	}
	// Retire frees the port for reuse.
	if err := n2.RetirePrivate("alpha"); err != nil {
		t.Fatal(err)
	}
	if u, _ := n2.ServePrivate(context.Background(), "gamma", okHandler); u != "http://host.netbird.cloud:21000" {
		t.Fatalf("gamma reuses the freed port: %q", u)
	}
	if err := n2.StopPrivate("unknown"); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateFallsBackToIP(t *testing.T) {
	cs, mu := install(t)
	dir := enrolled(t)
	old := newClient
	newClient = func(o clientOptions) (client, error) {
		f := &fakeClient{opts: o, ip: "100.64.0.9", pub: "k"}
		mu.Lock()
		*cs = append(*cs, f)
		mu.Unlock()
		return f, nil
	}
	t.Cleanup(func() { newClient = old })
	n := newNet(t, dir)
	if u, err := n.ServePrivate(context.Background(), "a", okHandler); err != nil || u != "http://100.64.0.9:21000" {
		t.Fatalf("%q %v", u, err)
	}
}

func TestPublicServeExposeFieldsAndStop(t *testing.T) {
	cs, mu := install(t)
	n := newNet(t, enrolled(t))
	url, err := n.ServePublic(context.Background(), "blog", okHandler)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://blog-1.proxy.example" {
		t.Fatalf("url = %q", url)
	}
	f := last(cs, mu)
	if len(f.requests) != 1 || f.requests[0] != (exposeRequest{NamePrefix: "blog", Port: 21000}) {
		t.Fatalf("requests %+v", f.requests)
	}
	if got := get(t, f.listeners[0], map[string]string{"tailscale-user-login": "evil"}); got != "hi |" {
		t.Fatalf("leaked: %q", got)
	}
	// Private and public ports are independent.
	if u, _ := n.ServePrivate(context.Background(), "blog", okHandler); !strings.HasSuffix(u, ":21001") {
		t.Fatalf("private port %q", u)
	}
	if st := n.PublicStatus(); st.Kind != "netbird-expose" || len(st.Hosts) != 1 || st.Hosts[0].State != "ready" || st.Hosts[0].URL != url {
		t.Fatalf("status %+v", st)
	}
	if err := n.StopPublic("blog"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.session(0).terminated:
	default:
		t.Fatal("session not terminated by stop")
	}
	if _, err := http.Get("http://" + f.listeners[0].Addr().String()); err == nil {
		t.Fatal("server still up")
	}
	if n.PublicURL("blog") != "" {
		t.Fatal("still served")
	}
	// Port record kept across stop; retired afterwards.
	if u, err := n.ServePublic(context.Background(), "blog", okHandler); err != nil || u == "" {
		t.Fatal(err)
	}
	if r := f.requests; r[len(r)-1].Port != 21000 {
		t.Fatalf("port changed after stop: %+v", r)
	}
	if err := n.RetirePublic("blog"); err != nil {
		t.Fatal(err)
	}
}

func TestPublicKeepAliveErrorReexposes(t *testing.T) {
	cs, mu := install(t)
	n := newNet(t, enrolled(t))
	if _, err := n.ServePublic(context.Background(), "blog", okHandler); err != nil {
		t.Fatal(err)
	}
	f := last(cs, mu)
	f.session(0).fail <- errors.New("keepalive refused")
	eventually(t, "second expose", func() bool { return n.PublicURL("blog") == "https://blog-2.proxy.example" })
	eventually(t, "ready again", func() bool { return n.PublicStatus().Hosts[0].State == "ready" })
	select {
	case <-f.session(0).terminated:
		t.Fatal("a failed session cannot be terminated again") // Wait returned via fail, not ctx
	default:
	}
	f.mu.Lock()
	nl := len(f.listeners)
	f.mu.Unlock()
	if nl != 2 {
		t.Fatalf("listeners = %d, want a rebind", nl)
	}
}

func TestPublicExposeFailureAtServeAndRecovery(t *testing.T) {
	cs, mu := install(t)
	n := newNet(t, enrolled(t))
	// Start the client first with a working route.
	if _, err := n.ServePrivate(context.Background(), "p", okHandler); err != nil {
		t.Fatal(err)
	}
	f := last(cs, mu)
	f.mu.Lock()
	f.exposeErr = errors.New("quota exceeded")
	f.mu.Unlock()
	_, err := n.ServePublic(context.Background(), "blog", okHandler)
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("err = %v", err)
	}
	if n.PublicURL("blog") != "" || len(n.PublicStatus().Hosts) != 0 {
		t.Fatal("failed first serve left an entry")
	}
	f.mu.Lock()
	f.exposeErr = nil
	f.mu.Unlock()
	if _, err := n.ServePublic(context.Background(), "blog", okHandler); err != nil {
		t.Fatal(err)
	}
	// An expose failure after being up retries with backoff.
	f.mu.Lock()
	f.exposeErr = errors.New("flaky")
	f.mu.Unlock()
	f.session(0).fail <- errors.New("gone")
	eventually(t, "error state", func() bool {
		h := n.PublicStatus().Hosts
		return len(h) == 1 && h[0].State == "error" && strings.Contains(h[0].Detail, "flaky")
	})
	f.mu.Lock()
	f.exposeErr = nil
	f.mu.Unlock()
	eventually(t, "recovered", func() bool { return n.PublicStatus().Hosts[0].State == "ready" })
}

func TestListenFailureReportsError(t *testing.T) {
	old := newClient
	t.Cleanup(func() { newClient = old })
	dir := func() string {
		install(t)
		return enrolled(t)
	}()
	newClient = func(o clientOptions) (client, error) { return &listenFail{fakeClient{opts: o}}, nil }
	n := newNet(t, dir)
	if _, err := n.ServePrivate(context.Background(), "h", okHandler); err == nil || !strings.Contains(err.Error(), "overlay") {
		t.Fatalf("err = %v", err)
	}
}

type listenFail struct{ fakeClient }

func (l *listenFail) ListenTCP(string) (net.Listener, error) { return nil, errors.New("overlay down") }

func TestClientStartFailureRetriesLater(t *testing.T) {
	cs, mu := install(t)
	dir := enrolled(t)
	n := newNet(t, dir)
	old := newClient
	first := true
	newClient = func(o clientOptions) (client, error) {
		f := &fakeClient{opts: o, fqdn: "h.nb", pub: "k"}
		if first {
			f.startErr, first = errors.New("management unreachable"), false
		}
		mu.Lock()
		*cs = append(*cs, f)
		mu.Unlock()
		return f, nil
	}
	t.Cleanup(func() { newClient = old })
	if _, err := n.ServePrivate(context.Background(), "h", okHandler); err == nil {
		t.Fatal("expected start failure")
	}
	if _, err := n.ServePrivate(context.Background(), "h", okHandler); err != nil {
		t.Fatal(err)
	}
}

func TestCloseStopsEverythingAndIsIdempotent(t *testing.T) {
	cs, mu := install(t)
	n := newNet(t, enrolled(t))
	if _, err := n.ServePublic(context.Background(), "a", okHandler); err != nil {
		t.Fatal(err)
	}
	if _, err := n.ServePrivate(context.Background(), "a", okHandler); err != nil {
		t.Fatal(err)
	}
	f := last(cs, mu)
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if f.stopped != 1 {
		t.Fatalf("stopped = %d", f.stopped)
	}
	select {
	case <-f.session(0).terminated:
	default:
		t.Fatal("session not terminated on close")
	}
	if _, err := n.ServePublic(context.Background(), "a", okHandler); err == nil {
		t.Fatal("serve after close")
	}
	if err := n.StopPublic("a"); err != nil {
		t.Fatal(err)
	}
	if err := n.RetirePublic("a"); err == nil {
		t.Fatal("retire after close should fail")
	}
}

func TestConcurrentServeStopRace(t *testing.T) {
	install(t)
	n := newNet(t, enrolled(t))
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("s%d", i%3)
			for j := 0; j < 5; j++ {
				if _, err := n.ServePublic(context.Background(), name, okHandler); err != nil {
					t.Error(err)
					return
				}
				if _, err := n.ServePrivate(context.Background(), name, okHandler); err != nil {
					t.Error(err)
					return
				}
				n.PublicStatus()
				n.StopPublic(name)
				n.StopPrivate(name)
			}
		}(i)
	}
	wg.Wait()
}

func TestIdentityHeader(t *testing.T) {
	for h, want := range map[string]bool{"Tailscale-User-Login": true, "tailscale_user_name": true, "TAILSCALE-USER-": true,
		"Tailscale-Cap": false, "X-Tailscale-User-Login": false} {
		if isIdentityHeader(h) != want {
			t.Errorf("%q = %v", h, !want)
		}
	}
}

// A retired public port stays out of the pool until the quarantine ends:
// NetBird only logs a failed StopExpose, so the proxy may still forward to the
// old port. A private flat must not inherit it.
func TestRetiredPublicPortIsQuarantined(t *testing.T) {
	install(t)
	dir := enrolled(t)
	n := newNet(t, dir)
	ctx := context.Background()
	if _, err := n.ServePublic(ctx, "blog", okHandler); err != nil {
		t.Fatal(err)
	}
	if err := n.RetirePublic("blog"); err != nil {
		t.Fatal(err)
	}
	u, err := n.ServePrivate(ctx, "notes", okHandler)
	if err != nil || strings.HasSuffix(u, ":21000") {
		t.Fatalf("a private flat reused the retired public port: %q %v", u, err)
	}
	// Retiring a private port still frees it at once.
	if err := n.RetirePrivate("notes"); err != nil {
		t.Fatal(err)
	}
	if u, _ := n.ServePrivate(ctx, "again", okHandler); !strings.HasSuffix(u, ":21001") {
		t.Fatalf("a retired private port was not reused: %q", u)
	}
	// After the quarantine the port is available again.
	old := quarantineTTL
	quarantineTTL = -time.Second
	defer func() { quarantineTTL = old }()
	p, err := readPorts(dir)
	if err != nil {
		t.Fatal(err)
	}
	p.quarantine(21000, time.Now())
	if !p.expire(time.Now()) {
		t.Fatal("an ended quarantine was not dropped")
	}
	if port, _, err := p.allocate("private/late"); err != nil || port != 21000 {
		t.Fatalf("port after the quarantine = %d, %v", port, err)
	}
}

// RetireStored releases ports with no Net running: the private port is freed
// at once, the public one is quarantined.
func TestRetireStoredWithoutBackend(t *testing.T) {
	install(t)
	dir := enrolled(t)
	n := newNet(t, dir)
	ctx := context.Background()
	if _, err := n.ServePrivate(ctx, "blog", okHandler); err != nil {
		t.Fatal(err)
	}
	if _, err := n.ServePublic(ctx, "blog", okHandler); err != nil {
		t.Fatal(err)
	}
	n.Close()
	if err := RetireStored(dir, "blog"); err != nil {
		t.Fatal(err)
	}
	if err := RetireStored(dir, "unknown"); err != nil {
		t.Fatal(err)
	}
	p, err := readPorts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p["private/blog"]; ok {
		t.Fatalf("private record kept: %v", p)
	}
	if _, ok := p["public/blog"]; ok {
		t.Fatalf("public record kept: %v", p)
	}
	if _, ok := p[quarantinePrefix+"21001"]; !ok {
		t.Fatalf("public port not quarantined: %v", p)
	}
	n2 := newNet(t, dir)
	if u, _ := n2.ServePrivate(ctx, "new", okHandler); !strings.HasSuffix(u, ":21000") {
		t.Fatalf("the freed private port is not reused first: %q", u)
	}
}

// The real client honors a start deadline even though NetBird's own Start
// authenticates before it looks at the context.
func TestEmbedClientStartHonorsDeadline(t *testing.T) {
	c, err := newEmbedClient(clientOptions{DeviceName: "probe", ManagementURL: "https://127.0.0.1:1",
		SetupKey: "00000000-0000-0000-0000-000000000000", ConfigPath: filepath.Join(t.TempDir(), "nb.json"),
		StatePath: filepath.Join(t.TempDir(), "dns.json"), LogOutput: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Start succeeded against an unreachable server")
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("Start took %s", time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start ignored its deadline")
	}
}

func TestNamePrefixFitsNetBirdLimit(t *testing.T) {
	long := strings.Repeat("a", 31) + "-" + strings.Repeat("b", 22) // 54 characters, hyphen at index 31
	for slug, want := range map[string]string{
		"blog":                  "blog",
		long:                    strings.Repeat("a", 31),
		strings.Repeat("c", 54): strings.Repeat("c", 32),
	} {
		if got := namePrefix(slug); got != want || len(got) > 32 {
			t.Fatalf("namePrefix(%q) = %q, want %q", slug, got, want)
		}
	}
}

func TestPublicExposeUsesBoundedPrefix(t *testing.T) {
	cs, mu := install(t)
	n := newNet(t, enrolled(t))
	slug := strings.Repeat("d", 54)
	if _, err := n.ServePublic(context.Background(), slug, okHandler); err != nil {
		t.Fatal(err)
	}
	f := last(cs, mu)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 || f.requests[0].NamePrefix != strings.Repeat("d", 32) {
		t.Fatalf("expose requests = %+v", f.requests)
	}
}
