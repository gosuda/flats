package board

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
)

// TestGoneNeverFollowsRejoin: when an editor's last socket closes and the
// same editor reconnects at once, a member never receives the departure
// after the reconnected socket's presence, so it keeps showing the editor.
// The hook holds relayGone after its member-facing decision while the
// editor reconnects and sends presence.
func TestGoneNeverFollowsRejoin(t *testing.T) {
	b := newTB(t)
	const key = "browser:eve"
	reached, proceed, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	hook := func(k string) {
		if k != key {
			return
		}
		// Only the first departure stops; every stop ends with the test.
		once.Do(func() {
			select {
			case reached <- struct{}{}:
			case <-done:
				return
			}
			select {
			case <-proceed:
			case <-done:
			}
		})
	}
	goneHook.Store(&hook)
	t.Cleanup(func() {
		close(done)
		goneHook.Store(nil)
	})

	m := connect(t, b.priv, map[string]any{"editor": "mia", "name": "Mia"}, nil)
	m.snapshot()
	e := connect(t, b.priv, map[string]any{"editor": "eve", "name": "Eve"}, nil)
	e.snapshot()
	e.send(map[string]any{"t": "pres", "c": []float64{1, 1}})
	m.expectPres(func(p map[string]any) bool { return p["key"] == key })

	e.ws.Close(1000, "")
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("relayGone did not run for the closed socket")
	}
	again := connect(t, b.priv, map[string]any{"editor": "eve", "name": "Eve"}, nil)
	again.snapshot()
	again.send(map[string]any{"t": "pres", "c": []float64{7, 7}})
	at := func(x float64) func(map[string]any) bool {
		return func(p map[string]any) bool {
			c, _ := p["c"].([]any)
			return p["t"] == "pres" && p["key"] == key && len(c) == 2 && c[0] == x
		}
	}
	// The member has the reconnected editor's presence before relayGone
	// goes on; whatever reached it before that is behind it now.
	if _, ok := m.next(at(7)); !ok {
		t.Fatalf("the member did not receive the reconnected editor's presence; backlog %v", m.backlog)
	}
	m.backlog = nil
	close(proceed)
	waitFor(t, func() bool {
		b.in.hub.mu.Lock()
		defer b.in.hub.mu.Unlock()
		return len(b.in.hub.departing) == 0
	})
	// A later presence marks the end of what relayGone could have queued.
	again.send(map[string]any{"t": "pres", "c": []float64{8, 8}})
	gone := func(p map[string]any) bool { return p["t"] == "pres" && p["key"] == key && p["gone"] == true }
	p, ok := m.next(func(p map[string]any) bool { return gone(p) || at(8)(p) })
	if !ok {
		t.Fatalf("the member did not receive the later presence; backlog %v", m.backlog)
	}
	if gone(p) {
		t.Fatal("a departure notice followed the reconnected editor's presence")
	}
}

