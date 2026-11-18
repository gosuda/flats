package sheet

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// ask sends a request frame on the recording connection and waits for the
// answer of the given kind that carries its id.
func (r *recorder) ask(frame, kind string, id int) {
	r.t.Helper()
	if err := r.ws.Send(frame); err != nil {
		r.t.Fatal(err)
	}
	r.until(fmt.Sprintf("the %s answer %d", kind, id), func() bool {
		for _, s := range r.steps {
			var f struct {
				Type string
				ID   int
			}
			if json.Unmarshal([]byte(s.Recv), &f) == nil && f.Type == kind && f.ID == id {
				return true
			}
		}
		return false
	})
}

// count is how many steps the recording holds.
func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.steps)
}

// swapTabNames has another person swap the names of two tabs through a
// third name, so each name is taken again by the other tab.
func swapTabNames(t *testing.T, e *env, a, b TabID) {
	t.Helper()
	bob := e.person("Bob")
	for _, rename := range [][2]any{{a, "Temp"}, {b, "Alpha"}, {a, "Beta"}} {
		if ack := bob.do(jsonf(`{"op":"rename_tab","tab":%d,"name":%q}`, rename[0], rename[1])); status(ack) != "applied" {
			t.Fatalf("Bob's rename was answered %s", status(ack))
		}
	}
}

// splitAtOpen cuts recorded steps at the moment the recording connection
// opened another tab: the steps before it and the steps after it.
func splitAtOpen(t *testing.T, steps []replayStep) (before, after []replayStep) {
	t.Helper()
	for i, s := range steps {
		if s.Open != 0 {
			return steps[:i], steps[i+1:]
		}
	}
	t.Fatal("the recording opened no other tab")
	return nil, nil
}

// checkHeads checks that the recording holds one head and that it
// carries the tab list and the date the engine has now. They are those of
// the head's sequence, as the recordings below change nothing after their
// last watch.
func checkHeads(t *testing.T, e *env, steps []replayStep) {
	t.Helper()
	want := e.clientView("Alpha")
	heads := 0
	for _, s := range steps {
		var f map[string]any
		if err := json.Unmarshal([]byte(s.Recv), &f); err != nil || f["type"] != "tab_head" {
			continue
		}
		heads++
		if !reflect.DeepEqual(f["tabs"], want["tabs"]) || f["date"] != want["date"] {
			t.Errorf("the head carries the tabs %v and the date %v, the engine has %v and %v", f["tabs"], f["date"], want["tabs"], want["date"])
		}
	}
	if heads != 1 {
		t.Fatalf("the recording holds %d heads after the switch, want 1", heads)
	}
}

// checkFormulaTarget checks that the tab bar shows the names of the
// snapshot after the switch. The person then types =Beta!A1 in B1 of the
// tab on screen, which is now called Alpha, and the host reads the A1 of
// the tab the tab bar calls Beta.
func checkFormulaTarget(t *testing.T, e *env, r *recorder, run clientRun, alpha, beta TabID) {
	t.Helper()
	if len(run.Looks) != 1 || len(run.Frames) != 1 {
		t.Fatalf("the client noted %v and sent %s", run.Looks, run.Frames)
	}
	if want := []string{"Beta", "Alpha"}; !slices.Equal(run.Looks[0], want) {
		t.Errorf("after the snapshot the tab bar shows %v, want %v, the names of the snapshot", run.Looks[0], want)
	}
	meant := []TabID{alpha, beta}[slices.Index(run.Looks[0], "Beta")]
	want := map[TabID]string{alpha: "111", beta: "222"}[meant]
	ack := r.answer(run.Frames[0])
	if len(ack.Results) != 1 || ack.Results[0].Status != "applied" {
		t.Fatalf("the formula was answered %+v", ack.Results)
	}
	if got := e.shown("Alpha!B1"); got != want {
		t.Errorf("the formula typed as =Beta!A1 shows %q, want %q, the A1 of the tab the screen called Beta", got, want)
	}
}

