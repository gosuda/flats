package notebook

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func dbTestApply(f *undoTestStore, browser bool, ops ...Operation) (editResult, error) {
	f.t.Helper()
	f.seq++
	var result editResult
	err := f.s.transaction(f.t.Context(), func(tx *sql.Tx) error {
		e := &edit{ctx: f.t.Context(), tx: tx, seq: f.seq, browser: browser, session: "a", refs: map[string]Ref{}, changes: map[ID]itemChange{}}
		for i, op := range ops {
			e.index = i
			handled, err := e.applyDatabase(op)
			if !handled {
				return fmt.Errorf("unhandled %s", op.OperationName())
			}
			if err != nil {
				return err
			}
		}
		if err := e.validateStructure(); err != nil {
			return err
		}
		if err := e.validateDatabases(); err != nil {
			return err
		}
		if err := e.refreshChanges(); err != nil {
			return err
		}
		result = e.result
		result.Refs = maps.Clone(e.refs)
		result.Records = e.effects
		return nil
	})
	return result, err
}

func dbMustApply(f *undoTestStore, browser bool, ops ...Operation) editResult {
	f.t.Helper()
	result, err := dbTestApply(f, browser, ops...)
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func TestDBCreateDatabase(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	result := dbMustApply(f, false, &CreateDatabaseOp{CreateFields: CreateFields{Ref: "db"}, BlockRef: "block", Place: Placement{Page: editTarget(Ref{ID: vocabID(PageKind, 1), Inc: 1})}, Title: "Tasks"})
	database := f.record(result.Refs["db"].ID).Item.Data.(Database)
	if database.Title != "Tasks" || database.Block != result.Refs["block"].ID || !slices.Equal(database.PropertyOrder, []ID{result.Refs["db.title"].ID}) || !slices.Equal(database.ViewOrder, []ID{result.Refs["db.view"].ID}) {
		t.Fatalf("database %+v", database)
	}
	title := f.record(result.Refs["db.title"].ID).Item.Data.(Property)
	view := f.record(result.Refs["db.view"].ID).Item.Data.(View)
	if title.Definition.Type != TitleType || title.Definition.Name != "Name" || view.Definition.Name != "Table" || !slices.Equal(view.Definition.Display.Visible, database.PropertyOrder) {
		t.Fatalf("title/view %+v %+v", title, view)
	}
	dbMustApply(f, false, &SetDatabaseTitleOp{Database: editTarget(result.Refs["db"]), Title: "Renamed"})
	if f.record(result.Refs["db"].ID).Item.Data.(Database).Title != "Renamed" {
		t.Fatal("title not assigned")
	}
}

func TestDBBrowserProposedIdentities(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	db, block, title, view := vocabID(DatabaseKind, 91), vocabID(BlockKind, 91), vocabID(PropertyKind, 91), vocabID(ViewKind, 91)
	result := dbMustApply(f, true, &CreateDatabaseOp{CreateFields: CreateFields{Ref: "db", ProposedID: &db}, BlockRef: "block", BlockProposedID: &block, TitlePropertyProposedID: &title, ViewProposedID: &view, Place: Placement{Page: editTarget(Ref{ID: vocabID(PageKind, 1), Inc: 1})}})
	for name, want := range map[string]ID{"db": db, "block": block, "db.title": title, "db.view": view} {
		if result.Refs[name].ID != want {
			t.Fatalf("%s %+v", name, result.Refs)
		}
	}
	zero := Ref{ID: db}
	dbMustApply(f, true, &SetDatabaseTitleOp{Database: editTarget(zero), Title: "session identity"})
}

func TestDBRowsCellsAndIncarnations(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, SelectType)
	dbMustApply(f.undoTestStore, false, &SetCellOp{Row: editTarget(f.first), Cell: CellInput{Property: editTarget(f.title), Value: CellInputValue{Text: dbText("first")}}}, &SetCellOp{Row: editTarget(f.first), Cell: CellInput{Property: editTarget(f.property), Value: CellInputValue{Option: &Target{Ref: &f.optionA}}}})
	row := f.record(f.first.ID).Item.Data.(Row)
	if row.Cells[f.title.ID].Value != "first" || row.Cells[f.property.ID].Value != f.optionA.ID {
		t.Fatalf("distinct writes %+v", row.Cells)
	}
	database, page := f.record(f.database.ID), f.record(vocabID(PageKind, 1))
	if database.Item.Ver != Version(f.seq) || page.Item.Ver != Version(f.seq) {
		t.Fatalf("aggregates %v %v", database.Item.Ver, page.Item.Ver)
	}
	f.must(&DeleteOptionOp{Option: editTarget(f.optionA)})
	if _, err := dbTestApply(f.undoTestStore, false, &SetCellOp{Row: editTarget(f.first), Cell: CellInput{Property: editTarget(f.property), Value: CellInputValue{Option: &Target{Ref: &f.optionA}}}}); err == nil {
		t.Fatal("accepted dead option")
	}
	dbMustApply(f.undoTestStore, false, &DeleteRowsOp{Rows: []Target{editTarget(f.first)}})
	if _, err := dbTestApply(f.undoTestStore, false, &SetCellOp{Row: editTarget(f.first), Cell: CellInput{Property: editTarget(f.title), Value: CellInputValue{Text: dbText("stale")}}}); err == nil {
		t.Fatal("accepted deleted row")
	}
	if err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		r, err := loadRecord(t.Context(), tx, f.first.ID)
		if err != nil {
			return err
		}
		r.Alive = true
		r.DeletedAt = 0
		r.Item.Ref.Inc = Incarnation(f.seq + 1)
		r.Item.Ver = Version(f.seq + 1)
		r.Aggregate = Version(f.seq + 1)
		r.Item.V = map[GroupKey]Version{}
		return putRecord(t.Context(), tx, r)
	}); err != nil {
		t.Fatal(err)
	}
	_, err := dbTestApply(f.undoTestStore, false, &SetCellOp{Row: editTarget(f.first), Cell: CellInput{Property: editTarget(f.title), Value: CellInputValue{Text: dbText("stale")}}})
	var failure *editFailure
	if !errors.As(err, &failure) || failure.Reason != "gone" {
		t.Fatalf("reincarnation: %v", err)
	}
}

