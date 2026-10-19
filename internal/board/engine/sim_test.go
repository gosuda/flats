package engine

import (
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/board/model"
)

// TestRandomClients runs 3 to 5 simulated editors against one State. Each
// editor keeps a server copy, a watermark and pending batches (4.7), prepares
// operations from its screen (the server copy with pending batches applied
// again on top), and loses its connection now and then: in-flight batches
// may be lost, deltas missed, and pending batches are resent with the same
// numbers after a reconnect. Rollbacks and agent calls interleave. After every
// step the board invariants hold and a mirror built only from the reported
// changed rows equals the state; at the end every editor's copy equals the
// server. BOARD_SEEDS sets the number of seeds (default 2000).
func TestRandomClients(t *testing.T) {
	forSeeds(t, 2000, runSim)
}

type simBatch struct {
	n   int64
	ops []Op
}

type simMsg struct {
	delta    *model.Delta
	ack      int64 // batch number acknowledged or refused
	snapshot *simSnapshot
}

type simSnapshot struct {
	seq   int64
	els   []model.PrivateElement
	board model.PrivateElement
}

type simClient struct {
	name      string
	session   string
	n         int64
	pending   []simBatch
	copy      map[string]model.PrivateElement
	deleted   map[string]int64
	board     model.PrivateElement
	watermark int64
	connected bool
	inbox     []simMsg
	outbox    []simBatch
	next      int
	groups    int
}

type sim struct {
	rng       *rand.Rand
	s         *State
	log       []model.Delta
	trimmed   int64 // deltas up to this seq are gone from the log
	clients   []*simClient
	revisions []Content
	mirror    *mirror
	now       int64
	assets    []string
	agentIDs  int
}

func runSim(seed uint64) error {
	rng := rand.New(rand.NewPCG(seed, 0xb0a7d))
	m := &sim{rng: rng, s: New("Board"), now: 1_760_000_000}
	m.mirror = newMirror(m.s)
	for i := range 3 {
		h := strings.Repeat(fmt.Sprint(i), 64)
		m.assets = append(m.assets, h)
		m.mirror.apply(m.s, m.s.HoldAsset(Asset{Hash: h, Mime: "image/png", W: 10, H: 10, Bytes: 100}, m.now+1e9))
	}
	for i := range 3 + rng.IntN(3) {
		c := &simClient{name: string(rune('a' + i)), copy: map[string]model.PrivateElement{}, deleted: map[string]int64{}}
		m.clients = append(m.clients, c)
		if err := m.connect(c); err != nil {
			return err
		}
	}
	for step := range 150 {
		if err := m.step(); err != nil {
			return fmt.Errorf("step %d: %w", step, err)
		}
		if err := checkInvariants(m.s); err != nil {
			return fmt.Errorf("step %d: %w", step, err)
		}
		if step%25 == 0 {
			if err := m.mirror.check(m.s); err != nil {
				return fmt.Errorf("step %d: mirror: %w", step, err)
			}
		}
	}
	if err := m.settle(); err != nil {
		return err
	}
	return m.mirror.check(m.s)
}

func (m *sim) step() error {
	c := m.clients[m.rng.IntN(len(m.clients))]
	switch k := m.rng.IntN(100); {
	case k < 30:
		m.send(c, m.prepare(c))
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
		m.revisions = append(m.revisions, content(m.s))
	case k < 97 && len(m.revisions) > 0:
		out, err := m.s.Rollback(1, m.revisions[m.rng.IntN(len(m.revisions))], m.now)
		if err != nil {
			return err
		}
		m.mirror.apply(m.s, out.Changed)
		m.broadcast(*out.Delta, nil)
	case k < 99:
		return m.agentCall()
	default:
		m.trimmed = m.s.Seq() - int64(m.rng.IntN(5))
	}
	return nil
}

// settle reconnects everyone and runs every queue empty.
func (m *sim) settle() error {
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
		if busy {
			continue
		}
		for _, c := range m.clients {
			if len(c.pending) > 0 {
				return fmt.Errorf("%s keeps %d pending batches", c.name, len(c.pending))
			}
			if err := c.matches(m.s); err != nil {
				return fmt.Errorf("%s: %w", c.name, err)
			}
		}
		return nil
	}
}

