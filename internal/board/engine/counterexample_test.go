package engine

import (
	"encoding/json"
	"fmt"
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
