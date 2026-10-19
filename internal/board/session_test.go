package board

import (
	"context"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
)

func (b *tb) tick() {
	b.t.Helper()
	if err := b.in.do(context.Background(), b.in.tick); err != nil {
		b.t.Fatal(err)
	}
}

func (b *tb) hasSession(id string) bool {
	ok := false
	b.state(func(st *engine.State) { _, ok = st.SessionRow(id) })
	return ok
}

// An open socket keeps its session alive past 24 hours without edits; the
// session expires 24 hours after the socket closes (4.8).
func TestOpenSocketKeepsSession(t *testing.T) {
	b := newTB(t)
	c := connect(t, b.priv, nil, nil)
	for range 3 {
		b.clock.Add(20 * time.Hour)
		b.tick()
		if !b.hasSession(c.session) {
			t.Fatal("the session of an open socket expired")
		}
	}
	c.ws.Close(1000, "")
	deadline := time.Now().Add(5 * time.Second)
	for b.in.hub.privateCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	b.clock.Add(23 * time.Hour)
	b.tick()
	if !b.hasSession(c.session) {
		t.Fatal("the session expired less than 24 hours after its last activity")
	}
	b.clock.Add(2 * time.Hour)
	b.tick()
	if b.hasSession(c.session) {
		t.Fatal("the session of a closed socket outlived 24 hours")
	}
}

// rollbackTo imports the outbox and rolls the board back to revision n.
func (b *tb) rollbackTo(n int) {
	b.t.Helper()
	ctx := context.Background()
	if err := b.core.importOutbox(ctx); err != nil {
		b.t.Fatal(err)
	}
	if _, err := b.core.Rollback(ctx, n, "Pat"); err != nil {
		b.t.Fatal(err)
	}
}

// reconnectReset reconnects alice's session at watermark and reports the snapshot's
// reset flag.
func (b *tb) reconnectReset(session string, watermark int64) bool {
	b.t.Helper()
	c := connect(b.t, b.priv, map[string]any{"editor": "alice", "session": session, "watermark": watermark}, nil)
	if c.session != session {
		b.t.Fatalf("reconnect got session %s, want %s", c.session, session)
	}
	return c.expect("snapshot")["reset"] == true
}

// A client reconnecting through a snapshot learns of every rollback after
// its watermark, even after the delta log trimmed the rollback's delta, and,
// through ResetFloor, of resets the count cap dropped (4.8).
func TestReconnectSnapshotReportsReset(t *testing.T) {
	b := newTB(t)
	b.in.deltaMaxEntries = 3
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	watermark, _, _ := a.snapshot()
	a.ws.Close(1000, "")
	b.rollbackTo(1)
	o := connect(t, b.priv, map[string]any{"editor": "olga"}, nil)
	o.snapshot()
	for i := range 10 {
		if m := o.settle(i+1, sticky(eid(), "later", float64(i*210))); m["t"] != "ack" {
			t.Fatalf("edit %d: %v", i, m)
		}
	}
	b.tick()
	b.clock.Add(time.Hour + time.Minute)
	b.tick()
	if !b.reconnectReset(a.session, watermark) {
		t.Fatal("a reconnect from before the rollback got a snapshot without reset")
	}
	var seq int64
	b.state(func(st *engine.State) { seq = st.Seq() })
	if b.reconnectReset(a.session, seq-1) {
		t.Fatal("a reconnect after the rollback got reset")
	}

	// The cap drops the oldest reset; a watermark before it still gets reset.
	b.in.resetsMax = 1
	b.rollbackTo(1)
	b.tick()
	var resets []int64
	var floor int64
	b.state(func(st *engine.State) { resets, floor = st.Resets(), st.ResetFloor() })
	if len(resets) != 1 || floor == 0 || floor >= resets[0] {
		t.Fatalf("resets %v, floor %d", resets, floor)
	}
	if !b.reconnectReset(a.session, watermark) {
		t.Fatal("a reconnect from before a dropped reset got no reset")
	}
	b.restart()
	b.state(func(st *engine.State) {
		if st.ResetFloor() != floor || len(st.Resets()) != 1 || st.ResetTimes()[0] == 0 {
			t.Fatalf("after restart: resets %v at %v, floor %d", st.Resets(), st.ResetTimes(), st.ResetFloor())
		}
	})
	// Resets older than the session lifetime are dropped.
	b.clock.Add(25 * time.Hour)
	b.tick()
	b.state(func(st *engine.State) {
		if len(st.Resets()) != 0 || st.ResetFloor() <= floor {
			t.Fatalf("after a day: resets %v, floor %d", st.Resets(), st.ResetFloor())
		}
	})
}
