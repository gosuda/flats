package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

// gated is a real connection that keeps every frame as it arrived, for
// the real client to read later, and stops reading when the test says.
type gated struct {
	t       *testing.T
	ws      *wsconn.Conn
	mu      sync.Mutex
	cond    *sync.Cond
	steps   []replayStep
	kinds   []string
	seq     uint64 // the newest sequence a frame brought
	resync  int    // generation of the newest resync
	done    int    // generation of the newest tab_done
	closed  bool
	paused  bool
	pauseOn string // the kind of frame after which the reader stops
}

// gate opens a connection on route, watches tab and waits for its
// snapshot.
func (s *site) gate(t *testing.T, route string, tab TabID) *gated {
	t.Helper()
	host := strings.TrimPrefix(s.server.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, err := wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) { return new(net.Dialer).DialContext(ctx, "tcp", host) }, host, "/"+route+"/_sheet/ws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close(1000, "") })
	g := &gated{t: t, ws: ws}
	g.cond = sync.NewCond(&g.mu)
	go g.read()
	hello := helloV1
	if route == "private" {
		hello = `{"type":"hello","version":1,"name":"Ann","pending":0}`
	}
	for _, frame := range []string{hello, fmt.Sprintf(`{"type":"watch","generation":1,"tab":%d}`, tab)} {
		if err := ws.Send(frame); err != nil {
			t.Fatal(err)
		}
	}
	g.until("the first snapshot", 10*time.Second, func() bool { return g.done == 1 })
	return g
}

func (g *gated) read() {
	for {
		g.mu.Lock()
		for g.paused {
			g.cond.Wait()
		}
		g.mu.Unlock()
		raw, err := g.ws.ReadMessage(64 << 20)
		g.mu.Lock()
		if err != nil {
			g.closed = true
			g.cond.Broadcast()
			g.mu.Unlock()
			return
		}
		g.steps = append(g.steps, replayStep{Recv: raw})
		var f struct {
			Type       string
			Seq        string
			Generation int
			Part       int
			Parts      int
		}
		if m := partHeader.FindStringSubmatch(raw[:min(len(raw), 64)]); m != nil {
			f.Type, f.Seq = "delta_part", m[1]
			f.Part, _ = strconv.Atoi(m[2])
			f.Parts, _ = strconv.Atoi(m[3])
		} else {
			_ = json.Unmarshal([]byte(raw), &f)
		}
		g.kinds = append(g.kinds, f.Type)
		seq, _ := strconv.ParseUint(f.Seq, 10, 64)
		switch {
		case f.Type == "delta_part" && f.Part != f.Parts-1:
		case f.Type == "tab_done":
			g.done = f.Generation
		case f.Type == "resync":
			g.resync, g.seq = f.Generation, seq
		case seq > g.seq:
			g.seq = seq
		}
		if g.pauseOn != "" && f.Type == g.pauseOn {
			g.paused, g.pauseOn = true, ""
		}
		g.cond.Broadcast()
		g.mu.Unlock()
	}
}

// until waits for the frames that make reached true.
func (g *gated) until(what string, wait time.Duration, reached func() bool) {
	g.t.Helper()
	expired := false
	timer := time.AfterFunc(wait, func() {
		g.mu.Lock()
		expired = true
		g.cond.Broadcast()
		g.mu.Unlock()
	})
	defer timer.Stop()
	g.mu.Lock()
	defer g.mu.Unlock()
	for !reached() {
		if g.closed || expired {
			g.t.Fatalf("the connection ended or stalled before %s; it read %v", what, g.kinds)
		}
		g.cond.Wait()
	}
}

// pause stops the reader after the next frame of kind, or, when kind is
// empty, before its next read. A read already waiting for a frame still
// takes that frame.
func (g *gated) pause(kind string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if kind == "" {
		g.paused = true
		return
	}
	g.pauseOn = kind
}

func (g *gated) resume() {
	g.mu.Lock()
	g.paused = false
	g.cond.Broadcast()
	g.mu.Unlock()
}

