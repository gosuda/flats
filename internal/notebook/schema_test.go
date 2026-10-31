package notebook

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type schemaTestStore struct {
	*undoTestStore
	database, title, property, view, first, second, optionA, optionB Ref
	lastUndo                                                         undoEntry
}

func newSchemaTestStore(t *testing.T, typ PropertyType) *schemaTestStore {
	t.Helper()
	f := &schemaTestStore{undoTestStore: newUndoTestStore(t)}
	f.database = Ref{ID: vocabID(DatabaseKind, 1), Inc: 1}
	f.title = Ref{ID: vocabID(PropertyKind, 1), Inc: 1}
	f.property = Ref{ID: vocabID(PropertyKind, 2), Inc: 1}
	f.view = Ref{ID: vocabID(ViewKind, 1), Inc: 1}
	f.first = Ref{ID: vocabID(RowKind, 1), Inc: 1}
	f.second = Ref{ID: vocabID(RowKind, 2), Inc: 1}
	f.optionA = Ref{ID: vocabID(OptionKind, 1), Inc: 1}
	f.optionB = Ref{ID: vocabID(OptionKind, 2), Inc: 1}
	block := vocabID(BlockKind, 1)
	options := []ID{}
	if typ == SelectType || typ == MultiSelectType {
		options = []ID{f.optionB.ID, f.optionA.ID}
	}
	data := map[ID]ItemData{
		block:         Block{Page: vocabID(PageKind, 1), Position: "A", Content: Content{Kind: InlineDatabase, Database: f.database.ID}},
		f.database.ID: Database{Block: block, Title: "Database", PropertyOrder: []ID{f.title.ID, f.property.ID}, ViewOrder: []ID{f.view.ID}},
		f.title.ID:    Property{Database: f.database.ID, Definition: PropertyDef{Name: "Name", Type: TitleType}, OptionOrder: []ID{}},
		f.property.ID: Property{Database: f.database.ID, Definition: PropertyDef{Name: "Value", Type: typ}, OptionOrder: options},
		f.first.ID:    Row{Database: f.database.ID, Position: "A", Cells: map[ID]CellValue{}},
		f.second.ID:   Row{Database: f.database.ID, Position: "B", Cells: map[ID]CellValue{}},
		f.view.ID:     View{Database: f.database.ID, Definition: StoredView{Name: "Table", Display: StoredDisplay{Layout: "table", Visible: []ID{f.title.ID}, Widths: []StoredWidth{}}, Sort: []StoredSort{}, Filter: []StoredFilter{}}},
	}
	if len(options) > 0 {
		data[f.optionA.ID] = Option{Property: f.property.ID, Name: "A", Color: Gray}
		data[f.optionB.ID] = Option{Property: f.property.ID, Name: "B", Color: Blue}
	}
	err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		for id, value := range data {
			if err := putItem(t.Context(), tx, Item{Ref: Ref{ID: id, Inc: 1}, Ver: 1, V: map[GroupKey]Version{}, Data: value}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *schemaTestStore) apply(browser bool, op Operation) (editResult, error) {
	f.t.Helper()
	f.seq++
	var result editResult
	err := f.s.transaction(f.t.Context(), func(tx *sql.Tx) error {
		e := &edit{ctx: f.t.Context(), tx: tx, seq: f.seq, browser: browser, session: "a", refs: map[string]Ref{}, changes: map[ID]itemChange{}}
		handled, err := e.applySchema(op)
		if !handled {
			return fmt.Errorf("unhandled %s", op.OperationName())
		}
		if err != nil {
			return err
		}
		if browser {
			if err := e.refreshChanges(); err != nil {
				return err
			}
			entry, err := makeDatabaseUndoEntry(e, e.changes)
			if err != nil {
				return err
			}
			f.lastUndo = entry
		}
		result = e.result
		result.Refs = e.refs
		return nil
	})
	return result, err
}

func (f *schemaTestStore) schemaUndo() undoSummary {
	f.t.Helper()
	f.seq++
	summary := undoSummary{}
	err := f.s.transaction(f.t.Context(), func(tx *sql.Tx) error {
		e := &edit{ctx: f.t.Context(), tx: tx, seq: f.seq, refs: map[string]Ref{}, changes: map[ID]itemChange{}}
		start := map[ID]itemRecord{}
		for _, component := range f.lastUndo.Components {
			for _, part := range component.Parts {
				record, err := loadRecord(e.ctx, tx, part.Written.Item.ID())
				if err != nil {
					return err
				}
				start[part.Written.Item.ID()] = record
			}
		}
		for _, component := range f.lastUndo.Components {
			reason, err := e.databaseUndoGuard(component, start, nil)
			if err != nil {
				return err
			}
			if reason != "" {
				summary.Skipped++
				continue
			}
			if err := e.applyDatabaseUndoComponent(component); err != nil {
				return err
			}
			summary.Applied++
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return summary
}

func (f *schemaTestStore) must(op Operation) editResult {
	f.t.Helper()
	result, err := f.apply(false, op)
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *schemaTestStore) cells(values ...CellValue) {
	f.t.Helper()
	err := f.s.transaction(f.t.Context(), func(tx *sql.Tx) error {
		for i, value := range values {
			id := f.first.ID
			if i == 1 {
				id = f.second.ID
			}
			r, err := loadRecord(f.t.Context(), tx, id)
			if err != nil {
				return err
			}
			row := r.Item.Data.(Row)
			row.Cells[f.property.ID] = value
			r.Item.Data = row
			if err := putRecord(f.t.Context(), tx, r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *schemaTestStore) cell(ref Ref) CellValue {
	return f.record(ref.ID).Item.Data.(Row).Cells[f.property.ID]
}
func (f *schemaTestStore) definition() StoredView {
	return f.record(f.view.ID).Item.Data.(View).Definition
}
func schemaRaw(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}
func schemaTable(title Ref) ViewDef {
	return ViewDef{Name: "Custom", Display: Display{Layout: "table", Visible: []Target{editTarget(title)}, Widths: []Width{}}, Sort: []Sort{}, Filter: []Filter{}}
}

func TestDBSchemaPropertyLifecycleAndNames(t *testing.T) {
	f := newSchemaTestStore(t, TextType)
	result := f.must(&CreatePropertyOp{CreateFields: CreateFields{Ref: "extra"}, Database: editTarget(f.database), Definition: PropertyDef{Name: "value", Type: NumberType}})
	extra := result.Refs["extra"]
	if extra.ID == "" {
		t.Fatal("missing created reference")
	}
	_, err := f.apply(false, &CreatePropertyOp{CreateFields: CreateFields{Ref: "duplicate"}, Database: editTarget(f.database), Definition: PropertyDef{Name: "Value", Type: TextType}})
	var failure *editFailure
	if !errors.As(err, &failure) || failure.Reason != "name" {
		t.Fatalf("duplicate: %v", err)
	}
	f.must(&SetPropertyOp{Property: editTarget(extra), Definition: PropertyDef{Name: "Renamed", Type: NumberType, Format: PercentFormat}})
	order := &SetPropertyOrderOp{Database: editTarget(f.database), Properties: []Target{editTarget(extra), editTarget(f.title), editTarget(f.property)}}
	if _, err := f.apply(false, order); err == nil {
		t.Fatal("agent moved title away from first")
	}
	if _, err := f.apply(true, order); err != nil {
		t.Fatal(err)
	}
	if got := f.record(f.database.ID).Item.Data.(Database).PropertyOrder; !slices.Equal(got, []ID{f.title.ID, extra.ID, f.property.ID}) {
		t.Fatalf("order %v", got)
	}
	f.must(&SetPropertyOrderOp{Database: editTarget(f.database), Properties: []Target{editTarget(extra), editTarget(f.property)}})
	if got := f.record(f.database.ID).Item.Data.(Database).PropertyOrder; !slices.Equal(got, []ID{f.title.ID, extra.ID, f.property.ID}) {
		t.Fatalf("omitted title %v", got)
	}
	f.cells(CellValue{Type: TextType, Value: "remove"})
	f.must(&DeletePropertyOp{Property: editTarget(f.property)})
	if f.record(f.property.ID).Alive {
		t.Fatal("property still alive")
	}
	if _, ok := f.record(f.first.ID).Item.Data.(Row).Cells[f.property.ID]; ok {
		t.Fatal("deleted property's cell retained")
	}
	for _, op := range []Operation{&DeletePropertyOp{Property: editTarget(f.title)}, &SetPropertyOp{Property: editTarget(f.title), Definition: PropertyDef{Name: "Name", Type: TextType}}, &CreatePropertyOp{Database: editTarget(f.database), Definition: PropertyDef{Name: "Other title", Type: TitleType}}} {
		if _, err := f.apply(false, op); err == nil {
			t.Fatalf("accepted %s", op.OperationName())
		}
	}
}

func TestDBSchemaConversionsAllPairs(t *testing.T) {
	t.Parallel()
	types := []PropertyType{TextType, NumberType, SelectType, MultiSelectType, DateType, CheckboxType, URLType}
	for _, from := range types {
		for _, to := range types {
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				f := newSchemaTestStore(t, from)
				values := map[PropertyType]any{TextType: " \t+001.2500e1\r", NumberType: Decimal("12.5"), SelectType: f.optionA.ID, MultiSelectType: []ID{f.optionA.ID, f.optionB.ID}, DateType: CalendarDate("2026-01-02"), CheckboxType: true, URLType: "https://example.com"}
				source := CellValue{Type: from, Value: values[from]}
				f.cells(source)
				f.must(&SetPropertyOp{Property: editTarget(f.property), Definition: PropertyDef{Name: "Value", Type: to}})
				got := f.cell(f.first)
				want := dbEmpty(to)
				switch {
				case from == to:
					want = source
				case from == NumberType && to == TextType:
					want.Value = "12.5"
				case from == TextType && to == NumberType:
					want.Value = Decimal("12.5")
				case from == SelectType && to == MultiSelectType:
					want.Value = []ID{f.optionA.ID}
				case from == MultiSelectType && to == SelectType:
					want.Value = f.optionB.ID
				case from == TextType && to == SelectType:
					id, ok := got.Value.(ID)
					if !ok || f.record(id).Item.Data.(Option).Name != values[from] {
						t.Fatalf("text option: %+v", got)
					}
					want.Value = id
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("got %+v want %+v", got, want)
				}
				if from != to {
					if empty := f.cell(f.second); !reflect.DeepEqual(empty, dbEmpty(to)) {
						t.Fatalf("missing cell became %+v", empty)
					}
				}
				if (from == SelectType || from == MultiSelectType) && to != SelectType && to != MultiSelectType {
					if f.record(f.optionA.ID).Alive || f.record(f.optionB.ID).Alive {
						t.Fatal("old options survived")
					}
				}
				if (from == SelectType || from == MultiSelectType) && (to == SelectType || to == MultiSelectType) {
					if !slices.Equal(f.record(f.property.ID).Item.Data.(Property).OptionOrder, []ID{f.optionB.ID, f.optionA.ID}) {
						t.Fatal("option order changed")
					}
				}
			})
		}
	}
}

func TestDBSchemaTextOptionsAndDecimalFailures(t *testing.T) {
	t.Run("manual order and exact names", func(t *testing.T) {
		f := newSchemaTestStore(t, TextType)
		f.cells(CellValue{Type: TextType, Value: "a"}, CellValue{Type: TextType, Value: "A"})
		f.must(&SetPropertyOp{Property: editTarget(f.property), Definition: PropertyDef{Name: "Value", Type: SelectType}})
		order := f.record(f.property.ID).Item.Data.(Property).OptionOrder
		if len(order) != 2 || f.cell(f.first).Value != order[0] || f.cell(f.second).Value != order[1] {
			t.Fatalf("order %v", order)
		}
		if f.record(order[0]).Item.Data.(Option).Name != "a" || f.record(order[1]).Item.Data.(Option).Name != "A" {
			t.Fatal("names normalized")
		}
	})
	t.Run("deduplicate", func(t *testing.T) {
		f := newSchemaTestStore(t, TextType)
		f.cells(CellValue{Type: TextType, Value: "same"}, CellValue{Type: TextType, Value: "same"})
		f.must(&SetPropertyOp{Property: editTarget(f.property), Definition: PropertyDef{Name: "Value", Type: SelectType}})
		if len(f.record(f.property.ID).Item.Data.(Property).OptionOrder) != 1 || f.cell(f.first).Value != f.cell(f.second).Value {
			t.Fatal("not deduplicated")
		}
	})
	for _, text := range []string{"12 trailing", "NaN", "\u00a012", "1e8001"} {
		t.Run(text, func(t *testing.T) {
			f := newSchemaTestStore(t, TextType)
			f.cells(CellValue{Type: TextType, Value: text})
			f.must(&SetPropertyOp{Property: editTarget(f.property), Definition: PropertyDef{Name: "Value", Type: NumberType}})
			if f.cell(f.first).Value != nil {
				t.Fatal("invalid number preserved")
			}
		})
	}
	t.Run("long option refuses atomically", func(t *testing.T) {
		f := newSchemaTestStore(t, TextType)
		text := strings.Repeat("한", MaxNameCodePoints+1)
		f.cells(CellValue{Type: TextType, Value: text})
		if _, err := f.apply(false, &SetPropertyOp{Property: editTarget(f.property), Definition: PropertyDef{Name: "Value", Type: SelectType}}); err == nil {
			t.Fatal("accepted long option")
		}
		if f.cell(f.first).Value != text || f.record(f.property.ID).Item.Data.(Property).Definition.Type != TextType {
			t.Fatal("refusal mutated")
		}
	})
}

func TestDBSchemaOptionLifecycleAndRepair(t *testing.T) {
	f := newSchemaTestStore(t, MultiSelectType)
	f.cells(CellValue{Type: MultiSelectType, Value: []ID{f.optionA.ID, f.optionB.ID}})
	view := schemaTable(f.title)
	view.Filter = []Filter{{Property: editTarget(f.property), Op: Contains, Value: schemaRaw(editTarget(f.optionA))}}
	f.must(&SetViewOp{View: editTarget(f.view), Definition: view})
	f.must(&SetOptionOp{Option: editTarget(f.optionA), Name: "Renamed", Color: Red})
	if len(f.definition().Filter) != 1 || !slices.Contains(f.cell(f.first).Value.([]ID), f.optionA.ID) {
		t.Fatal("rename changed references")
	}
	created := f.must(&CreateOptionOp{CreateFields: CreateFields{Ref: "new"}, Property: editTarget(f.property), Name: "Third", Color: Green}).Refs["new"]
	f.must(&SetOptionOrderOp{Property: editTarget(f.property), Options: []Target{editTarget(created), editTarget(f.optionA), editTarget(f.optionB)}})
	f.must(&DeleteOptionOp{Option: editTarget(f.optionA)})
	if len(f.definition().Filter) != 0 || f.record(f.optionA.ID).Alive || !slices.Equal(f.cell(f.first).Value.([]ID), []ID{f.optionB.ID}) {
		t.Fatal("option deletion did not repair")
	}
	var count int
	if err := f.s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM view_notices WHERE view_id=?`, f.view.ID).Scan(&count); err != nil || count == 0 {
		t.Fatalf("notices=%d err=%v", count, err)
	}
	if _, err := f.apply(false, &SetOptionOp{Option: editTarget(f.optionA), Name: "Revive", Color: Gray}); err == nil {
		t.Fatal("revived deleted option")
	}
}

func TestDBSchemaViewLifecycleAndRepair(t *testing.T) {
	f := newSchemaTestStore(t, SelectType)
	kanban := ViewDef{Name: "Board", Display: Display{Layout: "kanban", Visible: []Target{editTarget(f.property)}, GroupBy: &Target{Ref: &f.property}, ShowEmpty: true}, Sort: []Sort{{Property: editTarget(f.property), Direction: "asc"}}, Filter: []Filter{{Property: editTarget(f.property), Op: IsAnyOf, Value: schemaRaw([]Target{editTarget(f.optionA), editTarget(f.optionB)})}}}
	created := f.must(&CreateViewOp{CreateFields: CreateFields{Ref: "board"}, Database: editTarget(f.database), Definition: kanban}).Refs["board"]
	f.must(&SetViewOrderOp{Database: editTarget(f.database), Views: []Target{editTarget(created), editTarget(f.view)}})
	f.must(&DeleteOptionOp{Option: editTarget(f.optionA)})
	board := f.record(created.ID).Item.Data.(View).Definition
	if len(board.Filter) != 0 || board.Display.Layout != "kanban" {
		t.Fatal("set operand not removed whole")
	}
	f.must(&SetPropertyOp{Property: editTarget(f.property), Definition: PropertyDef{Name: "Value", Type: NumberType}})
	board = f.record(created.ID).Item.Data.(View).Definition
	if board.Name != "Board" || board.Display.Layout != "table" || board.Display.GroupBy != nil || board.Display.ShowEmpty || !slices.Contains(board.Display.Visible, f.title.ID) || !slices.Contains(board.Display.Visible, f.property.ID) || len(board.Sort) != 1 {
		t.Fatalf("fallback %+v", board)
	}
	table := schemaTable(f.title)
	table.Display.Visible = append(table.Display.Visible, editTarget(f.property))
	table.Display.Widths = []Width{{Property: editTarget(f.property), PX: 300}}
	table.Sort = []Sort{{Property: editTarget(f.property), Direction: "desc"}}
	table.Filter = []Filter{{Property: editTarget(f.property), Op: Greater, Value: schemaRaw("1")}}
	f.must(&SetViewOp{View: editTarget(f.view), Definition: table})
	f.must(&DeletePropertyOp{Property: editTarget(f.property)})
	got := f.definition()
	if len(got.Sort) != 0 || len(got.Filter) != 0 || len(got.Display.Widths) != 0 || !slices.Equal(got.Display.Visible, []ID{f.title.ID}) {
		t.Fatalf("delete repair %+v", got)
	}
	f.must(&DeleteViewOp{View: editTarget(created)})
	if _, err := f.apply(false, &DeleteViewOp{View: editTarget(f.view)}); err == nil {
		t.Fatal("deleted last view")
	}
}

func TestDBSchemaBrowserSanitizationAndAgentRefusal(t *testing.T) {
	f := newSchemaTestStore(t, SelectType)
	view := schemaTable(f.title)
	view.Display.Visible = append(view.Display.Visible, editTarget(f.property))
	view.Filter = []Filter{{Property: editTarget(f.property), Op: Is, Value: schemaRaw(editTarget(f.optionA))}}
	f.must(&DeleteOptionOp{Option: editTarget(f.optionA)})
	if _, err := f.apply(false, &SetViewOp{View: editTarget(f.view), Definition: view}); err == nil {
		t.Fatal("agent accepted dead option")
	}
	if _, err := f.apply(true, &SetViewOp{View: editTarget(f.view), Definition: view}); err != nil {
		t.Fatal(err)
	}
	if len(f.definition().Filter) != 0 || !slices.Contains(f.definition().Display.Visible, f.property.ID) {
		t.Fatal("browser sanitation failed")
	}
	f.must(&DeletePropertyOp{Property: editTarget(f.property)})
	if _, err := f.apply(true, &SetViewOp{View: editTarget(f.view), Definition: view}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.definition().Display.Visible, []ID{f.title.ID}) {
		t.Fatal("dead visible property retained")
	}
	invalid := schemaTable(f.title)
	invalid.Filter = []Filter{{Property: editTarget(f.title), Op: Equal, Value: schemaRaw("2")}}
	if _, err := f.apply(false, &SetViewOp{View: editTarget(f.view), Definition: invalid}); err == nil {
		t.Fatal("agent accepted invalid operator")
	}
	if _, err := f.apply(true, &SetViewOp{View: editTarget(f.view), Definition: invalid}); err != nil {
		t.Fatal(err)
	}
	if len(f.definition().Filter) != 0 {
		t.Fatal("invalid operator retained")
	}
}

func TestDBSchemaConversionConfirmation(t *testing.T) {
	f := newSchemaTestStore(t, TextType)
	f.cells(CellValue{Type: TextType, Value: "not a number"})
	op := &SetPropertyOp{Property: editTarget(f.property), Definition: PropertyDef{Name: "Value", Type: NumberType}}
	_, err := f.apply(true, op)
	var failure *editFailure
	if !errors.As(err, &failure) || failure.Reason != "confirmation" {
		t.Fatalf("preview: %v", err)
	}
	var preview propertyConversionPreview
	if err := json.Unmarshal([]byte(failure.Detail), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.EmptiedValues != 1 || preview.Token == "" || preview.Property != f.property {
		t.Fatalf("preview %+v", preview)
	}
	if f.cell(f.first).Type != TextType {
		t.Fatal("preview mutated")
	}
	f.cells(CellValue{Type: TextType, Value: "42"})
	op.ConfirmedPreview = &preview.Token
	if _, err := f.apply(true, op); !errors.As(err, &failure) || failure.Reason != "confirmation" {
		t.Fatalf("stale preview: %v", err)
	}
	if err := json.Unmarshal([]byte(failure.Detail), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.EmptiedValues != 0 {
		t.Fatalf("new preview %+v", preview)
	}
	op.ConfirmedPreview = &preview.Token
	if _, err := f.apply(true, op); err != nil {
		t.Fatal(err)
	}
	if f.cell(f.first).Value != Decimal("42") {
		t.Fatal("confirmed conversion not applied")
	}
}

func TestDBSchemaLimitsAndFormat(t *testing.T) {
	t.Run("format preserves decimal", func(t *testing.T) {
		f := newSchemaTestStore(t, NumberType)
		f.cells(CellValue{Type: NumberType, Value: Decimal("12345678901234567890.125")})
		f.must(&SetPropertyOp{Property: editTarget(f.property), Definition: PropertyDef{Name: "Value", Type: NumberType, Format: CommaFormat}})
		if f.cell(f.first).Value != Decimal("12345678901234567890.125") {
			t.Fatal("format rounded the number")
		}
	})
	t.Run("option count conversion", func(t *testing.T) {
		f := newSchemaTestStore(t, TextType)
		err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
			for n := byte(1); n <= MaxOptionsPerProperty+1; n++ {
				id := vocabID(RowKind, n)
				row := Row{Database: f.database.ID, Position: fmt.Sprintf("A%03d", n), Cells: map[ID]CellValue{f.property.ID: {Type: TextType, Value: fmt.Sprintf("option %d", n)}}}
				if err := putItem(t.Context(), tx, Item{Ref: Ref{ID: id, Inc: 1}, Ver: 1, V: map[GroupKey]Version{}, Data: row}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.apply(false, &SetPropertyOp{Property: editTarget(f.property), Definition: PropertyDef{Name: "Value", Type: SelectType}}); err == nil {
			t.Fatal("conversion exceeded option count")
		}
		if f.record(f.property.ID).Item.Data.(Property).Definition.Type != TextType {
			t.Fatal("refused conversion mutated schema")
		}
	})
}

func TestDBSchemaOrderSanitation(t *testing.T) {
	f := newSchemaTestStore(t, SelectType)
	f.must(&DeleteOptionOp{Option: editTarget(f.optionA)})
	op := &SetOptionOrderOp{Property: editTarget(f.property), Options: []Target{editTarget(f.optionA)}}
	if _, err := f.apply(false, op); err == nil {
		t.Fatal("agent accepted gone member")
	}
	if _, err := f.apply(true, op); err != nil {
		t.Fatal(err)
	}
	if got := f.record(f.property.ID).Item.Data.(Property).OptionOrder; !slices.Equal(got, []ID{f.optionB.ID}) {
		t.Fatalf("sanitized order %v", got)
	}
	f.must(&SetViewOrderOp{Database: editTarget(f.database), Views: []Target{}})
	if !slices.Equal(f.record(f.database.ID).Item.Data.(Database).ViewOrder, []ID{f.view.ID}) {
		t.Fatal("omitted live view deleted")
	}
}

func TestDBSchemaUndoConversionComponent(t *testing.T) {
	for _, later := range []bool{false, true} {
		t.Run(fmt.Sprint(later), func(t *testing.T) {
			f := newSchemaTestStore(t, SelectType)
			f.cells(CellValue{Type: SelectType, Value: f.optionA.ID})
			op := &SetPropertyOp{Property: editTarget(f.property), Definition: PropertyDef{Name: "Value", Type: NumberType}}
			_, err := f.apply(true, op)
			var failure *editFailure
			if !errors.As(err, &failure) {
				t.Fatal(err)
			}
			var preview propertyConversionPreview
			if err := json.Unmarshal([]byte(failure.Detail), &preview); err != nil {
				t.Fatal(err)
			}
			op.ConfirmedPreview = &preview.Token
			if _, err := f.apply(true, op); err != nil {
				t.Fatal(err)
			}
			if later {
				dbMustApply(f.undoTestStore, false, &SetCellOp{Row: editTarget(f.first), Cell: CellInput{Property: editTarget(f.property), Value: CellInputValue{Text: dbText("8")}}})
			}
			summary := f.schemaUndo()
			p := f.record(f.property.ID).Item.Data.(Property)
			if later {
				if p.Definition.Type != NumberType || f.record(f.optionA.ID).Alive || f.cell(f.first).Value != Decimal("8") || summary.Applied != 0 {
					t.Fatalf("partial undo %+v", summary)
				}
			} else {
				if p.Definition.Type != SelectType || !f.record(f.optionA.ID).Alive || f.cell(f.first).Value != f.optionA.ID {
					t.Fatalf("conversion undo %+v property %+v cell %+v", summary, p, f.cell(f.first))
				}
			}
		})
	}
}
