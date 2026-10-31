package notebook

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestMarkdownHTMLPasteSemantics(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		html string
		md   string
	}{
		{"headings", "<h1>One</h1><h2>Two</h2><h3>Three</h3>", "# One\n\n## Two\n\n### Three"},
		{"paragraphs and breaks", "<p>Hello\n  world<br>next</p><div>한국어 😀 &amp; text</div>", "Hello world  \nnext\n\n한국어 😀 & text"},
		{"marks", "<p><strong>bold</strong> <em>italic</em> <s>strike</s> <code>a * b</code></p>", "**bold** *italic* ~~strike~~ `a * b`"},
		{"combined marks", "<p><strong><em>both</em></strong> <strong><strong>once</strong></strong></p>", "***both*** **once**"},
		{"adjacent marks", "<p><b>a</b><strong>b</strong><b>c</b> <code>x</code><code>y</code></p>", "**abc** `xy`"},
		{"transparent wrappers", "<p><b>a</b><span><b>b</b></span> <code>x</code><span><code>y</code></span></p>", "**ab** `xy`"},
		{"mark whitespace", "<p>a<strong> b </strong>c</p>", "a **b** c"},
		{"links", `<p><a href="https://example.test/a_(b)?x=1&amp;y=2">web</a> <a href="mailto:person@example.test">mail</a></p>`, "[web](<https://example.test/a_(b)?x=1&y=2>) [mail](<mailto:person@example.test>)"},
		{"internal link", `<p><a href="#p_aaaaaaaaaaaaaaaaaaaaaaaaaa">page</a></p>`, "[page](#p_aaaaaaaaaaaaaaaaaaaaaaaaaa)"},
		{"entity in URL", `<p><a href="https://example.test/?x=&amp;lt;">web</a></p>`, "[web](<https://example.test/?x=&amp;lt;>)"},
		{"list nesting", "<ul><li>outer<ul><li>inner</li></ul></li><li>last</li></ul>", "- outer\n\n  - inner\n\n- last"},
		{"ordered list", "<ol><li>one</li><li>two</li></ol>", "1. one\n1. two"},
		{"tasks", `<ul><li><input type="checkbox" checked>done</li><li><input type="checkbox">next</li></ul>`, "- [x] done\n- [ ] next"},
		{"nested task", `<ul><li>outer<ul><li><input type="checkbox" checked>done</li></ul></li></ul>`, "- outer\n\n  - [x] done"},
		{"quote", "<blockquote><p>quoted <em>words</em></p></blockquote>", "> quoted *words*"},
		{"fenced code", "<pre><code class=\"language-go\">x := `text`\nfmt.Println(x)\n</code></pre>", "```go\nx := `text`\nfmt.Println(x)\n```"},
		{"code fence injection", "<pre><code>```\n# literal\n</code></pre>", "````\n```\n# literal\n````"},
		{"inline backticks", "<p><code>`x`</code></p>", "`` `x` ``"},
		{"divider", "<p>before</p><hr><p>after</p>", "before\n\n---\n\nafter"},
		{"table", "<table><thead><tr><th>Name</th><th>Value</th></tr></thead><tbody><tr><td>A | B</td><td>😀</td></tr></tbody></table>", "| Name | Value |\n| --- | --- |\n| A \\| B | 😀 |"},
		{"unknown wrappers", "<custom><span>safe</span></custom>", "safe"},
		{"malformed HTML", "<p>one<p>two", "one\n\ntwo"},
		{"empty", "<!-- clipboard metadata -->", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			place := Placement{Page: Target{Tmp: "paste-page"}}
			want, wantWarnings, err := ParseMarkdown(tc.md, place, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, warnings, err := ParseHTMLPaste(tc.html, place, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("HTML operations differ from Markdown\nHTML: %s\nMarkdown: %s\ngot: %#v\nwant: %#v", tc.html, tc.md, got, want)
			}
			if !slices.Equal(warnings, wantWarnings) {
				t.Errorf("warnings = %#v, want %#v", warnings, wantWarnings)
			}
		})
	}
}

