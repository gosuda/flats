package notebook

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func vocabFixtures(t testing.TB, name string) []json.RawMessage {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []json.RawMessage
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	return fixtures
}
func TestContentRoundTrip(t *testing.T) {
	t.Parallel()
	fixtures := vocabFixtures(t, "content")
	expected := vocabFixtures(t, "content.canonical")
	if len(fixtures) != len(expected) {
		t.Fatalf("%d content fixtures, %d expected outputs", len(fixtures), len(expected))
	}
	seen := map[ContentKind]bool{}
	for n, raw := range fixtures {
		c, err := DecodeContent(raw)
		if err != nil {
			t.Fatal(err)
		}
		seen[c.Kind] = true
		t.Run(string(c.Kind), func(t *testing.T) {
			data, err := CanonicalJSON(c)
			if err != nil {
				t.Fatal(err)
			}
			if want := bytes.TrimSpace(expected[n]); !bytes.Equal(data, want) {
				t.Fatalf("canonical content = %s, want %s", data, want)
			}
			decoded, err := DecodeContent(data)
			if err != nil {
				t.Fatal(err)
			}
			again, err := CanonicalJSON(decoded)
			if err != nil || !bytes.Equal(data, again) {
				t.Fatalf("round trip: %s -> %s, %v", data, again, err)
			}
		})
	}
	if len(seen) != 15 {
		t.Fatalf("covered %d kinds", len(seen))
	}
}
func TestContentRejectsKindSpecificFields(t *testing.T) {
	t.Parallel()
	fields := map[string]any{"text": []any{}, "checked": false, "open_default": false, "tone": "neutral", "language": "", "asset": strings.Repeat("a", 64), "alt": "", "caption": []any{}, "target": string(vocabID(PageKind, 0)), "database": string(vocabID(DatabaseKind, 0)), "extra": true}
	for _, raw := range vocabFixtures(t, "content") {
		content, err := DecodeContent(raw)
		if err != nil {
			t.Fatal(err)
		}
		legal := map[string]bool{"kind": true}
		if content.Kind.IsText() {
			legal["text"] = true
		}
		switch content.Kind {
		case Todo:
			legal["checked"] = true
		case Toggle:
			legal["open_default"] = true
		case Callout:
			legal["tone"] = true
		case Code:
			legal["text"] = true
			legal["language"] = true
		case Image:
			legal["asset"] = true
			legal["alt"] = true
			legal["caption"] = true
		case PageLink:
			legal["target"] = true
		case InlineDatabase:
			legal["database"] = true
		}
		for field, value := range fields {
			if legal[field] {
				continue
			}
			t.Run(string(content.Kind)+"/"+field, func(t *testing.T) {
				var obj map[string]any
				if err := json.Unmarshal(raw, &obj); err != nil {
					t.Fatal(err)
				}
				obj[field] = value
				data, _ := json.Marshal(obj)
				if _, err := DecodeContent(data); err == nil {
					t.Fatalf("accepted %s", data)
				}
			})
		}
	}
}
func TestContentRejectsMalformed(t *testing.T) {
	t.Parallel()
	inputs := []string{`null`, `{}`, `{"kind":"bogus"}`, `{"kind":"paragraph","text":null}`, `{"kind":"paragraph","text":[{"text":"a"}]}`, `{"kind":"paragraph","text":[{"text":"a","marks":null}]}`, `{"kind":"paragraph","text":[{"text":"a","marks":["underline"]}]}`, `{"kind":"paragraph","text":[{"text":"a","marks":[],"html":"x"}]}`, `{"kind":"paragraph","text":[{"text":"a","marks":[],"link":null}]}`, `{"kind":"paragraph","text":[{"text":"a","marks":[],"link":{"url":"https://example.com","page":"` + string(vocabID(PageKind, 0)) + `"}}]}`, `{"kind":"code","text":[]}`, `{"kind":"callout","text":[],"tone":"loud"}`, `{"kind":"todo","text":[],"checked":null}`, `{"kind":"image","asset":"` + strings.Repeat("A", 64) + `","alt":"","caption":[]}`, `{"kind":"page_link","target":"` + string(vocabID(BlockKind, 0)) + `"}`, `{"kind":"database","database":"` + string(vocabID(PageKind, 0)) + `"}`}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			if _, err := DecodeContent([]byte(input)); err == nil {
				t.Fatalf("accepted %s", input)
			}
		})
	}
}
func TestRichTextCanonicalization(t *testing.T) {
	t.Parallel()
	source := RichText{{Text: "", Marks: []Mark{Bold}}, {Text: "e\u0301", Marks: []Mark{Italic, Bold, Bold}}, {Text: "😀\n한글", Marks: []Mark{Bold, Italic}}, {Text: "different", Marks: []Mark{}, Link: &Link{Page: vocabID(PageKind, 0)}}}
	canonical, err := CanonicalizeRichText(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical) != 2 || canonical[0].Text != "e\u0301😀\n한글" || len(canonical[0].Marks) != 2 || canonical[0].Marks[0] != Bold || canonical[0].Marks[1] != Italic {
		t.Fatalf("canonical text = %#v", canonical)
	}
	first, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		again, err := CanonicalizeRichText(canonical)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(again)
		if err != nil || !bytes.Equal(data, first) {
			t.Fatalf("canonicalization not idempotent: %s != %s, %v", first, data, err)
		}
		canonical = again
	}
	canonical[1].Link.Page = vocabID(PageKind, 2)
	if source[3].Link.Page != vocabID(PageKind, 0) {
		t.Fatal("canonicalizer aliased input link")
	}
}
func TestContentRetainsOverLimitText(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		content Content
	}{
		{"rich text at limit", Content{Kind: Paragraph, Text: RichText{{Text: strings.Repeat("😀", MaxRichTextCodePoints), Marks: []Mark{}}}}},
		{"retained rich text", Content{Kind: Paragraph, Text: RichText{{Text: strings.Repeat("😀", MaxRichTextCodePoints+1), Marks: []Mark{}}}}},
		{"retained code", Content{Kind: Code, Code: strings.Repeat("x", MaxCodeCodePoints+1)}},
		{"retained language", Content{Kind: Code, Language: strings.Repeat("x", MaxNameCodePoints+1)}},
		{"retained alt", Content{Kind: Image, Asset: strings.Repeat("a", 64), Alt: strings.Repeat("x", MaxAltCodePoints+1)}},
		{"retained caption", Content{Kind: Image, Asset: strings.Repeat("a", 64), Caption: RichText{{Text: strings.Repeat("x", MaxRichTextCodePoints+1), Marks: []Mark{}}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.content.Validate(); err != nil {
				t.Fatal(err)
			}
			data, err := CanonicalJSON(tt.content)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeContent(data)
			if err != nil {
				t.Fatal(err)
			}
			again, err := CanonicalJSON(decoded)
			if err != nil || !bytes.Equal(data, again) {
				t.Fatalf("retained content changed: %v", err)
			}
		})
	}
}
func TestLinkURLs(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"https://example.com/a?b=c#d", "http://localhost:8080/a", "mailto:person@example.com?subject=Hello", "mailto:?subject=Hello", "https://[::1]:8080", "https://例え.example/한글", "https://example.com/" + strings.Repeat("a", MaxURLBytes+1)} {
		t.Run(s, func(t *testing.T) {
			if err := (Link{URL: s}).Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, s := range []string{"javascript:alert(1)", "data:text/html,a", "ftp://example.com", "/relative", "//example.com", "https:", "https:///path", "https://", "https://exa mple.com", "https://example.com/\n", "https://example.com\\a", "https://example.com/%zz", "https://example.com:65536", "https://example.com:", "https://[:::]", strings.Repeat("a", MaxURLBytes+1)} {
		t.Run(s, func(t *testing.T) {
			if err := (Link{URL: s}).Validate(); err == nil {
				t.Fatalf("accepted %q", s)
			}
		})
	}
}
func FuzzContent(f *testing.F) {
	for _, raw := range vocabFixtures(f, "content") {
		f.Add([]byte(raw))
	}
	f.Add([]byte(`{"kind":"paragraph","text":[]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := DecodeContent(data)
		if err != nil {
			return
		}
		encoded, err := CanonicalJSON(v)
		if err != nil {
			t.Fatal(err)
		}
		again, err := DecodeContent(encoded)
		if err != nil {
			t.Fatal(err)
		}
		second, err := CanonicalJSON(again)
		if err != nil || !bytes.Equal(encoded, second) {
			t.Fatalf("unstable content: %s -> %s (%v)", encoded, second, err)
		}
	})
}
