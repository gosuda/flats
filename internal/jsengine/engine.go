package jsengine

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/gosuda/flats/internal/qjs"
)

//go:linkname qjsWasmBytes github.com/gosuda/flats/internal/qjs.wasmBytes
var qjsWasmBytes []byte

const (
	DefaultTimeoutMS       = 10000
	DefaultMemoryPages     = 1024
	DefaultMaxInputBytes   = 10 << 20
	DefaultMaxOutputBytes  = 32 << 20
	DefaultMaxHostArgBytes = 2 << 20
	maxLogLine             = 64 << 10

	pinnedWasmSHA256 = "79284a9282ad8471573a4d52401362e27d149f1d70b4175cb5affc960a8dc9a9"

	stateReady int32 = iota
	stateBusy
	statePoisoned
	stateClosed
)

type valKind uint8

const (
	valUndefined valKind = iota
	valString
	valInt32
	valArrayBuffer
)

type Value struct {
	raw   *qjs.Value
	ctx   *qjs.Context
	eng   *Engine
	freed *atomic.Bool

	kind valKind
	str  string
	i32  int32
	buf  []byte
}

func StringValue(s string) Value {
	return Value{kind: valString, str: s}
}

func Int32Value(n int32) Value {
	return Value{kind: valInt32, i32: n}
}

func ArrayBufferValue(b []byte) Value {
	return Value{kind: valArrayBuffer, buf: b}
}

func (v Value) isFreed() bool {
	return v.freed != nil && v.freed.Load()
}

func (v Value) IsByteArray() bool {
	if v.isFreed() {
		return false
	}
	if v.raw != nil {
		return v.raw.IsByteArray()
	}
	return v.kind == valArrayBuffer
}

func (v Value) ByteLen() int64 {
	if v.isFreed() {
		return 0
	}
	if v.raw != nil {
		return v.raw.ByteLen()
	}
	if v.kind == valArrayBuffer {
		return int64(len(v.buf))
	}
	return 0
}

func (v Value) ToByteArray() []byte {
	if v.isFreed() {
		return nil
	}
	if v.raw != nil {
		return v.raw.ToByteArray()
	}
	if v.kind == valArrayBuffer {
		return slices.Clone(v.buf)
	}
	return nil
}

func (v Value) String() string {
	if v.isFreed() {
		return ""
	}
	if v.raw != nil {
		return v.raw.String()
	}
	if v.kind == valString {
		return v.str
	}
	return ""
}

func (v Value) Int32() int32 {
	if v.isFreed() {
		return 0
	}
	if v.raw != nil {
		return v.raw.Int32()
	}
	if v.kind == valInt32 {
		return v.i32
	}
	return 0
}

func (v Value) IsString() bool {
	if v.isFreed() {
		return false
	}
	if v.raw != nil {
		return v.raw.IsString()
	}
	return v.kind == valString
}
func (v Value) Free() {
	if v.raw != nil && (v.freed == nil || v.freed.CompareAndSwap(false, true)) {
		v.raw.Free()
		if v.eng != nil {
			v.eng.trackHandle(-1)
		}
	}
}

func (v Value) Clone() Value {
	if v.raw != nil {
		cloned := v.raw.Clone()
		if v.eng != nil {
			v.eng.trackHandle(1)
		}
		return Value{raw: cloned, ctx: v.ctx, eng: v.eng, freed: &atomic.Bool{}}
	}
	res := v
	if v.kind == valArrayBuffer {
		res.buf = slices.Clone(v.buf)
	}
	return res
}

func (v Value) NewString(s string) Value {
	if v.ctx != nil {
		if v.eng != nil {
			v.eng.trackHandle(1)
		}
		return Value{raw: v.ctx.NewString(s), ctx: v.ctx, eng: v.eng, freed: &atomic.Bool{}}
	}
	return StringValue(s)
}

func (v Value) NewInt32(n int32) Value {
	if v.ctx != nil {
		if v.eng != nil {
			v.eng.trackHandle(1)
		}
		return Value{raw: v.ctx.NewInt32(n), ctx: v.ctx, eng: v.eng, freed: &atomic.Bool{}}
	}
	return Int32Value(n)
}

