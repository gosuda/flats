package qjs

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/tetratelabs/wazero"
)

// compileAttempts bounds CompileModule. The workers that race for one cache
// entry start together, so the race ends within milliseconds.
const compileAttempts = 10

// CompileModule compiles wasm on rt. wazero reads the compilation cache, then
// compiles, keeps the result in memory and writes the cache. On Windows both
// file steps fail while another worker has the entry open or is storing it,
// which is routine while workers start on an empty cache. A later call reads
// the stored entry or returns the result from memory.
func CompileModule(ctx context.Context, rt wazero.Runtime, wasm []byte) (wazero.CompiledModule, error) {
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

// cacheRace reports a failed read or write of a cache entry.
func cacheRace(err error) bool {
	var store *os.LinkError
	var read *os.PathError
	return errors.As(err, &store) && store.Op == "rename" ||
		errors.As(err, &read) && read.Op == "open"
}
