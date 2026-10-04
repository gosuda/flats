package tsnet

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFunnelFailureAfterRestartPreservesPersistentIdentity(t *testing.T) {
	control := startControl(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dir := t.TempDir()
	getCert, _ := testCA(t)
	newNet := func() *Net {
		n, err := New(Config{Dir: dir, ControlURL: control.HTTPTestServer.URL, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		n.getCert = getCert
		return n
	}

	first := newNet()
	if _, err := first.ServeFunnel(ctx, "stable-funnel", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	waitFunnelState(t, first, "stable-funnel", FunnelReady)
	before := stableNodeID(t, first, "stable-funnel")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "stable-funnel", "tailscaled.state")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("graceful close removed Funnel identity: %v", err)
	}

	restarted := newNet()
	restarted.afterFunnelListen = func() error { return errors.New("injected listener setup failure") }
	if _, err := restarted.ServeFunnel(ctx, "stable-funnel", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	waitFunnelState(t, restarted, "stable-funnel", FunnelError)
	if after := stableNodeID(t, restarted, "stable-funnel"); after != before {
		t.Fatalf("Funnel setup failure changed persisted identity: before=%q after=%q", before, after)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("Funnel setup failure removed persisted state: %v", err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFreshFunnelFailureRetiresOnlyMintedIdentity(t *testing.T) {
	control := startControl(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dir := t.TempDir()
	getCert, _ := testCA(t)
	n, err := New(Config{Dir: dir, ControlURL: control.HTTPTestServer.URL, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	n.getCert = getCert
	n.afterFunnelListen = func() error { return errors.New("injected listener setup failure") }
	if _, err := n.ServeFunnel(ctx, "fresh-funnel", http.NotFoundHandler()); err != nil {
		t.Fatal(err)
	}
	waitFunnelState(t, n, "fresh-funnel", FunnelError)
	deadline := time.Now().Add(20 * time.Second)
	for {
		n.mu.Lock()
		_, active := n.nodes["fresh-funnel"]
		pending := n.retiring["fresh-funnel"]
		n.mu.Unlock()
		if !active && pending == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("minted Funnel identity did not retire: active=%v pending=%v", active, pending != nil)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dir, "fresh-funnel")); !os.IsNotExist(err) {
		t.Fatalf("minted Funnel state survived failed setup: %v", err)
	}
	if got := n.FunnelStatus("fresh-funnel"); got.State != FunnelError {
		t.Fatalf("terminal setup detail was lost after retirement: %+v", got)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
}
