package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
)

type docsTestClock struct{ nanos atomic.Int64 }

func (c *docsTestClock) now() time.Time      { return time.Unix(0, c.nanos.Load()) }
func (c *docsTestClock) add(d time.Duration) { c.nanos.Add(int64(d)) }

func managedDocsForTest(t *testing.T, lazy bool) (*docsInstance, *atomic.Int64, *docsTestClock) {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"server.js": `export default { fetch(request) { return new Response(String(request.runtimeGeneration)); }, websocket: {open(ws) {ws.send("ready");}} };`})
	next := new(atomic.Int64)
	clock := new(docsTestClock)
	clock.nanos.Store(time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	output := &logs{}
	in, err := newManager(t).Start(t.Context(), core.RuntimeSpec{Flat: "docs-lifecycle", Version: 1, Dir: dir, Entry: "server.js", DataDir: t.TempDir(), ManagedDocs: true, Lazy: lazy, Now: clock.now, Log: output.add, NextGeneration: func(context.Context) (int64, error) { return next.Add(1), nil }})
	if err != nil {
		t.Fatal(err)
	}
	d := in.(*docsInstance)
	t.Cleanup(func() {
		d.Stop()
		if output.find("DATA RACE") || output.find("panic:") {
			t.Errorf("child error: %s", output)
		}
	})
	return d, next, clock
}

func readyDocsForTest(t *testing.T, d *docsInstance) core.RuntimeLease {
	t.Helper()
	lease, err := d.Wake(t.Context(), func(ctx context.Context, h http.Handler) error {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, "GET", "http://flat/", nil))
		if rec.Code != 200 {
			return fmt.Errorf("prepare status %d: %s", rec.Code, rec.Body.String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}
func waitDocsSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("lifecycle signal timed out")
	}
}
func docsTransition(d *docsInstance) chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.transition
}

