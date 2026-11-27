package mcpx

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/store"
)

// allocated returns the bytes the Go heap allocated while fn ran.
func allocated(fn func()) uint64 {
	var mem runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mem)
	before := mem.TotalAlloc
	fn()
	runtime.ReadMemStats(&mem)
	return mem.TotalAlloc - before
}

// The generic forms of get_revision read a sheet's revision as the sheet
// does. Without selectors the file list is admitted by its count: a
// revision listing 200,003 files, one more than the list of a revision
// can hold far over, is refused as a damaged revision, with little
// allocated, and so are its path form and a structured read. With path
// the file is read within its entry and the writer's size: a manifest
// blob made 64 MiB longer than its entry is refused after one byte more,
// and one of other bytes fails its hash. Once the blob is whole again
// both forms answer as before.
func TestGenericRevisionReadsOfASheetAreBounded(t *testing.T) {
	dir := t.TempDir()
	e := newEnvIn(t, dir, nil)
	cs := e.local
	if _, bad := call(t, cs, "update_sheet", map[string]any{"slug": "budget", "ops": []any{
		map[string]any{"op": "write", "tab": "Sheet1", "at": "A1", "rows": []any{[]any{1, "two"}}}}}, nil); bad {
		t.Fatal("the creating write was refused")
	}
	var page struct {
		Revisions []store.Revision `json:"revisions"`
	}
	if _, bad := call(t, cs, "list_revisions", map[string]any{"slug": "budget"}, &page); bad || len(page.Revisions) == 0 {
		t.Fatalf("revisions: %+v", page)
	}
	latest := page.Revisions[0].Number
	var listed RevisionOut
	if text, bad := call(t, cs, "get_revision", map[string]any{"slug": "budget", "revision": latest}, &listed); bad || len(listed.Files) == 0 {
		t.Fatalf("the file list: %s %+v", text, listed)
	}
	var manifest store.RevisionFile
	for _, f := range listed.Files {
		if f.Path == "sheet.json" {
			manifest = f
		}
	}
	if manifest.Hash == "" {
		t.Fatalf("the list has no manifest: %+v", listed.Files)
	}
	var read RevisionOut
	if _, bad := call(t, cs, "get_revision", map[string]any{"slug": "budget", "revision": latest, "path": "sheet.json"}, &read); bad || read.File == nil || !strings.Contains(read.File.Content, `"Sheet1"`) {
		t.Fatalf("the manifest: %+v", read.File)
	}
	original := read.File.Content

	const many = 200_003
	files := make([]store.RevisionFile, 0, many)
	files = append(files, store.RevisionFile{Path: "sheet.json", Hash: manifest.Hash, Size: manifest.Size})
	for i := 1; len(files) < many; i++ {
		files = append(files, store.RevisionFile{Path: "tabs/1/rows/0-" + strconv.Itoa(i) + ".json", Hash: manifest.Hash, Size: manifest.Size})
	}
	if err := e.st.ContentTx(context.Background(), "budget", func(tx *store.ContentTx) error {
		if err := tx.InsertRevision(context.Background(), store.Revision{Number: 90, CreatedAt: page.Revisions[0].CreatedAt, Kind: "agent"}, files); err != nil {
			return err
		}
		// A structured read needs the calculation date an import records.
		return tx.PutSheetImport(context.Background(), "damaged", 1, "damaged", 90, "2026-11-13")
	}); err != nil {
		t.Fatal(err)
	}
	if many <= sheet.MaxRevisionFiles {
		t.Fatalf("%d files are within the list of a revision", many)
	}
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"the file list", map[string]any{"slug": "budget", "revision": 90}},
		{"a file", map[string]any{"slug": "budget", "revision": 90, "path": "sheet.json"}},
		{"a structured read", map[string]any{"slug": "budget", "revision": 90, "tab": "Sheet1"}},
	} {
		var text string
		var bad bool
		used := allocated(func() { text, bad = call(t, cs, "get_revision", c.args, nil) })
		if !bad || !strings.Contains(text, `"category":"unavailable"`) || !strings.Contains(text, `"reason":"revision"`) {
			t.Fatalf("%s of 200,003 files: %.300s", c.name, text)
		}
		t.Logf("%s of %d files refused after %d KiB", c.name, many, used>>10)
		if used > 8<<20 {
			t.Errorf("%s of %d files: the refusal allocated %d KiB", c.name, many, used>>10)
		}
	}

	blob := filepath.Join(dir, "flats", "budget", "revisions", "blobs", manifest.Hash[:2], manifest.Hash)
	if err := os.Chmod(blob, 0o600); err != nil {
		t.Fatal(err)
	}
	longer := int64(len(original)) + 64<<20
	if err := os.Truncate(blob, longer); err != nil {
		t.Fatal(err)
	}
	var text string
	var bad bool
	used := allocated(func() {
		text, bad = call(t, cs, "get_revision", map[string]any{"slug": "budget", "revision": latest, "path": "sheet.json"}, nil)
	})
	if !bad || !strings.Contains(text, `"reason":"revision"`) {
		t.Fatalf("a manifest blob of %d bytes listed at %d: %.300s", longer, manifest.Size, text)
	}
	t.Logf("a manifest blob of %d MiB refused after %d KiB", longer>>20, used>>10)
	if used > 8<<20 {
		t.Errorf("the refusal of a manifest blob of %d MiB allocated %d KiB", longer>>20, used>>10)
	}
	if err := os.WriteFile(blob, bytes.ToUpper([]byte(original)), 0o600); err != nil {
		t.Fatal(err)
	}
	if text, bad := call(t, cs, "get_revision", map[string]any{"slug": "budget", "revision": latest, "path": "sheet.json"}, nil); !bad || !strings.Contains(text, `"reason":"revision"`) {
		t.Fatalf("a manifest of other bytes: %.300s", text)
	}
	if err := os.WriteFile(blob, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	read = RevisionOut{}
	if _, bad := call(t, cs, "get_revision", map[string]any{"slug": "budget", "revision": latest, "path": "sheet.json"}, &read); bad || read.File == nil || read.File.Content != original {
		t.Fatalf("the repaired manifest: %+v", read.File)
	}
	listed = RevisionOut{}
	if _, bad := call(t, cs, "get_revision", map[string]any{"slug": "budget", "revision": latest}, &listed); bad || len(listed.Files) == 0 {
		t.Fatalf("the file list after the repair: %+v", listed)
	}
	for i := 1; i < len(listed.Files); i++ {
		if listed.Files[i-1].Path >= listed.Files[i].Path {
			t.Fatalf("the list is not in path order: %+v", listed.Files)
		}
	}
	if text, bad := call(t, cs, "get_revision", map[string]any{"slug": "budget", "revision": latest, "path": "tabs/1/nothing.json"}, nil); !bad || !strings.Contains(text, `"category":"not_found"`) {
		t.Fatalf("a path the revision lacks: %.300s", text)
	}
}
