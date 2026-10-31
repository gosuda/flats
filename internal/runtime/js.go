package runtime

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/egress"
	"github.com/gosuda/flats/internal/jsengine"
)

//go:embed prelude.js
var preludeJS string

// jsVM is one QuickJS runtime with the prelude and the flat's module loaded.
// It is not goroutine-safe: a pool hands it to one request at a time.
type jsVM struct {
	w             *worker
	eng           *jsengine.Engine
	broken        bool
	networkActive bool
	networkCalls  int
	hostUsed      bool

	db     *sql.Conn
	dbUsed bool
	ws     wsSink
}

type wsSink interface {
	send(id int64, text string) error
	close(id int64, code int, reason string) error
}

// jsError is an exception thrown by the flat's code.
type jsError struct {
	Name, Message, Stack string
}

func (e *jsError) Error() string {
	s := e.Name + ": " + e.Message
	if e.Stack != "" {
		s += "\n" + strings.TrimRight(e.Stack, "\n")
	}
	return s
}

// errVMFailed means the runtime is unusable (timeout, wasm trap, OOM).
type errVMFailed struct {
	err      error
	timedOut bool
}

func (e *errVMFailed) Error() string { return e.err.Error() }
func (e *errVMFailed) Unwrap() error { return e.err }

func newJSVM(w *worker, ws wsSink) (*jsVM, error) {
	if w.spec.ImagePath != "" && imagesEnabled() {
		return newDocumentVM(w, ws) // image.go
	}
	return newPlainVM(w, ws)
}

// newPlainVM loads the prelude and the flat's module into a new runtime.
func newPlainVM(w *worker, ws wsSink) (*jsVM, error) {
	vm := allocJSVM(w, ws)
	if err := vm.newEngine(vm.engineSpec()); err != nil {
		return nil, err
	}
	if err := vm.startup(vm.loadWithInit); err != nil {
		vm.close()
		return nil, err
	}
	return vm, nil
}

func allocJSVM(w *worker, ws wsSink) *jsVM {
	return &jsVM{w: w, ws: ws}
}

func (vm *jsVM) engineSpec() jsengine.Spec {
	w := vm.w
	return jsengine.Spec{
		Dir:             w.spec.Dir,
		Entry:           w.spec.Entry,
		TimeoutMS:       int(w.spec.timeout().Milliseconds()),
		MaxInputBytes:   MaxRequestBody,
		MaxOutputBytes:  MaxResponseBody,
		MaxHostArgBytes: MaxRequestBody,
		CacheDir:        w.spec.CacheDir,
		MemoryPages:     int(w.spec.pages()),
		Stdout:          &lineWriter{log: w.log, level: "info"},
		Stderr:          &lineWriter{log: w.log, level: "error"},
		Prelude:         preludeJS,
		RawCodec:        vm.docsCodec,
		Host:            vm.hostCall,
	}
}

func (vm *jsVM) newEngine(spec jsengine.Spec) (err error) {
	if s := takePrewarmedShell(spec); s != nil {
		sp := spec
		sp.Shell = s
		if vm.eng, err = jsengine.Open(sp, sp.Host); err == nil {
			return nil
		}
	}
	if vm.eng, err = jsengine.Open(spec, spec.Host); err != nil {
		return err
	}
	return nil
}

func (vm *jsVM) startup(f func(context.Context) error) error {
	w := vm.w
	ctx, cancel := context.WithTimeout(context.Background(), w.spec.timeout())
	defer cancel()
	err := f(ctx)
	if err != nil {
		if ctx.Err() != nil || strings.Contains(err.Error(), "context deadline exceeded") {
			return fmt.Errorf("%s did not finish loading within %s (top-level code must not block)", w.spec.Entry, w.spec.timeout())
		}
		if strings.Contains(err.Error(), "maximum call stack size exceeded") {
			return fmt.Errorf("loading %s: maximum call stack size exceeded (recursion too deep)", w.spec.Entry)
		}
		return err
	}
	return nil
}

const jsBindingOrder = "gc,__flats_host,__flats_docs_codec,prelude,boot"

func (vm *jsVM) load(ctx context.Context) error {
	return vm.loadModule(ctx)
}

func (vm *jsVM) loadWithInit(ctx context.Context) error {
	if err := vm.initRuntime(ctx); err != nil {
		return err
	}
	return vm.loadModule(ctx)
}

