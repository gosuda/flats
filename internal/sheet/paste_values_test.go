package sheet

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// gestures runs the real client over the recorded frames, makes the given
// gestures and returns the operations of each action it sent.
func gestures(t *testing.T, node string, steps []replayStep, gestures ...any) [][]json.RawMessage {
	t.Helper()
	run := play(t, node, steps, gestures...)
	actions := make([][]json.RawMessage, len(run.Frames))
	for i, f := range run.Frames {
		var a struct{ Ops []json.RawMessage }
		if err := json.Unmarshal(f, &a); err != nil {
			t.Fatalf("an action frame of the client: %v\n%s", err, f)
		}
		actions[i] = a.Ops
	}
	return actions
}

// clientRun is what the real client did in gestures.mjs: the action
// frames it sent, whole, its watch, peek and rebase frames, the text of
// each notice on its page, the text each notice offers back, the hello of
// each connection, the tab names each look found, the editor text each
// edit gesture found and the text each copy gave.
type clientRun struct {
	Frames   []json.RawMessage
	Requests []map[string]any
	Notices  []string
	Kept     []*string
	Hellos   []map[string]any
	Looks    [][]string
	Edits    []*string
	Copies   []*string
}

// play runs the real client over the recorded frames and makes the given
// gestures, which may take more recorded frames between them.
func play(t *testing.T, node string, steps []replayStep, gestures ...any) clientRun {
	t.Helper()
	return playFrom(t, node, false, steps, gestures...)
}

// playFrom is play, with loading true for steps that end before the
// client can edit, as inside a snapshot.
func playFrom(t *testing.T, node string, loading bool, steps []replayStep, gestures ...any) clientRun {
	t.Helper()
	// go test caches a pass by the files this process opens, and node's
	// reads are invisible to it.
	err := filepath.WalkDir("web", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		_, err = os.ReadFile(p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{"steps": steps, "gestures": gestures, "loading": loading})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "web/testdata/gestures.mjs")
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node gestures.mjs: %v\n%s", err, stderr.String())
	}
	var run clientRun
	if err := json.Unmarshal(out, &run); err != nil {
		t.Fatalf("gestures.mjs output: %v\n%.2000s", err, out)
	}
	return run
}

// TestPasteValuesKeepsDestinationFormats pastes values only from the real
// client into the real engine: one copied cell over a selection whose
// cells each have a format of their own. Every destination takes the
// value and keeps its own fill, number format and wrapping. A paste of
// everything over the same kind of range carries the source format, which
// shows that the formats checked are the ones a paste can change.
func TestPasteValuesKeepsDestinationFormats(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is not installed, so the client cannot be checked in CI")
		}
		t.Skip("node is not installed")
	}
	e := start(t)
	e.mustAgent(`{"ops":[
		{"op":"write","tab":"Sheet1","at":"A1","rows":[[5,"old","old","old"],["=A1*3","old","old","old"],[null,"old","old","old"]]},
		{"op":"format","tab":"Sheet1","ranges":["A1:A2"],"format":{"bold":true,"fill":"green-100","number":{"kind":"currency","symbol":"$","decimals":2}}},
		{"op":"format","tab":"Sheet1","ranges":["B1","C1","D1"],"format":{"fill":"red-200"}},
		{"op":"format","tab":"Sheet1","ranges":["B2","C2","D2"],"format":{"fill":"blue-200","number":{"kind":"percent","decimals":1}}},
		{"op":"format","tab":"Sheet1","ranges":["B3","C3","D3"],"format":{"wrap":true,"italic":true,"align":"right"}}]}`)
	s := serve(e)
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(e.tabID("Sheet1"))
	steps, _ := r.taken()

	dests := []string{"B1", "B2", "B3", "C1", "C2", "C3", "D1", "D2", "D3"}
	before := map[string]string{}
	for _, a := range append([]string{"A1", "A2"}, dests...) {
		before[a] = e.formatOf("Sheet1", a)
	}
	if before["B1"] == before["B2"] || before["B2"] == before["B3"] || before["B1"] == "{}" {
		t.Fatalf("the destinations should have distinct formats: %v", before)
	}
	actions := gestures(t, node, steps,
		// A1, a plain number, as values over B1:B3.
		map[string]any{"select": []int{0, 0, 0, 0}}, map[string]any{"copy": true},
		map[string]any{"select": []int{0, 1, 2, 1}}, map[string]any{"paste": "values"},
		// A2, a formula, as values over C1:C3.
		map[string]any{"select": []int{1, 0, 1, 0}}, map[string]any{"copy": true},
		map[string]any{"select": []int{0, 2, 2, 2}}, map[string]any{"paste": "values"},
		// A1 with everything over D1:D3.
		map[string]any{"select": []int{0, 0, 0, 0}}, map[string]any{"copy": true},
		map[string]any{"select": []int{0, 3, 2, 3}}, map[string]any{"paste": "all"},
	)
	if len(actions) != 3 {
		t.Fatalf("the client sent %d actions, want 3: %s", len(actions), actions)
	}
	p := e.person("Ann")
	for _, ops := range actions {
		list := make([]string, len(ops))
		for i, op := range ops {
			list[i] = string(op)
		}
		ack := p.do(list...)
		for _, res := range ack.Results {
			if res.Status != "applied" {
				t.Fatalf("the paste %v was answered with %s", list, status(ack))
			}
		}
	}

	for _, a := range []string{"B1", "B2", "B3", "D1", "D2", "D3"} {
		if got := e.text(a); got != "5" {
			t.Errorf("%s holds %q after the paste, want 5", a, got)
		}
	}
	for _, a := range []string{"C1", "C2", "C3"} {
		if got := e.text(a); got != "15" {
			t.Errorf("%s holds %q after the paste of values, want the value 15", a, got)
		}
	}
	for _, a := range []string{"A1", "A2", "B1", "B2", "B3", "C1", "C2", "C3"} {
		if got := e.formatOf("Sheet1", a); got != before[a] {
			t.Errorf("%s has the format %s after the paste of values, want its own %s", a, got, before[a])
		}
	}
	for _, a := range []string{"D1", "D2", "D3"} {
		if got := e.formatOf("Sheet1", a); got != before["A1"] {
			t.Errorf("%s has the format %s after the paste of everything, want the source's %s", a, got, before["A1"])
		}
	}
}

