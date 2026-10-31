package notebook

import (
	"bytes"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
	"golang.org/x/net/html"
)

func TestMarkdownLosslessCorpus(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("testdata/markdown/lossless/*.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 13 {
		t.Fatalf("lossless corpus has %d documents", len(files))
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			t.Parallel()
			source, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Count(source, []byte("\n")) > 300 {
				t.Fatal("fixture exceeds 300 lines")
			}
			md, warnings := markdownRoundTrip(t, string(source))
			if len(warnings) != 0 {
				t.Fatalf("lossless warnings: %#v", warnings)
			}
			want, got := markdownNormalized(t, string(source)), markdownNormalized(t, md)
			if want != got {
				t.Errorf("normalized structure changed\nwant: %s\ngot:  %s\nMarkdown:\n%s", want, got, md)
			}
			again, warnings := markdownRoundTrip(t, md)
			if len(warnings) != 0 || markdownStripMarkers(md) != markdownStripMarkers(again) {
				t.Errorf("not a fixed point\n%s\n%s\n%#v", md, again, warnings)
			}
		})
	}
}

func TestMarkdownLineBreaks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		source string
		want   string
	}{
		{name: "soft", source: "alpha\nbeta\n", want: "alpha beta"},
		{name: "spaces hard", source: "alpha  \nbeta\n", want: "alpha\nbeta"},
		{name: "backslash hard", source: "alpha\\\nbeta\n", want: "alpha\nbeta"},
		{name: "mixed", source: "alpha\nbeta  \ngamma\\\ndelta\nepsilon\n", want: "alpha beta\ngamma\ndelta epsilon"},
		{name: "marked", source: "**alpha\nbeta  \ngamma**\n", want: "alpha beta\ngamma"},
		{name: "linked", source: "[alpha\nbeta  \ngamma](https://example.test)\n", want: "alpha beta\ngamma"},
		{name: "quote", source: "> alpha\n> beta  \n> gamma\n", want: "alpha beta\ngamma"},
		{name: "bullet", source: "- alpha\n  beta  \n  gamma\n", want: "alpha beta\ngamma"},
		{name: "numbered", source: "1. alpha\n   beta  \n   gamma\n", want: "alpha beta\ngamma"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, warnings, err := ParseMarkdown(tc.source, Placement{Page: Target{Tmp: "page"}}, nil)
			if err != nil || len(warnings) != 0 || len(ops) != 1 {
				t.Fatalf("parse: ops=%v warnings=%v err=%v", ops, warnings, err)
			}
			if got := markdownPlain(ops[0].(CreateBlockOp).Content.Text); got != tc.want {
				t.Errorf("rich text = %q, want %q", got, tc.want)
			}
			md, warnings := markdownRoundTrip(t, tc.source)
			if len(warnings) != 0 || markdownNormalized(t, md) != markdownNormalized(t, tc.source) {
				t.Errorf("rendered line breaks changed: %q, warnings=%v", md, warnings)
			}
		})
	}
}

func TestMarkdownAlertLineBreaks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		source string
		kind   ContentKind
		want   string
	}{
		{name: "soft header", source: "> [!NOTE]\n> alpha\n> beta  \n> gamma\n", kind: Callout, want: "alpha beta\ngamma"},
		{name: "hard header", source: "> [!NOTE]  \n> alpha\n> beta\\\n> gamma\n", kind: Callout, want: "alpha beta\ngamma"},
		{name: "same line is prose", source: "> [!NOTE] alpha\n> beta\n", kind: Quote, want: "[!NOTE] alpha beta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, warnings, err := ParseMarkdown(tc.source, Placement{Page: Target{Tmp: "page"}}, nil)
			if err != nil || len(warnings) != 0 || len(ops) != 1 {
				t.Fatalf("parse: ops=%v warnings=%v err=%v", ops, warnings, err)
			}
			content := ops[0].(CreateBlockOp).Content
			if content.Kind != tc.kind || markdownPlain(content.Text) != tc.want || tc.kind == Callout && content.Tone != InfoTone {
				t.Fatalf("alert content = %#v, want %s %q", content, tc.kind, tc.want)
			}
			page, blocks, dbs := markdownBuild(t, ops)
			md, _, warnings := ExportMarkdown(page, blocks, dbs)
			again, nextWarnings, err := ParseMarkdown(md, Placement{Page: Target{Tmp: "page"}}, nil)
			if err != nil || len(warnings) != 0 || len(nextWarnings) != 0 || len(again) != 1 {
				t.Fatalf("reimport: ops=%v warnings=%v/%v err=%v", again, warnings, nextWarnings, err)
			}
			if got := again[0].(CreateBlockOp).Content; !reflect.DeepEqual(content, got) {
				t.Fatalf("alert changed after export: want=%#v got=%#v", content, got)
			}
		})
	}
}

