package bundle

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// goodPage passes every document check.
const goodPage = `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Team Lunch Poll</title><link rel="icon" href="/favicon.svg">
<link rel="stylesheet" href="app.css"></head>
<body><img src="img/a.png" srcset="img/a.png 1x, img/a@2x.png 2x"><script src="/app.js"></script></body></html>`

func cleanBundle() []File {
	return []File{
		{Path: "index.html", Data: []byte(goodPage)},
		{Path: "flats.json", Data: []byte(`{"screenshot":"screenshot.png"}`)},
		{Path: "screenshot.png", Data: []byte("png")},
		{Path: "favicon.svg", Data: []byte("<svg/>")},
		{Path: "app.css", Data: []byte("body{}")},
		{Path: "app.js", Data: []byte("")},
		{Path: "img/a.png", Data: []byte("png")},
		{Path: "img/a@2x.png", Data: []byte("png")},
	}
}

// with returns the clean bundle with files replaced or added.
func with(files ...File) []File {
	out := cleanBundle()
	for _, f := range files {
		replaced := false
		for i := range out {
			if out[i].Path == f.Path {
				out[i], replaced = f, true
			}
		}
		if !replaced {
			out = append(out, f)
		}
	}
	return out
}

func without(files []File, p string) []File {
	var out []File
	for _, f := range files {
		if f.Path != p {
			out = append(out, f)
		}
	}
	return out
}

func page(head, body string) []byte {
	return []byte(`<!doctype html><html><head><meta name="viewport" content="width=device-width"><title>Lunch Poll</title><link rel="icon" href="favicon.svg">` +
		head + `</head><body>` + body + `</body></html>`)
}

func render(ws []Problem) string {
	var b strings.Builder
	for _, w := range ws {
		fmt.Fprintf(&b, "%s: %s (fix: %s)\n", w.Path, w.Message, w.Fix)
	}
	return b.String()
}

func expectWarning(t *testing.T, files []File, path, substr string) {
	t.Helper()
	ws := Lint(files)
	for _, w := range ws {
		if w.Path == path && strings.Contains(w.Message, substr) && w.Fix != "" {
			return
		}
	}
	t.Fatalf("want a warning on %s containing %q, got:\n%s", path, substr, render(ws))
}

func expectClean(t *testing.T, files []File) {
	t.Helper()
	if ws := Lint(files); len(ws) != 0 {
		t.Fatalf("want no warnings, got:\n%s", render(ws))
	}
}

func TestLintCleanBundle(t *testing.T) {
	expectClean(t, cleanBundle())
}

func TestLintTitle(t *testing.T) {
	noTitle := []byte(`<!doctype html><html><head><meta name="viewport" content="width=device-width"><link rel="icon" href="favicon.svg"></head><body></body></html>`)
	expectWarning(t, with(File{Path: "index.html", Data: noTitle}), "index.html", "no <title>")
	empty := bytes.Replace(page("", ""), []byte("<title>Lunch Poll</title>"), []byte("<title>  </title>"), 1)
	expectWarning(t, with(File{Path: "index.html", Data: empty}), "index.html", "no <title>")
	placeholder := bytes.Replace(page("", ""), []byte("Lunch Poll"), []byte("Vite + React"), 1)
	expectWarning(t, with(File{Path: "index.html", Data: placeholder}), "index.html", "placeholder")
	// A title inside a script is not the document title.
	scripted := []byte(`<!doctype html><html><head><meta name="viewport" content="width=device-width"><link rel="icon" href="favicon.svg"><script>const s = "<title>x</title>";</script></head></html>`)
	expectWarning(t, with(File{Path: "index.html", Data: scripted}), "index.html", "no <title>")
	expectClean(t, with(File{Path: "index.html", Data: page("", "")}))
}

