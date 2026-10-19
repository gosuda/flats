package board

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
)

// TestPublicReconnectStartsFromASnapshot: a Public connection never reads
// back through the delta log, whatever session or watermark its hello
// names (board-TECH 7.3), and it gets no session of its own.
func TestPublicReconnectStartsFromASnapshot(t *testing.T) {
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice", "name": "Alice"}, nil)
	a.snapshot()
	id := eid()
	a.batch(1, sticky(id, "one", 0))
	first := a.expect("delta")
	a.batch(2, map[string]any{"op": "set", "id": id, "inc": 0, "g": "text", "val": "two"})
	second := a.expect("delta")
	watermark := first["seq"]

	for name, hello := range map[string]map[string]any{
		"own watermark":            {"watermark": watermark},
		"an editor's session":      {"session": a.session, "watermark": watermark, "editor": "alice"},
		"pending batches":          {"session": a.session, "watermark": watermark, "pending": map[string]any{"count": 1, "first_n": 3}},
		"watermark at the current": {"watermark": second["seq"]},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := dial(t, b.pub, origin(b.pub))
			if err != nil {
				t.Fatal(err)
			}
			m := map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}}
			for k, v := range hello {
				m[k] = v
			}
			p.send(m)
			w := p.expect("welcome")
			if w["access"] != "public" || w["session"] != "" || w["me"] != nil {
				t.Fatalf("public welcome %v", w)
			}
			// The first message after the welcome is a snapshot, never a
			// delta from the log.
			msg, ok := p.next(func(m map[string]any) bool { return m["t"] == "snapshot" || m["t"] == "delta" })
			if !ok || msg["t"] != "snapshot" {
				t.Fatalf("first message after a public hello %v", msg)
			}
			p.backlog = append([]map[string]any{msg}, p.backlog...)
			seq, els, _ := p.snapshot()
			if seq != int64(second["seq"].(float64)) || len(els) != 1 || els[0]["text"] != "two" {
				t.Fatalf("public snapshot seq %d %v", seq, els)
			}
			if !p.quiet(200*time.Millisecond, isDelta) {
				t.Fatal("a public reconnect received a past delta")
			}
		})
	}
}

// TestPublicRevisionRoutesAndWrites: every revision route is 404 on Public
// (GET and POST, with the canvas headers), and the other write routes are
// refused there with nothing changed (board-TECH 14 Security).
func TestPublicRevisionRoutesAndWrites(t *testing.T) {
	ctx := context.Background()
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	id := eid()
	a.batch(1, sticky(id, "x", 0))
	b.clock.Add(31 * time.Second)
	if err := b.in.do(ctx, b.in.tick); err != nil {
		t.Fatal(err)
	}
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	rev := itoa(b.core.lastRev())
	for _, path := range []string{"/_board/api/revisions", "/_board/api/revisions/" + rev, "/_board/api/revisions/" + rev + "/elements", "/_board/api/revisions/1"} {
		r, err := http.Get(b.pub.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("public GET %s: %d", path, r.StatusCode)
		}
		// The same route answers on Private, so the 404 is the boundary.
		r, err = http.Get(b.priv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("private GET %s: %d", path, r.StatusCode)
		}
	}
	pinned := func() bool {
		r, _ := b.core.rev(b.core.lastRev())
		return r.info.Pinned
	}
	for _, path := range []string{"/_board/api/revisions/" + rev + "/pin", "/_board/api/revisions/" + rev + "/rollback"} {
		resp, _ := b.post(b.pub, path, "application/json", strings.NewReader(`{"pinned":true}`))
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("public POST %s: %d", path, resp.StatusCode)
		}
	}
	if pinned() || len(revKinds(b.core)) != 2 {
		t.Fatalf("a public revision POST changed history: pinned %v, revisions %v", pinned(), revKinds(b.core))
	}
	var seq int64
	b.state(func(st *engine.State) { seq = st.Seq() })
	blobs := countBlobs(b.core)
	for path, body := range map[string]string{
		"/_board/api/recover": `{"revision":` + rev + `,"ids":["` + id + `"]}`,
		"/_board/api/import":  "",
		"/_board/api/assets":  string(pngBytes(t, 2, 2)),
	} {
		resp, _ := b.post(b.pub, path, "application/octet-stream", strings.NewReader(body))
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
			t.Errorf("public POST %s: %d", path, resp.StatusCode)
		}
	}
	var seqAfter int64
	b.state(func(st *engine.State) { seqAfter = st.Seq() })
	if seqAfter != seq || len(b.alive()) != 1 || countBlobs(b.core) != blobs {
		t.Fatal("a public write route changed the board")
	}
}

func countBlobs(f *fakeCore) int {
	f.data.Lock()
	defer f.data.Unlock()
	return len(f.blobs)
}

