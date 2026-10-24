package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/qjs"
)

// resetDocumentImages forgets the images this process holds, as a new
// worker process would start.
func resetDocumentImages() {
	documentImages.mu.Lock()
	documentImages.m = map[string]*documentImage{}
	documentImages.mu.Unlock()
}

// pinImageMode runs a test with images on and not forced, whatever
// FLATS_NO_IMAGES/FLATS_FORCE_IMAGES the suite runs with.
func pinImageMode(t *testing.T) {
	t.Setenv("FLATS_NO_IMAGES", "")
	t.Setenv("FLATS_FORCE_IMAGES", "")
}

// imageWorker is an in-process worker: init as WorkerMain runs it, without
// reporting ready (call ready).
type imageWorker struct {
	*worker
	logs *bytes.Buffer
	mu   *sync.Mutex
}

type lockedBuffer struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (l lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func newImageWorker(t *testing.T, spec workerSpec) (*imageWorker, error) {
	t.Helper()
	if spec.Entry == "" {
		spec.Entry = "server.js"
	}
	if spec.DataDir == "" {
		spec.DataDir = t.TempDir()
	}
	buf, mu := &bytes.Buffer{}, &sync.Mutex{}
	w := &worker{spec: spec, log: newChildLog(lockedBuffer{mu, buf})}
	w.data = newDataStore(spec.DataDir)
	if err := w.init(); err != nil {
		w.data.Close()
		return nil, err
	}
	iw := &imageWorker{worker: w, logs: buf, mu: mu}
	t.Cleanup(iw.close)
	return iw, nil
}

func mustImageWorker(t *testing.T, spec workerSpec) *imageWorker {
	t.Helper()
	w, err := newImageWorker(t, spec)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *imageWorker) ready() { w.js.afterReady() }

func (w *imageWorker) close() {
	if w.eng != nil {
		w.eng.close()
		w.eng = nil
	}
	if w.network != nil {
		w.network.Close()
		w.network = nil
	}
	w.data.Close()
}

func (w *imageWorker) log() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.logs.String()
}

