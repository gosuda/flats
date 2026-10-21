package board

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
	"github.com/gosuda/flats/internal/wsconn"
)

// Tests of anonymous public editing (docs/spec/board-public-edit-TECH.md
// section 10, the board's part).

// limits changes the board's guest limits in the apply loop.
func (b *tb) limits(fn func(p *publicLimits)) {
	b.t.Helper()
	b.state(func(*engine.State) { fn(&b.in.pub) })
}

// editTB is a board at public access level edit.
func editTB(t *testing.T) *tb {
	t.Helper()
	b := newTB(t)
	b.setLevel(AccessEdit)
	return b
}

// joinGuest says hello as a guest and returns the client and its welcome.
func joinGuest(t *testing.T, b *tb, name, token string) (*client, map[string]any) {
	t.Helper()
	c, err := dial(t, b.pub, origin(b.pub))
	if err != nil {
		t.Fatal(err)
	}
	g := map[string]any{"name": name}
	if token != "" {
		g["token"] = token
	}
	c.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "guest": g})
	w := c.expect("welcome")
	if w["access"] != "guest" {
		t.Fatalf("guest welcome %v", w)
	}
	c.session = w["session"].(string)
	c.snapshot()
	return c, w
}

// joinViewer says hello as a viewer with an optional reconnect token.
func joinViewer(t *testing.T, b *tb, token string, extra map[string]any) (*client, map[string]any) {
	t.Helper()
	c, err := dial(t, b.pub, origin(b.pub))
	if err != nil {
		t.Fatal(err)
	}
	h := map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}}
	if token != "" {
		h["viewer_token"] = token
	}
	for k, v := range extra {
		h[k] = v
	}
	c.send(h)
	w := c.expect("welcome")
	if w["access"] != "public" {
		t.Fatalf("viewer welcome %v", w)
	}
	return c, w
}

func expectFatal(t *testing.T, c *client, code string) map[string]any {
	t.Helper()
	m := c.expect("fatal")
	if m["code"] != code {
		t.Fatalf("fatal %v, want %s", m, code)
	}
	return m
}

func TestCleanGuestName(t *testing.T) {
	for in, want := range map[string]string{
		"  Ana  ":               "Ana",
		"A\u200bn\u0000a":       "Ana",
		"한글 이름":                 "한글 이름",
		strings.Repeat("x", 40): strings.Repeat("x", 40),
	} {
		if got, ok := model.CleanGuestName(in); !ok || got != want {
			t.Errorf("CleanGuestName(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "   ", "\u200b\u200e", "\x01\x02", strings.Repeat("x", 41), "\xff"} {
		if got, ok := model.CleanGuestName(in); ok {
			t.Errorf("CleanGuestName(%q) accepted %q", in, got)
		}
	}
}

// TestGuestNameRules: a guest edits only after a valid name; a viewer is
// never asked, gets Viewer N and keeps it across a reconnect within the
// hold (spec rules 8, 9).
func TestGuestNameRules(t *testing.T) {
	b := editTB(t)
	v, w := joinViewer(t, b, "", nil)
	me := w["me"].(map[string]any)
	if me["label"] != "Viewer 1" || w["level"] != AccessEdit || w["viewer_token"] == "" {
		t.Fatalf("viewer welcome %v", w)
	}
	// A viewer cannot edit by sending an edit.
	v.send(map[string]any{"t": "ops", "n": 1, "ops": []any{sticky(eid(), "x", 0)}})
	expectFatal(t, v, engine.FatalProtocol)

	for _, name := range []string{"", "   ", "\u200b", strings.Repeat("y", 41)} {
		c, err := dial(t, b.pub, origin(b.pub))
		if err != nil {
			t.Fatal(err)
		}
		c.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "guest": map[string]any{"name": name}})
		if m := expectFatal(t, c, FatalNameInvalid); m["detail"] == "" {
			t.Fatalf("name_invalid without detail: %v", m)
		}
	}
	g, gw := joinGuest(t, b, " Ana\u200b ", "")
	gme := gw["me"].(map[string]any)
	if gme["label"] != "Ana" || gme["guest"] != true || !strings.HasPrefix(gme["key"].(string), "p:") || gw["guest_token"] == nil {
		t.Fatalf("guest welcome %v", gw)
	}
	if m := g.settle(1, sticky(eid(), "from Ana", 0)); m["t"] != "ack" {
		t.Fatalf("guest batch %v", m)
	}

	// Viewer numbers: the lowest number not held; a token keeps its
	// number for a minute after its socket closed.
	tok1 := w["viewer_token"].(string)
	_, w2 := joinViewer(t, b, "", nil)
	if w2["me"].(map[string]any)["label"] != "Viewer 2" {
		t.Fatalf("second viewer %v", w2)
	}
	v.ws.Close(1000, "")
	waitFor(t, func() bool { _, n := b.in.PublicPresence(); return n == 1 })
	_, w3 := joinViewer(t, b, "", nil)
	if w3["me"].(map[string]any)["label"] != "Viewer 3" {
		t.Fatalf("a new viewer took a held number: %v", w3)
	}
	// A viewer hello may carry a leftover guest session and watermark.
	again, w4 := joinViewer(t, b, tok1, map[string]any{"session": g.session, "watermark": 1})
	if w4["me"].(map[string]any)["label"] != "Viewer 1" || w4["viewer_token"] != tok1 || w4["session"] != "" {
		t.Fatalf("reconnect within the hold %v", w4)
	}
	again.snapshot()
	defer func(h time.Duration) { viewerHold = h }(viewerHold)
	viewerHold = 10 * time.Millisecond
	again.ws.Close(1000, "")
	waitFor(t, func() bool { _, n := b.in.PublicPresence(); return n == 2 })
	time.Sleep(20 * time.Millisecond)
	if _, w5 := joinViewer(t, b, "", nil); w5["me"].(map[string]any)["label"] != "Viewer 1" {
		t.Fatalf("a released number was not reused: %v", w5)
	}
}

