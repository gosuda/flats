package notebook

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestMarkdownStreamingExporterParity(t *testing.T) {
	f := liveReadFixture(t)
	rich := RichText{
		{Text: strings.Repeat("start |\n한😀", 500), Marks: []Mark{Bold}, Link: &Link{URL: "https://example.test/a?b=1&c=2"}},
		{Text: " middle ", Marks: []Mark{Bold, Italic}, Link: &Link{URL: "https://example.test/a?b=1&c=2"}},
		{Text: "`code\n`", Marks: []Mark{InlineCode}},
		{Text: "  ", Marks: []Mark{InlineCode}},
		{Text: "after", Marks: []Mark{Strike}, Link: &Link{Page: f.page.ID}},
	}
	contents := []Content{
		{Kind: Bullet, Text: RichText{{Text: "parent", Marks: []Mark{}}}},
		{Kind: Paragraph, Text: rich},
		{Kind: Heading1, Text: rich}, {Kind: Heading2, Text: rich}, {Kind: Heading3, Text: rich},
		{Kind: Quote, Text: rich}, {Kind: Callout, Tone: WarningTone, Text: rich},
		{Kind: Numbered, Text: rich}, {Kind: Todo, Checked: true, Text: rich},
		{Kind: Todo, Checked: false, Text: rich}, {Kind: Toggle, Text: rich},
		{Kind: Code, Language: "go", Code: ""},
		{Kind: Code, Language: "go", Code: "```\n한😀\x00\\\"\n"},
		{Kind: Code, Code: "last line"}, {Kind: Divider},
		{Kind: Image, Asset: strings.Repeat("a", 64), Alt: "[<&>한😀]", Caption: rich},
		{Kind: PageLink, Target: f.page.ID},
	}
	var page Item
	blocks := make([]Item, 0, len(contents))
	parent := spillID(BlockKind, 900)
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		for i, content := range contents {
			block := Block{Page: f.page.ID, Position: fmt.Sprintf("A%03d", i), Content: content}
			if i > 0 {
				block.Parent = &parent
			}
			item := spillItem(spillID(BlockKind, uint64(900+i)), block)
			if err := putItem(t.Context(), tx, item); err != nil {
				return err
			}
			stored, err := readSelected(t.Context(), tx, item.ID(), nil)
			if err != nil {
				return err
			}
			blocks = append(blocks, stored)
		}
		var err error
		page, err = readSelected(t.Context(), tx, f.page.ID, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want, _, wantWarnings := ExportMarkdown(page, blocks, nil)
	for _, limit := range []int{1, 2000} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			body, _ := liveReadQuery(t, f.in, map[string]any{"page": f.page.ID, "format": "markdown", "limit": limit}, false)
			got, refs, warnings := continueMarkdownRead(t, f.in, body)
			if strings.TrimSpace(got) != strings.TrimSpace(want) {
				t.Fatalf("streamed Markdown differs:\ngot:\n%s\nwant:\n%s", got, want)
			}
			if !reflect.DeepEqual(warnings, wantWarnings) {
				t.Fatalf("warnings=%+v, want %+v", warnings, wantWarnings)
			}
			if len(refs) != len(blocks)+1 {
				t.Fatalf("references=%d", len(refs))
			}
		})
	}
}
