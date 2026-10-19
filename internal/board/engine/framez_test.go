package engine

import (
	"slices"
	"testing"

	"github.com/gosuda/flats/internal/board/model"
)

// TestAgentFramesGoBehind checks the stacking rule for frames an agent call
// adds without z (8.1): behind every element, in call order among the
// call's own frames, while other new elements stay in front.
func TestAgentFramesGoBehind(t *testing.T) {
	// pos returns each id's position in the full stacking order.
	pos := func(h *harness) map[string]int {
		m := map[string]int{}
		for i, id := range h.s.order() {
			m[id] = i
		}
		return m
	}
	seed := func(t *testing.T) (*harness, []string) {
		h := newHarness(t)
		a := h.session("a")
		var ids []string
		for i, n := range []string{"s1", "s2", "s3", "s4"} {
			h.ok(a, addSticky(n, float64(i)*300, 0))
			ids = append(ids, eid(n))
		}
		out, err := h.agent(AgentCall{Ops: []AgentOp{
			{Op: "add", Ref: "$t", El: elOf(map[string]any{"type": "sticky", "text": "top", "x": 0, "y": 900})},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return h, append(ids, out.IDs["$t"])
	}

	t.Run("members moved into a new frame stay above it", func(t *testing.T) {
		h, stickies := seed(t)
		out, err := h.agent(AgentCall{
			Ops: []AgentOp{
				{Op: "add", Ref: "$f", El: elOf(map[string]any{"type": "frame", "text": "Topic", "x": 0, "y": 2000, "w": 100, "h": 100})},
				{Op: "reparent", IDs: stickies, Frame: raw("$f")},
			},
			Layout: []Layout{{Kind: "grid", IDs: stickies, In: "$f", Columns: 3}},
		})
		if err != nil {
			t.Fatal(err)
		}
		f, p := out.IDs["$f"], pos(h)
		if p[f] != 0 {
			t.Fatalf("frame at %d of %v", p[f], h.s.order())
		}
		for _, id := range stickies {
			if h.row(id).El.Frame != f || p[id] < p[f] {
				t.Fatalf("sticky %s (frame %q) at %d is not above frame at %d", id, h.row(id).El.Frame, p[id], p[f])
			}
		}
	})

	t.Run("a frame alone goes below every element", func(t *testing.T) {
		h, stickies := seed(t)
		first, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "add", Ref: "$f", El: elOf(map[string]any{"type": "frame", "text": "One"})}}})
		if err != nil {
			t.Fatal(err)
		}
		second, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "add", Ref: "$f", El: elOf(map[string]any{"type": "frame", "text": "Two"})}}})
		if err != nil {
			t.Fatal(err)
		}
		want := append([]string{second.IDs["$f"], first.IDs["$f"]}, stickies...)
		if got := h.s.order(); !slices.Equal(got[:2], want[:2]) {
			t.Fatalf("order %v, want frames %v first", got, want[:2])
		}
	})

	t.Run("frames of one call keep their order behind everything", func(t *testing.T) {
		h, stickies := seed(t)
		out, err := h.agent(AgentCall{Ops: []AgentOp{
			{Op: "add", Ref: "$f1", El: elOf(map[string]any{"type": "frame", "text": "One"})},
			{Op: "add", Ref: "$n", El: elOf(map[string]any{"type": "sticky", "text": "new"})},
			{Op: "add", Ref: "$f2", El: elOf(map[string]any{"type": "frame", "text": "Two"})},
			{Op: "add", Ref: "$f3", El: elOf(map[string]any{"type": "frame", "text": "Three"})},
		}})
		if err != nil {
			t.Fatal(err)
		}
		f1, f2, f3, n := out.IDs["$f1"], out.IDs["$f2"], out.IDs["$f3"], out.IDs["$n"]
		got := h.s.order()
		if !slices.Equal(got[:3], []string{f1, f2, f3}) || got[len(got)-1] != n {
			t.Fatalf("order %v: want %v first and %s last (stickies %v)", got, []string{f1, f2, f3}, n, stickies)
		}
	})

	t.Run("a frame after an earlier send to back still goes below it", func(t *testing.T) {
		h, stickies := seed(t)
		out, err := h.agent(AgentCall{Ops: []AgentOp{
			{Op: "add", Ref: "$f1", El: elOf(map[string]any{"type": "frame", "text": "One"})},
			{Op: "zorder", IDs: []string{stickies[2]}, To: raw("back")},
			{Op: "add", Ref: "$f2", El: elOf(map[string]any{"type": "frame", "text": "Two"})},
			{Op: "add", Ref: "$e", El: elOf(map[string]any{"type": "sticky", "text": "explicit", "z": "0001"})},
			{Op: "add", Ref: "$f3", El: elOf(map[string]any{"type": "frame", "text": "Three"})},
		}})
		if err != nil {
			t.Fatal(err)
		}
		got := h.s.order()
		if got[0] != out.IDs["$f3"] || got[1] != out.IDs["$e"] || got[2] != out.IDs["$f2"] {
			t.Fatalf("order %v: want f3, explicit sticky, f2 first", got)
		}
	})

	t.Run("an explicit z is kept", func(t *testing.T) {
		h, _ := seed(t)
		out, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "add", Ref: "$f", El: elOf(map[string]any{"type": "frame", "text": "Front", "z": "z"})}}})
		if err != nil {
			t.Fatal(err)
		}
		if z := h.row(out.IDs["$f"]).El.Z; z != "z" {
			t.Fatalf("z %q", z)
		}
	})

	t.Run("a person add without z still goes on top", func(t *testing.T) {
		h, _ := seed(t)
		a := h.session("b")
		h.ok(a, addFrame("pf", 0, 0, 100, 100))
		if got := h.s.order(); got[len(got)-1] != eid("pf") {
			t.Fatalf("order %v", got)
		}
	})

	t.Run("a replacement puts its frames behind its other elements", func(t *testing.T) {
		for _, keyed := range []bool{false, true} {
			h, _ := seed(t)
			sticky := map[string]any{"type": "sticky", "text": "keyed", "x": 0, "y": 0}
			if keyed {
				sticky["z"] = "1"
			}
			out, err := h.agent(AgentCall{Replace: true, Ops: []AgentOp{
				{Op: "add", Ref: "$s", El: elOf(sticky)},
				{Op: "add", Ref: "$f1", El: elOf(map[string]any{"type": "frame", "text": "One", "x": 0, "y": 0, "w": 400, "h": 400})},
				{Op: "add", Ref: "$n", El: elOf(map[string]any{"type": "sticky", "text": "new", "x": 50, "y": 50, "frame": "$f1"})},
				{Op: "add", Ref: "$f2", El: elOf(map[string]any{"type": "frame", "text": "Two", "x": 500, "y": 0, "w": 400, "h": 400})},
			}})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{out.IDs["$f1"], out.IDs["$f2"], out.IDs["$s"], out.IDs["$n"]}
			if got := h.s.order(); !slices.Equal(got, want) {
				t.Fatalf("keyed %v: order %v, want %v", keyed, got, want)
			}
			if keyed && h.row(out.IDs["$s"]).El.Z != "1" {
				t.Fatal("explicit z changed")
			}
			for _, id := range want {
				if !model.ValidKey(h.row(id).El.Z) {
					t.Fatalf("key %q", h.row(id).El.Z)
				}
			}
		}
	})
}

