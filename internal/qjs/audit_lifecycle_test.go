package qjs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func lifecyclePanic(fn func()) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = AnyToError(recovered)
		}
	}()

	fn()

	return nil
}

func lifecycleRequirePanic(t *testing.T, target error, fn func()) error {
	t.Helper()

	err := lifecyclePanic(fn)
	if err == nil {
		t.Fatalf("no panic, want %v", target)
	}

	if target != nil && !errors.Is(err, target) {
		t.Fatalf("panic %v, want %v", err, target)
	}

	return err
}

func lifecycleRuntime(t *testing.T, option Option) *Runtime {
	t.Helper()

	rt, err := New(option)
	if err != nil {
		t.Fatal(err)
	}

	return rt
}

func lifecycleCloseQuietly(rt *Runtime) {
	defer func() { recover() }()

	rt.Close()
}

// Every value, context and runtime method reached after Close must signal the
// closed runtime; only the free/teardown methods stay callable.
func TestClosedRuntimeSignalsRuntimeClosed(t *testing.T) {
	rt := lifecycleRuntime(t, Option{})
	c := rt.Context()
	value, err := c.Eval("value.js", Code(`({a:1})`))
	if err != nil {
		t.Fatal(err)
	}
	packed := c.runtime.NewStringHandle("packed")
	rt.Close()

	for name, call := range map[string]func() error{
		"Runtime.Eval":    func() error { _, err := rt.Eval("x.js", Code(`1`)); return err },
		"Runtime.Load":    func() error { _, err := rt.Load("x.js", Code(`1`)); return err },
		"Runtime.Compile": func() error { _, err := rt.Compile("x.js", Code(`1`)); return err },
		"Context.Eval":    func() error { _, err := c.Eval("x.js", Code(`1`)); return err },
		"Context.Load":    func() error { _, err := c.Load("x.js", Code(`1`)); return err },
		"Context.Compile": func() error { _, err := c.Compile("x.js", Code(`1`)); return err },
	} {
		if err := call(); !errors.Is(err, ErrRuntimeClosed) {
			t.Errorf("%s = %v, want ErrRuntimeClosed", name, err)
		}
	}

	for name, call := range map[string]func(){
		"Runtime.Raw":             func() { rt.Raw() },
		"Runtime.Call":            func() { rt.Call("JS_NewObject") },
		"Runtime.CallUnPack":      func() { rt.CallUnPack("QJS_GetContext") },
		"Runtime.Malloc":          func() { rt.Malloc(8) },
		"Runtime.Mem":             func() { rt.Mem() },
		"Runtime.MemorySize":      func() { rt.MemorySize() },
		"Context.Raw":             func() { c.Raw() },
		"Context.Call":            func() { c.Call("JS_NewObject") },
		"Context.Global":          func() { c.Global() },
		"Context.Malloc":          func() { c.Malloc(8) },
		"Context.MemRead":         func() { c.MemRead(1, 1) },
		"Context.MemWrite":        func() { c.MemWrite(1, []byte{0}) },
		"Context.NewStringHandle": func() { c.NewStringHandle("x") },
		"Value.String":            func() { _ = value.String() },
		"Value.IsObject":          func() { _ = value.IsObject() },
		"Handle.String":           func() { _ = packed.String() },
		"Handle.Bytes":            func() { _ = packed.Bytes() },
	} {
		if err := lifecyclePanic(call); err == nil {
			t.Errorf("%s: no panic", name)
		} else if !errors.Is(err, ErrRuntimeClosed) {
			t.Errorf("%s = %v, want ErrRuntimeClosed", name, err)
		}
	}

	if err := lifecyclePanic(func() {
		value.Free()
		packed.Free()
		packed.Free()
		c.FreeHandle(0)
		c.FreeJsValue(0)
		rt.FreeHandle(0)
		rt.FreeJsValue(0)
		rt.FreeQJSRuntime()
		rt.Close()
	}); err != nil {
		t.Fatalf("teardown after Close panicked: %v", err)
	}

	if rt.Context() != nil {
		t.Fatal("closed runtime still exposes a context")
	}

	if _, err := rt.Image(nil); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("Image after Close = %v, want ErrRuntimeClosed", err)
	}

	if err := rt.ImageEligible(); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("ImageEligible after Close = %v, want ErrRuntimeClosed", err)
	}
}

