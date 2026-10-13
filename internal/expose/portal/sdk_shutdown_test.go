package portal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/portal-tunnel/v2/types"
)

// sdkBoundExposure models SDK v2.6.0 sdk/expose.go:267,312-315,988:
// cancellation invokes Close, and only the first caller receives its error.
type sdkBoundExposure struct {
	*fakeExposure
	once       sync.Once
	err        error
	closed     chan struct{}
	autoClosed chan struct{}
}

func (e *sdkBoundExposure) Close() error {
	var err error
	e.once.Do(func() {
		e.fakeExposure.Close()
		close(e.closed)
		err = e.err
	})
	return err
}

func sdkBoundNet(t *testing.T, failure error) (*Net, *sdkBoundExposure) {
	t.Helper()
	n, ff := newTestNet(t, Config{Discovery: true, ShutdownTimeout: time.Second})
	var exp *sdkBoundExposure
	n.expose = func(ctx context.Context, id types.Identity, relays []string, max int, meta types.LeaseMetadata) (exposure, error) {
		f, err := ff.expose(ctx, id, relays, max, meta)
		if err != nil {
			return nil, err
		}
		bound := &sdkBoundExposure{fakeExposure: f.(*fakeExposure), err: failure, closed: make(chan struct{}), autoClosed: make(chan struct{})}
		go func() { <-ctx.Done(); _ = bound.Close(); close(bound.autoClosed) }()
		if exp == nil {
			exp = bound // later exposures of a re-served slug are not tracked
		}
		return bound, nil
	}
	if failure != nil {
		runHTTP := n.runHTTP
		n.runHTTP = func(ctx context.Context, ln net.Listener, h http.Handler) error {
			err := runHTTP(ctx, ln, h)
			// Give the cancellation-triggered SDK Close first ownership on the old
			// implementation, without depending on scheduler luck.
			select {
			case <-n.ctx.Done():
				<-exp.autoClosed
			case <-time.After(30 * time.Millisecond):
			}
			return err
		}
	}
	if _, err := n.Serve(t.Context(), "blog", http.NotFoundHandler(), false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close(); awaitShutdown(t, exp.autoClosed) })
	return n, exp
}

func TestPortalCloseCapturesSDKUnregisterError(t *testing.T) {
	for _, concurrentStop := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrentStop=%t", concurrentStop), func(t *testing.T) {
			failure := errors.New("relay unregister failed")
			n, _ := sdkBoundNet(t, failure)
			var stopped chan error
			if concurrentStop {
				e := n.entry("blog")
				stopped = make(chan error, 1)
				go func() { stopped <- n.Stop("blog") }()
				awaitShutdown(t, e.ln.closed)
				if n.entry("blog") != e {
					t.Fatal("Stop must retain unconfirmed teardown")
				}
			}
			err := n.Close()
			if !errors.Is(err, failure) || !strings.Contains(err.Error(), "blog") {
				t.Fatalf("Close lost SDK unregister error: %v", err)
			}
			if stopped != nil {
				if err := <-stopped; !errors.Is(err, failure) {
					t.Fatalf("Stop lost unregister error: %v", err)
				}
			}
			if again := n.Close(); again != err {
				t.Fatalf("Close result changed: %v", again)
			}
		})
	}
}

