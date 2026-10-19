package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/board/model"
)

func TestUndoRedo(t *testing.T) {
	t.Run("edit, undo, redo", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("x", 0, 0))
		h.ok(a, move(h, "x", 10, 0, ""))
		h.ok(a, undoOp)
		if h.geom(eid("x")).X != 0 {
			t.Fatal("undo did not revert")
		}
		h.ok(a, redoOp)
		if h.geom(eid("x")).X != 10 {
			t.Fatal("redo did not reapply")
		}
		h.ok(a, undoOp)
		h.ok(a, undoOp)
		if h.alive(eid("x")) {
			t.Fatal("undoing the creation kept x")
		}
		h.ok(a, redoOp)
		h.ok(a, redoOp)
		if !h.alive(eid("x")) || h.geom(eid("x")).X != 10 {
			t.Fatal("redoing creation and move failed")
		}
	})

	t.Run("consecutive undos", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("x", 0, 0))
		for x := 1.0; x <= 3; x++ {
			h.ok(a, move(h, "x", x, 0, ""))
		}
		for want := 2.0; want >= 0; want-- {
			if u := undoResult(t, h.ok(a, undoOp)); u.Undone != 1 || h.geom(eid("x")).X != want {
				t.Fatalf("undo to %v: %+v, x = %v", want, u, h.geom(eid("x")).X)
			}
		}
	})

	t.Run("a new edit after undo clears redo", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("x", 0, 0))
		h.ok(a, move(h, "x", 10, 0, ""))
		h.ok(a, undoOp)
		h.ok(a, move(h, "x", 20, 0, ""))
		if u := undoResult(t, h.do(a, redoOp)); u.Undone != 0 || h.geom(eid("x")).X != 20 {
			t.Fatalf("redo after a new edit: %+v", u)
		}
	})

	t.Run("undo never changes a locked element except its own lock", func(t *testing.T) {
		h := newHarness(t)
		a, b := h.session("a"), h.session("b")
		h.ok(a, addSticky("x", 0, 0))
		h.ok(a, move(h, "x", 10, 0, ""))
		lock := Op{Op: OpSet, ID: eid("x"), Inc: h.inc(eid("x")), G: model.BundleLocked, Val: raw(true)}
		h.ok(b, lock)
		if u := undoResult(t, h.do(a, undoOp)); u.Skipped[VoidLocked] != 1 || h.geom(eid("x")).X != 10 {
			t.Fatalf("undo on a locked element: %+v", u)
		}
		h.ok(a, Op{Op: OpSet, ID: eid("x"), Inc: h.inc(eid("x")), G: model.BundleLocked, Val: raw(false)})
		h.ok(a, lock)
		if u := undoResult(t, h.ok(a, undoOp)); u.Undone != 1 || h.row(eid("x")).El.Locked {
			t.Fatalf("undoing my own lock: %+v", u)
		}
	})

	t.Run("edit text, delete, undo delete, undo text edit", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("x", 0, 0))
		h.ok(a, setText("x", 0, "edited", 0))
		h.ok(a, deleteOp(h, "x"))
		h.ok(a, undoOp)
		if u := undoResult(t, h.ok(a, undoOp)); u.Undone != 1 || h.row(eid("x")).El.Text != "" {
			t.Fatalf("text undo after revive: %+v", u)
		}
		if inc, _ := h.s.CreatedInc(CreatedKey{a, eid("x")}); inc != h.inc(eid("x")) {
			t.Fatal("the created row did not follow the revive")
		}
	})

	t.Run("undo of a frame deletion restores its members", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addFrame("f", 0, 0, 500, 500))
		h.ok(a, addIn("m", "f", h.inc(eid("f")), 10, 10), addIn("n", "f", h.inc(eid("f")), 200, 10))
		h.ok(a, groupOp(h, "g", "m", "n"))
		h.ok(a, deleteOp(h, "f"))
		if h.row(eid("m")).El.Frame != "" {
			t.Fatal("deleting the frame kept its membership")
		}
		u := undoResult(t, h.ok(a, undoOp))
		if h.row(eid("m")).El.Frame != eid("f") || h.row(eid("n")).El.Frame != eid("f") || u.Skipped[VoidStale] != 0 {
			t.Fatalf("undo %+v; m in %q", u, h.row(eid("m")).El.Frame)
		}
	})

	t.Run("undo of a stacking change", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("x", 0, 0), addSticky("y", 0, 0), addSticky("z", 0, 0))
		h.ok(a, Op{Op: OpZorder, IDs: []string{eid("x"), eid("y")}, IncOf: map[string]int64{eid("x"): 0, eid("y"): 0}, To: raw("back")})
		before := strings.Join(h.s.order(), ",")
		h.ok(a, Op{Op: OpZorder, IDs: []string{eid("x"), eid("y")}, IncOf: map[string]int64{eid("x"): 0, eid("y"): 0}, To: raw("front")})
		if got := strings.Join(h.s.order(), ","); got != eid("z")+","+eid("x")+","+eid("y") {
			t.Fatalf("front order = %s", got)
		}
		h.ok(a, undoOp)
		if got := strings.Join(h.s.order(), ","); got != before {
			t.Fatalf("after undo %s, want %s", got, before)
		}
	})

	t.Run("undo history is per session and capped", func(t *testing.T) {
		h := newHarness(t)
		a, b := h.session("a"), h.session("b")
		h.ok(a, addSticky("x", 0, 0))
		for i := range MaxUndoEntries + 5 {
			h.ok(a, move(h, "x", float64(i+1), 0, ""))
		}
		if n := len(h.s.undo[a].undo); n != MaxUndoEntries {
			t.Fatalf("%d undo entries", n)
		}
		if u := undoResult(t, h.do(b, undoOp)); u.Undone != 0 || h.geom(eid("x")).X != MaxUndoEntries+5 {
			t.Fatal("another session undid my edit")
		}
		h.s.TrimUndo(0)
		if len(h.s.undo[a].undo) != 0 {
			t.Fatal("TrimUndo kept entries")
		}
	})
}

