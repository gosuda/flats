package engine

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/board/model"
)

// eid turns a short name into an element id.
func eid(name string) string { return "e_" + name + strings.Repeat("0", 10-len(name)) }

func raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

type harness struct {
	t   testing.TB
	s   *State
	now int64
	n   map[string]int64
	ids int
}

func newHarness(t testing.TB) *harness {
	return &harness{t: t, s: New("Board"), now: 1_760_000_000, n: map[string]int64{}}
}

func (h *harness) session(name string) string {
	h.t.Helper()
	r := h.s.Hello(Hello{NewID: "s-" + name, Key: "browser:" + name, Label: name, Now: h.now})
	if r.Fatal != "" {
		h.t.Fatalf("hello %s: %s", name, r.Fatal)
	}
	return r.Session
}

// do applies a batch with the session's next number and checks invariants.
func (h *harness) do(session string, ops ...Op) Outcome {
	h.t.Helper()
	h.n[session]++
	out := h.s.ApplyBatch(Batch{Session: session, N: h.n[session], Ops: ops, Now: h.now})
	if err := checkInvariants(h.s); err != nil {
		h.t.Fatalf("after batch %d of %s: %v", h.n[session], session, err)
	}
	return out
}

// ok applies a batch that must not be refused.
func (h *harness) ok(session string, ops ...Op) Outcome {
	h.t.Helper()
	out := h.do(session, ops...)
	if out.Reject != nil || out.Fatal != "" {
		h.t.Fatalf("batch refused: %+v %s", out.Reject, out.Fatal)
	}
	return out
}

func (h *harness) agent(call AgentCall) (AgentOutcome, error) {
	h.t.Helper()
	if call.NewID == nil {
		call.NewID = func(prefix string) string {
			h.ids++
			return fmt.Sprintf("%sA%09d", prefix, h.ids)
		}
	}
	if call.Label == "" {
		call.Label = "agent"
	}
	call.Now = h.now
	out, err := h.s.ApplyAgent(call)
	if err := checkInvariants(h.s); err != nil {
		h.t.Fatalf("after agent call: %v", err)
	}
	return out, err
}

func (h *harness) row(id string) *Row {
	h.t.Helper()
	r := h.s.rows[id]
	if r == nil {
		h.t.Fatalf("no row %s", id)
	}
	return r
}

func (h *harness) inc(id string) int64 { return h.row(id).Inc }

func (h *harness) geom(id string) model.Geom { return h.row(id).El.Geom }

func (h *harness) alive(id string) bool { return h.s.rows[id].alive() }

// results decodes the results of an outcome.
func results(t testing.TB, out Outcome) []any {
	t.Helper()
	var rs []any
	if err := json.Unmarshal(out.Results, &rs); err != nil {
		t.Fatalf("results %s: %v", out.Results, err)
	}
	return rs
}

func content(s *State) Content {
	b := s.Board()
	c := Content{Title: b.Text, Grid: b.Grid, Assets: map[string]Asset{}}
	for _, p := range s.Alive() {
		c.Elements = append(c.Elements, p.Element)
		if p.Type == model.TypeImage {
			c.Assets[p.Asset] = *s.assets[p.Asset]
		}
	}
	return c
}

func elOf(fields map[string]any) json.RawMessage { return raw(fields) }

func addSticky(name string, x, y float64) Op {
	return Op{Op: OpAdd, El: elOf(map[string]any{"id": eid(name), "type": "sticky", "x": x, "y": y, "w": 100, "h": 100, "z": "V"})}
}

func addShape(name string, x, y, w, h, rot float64) Op {
	return Op{Op: OpAdd, El: elOf(map[string]any{"id": eid(name), "type": "shape", "x": x, "y": y, "w": w, "h": h, "rot": rot})}
}

func addFrame(name string, x, y, w, h float64) Op {
	return Op{Op: OpAdd, El: elOf(map[string]any{"id": eid(name), "type": "frame", "x": x, "y": y, "w": w, "h": h, "text": name})}
}

func addIn(name, frame string, frameInc int64, x, y float64) Op {
	return Op{Op: OpAdd, El: elOf(map[string]any{"id": eid(name), "type": "sticky", "x": x, "y": y, "w": 100, "h": 100, "frame": eid(frame)}),
		Refs: map[string]int64{eid(frame): frameInc}}
}

func attach(target string, ax, ay float64) map[string]any {
	return map[string]any{"el": eid(target), "at": []float64{ax, ay}}
}

func free(x, y float64) map[string]any { return map[string]any{"pt": []float64{x, y}} }

func addConnector(name string, e0, e1 map[string]any, refs map[string]int64) Op {
	return Op{Op: OpAdd, El: elOf(map[string]any{"id": eid(name), "type": "connector", "ends": []any{e0, e1}}), Refs: refs}
}

func setGeom(name string, inc int64, g model.Geom, carry string) Op {
	return Op{Op: OpSet, ID: eid(name), Inc: inc, G: model.BundleGeom, Val: raw(g), Carry: carry}
}

func move(h *harness, name string, x, y float64, carry string) Op {
	g := h.geom(eid(name))
	g.X, g.Y = x, y
	return setGeom(name, h.inc(eid(name)), g, carry)
}

func setText(name string, inc int64, text string, baseV int64) Op {
	return Op{Op: OpSet, ID: eid(name), Inc: inc, G: model.BundleText, Val: raw(text), BaseV: baseV}
}

func incOf(h *harness, names ...string) ([]string, map[string]int64) {
	ids := make([]string, len(names))
	m := map[string]int64{}
	for i, n := range names {
		ids[i] = eid(n)
		if r := h.s.rows[eid(n)]; r != nil {
			m[eid(n)] = r.Inc
		}
	}
	return ids, m
}