func (vm *jsVM) loadModule(ctx context.Context) error {
	entry := "/" + path.Clean(strings.TrimPrefix(vm.w.spec.Entry, "/"))
	boot := fmt.Sprintf("import mod from %s;\nglobalThis.__flats_setModule(mod);\n", jsString(entry))
	_, err := vm.eng.Eval(ctx, "/__flats_boot__.js", boot, true)
	if err != nil {
		return errors.New(cleanLoadError(err.Error(), vm.w.spec.Entry))
	}
	return nil
}

func (vm *jsVM) initRuntime(ctx context.Context) error {
	_, err := vm.eng.Call(ctx, "__flats_init", "")
	if err != nil {
		return fmt.Errorf("prelude: %w", err)
	}
	return nil
}

func cleanLoadError(msg, entry string) string {
	msg = strings.TrimSpace(msg)
	msg = strings.TrimPrefix(msg, "fatal: ")
	if strings.Contains(msg, "export 'default'") || (strings.Contains(msg, "default") && strings.Contains(msg, "not found")) {
		return fmt.Sprintf("%s has no default export; write `export default { async fetch(request, env) { ... } }` (%s)", entry, msg)
	}
	if strings.Contains(msg, "could not load module filename '/"+strings.TrimPrefix(path.Clean(entry), "/")+"'") {
		return fmt.Sprintf("entry %s not found in the version files", entry)
	}
	return "loading " + entry + ": " + msg
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (vm *jsVM) invoke(ctx context.Context, fn, arg string) (out string, err error) {
	if _, ok := ctx.Deadline(); !ok {
		timeout := DefaultTimeout
		if vm.w != nil {
			timeout = vm.w.spec.timeout()
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	vm.networkActive = fn == "__flats_dispatch" || fn == "__flats_ws"
	vm.networkCalls = 0
	defer func() { vm.networkActive = false }()

	out, err = vm.eng.Call(ctx, fn, arg)
	if err != nil {
		timedOut := ctx.Err() != nil || strings.Contains(err.Error(), "context deadline exceeded")
		if errors.Is(err, jsengine.ErrPoisoned) || errors.Is(err, jsengine.ErrClosed) {
			vm.broken = true
		}
		var fatal *jsengine.Fatal
		if errors.As(err, &fatal) {
			vm.broken = true
		}
		if vm.broken || timedOut {
			err = &errVMFailed{err: err, timedOut: timedOut}
		}
	}
	vm.afterCall()
	if err == nil && strings.HasPrefix(out, `{"__error":`) {
		var e struct {
			Error jsError `json:"__error"`
		}
		if json.Unmarshal([]byte(out), &e) == nil {
			if e.Error.Name == "InternalError" || strings.Contains(e.Error.Message, "out of memory") {
				vm.broken = true
			}
			return "", &e.Error
		}
	}
	return out, err
}

// afterCall rolls back a transaction a request left open.
func (vm *jsVM) afterCall() {
	if vm.db != nil && vm.dbUsed {
		vm.dbUsed = false
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		vm.db.ExecContext(ctx, "ROLLBACK") // error = no open transaction
		cancel()
	}
}

// WASI QuickJS reports zero malloc usable size, so automatic GC misses large
// payloads. Collect after 4 MiB of boundary traffic, linear-memory growth, or
// 32 calls; at >= 3/4 of the memory cap, collect every call because growth
// eventually stops signalling guest-only garbage. MemorySize is an O(1)
// high-water mark, not live usage; JS_ComputeMemoryUsage scans the heap.
// Uncaught engine failures discard the VM. A guest-caught OOM clears QuickJS's
// exception and has no persistent signal exposed by the binding, so it cannot
// reliably trigger recovery or VM replacement (nor does cap-sized linear memory
// prove exhaustion after GC, since linear memory never shrinks).

// moduleInfo is what a module's default export provides. PublicPools is a
// built-in docs internal (not runtime API v1): separate Public WebSocket
// pools (ws.go).
type moduleInfo struct {
	Fetch, Websocket, PublicPools bool
}

func (vm *jsVM) info() (moduleInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), vm.w.spec.timeout())
	defer cancel()
	var i moduleInfo
	out, err := vm.invoke(ctx, "__flats_info", "")
	if err != nil {
		return i, err
	}
	err = json.Unmarshal([]byte(out), &i)
	return i, err
}

func (vm *jsVM) close() {
	if vm.db != nil {
		vm.db.Close()
		vm.db = nil
	}
	if vm.eng != nil {
		vm.eng.Close()
		vm.eng = nil
	}
}

// The visitor's context is separate from the VM deadline: cancellation closes
// the wazero VM, so a disconnect must cancel only the host-mediated HTTP call.
type outboundRequestContextKey struct{}

func outboundContext(ctx context.Context) (context.Context, func()) {
	child, cancel := context.WithCancel(ctx)
	original, ok := ctx.Value(outboundRequestContextKey{}).(context.Context)
	if !ok {
		return child, cancel
	}
	if original.Err() != nil {
		cancel()
		return child, cancel
	}
	stop := context.AfterFunc(original, cancel)
	return child, func() { stop(); cancel() }
}

// hostCall implements __flats_host(op, argsJSON) -> resultJSON.
func (vm *jsVM) hostCall(ctx context.Context, op string, raw []byte) ([]byte, error) {
	vm.hostUsed = true
	res, err := vm.host(ctx, op, json.RawMessage(raw))
	if err != nil {
		return nil, err
	}
	return []byte(res), nil
}

func (vm *jsVM) host(ctx context.Context, op string, raw json.RawMessage) (string, error) {
	// Treat the guest ABI as untrusted, even if it replaces JSON.stringify.
	if op == "fetch" && len(raw) > 2<<20 {
		return "", egress.ErrLimit
	}
	var a []json.RawMessage
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("%s: bad arguments", op)
	}
	str := func(i int) string {
		var s string
		if i < len(a) {
			json.Unmarshal(a[i], &s)
		}
		return s
	}
	num := func(i int) int64 {
		var n int64
		if i < len(a) {
			json.Unmarshal(a[i], &n)
		}
		return n
	}
	arg := func(i int) json.RawMessage {
		if i < len(a) {
			return a[i]
		}
		return nil
	}
	jsonOf := func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	}
	switch op {
	case "runtime.generation":
		return jsonOf(vm.w.spec.Generation)
	case "fetch":
		if !vm.networkActive {
			return "", errors.New("fetch is only available inside a request or WebSocket handler")
		}
		if vm.networkCalls >= 16 {
			return "", egress.ErrLimit
		}
		vm.networkCalls++
		if len(a) != 1 {
			return "", egress.ErrInvalid
		}
		var req egress.Request
		if err := json.Unmarshal(a[0], &req); err != nil {
			return "", egress.ErrInvalid
		}
		if vm.w.network == nil {
			return "", egress.ErrDenied
		}
		fetchCtx, cancel := outboundContext(ctx)
		defer cancel()
		res, err := vm.w.network.Fetch(fetchCtx, req)
		if err != nil {
			return "", err
		}
		return jsonOf(res)
	case "env":
		return jsonOf(vm.w.spec.Env)
	case "log":
		level := str(0)
		if level != "warn" && level != "error" {
			level = "info"
		}
		vm.w.log.Log(level, str(1))
		return "", nil
	case "db.query", "db.exec":
		if vm.db == nil {
			c, err := vm.w.data.conn(ctx)
			if err != nil {
				return "", err
			}
			vm.db = c
		}
		vm.dbUsed = true
		if op == "db.query" {
			return dbQuery(ctx, vm.db, str(0), arg(1))
		}
		return dbExec(ctx, vm.db, str(0), arg(1))
	case "files.get":
		v, err := vm.w.data.fileGet(str(0))
		if err != nil {
			return "", err
		}
		return jsonOf(v)
	case "files.put":
		return "", vm.w.data.filePut(str(0), str(1))
	case "files.delete":
		ok, err := vm.w.data.fileDelete(str(0))
		if err != nil {
			return "", err
		}
		return jsonOf(ok)
	case "files.list":
		keys, err := vm.w.data.fileList(str(0))
		if err != nil {
			return "", err
		}
		return jsonOf(keys)
	case "crypto.random":
		n := num(0)
		if n < 0 || n > 65536 {
			return "", errors.New("crypto.getRandomValues: at most 65536 bytes per call")
		}
		b := make([]byte, n)
		if _, err := cryptorand.Read(b); err != nil {
			return "", err
		}
		return jsonOf(hex.EncodeToString(b))
	case "ws.send":
		if vm.ws == nil {
			return "", errors.New("WebSocket send outside a websocket handler")
		}
		return "", vm.ws.send(num(0), str(1))
	case "ws.limits":
		if sink, ok := vm.ws.(interface {
			setSendLimits(int64, int64, int64) error
		}); ok {
			return "", sink.setSendLimits(num(0), num(1), num(2))
		}
		return "", errors.New("WebSocket send limits unavailable")
	case "ws.wake":
		// Best effort and never blocking: a full queue or a worker without
		// WebSocket support drops it, and rooms catch up at the next heartbeat.
		if doc := str(0); len(doc) <= 4096 && vm.w.js != nil && vm.w.js.wsHub != nil {
			vm.w.js.wsHub.wake(doc)
		}
		return "", nil
	case "ws.signal":
		// Coalescing and never lost while the worker runs (ws.go signal).
		if name := str(0); name != "" && len(name) <= 64 && vm.w.js != nil && vm.w.js.wsHub != nil {
			vm.w.js.wsHub.signal(name)
		}
		return "", nil
	case "ws.close":
		if vm.ws == nil {
			return "", errors.New("WebSocket close outside a websocket handler")
		}
		return "", vm.ws.close(num(0), int(num(1)), str(2))
	}
	return "", fmt.Errorf("unknown host operation %q", op)
}

