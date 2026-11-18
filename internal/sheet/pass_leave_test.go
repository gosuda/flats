package sheet

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// TestPagesThatLeaveWhileTheirActionWaits has an admitted editor send an
// action that waits for a pass: at the pass of a date change, in the
// writer's queue when a date change begins, and at the pass of a
// rollback. The page then closes while nothing reads its connection. The
// pings of the connection find that it left, and the connection ends, with
// its place under the connection limit and its presence, while the pass
// is still held. The withdrawn action is not applied and has no receipt.
// The same action sent again over a new connection applies once.
func TestPagesThatLeaveWhileTheirActionWaits(t *testing.T) {
	ping := livePing
	livePing = 20 * time.Millisecond
	t.Cleanup(func() { livePing = ping })
	for _, tc := range []struct {
		name string
		// waits starts a pass that the returned function lets end, and has
		// send put the action where the case says.
		waits func(t *testing.T, e *env, send func()) (end func())
	}{
		{"at the pass of a date change", func(t *testing.T, e *env, send func()) func() {
			held := holdPasses(t)
			held.hold(e.in)
			e.clock.Add(15 * time.Hour) // past midnight, within the life of the session
			_, err := e.in.Read(context.Background(), ReadQuery{Tab: "Sheet1", Range: "B1"})
			wantCalculating(t, "the read that meets the new date", err)
			held.reached(t, 1)
			waiting := make(chan struct{})
			hookOnce(e, map[string]func(){"pass_wait": func() { close(waiting) }})
			send()
			within(t, "the action at the pass", waiting)
			return func() {
				held.release()
				e.read(ReadQuery{Tab: "Sheet1", Range: "B1"})
			}
		}},
		{"in the queue when a date change begins", func(t *testing.T, e *env, send func()) func() {
			held := holdPasses(t)
			held.hold(e.in)
			at, gate := make(chan struct{}), make(chan struct{})
			go e.in.execute(context.Background(), &work{run: func() error {
				close(at)
				<-gate
				return nil
			}})
			within(t, "the work that keeps the writer", at)
			e.clock.Add(15 * time.Hour) // past midnight, within the life of the session
			_, err := e.in.Read(context.Background(), ReadQuery{Tab: "Sheet1", Range: "B1"})
			wantCalculating(t, "the read that meets the new date", err)
			send()
			for deadline := time.Now().Add(30 * time.Second); len(e.in.queue) != 2; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("%d works are queued, want the date change and the action", len(e.in.queue))
				}
			}
			close(gate)
			held.reached(t, 1)
			return func() {
				held.release()
				e.read(ReadQuery{Tab: "Sheet1", Range: "B1"})
			}
		}},
		{"at the pass of a rollback", func(t *testing.T, e *env, send func()) func() {
			target := e.b.importAll(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"E1","rows":[["newer"]]}]}`)
			e.b.importAll(t)
			held := holdPasses(t)
			held.hold(e.in)
			done := make(chan error, 1)
			go func() {
				_, err := e.b.Rollback(context.Background(), target, "Ann")
				done <- err
			}()
			held.reached(t, 1)
			waiting := make(chan struct{})
			hookOnce(e, map[string]func(){"pass_wait": func() { close(waiting) }})
			send()
			within(t, "the action at the pass", waiting)
			return func() {
				held.release()
				if err := within(t, "the rollback", done); err != nil {
					t.Fatal(err)
				}
				e.want("E1", "")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := longSheet(t)
			s := serve(e)
			tab, row, col := e.ids("A1")
			observer := s.dial("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			observer.next("welcome")
			editor := s.dial("private", helloNamed)
			welcome := editor.next("welcome")
			token, _ := welcome["session"].(string)
			id, _ := welcome["session_id"].(string)
			editor.send(fmt.Sprintf(`{"type":"presence","tab":%d,"active":[%d,%d]}`, tab, row, col))
			for people := []any(nil); len(people) == 0; {
				people, _ = observer.next("presence")["people"].([]any)
			}
			action := fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[%s]}`, e.seq(), e.setOp("F1", "withdrawn"))
			end := tc.waits(t, e, func() { editor.send(action) })
			// The page closes after it was told that the sheet calculates:
			// the pump then has nothing more to send it.
			if f := editor.next("status"); f["calculating"] != true {
				t.Fatalf("the editor got %v", f)
			}
			editor.ws.Close(1000, "")
			timed(t, "the end of the connection that left", func() {
				for deadline := time.Now().Add(shortBound); ; time.Sleep(time.Millisecond) {
					e.in.smu.Lock()
					left := e.in.sessions[id] == nil || e.in.sessions[id].conn == nil
					e.in.smu.Unlock()
					if e.in.hub.count() == 1 && left {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("%d connections remain, the session is still connected: %v", e.in.hub.count(), !left)
					}
				}
				for people := []any{nil}; len(people) != 0; {
					people, _ = observer.next("presence")["people"].([]any)
				}
			})
			if !e.in.calculating() {
				t.Fatal("the pass ended before the connection did")
			}

			end()
			e.want("F1", "")
			var receipts int
			if err := e.in.rdb.QueryRow(`SELECT count(*) FROM receipts WHERE session=?`, id).Scan(&receipts); err != nil || receipts != 0 {
				t.Fatalf("the withdrawn action left %d receipts, %v", receipts, err)
			}
			again := s.dial("private", fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"pending":1}`, token))
			if f := again.next("welcome", "fatal"); f["type"] != "welcome" {
				t.Fatalf("the new connection got %v", f)
			}
			again.send(action)
			ack := again.next("ack")
			if results, _ := ack["results"].([]any); len(results) != 1 || results[0].(map[string]any)["status"] != "applied" {
				t.Fatalf("the action sent again: %v", ack)
			}
			seq := e.seq()
			again.send(action)
			if twice := again.next("ack"); twice["seq"] != ack["seq"] || e.seq() != seq {
				t.Fatalf("the action sent a third time: %v, sequence %d, want %v and %d", twice, e.seq(), ack["seq"], seq)
			}
			e.want("F1", "withdrawn")
		})
	}
}

// TestPageThatLeavesWhileTheWriterHasItsAction closes the page of an
// editor while the writer commits its action. Work the writer has taken
// is not withdrawn: the action applies, its receipt stands, and the
// connection ends once the commit is done. The action sent again over a
// new connection is answered from its receipt and changes nothing.
func TestPageThatLeavesWhileTheWriterHasItsAction(t *testing.T) {
	ping := livePing
	livePing = 20 * time.Millisecond
	t.Cleanup(func() { livePing = ping })
	e := start(t)
	var armed atomic.Bool
	at, gate := make(chan struct{}), make(chan struct{})
	e.in.fault = func() error {
		if armed.CompareAndSwap(true, false) {
			close(at)
			<-gate
		}
		return nil
	}
	s := serve(e)
	editor := s.dial("private", helloNamed)
	welcome := editor.next("welcome")
	token, _ := welcome["session"].(string)
	action := fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[%s]}`, e.seq(), e.setOp("A1", "taken"))
	armed.Store(true)
	editor.send(action)
	within(t, "the commit of the action", at)
	editor.ws.Close(1000, "")
	// The pings find that the page left while the commit is held.
	time.Sleep(10 * livePing)
	if e.in.hub.count() != 1 {
		t.Fatalf("%d connections, want the one whose action the writer commits", e.in.hub.count())
	}
	close(gate)
	for deadline := time.Now().Add(shortBound); e.in.hub.count() != 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d connections remain after the commit", e.in.hub.count())
		}
	}
	e.want("A1", "taken")
	seq := e.seq()
	again := s.dial("private", fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"pending":1}`, token))
	again.next("welcome")
	again.send(action)
	if ack := again.next("ack"); ack["seq"] != fmt.Sprint(seq) || e.seq() != seq {
		t.Fatalf("the action sent again: %v, sequence %d, want %d", ack, e.seq(), seq)
	}
}
