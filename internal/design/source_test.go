package design

import (
	"strings"
	"testing"
)

func TestScanEditableElements(t *testing.T) {
	src := `<!doctype html><html><head><title>T</title><style>p{}</style></head><body>
<h1 class="x">Hello <b>world</b></h1>
<div><p>One</p><p>Two<br>lines</p><img src=a.png><ul><li>implied<li>close</ul></div>
<button>Buy &amp; go</button>
<p>bad <div>block</div></p>
<p><!-- c -->comment</p>
<svg><text>no</text></svg>
<p>  </p>
<a href="x.html">Link</a>
</body></html>`
	s, err := Scan([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range s.Elements {
		got = append(got, e.Tag+":"+s.InnerText(e))
	}
	want := []string{"h1:Hello world", "p:One", "p:Twolines", "button:Buy & go", "div:block", "a:Link"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("elements = %q, want %q", got, want)
	}
	out := string(s.Rewrite(`<style data-flats-host></style>`))
	if !strings.Contains(out, `<h1 class="x" data-flats-node="0">`) || !strings.Contains(out, `<button data-flats-node="3">`) {
		t.Fatalf("rewrite missing node ids:\n%s", out)
	}
	if !strings.Contains(out, `<style data-flats-host></style></head>`) {
		t.Fatalf("insert not before </head>:\n%s", out)
	}
}

func TestScanTweaksAndInsertWithoutHead(t *testing.T) {
	src := `<!DOCTYPE html><body><script type="application/json" data-flats-tweaks>{"accent":{"type":"color","label":"A","value":"#fff"}}</script><p>x</p></body>`
	s, err := Scan([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasTweaks || string(s.src[s.Tweaks[0]:s.Tweaks[1]]) != `{"accent":{"type":"color","label":"A","value":"#fff"}}` {
		t.Fatalf("tweaks = %v %q", s.HasTweaks, s.src[s.Tweaks[0]:s.Tweaks[1]])
	}
	out := string(s.Rewrite("<!--host-->"))
	if !strings.HasPrefix(out, "<!DOCTYPE html><!--host--><body>") {
		t.Fatalf("insert position wrong: %s", out)
	}
	if _, err := Scan([]byte(src + src)); err == nil {
		t.Fatal("two tweak blocks accepted")
	}
}

func TestSanitizeInline(t *testing.T) {
	got, err := SanitizeInline("p", `Hi <b onclick="x()">bold</b><script>bad()</script><div style="c">blk</div>
next <span class="k" data-x="1">s</span><br>`)
	if err != nil {
		t.Fatal(err)
	}
	want := "Hi <b>bold</b>bad()blk\nnext <span class=\"k\">s</span><br>"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}