// TestPublicLevelDecidesGuests: guest editing needs both the header and the
// instance level at edit; otherwise a guest hello is a viewer.
func TestPublicLevelDecidesGuests(t *testing.T) {
	b := newTB(t)
	b.setLevel(AccessView)
	_, w := joinViewer(t, b, "", map[string]any{"guest": map[string]any{"name": "Ana"}})
	if w["level"] != AccessView {
		t.Fatalf("welcome %v", w)
	}
	// The header says edit but the instance was not told: still a viewer.
	b.level.mu.Lock()
	b.level.v = AccessEdit
	b.level.mu.Unlock()
	joinViewer(t, b, "", map[string]any{"guest": map[string]any{"name": "Ana"}})
	// The other way round as well.
	b.setLevel(AccessEdit)
	b.level.mu.Lock()
	b.level.v = AccessView
	b.level.mu.Unlock()
	joinViewer(t, b, "", map[string]any{"guest": map[string]any{"name": "Ana"}})
}

// TestGuestBoundary: a guest cannot use Private-only frames and routes, or
// name an asset no live element references (spec rules 18 to 20).
func TestGuestBoundary(t *testing.T) {
	ctx := context.Background()
	b := editTB(t)
	for _, frame := range []map[string]any{
		{"t": "ops_part", "n": 1, "part": 1, "parts": 2, "data": "[]"},
		{"t": "share", "ids": []string{eid()}},
	} {
		g, _ := joinGuest(t, b, "Ana", "")
		g.send(frame)
		expectFatal(t, g, engine.FatalProtocol)
	}
	for _, route := range []string{"/_board/api/assets", "/_board/api/import", "/_board/api/recover", "/_board/api/revisions/1/pin"} {
		resp, _ := b.post(b.pub, route, "application/json", strings.NewReader("{}"))
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s on the public route: %d", route, resp.StatusCode)
		}
	}
	if resp, err := http.Get(b.pub.URL + "/_board/api/revisions"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revisions on the public route: %v %v", resp, err)
	}

	img := pngBytes(t, 4, 4)
	info, err := b.in.AddAsset(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	image := func(id string) map[string]any {
		return map[string]any{"op": "add", "el": map[string]any{"id": id, "type": "image", "x": 0, "y": 0, "w": 4, "h": 4, "rot": 0,
			"asset": info.Hash, "natural": []int{4, 4}}}
	}
	g, _ := joinGuest(t, b, "Ana", "")
	// Held by the upload but referenced by no live element: refused.
	if m := g.settle(1, image(eid())); m["code"] != CodeGuestAsset || m["retry"] != false {
		t.Fatalf("guest add of a non-live asset: %v", m)
	}
	m := connect(t, b.priv, map[string]any{"editor": "mia"}, nil)
	m.snapshot()
	live := eid()
	if a := m.settle(1, image(live)); a["t"] != "ack" {
		t.Fatalf("member image %v", a)
	}
	// Now an element references it: a guest may copy it.
	copyID := eid()
	if a := g.settle(2, image(copyID)); a["t"] != "ack" {
		t.Fatalf("guest copy of a live image: %v", a)
	}
	// The guest deletes its copy, the member deletes the original; undoing
	// the delete of an image without a live reference is refused.
	if a := g.settle(3, map[string]any{"op": "delete", "ids": []string{copyID}}); a["t"] != "ack" {
		t.Fatalf("guest delete %v", a)
	}
	if a := m.settle(2, map[string]any{"op": "delete", "ids": []string{live}}); a["t"] != "ack" {
		t.Fatalf("member delete %v", a)
	}
	if a := g.settle(4, map[string]any{"op": "undo"}); a["code"] != CodeGuestAsset {
		t.Fatalf("guest undo of an image delete without a live reference: %v", a)
	}

	// Guests download the public view projection.
	resp, err := http.Get(b.pub.URL + "/_board/export.zip")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("guest export: %v %v", resp, err)
	}
	resp.Body.Close()
}

// TestGuestRevive: a guest reconnecting with a watermark gets a snapshot
// without deleted content, and may revive only what it deleted itself in
// its session (TECH public edit 3.2).
func TestGuestRevive(t *testing.T) {
	b := editTB(t)
	m := connect(t, b.priv, map[string]any{"editor": "mia"}, nil)
	m.snapshot()
	theirs := eid()
	m.settle(1, sticky(theirs, "member's", 0))
	m.settle(2, map[string]any{"op": "delete", "ids": []string{theirs}})
	var delSeq float64
	b.state(func(st *engine.State) { r, _ := st.Element(theirs); delSeq = float64(r.DelSeq) })

	g, w := joinGuest(t, b, "Ana", "")
	token := w["guest_token"].(string)
	if a := g.settle(1, map[string]any{"op": "revive", "ids": []string{theirs}, "if_del": delSeq}); a["code"] != CodeGuestRevive {
		t.Fatalf("guest revive of a member's deletion: %v", a)
	}
	mine := eid()
	g.settle(2, sticky(mine, "mine", 0))
	g.settle(3, map[string]any{"op": "delete", "ids": []string{mine}})
	if a := g.settle(4, map[string]any{"op": "undo"}); a["t"] != "ack" {
		t.Fatalf("guest undo of its own delete: %v", a)
	}
	if _, ok := b.alive()[mine]; !ok {
		t.Fatal("the guest's undo did not bring its element back")
	}

	// Reconnect with the same token and session and an old watermark: a
	// snapshot of live elements, never the log.
	c, err := dial(t, b.pub, origin(b.pub))
	if err != nil {
		t.Fatal(err)
	}
	c.send(map[string]any{"t": "hello", "v": 1, "session": g.session, "watermark": 1, "pending": map[string]any{"count": 0},
		"guest": map[string]any{"name": "Ana", "token": token}})
	w2 := c.expect("welcome")
	if w2["session"] != g.session || w2["guest_token"] != nil {
		t.Fatalf("guest reconnect %v", w2)
	}
	msg, ok := c.next(func(m map[string]any) bool { return m["t"] == "snapshot" || m["t"] == "delta" })
	if !ok || msg["t"] != "snapshot" || strings.Contains(msg["_raw"].(string), theirs) || strings.Contains(msg["_raw"].(string), "pre_rel") {
		t.Fatalf("guest reconnect got %v", msg)
	}
}

