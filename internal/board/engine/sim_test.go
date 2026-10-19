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
// numbers after a reconnect. Rollbacks, replacements and agent calls
// interleave. After every step the board invariants hold and a mirror built
// only from the reported changed rows equals the state; at the end every
// editor's copy equals the server. BOARD_SEEDS sets the number of seeds
// (default 2000).
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
	imports   int64 // replacements, for distinct author times
	// read is the agent's last read of the board, which its if_ver guards
	// come from.
	read map[string]agentRead
}

// agentRead is an element as the agent's last read saw it: its incarnation,
// its ver and its content without the stacking key, which a rekey rewrites
// without a version.
type agentRead struct {
	inc, ver int64
	content  string
}

func readOf(e model.PrivateElement, ver int64) agentRead {
	el := e.Element.Clone()
	el.Z = ""
	return agentRead{e.Inc, ver, string(el.CanonicalJSON())}
}

// readBoard is a get_board of the whole board by the agent.
func (m *sim) readBoard() {
	m.read = map[string]agentRead{}
	for _, e := range m.s.Alive() {
		v, _ := m.s.Ver(e.ID)
		m.read[e.ID] = readOf(e, v)
	}
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
		var out Outcome
		var err error
		if m.rng.IntN(2) == 0 {
			out, err = m.s.Rollback(1, m.revisions[m.rng.IntN(len(m.revisions))], m.now)
		} else {
			// A replacement that keeps every element and rewrites fields
			// outside every bundle: some author fields, pen points and image
			// assets. Each element that keeps its type keeps its
			// incarnation, and those fields reach editors and buckets too.
			// Now and then a sticky, shape or pen becomes another of those
			// types under the same id: it gets a new incarnation, editors'
			// pending operations against the old one are void:gone without
			// refusing their batches (4.1), and connectors attached to it
			// stay attached with the new end_inc.
			c := content(m.s)
			m.imports++
			retyped := map[string]bool{}
			for i := range c.Elements {
				el := &c.Elements[i]
				if m.rng.IntN(2) == 0 {
					el.By, el.At = "import:sim", m.imports
				}
				if to := retype(el.Type, m.rng); to != "" && m.rng.IntN(6) == 0 {
					el.Type, el.Text, el.Style, el.Pts = to, "", model.DefaultStyle(to), nil
					if to == model.TypePen {
						el.Pts = []int{0, 0, m.rng.IntN(10001), m.rng.IntN(10001)}
					}
					retyped[el.ID] = true
					continue
				}
				switch {
				case el.Type == model.TypePen && m.rng.IntN(2) == 0:
					el.Pts = []int{m.rng.IntN(10001), m.rng.IntN(10001), m.rng.IntN(10001), m.rng.IntN(10001)}
				case el.Type == model.TypeImage && m.rng.IntN(2) == 0:
					el.Asset = m.assets[m.rng.IntN(len(m.assets))]
					el.Natural = [2]int{1 + m.rng.IntN(20), 1 + m.rng.IntN(20)}
					c.Assets[el.Asset] = *m.s.assets[el.Asset]
				}
			}
			incs := map[string]int64{}
			for _, el := range c.Elements {
				incs[el.ID] = m.s.rows[el.ID].Inc
			}
			out, err = m.s.Replace(c, "import", m.now)
			for id, inc := range incs {
				if err == nil && (m.s.rows[id].Inc != inc) != retyped[id] {
					return fmt.Errorf("replacement gave %s incarnation %d, had %d, type changed %v", id, m.s.rows[id].Inc, inc, retyped[id])
				}
			}
		}
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

// retype is another type a replacement may give an element of type typ
// under the same id, or "" for types the simulator keeps.
func retype(typ string, rng *rand.Rand) string {
	others := map[string][]string{
		model.TypeSticky: {model.TypeShape, model.TypePen},
		model.TypeShape:  {model.TypeSticky, model.TypePen},
		model.TypePen:    {model.TypeSticky, model.TypeShape},
	}[typ]
	if len(others) == 0 {
		return ""
	}
	return others[rng.IntN(len(others))]
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
	return applyChanges(c.name, c.copy, c.deleted, &c.board, d)
}

// applyChanges applies a delta to a Private copy. A set of a connector end
// carries no end_inc: the end's target is alive in the copy once the whole
// delta applied, and end_inc is its incarnation (4.4, 4.6).
func applyChanges(name string, els map[string]model.PrivateElement, deleted map[string]int64, board *model.PrivateElement, d *model.Delta) error {
	var ends []string
	for _, ch := range d.Changes {
		switch {
		case ch.Add != nil:
			els[ch.ID] = *ch.Add
			delete(deleted, ch.ID)
		case ch.Revive != nil:
			els[ch.ID] = *ch.Revive
			delete(deleted, ch.ID)
		case ch.Del != 0:
			delete(els, ch.ID)
			deleted[ch.ID] = ch.Del
		case ch.ID == model.BoardID:
			if err := setBundles(board, ch.Set); err != nil {
				return err
			}
		default:
			e, ok := els[ch.ID]
			if !ok {
				return fmt.Errorf("%s: set on unknown element %s", name, ch.ID)
			}
			e.Element = e.Element.Clone()
			e.V = maps.Clone(e.V)
			if err := setBundles(&e, ch.Set); err != nil {
				return err
			}
			els[ch.ID] = e
			if _, ok := ch.Set[model.BundleEnd0]; ok {
				ends = append(ends, ch.ID)
			} else if _, ok := ch.Set[model.BundleEnd1]; ok {
				ends = append(ends, ch.ID)
			}
		}
	}
	for _, id := range ends {
		e := els[id]
		for k, end := range e.Ends {
			e.EndInc[k] = 0
			if end.El != "" {
				t, ok := els[end.El]
				if !ok {
					return fmt.Errorf("%s: connector %s ends at unknown element %s", name, id, end.El)
				}
				e.EndInc[k] = t.Inc
			}
		}
		els[id] = e
	}
	return nil
}

// samePrivate compares two Private copies of an element, end_inc included.
func samePrivate(x, y model.PrivateElement) bool {
	return model.Equal(x.Element, y.Element) && x.Inc == y.Inc && maps.Equal(x.V, y.V) && x.EndInc == y.EndInc
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
		if !ok || !samePrivate(got, w) {
			return fmt.Errorf("element %s differs:\n%s %d %v %v\n%s %d %v %v", id, got.CanonicalJSON(), got.Inc, got.V, got.EndInc, w.CanonicalJSON(), w.Inc, w.V, w.EndInc)
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
	if rng.IntN(8) == 0 {
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
			ops = append(ops, zorderOf(rng, pick(anyEl), func() string { return pick(anyEl) }, inc))
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

// zorderOf is a stacking change of one to three elements, to the front,
// the back, or after or before another element.
func zorderOf(rng *rand.Rand, first string, pick func() string, inc func(string) int64) Op {
	ids := []string{first}
	for range rng.IntN(3) {
		if id := pick(); !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	incOf := map[string]int64{}
	for _, id := range ids {
		incOf[id] = inc(id)
	}
	to := []any{"front", "back"}[rng.IntN(2)]
	if b := pick(); !slices.Contains(ids, b) && rng.IntN(2) == 0 {
		to = map[string]any{[]string{"after", "before"}[rng.IntN(2)]: b, "inc": inc(b)}
	}
	return Op{Op: OpZorder, IDs: ids, IncOf: incOf, To: raw(to)}
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
	if m.read == nil {
		m.readBoard()
	}
	defer func() {
		if m.rng.IntN(2) == 0 {
			m.readBoard()
		}
	}()
	alive := m.s.Alive()
	var ops []AgentOp
	// guarded holds the elements the call guards with the ver of the
	// agent's last read, and what they are now, before the call.
	guarded := map[string]agentRead{}
	guard := func(op *AgentOp, e model.PrivateElement) {
		r, ok := m.read[e.ID]
		if !ok || m.rng.IntN(2) == 0 {
			return
		}
		if _, dup := guarded[e.ID]; dup {
			return
		}
		v, _ := m.s.Ver(e.ID)
		guarded[e.ID] = readOf(e, v)
		op.IfVer = raw(r.ver)
	}
	for range 1 + m.rng.IntN(3) {
		switch k := m.rng.IntN(4); {
		case k == 0 || len(alive) == 0:
			ops = append(ops, AgentOp{Op: "add", El: raw(map[string]any{"type": "sticky", "text": "agent"})})
		case k == 1:
			e := alive[m.rng.IntN(len(alive))]
			op := AgentOp{Op: "delete", IDs: []string{e.ID}}
			guard(&op, e)
			ops = append(ops, op)
		default:
			e := alive[m.rng.IntN(len(alive))]
			f := map[string]any{"locked": false}
			if e.Type != model.TypeConnector {
				f = map[string]any{"x": e.Geom.X + 10}
			}
			op := AgentOp{Op: "set", ID: e.ID, Fields: raw(f)}
			guard(&op, e)
			ops = append(ops, op)
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
	// A guard that let the call through protected an element that has not
	// changed since the read (rule 33).
	for id, now := range guarded {
		if r := m.read[id]; now != r {
			return fmt.Errorf("if_ver %d let a call change %s, which changed since the read:\nread %d %s\nnow  %d %s",
				r.ver, id, r.inc, r.content, now.inc, now.content)
		}
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

// TestRandomUndoRedo runs one editor alone, so nothing else changes the
// board between an edit and its undo: undo must then return the board to
// exactly how it was before the edit, stacking order included, and redo to
// exactly how it was after (4.10). Edits mix additions, moves with carry,
// text, multi-element stacking changes, deletions, revives, membership,
// groups and metadata; undo and redo come in runs. BOARD_SEEDS sets the
// number of seeds (default 500).
func TestRandomUndoRedo(t *testing.T) {
	forSeeds(t, 500, runUndoRedo)
}

// boardView is the board as undo must restore it: the metadata and every
// alive element's content, author fields included, without its stacking
// key, by id, and separately the stacking order. Incarnations and versions
// may differ.
type boardView struct{ content, order string }

func viewOf(s *State) boardView {
	var b strings.Builder
	b.Write(s.Board().CanonicalJSON())
	for _, p := range s.Alive() {
		c := p.Content()
		delete(c, "z")
		b.WriteString("\n")
		b.Write(model.Canonical(c))
	}
	return boardView{b.String(), strings.Join(s.order(), " ")}
}

func runUndoRedo(seed uint64) error {
	rng := rand.New(rand.NewPCG(seed, 0x0d0))
	s := New("Board")
	se := s.Hello(Hello{NewID: "s-a", Key: "browser:a", Label: "a", Now: 1}).Session
	var n int64
	apply := func(ops ...Op) error {
		n++
		out := s.ApplyBatch(Batch{Session: se, N: n, Ops: ops, Now: 1})
		if out.Reject != nil || out.Fatal != "" {
			return fmt.Errorf("batch %s refused: %+v %s", raw(ops), out.Reject, out.Fatal)
		}
		return checkInvariants(s)
	}
	top := func(name string) *UndoEntry {
		if st := s.undo[se]; st != nil && len(st.stack(name)) > 0 {
			return st.stack(name)[len(st.stack(name))-1]
		}
		return &UndoEntry{}
	}
	// A revive keeps the stacking key an element had when deleted, so its
	// position among elements moved meanwhile is not restored (4.10 does
	// not promise it). An undo or redo records its opposite entry from the
	// order it found, so once an edit was undone or redone from another
	// order than the one it was made in, or gave another order through a
	// revive, the orders it gives are no longer compared.
	type edit struct {
		before, after boardView
		ops           []Op
		idx           int64 // its entry on the stack it is on
		exact         bool
	}
	var undos, redos []*edit
	same := func(what string, e *edit, pre, from, got, want boardView, revives bool) error {
		if got.content != want.content {
			return fmt.Errorf("%s of %s gave\n%s\nwant\n%s", what, raw(e.ops), got.content, want.content)
		}
		if e.exact = e.exact && pre.order == from.order; e.exact && got.order != want.order {
			if !revives {
				return fmt.Errorf("%s of %s gave order\n%s\nwant\n%s", what, raw(e.ops), got.order, want.order)
			}
			e.exact = false
		}
		return nil
	}
	next := 0
	for step := range 80 {
		switch k := rng.IntN(10); {
		case k < 2 && len(undos) > 0:
			for range 1 + rng.IntN(len(undos)) {
				e := undos[len(undos)-1]
				if top(StackUndo).Idx != e.idx {
					return fmt.Errorf("step %d: undo stack top %d, want %d", step, top(StackUndo).Idx, e.idx)
				}
				revives, was, pre := top(StackUndo).Deletes != nil, top(StackRedo).Idx, viewOf(s)
				if err := apply(undoOp); err != nil {
					return fmt.Errorf("step %d: %w", step, err)
				}
				// An entry whose batch left no visible change pushes nothing.
				if undos = undos[:len(undos)-1]; top(StackRedo).Idx != was {
					e.idx, redos = top(StackRedo).Idx, append(redos, e)
				}
				if err := same("undo", e, pre, e.after, viewOf(s), e.before, revives); err != nil {
					return fmt.Errorf("step %d: %w", step, err)
				}
			}
		case k < 4 && len(redos) > 0:
			for range 1 + rng.IntN(len(redos)) {
				e := redos[len(redos)-1]
				if top(StackRedo).Idx != e.idx {
					return fmt.Errorf("step %d: redo stack top %d, want %d", step, top(StackRedo).Idx, e.idx)
				}
				revives, was, pre := top(StackRedo).Deletes != nil, top(StackUndo).Idx, viewOf(s)
				if err := apply(redoOp); err != nil {
					return fmt.Errorf("step %d: %w", step, err)
				}
				if redos = redos[:len(redos)-1]; top(StackUndo).Idx != was {
					e.idx, undos = top(StackUndo).Idx, append(undos, e)
				}
				if err := same("redo", e, pre, e.before, viewOf(s), e.after, revives); err != nil {
					return fmt.Errorf("step %d: %w", step, err)
				}
			}
		default:
			ops := soloOps(rng, s, &next)
			if len(ops) == 0 {
				continue
			}
			before, was := viewOf(s), top(StackUndo).Idx
			if err := apply(ops...); err != nil {
				return fmt.Errorf("step %d: %w", step, err)
			}
			if top(StackUndo).Idx != was {
				undos, redos = append(undos, &edit{before, viewOf(s), ops, top(StackUndo).Idx, true}), nil
			}
		}
	}
	return nil
}

// TestRandomUndoTwoEditors interleaves two editors' edits, undos and redos.
// The board may end anywhere, but an undo or redo never deletes an element
// another session changed since its creation (4.10 step 2 and the guards
// judging the start of the undo): a stale record must not pass because a
// side effect of the same undo, such as a frame deletion releasing its
// members, already wrote the record's previous value. Batches sometimes
// create a frame and move elements into it at once, so one undo both
// deletes the frame and finds its members' membership records.
// BOARD_SEEDS sets the number of seeds (default 500).
func TestRandomUndoTwoEditors(t *testing.T) {
	forSeeds(t, 500, runUndoTwoEditors)
}

func runUndoTwoEditors(seed uint64) error {
	rng := rand.New(rand.NewPCG(seed, 0x2ed))
	s := New("Board")
	sessions := []string{
		s.Hello(Hello{NewID: "s-a", Key: "browser:a", Label: "a", Now: 1}).Session,
		s.Hello(Hello{NewID: "s-b", Key: "browser:b", Label: "b", Now: 1}).Session,
	}
	ns := map[string]int64{}
	// writers holds, by element id, the sessions that changed it (a bundle
	// version change or a deletion) since it last came alive. A creation
	// record holds the versions from that moment: an undo or redo deleting
	// the element again (undoing a revive) compares with those.
	writers := map[string]map[string]bool{}
	type snap struct {
		alive bool
		inc   int64
		v     map[string]int64
	}
	next := 0
	for step := range 120 {
		se := sessions[rng.IntN(2)]
		var ops []Op
		kind := ""
		switch k := rng.IntN(10); {
		case k < 3:
			ops, kind = []Op{undoOp}, "undo"
		case k < 4:
			ops, kind = []Op{redoOp}, "redo"
		default:
			ops = soloOps(rng, s, &next)
			if rng.IntN(3) == 0 {
				ops = append(ops, frameAndReparent(rng, s, &next)...)
			}
			if len(ops) == 0 {
				continue
			}
		}
		before := map[string]snap{}
		for id, r := range s.rows {
			before[id] = snap{r.alive(), r.Inc, maps.Clone(r.V)}
		}
		ns[se]++
		out := s.ApplyBatch(Batch{Session: se, N: ns[se], Ops: ops, Now: 1})
		if out.Reject != nil || out.Fatal != "" {
			return fmt.Errorf("step %d: batch %s refused: %+v %s", step, raw(ops), out.Reject, out.Fatal)
		}
		if err := checkInvariants(s); err != nil {
			return fmt.Errorf("step %d: %w", step, err)
		}
		for id, r := range s.rows {
			if id == model.BoardID {
				continue
			}
			b, existed := before[id]
			switch {
			case (!existed || !b.alive) && r.alive():
				writers[id] = map[string]bool{}
				continue
			case !r.alive() && (!existed || !b.alive):
				continue
			case b.alive && !r.alive() && kind != "":
				for w := range writers[id] {
					if w != se {
						return fmt.Errorf("step %d: %s of %s deleted %s, which %s changed after its creation", step, kind, se, id, w)
					}
				}
			case b.alive && r.alive() && b.inc == r.Inc && maps.Equal(b.v, r.V):
				continue
			}
			if writers[id] == nil {
				writers[id] = map[string]bool{}
			}
			writers[id][se] = true
		}
	}
	return nil
}

// frameAndReparent creates a frame and moves one or two elements into it in
// the same batch.
func frameAndReparent(rng *rand.Rand, s *State, next *int) []Op {
	var ids []string
	incOf := map[string]int64{}
	for _, e := range s.Alive() {
		if e.Type != model.TypeFrame && rng.IntN(3) == 0 && len(ids) < 2 {
			ids = append(ids, e.ID)
			incOf[e.ID] = s.rows[e.ID].Inc
		}
	}
	if len(ids) == 0 {
		return nil
	}
	*next++
	f := fmt.Sprintf("e_u%09d", *next)
	add := Op{Op: OpAdd, El: raw(map[string]any{"id": f, "type": "frame", "x": rng.IntN(1000), "y": rng.IntN(1000), "w": 600, "h": 600})}
	// An element created in this batch has the batch's seq as incarnation.
	return []Op{add, {Op: OpReparent, IDs: ids, IncOf: incOf, Frame: raw(f), FrameInc: s.Seq() + 1}}
}

// soloOps is a batch of one to three operations on the current board,
// groups and ungroups included: undo checks the group invariant of revived
// elements only after it restores relations (4.10 step 4), so a revived
// group member whose mate's frame step 4 restores keeps its group.
func soloOps(rng *rand.Rand, s *State, next *int) []Op {
	alive := s.Alive()
	ids := make([]string, len(alive))
	for i, e := range alive {
		ids[i] = e.ID
	}
	pick := func(filter func(model.PrivateElement) bool) string {
		var ok []string
		for _, e := range alive {
			if filter(e) {
				ok = append(ok, e.ID)
			}
		}
		if len(ok) == 0 {
			return ""
		}
		return ok[rng.IntN(len(ok))]
	}
	inc := func(id string) int64 { r := s.rows[id]; return r.Inc }
	anyEl := func(model.PrivateElement) bool { return true }
	isFrame := func(e model.PrivateElement) bool { return e.Type == model.TypeFrame }
	target := func(e model.PrivateElement) bool { return e.Type != model.TypeConnector }
	var ops []Op
	for range 1 + rng.IntN(3) {
		k := rng.IntN(13)
		if len(ids) < 3 {
			k = 0
		}
		switch {
		case k < 3:
			*next++
			id := fmt.Sprintf("e_u%09d", *next)
			el := map[string]any{"id": id, "type": "sticky", "x": rng.IntN(1000), "y": rng.IntN(1000), "w": 100, "h": 100}
			refs := map[string]int64{}
			switch rng.IntN(4) {
			case 0:
				el["type"], el["w"], el["h"] = "frame", 600, 600
			case 1:
				if t := pick(target); t != "" {
					el = map[string]any{"id": id, "type": "connector", "ends": []any{map[string]any{"el": t, "at": []float64{1, 0.5}}, map[string]any{"pt": []int{9, 9}}}}
					refs[t] = inc(t)
				}
			default:
				if f := pick(isFrame); f != "" && rng.IntN(2) == 0 {
					el["frame"], refs[f] = f, inc(f)
				}
			}
			ops = append(ops, Op{Op: OpAdd, El: raw(el), Refs: refs})
		case k < 5:
			id := pick(target)
			if id == "" {
				continue
			}
			g := s.rows[id].El.Geom
			g.X, g.Y = model.Round1(g.X+float64(rng.IntN(200)-100)), model.Round1(g.Y+float64(rng.IntN(200)-100))
			carry := ""
			if s.rows[id].El.Type == model.TypeFrame && rng.IntN(2) == 0 {
				carry = CarryMembers
			} else if s.rows[id].El.Group != "" && rng.IntN(2) == 0 {
				carry = CarryGroup
			}
			ops = append(ops, Op{Op: OpSet, ID: id, Inc: inc(id), G: model.BundleGeom, Val: raw(g), Carry: carry})
		case k < 6:
			if id := pick(func(e model.PrivateElement) bool { return model.HasText(e.Type) }); id != "" {
				ops = append(ops, Op{Op: OpSet, ID: id, Inc: inc(id), G: model.BundleText, Val: raw(fmt.Sprint("t", rng.IntN(9)))})
			}
		case k < 8:
			ops = append(ops, zorderOf(rng, pick(anyEl), func() string { return pick(anyEl) }, inc))
		case k < 9:
			a, b := pick(anyEl), pick(anyEl)
			ops = append(ops, Op{Op: OpDelete, IDs: []string{a, b}, IncOf: map[string]int64{a: inc(a), b: inc(b)}})
		case k < 10:
			a, b := pick(anyEl), pick(anyEl)
			op := Op{Op: OpReparent, IDs: []string{a, b}, IncOf: map[string]int64{a: inc(a), b: inc(b)}, Frame: raw(nil)}
			if f := pick(isFrame); f != "" && rng.IntN(3) > 0 {
				op.Frame, op.FrameInc = raw(f), inc(f)
			}
			ops = append(ops, op)
		case k < 11:
			if f := pick(isFrame); f != "" && rng.IntN(2) == 0 {
				ops = append(ops, Op{Op: OpDeleteFrameContents, Frame: raw(f), Inc: inc(f)})
			} else {
				ops = append(ops, Op{Op: OpMeta, G: model.BundleTitle, Val: raw(fmt.Sprint("title ", rng.IntN(5)))})
			}
		case k < 12:
			// Mostly a valid group: a second element in the same frame.
			a := pick(func(e model.PrivateElement) bool { return e.Type != model.TypeFrame })
			if a == "" {
				continue
			}
			b := pick(func(e model.PrivateElement) bool {
				return e.ID != a && e.Type != model.TypeFrame && (e.Frame == s.rows[a].El.Frame || rng.IntN(4) == 0)
			})
			if b == "" {
				continue
			}
			*next++
			ops = append(ops, Op{Op: OpGroup, IDs: []string{a, b}, IncOf: map[string]int64{a: inc(a), b: inc(b)}, Group: fmt.Sprintf("g_u%09d", *next)})
		default:
			if id := pick(func(e model.PrivateElement) bool { return e.Group != "" }); id != "" {
				ops = append(ops, Op{Op: OpUngroup, Group: s.rows[id].El.Group})
			}
		}
	}
	return ops
}
