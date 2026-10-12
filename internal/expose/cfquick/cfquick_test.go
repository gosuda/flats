package cfquick

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	v1 "github.com/oesni/libtunnel/v1"
)

type fakeSession struct {
	host        string
	spec        string
	mintFn      func() (string, error) // overrides host when set
	serializeFn func() string          // overrides spec when set

	mu       sync.Mutex
	origin   *url.URL
	events   func(v1.Event)
	err      error
	done     chan struct{}
	once     sync.Once
	canceled bool
}

func (s *fakeSession) Hostname() string {
	if s.mintFn != nil {
		h, err := s.mintFn()
		if err != nil {
			s.end(err)
			return ""
		}
		return h
	}
	return s.host
}
func (s *fakeSession) Serialize() string {
	if s.serializeFn != nil {
		return s.serializeFn()
	}
	return s.spec
}
func (s *fakeSession) isCanceled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.canceled
}
func (s *fakeSession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
func (s *fakeSession) Done() <-chan struct{} { return s.done }
func (s *fakeSession) Start(origin *url.URL, events func(v1.Event)) {
	s.mu.Lock()
	s.origin, s.events = origin, events
	s.mu.Unlock()
}
func (s *fakeSession) Cancel() {
	s.mu.Lock()
	s.canceled = true
	s.mu.Unlock()
	s.end(v1.ErrClosed)
}
func (s *fakeSession) end(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
	})
}
func (s *fakeSession) emit(kind v1.EventKind) {
	s.mu.Lock()
	ev := s.events
	s.mu.Unlock()
	ev(v1.Event{Kind: kind})
}
func (s *fakeSession) originURL() *url.URL {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.origin
}

type fakeOpener struct {
	mu       sync.Mutex
	replays  []string
	sessions []*fakeSession
	next     func(n int, spec string) *fakeSession
}

func (o *fakeOpener) open(spec string, _ *slog.Logger) session {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.replays = append(o.replays, spec)
	var s *fakeSession
	if o.next != nil {
		s = o.next(len(o.sessions), spec)
	} else {
		n := len(o.sessions)
		s = &fakeSession{host: fmt.Sprintf("t%d.trycloudflare.com", n), spec: fmt.Sprintf(`{"tunnel":%d}`, n)}
	}
	if s.done == nil {
		s.done = make(chan struct{})
	}
	o.sessions = append(o.sessions, s)
	return s
}

func (o *fakeOpener) session(i int) *fakeSession {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sessions[i]
}

func (o *fakeOpener) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.sessions)
}

func newTestNet(t *testing.T, dir string, o *fakeOpener) *Net {
	t.Helper()
	n, err := New(Config{Dir: dir, ShutdownTimeout: 5 * time.Second, MintTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v1.SetEnvironment(nil) })
	n.open = o.open
	t.Cleanup(func() { n.Close() })
	return n
}

func hostState(n *Net, slug string) (state, detail, url string) {
	for _, h := range n.Status().Hosts {
		if h.Host == slug {
			return h.State, h.Detail, h.URL
		}
	}
	return "", "", ""
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func get(t *testing.T, origin *url.URL, header http.Header) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, origin.String(), nil)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestServeMintsRecordsAndServesTheOrigin(t *testing.T) {
	dir := t.TempDir()
	o := &fakeOpener{}
	n := newTestNet(t, dir, o)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello id=%q", r.Header.Get("Tailscale-User-Login")+r.Header.Get("Tailscale_User_Name"))
	})
	got, err := n.Serve(context.Background(), "demo", h)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://t0.trycloudflare.com" || n.URL("demo") != got {
		t.Fatalf("Serve = %q, URL = %q", got, n.URL("demo"))
	}
	if o.replays[0] != "" {
		t.Errorf("first tunnel replayed %q, want a fresh mint", o.replays[0])
	}
	if state, _, _ := hostState(n, "demo"); state != "starting" {
		t.Errorf("state before the edge = %q, want starting", state)
	}

	s := o.session(0)
	body := get(t, s.originURL(), http.Header{"Tailscale-User-Login": {"forged"}, "Tailscale_User_Name": {"forged"}})
	if body != `hello id=""` {
		t.Errorf("origin body = %q, want identity headers stripped", body)
	}

	s.emit(v1.EventEstablished)
	if state, _, url := hostState(n, "demo"); state != "ready" || url != got {
		t.Errorf("after established: state %q url %q", state, url)
	}

	rec := filepath.Join(dir, "demo.json")
	fi, err := os.Stat(rec)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("record mode %v, want 0600", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(rec); string(b) != `{"tunnel":0}`+"\n" {
		t.Errorf("record = %q", b)
	}
}