func dbText(s string) *string { return &s }

func TestDBRowDefaultsAndManualOrder(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, TextType)
	definition := schemaTable(f.title)
	definition.Filter = []Filter{{Property: editTarget(f.property), Op: Is, Value: schemaRaw("match")}}
	f.must(&SetViewOp{View: editTarget(f.view), Definition: definition})
	result := dbMustApply(f.undoTestStore, false, &CreateRowOp{CreateFields: CreateFields{Ref: "row"}, Database: editTarget(f.database), Cells: []CellInput{}, Before: &Target{Ref: &f.second}, View: &Target{Ref: &f.view}})
	created := result.Refs["row"]
	if got := f.record(created.ID).Item.Data.(Row).Cells[f.property.ID].Value; got != "match" {
		t.Fatalf("default %v", got)
	}
	dbMustApply(f.undoTestStore, false, &MoveRowsOp{Rows: []Target{editTarget(f.second), editTarget(created)}, Before: &Target{Ref: &f.first}})
	var rows []itemRecord
	if err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		e := &edit{ctx: t.Context(), tx: tx}
		var err error
		rows, err = e.dbRows(f.database.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := []ID{}
	for _, r := range rows {
		got = append(got, r.Item.ID())
	}
	if !slices.Equal(got, []ID{created.ID, f.second.ID, f.first.ID}) {
		t.Fatalf("manual stable selection %v", got)
	}
	definition.Filter = append(definition.Filter, Filter{Property: editTarget(f.property), Op: Is, Value: schemaRaw("other")})
	f.must(&SetViewOp{View: editTarget(f.view), Definition: definition})
	result = dbMustApply(f.undoTestStore, false, &CreateRowOp{CreateFields: CreateFields{Ref: "conflict"}, Database: editTarget(f.database), Cells: []CellInput{}, View: &Target{Ref: &f.view}})
	if _, ok := f.record(result.Refs["conflict"].ID).Item.Data.(Row).Cells[f.property.ID]; ok {
		t.Fatal("invented contradictory default")
	}
	result = dbMustApply(f.undoTestStore, false, &CreateRowOp{CreateFields: CreateFields{Ref: "explicit"}, Database: editTarget(f.database), Cells: []CellInput{{Property: editTarget(f.property), Value: CellInputValue{Text: dbText("explicit")}}}, View: &Target{Ref: &f.view}})
	if f.record(result.Refs["explicit"].ID).Item.Data.(Row).Cells[f.property.ID].Value != "explicit" {
		t.Fatal("default overwrote explicit")
	}
}

