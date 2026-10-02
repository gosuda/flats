package portal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/portal-tunnel/v2/sdk"
	"github.com/gosuda/portal-tunnel/v2/types"
	zlog "github.com/rs/zerolog/log"

	"github.com/oesni/flats/internal/core"
)

// fakeExposure is a loopback TCP listener standing in for a Portal exposure,
// so the real sdk.RunHTTP serves the handler and tests can GET it.
type fakeExposure struct {
	net.Listener
	id     types.Identity
	relays []string
	max    int

	mu       sync.Mutex
	statuses []sdk.RelayStatus
	metas    []types.LeaseMetadata
	closes   int
	changed  chan struct{}
	updates  chan sdk.RelayStatus
	metaGate chan struct{} // if set, UpdateMetadata waits for it
}

func (f *fakeExposure) Relays() []sdk.RelayStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.statuses)
}

func (f *fakeExposure) Updates() <-chan sdk.RelayStatus { return f.updates }

func (f *fakeExposure) UpdateMetadata(m types.LeaseMetadata) error {
	if f.metaGate != nil {
		<-f.metaGate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closes > 0 {
		return net.ErrClosed
	}
	f.metas = append(f.metas, m)
	return nil
}

func (f *fakeExposure) WaitReady(ctx context.Context) ([]sdk.RelayStatus, error) {
	for {
		f.mu.Lock()
		var ready []sdk.RelayStatus
		for _, s := range f.statuses {
			if s.State == sdk.RelayReady {
				ready = append(ready, s)
			}
		}
		closed, ch := f.closes > 0, f.changed
		f.mu.Unlock()
		if len(ready) > 0 {
			return ready, nil
		}
		if closed {
			return nil, net.ErrClosed
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch:
		}
	}
}

func (f *fakeExposure) Close() error {
	f.mu.Lock()
	f.closes++
	f.mu.Unlock()
	f.Listener.Close()
	return nil
}

// set replaces the relay snapshot and, like the SDK, pushes the last
// changed status to Updates (best-effort, buffer of one).
func (f *fakeExposure) set(sts ...sdk.RelayStatus) {
	f.mu.Lock()
	f.statuses = sts
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
	if len(sts) > 0 {
		select {
		case f.updates <- sts[len(sts)-1]:
		default:
		}
	}
}

func (f *fakeExposure) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

func (f *fakeExposure) metadata() []types.LeaseMetadata {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.metas)
}

type fakeFactory struct {
	mu       sync.Mutex
	exps     []*fakeExposure
	onExpose func() // runs inside expose, after the exposure exists
}

func (ff *fakeFactory) expose(_ context.Context, id types.Identity, relays []string, maxActive int, meta types.LeaseMetadata) (exposure, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &fakeExposure{Listener: ln, id: id, relays: relays, max: maxActive,
		metas: []types.LeaseMetadata{meta}, changed: make(chan struct{}), updates: make(chan sdk.RelayStatus, 1)}
	ff.mu.Lock()
	ff.exps = append(ff.exps, f)
	hook := ff.onExpose
	ff.mu.Unlock()
	if hook != nil {
		hook()
	}
	return f, nil
}

func (ff *fakeFactory) last(t *testing.T) *fakeExposure {
	t.Helper()
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if len(ff.exps) == 0 {
		t.Fatal("no exposure created")
	}
	return ff.exps[len(ff.exps)-1]
}

func (ff *fakeFactory) count() int {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return len(ff.exps)
}

func newTestNet(t *testing.T, cfg Config) (*Net, *fakeFactory) {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ff := &fakeFactory{}
	n.expose = ff.expose
	t.Cleanup(func() { n.Close() })
	return n, ff
}

func hello(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
}

func ready(relay, public string) sdk.RelayStatus {
	return sdk.RelayStatus{RelayURL: relay, PublicURL: public, State: sdk.RelayReady}
}

