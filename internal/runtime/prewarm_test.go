package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/qjs"
)

// capturePrewarmLog points prewarm's messages at a buffer for one test.
func capturePrewarmLog(t *testing.T) func() []childMsg {
	t.Helper()
	buf, mu := &bytes.Buffer{}, &sync.Mutex{}
	old := prewarmLog
	prewarmLog = newChildLog(lockedBuffer{mu, buf})
	t.Cleanup(func() {
		waitPrewarm()
		prewarmLog = old
		if s := takePrewarmedShell(prewarmOptions(prewarmSpec{})); s != nil {
			s.Close()
		}
		prewarm.mu.Lock()
		if prewarm.shell != nil {
			prewarm.shell.Close()
			prewarm.shell = nil
		}
		prewarm.mu.Unlock()
	})
	return func() []childMsg {
		mu.Lock()
		defer mu.Unlock()
		var msgs []childMsg
		for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var m childMsg
			if json.Unmarshal([]byte(l), &m) == nil {
				msgs = append(msgs, m)
			}
		}
		return msgs
	}
}

func TestPrewarmShellUsedByFirstRuntime(t *testing.T) {
	msgs := capturePrewarmLog(t)
	cache := t.TempDir()
	start := time.Now()
	startPrewarm(prewarmSpec{CacheDir: cache})
	if since := time.Since(start); since > 50*time.Millisecond {
		t.Fatalf("startPrewarm blocked for %v", since)
	}
	waitPrewarm()
	t.Logf("prewarm took %v", time.Since(start).Round(time.Millisecond))
	if m := msgs(); len(m) != 1 || m[0].T != "warm" {
		t.Fatalf("messages = %+v, want one warm", m)
	}
	prewarm.mu.Lock()
	ready := prewarm.shell != nil
	prewarm.mu.Unlock()
	if !ready {
		t.Fatal("no shell after prewarm")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"server.js": metaApp})
	w := mustImageWorker(t, workerSpec{Dir: dir, CacheDir: cache, Generation: 3, Env: map[string]string{"SECRET": "x"}})
	prewarm.mu.Lock()
	left := prewarm.shell
	prewarm.mu.Unlock()
	if left != nil {
		t.Fatal("the first runtime did not take the shell")
	}
	if got := meta(t, w); got.Gen != 3 || got.Secret != "x" {
		t.Fatal(got)
	}
	// Later runtimes are created as before.
	vm, err := newJSVM(w.worker, nil)
	if err != nil {
		t.Fatal(err)
	}
	vm.close()
}

func TestPrewarmShellUsedByDocumentImage(t *testing.T) {
	pinImageMode(t)
	capturePrewarmLog(t)
	cache := t.TempDir()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"server.js": metaApp})
	path := filepath.Join(t.TempDir(), "image")
	resetDocumentImages()
	w := mustImageWorker(t, workerSpec{Dir: dir, CacheDir: cache, ImagePath: path})
	w.ready()
	w.close()
	resetDocumentImages()
	startPrewarm(prewarmSpec{CacheDir: cache})
	waitPrewarm()
	start := time.Now()
	w = mustImageWorker(t, workerSpec{Dir: dir, CacheDir: cache, ImagePath: path, Generation: 8})
	t.Logf("worker init from a prewarmed shell and an image file: %v", time.Since(start).Round(100*time.Microsecond))
	prewarm.mu.Lock()
	left := prewarm.shell
	prewarm.mu.Unlock()
	if left != nil {
		t.Fatal("the restored runtime did not take the shell")
	}
	w.firstVM(t, func(vm *jsVM) {
		if !vm.rt.Restored() {
			t.Fatal("not restored")
		}
	})
	if got := meta(t, w); got.Gen != 8 {
		t.Fatal(got)
	}
}

// A WASI flat does not use the prewarmed shell; the worker releases it
// rather than holding a second engine for its whole life.
func TestPrewarmShellReleasedByWASIWorker(t *testing.T) {
	capturePrewarmLog(t)
	cache := t.TempDir()
	startPrewarm(prewarmSpec{CacheDir: cache})
	waitPrewarm()
	prewarm.mu.Lock()
	shell := prewarm.shell
	prewarm.mu.Unlock()
	if shell == nil {
		t.Fatal("no shell after prewarm")
	}
	dir := t.TempDir()
	// The smallest valid module; the worker only compiles it at init.
	writeFiles(t, dir, map[string]string{"guest.wasm": "\x00asm\x01\x00\x00\x00"})
	w := mustImageWorker(t, workerSpec{Dir: dir, Entry: "guest.wasm", CacheDir: cache})
	if w.js != nil {
		t.Fatal("not a WASI worker")
	}
	prewarm.mu.Lock()
	left := prewarm.shell
	prewarm.mu.Unlock()
	if left != nil {
		t.Fatal("the WASI worker still holds the prewarmed shell")
	}
	opt := prewarmOptions(prewarmSpec{CacheDir: cache})
	opt.Shell = shell
	if rt, err := qjs.New(opt); err == nil {
		rt.Close()
		t.Fatal("the released shell is still usable")
	}
}