func TestDBConcurrentCells(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, TextType)
	start := make(chan struct{})
	results := make(chan error, 2)
	for property, text := range map[Ref]string{f.title: "title", f.property: "value"} {
		go func() {
			<-start
			results <- f.s.transaction(t.Context(), func(tx *sql.Tx) error {
				f.seq++
				e := &edit{ctx: t.Context(), tx: tx, seq: f.seq, refs: map[string]Ref{}}
				_, err := e.applyDatabase(&SetCellOp{Row: editTarget(f.first), Cell: CellInput{Property: editTarget(property), Value: CellInputValue{Text: &text}}})
				return err
			})
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	row := f.record(f.first.ID).Item.Data.(Row)
	if row.Cells[f.title.ID].Value != "title" || row.Cells[f.property.ID].Value != "value" {
		t.Fatalf("concurrent values %+v", row.Cells)
	}
}

func TestDBRowRekeyPreservesLogicalVersions(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, TextType)
	if err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		record, err := loadRecord(t.Context(), tx, f.first.ID)
		if err != nil {
			return err
		}
		row := record.Item.Data.(Row)
		row.Position = strings.Repeat("0", MaxPositionBytes-1) + "1"
		record.Item.Data = row
		return putRecord(t.Context(), tx, record)
	}); err != nil {
		t.Fatal(err)
	}
	before := f.record(f.first.ID)
	result := dbMustApply(f.undoTestStore, false, &CreateRowOp{CreateFields: CreateFields{Ref: "before"}, Database: editTarget(f.database), Before: &Target{Ref: &f.first}, Cells: []CellInput{}})
	after := f.record(f.first.ID)
	if before.Item.Ver != after.Item.Ver || !reflect.DeepEqual(before.Item.V, after.Item.V) || before.Item.Data.(Row).Position == after.Item.Data.(Row).Position {
		t.Fatal("rekey changed logical version or did not rebalance")
	}
	if f.record(result.Refs["before"].ID).Item.Data.(Row).Position >= after.Item.Data.(Row).Position {
		t.Fatal("new row not before anchor")
	}
}

func TestDBCopyAndCascade(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, SelectType)
	f.cells(CellValue{Type: SelectType, Value: f.optionA.ID})
	definition := schemaTable(f.title)
	definition.Display.Visible = append(definition.Display.Visible, editTarget(f.property))
	definition.Display.Widths = []Width{{Property: editTarget(f.property), PX: 300}}
	definition.Sort = []Sort{{Property: editTarget(f.property), Direction: "desc"}}
	definition.Filter = []Filter{{Property: editTarget(f.property), Op: Is, Value: schemaRaw(editTarget(f.optionA))}}
	f.must(&SetViewOp{View: editTarget(f.view), Definition: definition})
	owner := vocabID(BlockKind, 51)
	if err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		return putItem(t.Context(), tx, Item{Ref: Ref{ID: owner, Inc: 1}, Ver: 1, V: map[GroupKey]Version{}, Data: Block{Page: vocabID(PageKind, 1), Position: "Z", Content: Content{Kind: Divider}}})
	}); err != nil {
		t.Fatal(err)
	}
	remap := map[ID]ID{}
	f.seq++
	if err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		e := &edit{ctx: t.Context(), tx: tx, seq: f.seq, refs: map[string]Ref{}}
		if err := e.copyDatabase(f.database.ID, owner, remap); err != nil {
			return err
		}
		return e.validateDatabases()
	}); err != nil {
		t.Fatal(err)
	}
	if len(remap) != 9 {
		t.Fatalf("mapping %v", remap)
	}
	copy := f.record(remap[f.database.ID]).Item.Data.(Database)
	if copy.Block != owner || !slices.Equal(copy.PropertyOrder, []ID{remap[f.title.ID], remap[f.property.ID]}) {
		t.Fatalf("copy %+v", copy)
	}
	cell := f.record(remap[f.first.ID]).Item.Data.(Row).Cells[remap[f.property.ID]]
	if cell.Value != remap[f.optionA.ID] {
		t.Fatalf("cell %+v", cell)
	}
	view := f.record(remap[f.view.ID]).Item.Data.(View).Definition
	if view.Filter[0].Property != remap[f.property.ID] || view.Display.Widths[0].Property != remap[f.property.ID] || !reflect.DeepEqual(view.Filter[0].Value, schemaRaw(map[string]ID{"option": remap[f.optionA.ID]})) {
		t.Fatalf("view %+v", view)
	}
	f.seq++
	if err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		e := &edit{ctx: t.Context(), tx: tx, seq: f.seq}
		return e.deleteDatabaseOf(owner)
	}); err != nil {
		t.Fatal(err)
	}
	for source, target := range remap {
		if source.Kind() == BlockKind {
			continue
		}
		if f.record(target).Alive {
			t.Fatalf("copy survived cascade %s", target)
		}
	}
	if !f.record(f.database.ID).Alive || !f.record(f.first.ID).Alive {
		t.Fatal("source changed")
	}
}