// TestGuestFrameScan: no frame a guest receives names a Tailscale login, a
// member's typed name or an agent label, with two guests at once; guests
// and members see each other's presence in their own forms (TECH public
// edit 3.3, 3.4).
func TestGuestFrameScan(t *testing.T) {
	ctx := context.Background()
	b := editTB(t)
	alice := connect(t, b.priv, map[string]any{"editor": "alice", "name": "Alice Secret"},
		map[string]string{"Tailscale-User-Login": "alice@example.com", "Tailscale-User-Name": "Alice Secret"})
	alice.snapshot()
	bob := connect(t, b.priv, map[string]any{"editor": "bobby", "name": "Bob Hidden"}, nil)
	bob.snapshot()
	el := eid()
	alice.settle(1, sticky(el, "by alice", 0))
	bob.send(map[string]any{"t": "pres", "c": []float64{1, 2}})

	g1, _ := joinGuest(t, b, "Gus", "")
	g2, _ := joinGuest(t, b, "Gia", "")
	joinViewer(t, b, "", nil)
	bob.send(map[string]any{"t": "pres", "c": []float64{3, 4}})
	alice.settle(2, map[string]any{"op": "set", "id": el, "inc": 0, "g": "text", "val": "again"})
	err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.UpdateBoard(ctx, UpdateRequest{OpID: "scan-agent-1", Label: "SecretBot",
			Ops: []engine.AgentOp{{Op: "add", El: json.RawMessage(`{"id":"` + eid() + `","type":"sticky","x":0,"y":300,"w":100,"h":100,"text":"agent"}`)}}}, other)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	g1.settle(1, sticky(eid(), "by gus", 400))
	g1.settle(2, map[string]any{"op": "set", "id": el, "inc": 1, "g": "text", "val": "x"}) // void or applied, either way an ack
	g1.send(map[string]any{"t": "pres", "c": []float64{5, 6}})
	g2.batch(1, map[string]any{"op": "bogus"})

	// Members see the guest's presence with its key and guest set.
	gp := alice.expectPres(func(m map[string]any) bool { return m["name"] == "Gus" })
	if gp["guest"] != true || !strings.HasPrefix(gp["key"].(string), model.GuestKeyPrefix) || gp["verified"] != false {
		t.Fatalf("member sees guest presence %v", gp)
	}
	// Guests see a member as Member N, without guest, and the other guest
	// with guest set.
	mp := g2.expectPres(func(m map[string]any) bool { return strings.HasPrefix(m["name"].(string), "Member ") && m["c"] != nil })
	if mp["guest"] != nil || mp["verified"] != false || !strings.HasPrefix(mp["key"].(string), "p:") {
		t.Fatalf("guest sees member presence %v", mp)
	}
	op := g2.expectPres(func(m map[string]any) bool { return m["name"] == "Gus" })
	if op["guest"] != true || !strings.HasPrefix(op["key"].(string), "p:") {
		t.Fatalf("guest sees guest presence %v", op)
	}
	if _, ok := g2.next(func(m map[string]any) bool { return m["t"] == "viewers" }); !ok {
		t.Fatal("guests got no viewers frame")
	}
	bob.ws.Close(1000, "")
	g2.expectPres(func(m map[string]any) bool { return m["gone"] == true && strings.HasPrefix(m["key"].(string), "p:") })

	// The two guests receive the same body for each delta, apart from from.
	bodies := map[float64]string{}
	for _, g := range []*client{g1, g2} {
		g.quiet(300*time.Millisecond, func(map[string]any) bool { return false })
		for _, m := range g.backlog {
			raw := m["_raw"].(string)
			for _, bad := range []string{"ts:", "browser:", "alice@example.com", "Alice Secret", "Bob Hidden", "SecretBot"} {
				if strings.Contains(raw, bad) {
					t.Fatalf("a guest frame contains %q: %s", bad, raw)
				}
			}
			if m["t"] == "delta" {
				d := map[string]any{}
				json.Unmarshal([]byte(raw), &d)
				delete(d, "from")
				body, _ := json.Marshal(d)
				seq := d["seq"].(float64)
				if prev, ok := bodies[seq]; ok && prev != string(body) {
					t.Fatalf("guests got different bodies for seq %v:\n%s\n%s", seq, prev, body)
				}
				bodies[seq] = string(body)
				if strings.Contains(raw, `"by":{"kind":"agent"`) && !strings.Contains(raw, `"label":"Agent"`) {
					t.Fatalf("agent delta for guests %s", raw)
				}
			}
		}
	}
	if len(bodies) < 3 {
		t.Fatalf("guests saw %d deltas", len(bodies))
	}
}

// expectPres waits for a presence frame matching pred.
func (c *client) expectPres(pred func(map[string]any) bool) map[string]any {
	c.t.Helper()
	m, ok := c.next(func(m map[string]any) bool { return m["t"] == "pres" && pred(m) })
	if !ok {
		c.t.Fatalf("no matching presence; backlog %v", c.backlog)
	}
	return m
}

// TestGuestLimits: the per-guest and per-board limits of section 4 refuse
// with a reason and keep the socket; Private edits still succeed.
func TestGuestLimits(t *testing.T) {
	b := editTB(t)
	b.in.pub.guestSockets = 2
	g, _ := joinGuest(t, b, "Ana", "")
	ben, _ := joinGuest(t, b, "Ben", "")
	c, _ := dial(t, b.pub, origin(b.pub))
	c.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "guest": map[string]any{"name": "Cid"}})
	expectFatal(t, c, FatalFull)

	// Too many operations and too large a frame: recorded refusals, the
	// next batch number still works.
	var many []map[string]any
	for range 101 {
		many = append(many, sticky(eid(), "", 0))
	}
	if m := g.batch(1, many...); m["code"] != CodeGuestSize || m["retry"] != false {
		t.Fatalf("101 operations %v", m)
	}
	if m := g.batch(2, sticky(eid(), strings.Repeat("z", 70<<10), 0)); m["code"] != CodeGuestSize {
		t.Fatalf("a 70 KiB frame %v", m)
	}
	if m := g.settle(3, sticky(eid(), "ok", 0)); m["t"] != "ack" {
		t.Fatalf("after refusals %v", m)
	}
	// The per-guest batch rate: a burst beyond 20 gets guest_rate.
	n, limited := 4, false
	for range 40 {
		g.send(map[string]any{"t": "ops", "n": n, "ops": []any{map[string]any{"op": "bogus"}}})
		m, _ := g.next(func(m map[string]any) bool { return m["n"] == float64(n) })
		if m["code"] == CodeGuestRate {
			limited = true
			break
		}
		n++
	}
	if !limited {
		t.Fatal("no guest_rate over the per-guest batch rate")
	}

	// The board's guest operations.
	b.limits(func(p *publicLimits) { p.guestOps = newBucket(1, 3) })
	ben.ws.Close(1000, "")
	waitFor(t, func() bool { g, _ := b.in.PublicPresence(); return g == 1 })
	time.Sleep(time.Second)
	g2, _ := joinGuest(t, b, "Dee", "")
	if m := g2.batch(1, sticky(eid(), "", 0), sticky(eid(), "", 0), sticky(eid(), "", 0), sticky(eid(), "", 0)); m["code"] != CodeGuestRate || m["retry"] != true {
		t.Fatalf("over the board's guest operations %v", m)
	}
}

