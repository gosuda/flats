package sheet

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// The frame table, web/testdata/frames.json, holds what a client or an
// agent can send as the text it sends: a field twice, a name in another
// case or written with an escape, the form of a number and a null inside a
// list are in the bytes and in no parsed copy of them. Every row states
// what must come of its text. The engine is held to the table here, and
// the scripted server of the browser client's tests is held to the same
// rows (fakeconform_test.go), so the two agree on them.
//
// A row is written by hand, or built from a hand-written row that is the
// example of a shape (its base): TestFrameTableIsBuilt breaks the example
// once for each rule of Appendix A at each place the shapes of protocol.go
// and decode.go give it. The built rows are checked in. A run without
// -update-frames builds them again and fails when the file differs, and
// when a field, a kind or an alternative of a shape has no example to
// build rows from.
//
// What the two kinds of rows prove differs. A hand-written row states
// what Appendix A and sections 7.1 and 18.2 say of its text: what comes
// of it and, when it is read, what it leaves in the sheet. Such a row
// can find a schema in code that is not the one of the design. A built
// row cannot. Its verdict follows from the rule it breaks or keeps, and
// not from what a decoder made of it, so it proves that the walk applies
// every rule at every place of the shapes: no field can be written
// twice, in another case or as null, left out or given a value of
// another type without the answer of that rule. But the places
// themselves, with their fields, kinds and types, are read from the
// shapes of protocol.go and decode.go. A field the design names and the
// shapes lack has no built row, and a field with the wrong type in the
// shapes gets rows that agree with the wrong type. Only the hand-written
// examples and the rows around them hold the shapes to the design:
// TestFrameTableIsBuilt fails when a place has no example, and every
// example that is read says what it leaves.
var updateFrameTable = flag.Bool("update-frames", false, "rewrite the built rows of web/testdata/frames.json")

const frameTablePath = "web/testdata/frames.json"

// tableRow is one line of the frame table.
//
// kind says how raw is sent: "frame" after a hello, "hello" as the first
// frame of its connection, "call" as an update_sheet call. In raw, $tab
// and $other stand for the ids of the two tabs of the sheet, $row and $col
// for the ids of A1, $row2 and $col2 for those of B2, $seq for the
// sequence and $state for the state of the sheet.
//
// want is what must come of it: "read" (a frame is taken and every
// operation of an action applied; a call is applied), "protocol" (the
// connection ends with that fatal code, and so for every other code),
// "invalid" (an operation of the action, or the call, is refused as
// invalid), or the status and reason of the first operation that is not
// applied, as in "ignored:gone". Nothing but a row that wants "read"
// changes the sheet. reason is the field a refused call names.
//
// public is what comes of a frame on a Public connection, which may send
// a watch and a peek and nothing else: "read", "protocol" for a frame
// that is none of Appendix A, whatever it names as its type, or
// "forbidden". Every row of kind "frame" has it and is sent on both kinds
// of connection. Nothing a Public connection sends changes the sheet.
//
// A row that is read says what it leaves: cells are cells of the first
// tab with their text as typed, tabs the tab list in order (each as
// "name ROWSxCOLS FROZENROWS,FROZENCOLS"), widths the columns of the
// first tab that have a width of their own, by letter, and formats the
// format of cells of the first tab as a read reports it. same says that
// the row leaves the sheet as it was. A hand-written row that is read
// names at least one of them.
//
// base marks the example of a shape ("frame:watch", "op:set", "call",
// "callop:write"), and without says what comes of the example when an
// optional field is left out, where that is not what comes of the whole.
// rule and at mark a built row: the rule it breaks or keeps, and where.
type tableRow struct {
	Name    string                     `json:"name"`
	Kind    string                     `json:"kind"`
	Raw     string                     `json:"raw"`
	Want    string                     `json:"want"`
	Public  string                     `json:"public,omitempty"`
	Reason  string                     `json:"reason,omitempty"`
	Cells   map[string]string          `json:"cells,omitempty"`
	Tabs    []string                   `json:"tabs,omitempty"`
	Widths  map[string]int             `json:"widths,omitempty"`
	Formats map[string]json.RawMessage `json:"formats,omitempty"`
	Same    bool                       `json:"same,omitempty"`
	Base    string                     `json:"base,omitempty"`
	Without map[string]string          `json:"without,omitempty"`
	Rule    string                     `json:"rule,omitempty"`
	At      string                     `json:"at,omitempty"`
}

// says reports whether the row names what it leaves.
func (row *tableRow) says() bool {
	return row.Same || len(row.Cells)+len(row.Tabs)+len(row.Widths)+len(row.Formats) > 0
}

func loadFrameTable(t testing.TB) []tableRow {
	t.Helper()
	data, err := os.ReadFile(frameTablePath)
	if err != nil {
		t.Fatal(err)
	}
	var rows []tableRow
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rows); err != nil {
		t.Fatalf("%s: %v", frameTablePath, err)
	}
	return rows
}

