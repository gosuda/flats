package board

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

// TestFiftyGuestsAtTheirLimit runs the Limits criterion of
// docs/spec/board-public-edit.md for as long as BOARD_LOAD says (for
// example BOARD_LOAD=1m; skipped without it): 50 guests, the most a board
// admits, each send a batch of 4 operations on their own stickies every
// 100 ms (the per-guest batch limit of TECH public edit 4.1, with one batch
// in flight and a refused batch sent again at the next tick, without
// backoff) and presence at 10 per second, while one member adds a sticky
// twice per second. The board's limit for all guests together (200
// operations per second) refuses most guest batches as guest_rate; the
// accepted ones still keep the board's apply loop busy.
//
// For each acknowledged member edit it measures the time from the edit's
// first send to another member and to one of the guests receiving the
// change on its connection, and requires a p95 of 200 ms or less for both.
// Receipt on the client connection stands in for display on screen (a
// decision of the owner; client apply and paint are not measured). Every
// acknowledged member edit must reach both observers, the member must keep
// its schedule, guests must keep offering their limit, and no batch may be
// refused for good. Midway, while the guests keep sending, a member
// connects (timed to its first complete snapshot), adds a sticky, the
// sticky is recorded in a revision crediting that member, the member
// deletes it, and a rollback to that revision brings it back.
//
// Only admission speed differs from the product limits: the 50 guests join
// at once instead of 30 a minute, so the measurement starts with all of
// them.
func TestFiftyGuestsAtTheirLimit(t *testing.T) {
	v := os.Getenv("BOARD_LOAD")
	if v == "" {
		t.Skip("set BOARD_LOAD to a duration, for example BOARD_LOAD=1m, to run")
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 10*time.Second {
		t.Fatalf("BOARD_LOAD=%q is not a duration of at least 10s", v)
	}
	const guests = 50
	b := editTB(t)
	b.core.setQuota(1 << 30)
	b.limits(func(p *publicLimits) { p.guestWelcomes = newBucket(0.5, guests) })

	member := dialRaw(t, b.priv.URL, map[string]any{"editor": "load-member"}, "", "welcome")
	observer := dialRaw(t, b.priv.URL, map[string]any{"editor": "load-observer"}, "", "welcome")
	gs := make([]*rawSocket, guests)
	for i := range gs {
		gs[i] = dialRaw(t, b.pub.URL, map[string]any{"guest": map[string]any{"name": fmt.Sprintf("Guest %d", i)}}, `"access":"guest"`, "welcome")
	}
	if g, _ := b.in.PublicPresence(); g != guests {
		t.Fatalf("%d guests connected, want %d", g, guests)
	}

	start := time.Now()
	until := start.Add(d)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	stats := make([]guestStats, guests)
	for i, g := range gs {
		wg.Go(func() { stats[i] = g.runGuest(i, until) })
	}

	// The member: one sticky every 500 ms, one batch in flight.
	sent := sync.Map{}  // id → time.Time of the batch's first send
	acked := sync.Map{} // ids of acknowledged member edits
	var memberErr atomic.Value
	var memberBatches, memberSkipped, memberRetries atomic.Int64
	wg.Go(func() {
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		n, inflight := int64(0), false
		var id string
		var at time.Time
		for {
			select {
			case <-tick.C:
				if inflight {
					if time.Now().Before(until) {
						memberSkipped.Add(1)
					}
					continue
				}
				if !time.Now().Before(until) {
					return
				}
				n++
				id = fmt.Sprintf("e_lm%08d", n)
				at = time.Now()
				sent.Store(id, at)
				member.sendJSON(map[string]any{"t": "ops", "n": n, "ops": []any{sticky(id, "member "+id, float64(n%40)*220)}})
				inflight = true
			case a := <-member.answers:
				if !inflight {
					continue
				}
				if a.retry {
					// Send the same batch again at once; latency still counts
					// from the first send.
					memberRetries.Add(1)
					member.sendJSON(map[string]any{"t": "ops", "n": n, "ops": []any{sticky(id, "member "+id, float64(n%40)*220)}})
					continue
				}
				if !a.ack {
					memberErr.Store(fmt.Errorf("member batch %d refused: %s", n, a.raw))
					return
				}
				inflight = false
				acked.Store(id, true)
				memberBatches.Add(1)
			case <-stop:
				return
			}
		}
	})

	// Midway, while the guests keep sending: a member connects, edits, the
	// edit is saved as a revision, and the board is rolled back.
	var midway string
	var midErr error
	wg.Go(func() {
		select {
		case <-time.After(d / 2):
		case <-stop:
			return
		}
		midway, midErr = privateEditorDuringLoad(t, b)
	})

	wg.Wait()
	close(stop)
	if e, _ := memberErr.Load().(error); e != nil {
		t.Fatal(e)
	}
	if midErr != nil {
		t.Fatalf("private editor during the load: %v", midErr)
	}
	// Let the last changes arrive.
	time.Sleep(time.Second)

	var total guestStats
	for i, s := range stats {
		if s.err != nil {
			t.Fatalf("guest %d: %v", i, s.err)
		}
		total.add(s)
	}
	memberLat := latencies(&sent, &acked, observer.seenAt())
	guestLat := latencies(&sent, &acked, gs[0].seenAt())
	secs := d.Seconds()
	t.Logf("%s, %d guests: guests sent %d batches (%.0f/s; %d ops offered), %d acked (%.0f ops/s accepted), %d guest_rate, %d other transient, %d permanent refusals, %d presence frames; member %d batches acked, %d retries, %d ticks skipped while one was in flight; observer resyncs %d, guest-0 resyncs %d",
		d, guests, total.sent, float64(total.sent)/secs, total.sent*guestOpsPerBatch, total.acked, float64(total.acked*guestOpsPerBatch)/secs,
		total.guestRate, total.otherTransient, total.permanent, total.pres, memberBatches.Load(), memberRetries.Load(), memberSkipped.Load(), observer.resyncs.Load(), gs[0].resyncs.Load())
	t.Logf("member edit to another member: %s", memberLat)
	t.Logf("member edit to a guest: %s", guestLat)
	t.Logf("private editor midway: %s", midway)
	// The member kept its schedule of two edits a second, every
	// acknowledged edit reached both observers, and nothing was refused for
	// good.
	if want := int64(2*secs) - 2; memberBatches.Load() < want {
		t.Fatalf("the member had %d edits acknowledged, want at least %d", memberBatches.Load(), want)
	}
	if memberLat.missing > 0 || guestLat.missing > 0 || memberLat.n != int(memberBatches.Load()) {
		t.Fatalf("acknowledged member edits not delivered: %d to the member, %d to the guest (of %d)", memberLat.missing, guestLat.missing, memberBatches.Load())
	}
	if total.permanent > 0 {
		t.Fatalf("%d guest batches were refused for good", total.permanent)
	}
	// The guests kept offering close to their limit of 10 batches a second
	// (one batch in flight, so a slow answer delays the next).
	if want := 0.8 * guests * 10 * secs; float64(total.sent) < want {
		t.Fatalf("guests offered %d batches, want at least %.0f", total.sent, want)
	}
	// The guests were at the board's limit: at least half of 200 operations
	// a second were accepted.
	if accepted := float64(total.acked*guestOpsPerBatch) / secs; accepted < 100 {
		t.Fatalf("guests had only %.0f operations a second accepted; the load did not reach the board's guest limit", accepted)
	}
	if memberLat.p95 > 200*time.Millisecond || guestLat.p95 > 200*time.Millisecond {
		t.Fatalf("remote display p95 over 200 ms: to a member %s, to a guest %s", memberLat.p95, guestLat.p95)
	}
}

const guestOpsPerBatch = 4

// privateEditorDuringLoad connects a new member and waits for its first
// complete snapshot, adds a sticky (its ack is the board's Saved), records
// and imports a revision that must hold that sticky and credit the member,
// deletes the sticky, rolls the board back to that revision and checks the
// sticky is back with its text. It returns the time each step took.
func privateEditorDuringLoad(t *testing.T, b *tb) (string, error) {
	ctx := context.Background()
	t0 := time.Now()
	c, err := dialRawE(b.priv.URL, map[string]any{"editor": "late-member", "name": "Late Member"}, "", "welcome")
	if err != nil {
		return "", fmt.Errorf("connect: %w", err)
	}
	defer c.ws.Close(1000, "")
	select {
	case <-c.snapped:
	case <-time.After(10 * time.Second):
		return "", fmt.Errorf("no complete snapshot within 10 s")
	}
	connected := time.Since(t0)
	send := func(n int, op map[string]any) (time.Duration, error) {
		start := time.Now()
		for range 50 {
			c.sendJSON(map[string]any{"t": "ops", "n": n, "ops": []any{op}})
			select {
			case a := <-c.answers:
				if a.retry {
					time.Sleep(20 * time.Millisecond)
					continue
				}
				var res struct{ Results []any }
				_ = json.Unmarshal([]byte(a.raw), &res)
				if !a.ack || len(res.Results) != 1 || !applied(res.Results[0]) {
					return 0, fmt.Errorf("batch %d not applied: %s", n, a.raw)
				}
				return time.Since(start), nil
			case <-time.After(5 * time.Second):
				return 0, fmt.Errorf("no answer to batch %d", n)
			}
		}
		return 0, fmt.Errorf("batch %d kept being refused", n)
	}
	const id, text = "e_latemem001", "late member note"
	edited, err := send(1, sticky(id, text, 0))
	if err != nil {
		return "", err
	}
	// Saved as a revision: the member's group closes and core imports it.
	t2 := time.Now()
	revs := b.core.lastRev()
	b.clock.Add(31 * time.Second)
	if err := b.in.do(ctx, b.in.tick); err != nil {
		return "", fmt.Errorf("tick: %w", err)
	}
	if err := b.core.importOutbox(ctx); err != nil {
		return "", fmt.Errorf("import: %w", err)
	}
	saved := time.Since(t2)
	var rev int
	var info RevisionInfo
	for n := b.core.lastRev(); n > revs; n-- {
		ri, content, err := b.core.RevisionContent(ctx, n)
		if err != nil {
			return "", err
		}
		page, err := ReadContent(content, Query{IDs: []string{id}})
		if err == nil && len(page.Elements) == 1 && slices.Contains(ri.Authors, "Late Member") {
			rev, info = n, ri
			break
		}
	}
	if rev == 0 {
		return "", fmt.Errorf("no revision after %d holds the member's sticky and credits it", revs)
	}
	if _, err := send(2, map[string]any{"op": "delete", "ids": []string{id}}); err != nil {
		return "", err
	}
	present := func() (bool, error) {
		page, err := b.in.GetBoard(ctx, Query{IDs: []string{id}})
		if err != nil {
			return false, err
		}
		if len(page.Elements) == 0 {
			return false, nil
		}
		var el map[string]any
		if err := json.Unmarshal(page.Elements[0], &el); err != nil {
			return false, err
		}
		return el["text"] == text, nil
	}
	if ok, err := present(); err != nil || ok {
		return "", fmt.Errorf("the sticky is still there after its delete (%v)", err)
	}
	t3 := time.Now()
	n, err := b.core.Rollback(ctx, rev, "Late member")
	if err != nil {
		return "", fmt.Errorf("rollback: %w", err)
	}
	rolled := time.Since(t3)
	if ok, err := present(); err != nil || !ok {
		return "", fmt.Errorf("rollback to revision %d did not bring the sticky back (%v)", rev, err)
	}
	return fmt.Sprintf("connect to first complete snapshot %s, edit acked %s, revision %d recorded and imported %s (authors %v), rollback to %d as revision %d %s, sticky restored",
		connected.Round(time.Millisecond), edited.Round(time.Millisecond), rev, saved.Round(time.Millisecond), info.Authors, rev, n, rolled.Round(time.Millisecond)), nil
}

// rawSocket is a light WebSocket client for the load: it decodes only
// acks, refusals and fatals, and notes when each member sticky id first
// appears in a delta or snapshot.
type rawSocket struct {
	ws      *wsconn.Conn
	answers chan answer
	mu      sync.Mutex
	seen    map[string]time.Time
	resyncs atomic.Int64
	fatal   atomic.Value
	welcome chan string
	// snapped is closed when the first complete snapshot has arrived.
	snapped  chan struct{}
	snapOnce sync.Once
}

type answer struct {
	ack, retry bool
	code       string
	raw        string
}

var memberID = regexp.MustCompile(`e_lm[0-9]{8}`)

func dialRaw(t *testing.T, base string, hello map[string]any, want, wantT string) *rawSocket {
	t.Helper()
	c, err := dialRawE(base, hello, want, wantT)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.ws.Close(1000, "") })
	return c
}