func (v Value) NewArrayBuffer(b []byte) Value {
	if v.ctx != nil {
		if v.eng != nil {
			v.eng.trackHandle(1)
		}
		return Value{raw: v.ctx.NewArrayBuffer(b), ctx: v.ctx, eng: v.eng, freed: &atomic.Bool{}}
	}
	return ArrayBufferValue(b)
}

type CodecFunc func(op string, in Value) (Value, error)

type Census struct {
	WallReads   atomic.Int64
	MonoReads   atomic.Int64
	OutputBytes atomic.Int64
	FileOpens   atomic.Int64
	FileCloses  atomic.Int64
}

// SAFETY: Census mirrors qjs.Census field-for-field with identical memory layout.
func toQjsCensus(c *Census) *qjs.Census {
	if c == nil {
		return nil
	}
	return (*qjs.Census)(unsafe.Pointer(c))
}

type Spec struct {
	Dir             string
	Entry           string
	TimeoutMS       int
	MaxInputBytes   int
	MaxOutputBytes  int
	MaxHostArgBytes int

	CacheDir     string
	MemoryPages  int
	LogSink      io.Writer
	Stdout       io.Writer
	Stderr       io.Writer
	Prelude      string
	Boot         string
	ImagePath    string
	ImageBinding string

	Host HostFunc

	DocsCodec func(ctx context.Context, op string, in []byte) ([]byte, error)
	RawCodec  CodecFunc

	Shell  *Shell
	Image  *Image
	Census *Census
}

func (e *Engine) hasCodec() bool {
	return e.spec.RawCodec != nil || e.spec.DocsCodec != nil
}

type swapCtx struct {
	cur atomic.Pointer[context.Context]
}

func newSwapCtx() *swapCtx {
	s := &swapCtx{}
	bg := context.Background()
	s.cur.Store(&bg)
	return s
}

func (s *swapCtx) set(ctx context.Context)     { s.cur.Store(&ctx) }
func (s *swapCtx) current() context.Context    { return *s.cur.Load() }
func (s *swapCtx) Deadline() (time.Time, bool) { return s.current().Deadline() }
func (s *swapCtx) Done() <-chan struct{}       { return s.current().Done() }
func (s *swapCtx) Err() error                  { return s.current().Err() }
func (s *swapCtx) Value(key any) any           { return s.current().Value(key) }

type Shell struct {
	raw  *qjs.Shell
	spec Spec
}

func TakePrewarmed() (*Shell, error) {
	return NewShell(Spec{})
}

func NewShell(spec Spec) (*Shell, error) {
	pages := spec.MemoryPages
	if pages <= 0 {
		pages = DefaultMemoryPages
	}
	opt := qjs.Option{
		CacheDir:           spec.CacheDir,
		MemoryLimitPages:   uint32(pages),
		CloseOnContextDone: true,
	}
	s, err := qjs.NewShell(opt)
	if err != nil {
		return nil, err
	}
	return &Shell{raw: s, spec: spec}, nil
}

func (s *Shell) Compatible(spec Spec) bool {
	if s == nil || s.raw == nil {
		return false
	}
	pages := spec.MemoryPages
	if pages <= 0 {
		pages = DefaultMemoryPages
	}
	opt := qjs.Option{
		CacheDir:           spec.CacheDir,
		MemoryLimitPages:   uint32(pages),
		CloseOnContextDone: true,
	}
	return s.raw.Compatible(opt)
}

func (s *Shell) Close() error {
	if s == nil || s.raw == nil {
		return nil
	}
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("close shell panic: %v", r)
			}
		}()
		s.raw.Close()
	}()
	s.raw = nil
	return err
}

type Image struct {
	raw      *qjs.Image
	numFuncs int
}

func countImageFuncs(b []byte) int {
	if len(b) < 104 {
		return 0
	}
	nk := int(binary.LittleEndian.Uint32(b[100:104]))
	if nk < 0 || 104+nk > len(b) {
		return 0
	}
	n := 0
	for _, k := range b[104 : 104+nk] {
		if k == 'f' {
			n++
		}
	}
	return n
}

func UnmarshalImage(b []byte) (Image, error) {
	img, err := qjs.UnmarshalImage(b)
	if err != nil {
		return Image{}, err
	}
	return Image{raw: img, numFuncs: countImageFuncs(b)}, nil
}

