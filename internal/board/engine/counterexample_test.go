package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/board/model"
)

// TestCounterexamples holds the deterministic counterexamples of section 14
// of the board technical design that need neither a database nor a network.
func TestCounterexamples(t *testing.T) {
	t.Run("edits to elements that stayed alive after a rollback", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("x", 0, 0), addSticky("y", 200, 0), addSticky("z", 400, 0))
		xInc, zInc := h.inc(eid("x")), h.inc(eid("z"))
		rev := content(h.s)
		h.ok(a, setText("x", xInc, "after R", 0), deleteOp(h, "z"))
		h.ok(a, addSticky("w", 600, 0))
		late := []Op{move(h, "x", 50, 50, ""), setText("w", h.inc(eid("w")), "late", 0), setText("z", zInc, "late", 0)}
		out, err := h.s.Rollback(1, rev, h.now)
		if err != nil {
			t.Fatal(err)
		}
		if out.Delta.Reset == nil || out.Delta.Reset.Kind != ResetRollback || h.inc(eid("x")) != xInc {
			t.Fatalf("rollback delta %+v, x inc %d", out.Delta.Reset, h.inc(eid("x")))
		}
		if h.row(eid("x")).El.Text != "" || h.alive(eid("w")) || !h.alive(eid("z")) || h.inc(eid("z")) == zInc {
			t.Fatal("rollback did not produce R's board")
		}
		rs := results(t, h.ok(a, late...))
		if rs[0] != ResultOK || rs[1] != void(VoidGone) || rs[2] != void(VoidGone) {
			t.Fatalf("late results = %v", rs)
		}
		if g := h.geom(eid("x")); g.X != 50 {
			t.Fatalf("x = %v", g)
		}
	})

	t.Run("a lost ack plus another person's overwrite", func(t *testing.T) {
		h := newHarness(t)
		a, b := h.session("a"), h.session("b")
		h.ok(a, addSticky("x", 0, 0))
		mine := h.ok(a, move(h, "x", 10, 0, "")) // the ack is lost
		h.ok(b, move(h, "x", 20, 0, ""))
		resent := h.s.ApplyBatch(Batch{Session: a, N: h.n[a], Ops: []Op{move(h, "x", 10, 0, "")}, Now: h.now})
		if !resent.Replayed || resent.AckSeq != mine.Seq || resent.Delta != nil {
			t.Fatalf("resend = %+v", resent)
		}
		if g := h.geom(eid("x")); g.X != 20 {
			t.Fatalf("x = %v, want B's 20", g.X)
		}
	})

	t.Run("session expiry with pending batches", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("x", 0, 0))
		h.now += SessionTTL
		if r := h.s.Hello(Hello{Session: a, Pending: 1, NewID: "s-new", Key: "browser:a", Now: h.now}); r.Fatal != FatalSessionExpired {
			t.Fatalf("hello = %+v", r)
		}
		if out := h.s.ApplyBatch(Batch{Session: a, N: 2, Ops: []Op{move(h, "x", 1, 1, "")}, Now: h.now}); out.Fatal != FatalSessionExpired {
			t.Fatalf("batch on an expired session = %+v", out)
		}
		if r := h.s.Hello(Hello{Session: a, NewID: "s-new", Key: "browser:a", Now: h.now}); r.Session != "s-new" || !r.New {
			t.Fatalf("hello without pending = %+v", r)
		}
		if _, ok := h.s.CreatedInc(CreatedKey{a, eid("x")}); ok {
			t.Fatal("the expired session's created row survived")
		}
	})

	t.Run("number retention after a transient failure", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		m := h.s.Savepoint()
		out := h.s.ApplyBatch(Batch{Session: a, N: 1, Ops: []Op{addSticky("x", 0, 0)}, Now: h.now})
		if out.Seq == 0 {
			t.Fatal("batch did not apply")
		}
		h.s.RollbackTo(m) // the store refused it as transient
		if h.s.sessions[a].MaxN != 0 || h.s.rows[eid("x")] != nil || h.s.Seq() != 1 {
			t.Fatal("rollback kept the batch")
		}
		if err := checkInvariants(h.s); err != nil {
			t.Fatal(err)
		}
		out = h.s.ApplyBatch(Batch{Session: a, N: 1, Ops: []Op{addSticky("x", 0, 0)}, Now: h.now})
		if out.Replayed || out.Seq != 2 || !h.alive(eid("x")) {
			t.Fatalf("resend = %+v", out)
		}
	})

	t.Run("a member's membership changing while a group moves into a frame", func(t *testing.T) {
		for _, order := range []string{"group move first", "member move first"} {
			h := newHarness(t)
			a, b := h.session("a"), h.session("b")
			h.ok(a, addFrame("f1", 0, 0, 1000, 1000), addFrame("f2", 2000, 0, 1000, 1000), addSticky("p", 3000, 0), addSticky("q", 3200, 0))
			h.ok(a, groupOp(h, "g", "p", "q"))
			groupMove, memberMove := reparentOp(h, "f1", "p"), reparentOp(h, "f2", "q")
			if order == "group move first" {
				h.ok(a, groupMove)
				h.ok(b, memberMove)
			} else {
				h.ok(b, memberMove)
				h.ok(a, groupMove)
			}
			if fp, fq := h.row(eid("p")).El.Frame, h.row(eid("q")).El.Frame; fp != fq {
				t.Fatalf("%s: group split across %s and %s", order, fp, fq)
			}
		}
	})

	t.Run("two people moving a frame at once", func(t *testing.T) {
		h := newHarness(t)
		a, b, c := h.session("a"), h.session("b"), h.session("c")
		h.ok(a, addFrame("f", 0, 0, 1000, 1000))
		h.ok(a, addIn("m", "f", h.inc(eid("f")), 100, 100), addIn("n", "f", h.inc(eid("f")), 300, 100))
		fromA, fromB := move(h, "f", 10, 0, CarryMembers), move(h, "f", 20, 0, CarryMembers)
		h.ok(a, fromA)
		h.ok(c, move(h, "n", 500, 500, ""))
		h.ok(b, fromB)
		if f, m, n := h.geom(eid("f")), h.geom(eid("m")), h.geom(eid("n")); f.X != 20 || m.X != 120 || n.X != 510 {
			t.Fatalf("frame %v, member %v, moved member %v", f.X, m.X, n.X)
		}
	})

	t.Run("a late operation on an element revived after deletion", func(t *testing.T) {
		h := newHarness(t)
		a, b := h.session("a"), h.session("b")
		h.ok(a, addSticky("x", 0, 0))
		old := h.inc(eid("x"))
		late := setText("x", old, "late", 0)
		h.ok(a, deleteOp(h, "x"))
		h.ok(a, undoOp)
		if !h.alive(eid("x")) || h.inc(eid("x")) == old {
			t.Fatal("undo did not revive x with a new incarnation")
		}
		if rs := results(t, h.ok(b, late)); rs[0] != void(VoidGone) {
			t.Fatalf("late edit = %v", rs)
		}
	})

	t.Run("connector detach coordinates when the target is deleted", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addShape("t", 10, 20, 100, 50, 90), addSticky("u", 500, 0))
		h.ok(a, addConnector("c", attach("t", 1, 0.5), attach("u", 0, 0.5), map[string]int64{eid("t"): h.inc(eid("t")), eid("u"): h.inc(eid("u"))}))
		out := h.ok(a, deleteOp(h, "t"))
		c := h.row(eid("c"))
		want := model.EndpointWorld(model.Geom{X: 10, Y: 20, W: 100, H: 50, Rot: 90}, [2]float64{1, 0.5})
		if c.El.Ends[0] != (model.End{Pt: want}) || want != [2]float64{60, 95} || c.V[model.BundleEnd0] != out.Seq {
			t.Fatalf("detached end = %+v (v %d), want %v", c.El.Ends[0], c.V[model.BundleEnd0], want)
		}
		var change *model.Change
		for i := range out.Delta.Changes {
			if out.Delta.Changes[i].ID == eid("c") {
				change = &out.Delta.Changes[i]
			}
		}
		if change == nil || string(change.Set[model.BundleEnd0].Val) != `{"pt":[60,95]}` || change.Set[model.BundleEnd0].V != out.Seq {
			t.Fatalf("delta change = %+v", change)
		}
	})

	t.Run("consecutive undos with interleaved edits", func(t *testing.T) {
		h := newHarness(t)
		a, b := h.session("a"), h.session("b")
		h.ok(a, addSticky("x", 0, 0))
		h.ok(a, move(h, "x", 1, 0, ""))
		h.ok(b, move(h, "x", 2, 0, ""))
		h.ok(a, move(h, "x", 3, 0, ""))
		if u := undoResult(t, h.ok(a, undoOp)); u.Undone != 1 || h.geom(eid("x")).X != 2 {
			t.Fatalf("first undo %+v, x = %v", u, h.geom(eid("x")).X)
		}
		if u := undoResult(t, h.do(a, undoOp)); u.Undone != 0 || u.Skipped[VoidStale] != 1 || h.geom(eid("x")).X != 2 {
			t.Fatalf("second undo %+v, x = %v", u, h.geom(eid("x")).X)
		}
	})

	t.Run("undo after a frame move where someone else moved a member", func(t *testing.T) {
		h := newHarness(t)
		a, b := h.session("a"), h.session("b")
		h.ok(a, addFrame("f", 0, 0, 1000, 1000))
		h.ok(b, addIn("x", "f", h.inc(eid("f")), 100, 100), addIn("y", "f", h.inc(eid("f")), 300, 100))
		h.ok(a, move(h, "f", 50, 0, CarryMembers))
		h.ok(b, move(h, "x", 700, 700, ""))
		u := undoResult(t, h.ok(a, undoOp))
		if f, x, y := h.geom(eid("f")), h.geom(eid("x")), h.geom(eid("y")); f.X != 0 || y.X != 300 || x.X != 700 || u.Skipped[VoidStale] != 1 {
			t.Fatalf("frame %v, x %v, y %v, undo %+v", f.X, x.X, y.X, u)
		}
	})

	t.Run("undo of a group's frame move", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addFrame("f1", 0, 0, 1000, 1000), addFrame("f2", 2000, 0, 1000, 1000))
		h.ok(a, addIn("p", "f1", h.inc(eid("f1")), 10, 10), addIn("q", "f1", h.inc(eid("f1")), 200, 10))
		h.ok(a, groupOp(h, "g", "p", "q"))
		h.ok(a, reparentOp(h, "f2", "p"))
		if h.row(eid("q")).El.Frame != eid("f2") {
			t.Fatal("the group did not move together")
		}
		u := undoResult(t, h.ok(a, undoOp))
		if h.row(eid("p")).El.Frame != eid("f1") || h.row(eid("q")).El.Frame != eid("f1") || u.Undone != 2 {
			t.Fatalf("after undo p in %s, q in %s, %+v", h.row(eid("p")).El.Frame, h.row(eid("q")).El.Frame, u)
		}
	})

	t.Run("creation undo after undoing my own edit of the created element", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("x", 0, 0))
		h.ok(a, setText("x", 0, "mine", 0))
		u1 := undoResult(t, h.ok(a, undoOp))
		u2 := undoResult(t, h.ok(a, undoOp))
		if h.alive(eid("x")) || u1.Undone != 1 || u2.Undone != 1 {
			t.Fatalf("x alive %v, undos %+v %+v", h.alive(eid("x")), u1, u2)
		}
	})

	t.Run("deletion undo of an element someone else deleted again", func(t *testing.T) {
		h := newHarness(t)
		a, b := h.session("a"), h.session("b")
		h.ok(a, addSticky("x", 0, 0))
		del := h.ok(a, deleteOp(h, "x"))
		h.ok(b, Op{Op: OpRevive, IDs: []string{eid("x")}, IfDel: del.Seq})
		h.ok(b, deleteOp(h, "x"))
		u := undoResult(t, h.do(a, undoOp))
		if h.alive(eid("x")) || u.Undone != 0 || u.Skipped[VoidStale] != 1 {
			t.Fatalf("x alive %v, undo %+v", h.alive(eid("x")), u)
		}
	})

	t.Run("deletion undo after deleting a connector and its target at once", func(t *testing.T) {
		for _, order := range [][]string{{"c", "t"}, {"t", "c"}} {
			h := newHarness(t)
			a := h.session("a")
			h.ok(a, addSticky("t", 0, 0), addSticky("u", 500, 0))
			h.ok(a, addConnector("c", attach("t", 1, 0.5), attach("u", 0, 0.5), map[string]int64{eid("t"): h.inc(eid("t")), eid("u"): h.inc(eid("u"))}))
			for _, separate := range []bool{false, true} {
				ops := []Op{deleteOp(h, order...)}
				if separate {
					ops = []Op{deleteOp(h, order[0]), deleteOp(h, order[1])}
				}
				h.ok(a, ops...)
				h.ok(a, undoOp)
				c := h.row(eid("c"))
				if !c.alive() || !h.alive(eid("t")) || c.El.Ends[0].El != eid("t") || c.EndInc[0] != h.inc(eid("t")) {
					t.Fatalf("order %v, separate ops %v: connector after undo %+v", order, separate, c.El.Ends)
				}
			}
		}
	})

	t.Run("undo of a frame move with 10,000 pens", func(t *testing.T) {
		h := newHarness(t)
		h.s.SetLimits(model.Limits{Elements: 20000, Text: 4000, Label: 200, PenPoints: 2000, Assets: 512})
		c := Content{Title: "Board", Grid: true}
		frame := model.Element{ID: eid("f"), Type: model.TypeFrame, Geom: model.Geom{W: 50000, H: 50000}, Z: "1", Style: model.DefaultStyle(model.TypeFrame)}
		c.Elements = append(c.Elements, frame)
		keys := model.Rekey(10000)
		for i := range 10000 {
			c.Elements = append(c.Elements, model.Element{
				ID: fmt.Sprintf("e_p%09d", i), Type: model.TypePen, Frame: frame.ID, Z: "V" + keys[i],
				Geom: model.Geom{X: float64(i%100) * 100, Y: float64(i/100) * 100, W: 80, H: 80},
				Pts:  []int{0, 0, 10000, 10000}, Style: model.DefaultStyle(model.TypePen),
			})
		}
		if _, err := h.s.Replace(c, "test", h.now); err != nil {
			t.Fatal(err)
		}
		a := h.session("a")
		out := h.ok(a, move(h, "f", 100, 0, CarryMembers))
		if len(out.Delta.Changes) != 10001 {
			t.Fatalf("frame move delta has %d changes", len(out.Delta.Changes))
		}
		if n := entryBytes(h.s.undo[a].undo[0]); n > 2<<20 {
			t.Fatalf("undo entry is %d bytes", n)
		}
		u := undoResult(t, h.ok(a, undoOp))
		if u.Undone != 10001 || h.geom(eid("f")).X != 0 || h.geom("e_p000009999").X != 9900 {
			t.Fatalf("undo %+v", u)
		}
	})

	t.Run("no author fields in public deltas", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("alice")
		out := h.ok(a, addSticky("x", 0, 0))
		raw, _ := json.Marshal(model.PublicDelta(*out.Delta))
		private, _ := json.Marshal(out.Delta)
		if strings.Contains(string(raw), "browser:alice") || strings.Contains(string(raw), `"by"`) || strings.Contains(string(raw), "s-alice") ||
			!strings.Contains(string(private), "browser:alice") {
			t.Fatalf("public %s\nprivate %s", raw, private)
		}
	})

	t.Run("create-then-edit before the ack", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("x", 0, 0))
		if rs := results(t, h.ok(a, setText("x", 0, "edited", 0))); rs[0] != ResultOK || h.row(eid("x")).El.Text != "edited" {
			t.Fatalf("results %v", rs)
		}
		b := h.session("b")
		if rs := results(t, h.ok(b, setText("x", 0, "theirs", 0))); rs[0] != void(VoidGone) {
			t.Fatalf("another session's inc 0 = %v", rs)
		}
	})

	t.Run("create-then-connect in one batch", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		rs := results(t, h.ok(a, addSticky("x", 0, 0), addSticky("y", 300, 0),
			addConnector("c", attach("x", 1, 0.5), attach("y", 0, 0.5), map[string]int64{eid("x"): 0, eid("y"): 0})))
		c := h.row(eid("c"))
		if rs[2] != ResultOK || c.EndInc != [2]int64{h.inc(eid("x")), h.inc(eid("y"))} {
			t.Fatalf("results %v, end incs %v", rs, c.EndInc)
		}
	})

	t.Run("both resent after a reconnect", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		first := []Op{addSticky("x", 0, 0), addSticky("y", 300, 0)}
		second := []Op{setText("x", 0, "x", 0), addConnector("c", attach("x", 1, 0.5), attach("y", 0, 0.5), map[string]int64{eid("x"): 0, eid("y"): 0})}
		h.ok(a, first...) // the server got the first batch; its ack was lost
		if r := h.s.Hello(Hello{Session: a, Pending: 2, Key: "browser:a", Now: h.now}); r.Session != a {
			t.Fatalf("reconnect = %+v", r)
		}
		seq := h.s.Seq()
		if out := h.s.ApplyBatch(Batch{Session: a, N: 1, Ops: first, Now: h.now}); !out.Replayed || h.s.Seq() != seq {
			t.Fatalf("first resend = %+v", out)
		}
		out := h.s.ApplyBatch(Batch{Session: a, N: 2, Ops: second, Now: h.now})
		if rs := results(t, out); rs[0] != ResultOK || rs[1] != ResultOK || len(sortedIDs(h.s)) != 3 {
			t.Fatalf("second = %v", rs)
		}
		if out := h.s.ApplyBatch(Batch{Session: a, N: 2, Ops: second, Now: h.now}); !out.Replayed || len(sortedIDs(h.s)) != 3 {
			t.Fatal("a resent batch applied twice")
		}
	})

	t.Run("resizing a frame from its top-left corner", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addFrame("f", 0, 0, 400, 300))
		h.ok(a, addIn("m", "f", h.inc(eid("f")), 50, 50))
		h.ok(a, setGeom("f", h.inc(eid("f")), model.Geom{X: -100, Y: -100, W: 500, H: 400}, ""))
		if m := h.geom(eid("m")); m.X != 50 || m.Y != 50 {
			t.Fatalf("member moved to %v", m)
		}
	})

	t.Run("concurrent and sequential text edits", func(t *testing.T) {
		h := newHarness(t)
		a, b := h.session("a"), h.session("b")
		h.ok(a, addSticky("x", 0, 0))
		seen := h.row(eid("x")).V[model.BundleText]
		mine := h.ok(a, setText("x", 0, "mine", seen))
		theirs := h.ok(b, setText("x", h.inc(eid("x")), "theirs", seen))
		if mine.Delta.Changes[0].Overwrote != 0 || theirs.Delta.Changes[0].Overwrote != mine.Seq {
			t.Fatalf("overwrote %d / %d, want 0 / %d", mine.Delta.Changes[0].Overwrote, theirs.Delta.Changes[0].Overwrote, mine.Seq)
		}
		later := h.ok(a, setText("x", 0, "again", theirs.Seq))
		if later.Delta.Changes[0].Overwrote != 0 {
			t.Fatal("a sequential edit was marked as overwriting")
		}
	})

	t.Run("rollback to a revision whose image asset row was cleaned up", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		hash := strings.Repeat("ab", 32)
		h.s.HoldAsset(Asset{Hash: hash, Mime: "image/png", W: 4, H: 3, Bytes: 99}, h.now+3600)
		h.ok(a, Op{Op: OpAdd, El: elOf(map[string]any{"id": eid("i"), "type": "image", "x": 0, "y": 0, "w": 4, "h": 3, "asset": hash, "natural": []int{4, 3}})})
		rev := content(h.s)
		h.ok(a, deleteOp(h, "i"))
		if as, _ := h.s.AssetRow(hash); as.Refs != 0 || as.DeletedRefs != 1 {
			t.Fatalf("after delete %+v", as)
		}
		h.s.PurgeDeleted([]string{eid("i")})
		h.now += 7200
		h.s.SweepAssets(h.now)
		if _, ok := h.s.AssetRow(hash); ok {
			t.Fatal("the sweep kept an unreferenced asset row")
		}
		out, err := h.s.Rollback(3, rev, h.now)
		if err != nil {
			t.Fatal(err)
		}
		as, ok := h.s.AssetRow(hash)
		if !ok || as.Refs != 1 || as.Mime != "image/png" || !h.alive(eid("i")) || !slices.Contains(out.Changed.Assets, hash) {
			t.Fatalf("asset row %+v %v", as, ok)
		}
		if err := checkInvariants(h.s); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("whole-board replacement with pending batches", func(t *testing.T) {
		h := newHarness(t)
		a, b := h.session("a"), h.session("b")
		h.ok(a, addSticky("x", 0, 0), addSticky("y", 300, 0))
		h.ok(b, move(h, "x", 5, 5, ""))
		pending := []Op{setText("x", h.inc(eid("x")), "pending", 0), setText("y", h.inc(eid("y")), "pending", 0)}
		next := content(h.s)
		next.Elements = next.Elements[:1] // keep x only
		next.Elements[0].Text = "replaced"
		out, err := h.s.Replace(next, "agent:import", h.now)
		if err != nil || out.Delta.Reset.Kind != ResetReplace || len(h.s.undo) != 0 {
			t.Fatalf("replace %v %+v, undo sessions %d", err, out.Delta, len(h.s.undo))
		}
		rs := results(t, h.ok(a, pending...))
		if rs[0] != ResultOK || rs[1] != void(VoidGone) || h.row(eid("x")).El.Text != "pending" {
			t.Fatalf("pending results %v", rs)
		}
	})

	t.Run("align a selection holding a frame and one of its members", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addFrame("f", 0, 0, 400, 400), addSticky("o", 1000, 500))
		h.ok(a, addIn("m", "f", h.inc(eid("f")), 100, 100))
		// The client sends the frame with carry and leaves its member out.
		h.ok(a, move(h, "f", 0, 500, CarryMembers), move(h, "o", 1000, 500, ""))
		if m := h.geom(eid("m")); m.Y != 600 {
			t.Fatalf("member moved to %v", m)
		}
	})
}

