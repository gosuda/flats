package provider

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
)

// heldTail is a tailnet whose Serve and Stop block for the hosts a test
// holds, once per host, until the test releases them. It is safe for
// concurrent use.
type heldTail struct {
	mu      sync.Mutex
	serve   map[string]*hold
	stop    map[string]*hold
	all     []*hold
	stopped []string
	ops     sync.WaitGroup // operations the test started with async
}

type hold struct {
	entered chan struct{}
	release func()
	gate    chan struct{}
}

func newHold() *hold {
	h := &hold{entered: make(chan struct{}), gate: make(chan struct{})}
	h.release = sync.OnceFunc(func() { close(h.gate) })
	return h
}

func (f *heldTail) holdServe(host string) *hold {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.serve == nil {
		f.serve = map[string]*hold{}
	}
	h := newHold()
	f.serve[host] = h
	f.all = append(f.all, h)
	return h
}

func (f *heldTail) holdStop(host string) *hold {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stop == nil {
		f.stop = map[string]*hold{}
	}
	h := newHold()
	f.stop[host] = h
	f.all = append(f.all, h)
	return h
}

// releaseAll releases every hold the test set, consumed or not.
func (f *heldTail) releaseAll() {
	f.mu.Lock()
	all := slices.Clone(f.all)
	f.mu.Unlock()
	for _, h := range all {
		h.release()
	}
}

// wait blocks on the hold set for host in holds, consuming it.
func (f *heldTail) wait(holds map[string]*hold, host string) {
	f.mu.Lock()
	h := holds[host]
	delete(holds, host)
	f.mu.Unlock()
	if h != nil {
		close(h.entered)
		<-h.gate
	}
}

func (f *heldTail) Serve(_ context.Context, host string, _ http.Handler, _ bool) (string, error) {
	f.wait(f.serve, host)
	return "https://" + host + ".example.ts.net", nil
}

func (f *heldTail) Stop(host string) error {
	f.wait(f.stop, host)
	f.mu.Lock()
	f.stopped = append(f.stopped, host)
	f.mu.Unlock()
	return nil
}

func (f *heldTail) URL(host string) string { return "https://" + host + ".example.ts.net" }
func (f *heldTail) Status() core.NetStatus {
	return core.NetStatus{Kind: "tailscale", Enabled: true}
}
func (f *heldTail) Close() error { return nil }
func (f *heldTail) ServeFunnel(context.Context, string, http.Handler) (string, error) {
	return "", errors.New("funnel is not used here")
}
func (f *heldTail) StopFunnel(string) error { return nil }
func (f *heldTail) FunnelState(string) ExposureEndpoint {
	return ExposureEndpoint{Provider: Funnel, State: stateUnavailable}
}

func heldTailManager(t *testing.T) (*Manager, *heldTail) {
	t.Helper()
	dir := t.TempDir()
	f := File{Version: 1, Permitted: []ID{Tailscale}}
	f.Migration.GrantsFromState = []ID{}
	if err := Save(dir, f); err != nil {
		t.Fatal(err)
	}
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	tail := &heldTail{}
	// Runs before the listener closes, also after a failed assertion: no
	// operation the test started outlives it.
	t.Cleanup(func() {
		tail.releaseAll()
		done := make(chan struct{})
		go func() { tail.ops.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("operations started by the test did not finish")
		}
	})
	m, err := New(dir, Options{Local: ln, Tailscale: tail})
	if err != nil {
		t.Fatal(err)
	}
	return m, tail
}

func tailnetRequest(slug string) ExposureRequest {
	return ExposureRequest{
		Slug: slug, Host: slug, Visibility: "private", Audience: AudienceCurrent,
		Handler: text(slug), Permitted: []ID{Tailscale},
	}
}

// async runs fn in a goroutine tracked by m's heldTail and returns its
// result channel.
func async(m *Manager, fn func() error) <-chan error {
	tail := m.tailnet().(*heldTail)
	done := make(chan error, 1)
	tail.ops.Add(1)
	go func() {
		defer tail.ops.Done()
		done <- fn()
	}()
	return done
}

func serveAsync(ctx context.Context, m *Manager, slug string) <-chan error {
	return async(m, func() error {
		_, err := m.ServeExposure(ctx, tailnetRequest(slug))
		return err
	})
}

