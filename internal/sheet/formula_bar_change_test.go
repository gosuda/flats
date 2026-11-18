package sheet

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// barEdit is a formula of Sheet1 that the formula bar shows, a change
// another person makes that rewrites its text (a rename of the tab it
// names, an insertion above the row it names, or a sort that moves its
// row, product rule 15), and keys the person puts into the bar before it
// is drawn again.
type barEdit struct {
	name   string
	setup  string
	addr   string // the formula's cell
	text   string // what it holds, or what is typed into it
	change func(e *env) string
	moved  string // where the cell lies after the change
	now    string // the formula as the engine holds it after the change
	value  string
	at     int // where the keys go in the text the bar showed
	keys   string
}

func (c barEdit) typed() string { return c.text[:c.at] + c.keys + c.text[c.at:] }

// tabWith adds a tab holding 10 in A1 and 5 in B1.
func tabWith(name string) string {
	n, _ := json.Marshal(name)
	return fmt.Sprintf(`{"op":"add_tab","name":%s},{"op":"write","tab":%s,"at":"A1","rows":[[10,5]]}`, n, n)
}

func renameTo(tab, to string) func(e *env) string {
	return func(e *env) string {
		n, _ := json.Marshal(to)
		return jsonf(`{"op":"rename_tab","tab":%d,"name":%s}`, e.tabID(tab), n)
	}
}

var barEdits = []barEdit{
	{"rename/after", tabWith("Old"), "A1", "=Old!A1", renameTo("Old", "New"), "A1", "=New!A1", "10", 7, "+1"},
	{"insert/after", `{"op":"write","tab":"Sheet1","at":"B2","rows":[[10]]}`, "A1", "=B2",
		func(e *env) string { return e.insertRowsOp("Sheet1", 2, 1) }, "A1", "=B3", "10", 3, "+1"},
	// The row of A2 holds 40 and moves to row 4 with it.
	{"sort/after", `{"op":"write","tab":"Sheet1","at":"B2","rows":[[40],[30],[20]]}`, "A2", "=B2",
		func(e *env) string { return e.sortOp("Sheet1", 2, 3, 2, false) }, "A4", "=B4", "40", 3, "+1"},
	// A - after the = would land inside the quoted name if it kept its
	// distance from the end of the text.
	{"quoting rename/before", tabWith("Old"), "A1", "=Old!A1", renameTo("Old", "New Name"), "A1", "='New Name'!A1", "10", 1, "-"},
	{"unquoting rename/between", tabWith("Old Name"), "A1", "='Old Name'!A1+'Old Name'!B1", renameTo("Old Name", "New"), "A1", "=New!A1+New!B1", "15", 14, "*2"},
	// BA pasted between A and B gives =ABAB!A1, the text the formula has
	// after the rename, and reads as AB typed after =AB too, which would
	// name the tab ABABAB once AB is ABAB.
	{"repeating rename/inside", tabWith("AB") + `,{"op":"add_tab","name":"ABABAB"},{"op":"write","tab":"ABABAB","at":"A1","rows":[[99]]}`,
		"A1", "=AB!A1", renameTo("AB", "ABAB"), "A1", "=ABAB!A1", "10", 2, "BA"},
}

// wantOffered checks that the client sent no more than the written
// actions and that its newest notice offers the typed text back.
func wantOffered(t *testing.T, run clientRun, written int, typed, notice string) {
	t.Helper()
	if len(run.Frames) != written {
		t.Fatalf("the client sent %s, want no bar edit", run.Frames)
	}
	if n := len(run.Kept); n == 0 || run.Kept[n-1] == nil || *run.Kept[n-1] != typed || !strings.Contains(run.Notices[n-1], notice) {
		kept, _ := json.Marshal(run.Kept)
		t.Fatalf("the notices %q offer back %s, want %q", run.Notices, kept, typed)
	}
}

