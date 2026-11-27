package sheet

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"testing"
	"time"
)

// hashedResponse takes a response without keeping it: it counts and hashes
// what is written, and fails every write once more than limit bytes were
// offered, when limit is not zero.
type hashedResponse struct {
	header  http.Header
	sum     hash.Hash
	n       int64
	largest int
	limit   int64
	failed  int
}

func newHashedResponse(limit int64) *hashedResponse {
	return &hashedResponse{header: http.Header{}, sum: sha256.New(), limit: limit}
}

func (h *hashedResponse) Header() http.Header { return h.header }
func (h *hashedResponse) WriteHeader(int)     {}
func (h *hashedResponse) Write(p []byte) (int, error) {
	if h.limit != 0 && h.n+int64(len(p)) > h.limit {
		h.failed++
		return 0, errors.New("the reader is gone")
	}
	h.n += int64(len(p))
	h.largest = max(h.largest, len(p))
	h.sum.Write(p)
	return len(p), nil
}

// heldPeak samples the heap, as heapPeak does, until stop is called, and
// returns the most it held over its start at the beginning of a
// collection. A collection counts what is allocated while it runs as live,
// and on a busy machine one collection can run while concurrent requests
// allocate many pages, so each sample leaves out what was allocated during
// its collection. What was held when the collection started, and is still
// held, stays in the sample.
func heldPeak() (stop func() int64) {
	sample := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}, {Name: "/memory/classes/heap/objects:bytes"}}
	read := func() int64 {
		metrics.Read(sample)
		before := sample[0].Value.Uint64()
		runtime.GC()
		metrics.Read(sample)
		return int64(sample[1].Value.Uint64()) - int64(sample[0].Value.Uint64()-before)
	}
	base, peak := read(), int64(0)
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			peak = max(peak, read()-base)
			select {
			case <-quit:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	return func() int64 {
		close(quit)
		<-done
		return peak
	}
}

