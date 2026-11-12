package tsnet

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/ipn/store"
)

// The Tailscale backend stores a node's state with ReplaceFile and does not
// try again, and ReplaceFile fails while a reader holds the file without delete
// sharing. Neither reader of that file here may make a write fail: applyStatus
// reads it with durable.ReadFile, and snapshot through os.Root. The store
// flushes each of the 200 writes, so the test takes some seconds.
func TestStateReadersLetTheBackendWrite(t *testing.T) {
	dir := t.TempDir()
	nd := &node{host: "node", dir: filepath.Join(dir, "node")}
	if err := os.Mkdir(nd.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(nd.dir, "tailscaled.state")
	backend, err := store.New(t.Logf, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.WriteState("key", []byte("0")); err != nil {
		t.Fatal(err)
	}
	location, err := newNodeStateLocation(dir, nd.host)
	if err != nil {
		t.Fatal(err)
	}
	n := &Net{changed: make(chan struct{})}
	running := &ipnstate.Status{BackendState: ipn.Running.String()}

	var stop atomic.Bool
	var readers sync.WaitGroup
	readers.Go(func() {
		for !stop.Load() {
			n.applyStatus(nd, running)
		}
	})
	readers.Go(func() {
		for !stop.Load() {
			location.snapshot()
		}
	})
	for i := 1; i <= 200 && err == nil; i++ {
		err = backend.WriteState("key", []byte(strconv.Itoa(i)))
	}
	stop.Store(true)
	readers.Wait()
	if err != nil {
		t.Fatalf("a write of the state store failed beside the readers: %v", err)
	}

	n.applyStatus(nd, running)
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(nd.identityState, want) {
		t.Fatalf("applyStatus kept %q, the file holds %q", nd.identityState, want)
	}
}