// TestPendingPeekAcrossTabSwitchKeepsTabNames opens a formula while its
// text is still asked for, then switches tabs before the answer comes.
// The answer opens the editor on the tab being left, so the renames that
// follow are held for it, and the snapshot of the other tab drops them.
// That snapshot's head carries the tab list of its sequence, so the tab
// bar shows the names the grid has, and a formula typed after it names the
// tab the person sees under that name.
func TestPendingPeekAcrossTabSwitchKeepsTabNames(t *testing.T) {
	node := nodeOrSkip(t)
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"Alpha"},{"op":"write","tab":"Alpha","at":"A1","rows":[[111],["=A1+1"]]},
		{"op":"add_tab","name":"Beta"},{"op":"write","tab":"Beta","at":"A1","rows":[[222]]}]}`)
	alpha, beta := e.tabID("Alpha"), e.tabID("Beta")
	_, row, col := e.ids("Alpha!A2")
	s := serve(e)
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(alpha)
	steps, _ := r.taken()

	// The peek the client sends when the editor opens on A2, answered at
	// the sequence of the snapshot. Then the renames, and the watch of the
	// other tab, which the client sent before the answer arrived.
	peek := fmt.Sprintf(`{"type":"peek","id":1,"tab":%d,"row":%d,"col":%d}`, alpha, row, col)
	r.ask(peek, "peeked", 1)
	swapTabNames(t, e, alpha, beta)
	r.watch(beta)
	all, _ := r.taken()
	answer, snapshot := splitAtOpen(t, all[len(steps):])
	checkHeads(t, e, snapshot)

	run := play(t, node, steps,
		map[string]any{"select": []int{1, 0, 1, 0}},
		map[string]any{"key": "F2"},
		map[string]any{"steps": []replayStep{{Open: uint64(beta)}}},
		map[string]any{"steps": answer},
		map[string]any{"steps": snapshot},
		map[string]any{"look": true},
		map[string]any{"select": []int{0, 1, 0, 1}},
		map[string]any{"type": "=Beta!A1"},
		map[string]any{"key": "Enter"},
	)
	// The client asked what the recording answered, in that order, and
	// held the renames for the editor the answer opened.
	var sent map[string]any
	_ = json.Unmarshal([]byte(peek), &sent)
	kinds := []string{}
	for _, f := range run.Requests {
		kinds = append(kinds, fmt.Sprint(f["type"]))
	}
	if !slices.Equal(kinds, []string{"peek", "watch", "rebase"}) || !reflect.DeepEqual(roundTrip(run.Requests[0]), roundTrip(sent)) {
		t.Fatalf("the client sent %v, want the recorded peek, the watch and a rebase for the held renames", run.Requests)
	}
	if !slices.ContainsFunc(run.Kept, func(k *string) bool { return k != nil && *k == "=A1+1" }) {
		t.Errorf("the snapshot did not close the editor the answer opened: notices %v", run.Notices)
	}
	checkFormulaTarget(t, e, r, run, alpha, beta)
}

// TestReadOnlyRebaseAcrossTabSwitchKeepsTabNames switches tabs while the
// editor is read-only between two rebase exchanges. Switching then commits
// nothing and leaves the editor open, so the renames it holds are dropped
// by the snapshot of the other tab, which arrives before the answer to the
// last exchange. The snapshot's head brings the names of its sequence.
func TestReadOnlyRebaseAcrossTabSwitchKeepsTabNames(t *testing.T) {
	node := nodeOrSkip(t)
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"Alpha"},{"op":"write","tab":"Alpha","at":"A1","rows":[[111]]},
		{"op":"add_tab","name":"Beta"},{"op":"write","tab":"Beta","at":"A1","rows":[[222]]}]}`)
	alpha, beta := e.tabID("Alpha"), e.tabID("Beta")
	_, row, col := e.ids("Alpha!C3")
	s := serve(e)
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(alpha)
	steps, _ := r.taken()
	base := fmt.Sprint(e.seq())

	// A structural change the editor holds, then the two exchanges for it:
	// the first with the text as it was, the second with the text typed
	// meanwhile. Between them the names are swapped.
	if ack := e.person("Bob").do(e.insertRowsOp("Beta", 1, 1)); status(ack) != "applied" {
		t.Fatalf("Bob's insert was answered %s", status(ack))
	}
	r.sync()
	through := fmt.Sprint(e.seq())
	rebase := func(id int, text string) string {
		return fmt.Sprintf(`{"type":"rebase","id":%d,"tab":%d,"row":%d,"col":%d,"text":%q,"at":[2,2],"base":%q,"through":%q}`, id, alpha, row, col, text, base, through)
	}
	r.ask(rebase(1, "="), "rebased", 1)
	inserted := r.count()
	swapTabNames(t, e, alpha, beta)
	r.sync()
	renamed := r.count()
	r.ask(rebase(2, "=1"), "rebased", 2)
	answered := r.count()
	r.watch(beta)
	all, _ := r.taken()
	_, snapshot := splitAtOpen(t, all[answered:])
	checkHeads(t, e, snapshot)

	run := play(t, node, steps,
		map[string]any{"select": []int{2, 2, 2, 2}},
		map[string]any{"type": "="},
		map[string]any{"steps": all[len(steps) : inserted-1]},
		map[string]any{"type": "1"},
		// Long enough that the next exchange makes the editor read-only.
		map[string]any{"wait": 600},
		map[string]any{"steps": all[inserted-1 : inserted]},
		map[string]any{"steps": all[inserted:renamed]},
		map[string]any{"steps": []replayStep{{Open: uint64(beta)}}},
		map[string]any{"steps": all[renamed:answered]},
		map[string]any{"steps": snapshot},
		map[string]any{"look": true},
		map[string]any{"select": []int{0, 1, 0, 1}},
		map[string]any{"type": "=Beta!A1"},
		map[string]any{"key": "Enter"},
	)
	// The client sent the two recorded exchanges, then the watch, which
	// committed nothing, then a third exchange for the held renames that
	// the snapshot overtook.
	var kinds []string
	for _, f := range run.Requests {
		kinds = append(kinds, fmt.Sprint(f["type"]))
	}
	if !slices.Equal(kinds, []string{"rebase", "rebase", "watch", "rebase"}) {
		t.Fatalf("the client sent %v", run.Requests)
	}
	for i, text := range []string{"=", "=1"} {
		var want map[string]any
		_ = json.Unmarshal([]byte(rebase(i+1, text)), &want)
		if got := roundTrip(run.Requests[i]); !reflect.DeepEqual(got, roundTrip(want)) {
			t.Fatalf("the client's exchange %d was %v, the recording answered %v", i+1, got, want)
		}
	}
	checkFormulaTarget(t, e, r, run, alpha, beta)
}