// TestUndoRevivedMemberKeepsGroup: one batch moves group {p, q} from f1
// to f2 and deletes p. Undoing it revives p into f1 (its batch-start
// frame) while q's frame goes back to f1 only in the relation step, so the
// group invariant is checked after that step and p keeps its group (4.10).
func TestUndoRevivedMemberKeepsGroup(t *testing.T) {
	for _, order := range []string{"move then delete", "delete then move"} {
		t.Run(order, func(t *testing.T) {
			h := newHarness(t)
			a := h.session("a")
			h.ok(a, addFrame("f1", 0, 0, 1000, 1000), addFrame("f2", 2000, 0, 1000, 1000))
			h.ok(a, addIn("p", "f1", h.inc(eid("f1")), 10, 10), addIn("q", "f1", h.inc(eid("f1")), 200, 10))
			h.ok(a, groupOp(h, "g", "p", "q"))
			group := h.row(eid("p")).El.Group
			ops := []Op{reparentOp(h, "f2", "p", "q"), deleteOp(h, "p")}
			if order == "delete then move" {
				ops = []Op{deleteOp(h, "p"), reparentOp(h, "f2", "q")}
			}
			h.ok(a, ops...)
			if h.alive(eid("p")) || h.row(eid("q")).El.Frame != eid("f2") {
				t.Fatal("the batch did not move q and delete p")
			}
			check := func(step string) {
				t.Helper()
				p, q := h.row(eid("p")), h.row(eid("q"))
				if !p.alive() || p.El.Frame != eid("f1") || q.El.Frame != eid("f1") || p.El.Group != group || q.El.Group != group {
					t.Fatalf("after %s: p in %q group %q, q in %q group %q", step, p.El.Frame, p.El.Group, q.El.Frame, q.El.Group)
				}
				if err := checkInvariants(h.s); err != nil {
					t.Fatalf("after %s: %v", step, err)
				}
			}
			u := undoResult(t, h.ok(a, undoOp))
			for reason, n := range u.Skipped {
				if n != 0 {
					t.Fatalf("undo skipped %d (%s): %+v", n, reason, u)
				}
			}
			check("undo")
			h.ok(a, redoOp)
			if h.alive(eid("p")) || h.row(eid("q")).El.Frame != eid("f2") {
				t.Fatal("redo did not move q and delete p again")
			}
			h.ok(a, undoOp)
			check("undo after redo")
		})
	}
}

