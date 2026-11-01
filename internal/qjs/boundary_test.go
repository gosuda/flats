package qjs

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

// patchModule builds a minimal module exportStackPointer accepts: one
// mutable i32 global (the __stack_pointer) and an empty export section.
// II is the global's init value, at offset 14 of the binary.
func patchModule(init byte) []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x06, 0x06, 0x01, 0x7f, 0x01, 0x41, init, 0x0b,
		0x07, 0x01, 0x00,
	}
}

const patchModuleInitOff = 14

// A caller that clones QuickJSWasmBytes must still be judged compatible.
func TestSameBytesComparesContent(t *testing.T) {
	a := []byte{1, 2, 3}
	b := bytes.Clone(a)
	if !sameBytes(a, b) {
		t.Fatal("equal content in different slices: want true")
	}
	if sameBytes(a, []byte{1, 2, 4}) {
		t.Fatal("different content, same length: want false")
	}
	if !sameBytes(nil, nil) {
		t.Fatal("two empty slices: want true")
	}
	if sameBytes(a, nil) {
		t.Fatal("empty vs non-empty: want false")
	}
}

// wasmSum is a pure function of content: empty input must not panic, and
// clones must hash the same as the original.
func TestWasmSumKeysOnContent(t *testing.T) {
	if wasmSum(nil) != wasmSum([]byte{}) {
		t.Fatal("empty inputs: want equal sums")
	}
	content := []byte("flats wasm module bytes")
	if got := wasmSum(content); got != sha256.Sum256(content) {
		t.Fatal("want sha256 of the content")
	}
	clone := bytes.Clone(content)
	if wasmSum(clone) != wasmSum(content) {
		t.Fatal("cloned content: want equal sums")
	}
	content[0] ^= 0xff
	if wasmSum(content) == wasmSum(clone) {
		t.Fatal("mutated content: want different sums")
	}
}

// The patch cache must serve outputs by content: a mutated input may never
// receive the output patched for the old bytes.
func TestExportStackPointerCacheKeysOnContent(t *testing.T) {
	in := patchModule(0x00)
	first, err := exportStackPointer(in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := exportStackPointer(patchModule(0x00))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, again) {
		t.Fatal("same content through a different slice: want equal output")
	}

	// Mutate the same backing array between calls: the fresh call must
	// reflect the new content, not return stale patched bytes.
	in[patchModuleInitOff] = 0x7f
	second, err := exportStackPointer(in)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("mutated input returned stale patched bytes")
	}
	if second[patchModuleInitOff] != 0x7f {
		t.Fatalf("patched output not derived from current content: byte %d = %#x",
			patchModuleInitOff, second[patchModuleInitOff])
	}
}
