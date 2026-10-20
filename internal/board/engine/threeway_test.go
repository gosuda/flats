package engine

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/board/model"
)

// TestScreensServerAndReference feeds one operation sequence to simulated
// editors, the server and the reference model (board-TECH 14, Randomized
// tests: every screen = the server = the reference model). 3 to 5 editors
// prepare batches from their screens with the operations the reference
// model knows (add, set, delete, restore, group, ungroup), lose their
// connections now and then and resend pending batches. Rollbacks and agent
// calls interleave. Each batch reaches the reference model at the moment
// it reaches the server, with the appearances the editor saw when it
// prepared the batch. After every server action the server equals the
// reference model and both judged every operation alike; after every
// message an editor takes in, its copy equals the reference model at the
// copy's seq; at the end every editor's screen equals its copy, the server
// and the reference model. BOARD_SEEDS sets the number of seeds (default
// 2000).
func TestScreensServerAndReference(t *testing.T) {
	forSeeds(t, 2000, func(seed uint64) error {
		_, err := runThreeWay(seed)
		return err
	})
}

// threeWay runs the simulator's editors and server with the reference model
// beside them.
type threeWay struct {
	*sim
	ref *refBoard
	// ops holds each sent batch's operations as the reference model reads
	// them, by editor and batch number.
	ops    map[string]map[int64][]sentRef
	served map[string]map[int64]bool
	// appeared maps an element's incarnation to the reference appearance
	// it stands for; gone maps a deletion seq to the reference deletion.
	appeared map[string]map[int64]int
	gone     map[string]map[int64]int
	// created is the appearance each editor's latest own add or revive of
	// an element produced: what incarnation 0 names (4.1).
	created map[string]map[string]int
	// revivedIn is the batch number of each editor's latest own revive of
	// an element, for counting incarnation-0 operations after a revive.
	revivedIn map[string]map[string]int64
	// at is the reference board (alive elements' property groups) after
	// the server action that left the server at each seq.
	at        map[int64]map[string]map[string]string
	revisions []revision
	agentView view
	groups    int
	// counts records what the run exercised.
	counts map[string]int
}

// revision is the board at one point, for rollbacks: the server's content
// and the reference model's board.
type revision struct {
	c   Content
	ref *refBoard
}

// sentRef is an operation as the editor prepared it: the reference
// operation without its seen map, plus the incarnations and deletion seqs
// the editor's screen showed for the elements it names.
type sentRef struct {
	refOp
	incs map[string]int64
	dels map[string]int64
}

func runThreeWay(seed uint64) (*threeWay, error) {
	m := &threeWay{
		sim:       &sim{rng: rand.New(rand.NewPCG(seed, 0x3e1f)), s: New("Board"), now: 1_760_000_000},
		ref:       newRefBoard(),
		ops:       map[string]map[int64][]sentRef{},
		served:    map[string]map[int64]bool{},
		appeared:  map[string]map[int64]int{},
		gone:      map[string]map[int64]int{},
		created:   map[string]map[string]int{},
		revivedIn: map[string]map[string]int64{},
		at:        map[int64]map[string]map[string]string{},
		counts:    map[string]int{},
	}
	m.mirror = newMirror(m.s)
	if err := m.record(); err != nil {
		return nil, err
	}
	for i := range 3 + m.rng.IntN(3) {
		c := &simClient{name: string(rune('a' + i)), copy: map[string]model.PrivateElement{}, deleted: map[string]int64{}}
		m.clients = append(m.clients, c)
		m.ops[c.name], m.served[c.name], m.created[c.name] = map[int64][]sentRef{}, map[int64]bool{}, map[string]int{}
		m.revivedIn[c.name] = map[string]int64{}
		if err := m.connect(c); err != nil {
			return nil, err
		}
	}
	for step := range 150 {
		if err := m.step(); err != nil {
			return nil, fmt.Errorf("step %d: %w", step, err)
		}
		if err := checkInvariants(m.s); err != nil {
			return nil, fmt.Errorf("step %d: %w", step, err)
		}
	}
	if err := m.settle(); err != nil {
		return nil, err
	}
	return m, m.mirror.check(m.s)
}

