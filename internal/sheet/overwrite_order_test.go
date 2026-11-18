package sheet

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestOverwriteNoticeBeforeItsAckIsOffered types "warm" into B1 in the
// real client, and then types "mine" into A1 or pastes "first" and "mine"
// at the last row, which adds a row for "mine". It sends both actions on a
// recorded connection to the real engine. The connection's pump stops as
// it takes the first ack. Meanwhile the second action commits at S, an
// agent replaces the cell that holds "mine", and agent output larger than
// the delta log pushes S out of it. When the pump goes on, it resyncs the
// connection past S, so the client never gets the delta of S, and it
// sends the overwrite notice of S before the second ack, which waited in
// the connection's queue. The client, fed these frames in that order,
// offers "mine" in the notice once the ack files it, under the id the ack
// names for an added row.
func TestOverwriteNoticeBeforeItsAckIsOffered(t *testing.T) {
	node := nodeOrSkip(t)
	cases := []struct {
		name, addr string
		second     []any
	}{
		{"typed", "A1", []any{map[string]any{"select": []int{0, 0, 0, 0}}, map[string]any{"type": "mine"}, map[string]any{"key": "Enter"}}},
		{"pasted into an added row", fmt.Sprintf("A%d", newTabRows+1), []any{
			map[string]any{"select": []int{newTabRows - 1, 0, newTabRows - 1, 0}}, map[string]any{"paste": "all", "text": "first\nmine"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { overwriteBeforeAck(t, node, c.addr, c.second) })
	}
}

func overwriteBeforeAck(t *testing.T, node, addr string, second []any) {
	t.Helper()
	e := start(t)
	s := serve(e)
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(e.tabID("Sheet1"))
	steps, _ := r.taken()
	gs := append([]any{map[string]any{"select": []int{0, 1, 0, 1}}, map[string]any{"type": "warm"}, map[string]any{"key": "Enter"}}, second...)
	run := play(t, node, steps, gs...)
	if len(run.Frames) != 2 {
		t.Fatalf("the client sent %s, want two actions", run.Frames)
	}

	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	hookOnce(e, map[string]func(){"pump": func() {
		close(held)
		<-release
	}})
	base := e.seq()
	if err := r.ws.Send(string(run.Frames[0])); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the pump did not take the first ack")
	}
	if err := r.ws.Send(string(run.Frames[1])); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); e.seq() < base+2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the second action did not commit")
		}
	}
	committed := base + 2
	e.wantText(addr, "mine")
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"` + addr + `","rows":[["agent"]]}]}`)
	big := make([]string, 200)
	for i := range big {
		big[i] = `["` + strings.Repeat("x", 30000) + `"]`
	}
	for i := 0; i < 3; i++ {
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"C1","rows":[` + strings.Join(big, ",") + `]}]}`)
	}
	free()

	type frame struct {
		Type     string
		N        string
		Replaced string
		From     *struct{ N string }
	}
	read := func(st replayStep) frame {
		var f frame
		_ = json.Unmarshal([]byte(st.Recv), &f)
		return f
	}
	r.until("the second ack", func() bool {
		for _, st := range r.steps {
			if f := read(st); f.Type == "ack" && f.N == "2" {
				return true
			}
		}
		return false
	})
	got, _ := r.taken()
	later := got[len(steps):]
	notice, ack, resync := -1, -1, -1
	for i, st := range later {
		f := read(st)
		switch {
		case f.Type == "resync" && resync < 0:
			resync = i
		case f.Type == "overwritten" && f.Replaced == fmt.Sprint(committed):
			notice = i
		case f.Type == "ack" && f.N == "2":
			ack = i
		case f.Type == "delta" && f.From != nil && f.From.N == "2":
			t.Fatalf("the client got the delta of the second action: %.200s", st.Recv)
		}
	}
	if resync < 0 || notice < resync || ack < notice {
		t.Fatalf("the frames came as resync %d, notice %d, ack %d; want the resync, then the notice of %d, then the ack", resync, notice, ack, committed)
	}

	answered := play(t, node, steps, append(gs, map[string]any{"steps": later})...)
	var offered []any
	for i, text := range answered.Notices {
		if strings.HasPrefix(text, "Someone changed") {
			if k := answered.Kept[i]; k != nil {
				offered = append(offered, *k)
			} else {
				offered = append(offered, nil)
			}
		}
	}
	if fmt.Sprint(offered) != "[mine]" {
		t.Fatalf("the overwrite notices offer %v, want one that offers mine: %q", offered, answered.Notices)
	}
}
