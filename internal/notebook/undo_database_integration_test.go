package notebook

import (
	"database/sql"
	"reflect"
	"slices"
	"testing"
)

func confirmUndoDatabaseConversion(f *schemaTestStore, op *SetPropertyOp) {
	f.t.Helper()
	if err := f.s.transaction(f.t.Context(), func(tx *sql.Tx) error {
		e := &edit{ctx: f.t.Context(), tx: tx}
		preview, err := e.previewProperty(op)
		if err == nil {
			op.ConfirmedPreview = &preview.Token
		}
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
}

func TestUndoDatabaseIntegrationRecordedStack(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"schema", "row", "cell"} {
		t.Run(name, func(t *testing.T) {
			f := newSchemaTestStore(t, SelectType)
			f.cells(CellValue{Type: SelectType, Value: f.optionA.ID})
			operations := map[string]Operation{
				"schema": &SetPropertyOp{Property: editTarget(f.property), Definition: PropertyDef{Name: "Value", Type: TextType}},
				"row":    &DeleteRowsOp{Rows: []Target{editTarget(f.first)}},
				"cell":   &SetCellOp{Row: editTarget(f.first), Cell: CellInput{Property: editTarget(f.property), Value: CellInputValue{Option: &Target{Ref: &f.optionB}}}},
			}
			if op, ok := operations[name].(*SetPropertyOp); ok {
				confirmUndoDatabaseConversion(f, op)
			}
			f.change("a", editOptions{}, func(e *edit) error {
				if _, err := e.applyDatabase(operations[name]); err != nil {
					return err
				}
				return e.refreshChanges()
			})
			if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
				t.Fatalf("%s undo = %+v", name, got)
			}
			if !f.record(f.first.ID).Alive || f.cell(f.first).Value != f.optionA.ID || f.record(f.property.ID).Item.Data.(Property).Definition.Type != SelectType || !f.record(f.optionA.ID).Alive || !f.record(f.optionB.ID).Alive {
				t.Fatalf("%s undo did not restore database state", name)
			}
			if got := f.undo("a", true); got.Applied != 1 || got.Skipped != 0 {
				t.Fatalf("%s redo = %+v", name, got)
			}
			switch name {
			case "schema":
				if f.record(f.property.ID).Item.Data.(Property).Definition.Type != TextType || f.cell(f.first).Value != "" || f.record(f.optionA.ID).Alive {
					t.Fatal("schema redo did not reapply conversion")
				}
			case "row":
				if f.record(f.first.ID).Alive {
					t.Fatal("row redo did not delete restored row")
				}
			case "cell":
				if f.cell(f.first).Value != f.optionB.ID {
					t.Fatal("cell redo did not reapply assignment")
				}
			}
		})
	}
}