func (img Image) Compatible(spec Spec) bool {
	if img.raw == nil {
		return false
	}
	pages := spec.MemoryPages
	if pages <= 0 {
		pages = DefaultMemoryPages
	}
	opt := qjs.Option{
		ReadOnlyDir:        spec.Dir,
		CacheDir:           spec.CacheDir,
		MemoryLimitPages:   uint32(pages),
		MemoryLimit:        pages * 65536 * 3 / 4,
		CloseOnContextDone: true,
		ImageBinding:       spec.ImageBinding,
	}
	return img.raw.Compatible(opt)
}

func (img Image) MarshalBinary() ([]byte, error) {
	if img.raw == nil {
		return nil, ErrImageCorrupt
	}
	return img.raw.MarshalBinary()
}

type Engine struct {
	spec     Spec
	state    atomic.Int32
	rt       *qjs.Runtime
	gc       *qjs.Value
	gcBytes  uint64
	gcCalls  uint32
	gcMemory uint32
	swapCtx  *swapCtx
	out      *lineWriter
	errOut   *lineWriter
	host     HostFunc

	liveHandles  atomic.Int64
	hostPanicked atomic.Bool
	hostTainted  atomic.Bool
	userTainted  atomic.Bool
	bootRan      atomic.Bool
	bootActive   atomic.Bool
	gcDrained    atomic.Bool
	callMu       sync.Mutex
	callGoid     atomic.Int64
	inClose      atomic.Bool

	testHookLeakHandle       atomic.Bool
	testHookPendingException atomic.Bool
}

func Open(spec Spec, host HostFunc) (*Engine, error) {
	sum := sha256.Sum256(qjsWasmBytes)
	if hex.EncodeToString(sum[:]) != pinnedWasmSHA256 {
		return nil, ErrWasmMismatch
	}

	if host == nil && spec.Host != nil {
		host = spec.Host
	}
	pages := cmp.Or(spec.MemoryPages, DefaultMemoryPages)
	spec.MemoryPages = pages
	spec.TimeoutMS = cmp.Or(spec.TimeoutMS, DefaultTimeoutMS)
	spec.MaxInputBytes = cmp.Or(spec.MaxInputBytes, DefaultMaxInputBytes)
	spec.MaxOutputBytes = cmp.Or(spec.MaxOutputBytes, DefaultMaxOutputBytes)
	spec.MaxHostArgBytes = cmp.Or(spec.MaxHostArgBytes, DefaultMaxHostArgBytes)

	stdout := cmp.Or(spec.Stdout, spec.LogSink)
	stderr := cmp.Or(spec.Stderr, spec.LogSink)
	eng := &Engine{
		spec:    spec,
		host:    host,
		swapCtx: newSwapCtx(),
		out:     newLineWriter(stdout),
		errOut:  newLineWriter(stderr),
	}

	opt := qjs.Option{
		Context:            eng.swapCtx,
		ReadOnlyDir:        spec.Dir,
		CacheDir:           spec.CacheDir,
		MemoryLimitPages:   uint32(pages),
		MemoryLimit:        pages * 65536 * 3 / 4,
		CloseOnContextDone: true,
		Stdout:             eng.out,
		Stderr:             eng.errOut,
		ImageBinding:       spec.ImageBinding,
		Census:             toQjsCensus(spec.Census),
	}

	if spec.Shell != nil {
		if spec.Shell.raw == nil {
			return nil, errors.New("shell already closed")
		}
		if !spec.Shell.Compatible(spec) {
			return nil, errors.New("shell not compatible with engine spec")
		}
		opt.Shell = spec.Shell.raw
	}

	fns1 := []qjs.Function{eng.hostCall}
	fns2 := []qjs.Function{eng.hostCall, eng.codecCall}

	if spec.Image != nil && spec.Image.raw != nil {
		opt.Image = spec.Image.raw
		if spec.Image.numFuncs == 2 {
			opt.ImageFunctions = fns2
		} else if spec.Image.numFuncs == 1 {
			opt.ImageFunctions = fns1
		} else if eng.hasCodec() {
			opt.ImageFunctions = fns2
		} else {
			opt.ImageFunctions = fns1
		}
	}

	rt, err := qjs.New(opt)
	if err != nil && spec.Image != nil && spec.Image.raw != nil && opt.Shell == nil {
		if strings.Contains(err.Error(), "image has 2 Go functions, 1 given") {
			opt.ImageFunctions = fns2
			rt, err = qjs.New(opt)
		} else if strings.Contains(err.Error(), "image has 1 Go functions, 2 given") {
			opt.ImageFunctions = fns1
			rt, err = qjs.New(opt)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("start JavaScript runtime: %w", err)
	}
	eng.rt = rt

	c := rt.Context()
	if spec.Image == nil || spec.Image.raw == nil {
		eng.setupHostFunctions(c)
	}

	if spec.Image != nil && spec.Image.raw != nil {
		if gcVal, ok := rt.ImageValue("gc"); ok {
			eng.gc = gcVal
		}
	} else {
		eng.gc = c.Global().GetPropertyStr("gc")
		c.Global().SetPropertyStr("gc", c.NewUndefined())
	}
	startupTimeout := time.Duration(spec.TimeoutMS) * time.Millisecond
	startupCtx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()

	eng.bootActive.Store(true)

	if spec.Prelude != "" {
		eng.swapCtx.set(startupCtx)
		v, err := c.Eval("flats:prelude.js", qjs.Code(spec.Prelude))
		eng.swapCtx.set(context.Background())
		if err != nil {
			_ = eng.Close()
			return nil, fmt.Errorf("prelude: %w", err)
		}
		if v != nil {
			v.Free()
		}
	}

	if spec.Boot != "" {
		eng.swapCtx.set(startupCtx)
		v, err := c.Eval("/__flats_boot__.js", qjs.Code(spec.Boot), qjs.TypeModule())
		eng.swapCtx.set(context.Background())
		if err != nil {
			_ = eng.Close()
			return nil, fmt.Errorf("boot: %w", err)
		}
		if v != nil {
			v.Free()
		}
	}
	eng.bootActive.Store(false)
	if eng.hostPanicked.Load() || eng.hostTainted.Load() {
		_ = eng.Close()
		return nil, fmt.Errorf("%w: host function invoked during startup", ErrTainted)
	}

	if !rt.StackIntact() {
		_ = eng.Close()
		return nil, fmt.Errorf("loading %s: maximum call stack size exceeded", spec.Entry)
	}

	eng.gcMemory = rt.MemorySize()
	eng.state.Store(stateReady)
	return eng, nil
}
func (e *Engine) trackHandle(n int64) {
	e.liveHandles.Add(n)
}

func (e *Engine) setTestHookLeakHandle(v bool) {
	e.testHookLeakHandle.Store(v)
}

func (e *Engine) setTestHookPendingException(v bool) {
	e.testHookPendingException.Store(v)
}

func (e *Engine) isFatal(err error, ctx context.Context) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*Fatal](err); ok {
		return true
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, qjs.ErrRuntimeClosed) || errors.Is(err, qjs.ErrOutOfMemory) {
		return true
	}
	if e.rt != nil && !e.rt.StackIntact() {
		return true
	}
	se := parseScriptError(err)
	if se != nil && se.Name == "InternalError" {
		return true
	}
	return false
}

