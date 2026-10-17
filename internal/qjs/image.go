package qjs

import (
	"bytes"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	experimentalsys "github.com/tetratelabs/wazero/experimental/sys"
	"github.com/tetratelabs/wazero/experimental/sysfs"
)

// FORK (flats): runtime images.
//
// An Image is the state of a runtime between calls: qjs.wasm's linear memory
// (the 64 KiB pages that differ from a freshly instantiated module), its only
// mutable global (__stack_pointer), and the Go-side wrappers (runtime and
// context handles, the cached global object, the captured intrinsics, the
// relocated stack and the proxy registry's entry kinds). Instantiation runs
// no start function, so New_QJS created every other piece of state in
// memory. New(Option{Image: img}) instantiates the module, writes the image
// over it and rebuilds the wrappers instead of calling New_QJS.
//
// Two values in JSContext are per runtime: time_origin (the monotonic clock
// at creation, also performance.timeOrigin) and random_state (the wall clock
// in microseconds, Math.random's xorshift64* state). A restored runtime
// continues the captured runtime's monotonic clock (see clock.go) and gets a
// fresh random_state from crypto/rand before any guest code runs.
//
// Capture is refused unless, over the runtime's whole life: the caller never
// marked it with Taint (the caller's host operations do), the guest wrote no
// output, random_state is unchanged, no Go-converted JS function is
// retained, the proxy registry holds exactly ids 1..n of Go functions and
// this runtime's context, and the stack canary is intact.

const (
	imagePageSize = 65536
	imageFormat   = 1
	// imageScan is how far past the JSContext pointer random_state is
	// searched for.
	imageScan = 1024
)

var imageMagic = []byte("FLQJSIMG")

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// ErrImageKey means an image was captured with a different qjs.wasm,
// different memory-shaping options or a different caller binding.
var ErrImageKey = errors.New("qjs: image key mismatch")

// ErrImageCorrupt means serialized image bytes are malformed.
var ErrImageCorrupt = errors.New("qjs: corrupt image")

type imagePage struct {
	index uint32
	data  []byte
}

// Image is a captured runtime; see Runtime.Image. It is immutable and may
// restore any number of runtimes, also concurrently.
type Image struct {
	key        [sha256.Size]byte
	memSize    uint32
	pages      []imagePage
	sp         uint64
	rtHandle   uint64
	ctxHandle  uint64
	global     uint64
	intrinsics map[string]uint64
	extra      map[string]uint64
	stackLow   uint32
	stackTop   uint32
	randomAddr uint32
	mono       int64
	kinds      []byte // proxy registry ids 1..n: 'c' context, 'f' Go function
}

// Compatible reports whether New(opt) can restore the image: whether opt
// has the image's key (module, memory-shaping options, binding).
func (img *Image) Compatible(opt Option) bool {
	wasm := wasmBytes
	if len(opt.QuickJSWasmBytes) > 0 {
		wasm = opt.QuickJSWasmBytes
	}
	patched, err := exportStackPointer(wasm)
	return err == nil && imageKey(opt, patched) == img.key
}

// MemorySize is the linear memory size a restored runtime starts with.
func (img *Image) MemorySize() uint32 { return img.memSize }

// imageKey binds images to the module, the options that shape memory and
// the caller's binding.
func imageKey(opt Option, wasm []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte("flats qjs image\x00"))
	var b []byte
	b = binary.LittleEndian.AppendUint32(b, imageFormat)
	sum := wasmSum(wasm)
	b = append(b, sum[:]...)
	pages := opt.MemoryLimitPages
	if pages == 0 {
		pages = DefaultMemoryLimitPages
	}
	stack := opt.StackSize
	if stack == 0 {
		stack = DefaultStackSize
	}
	for _, v := range []int64{int64(opt.MemoryLimit), int64(opt.MaxStackSize), int64(opt.MaxExecutionTime), int64(opt.GCThreshold), int64(stack), int64(pages)} {
		b = binary.LittleEndian.AppendUint64(b, uint64(v))
	}
	// The libc preopen table differs with and without a mount.
	if opt.ReadOnlyDir != "" {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	b = binary.LittleEndian.AppendUint64(b, uint64(len(opt.ImageBinding)))
	h.Write(b)
	h.Write([]byte(opt.ImageBinding))
	var k [sha256.Size]byte
	h.Sum(k[:0])
	return k
}

