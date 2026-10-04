package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Minimal RFC 6455 implementation: text/binary messages (delivered to JS as
// text), fragmentation, ping/pong and close. No extensions or subprotocols.

const (
	opCont   = 0x0
	opText   = 0x1
	opBinary = 0x2
	opClose  = 0x8
	opPing   = 0x9
	opPong   = 0xA
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

func isWebSocketUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet && headerHasToken(r.Header, "Connection", "upgrade") &&
		headerHasToken(r.Header, "Upgrade", "websocket")
}

// wsConn is one WebSocket connection (server or client side).
type wsConn struct {
	c      net.Conn
	br     *bufio.Reader
	client bool // client frames are masked

	qmu          sync.Mutex // serializes enqueue and close; never held during socket writes
	budget       wsByteBudget
	sharedBudget *wsByteBudget
	wmu          sync.Mutex
	closed       atomic.Bool
	sendQ        chan []byte
	done         chan struct{}
	gone         chan struct{} // closed once end has closed the connection
	writerDone   chan struct{} // queued data has been sent or released
	onceEnd      sync.Once
	endCode      int // close code this side sent (set once in end)
	endWhy       string
}

func newWSConn(c net.Conn, br *bufio.Reader, client bool) *wsConn {
	return &wsConn{c: c, br: br, client: client, sendQ: make(chan []byte, 256), done: make(chan struct{}), gone: make(chan struct{}), writerDone: make(chan struct{})}
}

// upgradeWS completes the server handshake.
func upgradeWS(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "bad WebSocket handshake", http.StatusBadRequest)
		return nil, errors.New("bad handshake")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket not supported here", http.StatusInternalServerError)
		return nil, errors.New("not hijackable")
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + wsAccept(key) + "\r\n\r\n"
	if _, err := c.Write([]byte(resp)); err != nil {
		c.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	return newWSConn(c, brw.Reader, false), nil
}

func (ws *wsConn) writeFrame(op byte, payload []byte) error {
	ws.wmu.Lock()
	defer ws.wmu.Unlock()
	var hdr [14]byte
	hdr[0] = 0x80 | op
	n := 2
	l := len(payload)
	var maskBit byte
	if ws.client {
		maskBit = 0x80
	}
	switch {
	case l < 126:
		hdr[1] = maskBit | byte(l)
	case l <= 0xFFFF:
		hdr[1] = maskBit | 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(l))
		n = 4
	default:
		hdr[1] = maskBit | 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(l))
		n = 10
	}
	if ws.client {
		var key [4]byte
		rand.Read(key[:])
		copy(hdr[n:], key[:])
		n += 4
		masked := make([]byte, l)
		for i := range payload {
			masked[i] = payload[i] ^ key[i&3]
		}
		payload = masked
	}
	ws.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := ws.c.Write(hdr[:n]); err != nil {
		return err
	}
	_, err := ws.c.Write(payload)
	return err
}

type wsCloseError struct {
	Code   int
	Reason string
}

func (e *wsCloseError) Error() string {
	return fmt.Sprintf("websocket closed: %d %s", e.Code, e.Reason)
}

func (ws *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(ws.br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		return false, 0, nil, &wsCloseError{1002, "reserved bits set"}
	}
	op = h[0] & 0x0F
	masked := h[1]&0x80 != 0
	if masked == ws.client {
		return false, 0, nil, &wsCloseError{1002, "bad masking"}
	}
	l := uint64(h[1] & 0x7F)
	switch l {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(ws.br, b[:]); err != nil {
			return
		}
		l = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(ws.br, b[:]); err != nil {
			return
		}
		l = binary.BigEndian.Uint64(b[:])
	}
	if op >= opClose && (l > 125 || !fin) {
		return false, 0, nil, &wsCloseError{1002, "bad control frame"}
	}
	if l > MaxWSMessage {
		return false, 0, nil, &wsCloseError{1009, "message too big"}
	}
	var key [4]byte
	if masked {
		if _, err = io.ReadFull(ws.br, key[:]); err != nil {
			return
		}
	}
	payload = make([]byte, l)
	if _, err = io.ReadFull(ws.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i&3]
		}
	}
	return
}

