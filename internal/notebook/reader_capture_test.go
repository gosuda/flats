package notebook

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReaderQueryTextParity(t *testing.T) {
	for _, value := range []string{"", "Straße Σς K", "a\"\\\n<&한😀", "ABABABAC", "a\tb\rc"} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			raw, err := EncodeCellValue(CellValue{Type: TextType, Value: value})
			if err != nil {
				t.Fatal(err)
			}
			var queryError error
			column := dbQueryColumn{typ: TextType}
			stream := column.value(CellValue{Type: TextType, Value: &spillQueryText{raw: func() io.Reader { return strings.NewReader(string(raw)) }, err: &queryError}})
			ordinary := column.value(CellValue{Type: TextType, Value: value})
			if stream.empty != ordinary.empty || column.compare(stream, ordinary) != 0 {
				t.Fatal("streamed text differs from ordinary query value")
			}
			for _, pattern := range []string{"", "Σ", "😀", "abac", "not present"} {
				pattern = dbQueryFold(pattern)
				if dbQueryStreamContains(stream, pattern) != strings.Contains(ordinary.text, pattern) {
					t.Fatalf("contains differs for %q", pattern)
				}
			}
			if queryError != nil {
				t.Fatal(queryError)
			}
		})
	}
}

func TestReaderQueryMemory(t *testing.T) {
	sizes := []int{1, 8}
	if os.Getenv("NOTEBOOK_READER_LARGE") == "1" {
		sizes = append(sizes, 32)
	}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dMiB", size), func(t *testing.T) {
			source, read, db := spillSource(t, 2, "")
			property := spillID(PropertyKind, 1)
			if err := source.transaction(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), `UPDATE cells SET value=json_set(value,'$.value',?)`, strings.Repeat("X", size<<20))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, filtered := range []bool{false, true} {
				t.Run(fmt.Sprintf("filtered=%t", filtered), func(t *testing.T) {
					view := ViewDef{Sort: []Sort{{Property: Target{Ref: &Ref{ID: property, Inc: 7}}, Direction: "asc"}}}
					if filtered {
						view.Filter = []Filter{{Property: Target{Ref: &Ref{ID: property, Inc: 7}}, Op: Contains, Value: []byte(`"x"`)}}
					}
					manager := spillManager(t, 1<<30, newSpillClock())
					var heap readerHeapWriter
					manager.wrapWriter = func(out io.Writer) io.Writer {
						runtime.GC()
						var stats runtime.MemStats
						runtime.ReadMemStats(&stats)
						heap = readerHeapWriter{out: out, base: stats.HeapAlloc}
						return &heap
					}
					captured, err := manager.Capture(t.Context(), read, Scope{Database: db, View: &view}, OrderRows)
					if err != nil {
						t.Fatal(err)
					}
					defer captured.Close()
					t.Logf("sampled capture heap: %d", heap.peak)
					if heap.peak > 6<<20 {
						t.Fatalf("capture materializes retained query cells: %d", heap.peak)
					}
					c, closeFiles, err := agentFiles(captured)
					if err != nil {
						t.Fatal(err)
					}
					defer closeFiles()
					a, err := c.at(true, 0)
					if err != nil {
						t.Fatal(err)
					}
					id, err := readRecordID(io.NewSectionReader(c.s.data, a.start, a.length), false)
					if err != nil || id != spillID(RowKind, 2) {
						t.Fatalf("query membership/order: %s %v", id, err)
					}
				})
			}
		})
	}
}

func TestReaderSchemaMemory(t *testing.T) {
	source, read, db := spillSource(t, 0, "")
	if err := source.transaction(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE items SET data=json_set(data,'$.title',replace(hex(zeroblob(?)),'00','x')) WHERE id=?`, 8<<20, db)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	manager := spillManager(t, 1<<30, newSpillClock())
	var heap readerHeapWriter
	manager.wrapWriter = func(out io.Writer) io.Writer {
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		heap = readerHeapWriter{out: out, base: stats.HeapAlloc}
		return &heap
	}
	captured, err := manager.Capture(t.Context(), read, Scope{Database: db}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer captured.Close()
	t.Logf("sampled schema heap: %d", heap.peak)
	if heap.peak > 6<<20 {
		t.Fatalf("capture materializes retained schema: %d", heap.peak)
	}
}

type readerWriteFunc func([]byte) (int, error)

func (f readerWriteFunc) Write(p []byte) (int, error) { return f(p) }

func TestReaderReferencesDuringCapture(t *testing.T) {
	_, read, db := spillSource(t, 1, "")
	manager := spillManager(t, 1<<30, newSpillClock())
	indexed := false
	manager.wrapWriter = func(out io.Writer) io.Writer {
		return readerWriteFunc(func(p []byte) (int, error) {
			if bytes.Contains(p, []byte(`"cells"`)) {
				f, err := os.Open(filepath.Join(filepath.Dir(out.(*os.File).Name()), "agent-refs"))
				if err == nil {
					ref, err := lookupReadReference(f, spillID(PropertyKind, 1))
					indexed = err == nil && ref.Inc == 7
					_ = f.Close()
				}
			}
			return out.Write(p)
		})
	}
	captured, err := manager.Capture(t.Context(), read, Scope{Database: db}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer captured.Close()
	if !indexed {
		t.Fatal("schema references were not indexed before capturing rows")
	}
	refs, err := os.Open(filepath.Join(captured.dir, "agent-refs"))
	if err != nil {
		t.Fatalf("capture finished before indexing references: %v", err)
	}
	defer refs.Close()
	ref, err := lookupReadReference(refs, spillID(PropertyKind, 1))
	if err != nil || ref.Inc != 7 {
		t.Fatalf("captured reference: %+v %v", ref, err)
	}
}
