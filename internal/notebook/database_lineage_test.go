package notebook

import (
	"database/sql"
	"reflect"
	"slices"
	"testing"
)

func TestDatabaseAgentViewRepairs(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"delete property", "convert property", "delete option", "invalid explicit view"} {
		t.Run(name, func(t *testing.T) {
			f := newWriterFixture(t)
			setup, err := f.in.Update(t.Context(), Update{Slug: "notes", OpID: "setup", Ops: []Operation{
				&CreateDatabaseOp{CreateFields: CreateFields{Ref: "db"}, BlockRef: "block", Place: Placement{Page: editTarget(f.page)}},
				&CreatePropertyOp{CreateFields: CreateFields{Ref: "p"}, Database: Target{Tmp: "db"}, Definition: PropertyDef{Name: "Status", Type: SelectType}},
				&CreateOptionOp{CreateFields: CreateFields{Ref: "o"}, Property: Target{Tmp: "p"}, Name: "Open", Color: Gray},
			}}, 0)
			if err != nil {
				t.Fatalf("setup: %#v", err)
			}
			property, option, view, title := setup.Refs["p"].Ref, setup.Refs["o"].Ref, setup.Refs["db.view"].Ref, setup.Refs["db.title"].Ref
			def := schemaTable(title)
			def.Display.Layout, def.Display.GroupBy = "kanban", &Target{Ref: &property}
			def.Display.Widths = nil
			def.Display.Visible = append(def.Display.Visible, editTarget(property))
			def.Filter = []Filter{{Property: editTarget(property), Op: Is, Value: schemaRaw(editTarget(option))}}
			if _, err := f.in.Update(t.Context(), Update{Slug: "notes", OpID: "view", Ops: []Operation{&SetViewOp{View: editTarget(view), Definition: def}}}, 0); err != nil {
				t.Fatalf("view setup: %#v", err)
			}
			var op Operation
			switch name {
			case "delete property":
				op = &DeletePropertyOp{Property: editTarget(property)}
			case "convert property":
				op = &SetPropertyOp{Property: editTarget(property), Definition: PropertyDef{Name: "Status", Type: TextType}}
			case "delete option":
				op = &DeleteOptionOp{Option: editTarget(option)}
			default:
				def.Display.GroupBy = &Target{Ref: &title}
				op = &SetViewOp{View: editTarget(view), Definition: def}
			}
			u := Update{Slug: "notes", OpID: "repair", Ops: []Operation{op}}
			if name == "invalid explicit view" {
				u.Ops = []Operation{&DeleteOptionOp{Option: editTarget(option)}, op}
			}
			beforeOutbox := writerCount(t, f.in, "outbox")
			result, err := f.in.Update(t.Context(), u, 0)
			if name == "invalid explicit view" {
				if err == nil {
					t.Fatal("invalid explicit agent view committed")
				}
				if writerCount(t, f.in, "outbox") != beforeOutbox {
					t.Fatal("refused call created revision")
				}
				if err := f.in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
					record, err := loadRecord(t.Context(), tx, option.ID)
					if err != nil {
						return err
					}
					if !record.Alive {
						t.Error("refused call committed preceding option deletion")
					}
					var notices int
					if err := tx.QueryRowContext(t.Context(), `SELECT count(*) FROM view_notices`).Scan(&notices); err != nil {
						return err
					}
					if notices != 0 {
						t.Error("refused call committed repair notice")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Applied != 1 || !slices.ContainsFunc(result.Warnings, func(w WriteWarning) bool {
				return w.Code == "view_adjusted" && w.Path == string(view.ID) && w.Message != ""
			}) {
				t.Fatalf("repair outcome: %+v", result)
			}
			var saved itemRecord
			notices := 0
			read := func() {
				t.Helper()
				if err := f.in.ReadTx(t.Context(), func(tx *sql.Tx, _ Sequence) error {
					var err error
					saved, err = loadRecord(t.Context(), tx, view.ID)
					if err != nil {
						return err
					}
					return tx.QueryRowContext(t.Context(), `SELECT count(*) FROM view_notices WHERE view_id=?`, view.ID).Scan(&notices)
				}); err != nil {
					t.Fatal(err)
				}
			}
			read()
			got := saved.Item.Data.(View).Definition
			if len(got.Filter) != 0 || (name != "delete option" && (got.Display.Layout != "table" || got.Display.GroupBy != nil)) || (name == "delete property" && slices.Contains(got.Display.Visible, property.ID)) || notices != 1 {
				t.Fatalf("view repair: %+v notices=%d", got, notices)
			}
			if writerCount(t, f.in, "outbox") != beforeOutbox+1 {
				t.Fatal("repair did not produce exactly one revision")
			}
			replay, err := f.in.Update(t.Context(), u, 0)
			if err != nil || !replay.Replay || replay.State != result.State || !reflect.DeepEqual(replay.Warnings, result.Warnings) {
				t.Fatalf("replay: %+v %v", replay, err)
			}
			original := saved
			read()
			if !reflect.DeepEqual(saved, original) || notices != 1 || writerCount(t, f.in, "outbox") != beforeOutbox+1 {
				t.Fatal("replay reapplied repair")
			}
		})
	}
}

func TestDatabaseNameSwaps(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"property", "option"} {
		t.Run(kind, func(t *testing.T) {
			f := newSchemaTestStore(t, SelectType)
			x, y := f.optionA, f.optionB
			if kind == "property" {
				x, y = f.title, f.property
			}
			rename := func(ref Ref, name string, compound bool) Operation {
				meta := OperationMeta{}
				if compound {
					unit := "swap"
					meta.UnitID = &unit
				}
				if kind == "property" {
					definition := f.record(ref.ID).Item.Data.(Property).Definition
					definition.Name = name
					return &SetPropertyOp{OperationMeta: meta, Property: editTarget(ref), Definition: definition}
				}
				return &SetOptionOp{OperationMeta: meta, Option: editTarget(ref), Name: name, Color: Gray}
			}
			name := func(ref Ref) string {
				record := f.record(ref.ID)
				if kind == "property" {
					return record.Item.Data.(Property).Definition.Name
				}
				return record.Item.Data.(Option).Name
			}
			beforeX, beforeY := name(x), name(y)
			requireOutcomes(t, f.execute("a", rename(x, "temporary", true), rename(y, beforeX, true), rename(x, beforeY, true)), "applied", "applied", "applied")
			if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
				t.Fatalf("swap undo: %+v", got)
			}
			if name(x) != beforeX || name(y) != beforeY {
				t.Fatal("swap undo did not restore names")
			}
			if got := f.undo("a", true); got.Applied != 1 || got.Skipped != 0 {
				t.Fatalf("swap redo: %+v", got)
			}
			if name(x) != beforeY || name(y) != beforeX {
				t.Fatal("swap redo did not restore swapped names")
			}
		})
	}
}

