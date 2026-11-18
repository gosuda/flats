package sheet

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// TestDelayedFormulaPasteReopensFromItsBase chooses Paste values only on
// A3 in the real client while the clipboard read of =B3+1 waits, and
// another person sorts the row of A3 to row 4 before the text arrives. The
// paste is sent with the base of the menu's screen, and its pending text
// keeps the position the cell had on that screen. The person reopens the
// cell where it is now before the paste is confirmed and types +1 while
// the real engine adjusts the pending text from that base. The engine
// stores the adjusted text with +1 after it.
func TestDelayedFormulaPasteReopensFromItsBase(t *testing.T) {
	node := nodeOrSkip(t)
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B2","rows":[[40],[30],[20],[10]]}]}`)
	tab, row, col := e.ids("Sheet1!A3")
	before := e.place(tab, row, col)
	s := serve(e)
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(tab)
	steps, _ := r.taken()
	base := e.seq()
	// The row of A3 holds 30 and moves to row 4 with it.
	changed := recordedSince(r, func() {
		if ack := e.person("Bob").do(e.sortOp("Sheet1", 2, 4, 2, false)); status(ack) != "applied" {
			t.Fatalf("the sort was answered %s", status(ack))
		}
	})
	after := e.place(tab, row, col)
	if reflect.DeepEqual(before, after) {
		t.Fatalf("the sort left A3 at %v", after)
	}
	paste := fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"write","tab":%d,"rows":[%d],"cols":[%d],"values":[["=B3+1"]]}]}`, base, tab, row, col)
	rebase := fmt.Sprintf(`{"type":"rebase","id":1,"tab":%d,"row":%d,"col":%d,"text":"=B3+1","at":[%d,%d],"base":"%d","through":"%d"}`, tab, row, col, before[0], before[1], base, e.seq())
	r.ask(rebase, "rebased", 1)
	got, _ := r.taken()
	rebased := got[len(steps)+len(changed):]
	var answer struct {
		Text    string
		Refused string
	}
	if err := json.Unmarshal([]byte(rebased[len(rebased)-1].Recv), &answer); err != nil || answer.Text == "" {
		t.Fatalf("the engine's rebase answer %s: %v", rebased[len(rebased)-1].Recv, err)
	}
	if ack := r.answer(json.RawMessage(paste)); status(ack) != "applied" {
		t.Fatalf("the paste was answered %s", status(ack))
	}
	got, _ = r.taken()
	confirmed := got[len(steps)+len(changed)+len(rebased):]
	addr := fmt.Sprintf("Sheet1!%s%d", formula.ColumnName(int32(after[1])), after[0]+1)
	if adjusted := e.text(addr); adjusted != answer.Text {
		t.Fatalf("the paste stored %q and the exchange gave %q", adjusted, answer.Text)
	}

	run := play(t, node, steps,
		map[string]any{"select": []int{before[0], before[1], before[0], before[1]}},
		map[string]any{"clipboard": "=B3+1"},
		map[string]any{"menu": before},
		map[string]any{"click": "Paste values only"},
		map[string]any{"steps": changed},
		map[string]any{"read": true},
		map[string]any{"select": []int{after[0], after[1], after[0], after[1]}},
		map[string]any{"key": "F2"},
		map[string]any{"edit": true},
		map[string]any{"type": "+1"},
		map[string]any{"steps": rebased},
		map[string]any{"edit": true},
		map[string]any{"steps": confirmed},
		map[string]any{"edit": true},
		map[string]any{"key": "Enter"},
	)
	if !sameFrame(t, sentRequest(t, run, "rebase", 1), rebase) {
		t.Fatalf("the client's exchange differs from the recorded one, whose base and position name the same screen: %v", run.Requests)
	}
	if want := []any{nil, answer.Text + "+1", answer.Text + "+1"}; !reflect.DeepEqual(edited(run), want) {
		t.Fatalf("the editor held %v, want %v", edited(run), want)
	}
	if len(run.Notices) != 0 {
		t.Fatalf("notices %q", run.Notices)
	}
	if len(run.Frames) != 2 || !sameFrame(t, string(run.Frames[0]), paste) {
		t.Fatalf("the client sent %s, want the paste as recorded and one more", run.Frames)
	}
	var second struct{ Base string }
	_ = json.Unmarshal(run.Frames[1], &second)
	if second.Base != strconv.FormatUint(e.seq(), 10) {
		t.Fatalf("the second edit has base %s, want %d", second.Base, e.seq())
	}
	if ack := r.answer(run.Frames[1]); status(ack) != "applied" {
		t.Fatalf("the second edit was answered %s", status(ack))
	}
	e.wantText(addr, "=B4+1+1")
	e.want(addr, "32")
}

// TestDelayedPasteWithoutItsTabOffersTheText chooses Paste values only on
// A1 in the real client while the clipboard read waits. Before the text
// arrives, another person deletes the tab, or the person opens another
// tab, whose snapshot has arrived or not yet. The text is written to no
// cell, and the notice offers back the text that was read: the clipboard
// may hold other text by then.
func TestDelayedPasteWithoutItsTabOffersTheText(t *testing.T) {
	node := nodeOrSkip(t)
	cases := []struct {
		name   string
		change func(e *env, r *recorder, steps int) []replayStep
	}{
		{"tab deleted", func(e *env, r *recorder, _ int) []replayStep {
			return recordedSince(r, func() {
				if ack := e.person("Bob").do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Sheet1"))); status(ack) != "applied" {
					e.t.Fatalf("the deletion was answered %s", status(ack))
				}
			})
		}},
		{"another tab open", func(e *env, r *recorder, steps int) []replayStep {
			r.watch(e.tabID("Data"))
			got, _ := r.taken()
			return got[steps:]
		}},
		{"another tab asked for", func(e *env, _ *recorder, _ int) []replayStep {
			return []replayStep{{Open: uint64(e.tabID("Data"))}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"add_tab","name":"Data"},{"op":"write","tab":"Sheet1","at":"A1","rows":[["old"]]}]}`)
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(e.tabID("Sheet1"))
			steps, _ := r.taken()
			changed := c.change(e, r, len(steps))
			run := play(t, node, steps,
				map[string]any{"select": []int{0, 0, 0, 0}},
				map[string]any{"clipboard": "X\tP"},
				map[string]any{"menu": []int{0, 0}},
				map[string]any{"click": "Paste values only"},
				map[string]any{"steps": changed},
				map[string]any{"read": true},
			)
			if len(run.Frames) != 0 {
				t.Fatalf("the client sent %s", run.Frames)
			}
			if k := kept(run); len(k) == 0 || k[len(k)-1] != "X\tP" {
				t.Fatalf("the notices %q offer back %q, want the text that was read", run.Notices, k)
			}
			if c.name != "tab deleted" {
				e.wantText("Sheet1!A1", "old")
			}
		})
	}
}

