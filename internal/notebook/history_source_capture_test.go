package notebook

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type readerHistoryFixture struct {
	source             RevisionSource
	paths              map[string]string
	largeHash          string
	largeID            ID
	database, property ID
	expected           []ID
}

func readerHistorySource(t *testing.T, kind string, size int) readerHistoryFixture {
	t.Helper()
	dir := t.TempDir()
	page, block, database := spillID(PageKind, 700), spillID(BlockKind, 700), spillID(DatabaseKind, 700)
	title, property := spillID(PropertyKind, 700), spillID(PropertyKind, 701)
	option, after, view := spillID(OptionKind, 700), spillID(OptionKind, 701), spillID(ViewKind, 700)
	first, second := spillID(RowKind, 700), spillID(RowKind, 701)
	for n := uint64(702); contentBucket(first) != contentBucket(second); n++ {
		second = spillID(RowKind, n)
	}
	if second < first {
		first, second = second, first
	}
	fixture := readerHistoryFixture{source: RevisionSource{Number: 7}, paths: map[string]string{}, database: database, property: property, expected: []ID{database, title, property, option, after, view, first, second}}
	write := func(path string, large bool, parts ...string) {
		t.Helper()
		file, err := os.CreateTemp(dir, "blob-")
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.New()
		writer := io.MultiWriter(file, hash)
		for _, part := range parts {
			if part == "$payload" {
				chunk := strings.Repeat("x", 8192)
				for remaining := size; remaining > 0; {
					n := min(len(chunk), remaining)
					if _, err := io.WriteString(writer, chunk[:n]); err != nil {
						t.Fatal(err)
					}
					remaining -= n
				}
			} else if _, err := io.WriteString(writer, part); err != nil {
				t.Fatal(err)
			}
		}
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		sum := fmt.Sprintf("%x", hash.Sum(nil))
		fixture.paths[sum] = file.Name()
		fixture.source.Files = append(fixture.source.Files, File{Path: path, Hash: sum, Size: info.Size()})
		if large {
			fixture.largeHash = sum
		}
	}
	write("notebook.json", false, `{"format":1}`)
	schemaPrefix := fmt.Sprintf(`{"id":%q,"block":%q,"title":"Archive","property_order":[%q,%q],"view_order":[%q],"properties":[{"id":%q,"database":%q,"definition":{"name":"Title","type":"title"},"option_order":[]},{"id":%q,"database":%q,"definition":{"name":"Status","type":"select"},"option_order":[%q,%q]}],"options":[{"id":%q,"property":%q,"name":"`, database, block, title, property, view, title, database, property, database, option, after, option, property)
	schemaSuffix := fmt.Sprintf(`","color":"gray"},{"id":%q,"property":%q,"name":"After","color":"gray"}],"views":[{"id":%q,"database":%q,"definition":{"name":"After option","display":{"layout":"table","visible":[%q],"widths":[]},"sort":[],"filter":[]}}]}`, after, property, view, database, title)
	schemaPath := fmt.Sprintf("pages/%s/databases/%s/schema.json", page, database)
	rowPath := fmt.Sprintf("pages/%s/databases/%s/rows/%s.json", page, database, contentBucket(first))
	rowPrefix := fmt.Sprintf(`[{"id":%q,"database":%q,"position":"A","cells":{%q:{"type":"title","value":"`, first, database, title)
	rowSuffix := fmt.Sprintf(`"},%q:{"type":"select","value":%q}}},{"id":%q,"database":%q,"position":"B","cells":{%q:{"type":"title","value":"z"},%q:{"type":"select","value":%q}}}]`, property, option, second, database, title, property, after)
	if kind == "cell" {
		write(schemaPath, false, schemaPrefix, "Before", schemaSuffix)
		write(rowPath, true, rowPrefix, "$payload", rowSuffix)
		fixture.largeID = first
		fixture.property = title
	} else {
		write(schemaPath, true, schemaPrefix, "$payload", schemaSuffix)
		write(rowPath, false, rowPrefix, "a", rowSuffix)
		fixture.largeID = option
	}
	return fixture
}

type readerHistoryStream struct {
	*os.File
	afterRead func(int)
	closed    *int
}

func (r *readerHistoryStream) Read(p []byte) (int, error) {
	n, err := r.File.Read(p)
	r.afterRead(n)
	return n, err
}
func (r *readerHistoryStream) Close() error { *r.closed++; return r.File.Close() }