func (m *threeWay) step() error {
	c := m.clients[m.rng.IntN(len(m.clients))]
	switch k := m.rng.IntN(100); {
	case k < 30:
		ops, refs := m.prepare(c)
		if len(ops) > 0 {
			m.send(c, ops)
			m.ops[c.name][c.n] = refs
		}
	case k < 55:
		return m.serve(c)
	case k < 85:
		return m.deliver(c)
	case k < 89:
		m.disconnect(c)
	case k < 93:
		if !c.connected {
			return m.connect(c)
		}
	case k < 95:
		m.revisions = append(m.revisions, revision{content(m.s), m.ref.clone()})
	case k < 97 && len(m.revisions) > 0:
		r := m.revisions[m.rng.IntN(len(m.revisions))]
		out, err := m.s.Rollback(1, r.c, m.now)
		if err != nil {
			return err
		}
		m.ref.rollback(r.ref)
		m.counts["rollback"]++
		m.mirror.apply(m.s, out.Changed)
		m.broadcast(*out.Delta, nil)
		return m.record()
	case k < 99:
		if m.agentView.inc == nil || m.rng.IntN(3) == 0 {
			m.agentView = takeView(m.s, m.ref)
		}
		h := &harness{s: m.s, now: m.now}
		out, err := agentStep(h, m.ref, &m.agentView, m.rng, m.pickKey)
		if err != nil {
			return err
		}
		if out.Delta != nil {
			m.counts["agent"]++
			m.mirror.apply(m.s, out.Changed)
			m.broadcast(*out.Delta, nil)
		}
		return m.record()
	default:
		m.trimmed = m.s.Seq() - int64(m.rng.IntN(5))
	}
	return nil
}

func (m *threeWay) pickKey(set map[string]int64) string {
	if len(set) == 0 {
		return ""
	}
	keys := slices.Sorted(maps.Keys(set))
	return keys[m.rng.IntN(len(keys))]
}

// prepare builds a batch from the editor's screen, for the server and the
// reference model. Like the reference test, one batch never both regroups
// and deletes: revive restores relations from the start of the deleting
// batch, a mechanism outside the reference model.
func (m *threeWay) prepare(c *simClient) ([]Op, []sentRef) {
	sc := c.screen()
	incs := map[string]int64{}
	groups := map[string]string{}
	for id, e := range sc {
		incs[id], groups[id] = e.Inc, e.Group
	}
	var ops []Op
	var refs []sentRef
	kinds := map[string]bool{}
	for range 1 + m.rng.IntN(3) {
		op, r, ok := m.clientOp(c, incs, groups)
		grouping := r.kind == "group" || r.kind == "ungroup"
		if !ok || (grouping && kinds["delete"]) || (r.kind == "delete" && (kinds["group"] || kinds["ungroup"])) {
			continue
		}
		kinds[r.kind] = true
		ops, refs = append(ops, op), append(refs, r)
	}
	return ops, refs
}