// TestAgentFramesGoBehindOnEmptyBoard checks the same rule when the call
// starts on a board with no elements, so the first frame has nothing to go
// below: every later unkeyed frame of the call must still go strictly below
// every non-frame element the call added before it, with or without z, and
// the call's frames keep their order (8.1).
func TestAgentFramesGoBehindOnEmptyBoard(t *testing.T) {
	frame := func(ref string) AgentOp {
		return AgentOp{Op: "add", Ref: ref, El: elOf(map[string]any{"type": "frame", "text": ref, "x": 0, "y": 0, "w": 400, "h": 400})}
	}
	sticky := func(ref, z string) AgentOp {
		el := map[string]any{"type": "sticky", "text": ref, "x": 50, "y": 50}
		if z != "" {
			el["z"] = z
		}
		return AgentOp{Op: "add", Ref: ref, El: elOf(el)}
	}
	// check verifies the strict key order of want (lowest first) and that
	// every key is valid.
	check := func(t *testing.T, h *harness, out AgentOutcome, want []string) {
		t.Helper()
		var ids, zs []string
		for _, ref := range want {
			id := out.IDs[ref]
			ids = append(ids, id)
			zs = append(zs, h.row(id).El.Z)
			if !model.ValidKey(h.row(id).El.Z) {
				t.Fatalf("%s key %q", ref, h.row(id).El.Z)
			}
		}
		if got := h.s.order(); !slices.Equal(got, ids) {
			t.Fatalf("order %v, want %v (%v)", got, ids, want)
		}
		for i := 1; i < len(zs); i++ {
			if zs[i-1] >= zs[i] {
				t.Fatalf("keys %v of %v are not strictly increasing", zs, want)
			}
		}
	}

	t.Run("a sticky between frames", func(t *testing.T) {
		h := newHarness(t)
		out, err := h.agent(AgentCall{Ops: []AgentOp{frame("$f1"), sticky("$s", ""), frame("$f2"), frame("$f3")}})
		if err != nil {
			t.Fatal(err)
		}
		check(t, h, out, []string{"$f1", "$f2", "$f3", "$s"})
		// The sticky moved into the last frame stays above its fill.
		s, f3 := out.IDs["$s"], out.IDs["$f3"]
		if _, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "reparent", IDs: []string{s}, Frame: raw(f3)}}}); err != nil {
			t.Fatal(err)
		}
		if h.row(s).El.Frame != f3 || h.row(s).El.Z <= h.row(f3).El.Z {
			t.Fatalf("sticky %q (frame %q) not above frame %q", h.row(s).El.Z, h.row(s).El.Frame, h.row(f3).El.Z)
		}
	})

	t.Run("an explicit z above the earlier frames", func(t *testing.T) {
		h := newHarness(t)
		out, err := h.agent(AgentCall{Ops: []AgentOp{frame("$f1"), sticky("$e", "b"), frame("$f2"), sticky("$s", ""), frame("$f3")}})
		if err != nil {
			t.Fatal(err)
		}
		if z := h.row(out.IDs["$e"]).El.Z; z != "b" {
			t.Fatalf("explicit z changed to %q", z)
		}
		check(t, h, out, []string{"$f1", "$f2", "$f3", "$e", "$s"})
	})

	t.Run("an explicit z below the earlier frames", func(t *testing.T) {
		h := newHarness(t)
		out, err := h.agent(AgentCall{Ops: []AgentOp{frame("$f1"), sticky("$e", "0001"), frame("$f2"), frame("$f3")}})
		if err != nil {
			t.Fatal(err)
		}
		// The explicit key is below the first frame, so later frames go
		// below it and keep their order among themselves.
		check(t, h, out, []string{"$f2", "$f3", "$e", "$f1"})
	})

	t.Run("a replacement", func(t *testing.T) {
		for _, z := range []string{"", "b", "0001"} {
			h := newHarness(t)
			out, err := h.agent(AgentCall{Replace: true, Ops: []AgentOp{frame("$f1"), sticky("$s", ""), frame("$f2"), sticky("$e", z), frame("$f3")}})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"$f1", "$f2", "$f3", "$e", "$s"}
			if z == "" {
				want = []string{"$f1", "$f2", "$f3", "$s", "$e"}
			}
			check(t, h, out, want)
		}
	})
}