func writeFrameTable(t testing.TB, rows []tableRow) {
	t.Helper()
	var out bytes.Buffer
	out.WriteString("[\n")
	for i, row := range rows {
		var line bytes.Buffer
		enc := json.NewEncoder(&line)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
		out.WriteString("  ")
		out.Write(bytes.TrimSpace(line.Bytes()))
		if i < len(rows)-1 {
			out.WriteByte(',')
		}
		out.WriteByte('\n')
	}
	out.WriteString("]\n")
	if err := os.WriteFile(frameTablePath, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tableSheet is the sheet the rows of the table are sent to: Sheet1 with
// a, b, c and d in A1:B2, and a second tab.
type tableSheet struct {
	e    *env
	s    *site
	fill *strings.Replacer
	was  string
}

func newTableSheet(t testing.TB) *tableSheet {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["a","b"],["c","d"]]},{"op":"add_tab","name":"Other"}]}`)
	ts := &tableSheet{e: e, s: serve(e)}
	tab, row, col := e.ids("A1")
	_, row2, col2 := e.ids("B2")
	// The longer names first: $row2 is not $row followed by a 2.
	ts.fill = strings.NewReplacer("$other", fmt.Sprint(e.tabID("Other")), "$row2", fmt.Sprint(row2), "$col2", fmt.Sprint(col2),
		"$tab", fmt.Sprint(tab), "$row", fmt.Sprint(row), "$col", fmt.Sprint(col), "$seq", fmt.Sprint(e.seq()), "$state", e.state())
	ts.was = ts.state()
	return ts
}

// state is everything of the sheet a row could change, as text. Every
// change of a sheet takes a sequence.
func (ts *tableSheet) state() string {
	e := ts.e
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	var b strings.Builder
	fmt.Fprintf(&b, "seq %d", e.in.wb.seq)
	for _, t := range e.in.wb.tabs {
		fmt.Fprintf(&b, "\n%s %dx%d frozen %d,%d", t.name, t.rows.len(), t.cols.len(), t.frozenRows, t.frozenCols)
	}
	return b.String()
}

// tabs is the tab list as a row names it.
func (ts *tableSheet) tabs() []string {
	e := ts.e
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	var out []string
	for _, t := range e.in.wb.tabs {
		out = append(out, fmt.Sprintf("%s %dx%d %d,%d", t.name, t.rows.len(), t.cols.len(), t.frozenRows, t.frozenCols))
	}
	return out
}

// widths are the columns of the first tab that have a width of their own.
func (ts *tableSheet) widths() map[string]int {
	e := ts.e
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	out := map[string]int{}
	t := e.in.wb.tabs[0]
	for at, id := range t.cols.ids {
		if w, ok := t.widths[ColID(id)]; ok && w.px != defaultWidth {
			out[formula.ColumnName(int32(at))] = w.px
		}
	}
	return out
}

// left holds what the sheet holds after a row that was read against what
// the row says it leaves.
func (ts *tableSheet) left(t *testing.T, row tableRow) {
	e := ts.e
	fail := func(format string, args ...any) {
		t.Errorf("%s: %s\n\t%s", row.Name, fmt.Sprintf(format, args...), ts.fill.Replace(row.Raw))
	}
	for at, want := range row.Cells {
		if got := e.text(at); got != want {
			fail("%s holds %q after it, the table says %q", at, got, want)
		}
	}
	if row.Tabs != nil && !slices.Equal(ts.tabs(), row.Tabs) {
		fail("the tabs are %q after it, the table says %q", ts.tabs(), row.Tabs)
	}
	if row.Widths != nil && !maps.Equal(ts.widths(), row.Widths) {
		fail("the widths are %v after it, the table says %v", ts.widths(), row.Widths)
	}
	first := e.tabNames()[0]
	for at, want := range row.Formats {
		if got := e.formatOf(first, at); !sameJSON(json.RawMessage(got), want) {
			fail("%s has the format %s after it, the table says %s", at, got, want)
		}
	}
	if row.Same && !ts.unchanged() {
		fail("the sheet is not as it was, and the table says it is")
	}
}

// unchanged reports whether the sheet is as the row found it.
func (ts *tableSheet) unchanged() bool {
	e := ts.e
	return ts.state() == ts.was && e.text("A1")+"|"+e.text("B1")+"|"+e.text("A2")+"|"+e.text("B2") == "a|b|c|d"
}

// send sends the row on a connection of the route and returns what came
// of it, with the reason of a refused call.
func (ts *tableSheet) send(t testing.TB, row tableRow, route string) (verdict, reason string) {
	raw := ts.fill.Replace(row.Raw)
	if row.Kind == "call" {
		_, err := ts.e.agentRaw(raw)
		if err == nil {
			return "read", ""
		}
		werr := refusal(err)
		return werr.Code, werr.Reason
	}
	exchange := func(probe bool) string {
		hello := `{"type":"hello","version":1,"name":"Ann","pending":0}`
		if route == "public" {
			hello = helloV1 // a Public hello carries no name
		}
		if row.Kind == "hello" {
			hello = ""
		}
		c := ts.s.dial(route, hello)
		defer c.ws.Close(1000, "")
		if row.Kind == "hello" {
			_ = c.ws.Send(raw)
			got, ok := c.wait("welcome", "fatal")
			switch {
			case !ok:
				return "closed"
			case got["type"] == "welcome":
				return "read"
			}
			return fmt.Sprint(got["code"])
		}
		c.next("welcome")
		_ = c.ws.Send(raw)
		// Frames are read in order, so the answer to this peek follows the
		// frame under test unless that frame ended the connection.
		if probe {
			_ = c.ws.Send(ts.fill.Replace(`{"type":"peek","id":424242,"tab":$tab,"row":$row,"col":$col}`))
		}
		timeout := time.After(20 * time.Second)
		for {
			select {
			case got, open := <-c.frames:
				switch {
				case !open:
					return "closed"
				case got["type"] == "fatal":
					return fmt.Sprint(got["code"])
				case got["type"] == "ack":
					results, _ := got["results"].([]any)
					for _, raw := range results {
						res := raw.(map[string]any)
						switch {
						case res["status"] == "applied":
						case res["status"] == "refused" && res["reason"] == "invalid":
							return "invalid"
						default:
							return fmt.Sprint(res["status"], ":", res["reason"])
						}
					}
					return "read"
				case got["id"] == float64(424242):
					return "read"
				}
			case <-timeout:
				return "silent"
			}
		}
	}
	verdict = exchange(true)
	if verdict == "closed" && ts.state() == ts.was {
		// The server ended the connection while the probe was on its way.
		// Closing a socket with unread bytes can reset it, and a reset can
		// take the fatal frame with it. Sent alone, the frame gets its
		// answer.
		verdict = exchange(false)
	}
	return verdict, ""
}

// check holds what came of a row against the row. It returns a sheet for
// the next row: a new one when the row changed this one.
func (ts *tableSheet) check(t *testing.T, row tableRow) *tableSheet {
	if row.Public != "" {
		// On a Public connection first: it leaves the sheet as it is.
		if verdict, _ := ts.send(t, row, "public"); verdict != row.Public {
			t.Errorf("%s: on a Public connection the engine answered %q, the table says %q\n\t%s", row.Name, verdict, row.Public, ts.fill.Replace(row.Raw))
		}
		if !ts.unchanged() {
			t.Errorf("%s changed the sheet from a Public connection\n\t%s", row.Name, ts.fill.Replace(row.Raw))
			ts = newTableSheet(t)
		}
	}
	verdict, reason := ts.send(t, row, "private")
	if verdict != row.Want || row.Kind == "call" && row.Want != "read" && reason != row.Reason {
		t.Errorf("%s: the engine answered %q %s, the table says %q %s\n\t%s", row.Name, verdict, reason, row.Want, row.Reason, ts.fill.Replace(row.Raw))
	}
	if verdict == "read" {
		ts.left(t, row)
	}
	// The cells are read as well as the sequence: a row that is not read
	// must not have moved them either.
	if ts.unchanged() {
		return ts
	}
	if verdict != "read" {
		t.Errorf("%s was answered with %q and changed the sheet\n\t%s", row.Name, verdict, ts.fill.Replace(row.Raw))
	}
	return newTableSheet(t)
}

// TestFrameTableAgainstEngine sends every row of the table to the engine
// and holds the engine to it: what comes of the row on a Private
// connection and, for a frame, on a Public one, the reason of a refused
// call, what a row that is read leaves, and that a row which is not read
// leaves the sheet as it was.
func TestFrameTableAgainstEngine(t *testing.T) {
	rows := loadFrameTable(t)
	names := map[string]bool{}
	ts := newTableSheet(t)
	for _, row := range rows {
		if names[row.Name] {
			t.Errorf("the table has two rows named %q", row.Name)
		}
		names[row.Name] = true
		if row.Kind != "frame" && row.Kind != "hello" && row.Kind != "call" {
			t.Fatalf("%s: a row is a frame, a hello or a call, not %q", row.Name, row.Kind)
		}
		if (row.Kind == "frame") != (row.Public != "") {
			t.Errorf("%s: a frame, and no other row, says what comes of it on a Public connection", row.Name)
		}
		if row.Rule == "" && row.Want == "read" && !row.says() {
			t.Errorf("%s is read and does not say what it leaves", row.Name)
		}
		if row.Want != "read" && row.says() {
			t.Errorf("%s is not read and says what it leaves", row.Name)
		}
		ts = ts.check(t, row)
	}
}

// TestFrameTableIsBuilt builds the rows of the table that follow from the
// shapes and holds the file to them. It fails when a place of a shape has
// no example: a field, a kind of an object with kinds, an alternative of a
// place that takes more than one JSON type, the items of a list. With
// -update-frames it writes the built rows.
func TestFrameTableIsBuilt(t *testing.T) {
	var hand, built []tableRow
	for _, row := range loadFrameTable(t) {
		if row.Rule == "" {
			hand = append(hand, row)
		} else {
			built = append(built, row)
		}
	}
	g := &tableBuilder{seen: map[string]bool{}, visited: map[any]bool{}}
	for _, row := range hand {
		if row.Base != "" {
			g.build(t, row)
		}
	}
	// Every place of both schemas has an example.
	var missing []string
	g.places(frameShape, "frame", map[*shape]bool{}, &missing)
	g.places(callShape, "call", map[*shape]bool{}, &missing)
	if len(missing) > 0 {
		t.Errorf("%d places of the wire schema have no example row (a row with base) to build cases from:\n\t%s", len(missing), strings.Join(missing, "\n\t"))
	}
	// Every rule has rows for every kind of frame, operation and call.
	for _, rule := range []string{faultTwice, faultUnknown, faultNull, faultMissing, faultType, "case", "escaped"} {
		for _, base := range g.bases {
			if !g.seen[base+" "+rule] {
				t.Errorf("no row breaks or keeps the rule %q for %s", rule, base)
			}
		}
	}
	if *updateFrameTable {
		writeFrameTable(t, append(hand, g.rows...))
		return
	}
	if !slices.EqualFunc(built, g.rows, func(a, b tableRow) bool {
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		return bytes.Equal(x, y)
	}) {
		t.Errorf("%s holds %d built rows and the shapes give %d, or other ones: run go test ./internal/sheet -run TestFrameTableIsBuilt -update-frames", frameTablePath, len(built), len(g.rows))
	}
}

// jnode is a JSON value of an example, with the members of an object in
// their order and every scalar as it is written. A placeholder such as
// $tab is a number.
type jnode struct {
	kind  byte // { [ " 0 t n
	raw   string
	keys  []string
	items []*jnode
}

func parseExample(t testing.TB, text string) *jnode {
	i := 0
	var value func() *jnode
	value = func() *jnode {
		switch c := text[i]; {
		case c == '{' || c == '[':
			n := &jnode{kind: c}
			i++
			for text[i] != '}' && text[i] != ']' {
				if c == '{' {
					start := i
					for i++; text[i] != '"'; i++ {
					}
					i += 2 // the quote and the colon
					n.keys = append(n.keys, text[start+1:i-2])
				}
				n.items = append(n.items, value())
				if text[i] == ',' {
					i++
				}
			}
			i++
			return n
		case c == '"':
			start := i
			for i++; text[i] != '"'; i++ {
				if text[i] == '\\' {
					i++
				}
			}
			i++
			return &jnode{kind: '"', raw: text[start:i]}
		}
		start := i
		for i < len(text) && !strings.ContainsRune(",]}", rune(text[i])) {
			i++
		}
		n := &jnode{kind: '0', raw: text[start:i]}
		switch n.raw {
		case "true", "false":
			n.kind = 't'
		case "null":
			n.kind = 'n'
		}
		return n
	}
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("an example is compact JSON without spaces: %v\n\t%s", p, text)
		}
	}()
	n := value()
	if i != len(text) {
		panic("text after the value")
	}
	return n
}

func (n *jnode) write(b *strings.Builder) {
	switch n.kind {
	case '{', '[':
		b.WriteByte(n.kind)
		for i, item := range n.items {
			if i > 0 {
				b.WriteByte(',')
			}
			if n.kind == '{' {
				b.WriteString(`"` + n.keys[i] + `":`)
			}
			item.write(b)
		}
		b.WriteByte(n.kind + 2) // } follows { and ] follows [ by two
	default:
		b.WriteString(n.raw)
	}
}

func (n *jnode) String() string {
	var b strings.Builder
	n.write(&b)
	return b.String()
}

// first is the byte a walk sees first of the value.
func (n *jnode) first() byte {
	switch n.kind {
	case '0':
		return '1'
	case '{', '[', '"':
		return n.kind
	}
	return n.raw[0]
}

// tableBuilder builds rows from examples. A rule is broken once at each
// place of a shape: the first example that reaches the place gives the
// rows, and later ones only add what they reach first.
type tableBuilder struct {
	rows    []tableRow
	seen    map[string]bool // rows built, and "<base> <rule>"
	visited map[any]bool    // members, kinds and alternatives an example reached
	bases   []string

	t    testing.TB
	base tableRow
	root *jnode
}

type memberKey struct {
	s  *shape
	at int
}

type kindKey struct {
	s    *shape
	kind string
}

// wrongValues are values of each JSON type, to stand where a shape takes
// another.
var wrongValues = []string{`"x"`, `7`, `1.5`, `true`, `[]`, `{}`}

// wrongFor lists the values that stand at a place of the shape as values
// of the wrong type. A number with a fraction is one of them only where
// an integer is not: there it is the form of the number that is wrong.
func wrongFor(s *shape) []string {
	var out []string
	for _, value := range wrongValues {
		if !takes(s, value) && (value != `1.5` || takes(s, `7`)) {
			out = append(out, value)
		}
	}
	return out
}

// takes reports whether a place of the shape reads the value without a
// fault of its type. An empty list and an empty object are left out where
// the place takes a list or an object at all: what comes of them is not a
// matter of type.
func takes(s *shape, value string) bool {
	if s.kind == jOneOf {
		return slices.ContainsFunc(s.alts, func(alt *shape) bool { return takes(alt, value) })
	}
	switch value {
	case `"x"`:
		return s.kind == jText
	case `7`:
		return s.kind == jInt || s.kind == jNumber
	case `1.5`:
		return s.kind == jNumber
	case `true`:
		return s.kind == jFlag
	case `[]`:
		return s.kind == jList && s.count == 0
	}
	return s.kind == jObject
}

func (g *tableBuilder) build(t testing.TB, base tableRow) {
	g.t, g.base, g.root = t, base, parseExample(t, base.Raw)
	if g.root.String() != base.Raw {
		t.Fatalf("%s: the example does not read back as it is written\n\t%s\n\t%s", base.Name, base.Raw, g.root)
	}
	if !slices.Contains(g.bases, base.Base) {
		g.bases = append(g.bases, base.Base)
	}
	part, kind, _ := strings.Cut(base.Base, ":")
	op := func(s *shape) (*jnode, *shape) {
		ops := g.root.items[slices.Index(g.root.keys, "ops")]
		if len(ops.items) != 1 {
			t.Fatalf("%s: the example of an operation holds one operation", base.Name)
		}
		return ops.items[0], s
	}
	node, s, at, deep := g.root, frameShape, "", true
	switch part {
	case "frame":
		deep = false
	case "op":
		node, s = op(opShape)
		at = "ops[0]"
	case "call":
		s, deep = callShape, false
	case "callop":
		node, s = op(agentOpShape)
		at = "ops[0]"
	default:
		t.Fatalf("%s: a base is frame:<type>, op:<kind>, call or callop:<kind>, not %q", base.Name, base.Base)
	}
	if s.tag != 0 && part != "call" {
		if got := strings.Trim(node.items[slices.Index(node.keys, s.members[s.tag-1].name)].raw, `"`); got != kind {
			t.Fatalf("%s: the example of %s is a %s", base.Name, base.Base, got)
		}
	}
	g.object(node, s, at, deep)
}