var (
	baseMu  sync.Mutex
	baseMem = map[*byte][]byte{}
)

// recordBaseMemory keeps, once per module, the linear memory of a freshly
// instantiated module (its data segments): the reference an image's pages
// differ from.
func (r *Runtime) recordBaseMemory() {
	baseMu.Lock()
	defer baseMu.Unlock()
	if _, ok := baseMem[&r.wasm[0]]; ok {
		return
	}
	m := r.module.Memory()
	b, ok := m.Read(0, m.Size())
	if !ok {
		return
	}
	baseMem[&r.wasm[0]] = bytes.Clone(b)
}

func (r *Runtime) baseMemory() []byte {
	baseMu.Lock()
	defer baseMu.Unlock()
	return baseMem[&r.wasm[0]]
}

// findRandomState locates JSContext.random_state from the clock readings
// New_QJS handed to the guest: the 8-byte aligned slot within imageScan
// bytes of the context that holds a wall reading in microseconds, preceded
// by time_origin, a monotonic reading in milliseconds as a double. Exactly
// one slot must match.
func (r *Runtime) findRandomState(walls, monos []int64) {
	if r.context.handle == nil {
		r.randomErr = errors.New("qjs: no context")
		return
	}
	wall := map[uint64]bool{}
	for _, w := range walls {
		if w >= 0 {
			wall[uint64(w/1000)] = true
		}
	}
	mono := map[uint64]bool{}
	for _, m := range monos {
		mono[math.Float64bits(float64(uint64(m))/1e6)] = true
	}
	ctx := uint32(r.context.handle.raw)
	m := r.module.Memory()
	size := uint32(imageScan)
	if m.Size() < ctx {
		size = 0
	} else if left := m.Size() - ctx; left < size {
		size = left
	}
	b, _ := m.Read(ctx, size)
	var found []uint32
	for off := (8-ctx%8)%8 + 8; off+8 <= uint32(len(b)); off += 8 {
		if wall[binary.LittleEndian.Uint64(b[off:])] && mono[binary.LittleEndian.Uint64(b[off-8:])] {
			found = append(found, off)
		}
	}
	if len(found) != 1 {
		r.randomErr = fmt.Errorf("qjs: random state not identified (%d candidate slots)", len(found))
		return
	}
	r.randomAddr = ctx + found[0]
	r.randomInit = binary.LittleEndian.Uint64(b[found[0]:])
}

// Taint permanently makes the runtime non-imageable, for example because a
// host function wrote data into it. The first reason is kept.
func (r *Runtime) Taint(reason string) {
	if r != nil {
		r.taint.CompareAndSwap(nil, &reason)
	}
}

// Restored reports whether the runtime was restored from an image.
func (r *Runtime) Restored() bool { return r != nil && r.restored }

// ImageEligible returns why the runtime cannot be captured now, or nil.
func (r *Runtime) ImageEligible() error {
	switch {
	case r == nil || r.module == nil || r.handle == nil || r.context == nil || r.context.handle == nil:
		return errors.New("qjs: runtime closed")
	case r.restored:
		return errors.New("qjs: runtime was restored from an image")
	}
	if t := r.taint.Load(); t != nil {
		return fmt.Errorf("qjs: runtime tainted: %s", *t)
	}
	if r.output.Load() != 0 {
		return errors.New("qjs: the guest wrote output")
	}
	if r.randomErr != nil {
		return r.randomErr
	}
	if v, ok := r.module.Memory().ReadUint64Le(r.randomAddr); !ok || v != r.randomInit {
		return errors.New("qjs: Math.random state changed")
	}
	if len(r.context.goFunctions) != 0 {
		return errors.New("qjs: Go-converted JS functions are retained")
	}
	if _, err := r.registryKinds(); err != nil {
		return err
	}
	if !r.StackIntact() {
		return errors.New("qjs: stack canary overwritten")
	}
	return nil
}