func TestMarkdownLosslessOracleMutations(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("testdata/markdown/lossless/paragraphs.md")
	if err != nil {
		t.Fatal(err)
	}
	original := string(source)
	cases := []struct {
		name    string
		source  string
		changed string
	}{
		{name: "added hard break", source: original, changed: strings.Replace(original, "soft\nbreak", "soft  \nbreak", 1)},
		{name: "removed hard break", source: original, changed: strings.Replace(original, "break\\\n", "break\n", 1)},
		{name: "inline code spaces", source: "`alpha  beta`\n", changed: "`alpha beta`\n"},
		{name: "fenced code indentation", source: "```\n  alpha\n```\n", changed: "```\n alpha\n```\n"},
		{name: "fenced code trailing spaces", source: "```\nalpha  \n```\n", changed: "```\nalpha \n```\n"},
		{name: "fenced code blank line", source: "```\nalpha\n\nbeta\n```\n", changed: "```\nalpha\nbeta\n```\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.source == tc.changed {
				t.Fatal("mutation did not change the source")
			}
			if markdownNormalized(t, tc.source) == markdownNormalized(t, tc.changed) {
				t.Fatalf("lossless oracle accepted mutation:\n%s", tc.changed)
			}
		})
	}
	if markdownNormalized(t, "alpha\nbeta\n") != markdownNormalized(t, "alpha beta\n") {
		t.Fatal("soft break no longer normalizes to ordinary prose whitespace")
	}
}

func TestMarkdownLossyCorpus(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("testdata/markdown/lossy/*.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, ".want.md") {
			continue
		}
		t.Run(filepath.Base(file), func(t *testing.T) {
			t.Parallel()
			source, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			md, warnings := markdownRoundTrip(t, string(source))
			stem := strings.TrimSuffix(file, ".md")
			want, err := os.ReadFile(stem + ".want.md")
			if err != nil {
				data, _ := json.Marshal(warnings)
				t.Fatalf("missing golden %s; output=%q warnings=%s", stem, markdownStripMarkers(md), data)
			}
			if got := markdownStripMarkers(md); got != string(want) {
				t.Errorf("output differs\nwant:\n%s\ngot:\n%s", want, got)
			}
			data, err := os.ReadFile(stem + ".warnings.json")
			if err != nil {
				t.Fatal(err)
			}
			var expected []Warning
			if err = json.Unmarshal(data, &expected); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(warnings, expected) {
				t.Errorf("warnings\nwant: %#v\ngot: %#v", expected, warnings)
			}
		})
	}
}