func TestSemanticOps(t *testing.T) {
	h := newHarness(t)
	a, b := h.session("a"), h.session("b")
	h.ok(a, addFrame("f", 0, 0, 1000, 1000), addSticky("x", 10, 10), addSticky("y", 200, 10), addSticky("z", 400, 10))

	// group refuses targets in different frames, and is all or nothing
	h.ok(a, reparentOp(h, "f", "x"))
	g := h.ok(a, groupOp(h, "g1", "x", "y"))
	var mr []MultiResult
	json.Unmarshal(g.Results, &mr)
	if len(mr[0].OK) != 0 || mr[0].Void[eid("x")] != VoidInvalidGroup || h.row(eid("y")).El.Group != "" {
		t.Fatalf("mixed-frame group = %s", g.Results)
	}
	h.ok(a, reparentOp(h, "", "x"))
	h.ok(a, groupOp(h, "g1", "x", "y"))
	if h.row(eid("x")).El.Group == "" {
		t.Fatal("group did not apply")
	}
	if out := h.ok(a, groupOp(h, "g2", "x", "z")); strings.Contains(string(out.Results), `"ok"`) {
		t.Fatalf("grouping a grouped element = %s", out.Results)
	}

	// a locked element ignores direct changes but follows its group
	h.ok(b, Op{Op: OpSet, ID: eid("y"), Inc: h.inc(eid("y")), G: model.BundleLocked, Val: raw(true)})
	if rs := results(t, h.do(a, move(h, "y", 0, 0, ""))); rs[0] != void(VoidLocked) {
		t.Fatalf("move of a locked element = %v", rs)
	}
	h.ok(a, move(h, "x", 110, 110, CarryGroup))
	if y := h.geom(eid("y")); y.X != 300 || y.Y != 110 {
		t.Fatalf("locked group member = %v", y)
	}
	h.ok(a, reparentOp(h, "f", "x"))
	if h.row(eid("y")).El.Frame != eid("f") {
		t.Fatal("the locked group member did not change frame with its group")
	}
	h.ok(a, Op{Op: OpUngroup, Group: "g_g100000000"})
	if h.row(eid("y")).El.Group != "" {
		t.Fatal("ungroup kept a locked member")
	}

	// delete_frame_contents keeps locked members
	out := h.ok(a, Op{Op: OpDeleteFrameContents, Frame: raw(eid("f")), Inc: h.inc(eid("f"))})
	if h.alive(eid("f")) || h.alive(eid("x")) || !h.alive(eid("y")) || h.row(eid("y")).El.Frame != "" || !strings.Contains(string(out.Results), `"locked":1`) {
		t.Fatalf("delete_frame_contents = %s", out.Results)
	}

	// meta with a stale guard
	if rs := results(t, h.do(a, Op{Op: OpMeta, G: model.BundleTitle, Val: raw("Plan"), IfV: raw(99999)})); rs[0] != void(VoidStale) {
		t.Fatalf("stale meta = %v", rs)
	}
	m := h.ok(a, Op{Op: OpMeta, G: model.BundleTitle, Val: raw("Plan")})
	if h.s.Board().Text != "Plan" || m.Delta.Changes[0].ID != model.BoardID {
		t.Fatal("meta did not apply")
	}

	// a batch whose operations are all ignored gets no seq
	seq := h.s.Seq()
	out = h.do(a, move(h, "y", 1, 1, ""))
	if out.Seq != 0 || out.Delta != nil || out.AckSeq != seq || h.s.Seq() != seq {
		t.Fatalf("ignored batch = %+v", out)
	}

	// an add with an existing, deleted or malformed id is ignored
	rs := results(t, h.do(a, addSticky("x", 0, 0), Op{Op: OpAdd, El: elOf(map[string]any{"id": "bad", "type": "sticky", "x": 0, "y": 0, "w": 1, "h": 1})}))
	if rs[0] != void(VoidExists) || rs[1] != void(VoidExists) {
		t.Fatalf("add results %v", rs)
	}

	// a format error refuses the whole batch and consumes the number
	n := h.s.sessions[a].MaxN
	out = h.do(a, addSticky("q", 0, 0), Op{Op: OpSet, ID: eid("y"), Inc: h.inc(eid("y")), G: model.BundleFrame, Val: raw(nil)})
	if out.Reject == nil || out.Reject.Code != CodeInvalid || h.s.rows[eid("q")] != nil || h.s.sessions[a].MaxN != n+1 {
		t.Fatalf("format error = %+v", out)
	}
	if replay := h.s.ApplyBatch(Batch{Session: a, N: n + 1, Ops: []Op{undoOp}, Now: h.now}); replay.Reject == nil || !replay.Replayed {
		t.Fatal("a refused batch number was not kept")
	}
	if out := h.s.ApplyBatch(Batch{Session: a, N: n + 5, Ops: []Op{undoOp}, Now: h.now}); out.Fatal != FatalProtocol {
		t.Fatalf("skipped number = %+v", out)
	}
}