// TestPastedTextIsOfferedWhenOverwritten pastes text from another program
// into A1, or into A1:B1 after another person deleted column B, in the
// real client, and the real engine applies it. An agent then replaces A1.
// The overwrite notice offers the pasted text of A1, which the paste's
// delta filed under the sequence it committed at.
func TestPastedTextIsOfferedWhenOverwritten(t *testing.T) {
	node := nodeOrSkip(t)
	cases := []struct {
		name, text string
		width      int
		deleted    bool
	}{
		{"one cell", "mine", 1, false},
		{"a cell of a paste whose other column was deleted", "mine\tyours", 2, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["old","old"]]}]}`)
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(e.tabID("Sheet1"))
			steps, _ := r.taken()
			gs := []any{
				map[string]any{"select": []int{0, 0, 0, c.width - 1}},
				map[string]any{"paste": "all", "text": c.text},
			}
			run := play(t, node, steps, gs...)
			if len(run.Frames) != 1 {
				t.Fatalf("the client sent %s", run.Frames)
			}
			var changed []replayStep
			if c.deleted {
				changed = recordedSince(r, func() {
					if ack := e.person("Bob").do(e.deleteColsOp("Sheet1", 2, 1)); status(ack) != "applied" {
						t.Fatalf("the deletion was answered %s", status(ack))
					}
				})
			}
			ack := r.answer(run.Frames[0])
			if res := ack.Results; len(res) != 1 || res[0].Status != "applied" || res[0].Applied != 1 {
				t.Fatalf("the paste was answered %+v", ack.Results)
			}
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["agent"]]}]}`)
			r.until("the overwrite notice", func() bool {
				for _, s := range r.steps {
					if kindOf(s) == "overwritten" {
						return true
					}
				}
				return false
			})
			got, _ := r.taken()
			later := got[len(steps)+len(changed):]
			answered := play(t, node, steps, append(gs, map[string]any{"steps": changed}, map[string]any{"steps": later})...)
			if k := kept(answered); len(k) == 0 || k[len(k)-1] != "mine" {
				t.Fatalf("the notices %q offer back %q, want the pasted text of A1", answered.Notices, k)
			}
		})
	}
}
