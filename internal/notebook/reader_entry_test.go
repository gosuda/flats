package notebook

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// readerHeapCeiling bounds the sampled heap growth of one whole read.
const readerHeapCeiling = 4 << 20

// measureReaderHeap reports the transient sampled heap growth of one whole
// read and, after a final collection, the heap the read still retains. The
// transient peak serves the ceiling check. Growth comparisons use a separate
// measureReaderLiveHeap read, because these transient samples mix collected
// and uncollected regimes and the final retained figure misses capture buffers.
func measureReaderHeap(t *testing.T, in *Instance, query []byte) ([]byte, uint64, uint64, error) {
	t.Helper()
	oldGC := debug.SetGCPercent(20)
	defer debug.SetGCPercent(oldGC)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	peak.Store(before.HeapAlloc)
	sample := func() {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		if stats.HeapAlloc > before.HeapAlloc+readerHeapCeiling {
			// Under host CPU contention the GC CPU limiter can turn off mutator
			// assists, and the heap then outgrows the pacer's goal with garbage.
			// A collection frees that garbage; what the read still holds survives.
			runtime.GC()
			runtime.ReadMemStats(&stats)
		}
		for old := peak.Load(); stats.HeapAlloc > old; old = peak.Load() {
			if peak.CompareAndSwap(old, stats.HeapAlloc) {
				break
			}
		}
	}
	in.spills.wrapWriter = func(out io.Writer) io.Writer {
		return readerWriteFunc(func(p []byte) (int, error) { sample(); return out.Write(p) })
	}
	defer func() { in.spills.wrapWriter = nil }()
	stop, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				sample()
			}
		}
	}()
	body, err := in.Read(t.Context(), query, false)
	sample()
	close(stop)
	<-joined
	// Collect once more for the post-return retention diagnostic, excluding
	// garbage the read no longer holds.
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	var retained uint64
	if after.HeapAlloc > before.HeapAlloc {
		retained = after.HeapAlloc - before.HeapAlloc
	}
	return body, peak.Load() - before.HeapAlloc, retained, err
}

// measureReaderLiveHeap samples collected live heap throughout a fresh read.
// The ticker covers preparation before output; writes add bounded checkpoints.
// It also reports the post-collection baseline the growth is measured from,
// so callers can tell retained heap (baseline shift across reads) apart from
// transient in-read peaks.
func measureReaderLiveHeap(t *testing.T, in *Instance, query []byte) ([]byte, uint64, uint64, error) {
	t.Helper()
	oldGC := debug.SetGCPercent(20)
	defer debug.SetGCPercent(oldGC)
	before := settledReaderHeap()
	t.Logf("collected live-heap baseline=%d", before)
	peak := before
	var mu sync.Mutex
	sample := func() {
		mu.Lock()
		defer mu.Unlock()
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		peak = max(peak, stats.HeapAlloc)
	}
	var written atomic.Uint64
	in.spills.wrapWriter = func(out io.Writer) io.Writer {
		return readerWriteFunc(func(p []byte) (int, error) {
			after := written.Add(uint64(len(p)))
			before := after - uint64(len(p))
			if before == 0 || before/(64<<10) != after/(64<<10) {
				sample()
			}
			return out.Write(p)
		})
	}
	defer func() { in.spills.wrapWriter = nil }()
	stop, joined := make(chan struct{}), make(chan struct{})
	var periodic int
	go func() {
		defer close(joined)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				sample()
				periodic++
			}
		}
	}()
	body, err := in.Read(t.Context(), query, false)
	sample()
	close(stop)
	<-joined
	t.Logf("collected live-heap periodic samples=%d", periodic)
	return body, peak - before, before, err
}

// settledReaderHeap uses a second collection to release pool victims.
func settledReaderHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

func readerQueryFixture(t *testing.T) (writerFixture, WriteResult) {
	t.Helper()
	f := liveReadFixture(t)
	created := liveReadUpdate(t, f, "schema",
		CreateDatabaseOp{CreateFields: CreateFields{Ref: "db"}, BlockRef: "owner", Place: Placement{Page: Target{Ref: &f.page}}, Title: "Tasks"},
		CreatePropertyOp{CreateFields: CreateFields{Ref: "tags"}, Database: Target{Tmp: "db"}, Definition: PropertyDef{Name: "Tags", Type: MultiSelectType}},
		CreateOptionOp{CreateFields: CreateFields{Ref: "first"}, Property: Target{Tmp: "tags"}, Name: "First", Color: "gray"},
		CreateOptionOp{CreateFields: CreateFields{Ref: "second"}, Property: Target{Tmp: "tags"}, Name: "Second", Color: "gray"},
		CreateRowOp{CreateFields: CreateFields{Ref: "big"}, Database: Target{Tmp: "db"}, Cells: []CellInput{}},
		CreateRowOp{CreateFields: CreateFields{Ref: "small"}, Database: Target{Tmp: "db"}, Cells: []CellInput{{Property: Target{Tmp: "tags"}, Value: CellInputValue{Options: []Target{{Tmp: "second"}}}}}},
	)
	return f, created
}