func dialRawE(base string, hello map[string]any, want, wantT string) (*rawSocket, error) {
	host := strings.TrimPrefix(base, "http://")
	ws, err := wsconn.Dial(context.Background(), func(ctx context.Context) (net.Conn, error) {
		c, err := net.Dial("tcp", host)
		if err != nil {
			return nil, err
		}
		return &headerConn{Conn: c, extra: "Origin: " + base + "\r\n"}, nil
	}, host, "/_board/ws")
	if err != nil {
		return nil, err
	}
	c := &rawSocket{ws: ws, answers: make(chan answer, 64), seen: map[string]time.Time{}, welcome: make(chan string, 1), snapped: make(chan struct{})}
	go c.read()
	h := map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}}
	for k, v := range hello {
		h[k] = v
	}
	c.sendJSON(h)
	select {
	case w := <-c.welcome:
		if !strings.Contains(w, `"t":"`+wantT+`"`) || (want != "" && !strings.Contains(w, want)) {
			ws.Close(1000, "")
			return nil, fmt.Errorf("hello answered %s", w)
		}
	case <-time.After(10 * time.Second):
		ws.Close(1000, "")
		return nil, fmt.Errorf("no welcome")
	}
	return c, nil
}

func (c *rawSocket) sendJSON(v any) {
	b, _ := json.Marshal(v)
	_ = c.ws.WriteFrame(wsconn.OpText, b)
}