// jsPool hands out request VMs, creating up to size of them on demand and
// replacing broken ones.
type jsPool struct {
	w    *worker
	idle chan *jsVM
	sem  chan struct{} // one token per live VM
	mu   sync.Mutex
}

func newJSPool(w *worker, size int, first *jsVM) *jsPool {
	p := &jsPool{w: w, idle: make(chan *jsVM, size), sem: make(chan struct{}, size)}
	p.sem <- struct{}{}
	p.idle <- first
	return p
}

func (p *jsPool) get(ctx context.Context) (*jsVM, error) {
	select {
	case vm := <-p.idle:
		return vm, nil
	default:
	}
	select {
	case vm := <-p.idle:
		return vm, nil
	case p.sem <- struct{}{}:
		vm, err := newJSVM(p.w, nil)
		if err != nil {
			<-p.sem
			return nil, err
		}
		return vm, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *jsPool) put(vm *jsVM) {
	if vm.broken {
		vm.close()
		<-p.sem
		return
	}
	p.idle <- vm
}

func (p *jsPool) close() {
	for {
		select {
		case vm := <-p.idle:
			vm.close()
		default:
			return
		}
	}
}

// jsEngine serves requests with the JS pool.
type jsEngine struct {
	w     *worker
	pool  *jsPool
	hasWS bool
	wsMu  sync.Mutex
	wsHub *wsHub
}

func newJSEngine(w *worker) (*jsEngine, error) {
	vm, err := newJSVM(w, nil)
	if err != nil {
		return nil, err
	}
	info, err := vm.info()
	if err != nil {
		vm.close()
		return nil, err
	}
	if !info.Fetch && !info.Websocket {
		vm.close()
		return nil, fmt.Errorf("%s must `export default { async fetch(request, env) { ... } }` (found no fetch or websocket handler)", w.spec.Entry)
	}
	e := &jsEngine{w: w, pool: newJSPool(w, w.spec.poolSize(), vm), hasWS: info.Websocket}
	if info.Websocket {
		e.wsHub = newWSHub(w, info.PublicPools)
	}
	return e, nil
}

func (e *jsEngine) handle(ctx context.Context, req []byte) ([]byte, error) {
	vm, err := e.pool.get(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &errVMFailed{err: errors.New("all JavaScript runtimes stayed busy"), timedOut: true}
		}
		return nil, err
	}
	out, err := vm.invoke(ctx, "__flats_dispatch", string(req))
	e.pool.put(vm)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

func (e *jsEngine) close() {
	if e.wsHub != nil {
		e.wsHub.shutdown()
	}
	e.pool.close()
}

// docsCodec keeps bounded binary serialization out of the interpreted VM.
// It has no I/O capabilities. Fetch and binary response bodies use separate
// limits for their binary bridges.
func (vm *jsVM) docsCodec(op string, in jsengine.Value) (jsengine.Value, error) {
	readString := func() (string, error) {
		var value string
		raw := in.String()
		err := json.Unmarshal([]byte(raw), &value)
		return value, err
	}
	newString := func(s string) jsengine.Value {
		return in.NewString(s)
	}
	newBuffer := func(b []byte) jsengine.Value {
		return in.NewArrayBuffer(b)
	}
	max := 1536 * 1024
	if op == "fetch.encode" {
		max = egress.MaxRequestBody
	}
	if op == "fetch.decode" {
		max = egress.MaxResponseBody
	}
	if op == "response.encode" {
		max = MaxResponseBody
	}
	switch op {
	case "response.encode":
		if !in.IsByteArray() {
			return jsengine.Value{}, errors.New("docs codec binary limit")
		}
		if in.ByteLen() > int64(max) {
			return jsengine.Value{}, fmt.Errorf("response body larger than %d MiB", MaxResponseBody>>20)
		}
		return newString(base64.StdEncoding.EncodeToString(in.ToByteArray())), nil
	case "encode", "fetch.encode":
		if !in.IsByteArray() || in.ByteLen() > int64(max) {
			return jsengine.Value{}, errors.New("docs codec binary limit")
		}
		return newString(base64.StdEncoding.EncodeToString(in.ToByteArray())), nil
	case "decode", "fetch.decode":
		s, err := readString()
		if err != nil {
			return jsengine.Value{}, err
		}
		if len(s) > ((max+2)/3)*4 {
			return jsengine.Value{}, errors.New("docs codec base64 limit")
		}
		b, err := base64.StdEncoding.Strict().DecodeString(s)
		if err != nil || len(b) > max || base64.StdEncoding.EncodeToString(b) != s {
			return jsengine.Value{}, errors.New("invalid canonical base64")
		}
		return newBuffer(b), nil
	case "textEncode":
		s, err := readString()
		if err != nil {
			return jsengine.Value{}, err
		}
		if len(s) > max {
			return jsengine.Value{}, errors.New("docs codec text limit")
		}
		if !utf8.ValidString(s) {
			s = strings.ToValidUTF8(s, "\ufffd")
		}
		return newBuffer([]byte(s)), nil
	case "textDecode":
		if !in.IsByteArray() || in.ByteLen() > int64(max) {
			return jsengine.Value{}, errors.New("docs codec binary limit")
		}
		b := in.ToByteArray()
		if !utf8.Valid(b) {
			return jsengine.Value{}, errors.New("invalid UTF8")
		}
		raw, err := json.Marshal(string(b))
		if err != nil {
			return jsengine.Value{}, err
		}
		return newString(string(raw)), nil
	case "length":
		s, err := readString()
		if err != nil {
			return jsengine.Value{}, err
		}
		if len(s) > 4*1024*1024 {
			return jsengine.Value{}, errors.New("docs codec text limit")
		}
		return in.NewInt32(int32(len(s))), nil
	case "digest":
		s, err := readString()
		if err != nil {
			return jsengine.Value{}, err
		}
		if len(s) > 4*1024*1024 {
			return jsengine.Value{}, errors.New("docs codec digest limit")
		}
		sum := sha256.Sum256([]byte(s))
		return newString(hex.EncodeToString(sum[:])), nil
	case "digests":
		raw := in.String()
		if len(raw) > 8*1024*1024 {
			return jsengine.Value{}, errors.New("docs codec digest limit")
		}
		var list []string
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return jsengine.Value{}, err
		}
		if limit := vm.stringByteLimit() / (sha256.Size * 2); len(list) > limit {
			return jsengine.Value{}, fmt.Errorf("docs codec digest result is larger than this runtime can hold: at most %d digests", limit)
		}
		out := make([]byte, 0, len(list)*sha256.Size*2)
		for _, s := range list {
			sum := sha256.Sum256([]byte(s))
			out = hex.AppendEncode(out, sum[:])
		}
		return newString(string(out)), nil
	default:
		return jsengine.Value{}, errors.New("unknown docs codec")
	}
}

func (vm *jsVM) stringByteLimit() int {
	pages := DefaultMemoryPages
	if vm.w != nil && vm.w.spec.pages() > 0 {
		pages = int(vm.w.spec.pages())
	}
	return pages * 65536 * 3 / 8
}

// afterReady runs once the worker has reported ready: it writes a document
// image captured during startup (image.go).
func (e *jsEngine) afterReady() {
	if e.w.spec.ImagePath != "" {
		documentImageFor(e.w.spec.ImagePath).ready(e.w)
	}
}
