package board

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
)

func collect() {
	for range 3 {
		runtime.GC()
	}
}

// Consumed pages of a paging snapshot stop being charged and become
// collectible; the remaining page stays charged and reachable (6, 8.1).
func TestConsumedPagesAreReleased(t *testing.T) {
	c := newPageCache()
	defer c.clear()
	now := time.Now()
	base := pageHostBytes.Load()
	var weaks []weak.Pointer[byte]
	tok := func() string {
		s := &pageSnap{seq: 7}
		for range 3 {
			b := make([]byte, 1<<20)
			weaks = append(weaks, weak.Make(&b[0]))
			s.pages = append(s.pages, b)
			s.bytes += int64(len(b))
		}
		tok, err := c.put(s, now)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}()
	for i := range 2 {
		got, ok := c.next(tok, now)
		if !ok || got.seq != 7 || !got.more || len(got.page) != 1<<20 {
			t.Fatalf("page %d: %v %+v", i, ok, got.more)
		}
	}
	collect()
	for i := range 2 {
		if weaks[i].Value() != nil {
			t.Fatalf("consumed page %d is still reachable after its bytes were released", i)
		}
	}
	if weaks[2].Value() == nil {
		t.Fatal("the unread page was collected")
	}
	if n := pageHostBytes.Load() - base; n != 1<<20 {
		t.Fatalf("charged %d bytes for one remaining page", n)
	}
	if got, ok := c.next(tok, now); !ok || got.more {
		t.Fatalf("last page: %v more=%v", ok, got.more)
	}
	if pageHostBytes.Load() != base {
		t.Fatalf("charged %d after the last page", pageHostBytes.Load()-base)
	}
}

// A Public viewer that received its snapshot and stays idle does not keep
// the released snapshot state reachable through its queue.
func TestDrainedSnapshotQueueReleasesTheState(t *testing.T) {
	b := newTB(t)
	fillBoard(t, b, 50, 100)
	v := connect(t, b.pub, nil, nil)
	if _, els, _ := v.snapshot(); len(els) != 50 {
		t.Fatalf("snapshot of %d elements", len(els))
	}
	waitFor(t, func() bool { return publicSnaps.Load() == 0 })
	var w weak.Pointer[snapData]
	b.state(func(*engine.State) {
		if b.in.snapCur == nil {
			t.Error("no snapshot state")
			return
		}
		w = weak.Make(b.in.snapCur)
		b.in.snapCur = nil // as when the seq moves on
	})
	collect()
	if w.Value() != nil {
		t.Fatal("a sent and released snapshot state stays reachable from the idle socket's queue")
	}
	// The socket still works.
	fillBoard(t, b, 1, 10)
	if _, ok := v.next(isDelta); !ok {
		t.Fatal("no delta after the release")
	}
}

// Commits that leave the seq unchanged (ticks) keep the snapshot state, so
// viewers admitted across them share one copy; a new seq drops it.
func TestUnchangedSeqCommitsShareTheSnapshot(t *testing.T) {
	b := newTB(t)
	fillBoard(t, b, 20, 10)
	a := connect(t, b.pub, nil, nil)
	a.snapshot()
	var first, second *snapData
	b.state(func(*engine.State) { first = b.in.snapCur })
	b.tick()
	b.tick()
	c := connect(t, b.pub, nil, nil)
	c.snapshot()
	b.state(func(*engine.State) { second = b.in.snapCur })
	if first == nil || first != second {
		t.Fatal("a commit without a new seq made the next viewer copy the board again")
	}
	fillBoard(t, b, 1, 10)
	b.state(func(*engine.State) {
		if b.in.snapCur != nil {
			t.Error("the snapshot state of an old seq was kept")
		}
	})
}

