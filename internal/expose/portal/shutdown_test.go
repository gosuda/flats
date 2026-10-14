package portal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/portal-tunnel/v2/types"
)

type shutdownExposure struct {
	exposure
	listener          net.Listener
	closeGate         chan struct{}
	entered           chan struct{}
	finished          chan struct{}
	err               error
	skipListenerClose bool
}

func (e *shutdownExposure) Accept() (net.Conn, error) { return e.listener.Accept() }
func (e *shutdownExposure) Close() error {
	close(e.entered)
	if e.closeGate != nil {
		<-e.closeGate
	}
	if !e.skipListenerClose {
		e.listener.Close()
	}
	close(e.finished)
	return e.err
}

func shutdownEntry(t *testing.T, slug string) (*entry, *shutdownExposure) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	exp := &shutdownExposure{listener: ln, entered: make(chan struct{}), finished: make(chan struct{})}
	e := &entry{slug: slug, exp: exp, ln: newDrainListener(exp), done: make(chan struct{}), cancel: func() {}}
	t.Cleanup(func() { ln.Close() })
	return e, exp
}

func awaitShutdown(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fake teardown did not finish")
	}
}

func TestPortalStopEntryBounded(t *testing.T) {
	for _, stage := range []string{"HTTP drain", "exposure close", "listener pump"} {
		t.Run(stage, func(t *testing.T) {
			n := &Net{cfg: Config{ShutdownTimeout: 30 * time.Millisecond}}
			e, exp := shutdownEntry(t, "blocked")
			if stage != "HTTP drain" {
				close(e.done)
			}
			if stage == "exposure close" {
				exp.closeGate = make(chan struct{})
			}
			realPump := e.ln.pumped
			if stage == "listener pump" {
				exp.skipListenerClose = true
			}
			start := time.Now()
			err := n.stopEntry(e)
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), stage) || !strings.Contains(err.Error(), "blocked") {
				t.Fatalf("shutdown = %v", err)
			}
			if time.Since(start) > 500*time.Millisecond {
				t.Fatal("unbounded shutdown")
			}
			// Drain failure still starts SDK cleanup. Release all fake stalls.
			awaitShutdown(t, exp.entered)
			if stage == "HTTP drain" {
				close(e.done)
			}
			if exp.closeGate != nil {
				close(exp.closeGate)
			}
			awaitShutdown(t, exp.finished)
			if stage == "listener pump" {
				exp.listener.Close()
			}
			awaitShutdown(t, realPump)
		})
	}
}

func TestPortalCloseBoundedWithSlugLock(t *testing.T) {
	n, err := New(Config{Dir: t.TempDir(), Discovery: true, ShutdownTimeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	e, exp := shutdownEntry(t, "busy")
	close(e.done)
	n.entries[e.slug] = e
	lock := n.slugLock(e.slug)
	lock.Lock()
	start := time.Now()
	err = n.Close()
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("Close = %v after %s", err, time.Since(start))
	}
	if err2 := n.Close(); err2 != err {
		t.Fatalf("idempotent Close = %v, want %v", err2, err)
	}
	lock.Unlock()
	awaitShutdown(t, exp.finished)
	awaitShutdown(t, e.ln.pumped)
	lock.Lock()
	lock.Unlock()
}

type blockedMetadataExposure struct {
	exposure
	entered, release chan struct{}
}

func (e *blockedMetadataExposure) UpdateMetadata(meta types.LeaseMetadata) error {
	close(e.entered)
	<-e.release
	return e.exposure.UpdateMetadata(meta)
}

func TestPortalCloseBoundedWithAdmittedMetadata(t *testing.T) {
	for _, operation := range []string{"Serve", "SetHidden"} {
		t.Run(operation, func(t *testing.T) {
			n, ff := newTestNet(t, Config{Discovery: true, ShutdownTimeout: 40 * time.Millisecond})
			entered, release := make(chan struct{}), make(chan struct{})
			n.expose = func(ctx context.Context, id types.Identity, relays []string, max int, meta types.LeaseMetadata) (exposure, error) {
				exp, err := ff.expose(ctx, id, relays, max, meta)
				return &blockedMetadataExposure{exposure: exp, entered: entered, release: release}, err
			}
			if _, err := n.Serve(t.Context(), "busy", http.NotFoundHandler(), false); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if operation == "Serve" {
					n.Serve(context.Background(), "busy", http.NotFoundHandler(), true)
				} else {
					n.SetHidden("busy", true)
				}
			}()
			awaitShutdown(t, entered)
			start := time.Now()
			err := n.Close()
			close(release)
			awaitShutdown(t, done)
			n.ops.Wait()
			lock := n.slugLock("busy")
			lock.Lock()
			lock.Unlock()
			// Close's background stop can still be queued for the slug lock.
			deadline := time.After(time.Second)
			for ff.last(t).closeCount() == 0 {
				select {
				case <-deadline:
					t.Fatal("background stop did not finish")
				case <-time.After(time.Millisecond):
				}
			}
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 500*time.Millisecond {
				t.Fatalf("Close = %v after %s", err, time.Since(start))
			}
			if n.ctx.Err() == nil {
				t.Fatal("deadline did not cancel SDK context")
			}
		})
	}
}

