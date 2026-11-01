package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"
)

var errBodyRead = errors.New("body read failed")

// failingBody serves data and then fails with errBodyRead instead of io.EOF,
// as a request body does when the client disconnects. Each read returns at
// most chunk bytes, so the failure can land inside the tar or gzip readers'
// own reads. With together set, the final bytes come with the error in one
// read. It counts any read after the failure.
type failingBody struct {
	data     []byte
	chunk    int
	together bool
	served   int
	failed   bool
	after    int
}

func (f *failingBody) Read(p []byte) (int, error) {
	if f.failed {
		f.after++
		return 0, errBodyRead
	}
	n := min(len(p), f.chunk, len(f.data))
	copy(p, f.data[:n])
	f.data = f.data[n:]
	f.served += n
	if len(f.data) == 0 && (n == 0 || f.together) {
		f.failed = true
		return n, errBodyRead
	}
	return n, nil
}

func plainTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{{"index.html", "<h1>hi</h1>"}, {"app.js", "console.log(1)"}, {"style.css", "body{}"}} {
		writeEntry(t, tw, &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}, f.body)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipped(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// padded appends zeros up to a whole 10240-byte record, as tar(1) writes.
func padded(raw []byte) []byte {
	return append(append([]byte(nil), raw...), make([]byte, 10240-len(raw)%10240)...)
}

func wantReadError(t *testing.T, label string, body *failingBody, err error) {
	t.Helper()
	if _, ok := IsValidation(err); ok {
		t.Fatalf("%s: read error reported as an archive problem: %v", label, err)
	}
	if !errors.Is(err, errBodyRead) {
		t.Fatalf("%s: want the body read error, got %v", label, err)
	}
	if body.after != 0 {
		t.Fatalf("%s: body read %d more times after it failed", label, body.after)
	}
}

func TestArchiveReadErrorMidUpload(t *testing.T) {
	tr := plainTar(t)
	for kind, raw := range map[string][]byte{"tar": tr, "tar.gz": gzipped(t, tr)} {
		// Cut points cover the format sniff, header blocks, file bodies and
		// the end-of-archive marker.
		for _, cut := range []int{2, 10, 100, 512, 700, 1024, 1500, 2048, len(raw) / 2, len(raw) - 1} {
			if cut >= len(raw) {
				continue
			}
			for _, chunk := range []int{1, 7, 512, 1 << 16} {
				for _, together := range []bool{false, true} {
					body := &failingBody{data: raw[:cut], chunk: chunk, together: together}
					_, err := FromArchive(body, lim)
					wantReadError(t, kind, body, err)
				}
			}
		}
	}
}

func TestArchiveReadErrorAfterCompleteArchive(t *testing.T) {
	tr := plainTar(t)
	archives := map[string][]byte{"tar": tr, "padded tar": padded(tr), "tar.gz": gzipped(t, tr), "padded tar.gz": gzipped(t, padded(tr))}
	for kind, raw := range archives {
		t.Run(kind, func(t *testing.T) {
			if files, err := FromArchive(bytes.NewReader(raw), lim); err != nil || len(files) != 3 {
				t.Fatalf("complete archive: %d files, %v", len(files), err)
			}
			for _, together := range []bool{false, true} {
				body := &failingBody{data: raw, chunk: 1 << 16, together: together}
				_, err := FromArchive(body, lim)
				wantReadError(t, kind, body, err)
			}
		})
	}
}

func TestArchiveReadErrorWinsOverArchiveProblems(t *testing.T) {
	// Each body is rejected on its own; with a read error later in the
	// body, the read error is reported, as when the whole body was read
	// before parsing.
	const overhead = 64 << 10
	bomb := tgz(t, map[string]string{"index.html": "ok", "node_modules/huge": strings.Repeat("x", int(lim.MaxBytes+overhead)+1)})
	cases := map[string]struct {
		data []byte
		want string
	}{
		"not an archive":      {bytes.Repeat([]byte("x"), 4096), "not a tar"},
		"invalid gzip header": {append([]byte{0x1f, 0x8b, 0, 0}, make([]byte, 4096)...), "invalid gzip data"},
		"expansion budget":    {bomb, expansionMsg},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := fromArchive(bytes.NewReader(tc.data), lim, overhead)
			if v, ok := IsValidation(err); !ok || !strings.Contains(v.Error(), tc.want) {
				t.Fatalf("without a read error: want %q, got %v", tc.want, err)
			}
			body := &failingBody{data: tc.data, chunk: 1 << 16}
			_, err = fromArchive(body, lim, overhead)
			wantReadError(t, name, body, err)
		})
	}
}

func TestArchiveBodyReadUpToCapacity(t *testing.T) {
	small := Limits{MaxBytes: 1 << 10}
	capacity := int(small.MaxBytes + small.MaxBytes/4 + 1<<20)
	tr := plainTar(t)
	fill := func(n int) []byte { return append(append([]byte(nil), tr...), make([]byte, n-len(tr))...) }

	t.Run("error with the last allowed bytes", func(t *testing.T) {
		body := &failingBody{data: fill(capacity), chunk: 1 << 16, together: true}
		_, err := FromArchive(body, small)
		wantReadError(t, "tar", body, err)
	})
	t.Run("body past the capacity", func(t *testing.T) {
		// Read up to the capacity and no further, so a failure beyond it
		// is never reached.
		body := &failingBody{data: fill(capacity + 4096), chunk: 1 << 16}
		if files, err := FromArchive(body, small); err != nil || len(files) != 3 {
			t.Fatalf("%d files, %v", len(files), err)
		}
		if body.served != capacity {
			t.Fatalf("read %d bytes of the body, want exactly %d", body.served, capacity)
		}
	})
}

func TestZipReadErrors(t *testing.T) {
	// The zip path reads the whole body, plus one byte to detect an
	// oversize archive, before parsing.
	small := Limits{MaxBytes: 1 << 10}
	capacity := int(small.MaxBytes + small.MaxBytes/4 + 1<<20)
	zipBody := func(n int) []byte { return append([]byte("PK\x03\x04"), make([]byte, n-4)...) }
	t.Run("error within the capacity", func(t *testing.T) {
		body := &failingBody{data: zipBody(4096), chunk: 1 << 16}
		_, err := FromArchive(body, small)
		wantReadError(t, "zip", body, err)
	})
	t.Run("error with the byte past the capacity", func(t *testing.T) {
		// Reported as the read error whatever the read sizes.
		for _, chunk := range []int{1, 1000, 1 << 16} {
			body := &failingBody{data: zipBody(capacity + 1), chunk: chunk, together: true}
			_, err := FromArchive(body, small)
			wantReadError(t, "zip", body, err)
		}
	})
	t.Run("oversize without an error", func(t *testing.T) {
		_, err := FromArchive(bytes.NewReader(zipBody(capacity+1)), small)
		if v, ok := IsValidation(err); !ok || !strings.Contains(v.Error(), "larger than the upload limit") {
			t.Fatalf("want the oversize problem, got %v", err)
		}
	})
}