func (r *Runtime) registryKinds() ([]byte, error) {
	reg := r.registry
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	n := atomic.LoadUint64(&reg.nextID)
	if uint64(len(reg.proxies)) != n {
		return nil, fmt.Errorf("qjs: proxy registry ids are not 1..%d", n)
	}
	kinds := make([]byte, 0, n)
	for id := uint64(1); id <= n; id++ {
		switch v := reg.proxies[id].(type) {
		case *Context:
			if v != r.context {
				return nil, errors.New("qjs: proxy registry holds another context")
			}
			kinds = append(kinds, 'c')
		case Function:
			kinds = append(kinds, 'f')
		default:
			return nil, fmt.Errorf("qjs: proxy registry id %d holds %T", id, v)
		}
	}
	return kinds, nil
}

// Image captures the runtime between calls. extra names caller-owned raw
// JSValues (owned references the restored runtime takes over; see
// ImageValue). It fails unless ImageEligible is nil.
func (r *Runtime) Image(extra map[string]uint64) (*Image, error) {
	if err := r.ImageEligible(); err != nil {
		return nil, err
	}
	kinds, err := r.registryKinds()
	if err != nil {
		return nil, err
	}
	base := r.baseMemory()
	if base == nil {
		return nil, errors.New("qjs: no base memory")
	}
	m := r.module.Memory()
	size := m.Size()
	data, ok := m.Read(0, size)
	if !ok || size%imagePageSize != 0 {
		return nil, errors.New("qjs: cannot read memory")
	}
	img := &Image{
		key:        imageKey(r.option, r.wasm),
		memSize:    size,
		sp:         r.module.ExportedGlobal(stackPointerExport).Get(),
		rtHandle:   r.handle.raw,
		ctxHandle:  r.context.handle.raw,
		intrinsics: map[string]uint64{},
		extra:      map[string]uint64{},
		stackLow:   r.stackLow,
		stackTop:   r.stackTop,
		randomAddr: r.randomAddr,
		mono:       r.clock.now(),
		kinds:      kinds,
	}
	if r.context.global != nil {
		img.global = r.context.global.Raw()
	}
	for k, v := range r.context.intrinsics {
		img.intrinsics[k] = v.Raw()
	}
	for k, v := range extra {
		img.extra[k] = v
	}
	zero := make([]byte, imagePageSize)
	for i := uint32(0); i < size/imagePageSize; i++ {
		page := data[i*imagePageSize : (i+1)*imagePageSize]
		ref := zero
		if int(i+1)*imagePageSize <= len(base) {
			ref = base[i*imagePageSize : (i+1)*imagePageSize]
		}
		if !bytes.Equal(page, ref) {
			img.pages = append(img.pages, imagePage{index: i, data: bytes.Clone(page)})
		}
	}
	return img, nil
}

// restore replaces New_QJS: it writes img over the freshly instantiated
// module and rebuilds the Go-side state.
func (r *Runtime) restore(img *Image, funcs []Function) error {
	if r.registry.Len() != 0 || atomic.LoadUint64(&r.registry.nextID) != 0 {
		return errors.New("proxy registry is not empty")
	}
	nf := 0
	for _, k := range img.kinds {
		if k == 'f' {
			nf++
		}
	}
	if nf != len(funcs) {
		return fmt.Errorf("image has %d Go functions, %d given", nf, len(funcs))
	}
	m := r.module.Memory()
	if m.Size() > img.memSize {
		return errors.New("fresh memory is larger than the image")
	}
	if delta := (img.memSize - m.Size()) / imagePageSize; delta > 0 {
		if _, ok := m.Grow(delta); !ok {
			return errors.New("cannot grow memory to the image size")
		}
	}
	if m.Size() != img.memSize {
		return errors.New("memory size mismatch")
	}
	for _, p := range img.pages {
		if !m.Write(p.index*imagePageSize, p.data) {
			return errors.New("cannot write memory")
		}
	}
	sp, ok := r.module.ExportedGlobal(stackPointerExport).(api.MutableGlobal)
	if !ok {
		return errors.New("stack pointer is not exported")
	}
	sp.Set(img.sp)
	var seed uint64
	for seed == 0 {
		var b [8]byte
		if _, err := io.ReadFull(crand.Reader, b[:]); err != nil {
			return err
		}
		seed = binary.LittleEndian.Uint64(b[:])
	}
	if !m.WriteUint64Le(img.randomAddr, seed) {
		return errors.New("cannot write the random state")
	}
	r.clock.offset.Store(img.mono - hostNanotime())

	r.malloc = r.module.ExportedFunction("malloc")
	r.free = r.module.ExportedFunction("free")
	r.mem = &Mem{mem: m}
	r.handle = NewHandle(r, img.rtHandle)
	r.context.handle = NewHandle(r, img.ctxHandle)
	r.context.runtime = r
	if img.global != 0 {
		r.context.global = r.context.NewValue(NewHandle(r, img.global))
	}
	r.context.intrinsics = make(map[string]*Value, len(img.intrinsics))
	for k, raw := range img.intrinsics {
		r.context.intrinsics[k] = r.context.NewValue(NewHandle(r, raw))
	}
	r.stackLow, r.stackTop = img.stackLow, img.stackTop
	fi := 0
	for _, k := range img.kinds {
		if k == 'c' {
			r.registry.Register(r.context)
			continue
		}
		if funcs[fi] == nil {
			return errors.New("nil image function")
		}
		r.registry.Register(funcs[fi])
		fi++
	}
	if atomic.LoadUint64(&r.registry.nextID) != uint64(len(img.kinds)) {
		return errors.New("proxy registry mismatch")
	}
	r.randomAddr = img.randomAddr
	r.restored = true
	r.imageExtra = img.extra
	return nil
}

