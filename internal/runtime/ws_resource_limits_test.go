package runtime

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
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
		conns:  make(map[int64]*wsconn.Conn),
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
	client, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}, address, "/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(1006, "")

	close(h.stop)
	client.NetConn().SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.ReadMessage(MaxWSMessage); err == nil {
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

func TestWebSocketDefaultByteLimits(t *testing.T) {
	h := &wsHub{conns: map[int64]*wsconn.Conn{}}
	h.budget.Lower(MaxWSQueuedBytesPerFlat)
	upgraded := make(chan *wsconn.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := h.upgrade(w, r)
		if err != nil {
			t.Error(err)
			close(upgraded)
			return
		}
		upgraded <- c
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	client, err := wsconn.Dial(t.Context(), func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}, address, "/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(1006, "")
	c := <-upgraded
	if c == nil {
		t.FailNow()
	}
	defer c.Close(1006, "")
	h.conns[1] = c
	if got := c.Budget().Limit(); got != MaxWSQueuedBytesPerConn {
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
	if c.Budget().Limit() != 1024 || h.budget.Limit() != 2048 {
		t.Fatal("lowering budgets failed")
	}
}

// The worker reads messages of up to MaxWSMessage bytes and closes the
// connection with 1009 on one byte more.
func TestWebSocketMessageLimit(t *testing.T) {
	h := &wsHub{
		conns:  make(map[int64]*wsconn.Conn),
		events: make(chan wsEvent, 8),
		stop:   make(chan struct{}),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, r.URL.String(), nil)
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	client, err := wsconn.Dial(t.Context(), func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}, address, "/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(1006, "")
	next := func() wsEvent {
		t.Helper()
		select {
		case ev := <-h.events:
			return ev
		case <-time.After(10 * time.Second):
			t.Fatal("no event")
			return wsEvent{}
		}
	}
	if ev := next(); ev.Type != "open" {
		t.Fatalf("event %+v", ev)
	}
	if err := client.WriteFrame(wsconn.OpText, make([]byte, MaxWSMessage)); err != nil {
		t.Fatal(err)
	}
	if ev := next(); ev.Type != "message" || ev.Data == nil || len(*ev.Data) != MaxWSMessage {
		t.Fatalf("event %s", ev.Type)
	}
	go client.WriteFrame(wsconn.OpText, make([]byte, MaxWSMessage+1)) // the server stops reading at the header
	if ev := next(); ev.Type != "close" || ev.Code != 1009 || ev.Reason != "message too big" {
		t.Fatalf("event %+v", ev)
	}
}
