package notebook

import (
	"database/sql"
	"reflect"
	"testing"
)

func TestEditDatabaseFinalAdmission(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, TextType)
	limits := defaultEditLimits()
	limits.PropertiesPerDatabase = 2
	create := &CreatePropertyOp{CreateFields: CreateFields{Ref: "transient"}, Database: editTarget(f.database), Definition: PropertyDef{Name: "Transient", Type: TextType}}
	result, err := f.tryExecute(editOptions{Limits: &limits}, create, &DeletePropertyOp{Property: Target{Tmp: "transient"}})
	if err != nil {
		t.Fatal(err)
	}
	requireOutcomes(t, result, "applied", "applied")
	_, err = f.tryExecute(editOptions{Limits: &limits}, create)
	requireDatabaseAdmissionLimit(t, err, "properties")
	limits.Databases = 1
	owner := f.record(vocabID(BlockKind, 1)).Item.Ref
	result, err = f.tryExecute(editOptions{Limits: &limits}, &DuplicateBlocksOp{Blocks: []Target{editTarget(owner)}, After: editTarget(owner)}, &DeleteBlocksOp{Blocks: []Target{editTarget(owner)}})
	if err != nil {
		t.Fatal(err)
	}
	requireOutcomes(t, result, "applied", "applied")
	if len(result.CopyRoots) != 1 || f.record(owner.ID).Alive || !f.record(result.CopyRoots[0].Ref.ID).Alive {
		t.Fatal("transient copy admission lost final content")
	}
}

func TestEditDatabaseAdjustedUnitRollback(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, SelectType)
	f.must(&DeleteOptionOp{Option: editTarget(f.optionA)})
	unit := "schema-and-cell"
	order := &SetOptionOrderOp{OperationMeta: OperationMeta{UnitID: &unit}, Property: editTarget(f.property), Options: []Target{editTarget(f.optionA), editTarget(f.optionB)}}
	stale := f.first
	stale.Inc++
	result := f.execute("a", order, &SetCellOp{OperationMeta: OperationMeta{UnitID: &unit}, Row: editTarget(stale), Cell: CellInput{Property: editTarget(f.title), Value: CellInputValue{Text: dbText("lost")}}}, &SetDatabaseTitleOp{Database: editTarget(f.database), Title: "independent"})
	requireOutcomes(t, result, "ignored:gone", "ignored:gone", "applied")
	for i, outcome := range result.Results {
		if outcome.Index != i {
			t.Fatalf("outcomes not input ordered: %+v", result.Results)
		}
	}
	if f.count("a", "undo") != 1 {
		t.Fatal("failed unit leaked undo")
	}
	order.OperationMeta = OperationMeta{}
	requireOutcomes(t, f.execute("a", order), "adjusted:schema")
	if _, err := f.tryExecute(editOptions{}, order); err == nil {
		t.Fatal("agent order accepted a gone member")
	}
	definition := schemaTable(f.title)
	definition.Filter = []Filter{{Property: editTarget(f.property), Op: Is, Value: schemaRaw(editTarget(f.optionA))}}
	requireOutcomes(t, f.execute("a", &SetViewOp{View: editTarget(f.view), Definition: definition}), "adjusted:schema")
	if len(f.record(f.view.ID).Item.Data.(View).Definition.Filter) != 0 {
		t.Fatal("view retained gone option")
	}
}