// clientOp is personOp of the reference test, prepared from a screen.
func (m *threeWay) clientOp(c *simClient, incs map[string]int64, groups map[string]string) (Op, sentRef, bool) {
	rng := m.rng
	id := m.pickKey(incs)
	saw := map[string]int64{id: incs[id]}
	switch k := rng.IntN(10); {
	case k < 2 || id == "":
		c.next++
		nid := fmt.Sprintf("e_%s%09d", c.name, c.next)
		el := model.Element{ID: nid, Type: model.TypeSticky, Geom: model.Geom{X: float64(rng.IntN(1000)), Y: 0, W: 100, H: 100}, Z: "V", Style: model.DefaultStyle(model.TypeSticky)}
		return Op{Op: OpAdd, El: el.CanonicalJSON()}, sentRef{refOp: refOp{kind: "add", ids: []string{nid}, props: refProps(el)}}, true
	case k < 6:
		prop, val := randomProp(rng)
		return Op{Op: OpSet, ID: id, Inc: incs[id], G: prop, Val: json.RawMessage(val)},
			sentRef{refOp: refOp{kind: "set", ids: []string{id}, prop: prop, value: val}, incs: saw}, true
	case k < 7:
		return Op{Op: OpDelete, IDs: []string{id}, IncOf: map[string]int64{id: incs[id]}},
			sentRef{refOp: refOp{kind: "delete", ids: []string{id}}, incs: saw}, true
	case k < 8:
		gone := m.pickKey(c.deleted)
		if gone == "" {
			return Op{}, sentRef{}, false
		}
		return Op{Op: OpRevive, IDs: []string{gone}, IfDel: c.deleted[gone]},
			sentRef{refOp: refOp{kind: "restore", ids: []string{gone}}, dels: map[string]int64{gone: c.deleted[gone]}}, true
	case k < 9:
		other := m.pickKey(incs)
		if other == id {
			return Op{}, sentRef{}, false
		}
		m.groups++
		g := fmt.Sprintf("g_%s%09d", c.name, m.groups)
		saw[other] = incs[other]
		return Op{Op: OpGroup, IDs: []string{id, other}, IncOf: map[string]int64{id: incs[id], other: incs[other]}, Group: g},
			sentRef{refOp: refOp{kind: "group", ids: []string{id, other}, group: g}, incs: saw}, true
	default:
		grouped := map[string]int64{}
		for id, g := range groups {
			if g != "" {
				grouped[id] = 0
			}
		}
		member := m.pickKey(grouped)
		if member == "" {
			return Op{}, sentRef{}, false
		}
		g := groups[member]
		return Op{Op: OpUngroup, Group: g}, sentRef{refOp: refOp{kind: "ungroup", group: g}}, true
	}
}

// resolve gives an operation the reference appearances the editor saw.
// Incarnation 0 names the element as the editor's own latest add or revive
// left it when the operation applies, including a revive earlier in the
// same batch (4.1). An incarnation or deletion the server never reported
// maps to no appearance (-1), which matches nothing.
func (m *threeWay) resolve(c *simClient, n int64, s sentRef) refOp {
	op := s.refOp
	op.seen = map[string]int{}
	for id, inc := range s.incs {
		app, ok := m.appeared[id][inc]
		if inc == 0 {
			app, ok = m.created[c.name][id]
			if r, revived := m.revivedIn[c.name][id]; revived {
				m.counts["incarnation 0 after own revive"]++
				if r == n {
					m.counts["incarnation 0 after own revive in the batch"]++
				}
			}
		}
		if !ok {
			app = -1
		}
		op.seen[id] = app
	}
	for id, del := range s.dels {
		app, ok := m.gone[id][del]
		if !ok {
			app = -1
		}
		op.seen[id] = app
	}
	return op
}

// serve applies the next batch that reached the server from c, and the
// same batch to the reference model the first time it arrives.
func (m *threeWay) serve(c *simClient) error {
	if len(c.outbox) == 0 {
		return nil
	}
	b := c.outbox[0]
	c.outbox = c.outbox[1:]
	out := m.s.ApplyBatch(Batch{Session: c.session, N: b.n, Ops: b.ops, Now: m.now})
	m.mirror.apply(m.s, out.Changed)
	if out.Fatal != "" {
		return fmt.Errorf("%s batch %d: fatal %s", c.name, b.n, out.Fatal)
	}
	if out.Reject != nil && !out.Replayed {
		return fmt.Errorf("%s batch %d refused: %+v (%s)", c.name, b.n, out.Reject, raw(b.ops))
	}
	if m.served[c.name][b.n] {
		// A resent batch: the server answers from its receipt and changes
		// nothing; the reference model already has it.
		if out.Delta != nil {
			return fmt.Errorf("%s batch %d changed the board again when resent", c.name, b.n)
		}
		m.counts["resent"]++
	} else {
		if out.Replayed {
			return fmt.Errorf("%s batch %d was answered as a replay on its first arrival", c.name, b.n)
		}
		m.served[c.name][b.n] = true
		sent := m.ops[c.name][b.n]
		want := m.ref.personEach(len(sent), func(i int) refOp { return m.resolve(c, b.n, sent[i]) }, func(i int, applied bool) {
			if (sent[i].kind == "add" || sent[i].kind == "restore") && applied {
				id := sent[i].ids[0]
				m.created[c.name][id] = m.ref.els[id].appeared
				delete(m.revivedIn[c.name], id)
				if sent[i].kind == "restore" {
					m.revivedIn[c.name][id] = b.n
				}
			}
		})
		var got []json.RawMessage
		if err := json.Unmarshal(out.Results, &got); err != nil || len(got) != len(b.ops) {
			return fmt.Errorf("%s batch %d: %d results for %d operations (%v)", c.name, b.n, len(got), len(b.ops), err)
		}
		for i := range b.ops {
			if applied := resultApplied(got[i]); applied != want[i] {
				return fmt.Errorf("%s batch %d op %d (%s): server applied=%v (%s), reference %v", c.name, b.n, i, b.ops[i].Op, applied, got[i], want[i])
			}
			if want[i] {
				m.counts[sent[i].kind]++
			} else {
				m.counts["ignored"]++
			}
		}
	}
	if out.Delta != nil {
		m.broadcast(*out.Delta, c)
	}
	if c.connected {
		c.inbox = append(c.inbox, simMsg{ack: b.n})
	}
	return m.record()
}

