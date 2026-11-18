package sheet

import (
	"fmt"
	"reflect"
	"testing"
)

// TestEditCommitsBeforeFindOrMenuMovesTheSelection edits A1 in the real
// client, then moves to B2 with Find or with a right click, types again
// and presses Enter. The client's frames go to the real engine on the
// recorded connection: the first edit is in A1, where it was typed, and
// the second in B2, the cell the editor was shown on.
func TestEditCommitsBeforeFindOrMenuMovesTheSelection(t *testing.T) {
	node := nodeOrSkip(t)
	moves := map[string][]any{
		"find": {map[string]any{"find": "needle"}, map[string]any{"key": "Escape"}},
		"menu": {map[string]any{"menu": []int{1, 1}}, map[string]any{"key": "Escape"}},
	}
	for name, move := range moves {
		t.Run(name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"B2","rows":[["needle"]]}]}`)
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(e.tabID("Sheet1"))
			steps, _ := r.taken()
			g := []any{map[string]any{"select": []int{0, 0, 0, 0}}, map[string]any{"type": "draft"}}
			g = append(g, move...)
			g = append(g, map[string]any{"type": "more"}, map[string]any{"key": "Enter"})
			run := play(t, node, steps, g...)
			if len(run.Frames) != 2 {
				t.Fatalf("the client sent %d actions, want 2: %s", len(run.Frames), run.Frames)
			}
			for _, f := range run.Frames {
				if ack := r.answer(f); len(ack.Results) != 1 || ack.Results[0].Status != "applied" {
					t.Fatalf("the edit %s was answered %+v", f, ack.Results)
				}
			}
			if a1, b2 := e.text("A1"), e.text("B2"); a1 != "draft" || b2 != "more" {
				t.Errorf("A1 holds %q and B2 %q, want draft and more", a1, b2)
			}
		})
	}
}

// TestFormulaBarKeysWaitForTheFormula clicks the formula bar of a formula
// whose text is still asked for and types there. The recorded answer
// opens the editor with the formula and the typed keys after it, and
// Enter writes both into the real engine.
func TestFormulaBarKeysWaitForTheFormula(t *testing.T) {
	node := nodeOrSkip(t)
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["=1"]]}]}`)
	tab := e.tabID("Sheet1")
	_, row, col := e.ids("Sheet1!A1")
	s := serve(e)
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(tab)
	steps, _ := r.taken()
	r.ask(fmt.Sprintf(`{"type":"peek","id":1,"tab":%d,"row":%d,"col":%d}`, tab, row, col), "peeked", 1)
	all, _ := r.taken()

	run := play(t, node, steps,
		map[string]any{"select": []int{0, 0, 0, 0}},
		map[string]any{"bar": true},
		map[string]any{"type": "+2"},
		map[string]any{"edit": true},
		map[string]any{"steps": all[len(steps):]},
		map[string]any{"edit": true},
		map[string]any{"key": "Enter"},
	)
	var edits []any
	for _, text := range run.Edits {
		if text == nil {
			edits = append(edits, nil)
		} else {
			edits = append(edits, *text)
		}
	}
	if !reflect.DeepEqual(edits, []any{nil, "=1+2"}) {
		t.Fatalf("the editor held %v while waiting and after the answer, want nothing open and then =1+2", edits)
	}
	if len(run.Frames) != 1 {
		t.Fatalf("the client sent %d actions, want 1: %s", len(run.Frames), run.Frames)
	}
	if ack := r.answer(run.Frames[0]); len(ack.Results) != 1 || ack.Results[0].Status != "applied" {
		t.Fatalf("the edit was answered %+v", ack.Results)
	}
	if got := e.text("A1"); got != "=1+2" {
		t.Errorf("A1 holds %q, want =1+2", got)
	}
}
