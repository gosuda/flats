package runtime

import "testing"

// A wake never blocks the HTTP VM that commits a docs edit: a full queue
// drops it, and rooms catch up at the next heartbeat instead.
func TestWSWakeNeverBlocks(t *testing.T) {
	h := &wsHub{events: make(chan wsEvent, 1)}
	h.wake("index.md")
	h.wake("other.md")
	if len(h.events) != 1 {
		t.Fatal("queue", len(h.events))
	}
	ev := <-h.events
	if ev.Type != "wake" || ev.Data == nil || *ev.Data != "index.md" {
		t.Fatalf("%+v", ev)
	}
}
