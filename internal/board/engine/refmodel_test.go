package engine

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gosuda/flats/internal/board/model"
)

// The reference model transcribes the concurrency rules of board.md (rules
// 27 to 34) directly, with none of the engine's mechanisms: no
// incarnations, bundle versions, receipts or pre_rel. An element is a set of
// property groups. A preparer names the appearance of each element it saw;
// an element appears when it is added, restored or brought back by a
// rollback. Arrival order is the order of calls.

type refEl struct {
	alive    bool
	appeared int               // arrival at which the element (re)appeared
	deleted  int               // arrival of the deletion that removed it
	changed  int               // arrival of the last change, for agent guards
	props    map[string]string // property group → value (JSON)
}

type refBoard struct {
	els     map[string]*refEl
	arrival int
	changes int // count of changes, for the whole-board agent guard
}

func newRefBoard() *refBoard { return &refBoard{els: map[string]*refEl{}} }

func (b *refBoard) clone() *refBoard {
	c := &refBoard{els: map[string]*refEl{}, arrival: b.arrival, changes: b.changes}
	for id, e := range b.els {
		ec := *e
		ec.props = maps.Clone(e.props)
		c.els[id] = &ec
	}
	return c
}

// refOp is a person operation as the reference model sees it. Seen holds
// the appearance the preparer saw of each element it names.
type refOp struct {
	kind  string // add, set, delete, restore, group, ungroup
	ids   []string
	seen  map[string]int
	prop  string
	value string
	props map[string]string // add
	group string
}

// person applies one person action and reports for each operation whether
// it applied (true) or was ignored (false). Rule 30: deletion wins; rule
// 31: an element that reappeared after the preparer saw it ignores the edit;
// rule 3: a lock blocks direct changes but not the lock itself; rule 34:
// grouping is one operation.
func (b *refBoard) person(ops []refOp) []bool {
	return b.personEach(len(ops), func(i int) refOp { return ops[i] }, nil)
}

// personEach is person for n operations that op gives one at a time, just
// before each applies; after, when set, learns each operation's outcome.
func (b *refBoard) personEach(n int, op func(i int) refOp, after func(i int, applied bool)) []bool {
	b.arrival++
	applied := make([]bool, n)
	changed := false
	for i := range n {
		applied[i] = b.apply(op(i))
		changed = changed || applied[i]
		if after != nil {
			after(i, applied[i])
		}
	}
	if changed {
		b.changes++
	}
	return applied
}

func (b *refBoard) target(id string, seen map[string]int) *refEl {
	e := b.els[id]
	if e == nil || !e.alive || e.appeared != seen[id] {
		return nil
	}
	return e
}

func (b *refBoard) apply(op refOp) bool {
	switch op.kind {
	case "add":
		if b.els[op.ids[0]] != nil {
			return false
		}
		e := &refEl{alive: true, appeared: b.arrival, changed: b.arrival, props: maps.Clone(op.props)}
		e.wrote(model.BundleText, model.KindMember)
		b.els[op.ids[0]] = e
		return true
	case "set":
		e := b.target(op.ids[0], op.seen)
		if e == nil || (e.props["locked"] == "true" && op.prop != "locked") {
			return false
		}
		e.props[op.prop], e.changed = op.value, b.arrival
		e.wrote(op.prop, model.KindMember)
		return true
	case "delete":
		any := false
		for _, id := range op.ids {
			if e := b.target(id, op.seen); e != nil && e.props["locked"] != "true" {
				e.alive, e.deleted, e.changed = false, b.arrival, b.arrival
				any = true
			}
		}
		return any
	case "restore":
		any := false
		for _, id := range op.ids {
			if e := b.els[id]; e != nil && !e.alive && e.deleted == op.seen[id] {
				e.alive, e.appeared, e.changed = true, b.arrival, b.arrival
				any = true
			}
		}
		return any
	case "group":
		for _, id := range op.ids {
			e := b.target(id, op.seen)
			if e == nil || e.props["locked"] == "true" || e.props["group"] != "null" {
				return false
			}
		}
		for _, id := range op.ids {
			b.els[id].props["group"], b.els[id].changed = strconv.Quote(op.group), b.arrival
			b.els[id].wrote("group", model.KindMember)
		}
		return true
	case "ungroup":
		any := false
		for _, e := range b.els {
			if e.alive && e.props["group"] == strconv.Quote(op.group) {
				e.props["group"], e.changed, any = "null", b.arrival, true
				e.wrote("group", model.KindMember)
			}
		}
		return any
	}
	panic("unknown reference operation " + op.kind)
}

