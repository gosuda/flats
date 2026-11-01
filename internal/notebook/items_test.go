package notebook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestItemsRoundTrip(t *testing.T) {
	t.Parallel()
	seen := map[Kind]int{}
	for n, raw := range vocabFixtures(t, "items") {
		item, err := DecodeItem(raw)
		if err != nil {
			t.Fatalf("fixture %d: %v", n, err)
		}
		seen[item.Kind()]++
		t.Run(fmt.Sprintf("%s/%d", item.Kind(), n), func(t *testing.T) {
			first, err := EncodeItem(item)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeItem(first)
			if err != nil {
				t.Fatal(err)
			}
			second, err := EncodeItem(decoded)
			if err != nil || !bytes.Equal(first, second) {
				t.Fatalf("unstable item: %s -> %s, %v", first, second, err)
			}
			if decoded.ID() != item.ID() || decoded.Kind() != item.Kind() || decoded.Incarnation() != 7 || decoded.Version() != 11 {
				t.Fatal("identity stamps changed")
			}
			if !reflect.DeepEqual(decoded.Data, item.Data) {
				t.Fatalf("item data changed: %#v != %#v", decoded.Data, item.Data)
			}
		})
	}
	if len(seen) != 7 {
		t.Fatalf("covered %d item kinds", len(seen))
	}
}
func TestItemsRejectMalformed(t *testing.T) {
	t.Parallel()
	item := Item{Ref: Ref{ID: vocabID(PageKind, 0), Inc: 7}, Ver: 11, V: map[GroupKey]Version{TitleGroup: 9, PlaceGroup: 7}, Data: Page{Title: "Page", Position: "a0"}}
	data, err := EncodeItem(item)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, old, new string }{
		{"wrong kind", `"kind":"page"`, `"kind":"block"`}, {"wrong prefix", `"p_`, `"z_`}, {"unknown field", `"title":"Page"`, `"title":"Page","checked":false`}, {"missing parent", `"parent":null,`, ``}, {"missing title", `"title":"Page",`, ``}, {"null title", `"title":"Page"`, `"title":null`}, {"invalid position", `"a0"`, `"../"`}, {"empty position", `"a0"`, `""`}, {"zero inc", `"inc":"7"`, `"inc":"0"`}, {"early version", `"ver":"11"`, `"ver":"6"`}, {"future group version", `"title":"9"`, `"title":"12"`}, {"old group version", `"title":"9"`, `"title":"6"`}, {"wrong group", `"place":"7"`, `"content":"7"`}, {"unknown kind", `"kind":"page"`, `"kind":"widget"`}, {"unknown duplicate case", `"title":"Page"`, `"title":"Page","Title":"Oops"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := strings.ReplaceAll(string(data), tt.old, tt.new)
			if raw == string(data) {
				t.Fatalf("mutation did not match %s in %s", tt.old, data)
			}
			if _, err := DecodeItem([]byte(raw)); err == nil {
				t.Fatalf("accepted %s", raw)
			}
		})
	}
	item.Data = Block{Page: vocabID(PageKind, 0), Position: "a0", Content: Content{Kind: Divider}}
	if _, err := EncodeItem(item); err == nil {
		t.Fatal("wrong item subtype encoded")
	}
	var nilPage *Page
	item.Data = nilPage
	if _, err := EncodeItem(item); err == nil {
		t.Fatal("nil item subtype encoded")
	}
}
func TestImplicitGroupVersion(t *testing.T) {
	t.Parallel()
	for _, raw := range vocabFixtures(t, "items") {
		item, err := DecodeItem(raw)
		if err != nil {
			t.Fatal(err)
		}
		keys := []GroupKey{}
		for key := range item.V {
			keys = append(keys, key)
		}
		if row, ok := item.Data.(Row); ok {
			for id := range row.Cells {
				keys = append(keys, CellGroup(id))
			}
		}
		item.V = map[GroupKey]Version{}
		item.Ver = Version(item.Ref.Inc)
		if err := item.Validate(); err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			version, err := item.GroupVersion(key)
			if err != nil || version != Version(item.Ref.Inc) {
				t.Fatalf("implicit %s/%s version = %d: %v", item.Kind(), key, version, err)
			}
			item.V[key] = Version(item.Ref.Inc) + 1
			version, err = item.GroupVersion(key)
			if err != nil || version != Version(item.Ref.Inc)+1 {
				t.Fatalf("explicit group version = %d: %v", version, err)
			}
			delete(item.V, key)
		}
	}
}

func TestRowGroupsIndependent(t *testing.T) {
	t.Parallel()
	a, b := vocabID(PropertyKind, 0), vocabID(PropertyKind, 1)
	row := Item{Ref: Ref{ID: vocabID(RowKind, 0), Inc: 7}, Ver: 13, V: map[GroupKey]Version{PlaceGroup: 7, CellGroup(a): 11, CellGroup(b): 13}, Data: Row{Database: vocabID(DatabaseKind, 0), Position: "a0", Cells: map[ID]CellValue{a: {Type: TextType, Value: "one"}, b: {Type: TextType, Value: "two"}}}}
	encoded, err := EncodeItem(row)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeItem(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.V[CellGroup(a)] != 11 || got.V[CellGroup(b)] != 13 {
		t.Fatalf("cell versions changed: %#v", got.V)
	}
	got.V[CellGroup(a)] = 14
	got.Ver = 14
	encoded, err = EncodeItem(got)
	if err != nil {
		t.Fatal(err)
	}
	got, err = DecodeItem(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.V[CellGroup(b)] != 13 {
		t.Fatal("updating one group changed the other cell version")
	}
}
func TestStoredViewsResolveFreshReferences(t *testing.T) {
	t.Parallel()
	v := vocabView()
	v.Filter = []Filter{{Property: vocabTarget(PropertyKind, 1), Op: IsAnyOf, Value: json.RawMessage(`[{"id":"` + string(vocabID(OptionKind, 0)) + `","inc":"7"},{"id":"` + string(vocabID(OptionKind, 2)) + `","inc":"7"}]`)}}
	stored, err := StoreView(v)
	if err != nil {
		t.Fatal(err)
	}
	data, err := CanonicalJSON(stored)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"inc"`)) || bytes.Contains(data, []byte(`"tmp"`)) {
		t.Fatalf("runtime references leaked to stored view: %s", data)
	}
	var got StoredView
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	wire, err := got.Wire(func(id ID) (Ref, error) { return Ref{ID: id, Inc: 99}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if wire.Display.Visible[0].Ref.Inc != 99 || wire.Sort[0].Property.Ref.Inc != 99 {
		t.Fatal("view did not acquire current refs")
	}
	var options []Target
	if err := json.Unmarshal(wire.Filter[0].Value, &options); err != nil {
		t.Fatal(err)
	}
	if len(options) != 2 || options[0].Ref.Inc != 99 || options[1].Ref.Inc != 99 {
		t.Fatalf("option refs = %#v", options)
	}
	_, err = got.Wire(func(ID) (Ref, error) { return Ref{}, fmt.Errorf("gone") })
	if err == nil {
		t.Fatal("resolver failure ignored")
	}
	v.Display.Visible = []Target{{Tmp: "new-property"}}
	if _, err := StoreView(v); err == nil {
		t.Fatal("stored unresolved temporary ref")
	}
}
func FuzzItem(f *testing.F) {
	for _, raw := range vocabFixtures(f, "items") {
		f.Add([]byte(raw))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := DecodeItem(data)
		if err != nil {
			return
		}
		first, err := EncodeItem(v)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeItem(first)
		if err != nil {
			t.Fatal(err)
		}
		second, err := EncodeItem(got)
		if err != nil || !bytes.Equal(first, second) {
			t.Fatalf("unstable item: %s -> %s (%v)", first, second, err)
		}
	})
}
