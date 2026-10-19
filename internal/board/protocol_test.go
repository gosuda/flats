package board

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
)

func isDelta(m map[string]any) bool { return m["t"] == "delta" }

func TestTwoEditorsSeeEachOther(t *testing.T) {
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice", "name": "Alice"}, nil)
	seqA, els, meta := a.snapshot()
	if len(els) != 0 || meta["title"] != "Test" {
		t.Fatalf("snapshot %d %v %v", seqA, els, meta)
	}
	c := connect(t, b.priv, map[string]any{"editor": "carol", "name": "Carol"}, map[string]string{"Tailscale-User-Login": "carol@example.com"})
	c.snapshot()
	w := connect(t, b.priv, map[string]any{"editor": "bob"}, nil)
	w.snapshot()

	id := eid()
	ack := a.batch(1, sticky(id, "hello", 0))
	if ack["t"] != "ack" {
		t.Fatalf("answer %v", ack)
	}
	mine := a.expect("delta")
	if mine["from"] != a.session+":1" {
		t.Fatalf("own delta from %v", mine["from"])
	}
	for _, other := range []*client{c, w} {
		d := other.expect("delta")
		raw := d["_raw"].(string)
		if _, ok := d["from"]; ok || strings.Contains(raw, a.session) {
			t.Fatalf("another editor received the sender's session: %s", raw)
		}
		if d["seq"] != mine["seq"] {
			t.Fatalf("seq %v != %v", d["seq"], mine["seq"])
		}
	}
	if got := b.alive()[id]; got != "hello" {
		t.Fatalf("element %q", got)
	}

	// Presence: server-filled identity, verified only from the trusted header.
	c.send(map[string]any{"t": "pres", "c": []float64{1, 2}, "key": "ts:mallory", "name": "Mallory", "verified": true})
	p := a.expect("pres")
	if p["key"] != "ts:carol@example.com" || p["verified"] != true || p["name"] != "carol@example.com" {
		t.Fatalf("presence %v", p)
	}
	a.send(map[string]any{"t": "pres", "c": []float64{3, 4}, "verified": true})
	p = c.expect("pres")
	if p["key"] != "browser:alice" || p["verified"] != false || p["name"] != "Alice" {
		t.Fatalf("presence %v", p)
	}
	// A null field clears it on the other canvases; a malformed update is
	// dropped.
	a.send(map[string]any{"t": "pres", "drag": map[string]any{"e_aaaaaaaaaa": []float64{1, 2, 3, 4, 0}}})
	a.send(map[string]any{"t": "pres", "sel": []string{"<script>"}})
	a.send(map[string]any{"t": "pres", "drag": nil})
	c.expect("pres")
	p = c.expect("pres")
	if v, ok := p["drag"]; !ok || v != nil {
		t.Fatalf("clearing presence %v", p)
	}

	// Shares count up and are readable by agents.
	a.send(map[string]any{"t": "share", "ids": []string{id}})
	s1 := a.expect("shared")
	a.send(map[string]any{"t": "share", "ids": []string{id, eid()}})
	s2 := a.expect("shared")
	if s2["n"].(float64) != s1["n"].(float64)+1 {
		t.Fatalf("share numbers %v %v", s1, s2)
	}
	v, err := b.in.GetShare(context.Background(), int64(s2["n"].(float64)))
	if err != nil || len(v.Elements) != 1 || len(v.Missing) != 1 || v.Label != "Alice" || v.Verified {
		t.Fatalf("share %+v %v", v, err)
	}

	// A closing editor is announced as gone.
	w.send(map[string]any{"t": "pres", "c": []float64{0, 0}})
	a.expect("pres")
	w.ws.Close(1000, "")
	g, ok := a.next(func(m map[string]any) bool { return m["t"] == "pres" && m["gone"] == true })
	if !ok || g["key"] != "browser:bob" {
		t.Fatalf("gone %v", g)
	}
}

func TestReconnectResendsWithoutDuplicates(t *testing.T) {
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	id1, id2 := eid(), eid()
	if m := a.batch(1, sticky(id1, "one", 0)); m["t"] != "ack" {
		t.Fatal(m)
	}
	first := a.expect("delta")
	watermark := int64(first["seq"].(float64))
	// Batch 2 is sent and applied, but the socket drops before the answer
	// arrives.
	a.send(map[string]any{"t": "ops", "n": 2, "ops": []any{sticky(id2, "two", 300)}})
	waitFor(t, func() bool { _, ok := b.alive()[id2]; return ok })
	a.ws.Close(1000, "")

	r := connect(t, b.priv, map[string]any{"editor": "alice", "session": a.session, "watermark": watermark,
		"pending": map[string]any{"count": 1, "first_n": 2}}, nil)
	if r.session != a.session {
		t.Fatalf("session %s, want %s", r.session, a.session)
	}
	d := r.expect("delta")
	if d["from"] != a.session+":2" {
		t.Fatalf("resent delta %v", d)
	}
	ack := r.batch(2, sticky(id2, "two", 300))
	if ack["t"] != "ack" || ack["seq"] != d["seq"] {
		t.Fatalf("replayed answer %v", ack)
	}
	if n := len(b.alive()); n != 2 {
		t.Fatalf("%d elements after the resend", n)
	}
	if !r.quiet(200*time.Millisecond, isDelta) {
		t.Fatal("a resent batch produced a new delta")
	}

	// The next number applies; a number past it is a protocol error.
	if m := r.batch(3, sticky(eid(), "three", 600)); m["t"] != "ack" {
		t.Fatal(m)
	}
	r.send(map[string]any{"t": "ops", "n": 9, "ops": []any{sticky(eid(), "x", 0)}})
	if f := r.expect("fatal"); f["code"] != engine.FatalProtocol {
		t.Fatal(f)
	}
}

