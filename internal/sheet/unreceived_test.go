package sheet

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// kindOf is the type of the frame a step received, or "" for an open.
func kindOf(s replayStep) string {
	var f struct{ Type string }
	_ = json.Unmarshal([]byte(s.Recv), &f)
	return f.Type
}

// rowsFrames lists the indexes of the rows frames among steps.
func rowsFrames(steps []replayStep) []int {
	var out []int
	for i, s := range steps {
		if kindOf(s) == "rows" {
			out = append(out, i)
		}
	}
	return out
}

// TestUnreceivedCellsWaitForTheirRows lets the real client take a real
// snapshot frame by frame. The person selects the cell that holds
// "prefix" before its row has arrived, copies it, presses F2 and types
// "suffix". A cell whose row has not arrived is not blank: the copy gives
// no text, the editor waits, Enter commits nothing, and the row's arrival
// opens the editor on the content with the keys after it. The engine
// stores "prefixsuffix". Before the first rows frame the grid takes no
// edit at all. The row of the cell is the last of a frame, so it can go on
// in the next one and only a later row or the end of the snapshot makes it
// whole.
func TestUnreceivedCellsWaitForTheirRows(t *testing.T) {
	node := nodeOrSkip(t)
	big := strings.Repeat("w", 30000)
	cases := []struct {
		name  string
		setup func(e *env)
		addr  string
		pos   []int
		// The client has the steps before rows frame `before` when the
		// person acts. Rows frame `whole`, or the end of the snapshot for
		// -1, makes the cell's row whole.
		before, whole int
		split         bool // the cell's row starts in frame before-1
	}{
		// The only row is the last of its frame: the end makes it whole.
		{"first rows", func(e *env) {
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["prefix"]]}]}`)
		}, "A1", []int{0, 0}, 0, -1, false},
		{"later rows", func(e *env) {
			rows := make([]string, 2500)
			for i := range rows {
				rows[i] = fmt.Sprintf(`["v%d"]`, i+1)
			}
			rows[2399] = `["prefix"]`
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[` + strings.Join(rows, ",") + `]}]}`)
		}, "A2400", []int{2399, 0}, 1, 1, false},
		{"a row split across frames", func(e *env) {
			// Twenty cells of 30,000 characters pass the frame limit, so
			// row 1 goes on in the next frame, which then brings row 2.
			cells := make([]string, 21)
			for i := range cells {
				cells[i] = `"` + big + `"`
			}
			cells[20] = `"prefix"`
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[` + strings.Join(cells, ",") + `],["below"]]}]}`)
		}, "U1", []int{0, 20}, 1, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			c.setup(e)
			tab := e.tabID("Sheet1")
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(tab)
			steps, _ := r.taken()
			frames := rowsFrames(steps)
			if len(frames) <= c.before || len(frames) <= c.whole {
				t.Fatalf("the snapshot came in %d rows frames", len(frames))
			}
			known := len(steps) - 1
			if c.whole >= 0 {
				known = frames[c.whole]
			}
			if c.split {
				_, row, _ := e.ids("Sheet1!" + c.addr)
				for _, i := range frames[:2] {
					var f struct{ Rows [][]json.RawMessage }
					_ = json.Unmarshal([]byte(steps[i].Recv), &f)
					if len(f.Rows) == 0 || string(f.Rows[0][0]) != fmt.Sprint(row) {
						t.Fatalf("rows frame %d does not hold row 1: %.200s", i, steps[i].Recv)
					}
				}
			}
			if kindOf(steps[len(steps)-1]) != "tab_done" {
				t.Fatalf("the recording ends with %s", steps[len(steps)-1].Recv)
			}
			have := steps[:frames[c.before]]
			sel := map[string]any{"select": []int{c.pos[0], c.pos[1], c.pos[0], c.pos[1]}}
			gs := []any{sel, map[string]any{"copy": true}}
			if c.before == 0 {
				// No rows yet: the grid takes no edit.
				gs = append(gs, map[string]any{"key": "F2"}, map[string]any{"type": "lost"}, map[string]any{"edit": true})
			}
			// The frames that leave the cell's row open, then the one that
			// makes it whole and the rest.
			gs = append(gs,
				map[string]any{"steps": steps[len(have):known]}, map[string]any{"copy": true},
				map[string]any{"key": "F2"}, map[string]any{"type": "suffix"}, map[string]any{"edit": true},
				map[string]any{"key": "Enter"},
				map[string]any{"steps": steps[known : known+1]}, map[string]any{"edit": true},
				map[string]any{"steps": steps[known+1:]},
				map[string]any{"key": "Enter"},
				sel, map[string]any{"copy": true})
			run := playFrom(t, node, c.before == 0, have, gs...)

			if len(run.Frames) != 1 {
				t.Fatalf("the client sent %d actions, want the edit once its row arrived: %s", len(run.Frames), run.Frames)
			}
			if ack := r.answer(run.Frames[0]); status(ack) != "applied" {
				t.Fatalf("the edit was answered %s", status(ack))
			}
			e.wantText("Sheet1!"+c.addr, "prefixsuffix")
			want := []any{nil, "prefixsuffix"}
			if c.before == 0 {
				want = append([]any{nil}, want...)
			}
			if !reflect.DeepEqual(edited(run), want) {
				t.Fatalf("the editor held %v, want %v", edited(run), want)
			}
			var copies []any
			for _, text := range run.Copies {
				if text == nil {
					copies = append(copies, nil)
				} else {
					copies = append(copies, *text)
				}
			}
			wantCopies := []any{nil, nil, "prefixsuffix"}
			if !reflect.DeepEqual(copies, wantCopies) {
				t.Fatalf("the copies gave %q, want %q", copies, wantCopies)
			}
		})
	}
}