// TestLargeDeltaResyncsPublicSockets: a replacing import whose delta is
// larger than a Public socket's queue cap sends every guest and viewer a
// resync and a snapshot of the imported board instead of closing them,
// within the host's Public budget and the guests' share, which return to
// where they were once the sockets close.
func TestLargeDeltaResyncsPublicSockets(t *testing.T) {
	ctx := context.Background()
	queued, snaps, share := publicQueued.Load(), publicSnaps.Load(), guestShare.Load()
	b := editTB(t)
	m := connect(t, b.priv, map[string]any{"editor": "mia", "name": "Mia"}, nil)
	m.snapshot()
	g1, _ := joinGuest(t, b, "Gia", "")
	g2, _ := joinGuest(t, b, "Gus", "")
	v1, _ := joinViewer(t, b, "", nil)
	v1.snapshot()
	v2, _ := joinViewer(t, b, "", nil)
	v2.snapshot()

	const n = 1500
	keys := model.Rekey(n)
	els := make([]model.Element, n)
	imported := map[string]model.Element{}
	for i := range els {
		els[i] = model.Element{ID: eid(), Type: model.TypeSticky, Z: keys[i], Text: noise(1000, i),
			Geom: model.Geom{X: float64(i%40) * 210, Y: float64(i/40) * 210, W: 200, H: 200}, Style: model.DefaultStyle(model.TypeSticky)}
		imported[els[i].ID] = els[i]
	}
	data := buildArchive(t, boardFile{Format: 1, Title: "Big", Grid: true}, els, nil, nil)
	if len(data) < publicQueueMax/4 {
		t.Fatalf("archive of %d bytes is too small to matter", len(data))
	}
	err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.ImportBoard(ctx, ImportRequest{OpID: "big-replace-1", Mode: "replace", Archive: bytes.NewReader(data), Size: int64(len(data)), Label: "Agent"}, other)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	b.state(func(st *engine.State) { seq = st.Seq() })

	for name, c := range map[string]*client{"guest 1": g1, "guest 2": g2, "viewer 1": v1, "viewer 2": v2} {
		r, ok := c.next(func(x map[string]any) bool { return x["t"] == "resync" || x["t"] == "delta" || x["t"] == "delta_part" })
		if !ok {
			t.Fatalf("%s received nothing after the import (closed: %v)", name, c.closed(0))
		}
		if r["t"] != "resync" {
			t.Fatalf("%s got %v instead of a resync", name, r["t"])
		}
		first, ok := c.next(func(x map[string]any) bool { return x["t"] == "snapshot" })
		if !ok || first["part"] != 1.0 || first["reset"] != true {
			t.Fatalf("%s: the snapshot after the replacing import does not start with reset: %v", name, first["reset"])
		}
		c.backlog = append([]map[string]any{first}, c.backlog...)
		got, elements, meta := c.snapshot()
		if got != seq || len(elements) != n || meta["title"] != "Big" {
			t.Fatalf("%s snapshot at seq %d with %d elements titled %v, want %d with %d", name, got, len(elements), meta["title"], seq, n)
		}
		seen := map[string]bool{}
		for _, e := range elements {
			id, _ := e["id"].(string)
			want, ok := imported[id]
			if !ok || seen[id] || e["type"] != want.Type || e["text"] != want.Text ||
				e["x"] != want.Geom.X || e["y"] != want.Geom.Y || e["w"] != want.Geom.W || e["h"] != want.Geom.H {
				t.Fatalf("%s snapshot element %v is not the imported one", name, e)
			}
			seen[id] = true
		}
	}
	// The member, within its own 4 MiB cap, gets the delta itself.
	if d, ok := m.next(func(x map[string]any) bool { return x["t"] == "delta" || x["t"] == "delta_part" || x["t"] == "resync" }); !ok || d["t"] == "resync" {
		t.Fatalf("member got %v", d)
	}

	// Every Public socket is still open and current.
	if a := g1.settle(1, sticky(eid(), "after", 0)); a["t"] != "ack" {
		t.Fatalf("guest batch after the resync: %v", a)
	}
	for name, c := range map[string]*client{"guest 2": g2, "viewer 1": v1, "viewer 2": v2} {
		if _, ok := c.next(func(x map[string]any) bool { return x["t"] == "delta" }); !ok {
			t.Fatalf("%s did not receive the change after the resync", name)
		}
	}

	for _, c := range []*client{m, g1, g2, v1, v2} {
		c.ws.Close(1000, "")
	}
	waitFor(t, func() bool {
		return publicQueued.Load() == queued && publicSnaps.Load() == snaps && guestShare.Load() == share
	})
}