// orderNames is the stacking order, bottom first, as short names.
func orderNames(h *harness) string {
	var names []string
	for _, id := range h.s.order() {
		names = append(names, strings.TrimRight(strings.TrimPrefix(id, "e_"), "0"))
	}
	return strings.Join(names, " ")
}

func zorderOp(h *harness, to any, names ...string) Op {
	ids, m := incOf(h, names...)
	return Op{Op: OpZorder, IDs: ids, IncOf: m, To: raw(to)}
}

// TestReviewCounterexamples covers redo of stacking changes, one editor's
// own undo and redo, group id reuse, delete_frame_contents guards, layout of
// frames and members, replacement limits and summarized results.
func TestReviewCounterexamples(t *testing.T) {
	t.Run("redo keeps the relative order of a multi-element stacking change", func(t *testing.T) {
		for _, c := range []struct {
			name, want string
			to         func(h *harness) any
		}{
			{"front", "c d a b", func(*harness) any { return "front" }},
			{"after", "c a b d", func(h *harness) any { return map[string]any{"after": eid("c"), "inc": h.inc(eid("c"))} }},
			{"back", "c d a b", func(*harness) any { return "back" }},
		} {
			t.Run(c.name, func(t *testing.T) {
				h := newHarness(t)
				a := h.session("a")
				h.ok(a, addSticky("a", 0, 0), addSticky("b", 0, 0), addSticky("c", 0, 0), addSticky("d", 0, 0))
				if c.name == "back" {
					h.ok(a, zorderOp(h, "front", "a", "b"))
				}
				before := orderNames(h)
				h.ok(a, zorderOp(h, c.to(h), "a", "b"))
				after := orderNames(h)
				if c.name != "back" && after != c.want {
					t.Fatalf("after the change %q, want %q", after, c.want)
				}
				for range 3 {
					h.ok(a, undoOp)
					if got := orderNames(h); got != before {
						t.Fatalf("after undo %q, want %q", got, before)
					}
					h.ok(a, redoOp)
					if got := orderNames(h); got != after {
						t.Fatalf("after redo %q, want %q", got, after)
					}
				}
			})
		}
	})

	t.Run("redo of a stacking undo that skipped a stale element", func(t *testing.T) {
		h := newHarness(t)
		a, b := h.session("a"), h.session("b")
		h.ok(a, addSticky("a", 0, 0), addSticky("b", 0, 0), addSticky("c", 0, 0), addSticky("d", 0, 0))
		h.ok(a, zorderOp(h, "front", "a", "b", "c"))
		h.ok(b, zorderOp(h, "back", "b"))
		if got := orderNames(h); got != "b d a c" {
			t.Fatalf("order %q", got)
		}
		if u := undoResult(t, h.ok(a, undoOp)); u.Undone != 2 || u.Skipped[VoidStale] != 1 {
			t.Fatalf("undo %+v", u)
		}
		h.ok(a, redoOp)
		if got := orderNames(h); got != "b d a c" {
			t.Fatalf("after redo %q, want the order before the undo", got)
		}
	})

	// One editor alone undoes and redoes back to exactly the boards it had.
	t.Run("undo and redo of one editor's own deletions and relations", func(t *testing.T) {
		undoAll := func(t *testing.T, h *harness, a string, n int) {
			t.Helper()
			for range n {
				if u := undoResult(t, h.ok(a, undoOp)); u.Undone == 0 || u.Skipped[VoidStale]+u.Skipped[VoidGone] > 0 {
					t.Fatalf("undo %+v", u)
				}
			}
		}
		t.Run("a connector deleted before its target", func(t *testing.T) {
			h := newHarness(t)
			a := h.session("a")
			h.ok(a, addSticky("x", 0, 0))
			h.ok(a, addConnector("c", attach("x", 1, 0.5), free(500, 0), map[string]int64{eid("x"): h.inc(eid("x"))}))
			h.ok(a, deleteOp(h, "c"))
			h.ok(a, deleteOp(h, "x"))
			undoAll(t, h, a, 2)
			if end := h.row(eid("c")).El.Ends[0]; end.El != eid("x") {
				t.Fatalf("connector end %+v", end)
			}
		})
		t.Run("a member a frame deletion released and its revive restored", func(t *testing.T) {
			h := newHarness(t)
			a := h.session("a")
			h.ok(a, addFrame("f", 0, 0, 500, 500), addSticky("x", 900, 0))
			h.ok(a, reparentOp(h, "f", "x"))
			h.ok(a, deleteOp(h, "f"))
			undoAll(t, h, a, 2)
			if h.row(eid("x")).El.Frame != "" {
				t.Fatal("undoing the reparent kept x in the frame")
			}
		})
		t.Run("redo of an edit and a deletion of one element", func(t *testing.T) {
			h := newHarness(t)
			a := h.session("a")
			h.ok(a, addSticky("x", 0, 0))
			h.ok(a, setText("x", 0, "edited", 0), deleteOp(h, "x"))
			undoAll(t, h, a, 1)
			h.ok(a, redoOp)
			if h.alive(eid("x")) {
				t.Fatal("redo kept x")
			}
		})
		t.Run("a reparent out of a frame the same batch deleted", func(t *testing.T) {
			h := newHarness(t)
			a := h.session("a")
			h.ok(a, addFrame("f", 0, 0, 500, 500), addFrame("g", 900, 0, 500, 500))
			h.ok(a, addIn("x", "f", h.inc(eid("f")), 10, 10))
			h.ok(a, reparentOp(h, "g", "x"), deleteOp(h, "f"))
			undoAll(t, h, a, 1)
			h.ok(a, redoOp)
			if h.row(eid("x")).El.Frame != eid("g") || h.alive(eid("f")) {
				t.Fatalf("after redo x is in %q", h.row(eid("x")).El.Frame)
			}
		})
		t.Run("a membership a batch changed and changed back", func(t *testing.T) {
			h := newHarness(t)
			a := h.session("a")
			h.ok(a, addSticky("x", 0, 0), addFrame("f", 0, 0, 500, 500))
			h.ok(a, reparentOp(h, "f", "x"), deleteOp(h, "f"))
			undoAll(t, h, a, 2)
			if h.alive(eid("x")) {
				t.Fatal("undoing the creation kept x")
			}
		})
	})

	t.Run("a group id is never reused", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("a", 0, 0), addSticky("b", 0, 0), addSticky("c", 0, 0), addSticky("d", 0, 0), addSticky("e", 0, 0), addSticky("f", 0, 0))
		h.ok(a, groupOp(h, "g", "a", "b"))
		stale := Op{Op: OpUngroup, Group: "g_g000000000"}
		h.ok(a, stale)
		if rs := results(t, h.ok(a, groupOp(h, "g", "c", "d"))); rs[0].(map[string]any)["ok"] != nil {
			t.Fatalf("regrouping with an ungrouped id = %v", rs)
		}
		// after the deleted members' rows are purged
		h.ok(a, groupOp(h, "h", "c", "d"))
		h.ok(a, deleteOp(h, "c", "d"))
		h.s.PurgeDeleted([]string{eid("c"), eid("d")})
		if rs := results(t, h.ok(a, groupOp(h, "h", "e", "f"))); rs[0].(map[string]any)["ok"] != nil {
			t.Fatalf("regrouping with a purged group's id = %v", rs)
		}
		// after a reload
		s, err := Load(h.s.Export())
		if err != nil {
			t.Fatal(err)
		}
		h.s = s
		if rs := results(t, h.ok(a, groupOp(h, "g", "e", "f"))); rs[0].(map[string]any)["ok"] != nil {
			t.Fatalf("regrouping after a reload = %v", rs)
		}
		if _, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "group", IDs: []string{eid("e"), eid("f")}, Group: "g_h000000000"}}}); err == nil {
			t.Fatal("an agent reused a group id")
		}
		// a stale ungroup never reaches a new group
		h.ok(a, groupOp(h, "n", "e", "f"))
		h.ok(a, stale)
		if h.row(eid("e")).El.Group != "g_n000000000" {
			t.Fatal("a stale ungroup changed another group")
		}
	})

	t.Run("delete_frame_contents checks if_ver and refuses the whole call", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addFrame("f", 0, 0, 500, 500), addSticky("x", 900, 0))
		h.ok(a, addIn("m", "f", h.inc(eid("f")), 10, 10))
		ver, _ := h.s.Ver(eid("f"))
		h.ok(a, move(h, "f", 50, 0, CarryMembers))
		_, err := h.agent(AgentCall{Ops: []AgentOp{
			{Op: "set", ID: eid("x"), Fields: raw(map[string]any{"text": "earlier op"})},
			{Op: "delete_frame_contents", Frame: raw(eid("f")), IfVer: raw(ver)},
		}})
		var ae *AgentError
		if !errors.As(err, &ae) || ae.Category != CategoryConflict {
			t.Fatalf("stale guard = %v", err)
		}
		if !h.alive(eid("f")) || !h.alive(eid("m")) || h.row(eid("x")).El.Text != "" {
			t.Fatal("a refused call changed the board")
		}
		ver, _ = h.s.Ver(eid("f"))
		if _, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "delete_frame_contents", Frame: raw(eid("f")), IfVer: raw(ver)}}}); err != nil || h.alive(eid("m")) {
			t.Fatalf("current guard = %v", err)
		}
		// ungroup names its guarded members
		h.ok(a, addSticky("p", 0, 0), addSticky("q", 0, 0))
		h.ok(a, groupOp(h, "g", "p", "q"))
		pv, _ := h.s.Ver(eid("p"))
		h.ok(a, move(h, "p", 5, 5, ""))
		if _, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "ungroup", Group: "g_g000000000", IfVer: raw(map[string]int64{eid("p"): pv})}}}); !errors.As(err, &ae) || h.row(eid("p")).El.Group == "" {
			t.Fatalf("stale ungroup guard = %v", err)
		}
	})

	t.Run("layout places a frame and its member once and reports the result", func(t *testing.T) {
		for _, c := range []struct {
			name          string
			ids           []string
			frame, member [2]float64
		}{
			{"member first", []string{"m", "f"}, [2]float64{224, 0}, [2]float64{0, 0}},
			{"frame first", []string{"f", "m"}, [2]float64{0, 0}, [2]float64{824, 0}},
		} {
			t.Run(c.name, func(t *testing.T) {
				h := newHarness(t)
				a := h.session("a")
				h.ok(a, addFrame("f", 0, 0, 800, 600))
				h.ok(a, Op{Op: OpAdd, El: elOf(map[string]any{"id": eid("m"), "type": "sticky", "x": 100, "y": 100, "w": 200, "h": 200, "frame": eid("f")}),
					Refs: map[string]int64{eid("f"): h.inc(eid("f"))}}, addIn("o", "f", h.inc(eid("f")), 300, 300))
				var ids []string
				for _, n := range c.ids {
					ids = append(ids, eid(n))
				}
				out, err := h.agent(AgentCall{Layout: []Layout{{Kind: "row", IDs: ids}}})
				if err != nil {
					t.Fatal(err)
				}
				for id, want := range map[string][2]float64{eid("f"): c.frame, eid("m"): c.member} {
					g := h.geom(id)
					if got := [2]float64{g.X, g.Y}; got != want || out.Layout[id] != got {
						t.Fatalf("%s at %v, reported %v, want %v", id, got, out.Layout[id], want)
					}
				}
				// a member the layout does not place still follows its frame
				if g := h.geom(eid("o")); g.X != 300+c.frame[0] || g.Y != 300 {
					t.Fatalf("unplaced member at %v", g)
				}
			})
		}
	})

	t.Run("replacement checks pen points of reused ids", func(t *testing.T) {
		long := make([]int, 2*(model.MaxPenPoints+1))
		pen := func(name string, pts []int) model.Element {
			return model.Element{ID: eid(name), Type: model.TypePen, Z: "V", Geom: model.Geom{W: 10, H: 10}, Pts: pts, Style: model.Style{"stroke": "ink", "width": "pen"}}
		}
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, Op{Op: OpAdd, El: elOf(map[string]any{"id": eid("p"), "type": "pen", "x": 0, "y": 0, "w": 10, "h": 10, "pts": []int{0, 0, 1, 1}})}, addSticky("s", 0, 0))
		for name, next := range map[string]model.Element{"pen over pen": pen("p", long), "pen over sticky": pen("s", long)} {
			c := content(h.s)
			for i, el := range c.Elements {
				if el.ID == next.ID {
					c.Elements[i] = next
				}
			}
			if _, err := h.s.Replace(c, "agent:import", h.now); !errors.Is(err, ErrLimit) {
				t.Errorf("%s: replace = %v", name, err)
			}
			if _, err := h.agent(AgentCall{Replace: true, Ops: []AgentOp{
				{Op: "add", El: raw(next)},
			}}); err == nil {
				t.Errorf("%s: replacing agent call applied", name)
			}
		}
		if p := h.row(eid("p")); p.El.PenPoints() != 2 || h.row(eid("s")).El.Type != model.TypeSticky {
			t.Fatal("a refused replacement changed the board")
		}
		// a stroke already over a lowered limit may stay as it is
		h.s.SetLimits(model.Limits{Elements: 10, Text: 10, Label: 10, Alt: 10, PenPoints: 1, Assets: 1})
		c := content(h.s)
		c.Title = "renamed"
		if _, err := h.s.Replace(c, "agent:import", h.now); err != nil {
			t.Fatalf("replace keeping an over-limit stroke = %v", err)
		}
	})

	t.Run("image alternative text has a limit", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		hash := strings.Repeat("ab", 32)
		h.s.HoldAsset(Asset{Hash: hash, Mime: "image/png", W: 4, H: 3, Bytes: 99}, h.now+3600)
		img := func(name, alt string) Op {
			return Op{Op: OpAdd, El: elOf(map[string]any{"id": eid(name), "type": "image", "x": 0, "y": 0, "w": 4, "h": 3, "asset": hash, "natural": []int{4, 3}, "alt": alt})}
		}
		long := strings.Repeat("é", model.MaxAlt+1)
		if out := h.do(a, img("i", long)); out.Reject == nil || out.Reject.Code != CodeLimit {
			t.Fatalf("add with long alt = %+v", out)
		}
		h.ok(a, img("i", "fine"))
		if out := h.do(a, Op{Op: OpSet, ID: eid("i"), Inc: h.inc(eid("i")), G: model.BundleAlt, Val: raw(long)}); out.Reject == nil || out.Reject.Code != CodeLimit {
			t.Fatalf("set long alt = %+v", out)
		}
		if _, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "set", ID: eid("i"), Fields: raw(map[string]any{"alt": long})}}}); err == nil {
			t.Fatal("an agent set a long alt")
		}
		if _, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "add", El: raw(map[string]any{"type": "image", "asset": hash, "natural": []int{4, 3}, "alt": long})}}}); err == nil {
			t.Fatal("an agent added a long alt")
		}
		c := content(h.s)
		c.Elements[0].Alt = long
		if _, err := h.s.Replace(c, "agent:import", h.now); !errors.Is(err, ErrLimit) {
			t.Fatalf("replace with a long alt = %v", err)
		}
		if h.row(eid("i")).El.Alt != "fine" {
			t.Fatal("a refused change kept its alt")
		}
	})

	t.Run("summarized results stay within the cap with escaped ids", func(t *testing.T) {
		for name, ids := range map[string][]string{
			"one long id": {strings.Repeat("<", 60000)},
			"many ids": func() []string {
				var ids []string
				for i := range 6000 {
					ids = append(ids, fmt.Sprintf("<&>%06d<&>", i))
				}
				return ids
			}(),
		} {
			h := newHarness(t)
			a := h.session("a")
			out := h.do(a, Op{Op: OpDelete, IDs: ids, IncOf: map[string]int64{}})
			if len(out.Results) > model.MaxResultsBytes {
				t.Errorf("%s: results are %d bytes", name, len(out.Results))
			}
			if !json.Valid(out.Results) {
				t.Errorf("%s: results are not JSON", name)
			}
		}
	})
}

