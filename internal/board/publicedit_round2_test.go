package board

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
	"github.com/gosuda/flats/internal/wsconn"
)

// frameLog keeps every raw frame a socket received, before any test helper
// consumes it.
type frameLog struct {
	mu     sync.Mutex
	frames []string
}

func (l *frameLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.frames)
}

// dialLogged is dial that also records every received frame in a log.
func dialLogged(t *testing.T, srv *httptest.Server) (*client, *frameLog) {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "http://")
	ws, err := wsconn.Dial(context.Background(), func(ctx context.Context) (net.Conn, error) {
		c, err := net.Dial("tcp", host)
		if err != nil {
			return nil, err
		}
		return &headerConn{Conn: c, extra: "Origin: " + srv.URL + "\r\n"}, nil
	}, host, "/_board/ws")
	if err != nil {
		t.Fatal(err)
	}
	log := &frameLog{}
	c := &client{t: t, ws: ws, msgs: make(chan map[string]any, 4096)}
	go func() {
		defer close(c.msgs)
		for {
			raw, err := ws.ReadMessage(64 << 20)
			if err != nil {
				return
			}
			log.mu.Lock()
			log.frames = append(log.frames, raw)
			log.mu.Unlock()
			var m map[string]any
			if json.Unmarshal([]byte(raw), &m) == nil {
				m["_raw"] = raw
				c.msgs <- m
			}
		}
	}()
	t.Cleanup(func() { ws.Close(1000, "") })
	return c, log
}

// joinLoggedGuest says hello as a guest on a logged socket.
func joinLoggedGuest(t *testing.T, b *tb, name string) (*client, *frameLog) {
	t.Helper()
	c, log := dialLogged(t, b.pub)
	c.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "guest": map[string]any{"name": name}})
	w := c.expect("welcome")
	if w["access"] != "guest" {
		t.Fatalf("guest welcome %v", w)
	}
	c.session = w["session"].(string)
	c.snapshot()
	return c, log
}

// frameType names a frame for the coverage census: its t, with presence
// split into pres and gone.
func frameType(raw string) string {
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return "?"
	}
	t, _ := m["t"].(string)
	if t == "pres" && m["gone"] == true {
		return "gone"
	}
	return t
}

// privateIdentity lists what must never reach a guest or viewer: member
// keys, a Tailscale login and name, members' typed names and editor ids,
// and an agent label.
var privateIdentity = []string{"ts:", "browser:", "agent:", "alice@example.com", "Alice Secret", "Bob Hidden", "bobby", "SecretBot"}

func scanIdentity(t *testing.T, who, data string) {
	t.Helper()
	for _, bad := range privateIdentity {
		if strings.Contains(data, bad) {
			t.Fatalf("%s received %q: %.400s", who, bad, data)
		}
	}
}

