package board

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
)

// TestFiveEditorsUnderSustainedLoad runs the success criterion's load for
// as long as BOARD_LOAD says (for example BOARD_LOAD=10m; the test is
// skipped without it): 5 editors each send 2 batches per second over real
// sockets, pipelined without waiting for acks (board.md Success criteria,
// Concurrent editing). Each editor keeps its own copy of the board (its
// snapshot with every delta applied in seq order, starting over at each
// resync snapshot) and prepares every batch from that copy: stickies, frames
// with members, text and moves (frames carry their members, groups move
// together), deletions, groups, ungrouping, membership changes, undo and
// redo. Meanwhile edit groups close into revisions and the board is rolled
// back to earlier revisions now and then.
//
// Every batch is answered, and a session's acks come in seq order. Each
// editor keeps the rate within one batch of its schedule, with no gap over
// two seconds. Every rollback lands between edits acknowledged within two
// seconds before and after it. A batch that changed the board came back in
// the delta of its ack's seq, and at that seq the board shows what each
// applied operation asked for (the added element, the text or position
// sent, the deletion, the group, the membership). At every seq two editors both
// reached, their copies are the same board. At the end every copy and a
// fresh snapshot equal the server's board, and each kind of operation was
// applied at least once.
func TestFiveEditorsUnderSustainedLoad(t *testing.T) {
	v := os.Getenv("BOARD_LOAD")
	if v == "" {
		t.Skip("set BOARD_LOAD to a duration, for example BOARD_LOAD=10m, to run")
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		t.Fatalf("BOARD_LOAD=%q is not a duration", v)
	}
	const editors = 5
	ctx := context.Background()
	b := newTB(t)
	eds := make([]*loadEditor, editors)
	for i := range eds {
		c := connect(t, b.priv, map[string]any{"editor": fmt.Sprintf("load%d", i)}, nil)
		var els []map[string]any
		for {
			m := c.expect("snapshot")
			for _, el := range m["elements"].([]any) {
				els = append(els, el.(map[string]any))
			}
			if m["part"] == m["parts"] {
				eds[i] = newLoadEditor(i, c, newBoardView(false, int64(m["seq"].(float64)), els))
				eds[i].gen = m["gen"].(float64)
				break
			}
		}
	}

	start := time.Now()
	until := start.Add(d)
	var idle sync.WaitGroup
	var done sync.WaitGroup
	final := make([]chan int64, editors)
	for i, e := range eds {
		final[i] = make(chan int64, 1)
		idle.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			if err := e.run(start, until, &idle, final[i]); err != nil {
				t.Errorf("editor %d: %v", i, err)
			}
		}()
	}

	// Edit groups close into revisions (the clock jumps past the idle
	// limit), core imports them, and every other time the board is rolled
	// back to one of them while the editors keep editing.
	stop := make(chan struct{})
	var rollbacks []time.Time
	var roller sync.WaitGroup
	roller.Go(func() {
		every := min(20*time.Second, d/6)
		tick := time.NewTicker(every)
		defer tick.Stop()
		rng := rand.New(rand.NewPCG(9, 9))
		for k := 0; ; k++ {
			select {
			case <-tick.C:
			case <-stop:
				return
			}
			b.clock.Add(31 * time.Second)
			if err := b.in.do(ctx, b.in.tick); err != nil {
				t.Errorf("tick: %v", err)
				return
			}
			if err := b.core.importOutbox(ctx); err != nil {
				t.Errorf("import: %v", err)
				return
			}
			// Roll back only while the editors are still sending.
			if last := b.core.lastRev(); k%2 == 1 && last > 0 && time.Until(until) > 3*time.Second {
				if _, err := b.core.Rollback(ctx, 1+rng.IntN(last), "Load"); err != nil {
					t.Errorf("rollback: %v", err)
					return
				}
				rollbacks = append(rollbacks, time.Now())
			}
		}
	})

	idle.Wait()
	close(stop)
	roller.Wait()
	// Every batch is answered and no more changes come: each editor reads
	// until its copy reaches the server's seq.
	var last int64
	b.state(func(st *engine.State) { last = st.Seq() })
	for _, f := range final {
		f <- last
	}
	done.Wait()
	if t.Failed() {
		t.FailNow()
	}

	server := serverBoard(b)
	totals := map[string]int{}
	batches, effects, unheld := 0, 0, 0
	for i, e := range eds {
		if e.view.seq != last {
			t.Fatalf("editor %d ended at seq %d, server at %d", i, e.view.seq, last)
		}
		if diff := boardDiff(server, e.view.els); diff != "" {
			t.Fatalf("editor %d's copy differs from the server:\n%s", i, diff)
		}
		n, k, err := e.checkEffects()
		if err != nil {
			t.Fatalf("editor %d: %v", i, err)
		}
		effects, unheld = effects+n, unheld+k
		if err := e.checkRate(d); err != nil {
			t.Fatalf("editor %d: %v", i, err)
		}
		for k, n := range e.counts {
			totals[k] += n
		}
		batches += e.n
	}
	compared, err := sameBoardAtEachSeq(eds)
	if err != nil {
		t.Fatal(err)
	}
	f := connect(t, b.priv, map[string]any{"editor": "late"}, nil)
	fseq, els, _ := f.snapshot()
	if fseq != last {
		t.Fatalf("a fresh snapshot at seq %d, want %d", fseq, last)
	}
	if diff := boardDiff(server, viewOf(els)); diff != "" {
		t.Fatalf("a fresh snapshot differs from the server:\n%s", diff)
	}
	if err := rollbacksBetweenEdits(rollbacks, eds); err != nil {
		t.Fatal(err)
	}
	totals["rollback"] = len(rollbacks)
	for _, k := range []string{"add sticky", "add frame", "add member", "text", "move", "move frame", "move group", "delete", "group", "ungroup", "reparent", "undo", "redo", "rollback"} {
		if totals[k] == 0 {
			t.Errorf("no %q was applied under load (%v)", k, totals)
		}
	}
	t.Logf("%s: %d batches, server seq %d, %d elements; %d seqs compared between editors; %d operation effects checked (%d batches skipped in resyncs); applied %v",
		d, batches, last, len(server), compared, effects, unheld, totals)

	// The comparisons notice a difference: a copy with an older text, and
	// two editors that disagree at one seq.
	stale := maps.Clone(eds[0].view.els)
	for id, el := range stale {
		if _, ok := el["text"]; ok {
			stale[id] = jsonClone(el)
			stale[id]["text"] = "an older text"
			break
		}
	}
	if boardDiff(server, stale) == "" {
		t.Fatal("boardDiff accepted a stale text")
	}
	for seq := range eds[0].hashes {
		if _, ok := eds[1].hashes[seq]; ok {
			eds[1].hashes[seq] ^= 1
			break
		}
	}
	if _, err := sameBoardAtEachSeq(eds); err == nil {
		t.Fatal("sameBoardAtEachSeq accepted two different boards at one seq")
	}
}

