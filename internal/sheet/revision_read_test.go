package sheet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// servedBackend counts the bytes of blobs it hands out, by hash and in
// all.
type servedBackend struct {
	*fakeBackend
	mu     sync.Mutex
	byHash map[string]int64
	total  atomic.Int64
}

func newServed(b *fakeBackend) *servedBackend {
	return &servedBackend{fakeBackend: b, byHash: map[string]int64{}}
}

func (s *servedBackend) OpenBlob(ctx context.Context, hash string) (io.ReadCloser, error) {
	r, err := s.fakeBackend.OpenBlob(ctx, hash)
	if err != nil {
		return nil, err
	}
	return servedReader{r, s, hash}, nil
}

func (s *servedBackend) served(hash string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byHash[hash]
}

type servedReader struct {
	io.ReadCloser
	s    *servedBackend
	hash string
}

func (r servedReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.s.mu.Lock()
	r.s.byHash[r.hash] += int64(n)
	r.s.mu.Unlock()
	r.s.total.Add(int64(n))
	return n, err
}

// listedAt returns the files with the entry at path listed at size bytes.
func listedAt(files []File, path string, size int64) []File {
	out := slices.Clone(files)
	for i := range out {
		if out[i].Path == path {
			out[i].Size = size
		}
	}
	return out
}

// A revision file is read only up to the size its entry gives, which is
// at most what the writer writes at its path: a file listed past that is
// refused before a byte of it is read, one that holds more than its entry
// says after one byte more, and one that holds less at its end. Chunks
// past the most a tab can have, and parts of a chunk that end before a
// part does, are refused before any of their files is read, and so is a
// list of many empty row files sharing one blob after the first. The view
// and the rollback refuse each, and the sheet stays as it was.
func TestRevisionFilesAreReadWithinTheirBounds(t *testing.T) {
	f := newLiveFixture(t)
	e := f.e
	served := newServed(e.b)
	reopen(t, e, served)
	read := func(files []File) error {
		_, err := loadRevision(files, func(hash string) (io.ReadCloser, error) { return served.OpenBlob(context.Background(), hash) }, nil)
		return err
	}
	if err := read(f.files); err != nil {
		t.Fatal(err)
	}
	hashOf := func(files []File, path string) string {
		for _, file := range files {
			if file.Path == path {
				return file.Hash
			}
		}
		t.Fatalf("no file %s", path)
		return ""
	}
	cell := `{"rows":[{"cells":{"1":{"v":1}},"id":1}],"styles":[]}`
	longer := e.withFile(f.files, f.rows, cell+"  ")
	longer = listedAt(longer, f.rows, int64(len(cell)))
	shorter := e.withFile(f.files, f.rows, cell)
	shorter = listedAt(shorter, f.rows, int64(len(cell)+10))
	empty := `{"rows":[],"styles":[]}`
	var emptyFiles []File
	emptyFiles = slices.Clone(f.files)
	for i := range 100 {
		emptyFiles = e.addFile(emptyFiles, chunkPrefix(1, RowID(100+i))+"0.json", empty)
	}
	var tooMany []File
	tooMany = slices.Clone(f.files)
	for i := range maxTabChunks {
		tooMany = e.addFile(tooMany, chunkPrefix(1, RowID(2+i))+"0.json", `{"rows":[{"cells":{"2":{"v":2}},"id":2}],"styles":[]}`)
	}
	small := e.addFile(e.withFile(f.files, f.rows, cell), "tabs/1/rows/0-1.json", `{"rows":[{"cells":{"2":{"v":2}},"id":1}],"styles":[]}`)
	for _, c := range []struct {
		name  string
		files []File
		hash  string // the blob of which at most most bytes are read
		most  int64
	}{
		{"a manifest listed past the largest", listedAt(f.files, manifestPath, int64(maxManifestBytes)+1), hashOf(f.files, manifestPath), 0},
		{"axes listed past the largest", listedAt(f.files, axesPath(1), int64(maxAxesBytes)+1), hashOf(f.files, axesPath(1)), 0},
		{"a row file listed past the largest", listedAt(f.files, f.rows, int64(maxPartBytes)+1), hashOf(f.files, f.rows), 0},
		{"a row file longer than its entry", longer, hashOf(longer, f.rows), int64(len(cell)) + 1},
		{"a row file shorter than its entry", shorter, hashOf(shorter, f.rows), int64(len(cell))},
		{"empty row files of one blob", emptyFiles, hashOf(emptyFiles, chunkPrefix(1, 100)+"0.json"), int64(len(empty))},
		{"a chunk more than a tab has", tooMany, hashOf(tooMany, chunkPrefix(1, 2)+"0.json"), 0},
		{"a part before the last smaller than a part", small, hashOf(small, f.rows), 0},
	} {
		served.mu.Lock()
		clear(served.byHash)
		served.mu.Unlock()
		if err := read(c.files); err == nil {
			t.Errorf("%s: the revision was taken", c.name)
		}
		if got := served.served(c.hash); got > c.most {
			t.Errorf("%s: %d bytes of the file were read, at most %d", c.name, got, c.most)
		}
		f.wantRefused(c.name, c.files)
	}
}

