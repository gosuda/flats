package notebook

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func readerReferenceFile(t *testing.T, refs []Ref) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "refs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	slots := int64(1)
	for slots < int64(len(refs))*2 {
		slots *= 2
	}
	if err := f.Truncate(slots * readRefSlotBytes); err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		hash := sha256.Sum256([]byte(ref.ID))
		at := int64(binary.LittleEndian.Uint64(hash[:8]) & uint64(slots-1))
		var slot [readRefSlotBytes]byte
		for {
			if _, err := f.ReadAt(slot[:], at*readRefSlotBytes); err != nil {
				t.Fatal(err)
			}
			if slot[0] == 0 {
				break
			}
			at = (at + 1) & (slots - 1)
		}
		slot[0] = byte(len(ref.ID))
		copy(slot[1:32], ref.ID)
		binary.LittleEndian.PutUint64(slot[32:40], uint64(ref.Inc))
		if _, err := f.WriteAt(slot[:], at*readRefSlotBytes); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func readerRecordFile(t *testing.T, data []byte) (*os.File, spillNode) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "record"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if _, err := f.Write(append([]byte("prefix"), data...)); err != nil {
		t.Fatal(err)
	}
	return f, spillNode{start: 6, length: int64(len(data))}
}

func TestReaderContinuationMatchesOrdinary(t *testing.T) {
	t.Parallel()
	db := Ref{ID: spillID(DatabaseKind, 501), Inc: 17}
	text := Ref{ID: spillID(PropertyKind, 501), Inc: 19}
	selectProp := Ref{ID: spillID(PropertyKind, 502), Inc: 23}
	multiProp := Ref{ID: spillID(PropertyKind, 503), Inc: 29}
	nullProp := Ref{ID: spillID(PropertyKind, 504), Inc: 31}
	one := Ref{ID: spillID(OptionKind, 501), Inc: 37}
	two := Ref{ID: spillID(OptionKind, 502), Inc: 41}
	three := Ref{ID: spillID(OptionKind, 503), Inc: 43}
	refs := readerReferenceFile(t, []Ref{text, selectProp, multiProp, nullProp, one, two, three})
	meta := readMetadata{Owners: []Ref{db}}
	large := strings.Repeat("한\"\\\n", readPartBytes/8)
	option, _ := json.Marshal(map[string]ID{"option": one.ID})
	options, _ := json.Marshal([]map[string]ID{{"option": two.ID}, {"option": three.ID}})
	filterText, _ := json.Marshal(large)
	table := StoredView{
		Name:    large,
		Display: StoredDisplay{Layout: "table", Visible: []ID{text.ID, selectProp.ID}, Widths: []StoredWidth{{Property: text.ID, PX: 210}}},
		Sort:    []StoredSort{{Property: text.ID, Direction: "desc"}},
		Filter: []StoredFilter{
			{Property: text.ID, Op: Contains, Value: filterText},
			{Property: selectProp.ID, Op: Is, Value: option},
			{Property: selectProp.ID, Op: IsAnyOf, Value: options},
		},
	}
	kanban := table
	kanban.Display = StoredDisplay{Layout: "kanban", Visible: []ID{text.ID, multiProp.ID}, GroupBy: &selectProp.ID, ShowEmpty: true}
	for _, tc := range []struct {
		name string
		item Item
	}{
		{"row", spillItem(spillID(RowKind, 501), Row{Database: db.ID, Position: "A", Cells: map[ID]CellValue{
			text.ID:       {Type: TextType, Value: large},
			selectProp.ID: {Type: SelectType, Value: one.ID},
			multiProp.ID:  {Type: MultiSelectType, Value: []ID{two.ID, three.ID}},
			nullProp.ID:   {Type: SelectType, Value: nil},
		}})},
		{"table", spillItem(spillID(ViewKind, 501), View{Database: db.ID, Definition: table})},
		{"kanban", spillItem(spillID(ViewKind, 502), View{Database: db.ID, Definition: kanban})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := EncodeItem(tc.item)
			if err != nil {
				t.Fatal(err)
			}
			f, node := readerRecordFile(t, data)
			source, converted, incomplete, err := readContinuationSource(refs, meta, f, node)
			if err != nil {
				t.Fatal(err)
			}
			var assembled bytes.Buffer
			for offset := int64(0); offset < converted.length; offset += readPartBytes {
				start, err := readRecordBoundary(source, converted, offset)
				if err != nil {
					t.Fatal(err)
				}
				end, err := readRecordBoundary(source, converted, min(offset+readPartBytes, converted.length))
				if err != nil {
					t.Fatal(err)
				}
				part := make([]byte, end-start)
				if _, err := source.ReadAt(part, start); err != nil {
					t.Fatal(err)
				}
				if !utf8.Valid(part) {
					t.Fatal("continuation split a UTF-8 rune")
				}
				assembled.Write(part)
			}
			want, err := readWireItem(refs, meta, data)
			if err != nil {
				t.Fatal(err)
			}
			var actual, expected any
			if err := json.Unmarshal(assembled.Bytes(), &actual); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(want, &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, expected) {
				t.Fatal("continued record differs from ordinary mutation-ready wire fields")
			}
			if incomplete {
				t.Fatal("converted dependencies reported incomplete")
			}
			for _, offset := range []int64{converted.length - 13, 0, converted.length / 2} {
				part := make([]byte, 64)
				n, err := source.ReadAt(part, offset)
				want := assembled.Bytes()[offset:min(offset+64, converted.length)]
				if !bytes.Equal(part[:n], want) || (errors.Is(err, io.EOF) != (len(want) < len(part))) {
					t.Fatalf("replayed offset %d differs: n=%d err=%v", offset, n, err)
				}
			}
		})
	}
}

