package bundle

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// pageBundle is the clean bundle as a page, with the entry body replaced.
func pageBundle(body string, extra ...File) []File {
	entry := strings.Replace(goodPage, "<body>", "<body>"+body, 1)
	return with(append([]File{{Path: "flats.json", Data: []byte(`{"type":"page","screenshot":"screenshot.png"}`)}, {Path: "index.html", Data: []byte(entry)}}, extra...)...)
}

func dataWarnings(ws []Problem) []Problem {
	var out []Problem
	for _, w := range ws {
		if strings.Contains(w.Message, "loads data from") {
			out = append(out, w)
		}
	}
	return out
}

func TestLintPageCleanBundle(t *testing.T) {
	if ws := Lint(pageBundle("")); len(ws) != 0 {
		t.Fatalf("clean page: %+v", ws)
	}
}

func TestLintPageExternalData(t *testing.T) {
	tests := []struct {
		name, body, js string
		origins        []string
	}{
		{"fetch", `<script>fetch("https://api.example.com/sales.json").then(r => r.json())</script>`, "", []string{"https://api.example.com"}},
		{"d3 csv", `<script type="module">const rows = await d3.csv('https://data.example.org/a.csv')</script>`, "", []string{"https://data.example.org"}},
		{"xhr", `<script>x.open("GET", "http://feed.example.net/x")</script>`, "", []string{"http://feed.example.net"}},
		{"websocket", "<script>new WebSocket(`wss://live.example.com/s`)</script>", "", []string{"wss://live.example.com"}},
		{"event source", `<script>new EventSource("https://events.example.com/s")</script>`, "", []string{"https://events.example.com"}},
		{"papa", `<script>Papa.parse("https://data.example.org/b.csv", {download: true})</script>`, "", []string{"https://data.example.org"}},
		{"bundled js", "", `fetch("https://api.example.com/a"); fetch("https://API.example.com/b")`, []string{"https://api.example.com"}},
		{"one per origin", `<script>fetch("https://a.example/1"); fetch("https://a.example/2"); fetch("https://b.example/3")</script>`, "", []string{"https://a.example", "https://b.example"}},
		{"protocol relative", `<script>d3.json("//cdn.example.com/a.json")</script>`, "", []string{"https://cdn.example.com"}},
		{"credentials dropped", `<script>fetch("https://alice:secret@API.example:8443/a"); fetch("https://bob@api.example:8443/b"); fetch("https://[2001:db8::1]:9000/c")</script>`, "", []string{"https://api.example:8443", "https://[2001:db8::1]:9000"}},
		{"import is code", `<script type="module">await import("https://cdn.example.com/x@1.0.0/x.js")</script>`, "", nil},
		{"relative", `<script>fetch("data/sales.csv"); d3.csv("./a.csv")</script>`, "", nil},
		{"dynamic", `<script>fetch(base + "/x"); fetch(` + "`${base}/y`" + `)</script>`, "", nil},
		{"json block", `<script type="application/json">{"u": "fetch(\"https://api.example.com\")"}</script>`, "", nil},
		{"src script", `<script src="app.js">fetch("https://api.example.com")</script>`, "", nil},
		{"template", `<template><script>fetch("https://api.example.com")</script></template>`, "", nil},
		{"link not data", `<a href="https://example.com/report.csv">download</a>`, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := dataWarnings(Lint(pageBundle(tt.body, File{Path: "app.js", Data: []byte(tt.js)})))
			if len(ws) != len(tt.origins) {
				t.Fatalf("got %+v, want origins %v", ws, tt.origins)
			}
			for i, w := range ws {
				if strings.Contains(w.Message, "secret") || strings.Contains(w.Message, "@") {
					t.Fatalf("warning repeats credentials: %+v", w)
				}
				if !strings.Contains(w.Message, tt.origins[i]+" ") || w.Fix == "" {
					t.Fatalf("warning %d: %+v, want origin %s", i, w, tt.origins[i])
				}
				want := "index.html"
				if tt.js != "" {
					want = "app.js"
				}
				if w.Path != want {
					t.Fatalf("path %q, want %q", w.Path, want)
				}
			}
		})
	}
}

func TestLintWebsiteSkipsExternalData(t *testing.T) {
	entry := strings.Replace(goodPage, "<body>", `<body><script>fetch("https://api.example.com/x")</script>`, 1)
	if ws := dataWarnings(Lint(with(File{Path: "index.html", Data: []byte(entry)}))); len(ws) != 0 {
		t.Fatalf("website: %+v", ws)
	}
}

// A script with very many calls is matched one call at a time: it yields one
// warning per origin, and a repeated origin allocates nothing beyond the
// small per-call match slice.
func TestLintPageManyDataLoads(t *testing.T) {
	const calls = 50000
	js := []byte(strings.Repeat(`fetch("https://a.example/x");`, calls) + `fetch("https://B.example/y")`)
	ws := dataWarnings(Lint(pageBundle("", File{Path: "app.js", Data: js})))
	if len(ws) != 2 || !strings.Contains(ws[0].Message, "https://a.example ") || !strings.Contains(ws[1].Message, "https://b.example ") {
		t.Fatalf("warnings: %+v", ws)
	}
	if raceEnabled {
		return // the race detector allocates on its own
	}
	l := &linter{}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	l.dataLoads("app.js", js, map[string]bool{})
	runtime.ReadMemStats(&after)
	if perCall := (after.TotalAlloc - before.TotalAlloc) / calls; perCall > 96 {
		t.Fatalf("data lint allocated %d bytes per call", perCall)
	}
	if len(l.out) != 2 {
		t.Fatalf("direct scan: %+v", l.out)
	}
}

// Distinct origins past maxSeen stop the scan of that file.
func TestLintPageOriginBound(t *testing.T) {
	var b strings.Builder
	for i := range maxSeen + 10 {
		fmt.Fprintf(&b, "fetch(\"https://h%d.example/\");", i)
	}
	ws := Lint(pageBundle("", File{Path: "app.js", Data: []byte(b.String())}))
	if n := len(dataWarnings(ws)); n == 0 || n > maxWarnings {
		t.Fatalf("kept %d data warnings", n)
	}
	last := ws[len(ws)-1]
	if !strings.Contains(last.Message, fmt.Sprintf("%d more warnings not shown", maxSeen-maxWarnings)) {
		t.Fatalf("summary: %+v", last)
	}
}