func TestServeAgainSwapsTheHandler(t *testing.T) {
	o := &fakeOpener{}
	n := newTestNet(t, t.TempDir(), o)
	text := func(s string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, s) })
	}
	u1, err := n.Serve(context.Background(), "demo", text("v1"))
	if err != nil {
		t.Fatal(err)
	}
	u2, err := n.Serve(context.Background(), "demo", text("v2"))
	if err != nil {
		t.Fatal(err)
	}
	if u1 != u2 || o.count() != 1 {
		t.Fatalf("second Serve opened a new tunnel: %q %q, %d sessions", u1, u2, o.count())
	}
	if body := get(t, o.session(0).originURL(), nil); body != "v2" {
		t.Errorf("origin serves %q, want v2", body)
	}
}

func TestRestartReplaysTheRecordedTunnel(t *testing.T) {
	dir := t.TempDir()
	o := &fakeOpener{}
	n := newTestNet(t, dir, o)
	if _, err := n.Serve(context.Background(), "demo", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}

	o2 := &fakeOpener{next: func(_ int, spec string) *fakeSession {
		return &fakeSession{host: "t0.trycloudflare.com", spec: spec}
	}}
	n2 := newTestNet(t, dir, o2)
	got, err := n2.Serve(context.Background(), "demo", http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	if o2.replays[0] != `{"tunnel":0}` {
		t.Errorf("restart replayed %q, want the record", o2.replays[0])
	}
	if got != "https://t0.trycloudflare.com" {
		t.Errorf("URL after restart = %q", got)
	}
}

func TestGoneTunnelIsReplacedOnANewHostname(t *testing.T) {
	dir := t.TempDir()
	o := &fakeOpener{}
	n := newTestNet(t, dir, o)
	if _, err := n.Serve(context.Background(), "demo", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	o.session(0).emit(v1.EventGone)
	waitFor(t, "a replacement tunnel", func() bool { return o.count() == 2 && n.URL("demo") == "https://t1.trycloudflare.com" })
	o.mu.Lock()
	replay := o.replays[1]
	o.mu.Unlock()
	if replay != "" {
		t.Errorf("replacement replayed %q, want a fresh mint", replay)
	}
	waitFor(t, "the new record", func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "demo.json"))
		return string(b) == `{"tunnel":1}`+"\n"
	})
}

func TestFailedTunnelIsRetriedFromTheRecord(t *testing.T) {
	minBackoff, maxBackoff = 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { minBackoff, maxBackoff = 2*time.Second, time.Minute })
	o := &fakeOpener{next: func(i int, spec string) *fakeSession {
		return &fakeSession{host: "same.trycloudflare.com", spec: `{"tunnel":"same"}`}
	}}
	n := newTestNet(t, t.TempDir(), o)
	if _, err := n.Serve(context.Background(), "demo", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	o.session(0).end(errors.New("edge unreachable"))
	waitFor(t, "a retried tunnel", func() bool { return o.count() == 2 })
	o.mu.Lock()
	replay := o.replays[1]
	o.mu.Unlock()
	if replay != `{"tunnel":"same"}` {
		t.Errorf("retry replayed %q, want the record", replay)
	}
	if n.URL("demo") != "https://same.trycloudflare.com" {
		t.Errorf("URL after retry = %q", n.URL("demo"))
	}
}

func TestStopKeepsTheRecordRetireRemovesIt(t *testing.T) {
	dir := t.TempDir()
	o := &fakeOpener{}
	n := newTestNet(t, dir, o)
	if _, err := n.Serve(context.Background(), "demo", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	origin := o.session(0).originURL()
	if err := n.Stop("demo"); err != nil {
		t.Fatal(err)
	}
	if !o.session(0).canceled {
		t.Error("Stop did not cancel the tunnel")
	}
	if n.URL("demo") != "" || len(n.Status().Hosts) != 0 {
		t.Error("Stop left the slug served")
	}
	if _, err := http.Get(origin.String()); err == nil {
		t.Error("origin still accepts connections after Stop")
	}
	if _, err := os.Stat(filepath.Join(dir, "demo.json")); err != nil {
		t.Errorf("Stop removed the record: %v", err)
	}
	if err := n.Retire("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "demo.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Retire kept the record: %v", err)
	}
}

func TestMintFailureClosesTheOrigin(t *testing.T) {
	o := &fakeOpener{next: func(int, string) *fakeSession {
		return &fakeSession{mintFn: func() (string, error) { return "", errors.New("rate limited") }}
	}}
	n := newTestNet(t, t.TempDir(), o)
	if _, err := n.Serve(context.Background(), "demo", http.NotFoundHandler()); err == nil {
		t.Fatal("Serve succeeded without a hostname")
	}
	if n.URL("demo") != "" {
		t.Error("a failed Serve left the slug served")
	}
}

func TestServeHonorsContextDuringMint(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	o := &fakeOpener{next: func(int, string) *fakeSession {
		return &fakeSession{mintFn: func() (string, error) { <-release; return "late.trycloudflare.com", nil }}
	}}
	n := newTestNet(t, t.TempDir(), o)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := n.Serve(ctx, "demo", http.NotFoundHandler()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Serve = %v, want the context's deadline", err)
	}
	if !o.session(0).canceled {
		t.Error("an abandoned mint was not cancelled")
	}
}

func TestReplayFailureMintsFresh(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "demo.json"), []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &fakeOpener{next: func(i int, spec string) *fakeSession {
		if spec != "" {
			return &fakeSession{mintFn: func() (string, error) { return "", errors.New("credential rejected") }}
		}
		return &fakeSession{host: "fresh.trycloudflare.com", spec: `{"fresh":true}`}
	}}
	n := newTestNet(t, dir, o)
	got, err := n.Serve(context.Background(), "demo", http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://fresh.trycloudflare.com" {
		t.Errorf("Serve = %q", got)
	}
}

