package sheet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// agentRaw sends the bytes of an update_sheet call as an agent's request
// carries them: nothing reads or rewrites them before the decoder.
func (e *env) agentRaw(call string) (res WriteResult, err error) {
	e.t.Helper()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("the call panicked: %v", p)
		}
	}()
	u, err := DecodeUpdate([]byte(strings.ReplaceAll(call, "$state", e.state())))
	if err != nil {
		return WriteResult{}, err
	}
	return e.in.Update(context.Background(), u, e.b.other)
}

// TestCallFieldSpellings sends calls whose field names differ from the
// schema by case, are written twice, or are written with an escape. A name
// in another case is an unknown field and a name twice is refused: such a
// call is invalid, changes nothing and never reaches the code that applies
// operations. A name written with an escape is the name.
func TestCallFieldSpellings(t *testing.T) {
	e := start(t)
	add := `{"op":"add_tab","name":"Extra"}`
	write := `{"op":"write","tab":"Sheet1","at":"A1","rows":[["x"]]}`
	for _, c := range []struct{ name, call string }{
		{"empty ops, then OPS with an operation", `{"ops":[],"OPS":[` + add + `]}`},
		{"OPS with an operation, then empty ops", `{"OPS":[` + add + `],"ops":[]}`},
		{"one operation in ops, two in OPS", `{"state":"$state","ops":[` + write + `],"OPS":[` + add + `,` + add + `]}`},
		{"two operations in ops, one in Ops", `{"state":"$state","Ops":[` + add + `],"ops":[` + write + `,` + add + `]}`},
		{"ops twice, the first empty", `{"ops":[],"ops":[` + add + `]}`},
		{"ops twice, the second empty", `{"ops":[` + add + `],"ops":[]}`},
		{"State for state", `{"State":"$state","ops":[` + add + `]}`},
		{"OP_ID for op_id", `{"OP_ID":"a","ops":[` + add + `]}`},
		{"Message for message", `{"Message":"m","ops":[` + add + `]}`},
		{"Guard for guard", `{"state":"$state","Guard":{"unchanged":true},"ops":[` + add + `]}`},
		{"Unchanged inside the guard", `{"state":"$state","guard":{"Unchanged":true},"ops":[` + add + `]}`},
		{"unchanged twice inside the guard", `{"state":"$state","guard":{"unchanged":false,"unchanged":true},"ops":[` + add + `]}`},
		{"OP for op", `{"ops":[{"OP":"add_tab","name":"Extra"}]}`},
		{"op and OP", `{"ops":[{"op":"add_tab","OP":"delete_tab","name":"Extra"}]}`},
		{"Name for name", `{"ops":[{"op":"add_tab","Name":"Extra"}]}`},
		{"name twice", `{"ops":[{"op":"add_tab","name":"Extra","name":"More"}]}`},
		{"a property twice in a format", `{"state":"$state","ops":[{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"bold":true,"bold":false}}]}`},
		{"Bold for bold in a format", `{"state":"$state","ops":[{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"Bold":true}}]}`},
		{"kind twice in a number format", `{"state":"$state","ops":[{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"number":{"kind":"text","kind":"general"}}}]}`},
		{"formula twice in a cell", `{"state":"$state","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[{"formula":"=1","formula":"=2"}]]}]}`},
		{"Formula for formula in a cell", `{"state":"$state","ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[{"Formula":"=1"}]]}]}`},
	} {
		seq, tabs := e.seq(), len(e.tabNames())
		_, err := e.agentRaw(c.call)
		if werr := refusal(err); err == nil || werr.Code != "invalid" {
			t.Errorf("%s: %v, want an invalid call\n\t%s", c.name, err, c.call)
		}
		if e.seq() != seq || len(e.tabNames()) != tabs {
			t.Fatalf("%s changed the sheet\n\t%s", c.name, c.call)
		}
	}
	for _, c := range []struct{ name, call string }{
		{"ops written with an escape", `{"\u006fps":[` + add + `]}`},
		{"op and name written with escapes", `{"ops":[{"o\u0070":"add_tab","n\u0061me":"Escaped"}]}`},
		{"a property written with an escape", `{"state":"$state","ops":[{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"b\u006Fld":true}}]}`},
		{"formula written with an escape", `{"state":"$state","ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[[{"f\u006frmula":"=1+1"}]]}]}`},
	} {
		seq := e.seq()
		if _, err := e.agentRaw(c.call); err != nil || e.seq() != seq+1 {
			t.Errorf("%s: %v at sequence %d, want the call applied\n\t%s", c.name, err, e.seq(), c.call)
		}
	}
	e.want("B1", "2")
}

