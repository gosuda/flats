package board

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
)

func TestBatchPartsAndLargeDeltas(t *testing.T) {
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	o := connect(t, b.priv, map[string]any{"editor": "olga"}, nil)
	o.snapshot()
	var ops []map[string]any
	for i := 0; i < 150; i++ {
		ops = append(ops, sticky(eid(), noise(3900, i), float64(i*210)))
	}
	data, _ := json.Marshal(ops)
	const part = 150 << 10
	parts := (len(data) + part - 1) / part
	for i := 0; i < parts; i++ {
		a.send(map[string]any{"t": "ops_part", "n": 1, "part": i + 1, "parts": parts, "data": string(data[i*part : min(len(data), (i+1)*part)])})
	}
	ack, ok := a.next(func(m map[string]any) bool { return m["t"] == "ack" || m["t"] == "reject" })
	if !ok || ack["t"] != "ack" {
		t.Fatalf("assembled batch %v", ack)
	}
	// The delta is larger than 512 KiB: it arrives in parts.
	var text strings.Builder
	var seq float64
	for {
		p := o.expect("delta_part")
		seq = p["seq"].(float64)
		if len(p["_raw"].(string)) > maxServerFrame {
			t.Fatalf("a delta part of %d bytes", len(p["_raw"].(string)))
		}
		text.WriteString(p["data"].(string))
		if p["part"] == p["parts"] {
			break
		}
	}
	var d struct {
		Seq     float64 `json:"seq"`
		Changes []any   `json:"changes"`
	}
	if err := json.Unmarshal([]byte(text.String()), &d); err != nil || len(d.Changes) != 150 || d.Seq != seq {
		t.Fatalf("reassembled delta: %v %d", err, len(d.Changes))
	}
	if strings.Contains(text.String(), a.session) {
		t.Fatal("the other editor's delta parts name the sender's session")
	}
	// Parts out of order are a protocol error.
	a.send(map[string]any{"t": "ops_part", "n": 2, "part": 1, "parts": 3, "data": "["})
	a.send(map[string]any{"t": "ops_part", "n": 2, "part": 3, "parts": 3, "data": "]"})
	if f := a.expect("fatal"); f["code"] != engine.FatalProtocol {
		t.Fatal(f)
	}
}