func (c *rawSocket) read() {
	defer close(c.answers)
	first := true
	for {
		raw, err := c.ws.ReadMessage(64 << 20)
		if err != nil {
			return
		}
		now := time.Now()
		if first {
			first = false
			c.welcome <- raw
			continue
		}
		switch {
		case strings.Contains(raw, `"t":"delta`) || strings.Contains(raw, `"t":"snapshot"`):
			if strings.Contains(raw, `"t":"snapshot"`) {
				var p struct{ Part, Parts int }
				if json.Unmarshal([]byte(raw), &p) == nil && p.Part == p.Parts {
					c.snapOnce.Do(func() { close(c.snapped) })
				}
			}
			if ids := memberID.FindAllString(raw, -1); len(ids) > 0 {
				c.mu.Lock()
				for _, id := range ids {
					if _, ok := c.seen[id]; !ok {
						c.seen[id] = now
					}
				}
				c.mu.Unlock()
			}
		case strings.Contains(raw, `"t":"ack"`) || strings.Contains(raw, `"t":"reject"`):
			var m struct {
				T     string `json:"t"`
				Retry bool   `json:"retry"`
				Code  string `json:"code"`
			}
			_ = json.Unmarshal([]byte(raw), &m)
			c.answers <- answer{ack: m.T == "ack", retry: m.Retry, code: m.Code, raw: raw}
		case strings.Contains(raw, `"t":"resync"`):
			c.resyncs.Add(1)
		case strings.Contains(raw, `"t":"fatal"`):
			c.fatal.Store(raw)
		}
	}
}