func TestReceiptAndSessionExpiry(t *testing.T) {
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	if m := a.batch(1, sticky(eid(), "one", 0)); m["t"] != "ack" {
		t.Fatal(m)
	}
	b.apply(func(st *engine.State) engine.Changed { return st.TrimReceipts(0, b.clock.Now().Unix()) })
	a.send(map[string]any{"t": "ops", "n": 1, "ops": []any{sticky(eid(), "again", 0)}})
	if f := a.expect("fatal"); f["code"] != engine.FatalReceiptExpired {
		t.Fatal(f)
	}

	// A client with pending batches whose session is gone must stop.
	c := connectRaw(t, b, map[string]any{"editor": "alice", "session": "unknown", "pending": map[string]any{"count": 2, "first_n": 5}})
	if f := c.expect("fatal"); f["code"] != engine.FatalSessionExpired {
		t.Fatal(f)
	}
	// Without pending batches it gets a new session and a snapshot.
	n := connect(t, b.priv, map[string]any{"editor": "alice", "session": "unknown"}, nil)
	if n.session == "" || n.session == "unknown" {
		t.Fatalf("session %q", n.session)
	}
	n.snapshot()
}

// connectRaw says hello without waiting for a welcome.
func connectRaw(t *testing.T, b *tb, hello map[string]any) *client {
	c, err := dial(t, b.priv, origin(b.priv))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}}
	for k, v := range hello {
		m[k] = v
	}
	c.send(m)
	return c
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestPublicProjectionAndRefusals(t *testing.T) {
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice", "name": "Alice"}, nil)
	a.snapshot()
	id := eid()
	a.batch(1, sticky(id, "secret author", 0))

	p := connect(t, b.pub, map[string]any{"editor": "eve", "name": "Eve"}, map[string]string{"Tailscale-User-Login": "eve@example.com"})
	_, els, meta := p.snapshot()
	if len(els) != 1 {
		t.Fatalf("public snapshot %v", els)
	}
	for _, k := range []string{"by", "at", "inc", "v", "end_inc"} {
		if _, ok := els[0][k]; ok {
			t.Fatalf("public element has %s: %v", k, els[0])
		}
	}
	if _, ok := meta["v"]; ok {
		t.Fatalf("public meta %v", meta)
	}
	a.batch(2, map[string]any{"op": "set", "id": id, "inc": 0, "g": "text", "val": "changed"})
	d := p.expect("delta")
	raw := d["_raw"].(string)
	for _, k := range []string{`"from"`, `"by"`, `"reset"`, a.session, "Alice"} {
		if strings.Contains(raw, k) {
			t.Fatalf("public delta holds %s: %s", k, raw)
		}
	}
	if set := d["changes"].([]any)[0].(map[string]any)["set"].(map[string]any); set["text"] != "changed" {
		t.Fatalf("public set %v", set)
	}
	// Presence never reaches Public.
	a.send(map[string]any{"t": "pres", "c": []float64{1, 1}})
	if !p.quiet(200*time.Millisecond, func(m map[string]any) bool { return m["t"] == "pres" }) {
		t.Fatal("Public received presence")
	}
	// Writes, presence and shares on Public close the socket.
	for _, msg := range []map[string]any{
		{"t": "ops", "n": 1, "ops": []any{sticky(eid(), "x", 0)}},
		{"t": "pres", "c": []float64{1, 1}},
		{"t": "share", "ids": []string{id}},
		{"t": "ops_part", "n": 1, "part": 1, "parts": 2, "data": "["},
	} {
		q := connect(t, b.pub, nil, nil)
		q.snapshot()
		q.send(msg)
		if f := q.expect("fatal"); f["code"] != engine.FatalProtocol || !q.closed(2*time.Second) {
			t.Fatalf("%v: %v", msg["t"], f)
		}
	}
	if n := len(b.alive()); n != 1 {
		t.Fatalf("%d elements", n)
	}

	// History and past-only assets are not reachable on Public.
	resp, err := http.Get(b.pub.URL + "/_board/api/revisions")
	if err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("public revisions %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// Withdrawing Public access closes open public sockets at once.
	b.withdraw()
	if !p.closed(2 * time.Second) {
		t.Fatal("the public socket stayed open")
	}
}

func TestOriginChecks(t *testing.T) {
	b := newTB(t)
	for name, h := range map[string]map[string]string{
		"no origin":    {},
		"other origin": {"Origin": "http://evil.example"},
		"cross site":   {"Sec-Fetch-Site": "cross-site", "Origin": b.priv.URL},
	} {
		if _, err := dial(t, b.priv, h); err == nil || !strings.Contains(err.Error(), "403") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := dial(t, b.priv, map[string]string{"Sec-Fetch-Site": "same-origin"}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, b.priv.URL+"/_board/api/revisions/1/pin", strings.NewReader(`{"pinned":true}`))
	req.Header.Set("Origin", b.priv.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without X-Flats-Board: %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	resp2, _ := b.post(b.pub, "/_board/api/assets", "image/png", strings.NewReader("x"))
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("public upload %d", resp2.StatusCode)
	}
	// The shell carries the CSP.
	resp3, err := http.Get(b.pub.URL + "/")
	if err != nil || !strings.Contains(resp3.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("shell %v %v", resp3.Header, err)
	}
	resp3.Body.Close()
	resp4, err := http.Get(b.pub.URL + "/_board/app/tools/select.js")
	if err != nil || resp4.StatusCode != 200 || !strings.HasPrefix(resp4.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("app file %v %v", resp4.StatusCode, err)
	}
	resp4.Body.Close()
}