func deleteOp(h *harness, names ...string) Op {
	ids, m := incOf(h, names...)
	return Op{Op: OpDelete, IDs: ids, IncOf: m}
}

func reparentOp(h *harness, frame string, names ...string) Op {
	ids, m := incOf(h, names...)
	op := Op{Op: OpReparent, IDs: ids, IncOf: m, Frame: raw(nil)}
	if frame != "" {
		op.Frame, op.FrameInc = raw(eid(frame)), h.inc(eid(frame))
	}
	return op
}

func groupOp(h *harness, group string, names ...string) Op {
	ids, m := incOf(h, names...)
	return Op{Op: OpGroup, IDs: ids, IncOf: m, Group: "g_" + group + strings.Repeat("0", 10-len(group))}
}

var undoOp = Op{Op: OpUndo}
var redoOp = Op{Op: OpRedo}

func undoResult(t testing.TB, out Outcome) UndoResult {
	t.Helper()
	var rs []UndoResult
	if err := json.Unmarshal(out.Results, &rs); err != nil || len(rs) != 1 {
		t.Fatalf("undo results %s: %v", out.Results, err)
	}
	return rs[0]
}

// checkInvariants verifies the indexes and the board invariants: frames do
// not nest, members name alive frames, group members share a frame,
// attached ends name alive non-connector targets with their incarnation,
// alive elements pass the schema check (free ends within the coordinate
// range), deleted connectors keep their last end coordinates within it,
// asset counts match content, and stacking keys are valid.
func checkInvariants(s *State) error {
	alive := 0
	members := map[string]map[string]struct{}{}
	groups := map[string]map[string]struct{}{}
	attached := map[string]map[connEnd]struct{}{}
	buckets := map[int]map[string]struct{}{}
	refs, deleted := map[string]int{}, map[string]int{}
	groupFrame := map[string]string{}
	for id, r := range s.rows {
		if id == model.BoardID {
			continue
		}
		if r.El.ID != id {
			return fmt.Errorf("row %s holds %s", id, r.El.ID)
		}
		if r.El.Type == model.TypeImage {
			if r.Deleted {
				deleted[r.El.Asset]++
			} else {
				refs[r.El.Asset]++
			}
		}
		if r.El.Type == model.TypeConnector {
			// A deleted connector revives with free ends at EndLast.
			for k, p := range r.EndLast {
				if !(math.Abs(p[0]) <= model.MaxCoord && math.Abs(p[1]) <= model.MaxCoord) {
					return fmt.Errorf("connector %s end %d keeps last coordinate %v outside the range", id, k, p)
				}
			}
		}
		if r.Deleted {
			continue
		}
		alive++
		// Every alive element keeps its schema form, including free
		// connector ends derived from a deleted target (3.1).
		if err := r.El.Validate(); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		setIndex(buckets, model.BucketOf(id), id, 1)
		if !model.ValidKey(r.El.Z) {
			return fmt.Errorf("%s has stacking key %q", id, r.El.Z)
		}
		for _, g := range model.Bundles(r.El.Type) {
			if r.V[g] == 0 {
				return fmt.Errorf("%s bundle %s has no version", id, g)
			}
		}
		if f := r.El.Frame; f != "" {
			fr := s.rows[f]
			if !fr.alive() || fr.El.Type != model.TypeFrame || r.El.Type == model.TypeFrame {
				return fmt.Errorf("%s is a member of %s, which is not an alive frame", id, f)
			}
			setIndex(members, f, id, 1)
		}
		if g := r.El.Group; g != "" {
			if f, ok := groupFrame[g]; ok && f != r.El.Frame {
				return fmt.Errorf("group %s has members in frames %q and %q", g, f, r.El.Frame)
			}
			groupFrame[g] = r.El.Frame
			setIndex(groups, g, id, 1)
		}
		if r.El.Type == model.TypeConnector {
			for k, e := range r.El.Ends {
				if e.El == "" {
					continue
				}
				t := s.rows[e.El]
				if !t.alive() || t.El.Type == model.TypeConnector {
					return fmt.Errorf("connector %s end %d points at dead element %s", id, k, e.El)
				}
				if t.Inc != r.EndInc[k] {
					return fmt.Errorf("connector %s end %d has target incarnation %d, target has %d", id, k, r.EndInc[k], t.Inc)
				}
				setIndex(attached, e.El, connEnd{id, k}, 1)
			}
		}
	}
	if alive != s.alive {
		return fmt.Errorf("alive count %d, index says %d", alive, s.alive)
	}
	if !indexEqual(members, s.members) || !indexEqual(groups, s.groups) || !indexEqual(attached, s.attached) || !indexEqual(buckets, s.buckets) {
		return fmt.Errorf("an index differs from the rows")
	}
	live := 0
	for h, a := range s.assets {
		if a.Refs != refs[h] || a.DeletedRefs != deleted[h] {
			return fmt.Errorf("asset %s counts %d/%d, content says %d/%d", h, a.Refs, a.DeletedRefs, refs[h], deleted[h])
		}
		if a.Refs > 0 {
			live++
		}
	}
	for h := range refs {
		if s.assets[h] == nil {
			return fmt.Errorf("alive image asset %s has no row", h)
		}
	}
	if live != s.live {
		return fmt.Errorf("live assets %d, counter says %d", live, s.live)
	}
	return nil
}

func indexEqual[I, K comparable](a, b map[I]map[K]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k, set := range a {
		if !maps.Equal(set, b[k]) {
			return false
		}
	}
	return true
}

// sortedIDs lists the alive element ids.
func sortedIDs(s *State) []string {
	var ids []string
	for _, p := range s.Alive() {
		ids = append(ids, p.ID)
	}
	slices.Sort(ids)
	return ids
}