func TestMarkdownDeterminismAndPlacement(t *testing.T) {
	t.Parallel()
	place := Placement{Page: Target{Tmp: "page"}, Parent: &Target{Tmp: "parent"}, Before: &Target{Tmp: "anchor"}}
	src := "first\n\n- outer\n  - child\n\n| Name | Value |\n| --- | --- |\n| row | cell |\n\nlast\n"
	ops, warnings, err := ParseMarkdown(src, place, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		again, w, err := ParseMarkdown(src, place, nil)
		if err != nil || !reflect.DeepEqual(ops, again) || !slices.Equal(warnings, w) {
			t.Fatalf("not deterministic: %v", err)
		}
	}
	first := ops[0].(CreateBlockOp)
	if !reflect.DeepEqual(first.Place, place) || first.Ref != "$md1" {
		t.Fatalf("first placement or ref changed: %#v", first)
	}
	second := ops[1].(CreateBlockOp)
	if second.Place.Before != nil || second.Place.After == nil || second.Place.After.Tmp != first.Ref {
		t.Fatalf("source-order placement: %#v", second.Place)
	}
	refs := map[string]bool{}
	for _, op := range ops {
		switch v := op.(type) {
		case CreateBlockOp:
			refs[v.Ref] = true
		case CreateDatabaseOp:
			refs[v.Ref] = true
			refs[v.BlockRef] = true
			refs[v.Ref+".title"] = true
			refs[v.Ref+".view"] = true
		case CreatePropertyOp:
			refs[v.Ref] = true
		case CreateRowOp:
			refs[v.Ref] = true
		}
	}
	if len(refs) != 10 {
		t.Fatalf("creation names = %#v", refs)
	}
}

func TestMarkdownMarkersAreNotWrites(t *testing.T) {
	t.Parallel()
	id := markdownTestID(BlockKind, 42)
	ops, warnings, err := ParseMarkdown("<!-- flats:block "+string(id)+" -->\ntext", Placement{Page: Target{Tmp: "page"}}, nil)
	if err != nil || len(ops) != 1 || len(warnings) != 0 {
		t.Fatalf("marker parse: %#v %#v %v", ops, warnings, err)
	}
	if op := ops[0].(CreateBlockOp); op.ProposedID != nil || op.Ref != "$md1" || op.Content.Kind != Paragraph {
		t.Fatalf("marker reused identity: %#v", op)
	}
	for _, marker := range []string{"<!-- flats:block b_invalid -->", "<!-- flats:block " + string(id) + " inc=42 -->", "<!--flats:block " + string(id) + "-->"} {
		ops, warnings, err := ParseMarkdown(marker, Placement{Page: Target{Tmp: "page"}}, nil)
		if err != nil || len(ops) != 1 || len(warnings) != 1 || warnings[0].Code != "raw_html" || markdownPlain(ops[0].(CreateBlockOp).Content.Text) != marker {
			t.Fatalf("forged marker lost: %#v %#v %v", ops, warnings, err)
		}
	}
}

func TestMarkdownImagesAtomic(t *testing.T) {
	t.Parallel()
	hash := strings.Repeat("a", 64)
	resolve := func(path string) (string, bool) {
		return hash, path == "assets/current" || path == "uploaded" || path == "assets/"+hash
	}
	for _, path := range []string{"missing", "https://example.test/image.png", "//example.test/image.png"} {
		t.Run(path, func(t *testing.T) {
			ops, _, err := ParseMarkdown("before\n\n![image]("+path+")\n\nafter", Placement{Page: Target{Tmp: "page"}}, resolve)
			if err == nil || len(ops) != 0 || !strings.Contains(err.Error(), path) {
				t.Fatalf("unknown path not atomically refused: %#v %v", ops, err)
			}
		})
	}
	ops, _, err := ParseMarkdown("before ![current](assets/current) middle ![upload](uploaded) after", Placement{Page: Target{Tmp: "page"}}, resolve)
	if err != nil || len(ops) != 5 {
		t.Fatalf("images did not split in order: %#v %v", ops, err)
	}
	for i, op := range ops {
		c := op.(CreateBlockOp).Content
		if i%2 == 1 && (c.Kind != Image || c.Asset != hash) {
			t.Fatalf("image %d: %#v", i, c)
		}
	}
}