// rollback makes the board exactly R's board (rule 32). Elements that come
// back count as reappearing.
func (b *refBoard) rollback(r *refBoard) {
	b.arrival++
	b.changes++
	for id, e := range b.els {
		if re, ok := r.els[id]; ok && re.alive {
			if !e.alive {
				e.appeared, e.changed = b.arrival, b.arrival
			} else if !maps.Equal(e.props, re.props) {
				e.changed = b.arrival
			}
			e.alive, e.props = true, maps.Clone(re.props)
		} else if e.alive {
			e.alive, e.deleted, e.changed = false, b.arrival, b.arrival
		}
	}
	for id, re := range r.els {
		if _, ok := b.els[id]; !ok && re.alive {
			b.els[id] = &refEl{alive: true, appeared: b.arrival, changed: b.arrival, props: maps.Clone(re.props)}
		}
	}
}

// agent applies an agent call whole or not at all (rule 33): a missing or
// locked target, or a guard that does not match, refuses it.
func (b *refBoard) agent(ops []refOp, ifChanges *int, ifChanged map[string]int) bool {
	if ifChanges != nil && *ifChanges != b.changes {
		return false
	}
	for id, at := range ifChanged {
		if e := b.els[id]; e == nil || !e.alive || e.changed != at {
			return false
		}
	}
	trial := b.clone()
	trial.arrival++
	for _, op := range ops {
		e := trial.els[op.ids[0]]
		unlock := op.kind == "set" && op.prop == "locked" && op.value == "false"
		if e == nil || !e.alive || (e.props["locked"] == "true" && !unlock) {
			return false
		}
		switch op.kind {
		case "set":
			e.props[op.prop], e.changed = op.value, trial.arrival
			e.wrote(op.prop, model.KindAgent)
		case "delete":
			e.alive, e.deleted, e.changed = false, trial.arrival, trial.arrival
		}
	}
	trial.changes++
	*b = *trial
	return true
}

// refProps reads an engine element as reference property groups, with
// its authorship as two more: who last wrote the text and the kind of the
// last change (rule 27).
func refProps(el model.Element) map[string]string {
	p := map[string]string{}
	for _, g := range []string{model.BundleGeom, model.BundleText, "style.color", model.BundleLocked, model.BundleGroup} {
		p[g] = string(model.BundleJSON(g, el.Bundle(g)))
	}
	p["ta.text"], p["lk"] = el.TA[model.BundleText].K, el.LK
	return p
}

// wrote records a write of property prop by an editor of kind k.
func (e *refEl) wrote(prop, k string) {
	e.props["lk"] = k
	if prop == model.BundleText {
		e.props["ta.text"] = k
	}
}

// compareRef checks that the engine's alive elements equal the model's.
func compareRef(s *State, b *refBoard) error {
	alive := map[string]bool{}
	for id, r := range s.rows {
		if !r.alive() || id == model.BoardID {
			continue
		}
		alive[id] = true
		e := b.els[id]
		if e == nil || !e.alive {
			return fmt.Errorf("%s is alive in the engine only", id)
		}
		if got := refProps(r.El); !maps.Equal(got, e.props) {
			return fmt.Errorf("%s: engine %v, reference %v", id, got, e.props)
		}
	}
	for id, e := range b.els {
		if e.alive && !alive[id] {
			return fmt.Errorf("%s is alive in the reference only", id)
		}
	}
	return nil
}

