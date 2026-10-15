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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testSetupKey = "8f3c2a1e-4b5d-4c6e-9f70-0123456789ab"

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
		return nil
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
	block     chan struct{} // Start waits for it (or ctx), then the underlying start ends
	exposeErr error
	stopErr   error // returned once by Stop
	listeners []net.Listener
	addrs     []string
	requests  []exposeRequest
	sessions  []*fakeSession
	fqdn, ip  string
}

func (f *fakeClient) Start(ctx context.Context, finished func(error)) error {
	if f.block != nil {
		done := make(chan error, 1)
		go func() {
			<-f.block
			done <- f.startErr
		}()
		select {
		case err := <-done:
			finished(err)
			return err
		case <-ctx.Done():
			go func() { finished(<-done) }()
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		finished(f.startErr)
		return f.startErr
	}
	f.started = true
	if f.opts.SetupKey != "" {
		// embed persists the generated identity in ConfigPath.
		if _, err := os.Stat(f.opts.ConfigPath); err != nil {
			if err := os.WriteFile(f.opts.ConfigPath, []byte(`{"PrivateKey":"stored"}`), 0o644); err != nil {
				return err
			}
		}
	}
	finished(nil)
	return nil
}
func (f *fakeClient) Stop(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.stopErr; err != nil {
		f.stopErr = nil
		return err
	}
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
func (f *fakeClient) PeerFQDN(ip string) string {
	if ip == "100.64.9.9" {
		return "laptop.netbird.cloud"
	}
	return ""
}

func (f *fakeClient) session(i int) *fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < len(f.sessions) {
		return f.sessions[i]
	}
	return nil
}

func (f *fakeClient) stops() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// fakes records every fake client built, by name.
type fakes struct {
	mu   sync.Mutex
	all  []*fakeClient
	prep func(*fakeClient) // adjusts a client before it is returned
}

func (fs *fakes) last(name string) *fakeClient {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for i := len(fs.all) - 1; i >= 0; i-- {
		if fs.all[i].opts.DeviceName == name {
			return fs.all[i]
		}
	}
	return nil
}

func (fs *fakes) count() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.all)
}

// install replaces the client factory with one returning fakes whose primary
// FQDN carries the overlay IP, as NetBird names an ephemeral peer.
func install(t *testing.T) *fakes {
	t.Helper()
	old := newClient
	fs := &fakes{}
	newClient = func(o clientOptions) (client, error) {
		f := &fakeClient{opts: o, fqdn: o.DeviceName + "-1-2.netbird.cloud", ip: "100.64.0.7"}
		fs.mu.Lock()
		prep := fs.prep
		fs.all = append(fs.all, f)
		fs.mu.Unlock()
		if prep != nil {
			prep(f)
		}
		return f, nil
	}
	t.Cleanup(func() { newClient = old })
	return fs
}

// keyHolder is a setup key the test can change.
type keyHolder struct {
	mu  sync.Mutex
	key string
}

func (k *keyHolder) get() string  { k.mu.Lock(); defer k.mu.Unlock(); return k.key }
func (k *keyHolder) set(v string) { k.mu.Lock(); k.key = v; k.mu.Unlock() }