// TestGuestRoom: the reserved element room and the guests' created-row and
// group-id caps refuse guests while members still edit (TECH public edit
// 4.3, 4.4).
func TestGuestRoom(t *testing.T) {
	b := editTB(t)
	b.state(func(st *engine.State) {
		l := st.Limits()
		l.Elements = 10
		st.SetLimits(l)
	})
	b.limits(func(p *publicLimits) { p.createdMax = 100 })
	g, _ := joinGuest(t, b, "Ana", "")
	n := 1
	for range 9 {
		if m := g.settle(n, sticky(eid(), "", 0)); m["t"] != "ack" {
			t.Fatalf("guest add %d: %v", n, m)
		}
		n++
	}
	if m := g.settle(n, sticky(eid(), "", 0)); m["code"] != CodeGuestRoom {
		t.Fatalf("a guest add over 90%% of the elements: %v", m)
	}
	n++
	m := connect(t, b.priv, map[string]any{"editor": "mia"}, nil)
	m.snapshot()
	if a := m.settle(1, sticky(eid(), "", 0)); a["t"] != "ack" {
		t.Fatalf("member add in the reserved room: %v", a)
	}

	// Created rows of one guest session.
	b.state(func(st *engine.State) {
		l := st.Limits()
		l.Elements = 10000
		st.SetLimits(l)
	})
	b.limits(func(p *publicLimits) { p.createdMax = 11 })
	if a := g.settle(n, sticky(eid(), "", 0)); a["t"] != "ack" {
		t.Fatalf("guest add %v", a)
	}
	n++
	if a := g.settle(n, sticky(eid(), "", 0), sticky(eid(), "", 0)); a["code"] != CodeGuestRoom {
		t.Fatalf("over the created rows: %v", a)
	}
	n++
	var created int
	b.state(func(st *engine.State) { created = st.CreatedCount(g.session) })
	if created != 10 {
		t.Fatalf("created rows %d", created)
	}

	// Group ids first used by guests.
	b.limits(func(p *publicLimits) { p.createdMax, p.groupsMax = 1000, 1 })
	ids := []string{eid(), eid(), eid(), eid()}
	for _, id := range ids {
		g.settle(n, sticky(id, "", 0))
		n++
	}
	group := func(a, c string) map[string]any {
		return map[string]any{"op": "group", "ids": []string{a, c}, "inc_of": map[string]any{}, "group": newID("g_")}
	}
	if a := g.settle(n, group(ids[0], ids[1])); a["t"] != "ack" {
		t.Fatalf("first guest group %v", a)
	}
	n++
	if a := g.settle(n, group(ids[2], ids[3])); a["code"] != CodeGuestRoom {
		t.Fatalf("a guest group over the cap %v", a)
	}
	b.state(func(*engine.State) {
		if b.in.sd.guestGroups != 1 {
			t.Errorf("guest groups %d", b.in.sd.guestGroups)
		}
	})
}

// TestGuestGroupWindows: over an hour of fake time, guests editing with
// pauses and members' groups expiring in between create at most 12
// guest-only entries (TECH public edit 5.1).
func TestGuestGroupWindows(t *testing.T) {
	b := editTB(t)
	g, _ := joinGuest(t, b, "Ana", "")
	m := connect(t, b.priv, map[string]any{"editor": "mia", "name": "Mia"}, nil)
	m.snapshot()
	el := eid()
	g.settle(1, sticky(el, "", 0))
	gn, mn := 2, 1
	tick := func() {
		if err := b.in.do(context.Background(), b.in.tick); err != nil {
			t.Fatal(err)
		}
	}
	for s := 0; s < 3600; s += 10 {
		b.clock.Add(10 * time.Second)
		// Guests edit in bursts with 31-second and longer pauses.
		if (s/40)%3 != 2 {
			g.settle(gn, map[string]any{"op": "set", "id": el, "inc": 0, "g": "text", "val": noise(8, s)})
			gn++
		}
		// A member edits now and then, so its group expires idle in between.
		if s%170 == 0 {
			m.settle(mn, sticky(eid(), "", float64(s)))
			mn++
		}
		tick()
	}
	if err := b.core.importOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	guestOnly, credited := 0, 0
	b.core.data.Lock()
	for _, r := range b.core.revs {
		if len(r.info.GuestAuthors) > 0 {
			credited++
			if len(r.info.Authors) == 0 {
				guestOnly++
			}
		}
		for _, a := range r.info.Authors {
			if a == "Ana" {
				t.Fatalf("a guest name among the authors: %+v", r.info)
			}
		}
	}
	b.core.data.Unlock()
	if guestOnly > 12 || credited == 0 {
		t.Fatalf("%d guest-only entries in an hour (%d crediting guests)", guestOnly, credited)
	}
}