func TestLintViewport(t *testing.T) {
	noViewport := []byte(`<!doctype html><title>Lunch Poll</title><link rel="icon" href="favicon.svg">`)
	expectWarning(t, with(File{Path: "index.html", Data: noViewport}), "index.html", "viewport")
	upper := []byte(`<!DOCTYPE html><TITLE>Lunch Poll</TITLE><META NAME="Viewport" CONTENT="width=device-width"><LINK REL="icon" HREF="favicon.svg">`)
	expectClean(t, with(File{Path: "index.html", Data: upper}))
}

func TestLintOtherDocumentsAndFragments(t *testing.T) {
	bare := []byte(`<!doctype html><html><head></head><body>about</body></html>`)
	files := with(File{Path: "about/index.html", Data: bare})
	expectWarning(t, files, "about/index.html", "no <title>")
	expectWarning(t, files, "about/index.html", "viewport")
	// Only the entry needs a favicon link.
	for _, w := range Lint(files) {
		if w.Path == "about/index.html" && strings.Contains(w.Message, "favicon") {
			t.Fatalf("favicon warning on a non-entry page: %v", w)
		}
	}
	// A fragment (template, partial) is not a page.
	expectClean(t, with(File{Path: "partials/card.html", Data: []byte(`<div class="card"><h2>Card</h2></div>`)}))
}

func TestLintExternalAssets(t *testing.T) {
	pinned := []string{
		`<script src="https://cdnjs.cloudflare.com/ajax/libs/react/18.3.1/umd/react.production.min.js"></script>`,
		`<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.4/dist/chart.umd.min.js"></script>`,
		`<script src="https://cdn.jsdelivr.net/npm/@observablehq/plot@0.6.16/dist/plot.umd.min.js"></script>`,
		`<script src="https://unpkg.com/react@18.3.1/umd/react.production.min.js"></script>`,
		`<script src="https://code.jquery.com/jquery-3.7.1.min.js"></script>`,
		`<script src="//cdn.jsdelivr.net/gh/user/repo@v1.2.3/dist/x.js"></script>`,
		`<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/water.css@2.1.1/out/water.min.css">`,
		`<script src="https://esm.sh/preact@10.24.3/es2022/preact.mjs" type="module"></script>`,
		`<script type="importmap">{"imports":{"vue":"https://unpkg.com/vue@3.5.12/dist/vue.esm-browser.prod.js"}}</script>`,
		`<link rel="preload" as="image" href="https://images.example.com/hero.jpg">`, // not code
		`<img src="https://images.example.com/hero.jpg">`,                            // not code
	}
	for _, h := range pinned {
		expectClean(t, with(File{Path: "index.html", Data: page(h, "")}))
	}
	unpinned := map[string]string{
		`<script src="https://unpkg.com/react@latest/umd/react.production.min.js"></script>`:        `"latest"`,
		`<script src="https://cdn.jsdelivr.net/npm/react@18/umd/react.production.min.js"></script>`: `"18"`,
		`<script src="https://cdn.jsdelivr.net/npm/react@18.3/umd/react.js"></script>`:              `"18.3"`,
		`<script src="https://cdn.jsdelivr.net/npm/react@^18.3.1/umd/react.js"></script>`:           `"^18.3.1"`,
		`<script src="https://unpkg.com/htmx.org"></script>`:                                        "has no version",
		`<script src="https://cdn.tailwindcss.com"></script>`:                                       "has no version",
		`<link rel="stylesheet" href="https://cdn.example.com/theme.css">`:                          "has no version",
		`<link rel="modulepreload" href="https://esm.sh/preact">`:                                   "has no version",
		`<script type="importmap">{"imports":{"vue":"https://esm.sh/vue"}}</script>`:                "has no version",
	}
	for h, want := range unpinned {
		expectWarning(t, with(File{Path: "index.html", Data: page(h, "")}), "index.html", want)
	}
	fonts := `<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Inter:wght@400;600&display=swap">`
	expectWarning(t, with(File{Path: "index.html", Data: page(fonts, "")}), "index.html", "cannot be pinned")
}