// record checks the server against the reference model after a server
// action, learns which appearance each incarnation and deletion seq stands
// for, and keeps the reference board at the current seq.
func (m *threeWay) record() error {
	if err := compareRef(m.s, m.ref); err != nil {
		return err
	}
	learn := func(table map[string]map[int64]int, id string, key int64, val int) error {
		if table[id] == nil {
			table[id] = map[int64]int{}
		}
		if old, ok := table[id][key]; ok && old != val {
			return fmt.Errorf("%s: %d stands for reference arrival %d and %d", id, key, old, val)
		}
		table[id][key] = val
		return nil
	}
	for id, r := range m.s.rows {
		if id == model.BoardID {
			continue
		}
		e := m.ref.els[id]
		if e == nil {
			return fmt.Errorf("%s has a server row but no reference element", id)
		}
		var err error
		switch {
		case r.alive():
			err = learn(m.appeared, id, r.Inc, e.appeared)
		case r.Deleted:
			err = learn(m.gone, id, r.DelSeq, e.deleted)
		}
		if err != nil {
			return err
		}
	}
	m.at[m.s.Seq()] = refBoardProps(m.ref)
	return nil
}

// refBoardProps is the reference board as compared with copies: every alive
// element's property groups by id.
func refBoardProps(b *refBoard) map[string]map[string]string {
	out := map[string]map[string]string{}
	for id, e := range b.els {
		if e.alive {
			out[id] = maps.Clone(e.props)
		}
	}
	return out
}

// copyMatches compares an editor's elements with a reference board.
func copyMatches(els map[string]model.PrivateElement, want map[string]map[string]string) error {
	for id, props := range want {
		e, ok := els[id]
		if !ok {
			return fmt.Errorf("%s is missing", id)
		}
		if got := refProps(e.Element); !maps.Equal(got, props) {
			return fmt.Errorf("%s: copy %v, reference %v", id, got, props)
		}
	}
	for id := range els {
		if _, ok := want[id]; !ok {
			return fmt.Errorf("%s is not in the reference board", id)
		}
	}
	return nil
}

// deliver gives c its next message (a snapshot, a delta or an ack); the
// copy then equals the reference board at its seq.
func (m *threeWay) deliver(c *simClient) error {
	if len(c.inbox) == 0 {
		return nil
	}
	if err := m.sim.deliver(c); err != nil {
		return err
	}
	want, ok := m.at[c.watermark]
	if !ok {
		return fmt.Errorf("%s is at seq %d, which no server action produced", c.name, c.watermark)
	}
	if err := copyMatches(c.copy, want); err != nil {
		return fmt.Errorf("%s at seq %d: %w", c.name, c.watermark, err)
	}
	m.counts["copy checks"]++
	return nil
}

// connect is sim.connect with its server actions recorded.
func (m *threeWay) connect(c *simClient) error {
	if err := m.sim.connect(c); err != nil {
		return err
	}
	return m.record()
}