// get dispatches a GET to the worker's engine.
func (w *imageWorker) get(t *testing.T, path string) respPayload {
	t.Helper()
	req, _ := json.Marshal(reqPayload{Method: "GET", URL: "http://flat.test" + path, Headers: map[string]string{"host": "flat.test"}})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := w.eng.handle(ctx, req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	var r respPayload
	if err := json.Unmarshal(out, &r); err != nil || r.Error != nil {
		t.Fatalf("GET %s: %s %v", path, out, err)
	}
	return r
}

// firstVM borrows the worker's first runtime.
func (w *imageWorker) firstVM(t *testing.T, f func(vm *jsVM)) {
	t.Helper()
	vm, err := w.js.pool.get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer w.js.pool.put(vm)
	f(vm)
}

const metaApp = `
export default {
  fetch(request, env) {
    return Response.json({gen: request.runtimeGeneration, secret: env.SECRET,
      keys: Object.keys(env).sort(), frozen: Object.isFrozen(env), init: typeof globalThis.__flats_init});
  },
};
`

type metaResult struct {
	Gen    int64
	Secret string
	Keys   []string
	Frozen bool
	Init   string
}

func meta(t *testing.T, w *imageWorker) metaResult {
	t.Helper()
	var m metaResult
	if err := json.Unmarshal([]byte(w.get(t, "/").Body), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestImageInitSplitPerRuntime(t *testing.T) {
	pinImageMode(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"server.js": metaApp})
	for _, images := range []bool{false, true} {
		path := ""
		if images {
			path = filepath.Join(t.TempDir(), "image")
		}
		resetDocumentImages()
		for i, gen := range []int64{7, 9} {
			secret := "s-" + string(rune('a'+i))
			w := mustImageWorker(t, workerSpec{Dir: dir, Generation: gen, Env: map[string]string{"SECRET": secret}, ImagePath: path, PoolSize: 2})
			w.ready()
			want := metaResult{Gen: gen, Secret: secret, Keys: []string{"DB", "FILES", "SECRET"}, Frozen: true, Init: "undefined"}
			got := meta(t, w)
			if got.Gen != want.Gen || got.Secret != want.Secret || strings.Join(got.Keys, ",") != "DB,FILES,SECRET" || !got.Frozen || got.Init != "undefined" {
				t.Fatalf("images=%v worker %d: %+v, want %+v", images, i, got, want)
			}
			restored := false
			w.firstVM(t, func(vm *jsVM) { restored = vm.rt.Restored() })
			if images && i == 1 && !restored {
				t.Fatalf("second worker did not restore the image")
			}
			if !images && restored {
				t.Fatal("restored without an image path")
			}
			// A second runtime in the pool also gets its own values.
			vm, err := newJSVM(w.worker, nil)
			if err != nil {
				t.Fatal(err)
			}
			out, err := vm.invoke(t.Context(), "__flats_dispatch", `{"method":"GET","url":"http://flat.test/","headers":{}}`)
			vm.close()
			if err != nil || !strings.Contains(out, `\"gen\":`+jsonInt(gen)) || !strings.Contains(out, secret) {
				t.Fatalf("pool runtime: %s %v", out, err)
			}
		}
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestImageCreateLoadAndFiles(t *testing.T) {
	pinImageMode(t)
	resetDocumentImages()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"server.js": metaApp})
	state := t.TempDir()
	path := filepath.Join(state, "image")

	w := mustImageWorker(t, workerSpec{Dir: dir, ImagePath: path, Generation: 1})
	if _, err := os.Stat(path); err == nil {
		t.Fatal("image written before ready")
	}
	w.ready()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("image mode %v", fi.Mode())
	}
	if ents, _ := os.ReadDir(state); len(ents) != 1 {
		t.Fatalf("state dir holds %d entries", len(ents))
	}
	w.close()

	// A new process restores the file.
	resetDocumentImages()
	w = mustImageWorker(t, workerSpec{Dir: dir, ImagePath: path, Generation: 2})
	w.firstVM(t, func(vm *jsVM) {
		if !vm.rt.Restored() {
			t.Fatal("not restored from the file")
		}
	})
	if got := meta(t, w); got.Gen != 2 {
		t.Fatalf("generation after restore = %d", got.Gen)
	}
	w.ready()
	w.close()

	// Corrupt: deleted while loading, recaptured and written after ready.
	b, _ := os.ReadFile(path)
	b[len(b)/2] ^= 1
	os.WriteFile(path, b, 0o600)
	resetDocumentImages()
	w = mustImageWorker(t, workerSpec{Dir: dir, ImagePath: path, Generation: 3})
	if _, err := os.Stat(path); err == nil {
		t.Fatal("corrupt image not deleted")
	}
	w.firstVM(t, func(vm *jsVM) {
		if vm.rt.Restored() {
			t.Fatal("restored a corrupt image")
		}
	})
	w.ready()
	if _, err := qjs.UnmarshalImage(mustRead(t, path)); err != nil {
		t.Fatalf("rewritten image: %v", err)
	}
	if got := meta(t, w); got.Gen != 3 {
		t.Fatalf("generation = %d", got.Gen)
	}
	w.close()

	// Stale key: a changed version file invalidates the image.
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(dir, "server.js"), later, later)
	resetDocumentImages()
	w = mustImageWorker(t, workerSpec{Dir: dir, ImagePath: path, Generation: 4})
	if _, err := os.Stat(path); err == nil {
		t.Fatal("stale image not deleted")
	}
	w.ready()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("no new image after a stale one")
	}
	if got := meta(t, w); got.Gen != 4 {
		t.Fatalf("generation = %d", got.Gen)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// staleBinding checks that changing part of the binding refuses an image.
func TestImageBindingInvalidation(t *testing.T) {
	pinImageMode(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"server.js": metaApp})
	path := filepath.Join(t.TempDir(), "image")
	resetDocumentImages()
	w := mustImageWorker(t, workerSpec{Dir: dir, ImagePath: path})
	w.ready()
	w.close()
	img, err := qjs.UnmarshalImage(mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := imageBinding(w.worker)
	opt := (&jsVM{w: w.worker}).options()
	opt.ImageBinding = base
	if !img.Compatible(opt) {
		t.Fatal("image does not match its own binding")
	}
	for name, change := range map[string]func() func(){
		"prelude": func() func() {
			old := preludeJS
			preludeJS += "\n// changed\n"
			return func() { preludeJS = old }
		},
		"build identity": func() func() {
			old := executableIdentity
			executableIdentity = func() string { return "another binary" }
			return func() { executableIdentity = old }
		},
		"entry": func() func() {
			old := w.spec.Entry
			w.spec.Entry = "other.js"
			return func() { w.spec.Entry = old }
		},
	} {
		undo := change()
		b, err := imageBinding(w.worker)
		undo()
		if err != nil {
			t.Fatal(err)
		}
		opt.ImageBinding = b
		if b == base || img.Compatible(opt) {
			t.Errorf("%s: image still compatible", name)
		}
	}
	// The real path: a changed prelude makes the next worker recapture.
	old := preludeJS
	preludeJS += "\n// changed\n"
	defer func() { preludeJS = old }()
	resetDocumentImages()
	w = mustImageWorker(t, workerSpec{Dir: dir, ImagePath: path})
	w.firstVM(t, func(vm *jsVM) {
		if vm.rt.Restored() {
			t.Fatal("restored an image of another prelude")
		}
	})
	if _, err := os.Stat(path); err == nil {
		t.Fatal("image of another prelude not deleted")
	}
}

func TestImageUnwritableDirectory(t *testing.T) {
	pinImageMode(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"server.js": metaApp})
	state := filepath.Join(t.TempDir(), "gone.state")
	resetDocumentImages()
	w := mustImageWorker(t, workerSpec{Dir: dir, ImagePath: filepath.Join(state, "image")})
	w.ready()
	if _, err := os.Stat(state); err == nil {
		t.Fatal("the worker created the state directory")
	}
	if n := strings.Count(w.log(), "document image not saved"); n != 1 {
		t.Fatalf("logged %d times: %s", n, w.log())
	}
	if strings.Contains(w.log(), state) {
		t.Fatalf("log names the host path: %s", w.log())
	}
	if got := meta(t, w); got.Gen != 0 {
		t.Fatal(got)
	}
	// Later runtimes of the process still use the in-memory image.
	vm, err := newJSVM(w.worker, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !vm.rt.Restored() {
		t.Fatal("pool runtime not restored from memory")
	}
	vm.close()
	if n := strings.Count(w.log(), "document image not saved"); n != 1 {
		t.Fatalf("logged %d times", n)
	}
}

func TestImageModes(t *testing.T) {
	pinImageMode(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"server.js": metaApp})
	t.Run("off", func(t *testing.T) {
		t.Setenv("FLATS_NO_IMAGES", "1")
		resetDocumentImages()
		path := filepath.Join(t.TempDir(), "image")
		w := mustImageWorker(t, workerSpec{Dir: dir, ImagePath: path, Generation: 5})
		w.ready()
		if _, err := os.Stat(path); err == nil {
			t.Fatal("image written with images off")
		}
		if got := meta(t, w); got.Gen != 5 {
			t.Fatal(got)
		}
	})
	noisy := t.TempDir()
	writeFiles(t, noisy, map[string]string{"server.js": `console.log("loading"); ` + metaApp})
	t.Run("ineligible", func(t *testing.T) {
		resetDocumentImages()
		path := filepath.Join(t.TempDir(), "image")
		w := mustImageWorker(t, workerSpec{Dir: noisy, ImagePath: path, Generation: 6})
		w.ready()
		if _, err := os.Stat(path); err == nil {
			t.Fatal("image of a module that called the host")
		}
		if got := meta(t, w); got.Gen != 6 {
			t.Fatal(got)
		}
		if !strings.Contains(w.log(), "document image unavailable") || !strings.Contains(w.log(), "loading") {
			t.Fatalf("log: %s", w.log())
		}
	})
	t.Run("forced", func(t *testing.T) {
		t.Setenv("FLATS_FORCE_IMAGES", "1")
		resetDocumentImages()
		if _, err := newImageWorker(t, workerSpec{Dir: noisy, ImagePath: filepath.Join(t.TempDir(), "image")}); err == nil || !strings.Contains(err.Error(), "document image") {
			t.Fatalf("forced images with an ineligible module: %v", err)
		}
		resetDocumentImages()
		w := mustImageWorker(t, workerSpec{Dir: dir, ImagePath: filepath.Join(t.TempDir(), "image")})
		w.ready()
		meta(t, w)
	})
}