func TestLimits(t *testing.T) {
	h := newHarness(t)
	a := h.session("a")
	h.ok(a, addSticky("x", 0, 0), addSticky("y", 0, 0))
	h.ok(a, setText("x", 0, "0123456789", 0))
	h.s.SetLimits(model.Limits{Elements: 1, Text: 5, Label: 5, PenPoints: 2, Assets: 1})

	if out := h.do(a, setText("x", 0, "0123456789ab", 0)); out.Reject == nil || out.Reject.Code != CodeLimit {
		t.Fatalf("growing text over the limit = %+v", out)
	}
	h.ok(a, move(h, "x", 5, 5, ""))
	h.ok(a, setText("x", 0, "01234567", 0))
	if out := h.do(a, addSticky("z", 0, 0)); out.Reject == nil || out.Reject.Code != CodeLimit {
		t.Fatal("an add over the element limit applied")
	}
	h.ok(a, deleteOp(h, "y"))
	if out := h.do(a, Op{Op: OpAdd, El: elOf(map[string]any{"id": eid("p"), "type": "pen", "x": 0, "y": 0, "w": 1, "h": 1, "pts": []int{0, 0, 1, 1, 2, 2}})}); out.Reject == nil {
		t.Fatal("a pen over the point limit applied")
	}
	// undo of the deletion grows the element count and is refused as a whole
	if out := h.do(a, undoOp); out.Reject == nil || !h.alive(eid("x")) || h.alive(eid("y")) {
		t.Fatalf("undo over the element limit = %+v", out)
	}
}

func TestResultsSummary(t *testing.T) {
	h := newHarness(t)
	a := h.session("a")
	var ids []string
	for i := range 6000 {
		ids = append(ids, fmt.Sprintf("e_g%09d", i))
	}
	out := h.do(a, Op{Op: OpDelete, IDs: ids, IncOf: map[string]int64{}})
	if len(out.Results) > model.MaxResultsBytes {
		t.Fatalf("results are %d bytes", len(out.Results))
	}
	var sums []struct {
		OK      int               `json:"ok"`
		Void    map[string]int    `json:"void"`
		Samples map[string]string `json:"samples"`
	}
	if err := json.Unmarshal(out.Results, &sums); err != nil || sums[0].Void[VoidGone] != 6000 || len(sums[0].Samples) == 0 {
		t.Fatalf("summary = %.200s (%v)", out.Results, err)
	}
	if len(out.Results) < model.MaxResultsBytes-100 {
		t.Fatalf("samples left %d bytes unused", model.MaxResultsBytes-len(out.Results))
	}
}

