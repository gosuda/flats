package qjs

import (
	"context"
	crand "crypto/rand"
	_ "embed"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	wsp1 "github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed qjs.wasm
var wasmBytes []byte

// FORK (flats): wazero >= 1.12 binds function type IDs to one runtime's
// store, so a module compiled in a throwaway runtime cannot be instantiated in
// another one. Every Runtime now compiles qjs.wasm itself; a process-wide
// CompilationCache keeps that cheap after the first compile. The runtime
// config is built per New so CloseOnContextDone and MemoryLimitPages are
// per-runtime settings instead of being fixed by the first New in the process.
var (
	cacheMu     sync.Mutex
	sharedCache wazero.CompilationCache
	cacheDirs   = map[string]wazero.CompilationCache{}
)

// DefaultMemoryLimitPages caps the wasm linear memory (64 KiB pages) when
// Option.MemoryLimitPages is zero: 1024 pages = 64 MiB.
const DefaultMemoryLimitPages = 1024

func compilationCache(cacheDir string) (wazero.CompilationCache, error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cacheDir == "" {
		if sharedCache == nil {
			sharedCache = wazero.NewCompilationCache()
		}
		return sharedCache, nil
	}
	if c, ok := cacheDirs[cacheDir]; ok {
		return c, nil
	}
	c, err := wazero.NewCompilationCacheWithDir(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create compilation cache with dir %s: %w", cacheDir, err)
	}
	cacheDirs[cacheDir] = c
	return c, nil
}

// contextNanosleep returns the guest's sleep (WASI poll_oneoff clock waits:
// os.sleep, os.setTimeout, the std event loop). FORK (flats): upstream used
// wazero's host sleep, which ignores the context, so a guest sleeping for an
// hour blocked its call for an hour even with CloseOnContextDone. This one
// returns as soon as ctx (read at each sleep) is done; the closed module then
// stops at its next function call or loop iteration.
func contextNanosleep(ctx context.Context) func(ns int64) {
	return func(ns int64) {
		if ns <= 0 {
			return
		}
		done := ctx.Done()
		if done == nil {
			time.Sleep(time.Duration(ns))
			return
		}
		t := time.NewTimer(time.Duration(ns))
		defer t.Stop()
		select {
		case <-t.C:
		case <-done:
		}
	}
}

// Runtime wraps a QuickJS WebAssembly runtime with memory management.
type Runtime struct {
	wrt      wazero.Runtime
	module   api.Module
	malloc   api.Function
	free     api.Function
	mem      *Mem
	option   Option
	handle   *Handle
	context  *Context
	registry *ProxyRegistry
	stackLow uint32 // FORK (flats): relocated C stack, see stack.go
	stackTop uint32
}

// New creates a QuickJS runtime with optional configuration.
func New(options ...Option) (runtime *Runtime, err error) {
	defer func() {
		rerr := AnyToError(recover())
		if rerr != nil {
			runtime = nil
			err = fmt.Errorf("failed to create QJS runtime: %w", rerr)
		}
	}()

	proxyRegistry := NewProxyRegistry()

	option, err := getRuntimeOption(proxyRegistry, options...)
	if err != nil {
		return nil, fmt.Errorf("failed to get runtime options: %w", err)
	}

	cache, err := compilationCache(option.CacheDir)
	if err != nil {
		return nil, err
	}
	if option.DisableBuildCache {
		cache = wazero.NewCompilationCache()
	}
	pages := option.MemoryLimitPages
	if pages == 0 {
		pages = DefaultMemoryLimitPages
	}
	rcfg := wazero.NewRuntimeConfig().
		WithCompilationCache(cache).
		WithCloseOnContextDone(option.CloseOnContextDone).
		WithMemoryLimitPages(pages)

	runtime = &Runtime{
		option:   option,
		context:  &Context{Context: option.Context},
		registry: proxyRegistry,
	}

	runtime.wrt = wazero.NewRuntimeWithConfig(option.Context, rcfg)
	ok := false
	defer func() {
		if !ok {
			_ = runtime.wrt.Close(context.Background())
		}
	}()

	if _, err := wsp1.Instantiate(option.Context, runtime.wrt); err != nil {
		return nil, fmt.Errorf("failed to instantiate WASI: %w", err)
	}

	if _, err := runtime.wrt.NewHostModuleBuilder("env").
		NewFunctionBuilder().
		WithFunc(option.ProxyFunction).
		Export("jsFunctionProxy").
		Instantiate(option.Context); err != nil {
		return nil, fmt.Errorf("failed to setup host module: %w", err)
	}

	qjsBytes := wasmBytes
	if len(option.QuickJSWasmBytes) > 0 {
		qjsBytes = option.QuickJSWasmBytes
	}
	if qjsBytes, err = exportStackPointer(qjsBytes); err != nil {
		return nil, fmt.Errorf("failed to prepare qjs module: %w", err)
	}
	compiled, err := runtime.wrt.CompileModule(option.Context, qjsBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to compile qjs module: %w", err)
	}

	modCfg := wazero.NewModuleConfig().
		WithStartFunctions(option.StartFunctionName).
		WithSysWalltime().
		WithSysNanotime().
		WithNanosleep(contextNanosleep(option.Context)).
		// FORK (flats): wazero's default random source is a fixed-seed
		// PRNG (identical bytes in every runtime); use the OS CSPRNG.
		WithRandSource(crand.Reader).
		WithStdout(option.Stdout).
		WithStderr(option.Stderr)
	// FORK (flats): no host directory unless explicitly requested, and then
	// read-only. Upstream mounted Option.CWD (default os.Getwd()) read-write.
	if option.ReadOnlyDir != "" {
		modCfg = modCfg.WithFSConfig(wazero.NewFSConfig().WithReadOnlyDirMount(option.ReadOnlyDir, "/"))
	}
	if runtime.module, err = runtime.wrt.InstantiateModule(option.Context, compiled, modCfg); err != nil {
		return nil, fmt.Errorf("failed to instantiate module: %w", err)
	}

	stackSize := option.StackSize
	if stackSize == 0 {
		stackSize = DefaultStackSize
	}
	runtime.initializeRuntime()
	if err := runtime.relocateStack(stackSize); err != nil {
		return nil, fmt.Errorf("failed to relocate the JS stack: %w", err)
	}
	ok = true

	return runtime, nil
}

func (r *Runtime) Raw() uint64 {
	return r.handle.raw
}

// FreeQJSRuntime frees the QJS runtime.
func (r *Runtime) FreeQJSRuntime() {
	defer func() {
		err := AnyToError(recover())
		if err != nil {
			panic(fmt.Errorf("failed to free QJS runtime: %w", err))
		}
	}()

	r.context.freeOwnedValues()
	r.Call("QJS_Free", r.handle.raw)
}

// Mem returns the WebAssembly memory interface for this runtime.
func (r *Runtime) Mem() *Mem {
	return r.mem
}

// String returns a string representation of the runtime.
func (r *Runtime) String() string {
	return fmt.Sprintf("QJSRuntime: %p", r)
}

// Close cleanly shuts down the runtime and frees all associated resources.
func (r *Runtime) Close() {
	if r == nil {
		return
	}

	// A deadline may have already closed the guest module. JS teardown then
	// panics, but the native wazero store must still close and release its memory.
	// Preserve that panic for callers while making subsequent Close calls safe.
	defer func() {
		if r.module != nil {
			_ = r.module.Close(context.Background())
		}
		if r.wrt != nil {
			_ = r.wrt.Close(context.Background())
		}
		r.handle = nil
		r.module = nil
		r.wrt = nil
		if r.context != nil {
			r.context.handle = nil
			r.context.runtime = nil
			r.context.global = nil
			r.context.intrinsics = nil
			r.context.goFunctions = nil
			r.context = nil
		}
		if r.registry != nil {
			r.registry.Clear()
			r.registry = nil
		}
		r.malloc = nil
		r.free = nil
		r.mem = nil
	}()
	if r.handle != nil {
		r.FreeQJSRuntime()
	}

}

// Load executes a JavaScript file in the runtime's context.
func (r *Runtime) Load(file string, flags ...EvalOptionFunc) (*Value, error) {
	return r.context.Load(file, flags...)
}

// Eval executes JavaScript code in the runtime's context.
func (r *Runtime) Eval(file string, flags ...EvalOptionFunc) (*Value, error) {
	return r.context.Eval(file, flags...)
}

// Compile compiles JavaScript code to bytecode without executing it.
func (r *Runtime) Compile(file string, flags ...EvalOptionFunc) ([]byte, error) {
	return r.context.Compile(file, flags...)
}

// Context returns the JavaScript execution context for this runtime.
func (r *Runtime) Context() *Context {
	return r.context
}

// Call invokes a WebAssembly function by name with the given arguments.
func (r *Runtime) Call(name string, args ...uint64) *Handle {
	return NewHandle(r, r.call(name, args...))
}

// CallUnPack calls a WebAssembly function and unpacks the returned pointer.
func (r *Runtime) CallUnPack(name string, args ...uint64) (uint32, uint32) {
	h := r.Call(name, args...)
	defer h.Free()
	return r.mem.UnpackPtr(h.raw)
}

// Malloc allocates memory in the WebAssembly linear memory and return a pointer to it.
func (r *Runtime) Malloc(size uint64) uint64 {
	ptrs, err := r.malloc.Call(r.context, size)
	if err != nil {
		panic(fmt.Errorf("failed to allocate memory: %w", err))
	}

	return ptrs[0]
}

// FreeHandle releases memory allocated in WebAssembly linear memory.
func (r *Runtime) FreeHandle(ptr uint64) {
	if _, err := r.free.Call(r.context, ptr); err != nil {
		panic(fmt.Errorf("failed to free memory: %w", err))
	}
}

// FreeJsValue frees a JavaScript value in the QuickJS runtime.
func (r *Runtime) FreeJsValue(val uint64) {
	r.Call("QJS_FreeValue", r.context.Raw(), val)
}

// NewBytesHandle creates a handle for byte data in WebAssembly memory.
func (r *Runtime) NewBytesHandle(b []byte) *Handle {
	if len(b) == 0 {
		return nil
	}

	ptr := r.Malloc(uint64(len(b)))
	r.mem.MustWrite(uint32(ptr), b)

	return NewHandle(r, ptr)
}

// NewStringHandle creates a handle for string data with null termination.
func (r *Runtime) NewStringHandle(v string) *Handle {
	// Allocate len+1 for null terminator
	ptr := r.Malloc(uint64(len(v) + 1))

	// Write string data
	r.mem.MustWrite(uint32(ptr), []byte(v))
	// Add null terminator
	r.mem.MustWrite(uint32(ptr)+uint32(len(v)), []byte{0})

	return NewHandle(r, ptr)
}

// initializeRuntime sets up the runtime components after module instantiation.
func (r *Runtime) initializeRuntime() {
	r.malloc = r.module.ExportedFunction("malloc")
	r.free = r.module.ExportedFunction("free")
	r.mem = &Mem{mem: r.module.Memory()}
	r.handle = r.Call(
		"New_QJS",
		uint64(r.option.MemoryLimit),
		uint64(r.option.MaxStackSize),
		uint64(r.option.MaxExecutionTime),
		uint64(r.option.GCThreshold),
	)

	r.context.handle = r.Call("QJS_GetContext", r.handle.raw)
	r.context.runtime = r
	r.context.captureIntrinsics()
}

func (r *Runtime) call(name string, args ...uint64) uint64 {
	fn := r.module.ExportedFunction(name)
	if fn == nil {
		panic(fmt.Errorf("WASM function %s not found", name))
	}

	results, err := fn.Call(r.context, args...)
	if err != nil {
		stack := debug.Stack()
		panic(fmt.Errorf("failed to call %s: %w\nstack: %s", name, err, stack))
	}

	if len(results) == 0 {
		return 0
	}

	return results[0]
}

// Pool manages a collection of reusable QuickJS runtimes.
type Pool struct {
	pools      chan *Runtime
	size       int
	option     Option
	setupFuncs []func(*Runtime) error
	mu         sync.Mutex
}

// NewPool creates a new runtime pool with the specified size and configuration.
func NewPool(size int, option Option, setupFuncs ...func(*Runtime) error) *Pool {
	if size <= 0 {
		panic("pool size must be greater than 0")
	}

	p := &Pool{
		pools:      make(chan *Runtime, size),
		size:       size,
		option:     option,
		setupFuncs: setupFuncs,
	}

	return p
}

// Get returns a runtime from the pool or creates a new one if the pool is empty.
// The caller must call Put() to return the runtime when finished.
func (p *Pool) Get() (*Runtime, error) {
	if p == nil {
		return nil, errors.New("pool is nil")
	}

	// Try to get from pool first
	select {
	case rt := <-p.pools:
		return p.prepareRuntimeForUse(rt), nil
	// Pool is empty, need to create new runtime
	default:
	}

	// Double-check with lock to avoid race conditions
	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case rt := <-p.pools:
		return p.prepareRuntimeForUse(rt), nil
	default:
		return p.createNewRuntime()
	}
}

