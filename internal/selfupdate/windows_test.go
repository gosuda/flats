package selfupdate

import (
	"archive/zip"
	"bytes"
	"testing"
)

func zipArchive(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range entries {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractZipTakesTheExecutable(t *testing.T) {
	var out bytes.Buffer
	raw := zipArchive(t, map[string]string{"flats.exe": "binary", "LICENSE": "text"})
	if err := extractZipTo(&out, raw); err != nil {
		t.Fatal(err)
	}
	if out.String() != "binary" {
		t.Fatalf("got %q", out.String())
	}
}

func TestExtractZipRefuses(t *testing.T) {
	for name, entries := range map[string]map[string]string{
		"no executable": {"LICENSE": "text"},
		"empty":         {"flats.exe": ""},
		"parent path":   {"../flats.exe": "x"},
		"backslash":     {`a\flats.exe`: "x"},
		"absolute path": {"/flats.exe": "x"},
		"unclean path":  {"a//flats.exe": "x"},
	} {
		var out bytes.Buffer
		if err := extractZipTo(&out, zipArchive(t, entries)); Code(err) != "assets_unavailable" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := extractZipTo(&bytes.Buffer{}, []byte("not a zip")); Code(err) != "assets_unavailable" {
		t.Errorf("not a zip: %v", err)
	}
}

func TestSupportedPlatforms(t *testing.T) {
	for _, p := range [][2]string{{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}} {
		if !supportedPlatform(p[0], p[1]) {
			t.Errorf("%v is not supported", p)
		}
	}
	for _, p := range [][2]string{{"windows", "arm64"}, {"freebsd", "amd64"}, {"linux", "386"}} {
		if supportedPlatform(p[0], p[1]) {
			t.Errorf("%v is supported", p)
		}
	}
}
