package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

const idleFor = 10 * time.Minute

// idleEnv is a sheet that nothing uses: its revisions are imported and its
// last use is older than idleFor.
func idleEnv(t *testing.T) *env {
	t.Helper()
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[1,2],[3,4],[5,6]]}]}`)
	e.b.importAll(t)
	e.clock.Add(idleFor + time.Second)
	return e
}

func (e *env) stopped() bool { return e.in.ctx.Err() != nil }

// TestStopIfIdle stops an instance only when nothing uses it and nothing
// has for the given time, and refuses every later request as stopped.
func TestStopIfIdle(t *testing.T) {
	e := start(t)
	// The first revision is not imported yet.
	e.clock.Add(idleFor + time.Second)
	if e.in.StopIfIdle(idleFor) || e.stopped() {
		t.Fatal("stopped with an entry in the outbox")
	}
	// The import is a use: the idle time starts again.
	e.b.importAll(t)
	if e.in.StopIfIdle(idleFor) {
		t.Fatal("stopped right after the import")
	}
	e.clock.Add(idleFor - time.Second)
	if e.in.StopIfIdle(idleFor) {
		t.Fatal("stopped before the idle time passed")
	}
	// An edit leaves an open group, then an outbox entry.
	e.person("Alice").set("A1", "1")
	e.clock.Add(idleFor + time.Second)
	if e.in.StopIfIdle(idleFor) {
		t.Fatal("stopped with an open edit group")
	}
	e.tick()
	e.clock.Add(idleFor + time.Second)
	if e.in.StopIfIdle(idleFor) {
		t.Fatal("stopped with the group's revision not imported")
	}
	e.b.importAll(t)
	// A connection keeps the instance, whatever the clock says.
	s := serve(e)
	c := s.dial("public", helloV1)
	c.next("welcome")
	e.clock.Add(idleFor + time.Second)
	if e.in.StopIfIdle(idleFor) {
		t.Fatal("stopped under an open connection")
	}
	c.ws.Close(1000, "")
	for deadline := time.Now().Add(5 * time.Second); e.in.inflight.Load() != 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if e.in.StopIfIdle(idleFor) {
		t.Fatal("stopped right after the connection closed")
	}
	e.clock.Add(idleFor + time.Second)
	if !e.in.StopIfIdle(idleFor) || !e.stopped() {
		t.Fatal("an unused instance was not stopped")
	}
	if !e.in.StopIfIdle(idleFor) {
		t.Fatal("a stopped instance reports that it runs")
	}
	// Every request to the stopped instance says so, and the host opens a
	// new one.
	_, err := e.in.Read(context.Background(), ReadQuery{Tab: "Sheet1"})
	e.wantRefusal(err, "unavailable", "stopped")
	_, err = e.in.ReadRevision(context.Background(), 1, nil, "2026-11-13", ReadQuery{})
	e.wantRefusal(err, "unavailable", "stopped")
	_, err = e.agent(`{"state":"","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[2]]}]}`)
	e.wantRefusal(err, "unavailable", "stopped")
	if res, err := http.Get(s.server.URL + "/private/"); err != nil || res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a page request to a stopped instance: %v, %v", res, err)
	}
	e.open()
	if got := e.shown("A1"); got != "1" {
		t.Fatalf("the reopened sheet has A1 = %q", got)
	}
}

// TestStopIfIdleKeepsCaptures covers a read that creates a capture on an
// instance that was idle until then: the cursor it returned stays good.
func TestStopIfIdleKeepsCaptures(t *testing.T) {
	for _, revision := range []bool{false, true} {
		t.Run(fmt.Sprint("revision=", revision), func(t *testing.T) {
			e := idleEnv(t)
			read := func(q ReadQuery) map[string]any {
				if revision {
					return e.readRevision(2, q)
				}
				return e.read(q)
			}
			cursor := cursorOf(t, read(ReadQuery{Tab: "Sheet1", Limit: 2}))
			if e.in.StopIfIdle(idleFor) {
				t.Fatal("stopped right after a read returned a cursor")
			}
			if next := read(ReadQuery{Cursor: cursor}); next["start"] != "A2" {
				t.Fatalf("the cursor reads %v", next["start"])
			}
			// The capture outlives the idle time while its pages are read.
			for i := 0; i < 15; i++ {
				e.clock.Add(captureTTL - time.Second)
				if e.in.StopIfIdle(idleFor) {
					t.Fatal("stopped with a live capture")
				}
				read(ReadQuery{Cursor: cursor})
			}
			// Once it has expired and the idle time has passed, nothing
			// holds the instance.
			e.clock.Add(captureTTL + time.Second)
			if e.in.StopIfIdle(idleFor) {
				t.Fatal("stopped before the idle time passed since the last page")
			}
			e.clock.Add(idleFor)
			if !e.in.StopIfIdle(idleFor) {
				t.Fatal("an instance with only expired captures was not stopped")
			}
		})
	}
}

// TestStopIfIdleRacesReads runs a read against the stop, many times. A read
// that got a cursor finds its instance running; a read that lost is told
// that the instance stopped.
func TestStopIfIdleRacesReads(t *testing.T) {
	rounds := 40
	if testing.Short() {
		rounds = 8
	}
	kept, refused := 0, 0
	for round := 0; round < rounds; round++ {
		e := idleEnv(t)
		var wg sync.WaitGroup
		var out []byte
		var readErr error
		var stopped bool
		begin := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-begin
			out, readErr = e.in.Read(context.Background(), ReadQuery{Tab: "Sheet1", Limit: 2})
		}()
		go func() {
			defer wg.Done()
			<-begin
			stopped = e.in.StopIfIdle(idleFor)
		}()
		close(begin)
		wg.Wait()
		switch {
		case readErr != nil:
			e.wantRefusal(readErr, "unavailable", "stopped")
			if !stopped {
				t.Fatalf("round %d: the read was refused but the instance runs", round)
			}
			refused++
		case stopped:
			t.Fatalf("round %d: the instance stopped after a read returned %s", round, out)
		default:
			page := e.read(ReadQuery{Cursor: cursorOf(t, pageOf(t, out))})
			if page["start"] != "A2" {
				t.Fatalf("round %d: the cursor reads %v", round, page["start"])
			}
			kept++
		}
		e.in.Stop()
	}
	t.Logf("%d reads kept their instance, %d found it stopped", kept, refused)
}

func pageOf(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestStopIfIdleWaitsForTheWriter asks while the writer is at work: the
// instance is in use, and the call does not wait for the work to end.
func TestStopIfIdleWaitsForTheWriter(t *testing.T) {
	e := idleEnv(t)
	entered, leave := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		// Work that was admitted before the idle time and still runs.
		w := &work{ctx: context.Background(), done: make(chan error, 1), run: func() error {
			close(entered)
			<-leave
			return nil
		}}
		e.in.queue <- w
		done <- <-w.done
	}()
	<-entered
	begin := time.Now()
	if e.in.StopIfIdle(idleFor) {
		t.Fatal("stopped while the writer ran")
	}
	if took := time.Since(begin); took > 2*time.Second {
		t.Fatalf("the call waited %v for the writer", took)
	}
	// Requests are admitted again as soon as the call returns.
	if _, err := e.in.Read(context.Background(), ReadQuery{}); err != nil {
		t.Fatalf("a read after the call: %v", err)
	}
	close(leave)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.clock.Add(idleFor + time.Second)
	if !e.in.StopIfIdle(idleFor) {
		t.Fatal("not stopped after the writer finished")
	}
}