func (e *Engine) fatal(cause error) error {
	e.state.Store(statePoisoned)
	if f, ok := cause.(*Fatal); ok {
		return f
	}
	msg := ""
	if cause != nil {
		msg = trimStack(cause.Error())
	}
	return &Fatal{
		Cause:   cause,
		Message: msg,
	}
}

func curGoroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	s := string(buf[:n])
	const prefix = "goroutine "
	if !strings.HasPrefix(s, prefix) {
		return 0
	}
	s = s[len(prefix):]
	i := strings.IndexByte(s, ' ')
	if i < 0 {
		return 0
	}
	var id int64
	for _, c := range s[:i] {
		if c < '0' || c > '9' {
			return 0
		}
		id = id*10 + int64(c-'0')
	}
	return id
}

func (e *Engine) busyOrCurrentState() error {
	if e.inClose.Load() {
		return ErrClosed
	}
	switch e.state.Load() {
	case stateBusy:
		return ErrBusy
	case statePoisoned:
		return ErrPoisoned
	case stateClosed:
		return ErrClosed
	default:
		return ErrBusy
	}
}

func trimStack(s string) string {
	if before, _, found := strings.Cut(s, "\nstack:"); found {
		return before
	}
	return s
}

func bindCallTimeout(ctx context.Context, timeoutMS int) (context.Context, context.CancelFunc, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, nil, ErrNoDeadline
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	now := time.Now()
	if !deadline.After(now) {
		return nil, nil, context.DeadlineExceeded
	}
	if timeoutMS <= 0 {
		timeoutMS = DefaultTimeoutMS
	}
	timeout := time.Duration(timeoutMS) * time.Millisecond
	if deadline.Sub(now) <= timeout {
		return ctx, func() {}, nil
	}
	c, cancel := context.WithDeadline(ctx, now.Add(timeout))
	return c, cancel, nil
}

