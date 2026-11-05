package qjs

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"

	"github.com/tetratelabs/wazero"
)

// A cache error retries only where the race exists. Elsewhere it stays a real
// error that returns at once.
func TestCacheRaceIsWindowsOnly(t *testing.T) {
	windows := runtime.GOOS == "windows"
	for _, err := range []error{
		fmt.Errorf("read: %w", &os.PathError{Op: "open", Path: "entry", Err: os.ErrPermission}),
		fmt.Errorf("write: %w", &os.LinkError{Op: "rename", Old: "tmp", New: "entry", Err: os.ErrPermission}),
	} {
		if got := cacheRace(err); got != windows {
			t.Errorf("cacheRace(%v) = %v on %s", err, got, runtime.GOOS)
		}
	}
	if cacheRace(os.ErrPermission) {
		t.Error("an error that is not of a cache file counted as a race")
	}
}

// Workers are separate processes, so each one here has its own cache object
// and they meet only in the directory. On Windows about half of the plain
// rt.CompileModule calls in this test fail on the cache rename, and with this
// many workers some fail on the read of an entry that another one replaces.
func TestCompileModuleOnEmptySharedCache(t *testing.T) {
	// A module with one empty function.
	module := []byte{
		0, 'a', 's', 'm', 1, 0, 0, 0,
		1, 4, 1, 0x60, 0, 0,
		3, 2, 1, 0,
		10, 4, 1, 2, 0, 0x0b,
	}
	ctx := context.Background()
	for range 40 {
		dir := t.TempDir()
		start := make(chan struct{})
		var workers sync.WaitGroup
		for range 32 {
			workers.Go(func() {
				cache, err := wazero.NewCompilationCacheWithDir(dir)
				if err != nil {
					t.Error(err)
					return
				}
				defer cache.Close(ctx)
				rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(cache))
				defer rt.Close(ctx)
				<-start
				if _, err := CompileModule(ctx, rt, module); err != nil {
					t.Error(err)
				}
			})
		}
		close(start)
		workers.Wait()
	}
}