func newNetKey(t *testing.T, dir string, key *keyHolder, logf func(string, ...any)) *Net {
	t.Helper()
	n, err := New(Config{Dir: dir, SetupKey: key.get, ShutdownTimeout: 2 * time.Second, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	n.retryMin, n.retryMax = 10*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { n.Close() })
	return n
}

func newNet(t *testing.T, dir string) *Net {
	return newNetKey(t, dir, &keyHolder{key: testSetupKey}, nil)
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

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestEachNameRunsItsOwnPeer(t *testing.T) {
	fs := install(t)
	dir := t.TempDir()
	n := newNet(t, dir)
	ctx := context.Background()
	url, err := n.ServePrivate(ctx, "alpha", okHandler)
	if err != nil {
		t.Fatal(err)
	}
	if url != "http://alpha.netbird.cloud/" {
		t.Fatalf("url = %q, want the extra label", url)
	}
	f := fs.last("alpha")
	want := clientOptions{DeviceName: "alpha", SetupKey: testSetupKey, DNSLabels: []string{"alpha"},
		ConfigPath: filepath.Join(dir, "peers", "alpha", configFile), StatePath: filepath.Join(dir, "peers", "alpha", dnsFile)}
	if f.opts.DeviceName != want.DeviceName || f.opts.SetupKey != want.SetupKey || strings.Join(f.opts.DNSLabels, ",") != "alpha" ||
		f.opts.ConfigPath != want.ConfigPath || f.opts.StatePath != want.StatePath {
		t.Fatalf("client options = %+v", f.opts)
	}
	if f.addrs[0] != ":80" {
		t.Fatalf("private listens on %q, want :80", f.addrs[0])
	}
	if st, err := os.Stat(f.opts.ConfigPath); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("identity file: %v %v", st, err)
	}
	if got := get(t, f.listeners[0], map[string]string{"Tailscale-User-Login": "evil", "Tailscale_User_Name": "evil"}); got != "hi |" {
		t.Fatalf("identity headers leaked: %q", got)
	}
	u2, err := n.ServePrivate(ctx, "beta", okHandler)
	if err != nil || u2 != "http://beta.netbird.cloud/" {
		t.Fatalf("beta %q %v", u2, err)
	}
	if fs.last("beta") == f || n.PeerCount() != 2 {
		t.Fatalf("beta must run its own peer; peers = %d", n.PeerCount())
	}
	// Replacing the handler keeps the listener and the peer.
	if u, _ := n.ServePrivate(ctx, "alpha", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "new") })); u != url {
		t.Fatalf("replace url %q", u)
	}
	if got := get(t, f.listeners[0], nil); got != "new" {
		t.Fatalf("handler not replaced: %q", got)
	}
	if fs.count() != 2 {
		t.Fatalf("clients built = %d", fs.count())
	}
	if st := n.PrivateStatus(); st.Kind != "netbird" || len(st.Hosts) != 2 || st.Hosts[0].State != "ready" || st.Hosts[0].URL != url {
		t.Fatalf("status %+v", st)
	}
	// Stop ends the peer but keeps its identity.
	if err := n.StopPrivate("alpha"); err != nil {
		t.Fatal(err)
	}
	if n.PrivateURL("alpha") != "" || f.stops() != 1 || n.PeerCount() != 1 {
		t.Fatalf("after stop: url %q stops %d peers %d", n.PrivateURL("alpha"), f.stops(), n.PeerCount())
	}
	if !exists(f.opts.ConfigPath) {
		t.Fatal("stop deleted the identity")
	}
	if _, err := http.Get("http://" + f.listeners[0].Addr().String()); err == nil {
		t.Fatal("listener still open after stop")
	}
	if err := n.StopPrivate("unknown"); err != nil {
		t.Fatal(err)
	}
}

func TestPublicSharesTheFlatsPeer(t *testing.T) {
	fs := install(t)
	n := newNet(t, t.TempDir())
	ctx := context.Background()
	if _, err := n.ServePrivate(ctx, "blog", okHandler); err != nil {
		t.Fatal(err)
	}
	url, err := n.ServePublic(ctx, "blog", okHandler)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://blog-1.proxy.example" || fs.count() != 1 {
		t.Fatalf("url %q, clients %d", url, fs.count())
	}
	f := fs.last("blog")
	if len(f.requests) != 1 || f.requests[0] != (exposeRequest{NamePrefix: "blog", Port: publicPort}) || f.addrs[1] != ":8080" {
		t.Fatalf("expose %+v on %v", f.requests, f.addrs)
	}
	if got := get(t, f.listeners[1], map[string]string{"tailscale-user-login": "evil"}); got != "hi |" {
		t.Fatalf("leaked: %q", got)
	}
	if st := n.PublicStatus(); st.Kind != "netbird-expose" || len(st.Hosts) != 1 || st.Hosts[0].URL != url {
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
	if f.stops() != 0 {
		t.Fatal("the peer stopped while its private route still runs")
	}
	if err := n.StopPrivate("blog"); err != nil {
		t.Fatal(err)
	}
	if f.stops() != 1 {
		t.Fatalf("stops = %d", f.stops())
	}
}