// readMessage returns the next data message, answering pings. A close frame
// is returned as *wsCloseError.
func (ws *wsConn) readMessage() (string, error) {
	var msg []byte
	inMsg := false
	for {
		fin, op, p, err := ws.readFrame()
		if err != nil {
			return "", err
		}
		switch op {
		case opPing:
			ws.writeFrame(opPong, p)
			continue
		case opPong:
			continue
		case opClose:
			code, reason := 1005, ""
			if len(p) >= 2 {
				code = int(binary.BigEndian.Uint16(p))
				reason = string(p[2:])
			}
			return "", &wsCloseError{code, reason}
		case opText, opBinary:
			if inMsg {
				return "", &wsCloseError{1002, "unexpected data frame"}
			}
			msg, inMsg = p, true
		case opCont:
			if !inMsg {
				return "", &wsCloseError{1002, "unexpected continuation"}
			}
			msg = append(msg, p...)
		default:
			return "", &wsCloseError{1002, "unknown opcode"}
		}
		if len(msg) > MaxWSMessage {
			return "", &wsCloseError{1009, "message too big"}
		}
		if fin {
			return string(msg), nil
		}
	}
}

// wsByteBudget accounts for queued and in-flight text payloads. A zero limit
// preserves the original API's count-only queue behavior.
type wsByteBudget struct {
	used  atomic.Int64
	limit atomic.Int64
}

func (b *wsByteBudget) reserve(n int64) bool {
	for {
		old := b.used.Load()
		limit := b.limit.Load()
		if limit > 0 && (n > limit || old > limit-n) {
			return false
		}
		if b.used.CompareAndSwap(old, old+n) {
			return true
		}
	}
}
func (b *wsByteBudget) lower(n int64) {
	for {
		old := b.limit.Load()
		if old > 0 && old <= n {
			return
		}
		if b.limit.CompareAndSwap(old, n) {
			return
		}
	}
}
func (ws *wsConn) release(n int64) {
	ws.budget.used.Add(-n)
	if ws.sharedBudget != nil {
		ws.sharedBudget.used.Add(-n)
	}
}

// writer drains the send queue; bytes remain charged until the socket write
// completes, including a stalled in-flight frame.
func (ws *wsConn) writer() {
	defer close(ws.writerDone)
	defer func() {
		ws.qmu.Lock()
		defer ws.qmu.Unlock()
		for {
			select {
			case m := <-ws.sendQ:
				ws.release(int64(len(m)))
			default:
				return
			}
		}
	}()
	for {
		select {
		case m := <-ws.sendQ:
			err := ws.writeFrame(opText, m)
			ws.release(int64(len(m)))
			if err != nil {
				ws.end(1006, "")
				return
			}
		case <-ws.done:
			// enqueue and end share qmu, so no more messages can arrive.
			// Drain accepted messages before end writes the close frame.
			for {
				select {
				case m := <-ws.sendQ:
					err := ws.writeFrame(opText, m)
					ws.release(int64(len(m)))
					if err != nil {
						return
					}
				default:
					return
				}
			}
		}
	}
}
func (ws *wsConn) enqueue(text string) error {
	ws.qmu.Lock()
	if ws.closed.Load() {
		ws.qmu.Unlock()
		return errors.New("WebSocket is closed")
	}
	n := int64(len(text))
	if !ws.budget.reserve(n) {
		ws.qmu.Unlock()
		ws.end(1008, "send byte limit")
		return errors.New("WebSocket send byte limit exceeded")
	}
	if ws.sharedBudget != nil && !ws.sharedBudget.reserve(n) {
		ws.budget.used.Add(-n)
		ws.qmu.Unlock()
		ws.end(1008, "flat send byte limit")
		return errors.New("WebSocket flat send byte limit exceeded")
	}
	select {
	case ws.sendQ <- []byte(text):
		ws.qmu.Unlock()
		return nil
	default:
		ws.release(n)
		ws.qmu.Unlock()
		ws.end(1008, "send queue full")
		return errors.New("WebSocket send queue is full; the connection was closed")
	}
}

// wsCloseGrace bounds how long a closing connection may take to send its
// close frame to a peer that stopped reading.
const wsCloseGrace = 2 * time.Second

// end sends a close frame (best effort) and closes the connection once. It
// never blocks: it is called from the websocket runtime (ws.close, a full send
// queue), which must not wait for one peer, since the writer may be stuck on
// a peer that stopped reading. gone is closed when the connection is closed.
func (ws *wsConn) end(code int, reason string) {
	ws.onceEnd.Do(func() {
		ws.qmu.Lock()
		ws.endCode, ws.endWhy = code, reason
		ws.closed.Store(true)
		close(ws.done)
		ws.qmu.Unlock()
		go func() {
			defer close(ws.gone)
			t := time.AfterFunc(wsCloseGrace, func() { ws.c.Close() }) // unblocks a stuck writer
			defer t.Stop()
			if code != 1006 {
				<-ws.writerDone
				p := make([]byte, 2, 2+len(reason))
				binary.BigEndian.PutUint16(p, uint16(code))
				p = append(p, reason...)
				ws.writeFrame(opClose, p)
			}
			ws.c.Close()
		}()
	})
}

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
	w      *worker
	mu     sync.Mutex
	conns  map[int64]*wsConn
	next   atomic.Int64
	budget wsByteBudget
	events chan wsEvent
	stop   chan struct{}
	vm     *jsVM // owned by the loop goroutine
	wg     sync.WaitGroup
}