func TestPortalCloseDrainsBeforeSDKUnregister(t *testing.T) {
	n, exp := sdkBoundNet(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	n.entry("blog").handler.set(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, "finished")
	}))
	body := make(chan string, 1)
	go func() {
		c := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := c.Get("http://" + exp.Addr().String())
		if err != nil {
			body <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()
	awaitShutdown(t, entered)
	entry := n.entry("blog")
	done := make(chan error, 1)
	go func() { done <- n.Close() }()
	// Listener closure proves HTTP Shutdown started; the handler remains held.
	awaitShutdown(t, entry.ln.closed)
	select {
	case <-exp.closed:
		t.Error("SDK unregistered while request was in flight")
	case <-time.After(30 * time.Millisecond):
	}
	unblock()
	if got := <-body; got != "finished" {
		t.Fatalf("body = %q", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if exp.closeCount() != 1 {
		t.Fatalf("SDK teardown count = %d", exp.closeCount())
	}
}

func TestPortalStopDoesNotForgetSDKCloseFailure(t *testing.T) {
	failure := errors.New("relay refused unregister")
	n, exp := sdkBoundNet(t, failure)
	original := n.entry("blog")

	for attempt := 0; attempt < 2; attempt++ {
		if err := n.Stop("blog"); !errors.Is(err, failure) {
			t.Fatalf("Stop attempt %d = %v, want original error", attempt+1, err)
		}
		if n.entry("blog") != original {
			t.Fatal("failed Stop discarded the exposure")
		}
	}
	// SDK Close uses sync.Once, so calling it again would falsely return nil.
	if count := exp.closeCount(); count != 1 {
		t.Fatalf("SDK Close count = %d, want 1", count)
	}
	if _, err := n.Serve(t.Context(), "blog", http.NotFoundHandler(), false); err == nil {
		t.Fatal("Serve resurrected an exposure with unconfirmed teardown")
	}
	if err := n.SetHidden("blog", true); err == nil {
		t.Fatal("SetHidden updated an exposure with unconfirmed teardown")
	}
	if err := n.Close(); !errors.Is(err, failure) {
		t.Fatalf("Close discarded the failed unregister: %v", err)
	}
	if err := n.Stop("blog"); err == nil {
		t.Fatal("Stop after Close claimed an unconfirmed exposure was removed")
	}
}

func TestPortalCloseKeepsFailedUnpublishedServe(t *testing.T) {
	failure := errors.New("unpublished unregister failed")
	n, ff := newTestNet(t, Config{Discovery: true, ShutdownTimeout: time.Second})
	closed := make(chan error, 1)
	ff.onExpose = func() {
		go func() { closed <- n.Close() }()
		deadline := time.After(time.Second)
		for {
			n.mu.RLock()
			shuttingDown := n.closed
			n.mu.RUnlock()
			if shuttingDown {
				return
			}
			select {
			case <-deadline:
				t.Fatal("Close did not mark the network closed")
			case <-time.After(time.Millisecond):
			}
		}
	}
	n.expose = func(ctx context.Context, id types.Identity, relays []string, max int, meta types.LeaseMetadata) (exposure, error) {
		f, err := ff.expose(ctx, id, relays, max, meta)
		if err != nil {
			return nil, err
		}
		return &sdkBoundExposure{fakeExposure: f.(*fakeExposure), err: failure, closed: make(chan struct{})}, nil
	}

	if _, err := n.Serve(t.Context(), "late", http.NotFoundHandler(), false); !errors.Is(err, failure) {
		t.Fatalf("unpublished Serve cleanup = %v", err)
	}
	if err := <-closed; !errors.Is(err, failure) {
		t.Fatalf("Close lost unpublished unregister error: %v", err)
	}
	if n.entry("late") == nil {
		t.Fatal("failed unpublished Serve discarded the exposure")
	}
	if err := n.Stop("late"); err == nil {
		t.Fatal("Stop reported success for unconfirmed unpublished teardown")
	}
}

// A failed unregister cannot be retried through the SDK, but nothing renews
// the lease after Close returns. Once the lease must have expired, a retry
// confirms the teardown, and Serve can register the slug afresh.
func TestPortalFailedUnregisterSettlesAfterLeaseExpiry(t *testing.T) {
	for _, via := range []string{"Stop", "Serve"} {
		t.Run(via, func(t *testing.T) {
			failure := errors.New("relay unreachable")
			n, exp := sdkBoundNet(t, failure)
			n.leaseTTL = 200 * time.Millisecond
			original := n.entry("blog")

			if err := n.Stop("blog"); !errors.Is(err, failure) {
				t.Fatalf("first Stop = %v, want unregister error", err)
			}
			if _, err := n.Serve(t.Context(), "blog", http.NotFoundHandler(), false); !errors.Is(err, failure) {
				t.Fatalf("Serve before lease expiry = %v, want unregister error", err)
			}
			if n.entry("blog") != original {
				t.Fatal("teardown confirmed before the lease expired")
			}
			time.Sleep(n.leaseTTL)

			switch via {
			case "Stop":
				if err := n.Stop("blog"); err != nil {
					t.Fatalf("Stop after lease expiry = %v", err)
				}
				if n.entry("blog") != nil {
					t.Fatal("confirmed Stop retained the entry")
				}
			case "Serve":
				if _, err := n.Serve(t.Context(), "blog", http.NotFoundHandler(), false); err != nil {
					t.Fatalf("Serve after lease expiry = %v", err)
				}
				if e := n.entry("blog"); e == nil || e == original {
					t.Fatal("Serve did not register a fresh exposure")
				}
			}
			if count := exp.closeCount(); count != 1 {
				t.Fatalf("SDK Close count = %d, want 1", count)
			}
		})
	}
}
