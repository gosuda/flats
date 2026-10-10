package zrok

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
)

// fakeBackend is an in-memory zrok account. Listen returns a loopback
// listener so tests can make real HTTP requests to a served share.
type fakeBackend struct {
	mu       sync.Mutex
	names    map[string]string // name -> holder share token ("" = none)
	foreign  map[string]bool   // names another account owns
	shares   map[string]string // token -> name
	next     int
	lns      map[string]net.Listener
	listenFn func(token string) (net.Listener, error)

	unshareErr error
	shareErr   error
	createErr  error
	unshared   []string
	released   []string
	closed     bool
}

func newFake() *fakeBackend {
	return &fakeBackend{names: map[string]string{}, foreign: map[string]bool{}, shares: map[string]string{}, lns: map[string]net.Listener{}}
}

// key is how the fake stores a name: "<namespace>/<name>".
func key(namespace, name string) string { return namespace + "/" + name }

func (f *fakeBackend) CreateName(_ context.Context, namespace, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	if _, ok := f.names[key(namespace, name)]; ok || f.foreign[name] {
		return errNameExists
	}
	f.names[key(namespace, name)] = ""
	return nil
}

func (f *fakeBackend) NameHolder(_ context.Context, namespace, name string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	holder, ok := f.names[key(namespace, name)]
	return holder, ok, nil
}

func (f *fakeBackend) ReleaseName(_ context.Context, namespace, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.names, key(namespace, name))
	f.released = append(f.released, key(namespace, name))
	return nil
}

// Share returns a bare host name as the zrok controller does.
func (f *fakeBackend) Share(_ context.Context, namespace, name string) (string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.shareErr != nil {
		return "", nil, f.shareErr
	}
	k := key(namespace, name)
	if holder := f.names[k]; holder != "" {
		return "", nil, fmt.Errorf("name %q is held by share %s", name, holder)
	}
	f.next++
	token := fmt.Sprintf("tok%d", f.next)
	f.shares[token] = k
	f.names[k] = token
	return token, []string{name + "." + namespace + ".example"}, nil
}

func (f *fakeBackend) Unshare(ctx context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("unshare %s: %w", token, err)
	}
	if f.unshareErr != nil {
		return f.unshareErr
	}
	f.unshared = append(f.unshared, token)
	if k, ok := f.shares[token]; ok {
		delete(f.shares, token)
		if f.names[k] == token {
			f.names[k] = ""
		}
	}
	return nil
}

func (f *fakeBackend) Listen(token string) (net.Listener, error) {
	if f.listenFn != nil {
		return f.listenFn(token)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.lns[token] = ln
	f.mu.Unlock()
	return ln, nil
}

func (f *fakeBackend) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) addr(token string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ln := f.lns[token]; ln != nil {
		return ln.Addr().String()
	}
	return ""
}

func hello(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body+"|"+r.Header.Get("Tailscale-User-Login"))
	})
}

func waitState(t *testing.T, n *Net, slug, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, h := range n.Status().Hosts {
			if h.Host == slug && h.State == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not reach %s: %+v", slug, want, n.Status())
}

func get(t *testing.T, addr string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+addr+"/", nil)
	req.Header.Set("Tailscale-User-Login", "forged@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestServeReservesNameAndServes(t *testing.T) {
	f := newFake()
	n := newNet(Config{Dir: t.TempDir()}, f)
	defer n.Close()
	url, err := n.Serve(context.Background(), "blog", hello("v1"))
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://blog.public.example" || n.URL("blog") != url {
		t.Fatalf("url = %q, URL() = %q", url, n.URL("blog"))
	}
	waitState(t, n, "blog", stateReady)
	if got := get(t, f.addr("tok1")); got != "v1|" {
		t.Fatalf("body = %q (forged identity header must be stripped)", got)
	}
	// Serving again swaps the handler without a new share.
	if _, err := n.Serve(context.Background(), "blog", hello("v2")); err != nil {
		t.Fatal(err)
	}
	if got := get(t, f.addr("tok1")); got != "v2|" {
		t.Fatalf("body = %q", got)
	}
	if len(f.shares) != 1 {
		t.Fatalf("shares = %v", f.shares)
	}
	if st := n.Status(); st.Kind != "zrok" || st.Detail != "namespace public" {
		t.Fatalf("status = %+v", st)
	}
}