func (m *sim) connect(c *simClient) error {
	r := m.s.Hello(Hello{Session: c.session, Pending: len(c.pending), NewID: "s-" + c.name, Key: "browser:" + c.name, Label: c.name, Now: m.now})
	m.mirror.apply(m.s, r.Changed)
	if r.Fatal != "" {
		return fmt.Errorf("%s: %s", c.name, r.Fatal)
	}
	c.session, c.connected = r.Session, true
	if c.watermark >= m.trimmed && c.watermark > 0 {
		for _, d := range m.log {
			if d.Seq > c.watermark {
				c.inbox = append(c.inbox, simMsg{delta: m.forClient(d, c)})
			}
		}
	} else {
		c.inbox = append(c.inbox, simMsg{snapshot: &simSnapshot{seq: m.s.Seq(), els: m.s.Alive(), board: m.s.Board()}})
	}
	c.outbox = append(c.outbox, c.pending...)
	return nil
}

func (m *sim) disconnect(c *simClient) {
	c.connected = false
	c.inbox = nil
	c.outbox = c.outbox[:m.rng.IntN(len(c.outbox)+1)] // the rest never arrived
}

func (m *sim) send(c *simClient, ops []Op) {
	if len(ops) == 0 {
		return
	}
	c.n++
	b := simBatch{n: c.n, ops: ops}
	c.pending = append(c.pending, b)
	if c.connected {
		c.outbox = append(c.outbox, b)
	}
}

// serve applies the next batch that reached the server from c.
func (m *sim) serve(c *simClient) error {
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
	if out.Delta != nil {
		m.broadcast(*out.Delta, c)
	}
	if c.connected {
		c.inbox = append(c.inbox, simMsg{ack: b.n})
	}
	return nil
}

func (m *sim) forClient(d model.Delta, c *simClient) *model.Delta {
	if c.session == "" || !strings.HasPrefix(d.From, c.session+":") {
		d.From = ""
	}
	return &d
}

func (m *sim) broadcast(d model.Delta, sender *simClient) {
	m.log = append(m.log, d)
	for _, c := range m.clients {
		if c.connected {
			c.inbox = append(c.inbox, simMsg{delta: m.forClient(d, c)})
		}
	}
}

func (m *sim) deliver(c *simClient) error {
	if len(c.inbox) == 0 {
		return nil
	}
	msg := c.inbox[0]
	c.inbox = c.inbox[1:]
	switch {
	case msg.snapshot != nil:
		c.copy, c.deleted = map[string]model.PrivateElement{}, map[string]int64{}
		for _, e := range msg.snapshot.els {
			c.copy[e.ID] = e
		}
		c.board, c.watermark = msg.snapshot.board, msg.snapshot.seq
	case msg.delta != nil:
		d := msg.delta
		switch {
		case d.Seq <= c.watermark:
		case d.Seq > c.watermark+1:
			c.connected, c.inbox, c.outbox = false, nil, nil
			return m.connect(c)
		default:
			if err := c.applyDelta(d); err != nil {
				return err
			}
			c.watermark = d.Seq
			if d.From != "" {
				c.drop(d.From)
			}
		}
	default:
		c.drop(fmt.Sprintf("%s:%d", c.session, msg.ack))
	}
	return nil
}

func (c *simClient) drop(from string) {
	c.pending = slices.DeleteFunc(c.pending, func(b simBatch) bool { return fmt.Sprintf("%s:%d", c.session, b.n) == from })
}

func (c *simClient) applyDelta(d *model.Delta) error {
	for _, ch := range d.Changes {
		switch {
		case ch.Add != nil:
			c.copy[ch.ID] = *ch.Add
			delete(c.deleted, ch.ID)
		case ch.Revive != nil:
			c.copy[ch.ID] = *ch.Revive
			delete(c.deleted, ch.ID)
		case ch.Del != 0:
			delete(c.copy, ch.ID)
			c.deleted[ch.ID] = ch.Del
		case ch.ID == model.BoardID:
			if err := setBundles(&c.board, ch.Set); err != nil {
				return err
			}
		default:
			e, ok := c.copy[ch.ID]
			if !ok {
				return fmt.Errorf("%s: set on unknown element %s", c.name, ch.ID)
			}
			e.Element = e.Element.Clone()
			e.V = maps.Clone(e.V)
			if err := setBundles(&e, ch.Set); err != nil {
				return err
			}
			c.copy[ch.ID] = e
		}
	}
	return nil
}