func TestEditDatabaseCopyAndUndo(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"page", "block"} {
		t.Run(kind, func(t *testing.T) {
			f := newSchemaTestStore(t, MultiSelectType)
			f.cells(CellValue{Type: MultiSelectType, Value: []ID{f.optionA.ID, f.optionB.ID}})
			owner := f.record(vocabID(BlockKind, 1)).Item.Ref
			var op Operation = &DuplicateBlocksOp{Blocks: []Target{editTarget(owner)}, After: editTarget(owner)}
			if kind == "page" {
				op = &DuplicatePageOp{Page: editTarget(f.record(vocabID(PageKind, 1)).Item.Ref)}
			}
			result := f.execute("a", op)
			requireOutcomes(t, result, "applied")
			wantCount := 9
			if kind == "page" {
				wantCount++
			}
			if len(result.CopyRoots) != 1 || len(result.Copies) != wantCount {
				t.Fatalf("copy counts: roots=%d items=%d want=%d", len(result.CopyRoots), len(result.Copies), wantCount)
			}
			ids := map[ID]ID{}
			for _, copy := range result.Copies {
				ids[copy.Source.ID] = copy.Ref.ID
			}
			if err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
				e := &edit{ctx: t.Context(), tx: tx}
				for _, copy := range result.Copies {
					source, err := loadRecord(t.Context(), tx, copy.Source.ID)
					if err != nil {
						return err
					}
					copied, err := loadRecord(t.Context(), tx, copy.Ref.ID)
					if err != nil {
						return err
					}
					if source.Item.Kind() == PageKind || source.Item.Kind() == BlockKind {
						continue
					}
					want, err := (databaseRemapper{}).Remap(source.Item.Data, ids)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(copied.Item.Data, want) {
						t.Errorf("copy %s: got %+v want %+v", copy.Source.ID, copied.Item.Data, want)
					}
				}
				if err := e.validateDatabases(); err != nil {
					return err
				}
				summaries, err := copySummaries(t.Context(), tx, result, []Operation{op})
				if err != nil {
					return err
				}
				if len(summaries) != 1 || summaries[0].RootsTotal != 1 || summaries[0].OtherCounts[2] != 1 || summaries[0].OtherCounts[5] != 2 {
					t.Fatalf("copy summary=%+v", summaries)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
				t.Fatalf("copy undo=%+v", got)
			}
			for source, copy := range ids {
				if !f.record(source).Alive || f.record(copy).Alive {
					t.Fatalf("copy undo changed source or left copy: %s", source)
				}
			}
			if got := f.undo("a", true); got.Applied != 1 || got.Skipped != 0 {
				t.Fatalf("copy redo=%+v", got)
			}
		})
	}
}

func TestEditCopyRootsSurviveSourceReparent(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	root := f.block("b", 10, nil, Toggle)
	child := f.block("b", 11, &root.ID, Paragraph)
	other := f.block("b", 12, nil, Toggle)
	ops := []Operation{
		&DuplicateBlocksOp{Blocks: []Target{editTarget(root), editTarget(child)}, After: editTarget(other)},
		&MoveBlocksOp{Blocks: []Target{editTarget(child)}, Place: Placement{Page: editTarget(page)}},
	}
	result := f.execute("a", ops...)
	requireOutcomes(t, result, "applied", "applied")
	if err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		summaries, err := copySummaries(t.Context(), tx, result, ops)
		if err == nil && (len(summaries) != 1 || summaries[0].RootsTotal != 1 || summaries[0].OtherCounts[1] != 1) {
			t.Fatalf("root inferred from later source state: %+v", summaries)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEditMarkdownDispatchDepth(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := f.record(vocabID(PageKind, 1)).Item.Ref
	var parent *ID
	for i := byte(1); i <= 5; i++ {
		ref := f.block("b", i+30, parent, Toggle)
		parent = &ref.ID
	}
	parentRef := f.record(*parent).Item.Ref
	result, err := f.tryExecute(editOptions{}, &MarkdownToBlocksOp{Place: Placement{Page: editTarget(page), Parent: &Target{Ref: &parentRef}}, Markdown: "- one\n  - two\n    - three"})
	if err != nil {
		t.Fatal(err)
	}
	requireOutcomes(t, result, "applied")
	if len(result.Refs) != 3 || len(result.Warnings) != 2 {
		t.Fatalf("markdown refs=%d warnings=%+v", len(result.Refs), result.Warnings)
	}
	for _, ref := range result.Refs {
		if !sameParent(f.record(ref.ID).Item.Data.(Block).Parent, parent) {
			t.Fatal("markdown ignored destination depth")
		}
	}
}
