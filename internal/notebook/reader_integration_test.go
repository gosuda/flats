package notebook

import (
	"database/sql"
	"encoding/json"
	"os"
	"slices"
	"testing"
)

func TestSpillSchemaPropertyIDsQuery(t *testing.T) {
	t.Parallel()
	for _, public := range []bool{false, true} {
		name := "private"
		if public {
			name = "public"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source, read, db := spillSource(t, 3, "title")
			title, text, number := spillID(PropertyKind, 1), spillID(PropertyKind, 2), spillID(PropertyKind, 3)
			ids := []ID{title, number, text}
			if err := source.transaction(t.Context(), func(tx *sql.Tx) error {
				for _, item := range []Item{
					spillItem(db, Database{Block: spillID(BlockKind, 1), Title: "Database", PropertyOrder: ids, ViewOrder: []ID{}}),
					spillItem(text, Property{Database: db, Definition: PropertyDef{Name: "Text", Type: TextType}, OptionOrder: []ID{}}),
					spillItem(number, Property{Database: db, Definition: PropertyDef{Name: "Number", Type: NumberType}, OptionOrder: []ID{}}),
				} {
					if err := putItem(t.Context(), tx, item); err != nil {
						return err
					}
				}
				for i, value := range []Decimal{"20", "10", "30"} {
					item, err := loadRecord(t.Context(), tx, spillID(RowKind, uint64(i+1)))
					if err != nil {
						return err
					}
					item.Item.Data.(Row).Cells[number] = CellValue{Type: NumberType, Value: value}
					if err := putItem(t.Context(), tx, item.Item); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			property := Target{Ref: &Ref{ID: number, Inc: 7}}
			view := ViewDef{
				Sort:   []Sort{{Property: property, Direction: "asc"}},
				Filter: []Filter{{Property: property, Op: GreaterEqual, Value: json.RawMessage(`"20"`)}},
			}
			calls := 0
			order := func(schema Schema, view ViewDef, rows []RowValues) []ID {
				calls++
				if schema.Database != db || !slices.Equal(schema.PropertyIDs, ids) || len(schema.Properties) != len(ids) {
					t.Fatalf("query schema: %+v", schema)
				}
				for i, want := range []string{"Title", "Number", "Text"} {
					if got := schema.Properties[i].Definition.Name; got != want {
						t.Fatalf("property %s has payload %q, want %q", schema.PropertyIDs[i], got, want)
					}
				}
				return OrderRows(schema, view, rows)
			}
			m := spillManager(t, 1<<30, newSpillClock())
			captured, err := m.Capture(t.Context(), read, Scope{Database: db, View: &view, Public: public}, order)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = captured.Close() })
			var got []ID
			for _, window := range spillWindows(t, captured)[1:] {
				for _, raw := range window {
					var item struct {
						ID  ID  `json:"id"`
						Ref Ref `json:"ref"`
					}
					if err := json.Unmarshal(raw, &item); err != nil {
						t.Fatal(err)
					}
					id := item.ID
					if !public {
						id = item.Ref.ID
					}
					got = append(got, id)
				}
			}
			want := []ID{spillID(RowKind, 1), spillID(RowKind, 3)}
			if calls == 0 || !slices.Equal(got, want) {
				t.Fatalf("query calls=%d rows=%v, want %v", calls, got, want)
			}
		})
	}
}

func TestSpillCapturedClosePreserved(t *testing.T) {
	t.Parallel()
	_, read, db := spillSource(t, 1, "title")
	m := spillManager(t, 1<<30, newSpillClock())
	first, err := m.Capture(t.Context(), read, Scope{Database: db}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Capture(t.Context(), read, Scope{Database: db}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("repeat close: %v", err)
	}
	if _, ok := m.Lookup(first.Cursor()); ok {
		t.Fatal("closed spill still resolves")
	}
	if _, _, err := first.Part(0); err == nil {
		t.Fatal("closed spill still reads")
	}
	if _, err := os.Stat(first.dir); !os.IsNotExist(err) {
		t.Fatalf("closed spill scratch remains: %v", err)
	}
	if _, ok := m.Lookup(second.Cursor()); !ok {
		t.Fatal("closing a spill cancelled another capture of the same scope")
	}
	if _, _, err := second.Part(0); err != nil {
		t.Fatalf("second spill unavailable: %v", err)
	}
}
