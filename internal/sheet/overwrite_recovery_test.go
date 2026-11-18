package sheet

import (
	"encoding/json"
	"testing"
)

// TestTextPastedIntoAddedRowsIsOffered pastes text from another program at
// the last row or column of a tab in the real client, so the real engine
// adds a row or a column for its last line or field. An agent then
// replaces the added cell. The overwrite notice offers the pasted text of
// that cell, filed under the id the paste's delta gave it.
func TestTextPastedIntoAddedRowsIsOffered(t *testing.T) {
	node := nodeOrSkip(t)
	cases := []struct {
		name, text string
		at         []int
		added      string
	}{
		{"row", "first\nmine", []int{newTabRows - 1, 0}, "A1001"},
		{"column", "first\tmine", []int{0, newTabCols - 1}, "AA1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(e.tabID("Sheet1"))
			steps, _ := r.taken()
			gs := []any{
				map[string]any{"select": []int{c.at[0], c.at[1], c.at[0], c.at[1]}},
				map[string]any{"paste": "all", "text": c.text},
			}
			run := play(t, node, steps, gs...)
			if len(run.Frames) != 1 {
				t.Fatalf("the client sent %s", run.Frames)
			}
			if ack := r.answer(run.Frames[0]); status(ack) != "applied" {
				t.Fatalf("the paste was answered %s", status(ack))
			}
			e.wantText("Sheet1!"+c.added, "mine")
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"` + c.added + `","rows":[["agent"]]}]}`)
			r.until("the overwrite notice", func() bool {
				for _, s := range r.steps {
					if kindOf(s) == "overwritten" {
						return true
					}
				}
				return false
			})
			got, _ := r.taken()
			answered := play(t, node, steps, append(gs, map[string]any{"steps": got[len(steps):]})...)
			if k := kept(answered); len(k) == 0 || k[len(k)-1] != "mine" {
				t.Fatalf("the notices %q offer back %q, want the pasted text of %s", answered.Notices, k, c.added)
			}
		})
	}
}

// TestTextCommittedWhileAnotherTabIsWatchedIsOffered types or pastes
// "mine" into Sheet1!A1 in the real client, which then opens Other, whose
// A1 is =Sheet1!A1. The real engine takes the action after the watch of
// Other, so the action's delta carries the recalculated Other!A1 and none
// of the cells it wrote. An agent then replaces Sheet1!A1, and the
// overwrite notice offers "mine", which the ack filed.
func TestTextCommittedWhileAnotherTabIsWatchedIsOffered(t *testing.T) {
	node := nodeOrSkip(t)
	for _, how := range []string{"paste", "typed edit"} {
		t.Run(how, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["old"]]},{"op":"add_tab","name":"Other"},{"op":"write","tab":"Other","at":"A1","rows":[["=Sheet1!A1"]]}]}`)
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(e.tabID("Sheet1"))
			steps, _ := r.taken()
			gs := []any{map[string]any{"select": []int{0, 0, 0, 0}}}
			if how == "paste" {
				gs = append(gs, map[string]any{"paste": "all", "text": "mine"})
			} else {
				gs = append(gs, map[string]any{"type": "mine"}, map[string]any{"key": "Enter"})
			}
			run := play(t, node, steps, gs...)
			if len(run.Frames) != 1 {
				t.Fatalf("the client sent %s", run.Frames)
			}
			r.watch(e.tabID("Other"))
			if ack := r.answer(run.Frames[0]); status(ack) != "applied" {
				t.Fatalf("the action was answered %s", status(ack))
			}
			e.wantText("Other!A1", "=Sheet1!A1")
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
			projected := false
			for _, s := range got[len(steps):] {
				var d struct {
					Type    string
					From    *struct{ N string }
					Cells   [][]json.RawMessage
					Touched []TabID
				}
				_ = json.Unmarshal([]byte(s.Recv), &d)
				if d.Type == "delta" && d.From != nil {
					projected = len(d.Cells) == 1 && len(d.Touched) == 1 && d.Touched[0] == e.tabID("Sheet1")
				}
			}
			if !projected {
				t.Fatalf("the action's delta is not the one of Other: %+v", got[len(steps):])
			}
			answered := play(t, node, steps, append(gs, map[string]any{"steps": got[len(steps):]})...)
			if k := kept(answered); len(k) == 0 || k[len(k)-1] != "mine" {
				t.Fatalf("the notices %q offer back %q, want the text of Sheet1!A1", answered.Notices, k)
			}
		})
	}
}

// TestAddedRowsAreOfferedWithoutTheirDelta pastes text from another
// program at the last row or column of a tab in the real client, and the
// real engine adds a row or a column for it. The client never gets the
// paste's delta, as across a resync, only its ack and what follows. The
// ack names the first id the paste added, so the overwrite notice of the
// added cell still offers the pasted text.
func TestAddedRowsAreOfferedWithoutTheirDelta(t *testing.T) {
	node := nodeOrSkip(t)
	cases := []struct {
		name, text, added, field string
		at                       []int
	}{
		{"row", "first\nmine", "A1001", "rows", []int{newTabRows - 1, 0}},
		{"column", "first\tmine", "AA1", "cols", []int{0, newTabCols - 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			s := serve(e)
			r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(e.tabID("Sheet1"))
			steps, _ := r.taken()
			gs := []any{
				map[string]any{"select": []int{c.at[0], c.at[1], c.at[0], c.at[1]}},
				map[string]any{"paste": "all", "text": c.text},
			}
			run := play(t, node, steps, gs...)
			if len(run.Frames) != 1 {
				t.Fatalf("the client sent %s", run.Frames)
			}
			if ack := r.answer(run.Frames[0]); status(ack) != "applied" {
				t.Fatalf("the paste was answered %s", status(ack))
			}
			_, row, col := e.ids("Sheet1!" + c.added)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"` + c.added + `","rows":[["agent"]]}]}`)
			r.until("the overwrite notice", func() bool {
				for _, s := range r.steps {
					if kindOf(s) == "overwritten" {
						return true
					}
				}
				return false
			})
			got, _ := r.taken()
			var later []replayStep
			for _, s := range got[len(steps):] {
				var f struct {
					Type  string
					From  *struct{ N string }
					Added map[string]uint64
				}
				_ = json.Unmarshal([]byte(s.Recv), &f)
				switch {
				case f.Type == "delta" && f.From != nil:
					continue // the paste's delta does not reach the client
				case f.Type == "ack":
					want := uint64(row)
					if c.field == "cols" {
						want = uint64(col)
					}
					if f.Added[c.field] != want {
						t.Fatalf("the ack names %v as added, want %s %d: %s", f.Added, c.field, want, s.Recv)
					}
				}
				later = append(later, s)
			}
			answered := play(t, node, steps, append(gs, map[string]any{"steps": later})...)
			if k := kept(answered); len(k) == 0 || k[len(k)-1] != "mine" {
				t.Fatalf("the notices %q offer back %q, want the pasted text of %s", answered.Notices, k, c.added)
			}
		})
	}
}
