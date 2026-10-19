package board

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
)

// holdLoop parks the apply loop until the returned release runs, so items
// queue and the next batch's group commit collects them together.
func holdLoop(t *testing.T, b *tb) (release func()) {
	t.Helper()
	started, gate := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- b.in.do(context.Background(), func() error {
			close(started)
			<-gate
			return nil
		})
	}()
	<-started
	return func() {
		close(gate)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func groupsOf(t *testing.T, b *tb) map[string]editGroup {
	t.Helper()
	var g map[string]editGroup
	if err := b.in.do(context.Background(), func() error { g = maps.Clone(b.in.sd.groups); return nil }); err != nil {
		t.Fatal(err)
	}
	return g
}

// TestQuotaRefusalInsideOneGroupCommit: batches of two editors that one
// group commit collects are checked one by one against the quota; the one
// that fits applies and is seen, the one over the quota is refused with no
// delta and no edit group (board-TECH 5.4, 14 Quota).
func TestQuotaRefusalInsideOneGroupCommit(t *testing.T) {
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	o := connect(t, b.priv, map[string]any{"editor": "olga"}, nil)
	o.snapshot()
	w := connect(t, b.priv, map[string]any{"editor": "walt"}, nil)
	w.snapshot()
	if m := a.settle(1, sticky(eid(), "warm", 0)); m["t"] != "ack" {
		t.Fatal(m)
	}
	o.expect("delta")
	w.expect("delta")
	b.core.setQuota(measureUsage(t, b) + 20<<10)
	// The measurement is fresh: a small batch is answered without a
	// transient refusal.
	if m := o.settle(1, sticky(eid(), "warm", 300)); m["t"] != "ack" {
		t.Fatal(m)
	}
	a.expect("delta")
	w.expect("delta")
	groupsBefore := groupsOf(t, b)

	var commits atomic.Int32
	b.in.hook = func(stage string) {
		if stage == "commit" {
			commits.Add(1)
		}
	}
	release := holdLoop(t, b)
	small, big := eid(), eid()
	a.send(map[string]any{"t": "ops", "n": 2, "ops": []any{sticky(small, "fits", 600)}})
	var bigOps []any
	bigOps = append(bigOps, sticky(big, strings.Repeat("x", 4000), 900))
	for i := 0; i < 5; i++ {
		bigOps = append(bigOps, sticky(eid(), noise(4000, i), float64(1200+i*300)))
	}
	o.send(map[string]any{"t": "ops", "n": 2, "ops": bigOps})
	waitFor(t, func() bool { return len(b.in.items) == 2 })
	release()

	answer := func(c *client) map[string]any {
		m, ok := c.next(func(m map[string]any) bool {
			return (m["t"] == "ack" || m["t"] == "reject") && m["n"] == 2.0
		})
		if !ok {
			t.Fatal("no answer to batch 2")
		}
		return m
	}
	if m := answer(a); m["t"] != "ack" {
		t.Fatalf("the batch that fits %v", m)
	}
	if m := answer(o); m["t"] != "reject" || m["code"] != "disk_quota" || m["retry"] != false {
		t.Fatalf("the batch over the quota %v", m)
	}
	if n := commits.Load(); n != 1 {
		t.Fatalf("the two batches took %d commits, want one group commit", n)
	}
	// The third editor sees the applied batch only.
	d := w.expect("delta")
	if !strings.Contains(d["_raw"].(string), small) {
		t.Fatalf("delta %v", d["_raw"])
	}
	if !w.quiet(200*time.Millisecond, isDelta) {
		t.Fatal("another editor saw a delta of the refused batch")
	}
	alive := b.alive()
	if _, ok := alive[small]; !ok {
		t.Fatal("the batch that fits was not applied")
	}
	if _, ok := alive[big]; ok {
		t.Fatal("the refused batch applied")
	}
	// Only the applied batch touched an edit group: Olga's group is as it
	// was before.
	groups := groupsOf(t, b)
	if groups["browser:olga"] != groupsBefore["browser:olga"] {
		t.Fatalf("the refused batch changed its editor's group: %+v -> %+v", groupsBefore["browser:olga"], groups["browser:olga"])
	}
	if !groups["browser:alice"].Dirty {
		t.Fatalf("groups %+v", groups)
	}
}

// TestRefusedAgentWriteLeavesGroupsUnchanged: an agent write closes the
// open person groups before it applies; refused over the quota, it leaves
// those groups, the outbox and the board as they were (board-TECH 5.4,
// 14 Quota).
func TestRefusedAgentWriteLeavesGroupsUnchanged(t *testing.T) {
	ctx := context.Background()
	b := newTB(t)
	a := connect(t, b.priv, map[string]any{"editor": "alice", "name": "Alice"}, nil)
	a.snapshot()
	id := eid()
	if m := a.settle(1, sticky(id, "open group", 0)); m["t"] != "ack" {
		t.Fatal(m)
	}
	groupsBefore := groupsOf(t, b)
	if !groupsBefore["browser:alice"].Dirty {
		t.Fatalf("groups %+v", groupsBefore)
	}
	outbox := len(mustOutbox(t, b.in))
	b.core.setQuota(measureUsage(t, b) + 8<<10)
	var ops []engine.AgentOp
	for i := 0; i < 12; i++ {
		el, _ := json.Marshal(map[string]any{"type": "sticky", "text": noise(3900, i)})
		ops = append(ops, engine.AgentOp{Op: "add", El: el})
	}
	err := b.core.Exclusive(ctx, func(ctx context.Context, other int64) error {
		_, err := b.in.UpdateBoard(ctx, UpdateRequest{OpID: "refused-agent-1", Label: "Agent", Ops: ops}, other)
		return err
	})
	var be *Error
	if !errors.As(err, &be) || be.Category != CategoryCapacity {
		t.Fatalf("agent write over the quota: %v", err)
	}
	if got := groupsOf(t, b); !maps.Equal(got, groupsBefore) {
		t.Fatalf("a refused agent write changed the groups: %+v -> %+v", groupsBefore, got)
	}
	if n := len(mustOutbox(t, b.in)); n != outbox {
		t.Fatalf("a refused agent write changed the outbox: %d -> %d entries", outbox, n)
	}
	if _, ok, _ := b.in.Op(ctx, "refused-agent-1"); ok {
		t.Fatal("a refused agent write recorded its op")
	}
	if got := b.alive(); len(got) != 1 || got[id] != "open group" {
		t.Fatalf("board after the refusal %v", got)
	}
	// The person's group still closes into one person revision later.
	b.core.setQuota(0)
	b.clock.Add(31 * time.Second)
	if err := b.in.do(ctx, b.in.tick); err != nil {
		t.Fatal(err)
	}
	if err := b.core.importOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	kinds := revKinds(b.core)
	if kinds[len(kinds)-1] != "person" {
		t.Fatalf("revisions %v", kinds)
	}
}