func TestMarkdownHTMLPasteLineBreaks(t *testing.T) {
	t.Parallel()
	source := "<p>alpha\n beta<br>gamma</p>"
	ops, warnings, err := ParseHTMLPaste(source, Placement{Page: Target{Tmp: "page"}}, nil)
	if err != nil || len(warnings) != 0 || len(ops) != 1 {
		t.Fatalf("paste: ops=%v warnings=%v err=%v", ops, warnings, err)
	}
	if got := markdownPlain(ops[0].(CreateBlockOp).Content.Text); got != "alpha beta\ngamma" {
		t.Fatalf("paste line breaks = %q", got)
	}
	page, blocks, dbs := markdownBuild(t, ops)
	md, _, warnings := ExportMarkdown(page, blocks, dbs)
	if len(warnings) != 0 || markdownNormalized(t, md) != markdownNormalized(t, "alpha beta  \ngamma\n") {
		t.Fatalf("paste export changed line breaks: %q, warnings=%v", md, warnings)
	}
}

func TestMarkdownHTMLPasteLiteralText(t *testing.T) {
	t.Parallel()
	literal := "# heading [link](javascript:evil) ![image](remote) *bold* ~~strike~~ &lt;script&gt; <script> > quote | table |"
	src := "<p># heading [link](javascript:evil) ![image](remote) *bold* ~~strike~~ &amp;lt;script&amp;gt; &lt;script&gt; &gt; quote | table |</p>"
	ops, warnings, err := ParseHTMLPaste(src, Placement{Page: Target{Tmp: "page"}}, func(string) (string, bool) {
		t.Fatal("literal image text reached asset resolver")
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	blocks := markdownPasteBlocks(t, ops)
	if len(blocks) != 1 || blocks[0].Kind != Paragraph || markdownPasteText(blocks[0]) != literal {
		t.Fatalf("literal HTML text became markup: %#v", blocks)
	}
	for _, run := range blocks[0].Text {
		if len(run.Marks) != 0 || run.Link != nil {
			t.Fatalf("literal text gained formatting: %#v", run)
		}
	}
	if len(warnings) != 0 {
		t.Fatalf("literal text warnings = %#v", warnings)
	}
}

func TestMarkdownHTMLPasteHostileElements(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		html string
	}{
		{"script", `<p>before</p><script src="https://example.test/evil">SECRET</script><p>after</p>`},
		{"style", `<p>before</p><style>@import "https://example.test/SECRET";</style><p>after</p>`},
		{"iframe", `<p>before</p><iframe src="https://example.test/evil" srcdoc="SECRET">SECRET</iframe><p>after</p>`},
		{"object", `<p>before</p><object data="https://example.test/evil"><p>SECRET</p></object><p>after</p>`},
		{"template", `<p>before</p><template><img src="SECRET"></template><p>after</p>`},
		{"svg", `<p>before</p><svg><a href="javascript:evil"><text>SECRET</text></a></svg><p>after</p>`},
		{"math", `<p>before</p><math><mtext>SECRET</mtext></math><p>after</p>`},
		{"head URLs", `<html><head><base href="https://example.test/"><link rel="stylesheet" href="SECRET"><script>SECRET</script></head><body><p>before</p><p>after</p></body></html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ops, warnings, err := ParseHTMLPaste(tc.html, Placement{Page: Target{Tmp: "page"}}, func(string) (string, bool) {
				t.Fatal("hostile subtree reached asset resolver")
				return "", false
			})
			if err != nil {
				t.Fatal(err)
			}
			blocks := markdownPasteBlocks(t, ops)
			if len(blocks) != 2 || markdownPasteText(blocks[0]) != "before" || markdownPasteText(blocks[1]) != "after" {
				t.Fatalf("hostile subtree survived: %#v", blocks)
			}
			if len(warnings) == 0 || warnings[0].Code != "html_unsafe_element" {
				t.Fatalf("missing unsafe-element warning: %#v", warnings)
			}
		})
	}
}

func TestMarkdownHTMLPasteUnsafeLinks(t *testing.T) {
	t.Parallel()
	for _, href := range []string{"javascript:alert(1)", "jav&#x61;script:alert(1)", "java&#10;script:alert(1)", "data:text/html,evil", "vbscript:evil", "file:///etc/passwd", "//example.test/path", "/relative", "https://example.test/&#92;evil"} {
		t.Run(href, func(t *testing.T) {
			t.Parallel()
			ops, warnings, err := ParseHTMLPaste(`<p><a href="`+href+`" onclick="evil()"><strong>visible</strong></a></p>`, Placement{Page: Target{Tmp: "page"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			blocks := markdownPasteBlocks(t, ops)
			if len(blocks) != 1 || markdownPasteText(blocks[0]) != "visible" {
				t.Fatalf("link label lost: %#v", blocks)
			}
			for _, run := range blocks[0].Text {
				if run.Link != nil {
					t.Errorf("unsafe link survived: %#v", run.Link)
				}
			}
			if len(warnings) != 1 || warnings[0].Code != "html_unsafe_url" {
				t.Fatalf("warnings = %#v", warnings)
			}
		})
	}
}

func TestMarkdownHTMLPasteAssets(t *testing.T) {
	t.Parallel()
	hash := strings.Repeat("a", 64)
	var paths []string
	resolve := func(path string) (string, bool) {
		paths = append(paths, path)
		return hash, path == "assets/paste.png"
	}
	ops, warnings, err := ParseHTMLPaste(`<p>before<img src="assets/paste.png" alt="A *literal* [alt]" onerror="evil()" srcset="https://example.test/evil 2x">after</p>`, Placement{Page: Target{Tmp: "page"}}, resolve)
	if err != nil {
		t.Fatal(err)
	}
	blocks := markdownPasteBlocks(t, ops)
	if len(blocks) != 3 || markdownPasteText(blocks[0]) != "before" || blocks[1].Kind != Image || blocks[1].Asset != hash || blocks[1].Alt != "A *literal* [alt]" || markdownPasteText(blocks[2]) != "after" {
		t.Fatalf("image sequence = %#v", blocks)
	}
	if !reflect.DeepEqual(paths, []string{"assets/paste.png"}) || len(warnings) != 0 {
		t.Fatalf("resolver paths = %#v, warnings = %#v", paths, warnings)
	}
	for _, path := range []string{"missing.png", "https://example.test/image.png", "data:image/png;base64,AAAA", "javascript:evil"} {
		t.Run(path, func(t *testing.T) {
			ops, _, err := ParseHTMLPaste(`<p>must not partially import</p><img src="`+path+`">`, Placement{Page: Target{Tmp: "page"}}, resolve)
			if err == nil || len(ops) != 0 {
				t.Fatalf("invalid image path accepted: operations = %#v, err = %v", ops, err)
			}
		})
	}
}

func TestMarkdownHTMLPasteDeterministic(t *testing.T) {
	t.Parallel()
	src := `<h5>Title</h5><p><a href="javascript:evil">label</a></p><script>evil()</script><table><tr><th>Name</th></tr><tr><td>row</td></tr></table>`
	place := Placement{Page: Target{Tmp: "page"}, Parent: &Target{Tmp: "parent"}, Before: &Target{Tmp: "anchor"}}
	ops, warnings, err := ParseHTMLPaste(src, place, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		again, againWarnings, err := ParseHTMLPaste(src, place, nil)
		if err != nil || !reflect.DeepEqual(ops, again) || !reflect.DeepEqual(warnings, againWarnings) {
			t.Fatalf("nondeterministic parse: operations = %#v, warnings = %#v, err = %v", again, againWarnings, err)
		}
	}
}

func markdownPasteBlocks(t *testing.T, ops []Operation) []Content {
	t.Helper()
	var blocks []Content
	for _, op := range ops {
		switch op := op.(type) {
		case CreateBlockOp:
			blocks = append(blocks, op.Content)
		case *CreateBlockOp:
			blocks = append(blocks, op.Content)
		default:
			t.Fatalf("unexpected operation %T", op)
		}
	}
	return blocks
}

func markdownPasteText(c Content) string {
	var out strings.Builder
	for _, run := range c.Text {
		out.WriteString(run.Text)
	}
	return out.String()
}
