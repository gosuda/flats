package app

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

// suffixNet is a tailnet that learns its MagicDNS suffix after start.
type suffixNet struct {
	mu     sync.Mutex
	suffix string
}

func (n *suffixNet) Serve(context.Context, string, http.Handler, bool) (string, error) {
	return "", nil
}
func (n *suffixNet) Stop(string) error      { return nil }
func (n *suffixNet) Status() core.NetStatus { return core.NetStatus{} }
func (n *suffixNet) Close() error           { return nil }
func (n *suffixNet) URL(host string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	suffix := n.suffix
	if suffix == "" {
		suffix = "<tailnet>.ts.net"
	}
	return "https://" + host + "." + suffix
}

// The console address, and with it every approval link and the status
// response, picks up the tailnet name once it is known instead of keeping
// the placeholder from start-up.
func TestConsoleURLLearnsTailnetName(t *testing.T) {
	h := &Host{console: "http://127.0.0.1:7878"}
	if got := h.ConsoleURL(); got != "http://127.0.0.1:7878" {
		t.Fatalf("loopback console %q", got)
	}
	tail := &suffixNet{}
	h.consoleNet, h.consoleHost = tail, "flats"
	if got := h.ConsoleURL(); got != "https://flats.<tailnet>.ts.net" {
		t.Fatalf("before the tailnet is known: %q", got)
	}
	tail.mu.Lock()
	tail.suffix = "tail1234.ts.net"
	tail.mu.Unlock()
	if got := h.ConsoleURL(); got != "https://flats.tail1234.ts.net" {
		t.Fatalf("after the tailnet is known: %q", got)
	}
}