// commitGestures are the ways a person ends an edit with a commit: Enter,
// Tab and a press outside the grid and the formula bar.
var commitGestures = map[string]map[string]any{
	"Enter":   {"key": "Enter"},
	"Tab":     {"key": "Tab"},
	"outside": {"outside": true},
}

// checkTextKept checks that a commit the client could not send while the
// other tab loaded wrote nothing and left the editor open with its text,
// and that the snapshot then closed the editor and offered that text back.
func checkTextKept(t *testing.T, run clientRun, text string) {
	t.Helper()
	if len(run.Frames) != 0 {
		t.Fatalf("the client wrote %s while the other tab loaded", run.Frames)
	}
	var edits []any
	for _, e := range run.Edits {
		if e == nil {
			edits = append(edits, nil)
		} else {
			edits = append(edits, *e)
		}
	}
	if want := []any{text, text, nil}; !reflect.DeepEqual(edits, want) {
		t.Errorf("the editor held %v before the commit, after it and after the snapshot, want %v", edits, want)
	}
	if !slices.ContainsFunc(run.Kept, func(k *string) bool { return k != nil && *k == text }) {
		t.Errorf("the text %q was not offered back: notices %v", text, run.Notices)
	}
}

// TestRefusedCommitKeepsTextOfPeekAcrossTabSwitch starts an edit in place
// on a formula whose text is still asked for, types after it and switches
// tabs. The answer arrives before the other tab's snapshot and opens the
// editor on the tab being left. A commit then cannot be sent, as nothing
// is written into a tab the person left: the editor stays open with the
// formula and what was typed after it, and the snapshot offers that text
// back.
func TestRefusedCommitKeepsTextOfPeekAcrossTabSwitch(t *testing.T) {
	node := nodeOrSkip(t)
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"Alpha"},{"op":"write","tab":"Alpha","at":"A1","rows":[["=1"]]},
		{"op":"add_tab","name":"Beta"},{"op":"write","tab":"Beta","at":"A1","rows":[[222]]}]}`)
	alpha, beta := e.tabID("Alpha"), e.tabID("Beta")
	_, row, col := e.ids("Alpha!A1")
	s := serve(e)
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(alpha)
	steps, _ := r.taken()
	peek := fmt.Sprintf(`{"type":"peek","id":1,"tab":%d,"row":%d,"col":%d}`, alpha, row, col)
	r.ask(peek, "peeked", 1)
	r.watch(beta)
	all, _ := r.taken()
	answer, snapshot := splitAtOpen(t, all[len(steps):])

	for name, commit := range commitGestures {
		t.Run(name, func(t *testing.T) {
			run := play(t, node, steps,
				map[string]any{"select": []int{0, 0, 0, 0}},
				map[string]any{"key": "F2"},
				map[string]any{"type": "+2"},
				map[string]any{"steps": []replayStep{{Open: uint64(beta)}}},
				map[string]any{"steps": answer},
				map[string]any{"edit": true},
				commit,
				map[string]any{"edit": true},
				map[string]any{"steps": snapshot},
				map[string]any{"edit": true},
			)
			var kinds []string
			for _, f := range run.Requests {
				kinds = append(kinds, fmt.Sprint(f["type"]))
			}
			if !slices.Equal(kinds, []string{"peek", "watch"}) {
				t.Fatalf("the client sent %v, want the recorded peek and the watch", run.Requests)
			}
			checkTextKept(t, run, "=1+2")
		})
	}
}

// TestRefusedCommitKeepsTextOfReadOnlyRebaseAcrossTabSwitch switches tabs
// while the editor is read-only between two rebase exchanges, so the
// switch commits nothing. The answer to the last exchange arrives before
// the other tab's snapshot and makes the editor editable again. A commit
// then cannot be sent: the editor keeps its text, and the snapshot offers
// it back.
func TestRefusedCommitKeepsTextOfReadOnlyRebaseAcrossTabSwitch(t *testing.T) {
	node := nodeOrSkip(t)
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"Alpha"},{"op":"write","tab":"Alpha","at":"A1","rows":[[111]]},
		{"op":"add_tab","name":"Beta"},{"op":"write","tab":"Beta","at":"A1","rows":[[222]]}]}`)
	alpha, beta := e.tabID("Alpha"), e.tabID("Beta")
	_, row, col := e.ids("Alpha!C3")
	s := serve(e)
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(alpha)
	steps, _ := r.taken()
	base := fmt.Sprint(e.seq())

	// A structural change the editor holds and the two exchanges for it:
	// the first with the text as it was, the second with the text typed
	// meanwhile, which the read-only editor keeps until it is answered.
	if ack := e.person("Bob").do(e.insertRowsOp("Beta", 1, 1)); status(ack) != "applied" {
		t.Fatalf("Bob's insert was answered %s", status(ack))
	}
	r.sync()
	through := fmt.Sprint(e.seq())
	rebase := func(id int, text string) string {
		return fmt.Sprintf(`{"type":"rebase","id":%d,"tab":%d,"row":%d,"col":%d,"text":%q,"at":[2,2],"base":%q,"through":%q}`, id, alpha, row, col, text, base, through)
	}
	r.ask(rebase(1, "="), "rebased", 1)
	inserted := r.count()
	r.ask(rebase(2, "=1"), "rebased", 2)
	answered := r.count()
	r.watch(beta)
	all, _ := r.taken()
	_, snapshot := splitAtOpen(t, all[answered:])

	for name, commit := range commitGestures {
		t.Run(name, func(t *testing.T) {
			run := play(t, node, steps,
				map[string]any{"select": []int{2, 2, 2, 2}},
				map[string]any{"type": "="},
				map[string]any{"steps": all[len(steps) : inserted-1]},
				map[string]any{"type": "1"},
				// Long enough that the next exchange makes the editor read-only.
				map[string]any{"wait": 600},
				map[string]any{"steps": all[inserted-1 : inserted]},
				map[string]any{"steps": []replayStep{{Open: uint64(beta)}}},
				map[string]any{"steps": all[inserted:answered]},
				map[string]any{"edit": true},
				commit,
				map[string]any{"edit": true},
				map[string]any{"steps": snapshot},
				map[string]any{"edit": true},
			)
			var kinds []string
			for _, f := range run.Requests {
				kinds = append(kinds, fmt.Sprint(f["type"]))
			}
			if !slices.Equal(kinds, []string{"rebase", "rebase", "watch"}) {
				t.Fatalf("the client sent %v, want the two recorded exchanges and the watch", run.Requests)
			}
			checkTextKept(t, run, "=1")
		})
	}
}