func TestDatabaseOutsideNameConflict(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"property", "option"} {
		t.Run(kind, func(t *testing.T) {
			f := newSchemaTestStore(t, SelectType)
			x, y := f.optionA, f.optionB
			rename := func(ref Ref, name string) Operation {
				return &SetOptionOp{Option: editTarget(ref), Name: name, Color: Gray}
			}
			original := "A"
			if kind == "property" {
				x, y = f.title, f.property
				original = "Name"
				rename = func(ref Ref, name string) Operation {
					definition := f.record(ref.ID).Item.Data.(Property).Definition
					definition.Name = name
					return &SetPropertyOp{Property: editTarget(ref), Definition: definition}
				}
			}
			f.undoTestStore.title("a", vocabID(PageKind, 1), "earlier")
			requireOutcomes(t, f.execute("a", rename(x, "Z")), "applied")
			requireOutcomes(t, f.execute("b", rename(y, original)), "applied")
			got := f.undo("a", false)
			if got.Structure != 1 || got.Skipped != 1 || len(got.Notices) == 0 {
				t.Fatalf("outside name conflict: %+v", got)
			}
			if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
				t.Fatalf("stack wedged after conflict: %+v", got)
			}
		})
	}
}

func TestDatabaseCreateRowLineage(t *testing.T) {
	t.Parallel()
	for _, outside := range []string{"none", "own title", "later write", "outside row", "ABA"} {
		t.Run(outside, func(t *testing.T) {
			f := newUndoTestStore(t)
			dbID, blockID, titleID, viewID := vocabID(DatabaseKind, 90), vocabID(BlockKind, 90), vocabID(PropertyKind, 90), vocabID(ViewKind, 90)
			created := f.execute("a", &CreateDatabaseOp{CreateFields: CreateFields{Ref: "db", ProposedID: &dbID}, BlockRef: "block", BlockProposedID: &blockID, TitlePropertyProposedID: &titleID, ViewProposedID: &viewID, Place: Placement{Page: editTarget(f.record(vocabID(PageKind, 1)).Item.Ref)}})
			database := created.Refs["db"]
			rowID := vocabID(RowKind, 90)
			row := f.execute("a", &CreateRowOp{CreateFields: CreateFields{Ref: "row", ProposedID: &rowID}, Database: editTarget(database), Cells: []CellInput{}}).Refs["row"]
			if outside == "later write" || outside == "ABA" {
				requireOutcomes(t, f.execute("b", &SetDatabaseTitleOp{Database: editTarget(database), Title: "remote"}), "applied")
			}
			if outside == "ABA" || outside == "own title" {
				requireOutcomes(t, f.execute("a", &SetDatabaseTitleOp{Database: editTarget(database), Title: "local"}), "applied")
				if got := f.undo("a", false); got.Applied != 1 {
					t.Fatalf("undo local title: %+v", got)
				}
			}
			if outside == "outside row" {
				outsideID := vocabID(RowKind, 91)
				f.execute("b", &CreateRowOp{CreateFields: CreateFields{Ref: "outside", ProposedID: &outsideID}, Database: editTarget(database), Cells: []CellInput{}})
			}
			if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 || f.record(row.ID).Alive {
				t.Fatalf("row undo: %+v", got)
			}
			got := f.undo("a", false)
			if outside == "none" || outside == "own title" {
				if got.Applied != 1 || got.Skipped != 0 || f.record(database.ID).Alive || f.record(created.Refs["block"].ID).Alive {
					t.Fatalf("database creation undo: %+v", got)
				}
				for range 2 {
					if got := f.undo("a", true); got.Applied != 1 || got.Skipped != 0 {
						t.Fatalf("creation redo: %+v", got)
					}
				}
				if !f.record(database.ID).Alive || !f.record(row.ID).Alive {
					t.Fatal("redo did not revive database and row")
				}
				for range 2 {
					if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
						t.Fatalf("creation second undo: %+v", got)
					}
				}
			} else if got.LaterEdits != 1 || got.Skipped != 1 || !f.record(database.ID).Alive {
				t.Fatalf("unsafe database deletion: %+v", got)
			}
		})
	}
}

