package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/gosuda/flats/internal/wsconn"
)

// WebSocket resource limits, per worker (one per running flat version).
const (
	MaxWSConnections        = 256      // includes handshakes and sockets still closing
	MaxWSQueuedBytesPerConn = 4 << 20  // queued and in-flight outgoing bytes
	MaxWSQueuedBytesPerFlat = 16 << 20 // the same, across all connections
)

// --- hub: connections and the dedicated websocket VM ---

type wsEvent struct {
	Type    string            `json:"type"`
	ID      int64             `json:"id"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Data    *string           `json:"data,omitempty"`
	Code    int               `json:"code,omitempty"`
	Reason  string            `json:"reason,omitempty"`
}

// wsHub runs every websocket event of the worker on one JS runtime, in
// order, so module state (e.g. a set of connected sockets) is shared.
type wsHub struct {
	w        *worker
	mu       sync.Mutex
	conns    map[int64]*wsconn.Conn
	active   int // includes pending upgrades and sockets closing after removal from conns
	stopping bool
	next     atomic.Int64
	budget   wsconn.Budget
	events   chan wsEvent
	stop     chan struct{}
	vm       *jsVM // owned by the loop goroutine
	wg       sync.WaitGroup
}

// wsEventQueue bounds the events waiting for the websocket runtime. Readers
// block when it is full (backpressure), so at most about wsEventQueue x
// MaxWSMessage bytes of visitor data are buffered.
const wsEventQueue = 64

func newWSHub(w *worker) *wsHub {
	h := &wsHub{w: w, conns: map[int64]*wsconn.Conn{}, events: make(chan wsEvent, wsEventQueue), stop: make(chan struct{})}
	h.budget.Lower(MaxWSQueuedBytesPerFlat)
	h.wg.Add(1)
	go h.loop()
	return h
}

// admit reserves capacity before hijacking the HTTP connection. The slot is
// held until the socket's writer and closer have both exited.
func (h *wsHub) admit() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopping || h.active >= MaxWSConnections {
		return false
	}
	h.active++
	return true
}

func (h *wsHub) releaseSlot() {
	h.mu.Lock()
	h.active--
	h.mu.Unlock()
}

func (h *wsHub) send(id int64, text string) error {
	h.mu.Lock()
	c := h.conns[id]
	h.mu.Unlock()
	if c == nil {
		return errors.New("WebSocket is closed")
	}
	return c.Send(text)
}

// setSendLimits allows built-in apps to lower the enforced default budgets.
func (h *wsHub) setSendLimits(id, connectionBytes, flatBytes int64) error {
	if connectionBytes < 1024 || connectionBytes > MaxWSQueuedBytesPerConn || flatBytes < connectionBytes || flatBytes > MaxWSQueuedBytesPerFlat {
		return errors.New("invalid WebSocket send limits")
	}
	h.mu.Lock()
	c := h.conns[id]
	h.mu.Unlock()
	if c == nil {
		return errors.New("WebSocket is closed")
	}
	c.Budget().Lower(connectionBytes)
	h.budget.Lower(flatBytes)
	return nil
}

func (h *wsHub) close(id int64, code int, reason string) error {
	h.mu.Lock()
	c := h.conns[id]
	h.mu.Unlock()
	if c != nil {
		if code < 1000 || code > 4999 {
			code = 1000
		}
		if len(reason) > 123 {
			reason = reason[:123]
		}
		c.Close(code, reason)
	}
	return nil
}

func (h *wsHub) loop() {
	defer h.wg.Done()
	for {
		select {
		case ev := <-h.events:
			h.dispatch(ev)
		case <-h.stop:
			if h.vm != nil {
				h.vm.close()
			}
			return
		}
	}
}

func (h *wsHub) dispatch(ev wsEvent) {
	if h.vm == nil {
		if ev.Type != "open" {
			return // the VM was replaced; its connections were closed
		}
		vm, err := newJSVM(h.w, h)
		if err != nil {
			h.w.log.Log("error", "websocket runtime: "+err.Error())
			h.close(ev.ID, 1011, "server error")
			return
		}
		h.vm = vm
	}
	b, _ := json.Marshal(ev)
	ctx, cancel := context.WithTimeout(context.Background(), h.w.spec.timeout())
	_, err := h.vm.invoke(ctx, "__flats_ws", string(b))
	cancel()
	if err != nil {
		h.w.log.Log("error", fmt.Sprintf("websocket %s handler: %v", ev.Type, err))
		if ev.Type == "open" {
			h.close(ev.ID, 1011, "server error")
		}
	}
	if h.vm.broken {
		h.vm.close()
		h.vm = nil
		h.mu.Lock()
		conns := h.conns
		h.conns = map[int64]*wsconn.Conn{}
		h.mu.Unlock()
		for _, c := range conns {
			c.Close(1011, "server error")
		}
	}
}

// serve handles one upgraded connection until it closes.
func (h *wsHub) serve(w http.ResponseWriter, r *http.Request, url string, headers map[string]string) {
	if !h.admit() {
		http.Error(w, "WebSocket connection limit reached", http.StatusServiceUnavailable)
		return
	}
	defer h.releaseSlot()

	c, err := h.upgrade(w, r)
	if err != nil {
		return
	}

	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		c.Close(1006, "")
		return
	}
	id := h.next.Add(1)
	h.conns[id] = c
	h.mu.Unlock()

	code, reason := 1006, ""
	defer func() {
		c.Close(1006, "")
		h.mu.Lock()
		_, known := h.conns[id]
		delete(h.conns, id)
		h.mu.Unlock()
		if known {
			h.post(wsEvent{Type: "close", ID: id, Code: code, Reason: reason})
		}
		<-c.Done() // keep the slot charged while the closer and writer are alive
	}()

	if !h.post(wsEvent{Type: "open", ID: id, URL: url, Headers: headers}) {
		c.Close(1001, "going away")
		return
	}
	for {
		msg, err := c.ReadMessage(MaxWSMessage)
		if err != nil {
			var ce *wsconn.CloseError
			var ne net.Error
			switch {
			case errors.As(err, &ce):
				code, reason = ce.Code, ce.Reason
				if code == 1005 {
					c.Close(1000, "")
				} else {
					c.Close(code, "")
				}
			case errors.As(err, &ne) && ne.Timeout():
				code, reason = 1001, "idle timeout"
				c.Close(code, reason)
			default:
				c.Close(1006, "")
				if endCode, endWhy := c.CloseStatus(); endCode != 0 { // a concurrent close or full send queue won
					code, reason = endCode, endWhy
				}
			}
			break
		}
		if !h.post(wsEvent{Type: "message", ID: id, Data: &msg}) {
			c.Close(1001, "going away")
			break
		}
	}
}

// upgrade completes the handshake with the worker's per-connection and
// per-flat send budgets.
func (h *wsHub) upgrade(w http.ResponseWriter, r *http.Request) (*wsconn.Conn, error) {
	return wsconn.Upgrade(w, r, wsconn.Options{QueuedBytes: MaxWSQueuedBytesPerConn, Shared: &h.budget})
}

func (h *wsHub) post(ev wsEvent) bool {
	select {
	case h.events <- ev:
		return true
	case <-h.stop:
		return false
	}
}

// wake asks the WebSocket VM to catch up a room, without blocking the
// caller. It is dropped when the event queue is full or no VM runs.
func (h *wsHub) wake(doc string) {
	select {
	case h.events <- wsEvent{Type: "wake", Data: &doc}:
	default:
	}
}

func (h *wsHub) shutdown() {
	h.mu.Lock()
	h.stopping = true
	conns := h.conns
	h.conns = map[int64]*wsconn.Conn{}
	select {
	case <-h.stop:
	default:
		close(h.stop)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.Close(1001, "server restarting")
	}
	h.wg.Wait()
	// Let the close frames go out (each closer gives up after a short grace).
	for _, c := range conns {
		<-c.Done()
	}
}
