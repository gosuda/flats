package qjs

import (
	"bytes"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero/api"
)

// FORK (flats): a bigger, guarded C stack.
//
// qjs.wasm is linked with a 64 KiB shadow stack placed directly above its
// static data (__stack_pointer starts at 165408 and grows down toward the
// data segments; --stack-first did not take effect). QuickJS's own stack
// check is not active in this build, so about 250 levels of JS recursion
// (or ~5000 levels of nested JSON) silently overwrite QuickJS's static data:
// the call may even succeed and a later call fails with "invalid opcode".
//
// QuickJS-ng compiles its stack-overflow check out under WASI, so recursion
// past the end of any stack is still undetected while it happens.
//
// The fix, without rebuilding qjs.wasm: export the (only) global, the stack
// pointer, by patching the export section in memory, then after start-up
// point it at the top of a dedicated malloc'd block. The bottom of the block
// is filled with a canary; StackIntact reports whether a call came close to
// (or past) the bottom, after which the runtime must be discarded.

const stackPointerExport = "__flats_stack_pointer"

// DefaultStackSize is the relocated C stack size when Option.StackSize is 0.
const DefaultStackSize = 1 << 20

const stackCanarySize = 64 << 10

var canary = bytes.Repeat([]byte{0xA5}, stackCanarySize)

var (
	patchMu    sync.Mutex
	patchCache = map[*byte][]byte{}
)

func readLEB(b []byte, i int) (uint32, int, error) {
	var r uint32
	var s uint
	for {
		if i >= len(b) {
			return 0, 0, errors.New("truncated LEB128")
		}
		x := b[i]
		i++
		r |= uint32(x&0x7f) << s
		s += 7
		if x&0x80 == 0 {
			return r, i, nil
		}
		if s > 28 {
			return 0, 0, errors.New("LEB128 too long")
		}
	}
}

func appendLEB(b []byte, v uint32) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b = append(b, c|0x80)
		} else {
			return append(b, c)
		}
	}
}

// exportStackPointer returns wasm with global 0 exported as
// stackPointerExport. It checks that the module defines exactly one global,
// a mutable i32 (the linker's __stack_pointer), and imports none.
func exportStackPointer(wasm []byte) ([]byte, error) {
	if len(wasm) == 0 {
		return nil, errors.New("empty wasm")
	}
	patchMu.Lock()
	defer patchMu.Unlock()
	if out, ok := patchCache[&wasm[0]]; ok {
		return out, nil
	}
	if len(wasm) < 8 || !bytes.Equal(wasm[:4], []byte("\x00asm")) {
		return nil, errors.New("not a wasm module")
	}
	out := make([]byte, 0, len(wasm)+64)
	out = append(out, wasm[:8]...)
	sawGlobal, patched := false, false
	for i := 8; i < len(wasm); {
		id := wasm[i]
		size, j, err := readLEB(wasm, i+1)
		if err != nil {
			return nil, err
		}
		end := j + int(size)
		if end > len(wasm) {
			return nil, errors.New("truncated section")
		}
		body := wasm[j:end]
		switch id {
		case 2: // imports: a global import would shift global indices
			n, k, err := readLEB(body, 0)
			if err != nil {
				return nil, err
			}
			for ; n > 0; n-- {
				for f := 0; f < 2; f++ { // module, name
					l, k2, err := readLEB(body, k)
					if err != nil {
						return nil, err
					}
					k = k2 + int(l)
				}
				kind := body[k]
				k++
				switch kind {
				case 0: // func: type index
					_, k, err = readLEB(body, k)
				case 1: // table: reftype + limits
					k++
					var flags uint32
					flags, k, err = readLEB(body, k)
					if err == nil {
						_, k, err = readLEB(body, k)
						if err == nil && flags&1 != 0 {
							_, k, err = readLEB(body, k)
						}
					}
				case 2: // memory: limits
					var flags uint32
					flags, k, err = readLEB(body, k)
					if err == nil {
						_, k, err = readLEB(body, k)
						if err == nil && flags&1 != 0 {
							_, k, err = readLEB(body, k)
						}
					}
				case 3:
					return nil, errors.New("module imports a global")
				default:
					return nil, fmt.Errorf("unknown import kind %d", kind)
				}
				if err != nil {
					return nil, err
				}
			}
		case 6: // globals
			n, k, err := readLEB(body, 0)
			if err != nil || n != 1 || k+2 > len(body) || body[k] != 0x7f || body[k+1] != 1 {
				return nil, errors.New("unexpected globals: want exactly one mutable i32 (__stack_pointer)")
			}
			sawGlobal = true
		case 7: // exports: append ours
			if !sawGlobal {
				return nil, errors.New("export section before global section")
			}
			n, k, err := readLEB(body, 0)
			if err != nil {
				return nil, err
			}
			nb := appendLEB(nil, n+1)
			nb = append(nb, body[k:]...)
			nb = appendLEB(nb, uint32(len(stackPointerExport)))
			nb = append(nb, stackPointerExport...)
			nb = append(nb, 3, 0) // kind global, index 0
			out = append(out, id)
			out = appendLEB(out, uint32(len(nb)))
			out = append(out, nb...)
			patched = true
			i = end
			continue
		}
		out = append(out, wasm[i:end]...)
		i = end
	}
	if !patched {
		return nil, errors.New("no export section")
	}
	patchCache[&wasm[0]] = out
	return out, nil
}

// relocateStack moves the C stack into a malloc'd block of size bytes.
func (r *Runtime) relocateStack(size uint32) error {
	g, ok := r.module.ExportedGlobal(stackPointerExport).(api.MutableGlobal)
	if !ok {
		return errors.New("stack pointer is not exported")
	}
	base := r.Malloc(uint64(size))
	if base == 0 {
		return errors.New("cannot allocate the JS stack")
	}
	top := (base + uint64(size)) &^ 15
	if !r.mem.mem.Write(uint32(base), canary) {
		return errors.New("cannot write stack canary")
	}
	g.Set(top)
	r.stackLow = uint32(base)
	r.stackTop = uint32(top)
	if r.handle != nil {
		r.Call("QJS_UpdateStackTop", r.handle.raw)
	}
	return nil
}

// StackIntact reports whether the guarded bottom of the relocated stack is
// untouched. False means a call recursed to within 64 KiB of the end of the
// stack (or past it): discard the runtime.
func (r *Runtime) StackIntact() bool {
	if r == nil || r.module == nil || r.stackLow == 0 {
		return true
	}
	b, ok := r.module.Memory().Read(r.stackLow, stackCanarySize)
	return ok && bytes.Equal(b, canary)
}