// loadEditor is one editor of the sustained load test. Only its goroutine
// touches it until run returns.
type loadEditor struct {
	i    int
	c    *client
	rng  *rand.Rand
	view *boardView
	gen  float64
	// resyncing is set between a resync and the snapshot that follows it.
	resyncing bool
	snap      []map[string]any
	part      strings.Builder
	n         int
	// unanswered holds each sent batch until its ack; retry holds batches
	// refused as transient, to send again.
	unanswered map[int][]byte
	retry      []int
	kinds      map[int][]string
	sent       map[int][]map[string]any // each batch's operations
	results    map[int][]any
	acks       map[int]int64
	carried    map[int]int64 // batch number → seq of the delta that named it
	sendTimes  []time.Time
	ackTimes   []time.Time
	// history holds each element's states in the copy, by seq: nil when it
	// was deleted. Entries never change once added.
	history map[string][]elementAt
	// elHash and board are an order-free hash of the copy, kept up to date
	// per changed element; hashes holds it at each seq the copy reached.
	elHash map[string]uint64
	board  uint64
	hashes map[int64]uint64
	counts map[string]int
}

func newLoadEditor(i int, c *client, v *boardView) *loadEditor {
	e := &loadEditor{i: i, c: c, rng: rand.New(rand.NewPCG(uint64(i), 0x10ad)), view: v,
		unanswered: map[int][]byte{}, kinds: map[int][]string{}, sent: map[int][]map[string]any{}, results: map[int][]any{},
		acks: map[int]int64{}, carried: map[int]int64{}, history: map[string][]elementAt{},
		hashes: map[int64]uint64{}, counts: map[string]int{}}
	e.rehash()
	return e
}

