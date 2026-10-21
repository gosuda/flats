package board

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
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
			if me, _ := w["me"].(map[string]any); w["access"] != "public" || w["session"] != "" || me["key"] != nil || me["viewer"] == nil {
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
// in any delta, a rollback's included (board-TECH 7.3). It receives the
// add, the edit and the rollback as three consecutive public deltas whose
// changes, applied to its snapshot, give the board's public projection.
func TestPublicReceivesNoPresenceOrAuthors(t *testing.T) {
	ctx := context.Background()
	b := newTB(t)
	p := connect(t, b.pub, nil, nil)
	pseq, pels, _ := p.snapshot()
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
	var rollbackSeq int64
	b.state(func(st *engine.State) { rollbackSeq = st.Seq() })
	if rollbackSeq != pseq+3 {
		t.Fatalf("the rollback committed seq %d, want %d", rollbackSeq, pseq+3)
	}
	// A second editor joins and leaves.
	o := connect(t, b.priv, map[string]any{"editor": "olga", "name": "Olga"}, nil)
	o.snapshot()
	o.send(map[string]any{"t": "pres", "c": []float64{1, 1}})
	o.ws.Close(1000, "")
	a.expect("pres")

	var deltas []map[string]any
	timeout := time.After(10 * time.Second)
	var settle <-chan time.Time
collect:
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
			if err := publicLeak(raw, "alice@example.com", "Alice A", "Olga", a.session); err != nil {
				t.Fatalf("public %s: %v", m["t"], err)
			}
			if m["t"] == "delta" {
				deltas = append(deltas, m)
				if len(deltas) == 3 {
					// Wait a little longer for a late presence frame.
					settle = time.After(time.Second)
				}
			}
		case <-settle:
			break collect
		case <-timeout:
			break collect
		}
	}
	if len(deltas) != 3 {
		t.Fatalf("public saw %d deltas, want the add, the edit and the rollback", len(deltas))
	}
	view := newBoardView(true, pseq, pels)
	for _, d := range deltas {
		if err := view.apply(d); err != nil {
			t.Fatal(err)
		}
	}
	// The add, then the edit, as their public projections.
	if c := deltas[0]["changes"].([]any); len(c) != 1 || c[0].(map[string]any)["id"] != id ||
		c[0].(map[string]any)["add"] == nil || c[0].(map[string]any)["add"].(map[string]any)["text"] != "first" {
		t.Fatalf("public add delta %s", deltas[0]["_raw"])
	}
	if got, want := canonicalAny(deltas[1]["changes"]), canonicalAny([]any{map[string]any{"id": id, "set": map[string]any{"text": "second"}}}); got != want {
		t.Fatalf("public edit delta changes:\n got %s\nwant %s", got, want)
	}
	// The rollback: the next seq, one change restoring the text, as a set
	// of the text alone or the element's whole public form.
	rb := deltas[2]
	t.Logf("public rollback delta %s", rb["_raw"])
	if int64(rb["seq"].(float64)) != rollbackSeq {
		t.Fatalf("public rollback delta seq %v, want %d", rb["seq"], rollbackSeq)
	}
	rc := rb["changes"].([]any)
	if len(rc) != 1 {
		t.Fatalf("public rollback delta %s", rb["_raw"])
	}
	change := rc[0].(map[string]any)
	switch {
	case change["set"] != nil:
		if got, want := canonicalAny(change), canonicalAny(map[string]any{"id": id, "set": map[string]any{"text": "first"}}); got != want {
			t.Fatalf("public rollback change:\n got %s\nwant %s", got, want)
		}
	case change["revive"] != nil:
		var el model.PrivateElement
		b.state(func(st *engine.State) { el, _ = st.Get(id) })
		if got, want := canonicalAny(change), canonicalAny(map[string]any{"id": id, "revive": model.PublicElement(el.Element)}); got != want {
			t.Fatalf("public rollback change:\n got %s\nwant %s", got, want)
		}
	default:
		t.Fatalf("public rollback change %v", change)
	}
	// The public copy equals the board's public projection: the rolled
	// back text.
	want := map[string]map[string]any{}
	b.state(func(st *engine.State) {
		for _, e := range st.Alive() {
			var m map[string]any
			_ = json.Unmarshal([]byte(canonicalAny(model.PublicElement(e.Element))), &m)
			want[e.ID] = m
		}
	})
	if diff := boardDiff(want, view.els); diff != "" {
		t.Fatalf("the public copy differs from the public projection:\n%s", diff)
	}
	if view.els[id]["text"] != "first" {
		t.Fatalf("the public copy shows %v after the rollback", view.els[id]["text"])
	}
	// Every public delta has only the public fields; the check rejects a
	// rollback delta that kept its reset, author or versions.
	for _, d := range deltas {
		if err := publicDeltaFields(d); err != nil {
			t.Fatalf("public delta %s: %v", d["_raw"], err)
		}
	}
	for _, bad := range []func(m map[string]any){
		func(m map[string]any) { m["reset"] = map[string]any{"kind": "rollback"} },
		func(m map[string]any) { m["by"] = map[string]any{"kind": "system", "label": "rollback"} },
		func(m map[string]any) {
			m["changes"] = []any{map[string]any{"id": id, "inc": 1, "set": map[string]any{"text": []any{"first", 3}}}}
		},
		func(m map[string]any) {
			m["changes"] = []any{map[string]any{"id": id, "revive": map[string]any{"id": id, "text": "first", "by": "ts:alice@example.com"}}}
		},
	} {
		m := jsonClone(rb)
		bad(m)
		if publicDeltaFields(m) == nil {
			t.Fatalf("publicDeltaFields accepted %v", m)
		}
	}
}

// publicLeak reports a private field or value in a raw public frame.
func publicLeak(raw string, values ...string) error {
	for _, k := range append([]string{`"by"`, `"from"`, `"reset"`, `"session"`}, values...) {
		if k != "" && strings.Contains(raw, k) {
			return fmt.Errorf("holds %s: %s", k, raw)
		}
	}
	return nil
}

// publicDeltaFields checks a public delta message field by field: the frame
// has only t, gen, seq and changes; a change only id and one of add, revive,
// del (true) or set (bundle to bare value); an element only the public
// allow-list of model.PublicElement.
func publicDeltaFields(d map[string]any) error {
	for k := range d {
		if !slices.Contains([]string{"t", "gen", "seq", "changes", "_raw"}, k) {
			return fmt.Errorf("delta field %q", k)
		}
	}
	allowed := []string{"id", "type", "x", "y", "w", "h", "rot", "z", "frame", "group", "locked",
		"text", "style", "ends", "pts", "asset", "alt", "natural", "format", "title", "grid"}
	for _, raw := range d["changes"].([]any) {
		c := raw.(map[string]any)
		for k, v := range c {
			switch k {
			case "id":
			case "add", "revive":
				for f := range v.(map[string]any) {
					if !slices.Contains(allowed, f) {
						return fmt.Errorf("element field %q", f)
					}
				}
			case "del":
				if v != true {
					return fmt.Errorf("del %v", v)
				}
			case "set":
				// No public bundle value is an array: an array is the
				// private [val, v] form.
				for g, val := range v.(map[string]any) {
					if _, ok := val.([]any); ok {
						return fmt.Errorf("set %s carries a version: %v", g, val)
					}
				}
			default:
				return fmt.Errorf("change field %q", k)
			}
		}
	}
	return nil
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