// TestReviewRound2Counterexamples covers undo cleanup against a stale
// membership guard, null coordinates and agent connector ends.
func TestReviewRound2Counterexamples(t *testing.T) {
	t.Run("undo cleanup does not pass a stale membership guard", func(t *testing.T) {
		setup := func(t *testing.T) (*harness, string, string) {
			h := newHarness(t)
			a, b := h.session("a"), h.session("b")
			h.ok(a, addSticky("x", 900, 0))
			// One batch creates frame f and reparents x into it.
			_, m := incOf(h, "x")
			h.ok(a, addFrame("f", 0, 0, 500, 500), Op{Op: OpReparent, IDs: []string{eid("x")}, IncOf: m, Frame: raw(eid("f")), FrameInc: h.s.Seq() + 1})
			if h.row(eid("x")).El.Frame != eid("f") {
				t.Fatal("x is not in f")
			}
			// b takes x out and puts it back.
			h.ok(b, reparentOp(h, "", "x"))
			h.ok(b, reparentOp(h, "f", "x"))
			return h, a, b
		}
		undoFrame := func(t *testing.T, h *harness, a string) {
			t.Helper()
			// Deleting f releases x as a side effect; the membership
			// record is stale because b changed it after a's batch.
			if u := undoResult(t, h.ok(a, undoOp)); u.Undone != 1 || u.Skipped[VoidStale] != 1 {
				t.Fatalf("undo of the frame batch %+v", u)
			}
			if h.alive(eid("f")) || h.row(eid("x")).El.Frame != "" {
				t.Fatal("undo kept f")
			}
		}
		t.Run("creation undo keeps x", func(t *testing.T) {
			h, a, _ := setup(t)
			undoFrame(t, h, a)
			if u := undoResult(t, h.ok(a, undoOp)); u.Undone != 0 || u.Skipped[VoidStale] != 1 {
				t.Fatalf("undo of x's creation %+v", u)
			}
			if !h.alive(eid("x")) {
				t.Fatal("undo deleted x, which b changed after its creation")
			}
		})
		t.Run("after redo and undo again", func(t *testing.T) {
			h, a, _ := setup(t)
			undoFrame(t, h, a)
			// Redo revives f and restores what the undo changed: x is
			// back in f, as it was before the undo.
			if u := undoResult(t, h.ok(a, redoOp)); u.Undone == 0 {
				t.Fatalf("redo %+v", u)
			}
			if !h.alive(eid("f")) || h.row(eid("x")).El.Frame != eid("f") {
				t.Fatalf("after redo f alive %v, x in %q", h.alive(eid("f")), h.row(eid("x")).El.Frame)
			}
			// Undoing the redo deletes f again and releases x again; this
			// time the membership record is the redo's own, so it is not
			// stale.
			if u := undoResult(t, h.ok(a, undoOp)); u.Undone == 0 || u.Skipped[VoidStale] != 0 {
				t.Fatalf("undo after redo %+v", u)
			}
			if h.alive(eid("f")) || h.row(eid("x")).El.Frame != "" {
				t.Fatal("undo after redo kept f")
			}
			if u := undoResult(t, h.ok(a, undoOp)); u.Undone != 0 || u.Skipped[VoidStale] != 1 || !h.alive(eid("x")) {
				t.Fatalf("undo of x's creation after redo %+v, x alive %v", u, h.alive(eid("x")))
			}
		})
	})

	t.Run("null coordinates are refused with the whole batch or call", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("s", 0, 0))
		h.ok(a, addConnector("c", attach("s", 1, 0.5), free(500, 0), map[string]int64{eid("s"): h.inc(eid("s"))}))
		want := h.row(eid("c")).El.Ends
		unchanged := func(what string) {
			t.Helper()
			if h.row(eid("s")).El.Text != "" || h.row(eid("c")).El.Ends != want {
				t.Fatalf("%s changed the board: text %q, ends %+v", what, h.row(eid("s")).El.Text, h.row(eid("c")).El.Ends)
			}
		}
		for name, end := range map[string]string{
			"null in pt": `{"pt":[10,null]}`,
			"null in at": `{"el":"` + eid("s") + `","at":[null,0.5]}`,
		} {
			out := h.do(a, setText("s", h.inc(eid("s")), "earlier op", 0),
				Op{Op: OpSet, ID: eid("c"), Inc: h.inc(eid("c")), G: model.BundleEnd1, Val: json.RawMessage(end)})
			if out.Reject == nil || out.Reject.Code != CodeInvalid {
				t.Fatalf("person %s = %+v", name, out)
			}
			unchanged("person " + name)
			if _, err := h.agent(AgentCall{Ops: []AgentOp{
				{Op: "set", ID: eid("s"), Fields: raw(map[string]any{"text": "earlier op"})},
				{Op: "set", ID: eid("c"), Fields: json.RawMessage(`{"ends":[null,` + end + `]}`)},
			}}); err == nil {
				t.Fatalf("agent set %s applied", name)
			}
			unchanged("agent set " + name)
			if _, err := h.agent(AgentCall{Ops: []AgentOp{
				{Op: "set", ID: eid("s"), Fields: raw(map[string]any{"text": "earlier op"})},
				{Op: "add", El: json.RawMessage(`{"type":"connector","ends":[{"pt":[0,0]},` + end + `]}`)},
			}}); err == nil {
				t.Fatalf("agent add %s applied", name)
			}
			unchanged("agent add " + name)
			if len(h.s.Alive()) != 2 {
				t.Fatalf("agent add %s left %d elements", name, len(h.s.Alive()))
			}
		}
	})

	t.Run("agent connectors need their ends", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("s", 0, 0))
		before := sortedIDs(h.s)
		for name, el := range map[string]string{
			"no ends":   `{"type":"connector"}`,
			"null ends": `{"type":"connector","ends":null}`,
		} {
			var ae *AgentError
			_, err := h.agent(AgentCall{Ops: []AgentOp{
				{Op: "add", El: raw(map[string]any{"type": "sticky"})},
				{Op: "add", El: json.RawMessage(el)},
			}})
			if !errors.As(err, &ae) || ae.Category != CategoryInvalid {
				t.Fatalf("add %s = %v", name, err)
			}
			if got := sortedIDs(h.s); !slices.Equal(got, before) {
				t.Fatalf("add %s changed the board: %v", name, got)
			}
			_, err = h.agent(AgentCall{Replace: true, Ops: []AgentOp{
				{Op: "add", El: raw(map[string]any{"type": "sticky"})},
				{Op: "add", El: json.RawMessage(el)},
			}})
			if !errors.As(err, &ae) || ae.Category != CategoryInvalid {
				t.Fatalf("replacing add %s = %v", name, err)
			}
			if got := sortedIDs(h.s); !slices.Equal(got, before) {
				t.Fatalf("replacing add %s changed the board: %v", name, got)
			}
		}
		// an attached end may still omit "at" (8.1); null reads as omitted
		out, err := h.agent(AgentCall{Ops: []AgentOp{
			{Op: "add", El: json.RawMessage(`{"type":"connector","ends":[{"el":"` + eid("s") + `","at":null},{"pt":[900,50]}]}`)},
		}})
		if err != nil {
			t.Fatal(err)
		}
		id := out.Results[0].(string)
		if at := h.row(id).El.Ends[0].At; at != model.DefaultAt(h.geom(eid("s")), [2]float64{900, 50}) {
			t.Fatalf("omitted at placed at %v", at)
		}
	})
}