func readerMemorySizes() []int {
	sizes := []int{1 << 20, 3 << 19}
	if os.Getenv("NOTEBOOK_LARGE_MEMORY_TESTS") == "1" {
		sizes = append(sizes, 8<<20, 32<<20)
	}
	return sizes
}

func TestReaderSelectionPreparationMemory(t *testing.T) {
	for _, kind := range []string{"database", "owner", "property", "view"} {
		t.Run(kind, func(t *testing.T) {
			for _, size := range readerMemorySizes() {
				t.Run(fmt.Sprintf("%gMiB", float64(size)/(1<<20)), func(t *testing.T) {
					f, created := readerQueryFixture(t)
					database := created.Refs["db"].Ref
					query := map[string]any{"database": database.ID, "limit": 1}
					id, path := database.ID, "$.title"
					switch kind {
					case "owner":
						child := liveReadUpdate(t, f, "child", CreatePageOp{CreateFields: CreateFields{Ref: "child"}, Parent: &Target{Ref: &f.page}, Title: "Child"})
						id, path = f.page.ID, "$.title"
						query = map[string]any{"page": child.Refs["child"].Ref.ID, "limit": 1}
					case "property":
						property := created.Refs["db.title"].Ref
						id, path = property.ID, "$.definition.name"
						query["sort"] = []Sort{{Property: Target{Ref: &property}, Direction: "asc"}}
					case "view":
						id, path = created.Refs["db.view"].Ref.ID, "$.definition.name"
						query["view"] = id
					}
					if err := f.in.db.transaction(t.Context(), func(tx *sql.Tx) error {
						_, err := tx.ExecContext(t.Context(), `UPDATE items SET data=json_set(data,?,?) WHERE id=?`, path, strings.Repeat("x", size), id)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					raw, _ := json.Marshal(query)
					body, peak, _, err := measureReaderHeap(t, f.in, raw)
					if err != nil {
						t.Fatal(err)
					}
					var result readResult
					if err := json.Unmarshal(body, &result); err != nil {
						t.Fatal(err)
					}
					if kind == "owner" {
						if result.Page == nil || !result.Complete || len(result.Page.Items) != 1 {
							t.Fatalf("missing captured page: %s", body)
						}
						var item struct {
							Parent Ref `json:"parent_ref"`
						}
						if err := json.Unmarshal(result.Page.Items[0], &item); err != nil || item.Parent != f.page {
							t.Fatalf("missing owner reference: %s", body)
						}
					} else if result.Schema == nil || result.Complete || result.Next == nil {
						t.Fatalf("missing captured schema/cursor: %s", body)
					}
					// Bound above the heap immediately before Instance.Read, including
					// preparation, query, capture, reference indexing and delivery.
					t.Logf("whole read sampled heap growth: %d bytes", peak)
					if peak > readerHeapCeiling {
						t.Fatalf("read preparation materialized retained payload: %d bytes", peak)
					}
				})
			}
		})
	}
}

func TestReaderCollectionOrderingParity(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		reverse, descending, filtered bool
		wantBigFirst                  bool
	}{
		{name: "schema_order", wantBigFirst: true},
		{name: "reversed_cell_order", reverse: true, wantBigFirst: true},
		{name: "descending", descending: true},
		{name: "contains", reverse: true, filtered: true, wantBigFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, created := readerQueryFixture(t)
			property := created.Refs["tags"].Ref
			ids := []ID{created.Refs["first"].Ref.ID, created.Refs["second"].Ref.ID}
			if tc.reverse {
				ids[0], ids[1] = ids[1], ids[0]
			}
			cell, err := EncodeCellValue(CellValue{Type: MultiSelectType, Value: ids})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.in.db.transaction(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), `INSERT INTO cells(row_id,property_id,value) VALUES(?,?,?)`, created.Refs["big"].Ref.ID, property.ID, string(cell))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			direction := "asc"
			if tc.descending {
				direction = "desc"
			}
			query := map[string]any{"database": created.Refs["db"].Ref.ID, "sort": []Sort{{Property: Target{Ref: &property}, Direction: direction}}}
			if tc.filtered {
				option := created.Refs["first"].Ref
				operand, _ := json.Marshal(option)
				query["filter"] = []Filter{{Property: Target{Ref: &property}, Op: Contains, Value: operand}}
			}
			_, result := liveReadQuery(t, f.in, query, false)
			wantCount := 2
			if tc.filtered {
				wantCount = 1
			}
			if !result.Complete || result.Rows == nil || len(*result.Rows) != wantCount {
				t.Fatalf("incomplete collection query: %+v", result)
			}
			var first struct {
				Ref Ref `json:"ref"`
			}
			if err := json.Unmarshal((*result.Rows)[0], &first); err != nil {
				t.Fatal(err)
			}
			want := created.Refs["small"].Ref
			if tc.wantBigFirst {
				want = created.Refs["big"].Ref
			}
			if first.Ref != want {
				t.Fatalf("first row=%v, want %v", first.Ref, want)
			}
		})
	}
}