func elementHash(el map[string]any) uint64 {
	h := fnv.New64a()
	h.Write(model.Canonical(el))
	return h.Sum64()
}

// rehash hashes the whole copy, after a snapshot.
func (e *loadEditor) rehash() {
	e.elHash, e.board = map[string]uint64{}, 0
	for id, el := range e.view.els {
		e.elHash[id] = elementHash(el)
		e.board ^= e.elHash[id]
		e.remember(id)
	}
	for id := range e.history {
		if _, ok := e.view.els[id]; !ok {
			e.remember(id)
		}
	}
	e.hashes[e.view.seq] = e.board
}

// elementAt is an element's state from a seq on, nil when deleted.
type elementAt struct {
	seq int64
	el  map[string]any
}

// remember records element id as the copy holds it now.
func (e *loadEditor) remember(id string) {
	e.history[id] = append(e.history[id], elementAt{e.view.seq, e.view.els[id]})
}

// stateAt is element id as the copy held it at seq, or nil.
func (e *loadEditor) stateAt(id string, seq int64) map[string]any {
	var el map[string]any
	for _, h := range e.history[id] {
		if h.seq > seq {
			break
		}
		el = h.el
	}
	return el
}

// run sends batch k at start + k*500ms until the deadline, catching up if it
// falls behind, then reads until every batch is answered (marking idle) and
// until its copy reaches the seq that arrives on final.
func (e *loadEditor) run(start, until time.Time, idle *sync.WaitGroup, final <-chan int64) error {
	marked := false
	defer func() {
		if !marked {
			idle.Done()
		}
	}()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	timeout := time.NewTimer(time.Until(until) + 2*time.Minute)
	defer timeout.Stop()
	target := int64(-1)
	backlog := e.c.backlog
	e.c.backlog = nil
	for _, m := range backlog {
		if err := e.handle(m); err != nil {
			return err
		}
	}
	for {
		if !marked && !time.Now().Before(until) && len(e.unanswered) == 0 {
			marked = true
			idle.Done()
		}
		if target >= 0 && e.view.seq >= target && !e.resyncing {
			if e.view.seq > target {
				return fmt.Errorf("copy at seq %d after the server stopped at %d", e.view.seq, target)
			}
			return nil
		}
		select {
		case m, ok := <-e.c.msgs:
			if !ok {
				return fmt.Errorf("socket closed (copy at seq %d)", e.view.seq)
			}
			if err := e.handle(m); err != nil {
				return err
			}
		case now := <-tick.C:
			for _, n := range e.retry {
				if err := e.c.ws.WriteFrame(1, e.unanswered[n]); err != nil {
					return err
				}
			}
			e.retry = nil
			for now.Before(until) && !start.Add(time.Duration(e.n)*loadInterval).After(now) {
				if err := e.send(); err != nil {
					return err
				}
			}
		case s := <-final:
			target = s
		case <-timeout.C:
			return fmt.Errorf("timed out: copy at seq %d, target %d, %d batches unanswered", e.view.seq, target, len(e.unanswered))
		}
	}
}