func TestManagedDocsLazySingleFlightCancellation(t *testing.T) {
	d, next, _ := managedDocsForTest(t, true)
	if next.Load() != 0 || d.CleanDormant() {
		t.Fatal("lazy instance spawned or claimed clean")
	}
	started, release := make(chan struct{}), make(chan struct{})
	preparedContext := make(chan context.Context, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var preparations atomic.Int64
	prepare := func(ctx context.Context, h http.Handler) error {
		preparations.Add(1)
		preparedContext <- ctx
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	result := make(chan error, 9)
	initialCtx, cancelInitial := context.WithCancel(t.Context())
	defer cancelInitial()
	go func() {
		lease, err := d.Wake(initialCtx, prepare)
		if lease != nil {
			lease.Release()
		}
		result <- err
	}()
	waitDocsSignal(t, started)
	workCtx := <-preparedContext
	cancelInitial()
	if err := workCtx.Err(); err != nil {
		t.Fatalf("shared preparation inherited initial caller cancellation: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := d.Wake(ctx, prepare); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	waitersEntered := make(chan struct{}, 8)
	for range 8 {
		go func() {
			waitersEntered <- struct{}{}
			lease, err := d.Wake(t.Context(), prepare)
			if lease != nil {
				lease.Release()
			}
			result <- err
		}()
	}
	for range 8 {
		<-waitersEntered
	}
	select {
	case err := <-result:
		t.Fatalf("wake returned before preparation barrier released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := workCtx.Err(); err != nil {
		t.Fatalf("cancelled visitor ended shared work: %v", err)
	}
	unblock()
	for range 9 {
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	}
	if next.Load() != 1 || preparations.Load() != 1 {
		t.Fatalf("starts=%d preparations=%d", next.Load(), preparations.Load())
	}
}

func TestManagedDocsEagerPreparationLeasesAndIdleClock(t *testing.T) {
	d, next, clock := managedDocsForTest(t, false)
	if next.Load() != 1 {
		t.Fatal("eager instance missing child")
	}
	if _, ok := d.TryAcquire(true); ok {
		t.Fatal("unprepared eager admitted")
	}
	lease := readyDocsForTest(t, d)
	d.mu.Lock()
	uptime := d.childStarted
	d.mu.Unlock()
	clock.add(3 * time.Minute)
	if _, ok := d.TryQuiesce(clock.now(), 2*time.Minute); ok {
		t.Fatal("leased child quiesced")
	}
	lease.Release()
	lease.Release()
	d.mu.Lock()
	if d.leases != 0 || !d.childStarted.Equal(uptime) {
		t.Error("release changed uptime/count")
	}
	d.mu.Unlock()
	if _, ok := d.TryQuiesce(clock.now(), 2*time.Minute); ok {
		t.Fatal("foreground release failed to reset idle")
	}
	clock.add(2 * time.Minute)
	bg, ok := d.TryAcquire(false)
	if !ok {
		t.Fatal("background acquisition failed")
	}
	bg.Release()
	q, ok := d.TryQuiesce(clock.now(), 2*time.Minute)
	if !ok {
		t.Fatal("background reset foreground clock")
	}
	done := docsTransition(d)
	q.Suspend(true)
	waitDocsSignal(t, done)
	if !d.CleanDormant() {
		t.Fatal("confirmed clean sleep missing")
	}
	if next.Load() != 1 {
		t.Fatal("sleep spawned child")
	}
}

func TestManagedDocsAbortedDrainCooldown(t *testing.T) {
	d, _, clock := managedDocsForTest(t, true)
	readyDocsForTest(t, d).Release()
	clock.add(2 * time.Minute)
	for _, delay := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		q, ok := d.TryQuiesce(clock.now(), 2*time.Minute)
		if !ok {
			t.Fatal("eligible drain blocked")
		}
		if err := q.Resume(); err != nil {
			t.Fatal(err)
		}
		bg, ok := d.TryAcquire(false)
		if !ok {
			t.Fatal("cooldown blocks refresh")
		}
		bg.Release()
		clock.add(delay - time.Nanosecond)
		if _, ok := d.TryQuiesce(clock.now(), 2*time.Minute); ok {
			t.Fatal("cooldown ended early")
		}
		clock.add(time.Nanosecond)
	}
	lease, ok := d.TryAcquire(true)
	if !ok {
		t.Fatal("foreground acquisition")
	}
	lease.Release()
	clock.add(2 * time.Minute)
	q, ok := d.TryQuiesce(clock.now(), 2*time.Minute)
	if !ok {
		t.Fatal("foreground did not reset cooldown")
	}
	if err := q.Resume(); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	failures := d.idleFailures
	d.mu.Unlock()
	if failures != 1 {
		t.Fatalf("idle failures %d", failures)
	}
}

func TestManagedDocsStopCancelsPreparingAndRetireRejects(t *testing.T) {
	for _, retire := range []bool{false, true} {
		t.Run(fmt.Sprint(retire), func(t *testing.T) {
			d, next, _ := managedDocsForTest(t, true)
			preparing := make(chan struct{})
			result := make(chan error, 1)
			go func() {
				_, err := d.Wake(t.Context(), func(ctx context.Context, _ http.Handler) error { close(preparing); <-ctx.Done(); return ctx.Err() })
				result <- err
			}()
			waitDocsSignal(t, preparing)
			if retire {
				d.Retire()
			} else {
				d.Stop()
			}
			select {
			case err := <-result:
				if err == nil {
					t.Error("cancelled preparation admitted")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("preparation stuck")
			}
			d.Stop()
			if _, err := d.Wake(t.Context(), nil); !errors.Is(err, errStopped) {
				t.Fatalf("terminal wake %v", err)
			}
			if next.Load() != 1 {
				t.Fatal("retired/stopped instance respawned")
			}
		})
	}
}

func TestManagedDocsPreparationPanicCleanup(t *testing.T) {
	d, next, _ := managedDocsForTest(t, true)
	marker := errors.New("intentional callback panic")
	func() {
		defer func() {
			if recover() != marker {
				t.Error("panic was lost or changed")
			}
		}()
		_, _ = d.Wake(t.Context(), func(context.Context, http.Handler) error { panic(marker) })
	}()
	done := docsTransition(d)
	if done != nil {
		waitDocsSignal(t, done)
	}
	if d.CleanDormant() {
		t.Fatal("panic certified clean dormancy")
	}
	readyDocsForTest(t, d).Release()
	if next.Load() != 2 {
		t.Fatalf("panic recovery starts=%d, want2", next.Load())
	}
	stopped := make(chan struct{})
	go func() { d.Stop(); close(stopped) }()
	waitDocsSignal(t, stopped)
}

type gatedDocsClose struct {
	io.WriteCloser
	entered chan struct{}
	release chan struct{}
}

func (g *gatedDocsClose) Close() error { close(g.entered); <-g.release; return g.WriteCloser.Close() }

func TestManagedDocsSuspendWaitsForPhysicalExitAndInvalidatesCertificate(t *testing.T) {
	d, next, clock := managedDocsForTest(t, true)
	oldLease := readyDocsForTest(t, d)
	old := oldLease.(*docsLease).child
	oldLease.Release()
	old.mu.Lock()
	p := old.p
	gate := &gatedDocsClose{WriteCloser: p.stdin, entered: make(chan struct{}), release: make(chan struct{})}
	p.stdin = gate
	old.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { close(gate.release) }) }
	defer release()
	clock.add(2 * time.Minute)
	q, ok := d.TryQuiesce(clock.now(), 2*time.Minute)
	if !ok {
		t.Fatal("quiescence failed")
	}
	done := docsTransition(d)
	q.Suspend(true)
	waitDocsSignal(t, gate.entered)
	if d.CleanDormant() {
		t.Fatal("unreaped child certified")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if _, err := d.Wake(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wake during termination: %v", err)
	}
	if next.Load() != 1 {
		t.Fatal("replacement before exit")
	}
	d.InvalidateClean() // replacement build invalidates a pending certificate
	release()
	waitDocsSignal(t, done)
	if d.CleanDormant() {
		t.Fatal("late clean certificate survived invalidation")
	}
	select {
	case <-p.done:
	default:
		t.Fatal("logical dormancy preceded process exit")
	}
	lease := readyDocsForTest(t, d)
	defer lease.Release()
	if next.Load() != 2 {
		t.Fatal("wake did not advance generation")
	}
	newer := lease.(*docsLease).child
	if old.sock == newer.sock {
		t.Fatal("replacement reused exact-child socket")
	}
	rec := httptest.NewRecorder()
	oldLease.ServeHTTP(rec, httptest.NewRequest("GET", "http://flat/", nil))
	if rec.Code == 200 {
		t.Fatal("stale lease reached replacement")
	}
	if _, err := os.Stat(old.sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old socket retained: %v", err)
	}
}

func TestManagedDocsQuiescenceWaitBoundAndStopWithHeldLease(t *testing.T) {
	d, next, clock := managedDocsForTest(t, true)
	readyDocsForTest(t, d).Release()
	clock.add(2 * time.Minute)
	q, ok := d.TryQuiesce(clock.now(), 2*time.Minute)
	if !ok {
		t.Fatal("quiescence failed")
	}
	start := time.Now()
	if _, err := d.Wake(t.Context(), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unbounded transition: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 3900*time.Millisecond || elapsed > 6*time.Second {
		t.Fatalf("wait %v", elapsed)
	}
	if next.Load() != 1 {
		t.Fatal("reserved transition spawned")
	}
	if err := q.Resume(); err != nil {
		t.Fatal(err)
	}
	lease, ok := d.TryAcquire(true)
	if !ok {
		t.Fatal("ready lease failed")
	}
	stopped := make(chan struct{})
	go func() { d.Stop(); close(stopped) }()
	waitDocsSignal(t, stopped)
	lease.Release()
	lease.Release()
	if _, ok := d.TryAcquire(false); ok {
		t.Fatal("stopped child admitted")
	}
}

func TestManagedDocsStopOwnsUnconsumedQuiescence(t *testing.T) {
	d, _, clock := managedDocsForTest(t, true)
	readyDocsForTest(t, d).Release()
	clock.add(2 * time.Minute)
	q, ok := d.TryQuiesce(clock.now(), 2*time.Minute)
	if !ok {
		t.Fatal("quiescence failed")
	}
	stopped := make(chan struct{})
	go func() { d.Stop(); close(stopped) }()
	waitDocsSignal(t, stopped)
	if err := q.Resume(); err == nil {
		t.Fatal("terminal transition resumed")
	}
	q.Suspend(true)
	if d.CleanDormant() {
		t.Fatal("terminal stop certified")
	}
}

func TestManagedDocsDeadPreparationNeverPublishes(t *testing.T) {
	d, next, _ := managedDocsForTest(t, false)
	child := d.child
	child.mu.Lock()
	p := child.p
	child.mu.Unlock()
	lease, err := d.Wake(t.Context(), func(context.Context, http.Handler) error {
		if err := p.cmd.Process.Kill(); err != nil {
			return err
		}
		<-p.done
		return nil
	})
	if err == nil || lease != nil {
		t.Fatal("dead prepared child admitted")
	}
	done := docsTransition(d)
	if done != nil {
		waitDocsSignal(t, done)
	}
	if _, ok := d.TryAcquire(true); ok {
		t.Fatal("dead child ready")
	}
	readyDocsForTest(t, d).Release()
	if next.Load() != 2 {
		t.Fatal("failed preparation did not recover")
	}
}

func TestManagedDocsCrashBackoffUsesUptime(t *testing.T) {
	d, _, clock := managedDocsForTest(t, true)
	lease := readyDocsForTest(t, d)
	d.mu.Lock()
	d.failures = 9
	d.childStarted = time.Now().Add(-2 * time.Minute)
	oldStarted := d.childStarted
	child := d.child
	d.mu.Unlock()
	clock.add(time.Hour)
	lease.Release()
	d.mu.Lock()
	if !d.childStarted.Equal(oldStarted) {
		t.Error("foreground activity changed uptime")
	}
	d.mu.Unlock()
	child.mu.Lock()
	p := child.p
	child.mu.Unlock()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-p.done
	readyDocsForTest(t, d).Release()
	d.mu.Lock()
	failures := d.failures
	d.mu.Unlock()
	if failures != 1 {
		t.Fatalf("long-lived child backoff did not reset: %d", failures)
	}
}

// A real upgraded connection keeps ServeHTTP and its lease outstanding. Stop
// must close the physical worker without waiting for the client to disconnect.
func TestManagedDocsStopWithOpenWebSocket(t *testing.T) {
	d, next, clock := managedDocsForTest(t, true)
	readyDocsForTest(t, d).Release()
	srv := httptest.NewServer(d)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	ws, err := dialWS(t.Context(), func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "tcp", addr) }, addr, "/")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.c.Close()
	ws.c.SetDeadline(time.Now().Add(10 * time.Second))
	if message, err := ws.readMessage(); err != nil || message != "ready" {
		t.Fatalf("WS not established: %q %v", message, err)
	}
	d.mu.Lock()
	leases := d.leases
	d.mu.Unlock()
	if leases != 1 {
		t.Fatalf("upgraded request not pinned: leases=%d", leases)
	}
	clock.add(3 * time.Minute)
	if _, ok := d.TryQuiesce(clock.now(), 2*time.Minute); ok {
		t.Fatal("open WebSocket was eligible for sleep")
	}
	stopped := make(chan struct{})
	go func() { d.Stop(); close(stopped) }()
	waitDocsSignal(t, stopped)
	if _, err := ws.readMessage(); err == nil {
		t.Fatal("worker stopped but WebSocket remained usable")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("WebSocket timed out instead of closing after Stop: %v", err)
	}
	if next.Load() != 1 {
		t.Fatal("terminal Stop restarted WebSocket worker")
	}
}

func TestManagedDocsIdleLeavesOrdinaryServerRunning(t *testing.T) {
	d, _, clock := managedDocsForTest(t, true)
	readyDocsForTest(t, d).Release()
	ordinary := mustStart(t, d.m, "ordinary", map[string]string{"server.js": `export default {fetch() {return new Response("ordinary remains live");}};`}, "server.js", nil)
	child := ordinary.inst.(*instance)
	pid := child.pid()
	if pid <= 0 {
		t.Fatal("ordinary worker never started")
	}
	clock.add(3 * time.Minute)
	q, ok := d.TryQuiesce(clock.now(), 2*time.Minute)
	if !ok {
		t.Fatal("document did not quiesce")
	}
	done := docsTransition(d)
	q.Suspend(true)
	waitDocsSignal(t, done)
	if child.pid() != pid {
		t.Fatal("document sleep changed ordinary worker")
	}
	if got := ordinary.get(t, "/").body; got != "ordinary remains live" {
		t.Fatalf("ordinary response %q", got)
	}
	if child.pid() != pid {
		t.Fatal("ordinary request had to restart child")
	}
}

func TestManagedDocsBackoffRefusalsDoNotLog(t *testing.T) {
	d, next, _ := managedDocsForTest(t, true)
	var events atomic.Int64
	d.spec.Log = func(string, string) { events.Add(1) }
	// Establish a real failed wake, including its one lifecycle log. No worker
	// is spawned because the generation authority rejects the attempt.
	d.spec.NextGeneration = func(context.Context) (int64, error) { return 0, errors.New("injected generation failure") }
	if _, err := d.Wake(t.Context(), nil); err == nil {
		t.Fatal("generation failure accepted")
	}
	done := docsTransition(d)
	if done != nil {
		waitDocsSignal(t, done)
	}
	if events.Load() != 1 {
		t.Fatalf("initial failure logs=%d, want1", events.Load())
	}
	d.mu.Lock()
	d.retryAt = time.Now().Add(30 * time.Second)
	d.mu.Unlock()
	var preparations atomic.Int64
	for range 20 {
		lease, err := d.Wake(t.Context(), func(context.Context, http.Handler) error { preparations.Add(1); return nil })
		if lease != nil || err == nil || !strings.Contains(err.Error(), "retry in") {
			t.Fatalf("expected backoff refusal, got lease=%v err=%v", lease, err)
		}
	}
	if events.Load() != 1 || preparations.Load() != 0 || next.Load() != 0 {
		t.Fatalf("backoff caused side effects: logs=%d preparations=%d starts=%d", events.Load(), preparations.Load(), next.Load())
	}
}
