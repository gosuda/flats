package notebook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func liveReadFixture(t *testing.T) writerFixture {
	t.Helper()
	f := newWriterFixture(t)
	if f.in.spills == nil {
		f.in.spills = NewSpillManager(filepath.Join(t.TempDir(), "spills"), 0, f.in.now)
	}
	t.Cleanup(func() { f.in.spills.CancelScope(func(Scope) bool { return true }) })
	return f
}

func liveReadQuery(t *testing.T, in *Instance, query any, public bool) ([]byte, readResult) {
	t.Helper()
	raw, err := json.Marshal(query)
	if err != nil {
		t.Fatal(err)
	}
	body, err := in.Read(t.Context(), raw, public)
	if err != nil {
		t.Fatalf("Read(%s): %v", raw, err)
	}
	if len(body) > readPageBytes {
		t.Fatalf("read page exceeds byte target: %d", len(body))
	}
	var result readResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return body, result
}

func liveReadUpdate(t *testing.T, f writerFixture, id string, ops ...Operation) WriteResult {
	t.Helper()
	result, err := f.in.Update(t.Context(), Update{Slug: "notes", OpID: id, Ops: ops}, 0)
	if err != nil {
		t.Fatalf("Update(%s): %v", id, err)
	}
	return result
}

func liveReadError(t *testing.T, err error, code, reason string) {
	t.Helper()
	var problem *WriteError
	if !errors.As(err, &problem) || problem.Code != code || problem.Reason != reason {
		t.Fatalf("error = %v; want %s:%s", err, code, reason)
	}
}

func TestLiveReadTreeSnapshotAndCursorReplay(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	created := liveReadUpdate(t, f, "pages",
		CreatePageOp{CreateFields: CreateFields{Ref: "two"}, Title: "Second"},
		CreatePageOp{CreateFields: CreateFields{Ref: "three"}, Title: "Third"})
	_, first := liveReadQuery(t, f.in, map[string]any{"limit": 1}, false)
	if first.Complete || first.Next == nil || first.Tree == nil || len(*first.Tree) != 1 || first.State == nil || first.State.Seq != created.State.Seq {
		t.Fatalf("first page = %+v", first)
	}
	cursor := *first.Next
	oldBody, second := liveReadQuery(t, f.in, map[string]any{"cursor": cursor}, false)
	if second.State == nil || *second.State != *first.State {
		t.Fatal("snapshot state changed between pages")
	}
	page := created.Refs["two"].Ref
	liveReadUpdate(t, f, "rename", SetPageTitleOp{Page: Target{Ref: &page}, Title: "New title"})
	newBody, _ := liveReadQuery(t, f.in, map[string]any{"cursor": cursor}, false)
	if !bytes.Equal(oldBody, newBody) {
		t.Fatal("cursor replay changed after a live edit")
	}
	count := 1
	for !first.Complete {
		_, first = liveReadQuery(t, f.in, map[string]any{"cursor": *first.Next}, false)
		count += len(*first.Tree)
	}
	if count != 3 || first.Next != nil {
		t.Fatalf("tree traversed %d pages; final next=%v", count, first.Next)
	}
	_, err := f.in.Read(t.Context(), []byte(fmt.Sprintf(`{"cursor":%q,"limit":2}`, cursor)), false)
	liveReadError(t, err, "invalid", "cursor_query")
	position, _, _, err := decodeReadCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	position.Record++
	tampered, err := json.Marshal(position)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(cursor, ".")
	parts[0] = base64.RawURLEncoding.EncodeToString(tampered)
	_, err = f.in.Read(t.Context(), []byte(fmt.Sprintf(`{"cursor":%q}`, strings.Join(parts, "."))), false)
	liveReadError(t, err, "conflict", "read_expired")
}

