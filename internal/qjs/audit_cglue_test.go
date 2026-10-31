package qjs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Bytecode that throws when it runs must fail the same way every time: the
// execution path owns the function object it hands to JS_EvalFunction, so the
// first failure must not free it a second time and corrupt the guest heap.
func TestBytecodeFailureKeepsRuntimeUsable(t *testing.T) {
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	global, err := rt.Compile("boom.js", Code(`throw new Error("boom")`))
	if err != nil {
		t.Fatalf("compile global: %v", err)
	}
	module, err := rt.Compile("boom.js", Code(`throw new Error("boom")`), TypeModule())
	if err != nil {
		t.Fatalf("compile module: %v", err)
	}

	for i := range 3 {
		if _, err := rt.Eval("boom.js", Bytecode(global)); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("global bytecode run %d: %v", i, err)
		}
		if _, err := rt.Eval("boom.js", Bytecode(module), TypeModule()); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("module bytecode run %d: %v", i, err)
		}
	}

	if s, err := safeEval(rt, `1 + 1`); err != nil || s != "2" {
		t.Fatalf("runtime unusable after bytecode failures: %q %v", s, err)
	}
}

// A numeric conversion that the guest rejects (here a symbol and a throwing
// valueOf) returns zero for the requested Go type, and must clear its pending
// exception: a stale exception makes the next unrelated call fail.
func TestFailedNumericConversionLeavesNoException(t *testing.T) {
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	for _, code := range []string{
		`Symbol("s")`,
		`({ valueOf() { throw new Error("nope"); } })`,
	} {
		v, err := rt.Context().Eval("conversion.js", Code(code))
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		if got := v.Int64(); got != 0 {
			t.Errorf("%s: Int64 = %d, want 0", code, got)
		}
		if got := v.Int32(); got != 0 {
			t.Errorf("%s: Int32 = %d, want 0", code, got)
		}
		if got := v.Uint32(); got != 0 {
			t.Errorf("%s: Uint32 = %d, want 0", code, got)
		}
		if got := v.Float64(); got != 0 {
			t.Errorf("%s: Float64 = %v, want 0", code, got)
		}
		if rt.Context().HasException() {
			t.Errorf("%s: conversion left a pending exception", code)
		}
		v.Free()
		if s, err := safeEval(rt, `2 + 2`); err != nil || s != "4" {
			t.Fatalf("%s poisoned the context: %q %v", code, s, err)
		}
	}
}

// A Date whose getTime throws or returns a non-number is not epoch zero: the
// conversion reports NaN, which Value.DateTime maps to nil, and leaves no
// pending exception.
func TestDateWithBrokenGetTimeIsNotAnEpoch(t *testing.T) {
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	for _, code := range []string{
		`(() => { const d = new Date(1700000000000); d.getTime = () => { throw new Error("boom"); }; return d; })()`,
		`(() => { const d = new Date(1700000000000); d.getTime = () => "text"; return d; })()`,
	} {
		v, err := rt.Context().Eval("date.js", Code(code))
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		if !v.IsDate() {
			t.Fatalf("%s is not a Date", code)
		}
		if got := v.DateTime(); got != nil {
			t.Errorf("%s: DateTime = %v, want nil", code, got)
		}
		if rt.Context().HasException() {
			t.Errorf("%s left a pending exception", code)
		}
		v.Free()
	}

	v, err := rt.Context().Eval("date.js", Code(`new Date(1700000000000)`))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free()
	if got := v.DateTime(); got == nil || got.UnixMilli() != 1700000000000 {
		t.Fatalf("valid Date = %v, want 1700000000000 ms", got)
	}
}

// Loading a module from bytecode compiled as a script must be refused without
// leaking the compiled module, and a refused load must leave the runtime usable.
func TestLoadRejectsNonModuleBytecode(t *testing.T) {
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	bytecode, err := rt.Compile("plain.js", Code(`1 + 1`))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if v, err := rt.Load("plain.js", Bytecode(bytecode), TypeModule()); err == nil || !strings.Contains(err.Error(), "is not a module") {
			if v != nil {
				v.Free()
			}
			t.Fatalf("load %d: %v", i, err)
		}
	}
	if s, err := safeEval(rt, `3 + 3`); err != nil || s != "6" {
		t.Fatalf("runtime unusable after refused load: %q %v", s, err)
	}
}