// TestRoomCheckRefusesAssetsAndElements calls checkRoom directly. No
// guest batch can raise the number of referenced assets today (spec rule
// 19), so the asset branch is kept as defense in depth and tested here
// with a room taken as if before a batch that added one.
func TestRoomCheckRefusesAssetsAndElements(t *testing.T) {
	b := editTB(t)
	ctx := context.Background()
	m := connect(t, b.priv, map[string]any{"editor": "mia"}, nil)
	m.snapshot()
	for i := range 3 {
		info, err := b.in.AddAsset(ctx, pngBytes(t, 4+i, 4))
		if err != nil {
			t.Fatal(err)
		}
		if a := m.settle(i+1, map[string]any{"op": "add", "el": map[string]any{"id": eid(), "type": "image", "x": 0, "y": 0, "w": 4, "h": 4,
			"rot": 0, "asset": info.Hash, "natural": []int{4, 4}}}); a["t"] != "ack" {
			t.Fatalf("member image %v", a)
		}
	}
	b.state(func(st *engine.State) {
		alive, assets := st.Counts()
		if alive != 3 || assets != 3 {
			t.Fatalf("board holds %d elements and %d assets", alive, assets)
		}
		l := st.Limits()
		check := func(r guestRoom) string {
			r.created, r.createdTotal = st.CreatedCount(""), st.GuestCreated()
			detail, err := b.in.checkRoom(nil, &r, engine.Changed{})
			if err != nil {
				t.Fatal(err)
			}
			return detail
		}

		l.Assets, l.Elements = 3, 100 // the guests' share of assets is 2
		st.SetLimits(l)
		if d := check(guestRoom{alive: alive, assets: assets - 1}); d == "" {
			t.Fatal("a batch adding an asset above the guests' share was allowed")
		}
		if d := check(guestRoom{alive: alive, assets: assets}); d != "" {
			t.Fatalf("a batch adding no asset was refused: %s", d)
		}
		l.Assets = 4 // the guests' share is 3
		st.SetLimits(l)
		if d := check(guestRoom{alive: alive, assets: assets - 1}); d != "" {
			t.Fatalf("a batch reaching the guests' share of assets was refused: %s", d)
		}

		l.Elements = 3 // the guests' share of elements is 2
		st.SetLimits(l)
		if d := check(guestRoom{alive: alive - 1, assets: assets}); d == "" {
			t.Fatal("a batch adding an element above the guests' share was allowed")
		}
		if d := check(guestRoom{alive: alive, assets: assets}); d != "" {
			t.Fatalf("a batch adding no element was refused: %s", d)
		}
	})
}

// TestResyncKeepsRepliesAndCharges: a resync drops the queued deltas and
// returns their charges, keeps other frames in place and moves acks and
// rejects behind the snapshot, so a pending batch still settles. Every
// charge returns once the queue is dropped.
func TestResyncKeepsRepliesAndCharges(t *testing.T) {
	b := editTB(t)
	queued, snaps, share := publicQueued.Load(), publicSnaps.Load(), guestShare.Load()
	c := &conn{in: b.in, ws: stalledWS(t), public: true, guest: true, chargeGuest: true, wake: make(chan struct{}, 1)}
	pres := []byte(`{"t":"pres","key":"p:1"}`)
	delta := []byte(`{"t":"delta","gen":0,"seq":5,"changes":[]}`)
	ack := []byte(`{"t":"ack","n":1,"seq":5}`)
	if !c.enqueueAs(frameDelta, delta) || !c.enqueue(pres) || !c.enqueueAs(frameReply, ack) || !c.enqueueAs(frameDelta, delta) {
		t.Fatal("a frame was refused")
	}
	var seq int64
	b.state(func(st *engine.State) {
		seq = st.Seq()
		c.resync()
	})
	c.mu.Lock()
	q := slices.Clone(c.q)
	qBytes, nsnaps, gen := c.qBytes, c.snaps, c.gen
	c.mu.Unlock()
	if len(q) != 4 || !bytes.Equal(q[0].frame, pres) || q[2].snap == nil || !bytes.Equal(q[3].frame, ack) {
		t.Fatalf("queue after resync: %+v", q)
	}
	want := fmt.Sprintf(`{"gen":%d,"seq":%d,"t":"resync"}`, gen, seq)
	if gen != 1 || string(q[1].frame) != want || q[2].snap.gen != gen || nsnaps != 1 {
		t.Fatalf("resync frame %s gen %d snaps %d", q[1].frame, gen, nsnaps)
	}
	held := int64(len(pres) + len(want) + len(ack))
	if qBytes != held || publicQueued.Load()-queued != held {
		t.Fatalf("queue holds %d bytes, charged %d, want %d", qBytes, publicQueued.Load()-queued, held)
	}
	cost := q[2].snap.data.cost
	if publicSnaps.Load()-snaps != cost || guestShare.Load()-share != held+cost {
		t.Fatalf("snapshot state charged %d, guests' share %d", publicSnaps.Load()-snaps, guestShare.Load()-share)
	}
	c.dropQueue()
	if publicQueued.Load() != queued || publicSnaps.Load() != snaps || guestShare.Load() != share {
		t.Fatal("charges remain after the queue was dropped")
	}
}

