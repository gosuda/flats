package board

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
)

// TestFiveEditorsEditingTwicePerSecond runs the success criterion's load
// over real sockets: 5 editors each send 2 batches per second, all on the
// same few elements and pipelined without waiting for acks (board.md
// Success criteria, Concurrent editing). Every batch is acknowledged, every
// editor receives the same deltas for the same contiguous seqs, and each
// element ends with the value of the last batch (by seq) that set it.
func TestFiveEditorsEditingTwicePerSecond(t *testing.T) {
	const editors, batches = 5, 10
	b := newTB(t)
	var cs []*client
	for i := 0; i < editors; i++ {
		c := connect(t, b.priv, map[string]any{"editor": fmt.Sprintf("ed%d", i)}, nil)
		c.snapshot()
		cs = append(cs, c)
	}
	shared := []string{eid(), eid(), eid()}
	var ops []map[string]any
	for i, id := range shared {
		ops = append(ops, sticky(id, "start", float64(i*300)))
	}
	ack := cs[0].batch(1, ops...)
	if ack["t"] != "ack" {
		t.Fatal(ack)
	}
	// The setup batch's seq is the shared elements' incarnation; the
	// other editors name it, as their copies learned it from the delta.
	start := int64(ack["seq"].(float64))
	for _, c := range cs {
		c.expect("delta")
	}

	type sent struct {
		id, val string
	}
	plans := make([]map[int]sent, editors)
	var wg sync.WaitGroup
	for i, c := range cs {
		plans[i] = map[int]sent{}
		rng := rand.New(rand.NewPCG(uint64(i), 7))
		first := 1
		if i == 0 {
			first = 2
		}
		for n := first; n < first+batches; n++ {
			id := shared[rng.IntN(len(shared))]
			plans[i][n] = sent{id, fmt.Sprintf("ed%d-%d", i, n)}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(500 * time.Millisecond)
			defer tick.Stop()
			for n := first; n < first+batches; n++ {
				p := plans[i][n]
				body, _ := json.Marshal(map[string]any{"t": "ops", "n": n, "ops": []any{map[string]any{"op": "set", "id": p.id, "inc": start, "g": "text", "val": p.val}}})
				if err := c.ws.WriteFrame(1, body); err != nil {
					t.Error(err)
					return
				}
				<-tick.C
			}
		}()
	}
	wg.Wait()

	// Collect each editor's acks and every delta up to the last seq.
	type got struct {
		acks   map[int]int64
		deltas map[int64]string
	}
	results := make([]got, editors)
	last := start
	for i, c := range cs {
		r := got{acks: map[int]int64{}, deltas: map[int64]string{}}
		for n := range plans[i] {
			m, ok := c.next(func(m map[string]any) bool {
				return (m["t"] == "ack" || m["t"] == "reject") && int(m["n"].(float64)) == n
			})
			if !ok {
				t.Fatalf("editor %d: no answer to batch %d", i, n)
			}
			if m["t"] != "ack" {
				t.Fatalf("editor %d batch %d refused: %v", i, n, m)
			}
			r.acks[n] = int64(m["seq"].(float64))
			last = max(last, r.acks[n])
		}
		results[i] = r
	}
	want := last - start
	if want != int64(editors*batches) {
		t.Fatalf("%d seqs after the setup for %d batches", want, editors*batches)
	}
	for i, c := range cs {
		for int64(len(results[i].deltas)) < want {
			d, ok := c.next(isDelta)
			if !ok {
				t.Fatalf("editor %d received %d of %d deltas", i, len(results[i].deltas), want)
			}
			seq := int64(d["seq"].(float64))
			if seq <= start {
				continue
			}
			if _, dup := results[i].deltas[seq]; dup {
				t.Fatalf("editor %d received seq %d twice", i, seq)
			}
			ch, _ := json.Marshal(d["changes"])
			results[i].deltas[seq] = string(ch)
		}
	}
	for seq := start + 1; seq <= last; seq++ {
		ref, ok := results[0].deltas[seq]
		if !ok {
			t.Fatalf("seq %d missing", seq)
		}
		for i := 1; i < editors; i++ {
			if results[i].deltas[seq] != ref {
				t.Fatalf("seq %d differs between editor 0 and %d:\n%s\n%s", seq, i, ref, results[i].deltas[seq])
			}
		}
	}
	// Each element holds the value of the highest-seq batch that set it.
	wantText := map[string]string{}
	best := map[string]int64{}
	for i := range cs {
		for n, p := range plans[i] {
			if s := results[i].acks[n]; s > best[p.id] {
				best[p.id], wantText[p.id] = s, p.val
			}
		}
	}
	alive := b.alive()
	for _, id := range shared {
		if alive[id] != wantText[id] {
			t.Fatalf("element %s = %q, want %q (seq %d)", id, alive[id], wantText[id], best[id])
		}
	}
	// A fresh screen shows the same.
	f := connect(t, b.priv, map[string]any{"editor": "late"}, nil)
	_, els, _ := f.snapshot()
	for _, e := range els {
		if id := e["id"].(string); wantText[id] != "" && e["text"] != wantText[id] {
			t.Fatalf("a fresh snapshot shows %s = %v", id, e["text"])
		}
	}
}