func TestUndoDatabaseIntegrationSchemaComponent(t *testing.T) {
	t.Parallel()
	for _, later := range []bool{false, true} {
		t.Run(map[bool]string{false: "round trip", true: "later cell skips whole schema"}[later], func(t *testing.T) {
			f := newSchemaTestStore(t, SelectType)
			f.cells(CellValue{Type: SelectType, Value: f.optionA.ID}, CellValue{Type: SelectType, Value: f.optionB.ID})
			definition := schemaTable(f.title)
			definition.Filter = []Filter{{Property: editTarget(f.property), Op: Is, Value: schemaRaw(editTarget(f.optionA))}}
			f.must(&SetViewOp{View: editTarget(f.view), Definition: definition})
			beforeView := f.record(f.view.ID).Item.Data
			op := &SetPropertyOp{Property: editTarget(f.property), Definition: PropertyDef{Name: "Value", Type: TextType}}
			confirmUndoDatabaseConversion(f, op)
			result := f.execute("a", op)
			requireOutcomes(t, result, "adjusted:schema")
			if f.cell(f.first).Value != "" || f.record(f.optionA.ID).Alive {
				t.Fatal("conversion did not change cells and options")
			}
			if later {
				requireOutcomes(t, f.execute("b", &SetCellOp{Row: editTarget(f.first), Cell: CellInput{Property: editTarget(f.property), Value: CellInputValue{Text: dbText("later")}}}), "applied")
				got := f.undo("a", false)
				if got.Applied != 0 || got.LaterEdits != 1 || got.Skipped != 1 {
					t.Fatalf("schema guard = %+v", got)
				}
				if f.cell(f.first).Value != "later" || f.record(f.property.ID).Item.Data.(Property).Definition.Type != TextType || f.record(f.optionA.ID).Alive {
					t.Fatal("partially reverted guarded schema")
				}
				return
			}
			if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
				t.Fatalf("schema undo = %+v", got)
			}
			if f.record(f.property.ID).Item.Data.(Property).Definition.Type != SelectType || f.cell(f.first).Value != f.optionA.ID || f.cell(f.second).Value != f.optionB.ID || !f.record(f.optionA.ID).Alive || !f.record(f.optionB.ID).Alive || !reflect.DeepEqual(f.record(f.view.ID).Item.Data, beforeView) {
				t.Fatal("schema undo did not restore options, cells, and view together")
			}
			if got := f.undo("a", true); got.Applied != 1 || got.Skipped != 0 {
				t.Fatalf("schema redo = %+v", got)
			}
			if f.record(f.property.ID).Item.Data.(Property).Definition.Type != TextType || f.cell(f.first).Value != "" || f.record(f.optionA.ID).Alive {
				t.Fatal("schema redo did not restore conversion")
			}
		})
	}
}

func TestUndoDatabaseIntegrationCells(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, TextType)
	set := func(session string, property Ref, value string) {
		t.Helper()
		requireOutcomes(t, f.execute(session, &SetCellOp{Row: editTarget(f.first), Cell: CellInput{Property: editTarget(property), Value: CellInputValue{Text: &value}}}), "applied")
	}
	set("a", f.property, "mine")
	set("b", f.title, "other group")
	if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
		t.Fatalf("cell undo = %+v", got)
	}
	row := f.record(f.first.ID).Item.Data.(Row)
	if _, ok := row.Cells[f.property.ID]; ok || row.Cells[f.title.ID].Value != "other group" {
		t.Fatalf("cell undo overwrote another group: %+v", row.Cells)
	}
	if got := f.undo("a", true); got.Applied != 1 || got.Skipped != 0 || f.cell(f.first).Value != "mine" {
		t.Fatalf("cell redo = %+v", got)
	}
	set("b", f.property, "mine")
	if got := f.undo("a", false); got.Applied != 0 || got.LaterEdits != 1 {
		t.Fatalf("equal write guard = %+v", got)
	}
}

func TestUndoDatabaseIntegrationRows(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, TextType)
	f.cells(CellValue{Type: TextType, Value: "retained"})
	requireOutcomes(t, f.execute("a", &SetCellOp{Row: editTarget(f.first), Cell: CellInput{Property: editTarget(f.property), Value: CellInputValue{Text: dbText("edited")}}}), "applied")
	requireOutcomes(t, f.execute("a", &DeleteRowsOp{Rows: []Target{editTarget(f.first)}}), "applied")
	if f.record(f.first.ID).Alive {
		t.Fatal("row deletion did not apply")
	}
	if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
		t.Fatalf("row undo = %+v", got)
	}
	if row := f.record(f.first.ID); !row.Alive || row.Item.Ref.Inc == f.first.Inc || row.Item.Data.(Row).Cells[f.property.ID].Value != "edited" {
		t.Fatalf("revived row = %+v", row)
	}
	if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 || f.cell(f.first).Value != "retained" {
		t.Fatalf("revived cell lineage = %+v", got)
	}
	if got := f.undo("a", true); got.Applied != 1 || got.Skipped != 0 || f.cell(f.first).Value != "edited" {
		t.Fatalf("cell redo = %+v", got)
	}
	if got := f.undo("a", true); got.Applied != 1 || got.Skipped != 0 || f.record(f.first.ID).Alive {
		t.Fatalf("row redo = %+v", got)
	}
}