// ImageValue returns the caller-owned JSValue the image recorded under name
// (Runtime.Image's extra). The caller owns the returned value.
func (r *Runtime) ImageValue(name string) (*Value, bool) {
	raw, ok := r.imageExtra[name]
	if !ok {
		return nil, false
	}
	return r.context.NewValue(NewHandle(r, raw)), true
}

// MarshalBinary serializes the image: magic, format, key, fields, pages,
// then a CRC-32C of everything before it.
func (img *Image) MarshalBinary() ([]byte, error) {
	le := binary.LittleEndian
	b := make([]byte, 0, 256+len(img.pages)*(imagePageSize+4))
	b = append(b, imageMagic...)
	b = le.AppendUint32(b, imageFormat)
	b = append(b, img.key[:]...)
	b = le.AppendUint32(b, img.memSize)
	for _, v := range []uint64{img.sp, img.rtHandle, img.ctxHandle, img.global, uint64(img.mono)} {
		b = le.AppendUint64(b, v)
	}
	for _, v := range []uint32{img.stackLow, img.stackTop, img.randomAddr} {
		b = le.AppendUint32(b, v)
	}
	b = le.AppendUint32(b, uint32(len(img.kinds)))
	b = append(b, img.kinds...)
	for _, m := range []map[string]uint64{img.intrinsics, img.extra} {
		names := make([]string, 0, len(m))
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		b = le.AppendUint32(b, uint32(len(names)))
		for _, k := range names {
			b = le.AppendUint32(b, uint32(len(k)))
			b = append(b, k...)
			b = le.AppendUint64(b, m[k])
		}
	}
	b = le.AppendUint32(b, uint32(len(img.pages)))
	for _, p := range img.pages {
		b = le.AppendUint32(b, p.index)
		b = append(b, p.data...)
	}
	return le.AppendUint32(b, crc32.Checksum(b, crcTable)), nil
}