// Startup errors are the same with and without images.
func TestImageStartupErrorsUnchanged(t *testing.T) {
	cases := map[string]string{
		"syntax":     "export default {\n  fetch(request) {\n    return new Response(\"x\"\n  }\n}\n",
		"no default": "export const x = 1;",
		"no fetch":   "export default { hello() {} };",
		"throws":     "throw new Error('top-level failure');\nexport default { fetch() {} };",
		"timeout":    "while (true) {}\nexport default { fetch() {} };",
		"recursion":  "function f(n) { return n === 0 ? 0 : 1 + f(n - 1); }\nf(1e6);\nexport default { fetch() {} };",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"server.js": src})
			var msgs []string
			for _, path := range []string{"", filepath.Join(t.TempDir(), "image")} {
				_, err := newImageWorker(t, workerSpec{Dir: dir, ImagePath: path, TimeoutMS: 500})
				if err == nil {
					t.Fatal("started")
				}
				msgs = append(msgs, err.Error())
			}
			// Unbounded recursion overruns the C stack and traps the VM. The
			// call that traps depends on the memory layout, which differs
			// between a fresh and a restored runtime and with any change to
			// the prelude, so only the trap itself must match.
			const trap = "wasm error: out of bounds memory access"
			if name == "recursion" && strings.Contains(msgs[0], trap) && strings.Contains(msgs[1], trap) {
				msgs[1] = msgs[0]
			}
			if msgs[0] != msgs[1] {
				t.Fatalf("without images: %q\nwith images:    %q", msgs[0], msgs[1])
			}
			t.Logf("%s: %.200s", name, msgs[0])
		})
	}
	// A missing entry too.
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.js": "x"})
	_, e1 := newImageWorker(t, workerSpec{Dir: dir})
	_, e2 := newImageWorker(t, workerSpec{Dir: dir, ImagePath: filepath.Join(t.TempDir(), "image")})
	if e1 == nil || e2 == nil || e1.Error() != e2.Error() {
		t.Fatalf("missing entry: %v / %v", e1, e2)
	}
}

