package notebook

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestMarkdownExportedAssetAlias(t *testing.T) {
	t.Parallel()
	hash := strings.Repeat("a", 64)
	page, blocks, dbs := markdownBuild(t, []Operation{
		CreateBlockOp{CreateFields: CreateFields{Ref: "image"}, Content: Content{Kind: Image, Asset: hash, Alt: "image", Caption: RichText{}}},
	})
	md, _, warnings := ExportMarkdown(page, blocks, dbs)
	if len(warnings) != 0 || !strings.Contains(md, "assets/"+hash) {
		t.Fatalf("export: %q, warnings=%v", md, warnings)
	}
	var paths []string
	ops, warnings, err := ParseMarkdown(md, Placement{Page: Target{Tmp: "page"}}, func(path string) (string, bool) {
		paths = append(paths, path)
		return hash, path == hash
	})
	if err != nil || len(warnings) != 0 || len(ops) != 1 {
		t.Fatalf("parse exported alias: ops=%v warnings=%v err=%v", ops, warnings, err)
	}
	if !slices.Equal(paths, []string{"assets/" + hash, hash}) {
		t.Fatalf("asset lookup paths = %v", paths)
	}
	if got := ops[0].(CreateBlockOp).Content.Asset; got != hash {
		t.Fatalf("asset = %q, want %q", got, hash)
	}
	for _, path := range []string{"assets/" + strings.Repeat("b", 64), "assets/" + hash + "/extra", "assets/" + hash + "?query", "assets/" + strings.ToUpper(hash)} {
		t.Run(path, func(t *testing.T) {
			ops, _, err := ParseMarkdown("before\n\n![image]("+path+")", Placement{Page: Target{Tmp: "page"}}, func(path string) (string, bool) {
				return hash, path == hash
			})
			if err == nil || len(ops) != 0 || !strings.Contains(err.Error(), path) {
				t.Fatalf("unknown alias accepted: ops=%v err=%v", ops, err)
			}
		})
	}
}

func TestMarkdownParentDepthClamp(t *testing.T) {
	t.Parallel()
	var src strings.Builder
	for depth := range MaxBlockDepth + 2 {
		fmt.Fprintf(&src, "%s- item %d\n", strings.Repeat("  ", depth), depth)
	}
	for parentDepth := range MaxBlockDepth {
		t.Run(fmt.Sprintf("parent-depth-%d", parentDepth), func(t *testing.T) {
			place := Placement{Page: Target{Tmp: "page"}}
			depths := map[string]int{}
			if parentDepth > 0 {
				place.Parent = &Target{Tmp: "existing"}
				depths["existing"] = parentDepth
			}
			ops, warnings, err := parseMarkdownAtDepth(src.String(), place, nil, parentDepth)
			if err != nil || len(ops) != MaxBlockDepth+2 {
				t.Fatalf("parse: %d ops, err=%v", len(ops), err)
			}
			last := map[string]string{}
			for i, op := range ops {
				block := op.(CreateBlockOp)
				parent := ""
				if block.Place.Parent != nil {
					parent = block.Place.Parent.Tmp
				}
				depth := 1 + depths[parent]
				wantDepth := min(parentDepth+i+1, MaxBlockDepth)
				if depth != wantDepth || markdownPlain(block.Content.Text) != fmt.Sprintf("item %d", i) {
					t.Fatalf("block %d: depth=%d want=%d content=%v", i, depth, wantDepth, block.Content)
				}
				if previous := last[parent]; previous != "" {
					if block.Place.After == nil || block.Place.After.Tmp != previous || block.Place.Before != nil {
						t.Fatalf("flattened sibling order at %d: %+v", i, block.Place)
					}
				}
				depths[block.Ref] = depth
				last[parent] = block.Ref
			}
			if len(warnings) != parentDepth+2 {
				t.Fatalf("warnings=%v, want %d flattened items", warnings, parentDepth+2)
			}
			for _, warning := range warnings {
				if warning.Code != "nesting_flattened" {
					t.Fatalf("warning=%v", warning)
				}
			}
			again, againWarnings, err := parseMarkdownAtDepth(src.String(), place, nil, parentDepth)
			if err != nil || !reflect.DeepEqual(ops, again) || !slices.Equal(warnings, againWarnings) {
				t.Fatalf("depth-clamped parse is not deterministic: %v", err)
			}
		})
	}
	for _, depth := range []int{-1, MaxBlockDepth, MaxBlockDepth + 1} {
		t.Run(fmt.Sprintf("invalid-depth-%d", depth), func(t *testing.T) {
			ops, _, err := parseMarkdownAtDepth("text", Placement{Page: Target{Tmp: "page"}}, nil, depth)
			if err == nil || len(ops) != 0 {
				t.Fatalf("invalid destination accepted: ops=%v err=%v", ops, err)
			}
		})
	}
}

func TestMarkdownParentDepthClampMixedBlocks(t *testing.T) {
	t.Parallel()
	place := Placement{Page: Target{Tmp: "page"}, Parent: &Target{Tmp: "existing"}, Before: &Target{Tmp: "anchor"}}
	src := "- outer\n  - child\n\n    paragraph\n\n    ```go\n    code\n    ```\n\n    | Title |\n    | --- |\n    | row |\n\n# end\n"
	ops, warnings, err := parseMarkdownAtDepth(src, place, nil, MaxBlockDepth-1)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []ContentKind
	last := ""
	for _, op := range ops {
		var ref string
		var got Placement
		switch v := op.(type) {
		case CreateBlockOp:
			ref, got = v.Ref, v.Place
			kinds = append(kinds, v.Content.Kind)
		case CreateDatabaseOp:
			ref, got = v.BlockRef, v.Place
			kinds = append(kinds, InlineDatabase)
		default:
			continue
		}
		if got.Parent == nil || *got.Parent != *place.Parent || got.Page != place.Page {
			t.Fatalf("block escaped destination: %+v", got)
		}
		if last == "" {
			if !reflect.DeepEqual(got, place) {
				t.Fatalf("first block lost supplied anchor: %+v", got)
			}
		} else if got.Before != nil || got.After == nil || got.After.Tmp != last {
			t.Fatalf("mixed siblings not in source order: last=%q place=%+v", last, got)
		}
		last = ref
	}
	want := []ContentKind{Bullet, Bullet, Paragraph, Code, InlineDatabase, Heading1}
	if !slices.Equal(kinds, want) || len(warnings) == 0 {
		t.Fatalf("kinds=%v want=%v warnings=%v", kinds, want, warnings)
	}
}