func TestRetireDeletesTheIdentityOnceNothingIsServed(t *testing.T) {
	fs := install(t)
	dir := t.TempDir()
	n := newNet(t, dir)
	ctx := context.Background()
	for _, serve := range []func(context.Context, string, http.Handler) (string, error){n.ServePrivate, n.ServePublic} {
		if _, err := serve(ctx, "blog", okHandler); err != nil {
			t.Fatal(err)
		}
	}
	peer := filepath.Join(dir, "peers", "blog")
	if err := n.RetirePublic("blog"); err != nil {
		t.Fatal(err)
	}
	if !exists(peer) || fs.last("blog").stops() != 0 {
		t.Fatal("retiring one role removed the peer another role uses")
	}
	if err := n.RetirePrivate("blog"); err != nil {
		t.Fatal(err)
	}
	if exists(peer) || fs.last("blog").stops() != 1 {
		t.Fatal("the last retirement must stop the peer and delete its identity")
	}
	// A new flat with the same name registers a new peer.
	if _, err := n.ServePrivate(ctx, "blog", okHandler); err != nil || fs.count() != 2 {
		t.Fatalf("serve after retire: %v, clients %d", err, fs.count())
	}
}

func TestRestartKeepsTheIdentity(t *testing.T) {
	fs := install(t)
	dir := t.TempDir()
	n := newNet(t, dir)
	if _, err := n.ServePrivate(context.Background(), "blog", okHandler); err != nil {
		t.Fatal(err)
	}
	n.Close()
	b1, _ := os.ReadFile(filepath.Join(dir, "peers", "blog", configFile))
	// Without a key the stored identity still logs in.
	n2 := newNetKey(t, dir, &keyHolder{}, nil)
	if _, err := n2.ServePrivate(context.Background(), "blog", okHandler); err != nil {
		t.Fatal(err)
	}
	if f := fs.last("blog"); f.opts.SetupKey != "" {
		t.Fatalf("restart without a key used %q", f.opts.SetupKey)
	}
	b2, _ := os.ReadFile(filepath.Join(dir, "peers", "blog", configFile))
	if string(b1) != string(b2) || len(b1) == 0 {
		t.Fatal("the identity changed across restarts")
	}
}

func TestNoSetupKeyAndNoIdentity(t *testing.T) {
	fs := install(t)
	n := newNetKey(t, t.TempDir(), &keyHolder{}, nil)
	if _, err := n.ServePrivate(context.Background(), "blog", okHandler); !errors.Is(err, ErrNoSetupKey) {
		t.Fatalf("err = %v, want ErrNoSetupKey", err)
	}
	if fs.count() != 0 {
		t.Fatal("a client was built without a key or identity")
	}
	if d := n.PrivateStatus().Detail; d != "no setup key" {
		t.Fatalf("status detail %q", d)
	}
}

func TestSetupKeyIsReadAtEveryStart(t *testing.T) {
	fs := install(t)
	key := &keyHolder{}
	n := newNetKey(t, t.TempDir(), key, nil)
	if _, err := n.ServePrivate(context.Background(), "a", okHandler); !errors.Is(err, ErrNoSetupKey) {
		t.Fatal(err)
	}
	key.set(testSetupKey)
	if _, err := n.ServePrivate(context.Background(), "a", okHandler); err != nil {
		t.Fatal(err)
	}
	key.set("11111111-2222-4333-8444-555555555555")
	if _, err := n.ServePrivate(context.Background(), "b", okHandler); err != nil {
		t.Fatal(err)
	}
	if fs.last("a").opts.SetupKey != testSetupKey || fs.last("b").opts.SetupKey != "11111111-2222-4333-8444-555555555555" {
		t.Fatal("peers did not use the key current at their start")
	}
}

