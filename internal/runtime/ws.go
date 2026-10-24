package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

// WebSocket resource limits, per worker (one per running flat version).
const (
	MaxWSConnections        = 256      // includes handshakes and sockets still closing
	MaxWSQueuedBytesPerConn = 4 << 20  // queued and in-flight outgoing bytes
	MaxWSQueuedBytesPerFlat = 16 << 20 // the same, across all connections
)

// Public connections of a worker whose module asks for separate Public
// pools (the built-in docs app, docs/spec/docs-public-edit-TECH.md 3.3):
// their own slots, send budget and event queue, so Public visitors never
// take what Private connections use.
const (
	MaxWSPublicConnections = 80
	MaxWSPublicQueuedBytes = 4 << 20
	wsPublicEventQueue     = 16
)

// Public ingress limits: new Public connections per worker, and frames and
// bytes per Public connection and across all of them. A reader over a
// bucket waits before it posts (TCP backpressure); nothing is dropped.
var (
	wsPublicStartRate, wsPublicStartBurst = 20.0, 40.0
	wsPublicConnFrames, wsPublicConnBurst = 20.0, 40.0
	wsPublicConnBytes, wsPublicConnBytesB = 256.0 * 1024, 512.0 * 1024
	wsPublicFrames, wsPublicBytes         = 400.0, 4.0 * 1024 * 1024
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
	public  bool
}

// wsHub runs every websocket event of the worker on one JS runtime, in
// order, so module state (e.g. a set of connected sockets) is shared.
type wsHub struct {
	w            *worker
	mu           sync.Mutex
	conns        map[int64]*wsconn.Conn
	publicIDs    map[int64]bool
	active       int // includes pending upgrades and sockets closing after removal from conns
	publicActive int // the same, for Public connections when publicPools is set
	stopping     bool
	next         atomic.Int64
	budget       wsconn.Budget
	publicBudget wsconn.Budget
	events       chan wsEvent
	publicEvents chan wsEvent
	signals      map[string]bool // raised signals not yet handled
	stop         chan struct{}
	vm           *jsVM // owned by the loop goroutine
	wg           sync.WaitGroup

	// publicPools separates Public connections (the trusted X-Flats-Access
	// header is not "private") from Private ones.
	publicPools  bool
	publicStarts *tokenBucket
	publicFrames *tokenBucket
	publicBytes  *tokenBucket

	hooks *wsHooks // tests only
}

// wsHooks observe the hub in tests.
type wsHooks struct {
	posted     func(*wsEvent)
	dispatched func(wsEvent)
}

// wsEventQueue bounds the events waiting for the websocket runtime. Readers
// block when it is full (backpressure), so at most about wsEventQueue x
// MaxWSMessage bytes of visitor data are buffered.
const wsEventQueue = 64

func newWSHub(w *worker, publicPools bool) *wsHub {
	h := &wsHub{w: w, conns: map[int64]*wsconn.Conn{}, events: make(chan wsEvent, wsEventQueue), stop: make(chan struct{})}
	h.budget.Lower(MaxWSQueuedBytesPerFlat)
	if publicPools {
		h.enablePublicPools()
	}
	h.wg.Add(1)
	go h.loop()
	return h
}

func (h *wsHub) enablePublicPools() {
	h.publicPools = true
	h.publicIDs = map[int64]bool{}
	h.publicEvents = make(chan wsEvent, wsPublicEventQueue)
	h.publicBudget.Lower(MaxWSPublicQueuedBytes)
	h.publicStarts = newTokenBucket(wsPublicStartRate, wsPublicStartBurst)
	h.publicFrames = newTokenBucket(wsPublicFrames, wsPublicFrames)
	h.publicBytes = newTokenBucket(wsPublicBytes, wsPublicBytes)
}

// admit reserves capacity before hijacking the HTTP connection. The slot is
// held until the socket's writer and closer have both exited.
func (h *wsHub) admit(public bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopping {
		return false
	}
	if public {
		if h.publicActive >= MaxWSPublicConnections || !h.publicStarts.take(1) {
			return false
		}
		h.publicActive++
		return true
	}
	if h.active >= MaxWSConnections {
		return false
	}
	h.active++
	return true
}

