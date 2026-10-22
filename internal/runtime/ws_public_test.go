package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

// A signal raised while the event queue is full is not lost: the loop
// handles it after the event it takes next, once however often it was
// raised.
func TestWSSignalCoalescesAndIsNeverLost(t *testing.T) {
	t.Parallel()
	var got []string
	h := &wsHub{events: make(chan wsEvent, 1), hooks: &wsHooks{dispatched: func(ev wsEvent) {
		name := ev.Type
		if ev.Data != nil {
			name += ":" + *ev.Data
		}
		got = append(got, name)
	}}}
	pending := "index.md"
	h.events <- wsEvent{Type: "wake", Data: &pending}
	h.signal("access")
	h.signal("access")
	h.signal("other")
	if len(h.events) != 1 {
		t.Fatal("the signal blocked or replaced the queued event", len(h.events))
	}
	h.handle(<-h.events)
	if want := "wake:index.md signal:access signal:other"; strings.Join(got, " ") != want {
		t.Fatalf("handled %v, want %s", got, want)
	}
	// With room in the queue, a signal also wakes an idle loop; the wake-up
	// event itself reaches no handler.
	got = nil
	h.signal("access")
	ev := <-h.events
	if ev.Type != "signal" {
		t.Fatalf("%+v", ev)
	}
	h.handle(ev)
	h.handle(wsEvent{Type: "signal"})
	if strings.Join(got, " ") != "signal:access" {
		t.Fatal(got)
	}
	// Distinct pending names are bounded.
	for i := 0; i < 2*maxWSSignals; i++ {
		h.signal(fmt.Sprint("name-", i))
	}
	if n := len(h.takeSignals()); n != maxWSSignals {
		t.Fatal("pending names", n)
	}
}

func TestTokenBucketReserveRepaysDebt(t *testing.T) {
	t.Parallel()
	b := newTokenBucket(10, 2)
	if b.reserve(1) != 0 || b.reserve(1) != 0 {
		t.Fatal("the burst waited")
	}
	if w := b.reserve(1); w < 80*time.Millisecond || w > 110*time.Millisecond {
		t.Fatal("wait", w)
	}
	// A request larger than the burst waits for its whole debt.
	big := newTokenBucket(100, 10)
	if w := big.reserve(60); w < 490*time.Millisecond || w > 510*time.Millisecond {
		t.Fatal("wait", w)
	}
	if big.take(1) {
		t.Fatal("took from a bucket in debt")
	}
}

// Public connections have their own slots: filling them leaves every
// Private slot, and Private connections never take a Public slot.
func TestWSPublicSlotsAreSeparate(t *testing.T) {
	t.Parallel()
	h := &wsHub{}
	h.enablePublicPools()
	h.publicStarts = newTokenBucket(1e6, 1e6)
	for i := 0; i < MaxWSPublicConnections; i++ {
		if !h.admit(true) {
			t.Fatal("public slot", i)
		}
	}
	if h.admit(true) {
		t.Fatal("admitted a public connection over its slots")
	}
	for i := 0; i < MaxWSConnections; i++ {
		if !h.admit(false) {
			t.Fatal("private slot", i)
		}
	}
	if h.admit(false) {
		t.Fatal("admitted a private connection over its slots")
	}
	h.releaseSlot(true)
	if !h.admit(true) || h.active != MaxWSConnections {
		t.Fatal("released public slot")
	}
}

// New Public connections are admitted at most 20 a second after a burst of
// 40, before the upgrade, while Private connections are not limited.
func TestWSPublicConnectionChurnLimit(t *testing.T) {
	t.Parallel()
	h := &wsHub{}
	h.enablePublicPools()
	admitted := 0
	for i := 0; i < 60; i++ {
		if h.admit(true) {
			admitted++
			h.releaseSlot(true)
		}
	}
	if admitted < 40 || admitted > 41 {
		t.Fatal("admitted", admitted)
	}
	for i := 0; i < 100; i++ {
		if !h.admit(false) {
			t.Fatal("private admission limited")
		}
		h.releaseSlot(false)
	}
	time.Sleep(160 * time.Millisecond)
	if !h.admit(true) {
		t.Fatal("no admission after the bucket refilled")
	}
}

// headerConn adds a request header to the handshake wsconn.Dial writes.
type headerConn struct {
	net.Conn
	header string
	sent   bool
}