// rejectedLoginErr is what NetBird v0.80.0's embed Start returns when
// management refuses the stored peer and auth.Login falls back to registering
// with the empty setup key.
var rejectedLoginErr = fmt.Errorf("login: %w", status.Error(codes.InvalidArgument,
	"invalid setup-key or no sso information provided, err: invalid UUID length: 0"))

func TestRejectedIdentityWithoutKey(t *testing.T) {
	fs := install(t)
	dir := t.TempDir()
	if _, err := newNet(t, dir).ServePrivate(context.Background(), "blog", okHandler); err != nil {
		t.Fatal(err)
	}
	fs.prep = func(f *fakeClient) { f.startErr = rejectedLoginErr }
	n := newNetKey(t, dir, &keyHolder{}, nil)
	_, err := n.ServePrivate(context.Background(), "blog", okHandler)
	if !errors.Is(err, ErrPeerRejected) || strings.Contains(err.Error(), "UUID") {
		t.Fatalf("err = %v, want ErrPeerRejected", err)
	}
	// With a key the same error is NetBird's own answer about that key.
	n2 := newNet(t, dir)
	if _, err := n2.ServePrivate(context.Background(), "blog", okHandler); err == nil || errors.Is(err, ErrPeerRejected) {
		t.Fatalf("err with a key = %v", err)
	}
}

var noExtraLabelsErr = fmt.Errorf("login: %w", status.Error(codes.FailedPrecondition,
	"couldn't add peer: setup key doesn't allow extra DNS labels"))

func TestKeyWithoutExtraLabelsFailsFastAfterwards(t *testing.T) {
	fs := install(t)
	fs.prep = func(f *fakeClient) {
		if f.opts.SetupKey == testSetupKey {
			f.startErr = noExtraLabelsErr
		}
	}
	key := &keyHolder{key: testSetupKey}
	n := newNetKey(t, t.TempDir(), key, nil)
	for i := 0; i < 2; i++ {
		if _, err := n.ServePrivate(context.Background(), "blog", okHandler); !errors.Is(err, ErrKeyNoExtraLabels) {
			t.Fatalf("attempt %d: err = %v", i, err)
		}
	}
	if fs.count() != 1 {
		t.Fatalf("clients built = %d: the refused key was tried again", fs.count())
	}
	key.set("11111111-2222-4333-8444-555555555555")
	if _, err := n.ServePrivate(context.Background(), "blog", okHandler); err != nil {
		t.Fatalf("a new key: %v", err)
	}
}

func TestStartErrorsDoNotLeakTheKey(t *testing.T) {
	fs := install(t)
	fs.prep = func(f *fakeClient) { f.startErr = fmt.Errorf("register with %s failed", f.opts.SetupKey) }
	_, err := newNet(t, t.TempDir()).ServePrivate(context.Background(), "blog", okHandler)
	if err == nil || strings.Contains(err.Error(), testSetupKey) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("err = %v", err)
	}
}