// roundTrip gives a frame as JSON decodes it, so numbers compare alike.
func roundTrip(v any) any {
	data, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(data, &out)
	return out
}

// TestHeadTabListStaysInItsReservation fills a sheet with as many tabs as
// it may have, each with the longest name it may have in the characters
// that take the most bytes as JSON, and captures each tab. The head's
// buffer, which was charged to the capture before it was made, never
// grows, and it carries the whole tab list and the date.
func TestHeadTabListStaysInItsReservation(t *testing.T) {
	e := start(t)
	name := func(i int) string {
		// A quote takes two bytes as JSON, an emoji four.
		return strings.Repeat("\"😀", maxTabName/2-1) + string(rune('A'+i/26)) + string(rune('a'+i%26))
	}
	ops := []string{fmt.Sprintf(`{"op":"rename_tab","tab":"Sheet1","name":%s}`, quoted(name(0)))}
	for i := 1; i < maxTabs; i++ {
		ops = append(ops, fmt.Sprintf(`{"op":"add_tab","name":%s}`, quoted(name(i))))
	}
	e.mustAgent(`{"ops":[` + strings.Join(ops, ",") + `]}`)
	want := e.clientView(name(0))
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	wb := e.in.wb
	if len(wb.tabs) != maxTabs {
		t.Fatalf("the sheet has %d tabs, want %d", len(wb.tabs), maxTabs)
	}
	for _, tab := range wb.tabs {
		c, head, err := wb.captureTab(tab)
		if err != nil {
			t.Fatal(err)
		}
		made := cap(head.fixed)
		var fields map[string]any
		err = json.Unmarshal([]byte(`{"tab":1`+string(head.fixed)+`}`), &fields)
		c.release()
		if err != nil {
			t.Fatalf("the head's fields do not read as JSON: %v", err)
		}
		if made != 64*tab.cols.len()+256+headTabsSize(wb) || len(head.fixed) > made {
			t.Fatalf("the head's buffer holds %d bytes in %d, made for %d", len(head.fixed), made, 64*tab.cols.len()+256+headTabsSize(wb))
		}
		if !reflect.DeepEqual(fields["tabs"], want["tabs"]) || fields["date"] != want["date"] {
			t.Fatalf("the head carries the tabs %v and the date %v, the engine has %v and %v", fields["tabs"], fields["date"], want["tabs"], want["date"])
		}
	}
}

// quoted is a text as JSON.
func quoted(text string) string {
	data, _ := json.Marshal(text)
	return string(data)
}