// forSeeds runs fn for seeds 0 to n-1 on every CPU and fails with the
// lowest failing seed. BOARD_SEEDS overrides n, for example 100000 before
// a release.
func forSeeds(t *testing.T, n int, fn func(seed uint64) error) {
	if v, err := strconv.Atoi(os.Getenv("BOARD_SEEDS")); err == nil && v > 0 {
		n = v
	}
	// Seeds allocate many small, short-lived values; with the default GC
	// target, collection dominated the run time.
	defer debug.SetGCPercent(debug.SetGCPercent(400))
	var (
		next    atomic.Int64
		mu      sync.Mutex
		failed  = n
		failure error
		wg      sync.WaitGroup
	)
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for {
				seed := int(next.Add(1) - 1)
				mu.Lock()
				stop := seed >= n || seed > failed
				mu.Unlock()
				if stop {
					return
				}
				if err := fn(uint64(seed)); err != nil {
					mu.Lock()
					if seed < failed {
						failed, failure = seed, err
					}
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	if failure != nil {
		t.Fatalf("seed %d: %v", failed, failure)
	}
}

// view is what one preparer saw when it last synced: each element's
// incarnation (engine) and appearance (reference), and deletions.
type view struct {
	inc      map[string]int64
	appeared map[string]int
	delSeq   map[string]int64
	deleted  map[string]int
	ver      map[string]int64
	changed  map[string]int
	seq      int64
	changes  int
	values   map[string]map[string]string
}

func takeView(s *State, b *refBoard) view {
	v := view{inc: map[string]int64{}, appeared: map[string]int{}, delSeq: map[string]int64{}, deleted: map[string]int{},
		ver: map[string]int64{}, changed: map[string]int{}, seq: s.Seq(), changes: b.changes, values: map[string]map[string]string{}}
	for id, r := range s.rows {
		if id == model.BoardID {
			continue
		}
		e := b.els[id]
		if r.alive() {
			v.inc[id], v.appeared[id], v.ver[id], v.changed[id] = r.Inc, e.appeared, r.ver(), e.changed
			v.values[id] = maps.Clone(e.props)
		} else {
			v.delSeq[id], v.deleted[id] = r.DelSeq, e.deleted
		}
	}
	return v
}

// TestReferenceModel drives the engine and the reference model with the
// same actions, prepared by several people from views of different ages,
// and checks after every action that both hold the same board and judged
// every operation the same way.
func TestReferenceModel(t *testing.T) {
	forSeeds(t, 2000, runReference)
}

func runReference(seed uint64) error {
	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	h := &harness{t: nil, s: New("Board"), now: 1_760_000_000, n: map[string]int64{}}
	ref := newRefBoard()
	const people = 3
	sessions := make([]string, people)
	views := make([]view, people)
	for i := range sessions {
		sessions[i] = h.s.Hello(Hello{NewID: fmt.Sprintf("s%d", i), Key: fmt.Sprintf("browser:%d", i), Now: h.now}).Session
		views[i] = takeView(h.s, ref)
	}
	type revision struct {
		c   Content
		ref *refBoard
	}
	var revisions []revision
	next := 0
	for step := range 120 {
		p := rng.IntN(people)
		v := &views[p]
		pick := func(m map[string]int64) string {
			if len(m) == 0 {
				return ""
			}
			keys := slices.Sorted(maps.Keys(m))
			return keys[rng.IntN(len(keys))]
		}
		switch k := rng.IntN(20); {
		case k < 2:
			views[p] = takeView(h.s, ref)
			continue
		case k < 3:
			revisions = append(revisions, revision{content(h.s), ref.clone()})
			continue
		case k < 4 && len(revisions) > 0:
			r := revisions[rng.IntN(len(revisions))]
			if _, err := h.s.Rollback(1, r.c, h.now); err != nil {
				return err
			}
			ref.rollback(r.ref)
		case k < 7:
			if _, err := agentStep(h, ref, v, rng, pick); err != nil {
				return fmt.Errorf("step %d: %w", step, err)
			}
		default:
			var ops []Op
			var rops []refOp
			kinds := map[string]bool{}
			for range 1 + rng.IntN(3) {
				op, rop, ok := personOp(rng, v, pick, &next)
				// Revive restores relations from the start of the deleting
				// batch (a TECH mechanism outside the reference model), so
				// one batch never both regroups and deletes.
				grouping := rop.kind == "group" || rop.kind == "ungroup"
				if !ok || (grouping && kinds["delete"]) || (rop.kind == "delete" && (kinds["group"] || kinds["ungroup"])) {
					continue
				}
				kinds[rop.kind] = true
				ops, rops = append(ops, op), append(rops, rop)
			}
			if len(ops) == 0 {
				continue
			}
			h.n[sessions[p]]++
			out := h.s.ApplyBatch(Batch{Session: sessions[p], N: h.n[sessions[p]], Ops: ops, Now: h.now})
			if out.Reject != nil || out.Fatal != "" {
				return fmt.Errorf("step %d: refused %+v %s", step, out.Reject, out.Fatal)
			}
			want := ref.person(rops)
			var got []json.RawMessage
			json.Unmarshal(out.Results, &got)
			for i := range ops {
				if applied := resultApplied(got[i]); applied != want[i] {
					return fmt.Errorf("step %d op %d (%s): engine applied=%v (%s), reference %v", step, i, ops[i].Op, applied, got[i], want[i])
				}
				// the preparer knows its own new elements
				if rops[i].kind == "add" && want[i] {
					v.inc[rops[i].ids[0]], v.appeared[rops[i].ids[0]] = 0, ref.arrival
				}
			}
		}
		if err := checkInvariants(h.s); err != nil {
			return fmt.Errorf("step %d: %w", step, err)
		}
		if err := compareRef(h.s, ref); err != nil {
			return fmt.Errorf("step %d: %w", step, err)
		}
	}
	return nil
}

func resultApplied(raw json.RawMessage) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == ResultOK
	}
	var m MultiResult
	json.Unmarshal(raw, &m)
	return len(m.OK) > 0
}

