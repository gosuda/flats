package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/gosuda/flats/internal/board/model"
)

// Op is one client operation of a person batch (4.1, 4.3, Appendix A).
// An expected incarnation of 0 means the one this session's own add or
// revive produced.
type Op struct {
	Op       string           `json:"op"`
	El       json.RawMessage  `json:"el,omitempty"`
	Refs     map[string]int64 `json:"refs,omitempty"`
	ID       string           `json:"id,omitempty"`
	Inc      int64            `json:"inc,omitempty"`
	G        string           `json:"g,omitempty"`
	Val      json.RawMessage  `json:"val,omitempty"`
	IfV      json.RawMessage  `json:"if_v,omitempty"`
	BaseV    int64            `json:"base_v,omitempty"`
	Carry    string           `json:"carry,omitempty"`
	IDs      []string         `json:"ids,omitempty"`
	IfDel    int64            `json:"if_del,omitempty"`
	IncOf    map[string]int64 `json:"inc_of,omitempty"`
	Frame    json.RawMessage  `json:"frame,omitempty"`
	FrameInc int64            `json:"frame_inc,omitempty"`
	Group    string           `json:"group,omitempty"`
	To       json.RawMessage  `json:"to,omitempty"`
}

// Operation kinds.
const (
	OpAdd                 = "add"
	OpSet                 = "set"
	OpRevive              = "revive"
	OpReparent            = "reparent"
	OpGroup               = "group"
	OpUngroup             = "ungroup"
	OpDelete              = "delete"
	OpDeleteFrameContents = "delete_frame_contents"
	OpZorder              = "zorder"
	OpMeta                = "meta"
	OpUndo                = "undo"
	OpRedo                = "redo"
)

// Carry values of a geom set (4.2).
const (
	CarryMembers = "members"
	CarryGroup   = "group"
)

// DecodeOps reads the ops array of a person batch, refusing unknown fields.
func DecodeOps(raw []byte) ([]Op, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var ops []Op
	if err := dec.Decode(&ops); err != nil {
		return nil, fmt.Errorf("%w: ops: %v", model.ErrFormat, err)
	}
	return ops, nil
}

func formatErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", model.ErrFormat, fmt.Sprintf(format, args...))
}

// zTarget is a decoded zorder destination.
type zTarget struct {
	where string // front, back, after, before
	id    string
	inc   int64
}

func decodeTo(raw json.RawMessage) (zTarget, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "front" || s == "back" {
			return zTarget{where: s}, nil
		}
		return zTarget{}, formatErr("zorder to is front, back, {after} or {before}")
	}
	var f struct {
		After  string `json:"after"`
		Before string `json:"before"`
		Inc    int64  `json:"inc"`
	}
	if err := strictDecode(raw, &f); err != nil || (f.After == "") == (f.Before == "") {
		return zTarget{}, formatErr("zorder to is front, back, {after} or {before}")
	}
	if f.After != "" {
		return zTarget{"after", f.After, f.Inc}, nil
	}
	return zTarget{"before", f.Before, f.Inc}, nil
}

func strictDecode(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// ifV is a per-element bundle guard: {id: {g: v}}.
type ifV map[string]map[string]int64

func decodeIfV(raw json.RawMessage) (ifV, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m ifV
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, formatErr("if_v is {id: {bundle: v}}")
	}
	return m, nil
}