func TestLintMissingReferences(t *testing.T) {
	expectWarning(t, with(File{Path: "index.html", Data: page(`<script src="/assets/main.js"></script>`, "")}), "index.html", "no file assets/main.js")
	expectWarning(t, with(File{Path: "index.html", Data: page(`<link rel="stylesheet" href="styles.css">`, "")}), "index.html", "no file styles.css")
	expectWarning(t, with(File{Path: "index.html", Data: page("", `<img srcset="img/a.png 1x, img/b.png 2x">`)}), "index.html", "no file img/b.png")
	expectWarning(t, with(File{Path: "index.html", Data: page("", `<video src="clip.mp4" poster="poster.jpg"></video>`)}), "index.html", "no file poster.jpg")
	// Relative to the page's directory.
	sub := File{Path: "blog/post.html", Data: page(`<link rel="stylesheet" href="../app.css"><link rel="stylesheet" href="post.css">`, "")}
	expectWarning(t, with(sub), "blog/post.html", "no file blog/post.css")
	ok := []string{
		`<script src="app.js?v=3"></script>`,
		`<script src="./app.js#x"></script>`,
		`<img src="img/a%2Fb.png" hidden>`, // decoded below
		`<img src="data:image/png;base64,iVBORw0KGgo=">`,
		`<img src="#">`,
		`<iframe src="about/"></iframe>`,
		`<iframe src="about"></iframe>`,
		`<iframe src="/"></iframe>`,
		`<a href="missing.html">links are navigation, not assets</a>`,
	}
	for _, b := range ok {
		files := with(File{Path: "index.html", Data: page("", b)}, File{Path: "about/index.html", Data: []byte("<p>x</p>")}, File{Path: "img/a/b.png", Data: []byte("png")})
		expectClean(t, files)
	}
	// flats.json is never served.
	expectWarning(t, with(File{Path: "index.html", Data: page(`<link rel="manifest" href="flats.json">`, "")}), "index.html", "no file flats.json")
	// A <base> changes resolution; references are not checked.
	expectClean(t, with(File{Path: "index.html", Data: page(`<base href="/v2/"><script src="main.js"></script>`, "")}))
}

func TestLintMissingReferencesSPAAndServer(t *testing.T) {
	spa := with(File{Path: "flats.json", Data: []byte(`{"spa":true,"screenshot":"screenshot.png"}`)},
		File{Path: "index.html", Data: page("", `<iframe src="/settings"></iframe><script src="/assets/gone.js"></script>`)})
	ws := Lint(spa)
	if len(ws) != 1 || !strings.Contains(ws[0].Message, "assets/gone.js") {
		t.Fatalf("SPA serves its entry for extensionless paths, files are still required:\n%s", render(ws))
	}
	server := []File{
		{Path: "flats.json", Data: []byte(`{"kind":"server"}`)},
		{Path: "server.js", Data: []byte("export default {}")},
		{Path: "index.html", Data: page("", `<script src="/api/config.js"></script>`)},
	}
	// Server handlers own their routes: no reference, favicon or screenshot checks.
	expectClean(t, server)
	server[2].Data = []byte(`<!doctype html><html><head></head></html>`)
	expectWarning(t, server, "index.html", "no <title>")
}

func TestLintEntryInSubdirectory(t *testing.T) {
	files := []File{
		{Path: "flats.json", Data: []byte(`{"entry":"public/index.html","screenshot":"shot.png"}`)},
		{Path: "shot.png", Data: []byte("png")},
		{Path: "public/index.html", Data: page(`<script src="main.js"></script><script src="public/main.js"></script>`, "")},
		{Path: "public/main.js", Data: []byte("")},
		{Path: "favicon.svg", Data: []byte("")},
	}
	// public/main.js resolves from the page's directory, main.js... also from
	// the root where the entry is served; both forms point at existing files.
	ws := Lint(files)
	for _, w := range ws {
		if strings.Contains(w.Message, "main.js") {
			t.Fatalf("entry served at the root and at its own path: %s", render(ws))
		}
	}
}

