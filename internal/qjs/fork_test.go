package qjs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the flats fork changes (see NOTICE).

func safeEval(rt *Runtime, code string, flags ...EvalOptionFunc) (s string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	v, err := rt.Eval("t.js", append([]EvalOptionFunc{Code(code)}, flags...)...)
	if err != nil {
		return "", err
	}
	defer v.Free()
	return v.String(), nil
}

func TestForkNoFSByDefault(t *testing.T) {
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	for _, code := range []string{`String(std.loadFile("/etc/passwd"))`, `String(std.loadFile("go.mod"))`, `String(std.loadFile("fork_test.go"))`} {
		s, err := safeEval(rt, code)
		if err != nil || s != "null" {
			t.Errorf("%s = %q, %v; want null", code, s, err)
		}
	}
}

func TestForkReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "lib.js"), []byte("export const x = 41;"), 0o644)
	rt, err := New(Option{ReadOnlyDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if s, err := safeEval(rt, `std.loadFile("/a.txt")`); s != "hello" {
		t.Fatalf("read: %q %v", s, err)
	}
	s, err := safeEval(rt, `(() => { const f = std.open("/b.txt", "w"); return String(f); })()`)
	if _, statErr := os.Stat(filepath.Join(dir, "b.txt")); statErr == nil {
		t.Fatalf("guest wrote into the read-only mount (%q %v)", s, err)
	}
	v, err := func() (v *Value, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return rt.Eval("/main.js", Code(`import {x} from "./lib.js"; export default x + 1;`), TypeModule())
	}()
	if err != nil || v.Int32() != 42 {
		t.Fatalf("module import: %v", err)
	}
	v.Free()
	if s, _ := safeEval(rt, `String(std.loadFile("/../../../../etc/passwd"))`); s != "null" {
		t.Fatalf("escaped mount: %q", s)
	}
}

func TestForkCloseOnContextDonePerRuntime(t *testing.T) {
	// A first runtime WITHOUT CloseOnContextDone must not disable it for later ones.
	r0, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	r0.Close()
	// wazero reads the context from a per-call goroutine, so the deadline is
	// carried by a stable context rather than by assigning Context().Context.
	sc := &switchCtx{Context: context.Background()}
	rt, err := New(Option{Context: sc, CloseOnContextDone: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	sc.set(ctx)
	start := time.Now()
	_, err = safeEval(rt, `for(;;){}`)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("loop not interrupted: %v after %v", err, time.Since(start))
	}
}

func TestForkSleepHonoursContext(t *testing.T) {
	// os.sleep and os.setTimeout reach the host through WASI poll_oneoff;
	// the sleep must end when the context is done, not after the full delay.
	for _, code := range []string{`os.sleep(60000)`, `await new Promise((r) => os.setTimeout(r, 60000))`} {
		sc := &switchCtx{Context: context.Background()}
		rt, err := New(Option{Context: sc, CloseOnContextDone: true})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		sc.set(ctx)
		start := time.Now()
		_, err = safeEval(rt, code, FlagAsync())
		took := time.Since(start)
		cancel()
		if err == nil || took > 5*time.Second {
			t.Errorf("%s: not interrupted: %v after %v", code, err, took)
		}
		func() { defer func() { recover() }(); rt.Close() }()
	}
}

type switchCtx struct {
	context.Context
	mu sync.Mutex
}

func (s *switchCtx) set(c context.Context) { s.mu.Lock(); s.Context = c; s.mu.Unlock() }
func (s *switchCtx) cur() context.Context  { s.mu.Lock(); defer s.mu.Unlock(); return s.Context }
func (s *switchCtx) Done() <-chan struct{} { return s.cur().Done() }
func (s *switchCtx) Err() error            { return s.cur().Err() }

func TestForkMemoryLimitPages(t *testing.T) {
	rt, err := New(Option{MemoryLimitPages: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { recover() }()
	defer rt.Close()
	start := time.Now()
	_, err = safeEval(rt, `let a=[]; for(;;) a.push(new Array(100000).fill(1.5)); 0`)
	if err == nil {
		t.Fatal("no error")
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("took %v", d)
	}
	if size := rt.Mem().Size(); size > 512*65536 {
		t.Fatalf("memory grew to %d", size)
	}
	t.Logf("oom error after %v: %.200s", time.Since(start), err)
}

func TestForkStdoutRouting(t *testing.T) {
	var out bytes.Buffer
	rt, err := New(Option{Stdout: &out, Stderr: &out})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if _, err := safeEval(rt, `console.log("hi-there"); 1`); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hi-there") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestForkRelocatedStack(t *testing.T) {
	for _, tc := range []struct {
		code string
		want string
	}{
		{`const f = (n) => n >= 1000 ? 0 : f(n + 1) + 1; String(f(0))`, "1000"},
		{`const d = 5000; let x = JSON.parse("[".repeat(d) + "]".repeat(d)); let k = 0; while (x.length) { x = x[0]; k++; } String(k)`, "4999"},
	} {
		rt, err := New(Option{})
		if err != nil {
			t.Fatal(err)
		}
		s, err := safeEval(rt, tc.code)
		if err != nil || s != tc.want || !rt.StackIntact() {
			t.Errorf("%s = %q, %v (stack intact %v)", tc.code, s, err, rt.StackIntact())
		}
		if s, err := safeEval(rt, `[1,2,3].map((x) => x * 2).join(",")`); s != "2,4,6" {
			t.Errorf("runtime unusable afterwards: %q %v", s, err)
		}
		rt.Close()
	}
	// Overflowing the whole stack is detected by the canary.
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = safeEval(rt, `const f = (n) => f(n + 1) + 1; f(0)`)
	if err == nil || rt.StackIntact() {
		t.Fatalf("unbounded recursion: err=%v intact=%v", err, rt.StackIntact())
	}
	func() { defer func() { recover() }(); rt.Close() }()
}

func TestForkExportPatch(t *testing.T) {
	out, err := exportStackPointer(wasmBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(wasmBytes)+len(stackPointerExport)+3 {
		t.Fatalf("patched size %d (orig %d)", len(out), len(wasmBytes))
	}
	if _, err := exportStackPointer([]byte("\x00asm\x01\x00\x00\x00")); err == nil {
		t.Fatal("module without globals accepted")
	}
}