// TestReviewRound3Counterexamples covers replacements: text limits by each
// state's type, layout and placement of new elements, and empty attachment
// targets.
func TestReviewRound3Counterexamples(t *testing.T) {
	t.Run("replacement measures text against each state's own type", func(t *testing.T) {
		long := strings.Repeat("é", 1000)
		next := func(typ, text string) map[string]any {
			el := map[string]any{"id": eid("x"), "type": typ, "text": text, "z": "V"}
			if typ == model.TypeConnector {
				el["ends"] = []any{free(0, 0), free(100, 0)}
			} else {
				el["x"], el["y"], el["w"], el["h"] = 0, 0, 100, 100
			}
			return el
		}
		replaceWith := func(h *harness, el map[string]any) (error, error) {
			dec, _, err := model.DecodeElement(raw(el))
			if err != nil {
				t.Fatal(err)
			}
			c := content(h.s)
			for i := range c.Elements {
				if c.Elements[i].ID == dec.ID {
					c.Elements[i] = dec
				}
			}
			_, errReplace := h.s.Replace(c, "agent:import", h.now)
			_, errAgent := h.agent(AgentCall{Replace: true, Ops: []AgentOp{{Op: "add", El: raw(el)}}})
			return errReplace, errAgent
		}
		for _, from := range []string{model.TypeSticky, model.TypeText, model.TypeShape} {
			for _, to := range []string{model.TypeFrame, model.TypeConnector} {
				h := newHarness(t)
				a := h.session("a")
				h.ok(a, Op{Op: OpAdd, El: elOf(map[string]any{"id": eid("x"), "type": from, "x": 0, "y": 0, "w": 100, "h": 100, "text": long})})
				errReplace, errAgent := replaceWith(h, next(to, long))
				var ae *AgentError
				if !errors.Is(errReplace, ErrLimit) {
					t.Errorf("%s -> %s: replace = %v", from, to, errReplace)
				}
				if !errors.As(errAgent, &ae) || ae.Category != CategoryInvalid {
					t.Errorf("%s -> %s: replacing agent call = %v", from, to, errAgent)
				}
				if r := h.row(eid("x")); r.El.Type != from || r.El.Text != long {
					t.Errorf("%s -> %s: a refused replacement changed the board: %s", from, to, r.El.Type)
				}
				// a name within the frame and connector limit is fine
				if errReplace, _ := replaceWith(h, next(to, strings.Repeat("é", model.MaxLabel))); errReplace != nil {
					t.Errorf("%s -> %s within the limit: %v", from, to, errReplace)
				}
			}
		}
		// The other way, a frame already over a lowered name limit may
		// become a sticky whose text is less over the text limit.
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, Op{Op: OpAdd, El: elOf(map[string]any{"id": eid("x"), "type": "frame", "x": 0, "y": 0, "w": 100, "h": 100, "text": "12345678"})})
		h.s.SetLimits(model.Limits{Elements: 10, Text: 10, Label: 5, Alt: 10, PenPoints: 10, Assets: 1})
		if errReplace, errAgent := replaceWith(h, next(model.TypeSticky, "123456789012")); errReplace != nil || errAgent != nil {
			t.Fatalf("frame over its limit -> sticky less over its limit: %v, %v", errReplace, errAgent)
		}
	})

	t.Run("a replacing call places its new elements with layout", func(t *testing.T) {
		ops := []AgentOp{
			{Op: "add", Ref: "$f", El: elOf(map[string]any{"type": "frame", "text": "F", "x": 0, "y": 0, "w": 100, "h": 100})},
			{Op: "add", Ref: "$a", El: elOf(map[string]any{"type": "sticky", "x": 5000, "y": 5000})},
			{Op: "add", Ref: "$b", El: elOf(map[string]any{"type": "sticky", "x": -5000, "y": 0})},
			{Op: "add", Ref: "$c", El: elOf(map[string]any{"type": "connector", "ends": []any{map[string]any{"el": "$a"}, map[string]any{"el": "$b"}}})},
		}
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("old", 0, 0))
		before := sortedIDs(h.s)
		wide := float64(model.MaxSize)
		for name, l := range map[string]Layout{
			"unknown ref":       {Kind: "row", IDs: []string{"$a", "$zz"}},
			"old board element": {Kind: "row", IDs: []string{"$a", eid("old")}},
			"connector":         {Kind: "row", IDs: []string{"$a", "$c"}},
			"frame in a frame":  {Kind: "row", IDs: []string{"$f"}, In: "$f"},
			// fails after placing: the frame would grow past the maximum
			"frame too large": {Kind: "row", IDs: []string{"$a", "$b"}, In: "$f", Gap: &wide},
		} {
			if _, err := h.agent(AgentCall{Replace: true, Ops: ops, Layout: []Layout{l}}); err == nil {
				t.Fatalf("%s: a failing layout applied", name)
			}
			if got := sortedIDs(h.s); !slices.Equal(got, before) || h.geom(eid("old")).X != 0 {
				t.Fatalf("%s: a failed replacing call changed the board: %v", name, got)
			}
		}
		// layout names refs, so a replacing call declares each ref once
		dup := append(slices.Clone(ops), AgentOp{Op: "add", Ref: "$a", El: elOf(map[string]any{"type": "sticky"})})
		if _, err := h.agent(AgentCall{Replace: true, Ops: dup, Layout: []Layout{{Kind: "row", IDs: []string{"$a"}}}}); err == nil {
			t.Fatal("a replacing call declared $a twice")
		}
		if got := sortedIDs(h.s); !slices.Equal(got, before) {
			t.Fatalf("a refused replacing call changed the board: %v", got)
		}
		out, err := h.agent(AgentCall{Replace: true, Ops: ops, Layout: []Layout{{Kind: "row", IDs: []string{"$a", "$b"}, In: "$f"}}})
		if err != nil {
			t.Fatal(err)
		}
		if out.Delta.Reset == nil || out.Delta.Reset.Kind != ResetReplace || h.alive(eid("old")) {
			t.Fatalf("replace outcome %+v", out.Delta.Reset)
		}
		f, sa, sb, c := out.IDs["$f"], out.IDs["$a"], out.IDs["$b"], out.IDs["$c"]
		for id, want := range map[string][2]float64{sa: {32, 32}, sb: {32 + 200 + layoutGap, 32}} {
			g := h.geom(id)
			if got := [2]float64{g.X, g.Y}; got != want || out.Layout[id] != want {
				t.Fatalf("%s at %v, reported %v, want %v", id, got, out.Layout[id], want)
			}
			if h.row(id).El.Frame != f {
				t.Fatalf("%s is in %q, not the layout's frame", id, h.row(id).El.Frame)
			}
		}
		if g := h.geom(f); g.W != 32+200+layoutGap+200+32 || g.H != 32+200+32 {
			t.Fatalf("frame did not grow to fit: %+v", g)
		}
		// default sides follow the committed places: $a is left of $b
		if e := h.row(c).El.Ends; e[0].At != [2]float64{1, 0.5} || e[1].At != [2]float64{0, 0.5} {
			t.Fatalf("connector ends %+v", e)
		}
	})

	t.Run("an empty attachment target is refused with the whole batch or call", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("s", 0, 0))
		h.ok(a, addConnector("c", attach("s", 1, 0.5), free(500, 0), map[string]int64{eid("s"): h.inc(eid("s"))}))
		want := h.row(eid("c")).El.Ends
		before := sortedIDs(h.s)
		unchanged := func(what string) {
			t.Helper()
			if h.row(eid("s")).El.Text != "" || h.row(eid("c")).El.Ends != want || !slices.Equal(sortedIDs(h.s), before) {
				t.Fatalf("%s changed the board: text %q, ends %+v", what, h.row(eid("s")).El.Text, h.row(eid("c")).El.Ends)
			}
		}
		end := `{"el":"","at":[1,0.5]}`
		out := h.do(a, setText("s", h.inc(eid("s")), "earlier op", 0),
			Op{Op: OpSet, ID: eid("c"), Inc: h.inc(eid("c")), G: model.BundleEnd1, Val: json.RawMessage(end)})
		if out.Reject == nil || out.Reject.Code != CodeInvalid {
			t.Fatalf("person set = %+v", out)
		}
		unchanged("person set")
		out = h.do(a, setText("s", h.inc(eid("s")), "earlier op", 0),
			Op{Op: OpAdd, El: json.RawMessage(`{"id":"` + eid("d") + `","type":"connector","ends":[{"pt":[0,0]},` + end + `]}`)})
		if out.Reject == nil || out.Reject.Code != CodeInvalid {
			t.Fatalf("person add = %+v", out)
		}
		unchanged("person add")
		for name, call := range map[string]AgentCall{
			"agent set": {Ops: []AgentOp{
				{Op: "set", ID: eid("s"), Fields: raw(map[string]any{"text": "earlier op"})},
				{Op: "set", ID: eid("c"), Fields: json.RawMessage(`{"ends":[null,` + end + `]}`)},
			}},
			"agent add": {Ops: []AgentOp{
				{Op: "set", ID: eid("s"), Fields: raw(map[string]any{"text": "earlier op"})},
				{Op: "add", El: json.RawMessage(`{"type":"connector","ends":[{"pt":[0,0]},` + end + `]}`)},
			}},
			"replacing add": {Replace: true, Ops: []AgentOp{
				{Op: "add", El: raw(map[string]any{"type": "sticky"})},
				{Op: "add", El: json.RawMessage(`{"type":"connector","ends":[{"pt":[0,0]},` + end + `]}`)},
			}},
		} {
			var ae *AgentError
			if _, err := h.agent(call); !errors.As(err, &ae) || ae.Category != CategoryInvalid {
				t.Fatalf("%s = %v", name, err)
			}
			unchanged(name)
		}
	})

	t.Run("a replacing call places unplaced elements by its own content", func(t *testing.T) {
		at := func(h *harness, id string) [2]float64 { g := h.geom(id); return [2]float64{g.X, g.Y} }
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("far", 50000, 50000))
		out, err := h.agent(AgentCall{Replace: true, Ops: []AgentOp{
			{Op: "add", Ref: "$u1", El: elOf(map[string]any{"type": "sticky"})},
			{Op: "add", Ref: "$p", El: elOf(map[string]any{"type": "sticky", "x": 100, "y": 200})},
			{Op: "add", Ref: "$u2", El: elOf(map[string]any{"type": "shape"})},
			{Op: "add", El: elOf(map[string]any{"type": "connector", "ends": []any{map[string]any{"el": "$u1"}, map[string]any{"el": "$u2"}}})},
		}})
		if err != nil {
			t.Fatal(err)
		}
		// a row to the right of the call's placed sticky (100..300), not of
		// the replaced board at 50000
		first := 100 + 200 + float64(placeGap)
		for ref, want := range map[string][2]float64{"$p": {100, 200}, "$u1": {first, 200}, "$u2": {first + 200 + layoutGap, 200}} {
			if got := at(h, out.IDs[ref]); got != want {
				t.Fatalf("%s at %v, want %v", ref, got, want)
			}
		}
		// nothing placed: the empty-board default
		out, err = h.agent(AgentCall{Replace: true, Ops: []AgentOp{
			{Op: "add", Ref: "$a", El: elOf(map[string]any{"type": "sticky"})},
			{Op: "add", Ref: "$b", El: elOf(map[string]any{"type": "sticky"})},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if a, b := at(h, out.IDs["$a"]), at(h, out.IDs["$b"]); a != [2]float64{0, 0} || b != [2]float64{200 + layoutGap, 0} {
			t.Fatalf("empty-board default placed %v and %v", a, b)
		}
	})
}

// TestReviewRound4Counterexamples covers layout of new elements near the
// edge of the canvas and resets that change only author fields.
func TestReviewRound4Counterexamples(t *testing.T) {
	t.Run("layout places new elements the row would put off the canvas", func(t *testing.T) {
		far := float64(model.MaxCoord - 100)
		at := func(h *harness, id string) [2]float64 { g := h.geom(id); return [2]float64{g.X, g.Y} }
		var ae *AgentError
		// The row to the right of P starts past MaxCoord; layout puts S in F.
		t.Run("ordinary", func(t *testing.T) {
			h := newHarness(t)
			a := h.session("a")
			h.ok(a, addFrame("f", 0, 0, 1000, 1000), addSticky("p", far, 0))
			out, err := h.agent(AgentCall{
				Ops:    []AgentOp{{Op: "add", Ref: "$s", El: elOf(map[string]any{"type": "sticky"})}},
				Layout: []Layout{{Kind: "row", IDs: []string{"$s"}, In: eid("f")}},
			})
			if err != nil {
				t.Fatal(err)
			}
			s := out.IDs["$s"]
			if got := at(h, s); got != [2]float64{32, 32} || out.Layout[s] != got || h.row(s).El.Frame != eid("f") {
				t.Fatalf("S at %v (reported %v) in %q", got, out.Layout[s], h.row(s).El.Frame)
			}
			if g := h.geom(eid("f")); g.W != 1000 || g.H != 1000 {
				t.Fatalf("frame changed size: %+v", g)
			}
			// an element without coordinates that no layout places still
			// goes in the row and is refused off the canvas, with the call
			before := sortedIDs(h.s)
			_, err = h.agent(AgentCall{
				Ops: []AgentOp{
					{Op: "add", Ref: "$s", El: elOf(map[string]any{"type": "sticky"})},
					{Op: "add", Ref: "$t", El: elOf(map[string]any{"type": "sticky"})},
				},
				Layout: []Layout{{Kind: "row", IDs: []string{"$s"}, In: eid("f")}},
			})
			if !errors.As(err, &ae) || ae.Category != CategoryInvalid || !slices.Equal(sortedIDs(h.s), before) {
				t.Fatalf("unplaced element off the canvas: %v, board %v", err, sortedIDs(h.s))
			}
			// a final place off the canvas refuses the whole call
			_, err = h.agent(AgentCall{
				Ops: []AgentOp{
					{Op: "add", Ref: "$s", El: elOf(map[string]any{"type": "sticky"})},
					{Op: "add", Ref: "$u", El: elOf(map[string]any{"type": "sticky"})},
				},
				Layout: []Layout{{Kind: "row", IDs: []string{"$s", "$u"}}},
			})
			if !errors.As(err, &ae) || ae.Category != CategoryInvalid || !slices.Equal(sortedIDs(h.s), before) {
				t.Fatalf("layout off the canvas: %v, board %v", err, sortedIDs(h.s))
			}
		})
		t.Run("replace", func(t *testing.T) {
			h := newHarness(t)
			a := h.session("a")
			h.ok(a, addSticky("old", 0, 0))
			ops := []AgentOp{
				{Op: "add", Ref: "$f", El: elOf(map[string]any{"type": "frame", "text": "F", "x": 0, "y": 0, "w": 1000, "h": 1000})},
				{Op: "add", Ref: "$p", El: elOf(map[string]any{"type": "sticky", "x": far, "y": 0, "w": 100, "h": 100})},
				{Op: "add", Ref: "$s", El: elOf(map[string]any{"type": "sticky"})},
			}
			before := sortedIDs(h.s)
			off := append(slices.Clone(ops), AgentOp{Op: "add", Ref: "$u", El: elOf(map[string]any{"type": "sticky"})})
			for name, call := range map[string]AgentCall{
				"unplaced":   {Replace: true, Ops: off, Layout: []Layout{{Kind: "row", IDs: []string{"$s"}, In: "$f"}}},
				"off canvas": {Replace: true, Ops: off, Layout: []Layout{{Kind: "row", IDs: []string{"$s", "$u"}}}},
			} {
				if _, err := h.agent(call); !errors.As(err, &ae) || ae.Category != CategoryInvalid || !slices.Equal(sortedIDs(h.s), before) {
					t.Fatalf("%s: %v, board %v", name, err, sortedIDs(h.s))
				}
			}
			out, err := h.agent(AgentCall{Replace: true, Ops: ops, Layout: []Layout{{Kind: "row", IDs: []string{"$s"}, In: "$f"}}})
			if err != nil {
				t.Fatal(err)
			}
			s, f := out.IDs["$s"], out.IDs["$f"]
			if got := at(h, s); got != [2]float64{32, 32} || out.Layout[s] != got || h.row(s).El.Frame != f || h.alive(eid("old")) {
				t.Fatalf("S at %v (reported %v) in %q", got, out.Layout[s], h.row(s).El.Frame)
			}
		})
		// An element layout places takes no place in the row.
		t.Run("row", func(t *testing.T) {
			h := newHarness(t)
			a := h.session("a")
			h.ok(a, addFrame("f", 0, 0, 1000, 1000))
			out, err := h.agent(AgentCall{
				Ops: []AgentOp{
					{Op: "add", Ref: "$s", El: elOf(map[string]any{"type": "sticky"})},
					{Op: "add", Ref: "$t", El: elOf(map[string]any{"type": "sticky"})},
				},
				Layout: []Layout{{Kind: "row", IDs: []string{"$s"}, In: eid("f")}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := at(h, out.IDs["$t"]); got != [2]float64{1000 + placeGap, 0} {
				t.Fatalf("T at %v", got)
			}
		})
	})

	t.Run("a reset that changes only author fields reaches deltas and buckets", func(t *testing.T) {
		// check applies the delta to a copy of the board before it, as a
		// connected Private editor does, and compares it with a fresh read.
		check := func(t *testing.T, h *harness, before []model.PrivateElement, board model.PrivateElement, d *model.Delta, changed Changed, id string) {
			t.Helper()
			c := &simClient{name: "editor", copy: map[string]model.PrivateElement{}, deleted: map[string]int64{}, board: board}
			for _, e := range before {
				c.copy[e.ID] = e
			}
			if err := c.applyDelta(d); err != nil {
				t.Fatal(err)
			}
			c.watermark = d.Seq
			if err := c.matches(h.s); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(changed.Buckets, model.BucketOf(id)) || !slices.Contains(changed.Elements, id) {
				t.Fatalf("changed %+v misses %s", changed, id)
			}
		}
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("x", 0, 0))
		x := h.row(eid("x")).El
		rev := content(h.s)
		inc := h.inc(eid("x"))

		// same-id replacement by an agent later: same bundles, new by and at
		h.now += 100
		before, board := h.s.Alive(), h.s.Board()
		same := elOf(map[string]any{"id": eid("x"), "type": "sticky", "x": 0, "y": 0, "w": 100, "h": 100, "z": "V"})
		out, err := h.agent(AgentCall{Replace: true, Ops: []AgentOp{{Op: "add", El: same}}})
		if err != nil {
			t.Fatal(err)
		}
		r := h.row(eid("x"))
		if r.Inc != inc || r.El.By == x.By || r.El.At == x.At {
			t.Fatalf("replacement kept inc %d (was %d), author %s %d", r.Inc, inc, r.El.By, r.El.At)
		}
		check(t, h, before, board, out.Delta, out.Changed, eid("x"))

		// rollback brings back the revision's author fields only
		h.now += 100
		before, board = h.s.Alive(), h.s.Board()
		ro, err := h.s.Rollback(1, rev, h.now)
		if err != nil {
			t.Fatal(err)
		}
		if r := h.row(eid("x")); r.Inc != inc || r.El.By != x.By || r.El.At != x.At {
			t.Fatalf("rollback left inc %d, author %s %d", r.Inc, r.El.By, r.El.At)
		}
		check(t, h, before, board, ro.Delta, ro.Changed, eid("x"))
	})
}

// checkWhole applies a delta to a copy of the board before it, as a
// connected Private editor does, compares it with a fresh read and
// checks that id's row and bucket count as changed.
func checkWhole(t *testing.T, h *harness, before []model.PrivateElement, board model.PrivateElement, d *model.Delta, changed Changed, id string) {
	t.Helper()
	c := &simClient{name: "editor", copy: map[string]model.PrivateElement{}, deleted: map[string]int64{}, board: board}
	for _, e := range before {
		c.copy[e.ID] = e
	}
	if err := c.applyDelta(d); err != nil {
		t.Fatal(err)
	}
	c.watermark = d.Seq
	if err := c.matches(h.s); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(changed.Buckets, model.BucketOf(id)) || !slices.Contains(changed.Elements, id) {
		t.Fatalf("changed %+v misses %s", changed, id)
	}
	if err := CheckContent(content(h.s).Elements); err != nil {
		t.Fatal(err)
	}
}

// pendingMove applies a move of id prepared against incarnation inc
// before a reset; it must apply on top of the reset.
func pendingMove(t *testing.T, h *harness, session, name string, inc int64, x float64) {
	t.Helper()
	g := h.geom(eid(name))
	g.X = x
	out := h.ok(session, setGeom(name, inc, g, ""))
	if rs := results(t, out); rs[0] != ResultOK || h.geom(eid(name)).X != x {
		t.Fatalf("pending move gave %v, x %v", rs, h.geom(eid(name)).X)
	}
}

func TestReviewRound5Counterexamples(t *testing.T) {
	penOf := func(name string, pts []int) model.Element {
		el, _, err := model.DecodeElement(elOf(map[string]any{"id": eid(name), "type": "pen", "x": 0, "y": 0, "w": 100, "h": 100, "z": "V", "pts": pts}))
		if err != nil {
			t.Fatal(err)
		}
		return el
	}

	t.Run("same-type pen replacement and rollback keep the incarnation", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, Op{Op: OpAdd, El: elOf(map[string]any{"id": eid("p"), "type": "pen", "x": 0, "y": 0, "w": 100, "h": 100, "z": "V", "pts": []int{0, 0, 10000, 10000}})})
		rev := content(h.s)
		inc := h.inc(eid("p"))

		// State.Replace with other points under the same id and type
		h.now += 100
		before, board := h.s.Alive(), h.s.Board()
		c := content(h.s)
		for i := range c.Elements {
			c.Elements[i].Pts = []int{5000, 0, 0, 10000, 10000, 5000}
		}
		out, err := h.s.Replace(c, "import", h.now)
		if err != nil {
			t.Fatal(err)
		}
		if r := h.row(eid("p")); r.Inc != inc || !slices.Equal(r.El.Pts, []int{5000, 0, 0, 10000, 10000, 5000}) {
			t.Fatalf("replacement gave inc %d (was %d), pts %v", r.Inc, inc, r.El.Pts)
		}
		checkWhole(t, h, before, board, out.Delta, out.Changed, eid("p"))
		pendingMove(t, h, a, "p", inc, 40)

		// rollback to the revision with the first points
		h.now += 100
		before, board = h.s.Alive(), h.s.Board()
		ro, err := h.s.Rollback(1, rev, h.now)
		if err != nil {
			t.Fatal(err)
		}
		if r := h.row(eid("p")); r.Inc != inc || !slices.Equal(r.El.Pts, []int{0, 0, 10000, 10000}) {
			t.Fatalf("rollback gave inc %d (was %d), pts %v", r.Inc, inc, r.El.Pts)
		}
		checkWhole(t, h, before, board, ro.Delta, ro.Changed, eid("p"))
		pendingMove(t, h, a, "p", inc, 80)
	})

	t.Run("same-type image replacement by an agent and rollback keep the incarnation", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h1, h2 := strings.Repeat("1", 64), strings.Repeat("2", 64)
		h.s.HoldAsset(Asset{Hash: h1, Mime: "image/png", W: 4, H: 3, Bytes: 99}, h.now+3600)
		h.s.HoldAsset(Asset{Hash: h2, Mime: "image/png", W: 8, H: 6, Bytes: 199}, h.now+3600)
		h.ok(a, Op{Op: OpAdd, El: elOf(map[string]any{"id": eid("i"), "type": "image", "x": 0, "y": 0, "w": 40, "h": 30, "z": "V", "asset": h1, "natural": []int{4, 3}})})
		rev := content(h.s)
		inc := h.inc(eid("i"))

		h.now += 100
		before, board := h.s.Alive(), h.s.Board()
		img := elOf(map[string]any{"id": eid("i"), "type": "image", "x": 0, "y": 0, "w": 40, "h": 30, "z": "V", "asset": h2, "natural": []int{8, 6}})
		out, err := h.agent(AgentCall{Replace: true, Ops: []AgentOp{{Op: "add", El: img}}})
		if err != nil {
			t.Fatal(err)
		}
		if r := h.row(eid("i")); r.Inc != inc || r.El.Asset != h2 || r.El.Natural != [2]int{8, 6} {
			t.Fatalf("replacement gave inc %d (was %d), asset %s %v", r.Inc, inc, r.El.Asset, r.El.Natural)
		}
		if as := h.s.assets; as[h1].Refs != 0 || as[h2].Refs != 1 {
			t.Fatalf("asset refs %d/%d", as[h1].Refs, as[h2].Refs)
		}
		checkWhole(t, h, before, board, out.Delta, out.Changed, eid("i"))
		pendingMove(t, h, a, "i", inc, 40)

		h.now += 100
		before, board = h.s.Alive(), h.s.Board()
		ro, err := h.s.Rollback(1, rev, h.now)
		if err != nil {
			t.Fatal(err)
		}
		if r := h.row(eid("i")); r.Inc != inc || r.El.Asset != h1 || r.El.Natural != [2]int{4, 3} {
			t.Fatalf("rollback gave inc %d (was %d), asset %s %v", r.Inc, inc, r.El.Asset, r.El.Natural)
		}
		checkWhole(t, h, before, board, ro.Delta, ro.Changed, eid("i"))
		pendingMove(t, h, a, "i", inc, 80)
	})

	t.Run("a different type still starts a new incarnation", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("p", 0, 0))
		inc := h.inc(eid("p"))
		h.now += 100
		if _, err := h.s.Replace(Content{Title: "Board", Elements: []model.Element{penOf("p", []int{0, 0, 1, 1})}}, "import", h.now); err != nil {
			t.Fatal(err)
		}
		if r := h.row(eid("p")); r.Inc == inc || r.El.Type != model.TypePen {
			t.Fatalf("type change kept inc %d", r.Inc)
		}
		g := h.geom(eid("p"))
		out := h.ok(a, setGeom("p", inc, g, ""))
		if rs := results(t, out); rs[0] != void(VoidGone) {
			t.Fatalf("move of the old sticky gave %v", rs)
		}
	})

	t.Run("deleting a target at the edge of the range leaves a valid free end", func(t *testing.T) {
		max := float64(model.MaxCoord)
		cases := []struct {
			name   string
			target Op
			at     [2]float64
			want   [2]float64
		}{
			// sticky at (MaxCoord, 0), 100×100: its right side is past the range
			{"positive", addSticky("s", max, 0), [2]float64{1, 0.5}, [2]float64{max, 50}},
			// control: a shape at (-MaxCoord, -MaxCoord) rotated 180 puts
			// [1, 1] on its origin, in range, so nothing is clamped
			{"negative edge in range", addShape("s", -max, -max, 100, 100, 180), [2]float64{1, 1}, [2]float64{-max, -max}},
			// 1000×100 shape rotated 90 at y = -MaxCoord: its left side reaches y = -MaxCoord-450
			{"negative rotated 90", addShape("s", 0, -max, 1000, 100, 90), [2]float64{0, 0.5}, [2]float64{500, -max}},
			// rotated 45 near both positive limits
			{"rotated 45", addShape("s", max, max, 1000, 1000, 45), [2]float64{1, 1}, [2]float64{max, max}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				// delete: an ordinary delete of the target
				h := newHarness(t)
				a := h.session("a")
				h.ok(a, tc.target)
				h.ok(a, addConnector("c", attach("s", tc.at[0], tc.at[1]), free(0, 0), map[string]int64{eid("s"): h.inc(eid("s"))}))
				h.ok(a, deleteOp(h, "s"))
				if got := h.row(eid("c")).El.Ends[0]; got.El != "" || got.Pt != tc.want {
					t.Fatalf("free end %+v, want %v", got, tc.want)
				}
				if err := CheckContent(content(h.s).Elements); err != nil {
					t.Fatal(err)
				}

				// connector deleted first, then its target; the connector
				// alone comes back with a free end at its last coordinate
				h = newHarness(t)
				a = h.session("a")
				h.ok(a, tc.target)
				h.ok(a, addConnector("c", attach("s", tc.at[0], tc.at[1]), free(0, 0), map[string]int64{eid("s"): h.inc(eid("s"))}))
				h.ok(a, deleteOp(h, "c"))
				h.ok(a, deleteOp(h, "s"))
				h.ok(a, Op{Op: OpRevive, IDs: []string{eid("c")}, IfDel: h.row(eid("c")).DelSeq})
				if got := h.row(eid("c")).El.Ends[0]; got.El != "" || got.Pt != tc.want {
					t.Fatalf("revived free end %+v, want %v", got, tc.want)
				}
				if err := CheckContent(content(h.s).Elements); err != nil {
					t.Fatal(err)
				}

				// a rollback drops both; reviving the connector alone frees
				// its end at the clamped coordinate
				h = newHarness(t)
				a = h.session("a")
				empty := content(h.s)
				h.ok(a, tc.target)
				h.ok(a, addConnector("c", attach("s", tc.at[0], tc.at[1]), free(0, 0), map[string]int64{eid("s"): h.inc(eid("s"))}))
				h.now += 100
				ro, err := h.s.Rollback(1, empty, h.now)
				if err != nil {
					t.Fatal(err)
				}
				if err := checkInvariants(h.s); err != nil {
					t.Fatal(err)
				}
				h.ok(a, Op{Op: OpRevive, IDs: []string{eid("c")}, IfDel: ro.Seq})
				if got := h.row(eid("c")).El.Ends[0]; got.El != "" || got.Pt != tc.want {
					t.Fatalf("free end after rollback %+v, want %v", got, tc.want)
				}
				if err := CheckContent(content(h.s).Elements); err != nil {
					t.Fatal(err)
				}
			})
		}
	})

	t.Run("deleting frame contents at the edge of the range leaves a valid free end", func(t *testing.T) {
		max := float64(model.MaxCoord)
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addFrame("f", max-1000, max-1000, 1000, 1000))
		h.ok(a, addIn("s", "f", h.inc(eid("f")), max, max))
		h.ok(a, addConnector("c", attach("s", 1, 1), free(0, 0), map[string]int64{eid("s"): h.inc(eid("s"))}))
		h.ok(a, Op{Op: OpDeleteFrameContents, Frame: raw(eid("f")), Inc: h.inc(eid("f"))})
		if got := h.row(eid("c")).El.Ends[0]; got.El != "" || got.Pt != [2]float64{max, max} {
			t.Fatalf("free end %+v", got)
		}
		if err := CheckContent(content(h.s).Elements); err != nil {
			t.Fatal(err)
		}
	})
}

