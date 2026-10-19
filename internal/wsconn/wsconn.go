// Package wsconn is a minimal RFC 6455 WebSocket implementation: text and
// binary messages (read as text), fragmentation, ping/pong and close. It
// has no extensions or subprotocols.
package wsconn

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
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

// Frame opcodes.
const (
	OpCont   = 0x0
	OpText   = 0x1
	OpBinary = 0x2
	OpClose  = 0x8
	OpPing   = 0x9
	OpPong   = 0xA
)

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Defaults that Upgrade applies to zero Options fields.
const (
	DefaultQueuedBytes = 4 << 20 // queued and in-flight outgoing bytes per connection

	// A peer that sends nothing, not even a pong, for DefaultReadIdleTimeout
	// is gone. The server pings every DefaultPingInterval so that idle but
	// live clients (browsers answer pings automatically) are not closed.
	DefaultReadIdleTimeout = 2 * time.Minute
	DefaultPingInterval    = 30 * time.Second
)

const (
	handshakeTimeout = 5 * time.Second
	writeTimeout     = 10 * time.Second
	// closeGrace bounds how long a closing connection may take to send its
	// close frame to a peer that stopped reading.
	closeGrace = 2 * time.Second
)

// AcceptKey implements RFC 6455 section 4.2.2: SHA-1 of the public handshake
// nonce plus GUID, not credential hashing or authentication. Section 10.8
// explains why collision resistance is not required here. Keep SHA-1 for
// interoperability; see TestWSAcceptRFC6455 and docs/internal/websocket-handshake.md.
func AcceptKey(key string) string {
	h := sha1.Sum([]byte(key + guid))
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

// IsUpgrade reports whether r asks for a WebSocket upgrade.
func IsUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet && headerHasToken(r.Header, "Connection", "upgrade") &&
		headerHasToken(r.Header, "Upgrade", "websocket")
}

// Options configures a server connection. Zero fields take the defaults.
type Options struct {
	// QueuedBytes caps this connection's queued and in-flight outgoing bytes.
	QueuedBytes int64
	// Shared, when set, is charged for the same bytes, across every
	// connection that shares it.
	Shared          *Budget
	ReadIdleTimeout time.Duration
	PingInterval    time.Duration
}

// Conn is one WebSocket connection (server or client side).
type Conn struct {
	c               net.Conn
	br              *bufio.Reader
	client          bool          // client frames are masked
	readBroken      bool          // a read failed other than by an idle timeout; only the reader touches it
	readIdleTimeout time.Duration // 0: no read deadline
	pingInterval    time.Duration // 0: the writer sends no pings

	qmu          sync.Mutex // serializes Send and Close; never held during socket writes
	budget       Budget
	sharedBudget *Budget
	wmu          sync.Mutex
	closed       atomic.Bool
	sendQ        chan []byte
	done         chan struct{}
	gone         chan struct{} // closed once Close has closed the connection
	writerDone   chan struct{} // queued data has been sent or released
	onceEnd      sync.Once
	endCode      int // close code this side sent (set once in Close)
	endWhy       string
}

func newConn(c net.Conn, br *bufio.Reader, client bool) *Conn {
	ws := &Conn{c: c, br: br, client: client, sendQ: make(chan []byte, 256), done: make(chan struct{}), gone: make(chan struct{}), writerDone: make(chan struct{})}
	ws.budget.limit.Store(DefaultQueuedBytes)
	return ws
}

// Upgrade completes the server handshake and starts the connection's writer.
// On a bad handshake it writes the HTTP error response itself.
func Upgrade(w http.ResponseWriter, r *http.Request, opts Options) (*Conn, error) {
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
	if err := c.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		c.Close()
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + AcceptKey(key) + "\r\n\r\n"
	if _, err := c.Write([]byte(resp)); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		c.Close()
		return nil, err
	}
	ws := newConn(c, brw.Reader, false)
	ws.budget.limit.Store(cmp.Or(opts.QueuedBytes, DefaultQueuedBytes))
	ws.sharedBudget = opts.Shared
	ws.readIdleTimeout = cmp.Or(opts.ReadIdleTimeout, DefaultReadIdleTimeout)
	ws.pingInterval = cmp.Or(opts.PingInterval, DefaultPingInterval)
	go ws.writer()
	return ws, nil
}

// NetConn returns the underlying connection, for deadlines. Closing it does
// not end the Conn by itself: call Close, or for a client let ReadMessage
// observe the failure, so the writer goroutine exits.
func (ws *Conn) NetConn() net.Conn { return ws.c }

// Budget returns this connection's own byte budget, for lowering it.
func (ws *Conn) Budget() *Budget { return &ws.budget }

