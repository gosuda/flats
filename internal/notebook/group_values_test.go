package notebook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func TestGroupValuesRoundTrip(t *testing.T) {
	t.Parallel()
	fixtures := vocabFixtures(t, "groups")
	expected := vocabFixtures(t, "groups.canonical")
	if len(fixtures) != len(expected) {
		t.Fatalf("%d group fixtures, %d expected outputs", len(fixtures), len(expected))
	}
	for n, raw := range fixtures {
		var fixture struct {
			Kind  Kind            `json:"kind"`
			Key   GroupKey        `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(raw, &fixture); err != nil {
			t.Fatal(err)
		}
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			value, err := DecodeGroup(fixture.Kind, fixture.Key, fixture.Value)
			if err != nil {
				t.Fatal(err)
			}
			first, err := EncodeGroup(fixture.Kind, fixture.Key, value)
			if err != nil {
				t.Fatal(err)
			}
			if want := bytes.TrimSpace(expected[n]); !bytes.Equal(first, want) {
				t.Fatalf("canonical group = %s, want %s", first, want)
			}
			decoded, err := DecodeGroup(fixture.Kind, fixture.Key, first)
			if err != nil {
				t.Fatal(err)
			}
			second, err := EncodeGroup(fixture.Kind, fixture.Key, decoded)
			if err != nil || !bytes.Equal(first, second) {
				t.Fatalf("unstable group %s -> %s: %v", first, second, err)
			}
		})
	}
}

func TestGroupValuesRejectMalformed(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		kind Kind
		key  GroupKey
		raw  string
	}{
		{"unknown group", PageKind, "unknown", `"text"`},
		{"wrong kind", BlockKind, TitleGroup, `"text"`},
		{"missing parent", PageKind, PlaceGroup, `{"position":"a0"}`},
		{"extra place field", RowKind, PlaceGroup, `{"position":"a0","title":"wrong"}`},
		{"null title", PageKind, TitleGroup, `null`},
		{"null order", DatabaseKind, ViewOrderGroup, `null`},
		{"wrong order prefix", DatabaseKind, ViewOrderGroup, `["` + string(vocabID(PageKind, 0)) + `"]`},
		{"bad cell", RowKind, CellGroup(vocabID(PropertyKind, 0)), `{"type":"number","value":"NaN"}`},
		{"bad option", OptionKind, DefinitionGroup, `{"name":"test","color":"unicorn"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodeGroup(tt.kind, tt.key, []byte(tt.raw)); err == nil {
				t.Fatal("accepted invalid group")
			}
		})
	}
	if _, err := EncodeGroup(PageKind, PlaceGroup, TitleValue("wrong")); err == nil {
		t.Fatal("encoded wrong group union")
	}
}

func TestEncodeGroupText(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		kind  Kind
		key   GroupKey
		value GroupValue
		want  string
	}{
		{"invalid page title", PageKind, TitleGroup, TitleValue(string([]byte{0xff})), ""},
		{"invalid database title", DatabaseKind, TitleGroup, TitleValue(string([]byte{0xc3})), ""},
		{"invalid option name", OptionKind, DefinitionGroup, OptionDefinition{Name: string([]byte{0xff}), Color: Gray}, ""},
		{"decomposed title", PageKind, TitleGroup, TitleValue("e\u0301"), "\"e\u0301\""},
		{"decomposed option name", OptionKind, DefinitionGroup, OptionDefinition{Name: "e\u0301", Color: Gray}, "{\"color\":\"gray\",\"name\":\"e\u0301\"}"},
		{"replacement title", PageKind, TitleGroup, TitleValue("\ufffd"), `"�"`},
		{"replacement option name", OptionKind, DefinitionGroup, OptionDefinition{Name: "\ufffd", Color: Gray}, `{"color":"gray","name":"�"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data, err := EncodeGroup(tt.kind, tt.key, tt.value)
			if tt.want == "" {
				if err == nil || data != nil {
					t.Fatalf("invalid UTF-8 encoded as %q, error %v", data, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tt.want {
				t.Fatalf("encoded group = %s, want %s", data, tt.want)
			}
			decoded, err := DecodeGroup(tt.kind, tt.key, data)
			if err != nil {
				t.Fatal(err)
			}
			if decoded != tt.value {
				t.Fatalf("decoded group = %#v, want %#v", decoded, tt.value)
			}
		})
	}
}

func FuzzGroup(f *testing.F) {
	raw, err := os.ReadFile("testdata/groups.json")
	if err != nil {
		f.Fatal(err)
	}
	var fixtures []struct {
		Kind  Kind            `json:"kind"`
		Key   GroupKey        `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		f.Fatal(err)
	}
	for _, fixture := range fixtures {
		f.Add(string(fixture.Kind), string(fixture.Key), []byte(fixture.Value))
	}
	f.Fuzz(func(t *testing.T, kind, key string, data []byte) {
		value, err := DecodeGroup(Kind(kind), GroupKey(key), data)
		if err != nil {
			return
		}
		encoded, err := EncodeGroup(Kind(kind), GroupKey(key), value)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeGroup(Kind(kind), GroupKey(key), encoded)
		if err != nil {
			t.Fatal(err)
		}
		again, err := EncodeGroup(Kind(kind), GroupKey(key), decoded)
		if err != nil || !bytes.Equal(encoded, again) {
			t.Fatalf("unstable group: %s -> %s: %v", encoded, again, err)
		}
	})
}
