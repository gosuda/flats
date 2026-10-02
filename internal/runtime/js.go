package runtime

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oesni/flats/internal/qjs"
)

//go:embed prelude.js
var preludeJS string

// jsVM is one QuickJS runtime with the prelude and the flat's module loaded.
// It is not goroutine-safe: a pool hands it to one request at a time.
type jsVM struct {
	w      *worker
	ctx    *swapCtx // the qjs runtime's context; never replaced
	rt     *qjs.Runtime
	out    *lineWriter
	errOut *lineWriter
	broken bool

	db     *sql.Conn // per-VM connection (opened on first use)
	dbUsed bool
	ws     wsSink // nil for request VMs
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

func newJSVM(w *worker, ws wsSink) (*jsVM, error) {
	var err error
	vm := &jsVM{w: w, ws: ws, ctx: newSwapCtx(),
		out:    &lineWriter{log: w.log, level: "info"},
		errOut: &lineWriter{log: w.log, level: "error"},
	}
	vm.rt, err = qjs.New(qjs.Option{
		Context:            vm.ctx,
		ReadOnlyDir:        w.spec.Dir,
		CloseOnContextDone: true,
		CacheDir:           w.spec.CacheDir,
		MemoryLimitPages:   w.spec.pages(),
		// QuickJS's own heap limit, below the wasm cap so ordinary
		// exhaustion is a clean InternalError rather than a failed malloc.
		MemoryLimit: int(w.spec.pages()) * 65536 * 3 / 4,
		Stdout:      vm.out,
		Stderr:      vm.errOut,
	})
	if err != nil {
		return nil, fmt.Errorf("start JavaScript runtime: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			vm.close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), w.spec.timeout())
	defer cancel()
	err = vm.guard(ctx, func(c *qjs.Context) error {
		c.SetFunc("__flats_host", vm.hostCall)
		v, err := c.Eval("flats:prelude.js", qjs.Code(preludeJS))
		if err != nil {
			return fmt.Errorf("prelude: %w", err)
		}
		v.Free()
		entry := "/" + path.Clean(strings.TrimPrefix(w.spec.Entry, "/"))
		boot := fmt.Sprintf("import mod from %s;\nglobalThis.__flats_setModule(mod);\n", jsString(entry))
		v, err = c.Eval("/__flats_boot__.js", qjs.Code(boot), qjs.TypeModule())
		if err != nil {
			return errors.New(cleanLoadError(err.Error(), w.spec.Entry))
		}
		if v != nil {
			v.Free()
		}
		return nil
	})
	if err != nil {
		var fe *errVMFailed
		if errors.As(err, &fe) && fe.timedOut {
			return nil, fmt.Errorf("%s did not finish loading within %s (top-level code must not block)", w.spec.Entry, w.spec.timeout())
		}
		return nil, err
	}
	if !vm.rt.StackIntact() {
		return nil, fmt.Errorf("loading %s: maximum call stack size exceeded (recursion too deep)", w.spec.Entry)
	}
	ok = true
	return vm, nil
}

// swapCtx is the context.Context a qjs runtime passes to every wasm call.
// wazero (CloseOnContextDone) starts a watcher goroutine per call that reads
// ctx.Done(), possibly after the call returned, so the runtime's context must
// never be replaced; instead the current request's context is swapped
// atomically inside it. Between requests it is context.Background().
type swapCtx struct {
	cur atomic.Pointer[context.Context]
}

func newSwapCtx() *swapCtx {
	s := &swapCtx{}
	s.set(context.Background())
	return s
}

func (s *swapCtx) set(ctx context.Context)     { s.cur.Store(&ctx) }
func (s *swapCtx) current() context.Context    { return *s.cur.Load() }
func (s *swapCtx) Deadline() (time.Time, bool) { return s.current().Deadline() }
func (s *swapCtx) Done() <-chan struct{}       { return s.current().Done() }
func (s *swapCtx) Err() error                  { return s.current().Err() }
func (s *swapCtx) Value(key any) any           { return s.current().Value(key) }

// cleanLoadError turns QuickJS module load errors into a short message.
func cleanLoadError(msg, entry string) string {
	msg = strings.TrimSpace(msg)
	if strings.Contains(msg, "export 'default'") || strings.Contains(msg, "default") && strings.Contains(msg, "not found") {
		return fmt.Sprintf("%s has no default export; write `export default { async fetch(request, env) { ... } }` (%s)", entry, msg)
	}
	if strings.Contains(msg, "could not load module filename '/"+strings.TrimPrefix(path.Clean(entry), "/")+"'") {
		return fmt.Sprintf("entry %s not found in the version files", entry)
	}
	return "loading " + entry + ": " + msg
}

// trimStack drops the Go stack dump qjs appends to wasm call failures.
func trimStack(s string) string {
	if i := strings.Index(s, "\nstack:"); i >= 0 {
		s = s[:i]
	}
	return s
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// guard runs f with ctx as the deadline of every wasm call, converting the
// panics qjs raises on a closed module (timeout) or a trap into errVMFailed.
// After an errVMFailed the VM is marked broken and must be discarded.
func (vm *jsVM) guard(ctx context.Context, f func(*qjs.Context) error) (err error) {
	c := vm.rt.Context()
	vm.ctx.set(ctx)
	defer func() {
		if r := recover(); r != nil {
			vm.broken = true
			err = &errVMFailed{err: errors.New(trimStack(fmt.Sprint(r))), timedOut: ctx.Err() != nil}
		}
		vm.ctx.set(context.Background())
		vm.out.Flush()
		vm.errOut.Flush()
	}()
	return f(c)
}

// invoke calls a global JS function fn(arg) that returns a string (or a
// promise of one) and awaits it.
func (vm *jsVM) invoke(ctx context.Context, fn, arg string) (out string, err error) {
	err = vm.guard(ctx, func(c *qjs.Context) error {
		a := c.NewString(arg)
		p, err := c.Global().InvokeJS(fn, a)
		a.Free()
		if err != nil {
			vm.broken = true
			return &errVMFailed{err: errors.New(trimStack(err.Error())), timedOut: ctx.Err() != nil}
		}
		r := p
		if p.IsPromise() {
			if r, err = p.Await(); err != nil {
				vm.broken = true
				return &errVMFailed{err: errors.New(trimStack(err.Error())), timedOut: ctx.Err() != nil}
			}
		}
		out = r.String()
		r.Free()
		return nil
	})
	if vm.rt != nil && !vm.broken && !vm.rt.StackIntact() {
		// The call recursed into the guard zone at the bottom of the C stack
		// (or past it, corrupting the heap): the result cannot be trusted.
		vm.broken = true
		if err == nil {
			err = &jsError{Name: "RangeError", Message: "Maximum call stack size exceeded (recursion too deep)"}
		}
	}
	vm.afterCall()
	if err == nil && strings.HasPrefix(out, `{"__error":`) {
		var e struct {
			Error jsError `json:"__error"`
		}
		if json.Unmarshal([]byte(out), &e) == nil {
			// InternalError covers out-of-memory, stack overflow and other
			// engine failures after which the runtime may be inconsistent.
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

func (vm *jsVM) info() (fetch, ws bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), vm.w.spec.timeout())
	defer cancel()
	out, err := vm.invoke(ctx, "__flats_info", "")
	if err != nil {
		return false, false, err
	}
	var i struct{ Fetch, Websocket bool }
	err = json.Unmarshal([]byte(out), &i)
	return i.Fetch, i.Websocket, err
}

func (vm *jsVM) close() {
	if vm.db != nil {
		vm.db.Close()
		vm.db = nil
	}
	if vm.rt != nil {
		func() {
			defer func() { recover() }() // Close panics on a module closed by a timeout
			vm.rt.Close()
		}()
		vm.rt = nil
	}
}

// hostCall implements __flats_host(op, argsJSON) -> resultJSON.
func (vm *jsVM) hostCall(this *qjs.This) (*qjs.Value, error) {
	args := this.Args()
	defer func() {
		for _, a := range args {
			a.Free()
		}
	}()
	if len(args) != 2 {
		return nil, errors.New("bad host call")
	}
	op := args[0].String()
	raw := args[1].String()
	c := this.Context()
	res, err := vm.host(vm.ctx.current(), op, json.RawMessage(raw))
	if err != nil {
		return nil, err
	}
	return c.NewString(res), nil
}

func (vm *jsVM) host(ctx context.Context, op string, raw json.RawMessage) (string, error) {
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
	fetch, ws, err := vm.info()
	if err != nil {
		vm.close()
		return nil, err
	}
	if !fetch && !ws {
		vm.close()
		return nil, fmt.Errorf("%s must `export default { async fetch(request, env) { ... } }` (found no fetch or websocket handler)", w.spec.Entry)
	}
	e := &jsEngine{w: w, pool: newJSPool(w, w.spec.poolSize(), vm), hasWS: ws}
	if ws {
		e.wsHub = newWSHub(w)
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