// WriteFrame writes one final frame directly, bypassing the send queue. It
// is for test clients and probes; servers send with Send.
func (ws *Conn) WriteFrame(op byte, payload []byte) error {
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
	ws.c.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := ws.c.Write(hdr[:n]); err != nil {
		return err
	}
	_, err := ws.c.Write(payload)
	return err
}

// CloseError is a close frame from the peer, or a protocol violation the
// reader detected (with the code to close with).
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("websocket closed: %d %s", e.Code, e.Reason)
}

// errReadBroken is returned by every read after a read error other than a
// timeout between messages: the reader's position in the stream is lost.
var errReadBroken = errors.New("websocket: an earlier read failed; the connection cannot continue")

// readFrame reads one frame. started reports whether any byte of it was
// consumed, so the caller can tell a timeout between frames (nothing lost)
// from one inside a frame.
func (ws *Conn) readFrame(max int) (fin bool, op byte, payload []byte, started bool, err error) {
	var h [2]byte
	if h[0], err = ws.br.ReadByte(); err != nil {
		return
	}
	started = true
	if h[1], err = ws.br.ReadByte(); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		return false, 0, nil, true, &CloseError{1002, "reserved bits set"}
	}
	op = h[0] & 0x0F
	masked := h[1]&0x80 != 0
	if masked == ws.client {
		return false, 0, nil, true, &CloseError{1002, "bad masking"}
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
	if op >= OpClose && (l > 125 || !fin) {
		return false, 0, nil, true, &CloseError{1002, "bad control frame"}
	}
	if l > uint64(max) {
		return false, 0, nil, true, &CloseError{1009, "message too big"}
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

// ReadMessage returns the next data message of at most max bytes (max must
// be positive), answering pings. A close frame, or a message or frame over
// max (code 1009), is returned as *CloseError. Only one goroutine may read.
//
// A read deadline that expires between messages loses nothing: no byte of a
// data frame has been read, and any control frames before it were handled
// completely. The timeout error is returned and a later ReadMessage
// continues normally. A deadline that expires inside a frame, or between the
// frames of a fragmented message, loses the part already read, so the
// message can never be completed. The error still wraps the timeout (the
// peer did go quiet), but the connection cannot continue: every later
// ReadMessage fails at once with a non-timeout error. Any other read error
// leaves the connection unusable too.
//
// A server's owner closes the connection with a code chosen from the error.
// A client (Dial) has no such owner, so it closes its connection here (Close
// with 1006) on every read error except a timeout between messages: the
// socket failed, the peer closed or sent a close frame, the frames were
// invalid, or a timeout cut a frame or message short. A timeout between
// messages leaves the connection open for the caller to retry or Close.
func (ws *Conn) ReadMessage(max int) (string, error) {
	if ws.readBroken {
		return "", errReadBroken
	}
	msg, idle, err := ws.readMessage(max)
	if err != nil && !idle {
		ws.readBroken = true
		if ws.client {
			ws.Close(1006, "")
		}
	}
	return msg, err
}

// readMessage reports idle for a timeout that arrived between messages, the
// only read error after which reading can continue.
func (ws *Conn) readMessage(max int) (string, bool, error) {
	var msg []byte
	inMsg := false
	for {
		// Re-arm after each completed frame, including ping/pong keepalives.
		// A stalled or partial frame still has one finite read deadline.
		if ws.readIdleTimeout > 0 {
			if err := ws.c.SetReadDeadline(time.Now().Add(ws.readIdleTimeout)); err != nil {
				return "", false, err
			}
		}
		fin, op, p, started, err := ws.readFrame(max)
		if err != nil {
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() {
				return "", false, err
			}
			if started || inMsg {
				return "", false, fmt.Errorf("websocket: read timed out inside a message, which is lost: %w", err)
			}
			return "", true, err
		}
		switch op {
		case OpPing:
			ws.WriteFrame(OpPong, p)
			continue
		case OpPong:
			continue
		case OpClose:
			code, reason := 1005, ""
			if len(p) >= 2 {
				code = int(binary.BigEndian.Uint16(p))
				reason = string(p[2:])
			}
			return "", false, &CloseError{code, reason}
		case OpText, OpBinary:
			if inMsg {
				return "", false, &CloseError{1002, "unexpected data frame"}
			}
			msg, inMsg = p, true
		case OpCont:
			if !inMsg {
				return "", false, &CloseError{1002, "unexpected continuation"}
			}
			msg = append(msg, p...)
		default:
			return "", false, &CloseError{1002, "unknown opcode"}
		}
		if len(msg) > max {
			return "", false, &CloseError{1009, "message too big"}
		}
		if fin {
			return string(msg), false, nil
		}
	}
}

// Budget accounts for queued and in-flight outgoing text payloads. The zero
// value has no byte ceiling until Lower sets one.
type Budget struct {
	used  atomic.Int64
	limit atomic.Int64
}

func (b *Budget) reserve(n int64) bool {
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

// Lower sets the ceiling to n (positive) unless it is already at or below n.
func (b *Budget) Lower(n int64) {
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

// Used returns the bytes currently charged.
func (b *Budget) Used() int64 { return b.used.Load() }

// Limit returns the ceiling; 0 means none.
func (b *Budget) Limit() int64 { return b.limit.Load() }

func (ws *Conn) release(n int64) {
	ws.budget.used.Add(-n)
	if ws.sharedBudget != nil {
		ws.sharedBudget.used.Add(-n)
	}
}

// writer drains the send queue; bytes remain charged until the socket write
// completes, including a stalled in-flight frame.
func (ws *Conn) writer() {
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
	var ping <-chan time.Time
	if ws.pingInterval > 0 {
		t := time.NewTicker(ws.pingInterval)
		defer t.Stop()
		ping = t.C
	}
	for {
		select {
		case m := <-ws.sendQ:
			err := ws.WriteFrame(OpText, m)
			ws.release(int64(len(m)))
			if err != nil {
				ws.Close(1006, "")
				return
			}
		case <-ping:
			if err := ws.WriteFrame(OpPing, nil); err != nil {
				ws.Close(1006, "")
				return
			}
		case <-ws.done:
			// Send and Close share qmu, so no more messages can arrive.
			// Drain accepted messages before Close writes the close frame.
			for {
				select {
				case m := <-ws.sendQ:
					err := ws.WriteFrame(OpText, m)
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

// Send queues a text message without blocking. Over a byte budget or with
// a full queue it closes the connection (code 1008) and returns an error.
func (ws *Conn) Send(text string) error {
	ws.qmu.Lock()
	if ws.closed.Load() {
		ws.qmu.Unlock()
		return errors.New("WebSocket is closed")
	}
	n := int64(len(text))
	if !ws.budget.reserve(n) {
		ws.qmu.Unlock()
		ws.Close(1008, "send byte limit")
		return errors.New("WebSocket send byte limit exceeded")
	}
	if ws.sharedBudget != nil && !ws.sharedBudget.reserve(n) {
		ws.budget.used.Add(-n)
		ws.qmu.Unlock()
		ws.Close(1008, "flat send byte limit")
		return errors.New("WebSocket flat send byte limit exceeded")
	}
	select {
	case ws.sendQ <- []byte(text):
		ws.qmu.Unlock()
		return nil
	default:
		ws.release(n)
		ws.qmu.Unlock()
		ws.Close(1008, "send queue full")
		return errors.New("WebSocket send queue is full; the connection was closed")
	}
}

// Close sends a close frame after the queued messages (best effort) and
// closes the connection, once; code 1006 closes without a close frame. It
// never blocks, since the writer may be stuck on a peer that stopped
// reading. Done is closed when the connection is closed.
func (ws *Conn) Close(code int, reason string) {
	ws.onceEnd.Do(func() {
		ws.qmu.Lock()
		ws.endCode, ws.endWhy = code, reason
		ws.closed.Store(true)
		close(ws.done)
		ws.qmu.Unlock()
		go func() {
			defer close(ws.gone)
			t := time.AfterFunc(closeGrace, func() { ws.c.Close() }) // unblocks a stuck writer
			defer t.Stop()
			if code != 1006 {
				<-ws.writerDone
				p := make([]byte, 2, 2+len(reason))
				binary.BigEndian.PutUint16(p, uint16(code))
				p = append(p, reason...)
				ws.WriteFrame(OpClose, p)
			}
			ws.c.Close()
		}()
	})
}

// CloseStatus returns the code and reason this side closed with; code 0
// means Close has not been called.
func (ws *Conn) CloseStatus() (code int, reason string) {
	ws.qmu.Lock()
	defer ws.qmu.Unlock()
	return ws.endCode, ws.endWhy
}

// Done is closed once Close has closed the connection.
func (ws *Conn) Done() <-chan struct{} { return ws.gone }

// Dial is a minimal client for tests and probes. dial opens the transport;
// the client has no read deadline and sends no pings. Its writer goroutine
// runs until Close, or until ReadMessage returns any error other than a
// timeout between messages.
func Dial(ctx context.Context, dial func(ctx context.Context) (net.Conn, error), host, path string) (*Conn, error) {
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
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Accept") != AcceptKey(key) {
		c.Close()
		return nil, fmt.Errorf("websocket handshake failed: %s", resp.Status)
	}
	ws := newConn(c, br, true)
	go ws.writer()
	return ws, nil
}
