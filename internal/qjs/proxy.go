package qjs

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero/api"
)

// ProxyRegistry stores Go functions that can be called from JavaScript.
// It provides thread-safe registration and retrieval of functions with automatic ID generation.
type ProxyRegistry struct {
	nextID  uint64
	mu      sync.RWMutex
	proxies map[uint64]any
}

// NewProxyRegistry creates a new thread-safe proxy registry.
func NewProxyRegistry() *ProxyRegistry {
	return &ProxyRegistry{
		proxies: make(map[uint64]any),
	}
}

// Register adds a function to the registry and returns its unique ID.
// This method is thread-safe and can be called concurrently.
func (r *ProxyRegistry) Register(fn any) uint64 {
	if fn == nil {
		return 0
	}

	id := atomic.AddUint64(&r.nextID, 1)
	r.mu.Lock()
	r.proxies[id] = fn
	r.mu.Unlock()

	return id
}

// Get retrieves a function by its ID.
// Returns the function and true if found, nil and false otherwise.
// This method is thread-safe and can be called concurrently.
func (r *ProxyRegistry) Get(id uint64) (any, bool) {
	if id == 0 {
		return nil, false
	}

	r.mu.RLock()
	fn, ok := r.proxies[id]
	r.mu.RUnlock()

	return fn, ok
}

// Unregister removes a function from the registry by its ID.
// Returns true if the function was found and removed, false otherwise.
// This method is thread-safe and can be called concurrently.
func (r *ProxyRegistry) Unregister(id uint64) bool {
	if id == 0 {
		return false
	}

	r.mu.Lock()

	_, exists := r.proxies[id]
	if exists {
		delete(r.proxies, id)
	}

	r.mu.Unlock()

	return exists
}

// Len returns the number of registered functions.
// This method is thread-safe.
func (r *ProxyRegistry) Len() int {
	r.mu.RLock()
	count := len(r.proxies)
	r.mu.RUnlock()

	return count
}

// Clear removes all registered functions from the registry.
// This method is thread-safe.
func (r *ProxyRegistry) Clear() {
	r.mu.Lock()
	r.proxies = make(map[uint64]any)
	r.mu.Unlock()
}

// JsFunctionProxy is the Go host function that will be imported by the WASM module.
// It corresponds to the following C declaration:
//
//	__attribute__((import_module("env"), import_name("jsFunctionProxy")))
//	extern JSValue jsFunctionProxy(JSContext *ctx, JSValueConst this, int argc, JSValueConst *argv);
//
// Parameters:
//   - ctx: 	JSContext pointer
//   - this: 	JSValueConst this (the "this" value)
//   - argc: 	int argc (number of arguments)
//   - argv: 	pointer to argv (an array of JSValueConst/JSValue, each 8 bytes - uint64)
type JsFunctionProxy = func(
	ctx context.Context,
	module api.Module,
	jsCtx uint32,
	thisVal uint64,
	argc uint32,
	argv uint32,
) (rs uint64)

// createFuncProxyWithRegistry creates a WASM function proxy that bridges JavaScript function calls to Go functions.
// It handles parameter extraction, error recovery, and result conversion between JS and Go.
func createFuncProxyWithRegistry(registry *ProxyRegistry) JsFunctionProxy {
	return func(
		_ context.Context,
		module api.Module,
		_ uint32,
		thisVal uint64,
		argc uint32,
		argv uint32,
	) (rs uint64) {
		mem := module.Memory()
		var this *This
		defer func() {
			if this != nil {
				for _, arg := range this.args {
					arg.Free()
				}
			}
		}()

		defer func() {
			if r := recover(); r != nil {
				rs = recoverProxyCall(this, r)
			}
		}()

		goFunc, goContext, parsed, err := getProxyFuncParams(registry, mem, thisVal, argc, argv)
		if err != nil {
			if goContext != nil {
				return goContext.ThrowError(err).Raw()
			}
			return 0
		}
		this = parsed

		result, err := goFunc(this)
		if err != nil {
			return this.context.ThrowError(err).Raw()
		}

		// A callback may return one of its borrowed arguments. Transfer a
		// separate reference before argument cleanup runs.
		if result != nil {
			for _, arg := range this.args {
				if result.Raw() != 0 && result.Raw() == arg.Raw() {
					result = result.Clone()
					break
				}
			}
		}
		return validateAndReturnResult(this, result)
	}
}

// recoverProxyCall converts a panic anywhere in the proxy call into a JS
// exception. this is nil only when parsing failed before the context was
// recovered; the defined host result for that case is raw 0.
func recoverProxyCall(this *This, r any) uint64 {
	if this == nil || this.context == nil {
		return 0
	}
	return handlePanicRecovery(this, r)
}

// handlePanicRecovery handles panics during Go function execution.
func handlePanicRecovery(this *This, r any) uint64 {
	recoveredErr := newProxyErr(0, r)
	if this.isAsync && this.promise != nil {
		errVal := this.context.NewError(recoveredErr)
		rejectErr := this.promise.Reject(errVal)

		return this.context.ThrowError(combineErrors(recoveredErr, rejectErr)).Raw()
	}

	return this.context.ThrowError(recoveredErr).Raw()
}