// nodeOrSkip returns the node executable the client tests run on.
func nodeOrSkip(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is not installed, so the client cannot be checked in CI")
		}
		t.Skip("node is not installed")
	}
	return node
}

// sendAll submits the actions the client sent as one person, in order, and
// returns their acks.
func sendAll(e *env, p *person, actions [][]json.RawMessage) []ackBody {
	e.t.Helper()
	var acks []ackBody
	for _, ops := range actions {
		list := make([]string, len(ops))
		for i, op := range ops {
			list[i] = string(op)
		}
		acks = append(acks, p.do(list...))
	}
	return acks
}

// TestRepeatedPasteTakesEveryCellFromTheSource pastes one copied cell over
// a larger selection from the real client into the real engine. Each
// destination gets the source's formula moved from the source to that
// destination, so a destination where the reference falls off the sheet
// does not pass its error on to the others. A copy whose source tab was
// deleted before the paste arrived changes nothing: no destination stands
// in for the source.
func TestRepeatedPasteTakesEveryCellFromTheSource(t *testing.T) {
	node := nodeOrSkip(t)
	e := start(t)
	e.mustAgent(`{"ops":[
		{"op":"write","tab":"Sheet1","at":"A1","rows":[[5,null,"first"],["=$A1",null,"second"]]},
		{"op":"format","tab":"Sheet1","ranges":["C1"],"format":{"fill":"red-200"}},
		{"op":"format","tab":"Sheet1","ranges":["C2"],"format":{"fill":"blue-200","italic":true}},
		{"op":"add_tab","name":"Other"},
		{"op":"write","tab":"Other","at":"A1","rows":[["from other"]]}]}`)
	s := serve(e)
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(e.tabID("Sheet1"))
	onSheet1, _ := r.taken()
	r.watch(e.tabID("Other"))
	onOther, _ := r.taken()
	r.watch(e.tabID("Sheet1"))
	back, _ := r.taken()
	before := map[string]string{"C1": e.formatOf("Sheet1", "C1"), "C2": e.formatOf("Sheet1", "C2")}

	actions := gestures(t, node, onSheet1,
		// A2, =$A1, over B1:B2.
		map[string]any{"select": []int{1, 0, 1, 0}}, map[string]any{"copy": true},
		map[string]any{"select": []int{0, 1, 1, 1}}, map[string]any{"paste": "all"},
		// A1 of Other over C1:C2 of Sheet1.
		map[string]any{"steps": onOther[len(onSheet1):]},
		map[string]any{"select": []int{0, 0, 0, 0}}, map[string]any{"copy": true},
		map[string]any{"steps": back[len(onOther):]},
		map[string]any{"select": []int{0, 2, 1, 2}}, map[string]any{"paste": "all"},
	)
	if len(actions) != 2 {
		t.Fatalf("the client sent %d actions, want 2: %s", len(actions), actions)
	}
	p := e.person("Ann")
	sendAll(e, p, actions[:1])
	e.wantText("B1", "=#REF!")
	e.wantText("B2", "=$A1")
	e.want("B2", "5")

	// Another editor deletes the source tab before the second paste arrives.
	bob := e.person("Bob")
	bob.do(jsonf(`{"op":"delete_tab","tab":%d}`, e.tabID("Other")))
	ack := sendAll(e, p, actions[1:])[0]
	for _, res := range ack.Results {
		if res.Status != "ignored" || res.Reason != "gone" {
			t.Errorf("the paste from a deleted tab was answered %s, want every operation ignored as gone", status(ack))
		}
	}
	e.wantText("C1", "first")
	e.wantText("C2", "second")
	for a, f := range before {
		if got := e.formatOf("Sheet1", a); got != f {
			t.Errorf("%s has the format %s after the paste from a deleted tab, want its own %s", a, got, f)
		}
	}
}