// frames returns the recorded frames after the first snapshot, decoded,
// with the text of delta parts left out.
func (g *gated) frames() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []map[string]any
	for i, step := range g.steps {
		if g.kinds[i] == "delta_part" {
			out = append(out, map[string]any{"type": "delta_part"})
			continue
		}
		var f map[string]any
		if err := json.Unmarshal([]byte(step.Recv), &f); err != nil {
			g.t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

// holdPump stops the reader and fills the socket with edits until the
// send queue holds some of them, so a delta larger than the queue cannot
// go. The host's pump then waits at the next frame of the client until
// release, so it meets every change made meanwhile at once.
func (g *gated) holdPump(e *env, tab string) (release func()) {
	t := g.t
	t.Helper()
	e.in.hub.mu.Lock()
	var server *conn
	for c := range e.in.hub.conns {
		server = c
	}
	e.in.hub.mu.Unlock()
	queued := func() int64 { return server.ws.Budget().Used() }
	g.pause("")
	for n := 0; ; n++ {
		if n > 300 {
			t.Fatalf("the queue holds %d bytes after %d unread edits", queued(), n)
		}
		cells := make([]string, 8)
		for i := range cells {
			cells[i] = strconv.Quote(strconv.Itoa(n) + strings.Repeat("f", 25000))
		}
		e.mustAgent(`{"ops":[{"op":"write","tab":` + strconv.Quote(tab) + `,"at":"B1","rows":[[` + strings.Join(cells, ",") + `]]}]}`)
		time.Sleep(20 * time.Millisecond)
		if queued() > 0 {
			break
		}
	}
	entered, gone := make(chan struct{}), make(chan struct{})
	hookOnce(e, map[string]func(){"pump": func() { close(entered); <-gone }})
	t.Cleanup(func() {
		select {
		case <-gone:
		default:
			close(gone)
		}
	})
	id, row, col := e.ids(tab + "!A1")
	if err := g.ws.Send(fmt.Sprintf(`{"type":"peek","id":1,"tab":%d,"row":%d,"col":%d}`, id, row, col)); err != nil {
		t.Fatal(err)
	}
	<-entered
	return func() { close(gone) }
}

// escapedWrite writes 25 cells of an apostrophe, an equals sign and 30,000
// U+0001 into column A of a tab: one delta of about 9 MB, larger than both
// send queues.
func escapedWrite(e *env, tab string) {
	text := `["'=` + strings.Repeat(`\u0001`, 30000) + `"]`
	list := make([]string, 25)
	for i := range list {
		list[i] = text
	}
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"write","tab":%q,"at":"A1","rows":[%s]}]}`, tab, strings.Join(list, ",")))
}

// TestResyncBringsTheWorkbook cuts a Public and a Private connection off
// while the workbook changes around the tab it watches: the tab is
// deleted, another one is added and moved to the front, a third one is
// renamed and the calculation date moves. Each change is a delta the
// connection never gets, because the connection skips to the newest
// sequence: once when its send queue cannot take the next delta, and once
// when the host revokes a delta it was sending in parts. The resync
// itself carries the tab list and the date, and the snapshot after it is
// of the first tab of the list, since the watched one is gone. The real
// client then reads the recorded frames and must hold the engine's tab
// list, date and first tab, with no watch of its own.
func TestResyncBringsTheWorkbook(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is not installed, so the client cannot be checked in CI")
		}
		t.Skip("node is not installed")
	}
	for _, how := range []string{"lagged", "revoked"} {
		for _, route := range []string{"public", "private"} {
			t.Run(how+" "+route, func(t *testing.T) { resyncAcrossTabChanges(t, node, how, route) })
		}
	}
}

func resyncAcrossTabChanges(t *testing.T, node, how, route string) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Two"},{"op":"write","tab":"Two","at":"A1","rows":[["kept"]]}]}`)
	s := serve(e)
	watched := e.tabID("Sheet1")
	g := s.gate(t, route, watched)
	release := func() {}
	var pushed uint64 // the delta the connection cannot take
	if how == "lagged" {
		release = g.holdPump(e, "Sheet1")
		escapedWrite(e, "Sheet1")
		pushed = e.seq()
	} else {
		// The reader stops inside a delta of many parts. The deltas after
		// it push it out of the log while the connection still sends it,
		// and its charge does not fit the held budget: the host revokes it.
		old := deltaHeldBudget.Load()
		deltaHeldBudget.Store(1 << 20)
		t.Cleanup(func() { deltaHeldBudget.Store(old) })
		g.pause("delta_part")
		escapedWrite(e, "Sheet1")
		pushed = e.seq()
		g.until("a part of the large delta", 30*time.Second, func() bool { return g.paused })
	}
	before := deltaHeld.Load()
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Two","name":"Renamed"}]}`)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Added"},{"op":"write","tab":"Added","at":"A1","rows":[["first"]]}]}`)
	e.mustAgent(`{"ops":[{"op":"move_tab","tab":"Added","to":1}]}`)
	day := e.calcDate()
	e.clock.Add(24 * time.Hour)
	if err := e.in.syncDate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.calcDate() == day {
		t.Fatal("the date did not move")
	}
	e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Sheet1"}]}`)
	changed := e.seq()
	if how == "revoked" {
		escapedWrite(e, "Renamed")
		for deadline := time.Now().Add(10 * time.Second); deltaHeld.Load() != before; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the revoked delta is still held: %d bytes", deltaHeld.Load()-before)
			}
		}
	}
	release()
	g.resume()
	g.until("the snapshot after the resync", 20*time.Second, func() bool { return g.resync > 0 && g.done >= g.resync && g.seq >= e.seq() })

	// The host side: the changes reached the connection only through the
	// resync, which names the tab list and the date of its sequence, and
	// the snapshot after it is of the first tab.
	want := e.clientView("Added")
	var resync, head map[string]any
	for _, f := range g.frames() {
		switch f["type"] {
		case "resync":
			if resync != nil {
				t.Fatalf("a second resync: %v", f)
			}
			resync = f
		case "delta":
			if seq, _ := parseDecimal(f["seq"].(string)); resync == nil && seq > pushed-1 {
				t.Fatalf("delta %d arrived before the resync; the connection did not skip", seq)
			}
		case "tab_head":
			if resync != nil && head == nil {
				head = f
			}
		case "fatal":
			t.Fatalf("the connection ended: %v", f)
		}
	}
	if resync == nil {
		t.Fatal("no resync")
	}
	if at, _ := parseDecimal(resync["seq"].(string)); at < changed {
		t.Fatalf("the resync goes on from %d, before the changes ending at %d", at, changed)
	}
	if !reflect.DeepEqual(resync["tabs"], want["tabs"]) || resync["date"] != want["date"] {
		t.Errorf("the resync carries the tabs %v and the date %v, the engine has %v and %v", resync["tabs"], resync["date"], want["tabs"], want["date"])
	}
	if head == nil || head["tab"] != float64(e.tabID("Added")) || head["generation"] != resync["generation"] {
		t.Fatalf("the snapshot after resync %v: %v", resync, head)
	}

	// The client side.
	steps := func() []replayStep {
		g.mu.Lock()
		defer g.mu.Unlock()
		return append([]replayStep(nil), g.steps...)
	}()
	got := replay(t, node, steps)
	if diffs := compareClient(got, want); len(diffs) > 0 {
		t.Errorf("the client differs from the engine:\n  %s", strings.Join(diffs, "\n  "))
	}
	if watches, _ := got["watches"].([]any); len(watches) != 1 {
		t.Errorf("the client sent the watches %v, want only its first", watches)
	}
}