func (e *loadEditor) handle(m map[string]any) error {
	gen, _ := m["gen"].(float64)
	switch m["t"] {
	case "ack":
		n := int(m["n"].(float64))
		if _, ok := e.unanswered[n]; !ok {
			return fmt.Errorf("an ack of batch %d, which is not waiting", n)
		}
		delete(e.unanswered, n)
		seq := int64(m["seq"].(float64))
		if prev, ok := e.acks[n-1]; ok && seq < prev {
			return fmt.Errorf("batch %d acked at seq %d, before batch %d at %d", n, seq, n-1, prev)
		}
		e.acks[n] = seq
		e.ackTimes = append(e.ackTimes, time.Now())
		results, _ := m["results"].([]any)
		e.results[n] = results
		if len(results) != len(e.kinds[n]) {
			return fmt.Errorf("batch %d: %d results for %d operations", n, len(results), len(e.kinds[n]))
		}
		for i, r := range results {
			if applied(r) {
				e.counts[e.kinds[n][i]]++
			}
		}
	case "reject":
		n := int(m["n"].(float64))
		if m["retry"] != true {
			return fmt.Errorf("batch %d refused: %v (%s)", n, m, e.unanswered[n])
		}
		e.counts["transient refusal"]++
		e.retry = append(e.retry, n)
	case "resync":
		e.gen, e.resyncing, e.snap = gen, true, nil
		e.part.Reset()
	case "snapshot":
		if gen != e.gen {
			return nil
		}
		for _, el := range m["elements"].([]any) {
			e.snap = append(e.snap, el.(map[string]any))
		}
		if m["part"] == m["parts"] {
			e.view = newBoardView(false, int64(m["seq"].(float64)), e.snap)
			e.snap, e.resyncing = nil, false
			e.rehash()
			e.counts["resync"]++
		}
	case "delta":
		if gen != e.gen || e.resyncing {
			return nil
		}
		return e.applyDelta(m)
	case "delta_part":
		if gen != e.gen || e.resyncing {
			return nil
		}
		e.part.WriteString(m["data"].(string))
		if m["part"] == m["parts"] {
			var d map[string]any
			if err := json.Unmarshal([]byte(e.part.String()), &d); err != nil {
				return err
			}
			e.part.Reset()
			return e.applyDelta(d)
		}
	case "fatal":
		return fmt.Errorf("fatal: %v", m)
	}
	return nil
}

// applied reports whether an operation result changed something: "ok", a
// semantic result with applied elements, or an undo that undid something.
func applied(r any) bool {
	switch v := r.(type) {
	case string:
		return v == "ok"
	case map[string]any:
		ok, _ := v["ok"].([]any)
		undone, _ := v["undone"].(float64)
		return len(ok) > 0 || undone > 0
	}
	return false
}

func (e *loadEditor) applyDelta(d map[string]any) error {
	changes, _ := d["changes"].([]any)
	if err := e.view.apply(d); err != nil {
		return err
	}
	for _, raw := range changes {
		id := raw.(map[string]any)["id"].(string)
		e.board ^= e.elHash[id]
		delete(e.elHash, id)
		if el, ok := e.view.els[id]; ok {
			e.elHash[id] = elementHash(el)
			e.board ^= e.elHash[id]
		}
		e.remember(id)
	}
	e.hashes[e.view.seq] = e.board
	if from, ok := d["from"].(string); ok {
		n, err := strconv.Atoi(strings.TrimPrefix(from, e.c.session+":"))
		if err != nil || !strings.HasPrefix(from, e.c.session+":") {
			return fmt.Errorf("a delta from %q, not this session's batch", from)
		}
		e.carried[n] = e.view.seq
	}
	return nil
}

// loadInterval is each editor's time between batches: 2 per second.
const loadInterval = 500 * time.Millisecond

// checkRate checks that the editor sent its batches on schedule: within one
// batch of d at 2 per second, never more than two seconds apart.
func (e *loadEditor) checkRate(d time.Duration) error {
	want := int(d / loadInterval)
	if e.n < want-1 || e.n > want+1 {
		return fmt.Errorf("sent %d batches in %s, want %d", e.n, d, want)
	}
	for i := 1; i < len(e.sendTimes); i++ {
		if gap := e.sendTimes[i].Sub(e.sendTimes[i-1]); gap > 2*time.Second {
			return fmt.Errorf("batches %d and %d were sent %s apart", i, i+1, gap)
		}
	}
	return nil
}

