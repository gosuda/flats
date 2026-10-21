package engine

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/gosuda/flats/internal/board/model"
)

// trimSizes measures what the two-step trims count, split into guest and
// member parts: undo entries by session, deleted rows by who deleted them.
type trimSizes struct {
	memberUndo, guestUndo int64
	memberDel, guestDel   int64
	memberRows            []string
}

func measureTrim(s *State) trimSizes {
	var z trimSizes
	for id, st := range s.undo {
		for _, e := range append(slices.Clone(st.undo), st.redo...) {
			if s.GuestSession(id) {
				z.guestUndo += entryBytes(e)
			} else {
				z.memberUndo += entryBytes(e)
			}
		}
	}
	for id, r := range s.rows {
		if !r.Deleted {
			continue
		}
		b, _ := json.Marshal(r.PreRel)
		n := int64(len(r.El.CanonicalJSON()) + len(b))
		if model.IsGuestKey(r.DelKey) {
			z.guestDel += n
		} else {
			z.memberDel += n
			z.memberRows = append(z.memberRows, id)
		}
	}
	slices.Sort(z.memberRows)
	return z
}

// TestGuestTrimAggregates checks the two-step trims of undo storage and
// deleted rows on the aggregate of all guests' items (TECH public edit
// 4.4), with members' state already at the global cap:
//
//   - step 1: several guests together are cut to the guest cap, measured
//     over all of them, and no member item is dropped;
//   - step 2: over the global cap, guest items go first even when they are
//     under their own cap, and members keep everything while member items
//     alone fit;
//   - only when member items alone exceed the global cap are member items
//     trimmed, after every guest item is gone.
func TestGuestTrimAggregates(t *testing.T) {
	setup := func(t *testing.T) (*harness, trimSizes) {
		h := newHarness(t)
		m := h.session("mia")
		h.churn(m, "m", 20)
		before := measureTrim(h.s)
		for _, g := range []string{"gus", "gia", "gil"} {
			h.churn(h.guest(g), g, 15)
		}
		if z := measureTrim(h.s); z.guestUndo == 0 || z.guestDel == 0 {
			t.Fatalf("guests left no state: %+v", z)
		}
		return h, before
	}
	t.Run("step 1: guests together to their cap", func(t *testing.T) {
		h, member := setup(t)
		mid := measureTrim(h.s)
		// Caps of two thirds of the three guests' total: each guest alone
		// stays under its cap and only their sum exceeds it, so a cap
		// applied per session would trim nothing.
		undoCap, delCap := mid.guestUndo*2/3, mid.guestDel*2/3
		for id, st := range h.s.undo {
			if !h.s.GuestSession(id) {
				continue
			}
			var n int64
			for _, e := range append(slices.Clone(st.undo), st.redo...) {
				n += entryBytes(e)
			}
			if n > undoCap {
				t.Fatalf("one guest alone holds %d undo bytes, over the %d cap", n, undoCap)
			}
		}
		perGuestDel := map[string]int64{}
		for _, r := range h.s.rows {
			if r.Deleted && model.IsGuestKey(r.DelKey) {
				b, _ := json.Marshal(r.PreRel)
				perGuestDel[r.DelKey] += int64(len(r.El.CanonicalJSON()) + len(b))
			}
		}
		if len(perGuestDel) != 3 {
			t.Fatalf("deleted rows of %d guests", len(perGuestDel))
		}
		for k, n := range perGuestDel {
			if n > delCap {
				t.Fatalf("guest %s alone deleted %d bytes, over the %d cap", k, n, delCap)
			}
		}
		// The global cap does not bind: only the guest cap can trim.
		h.s.TrimUndoCapped(1<<40, undoCap)
		h.s.TrimDeletedCapped(1<<40, delCap)
		z := measureTrim(h.s)
		if z.guestUndo > undoCap || z.guestDel > delCap {
			t.Fatalf("guests' aggregate after trim: %d undo (cap %d), %d deleted bytes (cap %d)", z.guestUndo, undoCap, z.guestDel, delCap)
		}
		if z.guestUndo == 0 || z.guestDel == 0 {
			t.Fatalf("the guest cap trimmed everything: %d undo, %d deleted bytes", z.guestUndo, z.guestDel)
		}
		if z.memberUndo != member.memberUndo || !slices.Equal(z.memberRows, member.memberRows) {
			t.Fatalf("a member item was trimmed: undo %d -> %d, rows %d -> %d", member.memberUndo, z.memberUndo, len(member.memberRows), len(z.memberRows))
		}
	})

	t.Run("step 2: guests first over the global cap", func(t *testing.T) {
		h, member := setup(t)
		// A guest cap that does not bind; the global cap is exactly what
		// members hold, so every guest item has to go and no member item.
		h.s.TrimUndoCapped(member.memberUndo, 1<<40)
		h.s.TrimDeletedCapped(member.memberDel, 1<<40)
		z := measureTrim(h.s)
		if z.guestUndo != 0 || z.guestDel != 0 {
			t.Fatalf("guest items left over the global cap: %d undo, %d deleted bytes", z.guestUndo, z.guestDel)
		}
		if z.memberUndo != member.memberUndo || !slices.Equal(z.memberRows, member.memberRows) {
			t.Fatalf("a member item was trimmed while guest items could go")
		}
	})

	t.Run("members trimmed only when they alone exceed the cap", func(t *testing.T) {
		h, member := setup(t)
		h.s.TrimUndoCapped(member.memberUndo/2, 1<<40)
		h.s.TrimDeletedCapped(member.memberDel/2, 1<<40)
		z := measureTrim(h.s)
		if z.guestUndo != 0 || z.guestDel != 0 {
			t.Fatalf("guest items left while member items were trimmed: %d undo, %d deleted", z.guestUndo, z.guestDel)
		}
		if z.memberUndo > member.memberUndo/2 || z.memberDel > member.memberDel/2 || z.memberUndo == 0 || z.memberDel == 0 {
			t.Fatalf("member items after trim to half: undo %d of %d, deleted %d of %d", z.memberUndo, member.memberUndo, z.memberDel, member.memberDel)
		}
	})
}