func setBundles(e *model.PrivateElement, set map[string]model.SetVal) error {
	for g, sv := range set {
		v, err := model.DecodeBundle(e.Type, g, sv.Val)
		if err != nil {
			return err
		}
		e.SetBundle(g, v)
		e.V[g] = sv.V
	}
	return nil
}

// matches compares the editor's server copy with the server.
func (c *simClient) matches(s *State) error {
	if c.watermark != s.Seq() {
		return fmt.Errorf("watermark %d, server seq %d", c.watermark, s.Seq())
	}
	want := map[string]model.PrivateElement{}
	for _, e := range s.Alive() {
		want[e.ID] = e
	}
	if len(want) != len(c.copy) {
		return fmt.Errorf("%d elements, server has %d", len(c.copy), len(want))
	}
	for id, w := range want {
		got, ok := c.copy[id]
		if !ok || !model.Equal(got.Element, w.Element) || got.Inc != w.Inc || !maps.Equal(got.V, w.V) {
			return fmt.Errorf("element %s differs:\n%s %d %v\n%s %d %v", id, got.CanonicalJSON(), got.Inc, got.V, w.CanonicalJSON(), w.Inc, w.V)
		}
	}
	b := s.Board()
	if !model.Equal(c.board.Element, b.Element) || !maps.Equal(c.board.V, b.V) {
		return fmt.Errorf("board metadata differs")
	}
	return nil
}

// screen is the server copy with pending batches applied again on top. An
// element whose add is still pending has incarnation 0.
func (c *simClient) screen() map[string]model.PrivateElement {
	sc := maps.Clone(c.copy)
	for _, b := range c.pending {
		for _, op := range b.ops {
			switch op.Op {
			case OpAdd:
				el, _, err := model.DecodeElement(op.El)
				if err == nil {
					if _, exists := sc[el.ID]; !exists {
						sc[el.ID] = model.PrivateElement{Element: el, V: map[string]int64{}}
					}
				}
			case OpSet:
				if e, ok := sc[op.ID]; ok {
					if v, err := model.DecodeBundle(e.Type, op.G, op.Val); err == nil {
						e.Element = e.Element.Clone()
						e.SetBundle(op.G, v)
						sc[op.ID] = e
					}
				}
			case OpDelete:
				for _, id := range op.IDs {
					delete(sc, id)
				}
			}
		}
	}
	return sc
}

