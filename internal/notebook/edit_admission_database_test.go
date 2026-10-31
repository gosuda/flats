package notebook

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func databaseAdmissionAction(t *testing.T, f *schemaTestStore, limits editLimits, fn func(*edit) error) error {
	t.Helper()
	f.seq++
	return f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		before, err := measureEditUsage(t.Context(), tx)
		if err != nil {
			return err
		}
		e := &edit{ctx: t.Context(), tx: tx, seq: f.seq, refs: map[string]Ref{}, changes: map[ID]itemChange{}, deferAdmission: true}
		if err := fn(e); err != nil {
			return err
		}
		if err := e.validateDatabases(); err != nil {
			return err
		}
		return checkEditUsage(t.Context(), tx, before, &limits)
	})
}

func databaseAdmissionOp(e *edit, op Operation) error {
	handled, err := e.applyDatabase(op)
	if !handled {
		return fmt.Errorf("unhandled database operation %s", op.OperationName())
	}
	return err
}

func databaseAdmissionUsage(t *testing.T, f *schemaTestStore) editUsage {
	t.Helper()
	var usage editUsage
	if err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		var err error
		usage, err = measureEditUsage(t.Context(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return usage
}

func databaseAdmissionCell(row, property Ref, text string) *SetCellOp {
	return &SetCellOp{Row: Target{Ref: &row}, Cell: CellInput{Property: Target{Ref: &property}, Value: CellInputValue{Text: &text}}}
}

func requireDatabaseAdmissionLimit(t *testing.T, err error, metric string) {
	t.Helper()
	var failure *editFailure
	if !errors.As(err, &failure) || failure.Reason != "item_limit" || !strings.Contains(failure.Detail, metric) {
		t.Fatalf("admission error = %v (%+v), want item_limit for %s", err, failure, metric)
	}
}

func TestDatabaseAdmissionCanonicalOwnedBytes(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, MultiSelectType)
	f.cells(CellValue{Type: MultiSelectType, Value: []ID{f.optionA.ID, f.optionB.ID}})
	usage := databaseAdmissionUsage(t, f)
	page := vocabID(PageKind, 1)
	var pageBytes, rowBytes int64
	if err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		sources, err := contentSources(t.Context(), tx)
		if err != nil {
			return err
		}
		for _, source := range sources {
			if source.page != page || strings.HasSuffix(source.path, "/page.json") {
				continue
			}
			data, err := encodeContentSource(t.Context(), tx, source)
			if err != nil {
				return err
			}
			pageBytes += int64(len(data))
			if strings.Contains(source.path, "/rows/") {
				rowBytes += int64(len(data))
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := usage[usageKey{Owner: page, Metric: "page_bytes"}]; got != pageBytes {
		t.Fatalf("page bytes = %d, content encoder = %d", got, pageBytes)
	}
	if got := usage[usageKey{Owner: f.database.ID, Metric: "database_bytes"}]; got != rowBytes {
		t.Fatalf("database bytes = %d, row encoder = %d", got, rowBytes)
	}
	for key, want := range map[usageKey]int64{
		{Metric: "databases"}:                        1,
		{Owner: f.database.ID, Metric: "rows"}:       2,
		{Owner: f.database.ID, Metric: "properties"}: 2,
		{Owner: f.database.ID, Metric: "views"}:      1,
		{Owner: f.property.ID, Metric: "options"}:    2,
	} {
		if usage[key] != want {
			t.Errorf("%+v = %d, want %d", key, usage[key], want)
		}
	}
	if err := databaseAdmissionAction(t, f, defaultEditLimits(), func(e *edit) error {
		return databaseAdmissionOp(e, &SetDatabaseTitleOp{Database: Target{Ref: &f.database}, Title: "Database"})
	}); err != nil {
		t.Fatal(err)
	}
	if after := databaseAdmissionUsage(t, f); !reflect.DeepEqual(after, usage) {
		t.Fatal("runtime version changes affected content usage")
	}
}

func TestDatabaseAdmissionFinalCapsAndRollback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		metric    string
		configure func(*editLimits, *schemaTestStore, editUsage)
		op        func(*schemaTestStore) Operation
	}{
		{"databases", "databases", func(l *editLimits, _ *schemaTestStore, _ editUsage) { l.Databases = 1 }, func(f *schemaTestStore) Operation {
			page := Ref{ID: vocabID(PageKind, 1), Inc: 1}
			return &CreateDatabaseOp{CreateFields: CreateFields{Ref: "other"}, BlockRef: "other.block", Place: Placement{Page: Target{Ref: &page}}, Title: "Other"}
		}},
		{"rows", "rows", func(l *editLimits, _ *schemaTestStore, _ editUsage) { l.RowsPerDatabase = 2 }, func(f *schemaTestStore) Operation {
			return &CreateRowOp{CreateFields: CreateFields{Ref: "row"}, Database: Target{Ref: &f.database}, Cells: []CellInput{}}
		}},
		{"properties", "properties", func(l *editLimits, _ *schemaTestStore, _ editUsage) { l.PropertiesPerDatabase = 2 }, func(f *schemaTestStore) Operation {
			return &CreatePropertyOp{CreateFields: CreateFields{Ref: "property"}, Database: Target{Ref: &f.database}, Definition: PropertyDef{Name: "Extra", Type: TextType}}
		}},
		{"options", "options", func(l *editLimits, _ *schemaTestStore, _ editUsage) { l.OptionsPerProperty = 2 }, func(f *schemaTestStore) Operation {
			return &CreateOptionOp{CreateFields: CreateFields{Ref: "option"}, Property: Target{Ref: &f.property}, Name: "Extra", Color: Gray}
		}},
		{"views", "views", func(l *editLimits, _ *schemaTestStore, _ editUsage) { l.ViewsPerDatabase = 1 }, func(f *schemaTestStore) Operation {
			wire := ViewDef{Name: "Extra", Display: Display{Layout: "table", Visible: []Target{{Ref: &f.title}}, Widths: []Width{}}, Sort: []Sort{}, Filter: []Filter{}}
			return &CreateViewOp{CreateFields: CreateFields{Ref: "view"}, Database: Target{Ref: &f.database}, Definition: wire}
		}},
		{"row bytes", "database_bytes", func(l *editLimits, f *schemaTestStore, u editUsage) {
			l.DatabaseBytes = u[usageKey{Owner: f.database.ID, Metric: "database_bytes"}]
		}, func(f *schemaTestStore) Operation {
			return databaseAdmissionCell(f.first, f.title, "more row bytes")
		}},
		{"owned page bytes", "page_bytes", func(l *editLimits, _ *schemaTestStore, u editUsage) {
			l.PageBytes = u[usageKey{Owner: vocabID(PageKind, 1), Metric: "page_bytes"}]
		}, func(f *schemaTestStore) Operation {
			return &SetOptionOp{Option: Target{Ref: &f.optionA}, Name: "A longer option", Color: Gray}
		}},
		{"cell", "cell:", func(l *editLimits, _ *schemaTestStore, _ editUsage) { l.Cell = 4 }, func(f *schemaTestStore) Operation {
			return databaseAdmissionCell(f.first, f.title, "가나다라마")
		}},
		{"name", "name", func(l *editLimits, _ *schemaTestStore, _ editUsage) { l.Name = 5 }, func(f *schemaTestStore) Operation {
			return &SetOptionOp{Option: Target{Ref: &f.optionA}, Name: "가나다라마바", Color: Gray}
		}},
		{"database title", "title", func(l *editLimits, _ *schemaTestStore, _ editUsage) { l.Title = 8 }, func(f *schemaTestStore) Operation {
			return &SetDatabaseTitleOp{Database: Target{Ref: &f.database}, Title: "123456789"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchemaTestStore(t, SelectType)
			before := databaseAdmissionUsage(t, f)
			limits := defaultEditLimits()
			tc.configure(&limits, f, before)
			op := tc.op(f)
			err := databaseAdmissionAction(t, f, limits, func(e *edit) error { return databaseAdmissionOp(e, op) })
			requireDatabaseAdmissionLimit(t, err, tc.metric)
			if after := databaseAdmissionUsage(t, f); !reflect.DeepEqual(after, before) {
				t.Fatal("refused action changed content usage")
			}
		})
	}
}

func TestDatabaseAdmissionTransientPropertyExcess(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, TextType)
	if err := databaseAdmissionAction(t, f, defaultEditLimits(), func(e *edit) error {
		for i := 2; i < MaxPropertiesPerDatabase; i++ {
			if err := e.createProperty(&CreatePropertyOp{Database: Target{Ref: &f.database}, Definition: PropertyDef{Name: fmt.Sprintf("Extra%d", i), Type: TextType}}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := databaseAdmissionAction(t, f, defaultEditLimits(), func(e *edit) error {
		if err := e.createProperty(&CreatePropertyOp{Database: Target{Ref: &f.database}, Definition: PropertyDef{Name: "Replacement", Type: TextType}}); err != nil {
			return err
		}
		return e.deleteProperty(&DeletePropertyOp{Property: Target{Ref: &f.property}})
	}); err != nil {
		t.Fatalf("legal final property count refused: %v", err)
	}
	if got := databaseAdmissionUsage(t, f)[usageKey{Owner: f.database.ID, Metric: "properties"}]; got != MaxPropertiesPerDatabase {
		t.Fatalf("properties = %d", got)
	}
}

func TestDatabaseAdmissionCellExcessIsPerFieldAndActionStart(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, TextType)
	original := strings.Repeat("가", MaxCellCodePoints+2)
	f.cells(CellValue{Type: TextType, Value: original}, CellValue{Type: TextType, Value: original})
	if err := databaseAdmissionAction(t, f, defaultEditLimits(), func(e *edit) error {
		for _, text := range []string{original[:len(original)-3], original} {
			if err := databaseAdmissionOp(e, databaseAdmissionCell(f.first, f.property, text)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("unchanged action-start excess refused: %v", err)
	}
	err := databaseAdmissionAction(t, f, defaultEditLimits(), func(e *edit) error {
		if err := databaseAdmissionOp(e, databaseAdmissionCell(f.second, f.property, "")); err != nil {
			return err
		}
		return databaseAdmissionOp(e, databaseAdmissionCell(f.first, f.property, original+"가"))
	})
	requireDatabaseAdmissionLimit(t, err, "cell:")
}

func TestDatabaseAdmissionURLBytes(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, URLType)
	text := "https://example.com/" + strings.Repeat("가", MaxURLBytes/3)
	err := databaseAdmissionAction(t, f, defaultEditLimits(), func(e *edit) error {
		return databaseAdmissionOp(e, databaseAdmissionCell(f.first, f.property, text))
	})
	requireDatabaseAdmissionLimit(t, err, "cell_url:")
}

func copyDatabaseForAdmission(e *edit, f *schemaTestStore, page ID) error {
	id, err := NewID(DatabaseKind)
	if err != nil {
		return err
	}
	owner, err := e.newItem(BlockKind, CreateFields{}, Block{Page: page, Position: "C", Content: Content{Kind: InlineDatabase, Database: id}})
	if err != nil {
		return err
	}
	return e.copyDatabase(f.database.ID, owner.Item.ID(), map[ID]ID{f.database.ID: id})
}

func TestDatabaseAdmissionCopiesCannotInheritExcess(t *testing.T) {
	t.Parallel()
	for _, metric := range []string{"databases", "rows", "properties", "options", "views", "database_bytes", "page_bytes"} {
		t.Run(metric, func(t *testing.T) {
			f := newSchemaTestStore(t, SelectType)
			limits := defaultEditLimits()
			usage := databaseAdmissionUsage(t, f)
			page := vocabID(PageKind, 1)
			switch metric {
			case "databases":
				limits.Databases = 1
			case "rows":
				limits.RowsPerDatabase = 1
			case "properties":
				limits.PropertiesPerDatabase = 1
			case "options":
				limits.OptionsPerProperty = 1
			case "views":
				limits.ViewsPerDatabase = 0
			case "database_bytes":
				limits.DatabaseBytes = usage[usageKey{Owner: f.database.ID, Metric: metric}] - 1
			case "page_bytes":
				limits.PageBytes = usage[usageKey{Owner: page, Metric: metric}]
			}
			err := databaseAdmissionAction(t, f, limits, func(e *edit) error { return copyDatabaseForAdmission(e, f, page) })
			requireDatabaseAdmissionLimit(t, err, metric)
			if after := databaseAdmissionUsage(t, f); !reflect.DeepEqual(after, usage) {
				t.Fatal("refused copy changed owned content")
			}
		})
	}
}

func TestDatabaseAdmissionTransientCopyExcess(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, SelectType)
	limits := defaultEditLimits()
	limits.Databases = 1
	before := databaseAdmissionUsage(t, f)
	if err := databaseAdmissionAction(t, f, limits, func(e *edit) error {
		if err := copyDatabaseForAdmission(e, f, vocabID(PageKind, 1)); err != nil {
			return err
		}
		var copy ID
		for _, mapping := range e.result.Copies {
			if mapping.Source.ID == f.database.ID {
				copy = mapping.Ref.ID
			}
		}
		database, err := loadRecord(e.ctx, e.tx, copy)
		if err != nil {
			return err
		}
		block := database.Item.Data.(Database).Block
		if err := e.deleteDatabaseOf(block); err != nil {
			return err
		}
		owner, err := loadRecord(e.ctx, e.tx, block)
		if err != nil {
			return err
		}
		owner.Alive, owner.DeletedAt = false, e.seq
		return e.write(owner)
	}); err != nil {
		t.Fatalf("legal final copied-database count refused: %v", err)
	}
	if after := databaseAdmissionUsage(t, f); !reflect.DeepEqual(after, before) {
		t.Fatal("deleted copy still contributes to owned content")
	}
}

func TestDatabaseAdmissionMoveChecksDestinationPage(t *testing.T) {
	t.Parallel()
	f := newSchemaTestStore(t, TextType)
	usage := databaseAdmissionUsage(t, f)
	page := vocabID(PageKind, 2)
	block := vocabID(BlockKind, 1)
	limits := defaultEditLimits()
	limits.PageBytes = usage[usageKey{Owner: vocabID(PageKind, 1), Metric: "page_bytes"}] - 1
	err := databaseAdmissionAction(t, f, limits, func(e *edit) error {
		record, err := loadRecord(e.ctx, e.tx, block)
		if err != nil {
			return err
		}
		data := record.Item.Data.(Block)
		data.Page = page
		record.Item.Data = data
		return e.write(record, PlaceGroup)
	})
	requireDatabaseAdmissionLimit(t, err, "page_bytes")
	if after := databaseAdmissionUsage(t, f); !reflect.DeepEqual(after, usage) {
		t.Fatal("refused cross-page move changed usage")
	}
}