func (e *Engine) postCallChecks(c *qjs.Context, out string) error {
	if !e.rt.StackIntact() {
		return e.fatal(errors.New("maximum call stack size exceeded (recursion too deep)"))
	}
	if e.hostPanicked.Load() {
		return e.fatal(errors.New("host function panicked"))
	}
	maxOutput := e.spec.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = DefaultMaxOutputBytes
	}
	if len(out) > maxOutput {
		return ErrTooLarge
	}
	if c.HasException() {
		return e.fatal(c.Exception())
	}
	if e.testHookLeakHandle.Load() {
		e.trackHandle(1)
	}
	if e.liveHandles.Load() != 0 {
		return e.fatal(fmt.Errorf("live handle leak: %d handles active", e.liveHandles.Load()))
	}
	if err := e.collectCycles(c); err != nil {
		return err
	}
	return nil
}

func (e *Engine) handleCallResult(p *qjs.Value, callErr error, ctx context.Context) (string, error) {
	if callErr != nil {
		if p != nil {
			p.Free()
			e.trackHandle(-1)
		}
		if e.hostPanicked.Load() || e.isFatal(callErr, ctx) {
			return "", e.fatal(callErr)
		}
		return "", parseScriptError(callErr)
	}
	r := p
	var err error
	if p.IsPromise() {
		r, err = p.Await()
		if err != nil {
			p.Free()
			e.trackHandle(-1)
			if e.hostPanicked.Load() || e.isFatal(err, ctx) {
				return "", e.fatal(err)
			}
			return "", parseScriptError(err)
		}
	}
	out := r.String()
	if r != p {
		r.Free()
	}
	p.Free()
	e.trackHandle(-1)
	return out, nil
}

func (e *Engine) handleEvalResult(v *qjs.Value, evalErr error, ctx context.Context) (string, error) {
	if evalErr != nil {
		if v != nil {
			v.Free()
			e.trackHandle(-1)
		}
		if e.hostPanicked.Load() || e.isFatal(evalErr, ctx) {
			return "", e.fatal(evalErr)
		}
		return "", parseScriptError(evalErr)
	}
	if v == nil {
		return "", nil
	}
	r := v
	var err error
	if v.IsPromise() {
		r, err = v.Await()
		if err != nil {
			v.Free()
			e.trackHandle(-1)
			if e.hostPanicked.Load() || e.isFatal(err, ctx) {
				return "", e.fatal(err)
			}
			return "", parseScriptError(err)
		}
	}
	out := r.String()
	if r != v {
		r.Free()
	}
	v.Free()
	e.trackHandle(-1)
	return out, nil
}

func (e *Engine) Call(ctx context.Context, fn string, arg string) (out string, retErr error) {
	if fn == "gc" {
		return "", errors.New("cannot call gc directly")
	}
	cCtx, cancel, err := bindCallTimeout(ctx, e.spec.TimeoutMS)
	if err != nil {
		return "", err
	}
	defer cancel()

	maxInput := e.spec.MaxInputBytes
	if maxInput <= 0 {
		maxInput = DefaultMaxInputBytes
	}
	if len(arg) > maxInput {
		return "", ErrTooLarge
	}

	if e.inClose.Load() || !e.state.CompareAndSwap(stateReady, stateBusy) {
		return "", e.busyOrCurrentState()
	}
	e.userTainted.Store(true)
	if e.rt != nil {
		e.rt.Taint("guest call")
	}
	goid := curGoroutineID()
	e.callGoid.Store(goid)
	e.callMu.Lock()
	defer func() {
		if e.state.Load() == stateBusy && !e.inClose.Load() {
			e.state.Store(stateReady)
		}
		e.callGoid.Store(0)
		e.callMu.Unlock()
	}()
	if e.inClose.Load() || e.state.Load() == stateClosed {
		return "", ErrClosed
	}
	e.swapCtx.set(cCtx)
	e.hostPanicked.Store(false)
	defer func() {
		if r := recover(); r != nil {
			e.state.Store(statePoisoned)
			retErr = &Fatal{
				Cause:   fmt.Errorf("%v", r),
				Message: trimStack(fmt.Sprint(r)),
			}
		}
		e.swapCtx.set(context.Background())
		e.out.Flush()
		e.errOut.Flush()
	}()

	c := e.rt.Context()
	e.gcBytes += uint64(len(arg))

	a := c.NewString(arg)
	e.trackHandle(1)
	p, err := c.Global().InvokeJS(fn, a)
	a.Free()
	e.trackHandle(-1)

	if p != nil {
		if err != nil {
			p.Free()
			p = nil
		} else {
			e.trackHandle(1)
		}
	}

	out, callErr := e.handleCallResult(p, err, cCtx)
	e.gcBytes += uint64(len(out))

	if e.testHookPendingException.Load() {
		c.Throw(c.NewError(errors.New("leaked pending exception")))
	}

	if checkErr := e.postCallChecks(c, out); checkErr != nil {
		return "", checkErr
	}
	if callErr != nil {
		return "", callErr
	}
	return out, nil
}