// emit adds a built row unless the rule was already broken at the place.
func (g *tableBuilder) emit(key any, rule, at, detail string, change func() func()) {
	id := rule + " " + detail
	switch k := key.(type) {
	case memberKey:
		id += fmt.Sprintf(" %p.%d", k.s, k.at)
	case kindKey:
		id += fmt.Sprintf(" %p:%s", k.s, k.kind)
	case *shape:
		id += fmt.Sprintf(" %p", k)
	}
	g.visitedRule(rule)
	if g.seen[id] {
		return
	}
	g.seen[id] = true
	undo := change()
	raw := g.root.String()
	undo()
	row := tableRow{Kind: g.base.Kind, Raw: raw, Rule: rule, At: at, Public: g.base.Public}
	place := at
	if place == "" {
		place = "the " + g.base.Kind
	}
	row.Name = strings.TrimSpace(fmt.Sprintf("%s, %s: %s %s", g.base.Name, place, rule, detail))
	switch rule {
	case "escaped", "optional", "nullable":
		row.Want, row.Reason = g.base.Want, g.base.Reason
		if rule == "escaped" {
			// The same frame in other bytes leaves what the example leaves.
			row.Cells, row.Tabs, row.Widths, row.Formats, row.Same = g.base.Cells, g.base.Tabs, g.base.Widths, g.base.Formats, g.base.Same
		}
		if want, ok := g.base.Without[detail]; ok && rule == "optional" {
			row.Want, row.Reason, _ = strings.Cut(want, " ")
		}
	default:
		row.Want = "protocol"
		if g.base.Kind == "frame" {
			// A frame that breaks a rule of the reading is none of
			// Appendix A on any connection.
			row.Public = "protocol"
		}
		if g.base.Kind == "call" {
			row.Want, row.Reason = "invalid", callReason(at, rule, detail)
		}
	}
	g.rows = append(g.rows, row)
}