func TestIdentityPersistedAndReused(t *testing.T) {
	dir := t.TempDir()
	n, ff := newTestNet(t, Config{Dir: dir, Discovery: true})
	if _, err := n.Serve(t.Context(), "blog", hello("x"), false); err != nil {
		t.Fatal(err)
	}
	first := ff.last(t).id
	if first.Name != "blog" || first.Address == "" || first.PrivateKey == "" {
		t.Fatalf("identity = name %q address %q", first.Name, first.Address)
	}
	path := filepath.Join(dir, "blog.json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode = %v, want 0600", fi.Mode().Perm())
	}

	if err := n.Stop("blog"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Serve(t.Context(), "blog", hello("x"), false); err != nil {
		t.Fatal(err)
	}
	if got := ff.last(t).id.Address; got != first.Address {
		t.Fatalf("re-serve address = %s, want %s", got, first.Address)
	}

	// A new process (new Net) over the same directory keeps the hostname,
	// and a loose mode on an existing file is tightened.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	n2, ff2 := newTestNet(t, Config{Dir: dir, Discovery: true})
	if _, err := n2.Serve(t.Context(), "blog", hello("x"), false); err != nil {
		t.Fatal(err)
	}
	if got := ff2.last(t).id; got.Address != first.Address || got.Name != "blog" {
		t.Fatalf("restart identity = %s/%s, want blog/%s", got.Name, got.Address, first.Address)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode after load = %v", fi.Mode().Perm())
	}

	// Different slugs get different keys.
	if _, err := n2.Serve(t.Context(), "shop", hello("x"), false); err != nil {
		t.Fatal(err)
	}
	if ff2.last(t).id.Address == first.Address {
		t.Fatal("two slugs share an identity")
	}
}

func TestServeRejectsBadSlug(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	for _, s := range []string{"", "../x", "a/b", ".hidden"} {
		if _, err := n.Serve(t.Context(), s, hello("x"), false); err == nil {
			t.Errorf("Serve(%q) succeeded", s)
		}
	}
	if ff.count() != 0 {
		t.Fatal("exposure created for a bad slug")
	}
}

func TestConfigPlumbing(t *testing.T) {
	n, ff := newTestNet(t, Config{Relays: []string{"https://s-h.day/", "", "s-h.day", "https://kakashit.org"}, Discovery: true})
	if _, err := n.Serve(t.Context(), "blog", hello("x"), true); err != nil {
		t.Fatal(err)
	}
	f := ff.last(t)
	if want := []string{"https://s-h.day", "https://kakashit.org"}; !slices.Equal(f.relays, want) {
		t.Fatalf("relays = %v, want %v", f.relays, want)
	}
	if f.max != DefaultMaxActiveRelays {
		t.Fatalf("max active = %d", f.max)
	}
	if m := f.metadata()[0]; !m.Hide || m.Description != "Flats: blog" {
		t.Fatalf("initial metadata = %+v", m)
	}

	n2, ff2 := newTestNet(t, Config{MaxActiveRelays: 7, Discovery: true})
	if _, err := n2.Serve(t.Context(), "blog", hello("x"), false); err != nil {
		t.Fatal(err)
	}
	if f := ff2.last(t); f.max != 7 || len(f.relays) != 0 || f.metadata()[0].Hide {
		t.Fatalf("relays=%v max=%d meta=%+v", f.relays, f.max, f.metadata()[0])
	}
	if !strings.Contains(n2.Status().Detail, "Portal defaults") {
		t.Fatalf("detail = %q", n2.Status().Detail)
	}

	if _, err := New(Config{Dir: t.TempDir(), Relays: []string{"ftp://bad relay"}}); err == nil {
		t.Fatal("bad relay accepted")
	}
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty Dir accepted")
	}
}