// personOp prepares one operation from a view, for both models.
func personOp(rng *rand.Rand, v *view, pick func(map[string]int64) string, next *int) (Op, refOp, bool) {
	id := pick(v.inc)
	seen := map[string]int{id: v.appeared[id]}
	switch k := rng.IntN(10); {
	case k < 2 || id == "":
		*next++
		nid := fmt.Sprintf("e_n%09d", *next)
		el := model.Element{ID: nid, Type: model.TypeSticky, Geom: model.Geom{X: float64(rng.IntN(1000)), Y: 0, W: 100, H: 100}, Z: "V", Style: model.DefaultStyle(model.TypeSticky)}
		return Op{Op: OpAdd, El: el.CanonicalJSON()}, refOp{kind: "add", ids: []string{nid}, props: refProps(el)}, true
	case k < 6:
		prop, val := randomProp(rng)
		return Op{Op: OpSet, ID: id, Inc: v.inc[id], G: prop, Val: json.RawMessage(val)},
			refOp{kind: "set", ids: []string{id}, seen: seen, prop: prop, value: val}, true
	case k < 7:
		return Op{Op: OpDelete, IDs: []string{id}, IncOf: map[string]int64{id: v.inc[id]}}, refOp{kind: "delete", ids: []string{id}, seen: seen}, true
	case k < 8:
		gone := pick(v.delSeq)
		if gone == "" {
			return Op{}, refOp{}, false
		}
		return Op{Op: OpRevive, IDs: []string{gone}, IfDel: v.delSeq[gone]},
			refOp{kind: "restore", ids: []string{gone}, seen: map[string]int{gone: v.deleted[gone]}}, true
	case k < 9:
		other := pick(v.inc)
		if other == id {
			return Op{}, refOp{}, false
		}
		*next++
		g := fmt.Sprintf("g_n%09d", *next)
		seen[other] = v.appeared[other]
		return Op{Op: OpGroup, IDs: []string{id, other}, IncOf: map[string]int64{id: v.inc[id], other: v.inc[other]}, Group: g},
			refOp{kind: "group", ids: []string{id, other}, seen: seen, group: g}, true
	default:
		var g string
		json.Unmarshal([]byte(v.values[id][model.BundleGroup]), &g)
		if g == "" {
			return Op{}, refOp{}, false
		}
		return Op{Op: OpUngroup, Group: g}, refOp{kind: "ungroup", group: g}, true
	}
}

