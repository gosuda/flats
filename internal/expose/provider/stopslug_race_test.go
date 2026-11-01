package provider

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/expose/local"
)

// gatedTail is a fakeTail whose node Stop blocks until released, holding
// StopSlug between its route snapshot and its completion.
type gatedTail struct {
	fakeTail
	stopEntered chan struct{}
	releaseStop chan struct{}
	stopOnce    sync.Once
	nodeStopped atomic.Bool
	// servedAfterNodeStop records whether the last ServeFunnel observed the
	// node stop completed: with opens serialized against the retirement it
	// is always true for an open started while StopSlug held the retirement.
	servedAfterNodeStop atomic.Bool
}

func (f *gatedTail) ServeFunnel(ctx context.Context, host string, h http.Handler) (string, error) {
	f.funnel++
	if f.funnelErr != nil {
		return "", f.funnelErr
	}
	f.stopped = false
	f.servedAfterNodeStop.Store(f.nodeStopped.Load())
	return "https://notes.example.ts.net", nil
}

func (f *gatedTail) Stop(host string) error {
	f.stop++
	f.stopHosts = append(f.stopHosts, host)
	f.stopOnce.Do(func() {
		close(f.stopEntered)
		<-f.releaseStop
	})
	f.nodeStopped.Store(true)
	return f.tailStopErr
}

func gatedRetirementManager(t *testing.T, tail Tailnet) *Manager {
	t.Helper()
	dir := t.TempDir()
	f := File{Version: 1, Permitted: []ID{Funnel}}
	f.Migration.GrantsFromState = []ID{}
	if err := Save(dir, f); err != nil {
		t.Fatal(err)
	}
	ln, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	m, err := New(dir, Options{Local: ln, Tailscale: tail, Portal: &fakePortal{}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestStopSlugBlocksConcurrentOpen pins the open-versus-retirement contract:
// an open racing StopSlug either registers before the route snapshot (and is
// stopped by the retirement) or starts only after StopSlug returned as a
// fresh open. Registering between the two would orphan a live listener for a
// retired slug.
func TestStopSlugBlocksConcurrentOpen(t *testing.T) {
	tail := &gatedTail{stopEntered: make(chan struct{}), releaseStop: make(chan struct{})}
	m := gatedRetirementManager(t, tail)
	ctx := t.Context()
	release := sync.OnceFunc(func() { close(tail.releaseStop) })
	defer release() // every exit path unblocks the retirement goroutine

	// The open that registers before the snapshot is stopped by the retirement.
	req := ExposureRequest{
		Slug: "notes", Host: "notes", Visibility: "public", Audience: AudienceCurrent,
		Handler: text("notes"), Permitted: []ID{Funnel},
	}
	if _, err := m.ServeExposure(ctx, req); err != nil {
		t.Fatal(err)
	}

	stopErr := make(chan error, 1)
	go func() { stopErr <- m.StopSlug(ctx, "notes") }()
	<-tail.stopEntered // snapshot taken; the retirement now blocks in the node stop

	// An open arriving mid-retirement must wait, not register an orphan.
	openErr := make(chan error, 1)
	openRes := make(chan ExposureResult, 1)
	go func() {
		res, err := m.ServeExposure(ctx, req)
		openRes <- res
		openErr <- err
	}()
	select {
	case res := <-openRes:
		t.Fatalf("open registered while the slug was being retired: %+v", res)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	if err := <-stopErr; err != nil {
		t.Fatalf("StopSlug failed: %v", err)
	}
	if err := <-openErr; err != nil {
		t.Fatalf("open after the retirement failed: %v", err)
	}
	if !tail.servedAfterNodeStop.Load() {
		t.Fatal("open served before the retirement's node stop completed")
	}
	if tail.stop != 1 || tail.funnelStops != 1 {
		t.Fatalf("retirement teardown calls: Stop=%d StopFunnel=%d", tail.stop, tail.funnelStops)
	}
	if tail.funnel != 2 {
		t.Fatalf("ServeFunnel calls: %d, want the first open and the post-retirement open", tail.funnel)
	}
	if endpointURL(<-openRes, Funnel) == "" {
		t.Fatal("open after the retirement reported no Funnel endpoint")
	}
	// The fresh open is a legitimate post-retirement route.
	if tracked, err := m.HasProviderRoute(ctx, "notes", Funnel); err != nil || !tracked {
		t.Fatalf("post-retirement open left no route: tracked=%v err=%v", tracked, err)
	}
}