// wsEventQueue bounds the events waiting for the websocket runtime. Readers
// block when it is full (backpressure), so at most about wsEventQueue x
// MaxWSMessage bytes of visitor data are buffered.
const wsEventQueue = 64

func newWSHub(w *worker) *wsHub {
	h := &wsHub{w: w, conns: map[int64]*wsConn{}, events: make(chan wsEvent, wsEventQueue), stop: make(chan struct{})}
	h.wg.Add(1)
	go h.loop()
	return h
}

func (h *wsHub) send(id int64, text string) error {
	h.mu.Lock()
	c := h.conns[id]
	h.mu.Unlock()
	if c == nil {
		return errors.New("WebSocket is closed")
	}
	return c.enqueue(text)
}

// setSendLimits is opt-in; subsequent calls may only lower limits.
func (h *wsHub) setSendLimits(id, connectionBytes, flatBytes int64) error {
	if connectionBytes < 1024 || connectionBytes > 32<<20 || flatBytes < connectionBytes || flatBytes > 256<<20 {
		return errors.New("invalid WebSocket send limits")
	}
	h.mu.Lock()
	c := h.conns[id]
	h.mu.Unlock()
	if c == nil {
		return errors.New("WebSocket is closed")
	}
	c.budget.lower(connectionBytes)
	h.budget.lower(flatBytes)
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
		c.end(code, reason)
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
		h.conns = map[int64]*wsConn{}
		h.mu.Unlock()
		for _, c := range conns {
			c.end(1011, "server error")
		}
	}
}

// serve handles one upgraded connection until it closes.
func (h *wsHub) serve(w http.ResponseWriter, r *http.Request, url string, headers map[string]string) {
	c, err := upgradeWS(w, r)
	if err != nil {
		return
	}
	id := h.next.Add(1)
	h.mu.Lock()
	h.conns[id] = c
	h.mu.Unlock()
	c.sharedBudget = &h.budget
	go c.writer()
	if !h.post(wsEvent{Type: "open", ID: id, URL: url, Headers: headers}) {
		c.end(1001, "going away")
		return
	}
	code, reason := 1006, ""
	for {
		msg, err := c.readMessage()
		if err != nil {
			var ce *wsCloseError
			if errors.As(err, &ce) {
				code, reason = ce.Code, ce.Reason
				if code == 1005 {
					c.end(1000, "")
				} else {
					c.end(code, "")
				}
			} else {
				c.end(1006, "")
				if c.endCode != 0 { // we closed it (ws.close or a full queue)
					code, reason = c.endCode, c.endWhy
				}
			}
			break
		}
		if !h.post(wsEvent{Type: "message", ID: id, Data: &msg}) {
			c.end(1001, "going away")
			break
		}
	}
	h.mu.Lock()
	_, known := h.conns[id]
	delete(h.conns, id)
	h.mu.Unlock()
	if known {
		h.post(wsEvent{Type: "close", ID: id, Code: code, Reason: reason})
	}
}

func (h *wsHub) post(ev wsEvent) bool {
	select {
	case h.events <- ev:
		return true
	case <-h.stop:
		return false
	}
}

func (h *wsHub) shutdown() {
	h.mu.Lock()
	conns := h.conns
	h.conns = map[int64]*wsConn{}
	h.mu.Unlock()
	for _, c := range conns {
		c.end(1001, "server restarting")
	}
	select {
	case <-h.stop:
	default:
		close(h.stop)
	}
	h.wg.Wait()
	// Let the close frames go out (each closer gives up after wsCloseGrace).
	for _, c := range conns {
		<-c.gone
	}
}

// dialWS is a minimal client used by tests (and available for probes).
func dialWS(ctx context.Context, dial func(ctx context.Context) (net.Conn, error), host, path string) (*wsConn, error) {
	c, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	var k [16]byte
	rand.Read(k[:])
	key := base64.StdEncoding.EncodeToString(k[:])
	req := "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Accept") != wsAccept(key) {
		c.Close()
		return nil, fmt.Errorf("websocket handshake failed: %s", resp.Status)
	}
	return newWSConn(c, br, true), nil
}