func TestRollbackDetails(t *testing.T) {
	h := newHarness(t)
	a := h.session("a")
	h.ok(a, addSticky("x", 0, 0), addSticky("y", 100, 0))
	h.ok(a, Op{Op: OpMeta, G: model.BundleTitle, Val: raw("R")})
	rev := content(h.s)
	xv := h.row(eid("x")).V
	h.ok(a, setText("x", 0, "changed", 0), Op{Op: OpMeta, G: model.BundleTitle, Val: raw("later")})
	out, err := h.s.Rollback(7, rev, h.now)
	if err != nil {
		t.Fatal(err)
	}
	x := h.row(eid("x"))
	if x.V[model.BundleText] != out.Seq || x.V[model.BundleGeom] != xv[model.BundleGeom] || h.s.Board().Text != "R" {
		t.Fatalf("x versions %v", x.V)
	}
	if out.Delta.Reset.Of != 7 || len(h.s.undo) != 0 || h.s.Resets()[0] != out.Seq || !out.Changed.Meta {
		t.Fatalf("rollback outcome %+v", out)
	}
	if len(out.Delta.Changes) != 2 {
		t.Fatalf("delta changes %+v", out.Delta.Changes)
	}
	// rolling back to identical content still takes a seq and resets
	again, _ := h.s.Rollback(7, rev, h.now)
	if again.Seq != out.Seq+1 || len(again.Delta.Changes) != 0 {
		t.Fatalf("identical rollback %+v", again)
	}
}