// prepare builds a batch from the editor's screen.
func (m *sim) prepare(c *simClient) []Op {
	rng := m.rng
	if rng.IntN(12) == 0 {
		return []Op{{Op: []string{OpUndo, OpRedo}[rng.IntN(2)]}}
	}
	sc := c.screen()
	ids := slices.Sorted(maps.Keys(sc))
	inc := func(id string) int64 { return sc[id].Inc }
	pick := func(filter func(model.PrivateElement) bool) string {
		var ok []string
		for _, id := range ids {
			if filter(sc[id]) {
				ok = append(ok, id)
			}
		}
		if len(ok) == 0 {
			return ""
		}
		return ok[rng.IntN(len(ok))]
	}
	anyEl := func(model.PrivateElement) bool { return true }
	hasGeom := func(e model.PrivateElement) bool { return e.Type != model.TypeConnector }
	isFrame := func(e model.PrivateElement) bool { return e.Type == model.TypeFrame }
	target := func(e model.PrivateElement) bool { return e.Type != model.TypeConnector }
	var ops []Op
	for range 1 + rng.IntN(3) {
		switch k := rng.IntN(16); {
		case k < 4 || len(ids) == 0:
			ops = append(ops, m.newElement(c, sc, pick(isFrame), pick(target)))
		case k < 6:
			id := pick(hasGeom)
			if id == "" {
				continue
			}
			g := sc[id].Geom
			g.X, g.Y = model.Round1(g.X+float64(rng.IntN(200)-100)), model.Round1(g.Y+float64(rng.IntN(200)-100))
			carry := ""
			if sc[id].Type == model.TypeFrame && rng.IntN(2) == 0 {
				carry = CarryMembers
			} else if sc[id].Group != "" && rng.IntN(2) == 0 {
				carry = CarryGroup
			}
			if rng.IntN(4) == 0 {
				g.W, g.H = g.W+10, g.H+10
			}
			ops = append(ops, Op{Op: OpSet, ID: id, Inc: inc(id), G: model.BundleGeom, Val: raw(g), Carry: carry})
		case k < 7:
			id := pick(func(e model.PrivateElement) bool { return model.HasText(e.Type) })
			if id != "" {
				ops = append(ops, Op{Op: OpSet, ID: id, Inc: inc(id), G: model.BundleText, Val: raw(fmt.Sprint("t", rng.IntN(9))), BaseV: sc[id].V[model.BundleText]})
			}
		case k < 8:
			id := pick(func(e model.PrivateElement) bool { return e.Type == model.TypeSticky })
			if id != "" {
				ops = append(ops, Op{Op: OpSet, ID: id, Inc: inc(id), G: "style.color", Val: raw(model.StickyColors[rng.IntN(6)])})
			}
		case k < 9:
			id := pick(anyEl)
			ops = append(ops, Op{Op: OpSet, ID: id, Inc: inc(id), G: model.BundleLocked, Val: raw(rng.IntN(3) == 0)})
		case k < 10:
			id := pick(func(e model.PrivateElement) bool { return e.Type == model.TypeConnector })
			if id == "" {
				continue
			}
			end := map[string]any{"pt": []float64{float64(rng.IntN(300)), 0}}
			op := Op{Op: OpSet, ID: id, Inc: inc(id), G: model.EndBundle(rng.IntN(2))}
			if t := pick(target); t != "" && rng.IntN(2) == 0 {
				end = map[string]any{"el": t, "at": []float64{0.5, 1}}
				op.Refs = map[string]int64{t: inc(t)}
			}
			op.Val = raw(end)
			ops = append(ops, op)
		case k < 11:
			a, b := pick(anyEl), pick(anyEl)
			ops = append(ops, Op{Op: OpDelete, IDs: []string{a, b}, IncOf: map[string]int64{a: inc(a), b: inc(b)}})
		case k < 12:
			a, b := pick(anyEl), pick(anyEl)
			op := Op{Op: OpReparent, IDs: []string{a, b}, IncOf: map[string]int64{a: inc(a), b: inc(b)}, Frame: raw(nil)}
			if f := pick(isFrame); f != "" && rng.IntN(3) > 0 {
				op.Frame, op.FrameInc = raw(f), inc(f)
			}
			ops = append(ops, op)
		case k < 13:
			a, b := pick(anyEl), pick(anyEl)
			c.groups++
			ops = append(ops, Op{Op: OpGroup, IDs: []string{a, b}, IncOf: map[string]int64{a: inc(a), b: inc(b)}, Group: fmt.Sprintf("g_%s%09d", c.name, c.groups)})
		case k < 14:
			if id := pick(func(e model.PrivateElement) bool { return e.Group != "" }); id != "" {
				ops = append(ops, Op{Op: OpUngroup, Group: sc[id].Group})
			} else if f := pick(isFrame); f != "" {
				ops = append(ops, Op{Op: OpDeleteFrameContents, Frame: raw(f), Inc: inc(f)})
			}
		case k < 15:
			a := pick(anyEl)
			to := []any{"front", "back"}[rng.IntN(2)]
			if b := pick(anyEl); b != a && rng.IntN(2) == 0 {
				to = map[string]any{"after": b, "inc": inc(b)}
			}
			ops = append(ops, Op{Op: OpZorder, IDs: []string{a}, IncOf: map[string]int64{a: inc(a)}, To: raw(to)})
		default:
			if gone := slices.Sorted(maps.Keys(c.deleted)); len(gone) > 0 {
				id := gone[rng.IntN(len(gone))]
				ops = append(ops, Op{Op: OpRevive, IDs: []string{id}, IfDel: c.deleted[id]})
			} else {
				ops = append(ops, Op{Op: OpMeta, G: model.BundleTitle, Val: raw(fmt.Sprint("title ", rng.IntN(5)))})
			}
		}
	}
	return ops
}