// A deadline closes the guest module; teardown during the unwind must not
// replace the timeout error, and the runtime must refuse further calls.
func TestDeadlineTeardownPreservesCause(t *testing.T) {
	sc := &switchCtx{Context: context.Background()}
	rt := lifecycleRuntime(t, Option{Context: sc, CloseOnContextDone: true})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	sc.set(ctx)

	start := time.Now()
	err := lifecyclePanic(func() { _, _ = rt.Eval("loop.js", Code(`for(;;){}`)) })
	if err == nil {
		t.Fatal("infinite loop returned without an error")
	}

	if !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("timeout cause lost: %v", err)
	}

	if strings.Contains(err.Error(), "failed to free memory") {
		t.Fatalf("teardown masked the timeout: %v", err)
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("loop was not interrupted promptly: %v", elapsed)
	}

	if _, err := rt.Eval("x.js", Code(`1`)); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("Eval after the deadline = %v, want ErrRuntimeClosed", err)
	}

	if err := rt.ImageEligible(); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("ImageEligible after the deadline = %v, want ErrRuntimeClosed", err)
	}

	closeErr := lifecyclePanic(rt.Close)
	if closeErr == nil || !strings.Contains(closeErr.Error(), "failed to free QJS runtime") {
		t.Fatalf("teardown error not preserved: %v", closeErr)
	}

	rt.Close()
}

func TestMemBoundsAndZeroLength(t *testing.T) {
	rt := lifecycleRuntime(t, Option{})
	defer rt.Close()
	mem := rt.Mem()
	size := mem.Size()
	if size == 0 {
		t.Fatal("empty memory")
	}

	zero, err := mem.Read(0, 0)
	if err != nil || len(zero) != 0 {
		t.Fatalf("Read(0,0) = %v, %v; want empty", zero, err)
	}

	if err := mem.Write(0, nil); err != nil {
		t.Fatalf("Write(0,nil) = %v", err)
	}

	if s, err := mem.ReadString(0, 0); s != "" || err != nil {
		t.Fatalf("ReadString(0,0) = %q, %v", s, err)
	}

	for name, tc := range map[string]struct {
		err  error
		call func() error
	}{
		"Read null":             {ErrInvalidPointer, func() error { _, err := mem.Read(0, 1); return err }},
		"ReadUint8 null":        {ErrInvalidPointer, func() error { _, err := mem.ReadUint8(0); return err }},
		"Read past end":         {ErrIndexOutOfRange, func() error { _, err := mem.Read(size-4, 8); return err }},
		"Read wrapping address": {ErrIndexOutOfRange, func() error { _, err := mem.Read(math.MaxUint32-3, 8); return err }},
		"Read max size":         {ErrIndexOutOfRange, func() error { _, err := mem.Read(1, math.MaxUint32); return err }},
		"WriteUint64 past end":  {ErrIndexOutOfRange, func() error { return mem.WriteUint64(size-2, 0) }},
		"ReadString null":       {ErrInvalidPointer, func() error { _, err := mem.ReadString(0, 8); return err }},
		"ReadString at end":     {ErrIndexOutOfRange, func() error { _, err := mem.ReadString(size, 8); return err }},
		"WriteString past end":  {ErrIndexOutOfRange, func() error { return mem.WriteString(size-2, "abc") }},
	} {
		if err := tc.call(); !errors.Is(err, tc.err) {
			t.Errorf("%s = %v, want %v", name, err, tc.err)
		}
	}

	lifecycleRequirePanic(t, ErrIndexOutOfRange, func() { mem.UnpackPtr(uint64(size - 4)) })
	lifecycleRequirePanic(t, ErrIndexOutOfRange, func() { mem.StringFromPackedPtr(uint64(size - 4)) })

	ptr := rt.Malloc(8)
	defer rt.FreeHandle(ptr)
	mem.MustWrite(uint32(ptr), []byte("abc\x00"))
	for _, tc := range []struct {
		maxlen uint32
		want   string
		err    error
	}{
		{4, "abc", nil},
		{3, "abc", nil},
		{2, "", ErrNoNullTerminator},
		{math.MaxUint32, "abc", nil},
	} {
		got, err := mem.ReadString(uint32(ptr), tc.maxlen)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("ReadString(maxlen %d) = %q, %v; want %q, %v", tc.maxlen, got, err, tc.want, tc.err)
		}
	}
}

