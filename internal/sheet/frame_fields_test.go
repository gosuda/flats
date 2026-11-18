package sheet

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// frameVerdict sends one frame on a connection of its own and tells what
// the engine did with it: "read", or how the connection ended.
func frameVerdict(t *testing.T, e *env, s *site, frame string) string {
	t.Helper()
	tab, row, col := e.ids("A1")
	c := s.dial("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	c.next("welcome")
	c.send(frame)
	// Frames are read in order, so the answer to this peek follows the
	// frame under test unless that frame ended the connection.
	_ = c.ws.Send(fmt.Sprintf(`{"type":"peek","id":424242,"tab":%d,"row":%d,"col":%d}`, tab, row, col))
	defer c.ws.Close(1000, "")
	for {
		got, ok := c.wait("fatal", "peeked")
		switch {
		case !ok:
			return "closed"
		case got["type"] == "fatal":
			return fmt.Sprint("fatal ", got["code"])
		case got["id"] == float64(424242):
			return "read"
		}
	}
}

// TestClientFrameFields holds every client frame against Appendix A field
// by field: a frame without a required field, or with null for any field,
// ends the connection as a protocol error, and the whole frame is read.
func TestClientFrameFields(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["a","b"],["c","d"]]}]}`)
	s := serve(e)
	tab, row, col := e.ids("A1")
	seq := e.seq()
	frames := []struct {
		frame    string
		required []string
	}{
		{fmt.Sprintf(`{"type":"watch","generation":1,"tab":%d}`, tab), []string{"generation", "tab"}},
		{fmt.Sprintf(`{"type":"peek","id":7,"tab":%d,"row":%d,"col":%d}`, tab, row, col), []string{"id", "tab", "row", "col"}},
		{fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"undo"}]}`, seq), []string{"n", "base", "ops"}},
		{`{"type":"action_part","n":"1","part":0,"parts":2,"data":"{\"type\":"}`, []string{"n", "part", "parts", "data"}},
		{fmt.Sprintf(`{"type":"rebase","id":3,"tab":%d,"row":%d,"col":%d,"text":"=A1","at":[0,0],"base":"%d","through":"%d"}`, tab, row, col, seq, seq),
			[]string{"id", "tab", "row", "col", "text", "at", "base", "through"}},
		{fmt.Sprintf(`{"type":"presence","tab":%d,"active":[%d,%d],"range":{"rows":[[%d,1]],"cols":[[%d,1]]},"editing":true}`, tab, row, col, row, col), []string{"tab"}},
	}
	for _, f := range frames {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(f.frame), &fields); err != nil {
			t.Fatal(err)
		}
		kind := strings.Trim(string(fields["type"]), `"`)
		if got := frameVerdict(t, e, s, f.frame); got != "read" {
			t.Errorf("%s: the whole frame got %q", kind, got)
		}
		names := make([]string, 0, len(fields))
		for name := range fields {
			if name != "type" {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		with := func(name string, value json.RawMessage) string {
			changed := map[string]json.RawMessage{}
			for k, v := range fields {
				changed[k] = v
			}
			if value == nil {
				delete(changed, name)
			} else {
				changed[name] = value
			}
			out, _ := json.Marshal(changed)
			return string(out)
		}
		for _, name := range names {
			if got := frameVerdict(t, e, s, with(name, json.RawMessage("null"))); got != "fatal protocol" {
				t.Errorf("%s with %s null got %q", kind, name, got)
			}
			required := false
			for _, r := range f.required {
				required = required || r == name
			}
			want := "read"
			if required {
				want = "fatal protocol"
			}
			if got := frameVerdict(t, e, s, with(name, nil)); got != want {
				t.Errorf("%s without %s got %q, want %q", kind, name, got, want)
			}
		}
	}
	// The range of a presence frame names both of its lists.
	for _, rng := range []string{`{"rows":[[1,1]]}`, `{"cols":[[1,1]]}`, `{"rows":null,"cols":[[1,1]]}`, `{}`} {
		if got := frameVerdict(t, e, s, fmt.Sprintf(`{"type":"presence","tab":%d,"range":%s}`, tab, rng)); got != "fatal protocol" {
			t.Errorf("presence with the range %s got %q", rng, got)
		}
	}
}

// TestHelloFields does the same for the first frame of a connection.
func TestHelloFields(t *testing.T) {
	e := start(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	first := s.dial("private", `{"type":"hello","version":1,"name":"Ann","pending":0}`)
	session := first.next("welcome")["session"].(string)
	first.ws.Close(1000, "")
	whole := map[string]json.RawMessage{
		"type": json.RawMessage(`"hello"`), "version": json.RawMessage("1"), "pending": json.RawMessage("0"),
		"session": json.RawMessage(`"` + session + `"`), "watermark": json.RawMessage(fmt.Sprintf(`"%d"`, e.seq())), "tab": json.RawMessage(fmt.Sprint(tab)),
	}
	verdict := func(fields map[string]json.RawMessage) string {
		hello, _ := json.Marshal(fields)
		c := s.dial("private", string(hello))
		defer c.ws.Close(1000, "")
		got, ok := c.wait("fatal", "welcome")
		if !ok {
			return "closed"
		}
		return fmt.Sprint(got["type"], " ", got["code"])
	}
	if got := verdict(whole); got != "welcome <nil>" {
		t.Fatalf("the whole hello got %q", got)
	}
	for name := range whole {
		if name == "type" {
			continue
		}
		changed := map[string]json.RawMessage{}
		for k, v := range whole {
			changed[k] = v
		}
		changed[name] = json.RawMessage("null")
		if got := verdict(changed); got != "fatal protocol" {
			t.Errorf("hello with %s null got %q", name, got)
		}
		delete(changed, name)
		want := "welcome <nil>"
		if name == "version" || name == "pending" {
			want = "fatal protocol"
		}
		if got := verdict(changed); got != want {
			t.Errorf("hello without %s got %q, want %q", name, got, want)
		}
	}
}