func (m *sim) newElement(c *simClient, sc map[string]model.PrivateElement, frame, target string) Op {
	rng := m.rng
	c.next++
	id := fmt.Sprintf("e_%s%09d", c.name, c.next)
	el := map[string]any{"id": id, "x": rng.IntN(1000), "y": rng.IntN(1000), "w": 50 + rng.IntN(100), "h": 50 + rng.IntN(100)}
	refs := map[string]int64{}
	switch rng.IntN(7) {
	case 0:
		el["type"] = "frame"
		el["w"], el["h"] = 600, 600
	case 1:
		el = map[string]any{"id": id, "type": "connector", "ends": []any{map[string]any{"pt": []int{0, 0}}, map[string]any{"pt": []int{9, 9}}}}
		if target != "" {
			el["ends"] = []any{map[string]any{"el": target, "at": []float64{1, 0.5}}, map[string]any{"pt": []int{9, 9}}}
			refs[target] = sc[target].Inc
		}
	case 2:
		el["type"], el["pts"] = "pen", []int{0, 0, 5000, 10000}
	case 3:
		el["type"], el["asset"], el["natural"] = "image", m.assets[rng.IntN(len(m.assets))], []int{10, 10}
	case 4:
		el["type"] = "shape"
	default:
		el["type"] = "sticky"
	}
	if el["type"] != "frame" && el["type"] != "connector" && frame != "" && rng.IntN(2) == 0 {
		el["frame"] = frame
		refs[frame] = sc[frame].Inc
	}
	if rng.IntN(3) == 0 {
		if k, err := model.KeyBetween(topZ(sc), ""); err == nil && len(k) <= model.MaxZLen {
			el["z"] = k
		}
	}
	return Op{Op: OpAdd, El: raw(el), Refs: refs}
}

func topZ(sc map[string]model.PrivateElement) string {
	top := ""
	for _, e := range sc {
		top = max(top, e.Z)
	}
	return top
}

func (m *sim) agentCall() error {
	alive := m.s.Alive()
	var ops []AgentOp
	for range 1 + m.rng.IntN(3) {
		switch k := m.rng.IntN(4); {
		case k == 0 || len(alive) == 0:
			ops = append(ops, AgentOp{Op: "add", El: raw(map[string]any{"type": "sticky", "text": "agent"})})
		case k == 1:
			ops = append(ops, AgentOp{Op: "delete", IDs: []string{alive[m.rng.IntN(len(alive))].ID}})
		default:
			e := alive[m.rng.IntN(len(alive))]
			f := map[string]any{"locked": false}
			if e.Type != model.TypeConnector {
				f = map[string]any{"x": e.Geom.X + 10}
			}
			ops = append(ops, AgentOp{Op: "set", ID: e.ID, Fields: raw(f)})
		}
	}
	out, err := m.s.ApplyAgent(AgentCall{Ops: ops, Label: "agent", Now: m.now, NewID: func(p string) string {
		m.agentIDs++
		return fmt.Sprintf("%sZ%09d", p, m.agentIDs)
	}})
	if ae := (*AgentError)(nil); errors.As(err, &ae) {
		return nil
	} else if err != nil {
		return err
	}
	m.mirror.apply(m.s, out.Changed)
	if out.Delta != nil {
		m.broadcast(*out.Delta, nil)
	}
	return nil
}

// mirror is a store that learns rows only from Changed lists.
type mirror struct {
	elements map[string]ElementRow
	sessions map[string]Session
	created  map[CreatedKey]CreatedRow
	receipts map[ReceiptKey]Receipt
	undo     map[UndoKey]string
	assets   map[string]Asset
	buckets  map[int]string
	seq      int64
	resets   []int64
}

func newMirror(s *State) *mirror {
	m := &mirror{elements: map[string]ElementRow{}, sessions: map[string]Session{}, created: map[CreatedKey]CreatedRow{},
		receipts: map[ReceiptKey]Receipt{}, undo: map[UndoKey]string{}, assets: map[string]Asset{}, buckets: map[int]string{}}
	rows := s.Export()
	for _, e := range rows.Elements {
		m.elements[e.ID] = e
	}
	for b := range model.BucketCount + 1 {
		if f, ok := s.BucketFile(b); ok {
			m.buckets[b] = string(f)
		}
	}
	m.seq, m.resets = rows.Seq, rows.Resets
	return m
}

