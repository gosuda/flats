package formula

import (
	"slices"
	"unicode/utf8"
	"unsafe"
)

// Size returns the bytes of heap the formula keeps alive: its own record,
// its stored text, the pieces of text around its references, its
// references and its tree with the argument list of every node. A holder
// that keeps the formula after every other holder has let go keeps that
// much. The formulas that Parse, ParseStored, ParseRetained, the
// transforms and TextForm return share nothing with others. The stored text given to ParseStored
// is counted as the formula's own, as every caller in the engine gives a
// text of its own.
//
// The bytes are those the Go allocator takes for each object: its size
// class, the header of a small object with pointers above 512 bytes and
// the bitmap of a smaller one, and the whole block of 16 bytes that a
// short text may share with others.
func (f *Formula) Size() int64 { return int64(f.size) }

// measure sets the size of a finished formula. copied holds the bytes of
// texts the parser built for the tree, which hold no part of stored.
func (f *Formula) measure(copied int64) {
	n := heapBytes(unsafe.Sizeof(Formula{}), true) + textBytes(len(f.stored)) + copied
	n += heapBytes(uintptr(cap(f.lits))*unsafe.Sizeof(""), true)
	n += heapBytes(uintptr(cap(f.slots))*unsafe.Sizeof(slot{}), true)
	if f.root != nil {
		// A sum of thousands of terms is a tree of that depth, so the walk
		// keeps its own stack.
		stack := []*node{f.root}
		for len(stack) > 0 {
			nd := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			n += heapBytes(unsafe.Sizeof(node{}), true)
			n += heapBytes(uintptr(cap(nd.args))*unsafe.Sizeof(nd), true)
			stack = append(stack, nd.args...)
		}
	}
	// A formula is at most a few MiB: 8,192 characters, each at most a
	// node.
	f.size = int32(n)
}

// TextForm returns the formula as a reader that shows it needs it: its
// stored text and what Stored, Text, TextLen, Unreadable, ReadsToday and
// DateResult give, which are the same. It keeps no tree and no
// references, which a long sum takes hundreds of times its text for, and
// works the references out of the stored text when Text or TextLen asks.
// Its value is #ERROR!, it names no tab and no reference, and no transform
// changes it.
func (f *Formula) TextForm() *Formula {
	if f.form {
		return f
	}
	g := &Formula{stored: f.stored, unreadable: f.unreadable, today: f.today, dateResult: f.dateResult, form: true}
	g.measure(0)
	return g
}

// TextFormSize returns the Size of the formula's TextForm without making
// it.
func (f *Formula) TextFormSize() int64 {
	if f.form {
		return f.Size()
	}
	return heapBytes(unsafe.Sizeof(Formula{}), true) + textBytes(len(f.stored))
}

// pieces returns the text around the references and the references. A
// text form works them out of its stored text.
func (f *Formula) pieces() (lits []string, slots []slot, litRunes int) {
	if !f.form {
		return f.lits, f.slots, f.litRunes
	}
	if f.unreadable {
		return []string{f.stored}, nil, utf8.RuneCountInString(f.stored)
	}
	// The arrays have room for the most tokens and references the text
	// can hold, so they never grow and TextWork covers them.
	lx := lexer{src: f.stored, stored: true, toks: make([]token, 0, len(f.stored)),
		slots: make([]slot, 0, len(f.stored)/minStoredSlot)}
	if !lx.run() {
		return []string{f.stored}, nil, utf8.RuneCountInString(f.stored)
	}
	lits = make([]string, 0, len(lx.slots)+1)
	prev := 0
	for _, t := range lx.toks {
		if t.kind == tokRef {
			lits = append(lits, f.stored[prev:t.start])
			prev = int(t.end)
		}
	}
	lits = append(lits, f.stored[prev:])
	for _, l := range lits {
		litRunes += utf8.RuneCountInString(l)
	}
	return lits, lx.slots, litRunes
}

// minStoredSlot is the fewest bytes a reference takes in a stored text:
// {R1C1}. A token takes one byte at the least.
const minStoredSlot = 6

// maxTextRatio bounds the bytes of Text by those of the stored text. A
// reference writes at most three times the bytes of its slot: a cell at
// most 14 for a slot of 6 or more, a range 29 for 11, whole rows 21 for 7
// and whole columns 9 for 7, besides its tab name, and #REF! 5. The text
// around the references is the same in both.
const maxTextRatio = 3

// TextWork returns the most bytes of heap Text allocates for the formula
// at any cell, its result included, so that a caller can charge them
// before it calls Text. It counts from the length of the stored text
// alone. A text form works its references out of that text when Text
// asks, in arrays of the most tokens and references the text can hold.
func (f *Formula) TextWork() int64 {
	n := uintptr(len(f.stored))
	work := textBytes(maxTextRatio * len(f.stored))
	if f.form {
		slots := n / minStoredSlot
		work += heapBytes(n*unsafe.Sizeof(token{}), false)
		work += heapBytes(slots*unsafe.Sizeof(slot{}), true)
		work += heapBytes((slots+1)*unsafe.Sizeof(""), true)
	}
	return work
}

// The size classes of the Go allocator up to 32 KiB (runtime/sizeclasses.go,
// unchanged since Go 1.12).
var sizeClasses = [...]uint16{8, 16, 24, 32, 48, 64, 80, 96, 112, 128, 144, 160, 176, 192, 208, 224,
	240, 256, 288, 320, 352, 384, 416, 448, 480, 512, 576, 640, 704, 768, 896, 1024, 1152, 1280,
	1408, 1536, 1792, 2048, 2304, 2688, 3072, 3200, 3456, 4096, 4864, 5376, 6144, 6528, 6784,
	6912, 8192, 9472, 9728, 10240, 10880, 12288, 13568, 14336, 16384, 18432, 19072, 20480, 21760,
	24576, 27264, 28672, 32768}

const (
	smallMax    = 32 << 10
	pageSize    = 8 << 10
	headerFrom  = 512 // a small object with pointers above this has a header
	headerBytes = 8
)

// heapBytes returns what the allocator takes for one object of n bytes.
func heapBytes(n uintptr, pointers bool) int64 {
	if n == 0 {
		return 0
	}
	if pointers && n > headerFrom {
		n += headerBytes
	}
	if n > smallMax {
		return int64((n + pageSize - 1) / pageSize * pageSize)
	}
	i, _ := slices.BinarySearch(sizeClasses[:], uint16(n))
	c := int64(sizeClasses[i])
	if pointers && c <= headerFrom {
		c += (c + 63) / 64 // the span keeps a bit for each word of the object
	}
	return c
}

// HeapBytes returns what the allocator takes for one object of n bytes,
// with pointers or without.
func HeapBytes(n uintptr, pointers bool) int64 { return heapBytes(n, pointers) }

// TextBytes returns what the allocator takes for a text of n bytes of its
// own, which a holder of it keeps alive. A text under 16 bytes may share a
// block of 16 with others.
func TextBytes(n int) int64 { return textBytes(n) }

// textBytes returns what the allocator takes for the bytes of a text of
// its own. A text under 16 bytes may share a block of 16 with others,
// which it keeps alive.
func textBytes(n int) int64 {
	if n == 0 {
		return 0
	}
	return heapBytes(uintptr(max(n, 16)), false)
}
