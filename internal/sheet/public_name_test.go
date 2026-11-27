package sheet

import (
	"fmt"
	"slices"
	"testing"
)

// A browser keeps the name a person gave on a Private route and sends it
// in its first hello, whatever the route is by then. A Public route does
// not read it: the reader gets the welcome and the sheet as any Public
// reader does, without a session, a name or presence, and cannot edit.
// What only a page that was open on a Private route can send, a session, a
// watermark or pending actions, still ends a Public connection. An empty
// session names none, so a hello that holds one is read like one without.
func TestAPublicHelloWithANameReadsTheSheet(t *testing.T) {
	e := start(t)
	e.mustAgent(numbers)
	s := serve(e)
	tab := e.tabID("Sheet1")
	const named = `{"type":"hello","version":1,"name":"Ann","pending":0}`
	editor := s.dial("private", named)
	ann := editor.next("welcome")
	if me := ann["me"].(map[string]any); me["label"] != "Ann" {
		t.Fatalf("the Private session = %v", me)
	}

	// welcomed reads the answer to a Public hello: a welcome at the
	// current sequence with the tab list and nothing of a session.
	welcomed := func(c *client, hello string) {
		t.Helper()
		welcome := c.next("welcome", "fatal")
		if welcome["type"] != "welcome" || welcome["access"] != "public" || welcome["seq"] != fmt.Sprint(e.seq()) || welcome["tabs"] == nil {
			t.Fatalf("the Public hello %s got %v", hello, welcome)
		}
		for _, field := range []string{"session", "session_id", "me"} {
			if _, has := welcome[field]; has {
				t.Fatalf("the Public welcome to %s carries %s: %v", hello, field, welcome)
			}
		}
	}
	const emptySession = `{"type":"hello","version":1,"session":"","name":"Ann","pending":0}`
	welcomed(s.dial("public", emptySession), emptySession)
	reader := s.dial("public", named)
	welcomed(reader, named)
	_, cells := reader.watch(1, tab)
	if b3 := cells[cellKey(e, "B3")]; len(b3) < 3 || b3[2] != 30.0 {
		t.Fatalf("the reader sees B3 = %v", b3)
	}

	// The name opened no session: another editor sees one person, and
	// no presence reaches the reader.
	other := s.dial("private", `{"type":"hello","version":1,"name":"Bo","pending":0}`)
	other.next("welcome")
	_, row, col := e.ids("A1")
	editor.send(fmt.Sprintf(`{"type":"presence","tab":%d,"active":[%d,%d]}`, tab, row, col))
	var people []any
	for len(people) == 0 {
		people = other.next("presence")["people"].([]any)
	}
	if len(people) != 1 || people[0].(map[string]any)["label"] != "Ann" {
		t.Fatalf("presence = %v", people)
	}
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"C1","rows":[["later"]]}]}`)
	if d := reader.next("delta"); d["from"] != nil {
		t.Fatalf("a Public delta names its author: %v", d)
	}
	// An agent write names no author on any connection. A person's edit
	// names its session and number to a Private connection, and nothing
	// to the named reader.
	if d := other.next("delta"); d["from"] != nil {
		t.Fatalf("the delta of an agent write names an author: %v", d)
	}
	editor.send(fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"set","tab":%d,"row":%d,"col":%d,"text":"mine","at":[0,0]}]}`, e.seq(), tab, row, col))
	if from, _ := other.next("delta")["from"].(map[string]any); from["session_id"] != ann["session_id"] || from["n"] != "1" {
		t.Fatalf("a Private delta of a person's edit names %v, want action 1 of session %v", from, ann["session_id"])
	}
	if d := reader.next("delta"); d["from"] != nil || d["seq"] != fmt.Sprint(e.seq()) {
		t.Fatalf("the Public delta of a person's edit at %d: %v", e.seq(), d)
	}
	if slices.Contains(reader.kinds(), "presence") {
		t.Fatal("a Public connection received presence")
	}
	seq := e.seq()
	reader.send(fmt.Sprintf(`{"type":"action","n":"1","base":"%d","ops":[{"op":"append_rows","tab":%d,"count":1}]}`, seq, tab))
	if f := reader.next("fatal"); f["code"] != "forbidden" || e.seq() != seq {
		t.Fatalf("an action of the named reader: %v at %d, was %d", f, e.seq(), seq)
	}

	for _, hello := range []string{
		`{"type":"hello","version":1,"session":"0123456789abcdef0123456789abcdef","name":"Ann","pending":0}`,
		`{"type":"hello","version":1,"name":"Ann","pending":1}`,
		fmt.Sprintf(`{"type":"hello","version":1,"name":"Ann","watermark":"%d","tab":%d,"pending":0}`, seq, tab),
	} {
		if f := s.dial("public", hello).next("welcome", "fatal"); f["code"] != "forbidden" {
			t.Fatalf("%s on a Public route: %v", hello, f)
		}
	}
}