// TestCSVWideRowStaysBounded reads one row whose cells hold the longest
// text the sheet allows, in characters of four bytes: 92 MB of CSV in one
// row. A page of one cell builds one field, however many requests repeat
// its cursor at once; the download streams the row through a fixed buffer;
// and a download whose reader fails stops and leaves nothing charged.
func TestCSVWideRowStaysBounded(t *testing.T) {
	slow(t, "writes a row of 702 cells of 32,767 four-byte characters")
	// Under the race detector the row has 60 such cells, and the pages,
	// the download and the charged bytes are checked; what the heap holds
	// and what a request allocates are not measured.
	const perCall = 50
	cols := raced(702, 60)
	e := start(t)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_cols","tab":"Sheet1","at":"A","count":%d}]}`, cols-newTabCols))
	// Every cell starts with a character of its own, so a page that
	// returns the wrong field is seen.
	cell := func(i int) string { return string(rune(0x4E00+i)) + strings.Repeat("\U0001F600", 32766) }
	for from := 0; from < cols; from += perCall {
		var b strings.Builder
		b.WriteString(`{"ops":[{"op":"write","tab":"Sheet1","at":"` + columnLetters(from) + `1","rows":[[`)
		for i := from; i < min(from+perCall, cols); i++ {
			if i > from {
				b.WriteByte(',')
			}
			b.WriteString(`"` + cell(i) + `"`)
		}
		b.WriteString(`]]}]}`)
		e.mustAgent(b.String())
	}
	field := int64(len(cell(0)))
	rowBytes := int64(cols) * (field + 1)
	base := captureBytes.Load()
	ctx := context.Background()
	page := func(data json.RawMessage) (csv, cursor string) {
		t.Helper()
		var out struct {
			CSV  string `json:"csv"`
			Next struct {
				Cursor string `json:"cursor"`
				Start  string `json:"start"`
			} `json:"next"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		return out.CSV, out.Next.Cursor
	}

	// A page of one cell allocates about that cell, not the row.
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	data, err := e.in.Read(ctx, ReadQuery{Tab: "Sheet1", Format: "csv", Limit: 1})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	csv, cursor := page(data)
	if csv != cell(0)+"," || cursor == "" {
		t.Fatalf("the first page of one cell holds %d bytes and the cursor %q", len(csv), cursor)
	}
	if spent := int64(after.TotalAlloc - before.TotalAlloc); measures && spent > 16*field {
		t.Fatalf("a page of one cell allocated %d bytes; the cell has %d and the row %d", spent, field, rowBytes)
	}
	kept := captureBytes.Load() - base
	if kept < rowBytes {
		t.Fatalf("the kept capture of the row counts %d bytes; its text has %d", kept, rowBytes)
	}

	// Many requests repeat the cursor at once. Each gets the same page,
	// and together they hold a few fields, never a row each.
	const readers, rounds = 16, 4
	var quiet int64
	if measures {
		quiet = quietHeap(t, 1<<20)
	}
	stopHeld := heldPeak()
	runtime.ReadMemStats(&before)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < rounds; round++ {
				data, err := e.in.Read(ctx, ReadQuery{Cursor: cursor})
				if err != nil {
					t.Error(err)
					return
				}
				if got, _ := page(data); got != cell(1)+"," {
					t.Errorf("a repeated cursor gave a page of %d bytes", len(got))
					return
				}
			}
		}()
	}
	wg.Wait()
	runtime.ReadMemStats(&after)
	held := stopHeld()
	// A request holds its page, and the test its decoded copy: a few
	// fields each. A request that built the row would hold it while it
	// cuts its page, so half a row held at any one moment fails, and the
	// pages together would allocate a row each. What stays after the
	// requests is a few fields at most, and a row kept for later requests
	// would stay.
	if limit := rowBytes / 2; measures && held > limit {
		t.Fatalf("%d requests on one cursor held %d bytes at once, more than %d; the row has %d", readers, held, limit, rowBytes)
	}
	if spent, limit := int64(after.TotalAlloc-before.TotalAlloc), readers*rounds*16*field; measures && spent > limit {
		t.Fatalf("%d pages of one cell allocated %d bytes, more than %d", readers*rounds, spent, limit)
	}
	if measures {
		if kept := quietHeap(t, 1<<20) - quiet; kept > 16*field {
			t.Fatalf("after %d requests on one cursor the heap holds %d bytes more than before them; a field has %d", readers, kept, field)
		}
	}
	if got := captureBytes.Load() - base; got != kept {
		t.Fatalf("after the pages %d bytes are charged; the kept capture counts %d", got, kept)
	}

	// The pages of the whole row, at the default size, join to the text
	// the download writes.
	joined := sha256.New()
	var total int64
	pages := 0
	for q := (ReadQuery{Tab: "Sheet1", Format: "csv"}); ; pages++ {
		data, err := e.in.Read(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		csv, cursor := page(data)
		joined.Write([]byte(csv))
		total += int64(len(csv))
		if cursor == "" {
			break
		}
		q = ReadQuery{Cursor: cursor}
	}
	// The full row takes more than twenty pages; the shorter one of the
	// race detector's run still takes more than one.
	if total != rowBytes+1 || pages < raced(20, 1) {
		t.Fatalf("%d pages hold %d bytes; the row has %d", pages+1, total, rowBytes+1)
	}
	e.in.captures.clear()
	if got := captureBytes.Load() - base; got != 0 {
		t.Fatalf("%d bytes stay charged after the paged reads", got)
	}
	download := func(limit int64) *hashedResponse {
		w := newHashedResponse(limit)
		r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/_sheet/csv?tab=%d", e.tabID("Sheet1")), nil)
		r.Header.Set("X-Flats-Access", "private")
		e.in.ServeHTTP(w, r)
		return w
	}
	stop := heapPeak()
	runtime.ReadMemStats(&before)
	whole := download(0)
	runtime.ReadMemStats(&after)
	peak := stop()
	if whole.n != int64(len(csvBOM))+total {
		t.Fatalf("the download has %d bytes, want %d", whole.n, int64(len(csvBOM))+total)
	}
	// The row as it must come out, with and without the byte order mark.
	text, withBOM := sha256.New(), sha256.New()
	withBOM.Write([]byte(csvBOM))
	for i := 0; i < cols; i++ {
		sep := ","
		if i == cols-1 {
			sep = "\r\n"
		}
		text.Write([]byte(cell(i) + sep))
		withBOM.Write([]byte(cell(i) + sep))
	}
	if fmt.Sprintf("%x", joined.Sum(nil)) != fmt.Sprintf("%x", text.Sum(nil)) {
		t.Fatal("the joined pages differ from the row")
	}
	if fmt.Sprintf("%x", whole.sum.Sum(nil)) != fmt.Sprintf("%x", withBOM.Sum(nil)) {
		t.Fatal("the download differs from the row")
	}
	// The download holds its capture's records, a buffer of 64 KiB and one
	// field, and allocates little more than those. A sample counts no more
	// than what is held and what was allocated since the start, so the heap
	// grows by a few fields. A download that held the row would grow it by
	// the row.
	if spent := int64(after.TotalAlloc - before.TotalAlloc); int64(whole.largest) > 4*field || (measures && (spent > 64*field || peak > 16*field)) {
		t.Fatalf("the download allocated %d bytes, raised the heap by %d and wrote up to %d at once; a field has %d bytes and the row %d", spent, peak, whole.largest, field, rowBytes)
	}
	if got := captureBytes.Load() - base; got != 0 {
		t.Fatalf("%d bytes stay charged after the download", got)
	}

	// A reader that fails after 1 MiB ends the download at once.
	runtime.ReadMemStats(&before)
	failed := download(1 << 20)
	runtime.ReadMemStats(&after)
	if failed.failed != 1 || failed.n > 1<<20 {
		t.Fatalf("a failing reader took %d bytes and refused %d writes", failed.n, failed.failed)
	}
	if spent := int64(after.TotalAlloc - before.TotalAlloc); measures && spent > 64*field {
		t.Fatalf("a download that failed after 1 MiB allocated %d bytes", spent)
	}
	if got := captureBytes.Load() - base; got != 0 {
		t.Fatalf("%d bytes stay charged after the failed download", got)
	}
}

// TestCSVPageEndsBeforeARowThatDoesNotFit reads a short row followed by a
// row that alone is larger than a page. The first page is the short row
// alone: the fields of the long row that were tried are taken back out.
// The long row then starts a page of its own and is cut after a comma.
func TestCSVPageEndsBeforeARowThatDoesNotFit(t *testing.T) {
	const cols = 129
	e := start(t)
	e.mustAgent(fmt.Sprintf(`{"ops":[{"op":"insert_cols","tab":"Sheet1","at":"A","count":%d},{"op":"write","tab":"Sheet1","at":"A1","rows":[["x","y"]]}]}`, cols-newTabCols))
	text := strings.Repeat("a", 32767)
	for start := 0; start < cols; start += 43 {
		cells := make([]string, 43)
		for i := range cells {
			cells[i] = `"` + text + `"`
		}
		e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"` + columnLetters(start) + `2","rows":[[` + strings.Join(cells, ",") + `]]}]}`)
	}
	short := "x,y" + strings.Repeat(",", cols-2) + "\r\n"
	long := strings.Repeat(text+",", cols-1) + text + "\r\n"
	var pages []map[string]any
	for q := (ReadQuery{Tab: "Sheet1", Format: "csv"}); ; {
		out := e.read(q)
		pages = append(pages, out)
		next, ok := out["next"].(map[string]any)
		if !ok {
			break
		}
		q = ReadQuery{Cursor: next["cursor"].(string)}
	}
	if len(pages) != 3 {
		t.Fatalf("the two rows were read in %d pages, want 3", len(pages))
	}
	if got := pages[0]["csv"].(string); got != short {
		t.Fatalf("the first page holds %d bytes, want the short row of %d", len(got), len(short))
	}
	if next := pages[0]["next"].(map[string]any); next["start"] != "A2" || pages[1]["start"] != "A2" {
		t.Fatalf("the long row starts at %v on a page that starts at %v", next["start"], pages[1]["start"])
	}
	second := pages[1]["csv"].(string)
	if !strings.HasSuffix(second, ",") || len(second)%(len(text)+1) != 0 {
		t.Fatalf("the second page of %d bytes does not end after a field", len(second))
	}
	if got := second + pages[2]["csv"].(string); got != long {
		t.Fatalf("the pages of the long row join to %d bytes, want %d", len(got), len(long))
	}
	e.in.captures.clear()
}