// Secrets, DB rows and FILES values never enter an image.
func TestImageHoldsNoSecrets(t *testing.T) {
	pinImageMode(t)
	const (
		envSentinel   = "ENV-SENTINEL-4f9a1c"
		dbSentinel    = "DB-SENTINEL-77d2e0"
		filesSentinel = "FILES-SENTINEL-c03b5e"
	)
	data := t.TempDir()
	seed := t.TempDir()
	writeFiles(t, seed, map[string]string{"server.js": `export default { fetch(r, env) {
  env.DB.exec("CREATE TABLE IF NOT EXISTS s (v TEXT)");
  env.DB.exec("INSERT INTO s VALUES (?)", "` + dbSentinel + `");
  env.FILES.put("k", "` + filesSentinel + `");
  return new Response("seeded");
} };`})
	sw := mustImageWorker(t, workerSpec{Dir: seed, DataDir: data})
	if r := sw.get(t, "/"); r.Body != "seeded" {
		t.Fatal(r)
	}
	sw.close()

	// The docs app and a module that reads everything in its handler.
	app := t.TempDir()
	writeFiles(t, app, map[string]string{"server.js": `export default { fetch(r, env) {
  return Response.json([env.SECRET, env.DB.query("SELECT v FROM s")[0].v, env.FILES.get("k")]);
} };`})
	for name, dir := range map[string]string{"reader": app, "docs": docsDist(t, "# Sentinel test\n")} {
		resetDocumentImages()
		path := filepath.Join(t.TempDir(), "image")
		w := mustImageWorker(t, workerSpec{Dir: dir, DataDir: data, ImagePath: path, Env: map[string]string{"SECRET": envSentinel}})
		if name == "reader" {
			body := w.get(t, "/").Body
			for _, s := range []string{envSentinel, dbSentinel, filesSentinel} {
				if !strings.Contains(body, s) {
					t.Fatalf("handler did not read %s: %s", s, body)
				}
			}
		} else {
			w.get(t, "/")
		}
		w.ready()
		b := mustRead(t, path)
		for _, s := range []string{envSentinel, dbSentinel, filesSentinel} {
			if bytes.Contains(b, []byte(s)) {
				t.Fatalf("%s image contains %s", name, s)
			}
		}
		// Restored runtimes see the env again.
		resetDocumentImages()
		w2 := mustImageWorker(t, workerSpec{Dir: dir, DataDir: data, ImagePath: path, Env: map[string]string{"SECRET": envSentinel}})
		if name == "reader" && !strings.Contains(w2.get(t, "/").Body, envSentinel) {
			t.Fatal("restored runtime lost the env")
		}
		w2.firstVM(t, func(vm *jsVM) {
			if !vm.rt.Restored() {
				t.Fatalf("%s: not restored", name)
			}
		})
	}
}
