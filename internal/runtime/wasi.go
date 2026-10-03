package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// wasiEngine runs a WASI preview1 command module once per request: request
// JSON on stdin, response JSON on stdout, env vars from the flat's env, no
// file system mounts, no network, DB/FILES host ABI or WebSocket API.
// Clocks and CSPRNG are enabled; wasm memory and request time are capped.
type wasiEngine struct {
	w   *worker
	rt  wazero.Runtime
	cm  wazero.CompiledModule
	sem chan struct{}
}

func newWASIEngine(w *worker) (*wasiEngine, error) {
	src, err := readEntry(w.spec.Dir, w.spec.Entry)
	if err != nil {
		return nil, err
	}
	cfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(w.spec.pages())
	if w.spec.CacheDir != "" {
		if cache, err := wazero.NewCompilationCacheWithDir(filepath.Join(w.spec.CacheDir, "wasi")); err == nil {
			cfg = cfg.WithCompilationCache(cache)
		}
	}
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, err
	}
	cm, err := rt.CompileModule(ctx, src)
	if err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("%s is not a valid WebAssembly module: %w", w.spec.Entry, err)
	}
	for _, f := range cm.ImportedFunctions() {
		if mod, _, _ := f.Import(); mod != wasi_snapshot_preview1.ModuleName {
			rt.Close(ctx)
			return nil, fmt.Errorf("%s imports from module %q; only %s is available", w.spec.Entry, mod, wasi_snapshot_preview1.ModuleName)
		}
	}
	return &wasiEngine{w: w, rt: rt, cm: cm, sem: make(chan struct{}, w.spec.poolSize())}, nil
}

func readEntry(dir, entry string) ([]byte, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := r.ReadFile(filepath.Clean(entry))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("entry %s not found in the version files", entry)
	}
	return b, err
}

// capWriter fails writes beyond max bytes.
type capWriter struct {
	buf bytes.Buffer
	max int
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		return 0, fmt.Errorf("response larger than %d MiB", c.max>>20)
	}
	return c.buf.Write(p)
}

func (e *wasiEngine) handle(ctx context.Context, req []byte) ([]byte, error) {
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return nil, &errVMFailed{err: errors.New("all WebAssembly instances stayed busy"), timedOut: true}
	}
	out := &capWriter{max: MaxResponseBody}
	errOut := &lineWriter{log: e.w.log, level: "error"}
	defer errOut.Flush()
	cfg := wazero.NewModuleConfig().
		WithName("").
		WithArgs(filepath.Base(e.w.spec.Entry)).
		WithStdin(bytes.NewReader(req)).
		WithStdout(out).
		WithStderr(errOut).
		WithSysWalltime().
		WithSysNanotime().
		// Not WithSysNanosleep: wazero's host sleep ignores the context, so a
		// guest time.Sleep(time.Hour) would outlive the request timeout.
		WithNanosleep(ctxSleep(ctx)).
		WithRandSource(rand.Reader)
	keys := make([]string, 0, len(e.w.spec.Env))
	for k := range e.w.spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cfg = cfg.WithEnv(k, e.w.spec.Env[k])
	}
	mod, err := e.rt.InstantiateModule(ctx, e.cm, cfg)
	if mod != nil {
		mod.Close(context.Background())
	}
	if err != nil {
		var ee *sys.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 0 {
			err = nil
		} else if ctx.Err() != nil {
			return nil, &errVMFailed{err: err, timedOut: true}
		} else if errors.As(err, &ee) {
			return nil, &errVMFailed{err: fmt.Errorf("module exited with code %d", ee.ExitCode())}
		} else {
			return nil, &errVMFailed{err: err}
		}
	}
	return out.buf.Bytes(), nil
}

// ctxSleep is a guest sleep (WASI poll_oneoff clock wait) that ends when ctx
// is done; CloseOnContextDone then stops the module.
func ctxSleep(ctx context.Context) func(ns int64) {
	return func(ns int64) {
		if ns <= 0 {
			return
		}
		t := time.NewTimer(time.Duration(ns))
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
}

func (e *wasiEngine) close() {
	e.rt.Close(context.Background())
}