func TestReviewRound6Counterexamples(t *testing.T) {
	decode := func(t *testing.T, fields map[string]any) model.Element {
		t.Helper()
		el, _, err := model.DecodeElement(elOf(fields))
		if err != nil {
			t.Fatal(err)
		}
		return el
	}
	ver := func(t *testing.T, h *harness, id string) int64 {
		t.Helper()
		v, ok := h.s.Ver(id)
		if !ok {
			t.Fatalf("%s is not alive", id)
		}
		return v
	}
	// stale checks that a guard read before a reset refuses a deletion
	// after it and that a guard with the current ver still applies.
	stale := func(t *testing.T, h *harness, id string, read int64) {
		t.Helper()
		now := ver(t, h, id)
		if now <= read {
			t.Fatalf("ver of %s is %d after the reset, read %d", id, now, read)
		}
		_, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "delete", IDs: []string{id}, IfVer: raw(read)}}})
		if ae := (*AgentError)(nil); !errors.As(err, &ae) || ae.Category != CategoryConflict || !h.alive(id) {
			t.Fatalf("stale guard %d (now %d) gave %v, alive %v", read, now, err, h.alive(id))
		}
		g := h.geom(id)
		if _, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "set", ID: id, Fields: raw(map[string]any{"x": g.X + 1}), IfVer: raw(now)}}}); err != nil {
			t.Fatalf("current guard %d: %v", now, err)
		}
	}
	// stamped checks that the reset out stamped v.fixed of id with its seq
	// and left the incarnation and every bundle version as they were.
	stamped := func(t *testing.T, h *harness, id string, inc int64, v map[string]int64, out Outcome) {
		t.Helper()
		r := h.row(id)
		want := maps.Clone(v)
		want[model.VFixed] = out.Seq
		if r.Inc != inc || !maps.Equal(r.V, want) {
			t.Fatalf("reset left inc %d (was %d), v %v, want %v", r.Inc, inc, r.V, want)
		}
	}
	pen := func(name string, pts []int) map[string]any {
		return map[string]any{"id": eid(name), "type": "pen", "x": 0, "y": 0, "w": 100, "h": 100, "z": "V", "pts": pts}
	}

	t.Run("a replacement of pen points refuses a guard read before it", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, Op{Op: OpAdd, El: elOf(pen("p", []int{0, 0, 10000, 10000}))})
		inc, read, v := h.inc(eid("p")), ver(t, h, eid("p")), maps.Clone(h.row(eid("p")).V)

		// an unchanged replacement changes no ver
		h.now += 100
		if _, err := h.s.Replace(content(h.s), "import", h.now); err != nil {
			t.Fatal(err)
		}
		if got := ver(t, h, eid("p")); got != read || h.row(eid("p")).V[model.VFixed] != 0 {
			t.Fatalf("unchanged replacement gave ver %d (read %d), v %v", got, read, h.row(eid("p")).V)
		}

		h.now += 100
		before, board := h.s.Alive(), h.s.Board()
		c := content(h.s)
		c.Elements[0].Pts = []int{5000, 0, 0, 10000}
		out, err := h.s.Replace(c, "import", h.now)
		if err != nil {
			t.Fatal(err)
		}
		stamped(t, h, eid("p"), inc, v, out)
		checkWhole(t, h, before, board, out.Delta, out.Changed, eid("p"))
		pendingMove(t, h, a, "p", inc, 40)
		stale(t, h, eid("p"), read)
	})

	t.Run("a replacement of an image asset refuses a guard read before it", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h1, h2 := strings.Repeat("1", 64), strings.Repeat("2", 64)
		h.s.HoldAsset(Asset{Hash: h1, Mime: "image/png", W: 4, H: 3, Bytes: 99}, h.now+3600)
		h.s.HoldAsset(Asset{Hash: h2, Mime: "image/png", W: 8, H: 6, Bytes: 199}, h.now+3600)
		h.ok(a, Op{Op: OpAdd, El: elOf(map[string]any{"id": eid("i"), "type": "image", "x": 0, "y": 0, "w": 40, "h": 30, "z": "V", "asset": h1, "natural": []int{4, 3}})})
		inc, read, v := h.inc(eid("i")), ver(t, h, eid("i")), maps.Clone(h.row(eid("i")).V)

		h.now += 100
		before, board := h.s.Alive(), h.s.Board()
		c := content(h.s)
		c.Elements[0].Asset, c.Elements[0].Natural = h2, [2]int{8, 6}
		c.Assets[h2] = *h.s.assets[h2]
		out, err := h.s.Replace(c, "import", h.now)
		if err != nil {
			t.Fatal(err)
		}
		stamped(t, h, eid("i"), inc, v, out)
		checkWhole(t, h, before, board, out.Delta, out.Changed, eid("i"))
		pendingMove(t, h, a, "i", inc, 40)
		stale(t, h, eid("i"), read)
	})

	t.Run("a replacement of the author fields refuses a guard read before it", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("x", 0, 0))
		inc, read, v := h.inc(eid("x")), ver(t, h, eid("x")), maps.Clone(h.row(eid("x")).V)
		h.now += 100
		c := content(h.s)
		c.Elements[0].By, c.Elements[0].At = "import:other", 7
		out, err := h.s.Replace(c, "import", h.now)
		if err != nil {
			t.Fatal(err)
		}
		stamped(t, h, eid("x"), inc, v, out)
		pendingMove(t, h, a, "x", inc, 40)
		stale(t, h, eid("x"), read)
	})

	t.Run("an agent replacement of pen points refuses a guard read before it", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, Op{Op: OpAdd, El: elOf(pen("p", []int{0, 0, 10000, 10000}))})
		inc, read := h.inc(eid("p")), ver(t, h, eid("p"))
		h.now += 100
		before, board := h.s.Alive(), h.s.Board()
		out, err := h.agent(AgentCall{Replace: true, Ops: []AgentOp{{Op: "add", El: elOf(pen("p", []int{5000, 0, 0, 10000}))}}})
		if err != nil {
			t.Fatal(err)
		}
		if r := h.row(eid("p")); r.Inc != inc || r.V[model.VFixed] != out.Seq {
			t.Fatalf("agent replacement left inc %d (was %d), v %v", r.Inc, inc, r.V)
		}
		checkWhole(t, h, before, board, out.Delta, out.Changed, eid("p"))
		pendingMove(t, h, a, "p", inc, 40)
		stale(t, h, eid("p"), read)
	})

	t.Run("a rollback of pen points refuses a guard read before it", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, Op{Op: OpAdd, El: elOf(pen("p", []int{0, 0, 10000, 10000}))})
		rev := content(h.s)
		inc := h.inc(eid("p"))
		h.now += 100
		c := content(h.s)
		c.Elements[0].Pts = []int{5000, 0, 0, 10000}
		if _, err := h.s.Replace(c, "import", h.now); err != nil {
			t.Fatal(err)
		}
		read, v := ver(t, h, eid("p")), maps.Clone(h.row(eid("p")).V)

		h.now += 100
		before, board := h.s.Alive(), h.s.Board()
		ro, err := h.s.Rollback(1, rev, h.now)
		if err != nil {
			t.Fatal(err)
		}
		stamped(t, h, eid("p"), inc, v, ro)
		checkWhole(t, h, before, board, ro.Delta, ro.Changed, eid("p"))
		pendingMove(t, h, a, "p", inc, 40)
		stale(t, h, eid("p"), read)
	})

	// A connector C attached to shape S at [1, 0.5] (world (200, 150)) is
	// deleted by a reset whose content reuses S's id for another type. C's
	// end_last comes from S as it was, and reviving C alone frees the end
	// there.
	connector := func(t *testing.T, name string) map[string]any {
		return map[string]any{"id": eid(name), "type": "connector", "z": "W", "ends": []any{free(0, 0), free(10, 10)}}
	}
	sticky := func(t *testing.T, name string) map[string]any {
		return map[string]any{"id": eid(name), "type": "sticky", "x": 500, "y": 500, "w": 100, "h": 100, "z": "W"}
	}
	resets := []struct {
		name  string
		reuse func(t *testing.T, name string) map[string]any
		run   func(t *testing.T, h *harness, el map[string]any) int64
	}{
		{"replacement as a connector", connector, func(t *testing.T, h *harness, el map[string]any) int64 {
			out, err := h.s.Replace(Content{Title: "Board", Grid: true, Elements: []model.Element{decode(t, el)}}, "import", h.now)
			if err != nil {
				t.Fatal(err)
			}
			return out.Seq
		}},
		{"replacement as a sticky", sticky, func(t *testing.T, h *harness, el map[string]any) int64 {
			out, err := h.s.Replace(Content{Title: "Board", Grid: true, Elements: []model.Element{decode(t, el)}}, "import", h.now)
			if err != nil {
				t.Fatal(err)
			}
			return out.Seq
		}},
		{"agent replacement as a connector", connector, func(t *testing.T, h *harness, el map[string]any) int64 {
			delete(el, "z")
			out, err := h.agent(AgentCall{Replace: true, Ops: []AgentOp{{Op: "add", El: elOf(el)}}})
			if err != nil {
				t.Fatal(err)
			}
			return out.Seq
		}},
		{"rollback as a connector", connector, func(t *testing.T, h *harness, el map[string]any) int64 {
			out, err := h.s.Rollback(1, Content{Title: "Board", Grid: true, Elements: []model.Element{decode(t, el)}}, h.now)
			if err != nil {
				t.Fatal(err)
			}
			return out.Seq
		}},
		{"rollback as a sticky", sticky, func(t *testing.T, h *harness, el map[string]any) int64 {
			out, err := h.s.Rollback(1, Content{Title: "Board", Grid: true, Elements: []model.Element{decode(t, el)}}, h.now)
			if err != nil {
				t.Fatal(err)
			}
			return out.Seq
		}},
	}
	for _, tc := range resets {
		t.Run("a reset reusing a target id for "+tc.name+" keeps the deleted connector's end", func(t *testing.T) {
			h := newHarness(t)
			a := h.session("a")
			h.ok(a, addShape("s", 100, 100, 100, 100, 0))
			sInc := h.inc(eid("s"))
			h.ok(a, addConnector("c", attach("s", 1, 0.5), free(400, 400), map[string]int64{eid("s"): sInc}))
			h.now += 100
			seq := tc.run(t, h, tc.reuse(t, "s"))
			if err := checkInvariants(h.s); err != nil {
				t.Fatal(err)
			}
			if s := h.row(eid("s")); s.Inc == sInc || s.El.Type == model.TypeShape {
				t.Fatalf("target kept inc %d and type %s", s.Inc, s.El.Type)
			}
			c := h.row(eid("c"))
			if want := [2][2]float64{{200, 150}, {400, 400}}; !c.Deleted || c.DelSeq != seq || c.EndLast != want {
				t.Fatalf("connector deleted %v at %d (reset %d), end_last %v, want %v", c.Deleted, c.DelSeq, seq, c.EndLast, want)
			}
			out := h.ok(a, Op{Op: OpRevive, IDs: []string{eid("c")}, IfDel: seq})
			if !h.alive(eid("c")) {
				t.Fatalf("revive gave %v", results(t, out))
			}
			c = h.row(eid("c"))
			if c.El.Ends[0] != (model.End{Pt: [2]float64{200, 150}}) || c.El.Ends[1] != (model.End{Pt: [2]float64{400, 400}}) || c.EndInc != [2]int64{} {
				t.Fatalf("revived ends %+v, end_inc %v", c.El.Ends, c.EndInc)
			}
			if err := CheckContent(content(h.s).Elements); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReviewRound7Counterexamples(t *testing.T) {
	decode := func(t *testing.T, fields map[string]any) model.Element {
		t.Helper()
		el, _, err := model.DecodeElement(elOf(fields))
		if err != nil {
			t.Fatal(err)
		}
		return el
	}
	// reset installs the board's content with element el in place of the
	// alive element of the same id.
	type resetFunc func(t *testing.T, h *harness, el model.Element) Outcome
	swap := func(h *harness, el model.Element) Content {
		c := content(h.s)
		for i := range c.Elements {
			if c.Elements[i].ID == el.ID {
				el.Z = c.Elements[i].Z
				c.Elements[i] = el
			}
		}
		return c
	}
	resets := []struct {
		name string
		run  resetFunc
	}{
		{"replacement", func(t *testing.T, h *harness, el model.Element) Outcome {
			out, err := h.s.Replace(swap(h, el), "import", h.now)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"rollback", func(t *testing.T, h *harness, el model.Element) Outcome {
			out, err := h.s.Rollback(1, swap(h, el), h.now)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
	}
	pen := func(name string) map[string]any {
		return map[string]any{"id": eid(name), "type": "pen", "x": 0, "y": 0, "w": 50, "h": 50, "pts": []int{0, 0, 10000, 10000}}
	}
	sticky := func(name string) map[string]any {
		return map[string]any{"id": eid(name), "type": "sticky", "x": 0, "y": 0, "w": 100, "h": 100}
	}
	connector := func(name string) map[string]any {
		return map[string]any{"id": eid(name), "type": "connector", "ends": []any{free(0, 0), free(10, 10)}}
	}
	// A pending operation against an element whose id a reset gave another
	// type, under a new incarnation, is void:gone, and the rest of its batch
	// applies (4.1, rules 30 and 31).
	cases := []struct {
		name  string
		add   Op
		after map[string]any
		op    func(inc int64) Op
	}{
		{"text of a sticky that became a pen", addSticky("s", 0, 0), pen("s"), func(inc int64) Op {
			return setText("s", inc, "pending", 0)
		}},
		{"geom of a sticky that became a connector", addSticky("s", 0, 0), connector("s"), func(inc int64) Op {
			return setGeom("s", inc, model.Geom{X: 7, Y: 7, W: 100, H: 100}, "")
		}},
		{"end0 of a connector that became a sticky", addConnector("s", free(0, 0), free(10, 10), nil), sticky("s"), func(inc int64) Op {
			return Op{Op: OpSet, ID: eid("s"), Inc: inc, G: model.BundleEnd0, Val: raw(free(5, 5))}
		}},
		{"a sticky's color of a sticky that became a pen", addSticky("s", 0, 0), pen("s"), func(inc int64) Op {
			return Op{Op: OpSet, ID: eid("s"), Inc: inc, G: "style.color", Val: raw("blue")}
		}},
		{"a shape's kind of a shape that became a sticky", addShape("s", 0, 0, 100, 100, 0), sticky("s"), func(inc int64) Op {
			return Op{Op: OpSet, ID: eid("s"), Inc: inc, G: "style.shape", Val: raw("ellipse")}
		}},
	}
	for _, rs := range resets {
		for _, tc := range cases {
			t.Run(rs.name+": pending "+tc.name, func(t *testing.T) {
				h := newHarness(t)
				a := h.session("a")
				h.ok(a, tc.add, addSticky("x", 300, 0))
				old, oldType := h.inc(eid("s")), h.row(eid("s")).El.Type
				pending := []Op{tc.op(old), move(h, "x", 40, 40, ""), setText("x", h.inc(eid("x")), "kept", 0)}
				rs.run(t, h, decode(t, tc.after))
				if s := h.row(eid("s")); s.Inc == old || s.El.Type == oldType {
					t.Fatalf("reset kept incarnation %d of %s", s.Inc, s.El.Type)
				}
				out := h.do(a, pending...)
				if out.Reject != nil || out.Fatal != "" {
					t.Fatalf("pending batch refused: %+v %s", out.Reject, out.Fatal)
				}
				got := results(t, out)
				if got[0] != void(VoidGone) || got[1] != ResultOK || got[2] != ResultOK {
					t.Fatalf("results %v", got)
				}
				if x := h.row(eid("x")); x.El.Geom.X != 40 || x.El.Text != "kept" {
					t.Fatalf("unrelated element %+v", x.El)
				}
			})
		}
	}

	// Form errors stay permanent refusals whatever the board holds, and a
	// value that the resolved incarnation's own type does not accept is a
	// format error too.
	t.Run("malformed sets are refused before the incarnation resolves", func(t *testing.T) {
		h := newHarness(t)
		a := h.session("a")
		h.ok(a, addSticky("s", 0, 0), Op{Op: OpAdd, El: elOf(pen("p"))})
		stale := h.inc(eid("s")) + 1000
		for _, op := range []Op{
			{Op: OpSet, ID: eid("s"), Inc: stale, G: model.BundleText, Val: raw(5)},
			{Op: OpSet, ID: eid("s"), Inc: stale, G: "style.color", Val: raw("no-such-color")},
			{Op: OpSet, ID: eid("s"), Inc: stale, G: "style.nope", Val: raw("x")},
			{Op: OpSet, ID: model.BoardID, Inc: stale, G: model.BundleTitle, Val: raw("t")},
			{Op: OpSet, ID: eid("s"), Inc: stale, G: model.BundleFrame, Val: raw(nil)},
			{Op: OpSet, ID: eid("q"), Inc: stale, G: model.BundleGeom, Val: raw(map[string]any{"x": 1})},
		} {
			out := h.do(a, op, move(h, "s", 9, 9, ""))
			if out.Reject == nil || out.Reject.Code != CodeInvalid {
				t.Fatalf("%s %s: %+v %s", op.G, op.Val, out.Reject, out.Results)
			}
		}
		out := h.do(a, setText("p", h.inc(eid("p")), "x", 0), move(h, "s", 9, 9, ""))
		if out.Reject == nil || out.Reject.Code != CodeInvalid {
			t.Fatalf("text on a pen's own incarnation: %+v %s", out.Reject, out.Results)
		}
		if h.geom(eid("s")).X != 0 {
			t.Fatalf("a refused batch moved s")
		}
	})

	// A reset that keeps connector C exactly while its target id gets a new
	// incarnation (another type) changes only C's end_inc. The delta carries
	// it, so a Private copy that applies the delta equals a fresh snapshot.
	for _, rs := range resets {
		t.Run(rs.name+": a kept connector's end_inc reaches the delta", func(t *testing.T) {
			h := newHarness(t)
			a := h.session("a")
			h.ok(a, addShape("s", 100, 100, 100, 100, 0))
			h.ok(a, addConnector("c", attach("s", 1, 0.5), free(400, 400), map[string]int64{eid("s"): h.inc(eid("s"))}))
			before := map[string]model.PrivateElement{}
			for _, e := range h.s.Alive() {
				before[e.ID] = e
			}
			board := h.s.Board()
			cInc, sInc := h.inc(eid("c")), h.inc(eid("s"))
			out := rs.run(t, h, decode(t, map[string]any{"id": eid("s"), "type": "sticky", "x": 100, "y": 100, "w": 100, "h": 100}))
			c := h.row(eid("c"))
			if c.Inc != cInc || c.El.Ends[0].El != eid("s") || h.inc(eid("s")) == sInc || c.EndInc[0] != h.inc(eid("s")) {
				t.Fatalf("connector inc %d (was %d), ends %+v, end_inc %v, target inc %d", c.Inc, cInc, c.El.Ends, c.EndInc, h.inc(eid("s")))
			}
			if !slices.Contains(out.Changed.Elements, eid("c")) {
				t.Fatalf("changed rows %v", out.Changed.Elements)
			}
			if err := applyChanges("private", before, map[string]int64{}, &board, out.Delta); err != nil {
				t.Fatal(err)
			}
			want := h.s.Alive()
			if len(want) != len(before) {
				t.Fatalf("%d elements after the delta, snapshot has %d", len(before), len(want))
			}
			for _, w := range want {
				if got := before[w.ID]; !samePrivate(got, w) {
					t.Fatalf("%s after the delta: inc %d end_inc %v, snapshot: inc %d end_inc %v", w.ID, got.Inc, got.EndInc, w.Inc, w.EndInc)
				}
			}
		})
	}
}