func (c *rawSocket) seenAt() map[string]time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]time.Time, len(c.seen))
	for k, v := range c.seen {
		out[k] = v
	}
	return out
}

type guestStats struct {
	sent, acked, guestRate, otherTransient, permanent, pres int
	err                                                     error
}

func (s *guestStats) add(o guestStats) {
	s.sent += o.sent
	s.acked += o.acked
	s.guestRate += o.guestRate
	s.otherTransient += o.otherTransient
	s.permanent += o.permanent
	s.pres += o.pres
}

// runGuest sends at the per-guest limit until the deadline: at every 100 ms
// tick a presence frame and, when no batch is in flight, the next batch or
// the refused one again. The first batch adds the guest's 3 stickies; the
// others change their text and position (inc 0 resolves to the guest's own
// created rows).
func (c *rawSocket) runGuest(i int, until time.Time) guestStats {
	var s guestStats
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	ids := []string{fmt.Sprintf("e_g%03d%06d", i, 1), fmt.Sprintf("e_g%03d%06d", i, 2), fmt.Sprintf("e_g%03d%06d", i, 3)}
	n, inflight, resend := int64(0), false, false
	var body []byte
	k := 0
	for {
		select {
		case <-tick.C:
			if f, _ := c.fatal.Load().(string); f != "" {
				s.err = fmt.Errorf("fatal %s", f)
				return s
			}
			if !time.Now().Before(until) {
				if !inflight {
					return s
				}
				continue
			}
			c.sendJSON(map[string]any{"t": "pres", "c": []float64{float64(k % 500), float64(i * 10)}})
			s.pres++
			if inflight {
				continue
			}
			if !resend {
				n++
				k++
				var ops []any
				if n == 1 {
					for j, id := range ids {
						ops = append(ops, sticky(id, "guest", float64(i*700+j*220)))
					}
					ops = append(ops, map[string]any{"op": "set", "id": ids[0], "inc": 0, "g": "text", "val": "hello"})
				} else {
					a, bb := ids[k%3], ids[(k+1)%3]
					ops = []any{
						map[string]any{"op": "set", "id": a, "inc": 0, "g": "text", "val": fmt.Sprintf("g%d-%d", i, k)},
						map[string]any{"op": "set", "id": bb, "inc": 0, "g": "text", "val": fmt.Sprintf("g%d-%d", i, k)},
						map[string]any{"op": "set", "id": a, "inc": 0, "g": "geom", "val": map[string]any{"x": float64(i*700 + k%200), "y": float64(1000 + k%100), "w": 200, "h": 200, "rot": 0}},
						map[string]any{"op": "set", "id": bb, "inc": 0, "g": "geom", "val": map[string]any{"x": float64(i*700 + 220 + k%200), "y": float64(1000 + k%100), "w": 200, "h": 200, "rot": 0}},
					}
				}
				body, _ = json.Marshal(map[string]any{"t": "ops", "n": n, "ops": ops})
			}
			if err := c.ws.WriteFrame(wsconn.OpText, body); err != nil {
				s.err = err
				return s
			}
			s.sent++
			inflight, resend = true, false
		case a, ok := <-c.answers:
			if !ok {
				if f, _ := c.fatal.Load().(string); f != "" {
					s.err = fmt.Errorf("fatal %s", f)
				} else {
					s.err = fmt.Errorf("socket closed")
				}
				return s
			}
			inflight = false
			switch {
			case a.ack:
				s.acked++
			case a.retry && a.code == CodeGuestRate:
				s.guestRate++
				resend = true
			case a.retry:
				s.otherTransient++
				resend = true
			default:
				s.permanent++
			}
			if !time.Now().Before(until) {
				return s
			}
		}
	}
}

type latencySummary struct {
	n             int
	missing       int
	p50, p95, max time.Duration
}

func (l latencySummary) String() string {
	return fmt.Sprintf("%d edits, p50 %s, p95 %s, max %s, %d never seen", l.n, l.p50.Round(time.Millisecond), l.p95.Round(time.Millisecond), l.max.Round(time.Millisecond), l.missing)
}

func latencies(sent, acked *sync.Map, seen map[string]time.Time) latencySummary {
	var out latencySummary
	var ds []time.Duration
	sent.Range(func(k, v any) bool {
		if _, ok := acked.Load(k); !ok {
			return true
		}
		at, ok := seen[k.(string)]
		if !ok {
			out.missing++
			return true
		}
		ds = append(ds, at.Sub(v.(time.Time)))
		return true
	})
	slices.Sort(ds)
	out.n = len(ds)
	if len(ds) > 0 {
		out.p50 = ds[len(ds)/2]
		out.p95 = ds[(len(ds)*95+99)/100-1]
		out.max = ds[len(ds)-1]
	}
	return out
}