// TestGuestIdentityEveryFrame is the Identity criterion over every frame a
// guest socket receives, read from the socket before any helper consumes
// it: welcome, every snapshot part, acks and refusals (recorded and
// transient), deltas of member, guest and agent writes, delta parts after a
// replacing import, presence of members and guests, gone, viewers, and the
// editing_ended fatal; a viewer socket's frames including level; and the
// decoded contents of the export downloaded on the Public route.
func TestGuestIdentityEveryFrame(t *testing.T) {
	ctx := context.Background()
	b := newTB(t)
	b.setLevel(AccessView)
	agent := func(op string, ops string) {
		t.Helper()
		err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
			var aops []engine.AgentOp
			if err := json.Unmarshal([]byte(ops), &aops); err != nil {
				return err
			}
			_, err := b.in.UpdateBoard(ctx, UpdateRequest{OpID: op, Label: "SecretBot", Ops: aops}, other)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// A board larger than one snapshot frame, written by an agent, a
	// Tailscale member and a browser member.
	var els []model.Element
	keys := model.Rekey(2500)
	for i := range 2500 {
		els = append(els, model.Element{ID: eid(), Type: model.TypeSticky, Z: keys[i], Text: noise(120, i),
			Geom: model.Geom{X: float64(i%50) * 210, Y: float64(i/50) * 210, W: 200, H: 200}, Style: model.DefaultStyle(model.TypeSticky)})
	}
	data := buildArchive(t, boardFile{Format: 1, Title: "Scan", Grid: true}, els, nil, nil)
	if err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.ImportBoard(ctx, ImportRequest{OpID: "scan-import-1", Mode: "replace", Archive: bytes.NewReader(data), Size: int64(len(data)), Label: "SecretBot"}, other)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	alice := connect(t, b.priv, map[string]any{"editor": "alice", "name": "Alice Secret"},
		map[string]string{"Tailscale-User-Login": "alice@example.com", "Tailscale-User-Name": "Alice Secret"})
	alice.snapshot()
	bob := connect(t, b.priv, map[string]any{"editor": "bobby", "name": "Bob Hidden"}, nil)
	bob.snapshot()
	el := eid()
	alice.settle(1, sticky(el, "member note", 0))
	bob.settle(1, sticky(eid(), "second member note", 300))

	// A viewer joins at view, then the level becomes edit: it gets level.
	v, vlog := dialLogged(t, b.pub)
	v.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}})
	v.expect("welcome")
	b.setLevel(AccessEdit)
	v.expect("level")

	g1, log1 := joinLoggedGuest(t, b, "Gus")
	g2, log2 := joinLoggedGuest(t, b, "Gia")
	bob.send(map[string]any{"t": "pres", "c": []float64{1, 2}})
	alice.send(map[string]any{"t": "pres", "c": []float64{3, 4}, "sel": []string{el}})
	alice.settle(2, map[string]any{"op": "set", "id": el, "inc": 0, "g": "text", "val": "member again"})
	agent("scan-agent-1", `[{"op":"add","el":{"type":"sticky","x":0,"y":-300,"w":100,"h":100,"text":"agent note"}}]`)
	g1.settle(1, sticky(eid(), "guest note", 400))
	g1.send(map[string]any{"t": "pres", "c": []float64{5, 6}})
	g2.batch(1, map[string]any{"op": "bogus"}) // a recorded refusal
	// A transient refusal: the board's guest operations are used up.
	b.limits(func(p *publicLimits) { p.guestOps = newBucket(0.001, 1) })
	if m := g2.batch(2, sticky(eid(), "", 0), sticky(eid(), "", 0)); m["code"] != CodeGuestRate {
		t.Fatalf("expected a transient guest_rate refusal: %v", m)
	}
	b.limits(func(p *publicLimits) { p.guestOps = newBucket(200, 200) })
	g2.settle(2, sticky(eid(), "", 0), sticky(eid(), "", 0))
	// Another viewer, then a member leaves: viewers and gone.
	joinViewer(t, b, "", nil)
	bob.ws.Close(1000, "")
	g2.expectPres(func(m map[string]any) bool { return m["gone"] == true })
	// The export on the Public route, while guests edit.
	resp, err := http.Get(b.pub.URL + "/_board/export.zip")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("export: %v %v", resp, err)
	}
	zipData, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, _ := io.ReadAll(rc)
		rc.Close()
		scanIdentity(t, "the Public export file "+f.Name, string(content))
		if strings.HasSuffix(f.Name, ".json") || strings.Contains(f.Name, "elements") {
			files++
		}
	}
	if files == 0 || !bytes.Contains(zipData, []byte("PK")) {
		t.Fatalf("the export held no board data: %d files", len(zr.File))
	}
	// A replacing import resyncs everyone.
	agent("scan-agent-2", `[{"op":"add","el":{"type":"sticky","x":0,"y":-600,"w":100,"h":100,"text":"agent two"}}]`)
	for i := range els {
		els[i].ID, els[i].Text = eid(), noise(120, 5000+i)
	}
	// About 700 KiB of changes: above one 512 KiB frame, below the 1 MiB
	// queue of a Public socket.
	data2 := buildArchive(t, boardFile{Format: 1, Title: "Scan two", Grid: true}, els[:1300], nil, nil)
	if err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.ImportBoard(ctx, ImportRequest{OpID: "scan-import-2", Mode: "replace", Archive: bytes.NewReader(data2), Size: int64(len(data2)), Label: "SecretBot"}, other)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Its delta is larger than one server frame: guests get delta parts.
	if _, ok := g1.next(func(m map[string]any) bool { return m["t"] == "delta_part" && m["part"] == m["parts"] }); !ok {
		t.Fatal("no delta parts after the replacing import")
	}
	// Stop guest editing: editing_ended.
	b.setLevel(AccessView)
	expectFatal(t, g1, FatalEditingEnded)
	expectFatal(t, g2, FatalEditingEnded)
	for _, c := range []*client{g1, g2, v} {
		c.closed(5 * time.Second)
	}

	seen := map[string]int{}
	snapParts := 0
	for name, log := range map[string]*frameLog{"guest Gus": log1, "guest Gia": log2, "viewer": vlog} {
		for _, raw := range log.all() {
			scanIdentity(t, name, raw)
			if name != "viewer" {
				seen[frameType(raw)]++
				if frameType(raw) == "snapshot" && strings.Contains(raw, `"parts":`) && !strings.Contains(raw, `"parts":1,`) {
					snapParts++
				}
			} else {
				seen["viewer "+frameType(raw)]++
			}
		}
	}
	for _, want := range []string{"welcome", "snapshot", "ack", "reject", "delta", "delta_part", "pres", "gone", "viewers", "fatal", "viewer welcome", "viewer level", "viewer snapshot"} {
		if seen[want] == 0 {
			t.Errorf("no %q frame was scanned (seen %v)", want, seen)
		}
	}
	if snapParts == 0 {
		t.Errorf("no multi-part snapshot reached a guest (seen %v)", seen)
	}
	t.Logf("scanned frames by type: %v; snapshot parts in multi-part snapshots: %d; export files %d", seen, snapParts, len(zr.File))
}