func TestReaderHistoricalSourceCaptureProgress(t *testing.T) {
	fixture := readerHistorySource(t, "cell", 32<<20)
	var clock atomic.Int64
	clock.Store(10000)
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	in, _, err := openNotebookFixture(t, t.TempDir(), &writerBackend{}, Options{Slug: "archive", Title: "Current", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Stop()
	var bytes int64
	closed := 0
	advance := func(n int) { bytes += int64(n); clock.Store(10000 + bytes/(1<<20)) }
	fixture.source.Read = func(_ context.Context, hash string) ([]byte, error) {
		data, err := os.ReadFile(fixture.paths[hash])
		advance(len(data))
		return data, err
	}
	open := func(_ context.Context, hash string) (io.ReadCloser, error) {
		file, err := os.Open(fixture.paths[hash])
		if err != nil {
			return nil, err
		}
		return &readerHistoryStream{File: file, closed: &closed, afterRead: advance}, nil
	}
	field := reflect.ValueOf(&fixture.source).Elem().FieldByName("Open")
	if field.IsValid() {
		field.Set(reflect.ValueOf(open))
	}
	request, _ := json.Marshal(map[string]any{"database": fixture.database})
	if _, err := in.ReadRevision(t.Context(), fixture.source, request); err != nil {
		t.Fatal(err)
	}
	if clock.Load() < 10032 {
		t.Fatal("source did not cross capture progress windows")
	}
	if closed != len(fixture.source.Files) {
		t.Fatalf("closed %d of %d source handles", closed, len(fixture.source.Files))
	}
}

func TestReaderHistoricalSourceMemory(t *testing.T) {
	for _, kind := range []string{"cell", "option"} {
		t.Run(kind, func(t *testing.T) {
			for _, size := range readerMemorySizes() {
				t.Run(fmt.Sprintf("%gMiB", float64(size)/(1<<20)), func(t *testing.T) {
					fixture := readerHistorySource(t, kind, size)
					backend := &historyHTTPBackend{}
					in, _, err := openNotebookFixture(t, t.TempDir(), backend, Options{Slug: "archive", Title: "Current"})
					if err != nil {
						t.Fatal(err)
					}
					defer in.Stop()
					state, err := in.historyRuntime()
					if err != nil {
						t.Fatal(err)
					}
					var peak atomic.Uint64
					sample := func() {
						var stats runtime.MemStats
						runtime.ReadMemStats(&stats)
						for old := peak.Load(); stats.HeapAlloc > old; old = peak.Load() {
							if peak.CompareAndSwap(old, stats.HeapAlloc) {
								break
							}
						}
					}
					wholeReads, opened, closed := 0, 0, 0
					var sourceBytes int64
					underCapture, streamedProgress := true, false
					captureProgress := func() int64 {
						state.manager.mu.Lock()
						defer state.manager.mu.Unlock()
						for _, spill := range state.manager.spills {
							if spill.capturing && !spill.removed {
								return spill.written
							}
						}
						underCapture = false
						return 0
					}
					fixture.source.Read = func(_ context.Context, hash string) ([]byte, error) {
						wholeReads++
						data, err := os.ReadFile(fixture.paths[hash])
						sourceBytes += int64(len(data))
						sample()
						return data, err
					}
					open := func(_ context.Context, hash string) (io.ReadCloser, error) {
						file, err := os.Open(fixture.paths[hash])
						if err != nil {
							return nil, err
						}
						opened++
						var read int64
						return &readerHistoryStream{File: file, closed: &closed, afterRead: func(n int) {
							read += int64(n)
							sourceBytes += int64(n)
							progress := captureProgress()
							if hash == fixture.largeHash && read >= int64(min(size/2, 2<<20)) && progress >= int64(min(size/4, 1<<20)) {
								streamedProgress = true
							}
							sample()
						}}, nil
					}
					// Reflection keeps this regression executable against the pre-streaming API.
					field := reflect.ValueOf(&fixture.source).Elem().FieldByName("Open")
					if field.IsValid() {
						field.Set(reflect.ValueOf(open))
					}
					backend.source = fixture.source
					assembled, err := os.Create(filepath.Join(t.TempDir(), "record.json"))
					if err != nil {
						t.Fatal(err)
					}
					defer assembled.Close()
					oldGC := debug.SetGCPercent(20)
					defer debug.SetGCPercent(oldGC)
					runtime.GC()
					var before runtime.MemStats
					runtime.ReadMemStats(&before)
					peak.Store(before.HeapAlloc)
					stop := make(chan struct{})
					joined := make(chan struct{})
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
					seen := map[ID]bool{}
					query := url.Values{"database": {string(fixture.database)}, "limit": {"1"}, "sort": {fmt.Sprintf(`[{"property":{"id":%q,"inc":"1"},"direction":"asc"}]`, fixture.property)}}
					var callErr error
					complete := false
					for page := 0; page < 1000; page++ {
						request := httptest.NewRequest("GET", "http://example.test/_notebook/api/revisions/7?"+query.Encode(), nil)
						request.Header.Set("X-Flats-Access", "private")
						response := httptest.NewRecorder()
						in.serveHistory(response, request)
						if response.Code != 200 {
							callErr = fmt.Errorf("history status %d: %s", response.Code, response.Body)
							break
						}
						var result struct {
							Schema       []json.RawMessage `json:"schema"`
							Rows         []json.RawMessage `json:"rows"`
							Continuation *struct {
								Record ID     `json:"record"`
								Data   string `json:"data"`
							} `json:"continuation"`
							Next     string `json:"next"`
							Complete bool   `json:"complete"`
						}
						if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
							callErr = err
							break
						}
						for _, raw := range append(result.Schema, result.Rows...) {
							var item struct {
								Ref Ref `json:"ref"`
							}
							if err := json.Unmarshal(raw, &item); err != nil {
								callErr = err
								break
							}
							seen[item.Ref.ID] = true
						}
						if result.Continuation != nil {
							if result.Continuation.Record != fixture.largeID {
								callErr = fmt.Errorf("unexpected large record %s", result.Continuation.Record)
								break
							}
							seen[result.Continuation.Record] = true
							if _, err := io.WriteString(assembled, result.Continuation.Data); err != nil {
								callErr = err
								break
							}
						}
						sample()
						if result.Complete {
							complete = true
							break
						}
						if result.Next == "" {
							callErr = fmt.Errorf("missing historical cursor")
							break
						}
						query = url.Values{"cursor": {result.Next}}
					}
					close(stop)
					<-joined
					sample()
					growth := peak.Load() - min(peak.Load(), before.HeapAlloc)
					if callErr != nil {
						t.Fatal(callErr)
					}
					if !complete {
						t.Fatal("historical traversal did not complete")
					}
					if len(seen) != len(fixture.expected) {
						t.Fatalf("visited %d records, want %d: %v", len(seen), len(fixture.expected), seen)
					}
					for _, id := range fixture.expected {
						if !seen[id] {
							t.Errorf("did not traverse child %s", id)
						}
					}
					var expectedBytes int64
					for _, file := range fixture.source.Files {
						expectedBytes += file.Size
					}
					if sourceBytes != expectedBytes {
						t.Errorf("read %d source bytes, want complete %d", sourceBytes, expectedBytes)
					}
					if wholeReads != 0 {
						t.Errorf("materialized %d source blobs", wholeReads)
					}
					if opened != len(fixture.source.Files) || closed != opened {
						t.Errorf("source handles opened=%d closed=%d files=%d", opened, closed, len(fixture.source.Files))
					}
					if !underCapture || !streamedProgress {
						t.Errorf("source/capture lifecycle: active=%t streamed output before source completion=%t", underCapture, streamedProgress)
					}
					// This is sampled peak Go heap above the pre-call live heap, including
					// source acquisition, import, query comparison and every HTTP continuation.
					t.Logf("%d-byte %s: peak heap growth=%d; all %d records visited", size, kind, growth, len(seen))
					if growth > 4<<20 {
						t.Errorf("historical read heap grew %d bytes above baseline; bound is 4 MiB independent of source size", growth)
					}
					// Validate the complete returned oversized payload after measurement; the
					// verification decoder is deliberately not part of the read's heap sample.
					raw, err := os.ReadFile(assembled.Name())
					if err != nil {
						t.Fatal(err)
					}
					item, err := DecodeItem(raw)
					if err != nil {
						t.Fatal(err)
					}
					text := ""
					if kind == "cell" {
						text = item.Data.(Row).Cells[fixture.property].Value.(string)
					} else {
						text = item.Data.(Option).Name
					}
					if len(text) != size || strings.Count(text, "x") != size {
						t.Fatalf("oversized payload changed: length=%d want=%d", len(text), size)
					}
				})
			}
		})
	}
}