func randomProp(rng *rand.Rand) (string, string) {
	switch rng.IntN(4) {
	case 0:
		return model.BundleGeom, string(model.Canonical(model.Geom{X: float64(rng.IntN(500)), Y: float64(rng.IntN(500)), W: 100, H: 100}))
	case 1:
		return model.BundleText, strconv.Quote(fmt.Sprint("t", rng.IntN(5)))
	case 2:
		return "style.color", strconv.Quote(model.StickyColors[rng.IntN(len(model.StickyColors))])
	}
	return model.BundleLocked, strconv.FormatBool(rng.IntN(3) == 0)
}

// agentStep prepares an agent call from a view, with or without guards. It
// returns the engine's outcome, which is empty when the call was refused.
func agentStep(h *harness, ref *refBoard, v *view, rng *rand.Rand, pick func(map[string]int64) string) (AgentOutcome, error) {
	var ops []AgentOp
	var rops []refOp
	var ifSeq *int64
	var ifChanges *int
	ifVer := map[string]int{}
	for range 1 + rng.IntN(3) {
		id := pick(v.inc)
		if id == "" {
			return AgentOutcome{}, nil
		}
		if rng.IntN(5) == 0 {
			ops = append(ops, AgentOp{Op: "delete", IDs: []string{id}})
			rops = append(rops, refOp{kind: "delete", ids: []string{id}})
			continue
		}
		prop, val := randomProp(rng)
		field := map[string]json.RawMessage{}
		switch prop {
		case model.BundleGeom:
			var g model.Geom
			json.Unmarshal([]byte(val), &g)
			// an agent names fields; the rest of the bundle comes from the
			// current value, so give the whole bundle to compare exactly
			field = map[string]json.RawMessage{"x": raw(g.X), "y": raw(g.Y), "w": raw(g.W), "h": raw(g.H), "rot": raw(g.Rot)}
		case "style.color":
			field["style"] = raw(map[string]json.RawMessage{"color": json.RawMessage(val)})
		default:
			field[prop] = json.RawMessage(val)
		}
		op := AgentOp{Op: "set", ID: id, Fields: raw(field)}
		if rng.IntN(3) == 0 {
			op.IfVer = raw(v.ver[id])
			ifVer[id] = v.changed[id]
		}
		ops = append(ops, op)
		rops = append(rops, refOp{kind: "set", ids: []string{id}, prop: prop, value: val})
	}
	if rng.IntN(3) == 0 {
		seq, changes := v.seq, v.changes
		ifSeq, ifChanges = &seq, &changes
	}
	out, err := h.s.ApplyAgent(AgentCall{Ops: ops, IfSeq: ifSeq, Label: "agent", Now: h.now, NewID: func(string) string { return "" }})
	want := ref.agent(rops, ifChanges, ifVer)
	if (err == nil) != want {
		return AgentOutcome{}, fmt.Errorf("agent call %v: engine err %v, reference applied %v", ops, err, want)
	}
	return out, nil
}