// TestAgentReattachRecomputesSides checks what the agent guide tells agents
// to do after moving connected elements: an attached end keeps the side it
// was given, and setting the ends again without "at" recomputes the sides
// from the elements' final places, after the call's layout (4.4, 8.1).
func TestAgentReattachRecomputesSides(t *testing.T) {
	h := newHarness(t)
	left, right, top, bottom := [2]float64{0, 0.5}, [2]float64{1, 0.5}, [2]float64{0.5, 0}, [2]float64{0.5, 1}
	out, err := h.agent(AgentCall{Ops: []AgentOp{
		{Op: "add", Ref: "$a", El: elOf(map[string]any{"type": "sticky", "text": "A", "x": 0, "y": 0})},
		{Op: "add", Ref: "$b", El: elOf(map[string]any{"type": "sticky", "text": "B", "x": 600, "y": 0})},
		{Op: "add", Ref: "$c", El: elOf(map[string]any{"type": "connector", "ends": []any{map[string]any{"el": "$a"}, map[string]any{"el": "$b"}}})},
	}})
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := out.IDs["$a"], out.IDs["$b"], out.IDs["$c"]
	sides := func() [2][2]float64 {
		e := h.row(c).El.Ends
		return [2][2]float64{e[0].At, e[1].At}
	}
	if sides() != [2][2]float64{right, left} {
		t.Fatalf("initial %v", sides())
	}
	// Moving A to the right of B keeps the stored sides.
	if _, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "set", ID: a, Fields: raw(map[string]any{"x": 1200})}}}); err != nil {
		t.Fatal(err)
	}
	if sides() != [2][2]float64{right, left} {
		t.Fatalf("after move %v", sides())
	}
	reattach := AgentOp{Op: "set", ID: c, Fields: raw(map[string]any{"ends": []any{map[string]any{"el": a}, map[string]any{"el": b}}})}
	if _, err := h.agent(AgentCall{Ops: []AgentOp{reattach}}); err != nil {
		t.Fatal(err)
	}
	if sides() != [2][2]float64{left, right} {
		t.Fatalf("after re-attach %v", sides())
	}
	// In one call with a layout, the sides follow the laid-out places.
	if _, err := h.agent(AgentCall{Ops: []AgentOp{reattach}, Layout: []Layout{{Kind: "column", IDs: []string{a, b}}}}); err != nil {
		t.Fatal(err)
	}
	if sides() != [2][2]float64{bottom, top} {
		t.Fatalf("after layout and re-attach %v", sides())
	}
}
