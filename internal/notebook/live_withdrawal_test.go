package notebook

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

type liveSnapshotGateWriter struct {
	http.ResponseWriter
	entered, release chan struct{}
	once             *sync.Once
}

func (w liveSnapshotGateWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &liveSnapshotGateConn{conn, w.entered, w.release, w.once}, rw, nil
}

type liveSnapshotGateConn struct {
	net.Conn
	entered, release chan struct{}
	once             *sync.Once
}

func (c *liveSnapshotGateConn) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"type":"snapshot"`)) {
		c.once.Do(func() { close(c.entered); <-c.release })
	}
	return c.Conn.Write(p)
}

func TestLiveWithdrawalAfterHandlerExit(t *testing.T) {
	f := newWriterFixture(t)
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(returned)
		r.Header.Set("X-Flats-Access", "public")
		r.Header.Set("Origin", "http://"+r.Host)
		f.in.ServeHTTP(liveSnapshotGateWriter{w, entered, release, &once}, r)
	}))
	defer server.Close()
	ws, err := liveConnect(t.Context(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Abort()
	liveHelloSend(t, ws, "", f.page.ID)
	liveUntil(t, ws, "welcome")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot transport not gated")
	}
	if err := ws.WriteFrame(wsconn.OpText, []byte(`{`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("handler did not exit")
	}
	f.in.live.mu.Lock()
	registered := len(f.in.live.conns)
	f.in.live.mu.Unlock()
	if registered != 0 {
		t.Fatalf("handler left %d registered connections", registered)
	}
	f.in.CancelPublic()
	unblock()
	_ = ws.NetConn().SetReadDeadline(time.Now().Add(2 * time.Second))
	raw, err := ws.ReadMessage(liveServerFrame)
	if err == nil {
		t.Fatalf("content delivered after withdrawal and handler exit: %s", raw)
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatal("withdrawal did not abort the transport")
	}
}