// With discovery off, an exposure uses only the explicit relays: the SDK gets
// no WithDiscovery option (maxActive 0), whatever MaxActiveRelays says.
func TestDiscoveryOff(t *testing.T) {
	n, ff := newTestNet(t, Config{Relays: []string{"https://s-h.day"}, MaxActiveRelays: 5})
	if _, err := n.Serve(t.Context(), "blog", hello("x"), false); err != nil {
		t.Fatal(err)
	}
	if f := ff.last(t); f.max != 0 || !slices.Equal(f.relays, []string{"https://s-h.day"}) {
		t.Fatalf("relays=%v max=%d, want only the explicit relay without discovery", f.relays, f.max)
	}
	if d := n.Status().Detail; !strings.Contains(d, "discovery off") || strings.Contains(d, "more") {
		t.Fatalf("detail = %q", d)
	}
	if _, err := New(Config{Dir: t.TempDir()}); err == nil {
		t.Fatal("discovery off without relays accepted")
	}
}

func TestServeServesHandlerAndStripsIdentity(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "proto=%s login=%q", r.Header.Get("X-Forwarded-Proto"), r.Header.Get("Tailscale-User-Login"))
	})
	if _, err := n.Serve(t.Context(), "blog", h, false); err != nil {
		t.Fatal(err)
	}
	addr := ff.last(t).Addr().String()
	req, _ := http.NewRequest("GET", "http://"+addr+"/", nil)
	req.Header.Set("Tailscale-User-Login", "mallory@example.com")
	req.Header.Set("X-Forwarded-Proto", "http")
	body := get(t, req)
	if body != `proto=https login=""` {
		t.Fatalf("body = %s", body)
	}

	// Serving again swaps the handler on the same exposure.
	if _, err := n.Serve(t.Context(), "blog", hello("v2"), false); err != nil {
		t.Fatal(err)
	}
	if ff.count() != 1 {
		t.Fatalf("re-serve created %d exposures", ff.count())
	}
	req, _ = http.NewRequest("GET", "http://"+addr+"/", nil)
	if body := get(t, req); body != "v2" {
		t.Fatalf("after swap body = %s", body)
	}
}

