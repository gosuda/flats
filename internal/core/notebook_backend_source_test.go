package core

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/store"
)

func notebookSourceID(kind notebook.Kind, n uint64) notebook.ID {
	var b [16]byte
	binary.BigEndian.PutUint64(b[8:], n)
	prefix := map[notebook.Kind]string{notebook.PageKind: "p_", notebook.BlockKind: "b_", notebook.DatabaseKind: "d_", notebook.PropertyKind: "pr_", notebook.OptionKind: "o_", notebook.RowKind: "r_", notebook.ViewKind: "v_"}[kind]
	return notebook.ID(prefix + base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding).EncodeToString(b[:]))
}
func notebookSourceBucket(id notebook.ID) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:1])
}

type notebookSourceFixture struct {
	files              []store.RevisionFile
	paths              map[string]string
	largeHash          string
	largeID            notebook.ID
	database, property notebook.ID
	expected           []notebook.ID
}

func newNotebookSourceFixture(t *testing.T, s *Service, kind string, size int) notebookSourceFixture {
	t.Helper()
	dir := t.TempDir()
	page, block, database := notebookSourceID(notebook.PageKind, 700), notebookSourceID(notebook.BlockKind, 700), notebookSourceID(notebook.DatabaseKind, 700)
	title, property := notebookSourceID(notebook.PropertyKind, 700), notebookSourceID(notebook.PropertyKind, 701)
	option, after, view := notebookSourceID(notebook.OptionKind, 700), notebookSourceID(notebook.OptionKind, 701), notebookSourceID(notebook.ViewKind, 700)
	first, second := notebookSourceID(notebook.RowKind, 700), notebookSourceID(notebook.RowKind, 701)
	for n := uint64(702); notebookSourceBucket(first) != notebookSourceBucket(second); n++ {
		second = notebookSourceID(notebook.RowKind, n)
	}
	if second < first {
		first, second = second, first
	}
	fixture := notebookSourceFixture{paths: map[string]string{}, database: database, property: property, expected: []notebook.ID{database, title, property, option, after, view, first, second}}
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
		pathOnDisk, _ := blobPath(s.docsStoreDir("notes"), sum)
		if err := os.MkdirAll(filepath.Dir(pathOnDisk), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(file.Name(), pathOnDisk); err != nil {
			t.Fatal(err)
		}
		fixture.paths[sum] = pathOnDisk
		fixture.files = append(fixture.files, store.RevisionFile{Path: path, Hash: sum, Size: info.Size()})
		if large {
			fixture.largeHash = sum
		}
	}
	write("notebook.json", false, `{"format":1}`)
	schemaPrefix := fmt.Sprintf(`{"id":%q,"block":%q,"title":"Archive","property_order":[%q,%q],"view_order":[%q],"properties":[{"id":%q,"database":%q,"definition":{"name":"Title","type":"title"},"option_order":[]},{"id":%q,"database":%q,"definition":{"name":"Status","type":"select"},"option_order":[%q,%q]}],"options":[{"id":%q,"property":%q,"name":"`, database, block, title, property, view, title, database, property, database, option, after, option, property)
	schemaSuffix := fmt.Sprintf(`","color":"gray"},{"id":%q,"property":%q,"name":"After","color":"gray"}],"views":[{"id":%q,"database":%q,"definition":{"name":"After option","display":{"layout":"table","visible":[%q],"widths":[]},"sort":[],"filter":[]}}]}`, after, property, view, database, title)
	schemaPath := fmt.Sprintf("pages/%s/databases/%s/schema.json", page, database)
	rowPath := fmt.Sprintf("pages/%s/databases/%s/rows/%s.json", page, database, notebookSourceBucket(first))
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
	if err := s.st.ContentTx(t.Context(), "notes", func(tx *store.ContentTx) error {
		var size int64
		for _, f := range fixture.files {
			size += f.Size
		}
		return tx.InsertRevision(t.Context(), store.Revision{Number: 7, Kind: "agent", Files: len(fixture.files), Size: size, CreatedAt: time.Now()}, fixture.files)
	}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f notebookSourceFixture) filesSize(hash string) int64 {
	for _, file := range f.files {
		if file.Hash == hash {
			return file.Size
		}
	}
	return 0
}

func notebookSourceMemorySizes() []int {
	sizes := []int{1 << 20, 3 << 19}
	if os.Getenv("NOTEBOOK_LARGE_MEMORY_TESTS") == "1" {
		sizes = append(sizes, 8<<20, 32<<20)
	}
	return sizes
}

// Observe the production callbacks, without replacing their file or hash readers.
type notebookSourceBackend struct {
	notebookBackend
	sample                               func()
	afterRead                            func(string, int64)
	wholeReads, opened, closed, released int
	sourceBytes                          int64
}

func (b *notebookSourceBackend) RevisionSource(ctx context.Context, n int) (notebook.RevisionSource, func(), error) {
	src, release, err := b.notebookBackend.RevisionSource(ctx, n)
	if err != nil {
		return src, release, err
	}
	read := src.Read
	src.Read = func(ctx context.Context, hash string) ([]byte, error) {
		b.wholeReads++
		data, err := read(ctx, hash)
		b.sourceBytes += int64(len(data))
		b.sample()
		return data, err
	}
	open := src.Open
	if open != nil {
		src.Open = func(ctx context.Context, hash string) (io.ReadCloser, error) {
			r, err := open(ctx, hash)
			if err != nil {
				return nil, err
			}
			b.opened++
			var read int64
			return &notebookSourceStream{ReadCloser: r, close: func() { b.closed++ }, afterRead: func(n int) {
				read += int64(n)
				b.sourceBytes += int64(n)
				b.afterRead(hash, read)
				b.sample()
			}}, nil
		}
	}
	return src, func() { b.released++; release() }, nil
}

type notebookSourceStream struct {
	io.ReadCloser
	afterRead func(int)
	close     func()
}

func (r *notebookSourceStream) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.afterRead(n)
	return n, err
}
func (r *notebookSourceStream) Close() error { r.close(); return r.ReadCloser.Close() }
func notebookSourceLeases(s *Service, hash string) int {
	d := s.designs()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.leases["notes\x00"+hash]
}