func (g *tableBuilder) visitedRule(rule string) {
	switch rule {
	case "optional", "nullable":
		rule = map[string]string{"optional": faultMissing, "nullable": faultNull}[rule]
	case faultForeign, faultCount:
		return
	}
	g.seen[g.base.Base+" "+rule] = true
}

// callReason is the field a refused call names for a fault (section
// 18.3). at is the object a rule about a field names (a field twice,
// unknown, in another case, of another operation, or missing), and else
// the place of the value. The reason is the field of the operation, cell
// for what is wrong inside a cell of rows, the field of the call, or
// field for a field that belongs to no operation and to no call.
func callReason(at, rule, field string) string {
	path := strings.FieldsFunc(at, func(r rune) bool { return r == '.' || r == '[' || r == ']' })
	inOp := len(path) >= 2 && path[0] == "ops"
	ofField := rule != faultNull && rule != faultType && rule != faultCount
	if ofField && (len(path) == 0 || inOp && len(path) == 2) {
		if rule == faultTwice || rule == faultMissing {
			return field
		}
		return "field"
	}
	switch {
	case len(path) == 0:
		return "json"
	case inOp && len(path) == 2:
		return "ops"
	case inOp && path[2] == "rows" && len(path) >= 5:
		return "cell"
	case inOp:
		return path[2]
	}
	return path[0]
}