func TestLintLargeImages(t *testing.T) {
	big := bytes.Repeat([]byte{0}, MaxImageBytes+1)
	expectWarning(t, with(File{Path: "img/hero.jpg", Data: big}), "img/hero.jpg", "MiB")
	expectClean(t, with(File{Path: "img/hero.jpg", Data: big[:MaxImageBytes]}))
	// Other large files are not images.
	expectClean(t, with(File{Path: "data/big.json", Data: big}))
}

func TestLintScreenshotAndFavicon(t *testing.T) {
	expectWarning(t, with(File{Path: "flats.json", Data: []byte(`{}`)}), ManifestName, "screenshot")
	expectWarning(t, without(cleanBundle(), "flats.json"), ManifestName, "screenshot")
	noIcon := bytes.Replace([]byte(goodPage), []byte(`<link rel="icon" href="/favicon.svg">`), nil, 1)
	expectWarning(t, with(File{Path: "index.html", Data: noIcon}), "index.html", "favicon")
	expectClean(t, with(File{Path: "index.html", Data: noIcon}, File{Path: "favicon.ico", Data: []byte("ico")}))
	touch := bytes.Replace([]byte(goodPage), []byte(`rel="icon" href="/favicon.svg"`), []byte(`rel="apple-touch-icon" href="favicon.svg"`), 1)
	expectClean(t, with(File{Path: "index.html", Data: touch}))
}

func TestLintSkipsDocsAndInvalid(t *testing.T) {
	docs := []File{{Path: "index.md", Data: []byte("# Notes")}, {Path: "page.html", Data: []byte("<!doctype html><html></html>")}}
	if ws := Lint(docs); ws != nil {
		t.Fatalf("docs bundles are not linted: %s", render(ws))
	}
	// A spa without its entry is already a validation error, not a warning.
	spa := []File{{Path: "flats.json", Data: []byte(`{"spa":true}`)}, {Path: "app.js", Data: []byte("")}}
	if _, err := ParseManifest(spa); err == nil {
		t.Fatal("spa without entry must fail validation")
	}
	if ws := Lint(spa); ws != nil {
		t.Fatalf("invalid bundles are not linted: %s", render(ws))
	}
}

func TestLintCapsWarnings(t *testing.T) {
	var body strings.Builder
	for i := range 80 {
		fmt.Fprintf(&body, `<img src="missing-%d.png">`, i)
	}
	ws := Lint(with(File{Path: "index.html", Data: page("", body.String())}))
	if len(ws) != maxWarnings+1 || !strings.Contains(ws[maxWarnings].Message, "30 more warnings") {
		t.Fatalf("cap: %d warnings, last %+v", len(ws), ws[len(ws)-1])
	}
}

func TestExactVersion(t *testing.T) {
	for p, want := range map[string]bool{
		"/npm/react@18.3.1/umd/react.js":      true,
		"/npm/react@18.3.1-rc.0/umd/react.js": true,
		"/npm/@scope/pkg@1.2.3/x.js":          true,
		"/npm/@scope/pkg/x.js":                false,
		"/npm/@scope/pkg@next/x.js":           false,
		"/npm/react@~18.3.1/x.js":             false,
		"/ajax/libs/vue/3.5.12/vue.min.js":    true,
		"/v3.5.12/vue.js":                     true,
		"/jquery-3.7.1.min.js":                true,
		"/react.production.min.js":            false,
		"/app.3f9a1c2b.js":                    false,
		"/react%4018.3.1/x.js":                true,
	} {
		if got, _ := exactVersion(p); got != want {
			t.Errorf("exactVersion(%q) = %v, want %v", p, got, want)
		}
	}
}