func decodeV(raw json.RawMessage) (int64, bool, error) {
	if len(raw) == 0 {
		return 0, false, nil
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil || v < 1 {
		return 0, false, formatErr("if_v is a positive version")
	}
	return v, true, nil
}

// checkOps validates the form of a person batch before anything applies.
func checkOps(ops []Op) error {
	if len(ops) == 0 {
		return formatErr("a batch holds at least one operation")
	}
	if len(ops) > model.MaxPersonOps {
		return formatErr("a batch holds at most %d operations", model.MaxPersonOps)
	}
	for i, op := range ops {
		if err := checkOp(op); err != nil {
			return fmt.Errorf("operation %d: %w", i, err)
		}
		if (op.Op == OpUndo || op.Op == OpRedo) && len(ops) != 1 {
			return formatErr("undo and redo are batches of their own")
		}
	}
	return nil
}

func checkOp(op Op) error {
	need := func(ok bool, what string) error {
		if !ok {
			return formatErr("%s needs %s", op.Op, what)
		}
		return nil
	}
	switch op.Op {
	case OpAdd:
		return need(len(op.El) > 0, "el")
	case OpSet:
		if err := need(op.ID != "" && op.G != "" && len(op.Val) > 0, "id, g and val"); err != nil {
			return err
		}
		if op.Carry != "" && op.Carry != CarryMembers && op.Carry != CarryGroup {
			return formatErr("carry is members or group")
		}
		if op.Carry != "" && op.G != model.BundleGeom {
			return formatErr("carry applies to geom")
		}
		if err := checkSetValue(op.G, op.Val); err != nil {
			return err
		}
		_, _, err := decodeV(op.IfV)
		return err
	case OpRevive:
		return need(len(op.IDs) > 0 && op.IfDel > 0, "ids and if_del")
	case OpReparent:
		if err := need(len(op.IDs) > 0 && len(op.Frame) > 0, "ids and frame"); err != nil {
			return err
		}
		_, err := decodeFrame(op.Frame)
		return err
	case OpGroup:
		if err := need(len(op.IDs) > 0, "ids"); err != nil {
			return err
		}
		if !model.ValidGroupID(op.Group) {
			return formatErr("group %q is not a group id", op.Group)
		}
		return nil
	case OpUngroup:
		if !model.ValidGroupID(op.Group) {
			return formatErr("group %q is not a group id", op.Group)
		}
		return nil
	case OpDelete:
		if err := need(len(op.IDs) > 0, "ids"); err != nil {
			return err
		}
		_, err := decodeIfV(op.IfV)
		return err
	case OpDeleteFrameContents:
		f, err := decodeFrame(op.Frame)
		if err != nil {
			return err
		}
		return need(f != "", "frame")
	case OpZorder:
		if err := need(len(op.IDs) > 0 && len(op.To) > 0, "ids and to"); err != nil {
			return err
		}
		if _, err := decodeTo(op.To); err != nil {
			return err
		}
		_, err := decodeIfV(op.IfV)
		return err
	case OpMeta:
		if err := need(op.G == model.BundleTitle || op.G == model.BundleGrid, "g title or grid"); err != nil {
			return err
		}
		if _, err := model.DecodeBundle(model.TypeBoard, op.G, op.Val); err != nil {
			return err
		}
		_, _, err := decodeV(op.IfV)
		return err
	case OpUndo, OpRedo:
		return nil
	}
	return formatErr("unknown operation %q", op.Op)
}

// checkSetValue checks the form of a set's bundle and value without the
// target: g must be settable on some element type and val a valid value of
// g for at least one of them. The target's own type is checked only after
// its incarnation resolves (set), so a malformed operation is refused
// whatever the board holds, while a well-formed one against an incarnation a
// reset replaced with another type is void:gone (4.1).
func checkSetValue(g string, val json.RawMessage) error {
	var first error
	for _, t := range model.Types {
		if !model.Settable(t, g) {
			continue
		}
		_, err := model.DecodeBundle(t, g, val)
		if err == nil {
			return nil
		}
		if first == nil {
			first = err
		}
	}
	if first == nil {
		return formatErr("no element has a settable %q", g)
	}
	return first
}

func decodeFrame(raw json.RawMessage) (string, error) {
	var f *string
	if err := json.Unmarshal(raw, &f); err != nil {
		return "", formatErr("frame is an id or null")
	}
	if f == nil {
		return "", nil
	}
	if !model.ValidID(*f) {
		return "", formatErr("frame %q is not an element id", *f)
	}
	return *f, nil
}

// Result reasons of an ignored operation (Appendix A).
const (
	VoidExists       = "exists"
	VoidGone         = "gone"
	VoidStale        = "stale"
	VoidLocked       = "locked"
	VoidRef          = "ref"
	VoidInvalidGroup = "invalid_group"
)

// ResultOK is the result of an applied single-target operation.
const ResultOK = "ok"

func void(reason string) string { return "void:" + reason }

// MultiResult is the result of a semantic operation over several elements.
type MultiResult struct {
	OK     []string          `json:"ok,omitempty"`
	Void   map[string]string `json:"void,omitempty"`
	Locked int               `json:"locked,omitempty"` // delete_frame_contents: locked members kept
}

func (m *MultiResult) fail(id, reason string) {
	if m.Void == nil {
		m.Void = map[string]string{}
	}
	m.Void[id] = reason
}

// UndoResult is the result of an undo or redo.
type UndoResult struct {
	Undone  int            `json:"undone"`
	Skipped map[string]int `json:"skipped"`
}

func newUndoResult() *UndoResult {
	return &UndoResult{Skipped: map[string]int{VoidStale: 0, VoidLocked: 0, VoidGone: 0, VoidInvalidGroup: 0}}
}

func (u *UndoResult) skip(reason string) { u.Skipped[reason]++ }

// anyApplied reports whether a result applied anything.
func anyApplied(r any) bool {
	switch r := r.(type) {
	case string:
		return r == ResultOK
	case *MultiResult:
		return len(r.OK) > 0
	case *UndoResult:
		return r.Undone > 0
	}
	return false
}

// EncodeResults encodes the results of a batch, summarizing them when they
// exceed model.MaxResultsBytes (Appendix A "Results"): every result becomes
// {ok: count, void: {reason: count}}, then ignored ids are added as samples
// from the first operation on while they fit.
func EncodeResults(results []any) json.RawMessage {
	raw, _ := json.Marshal(results)
	if len(raw) <= model.MaxResultsBytes {
		return raw
	}
	type summary struct {
		OK      int               `json:"ok"`
		Void    map[string]int    `json:"void,omitempty"`
		Samples map[string]string `json:"samples,omitempty"`
	}
	sums := make([]*summary, len(results))
	voids := make([]map[string]string, len(results))
	for i, r := range results {
		s := &summary{}
		switch r := r.(type) {
		case string:
			if r == ResultOK {
				s.OK = 1
			} else {
				s.Void = map[string]int{r[len("void:"):]: 1}
			}
		case *MultiResult:
			s.OK = len(r.OK)
			if len(r.Void) > 0 {
				s.Void = map[string]int{}
				for _, reason := range r.Void {
					s.Void[reason]++
				}
			}
			voids[i] = r.Void
		case *UndoResult:
			s.OK = r.Undone
			s.Void = map[string]int{}
			for k, n := range r.Skipped {
				if n > 0 {
					s.Void[k] = n
				}
			}
		}
		sums[i] = s
	}
	raw, _ = json.Marshal(sums)
	budget := model.MaxResultsBytes - len(raw)
	for i, v := range voids {
		for _, id := range slices.Sorted(maps.Keys(v)) {
			// Measured encoded: json escapes <, > and & as six bytes and
			// control characters too, so raw lengths undercount.
			k, _ := json.Marshal(id)
			r, _ := json.Marshal(v[id])
			cost := len(k) + len(r) + 2 // colon and comma
			if sums[i].Samples == nil {
				cost += len(`,"samples":{}`)
			}
			if cost > budget {
				out, _ := json.Marshal(sums)
				return out
			}
			if sums[i].Samples == nil {
				sums[i].Samples = map[string]string{}
			}
			sums[i].Samples[id] = v[id]
			budget -= cost
		}
	}
	out, _ := json.Marshal(sums)
	return out
}