func (g *tableBuilder) object(n *jnode, s *shape, at string, deep bool) {
	var of kindFields
	kind, known := "", false
	if s.tag != 0 {
		kind = strings.Trim(n.items[slices.Index(n.keys, s.members[s.tag-1].name)].raw, `"`)
		of, known = s.kinds[kind]
		g.visited[kindKey{s, kind}] = true
	}
	join := func(name string) string {
		if at == "" {
			return name
		}
		return at + "." + name
	}
	for i := 0; i < len(n.keys); i++ {
		name, value := n.keys[i], n.items[i]
		m := s.index(name)
		key := memberKey{s, m}
		g.visited[key] = true
		val := s.members[m].val
		swap := func(to *jnode) func() func() {
			return func() func() {
				n.items[i] = to
				return func() { n.items[i] = value }
			}
		}
		g.emit(key, faultTwice, at, name, func() func() {
			keys, items := n.keys, n.items
			n.keys = slices.Insert(slices.Clone(keys), i+1, name)
			n.items = slices.Insert(slices.Clone(items), i+1, value)
			return func() { n.keys, n.items = keys, items }
		})
		rename := func(to string) func() func() {
			return func() func() {
				n.keys[i] = to
				return func() { n.keys[i] = name }
			}
		}
		g.emit(key, "case", at, name, rename(strings.ToUpper(name)))
		g.emit(key, "escaped", at, name, rename(fmt.Sprintf(`\u%04x`, name[0])+name[1:]))
		if val.null {
			g.emit(key, "nullable", join(name), "", swap(&jnode{kind: 'n', raw: "null"}))
		} else {
			g.emit(key, faultNull, join(name), "", swap(&jnode{kind: 'n', raw: "null"}))
		}
		required := s.members[m].required
		if s.tag != 0 {
			required = m == s.tag-1 || known && of.required&(1<<m) != 0
		}
		rule := "optional"
		if required {
			rule = faultMissing
		}
		g.emit(key, rule, at, name, func() func() {
			keys, items := n.keys, n.items
			n.keys = slices.Delete(slices.Clone(keys), i, i+1)
			n.items = slices.Delete(slices.Clone(items), i, i+1)
			return func() { n.keys, n.items = keys, items }
		})
		for _, wrong := range wrongFor(val) {
			g.emit(key, faultType, join(name), wrong, swap(&jnode{kind: wrong[0], raw: wrong}))
		}
		if deep || val.kind != jList && val.kind != jObject && val.kind != jOneOf {
			g.value(value, val, join(name))
		} else if value.kind == '[' {
			// The items of a list of operations are covered by the
			// examples of operations; the list itself is covered here.
			g.items(value, g.alt(value, val), join(name), false)
		} else {
			g.value(value, val, join(name))
		}
	}
	g.emit(s, faultUnknown, at, "zz", func() func() {
		keys, items := n.keys, n.items
		n.keys = append(slices.Clone(keys), "zz")
		n.items = append(slices.Clone(items), &jnode{kind: '0', raw: "1"})
		return func() { n.keys, n.items = keys, items }
	})
	if s.tag != 0 && known {
		// A field of another kind, with a value of its own type.
		for m := range s.members {
			if of.allowed&(1<<m) == 0 {
				g.emit(kindKey{s, kind}, faultForeign, at, s.members[m].name, func() func() {
					keys, items := n.keys, n.items
					n.keys = append(slices.Clone(keys), s.members[m].name)
					n.items = append(slices.Clone(items), parseExample(g.t, sample(s.members[m].val)))
					return func() { n.keys, n.items = keys, items }
				})
				break
			}
		}
	}
}