// imageRevision records a revision holding an image element and returns
// its number, the image's hash and the element id.
func imageRevision(t *testing.T, b *tb, extra ...map[string]any) (int, string, string) {
	t.Helper()
	a := connect(t, b.priv, map[string]any{"editor": "alice"}, nil)
	a.snapshot()
	img := pngBytes(t, 5, 5)
	_, up := b.post(b.priv, "/_board/api/assets", "image/png", bytes.NewReader(img))
	hash := up["hash"].(string)
	pic := eid()
	ops := append([]map[string]any{{"op": "add", "refs": map[string]any{hash: 0},
		"el": map[string]any{"id": pic, "type": "image", "x": 0, "y": 0, "w": 5, "h": 5, "rot": 0, "asset": hash, "natural": []int{5, 5}}}}, extra...)
	if r := a.batch(1, ops...); r["t"] != "ack" {
		t.Fatal(r)
	}
	b.clock.Add(31 * time.Second)
	b.tick()
	if err := b.core.importOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	return b.core.lastRev(), hash, pic
}

func (b *tb) setHook(fn func(stage string)) {
	b.state(func(*engine.State) { b.in.hook = fn })
}

// Element recovery keeps the revision's assets leased from their selection
// until its commit: retention and a sweep in between cannot leave a
// recovered image without its bytes (5.5).
func TestRecoveryKeepsRevisionAssetsLeasedUntilCommit(t *testing.T) {
	b := newTB(t)
	rev, hash, pic := imageRevision(t, b)
	swept := false
	b.setHook(func(stage string) {
		if stage == "recover-selected" {
			// Retention dropped the revision and the sweep runs now.
			b.core.sweep(hash)
			swept = true
		}
	})
	resp, m := b.post(b.priv, "/_board/api/recover", "application/json", strings.NewReader(`{"revision":`+itoa(rev)+`,"ids":["`+pic+`"]}`))
	if resp.StatusCode != 200 || !swept {
		t.Fatalf("recover %d %v (swept %v)", resp.StatusCode, m, swept)
	}
	if _, err := b.core.ReadBlob(context.Background(), hash); err != nil {
		t.Fatalf("the recovered image's blob was swept between selection and commit: %v", err)
	}
	if b.core.leased(hash) {
		t.Fatal("the lease outlived the recovery")
	}
	if a, ok := b.in.assets.get(hash); !ok || a.Refs == 0 {
		t.Fatalf("the recovered image's asset row %+v %v", a, ok)
	}
}

// Export leases the assets it selects until the archive is written, so a
// sweep in between cannot truncate the file (5.5).
func TestExportKeepsAssetsLeasedUntilWritten(t *testing.T) {
	b := newTB(t)
	_, hash, _ := imageRevision(t, b)
	swept := false
	b.setHook(func(stage string) {
		if stage == "export-selected" {
			if !b.core.leased(hash) {
				t.Error("export selected an asset without leasing it")
			}
			b.core.sweep(hash)
			swept = true
		}
	})
	r, err := http.Get(b.priv.URL + "/_board/export.zip")
	if err != nil || r.StatusCode != 200 {
		t.Fatal(r.StatusCode, err)
	}
	data, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || !swept {
		t.Fatalf("read %v (swept %v)", err, swept)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("export is not a complete ZIP: %v", err)
	}
	found := false
	for _, f := range zr.File {
		found = found || f.Name == "assets/"+hash+".png"
	}
	if !found {
		t.Fatal("the export lost its image")
	}
	if b.core.leased(hash) {
		t.Fatal("the lease outlived the export")
	}
}

// An export whose asset cannot be read never ends as a successful,
// complete-looking file: before the response starts it is an error, after
// it the transfer is aborted.
func TestFailedExportIsNotReportedAsSuccess(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(fmt.Sprintf("started=%v", large), func(t *testing.T) {
			b := newTB(t)
			_, hash, _ := imageRevision(t, b)
			if large {
				fillBoard(t, b, 60, 1000) // more than the ZIP writer buffers
			}
			b.setHook(func(stage string) {
				if stage == "export-selected" {
					b.core.data.Lock()
					delete(b.core.blobs, hash) // lost despite the lease (a disk fault)
					b.core.data.Unlock()
				}
			})
			r, err := http.Get(b.priv.URL + "/_board/export.zip")
			if err != nil {
				t.Fatal(err)
			}
			data, rerr := io.ReadAll(r.Body)
			r.Body.Close()
			if r.StatusCode == http.StatusOK && rerr == nil {
				_, zerr := zip.NewReader(bytes.NewReader(data), int64(len(data)))
				t.Fatalf("a failed export ended as a successful %d-byte response (zip: %v)", len(data), zerr)
			}
			if large && r.StatusCode != http.StatusOK {
				t.Fatalf("expected the started response to be aborted, got %d", r.StatusCode)
			}
		})
	}
}