func stopAsync(ctx context.Context, m *Manager, slug string) <-chan error {
	return async(m, func() error { return m.StopSlug(ctx, slug) })
}

// within fails the test unless done delivers within a generous bound; what
// is wrong names the call that was held up.
func within(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return: it waited on another operation", what)
		return nil
	}
}

// entered waits until an operation reached the backend call h holds.
func entered(t *testing.T, h *hold, what string) {
	t.Helper()
	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never reached its backend call", what)
	}
}

// pending fails the test if done delivers within a short wait.
func pending(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s returned (%v) while it should have waited", what, err)
	case <-time.After(100 * time.Millisecond):
	}
}

func gateCount(m *Manager) int {
	m.gateMu.Lock()
	defer m.gateMu.Unlock()
	return len(m.gates)
}

// waitGate polls until slug's gate satisfies ok.
func waitGate(t *testing.T, m *Manager, slug string, ok func(*slugGate) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.gateMu.Lock()
		g := m.gates[slug]
		done := g != nil && ok(g)
		m.gateMu.Unlock()
		if done {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("gate for %s never reached the expected state", slug)
}

// TestSlowOpenDoesNotHoldUpOtherFlats: an open of one flat that is stuck in
// its backend must not delay the retirement or the opens of other flats.
func TestSlowOpenDoesNotHoldUpOtherFlats(t *testing.T) {
	m, tail := heldTailManager(t)
	ctx := t.Context()
	slow := tail.holdServe("slow")
	defer slow.release()

	slowDone := serveAsync(ctx, m, "slow")
	entered(t, slow, "ServeExposure(slow)")

	if err := within(t, stopAsync(ctx, m, "other"), "StopSlug(other)"); err != nil {
		t.Fatalf("StopSlug(other): %v", err)
	}
	if err := within(t, serveAsync(ctx, m, "third"), "ServeExposure(third)"); err != nil {
		t.Fatalf("ServeExposure(third): %v", err)
	}
	if tracked, err := m.HasProviderRoute(ctx, "third", Tailscale); err != nil || !tracked {
		t.Fatalf("third has no Tailscale route: tracked=%v err=%v", tracked, err)
	}

	slow.release()
	if err := within(t, slowDone, "ServeExposure(slow)"); err != nil {
		t.Fatalf("ServeExposure(slow): %v", err)
	}
	if n := gateCount(m); n != 0 {
		t.Fatalf("%d gate entries left after every call returned", n)
	}
}

// TestRetirementDoesNotHoldUpOtherFlats: a retirement stuck in its network
// teardown must not delay the opens or the retirements of other flats.
func TestRetirementDoesNotHoldUpOtherFlats(t *testing.T) {
	m, tail := heldTailManager(t)
	ctx := t.Context()
	leaving := tail.holdStop("leaving")
	defer leaving.release()

	leavingDone := stopAsync(ctx, m, "leaving")
	entered(t, leaving, "StopSlug(leaving)")

	if err := within(t, serveAsync(ctx, m, "other"), "ServeExposure(other)"); err != nil {
		t.Fatalf("ServeExposure(other): %v", err)
	}
	if err := within(t, stopAsync(ctx, m, "another"), "StopSlug(another)"); err != nil {
		t.Fatalf("StopSlug(another): %v", err)
	}

	leaving.release()
	if err := within(t, leavingDone, "StopSlug(leaving)"); err != nil {
		t.Fatalf("StopSlug(leaving): %v", err)
	}
	if n := gateCount(m); n != 0 {
		t.Fatalf("%d gate entries left after every call returned", n)
	}
}

// TestOpenWaitingForRetirementHonorsContext: an open of a flat that is being
// retired waits, and gives up with its context instead of waiting on.
func TestOpenWaitingForRetirementHonorsContext(t *testing.T) {
	m, tail := heldTailManager(t)
	ctx := t.Context()
	retiring := tail.holdStop("notes")
	defer retiring.release()

	stopDone := stopAsync(ctx, m, "notes")
	entered(t, retiring, "StopSlug(notes)")

	openCtx, cancel := context.WithCancel(ctx)
	openDone := serveAsync(openCtx, m, "notes")
	// The open is waiting behind the held retirement before it is cancelled.
	waitGate(t, m, "notes", func(g *slugGate) bool { return g.retiring && g.users == 2 })
	pending(t, openDone, "ServeExposure(notes) during its retirement")
	cancel()
	if err := within(t, openDone, "cancelled ServeExposure(notes)"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled open returned %v, want context.Canceled", err)
	}
	waitGate(t, m, "notes", func(g *slugGate) bool { return g.retiring && g.users == 1 })
	if tracked, err := m.HasProviderRoute(ctx, "notes", Tailscale); err != nil || tracked {
		t.Fatalf("cancelled open registered a route: tracked=%v err=%v", tracked, err)
	}

	retiring.release()
	if err := within(t, stopDone, "StopSlug(notes)"); err != nil {
		t.Fatalf("StopSlug(notes): %v", err)
	}
	if n := gateCount(m); n != 0 {
		t.Fatalf("%d gate entries left after every call returned", n)
	}
}

// TestRetirementWaitingForOpenHonorsContext: a retirement waits for an open
// of its own flat, holds back later opens of that flat while it waits, and
// gives up with its context, letting those opens through.
func TestRetirementWaitingForOpenHonorsContext(t *testing.T) {
	m, tail := heldTailManager(t)
	ctx := t.Context()
	first := tail.holdServe("notes")
	defer first.release()

	firstDone := serveAsync(ctx, m, "notes")
	entered(t, first, "ServeExposure(notes)")

	stopCtx, cancel := context.WithCancel(ctx)
	stopDone := stopAsync(stopCtx, m, "notes")
	waitGate(t, m, "notes", func(g *slugGate) bool { return g.waiting == 1 })
	pending(t, stopDone, "StopSlug(notes) during an open of notes")

	// The waiting retirement holds back a new open of the same flat.
	secondDone := serveAsync(ctx, m, "notes")
	waitGate(t, m, "notes", func(g *slugGate) bool { return g.users == 3 })
	pending(t, secondDone, "ServeExposure(notes) behind a waiting retirement")

	cancel()
	if err := within(t, stopDone, "cancelled StopSlug(notes)"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled retirement returned %v, want context.Canceled", err)
	}
	if err := within(t, secondDone, "ServeExposure(notes) after the retirement gave up"); err != nil {
		t.Fatalf("second open: %v", err)
	}
	tail.mu.Lock()
	stopped := len(tail.stopped)
	tail.mu.Unlock()
	if stopped != 0 {
		t.Fatalf("a cancelled retirement stopped %d nodes", stopped)
	}

	first.release()
	if err := within(t, firstDone, "first ServeExposure(notes)"); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if n := gateCount(m); n != 0 {
		t.Fatalf("%d gate entries left after every call returned", n)
	}
}

// TestRetirementDrainsInFlightOpenOfItsFlat: a retirement that arrives while
// an open of its flat is still in the backend waits for that open to register
// and then retires the route it registered, leaving nothing behind.
func TestRetirementDrainsInFlightOpenOfItsFlat(t *testing.T) {
	m, tail := heldTailManager(t)
	ctx := t.Context()
	opening := tail.holdServe("notes")
	defer opening.release()

	openDone := serveAsync(ctx, m, "notes")
	entered(t, opening, "ServeExposure(notes)")
	stopDone := stopAsync(ctx, m, "notes")
	waitGate(t, m, "notes", func(g *slugGate) bool { return g.waiting == 1 })
	pending(t, stopDone, "StopSlug(notes) during an open of notes")

	opening.release()
	if err := within(t, openDone, "ServeExposure(notes)"); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := within(t, stopDone, "StopSlug(notes)"); err != nil {
		t.Fatalf("StopSlug(notes): %v", err)
	}
	for _, id := range []ID{Local, Tailscale} {
		if tracked, err := m.HasProviderRoute(ctx, "notes", id); err != nil || tracked {
			t.Fatalf("%s route survived the retirement: tracked=%v err=%v", id, tracked, err)
		}
	}
	tail.mu.Lock()
	stopped := slices.Clone(tail.stopped)
	tail.mu.Unlock()
	if !slices.Equal(stopped, []string{"notes"}) {
		t.Fatalf("tailnet stops %v, want the retired node notes", stopped)
	}
	if st, err := m.ExposureStatus(ctx, "notes"); err != nil || len(st.Endpoints) != 0 {
		t.Fatalf("retired flat still reports endpoints %+v (err %v)", st.Endpoints, err)
	}
	if n := gateCount(m); n != 0 {
		t.Fatalf("%d gate entries left after every call returned", n)
	}
}