func TestNotebookSourceAuthorizedStreamOwnership(t *testing.T) {
	s, _ := newTestService(t)
	newNotebook(t, s, "notes")
	fixture := newNotebookSourceFixture(t, s, "cell", 1<<20)
	src, release, err := (notebookBackend{s: s, slug: "notes"}).RevisionSource(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if src.Open == nil {
		t.Fatal("production revision source does not supply a bounded opener")
	}
	outside, err := putBlob(s.docsStoreDir("notes"), []byte("not in this revision"))
	if err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{outside, "../../outside", strings.Repeat("0", 64)} {
		if r, err := src.Open(t.Context(), hash); !errors.Is(err, store.ErrNotFound) || r != nil {
			t.Fatalf("unauthorized open %q = %v, %v", hash, r, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if r, err := src.Open(ctx, fixture.largeHash); !errors.Is(err, context.Canceled) || r != nil {
		t.Fatalf("cancelled open = %v, %v", r, err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	r, err := src.Open(ctx, fixture.largeHash)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := notebookSourceLeases(s, fixture.largeHash); got != 2 {
		t.Fatalf("open leases = %d", got)
	}
	release()
	release()
	if got := notebookSourceLeases(s, fixture.largeHash); got != 1 {
		t.Fatalf("reader lost lease on source release: %d", got)
	}
	path := fixture.paths[fixture.largeHash]
	s.removeUnleasedBlob("notes", fixture.largeHash, path, time.Now().Add(time.Hour))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("swept open stream: %v", err)
	}
	buf := make([]byte, 8192)
	if n, err := r.Read(buf); err != nil || n != len(buf) {
		t.Fatalf("first read = %d, %v", n, err)
	}
	cancel()
	if n, err := r.Read(buf); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read = %d, %v", n, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if got := notebookSourceLeases(s, fixture.largeHash); got != 0 {
		t.Fatalf("closed reader leaked %d leases", got)
	}
	s.removeUnleasedBlob("notes", fixture.largeHash, path, time.Now().Add(time.Hour))
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released stream is not sweepable: %v", err)
	}
	if r, err := src.Open(t.Context(), fixture.largeHash); !errors.Is(err, store.ErrNotFound) || r != nil {
		t.Fatalf("missing stream = %v, %v", r, err)
	}
	if got := notebookSourceLeases(s, fixture.largeHash); got != 0 {
		t.Fatalf("failed open leaked %d leases", got)
	}
}

func TestNotebookSourceIntegrityRejectsHistory(t *testing.T) {
	for _, damage := range []string{"changed", "truncated", "extended"} {
		t.Run(damage, func(t *testing.T) {
			s, _ := newTestService(t)
			newNotebook(t, s, "notes")
			fixture := newNotebookSourceFixture(t, s, "cell", 1<<20)
			path := fixture.paths[fixture.largeHash]
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "changed":
				_, err = f.WriteAt([]byte("y"), 8192)
			case "truncated":
				err = f.Truncate(fixture.filesSize(fixture.largeHash) - 1)
			case "extended":
				_, err = f.WriteAt([]byte("x"), fixture.filesSize(fixture.largeHash))
			}
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("damage fixture: %v, %v", err, closeErr)
			}
			src, release, err := (notebookBackend{s: s, slug: "notes"}).RevisionSource(t.Context(), 7)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if src.Open == nil {
				t.Fatal("production revision source does not supply a bounded opener")
			}
			r, err := src.Open(t.Context(), fixture.largeHash)
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.Copy(io.Discard, r)
			if readErr == nil || !strings.Contains(readErr.Error(), "integrity") {
				t.Fatalf("corrupt stream accepted: %v", readErr)
			}
			if n, err := r.Read(make([]byte, 8)); n != 0 || err != readErr {
				t.Fatalf("corruption became success on later read: %d, %v", n, err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			release()
			req := httptest.NewRequest("GET", "http://example.test/_notebook/api/revisions/7?database="+string(fixture.database), nil)
			response := httptest.NewRecorder()
			s.siteHandler("notes", false).ServeHTTP(response, req)
			if response.Code == 200 || strings.Contains(response.Body.String(), `"next"`) || strings.Contains(response.Body.String(), `"rows"`) {
				t.Fatalf("corrupt History returned content: %d %s", response.Code, response.Body)
			}
			if got := notebookSourceLeases(s, fixture.largeHash); got != 0 {
				t.Fatalf("failed capture leaked %d leases", got)
			}
		})
	}
}

func TestNotebookSourceHistoryCancellationClosesStreams(t *testing.T) {
	s, _ := newTestService(t)
	newNotebook(t, s, "notes")
	fixture := newNotebookSourceFixture(t, s, "cell", 1<<20)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	backend := &notebookSourceBackend{notebookBackend: notebookBackend{s: s, slug: "notes"}, sample: func() {}, afterRead: func(hash string, n int64) {
		if hash == fixture.largeHash && n >= 64<<10 {
			cancel()
		}
	}}
	in, _, err := notebook.Open(t.TempDir(), backend, notebook.Options{Slug: "notes", Title: "Notes"})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Stop()
	req := httptest.NewRequest("GET", "http://example.test/_notebook/api/revisions/7?database="+string(fixture.database), nil).WithContext(ctx)
	req.Header.Set("X-Flats-Access", "private")
	response := httptest.NewRecorder()
	in.ServeHTTP(response, req)
	if ctx.Err() != context.Canceled || response.Code == 200 {
		t.Fatalf("capture ignored cancellation: %v, %d %s", ctx.Err(), response.Code, response.Body)
	}
	if backend.opened != len(fixture.files) || backend.closed != backend.opened || backend.released != 1 || backend.wholeReads != 0 {
		t.Fatalf("cancelled capture ownership: %+v", backend)
	}
	if got := notebookSourceLeases(s, fixture.largeHash); got != 0 {
		t.Fatalf("cancelled capture leaked %d leases", got)
	}
}

func TestNotebookSourceHistoryMemory(t *testing.T) {
	for _, mode := range []string{"host", "observed"} {
		t.Run(mode, func(t *testing.T) {
			for _, kind := range []string{"cell", "option"} {
				t.Run(kind, func(t *testing.T) {
					for _, size := range notebookSourceMemorySizes() {
						t.Run(fmt.Sprintf("%gMiB", float64(size)/(1<<20)), func(t *testing.T) {
							s, _ := newTestService(t)
							newNotebook(t, s, "notes")
							s.state("notes").limiter.setRate(10000)
							fixture := newNotebookSourceFixture(t, s, kind, size)
							backend := &notebookSourceBackend{notebookBackend: notebookBackend{s: s, slug: "notes"}}
							scratch := t.TempDir()
							var handler http.Handler = s.siteHandler("notes", false)
							if mode == "observed" {
								in, _, err := notebook.Open(t.TempDir(), backend, notebook.Options{Slug: "notes", Title: "Current", Spills: notebook.NewSpillManager(scratch, 0, time.Now)})
								if err != nil {
									t.Fatal(err)
								}
								defer in.Stop()
								handler = in
							}
							src, release, err := backend.notebookBackend.RevisionSource(t.Context(), 7)
							if err != nil {
								t.Fatal(err)
							}
							release()
							if src.Open == nil {
								t.Fatal("production revision source does not supply a bounded opener")
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
							streamedProgress := false
							backend.sample = sample
							backend.afterRead = func(hash string, read int64) {
								if streamedProgress || hash != fixture.largeHash || read < int64(min(size/2, 2<<20)) || read >= fixture.filesSize(hash) {
									return
								}
								_ = filepath.WalkDir(scratch, func(path string, entry fs.DirEntry, err error) error {
									if err == nil && !entry.IsDir() && entry.Name() == "data" {
										if info, err := entry.Info(); err == nil && info.Size() >= int64(min(size/4, 1<<20)) {
											streamedProgress = true
										}
									}
									return nil
								})
							}
							assembled, err := os.Create(filepath.Join(t.TempDir(), "record.json"))
							if err != nil {
								t.Fatal(err)
							}
							defer assembled.Close()
							// Keep host-wide GC headroom independent of the service fixture's larger live heap.
							oldGC := debug.SetGCPercent(10)
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
							seen := map[notebook.ID]bool{}
							query := url.Values{"database": {string(fixture.database)}, "limit": {"1"}, "sort": {fmt.Sprintf(`[{"property":{"id":%q,"inc":"1"},"direction":"asc"}]`, fixture.property)}}
							var callErr error
							complete := false
							for page := 0; page < 1000; page++ {
								request := httptest.NewRequest("GET", "http://example.test/_notebook/api/revisions/7?"+query.Encode(), nil)
								request.Header.Set("X-Flats-Access", "private")
								response := httptest.NewRecorder()
								handler.ServeHTTP(response, request)
								if response.Code != 200 {
									callErr = fmt.Errorf("history status %d: %s", response.Code, response.Body)
									break
								}
								var result struct {
									Schema       []json.RawMessage `json:"schema"`
									Rows         []json.RawMessage `json:"rows"`
									Continuation *struct {
										Record notebook.ID `json:"record"`
										Data   string      `json:"data"`
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
										Ref notebook.Ref `json:"ref"`
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
							for _, file := range fixture.files {
								expectedBytes += file.Size
							}
							if mode == "observed" {
								if backend.sourceBytes != expectedBytes {
									t.Errorf("read %d source bytes, want %d", backend.sourceBytes, expectedBytes)
								}
								if backend.wholeReads != 0 {
									t.Errorf("materialized %d source blobs", backend.wholeReads)
								}
								if backend.opened != len(fixture.files) || backend.closed != backend.opened {
									t.Errorf("source handles opened=%d closed=%d files=%d", backend.opened, backend.closed, len(fixture.files))
								}
								if !streamedProgress {
									t.Error("no captured output before source completion")
								}
								if backend.released != 1 {
									t.Errorf("source release calls=%d, want 1", backend.released)
								}
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
							item, err := notebook.DecodeItem(raw)
							if err != nil {
								t.Fatal(err)
							}
							text := ""
							if kind == "cell" {
								text = item.Data.(notebook.Row).Cells[fixture.property].Value.(string)
							} else {
								text = item.Data.(notebook.Option).Name
							}
							if len(text) != size || strings.Count(text, "x") != size {
								t.Fatalf("oversized payload changed: length=%d want=%d", len(text), size)
							}
						})
					}
				})
			}
		})
	}
}