func TestMarkdownLiteralCodeAndEntities(t *testing.T) {
	t.Parallel()
	marker := "<!-- flats:block " + string(markdownTestID(BlockKind, 42)) + " -->"
	src := "```text\n" + marker + "\n[^a]: literal\n```\n\n`[^b]` and \\[^c] and \\&lt;tag\\&gt;.\n"
	ops, warnings, err := ParseMarkdown(src, Placement{Page: Target{Tmp: "page"}}, nil)
	if err != nil || len(warnings) != 0 || len(ops) != 2 {
		t.Fatalf("literal syntax interpreted: %#v %#v %v", ops, warnings, err)
	}
	if c := ops[0].(CreateBlockOp).Content; c.Code != marker+"\n[^a]: literal" {
		t.Fatalf("code changed: %q", c.Code)
	}
	if text := markdownPlain(ops[1].(CreateBlockOp).Content.Text); text != "[^b] and [^c] and &lt;tag&gt;." {
		t.Fatalf("entity double decoding: %q", text)
	}
	raw := "<div>\n" + marker + "\n[^a]: literal\n</div>"
	ops, warnings, err = ParseMarkdown(raw, Placement{Page: Target{Tmp: "page"}}, nil)
	if err != nil || len(warnings) != 1 || len(ops) != 1 || markdownPlain(ops[0].(CreateBlockOp).Content.Text) != raw {
		t.Fatalf("literal HTML changed: %#v %#v %v", ops, warnings, err)
	}
}

func TestMarkdownImagesEveryContext(t *testing.T) {
	t.Parallel()
	for _, src := range []string{"- ![alt](https://example.test/image)", "> ![alt](https://example.test/image)", "| Title |\n| --- |\n| ![alt](https://example.test/image) |"} {
		t.Run(src, func(t *testing.T) {
			ops, _, err := ParseMarkdown(src, Placement{Page: Target{Tmp: "page"}}, func(string) (string, bool) {
				t.Fatal("external image reached asset lookup")
				return strings.Repeat("a", 64), true
			})
			if err == nil || len(ops) != 0 {
				t.Fatalf("external image accepted: %#v %v", ops, err)
			}
		})
	}
	for _, src := range []string{"- before ![alt](asset) after", "> before ![alt](asset) after"} {
		t.Run(src, func(t *testing.T) {
			ops, warnings, err := ParseMarkdown(src, Placement{Page: Target{Tmp: "page"}}, func(string) (string, bool) { return strings.Repeat("a", 64), true })
			if err != nil || len(warnings) != 0 || len(ops) != 3 || ops[1].(CreateBlockOp).Content.Kind != Image {
				t.Fatalf("context image lost: %#v %#v %v", ops, warnings, err)
			}
		})
	}
}

func TestMarkdownLossyBlockKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		kinds []ContentKind
		texts []string
	}{
		{name: "headings", kinds: []ContentKind{Heading3, Heading3, Heading3}, texts: []string{"Four", "Five", "Six"}},
		{name: "nesting", kinds: []ContentKind{Bullet, Bullet, Bullet, Bullet, Bullet, Bullet, Bullet, Bullet}, texts: []string{"one", "two", "three", "four", "five", "six", "seven", "eight"}},
		{name: "html", kinds: []ContentKind{Paragraph, Paragraph, Paragraph}, texts: []string{"<div>literal HTML</div>", "Inline <b>bold</b> text.", "<!-- ordinary comment -->"}},
		{name: "footnotes", kinds: []ContentKind{Paragraph, Paragraph}, texts: []string{"A footnote[^a].", "[^a]: Footnote body."}},
		{name: "quotes", kinds: []ContentKind{Quote, Paragraph, Quote, Bullet}, texts: []string{"simple", "second paragraph", "nested", "item"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, err := os.ReadFile("testdata/markdown/lossy/" + tc.name + ".md")
			if err != nil {
				t.Fatal(err)
			}
			ops, _, err := ParseMarkdown(string(src), Placement{Page: Target{Tmp: "page"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(ops) != len(tc.kinds) {
				t.Fatalf("blocks = %d", len(ops))
			}
			depth := map[string]int{}
			for i, op := range ops {
				b := op.(CreateBlockOp)
				if b.Content.Kind != tc.kinds[i] || markdownPlain(b.Content.Text) != tc.texts[i] {
					t.Fatalf("block %d = %#v", i, b.Content)
				}
				d := 1
				if b.Place.Parent != nil {
					d += depth[b.Place.Parent.Tmp]
				}
				depth[b.Ref] = d
				if d > MaxBlockDepth {
					t.Fatalf("depth %d exceeds limit", d)
				}
			}
		})
	}
}

func markdownRoundTrip(t *testing.T, src string) (string, []Warning) {
	t.Helper()
	ops, warnings, err := ParseMarkdown(src, Placement{Page: Target{Tmp: "page"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, blocks, dbs := markdownBuild(t, ops)
	md, _, exportWarnings := ExportMarkdown(page, blocks, dbs)
	return md, append(warnings, exportWarnings...)
}

func markdownTestID(kind Kind, n int) ID {
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[8:], uint64(n))
	prefix := map[Kind]string{PageKind: "p_", BlockKind: "b_", DatabaseKind: "d_", PropertyKind: "pr_", RowKind: "r_", ViewKind: "v_", OptionKind: "o_"}[kind]
	return ID(prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])))
}
func markdownBuild(t *testing.T, ops []Operation) (Item, []Item, map[ID]DatabaseSnapshot) {
	t.Helper()
	n := 0
	makeItem := func(kind Kind, data ItemData) Item {
		n++
		return Item{Ref: Ref{ID: markdownTestID(kind, n), Inc: 1}, Ver: 1, V: map[GroupKey]Version{}, Data: data}
	}
	page := makeItem(PageKind, Page{Title: "Page", Position: "0"})
	refs := map[string]ID{"page": page.ID()}
	blocks := []Item{}
	dbs := map[ID]DatabaseSnapshot{}
	position := 0
	placeBlock := func(place Placement, c Content) Block {
		position++
		var parent *ID
		if place.Parent != nil {
			id, ok := refs[place.Parent.Tmp]
			if !ok {
				t.Fatalf("forward parent %s", place.Parent.Tmp)
			}
			parent = &id
		}
		return Block{Page: page.ID(), Parent: parent, Position: fmt.Sprintf("%06d", position), Content: c}
	}
	for _, op := range ops {
		switch op := op.(type) {
		case CreateBlockOp:
			item := makeItem(BlockKind, placeBlock(op.Place, op.Content))
			refs[op.Ref] = item.ID()
			blocks = append(blocks, item)
		case CreateDatabaseOp:
			block := makeItem(BlockKind, placeBlock(op.Place, Content{}))
			db := makeItem(DatabaseKind, Database{Block: block.ID(), Title: op.Title, PropertyOrder: []ID{}, ViewOrder: []ID{}})
			refs[op.Ref], refs[op.BlockRef] = db.ID(), block.ID()
			prop := makeItem(PropertyKind, Property{Database: db.ID(), Definition: PropertyDef{Name: *op.TitlePropertyName, Type: TitleType}, OptionOrder: []ID{}})
			view := makeItem(ViewKind, View{Database: db.ID()})
			refs[op.Ref+".title"], refs[op.Ref+".view"] = prop.ID(), view.ID()
			d := db.Data.(Database)
			d.PropertyOrder = append(d.PropertyOrder, prop.ID())
			d.ViewOrder = append(d.ViewOrder, view.ID())
			db.Data = d
			b := block.Data.(Block)
			b.Content = Content{Kind: InlineDatabase, Database: db.ID()}
			block.Data = b
			blocks = append(blocks, block)
			dbs[db.ID()] = DatabaseSnapshot{Database: db, Schema: Schema{Database: db.ID(), Properties: []Property{prop.Data.(Property)}, Options: map[ID][]Option{}}, Views: []Item{view}, Rows: []RowValues{}}
		case CreatePropertyOp:
			id := refs[op.Database.Tmp]
			snapshot := dbs[id]
			item := makeItem(PropertyKind, Property{Database: id, Definition: op.Definition, OptionOrder: []ID{}})
			refs[op.Ref] = item.ID()
			snapshot.Schema.Properties = append(snapshot.Schema.Properties, item.Data.(Property))
			d := snapshot.Database.Data.(Database)
			d.PropertyOrder = append(d.PropertyOrder, item.ID())
			snapshot.Database.Data = d
			dbs[id] = snapshot
		case CreateRowOp:
			id := refs[op.Database.Tmp]
			snapshot := dbs[id]
			row := makeItem(RowKind, Row{})
			refs[op.Ref] = row.ID()
			cells := map[ID]CellValue{}
			for i, cell := range op.Cells {
				cells[refs[cell.Property.Tmp]] = CellValue{Type: snapshot.Schema.Properties[i].Definition.Type, Value: *cell.Value.Text}
			}
			snapshot.Rows = append(snapshot.Rows, RowValues{Row: row.ID(), Position: fmt.Sprintf("%06d", len(snapshot.Rows)), Cells: cells})
			dbs[id] = snapshot
		default:
			t.Fatalf("unexpected Markdown operation %T", op)
		}
	}
	for _, item := range append([]Item{page}, blocks...) {
		if err := item.Validate(); err != nil {
			t.Fatalf("invalid built item: %#v: %v", item, err)
		}
	}
	return page, blocks, dbs
}

var markdownTestMarker = regexp.MustCompile(`(?m)^[ \t]*<!-- flats:block b_[a-z2-7]{26} -->\n`)

func markdownStripMarkers(md string) string {
	source := []byte(md)
	doc := goldmark.New().Parser().Parse(text.NewReader(source))
	var code [][2]int
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && (n.Kind() == ast.KindCodeBlock || n.Kind() == ast.KindFencedCodeBlock) {
			for i := 0; i < n.Lines().Len(); i++ {
				line := n.Lines().At(i)
				code = append(code, [2]int{line.Start, line.Stop})
			}
		}
		return ast.WalkContinue, nil
	})
	var out strings.Builder
	last := 0
	for _, marker := range markdownTestMarker.FindAllStringIndex(md, -1) {
		literal := false
		for _, span := range code {
			if marker[0] < span[1] && marker[1] > span[0] {
				literal = true
				break
			}
		}
		if !literal {
			out.WriteString(md[last:marker[0]])
			last = marker[1]
		}
	}
	out.WriteString(md[last:])
	return out.String()
}
func markdownNormalized(t *testing.T, src string) string {
	t.Helper()
	var rendered bytes.Buffer
	if err := goldmark.New(goldmark.WithExtensions(extension.Table, extension.TaskList, extension.Strikethrough)).Convert([]byte(markdownStripMarkers(src)), &rendered); err != nil {
		t.Fatal(err)
	}
	root, err := html.Parse(&rendered)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.CommentNode {
			return
		}
		if n.Type == html.TextNode {
			for parent := n.Parent; parent != nil; parent = parent.Parent {
				if parent.Type == html.ElementNode && (parent.Data == "code" || parent.Data == "pre") {
					out.WriteString(strconv.Quote(n.Data))
					return
				}
			}
			text := strings.Join(strings.Fields(n.Data), " ")
			if text != "" {
				out.WriteString(text + " ")
			}
			return
		}
		unwrap := n.Data == "p" && n.Parent != nil && n.Parent.Data == "li"
		if n.Type == html.ElementNode && !unwrap {
			out.WriteString("<" + n.Data)
			for _, a := range n.Attr {
				if a.Key == "href" || a.Key == "class" || a.Key == "checked" {
					out.WriteString(" " + a.Key + "=" + a.Val)
				}
			}
			out.WriteString(">")
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
		if n.Type == html.ElementNode && !unwrap {
			out.WriteString("</" + n.Data + ">")
		}
	}
	visit(root)
	return out.String()
}
