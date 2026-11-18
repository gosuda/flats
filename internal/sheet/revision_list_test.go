package sheet

import (
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// partsOf returns the files of a revision with count more parts of the
// chunk of path, after its own, each listed at size bytes of the blob
// hash.
func partsOf(files []File, path string, count int, hash string, size int64) []File {
	out := slices.Clone(files)
	prefix := strings.TrimSuffix(path, "0.json")
	for i := 1; i <= count; i++ {
		out = append(out, File{Path: prefix + strconv.Itoa(i) + ".json", Hash: hash, Size: size})
	}
	return out
}

// A revision file list is admitted by its count and the bytes it lists
// before anything in proportion to it is built. A list of far more files
// than a revision has, all parts of one chunk sharing one blob, is refused
// before a blob is opened and with little allocated, and so are parts of
// one blob listed past the bytes the row files of a sheet hold. Parts
// within those bytes are read at most two, until the second repeats the
// first. The view and the rollback refuse each, and the sheet stays as it
// was.
func TestRevisionFileListsAreAdmittedBeforeTheyAreHeld(t *testing.T) {
	f := newLiveFixture(t, unhurried)
	e := f.e
	served := newServed(e.b)
	reopen(t, e, served)
	cells := make([]string, 22)
	for i := range cells {
		cells[i] = jsonf(`"%d":{"v":"%s"}`, i+1, strings.Repeat(`\u0001`, formula.MaxTextLen))
	}
	part := `{"rows":[{"cells":{` + strings.Join(cells, ",") + `},"id":1}],"styles":[]}`
	if len(part) < chunkPartSize || len(part) > maxPartBytes {
		t.Fatalf("the part holds %d bytes", len(part))
	}
	shared := e.withFile(f.files, f.rows, part)
	hash := shared[slices.IndexFunc(shared, func(file File) bool { return file.Path == f.rows })].Hash
	within := int(maxSheetFileBytes/len(part)) - 1
	for _, c := range []struct {
		name  string
		files []File
		most  int64 // the bytes of the shared blob read at most
	}{
		{"many parts of one blob", partsOf(shared, f.rows, 200_000, hash, int64(len(part))), 0},
		{"parts past the bytes of a sheet", partsOf(shared, f.rows, within+2, hash, int64(len(part))), 0},
		{"parts within the bytes of a sheet", partsOf(shared, f.rows, within, hash, int64(len(part))), 2 * int64(len(part))},
	} {
		served.mu.Lock()
		clear(served.byHash)
		served.mu.Unlock()
		var err error
		allocated := allocatedBy(func() {
			_, err = loadRevision(c.files, func(hash string) (io.ReadCloser, error) { return served.OpenBlob(context.Background(), hash) }, nil)
		})
		if err == nil {
			t.Errorf("%s: the revision was taken", c.name)
		}
		if got := served.served(hash); got > c.most {
			t.Errorf("%s: %d bytes of the shared blob were read, at most %d", c.name, got, c.most)
		}
		t.Logf("%s: %d files refused after %d KiB: %v", c.name, len(c.files), allocated>>10, err)
		if measures && c.most == 0 && allocated > 1<<20 {
			t.Errorf("%s: the refusal of %d files allocated %d KiB", c.name, len(c.files), allocated>>10)
		}
		f.wantRefused(c.name, c.files)
	}
}

// A list of as many files as a revision holds, and row files listed at as
// many bytes as a sheet's hold, get past the admission of the list: the
// build opens the manifest. One file more, or one byte more, is refused
// before any file is opened. The build of a list of the most files reaches
// a check point of the list, where a pause that reports false ends it
// before it opens a file.
func TestRevisionFileListsAdmitTheirLargest(t *testing.T) {
	f := newLiveFixture(t)
	chunked := func(count int, size int64) []File {
		files := slices.Clone(f.files)
		for len(files) < count {
			files = append(files, File{Path: chunkPrefix(1, RowID(len(files)+1000)) + "0.json", Hash: strings.Repeat("a", 64), Size: size})
		}
		return files
	}
	parts := func(total int64) []File {
		files := slices.Clone(f.files)
		listed := int64(0)
		for _, file := range files {
			if strings.HasPrefix(file.Path, "tabs/") && strings.Contains(file.Path, "/rows/") {
				listed += file.Size
			}
		}
		for i := 1; listed < total; i++ {
			size := min(int64(maxPartBytes), total-listed)
			files = append(files, File{Path: chunkPrefix(1, 0) + strconv.Itoa(i) + ".json", Hash: strings.Repeat("b", 64), Size: size})
			listed += size
		}
		return files
	}
	opened := func(files []File, pause func(string) bool) (int, error) {
		var n atomic.Int64
		_, err := loadRevision(files, func(hash string) (io.ReadCloser, error) {
			n.Add(1)
			return f.e.b.OpenBlob(context.Background(), hash)
		}, pause)
		return int(n.Load()), err
	}
	for _, c := range []struct {
		name  string
		files []File
		opens bool
	}{
		{"the most files", chunked(maxRevisionFiles, 1), true},
		{"a file more", chunked(maxRevisionFiles+1, 1), false},
		{"the bytes of a sheet", parts(maxSheetFileBytes), true},
		{"a byte more", parts(maxSheetFileBytes + 1), false},
	} {
		n, err := opened(c.files, nil)
		if err == nil {
			t.Fatalf("%s: the damaged revision was taken", c.name)
		}
		if (n > 0) != c.opens {
			t.Errorf("%s: the build opened %d files: %v", c.name, n, err)
		}
	}
	stops := 0
	n, err := opened(chunked(maxRevisionFiles, 1), func(phase string) bool {
		if phase == "files" {
			stops++
			return false
		}
		return true
	})
	if !errors.Is(err, errRevisionStopped) || stops != 1 || n != 0 {
		t.Fatalf("a pause in the list ended the build after %d check points and %d opens: %v", stops, n, err)
	}
}