// TestPasteOfEmptyShownText pastes copies whose shown text is empty from
// the real client into the real engine: a formula whose result is empty
// text, a blank cell with a format, a cut of such a formula and the
// formula's value over a formatted range. Each is a copy inside the sheet
// like any other.
func TestPasteOfEmptyShownText(t *testing.T) {
	node := nodeOrSkip(t)
	e := start(t)
	e.mustAgent(`{"ops":[
		{"op":"write","tab":"Sheet1","at":"A1","rows":[["=\"\"",7,null,"old"],[null,null,null,"old"],[null,"x",null,"old"],["=\"\"",null,"y",null]]},
		{"op":"format","tab":"Sheet1","ranges":["A3"],"format":{"fill":"green-100","bold":true}},
		{"op":"format","tab":"Sheet1","ranges":["D1"],"format":{"fill":"red-200"}},
		{"op":"format","tab":"Sheet1","ranges":["D2"],"format":{"fill":"blue-200"}},
		{"op":"format","tab":"Sheet1","ranges":["D3"],"format":{"italic":true}}]}`)
	s := serve(e)
	r := s.record("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	r.watch(e.tabID("Sheet1"))
	steps, _ := r.taken()
	before := map[string]formula.Format{}
	for _, a := range []string{"A3", "B3", "D1", "D2", "D3"} {
		before[a] = e.styleAt(a)
	}
	if before["A3"] == before["B3"] || before["A3"] == (formula.Format{}) {
		t.Fatalf("the source and the destination of the formatted blank should differ: %v", before)
	}

	actions := gestures(t, node, steps,
		// A1, ="", over B1.
		map[string]any{"select": []int{0, 0, 0, 0}}, map[string]any{"copy": true},
		map[string]any{"select": []int{0, 1, 0, 1}}, map[string]any{"paste": "all"},
		// A3, blank with a format, over B3.
		map[string]any{"select": []int{2, 0, 2, 0}}, map[string]any{"copy": true},
		map[string]any{"select": []int{2, 1, 2, 1}}, map[string]any{"paste": "all"},
		// A4, ="", cut and pasted over C4.
		map[string]any{"select": []int{3, 0, 3, 0}}, map[string]any{"cut": true},
		map[string]any{"select": []int{3, 2, 3, 2}}, map[string]any{"paste": "all"},
		// A1 as values over D1:D3.
		map[string]any{"select": []int{0, 0, 0, 0}}, map[string]any{"copy": true},
		map[string]any{"select": []int{0, 3, 2, 3}}, map[string]any{"paste": "values"},
	)
	if len(actions) != 4 {
		t.Fatalf("the client sent %d actions, want 4: %s", len(actions), actions)
	}
	p := e.person("Ann")
	for _, ack := range sendAll(e, p, actions) {
		for _, res := range ack.Results {
			if res.Status != "applied" {
				t.Fatalf("a paste was answered with %s", status(ack))
			}
		}
	}
	e.wantText("B1", `=""`)
	e.want("B1", "")
	e.wantText("B3", "")
	if got := e.styleAt("B3"); got != before["A3"] {
		t.Errorf("B3 has the format %+v after the paste of a formatted blank, want the source's %+v", got, before["A3"])
	}
	e.wantText("C4", `=""`)
	e.wantText("A4", "")
	for _, a := range []string{"D1", "D2", "D3"} {
		e.want(a, "")
		if got := e.text(a); got == "old" {
			t.Errorf("%s still holds %q after the paste of an empty value", a, got)
		}
		if got := e.styleAt(a); got != before[a] {
			t.Errorf("%s has the format %+v after the paste of values, want its own %+v", a, got, before[a])
		}
	}
}

// styleAt returns the format of the cell at an address, read from the
// workbook: a blank cell with a format has one too.
func (e *env) styleAt(addr string) formula.Format {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	t, row, col := e.at(addr)
	c := t.cell(row, col)
	if c == nil {
		return formula.Format{}
	}
	return e.in.wb.styles.format(c.style)
}