// TestCallRequestIdentity covers which calls are one request under an
// op_id (technical design, section 18.2). A request is what the call asks
// of its sheet: the interface that carries it, the spelling of its bytes
// and a field written at the value it has when it is left out do not make
// another request, and anything the sheet would do differently does.
func TestCallRequestIdentity(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[3],[1],[2]]}]}`)
	hashOf := func(call string) string {
		t.Helper()
		u, err := DecodeUpdate([]byte(strings.ReplaceAll(call, "$state", e.state())))
		if err != nil {
			t.Fatalf("%v\n\t%s", err, call)
		}
		return u.hash
	}
	sort := `{"op":"sort","tab":"Sheet1","rows":"1:3","by":"A"}`
	for _, c := range []struct {
		name string
		same []string
	}{
		{"the tool's slug", []string{
			`{"op_id":"a","state":"$state","ops":[` + sort + `]}`,
			`{"slug":"budget","op_id":"a","state":"$state","ops":[` + sort + `]}`,
			`{"slug":"other","op_id":"a","state":"$state","ops":[` + sort + `]}`,
		}},
		{"the order of fields and white space", []string{
			`{"state":"$state","message":"m","ops":[` + sort + `]}`,
			`{ "ops" : [ {"by":"A","rows":"1:3","tab":"Sheet1","op":"sort"} ], "message":"m", "state":"$state" }`,
		}},
		{"an empty state or message", []string{
			`{"ops":[{"op":"add_tab","name":"Extra"}]}`,
			`{"state":"","message":"","op_id":"","ops":[{"op":"add_tab","name":"Extra"}]}`,
		}},
		{"a flag that is false", []string{
			`{"state":"$state","ops":[` + sort + `,{"op":"clear","tab":"Sheet1","ranges":["A1"]}]}`,
			`{"state":"$state","ops":[{"op":"sort","tab":"Sheet1","rows":"1:3","by":"A","desc":false},{"op":"clear","tab":"Sheet1","ranges":["A1"],"formats":false}]}`,
		}},
		{"a guard field that is false or empty", []string{
			`{"state":"$state","guard":{},"ops":[` + sort + `]}`,
			`{"state":"$state","guard":{"unchanged":false},"ops":[` + sort + `]}`,
			`{"state":"$state","guard":{"ranges":[]},"ops":[` + sort + `]}`,
			`{"state":"$state","guard":{"ranges":[],"unchanged":false},"ops":[` + sort + `]}`,
		}},
		{"a number, a formula and a text in two spellings", []string{
			`{"state":"$state","ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[[1,"=A1","x",null,{"time":"10:00"}]]}]}`,
			`{"state":"$state","ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[[1.0,{"formula":"=A1","value":3},"'x","",{"time":"10:00:00"}]]}]}`,
			`{"state":"$state","ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[[1e0,{"formula":"=A1","error":"#REF!"},"\u0078","'",{"time":"10:00"}]]}]}`,
		}},
	} {
		want := hashOf(c.same[0])
		for _, call := range c.same[1:] {
			if got := hashOf(call); got != want {
				t.Errorf("%s makes another request\n\t%s\n\t%s", c.name, c.same[0], call)
			}
		}
	}
	base := `{"state":"$state","ops":[` + sort + `]}`
	for name, call := range map[string]string{
		"another message":       `{"state":"$state","message":"m","ops":[` + sort + `]}`,
		"no state":              `{"ops":[` + sort + `]}`,
		"a guard":               `{"state":"$state","guard":{},"ops":[` + sort + `]}`,
		"a guard that is set":   `{"state":"$state","guard":{"unchanged":true},"ops":[` + sort + `]}`,
		"a guarded range":       `{"state":"$state","guard":{"ranges":["Sheet1!A1"]},"ops":[` + sort + `]}`,
		"a flag that is true":   `{"state":"$state","ops":[{"op":"sort","tab":"Sheet1","rows":"1:3","by":"A","desc":true}]}`,
		"another operation":     `{"state":"$state","ops":[{"op":"sort","tab":"Sheet1","rows":"1:2","by":"A"}]}`,
		"an operation more":     `{"state":"$state","ops":[` + sort + `,` + sort + `]}`,
		"text for a number":     `{"state":"$state","ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[["1"]]}]}`,
		"a number for the text": `{"state":"$state","ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[[1]]}]}`,
	} {
		if hashOf(call) == hashOf(base) {
			t.Errorf("%s is the same request as\n\t%s\n\t%s", name, base, call)
		}
	}
	if hashOf(`{"state":"$state","ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[["1"]]}]}`) == hashOf(`{"state":"$state","ops":[{"op":"write","tab":"Sheet1","at":"B1","rows":[[1]]}]}`) {
		t.Error("a text and a number are one request")
	}

	// The sheet answers a second spelling under the same op_id with the
	// stored result, and a different request under it with a conflict.
	state := e.state()
	first, err := e.agentRaw(`{"state":"` + state + `","op_id":"sort-1","ops":[` + sort + `]}`)
	if err != nil || first.Replay {
		t.Fatalf("the first send: %v %+v", err, first)
	}
	seq := e.seq()
	again, err := e.agentRaw(`{"slug":"budget","ops":[{"desc":false,"by":"A","rows":"1:3","tab":"Sheet1","op":"sort"}],"op_id":"sort-1","message":"","state":"` + state + `"}`)
	if err != nil || !again.Replay || again.State != first.State || e.seq() != seq {
		t.Fatalf("the second spelling: %v %+v at sequence %d, want the stored result of %+v at %d", err, again, e.seq(), first, seq)
	}
	_, err = e.agentRaw(`{"state":"` + state + `","op_id":"sort-1","ops":[{"op":"sort","tab":"Sheet1","rows":"1:3","by":"A","desc":true}]}`)
	e.wantRefusal(err, "conflict", "op_id_reuse")
	if e.seq() != seq {
		t.Fatalf("a refused call moved the sequence to %d", e.seq())
	}
}

