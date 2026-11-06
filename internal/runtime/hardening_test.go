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
class BigIntNameError extends Error {
  get name() { return 1n; }
}
class NumberNameError extends Error {
  constructor() {
    super("num-msg");
    this.name = 42;
  }
}
class ThrowingMessageError extends Error {
  get message() { throw new Error("message trap"); }
}
export default {
  async fetch(req) {
    const u = new URL(req.url);
    const check = u.searchParams.get("check");
    if (check) {
      let raw;
      if (check === "boom") {
        raw = await globalThis.__flats_dispatch(JSON.stringify({
          method: "GET",
          url: "http://flat.test/throw?which=boom",
          headers: {},
        }));
      } else if (check === "bigint-name") {
        raw = await globalThis.__flats_dispatch(JSON.stringify({
          method: "GET",
          url: "http://flat.test/throw?which=bigint-name",
          headers: {},
        }));
      } else if (check === "number-name") {
        raw = await globalThis.__flats_dispatch(JSON.stringify({
          method: "GET",
          url: "http://flat.test/throw?which=number-name",
          headers: {},
        }));
      } else if (check === "throw-message") {
        raw = await globalThis.__flats_dispatch(JSON.stringify({
          method: "GET",
          url: "http://flat.test/throw?which=throw-message",
          headers: {},
        }));
      }
      return new Response(raw, { headers: { "content-type": "application/json" } });
    }
    const which = u.searchParams.get("which");
    if (which === "boom") throw hostile();
    if (which === "bigint-name") throw new BigIntNameError("bigint-msg");
    if (which === "number-name") throw new NumberNameError();
    if (which === "throw-message") throw new ThrowingMessageError();
    return new Response("http");
  },
  websocket: {
    open(ws) { ws.send("open"); },
    message(ws, data) {
      if (data === "boom") throw hostile();
      if (data === "bigint-name") throw new BigIntNameError("bigint-msg");
      if (data === "number-name") throw new NumberNameError();
      if (data === "throw-message") throw new ThrowingMessageError();
      ws.send("echo:" + data);
    },
  },
};`}, "index.js", nil)
	addr := strings.TrimPrefix(f.srv.URL, "http://")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	c1, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}, addr, "/ws")
	if err != nil {
		t.Fatalf("dial c1: %v; logs:\n%s", err, f.logs)
	}
	defer c1.Close(1006, "")
	c1.NetConn().SetDeadline(time.Now().Add(10 * time.Second))

	c2, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}, addr, "/ws")
	if err != nil {
		t.Fatalf("dial c2: %v; logs:\n%s", err, f.logs)
	}
	defer c2.Close(1006, "")
	c2.NetConn().SetDeadline(time.Now().Add(10 * time.Second))

	if got, err := c1.ReadMessage(MaxWSMessage); err != nil || got != "open" {
		t.Fatalf("c1 open = %q, %v; logs:\n%s", got, err, f.logs)
	}
	if got, err := c2.ReadMessage(MaxWSMessage); err != nil || got != "open" {
		t.Fatalf("c2 open = %q, %v; logs:\n%s", got, err, f.logs)
	}

	cases := []struct {
		name     string
		trigger  string
		wantLog  string
		wantName string
		wantMsg  string
	}{
		{
			name:     "hostile-proxy",
			trigger:  "boom",
			wantLog:  "websocket message handler: Error: uncaught: a value that cannot be described",
			wantName: "Error",
			wantMsg:  "uncaught: a value that cannot be described",
		},
		{
			name:     "bigint-name-getter",
			trigger:  "bigint-name",
			wantLog:  "websocket message handler: 1: bigint-msg",
			wantName: "1",
			wantMsg:  "bigint-msg",
		},
		{
			name:     "number-name",
			trigger:  "number-name",
			wantLog:  "websocket message handler: 42: num-msg",
			wantName: "42",
			wantMsg:  "num-msg",
		},
		{
			name:     "throwing-message-getter",
			trigger:  "throw-message",
			wantLog:  "websocket message handler: Error: uncaught: a value that cannot be described",
			wantName: "Error",
			wantMsg:  "uncaught: a value that cannot be described",
		},
	}

	for _, tc := range cases {
		c1.NetConn().SetDeadline(time.Now().Add(10 * time.Second))
		c2.NetConn().SetDeadline(time.Now().Add(10 * time.Second))

		if err := c1.WriteFrame(wsconn.OpText, []byte(tc.trigger)); err != nil {
			t.Fatalf("%s: write trigger: %v", tc.name, err)
		}
		if err := c1.WriteFrame(wsconn.OpText, []byte("after:"+tc.trigger)); err != nil {
			t.Fatalf("%s: write after: %v", tc.name, err)
		}
		got, err := c1.ReadMessage(MaxWSMessage)
		if err != nil {
			t.Fatalf("%s: connection c1 was closed by thrown value: %v; logs:\n%s", tc.name, err, f.logs)
		}
		if got != "echo:after:"+tc.trigger {
			t.Fatalf("%s: c1 message after thrown value = %q, want echo:after:%s; logs:\n%s", tc.name, got, tc.trigger, f.logs)
		}

		if err := c2.WriteFrame(wsconn.OpText, []byte("ping:"+tc.trigger)); err != nil {
			t.Fatalf("%s: other connection c2 write failed: %v", tc.name, err)
		}
		got2, err := c2.ReadMessage(MaxWSMessage)
		if err != nil {
			t.Fatalf("%s: other ws connection c2 was closed by thrown value: %v; logs:\n%s", tc.name, err, f.logs)
		}
		if got2 != "echo:ping:"+tc.trigger {
			t.Fatalf("%s: other ws connection c2 message = %q, want echo:ping:%s; logs:\n%s", tc.name, got2, tc.trigger, f.logs)
		}

		f.logs.waitFor(t, tc.wantLog)

		var rawEnv struct {
			Error map[string]any `json:"__error"`
		}
		f.json(t, "/?check="+tc.trigger, &rawEnv)
		nameVal, ok := rawEnv.Error["name"].(string)
		if !ok {
			t.Fatalf("%s: expected string name in __error envelope, got %T (%v)", tc.name, rawEnv.Error["name"], rawEnv.Error["name"])
		}
		if nameVal != tc.wantName {
			t.Fatalf("%s: name = %q, want %q", tc.name, nameVal, tc.wantName)
		}
		msgVal, ok := rawEnv.Error["message"].(string)
		if !ok {
			t.Fatalf("%s: expected string message in __error envelope, got %T (%v)", tc.name, rawEnv.Error["message"], rawEnv.Error["message"])
		}
		if msgVal != tc.wantMsg {
			t.Fatalf("%s: message = %q, want %q", tc.name, msgVal, tc.wantMsg)
		}
		if _, ok := rawEnv.Error["stack"].(string); !ok {
			t.Fatalf("%s: expected string stack in __error envelope, got %T (%v)", tc.name, rawEnv.Error["stack"], rawEnv.Error["stack"])
		}

		var typedEnv struct {
			Error jsError `json:"__error"`
		}
		f.json(t, "/?check="+tc.trigger, &typedEnv)
		if typedEnv.Error.Name != tc.wantName || typedEnv.Error.Message != tc.wantMsg {
			t.Fatalf("%s: unmarshaled jsError = %+v, want name=%q msg=%q", tc.name, typedEnv.Error, tc.wantName, tc.wantMsg)
		}
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