func (e *Engine) Eval(ctx context.Context, name, src string, module bool) (out string, retErr error) {
	cCtx, cancel, err := bindCallTimeout(ctx, e.spec.TimeoutMS)
	if err != nil {
		return "", err
	}
	defer cancel()

	maxInput := e.spec.MaxInputBytes
	if maxInput <= 0 {
		maxInput = DefaultMaxInputBytes
	}
	if len(src) > maxInput {
		return "", ErrTooLarge
	}

	if e.inClose.Load() || !e.state.CompareAndSwap(stateReady, stateBusy) {
		return "", e.busyOrCurrentState()
	}
	entry := "/" + path.Clean(strings.TrimPrefix(e.spec.Entry, "/"))
	quotedEntry, _ := json.Marshal(entry)
	expectedBoot := fmt.Sprintf("import mod from %s;\nglobalThis.__flats_setModule(mod);\n", quotedEntry)
	isBuiltinBoot := name == "/__flats_boot__.js" && module && !e.bootRan.Swap(true) &&
		(src == expectedBoot || (e.spec.Boot != "" && src == e.spec.Boot))
	if !isBuiltinBoot {
		e.userTainted.Store(true)
		if e.rt != nil {
			e.rt.Taint("user code evaluated")
		}
	} else {
		e.bootActive.Store(true)
	}
	defer func() {
		if isBuiltinBoot {
			e.bootActive.Store(false)
		}
	}()
	goid := curGoroutineID()
	e.callGoid.Store(goid)
	e.callMu.Lock()
	defer func() {
		if e.state.Load() == stateBusy && !e.inClose.Load() {
			e.state.Store(stateReady)
		}
		e.callGoid.Store(0)
		e.callMu.Unlock()
	}()
	if e.inClose.Load() || e.state.Load() == stateClosed {
		return "", ErrClosed
	}
	e.swapCtx.set(cCtx)
	e.hostPanicked.Store(false)
	defer func() {
		if r := recover(); r != nil {
			e.state.Store(statePoisoned)
			retErr = &Fatal{
				Cause:   fmt.Errorf("%v", r),
				Message: trimStack(fmt.Sprint(r)),
			}
		}
		e.swapCtx.set(context.Background())
		e.out.Flush()
		e.errOut.Flush()
	}()

	c := e.rt.Context()
	e.gcBytes += uint64(len(src))

	var v *qjs.Value
	if module {
		v, err = c.Eval(name, qjs.Code(src), qjs.TypeModule())
	} else {
		v, err = c.Eval(name, qjs.Code(src), qjs.TypeGlobal(), qjs.FlagAsync())
	}

	if v != nil {
		if err != nil {
			v.Free()
			v = nil
		} else {
			e.trackHandle(1)
		}
	}

	out, evalErr := e.handleEvalResult(v, err, cCtx)
	e.gcBytes += uint64(len(out))

	if e.testHookPendingException.Load() {
		c.Throw(c.NewError(errors.New("leaked pending exception")))
	}

	if checkErr := e.postCallChecks(c, out); checkErr != nil {
		return "", checkErr
	}
	if evalErr != nil {
		return "", evalErr
	}
	return out, nil
}

