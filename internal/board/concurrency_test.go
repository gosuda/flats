package board

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"net/http"
	"regexp"
	"slices"
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
// editor receives, for each contiguous seq, the one change its batch made,
// and each element ends with the value of the last batch (by seq) that set
// it: on the server, on every editor's copy (its snapshot with the deltas
// applied) and in a fresh snapshot.
func TestFiveEditorsEditingTwicePerSecond(t *testing.T) {
	const editors, batches = 5, 10
	b := newTB(t)
	var cs []*client
	var views []*boardView
	for i := 0; i < editors; i++ {
		c := connect(t, b.priv, map[string]any{"editor": fmt.Sprintf("ed%d", i)}, nil)
		seq, els, _ := c.snapshot()
		cs = append(cs, c)
		views = append(views, newBoardView(false, seq, els))
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
	for i, c := range cs {
		if err := views[i].apply(c.expect("delta")); err != nil {
			t.Fatalf("editor %d setup delta: %v", i, err)
		}
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
		full   map[int64]map[string]any
	}
	results := make([]got, editors)
	last := start
	for i, c := range cs {
		r := got{acks: map[int]int64{}, deltas: map[int64]string{}, full: map[int64]map[string]any{}}
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
			results[i].full[seq] = d
		}
	}
	// Each seq carries exactly the change of the batch acknowledged with
	// it: that batch's element, at the setup incarnation, with its text
	// at version seq.
	bySeq := map[int64]sent{}
	for i := range cs {
		for n, p := range plans[i] {
			s := results[i].acks[n]
			if _, dup := bySeq[s]; dup {
				t.Fatalf("seq %d acknowledged twice", s)
			}
			bySeq[s] = p
		}
	}
	expectedChanges := func(seq int64) string {
		p := bySeq[seq]
		return canonicalAny([]any{map[string]any{"id": p.id, "inc": start, "set": map[string]any{"text": []any{p.val, seq}}}})
	}
	for i := range cs {
		for seq := start + 1; seq <= last; seq++ {
			if got, want := canonicalAny(results[i].full[seq]["changes"]), expectedChanges(seq); got != want {
				t.Fatalf("editor %d seq %d changes:\n got %s\nwant %s", i, seq, got, want)
			}
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
	if len(alive) != len(shared) {
		t.Fatalf("%d elements alive, want %d", len(alive), len(shared))
	}
	for _, id := range shared {
		if alive[id] != wantText[id] {
			t.Fatalf("element %s = %q, want %q (seq %d)", id, alive[id], wantText[id], best[id])
		}
	}
	server := serverBoard(b)
	// Every editor's copy, its snapshot with the received deltas applied in
	// seq order, equals the server's board.
	for i := range cs {
		for seq := start + 1; seq <= last; seq++ {
			if err := views[i].apply(results[i].full[seq]); err != nil {
				t.Fatalf("editor %d: %v", i, err)
			}
		}
		if diff := boardDiff(server, views[i].els); diff != "" {
			t.Fatalf("editor %d's copy differs from the server:\n%s", i, diff)
		}
		for _, id := range shared {
			if views[i].els[id]["text"] != wantText[id] {
				t.Fatalf("editor %d shows %s = %v, want %q", i, id, views[i].els[id]["text"], wantText[id])
			}
		}
	}
	// A fresh screen shows the same board: every element, none missing.
	f := connect(t, b.priv, map[string]any{"editor": "late"}, nil)
	fseq, els, _ := f.snapshot()
	if fseq != last {
		t.Fatalf("a fresh snapshot at seq %d, want %d", fseq, last)
	}
	fresh := viewOf(els)
	if diff := boardDiff(server, fresh); diff != "" {
		t.Fatalf("a fresh snapshot differs from the server:\n%s", diff)
	}
	// The comparison rejects a snapshot that lacks an element or shows an
	// older text.
	short := maps.Clone(fresh)
	delete(short, shared[0])
	if boardDiff(server, short) == "" {
		t.Fatal("boardDiff accepted a snapshot without an element")
	}
	stale := maps.Clone(fresh)
	stale[shared[1]] = jsonClone(fresh[shared[1]])
	stale[shared[1]]["text"] = "an older text"
	if boardDiff(server, stale) == "" {
		t.Fatal("boardDiff accepted a stale text")
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
	oseq, oels, _ := o.snapshot()
	if len(oels) != members+1 {
		t.Fatalf("olga's snapshot holds %d elements, want %d", len(oels), members+1)
	}
	olga := newBoardView(false, oseq, oels)
	before := viewOf(oels)
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
	// The changes are exactly one geometry set per element, the frame and
	// every member moved 500 to the right, at their incarnations with
	// version seq: no element twice, none missing, no stale geometry.
	seq := int64(d.Seq)
	wantChanges := map[string]string{}
	for _, e := range els {
		g := e.Geom
		if e.ID != frame {
			g.X += 500
		} else {
			g = model.Geom{X: 500, W: 100000, H: 100000}
		}
		wantChanges[e.ID] = canonicalAny(map[string]any{"id": e.ID, "inc": before[e.ID]["inc"], "set": map[string]any{"geom": []any{g, seq}}})
	}
	checkChanges := func(changes []map[string]any) error {
		seen := map[string]bool{}
		for _, c := range changes {
			id, _ := c["id"].(string)
			if seen[id] {
				return fmt.Errorf("%s changes twice", id)
			}
			seen[id] = true
			w, ok := wantChanges[id]
			if !ok {
				return fmt.Errorf("unexpected change %v", c)
			}
			if got := canonicalAny(c); got != w {
				return fmt.Errorf("change of %s:\n got %s\nwant %s", id, got, w)
			}
		}
		if len(seen) != len(wantChanges) {
			return fmt.Errorf("%d of %d elements changed", len(seen), len(wantChanges))
		}
		return nil
	}
	if err := checkChanges(d.Changes); err != nil {
		t.Fatal(err)
	}
	// The check rejects a duplicated member change and one with stale
	// geometry.
	dup := append(slices.Clone(d.Changes), d.Changes[1])
	if checkChanges(dup) == nil {
		t.Fatal("checkChanges accepted a duplicate member change")
	}
	stale := slices.Clone(d.Changes)
	stale[1] = jsonClone(stale[1])
	old := before[stale[1]["id"].(string)]
	stale[1]["set"] = map[string]any{"geom": []any{map[string]any{"x": old["x"], "y": old["y"], "w": old["w"], "h": old["h"], "rot": old["rot"]}, seq}}
	if checkChanges(stale) == nil {
		t.Fatal("checkChanges accepted stale member geometry")
	}
	// Olga's copy, her snapshot with the reassembled delta applied, equals
	// the server's board.
	var dm map[string]any
	if err := json.Unmarshal([]byte(text.String()), &dm); err != nil {
		t.Fatal(err)
	}
	if err := olga.apply(dm); err != nil {
		t.Fatal(err)
	}
	server := serverBoard(b)
	if diff := boardDiff(server, olga.els); diff != "" {
		t.Fatalf("olga's copy differs from the server:\n%s", diff)
	}
	for _, e := range els[1:] {
		if x := olga.els[e.ID]["x"]; x != e.Geom.X+500 {
			t.Fatalf("olga shows member %s at x %v, want %v", e.ID, x, e.Geom.X+500)
		}
	}
	// A copy that kept one member's old geometry is not the server's.
	kept := maps.Clone(olga.els)
	kept[els[1].ID] = jsonClone(olga.els[els[1].ID])
	kept[els[1].ID]["x"] = before[els[1].ID]["x"]
	if boardDiff(server, kept) == "" {
		t.Fatal("boardDiff accepted a member's stale geometry")
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

// boardView is a test-side copy of the board as a client keeps it: the
// elements of a snapshot, changed by each later delta in seq order. apply
// follows the client's rules (web/state.js applyDelta, withBundle) on the
// engine's wire forms (model.Change, model.PublicDelta): add and revive
// install the element, del removes it, and set writes each bundle and, for
// a private copy, its version and the change's incarnation.
type boardView struct {
	public bool
	seq    int64
	els    map[string]map[string]any
	meta   map[string]any
}

func newBoardView(public bool, seq int64, els []map[string]any) *boardView {
	v := &boardView{public: public, seq: seq, els: map[string]map[string]any{}, meta: map[string]any{}}
	for _, e := range els {
		v.els[e["id"].(string)] = jsonClone(e)
	}
	return v
}

// jsonClone deep-copies a decoded JSON object.
func jsonClone(m map[string]any) map[string]any {
	var out map[string]any
	data, _ := json.Marshal(m)
	_ = json.Unmarshal(data, &out)
	return out
}

// apply applies one delta message (or a reassembled delta), which must be
// the next seq.
func (v *boardView) apply(d map[string]any) error {
	seq := int64(d["seq"].(float64))
	if seq != v.seq+1 {
		return fmt.Errorf("delta seq %d after watermark %d", seq, v.seq)
	}
	changes, _ := d["changes"].([]any)
	for _, raw := range changes {
		c := raw.(map[string]any)
		id := c["id"].(string)
		if id == model.BoardID {
			set, _ := c["set"].(map[string]any)
			for g, e := range set {
				if g == model.BundleTitle || g == model.BundleGrid {
					v.meta[g] = v.value(e)
				}
			}
			continue
		}
		switch {
		case c["add"] != nil:
			v.els[id] = jsonClone(c["add"].(map[string]any))
		case c["revive"] != nil:
			v.els[id] = jsonClone(c["revive"].(map[string]any))
		case c["del"] != nil:
			delete(v.els, id)
		case c["set"] != nil:
			prev, ok := v.els[id]
			if !ok {
				continue // the client ignores a set of an element it lacks
			}
			el := jsonClone(prev)
			if inc, ok := c["inc"]; ok && !v.public {
				el["inc"] = inc
			}
			for g, e := range c["set"].(map[string]any) {
				withBundleTest(el, g, v.value(e))
				if !v.public {
					vs, _ := el["v"].(map[string]any)
					if vs == nil {
						vs = map[string]any{}
					}
					vs[g] = e.([]any)[1]
					el["v"] = vs
				}
			}
			v.els[id] = el
		default:
			return fmt.Errorf("change %v has no add, revive, del or set", c)
		}
	}
	v.seq = seq
	return nil
}

// value is a set entry's value: [val, v] on a private socket, val on Public.
func (v *boardView) value(e any) any {
	if v.public {
		return e
	}
	return e.([]any)[0]
}

// withBundleTest sets bundle g of el to val as web/schema.js withBundle
// does. An empty membership (null) is dropped, as the content form omits it.
func withBundleTest(el map[string]any, g string, val any) {
	switch {
	case g == model.BundleGeom:
		geom := val.(map[string]any)
		for _, k := range []string{"x", "y", "w", "h", "rot"} {
			el[k] = geom[k]
		}
	case g == model.BundleEnd0 || g == model.BundleEnd1:
		ends := slices.Clone(el["ends"].([]any))
		ends[model.EndIndex(g)] = val
		el["ends"] = ends
	case strings.HasPrefix(g, model.StylePrefix):
		style, _ := el["style"].(map[string]any)
		style = maps.Clone(style)
		if style == nil {
			style = map[string]any{}
		}
		style[strings.TrimPrefix(g, model.StylePrefix)] = val
		el["style"] = style
	case val == nil:
		delete(el, g)
	default:
		el[g] = val
	}
}

// serverBoard is the authoritative board as a private client receives it:
// every alive element's private wire form, by id.
func serverBoard(b *tb) map[string]map[string]any {
	out := map[string]map[string]any{}
	b.state(func(st *engine.State) {
		for _, e := range st.Alive() {
			data, err := json.Marshal(e)
			if err != nil {
				b.t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				b.t.Fatal(err)
			}
			out[e.ID] = m
		}
	})
	return out
}

// boardDiff describes how got differs from want, or is empty when every
// element is present in both with equal canonical JSON.
func boardDiff(want, got map[string]map[string]any) string {
	var out []string
	for _, id := range sortedKeys(want) {
		g, ok := got[id]
		if !ok {
			out = append(out, "missing "+id)
			continue
		}
		if w, gs := string(model.Canonical(want[id])), string(model.Canonical(g)); w != gs {
			out = append(out, fmt.Sprintf("%s:\n got %s\nwant %s", id, gs, w))
		}
	}
	for _, id := range sortedKeys(got) {
		if _, ok := want[id]; !ok {
			out = append(out, "unexpected "+id)
		}
	}
	if len(out) > 6 {
		out = append(out[:6], fmt.Sprintf("… %d differences in all", len(out)))
	}
	return strings.Join(out, "\n")
}

// viewOf is a board of decoded snapshot elements by id.
func viewOf(els []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, e := range els {
		out[e["id"].(string)] = e
	}
	return out
}

// canonicalAny is the canonical JSON of v after a JSON round trip, so Go
// values and decoded messages compare alike.
func canonicalAny(v any) string {
	data, _ := json.Marshal(v)
	var x any
	_ = json.Unmarshal(data, &x)
	return string(model.Canonical(x))
}