func TestPortalShutdownReportsErrors(t *testing.T) {
	for _, failure := range []error{errors.New("relay refused unregister"), net.ErrClosed, context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			n := &Net{cfg: Config{ShutdownTimeout: time.Second}}
			e, exp := shutdownEntry(t, "example")
			exp.err = failure
			close(e.done)
			err := n.stopEntry(e)
			if errors.Is(failure, net.ErrClosed) || errors.Is(failure, context.Canceled) {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, failure) || !strings.Contains(err.Error(), "example") {
				t.Fatalf("shutdown = %v", err)
			}
		})
	}
}

func TestPortalCloseWaitsForUnpublishedServe(t *testing.T) {
	n, err := New(Config{Dir: t.TempDir(), Discovery: true, ShutdownTimeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	n.expose = func(ctx context.Context, id types.Identity, relays []string, max int, meta types.LeaseMetadata) (exposure, error) {
		close(entered)
		<-release
		return nil, errors.New("fake SDK stopped")
	}
	go func() {
		defer close(finished)
		n.Serve(context.Background(), "unpublished", http.NotFoundHandler(), false)
	}()
	awaitShutdown(t, entered)
	err = n.Close()
	close(release)
	awaitShutdown(t, finished)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close missed unpublished Serve: %v", err)
	}
}

func TestPortalCloseObservesConcurrentStop(t *testing.T) {
	n, err := New(Config{Dir: t.TempDir(), Discovery: true, ShutdownTimeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	e, exp := shutdownEntry(t, "stopping")
	close(e.done)
	exp.closeGate = make(chan struct{})
	n.entries[e.slug] = e
	finished := make(chan error, 1)
	go func() { finished <- n.Stop(e.slug) }()
	awaitShutdown(t, exp.entered)
	if n.entry(e.slug) != e {
		t.Fatal("Stop must retain the entry until teardown is confirmed")
	}
	err = n.Close()
	stopErr := <-finished
	close(exp.closeGate)
	awaitShutdown(t, exp.finished)
	awaitShutdown(t, e.ln.pumped)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("lost concurrent Stop failure: Close=%v Stop=%v", err, stopErr)
	}
}

// A Stop admitted before Close can time out after Close has started. Close
// then resumes the same teardown; once that confirms it, the earlier
// failure must not make Close report an unconfirmed shutdown.
func TestPortalCloseClearsConcurrentStopFailureAfterConfirmedRetry(t *testing.T) {
	const timeout = time.Second
	n, err := New(Config{Dir: t.TempDir(), Discovery: true, ShutdownTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	e, exp := shutdownEntry(t, "stopping")
	close(e.done)
	exp.closeGate = make(chan struct{})
	n.entries[e.slug] = e
	stopped := make(chan error, 1)
	go func() { stopped <- n.Stop(e.slug) }()
	awaitShutdown(t, exp.entered)

	// Start Close well inside the Stop deadline, so the Stop fails while the
	// network is closed and Close still has time left for its own retry.
	time.Sleep(timeout / 2)
	closed := make(chan error, 1)
	go func() { closed <- n.Close() }()
	if stopErr := <-stopped; !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("admitted Stop = %v, want deadline", stopErr)
	}
	n.mu.RLock()
	closing := n.closed
	n.mu.RUnlock()
	if !closing {
		t.Fatal("Stop failed before Close started; the race was not exercised")
	}
	close(exp.closeGate)
	awaitShutdown(t, exp.finished)
	awaitShutdown(t, e.ln.pumped)
	if err := <-closed; err != nil {
		t.Fatalf("Close after a confirmed retry = %v", err)
	}
	if n.entry(e.slug) != nil {
		t.Fatal("confirmed teardown retained entry")
	}
	if err := n.Stop(e.slug); err != nil {
		t.Fatalf("Stop after confirmed Close = %v", err)
	}
}

func TestPortalCloseDeadlineCancelsUnpublishedServe(t *testing.T) {
	n, err := New(Config{Dir: t.TempDir(), Discovery: true, ShutdownTimeout: 40 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	n.expose = func(ctx context.Context, id types.Identity, relays []string, max int, meta types.LeaseMetadata) (exposure, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	served := make(chan error, 1)
	go func() {
		_, err := n.Serve(context.Background(), "creating", http.NotFoundHandler(), false)
		served <- err
	}()
	awaitShutdown(t, entered)
	start := time.Now()
	err = n.Close()
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("Close = %v after %s", err, time.Since(start))
	}
	select {
	case err := <-served:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("creation context was not cancelled at deadline")
	}
	n.ops.Wait()
}

func TestPortalStopResumesIncompleteTeardown(t *testing.T) {
	for _, stage := range []string{"HTTP drain", "exposure close", "listener pump"} {
		t.Run(stage, func(t *testing.T) {
			n, err := New(Config{Dir: t.TempDir(), Discovery: true, ShutdownTimeout: 30 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = n.Close() })
			e, exp := shutdownEntry(t, "retry")
			if stage != "HTTP drain" {
				close(e.done)
			}
			if stage == "exposure close" {
				exp.closeGate = make(chan struct{})
			}
			if stage == "listener pump" {
				exp.skipListenerClose = true
			}
			n.entries[e.slug] = e

			if err := n.Stop(e.slug); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("first Stop = %v, want deadline", err)
			}
			if n.entry(e.slug) != e {
				t.Fatal("failed Stop lost the original exposure")
			}
			if _, err := n.Serve(t.Context(), e.slug, http.NotFoundHandler(), false); err == nil || !strings.Contains(err.Error(), "teardown") {
				t.Fatalf("Serve during failed teardown = %v", err)
			}
			if err := n.SetHidden(e.slug, true); err == nil || !strings.Contains(err.Error(), "teardown") {
				t.Fatalf("SetHidden during failed teardown = %v", err)
			}
			awaitShutdown(t, exp.entered)
			switch stage {
			case "HTTP drain":
				close(e.done)
			case "exposure close":
				// A retry must wait for this first SDK Close, not call it again.
				if err := n.Stop(e.slug); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("retry while Close is pending = %v", err)
				}
				close(exp.closeGate)
			case "listener pump":
				exp.listener.Close()
			}
			awaitShutdown(t, exp.finished)
			awaitShutdown(t, e.ln.pumped)
			if err := n.Stop(e.slug); err != nil {
				t.Fatalf("retry after %s recovered: %v", stage, err)
			}
			if n.entry(e.slug) != nil {
				t.Fatal("confirmed Stop retained entry")
			}
			if err := n.Stop(e.slug); err != nil {
				t.Fatalf("idempotent Stop: %v", err)
			}
		})
	}
}

// sdk.RunHTTP returns DeadlineExceeded after its own shutdown timeout while
// requests may still run. Stop must wait for those requests on retry
// instead of either forgetting them or failing forever.
func TestPortalStopWaitsForRequestsOutlivingHTTPShutdown(t *testing.T) {
	n, err := New(Config{Dir: t.TempDir(), Discovery: true, ShutdownTimeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	e, exp := shutdownEntry(t, "slow")
	release, entered := make(chan struct{}), make(chan struct{})
	e.handler = &handlerBox{h: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})}
	served := make(chan struct{})
	go func() {
		defer close(served)
		publicHandler(e.handler).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	awaitShutdown(t, entered)
	e.httpErr = fmt.Errorf("serve relay http: %w", context.DeadlineExceeded)
	close(e.done)
	n.entries[e.slug] = e

	if err := n.Stop(e.slug); err == nil || !strings.Contains(err.Error(), "requests still running") {
		t.Fatalf("Stop with a running request = %v", err)
	}
	if n.entry(e.slug) != e {
		t.Fatal("Stop forgot a running request")
	}
	close(release)
	awaitShutdown(t, served)
	awaitShutdown(t, exp.finished)
	if err := n.Stop(e.slug); err != nil {
		t.Fatalf("Stop after the request finished = %v", err)
	}
	if n.entry(e.slug) != nil {
		t.Fatal("confirmed Stop retained the entry")
	}
}

// An error from a stage that has finished leaves nothing to retry: the
// first Stop reports it and a retry confirms the teardown.
func TestPortalStopReportsFinishedStageErrorOnce(t *testing.T) {
	for _, stage := range []string{"HTTP shutdown", "listener pump"} {
		t.Run(stage, func(t *testing.T) {
			n, err := New(Config{Dir: t.TempDir(), Discovery: true, ShutdownTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = n.Close() })
			e, exp := shutdownEntry(t, "failed")
			failure := errors.New("boom")
			if stage == "HTTP shutdown" {
				e.httpErr = failure
			} else {
				failing := &shutdownExposure{listener: failingListener{exp.listener, failure}, entered: make(chan struct{}), finished: make(chan struct{})}
				e.exp, e.ln = failing, newDrainListener(failing)
			}
			close(e.done)
			n.entries[e.slug] = e

			if err := n.Stop(e.slug); !errors.Is(err, failure) || !strings.Contains(err.Error(), stage) {
				t.Fatalf("first Stop = %v", err)
			}
			if n.entry(e.slug) != e {
				t.Fatal("first Stop forgot the entry")
			}
			if err := n.Stop(e.slug); err != nil {
				t.Fatalf("retry = %v", err)
			}
			if n.entry(e.slug) != nil {
				t.Fatal("retry retained the entry")
			}
		})
	}
}

type failingListener struct {
	net.Listener
	err error
}

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }

// The lease-expiry wait after a failed unregister starts when SDK Close
// returned, not when a later retry happens to read its result.
func TestPortalLeaseExpiryCountsFromCloseReturn(t *testing.T) {
	n, err := New(Config{Dir: t.TempDir(), Discovery: true, ShutdownTimeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	n.leaseTTL = 150 * time.Millisecond
	e, exp := shutdownEntry(t, "late")
	failure := errors.New("unregister failed")
	exp.err, exp.closeGate = failure, make(chan struct{})
	close(e.done)
	n.entries[e.slug] = e

	if err := n.Stop(e.slug); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop with a blocked Close = %v", err)
	}
	close(exp.closeGate)
	awaitShutdown(t, exp.finished)
	time.Sleep(n.leaseTTL)
	if err := n.Stop(e.slug); err != nil {
		t.Fatalf("retry after the lease expired = %v", err)
	}
	if n.entry(e.slug) != nil {
		t.Fatal("retry retained the entry")
	}
}