func TestDatabasePageCreationAggregateLineage(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"create database", "copy database", "compound block and database"} {
		t.Run(action, func(t *testing.T) {
			f := newSchemaTestStore(t, TextType)
			result := f.execute("a", &DuplicatePageOp{Page: editTarget(f.record(vocabID(PageKind, 1)).Item.Ref)})
			var page, block Ref
			for _, copy := range result.Copies {
				if copy.Source.ID == vocabID(PageKind, 1) {
					page = copy.Ref
				}
				if copy.Source.ID == vocabID(BlockKind, 1) {
					block = copy.Ref
				}
			}
			if action == "copy database" {
				f.execute("a", &DuplicateBlocksOp{Blocks: []Target{editTarget(block)}, After: editTarget(block)})
			} else {
				dbID, blockID, titleID, viewID := vocabID(DatabaseKind, 90), vocabID(BlockKind, 90), vocabID(PropertyKind, 90), vocabID(ViewKind, 90)
				create := &CreateDatabaseOp{CreateFields: CreateFields{Ref: "db", ProposedID: &dbID}, BlockRef: "block", BlockProposedID: &blockID, TitlePropertyProposedID: &titleID, ViewProposedID: &viewID, Place: Placement{Page: editTarget(page)}}
				ops := []Operation{create}
				if action == "compound block and database" {
					unit := "combined"
					create.UnitID = &unit
					extra := vocabID(BlockKind, 91)
					ops = []Operation{&CreateBlockOp{OperationMeta: OperationMeta{UnitID: &unit}, CreateFields: CreateFields{Ref: "extra", ProposedID: &extra}, Place: Placement{Page: editTarget(page)}, Content: editText(Paragraph, "extra")}, create}
				}
				f.execute("a", ops...)
			}
			for range 2 {
				if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
					t.Fatalf("creation aggregate undo: %+v", got)
				}
			}
			if f.record(page.ID).Alive {
				t.Fatal("page copy survived its reverted database creation")
			}
		})
	}
}

func TestDatabaseLegacyUndoEntry(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, TextType)
	text := "local"
	requireOutcomes(t, f.execute("a", &SetCellOp{Row: editTarget(f.first), Cell: CellInput{Property: editTarget(f.property), Value: CellInputValue{Text: &text}}}), "applied")
	if err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE undo SET components=json_remove(components,'$.aggregates') WHERE session='a' AND stack='undo'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
		t.Fatalf("legacy cell undo: %+v", got)
	}
	if _, ok := f.record(f.first.ID).Item.Data.(Row).Cells[f.property.ID]; ok {
		t.Fatal("legacy undo retained edited cell")
	}
}

func TestDatabasePageTitleAndAggregateLineage(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, TextType)
	result := f.execute("a", &DuplicatePageOp{Page: editTarget(f.record(vocabID(PageKind, 1)).Item.Ref)})
	remap := map[ID]Ref{}
	for _, copy := range result.Copies {
		remap[copy.Source.ID] = copy.Ref
	}
	page, row, property := remap[vocabID(PageKind, 1)], remap[f.first.ID], remap[f.property.ID]
	requireOutcomes(t, f.execute("a", &SetPageTitleOp{Page: editTarget(page), Title: "mine"}), "applied")
	text := "cell"
	requireOutcomes(t, f.execute("a", &SetCellOp{Row: editTarget(row), Cell: CellInput{Property: editTarget(property), Value: CellInputValue{Text: &text}}}), "applied")
	for range 3 {
		if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
			t.Fatalf("page/title/cell undo: %+v", got)
		}
	}
	if f.record(page.ID).Alive {
		t.Fatal("page copy survived its own reverted edits")
	}
}