// settle reconnects everyone and runs every queue empty, then compares
// every screen, copy, the server and the reference model.
func (m *threeWay) settle() error {
	for round := 0; ; round++ {
		if round > 100 {
			return fmt.Errorf("editors did not settle")
		}
		busy := false
		for _, c := range m.clients {
			if !c.connected {
				if err := m.connect(c); err != nil {
					return err
				}
			}
			for len(c.outbox) > 0 {
				busy = true
				if err := m.serve(c); err != nil {
					return err
				}
			}
		}
		for _, c := range m.clients {
			for len(c.inbox) > 0 {
				busy = true
				if err := m.deliver(c); err != nil {
					return err
				}
			}
		}
		if !busy {
			break
		}
	}
	if err := compareRef(m.s, m.ref); err != nil {
		return fmt.Errorf("server and reference: %w", err)
	}
	want := refBoardProps(m.ref)
	for _, c := range m.clients {
		if len(c.pending) > 0 {
			return fmt.Errorf("%s keeps %d pending batches", c.name, len(c.pending))
		}
		if err := c.matches(m.s); err != nil {
			return fmt.Errorf("%s and server: %w", c.name, err)
		}
		if err := copyMatches(c.screen(), want); err != nil {
			return fmt.Errorf("%s's screen and reference: %w", c.name, err)
		}
	}
	return nil
}

// TestThreeWayComparisonsFail checks that the comparisons above notice a
// difference, and that a run exercises every kind of operation.
func TestThreeWayComparisonsFail(t *testing.T) {
	m, err := runThreeWay(1)
	if err != nil {
		t.Fatal(err)
	}
	totals := map[string]int{}
	for seed := range uint64(40) {
		r, err := runThreeWay(seed)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		for k, n := range r.counts {
			totals[k] += n
		}
	}
	for _, k := range []string{"add", "set", "delete", "restore", "group", "ungroup", "ignored", "resent", "rollback", "agent", "copy checks"} {
		if totals[k] == 0 {
			t.Errorf("40 seeds never exercised %q (%v)", k, totals)
		}
	}
	t.Logf("40 seeds: %v", totals)

	c := m.clients[0]
	want := refBoardProps(m.ref)
	if len(want) == 0 {
		t.Fatal("seed 1 ends with an empty board; pick another seed")
	}
	id := slices.Sorted(maps.Keys(want))[0]
	changed := maps.Clone(c.copy)
	e := changed[id]
	e.Element = e.Element.Clone()
	e.Text += " changed"
	changed[id] = e
	if err := copyMatches(changed, want); err == nil || !strings.Contains(err.Error(), id) {
		t.Fatalf("copyMatches accepted a changed text: %v", err)
	}
	short := maps.Clone(c.copy)
	delete(short, id)
	if copyMatches(short, want) == nil {
		t.Fatal("copyMatches accepted a missing element")
	}
	extra := maps.Clone(c.copy)
	extra["e_zz00000000"] = c.copy[id]
	if copyMatches(extra, want) == nil {
		t.Fatal("copyMatches accepted an extra element")
	}
	// A reference model that judged one operation differently fails the
	// run: a reference board missing an element no longer equals the server.
	m.ref.els[id].alive = false
	if compareRef(m.s, m.ref) == nil {
		t.Fatal("compareRef accepted a reference board without an alive element")
	}
}

// TestThreeWayIncarnationZeroSeeds runs fixed seeds that reach the two
// cases that once made the server and the reference model disagree through
// the test's own reading of incarnation 0 (4.1): an operation after the
// editor's own revive (seed 2277), and one after a revive earlier in the
// same batch (seed 13979). If the generator changes so that they no longer
// reach those cases, pick new seeds.
func TestThreeWayIncarnationZeroSeeds(t *testing.T) {
	for seed, kind := range map[uint64]string{2277: "incarnation 0 after own revive", 13979: "incarnation 0 after own revive in the batch"} {
		m, err := runThreeWay(seed)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if m.counts[kind] == 0 {
			t.Errorf("seed %d no longer reaches %q (%v)", seed, kind, m.counts)
		}
	}
}
