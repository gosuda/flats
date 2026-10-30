package qjs

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// memModule instantiates a minimal wasm module declaring one memory page, so
// tests run against a real guest memory rather than a hand-rolled fake.
func memModule(t *testing.T) (api.Module, api.Memory) {
	t.Helper()
	// (module (memory 1))
	bin := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x05, 0x03, 0x01, 0x00, 0x01}
	r := wazero.NewRuntime(t.Context())
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	mod, err := r.Instantiate(t.Context(), bin)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	return mod, mod.Memory()
}

func TestReadArgsRejectsShortArgc(t *testing.T) {
	_, mem := memModule(t)
	for _, argc := range []uint32{0, 1, 3} {
		if _, err := readArgsFromWasmMem(mem, argc, 0); err == nil {
			t.Fatalf("argc %d: want error", argc)
		}
	}
}

func TestReadArgsRejectsOutOfRangeArgv(t *testing.T) {
	_, mem := memModule(t)
	// argv 65505 + 32-byte read overruns the 65536-byte page.
	if _, err := readArgsFromWasmMem(mem, 4, 65505); err == nil {
		t.Fatal("want out-of-memory error")
	}
	// Arithmetic-overflow argv: the old code wrapped argv + i*8.
	if _, err := readArgsFromWasmMem(mem, 4, 0xFFFFFFF0); err == nil {
		t.Fatal("want overflow error")
	}
}

func TestReadArgsReadsValidLayout(t *testing.T) {
	_, mem := memModule(t)
	if !mem.WriteUint32Le(0, 7) || !mem.WriteUint32Le(8, 9) || !mem.WriteUint32Le(16, 1) || !mem.WriteUint32Le(24, 11) {
		t.Fatal("mem writes failed")
	}
	args, err := readArgsFromWasmMem(mem, 5, 0)
	if err != nil || len(args) != 5 || args[0] != 7 || args[1] != 9 || args[2] != 1 || args[3] != 11 {
		t.Fatalf("args=%v err=%v", args, err)
	}
}

// The proxy entry must return a defined host result, not panic, when the
// guest supplies malformed argv.
func TestProxyCallMalformedArgvDoesNotPanic(t *testing.T) {
	mod, _ := memModule(t)
	registry := NewProxyRegistry()
	proxy := createFuncProxyWithRegistry(registry)

	// argc < 4: defined result, no panic, no context to throw through.
	if rs := proxy(t.Context(), mod, 0, 0, 0, 0); rs != 0 {
		t.Fatalf("short argc rs=%d, want 0", rs)
	}
	// argv out of bounds: same defined result.
	if rs := proxy(t.Context(), mod, 0, 0, 4, 0xFFFFFFF0); rs != 0 {
		t.Fatalf("bad argv rs=%d, want 0", rs)
	}
}

// Unknown handles are reported; a resolvable context receives the throw.
func TestGetProxyFuncParamsUnknownHandles(t *testing.T) {
	_, mem := memModule(t)
	registry := NewProxyRegistry()
	registry.Register(&Context{}) // context handle id 1
	put := func(off uint32, v uint64) {
		if !mem.WriteUint64Le(off, v) {
			t.Fatalf("mem write at %d", off)
		}
	}
	put(0, 99) // unknown function handle
	put(8, 1)  // known context handle
	_, ctxOut, _, err := getProxyFuncParams(registry, mem, 0, 4, 0)
	if err == nil || ctxOut == nil {
		t.Fatalf("want function error with resolved context, got err=%v ctx=%v", err, ctxOut)
	}
	// Unknown context reports and resolves nothing.
	put(8, 77)
	_, ctxOut, _, err = getProxyFuncParams(registry, mem, 0, 4, 0)
	if err == nil || ctxOut != nil {
		t.Fatalf("want context error and nil context, got err=%v ctx=%v", err, ctxOut)
	}
}