// A start that outlives its timeout keeps the start lock until NetBird's own
// start ends, so no other peer starts meanwhile; waiters give up with their
// own context.
func TestStartLockIsHeldUntilTheUnderlyingStartEnds(t *testing.T) {
	fs := install(t)
	release := make(chan struct{})
	fs.prep = func(f *fakeClient) {
		if f.opts.DeviceName == "slow" {
			f.block = release
			f.startErr = noExtraLabelsErr
		}
	}
	old := startTimeout
	startTimeout = 100 * time.Millisecond
	t.Cleanup(func() { startTimeout = old })
	var logMu sync.Mutex
	var logs []string
	n := newNetKey(t, t.TempDir(), &keyHolder{key: testSetupKey}, func(f string, a ...any) {
		logMu.Lock()
		logs = append(logs, fmt.Sprintf(f, a...))
		logMu.Unlock()
	})
	_, err := n.ServePrivate(context.Background(), "slow", okHandler)
	if err == nil || !strings.Contains(err.Error(), "did not connect within") {
		t.Fatalf("slow start: %v", err)
	}
	if _, err := n.ServePrivate(context.Background(), "other", okHandler); err == nil || !strings.Contains(err.Error(), "still starting") {
		t.Fatalf("a second start must wait for the first: %v", err)
	}
	if fs.last("other") != nil {
		t.Fatal("a client was built while another start held the lock")
	}
	close(release)
	eventually(t, "the late outcome is logged", func() bool {
		logMu.Lock()
		defer logMu.Unlock()
		return len(logs) > 0 && strings.Contains(strings.Join(logs, "\n"), "extra DNS labels")
	})
	if _, err := n.ServePrivate(context.Background(), "other", okHandler); err != nil {
		t.Fatalf("after the slow start ended: %v", err)
	}
	if _, err := n.ServePrivate(context.Background(), "slow", okHandler); !errors.Is(err, ErrKeyNoExtraLabels) {
		t.Fatalf("the late refusal must be remembered: %v", err)
	}
}

func TestConsoleRole(t *testing.T) {
	fs := install(t)
	dir := t.TempDir()
	n := newNet(t, dir)
	url, err := n.ServeConsole(context.Background(), "flats", okHandler)
	if err != nil || url != "http://flats.netbird.cloud/" {
		t.Fatalf("console %q %v", url, err)
	}
	if n.ConsoleURL("flats") != url || len(n.PrivateStatus().Hosts) != 0 || len(n.ConsoleStatus().Hosts) != 1 {
		t.Fatal("the console role must be reported apart from private flats")
	}
	if got := strings.Join(n.Hostnames("flats"), ","); got != "flats.netbird.cloud,flats-1-2.netbird.cloud,100.64.0.7" {
		t.Fatalf("hostnames %q", got)
	}
	if got := n.IdentityForIP("flats", "100.64.9.9"); got != "laptop.netbird.cloud" {
		t.Fatalf("identity %q", got)
	}
	if fs.last("flats").addrs[0] != ":80" {
		t.Fatal("the console listens on port 80")
	}
	if err := n.RetireConsole("flats"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, "peers", "flats")) || n.Hostnames("flats") != nil {
		t.Fatal("retiring a console domain must delete its peer")
	}
}

// A peer whose Stop fails keeps its identity and its name: nothing starts on
// that name, and the retirement fails, until a later Stop succeeds.
func TestFailedStopKeepsTheNameUntilItStops(t *testing.T) {
	fs := install(t)
	dir := t.TempDir()
	n := newNet(t, dir)
	ctx := context.Background()
	if _, err := n.ServePrivate(ctx, "blog", okHandler); err != nil {
		t.Fatal(err)
	}
	f := fs.last("blog")
	f.mu.Lock()
	f.stopErr = context.DeadlineExceeded
	f.mu.Unlock()
	if err := n.RetirePrivate("blog"); err == nil {
		t.Fatal("a retirement whose peer did not stop succeeded")
	}
	if !exists(filepath.Join(dir, "peers", "blog")) {
		t.Fatal("the identity of a peer still stopping was deleted")
	}
	// The next change of the name stops the old peer first.
	if err := n.RetirePrivate("blog"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, "peers", "blog")) || f.stops() != 1 {
		t.Fatal("the retried retirement must stop the peer and delete its identity")
	}
	// A serve after a failed stop waits for that stop too.
	if _, err := n.ServePrivate(ctx, "news", okHandler); err != nil {
		t.Fatal(err)
	}
	g := fs.last("news")
	g.mu.Lock()
	g.stopErr = context.DeadlineExceeded
	g.mu.Unlock()
	if err := n.StopPrivate("news"); err == nil {
		t.Fatal("a failed stop was not reported")
	}
	if _, err := n.ServePrivate(ctx, "news", okHandler); err != nil || g.stops() != 1 || fs.count() != 3 {
		t.Fatalf("serve after a failed stop: %v, stops %d, clients %d", err, g.stops(), fs.count())
	}
}