func (h *wsHub) releaseSlot(public bool) {
	h.mu.Lock()
	if public {
		h.publicActive--
	} else {
		h.active--
	}
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
// flatBytes lowers the budget of the connection's class.
func (h *wsHub) setSendLimits(id, connectionBytes, flatBytes int64) error {
	h.mu.Lock()
	c, public := h.conns[id], h.publicIDs[id]
	h.mu.Unlock()
	shared, flatMax := &h.budget, int64(MaxWSQueuedBytesPerFlat)
	if public {
		shared, flatMax = &h.publicBudget, MaxWSPublicQueuedBytes
	}
	if connectionBytes < 1024 || connectionBytes > MaxWSQueuedBytesPerConn || flatBytes < connectionBytes || flatBytes > flatMax {
		return errors.New("invalid WebSocket send limits")
	}
	if c == nil {
		return errors.New("WebSocket is closed")
	}
	c.Budget().Lower(connectionBytes)
	shared.Lower(flatBytes)
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

// loop takes Private events first: a Public event is taken only while the
// Private queue is empty, so a Private event waits for at most the one
// Public event being handled.
func (h *wsHub) loop() {
	defer h.wg.Done()
	for {
		select {
		case ev := <-h.events:
			h.handle(ev)
			continue
		case <-h.stop:
			h.closeVM()
			return
		default:
		}
		select {
		case ev := <-h.events:
			h.handle(ev)
		case ev := <-h.publicEvents:
			h.handle(ev)
		case <-h.stop:
			h.closeVM()
			return
		}
	}
}

func (h *wsHub) closeVM() {
	if h.vm != nil {
		h.vm.close()
	}
}

// handle dispatches one event, then every signal raised meanwhile.
func (h *wsHub) handle(ev wsEvent) {
	if ev.Type != "signal" {
		h.dispatch(ev)
	}
	for _, name := range h.takeSignals() {
		h.dispatch(wsEvent{Type: "signal", Data: &name})
	}
}

func (h *wsHub) dispatch(ev wsEvent) {
	if h.hooks != nil && h.hooks.dispatched != nil {
		h.hooks.dispatched(ev)
		return
	}
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
	public := h.publicPools && headers["x-flats-access"] != "private"
	if !h.admit(public) {
		http.Error(w, "WebSocket connection limit reached", http.StatusServiceUnavailable)
		return
	}
	defer h.releaseSlot(public)

	c, err := h.upgrade(w, r, public)
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
	if public {
		h.publicIDs[id] = true
	}
	h.mu.Unlock()

	code, reason := 1006, ""
	defer func() {
		c.Close(1006, "")
		h.mu.Lock()
		_, known := h.conns[id]
		delete(h.conns, id)
		delete(h.publicIDs, id)
		h.mu.Unlock()
		if known {
			h.post(wsEvent{Type: "close", ID: id, Code: code, Reason: reason, public: public})
		}
		<-c.Done() // keep the slot charged while the closer and writer are alive
	}()

	if !h.post(wsEvent{Type: "open", ID: id, URL: url, Headers: headers, public: public}) {
		c.Close(1001, "going away")
		return
	}
	var frames, bytes *tokenBucket
	if public {
		frames = newTokenBucket(wsPublicConnFrames, wsPublicConnBurst)
		bytes = newTokenBucket(wsPublicConnBytes, wsPublicConnBytesB)
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
		if public && !h.pace(c, frames, bytes, len(msg)) {
			c.Close(1001, "going away")
			break
		}
		if !h.post(wsEvent{Type: "message", ID: id, Data: &msg, public: public}) {
			c.Close(1001, "going away")
			break
		}
	}
}

// pace waits until a Public connection's message fits its own buckets and
// the worker's shared Public buckets. It reports false when the hub stops
// or the connection closes meanwhile.
func (h *wsHub) pace(c *wsconn.Conn, frames, bytes *tokenBucket, size int) bool {
	wait := max(frames.reserve(1), bytes.reserve(float64(size)),
		h.publicFrames.reserve(1), h.publicBytes.reserve(float64(size)))
	if wait <= 0 {
		return true
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-h.stop:
		return false
	case <-c.Done():
		return false
	}
}

// upgrade completes the handshake with the worker's per-connection and
// per-flat send budgets.
func (h *wsHub) upgrade(w http.ResponseWriter, r *http.Request, public bool) (*wsconn.Conn, error) {
	shared := &h.budget
	if public {
		shared = &h.publicBudget
	}
	return wsconn.Upgrade(w, r, wsconn.Options{QueuedBytes: MaxWSQueuedBytesPerConn, Shared: shared})
}

// post queues an event of a connection in its class's queue, waiting for
// space, so each connection's events stay in order.
func (h *wsHub) post(ev wsEvent) bool {
	if h.hooks != nil && h.hooks.posted != nil {
		h.hooks.posted(&ev)
	}
	queue := h.events
	if ev.public {
		queue = h.publicEvents
	}
	select {
	case queue <- ev:
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

// maxWSSignals bounds the distinct signal names waiting at once.
const maxWSSignals = 16

// signal raises a coalescing signal for the WebSocket VM without blocking
// the caller. Unlike wake it is never lost: the name stays pending until
// the loop handles it, which it checks after every event; the queued event
// only makes an idle loop run.
func (h *wsHub) signal(name string) {
	h.mu.Lock()
	if h.signals == nil {
		h.signals = map[string]bool{}
	}
	if !h.signals[name] && len(h.signals) >= maxWSSignals {
		h.mu.Unlock()
		return
	}
	h.signals[name] = true
	h.mu.Unlock()
	select {
	case h.events <- wsEvent{Type: "signal"}:
	default:
	}
}

// takeSignals returns the pending signal names, sorted, and clears them.
func (h *wsHub) takeSignals() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.signals) == 0 {
		return nil
	}
	names := make([]string, 0, len(h.signals))
	for name := range h.signals {
		names = append(names, name)
	}
	clear(h.signals)
	sort.Strings(names)
	return names
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

// tokenBucket is a token bucket on the wall clock, shared by goroutines.
type tokenBucket struct {
	mu          sync.Mutex
	rate, burst float64
	tokens      float64
	last        time.Time
}

func newTokenBucket(perSecond, burst float64) *tokenBucket {
	return &tokenBucket{rate: perSecond, burst: burst, tokens: burst}
}

func (b *tokenBucket) refill(now time.Time) {
	if !b.last.IsZero() {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
}

// take takes n tokens when the bucket holds them.
func (b *tokenBucket) take(n float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(time.Now())
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// reserve takes n tokens, going into debt when the bucket holds fewer, and
// returns how long the caller waits until the debt is repaid.
func (b *tokenBucket) reserve(n float64) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(time.Now())
	b.tokens -= n
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}