// shownFormula writes text into addr of Sheet1 and records what the real
// server sends a client that watches the tab and asks for the formula of
// that cell: the snapshot, then the answer to the peek.
func shownFormula(e *env, addr, text string) (r *recorder, at []int, peek func(id int) string, steps, shown []replayStep) {
	js, _ := json.Marshal(text)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"` + addr + `","rows":[[` + string(js) + `]]}]}`)
	tab, row, col := e.ids("Sheet1!" + addr)
	at = e.place(tab, row, col)
	r = serve(e).record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(tab)
	steps, _ = r.taken()
	peek = func(id int) string {
		return fmt.Sprintf(`{"type":"peek","id":%d,"tab":%d,"row":%d,"col":%d}`, id, tab, row, col)
	}
	r.ask(peek(1), "peeked", 1)
	got, _ := r.taken()
	return r, at, peek, steps, got[len(steps):]
}

// changedFormula has Bob make the change and records the delta and the
// answer to a second peek of the cell.
func changedFormula(e *env, r *recorder, change, peek string) (changed, now []replayStep) {
	before, _ := r.taken()
	e.person("Bob").do(change)
	got, _ := r.taken()
	changed = got[len(before):]
	r.ask(peek, "peeked", 2)
	all, _ := r.taken()
	return changed, all[len(got):]
}

// TestFormulaBarEditOfTextAChangeRewroteIsRefused has the real client show
// a formula of Sheet1 in the formula bar. Another person then renames the
// tab it names, inserts a row above the cell it names or sorts its row,
// and the client applies the change but does not draw the screen before
// the person puts keys into the bar. The keys were typed into text the
// cell no longer holds. The edit waits for the formula as the real engine
// gives it now, and is then refused with its text offered back. Nothing is
// written, so the engine keeps the formula the change made and its value.
func TestFormulaBarEditOfTextAChangeRewroteIsRefused(t *testing.T) {
	node := nodeOrSkip(t)
	for _, c := range barEdits {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[` + c.setup + `]}`)
			r, at, peek, steps, shown := shownFormula(e, c.addr, c.text)
			changed, now := changedFormula(e, r, c.change(e), peek(2))
			e.wantText("Sheet1!"+c.moved, c.now)

			run := play(t, node, steps,
				map[string]any{"down": map[string]any{"zone": "cell", "r": at[0], "c": at[1]}}, map[string]any{"up": true},
				map[string]any{"wait": 100},
				map[string]any{"steps": shown},
				map[string]any{"hold": true},
				map[string]any{"steps": changed},
				map[string]any{"barInsert": []any{c.at, c.keys}},
				map[string]any{"edit": true},
				map[string]any{"steps": now},
				map[string]any{"edit": true},
				map[string]any{"draw": true},
			)
			if want := []any{nil, nil}; !reflect.DeepEqual(edited(run), want) {
				t.Fatalf("the editor held %v, want none: it waits for the formula of the grid on show and does not open", edited(run))
			}
			if !sameFrame(t, sentRequest(t, run, "peek", 1), peek(1)) || !sameFrame(t, sentRequest(t, run, "peek", 2), peek(2)) {
				t.Fatalf("the client's requests differ from the recorded ones: %v", run.Requests)
			}
			wantOffered(t, run, 0, c.typed(), "changed before your edit in the formula bar opened")
			e.wantText("Sheet1!"+c.moved, c.now)
			e.want("Sheet1!"+c.moved, c.value)
		})
	}
}

