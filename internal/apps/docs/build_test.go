package docs

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestBuildInputs(t *testing.T) {
	t.Parallel()
	var paths []string
	err := filepath.WalkDir("_web/src", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			paths = append(paths, strings.TrimPrefix(filepath.ToSlash(path), "_web/"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, "package.json", "package-lock.json", "build.mjs", "../testdata/spike.js", "../testdata/client-edits.js", "../../../design/web/wheel.js")
	outputs, err := fs.ReadDir(FS(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range outputs {
		if output.Name() != "BUILD-INPUTS.sha256" {
			paths = append(paths, "../dist/"+output.Name())
		}
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Join("_web", p))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		hash.Write([]byte(p + "\x00" + hex.EncodeToString(sum[:]) + "\n"))
	}
	got := hex.EncodeToString(hash.Sum(nil))
	want, err := fs.ReadFile(FS(), "BUILD-INPUTS.sha256")
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.TrimSpace(string(want)) {
		t.Fatal("dist is stale; run npm ci && npm run build in internal/apps/docs/_web")
	}
	if _, err := fs.Stat(FS(), ContentModule); err == nil {
		t.Fatal("host-owned content.js must not be embedded")
	}
}

// TestMermaidGzipHeader: the build writes a gzip header that does not depend
// on the system it ran on (no flags, modification time 0, operating system
// 255), so rebuilding dist on Linux, macOS or Windows gives the committed
// mermaid-asset.js.
func TestMermaidGzipHeader(t *testing.T) {
	t.Parallel()
	b, err := fs.ReadFile(FS(), "mermaid-asset.js")
	if err != nil {
		t.Fatal(err)
	}
	const export = `export const gzip = "`
	i := bytes.Index(b, []byte(export))
	if i < 0 || len(b) < i+len(export)+16 {
		t.Fatal("mermaid-asset.js has no gzip export")
	}
	// 16 base64 characters decode to the first 12 bytes, past the header.
	header, err := base64.StdEncoding.DecodeString(string(b[i+len(export) : i+len(export)+16]))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(header[:8], []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0}) || header[9] != 255 {
		t.Fatalf("gzip header % x: want 1f 8b 08 00, modification time 0 and operating system ff; run npm run build in internal/apps/docs/_web", header[:10])
	}
}