// Put returns the runtime back to the pool for reuse.
// If the pool is full, the runtime is closed to prevent resource leaks.
func (p *Pool) Put(rt *Runtime) {
	if rt == nil {
		return
	}

	// Update stack top before returning to pool
	if rt.handle != nil {
		rt.Call("QJS_UpdateStackTop", rt.handle.raw)
	}

	select {
	case p.pools <- rt:
		// Successfully returned to pool
	default:
		// Pool is full, close the runtime
		rt.Close()
	}
}

// prepareRuntimeForUse prepares a pooled runtime for use.
func (p *Pool) prepareRuntimeForUse(rt *Runtime) *Runtime {
	if rt != nil && rt.handle != nil {
		rt.Call("QJS_UpdateStackTop", rt.handle.raw)
	}

	return rt
}

// createNewRuntime creates a new runtime with setup functions applied.
func (p *Pool) createNewRuntime() (*Runtime, error) {
	rt, err := New(p.option)
	if err != nil {
		return nil, fmt.Errorf("failed to create new runtime: %w", err)
	}

	// Apply setup functions
	for i, setupFunc := range p.setupFuncs {
		err := setupFunc(rt)
		if err != nil {
			rt.Close()

			return nil, fmt.Errorf("setup function %d failed: %w", i, err)
		}
	}

	return rt, nil
}

// MemorySize returns linear memory bytes in O(1), without walking the JS heap.
func (r *Runtime) MemorySize() uint32 { return r.module.Memory().Size() }