func get(t *testing.T, req *http.Request) string {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestSetHidden(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	if err := n.SetHidden("blog", true); err == nil {
		t.Fatal("SetHidden on unknown slug succeeded")
	}
	if _, err := n.Serve(t.Context(), "blog", hello("x"), false); err != nil {
		t.Fatal(err)
	}
	f := ff.last(t)
	for _, h := range []bool{true, true, false} {
		if err := n.SetHidden("blog", h); err != nil {
			t.Fatal(err)
		}
	}
	// Unchanged values are not re-sent.
	got := f.metadata()
	if len(got) != 3 || got[0].Hide || !got[1].Hide || got[2].Hide {
		t.Fatalf("metadata history = %+v", got)
	}
	for _, m := range got {
		if m.Description != "Flats: blog" {
			t.Fatalf("description = %q", m.Description)
		}
	}

	// Re-serving with a different hidden value updates the listing too.
	if _, err := n.Serve(t.Context(), "blog", hello("x"), true); err != nil {
		t.Fatal(err)
	}
	if got := f.metadata(); !got[len(got)-1].Hide {
		t.Fatal("re-serve did not apply hidden")
	}
	if hi := n.Status().Hosts[0]; !strings.Contains(hi.Detail, "unlisted") {
		t.Fatalf("detail = %q", hi.Detail)
	}
}

func TestStatusBookkeeping(t *testing.T) {
	n, ff := newTestNet(t, Config{Relays: []string{"https://s-h.day"}, Discovery: true})
	if _, err := n.Serve(t.Context(), "blog", hello("x"), false); err != nil {
		t.Fatal(err)
	}
	f := ff.last(t)

	st := n.Status()
	if st.Kind != "portal" || !st.Enabled || len(st.Hosts) != 1 {
		t.Fatalf("status = %+v", st)
	}
	if hi := st.Hosts[0]; hi.Host != "blog" || hi.State != "starting" || hi.URL != "" {
		t.Fatalf("initial host = %+v", hi)
	}
	if n.URL("blog") != "" {
		t.Fatal("URL before ready")
	}

	f.set(sdk.RelayStatus{RelayURL: "https://s-h.day", State: sdk.RelayConnecting})
	if hi := n.Status().Hosts[0]; hi.State != "starting" || !strings.Contains(hi.Detail, "connecting to s-h.day") {
		t.Fatalf("connecting host = %+v", hi)
	}

	// A discovered relay sorting before the explicit one must not take over URL().
	f.set(ready("https://a-relay.example", "https://blog.a-relay.example"), ready("https://s-h.day", "https://blog.s-h.day"))
	if got := n.URL("blog"); got != "https://blog.s-h.day" {
		t.Fatalf("URL = %s", got)
	}
	if got := n.URLs("blog"); !slices.Equal(got, []string{"https://blog.s-h.day", "https://blog.a-relay.example"}) {
		t.Fatalf("URLs = %v", got)
	}
	if hi := n.Status().Hosts[0]; hi.State != "ready" || hi.URL != "https://blog.s-h.day" {
		t.Fatalf("ready host = %+v", hi)
	}

	// A hostname conflict on another relay is reported with the fix hint even
	// after discovery drops that relay from the snapshot.
	conflict := sdk.RelayStatus{RelayURL: "https://other.example", State: sdk.RelayFailed, Failure: sdk.RelayFailureTerminal,
		Err: fmt.Errorf("register: %w", &types.APIRequestError{StatusCode: 409, Code: types.APIErrorCodeHostnameConflict, Message: "taken"})}
	f.set(ready("https://s-h.day", "https://blog.s-h.day"), conflict)
	waitFor(t, func() bool { return strings.Contains(n.Status().Detail, ConflictHint) })
	deselected := conflict
	deselected.Deselected = true
	f.set(ready("https://s-h.day", "https://blog.s-h.day"))
	f.updates <- deselected
	time.Sleep(50 * time.Millisecond)
	st = n.Status()
	if hi := st.Hosts[0]; hi.State != "ready" || !strings.Contains(hi.Detail, "other.example: hostname conflict: "+ConflictHint) {
		t.Fatalf("host after deselection = %+v", hi)
	}
	if !strings.Contains(st.Detail, "hostname conflict for blog") {
		t.Fatalf("net detail = %q", st.Detail)
	}

	// With no relay ready, a conflict makes the host an error.
	f.set(sdk.RelayStatus{RelayURL: "https://s-h.day", State: sdk.RelayFailed, Failure: sdk.RelayFailureTerminal,
		Err: &types.APIRequestError{StatusCode: 409, Code: types.APIErrorCodeHostnameConflict}})
	waitFor(t, func() bool { return n.Status().Hosts[0].State == "error" })
	if n.URL("blog") != "" {
		t.Fatal("URL while failed")
	}

	// Recovery clears the recorded failure.
	f.set(ready("https://s-h.day", "https://blog.s-h.day"))
	waitFor(t, func() bool {
		hi := n.Status().Hosts[0]
		return hi.State == "ready" && !strings.Contains(hi.Detail, "s-h.day: hostname conflict")
	})
}

func TestStatusRuntimeFailureIsStarting(t *testing.T) {
	n, ff := newTestNet(t, Config{Relays: []string{"https://s-h.day"}, Discovery: true})
	if _, err := n.Serve(t.Context(), "blog", hello("x"), false); err != nil {
		t.Fatal(err)
	}
	ff.last(t).set(sdk.RelayStatus{RelayURL: "https://s-h.day", State: sdk.RelayFailed, Failure: sdk.RelayFailureRuntime, Err: errors.New("dial tcp: timeout")})
	hi := n.Status().Hosts[0]
	if hi.State != "starting" || !strings.Contains(hi.Detail, "dial tcp: timeout (retrying)") {
		t.Fatalf("host = %+v", hi)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * pollInterval)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWaitReady(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	if _, err := n.WaitReady(t.Context(), "blog"); err == nil {
		t.Fatal("WaitReady on unknown slug succeeded")
	}
	if _, err := n.Serve(t.Context(), "blog", hello("x"), false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := n.WaitReady(ctx, "blog"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		ff.last(t).set(ready("https://s-h.day", "https://blog.s-h.day"))
	}()
	urls, err := n.WaitReady(t.Context(), "blog")
	if err != nil || !slices.Equal(urls, []string{"https://blog.s-h.day"}) {
		t.Fatalf("WaitReady = %v, %v", urls, err)
	}
}

func TestStopIdempotentAndClose(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	if err := n.Stop("nope"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Serve(t.Context(), "blog", hello("x"), false); err != nil {
		t.Fatal(err)
	}
	f := ff.last(t)
	f.set(ready("https://s-h.day", "https://blog.s-h.day"))
	addr := f.Addr().String()

	// RunHTTP's shutdown closes only the drain listener; Stop closes
	// (unregisters) the exposure itself, exactly once.
	if err := n.Stop("blog"); err != nil {
		t.Fatal(err)
	}
	if c := f.closeCount(); c != 1 {
		t.Fatalf("exposure closed %d times, want 1", c)
	}
	if err := n.Stop("blog"); err != nil {
		t.Fatal(err)
	}
	if c := f.closeCount(); c != 1 {
		t.Fatalf("second Stop: exposure closed %d times, want 1", c)
	}
	if n.URL("blog") != "" || len(n.Status().Hosts) != 0 {
		t.Fatal("status kept after Stop")
	}
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Fatal("handler still reachable after Stop")
	}

	// Close stops every exposure and refuses new ones.
	for _, s := range []string{"a", "b"} {
		if _, err := n.Serve(t.Context(), s, hello("x"), false); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	for _, f := range ff.exps[1:] {
		if f.closeCount() == 0 {
			t.Fatal("exposure not closed by Close")
		}
	}
	if _, err := n.Serve(t.Context(), "c", hello("x"), false); err == nil {
		t.Fatal("Serve after Close succeeded")
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestServeDoesNotBindRequestContext(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	ctx, cancel := context.WithCancel(t.Context())
	if _, err := n.Serve(ctx, "blog", hello("alive"), false); err != nil {
		t.Fatal(err)
	}
	cancel()
	req, _ := http.NewRequest("GET", "http://"+ff.last(t).Addr().String()+"/", nil)
	if body := get(t, req); body != "alive" {
		t.Fatalf("body = %s", body)
	}
	if _, err := n.Serve(ctx, "other", hello("x"), false); err == nil {
		t.Fatal("Serve with cancelled ctx succeeded")
	}
}

func TestSDKLogBridge(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	installSDKLogger(logf)
	got := formatSDKLog([]byte(`{"level":"warn","relay_url":"https://s-h.day","error":"boom","time":"x","message":"add relay listener"}` + "\n"))
	if got != "portal sdk warn: add relay listener error=boom relay_url=https://s-h.day" {
		t.Fatalf("formatted = %q", got)
	}
	if formatSDKLog([]byte("not json\n")) != "portal sdk: not json" {
		t.Fatal("non-JSON line")
	}

	// Through the real zerolog global used by the SDK: info is dropped, warn forwarded.
	zlog.Info().Msg("i")
	zlog.Warn().Str("relay_url", "r").Msg("w")
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || lines[0] != "portal sdk warn: w relay_url=r" {
		t.Fatalf("lines = %q", lines)
	}
}

// Stop must let an in-flight public request finish before it unregisters
// the exposure (closing it drops the relay route).
func TestStopDrainsBeforeUnregister(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // runs before newTestNet's Close, also on failure
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, "finished")
	})
	if _, err := n.Serve(t.Context(), "blog", h, false); err != nil {
		t.Fatal(err)
	}
	f := ff.last(t)
	req, _ := http.NewRequest("GET", "http://"+f.Addr().String()+"/", nil)
	body := make(chan string, 1)
	go func() {
		c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := c.Do(req)
		if err != nil {
			body <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the handler")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- n.Stop("blog") }()
	time.Sleep(100 * time.Millisecond)
	if c := f.closeCount(); c != 0 {
		t.Fatal("exposure closed (unregistered) while a request was in flight")
	}
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned before the request finished: %v", err)
	default:
	}
	unblock()
	if got := <-body; got != "finished" {
		t.Fatalf("in-flight body = %q", got)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if c := f.closeCount(); c != 1 {
		t.Fatalf("exposure closed %d times, want 1", c)
	}
}

// A Serve that is creating its exposure while Close runs must not leave a
// live entry behind.
func TestServeRacingClose(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	ff.onExpose = func() {
		if err := n.Close(); err != nil {
			t.Error(err)
		}
	}
	if _, err := n.Serve(t.Context(), "blog", hello("x"), false); err == nil {
		t.Fatal("Serve succeeded although Close ran during it")
	}
	if c := ff.last(t).closeCount(); c != 1 {
		t.Fatalf("exposure closed %d times, want 1", c)
	}
	if hs := n.Status().Hosts; len(hs) != 0 {
		t.Fatalf("hosts after Close = %+v", hs)
	}
}

// SetHidden waits on the SDK's reconcile lock; Status must not wait with it.
func TestSetHiddenDoesNotBlockStatus(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	if _, err := n.Serve(t.Context(), "blog", hello("x"), false); err != nil {
		t.Fatal(err)
	}
	f := ff.last(t)
	f.metaGate = make(chan struct{})
	var openOnce sync.Once
	openGate := func() { openOnce.Do(func() { close(f.metaGate) }) }
	t.Cleanup(openGate)
	done := make(chan error, 1)
	go func() { done <- n.SetHidden("blog", true) }()
	time.Sleep(20 * time.Millisecond)

	got := make(chan core.NetStatus, 1)
	go func() { got <- n.Status() }()
	select {
	case st := <-got:
		if strings.Contains(st.Hosts[0].Detail, "unlisted") {
			t.Fatal("unlisted reported before UpdateMetadata succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Status blocked behind SetHidden")
	}
	openGate()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(n.Status().Hosts[0].Detail, "unlisted") {
		t.Fatal("unlisted missing after SetHidden")
	}
}

// With discovery only, URL() keeps the relay it returned first while that
// relay stays ready, even when a relay that sorts earlier becomes ready.
func TestURLStaysOnPrimary(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	if _, err := n.Serve(t.Context(), "blog", hello("x"), false); err != nil {
		t.Fatal(err)
	}
	f := ff.last(t)
	f.set(ready("https://m-relay.example", "https://blog.m-relay.example"))
	if got := n.URL("blog"); got != "https://blog.m-relay.example" {
		t.Fatalf("URL = %s", got)
	}
	f.set(ready("https://a-relay.example", "https://blog.a-relay.example"), ready("https://m-relay.example", "https://blog.m-relay.example"))
	if got := n.URL("blog"); got != "https://blog.m-relay.example" {
		t.Fatalf("URL after discovery added a relay = %s", got)
	}
	if hi := n.Status().Hosts[0]; hi.URL != "https://blog.m-relay.example" {
		t.Fatalf("status URL = %s", hi.URL)
	}
	if got := n.URLs("blog"); !slices.Equal(got, []string{"https://blog.m-relay.example", "https://blog.a-relay.example"}) {
		t.Fatalf("URLs = %v", got)
	}
	// The primary drops out: the next ready relay takes over and keeps it.
	f.set(ready("https://a-relay.example", "https://blog.a-relay.example"),
		sdk.RelayStatus{RelayURL: "https://m-relay.example", State: sdk.RelayConnecting})
	if got := n.URL("blog"); got != "https://blog.a-relay.example" {
		t.Fatalf("URL after primary dropped = %s", got)
	}
	f.set(ready("https://a-relay.example", "https://blog.a-relay.example"), ready("https://m-relay.example", "https://blog.m-relay.example"))
	if got := n.URL("blog"); got != "https://blog.a-relay.example" {
		t.Fatalf("URL after old primary returned = %s", got)
	}
}
