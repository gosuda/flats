package notebook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func TestReaderHistoryReadCancellation(t *testing.T) {
	data := []byte(`{"format":1}`)
	file := File{Path: "notebook.json", Hash: contentHash(data), Size: int64(len(data))}
	for _, afterRead := range []bool{false, true} {
		t.Run(fmt.Sprintf("after_read=%t", afterRead), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			called := false
			source := RevisionSource{Read: func(context.Context, string) ([]byte, error) {
				called = true
				cancel()
				return data, nil
			}}
			if !afterRead {
				cancel()
			}
			got, err := readRevisionFile(ctx, source, file)
			if !errors.Is(err, context.Canceled) || got != nil {
				t.Fatalf("canceled revision read returned data=%q err=%v", got, err)
			}
			if called != afterRead {
				t.Fatalf("source called=%t, want %t", called, afterRead)
			}
		})
	}
}

func TestReaderHistoryChargesEachImportedRecord(t *testing.T) {
	page, database := spillID(PageKind, 1), spillID(DatabaseKind, 1)
	first, second := spillID(RowKind, 1), spillID(RowKind, 2)
	for n := uint64(3); contentBucket(first) != contentBucket(second); n++ {
		second = spillID(RowKind, n)
	}
	marker := []byte(`{"format":1}`)
	data := []byte(fmt.Sprintf(`[{"id":%q,"database":%q,"position":"A","cells":{}},{"id":%q,"database":%q,"position":"B","cells":{}}]`, first, database, second, database))
	blobs := map[string][]byte{contentHash(marker): marker, contentHash(data): data}
	source := RevisionSource{Number: 1, Files: []File{
		{Path: "notebook.json", Hash: contentHash(marker), Size: int64(len(marker))},
		{Path: fmt.Sprintf("pages/%s/databases/%s/rows/%s.json", page, database, contentBucket(first)), Hash: contentHash(data), Size: int64(len(data))},
	}, Read: func(_ context.Context, hash string) ([]byte, error) { return blobs[hash], nil }}
	stop := errors.New("stop after stored record")
	consumed, charged := 0, 0
	store, err := streamRevisionStore(t.Context(), source, nil, func(*sql.Tx, Item) error {
		consumed++
		return nil
	}, func(string) error {
		if consumed == 0 {
			return nil
		}
		charged++
		return stop
	})
	if store != nil {
		_ = store.close()
	}
	if !errors.Is(err, stop) || consumed != 1 || charged != 1 {
		t.Fatalf("record admission: consumed=%d charged=%d err=%v", consumed, charged, err)
	}
}

func TestReaderHistorySchemaTraversalMemory(t *testing.T) {
	for _, size := range []int{1, 4, 16} {
		t.Run(fmt.Sprintf("%dMiB", size), func(t *testing.T) {
			database, block, property := spillID(DatabaseKind, 10000), spillID(BlockKind, 10000), spillID(PropertyKind, 10000)
			option := spillID(OptionKind, 1)
			header := fmt.Sprintf(`{"id":%q,"block":%q,"title":"data","property_order":[%q],"view_order":[],"properties":[{"id":%q,"database":%q,"definition":{"name":"Title","type":"title"},"option_order":[]}],"options":[{"id":%q,"property":%q,"name":"`, database, block, property, property, database, option, property)
			data := []byte(header + strings.Repeat("x", size<<20) + `","color":"gray"}],"views":[]}`)
			store, _, _ := spillSource(t, 0, "")
			stop := errors.New("first schema child")
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			var retained uint64
			err := store.transaction(t.Context(), func(tx *sql.Tx) error {
				return importRevisionFile(t.Context(), tx, revisionPath{kind: "schema", database: database}, data, func(_ *sql.Tx, item Item) error {
					if item.Kind() != PropertyKind {
						t.Fatalf("first schema item = %s", item.Kind())
					}
					runtime.GC()
					var current runtime.MemStats
					runtime.ReadMemStats(&current)
					if current.HeapAlloc > before.HeapAlloc {
						retained = current.HeapAlloc - before.HeapAlloc
					}
					return stop
				})
			})
			runtime.KeepAlive(data)
			if !errors.Is(err, stop) {
				t.Fatalf("schema import: %v", err)
			}
			t.Logf("schema traversal retained Go heap above source baseline: %d bytes", retained)
			if retained > 2<<20 {
				t.Fatalf("schema traversal materialized unvisited children: %d bytes", retained)
			}
		})
	}
}

func TestReaderHistoryRejectsMalformedContainers(t *testing.T) {
	for _, test := range []struct {
		name string
		kind string
		raw  string
	}{
		{"bucket_null", "row", `null`},
		{"bucket_object", "block", `{}`},
		{"bucket_trailing", "row", `[] true`},
		{"bucket_comma", "row", `[,]`},
		{"bucket_trailing_comma", "row", `[0,]`},
		{"bucket_mismatched_close", "row", `[{}]}`},
		{"bucket_unicode_space", "row", "\u00a0[]"},
		{"bucket_unicode_trailer", "row", "[]\u00a0"},
		{"schema_duplicate", "schema", `{"properties":[],"properties":[],"options":[],"views":[]}`},
		{"schema_escaped_duplicate", "schema", `{"properties":[],"propert\u0069es":[],"options":[],"views":[]}`},
		{"schema_trailing_comma", "schema", `{"properties":[],"options":[],"views":[],}`},
		{"schema_null_children", "schema", `{"properties":null,"options":[],"views":[]}`},
		{"schema_missing_children", "schema", `{"options":[],"views":[]}`},
		{"schema_unknown", "schema", `{"properties":[],"options":[],"views":[],"unknown":0}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _, _ := spillSource(t, 0, "")
			err := store.transaction(t.Context(), func(tx *sql.Tx) error {
				return importRevisionFile(t.Context(), tx, revisionPath{kind: test.kind}, []byte(test.raw), nil)
			})
			if err == nil {
				t.Fatal("malformed revision container accepted")
			}
		})
	}
}

func TestReaderHistoryRetainsEscapedContent(t *testing.T) {
	f := newWriterFixture(t)
	text := strings.Repeat("a\"\\\n한😀", 200)
	created := historyEdit(t, f.in, CreateBlockOp{CreateFields: CreateFields{Ref: "block"}, Place: Placement{Page: Target{Ref: &f.page}}, Content: Content{Kind: Code, Code: text}})
	source := historySource(t, f.in, 1)
	store, err := openRevisionStore(t.Context(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	item, err := store.store.loadItem(t.Context(), created.Refs["block"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Data.(Block).Content.Code != text {
		encoded, _ := json.Marshal(item.Data)
		t.Fatalf("historical text changed: %s", encoded)
	}
}