func TestAgentCalls(t *testing.T) {
	t.Run("refs, defaults, layout and membership", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("old", 0, 0))
		seq := h.s.Seq()
		out, err := h.agent(AgentCall{
			IfSeq: &seq,
			Ops: []AgentOp{
				{Op: "add", Ref: "$f", El: elOf(map[string]any{"type": "frame", "text": "Onboarding", "x": 0, "y": 500, "w": 100, "h": 100})},
				{Op: "add", Ref: "$s", El: elOf(map[string]any{"type": "sticky", "text": "Fewer steps"})},
				{Op: "reparent", IDs: []string{"$s", eid("old")}, Frame: raw("$f")},
				{Op: "set", ID: eid("old"), Fields: raw(map[string]any{"style": map[string]any{"color": "blue"}})},
				{Op: "add", El: elOf(map[string]any{"type": "connector", "ends": []any{map[string]any{"el": "$s"}, map[string]any{"el": eid("old")}}})},
			},
			Layout: []Layout{{Kind: "grid", IDs: []string{"$s", eid("old")}, In: "$f", Columns: 4}},
		})
		if err != nil {
			t.Fatal(err)
		}
		f, s := out.IDs["$f"], out.IDs["$s"]
		if out.Seq != seq+1 || out.Layout[s] != [2]float64{32, 532} || out.Layout[eid("old")] != [2]float64{256, 532} {
			t.Fatalf("outcome %+v", out)
		}
		if fg := h.geom(f); fg.W != 388 || fg.H != 264 {
			t.Fatalf("frame did not grow: %v", fg)
		}
		if h.row(s).El.Frame != f || h.row(eid("old")).El.Style["color"] != "blue" || h.row(s).El.Geom.W != 200 {
			t.Fatal("refs or defaults did not apply")
		}
		if out.Delta.By.Kind != "agent" || out.Delta.Seq != out.Seq {
			t.Fatalf("delta by %+v", out.Delta.By)
		}
		// the same layout gives the same coordinates again
		again, err := h.agent(AgentCall{Layout: []Layout{{Kind: "grid", IDs: []string{s, eid("old")}, In: f, Columns: 4}}})
		if err != nil || again.Seq != 0 || again.Layout[s] != out.Layout[s] {
			t.Fatalf("repeated layout %+v %v", again, err)
		}
	})

	t.Run("ends without at attach to the sides that face each other", func(t *testing.T) {
		h := newHarness(t)
		conn := func(ends ...any) AgentOp {
			return AgentOp{Op: "add", Ref: "$c", El: elOf(map[string]any{"type": "connector", "ends": ends})}
		}
		sides := func(out AgentOutcome) [2][2]float64 {
			e := h.row(out.IDs["$c"]).El.Ends
			return [2][2]float64{e[0].At, e[1].At}
		}
		left, right, top, bottom := [2]float64{0, 0.5}, [2]float64{1, 0.5}, [2]float64{0.5, 0}, [2]float64{0.5, 1}
		add := []AgentOp{
			{Op: "add", Ref: "$a", El: elOf(map[string]any{"type": "sticky", "text": "A"})},
			{Op: "add", Ref: "$b", El: elOf(map[string]any{"type": "sticky", "text": "B"})},
		}
		// New elements go in a row, so the ends face left and right.
		out, err := h.agent(AgentCall{Ops: append(slices.Clone(add), conn(map[string]any{"el": "$a"}, map[string]any{"el": "$b"}))})
		if err != nil || sides(out) != [2][2]float64{right, left} {
			t.Fatalf("row: %v %v", sides(out), err)
		}
		// Layout runs before the ends are placed: a column faces top and bottom.
		out, err = h.agent(AgentCall{
			Ops:    append(slices.Clone(add), conn(map[string]any{"el": "$a"}, map[string]any{"el": "$b"})),
			Layout: []Layout{{Kind: "column", IDs: []string{"$a", "$b"}}},
		})
		if err != nil || sides(out) != [2][2]float64{bottom, top} {
			t.Fatalf("column: %v %v", sides(out), err)
		}
		// An explicit at stays; the other end faces the explicit point, and a
		// free end faces its coordinate.
		a, b := out.IDs["$a"], out.IDs["$b"]
		out, err = h.agent(AgentCall{Ops: []AgentOp{conn(map[string]any{"el": a, "at": []float64{0, 0}}, map[string]any{"el": b})}})
		if err != nil || sides(out) != [2][2]float64{{0, 0}, top} {
			t.Fatalf("explicit: %v %v", sides(out), err)
		}
		g := h.geom(a)
		out, err = h.agent(AgentCall{Ops: []AgentOp{conn(map[string]any{"pt": []float64{g.X - 500, g.Y + 100}}, map[string]any{"el": a})}})
		if err != nil || sides(out)[1] != left {
			t.Fatalf("free: %v %v", sides(out), err)
		}
		// set ends places an end it attaches without at; the other end,
		// attached earlier, stays.
		c := out.IDs["$c"]
		if _, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "set", ID: c, Fields: raw(map[string]any{"ends": []any{map[string]any{"el": b}, nil}})}}}); err != nil {
			t.Fatal(err)
		}
		if e := h.row(c).El.Ends; e[0].El != b || e[0].At != top || e[1].At != left {
			t.Fatalf("set ends: %+v", e)
		}
		// A replacement places its connectors from the replacing elements.
		out, err = h.agent(AgentCall{Replace: true, Ops: []AgentOp{
			{Op: "add", Ref: "$a", El: elOf(map[string]any{"type": "sticky", "x": 0, "y": 0})},
			{Op: "add", Ref: "$b", El: elOf(map[string]any{"type": "sticky", "x": 600, "y": 0})},
			conn(map[string]any{"el": "$b"}, map[string]any{"el": "$a"}),
		}})
		if err != nil || sides(out) != [2][2]float64{left, right} {
			t.Fatalf("replace: %v %v", sides(out), err)
		}
	})

	t.Run("a frame's x alone carries its members", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addFrame("f", 0, 0, 500, 500))
		h.ok(a, addIn("m", "f", h.inc(eid("f")), 10, 10))
		if _, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "set", ID: eid("f"), Fields: raw(map[string]any{"x": 100})}}}); err != nil {
			t.Fatal(err)
		}
		if h.geom(eid("m")).X != 110 {
			t.Fatal("members did not follow")
		}
		h.agent(AgentCall{Ops: []AgentOp{{Op: "set", ID: eid("f"), Fields: raw(map[string]any{"x": 0, "w": 600})}}})
		h.agent(AgentCall{Ops: []AgentOp{{Op: "set", ID: eid("f"), Fields: raw(map[string]any{"y": 50}), Carry: raw(false)}}})
		if m := h.geom(eid("m")); m.X != 110 || m.Y != 10 {
			t.Fatalf("members followed a resize or carry false: %v", m)
		}
	})

	t.Run("guards and atomicity", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("x", 0, 0), addSticky("y", 0, 0))
		ver, _ := h.s.Ver(eid("x"))
		stale := int64(1)
		cases := map[string]struct {
			call     AgentCall
			category string
		}{
			"if_seq":        {AgentCall{IfSeq: &stale, Ops: []AgentOp{{Op: "delete", IDs: []string{eid("y")}}}}, CategoryConflict},
			"if_ver":        {AgentCall{Ops: []AgentOp{{Op: "set", ID: eid("x"), Fields: raw(map[string]any{"text": "a"}), IfVer: raw(ver - 1)}}}, CategoryConflict},
			"missing":       {AgentCall{Ops: []AgentOp{{Op: "delete", IDs: []string{eid("y")}}, {Op: "set", ID: eid("nope"), Fields: raw(map[string]any{"text": "a"})}}}, CategoryConflict},
			"format":        {AgentCall{Ops: []AgentOp{{Op: "delete", IDs: []string{eid("y")}}, {Op: "set", ID: eid("x"), Fields: raw(map[string]any{"style": map[string]any{"color": "ink"}})}}}, CategoryInvalid},
			"frame field":   {AgentCall{Ops: []AgentOp{{Op: "set", ID: eid("x"), Fields: raw(map[string]any{"frame": nil})}}}, CategoryInvalid},
			"limit":         {AgentCall{Ops: []AgentOp{{Op: "set", ID: eid("x"), Fields: raw(map[string]any{"text": strings.Repeat("x", 4001)})}}}, CategoryInvalid},
			"unknown ref":   {AgentCall{Ops: []AgentOp{{Op: "delete", IDs: []string{"$nope"}}}}, CategoryInvalid},
			"bad layout":    {AgentCall{Layout: []Layout{{Kind: "spiral", IDs: []string{eid("x")}}}}, CategoryInvalid},
			"locked layout": {AgentCall{Ops: []AgentOp{{Op: "set", ID: eid("x"), Fields: raw(map[string]any{"locked": true})}}, Layout: []Layout{{Kind: "row", IDs: []string{eid("x")}}}}, CategoryConflict},
		}
		for name, c := range cases {
			seq := h.s.Seq()
			_, err := h.agent(c.call)
			var ae *AgentError
			if !errors.As(err, &ae) || ae.Category != c.category {
				t.Errorf("%s: err = %v", name, err)
			}
			if h.s.Seq() != seq || !h.alive(eid("y")) || h.row(eid("x")).El.Text != "" || h.row(eid("x")).El.Locked {
				t.Errorf("%s: a refused call changed the board", name)
			}
		}
		if _, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "set", ID: eid("x"), Fields: raw(map[string]any{"text": "a"}), IfVer: raw(ver)}}}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("locked targets need an earlier unlock", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("x", 0, 0))
		h.ok(a, Op{Op: OpSet, ID: eid("x"), Inc: 0, G: model.BundleLocked, Val: raw(true)})
		move := AgentOp{Op: "set", ID: eid("x"), Fields: raw(map[string]any{"x": 50})}
		if _, err := h.agent(AgentCall{Ops: []AgentOp{move}}); err == nil {
			t.Fatal("moved a locked element")
		}
		if _, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "set", ID: eid("x"), Fields: raw(map[string]any{"locked": false, "x": 50})}}}); err == nil {
			t.Fatal("unlocked and moved in one operation")
		}
		unlock := AgentOp{Op: "set", ID: eid("x"), Fields: raw(map[string]any{"locked": false})}
		if _, err := h.agent(AgentCall{Ops: []AgentOp{unlock, move}, Layout: []Layout{{Kind: "column", IDs: []string{eid("x")}}}}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("replace", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("x", 0, 0), addSticky("y", 0, 0))
		xInc := h.inc(eid("x"))
		out, err := h.agent(AgentCall{Replace: true, Ops: []AgentOp{
			{Op: "add", El: elOf(map[string]any{"id": eid("x"), "type": "sticky", "x": 0, "y": 0, "w": 100, "h": 100, "text": "kept"})},
			{Op: "add", Ref: "$n", El: elOf(map[string]any{"type": "text", "text": "new"})},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if out.Delta.Reset.Kind != ResetReplace || h.inc(eid("x")) != xInc || h.alive(eid("y")) || !h.alive(out.IDs["$n"]) {
			t.Fatalf("replace outcome %+v", out)
		}
		if _, err := h.agent(AgentCall{Replace: true, Ops: []AgentOp{{Op: "delete", IDs: []string{eid("x")}}}}); err == nil {
			t.Fatal("a replacing call accepted a delete")
		}
	})
}

func TestLoadExport(t *testing.T) {
	h := newHarness(t)
	a := h.session("a")
	h.ok(a, addFrame("f", 0, 0, 500, 500), addSticky("x", 0, 0))
	h.ok(a, addIn("m", "f", h.inc(eid("f")), 10, 10), addConnector("c", attach("x", 1, 0.5), free(5, 5), map[string]int64{eid("x"): h.inc(eid("x"))}))
	h.ok(a, deleteOp(h, "x"))
	rows := h.s.Export()
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var back Rows
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	s, err := Load(back)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkInvariants(s); err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(s.Export())
	if string(again) != string(raw) {
		t.Fatalf("export after load differs:\n%s\n%s", raw, again)
	}
	h.s = s
	h.ok(a, undoOp)
	if c := h.row(eid("c")); !h.alive(eid("x")) || c.El.Ends[0].El != eid("x") {
		t.Fatal("undo after a reload did not restore the connection")
	}
}

func TestRetention(t *testing.T) {
	h := newHarness(t)
	a, b := h.session("a"), h.session("b")
	h.ok(a, addSticky("x", 0, 0))
	h.ok(b, addSticky("y", 0, 0))
	h.ok(a, deleteOp(h, "x"))
	h.now += SessionTTL - 10
	h.s.Hello(Hello{Session: a, Key: "browser:a", Now: h.now})
	h.now += 10 // b went idle
	var total int64
	for _, r := range h.s.receipts {
		total += receiptBytes(r)
	}
	if c := h.s.TrimReceipts(total-1, h.now); len(c.Receipts) != 1 || c.Receipts[0].Session != b {
		t.Fatalf("first trim dropped %v, want the idle session's receipt", c.Receipts)
	}
	c := h.s.TrimReceipts(1, h.now)
	if len(c.Receipts) != 2 || len(h.s.receipts) != 0 {
		t.Fatalf("trimmed %v, left %d", c.Receipts, len(h.s.receipts))
	}
	if out := h.s.ApplyBatch(Batch{Session: a, N: 1, Ops: []Op{addSticky("x", 0, 0)}, Now: h.now}); out.Fatal != FatalReceiptExpired {
		t.Fatalf("resend after trim = %+v", out)
	}
	h.s.TrimDeleted(0)
	if h.s.rows[eid("x")] != nil {
		t.Fatal("TrimDeleted kept a deleted row")
	}
	if u := undoResult(t, h.ok(a, undoOp)); u.Skipped[VoidGone] != 1 {
		t.Fatalf("undo of a purged deletion = %+v", u)
	}
	if ch := h.s.ExpireSessions(h.now); len(ch.Sessions) != 1 || h.s.sessions[b] != nil {
		t.Fatalf("expire = %+v", ch)
	}
	if err := checkInvariants(h.s); err != nil {
		t.Fatal(err)
	}
}