// validateAndReturnResult validates the function result and handles JavaScript exceptions.
func validateAndReturnResult(this *This, result *Value) uint64 {
	if this.context.HasException() {
		panic(this.context.Exception())
	}

	if result == nil {
		return this.context.NewUndefined().Raw()
	}

	return result.Raw()
}

// getProxyFuncParams extracts and validates parameters for proxy function calls.
// It parses WASM memory to extract function ID, context, arguments, and async
// information. On failure the resolved context is returned with the error so
// the caller can still throw through it.
func getProxyFuncParams(
	registry *ProxyRegistry,
	mem api.Memory,
	thisRef uint64,
	argc uint32,
	argv uint32,
) (Function, *Context, *This, error) {
	args, err := readArgsFromWasmMem(mem, argc, argv)
	if err != nil {
		return nil, nil, nil, err
	}
	functionHandle := args[0]  // The first argument is the function ID
	jsContextHandle := args[1] // The second argument is the context ID
	isAsync := args[2]         // The third argument is the async flag

	goFunc, goContext := retrieveGoResources(registry, functionHandle, jsContextHandle)
	if goContext == nil {
		return nil, nil, nil, fmt.Errorf("jsFunctionProxy: unknown context handle %d", jsContextHandle)
	}
	if goFunc == nil {
		return nil, goContext, nil, fmt.Errorf("jsFunctionProxy: unknown function handle %d", functionHandle)
	}
	promise := extractPromiseIfAsync(goContext, args, isAsync)
	fnArgs := extractFunctionArguments(goContext, args)
	this := createThisContext(goContext, thisRef, fnArgs, promise, isAsync)

	return goFunc, goContext, this, nil
}

// extractPromiseIfAsync extracts the promise handle for async functions.
func extractPromiseIfAsync(context *Context, args []uint64, isAsync uint64) *Value {
	if isAsync == 0 {
		return context.NewUndefined()
	}

	promiseHandle := args[3]

	return context.NewValue(NewHandle(context.runtime, promiseHandle))
}

// extractFunctionArguments extracts and clones function arguments from WASM memory.
func extractFunctionArguments(context *Context, args []uint64) []*Value {
	fnArgs := make([]*Value, len(args)-4)
	for i := range fnArgs {
		argHandle := args[4+i]
		arg := context.NewValue(NewHandle(context.runtime, argHandle))
		fnArgs[i] = arg.Clone()
	}

	return fnArgs
}

// createThisContext creates the This context for function execution.
func createThisContext(context *Context, thisRef uint64, args []*Value, promise *Value, isAsync uint64) *This {
	thisValue := context.NewValue(NewHandle(context.runtime, thisRef))
	this := &This{
		context: context,
		Value:   thisValue,
		args:    args,
		promise: promise,
		isAsync: isAsync != 0,
	}

	return this
}

// retrieveGoResources retrieves and validates Go function and context from the registry.
func retrieveGoResources(
	registry *ProxyRegistry,
	functionHandle uint64,
	jsContextHandle uint64,
) (Function, *Context) {
	fn, _ := registry.Get(functionHandle)
	goFunc, _ := fn.(Function)
	jsContext, _ := registry.Get(jsContextHandle)
	goContext, _ := jsContext.(*Context)

	return goFunc, goContext
}

// maxProxyArgs caps the guest-supplied argument count. Real argument lists
// are small; the header alone carries 4 slots. 1<<16 bounds a single read to
// 512 KiB no matter how large the guest memory is.
const maxProxyArgs = 1 << 16

// readArgsFromWasmMem reads the argument array from WASM memory with proper
// validation. The header carries function ID, context ID, async flag and
// promise handle, so argc must be at least 4; each argument is 8 bytes
// (uint64) little-endian.
func readArgsFromWasmMem(mem api.Memory, argc uint32, argv uint32) ([]uint64, error) {
	const headerArgs = 4
	if argc < headerArgs {
		return nil, fmt.Errorf("jsFunctionProxy: argc %d < %d", argc, headerArgs)
	}
	if argc > maxProxyArgs {
		return nil, fmt.Errorf("jsFunctionProxy: too many arguments")
	}
	if uint64(argc) > uint64(math.MaxUint32-argv)/8 {
		return nil, fmt.Errorf("jsFunctionProxy: argv range overflows")
	}
	byteCount := argc * 8
	data, ok := mem.Read(argv, byteCount)
	if !ok {
		return nil, fmt.Errorf("jsFunctionProxy: argv %d..%d out of memory", argv, uint64(argv)+uint64(byteCount))
	}
	args := make([]uint64, argc)
	for i := range args {
		args[i] = binary.LittleEndian.Uint64(data[i*8 : i*8+8])
	}
	return args, nil
}
