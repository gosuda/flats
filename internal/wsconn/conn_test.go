package wsconn

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebSocketIdleReadDeadline(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ws := newConn(a, bufio.NewReader(a), false)
	ws.readIdleTimeout = 20 * time.Millisecond
	_, err := ws.ReadMessage(1 << 20)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("idle read error = %v, want timeout", err)
	}
}

func TestWebSocketControlFramesKeepReadAlive(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	server := newConn(a, bufio.NewReader(a), false)
	peer := newConn(b, bufio.NewReader(b), true)
	server.readIdleTimeout = 500 * time.Millisecond
	result := make(chan error, 1)
	go func() {
		msg, err := server.ReadMessage(1 << 20)
		if err == nil && msg != "ok" {
			err = errors.New("unexpected message")
		}
		result <- err
	}()
	for i := 0; i < 4; i++ {
		time.Sleep(200 * time.Millisecond)
		if err := peer.WriteFrame(OpPong, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := peer.WriteFrame(OpText, []byte("ok")); err != nil {
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

// A browser that only listens sends nothing but pongs. Server pings must keep
// such an idle, live client connected past the read deadline.
func TestWebSocketServerPingsKeepIdleClientAlive(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	server := newConn(a, bufio.NewReader(a), false)
	server.readIdleTimeout = 300 * time.Millisecond
	server.pingInterval = 50 * time.Millisecond
	go server.writer()
	defer server.Close(1006, "")
	peer := newConn(b, bufio.NewReader(b), true)
	go peer.ReadMessage(1 << 20) // answers pings with pongs, like a browser

	result := make(chan error, 1)
	go func() {
		msg, err := server.ReadMessage(1 << 20)
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
	if err := peer.WriteFrame(OpText, []byte("ok")); err != nil {
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

// The maximum message size is the caller's: a message of exactly max bytes
// passes, one byte more is refused with 1009, whether it arrives in one frame
// or in fragments that are each under the limit.
func TestReadMessageMaxSize(t *testing.T) {
	const max = 1000
	cases := []struct {
		name   string
		frames func(peer *Conn) error
		want   string // "" means refused with 1009
	}{
		{"one frame at max", func(p *Conn) error { return p.WriteFrame(OpText, make([]byte, max)) }, strings.Repeat("\x00", max)},
		{"one frame over max", func(p *Conn) error { return p.WriteFrame(OpText, make([]byte, max+1)) }, ""},
		{"fragments over max", func(p *Conn) error { return writeFragments(p, max/2+1, max/2) }, ""},
		{"fragments at max", func(p *Conn) error { return writeFragments(p, max/2, max/2) }, strings.Repeat("\x00", max)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			server := newConn(a, bufio.NewReader(a), false)
			peer := newConn(b, bufio.NewReader(b), true)
			go tc.frames(peer)
			got, err := server.ReadMessage(max)
			if tc.want != "" {
				if err != nil || got != tc.want {
					t.Fatalf("got %d bytes, %v", len(got), err)
				}
				return
			}
			var ce *CloseError
			if !errors.As(err, &ce) || ce.Code != 1009 || ce.Reason != "message too big" {
				t.Fatalf("err = %v, want 1009 message too big", err)
			}
		})
	}
}

// writeFragments sends a text message as a non-final text frame and a final
// continuation frame of the given sizes.
func writeFragments(p *Conn, first, second int) error {
	// A masked text frame without FIN, with a 16-bit length and a zero mask
	// key (so the payload goes out unchanged).
	hdr := []byte{OpText, 0x80 | 126, byte(first >> 8), byte(first), 0, 0, 0, 0}
	if _, err := p.c.Write(append(hdr, make([]byte, first)...)); err != nil {
		return err
	}
	return p.WriteFrame(OpCont, make([]byte, second))
}

func TestUpgradeAndDial(t *testing.T) {
	shared := &Budget{}
	shared.Lower(1 << 20)
	conns := make(chan *Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsUpgrade(r) {
			http.Error(w, "not an upgrade", http.StatusBadRequest)
			return
		}
		c, err := Upgrade(w, r, Options{QueuedBytes: 2048, Shared: shared})
		if err != nil {
			t.Error(err)
			return
		}
		conns <- c
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	client, err := Dial(t.Context(), func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}, addr, "/")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(1006, "")
	server := <-conns
	if server.Budget().Limit() != 2048 || server.readIdleTimeout != DefaultReadIdleTimeout || server.pingInterval != DefaultPingInterval {
		t.Fatalf("options: limit %d, idle %v, ping %v", server.Budget().Limit(), server.readIdleTimeout, server.pingInterval)
	}
	if client.Budget().Limit() != DefaultQueuedBytes || client.readIdleTimeout != 0 || client.pingInterval != 0 {
		t.Fatal("the test client has a deadline or pings")
	}
	if err := server.Send("hello"); err != nil {
		t.Fatal(err)
	}
	if got, err := client.ReadMessage(1 << 20); err != nil || got != "hello" {
		t.Fatalf("client read %q, %v", got, err)
	}
	if err := client.WriteFrame(OpBinary, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if got, err := server.ReadMessage(1 << 20); err != nil || got != "hi" {
		t.Fatalf("server read %q, %v", got, err)
	}
	server.Close(4000, "bye")
	_, err = client.ReadMessage(1 << 20)
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != 4000 || ce.Reason != "bye" {
		t.Fatalf("close: %v", err)
	}
	<-server.Done()
	if code, reason := server.CloseStatus(); code != 4000 || reason != "bye" {
		t.Fatalf("close status %d %q", code, reason)
	}
	if shared.Used() != 0 {
		t.Fatal("shared budget leaked", shared.Used())
	}
}

func TestUpgradeRefusesBadHandshake(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	if _, err := Upgrade(w, r, Options{}); err == nil || w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, err %v", w.Code, err)
	}
}

// dialPair returns a Dial client and the server Conn it reached.
func dialPair(t *testing.T) (client, server *Conn) {
	t.Helper()
	conns := make(chan *Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, Options{})
		if err != nil {
			t.Error(err)
			close(conns)
			return
		}
		conns <- c
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	client, err := Dial(t.Context(), func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}, addr, "/")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(1006, "") })
	server = <-conns
	if server == nil {
		t.FailNow()
	}
	t.Cleanup(func() { server.Close(1006, "") })
	return client, server
}

// waitEnded fails unless the client's writer goroutine has exited and its
// connection is closed.
func waitEnded(t *testing.T, ws *Conn) {
	t.Helper()
	for name, ch := range map[string]<-chan struct{}{"writer": ws.writerDone, "connection": ws.Done()} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("client %s still running after its socket failed", name)
		}
	}
}

// A Dial client whose read fails ends its whole lifecycle, so a caller that
// only closes the raw socket (or a peer that goes away) leaks no writer.
func TestDialClientEndsWhenReadFails(t *testing.T) {
	cases := []struct {
		name string
		end  func(client, server *Conn)
	}{
		{"raw socket closed locally", func(client, _ *Conn) { client.NetConn().Close() }},
		{"peer closed without a close frame", func(_, server *Conn) { server.Close(1006, "") }},
		{"peer sent a close frame", func(_, server *Conn) { server.Close(4000, "bye") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, server := dialPair(t)
			tc.end(client, server)
			if _, err := client.ReadMessage(1 << 20); err == nil {
				t.Fatal("read succeeded on an ended connection")
			}
			waitEnded(t, client)
			if code, _ := client.CloseStatus(); code != 1006 {
				t.Fatalf("client close code %d, want 1006", code)
			}
			if err := client.Send("late"); err == nil {
				t.Fatal("Send accepted a message on an ended connection")
			}
		})
	}
}

// A read timeout is the caller's deadline, not a dead socket: the client stays
// usable.
func TestDialClientSurvivesReadTimeout(t *testing.T) {
	client, server := dialPair(t)
	client.NetConn().SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	_, err := client.ReadMessage(1 << 20)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read error = %v, want timeout", err)
	}
	select {
	case <-client.writerDone:
		t.Fatal("a read timeout ended the client")
	default:
	}
	client.NetConn().SetReadDeadline(time.Time{})
	if err := server.Send("still here"); err != nil {
		t.Fatal(err)
	}
	if got, err := client.ReadMessage(1 << 20); err != nil || got != "still here" {
		t.Fatalf("client read %q, %v", got, err)
	}
	client.Close(1006, "")
	waitEnded(t, client)
}