func TestReaderCollectionQueryMemory(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		t.Run(fmt.Sprintf("contains=%t", filtered), func(t *testing.T) {
			for _, size := range readerMemorySizes() {
				t.Run(fmt.Sprintf("%gMiB", float64(size)/(1<<20)), func(t *testing.T) {
					f, created := readerQueryFixture(t)
					property := created.Refs["tags"].Ref
					row := created.Refs["big"].Ref
					var stored strings.Builder
					stored.WriteString(`{"type":"multi_select","value":[`)
					for n := uint64(100); stored.Len() < size; n++ {
						if n > 100 {
							stored.WriteByte(',')
						}
						id := spillID(OptionKind, n)
						if err := id.ValidateKind(OptionKind); err != nil {
							t.Fatal(err)
						}
						fmt.Fprintf(&stored, "%q", id)
					}
					fmt.Fprintf(&stored, ",%q]}", created.Refs["first"].Ref.ID)
					if err := f.in.db.transaction(t.Context(), func(tx *sql.Tx) error {
						_, err := tx.ExecContext(t.Context(), `INSERT INTO cells(row_id,property_id,value) VALUES(?,?,?)`, row.ID, property.ID, stored.String())
						return err
					}); err != nil {
						t.Fatal(err)
					}
					stored.Reset()
					query := map[string]any{"database": created.Refs["db"].Ref.ID, "limit": 1, "sort": []Sort{{Property: Target{Ref: &property}, Direction: "asc"}}}
					if filtered {
						query["filter"] = []Filter{{Property: Target{Ref: &property}, Op: Contains, Value: json.RawMessage(fmt.Sprintf(`{"id":%q,"inc":%q}`, created.Refs["first"].Ref.ID, fmt.Sprint(created.Refs["first"].Ref.Inc)))}}
					}
					raw, _ := json.Marshal(query)
					body, peak, _, err := measureReaderHeap(t, f.in, raw)
					if err != nil {
						t.Fatal(err)
					}
					var result readResult
					if err := json.Unmarshal(body, &result); err != nil {
						t.Fatal(err)
					}
					if result.Next == nil {
						t.Fatal("capture has no cursor")
					}
					position, _, _, err := decodeReadCursor(*result.Next)
					if err != nil {
						t.Fatal(err)
					}
					spill, ok := f.in.spills.Lookup(position.Token)
					if !ok {
						t.Fatal("capture disappeared")
					}
					c, closeFiles, err := agentFiles(spill)
					if err != nil {
						t.Fatal(err)
					}
					defer closeFiles()
					wantRows := int64(2)
					if filtered {
						wantRows = 1
					}
					summary, err := c.node(c.root)
					if err != nil || summary.rows != wantRows {
						t.Fatalf("captured rows = %d, want %d: %v", summary.rows, wantRows, err)
					}
					first, err := c.at(true, 0)
					if err != nil {
						t.Fatal(err)
					}
					id, err := readRecordID(io.NewSectionReader(c.s.data, first.start, first.length), false)
					if err != nil || id != row.ID {
						t.Fatalf("collection query order/membership: %s %v", id, err)
					}
					t.Logf("whole collection read sampled heap growth: %d bytes", peak)
					if peak > readerHeapCeiling {
						t.Fatalf("query materialized retained collection: %d bytes", peak)
					}
				})
			}
		})
	}
}
