package portal

import (
	"context"
	"errors"
	"github.com/gosuda/portal-tunnel/v2/types"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
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

func TestPortalCloseObservesAlreadyRemovedStop(t *testing.T) {
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
	if n.entry(e.slug) != nil {
		t.Fatal("Stop has not removed the entry")
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
