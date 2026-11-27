package sheet

import (
	"fmt"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// TestFrameArrayLengths sends frames whose fixed-length arrays have the
// wrong number of items. Each is a protocol error, and none is read as if
// the missing items were zero or the extra ones absent.
func TestFrameArrayLengths(t *testing.T) {
	e := start(t)
	e.mustAgent(numbers)
	s := serve(e)
	tab, row, col := e.ids("C1")
	seq := e.seq()
	set := func(at string) string {
		return fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":"=A1","at":%s}]}`, seq, tab, row, col, at)
	}
	rebase := func(at string) string {
		return fmt.Sprintf(`{"type":"rebase","id":1,"tab":%d,"row":%d,"col":%d,"text":"=A1","at":%s,"base":"%d","through":"%d"}`, tab, row, col, at, seq, seq)
	}
	presence := func(active string) string {
		return fmt.Sprintf(`{"type":"presence","tab":%d,"active":%s}`, tab, active)
	}
	clear := func(rows string) string {
		return fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"clear","tab":%d,"rows":%s,"cols":[[%d,1]]}]}`, seq, tab, rows, col)
	}
	for _, c := range []struct{ name, frame string }{
		{"a set with one index", set(`[0]`)},
		{"a set with three indexes", set(`[0,2,5]`)},
		{"a set with no index", set(`[]`)},
		{"a rebase with one index", rebase(`[0]`)},
		{"a rebase with three indexes", rebase(`[0,2,5]`)},
		{"a rebase without indexes", rebase(`null`)},
		{"presence with one id", presence(fmt.Sprintf(`[%d]`, row))},
		{"presence with three ids", presence(fmt.Sprintf(`[%d,%d,1]`, row, col))},
		{"presence with an unknown field", `{"type":"presence","tab":1,"extra":true}`},
	} {
		conn := s.dial("private", `{"type":"hello","version":1,"name":"A","pending":0}`)
		conn.next("welcome")
		conn.send(c.frame)
		if f, ok := conn.wait("fatal", "ack", "rebased"); !ok || f["type"] != "fatal" || f["code"] != "protocol" {
			t.Errorf("%s was answered with %v", c.name, f)
		}
	}
	// A run of ids is read inside its operation, which is refused.
	for _, rows := range []string{fmt.Sprintf(`[[%d]]`, row), fmt.Sprintf(`[[%d,1,7]]`, row), `[[]]`} {
		conn := s.dial("private", `{"type":"hello","version":1,"name":"A","pending":0}`)
		conn.next("welcome")
		conn.send(clear(rows))
		if ack := conn.next("ack", "fatal"); fmt.Sprint(ack["results"]) != "[map[reason:invalid status:refused]]" {
			t.Errorf("the run %s was answered with %v", rows, ack)
		}
	}
	if e.seq() != seq {
		t.Fatalf("a malformed frame changed the sheet: sequence %d", e.seq())
	}
	// The well-formed frames are taken.
	conn := s.dial("private", `{"type":"hello","version":1,"name":"A","pending":0}`)
	conn.next("welcome")
	conn.send(presence(fmt.Sprintf(`[%d,%d]`, row, col)))
	conn.send(rebase(`[0,2]`))
	if r := conn.next("rebased", "fatal"); r["text"] != "=A1" || fmt.Sprint(r["at"]) != "[0 2]" {
		t.Fatalf("a well-formed rebase was answered with %v", r)
	}
	conn.send(set(`[0,2]`))
	if ack := conn.next("ack", "fatal"); ack["type"] != "ack" {
		t.Fatalf("a well-formed set was answered with %v", ack)
	}
	e.want("C1", "1")
}

// TestFrameFieldRules sends frames and operations beside the ones of the
// shared frame table. A field twice, a field of another kind of operation
// and a range that has not the fields of its place are protocol errors. A
// field name written with an escape is the name it spells. An id that is
// no positive integer is a protocol error in a frame and refuses an
// operation as invalid, as an unknown operation and a side other than
// before and after do. Nothing of such a frame is carried out.
func TestFrameFieldRules(t *testing.T) {
	e := start(t)
	e.mustAgent(numbers)
	s := serve(e)
	tab, row, col := e.ids("C1")
	seq := e.seq()
	action := func(op string) string {
		return fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[%s]}`, seq, op)
	}
	rect := fmt.Sprintf(`"rows":[[%d,1]],"cols":[[%d,1]]`, row, col)
	for _, c := range []struct{ name, frame, want string }{
		{"a peek at tab 0", fmt.Sprintf(`{"type":"peek","id":1,"tab":0,"row":%d,"col":%d}`, row, col), "fatal protocol"},
		{"a peek at an id of 2^53", fmt.Sprintf(`{"type":"peek","id":1,"tab":%d,"row":9007199254740992,"col":%d}`, tab, col), "fatal protocol"},
		{"a watch of tab 0", `{"type":"watch","generation":1,"tab":0}`, "fatal protocol"},
		{"a peek with a field twice", fmt.Sprintf(`{"type":"peek","id":1,"tab":null,"tab":%d,"row":%d,"col":%d}`, tab, row, col), "fatal protocol"},
		{"a peek with an escaped field name", fmt.Sprintf(`{"type":"peek","id":1,"t%su0061b":%d,"row":%d,"col":%d}`, `\`, tab, row, col), "read"},
		{"a rebase of row 0", fmt.Sprintf(`{"type":"rebase","id":1,"tab":%d,"row":0,"col":%d,"text":"=A1","at":[0,2],"base":"%d","through":"%d"}`, tab, col, seq, seq), "fatal protocol"},
		{"a set with a width", action(fmt.Sprintf(`{"op":"set","tab":%d,"row":%d,"col":%d,"text":"x","width":90}`, tab, row, col)), "fatal protocol"},
		{"a set with its tab twice", action(fmt.Sprintf(`{"op":"set","tab":%d,"tab":%d,"row":%d,"col":%d,"text":"x"}`, tab, tab, row, col)), "fatal protocol"},
		{"a fill whose source names a tab", action(fmt.Sprintf(`{"op":"fill","tab":%d,"source":{"tab":%d,%s},"target":{%s}}`, tab, tab, rect, rect)), "fatal protocol"},
		{"a copy whose source names no tab", action(fmt.Sprintf(`{"op":"copy","from":{%s},"to":{"tab":%d,%s},"mode":"all"}`, rect, tab, rect)), "fatal protocol"},
		{"a copy to a range without cols", action(fmt.Sprintf(`{"op":"copy","from":{"tab":%d,%s},"to":{"tab":%d,"rows":[[%d,1]]},"mode":"all"}`, tab, rect, tab, row)), "fatal protocol"},
		{"a copy to a range with null rows", action(fmt.Sprintf(`{"op":"copy","from":{"tab":%d,%s},"to":{"tab":%d,"rows":null,"cols":[[%d,1]]},"mode":"all"}`, tab, rect, tab, col)), "fatal protocol"},
		{"a move before null", action(fmt.Sprintf(`{"op":"move_tab","tab":%d,"before":null}`, tab)), "applied"},
		{"a set in tab 0", action(fmt.Sprintf(`{"op":"set","tab":0,"row":%d,"col":%d,"text":"x"}`, row, col)), "refused invalid"},
		{"a set in column 2^53", action(fmt.Sprintf(`{"op":"set","tab":%d,"row":%d,"col":9007199254740992,"text":"x"}`, tab, row)), "refused invalid"},
		{"a sort by column 0", action(fmt.Sprintf(`{"op":"sort","tab":%d,"rows":"all","by":0}`, tab)), "refused invalid"},
		{"an insert beside row 0", action(fmt.Sprintf(`{"op":"insert_rows","tab":%d,"beside":0,"side":"before","count":1}`, tab)), "refused invalid"},
		{"an insert on a side that is none", action(fmt.Sprintf(`{"op":"insert_rows","tab":%d,"beside":%d,"side":"below","count":1}`, tab, row)), "refused invalid"},
		{"a copy from tab 0", action(fmt.Sprintf(`{"op":"copy","from":{"tab":0,%s},"to":{"tab":%d,%s},"mode":"all"}`, rect, tab, rect)), "refused invalid"},
		{"an operation of an unknown kind", action(`{"op":"explode","tab":1}`), "refused invalid"},
	} {
		conn := s.dial("private", `{"type":"hello","version":1,"name":"A","pending":0}`)
		conn.next("welcome")
		conn.send(c.frame)
		got := "closed"
		if f, ok := conn.wait("fatal", "ack", "peeked", "rebased", "tab_head"); ok {
			switch f["type"] {
			case "fatal":
				got = fmt.Sprint("fatal ", f["code"])
			case "ack":
				res := f["results"].([]any)[0].(map[string]any)
				got = fmt.Sprint(res["status"])
				if res["reason"] != nil {
					got += fmt.Sprint(" ", res["reason"])
				}
			default:
				got = "read"
			}
		}
		if got != c.want {
			t.Errorf("%s was answered with %q, want %q\n\t%s", c.name, got, c.want, c.frame)
		}
		conn.ws.Close(1000, "")
		if c.want != "applied" && e.seq() != seq {
			t.Fatalf("%s moved the sequence to %d", c.name, e.seq())
		}
		seq = e.seq()
	}
}

// TestShapeCheckAllocatesNothing checks a frame and an operation against
// their shapes, as every frame is checked while it is read: the check
// itself allocates nothing, whatever the values hold. What a frame
// allocates is the value built from it.
func TestShapeCheckAllocatesNothing(t *testing.T) {
	frame := []byte(`{"type":"peek","id":7,"tab":1,"row":20,"col":3}`)
	op := []byte(`{"op":"write","tab":1,"rows":[[1,2],{"new":1}],"c\u006fls":[[1,2]],"values":[["a \"quoted\" } text","b"],["c","d"],["e","f"]]}`)
	set := []byte(`{"op":"format","tab":1,"rows":"all","cols":[3],"set":{"bold":null,"fill":"red-600","number":{"kind":"currency","symbol":"$","decimals":2}}}`)
	if n := testing.AllocsPerRun(100, func() {
		if kind, fault := walk(frame, frameShape, nil); fault != nil || kind != "peek" {
			t.Fatal(fault)
		}
		if _, fault := walk(op, opShape, nil); fault != nil {
			t.Fatal(fault)
		}
		if _, fault := walk(set, opShape, nil); fault != nil {
			t.Fatal(fault)
		}
	}); n != 0 {
		t.Fatalf("a check of a frame against its shape allocates %v times", n)
	}
	// The value of a peek is its four numbers: reading it allocates
	// nothing either.
	var peek peekFrame
	if n := testing.AllocsPerRun(100, func() {
		if err := peek.UnmarshalJSON(frame); err != nil || peek.Row != 20 {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("reading a peek allocates %v times", n)
	}
	for _, bad := range []string{``, `[]`, `{`, `{"type"`, `{"type":`, `{"type":"peek"`, `{"type":"peek",}`, `{"type":"peek" "id":1}`, `{"type":"pe`, `{"id":[1,{"a":"}"`, `{"nope":1}`, `{"type":1,"type":2}`,
		`{"type":"peek","id":7,"tab":1,"row":20,"col":3} x`, `{"type":"peek","id":7,"tab":1,"row":20,"col":03}`, `{"type":"peek","id":7,"tab":1,"row":20,"col":3,}`, "{\"type\":\"peek\",\"id\":7,\"tab\":1,\"row\":20,\"col\":3}\x00"} {
		if _, fault := walk([]byte(bad), frameShape, nil); fault == nil {
			t.Errorf("the walk read %q", bad)
		}
	}
}

// TestOperationSlotsFollowMembers holds the slots of the members of an
// operation to the order of its member list: the bit of a member in has,
// and the member written back for a bit, are worked out from that order.
func TestOperationSlotsFollowMembers(t *testing.T) {
	for _, c := range []struct {
		name    string
		members []member
		first   int
	}{{"an operation of an action", opMembers, sOpKind}, {"an operation of a call", agentOpMembers, cOpKind}} {
		for at, m := range c.members {
			if m.val.slot != c.first+at {
				t.Errorf("%s: member %d, %s, has slot %d, want %d", c.name, at, m.name, m.val.slot, c.first+at)
			}
		}
	}
	if sOpBefore-sOpKind >= 32 || cOpCSV-cOpKind >= 32 {
		t.Error("an operation has more members than its has holds bits")
	}
}

// TestNumberFormatReadsAsItIsWritten reads every number format of rule 6
// from the object the formula package writes for it, inside a format
// operation, and gets the format back: the kinds, the fields of each kind
// and the patterns of the wire schema are those of the formula package.
func TestNumberFormatReadsAsItIsWritten(t *testing.T) {
	var formats []formula.NumberFormat
	for _, decimals := range []uint8{0, 2, 10} {
		formats = append(formats,
			formula.NumberFormat{Kind: formula.NumberFixed, Decimals: decimals},
			formula.NumberFormat{Kind: formula.NumberFixed, Decimals: decimals, Thousands: true},
			formula.NumberFormat{Kind: formula.NumberPercent, Decimals: decimals},
			formula.NumberFormat{Kind: formula.NumberCurrency, Decimals: decimals, Symbol: "$"},
			formula.NumberFormat{Kind: formula.NumberCurrency, Decimals: decimals, Symbol: "USD"})
	}
	for date := formula.DateISO; date <= formula.DateMonth; date++ {
		formats = append(formats, formula.NumberFormat{Kind: formula.NumberDate, Date: date})
		for clock := formula.TimeHM; clock <= formula.TimeAMPM; clock++ {
			formats = append(formats, formula.NumberFormat{Kind: formula.NumberDateTime, Date: date, Time: clock})
		}
	}
	for clock := formula.TimeHM; clock <= formula.TimeAMPM; clock++ {
		formats = append(formats, formula.NumberFormat{Kind: formula.NumberTime, Time: clock})
	}
	formats = append(formats, formula.NumberFormat{Kind: formula.NumberText})
	kinds := map[formula.NumberKind]bool{formula.NumberGeneral: true}
	for _, nf := range formats {
		kinds[nf.Kind] = true
		data, err := nf.MarshalJSON()
		if err != nil {
			t.Fatalf("%+v: %v", nf, err)
		}
		var op personOp
		if err := op.UnmarshalJSON([]byte(`{"op":"format","tab":1,"rows":"all","cols":"all","set":{"number":` + string(data) + `}}`)); err != nil || op.setBad != nil || op.Set.f.Number != nf {
			t.Errorf("%s reads as %+v (%v, %v), want %+v", data, op.Set.f.Number, err, op.setBad, nf)
		}
		u, err := DecodeUpdate([]byte(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"number":` + string(data) + `}}]}`))
		if err != nil || u.Ops[0].patch.f.Number != nf {
			t.Errorf("a call with %s: %v", data, err)
		}
	}
	// General is what a cell without a number format has.
	var op personOp
	if err := op.UnmarshalJSON([]byte(`{"op":"format","tab":1,"rows":"all","cols":"all","set":{"number":{"kind":"general"}}}`)); err != nil || op.setBad != nil || op.Set.f.Number != (formula.NumberFormat{}) || !op.Set.has(propNumber) {
		t.Errorf("general reads as %+v (%v, %v)", op.Set, err, op.setBad)
	}
	if len(kinds) != len(numberKinds) || len(kinds) != len(numberShape.kinds) {
		t.Errorf("the test reads %d kinds, the names list %d and the shape %d", len(kinds), len(numberKinds), len(numberShape.kinds))
	}
}