func TestSlowEditorSwitchesToSnapshot(t *testing.T) {
	b := newTB(t)
	slow, err := dial(t, b.priv, origin(b.priv))
	if err != nil {
		t.Fatal(err)
	}
	slow.send(map[string]any{"t": "hello", "v": 1, "pending": map[string]any{"count": 0}, "editor": "slow"})
	slow.expect("welcome")
	slow.snapshot()
	var sc *conn
	waitFor(t, func() bool {
		for _, c := range b.in.hub.list() {
			if c.key == "browser:slow" {
				sc = c
			}
		}
		return sc != nil
	})
	slow.gate.Lock() // the client stops reading
	// Once the socket buffers are full, the server's writer stalls on one
	// frame and drops the socket after its 10-second write deadline. With a
	// cap below two deltas (about 180 KB each), the delta after the stalled
	// one switches the socket to the snapshot long before that. The writes
	// come from the agent, so no other socket is under this cap.
	b.in.privateQueue = 256 << 10
	n := 1
	for sc.currentGen() == 0 && n < 80 {
		var ops []engine.AgentOp
		for i := 0; i < 60; i++ {
			el, _ := json.Marshal(map[string]any{"type": "sticky", "text": noise(3000, n*100+i), "x": n*100 + i, "y": 0})
			ops = append(ops, engine.AgentOp{Op: "add", El: el})
		}
		// UpdateBoard returns after the delta was delivered to the slow
		// socket, so the generation read next is final.
		if _, err := b.in.UpdateBoard(context.Background(), UpdateRequest{OpID: "slow-fill-" + strconv.Itoa(n), Label: "Agent", Ops: ops}, 0); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if sc.currentGen() == 0 {
		t.Fatal("no resync")
	}
	sc.mu.Lock()
	queued := sc.qBytes
	sc.mu.Unlock()
	if queued > b.in.privateQueue {
		t.Fatalf("queue %d after resync", queued)
	}
	slow.gate.Unlock()
	r := slow.expect("resync")
	if r["gen"] != 1.0 {
		t.Fatalf("resync %v", r)
	}
	for {
		m, ok := slow.next(func(m map[string]any) bool { return m["t"] == "snapshot" && m["gen"] == 1.0 })
		if !ok {
			t.Fatal("no snapshot after the resync")
		}
		if m["part"] == m["parts"] {
			break
		}
	}
}

func TestPagedReadsAreConsistent(t *testing.T) {
	b := newTB(t)
	ctx := context.Background()
	var ops []engine.AgentOp
	for i := 0; i < 1200; i++ {
		el, _ := json.Marshal(map[string]any{"type": "sticky", "text": "n", "x": i * 10, "y": 0})
		ops = append(ops, engine.AgentOp{Op: "add", El: el})
	}
	if _, err := b.in.UpdateBoard(ctx, UpdateRequest{OpID: "fill-board", Label: "Agent", Ops: ops[:1000]}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.in.UpdateBoard(ctx, UpdateRequest{OpID: "fill-board-2", Label: "Agent", Ops: ops[1000:]}, 0); err != nil {
		t.Fatal(err)
	}
	p1, err := b.in.GetBoard(ctx, Query{})
	if err != nil || len(p1.Elements) != 500 || p1.Next == "" {
		t.Fatalf("first page %d %q %v", len(p1.Elements), p1.Next, err)
	}
	// An edit between pages does not show up in the paged read.
	if _, err := b.in.UpdateBoard(ctx, UpdateRequest{OpID: "between-pages", Label: "Agent", Ops: ops[:1]}, 0); err != nil {
		t.Fatal(err)
	}
	total := len(p1.Elements)
	cur := p1.Next
	for cur != "" {
		p, err := b.in.GetBoard(ctx, Query{Cursor: cur})
		if err != nil || p.Seq != p1.Seq {
			t.Fatalf("page %v seq %d", err, p.Seq)
		}
		total += len(p.Elements)
		cur = p.Next
	}
	if total != 1200 {
		t.Fatalf("%d elements over the pages", total)
	}
	var e map[string]any
	json.Unmarshal(p1.Elements[0], &e)
	if e["ver"] == nil {
		t.Fatalf("element without ver %v", e)
	}
	_, err = b.in.GetBoard(ctx, Query{Cursor: p1.Next})
	var be *Error
	if !errors.As(err, &be) || be.Category != CategoryConflict {
		t.Fatalf("expired cursor: %v", err)
	}
	o, err := b.in.GetBoard(ctx, Query{Outline: true, Limit: 3})
	if err != nil || len(o.Elements) != 3 || strings.Contains(string(o.Elements[0]), `"x"`) {
		t.Fatalf("outline %s %v", o.Elements, err)
	}
}

func TestHistoryRoutesAndElementRecovery(t *testing.T) {
	b := newTB(t)
	ctx := context.Background()
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	img := pngBytes(t, 6, 6)
	_, up := b.post(b.priv, "/_board/api/assets", "image/png", bytes.NewReader(img))
	hash := up["hash"].(string)
	x, y, pic, conn := eid(), eid(), eid(), eid()
	a.batch(1, sticky(x, "x", 0), sticky(y, "y", 400),
		map[string]any{"op": "add", "refs": map[string]any{hash: 0}, "el": map[string]any{"id": pic, "type": "image", "x": 0, "y": 300, "w": 6, "h": 6, "rot": 0, "asset": hash, "natural": []int{6, 6}}},
		map[string]any{"op": "add", "refs": map[string]any{x: 0, y: 0}, "el": map[string]any{"id": conn, "type": "connector", "ends": []any{map[string]any{"el": x, "at": []float64{1, 0.5}}, map[string]any{"el": y, "at": []float64{0, 0.5}}}}})
	b.clock.Add(31 * time.Second)
	if err := b.in.do(ctx, b.in.tick); err != nil {
		t.Fatal(err)
	}
	get := func(path string) map[string]any {
		r, err := http.Get(b.priv.URL + path)
		if err != nil || r.StatusCode != 200 {
			t.Fatalf("GET %s: %v %v", path, r.StatusCode, err)
		}
		defer r.Body.Close()
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		return m
	}
	list := get("/_board/api/revisions")
	revs := list["revisions"].([]any)
	if len(revs) != 2 || list["count"] != 2.0 {
		t.Fatalf("revisions %v", list)
	}
	rev := int(revs[0].(map[string]any)["number"].(float64))
	els := get("/_board/api/revisions/" + itoa(rev) + "/elements")["elements"].([]any)
	if len(els) != 4 {
		t.Fatalf("revision elements %v", els)
	}
	resp, m := b.post(b.priv, "/_board/api/revisions/"+itoa(rev)+"/pin", "application/json", strings.NewReader(`{"pinned":true}`))
	if resp.StatusCode != 200 || m["pinned"] != true {
		t.Fatalf("pin %v", m)
	}

	// Delete everything, purge the deleted rows and sweep the asset row.
	inc := map[string]any{x: 0, y: 0, pic: 0, conn: 0}
	a.batch(2, map[string]any{"op": "delete", "ids": []string{x, y, pic, conn}, "inc_of": inc})
	b.clock.Add(2 * time.Hour)
	b.apply(func(st *engine.State) engine.Changed { return st.TrimDeleted(0) })
	b.apply(func(st *engine.State) engine.Changed { return st.SweepAssets(b.clock.Now().Unix()) })
	if _, ok := b.in.assets.get(hash); ok {
		t.Fatal("asset row survived")
	}

	// Rollback through the route restores the cleaned-up asset row from
	// the revision; after the protection ends a sweep keeps it.
	resp, m = b.post(b.priv, "/_board/api/revisions/"+itoa(rev)+"/rollback", "application/json", strings.NewReader(`{}`))
	if resp.StatusCode != 200 || m["rollback_of"] != float64(rev) {
		t.Fatalf("rollback %d %v", resp.StatusCode, m)
	}
	if got := b.alive(); len(got) != 4 || got[x] != "x" {
		t.Fatalf("after rollback %v", got)
	}
	b.clock.Add(2 * time.Hour)
	b.apply(func(st *engine.State) engine.Changed { return st.SweepAssets(b.clock.Now().Unix()) })
	if r, err := http.Get(b.pub.URL + "/_board/a/" + hash); err != nil || r.StatusCode != 200 {
		t.Fatalf("image after rollback and sweep %v %v", r.StatusCode, err)
	}

	// Element recovery copies x and the connector with new ids; the
	// connector's end at y (not copied) becomes free.
	resp, m = b.post(b.priv, "/_board/api/recover", "application/json", strings.NewReader(`{"revision":`+itoa(rev)+`,"ids":["`+x+`","`+conn+`"]}`))
	if resp.StatusCode != 200 {
		t.Fatalf("recover %d %v", resp.StatusCode, m)
	}
	ids := m["ids"].([]any)
	got := map[string]model.Element{}
	b.state(func(st *engine.State) {
		for _, e := range st.Alive() {
			got[e.ID] = e.Element
		}
	})
	nx, nc := got[ids[0].(string)], got[ids[1].(string)]
	if nx.Text != "x" || nx.ID == x || nc.Ends[0].El != nx.ID || nc.Ends[1].El != "" || nc.Ends[1].Pt != [2]float64{400, 100} {
		t.Fatalf("recovered %+v %+v", nx, nc)
	}
	d, ok := a.next(func(m map[string]any) bool {
		by, _ := m["by"].(map[string]any)
		return m["t"] == "delta" && by != nil && by["kind"] == "person" && m["from"] == nil
	})
	if !ok {
		t.Fatalf("no recovery delta credited to a person: %v", d)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestStorageSettings(t *testing.T) {
	b := newTB(t)
	var av, sync int
	b.in.do(context.Background(), func() error {
		b.in.db.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&av)
		b.in.db.db.QueryRow(`PRAGMA synchronous`).Scan(&sync)
		return nil
	})
	if av != 2 || sync != 2 {
		t.Fatalf("auto_vacuum %d synchronous %d", av, sync)
	}
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	for n := 1; n <= 40; n++ {
		a.batch(n, sticky(eid(), noise(2000, n), float64(n)))
	}
	// The log is truncated once it holds more than 64 frames.
	fi, err := os.Stat(filepath.Join(b.dir, "board", dbName+"-wal"))
	if err == nil && fi.Size() > 64*(b.in.db.pageSize+24)+32+2*(1<<20) {
		t.Fatalf("WAL is %d bytes", fi.Size())
	}
}

func TestReplacementWithPendingBatches(t *testing.T) {
	b := newTB(t)
	ctx := context.Background()
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	old, kept := eid(), eid()
	a.batch(1, sticky(old, "old", 0), sticky(kept, "kept", 300))
	keptEl := model.Element{ID: kept, Type: model.TypeSticky, Geom: model.Geom{X: 300, W: 200, H: 200}, Z: "V", Text: "kept", Style: model.DefaultStyle(model.TypeSticky)}
	data := buildArchive(t, boardFile{Format: 1, Title: "New", Grid: false}, []model.Element{keptEl}, nil, nil)
	err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.ImportBoard(ctx, ImportRequest{OpID: "replace-pending", Mode: "replace", Archive: bytes.NewReader(data), Size: int64(len(data)), Label: "Agent"}, other)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// A batch prepared before the replacement: the removed element is
	// ignored, the kept one (same incarnation) still applies.
	m := a.batch(2, map[string]any{"op": "set", "id": old, "inc": 0, "g": "text", "val": "late"},
		map[string]any{"op": "set", "id": kept, "inc": 0, "g": "text", "val": "late"})
	res := m["results"].([]any)
	if m["t"] != "ack" || res[0] != "void:gone" || res[1] != "ok" {
		t.Fatalf("pending batch after replacement %v", m)
	}
	if got := b.alive(); len(got) != 1 || got[kept] != "late" || b.in.st.Board().Text != "New" {
		t.Fatalf("after replacement %v", got)
	}
}

func TestCacheMissWhileTheFlatLockIsHeld(t *testing.T) {
	b := newTB(t)
	b.core.setQuota(1 << 30)
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	b.core.mu.Lock() // a rollback or import holds the flat lock
	defer b.core.mu.Unlock()
	b.clock.Add(2 * time.Minute)
	if m := a.settle(1, sticky(eid(), "while locked", 0)); m["t"] != "ack" {
		t.Fatalf("batch while the flat lock is held: %v", m)
	}
	// A failing measurement keeps refusing as transient, never permanently.
	b.core.data.Lock()
	b.core.failMeas = true
	b.core.data.Unlock()
	b.clock.Add(2 * time.Minute)
	if m := a.batch(2, sticky(eid(), "x", 0)); m["t"] != "reject" || m["retry"] != true {
		t.Fatalf("batch without a measurement: %v", m)
	}
}

func TestShutdownTellsCanvasesWhy(t *testing.T) {
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	p := connect(t, b.pub, nil, nil)
	p.snapshot()
	b.in.Shutdown(FatalDeleted)
	for _, c := range []*client{a, p} {
		if f := c.expect("fatal"); f["code"] != FatalDeleted || !c.closed(3*time.Second) {
			t.Fatalf("fatal %v", f)
		}
	}
	resp, err := http.Get(b.priv.URL + "/")
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("after stop %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()
}
