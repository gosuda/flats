package bundle

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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