func (c *headerConn) Write(b []byte) (int, error) {
	if !c.sent && c.header != "" {
		c.sent = true
		_, err := c.Conn.Write([]byte(strings.Replace(string(b), "\r\n\r\n", "\r\n"+c.header+"\r\n\r\n", 1)))
		return len(b), err
	}
	return c.Conn.Write(b)
}

// publicHub serves a hub with separate Public pools whose events go to
// dispatched instead of a JavaScript VM.
func publicHub(t *testing.T, hooks *wsHooks) (*wsHub, func(ctx context.Context, private bool) (*wsconn.Conn, error)) {
	t.Helper()
	h := &wsHub{conns: map[int64]*wsconn.Conn{}, events: make(chan wsEvent, wsEventQueue), stop: make(chan struct{}), hooks: hooks}
	h.budget.Lower(MaxWSQueuedBytesPerFlat)
	h.enablePublicPools()
	h.wg.Add(1)
	go h.loop()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, r.URL.String(), requestHeaders(r))
	}))
	t.Cleanup(func() {
		h.shutdown()
		srv.Close()
	})
	addr := strings.TrimPrefix(srv.URL, "http://")
	dial := func(ctx context.Context, private bool) (*wsconn.Conn, error) {
		header := "X-Flats-Access: public"
		if private {
			header = "X-Flats-Access: private"
		}
		return wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			return &headerConn{Conn: c, header: header}, nil
		}, addr, "/ws")
	}
	return h, dial
}

