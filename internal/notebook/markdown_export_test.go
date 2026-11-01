package notebook

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestMarkdownExportOrderReferencesAndFallbacks(t *testing.T) {
	t.Parallel()
	page, blocks, dbs := markdownBuild(t, []Operation{
		CreateBlockOp{CreateFields: CreateFields{Ref: "toggle"}, Place: Placement{}, Content: Content{Kind: Toggle, Text: RichText{{Text: "Summary", Marks: []Mark{}}}}},
		CreateBlockOp{CreateFields: CreateFields{Ref: "child"}, Place: Placement{Parent: &Target{Tmp: "toggle"}}, Content: Content{Kind: Paragraph, Text: RichText{{Text: "Child", Marks: []Mark{}}}}},
		CreateBlockOp{CreateFields: CreateFields{Ref: "link"}, Place: Placement{}, Content: Content{Kind: PageLink, Target: markdownTestID(PageKind, 88)}},
		CreateBlockOp{CreateFields: CreateFields{Ref: "image"}, Place: Placement{}, Content: Content{Kind: Image, Asset: strings.Repeat("a", 64), Alt: "Alt", Caption: RichText{{Text: "Caption", Marks: []Mark{}}}}},
	})
	before := append([]Item{}, blocks...)
	md, refs, warnings := ExportMarkdown(page, blocks, dbs)
	if !reflect.DeepEqual(blocks, before) {
		t.Fatal("export mutated input")
	}
	wantCodes := []string{"toggle", "page_link", "image_caption"}
	codes := []string{}
	for _, w := range warnings {
		codes = append(codes, w.Code)
	}
	if !slices.Equal(codes, wantCodes) {
		t.Fatalf("fallback warnings = %#v", warnings)
	}
	if len(refs) != 4 {
		t.Fatalf("references = %#v", refs)
	}
	for i, ref := range refs {
		if ref != blocks[i].Ref {
			t.Fatalf("reference %d lost incarnation", i)
		}
	}
	if !strings.Contains(md, "Summary\n\n") || !strings.Contains(md, "Child\n\n") || !strings.Contains(md, "[Missing page]") || !strings.Contains(md, "Caption") {
		t.Fatalf("unreadable fallback:\n%s", md)
	}
	slices.Reverse(blocks)
	again, againRefs, againWarnings := ExportMarkdown(page, blocks, dbs)
	if md != again || !slices.Equal(refs, againRefs) || !slices.Equal(warnings, againWarnings) {
		t.Fatal("export depends on input slice order")
	}
}

func TestMarkdownExportDatabaseDisplay(t *testing.T) {
	t.Parallel()
	ops, _, err := ParseMarkdown("| Title | Status | Tags | Done | Amount |\n| --- | --- | --- | --- | --- |\n| Row | | | | |", Placement{Page: Target{Tmp: "page"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, blocks, dbs := markdownBuild(t, ops)
	id := blocks[0].Data.(Block).Content.Database
	snapshot := dbs[id]
	ids := snapshot.Database.Data.(Database).PropertyOrder
	o1, o2 := markdownTestID(OptionKind, 200), markdownTestID(OptionKind, 201)
	snapshot.Schema.Properties[1].Definition.Type = SelectType
	snapshot.Schema.Properties[1].OptionOrder = []ID{o1, o2}
	snapshot.Schema.Properties[2].Definition.Type = MultiSelectType
	snapshot.Schema.Properties[2].OptionOrder = []ID{o1, o2}
	snapshot.Schema.Properties[3].Definition.Type = CheckboxType
	snapshot.Schema.Properties[4].Definition.Type = NumberType
	snapshot.Schema.Options[ids[1]] = []Option{{Property: ids[1], Name: "First"}, {Property: ids[1], Name: "Second"}}
	snapshot.Schema.Options[ids[2]] = []Option{{Property: ids[2], Name: "First"}, {Property: ids[2], Name: "Second"}}
	snapshot.Rows[0].Cells[ids[1]] = CellValue{Type: SelectType, Value: o2}
	snapshot.Rows[0].Cells[ids[2]] = CellValue{Type: MultiSelectType, Value: []ID{o2, o1}}
	snapshot.Rows[0].Cells[ids[3]] = CellValue{Type: CheckboxType, Value: true}
	snapshot.Rows[0].Cells[ids[4]] = CellValue{Type: NumberType, Value: Decimal("123.5")}
	dbs[id] = snapshot
	md, refs, warnings := ExportMarkdown(page, blocks, dbs)
	if !strings.Contains(md, "| Row | Second | First, Second | true | 123\\.5 |") {
		t.Fatalf("cell display:\n%s", md)
	}
	if len(warnings) != 1 || warnings[0].Code != "database_display_text" {
		t.Fatalf("display warning: %#v", warnings)
	}
	if len(refs) != 3 || refs[1] != snapshot.Database.Ref || refs[2] != snapshot.Views[0].Ref {
		t.Fatalf("database references: %#v", refs)
	}
}

func TestMarkdownExportMarksTransitions(t *testing.T) {
	t.Parallel()
	for _, src := range []string{"***a*b**", "**a***b*", "**a* b***", "[**linked** text](https://example.test)", "**before ![alt](asset) after**"} {
		t.Run(src, func(t *testing.T) {
			ops, _, err := ParseMarkdown(src, Placement{Page: Target{Tmp: "page"}}, func(string) (string, bool) { return strings.Repeat("a", 64), true })
			if err != nil {
				t.Fatal(err)
			}
			page, blocks, dbs := markdownBuild(t, ops)
			md, _, _ := ExportMarkdown(page, blocks, dbs)
			again, _, err := ParseMarkdown(md, Placement{Page: Target{Tmp: "page"}}, func(string) (string, bool) { return strings.Repeat("a", 64), true })
			if err != nil {
				t.Fatal(err)
			}
			_, next, _ := markdownBuild(t, again)
			if len(blocks) != len(next) {
				t.Fatalf("block count changed:\n%s", md)
			}
			for i, item := range blocks {
				if !reflect.DeepEqual(item.Data.(Block).Content, next[i].Data.(Block).Content) {
					t.Fatalf("marks changed:\n%s\nwant: %#v\ngot: %#v", md, item.Data.(Block).Content, next[i].Data.(Block).Content)
				}
			}
		})
	}
}