// rollbacksBetweenEdits checks that at least one rollback ran and that each
// one had edits acknowledged within two seconds before and after it.
func rollbacksBetweenEdits(rollbacks []time.Time, eds []*loadEditor) error {
	if len(rollbacks) == 0 {
		return fmt.Errorf("no rollback ran while the editors were sending")
	}
	for i, r := range rollbacks {
		before, after := false, false
		for _, e := range eds {
			for _, a := range e.ackTimes {
				before = before || (a.Before(r) && r.Sub(a) <= 2*time.Second)
				after = after || (a.After(r) && a.Sub(r) <= 2*time.Second)
			}
		}
		if !before || !after {
			return fmt.Errorf("rollback %d had no acknowledged edit within 2 s before (%v) and after (%v) it", i+1, before, after)
		}
	}
	return nil
}

// checkEffects checks what each batch's applied operations must have done,
// independently of whether the copies agree: a batch that changed the board
// came back in the delta of its ack's seq, a batch that applied nothing came
// back in no delta, and at the ack's seq the copy shows each applied
// operation's effect on each element it applied to, except the position of
// an element a later move in the batch carried along. A server that
// acknowledged an operation but dropped or altered its change for every
// editor fails here. Where two operations of a batch apply to one element,
// only the later one is checked for it. Batches whose ack seq the copy never
// held (it skipped it in a resync) are not checked. It returns the numbers
// of effects checked and batches skipped.
func (e *loadEditor) checkEffects() (checked, skipped int, err error) {
	for n, ops := range e.sent {
		results, ok := e.results[n]
		if !ok || len(results) != len(ops) {
			return checked, skipped, fmt.Errorf("batch %d has %d results for %d operations", n, len(results), len(ops))
		}
		ids := make([][]string, len(ops))
		last := map[string]int{}
		anyApplied := false
		for i, op := range ops {
			if applied(results[i]) {
				anyApplied = true
				ids[i] = appliedTo(op, results[i])
				for _, id := range ids[i] {
					last[id] = i
				}
			}
		}
		seq := e.acks[n]
		if c, carried := e.carried[n]; carried {
			if !anyApplied {
				return checked, skipped, fmt.Errorf("batch %d applied nothing (%v), yet the delta of seq %d named it", n, results, c)
			}
			if c != seq {
				return checked, skipped, fmt.Errorf("batch %d came back in the delta of seq %d, acked at seq %d", n, c, seq)
			}
		}
		if !anyApplied {
			continue
		}
		if _, held := e.hashes[seq]; !held {
			skipped++
			continue
		}
		for i, op := range ops {
			for _, id := range ids[i] {
				if last[id] != i {
					continue
				}
				el := e.stateAt(id, seq)
				if op["g"] == "geom" && e.carriedLater(ops, results, i, el, seq) {
					e.counts["position carried by a later move"]++
					continue
				}
				if err := expectState(op, el); err != nil {
					return checked, skipped, fmt.Errorf("batch %d op %d %s on %s, at its ack seq %d: %v\nelement %s\nbatch %s\nresults %s",
						n, i, op["op"], id, seq, err, canonicalAny(el), canonicalAny(ops), canonicalAny(results))
				}
				checked++
			}
		}
	}
	return checked, skipped, nil
}

// carriedLater reports whether an applied move after ops[i] in the same
// batch carried el along: a move of its frame with its members, or of a
// member of its group with the group. Such a move shifts el by the carrier's
// actual offset, which depends on where the carrier was on the server.
func (e *loadEditor) carriedLater(ops []map[string]any, results []any, i int, el map[string]any, seq int64) bool {
	if el == nil {
		return false
	}
	for j := i + 1; j < len(ops); j++ {
		op := ops[j]
		if op["g"] != "geom" || !applied(results[j]) {
			continue
		}
		switch op["carry"] {
		case "members":
			if el[model.BundleFrame] == op["id"] {
				return true
			}
		case "group":
			if g := el[model.BundleGroup]; g != nil {
				if t := e.stateAt(op["id"].(string), seq); t != nil && t[model.BundleGroup] == g {
					return true
				}
			}
		}
	}
	return false
}