// sample is a value a shape takes.
func sample(s *shape) string {
	switch s.kind {
	case jText:
		return `"x"`
	case jInt, jNumber:
		return `1`
	case jFlag:
		return `true`
	case jList:
		items := make([]string, max(s.count, 1))
		for i := range items {
			items[i] = sample(s.elem)
		}
		return "[" + strings.Join(items, ",") + "]"
	case jOneOf:
		return sample(s.alts[0])
	}
	var fields []string
	for _, m := range s.members {
		if m.required {
			fields = append(fields, `"`+m.name+`":`+sample(m.val))
		}
	}
	return "{" + strings.Join(fields, ",") + "}"
}

// alt is the alternative of a place that the example's value is of.
func (g *tableBuilder) alt(n *jnode, s *shape) *shape {
	if s.kind != jOneOf {
		return s
	}
	for _, alt := range s.alts {
		if alt.starts(n.first()) {
			g.visited[alt] = true
			return alt
		}
	}
	g.t.Fatalf("%s: the example holds %s where its shape takes none of that type", g.base.Name, n)
	return nil
}

func (g *tableBuilder) value(n *jnode, s *shape, at string) {
	if n.kind == 'n' {
		return
	}
	s = g.alt(n, s)
	switch n.kind {
	case '{':
		g.object(n, s, at, true)
	case '[':
		g.items(n, s, at, true)
	}
}