func put[K comparable, V any](m map[K]V, k K, v V, ok bool) {
	if ok {
		m[k] = v
	} else {
		delete(m, k)
	}
}

func (m *mirror) apply(s *State, c Changed) {
	for _, id := range c.Elements {
		r, ok := s.Element(id)
		put(m.elements, id, r, ok)
	}
	for _, b := range c.Buckets {
		f, ok := s.BucketFile(b)
		put(m.buckets, b, string(f), ok)
	}
	for _, k := range c.Created {
		inc, ok := s.CreatedInc(k)
		put(m.created, k, CreatedRow{k.Session, k.ID, inc}, ok)
	}
	for _, k := range c.Undo {
		r, ok := s.UndoRow(k)
		put(m.undo, k, string(raw(r.Entry)), ok)
	}
	for _, id := range c.Sessions {
		se, ok := s.SessionRow(id)
		put(m.sessions, id, se, ok)
	}
	for _, k := range c.Receipts {
		r, ok := s.ReceiptRow(k)
		put(m.receipts, k, r, ok)
	}
	for _, h := range c.Assets {
		a, ok := s.AssetRow(h)
		put(m.assets, h, a, ok)
	}
	if c.Meta {
		m.seq, m.resets = s.Seq(), s.Resets()
	}
}

func (m *mirror) check(s *State) error {
	rows := s.Export()
	if m.seq != rows.Seq || !slices.Equal(m.resets, rows.Resets) {
		return fmt.Errorf("seq %d/%v, state %d/%v", m.seq, m.resets, rows.Seq, rows.Resets)
	}
	if len(m.elements) != len(rows.Elements) {
		return fmt.Errorf("%d element rows, state has %d", len(m.elements), len(rows.Elements))
	}
	for _, e := range rows.Elements {
		if got := m.elements[e.ID]; string(raw(got)) != string(raw(e)) {
			return fmt.Errorf("element row %s:\n%s\n%s", e.ID, raw(got), raw(e))
		}
	}
	for b := range model.BucketCount + 1 {
		f, ok := s.BucketFile(b)
		if got, have := m.buckets[b]; have != ok || got != string(f) {
			return fmt.Errorf("bucket %d is stale", b)
		}
	}
	checks := []struct {
		name      string
		got, want int
		same      bool
	}{
		{"sessions", len(m.sessions), len(rows.Sessions), sameRows(m.sessions, rows.Sessions, func(r Session) string { return r.ID })},
		{"created", len(m.created), len(rows.Created), sameRows(m.created, rows.Created, func(r CreatedRow) CreatedKey { return CreatedKey{r.Session, r.ID} })},
		{"receipts", len(m.receipts), len(rows.Receipts), sameRows(m.receipts, rows.Receipts, func(r Receipt) ReceiptKey { return ReceiptKey{r.Session, r.N} })},
		{"assets", len(m.assets), len(rows.Assets), sameRows(m.assets, rows.Assets, func(r Asset) string { return r.Hash })},
	}
	for _, c := range checks {
		if c.got != c.want || !c.same {
			return fmt.Errorf("%s rows differ (%d, state %d)", c.name, c.got, c.want)
		}
	}
	if len(m.undo) != len(rows.Undo) {
		return fmt.Errorf("%d undo rows, state has %d", len(m.undo), len(rows.Undo))
	}
	for _, u := range rows.Undo {
		if m.undo[UndoKey{u.Session, u.Stack, u.Idx}] != string(raw(u.Entry)) {
			return fmt.Errorf("undo row %s/%s/%d differs", u.Session, u.Stack, u.Idx)
		}
	}
	return nil
}

func sameRows[K comparable, R any](m map[K]R, rows []R, key func(R) K) bool {
	for _, r := range rows {
		got, ok := m[key(r)]
		if !ok || string(raw(got)) != string(raw(r)) {
			return false
		}
	}
	return true
}