// appliedTo lists the elements an applied operation changed: its target,
// or the elements a semantic result names.
func appliedTo(op map[string]any, result any) []string {
	if m, ok := result.(map[string]any); ok {
		var out []string
		ok, _ := m["ok"].([]any) // an undo result has none
		for _, id := range ok {
			out = append(out, id.(string))
		}
		return out
	}
	switch op["op"] {
	case "add":
		return []string{op["el"].(map[string]any)["id"].(string)}
	case "set":
		return []string{op["id"].(string)}
	}
	return nil
}

// expectState checks that el, an element right after op applied, shows
// op's effect.
func expectState(op map[string]any, el map[string]any) error {
	if op["op"] == "delete" {
		if el != nil {
			return fmt.Errorf("still there")
		}
		return nil
	}
	if el == nil {
		return fmt.Errorf("not there")
	}
	switch op["op"] {
	case "set":
		if op["g"] == "geom" {
			want := op["val"].(map[string]any)
			if el["x"] != want["x"] || el["y"] != want["y"] {
				return fmt.Errorf("at (%v, %v), sent (%v, %v)", el["x"], el["y"], want["x"], want["y"])
			}
		} else if el[op["g"].(string)] != op["val"] {
			return fmt.Errorf("%s %v, sent %v", op["g"], el[op["g"].(string)], op["val"])
		}
	case "group":
		if el[model.BundleGroup] != op["group"] {
			return fmt.Errorf("group %v, sent %v", el[model.BundleGroup], op["group"])
		}
	case "ungroup":
		if el[model.BundleGroup] != nil {
			return fmt.Errorf("group %v after ungrouping", el[model.BundleGroup])
		}
	case "reparent":
		if el[model.BundleFrame] != op["frame"] {
			return fmt.Errorf("frame %v, sent %v", el[model.BundleFrame], op["frame"])
		}
	}
	return nil
}

// sameBoardAtEachSeq compares the editors' copies at every seq that two of
// them reached, and returns how many seqs it compared.
func sameBoardAtEachSeq(eds []*loadEditor) (int, error) {
	compared := 0
	seqs := map[int64]bool{}
	for _, e := range eds {
		for s := range e.hashes {
			seqs[s] = true
		}
	}
	for _, s := range slices.Sorted(maps.Keys(seqs)) {
		first, have := -1, 0
		for i, e := range eds {
			h, ok := e.hashes[s]
			if !ok {
				continue
			}
			have++
			if first < 0 {
				first = i
			} else if h != eds[first].hashes[s] {
				return compared, fmt.Errorf("at seq %d editor %d's copy differs from editor %d's", s, i, first)
			}
		}
		if have > 1 {
			compared++
		}
	}
	return compared, nil
}

// send prepares one batch of one or two operations from the copy.
func (e *loadEditor) send() error {
	e.n++
	var ops []map[string]any
	var kinds []string
	for range 1 + e.rng.IntN(4)/3 {
		op, kind := e.operation()
		// Undo and redo are batches of their own.
		alone := kind == "undo" || kind == "redo"
		if op == nil || (alone && len(ops) > 0) {
			continue
		}
		ops, kinds = append(ops, op), append(kinds, kind)
		if alone {
			break
		}
	}
	if len(ops) == 0 {
		ops, kinds = []map[string]any{{"op": "undo"}}, []string{"undo"}
	}
	body, _ := json.Marshal(map[string]any{"t": "ops", "n": e.n, "ops": ops})
	// The operations as the server reads them: JSON numbers are float64.
	var wire []map[string]any
	if err := json.Unmarshal(body, &struct {
		Ops *[]map[string]any `json:"ops"`
	}{&wire}); err != nil {
		return err
	}
	e.unanswered[e.n], e.kinds[e.n], e.sent[e.n] = body, kinds, wire
	e.sendTimes = append(e.sendTimes, time.Now())
	return e.c.ws.WriteFrame(1, body)
}