// reopen opens the sheet of e again with the backend b, which stands in
// front of e.b.
func reopen(t *testing.T, e *env, b Backend) {
	t.Helper()
	e.in.Stop()
	in, _, err := Open(e.dir, b, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	e.in, e.b.in = in, in
	if err := in.await(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
}

// A row file is refused at the first entry the writer could not have
// written, before it holds the rest: a file of one cell and as many empty
// formats as its largest size holds, a format more than its cells, empty
// rows, a row more than a chunk holds, a row of a cell more than a tab has
// columns, and formats before the rows. The decode reads little of each
// and allocates far less than the file.
func TestRowFileDecodeRefusesAtTheFirstImpossibleEntry(t *testing.T) {
	cells := func(n int) string {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"%d":{"v":1}`, i+1)
		}
		return b.String()
	}
	rows := func(n int, cells string) string {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"cells":{%s},"id":%d}`, cells, i+1)
		}
		return b.String()
	}
	formats := func(n int, format string) string {
		return strings.TrimSuffix(strings.Repeat(format+",", n), ",")
	}
	empties := (maxPartBytes - 64) / 3
	for _, c := range []struct {
		name string
		file string
		most int // the bytes the decode may read
	}{
		{"empty formats", `{"rows":[` + rows(1, cells(1)) + `],"styles":[` + formats(empties, `{}`) + `]}`, 128 << 10},
		{"a format more than the cells", `{"rows":[` + rows(1, cells(2)) + `],"styles":[{"bold":true},{"italic":true},{"wrap":true}` +
			strings.Repeat(`,{"underline":true}`, 200_000) + `]}`, 128 << 10},
		{"empty rows", `{"rows":[` + rows(150_000, "") + `],"styles":[]}`, 128 << 10},
		{"a row more than a chunk", `{"rows":[` + rows(100_000, cells(1)) + `],"styles":[]}`, 256 << 10},
		{"a cell more than the columns", `{"rows":[` + rows(1, cells(50_000)) + `],"styles":[]}`, 128 << 10},
		{"formats before the rows", `{"styles":[` + formats(200_000, `{"bold":true}`) + `],"rows":[` + rows(1, cells(1)) + `]}`, 128 << 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			if len(c.file) > maxPartBytes {
				t.Fatalf("the file of %d bytes is past the largest the writer writes", len(c.file))
			}
			in := &countingReader{r: strings.NewReader(c.file)}
			var err error
			got := allocatedBy(func() { _, err = decodeRowFile(in, &loadPace{}) })
			if err == nil {
				t.Fatal("the file was taken")
			}
			t.Logf("refused after %d of %d bytes and %d KiB: %v", in.n, len(c.file), got>>10, err)
			if in.n > c.most {
				t.Errorf("the decode read %d of %d bytes before it refused", in.n, len(c.file))
			}
			if measures && got > 1<<20 {
				t.Errorf("the decode of %d bytes allocated %d KiB before it refused", len(c.file), got>>10)
			}
		})
	}
}