// A Handle wraps either a JSValue or a malloc'd packed record. Feeding it a
// truncated record, a null payload or an out-of-range payload must fail with
// a descriptive error and must not free a pointer it never read.
func TestPackedHandleBounds(t *testing.T) {
	rt := lifecycleRuntime(t, Option{})
	defer rt.Close()
	mem := rt.Mem()
	size := mem.Size()

	nullRecord := rt.Malloc(8)
	defer rt.FreeHandle(nullRecord)
	if err := mem.WriteUint64(uint32(nullRecord), 32); err != nil {
		t.Fatal(err)
	}
	nullPayload := NewHandle(rt, nullRecord)
	lifecycleRequirePanic(t, ErrInvalidPointer, func() { _ = nullPayload.Bytes() })
	lifecycleRequirePanic(t, ErrInvalidPointer, func() { _ = nullPayload.String() })

	truncated := NewHandle(rt, uint64(size-4))
	lifecycleRequirePanic(t, ErrIndexOutOfRange, func() { _ = truncated.Bytes() })
	lifecycleRequirePanic(t, ErrIndexOutOfRange, func() { _ = truncated.String() })

	record := rt.Malloc(8)
	defer rt.FreeHandle(record)
	if err := mem.WriteUint32(uint32(record), 1<<20); err != nil {
		t.Fatal(err)
	}

	if err := mem.WriteUint32(uint32(record)+4, size); err != nil {
		t.Fatal(err)
	}

	lifecycleRequirePanic(t, ErrIndexOutOfRange, func() { _ = NewHandle(rt, record).Bytes() })

	if _, err := mem.ReadUint32(uint32(record)); err != nil {
		t.Fatalf("runtime broken after refused handle reads: %v", err)
	}
}

func TestAllocationRefusesOversizedAndExhausted(t *testing.T) {
	rt := lifecycleRuntime(t, Option{})
	defer rt.Close()
	if ptr := rt.Malloc(0); ptr == 0 {
		t.Fatal("Malloc(0) returned 0")
	}

	for _, size := range []uint64{1 << 32, 1 << 40, math.MaxUint64} {
		err := lifecycleRequirePanic(t, nil, func() { rt.Malloc(size) })
		if !strings.Contains(err.Error(), "address space") {
			t.Errorf("Malloc(%d) = %v, want an address-space error", size, err)
		}
	}

	small := lifecycleRuntime(t, Option{MemoryLimitPages: 64})
	defer lifecycleCloseQuietly(small)
	lifecycleRequirePanic(t, ErrOutOfMemory, func() { small.NewBytesHandle(make([]byte, 8<<20)) })

	if _, err := New(Option{MemoryLimitPages: 65537}); err == nil || !strings.Contains(err.Error(), "memoryLimitPages") {
		t.Fatalf("New with 65537 pages = %v", err)
	}
}

func TestStackSizeBelowCanaryIsRefused(t *testing.T) {
	for _, size := range []uint32{1, 4096, stackCanarySize - 1, stackCanarySize} {
		if _, err := New(Option{StackSize: size}); err == nil || !strings.Contains(err.Error(), "canary") {
			t.Errorf("StackSize %d: err = %v, want a canary error", size, err)
		}
	}

	rt := lifecycleRuntime(t, Option{StackSize: stackCanarySize + 1})
	lifecycleCloseQuietly(rt)
}

func TestNegativeQuickJSLimitsAreRefused(t *testing.T) {
	for name, option := range map[string]Option{
		"MemoryLimit":      {MemoryLimit: -1},
		"MaxStackSize":     {MaxStackSize: -1},
		"MaxExecutionTime": {MaxExecutionTime: -1},
		"GCThreshold":      {GCThreshold: -1},
	} {
		if _, err := New(option); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("New(%s: -1) = %v, want an error naming %s", name, err, name)
		}
	}
}