// TestNestedValuesOfAnAction sends actions whose nested values are not the
// ones Appendix A names. Each ends the connection as a protocol error and
// leaves the cells as they were: none is read as an empty text, as the
// last of two fields, or as a field of another spelling.
func TestNestedValuesOfAnAction(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["kept","b"],["c","d"]]},{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"bold":true}}]}`)
	s := serve(e)
	tab, row, col := e.ids("A1")
	seq := e.seq()
	action := func(op string) string {
		return fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[%s]}`, seq, op)
	}
	cell := fmt.Sprintf(`"tab":%d,"rows":[[%d,1]],"cols":[[%d,1]]`, tab, row, col)
	verdict := func(frame string) string {
		conn := s.dial("private", `{"type":"hello","version":1,"name":"A","pending":0}`)
		defer conn.ws.Close(1000, "")
		conn.next("welcome")
		conn.send(frame)
		f, ok := conn.wait("fatal", "ack")
		switch {
		case !ok:
			return "closed"
		case f["type"] == "fatal":
			return fmt.Sprint("fatal ", f["code"])
		}
		res := f["results"].([]any)[0].(map[string]any)
		return strings.TrimSpace(fmt.Sprint(res["status"], " ", res["reason"]))
	}
	for _, c := range []struct{ name, op string }{
		{"a write of a null cell", `{"op":"write",` + cell + `,"values":[[null]]}`},
		{"a write of a null row", `{"op":"write",` + cell + `,"values":[null]}`},
		{"a write of a number cell", `{"op":"write",` + cell + `,"values":[[0]]}`},
		{"a write of a row that is a text", `{"op":"write",` + cell + `,"values":["x"]}`},
		{"a format with a property twice", `{"op":"format",` + cell + `,"set":{"bold":true,"bold":false}}`},
		{"a format with a property twice, the first null", `{"op":"format",` + cell + `,"set":{"bold":null,"bold":false}}`},
		{"a format with a number format field twice", `{"op":"format",` + cell + `,"set":{"number":{"kind":"percent","decimals":1,"decimals":2}}}`},
		{"a format with Bold for bold", `{"op":"format",` + cell + `,"set":{"Bold":false}}`},
		{"a clear with a null row id", fmt.Sprintf(`{"op":"clear","tab":%d,"rows":[null],"cols":[[%d,1]]}`, tab, col)},
		{"a clear with a run that holds a null", fmt.Sprintf(`{"op":"clear","tab":%d,"rows":[[%d,null]],"cols":[[%d,1]]}`, tab, row, col)},
		{"a clear with new twice", fmt.Sprintf(`{"op":"clear","tab":%d,"rows":[{"new":1,"new":2}],"cols":[[%d,1]]}`, tab, col)},
		{"a clear with New for new", fmt.Sprintf(`{"op":"clear","tab":%d,"rows":[{"New":1}],"cols":[[%d,1]]}`, tab, col)},
		{"a clear with ROWS for rows", fmt.Sprintf(`{"op":"clear","tab":%d,"ROWS":[[%d,1]],"cols":[[%d,1]]}`, tab, row, col)},
		{"a set with TEXT beside text", fmt.Sprintf(`{"op":"set","tab":%d,"row":%d,"col":%d,"text":"kept","TEXT":""}`, tab, row, col)},
	} {
		if got := verdict(action(c.op)); got != "fatal protocol" {
			t.Errorf("%s was answered with %q, want a protocol error\n\t%s", c.name, got, c.op)
		}
		if e.seq() != seq {
			t.Fatalf("%s moved the sequence to %d", c.name, e.seq())
		}
	}
	e.wantText("A1", "kept")
	if got := e.formatOf("Sheet1", "A1"); !strings.Contains(got, "bold") {
		t.Fatalf("A1 lost its format: %s", got)
	}
	// A name written with an escape is the name it spells.
	for _, frame := range []string{
		fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"set","tab":%d,"r\u006fw":%d,"col":%d,"text":"escaped"}]}`, seq, tab, row, col),
		fmt.Sprintf(`{"\u0074ype":"action","n":"2","b\u0061se":"%d","ops":[{"\u006Fp":"format",%s,"s\u0065t":{"\u0062old":null}}]}`, seq+1, cell),
	} {
		if got := verdict(frame); got != "applied <nil>" {
			t.Errorf("a frame with escaped field names was answered with %q\n\t%s", got, frame)
		}
	}
	e.wantText("A1", "escaped")
}

// TestWalkStopsAtTheFault gives the walk texts that are long because of
// what is wrong with them: nesting no shape has, and a field name no
// object has. The walk reads neither to its end: it stops at the first
// byte that fits no shape, so a refused text costs what stands before its
// fault, whatever follows.
func TestWalkStopsAtTheFault(t *testing.T) {
	deep := strings.Repeat("[", 100_000) + strings.Repeat("]", 100_000)
	long := strings.Repeat("o", 200_000)
	for _, c := range []struct {
		name, text string
		s          *shape
		rule       string
		before     int // the walk stops before this byte
	}{
		{"a frame nested 100,000 lists deep", `{"type":"peek","id":` + deep + `}`, frameShape, faultType, 24},
		{"lists inside an operation of an action", `{"type":"action","n":"1","base":"1","ops":[{"op":"set","tab":` + deep + `}]}`, frameShape, faultType, 64},
		{"lists inside a list of ids", `{"type":"action","n":"1","base":"1","ops":[{"op":"clear","tab":1,"rows":[[` + deep + `]]}]}`, frameShape, faultType, 80},
		{"a frame with a field name of 200,000 characters", `{"type":"peek","` + long + `":1}`, frameShape, faultUnknown, 24},
		{"a field name of 200,000 escapes", `{"type":"peek","` + strings.Repeat(`\u006f`, 200_000) + `":1}`, frameShape, faultUnknown, 24},
		{"a call nested 100,000 lists deep", `{"ops":[` + deep + `]}`, callShape, faultType, 16},
		{"lists inside a cell of a call", `{"ops":[{"op":"write","tab":"S","at":"A1","rows":[[[` + deep + `]]]}]}`, callShape, faultType, 64},
		{"a call with a field name of 200,000 characters", `{"` + long + `":1,"ops":[]}`, callShape, faultUnknown, 8},
		{"objects nested inside the value of a formula object", `{"ops":[{"op":"write","tab":"S","at":"A1","rows":[[{"formula":"=1","value":` + strings.Repeat(`{"a":`, 50_000) + `1` + strings.Repeat("}", 50_000) + `}]]}]}`, callShape, faultType, 80},
	} {
		_, fault := walk([]byte(c.text), c.s, nil)
		if fault == nil || fault.rule != c.rule || fault.offset >= c.before {
			t.Errorf("%s: the walk stopped with %+v, want a fault of %s before byte %d of %d", c.name, fault, c.rule, c.before, len(c.text))
		}
	}
	// The typed readers refuse the same texts.
	if err := new(peekFrame).UnmarshalJSON([]byte(`{"type":"peek","id":` + deep + `}`)); err == nil {
		t.Error("a peek nested 100,000 lists deep was read")
	}
	if _, err := DecodeUpdate([]byte(`{"ops":[` + deep + `]}`)); refusal(err).Code != "invalid" {
		t.Errorf("a call nested 100,000 lists deep: %v", err)
	}
}

// TestHistoryBodies posts bodies to the History routes that change the
// sheet. A body is read by the shape of its route, like a frame: a field
// twice, in another case, null, of another type or of another route is
// refused as invalid, and no revision is pinned, rolled back to or
// recovered from. A field name written with an escape is the name.
func TestHistoryBodies(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"add_tab","name":"Two"},{"op":"write","tab":"Sheet1","at":"A1","rows":[["kept"]]}]}`)
	rev := e.b.importAll(t)
	e.mustAgent(`{"ops":[{"op":"delete_tab","tab":"Two"}]}`)
	s := serve(e)
	post := func(route, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, s.server.URL+"/private/_sheet/api/"+route, strings.NewReader(body))
		req.Header.Set("X-Flats-Sheet", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	seq, tabs := e.seq(), len(e.tabNames())
	for _, c := range []struct{ route, body string }{
		{"pin", fmt.Sprintf(`{"revision":%d,"revision":%d,"pinned":true}`, rev, rev)},
		{"pin", fmt.Sprintf(`{"revision":%d,"pinned":false,"pinned":true}`, rev)},
		{"pin", fmt.Sprintf(`{"Revision":%d,"pinned":true}`, rev)},
		{"pin", fmt.Sprintf(`{"revision":%d,"PINNED":true}`, rev)},
		{"pin", fmt.Sprintf(`{"revision":%d,"pinned":null}`, rev)},
		{"pin", fmt.Sprintf(`{"revision":%d,"pinned":"true"}`, rev)},
		{"pin", fmt.Sprintf(`{"revision":%d,"pinned":1}`, rev)},
		{"pin", fmt.Sprintf(`{"revision":%d}`, rev)},
		{"pin", fmt.Sprintf(`{"revision":%d,"pinned":true,"tab":1}`, rev)},
		{"pin", fmt.Sprintf(`{"revision":%d.0,"pinned":true}`, rev)},
		{"pin", fmt.Sprintf(`{"revision":"%d","pinned":true}`, rev)},
		{"pin", `{"revision":0,"pinned":true}`},
		{"pin", fmt.Sprintf(`[{"revision":%d,"pinned":true}]`, rev)},
		{"pin", fmt.Sprintf(`{"revision":%d,"pinned":true} {}`, rev)},
		{"rollback", fmt.Sprintf(`{"revision":1,"revision":%d}`, rev)},
		{"rollback", fmt.Sprintf(`{"REVISION":%d}`, rev)},
		{"rollback", fmt.Sprintf(`{"revision":%d,"session":null}`, rev)},
		{"rollback", fmt.Sprintf(`{"revision":%d,"pinned":true}`, rev)},
		{"rollback", `{}`},
		{"recover", fmt.Sprintf(`{"revision":%d,"tab":2,"tab":2}`, rev)},
		{"recover", fmt.Sprintf(`{"revision":%d,"Tab":2}`, rev)},
		{"recover", fmt.Sprintf(`{"revision":%d,"tab":null}`, rev)},
		{"recover", fmt.Sprintf(`{"revision":%d,"tab":"2"}`, rev)},
		{"recover", fmt.Sprintf(`{"revision":%d,"tab":0}`, rev)},
		{"recover", fmt.Sprintf(`{"revision":%d,"tab":-2}`, rev)},
		{"recover", fmt.Sprintf(`{"revision":%d}`, rev)},
	} {
		if code, out := post(c.route, c.body); code != http.StatusBadRequest || out["code"] != "invalid" {
			t.Errorf("%s %s: answered %d %v, want it refused as invalid", c.route, c.body, code, out)
		}
		if e.seq() != seq || len(e.tabNames()) != tabs || e.b.isPinned(rev) {
			t.Fatalf("%s %s changed the sheet or pinned the revision", c.route, c.body)
		}
	}
	if code, out := post("pin", fmt.Sprintf(`{"r\u0065vision":%d,"pinn\u0065d":true}`, rev)); code != http.StatusOK || !e.b.isPinned(rev) {
		t.Fatalf("a pin with escaped field names: %d %v", code, out)
	}
}