func TestStopKeepsNameAndReservesSameURL(t *testing.T) {
	f := newFake()
	n := newNet(Config{Dir: t.TempDir()}, f)
	defer n.Close()
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err != nil {
		t.Fatal(err)
	}
	waitState(t, n, "blog", stateReady)
	addr := f.addr("tok1")
	if err := n.Stop("blog"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.shares["tok1"]; ok {
		t.Fatal("share was not deleted")
	}
	if _, ok := f.names["public/blog"]; !ok {
		t.Fatal("Stop released the name")
	}
	if n.URL("blog") != "" || len(n.Status().Hosts) != 0 {
		t.Fatalf("stopped slug still reported: %+v", n.Status())
	}
	if _, err := http.Get("http://" + addr + "/"); err == nil {
		t.Fatal("listener still serves after Stop")
	}
	url, err := n.Serve(context.Background(), "blog", hello("v1"))
	if err != nil || url != "https://blog.public.example" {
		t.Fatalf("reserve again: %q, %v", url, err)
	}
	if err := n.Stop("missing"); err != nil {
		t.Fatalf("unknown slug: %v", err)
	}
}

func TestServeRemovesStaleShareItCreated(t *testing.T) {
	f := newFake()
	f.names["public/blog"] = "old"
	f.shares["old"] = "public/blog"
	n := newNet(Config{Dir: t.TempDir()}, f)
	defer n.Close()
	// A crashed process recorded the share it created.
	if err := n.writeRecord("blog", record{Namespace: "public", Created: true, Token: "old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err != nil {
		t.Fatal(err)
	}
	if len(f.unshared) != 1 || f.unshared[0] != "old" {
		t.Fatalf("unshared = %v", f.unshared)
	}
	rec, _, _ := n.readRecord("blog")
	if !rec.Created || rec.Token != "tok1" {
		t.Fatalf("record = %+v", rec)
	}
}

func TestServeLeavesShareFlatsDidNotCreate(t *testing.T) {
	f := newFake()
	f.names["public/blog"] = "operator"
	f.shares["operator"] = "public/blog"
	n := newNet(Config{Dir: t.TempDir()}, f)
	defer n.Close()
	_, err := n.Serve(context.Background(), "blog", hello("v1"))
	if err == nil || !strings.Contains(err.Error(), "Flats did not create") {
		t.Fatalf("err = %v", err)
	}
	if len(f.unshared) != 0 || f.names["public/blog"] != "operator" || n.URL("blog") != "" {
		t.Fatalf("an operator's share was touched: unshared=%v names=%v", f.unshared, f.names)
	}
}

func TestServeNameOwnedByAnotherAccount(t *testing.T) {
	f := newFake()
	f.foreign["blog"] = true
	n := newNet(Config{Dir: t.TempDir()}, f)
	defer n.Close()
	_, err := n.Serve(context.Background(), "blog", hello("v1"))
	if err == nil || !strings.Contains(err.Error(), TakenHint) {
		t.Fatalf("err = %v", err)
	}
}

func TestListenFailureReportsErrorAndRetries(t *testing.T) {
	f := newFake()
	var mu sync.Mutex
	fails := 2
	f.listenFn = func(string) (net.Listener, error) {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			return nil, errors.New("no edge routers")
		}
		return net.Listen("tcp", "127.0.0.1:0")
	}
	n := newNet(Config{Dir: t.TempDir()}, f)
	n.retryMin, n.retryMax = 50*time.Millisecond, 100*time.Millisecond
	defer n.Close()
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err != nil {
		t.Fatal(err)
	}
	waitState(t, n, "blog", stateError)
	if d := n.Status().Hosts[0].Detail; !strings.Contains(d, "no edge routers") || !strings.Contains(d, "retrying") {
		t.Fatalf("detail = %q", d)
	}
	waitState(t, n, "blog", stateReady)
	if err := n.Stop("blog"); err != nil {
		t.Fatal(err)
	}
	if len(f.shares) != 0 {
		t.Fatalf("shares = %v", f.shares)
	}
}

func TestClosedListenerIsRebound(t *testing.T) {
	f := newFake()
	n := newNet(Config{Dir: t.TempDir()}, f)
	n.retryMin, n.retryMax = 10*time.Millisecond, 10*time.Millisecond
	defer n.Close()
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err != nil {
		t.Fatal(err)
	}
	waitState(t, n, "blog", stateReady)
	f.mu.Lock()
	first := f.lns["tok1"]
	f.mu.Unlock()
	first.Close() // the overlay drops the binding
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		again := f.lns["tok1"]
		f.mu.Unlock()
		if again != first {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("share was not rebound")
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitState(t, n, "blog", stateReady)
	if got := get(t, f.addr("tok1")); got != "v1|" {
		t.Fatalf("body = %q", got)
	}
}

func TestStopDuringSlowBindDoesNotLeaveListener(t *testing.T) {
	f := newFake()
	release := make(chan struct{})
	var got net.Listener
	f.listenFn = func(string) (net.Listener, error) {
		<-release
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		got = ln
		return ln, err
	}
	n := newNet(Config{Dir: t.TempDir(), ShutdownTimeout: 5 * time.Second}, f)
	defer n.Close()
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- n.Stop("blog") }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("listener was never created")
	}
	if _, err := http.Get("http://" + got.Addr().String() + "/"); err == nil {
		t.Fatal("listener bound after Stop still serves")
	}
}

