package runtime

import (
	"sync"

	"github.com/gosuda/flats/internal/qjs"
)

// A pre-started worker builds, before it has a spec, everything its first
// JavaScript runtime needs that does not depend on a flat: a qjs shell
// (wazero runtime, WASI, host module and the compiled qjs.wasm, loaded from
// the compilation cache). The first runtime of the worker takes the shell
// when its options match; any later runtime is created as before.

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
		prewarmLog.send(childMsg{T: "warm"})
	}()
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