// TestOutboxGuestCredits: an entry lists five guest names and counts the
// rest; leaving edit closes the guests' group with an entry.
func TestOutboxGuestCredits(t *testing.T) {
	b := editTB(t)
	b.in.pub.guestWelcomes = newBucket(1000, 1000)
	el := eid()
	var inc int64
	for i, name := range []string{"G1", "G2", "G3", "G4", "G5", "G6", "G7"} {
		g, _ := joinGuest(t, b, name, "")
		if i == 0 {
			g.settle(1, sticky(el, "", 0))
			b.state(func(st *engine.State) { r, _ := st.Element(el); inc = r.Inc })
		} else if a := g.settle(1, map[string]any{"op": "set", "id": el, "inc": inc, "g": "text", "val": name}); a["t"] != "ack" {
			t.Fatalf("guest set %v", a)
		}
	}
	before := len(mustOutbox(t, b.in))
	b.setLevel(AccessView)
	entries := mustOutbox(t, b.in)
	if len(entries) != before+1 {
		t.Fatalf("leaving edit recorded %d entries", len(entries)-before)
	}
	e := entries[len(entries)-1]
	if len(e.Guests) != 5 || e.GuestsMore != 2 || len(e.Authors) != 0 || e.Guests[0] != "G1" {
		t.Fatalf("entry %+v", e)
	}
	if _, ok := groupsOf(t, b)[guestGroup]; ok {
		t.Fatal("the guests' group stayed open after edit ended")
	}
}

// TestKillSwitchEpoch: a guest batch in flight when the level drops is
// refused, and guest sockets get editing_ended (spec rule 29).
func TestKillSwitchEpoch(t *testing.T) {
	b := editTB(t)
	g, _ := joinGuest(t, b, "Ana", "")
	v, _ := joinViewer(t, b, "", nil)
	release := make(chan struct{})
	held := make(chan struct{})
	go b.in.do(context.Background(), func() error { close(held); <-release; return nil })
	<-held
	done := make(chan error, 1)
	go func() { done <- b.in.SetPublicAccess(context.Background(), AccessView) }()
	waitFor(t, func() bool { return len(b.in.items) == 1 })
	el := eid()
	g.send(map[string]any{"t": "ops", "n": 1, "ops": []any{sticky(el, "late", 0)}})
	waitFor(t, func() bool { return len(b.in.items) == 2 })
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	expectFatal(t, g, FatalEditingEnded)
	if !g.closed(5 * time.Second) {
		t.Fatal("the guest socket stayed open")
	}
	if _, ok := b.alive()[el]; ok {
		t.Fatal("a guest batch of the old epoch committed")
	}
	if l := v.expect("level"); l["access"] != AccessView {
		t.Fatalf("viewer level frame %v", l)
	}
	// A guest hello now joins as a viewer.
	joinViewer(t, b, "", map[string]any{"guest": map[string]any{"name": "Ana"}})
}

// TestLabelChurn: the connection labels return to the live editors after
// 1,000 guest sessions joined, sent presence and left.
func TestLabelChurn(t *testing.T) {
	b := editTB(t)
	b.in.pub.guestWelcomes = newBucket(1e6, 1e6)
	b.in.publicStarts = newStartLimiter(1e6)
	m := connect(t, b.priv, map[string]any{"editor": "mia"}, nil)
	m.snapshot()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range 1000 {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			g, _ := joinGuest(t, b, "G"+noise(4, i), "")
			g.send(map[string]any{"t": "pres", "c": []float64{1, 1}})
			g.ws.Close(1000, "")
		}()
	}
	wg.Wait()
	waitFor(t, func() bool {
		b.in.hub.mu.Lock()
		defer b.in.hub.mu.Unlock()
		return len(b.in.hub.labels) == 1 && len(b.in.hub.keys) == 1 && b.in.hub.guests == 0
	})
	if err := b.in.do(context.Background(), b.in.tick); err != nil {
		t.Fatal(err)
	}
	n := 0
	b.state(func(st *engine.State) {
		for _, c := range st.Export().Sessions {
			if model.IsGuestKey(c.Key) {
				n++
			}
		}
	})
	if n > b.in.pub.guestSessions {
		t.Fatalf("%d guest sessions kept", n)
	}
}

