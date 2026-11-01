package notebook

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestMarkdownCellExporterParity(t *testing.T) {
	f := liveReadFixture(t)
	owner, db := spillID(BlockKind, 1000), spillID(DatabaseKind, 1000)
	types := []PropertyType{TitleType, TextType, URLType, NumberType, DateType, CheckboxType, SelectType, MultiSelectType}
	ids := make([]ID, len(types))
	for i := range ids {
		ids[i] = spillID(PropertyKind, uint64(1000+i))
	}
	options := map[ID][]ID{}
	for _, pid := range ids[6:] {
		options[pid] = []ID{spillID(OptionKind, uint64(len(options)*10+1000)), spillID(OptionKind, uint64(len(options)*10+1001))}
	}
	block := spillItem(owner, Block{Page: f.page.ID, Position: "A", Content: Content{Kind: InlineDatabase, Database: db}})
	snapshot := DatabaseSnapshot{Schema: Schema{Database: db, PropertyIDs: ids, Options: map[ID][]Option{}}}
	var page Item
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		if err := putItem(t.Context(), tx, block); err != nil {
			return err
		}
		if err := putItem(t.Context(), tx, spillItem(db, Database{Block: owner, Title: "Cells", PropertyOrder: ids, ViewOrder: []ID{}})); err != nil {
			return err
		}
		for i, pid := range ids {
			order := options[pid]
			if order == nil {
				order = []ID{}
			}
			p := Property{Database: db, Definition: PropertyDef{Name: string(types[i]) + " | name", Type: types[i]}, OptionOrder: order}
			if err := putItem(t.Context(), tx, spillItem(pid, p)); err != nil {
				return err
			}
			snapshot.Schema.Properties = append(snapshot.Schema.Properties, p)
			for j, oid := range order {
				o := Option{Property: pid, Name: fmt.Sprintf("choice %d | <&>\nnext", j), Color: Gray}
				if err := putItem(t.Context(), tx, spillItem(oid, o)); err != nil {
					return err
				}
				snapshot.Schema.Options[pid] = append(snapshot.Schema.Options[pid], o)
			}
		}
		hidden := spillID(OptionKind, 2000)
		if err := putItem(t.Context(), tx, spillItem(hidden, Option{Property: ids[6], Name: "Unordered", Color: Gray})); err != nil {
			return err
		}
		unknown := spillID(OptionKind, 2001)
		for i := 0; i < 4; i++ {
			cells := map[ID]CellValue{
				ids[0]: {Type: TitleType, Value: "title"},
				ids[1]: {Type: TextType, Value: "line\n[<&>] 한😀\\"},
				ids[2]: {Type: URLType, Value: "https://example.test/a?b=1&c=2"},
				ids[3]: {Type: NumberType, Value: Decimal("-123.5")},
				ids[4]: {Type: DateType, Value: CalendarDate("2026-01-02")},
				ids[7]: {Type: MultiSelectType, Value: []ID{options[ids[7]][1], unknown, options[ids[7]][0]}},
			}
			switch i {
			case 0:
				cells[ids[5]] = CellValue{Type: CheckboxType, Value: true}
				cells[ids[6]] = CellValue{Type: SelectType, Value: options[ids[6]][1]}
			case 1:
				cells[ids[5]] = CellValue{Type: CheckboxType, Value: false}
				cells[ids[6]] = CellValue{Type: SelectType, Value: hidden}
			case 2:
				cells[ids[6]] = CellValue{Type: SelectType, Value: unknown}
			case 3:
				cells[ids[6]] = CellValue{Type: SelectType}
				cells[ids[7]] = CellValue{Type: MultiSelectType, Value: []ID{}}
			}
			rid := spillID(RowKind, uint64(1000+i))
			r := Row{Database: db, Position: fmt.Sprintf("A%d", i), Cells: cells}
			if err := putItem(t.Context(), tx, spillItem(rid, r)); err != nil {
				return err
			}
			snapshot.Rows = append(snapshot.Rows, RowValues{Row: rid, Position: r.Position, Cells: cells})
		}
		var err error
		snapshot.Database, err = readSelected(t.Context(), tx, db, nil)
		if err != nil {
			return err
		}
		page, err = readSelected(t.Context(), tx, f.page.ID, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want, _, wantWarnings := ExportMarkdown(page, []Item{block}, map[ID]DatabaseSnapshot{db: snapshot})
	for _, limit := range []int{1, 2000} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			body, _ := liveReadQuery(t, f.in, map[string]any{"page": f.page.ID, "format": "markdown", "limit": limit}, false)
			got, _, warnings := continueMarkdownRead(t, f.in, body)
			if strings.TrimSpace(got) != strings.TrimSpace(want) {
				t.Fatalf("cell rendering differs:\ngot:\n%s\nwant:\n%s", got, want)
			}
			if !reflect.DeepEqual(warnings, wantWarnings) {
				t.Fatalf("warnings=%+v want=%+v", warnings, wantWarnings)
			}
		})
	}
}