// TestFormulaBarEditOfAPendingFormulaAChangeRewroteIsRefused commits the
// formula in the real client, and the bar shows it pending. Another person
// changes the structure, and the client applies the change but does not
// draw the screen before the person puts keys into the bar. The pending
// text is adjusted through the real engine's exchange, the keys were typed
// into the text before it, and the edit is refused with its text offered
// back. Only the first edit is written, and the engine holds it as the
// change rewrote it.
func TestFormulaBarEditOfAPendingFormulaAChangeRewroteIsRefused(t *testing.T) {
	node := nodeOrSkip(t)
	for _, c := range barEdits {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[` + c.setup + `]}`)
			tab, row, col := e.ids("Sheet1!" + c.addr)
			at := e.place(tab, row, col)
			r := serve(e).record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(tab)
			steps, _ := r.taken()
			base := e.seq()
			text, _ := json.Marshal(c.text)
			first := fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":%s,"at":[%d,%d]}]}`, base, tab, row, col, text, at[0], at[1])
			e.person("Bob").do(c.change(e))
			got, _ := r.taken()
			changed := got[len(steps):]
			rebase := fmt.Sprintf(`{"type":"rebase","id":1,"tab":%d,"row":%d,"col":%d,"text":%s,"at":[%d,%d],"base":"%d","through":"%d"}`, tab, row, col, text, at[0], at[1], base, e.seq())
			r.ask(rebase, "rebased", 1)
			got, _ = r.taken()
			rebased := got[len(steps)+len(changed):]
			if ack := r.answer(json.RawMessage(first)); status(ack) != "applied" {
				t.Fatalf("the first edit was answered %s", status(ack))
			}
			got, _ = r.taken()
			confirmed := got[len(steps)+len(changed)+len(rebased):]
			e.wantText("Sheet1!"+c.moved, c.now)

			run := play(t, node, steps,
				map[string]any{"select": []int{at[0], at[1], at[0], at[1]}},
				map[string]any{"type": c.text},
				map[string]any{"key": "Enter"},
				// A press on the cell draws the bar with the pending text.
				map[string]any{"down": map[string]any{"zone": "cell", "r": at[0], "c": at[1]}}, map[string]any{"up": true},
				map[string]any{"hold": true},
				map[string]any{"steps": changed},
				map[string]any{"barInsert": []any{c.at, c.keys}},
				map[string]any{"edit": true},
				map[string]any{"steps": rebased},
				map[string]any{"edit": true},
				map[string]any{"steps": confirmed},
				map[string]any{"draw": true},
			)
			if want := []any{nil, nil}; !reflect.DeepEqual(edited(run), want) {
				t.Fatalf("the editor held %v, want none: it waits for the adjusted pending text and does not open", edited(run))
			}
			if !sameFrame(t, sentRequest(t, run, "rebase", 1), rebase) {
				t.Fatalf("the client's exchange differs from the recorded one: %v", run.Requests)
			}
			if len(run.Frames) == 0 || !sameFrame(t, string(run.Frames[0]), first) {
				t.Fatalf("the client sent %s, want the first edit as recorded", run.Frames)
			}
			wantOffered(t, run, 1, c.typed(), "changed before your edit in the formula bar opened")
			e.wantText("Sheet1!"+c.moved, c.now)
			e.want("Sheet1!"+c.moved, c.value)
		})
	}
}