// A thrown value need not be an Error. Formatting it must not panic, must not
// recurse through a throwing conversion, and must leave no pending exception.
func TestNonErrorThrowsFormatAndClear(t *testing.T) {
	for name, tc := range map[string]struct {
		code string
		want string
	}{
		"null":            {`throw null`, "null"},
		"undefined":       {`throw undefined`, "undefined"},
		"symbol":          {`throw Symbol("boom")`, "Symbol(boom)"},
		"number":          {`throw 42`, "42"},
		"plain object":    {`throw {a:1}`, "[object Object]"},
		"throwing stack":  {`throw {get stack(){throw new Error("stack failed")}}`, "[object Object]"},
		"throwing string": {`throw {toString(){throw new Error("toString failed")}}`, "unprintable object"},
		"throwing proxy":  {`throw new Proxy({}, {get(){throw new Error("get failed")}})`, "unprintable object"},
	} {
		t.Run(name, func(t *testing.T) {
			rt := lifecycleRuntime(t, Option{})
			defer rt.Close()
			c := rt.Context()

			_, err := c.Eval("throw.js", Code(tc.code))
			if err == nil {
				t.Fatal("no error")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}

			if c.HasException() {
				t.Fatal("pending exception left behind")
			}

			recovered, err := safeEval(rt, `1+1`)
			if err != nil || recovered != "2" {
				t.Fatalf("runtime poisoned: %q, %v", recovered, err)
			}
		})
	}
}

// Strings keep embedded NULs in both directions; the C-string entry points
// that really truncate at NUL refuse them instead of silently taking a prefix.
func TestNULStringHandling(t *testing.T) {
	rt := lifecycleRuntime(t, Option{})
	defer rt.Close()
	c := rt.Context()

	value := c.NewString("a\x00b")
	defer value.Free()
	if got := value.String(); got != "a\x00b" {
		t.Fatalf("NewString round trip = %q", got)
	}

	if got := c.NewString("").String(); got != "" {
		t.Fatalf("empty string = %q", got)
	}

	if _, err := c.Eval("nul.js", Code("1+1;\x00throw new Error('x')")); !errors.Is(err, ErrStringContainsNUL) {
		t.Fatalf("Eval with NUL in code = %v, want ErrStringContainsNUL", err)
	}

	lifecycleRequirePanic(t, ErrStringContainsNUL, func() { c.ParseJSON("{\"a\":\"x\x00y\"}") })
	lifecycleRequirePanic(t, ErrStringContainsNUL, func() { c.NewAtom("a\x00b") })
	obj := c.NewObject()
	defer obj.Free()
	lifecycleRequirePanic(t, ErrStringContainsNUL, func() { obj.GetPropertyStr("a\x00b") })
	lifecycleRequirePanic(t, ErrStringContainsNUL, func() { obj.SetPropertyStr("a\x00b", c.NewInt32(1)) })

	jsString, err := c.Eval("nul.js", Code(`"a\u0000b"`))
	if err != nil {
		t.Fatal(err)
	}
	defer jsString.Free()
	if got := jsString.String(); got != "a\x00b" {
		t.Fatalf("JS string with NUL = %q", got)
	}

	object, err := c.Eval("keys.js", Code(`({"a\u0000b": 1})`))
	if err != nil {
		t.Fatal(err)
	}
	defer object.Free()
	keys, err := object.GetOwnPropertyNames()
	if err != nil || len(keys) != 1 || keys[0] != "a\x00b" {
		t.Fatalf("atom with NUL = %q, %v", keys, err)
	}

	recovered, err := safeEval(rt, `1+1`)
	if err != nil || recovered != "2" {
		t.Fatalf("runtime poisoned: %q, %v", recovered, err)
	}
}