// TestGuestTextAuthorshipAfterMemberMove is the Agent safety criterion: a
// guest writes a sticky's text, a member then moves and restyles it, and
// get_board and get_revision still report the text as guest-written with
// the guest's name, the last change as the member's.
func TestGuestTextAuthorshipAfterMemberMove(t *testing.T) {
	ctx := context.Background()
	b := editTB(t)
	g, _ := joinGuest(t, b, "Robin", "")
	id := eid()
	if a := g.settle(1, sticky(id, "guest wrote this", 0)); a["t"] != "ack" {
		t.Fatalf("guest add %v", a)
	}
	m := connect(t, b.priv, map[string]any{"editor": "mia", "name": "Mia"}, nil)
	m.snapshot()
	var inc int64
	b.state(func(st *engine.State) { r, _ := st.Element(id); inc = r.Inc })
	if a := m.settle(1, map[string]any{"op": "set", "id": id, "inc": inc, "g": "geom", "val": map[string]any{"x": 300, "y": 40, "w": 200, "h": 200, "rot": 0}}); a["t"] != "ack" {
		t.Fatalf("member move %v", a)
	}
	if a := m.settle(2, map[string]any{"op": "set", "id": id, "inc": inc, "g": "style.color", "val": "blue"}); a["t"] != "ack" {
		t.Fatalf("member restyle %v", a)
	}
	check := func(where string, raw json.RawMessage) {
		t.Helper()
		var e map[string]any
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		ta, _ := e["text_authors"].(map[string]any)
		text, _ := ta["text"].(map[string]any)
		if text["kind"] != model.KindGuest || text["name"] != "Robin" || e["last_change"] != model.KindMember {
			t.Fatalf("%s: text authors %v, last change %v (%s)", where, ta, e["last_change"], raw)
		}
		if e["x"] != float64(300) || e["text"] != "guest wrote this" {
			t.Fatalf("%s: the member's move or the guest's text is missing: %s", where, raw)
		}
	}
	page, err := b.in.GetBoard(ctx, Query{IDs: []string{id}})
	if err != nil || len(page.Elements) != 1 {
		t.Fatalf("get_board %v %v", page, err)
	}
	check("get_board", page.Elements[0])
	// Record a revision: the member's and the guests' groups close.
	b.clock.Add(31 * time.Second)
	if err := b.in.do(ctx, b.in.tick); err != nil {
		t.Fatal(err)
	}
	if err := b.in.SetPublicAccess(ctx, AccessView); err != nil {
		t.Fatal(err)
	}
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	_, c, err := b.core.RevisionContent(ctx, b.core.lastRev())
	if err != nil {
		t.Fatal(err)
	}
	rp, err := ReadContent(c, Query{IDs: []string{id}})
	if err != nil || len(rp.Elements) != 1 {
		t.Fatalf("get_revision %v %v", rp, err)
	}
	check("get_revision", rp.Elements[0])
}