// TestFormulaBarEditStandsWhenAChangeLeftTheText has another person rename
// a tab the shown formula does not name, or insert a row below the cell it
// names. The client applies the change and does not draw the screen before
// the person adds +1 in the bar. The real engine still gives the formula
// the bar showed, so the edit opens as typed and the engine stores it.
func TestFormulaBarEditStandsWhenAChangeLeftTheText(t *testing.T) {
	node := nodeOrSkip(t)
	cases := []struct {
		name   string
		setup  string
		text   string
		change func(e *env) string
	}{
		{"rename", tabWith("Old") + `,{"op":"add_tab","name":"Other"}`, "=Old!A1", renameTo("Other", "Else")},
		{"insert", `{"op":"write","tab":"Sheet1","at":"B2","rows":[[10]]}`, "=B2", func(e *env) string { return e.insertRowsOp("Sheet1", 3, 1) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[` + c.setup + `]}`)
			r, at, peek, steps, shown := shownFormula(e, "A1", c.text)
			changed, now := changedFormula(e, r, c.change(e), peek(2))
			e.wantText("Sheet1!A1", c.text)

			run := play(t, node, steps,
				map[string]any{"down": map[string]any{"zone": "cell", "r": at[0], "c": at[1]}}, map[string]any{"up": true},
				map[string]any{"wait": 100},
				map[string]any{"steps": shown},
				map[string]any{"hold": true},
				map[string]any{"steps": changed},
				map[string]any{"barType": "+1"},
				map[string]any{"edit": true},
				map[string]any{"steps": now},
				map[string]any{"edit": true},
				map[string]any{"draw": true},
				map[string]any{"key": "Enter"},
			)
			if want := []any{nil, c.text + "+1"}; !reflect.DeepEqual(edited(run), want) {
				t.Fatalf("the editor held %v, want %v", edited(run), want)
			}
			if len(run.Frames) != 1 || len(run.Notices) != 0 {
				t.Fatalf("the client sent %s and showed %q", run.Frames, run.Notices)
			}
			if ack := r.answer(run.Frames[0]); status(ack) != "applied" {
				t.Fatalf("the edit was answered %s", status(ack))
			}
			e.wantText("Sheet1!A1", c.text+"+1")
			e.want("Sheet1!A1", "11")
		})
	}
}

// TestFormulaBarEditWithNothingTypedOpensOnTheFormulaAsItIs has another
// person rename the tab the shown formula names so that its name is
// quoted. The person presses the bar before the screen is drawn again and
// types nothing until the edit has opened, which it does on the formula as
// the real engine gives it now. Keys added then are of that text, and the
// engine stores them with the reference to the renamed tab.
func TestFormulaBarEditWithNothingTypedOpensOnTheFormulaAsItIs(t *testing.T) {
	node := nodeOrSkip(t)
	e := start(t)
	e.mustAgent(`{"ops":[` + tabWith("Old") + `]}`)
	r, at, peek, steps, shown := shownFormula(e, "A1", "=Old!A1")
	changed, now := changedFormula(e, r, renameTo("Old", "New Name")(e), peek(2))

	run := play(t, node, steps,
		map[string]any{"down": map[string]any{"zone": "cell", "r": at[0], "c": at[1]}}, map[string]any{"up": true},
		map[string]any{"wait": 100},
		map[string]any{"steps": shown},
		map[string]any{"hold": true},
		map[string]any{"steps": changed},
		map[string]any{"bar": true},
		map[string]any{"edit": true},
		map[string]any{"steps": now},
		map[string]any{"edit": true},
		map[string]any{"barType": "+1"},
		map[string]any{"draw": true},
		map[string]any{"key": "Enter"},
	)
	if want := []any{nil, "='New Name'!A1"}; !reflect.DeepEqual(edited(run), want) {
		t.Fatalf("the editor held %v, want %v", edited(run), want)
	}
	if len(run.Frames) != 1 || len(run.Notices) != 0 {
		t.Fatalf("the client sent %s and showed %q", run.Frames, run.Notices)
	}
	if ack := r.answer(run.Frames[0]); status(ack) != "applied" {
		t.Fatalf("the edit was answered %s", status(ack))
	}
	e.wantText("Sheet1!A1", "='New Name'!A1+1")
	e.want("Sheet1!A1", "11")
}

// TestFormulaBarKeysOfTheCellTheSelectionLeftAreRefused has the real
// client show A1, which holds 11, in the formula bar. The person selects
// A2, which holds 5, and puts a key into the bar before the screen is
// drawn again: at the start of 11, at its end, where another 1 reads as
// either, or a key at its end that reads one way only. The key was typed
// into the text of A1, so nothing goes into A2. The text is offered back,
// nothing is written and the engine keeps both cells.
func TestFormulaBarKeysOfTheCellTheSelectionLeftAreRefused(t *testing.T) {
	node := nodeOrSkip(t)
	cases := []struct {
		name string
		at   int
		keys string
	}{
		{"repeated/start", 0, "1"},
		{"repeated/end", 2, "1"},
		{"other/end", 2, "2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[11],[5]]}]}`)
			r := serve(e).record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
			r.watch(e.tabID("Sheet1"))
			steps, _ := r.taken()

			run := play(t, node, steps,
				map[string]any{"down": map[string]any{"zone": "cell", "r": 0, "c": 0}}, map[string]any{"up": true},
				map[string]any{"hold": true},
				map[string]any{"down": map[string]any{"zone": "cell", "r": 1, "c": 0}}, map[string]any{"up": true},
				map[string]any{"barInsert": []any{c.at, c.keys}},
				map[string]any{"edit": true},
				map[string]any{"draw": true},
			)
			if want := []any{nil}; !reflect.DeepEqual(edited(run), want) {
				t.Fatalf("the editor held %v, want none", edited(run))
			}
			wantOffered(t, run, 0, "11"[:c.at]+c.keys+"11"[c.at:], "selection moved before the formula bar showed the new cell")
			e.wantText("Sheet1!A1", "11")
			e.wantText("Sheet1!A2", "5")
		})
	}
}
