package design

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func files(m map[string]string) Getter {
	return func(p string) (File, bool) { v, ok := m[p]; return File{Data: []byte(v), Gen: 1}, ok }
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestApplyOps(t *testing.T) {
	fs := map[string]string{
		"canvas.json": `{"title":"T","boards":{"a.html":{"x":0,"y":0,"w":400,"h":300}},"order":["a.html"],"custom":{"k":1}}`,
		"a.html":      `<html><head><script type="application/json" data-flats-tweaks>{"accent":{"type":"color","value":"#ffffff"}}</script></head><body><h1>Old <b>title</b></h1></body></html>`,
	}
	out, err := Apply(Op{Kind: "board.set", Gen: 1, Path: "a.html", Fields: map[string]json.RawMessage{"x": raw("120")}, Base: map[string]json.RawMessage{"x": raw("0")}}, files(fs))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out["canvas.json"]), `"x": 120`) || !strings.Contains(string(out["canvas.json"]), `"custom"`) {
		t.Fatalf("canvas = %s", out["canvas.json"])
	}
	if _, err := Apply(Op{Kind: "board.set", Gen: 1, Path: "a.html", Fields: map[string]json.RawMessage{"x": raw("5")}, Base: map[string]json.RawMessage{"x": raw("99")}}, files(fs)); !errors.Is(err, ErrStale) {
		t.Fatalf("stale move: %v", err)
	}
	n, err := NodeOf([]byte(fs["a.html"]), 0)
	if err != nil || n.Inner != "Old <b>title</b>" {
		t.Fatalf("node = %+v %v", n, err)
	}
	if _, err := Apply(Op{Kind: "text.set", Gen: 2, Path: "a.html", Node: 0, Tag: "h1", InnerHash: n.InnerHash, HTML: "x"}, files(fs)); !errors.Is(err, ErrStale) {
		t.Fatalf("stale gen accepted: %v", err)
	}
	out, err = Apply(Op{Kind: "text.set", Gen: 1, Path: "a.html", Node: 0, Tag: "h1", InnerHash: n.InnerHash, HTML: "New <i>head</i><script>x</script>"}, files(fs))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out["a.html"]), "<h1>New <i>head</i>x</h1>") {
		t.Fatalf("text = %s", out["a.html"])
	}
	if _, err := Apply(Op{Kind: "text.set", Gen: 1, Path: "a.html", Node: 0, Tag: "h1", InnerHash: "00", HTML: "x"}, files(fs)); !errors.Is(err, ErrStale) {
		t.Fatalf("stale text: %v", err)
	}
	out, err = Apply(Op{Kind: "tweak.set", Gen: 1, Path: "a.html", Name: "accent", Value: raw(`"#000000"`), BaseValue: raw(`"#ffffff"`)}, files(fs))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out["a.html"]), `"value": "#000000"`) || !strings.Contains(string(out["a.html"]), "<h1>Old <b>title</b></h1>") {
		t.Fatalf("tweak = %s", out["a.html"])
	}
	out, err = Apply(Op{Kind: "note.add", Gen: 1, ID: "n1", Fields: map[string]json.RawMessage{"x": raw("1"), "y": raw("2"), "text": raw(`"hi"`)}}, files(fs))
	if err != nil || !strings.Contains(string(out["canvas.json"]), `"n1"`) {
		t.Fatalf("note add: %v %s", err, out["canvas.json"])
	}
}

func TestOrderOpOnPartialOrder(t *testing.T) {
	fs := map[string]string{
		"canvas.json": `{"title":"T","boards":{"a.html":{"x":0,"y":0,"w":400,"h":300},"b.html":{"x":500,"y":0,"w":400,"h":300}},"order":["a.html"]}`,
		"a.html":      "<p>a</p>", "b.html": "<p>b</p>",
	}
	out, err := Apply(Op{Kind: "board.order", Gen: 1, Path: "b.html", To: "back", BaseIndex: 1}, files(fs))
	if err != nil || !strings.Contains(string(out["canvas.json"]), `"order": [
    "b.html",
    "a.html"
  ]`) {
		t.Fatalf("order = %v %s", err, out["canvas.json"])
	}
}