func TestInvalidNamesAreRefused(t *testing.T) {
	install(t)
	n := newNet(t, t.TempDir())
	for _, name := range []string{"", "../x", "Blog", "-a", "a-", strings.Repeat("a", 64), "a.b"} {
		if _, err := n.ServePrivate(context.Background(), name, okHandler); err == nil {
			t.Errorf("ServePrivate(%q) succeeded", name)
		}
		if err := RetireStored(t.TempDir(), name); err != nil {
			t.Errorf("RetireStored(%q) = %v", name, err)
		}
	}
}

func TestPrivateFallsBackToIP(t *testing.T) {
	fs := install(t)
	fs.prep = func(f *fakeClient) { f.fqdn, f.ip = "", "100.64.0.9" }
	if u, err := newNet(t, t.TempDir()).ServePrivate(context.Background(), "a", okHandler); err != nil || u != "http://100.64.0.9/" {
		t.Fatalf("%q %v", u, err)
	}
}

func TestPublicKeepAliveErrorReexposes(t *testing.T) {
	fs := install(t)
	n := newNet(t, t.TempDir())
	if _, err := n.ServePublic(context.Background(), "blog", okHandler); err != nil {
		t.Fatal(err)
	}
	f := fs.last("blog")
	f.session(0).fail <- errors.New("keepalive refused")
	eventually(t, "second expose", func() bool { return n.PublicURL("blog") == "https://blog-2.proxy.example" })
	eventually(t, "ready again", func() bool { return n.PublicStatus().Hosts[0].State == "ready" })
	f.mu.Lock()
	nl := len(f.listeners)
	f.mu.Unlock()
	if nl != 2 {
		t.Fatalf("listeners = %d, want a rebind", nl)
	}
}

func TestPublicExposeFailureAtServeAndRecovery(t *testing.T) {
	fs := install(t)
	n := newNet(t, t.TempDir())
	if _, err := n.ServePrivate(context.Background(), "blog", okHandler); err != nil {
		t.Fatal(err)
	}
	f := fs.last("blog")
	f.mu.Lock()
	f.exposeErr = errors.New("quota exceeded")
	f.mu.Unlock()
	if _, err := n.ServePublic(context.Background(), "blog", okHandler); err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("err = %v", err)
	}
	if n.PublicURL("blog") != "" || len(n.PublicStatus().Hosts) != 0 || f.stops() != 0 {
		t.Fatal("a failed public serve left an entry or stopped the private route's peer")
	}
	f.mu.Lock()
	f.exposeErr = nil
	f.mu.Unlock()
	if _, err := n.ServePublic(context.Background(), "blog", okHandler); err != nil {
		t.Fatal(err)
	}
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

type listenFail struct{ *fakeClient }

func (l listenFail) ListenTCP(string) (net.Listener, error) { return nil, errors.New("overlay down") }

func TestListenFailureStopsTheNewPeer(t *testing.T) {
	old := newClient
	t.Cleanup(func() { newClient = old })
	var f *fakeClient
	newClient = func(o clientOptions) (client, error) {
		f = &fakeClient{opts: o, fqdn: "h.netbird.cloud"}
		return listenFail{f}, nil
	}
	n := newNet(t, t.TempDir())
	if _, err := n.ServePrivate(context.Background(), "h", okHandler); err == nil || !strings.Contains(err.Error(), "overlay") {
		t.Fatalf("err = %v", err)
	}
	if f.stops() != 1 || n.PeerCount() != 0 {
		t.Fatal("a peer whose only route failed must stop")
	}
}

func TestCloseStopsEverythingAndIsIdempotent(t *testing.T) {
	fs := install(t)
	n := newNet(t, t.TempDir())
	if _, err := n.ServePublic(context.Background(), "a", okHandler); err != nil {
		t.Fatal(err)
	}
	if _, err := n.ServeConsole(context.Background(), "c", okHandler); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if fs.last("a").stops() != 1 || fs.last("c").stops() != 1 {
		t.Fatal("close must stop every peer once")
	}
	select {
	case <-fs.last("a").session(0).terminated:
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
	n := newNet(t, t.TempDir())
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
				n.PeerCount()
				n.StopPublic(name)
				n.StopPrivate(name)
			}
		}(i)
	}
	wg.Wait()
	if n.PeerCount() != 0 {
		t.Fatalf("peers left = %d", n.PeerCount())
	}
}