func (g *tableBuilder) items(n *jnode, s *shape, at string, deep bool) {
	if len(n.items) == 0 {
		return
	}
	g.visited[s] = true
	first := n.items[0]
	swap := func(to string) func() func() {
		return func() func() {
			n.items[0] = &jnode{kind: to[0], raw: to}
			return func() { n.items[0] = first }
		}
	}
	item := at + "[0]"
	if s.elem.null {
		g.emit(s, "nullable", item, "", swap("null"))
	} else {
		g.emit(s, faultNull, item, "", swap("null"))
	}
	for _, wrong := range wrongFor(s.elem) {
		g.emit(s, faultType, item, wrong, swap(wrong))
	}
	if s.count != 0 {
		g.emit(s, faultCount, at, "one item more", func() func() {
			items := n.items
			n.items = append(slices.Clone(items), first)
			return func() { n.items = items }
		})
		g.emit(s, faultCount, at, "one item less", func() func() {
			items := n.items
			n.items = items[:len(items)-1]
			return func() { n.items = items }
		})
	}
	if deep {
		for i, child := range n.items {
			g.value(child, s.elem, fmt.Sprintf("%s[%d]", at, i))
		}
	}
}

// places lists the places of a shape no example reached.
func (g *tableBuilder) places(s *shape, at string, done map[*shape]bool, missing *[]string) {
	if done[s] {
		return
	}
	done[s] = true
	switch s.kind {
	case jOneOf:
		for i, alt := range s.alts {
			if !g.visited[alt] {
				*missing = append(*missing, fmt.Sprintf("%s: alternative %d, a %s", at, i+1, kindName(alt.kind)))
			}
			g.places(alt, at, done, missing)
		}
	case jList:
		if !g.visited[s] {
			*missing = append(*missing, at+": a list with an item")
		}
		g.places(s.elem, at+"[]", done, missing)
	case jObject:
		kinds := make([]string, 0, len(s.kinds))
		for kind := range s.kinds {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		for _, kind := range kinds {
			if !g.visited[kindKey{s, kind}] {
				*missing = append(*missing, at+": the kind "+kind)
			}
		}
		for m := range s.members {
			if !g.visited[memberKey{s, m}] {
				*missing = append(*missing, at+": the field "+s.members[m].name)
			}
			g.places(s.members[m].val, at+"."+s.members[m].name, done, missing)
		}
	}
}

func kindName(k jkind) string {
	return [...]string{jText: "text", jInt: "integer", jNumber: "number", jFlag: "boolean", jList: "list", jObject: "object", jOneOf: "choice"}[k]
}

// TestWireSchemaDepth holds the shapes to the depth the walk keeps a path
// for, and every object to the 32 fields a mask holds.
func TestWireSchemaDepth(t *testing.T) {
	var depth func(s *shape) int
	depth = func(s *shape) int {
		switch s.kind {
		case jList:
			return 1 + depth(s.elem)
		case jOneOf:
			deepest := 0
			for _, alt := range s.alts {
				deepest = max(deepest, depth(alt))
			}
			return deepest
		case jObject:
			if len(s.members) > 32 {
				t.Errorf("an object of %d fields", len(s.members))
			}
			deepest := 0
			for _, m := range s.members {
				deepest = max(deepest, 1+depth(m.val))
			}
			return deepest
		}
		return 0
	}
	for name, s := range map[string]*shape{"frame": frameShape, "operation": opShape, "call": callShape} {
		if d := depth(s); d > wireDepth {
			t.Errorf("the shape of a %s is %d deep, and a path holds %d steps", name, d, wireDepth)
		}
	}
}

// TestConnFrameIsOneReading holds the reading of a connection's frames to
// the reading of each frame alone, on every frame of the table and on the
// same frame with its fields in the opposite order: the reading takes a
// frame exactly when the reader of one kind takes it, names that kind,
// and builds the frame that reader builds. Where the type stands among the
// fields changes nothing.
func TestConnFrameIsOneReading(t *testing.T) {
	fill := strings.NewReplacer("$other", "2", "$row2", "12", "$col2", "22", "$tab", "1", "$row", "11", "$col", "21", "$seq", "5")
	alone := func(data []byte) (kind string, form []byte) {
		var (
			watch    watchFrame
			peek     peekFrame
			action   actionFrame
			part     actionPartFrame
			rebase   rebaseFrame
			presence presenceFrame
			hello    helloFrame
		)
		for _, f := range []struct {
			kind string
			read func([]byte) error
			form func([]byte) []byte
		}{
			{"watch", watch.UnmarshalJSON, watch.appendJSON}, {"peek", peek.UnmarshalJSON, peek.appendJSON},
			{"action", action.UnmarshalJSON, action.appendJSON}, {"action_part", part.UnmarshalJSON, part.appendJSON},
			{"rebase", rebase.UnmarshalJSON, rebase.appendJSON}, {"presence", presence.UnmarshalJSON, presence.appendJSON},
			{"hello", hello.UnmarshalJSON, hello.appendJSON},
		} {
			if f.read(data) == nil {
				if kind != "" {
					t.Fatalf("%s is read as a %s and as a %s", data, kind, f.kind)
				}
				kind, form = f.kind, f.form(nil)
			}
		}
		return kind, form
	}
	form := func(f *connFrame) []byte {
		switch f.kind {
		case "watch":
			return f.watch.appendJSON(nil)
		case "peek":
			return f.peek.appendJSON(nil)
		case "action":
			return f.action.appendJSON(nil)
		case "action_part":
			return f.part.appendJSON(nil)
		case "rebase":
			return f.rebase.appendJSON(nil)
		case "presence":
			return f.presence.appendJSON(nil)
		}
		return f.hello.appendJSON(nil)
	}
	read, refused := 0, 0
	check := func(name string, data []byte) {
		kind, want := alone(data)
		f, err := readConnFrame(data)
		switch {
		case kind == "" && err == nil:
			t.Errorf("%s: read as a %s, and no reader of a frame takes it\n\t%s", name, f.kind, data)
		case kind != "" && err != nil:
			t.Errorf("%s: refused, and it is a %s\n\t%s", name, kind, data)
		case kind == "":
			refused++
		case f.kind != kind:
			t.Errorf("%s: read as a %s, and it is a %s\n\t%s", name, f.kind, kind, data)
		case !bytes.Equal(form(f), want):
			t.Errorf("%s: read as %s, and its own reader reads %s\n\t%s", name, form(f), want, data)
		default:
			read++
		}
	}
	for _, row := range loadFrameTable(t) {
		if row.Kind == "call" {
			continue
		}
		raw := fill.Replace(row.Raw)
		check(row.Name, []byte(raw))
		// The fields of the frame in the opposite order, where the row is
		// compact JSON that the test's own reader takes.
		if n := reversedFields(raw); n != "" {
			check(row.Name+", fields reversed", []byte(n))
		}
	}
	if read < 300 || refused < 300 {
		t.Errorf("%d frames were read and %d refused, and the table holds hundreds of each", read, refused)
	}
}

// reversedFields writes a frame with its fields in the opposite order, or
// "" for a text that is no object in compact JSON.
func reversedFields(raw string) (out string) {
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	i := 0
	var skip func()
	skip = func() {
		switch raw[i] {
		case '{', '[':
			for i++; raw[i] != '}' && raw[i] != ']'; {
				skip()
				if raw[i] == ':' || raw[i] == ',' {
					i++
				}
			}
			i++
		case '"':
			for i++; raw[i] != '"'; i++ {
				if raw[i] == '\\' {
					i++
				}
			}
			i++
		default:
			for !strings.ContainsRune(",:]}", rune(raw[i])) {
				i++
			}
		}
	}
	if raw[0] != '{' {
		return ""
	}
	var fields []string
	for i = 1; raw[i] != '}'; {
		start := i
		skip() // the name
		i++    // the colon
		skip() // the value
		fields = append(fields, raw[start:i])
		if raw[i] == ',' {
			i++
		}
	}
	if i != len(raw)-1 || len(fields) < 2 {
		return ""
	}
	slices.Reverse(fields)
	return "{" + strings.Join(fields, ",") + "}"
}
