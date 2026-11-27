package sheet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// probed holds what the instances of the running tests do at their probes.
var probed = struct {
	sync.Mutex
	steps map[*Instance]map[string]func()
}{steps: map[*Instance]map[string]func(){}}

func init() {
	fn := func(in *Instance, step string) {
		probed.Lock()
		run := probed.steps[in][step]
		delete(probed.steps[in], step)
		probed.Unlock()
		if run != nil {
			run()
		}
	}
	probes.Store(&fn)
}

// hookOnce runs each function the first time the instance reaches its
// probe. It replaces what an earlier call set for the instance.
func hookOnce(e *env, steps map[string]func()) {
	probed.Lock()
	probed.steps[e.in] = steps
	probed.Unlock()
	in := e.in
	e.t.Cleanup(func() {
		probed.Lock()
		delete(probed.steps, in)
		probed.Unlock()
	})
}

// TestAckBeforeFatal applies an action whose ack is still waiting in the
// connection's queue when the next action ends the connection. The client
// reads the ack first: the fatal frame keeps its place in the queue.
func TestAckBeforeFatal(t *testing.T) {
	for _, bad := range []struct{ name, frame, code string }{
		{"a frame the server cannot read", `{"type":"action","n":"2","base":"x","ops":[{"op":"undo"}]}`, "protocol"},
		{"an action the host cannot commit", `{"type":"action","n":"2","base":"%d","ops":[{"op":"add_tab","name":"Lost"}]}`, "unavailable"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			e := start(t)
			var failing atomic.Bool
			e.in.fault = func() error {
				if failing.Load() {
					return errors.New("the disk is gone")
				}
				return nil
			}
			s := serve(e)
			c := s.dial("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			c.next("welcome")
			tab, row, col := e.ids("A1")
			c.watch(1, tab)
			e.in.hub.mu.Lock()
			var server *conn
			for peer := range e.in.hub.conns {
				server = peer
			}
			e.in.hub.mu.Unlock()
			// The pump stops when it takes the ack of the first action off
			// the queue, until the end of the connection is queued behind.
			held, release := make(chan struct{}), make(chan struct{})
			hookOnce(e, map[string]func(){"pump": func() {
				close(held)
				<-release
			}})
			base := e.seq()
			c.send(fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":"kept","at":[0,0]}]}`, base, tab, row, col))
			<-held
			failing.Store(bad.code == "unavailable")
			frame := bad.frame
			if strings.Contains(frame, "%d") {
				frame = fmt.Sprintf(frame, base)
			}
			c.send(frame)
			for deadline := time.Now().Add(10 * time.Second); len(server.cmds) == 0; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the end of the connection was not queued behind the ack")
				}
			}
			close(release)
			var order []string
			for {
				f, ok := c.wait("ack", "fatal")
				if !ok {
					break
				}
				order = append(order, fmt.Sprint(f["type"], " ", f["n"], f["code"]))
			}
			if want := []string{"ack 1<nil>", "fatal <nil>" + bad.code}; fmt.Sprint(order) != fmt.Sprint(want) {
				t.Fatalf("the client read %v, want %v", order, want)
			}
			failing.Store(false)
			if got := e.shown("A1"); got != "kept" {
				t.Fatalf("A1 = %q", got)
			}
		})
	}
}

// TestSnapshotSupersededByResync switches tabs while the log runs away
// from the connection. The tab is captured at S; before the deltas up to S
// are out, edits push the log past the connection and change the new tab.
// The connection is resynced, and the capture at S is never sent: the
// client ends with the tab as the engine has it, at the engine's sequence.
func TestSnapshotSupersededByResync(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Second"},{"op":"write","tab":"Second","at":"A1","rows":[["old","stays"]]}]}`)
	s := serve(e)
	first, second := e.tabID("Sheet1"), e.tabID("Second")
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(first)

	big := make([]string, 200)
	for i := range big {
		big[i] = `["` + strings.Repeat("x", 30000) + `"]`
	}
	captured := uint64(0)
	hookOnce(e, map[string]func(){
		// When the pump takes the watch, an edit commits that the
		// connection has not been sent: the snapshot is captured at its
		// sequence S, and the delta of S has to go out before the head.
		"pump": func() {
			e.mustAgent(`{"ops":[{"op":"write","tab":"Second","at":"A1","rows":[["mid"]]}]}`)
			captured = e.seq()
		},
		// Before that delta is out, three edits of 6 MB each push the log
		// past the connection, and the last edit changes the captured tab.
		"snapshot_captured": func() {
			for i := 0; i < 3; i++ {
				e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.Join(big, ",") + `]}]}`)
			}
			e.mustAgent(`{"ops":[{"op":"write","tab":"Second","at":"A1","rows":[["new"]]}]}`)
		},
	})
	r.sync()
	r.mu.Lock()
	r.steps = append(r.steps, replayStep{Open: uint64(second)})
	r.mu.Unlock()
	if err := r.ws.Send(fmt.Sprintf(`{"type":"watch","generation":2,"tab":%d}`, second)); err != nil {
		t.Fatal(err)
	}
	r.until("the snapshot after the resync", func() bool { return r.done >= 2 })

	steps, kinds := r.taken()
	if kinds["resync"] != 1 {
		t.Fatalf("the connection got %d resyncs: %v", kinds["resync"], kinds)
	}
	// No head of the superseded capture is on the wire: every head after
	// the resync is at or after the resync's sequence.
	resynced := uint64(0)
	for _, step := range steps {
		var f struct {
			Type       string
			Seq        string
			Tab        uint64
			Generation int
		}
		_ = json.Unmarshal([]byte(step.Recv), &f)
		seq, _ := parseDecimal(f.Seq)
		switch {
		case f.Type == "resync":
			resynced = seq
			if f.Generation <= 2 {
				t.Fatalf("the resync reuses generation %d of the watch it interrupted", f.Generation)
			}
		case f.Type == "tab_head" && resynced != 0:
			if seq < resynced || seq == captured || f.Tab != uint64(second) || f.Generation <= 2 {
				t.Fatalf("after a resync at %d the connection got the head of tab %d at %d under generation %d", resynced, f.Tab, seq, f.Generation)
			}
		case f.Type == "tab_head" && f.Tab == uint64(second):
			t.Fatalf("the capture at %d went out before the resync", seq)
		}
	}
	if resynced == 0 {
		t.Fatal("no resync")
	}
	// The real client, fed these frames, holds what the engine holds.
	got := replay(t, node, steps)
	if diffs := compareClient(got, e.clientView("Second")); len(diffs) > 0 {
		t.Fatalf("the client differs from the engine:\n%s", strings.Join(diffs, "\n"))
	}
	if drawn := fmt.Sprint(got["drawn"].([]any)[0]); !strings.HasPrefix(drawn, "[new stays") {
		t.Fatalf("the client draws %s", drawn)
	}
}