// The read-only mount must not follow a symlink out of the mounted directory,
// with or without the counting wrapper.
func TestMountSymlinkConfinement(t *testing.T) {
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "inside.txt"), []byte("inside"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "ok.txt"), []byte("ok"), 0o644)
	for link, target := range map[string]string{
		"out":        outside,
		"abs":        "/etc/passwd",
		"in":         filepath.Join(dir, "inside.txt"),
		"dirlink":    dir,
		"sub/escape": outside,
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}

	for name, option := range map[string]Option{
		"plain":         {ReadOnlyDir: dir},
		"census":        {ReadOnlyDir: dir, Census: &Census{}},
		"symlinked dir": {ReadOnlyDir: filepath.Join(dir, "dirlink")},
	} {
		t.Run(name, func(t *testing.T) {
			rt := lifecycleRuntime(t, option)
			defer rt.Close()

			if got, err := safeEval(rt, `String(std.loadFile("/inside.txt"))`); got != "inside" || err != nil {
				t.Fatalf("inside file = %q, %v", got, err)
			}

			if got, err := safeEval(rt, `String(std.loadFile("/in"))`); got != "inside" || err != nil {
				t.Fatalf("in-mount symlink = %q, %v", got, err)
			}

			if got, err := safeEval(rt, `String(std.loadFile("/sub/ok.txt"))`); got != "ok" || err != nil {
				t.Fatalf("subdirectory file = %q, %v", got, err)
			}

			for _, path := range []string{"/out", "/abs", "/sub/escape", "/../../../../etc/passwd"} {
				if got, err := safeEval(rt, `String(std.loadFile("`+path+`"))`); got != "null" || err != nil {
					t.Errorf("%s escaped the mount: %q, %v", path, got, err)
				}
			}
		})
	}

	if _, err := New(Option{ReadOnlyDir: filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("mounted a missing directory")
	}

	file := filepath.Join(dir, "inside.txt")
	if _, err := New(Option{ReadOnlyDir: file}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("mounted a regular file: %v", err)
	}
}

func TestPropertySetFailuresDoNotPoison(t *testing.T) {
	rt := lifecycleRuntime(t, Option{})
	defer rt.Close()
	c := rt.Context()

	proxy, err := c.Eval("proxy.js", Code(`new Proxy({}, {set(){ throw new Error("set failed") }})`))
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Free()

	key := c.NewString("c")
	defer key.Free()
	for name, call := range map[string]func(){
		"SetPropertyStr":   func() { proxy.SetPropertyStr("b", c.NewInt32(1)) },
		"SetProperty":      func() { proxy.SetProperty(key, c.NewInt32(2)) },
		"SetPropertyIndex": func() { proxy.SetPropertyIndex(0, c.NewInt32(3)) },
	} {
		err := lifecycleRequirePanic(t, nil, call)
		if !strings.Contains(err.Error(), "set failed") {
			t.Errorf("%s = %v, want the set trap error", name, err)
		}

		if c.HasException() {
			t.Errorf("%s left a pending exception", name)
		}
	}

	frozen, err := c.Eval("frozen.js", Code(`Object.freeze({a:1})`))
	if err != nil {
		t.Fatal(err)
	}
	defer frozen.Free()
	lifecyclePanic(func() { frozen.SetPropertyStr("b", c.NewInt32(4)) })

	if recovered, err := safeEval(rt, `1+1`); err != nil || recovered != "2" {
		t.Fatalf("runtime poisoned: %q, %v", recovered, err)
	}
}

func TestPoolRefusesClosedRuntimes(t *testing.T) {
	pool := NewPool(1, Option{})
	rt, err := pool.Get()
	if err != nil {
		t.Fatal(err)
	}
	rt.Close()
	pool.Put(rt)
	pool.Put(nil)

	reused, err := pool.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer reused.Close()
	if got, err := safeEval(reused, `1+1`); got != "2" || err != nil {
		t.Fatalf("pool returned a closed runtime: %q, %v", got, err)
	}
}