// Revision reads are not limited by the current-state snapshot cap: a
// revision larger than it is read page by page, without a snapshot (8.1).
func TestRevisionReadsPageBeyondTheSnapshotCap(t *testing.T) {
	old := pageSnapMax
	pageSnapMax = 64 << 10
	defer func() { pageSnapMax = old }()
	var els []model.Element
	for i := range 300 {
		els = append(els, model.Element{ID: fmt.Sprintf("e_%010d", 300-i), Type: model.TypeSticky, Text: noise(1000, i),
			Geom: model.Geom{X: float64(i * 10), W: 100, H: 100}, Z: fmt.Sprint(i)})
	}
	c := engine.Content{Title: "r", Grid: true, Elements: els}
	sorted := slices.Clone(els)
	slices.SortFunc(sorted, func(a, b model.Element) int { return strings.Compare(a.ID, b.ID) })
	if _, err := selectElements(sorted, elementsByID(sorted), Query{}, nil); err == nil {
		t.Fatal("the revision does not exceed the lowered cap")
	}
	base := pageHostBytes.Load()
	var got []string
	q := Query{Limit: 7}
	pages := 0
	for {
		p, err := ReadContent(c, q)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if len(p.Elements) > 7 {
			t.Fatalf("page of %d elements", len(p.Elements))
		}
		for _, e := range p.Elements {
			got = append(got, string(e))
		}
		if p.Next == "" {
			break
		}
		q.Cursor = p.Next
	}
	var want []string
	for _, e := range sorted {
		want = append(want, string(encodeElement(e, Query{}, nil)))
	}
	if !slices.Equal(got, want) || pages != 43 {
		t.Fatalf("%d pages, %d elements, want %d", pages, len(got), len(want))
	}
	if pageHostBytes.Load() != base {
		t.Fatal("a revision read took a paging snapshot")
	}
	// A filtered read pages the same way.
	q = Query{IDs: []string{sorted[3].ID, sorted[250].ID}, Limit: 1}
	p, err := ReadContent(c, q)
	if err != nil || len(p.Elements) != 1 || p.Next == "" {
		t.Fatalf("filtered first page %d %q %v", len(p.Elements), p.Next, err)
	}
	q.Cursor = p.Next
	if p, err = ReadContent(c, q); err != nil || len(p.Elements) != 1 || p.Next != "" {
		t.Fatalf("filtered last page %d %q %v", len(p.Elements), p.Next, err)
	}
}

// Concurrent reads of one cursor each take a distinct page, and exactly
// one sees the last page (run with -race).
func TestConcurrentCursorReads(t *testing.T) {
	b := newTB(t)
	ctx := context.Background()
	fillBoard(t, b, 400, 10)
	p1, err := b.in.GetBoard(ctx, Query{Limit: 5})
	if err != nil || p1.Next == "" {
		t.Fatal(p1.Next, err)
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	ends := 0
	add := func(p Page) {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range p.Elements {
			var m struct{ ID string }
			_ = json.Unmarshal(e, &m)
			if seen[m.ID] {
				t.Errorf("element %s read twice", m.ID)
			}
			seen[m.ID] = true
		}
		if p.Next == "" {
			ends++
		}
	}
	add(p1)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for {
				p, err := b.in.GetBoard(ctx, Query{Cursor: p1.Next})
				if err != nil {
					return
				}
				if p.Seq != p1.Seq {
					t.Errorf("page of seq %d, want %d", p.Seq, p1.Seq)
				}
				add(p)
				if p.Next == "" {
					return
				}
			}
		})
	}
	wg.Wait()
	if len(seen) != 400 || ends != 1 {
		t.Fatalf("%d elements read, %d last pages", len(seen), ends)
	}
}
