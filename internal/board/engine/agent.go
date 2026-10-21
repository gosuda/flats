package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/gosuda/flats/internal/board/model"
)

// Agent refusal categories (8.3).
const (
	CategoryConflict = "conflict"
	CategoryInvalid  = "invalid"
)

// AgentError refuses a whole agent call; nothing changed.
type AgentError struct {
	Category string
	Msg      string
}

func (e *AgentError) Error() string { return e.Category + ": " + e.Msg }

func conflict(format string, args ...any) error {
	return &AgentError{CategoryConflict, fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) error {
	return &AgentError{CategoryInvalid, fmt.Sprintf(format, args...)}
}

// AgentOp is one operation of an update_board call (8.1). Elements are named
// by id or by a "$ref" an earlier add of the same call declared.
type AgentOp struct {
	Op     string          `json:"op"`
	Ref    string          `json:"ref,omitempty"`
	El     json.RawMessage `json:"el,omitempty"`
	ID     string          `json:"id,omitempty"`
	IDs    []string        `json:"ids,omitempty"`
	Fields json.RawMessage `json:"fields,omitempty"`
	IfVer  json.RawMessage `json:"if_ver,omitempty"`
	Carry  json.RawMessage `json:"carry,omitempty"`
	Frame  json.RawMessage `json:"frame,omitempty"`
	Group  string          `json:"group,omitempty"`
	To     json.RawMessage `json:"to,omitempty"`
	G      string          `json:"g,omitempty"`
	Val    json.RawMessage `json:"val,omitempty"`
}

// Layout is one layout help request (8.1).
type Layout struct {
	Kind    string   `json:"kind"` // grid, row, column
	IDs     []string `json:"ids"`
	In      string   `json:"in,omitempty"`
	Columns int      `json:"columns,omitempty"`
	Gap     *float64 `json:"gap,omitempty"`
}

// AgentCall is one update_board call.
type AgentCall struct {
	Ops     []AgentOp
	Layout  []Layout
	IfSeq   *int64
	Replace bool
	Label   string
	// By is the creator key new elements record; empty means "agent:"
	// plus Label.
	By string
	// Unlimited lifts the operation limit for data file imports and element
	// recovery, whose size the archive checks bound instead.
	Unlimited bool
	Now       int64
	// NewID returns a fresh random id with the prefix "e_" or "g_".
	NewID func(prefix string) string
}

// AgentOutcome is the result of an applied agent call.
type AgentOutcome struct {
	Seq     int64 // 0 when the call changed nothing
	IDs     map[string]string
	Layout  map[string][2]float64
	Results []any
	Delta   *model.Delta
	Changed Changed
}

var errEmptyReplace = errors.New("a replacement names its elements with add operations only")

// Layout defaults.
const (
	layoutGap   = 24
	layoutInset = 32
	placeGap    = 64
)

// ApplyAgent applies an agent call whole or not at all (rule 33). A guard
// mismatch, a missing or locked target gives a conflict; a malformed call or
// a limit gives invalid.
func (s *State) ApplyAgent(call AgentCall) (AgentOutcome, error) {
	if len(call.Ops) > model.MaxAgentOps && !call.Unlimited {
		return AgentOutcome{}, invalid("a call holds at most %d operations", model.MaxAgentOps)
	}
	targets := 0
	for _, l := range call.Layout {
		targets += len(l.IDs)
	}
	if targets > model.MaxLayoutTargets {
		return AgentOutcome{}, invalid("layout places at most %d elements per call", model.MaxLayoutTargets)
	}
	if call.IfSeq != nil && *call.IfSeq != s.seq {
		return AgentOutcome{}, conflict("the board changed since seq %d (now %d); read it again", *call.IfSeq, s.seq)
	}
	by := call.By
	if by == "" {
		by = "agent:" + call.Label
	}
	a, m := s.newApplier("", by, call.Now)
	c := &agentCall{a: a, call: call, refs: map[string]string{}, auto: map[string][2]bool{}, out: AgentOutcome{IDs: map[string]string{}, Layout: map[string][2]float64{}}}
	err := c.run()
	if err == nil {
		err = a.finish()
	}
	if err != nil {
		s.RollbackTo(m)
		var ae *AgentError
		switch {
		case errors.As(err, &ae):
			return AgentOutcome{}, err
		case errors.Is(err, model.ErrFormat), errors.Is(err, ErrLimit):
			return AgentOutcome{}, invalid("%v", err)
		}
		return AgentOutcome{}, err
	}
	if call.Replace {
		out := a.commitReset(ResetReplace, 0, m)
		c.out.Seq, c.out.Delta, c.out.Changed = out.Seq, out.Delta, out.Changed
		c.out.Delta.By = &model.By{Kind: "agent", Label: call.Label}
		return c.out, nil
	}
	if d := a.delta(); d != nil {
		d.By = &model.By{Kind: "agent", Label: call.Label}
		s.seq = a.seq
		c.out.Seq, c.out.Delta = a.seq, d
	}
	c.out.Changed = s.changedSince(a.sp)
	s.Release(m)
	return c.out, nil
}

type agentCall struct {
	a    *applier
	call AgentCall
	refs map[string]string
	out  AgentOutcome
	// connector ends this call attached without "at", by connector id
	auto map[string][2]bool
	// next free position for new elements without coordinates
	placed       bool
	nextX, nextY float64
	// elements the call's layout places explicitly
	laid map[string]bool
	// the ids and refs the call's layout names, known before any operation
	// runs, so an add can tell that layout will place its element
	layoutNames map[string]bool
}

func (c *agentCall) run() error {
	c.layoutNames = map[string]bool{}
	for _, l := range c.call.Layout {
		for _, ref := range l.IDs {
			c.layoutNames[ref] = true
		}
	}
	if c.call.Replace {
		return c.replace()
	}
	for i, op := range c.call.Ops {
		r, err := c.op(op)
		if err != nil {
			return fmt.Errorf("operation %d (%s): %w", i, op.Op, err)
		}
		c.out.Results = append(c.out.Results, r)
	}
	if err := c.layout(); err != nil {
		return err
	}
	c.placeEnds()
	return nil
}

// id resolves an id or "$ref".
func (c *agentCall) id(ref string) (string, error) {
	if !strings.HasPrefix(ref, "$") {
		return ref, nil
	}
	id, ok := c.refs[ref]
	if !ok {
		return "", invalid("%s names no element added earlier in this call", ref)
	}
	return id, nil
}

// alive resolves ref to an alive element or refuses the call.
func (c *agentCall) alive(ref string) (*Row, error) {
	id, err := c.id(ref)
	if err != nil {
		return nil, err
	}
	r := c.a.s.rows[id]
	if !r.alive() || id == model.BoardID {
		return nil, conflict("element %s does not exist; read the board again", id)
	}
	return r, nil
}

func (c *agentCall) unlocked(ref string) (*Row, error) {
	r, err := c.alive(ref)
	if err == nil && r.El.Locked {
		return nil, conflict("element %s is locked; unlock it first with set {locked: false}", r.El.ID)
	}
	return r, err
}

func (c *agentCall) op(op AgentOp) (any, error) {
	switch op.Op {
	case OpAdd:
		return c.add(op)
	case OpSet:
		return ResultOK, c.set(op)
	case OpReparent, OpGroup, OpDelete, OpZorder:
		return ResultOK, c.multi(op)
	case OpUngroup:
		// A group is no element, so its guard names members: {id: ver}.
		if err := c.ifVer(op.IfVer); err != nil {
			return nil, err
		}
		if len(c.a.s.groups[op.Group]) == 0 {
			return nil, conflict("group %s has no members", op.Group)
		}
		c.a.ungroup(Op{Group: op.Group})
		return ResultOK, nil
	case OpDeleteFrameContents:
		return ResultOK, c.deleteFrameContents(op)
	case OpMeta:
		if op.G != model.BundleTitle && op.G != model.BundleGrid {
			return nil, invalid("meta changes title or grid")
		}
		if _, err := model.DecodeBundle(model.TypeBoard, op.G, op.Val); err != nil {
			return nil, err
		}
		if err := c.ifVer(op.IfVer, model.BoardID); err != nil {
			return nil, err
		}
		c.a.meta(Op{G: op.G, Val: op.Val})
		return ResultOK, nil
	}
	return nil, invalid("unknown operation %q", op.Op)
}

// ifVer checks an if_ver guard, a number for one element or {id: ver},
// against the element as it was when the call started: the agent read the
// board before the call, so the call's own earlier operations do not count.
func (c *agentCall) ifVer(raw json.RawMessage, ids ...string) error {
	if len(raw) == 0 {
		return nil
	}
	want := map[string]int64{}
	var one int64
	if json.Unmarshal(raw, &one) == nil {
		if len(ids) != 1 {
			return invalid("if_ver for several elements is {id: ver}")
		}
		want[ids[0]] = one
	} else if err := json.Unmarshal(raw, &want); err != nil {
		return invalid("if_ver is a version or {id: ver}")
	}
	for ref, v := range want {
		id, err := c.id(ref)
		if err != nil {
			return err
		}
		r := c.a.batchRow(id)
		if !r.alive() {
			return conflict("element %s did not exist when the call started; read the board again", id)
		}
		if r.ver() != v {
			return conflict("element %s changed since version %d (now %d); read it again", id, v, r.ver())
		}
	}
	return nil
}

// add applies an agent add: a new id unless the element names one, "$ref"
// references, default sizes and a place for elements without coordinates.
func (c *agentCall) add(op AgentOp) (any, error) {
	el, present, err := model.DecodeNewElement(op.El)
	if err != nil {
		return nil, err
	}
	if err := c.checkRef(op.Ref); err != nil {
		return nil, err
	}
	if el.ID == "" {
		el.ID = c.call.NewID("e_")
	}
	if !model.ValidID(el.ID) {
		return nil, invalid("element id %q must be e_ plus 10 base62 characters", el.ID)
	}
	if c.a.s.rows[el.ID] != nil {
		return nil, conflict("element %s already exists", el.ID)
	}
	if el.Frame, err = c.id(el.Frame); err != nil {
		return nil, err
	}
	for k := range el.Ends {
		if el.Ends[k].El, err = c.id(el.Ends[k].El); err != nil {
			return nil, err
		}
	}
	if err := c.fillGeom(&el, present, c.layoutPlaces(el.ID, op.Ref)); err != nil {
		return nil, err
	}
	el.Normalize()
	if err := el.Validate(); err != nil {
		return nil, err
	}
	ref := func(id string) (*Row, bool) {
		r := c.a.s.rows[id]
		return r, r.alive() && id != model.BoardID
	}
	if c.a.checkRefs(el, ref) != "" {
		if el.Type == model.TypeImage && !c.a.assetKnown(el.Asset) {
			return nil, invalid("image asset %s was not uploaded", el.Asset)
		}
		return nil, conflict("element %s references an element or group that does not exist", el.ID)
	}
	if el.Type == model.TypeFrame && el.Z == "" {
		// A new frame goes behind everything, so the elements the call
		// then moves into it stay visible (8.1).
		el.Z = c.a.backKey(el.ID)
	}
	c.a.insert(el)
	if el.Type == model.TypeConnector {
		c.auto[el.ID] = omittedAt(op.El)
	}
	if op.Ref != "" {
		c.refs[op.Ref] = el.ID
		c.out.IDs[op.Ref] = el.ID
	}
	return el.ID, nil
}

// checkRef checks a ref an add declares: "$" plus a name, once per call.
func (c *agentCall) checkRef(ref string) error {
	if ref == "" {
		return nil
	}
	if !strings.HasPrefix(ref, "$") || len(ref) < 2 {
		return invalid("ref %q must start with $", ref)
	}
	if _, dup := c.refs[ref]; dup {
		return invalid("ref %s is declared twice", ref)
	}
	return nil
}

// layoutPlaces reports whether the call's layout names the element added
// with id and ref.
func (c *agentCall) layoutPlaces(id, ref string) bool {
	return c.layoutNames[id] || (ref != "" && c.layoutNames[ref])
}

// fillGeom gives a new element default sizes and, without coordinates, a
// place in a row to the right of the board's bounding box. A connector has
// no geometry but needs its two ends: only an attached end's "at" has a
// default (8.1).
//
// An element the call's layout places (laid) only waits for that layout:
// it takes no place in the row, and its temporary coordinates stay on the
// canvas, so a valid layout is not refused for where the row would have
// put it. Layout then checks the final place, and refuses the whole call
// when that is off the canvas.
func (c *agentCall) fillGeom(el *model.Element, present model.Present, laid bool) error {
	if el.Type == model.TypeConnector {
		if !present["ends"] {
			return invalid("a connector needs ends")
		}
		return nil
	}
	if el.Type == model.TypePen && !present.HasGeom() {
		return invalid("a pen needs x, y, w and h")
	}
	if !present["w"] || !present["h"] {
		w, h := defaultSize(*el)
		if !present["w"] {
			el.Geom.W = w
		}
		if !present["h"] {
			el.Geom.H = h
		}
	}
	if present["x"] && present["y"] {
		return nil
	}
	if !c.placed {
		box, ok := c.a.bounds()
		if ok {
			c.nextX, c.nextY = box.X+box.W+placeGap, box.Y
		}
		c.placed = true
	}
	// Only the coordinates the row fills are kept on the canvas; given ones
	// are checked as given.
	onCanvas := func(v float64) float64 {
		if laid {
			return math.Max(-model.MaxCoord, math.Min(model.MaxCoord, v))
		}
		return v
	}
	if !present["x"] {
		el.Geom.X = onCanvas(c.nextX)
	}
	if !present["y"] {
		el.Geom.Y = onCanvas(c.nextY)
	}
	if laid {
		return nil
	}
	c.nextX = el.Geom.X + el.Geom.W + layoutGap
	return nil
}

func defaultSize(el model.Element) (float64, float64) {
	switch el.Type {
	case model.TypeSticky:
		return 200, 200
	case model.TypeShape:
		return 160, 100
	case model.TypeFrame:
		return 800, 600
	case model.TypeImage:
		w, h := float64(el.Natural[0]), float64(el.Natural[1])
		if f := 800 / math.Max(w, h); f < 1 {
			w, h = w*f, h*f
		}
		return math.Max(w, 1), math.Max(h, 1)
	case model.TypeText:
		size, _ := el.Style["fontSize"].(float64)
		lines := strings.Split(el.Text, "\n")
		longest := 1
		for _, l := range lines {
			longest = max(longest, model.TextLen(l))
		}
		return math.Min(model.MaxSize, float64(longest)*size*0.6+16), math.Min(model.MaxSize, float64(len(lines))*size*1.4+8)
	}
	return 200, 200
}

// bounds is the bounding box of every alive element with geometry.
func (a *applier) bounds() (model.Geom, bool) {
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, r := range a.s.rows {
		if !r.alive() || r.El.Type == model.TypeBoard || r.El.Type == model.TypeConnector {
			continue
		}
		g := r.El.Geom
		minX, minY = math.Min(minX, g.X), math.Min(minY, g.Y)
		maxX, maxY = math.Max(maxX, g.X+g.W), math.Max(maxY, g.Y+g.H)
	}
	if math.IsInf(minX, 1) {
		return model.Geom{}, false
	}
	return model.Geom{X: minX, Y: minY, W: maxX - minX, H: maxY - minY}, true
}

// placedBounds is the bounding box of the elements that give both x and y,
// with default sizes where w or h is missing; connectors have no box.
func placedBounds(els []model.Element, presents []model.Present) (model.Geom, bool) {
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for i, el := range els {
		p := presents[i]
		if el.Type == model.TypeConnector || !p["x"] || !p["y"] {
			continue
		}
		g := el.Geom
		w, h := defaultSize(el)
		if !p["w"] {
			g.W = w
		}
		if !p["h"] {
			g.H = h
		}
		minX, minY = math.Min(minX, g.X), math.Min(minY, g.Y)
		maxX, maxY = math.Max(maxX, g.X+g.W), math.Max(maxY, g.Y+g.H)
	}
	if math.IsInf(minX, 1) {
		return model.Geom{}, false
	}
	return model.Geom{X: minX, Y: minY, W: maxX - minX, H: maxY - minY}, true
}

var geomFields = []string{"x", "y", "w", "h", "rot"}

// set applies an agent set: field names split into bundles, the rest of a
// partly given bundle filled from current values (8.1).
func (c *agentCall) set(op AgentOp) error {
	r, err := c.alive(op.ID)
	if err != nil {
		return err
	}
	id := r.El.ID
	if err := c.ifVer(op.IfVer, op.ID); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(op.Fields, &fields); err != nil || len(fields) == 0 {
		return invalid("set needs fields")
	}
	unlockOnly := len(fields) == 1 && string(fields["locked"]) == "false"
	if r.El.Locked && !unlockOnly {
		return conflict("element %s is locked; unlock it first with set {locked: false}", id)
	}
	vals, err := c.bundlesOf(r, fields)
	if err != nil {
		return err
	}
	carry, err := c.carry(r, op.Carry, fields)
	if err != nil {
		return err
	}
	// write uses carry only for geom.
	for _, g := range slices.Sorted(maps.Keys(vals)) {
		c.a.write(c.a.s.rows[id], g, vals[g], carry, 0)
	}
	return nil
}

func (c *agentCall) bundlesOf(r *Row, fields map[string]json.RawMessage) (map[string]any, error) {
	typ := r.El.Type
	vals := map[string]any{}
	geom := map[string]json.RawMessage{}
	for k, raw := range fields {
		switch {
		case slices.Contains(geomFields, k):
			geom[k] = raw
		case k == "style":
			part, err := model.DecodeStylePart(typ, raw)
			if err != nil {
				return nil, err
			}
			for sk, v := range part {
				vals[model.StylePrefix+sk] = v
			}
		case k == "ends":
			if err := c.endsOf(r.El.ID, typ, raw, vals); err != nil {
				return nil, err
			}
		case k == model.BundleText || k == model.BundleAlt || k == model.BundleLocked || k == model.BundleZ:
			if !model.Settable(typ, k) {
				return nil, invalid("a %s has no %s", typ, k)
			}
			v, err := model.DecodeBundle(typ, k, raw)
			if err != nil {
				return nil, err
			}
			vals[k] = v
		case k == "frame" || k == "group":
			return nil, invalid("change %s with the reparent, group or ungroup operations", k)
		default:
			return nil, invalid("a %s has no settable field %q", typ, k)
		}
	}
	if len(geom) > 0 {
		if !model.Settable(typ, model.BundleGeom) {
			return nil, invalid("a %s has no position or size", typ)
		}
		g := r.El.Geom
		full := map[string]any{"x": g.X, "y": g.Y, "w": g.W, "h": g.H, "rot": g.Rot}
		for k, raw := range geom {
			full[k] = raw
		}
		v, err := model.DecodeBundle(typ, model.BundleGeom, model.Canonical(full))
		if err != nil {
			return nil, err
		}
		vals[model.BundleGeom] = v
	}
	return vals, nil
}

func (c *agentCall) endsOf(id, typ string, raw json.RawMessage, vals map[string]any) error {
	if typ != model.TypeConnector {
		return invalid("a %s has no ends", typ)
	}
	var ends []json.RawMessage
	if err := json.Unmarshal(raw, &ends); err != nil || len(ends) != 2 {
		return invalid("ends is [end or null, end or null]")
	}
	auto := c.auto[id]
	for k, er := range ends {
		if string(er) == "null" {
			continue
		}
		var end model.End
		if err := json.Unmarshal(er, &end); err != nil {
			return err
		}
		target, err := c.id(end.El)
		if err != nil {
			return err
		}
		end.El = target
		v, err := model.DecodeBundle(typ, model.EndBundle(k), model.BundleJSON(model.BundleEnd0, end))
		if err != nil {
			return err
		}
		if target != "" {
			t, err := c.alive(target)
			if err != nil {
				return err
			}
			if t.El.Type == model.TypeConnector {
				return invalid("a connector cannot attach to a connector")
			}
		}
		vals[model.EndBundle(k)] = v
		auto[k] = omittedAt(er)[0]
	}
	c.auto[id] = auto
	return nil
}

// omittedAt reports which ends of a raw element, or the one raw end, are
// attached without "at" (4.4).
func omittedAt(raw json.RawMessage) [2]bool {
	var el struct {
		Ends []json.RawMessage `json:"ends"`
	}
	ends := []json.RawMessage{raw}
	if json.Unmarshal(raw, &el) == nil && el.Ends != nil {
		ends = el.Ends
	}
	var out [2]bool
	for k, er := range ends[:min(len(ends), 2)] {
		var f map[string]json.RawMessage
		if json.Unmarshal(er, &f) == nil {
			_, attached := f["el"]
			at, ok := f["at"]
			out[k] = attached && (!ok || string(bytes.TrimSpace(at)) == "null")
		}
	}
	return out
}

// placeEnds gives the ends this call attached without "at" their default
// positions, once layout has placed every element.
func (c *agentCall) placeEnds() {
	geom := func(id string) (model.Geom, bool) {
		r := c.a.s.rows[id]
		if !r.alive() {
			return model.Geom{}, false
		}
		return r.El.Geom, true
	}
	for _, id := range slices.Sorted(maps.Keys(c.auto)) {
		r := c.a.s.rows[id]
		if !r.alive() {
			continue
		}
		ends := placeAuto(r.El.Ends, c.auto[id], geom)
		for k, end := range ends {
			if end != r.El.Ends[k] {
				c.a.write(c.a.s.rows[id], model.EndBundle(k), end, "", 0)
			}
		}
	}
}

// placeAuto attaches each end marked in auto at the middle of its target's
// side that faces the other end (model.DefaultAt). The other end counts at
// its target's center when it is marked too, so the choice does not depend
// on the order of the ends.
func placeAuto(ends [2]model.End, auto [2]bool, geom func(id string) (model.Geom, bool)) [2]model.End {
	out := ends
	for k, end := range ends {
		g, ok := geom(end.El)
		if !auto[k] || end.El == "" || !ok {
			continue
		}
		other := ends[1-k]
		toward := other.Pt
		if og, ok := geom(other.El); other.El != "" && ok {
			toward = model.EndpointWorld(og, other.At)
			if auto[1-k] {
				toward = [2]float64{og.X + og.W/2, og.Y + og.H/2}
			}
		}
		out[k].At = model.DefaultAt(g, toward)
	}
	return out
}

// carry decides container following for an agent set (8.1): a frame whose
// x or y alone changes carries its members unless carry says otherwise.
func (c *agentCall) carry(r *Row, raw json.RawMessage, fields map[string]json.RawMessage) (string, error) {
	if len(raw) > 0 {
		var s string
		if json.Unmarshal(raw, &s) == nil && (s == CarryMembers || s == CarryGroup) {
			return s, nil
		}
		var b bool
		if json.Unmarshal(raw, &b) == nil && !b {
			return "", nil
		}
		return "", invalid(`carry is "members", "group" or false`)
	}
	_, x := fields["x"]
	_, y := fields["y"]
	_, w := fields["w"]
	_, h := fields["h"]
	if r.El.Type == model.TypeFrame && (x || y) && !w && !h {
		return CarryMembers, nil
	}
	return "", nil
}

// multi runs reparent, group, delete and zorder through the person
// operations with the current incarnations, refusing the call when any
// target is ignored.
func (c *agentCall) multi(op AgentOp) error {
	a := c.a
	ids := make([]string, 0, len(op.IDs))
	incOf := map[string]int64{}
	for _, ref := range op.IDs {
		r, err := c.unlocked(ref)
		if err != nil {
			return err
		}
		ids = append(ids, r.El.ID)
		incOf[r.El.ID] = r.Inc
	}
	if len(ids) == 0 {
		return invalid("%s needs ids", op.Op)
	}
	if err := c.ifVer(op.IfVer, ids...); err != nil {
		return err
	}
	pop := Op{Op: op.Op, IDs: ids, IncOf: incOf}
	var res *MultiResult
	switch op.Op {
	case OpReparent:
		frame, err := c.frameArg(op.Frame)
		if err != nil {
			return err
		}
		pop.Frame = model.BundleJSON(model.BundleFrame, frame)
		if frame != "" {
			pop.FrameInc = a.s.rows[frame].Inc
		}
		res = a.reparent(pop).(*MultiResult)
	case OpGroup:
		pop.Group = op.Group
		if pop.Group == "" {
			pop.Group = c.call.NewID("g_")
		}
		if !model.ValidGroupID(pop.Group) {
			return invalid("group %q is not a group id", pop.Group)
		}
		res = a.group(pop).(*MultiResult)
	case OpDelete:
		res = a.deleteOp(pop).(*MultiResult)
	case OpZorder:
		to, err := decodeTo(op.To)
		if err != nil {
			return invalid("%v", err)
		}
		if to.id != "" {
			anchor, err := c.alive(to.id)
			if err != nil {
				return err
			}
			to.id, to.inc = anchor.El.ID, anchor.Inc
		}
		pop.To = model.Canonical(toJSON(to))
		res = a.zorder(pop).(*MultiResult)
	}
	for _, id := range slices.Sorted(maps.Keys(res.Void)) {
		switch reason := res.Void[id]; reason {
		case VoidInvalidGroup:
			return invalid("cannot group %v: every element must be ungrouped, unlocked, not a frame and in the same frame", ids)
		case VoidRef:
			return invalid("element %s cannot go there (frames do not nest)", id)
		default:
			return conflict("element %s was not changed (%s); read the board again", id, reason)
		}
	}
	return nil
}

func toJSON(t zTarget) any {
	if t.id == "" {
		return t.where
	}
	return map[string]any{t.where: t.id, "inc": t.inc}
}

func (c *agentCall) frameArg(raw json.RawMessage) (string, error) {
	var f *string
	if err := json.Unmarshal(raw, &f); err != nil {
		return "", invalid("frame is an id, a $ref or null")
	}
	if f == nil {
		return "", nil
	}
	r, err := c.alive(*f)
	if err != nil {
		return "", err
	}
	if r.El.Type != model.TypeFrame {
		return "", invalid("element %s is not a frame", r.El.ID)
	}
	return r.El.ID, nil
}

// deleteFrameContents deletes a frame and its members; a locked member
// refuses the call, since an agent changes locked elements only after
// unlocking them.
func (c *agentCall) deleteFrameContents(op AgentOp) error {
	frame, err := c.frameArg(op.Frame)
	if err != nil {
		return err
	}
	if frame == "" {
		return invalid("delete_frame_contents needs a frame")
	}
	if err := c.ifVer(op.IfVer, frame); err != nil {
		return err
	}
	if _, err := c.unlocked(frame); err != nil {
		return err
	}
	for id := range c.a.s.members[frame] {
		if c.a.s.rows[id].El.Locked {
			return conflict("member %s of frame %s is locked; unlock it first", id, frame)
		}
	}
	c.a.deleteFrameContents(Op{Frame: model.BundleJSON(model.BundleFrame, frame), Inc: c.a.s.rows[frame].Inc})
	return nil
}

// layout applies layout help after every operation of the call (8.1). A
// frame it moves carries only the members no layout of the call places, so
// an explicitly placed member moves once, to its own place. The reported
// positions are read after every layout has applied.
func (c *agentCall) layout() error {
	c.laid = map[string]bool{}
	for _, l := range c.call.Layout {
		for _, ref := range l.IDs {
			if id, err := c.id(ref); err == nil {
				c.laid[id] = true
			}
		}
	}
	for i, l := range c.call.Layout {
		if err := c.layoutOne(l); err != nil {
			return fmt.Errorf("layout %d: %w", i, err)
		}
	}
	for id := range c.out.Layout {
		g := c.a.s.rows[id].El.Geom
		c.out.Layout[id] = [2]float64{g.X, g.Y}
	}
	return nil
}

func (c *agentCall) layoutOne(l Layout) error {
	a := c.a
	if l.Kind != "grid" && l.Kind != "row" && l.Kind != "column" {
		return invalid("layout kind is grid, row or column")
	}
	if len(l.IDs) == 0 {
		return invalid("layout needs ids")
	}
	var frame *Row
	if l.In != "" {
		id, err := c.frameArg(model.Canonical(l.In))
		if err != nil {
			return err
		}
		frame = a.s.rows[id]
	}
	gap := float64(layoutGap)
	if l.Gap != nil {
		if *l.Gap < 0 || *l.Gap > model.MaxSize {
			return invalid("gap must be 0 to %d", model.MaxSize)
		}
		gap = *l.Gap
	}
	var rows []*Row
	seen := map[string]bool{}
	for _, ref := range l.IDs {
		r, err := c.unlocked(ref)
		if err != nil {
			return err
		}
		if r.El.Type == model.TypeConnector {
			return invalid("layout cannot place connector %s", r.El.ID)
		}
		if frame != nil && r.El.Type == model.TypeFrame {
			return invalid("frame %s cannot go inside a frame", r.El.ID)
		}
		if seen[r.El.ID] {
			return invalid("layout names %s twice", r.El.ID)
		}
		seen[r.El.ID] = true
		rows = append(rows, r)
	}
	cols := l.Columns
	switch l.Kind {
	case "row":
		cols = len(rows)
	case "column":
		cols = 1
	default:
		if cols <= 0 {
			cols = int(math.Ceil(math.Sqrt(float64(len(rows)))))
		}
	}
	var ox, oy float64
	if frame != nil {
		ox, oy = frame.El.Geom.X+layoutInset, frame.El.Geom.Y+layoutInset
	} else {
		ox, oy = math.Inf(1), math.Inf(1)
		for _, r := range rows {
			ox, oy = math.Min(ox, r.El.Geom.X), math.Min(oy, r.El.Geom.Y)
		}
	}
	var cw, ch float64
	for _, r := range rows {
		cw, ch = math.Max(cw, r.El.Geom.W), math.Max(ch, r.El.Geom.H)
	}
	x, y := ox, oy
	right, bottom := ox, oy
	for i, r := range rows {
		switch l.Kind {
		case "grid":
			x, y = ox+float64(i%cols)*(cw+gap), oy+float64(i/cols)*(ch+gap)
		case "row", "column":
			if i > 0 {
				prev := rows[i-1].El.Geom
				if l.Kind == "row" {
					x += prev.W + gap
				} else {
					y += prev.H + gap
				}
			}
		}
		g := r.El.Geom
		g.X, g.Y = x, y
		g = g.Round()
		if math.Abs(g.X) > model.MaxCoord || math.Abs(g.Y) > model.MaxCoord {
			return invalid("layout places %s outside the canvas", r.El.ID)
		}
		if cur := a.s.rows[r.El.ID]; cur.El.Geom != g {
			a.write(cur, model.BundleGeom, g, "", 0)
			if r.El.Type == model.TypeFrame {
				dx, dy := g.X-cur.El.Geom.X, g.Y-cur.El.Geom.Y
				for _, m := range slices.Sorted(maps.Keys(a.s.members[r.El.ID])) {
					if !c.laid[m] && (dx != 0 || dy != 0) {
						a.moveBy(m, dx, dy)
					}
				}
			}
		}
		c.out.Layout[r.El.ID] = [2]float64{g.X, g.Y}
		right, bottom = math.Max(right, g.X+g.W), math.Max(bottom, g.Y+g.H)
	}
	if frame == nil {
		return nil
	}
	ids := make([]string, len(rows))
	incOf := map[string]int64{}
	for i, r := range rows {
		ids[i] = r.El.ID
		incOf[r.El.ID] = r.Inc
	}
	res := a.reparent(Op{IDs: ids, IncOf: incOf, Frame: model.BundleJSON(model.BundleFrame, frame.El.ID), FrameInc: frame.Inc}).(*MultiResult)
	if len(res.Void) > 0 {
		return conflict("layout could not move %v into frame %s", slices.Sorted(maps.Keys(res.Void)), frame.El.ID)
	}
	f := a.s.rows[frame.El.ID]
	g := f.El.Geom
	g.W = math.Max(g.W, right-g.X+layoutInset)
	g.H = math.Max(g.H, bottom-g.Y+layoutInset)
	g = g.Round()
	if g != f.El.Geom {
		if f.El.Locked {
			return conflict("frame %s is locked and too small for the layout", f.El.ID)
		}
		if g.W > model.MaxSize || g.H > model.MaxSize {
			return invalid("frame %s would exceed the maximum size", f.El.ID)
		}
		a.write(f, model.BundleGeom, g, "", 0)
	}
	return nil
}

// replace applies update_board {replace: true}: the call's add operations
// become the whole board (4.12). The call's layout then places those
// elements as in an ordinary call, and the ends attached without "at" take
// their sides from the committed places; a failure rolls back the
// replacement with the call.
func (c *agentCall) replace() error {
	var els []model.Element
	var presents []model.Present
	for i, op := range c.call.Ops {
		if op.Op != OpAdd {
			return invalid("%v (operation %d is %s)", errEmptyReplace, i, op.Op)
		}
		el, present, err := model.DecodeNewElement(op.El)
		if err != nil {
			return err
		}
		if err := c.checkRef(op.Ref); err != nil {
			return err
		}
		if el.ID == "" {
			el.ID = c.call.NewID("e_")
		}
		if el.Type == model.TypeConnector {
			c.auto[el.ID] = omittedAt(op.El)
		}
		if op.Ref != "" {
			c.refs[op.Ref] = el.ID
			c.out.IDs[op.Ref] = el.ID
		}
		els = append(els, el)
		presents = append(presents, present)
	}
	// Elements without coordinates go to the right of the board as it is
	// after the reset: the call's own elements that have coordinates, not
	// the board being replaced (8.1).
	c.placed = true
	if box, ok := placedBounds(els, presents); ok {
		c.nextX, c.nextY = box.X+box.W+placeGap, box.Y
	}
	for i, op := range c.call.Ops {
		if err := c.fillGeom(&els[i], presents[i], c.layoutPlaces(els[i].ID, op.Ref)); err != nil {
			return err
		}
		els[i].By, els[i].At = c.a.by, c.a.now
		els[i].SetAuthor(c.a.kind, "")
	}
	if len(els) == 0 {
		return invalid("%v", errEmptyReplace)
	}
	resolve := func(id string) (string, error) {
		if !strings.HasPrefix(id, "$") {
			return id, nil
		}
		if v, ok := c.refs[id]; ok {
			return v, nil
		}
		return "", invalid("%s names no element of this call", id)
	}
	top, bottom := "", ""
	var unkeyed, unkeyedFrames []int
	for i := range els {
		el := &els[i]
		var err error
		if el.Frame, err = resolve(el.Frame); err != nil {
			return err
		}
		for k := range el.Ends {
			if el.Ends[k].El, err = resolve(el.Ends[k].El); err != nil {
				return err
			}
		}
		switch {
		case el.Z == "" && el.Type == model.TypeFrame:
			unkeyedFrames = append(unkeyedFrames, i)
		case el.Z == "":
			unkeyed = append(unkeyed, i)
		case bottom == "":
			top, bottom = el.Z, el.Z
		default:
			top, bottom = max(top, el.Z), min(bottom, el.Z)
		}
		c.out.Results = append(c.out.Results, el.ID)
	}
	// Frames without z go behind the call's other elements in call order,
	// the rest in front of them, as in an ordinary call (8.1).
	if bottom == "" {
		keys := spread("", "", len(unkeyedFrames)+len(unkeyed))
		unkeyed = append(unkeyedFrames, unkeyed...)
		for i, k := range keys {
			els[unkeyed[i]].Z = k
		}
	} else {
		for i, k := range spread("", bottom, len(unkeyedFrames)) {
			els[unkeyedFrames[i]].Z = k
		}
		for i, k := range spread(top, "", len(unkeyed)) {
			els[unkeyed[i]].Z = k
		}
	}
	geoms := map[string]model.Geom{}
	for _, el := range els {
		if el.Type != model.TypeConnector {
			geoms[el.ID] = el.Geom
		}
	}
	geom := func(id string) (model.Geom, bool) { g, ok := geoms[id]; return g, ok }
	for i := range els {
		if el := &els[i]; el.Type == model.TypeConnector {
			el.Ends = placeAuto(el.Ends, c.auto[el.ID], geom)
		}
	}
	for _, el := range els {
		if el.Type == model.TypeImage && !c.a.assetKnown(el.Asset) {
			return invalid("image asset %s was not uploaded", el.Asset)
		}
	}
	if err := checkContent(els); err != nil {
		return err
	}
	board := c.a.s.rows[model.BoardID].El
	if err := c.a.resetContent(Content{Title: board.Text, Grid: board.Grid, Elements: els}); err != nil {
		return err
	}
	if err := c.layout(); err != nil {
		return err
	}
	c.placeEnds()
	return nil
}
