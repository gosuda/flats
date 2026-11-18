package sheet

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

// TestResumedTabNamesMatchTheBase reconnects the real client to the real
// host after another writer swapped the names of two tabs, and lets the
// replayed deltas arrive late. Until they do, the client's tab bar shows
// the names of the sequence the stream goes on from, so a formula typed
// in that time names the tab the person sees under that name. The host
// reads it against the same names, from its base.
func TestResumedTabNamesMatchTheBase(t *testing.T) {
	node := nodeOrSkip(t)
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"Alpha"},{"op":"write","tab":"Alpha","at":"A1","rows":[[111]]},
		{"op":"add_tab","name":"Beta"},{"op":"write","tab":"Beta","at":"A1","rows":[[222]]}]}`)
	alpha, beta := e.tabID("Alpha"), e.tabID("Beta")
	s := serve(e)
	first := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	first.watch(alpha)
	steps, _ := first.taken()
	var welcome struct{ Session string }
	if err := json.Unmarshal([]byte(steps[0].Recv), &welcome); err != nil || welcome.Session == "" {
		t.Fatalf("the first frame is no welcome with a session: %s", steps[0].Recv)
	}
	mark := e.seq()
	first.ws.Close(1000, "")

	// While the page is away another writer swaps the two names.
	bob := e.person("Bob")
	for _, rename := range [][2]any{{alpha, "Temp"}, {beta, "Alpha"}, {alpha, "Beta"}} {
		if ack := bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":%q}`, rename[0], rename[1])); status(ack) != "applied" {
			t.Fatalf("Bob's rename was answered %s", status(ack))
		}
	}

	// The client's next connection resumes from its watermark. The host's
	// welcome is taken first, on its own; the replayed deltas come after
	// the person has typed.
	hello := fmt.Sprintf(`{"type":"hello","version":1,"session":%q,"watermark":"%d","tab":%d,"pending":0}`, welcome.Session, mark, alpha)
	second := s.record("private", hello)
	resumed, kinds := second.taken()
	if kinds["delta"] != 3 {
		t.Fatalf("the resumed connection got %v, want the welcome and three renames: %v", kinds, resumed)
	}
	var w map[string]any
	if err := json.Unmarshal([]byte(resumed[0].Recv), &w); err != nil || w["type"] != "welcome" || w["seq"] != fmt.Sprint(mark) {
		t.Fatalf("the resumed connection began with %s", resumed[0].Recv)
	}
	if _, ok := w["tabs"]; ok {
		t.Errorf("the welcome of a replayed stream carries the tab list of a later sequence: %s", resumed[0].Recv)
	}
	if _, ok := w["date"]; ok {
		t.Errorf("the welcome of a replayed stream carries the date of a later sequence: %s", resumed[0].Recv)
	}

	run := play(t, node, steps,
		map[string]any{"drop": true},
		map[string]any{"steps": resumed[:1]},
		map[string]any{"look": true},
		map[string]any{"select": []int{0, 1, 0, 1}},
		map[string]any{"type": "=Beta!A1"},
		map[string]any{"key": "Enter"},
		map[string]any{"steps": resumed[1:]},
		map[string]any{"look": true},
	)
	if len(run.Hellos) != 2 {
		t.Fatalf("the client sent %d hellos, want 2", len(run.Hellos))
	}
	if h := run.Hellos[1]; h["session"] != welcome.Session || h["watermark"] != fmt.Sprint(mark) || h["tab"] != float64(alpha) {
		t.Fatalf("the client resumed with %v, the recording with %s", h, hello)
	}
	if len(run.Looks) != 2 || len(run.Frames) != 1 {
		t.Fatalf("the client noted %v and sent %s", run.Looks, run.Frames)
	}
	if want := []string{"Alpha", "Beta"}; !slices.Equal(run.Looks[0], want) {
		t.Errorf("before the replay the tab bar shows %v, want %v, the names of sequence %d", run.Looks[0], want, mark)
	}
	if want := []string{"Beta", "Alpha"}; !slices.Equal(run.Looks[1], want) {
		t.Errorf("after the replay the tab bar shows %v, want %v", run.Looks[1], want)
	}
	// The tab the person meant is the one the tab bar named Beta, and the
	// A1 they meant is that tab's.
	meant := []TabID{alpha, beta}[slices.Index(run.Looks[0], "Beta")]
	want := map[TabID]string{alpha: "111", beta: "222"}[meant]

	ack := second.answer(run.Frames[0])
	if len(ack.Results) != 1 || ack.Results[0].Status != "applied" {
		t.Fatalf("the formula was answered %+v", ack.Results)
	}
	// Tab 1 is now called Beta and tab 2 Alpha.
	if got := e.shown("Beta!B1"); got != want {
		t.Errorf("the formula typed as =Beta!A1 shows %q, want %q, the A1 of the tab the screen called Beta", got, want)
	}
	if got := e.text("Beta!B1"); got != "=Alpha!A1" {
		t.Errorf("the formula reads %q, want =Alpha!A1, the name the meant tab has now", got)
	}
}