func TestDatabasePartialComponentLineage(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, TextType)
	result := f.execute("a", &DuplicatePageOp{Page: editTarget(f.record(vocabID(PageKind, 1)).Item.Ref)})
	remap := map[ID]Ref{}
	for _, copy := range result.Copies {
		remap[copy.Source.ID] = copy.Ref
	}
	row, property, title := remap[f.first.ID], remap[f.property.ID], remap[f.title.ID]
	f.change("a", editOptions{}, func(e *edit) error {
		text := "local"
		for _, p := range []Ref{property, title} {
			if err := e.apply(&SetCellOp{Row: editTarget(row), Cell: CellInput{Property: editTarget(p), Value: CellInputValue{Text: &text}}}); err != nil {
				return err
			}
		}
		return e.refreshChanges()
	})
	text := "remote"
	requireOutcomes(t, f.execute("b", &SetCellOp{Row: editTarget(row), Cell: CellInput{Property: editTarget(title), Value: CellInputValue{Text: &text}}}), "applied")
	if got := f.undo("a", false); got.Applied != 1 || got.LaterEdits != 1 {
		t.Fatalf("partial cell undo: %+v", got)
	}
	if got := f.undo("a", false); got.Applied != 0 || got.LaterEdits != 1 {
		t.Fatalf("partial inverse blessed copy deletion: %+v", got)
	}
	if f.record(row.ID).Item.Data.(Row).Cells[title.ID].Value != "remote" {
		t.Fatal("lost outside cell")
	}
}

func TestDatabaseCopyCellLineage(t *testing.T) {
	t.Parallel()
	for _, outside := range []string{"local", "page copy", "remote other group", "remote other row", "page copy remote"} {
		t.Run(outside, func(t *testing.T) {
			f := newSchemaTestStore(t, TextType)
			source := f.record(vocabID(BlockKind, 1)).Item.Ref
			var result editResult
			if outside == "page copy" || outside == "page copy remote" {
				result = f.execute("a", &DuplicatePageOp{Page: editTarget(f.record(vocabID(PageKind, 1)).Item.Ref)})
			} else {
				result = f.execute("a", &DuplicateBlocksOp{Blocks: []Target{editTarget(source)}, After: editTarget(source)})
			}
			remap := map[ID]Ref{}
			for _, copy := range result.Copies {
				remap[copy.Source.ID] = copy.Ref
			}
			row, property, title, database := remap[f.first.ID], remap[f.property.ID], remap[f.title.ID], remap[f.database.ID]
			set := func(session string, p Ref, text string) {
				requireOutcomes(t, f.execute(session, &SetCellOp{Row: editTarget(row), Cell: CellInput{Property: editTarget(p), Value: CellInputValue{Text: &text}}}), "applied")
			}
			set("a", property, "first")
			set("a", property, "second")
			if outside == "remote other group" {
				set("b", title, "remote")
			}
			if outside == "remote other row" || outside == "page copy remote" {
				text := "remote"
				requireOutcomes(t, f.execute("b", &SetCellOp{Row: editTarget(remap[f.second.ID]), Cell: CellInput{Property: editTarget(title), Value: CellInputValue{Text: &text}}}), "applied")
			}
			for range 2 {
				if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
					t.Fatalf("cell undo: %+v", got)
				}
			}
			got := f.undo("a", false)
			if outside != "local" && outside != "page copy" {
				if got.LaterEdits != 1 || !f.record(database.ID).Alive {
					t.Fatalf("copy deletion lost remote edit: %+v", got)
				}
				return
			}
			if got.Applied != 1 || got.Skipped != 0 {
				t.Fatalf("copy undo: %+v", got)
			}
			for source, copy := range remap {
				if f.record(copy.ID).Alive || !f.record(source).Alive {
					t.Fatal("copy undo changed source or left copy alive")
				}
			}
			for i := range 3 {
				if got := f.undo("a", true); got.Applied != 1 || got.Skipped != 0 {
					t.Fatalf("copy redo %d: %+v", i, got)
				}
			}
			if got := f.record(row.ID).Item.Data.(Row).Cells[property.ID].Value; got != "second" {
				t.Fatalf("redo cell value: %v", got)
			}
			for range 3 {
				if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
					t.Fatalf("copy second undo: %+v", got)
				}
			}
		})
	}
}
