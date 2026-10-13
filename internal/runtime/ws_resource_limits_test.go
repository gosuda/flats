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
	ws.readIdleTimeout = 20 * time.Millisecond
	_, err := ws.readMessage()
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("idle read error = %v, want timeout", err)
	}
}

func TestWebSocketControlFramesKeepReadAlive(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	server := newWSConn(a, bufio.NewReader(a), false)
	peer := newWSConn(b, bufio.NewReader(b), true)
	server.readIdleTimeout = 500 * time.Millisecond
	result := make(chan error, 1)
	go func() {
		msg, err := server.readMessage()
		if err == nil && msg != "ok" {
			err = errors.New("unexpected message")
		}
		result <- err
	}()
	for i := 0; i < 4; i++ {
		time.Sleep(200 * time.Millisecond)
		if err := peer.writeFrame(opPong, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := peer.writeFrame(opText, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("read stayed blocked")
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

// A browser that only listens sends nothing but pongs. Server pings must keep
// such an idle, live client connected past the read deadline.
func TestWebSocketServerPingsKeepIdleClientAlive(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	server := newWSConn(a, bufio.NewReader(a), false)
	server.readIdleTimeout = 300 * time.Millisecond
	server.pingInterval = 50 * time.Millisecond
	go server.writer()
	defer server.end(1006, "")
	peer := newWSConn(b, bufio.NewReader(b), true)
	go peer.readMessage() // answers pings with pongs, like a browser

	result := make(chan error, 1)
	go func() {
		msg, err := server.readMessage()
		if err == nil && msg != "ok" {
			err = errors.New("unexpected message")
		}
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("idle client with pongs was dropped: %v", err)
	case <-time.After(time.Second):
	}
	if err := peer.writeFrame(opText, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("read stayed blocked")
	}
}