func (e *Engine) Close() error {
	goid := curGoroutineID()
	if goid != 0 && e.callGoid.Load() == goid {
		return ErrBusy
	}
	e.inClose.Store(true)
	e.callMu.Lock()
	defer e.callMu.Unlock()

	prev := e.state.Swap(stateClosed)
	if prev == stateClosed {
		return nil
	}
	var closeErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				closeErr = fmt.Errorf("close panic: %v", r)
			}
		}()
		if e.gc != nil {
			func() {
				defer func() { _ = recover() }()
				e.gc.Free()
			}()
			e.gc = nil
		}
		if e.rt != nil {
			func() {
				defer func() { _ = recover() }()
				e.rt.Close()
			}()
			e.rt = nil
		}
	}()
	return closeErr
}

func (e *Engine) Snapshot() (snap []byte, retErr error) {
	if e.state.Load() == statePoisoned {
		return nil, ErrPoisoned
	}
	if e.state.Load() == stateClosed || e.rt == nil {
		return nil, ErrClosed
	}
	if e.spec.ImagePath == "" {
		return nil, ErrSnapshotNotAllowed
	}
	if e.inClose.Load() || !e.state.CompareAndSwap(stateReady, stateBusy) {
		return nil, e.busyOrCurrentState()
	}
	goid := curGoroutineID()
	e.callGoid.Store(goid)
	e.callMu.Lock()
	defer e.callMu.Unlock()
	defer func() {
		if e.state.Load() == stateBusy && !e.inClose.Load() {
			e.state.Store(stateReady)
		}
		e.callGoid.Store(0)
	}()
	if e.inClose.Load() || e.state.Load() == stateClosed {
		return nil, ErrClosed
	}
	defer func() {
		if r := recover(); r != nil {
			e.state.Store(statePoisoned)
			retErr = &Fatal{
				Cause:   fmt.Errorf("%v", r),
				Message: trimStack(fmt.Sprint(r)),
			}
		}
	}()

	if e.hostTainted.Load() {
		return nil, fmt.Errorf("%w: host function called", ErrTainted)
	}
	if e.userTainted.Load() {
		return nil, fmt.Errorf("%w: user code executed", ErrTainted)
	}
	if err := e.rt.ImageEligible(); err != nil {
		return nil, err
	}
	extra := map[string]uint64{}
	if e.gc != nil {
		extra["gc"] = e.gc.Raw()
	}
	img, err := e.rt.Image(extra)
	if err != nil {
		return nil, err
	}
	return img.MarshalBinary()
}

func (e *Engine) Restored() bool {
	return e.rt != nil && e.rt.Restored()
}

func (e *Engine) ImageEligible() error {
	if e == nil {
		return ErrClosed
	}
	if e.state.Load() == statePoisoned {
		return ErrPoisoned
	}
	if e.state.Load() == stateClosed || e.rt == nil {
		return ErrClosed
	}
	if e.hostTainted.Load() {
		return fmt.Errorf("%w: host function called", ErrTainted)
	}
	if e.userTainted.Load() {
		return fmt.Errorf("%w: user code executed", ErrTainted)
	}
	return e.rt.ImageEligible()
}

func (e *Engine) GCDrained() bool {
	return e.gcDrained.Load()
}

func (e *Engine) collectCycles(c *qjs.Context) (err error) {
	if e.state.Load() != stateBusy && e.state.Load() != stateReady {
		return nil
	}
	if e.gc == nil || e.rt == nil {
		return nil
	}
	e.gcCalls++
	memory := e.rt.MemorySize()
	trafficDue := e.gcBytes >= 4*1024*1024
	limit := uint32(32)
	capBytes := uint64(e.spec.MemoryPages) * 65536
	if uint64(memory) >= capBytes-capBytes/4 {
		limit = 1
	}
	if !trafficDue && memory <= e.gcMemory && e.gcCalls < limit {
		e.gcDrained.Store(false)
		return nil
	}
	e.gcBytes, e.gcCalls, e.gcMemory = 0, 0, memory
	e.gcDrained.Store(true)
	defer func() {
		if r := recover(); r != nil {
			err = e.fatal(fmt.Errorf("gc panic: %v", r))
		}
	}()
	v, invokeErr := c.Invoke(e.gc, c.Global())
	if v != nil {
		v.Free()
	}
	if invokeErr != nil {
		return e.fatal(fmt.Errorf("gc failure: %w", invokeErr))
	}
	return nil
}