func TestIdentityHeader(t *testing.T) {
	for h, want := range map[string]bool{"Tailscale-User-Login": true, "tailscale_user_name": true, "TAILSCALE-USER-": true,
		"Tailscale-Cap": false, "X-Tailscale-User-Login": false} {
		if isIdentityHeader(h) != want {
			t.Errorf("%q = %v", h, !want)
		}
	}
}

func TestCleanupLegacy(t *testing.T) {
	dir := t.TempDir()
	if _, found, err := CleanupLegacy(dir); err != nil || found {
		t.Fatalf("empty dir: %v %v", found, err)
	}
	for name, body := range map[string]string{
		legacyAccountFile: `{"management_url":"https://nb.example","device_name":"flats-box","fingerprint":"abc"}`,
		legacyConfigFile:  `{"PrivateKey":"x"}`, legacyDNSFile: `{}`, legacyPortsFile: `{"private/blog":21000}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(dir, "peers", "blog", configFile)
	os.MkdirAll(filepath.Dir(keep), 0o700)
	os.WriteFile(keep, []byte(`{}`), 0o600)
	p, found, err := CleanupLegacy(dir)
	if err != nil || !found || p.DeviceName != "flats-box" || p.ManagementURL != "https://nb.example" {
		t.Fatalf("cleanup = %+v %v %v", p, found, err)
	}
	for _, name := range []string{legacyAccountFile, legacyConfigFile, legacyDNSFile, legacyPortsFile} {
		if exists(filepath.Join(dir, name)) {
			t.Fatalf("%s kept", name)
		}
	}
	if !exists(keep) {
		t.Fatal("a per-flat peer was removed")
	}
	if _, found, _ := CleanupLegacy(dir); found {
		t.Fatal("found twice")
	}
}

func TestRetireStoredDeletesThePeer(t *testing.T) {
	dir := t.TempDir()
	peer := filepath.Join(dir, "peers", "blog")
	os.MkdirAll(peer, 0o700)
	os.WriteFile(filepath.Join(peer, configFile), []byte(`{}`), 0o600)
	if err := RetireStored(dir, "blog"); err != nil || exists(peer) {
		t.Fatalf("%v, exists %v", err, exists(peer))
	}
	if err := RetireStored(dir, "blog"); err != nil {
		t.Fatal(err)
	}
}

// The real client honors a start deadline even though NetBird's own Start
// authenticates before it looks at the context, and reports when that start
// really ended.
func TestEmbedClientStartHonorsDeadline(t *testing.T) {
	c, err := newEmbedClient(clientOptions{DeviceName: "probe", ManagementURL: "https://127.0.0.1:1",
		SetupKey: "00000000-0000-0000-0000-000000000000", DNSLabels: []string{"probe"},
		ConfigPath: filepath.Join(t.TempDir(), "nb.json"), StatePath: filepath.Join(t.TempDir(), "dns.json"), LogOutput: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	done := make(chan error, 1)
	finished := make(chan error, 1)
	go func() { done <- c.Start(ctx, func(err error) { finished <- err }) }()
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
	fs := install(t)
	n := newNet(t, t.TempDir())
	slug := strings.Repeat("d", 54)
	if _, err := n.ServePublic(context.Background(), slug, okHandler); err != nil {
		t.Fatal(err)
	}
	f := fs.last(slug)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 || f.requests[0].NamePrefix != strings.Repeat("d", 32) {
		t.Fatalf("expose requests = %+v", f.requests)
	}
}

// The adapter tears a peer down once: a Stop that times out leaves that
// teardown running, and a later Stop waits for it instead of calling
// NetBird's Stop again, which would only answer that nothing runs.
func TestEmbedClientStopWaitsForTheOneTeardown(t *testing.T) {
	release := make(chan struct{})
	calls := 0
	var mu sync.Mutex
	e := &embedClient{stop: func(context.Context) error {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := e.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Stop = %v", err)
	}
	close(release)
	if err := e.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("NetBird's Stop ran %d times", calls)
	}
	failed := &embedClient{stop: func(context.Context) error { return errors.New("wireguard down failed") }}
	var torn teardownError
	if err := failed.Stop(context.Background()); !errors.As(err, &torn) {
		t.Fatalf("a failed teardown = %v, want teardownError", err)
	}
}

// A peer whose teardown ended with an error is stopped: its identity can be
// deleted and its name reused at once.
func TestTeardownErrorStillRetires(t *testing.T) {
	fs := install(t)
	dir := t.TempDir()
	n := newNet(t, dir)
	if _, err := n.ServePrivate(context.Background(), "blog", okHandler); err != nil {
		t.Fatal(err)
	}
	f := fs.last("blog")
	f.mu.Lock()
	f.stopErr = teardownError{errors.New("wireguard down failed")}
	f.mu.Unlock()
	if err := n.RetirePrivate("blog"); err != nil || exists(filepath.Join(dir, "peers", "blog")) {
		t.Fatalf("retire after a teardown error: %v", err)
	}
}

// A start its caller abandoned keeps the name busy: the peer is neither
// started again nor retired until that start has ended.
func TestAbandonedStartBlocksTheName(t *testing.T) {
	fs := install(t)
	release := make(chan struct{})
	fs.prep = func(f *fakeClient) {
		if f.opts.DeviceName == "slow" && f.block == nil && fs.count() == 1 {
			f.block = release
		}
	}
	old := startTimeout
	startTimeout = 50 * time.Millisecond
	t.Cleanup(func() { startTimeout = old })
	dir := t.TempDir()
	n := newNet(t, dir)
	if _, err := n.ServePrivate(context.Background(), "slow", okHandler); err == nil {
		t.Fatal("the slow start did not time out")
	}
	if err := n.RetirePrivate("slow"); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("retire during an abandoned start: %v", err)
	}
	if !exists(filepath.Join(dir, "peers", "slow")) {
		t.Fatal("the identity was deleted while a start still ran")
	}
	close(release)
	eventually(t, "the abandoned start ends", func() bool { return n.RetirePrivate("slow") == nil })
	if exists(filepath.Join(dir, "peers", "slow")) {
		t.Fatal("the retirement after the start ended kept the identity")
	}
}

// Close waits for starts their callers abandoned, and reports one that is
// still running at its deadline.
func TestCloseWaitsForAbandonedStarts(t *testing.T) {
	fs := install(t)
	release := make(chan struct{})
	fs.prep = func(f *fakeClient) { f.block = release }
	old := startTimeout
	startTimeout = 50 * time.Millisecond
	t.Cleanup(func() { startTimeout = old })
	n, err := New(Config{Dir: t.TempDir(), SetupKey: func() string { return testSetupKey }, ShutdownTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.ServePrivate(context.Background(), "slow", okHandler); err == nil {
		t.Fatal("the slow start did not time out")
	}
	if err := n.Close(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close with a start still running = %v", err)
	}
	close(release)
	n2, err := New(Config{Dir: t.TempDir(), SetupKey: func() string { return testSetupKey }, ShutdownTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	fs.prep = func(f *fakeClient) {}
	if _, err := n2.ServePrivate(context.Background(), "fast", okHandler); err != nil {
		t.Fatal(err)
	}
	if err := n2.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPeerDirStaysInsidePeers(t *testing.T) {
	for _, name := range []string{"blog", "../x", "../../etc", "a/../../b"} {
		got := peerDir("/data/netbird", name)
		if !strings.HasPrefix(got, "/data/netbird/peers/") {
			t.Errorf("peerDir(%q) = %q", name, got)
		}
	}
}
