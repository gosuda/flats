package runtime

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/qjs"
)

// docsDist is a document runtime directory: the built-in docs app with a
// content module holding markdown.
func docsDist(t *testing.T, markdown string) string {
	t.Helper()
	src, err := filepath.Abs("../apps/docs/dist")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, n := range []string{"server.js", "mermaid-asset.js"} {
		b, err := os.ReadFile(filepath.Join(src, n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	content, _ := json.Marshal(map[string]any{"format": 1, "hash": "0123456789abcdef0123", "entry": "index.md", "title": "Test",
		"documents": []any{map[string]any{"path": "index.md", "hash": "x", "markdown": markdown}}, "assets": []string{}})
	if err := os.WriteFile(filepath.Join(dir, "content.js"), append(append([]byte("export default "), content...), ';', '\n'), 0o444); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestImageDocsRestore(t *testing.T) {
	pinImageMode(t)
	dir := docsDist(t, "# Hello images\n\nSome text.\n")
	path := filepath.Join(t.TempDir(), "image")
	resetDocumentImages()
	start := time.Now()
	w := mustImageWorker(t, workerSpec{Dir: dir, ImagePath: path})
	fresh := time.Since(start)
	page := w.get(t, "/")
	w.ready()
	w.close()
	b := mustRead(t, path)
	for i := 0; i < 3; i++ {
		resetDocumentImages()
		start = time.Now()
		w = mustImageWorker(t, workerSpec{Dir: dir, ImagePath: path})
		restored := time.Since(start)
		start = time.Now()
		got := w.get(t, "/")
		first := time.Since(start)
		if got.Status != page.Status || got.Body != page.Body {
			t.Fatalf("restored page differs: %d %.200q", got.Status, got.Body)
		}
		w.firstVM(t, func(vm *jsVM) {
			if !vm.rt.Restored() {
				t.Fatal("not restored")
			}
		})
		if !w.js.hasWS {
			t.Fatal("restored docs runtime lost its websocket handler")
		}
		t.Logf("docs worker init: fresh %v, restored %v (image %d KiB), first GET %v", fresh.Round(time.Millisecond), restored.Round(100*time.Microsecond), len(b)>>10, first.Round(100*time.Microsecond))
		w.close()
	}
}

// The docs app's module-scope events, pinned: a change that adds module-load
// nondeterminism (a host call, output, Math.random) or leaves a file open
// makes it non-imageable and fails here.
func TestImageDocsCensus(t *testing.T) {
	census := func(dir string) (wall, mono, opens, closes int64, eligible error, hostUsed bool) {
		w := &worker{spec: workerSpec{Dir: dir, Entry: "server.js", DataDir: t.TempDir()}, log: newChildLog(io.Discard)}
		w.data = newDataStore(w.spec.DataDir)
		defer w.data.Close()
		vm := allocJSVM(w, nil)
		opt := vm.options()
		c := &qjs.Census{}
		opt.Census = c
		if err := vm.newRuntime(opt); err != nil {
			t.Fatal(err)
		}
		defer vm.close()
		if err := vm.startup(func(qc *qjs.Context) error { return vm.load(qc, false) }); err != nil {
			t.Fatal(err)
		}
		return c.WallReads.Load(), c.MonoReads.Load(), c.FileOpens.Load(), c.FileCloses.Load(), vm.rt.ImageEligible(), vm.hostUsed
	}
	empty := t.TempDir()
	writeFiles(t, empty, map[string]string{"server.js": "export default {};"})
	bw, bm, bo, bc, berr, bhost := census(empty)
	if berr != nil || bhost || bo != bc {
		t.Fatalf("prelude alone: eligible=%v host=%v opens=%d closes=%d", berr, bhost, bo, bc)
	}
	w, m, o, c, err, host := census(docsDist(t, "# Census\n"))
	t.Logf("docs module scope: %d wall clock reads, %d monotonic reads, %d files opened and %d closed beyond the entry module (prelude and New_QJS: %d wall, %d mono)", w-bw, m-bm, o-bo, c-bc, bw, bm)
	if w-bw != 1 {
		t.Errorf("docs module scope read the wall clock %d times, want exactly 1 (lib0 logging's lastLoggingTime)", w-bw)
	}
	if host {
		t.Error("docs module scope called the host")
	}
	if err != nil {
		t.Errorf("docs runtime not imageable: %v", err)
	}
	if o != c || o-bo == 0 {
		t.Errorf("docs module loader opened %d files and closed %d", o-bo, c-bc)
	}
}
