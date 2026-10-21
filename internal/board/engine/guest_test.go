package engine

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/gosuda/flats/internal/board/model"
)

func (h *harness) guest(name string) string {
	h.t.Helper()
	r := h.s.Hello(Hello{NewID: "g-" + name, Key: model.GuestKeyPrefix + name, Label: name, Now: h.now})
	if r.Fatal != "" {
		h.t.Fatalf("hello %s: %s", name, r.Fatal)
	}
	return r.Session
}

// churn makes session create and delete elements: receipts, undo entries
// and deleted rows.
func (h *harness) churn(session, prefix string, n int) {
	for i := range n {
		name := fmt.Sprintf("%s%d", prefix, i)
		h.ok(session, addSticky(name, float64(i), 0))
		h.ok(session, Op{Op: OpDelete, IDs: []string{eid(name)}})
	}
}

// TestGuestTrimKeepsMembers: with members' receipts, undo entries and
// deleted rows already at the global cap, guest traffic trims only guest
// items (TECH public edit 4.4).
func TestGuestTrimKeepsMembers(t *testing.T) {
	h := newHarness(t)
	m := h.session("mia")
	g := h.guest("gus")
	h.churn(m, "m", 30)
	var memberReceipts int64
	for _, r := range h.s.receipts {
		memberReceipts += receiptBytes(r)
	}
	memberUndo := h.s.UndoBytes()
	memberRows := map[string]bool{}
	for id, r := range h.s.rows {
		if r.Deleted {
			memberRows[id] = true
		}
	}
	nReceipts, nUndo := len(h.s.receipts), len(h.s.undo[m].undo)

	h.churn(g, "g", 30)
	h.s.TrimReceiptsCapped(memberReceipts, 500, h.now)
	h.s.TrimUndoCapped(memberUndo, 500)
	// The member rows' size is measured as TrimDeleted does.
	var limit int64
	for id := range memberRows {
		r := h.s.rows[id]
		b, _ := json.Marshal(r.PreRel)
		limit += int64(len(r.El.CanonicalJSON()) + len(b))
	}
	h.s.TrimDeletedCapped(limit, 500)

	got := 0
	for k := range h.s.receipts {
		if k.Session == m {
			got++
		}
	}
	if got != nReceipts {
		t.Fatalf("member receipts %d of %d kept", got, nReceipts)
	}
	if len(h.s.undo[m].undo) != nUndo {
		t.Fatalf("member undo entries %d of %d kept", len(h.s.undo[m].undo), nUndo)
	}
	for id := range memberRows {
		if h.s.rows[id] == nil {
			t.Fatalf("member deleted row %s trimmed", id)
		}
	}
	var guestReceipts int64
	for k, r := range h.s.receipts {
		if k.Session == g {
			guestReceipts += receiptBytes(r)
		}
	}
	if guestReceipts > 500 {
		t.Fatalf("guest receipts %d bytes over their cap", guestReceipts)
	}
	for id, r := range h.s.rows {
		if r.Deleted && model.IsGuestKey(r.DelKey) && !memberRows[id] {
			b, _ := json.Marshal(r.PreRel)
			if int64(len(r.El.CanonicalJSON())+len(b)) > 500 {
				t.Fatalf("guest deleted rows over their cap")
			}
		}
	}
}

func TestTrimTwoStep(t *testing.T) {
	sizes := []int64{10, 10, 10, 10, 10, 10}
	guest := []bool{false, true, false, true, true, false}
	got := trimTwoStep(len(sizes), func(i int) int64 { return sizes[i] }, func(i int) bool { return guest[i] }, 20, 10)
	// Step 1: guests down to 10 bytes (items 1, 3); step 2: the last guest
	// (4), then the oldest member (0).
	want := []int{1, 3, 4, 0}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("dropped %v, want %v", got, want)
	}
}