func TestReaderOversizedRowReadReferences(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	created := liveReadUpdate(t, f, "continuation-schema",
		CreateDatabaseOp{CreateFields: CreateFields{Ref: "db"}, BlockRef: "block", Place: Placement{Page: Target{Ref: &f.page}}, Title: "Rows"},
		CreatePropertyOp{CreateFields: CreateFields{Ref: "select"}, Database: Target{Tmp: "db"}, Definition: PropertyDef{Name: "Select", Type: SelectType}},
		CreatePropertyOp{CreateFields: CreateFields{Ref: "multi"}, Database: Target{Tmp: "db"}, Definition: PropertyDef{Name: "Multi", Type: MultiSelectType}},
		CreateOptionOp{CreateFields: CreateFields{Ref: "one"}, Property: Target{Tmp: "select"}, Name: "One", Color: "green"},
		CreateOptionOp{CreateFields: CreateFields{Ref: "two"}, Property: Target{Tmp: "multi"}, Name: "Two", Color: "blue"},
		CreateRowOp{CreateFields: CreateFields{Ref: "row"}, Database: Target{Tmp: "db"}, Cells: []CellInput{
			{Property: Target{Tmp: "db.title"}, Value: CellInputValue{Text: ptrRead("Seed")}},
			{Property: Target{Tmp: "select"}, Value: CellInputValue{Option: &Target{Tmp: "one"}}},
			{Property: Target{Tmp: "multi"}, Value: CellInputValue{Options: []Target{{Tmp: "two"}}}},
		}},
	)
	row, title := created.Refs["row"].Ref, created.Refs["db.title"].Ref
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE cells SET value=json_set(value,'$.value',replace(hex(zeroblob(?)),'00','x')) WHERE row_id=? AND property_id=?`, readPageBytes, row.ID, title.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, result := liveReadQuery(t, f.in, map[string]any{"database": created.Refs["db"].Ref}, false)
	var assembled strings.Builder
	parts := int64(0)
	for {
		for _, warning := range result.Warnings {
			if warning.Code == "continuation_references" {
				t.Fatal("oversized record still needs manual dependency repair")
			}
		}
		if c := result.Continuation; c != nil {
			if c.Record != row.ID || c.Part != parts {
				t.Fatal("continuation identity or order changed")
			}
			assembled.WriteString(c.Data)
			parts++
			if result.Complete && parts != c.Parts {
				t.Fatal("continuation count changed")
			}
		}
		if result.Complete {
			break
		}
		before, next := liveReadQuery(t, f.in, map[string]any{"cursor": *result.Next}, false)
		after, _ := liveReadQuery(t, f.in, map[string]any{"cursor": *result.Next}, false)
		if !bytes.Equal(before, after) {
			t.Fatal("cursor replay changed converted bytes")
		}
		result = next
	}
	var got struct {
		Ref        Ref        `json:"ref"`
		Properties map[ID]Ref `json:"property_refs"`
		Options    map[ID]Ref `json:"option_refs"`
	}
	if err := json.Unmarshal([]byte(assembled.String()), &got); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"db.title", "select", "multi"} {
		ref := created.Refs[name].Ref
		if got.Properties[ref.ID] != ref {
			t.Fatalf("missing property reference %s", name)
		}
	}
	for _, name := range []string{"one", "two"} {
		ref := created.Refs[name].Ref
		if got.Options[ref.ID] != ref {
			t.Fatalf("missing option reference %s", name)
		}
	}
	option := got.Options[created.Refs["one"].Ref.ID]
	property := got.Properties[created.Refs["select"].Ref.ID]
	liveReadUpdate(t, f, "use-read-references", SetCellOp{Row: Target{Ref: &got.Ref}, Cell: CellInput{Property: Target{Ref: &property}, Value: CellInputValue{Option: &Target{Ref: &option}}}})
}

func TestReaderOversizedViewReadReferences(t *testing.T) {
	t.Parallel()
	f := liveReadFixture(t)
	created := liveReadUpdate(t, f, "continuation-view",
		CreateDatabaseOp{CreateFields: CreateFields{Ref: "db"}, BlockRef: "block", Place: Placement{Page: Target{Ref: &f.page}}, Title: "Views"},
		CreatePropertyOp{CreateFields: CreateFields{Ref: "select"}, Database: Target{Tmp: "db"}, Definition: PropertyDef{Name: "Select", Type: SelectType}},
		CreateOptionOp{CreateFields: CreateFields{Ref: "one"}, Property: Target{Tmp: "select"}, Name: "One", Color: "green"},
	)
	view, title := created.Refs["db.view"].Ref, created.Refs["db.title"].Ref
	property, option := created.Refs["select"].Ref, created.Refs["one"].Ref
	operand, err := json.Marshal(Target{Ref: &option})
	if err != nil {
		t.Fatal(err)
	}
	liveReadUpdate(t, f, "set-continuation-view", SetViewOp{View: Target{Ref: &view}, Definition: ViewDef{
		Name:    "Board",
		Display: Display{Layout: "kanban", Visible: []Target{{Ref: &title}}, GroupBy: &Target{Ref: &property}, ShowEmpty: true},
		Sort:    []Sort{{Property: Target{Ref: &title}, Direction: "asc"}},
		Filter:  []Filter{{Property: Target{Ref: &property}, Op: Is, Value: operand}},
	}})
	if err := f.in.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE items SET data=json_set(data,'$.definition.name',replace(hex(zeroblob(?)),'00','x')) WHERE id=?`, readPageBytes, view.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, result := liveReadQuery(t, f.in, map[string]any{"database": created.Refs["db"].Ref}, false)
	var assembled strings.Builder
	for {
		if c := result.Continuation; c != nil {
			if c.Record != view.ID {
				t.Fatal("continued a different view")
			}
			assembled.WriteString(c.Data)
		}
		if result.Complete {
			break
		}
		_, result = liveReadQuery(t, f.in, map[string]any{"cursor": *result.Next}, false)
	}
	var got struct {
		Ref        Ref     `json:"ref"`
		Definition ViewDef `json:"definition"`
	}
	if err := json.Unmarshal([]byte(assembled.String()), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Definition.Name) != readPageBytes || got.Definition.Display.GroupBy.Ref == nil || *got.Definition.Display.GroupBy.Ref != property {
		t.Fatal("continued view lost content or grouping reference")
	}
	got.Definition.Name = "Returned view"
	liveReadUpdate(t, f, "use-view-read-references", SetViewOp{View: Target{Ref: &got.Ref}, Definition: got.Definition})
}