// TestResyncLeavesClosingSocket: a socket closing after its queue, for
// example after a fatal frame, gets no resync, so no reply moves behind
// the fatal frame that ends it, and takes no snapshot state, so a full
// budget does not close it before its queue drains.
func TestResyncLeavesClosingSocket(t *testing.T) {
	b := editTB(t)
	queued, snaps, share := publicQueued.Load(), publicSnaps.Load(), guestShare.Load()
	c := &conn{in: b.in, ws: stalledWS(t), public: true, guest: true, chargeGuest: true, wake: make(chan struct{}, 1)}
	delta := []byte(`{"t":"delta","gen":0,"seq":5,"changes":[]}`)
	ack := []byte(`{"t":"ack","n":1,"seq":5}`)
	if !c.enqueueDelta(5, [][]byte{delta}) || !c.enqueueAs(frameReply, ack) {
		t.Fatal("a frame was refused")
	}
	c.fatal(FatalEditingEnded)
	c.mu.Lock()
	before := slices.Clone(c.q)
	c.mu.Unlock()
	// No snapshot state would fit: a resync that took one would close
	// the socket before its queue drained.
	defer func(v int64) { hostPublicCap = v }(hostPublicCap)
	hostPublicCap = 0
	b.state(func(*engine.State) { c.resync() })
	if code, _ := c.ws.CloseStatus(); code != 0 {
		t.Fatalf("resync closed a closing socket with %d", code)
	}
	c.mu.Lock()
	after, gen := slices.Clone(c.q), c.gen
	c.mu.Unlock()
	if gen != 0 || len(after) != len(before) || !bytes.Equal(after[1].frame, ack) || !bytes.Contains(after[2].frame, []byte(FatalEditingEnded)) {
		t.Fatalf("resync changed a closing socket's queue: gen %d, %d frames", gen, len(after))
	}
	if publicSnaps.Load() != snaps {
		t.Fatal("a snapshot state stays charged")
	}
	c.dropQueue()
	if publicQueued.Load() != queued || guestShare.Load() != share {
		t.Fatal("charges remain after the queue was dropped")
	}
}

// TestFatalAndResyncInEitherOrder: with no room for snapshot state, a
// queued fatal frame keeps a resync from closing the socket or moving a
// reply behind it, and a resync alone closes it (1013). The fatal frame
// and the closing mark are one step, and resync decides and rebuilds the
// queue in one step, so no order can come in between; the last part runs
// both at once for the race detector.
func TestFatalAndResyncInEitherOrder(t *testing.T) {
	b := editTB(t)
	ack := []byte(`{"t":"ack","n":1,"seq":5}`)
	setup := func() *conn {
		c := &conn{in: b.in, ws: stalledWS(t), public: true, guest: true, chargeGuest: true, wake: make(chan struct{}, 1)}
		if !c.enqueueDelta(5, [][]byte{[]byte(`{"t":"delta","gen":0,"seq":5,"changes":[]}`)}) || !c.enqueueAs(frameReply, ack) {
			t.Fatal("a frame was refused")
		}
		return c
	}
	// Room for frames, none for a snapshot state.
	defer func(v int64) { hostPublicCap = v }(hostPublicCap)
	hostPublicCap = publicQueued.Load() + publicSnaps.Load() + publicBudget.Used() + 512
	order := func(c *conn) (ackAt, fatalAt int) {
		c.mu.Lock()
		defer c.mu.Unlock()
		ackAt, fatalAt = -1, -1
		for i, m := range c.q {
			switch {
			case bytes.Equal(m.frame, ack):
				ackAt = i
			case bytes.Contains(m.frame, []byte(`"fatal"`)):
				fatalAt = i
			}
		}
		return ackAt, fatalAt
	}

	c := setup()
	c.fatal(engine.FatalProtocol)
	b.state(func(*engine.State) { c.resync() })
	if code, _ := c.ws.CloseStatus(); code != 0 {
		t.Fatalf("fatal then resync: closed with %d", code)
	}
	if ackAt, fatalAt := order(c); ackAt < 0 || fatalAt < ackAt {
		t.Fatalf("fatal then resync: ack at %d, fatal at %d", ackAt, fatalAt)
	}
	c.dropQueue()

	c = setup()
	b.state(func(*engine.State) { c.resync() })
	if code, _ := c.ws.CloseStatus(); code != 1013 {
		t.Fatalf("resync without room: closed with %d", code)
	}
	c.dropQueue()

	hostPublicCap = hostPublicMax
	for range 100 {
		c := setup()
		done := make(chan struct{})
		go func() { c.fatal(engine.FatalProtocol); close(done) }()
		b.state(func(*engine.State) { c.resync() })
		<-done
		ackAt, fatalAt := order(c)
		if ackAt < 0 || fatalAt < ackAt {
			t.Fatalf("at once: ack at %d, fatal at %d", ackAt, fatalAt)
		}
		c.dropQueue()
	}
}

