package sheet

import (
	"testing"
)

// TestFormulaBarTextOfAnOpenRowIsTypedOnce lets the real client take the
// first rows frame of a real snapshot, which holds A1 with "prefix". Row 1
// is the last of that frame, so it may go on in the next one, but A1 has
// arrived and the formula bar shows it. The person reaches the bar with
// Shift+F6, by pressing it, or composes there, and adds text. The edit
// waits for the row, and the end of the snapshot opens it on the text the
// bar showed, once. The engine stores that text.
func TestFormulaBarTextOfAnOpenRowIsTypedOnce(t *testing.T) {
	node := nodeOrSkip(t)
	cases := []struct {
		name string
		gs   []any
		want string
	}{
		{"keyboard", []any{map[string]any{"key": "F6", "shift": true}, map[string]any{"barType": "suffix"}}, "prefixsuffix"},
		{"pointer", []any{map[string]any{"bar": true}, map[string]any{"barType": "suffix"}}, "prefixsuffix"},
		{"composition", []any{map[string]any{"key": "F6", "shift": true}, map[string]any{"compose": "한"}}, "prefix한"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["prefix"]]}]}`)
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(e.tabID("Sheet1"))
			steps, _ := r.taken()
			frames := rowsFrames(steps)
			if len(frames) != 1 || kindOf(steps[len(steps)-1]) != "tab_done" || frames[0] != len(steps)-2 {
				t.Fatalf("the snapshot came in %d rows frames", len(frames))
			}
			gs := append([]any{map[string]any{"select": []int{0, 0, 0, 0}}}, c.gs...)
			gs = append(gs,
				map[string]any{"edit": true}, map[string]any{"key": "Enter"},
				map[string]any{"steps": steps[len(steps)-1:]},
				map[string]any{"edit": true}, map[string]any{"key": "Enter"})
			run := play(t, node, steps[:len(steps)-1], gs...)
			if got := edited(run); len(got) != 2 || got[0] != nil || got[1] != c.want {
				t.Fatalf("the editor held %v, want nil while it waits and then %q", got, c.want)
			}
			if len(run.Frames) != 1 {
				t.Fatalf("the client sent %s", run.Frames)
			}
			if ack := r.answer(run.Frames[0]); status(ack) != "applied" {
				t.Fatalf("the edit was answered %s", status(ack))
			}
			e.wantText("Sheet1!A1", c.want)
		})
	}
}
