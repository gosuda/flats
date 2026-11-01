package notebook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestSpillPublicItemProjection(t *testing.T) {
	t.Parallel()
	allowed := map[Kind][]string{
		PageKind:     {"title", "parent", "position"},
		BlockKind:    {"page", "parent", "position", "content"},
		DatabaseKind: {"block", "title", "property_order", "view_order"},
		PropertyKind: {"database", "definition", "option_order"},
		OptionKind:   {"property", "name", "color"},
		RowKind:      {"database", "position", "cells"},
		ViewKind:     {"database", "definition"},
	}
	seen := map[Kind]bool{}
	contentKinds := map[ContentKind]bool{}
	cellTypes := map[PropertyType]bool{}
	layouts := map[string]bool{}
	for n, raw := range vocabFixtures(t, "items") {
		item, err := DecodeItem(raw)
		if err != nil {
			t.Fatal(err)
		}
		seen[item.Kind()] = true
		switch data := item.Data.(type) {
		case Block:
			contentKinds[data.Content.Kind] = true
		case Row:
			for _, cell := range data.Cells {
				cellTypes[cell.Type] = true
			}
		case View:
			layouts[data.Definition.Display.Layout] = true
		}
		t.Run(fmt.Sprintf("%s/%d", item.Kind(), n), func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			if err := writePublicItem(&out, item); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got["id"] != string(item.ID()) || got["kind"] != string(item.Kind()) {
				t.Fatalf("public identity = %#v", got)
			}
			payload, err := json.Marshal(item.Data)
			if err != nil {
				t.Fatal(err)
			}
			var want map[string]any
			if err := json.Unmarshal(payload, &want); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(allowed[item.Kind()])+2 {
				t.Fatalf("unexpected public fields: %s", out.Bytes())
			}
			for _, field := range allowed[item.Kind()] {
				value, ok := got[field]
				if !ok || !reflect.DeepEqual(value, want[field]) {
					t.Errorf("%s = %#v, want %#v", field, value, want[field])
				}
			}
			spillAssertPublicFields(t, got)

			item.Ref.Inc = 0
			item.Ver = 0
			item.V = nil
			var unstamped bytes.Buffer
			if err := writePublicItem(&unstamped, item); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), unstamped.Bytes()) {
				t.Fatalf("private stamps changed public output: %s", unstamped.Bytes())
			}
		})
	}
	if len(seen) != 7 || len(contentKinds) != 15 || len(cellTypes) != 8 || len(layouts) != 2 {
		t.Fatalf("fixture coverage: kinds=%d content=%d cells=%d layouts=%d",
			len(seen), len(contentKinds), len(cellTypes), len(layouts))
	}
}

func TestSpillPublicItemStoredViewIDs(t *testing.T) {
	t.Parallel()
	property := vocabID(PropertyKind, 1)
	option := vocabID(OptionKind, 2)
	view := View{
		Database: vocabID(DatabaseKind, 0),
		Definition: StoredView{
			Name: "Current view",
			Display: StoredDisplay{
				Layout: "table", Visible: []ID{property},
				Widths: []StoredWidth{{Property: property, PX: 200}},
			},
			Sort: []StoredSort{{Property: property, Direction: "desc"}},
			Filter: []StoredFilter{{
				Property: property, Op: IsAnyOf,
				Value: json.RawMessage(`[{"option":"` + string(option) + `"}]`),
			}},
		},
	}
	item := Item{Ref: Ref{ID: vocabID(ViewKind, 0), Inc: 7}, Ver: 11, Data: view}
	var out bytes.Buffer
	if err := writePublicItem(&out, item); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Definition StoredView `json:"definition"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Definition, view.Definition) {
		t.Fatalf("stored IDs changed: %#v", got.Definition)
	}
	var fields map[string]any
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	spillAssertPublicFields(t, fields)
}

func TestSpillPublicItemRejectsUnsupportedData(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		data ItemData
	}{
		{name: "missing"},
		{name: "typed nil", data: (*Page)(nil)},
		{name: "unlisted concrete type", data: spillPrivateItemData{Page: Page{Title: "private"}}},
		{name: "wrong kind", data: Option{Property: vocabID(PropertyKind, 0), Name: "Option", Color: Blue}},
		{name: "invalid content", data: Block{
			Page: vocabID(PageKind, 0), Position: "a0", Content: Content{Kind: "private"},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			item := Item{Ref: Ref{ID: vocabID(PageKind, 0)}, Data: tt.data}
			if tt.name == "invalid content" {
				item.Ref.ID = vocabID(BlockKind, 0)
			}
			var out bytes.Buffer
			if err := writePublicItem(&out, item); err == nil {
				t.Fatal("accepted unsupported public item")
			}
			if out.Len() != 0 {
				t.Fatalf("failed projection wrote bytes: %s", out.Bytes())
			}
		})
	}
}

func TestSpillPublicItemPreservesContent(t *testing.T) {
	t.Parallel()
	title := `ref inc ver v authors store receipt reset undo presence <script> 한글`
	item := Item{Ref: Ref{ID: vocabID(PageKind, 0)}, Data: Page{Title: title, Position: "a0"}}
	var out bytes.Buffer
	if err := writePublicItem(&out, item); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != title {
		t.Fatalf("content was sanitized: %q", got.Title)
	}
	sentinel := errors.New("spill write failed")
	if err := writePublicItem(spillProjectionErrorWriter{err: sentinel}, item); !errors.Is(err, sentinel) {
		t.Fatalf("write error = %v, want %v", err, sentinel)
	}
}

func spillAssertPublicFields(t *testing.T, value any) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			switch key {
			case "ref", "inc", "ver", "v", "store", "state", "authors", "editor", "receipt",
				"reset", "undo", "presence", "session", "revision", "adjustment", "overwrote":
				t.Errorf("private field %q in public projection", key)
			}
			spillAssertPublicFields(t, child)
		}
	case []any:
		for _, child := range value {
			spillAssertPublicFields(t, child)
		}
	}
}

type spillPrivateItemData struct {
	Page
}

func (spillPrivateItemData) MarshalJSON() ([]byte, error) {
	panic("public projection must not marshal unlisted item data")
}

type spillProjectionErrorWriter struct {
	err error
}

func (w spillProjectionErrorWriter) Write([]byte) (int, error) { return 0, w.err }