// UnmarshalImage parses MarshalBinary's output. Malformed or corrupted bytes
// return ErrImageCorrupt. The key is checked by New.
func UnmarshalImage(b []byte) (*Image, error) {
	le := binary.LittleEndian
	if len(b) < len(imageMagic)+8 || !bytes.Equal(b[:len(imageMagic)], imageMagic) {
		return nil, ErrImageCorrupt
	}
	body, sum := b[:len(b)-4], le.Uint32(b[len(b)-4:])
	if crc32.Checksum(body, crcTable) != sum {
		return nil, ErrImageCorrupt
	}
	d := &imageDecoder{b: body[len(imageMagic):]}
	if d.u32() != imageFormat {
		return nil, ErrImageCorrupt
	}
	img := &Image{intrinsics: map[string]uint64{}, extra: map[string]uint64{}}
	copy(img.key[:], d.bytes(sha256.Size))
	img.memSize = d.u32()
	img.sp, img.rtHandle, img.ctxHandle, img.global = d.u64(), d.u64(), d.u64(), d.u64()
	img.mono = int64(d.u64())
	img.stackLow, img.stackTop, img.randomAddr = d.u32(), d.u32(), d.u32()
	nk := d.u32()
	if uint64(nk) > uint64(len(d.b)) {
		return nil, ErrImageCorrupt
	}
	img.kinds = bytes.Clone(d.bytes(int(nk)))
	for _, k := range img.kinds {
		if k != 'c' && k != 'f' {
			return nil, ErrImageCorrupt
		}
	}
	for _, m := range []map[string]uint64{img.intrinsics, img.extra} {
		n := d.u32()
		for i := uint32(0); i < n && d.err == nil; i++ {
			l := d.u32()
			if l > 256 {
				return nil, ErrImageCorrupt
			}
			k := string(d.bytes(int(l)))
			m[k] = d.u64()
		}
	}
	if img.memSize == 0 || img.memSize%imagePageSize != 0 {
		return nil, ErrImageCorrupt
	}
	np := d.u32()
	if np > img.memSize/imagePageSize {
		return nil, ErrImageCorrupt
	}
	img.pages = make([]imagePage, 0, np)
	for i := uint32(0); i < np && d.err == nil; i++ {
		idx := d.u32()
		if idx >= img.memSize/imagePageSize || (i > 0 && idx <= img.pages[i-1].index) {
			return nil, ErrImageCorrupt
		}
		img.pages = append(img.pages, imagePage{index: idx, data: d.bytes(imagePageSize)})
	}
	if d.err != nil || len(d.b) != 0 {
		return nil, ErrImageCorrupt
	}
	if img.randomAddr == 0 || img.randomAddr > img.memSize-8 {
		return nil, ErrImageCorrupt
	}
	return img, nil
}

type imageDecoder struct {
	b   []byte
	err error
}

func (d *imageDecoder) bytes(n int) []byte {
	if d.err != nil || n < 0 || n > len(d.b) {
		d.err = ErrImageCorrupt
		return make([]byte, max(n, 0))
	}
	v := d.b[:n:n]
	d.b = d.b[n:]
	return v
}

func (d *imageDecoder) u32() uint32 { return binary.LittleEndian.Uint32(d.bytes(4)) }
func (d *imageDecoder) u64() uint64 { return binary.LittleEndian.Uint64(d.bytes(8)) }

// outputCounter forwards guest output, counting it for ImageEligible.
type outputCounter struct {
	w      io.Writer
	n      *atomic.Int64
	census *Census
}

func (o *outputCounter) Write(p []byte) (int, error) {
	o.n.Add(int64(len(p)))
	if o.census != nil {
		o.census.OutputBytes.Add(int64(len(p)))
	}
	return o.w.Write(p)
}

// readOnlyMount mounts dir read-only at "/" (as FSConfig.WithReadOnlyDirMount
// does), counting file opens and closes when census is set.
func readOnlyMount(dir string, census *Census) wazero.FSConfig {
	cfg := wazero.NewFSConfig()
	if census == nil {
		return cfg.WithReadOnlyDirMount(dir, "/")
	}
	return cfg.(sysfs.FSConfig).WithSysFSMount(&sysfs.ReadFS{FS: censusFS{FS: sysfs.DirFS(dir), census: census}}, "/")
}

type censusFS struct {
	experimentalsys.FS
	census *Census
}

func (f censusFS) OpenFile(path string, flag experimentalsys.Oflag, perm fs.FileMode) (experimentalsys.File, experimentalsys.Errno) {
	file, errno := f.FS.OpenFile(path, flag, perm)
	if errno != 0 {
		return file, errno
	}
	if path == "." {
		// wazero opens the mount root on first use and keeps it as the
		// preopened directory; a restored runtime opens its own again.
		return file, 0
	}
	f.census.FileOpens.Add(1)
	return &censusFile{File: file, census: f.census}, 0
}

type censusFile struct {
	experimentalsys.File
	census *Census
	closed atomic.Bool
}

func (f *censusFile) Close() experimentalsys.Errno {
	if f.closed.CompareAndSwap(false, true) {
		f.census.FileCloses.Add(1)
	}
	return f.File.Close()
}