// While Public connections fill their slots and flood frames at the
// ingress limits, a Private connection is admitted and each of its events
// is handled with at most one Public event ahead of it
// (docs-public-edit-TECH.md 3.3).
func TestWSPublicFloodKeepsPrivateFirst(t *testing.T) {
	t.Parallel()
	var publicStarted atomic.Int64
	var seen sync.Map // private message -> public events started when it was posted
	worst := atomic.Int64{}
	privateHandled := make(chan struct{}, 64)
	hooks := &wsHooks{
		posted: func(ev *wsEvent) {
			if !ev.public && ev.Type == "message" {
				seen.Store(*ev.Data, publicStarted.Load())
			}
		},
		dispatched: func(ev wsEvent) {
			if ev.public {
				publicStarted.Add(1)
				time.Sleep(time.Millisecond) // a Public event being handled
				return
			}
			if ev.Type == "message" {
				at, _ := seen.Load(*ev.Data)
				if ahead := publicStarted.Load() - at.(int64); ahead > worst.Load() {
					worst.Store(ahead)
				}
				privateHandled <- struct{}{}
			}
		},
	}
	h, dial := publicHub(t, hooks)
	h.publicStarts = newTokenBucket(1e6, 1e6) // fill the slots at once
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var flood sync.WaitGroup
	stopFlood := make(chan struct{})
	for i := 0; i < MaxWSPublicConnections; i++ {
		c, err := dial(ctx, false)
		if err != nil {
			t.Fatal("public connection", i, err)
		}
		defer c.Close(1006, "")
		flood.Add(1)
		go func() {
			defer flood.Done()
			payload := []byte(strings.Repeat("x", 4096))
			for {
				select {
				case <-stopFlood:
					return
				default:
				}
				c.NetConn().SetWriteDeadline(time.Now().Add(time.Second))
				if c.WriteFrame(wsconn.OpText, payload) != nil {
					select {
					case <-stopFlood:
						return
					default:
						continue
					}
				}
			}
		}()
	}
	if _, err := dial(ctx, false); err == nil {
		t.Fatal("admitted a public connection over the slots")
	}
	member, err := dial(ctx, true)
	if err != nil {
		t.Fatal("private admission while public slots are full:", err)
	}
	defer member.Close(1006, "")
	for i := 0; i < 30; i++ {
		if err := member.WriteFrame(wsconn.OpText, []byte(fmt.Sprint("member-", i))); err != nil {
			t.Fatal(err)
		}
		select {
		case <-privateHandled:
		case <-ctx.Done():
			t.Fatal("private event not handled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stopFlood)
	if worst.Load() > 1 {
		t.Fatalf("a private event waited behind %d public events", worst.Load())
	}
	if publicStarted.Load() < 100 {
		t.Fatal("the public flood did not run", publicStarted.Load())
	}
}

// One Public connection's frames are paced by its own buckets: a burst of
// 40, then 20 a second; large frames by 256 KiB a second after 512 KiB.
func TestWSPublicIngressPacesReaders(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var times []time.Time
	var sizes []int
	hooks := &wsHooks{dispatched: func(ev wsEvent) {
		if ev.Type == "message" {
			mu.Lock()
			times = append(times, time.Now())
			sizes = append(sizes, len(*ev.Data))
			mu.Unlock()
		}
	}}
	_, dial := publicHub(t, hooks)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	c, err := dial(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(1006, "")
	start := time.Now()
	for i := 0; i < 80; i++ {
		if err := c.WriteFrame(wsconn.OpText, []byte("f")); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(time.Second)
	mu.Lock()
	n := 0
	for _, at := range times {
		if at.Sub(start) <= time.Second {
			n++
		}
	}
	mu.Unlock()
	if n < 40 || n > 62 {
		t.Fatalf("%d frames in the first second, want a burst of 40 then 20 a second", n)
	}
	// Bytes: three 400 KiB frames after the frame bucket refilled.
	time.Sleep(2 * time.Second)
	mu.Lock()
	times, sizes = nil, nil
	mu.Unlock()
	big := []byte(strings.Repeat("y", 400<<10))
	for i := 0; i < 3; i++ {
		if err := c.WriteFrame(wsconn.OpText, big); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		done := len(times) == 3
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("large frames not delivered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	// 512 KiB burst, then 256 KiB a second: the third frame's 1200 KiB
	// arrive no sooner than about 2.7 s after the first.
	if gap := times[2].Sub(times[0]); gap < 2*time.Second {
		t.Fatalf("1200 KiB in %v", gap)
	}
}

// A request VM's __flats_docsSignal reaches websocket.signal in the
// WebSocket VM; a module asks for separate Public pools with
// websocket.publicPools, and Public connections then use them.
func TestWSDocsSignalAndPublicPools(t *testing.T) {
	t.Parallel()
	code := `const sockets = new Set();
export default {
 fetch() { globalThis.__flats_docsSignal("access"); return new Response("raised"); },
 websocket: {
  publicPools: true,
  open(ws) { sockets.add(ws); ws.send("open " + ws.headers.get("x-flats-access")); },
  close(ws) { sockets.delete(ws); },
  signal(name) { for (const ws of sockets) ws.send("signal " + name); },
 }};`
	f := mustStart(t, newManager(t), "docs-signal", map[string]string{"index.js": code}, "index.js", nil)
	addr := strings.TrimPrefix(f.srv.URL, "http://")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	dial := func(access string) *wsconn.Conn {
		c, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			return &headerConn{Conn: c, header: "X-Flats-Access: " + access}, err
		}, addr, "/")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close(1006, "") })
		c.NetConn().SetDeadline(time.Now().Add(10 * time.Second))
		return c
	}
	member, visitor := dial("private"), dial("public")
	for c, want := range map[*wsconn.Conn]string{member: "open private", visitor: "open public"} {
		if got, err := c.ReadMessage(MaxWSMessage); err != nil || got != want {
			t.Fatal(got, err)
		}
	}
	if r := f.get(t, "/"); r.body != "raised" {
		t.Fatal(r)
	}
	for _, c := range []*wsconn.Conn{member, visitor} {
		if got, err := c.ReadMessage(MaxWSMessage); err != nil || got != "signal access" {
			t.Fatal(got, err)
		}
	}
}

// A module without publicPools keeps one pool for every connection.
func TestWSPublicPoolsAreOptIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, tc := range []struct {
		code string
		want bool
	}{
		{`export default {websocket: {open() {}}};`, false},
		{`export default {websocket: {publicPools: true, open() {}}};`, true},
	} {
		writeFiles(t, dir, map[string]string{"index.js": tc.code})
		w := &worker{spec: workerSpec{Dir: dir, Entry: "index.js", DataDir: t.TempDir()}, log: newChildLog(io.Discard)}
		w.data = newDataStore(w.spec.DataDir)
		if err := w.init(); err != nil {
			t.Fatal(err)
		}
		got := w.js.wsHub.publicPools
		w.eng.close()
		w.data.Close()
		if got != tc.want {
			t.Fatalf("%s: publicPools %v", tc.code, got)
		}
	}
}