// TestResyncMarksMissedReset: a resync's snapshot carries reset when a
// reset happened after what the socket was handed, and not otherwise.
func TestResyncMarksMissedReset(t *testing.T) {
	ctx := context.Background()
	b := editTB(t)
	c := &conn{in: b.in, ws: stalledWS(t), public: true, guest: true, chargeGuest: true, wake: make(chan struct{}, 1)}
	t.Cleanup(c.dropQueue)
	var seq int64
	b.state(func(st *engine.State) { seq = st.Seq() })
	c.setHanded(seq)
	resync := func() *snapJob {
		var job *snapJob
		b.state(func(*engine.State) {
			c.resync()
			c.mu.Lock()
			for _, m := range c.q {
				if m.snap != nil {
					job = m.snap
				}
			}
			c.mu.Unlock()
		})
		if job == nil {
			t.Fatal("no snapshot queued")
		}
		return job
	}
	if resync().reset {
		t.Fatal("a resync without a reset is marked reset")
	}
	data := buildArchive(t, boardFile{Format: 1, Title: "New"}, []model.Element{{ID: eid(), Type: model.TypeSticky, Z: model.Rekey(1)[0],
		Geom: model.Geom{W: 200, H: 200}, Style: model.DefaultStyle(model.TypeSticky)}}, nil, nil)
	err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.ImportBoard(ctx, ImportRequest{OpID: "reset-import-1", Mode: "replace", Archive: bytes.NewReader(data), Size: int64(len(data)), Label: "Agent"}, other)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resync().reset {
		t.Fatal("a resync after a replacement the socket was not handed is not marked reset")
	}
	b.state(func(st *engine.State) { c.setHanded(st.Seq()) })
	c.dropQueue()
	if resync().reset {
		t.Fatal("a resync after a handed replacement is marked reset")
	}
}

// TestPendingBatchSettlesAcrossResync: a guest whose socket the server
// cannot write to has the answer to its batch in flight still queued on
// the server when a large import resyncs it. Like the web client, the test client sends its
// pending batches one at a time, the next only after the answer to the
// last. After the resync and the snapshot, which holds the acked change,
// the answer arrives and every pending batch settles, for an ack and for a
// reject.
func TestPendingBatchSettlesAcrossResync(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first map[string]any
		want  string
	}{
		{"ack", sticky("e_pending001", "mine", 0), "ack"},
		{"reject", map[string]any{"op": "nonsense"}, "reject"},
	} {
		t.Run(tc.name, func(t *testing.T) { pendingAcrossResync(t, tc.first, tc.want) })
	}
}