// Resolving a file or module entry point allocates the resolved path; the
// caller frees it. Every repeated evaluation must therefore be flat in guest
// memory: before, each file evaluation leaked one resolved-path allocation.
func TestFileEvalDoesNotGrowGuestMemory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mod.js"), []byte(`globalThis.n = (globalThis.n || 0) + 1;`), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Option{ReadOnlyDir: dir, MemoryLimitPages: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	run := func(n int) {
		for i := range n {
			v, err := rt.Eval("/mod.js")
			if err != nil {
				t.Fatalf("eval %d: %v", i, err)
			}
			v.Free()
		}
	}
	run(2000)
	base := rt.MemorySize()
	run(20000)
	if growth := rt.MemorySize() - base; growth >= 64*1024 {
		t.Fatalf("20000 file evaluations grew guest memory by %d bytes, want < 65536", growth)
	}
	v, err := rt.Eval("n.js", Code(`globalThis.n`))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free()
	if got := v.Int64(); got != 22000 {
		t.Fatalf("evaluated %d times, want 22000", got)
	}
}

// The module loader resolves common suffixes and JSON files, hands the
// resolver an owned path, and reports unresolvable or broken modules as
// script errors instead of returning a null module.
func TestModuleLoaderResolution(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("script.js", `2 + 2`)
	write("lib.js", `export const x = 41;`)
	write("indexed/index.js", `export const y = 1;`)
	write("data.json", `{"a": 1}`)
	write("broken.json", `{oops}`)

	rt, err := New(Option{ReadOnlyDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	for _, tc := range []struct{ file, want string }{
		{"/script", "4"},
		{"/script.js", "4"},
	} {
		v, err := rt.Eval(tc.file)
		if err != nil {
			t.Fatalf("file eval %s: %v", tc.file, err)
		}
		if got := v.String(); got != tc.want {
			t.Errorf("file eval %s = %q, want %q", tc.file, got, tc.want)
		}
		v.Free()
	}

	for _, tc := range []struct{ name, code, want string }{
		{"import with extension", `import {x} from "./lib.js"; export default x + 1;`, "42"},
		{"import with suffix resolution", `import {x} from "./lib"; export default x + 2;`, "43"},
		{"import index resolution", `import {y} from "./indexed"; export default y + 3;`, "4"},
		{"import json module", `import data from "./data.json"; export default data.a + 4;`, "5"},
	} {
		v, err := rt.Eval("/main.js", Code(tc.code), TypeModule())
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, got, tc.want)
		}
		v.Free()
	}

	if _, err := rt.Eval("/main.js", Code(`import m from "./missing.js"; export default m;`), TypeModule()); err == nil || !strings.Contains(err.Error(), "could not load module") {
		t.Errorf("missing module: %v", err)
	}
	if _, err := rt.Eval("/main.js", Code(`import b from "./broken.json"; export default b;`), TypeModule()); err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Errorf("broken json module: %v", err)
	}
	if s, err := safeEval(rt, `4 + 4`); err != nil || s != "8" {
		t.Fatalf("runtime unusable after module errors: %q %v", s, err)
	}
}

// A JavaScript string may contain U+0000 and multibyte runes. Both the value
// and the atom path must hand Go the whole string, not the prefix before a
// NUL, and the length-taking constructor must keep one.
func TestStringsWithNULAreNotTruncated(t *testing.T) {
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	c := rt.Context()

	for _, tc := range []struct{ code, want string }{
		{`"a\u0000b"`, "a\x00b"},
		{`"\u0000"`, "\x00"},
		{`"héllo世界"`, "héllo世界"},
		{`"plain"`, "plain"},
	} {
		v, err := c.Eval("string.js", Code(tc.code))
		if err != nil {
			t.Fatalf("%s: %v", tc.code, err)
		}
		if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.code, got, tc.want)
		}
		v.Free()
	}

	obj, err := c.Eval("keys.js", Code(`({"a\u0000b": 1})`))
	if err != nil {
		t.Fatal(err)
	}
	props := obj.GetOwnProperties()
	if len(props) != 1 {
		t.Fatalf("got %d properties, want 1", len(props))
	}
	if got := props[0].atom.String(); got != "a\x00b" {
		t.Errorf("property atom = %q, want %q", got, "a\x00b")
	}
	for i := range props {
		props[i].atom.Free()
	}
	obj.Free()

	raw := []byte("a\x00b")
	ptr := c.Malloc(uint64(len(raw)))
	c.MemWrite(uint32(ptr), raw)
	constructed := c.Call("QJS_NewStringLen", c.Raw(), ptr, uint64(len(raw)))
	c.FreeHandle(ptr)
	defer constructed.Free()
	if got := constructed.String(); got != "a\x00b" {
		t.Errorf("QJS_NewStringLen = %q, want %q", got, "a\x00b")
	}
}