// TestCanvasNeedsNoOutsideService: the canvas app is served from the
// binary's embedded files and names no outside origin, and the shell's
// policy allows only its own (board.md Success criteria, Dependencies).
func TestCanvasNeedsNoOutsideService(t *testing.T) {
	url := regexp.MustCompile(`(?i)\b(https?:)?//[a-z0-9.-]+\.[a-z]{2,}`)
	err := fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.Contains(p, "testdata") {
			return err
		}
		data, err := fs.ReadFile(webFS, p)
		if err != nil {
			return err
		}
		for _, m := range url.FindAllString(string(data), -1) {
			if !strings.Contains(m, "www.w3.org") {
				t.Errorf("%s names an outside origin: %s", p, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b := newTB(t)
	resp, err := http.Get(b.priv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") && !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("shell policy %q", csp)
	}
	for _, part := range strings.Split(csp, ";") {
		if url.MatchString(part) {
			t.Fatalf("shell policy allows an outside origin: %q", csp)
		}
	}
}

// TestLargeFrameMoveDeltaArrivesInParts moves a frame carrying 6,000
// members over the socket: the delta is larger than one server frame, so
// the other editor receives it in parts that reassemble into every member's
// change under one seq (board-TECH 14 Deterministic counterexamples,
// splitting the delta of a large frame move).
func TestLargeFrameMoveDeltaArrivesInParts(t *testing.T) {
	ctx := context.Background()
	b := newTB(t)
	const members = 6000
	frame := eid()
	keys := model.Rekey(members + 1)
	els := []model.Element{{ID: frame, Type: model.TypeFrame, Geom: model.Geom{W: 100000, H: 100000}, Z: keys[0], Text: "F", Style: model.DefaultStyle(model.TypeFrame)}}
	for i := 0; i < members; i++ {
		els = append(els, model.Element{ID: eid(), Type: model.TypeSticky, Frame: frame, Z: keys[i+1],
			Geom: model.Geom{X: float64(i%100) * 210, Y: float64(i/100) * 210, W: 200, H: 200}, Style: model.DefaultStyle(model.TypeSticky)})
	}
	data := buildArchive(t, boardFile{Format: 1, Title: "Big", Grid: true}, els, nil, nil)
	err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.ImportBoard(ctx, ImportRequest{OpID: "big-frame-1", Mode: "replace", Archive: strings.NewReader(string(data)), Size: int64(len(data)), Label: "Agent"}, other)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var inc int64
	b.state(func(st *engine.State) {
		for _, e := range st.Alive() {
			if e.ID == frame {
				inc = e.Inc
			}
		}
	})
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	o := connect(t, b.priv, map[string]any{"editor": "olga"}, nil)
	o.snapshot()
	// A 6,001-change batch can take longer than the usual 5 seconds on a
	// loaded host or under the race detector.
	m := batchWithin(a, time.Minute, 1, map[string]any{"op": "set", "id": frame, "inc": inc, "g": "geom", "carry": "members",
		"val": map[string]any{"x": 500, "y": 0, "w": 100000, "h": 100000, "rot": 0}})
	if m["t"] != "ack" {
		t.Fatal(m)
	}
	var text strings.Builder
	parts := 0
	for {
		p := o.expect("delta_part")
		if p["seq"] != m["seq"] {
			t.Fatalf("delta part seq %v, ack seq %v", p["seq"], m["seq"])
		}
		if len(p["_raw"].(string)) > maxServerFrame {
			t.Fatalf("a delta part of %d bytes", len(p["_raw"].(string)))
		}
		parts++
		text.WriteString(p["data"].(string))
		if p["part"] == p["parts"] {
			break
		}
	}
	var d struct {
		Seq     float64          `json:"seq"`
		Changes []map[string]any `json:"changes"`
	}
	if err := json.Unmarshal([]byte(text.String()), &d); err != nil || len(d.Changes) != members+1 || d.Seq != m["seq"] {
		t.Fatalf("reassembled delta: %v, %d changes in %d parts", err, len(d.Changes), parts)
	}
	if parts < 2 {
		t.Fatalf("the delta came in %d part", parts)
	}
	want := map[string]float64{}
	for _, e := range els[1:] {
		want[e.ID] = e.Geom.X + 500
	}
	moved := 0
	b.state(func(st *engine.State) {
		for _, e := range st.Alive() {
			if x, ok := want[e.ID]; ok && e.Frame == frame && e.Geom.X == x {
				moved++
			}
		}
	})
	if moved != members {
		t.Fatalf("%d of %d members moved with the frame", moved, members)
	}
	t.Logf("%d changes in %d parts", len(d.Changes), parts)
}