func TestInvalidSlugsAreRefused(t *testing.T) {
	n := newTestNet(t, t.TempDir(), &fakeOpener{})
	for _, slug := range []string{"", "../x", "a/b", "UP"} {
		if _, err := n.Serve(context.Background(), slug, http.NotFoundHandler()); err == nil {
			t.Errorf("Serve(%q) succeeded", slug)
		}
		if err := RemoveRecord(t.TempDir(), slug); err == nil {
			t.Errorf("RemoveRecord(%q) succeeded", slug)
		}
	}
}

func TestNewIsolatesTheLibtunnelEnvironment(t *testing.T) {
	t.Setenv(v1.LocalURLEnv, "http://127.0.0.1:9")
	newTestNet(t, t.TempDir(), &fakeOpener{})
	if _, ok := v1.LookupEnv(v1.LocalURLEnv); ok {
		t.Error("libtunnel still reads the process environment after New")
	}
}

// A Stop that overlaps a replacement must close the replacement too: the
// supervisor may install it after Stop began, and Stop must not report the
// tunnel closed while that one runs.
func TestStopDuringReplacementClosesTheReplacement(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	o := &fakeOpener{next: func(i int, spec string) *fakeSession {
		if i == 0 {
			return &fakeSession{host: "t0.trycloudflare.com", spec: `{"tunnel":0}`}
		}
		return &fakeSession{host: "t1.trycloudflare.com", serializeFn: func() string {
			close(entered)
			<-release
			return `{"tunnel":1}`
		}}
	}}
	n := newTestNet(t, t.TempDir(), o)
	if _, err := n.Serve(context.Background(), "demo", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	o.session(0).emit(v1.EventGone)
	<-entered // the replacement is minted and being recorded
	stopped := make(chan error, 1)
	go func() { stopped <- n.Stop("demo") }()
	time.Sleep(50 * time.Millisecond) // let Stop begin before the replacement is installed
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if !o.session(1).isCanceled() {
		t.Fatal("Stop reported success while the replacement tunnel still runs")
	}
	if n.URL("demo") != "" {
		t.Error("Stop left the slug served")
	}
}

// A corrupt record must not crash the host: through the real libtunnel
// adapter, replaying it fails as an error, which Serve answers with a fresh
// mint. Only the replay is exercised here, so nothing contacts Cloudflare.
func TestCorruptRecordFailsWithoutPanic(t *testing.T) {
	n := newTestNet(t, t.TempDir(), &fakeOpener{})
	n.open = libtunnelOpener
	e := &entry{slug: "demo", stop: make(chan struct{})}
	for _, spec := range []string{`{"stale":true}`, "{not json", `{"backend":"other","spec":{}}`} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _, err := n.resolve(ctx, e, spec)
		cancel()
		if err == nil {
			t.Errorf("replaying %q succeeded", spec)
		}
	}
}

// A session whose hostname resolution panics fails that tunnel instead of
// the process.
func TestHostnamePanicBecomesAnError(t *testing.T) {
	o := &fakeOpener{next: func(int, string) *fakeSession {
		return &fakeSession{mintFn: func() (string, error) { panic("boom") }}
	}}
	n := newTestNet(t, t.TempDir(), o)
	if _, err := n.Serve(context.Background(), "demo", http.NotFoundHandler()); err == nil {
		t.Fatal("Serve succeeded after a panicking mint")
	}
}

func TestNewRefusesUnsafeRecordDirectories(t *testing.T) {
	t.Cleanup(func() { v1.SetEnvironment(nil) })
	for _, dir := range []string{"relative/dir", filepath.Join(t.TempDir(), "a..b")} {
		if _, err := New(Config{Dir: dir}); err == nil {
			t.Errorf("New accepted record directory %q", dir)
		}
	}
}