// A timeout inside a frame or a fragmented message loses the part already
// read, so a client cannot retry: it ends its connection and every later read
// fails without waiting.
func TestDialClientEndsOnTimeoutInsideMessage(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte // unmasked server-to-client bytes
	}{
		{"between fragments", []byte{0x01, 0x03, 'a', 'b', 'c'}}, // non-final text frame
		{"inside a frame header", []byte{0x81}},
		{"inside a frame payload", []byte{0x81, 0x05, 'a', 'b'}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, server := dialPair(t)
			if _, err := server.NetConn().Write(tc.raw); err != nil {
				t.Fatal(err)
			}
			client.NetConn().SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			_, err := client.ReadMessage(1 << 20)
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() || !strings.Contains(err.Error(), "inside a message") {
				t.Fatalf("read error = %v, want a timeout inside a message", err)
			}
			waitEnded(t, client)
			if code, _ := client.CloseStatus(); code != 1006 {
				t.Fatalf("client close code %d, want 1006", code)
			}
			_, err = client.ReadMessage(1 << 20)
			if !errors.Is(err, errReadBroken) || errors.As(err, &ne) {
				t.Fatalf("retry error = %v, want errReadBroken and no timeout", err)
			}
		})
	}
}

// On a server connection an idle timeout between messages can be retried,
// while one between the fragments of a message leaves the reader unusable;
// the owner closes the connection.
func TestServerReadTimeoutBetweenAndInsideMessages(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	server := newConn(a, bufio.NewReader(a), false)
	server.readIdleTimeout = 200 * time.Millisecond
	var ne net.Error
	_, err := server.ReadMessage(1 << 20)
	if !errors.As(err, &ne) || !ne.Timeout() || strings.Contains(err.Error(), "inside a message") {
		t.Fatalf("idle read error = %v, want a plain timeout", err)
	}
	// Masked client frames with a zero key carry their payload unchanged.
	write := func(raw ...byte) {
		go b.Write(raw)
	}
	write(0x81, 0x82, 0, 0, 0, 0, 'o', 'k')
	if got, err := server.ReadMessage(1 << 20); err != nil || got != "ok" {
		t.Fatalf("read after an idle timeout = %q, %v", got, err)
	}
	write(0x01, 0x83, 0, 0, 0, 0, 'a', 'b', 'c') // non-final text frame
	_, err = server.ReadMessage(1 << 20)
	if !errors.As(err, &ne) || !ne.Timeout() || !strings.Contains(err.Error(), "inside a message") {
		t.Fatalf("read error = %v, want a timeout inside a message", err)
	}
	write(0x80, 0x81, 0, 0, 0, 0, 'd') // the final continuation arrives late
	_, err = server.ReadMessage(1 << 20)
	if !errors.Is(err, errReadBroken) || errors.As(err, &ne) {
		t.Fatalf("retry error = %v, want errReadBroken and no timeout", err)
	}
	if code, _ := server.CloseStatus(); code != 0 {
		t.Fatalf("the reader closed a server connection (code %d); its owner closes it", code)
	}
}
