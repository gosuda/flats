package runtime

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/wsconn"
)

// A handler may throw a value that cannot be described: the runtime must
// report the event failure, not poison the shared WebSocket runtime.
func TestWebSocketThrownValueKeepsConnections(t *testing.T) {
	t.Parallel()
	f := mustStart(t, newManager(t), "ws-throw", map[string]string{"index.js": `
const hostile = () => new Proxy({}, {
  getPrototypeOf() { throw new Error("prototype trap"); },
  get() { throw new Error("property trap"); },
});
export default {
  fetch() { return new Response("http"); },
  websocket: {
    open(ws) { ws.send("open"); },
    message(ws, data) {
      if (data === "boom") throw hostile();
      ws.send("echo:" + data);
    },
  },
};`}, "index.js", nil)
	addr := strings.TrimPrefix(f.srv.URL, "http://")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}, addr, "/ws")
	if err != nil {
		t.Fatalf("dial: %v; logs:\n%s", err, f.logs)
	}
	defer c.Close(1006, "")
	c.NetConn().SetDeadline(time.Now().Add(10 * time.Second))
	if got, err := c.ReadMessage(MaxWSMessage); err != nil || got != "open" {
		t.Fatalf("open = %q, %v; logs:\n%s", got, err, f.logs)
	}
	if err := c.WriteFrame(wsconn.OpText, []byte("boom")); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteFrame(wsconn.OpText, []byte("after")); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadMessage(MaxWSMessage)
	if err != nil {
		t.Fatalf("connection was closed by the thrown value: %v; logs:\n%s", err, f.logs)
	}
	if got != "echo:after" {
		t.Fatalf("message after the thrown value = %q, want echo:after; logs:\n%s", got, f.logs)
	}
}

// A close frame carries at most 123 bytes of valid UTF-8 and a defined code.
func TestWebSocketCloseReasonAndCodeAreValid(t *testing.T) {
	t.Parallel()
	f := mustStart(t, newManager(t), "ws-close", map[string]string{"index.js": `
export default {
  fetch() { return new Response("http"); },
  websocket: {
    open(ws) { ws.send("open"); },
    message(ws, data) {
      if (data === "long") ws.close(1000, "é".repeat(100));
      else ws.close(1005, "no status");
    },
  },
};`}, "index.js", nil)
	addr := strings.TrimPrefix(f.srv.URL, "http://")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	dial := func() *wsconn.Conn {
		t.Helper()
		c, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}, addr, "/ws")
		if err != nil {
			t.Fatalf("dial: %v; logs:\n%s", err, f.logs)
		}
		c.NetConn().SetDeadline(time.Now().Add(10 * time.Second))
		if got, err := c.ReadMessage(MaxWSMessage); err != nil || got != "open" {
			t.Fatalf("open = %q, %v", got, err)
		}
		return c
	}
	closeOf := func(c *wsconn.Conn, data string) *wsconn.CloseError {
		t.Helper()
		if err := c.WriteFrame(wsconn.OpText, []byte(data)); err != nil {
			t.Fatal(err)
		}
		_, err := c.ReadMessage(MaxWSMessage)
		ce, ok := err.(*wsconn.CloseError)
		if !ok {
			t.Fatalf("%s: close = %v, want a close frame; logs:\n%s", data, err, f.logs)
		}
		return ce
	}

	c := dial()
	defer c.Close(1006, "")
	ce := closeOf(c, "long")
	if ce.Code != 1000 {
		t.Fatalf("close code = %d, want 1000", ce.Code)
	}
	if len(ce.Reason) > 123 {
		t.Fatalf("close reason is %d bytes, over the 123-byte limit", len(ce.Reason))
	}
	if !utf8.ValidString(ce.Reason) {
		t.Fatalf("close reason split a UTF-8 rune: %q", ce.Reason)
	}
	if want := strings.Repeat("é", len(ce.Reason)/2); ce.Reason != want {
		t.Fatalf("close reason = %q, want the longest rune-aligned prefix %q", ce.Reason, want)
	}

	c2 := dial()
	defer c2.Close(1006, "")
	if ce := closeOf(c2, "reserved"); ce.Code != 1000 {
		t.Fatalf("reserved close code was sent as %d, want 1000", ce.Code)
	}
}

func TestDataStoreUseAfterClose(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	d := newDataStore(t.TempDir())
	if err := d.filePut("k", "v"); err != nil {
		t.Fatal(err)
	}
	if s, err := d.fileGet("k"); err != nil || s == nil || *s != "v" {
		t.Fatalf("get before close = %v, %v", s, err)
	}
	c, err := d.conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	d.Close()
	d.Close()
	for name, call := range map[string]func() error{
		"get":    func() error { _, err := d.fileGet("k"); return err },
		"put":    func() error { return d.filePut("k2", "v2") },
		"list":   func() error { _, err := d.fileList(""); return err },
		"delete": func() error { _, err := d.fileDelete("k"); return err },
		"conn": func() error {
			c, err := d.conn(ctx)
			if err == nil {
				c.Close()
			}
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s succeeded after Close", name)
		}
	}
}

func TestDataStoreCloseDuringOpen(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for range 100 {
		d := newDataStore(t.TempDir())
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if c, err := d.conn(ctx); err == nil {
				c.Close()
			}
		}()
		go func() {
			defer wg.Done()
			d.Close()
		}()
		wg.Wait()
		d.Close()
	}
}

// The batched digest result is one guest string: a result that cannot fit the
// runtime's memory must be refused with a clean error, not fail inside the
// wasm call, whose error carries a host stack trace.
func TestDocsCodecDigestsFitRuntimeMemory(t *testing.T) {
	t.Parallel()
	f := mustStart(t, newManager(t), "digests-limit", map[string]string{"index.js": `
export default {
  fetch(request) {
    const c = globalThis.__flats_docsCodec;
    const n = Number(new URL(request.url).searchParams.get("n"));
    try {
      return new Response("ok " + c.digests(new Array(n).fill("")).length);
    } catch (e) { return new Response("caught " + e.name + ": " + e.message); }
  },
};`}, "index.js", nil)
	const digests = DefaultMemoryPages * 65536 * 3 / 8 / 64
	r := f.do(t, "GET", fmt.Sprintf("/?n=%d", digests), "")
	if r.status != 200 || r.body != fmt.Sprintf("ok %d", digests*64) {
		t.Fatalf("largest result = %d %q; logs:\n%s", r.status, r.body, f.logs)
	}
	for _, n := range []int{digests + 1, 1 << 20} {
		r = f.do(t, "GET", fmt.Sprintf("/?n=%d", n), "")
		if r.status != 200 || !strings.Contains(r.body, "docs codec digest result is larger than this runtime can hold") {
			t.Fatalf("%d digests = %d %q; logs:\n%s", n, r.status, r.body, f.logs)
		}
		if strings.Contains(r.body, "goroutine") || strings.Contains(r.body, "null pointer") {
			t.Fatalf("host stack trace reached the guest: %q", r.body)
		}
	}
}