// operation is one operation on the copy and the kind it counts as, or nil
// when the copy has nothing it applies to.
func (e *loadEditor) operation() (map[string]any, string) {
	rng := e.rng
	ids := slices.Sorted(maps.Keys(e.view.els))
	var frames, boxes []string
	for _, id := range ids {
		switch e.view.els[id]["type"] {
		case model.TypeFrame:
			frames = append(frames, id)
		case model.TypeSticky:
			boxes = append(boxes, id)
		}
	}
	inc := func(id string) float64 { return e.view.els[id]["inc"].(float64) }
	pick := func(from []string) string { return from[rng.IntN(len(from))] }
	geom := func(id string) map[string]any {
		el := e.view.els[id]
		return map[string]any{"x": el["x"], "y": el["y"], "w": el["w"], "h": el["h"], "rot": el["rot"]}
	}
	text := fmt.Sprintf("ed%d-%d", e.i, e.n)
	r := rng.IntN(100)
	if len(ids) > 250 && r < 22 {
		r = 60 // keep the board from growing without bound: delete instead
	}
	switch {
	case r < 18 || len(boxes) < 4:
		el := map[string]any{"id": eid(), "type": "sticky", "x": rng.IntN(4000), "y": rng.IntN(4000), "w": 200, "h": 200, "text": text}
		if len(frames) > 0 && rng.IntN(2) == 0 {
			f := pick(frames)
			g := e.view.els[f]
			el["x"], el["y"], el["frame"] = g["x"].(float64)+20, g["y"].(float64)+20, f
			return map[string]any{"op": "add", "el": el, "refs": map[string]any{f: inc(f)}}, "add member"
		}
		return map[string]any{"op": "add", "el": el}, "add sticky"
	case r < 22:
		if len(frames) >= 8 {
			return nil, ""
		}
		el := map[string]any{"id": eid(), "type": "frame", "x": rng.IntN(4000), "y": rng.IntN(4000), "w": 900, "h": 700, "text": text}
		return map[string]any{"op": "add", "el": el}, "add frame"
	case r < 42:
		id := pick(boxes)
		return map[string]any{"op": "set", "id": id, "inc": inc(id), "g": "text", "val": text}, "text"
	case r < 58:
		id, kind := pick(ids), "move"
		g := geom(id)
		g["x"], g["y"] = g["x"].(float64)+float64(rng.IntN(41)-20), g["y"].(float64)+float64(rng.IntN(41)-20)
		op := map[string]any{"op": "set", "id": id, "inc": inc(id), "g": "geom", "val": g}
		if e.view.els[id]["type"] == model.TypeFrame {
			op["carry"], kind = "members", "move frame"
		} else if gr, _ := e.view.els[id]["group"].(string); gr != "" {
			op["carry"], kind = "group", "move group"
		}
		return op, kind
	case r < 68:
		a, b := pick(ids), pick(ids)
		return map[string]any{"op": "delete", "ids": []string{a, b}, "inc_of": map[string]any{a: inc(a), b: inc(b)}}, "delete"
	case r < 75:
		a, b := pick(boxes), pick(boxes)
		if a == b {
			return nil, ""
		}
		g := fmt.Sprintf("g_%d%09d", e.i, e.n)
		return map[string]any{"op": "group", "ids": []string{a, b}, "inc_of": map[string]any{a: inc(a), b: inc(b)}, "group": g}, "group"
	case r < 79:
		for _, id := range ids {
			if gr, _ := e.view.els[id]["group"].(string); gr != "" && rng.IntN(3) == 0 {
				return map[string]any{"op": "ungroup", "group": gr}, "ungroup"
			}
		}
		return nil, ""
	case r < 83:
		id := pick(boxes)
		op := map[string]any{"op": "reparent", "ids": []string{id}, "inc_of": map[string]any{id: inc(id)}, "frame": nil}
		if len(frames) > 0 && rng.IntN(3) > 0 {
			f := pick(frames)
			op["frame"], op["frame_inc"] = f, inc(f)
		}
		return op, "reparent"
	case r < 93:
		return map[string]any{"op": "undo"}, "undo"
	default:
		return map[string]any{"op": "redo"}, "redo"
	}
}