// joinUntilFull joins guests until one is refused as full and returns the
// clients admitted.
func joinUntilFull(t *testing.T, b *tb, max int) []*client {
	t.Helper()
	var out []*client
	for i := range max {
		c, err := dial(t, b.pub, origin(b.pub))
		if err != nil {
			t.Fatal(err)
		}
		c.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "guest": map[string]any{"name": fmt.Sprintf("G%d", i)}})
		m, ok := c.next(func(m map[string]any) bool { return m["t"] == "welcome" || m["t"] == "fatal" })
		if !ok {
			t.Fatalf("guest %d got no answer", i)
		}
		if m["t"] == "fatal" {
			if m["code"] != FatalFull {
				t.Fatalf("guest %d refused with %v", i, m)
			}
			return out
		}
		out = append(out, c)
	}
	return out
}

// TestGuestAdmissionCaps: a board admits 30 new guests a minute and 50
// guest sockets at once; over either the visitor is told the board is full,
// and viewers keep being admitted (spec rule 23, TECH public edit 3.1).
func TestGuestAdmissionCaps(t *testing.T) {
	t.Run("30 welcomes a minute", func(t *testing.T) {
		b := editTB(t)
		start := time.Now()
		admitted := joinUntilFull(t, b, 60)
		elapsed := time.Since(start).Seconds()
		// The bucket holds 30 and refills one every two seconds.
		if len(admitted) < 30 || float64(len(admitted)) > 30+elapsed/2+1 {
			t.Fatalf("%d guests admitted in %.1fs at 30 a minute", len(admitted), elapsed)
		}
		if _, w := joinViewer(t, b, "", nil); w["access"] != "public" {
			t.Fatalf("a viewer after the guests were refused: %v", w)
		}
	})
	t.Run("50 guest sockets", func(t *testing.T) {
		b := editTB(t)
		b.limits(func(p *publicLimits) { p.guestWelcomes = newBucket(1000, 1000) })
		admitted := joinUntilFull(t, b, 60)
		if len(admitted) != 50 {
			t.Fatalf("%d guest sockets admitted, want 50", len(admitted))
		}
		if g, _ := b.in.PublicPresence(); g != 50 {
			t.Fatalf("guests on the board %d", g)
		}
		if _, w := joinViewer(t, b, "", nil); w["access"] != "public" {
			t.Fatalf("a viewer with 50 guests: %v", w)
		}
		admitted[0].ws.Close(1000, "")
		waitFor(t, func() bool { g, _ := b.in.PublicPresence(); return g == 49 })
		if again := joinUntilFull(t, b, 2); len(again) != 1 {
			t.Fatalf("after one guest left, %d joined, want 1", len(again))
		}
	})
}