func pendingAcrossResync(t *testing.T, first map[string]any, want string) {
	ctx := context.Background()
	b := editTB(t)
	m := connect(t, b.priv, map[string]any{"editor": "mia", "name": "Mia"}, nil)
	m.snapshot()
	// The guest connects through a listener whose writes to it the test
	// can hold, so what the server queues for it stays queued.
	gate := &writeGate{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("X-Flats-Access")
		r.Header.Del("Tailscale-User-Login")
		r.Header.Set(PublicAccessHeader, AccessEdit)
		b.in.ServeHTTP(w, r)
	}))
	srv.Listener = gatedListener{Listener: srv.Listener, gate: gate}
	srv.Start()
	t.Cleanup(func() {
		gate.release()
		srv.Close()
	})
	g, err := dial(t, srv, origin(srv))
	if err != nil {
		t.Fatal(err)
	}
	g.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "guest": map[string]any{"name": "Gia"}})
	g.session = g.expect("welcome")["session"].(string)
	g.snapshot()
	var sc *conn
	for _, c := range b.in.hub.list() {
		if c.guest {
			sc = c
		}
	}
	queued := func(kind frameKind) bool {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		return slices.ContainsFunc(sc.q, func(m outMsg) bool { return m.snap == nil && m.kind == kind })
	}

	gate.hold()
	// Member edits fill the guest's socket until its pump waits: the socket
	// holds a guest's feed limit unwritten, which cannot drain while the
	// writes are held, and a delta is queued behind it.
	for i := 1; sc.ws.Budget().Used() < guestFeedBelow || !queued(frameDelta); i++ {
		if i > 200 {
			t.Fatal("the guest's queue never filled")
		}
		ops := make([]map[string]any, 16)
		for k := range ops {
			ops[k] = sticky(eid(), noise(3900, i*16+k), float64(i))
		}
		if a := m.settle(i, ops...); a["t"] != "ack" {
			t.Fatalf("member batch %v", a)
		}
	}
	pending := [][]map[string]any{{first}, {sticky(eid(), "second", 0)}, {sticky(eid(), "third", 0)}}
	n := 1
	g.send(map[string]any{"t": "ops", "n": n, "ops": pending[0]})
	waitFor(t, func() bool { return queued(frameReply) })

	els := []model.Element{}
	keys := model.Rekey(1200)
	for i := range keys {
		els = append(els, model.Element{ID: eid(), Type: model.TypeSticky, Z: keys[i], Text: noise(1000, i),
			Geom: model.Geom{X: float64(i) * 210, W: 200, H: 200}, Style: model.DefaultStyle(model.TypeSticky)})
	}
	data := buildArchive(t, boardFile{Format: 1, Title: "Big"}, els, nil, nil)
	err = b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.ImportBoard(ctx, ImportRequest{OpID: "pending-import-1", Mode: "add", Archive: bytes.NewReader(data), Size: int64(len(data)), Label: "Agent"}, other)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	gate.release()

	var order []string
	var answers []string
	inSnapshot := false
	for len(pending) > 0 {
		x, ok := g.next(func(x map[string]any) bool {
			return x["t"] == "resync" || x["t"] == "snapshot" || x["t"] == "ack" || x["t"] == "reject"
		})
		if !ok {
			t.Fatalf("guest stalled after %v with %d batches pending; closed %v", order, len(pending), g.closed(0))
		}
		switch x["t"] {
		case "snapshot":
			for _, e := range x["elements"].([]any) {
				inSnapshot = inSnapshot || e.(map[string]any)["id"] == "e_pending001"
			}
			if x["part"] == x["parts"] {
				order = append(order, "snapshot")
			}
		case "resync":
			order = append(order, "resync")
		default:
			if int(x["n"].(float64)) != n {
				t.Fatalf("answer %v while batch %d is in flight", x, n)
			}
			if len(answers) == 0 {
				order = append(order, x["t"].(string))
			}
			answers = append(answers, x["t"].(string))
			pending = pending[1:]
			if len(pending) > 0 {
				n++
				g.send(map[string]any{"t": "ops", "n": n, "ops": pending[0]})
			}
		}
	}
	if wantOrder := []string{"resync", "snapshot", want}; !slices.Equal(order, wantOrder) {
		t.Fatalf("guest received %v, want %v", order, wantOrder)
	}
	if wantAnswers := []string{want, "ack", "ack"}; !slices.Equal(answers, wantAnswers) {
		t.Fatalf("answers %v, want %v", answers, wantAnswers)
	}
	if inSnapshot != (want == "ack") {
		t.Fatalf("the acked element in the snapshot: %v", inSnapshot)
	}
}

// writeGate holds writes through a gatedListener's connections while held.
type writeGate struct{ ch atomic.Pointer[chan struct{}] }

func (g *writeGate) hold() {
	ch := make(chan struct{})
	g.ch.Store(&ch)
}

func (g *writeGate) release() {
	if ch := g.ch.Swap(nil); ch != nil {
		close(*ch)
	}
}

type gatedListener struct {
	net.Listener
	gate *writeGate
}

func (l gatedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &gatedConn{Conn: c, gate: l.gate, closed: make(chan struct{})}, nil
}

type gatedConn struct {
	net.Conn
	gate   *writeGate
	closed chan struct{}
	once   sync.Once
}

func (c *gatedConn) Write(p []byte) (int, error) {
	if ch := c.gate.ch.Load(); ch != nil {
		select {
		case <-*ch:
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
	return c.Conn.Write(p)
}

func (c *gatedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