func TestPrewarmMismatchAndFailure(t *testing.T) {
	msgs := capturePrewarmLog(t)
	startPrewarm(prewarmSpec{CacheDir: t.TempDir()})
	waitPrewarm()
	// A spec with another cache directory cannot use the shell; it is
	// released and the runtime is created as before.
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"server.js": metaApp})
	w := mustImageWorker(t, workerSpec{Dir: dir, CacheDir: t.TempDir(), Generation: 2})
	if got := meta(t, w); got.Gen != 2 {
		t.Fatal(got)
	}
	prewarm.mu.Lock()
	left := prewarm.shell
	prewarm.mu.Unlock()
	if left != nil {
		t.Fatal("mismatched shell kept")
	}
	// A failed prewarm logs the failure and still reports warm.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	os.WriteFile(file, nil, 0o600)
	startPrewarm(prewarmSpec{CacheDir: file})
	waitPrewarm()
	m := msgs()
	if len(m) != 3 || m[0].T != "warm" || m[1].T != "log" || m[1].Level != "error" || !strings.Contains(m[1].Msg, "prewarm") || m[2].T != "warm" {
		t.Fatalf("messages = %+v", m)
	}
	// waitPrewarm without a prewarm returns at once.
	waitPrewarm()
}

// Prewarm also creates its warm-up directory, opens and uses a throwaway
// database in it through the code path of env.DB, and removes it. A failure
// only means no warm-up: it is logged once and the worker still reports
// warm.
func TestPrewarmDatabase(t *testing.T) {
	msgs := capturePrewarmLog(t)
	scratch := t.TempDir()
	var during []os.DirEntry
	warmDatabaseHook = func(dir string) { during, _ = os.ReadDir(dir) }
	t.Cleanup(func() { warmDatabaseHook = func(string) {} })
	startPrewarm(prewarmSpec{CacheDir: t.TempDir(), WarmDir: filepath.Join(scratch, "s1")})
	waitPrewarm()
	if m := msgs(); len(m) != 1 || m[0].T != "warm" {
		t.Fatalf("messages = %+v, want one warm", m)
	}
	if len(during) == 0 {
		t.Fatal("no database in the warm-up directory")
	}
	if left, err := os.ReadDir(scratch); err != nil || len(left) != 0 {
		t.Fatalf("scratch directory after the warm-up: %v %v", left, err)
	}
	// Without a usable scratch directory there is no warm-up.
	missing := filepath.Join(t.TempDir(), "missing")
	startPrewarm(prewarmSpec{CacheDir: t.TempDir(), WarmDir: filepath.Join(missing, "s1")})
	waitPrewarm()
	m := msgs()
	if len(m) != 3 || m[1].T != "log" || m[1].Level != "warn" || !strings.Contains(m[1].Msg, "database warm-up") || m[2].T != "warm" {
		t.Fatalf("messages = %+v", m)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("the warm-up created its scratch directory: %v", err)
	}
	// A warm-up directory that exists is not this worker's: it is neither
	// used nor removed.
	taken := filepath.Join(scratch, "s2")
	if err := os.Mkdir(taken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taken, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	startPrewarm(prewarmSpec{CacheDir: t.TempDir(), WarmDir: taken})
	waitPrewarm()
	if m := msgs(); len(m) != 5 || m[3].Level != "warn" || m[4].T != "warm" {
		t.Fatalf("messages = %+v", m)
	}
	if left, err := os.ReadDir(taken); err != nil || len(left) != 1 || left[0].Name() != "keep" {
		t.Fatalf("existing directory after the warm-up: %v %v", left, err)
	}
	// The worker's own database still works afterwards.
	d := newDataStore(t.TempDir())
	defer d.Close()
	c, err := d.conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := dbExec(t.Context(), c, "CREATE TABLE t (x)", nil); err != nil {
		t.Fatal(err)
	}
}
