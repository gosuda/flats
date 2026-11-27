package sheet

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestRebuiltFormatListKeepsTabNames swaps the names of two tabs while the
// real client's editor holds a formula, so the client holds the renames
// back, and then rebuilds the wire format list. The host's resync drops
// what the client held. It carries the tab list and the date of the
// sequence it names, so the tab bar shows the names of the snapshot that
// follows, and a formula typed after it names the tab the person sees
// under that name. The host reads it against the same names.
func TestRebuiltFormatListKeepsTabNames(t *testing.T) {
	node := nodeOrSkip(t)
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"Alpha"},{"op":"write","tab":"Alpha","at":"A1","rows":[[111]]},
		{"op":"add_tab","name":"Beta"},{"op":"write","tab":"Beta","at":"A1","rows":[[222]]}]}`)
	alpha, beta := e.tabID("Alpha"), e.tabID("Beta")
	s := serve(e)
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(alpha)
	steps, _ := r.taken()

	// Another writer swaps the two names, each name taken again by the
	// other tab.
	bob := e.person("Bob")
	for _, rename := range [][2]any{{alpha, "Temp"}, {beta, "Alpha"}, {alpha, "Beta"}} {
		if ack := bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":%q}`, rename[0], rename[1])); status(ack) != "applied" {
			t.Fatalf("Bob's rename was answered %s", status(ack))
		}
	}
	// New formats, each replacing the last one of the cell, until the host
	// rebuilds the format list under the connection.
	e.in.hub.mu.Lock()
	epoch := e.in.hub.wireEpoch
	e.in.hub.mu.Unlock()
	for call := 0; ; call++ {
		if call == 12 {
			t.Fatal("the format list was not rebuilt")
		}
		ops := make([]string, 0, 250)
		for i := range 250 {
			n := call*250 + i
			ops = append(ops, fmt.Sprintf(`{"op":"format","tab":"Alpha","ranges":["C1"],"format":{"number":{"kind":"currency","symbol":"%c%c%c","decimals":2}}}`, 'A'+n%26, 'A'+n/26%26, 'A'+n/676%26))
		}
		e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
		e.in.hub.mu.Lock()
		rebuilt := e.in.hub.wireEpoch != epoch
		e.in.hub.mu.Unlock()
		if rebuilt {
			break
		}
	}
	r.until("the snapshot after the rebuilt list", func() bool { return r.done > 1 })
	all, kinds := r.taken()
	later := all[len(steps):]
	if kinds["resync"] != 1 {
		t.Fatalf("the connection got %v, want one resync", kinds)
	}
	for _, step := range later {
		var f map[string]any
		if err := json.Unmarshal([]byte(step.Recv), &f); err != nil || f["type"] != "resync" {
			continue
		}
		want := e.clientView("Alpha")
		if !reflect.DeepEqual(f["tabs"], want["tabs"]) || f["date"] != want["date"] {
			t.Errorf("the resync carries the tabs %v and the date %v, the engine has %v and %v", f["tabs"], f["date"], want["tabs"], want["date"])
		}
	}

	run := play(t, node, steps,
		map[string]any{"select": []int{0, 1, 0, 1}},
		map[string]any{"type": "="},
		map[string]any{"steps": later},
		map[string]any{"look": true},
		map[string]any{"select": []int{0, 1, 0, 1}},
		map[string]any{"type": "=Beta!A1"},
		map[string]any{"key": "Enter"},
	)
	if len(run.Looks) != 1 || len(run.Frames) != 1 {
		t.Fatalf("the client noted %v and sent %s", run.Looks, run.Frames)
	}
	if want := []string{"Beta", "Alpha"}; !slices.Equal(run.Looks[0], want) {
		t.Errorf("after the resync the tab bar shows %v, want %v, the names of the snapshot", run.Looks[0], want)
	}
	// The tab the person meant is the one the tab bar named Beta, and the
	// A1 they meant is that tab's.
	meant := []TabID{alpha, beta}[slices.Index(run.Looks[0], "Beta")]
	want := map[TabID]string{alpha: "111", beta: "222"}[meant]

	ack := r.answer(run.Frames[0])
	if len(ack.Results) != 1 || ack.Results[0].Status != "applied" {
		t.Fatalf("the formula was answered %+v", ack.Results)
	}
	// Tab 1 is now called Beta.
	if got := e.shown("Beta!B1"); got != want {
		t.Errorf("the formula typed as =Beta!A1 shows %q, want %q, the A1 of the tab the screen called Beta", got, want)
	}
}