// The build of a revision view takes turns at the gate while it reads a
// row file: a small sheet loads twice while every slot is taken by builds
// held between two pieces of the file, which the backend has not handed
// out whole. A stop of their sheets ends the builds there, before they
// read any more of it or decode it. It runs with a gate of one slot and
// with several.
func TestRevisionBuildTakesTurnsWhileItReads(t *testing.T) {
	slow(t, "builds revisions of 256 full rows while others wait")
	ctx := context.Background()
	cols := sized(formula.MaxCols, 96)
	for _, slots := range gateSizes {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			gateSlots(t, slots)
			dense := make([]*env, slots)
			served := make([]*servedBackend, slots)
			for i := range dense {
				dense[i] = denseSheet(t, cols)
				served[i] = newServed(dense[i].b)
				reopen(t, dense[i], served[i])
			}
			w := watchLoads(t)
			w.holdAt("read")
			sizes := make([]int64, slots)
			hashes := make([]string, slots)
			for i, e := range dense {
				n := e.b.importAll(t)
				files, date, release, err := e.b.RevisionFiles(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				for _, f := range files {
					if strings.HasPrefix(f.Path, "tabs/1/rows/") {
						sizes[i], hashes[i] = f.Size, f.Hash
					}
				}
				// A check point comes after every four pieces of the read.
				if sizes[i] <= 4*filePiece {
					t.Fatalf("the row file of %d bytes reaches no check point while it is read", sizes[i])
				}
				if _, err := e.in.ReadRevision(ctx, n, files, date, ReadQuery{Tab: "Sheet1", Range: "A1"}); refusal(err).Reason != "calculating" {
					t.Fatalf("the view of revision %d did not start a build: %v", n, err)
				}
			}
			w.heldAt(t, slots, "read")
			held := make([]int64, slots)
			for i := range dense {
				if held[i] = served[i].served(hashes[i]); held[i] >= sizes[i] {
					t.Fatalf("the build held in the read had read %d of %d bytes", held[i], sizes[i])
				}
			}
			handOverAndStop(t, w, slots, "read", []string{"rows", "styles", "cells", "formats"}, dense)
			for i := range dense {
				if got := served[i].served(hashes[i]); got != held[i] {
					t.Fatalf("the stopped build read %d bytes more of its row file", got-held[i])
				}
			}
		})
	}
}

// A rollback whose request is cancelled while it reads a row file ends
// there: the backend hands out no more of the file, and the sheet stays as
// it was.
func TestRevisionReadEndsWithItsRequest(t *testing.T) {
	slow(t, "writes 256 full rows")
	e := denseSheet(t, sized(formula.MaxCols, 96))
	served := newServed(e.b)
	reopen(t, e, served)
	target := e.b.importAll(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["newer"]]}]}`)
	files, _, release, err := e.b.RevisionFiles(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var rows File
	for _, f := range files {
		if strings.HasPrefix(f.Path, "tabs/1/rows/") {
			rows = f
		}
	}
	w := watchLoads(t)
	w.holdAt("read")
	ctx, cancel := context.WithCancel(context.Background())
	seq, dump := e.seq(), e.dump()
	done := make(chan error, 1)
	go func() { done <- e.in.Rollback(ctx, files, "Ann", "rollback-cancelled") }()
	w.heldAt(t, 1, "read")
	held := served.served(rows.Hash)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled rollback ended with %v", err)
	}
	if got := served.served(rows.Hash); got != held || got >= rows.Size {
		t.Fatalf("the rollback read %d bytes of the row file of %d, %d of them after its request was cancelled", got, rows.Size, got-held)
	}
	if e.seq() != seq || e.dump() != dump {
		t.Fatal("the cancelled rollback changed the sheet")
	}
	e.want("A1", "newer")
}

// The largest row files the writer writes load: a chunk of long texts of
// control characters, which a row file holds six bytes each, takes parts
// of 4 MiB and one text more. A view and a rollback take them, and the
// sheet opens the same again.
func TestLargestRowFilesLoad(t *testing.T) {
	slow(t, "writes 4 MiB parts of long texts")
	e := start(t, unhurried)
	text := strings.Repeat("\x01", formula.MaxTextLen)
	n := chunkPartSize/(6*formula.MaxTextLen) + 4
	for i := range n {
		e.mustAgent(jsonf(`{"ops":[{"op":"write","tab":"Sheet1","at":"A%d","rows":[["%s"]]}]}`, i+1, strings.Repeat(`\u0001`, formula.MaxTextLen)))
	}
	rev := closeRevision(e)
	files, date, release, err := e.b.RevisionFiles(context.Background(), rev)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	largest := int64(0)
	for _, f := range files {
		if strings.HasPrefix(f.Path, "tabs/1/rows/") {
			largest = max(largest, f.Size)
		}
	}
	if largest <= chunkPartSize || largest > int64(maxPartBytes) {
		t.Fatalf("the largest part holds %d bytes, want past %d and within %d", largest, chunkPartSize, maxPartBytes)
	}
	e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:A100"]}]}`)
	if err := e.viewRevision(5000, files, date); err != nil {
		t.Fatalf("the view of the largest parts: %v", err)
	}
	if err := e.in.Rollback(context.Background(), files, "Ann", "largest"); err != nil {
		t.Fatalf("the rollback to the largest parts: %v", err)
	}
	e.want(fmt.Sprintf("A%d", n), text)
	dump := e.dump()
	e.restart()
	if after := e.dump(); after != dump {
		t.Fatalf("the sheet opens differently:\n%s", dumpDiff(dump, after))
	}
}