// TestWholeNumberDetail refuses whole numbers written in a form the wire
// does not take. The refusal stays invalid with the field as its reason,
// and its detail says how the field takes the number.
func TestWholeNumberDetail(t *testing.T) {
	const plain = "count takes a whole number written in plain digits, such as "
	for _, c := range []struct{ count, detail string }{
		{"2.0", plain + "2, not 2.0 or 2e0"},
		{"2e0", plain + "2, not 2.0 or 2e0"},
		{"2E+0", plain + "2, not 2.0 or 2e0"},
		{"-0", plain + "0, not -0"},
		{"1000000000000000000", "count takes a whole number of at most 18 digits"},
	} {
		_, err := DecodeUpdate([]byte(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":` + c.count + `}]}`))
		werr := refusal(err)
		if werr.Code != "invalid" || werr.Reason != "count" || werr.Detail != "operation 1 (insert_rows): "+c.detail {
			t.Errorf("a count of %s: %s %s %q, want invalid count %q", c.count, werr.Code, werr.Reason, werr.Detail, c.detail)
		}
	}
	// Another JSON type keeps the general detail.
	_, err := DecodeUpdate([]byte(`{"ops":[{"op":"insert_rows","tab":"Sheet1","at":1,"count":"2"}]}`))
	if werr := refusal(err); werr.Reason != "count" || !strings.HasSuffix(werr.Detail, "count has a value of the wrong JSON type") {
		t.Errorf("a count written as text: %s %q", werr.Reason, werr.Detail)
	}
}