func TestReaderContinuationMemory(t *testing.T) {
	db := Ref{ID: spillID(DatabaseKind, 601), Inc: 7}
	property := Ref{ID: spillID(PropertyKind, 601), Inc: 11}
	refs := readerReferenceFile(t, []Ref{property})
	for _, kind := range []Kind{RowKind, ViewKind} {
		t.Run(string(kind), func(t *testing.T) {
			var allocations []uint64
			for _, size := range []int{1 << 20, 16 << 20} {
				f, err := os.Create(filepath.Join(t.TempDir(), "record"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = f.Close() })
				if kind == RowKind {
					fmt.Fprintf(f, `{"kind":"row","ref":{"id":%q,"inc":"7"},"database":%q,"cells":{%q:{"type":"text","value":"`, spillID(RowKind, 601), db.ID, property.ID)
				} else {
					fmt.Fprintf(f, `{"kind":"view","ref":{"id":%q,"inc":"7"},"database":%q,"definition":{"name":"`, spillID(ViewKind, 601), db.ID)
				}
				chunk := []byte(strings.Repeat("x", 32<<10))
				for written := 0; written < size; written += len(chunk) {
					if _, err := f.Write(chunk); err != nil {
						t.Fatal(err)
					}
				}
				if kind == RowKind {
					io.WriteString(f, `"}}}`)
				} else {
					fmt.Fprintf(f, `","display":{"layout":"table","visible":[%q],"widths":[]},"sort":[],"filter":[]}}`, property.ID)
				}
				length, err := f.Seek(0, io.SeekCurrent)
				if err != nil {
					t.Fatal(err)
				}
				part := make([]byte, readPartBytes)
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				source, node, _, err := readContinuationSource(refs, readMetadata{Owners: []Ref{db}}, f, spillNode{length: length})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := source.ReadAt(part, node.length-int64(len(part))); err != nil {
					t.Fatal(err)
				}
				runtime.ReadMemStats(&after)
				allocations = append(allocations, after.TotalAlloc-before.TotalAlloc)
				t.Logf("source=%d allocated=%d", size, allocations[len(allocations)-1])
			}
			if allocations[1] > allocations[0]+2<<20 {
				t.Fatalf("working allocation grew with retained text: %v", allocations)
			}
		})
	}
}

func TestReaderMarkerCodeDeletion(t *testing.T) {
	t.Parallel()
	marker := "<!-- flats:block b_aaaaaaaaaaaaaaaaaaaaaaaaaa -->\n"
	for _, tc := range []struct {
		name   string
		source string
	}{
		{"backticks", "```text\n" + marker + "keep\n```\n"},
		{"tildes", "~~~\n" + marker + "keep\n~~~\n"},
		{"indented", "    " + marker + "    keep\n"},
		{"tab indented", "\t" + marker + "\tkeep\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := strings.Index(tc.source, marker)
			lineStart := strings.LastIndex(tc.source[:at], "\n") + 1
			changed := tc.source[:lineStart] + tc.source[at+len(marker):]
			if markdownNormalized(t, tc.source) == markdownNormalized(t, changed) {
				t.Fatal("lossless oracle accepted deleted marker-looking code")
			}
			if markdownStripMarkers(tc.source) != tc.source {
				t.Fatal("marker stripping changed literal code")
			}
			mixed := marker + "\n" + tc.source + "\n" + marker
			if got := markdownStripMarkers(mixed); got != "\n"+tc.source+"\n" {
				t.Fatalf("generated markers outside code were not stripped: %q", got)
			}
		})
	}
}