func TestLiveReadPageAndDatabaseQueries(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	created := liveReadUpdate(t, f, "database",
		CreateDatabaseOp{CreateFields: CreateFields{Ref: "db"}, BlockRef: "owner", Place: Placement{Page: Target{Ref: &f.page}}, Title: "Tasks"},
		CreatePropertyOp{CreateFields: CreateFields{Ref: "status"}, Database: Target{Tmp: "db"}, Definition: PropertyDef{Name: "Status", Type: SelectType}},
		CreateOptionOp{CreateFields: CreateFields{Ref: "done"}, Property: Target{Tmp: "status"}, Name: "Done", Color: "green"},
		CreateRowOp{CreateFields: CreateFields{Ref: "row-b"}, Database: Target{Tmp: "db"}, Cells: []CellInput{
			{Property: Target{Tmp: "db.title"}, Value: CellInputValue{Text: ptrRead("B")}},
			{Property: Target{Tmp: "status"}, Value: CellInputValue{Option: &Target{Tmp: "done"}}},
		}},
		CreateRowOp{CreateFields: CreateFields{Ref: "row-a"}, Database: Target{Tmp: "db"}, Cells: []CellInput{{Property: Target{Tmp: "db.title"}, Value: CellInputValue{Text: ptrRead("A")}}}},
	)
	_, page := liveReadQuery(t, f.in, map[string]any{"page": f.page}, false)
	if !page.Complete || page.Page == nil || len(page.Page.Items) != 7 || page.Rows != nil {
		t.Fatalf("page schema or row isolation: %+v", page)
	}
	db := created.Refs["db"].Ref
	title := created.Refs["db.title"].Ref
	body, result := liveReadQuery(t, f.in, map[string]any{
		"database": db.ID,
		"sort":     []Sort{{Property: Target{Ref: &title}, Direction: "asc"}},
	}, false)
	if !result.Complete || result.Rows == nil || len(*result.Rows) != 2 || result.Schema == nil || len(*result.Schema) != 5 {
		t.Fatalf("database query result: %s", body)
	}
	var row struct {
		Ref        Ref        `json:"ref"`
		Properties map[ID]Ref `json:"property_refs"`
		Options    map[ID]Ref `json:"option_refs"`
	}
	if err := json.Unmarshal((*result.Rows)[0], &row); err != nil {
		t.Fatal(err)
	}
	if row.Ref != created.Refs["row-a"].Ref {
		t.Fatal("ad-hoc sort did not order rows")
	}
	if err := json.Unmarshal((*result.Rows)[1], &row); err != nil {
		t.Fatal(err)
	}
	property, option := created.Refs["status"].Ref, created.Refs["done"].Ref
	if row.Properties[property.ID] != property || row.Options[option.ID] != option {
		t.Fatal("read did not return property and option incarnations")
	}
	status := created.Refs["status"].Ref
	done := created.Refs["done"].Ref
	operand, err := json.Marshal(Target{Ref: &done})
	if err != nil {
		t.Fatal(err)
	}
	_, filtered := liveReadQuery(t, f.in, map[string]any{"database": db, "filter": []Filter{{Property: Target{Ref: &status}, Op: Is, Value: operand}}}, false)
	if len(*filtered.Rows) != 1 {
		t.Fatal("filter did not select exactly the matching row")
	}
	view := created.Refs["db.view"].Ref
	_, saved := liveReadQuery(t, f.in, map[string]any{"database": db, "view": view}, false)
	if len(*saved.Rows) != 2 {
		t.Fatal("saved view lost rows")
	}
	stale := db
	stale.Inc++
	query, err := json.Marshal(map[string]any{"database": stale})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.in.Read(t.Context(), query, false)
	liveReadError(t, err, "conflict", "incarnation")
}

func TestLiveReadPublicProjectionAndCursorIsolation(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	liveReadUpdate(t, f, "public-pages", CreatePageOp{CreateFields: CreateFields{Ref: "second"}, Title: "Second"})
	_, private := liveReadQuery(t, f.in, map[string]any{"limit": 1}, false)
	body, public := liveReadQuery(t, f.in, map[string]any{"limit": 1}, true)
	for _, forbidden := range []string{`"ref":`, `"inc":`, `"ver":`, `"v":`, `"state":`, `"revision":`, `"pending":`} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("public read contains private field %s", forbidden)
		}
	}
	query, err := json.Marshal(map[string]any{"cursor": *private.Next})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.in.Read(t.Context(), query, true)
	liveReadError(t, err, "conflict", "read_expired")
	query, err = json.Marshal(map[string]any{"cursor": *public.Next})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.in.Read(t.Context(), query, false)
	liveReadError(t, err, "conflict", "read_expired")
	_, err = f.in.Read(t.Context(), []byte(`{"op_id":"private","cursor":"mapping"}`), true)
	liveReadError(t, err, "forbidden", "public")
}

