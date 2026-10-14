package bundle

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tgz(t *testing.T, entries map[string]string, extra ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	for _, h := range extra {
		tw.WriteHeader(h)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

var lim = Limits{MaxBytes: 1 << 20}

func TestTarGzAndRootStrip(t *testing.T) {
	files, err := FromArchive(bytes.NewReader(tgz(t, map[string]string{"dist/index.html": "hi", "dist/a/b.css": "x", "dist/.DS_Store": "junk"})), lim)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[1].Path != "index.html" || files[0].Path != "a/b.css" {
		t.Fatalf("unexpected files %+v", files)
	}
	m, err := ParseManifest(files)
	if err != nil || m.Kind != "static" || m.Entry != "index.html" || m.Health != "/" {
		t.Fatalf("manifest %+v %v", m, err)
	}
}

func TestZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("index.html")
	w.Write([]byte("<p>z</p>"))
	zw.Close()
	files, err := FromArchive(&buf, lim)
	if err != nil || len(files) != 1 || string(files[0].Data) != "<p>z</p>" {
		t.Fatalf("zip: %v %+v", err, files)
	}
}

func TestRejectsTraversalLinksAndOversize(t *testing.T) {
	_, err := FromArchive(bytes.NewReader(tgz(t, map[string]string{"../evil": "x", "index.html": "y"})), lim)
	if v, ok := IsValidation(err); !ok || !strings.Contains(v.Error(), "escapes") {
		t.Fatalf("traversal: %v", err)
	}
	_, err = FromArchive(bytes.NewReader(tgz(t, map[string]string{"index.html": "y"}, &tar.Header{Name: "link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink})), lim)
	if v, ok := IsValidation(err); !ok || !strings.Contains(v.Error(), "links") {
		t.Fatalf("symlink: %v", err)
	}
	big := strings.Repeat("a", 2<<20)
	_, err = FromArchive(bytes.NewReader(tgz(t, map[string]string{"index.html": big})), lim)
	if v, ok := IsValidation(err); !ok || v.Problems[0].Fix == "" {
		t.Fatalf("oversize: %v", err)
	}
	_, err = FromArchive(strings.NewReader("this is not an archive at all, just text that is long enough"), lim)
	if _, ok := IsValidation(err); !ok {
		t.Fatalf("garbage: %v", err)
	}
}

// requireRejectedArchive checks the public upload error rather than the tar
// reader's internal EOF or checksum errors.
func requireRejectedArchive(t *testing.T, data []byte, limits Limits, overhead int64, want string) {
	t.Helper()
	_, err := fromArchive(bytes.NewReader(data), limits, overhead)
	v, ok := IsValidation(err)
	if !ok || !strings.Contains(v.Error(), want) {
		t.Fatalf("want validation containing %q, got %v", want, err)
	}
}

// writeTgz packs the entries add writes. It uses BestSpeed so large but
// legitimate fixtures stay quick.
func writeTgz(t *testing.T, add func(tw *tar.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(gz)
	add(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeEntry writes one header and body, failing the test on any error so a
// fixture the tar writer refuses cannot silently become a smaller archive.
func writeEntry(t *testing.T, tw *tar.Writer, h *tar.Header, body string) {
	t.Helper()
	if err := tw.WriteHeader(h); err != nil {
		t.Fatalf("header %s: %v", h.Name, err)
	}
	if _, err := io.WriteString(tw, body); err != nil {
		t.Fatalf("body %s: %v", h.Name, err)
	}
}

const expansionMsg = "bytes of tar data"

func TestTarGzBoundsSkippedAndRejectedBodies(t *testing.T) {
	// The collector never reads these bodies, but tar.Reader still
	// decompresses them to reach the next header.
	const overhead = 64 << 10
	big := strings.Repeat("x", int(lim.MaxBytes+overhead)+1)
	for _, name := range []string{"node_modules/huge", ".git/objects/pack", "../escaped", ".DS_Store"} {
		t.Run(name, func(t *testing.T) {
			data := tgz(t, map[string]string{"index.html": "ok", name: big})
			requireRejectedArchive(t, data, lim, overhead, expansionMsg)
		})
	}
}

func TestTarGzKeepsSkippedBodiesWithinBudget(t *testing.T) {
	// Skipped entries larger than the upload limit itself were accepted
	// before the bound and still are while the whole stream fits.
	big := strings.Repeat("x", int(lim.MaxBytes)+1)
	data := tgz(t, map[string]string{"index.html": "ok", "node_modules/huge": big, ".git/pack": big})
	files, err := fromArchive(bytes.NewReader(data), lim, 4<<20)
	if err != nil || len(files) != 1 || files[0].Path != "index.html" {
		t.Fatalf("files %+v err %v", files, err)
	}
}

func TestTarGzBoundsAggregateSkippedContent(t *testing.T) {
	// Each body fits on its own; their sum exceeds the budget.
	const overhead = 64 << 10
	entries := map[string]string{"index.html": "ok"}
	for i := 0; i < 4; i++ {
		entries[fmt.Sprintf("node_modules/dependency-%d", i)] = strings.Repeat("x", int(lim.MaxBytes)/3)
	}
	requireRejectedArchive(t, tgz(t, entries), lim, overhead, expansionMsg)
}

func TestTarGzBoundsPAXMetadata(t *testing.T) {
	// tar.Reader consumes local PAX headers internally and returns global
	// ones as entries; both are decompressed and count toward the budget.
	const overhead = 64 << 10
	payload := strings.Repeat("x", 512<<10)
	for name, typ := range map[string]byte{"local": tar.TypeReg, "global": tar.TypeXGlobalHeader} {
		t.Run(name, func(t *testing.T) {
			data := writeTgz(t, func(tw *tar.Writer) {
				writeEntry(t, tw, &tar.Header{Name: "index.html", Mode: 0o644, Size: 2}, "ok")
				for i := 0; i < 4; i++ {
					h := &tar.Header{
						Name:       fmt.Sprintf("node_modules/pax-%d", i),
						Typeflag:   typ,
						PAXRecords: map[string]string{"comment": payload},
					}
					if typ == tar.TypeReg {
						h.Mode = 0o644
					}
					writeEntry(t, tw, h, "")
				}
			})
			requireRejectedArchive(t, data, lim, overhead, expansionMsg)
		})
	}
}

func TestTarGzBoundsEntryCount(t *testing.T) {
	// Directories do not count toward MaxFiles, but each still costs parser
	// work and counts toward the separate entry limit.
	limits := Limits{MaxBytes: 16 << 20}
	data := writeTgz(t, func(tw *tar.Writer) {
		writeEntry(t, tw, &tar.Header{Name: "index.html", Mode: 0o644, Size: 2}, "ok")
		for i := 0; i < maxTarEntries; i++ {
			writeEntry(t, tw, &tar.Header{Name: fmt.Sprintf("d%d/", i), Typeflag: tar.TypeDir, Mode: 0o755}, "")
		}
	})
	_, err := FromArchive(bytes.NewReader(data), limits)
	v, ok := IsValidation(err)
	if !ok || !strings.Contains(v.Error(), fmt.Sprintf("more than %d entries", maxTarEntries)) {
		t.Fatalf("want entry-count validation, got %v", err)
	}
}

func TestTarGzRejectsBombWithDefaultBudget(t *testing.T) {
	// One skipped entry just past the real budget, streamed as zeros: the
	// gzip input is small, the decompressed tar is not.
	budget := lim.MaxBytes + tarOverhead
	data := writeTgz(t, func(tw *tar.Writer) {
		writeEntry(t, tw, &tar.Header{Name: "index.html", Mode: 0o644, Size: 2}, "ok")
		writeEntry(t, tw, &tar.Header{Name: "node_modules/bomb", Mode: 0o644, Size: budget}, "")
		if _, err := io.CopyN(tw, zeroReader{}, budget); err != nil {
			t.Fatal(err)
		}
	})
	_, err := FromArchive(bytes.NewReader(data), lim)
	v, ok := IsValidation(err)
	if !ok || !strings.Contains(v.Error(), fmt.Sprintf("more than %d bytes of tar data", budget)) {
		t.Fatalf("want expansion validation, got %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestTarGzAcceptsMetadataHeavyArchiveAtFileLimit(t *testing.T) {
	// macOS tar writes PAX extended attributes for every entry (about 2.5 KiB
	// each). A build at MaxFiles with long paths and such metadata must fit.
	xattr := strings.Repeat("A", 2<<10)
	data := writeTgz(t, func(tw *tar.Writer) {
		for d := 0; d < MaxFiles/10; d++ {
			dir := fmt.Sprintf("assets/some/deeper/directory/structure/with-a-fairly-long-name/%05d/", d)
			writeEntry(t, tw, &tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755,
				PAXRecords: map[string]string{"LIBARCHIVE.xattr.com.apple.provenance": xattr}}, "")
			for f := 0; f < 10; f++ {
				name := dir + fmt.Sprintf("generated-chunk-%d.js", f)
				if d == 0 && f == 0 {
					name = "index.html"
				}
				writeEntry(t, tw, &tar.Header{Name: name, Mode: 0o644, Size: 1,
					PAXRecords: map[string]string{"LIBARCHIVE.xattr.com.apple.provenance": xattr}}, "x")
			}
		}
	})
	files, err := FromArchive(bytes.NewReader(data), Limits{MaxBytes: 1 << 20})
	if err != nil || len(files) != MaxFiles {
		t.Fatalf("got %d files, err %v", len(files), err)
	}
}

func TestManifestValidation(t *testing.T) {
	cases := []struct {
		files []File
		want  string
	}{
		{[]File{{Path: "app.js", Data: []byte("x")}}, "no index.html"},
		{[]File{{Path: "index.html"}, {Path: "flats.json", Data: []byte(`{"kind":"lambda"}`)}}, "unknown kind"},
		{[]File{{Path: "index.html"}, {Path: "flats.json", Data: []byte(`{"bogus":1}`)}}, "unknown field"},
		{[]File{{Path: "flats.json", Data: []byte(`{"kind":"server"}`)}}, "no entry"},
		{[]File{{Path: "index.html"}, {Path: "flats.json", Data: []byte(`{"screenshot":"shot.png"}`)}}, "screenshot file is missing"},
	}
	for _, c := range cases {
		_, err := ParseManifest(c.files)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
	}
	m, err := ParseManifest([]File{{Path: "server.js"}, {Path: "flats.json", Data: []byte(`{"kind":"server"}`)}})
	if err != nil || m.Entry != "server.js" {
		t.Fatalf("server default entry: %+v %v", m, err)
	}
}

func TestWriteIsImmutableAndHashed(t *testing.T) {
	dir := t.TempDir()
	files := []File{{Path: "index.html", Data: []byte("a")}, {Path: "x/y.txt", Data: []byte("b")}}
	res, err := Write(files, filepath.Join(dir, "1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Hash != Hash(files) || res.Files != 2 || res.Size != 2 {
		t.Fatalf("result %+v", res)
	}
	if _, err := Write(files, filepath.Join(dir, "1")); err == nil {
		t.Fatal("overwriting a version must fail")
	}
	st, _ := os.Stat(filepath.Join(dir, "1", "index.html"))
	if st.Mode().Perm()&0o222 != 0 {
		t.Fatalf("version files must be read-only, mode %v", st.Mode())
	}
	other := []File{{Path: "index.html", Data: []byte("a")}, {Path: "x/y.txt", Data: []byte("c")}}
	if Hash(other) == res.Hash {
		t.Fatal("hash must change with content")
	}
}

func TestWriteConfinesDirectPathsAndSymlinks(t *testing.T) {
	t.Run("direct traversal", func(t *testing.T) {
		for _, hostile := range []string{"../escaped", "/absolute", `..\escaped`, "a/../escaped", "a//escaped", "bad\x00name"} {
			parent := t.TempDir()
			dst := filepath.Join(parent, "versions", "1")
			files := []File{{Path: "index.html", Data: []byte("safe")}, {Path: hostile, Data: []byte("unsafe")}}
			if _, err := Write(files, dst); err == nil {
				t.Errorf("Write accepted hostile path %q", hostile)
			} else if _, ok := IsValidation(err); !ok {
				t.Errorf("Write(%q) returned a non-validation error: %v", hostile, err)
			}
			if _, err := os.Stat(dst); !os.IsNotExist(err) {
				t.Errorf("Write(%q) created destination: %v", hostile, err)
			}
		}
	})

	t.Run("legacy staging symlink", func(t *testing.T) {
		parent := t.TempDir()
		dst := filepath.Join(parent, "1")
		outside := t.TempDir()
		legacy := dst + ".tmp"
		if err := os.Symlink(outside, legacy); err != nil {
			t.Fatal(err)
		}
		if _, err := Write([]File{{Path: "index.html", Data: []byte("safe")}}, dst); err != nil {
			t.Fatal(err)
		}
		if target, err := os.Readlink(legacy); err != nil || target != outside {
			t.Fatalf("Write changed legacy staging symlink: target=%q err=%v", target, err)
		}
		if _, err := os.Stat(filepath.Join(outside, "index.html")); !os.IsNotExist(err) {
			t.Fatalf("Write touched legacy staging target: %v", err)
		}
	})

	t.Run("file directory collision cleans staging", func(t *testing.T) {
		parent := t.TempDir()
		dst := filepath.Join(parent, "1")
		files := []File{{Path: "index.html"}, {Path: "a", Data: []byte("file")}, {Path: "a/b", Data: []byte("child")}}
		if _, err := Write(files, dst); err == nil {
			t.Fatal("Write accepted a file/directory collision")
		}
		if matches, err := filepath.Glob(filepath.Join(parent, ".1.tmp-*")); err != nil || len(matches) != 0 {
			t.Fatalf("failed Write retained staging directories: %v err=%v", matches, err)
		}
	})

	t.Run("root rejects escaping symlink", func(t *testing.T) {
		staging := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(staging, "linked")); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(staging)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		if err := writeBundleFiles(root, []File{{Path: "linked/escaped", Data: []byte("unsafe")}}); err == nil {
			t.Fatal("rooted write followed a symlink outside the staging directory")
		}
		if _, err := os.Stat(filepath.Join(outside, "escaped")); !os.IsNotExist(err) {
			t.Fatalf("rooted write changed the symlink target: %v", err)
		}
	})
}

func TestFromDirSkipsLinksAndJunk(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("i"), 0o644)
	os.MkdirAll(filepath.Join(dir, "node_modules", "x"), 0o755)
	os.WriteFile(filepath.Join(dir, "node_modules", "x", "a.js"), []byte("n"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(dir, "pw"))
	_, err := FromDir(dir, lim)
	if v, ok := IsValidation(err); !ok || !strings.Contains(v.Error(), "pw") {
		t.Fatalf("symlink in dir must be rejected: %v", err)
	}
	os.Remove(filepath.Join(dir, "pw"))
	files, err := FromDir(dir, lim)
	if err != nil || len(files) != 1 {
		t.Fatalf("FromDir: %v %+v", err, files)
	}
}

func TestHealthPathValidation(t *testing.T) {
	for _, h := range []string{"/ x", "/a b", "/%zz", "/\x01", "//evil.example/x", "http://x/", "health", "/a#b", "/é"} {
		_, err := ParseManifest([]File{{Path: "index.html"}, {Path: "flats.json", Data: []byte(`{"health":` + jsonString(h) + `}`)}})
		v, ok := IsValidation(err)
		if !ok || len(v.Problems) != 1 || !strings.Contains(v.Problems[0].Message, "health") || v.Problems[0].Fix == "" {
			t.Errorf("health %q: want one health problem with a fix, got %v", h, err)
		}
	}
	for _, h := range []string{"/", "/healthz", "/api/health?deep=1", "/a%20b"} {
		m, err := ParseManifest([]File{{Path: "index.html"}, {Path: "flats.json", Data: []byte(`{"health":` + jsonString(h) + `}`)}})
		if err != nil || m.Health != h {
			t.Errorf("health %q must be accepted: %v", h, err)
		}
		// Every accepted path must form a valid request (the deploy builds one).
		if _, err := http.NewRequest(http.MethodGet, "http://flat"+h, nil); err != nil {
			t.Errorf("health %q: %v", h, err)
		}
	}
}

func TestManifestErrorsAreSpecificAndComplete(t *testing.T) {
	problems := func(manifest string, extra ...File) []Problem {
		t.Helper()
		files := append([]File{{Path: "flats.json", Data: []byte(manifest)}}, extra...)
		_, err := ParseManifest(files)
		v, ok := IsValidation(err)
		if !ok {
			t.Fatalf("%s: want a validation error, got %v", manifest, err)
		}
		return v.Problems
	}
	has := func(ps []Problem, msg, fix string) bool {
		for _, p := range ps {
			if strings.Contains(p.Message, msg) && strings.Contains(p.Fix, fix) {
				return true
			}
		}
		return false
	}

	// A type error names the field and the expected type, and the missing
	// index.html is still reported in the same response.
	ps := problems(`{"spa":"yes","name":5}`)
	if !has(ps, `"spa" must be a boolean, not a string`, "true or false") ||
		!has(ps, `"name" must be a string, not a number`, "double quotes") ||
		!has(ps, "no index.html", "index.html") || len(ps) != 3 {
		t.Fatalf("type errors: %+v", ps)
	}

	// A syntax error gives the position.
	ps = problems("{\n  \"kind\": \"static\",\n}")
	if !has(ps, "invalid JSON at byte", "syntax") || !strings.Contains(ps[0].Message, "line 3") || len(ps) != 2 {
		t.Fatalf("syntax error: %+v", ps)
	}

	// An unknown field lists the allowed fields; other problems still show.
	ps = problems(`{"bogus":1,"Health":"/","health":"x"}`, File{Path: "index.html"})
	if !has(ps, `unknown field "bogus"`, "name, kind, entry, spa, not_found, health, screenshot") ||
		!has(ps, `unknown field "Health"`, `rename it to "health"`) ||
		!has(ps, `health "x"`, "/healthz") || len(ps) != 3 {
		t.Fatalf("unknown fields: %+v", ps)
	}

	// A non-object manifest says so.
	ps = problems(`["kind"]`, File{Path: "index.html"})
	if !has(ps, "must be a JSON object, not a JSON array", "braces") {
		t.Fatalf("array manifest: %+v", ps)
	}

	// An unreadable kind does not produce a spurious static-site error for a
	// server bundle.
	ps = problems(`{"kind":1}`, File{Path: "server.js"})
	if len(ps) != 1 || !has(ps, `"kind" must be a string`, `"kind": "static"`) {
		t.Fatalf("bad kind: %+v", ps)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestFromDirSwapRaceNeverLeaks races FromDir against a writer that keeps
// replacing a file with a symlink to a file outside the directory. A failure
// is always a real leak; a pass on vulnerable code is possible.
func TestFromDirSwapRaceNeverLeaks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "a", "page.html")
	if err := os.WriteFile(target, []byte("public"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "a", "link.tmp")
	plain := filepath.Join(dir, "a", "plain.tmp")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Symlink(secret, link)
			_ = os.Rename(link, target)
			_ = os.WriteFile(plain, []byte("public"), 0o644)
			_ = os.Rename(plain, target)
		}
	}()
	defer func() { close(stop); <-done }()
	for range 1000 {
		files, err := FromDir(dir, lim)
		if err != nil {
			continue
		}
		for _, f := range files {
			if strings.Contains(string(f.Data), "SECRET") {
				t.Fatalf("%s carried the outside file into the bundle", f.Path)
			}
		}
	}
}
