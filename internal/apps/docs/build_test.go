package docs

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestBuildInputs(t *testing.T) {
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
	paths = append(paths, "package.json", "package-lock.json", "build.mjs", "../testdata/spike.js", "../testdata/client-edits.js")
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
