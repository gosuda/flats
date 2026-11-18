package sheet

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

// codeUnits writes a text as its UTF-16 code units, four hex digits each,
// as conform.mjs writes the text of a cell.
func codeUnits(text string) string {
	var b strings.Builder
	for _, unit := range utf16.Encode([]rune(text)) {
		fmt.Fprintf(&b, "%04x", unit)
	}
	return b.String()
}

// fakeLeavesOut are the operations the scripted server does not carry out
// as the engine does: it changes no cell for a copy or a fill (it answers
// a copy with the engine's counts), reverses the rows of a sort, adds rows
// and columns for a write's target {"new": k} in its own way and none for
// a copy's, and keeps no history, so that an undo or a redo takes a
// sequence and changes nothing else. For a row with
// one of them it is held to what comes of the frame and to nothing the
// frame leaves.
var fakeLeavesOut = []string{`"op":"copy"`, `"op":"fill"`, `"op":"sort"`, `{"new":`, `"op":"undo"`, `"op":"redo"`}

// TestFakeServerConformance runs the scripted server of the client's Node
// tests against the engine: the frame table, a session token the server
// does not know, the format list a connection gets for a tab it does not
// watch, and the structural operations of a delta on two connections. The
// scripted server gets the text of every frame of the table as the engine
// does (TestFrameTableAgainstEngine) and is held to the same row: what
// comes of the frame, what a frame that is read leaves (the cells, the
// tabs, the widths and the formats the row names, or nothing), and that a
// frame which is not read changes nothing.
//
// It is not held to everything. The rows that are calls of an agent are
// not sent to it: the scripted server speaks to a browser. It is sent
// every frame as a Private connection sends it, and not what a row says of
// a Public one: its Public mode refuses what a Public client must not
// send and reads nothing else differently. For the operations of
// fakeLeavesOut it is held to the verdict alone. This proves agreement on
// these cases only; the Node tests rely on the scripted server for
// everything else.
//
// The text of a cell is compared by its UTF-16 code units, which the
// script prints as hex. A text printed as a JSON string would pass
// through this test's JSON reader, which turns half of a surrogate pair
// into U+FFFD: a scripted server that stored the half would then look
// like the engine, which stores U+FFFD.
func TestFakeServerConformance(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is not installed, so the scripted server cannot be checked in CI")
		}
		t.Skip("node is not installed")
	}
	for _, name := range []string{"web/testdata/conform.mjs", "web/testdata/server.mjs", "web/testdata/frames.json"} {
		if _, err := os.ReadFile(name); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command(node, "web/testdata/conform.mjs").Output()
	if err != nil {
		t.Fatalf("node conform.mjs: %v\n%s", err, out)
	}
	var fake struct {
		Rows map[string]struct {
			Verdict string                     `json:"verdict"`
			Why     string                     `json:"why"`
			Changed bool                       `json:"changed"`
			Cells   map[string]string          `json:"cells"`
			Tabs    []string                   `json:"tabs"`
			Widths  map[string]int             `json:"widths"`
			Formats map[string]json.RawMessage `json:"formats"`
		} `json:"rows"`
		Expired []string `json:"expired"`
		Styles  []string `json:"styles"`
		Structs []string `json:"structs"`
	}
	if err := json.Unmarshal(out, &fake); err != nil {
		t.Fatalf("conform.mjs output: %v\n%s", err, out)
	}
	sent, held := 0, 0
	for _, row := range loadFrameTable(t) {
		got, ok := fake.Rows[row.Name]
		if row.Kind == "call" {
			if ok {
				t.Errorf("%s: the scripted server was sent a call of an agent", row.Name)
			}
			continue
		}
		sent++
		fail := func(format string, args ...any) {
			t.Errorf("%s: %s\n\t%s", row.Name, fmt.Sprintf(format, args...), row.Raw)
		}
		if !ok || got.Verdict != row.Want {
			fail("the scripted server answered %q (%s), the table says %q", got.Verdict, got.Why, row.Want)
			continue
		}
		if row.Want != "read" {
			if got.Changed {
				fail("the scripted server answered %q and changed its sheet", got.Verdict)
			}
			continue
		}
		if slices.ContainsFunc(fakeLeavesOut, func(part string) bool { return strings.Contains(row.Raw, part) }) {
			continue
		}
		if row.says() {
			held++
		}
		for at, want := range row.Cells {
			if got.Cells[at] != codeUnits(want) {
				fail("%s holds the code units %s on the scripted server, the table says %q (%s)", at, got.Cells[at], want, codeUnits(want))
			}
		}
		if row.Tabs != nil && !slices.Equal(got.Tabs, row.Tabs) {
			fail("the tabs are %q on the scripted server, the table says %q", got.Tabs, row.Tabs)
		}
		if row.Widths != nil && !maps.Equal(got.Widths, row.Widths) {
			fail("the widths are %v on the scripted server, the table says %v", got.Widths, row.Widths)
		}
		for at, want := range row.Formats {
			if !sameJSON(got.Formats[at], want) {
				fail("%s has the format %s on the scripted server, the table says %s", at, got.Formats[at], want)
			}
		}
		if row.Same && got.Changed {
			fail("the scripted server changed its sheet, and the table says the sheet is as it was")
		}
	}
	if sent == 0 || sent != len(fake.Rows) {
		t.Errorf("the scripted server answered %d rows, the table has %d for it", len(fake.Rows), sent)
	}
	if held < 120 {
		t.Errorf("the scripted server was held to what only %d rows leave", held)
	}

	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Two"},{"op":"write","tab":"Sheet1","at":"A1","rows":[["a"]]}]}`)
	s := serve(e)

	// A session token the server does not know.
	var expired []string
	for _, hello := range []string{
		`{"type":"hello","version":1,"session":"no-such-token","pending":0}`,
		`{"type":"hello","version":1,"session":"no-such-token","pending":2}`,
		`{"type":"hello","version":1,"pending":2}`,
	} {
		c := s.dial("private", hello)
		got := c.next("welcome", "fatal")
		expired = append(expired, strings.TrimSpace(fmt.Sprint(got["type"], " ", got["code"])))
		c.ws.Close(1000, "")
	}
	for i := range expired {
		expired[i] = strings.TrimSuffix(expired[i], " <nil>")
	}
	if fmt.Sprint(expired) != fmt.Sprint(fake.Expired) {
		t.Errorf("unknown session: the engine answered %v, the scripted server %v", expired, fake.Expired)
	}

	// The format list is one stream for the whole sheet: a connection gets
	// a new format in the delta that introduces it, whatever tab it
	// watches, and a later tab_head carries the whole list.
	one, two := e.tabID("Sheet1"), e.tabID("Two")
	_, row, col := e.ids("A1")
	a := s.dial("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	a.next("welcome")
	a.watch(1, one)
	b := s.dial("private", `{"type":"hello","version":1,"name":"Ben","pending":0}`)
	b.next("welcome")
	head, _ := b.watch(1, two)
	var styles []string
	shape := func(what string, f map[string]any) {
		list, _ := f["styles"].([]any)
		data, _ := json.Marshal(list)
		_, touched := f["touched"]
		_, cells := f["cells"]
		styles = append(styles, fmt.Sprintf("%s styles=%s touched=%v cells=%v", what, data, touched, cells))
	}
	shape("head of the other tab", head)
	a.send(fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"format","tab":%d,"rows":[[%d,1]],"cols":[[%d,1]],"set":{"bold":true}}]}`, e.seq(), one, row, col))
	shape("delta on the watching connection", a.next("delta"))
	shape("delta on the other connection", b.next("delta"))
	head, cells := b.watch(2, one)
	shape("head of the formatted tab", head)
	styles = append(styles, fmt.Sprintf("formatted cell style=%v", cells[cellKey(e, "A1")][5]))
	if strings.Join(styles, "\n") != strings.Join(fake.Styles, "\n") {
		t.Errorf("format list projection differs.\nengine:\n%s\nscripted server:\n%s", strings.Join(styles, "\n"), strings.Join(fake.Styles, "\n"))
	}

	// Structural operations reach every connection in one shape: each with
	// its fields and the id of its tab, whichever tab the connection
	// watches.
	b.watch(3, two)
	var structs []string
	structShape := func(what string, f map[string]any) {
		list, _ := f["struct"].([]any)
		entries := make([]string, len(list))
		for i, raw := range list {
			entry := raw.(map[string]any)
			keys := make([]string, 0, len(entry))
			for key := range entry {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			tab := "tab 2"
			switch id := TabID(entry["tab"].(float64)); {
			case id == one:
				tab = "first tab"
			case id != two:
				tab = "tab new"
			}
			entries[i] = fmt.Sprintf("[%q,%q,%q]", entry["op"], strings.Join(keys, ","), tab)
		}
		_, tabs := f["tabs"]
		_, touched := f["touched"]
		structs = append(structs, fmt.Sprintf("%s struct=[%s] tabs=%v touched=%v", what, strings.Join(entries, ","), tabs, touched))
	}
	_, second, _ := e.ids("A2")
	third := func() TabID { return e.tabID("Third") }
	for i, c := range []struct {
		name string
		op   func() string
	}{
		{"insert rows", func() string {
			return fmt.Sprintf(`{"op":"insert_rows","tab":%d,"beside":%d,"side":"before","count":2}`, one, row)
		}},
		{"delete rows", func() string { return fmt.Sprintf(`{"op":"delete_rows","tab":%d,"ids":[[%d,1]]}`, one, second) }},
		{"rename tab", func() string { return fmt.Sprintf(`{"op":"rename_tab","tab":%d,"name":"Renamed"}`, one) }},
		{"add tab", func() string { return `{"op":"add_tab","name":"Third"}` }},
		{"move tab", func() string { return fmt.Sprintf(`{"op":"move_tab","tab":%d,"before":%d}`, third(), one) }},
		{"delete tab", func() string { return fmt.Sprintf(`{"op":"delete_tab","tab":%d}`, third()) }},
	} {
		a.send(fmt.Sprintf(`{"type":"action","n":"%d","base":"%d","ops":[%s]}`, i+2, e.seq(), c.op()))
		structShape(c.name+", watching the tab", a.next("delta"))
		structShape(c.name+", watching another tab", b.next("delta"))
		a.next("ack")
	}
	if strings.Join(structs, "\n") != strings.Join(fake.Structs, "\n") {
		t.Errorf("structural operations differ.\nengine:\n%s\nscripted server:\n%s", strings.Join(structs, "\n"), strings.Join(fake.Structs, "\n"))
	}
}
