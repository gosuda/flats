package runtime

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
)

// Stop must also wait for a restart already in progress. Otherwise a
// replacement process can start after Stop returns and recreate the old path.
func TestStopWaitsForConcurrentRestart(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	in := &instance{
		tr:   &http.Transport{},
		sock: filepath.Join(t.TempDir(), "worker.sock"),
		spec: core.RuntimeSpec{NextGeneration: func(context.Context) (int64, error) {
			close(started)
			<-release
			return 0, errors.New("injected restart failure")
		}},
	}
	requestDone := make(chan error, 1)
	go func() { requestDone <- in.ensure(context.Background()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("restart never began")
	}

	stopped := make(chan struct{})
	go func() {
		in.Stop()
		close(stopped)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		in.mu.Lock()
		stopRequested := in.stopped
		in.mu.Unlock()
		if stopRequested {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Stop never began")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned while the restart was still running")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not wait for restart cleanup")
	}
	if err := <-requestDone; !errors.Is(err, errStopped) {
		t.Fatalf("restart after Stop returned %v, want errStopped", err)
	}
}