// TestPublicPastOnlyAndHeldAssets: Public reaches only assets alive
// elements reference. An asset that only a past revision holds, and an
// upload no element references yet, are 404 there while Private reaches
// them (board-TECH 7.2, 14 Security).
func TestPublicPastOnlyAndHeldAssets(t *testing.T) {
	ctx := context.Background()
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	status := func(srv string, hash string) int {
		r, err := http.Get(srv + "/_board/a/" + hash)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}

	// Held only: uploaded, no element yet.
	_, up := b.post(b.priv, "/_board/api/assets", "image/png", bytes.NewReader(pngBytes(t, 3, 3)))
	held := up["hash"].(string)
	if got := status(b.pub.URL, held); got != http.StatusNotFound {
		t.Errorf("public held-only asset %d", got)
	}
	if got := status(b.priv.URL, held); got != http.StatusOK {
		t.Errorf("private held-only asset %d", got)
	}

	// Past only: referenced in a revision, then deleted and its row
	// cleaned up.
	_, up = b.post(b.priv, "/_board/api/assets", "image/png", bytes.NewReader(pngBytes(t, 5, 5)))
	past := up["hash"].(string)
	pic := eid()
	a.batch(1, map[string]any{"op": "add", "refs": map[string]any{past: 0}, "el": map[string]any{"id": pic, "type": "image", "x": 0, "y": 0, "w": 5, "h": 5, "rot": 0, "asset": past, "natural": []int{5, 5}}})
	if got := status(b.pub.URL, past); got != http.StatusOK {
		t.Fatalf("public live asset %d", got)
	}
	b.clock.Add(31 * time.Second)
	if err := b.in.do(ctx, b.in.tick); err != nil {
		t.Fatal(err)
	}
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	a.batch(2, map[string]any{"op": "delete", "ids": []string{pic}, "inc_of": map[string]any{pic: 0}})
	b.clock.Add(2 * time.Hour)
	b.apply(func(st *engine.State) engine.Changed { return st.TrimDeleted(0) })
	b.apply(func(st *engine.State) engine.Changed { return st.SweepAssets(b.clock.Now().Unix()) })
	if _, ok := b.in.assets.get(past); ok {
		t.Fatal("the asset row survived")
	}
	if got := status(b.pub.URL, past); got != http.StatusNotFound {
		t.Errorf("public past-only asset %d", got)
	}
	if got := status(b.priv.URL, past); got != http.StatusOK {
		t.Errorf("private past-only asset %d", got)
	}
	// A malformed or unknown hash is 404 on both.
	for _, h := range []string{"x", strings.Repeat("0", 64)} {
		if got := status(b.pub.URL, h); got != http.StatusNotFound {
			t.Errorf("public %q %d", h, got)
		}
	}
}

// TestPublicReceivesNoPresenceOrAuthors: a Public socket gets no presence
// frame (updates, joins or leaves), and no author, session or reset field
// in any delta, a rollback's included (board-TECH 7.3).
func TestPublicReceivesNoPresenceOrAuthors(t *testing.T) {
	ctx := context.Background()
	b := newTB(t)
	p := connect(t, b.pub, nil, nil)
	p.snapshot()
	a := connect(t, b.priv, map[string]any{"editor": "alice", "name": "Alice"}, map[string]string{"Tailscale-User-Login": "alice@example.com", "Tailscale-User-Name": "Alice A"})
	a.snapshot()
	a.send(map[string]any{"t": "pres", "c": []float64{10, 10}, "sel": []string{}})
	id := eid()
	a.batch(1, sticky(id, "first", 0))
	b.clock.Add(31 * time.Second)
	if err := b.in.do(ctx, b.in.tick); err != nil {
		t.Fatal(err)
	}
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	a.batch(2, map[string]any{"op": "set", "id": id, "inc": 0, "g": "text", "val": "second"})
	if _, err := b.core.Rollback(ctx, b.core.lastRev(), "Alice A"); err != nil {
		t.Fatal(err)
	}
	// A second editor joins and leaves.
	o := connect(t, b.priv, map[string]any{"editor": "olga", "name": "Olga"}, nil)
	o.snapshot()
	o.send(map[string]any{"t": "pres", "c": []float64{1, 1}})
	o.ws.Close(1000, "")
	a.expect("pres")

	deadline := time.After(time.Second)
	seen := 0
	for {
		select {
		case m, ok := <-p.msgs:
			if !ok {
				t.Fatal("the public socket closed")
			}
			raw := m["_raw"].(string)
			if m["t"] == "pres" {
				t.Fatalf("public received presence %s", raw)
			}
			for _, k := range []string{`"by"`, `"from"`, `"reset"`, `"session"`, "alice@example.com", "Alice A", "Olga", a.session} {
				if strings.Contains(raw, k) {
					t.Fatalf("public %s holds %s: %s", m["t"], k, raw)
				}
			}
			if m["t"] == "delta" {
				seen++
			}
		case <-deadline:
			if seen < 2 {
				t.Fatalf("public saw %d deltas, want the edits and the rollback", seen)
			}
			return
		}
	}
}

// TestOriginMismatchOnEveryRoute: the canvas socket on both routes and
// the write routes refuse a request whose origin is not the board's own
// (board-TECH 14 Security).
func TestOriginMismatchOnEveryRoute(t *testing.T) {
	b := newTB(t)
	for name, h := range map[string]map[string]string{
		"no origin":         {},
		"other origin":      {"Origin": "http://evil.example"},
		"private on public": {"Origin": b.priv.URL},
		"cross site":        {"Sec-Fetch-Site": "cross-site", "Origin": b.pub.URL},
	} {
		if _, err := dial(t, b.pub, h); err == nil || !strings.Contains(err.Error(), "403") {
			t.Errorf("public socket, %s: %v", name, err)
		}
	}
	// Private write routes with the canvas header but a foreign origin.
	for _, path := range []string{"/_board/api/assets", "/_board/api/recover", "/_board/api/import", "/_board/api/revisions/1/pin", "/_board/api/revisions/1/rollback"} {
		for name, h := range map[string]map[string]string{
			"other origin":  {"Origin": "http://evil.example"},
			"no origin":     {},
			"cross site":    {"Origin": b.priv.URL, "Sec-Fetch-Site": "cross-site"},
			"public origin": {"Origin": b.pub.URL},
		} {
			req, _ := http.NewRequest(http.MethodPost, b.priv.URL+path, strings.NewReader(`{}`))
			req.Header.Set("X-Flats-Board", "1")
			for k, v := range h {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("POST %s, %s: %d", path, name, resp.StatusCode)
			}
		}
	}
}