func TestLiveReadAgentCapExpiryAndCancellation(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	liveReadUpdate(t, f, "cap-pages", CreatePageOp{CreateFields: CreateFields{Ref: "second"}, Title: "Second"})
	var cursors []string
	for i := 0; i < 5; i++ {
		_, r := liveReadQuery(t, f.in, map[string]any{"limit": 1}, false)
		cursors = append(cursors, *r.Next)
	}
	query, err := json.Marshal(map[string]any{"cursor": cursors[0]})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.in.Read(t.Context(), query, false)
	liveReadError(t, err, "conflict", "read_expired")
	_, _ = liveReadQuery(t, f.in, map[string]any{"cursor": cursors[1]}, false)
	f.clock.Add(61)
	query, err = json.Marshal(map[string]any{"cursor": cursors[4]})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.in.Read(t.Context(), query, false)
	liveReadError(t, err, "conflict", "read_expired")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.in.Read(ctx, []byte(`{}`), false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
	f.in.spills.CancelScope(func(Scope) bool { return true })
	entries, err := os.ReadDir(f.in.spills.dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancellation left spills: %v, %v", entries, err)
	}
}

func TestLiveReadLargeRecordContinuations(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	db := spillID(DatabaseKind, 40)
	block := spillID(BlockKind, 40)
	rowID := spillID(RowKind, 40)
	err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		properties := []ID{}
		cells := map[ID]CellValue{}
		for n := uint64(1); n <= 32; n++ {
			id := spillID(PropertyKind, n)
			properties = append(properties, id)
			if err := putItem(t.Context(), tx, spillItem(id, Property{Database: db, Definition: PropertyDef{Name: fmt.Sprint(n), Type: TextType}, OptionOrder: []ID{}})); err != nil {
				return err
			}
			cells[id] = CellValue{Type: TextType, Value: strings.Repeat("\x01한", 4000)}
		}
		for _, item := range []Item{
			spillItem(block, Block{Page: f.page.ID, Position: "A", Content: Content{Kind: InlineDatabase, Database: db}}),
			spillItem(db, Database{Block: block, Title: "Retained", PropertyOrder: properties, ViewOrder: []ID{}}),
			spillItem(rowID, Row{Database: db, Position: "A", Cells: cells}),
		} {
			if err := putItem(t.Context(), tx, item); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, result := liveReadQuery(t, f.in, map[string]any{"database": db}, false)
	var assembled strings.Builder
	parts := int64(0)
	for !result.Complete {
		_, result = liveReadQuery(t, f.in, map[string]any{"cursor": *result.Next}, false)
		if result.Continuation == nil {
			t.Fatal("oversized row was not explicitly continued")
		}
		c := result.Continuation
		if c.Record != rowID || c.Part != parts || !utf8.ValidString(c.Data) {
			t.Fatalf("invalid continuation identity/order: %+v", c)
		}
		parts++
		assembled.WriteString(c.Data)
		if result.Complete && c.Parts != parts {
			t.Fatalf("continuation total=%d, received=%d", c.Parts, parts)
		}
	}
	if parts < 2 {
		t.Fatal("fixture did not exceed the agent page target")
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal([]byte(assembled.String()), &wire); err != nil {
		t.Fatalf("continued record is not one JSON object: %v", err)
	}
	for _, field := range []string{"database_ref", "property_refs"} {
		if _, ok := wire[field]; !ok {
			t.Fatalf("continued record lacks mutation-ready %s", field)
		}
	}
	for field := range wire {
		if strings.HasSuffix(field, "_ref") || strings.HasSuffix(field, "_refs") {
			delete(wire, field)
		}
	}
	stored, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	item, err := DecodeItem(stored)
	if err != nil || item.ID() != rowID || len(item.Data.(Row).Cells) != 32 {
		t.Fatalf("continued record did not round-trip: %v", err)
	}
}

func TestLiveReadMappingPagingReplayAndExpiry(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	token := "immutable-mapping-token"
	want := make([]MappingEntry, 1001)
	err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO commands(op_id,request_hash,outcome,mapping_cursor,state,import_status) VALUES('many','hash','{}',?,'{}','pending')`, token); err != nil {
			return err
		}
		for i := range want {
			ref := Ref{ID: spillID(BlockKind, uint64(i+1)), Inc: 17}
			want[i] = MappingEntry{Index: i / 3, Tmp: fmt.Sprintf("made-%d", i), Ref: ref, Ver: 17}
			var tmp, source any = want[i].Tmp, nil
			if i%2 == 1 {
				original := Ref{ID: spillID(BlockKind, uint64(3000+i)), Inc: 11}
				want[i].Tmp, want[i].Source = "", &original
				encoded, err := json.Marshal(original)
				if err != nil {
					return err
				}
				tmp, source = nil, string(encoded)
			}
			assigned, err := json.Marshal(ref)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO command_mappings(op_id,offset,operation_index,tmp,source,assigned,version) VALUES('many',?,?,?,?,?,'17')`, i, i/3, tmp, source, string(assigned)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []MappingEntry
	cursor := token
	for {
		query, err := json.Marshal(map[string]any{"op_id": "many", "cursor": cursor})
		if err != nil {
			t.Fatal(err)
		}
		before, err := f.in.Read(t.Context(), query, false)
		if err != nil {
			t.Fatal(err)
		}
		f.clock.Add(int64(time.Minute / time.Second))
		after, err := f.in.Read(t.Context(), query, false)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("mapping replay changed with time: %v", err)
		}
		var result struct {
			Total    int            `json:"total"`
			Entries  []MappingEntry `json:"entries"`
			Next     *string        `json:"next"`
			Complete bool           `json:"complete"`
		}
		if err := json.Unmarshal(before, &result); err != nil {
			t.Fatal(err)
		}
		if result.Total != len(want) || len(result.Entries) > 500 || len(before) > readPageBytes {
			t.Fatal("mapping page bounds or declared total")
		}
		got = append(got, result.Entries...)
		if result.Complete {
			if result.Next != nil {
				t.Fatal("completed mapping has a next cursor")
			}
			break
		}
		cursor = *result.Next
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatal("mapping entries were omitted, reordered, or changed")
	}
	_, err = f.in.Read(t.Context(), []byte(fmt.Sprintf(`{"op_id":"other","cursor":%q}`, token)), false)
	liveReadError(t, err, "conflict", "result_expired")
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM commands WHERE op_id='many'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err = f.in.Read(t.Context(), []byte(fmt.Sprintf(`{"op_id":"many","cursor":%q}`, token)), false)
	liveReadError(t, err, "conflict", "result_expired")
}

func TestLiveReadRejectsInvalidQueries(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	cases := []struct {
		name string
		json string
	}{
		{"unknown", `{"surprise":true}`},
		{"duplicate", `{"limit":1,"limit":2}`},
		{"null", `null`},
		{"zero-limit", `{"limit":0}`},
		{"negative-limit", `{"limit":-1}`},
		{"large-limit", `{"limit":2001}`},
		{"fractional-limit", `{"limit":1.5}`},
		{"bad-page", `{"page":"b_aaaaaaaaaaaaaaaaaaaaaaaaaa"}`},
		{"zero-incarnation", fmt.Sprintf(`{"page":{"id":%q,"inc":"0"}}`, f.page.ID)},
		{"multiple-selectors", fmt.Sprintf(`{"page":%q,"database":"d_aaaaaaaaaaaaaaaaaaaaaaaaaa"}`, f.page.ID)},
		{"sort-without-database", `{"sort":[]}`},
		{"view-and-sort", `{"database":"d_aaaaaaaaaaaaaaaaaaaaaaaaaa","view":"v_aaaaaaaaaaaaaaaaaaaaaaaaaa","sort":[]}`},
		{"empty-cursor", `{"cursor":""}`},
		{"mapping-with-limit", `{"op_id":"many","cursor":"x","limit":1}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.in.Read(t.Context(), []byte(test.json), false)
			var problem *WriteError
			if !errors.As(err, &problem) || problem.Code != "invalid" {
				t.Fatalf("invalid query accepted or wrong error: %v", err)
			}
		})
	}
}

func ptrRead(s string) *string { return &s }
