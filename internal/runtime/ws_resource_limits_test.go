package runtime

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWebSocketAdmissionCeiling(t *testing.T) {
	var h wsHub
	const attempts = MaxWSConnections * 2
	results := make(chan bool, attempts)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok := h.admit()
			results <- ok
			if ok {
				<-release
				h.releaseSlot()
			}
		}()
	}
	admitted := 0
	for i := 0; i < attempts; i++ {
		if <-results {
			admitted++
		}
	}
	close(release)
	wg.Wait()
	if admitted != MaxWSConnections {
		t.Fatalf("admitted %d, want %d", admitted, MaxWSConnections)
	}
	h.mu.Lock()
	active := h.active
	h.stopping = true
	h.mu.Unlock()
	if active != 0 {
		t.Fatalf("%d slots leaked", active)
	}
	if h.admit() {
		t.Fatal("admitted during shutdown")
	}
}

func TestWebSocketRejectedUpgrade(t *testing.T) {
	h := &wsHub{active: MaxWSConnections}
	w := httptest.NewRecorder()
	h.serve(w, httptest.NewRequest(http.MethodGet, "/ws", nil), "", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if h.active != MaxWSConnections {
		t.Fatal("rejected upgrade changed the reservation count")
	}
	h.active = 0
	h.stopping = true
	if h.admit() {
		t.Fatal("admitted during shutdown")
	}
}

func TestWebSocketPostFailureReleasesSlot(t *testing.T) {
	h := &wsHub{
		conns:  make(map[int64]*wsConn),
		events: make(chan wsEvent), // no receiver: the open event must block
		stop:   make(chan struct{}),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, r.URL.String(), nil)
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := dialWS(ctx, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}, address, "/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer client.c.Close()

	close(h.stop)
	client.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.readMessage(); err == nil {
		t.Fatal("connection remained open after its open event was rejected")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		h.mu.Lock()
		active, registered := h.active, len(h.conns)
		h.mu.Unlock()
		if active == 0 && registered == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("leaked after event rejection: active=%d registered=%d", active, registered)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWebSocketIdleReadDeadline(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ws := newWSConn(a, bufio.NewReader(a), false)
	_, err := ws.readMessageWithin(20 * time.Millisecond)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("idle read error = %v, want timeout", err)
	}
}

func TestWebSocketDefaultByteLimits(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := newWSConn(a, bufio.NewReader(a), false)
	h := &wsHub{conns: map[int64]*wsConn{1: c}}
	h.budget.limit.Store(MaxWSQueuedBytesPerFlat)
	c.sharedBudget = &h.budget
	if got := c.budget.limit.Load(); got != MaxWSQueuedBytesPerConn {
		t.Fatalf("connection budget = %d", got)
	}
	if err := h.setSendLimits(1, MaxWSQueuedBytesPerConn, MaxWSQueuedBytesPerFlat); err != nil {
		t.Fatal(err)
	}
	if err := h.setSendLimits(1, MaxWSQueuedBytesPerConn+1, MaxWSQueuedBytesPerFlat); err == nil {
		t.Fatal("accepted connection budget above the default")
	}
	if err := h.setSendLimits(1, MaxWSQueuedBytesPerConn, MaxWSQueuedBytesPerFlat+1); err == nil {
		t.Fatal("accepted aggregate budget above the default")
	}
	if err := h.setSendLimits(1, 1024, 2048); err != nil {
		t.Fatal(err)
	}
	if c.budget.limit.Load() != 1024 || h.budget.limit.Load() != 2048 {
		t.Fatal("lowering budgets failed")
	}
}
