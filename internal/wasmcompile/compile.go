// Package wasmcompile compiles wasm on a wazero runtime that shares a cache
// directory with other worker processes.
package wasmcompile

import (
	"context"
	"errors"
	"os"
	"runtime"
	"time"

	"github.com/tetratelabs/wazero"
)

// compileAttempts bounds Module. The workers that race for one cache
// entry start together, so the race ends within milliseconds.
const compileAttempts = 10

// Module compiles wasm on rt. wazero reads the compilation cache, then
// compiles, keeps the result in memory and writes the cache. On Windows both
// file steps fail while another worker has the entry open or is storing it,
// which is routine while workers start on an empty cache. A later call reads
// the stored entry or returns the result from memory.
func Module(ctx context.Context, rt wazero.Runtime, wasm []byte) (wazero.CompiledModule, error) {
	for attempt := 1; ; attempt++ {
		compiled, err := rt.CompileModule(ctx, wasm)
		if err == nil || attempt == compileAttempts || !cacheRace(err) {
			return compiled, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * 10 * time.Millisecond):
		}
	}
}

// cacheRace reports a failed read or write of a cache entry on Windows. Elsewhere
// a rename replaces an open file, so such an error is real and returns at once.
func cacheRace(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	var store *os.LinkError
	var read *os.PathError
	return errors.As(err, &store) && store.Op == "rename" ||
		errors.As(err, &read) && read.Op == "open"
}