// TestGuestPresenceCaps: one guest's presence is relayed at most 10 a
// second (burst 10), all guests' together at most 200 a second (burst 200)
// (TECH public edit 3.4, 4.2).
func TestGuestPresenceCaps(t *testing.T) {
	t.Run("per guest", func(t *testing.T) {
		b := editTB(t)
		m := connect(t, b.priv, map[string]any{"editor": "mia"}, nil)
		m.snapshot()
		g, _ := joinGuest(t, b, "Gus", "")
		start := time.Now()
		for i := range 60 {
			g.send(map[string]any{"t": "pres", "c": []float64{float64(i), 0}})
		}
		sent := time.Since(start).Seconds()
		got, last := 0, start
		for {
			if _, ok := nextWithin(m, 300*time.Millisecond, func(x map[string]any) bool { return x["t"] == "pres" && x["name"] == "Gus" }); !ok {
				break
			}
			got, last = got+1, time.Now()
		}
		// Tokens refill while the server still reads the frames: the bound
		// runs to the last relayed frame's arrival.
		span := last.Sub(start).Seconds()
		if got < 1 || float64(got) > 10+10*span+1 {
			t.Fatalf("relayed %d of 60 presence frames within %.3fs; the cap is 10 a second, burst 10", got, span)
		}
		t.Logf("one guest: %d of 60 relayed (sent in %.3fs, last relayed after %.3fs)", got, sent, span)
	})
	t.Run("all guests", func(t *testing.T) {
		b := editTB(t)
		b.limits(func(p *publicLimits) { p.guestWelcomes = newBucket(1000, 1000) })
		m := connect(t, b.priv, map[string]any{"editor": "mia"}, nil)
		m.snapshot()
		var gs []*client
		for i := range 40 {
			g, _ := joinGuest(t, b, fmt.Sprintf("G%d", i), "")
			gs = append(gs, g)
		}
		time.Sleep(1100 * time.Millisecond) // each guest's own bucket refills
		// Drop what the joins sent the member.
		for {
			if _, ok := nextWithin(m, 300*time.Millisecond, func(map[string]any) bool { return true }); !ok {
				break
			}
		}
		var wg sync.WaitGroup
		start := time.Now()
		for i, g := range gs {
			wg.Go(func() {
				for k := range 10 {
					b, _ := json.Marshal(map[string]any{"t": "pres", "c": []float64{float64(i), float64(k)}})
					_ = g.ws.WriteFrame(wsconn.OpText, b)
				}
			})
		}
		wg.Wait()
		sent := time.Since(start).Seconds()
		got, last := 0, start
		names := map[string]bool{}
		for {
			x, ok := nextWithin(m, 300*time.Millisecond, func(x map[string]any) bool { return x["t"] == "pres" && x["guest"] == true && x["gone"] != true })
			if !ok {
				break
			}
			names[x["name"].(string)] = true
			got, last = got+1, time.Now()
		}
		// Tokens refill while the server still reads the frames: the bound
		// runs to the last relayed frame's arrival.
		span := last.Sub(start).Seconds()
		if float64(got) > 200+200*span+1 {
			t.Fatalf("relayed %d of 400 guest presence frames sent in %.3fs; the cap is 200 a second, burst 200", got, sent)
		}
		if got < 20 {
			t.Fatalf("only %d guest presence frames relayed", got)
		}
		t.Logf("40 guests: %d of 400 relayed from %d guests (sent in %.3fs, last relayed after %.3fs)", got, len(names), sent, span)
	})
}

// nextWithin is client.next with its own timeout.
func nextWithin(c *client, d time.Duration, pred func(map[string]any) bool) (map[string]any, bool) {
	for i, m := range c.backlog {
		if pred(m) {
			c.backlog = slices.Delete(c.backlog, i, i+1)
			return m, true
		}
	}
	timeout := time.After(d)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				return nil, false
			}
			if pred(m) {
				return m, true
			}
			c.backlog = append(c.backlog, m)
		case <-timeout:
			return nil, false
		}
	}
}

// TestGuestAssetRoom: with referenced assets above the guests' share of
// the asset limit, a guest still copies a live image (it adds no asset),
// can add no new asset, and a member adds a new image in the reserved
// room. Guests can only reference assets that live elements reference
// already (spec rule 19), so no guest batch can raise the number of
// referenced assets: the asset room check has no guest path that reaches it.
func TestGuestAssetRoom(t *testing.T) {
	ctx := context.Background()
	b := editTB(t)
	var hashes []string
	for i := range 3 {
		info, err := b.in.AddAsset(ctx, pngBytes(t, 4+i, 4))
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, info.Hash)
	}
	image := func(id, hash string) map[string]any {
		return map[string]any{"op": "add", "el": map[string]any{"id": id, "type": "image", "x": 0, "y": 0, "w": 4, "h": 4, "rot": 0,
			"asset": hash, "natural": []int{4, 4}}}
	}
	b.state(func(st *engine.State) {
		l := st.Limits()
		l.Assets = 3 // the guests' share is 90%: 2
		st.SetLimits(l)
	})
	m := connect(t, b.priv, map[string]any{"editor": "mia"}, nil)
	m.snapshot()
	for i, h := range hashes[:2] {
		if a := m.settle(i+1, image(eid(), h)); a["t"] != "ack" {
			t.Fatalf("member image %v", a)
		}
	}
	assets := func() int {
		var n int
		b.state(func(st *engine.State) { _, n = st.Counts() })
		return n
	}
	if assets() != 2 {
		t.Fatalf("referenced assets %d", assets())
	}
	g, _ := joinGuest(t, b, "Gus", "")
	if a := g.settle(1, image(eid(), hashes[0])); a["t"] != "ack" {
		t.Fatalf("guest copy of a live image at the room line: %v", a)
	}
	if a := g.settle(2, image(eid(), hashes[2])); a["code"] != CodeGuestAsset {
		t.Fatalf("guest add of a new asset: %v", a)
	}
	if assets() != 2 {
		t.Fatalf("a guest changed the referenced assets to %d", assets())
	}
	if a := m.settle(3, image(eid(), hashes[2])); a["t"] != "ack" {
		t.Fatalf("member image in the reserved room: %v", a)
	}
	if assets() != 3 {
		t.Fatalf("referenced assets after the member's image %d", assets())
	}
}

