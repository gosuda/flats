package runtime

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/qjs"
)

// A pre-started worker builds, before it has a spec, everything its first
// JavaScript runtime needs that does not depend on a flat: a qjs shell
// (wazero runtime, WASI, host module and the compiled qjs.wasm, loaded from
// the compilation cache). The first runtime of the worker takes the shell
// when its options match; any later runtime is created as before. It also
// pays SQLite's once-per-process costs with a throwaway database, so the
// flat's first env.DB use does not.

var prewarm struct {
	mu    sync.Mutex
	done  chan struct{} // closed when the prewarm started last has finished
	shell *qjs.Shell
}

// prewarmLog receives the "warm" message and prewarm failures.
var prewarmLog = stderrLog

// startPrewarm prepares flat-independent engine state in a pre-started
// worker before it receives its spec. It does not block. When it finishes
// it sends {"t":"warm"}, also after a failure, which it logs first.
func startPrewarm(p prewarmSpec) {
	done := make(chan struct{})
	prewarm.mu.Lock()
	old := prewarm.shell
	prewarm.shell, prewarm.done = nil, done
	prewarm.mu.Unlock()
	old.Close()
	go func() {
		defer close(done)
		executableIdentity() // read once, at worker start (see imageBinding)
		s, err := qjs.NewShell(prewarmOptions(p))
		if err != nil {
			prewarmLog.Log("error", "prewarm: "+err.Error())
		} else {
			prewarm.mu.Lock()
			if prewarm.done == done {
				prewarm.shell = s
				s = nil
			}
			prewarm.mu.Unlock()
			s.Close()
		}
		if err := warmDatabase(p.WarmDir); err != nil {
			prewarmLog.Log("warn", "prewarm: database warm-up: "+err.Error())
		}
		prewarmLog.send(childMsg{T: "warm"})
	}()
}

// warmDatabaseHook runs in warmDatabase after the statement, before the
// directory is removed; for tests.
var warmDatabaseHook = func(dir string) {}

// warmDatabase creates the directory dir, which must not exist yet, opens a
// throwaway database in it through the same code path as a flat's env.DB,
// runs one statement and removes it again. The first file database a
// process opens pays for SQLite's one-time setup (library, VFS, first
// statement); later opens, also of other files, do not. A failure only means
// no warm-up.
func warmDatabase(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err // also when it exists: not this worker's
	}
	defer os.RemoveAll(dir)
	d := newDataStore(dir)
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := d.conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err = c.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS warm (x)"); err != nil {
		return err
	}
	warmDatabaseHook(dir)
	return nil
}

// prewarmOptions are the runtime-wide options a worker's first runtime uses
// with the default memory limit (jsVM.options).
func prewarmOptions(p prewarmSpec) qjs.Option {
	return qjs.Option{CacheDir: p.CacheDir, CloseOnContextDone: true, MemoryLimitPages: DefaultMemoryPages}
}

// waitPrewarm returns when prewarm started by startPrewarm has finished.
func waitPrewarm() {
	prewarm.mu.Lock()
	done := prewarm.done
	prewarm.mu.Unlock()
	if done != nil {
		<-done
	}
}

// releasePrewarmedShell closes a prewarmed shell that the worker's first
// runtime did not take (a WASI flat, or a runtime created without it), so
// the worker does not hold a second engine for its whole life.
func releasePrewarmedShell() {
	prewarm.mu.Lock()
	s := prewarm.shell
	prewarm.shell = nil
	prewarm.mu.Unlock()
	s.Close()
}

// takePrewarmedShell hands out the prewarmed shell once, if opt can use it.
// A shell that does not fit is released: every runtime of a worker has the
// same options.
func takePrewarmedShell(opt qjs.Option) *qjs.Shell {
	prewarm.mu.Lock()
	s := prewarm.shell
	prewarm.shell = nil
	prewarm.mu.Unlock()
	if s == nil {
		return nil
	}
	if !s.Compatible(opt) {
		s.Close()
		return nil
	}
	return s
}