// TestWaitingWatchThroughClient plays a watch that waited for the capture
// budget into the real client: the status frames around the wait and the
// snapshot that follows leave it with the tab it asked for.
func TestWaitingWatchThroughClient(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Second"},{"op":"write","tab":"Second","at":"A1","rows":[["there",2]]}]}`)
	s := serve(e)
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(e.tabID("Sheet1"))
	release := fillCaptures(t, 0)
	r.sync()
	r.mu.Lock()
	r.steps = append(r.steps, replayStep{Open: uint64(e.tabID("Second"))})
	r.mu.Unlock()
	if err := r.ws.Send(fmt.Sprintf(`{"type":"watch","generation":2,"tab":%d}`, e.tabID("Second"))); err != nil {
		t.Fatal(err)
	}
	statuses := func() (n int) {
		for _, step := range r.steps {
			if strings.HasPrefix(step.Recv, `{"type":"status"`) {
				n++
			}
		}
		return n
	}
	r.until("the waiting status", func() bool { return statuses() == 1 })
	// An edit arrives while the watch waits.
	e.mustAgent(`{"ops":[{"op":"write","tab":"Second","at":"A2","rows":[["later"]]}]}`)
	r.sync()
	release()
	r.until("the snapshot", func() bool { return r.done == 2 })
	steps, kinds := r.taken()
	if kinds["status"] != 2 || kinds["resync"] != 0 {
		t.Fatalf("the connection got %v", kinds)
	}
	if diffs := compareClient(replay(t, node, steps), e.clientView("Second")); len(diffs) > 0 {
		t.Fatalf("the client differs from the engine:\n%s", strings.Join(diffs, "\n"))
	}
}

// TestResyncKeepsTheWantedTab resyncs a connection whose watch of another
// tab is waiting for the capture budget. The resync goes on with the tab
// the client asked for, under a generation above that watch.
func TestResyncKeepsTheWantedTab(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Second"},{"op":"write","tab":"Second","at":"A1","rows":[["wanted"]]}]}`)
	s := serve(e)
	first, second := e.tabID("Sheet1"), e.tabID("Second")
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(first)
	release := fillCaptures(t, 0)
	r.sync()
	r.mu.Lock()
	r.steps = append(r.steps, replayStep{Open: uint64(second)})
	r.mu.Unlock()
	if err := r.ws.Send(fmt.Sprintf(`{"type":"watch","generation":2,"tab":%d}`, second)); err != nil {
		t.Fatal(err)
	}
	count := func(kind string) (n int) {
		for _, step := range r.steps {
			if strings.HasPrefix(step.Recv, `{"type":"`+kind+`"`) {
				n++
			}
		}
		return n
	}
	r.until("the waiting status", func() bool { return count("status") == 1 })
	// The pump stops at its next command while the log runs away from the
	// connection.
	held, proceed := make(chan struct{}), make(chan struct{})
	hookOnce(e, map[string]func(){"pump": func() {
		close(held)
		<-proceed
	}})
	_, row, col := e.ids("A1")
	if err := r.ws.Send(fmt.Sprintf(`{"type":"peek","id":1,"tab":%d,"row":%d,"col":%d}`, first, row, col)); err != nil {
		t.Fatal(err)
	}
	<-held
	big := make([]string, 200)
	for i := range big {
		big[i] = `["` + strings.Repeat("x", 30000) + `"]`
	}
	for i := 0; i < 3; i++ {
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.Join(big, ",") + `]}]}`)
	}
	close(proceed)
	r.until("the resync", func() bool { return count("resync") == 1 })
	release()
	r.until("the snapshot", func() bool { return r.done > 2 })
	steps, kinds := r.taken()
	if kinds["resync"] != 1 {
		t.Fatalf("the connection got %v", kinds)
	}
	for _, step := range steps {
		var f struct {
			Type       string
			Tab        uint64
			Generation int
		}
		_ = json.Unmarshal([]byte(step.Recv), &f)
		if f.Type == "tab_head" && f.Generation > 1 && f.Tab != uint64(second) {
			t.Fatalf("after the watch of tab %d the connection got the head of tab %d under generation %d", second, f.Tab, f.Generation)
		}
	}
	if diffs := compareClient(replay(t, node, steps), e.clientView("Second")); len(diffs) > 0 {
		t.Fatalf("the client differs from the engine:\n%s", strings.Join(diffs, "\n"))
	}
}