// TestTwentyGuestsHourOfRevisions is the Revisions criterion with 20
// guests: over an hour of fake time, continuously or with pauses of many
// lengths, and with a member's groups expiring in between, guests create
// at most 12 guest-only revisions.
func TestTwentyGuestsHourOfRevisions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active func(step int) bool // whether guests edit in this 10 s step
		member int                 // a member edits every member steps
	}{
		{"continuously", func(int) bool { return true }, 17},
		{"with pauses", func(step int) bool {
			// Edit 10 s, then pause 40 s, 70 s, 300 s, 310 s, 20 s in turn.
			pauses := []int{4, 7, 30, 31, 2}
			s := 0
			for i := 0; ; i++ {
				if step == s {
					return true
				}
				s += 1 + pauses[i%len(pauses)]
				if s > step {
					return false
				}
			}
		}, 97},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b := editTB(t)
			b.limits(func(p *publicLimits) { p.guestWelcomes, p.guestOps = newBucket(1000, 1000), newBucket(1e6, 1e6) })
			var gs []*client
			var ids []string
			for i := range 20 {
				g, _ := joinGuest(t, b, fmt.Sprintf("Guest %d", i), "")
				id := eid()
				if a := g.settle(1, sticky(id, "", float64(i)*210)); a["t"] != "ack" {
					t.Fatalf("guest %d add %v", i, a)
				}
				gs, ids = append(gs, g), append(ids, id)
			}
			m := connect(t, b.priv, map[string]any{"editor": "mia", "name": "Mia"}, nil)
			m.snapshot()
			n := make([]int, 20)
			for i := range n {
				n[i] = 2
			}
			mn := 1
			editing := 0
			for step := 0; step < 360; step++ {
				b.clock.Add(10 * time.Second)
				if tc.active(step) {
					editing++
					// Five of the 20 guests edit in each step, all 20 every
					// four steps.
					for k := range 5 {
						i := (step*5 + k) % 20
						if a := gs[i].settle(n[i], map[string]any{"op": "set", "id": ids[i], "inc": 0, "g": "text", "val": noise(8, step*20+i)}); a["t"] != "ack" {
							t.Fatalf("guest %d at step %d: %v", i, step, a)
						}
						n[i]++
					}
				}
				if step%tc.member == 0 {
					m.settle(mn, sticky(eid(), "", float64(step)))
					mn++
				}
				if err := b.in.do(ctx, b.in.tick); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.core.importOutbox(ctx); err != nil {
				t.Fatal(err)
			}
			guestOnly, credited, member := 0, 0, 0
			b.core.data.Lock()
			for _, r := range b.core.revs {
				if len(r.info.GuestAuthors) > 0 {
					credited++
					if len(r.info.Authors) == 0 {
						guestOnly++
					}
				}
				if len(r.info.Authors) > 0 {
					member++
				}
			}
			b.core.data.Unlock()
			t.Logf("%d steps with guest edits; %d revisions crediting guests, %d guest-only, %d with members", editing, credited, guestOnly, member)
			if guestOnly > 12 || guestOnly == 0 || member == 0 {
				t.Fatalf("%d guest-only revisions in an hour (want 1 to 12), %d with members", guestOnly, member)
			}
		})
	}
}