// TestGuestChargeRecovery: the guests' byte budget fills, space freed by
// retention returns budget to guests (TECH public edit 4.3).
func TestGuestChargeRecovery(t *testing.T) {
	b := editTB(t)
	b.core.data.Lock()
	b.core.extra = 2 << 30 // the board's other content
	b.core.data.Unlock()
	b.core.setQuota(4 << 30)
	b.limits(func(p *publicLimits) { p.chargeMax = 200 << 10 })
	if _, err := b.in.meas.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	tick := func() {
		if err := b.in.do(context.Background(), b.in.tick); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	g, _ := joinGuest(t, b, "Ana", "")
	n := 1
	fill := func() int64 {
		var wrote int64
		for i := 0; i < 400; i++ {
			m := g.settle(n, sticky(eid(), noise(3000, n), 0))
			n++
			if m["code"] == CodeGuestRoom {
				return wrote
			}
			if m["t"] != "ack" {
				t.Fatalf("guest batch %v", m)
			}
			wrote += 3000
		}
		t.Fatal("the guests' budget never filled")
		return 0
	}
	charge := func() (n int64) {
		b.state(func(*engine.State) { n = b.in.sd.guestCharge })
		return n
	}
	first := fill()
	if c := charge(); c > 200<<10 || c < 100<<10 {
		t.Fatalf("charge %d", c)
	}
	// A member can still write.
	m := connect(t, b.priv, map[string]any{"editor": "mia"}, nil)
	m.snapshot()
	if a := m.settle(1, sticky(eid(), noise(3000, 1), 0)); a["t"] != "ack" {
		t.Fatalf("member write at the guest budget %v", a)
	}
	tick()
	// Retention frees the board's other content by about the budget.
	b.core.data.Lock()
	b.core.extra -= 400 << 10
	b.core.data.Unlock()
	b.in.meas.start()
	waitFor(t, func() bool { o, ok := b.in.meas.get(); return ok && o < 2<<30 })
	tick()
	if c := charge(); c != 0 {
		t.Fatalf("charge %d after freeing space", c)
	}
	if again := fill(); again < first/2 {
		t.Fatalf("guests wrote %d after freeing, %d before", again, first)
	}
}

// TestGuestDiskRoom: a guest batch that would leave less than the reserved
// share of the disk quota is refused, a member's is not.
func TestGuestDiskRoom(t *testing.T) {
	b := editTB(t)
	b.core.setQuota(64 << 20)
	if _, err := b.in.meas.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.core.data.Lock()
	b.core.extra = 64<<20*90/100 - 100<<10
	b.core.data.Unlock()
	b.in.meas.start()
	waitFor(t, func() bool { o, ok := b.in.meas.get(); return ok && o > 50<<20 })
	g, _ := joinGuest(t, b, "Ana", "")
	refused := false
	for n := 1; n < 200 && !refused; n++ {
		m := g.settle(n, sticky(eid(), noise(3000, n), 0))
		refused = m["code"] == CodeGuestRoom
	}
	if !refused {
		t.Fatal("guests filled the reserved disk room")
	}
	m := connect(t, b.priv, map[string]any{"editor": "mia"}, nil)
	m.snapshot()
	if a := m.settle(1, sticky(eid(), noise(3000, 1), 0)); a["t"] != "ack" {
		t.Fatalf("member write in the reserved room %v", a)
	}
}

// TestSafePoint: a safe point is recorded, reused while the board is
// unchanged and recorded again after a change (TECH public edit 6.2).
func TestSafePoint(t *testing.T) {
	ctx := context.Background()
	b := newTB(t)
	m := connect(t, b.priv, map[string]any{"editor": "mia", "name": "Mia"}, nil)
	m.snapshot()
	m.settle(1, sticky(eid(), "a", 0))
	seq, reuse, err := b.in.SafePoint(ctx, nil)
	if err != nil || reuse || seq == 0 {
		t.Fatalf("first safe point %d %v %v", seq, reuse, err)
	}
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	revs := b.core.revs
	sp := revs[len(revs)-1]
	if sp.info.Kind != KindSafePoint {
		t.Fatalf("revision %+v", sp.info)
	}
	// The member's open group was recorded first, as a person entry.
	if prev := revs[len(revs)-2]; prev.info.Kind != "person" {
		t.Fatalf("before the safe point %+v", prev.info)
	}
	before := len(b.core.revs)
	if seq, reuse, err := b.in.SafePoint(ctx, sp.files); err != nil || !reuse || seq != 0 {
		t.Fatalf("unchanged safe point %d %v %v", seq, reuse, err)
	}
	if err := b.core.importOutbox(ctx); err != nil || len(b.core.revs) != before {
		t.Fatalf("reuse recorded a revision: %v", err)
	}
	m.settle(2, sticky(eid(), "b", 0))
	if seq, reuse, err := b.in.SafePoint(ctx, sp.files); err != nil || reuse || seq == 0 {
		t.Fatalf("safe point after a change %d %v %v", seq, reuse, err)
	}
}

// TestAuthorshipAfterMoves: an agent's text stays agent-written after a
// member moves it and a guest restyles it, in get_board, a revision and
// get_revision; a guest's retype makes it guest-written (TECH public edit 7).
func TestAuthorshipAfterMoves(t *testing.T) {
	ctx := context.Background()
	b := editTB(t)
	id := eid()
	err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.UpdateBoard(ctx, UpdateRequest{OpID: "author-1", Label: "Bot",
			Ops: []engine.AgentOp{{Op: "add", El: json.RawMessage(`{"id":"` + id + `","type":"sticky","x":0,"y":0,"w":100,"h":100,"text":"plan"}`)}}}, other)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	m := connect(t, b.priv, map[string]any{"editor": "mia"}, nil)
	m.snapshot()
	if a := m.settle(1, map[string]any{"op": "set", "id": id, "inc": 2, "g": "geom", "val": map[string]any{"x": 50, "y": 0, "w": 100, "h": 100, "rot": 0}}); a["t"] != "ack" {
		t.Fatalf("member move %v", a)
	}
	g, _ := joinGuest(t, b, "Gus", "")
	if a := g.settle(1, map[string]any{"op": "set", "id": id, "inc": 2, "g": "style.color", "val": "pink"}); a["t"] != "ack" {
		t.Fatalf("guest restyle %v", a)
	}
	authors := func(raw json.RawMessage) (map[string]any, any) {
		var e map[string]any
		json.Unmarshal(raw, &e)
		if _, ok := e["ta"]; ok {
			t.Fatalf("agents see the stored ta: %s", raw)
		}
		ta, _ := e["text_authors"].(map[string]any)
		text, _ := ta["text"].(map[string]any)
		return text, e["last_change"]
	}
	page, err := b.in.GetBoard(ctx, Query{IDs: []string{id}})
	if err != nil {
		t.Fatal(err)
	}
	if text, last := authors(page.Elements[0]); text["kind"] != model.KindAgent || last != model.KindGuest {
		t.Fatalf("get_board %s", page.Elements[0])
	}
	// A revision captures it; get_revision reads it the same way.
	if err := b.in.SetPublicAccess(ctx, AccessView); err != nil { // closes the guests' group
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
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := authors(rp.Elements[0]); text["kind"] != model.KindAgent {
		t.Fatalf("get_revision %s", rp.Elements[0])
	}
	b.setLevel(AccessEdit)
	g2, _ := joinGuest(t, b, "Gus", "")
	if a := g2.settle(1, map[string]any{"op": "set", "id": id, "inc": 2, "g": "text", "val": "mine now"}); a["t"] != "ack" {
		t.Fatalf("guest retype %v", a)
	}
	page, _ = b.in.GetBoard(ctx, Query{IDs: []string{id}})
	if text, _ := authors(page.Elements[0]); text["kind"] != model.KindGuest || text["name"] != "Gus" {
		t.Fatalf("after a guest retype %s", page.Elements[0])
	}
}

// TestBoardDBAddsColumns: an older board.db without the public editing
// columns opens and gains them.
func TestBoardDBAddsColumns(t *testing.T) {
	b := newTB(t)
	m := connect(t, b.priv, map[string]any{"editor": "mia"}, nil)
	m.snapshot()
	m.settle(1, sticky(eid(), "kept", 0))
	b.in.Stop()
	db, err := sql.Open("sqlite", filepath.Join(b.dir, "board", dbName))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range addedColumns {
		if _, err := db.Exec(`ALTER TABLE ` + c.table + ` DROP COLUMN ` + c.column); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	b.open()
	if len(b.alive()) != 1 {
		t.Fatal("the board lost its content")
	}
	g := map[string]bool{}
	rows, err := b.in.db.db.Query(`SELECT name FROM pragma_table_info('outbox')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		g[n] = true
	}
	rows.Close()
	if !g["guests"] || !g["guests_more"] {
		t.Fatalf("outbox columns %v", g)
	}
}

// TestGuestTokenMustBeIssued: a token the server did not issue (or whose
// session ended) selects no identity; an issued one resumes it.
func TestGuestTokenMustBeIssued(t *testing.T) {
	b := editTB(t)
	chosen := strings.Repeat("0", 32)
	c1, w1 := joinGuest(t, b, "Ana", chosen)
	_, w2 := joinGuest(t, b, "Ben", chosen)
	t1, _ := w1["guest_token"].(string)
	t2, _ := w2["guest_token"].(string)
	if t1 == "" || t2 == "" || t1 == chosen || t1 == t2 {
		t.Fatalf("tokens %q %q for a chosen token", t1, t2)
	}
	keys := func() map[string]int {
		out := map[string]int{}
		b.state(func(st *engine.State) {
			for _, se := range st.Export().Sessions {
				out[se.Key]++
			}
		})
		return out
	}
	if k := keys(); k[guestKey(chosen)] != 0 || k[guestKey(t1)] != 1 || k[guestKey(t2)] != 1 {
		t.Fatalf("sessions %v", k)
	}
	// The issued token resumes its identity on a new socket.
	again, w3 := joinGuest(t, b, "Ana", t1)
	if k := keys(); w3["guest_token"] != nil || k[guestKey(t1)] != 2 {
		t.Fatalf("resume with the issued token %v, sessions %v", w3, k)
	}
	// Once its session expired, the token is a stranger's again.
	again.ws.Close(1000, "")
	c1.ws.Close(1000, "")
	waitFor(t, func() bool { g, _ := b.in.PublicPresence(); return g == 1 })
	b.clock.Add(2 * time.Hour)
	_, w4 := joinGuest(t, b, "Ana", t1)
	if w4["guest_token"] == nil {
		t.Fatalf("an expired session's token selected its identity: %v", w4)
	}
}

// TestGuestUnknownFrameFlood: frames a board ignores still count toward the
// guests' byte budget, so a flood of them leaves guests' operations refused
// until the budget recovers, and the sockets stay open.
func TestGuestUnknownFrameFlood(t *testing.T) {
	b := editTB(t)
	b.in.pub.guestBytes = newBucket(1<<20, 200<<10)
	flooder, _ := joinGuest(t, b, "Flo", "")
	other, _ := joinGuest(t, b, "Ana", "")
	pad := strings.Repeat("p", 100<<10) // larger than a guest frame, too
	for range 30 {
		flooder.send(map[string]any{"t": "unknown", "padding": pad})
	}
	if m := other.batch(1, sticky(eid(), "", 0)); m["code"] != CodeGuestRate || m["retry"] != true {
		t.Fatalf("an operation during the flood: %v", m)
	}
	if !flooder.quiet(100*time.Millisecond, func(m map[string]any) bool { return m["t"] == "fatal" }) {
		t.Fatal("the flood closed the socket")
	}
	if m := other.settle(1, sticky(eid(), "", 0)); m["t"] != "ack" {
		t.Fatalf("after the budget recovered: %v", m)
	}
}

// TestStalledGuestsStayInTheirShare: guests that stop reading, on several
// boards, never hold more queued and unwritten bytes than the guests'
// share of the host's Public budget; viewers still get in, and the share
// returns to zero once the guests are gone (TECH public edit 4.2).
func TestStalledGuestsStayInTheirShare(t *testing.T) {
	defer func(c int64) { guestShareCap = c }(guestShareCap)
	guestShareCap = 2 << 20
	boards := []*tb{editTB(t), editTB(t)}
	var guests []*client
	var members []*client
	for _, b := range boards {
		for i := range 3 {
			g, _ := joinGuest(t, b, "G"+strconv.Itoa(i), "")
			g.gate.Lock() // stops reading
			guests = append(guests, g)
		}
		m := connect(t, b.priv, map[string]any{"editor": "mia"}, nil)
		m.snapshot()
		members = append(members, m)
	}
	held := func() (n int64) {
		for _, b := range boards {
			for _, c := range b.in.hub.list() {
				if c.guest {
					c.mu.Lock()
					q := c.qBytes
					c.mu.Unlock()
					n += q + c.ws.Budget().Used()
				}
			}
		}
		return n
	}
	stop := make(chan struct{})
	worst := make(chan int64, 1)
	go func() {
		var w int64
		for {
			select {
			case <-stop:
				worst <- w
				return
			case <-time.After(2 * time.Millisecond):
				if n := held(); n > w {
					w = n
				}
				if s := guestShare.Load(); s > guestShareCap {
					w = max(w, s)
				}
			}
		}
	}()
	for n := 1; n <= 20; n++ {
		for i, m := range members {
			var ops []map[string]any
			for range 40 {
				ops = append(ops, sticky(eid(), noise(3900, n*100+i), 0))
			}
			if a := m.settle(n, ops...); a["t"] != "ack" {
				t.Fatalf("member batch %v", a)
			}
		}
	}
	close(stop)
	if w := <-worst; w > guestShareCap {
		t.Fatalf("stalled guests held %d bytes, over the guests' share of %d", w, guestShareCap)
	}
	// Viewers are admitted while the guests are stalled.
	joinViewer(t, boards[0], "", nil)
	for _, g := range guests {
		g.gate.Unlock()
		g.ws.Close(1000, "")
	}
	waitFor(t, func() bool { return guestShare.Load() == 0 })
}

// TestContentEquals: the live board, unrecorded edits included, is
// compared with a file list without recording anything.
func TestContentEquals(t *testing.T) {
	ctx := context.Background()
	b := newTB(t)
	m := connect(t, b.priv, map[string]any{"editor": "mia"}, nil)
	m.snapshot()
	m.settle(1, sticky(eid(), "a", 0))
	if _, _, err := b.in.SafePoint(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	files := b.core.revs[len(b.core.revs)-1].files
	reversed := slices.Clone(files)
	slices.Reverse(reversed)
	if eq, err := b.in.ContentEquals(ctx, reversed); err != nil || !eq {
		t.Fatalf("unchanged board: %v %v", eq, err)
	}
	outbox := len(mustOutbox(t, b.in))
	m.settle(2, sticky(eid(), "b", 0)) // not recorded yet: its group is open
	if eq, err := b.in.ContentEquals(ctx, files); err != nil || eq {
		t.Fatalf("after an unrecorded edit: %v %v", eq, err)
	}
	if n := len(mustOutbox(t, b.in)); n != outbox {
		t.Fatalf("ContentEquals recorded %d entries", n-outbox)
	}
	if _, ok := groupsOf(t, b)["browser:mia"]; !ok {
		t.Fatal("ContentEquals closed the open group")
	}
}

// stalledWS returns a WebSocket whose peer completes the handshake and
// then never reads, so every frame stays unwritten in it until it closes.
func stalledWS(t *testing.T) *wsconn.Conn {
	t.Helper()
	near, far := net.Pipe()
	go func() {
		req, err := http.ReadRequest(bufio.NewReader(far))
		if err != nil {
			return
		}
		fmt.Fprintf(far, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
			wsconn.AcceptKey(req.Header.Get("Sec-WebSocket-Key")))
	}()
	ws, err := wsconn.Dial(context.Background(), func(context.Context) (net.Conn, error) { return near, nil }, "board", "/_board/ws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close(1006, ""); far.Close() })
	return ws
}

// TestClosingGuestKeepsItsShare: a guest socket that closes while its
// writer is stalled keeps its unwritten bytes charged to the guests' share
// until the socket is gone, so a replacement guest cannot take that room
// meanwhile (TECH public edit 4.2).
func TestClosingGuestKeepsItsShare(t *testing.T) {
	b := newTB(t)
	base := guestShare.Load()
	c := &conn{in: b.in, ws: stalledWS(t), public: true, guest: true, chargeGuest: true, wake: make(chan struct{}, 1)}
	pumped := make(chan struct{})
	go func() { c.pump(); close(pumped) }()
	frame := []byte(`{"t":"x","pad":"` + strings.Repeat("p", 100<<10) + `"}`)
	if !c.enqueue(frame) {
		t.Fatal("the frame was refused")
	}
	n := int64(len(frame))
	waitFor(t, func() bool { return c.ws.Budget().Used() == n })
	c.closeAfterQueue()
	<-pumped
	if got := guestShare.Load() - base; got != n {
		t.Fatalf("a closing guest's unwritten %d bytes charge %d to the guests' share", n, got)
	}
	defer func(v int64) { guestShareCap = v }(guestShareCap)
	guestShareCap = base + n
	if reserveShare(&publicQueued, 1, 1) {
		publicQueued.Add(-1)
		guestShare.Add(-1)
		t.Fatal("a replacement guest took the room of a closing guest's unwritten bytes")
	}
	<-c.ws.Done()
	waitFor(t, func() bool { return guestShare.Load() == base })
}

// TestDepartedLabelNotReusedBeforeGone: the others learn at once that an
// editor left, even while its own socket is stalled, and a new editor
// taking a label afterwards is never cleared by a late gone frame.
func TestDepartedLabelNotReusedBeforeGone(t *testing.T) {
	b := editTB(t)
	watcher, _ := joinGuest(t, b, "Wat", "")
	leaving, _ := joinGuest(t, b, "Lea", "")
	leaving.send(map[string]any{"t": "pres", "c": []float64{1, 1}})
	lp := watcher.expectPres(func(m map[string]any) bool { return m["name"] == "Lea" })
	leaving.gate.Lock() // its socket stalls as it leaves
	defer leaving.gate.Unlock()
	start := time.Now()
	leaving.ws.Close(1000, "")
	watcher.expectPres(func(m map[string]any) bool { return m["gone"] == true && m["key"] == lp["key"] })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the gone frame waited %v for the leaving socket", d)
	}
	newcomer, _ := joinGuest(t, b, "New", "")
	newcomer.send(map[string]any{"t": "pres", "c": []float64{2, 2}})
	np := watcher.expectPres(func(m map[string]any) bool { return m["name"] == "New" })
	if !watcher.quiet(2500*time.Millisecond, func(m map[string]any) bool { return m["gone"] == true && m["key"] == np["key"] }) {
		t.Fatal("a stale gone frame removed the newcomer")
	}
}

// TestGuestDeltaMarkedForMembers: a member-facing delta of a guest's batch
// says it came from a guest, so a name like "Operator" never passes as a
// member's.
func TestGuestDeltaMarkedForMembers(t *testing.T) {
	b := editTB(t)
	m := connect(t, b.priv, map[string]any{"editor": "mia", "name": "Mia"}, nil)
	m.snapshot()
	g, _ := joinGuest(t, b, "Operator", "")
	g.settle(1, sticky(eid(), "", 0))
	d := m.expect("delta")
	by, _ := d["by"].(map[string]any)
	if by["kind"] != "person" || by["label"] != "Operator" || by["guest"] != true {
		t.Fatalf("member-facing delta by %v", by)
	}
	m.settle(1, sticky(eid(), "", 0))
	own, _ := m.next(func(x map[string]any) bool { return x["t"] == "delta" })
	if _, ok := own["by"].(map[string]any)["guest"]; ok {
		t.Fatalf("a member's delta is marked as a guest's: %v", own["by"])
	}
}

// TestDepartingLabelReserved: a departed key's label stays out of reach
// until its gone frame is queued, so a late gone frame can never name a
// newcomer.
func TestDepartingLabelReserved(t *testing.T) {
	h := newHub()
	a, b, c := &conn{key: "guest:a"}, &conn{key: "guest:b"}, &conn{key: "guest:c"}
	h.admit(false, 1, 1)
	h.join(a)
	if !h.leave(a) {
		t.Fatal("not the key's last socket")
	}
	h.join(b)
	if b.plabel == a.plabel {
		t.Fatalf("%s reused before its gone frame", a.plabel)
	}
	h.gone(a)
	h.join(c)
	if c.plabel != a.plabel {
		t.Fatalf("the freed label %s was not reused: %s", a.plabel, c.plabel)
	}
}