// TestWatchOfAGoneTab asks for a tab that was deleted after the client
// last heard of it. The answer is a snapshot of the first tab under the
// watch's generation, after the delta of the deletion, so the client is
// never left waiting for a tab that cannot come.
func TestWatchOfAGoneTab(t *testing.T) {
	for _, route := range []string{"public", "private"} {
		t.Run(route, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"Two"},{"op":"add_tab","name":"Three"}]}`)
			s := serve(e)
			g := s.gate(t, route, e.tabID("Three"))
			two := e.tabID("Two")
			e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Two"}]}`)
			deleted := e.seq()
			if err := g.ws.Send(fmt.Sprintf(`{"type":"watch","generation":2,"tab":%d}`, two)); err != nil {
				t.Fatal(err)
			}
			g.until("the answer to the watch", 10*time.Second, func() bool { return g.done == 2 })
			var head map[string]any
			got := false
			for _, f := range g.frames() {
				switch f["type"] {
				case "delta":
					got = got || f["seq"] == strconv.FormatUint(deleted, 10)
				case "tab_head":
					head = f
				}
			}
			if !got {
				t.Fatal("the delta of the deletion did not come before the snapshot")
			}
			if head["tab"] != float64(e.tabID("Sheet1")) || head["generation"] != float64(2) {
				t.Fatalf("the watch of a deleted tab got %v", head)
			}
		})
	}
}

// TestLagOverARollback cuts a connection off while a rollback is among the
// deltas it skips. The client gets the reset it would have got at that
// delta, and no resync: a reset makes it start over with a new welcome,
// which carries everything the rollback changed.
func TestLagOverARollback(t *testing.T) {
	for _, route := range []string{"public", "private"} {
		t.Run(route, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["before"]]}]}`)
			rev := e.b.importAll(t)
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"Later"}]}`)
			s := serve(e)
			g := s.gate(t, route, e.tabID("Sheet1"))
			release := g.holdPump(e, "Sheet1")
			escapedWrite(e, "Sheet1")
			if _, err := e.b.Rollback(context.Background(), rev, "Alice"); err != nil {
				t.Fatal(err)
			}
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A2","rows":[["after"]]}]}`)
			release()
			g.resume()
			g.until("the reset", 20*time.Second, func() bool { return slices.Contains(g.kinds, "reset") })
			for _, f := range g.frames() {
				switch f["type"] {
				case "resync":
					t.Fatalf("a resync over a rollback: %v", f)
				case "reset":
					return
				}
			}
		})
	}
}