func TestUndoDatabaseIntegrationDeletedOwner(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, SelectType)
	f.cells(CellValue{Type: SelectType, Value: f.optionA.ID})
	owner := f.record(vocabID(BlockKind, 1)).Item.Ref
	ids := []ID{owner.ID, f.database.ID, f.title.ID, f.property.ID, f.optionA.ID, f.optionB.ID, f.first.ID, f.second.ID, f.view.ID}
	before := map[ID]ItemData{}
	for _, id := range ids {
		before[id] = f.record(id).Item.Data
	}
	requireOutcomes(t, f.execute("a", &DeleteBlocksOp{Blocks: []Target{editTarget(owner)}}), "applied")
	for _, id := range ids {
		if f.record(id).Alive {
			t.Fatalf("delete left %s alive", id)
		}
	}
	if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
		t.Fatalf("owner undo = %+v", got)
	}
	for _, id := range ids {
		record := f.record(id)
		if !record.Alive || record.Item.Ref.Inc == 1 {
			t.Fatalf("owner undo did not revive %s: %+v", id, record)
		}
		switch data := record.Item.Data.(type) {
		case Block:
			old := before[id].(Block)
			data.Position = old.Position
			record.Item.Data = data
		case Row:
			old := before[id].(Row)
			data.Position = old.Position
			record.Item.Data = data
		}
		if !reflect.DeepEqual(record.Item.Data, before[id]) {
			t.Fatalf("owner undo changed %s data: got %+v want %+v", id, record.Item.Data, before[id])
		}
	}
	if got := f.undo("a", true); got.Applied != 1 || got.Skipped != 0 {
		t.Fatalf("owner redo = %+v", got)
	}
	for _, id := range ids {
		if f.record(id).Alive {
			t.Fatalf("redo left %s alive", id)
		}
	}
}

func TestUndoDatabaseIntegrationNeighborNamespaces(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, TextType)
	other := dbMustApply(f.undoTestStore, false, &CreateDatabaseOp{CreateFields: CreateFields{Ref: "other"}, BlockRef: "owner", Place: Placement{Page: editTarget(Ref{ID: vocabID(PageKind, 2), Inc: 1})}})
	rows := dbMustApply(f.undoTestStore, false,
		&CreateRowOp{CreateFields: CreateFields{Ref: "first"}, Database: editTarget(other.Refs["other"])},
		&CreateRowOp{CreateFields: CreateFields{Ref: "second"}, Database: editTarget(other.Refs["other"])})
	firstOther := rows.Refs["first"]
	f.change("a", editOptions{}, func(e *edit) error {
		for _, op := range []Operation{
			&MoveRowsOp{Rows: []Target{editTarget(f.second)}, Before: &Target{Ref: &f.first}},
			&MoveRowsOp{Rows: []Target{editTarget(rows.Refs["second"])}, Before: &Target{Ref: &firstOther}},
		} {
			if _, err := e.applyDatabase(op); err != nil {
				return err
			}
		}
		return nil
	})
	if got := f.undo("a", false); got.Applied != 2 || got.Skipped != 0 {
		t.Fatalf("row namespace undo = %+v", got)
	}
	if err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		e := &edit{ctx: t.Context(), tx: tx}
		for database, want := range map[ID][]ID{f.database.ID: {f.first.ID, f.second.ID}, other.Refs["other"].ID: {rows.Refs["first"].ID, rows.Refs["second"].ID}} {
			records, err := e.dbRows(database)
			if err != nil {
				return err
			}
			got := make([]ID, len(records))
			for i, record := range records {
				got[i] = record.Item.ID()
			}
			if !slices.Equal(got, want) {
				t.Errorf("database %s rows = %v, want %v", database, got, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