// Images are CRC-checked and then semantically validated; a crafted header
// must be refused before it reaches restore.
func TestImageFieldValidation(t *testing.T) {
	rt := lifecycleRuntime(t, Option{})
	img, err := rt.Image(nil)
	if err != nil {
		t.Fatal(err)
	}
	rt.Close()
	page := img.memSize / imagePageSize

	mutations := map[string]func(*Image){
		"zero memory":       func(i *Image) { i.memSize = 0 },
		"partial page":      func(i *Image) { i.memSize++ },
		"zero random":       func(i *Image) { i.randomAddr = 0 },
		"random past end":   func(i *Image) { i.randomAddr = i.memSize - 4 },
		"invalid kind":      func(i *Image) { i.kinds = append(i.kinds, 'z') },
		"page out of range": func(i *Image) { i.pages = []imagePage{{index: page, data: make([]byte, imagePageSize)}} },
		"duplicate page": func(i *Image) {
			i.pages = []imagePage{{index: 0, data: make([]byte, imagePageSize)}, {index: 0, data: make([]byte, imagePageSize)}}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			broken := *img
			broken.pages = slices.Clone(img.pages)
			mutate(&broken)
			serialized, err := broken.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}

			if _, err := UnmarshalImage(serialized); !errors.Is(err, ErrImageCorrupt) {
				t.Fatalf("UnmarshalImage = %v, want ErrImageCorrupt", err)
			}
		})
	}

	for name, mutate := range map[string]func(*Image){
		"random past end": func(i *Image) { i.randomAddr = i.memSize },
		"smaller memory":  func(i *Image) { i.memSize = imagePageSize },
	} {
		t.Run("restore "+name, func(t *testing.T) {
			broken := *img
			mutate(&broken)
			if _, err := New(Option{Image: &broken}); err == nil {
				t.Fatal("restored an invalid image")
			}
		})
	}
}

// One Runtime serves one goroutine at a time. Serialized use from many
// goroutines is supported; unserialized use is not and crashes the wazero
// call engine with a fatal (unrecoverable) error, so its reproduction runs
// only when explicitly requested.
func TestConcurrentRuntimeUseRequiresSerialization(t *testing.T) {
	if os.Getenv("QJS_CONCURRENCY_CRASH_REPRO") != "" {
		concurrentRuntimeUseRepro(t)
		return
	}

	t.Skip("set QJS_CONCURRENCY_CRASH_REPRO=1 to reproduce: unserialized concurrent calls into one Runtime abort the process (fatal error: runtime: split stack overflow / unsafe.Slice: len out of range) instead of panicking")
}

func concurrentRuntimeUseRepro(t *testing.T) {
	rt := lifecycleRuntime(t, Option{Context: context.Background(), CloseOnContextDone: true})
	defer func() { recover() }()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				_, _ = rt.Eval("c.js", Code(`[1,2].join(",")`))
			}
		})
	}
	wg.Wait()
	rt.Close()
}

func TestConcurrentRuntimeUseSerialized(t *testing.T) {
	rt := lifecycleRuntime(t, Option{})
	defer rt.Close()
	c := rt.Context()

	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 25 {
				want := fmt.Sprintf("%d,%d", i, j)
				mu.Lock()
				v, err := c.Eval("c.js", Code(fmt.Sprintf(`[%d,%d].join(",")`, i, j)))
				if err != nil {
					mu.Unlock()
					t.Errorf("goroutine %d iteration %d: %v", i, j, err)
					return
				}

				got := v.String()
				v.Free()
				mu.Unlock()
				if got != want {
					t.Errorf("goroutine %d iteration %d = %q, want %q", i, j, got, want)
					return
				}
			}
		})
	}
	wg.Wait()
}

// Invalid JSON used to return the exception sentinel and leave the error
// pending, so the next unrelated call failed. It now panics with the syntax
// error and clears it.
func TestParseJSONFailureDoesNotPoison(t *testing.T) {
	rt := lifecycleRuntime(t, Option{})
	defer rt.Close()
	c := rt.Context()

	err := lifecycleRequirePanic(t, nil, func() { c.ParseJSON("{") })
	if !strings.Contains(err.Error(), "SyntaxError") {
		t.Fatalf("ParseJSON error = %v, want a SyntaxError", err)
	}

	if c.HasException() {
		t.Fatal("ParseJSON left a pending exception")
	}

	object := c.ParseJSON(`{"a":1}`)
	defer object.Free()
	names, err := object.GetOwnPropertyNames()
	if err != nil || len(names) != 1 || names[0] != "a" {
		t.Fatalf("valid JSON after a failure = %q, %v", names, err)
	}

	recovered, err := safeEval(rt, `1+1`)
	if err != nil || recovered != "2" {
		t.Fatalf("runtime poisoned: %q, %v", recovered, err)
	}
}