// TestCreatedCountFollowsRollback: the created-row counts stay exact
// through savepoint rollbacks and dropped sessions.
func TestCreatedCountFollowsRollback(t *testing.T) {
	h := newHarness(t)
	g := h.guest("gus")
	h.ok(g, addSticky("a", 0, 0))
	m := h.s.Savepoint()
	h.ok(g, addSticky("b", 0, 0))
	if h.s.CreatedCount(g) != 2 {
		t.Fatalf("count %d", h.s.CreatedCount(g))
	}
	h.s.RollbackTo(m)
	if h.s.CreatedCount(g) != 1 || h.s.GuestCreated() != 1 {
		t.Fatalf("count after rollback %d", h.s.CreatedCount(g))
	}
	h.s.CapGuestSessions(0, nil)
	if h.s.CreatedCount(g) != 0 || h.s.Sessions() != 0 {
		t.Fatalf("count after the session was dropped %d", h.s.CreatedCount(g))
	}
}

// TestGuestReviveRules: a guest revives only rows it deleted after its
// session started, and copies only images that are live now.
func TestGuestReviveRules(t *testing.T) {
	h := newHarness(t)
	m := h.session("mia")
	h.ok(m, addSticky("a", 0, 0))
	h.ok(m, Op{Op: OpDelete, IDs: []string{eid("a")}})
	g := h.guest("gus")
	out := h.do(g, Op{Op: OpRevive, IDs: []string{eid("a")}, IfDel: h.row(eid("a")).DelSeq})
	if out.Reject == nil || out.Reject.Code != CodeGuestRevive {
		t.Fatalf("revive of a member's deletion %+v", out.Reject)
	}
	h.ok(g, addSticky("b", 0, 0))
	h.ok(g, Op{Op: OpDelete, IDs: []string{eid("b")}})
	if out := h.do(g, Op{Op: OpRevive, IDs: []string{eid("b")}, IfDel: h.row(eid("b")).DelSeq}); out.Reject != nil {
		t.Fatalf("revive of its own deletion %+v", out.Reject)
	}
	el := h.row(eid("b")).El
	if el.LK != model.KindGuest || el.TA[model.BundleText] != (model.TextAuthor{K: model.KindGuest, N: "gus"}) {
		t.Fatalf("authorship %+v %q", el.TA, el.LK)
	}
}

// TestAuthorshipOnlyRollback: a rollback that restores the same visible
// content with other authorship raises the element's ver, so a guard read
// before fails, and sends the element whole; an ordinary set that changes
// authorship carries it (TECH public edit 7, board-TECH 8.1).
func TestAuthorshipOnlyRollback(t *testing.T) {
	h := newHarness(t)
	if _, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "add", El: elOf(map[string]any{"id": eid("a"), "type": "sticky",
		"x": 0, "y": 0, "w": 100, "h": 100, "text": "plan"})}}, Label: "bot"}); err != nil {
		t.Fatal(err)
	}
	rev := content(h.s)
	g := h.guest("gus")
	// The guest retypes the same text: visible content unchanged, text now
	// guest-written. The set carries the new authorship.
	out := h.ok(g, Op{Op: OpSet, ID: eid("a"), Inc: h.inc(eid("a")), G: model.BundleText, Val: raw("plan")})
	c := out.Delta.Changes[0]
	if c.Author == nil || c.Author.TA[model.BundleText].K != model.KindGuest || c.Author.LK != model.KindGuest {
		t.Fatalf("set change %+v", c)
	}
	ver, _ := h.s.Ver(eid("a"))
	r, err := h.s.Rollback(1, rev, h.now)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := h.s.Ver(eid("a"))
	if after <= ver {
		t.Fatalf("ver %d after the rollback, %d before", after, ver)
	}
	if len(r.Delta.Changes) != 1 || r.Delta.Changes[0].Revive == nil || r.Delta.Changes[0].Revive.TA[model.BundleText].K != model.KindAgent {
		t.Fatalf("rollback delta %+v", r.Delta.Changes)
	}
	if _, err := h.agent(AgentCall{Ops: []AgentOp{{Op: "set", ID: eid("a"), Fields: raw(map[string]any{"text": "x"}), IfVer: raw(ver)}}, Label: "bot"}); err == nil {
		t.Fatal("a guard read before the authorship change still matched")
	}
}