func TestStopFailureKeepsEntryForRetry(t *testing.T) {
	f := newFake()
	n := newNet(Config{Dir: t.TempDir()}, f)
	defer n.Close()
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err != nil {
		t.Fatal(err)
	}
	waitState(t, n, "blog", stateReady)
	f.mu.Lock()
	f.unshareErr = errors.New("controller unavailable")
	f.mu.Unlock()
	if err := n.Stop("blog"); err == nil {
		t.Fatal("Stop reported success while the share remains")
	}
	if n.URL("blog") == "" {
		t.Fatal("failed Stop forgot the share")
	}
	f.mu.Lock()
	f.unshareErr = nil
	f.mu.Unlock()
	if err := n.Stop("blog"); err != nil {
		t.Fatal(err)
	}
	if len(f.shares) != 0 {
		t.Fatalf("shares = %v", f.shares)
	}
}

func TestRetireReleasesName(t *testing.T) {
	f := newFake()
	n := newNet(Config{Dir: t.TempDir()}, f)
	defer n.Close()
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err != nil {
		t.Fatal(err)
	}
	if err := n.Retire("blog"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.names["public/blog"]; ok || len(f.shares) != 0 {
		t.Fatalf("names = %v shares = %v", f.names, f.shares)
	}
	// A name an earlier process recorded, still held by its share, is
	// released too.
	f.names["public/old"] = "tok-old"
	f.shares["tok-old"] = "public/old"
	if err := n.writeRecord("old", record{Namespace: "public", Created: true, Token: "tok-old"}); err != nil {
		t.Fatal(err)
	}
	if err := n.Retire("old"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.names["public/old"]; ok || len(f.shares) != 0 {
		t.Fatalf("names = %v shares = %v", f.names, f.shares)
	}
	if err := n.Retire("never"); err != nil {
		t.Fatalf("unknown name: %v", err)
	}
}

func TestCloseUnsharesAndKeepsNames(t *testing.T) {
	f := newFake()
	n := newNet(Config{Dir: t.TempDir()}, f)
	for _, slug := range []string{"a", "b"} {
		if _, err := n.Serve(context.Background(), slug, hello(slug)); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if len(f.shares) != 0 || len(f.names) != 2 || !f.closed {
		t.Fatalf("shares = %v names = %v closed = %t", f.shares, f.names, f.closed)
	}
	if _, err := n.Serve(context.Background(), "c", hello("c")); err == nil {
		t.Fatal("Serve after Close succeeded")
	}
	if err := n.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestShareFailureLeavesNothingServed(t *testing.T) {
	f := newFake()
	f.shareErr = errors.New("share limit reached")
	n := newNet(Config{Dir: t.TempDir()}, f)
	defer n.Close()
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err == nil || !strings.Contains(err.Error(), "share limit reached") {
		t.Fatalf("err = %v", err)
	}
	if n.URL("blog") != "" || len(n.Status().Hosts) != 0 {
		t.Fatal("failed share is reported as served")
	}
}

func TestLoadRootRequiresEnabledEnvironment(t *testing.T) {
	_, err := loadRoot(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "zrok2 enable") {
		t.Fatalf("err = %v", err)
	}
}

func TestReservedNameRecordOutlivesRestartUntilRetire(t *testing.T) {
	f := newFake()
	dir := t.TempDir()
	n := newNet(Config{Dir: dir}, f)
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err != nil {
		t.Fatal(err)
	}
	if !reserved(t, n, "blog") || reserved(t, n, "other") {
		t.Fatal("reservation record missing")
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	// A new process, for example after the flat lost its zrok permission,
	// still knows it reserved the name and releases it.
	n = newNet(Config{Dir: dir}, f)
	defer n.Close()
	if !reserved(t, n, "blog") {
		t.Fatal("record did not survive a restart")
	}
	if err := n.Retire("blog"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.names["public/blog"]; ok || reserved(t, n, "blog") {
		t.Fatalf("name or record kept after Retire: names=%v", f.names)
	}
}

func TestFailedReservationLeavesNoRecord(t *testing.T) {
	f := newFake()
	f.foreign["blog"] = true
	n := newNet(Config{Dir: t.TempDir()}, f)
	defer n.Close()
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err == nil {
		t.Fatal("served a name another account owns")
	}
	if reserved(t, n, "blog") {
		t.Fatal("a name another account owns was recorded as reserved")
	}
	if err := n.Retire("blog"); err != nil || len(f.released) != 0 {
		t.Fatalf("Retire: %v, released %v", err, f.released)
	}
}

func TestRetireKeepsNamesFlatsDidNotCreate(t *testing.T) {
	f := newFake()
	f.names["public/blog"] = "" // the operator created it with zrok2 create name
	n := newNet(Config{Dir: t.TempDir()}, f)
	defer n.Close()
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err != nil {
		t.Fatal(err)
	}
	if err := n.Retire("blog"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.names["public/blog"]; !ok || len(f.released) != 0 || reserved(t, n, "blog") {
		t.Fatalf("names=%v released=%v reserved=%t", f.names, f.released, reserved(t, n, "blog"))
	}
}

func TestRetireLeavesNameAnotherShareUses(t *testing.T) {
	f := newFake()
	n := newNet(Config{Dir: t.TempDir()}, f)
	defer n.Close()
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err != nil {
		t.Fatal(err)
	}
	if err := n.Stop("blog"); err != nil {
		t.Fatal(err)
	}
	// The operator then shares something else under the name.
	f.names["public/blog"] = "operator"
	f.shares["operator"] = "public/blog"
	if err := n.Retire("blog"); err != nil {
		t.Fatal(err)
	}
	if f.names["public/blog"] != "operator" || len(f.released) != 0 || slices.Contains(f.unshared, "operator") {
		t.Fatalf("names=%v released=%v unshared=%v", f.names, f.released, f.unshared)
	}
}

func TestNamespaceChangeReleasesOldName(t *testing.T) {
	f := newFake()
	dir := t.TempDir()
	n := newNet(Config{Dir: dir, Namespace: "old-ns"}, f)
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	n = newNet(Config{Dir: dir, Namespace: "new-ns"}, f)
	defer n.Close()
	url, err := n.Serve(context.Background(), "blog", hello("v1"))
	if err != nil || url != "https://blog.new-ns.example" {
		t.Fatalf("serve in the new namespace: %q, %v", url, err)
	}
	if _, ok := f.names["old-ns/blog"]; ok || !slices.Equal(f.released, []string{"old-ns/blog"}) {
		t.Fatalf("old name kept: names=%v released=%v", f.names, f.released)
	}
	if err := n.Retire("blog"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.names["new-ns/blog"]; ok {
		t.Fatalf("new name kept: %v", f.names)
	}
}

func TestAbsoluteURL(t *testing.T) {
	for in, want := range map[string]string{
		"blog.share.zrok.io":         "https://blog.share.zrok.io",
		"https://blog.share.zrok.io": "https://blog.share.zrok.io",
		"http://blog.zrok.test:8080": "http://blog.zrok.test:8080",
		"":                           "",
	} {
		if got := absoluteURL(in); got != want {
			t.Errorf("absoluteURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStopDrainsRequestFromLostListener(t *testing.T) {
	f := newFake()
	n := newNet(Config{Dir: t.TempDir(), ShutdownTimeout: 5 * time.Second}, f)
	n.retryMin, n.retryMax = 10*time.Millisecond, 10*time.Millisecond
	defer n.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, "done")
	})
	if _, err := n.Serve(context.Background(), "blog", slow); err != nil {
		t.Fatal(err)
	}
	waitState(t, n, "blog", stateReady)
	f.mu.Lock()
	first := f.lns["tok1"]
	f.mu.Unlock()
	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + first.Addr().String() + "/")
		if err != nil {
			body <- err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body <- string(b)
	}()
	<-started
	first.Close() // the overlay drops the binding mid-request
	stopped := make(chan error, 1)
	go func() { stopped <- n.Stop("blog") }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned while a request was in flight: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if got := <-body; got != "done" {
		t.Fatalf("in-flight request = %q", got)
	}
}

func TestNameConflictKeepsControllerReason(t *testing.T) {
	if err := nameConflict("blog", ""); !errors.Is(err, errNameExists) {
		t.Fatalf("existing name: %v", err)
	}
	err := nameConflict("blog", "names limit reached; cannot reserve additional names")
	if errors.Is(err, errNameExists) || !strings.Contains(err.Error(), "names limit reached") {
		t.Fatalf("limit: %v", err)
	}
}

func reserved(t *testing.T, n *Net, slug string) bool {
	t.Helper()
	ok, err := n.Reserved(slug)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestAmbiguousNameCreationStaysRecorded(t *testing.T) {
	f := newFake()
	n := newNet(Config{Dir: t.TempDir()}, f)
	defer n.Close()
	f.createErr = errors.New("controller timed out")
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err == nil {
		t.Fatal("Serve succeeded without a name")
	}
	if !reserved(t, n, "blog") {
		t.Fatal("an attempted creation was not recorded")
	}
	// The controller did create it after all.
	f.names["public/blog"] = ""
	if err := n.Retire("blog"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.names["public/blog"]; ok || reserved(t, n, "blog") {
		t.Fatalf("name kept: %v", f.names)
	}
}

func TestUnrecordedShareIsRemovedLater(t *testing.T) {
	f := newFake()
	n := newNet(Config{Dir: t.TempDir()}, f)
	defer n.Close()
	n.failWrite = func(rec record) error {
		if rec.Token != "" {
			return errors.New("disk full")
		}
		return nil
	}
	f.unshareErr = errors.New("controller unavailable")
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	if f.names["public/blog"] != "tok1" {
		t.Fatalf("names = %v", f.names)
	}
	n.failWrite, f.unshareErr = nil, nil
	if _, err := n.Serve(context.Background(), "blog", hello("v1")); err != nil {
		t.Fatalf("the share left by the failed rollback blocked Serve: %v", err)
	}
	if !slices.Contains(f.unshared, "tok1") {
		t.Fatalf("unshared = %v", f.unshared)
	}
}

func TestUnshareGetsItsOwnDeadlineAfterDrainTimeout(t *testing.T) {
	f := newFake()
	n := newNet(Config{Dir: t.TempDir(), ShutdownTimeout: 200 * time.Millisecond}, f)
	defer n.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})
	if _, err := n.Serve(context.Background(), "blog", slow); err != nil {
		t.Fatal(err)
	}
	waitState(t, n, "blog", stateReady)
	go http.Get("http://" + f.addr("tok1") + "/")
	<-started
	if err := n.Stop("blog"); err == nil {
		t.Fatal("Stop hid the drain timeout")
	}
	if !slices.Contains(f.unshared, "tok1") {
		t.Fatalf("share kept after the drain timed out: unshared = %v", f.unshared)
	}
}

func TestHasRecordFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if ok, err := HasRecord(dir, "blog"); ok || err != nil {
		t.Fatalf("missing record: %v %v", ok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blog"), []byte(`{"namespace":"public"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := HasRecord(dir, "blog"); !ok || err != nil {
		t.Fatalf("present record: %v %v", ok, err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	if _, err := HasRecord(dir, "other"); err == nil {
		t.Fatal("an unreadable record directory was reported as no record")
	}
}
